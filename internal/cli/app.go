package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"gear/internal/app"
	"gear/internal/applog"
	"gear/internal/engine"
	"gear/internal/gearhome"
)

// appCmd 构造 `gear app` 命令组：run 是**前台**监管器（人/CI 持有），
// start/stop/status/list/prune 是**后台**启动器 + 台账（进程自持，跨会话存活）。
// 五条子命令与 MCP 面的 gear_app 共用 internal/app 同一份实现——MCP 只是协议翻译层。
func appCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "app",
		Short: "应用生命周期：run（前台监管）/ start·stop·status·list·prune（后台进程 + 台账）",
		Long: "把 Flow 变成受管进程。\n" +
			"  run    前台监管，进程归你（或 CI）持有，Ctrl+C 停止\n" +
			"  start  派生独立后台进程后立即返回；进程自持，跨会话存活（不随本命令退出）\n" +
			"  stop   终止应用并复检端口；收不干净会如实报 ORPHAN_APP，不谎报成功\n" +
			"  status 单个应用的状态快照（pid + 端口双判据）\n" +
			"  list   全部台账与遗留（orphan）项\n" +
			"  prune  清理台账里确定已终止的项（running/unknown 一律保留；--dry-run 先预览）\n" +
			"  delete 注销 App 清单（不做引用守卫：App 只是聚合描述，删清单不等于删应用）\n" +
			"台账落在 <registry 同目录>/apps/（默认 ~/.gear/apps/），--apps-dir 可覆盖。",
		// 台账目录是「路径类 flag」，与 --registry 一样要做 POSIX 路径误用提示：
		// 从 Git Bash 传 `/d/...` 会被 Windows 当成「当前盘符根下的绝对路径」，
		// 于是台账静默落到 `D:\d\...`，呈现出来却是「无应用台账」这种下游症状
		// （2026-09-21 实测：`--apps-dir /d/x` 的 log_path 变成 `\d\x\...`）。
		// 放在父命令的 PersistentPreRunE 里，四个子命令一次覆盖。
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			if f := cmd.Flags().Lookup("apps-dir"); f != nil {
				warnIfPosixPath("--apps-dir", f.Value.String())
			}
			return nil
		},
	}
	c.AddCommand(appRunCmd(), appStartCmd(), appStopCmd(), appStatusCmd(), appListCmd(), appPruneCmd(), appDeleteCmd())
	return c
}

