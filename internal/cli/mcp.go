package cli

// mcp.go 提供 `gear mcp`：把内核的目录管理 / 静态校验 / 执行能力经 Model Context Protocol
// 暴露给任意 MCP 客户端（Claude Desktop、WorkBuddy、Cursor …），让外部智能体用标准协议
// 完成「读目录 → 写配置 → 校验 → 跑一把 → 看结果」。
//
// 两种 transport：
//   - stdio（默认）：客户端以子进程方式拉起（`command: gear, args: [mcp]`）。stdio 下
//     **stdout 只承载协议帧**，任何日志/提示必须走 stderr，否则污染协议。
//   - streamable HTTP（`--http 127.0.0.1:8788`）：供远程智能体或 MCP 连接器接入，端点 /mcp。
//     默认**拒绝非回环地址**（MCP 面无鉴权），确有远程需求时才显式加 --allow-remote。
//     跨站请求由 httpsafe.OriginAllowed 挡掉（与 REST 面同一判据）。
//
// 服务对象因 transport 而异但引擎一致：都走 cliEngine（与 `gear exec` 同一构造路径），
// 所以密钥源（GEAR_SECRET_*）、扩展目录、守卫口径与 CLI/REST 完全一致，不会出现
// 「MCP 路径少了某个装配」的漂移。

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	gearmcp "gear/internal/mcp"
)

// mcpHTTPAddr 是 `--http` 的目标地址（host:port）。留空 = 用 stdio transport。
var mcpHTTPAddr string

// mcpExpose 是 `--expose {flows,tools,all}` 的原值，启动前经 ParseExposeMode 校验。
//
// 默认空 = 两种反向暴露都关闭（MCP_AGENT_DESIGN.md §6.1）：flow__{id} 是**启动时快照**
// （新建 Flow 本次不可用）、与 gear_run 语义重复、且让工具数不可预测——三者都损害工具
// 选择准确率。tools 模式不同：它按 App 清单的 mcp.tools 白名单暴露，工具数启动期确定，
// 但必须同时给 --scope <appId>。
var mcpExpose string

// mcpAppsDir 是应用台账目录（gear_app / gear app 共用）；留空 = <registry 同目录>/apps。
var mcpAppsDir string

// mcpAllowRemote 允许 HTTP transport 监听**非回环**地址（默认 false）。
//
// 默认关闭是刻意的：MCP 面无鉴权，绑 0.0.0.0 会把「写 registry / 删对象 / 执行编排」的能力
// 暴露给局域网。需要远程接入时必须显式打开这层开关（评审 B-2）。
var mcpAllowRemote bool

// mcpOnlyExposed = `--only-exposed`：只暴露策展/快照工具，不挂 15 个常驻 gear_*。
// 用途是给终端客户端一个干净的应用专用面（否则 5 个业务工具会连同 registry 写面一起交出去）。
var mcpOnlyExposed bool

