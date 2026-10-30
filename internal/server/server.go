// Package server 实现 Gear 的 REST 执行面（PRD §11 最小集）。
//
// 设计取舍：
//   - 用标准库 net/http + Go 1.22+ 的「方法 + 路径变量」路由，**不引 web 框架**：
//     内核要能单二进制分发、依赖最小（与方法论 §5 的取向一致）。
//   - 执行是**异步**的：POST 立即返回 202 + execution id，结果经 GET 查询——
//     这与 PRD §11「触发执行 / 查询执行状态」两个端点的分工一致。
//   - 内存态：ExecStore / AppStore 都是进程内状态（持久性见各自注释的显式声明）。
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gear/internal/app"
	"gear/internal/cap"
	"gear/internal/engine"
	"gear/internal/gearhome"
	"gear/internal/gearlog"
	"gear/internal/httpsafe"
	"gear/internal/model"
	"gear/internal/registry"
)

// AppInstance 是一个由 REST 层托管的受管 Flow 进程（对应 CLI 的 `app run`）。
type AppInstance struct {
	ID        string    `json:"id"`
	FlowID    string    `json:"flow_id"`
	Restart   string    `json:"restart"`
	StartedAt time.Time `json:"started_at"`
	// OpenURLs 是该 Flow 收尾步（如 web_ui.done）带回的「打开地址」，由引擎在跑 once 步时收集
	// （见 engine.OpenURLs）。前端据此把可点击链接直接呈现，用户启动后即可一键跳转。
	// 与 CLI `app start` 走日志扫描 GEAR_OPEN_URL= 是两条互补通道（REST 监管是进程内、无日志文件）。
	OpenURLs []string `json:"open_urls"`

	cancel context.CancelFunc
	// done 在该 app 的监管 goroutine 真正退出时关闭。
	//
	// 为什么需要它：`cancel()` 只是「发出停止信号」，stopAll（杀进程树）在 goroutine 里
	// 异步执行。此前 DELETE /api/apps/{id} 一 cancel 就回 200 stopped，
	// 于是「端口还没释放 / 进程还没死」被当成成功上报——与 `gear app stop`
	// 「收不干净如实报 ORPHAN_APP」是两套口径（2026-10-29 审出）。
	// 有了 done，停应用可以等到真收敛，等不到就如实说等不到。
	done chan struct{}
}

// Server 是 REST 执行面。
type Server struct {
	reg    *registry.Registry
	execs  *ExecStore
	engine *engine.Engine
	mux    *http.ServeMux

	// uiHandler 托管编译进二进制的 Web 管理界面（REQ-UI-LAUNCH-*）。
	// 非 nil 时在 / 与 /assets/* 注册静态路由；nil 表示 --no-ui（不托管 UI）。
	uiHandler http.Handler

	// secrets 是密钥源（REQ-API-SECRETS，2026-10-02 改为持久化到 secrets.db）：管理面经
	// REST /api/secrets 或 CLI `gear secret` 写入 secrets.db，启动期经 GEAR_SECRET_* 环境变量
	// 播种（仅内存）；注入引擎后运行期只读（ctx.secret.get）。
	secrets *cap.SecretStore
	// secretsPath 是 secrets.db 落点（默认 gearhome.SecretsDBPath）；CLI 经 WithSecretsDBPath
	// 注入，供测试隔离，避免读写真实 <GEAR_HOME>/secrets.db。
	secretsPath string

	appMu  sync.Mutex
	apps   map[string]*AppInstance
	appSeq []string
	appWG  sync.WaitGroup // 受管 app 的 goroutine 计数（Shutdown 需等其收敛）

	// registryPath / appsDir 是**落盘台账**（`gear app start` 那套）的定位依据，
	// 供 POST /api/apps/prune 使用。构造期由 CLI 经 WithRegistryPath 注入。
	//
	// 为什么要显式注入而不是各自取默认值：默认值（gearhome）只是「没配时的兜底」，
	// 而 `gear serve --registry /x` 起服时真正在用的是 /x——若 prune 内部再取一次默认值，
	// 就会去清**另一个库的**台账（清错对象比清不掉更难排查）。同一教训见 mcp.Server.registryPath。
	registryPath string
	appsDir      string

	// gate 是执行入口的全局并发闸门（REQ-CONCURRENCY-CROSS / #73）：
	// 固定 worker 池 = 最大并发执行数，超额排队、再超额在入口拒绝（回 429，背压）。
	// 区别于 S4 step 级 maxParallel（限单个 Flow 内 step 并发）与 S2 app run --max（限 Flow 数）。
	gate          *ConcurrencyGate
	maxConcurrent int // worker 数（并发执行上限）；0 表示取默认
	execQueueSize int // 待执行队列长度；0 表示取默认

	// apiIdx 缓存声明式 REST 路由的解析索引（FR-3）：按 registry 的 Tool 表指纹失效，
	// 因此运行期新建 / 改声明的 Tool 不必重启就生效。零值时按「未缓存」使用。
	apiIdx apiIndex

	// onListen 在**监听绑定成功、开始 Serve 之前**回调真实地址。
	//
	// 为什么需要它：addr=":0" 时真实端口只有内核知道，而 ListenAndServe 是阻塞的、
	// 调用方（CLI）在它返回前无法打印任何东西。没有这个钩子，「我实际在哪个端口」这件事
	// 对调用方完全不可见——而那正是长驻子进程要向 Flow 上报的东西。
	onListen func(realAddr string)

	// liveCtx / liveCancel 是所有「在途执行」的父上下文（停机时先取消它）。
	//
	// 为什么必须有：此前 handleCreateExecution 提交给闸门的 job 用 context.Background()，
	// 于是停机只能干等它跑完——一个声明 900s 的 Tool 能把停机整整拖 900s，
	// 而「停不掉」呈现出来就是进程卡死（2026-10-29 审出）。
	liveCtx    context.Context
	liveCancel context.CancelFunc

	// httpSrv 是本进程真正在跑的 http.Server（Serve 时赋值）。
	// 优雅停机要先停监听（有界 Shutdown），超时再强关（Close）——
	// 否则一条挂着的 SSE 长连接就能让停机永久等待。
	httpMu  sync.Mutex
	httpSrv *http.Server

	// shutdownTimeout 是等待「在途执行 / 受管 app」收敛的上限。
	// 超时**不再等待**，但会把未收敛清单打印出来并作为 error 返回——
	// 静默跳过比多等一会儿坏得多：用户会以为停干净了，实际子进程还在跑。
	shutdownTimeout time.Duration
}

// ServerOption 调整 Server 的构造期配置（目前仅并发闸门相关）。
type ServerOption func(*Server)

// WithMaxConcurrent 设执行入口「同时跑的 Flow 执行数」上限（即 gate worker 数）。
// 影响的是跨 Flow 的全局并发，不是单 Flow 内 step 并发（那是 S4 maxParallel）。
func WithMaxConcurrent(n int) ServerOption { return func(s *Server) { s.maxConcurrent = n } }

// WithShutdownTimeout 设停机时等待在途执行 / 受管 app 收敛的上限。
// 超时不再等待，但未收敛清单会打印出来并由 Shutdown 作为 error 返回（绝不静默）。
// 主要用于测试注入短时限；生产用默认值（defaultShutdownTimeout）。
func WithShutdownTimeout(d time.Duration) ServerOption {
	return func(s *Server) { s.shutdownTimeout = d }
}

// WithExecQueueSize 设执行入口的有界排队队列长度：worker 全忙时进入排队，
// 队列满再提交即被拒绝（回 429）。0 表示不排队、满即拒。
func WithExecQueueSize(n int) ServerOption { return func(s *Server) { s.execQueueSize = n } }

