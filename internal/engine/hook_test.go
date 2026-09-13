package engine

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"gear/internal/model"
)

// objSchema 是测试用的宽松 object schema。
func objSchema() map[string]interface{} { return map[string]interface{}{"type": "object"} }

// nameSchema 要求 name 为 string 的 object schema。
func nameSchema() map[string]interface{} {
	return map[string]interface{}{
		"type":       "object",
		"properties": map[string]interface{}{"name": map[string]interface{}{"type": "string"}},
		"required":   []interface{}{"name"},
	}
}

func putTool(t *testing.T, reg interface {
	UpsertTool(*model.Tool) error
}, tool *model.Tool) {
	t.Helper()
	if err := reg.UpsertTool(tool); err != nil {
		t.Fatalf("注册 Tool %s 失败: %v", tool.ID, err)
	}
}

func putHook(t *testing.T, reg interface {
	UpsertHook(*model.Hook) error
}, h *model.Hook) {
	t.Helper()
	if err := reg.UpsertHook(h); err != nil {
		t.Fatalf("注册 Hook %s 失败: %v", h.ID, err)
	}
}

func putFlow(t *testing.T, reg interface {
	UpsertFlow(*model.Flow) error
}, f *model.Flow) {
	t.Helper()
	if err := reg.UpsertFlow(f); err != nil {
		t.Fatalf("注册 Flow %s 失败: %v", f.ID, err)
	}
}

func luaTool(id, entry string, in, out map[string]interface{}) *model.Tool {
	return &model.Tool{
		ID: id, Name: id, Runtime: model.RuntimeLua, Entry: entry,
		InputSchema: in, OutputSchema: out,
		Versions: []model.ToolVersion{{Version: "1.0.0", State: model.StateStable}},
	}
}

func procTool(id, entry string, in, out map[string]interface{}) *model.Tool {
	return &model.Tool{
		ID: id, Name: id, Runtime: model.RuntimeProcess, Entry: entry,
		InputSchema: in, OutputSchema: out,
		Versions: []model.ToolVersion{{Version: "1.0.0", State: model.StateStable}},
	}
}

// 取某 step 的 output 字段（字符串化比较）。
func stepField(t *testing.T, res *RunResult, step, field string) string {
	t.Helper()
	o, ok := res.Steps[step]["output"].(map[string]interface{})
	if !ok {
		t.Fatalf("step %s 无 output: %v", step, res.Steps[step])
	}
	return fmt.Sprint(o[field])
}

// H-1：before_step Hook 改写 input，下游看到改后值。
func TestHookBeforeStepRewritesInput(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	putTool(t, reg, luaTool("echo", `return ctx.input`, nameSchema(), nameSchema()))
	putTool(t, reg, luaTool("mask", `return { name = "MASKED" }`, objSchema(), objSchema()))
	putHook(t, reg, &model.Hook{ID: "h1", Target: "echo", Stage: model.StageBeforeStep, Action: "mask"})
	putFlow(t, reg, &model.Flow{ID: "f", Steps: []model.Step{
		{ID: "s1", Call: "echo", Input: map[string]interface{}{"name": "raw"}},
	}})

	res, err := New(reg).RunFlowResult("f")
	if err != nil {
		t.Fatalf("RunFlow 失败: %v", err)
	}
	if got := stepField(t, res, "s1", "name"); got != "MASKED" {
		t.Fatalf("before_step 改写未生效，实际 %q", got)
	}
}

// H-2：Hook 改写后必须重校验 schema —— 塞非法值要被拦（§9.1 不可绕过）。
func TestHookBeforeStepRewriteRevalidated(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	putTool(t, reg, luaTool("echo", `return ctx.input`, nameSchema(), nameSchema()))
	// 改写后丢掉必填 name
	putTool(t, reg, luaTool("break", `return { other = 1 }`, objSchema(), objSchema()))
	putHook(t, reg, &model.Hook{ID: "h1", Target: "echo", Stage: model.StageBeforeStep, Action: "break"})
	putFlow(t, reg, &model.Flow{ID: "f", Steps: []model.Step{
		{ID: "s1", Call: "echo", Input: map[string]interface{}{"name": "raw"}},
	}})

	_, err := New(reg).RunFlowResult("f")
	if err == nil {
		t.Fatal("Hook 改写出非法 input 应被 schema 拦下")
	}
	if !strings.Contains(err.Error(), "改写后校验失败") {
		t.Fatalf("错误应指明「改写后校验失败」，实际: %v", err)
	}
}

