package engine

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"gear/internal/model"
	"gear/internal/registry"
)

func newTestRegistry(t *testing.T) *registry.Registry {
	t.Helper()
	reg, err := registry.Open(filepath.Join(t.TempDir(), "registry.db"))
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	return reg
}

func sampleTool() *model.Tool {
	return &model.Tool{
		ID: "hello", Name: "hello", Runtime: model.RuntimeProcess, Entry: "cat",
		InputSchema:  map[string]interface{}{"type": "object", "properties": map[string]interface{}{"name": map[string]interface{}{"type": "string"}}, "required": []interface{}{"name"}},
		OutputSchema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"name": map[string]interface{}{"type": "string"}}},
		Versions:     []model.ToolVersion{{Version: "1.0.0", State: model.StateStable}},
	}
}

// E-topo-1：线性步骤拓扑序保持原序。
func TestTopoOrderLinear(t *testing.T) {
	steps := []model.Step{{ID: "a"}, {ID: "b"}, {ID: "c"}}
	order, err := topoOrder(steps)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if order[0] != 0 || order[1] != 1 || order[2] != 2 {
		t.Fatalf("线性序应为 0,1,2，实际 %v", order)
	}
}

// E-topo-2：依赖关系得到尊重。
func TestTopoOrderDependency(t *testing.T) {
	steps := []model.Step{{ID: "a"}, {ID: "b", DependsOn: []string{"a"}}, {ID: "c", DependsOn: []string{"b"}}}
	order, err := topoOrder(steps)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if !(order[0] < order[1] && order[1] < order[2]) {
		t.Fatalf("依赖序被破坏: %v", order)
	}
}

// E-topo-3：循环依赖被检出。
func TestTopoOrderCycle(t *testing.T) {
	steps := []model.Step{{ID: "a", DependsOn: []string{"b"}}, {ID: "b", DependsOn: []string{"a"}}}
	if _, err := topoOrder(steps); err == nil {
		t.Fatal("循环依赖应报错")
	}
}

// E-topo-4：依赖不存在的 step 报错。
func TestTopoOrderMissingDep(t *testing.T) {
	steps := []model.Step{{ID: "a", DependsOn: []string{"ghost"}}}
	if _, err := topoOrder(steps); err == nil {
		t.Fatal("缺失依赖应报错")
	}
}

// E-tpl-1：${vars.x} 与 ${steps.id.output.field} 解析。
func TestExpandString(t *testing.T) {
	ctx := &runContext{
		vars:  map[string]interface{}{"topic": "world"},
		steps: map[string]map[string]interface{}{"s1": map[string]interface{}{"output": map[string]interface{}{"name": "world"}}},
	}
	if got, err := expandString("${vars.topic}", ctx); err != nil || got != "world" {
		t.Fatalf("vars 解析失败: got=%q err=%v", got, err)
	}
	if got, err := expandString("${steps.s1.output.name}-reply", ctx); err != nil || got != "world-reply" {
		t.Fatalf("steps 解析失败: got=%q err=%v", got, err)
	}
}

// E-tpl-2：未闭合模板报错。
func TestExpandStringUnclosed(t *testing.T) {
	ctx := &runContext{vars: map[string]interface{}{}}
	if _, err := expandString("${vars.topic", ctx); err == nil {
		t.Fatal("未闭合应报错")
	}
}

// E-tpl-3：未知前缀报错。
func TestExpandStringUnknownPrefix(t *testing.T) {
	ctx := &runContext{vars: map[string]interface{}{}}
	if _, err := expandString("${foo.bar}", ctx); err == nil {
		t.Fatal("未知前缀应报错")
	}
}

// E-val-1：合法 instance 通过校验。
func TestValidatePass(t *testing.T) {
	schema := map[string]interface{}{"type": "object", "properties": map[string]interface{}{"name": map[string]interface{}{"type": "string"}}, "required": []interface{}{"name"}}
	if err := validate(schema, map[string]interface{}{"name": "x"}); err != nil {
		t.Fatalf("合法应过: %v", err)
	}
}

// E-val-2：缺必填字段被拒。
func TestValidateFail(t *testing.T) {
	schema := map[string]interface{}{"type": "object", "properties": map[string]interface{}{"name": map[string]interface{}{"type": "string"}}, "required": []interface{}{"name"}}
	if err := validate(schema, map[string]interface{}{}); err == nil {
		t.Fatal("缺字段应拒")
	}
}

