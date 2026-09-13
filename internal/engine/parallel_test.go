package engine

import (
	"testing"
	"time"

	"gear/internal/model"
)

// 建一个「先 sleep 若干秒再回显 JSON」的 process Tool（用于测量并行/串行耗时）。
// 注意：process runtime 契约要求 stdout 末行为 JSON，故 sleep 后必须 echo。
func sleepTool(id, ms string) *model.Tool {
	return &model.Tool{
		ID: id, Name: id, Runtime: model.RuntimeProcess,
		Entry:       `sleep ` + ms + `; echo '{"ok":true}'`,
		InputSchema: objSchema(), OutputSchema: objSchema(),
		Versions: []model.ToolVersion{{Version: "1.0.0", State: model.StateStable}},
	}
}

// P-1：无依赖 step **并行**执行——与串行基线**相对比较**。
//
// 注意（方法论 §11·时序断言）：不要用绝对阈值（如「必须 < 900ms」）——`go test ./...`
// 会并行跑多个包，机器负载会让绝对时长抖动，测试变成偶发失败。
// 正确做法：在同一进程、同一负载条件下先量串行基线，再比并行，比较**比值**。
func TestParallelExecution(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	const dur = "0.4"
	for _, id := range []string{"a", "b", "c"} {
		putTool(t, reg, sleepTool(id, dur))
	}
	putFlow(t, reg, &model.Flow{ID: "f", Steps: []model.Step{
		{ID: "s1", Call: "a", Input: map[string]interface{}{}},
		{ID: "s2", Call: "b", Input: map[string]interface{}{}},
		{ID: "s3", Call: "c", Input: map[string]interface{}{}},
	}})

	// 串行基线（maxParallel=1）
	t0 := time.Now()
	if _, err := New(reg, WithMaxParallel(1)).RunFlowResult("f"); err != nil {
		t.Fatalf("串行基线执行失败: %v", err)
	}
	serial := time.Since(t0)

	// 并行（maxParallel=4）
	t1 := time.Now()
	res, err := New(reg, WithMaxParallel(4)).RunFlowResult("f")
	parallel := time.Since(t1)
	if err != nil {
		t.Fatalf("并行执行失败: %v", err)
	}
	if len(res.Steps) != 3 {
		t.Fatalf("应有 3 个 step 结果，实际 %d", len(res.Steps))
	}

	// 3 个 step 并行 ⇒ 理想约为串行的 1/3；取 0.75 作判别线，容忍负载抖动。
	if parallel*4 >= serial*3 {
		t.Fatalf("并行未生效：serial=%v parallel=%v（并行耗时应显著小于串行）", serial, parallel)
	}
}

// P-2：maxParallel=1 退化为串行（并发闸门生效）。
func TestMaxParallelOneSerializes(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	for _, id := range []string{"a", "b", "c"} {
		putTool(t, reg, sleepTool(id, "0.3"))
	}
	putFlow(t, reg, &model.Flow{ID: "f", Steps: []model.Step{
		{ID: "s1", Call: "a", Input: map[string]interface{}{}},
		{ID: "s2", Call: "b", Input: map[string]interface{}{}},
		{ID: "s3", Call: "c", Input: map[string]interface{}{}},
	}})

	start := time.Now()
	if _, err := New(reg, WithMaxParallel(1)).RunFlowResult("f"); err != nil {
		t.Fatalf("RunFlow 失败: %v", err)
	}
	elapsed := time.Since(start)
	// 串行：3×0.3s ≈ 0.9s；并行会 <0.5s。
	if elapsed < 800*time.Millisecond {
		t.Fatalf("maxParallel=1 应串行执行（≈0.9s），实际 %v", elapsed)
	}
}

// P-3：maxParallel 限制并发度（2 个并发槽跑 4 个 0.3s step ⇒ 至少两批）。
func TestMaxParallelTwoBatches(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	for _, id := range []string{"a", "b", "c", "d"} {
		putTool(t, reg, sleepTool(id, "0.3"))
	}
	putFlow(t, reg, &model.Flow{ID: "f", Steps: []model.Step{
		{ID: "s1", Call: "a", Input: map[string]interface{}{}},
		{ID: "s2", Call: "b", Input: map[string]interface{}{}},
		{ID: "s3", Call: "c", Input: map[string]interface{}{}},
		{ID: "s4", Call: "d", Input: map[string]interface{}{}},
	}})

	start := time.Now()
	if _, err := New(reg, WithMaxParallel(2)).RunFlowResult("f"); err != nil {
		t.Fatalf("RunFlow 失败: %v", err)
	}
	elapsed := time.Since(start)
	// 2 并发 ⇒ 2 批 ≈ 0.6s；全并发 ≈ 0.3s。取 0.5s 作判别线。
	if elapsed < 500*time.Millisecond {
		t.Fatalf("maxParallel=2 应分两批（≈0.6s），实际 %v", elapsed)
	}
}

