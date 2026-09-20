package cli

// mcp.go 提供 `gear mcp`：把内核的目录管理 / 静态校验 / 执行能力经 Model Context Protocol
// 暴露给任意 MCP 客户端（Claude Desktop、WorkBuddy、Cursor …），让外部智能体用标准协议
// 完成「读目录 → 写配置 → 校验 → 跑一把 → 看结果」。
//
// 两种 transport：
//   - stdio（默认）：客户端以子进程方式拉起（`command: gear, args: [mcp]`）。stdio 下
//     **stdout 只承载协议帧**，任何日志/提示必须走 stderr，否则污染协议。
//   - streamable HTTP（`--http :8788`）：供远程智能体或 MCP 连接器接入，端点 /mcp。
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

// mcpCmd 构造 `gear mcp` 子命令。
func mcpCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "以 MCP (Model Context Protocol) 执行面暴露 Gear",
		Long: "启动 Gear 的 MCP Server，把 Tool/Flow/Hook 的目录管理、静态校验与执行能力暴露给任何 MCP 客户端（智能体）。\n" +
			"默认 stdio transport（客户端配置：{\"mcpServers\": {\"gear\": {\"command\": \"gear\", \"args\": [\"mcp\"]}}}）。\n" +
			"加 --http :port 则改走 streamable HTTP transport（供远程智能体或 WorkBuddy 的 MCP 连接器接入）。\n" +
			"已注册的 Flow 会被反向暴露为 flow__{id} 工具，外部智能体可直接调用你的编排。",
		Args: cobra.NoArgs,
		RunE: runMCP,
	}
	cmd.Flags().StringVar(&mcpHTTPAddr, "http", "",
		"以 streamable HTTP transport 暴露（格式 host:port，如 :8788）；留空则用 stdio")
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

	srv, err := gearmcp.New(reg, eng)
	if err != nil {
		return err
	}
	defer srv.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if mcpHTTPAddr != "" {
		// 提示走 stderr：HTTP 模式下 stdout 也没有协议帧，但保持「诊断一律 stderr」的口径，
		// 免得将来有人在 stdio 模式下照抄这行把协议打脏。
		fmt.Fprintf(os.Stderr, "gear mcp: streamable HTTP transport 监听 %s（端点 /mcp）\n", mcpHTTPAddr)
		return srv.ServeHTTP(ctx, mcpHTTPAddr)
	}
	fmt.Fprintln(os.Stderr, "gear mcp: stdio transport 就绪（stdout 只承载协议帧，日志一律走 stderr）")
	return srv.Run(ctx, &mcp.StdioTransport{})
}