// WithRegistryPath 告诉 Server「本进程实际在用的 registry 路径」。
//
// 只有需要触碰**落盘台账**的端点（POST /api/apps/prune）才用得到它；
// 不传则回退到 gearhome 默认值。CLI 起服必须显式传——否则 `gear serve --registry /x`
// 下的 prune 会去清默认库的台账。
func WithRegistryPath(p string) ServerOption { return func(s *Server) { s.registryPath = p } }

// WithAppsDir 显式指定应用台账目录（覆盖 <registry 同目录>/apps 的推导）。
func WithAppsDir(d string) ServerOption { return func(s *Server) { s.appsDir = d } }

// WithSecretsDBPath 显式指定密钥库（secret 原语）落点（覆盖 <gearhome>/secrets.db 的推导）。
// CLI `gear serve` 默认用 gearhome；测试注入临时库以保证密闭，避免污染真实密钥库。
func WithSecretsDBPath(p string) ServerOption { return func(s *Server) { s.secretsPath = p } }

// Close 释放 Server 持有的外部资源（目前仅 secrets.db 连接）；应在进程退出/测试清理时调用，
// 否则 SQLite 连接持有的文件锁会让临时目录无法删除（Windows 上表现为 cleanup 失败）。
func (s *Server) Close() error {
	if s.secrets != nil {
		return s.secrets.Close()
	}
	return nil
}

// WithOnListen 注册「监听绑定成功」回调，参数是**真实**绑定地址（含内核分配的端口号）。
// 回调在 Serve 之前同步执行，故可安全地在里面打印（CLI 用它把真实端口吐给父进程解析）。
func WithOnListen(h func(realAddr string)) ServerOption { return func(s *Server) { s.onListen = h } }

// WithUI 启用内嵌 Web 管理界面托管（REQ-UI-LAUNCH-SERVE）。
// h 非 nil 时在 / 注册同源静态路由；传 nil 表示 --no-ui（不托管 UI，/ 返回 404 指引）。
func WithUI(h http.Handler) ServerOption { return func(s *Server) { s.uiHandler = h } }

const (
	// defaultShutdownTimeout 是停机时等待在途执行与受管 app 收敛的上限。
	//
	// 为什么必须有界：此前 Shutdown 是 `gate.Stop(); appWG.Wait()` 两处**无限等待**——
	// 只要有一个长跑 Flow 或一个卡住的监管 goroutine，停机就永不返回，
	// 呈现出来就是「Ctrl+C 之后进程卡住」（2026-10-29 审出）。有界之后，
	// 最坏情况是「等 20s 后打印未收敛清单并退出」，而不是无限期挂起。
	defaultShutdownTimeout = 20 * time.Second

	// httpShutdownGrace 是 http.Server 优雅停机的等待上限：先停止接受新连接并等
	// 活动请求收尾，超时则强关（Close）。SSE 流式端点可能长期持连接，
	// 没有这个上限，一条挂着的长连接就能把停机拖死。
	httpShutdownGrace = 5 * time.Second
)

// New 构造 Server（不启动监听）。支持构造期选项（如 WithMaxConcurrent /
// WithExecQueueSize 调并发闸门，REQ-CONCURRENCY-CROSS / #73）。
// 扩展原语经 extensions.yaml 配置化加载（含内建 ext.registry；无配置回退默认 ext.ai）；
// 配置非法时返回 error（fail-closed，不让引擎带残缺扩展目录起来）。
func New(reg *registry.Registry, opts ...ServerOption) (*Server, error) {
	const (
		defaultMaxConcurrent = 64  // 全局部署默认：单机能同时跑 64 个 Flow 执行
		defaultQueueSize     = 256 // 排队缓冲，避免瞬时抖动直接 429
	)
	s := &Server{
		reg:             reg,
		execs:           NewExecStore(),
		mux:             http.NewServeMux(),
		apps:            map[string]*AppInstance{},
		maxConcurrent:   defaultMaxConcurrent,
		execQueueSize:   defaultQueueSize,
		shutdownTimeout: defaultShutdownTimeout,
	}
	for _, o := range opts {
		o(s)
	}
	if s.shutdownTimeout <= 0 {
		s.shutdownTimeout = defaultShutdownTimeout
	}
	// 在途执行的父上下文：停机时先取消它，闸门才有机会快速收敛（见 liveCtx 的注释）。
	s.liveCtx, s.liveCancel = context.WithCancel(context.Background())
	// 密钥源（REQ-API-SECRETS）：启动期先从 GEAR_SECRET_* 环境变量播种部署凭据，
	// 再注入引擎——声明了 secret capability 的 Tool 运行期 ctx.secret.get 可用，
	// ${secrets.x} 模板可解析；管理面写入经 /api/secrets，运行期只读。
	// 播种失败即 fail-closed 返回 error：GEAR_SECRET_ 前缀下的非法名字（空名/含空白/${}）
	// 会让该密钥**静默缺失**——运维「配了却读不到」且无任何提示，是最难排查的一类故障。
	// cap.SecretStore.SeedFromEnv 刻意返回 error 就是为了这个，此处不得丢弃。
	//
	// 顺序要求：必须排在并发闸门**之前**——闸门的 worker goroutine 在构造期就已启动，
	// 之后再返回 error 会留下永不退出的 goroutine（NewConcurrencyGate 无人 Shutdown）。
	// secrets 密钥源（REQ-API-SECRETS，2026-10-02 改为持久化到 secrets.db）：
	// Open 先把库里全部密钥载入内存，再 SeedFromEnv 用环境变量覆盖同名项（环境是部署期权威）。
	if s.secretsPath == "" {
		s.secretsPath = gearhome.SecretsDBPath()
	}
	s.secrets = cap.NewSecretStore()
	if err := s.secrets.Open(s.secretsPath); err != nil {
		return nil, fmt.Errorf("打开密钥库失败: %w", err)
	}
	if err := s.secrets.SeedFromEnv(); err != nil {
		return nil, fmt.Errorf("播种环境变量密钥失败: %w", err)
	}
	// 并发闸门（#73）：执行入口的「进程池」。worker 数 = 最大并发执行数，
	// 待执行任务入有界队列，队列满且 worker 全忙时 Submit 返回 false（入口拒绝、回 429）。
	s.gate = NewConcurrencyGate(s.maxConcurrent, s.execQueueSize)
	// 日志采集（REQ-API-LOGS / #71）：引擎执行 step 时把 stdout/stderr 行经 ExecStore 落盘，
	// 供 GET /api/executions/{id}/logs 查询。
	// 扩展原语目录（PRD §9 去内置化）：配置化加载（extensions.yaml，GEAR_EXT_CONFIG 可覆盖），
	// 始终含内建 ext.registry（读引擎 registry，供 agent 自我锚定）；无配置回退默认 ext.ai。
	// 使声明了 capabilities:[ext.ai] 的 Tool 在 REST 执行面也能调用 AI 能力；sidecar 二进制
	// 缺失时调用返回 ErrCapabilityUnavailable（fail-closed），不影响其它能力。
	capReg := engine.NewCapabilityRegistry()
	agent, err := engine.SetupExtensions(capReg, reg, "")
	if err != nil {
		return nil, fmt.Errorf("初始化扩展失败: %w", err)
	}
	s.engine = engine.New(reg, engine.WithLogSink(s.execs.LogSink()), engine.WithCapabilities(capReg), engine.WithAIAgent(agent), engine.WithSecret(s.secrets),
		// 配置库（config 原语 + AI 缺省配置）：与 CLI 同一个库（gearhome.ConfigDBPath()）。
		engine.WithConfigStore(engine.NewDefaultConfigStore()))
	// §8.2 第三条守卫需要「运行期引用」，而运行期状态在 Server 手里 —— 由这里注入，
	// registry 不反向依赖执行状态。
	reg.SetRunningChecker(s.runningToolReason)
	s.routes()
	return s, nil
}