// H-3：after_step Hook 改写 output，下游 step 看到改后值。
func TestHookAfterStepRewritesOutput(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	putTool(t, reg, luaTool("echo", `return ctx.input`, nameSchema(), nameSchema()))
	putTool(t, reg, luaTool("appendH", `return { name = ctx.input.name .. "-H" }`, objSchema(), objSchema()))
	putHook(t, reg, &model.Hook{ID: "hs", Target: "echo", Stage: model.StageAfterStep, Action: "appendH"})
	putFlow(t, reg, &model.Flow{ID: "f", Steps: []model.Step{
		{ID: "s1", Call: "echo", Input: map[string]interface{}{"name": "ab"}},
		{ID: "s2", Call: "echo", DependsOn: []string{"s1"}, Input: map[string]interface{}{"name": "${steps.s1.output.name}"}},
	}})

	res, err := New(reg).RunFlowResult("f")
	if err != nil {
		t.Fatalf("RunFlow 失败: %v", err)
	}
	if got := stepField(t, res, "s1", "name"); got != "ab-H" {
		t.Fatalf("s1 output 应被 after_step 改写为 ab-H，实际 %q", got)
	}
	// s2 读到 s1 的改后值 "ab-H"，再被 after_step 改写为 "ab-H-H"
	if got := stepField(t, res, "s2", "name"); got != "ab-H-H" {
		t.Fatalf("下游应看到改后值（累积），实际 %q", got)
	}
}

// H-4：critical Hook 失败 → fail-closed，终止 Flow。
func TestHookCriticalFailureAborts(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	putTool(t, reg, luaTool("echo", `return ctx.input`, nameSchema(), nameSchema()))
	putTool(t, reg, procTool("boom", "exit 1", objSchema(), objSchema()))
	putHook(t, reg, &model.Hook{ID: "hc", Target: "echo", Stage: model.StageBeforeStep, Action: "boom", Criticality: model.CritCritical})
	putFlow(t, reg, &model.Flow{ID: "f", Steps: []model.Step{
		{ID: "s1", Call: "echo", Input: map[string]interface{}{"name": "raw"}},
	}})

	_, err := New(reg).RunFlowResult("f")
	if err == nil {
		t.Fatal("critical Hook 失败应终止 Flow")
	}
	if !strings.Contains(err.Error(), "critical") {
		t.Fatalf("错误应标明 critical，实际: %v", err)
	}
}

// H-5：best-effort Hook 失败 → 仅告警，Flow 继续（payload 不变）。
func TestHookBestEffortFailureContinues(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	putTool(t, reg, luaTool("echo", `return ctx.input`, nameSchema(), nameSchema()))
	putTool(t, reg, procTool("boom", "exit 1", objSchema(), objSchema()))
	putHook(t, reg, &model.Hook{ID: "hb", Target: "echo", Stage: model.StageBeforeStep, Action: "boom", Criticality: model.CritBestEffort})
	putFlow(t, reg, &model.Flow{ID: "f", Steps: []model.Step{
		{ID: "s1", Call: "echo", Input: map[string]interface{}{"name": "raw"}},
	}})

	res, err := New(reg).RunFlowResult("f")
	if err != nil {
		t.Fatalf("best-effort Hook 失败不应终止 Flow: %v", err)
	}
	if got := stepField(t, res, "s1", "name"); got != "raw" {
		t.Fatalf("best-effort 失败不应改写 payload，实际 %q", got)
	}
}

// H-6：on_error Hook 触发但**不能恢复**主流程（Flow 仍失败）。
func TestHookOnErrorDoesNotRecover(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	putTool(t, reg, procTool("boom", "exit 7", objSchema(), objSchema()))
	// on_error 载体自身也失败（critical）→ 其错误会被附加，证明 on_error 确实跑了
	putTool(t, reg, procTool("onerr", "exit 3", objSchema(), objSchema()))
	putHook(t, reg, &model.Hook{ID: "ho", Target: "boom", Stage: model.StageOnError, Action: "onerr", Criticality: model.CritCritical})
	putFlow(t, reg, &model.Flow{ID: "f", Steps: []model.Step{
		{ID: "s1", Call: "boom", Input: map[string]interface{}{}},
	}})

	_, err := New(reg).RunFlowResult("f")
	if err == nil {
		t.Fatal("on_error Hook 不能恢复主流程，Flow 应失败")
	}
	if !strings.Contains(err.Error(), "执行失败") {
		t.Fatalf("主错误应为 step 执行失败，实际: %v", err)
	}
	if !strings.Contains(err.Error(), "ho") {
		t.Fatalf("错误应包含 on_error Hook 的失败信息（证明其被触发），实际: %v", err)
	}
}

