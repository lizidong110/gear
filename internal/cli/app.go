package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"gear/internal/app"
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
	c.AddCommand(appRunCmd(), appStartCmd(), appStopCmd(), appStatusCmd(), appListCmd(), appPruneCmd())
	return c
}

// appRunCmd 启动并监管一个或多个 Flow 进程（S2 实现：长驻 step + restart + 信号停止）。
// 设计见 PRD §14：一个 app run = 一个进程，负责整个 Flow 生命周期；
// 服务 = 长驻不退出的 Flow，并发 = 启动多个 app run（或一次传多个 flow-id 受 maxConcurrent 约束）。
func appRunCmd() *cobra.Command {
	var restart string
	var maxConc int
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

			fmt.Printf("[app run] 监管 %d 个 Flow，restart=%s，maxConcurrent=%d（Ctrl+C 停止）\n",
				len(args), policy, maxConc)
			eng, err := cliEngine(reg)
			if err != nil {
				return err
			}
			sup := app.New(eng, policy, maxConc)
			if err := sup.Supervise(ctx, args); err != nil {
				return err
			}
			fmt.Println("[app run] 已停止")
			return nil
		},
	}
	c.Flags().StringVar(&restart, "restart", "always", "service 退出后的重启策略：always / never")
	c.Flags().IntVar(&maxConc, "max", 0, "同时启动的 Flow 上限（0=不限；S2 并发闸门，进程池留 S4）")
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
			})
			if err != nil {
				return err
			}
			fmt.Printf("%s  %s  pid=%d\n", res.State, res.AppID, res.PID)
			fmt.Printf("  flow: %s\n  log:  %s\n", res.FlowID, res.LogPath)
			for _, p := range res.Ports {
				fmt.Printf("  port %-6d listening=%v\n", p.Port, p.Listening)
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
				return fmt.Errorf("应用启动即退出（pid=%d）——上面是日志尾部，常见原因：端口被占、侧车未构建、registry 路径不符",
					res.PID)
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

// printAppStatus 以统一格式打印一条状态（status 与 list 共用，避免两处格式漂移）。
func printAppStatus(s app.StatusResult) {
	fmt.Printf("%-12s %s  pid=%d  flow=%s\n", s.State, s.AppID, s.PID, s.FlowID)
	if s.UptimeS > 0 {
		fmt.Printf("  uptime: %ds  started_at: %s\n", s.UptimeS, s.StartedAt.Format("2006-01-02T15:04:05Z07:00"))
	}
	for _, p := range s.Ports {
		fmt.Printf("  port %-6d listening=%v\n", p.Port, p.Listening)
	}
	if s.LogPath != "" {
		fmt.Printf("  log: %s\n", s.LogPath)
	}
	for _, ln := range s.LastLogs {
		fmt.Printf("  │ %s\n", ln)
	}
}