// runningToolReason 报告 toolID 是否正被运行中的执行使用（PRD §8.2 第三条守卫）。
//
// 为什么 Registry 的静态引用守卫不够：Hook 被删除、或 Flow 被重新注册去掉该 step 之后，
// Registry 已看不到任何引用，但**在跑的执行**仍持有启动时的快照 —— 此时注销会让它
// 在下次触发 Hook / 执行 step 时拿到 nil Tool 而中途失败。
func (s *Server) runningToolReason(toolID string) string {
	if id, ok := s.execs.RunningToolIDs()[toolID]; ok {
		return fmt.Sprintf("正被运行中的执行 %s 使用", id)
	}
	return ""
}

// Handler 返回可供 httptest / 反向代理使用的 handler。
func (s *Server) Handler() http.Handler { return s.mux }

// serverBaseURL 计算 gear 自身回推基址（供 subscribe 机制拼回推端点 /api/events/{handle}）。
// addr 是**监听绑定后的真实地址**（realAddr）：":port" 补 127.0.0.1，空回退 127.0.0.1:8787，
// 已带 scheme 前缀则原样。
//
// 必须传真实地址、不能传用户给的 addr：addr=":0"（自动分配）时 user 给的还是 ":0"，
// 拼出的回推基址是 http://127.0.0.1:0 —— subscribe 会往一个死端口回推。
func serverBaseURL(realAddr string) string {
	if strings.HasPrefix(realAddr, "http://") || strings.HasPrefix(realAddr, "https://") {
		return strings.TrimRight(realAddr, "/")
	}
	if realAddr == "" {
		realAddr = "127.0.0.1:8787"
	}
	if strings.HasPrefix(realAddr, ":") {
		realAddr = "127.0.0.1" + realAddr
	}
	return "http://" + realAddr
}

// realAddrOf 从 net.Listener 解析出「host:port」真实绑定地址（错误一律向上抛，绝不静默回退）。
func realAddrOf(ln net.Listener) (string, error) {
	switch addr := ln.Addr().(type) {
	case *net.TCPAddr:
		host := addr.IP.String()
		if host == "0.0.0.0" || host == "::" {
			host = "127.0.0.1"
		}
		return net.JoinHostPort(host, strconv.Itoa(addr.Port)), nil
	case *net.UDPAddr:
		return ln.Addr().String(), nil
	default:
		return "", fmt.Errorf("未知 listener 地址类型 %T，无法解析真实端口", ln.Addr())
	}
}

// ListenAndServe 启动 HTTP 服务（阻塞）。
//
// 用 net.Listen 显式建监听、再 srv.Serve(ln)，而非 http.Server.ListenAndServe(addr)：
// 后者在 addr=":0" 时内核分配的真实端口无从回读，调用方（process runtime 的长驻子进程
// 上报、CLI banner）拿到的就是 ":0"。端口由**内核**分配（不是自己从基址逐个试）就没有
// TOCTOU 窗口——试成功→释放→被别人抢，两个实例会挑到同一个口，且都报成功。
func (s *Server) ListenAndServe(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	realAddr, err := realAddrOf(ln)
	if err != nil {
		_ = ln.Close()
		return err
	}
	// subscribe 机制的回推回调 URL 由 gear 自身**真实**监听地址派生（每个 gear 实例独立），
	// 而非经 env 传给 sidecar（多 gear 并行时单一 env 会串扰）。
	s.engine.SetSubscribeCallbackBase(serverBaseURL(realAddr))
	if s.onListen != nil {
		s.onListen(realAddr)
	}
	gearlog.L().Info("REST 执行面监听", slog.String("addr", realAddr))
	srv := &http.Server{
		Handler:           s.mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	// 记住 http.Server：优雅停机（Server.Shutdown）要先停监听，否则停不掉新连接、
	// 也没法让 Serve 立刻返回——调用方（CLI 的信号处理）正等这个返回。
	s.httpMu.Lock()
	s.httpSrv = srv
	s.httpMu.Unlock()
	serveErr := srv.Serve(ln)
	// 停机与崩溃必须分开记：ErrServerClosed 是主动关停（INFO），其余是异常退出
	// （ERROR）——「服务停了」和「服务崩了」在事后回看时是两件事，混为一谈
	// 会让「昨晚为什么没数据」查错方向。
	if errors.Is(serveErr, http.ErrServerClosed) {
		gearlog.L().Info("REST 执行面已停止", slog.String("addr", realAddr))
	} else {
		gearlog.L().Error("REST 执行面异常退出", slog.String("addr", realAddr), slog.String("err", fmt.Sprint(serveErr)))
	}
	return serveErr
}

// Shutdown 停止监听、取消在途执行、停止所有受管 app，并**有界等待其收敛**。
//
// 必须先等 goroutine 退出再让调用方关闭 registry：否则 Supervisor 可能还在读库，
// 导致 `sql: database is closed` 这类停机竞态。
//
// 等待**有界**（shutdownTimeout，默认 defaultShutdownTimeout），超时不再等：
// 此前 gate.Stop() 与 appWG.Wait() 都是无限等待，一个长跑 Flow 或一个卡在启动阶段的
// 监管 goroutine 就能让停机永不返回——用户看到的是「Ctrl+C 之后进程卡住」
// （2026-10-29 审出）。有界之后最坏情况是「等满超时 → 打印未收敛清单 → 退出」，
// 而不是无限期挂起。
//
// 返回值：全部收敛为 nil；有未收敛项时返回描述它们的 error（已打印，交给调用方决定
// 是否当作失败）。**绝不因为没收敛就跳过收尾**——execs / secrets 句柄照常释放。
func (s *Server) Shutdown() error {
	// ① 停监听：不再接受新请求。有界等待活动请求收尾，超时强关——
	// 一条挂着的 SSE 长连接不该决定整个进程的生死。
	s.stopHTTP()

	// ② 取消在途执行：让长跑 Flow 自己中断（子进程随 ctx 被杀），闸门才有机会收敛。
	if s.liveCancel != nil {
		s.liveCancel()
	}

	// ③ 取消受管 app 的监管 ctx，并留一份引用用于事后点名「谁没停干净」。
	s.appMu.Lock()
	insts := make([]*AppInstance, 0, len(s.apps))
	for _, id := range s.appSeq {
		if a := s.apps[id]; a != nil {
			insts = append(insts, a)
		}
	}
	for _, a := range insts {
		a.cancel()
	}
	s.apps = map[string]*AppInstance{}
	s.appSeq = nil
	s.appMu.Unlock()

	deadline := time.Now().Add(s.shutdownTimeout)
	var pending []string

	// ④ 收敛执行入口闸门（等 worker / 在途执行退出）。
	if s.gate != nil {
		if !s.gate.StopWithTimeout(time.Until(deadline)) {
			pending = append(pending, s.pendingExecutionSummary())
		}
	}
	// ⑤ 收敛受管 app 的监管 goroutine。
	if !waitWGDeadline(&s.appWG, deadline) {
		for _, a := range insts {
			select {
			case <-a.done:
			default:
				pending = append(pending, fmt.Sprintf("app %s（flow=%s）未收敛", a.ID, a.FlowID))
			}
		}
		if len(pending) == 0 {
			pending = append(pending, "受管 app 未收敛")
		}
	}

	// ⑥ 释放执行记录库句柄（#70 SQLite 持久化）：停机后不再查询，关闭以解锁 WAL 侧车文件。
	_ = s.execs.Close()
	// ⑦ 释放 secrets.db 句柄：本方法常在测试 cleanup / 优雅退出末尾被调用，
	// 若只关 execs 不关 secrets，SQLite 持有的文件锁会让临时目录无法删除（Windows cleanup 失败）。
	_ = s.Close()

	if len(pending) > 0 {
		msg := fmt.Sprintf("停机等待 %s 后仍未收敛: %s", s.shutdownTimeout, strings.Join(pending, "；"))
		fmt.Printf("! %s\n", msg)
		gearlog.L().Warn("REST 执行面停机未完全收敛", slog.String("pending", strings.Join(pending, "；")))
		return errors.New(msg)
	}
	return nil
}

// stopHTTP 停止 HTTP 监听：先有界优雅停机，超时强关。
//
// 为什么必须强兜一层 http.Server.Close()：Shutdown 只保证「不再接受新连接 + 等活动请求
// 结束」，但 SSE 流式端点（/api/capabilities/invoke/stream）可能长期持有连接，
// 且 agent 流式调用此前不接受 ctx（无超时）。只调 Shutdown 的结果就是停机被一条连接拖死。
func (s *Server) stopHTTP() {
	s.httpMu.Lock()
	srv := s.httpSrv
	s.httpMu.Unlock()
	if srv == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), httpShutdownGrace)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		// 超时（或连接收不了尾）：强关，绝不把停机挂在这一步。
		gearlog.L().Warn("HTTP 优雅停机超时，强制关闭连接", slog.String("err", fmt.Sprint(err)))
		_ = srv.Close()
	}
}