// E-val-3：空/nil schema 跳过校验。
func TestValidateEmptySchema(t *testing.T) {
	if err := validate(map[string]interface{}{}, nil); err != nil {
		t.Fatalf("空 schema 应跳过: %v", err)
	}
	if err := validate(nil, nil); err != nil {
		t.Fatalf("nil schema 应跳过: %v", err)
	}
}

// E-tpl-4：引用尚未执行的 step 报错。
func TestLookupStepsNotExecuted(t *testing.T) {
	ctx := &runContext{vars: map[string]interface{}{}, steps: map[string]map[string]interface{}{}}
	if _, err := expandString("${steps.ghost.output.x}", ctx); err == nil {
		t.Fatal("未执行 step 应报错")
	}
}

// E-tpl-5：引用不存在的 vars key 报错。
func TestLookupVarsMissingKey(t *testing.T) {
	ctx := &runContext{vars: map[string]interface{}{}, steps: map[string]map[string]interface{}{}}
	if _, err := expandString("${vars.nope}", ctx); err == nil {
		t.Fatal("缺失 vars key 应报错")
	}
}

// E-tpl-6：map / slice 内嵌模板递归解析。
func TestResolveValueComposite(t *testing.T) {
	ctx := &runContext{vars: map[string]interface{}{"k": "v"}, steps: map[string]map[string]interface{}{}}
	in := map[string]interface{}{
		"a": "${vars.k}",
		"b": map[string]interface{}{"c": "${vars.k}"},
		"d": []interface{}{"${vars.k}", "lit"},
	}
	out, err := resolveValue(in, ctx)
	if err != nil {
		t.Fatalf("resolveValue 失败: %v", err)
	}
	m := out.(map[string]interface{})
	if fmt.Sprint(m["a"]) != "v" {
		t.Fatalf("a 未解析: %v", m["a"])
	}
	if fmt.Sprint(m["b"].(map[string]interface{})["c"]) != "v" {
		t.Fatalf("b.c 未解析")
	}
	ds := m["d"].([]interface{})
	if fmt.Sprint(ds[0]) != "v" || fmt.Sprint(ds[1]) != "lit" {
		t.Fatalf("d 未解析: %v", ds)
	}
}

// E-proc-1：process runtime 经 cat 回显 JSON。
func TestRunProcessEcho(t *testing.T) {
	out, err := runProcess(context.Background(), "cat", map[string]interface{}{"name": "x"}, nil, "", "", nil)
	if err != nil {
		t.Fatalf("runProcess 失败: %v", err)
	}
	if fmt.Sprint(out["name"]) != "x" {
		t.Fatalf("回显异常: %v", out)
	}
}

// E-run-1：端到端跑通 demo Flow（vars + 跨 step 引用 + schema 双向校验）。
func TestRunFlowEndToEnd(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	if err := reg.UpsertTool(sampleTool()); err != nil {
		t.Fatalf("注册 Tool 失败: %v", err)
	}
	flow := &model.Flow{
		ID:   "demo",
		Vars: map[string]interface{}{"topic": "world"},
		Steps: []model.Step{
			{ID: "s1", Call: "hello", Input: map[string]interface{}{"name": "${vars.topic}"}},
			{ID: "s2", Call: "hello", DependsOn: []string{"s1"}, Input: map[string]interface{}{"name": "${steps.s1.output.name}-reply"}},
		},
	}
	if err := reg.UpsertFlow(flow); err != nil {
		t.Fatalf("注册 Flow 失败: %v", err)
	}
	if err := New(reg).RunFlow("demo"); err != nil {
		t.Fatalf("RunFlow 失败: %v", err)
	}
}

// E-run-2：缺少必填 input 被 schema 拦截，Flow 中止。
func TestRunFlowRejectsInvalidInput(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	if err := reg.UpsertTool(sampleTool()); err != nil {
		t.Fatalf("注册 Tool 失败: %v", err)
	}
	flow := &model.Flow{
		ID:    "bad",
		Steps: []model.Step{{ID: "s1", Call: "hello", Input: map[string]interface{}{}}},
	}
	if err := reg.UpsertFlow(flow); err != nil {
		t.Fatalf("注册 Flow 失败: %v", err)
	}
	if err := New(reg).RunFlow("bad"); err == nil {
		t.Fatal("非法 input 应被拦截")
	}
}
