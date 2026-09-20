package engine

import (
	"testing"

	"gear/internal/model"
)

// E-tool-1：RunTool 单跑一个 Tool，input→output schema 双向校验（语义同「Flow 里的一步」）。
func TestRunTool(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	if err := reg.UpsertTool(sampleTool()); err != nil {
		t.Fatalf("注册 Tool 失败: %v", err)
	}
	out, err := New(reg).RunTool("hello", map[string]interface{}{"name": "bob"})
	if err != nil {
		t.Fatalf("RunTool 失败: %v", err)
	}
	if out["name"] != "bob" {
		t.Fatalf("回显异常: %v", out)
	}
}

// E-tool-2：RunTool 的 input 缺必填字段应被 input_schema 拦截（fail-closed，不静默放行）。
func TestRunToolRejectsInvalidInput(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	if err := reg.UpsertTool(sampleTool()); err != nil {
		t.Fatalf("注册 Tool 失败: %v", err)
	}
	if _, err := New(reg).RunTool("hello", map[string]interface{}{}); err == nil {
		t.Fatal("缺 name 应被 input_schema 拦截")
	}
}

// E-tool-3：RunTool 对不存在的 Tool 明确报错。
func TestRunToolUnknown(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	if _, err := New(reg).RunTool("nope", nil); err == nil {
		t.Fatal("Tool 不存在应报错")
	}
}

// E-var-1：RunFlowWithVars 用覆盖值跑（覆盖 Flow 声明的 vars），且**不写回 registry**。
func TestRunFlowWithVars(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	if err := reg.UpsertTool(sampleTool()); err != nil {
		t.Fatalf("注册 Tool 失败: %v", err)
	}
	flow := &model.Flow{
		ID:    "demo",
		Vars:  map[string]interface{}{"topic": "world"},
		Steps: []model.Step{{ID: "s1", Call: "hello", Input: map[string]interface{}{"name": "${vars.topic}"}}},
	}
	if err := reg.UpsertFlow(flow); err != nil {
		t.Fatalf("注册 Flow 失败: %v", err)
	}
	eng := New(reg)

	// 不覆盖 → 用 Flow 声明值
	res, err := eng.RunFlowResult("demo")
	if err != nil {
		t.Fatalf("RunFlowResult 失败: %v", err)
	}
	if got := stepName(t, res, "s1"); got != "world" {
		t.Fatalf("未覆盖时应为 world，实际 %v", got)
	}

	// 覆盖 → 本次运行生效
	res, err = eng.RunFlowWithVars("demo", map[string]interface{}{"topic": "override"})
	if err != nil {
		t.Fatalf("RunFlowWithVars 失败: %v", err)
	}
	if got := stepName(t, res, "s1"); got != "override" {
		t.Fatalf("覆盖后应为 override，实际 %v", got)
	}

	// 不写回：重新取 Flow，声明值仍是 world
	after, err := reg.GetFlow("demo")
	if err != nil || after == nil {
		t.Fatalf("GetFlow 失败: %v", err)
	}
	if after.Vars["topic"] != "world" {
		t.Fatalf("覆盖不应写回 registry，实际 %v", after.Vars["topic"])
	}
}

// stepName 取某 step 输出的 name 字段（sampleTool 是 cat，回显 input）。
func stepName(t *testing.T, res *RunResult, stepID string) interface{} {
	t.Helper()
	s, ok := res.Steps[stepID]
	if !ok {
		t.Fatalf("结果里没有 step %s", stepID)
	}
	out, ok := s["output"].(map[string]interface{})
	if !ok {
		t.Fatalf("step %s 的 output 不是对象: %v", stepID, s["output"])
	}
	return out["name"]
}