// pendingExecutionSummary 汇总「停机时仍未落终态」的执行，供未收敛清单点名。
func (s *Server) pendingExecutionSummary() string {
	var running []string
	for _, e := range s.execs.List() {
		if e.Status == "running" {
			running = append(running, e.ID)
		}
	}
	if len(running) == 0 {
		return "在途执行未收敛"
	}
	sort.Strings(running)
	if len(running) > 5 {
		return fmt.Sprintf("在途执行未收敛（%d 条，前 5: %s）", len(running), strings.Join(running[:5], ", "))
	}
	return fmt.Sprintf("在途执行未收敛: %s", strings.Join(running, ", "))
}

// waitWGDeadline 有界等待一组 goroutine 结束；到 deadline 仍未结束返回 false。
//
// 超时后等待用的那一个 goroutine 仍在 wg.Wait() 上——每次调用至多一个，
// 且此刻进程正要退出，不像「每轮泄漏 N 个」那样会随时间累积。
func waitWGDeadline(wg *sync.WaitGroup, deadline time.Time) bool {
	done := make(chan struct{})
	go func() {
		defer close(done)
		wg.Wait()
	}()
	d := time.Until(deadline)
	if d <= 0 {
		d = 0
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-done:
		return true
	case <-t.C:
		return false
	}
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /api/healthz", s.handleHealthz)
	s.mux.HandleFunc("GET /api/news", s.handleNews)
	s.mux.HandleFunc("GET /api/capabilities", s.handleCapabilities)
	s.mux.HandleFunc("POST /api/capabilities/invoke", s.handleInvokeCapability)
	s.mux.HandleFunc("POST /api/capabilities/invoke/stream", s.handleInvokeCapabilityStream)

	s.mux.HandleFunc("GET /api/secrets", s.handleListSecrets)
	s.mux.HandleFunc("PUT /api/secrets/{name}", s.handleSetSecret)
	s.mux.HandleFunc("DELETE /api/secrets/{name}", s.handleDeleteSecret)

	s.mux.HandleFunc("GET /api/tools", s.handleListTools)
	s.mux.HandleFunc("GET /api/tools/{id}", s.handleGetTool)
	s.mux.HandleFunc("POST /api/tools", s.handleCreateTool)
	s.mux.HandleFunc("DELETE /api/tools/{id}", s.handleDeleteTool)
	s.mux.HandleFunc("POST /api/tools/{id}/run", s.handleRunTool)
	s.mux.HandleFunc("POST /api/tools/{id}/versions/{v}/promote", s.handlePromoteVersion)

	s.mux.HandleFunc("GET /api/flows", s.handleListFlows)
	s.mux.HandleFunc("POST /api/flows", s.handleCreateFlow)
	s.mux.HandleFunc("DELETE /api/flows/{id}", s.handleDeleteFlow)
	s.mux.HandleFunc("POST /api/flows/{id}/validate", s.handleValidateFlow)

	s.mux.HandleFunc("GET /api/hooks", s.handleListHooks)
	s.mux.HandleFunc("POST /api/hooks", s.handleCreateHook)
	s.mux.HandleFunc("DELETE /api/hooks/{id}", s.handleDeleteHook)

	s.mux.HandleFunc("POST /api/executions", s.handleCreateExecution)
	s.mux.HandleFunc("GET /api/executions", s.handleListExecutions)
	s.mux.HandleFunc("GET /api/executions/{id}", s.handleGetExecution)
	s.mux.HandleFunc("GET /api/executions/{id}/logs", s.handleGetExecutionLogs)

	s.mux.HandleFunc("POST /api/events/{handle}", s.handleSubscriptionEvent)

	s.mux.HandleFunc("POST /api/apps", s.handleCreateApp)
	s.mux.HandleFunc("GET /api/apps", s.handleListApps)
	s.mux.HandleFunc("POST /api/apps/prune", s.handlePruneApps)
	s.mux.HandleFunc("DELETE /api/apps/{id}", s.handleDeleteApp)

	// 声明式资源路由（FR-3）：Tool YAML 里写了 `api:` 的就在这几个挂载点上生效，
	// 由 handleToolAPIAny 在**请求期**按 registry 当前的声明匹配到具体 Tool。
	// 用「每方法一条 /api/{path...}」而不是「每条声明一条」：后者会因同名 pattern
	// 二次注册而 panic（net/http 实测行为），「改声明 / 删 Tool / 运行期新建 Tool」
	// 每次都会把服务打挂——挂载点只有一条，就没有「注册即固化」这个坑。
	for _, m := range []string{"GET", "POST", "PUT", "PATCH", "DELETE"} {
		s.mux.HandleFunc(m+" /api/{path...}", s.handleToolAPIAny)
	}

	// Web 管理界面（REQ-UI-LAUNCH-SERVE）：同源托管，浏览器永不跨域 → 内核无需 CORS 头。
	// 路由特异性保证不干扰 /api/*：`/api/{path...}` 比 `/{path...}` 更具体，工具 API 不被吞。
	// 兜底用 `GET /{path...}` 一条即可（它天然匹配根路径 `/`，uiHandler 内部再按 path 区分
	// index.html 与 SPA 兜底）；单独再注册 `GET /` 会与 `/{path...}` 在 `/` 上冲突而 panic。
	if s.uiHandler != nil {
		s.mux.Handle("GET /assets/{path...}", s.uiHandler)
		s.mux.Handle("GET /{path...}", s.uiHandler) // SPA 兜底；/api/* 更具体优先，根路径也由此兜底
	} else {
		// --no-ui：/ 返回明确指引而不是静默 404 空白。
		s.mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, "Web UI 未启用（gear serve --no-ui）。用 `gear serve`（默认带 UI）或 `gear ui` 打开管理界面。\n")
		})
	}
}

// ---- 通用响应helper ----

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// errorBody 是 REST 错误响应的**结构化**形状（2026-10-03，此前只有 `{"error": "一整段文本"}`）。
//
// 为什么改：调用方（前端 / 脚本 / 智能体）此前只能正则剥错误文本，实测里有人写下
// `replace(/^step \S+ 执行失败: lua 执行失败: /,”)` 这种正则——**文本形状一改就崩**。
// 而 MCP 面早就是 `{code, message, hint}`，同一个内核两套错误口径，本身也是漂移。
//
// ⚠ `Error` 字段**保留且与 Message 同值**：既有调用方（`plugin/web-ui` 的 api client、
// `apps/mindstorm/www/api.js`、全部 server 测试）都在读 `error` 字符串，删掉就是破坏性变更。
// 它是兼容位，新代码读 `code` / `message` / `detail`。
type errorBody struct {
	// Error 是**兼容字段**（= Message）。新代码请读 Message。
	Error string `json:"error"`
	// Code 是机器可读错误码；取法与 MCP 面同源（诊断优先，状态兜底，见 restErrorCode）。
	Code string `json:"code"`
	// Message 是给人看的完整主句（栈回溯已拆到 detail）。
	Message string `json:"message"`
	// Detail 放 message 之后的技术细节（目前是 Lua stack traceback）：
	// 让人读的部分与技术噪音分开，前端不必再自己剥。
	// ⚠ 它以原文里那个分隔换行开头——这样 `message + detail` 与旧文本逐字相同（不丢信息）。
	Detail string `json:"detail,omitempty"`
	// Hint 是可据以行动的下一招（不是所有错误都有；不写就不出现，不塞占位）。
	Hint string `json:"hint,omitempty"`
	// TraceID 预留：将来把执行 id / 请求 id 串进来，便于日志对账。现在不生成——
	// 填一个每次调用都不同的假 id 比不填更糟（它会让人以为真能查到东西）。
	TraceID string `json:"trace_id,omitempty"`
}

