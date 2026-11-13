package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"gear/internal/cap"
	"gear/internal/model"
	"gear/internal/sidecar/coord"
)

// ServiceHandle 是某个 service 模式 step 的长驻子进程句柄，由 app.Supervisor 监管。
type ServiceHandle struct {
	StepID    string                 // step id
	Runtime   model.Runtime          // 重启时按同一 runtime 重新分发
	Proc      Process                // 当前（或重启后的）子进程抽象句柄
	procMu    sync.Mutex             // 保护 Proc：探针 goroutine 与 Relaunch/stopAll 并发读写同一句柄
	Entry     string                 // 重启时复用的命令
	Input     map[string]interface{} // 已解析并校验的 input（重启时复用）
	Host      *cap.Context           // 重启时复用的宿主能力视图（M5，按原 Tool.Capabilities 授权）
	Probe     *model.HealthProbe     // service 健康检查探针（nil = 不探）；仅 service 模式生效
	Restarts  int                    // 已重启次数（restart=always 时 > 0）
	StartedAt time.Time              // 本次（重）启动时间；Supervisor 据此判断存活时长以重置退避
	// Coord 是 service ext 的协调会话（仅当该 step 引用 service ext 时非空）。
	// 持有看门狗心跳；Supervisor.stopAll 杀进程后调用其 Close() 完成 Deregister
	//（占用列表空则侧车自动退出，见 EXT_SERVICE_DESIGN.md §12）。
	Coord *coord.Session

	// Input/Output 落进 runContext（steps.<id>），使下游 step 能用 `${steps.<id>.output.<k>}`
	// 引用本 step —— service 的 output 是子进程 stdout 首行声明（如 `{"port":8787}`），
	// 这就是全链路端口分配的那条传递通道。
	Output map[string]interface{}
}

// ProcKill 在锁内杀掉当前 service 进程（探针与停机共用，避免与 Relaunch 并发写 Proc 竞争）。
func (h *ServiceHandle) ProcKill() error {
	h.procMu.Lock()
	defer h.procMu.Unlock()
	if h.Proc == nil {
		return nil
	}
	return h.Proc.Kill()
}

// waitExitPoll 是「本 gear 未持有本地 sidecar（client 共享他人实例）」时，
// 复查是否已接管为 owner 的间隔。
const waitExitPoll = 500 * time.Millisecond

// WaitExit 返回在该 service 需要重启时关闭的 channel；ctx 取消则直接返回（不关 channel）。
//
// 为什么不能只等「当前进程退出」：service ext 走 coord 握手，同主机多个 gear **共享同一个**
// sidecar（§12.4 注册优先 / 竞态退让）。退让为 client 的一方手上没有本地进程 —— 它的 Proc
// 是 noopProcess，Wait() 立即返回。若据此判定「service 已退出」，监管器就会去 Relaunch，
// 而 Relaunch 若拿 Entry 直接 rt.Start，就会硬抢已被占用的 coord 端口、拿到
// ExitAddrInUse(2) 速退 ——「共享成功」被误判成崩溃循环（2026-09-23 实测：两个 flow 各跑
// 一个 gear app run，第二个无限重启，日志里先是无退出信息、后是 exit=2）。
//
// 因此共享状态下本 gear 没有进程可等：sidecar 的存活由 coord 看门狗负责（心跳连续失败即
// rehandshake，必要时本 gear 接管为新 owner）。这里轮询「是否已持有本地进程」：一旦接管为
// owner（Proc 变成真实子进程）就转为等待该进程退出。
func (h *ServiceHandle) WaitExit(ctx context.Context) <-chan struct{} {
	ch := make(chan struct{})
	go func() {
		for {
			if p := h.CurrentProc(); h.HasLocalProc() && p != nil {
				// 把不可中断的 p.Wait() 放进子 goroutine，本循环据此可被 ctx 打断。
				//
				// 为什么必须这层中转：p.Wait() 是 os/exec 的阻塞等待，不接受 ctx。
				// 直接原地 Wait 的结果是——waitAny 每轮给每个 handle 起一个 goroutine，
				// ctx 取消后那 N-1 个「等其他 handle」的 goroutine 永久挂着，
				// 崩溃循环下每轮泄漏 N-1 个（2026-10-29 审出）。
				//
				// 代价如实说明：ctx 取消时子 goroutine 仍在 Wait（无法强拆 syscall），
				// 但泄漏量从「每轮 +N-1」降为「每个停机时仍存活的 service 至多 1 个」，
				// 且此刻进程正要退出——有界，且不再随重启次数增长。
				waited := make(chan struct{})
				go func(p Process) {
					defer close(waited)
					_ = p.Wait()
				}(p)
				select {
				case <-waited:
					close(ch)
					return
				case <-ctx.Done():
					return // 不关 channel：由等待方走 ctx 分支，避免被误读成「进程退出」
				}
			}
			select {
			case <-ctx.Done():
				return // 不关 channel：由等待方走 ctx 分支，避免被误读成「进程退出」
			case <-time.After(waitExitPoll):
			}
		}
	}()
	return ch
}

