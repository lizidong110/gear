// Package app 实现 Gear 的「app run」进程监管（S2，见 PRD §14.2/§14.3）。
//
// 内核三件套 = 引擎 + ctx.* + app run。本包是 app run 的监管循环：
//   - 运行 Flow 的 once 步骤（交给引擎）
//   - 拉起并监管 service 步骤（交给引擎，本包持有其生命周期）
//   - 按 restart 策略重启崩溃的 service
//   - 响应 SIGINT/SIGTERM 平滑停止
//   - 以 maxConcurrent 限制同时启动的 Flow 数（S2 并发闸门，进程池留 S4）
package app

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"gear/internal/engine"
)

// RestartPolicy 决定 service 子进程退出后的行为。
type RestartPolicy string

const (
	// RestartAlways service 退出即重启（崩溃循环有固定退避，见 superviseOne）。
	RestartAlways RestartPolicy = "always"
	// RestartNever service 退出即停止整个 Flow 监管（不重启）。
	RestartNever RestartPolicy = "never"
)

const (
	// restartBase 是首次重启前的等待；崩溃循环按 2 倍递增，直到 restartMax（S4 指数退避）。
	restartBase = 50 * time.Millisecond
	restartMax  = 5 * time.Second
	// stableWindow：service 存活超过该时长视为「稳定」，退避计数归零
	//（避免一次偶发崩溃让后续重启被永久惩罚）。
	stableWindow = time.Second
)

// nextBackoff 计算该 service 本次重启前应等待的时长。
// 若本次存活时长 ≥ stableWindow，说明不是崩溃循环，退避计数归零（只等 restartBase）。
func nextBackoff(h *engine.ServiceHandle) time.Duration {
	if time.Since(h.StartedAt) >= stableWindow {
		h.Restarts = 0
	}
	return backoffFor(h.Restarts + 1)
}

// backoffFor 返回第 n 次重启（n>=1）前应等待的时长：restartBase 起指数翻倍，上限 restartMax。
func backoffFor(n int) time.Duration {
	if n < 1 {
		n = 1
	}
	d := restartBase
	for i := 1; i < n; i++ {
		if d >= restartMax {
			return restartMax
		}
		d *= 2
	}
	if d > restartMax {
		return restartMax
	}
	return d
}

// Supervisor 承载 app run 的进程监管。
type Supervisor struct {
	eng           *engine.Engine
	policy        RestartPolicy
	maxConcurrent int

	mu          sync.Mutex
	active      int
	maxObserved int
	restarts    int // 累计重启次数（restart=always 时 > 0，测试可观测）
}

// New 构造 Supervisor。maxConcurrent < 1 表示不限（由 Supervise 按 flow 数兜底）。
func New(eng *engine.Engine, policy RestartPolicy, maxConcurrent int) *Supervisor {
	return &Supervisor{eng: eng, policy: policy, maxConcurrent: maxConcurrent}
}

// MaxObserved 返回监管期间同时运行的 Flow 数的峰值（测试并发闸门用）。
func (s *Supervisor) MaxObserved() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.maxObserved
}

// RestartCount 返回累计重启次数（restart=always 时 > 0，测试可观测）。
func (s *Supervisor) RestartCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.restarts
}

// ContextWithSignals 返回在收到 SIGINT/SIGTERM 时取消的 ctx（CLI 用）。
func ContextWithSignals(parent context.Context) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
}

// Supervise 监管 flowIDs 列表：运行其 once 步骤、拉起 service 步骤，阻塞直到 ctx 取消
// 或（restart=never 时）所有 service 退出。maxConcurrent 限制并发启动的 Flow 数。
func (s *Supervisor) Supervise(ctx context.Context, flowIDs []string) error {
	cap := s.maxConcurrent
	if cap < 1 {
		cap = len(flowIDs)
		if cap < 1 {
			cap = 1
		}
	}
	sem := make(chan struct{}, cap)

	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error

	for _, fid := range flowIDs {
		select {
		case <-ctx.Done():
			goto done
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(fid string) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := s.superviseOne(ctx, fid); err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
				fmt.Printf("! Flow %s 监管结束: %v\n", fid, err)
			}
		}(fid)
	}
done:
	wg.Wait()
	return firstErr
}

