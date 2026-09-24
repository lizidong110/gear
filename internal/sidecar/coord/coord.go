// Package coord 是 Gear 的 sidecar 协调 SDK（EXT_SERVICE_DESIGN.md §12）。
//
// 解决的问题：同主机上多个 gear 进程引用同一个 service ext（如 ext.mqtt）时，
// 每个 gear 都会想拉起自己的 sidecar 子进程，导致端口冲突（EADDRINUSE）。
// coord 用一套轻量协调协议把多个 gear 收敛到「单个共享 sidecar 实例」：
//
//   - sidecar 侧（Coordinator）：Serve 绑定端口并提供 /register /heartbeat /deregister
//     /occupants 端点，维护 occupants: map[pid]→{token,lastSeen}，周期巡检剔除
//     「进程已死」或「心跳超时」的占用项，列表为空即触发 OnEmpty 自停（§12.5）。
//   - gear 侧（Client）：RegisterOrStart 走「注册优先 / 竞态退让」握手（§12.4）——
//     先尝试注册自身 PID；无人监听则本 gear 当 owner 拉起 sidecar，否则退让为 client
//     复用同一实例；注册成功后启动看门狗（§12.4.4）周期心跳，连续失败则重走握手
//     （必要时成为新 owner），使「sidecar 崩溃后自愈」这条链路成立。
//
// wire 协议（§12.3）：/register 收 {pid,token,ttl}，回 {"ok":true} 或
// {"ok":false,"reason":"taken"}；token 约束心跳/注销的所有权，用于防「PID 复用
// 导致的幽灵占用」（§12.6，双保险：token 匹配 + 心跳 TTL 超时）。
//
// **恢复说明**：本包在 2026-09-19 的批量误删事件中整体丢失，且原件从未进入 git、
// 四路取证（git 全对象库 / 全盘搜索 / 回收站 / 编辑器本地历史）均无副本（其
// modify_backup 记录为 0 字节的「新增」占位）。故本实现按 EXT_SERVICE_DESIGN.md §12
// 的协议与验收条款完整重建；API 面（Options/New/Serve/IsAddrInUse/ExitAddrInUse/
// NewClient/RegisterOrStart/Session.Proc/Session.Close）与 8 个调用点逐一比对吻合。
package coord

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ExitAddrInUse 是 sidecar 在端口被占用时「退让退出」的退出码。
// sidecar main 约定：if coord.IsAddrInUse(err) { os.Exit(coord.ExitAddrInUse) }。
const ExitAddrInUse = 2

const (
	// startupBudget 是 owner 拉起 sidecar 后等待其就绪（绑端口并接受注册）的时间预算。
	// 必须小于巡检间隔（Options.CheckInterval），否则新 sidecar 可能在 owner 注册成功前
	// 就因「占用列表为空」触发 OnEmpty 自停（§12.5 / §12.6 讨论的边界）。
	startupBudget = 4 * time.Second
	// takeoverBudget 是竞态退让后重试注册到「胜者」实例的时间预算。
	// §12.9 要求至多一轮重试即收敛，故给一个短预算。
	takeoverBudget = 5 * time.Second
	// takenRetryFactor：step-1 拿到 taken（PID 被别的进程身份占用）时的重试预算倍数。
	// 旧占用要等心跳超时（≈TTL）才被剔除，故预算取 2×TTL 留出余量（§12.6）。
	takenRetryFactor = 2
	// heartbeatFailureThreshold 是「连续心跳失败多少次」才重走握手（§12.4.4）。
	// 取 2 而非 1：容忍偶发的网络抖动/瞬时超时，避免误判为 sidecar 已崩。
	heartbeatFailureThreshold = 2
	// emptyGrace 是「尚未被任何 gear 认领」时的空列表宽限期（见 poll 的抑制条件）。
	// 覆盖两种场景：① 启动竞态里 owner 还没来得及注册；② owner 拉起 sidecar 后立刻崩溃
	// （无人认领，超期后仍会自停，不留孤儿进程）。
	emptyGrace = 15 * time.Second
)