// HasLocalProc 报告本 gear 手上是否有一个**自己持有**的本地进程可监管。
//
// service ext（Coord != nil）只有 owner 才有：client 共享他人实例时 Proc 是 noopProcess，
// 既不可 kill 也不可 wait，不能拿它判存活（详见 WaitExit 的注释）。
func (h *ServiceHandle) HasLocalProc() bool {
	if h.Coord != nil {
		return h.Coord.IsOwner()
	}
	return h.CurrentProc() != nil
}

// CurrentProc 在锁内返回当前 Proc（探针据此判断进程是否已被重启，重置失败计数）。
func (h *ServiceHandle) CurrentProc() Process {
	h.procMu.Lock()
	defer h.procMu.Unlock()
	return h.Proc
}

// SuperviseResult 是 app run 监管启动后的结果：once 步骤已执行，service 步骤已拉起。
type SuperviseResult struct {
	FlowID   string
	Services []*ServiceHandle
}

// Supervise 执行 once 步骤并拉起 service 步骤，返回句柄（不阻塞等待）。
// 实际阻塞 / 重启 / 信号处理由 app.Supervisor 负责（PRD §14.2 / §14.3）。
//
// ctx 是**监管周期**的生命周期上下文：它只覆盖「启动阶段」（once 步骤 + 拉起 service），
// service 子进程本身是长驻的，用 context.Background() 起（见 startServiceStep），不随它取消。
// 传 ctx 的意义是让停机能打断启动阶段——此前这里完全不接受 ctx，
// 「Ctrl+C / DELETE app」只能靠上层干等一个可能永不返回的启动（2026-10-29 审出）。
//
// 执行期若任一 step 出错（含某 service 启动失败），已拉起的长驻 service 会在返回 error 前
// 被 stopStartedServices 回收——它们用 context.Background() 起、不随层级 ctx 取消，若在此
// 不回收即成孤儿进程（见该函数的注释）。
//
// 约束（F3 严格守卫）：
//   - service step 不能作为任何 step 的依赖（它永不「完成」，依赖它无意义）
//   - service 与 once 共用同一 runtime 分发（runtimeFor）；未实现 runtime 由 runtimeFor 统一报错，
//     本函数不再维护第二份白名单（M10 收口）
func (e *Engine) Supervise(ctx context.Context, flowID string) (*SuperviseResult, error) {
	flow, err := e.reg.GetFlow(flowID)
	if err != nil {
		return nil, err
	}
	if flow == nil {
		return nil, fmt.Errorf("Flow 不存在: %s", flowID)
	}

	svcIDs := map[string]bool{}
	for _, s := range flow.Steps {
		if s.IsService() {
			svcIDs[s.ID] = true
		}
	}
	for _, s := range flow.Steps {
		for _, d := range s.DependsOn {
			if svcIDs[d] {
				return nil, fmt.Errorf("step %s 依赖了 service 模式 step %s（service 永不结束，不能作为依赖）", s.ID, d)
			}
		}
	}

	levels, err := topoLevels(flow.Steps)
	if err != nil {
		return nil, err
	}

	hooks, err := e.loadHooks()
	if err != nil {
		return nil, err
	}

	// 这里**不**上报「本次运行用到的 Tool」（对比 run() 的 ToolObserver）：
	// app 路径下 service 进程一旦拉起就与 Registry 脱钩——重启走 ServiceHandle 缓存的
	// Entry/Runtime，不再读 Registry；once 步骤此时也已跑完。因此注销 Tool 不会打断
	// 已受管的长驻进程，§8.2 的静态引用守卫（Flow 的 step.call）已足够覆盖。
	// 若将来 service 支持「运行期重读 Tool」，此处必须改为与 run() 同样的上报。

	rc := &runContext{
		vars:      flow.Vars,
		steps:     map[string]map[string]interface{}{},
		secretGet: e.secretResolver(),
		flowID:    flowID,
	}
	// 实时日志挂点（REQ-API-APP-LOGS）：`app run` / REST 受管 App 走的是 Supervise，
	// 而不是 run()，没有 REST 执行记录 id。但阶段 1 的 logTee（once process）与
	// drainServiceTail（长驻 service）都挂在 rc.logSink 上 —— 这里不注入，就会变成
	// 「台账标着已接入实时日志、UI 却一行都收不到」的纯静默失效。
	// 用一个稳定的伪 execID 关联来源；前缀刻意与 broker 的 app:<appID> 命名空间隔开，
	// 免得两条不同的 App 模型把日志串到同一个订阅 key 上。回推器只消费 stepID/stream/line。
	if e.logSink != nil {
		rc.execID = "supervise:" + flowID
		rc.logSink = e.logSinkFor(rc.execID)
	}
	res := &SuperviseResult{FlowID: flowID}
	var svcMu sync.Mutex // 并行 goroutine 都会追加 service 句柄

	deps, err := depMap(flow.Steps)
	if err != nil {
		return nil, err
	}
	skipped := map[string]string{}
	var skMu sync.Mutex
	markSkipped := func(id, reason string) {
		skMu.Lock()
		skipped[id] = reason
		skMu.Unlock()
	}
	skipReasonFor := func(id string) (string, bool) {
		skMu.Lock()
		defer skMu.Unlock()
		for _, d := range deps[id] {
			if r, ok := skipped[d]; ok {
				return fmt.Sprintf("上游 step %s 被跳过（%s）", d, r), true
			}
		}
		return "", false
	}

	// S4：与 run() 同样走分层并行；service 与 once 共用同一执行路径。
	for _, level := range levels {
		err := e.execLevel(ctx, flow.Steps, level, func(ctx context.Context, step model.Step, call resolvedCall) error {
			if reason, ok := skipReasonFor(step.ID); ok {
				markSkipped(step.ID, reason)
				e.progressf("- step %s 跳过（%s）\n", step.ID, reason)
				return nil
			}

			if step.IsService() {
				h, err := e.startServiceStep(ctx, rc, hooks, flowID, step, call)
				if errors.Is(err, ErrStepSkipped) {
					markSkipped(step.ID, "when=false")
					e.progressf("- step %s 跳过（when=false）\n", step.ID)
					return nil
				}
				if err != nil {
					return err
				}
				svcMu.Lock()
				res.Services = append(res.Services, h)
				svcMu.Unlock()
				// 必须 setStep：否则 `${steps.<id>.output.<k>}` 在 service step 上永远取不到，
				// 全链路端口分配就只差「子进程报得出端口」这一环却仍走不通（2026-09-26 实测：
				// 一次性 step 走 setStep、service 分支没走，输出通道对长驻进程整个缺失）。
				rc.setStep(step.ID, h.Input, h.Output)
				e.progressf("• step %s (%s) 作为 service 长驻启动 (pid=%d)\n", step.ID, step.Call, h.Proc.Pid())
				return nil
			}

			input, output, err := e.runOnceStep(ctx, rc, hooks, flowID, step, call)
			if errors.Is(err, ErrStepSkipped) {
				markSkipped(step.ID, "when=false")
				e.progressf("- step %s 跳过（when=false）\n", step.ID)
				return nil
			}
			if err != nil {
				return err
			}
			rc.setStep(step.ID, input, output)
			// 收尾步（如站点/serve 类 once 步）带回的「打开地址」顺手打印成标记行，
			// 供 detached 启动的日志扫描通道（app.scanOpenURLs）透出到台账与 UI。
			printStepOpenURLs(e, output)
			e.progressf("✓ step %s (%s) -> %v\n", step.ID, step.Call, output)
			return nil
		})
		if err != nil {
			// 部分步骤失败（某 step 执行出错 / 某 service 启动失败）：已拉起的长驻 service
			// 用 context.Background() 起、脱离层级 ctx（见 startServiceStep：层结束的
			// cancel() 杀不到它），若不在此显式回收就会脱离监管成孤儿进程
			//（用户 2026-09-21 实测：flow 内两 serve，site 失败 → api(pid=8512) 残留）。
			// 返回 res（含已回收句柄）便于上层 / 测试观测，且即使上层忽略 res 也不泄漏。
			e.stopStartedServices(res)
			return res, err
		}
	}

	if len(res.Services) > 0 {
		e.progressf("• Flow %s 进入监管态（%d 个 service 长驻）\n", flowID, len(res.Services))
	} else {
		e.progressf("✓ Flow %s 执行完成（纯 once），共 %d 步\n", flowID, len(flow.Steps))
	}
	return res, nil
}