// P-4：同层内某个 step 失败 → 返回错误，且**后续层不再执行**。
func TestParallelErrorStopsLaterLevels(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	putTool(t, reg, procTool("boom", "exit 5", objSchema(), objSchema()))
	putTool(t, reg, luaTool("ok", `return { v = 1 }`, objSchema(), objSchema()))
	// s1 失败（第 1 层），s2 依赖 s1（第 2 层）→ 不应被执行
	putFlow(t, reg, &model.Flow{ID: "f", Steps: []model.Step{
		{ID: "s1", Call: "boom", Input: map[string]interface{}{}},
		{ID: "s2", Call: "ok", DependsOn: []string{"s1"}, Input: map[string]interface{}{}},
	}})

	res, err := New(reg).RunFlowResult("f")
	if err == nil {
		t.Fatal("step 失败应返回错误")
	}
	if res != nil {
		if _, ran := res.Steps["s2"]; ran {
			t.Fatal("上游失败后，下游 step 不应被执行")
		}
	}
}

// P-5：并行 step 的 Hook 仍然逐 step 生效（并发下 Hook 语义不被破坏）。
func TestParallelHooksApplyPerStep(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	putTool(t, reg, luaTool("echo", `return ctx.input`, nameSchema(), nameSchema()))
	putTool(t, reg, luaTool("mask", `return { name = "MASKED" }`, objSchema(), objSchema()))
	putHook(t, reg, &model.Hook{ID: "h", Target: "*", Stage: model.StageBeforeStep, Action: "mask"})
	putFlow(t, reg, &model.Flow{ID: "f", Steps: []model.Step{
		{ID: "s1", Call: "echo", Input: map[string]interface{}{"name": "raw1"}},
		{ID: "s2", Call: "echo", Input: map[string]interface{}{"name": "raw2"}},
	}})

	res, err := New(reg, WithMaxParallel(4)).RunFlowResult("f")
	if err != nil {
		t.Fatalf("RunFlow 失败: %v", err)
	}
	for _, id := range []string{"s1", "s2"} {
		if got := stepField(t, res, id, "name"); got != "MASKED" {
			t.Fatalf("%s 的 Hook 未生效，实际 %q", id, got)
		}
	}
}

// P-6：并行下依赖推断仍保证时序——引用下游输出的 step 不会抢跑。
func TestParallelWithInferredDeps(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	putTool(t, reg, luaTool("echo", `return ctx.input`, nameSchema(), nameSchema()))
	putFlow(t, reg, &model.Flow{ID: "f", Steps: []model.Step{
		// s2 引用 s1 的输出，但**未**声明 depends_on；并行下靠推断保证先跑 s1
		{ID: "s2", Call: "echo", Input: map[string]interface{}{"name": "${steps.s1.output.name}-x"}},
		{ID: "s1", Call: "echo", Input: map[string]interface{}{"name": "base"}},
	}})

	res, err := New(reg, WithMaxParallel(4)).RunFlowResult("f")
	if err != nil {
		t.Fatalf("RunFlow 失败: %v", err)
	}
	if got := stepField(t, res, "s2", "name"); got != "base-x" {
		t.Fatalf("依赖推断应保证时序，实际 %q", got)
	}
}

// P-7：并发写 steps 的**并发安全**（-race 下运行；同时断言结果齐全）。
func TestParallelStepResultsComplete(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	for _, id := range []string{"a", "b", "c", "d"} {
		putTool(t, reg, luaTool(id, `return { from = "`+id+`" }`, objSchema(), objSchema()))
	}
	putFlow(t, reg, &model.Flow{ID: "f", Steps: []model.Step{
		{ID: "s1", Call: "a", Input: map[string]interface{}{}},
		{ID: "s2", Call: "b", Input: map[string]interface{}{}},
		{ID: "s3", Call: "c", Input: map[string]interface{}{}},
		{ID: "s4", Call: "d", Input: map[string]interface{}{}},
	}})

	res, err := New(reg, WithMaxParallel(4)).RunFlowResult("f")
	if err != nil {
		t.Fatalf("RunFlow 失败: %v", err)
	}
	want := map[string]string{"s1": "a", "s2": "b", "s3": "c", "s4": "d"}
	for step, from := range want {
		if got := stepField(t, res, step, "from"); got != from {
			t.Fatalf("%s 结果异常: %q（应 %q）", step, got, from)
		}
	}
}