// Process 是 service 进程 / sidecar 子进程的抽象句柄，与 engine.Runtime.Start 的返回值契约一致。
//
// 方法集必须与 engine.Process（Pid/Kill/Wait）**完全一致**——两侧句柄要互相赋值：
// hook.startServiceStep 把 engine 侧进程句柄传进 RegisterOrStart，又把 Session.Proc()
// 装进 ServiceHandle.Proc。少一个方法就会在该赋值处编译失败。
// 调用方依赖 Kill/Wait（ServiceHandle 的 ProcKill / ProcWaitDone）与 Pid（进程归属、诊断、
// 以及看门狗判断「本地 sidecar 是否仍存活」）。
type Process interface {
	Pid() int
	Kill() error
	Wait() error
}

// noopProcess 用于「client 退让」场景：本 gear 没拉起本地 sidecar（复用他人实例），
// 此时没有可 kill 的本地进程，Proc() 返回 no-op 实现，Supervisor 调 Kill/Wait 无害。
type noopProcess struct{}

func (noopProcess) Pid() int    { return 0 } // 共享实例：本 gear 未拉进程，无 PID
func (noopProcess) Kill() error { return nil }
func (noopProcess) Wait() error { return nil }

// Options 配置 sidecar 侧的协调器。
type Options struct {
	// CheckInterval 存活巡检间隔（默认 5s）。
	CheckInterval time.Duration
	// TTL 占用项心跳超时的**默认**值（默认 15s）；注册方可在 /register 里声明自己的 ttl。
	TTL time.Duration
	// OnEmpty 占用列表变空时的回调（sidecar 借此自停：svc.Stop + os.Exit）。
	OnEmpty func()
}

// Coordinator 是 sidecar 侧的协调服务。
type Coordinator struct {
	opts Options

	mu          sync.Mutex
	occupants   map[int]*occupant
	onEmptyOnce sync.Once
	// claimed 表示「本 sidecar 至少被注册过一次（有人认领）」。
	// 与 startedAt 一起用于抑制「启动竞态期的空列表误自停」——见 poll 的说明。
	claimed bool
	// startedAt 是开始对外服务（绑定端口）的时刻。
	startedAt time.Time

	srv *http.Server
}

// occupant 是一条占用记录：谁的 PID、凭哪个 token 持有、有效期多长、最后一次露面时间。
type occupant struct {
	PID      int
	Token    string
	TTL      time.Duration
	LastSeen time.Time
}

// New 构造一个协调器（sidecar 侧）。零值字段补默认值。
func New(opts Options) *Coordinator {
	if opts.CheckInterval <= 0 {
		opts.CheckInterval = 5 * time.Second
	}
	if opts.TTL <= 0 {
		opts.TTL = 15 * time.Second
	}
	return &Coordinator{
		opts:      opts,
		occupants: make(map[int]*occupant),
	}
}

// addrInUseError 表示绑定端口被占用（sidecar 应退让退出，而非自杀）。
type addrInUseError struct{ addr string }

func (e *addrInUseError) Error() string {
	return fmt.Sprintf("coord: address %s already in use", e.addr)
}

// IsAddrInUse 判断错误是否「端口被占用」，供 sidecar main 决定是否退让退出。
func IsAddrInUse(err error) bool {
	if err == nil {
		return false
	}
	var aie *addrInUseError
	return errors.As(err, &aie)
}

// Serve 绑定 addr 并阻塞服务：在自带 /register /heartbeat /deregister /occupants 端点之外，
// 还挂载业务 handlers（publish / subscribe 等）。
//
//   - 绑定失败（端口被占用）→ 返回 *addrInUseError（main 据此 os.Exit(ExitAddrInUse) 退让）。
//   - 正常由 OnEmpty 触发 srv.Shutdown → 返回 nil。
//
// 注意：addr 约定为 localhost（127.0.0.1 / ::1），注册不暴露到网络（§12.6）。
func (c *Coordinator) Serve(addr string, handlers map[string]http.HandlerFunc) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		if isAddrInUseErr(err) {
			return &addrInUseError{addr: addr}
		}
		return fmt.Errorf("coord: listen %s: %w", addr, err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/register", c.handleRegister)
	mux.HandleFunc("/heartbeat", c.handleHeartbeat)
	mux.HandleFunc("/deregister", c.handleDeregister)
	mux.HandleFunc("/occupants", c.handleOccupants)
	for path, h := range handlers {
		mux.HandleFunc(path, h)
	}

	c.srv = &http.Server{Handler: mux}
	c.mu.Lock()
	c.startedAt = time.Now()
	c.mu.Unlock()
	go c.pollLoop()

	if serr := c.srv.Serve(ln); serr != nil && !errors.Is(serr, http.ErrServerClosed) {
		return serr
	}
	return nil
}