// appRunCmd 启动并监管一个或多个 Flow 进程（S2 实现：长驻 step + restart + 信号停止）。
// 设计见 PRD §14：一个 app run = 一个进程，负责整个 Flow 生命周期；
// 服务 = 长驻不退出的 Flow，并发 = 启动多个 app run（或一次传多个 flow-id 受 maxConcurrent 约束）。
func appRunCmd() *cobra.Command {
	var restart string
	var maxConc int
	var maxRestarts int
	var restartWindow time.Duration
	c := &cobra.Command{
		Use:   "run <flow-id>...",
		Short: "前台监管 Flow 进程（长驻 step + restart 策略；Ctrl+C 停止）",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			reg, err := openRegistry()
			if err != nil {
				return err
			}
			defer reg.Close()

			policy := app.RestartAlways
			switch app.RestartPolicy(restart) {
			case app.RestartNever:
				policy = app.RestartNever
			case app.RestartAlways, "":
				policy = app.RestartAlways
			default:
				return fmt.Errorf("非法的 --restart=%q（应为 always 或 never）", restart)
			}

			ctx, stop := app.ContextWithSignals(context.Background())
			defer stop()

			// 硬杀连坐（REQ-APP-JOB-KILLONCLOSE）：把本进程与其后派生的整棵子进程树
			// 放进 kill-on-close Job——监管进程被硬杀（taskkill /F）时子进程自动连坐，
			// 不再「手动杀 sidecar 又被 --restart always 拉回」。失败降级为警告：
			// 监管功能不依赖 Job，只是少一层保障（如 harness 的 Job 禁 breakaway 时）。
			if err := app.InitJobKillOnClose(); err != nil {
				fmt.Fprintf(os.Stderr, "⚠ %v\n", err)
			}

			fmt.Printf("[app run] 监管 %d 个 Flow，restart=%s，maxConcurrent=%d（Ctrl+C 停止）\n",
				len(args), policy, maxConc)
			// 实时日志回推（REQ-API-APP-LOGS）：只有 `app start` 派生出来的子进程会带
			// GEAR_APP_LOG_* 三个 env；人直接跑前台 `app run` 时 reporter=nil，行为零变化。
			// 挂点选 engine.LogSink（阶段 1 已让 process / service 输出逐行走这里），
			// 而不是 tail <appID>.log —— 后者正是用户拒绝的「从日志文件拉取」。
			reporter, err := applog.FromEnv()
			if err != nil {
				return err
			}
			var engOpts []func(*engine.Engine)
			if reporter != nil {
				defer reporter.Close()
				engOpts = append(engOpts, engine.WithLogSink(reporter.Sink))
				fmt.Printf("[app run] 实时日志回推已启用（app=%s）\n", reporter.AppID())
			}
			eng, err := cliEngine(reg, engOpts...)
			if err != nil {
				return err
			}
			sup := app.New(eng, policy, maxConc)
			// 重启预算（REQ-APP-RESTART-LIMIT）：窗口内同一 service 退出超限即放弃重启。
			// 默认 5 次/10 分钟——此前只有退避没有上限，手动 kill 掉的 service 会被
			// 无限拉回，「杀几次起几次」。--max-restarts 0 可恢复旧的无上限行为。
			sup.SetRestartLimit(maxRestarts, restartWindow)
			// 台账身份 env（GEAR_APP_LEDGER_*）只存在于 `app start` 的派生链路上：
			// 有则把 service 重启次数回写台账记录（UI / CLI 才能看到真实重启数），
			// 前台 `app run` 没有 env，回调为 nil，行为零变化。
			if cb := app.RestartRecorderFromEnv(); cb != nil {
				sup.SetOnRestart(cb)
			}
			// 预算耗尽时要让台账记住「为什么收工」，否则 UI 只能显示笼统的 exited。
			if cb := app.ExhaustedRecorderFromEnv(); cb != nil {
				sup.SetOnExhausted(cb)
			}
			if err := sup.Supervise(ctx, args); err != nil {
				return err
			}
			fmt.Println("[app run] 已停止")
			return nil
		},
	}
	c.Flags().StringVar(&restart, "restart", "always", "service 退出后的重启策略：always / never")
	c.Flags().IntVar(&maxConc, "max", 0, "同时启动的 Flow 上限（0=不限；S2 并发闸门，进程池留 S4）")
	c.Flags().IntVar(&maxRestarts, "max-restarts", app.DefaultRestartLimit(),
		"重启预算：窗口内同一 service 退出超过该次数即放弃重启（0=不限，恢复旧行为）")
	c.Flags().DurationVar(&restartWindow, "restart-window", app.DefaultRestartWindow(),
		"重启预算的统计窗口（与 --max-restarts 配合）")
	return c
}

