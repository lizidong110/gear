package app

import (
	"context"
	"errors"
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

// REQ-APP-MANAGED-STATE：SetOnRestart 重启观察回调——每次重启后收到累计次数。
// REST 面靠它把重启计数回填进 /api/apps（supervisor 在 server 侧是局部创建的，
// 事后读不到 RestartCount，没有回调则崩溃重启对管理面不可见 → UI 假绿）。
func TestSupervisorOnRestartCallback(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	if err := reg.UpsertTool(svcTool("sleep 0.2")); err != nil {
		t.Fatalf("注册 Tool 失败: %v", err)
	}
	registerSvcFlow(t, reg, "cb")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sup := New(engine.New(reg), RestartAlways, 0)
	totals := make(chan int, 64)
	sup.SetOnRestart(func(total int) { totals <- total })
	done := make(chan error, 1)
	go func() { done <- sup.Supervise(ctx, []string{"cb"}) }()

	// 等「回调已触发」这一**事件**，而不是固定窗口（红线 13）。
	deadline := time.Now().Add(15 * time.Second)
	last := 0
	for last < 1 && time.Now().Before(deadline) {
		select {
		case last = <-totals:
		case <-time.After(50 * time.Millisecond):
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Supervise 失败: %v", err)
	}
	if last < 1 {
		t.Fatal("restart=always 下重启回调应至少触发一次")
	}
	if n := sup.RestartCount(); n < last {
		t.Fatalf("回调累计(%d) 不应超过 RestartCount(%d)", last, n)
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

// 回归（2026-11-13 实测复现）：a9c1cff 把正常监管路径的「等到底」换成了有界
// waitGroup(superviseGrace)，长驻 app run 启动 15s 后被误判「未收敛」而整体退出、
// service 子进程连坐（托管启动的 study「起一会儿就退出」的元凶）。
// 双段等待语义：ctx 未取消 → supervise 必须阻塞到底，哪怕远超 superviseGrace。
func TestSuperviseKeepsRunningBeyondGrace(t *testing.T) {
	oldGrace := superviseGrace
	superviseGrace = 150 * time.Millisecond
	defer func() { superviseGrace = oldGrace }()

	reg := newTestRegistry(t)
	defer reg.Close()
	if err := reg.UpsertTool(svcTool("sleep 30")); err != nil {
		t.Fatalf("注册 Tool 失败: %v", err)
	}
	registerSvcFlow(t, reg, "stay")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sup := New(engine.New(reg), RestartAlways, 0)
	done := make(chan error, 1)
	go func() { done <- sup.Supervise(ctx, []string{"stay"}) }()

	// 核心断言：远超 superviseGrace 后监管仍未返回（旧实现在此早已「自杀」）。
	select {
	case err := <-done:
		t.Fatalf("ctx 取消前 Supervise 提前返回（15 秒自杀回归）: %v", err)
	case <-time.After(2 * time.Second):
	}

	// 停机段仍有效：取消后有界收敛、无错返回。
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Supervise 失败: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("取消后 30s 未收敛，停机路径挂死")
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
		Kind:      model.ProbeKindTCP,
		Addr:      "127.0.0.1:1", // 端口 1 通常无监听，拨号必败
		Interval:  "150ms",
		Timeout:   "100ms",
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

// TestSuperviseRestartBudgetStopsRestarting：重启预算耗尽后必须**停手**，不再无限复活。
//
// 这条修的是 2026-11-13 实测的行为：手动 kill 掉 service 子进程（如 study.api 的
// `gear.exe serve --no-ui`），监管器按 restart=always 每 5s 拉回一个，杀几次起几次，
// 台账 restarts 一路涨，永不停止——旧实现只有退避、没有上限。
func TestSuperviseRestartBudgetStopsRestarting(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	if err := reg.UpsertTool(svcTool("exit 0")); err != nil {
		t.Fatalf("注册 Tool 失败: %v", err)
	}
	registerSvcFlow(t, reg, "crash")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sup := New(engine.New(reg), RestartAlways, 0)
	sup.SetRestartLimit(2, time.Minute)

	done := make(chan error, 1)
	go func() { done <- sup.Supervise(ctx, []string{"crash"}) }()

	select {
	case err := <-done:
		if !errors.Is(err, ErrRestartExhausted) {
			t.Fatalf("预算耗尽应返回 ErrRestartExhausted，实际 %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("预算耗尽后 Supervise 仍未返回（仍在无限重启）")
	}
	if n := sup.RestartCount(); n != 2 {
		t.Fatalf("limit=2 时应恰好重启 2 次后停手，实际 %d", n)
	}
}

// TestRestartBudgetWindowSlides：预算按滑动窗口统计，不累计——稳定运行的服务偶发崩几次
// 不能被永久判死（这条是「加预算」这个改动本身最容易引入的回归）。
func TestRestartBudgetWindowSlides(t *testing.T) {
	sup := New(nil, RestartAlways, 0)
	sup.SetRestartLimit(2, 80*time.Millisecond)
	h := &engine.ServiceHandle{StepID: "s"}

	for i := 1; i <= 2; i++ {
		if over, _, _ := sup.overRestartLimit(h); over {
			t.Fatalf("窗口内第 %d 次退出不应超限", i)
		}
	}
	if over, _, _ := sup.overRestartLimit(h); !over {
		t.Fatal("窗口内第 3 次退出应超限")
	}
	time.Sleep(120 * time.Millisecond)
	if over, _, _ := sup.overRestartLimit(h); over {
		t.Fatal("窗口滑走后不应继续判超限（长期稳定服务不能被累计惩罚）")
	}
}

// TestRestartBudgetDisabled：limit<=0 = 旧的无上限行为，仅供兼容开关使用。
func TestRestartBudgetDisabled(t *testing.T) {
	sup := New(nil, RestartAlways, 0)
	sup.SetRestartLimit(0, time.Minute)
	h := &engine.ServiceHandle{StepID: "s"}
	for i := 0; i < 50; i++ {
		if over, _, _ := sup.overRestartLimit(h); over {
			t.Fatalf("limit=0 时第 %d 次退出不应超限（应保持无上限）", i+1)
		}
	}
}