// printStepOpenURLs 把一次 once 步 output 里的「打开地址」打印成 `GEAR_OPEN_URL=<url>` 标记行。
// 约定：step 返回 `{url: ...}`、`{open_urls: [...]}` 或 `{lan_urls: [...]}`（可兼有），空串跳过。
//
// 为什么 `lan_urls` 也要打（2026-11-13）：ext.serve 会把「绑定地址」与「访问地址」分开——
// url 是通配绑定降级后的回环地址（本机点得开），lan_urls 是局域网地址（手机/平板用）。
// 只打 url 的话，手机用户拿到的是 127.0.0.1，等于没有地址；两者都进台账，
// 面板才能同时给出「本机打开」与「手机打开」两个入口。
//
// 为什么走 progressf 打印而不是内存聚合：a9c1cff 删掉了引擎侧 openurl.go 后，
// 唯一的消费方是 `gear app start` launcher 的日志扫描（app.scanOpenURLs）——它读的是
// app run 进程 stdout 落成的 <appID>.log，只认行首前缀。引擎不打印，serve 类 step 又都带
// `--no-ui`（自己不发 GEAR_OPEN_URL），托管启动后 UI 就永远没有可访问网址
// （2026-11-13 实测复现）。这里只做纯读取 + 打印，不触碰 output，也不因缺少字段报错；
// progressf 默认指 os.Stdout（app run 的 stdout），前台 `app run` 同样看得到，无害且有用。
func printStepOpenURLs(e *Engine, output map[string]interface{}) {
	if output == nil {
		return
	}
	urls := []string{}
	if u, ok := output["url"].(string); ok {
		urls = append(urls, u)
	}
	for _, key := range []string{"open_urls", "lan_urls"} {
		if arr, ok := output[key].([]interface{}); ok {
			for _, v := range arr {
				if s, ok := v.(string); ok {
					urls = append(urls, s)
				}
			}
		}
	}
	for _, u := range urls {
		if u = strings.TrimSpace(u); u != "" {
			e.progressf("GEAR_OPEN_URL=%s\n", u)
		}
	}
}