// restStatusCodes 把 HTTP 状态映射为**兜底**错误码。
//
// 只列真的会出现的状态（发明一整套 RESTful 状态码不是本项目的风格）：
// 有结构化诊断的错误一律用诊断里的真码，这里只是「这条路径没有诊断」时的诚实兜底。
var restStatusCodes = map[int]string{
	http.StatusBadRequest:          engine.CodeRESTBadRequest,
	http.StatusNotFound:            "NOT_FOUND",
	http.StatusMethodNotAllowed:    engine.CodeRESTMethodNotAllowed,
	http.StatusConflict:            engine.CodeRESTConflict,
	http.StatusTooManyRequests:     "EXECUTION_BUSY",
	http.StatusInternalServerError: engine.CodeRESTInternal,
	http.StatusServiceUnavailable:  engine.CodeRESTUnavailable,
}

// restErrorCode 取一个错误的结构化码：**诊断优先**（与 MCP 面 toolErrorFrom 同一条规则），
// 没有诊断才按状态兜底，都没有就明说 UNCLASSIFIED（不假装知道原因）。
func restErrorCode(status int, err error) string {
	if diags := engine.Diagnostics(err); len(diags) > 0 {
		if c := diags[0].Code; c != "" && c != engine.CodeUnclassified {
			return c
		}
	}
	if c, ok := restStatusCodes[status]; ok {
		return c
	}
	return "UNCLASSIFIED"
}

// stackTraceMarker 是 gopher-lua 追加栈回溯的位置（实测输出里的字面量）。
const stackTraceMarker = "\nstack traceback:"

// splitStack 把「主句 + 栈回溯」拆开。
//
// 实测样本（REST 原样返回的一整段）：
//
//	step wms__list 执行失败: lua 执行失败: <string>:5: attempt to call ...
//	stack traceback:
//		[G]: in function 'call'
//
// 栈对排障有用、但对人读的第一句是噪音，故分字段；**不丢弃任何内容**
// ——`message + detail` 与旧文本**逐字相同**（为此 detail 保留了原文里那个分隔换行：
// 把它吃掉会让两个字段拼不回原文，而「不丢信息」是靠可复原来保证的，不是靠嘴说）。
func splitStack(msg string) (message, detail string) {
	if i := strings.Index(msg, stackTraceMarker); i >= 0 {
		return msg[:i], msg[i:]
	}
	return msg, ""
}

// writeErr 写一个结构化错误响应（无 hint）。
func writeErr(w http.ResponseWriter, code int, err error) {
	writeErrHint(w, code, err, "")
}

// writeErrHint 写一个带 hint 的结构化错误响应。
func writeErrHint(w http.ResponseWriter, code int, err error, hint string) {
	msg := err.Error()
	message, detail := splitStack(msg)
	writeJSON(w, code, errorBody{
		Error:   message, // 兼容位：与 Message 同值
		Code:    restErrorCode(code, err),
		Message: message,
		Detail:  detail,
		Hint:    hint,
	})
}

func decodeJSON(r *http.Request, v interface{}) error {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		return fmt.Errorf("请求体不是合法 JSON: %w", err)
	}
	return nil
}

// statusForErr 把 registry 的语义化错误映射为 HTTP 状态码。
func statusForErr(err error) int {
	switch {
	case errors.Is(err, registry.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, registry.ErrInUse):
		return http.StatusConflict
	default:
		return http.StatusBadRequest
	}
}

// ---- handlers ----

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	tools, err := s.reg.ListTools()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	flows, err := s.reg.ListFlows()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status": "ok",
		"tools":  len(tools),
		"flows":  len(flows),
	})
}

// handleCapabilities 返回当前实时「原子能力目录」：内核原语（builtin + pure）+ 已注册 ext.*，
// 每个能力含方法级参数 schema（type/description/required）。人与 web-ui 用同一份
// engine.AIAgent().CapabilitiesMethod() 数据源——即 CLI `gear ai capabilities` 的 REST 入口，
// 直接回答「有哪些原子能力 / 扩展了哪些 / 各能力要什么参数」（用户决策：能力必须可查询、可扩展）。
// 能力目录只读、无副作用，失败仅回 500。
func (s *Server) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	agent := s.engine.AIAgent()
	if agent == nil {
		writeErr(w, http.StatusInternalServerError, fmt.Errorf("扩展目录未初始化"))
		return
	}
	caps, err := agent.CapabilitiesMethod()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if caps == nil {
		caps = []map[string]interface{}{}
	}
	writeJSON(w, http.StatusOK, caps)
}

// invokeCapabilityReq 是 POST /api/capabilities/invoke 的请求体：直接执行一个原子能力的方法。
// args 以「参数名为键」的 map（与 capabilities() 的 schema 对齐）——内置原语经
// builtinParamOrder 重排为位置参数、ext.* 原样作为单 table 传入。
type invokeCapabilityReq struct {
	Name   string                 `json:"name"`
	Method string                 `json:"method"`
	Args   map[string]interface{} `json:"args"`
}

// handleInvokeCapability 直接执行一个原子能力的方法（「能力目录」页调用测试用）。
// 与 react 同权限（全量宿主授权 + Lua 沙箱）：db 为内存态、secret 需内核已注入密钥源、
// http 可访问任意 URL。未知能力名 / 方法在引擎侧 fail-closed 报错，不静默执行。
//
// ⚠ 收紧（2026-09-22）：本端点威力等价于「以宿主机身份执行任意能力」，此前无任何校验，
// 而做带界面的应用必然经 ext.serve 反代把它暴露给浏览器。现在只接受本机来源、且禁调
// secret.*（见 httpsafe.OriginAllowed / blockedInvokeCapability 的论证）。
func (s *Server) handleInvokeCapability(w http.ResponseWriter, r *http.Request) {
	if !httpsafe.OriginAllowed(r) {
		writeErr(w, http.StatusForbidden, fmt.Errorf(
			"拒绝跨站调用：Origin %q 不是本机来源——本端点可直达 secret/db 等宿主能力，只接受本机页面与无 Origin 的命令行客户端",
			r.Header.Get("Origin")))
		return
	}
	var req invokeCapabilityReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.Name == "" || req.Method == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("name 与 method 均不能为空"))
		return
	}
	if blockedInvokeCapability(req.Name) {
		writeErr(w, http.StatusForbidden, fmt.Errorf(
			"能力 %s 不允许经 REST 直调：它是唯一「读到即不可挽回」的能力（密钥明文），"+
				"正当用法全在 Flow 内部（${secrets.NAME} / ctx.secret.get），没有从浏览器手调的场景。"+
				"密钥请用 `gear secret` 管理面或 GEAR_SECRET_* 播种", req.Name))
		return
	}
	res, err := s.engine.InvokeCapability(req.Name, req.Method, req.Args)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"result": res})
}

