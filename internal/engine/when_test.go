package engine

import (
	"strings"
	"testing"

	"gear/internal/model"
)

// 返回 {n: 5} 的 lua Tool（n 为数字，用于验证类型保持）。
func numTool() *model.Tool {
	return luaTool("five", `return { n = 5 }`, objSchema(), map[string]interface{}{
		"type":       "object",
		"properties": map[string]interface{}{"n": map[string]interface{}{"type": "number"}},
	})
}

// 返回 {s: "200"} 的 lua Tool（s 为**字符串**，用于验证不会与数字 200 混淆）。
func strTool() *model.Tool {
	return luaTool("str", `return { s = "200" }`, objSchema(), map[string]interface{}{
		"type":       "object",
		"properties": map[string]interface{}{"s": map[string]interface{}{"type": "string"}},
	})
}

// W-1：when 为假 → step 被跳过（**不是失败**），Flow 仍成功。
func TestWhenFalseSkipsStep(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	putTool(t, reg, luaTool("echo", `return ctx.input`, nameSchema(), nameSchema()))
	putFlow(t, reg, &model.Flow{ID: "f", Steps: []model.Step{
		{ID: "s1", Call: "echo", When: "false", Input: map[string]interface{}{"name": "x"}},
	}})

	res, err := New(reg).RunFlowResult("f")
	if err != nil {
		t.Fatalf("跳过不应视为失败: %v", err)
	}
	if _, ran := res.Steps["s1"]; ran {
		t.Fatal("when=false 的 step 不应出现在已执行结果里")
	}
	if got := res.Skipped["s1"]; got != "when=false" {
		t.Fatalf("应在 Skipped 中记录，实际 %q", got)
	}
}

// W-2：when 为真 → 正常执行。
func TestWhenTrueRuns(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	putTool(t, reg, luaTool("echo", `return ctx.input`, nameSchema(), nameSchema()))
	putFlow(t, reg, &model.Flow{ID: "f", Steps: []model.Step{
		{ID: "s1", Call: "echo", When: "true", Input: map[string]interface{}{"name": "x"}},
	}})

	res, err := New(reg).RunFlowResult("f")
	if err != nil {
		t.Fatalf("RunFlow 失败: %v", err)
	}
	if got := stepField(t, res, "s1", "name"); got != "x" {
		t.Fatalf("when=true 应执行，实际 %v", res.Steps["s1"])
	}
	if len(res.Skipped) != 0 {
		t.Fatalf("不应有跳过项: %v", res.Skipped)
	}
}

// W-3：when 基于上游输出求值（真/假分支各一）。
func TestWhenUsesStepOutput(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	putTool(t, reg, numTool())
	putTool(t, reg, luaTool("echo", `return ctx.input`, nameSchema(), nameSchema()))
	putFlow(t, reg, &model.Flow{ID: "f", Steps: []model.Step{
		{ID: "s1", Call: "five", Input: map[string]interface{}{}},
		{ID: "yes", Call: "echo", When: "steps.s1.output.n > 3", Input: map[string]interface{}{"name": "yes"}},
		{ID: "no", Call: "echo", When: "steps.s1.output.n > 10", Input: map[string]interface{}{"name": "no"}},
	}})

	res, err := New(reg).RunFlowResult("f")
	if err != nil {
		t.Fatalf("RunFlow 失败: %v", err)
	}
	if _, ok := res.Steps["yes"]; !ok {
		t.Fatal("n=5 > 3 的分支应执行")
	}
	if _, ok := res.Skipped["no"]; !ok {
		t.Fatal("n=5 > 10 的分支应被跳过")
	}
}

// W-4：**类型保持**——字符串 "200" 不等于数字 200（证明用的是表达式而非字符串插值）。
func TestWhenKeepsTypes(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	putTool(t, reg, strTool())
	putTool(t, reg, luaTool("echo", `return ctx.input`, nameSchema(), nameSchema()))
	// wrong：拿字符串 "200" 和数字 200 比 ⇒ 应为假（若走插值就会变成 "200"=="200" 而误判为真）
	putFlow(t, reg, &model.Flow{ID: "f", Steps: []model.Step{
		{ID: "s1", Call: "str", Input: map[string]interface{}{}},
		{ID: "wrong", Call: "echo", When: `steps.s1.output.s == 200`, Input: map[string]interface{}{"name": "w"}},
		{ID: "right", Call: "echo", When: `steps.s1.output.s == "200"`, Input: map[string]interface{}{"name": "r"}},
	}})

	res, err := New(reg).RunFlowResult("f")
	if err != nil {
		t.Fatalf("RunFlow 失败: %v", err)
	}
	if _, ok := res.Skipped["wrong"]; !ok {
		t.Fatal(`字符串 "200" 与数字 200 不应相等（类型必须保持）`)
	}
	if _, ok := res.Steps["right"]; !ok {
		t.Fatal(`字符串比较应成立`)
	}
}