// appStartCmd 派生后台进程监管 Flow。
//
// ⚠ 与 `run` 的关键差别：本命令**立即返回**，进程成为独立后台进程（跨会话存活）。
// 所以必须提供 stop——「能活过会话却关不掉」才是真缺陷。
func appStartCmd() *cobra.Command {
	var restart string
	var ports []int
	var waitSeconds int
	var replace bool
	var appsDir string
	c := &cobra.Command{
		Use:   "start <flow-id>",
		Short: "派生独立后台进程监管 Flow（立即返回；跨会话存活，用 app stop 关闭）",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := app.Start(app.StartOptions{
				RegistryPath: registryPath,
				AppsDir:      appsDir,
				FlowID:       args[0],
				Restart:      restart,
				Ports:        ports,
				WaitSeconds:  waitSeconds,
				Replace:      replace,
				// 作用域必须传给派生进程（它才是跑 Flow 的那个），见 StartOptions.Scope 的注释。
				Scope: cliScope,
				// 入参没传 --port 时回退 App 清单的 start.ports（REQ-MCP-PORTS-ONE-TRUTH）。
				// launch 层不直接读 registry，由这里注入查询；生效来源在 res.PortsSource。
				AppLookup: func(flowID string) ([]int, bool, error) {
					return app.AppPortsFromRegistry(registryPath, flowID)
				},
			})
			if err != nil {
				return err
			}
			fmt.Printf("%s  %s  pid=%d\n", res.State, res.AppID, res.PID)
			fmt.Printf("  flow: %s\n  log:  %s\n", res.FlowID, res.LogPath)
			// 端口来源如实上报：explicit=本次入参 / bundle=App 清单 start.ports 回退 /
			// none=没声明（探活退化为只看 pid，半死状态测不出来）。
			fmt.Printf("  ports_source: %s\n", res.PortsSource)
			for _, p := range res.Ports {
				fmt.Printf("  port %-6d listening=%v\n", p.Port, p.Listening)
			}
			for _, u := range res.OpenURLs {
				fmt.Printf("  打开地址：%s\n", u)
			}
			for _, ln := range res.LastLogs {
				fmt.Printf("  │ %s\n", ln)
			}
			if res.Warning != "" {
				fmt.Fprintf(os.Stderr, "⚠ %s\n", res.Warning)
			}
			if res.State == string(app.StateAlreadyRunning) {
				fmt.Println("  （同 flow 已有活跃实例；要换新实例加 --replace）")
			} else if res.State == string(app.StateStartupFailed) {
				// 根因（res.Reason）由内核从监管进程日志里挑出，认不出时退回 last_logs。
				// 之前这里印的是一句对所有故障都成立的泛化文案，等于没说。
				if res.Reason != "" {
					return fmt.Errorf("应用启动即退出（pid=%d）——%s", res.PID, res.Reason)
				}
				return fmt.Errorf("应用启动即退出（pid=%d）——上面是日志尾部", res.PID)
			}
			return nil
		},
	}
	c.Flags().StringVar(&restart, "restart", "always", "service 退出后的重启策略：always / never")
	c.Flags().IntSliceVar(&ports, "port", nil, "声明端口（探活与 stop 后复检；可重复，如 --port 8080 --port 9090）")
	c.Flags().IntVar(&waitSeconds, "wait", 0, "短等秒数确认启动结果（0=立即返回）")
	c.Flags().BoolVar(&replace, "replace", false, "已有同 flow 活跃实例时先停再启")
	c.Flags().StringVar(&appsDir, "apps-dir", "", "应用台账目录（默认 <registry 同目录>/apps）")
	return c
}

// appStopCmd 终止应用并复检端口。
func appStopCmd() *cobra.Command {
	var appsDir string
	c := &cobra.Command{
		Use:   "stop <app-id>",
		Short: "终止应用并复检端口（收不干净则报 ORPHAN_APP，不谎报成功）",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := app.Stop(app.StopOptions{
				RegistryPath: registryPath,
				AppsDir:      appsDir,
				AppID:        args[0],
			})
			if err != nil {
				return err
			}
			fmt.Printf("%s  %s\n", res.State, res.AppID)
			for _, p := range res.Ports {
				fmt.Printf("  port %-6d listening=%v\n", p.Port, p.Listening)
			}
			if res.Code != "" {
				return fmt.Errorf("%s；%s", res.Code, res.Hint)
			}
			return nil
		},
	}
	c.Flags().StringVar(&appsDir, "apps-dir", "", "应用台账目录（默认 <registry 同目录>/apps）")
	return c
}