// pollLoop 周期巡检（§12.5）。首轮巡检在 CheckInterval 之后，这天然给 owner 留出
// 「拉起 sidecar → 注册」的时间窗；一旦巡检发现列表为空即自停，不再继续巡检。
func (c *Coordinator) pollLoop() {
	t := time.NewTicker(c.opts.CheckInterval)
	defer t.Stop()
	for range t.C {
		if c.poll() {
			return
		}
	}
}

// poll 执行一次巡检，返回是否「因列表为空而触发自停」。
//
// §12.5 判定条件：`!processAlive(pid) || now-lastSeen > ttl` → 剔除。
// 两个条件缺一不可——只看心跳会漏掉「进程已死但 TTL 未到」的占用（sidecar 会多活一个 TTL），
// 只看 PID 会漏掉「PID 被复用/进程存活但 gear 已不再关心该服务」的占用（§12.6 双保险）。
//
// 自停条件比 §12.5 的伪码多一层抑制：**仅当已被人认领过、或已超过 emptyGrace 仍未认领**时，
// 空列表才触发 OnEmpty。原因是 §12.5 的伪码对「刚启动、占用列表天然为空」的新 sidecar 会
// 在首轮巡检（CheckInterval 后）直接自停，而 owner 可能还在注册退避中——§12.6 也承认
// 「列表短暂为空时新 sidecar 可能误退，看门狗会再拉起」。加这层抑制后：
//   - 正常路径（owner 注册 → 使用者退出后清空）自停行为不变（§12.9 验收仍成立）；
//   - 启动竞态不再误自停（不必依赖「看门狗慢速重拉」兜底）；
//   - owner 拉起 sidecar 后立刻崩溃 → 无人认领 → 超过 emptyGrace 仍会自停，不留孤儿进程。
func (c *Coordinator) poll() bool {
	c.mu.Lock()
	now := time.Now()
	for pid, o := range c.occupants {
		ttl := o.TTL
		if ttl <= 0 {
			ttl = c.opts.TTL
		}
		if now.Sub(o.LastSeen) > ttl || !processAlive(pid) {
			delete(c.occupants, pid)
		}
	}
	empty := len(c.occupants) == 0
	// startedAt 为零值（如单测直接调 poll 而未走 Serve）时视为「已远超宽限期」。
	unclaimedExpired := c.startedAt.IsZero() || now.Sub(c.startedAt) > emptyGrace
	c.mu.Unlock()

	if empty && (c.isClaimed() || unclaimedExpired) {
		c.fireOnEmpty()
		return true
	}
	return false
}

func (c *Coordinator) isClaimed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.claimed
}

// fireOnEmpty 在占用列表变空时调用 OnEmpty（仅触发一次，避免重复退出）。
func (c *Coordinator) fireOnEmpty() {
	if c.opts.OnEmpty == nil {
		return
	}
	c.onEmptyOnce.Do(func() {
		// 在独立 goroutine 里 Shutdown，避免从 handler 内调用导致的死锁；
		// 真正退出由 sidecar 的 OnEmpty（svc.Stop + os.Exit）完成。
		go func() {
			if c.srv != nil {
				_ = c.srv.Shutdown(context.Background())
			}
		}()
		c.opts.OnEmpty()
	})
}

// registerReq 是 /register 的请求体（§12.3）。
type registerReq struct {
	PID   int    `json:"pid"`
	Token string `json:"token"`
	TTL   int    `json:"ttl"` // 秒
}

