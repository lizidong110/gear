package cli

import (
	"net"
	"testing"
	"time"
)

// timeoutAfter 是 stdio 用例的兜底等待：卡住即失败（而不是让整个测试包挂到超时）。
func timeoutAfter() <-chan time.Time { return time.After(30 * time.Second) }

// 本文件覆盖 `gear mcp` 的启动路径（runMCP）——此前 0% 覆盖，而它是 WorkBuddy/Claude
// 等客户端实际拉起 Gear 的入口（`command: gear, args: [mcp]`）。
// 两条 transport 各测一次：**装配链路（registry → cliEngine → MCP Server）是共用的**，
// 只测一条就不知道另一条有没有接错。

// TestMCPHTTPTransportReportsBindFailure：`--http` 指向已被占用的地址时必须报错退出，
// 绝不「端口没起来但退出码 0」——那会让客户端以为服务在跑。
func TestMCPHTTPTransportReportsBindFailure(t *testing.T) {
	withTempRegistry(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("占端口失败: %v", err)
	}
	defer ln.Close()

	if err := runCLI(t, "mcp", "--http", ln.Addr().String()); err == nil {
		t.Fatal("端口已被占用时 `gear mcp --http` 必须报错退出（不静默假成功）")
	}
}

// TestMCPStdioTransportRunsUntilInputEOF：stdio 是默认 transport。
// go test 下进程 stdin 是空设备，协议读到 EOF 即正常结束——借此走通「装配 → 起服务 → 收尾」
// 全链路；若它卡住不返回，说明 Run 没有正确响应 EOF（客户端断开后进程不退，会漏 sidecar）。
func TestMCPStdioTransportRunsUntilInputEOF(t *testing.T) {
	withTempRegistry(t)
	done := make(chan error, 1)
	go func() { done <- runCLI(t, "mcp") }()

	select {
	case err := <-done:
		// EOF 属正常断开：不应报错（否则客户端正常断开也被当成故障）
		if err != nil {
			t.Fatalf("stdio transport 在输入 EOF 后应正常收尾，实际 %v", err)
		}
	case <-timeoutAfter():
		t.Fatal("stdio transport 在 stdin EOF 后未退出（客户端断开后进程会残留）")
	}
}