// W-5：跳过沿**显式依赖**传播（上游跳过 ⇒ 下游跳过，而非报错）。
func TestWhenCascadeSkipExplicitDep(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	putTool(t, reg, luaTool("echo", `return ctx.input`, nameSchema(), nameSchema()))
	putFlow(t, reg, &model.Flow{ID: "f", Steps: []model.Step{
		{ID: "s1", Call: "echo", When: "false", Input: map[string]interface{}{"name": "x"}},
		{ID: "s2", Call: "echo", DependsOn: []string{"s1"}, Input: map[string]interface{}{"name": "y"}},
	}})

	res, err := New(reg).RunFlowResult("f")
	if err != nil {
		t.Fatalf("级联跳过不应视为失败: %v", err)
	}
	if _, ok := res.Skipped["s2"]; !ok {
		t.Fatalf("下游 s2 应被级联跳过，实际 Skipped=%v", res.Skipped)
	}
	if !strings.Contains(res.Skipped["s2"], "上游 step s1") {
		t.Fatalf("跳过原因应说明来自上游，实际 %q", res.Skipped["s2"])
	}
}

// W-6：跳过沿**推断依赖**传播（引用被跳过的上游 ⇒ 也跳过，不报「尚未执行」）。
func TestWhenCascadeSkipInferredDep(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	putTool(t, reg, luaTool("echo", `return ctx.input`, nameSchema(), nameSchema()))
	putFlow(t, reg, &model.Flow{ID: "f", Steps: []model.Step{
		{ID: "s1", Call: "echo", When: "false", Input: map[string]interface{}{"name": "x"}},
		// 未写 depends_on，但引用了 s1 的输出 ⇒ 推断依赖 ⇒ 应级联跳过
		{ID: "s2", Call: "echo", Input: map[string]interface{}{"name": "${steps.s1.output.name}-y"}},
	}})

	res, err := New(reg).RunFlowResult("f")
	if err != nil {
		t.Fatalf("引用被跳过的上游不应报错，而应级联跳过: %v", err)
	}
	if _, ok := res.Skipped["s2"]; !ok {
		t.Fatalf("s2 应被级联跳过，实际 Skipped=%v", res.Steps)
	}
}

// W-7：when 表达式语法错误 → 明确报错（且注册期 CompileWhen 也能拦）。
func TestWhenCompileError(t *testing.T) {
	if err := CompileWhen("1 +"); err == nil {
		t.Fatal("语法错误的 when 应在编译期被拦下")
	}
	reg := newTestRegistry(t)
	defer reg.Close()
	putTool(t, reg, luaTool("echo", `return ctx.input`, nameSchema(), nameSchema()))
	putFlow(t, reg, &model.Flow{ID: "f", Steps: []model.Step{
		{ID: "s1", Call: "echo", When: "1 +", Input: map[string]interface{}{"name": "x"}},
	}})
	if _, err := New(reg).RunFlowResult("f"); err == nil {
		t.Fatal("when 语法错误应导致执行失败")
	}
}

// W-8：when 返回非布尔 → 报错（不允许隐式真值）。
func TestWhenNonBoolRejected(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	putTool(t, reg, luaTool("echo", `return ctx.input`, nameSchema(), nameSchema()))
	putFlow(t, reg, &model.Flow{ID: "f", Steps: []model.Step{
		{ID: "s1", Call: "echo", When: "1 + 1", Input: map[string]interface{}{"name": "x"}},
	}})
	_, err := New(reg).RunFlowResult("f")
	if err == nil {
		t.Fatal("when 返回非布尔值应报错")
	}
	if !strings.Contains(err.Error(), "布尔") {
		t.Fatalf("错误应说明需要布尔值，实际: %v", err)
	}
}

// W-9：when 里误用 ${} 模板语法 → 拒绝（表达式与模板必须分开）。
func TestWhenRejectsTemplateSyntax(t *testing.T) {
	if err := CompileWhen("${steps.s1.output.n} > 1"); err == nil {
		t.Fatal("when 不应接受 ${} 模板语法")
	}
	if err := CompileWhen(""); err != nil {
		t.Fatalf("空 when 应合法（表示无条件执行）: %v", err)
	}
	if err := CompileWhen("steps.s1.output.n > 1"); err != nil {
		t.Fatalf("合法表达式应通过: %v", err)
	}
}

// W-10：service step 的 when 为假 → 不拉起进程。
func TestWhenSkipsServiceStep(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	putTool(t, reg, procTool("sleepy", "sleep 30", objSchema(), objSchema()))
	putFlow(t, reg, &model.Flow{ID: "f", Steps: []model.Step{
		{ID: "keep", Call: "sleepy", Mode: "service", When: "false", Input: map[string]interface{}{}},
	}})

	res, err := New(reg).Supervise("f")
	if err != nil {
		t.Fatalf("跳过 service 不应视为失败: %v", err)
	}
	if len(res.Services) != 0 {
		t.Fatalf("when=false 的 service 不应被拉起，实际 %d 个", len(res.Services))
	}
}
