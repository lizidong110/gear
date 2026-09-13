package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"

	"gear/internal/model"
)

// F2 规格一致性：完全照 PRD §4.1 文档格式（修正后：id/call/vars/${...}，无 when/parallel/version）
// 写的 Flow 必须能被加载并执行——把「文档说有」变成可机器验证的行为。
func TestSpecFlowFormatLoadsAndRuns(t *testing.T) {
	yamlStr := `
id: spec_demo
name: Spec Demo
vars:
  topic: world
steps:
  - id: s1
    call: hello
    input:
      name: "${vars.topic}"
  - id: s2
    call: hello
    depends_on: [s1]
    input:
      name: "${steps.s1.output.name}-reply"
`
	reg := newTestRegistry(t)
	defer reg.Close()
	if err := reg.UpsertTool(sampleTool()); err != nil {
		t.Fatalf("注册 Tool 失败: %v", err)
	}
	var f model.Flow
	if err := yaml.Unmarshal([]byte(yamlStr), &f); err != nil {
		t.Fatalf("解析 PRD 格式 Flow 失败: %v", err)
	}
	if err := reg.UpsertFlow(&f); err != nil {
		t.Fatalf("PRD 格式 Flow 应可注册: %v", err)
	}
	if err := New(reg).RunFlow("spec_demo"); err != nil {
		t.Fatalf("PRD 格式 Flow 应可运行: %v", err)
	}
}

// F2/F3：PRD 旧示例用 flow_id / tool: / when / version（M8/M9）。
// 修正后模型只认 id/call；use_version/when/parallel 在 S1 被 ValidateFlow 显式拒绝。
// 这条测试断言「旧字段被忽略 + 不支持字段被拒」，从而把文档漂移变成可见的失败。
func TestSpecRejectsOldFieldNames(t *testing.T) {
	yamlStr := `
flow_id: bad
version: "1.0"
steps:
  - id: s1
    tool: hello
    when: "${check.output.x != 1}"
    input: { name: x }
`
	var f model.Flow
	if err := yaml.Unmarshal([]byte(yamlStr), &f); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if f.ID != "" {
		t.Fatalf("flow_id 应被忽略（模型无此字段），实际 id=%q", f.ID)
	}
	if f.Steps[0].Call != "" {
		t.Fatalf("tool: 应被忽略（模型用 call），实际 call=%q", f.Steps[0].Call)
	}
	if err := f.Validate(); err == nil {
		t.Fatal("含 when / 空 call 的 Flow 应被 ValidateFlow 拒绝")
	}
}

// F2/M3：PRD §3.1 旧简写 input_schema: { "url": "string" } 不是合法 JSON Schema
// （缺 type:object）。Tool.Validate 应拒绝；修正后的 PRD §3.1 必须改用标准 JSON Schema。
func TestSpecShorthandSchemaRejected(t *testing.T) {
	tl := &model.Tool{
		ID: "bad", Name: "bad", Runtime: model.RuntimeProcess, Entry: "cat",
		InputSchema:  map[string]interface{}{"url": "string"},
		OutputSchema: map[string]interface{}{"type": "object"},
		Versions:     []model.ToolVersion{{Version: "1.0.0", State: model.StateStable}},
	}
	if err := tl.Validate(); err == nil {
		t.Fatal("PRD §3.1 简写 schema 应被拒绝（代码只认 type:object 的 JSON Schema）")
	}
}

// F2：examples/hello_flow.yaml（PRD §4.1 修正后示例）应当能端到端跑通。
func TestRunExampleHelloFlow(t *testing.T) {
	path := filepath.Join("..", "..", "examples", "hello_flow.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取示例失败: %v", err)
	}
	var f model.Flow
	if err := yaml.Unmarshal(raw, &f); err != nil {
		t.Fatalf("解析示例失败: %v", err)
	}
	reg := newTestRegistry(t)
	defer reg.Close()
	if err := reg.UpsertTool(sampleTool()); err != nil {
		t.Fatalf("注册 Tool 失败: %v", err)
	}
	if err := reg.UpsertFlow(&f); err != nil {
		t.Fatalf("注册示例 Flow 失败: %v", err)
	}
	if err := New(reg).RunFlow("demo"); err != nil {
		t.Fatalf("示例 Flow 运行失败: %v", err)
	}
}