// appStatusCmd 查单个应用状态。
func appStatusCmd() *cobra.Command {
	var appsDir string
	var tail int
	c := &cobra.Command{
		Use:   "status <app-id>",
		Short: "查看单个应用状态（pid 存活 + 端口探活双判据；不一致给 unknown）",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := app.Status(app.StatusOptions{
				RegistryPath: registryPath,
				AppsDir:      appsDir,
				AppID:        args[0],
				TailLogs:     tail,
			})
			if err != nil {
				return err
			}
			printAppStatus(*res)
			return nil
		},
	}
	c.Flags().StringVar(&appsDir, "apps-dir", "", "应用台账目录（默认 <registry 同目录>/apps）")
	c.Flags().IntVar(&tail, "tail", 0, "附加日志末 N 行（排障用）")
	return c
}

// appListCmd 列出全部台账与遗留项。
func appListCmd() *cobra.Command {
	var appsDir string
	c := &cobra.Command{
		Use:   "list",
		Short: "列出全部应用台账与遗留（orphan）项",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := app.List(registryPath, appsDir)
			if err != nil {
				return err
			}
			if len(res.Items) == 0 {
				fmt.Println("（无应用台账）")
			}
			for _, it := range res.Items {
				printAppStatus(it)
			}
			for _, o := range res.Orphan {
				fmt.Printf("! orphan %s: %s（pid=%d，日志 %s）\n", o.AppID, o.Reason, o.PID, o.LogPath)
			}
			return nil
		},
	}
	c.Flags().StringVar(&appsDir, "apps-dir", "", "应用台账目录（默认 <registry 同目录>/apps）")
	return c
}

// appPruneCmd 清理台账里**确定已终止**的记录。
//
// 与 MCP 的 `gear_app(action=prune)`、REST 的 `POST /api/apps/prune` 共用 app.Prune。
// 为什么要专门给个人工入口（2026-09-22）：`start` 失败一次就落一条台账，调试一轮下来堆十几条，
// 而此前只有按 app_id 单个 `stop`——清理要调十几次，且无从按状态过滤。prune 之前只在 MCP 面，
// 于是「智能体能清、人不能清」这种倒挂。
func appPruneCmd() *cobra.Command {
	var appsDir string
	var flowID string
	var dryRun bool
	c := &cobra.Command{
		Use:   "prune",
		Short: "清理台账里确定已终止的项（running/unknown 一律保留；--dry-run 先预览）",
		Long: "只清 `exited` / `stopped` / `startup_failed` 三种**终态**的台账记录。\n" +
			"`running` 与 `unknown` 一律保留——unknown 的双判据互相矛盾，删掉可能把「还在跑却再无人可查」\n" +
			"的进程变成真孤儿，而台账存在的全部意义就是避免这个。\n" +
			"只删记录，**不杀进程、不碰端口**（要停进程用 `gear app stop <app-id>`）。",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := app.ResolveAppsDir(registryPath, appsDir)
			if err != nil {
				return err
			}
			res, err := app.Prune(app.PruneOptions{AppsDir: dir, FlowID: flowID, DryRun: dryRun})
			if err != nil {
				return err
			}
			verb := "已清理"
			if res.DryRun {
				verb = "将清理（--dry-run，未动磁盘）"
			}
			fmt.Printf("%s %d 条台账项（目录 %s）\n", verb, len(res.Pruned), dir)
			for _, it := range res.Pruned {
				fmt.Printf("  - %s  flow=%s  state=%s\n", it.AppID, it.FlowID, it.State)
			}
			if len(res.Skipped) > 0 {
				// 只说「清了 N 条」而不说「还有 M 条没敢动、为什么」，调用方会以为台账已经干净了。
				fmt.Printf("保留 %d 条（实时判据未能确定已终止）：\n", len(res.Skipped))
				for _, it := range res.Skipped {
					fmt.Printf("  · %s  flow=%s  state=%s(实时 %s)\n",
						it.AppID, it.FlowID, it.State, it.ProbeState)
				}
				fmt.Println("  要停它们请先 `gear app stop <app-id>`（prune 只删台账，不杀进程）")
			}
			return nil
		},
	}
	c.Flags().StringVar(&appsDir, "apps-dir", "", "应用台账目录（默认 <registry 同目录>/apps）")
	c.Flags().StringVar(&flowID, "flow", "", "只清该 Flow 的台账项（留空=全部）")
	c.Flags().BoolVar(&dryRun, "dry-run", false, "只汇报将清理哪些，不实际删除")
	return c
}

