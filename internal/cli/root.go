// Package cli 实现 gear 命令行管理面（V1 永久基线，见 PRD §14）。
package cli

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"gear/internal/buildinfo"
	"gear/internal/gearhome"
	"gear/internal/gearlog"
	"gear/internal/registry"
)

var registryPath string

// serverAddr 是 REST 执行面地址（exec / ai 等客户端子命令与 serve 共用，作为持久标志）。
// 默认 127.0.0.1:8787；可用环境变量 GEAR_ADDR 覆盖（便于联调远端执行面）。
var serverAddr string

// cliScope 是运行期命名空间守卫的作用域（`--scope`，IDENTITY_CONTEXT_DESIGN.md §4.3）：
// 一个 registry apps 表里的 App.ID。非空时，本进程运行期只认这个应用名下的定义。
//
// 空值（默认）= 不启用守卫，行为与没有本特性时逐字一致。
//
// 它是**进程级**开关，不是按请求的：gear serve 的引擎是长驻共享实例，而 cap.Resolver
// 的签名里没有 context，按请求切换作用域在那个面上做不到。
var cliScope string

// logFilePath / logLevel 是诊断日志选型（REQ-CLI-DIAG-LOG）：
// 空 path = 不启用（零行为变化，存量脚本逐字不受影响）；给了 path 才开文件归档。
// 与执行进度（Engine.progressf → stdout）正交：进度面向人盯着看，诊断面向事后归档，
// 合体会毁掉「stdout 结果可管道消费」。level 非法是用户输入错误（尽早死）；
// 文件开不了是环境问题（主命令照常跑 + stderr 明说，不静默降级）。
var logFilePath, logLevel string

// cmdStartedAt 是本次命令的起始时刻（Execute 入口记，收尾算耗时）。
var cmdStartedAt time.Time

// newRootCmd 构造根命令（供 CLI 入口与测试复用）。
func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "gear",
		Short: "Gear — 插件化自动化编排框架内核",
		Long:  "Gear 内核：引擎 + ctx.* 宿主能力 + app run/start 启动器。\nTool 是原子能力，Flow 是通用运行单元，服务=长驻不退出的 Flow。\n怀疑二进制过时或环境没准备好时，先跑 `gear version` 与 `gear doctor`。",
		// `--version` 与 `gear version` 同源（cobra 按模板自动生成 flag）。
		// 此前两者都没有，于是「unknown command/flag」无法区分「功能没有」与「二进制旧」。
		Version: buildinfo.Version,
	}
	root.SetVersionTemplate("gear {{.Version}}\n")
	root.PersistentFlags().StringVar(&registryPath, "registry", defaultRegistry(),
		"registry 数据库路径（默认 ~/.gear/registry.db）")
	root.PersistentFlags().StringVar(&serverAddr, "addr", defaultAddr(),
		"REST 执行面地址（默认 127.0.0.1:8787，可用 GEAR_ADDR 覆盖）")
	root.PersistentFlags().StringVar(&cliScope, "scope", "",
		"运行期命名空间守卫的作用域（registry 里的 App.ID）：本进程只引用它名下的定义；空=不启用")
	root.PersistentFlags().StringVar(&logFilePath, "log-file", "",
		"诊断日志文件路径（可选）：记录命令生命周期与内核事件的归档档案；空=不写（默认）")
	root.PersistentFlags().StringVar(&logLevel, "log-level", "",
		"诊断日志级别 debug|info|warn|error（可选，默认 info；仅 --log-file 给了才生效）")

	// PersistentPreRunE 是**每条命令执行前**的统一入口（cobra 只会调离执行命令最近的一个，
	// 挂在 root 上即全树生效）——诊断日志在这里起，后面的告警/错误才有档案可落。
	// 注意 help/version 不经过 RunE，也不经过这里：它们无诊断价值，不留档案。
	root.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		// level 先单独校验：写错级别是**用户输入错误**，直接失败；
		// 而 Setup 内部的文件错误走降级（下面分开处置）——两类失败处置相反。
		if _, err := gearlog.ParseLevel(logLevel); err != nil {
			return err
		}
		if _, err := gearlog.Setup(logFilePath, logLevel); err != nil {
			// 路径开不了（权限/被占/是目录）：主命令照常跑，但必须明说没档案——
			// 静默降级会让排障时对着一个不存在的文件猜（见 gearlog 包注释红线 2）。
			fmt.Fprintf(os.Stderr, "⚠ 诊断日志未启用（%v）：本次运行不留档案，主命令继续\n", err)
		}
		cwd, _ := os.Getwd()
		gearlog.L().Info("命令开始",
			slog.String("cmd", cmd.CommandPath()),
			slog.Int("pid", os.Getpid()),
			gearlog.Attr("cwd", cwd),
			slog.Any("invocation", redactedInvocation(cmd, args)),
			gearlog.Attr("registry", registryPath),
			slog.String("version", buildinfo.Version),
		)
		return nil
	}

	// ⚠ 必须保持**单行**：tools/doc_audit.py 的 CLI 双向核对按这一行的文本提取子命令集，
	//    拆成多行会让它只读到第一行的两个命令，进而把其余十几个全报成「文档有、代码无」。
	root.AddCommand(toolCmd(), flowCmd(), appCmd(), hookCmd(), serveCmd(), execCmd(), aiCmd(), mcpCmd(), extCmd(), scaffoldCmd(), configCmd(), secretCmd(), exportCmd(), importCmd(), applyCmd(), historyCmd(), versionCmd(), doctorCmd())
	return root
}