// H-7：Target 过滤 —— 指定其他 tool id 的 Hook 不生效。
func TestHookTargetFilter(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	putTool(t, reg, luaTool("echo", `return ctx.input`, nameSchema(), nameSchema()))
	putTool(t, reg, luaTool("mask", `return { name = "MASKED" }`, objSchema(), objSchema()))
	putHook(t, reg, &model.Hook{ID: "h1", Target: "other_tool", Stage: model.StageBeforeStep, Action: "mask"})
	putFlow(t, reg, &model.Flow{ID: "f", Steps: []model.Step{
		{ID: "s1", Call: "echo", Input: map[string]interface{}{"name": "raw"}},
	}})

	res, err := New(reg).RunFlowResult("f")
	if err != nil {
		t.Fatalf("RunFlow 失败: %v", err)
	}
	if got := stepField(t, res, "s1", "name"); got != "raw" {
		t.Fatalf("Target 不匹配的 Hook 不应生效，实际 %q", got)
	}
}

// H-8：Order 决定装配顺序（升序执行，后者覆盖前者）。
func TestHookOrder(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	putTool(t, reg, luaTool("echo", `return ctx.input`, nameSchema(), nameSchema()))
	putTool(t, reg, luaTool("setA", `return { name = "A" }`, objSchema(), objSchema()))
	putTool(t, reg, luaTool("setB", `return { name = "B" }`, objSchema(), objSchema()))
	// order 10 先跑（B），order 20 后跑（A）→ 最终 A
	putHook(t, reg, &model.Hook{ID: "h20", Target: "echo", Stage: model.StageBeforeStep, Action: "setA", Order: 20})
	putHook(t, reg, &model.Hook{ID: "h10", Target: "echo", Stage: model.StageBeforeStep, Action: "setB", Order: 10})
	putFlow(t, reg, &model.Flow{ID: "f", Steps: []model.Step{
		{ID: "s1", Call: "echo", Input: map[string]interface{}{"name": "raw"}},
	}})

	res, err := New(reg).RunFlowResult("f")
	if err != nil {
		t.Fatalf("RunFlow 失败: %v", err)
	}
	if got := stepField(t, res, "s1", "name"); got != "A" {
		t.Fatalf("Order 升序装配应让 Order=20 最后生效（A），实际 %q", got)
	}
}

// H-9：无递归 —— Hook 自身执行不再触发 Hook（Target="*" 覆盖包括 Hook 载体自身）。
func TestHookNoRecursion(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	// mask 自身也在 Target="*" 的覆盖范围内；若 Hook 会递归，将无限递归/挂死。
	putTool(t, reg, luaTool("mask", `return { name = "M" }`, nameSchema(), nameSchema()))
	putHook(t, reg, &model.Hook{ID: "hall", Target: "*", Stage: model.StageBeforeStep, Action: "mask"})
	putFlow(t, reg, &model.Flow{ID: "f", Steps: []model.Step{
		{ID: "s1", Call: "mask", Input: map[string]interface{}{"name": "raw"}},
	}})

	done := make(chan struct{})
	var res *RunResult
	var err error
	go func() {
		res, err = New(reg).RunFlowResult("f")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Hook 疑似递归导致挂死")
	}
	if err != nil {
		t.Fatalf("RunFlow 失败: %v", err)
	}
	if got := stepField(t, res, "s1", "name"); got != "M" {
		t.Fatalf("输出应为 M，实际 %q", got)
	}
}

// H-10：service step 同样走 Hook 路径（before_step 生效，after_step 不适用）。
func TestHookAppliesToServiceStep(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	putTool(t, reg, procTool("sleepy", "sleep 3", objSchema(), objSchema()))
	putTool(t, reg, luaTool("tag", `return { tag = "hooked" }`, objSchema(), objSchema()))
	putHook(t, reg, &model.Hook{ID: "hs", Target: "sleepy", Stage: model.StageBeforeStep, Action: "tag"})
	putFlow(t, reg, &model.Flow{ID: "f", Steps: []model.Step{
		{ID: "keep", Call: "sleepy", Mode: "service", Input: map[string]interface{}{}},
	}})

	res, err := New(reg).Supervise("f")
	if err != nil {
		t.Fatalf("Supervise 失败: %v", err)
	}
	if len(res.Services) != 1 {
		t.Fatalf("应有 1 个 service，实际 %d", len(res.Services))
	}
	h := res.Services[0]
	defer func() {
		_ = h.Proc.Kill()
		_ = h.Proc.Wait()
	}()
	if got := fmt.Sprint(h.Input["tag"]); got != "hooked" {
		t.Fatalf("service 的 input 应被 before_step Hook 改写为 hooked，实际 %q", got)
	}
}