// appDeleteCmd 注销 App 清单。
//
// 为什么必须有这条命令（2026-09-27）：`registry.DeleteFlow` 的第 4 条守卫会拒绝注销
// 「某个 App 的 entry_flow」，而它给出的处置办法正是「先注销该 App」。CLI 面没有入口的话，
// 那条建议就是句空话——被拦住的人手上是 `gear flow delete`，他能走的只有 Web UI / MCP。
//
// 与 §8.2 的其它注销不同，这里**不做引用守卫**（见 registry.DeleteApp 的注释）：App 只是
// 聚合描述，它引用的 Tool/Flow 是独立实体，要求「先拆掉所有 Tool 才能删清单」会堵死清理路径。
//
// `--cascade`（2026-11-11）：默认只删清单（保持「删清单≠删应用」语义）；加 `--cascade`
// 则连根拔起——一并删 App 拥有的全部 Tool/Flow/Hook（见 registry.DeleteAppCascade），
// 并清理磁盘残留（静态资源目录、应用数据库、运行台账）。这是给「删一个应用要手动逐个清工具、
// 还得手动清库清文件」的清理痛点的一刀切入口，跨 App 引用与运行中守卫仍生效。
func appDeleteCmd() *cobra.Command {
	var cascade bool
	var appsDir string
	cmd := &cobra.Command{
		Use:   "delete <id>",
		Short: "注销 App 清单（--cascade 时一并删其拥有的 Tool/Flow/Hook 并清磁盘）",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			reg, err := openRegistry()
			if err != nil {
				return err
			}
			defer reg.Close()
			if cascade {
				// 先取 App 拿到入口 Flow，稍后用于精确匹配运行台账记录（cascade 会删掉 App 本身）。
				appObj, gerr := reg.GetApp(args[0])
				if gerr != nil {
					return gerr
				}
				entryFlow := ""
				if appObj != nil {
					entryFlow = appObj.EntryFlow
				}
				removed, err := reg.DeleteAppCascade(args[0], act())
				if err != nil {
					return err
				}
				fmt.Printf("✓ 已级联注销 App %s 及其 %d 个子资源:\n", args[0], len(removed)-1)
				for _, id := range removed {
					fmt.Printf("  - %s\n", id)
				}
				fmt.Println("正在清理磁盘残留（静态资源 / 台账 / 数据库）…")
				cleanupAppDisk(registryPath, appsDir, args[0], entryFlow)
				return nil
			}
			if err := reg.DeleteApp(args[0], act()); err != nil {
				return err
			}
			fmt.Printf("✓ 已注销 App: %s\n", args[0])
			return nil
		},
	}
	cmd.Flags().BoolVar(&cascade, "cascade", false, "一并删掉该 App 拥有的 Tool/Flow/Hook（连根拔起）")
	cmd.Flags().StringVar(&appsDir, "apps-dir", "", "应用台账目录（默认 <registry 同目录>/apps；用于定位静态资源与台账）")
	return cmd
}

