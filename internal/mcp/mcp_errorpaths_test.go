package mcp

import (
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"gear/internal/engine"
	"gear/internal/registry"
)

// 本文件补两块此前完全没人走的区域：
//   1. ServeHTTP —— streamable HTTP transport 的**监听与退出**路径。此前的测试只走
//      httpHandler()（拿 handler 自己起 httptest），于是「ListenAndServe + 信号
//      优雅退出」这 12 条语句一直是 0%，而它恰恰是 `gear mcp --http` 的真实入口。
//   2. 各 handler 的**错误分支**——「id 不存在必须报错，绝不静默成功」是 Gear 的一贯
//      口径，错误分支不测等于没测。

// newServeFixture 装配一个可直接起监听的 MCP Server（registry + 引擎 + 扩展已加载）。
func newServeFixture(t *testing.T) (*registry.Registry, *engine.Engine, *Server) {
	t.Helper()
	t.Setenv("GEAR_EXEC_DB", filepath.Join(t.TempDir(), "exec.db"))
	reg, err := registry.Open(filepath.Join(t.TempDir(), "registry.db"))
	if err != nil {
		t.Fatalf("打开 registry 失败: %v", err)
	}
	capReg := engine.NewCapabilityRegistry()
	agent, err := engine.SetupExtensions(capReg, reg, "")
	if err != nil {
		t.Fatalf("初始化扩展失败: %v", err)
	}
	eng := engine.New(reg, engine.WithCapabilities(capReg), engine.WithAIAgent(agent))
	s, err := New(reg, eng)
	if err != nil {
		t.Fatalf("构造 MCP Server 失败: %v", err)
	}
	return reg, eng, s
}

// freeAddr 取一个当前空闲的地址（监听后立即关闭，供 ServeHTTP 复用该端口）。
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("取空闲端口失败: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

func TestServeHTTPListensAndShutsDownOnCtxCancel(t *testing.T) {
	reg, _, s := newServeFixture(t)
	defer func() { _ = s.Close(); _ = reg.Close() }()

	addr := freeAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- s.ServeHTTP(ctx, addr) }()

	// 轮询直到端口真的在监听（ServeHTTP 是阻塞调用，起监听需要一点时间）。
	deadline := time.Now().Add(10 * time.Second)
	for {
		conn, derr := net.DialTimeout("tcp", addr, 500*time.Millisecond)
		if derr == nil {
			_ = conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("ServeHTTP 未在 10s 内开始监听 %s: %v", addr, derr)
		}
		time.Sleep(50 * time.Millisecond)
	}

	// 真连一次：streamable HTTP 下 ListTools 必须可用（与 stdio 同一套工具面）。
	client := mcp.NewClient(&mcp.Implementation{Name: "http-smoke", Version: "0"}, nil)
	hs, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: "http://" + addr}, nil)
	if err != nil {
		t.Fatalf("连接 ServeHTTP 暴露的端点失败: %v", err)
	}
	tools, err := hs.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil || len(tools.Tools) == 0 {
		t.Fatalf("ServeHTTP 应暴露工具面，实际 (%d 个, err=%v)", len(tools.Tools), err)
	}
	_ = hs.Close()

	// ctx 取消 → 优雅退出；ErrServerClosed 必须被吞掉（返回 nil 而不是「退出即报错」）。
	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("ctx 取消后 ServeHTTP 应优雅退出并返回 nil，实际 %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ServeHTTP 在 ctx 取消后 10s 内未退出")
	}
}

// TestServeHTTPRejectsUnusableAddr 覆盖「监听失败即上抛」分支：地址已被占用时必须
// 返回错误——绝不「端口没起来但退出码 0」这种假成功。
func TestServeHTTPRejectsUnusableAddr(t *testing.T) {
	reg, _, s := newServeFixture(t)
	defer func() { _ = s.Close(); _ = reg.Close() }()

	ln, err := net.Listen("tcp", "127.0.0.1:0") // 先占住端口
	if err != nil {
		t.Fatalf("占端口失败: %v", err)
	}
	defer ln.Close()

	if err := s.ServeHTTP(context.Background(), ln.Addr().String()); err == nil {
		t.Fatal("端口已被占用时 ServeHTTP 必须返回错误（不静默假成功）")
	}
}

// TestMCPErrorPathsReportInsteadOfSilentSuccess 「不存在」类入参必须报错。
func TestMCPErrorPathsReportInsteadOfSilentSuccess(t *testing.T) {
	session := newSmokeSession(t)

	cases := []struct {
		tool, want string
		args       map[string]any
	}{
		{"get_tool", "Tool 不存在", map[string]any{"id": "no_such_tool"}},
		{"get_flow", "Flow 不存在", map[string]any{"id": "no_such_flow"}},
		{"validate_flow", "Flow 不存在", map[string]any{"id": "no_such_flow"}},
		{"get_execution", "执行记录不存在", map[string]any{"id": "no_such_exec"}},
		{"get_execution_logs", "执行记录不存在", map[string]any{"id": "no_such_exec"}},
		{"delete_tool", "", map[string]any{"id": "no_such_tool"}},
		{"delete_flow", "", map[string]any{"id": "no_such_flow"}},
		{"delete_hook", "", map[string]any{"id": "no_such_hook"}},
		{"run_flow", "Flow 不存在", map[string]any{"id": "no_such_flow"}},
		{"run_tool", "", map[string]any{"id": "no_such_tool"}},
		{"promote_tool", "非法目标状态", map[string]any{"tool_id": "x", "version": "1.0.0", "target": "bogus"}},
	}
	for _, c := range cases {
		expectToolError(t, session, c.tool, c.args, c.want)
	}
}

// TestMCPRunFlowRejectsServiceFlow Flow 含 service 长驻 step 时，MCP 同步运行面必须
// 拒绝（须 gear app run 监管），与 REQ-FLOW-RUN-REJECTS-SERVICE 同口径。
func TestMCPRunFlowRejectsServiceFlow(t *testing.T) {
	session := newSmokeSession(t)
	var created map[string]any
	callTool(t, session, "create_tool", map[string]any{"yaml": smokeToolYAML}, &created)
	callTool(t, session, "create_flow", map[string]any{"yaml": svcFlowYAML}, &created)
	expectToolError(t, session, "run_flow", map[string]any{"id": "svc_demo"}, "service")

	// service Flow 不反向暴露为同步工具：flow__svc_demo 根本不该出现在工具面里
	// （未知工具在协议层就报错，故用 ListTools 断言「不存在」，而不是 CallTool）。
	tools, err := session.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("ListTools 失败: %v", err)
	}
	for _, tt := range tools.Tools {
		if tt.Name == "flow__svc_demo" {
			t.Fatal("service 长驻 Flow 不得反向暴露为同步 MCP 工具（会常驻不结束）")
		}
	}
}