// handleInvokeCapabilityStream 是 /invoke 的流式版本：目前仅支持 ext.ai.complete_stream，
// 以 SSE 逐增量推送。沿用 /invoke 的同源与禁 secret 约束（httpsafe.OriginAllowed / blockedInvokeCapability）。
func (s *Server) handleInvokeCapabilityStream(w http.ResponseWriter, r *http.Request) {
	if !httpsafe.OriginAllowed(r) {
		writeErr(w, http.StatusForbidden, fmt.Errorf(
			"拒绝跨站调用：Origin %q 不是本机来源——流式端点同样可直达宿主能力，只接受本机来源",
			r.Header.Get("Origin")))
		return
	}
	var req invokeCapabilityReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.Name == "" || req.Method == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("name 与 method 均不能为空"))
		return
	}
	if req.Name != "ext.ai" || req.Method != "complete_stream" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf(
			"流式调用目前仅支持 ext.ai.complete_stream（其他方法请走 POST /api/capabilities/invoke）"))
		return
	}
	if blockedInvokeCapability(req.Name) {
		writeErr(w, http.StatusForbidden, fmt.Errorf("能力 %s 不允许经 REST 直调", req.Name))
		return
	}
	agent := s.engine.AIAgent()
	if agent == nil {
		writeErr(w, http.StatusInternalServerError, fmt.Errorf("扩展未初始化"))
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, fmt.Errorf("当前传输层不支持流式响应"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	send := func(event string, payload interface{}) {
		if event != "" {
			fmt.Fprintf(w, "event: %s\n", event)
		}
		b, _ := json.Marshal(payload)
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
	}

	if err := agent.CompleteStream(req.Args, func(content, reasoning string) error {
		send("", map[string]interface{}{"delta": content, "reasoning": reasoning})
		return nil
	}); err != nil {
		send("error", map[string]string{"error": err.Error()})
		return
	}
	send("", map[string]interface{}{"done": true})
}

// handleRunTool 同步执行单个 Tool 并直接返回输出：`POST /api/tools/{id}/run`。
//
// 存在的理由（2026-09-22 实测缺陷）：MCP 面有 `gear_run(kind=tool)` 同步跑 Tool，而 REST
// 面此前只有**异步**的 `POST /api/executions`（跑 Flow，202 + 轮询）。于是做带界面的应用时，
// 前端要么把 Tool 包成 Flow 再轮询，要么绕过 Tool 直接调 `/api/capabilities/invoke` ——
// 后者把 prompt 工程从 Tool 里漏进前端 JS，等于放弃 Tool 这一层的复用价值。
// 补上这个端点，REST 与 MCP 的执行能力才是对齐的。
//
// 请求体是 Tool 的入参 map（可留空 = 无入参执行，空 body 不算错误：零入参 Tool 很常见）。
// 响应 `{id, output}`。与 MCP 侧同一套分流：先查 registry 判存在性——engine.RunTool 对
// 「不存在」给的是无 sentinel 的纯文本错误，不先分流会把「id 写错」与「Tool 跑挂了」
// 混成同一个 400，而两者修法相反（一个是改 id，一个要看日志）。
func (s *Server) handleRunTool(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("tool id 不能为空"))
		return
	}
	found, err := s.reg.GetTool(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if found == nil {
		writeErr(w, http.StatusNotFound, fmt.Errorf(
			"Tool 不存在: %s（用 GET /api/tools 确认 id，注意应用前缀）", id))
		return
	}
	input := map[string]interface{}{}
	if derr := json.NewDecoder(r.Body).Decode(&input); derr != nil && !errors.Is(derr, io.EOF) {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("入参不是合法 JSON 对象: %w", derr))
		return
	}
	// 身份链路（IDENTITY_CONTEXT_DESIGN.md §3.5）：REST 是**真身份面**，在引擎边界把
	// 调用主体注入执行上下文，此后 Hook 与 ctx.caller 都能看到。
	ctx := engine.WithCaller(r.Context(), callerOf(r))
	out, err := s.engine.RunToolExec(ctx, id, input, "")
	if err != nil {
		// 请求体多包了一层 `{"input":{...}}` 是实测里最常见的一类误用，而它的报错
		// （schema missing properties）看着像「字段没传」——所以在这里点名。
		writeErrHint(w, http.StatusBadRequest, err, wrappedInputHint(found, input))
		return
	}
	if out == nil {
		out = map[string]interface{}{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"id": id, "output": out})
}

// blockedInvokeCapability 判定某能力是否禁止经 REST 直调。
//
// 判据（禁 `secret`）的单一真相源在 `cap.BlockedDirectInvoke`：MCP 面新增 `gear_invoke`
// 时复用**同一份**禁则，避免「同一进程两个执行面安全口径不一致」（评审 I-8）。
func blockedInvokeCapability(name string) bool {
	return cap.BlockedDirectInvoke(name)
}

// handleNews 经引擎同步运行 news.top10 Flow（抓取并整理百度热搜榜 Top10），
// 直接把结果作为 HTTP 响应返回——这是「Flow + Lua 工具直接调用 ctx.http 干活」的对外服务化示例。
// Flow 未注册（如未用 `gear serve --seed examples/news-top10.yaml`）→ 404。
func (s *Server) handleNews(w http.ResponseWriter, r *http.Request) {
	res, err := s.engine.RunFlowResult("news.top10")
	if err != nil {
		writeErr(w, http.StatusNotFound, fmt.Errorf("运行 news.top10 失败（是否用 `gear serve --seed examples/news-top10.yaml` 注册了示例?）: %w", err))
		return
	}
	gen, ok := res.Steps["gen"]
	if !ok {
		writeErr(w, http.StatusInternalServerError, fmt.Errorf("news.top10 缺少 step gen 输出"))
		return
	}
	out, _ := gen["output"].(map[string]interface{})
	if out == nil {
		writeErr(w, http.StatusInternalServerError, fmt.Errorf("step gen 无 output: %v", gen))
		return
	}
	if errMsg, _ := out["error"].(string); errMsg != "" {
		writeErr(w, http.StatusBadGateway, fmt.Errorf("news.fetch 失败: %s", errMsg))
		return
	}
	out["served_at"] = time.Now().UTC().Format(time.RFC3339)
	writeJSON(w, http.StatusOK, out)
}

