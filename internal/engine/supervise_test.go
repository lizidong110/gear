package engine

import (
	"testing"
	"time"

	"gear/internal/model"
)

// svcTool 构造一个 entry 可定制的 process Tool（service 模式测试用）。
func svcTool(entry string) *model.Tool {
	return &model.Tool{
		ID: "svc", Name: "svc", Runtime: model.RuntimeProcess, Entry: entry,
		InputSchema:  map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
		OutputSchema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
		Versions:     []model.ToolVersion{{Version: "1.0.0", State: model.StateStable}},
	}
}

// S2-run-1：flow run（一次性执行）遇到 service step 应显式拒绝（F3 守卫）。
func TestRunFlowRejectsServiceStep(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	if err := reg.UpsertTool(sampleTool()); err != nil {
		t.Fatalf("注册 Tool 失败: %v", err)
	}
	flow := &model.Flow{ID: "svcf", Steps: []model.Step{
		{ID: "a", Call: "hello", Mode: "service", Input: map[string]interface{}{"name": "x"}},
	}}
	if err := reg.UpsertFlow(flow); err != nil {
		t.Fatalf("注册 Flow 失败: %v", err)
	}
	if err := New(reg).RunFlow("svcf"); err == nil {
		t.Fatal("service step 用 flow run 应被拒绝")
	}
}

// S2-sup-1：Supervise 拉起 service step 但不等待，返回存活句柄。
func TestSuperviseLaunchesService(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	if err := reg.UpsertTool(svcTool("sleep 3")); err != nil {
		t.Fatalf("注册 Tool 失败: %v", err)
	}
	flow := &model.Flow{ID: "s", Steps: []model.Step{
		{ID: "keep", Call: "svc", Mode: "service", Input: map[string]interface{}{}},
	}}
	if err := reg.UpsertFlow(flow); err != nil {
		t.Fatalf("注册 Flow 失败: %v", err)
	}
	res, err := New(reg).Supervise("s")
	if err != nil {
		t.Fatalf("Supervise 失败: %v", err)
	}
	if len(res.Services) != 1 {
		t.Fatalf("应有 1 个 service，实际 %d", len(res.Services))
	}
	h := res.Services[0]
	if h.Proc == nil {
		t.Fatal("service 句柄缺进程")
	}
	if h.Proc.Pid() <= 0 {
		t.Fatal("service 进程 pid 非法")
	}
	// 断言子进程确实存活：启动 Wait 并在 150ms 内未退出（平台无关，避免 Windows signal 0 不支持）。
	done := make(chan error, 1)
	go func() { done <- h.Proc.Wait() }()
	select {
	case err := <-done:
		t.Fatalf("service 进程过早退出: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	// 清理：杀掉并等 Wait 归位。
	_ = h.Proc.Kill()
	<-done
}

// S2-sup-2：依赖 service step 的 Flow 被拒绝（service 永不结束）。
func TestServiceDepGuard(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	if err := reg.UpsertTool(svcTool("sleep 3")); err != nil {
		t.Fatalf("注册 Tool 失败: %v", err)
	}
	if err := reg.UpsertTool(sampleTool()); err != nil {
		t.Fatalf("注册 Tool 失败: %v", err)
	}
	flow := &model.Flow{ID: "bad", Steps: []model.Step{
		{ID: "a", Call: "svc", Mode: "service", Input: map[string]interface{}{}},
		{ID: "b", Call: "hello", DependsOn: []string{"a"}, Input: map[string]interface{}{"name": "x"}},
	}}
	if err := reg.UpsertFlow(flow); err != nil {
		t.Fatalf("注册 Flow 失败: %v", err)
	}
	if _, err := New(reg).Supervise("bad"); err == nil {
		t.Fatal("依赖 service step 应被拒绝")
	}
}

// S2-sup-3：纯 once Flow 经 Supervise 正常执行且不产生 service 句柄。
func TestSupervisePureOnce(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	if err := reg.UpsertTool(sampleTool()); err != nil {
		t.Fatalf("注册 Tool 失败: %v", err)
	}
	flow := &model.Flow{ID: "o", Vars: map[string]interface{}{"t": "world"}, Steps: []model.Step{
		{ID: "s1", Call: "hello", Input: map[string]interface{}{"name": "${vars.t}"}},
	}}
	if err := reg.UpsertFlow(flow); err != nil {
		t.Fatalf("注册 Flow 失败: %v", err)
	}
	res, err := New(reg).Supervise("o")
	if err != nil {
		t.Fatalf("Supervise 失败: %v", err)
	}
	if len(res.Services) != 0 {
		t.Fatalf("纯 once 不应有 service，实际 %d", len(res.Services))
	}
}