// handleRegister 处理注册（§12.3 / §12.4）。
//
// 返回 {"ok":true} 表示占用已登记；返回 {"ok":false,"reason":"taken"} 表示**同一个 PID
// 已被另一个进程身份（不同 token）占用**——即发生了 PID 复用（§12.6）。此时调用方应带退避
// 重试：旧占用会因「进程已死」或「心跳超时」被巡检剔除，随后重试即成功。
//
// 之所以不直接覆盖旧记录：那会让「已死 gear 的 PID 被新无关进程占用」直接接管条目，
// 丢失 §12.6 想防的可见性；用 taken 让调用方显式退避，语义更诚实。
//
// 同 PID 同 token 的重复注册是幂等的（刷新 lastSeen），便于看门狗重握手时无脑重发。
func (c *Coordinator) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req registerReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.PID <= 0 || req.Token == "" {
		writeJSON(w, map[string]any{"ok": false, "reason": "bad_request"})
		return
	}

	// ttl 由注册方声明（秒），缺失/异常时回落服务端默认值；上限 2×服务端 TTL，
	// 防止某个调用方声明超大 ttl 让死占用长期滞留。
	ttl := time.Duration(req.TTL) * time.Second
	if ttl <= 0 {
		ttl = c.opts.TTL
	}
	if ttl < time.Second {
		ttl = time.Second
	}
	if max := 2 * c.opts.TTL; ttl > max {
		ttl = max
	}

	c.mu.Lock()
	if old, ok := c.occupants[req.PID]; ok && old.Token != req.Token {
		c.mu.Unlock()
		writeJSON(w, map[string]any{"ok": false, "reason": "taken"})
		return
	}
	c.occupants[req.PID] = &occupant{PID: req.PID, Token: req.Token, TTL: ttl, LastSeen: time.Now()}
	c.claimed = true // 已被认领：此后空列表即可自停（见 poll）
	c.mu.Unlock()
	writeJSON(w, map[string]any{"ok": true})
}

// handleHeartbeat 刷新 lastSeen（§12.3）。**必须 PID+token 匹配**（§12.6）：
// 不匹配（含条目已被巡检剔除）返回 {"ok":false,"reason":"taken"}，
// 调用方看门狗据此重走注册握手。
func (c *Coordinator) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	var req registerReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.PID <= 0 || req.Token == "" {
		writeJSON(w, map[string]any{"ok": false, "reason": "bad_request"})
		return
	}
	c.mu.Lock()
	o, ok := c.occupants[req.PID]
	if !ok || o.Token != req.Token {
		c.mu.Unlock()
		writeJSON(w, map[string]any{"ok": false, "reason": "taken"})
		return
	}
	o.LastSeen = time.Now()
	c.mu.Unlock()
	writeJSON(w, map[string]any{"ok": true})
}

// handleDeregister 注销占用（§12.3），同样要求 PID+token 匹配。
// 条目本就不存在视为成功（幂等：进程崩溃后看门狗/兜底路径可能重复注销）。
// token 不匹配返回 ok:false 且**不删除**——避免冒名注销他人占用。
func (c *Coordinator) handleDeregister(w http.ResponseWriter, r *http.Request) {
	var req registerReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.PID <= 0 || req.Token == "" {
		writeJSON(w, map[string]any{"ok": false, "reason": "bad_request"})
		return
	}
	c.mu.Lock()
	o, ok := c.occupants[req.PID]
	if ok && o.Token != req.Token {
		c.mu.Unlock()
		writeJSON(w, map[string]any{"ok": false, "reason": "taken"})
		return
	}
	delete(c.occupants, req.PID)
	empty := len(c.occupants) == 0
	c.mu.Unlock()

	writeJSON(w, map[string]any{"ok": true})
	if empty {
		// 最后一个使用者退出 → 无需等下一轮巡检，立即自停（§12.5）。
		c.fireOnEmpty()
	}
}

// handleOccupants 返回占用 PID 列表，供 probe / 调试（§12.3 规定响应体为 {"pids":[...]}）。
func (c *Coordinator) handleOccupants(w http.ResponseWriter, _ *http.Request) {
	c.mu.Lock()
	pids := make([]int, 0, len(c.occupants))
	for pid := range c.occupants {
		pids = append(pids, pid)
	}
	c.mu.Unlock()
	// 排序让 probe 输出稳定（map 迭代顺序随机，不利于断言/比对）。
	sort.Ints(pids)
	writeJSON(w, map[string]any{"pids": pids})
}

// ---- engine 侧：Client ----

// Client 是 gear 连向 sidecar 协调服务的客户端。
type Client struct {
	baseURL string
	ttl     time.Duration
	token   string
	http    *http.Client
	pid     int
}

