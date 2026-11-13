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
	"sort"
	"strings"
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

// superviseGrace 是收到停止信号后等待各 Flow 监管 goroutine 收敛的上限。
//
// 为什么必须有界：superviseOne 已在所有阻塞点响应 ctx，但「响应」不等于「立刻返回」——
// 重启退避最多再睡 5s、ProcKill 要跑 taskkill、once step 要等子进程被杀收尸。
// 没有上限时，一个卡在启动阶段的 Flow 能把整个 `app run` / `Server.Shutdown()`
// 永久挂住（2026-10-29 审出，属停机挂死类缺陷）。
// 超时**绝不静默**：未收敛的 Flow 逐个点名打印，把「谁没停干净」暴露出来。
// 设为变量便于测试注入短时限。
var superviseGrace = 15 * time.Second

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

	// probes 记录每个 handle 当前探针的停止函数（**一 handle 一探针**）。
	// 为什么需要它：旧实现在每次 Relaunch 后无差别 `go probeLoop(...)`，而探针只在
	// ctx 取消或「连续失败把进程杀掉」时才退出。健康服务因外部原因崩溃重启时，
	// 旧探针既不退出也没有被通知 ⇒ 同一 handle 上探针 goroutine 逐次累积
	// （2026-10-29 审出）。这里改为起新探针前先停掉同一 handle 的旧探针。
	probeMu sync.Mutex
	probes  map[*engine.ServiceHandle]*probeToken

	// onRestart 是「发生了一次 service 重启」的观察口（SetOnRestart 注册）。
	// 为什么需要它：REST 面（server.handleCreateApp）在局部 goroutine 里构造 Supervisor，
	// 事后拿不到实例、读不到 RestartCount——崩溃重启对管理面完全不可见，
	// 呈现出来就是 UI 假绿。回调参数是**累计**重启次数（与 RestartCount 同源）。
	onRestart func(total int)
}

// SetOnRestart 注册重启观察回调（total = 累计重启次数）；传 nil 表示取消。
// 回调在重启计数递增后异步上下文中触发，实现方自行保证并发安全。
func (s *Supervisor) SetOnRestart(cb func(total int)) {
	s.mu.Lock()
	s.onRestart = cb
	s.mu.Unlock()
}

// New 构造 Supervisor。maxConcurrent < 1 表示不限（由 Supervise 按 flow 数兜底）。
func New(eng *engine.Engine, policy RestartPolicy, maxConcurrent int) *Supervisor {
	return &Supervisor{eng: eng, policy: policy, maxConcurrent: maxConcurrent, probes: map[*engine.ServiceHandle]*probeToken{}}
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
	// running 记录尚未收敛的 Flow，供等待超时后点名（绝不静默吞掉「谁没停干净」）。
	running := map[string]bool{}

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
			mu.Lock()
			running[fid] = true
			mu.Unlock()
			defer func() {
				mu.Lock()
				delete(running, fid)
				mu.Unlock()
			}()
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
	if !waitGroup(&wg, superviseGrace) {
		mu.Lock()
		pending := make([]string, 0, len(running))
		for fid := range running {
			pending = append(pending, fid)
		}
		mu.Unlock()
		sort.Strings(pending)
		fmt.Printf("! 停止信号已发出 %s，以下 Flow 仍未收敛（其 service 子进程可能仍在运行）: %s\n",
			superviseGrace, strings.Join(pending, ", "))
	}
	return firstErr
}

