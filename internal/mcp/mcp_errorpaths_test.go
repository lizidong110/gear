package mcp

import (
	"context"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"gear/internal/engine"
	"gear/internal/registry"
)

// 本文件补两块此前完全没人走的区域：
//  1. ServeHTTP —— streamable HTTP transport 的**监听与退出**路径。此前的测试只走
//     httpHandler()（拿 handler 自己起 httptest），于是「ListenAndServe + 信号
//     优雅退出」这 12 条语句一直是 0%，而它恰恰是 `gear mcp --http` 的真实入口。
//  2. 各 handler 的**错误分支**——「id 不存在必须报错，绝不静默成功」是 Gear 的一贯
//     口径，错误分支不测等于没测。

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

// TestMCPErrorPathsReportInsteadOfSilentSuccess 「不存在」/「参数非法」类入参必须报错。
//
// 这一组是**自愈回路的地基**：智能体拿不到明确的失败信号，就会把「没报错」当成
// 「已经生效」，然后在下一步拿到一个看似无关的错，开始乱改 YAML。
func TestMCPErrorPathsReportInsteadOfSilentSuccess(t *testing.T) {
	session := newSmokeSession(t)

	cases := []struct {
		name, tool, wantCode string
		args                 map[string]any
	}{
		{"tool 不存在", "gear_describe", "NOT_FOUND", map[string]any{"kind": "tool", "id": "no_such_tool"}},
		{"flow 不存在", "gear_describe", "NOT_FOUND", map[string]any{"kind": "flow", "id": "no_such_flow"}},
		{"hook 不存在", "gear_describe", "NOT_FOUND", map[string]any{"kind": "hook", "id": "no_such_hook"}},
		{"describe 的 kind 非法", "gear_describe", "BAD_ARGUMENT", map[string]any{"kind": "bogus", "id": "x"}},
		{"describe 的空 id", "gear_describe", "BAD_ARGUMENT", map[string]any{"kind": "tool", "id": ""}},
		{"执行记录不存在", "gear_execution", "NOT_FOUND", map[string]any{"id": "no_such_exec"}},
		{"删不存在的 tool", "gear_delete", "NOT_FOUND", map[string]any{"kind": "tool", "id": "no_such_tool"}},
		{"删不存在的 flow", "gear_delete", "NOT_FOUND", map[string]any{"kind": "flow", "id": "no_such_flow"}},
		{"删不存在的 hook", "gear_delete", "NOT_FOUND", map[string]any{"kind": "hook", "id": "no_such_hook"}},
		{"delete 的 kind 非法", "gear_delete", "BAD_ARGUMENT", map[string]any{"kind": "bogus", "id": "x"}},
		{"delete 的空 id", "gear_delete", "BAD_ARGUMENT", map[string]any{"kind": "tool", "id": ""}},
		{"跑不存在的 flow", "gear_run", "NOT_FOUND", map[string]any{"kind": "flow", "id": "no_such_flow"}},
		{"跑不存在的 tool", "gear_run", "NOT_FOUND", map[string]any{"kind": "tool", "id": "no_such_tool"}},
		{"run 的 kind 非法", "gear_run", "BAD_ARGUMENT", map[string]any{"kind": "bogus", "id": "x"}},
		{"run 的空 id", "gear_run", "BAD_ARGUMENT", map[string]any{"kind": "flow", "id": ""}},
		{"promote 目标状态非法", "gear_promote", "BAD_ARGUMENT", map[string]any{"tool_id": "x", "version": "1.0.0", "target": "bogus"}},
		{"validate 空 yaml", "gear_validate", "BAD_ARGUMENT", map[string]any{"yaml": "   "}},
		{"apply 空 bundle", "gear_apply", "BAD_ARGUMENT", map[string]any{"bundle": ""}},
		{"guide 主题不存在", "gear_guide", "UNKNOWN_TOPIC", map[string]any{"topic": "no_such_topic"}},
		{"app 的 action 非法", "gear_app", "BAD_ARGUMENT", map[string]any{"action": "bogus"}},
		{"app start 缺 flow_id", "gear_app", "BAD_ARGUMENT", map[string]any{"action": "start"}},
		{"app status 缺 app_id", "gear_app", "BAD_ARGUMENT", map[string]any{"action": "status"}},
		{"app stop 缺 app_id", "gear_app", "BAD_ARGUMENT", map[string]any{"action": "stop"}},
		{"app_id 不在台账", "gear_app", "APP_NOT_FOUND", map[string]any{"action": "status", "app_id": "app_nope"}},
		// 2026-10-04 补：catalog 的 kind 非法此前**没有测试**锁，而它恰恰是最容易被
		// 写成「静默返回空列表」的地方——空数组会被当成「这个对象真的不存在」，
		// 于是调用方开始排查一个压根不存在的故障。合法值必须与入参描述同源。
		{"catalog 的 kind 非法", "gear_catalog", "BAD_ARGUMENT", map[string]any{"kind": "bogus"}},
		// 能力目录的 kind 传错**刻意不报错**（它返回空清单 + 可用能力名 hint）：
		// 过滤后为空不是错误——按 name 过滤本就可能什么都不匹配，报 BAD_ARGUMENT 会让
		// 「查无此能力」与「查错了」混为一谈。故此处**不加** capabilities 的错误用例。
		// ⚠ 与 gear_catalog 的容错口径**刻意不同**（catalog 对未知 kind 报 BAD_ARGUMENT），
		// 别为了「统一」把两边改成一个样——两者的空结果语义本就不同。
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			text := expectToolError(t, session, c.tool, c.args, "")
			if !strings.Contains(text, "["+c.wantCode+"]") {
				t.Fatalf("应带 [%s] 码，实际: %s", c.wantCode, text)
			}
		})
	}
}

// TestMCPSchemaRequiredArgsFailLoudly 缺失**必填**参数由 SDK 的 input schema 校验挡下，
// 报的是 JSON-RPC 层的错误文本（没有 [CODE] 前缀——它压根到不了我们的 handler）。
//
// 这不是缺口而是取舍：`id` 声明为必填后，客户端能就地发现漏参（省一次往返）；代价是这条
// 错误没有码。本用例把该行为钉住——若哪天 SDK 改成静默放行（那才是真缺口），会立刻红。
func TestMCPSchemaRequiredArgsFailLoudly(t *testing.T) {
	session := newSmokeSession(t)
	for _, c := range []struct {
		tool string
		args map[string]any
	}{
		{"gear_describe", map[string]any{"kind": "tool"}},
		{"gear_delete", map[string]any{"kind": "tool"}},
		{"gear_run", map[string]any{"kind": "flow"}},
	} {
		res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: c.tool, Arguments: c.args})
		if err != nil {
			t.Fatalf("%s: 协议层错误: %v", c.tool, err)
		}
		if text := contentText(res); !strings.Contains(text, "missing properties") {
			t.Fatalf("%s 缺必填参数时应由 input schema 校验拒绝，实际: %s", c.tool, text)
		}
	}
}