// NewClient 构造协调客户端。listen 为 sidecar 的 coord 地址（如 127.0.0.1:8089），
// ttl 为占用有效期 / 心跳周期的基准（默认 15s；心跳周期取 ttl/2，须 <= 服务端 TTL 以免被误剔除）。
//
// token 每次构造随机生成（§12.3）：随 PID 一起登记，注销/心跳必须 PID+token 匹配，
// 从而把「某个 PID 的占用」绑定到「这个具体的 gear 进程实例」，防止 PID 复用造成幽灵占用（§12.6）。
func NewClient(listen string, ttl time.Duration) (*Client, error) {
	if listen == "" {
		return nil, fmt.Errorf("coord: NewClient 需要非空的 listen 地址")
	}
	if ttl <= 0 {
		ttl = 15 * time.Second
	}
	tok, err := newToken()
	if err != nil {
		return nil, fmt.Errorf("coord: 生成注册 token 失败: %w", err)
	}
	return &Client{
		baseURL: "http://" + listen,
		ttl:     ttl,
		token:   tok,
		// 显式关掉代理：协调端点只在本机 localhost（§12.6），而 http.DefaultTransport
		// 会走 ProxyFromEnvironment —— 若用户环境设了 HTTP_PROXY，127.0.0.1 的请求会被
		// 代理劫持成 502/连接失败，导致「注册失败 → 反复重拉 sidecar」这种假故障。
		http: &http.Client{
			Timeout:   5 * time.Second,
			Transport: &http.Transport{Proxy: nil},
		},
		pid: os.Getpid(),
	}, nil
}

// RegisterOrStart 实现 §12.4 的「注册优先 / 竞态退让」握手，并返回一个持有占用、
// 自动心跳、可注销的 Session。
//
//  1. 先尝试向已有 sidecar 注册自身 PID —— 成功说明别人是 owner，本 gear 退让为 client；
//  2. 失败（端口无人监听）→ 本 gear 当 owner，调用 start() 拉起 sidecar；
//  3. 注册自身；若仍失败，说明本 gear 的 sidecar 在竞态中输掉端口（速退 ExitAddrInUse），
//     重试注册到胜者 sidecar（一轮重试收敛，无 EADDRINUSE 报错）。
//
// owner 的 Session.Proc() 返回真实 sidecar 进程；client 返回 no-op 进程（共享实例，不可本地杀）。
func (c *Client) RegisterOrStart(start func() (Process, error)) (*Session, error) {
	owner, proc, err := c.handshake(start)
	if err != nil {
		return nil, err
	}
	return c.newSession(owner, proc, start), nil
}

// handshake 执行 §12.4 的握手，返回「本 gear 是否为 owner」与对应的进程句柄。
func (c *Client) handshake(start func() (Process, error)) (bool, Process, error) {
	// 步骤 1：向 listen 端口注册。连接成功且 ok → 已有实例，直接复用。
	ok, rerr := c.register()
	if rerr == nil {
		if ok {
			return false, noopProcess{}, nil
		}
		// ok:false/taken —— 端口上确有实例，但本 PID 已被另一个进程身份占用（PID 复用）。
		// 退避重试直到旧占用因无心跳超时被剔除（§12.6）。
		if e := c.registerUntilOK(takenRetryFactor * c.ttl); e != nil {
			return false, nil, fmt.Errorf("coord: register 持续被拒(taken): %w", e)
		}
		return false, noopProcess{}, nil
	}

	// 步骤 2：连接被拒（无人监听）→ 本 gear 是首到者 / owner，拉起 sidecar。
	proc, serr := start()
	if serr != nil {
		return false, nil, fmt.Errorf("coord: 启动 sidecar 失败: %w", serr)
	}

	// 带退避重试 register，直到 sidecar 绑好端口（成功即 owner 成立）。
	if c.registerWithinBudget(startupBudget) {
		return true, proc, nil
	}

	// 步骤 3：sidecar 就绪不了，或它在竞态中输了端口（速退）→ 退让为 client，
	// 重试注册到「胜者」实例（§12.4 竞态收敛：最多一轮重试）。
	if e := c.registerUntilOK(takeoverBudget); e != nil {
		_ = proc.Kill()
		_ = proc.Wait()
		return false, nil, fmt.Errorf("coord: 拉起 sidecar 后注册失败（竞态未收敛）: %w", e)
	}
	return false, noopProcess{}, nil
}

// Session 是一次「注册 → 持有」的会话，绑定一个 service ext 的协调生命周期。
type Session struct {
	cli   *Client
	pid   int
	start func() (Process, error)

	mu     sync.Mutex
	owner  bool
	proc   Process
	closed bool
	stop   chan struct{}
}

func (c *Client) newSession(owner bool, proc Process, start func() (Process, error)) *Session {
	if proc == nil {
		proc = noopProcess{}
	}
	s := &Session{cli: c, pid: c.pid, start: start, owner: owner, proc: proc, stop: make(chan struct{})}
	go s.watchdog()
	return s
}