// defaultAddr 返回 --addr 的默认值：优先环境变量 GEAR_ADDR，否则内置默认。
func defaultAddr() string {
	if v := os.Getenv("GEAR_ADDR"); v != "" {
		return v
	}
	return "127.0.0.1:8787"
}

// Execute 是 CLI 入口。
func Execute() {
	cmdStartedAt = time.Now()
	err := newRootCmd().Execute()
	logCmdFinish(err)
	// `gear doctor` 用哨兵 error 表达「自检跑完了、但有告警」：退出码仍要给 1
	//（脚本要能判断），但不打印错误文案——告警已经写在报告里了，重复一遍只是噪声。
	if err != nil {
		if errors.Is(err, errSilentFailure) {
			os.Exit(1)
		}
		// 未知子命令时补一句可判定的信息（version.go）：让人能区分「命令真没有」
		// 与「二进制旧 / PATH 指错」——2026-10-03 实测的卡死点就在这里。
		if hint := unknownCommandHint(err); hint != "" {
			fmt.Fprintln(os.Stderr, hint)
		}
		os.Exit(1)
	}
}

// logCmdFinish 是命令收尾的档案行（成功/失败都记）。
//
// 抽成包级函数而不是内联在 Execute 里：**任何驱动 newRootCmd 的调用方都该拿到收尾行**
// （测试就是这么绕过一次的——失败没有档案比没有开始更难查）。cobra 没有
// 「无论成败」的钩子（PersistentPostRunE 只在成功路径跑），所以收尾只能由
// 驱动方显式调用：Execute 是生产路径，测试助手 runCLI 是测试路径。
func logCmdFinish(err error) {
	elapsed := time.Since(cmdStartedAt)
	if err != nil {
		// 先落档案再决定退出码：errSilentFailure（doctor 有告警）不打印错误文案，
		// 但档案里必须有——「静默退出」在事后回看时是最难查的一类。
		gearlog.L().Error("命令失败", slog.String("err", err.Error()), slog.Duration("elapsed_ms", elapsed))
		return
	}
	gearlog.L().Info("命令完成", slog.Duration("elapsed_ms", elapsed))
}