// cleanupAppDisk 在级联注销 registry 成功后，清掉该 App 在磁盘上的残留：
//   - <appsDir>/<appId>/        ：import 落地的静态资源（前端 dist 等），以 App id 命名、专属本应用
//   - <appsDir>/*.json 台账中 FlowID 命中该 App 入口 Flow 的记录：本应用的运行台账
//   - <AppDataDir>/<appId>.db   ：该 App 默认可持久化数据库（dsn 恰好等于 <appId> 时）
//
// 这些都是「与 App 一一对应、专属于它」的路径，删它们不会误伤其它应用。
// 磁盘清理是 best-effort：registry 已删，即使某个路径删不掉也不该让命令失败，
// 但每个动作都要如实打印，让人知道还留了什么、为什么。
func cleanupAppDisk(registryPath, appsDir, appID, entryFlow string) {
	if dir, err := app.ResolveAppsDir(registryPath, appsDir); err == nil {
		// 1) 静态资源目录（以 App id 命名）
		staticDir := filepath.Join(dir, appID)
		if removeIfExists(staticDir) {
			fmt.Printf("  ✓ 已删静态资源目录: %s\n", staticDir)
		} else {
			fmt.Fprintf(os.Stderr, "⚠ 未发现静态资源目录 %s（应用若从未 import 静态资源则正常）\n", staticDir)
		}
		// 2) 运行台账：删 FlowID 命中入口 Flow 的记录（精确匹配，不靠猜文件名）
		if recs, rerr := app.ListRecords(dir); rerr == nil {
			for _, rec := range recs {
				if rec.FlowID == entryFlow {
					p := filepath.Join(dir, rec.AppID+".json")
					if removeIfExists(p) {
						fmt.Printf("  ✓ 已删台账记录: %s\n", p)
					}
				}
			}
		}
	} else {
		fmt.Fprintf(os.Stderr, "⚠ 无法解析应用目录，跳过静态资源与台账清理: %v\n", err)
	}

	// 3) 业务数据库（相对 dsn 锚定到 AppDataDir；只有 dsn 恰为 <appId>.db 时才命中）
	dataDB := filepath.Join(gearhome.AppDataDir(), appID+".db")
	if removeIfExists(dataDB) {
		fmt.Printf("  ✓ 已删应用数据库: %s\n", dataDB)
	} else {
		fmt.Fprintf(os.Stderr, "⚠ 未找到 %s（若工具用了其它 dsn 名，请手动清理 %s 下相关 .db）\n",
			dataDB, gearhome.AppDataDir())
	}
}

// removeIfExists 删除一个文件或目录（递归）；不存在返回 false，访问/删除出错打印到 stderr 仍返回 false。
func removeIfExists(path string) bool {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return false
		}
		fmt.Fprintf(os.Stderr, "⚠ 无法访问 %s（跳过）: %v\n", path, err)
		return false
	}
	if err := os.RemoveAll(path); err != nil {
		fmt.Fprintf(os.Stderr, "⚠ 删除 %s 失败: %v\n", path, err)
		return false
	}
	return true
}

// printAppStatus 以统一格式打印一条状态（status 与 list 共用，避免两处格式漂移）。
func printAppStatus(s app.StatusResult) {
	fmt.Printf("%-12s %s  pid=%d  flow=%s\n", s.State, s.AppID, s.PID, s.FlowID)
	if s.UptimeS > 0 {
		fmt.Printf("  uptime: %ds  started_at: %s\n", s.UptimeS, s.StartedAt.Format("2006-01-02T15:04:05Z07:00"))
	}
	for _, p := range s.Ports {
		fmt.Printf("  port %-6d listening=%v\n", p.Port, p.Listening)
	}
	for _, u := range s.OpenURLs {
		fmt.Printf("  打开地址：%s\n", u)
	}
	if s.LogPath != "" {
		fmt.Printf("  log: %s\n", s.LogPath)
	}
	for _, ln := range s.LastLogs {
		fmt.Printf("  │ %s\n", ln)
	}
}