// watchdog 是注册看门狗（§12.4.4）：每 ttl/2 发 /heartbeat 保活。
// 连续失败（阈值 heartbeatFailureThreshold）说明 sidecar 可能已崩溃/重启 →
// 重走握手（必要时成为新 owner），让「sidecar 崩了由某个 gear 重新拉起」这条链路自愈。
func (s *Session) watchdog() {
	interval := s.cli.ttl / 2
	if interval <= 0 {
		interval = time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()

	fails := 0
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			if ok, err := s.cli.heartbeat(); err == nil && ok {
				fails = 0
				continue
			}
			fails++
			if fails < heartbeatFailureThreshold {
				continue
			}
			fails = 0
			s.rehandshake()
		}
	}
}

// Rehandshake 重走 §12.4 握手（可能成为新 owner），并更新本会话持有的进程句柄。
//
// 两个调用方：
//   - 看门狗 watchdog：心跳连续失败后自愈（返回 error 只表示本次没收敛，下一轮继续）；
//   - 引擎 Relaunch：service ext 的「重启」必须走这里，不能拿 Entry 直接 rt.Start ——
//     Entry 带着 COORD_ADDR=<listen>，当端口已被他人 sidecar 占用时会拿到
//     ExitAddrInUse(2) 速退，把「共享复用」变成崩溃循环（2026-09-23 实测）。
//
// 关键防误伤：若本 gear 本是 owner、且**本地 sidecar 进程仍然存活**，则说明刚才的
// 心跳失败只是瞬时故障——握手返回的 client 角色不能采纳，否则会把一个健康的共享实例
// 降级并杀掉，影响其它已注册的 gear。只有确认本地 sidecar 已死才降级为 client。
func (s *Session) Rehandshake() error {
	owner, proc, err := s.cli.handshake(s.start)
	if err != nil {
		return err // 下一轮心跳继续尝试；不放弃也不 panic
	}

	s.mu.Lock()
	wasOwner, old := s.owner, s.proc
	if wasOwner && !owner && old != nil && processAlive(old.Pid()) {
		s.mu.Unlock()
		// 本地 sidecar 仍活着：保留 owner 角色，不降级、不误杀（若 proc 是新建的
		// 句柄则丢弃之——它对应的进程在握手中并未被使用）。
		if proc != nil && proc.Pid() != 0 && proc.Pid() != old.Pid() {
			_ = proc.Kill()
			_ = proc.Wait()
		}
		return nil
	}
	s.owner, s.proc = owner, proc
	if s.proc == nil {
		s.proc = noopProcess{}
	}
	s.mu.Unlock()

	// 角色由 owner 退让为 client：本地那个（已死的）sidecar 句柄回收掉。
	if wasOwner && !owner && old != nil {
		_ = old.Kill()
		_ = old.Wait()
	}
	return nil
}

// rehandshake 是看门狗用的包装：重握手失败不留错误（下一轮心跳继续尝试）。
func (s *Session) rehandshake() {
	_ = s.Rehandshake()
}

// Proc 返回该 session 对应的 service 进程句柄：
// owner 为真实 sidecar 子进程，client 为 no-op（共享实例，不可本地杀）。
func (s *Session) Proc() Process {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.proc
}

// IsOwner 报告本 gear 是否持有本地 sidecar 进程（owner）。
//
// client（退让复用他人实例）返回 false —— 此时手上没有本地进程，
// 监管器不得把「无本地进程」当成「service 已退出」（见 engine.ServiceHandle.WaitExit）。
func (s *Session) IsOwner() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.owner
}

// Close 退订并清理：停看门狗 → /deregister（占用列表空则侧车自动退出）→
// owner 额外杀掉自己拉起的 sidecar。幂等，可重复调用。
// 供 Supervisor.stopAll 在杀进程后调用（见 §12.6 优雅退出）。
func (s *Session) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	owner, proc := s.owner, s.proc
	s.mu.Unlock()

	close(s.stop)
	_ = s.cli.deregister()
	if owner && proc != nil {
		_ = proc.Kill()
		_ = proc.Wait()
	}
	return nil
}

// ---- 内部 HTTP 辅助 ----

// coordResp 是协调端点统一的响应体形态（§12.3）。
type coordResp struct {
	OK     bool   `json:"ok"`
	Reason string `json:"reason"`
}

