package app

import (
	"context"
	"testing"
	"time"

	"gear/internal/engine"
)

// B-1：指数退避序列（50ms 起翻倍，上限 5s）。
func TestBackoffFor(t *testing.T) {
	cases := []struct {
		n    int
		want time.Duration
	}{
		{1, 50 * time.Millisecond},
		{2, 100 * time.Millisecond},
		{3, 200 * time.Millisecond},
		{4, 400 * time.Millisecond},
		{5, 800 * time.Millisecond},
	}
	for _, c := range cases {
		if got := backoffFor(c.n); got != c.want {
			t.Fatalf("backoffFor(%d)=%v，应 %v", c.n, got, c.want)
		}
	}
	// 上限封顶。
	if got := backoffFor(100); got != restartMax {
		t.Fatalf("超多次重启应封顶为 %v，实际 %v", restartMax, got)
	}
	// 非法入参兜底。
	if got := backoffFor(0); got != restartBase {
		t.Fatalf("backoffFor(0) 应兜底为 %v，实际 %v", restartBase, got)
	}
}

// B-2：存活足够久 → 退避计数归零（不被偶发崩溃永久惩罚）。
func TestNextBackoffResetsWhenStable(t *testing.T) {
	h := &engine.ServiceHandle{
		StepID:    "s",
		Restarts:  5,                                // 已累计 5 次
		StartedAt: time.Now().Add(-2 * time.Second), // 但本次存活 2s（> stableWindow）
	}
	if got := nextBackoff(h); got != restartBase {
		t.Fatalf("稳定后应重置为 %v，实际 %v", restartBase, got)
	}
	if h.Restarts != 0 {
		t.Fatalf("稳定后 Restarts 应归零，实际 %d", h.Restarts)
	}
}

// B-3：刚启动就崩 → 退避随累计次数增长。
func TestNextBackoffGrowsWhenCrashLoop(t *testing.T) {
	h := &engine.ServiceHandle{
		StepID:    "s",
		Restarts:  3,
		StartedAt: time.Now(), // 本次几乎立刻退出
	}
	if got := nextBackoff(h); got != 400*time.Millisecond {
		t.Fatalf("崩溃循环第 4 次应等 400ms，实际 %v", got)
	}
	if h.Restarts != 3 {
		t.Fatalf("崩溃循环不应重置计数，实际 %d", h.Restarts)
	}
}

// B-4：集成——崩溃循环被**指数退避**节流。
//
// 断言刻意只取**下界**：等到重启发生 4 次，检查这期间至少消耗了
// 50+100+200+400=750ms 的退避量。若退避退化回固定 50ms，4 次重启约 200ms 就会完成而失败；
// 机器越忙耗时只会更长，故该下界**不受负载影响**（红线 13：时序断言必须相对化）。
// 反方向（「固定窗口内重启次数不超过 N」）禁止使用——满载时必然抖动，
// 曾把「真故障」淹没在「偶发」里。
func TestSuperviseBackoffThrottlesCrashLoop(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	svc := svcTool("exit 0") // 立即退出 ⇒ 崩溃循环
	if err := reg.UpsertTool(svc); err != nil {
		t.Fatalf("注册 Tool 失败: %v", err)
	}
	registerSvcFlow(t, reg, "crash")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sup := New(engine.New(reg), RestartAlways, 0)
	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- sup.Supervise(ctx, []string{"crash"}) }()

	const wantRestarts = 4
	deadline := time.Now().Add(20 * time.Second)
	for sup.RestartCount() < wantRestarts && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	elapsed := time.Since(start)
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Supervise 失败: %v", err)
	}
	if n := sup.RestartCount(); n < wantRestarts {
		t.Fatalf("等待 %d 次重启超时，实际 %d", wantRestarts, n)
	}
	// 下界留 50ms 余量以吸收计时粒度；真实值含进程拉起开销，必然 ≥ 750ms。
	if elapsed < 700*time.Millisecond {
		t.Fatalf("指数退避未生效：达到 %d 次重启仅耗时 %v（至少应消耗约 750ms 退避）", wantRestarts, elapsed)
	}
}