// F3：output schema 校验失败必须终止 Flow（单一 Schema 真理源的另一半，S1 原漏测）。
func TestRunFlowRejectsInvalidOutput(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	// 要求 output 含 code，但 cat 只回显 input（无 code）→ output 校验应失败。
	tl := &model.Tool{
		ID: "needcode", Name: "needcode", Runtime: model.RuntimeProcess, Entry: "cat",
		InputSchema:  map[string]interface{}{"type": "object", "properties": map[string]interface{}{"name": map[string]interface{}{"type": "string"}}, "required": []interface{}{"name"}},
		OutputSchema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"code": map[string]interface{}{"type": "integer"}}, "required": []interface{}{"code"}},
		Versions:     []model.ToolVersion{{Version: "1.0.0", State: model.StateStable}},
	}
	if err := reg.UpsertTool(tl); err != nil {
		t.Fatalf("注册 Tool 失败: %v", err)
	}
	flow := &model.Flow{ID: "badout", Steps: []model.Step{{ID: "s1", Call: "needcode", Input: map[string]interface{}{"name": "x"}}}}
	if err := reg.UpsertFlow(flow); err != nil {
		t.Fatalf("注册 Flow 失败: %v", err)
	}
	if err := New(reg).RunFlow("badout"); err == nil {
		t.Fatal("output schema 校验失败应终止 Flow")
	}
}

// M10：未实现的 runtime 必须显式报错，不静默降级。
// S7a 起 lua 已实现（沙箱），不再属于此列；go-host / wasm 仍未实现。
func TestRuntimeForRejectsUnimplemented(t *testing.T) {
	// #76 起 go-host 已实现；仅 wasm 仍 deferred（V3）。
	for _, rt := range []model.Runtime{model.RuntimeWASM} {
		if _, err := runtimeFor(rt, ""); err == nil {
			t.Fatalf("runtime %q 应报错（未实现），实际未报错", rt)
		}
	}
	for _, rt := range []model.Runtime{model.RuntimeProcess, model.RuntimeLua, model.RuntimeGoHost} {
		if _, err := runtimeFor(rt, ""); err != nil {
			t.Fatalf("已实现 runtime %q 应可用: %v", rt, err)
		}
	}
	if _, err := runtimeFor(model.Runtime("foobar"), ""); err == nil {
		t.Fatal("未知 runtime 应报错")
	}
}

// G1 修复（F3）：未实现 / 未知 runtime 的 Tool 在**注册期**即被拒，不推迟到执行期。
// #76 起 go-host 可注册（已实现），故只断言 wasm 被拒。
func TestSpecRejectsUnimplementedRuntimeTool(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()

	mk := func(id string, rt model.Runtime) *model.Tool {
		return &model.Tool{ID: id, Runtime: rt, Entry: "x",
			InputSchema:  map[string]interface{}{"type": "object"},
			OutputSchema: map[string]interface{}{"type": "object"},
			Versions:     []model.ToolVersion{{Version: "1.0.0", State: model.StateStable}}}
	}
	for _, rt := range []model.Runtime{model.RuntimeWASM} {
		if err := reg.UpsertTool(mk("unimpl_"+string(rt), rt)); err == nil {
			t.Fatalf("未实现 runtime %q 的 Tool 注册应被拒绝", rt)
		}
	}
	if err := reg.UpsertTool(mk("bogus", model.Runtime("foobar"))); err == nil {
		t.Fatal("未知 runtime 的 Tool 注册应被拒绝")
	}
	for _, rt := range []model.Runtime{model.RuntimeProcess, model.RuntimeLua, model.RuntimeGoHost} {
		if err := reg.UpsertTool(mk("ok_"+string(rt), rt)); err != nil {
			t.Fatalf("已实现 runtime %q 的 Tool 应可注册: %v", rt, err)
		}
	}
}

// B 修复：runtimeFor(process) 返回的适配器同时实现 Run 与 Start（service 分发路径）。
func TestProcessRuntimeStart(t *testing.T) {
	rt, err := runtimeFor(model.RuntimeProcess, "")
	if err != nil {
		t.Fatalf("process runtime 应可用: %v", err)
	}
	p, err := rt.Start(context.Background(), "sleep 3", map[string]interface{}{}, nil)
	if err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	if p.Pid() <= 0 {
		t.Fatalf("pid 非法: %d", p.Pid())
	}
	if err := p.Kill(); err != nil {
		t.Fatalf("Kill 失败: %v", err)
	}
	_ = p.Wait()
}
