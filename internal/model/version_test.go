package model

import "testing"

// S5：版本状态机合法迁移。
func TestCanPromote(t *testing.T) {
	cases := []struct {
		from, to ToolVersionState
		ok       bool
	}{
		{StateDraft, StateTesting, true},
		{StateDraft, StateStable, true},
		{StateDraft, StateRejected, true},
		{StateDraft, StateDeprecated, false}, // draft 不能直接 deprecated（跳过 testing）
		{StateTesting, StateStable, true},
		{StateTesting, StateDraft, true}, // 回退复测
		{StateTesting, StateRejected, true},
		{StateTesting, StateDeprecated, false}, // testing 不能直接 deprecated
		{StateStable, StateTesting, true},      // 回退复测
		{StateStable, StateDeprecated, true},
		{StateStable, StateRejected, true},
		{StateStable, StateDraft, false}, // stable 不能跳级回 draft
		{StateDeprecated, StateStable, true},
		{StateDeprecated, StateRejected, true},
		{StateRejected, StateDraft, true},
		{StateRejected, StateStable, false}, // rejected 不能跳级复活
	}
	for _, c := range cases {
		if got := CanPromote(c.from, c.to); got != c.ok {
			t.Fatalf("CanPromote(%s -> %s) = %v, 期望 %v", c.from, c.to, got, c.ok)
		}
	}
}

// S5：Flow.Validate 不再拒绝 use_version（灰度已实现）。
func TestFlowValidateAllowsUseVersion(t *testing.T) {
	f := Flow{
		ID: "f",
		Steps: []Step{
			{ID: "s1", Call: "t", UseVersion: "2.0.0"},
			{ID: "s2", Call: "t", DependsOn: []string{"s1"}},
		},
	}
	if err := f.Validate(); err != nil {
		t.Fatalf("use_version 不应再被拒绝: %v", err)
	}
}

// S5：Hook.Validate 允许 before_promote 阶段（由 PromoteTool 触发）。
func TestHookValidateAllowsBeforePromote(t *testing.T) {
	h := Hook{
		ID: "h", Target: "*", Stage: StageBeforePromote,
		Action: "gate", Criticality: CritCritical,
	}
	if err := h.Validate(); err != nil {
		t.Fatalf("before_promote 应允许注册: %v", err)
	}
}