func (s *Supervisor) superviseOne(ctx context.Context, flowID string) error {
	s.mu.Lock()
	s.active++
	if s.active > s.maxObserved {
		s.maxObserved = s.active
	}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.active--
		s.mu.Unlock()
	}()

	res, err := s.eng.Supervise(flowID)
	if err != nil {
		return err
	}
	if len(res.Services) == 0 {
		return nil // 纯 once Flow：执行完即结束
	}

	// 为每个 service 句柄启动健康检查探针（REQ-APP-HEALTH / #75）；nil 探针不启动。
	for _, h := range res.Services {
		go s.probeLoop(ctx, h)
	}

	for {
		select {
		case <-ctx.Done():
			s.stopAll(res.Services)
			return nil
		default:
		}

		h := s.waitAny(ctx, res.Services)
		if h == nil {
			s.stopAll(res.Services)
			return nil // ctx 取消
		}
		// 先说清「为什么退出」，再谈重启：否则端口占用 / 配置写错这类秒退
		// 只会表现为无限重启，原因完全不可见。
		reportServiceExit(h)
		if s.policy == RestartNever {
			s.stopAll(res.Services)
			return nil
		}
		// 崩溃循环退避：本次存活足够久 → 视为稳定，退避计数归零。
		time.Sleep(nextBackoff(h))
		if ctx.Err() != nil {
			s.stopAll(res.Services)
			return nil
		}
		if err := s.eng.Relaunch(h); err != nil {
			s.stopAll(res.Services)
			return err
		}
		s.mu.Lock()
		s.restarts++
		s.mu.Unlock()
		fmt.Printf("↻ step %s service 重启 (第 %d 次)\n", h.StepID, h.Restarts)
		// 重启后该 handle 的进程已换柄，启动新一轮探针（旧探针循环已在 procKill 后退出）。
		go s.probeLoop(ctx, h)
	}
}

// reportServiceExit 打印 service 的退出原因：退出码 + 输出尾部若干行。
//
// 为什么必须有这个出口：service 子进程的输出**不继承调用方管道**（红线 16，避免孤儿
// 孙进程占住 `go test` 的管道），所以它天然看不见自己的输出。若不给一个出口，
// 「拉起即秒退」的故障（端口被占用、命令写错、依赖缺失）在监管侧就只剩下
// 「↻ 重启 第 N 次」——2026-09-21 就是这样：`:8787` 被占，`gear serve` 秒退，
// 用户看到无限重启却拿不到任何线索。
//
// 该 runtime 的 Process 未实现 ExitReporter 时如实说明「拿不到退出信息」，
// 不静默假装知道。
func reportServiceExit(h *engine.ServiceHandle) {
	rep, ok := h.CurrentProc().(engine.ExitReporter)
	if !ok {
		fmt.Printf("✗ step %s service 退出（该 runtime 未提供退出信息）\n", h.StepID)
		return
	}
	code, tail := rep.ExitInfo()
	fmt.Printf("✗ step %s service 退出 (exit=%d)\n", h.StepID, code)
	for _, ln := range tail {
		fmt.Printf("    │ %s\n", ln)
	}
}

// waitAny 等待任意一个 service 需要重启；ctx 取消时返回 nil。
//
// 注意「需要重启」不等于「有进程退出」：service ext 退让为 client 共享他人 sidecar 时
// 手上没有本地进程，此时不算退出（见 engine.ServiceHandle.WaitExit）——否则会把共享成功
// 误判成崩溃、进而无限重启。
func (s *Supervisor) waitAny(ctx context.Context, handles []*engine.ServiceHandle) *engine.ServiceHandle {
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()

	ch := make(chan *engine.ServiceHandle, 1)
	for _, h := range handles {
		go func(h *engine.ServiceHandle) {
			select {
			case <-wctx.Done():
			case <-h.WaitExit(wctx):
				select {
				case ch <- h:
				case <-wctx.Done():
				}
			}
		}(h)
	}
	select {
	case <-wctx.Done():
		return nil
	case h := <-ch:
		return h
	}
}

func (s *Supervisor) stopAll(handles []*engine.ServiceHandle) {
	for _, h := range handles {
		_ = h.ProcKill()
		// service ext：杀进程后 Deregister 自身 PID；占用列表空则侧车自动退出
		//（EXT_SERVICE_DESIGN.md §12）。Coord 为 nil 时（非 service ext）no-op。
		if h.Coord != nil {
			_ = h.Coord.Close()
		}
	}
}