// waitGroup 有界等待一组 goroutine 结束；超时返回 false（不等到底，不留死等）。
//
// 注意：超时后等待用的那一个 goroutine 仍在 wg.Wait() 上——它随进程退出而消失，
// 且每次调用至多一个，不像「每轮泄漏 N 个」那样会随时间累积。
func waitGroup(wg *sync.WaitGroup, d time.Duration) bool {
	done := make(chan struct{})
	go func() {
		defer close(done)
		wg.Wait()
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

	// ctx 穿进启动阶段：停机时 once step 的子进程会随 ctx 被杀，
	// 监管器不会卡在一个永不返回的启动上（2026-10-29 修）。
	res, err := s.eng.Supervise(ctx, flowID)
	if err != nil {
		return err
	}
	if len(res.Services) == 0 {
		return nil // 纯 once Flow：执行完即结束
	}

	// 为每个 service 句柄启动健康检查探针（REQ-APP-HEALTH / #75）；nil 探针不启动。
	for _, h := range res.Services {
		s.startProbe(ctx, h)
	}
	// 本 Flow 监管结束（无论是停机、restart=never 还是出错）都停掉它的探针：
	// 探针只在 ctx 取消或主动 stop 时退出，少了这一句，正常退出的 Flow 会留下常驻探针。
	defer s.stopProbes(res.Services)

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
		cb := s.onRestart
		total := s.restarts
		s.mu.Unlock()
		if cb != nil {
			cb(total)
		}
		fmt.Printf("↻ step %s service 重启 (第 %d 次)\n", h.StepID, h.Restarts)
		// 重启后该 handle 的进程已换柄：起新探针前先停掉同一 handle 的旧探针
		//（旧探针只在 ctx 取消或 kill 后退出，放任不管会逐次累积）。
		s.startProbe(ctx, h)
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

// probeToken 是一代探针的身份凭据（指针即身份），用于「只摘自己的牌」。
//
// 为什么需要身份：探针退出时要把自己从 s.probes 摘掉，但此刻登记项可能已经是
// 新一代探针（崩溃 → 重启 → 新探针），直接 delete 会误删后来者的登记，
// 让 stopProbes 停不掉新探针。按指针比对，只删自己那一代。
type probeToken struct {
	stop  chan struct{}
	once  sync.Once
	stops func()
}

func newProbeToken() *probeToken {
	t := &probeToken{stop: make(chan struct{})}
	t.stops = func() { t.once.Do(func() { close(t.stop) }) }
	return t
}

// startProbe 为 handle 启动健康检查探针，**保证同一 handle 至多一个探针在跑**。
//
// 旧实现每次 Relaunch 都直接 `go probeLoop(...)`：探针只在 ctx 取消或「连续失败后
// 把进程杀掉」时退出，于是「进程因外部原因崩溃 → 重启」这条最普通的路径上，
// 旧探针既不退出也没被通知，探针 goroutine 随重启次数线性累积（2026-10-29 审出）。
// 这里先停旧的再起新的，并把它登记进 s.probes 供 stopProbes 收尾。
// h.Probe 为 nil（未配置探针）时直接返回——与 probeLoop 内的判据保持单一处语义。
func (s *Supervisor) startProbe(ctx context.Context, h *engine.ServiceHandle) {
	if h.Probe == nil {
		return
	}
	tok := newProbeToken()

	s.probeMu.Lock()
	if old, ok := s.probes[h]; ok {
		old.stops() // 停掉同一 handle 的上一代探针
	}
	s.probes[h] = tok
	s.probeMu.Unlock()

	go func() {
		s.probeLoop(ctx, tok.stop, h)
		s.probeMu.Lock()
		if cur, ok := s.probes[h]; ok && cur == tok {
			delete(s.probes, h)
		}
		s.probeMu.Unlock()
	}()
}

// stopProbe 停掉单个 handle 的探针并摘牌（无探针则 no-op）。
func (s *Supervisor) stopProbe(h *engine.ServiceHandle) {
	s.probeMu.Lock()
	defer s.probeMu.Unlock()
	if tok, ok := s.probes[h]; ok {
		tok.stops()
		delete(s.probes, h)
	}
}

// stopProbes 停掉一批 handle 的探针（监管结束时收尾）。
func (s *Supervisor) stopProbes(handles []*engine.ServiceHandle) {
	for _, h := range handles {
		s.stopProbe(h)
	}
}

func (s *Supervisor) stopAll(handles []*engine.ServiceHandle) {
	for _, h := range handles {
		s.stopProbe(h)
		_ = h.ProcKill()
		// service ext：杀进程后 Deregister 自身 PID；占用列表空则侧车自动退出
		//（EXT_SERVICE_DESIGN.md §12）。Coord 为 nil 时（非 service ext）no-op。
		if h.Coord != nil {
			_ = h.Coord.Close()
		}
	}
}
