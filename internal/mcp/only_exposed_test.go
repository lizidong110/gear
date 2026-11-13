package mcp

// only_exposed_test.go：`--only-exposed` 应用专用面的回归（REQ-MCP-ONLY-EXPOSED）。
//
// 修的是一类**默认暴露面错误**：只想给终端客户端 5 个业务工具，结果 15 个常驻 gear_*
// （含 apply / delete / run / invoke 这一整套 registry 写面）一起挂了上去。
// 三条判据：① 只有白名单工具；② instructions 必须换成应用专用版（否则教 AI 调不存在的工具）；
// ③ 默认模式一个字节都不能变（零破坏）。

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"gear/internal/engine"
	"gear/internal/mcp/guide"
	"gear/internal/registry"
)

// TestMCPOnlyExposedHidesCoreSurface 应用专用面只挂白名单工具，且不下发标准面心智模型。
func TestMCPOnlyExposedHidesCoreSurface(t *testing.T) {
	f := newExposeFixture(t, curatedRules(),
		WithExpose(ExposeTools), WithScope("study"), WithOnlyExposed(true))

	ctx := context.Background()
	tools, err := f.session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools.Tools) != 2 {
		names := make([]string, 0, len(tools.Tools))
		for _, tt := range tools.Tools {
			names = append(names, tt.Name)
		}
		t.Fatalf("应用专用面应恰好只有 2 个策展工具，实际 %d: %v", len(tools.Tools), names)
	}
	for _, tt := range tools.Tools {
		if strings.HasPrefix(tt.Name, "gear_") || strings.HasPrefix(tt.Name, "flow__") {
			t.Fatalf("应用专用面不该出现 %s（常驻面/快照被带进来了）", tt.Name)
		}
	}

	// 协议层真的下发了应用专用 instructions：标准面那份通篇教 gear_validate / gear_apply，
	// 在这个端点上全是「不存在的工具」，照旧下发就是最贵的误导。
	ir := f.session.InitializeResult()
	if ir == nil {
		t.Fatal("客户端应拿到 InitializeResult")
	}
	if !strings.Contains(ir.Instructions, "没有 `gear_*` 管理工具") {
		t.Fatalf("应下发 guide/app-only.md，实际: %q", ir.Instructions)
	}
	if strings.Contains(ir.Instructions, "八条铁律") {
		t.Fatal("不应再下发标准面的 gear 心智模型")
	}

	// 手册资源也不该挂（它们是「怎么用内核写面」的说明书）。
	res, err := f.session.ListResources(ctx, nil)
	if err != nil {
		t.Fatalf("ListResources: %v", err)
	}
	if len(res.Resources) != 0 {
		t.Fatalf("应用专用面不应暴露 gear://guide/* 资源: %v", res.Resources)
	}

	// 工具本身仍可调用：关掉的是常驻面，不是执行路径。
	var out map[string]any
	callTool(t, f.session, "study_plan_propose", map[string]any{"name": "东东"}, &out)
	if out["kind"] != "tool" || out["id"] != "study__plan_propose" {
		t.Fatalf("策展工具仍应能执行: %v", out)
	}
}

// TestMCPOnlyExposedInstructionsAreNotATopic 两份 instructions 都不许混进 topics，
// 否则标准面的 AI 会读到「本端点没有 gear_*」这句，去理解一个明明有 15 个的端点。
func TestMCPOnlyExposedInstructionsAreNotATopic(t *testing.T) {
	if guide.AppOnly() == "" {
		t.Fatal("guide/app-only.md 必须嵌入")
	}
	for _, topic := range guide.Topics() {
		if topic == guide.SummaryTopic || topic == guide.AppOnlyTopic {
			t.Fatalf("instructions 文档 %q 不该出现在 guide topics 里", topic)
		}
	}
}

// TestMCPOnlyExposedRequiresExpose 没有 --expose 时直接拒绝装配：
// 关掉常驻面又没有任何反向暴露 = 零工具空端点，比多暴露更难排查。
func TestMCPOnlyExposedRequiresExpose(t *testing.T) {
	err := newServerError(t, WithOnlyExposed(true))
	if err == nil || !strings.Contains(err.Error(), "--expose") {
		t.Fatalf("--only-exposed 无 --expose 应 fail-closed，实际: %v", err)
	}
}

// TestMCPDefaultKeepsCoreSurface 默认行为零破坏：15 个常驻 gear_* 仍在，
// instructions 仍是标准面心智模型。
func TestMCPDefaultKeepsCoreSurface(t *testing.T) {
	f := newSmokeFixture(t)
	tools, err := f.session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	core := 0
	for _, tt := range tools.Tools {
		if strings.HasPrefix(tt.Name, "gear_") {
			core++
		}
	}
	if core != len(mcpToolRegistry) {
		t.Fatalf("默认面应保留 %d 个常驻 gear_* 工具，实际 %d", len(mcpToolRegistry), core)
	}
	ir := f.session.InitializeResult()
	if ir == nil || !strings.Contains(ir.Instructions, "八条铁律") {
		t.Fatalf("默认面应下发标准心智模型: %v", ir)
	}
}

// newServerError 只做装配、返回 New 的 error（装配期错误若走 transport 会被 `_ =` 吞掉）。
func newServerError(t *testing.T, opts ...Option) error {
	t.Helper()
	t.Setenv("GEAR_EXEC_DB", filepath.Join(t.TempDir(), "exec.db"))
	reg, err := registry.Open(filepath.Join(t.TempDir(), "registry.db"))
	if err != nil {
		t.Fatalf("打开 registry 失败: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	capReg := engine.NewCapabilityRegistry()
	agent, err := engine.SetupExtensions(capReg, reg, "")
	if err != nil {
		t.Fatalf("初始化扩展失败: %v", err)
	}
	eng := engine.New(reg, engine.WithCapabilities(capReg), engine.WithAIAgent(agent))
	s, err := New(reg, eng, opts...)
	if s != nil {
		t.Cleanup(func() { _ = s.Close() })
	}
	return err
}