// redactedInvocation 把本次调用渲染成可归档的一行：显式给的 flag（值过脱敏）+ 位置参数。
//
// 为什么记这个：排障的第一个问题永远是「他到底跑了什么」。os.Args 原样记会把
// --api-key 的密钥写进档案（gearlog 红线 3），所以逐 flag 过 RedactValue；
// 位置参数（任务描述）截断 200 字符——那是内容不是标识，全量进档案只会淹掉关键行。
func redactedInvocation(cmd *cobra.Command, args []string) string {
	var parts []string
	cmd.Flags().Visit(func(f *pflag.Flag) {
		parts = append(parts, "--"+f.Name+"="+gearlog.RedactValue(f.Name, f.Value.String()))
	})
	if len(args) > 0 {
		joined := strings.Join(args, " ")
		const maxPosArg = 200
		if len([]rune(joined)) > maxPosArg {
			joined = string([]rune(joined)[:maxPosArg]) + "…"
		}
		parts = append(parts, "args="+joined)
	}
	if len(parts) == 0 {
		return "(无参数)"
	}
	return strings.Join(parts, " ")
}

// defaultRegistry 返回 --registry 的默认值。
//
// 委托给 gearhome：registry、执行记录库、应用台账必须同源（见该包注释——三者此前各有
// 一份「相对 cwd」的默认值，是同一类缺陷的三个实例）。
func defaultRegistry() string {
	return gearhome.RegistryPath()
}

