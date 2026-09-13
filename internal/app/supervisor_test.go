package app

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"gear/internal/engine"
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

func svcTool(entry string) *model.Tool {
	return &model.Tool{
		ID: "svc", Name: "svc", Runtime: model.RuntimeProcess, Entry: entry,
		InputSchema:  map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
		OutputSchema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
		Versions:     []model.ToolVersion{{Version: "1.0.0", State: model.StateStable}},
	}
}

// registerSvcFlow 注册一个只含单个 service step 的 Flow。
func registerSvcFlow(t *testing.T, reg *registry.Registry, flowID string) {
	t.Helper()
	flow := &model.Flow{ID: flowID, Steps: []model.Step{
		{ID: "keep", Call: "svc", Mode: "service", Input: map[string]interface{}{}},
	}}
	if err := reg.UpsertFlow(flow); err != nil {
		t.Fatalf("注册 Flow 失败: %v", err)
	}
}

// S2-restart-never：restart=never 时 service 退出即结束监管，不重启。
func TestSuperviseRestartNever(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	if err := reg.UpsertTool(svcTool("exit 0")); err != nil {
		t.Fatalf("注册 Tool 失败: %v", err)
	}
	registerSvcFlow(t, reg, "n")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sup := New(engine.New(reg), RestartNever, 0)
	if err := sup.Supervise(ctx, []string{"n"}); err != nil {
		t.Fatalf("Supervise 失败: %v", err)
	}
	if sup.RestartCount() != 0 {
		t.Fatalf("restart=never 不应重启，实际 %d", sup.RestartCount())
	}
}

// S2-restart-always：restart=always 时 service 退出后被重启。
func TestSuperviseRestartAlways(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	if err := reg.UpsertTool(svcTool("sleep 0.2")); err != nil {
		t.Fatalf("注册 Tool 失败: %v", err)
	}
	registerSvcFlow(t, reg, "a")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sup := New(engine.New(reg), RestartAlways, 0)
	done := make(chan error, 1)
	go func() { done <- sup.Supervise(ctx, []string{"a"}) }()

	// 等「已发生重启」这一**事件**，而不是「固定 700ms 窗口内至少重启一次」——
	// 后者是绝对时长阈值，机器一忙（-race 全量并行跑包）就偶发假失败（红线 13）。
	deadline := time.Now().Add(15 * time.Second)
	for sup.RestartCount() < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Supervise 失败: %v", err)
	}
	if sup.RestartCount() < 1 {
		t.Fatalf("restart=always 应至少重启一次，实际 %d", sup.RestartCount())
	}
}

// S2-stays-alive：长驻 service 在 ctx 取消前不退出、不重启，取消后监管结束。
func TestSuperviseStaysAliveThenCancel(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	if err := reg.UpsertTool(svcTool("sleep 3")); err != nil {
		t.Fatalf("注册 Tool 失败: %v", err)
	}
	registerSvcFlow(t, reg, "long")

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	sup := New(engine.New(reg), RestartAlways, 0)
	if err := sup.Supervise(ctx, []string{"long"}); err != nil {
		t.Fatalf("Supervise 失败: %v", err)
	}
	if sup.RestartCount() != 0 {
		t.Fatalf("长驻期间不应重启，实际 %d", sup.RestartCount())
	}
}

// S2-maxconcurrent：maxConcurrent=1 时两个 Flow 串行监管，峰值并发为 1。
func TestSuperviseMaxConcurrent(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	if err := reg.UpsertTool(svcTool("sleep 3")); err != nil {
		t.Fatalf("注册 Tool 失败: %v", err)
	}
	registerSvcFlow(t, reg, "l1")
	registerSvcFlow(t, reg, "l2")

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(400*time.Millisecond, cancel)
	sup := New(engine.New(reg), RestartAlways, 1)
	if err := sup.Supervise(ctx, []string{"l1", "l2"}); err != nil {
		t.Fatalf("Supervise 失败: %v", err)
	}
	if got := sup.MaxObserved(); got != 1 {
		t.Fatalf("maxConcurrent=1 时峰值并发应为 1，实际 %d", got)
	}
}

// REQ-APP-HEALTH / #75：service 进程「活着」但健康检查连续失败 → Supervisor 杀掉并重启。
// 用 tcp 探针指向一个永不监听的端口；进程 sleep 长驻，探针必败，达到阈值即触发重启。
func TestProbeRestartsUnhealthyService(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	tool := svcTool("sleep 3") // 进程活着，但从不监听端口 → tcp 探针必败
	tool.HealthProbe = &model.HealthProbe{
		Kind:     model.ProbeKindTCP,
		Addr:     "127.0.0.1:1", // 端口 1 通常无监听，拨号必败
		Interval: "150ms",
		Timeout:  "100ms",
		Threshold: 1,
	}
	if err := reg.UpsertTool(tool); err != nil {
		t.Fatalf("注册 Tool 失败: %v", err)
	}
	registerSvcFlow(t, reg, "hp")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sup := New(engine.New(reg), RestartAlways, 0)
	done := make(chan error, 1)
	go func() { done <- sup.Supervise(ctx, []string{"hp"}) }()

	// 等「健康检查触发重启」事件，而非固定窗口（红线 13：禁用绝对阈值）。
	deadline := time.Now().Add(15 * time.Second)
	for sup.RestartCount() < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Supervise 失败: %v", err)
	}
	if sup.RestartCount() < 1 {
		t.Fatalf("健康检查失败应触发重启，实际 %d", sup.RestartCount())
	}
}

// REQ-APP-HEALTH / #75：无探针的 service 长驻期间不应被探针误重启（回归：探针循环对 nil 立即返回）。
func TestProbeNoRestartWhenHealthyNoProbe(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	if err := reg.UpsertTool(svcTool("sleep 3")); err != nil {
		t.Fatalf("注册 Tool 失败: %v", err)
	}
	registerSvcFlow(t, reg, "np")

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(300*time.Millisecond, cancel)
	sup := New(engine.New(reg), RestartAlways, 0)
	if err := sup.Supervise(ctx, []string{"np"}); err != nil {
		t.Fatalf("Supervise 失败: %v", err)
	}
	if sup.RestartCount() != 0 {
		t.Fatalf("无探针长驻期间不应重启，实际 %d", sup.RestartCount())
	}
}
