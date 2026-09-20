package engine

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gear/internal/cap"
	"gear/internal/model"
	lua "github.com/yuin/gopher-lua"
	"gopkg.in/yaml.v3"
)

// mockAI 是 in-process 的 ext.ai 桩：实现 complete/catalog/domains/validate 的稳定契约，
// 让「易变的 Lua agent」在无真实 LLM 下组合跑通——验证分层：Lua 编排 + Go 原语。
// 它刻意不在 agent 里写任何编排逻辑（编排在 Lua），只提供原语。
type mockAI struct {
	completeCalls      int
	validateCalls      int
	firstValidateFails bool
}

func (m *mockAI) complete(system, prompt, format string) map[string]interface{} {
	m.completeCalls++
	var data interface{}
	switch {
	case strings.Contains(system, "规划器"):
		data = []interface{}{
			map[string]interface{}{"subtask": "执行步骤", "domain": "general"},
		}
	case strings.Contains(system, "原子能力") || strings.Contains(system, "Tool"):
		data = map[string]interface{}{
			"id": "demo.generated", "name": "生成的工具", "description": "示例",
			"runtime": "lua", "entry": "--", "input_schema": map[string]interface{}{"type": "object"},
			"output_schema": map[string]interface{}{"type": "object"},
			"versions":      []interface{}{map[string]interface{}{"version": "1.0.0", "state": "draft"}},
		}
	default:
		data = map[string]interface{}{
			"id": "demo.flow", "name": "生成的流程", "description": "示例",
			"steps": []interface{}{
				map[string]interface{}{"id": "s1", "call": "demo.echo", "input": map[string]interface{}{}},
			},
		}
	}
	if format == "json" {
		return map[string]interface{}{"data": data}
	}
	b, _ := json.Marshal(data)
	return map[string]interface{}{"text": string(b)}
}

func (m *mockAI) buildLua(L *lua.LState, c *cap.Context) (lua.LValue, error) {
	ai := m
	tbl := L.NewTable()
	tbl.RawSetString("complete", L.NewFunction(func(L *lua.LState) int {
		args := map[string]interface{}{}
		if mt, ok := fromLuaValue(L.Get(1)).(map[string]interface{}); ok {
			args = mt
		}
		sys, _ := args["system"].(string)
		prompt, _ := args["prompt"].(string)
		format, _ := args["format"].(string)
		L.Push(toLuaValue(L, ai.complete(sys, prompt, format)))
		return 1
	}))
	tbl.RawSetString("catalog", L.NewFunction(func(L *lua.LState) int {
		args := map[string]interface{}{}
		if mt, ok := fromLuaValue(L.Get(1)).(map[string]interface{}); ok {
			args = mt
		}
		L.Push(toLuaValue(L, map[string]interface{}{"tools": args["tools"]}))
		return 1
	}))
	tbl.RawSetString("domains", L.NewFunction(func(L *lua.LState) int {
		L.Push(toLuaValue(L, map[string]interface{}{"domains": []interface{}{
			map[string]interface{}{"name": "general", "tool_ids": []interface{}{"demo.echo"}, "summary": "general"},
		}}))
		return 1
	}))
	tbl.RawSetString("validate", L.NewFunction(func(L *lua.LState) int {
		m.validateCalls++
		if m.firstValidateFails && m.validateCalls == 1 {
			L.Push(toLuaValue(L, map[string]interface{}{"ok": false, "error": "mock 首次校验失败，触发重试"}))
		} else {
			L.Push(toLuaValue(L, map[string]interface{}{"ok": true}))
		}
		return 1
	}))
	return tbl, nil
}

// loadAIAgentEntry 从仓库的 testdata/ai-create-flow.yaml 抽出 ai-gen 的 Lua entry（单一真相源）。
func loadAIAgentEntry(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "ai-create-flow.yaml"))
	if err != nil {
		t.Fatalf("读取参考 Flow 失败: %v", err)
	}
	for _, doc := range strings.Split(string(raw), "\n---") {
		if strings.Contains(doc, "id: ai-gen") {
			var tool model.Tool
			if err := yaml.Unmarshal([]byte(doc), &tool); err != nil {
				t.Fatalf("解析 ai-gen Tool 失败: %v", err)
			}
			return tool.Entry
		}
	}
	t.Fatal("未找到 ai-gen 文档")
	return ""
}