// handleSubscriptionEvent 是 subscribe 机制的回推端点（mqtt-sidecar 收到消息后 POST 到
// /api/events/{handle}）。按 handle 定位订阅，取出 payload 交引擎投递（run/notify）。
// 这是「回调 URL 不走 sidecar env」的关键：每个 gear 用自己的 --addr 派生回推地址，
// sidecar 只维护 handle -> callbackURL 映射，多 gear 并行互不串扰。
func (s *Server) handleSubscriptionEvent(w http.ResponseWriter, r *http.Request) {
	handle := r.PathValue("handle")
	var body struct {
		Payload interface{} `json:"payload"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.engine.Deliver(handle, body.Payload); err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleListTools(w http.ResponseWriter, r *http.Request) {
	tools, err := s.reg.ListTools()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if tools == nil {
		tools = []*model.Tool{}
	}
	writeJSON(w, http.StatusOK, tools)
}

// handleGetTool 返回单个 Tool 的完整定义（含 versions）。不存在 → 404。
//
// 此前 UI 只能「拉全量列表再本地索引」——那时内核确实没有单查端点。现在补上，
// 供按 id 直取（如执行详情里点某个 tool 想看完整定义）而不必先取全表。
func (s *Server) handleGetTool(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	t, err := s.reg.GetTool(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if t == nil {
		writeErr(w, http.StatusNotFound, fmt.Errorf("Tool 不存在: %s: %w", id, registry.ErrNotFound))
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (s *Server) handleCreateTool(w http.ResponseWriter, r *http.Request) {
	var t model.Tool
	if err := decodeJSON(r, &t); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	// ③ 审计：把调用主体记进 definition_history.actor，否则这条历史查出来是「无主变更」。
	if err := s.reg.UpsertTool(&t, registry.WithActor(subjectOf(r))); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, t)
}

func (s *Server) handleDeleteTool(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.reg.DeleteTool(id, registry.WithActor(subjectOf(r))); err != nil {
		writeErr(w, statusForErr(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"deleted": id})
}

// promoteReq 是 POST /api/tools/{id}/versions/{v}/promote 的请求体（S5 灰度 REST 端点）。
type promoteReq struct {
	Target string `json:"target"`
}

// handlePromoteVersion 晋升某 Tool 版本到目标状态（S5 灰度 REST 端点，对应 CLI `tool promote`）。
// 走 engine.PromoteTool：先触发 before_promote Hook 门禁（fail-closed），再落库；
// 状态迁移合法性由 model.CanPromote 在 registry 层校验。
func (s *Server) handlePromoteVersion(w http.ResponseWriter, r *http.Request) {
	toolID := r.PathValue("id")
	version := r.PathValue("v")
	var req promoteReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.Target == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("target 不能为空"))
		return
	}
	target := model.ToolVersionState(req.Target)
	if !model.ValidStates[target] {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("非法目标状态: %q（应为 draft/testing/stable/deprecated/rejected）", req.Target))
		return
	}
	if err := s.engine.PromoteTool(toolID, version, target, registry.WithActor(subjectOf(r))); err != nil {
		writeErr(w, statusForErr(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"tool_id": toolID,
		"version": version,
		"state":   string(target),
	})
}

func (s *Server) handleListFlows(w http.ResponseWriter, r *http.Request) {
	flows, err := s.reg.ListFlows()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if flows == nil {
		flows = []*model.Flow{}
	}
	writeJSON(w, http.StatusOK, flows)
}

func (s *Server) handleCreateFlow(w http.ResponseWriter, r *http.Request) {
	var f model.Flow
	if err := decodeJSON(r, &f); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	// 静态校验（含 Tool/原子能力存在性、when 可编译）——与 CLI、AI 闭环共用同一份规则。
	// 必须用**已装配的** s.engine：扩展目录（capReg）与 ext.ai 都挂在它上面，
	// 若在此新建一次性 engine，注册期就看不到任何 ext.*，判定会比运行期更宽/更严（两处漂移）。
	if err := s.engine.ValidateFlowStatic(&f); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.reg.UpsertFlow(&f, registry.WithActor(subjectOf(r))); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, f)
}

func (s *Server) handleValidateFlow(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	f, err := s.reg.GetFlow(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if f == nil {
		writeErr(w, http.StatusNotFound, fmt.Errorf("Flow 不存在: %s: %w", id, registry.ErrNotFound))
		return
	}
	if err := s.engine.ValidateFlowStatic(f); err != nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{"valid": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"valid": true})
}

func (s *Server) handleListHooks(w http.ResponseWriter, r *http.Request) {
	hooks, err := s.reg.ListHooks()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if hooks == nil {
		hooks = []*model.Hook{}
	}
	writeJSON(w, http.StatusOK, hooks)
}

func (s *Server) handleCreateHook(w http.ResponseWriter, r *http.Request) {
	var h model.Hook
	if err := decodeJSON(r, &h); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.reg.UpsertHook(&h, registry.WithActor(subjectOf(r))); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, h)
}

// handleDeleteFlow 注销 Flow。守卫全部在 registry.DeleteFlow 里（PRD §8.2 三条）：
// 被其它 Flow 的 step 引用、被 Flow 级 Hook 的 action 引用、或正被运行中的执行作为
// Hook 载体使用 → 拒绝（409）；不存在 → 404。REST 层只做「错误 → 状态码」映射，
// 不重复实现判定（守卫必须只有一份，否则 CLI 与 REST 会漂移）。
func (s *Server) handleDeleteFlow(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.reg.DeleteFlow(id, registry.WithActor(subjectOf(r))); err != nil {
		writeErr(w, statusForErr(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"deleted": id})
}

// handleDeleteHook 删除一个 Hook。与 Tool/Flow 不同，Hook 不被其它定义引用
// （它只是 stage+target 的匹配关系），所以没有引用守卫；但「不存在」仍报 404，
// 不静默 no-op。
func (s *Server) handleDeleteHook(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.reg.DeleteHook(id, registry.WithActor(subjectOf(r))); err != nil {
		writeErr(w, statusForErr(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"deleted": id})
}

type createExecutionReq struct {
	FlowID string `json:"flow_id"`
}

func (s *Server) handleCreateExecution(w http.ResponseWriter, r *http.Request) {
	var req createExecutionReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.FlowID == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("flow_id 不能为空"))
		return
	}
	f, err := s.reg.GetFlow(req.FlowID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if f == nil {
		writeErr(w, http.StatusNotFound, fmt.Errorf("Flow 不存在: %s: %w", req.FlowID, registry.ErrNotFound))
		return
	}

	// 身份链路（IDENTITY_CONTEXT_DESIGN.md §3.5）：把调用主体**在派发前抓成字符串**。
	// 不能沿用 r.Context() 进这个闭包——执行是在闸门 worker 里异步跑的，handler 一返回
	// 请求 ctx 就被取消，跑到一半会被判超时。抓字符串、在这里重新构造活着的 ctx 才对。
	// 必须在 Create **之前**取：主体要一起落进执行记录，而下面那条 429 分支也要用到它。
	caller := callerOf(r)
	rec := s.execs.Create(req.FlowID, caller.Subject)
	// 经并发闸门派发（REQ-CONCURRENCY-CROSS / #73）：worker 全忙且队列已满 → 拒绝（背压），
	// 执行记录置 rejected 并回 429，由调用方决定退避重试，而非放任高扇出打爆机器。
	job := func() {
		// 引擎在加载完 Flow + Hook 快照后回报「本次可能用到的 Tool」，
		// 作为 §8.2「运行中不可注销」守卫的判据（静态引用可能已被删/改）。
		// RunFlowExec 带上 exec id，使 step 运行日志经 logSink 落盘（REQ-API-LOGS / #71）。
		//
		// 父 ctx 是 s.liveCtx（**不是** context.Background()）：停机时它先被取消，
		// 长跑执行随之中断，闸门才能收敛。用 Background 的后果是停机只能干等
		// 最慢的那个执行跑完——一个声明 900s 的 Tool 就把停机拖 900s（2026-10-29 修）。
		res, runErr := s.engine.RunFlowExecCtx(
			engine.WithCaller(s.liveCtx, caller), rec.FlowID, rec.ID, func(toolIDs []string) {
				s.execs.SetTools(rec.ID, toolIDs)
			})
		s.execs.Finish(rec.ID, res, runErr)
	}
	if !s.gate.Submit(job) {
		const reason = "执行入口繁忙：并发已达上限且队列已满"
		s.execs.Reject(rec.ID, reason)
		writeJSON(w, http.StatusTooManyRequests, map[string]string{
			"id":     rec.ID,
			"status": StatusRejected,
			"error":  reason + "，请稍后重试",
		})
		return
	}

	writeJSON(w, http.StatusAccepted, map[string]string{"id": rec.ID, "status": rec.Status})
}

func (s *Server) handleGetExecution(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rec, ok := s.execs.Get(id)
	if !ok {
		writeErr(w, http.StatusNotFound, fmt.Errorf("执行记录不存在: %s: %w", id, registry.ErrNotFound))
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

// handleGetExecutionLogs 返回某次执行的 step 运行日志（stdout/stderr 行，按产生顺序）。
// 执行记录不存在 → 404；存在但无日志 → 返回空数组（200）。
func (s *Server) handleGetExecutionLogs(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.execs.Get(id); !ok {
		writeErr(w, http.StatusNotFound, fmt.Errorf("执行记录不存在: %s: %w", id, registry.ErrNotFound))
		return
	}
	writeJSON(w, http.StatusOK, s.execs.Logs(id))
}

// handleListExecutions 返回全部执行记录（持久化于 SQLite，重启可恢复；见 ExecStore）。
func (s *Server) handleListExecutions(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.execs.List())
}

type createAppReq struct {
	FlowID  string `json:"flow_id"`
	Restart string `json:"restart"` // always（默认）/ never
}

func (s *Server) handleCreateApp(w http.ResponseWriter, r *http.Request) {
	var req createAppReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.FlowID == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("flow_id 不能为空"))
		return
	}
	f, err := s.reg.GetFlow(req.FlowID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if f == nil {
		writeErr(w, http.StatusNotFound, fmt.Errorf("Flow 不存在: %s: %w", req.FlowID, registry.ErrNotFound))
		return
	}
	policy := app.RestartAlways
	switch app.RestartPolicy(req.Restart) {
	case app.RestartNever:
		policy = app.RestartNever
	case app.RestartAlways, "":
		policy = app.RestartAlways
	default:
		writeErr(w, http.StatusBadRequest, fmt.Errorf("非法的 restart=%q（应为 always 或 never）", req.Restart))
		return
	}

	// 父 ctx 取 s.liveCtx：停机时先取消它，监管循环（以及它拉起的 service 子进程）
	// 随之中断，不必等 Server.Shutdown 逐个 cancel 才收敛。
	ctx, cancel := context.WithCancel(s.liveCtx)
	inst := &AppInstance{
		ID: newID("app_"), FlowID: req.FlowID,
		Restart: string(policy), StartedAt: time.Now(), cancel: cancel, done: make(chan struct{}),
	}
	s.appMu.Lock()
	s.apps[inst.ID] = inst
	s.appSeq = append(s.appSeq, inst.ID)
	s.appMu.Unlock()

	s.appWG.Add(1)
	go func() {
		defer s.appWG.Done()
		// 监管 goroutine 真正退出时才关 done：它是「这个 app 是否真的停干净了」的
		// 唯一可靠判据（cancel 只是发信号，杀进程树的活儿在 Supervisor 里异步做）。
		defer close(inst.done)
		// 复用已装配的 s.engine：service step 同样需要扩展目录 / 密钥源 / 宿主原语，
		// 新建一次性 engine 会让「经 Apps 拉起的 Flow」比「经 /api/executions 跑的同一个 Flow」能力更弱。
		sup := app.New(s.engine, policy, 0)
		if err := sup.Supervise(ctx, []string{req.FlowID}); err != nil {
			fmt.Fprintf(os.Stderr, "app %s(%s) 结束: %v\n", inst.ID, req.FlowID, err)
		}
	}()

	writeJSON(w, http.StatusCreated, inst)
}

func (s *Server) handleListApps(w http.ResponseWriter, r *http.Request) {
	s.appMu.Lock()
	out := make([]*AppInstance, 0, len(s.appSeq))
	for _, id := range s.appSeq {
		a, ok := s.apps[id]
		if !ok {
			continue
		}
		// 复制一份再回填打开地址：避免并发读写共享的 *AppInstance（其 cancel 也随之复制，
		// 但副本不调用 cancel，无副作用）。OpenURLs 来自引擎按 flowID 聚合的收尾步输出。
		c := *a
		if s.engine != nil {
			c.OpenURLs = s.engine.OpenURLs(a.FlowID)
		}
		out = append(out, &c)
	}
	s.appMu.Unlock()
	writeJSON(w, http.StatusOK, out)
}

// appStopTimeout 是 DELETE /api/apps/{id} 等待「真的停干净」的上限。
//
// cancel() 只是发出停止信号，杀进程树（Windows 上走 taskkill /T）在 Supervisor 里异步执行。
// 此前这里一 cancel 就回 200 stopped，于是「进程还没死 / 端口还没释放」被当成成功——
// 与 `gear app stop`「收不干净如实报 ORPHAN_APP」是两套口径（2026-10-29 审出）。
// 现在是：等到真收敛（inst.done）才报成功；等不到就如实报 504 并说明还在跑。
const appStopTimeout = 10 * time.Second

func (s *Server) handleDeleteApp(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.appMu.Lock()
	a, ok := s.apps[id]
	if ok {
		delete(s.apps, id)
	}
	s.appMu.Unlock()
	if !ok {
		writeErr(w, http.StatusNotFound, fmt.Errorf("app 不存在: %s: %w", id, registry.ErrNotFound))
		return
	}
	a.cancel() // 取消 ctx ⇒ Supervisor 停止并杀掉所有 service 子进程

	// 等真的停干净：done 关闭 = 监管 goroutine 已退出 = stopAll 已跑完。
	// 请求 ctx 也一并听：客户端断开不必继续等。
	select {
	case <-a.done:
	case <-r.Context().Done():
		writeErr(w, http.StatusGatewayTimeout, fmt.Errorf("等待 app %s 停止时被客户端中断", id))
		return
	case <-time.After(appStopTimeout):
		// 如实上报：绝不说「stopped」——说了就等于告诉调用方端口已释放，而它可能还占着。
		writeJSON(w, http.StatusGatewayTimeout, map[string]interface{}{
			"id":      id,
			"stopped": false,
			"error":   fmt.Sprintf("已发出停止信号，但 %s 内未收敛：service 子进程可能仍存活，端口可能仍被占用", appStopTimeout),
			"hint":    "用 gear app list 复核进程与端口；必要时 gear app stop <id> 再收一次",
		})
		return
	}

	// 清理该 Flow 的打开地址缓存，避免留下过期链接（同名 Flow 再次受管时会被重新收集）。
	if s.engine != nil {
		s.engine.ClearOpenURLs(a.FlowID)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"stopped": id, "converged": true})
}

type pruneAppsReq struct {
	FlowID string `json:"flow_id,omitempty"`
	DryRun bool   `json:"dry_run,omitempty"`
}

// handlePruneApps 清理**落盘台账**里确定已终止的应用记录（body 空 = 全量、非 dry-run）。
//
// ⚠ 与同前缀的 GET/POST/DELETE /api/apps **不是同一个模型**：那三个管的是**本进程内存里**的
// AppInstance（等价 `gear app run`，进程一退即消失，既没有台账也没有孤儿）；本端点动的是
// `gear app start` 落下的**持久台账**（等价 MCP `gear_app(action=prune)`）。
// 沿用同一前缀是因为「应用」归这里管；而「批量清理」这件事只有台账侧存在——内存侧无物可清。
//
// 边界（保守优先，宁可少删）：只清 exited / stopped / startup_failed；running / starting / unknown
// 一律保留，且**只删记录、不杀进程、不碰端口**（要停进程走 DELETE /api/apps/{id}）。
func (s *Server) handlePruneApps(w http.ResponseWriter, r *http.Request) {
	var req pruneAppsReq
	if derr := json.NewDecoder(r.Body).Decode(&req); derr != nil && !errors.Is(derr, io.EOF) {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("请求体不是合法 JSON 对象: %w", derr))
		return
	}
	dir, err := app.ResolveAppsDir(s.registryPath, s.appsDir)
	if err != nil {
		// 定位不到台账就拒绝，而不是退回默认值去清另一个库（清错对象比清不掉更难排查）。
		writeErr(w, http.StatusInternalServerError, fmt.Errorf(
			"无法定位应用台账（本进程未配置 registry 路径）: %w", err))
		return
	}
	res, err := app.Prune(app.PruneOptions{
		AppsDir: dir, FlowID: strings.TrimSpace(req.FlowID), DryRun: req.DryRun,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, fmt.Errorf("清理台账失败: %w", err))
		return
	}
	out := map[string]interface{}{
		"dry_run": res.DryRun, "apps_dir": dir,
		"pruned": res.Pruned, "skipped": res.Skipped,
		"pruned_count": len(res.Pruned), "skipped_count": len(res.Skipped),
	}
	if len(res.Skipped) > 0 {
		out["hint"] = "skipped 里的项实时判据未能确定已终止（running/unknown 一律保留，" +
			"删了可能变成无人可查的孤儿）；要停它们走 DELETE /api/apps/{id}"
	}
	writeJSON(w, http.StatusOK, out)
}
