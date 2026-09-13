package engine

import (
	"sort"
	"testing"

	"gear/internal/model"
)

// T-plan-1：plannedToolIDs = step 的 call ∪ **生效** Hook 的 action。
//
// 关键点：Hook 的选择规则必须与 applyHooks 完全一致（共用 hooksFor），
// 所以 target 不命中的 Hook 的 action 不得出现在集合里 —— 否则注销守卫会误拦，
// 而「误拦」和「漏拦」一样是 bug（只是方向相反）。
func TestPlannedToolIDsIncludesOnlyEffectiveHookActions(t *testing.T) {
	flow := &model.Flow{ID: "f", Steps: []model.Step{
		{ID: "s1", Call: "a", Input: map[string]interface{}{}},
		{ID: "s2", Call: "b", Input: map[string]interface{}{}},
	}}
	hooks := []model.Hook{
		{ID: "h1", Stage: model.StageBeforeStep, Target: "*", Action: "mask", Criticality: model.CritCritical},
		{ID: "h2", Stage: model.StageAfterStep, Target: "a", Action: "redact", Criticality: model.CritBestEffort},
		{ID: "h3", Stage: model.StageOnError, Target: "b", Action: "alert", Criticality: model.CritBestEffort},
		// target 不命中任何 step → 其 action 不应进入集合
		{ID: "h4", Stage: model.StageBeforeStep, Target: "zzz", Action: "never", Criticality: model.CritCritical},
		// before_flow（Flow 级）：Target="*" 命中 flow f → 其 action 应进入集合（#72 已实现）
		{ID: "h5", Stage: model.StageBeforeFlow, Target: "*", Action: "never2", Criticality: model.CritCritical},
		// on_success（Flow 级）：Target 指向别的 flow，不命中 f → 其 action 不应进入集合
		{ID: "h6", Stage: model.StageOnSuccess, Target: "other-flow", Action: "never3", Criticality: model.CritCritical},
	}
	got := plannedToolIDs(flow, hooks)
	sort.Strings(got)
	want := []string{"a", "alert", "b", "mask", "never2", "redact"}
	if len(got) != len(want) {
		t.Fatalf("计划用到的 Tool 集合不符：got=%v want=%v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("计划用到的 Tool 集合不符：got=%v want=%v", got, want)
		}
	}
}

// T-plan-2：去重 —— 同一个 Tool 既是 step 的 call 又是 Hook 的 action 时只出现一次。
func TestPlannedToolIDsDedup(t *testing.T) {
	flow := &model.Flow{ID: "f", Steps: []model.Step{{ID: "s1", Call: "a", Input: map[string]interface{}{}}}}
	hooks := []model.Hook{
		{ID: "h1", Stage: model.StageBeforeStep, Target: "a", Action: "a", Criticality: model.CritCritical},
	}
	if got := plannedToolIDs(flow, hooks); len(got) != 1 || got[0] != "a" {
		t.Fatalf("应去重为 [a]，实际 %v", got)
	}
}

// T-plan-3：端到端 —— RunFlowResultObserved 在快照点回报集合，
// 且 Hook 的 action 确实被算进去（这正是静态引用守卫看不到、必须由运行侧上报的部分）。
func TestRunFlowResultObservedReportsToolIDs(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	putTool(t, reg, luaTool("a", `return {}`, objSchema(), objSchema()))
	putTool(t, reg, luaTool("mask", `return {}`, objSchema(), objSchema()))
	// 只作为 Hook 载体存在：没有任何 Flow 引用 mask。
	putHook(t, reg, &model.Hook{
		ID: "h1", Target: "*", Stage: model.StageBeforeStep,
		Action: "mask", Criticality: model.CritBestEffort,
	})
	putFlow(t, reg, &model.Flow{ID: "f", Steps: []model.Step{
		{ID: "s1", Call: "a", Input: map[string]interface{}{}},
	}})

	var got []string
	if _, err := New(reg).RunFlowResultObserved("f", func(ids []string) { got = ids }); err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	sort.Strings(got)
	if len(got) != 2 || got[0] != "a" || got[1] != "mask" {
		t.Fatalf("应回报 [a mask]，实际 %v", got)
	}
}

// T-plan-4：不传观察者时行为与 RunFlowResult 一致（向后兼容，不强制上层关心此事）。
func TestRunFlowResultWithoutObserver(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	putTool(t, reg, luaTool("a", `return {}`, objSchema(), objSchema()))
	putFlow(t, reg, &model.Flow{ID: "f", Steps: []model.Step{{ID: "s1", Call: "a", Input: map[string]interface{}{}}}})
	res, err := New(reg).RunFlowResult("f")
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if _, ok := res.Steps["s1"]; !ok {
		t.Fatalf("结果应含 s1: %v", res.Steps)
	}
}