// stopStartedServices 回收本次 Supervise 已拉起的全部 service 子进程（Flow 执行失败时调用，
// 防长驻进程脱离监管成孤儿）。service ext 还会 Deregister 自身 PID（Coord.Close，占用列表空
// 则侧车自动退出，见 EXT_SERVICE_DESIGN.md §12）。
func (e *Engine) stopStartedServices(res *SuperviseResult) {
	if len(res.Services) == 0 {
		return
	}
	e.progressf("• 清理 %d 个已启动的 service（Flow 执行失败，防孤儿进程）\n", len(res.Services))
	for _, h := range res.Services {
		_ = h.ProcKill()
		if h.Coord != nil {
			_ = h.Coord.Close()
		}
	}
}

// Relaunch 重启一个已退出的 service step（restart=always 时由 Supervisor 调用）。
// 按原 runtime 重新分发，复用 Entry 与已校验的 input，Restarts 计数 +1。
//
// service ext（Coord != nil）**不走** runtime：它的 Entry 形如
// `COORD_ADDR=<listen> <sidecar>`，直接 rt.Start 会在端口已被他人 sidecar 占用时
// 拿到 ExitAddrInUse(2) 速退 —— 而握手本来的设计就是「抢不到就退让复用」，
// 所以重启必须重走 coord 握手：端口无人听则本 gear 接管为 owner 拉起 sidecar，
// 否则继续退让为 client 复用同一实例（见 coord.Session.Rehandshake）。
func (e *Engine) Relaunch(h *ServiceHandle) error {
	var proc Process
	if h.Coord != nil {
		if err := h.Coord.Rehandshake(); err != nil {
			return fmt.Errorf("restart step %s: coord 重握手失败: %w", h.StepID, err)
		}
		proc = h.Coord.Proc()
	} else {
		rt, err := runtimeForCaps(h.Runtime, e.sandboxPolicy, e.capReg)
		if err != nil {
			return fmt.Errorf("restart step %s: %w", h.StepID, err)
		}
		proc, err = rt.Start(context.Background(), h.Entry, h.Input, h.Host)
		if err != nil {
			return fmt.Errorf("restart step %s 失败: %w", h.StepID, err)
		}
	}
	h.procMu.Lock()
	h.Proc = proc
	h.procMu.Unlock()
	h.Restarts++
	h.StartedAt = time.Now()
	return nil
}
