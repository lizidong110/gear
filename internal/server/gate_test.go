package server

import (
	"sync"
	"testing"
	"time"
)

// TestConcurrencyGateRunsAllUnderCapacity 验证「容量内」全部任务被接收并执行（worker 池 + 队列都够用）。
func TestConcurrencyGateRunsAllUnderCapacity(t *testing.T) {
	g := NewConcurrencyGate(4, 8)
	var mu sync.Mutex
	n := 0
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		if !g.Submit(func() {
			mu.Lock()
			n++
			mu.Unlock()
			wg.Done()
		}) {
			t.Fatalf("submit %d 在容量内不应被拒", i)
		}
	}
	wg.Wait()
	if n != 10 {
		t.Fatalf("期望运行 10 个任务, 实得 %d", n)
	}
	g.Stop()
}

// TestConcurrencyGateRejectsWhenFull 验证「worker 全忙 + 队列满」时 Submit 返回 false（入口拒绝的判据）。
func TestConcurrencyGateRejectsWhenFull(t *testing.T) {
	started := make(chan struct{})
	block := make(chan struct{})
	g := NewConcurrencyGate(1, 0) // 单 worker、不排队
	// 第一个任务占住唯一 worker
	if !g.Submit(func() { close(started); <-block }) {
		t.Fatal("首个任务应被接受")
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("worker 未启动首个任务")
	}
	// worker 忙碌且队列=0 → 第二个提交必须被拒（非阻塞、立即 false）
	if g.Submit(func() {}) {
		t.Fatal("闸门满时提交应被拒绝")
	}
	close(block)
	g.Stop()
}

// TestConcurrencyGateStopWaitsForInFlight 验证 Stop 会阻塞直到在途任务收敛（Shutdown 安全停机的前提）。
func TestConcurrencyGateStopWaitsForInFlight(t *testing.T) {
	done := make(chan struct{})
	block := make(chan struct{})
	g := NewConcurrencyGate(1, 0)
	g.Submit(func() { <-block; close(done) })
	time.Sleep(20 * time.Millisecond) // 让 worker 真正进入任务
	close(block)

	doneCh := make(chan struct{})
	go func() { g.Stop(); close(doneCh) }()

	// 在途任务完成前，Stop 不应返回
	select {
	case <-doneCh:
		t.Fatal("Stop 不应在在途任务完成前返回")
	case <-done:
		// 在途任务已完成，接下来 Stop 应该很快返回
	}
	select {
	case <-doneCh:
	case <-time.After(time.Second):
		t.Fatal("Stop 应在在途任务完成后返回")
	}
}

// TestConcurrencyGateStopWithTimeoutReturnsFalseWhenStuck 验证 StopWithTimeout 在
// 在途任务不肯退出时能**超时返回 false**而非死等（2026-10-29 修：此前 Server.Shutdown
// 经无界 gate.Stop 卡死，一个长跑 Flow 就能把停机拖成永久挂起）。
func TestConcurrencyGateStopWithTimeoutReturnsFalseWhenStuck(t *testing.T) {
	block := make(chan struct{})
	g := NewConcurrencyGate(1, 0)
	g.Submit(func() { <-block }) // 占住唯一 worker 且不退出
	time.Sleep(20 * time.Millisecond)

	start := time.Now()
	if g.StopWithTimeout(150 * time.Millisecond) {
		t.Fatal("worker 卡住时 StopWithTimeout 应返回 false")
	}
	if elapsed := time.Since(start); elapsed > 600*time.Millisecond {
		t.Fatalf("StopWithTimeout 不应远超时限（实耗 %v）", elapsed)
	}
	// 收尾：放行 worker，避免测试泄漏 goroutine。
	close(block)
}