// runAIAgent 用 in-process mock ext.ai 跑真实 Lua agent（从 YAML 载入），注入 intent/tools/mode。
// 返回 agent 输出与 mock（用于断言重试次数）。
func runAIAgent(t *testing.T, entry, mode string) (map[string]interface{}, *mockAI) {
	t.Helper()
	ai := &mockAI{firstValidateFails: true}
	reg := NewCapabilityRegistry()
	if err := reg.Register("ext.ai", &CapabilityExtension{
		Provider: func(c *cap.Context) (interface{}, error) { return ai, nil },
		BuildLua: ai.buildLua,
	}); err != nil {
		t.Fatalf("注册 mock ext.ai 失败: %v", err)
	}

	engReg := newTestRegistry(t)
	defer engReg.Close()
	if err := engReg.UpsertTool(&model.Tool{
		ID: "demo.echo", Name: "回声", Description: "回声工具", Runtime: model.RuntimeLua,
		Entry:        "return ctx.input",
		InputSchema:  map[string]interface{}{"type": "object"},
		OutputSchema: map[string]interface{}{"type": "object"},
		Versions:     []model.ToolVersion{{Version: "1.0.0", State: model.StateStable}},
	}); err != nil {
		t.Fatalf("灌入 demo 工具失败: %v", err)
	}
	// ext.registry：用真实 RegistryExtension（关闭于 engReg），验证 agent 经
	// ctx.ext.registry.tools() 自我锚定，而非依赖调用方喂 tools。
	// 第二个参数是「已注册 ext.* 的方法级 schema」（生产由 SetupExtensions 汇总），
	// 本测试按真实注册的东西给（ext.ai 用默认 schema），使 capabilities() 输出可信。
	extCaps := map[string]map[string]MethodSchema{"ext.ai": defaultAISchemas()}
	if err := reg.Register("ext.registry", RegistryExtension(engReg, extCaps)); err != nil {
		t.Fatalf("注册 ext.registry 失败: %v", err)
	}

	tool := &model.Tool{
		ID:           "ai-gen",
		Name:         "ai-gen",
		Runtime:      model.RuntimeLua,
		Entry:        entry,
		Capabilities: []string{"ext.ai", "ext.registry"},
		InputSchema:  map[string]interface{}{"type": "object"},
		OutputSchema: map[string]interface{}{"type": "object"},
		Versions:     []model.ToolVersion{{Version: "1.0.0", State: model.StateStable}},
	}
	if err := engReg.UpsertTool(tool); err != nil {
		t.Fatalf("注册 ai-gen 失败: %v", err)
	}

	flow := &model.Flow{
		ID: "ai-create-flow",
		Vars: map[string]interface{}{
			"intent":      "把结果发到 Slack",
			"mode":        mode,
			"max_retries": 3,
		},
		Steps: []model.Step{{ID: "gen", Call: "ai-gen", Input: map[string]interface{}{
			"intent":      "${vars.intent}",
			"mode":        "${vars.mode}",
			"max_retries": "${vars.max_retries}",
		}}},
	}
	if err := engReg.UpsertFlow(flow); err != nil {
		t.Fatalf("注册 Flow 失败: %v", err)
	}

	eng := New(engReg, WithCapabilities(reg))
	res, err := eng.RunFlowResult("ai-create-flow")
	if err != nil {
		t.Fatalf("运行 Lua agent 失败: %v", err)
	}
	stepOut, ok := res.Steps["gen"]["output"].(map[string]interface{})
	if !ok {
		t.Fatalf("agent 输出异常: %v", res.Steps["gen"])
	}
	return stepOut, ai
}

// TestLuaAgentFlowCompilesPrimitives 真实 Lua agent（flow 模式）组合 ext.ai 原语跑通：
// ① 经 ctx.ext.registry.tools() 自我锚定（agent 不再依赖调用方喂 tools）② decompose→catalog→assemble→validate
// ③ 首次校验失败后触发 Lua 重试循环。验证「易变 Lua 编排 + 稳定 Go 原语」分层可行。
func TestLuaAgentFlowCompilesPrimitives(t *testing.T) {
	entry := loadAIAgentEntry(t)
	out, ai := runAIAgent(t, entry, "flow")
	if fmt.Sprint(out["kind"]) != "flow" {
		t.Fatalf("kind 应为 flow，实际 %v", out["kind"])
	}
	if out["flow"] == nil {
		t.Fatalf("flow 草稿缺失: %v", out)
	}
	// 1 拆解 + 至少 2 次组装（重试） ⇒ completeCalls >= 3；validateCalls >= 2（重试生效）。
	if ai.completeCalls < 3 {
		t.Fatalf("应至少重试一次（completeCalls>=3），实际 %d", ai.completeCalls)
	}
	if ai.validateCalls < 2 {
		t.Fatalf("应至少重试一次（validateCalls>=2），实际 %d", ai.validateCalls)
	}
}

// TestLuaAgentToolCompilesPrimitives 真实 Lua agent（tool 模式）组合原语跑通 + 重试。
func TestLuaAgentToolCompilesPrimitives(t *testing.T) {
	entry := loadAIAgentEntry(t)
	out, ai := runAIAgent(t, entry, "tool")
	if fmt.Sprint(out["kind"]) != "tool" {
		t.Fatalf("kind 应为 tool，实际 %v", out["kind"])
	}
	if out["tool"] == nil {
		t.Fatalf("tool 草稿缺失: %v", out)
	}
	if ai.completeCalls < 2 {
		t.Fatalf("tool 模式应至少重试一次（completeCalls>=2），实际 %d", ai.completeCalls)
	}
	if ai.validateCalls < 2 {
		t.Fatalf("tool 模式应至少重试一次（validateCalls>=2），实际 %d", ai.validateCalls)
	}
}