// openRegistry 确保目录存在并打开 registry。
//
// 顺带做两次「误用护栏」：
//   - --registry 指向的应是 gear 的**目录库**（默认 ~/.gear/registry.db），不是用户应用自己的
//     数据库。gear 会在此文件建 tools/flows/hooks 三张表；若该文件已含其它表（业务库），
//     只警告、不阻断（有人可能有意共用），避免把业务库当注册表还不自知。
//   - 值是**相对路径**时警告，因为 cwd 是调用方的属性、不是 gear 的属性（详见
//     warnIfRelativeRegistryPath）。
//
// 最后把生效路径钉进 GEAR_REGISTRY：**这里是「本进程决定用哪个库」的唯一分叉点**，
// 而 cmdline 的子进程环境（os.Environ 基座）与 app 的 detached 派生都从本进程继承，
// 于是「我用的库」自动变成「我派生的一切用的库」。
func openRegistry() (*registry.Registry, error) {
	dir := filepath.Dir(registryPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	warnIfPosixRegistryPath(registryPath)
	warnIfRelativeRegistryPath(registryPath)
	warnIfForeignRegistry(registryPath)
	pinRegistryEnv(registryPath)
	return registry.Open(registryPath)
}

// pinRegistryEnv 把本次调用生效的 registry 绝对化后写进 GEAR_REGISTRY，供子进程继承。
//
// 为什么必须绝对化：子进程的 cwd 未必与本进程相同，把相对值原样传下去等于把
// 「按 cwd 解析」这个不确定性一并传下去——那正是本机制要消除的东西。
//
// 为什么是环境变量而不是命令行参数：需要继承的不只是 `gear app start` 派生出的监管进程
// （那条路已显式拼 `--registry`），还有**监管进程内部 engine 拉起的长驻 Tool 进程**——
// 后者的命令行完全由 Tool 定义决定，内核无从插手。环境变量是唯一不要求改 entry 的信道。
func pinRegistryEnv(path string) {
	// URI 形态（file:… / 含 ?query）不做路径处理，也不下传：子进程的语义未必相同。
	if strings.Contains(path, "://") || strings.HasPrefix(path, "file:") {
		return
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		fmt.Fprintf(os.Stderr,
			"⚠ 无法绝对化 registry 路径 %q（%v）：派生出的子进程会退回各自的默认 registry，可能指向另一个库\n",
			path, err)
		return
	}
	if err := os.Setenv(gearhome.EnvRegistry, abs); err != nil {
		fmt.Fprintf(os.Stderr,
			"⚠ 无法设置 %s（派生出的子进程会退回各自的默认 registry，可能指向另一个库）: %v\n",
			gearhome.EnvRegistry, err)
	}
}

// gearRegistryTables 是 gear registry 自建的表；出现其它表说明该文件不是单纯的 gear 目录库。
// 2026-09-26：加 "apps"（App 清单入 registry），漏掉会让每个已有库都误报「已含非 gear 表」。
// 同日加 "definition_history"（④ 的留旧表）时漏了这一处，导致每个跑过 history 的库
// 每次 CLI 调用都刷一行告警；这张表是 gear 自建的，与 tools/flows/hooks 同级。
var gearRegistryTables = map[string]bool{
	"tools": true, "flows": true, "hooks": true, "apps": true, "definition_history": true,
}

// warnIfPosixRegistryPath 对 --registry 做一次「POSIX 路径误用」提示（不阻断）。
//
// 2026-09-21 实测：`gear app start --registry /d/x/reg.db` 会把注册库落到 `D:\d\x\reg.db`
// ——Windows 把 `/d/...` 当成「当前盘符根下的绝对路径」，`filepath.IsAbs` 也说它合法，
// 于是**静默**换了一个库。智能体与人都只会看到「Flow 不存在」这种下游症状。
// 判据与真实落点由 gearhome.PosixPathHint 提供（与 build 脚本踩的是同一个坑）。
func warnIfPosixRegistryPath(path string) {
	warnIfPosixPath("--registry", path)
}

// warnIfRelativeRegistryPath 提示「--registry 用了相对路径」（不阻断）。
//
// 2026-09-22 实测：同一个根因发作过两次，都是**静默**的——
//
//	① `gear app start --registry build/reg.db`：台账落在 `<cwd>/build/`，换个 cwd 再查就「没有应用」；
//	② 应用自带 REST 面的 `web_ui.api` Tool entry 写死 `./gear.exe serve --registry ./ui-registry.db`：
//	   该子进程按自己的 cwd 解析，读到的是另一次实验留下的空库 ⇒ 页面列表永远为空，日志一个字都没有。
//
// cwd 是调用方的属性，不是 gear 的属性（同 gearhome 包注释的红线）。相对值本身不是错
// （在仓库里手跑时很自然），但它把「哪个库」这件事交给了运行时位置，就该被点出来。
func warnIfRelativeRegistryPath(path string) {
	if msg := relativeRegistryPathWarning(path); msg != "" {
		fmt.Fprint(os.Stderr, msg)
	}
}

// relativeRegistryPathWarning 构造提示文案（空串=不必提示）。
//
// 与打印分开是为了可断言：文案本身承载判据（真实落点、传给子进程的变量名），
// 而「写 stderr 这一行」不需要测试。
func relativeRegistryPathWarning(path string) string {
	// Windows 上 `/d/x/reg.db` 被 filepath.IsAbs 判为合法（POSIX 路径另有专门提示），
	// 这里只管真正的相对值。
	if path == "" || filepath.IsAbs(path) {
		return ""
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = "（无法绝对化）"
	}
	return fmt.Sprintf(
		"⚠ --registry %q 是相对路径：它按**调用时的 cwd** 解析，换个 cwd 就会静默指向另一个库。\n"+
			"  本次已按当前 cwd 解析为 %s，并经 %s 传给派生的子进程；要长期可复现请直接写绝对路径。\n",
		path, abs, gearhome.EnvRegistry)
}

// warnIfPosixPath 是通用的 POSIX 路径误用提示，供各路径类 flag 复用。
func warnIfPosixPath(flag, value string) {
	if h := gearhome.PosixPathHint(value); h != "" {
		fmt.Fprintf(os.Stderr, "⚠ %s：%s\n", flag, h)
	}
}

func warnIfForeignRegistry(path string) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return
	}
	defer db.Close()
	rows, err := db.Query("SELECT name FROM sqlite_master WHERE type='table'")
	if err != nil {
		return
	}
	defer rows.Close()
	var foreign []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return
		}
		if gearRegistryTables[n] || strings.HasPrefix(n, "sqlite_") {
			continue
		}
		foreign = append(foreign, n)
	}
	if len(foreign) > 0 {
		fmt.Fprintf(os.Stderr,
			"⚠ %s 已含非 gear 表 %v —— --registry 应是 gear 的目录库（默认 ~/.gear/registry.db），不是你应用的数据库。\n",
			path, foreign)
	}
}
