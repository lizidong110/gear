package server

import (
	"sync"
	"time"
)

// ConcurrencyGate 是执行入口的全局并发闸门（REQ-CONCURRENCY-CROSS / #73）：
// 由固定数量的 worker goroutine 构成「进程池」，worker 数 = 最大并发执行数；
// 待执行任务进入有界队列，队列满且 worker 全忙时新提交被拒绝（调用方回 429），实现背压。
//
// 为什么是 worker 池而非「每次提交一个 goroutine」：step 级 maxParallel（S4）只限单个 Flow
// 内的 step 并发；此处限的是「同时跑的 Flow 执行数」——防止高扇出场景一次性 fork 上千进程
// 把机器打爆（PRD §14.5）。worker 池把全局并发收敛到可配置上限，超额排队、再超额拒绝。
type ConcurrencyGate struct {
	queue    chan func()
	workers  int
	wg       sync.WaitGroup
	stopOnce sync.Once
}

// NewConcurrencyGate 构造闸门：workers 个常驻 worker（= 并发上限），queueSize 为待执行队列长度
// （0 表示不排队、满即拒绝）。worker 启动后阻塞在队列上，直到 Stop 关闭队列。
//
// 关键：构造结束前用 ready WaitGroup 等全部 worker 真正「就位」（已进入 range 阻塞在接收上），
// 否则首个 Submit 会撞上「worker 尚未被调度」的启动竞态，在 queueSize=0（无缓冲通道）时
// 因 select 的 default 分支误判为拒绝。ready.Wait() 保证 Submit 调用时 worker 已就位。
func NewConcurrencyGate(workers, queueSize int) *ConcurrencyGate {
	if workers < 1 {
		workers = 1
	}
	if queueSize < 0 {
		queueSize = 0
	}
	g := &ConcurrencyGate{
		queue:   make(chan func(), queueSize),
		workers: workers,
	}
	var ready sync.WaitGroup
	ready.Add(workers)
	for i := 0; i < workers; i++ {
		g.wg.Add(1)
		go func() {
			ready.Done() // 声明就位：紧接着进入 range 阻塞在接收上
			defer g.wg.Done()
			for job := range g.queue {
				job()
			}
		}()
	}
	ready.Wait() // 等全部 worker  parked 在接收上，消除启动竞态
	return g
}

// Submit 提交一个执行任务：入队成功返回 true；队列满且 worker 全忙（已达并发上限 + 排队上限）
// 返回 false，调用方应拒绝（429）。非阻塞，执行入口可立即返回 202 或 429。
func (g *ConcurrencyGate) Submit(job func()) bool {
	select {
	case g.queue <- job:
		return true
	default:
		return false
	}
}

// Stop 关闭队列并等待全部 worker 退出（Server.Shutdown 时调用，保证在途执行收敛后再关库）。
func (g *ConcurrencyGate) Stop() {
	g.StopWithTimeout(0)
}

// StopWithTimeout 关闭队列并等待全部 worker 退出；d > 0 时最多等 d，超时返回 false。
//
// 为什么要有超时版本：worker 手上跑的是一整个 Flow 执行，可能包含声明了超长 timeout 的
// Tool。无限等待等于把停机时长交给最慢的那个执行——此前 Server.Shutdown 就卡在这里
// （2026-10-29 审出）。超时返回 false 是**如实上报**：调用方据此把未收敛项点出来，
// 而不是假装已经停干净。
//
// 可重复调用：close 由 once 保护，第二次调用直接返回当前收敛状态（不 panic）。
func (g *ConcurrencyGate) StopWithTimeout(d time.Duration) bool {
	g.stopOnce.Do(func() { close(g.queue) })
	if d <= 0 {
		g.wg.Wait()
		return true
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		g.wg.Wait()
	}()
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-done:
		return true
	case <-t.C:
		return false
	}
}