// mcpCmd 构造 `gear mcp` 子命令。
func mcpCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "以 MCP (Model Context Protocol) 执行面暴露 Gear",
		Long: "启动 Gear 的 MCP Server，把 Tool/Flow/Hook 的目录管理、静态校验、执行与**应用生命周期**" +
			"暴露给任何 MCP 客户端（智能体）。\n" +
			"默认 stdio transport（客户端配置：{\"mcpServers\": {\"gear\": {\"command\": \"gear\", \"args\": [\"mcp\"]}}}）。\n" +
			"加 --http 127.0.0.1:port 则改走 streamable HTTP transport（非回环地址默认拒绝，需 --allow-remote）。\n" +
			"工具面共 15 个 gear_ 前缀工具，另通过 ServerOptions.Instructions 常驻下发心智模型。\n" +
			"--expose {flows,tools,all} 开启反向暴露：flows=已注册 Flow 快照；" +
			"tools=按 --scope 指向的 App 清单 mcp.tools 白名单暴露注册 Tool（须同时给 --scope，且该 App 必须写了策展区）。\n" +
			"--only-exposed 关掉 15 个常驻 gear_* 工具，只留反向暴露的工具（应用专用面，须配 --expose）。\n" +
			"执行进度与全部诊断一律走 stderr（stdio 下 stdout 只承载协议帧）。\n" +
			"gear_app(action=start) 会**派生独立后台进程**监管应用；它是否跨会话存活**以返回的 escaped_job 为准**" +
			"（false 时随本进程退出，不要向用户承诺长期可用），停止用 gear_app(action=stop)。",
		Args: cobra.NoArgs,
		RunE: runMCP,
	}
	cmd.Flags().StringVar(&mcpHTTPAddr, "http", "",
		"以 streamable HTTP transport 暴露（格式 host:port，如 127.0.0.1:8788）；留空则用 stdio")
	cmd.Flags().BoolVar(&mcpAllowRemote, "allow-remote", false,
		"允许 --http 监听非回环地址（默认拒绝：MCP 面无鉴权，绑全网卡会暴露写/删/执行能力）")
	cmd.Flags().StringVar(&mcpExpose, "expose", "",
		"反向暴露面：flows=已注册 Flow 快照 | tools=App 清单 mcp.tools 策展工具（须配 --scope） | all=两者。留空=全关（默认）")
	cmd.Flags().BoolVar(&mcpOnlyExposed, "only-exposed", false,
		"只暴露 --expose 带进来的工具，不挂 15 个常驻 gear_*（应用专用面；须配 --expose，默认关闭）")
	cmd.Flags().StringVar(&mcpAppsDir, "apps-dir", "",
		"应用台账目录（默认 <registry 同目录>/apps）")
	return cmd
}

// runMCP 装配引擎 + MCP Server 并阻塞到 ctx 结束（Ctrl-C / SIGTERM / 客户端断开）。
func runMCP(_ *cobra.Command, _ []string) error {
	reg, err := openRegistry()
	if err != nil {
		return err
	}
	defer reg.Close()

	eng, err := cliEngine(reg)
	if err != nil {
		return err
	}
	// 未知值必须在这里就失败：静默当成「不暴露」会让客户端看到一份缺工具的工具面，
	// 却不知道自己拼错了开关（不静默降级）。
	expose, err := gearmcp.ParseExposeMode(mcpExpose)
	if err != nil {
		return err
	}

	// 注意：执行进度改道（stdout → stderr）**不在这里做**，而在 gearmcp.New 内部。
	// 「stdio 下 stdout 只承载协议帧」是 MCP 面的协议约束，须由 MCP 面自己保证
	// （这样任何 embedding 都成立），而不是依赖每个调用方都记得补一行。见 REQ-MCP-STDIO-PURE。

	// 显式把 registry 路径交给 MCP Server：gear_app 派生子进程时**必须**继承它，
	// 否则子进程读默认 ~/.gear/registry.db，可能启动另一个库里的同名 Flow（最难排查的一类错）。
	srv, err := gearmcp.New(reg, eng,
		gearmcp.WithRegistryPath(registryPath),
		gearmcp.WithAppsDir(mcpAppsDir),
		gearmcp.WithExpose(expose),
		gearmcp.WithScope(cliScope),
		gearmcp.WithAllowRemote(mcpAllowRemote),
		gearmcp.WithOnlyExposed(mcpOnlyExposed))
	if err != nil {
		return err
	}
	defer srv.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if mcpHTTPAddr != "" {
		// 提示走 stderr：HTTP 模式下 stdout 也没有协议帧，但保持「诊断一律 stderr」的口径，
		// 免得将来有人在 stdio 模式下照抄这行把协议打脏。
		// 已过 checkBindAddr：能走到这里说明是回环地址，或使用者显式开了 --allow-remote。
		fmt.Fprintf(os.Stderr, "gear mcp: streamable HTTP transport 监听 %s（端点 /mcp；当前无鉴权，跨站请求已按 Origin 拒绝）\n", mcpHTTPAddr)
		return srv.ServeHTTP(ctx, mcpHTTPAddr)
	}
	fmt.Fprintln(os.Stderr, "gear mcp: stdio transport 就绪（stdout 只承载协议帧，日志一律走 stderr）")
	return srv.Run(ctx, &mcp.StdioTransport{})
}