// postJSON 向协调端点发一个 JSON 请求并解析响应。
// 返回 error 仅代表**传输层/协议层失败**（连不上、HTTP 非 2xx、响应不可解析）；
// 业务层的拒绝（ok:false）通过 coordResp.OK 表达，不算 error——两者语义不同，调用方分别处理。
func (c *Client) postJSON(path string, body any, out *coordResp) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, c.baseURL+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("coord: %s 返回 HTTP %d", path, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// register 发一次 /register，返回 (是否登记成功, 传输层错误)。
// ok=false 且 err=nil 即 §12.3 的 taken（PID 已被别的进程身份占用）。
func (c *Client) register() (bool, error) {
	body := map[string]any{"pid": c.pid, "token": c.token, "ttl": int(c.ttl.Seconds())}
	var resp coordResp
	if err := c.postJSON("/register", body, &resp); err != nil {
		return false, err
	}
	return resp.OK, nil
}

// heartbeat 发一次 /heartbeat，返回 (是否续期成功, 传输层错误)。
func (c *Client) heartbeat() (bool, error) {
	body := map[string]any{"pid": c.pid, "token": c.token}
	var resp coordResp
	if err := c.postJSON("/heartbeat", body, &resp); err != nil {
		return false, err
	}
	return resp.OK, nil
}

// deregister 发一次 /deregister（best-effort：调用方通常在退出路径上，失败不阻塞）。
func (c *Client) deregister() error {
	body := map[string]any{"pid": c.pid, "token": c.token}
	var resp coordResp
	return c.postJSON("/deregister", body, &resp)
}

// registerWithinBudget 在预算内带退避重试注册，用于「owner 等 sidecar 就绪」。
// 任意一次成功即返回 true；预算耗尽返回 false。
func (c *Client) registerWithinBudget(budget time.Duration) bool {
	deadline := time.Now().Add(budget)
	for i := 0; time.Now().Before(deadline); i++ {
		time.Sleep(backoffFor(i))
		if ok, err := c.register(); err == nil && ok {
			return true
		}
	}
	return false
}

// registerUntilOK 在预算内**持续重试直到登记成功**（taken / 连接失败都视为可重试）。
// 用于两处：① step-1 拿到 taken 时等旧占用超时；② 竞态退让后重试注册到胜者实例。
func (c *Client) registerUntilOK(budget time.Duration) error {
	deadline := time.Now().Add(budget)
	var last error = fmt.Errorf("coord: 注册重试预算耗尽（%s）", budget)
	for i := 0; time.Now().Before(deadline); i++ {
		time.Sleep(backoffFor(i))
		ok, err := c.register()
		switch {
		case err == nil && ok:
			return nil
		case err != nil:
			last = err
		default:
			last = fmt.Errorf("coord: register 被拒(taken)")
		}
	}
	return last
}

// backoffFor 返回第 i 次重试前的退避时长：起步 100ms、上限 500ms。
// 上限压得较低是为了让「sidecar 就绪」这类常见等待尽快收敛（首轮 100ms 即可命中）。
func backoffFor(i int) time.Duration {
	d := time.Duration(100*(i+1)) * time.Millisecond
	if d > 500*time.Millisecond {
		d = 500 * time.Millisecond
	}
	return d
}

// newToken 生成 16 字节随机 token 的十六进制串（§12.3 的 "<uuid>"；此处用等强度随机串）。
func newToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// isAddrInUseErr 跨平台判断「地址已占用」错误。
//
// 三层判定（顺序即优先级）：
//  1. 字符串兜底（**大小写不敏感**：Windows 的原文是 "Only one usage of each socket address…"，
//     首字母大写；Linux 是 "address already in use"）；
//  2. 直接是 syscall.Errno == EADDRINUSE；
//  3. 包在 *net.OpError 里的 syscall.Errno == EADDRINUSE。
func isAddrInUseErr(err error) bool {
	if err == nil {
		return false
	}
	lower := strings.ToLower(err.Error())
	if strings.Contains(lower, "address already in use") ||
		strings.Contains(lower, "only one usage of each socket address") {
		return true
	}
	var se syscall.Errno
	if errors.As(err, &se) && se == syscall.EADDRINUSE {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		var se2 syscall.Errno
		if errors.As(opErr.Err, &se2) && se2 == syscall.EADDRINUSE {
			return true
		}
	}
	return false
}
