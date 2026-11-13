package app

// launch.go 实现「把应用派生为独立后台进程」的启动器 + 台账管理。
//
// 定位（MCP_AGENT_DESIGN.md §0.2 / §6.2⑤）：这是 `gear app run` 的**后台启动器**，
// 与 `gear app run`（前台，人/CI 持有）、`POST /api/apps`（gear serve 持有）并列，
// 是**第三个等价启动器入口**。它派生进程后立即返回，**从不持有**那个进程——
// 持有者是派生出去的 `app run` 自己（PRD §14.1）。
//
// 这不违反 PRD §14.4「子进程归内核持有、避免进程树孤儿」：那条管的是**运行期 Flow→Flow
// 触发**；而「从零拉起一个应用」只能由外部发起。判据是「长驻子进程不得出现在调用方
// 进程树内」——本文件用 detach（见 launch_windows.go / launch_unix.go）满足它。

import (
	"errors"
	"fmt"
	neturl "net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gear/internal/procalive"
)

// StartOptions 是后台启动一个应用的入参。
type StartOptions struct {
	// RegistryPath 必填：派生进程**必须显式继承** registry 路径。不传就会读到子进程默认的
	// ~/.gear/registry.db，可能「启动了另一个库里的同名 Flow」——这是最难排查的一类错。
	RegistryPath string
	// AppsDir 台账目录；留空 = <registry 同目录>/apps。
	AppsDir string
	// FlowID 要监管的入口 Flow（须含 mode: service 的长驻 step，否则进程起来就退出）。
	FlowID string
	// Restart 重启策略 always | never；留空 = always。
	Restart string
	// Ports 是显式传入的声明端口（CLI --port / gear_app ports），用于台账探活与 stop 后
	// 复检。留空时**回退**到 App 清单的 start.ports（经 AppLookup 反查，见 REQ-MCP-PORTS-
	// ONE-TRUTH）；两处都没有则只按 pid 判存活。实际生效来源见 StartResult.PortsSource。
	Ports []int
	// AppLookup（可选）：按入口 FlowID 反查 App 清单的声明端口，Ports 为空时的回退来源。
	//
	// 由调用方注入（CLI / MCP 各自持 registry），launch 层保持零 registry 依赖——Start 的
	// 执行路径仍只吃入参，「起服失败因 registry 读不到」不会成为新故障模式。
	// 约定：(ports, matched, err)。matched=false 表示**没有唯一命中的 App**（没有 App、
	// 或多个 App 共用同一 entry_flow、或清单没声明端口）——此时不回退。歧义即放弃：
	// 多个 App 共用 entry_flow 时猜一个，比让调用方显式传更危险。
	AppLookup func(flowID string) (ports []int, matched bool, err error)
	// CWD 是派生进程的工作目录；留空 = 当前进程 cwd（相对路径依赖它，见 guide §环境）。
	CWD string
	// WaitSeconds > 0 时短等这么多秒确认启动结果；0 = 立即返回（默认）。
	WaitSeconds int
	// Replace 为 true 时，若同 Flow 已有活跃实例则先停再启；默认 false（幂等返回 already_running）。
	Replace bool
	// Self 是 gear 可执行文件路径；留空 = os.Executable()。
	Self string
	// Scope 是运行期命名空间守卫的作用域（App.ID），原样传给被派生的监管进程。
	//
	// 留空 = 不启用。它**必须**进派生 argv：否则 `gear app start --scope x` 只在父进程守住了，
	// 监管进程（真正跑 Flow 的那个）照旧能引用别的应用——守卫在最该生效的那一层失效，
	// 比完全不守卫更难发现（表面上看加过 --scope）。
	Scope string
}

// deriveRunArgs 拼出派生监管进程的命令行。
//
// 抽出来的理由：argv 里少一个 flag 就是「守卫在最该生效的那一层失效」，
// 而这类漏传不会报错——父进程照常返回「已启动」。把它变成可单测的小函数，
// 「--scope 必须跟到派生进程」就成了被测试盯住的约定，而不是靠人记得。
//
// --registry 放在最前（必须被显式继承，见 StartOptions.RegistryPath 的说明）；
// --scope 仅在非空时追加：空 scope 是默认（不启用守卫），不该在命令行上留下痕迹。
func deriveRunArgs(regPath, flowID, policy, scope string) []string {
	args := []string{"--registry", regPath, "app", "run", flowID, "--restart", policy}
	if scope != "" {
		args = append(args, "--scope", scope)
	}
	return args
}

// StartResult 是 start 的返回。
type StartResult struct {
	AppID   string      `json:"app_id"`
	FlowID  string      `json:"flow_id"`
	PID     int         `json:"pid"`
	State   string      `json:"state"`
	LogPath string      `json:"log_path"`
	Ports   []PortState `json:"ports"`
	// Reason 是启动即退出时的**根因**（从日志尾部里挑出），仅当 State==startup_failed 时非空。
	//
	// 它的存在理由：此前这一句只有 last_logs，而 CLI / MCP 又各印一句硬编码的泛化文案，
	// 三种常见故障共用一句话——等于没说。见 launch_reason.go 的口径（只收录实测见过的模式）。
	Reason   string   `json:"reason,omitempty"`
	LastLogs []string `json:"last_logs,omitempty"`
	// EscapedJob 说明派生进程是否**真的**脱离了调用方的进程沙箱（Windows Job Object）。
	//
	// 为什么必须报出来：`CREATE_BREAKAWAY_FROM_JOB` 失败时会退化为普通派生，进程仍在调用方
	// 的 Job 里——调用方一退出它就被连坐回收，「跨会话存活」的承诺并不成立。此前这条退化是
	// 静默的，于是「应用被我这条命令杀掉了」会被误读成「应用自己崩了」。
	// 为 nil 表示本平台/本路径无此概念（Unix 的 Setsid 恒成立）。
	EscapedJob *bool `json:"escaped_job,omitempty"`
	// Warning 是「本次启动虽然成功、但有个已知隐患」的一句话（目前只有未脱离 Job 这一种）。
	Warning string `json:"warning,omitempty"`
	// PortsSource 如实说明本次生效的声明端口来自哪里：explicit（入参）/ bundle（App 清单
	// 的 start.ports 回退）/ none。三级来源若不报出来，「这次到底用了哪份」就成了暗口径
	// ——排查「为什么探活没生效」时第一个要回答的就是它。
	PortsSource string `json:"ports_source,omitempty"`
	// OpenURLs 是从应用日志里挑出的「打开地址」标记（Flow 收尾步打印的 GEAR_OPEN_URL=…）。
	// 起服 Flow 把静态站/后端地址经 sidecar 返回，但此前既不回显也不落台账，用户跑完
	// app start 后「不知道到哪儿打开」——这条就是把地址显式捞出来（见 web_ui.done）。
	OpenURLs []string `json:"open_urls,omitempty"`
}

// StopOptions 是停止一个应用的入参。
type StopOptions struct {
	RegistryPath string
	AppsDir      string
	AppID        string
}

// StopResult 是 stop 的返回。
type StopResult struct {
	AppID         string      `json:"app_id"`
	FlowID        string      `json:"flow_id,omitempty"`
	PID           int         `json:"pid,omitempty"`
	State         string      `json:"state"`
	Code          string      `json:"code,omitempty"`
	PortsReleased bool        `json:"ports_released"`
	Ports         []PortState `json:"ports"`
	Hint          string      `json:"hint,omitempty"`
}

// StatusOptions 是查询一个应用的入参。
type StatusOptions struct {
	RegistryPath string
	AppsDir      string
	AppID        string
	// TailLogs > 0 时附上日志末 N 行（排障用）。
	TailLogs int
}

// StatusResult 是单个应用的状态快照。
type StatusResult struct {
	AppID     string      `json:"app_id"`
	FlowID    string      `json:"flow_id"`
	PID       int         `json:"pid"`
	State     string      `json:"state"`
	StartedAt time.Time   `json:"started_at"`
	UptimeS   int         `json:"uptime_s,omitempty"`
	Ports     []PortState `json:"ports"`
	LogPath   string      `json:"log_path"`
	LastLogs  []string    `json:"last_logs,omitempty"`
	// OpenURLs 见 StartResult.OpenURLs：从日志里捞出的「打开地址」。
	OpenURLs []string `json:"open_urls,omitempty"`
	// EscapedJob 见 StartResult.EscapedJob：进程死因的事后归因依据之一。
	EscapedJob *bool `json:"escaped_job,omitempty"`
	// Restart 是该实例的重启策略（always / never）。
	Restart string `json:"restart,omitempty"`
	// Restarts 是监管期间 service 的累计重启次数（台账回写，见 Record.Restarts）。
	Restarts int `json:"restarts,omitempty"`
	// LiveLogs 见 Record.LiveLogs：派生时是否注入了实时日志回推 env。
	LiveLogs bool `json:"live_logs,omitempty"`
}

// ListResult 是全部台账 + 遗留项。
type ListResult struct {
	Items  []StatusResult `json:"items"`
	Orphan []Orphan       `json:"orphan"`
}

// ResolveAppsDir 解析台账目录（AppsDir 显式指定优先，否则由 registry 路径推）。
//
// 导出它是为了让**展示层**（gear_doctor / gear app list）报出与真实落盘位置一致的目录——
// 直接展示入参里的空串会让智能体拿着一个空路径去猜（踩过：doctor 的 apps_dir 是空的）。
func ResolveAppsDir(registryPath, appsDir string) (string, error) {
	if appsDir != "" {
		return appsDir, nil
	}
	if registryPath == "" {
		return "", fmt.Errorf("需指定 RegistryPath 或 AppsDir 以定位应用台账")
	}
	return AppsDir(registryPath), nil
}

// Start 派生一个独立后台进程监管 FlowID，立即返回（不持有）。
//
// 幂等：同 Flow 已有活跃实例时直接返回 already_running 且**不重复拉起**（避免端口冲突）；
// 要换新实例须显式 Replace。
func Start(opts StartOptions) (*StartResult, error) {
	if opts.FlowID == "" {
		return nil, fmt.Errorf("FlowID 必填")
	}
	if opts.RegistryPath == "" {
		return nil, fmt.Errorf("RegistryPath 必填：派生进程须显式继承 registry 路径，" +
			"否则可能启动另一个库里的同名 Flow")
	}
	absReg, err := filepath.Abs(opts.RegistryPath)
	if err != nil {
		return nil, err
	}
	dir, err := ResolveAppsDir(absReg, opts.AppsDir)
	if err != nil {
		return nil, err
	}
	policy := opts.Restart
	if policy == "" {
		policy = string(RestartAlways)
	}
	if policy != string(RestartAlways) && policy != string(RestartNever) {
		return nil, fmt.Errorf("非法的 restart=%q（应为 always 或 never）", policy)
	}

	// 解析声明端口（入参 > App 清单 start.ports > 无，见 ports_lookup.go）。
	// 后面所有用到 opts.Ports 的地方（preBusy、台账、探活）一律吃这份解析结果，
	// 否则回退来的端口只报不探，等于又造半个暗口径。
	ports, portsSource, err := ResolveStartPorts(opts)
	if err != nil {
		return nil, err
	}

	// 幂等检查：同 Flow 的活跃实例（未被 stop 且探活为 running）。
	recs, err := ListRecords(dir)
	if err != nil {
		return nil, err
	}
	for _, r := range recs {
		if r.FlowID != opts.FlowID || r.StoppedAt != nil || r.Probe() != StateRunning {
			continue
		}
		if !opts.Replace {
			return &StartResult{
				AppID: r.AppID, FlowID: r.FlowID, PID: r.PID,
				State: string(StateAlreadyRunning), LogPath: r.LogPath, Ports: r.PortStates(),
			}, nil
		}
		if _, err := Stop(StopOptions{AppsDir: dir, AppID: r.AppID}); err != nil {
			return nil, fmt.Errorf("replace 前停止旧实例 %s 失败: %w", r.AppID, err)
		}
	}

	self, err := resolveSelf(opts.Self)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	appID := NewRecordID(opts.FlowID, now)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	// app_id 的时间戳只到**秒**：同一秒内对同一 Flow 再 start 会算出同一个 id，而台账是
	// 按 app_id 落文件的——重名会覆盖上一条记录，把前一个进程的 pid 与日志路径弄丢
	//（于是谁都不知道那个进程还在跑）。探测重名并加序号后缀。
	appID = uniqueAppID(dir, appID)
	logPath := filepath.Join(dir, appID+".log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("打开应用日志 %s 失败: %w", logPath, err)
	}
	defer logFile.Close()

	cwd := opts.CWD
	if cwd == "" {
		if wd, e := os.Getwd(); e == nil {
			cwd = wd
		}
	}

	// 声明端口在**派生之前**是否已被占用。端口探活无法归因：只有「启动前空闲、启动后在听」
	// 的端口才能证明「是我们的进程把服务起起来了」。启动前就被人占着的端口不能用于判定就绪
	// ——同端口已有常驻实例时引擎会复用其监听，于是 app 的 service 步立即完成、进程退出，
	// 而端口照样在服务；拿它判就绪就会把这种情况误报成 running。
	//
	// 注（2026-09-23）：上面「service 步立即完成、进程退出」是修复前的现象——当时监管层把
	// 「client 退让、本 gear 手上没有本地进程」误判成 service 退出并无限重启。现在共享实例
	// 不再触发重启（见 EXT_SERVICE_DESIGN.md §12.10），但**探活归因**这条理由依然成立：
	// 端口在听不能证明是本次派生的进程把它拉起来的，preBusy 的排除逻辑照旧。
	preBusy := make(map[int]bool, len(ports))
	for _, p := range ports {
		if PortListening(p) {
			preBusy[p] = true
		}
	}

	// 实时日志回推（REQ-API-APP-LOGS）：serve 绑定成功后会在 <appsDir>/server.addr
	// 留下「回推地址 + token」。派生前读出来以 env 注入子进程；读不到（serve 没起 /
	// 地址已过期）就返回 nil，纯本地日志照旧完整，绝不因为日志功能不让应用启动。
	// 台账记下到底有没有注入，UI 据此明说「未接入实时日志」，而不是给一个空白面板。
	logEnv := LogIngestEnv(dir, appID)
	// 台账身份 env（见 Record.Restarts）：把「我是哪条台账」传给被派生的监管进程，
	// 它才有能力把 service 重启次数写回记录。没有这段 env 不影响启动——
	// 重启次数只是「记不上」，而不是起不来。
	env := append(logEnv, LedgerEnv(dir, appID)...)
	args := deriveRunArgs(absReg, opts.FlowID, policy, opts.Scope)
	proc, escaped, err := launchDetached(self, args, cwd, logFile, env)
	if err != nil {
		return nil, fmt.Errorf("派生后台进程失败: %w", err)
	}

	rec := &Record{
		AppID: appID, FlowID: opts.FlowID, Registry: absReg, PID: proc.Pid,
		Restart: policy, StartedAt: now, CWD: cwd, LogPath: logPath, Ports: ports,
		EscapedJob: &escaped, LiveLogs: len(logEnv) > 0,
	}
	if err := SaveRecord(dir, rec); err != nil {
		// 进程已经在跑，台账却写不下去：如实报错并给出 pid/日志，让人能手动收掉——
		// 绝不静默当作成功（否则就是一个「无人知晓的进程树」）。
		return nil, fmt.Errorf("后台进程已启动（pid=%d，日志 %s）但台账写入失败: %w；"+
			"请用 `gear app stop` / 手动结束该 pid 处理", proc.Pid, logPath, err)
	}

	res := &StartResult{AppID: appID, FlowID: opts.FlowID, PID: proc.Pid, State: string(StateStarting), LogPath: logPath, PortsSource: portsSource}
	res.EscapedJob = &escaped
	if !escaped {
		// 退化派生：进程仍在调用方的 Job Object 里，调用方退出即被回收。
		// 不阻断（多数环境没有这个限制，因此仍可能「碰巧」活得很好），但必须说清后果——
		// 否则用户会在会话结束后才发现应用没了，并把原因归到应用自己身上。
		res.Warning = "派生进程未能脱离调用方的进程沙箱（Windows Job Object 不允许 breakaway）：" +
			"它会在**发起本次启动的会话/命令结束时被一并回收**，因此不具备跨会话存活能力。" +
			"若要长期托管，请让 gear 由一个不受该 Job 约束的进程发起（如在独立终端里执行 app start）。"
	}
	res.State, res.Ports, res.LastLogs = waitReady(rec, opts.WaitSeconds, preBusy)
	// 捞「打开地址」：Flow 收尾步打印的 GEAR_OPEN_URL=…（见 web_ui.done）。
	// 纯读取、不改动任何既有行为；没打标记的旧 Flow 返回 nil，完全不受影响。
	res.OpenURLs = scanOpenURLs(rec.LogPath)
	if res.State == string(StateStartupFailed) {
		// 根因只在监管进程的日志里（派生进程是 detached 的，取不到退出码），挑出来挂在
		// StartResult 上，好让 CLI / MCP 不必各自印那句对所有故障都成立的泛化文案。
		res.Reason = classifyStartupFailure(startupFailureReason(res.LastLogs))
	} else {
		res.Reason = ""
	}
	if res.State == string(StateStartupFailed) {
		// 短等窗口内进程就退出了：**我们已经知道**它不在跑，就不该让它以「遗留应用」的身份
		// 永远挂在 doctor 的 orphan 列表里（遗留项的语义是「pid 没了却从未被 stop，得有人过目」）。
		// 结案时记下 FinalState，让 status 仍然能如实报出「它其实是启动失败」，而不是笼统的 stopped。
		now := time.Now()
		rec.StoppedAt = &now
		rec.FinalState = string(StateStartupFailed)
		_ = SaveRecord(dir, rec)
	}
	return res, nil
}

// uniqueAppID 保证 app_id 在台账目录里唯一：重名时依次尝试 `-2` / `-3` … 后缀。
// 为什么需要：NewRecordID 的时间戳粒度是秒，而台账按 app_id 落文件；没有这层去重，
// 「停掉后马上重启」（最常见的操作）会覆盖上一条记录，连带丢掉前一个进程的 pid。
func uniqueAppID(dir, base string) string {
	if _, err := os.Stat(recordPath(dir, base)); err != nil {
		return base
	}
	for i := 2; ; i++ {
		cand := fmt.Sprintf("%s-%d", base, i)
		if _, err := os.Stat(recordPath(dir, cand)); err != nil {
			return cand
		}
	}
}

// 注入给派生监管进程的台账身份 env（launcher → 子进程；RestartRecorderFromEnv 消费）。
const (
	EnvLedgerDir = "GEAR_APP_LEDGER_DIR"
	EnvLedgerID  = "GEAR_APP_LEDGER_ID"
)

// LedgerEnv 生成台账身份 env：让被派生的监管进程知道自己对应哪条台账记录。
func LedgerEnv(dir, appID string) []string {
	return []string{EnvLedgerDir + "=" + dir, EnvLedgerID + "=" + appID}
}

// RestartRecorderFromEnv 返回「把累计重启次数写回台账」的回调（Supervisor.SetOnRestart 用）。
//
// 为什么是 env 而不是命令行 flag：台账身份对 `app run` 是**可选**信息——人前台跑
// `app run` 时根本没有台账，用 flag 就得为「不存在的东西」留参数位；env 只在
// Start 派生链路上存在，前台路径天然为零。台账读不到（已被 prune / stop）时静默跳过：
// 重启照常进行，只是记不上，绝不因为记账失败把监管停了。
func RestartRecorderFromEnv() func(total int) {
	dir := strings.TrimSpace(os.Getenv(EnvLedgerDir))
	id := strings.TrimSpace(os.Getenv(EnvLedgerID))
	if dir == "" || id == "" {
		return nil
	}
	return func(total int) {
		rec, err := LoadRecord(dir, id)
		if err != nil {
			return
		}
		rec.Restarts = total
		_ = SaveRecord(dir, rec)
	}
}

// ExhaustedRecorderFromEnv 返回「重启预算耗尽时把终态写回台账」的回调
// （Supervisor.SetOnExhausted 用，REQ-APP-RESTART-LIMIT）。
//
// 为什么需要它：预算耗尽时监管进程会**自行退出**，此后 pid 消失、端口释放，
// 台账若只留一条 running 记录，UI 就只能显示「exited」——把「被反复弄死到放弃重启」
// 和「正常退出」混为一谈。写成 final_state=restart_exhausted 才能让 UI / app list
// 直接看出原因。与 RestartRecorderFromEnv 同口径：读不到台账就静默跳过，
// 绝不因为记账失败影响停机。
func ExhaustedRecorderFromEnv() func(total int, reason string) {
	dir := strings.TrimSpace(os.Getenv(EnvLedgerDir))
	id := strings.TrimSpace(os.Getenv(EnvLedgerID))
	if dir == "" || id == "" {
		return nil
	}
	return func(total int, _ string) {
		rec, err := LoadRecord(dir, id)
		if err != nil {
			return
		}
		now := time.Now()
		rec.Restarts = total
		rec.StoppedAt = &now
		rec.FinalState = string(StateRestartExhausted)
		_ = SaveRecord(dir, rec)
	}
}

// waitReady 在 waitSeconds 窗口内轮询启动结果。
//
// 三种收敛：running（起来了）/ startup_failed（进程已退出，附日志尾部）/ starting（窗口内
// 进程仍活着但没探测到就绪——此时不是失败，只是还没起完）。
//
// 判据以**进程存活**为底线，以**可归因的端口**为就绪信号：
//
//	pid 已退出                                 → startup_failed（无论端口谁在听）
//	pid 活着 + 某个「启动前空闲」的端口在听    → running
//	pid 活着 + 没有可归因的就绪信号            → 继续观察到窗口结束，全程存活才给 running
//
// 为什么 pid 是底线（而不复用 Record.Probe()）：Probe 在「pid 已死 + 端口仍有监听」时给
// unknown，而那个监听**可能是别人**（同端口已有常驻实例时引擎复用其监听，app 的 service
// 步随即完成、进程退出）。unknown 不是 Probe 调用点那个 switch 的任何分支，于是一路等到
// 窗口超时并报 starting——让智能体以为「还在启动」，而进程其实早就死了（plugin/verify.py
// 的 P4 场景暴露过这条误导）。
//
// 为什么还要 preBusy：端口探活**不区分归属**。若把一个「启动前就已经被别人占着」的端口
// 当成就绪信号，一个「派生即秒退」的进程就会在它退出的那一瞬被判成 running；但若只看
// pid，又区分不了「起来了」和「在起、还没起完」。所以只有**启动前空闲、启动后在听**的
// 端口才算「我们把服务起起来了」——这一条同时挡住两种误报，且不需要任何时间魔法常数。
//
// 为什么「未声明端口」要等满窗口才给 running（2026-09-21 修正）：此前 `len(ports)==0`
// 直接返回 running，等于「pid 在第 0 毫秒还活着 ⇒ 启动成功」——那不是一个证据，是一个猜测。
// 实测连中两次：Flow 因 step 校验失败而必崩，可 `gear_app start` 照样回 `running`，
// 智能体据此认为应用已就绪，直到几十秒后应用早已不在（`app list` 才给真相）。
// 未声明端口时**结构上不存在**就绪信号，能给出的最强结论只能是「它在窗口内一直活着」，
// 所以就把窗口等满——调用方要是不想等，本来就会传 --wait 0（那时返回 starting，不猜）。
func waitReady(rec *Record, waitSeconds int, preBusy map[int]bool) (string, []PortState, []string) {
	if waitSeconds <= 0 {
		return string(StateStarting), rec.PortStates(), nil
	}
	deadline := time.Now().Add(time.Duration(waitSeconds) * time.Second)
	for {
		if !processAlive(rec.PID) {
			return string(StateStartupFailed), rec.PortStates(), tailLogs(rec.LogPath, 20)
		}
		ports := rec.PortStates()
		for _, ps := range ports {
			if ps.Listening && !preBusy[ps.Port] {
				return string(StateRunning), ports, nil
			}
		}
		if time.Now().After(deadline) {
			if len(ports) == 0 {
				// 全程存活，且未声明端口 ⇒ 这就是能给出的最强结论（口径与 Record.Probe 一致）。
				return string(StateRunning), ports, nil
			}
			return string(StateStarting), ports, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// Stop 终止应用并**复检端口**。
//
// 「发了信号」不等于「收干净了」：Windows 上没有控制台只能硬杀，孙进程可能攥着端口不放。
// 收不干净时返回 ORPHAN_APP + pid，**不谎报 stopped**（§6.2⑤ / R3b）。
func Stop(opts StopOptions) (*StopResult, error) {
	dir, err := ResolveAppsDir(opts.RegistryPath, opts.AppsDir)
	if err != nil {
		return nil, err
	}
	if opts.AppID == "" {
		return nil, fmt.Errorf("AppID 必填")
	}
	rec, err := LoadRecord(dir, opts.AppID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// %w 保留 ErrNotExist：REST 面（DELETE /api/apps/{id}）据此区分 404 与 500，
			// 不必再自己读一遍台账做存在性预检。
			return nil, fmt.Errorf("应用不存在: %s（先 gear_app(action=list) 看台账）: %w", opts.AppID, os.ErrNotExist)
		}
		return nil, err
	}

	killTree(rec.PID) // 进程已不存在时是 no-op，不报错

	pidGone, portsFree := waitStopped(rec, 5*time.Second)
	res := &StopResult{
		AppID: rec.AppID, FlowID: rec.FlowID, PID: rec.PID,
		Ports: rec.PortStates(), PortsReleased: portsFree,
	}
	if pidGone && portsFree {
		now := time.Now()
		rec.StoppedAt = &now
		rec.FinalState = string(StateStopped)
		_ = SaveRecord(dir, rec)
		res.State = string(StateStopped)
		return res, nil
	}
	res.State = string(StateUnknown)
	res.Code = "ORPHAN_APP"
	res.Hint = fmt.Sprintf("未收干净（pid=%d %s，端口%s）——用 gear_doctor 看谁占着端口",
		rec.PID, aliveWord(pidGone), freeWord(portsFree))
	return res, nil
}

// waitStopped 轮询等待 pid 消失且端口释放。
func waitStopped(rec *Record, timeout time.Duration) (pidGone, portsFree bool) {
	deadline := time.Now().Add(timeout)
	for {
		pidGone = !processAlive(rec.PID)
		portsFree = true
		for _, p := range rec.Ports {
			if PortListening(p) {
				portsFree = false
				break
			}
		}
		if (pidGone && portsFree) || time.Now().After(deadline) {
			return pidGone, portsFree
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func aliveWord(pidGone bool) string {
	if pidGone {
		return "已退出"
	}
	return "仍在"
}

func freeWord(portsFree bool) string {
	if portsFree {
		return "已释放"
	}
	return "仍被占用"
}

// ProcessAlive 报告 pid 是否仍存活（导出给 MCP 层的「孤儿执行」识别用，评审 I-1④）。
//
// 平台差异收在 internal/procalive，不在本包各平台文件里各写一份：Windows 的判据是
// 「OpenProcess + 退出码 == STILL_ACTIVE」，Unix 是 signal 0，两者在「已退出但句柄未释放」
// 这种形态下结论相反——各写一份必然漂移（本包已经踩过一次）。
func ProcessAlive(pid int) bool { return procalive.Alive(pid) }

// processAlive 是包内判据，与 ProcessAlive 同源（不导出是为了让「判存活」这件事只有一个
// 实现可改，Windows / Unix 两个平台文件都不再自带 processAlive）。
func processAlive(pid int) bool { return procalive.Alive(pid) }

// Status 返回单个应用的状态快照。
func Status(opts StatusOptions) (*StatusResult, error) {
	dir, err := ResolveAppsDir(opts.RegistryPath, opts.AppsDir)
	if err != nil {
		return nil, err
	}
	rec, err := LoadRecord(dir, opts.AppID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("应用不存在: %s（先 gear_app(action=list) 看台账）", opts.AppID)
		}
		return nil, err
	}
	return statusOf(rec, opts.TailLogs), nil
}

// statusOf 把一条台账渲染成状态快照（已结案的记录取**结案原因**，而不是笼统的 stopped）。
func statusOf(rec *Record, tail int) *StatusResult {
	st := rec.Probe()
	if rec.StoppedAt != nil {
		st = StateStopped
		if rec.FinalState != "" {
			// 保留「启动失败」这类事实：统一报 stopped 会让排障时看不到它压根没起来过。
			st = AppState(rec.FinalState)
		}
	}
	res := &StatusResult{
		AppID: rec.AppID, FlowID: rec.FlowID, PID: rec.PID, State: string(st),
		StartedAt: rec.StartedAt, Ports: rec.PortStates(), LogPath: rec.LogPath,
		EscapedJob: rec.EscapedJob, Restart: rec.Restart,
		Restarts: rec.Restarts, LiveLogs: rec.LiveLogs,
	}
	if st == StateRunning {
		res.UptimeS = int(time.Since(rec.StartedAt).Seconds())
	}
	if tail > 0 {
		res.LastLogs = tailLogs(rec.LogPath, tail)
	}
	res.OpenURLs = scanOpenURLs(rec.LogPath)
	return res
}

// List 返回全部台账（含已停止的历史记录）与遗留项。
func List(registryPath, appsDir string) (*ListResult, error) {
	dir, err := ResolveAppsDir(registryPath, appsDir)
	if err != nil {
		return nil, err
	}
	recs, err := ListRecords(dir)
	if err != nil {
		return nil, err
	}
	out := &ListResult{Items: []StatusResult{}, Orphan: []Orphan{}}
	for _, r := range recs {
		out.Items = append(out.Items, *statusOf(r, 0))
	}
	out.Orphan = Orphans(recs)
	return out, nil
}

// resolveSelf 定位 gear 可执行文件路径（派生用的 argv[0]）。
func resolveSelf(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("无法定位 gear 可执行文件（os.Executable 失败）: %w", err)
	}
	return exe, nil
}

// tailLogs 读日志末尾 n 行（读不到就返回 nil，不把「没日志」变成错误）。
func tailLogs(path string, n int) []string {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	text := strings.TrimRight(string(b), "\n")
	if text == "" {
		return nil
	}
	lines := strings.Split(text, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}

// scanOpenURLs 从应用日志里挑出「打开地址」标记行（约定格式：GEAR_OPEN_URL=<url>）。
//
// 为什么有这个约定（2026-10-10）：起服 Flow 经由 sidecar 把静态站/后端地址返回，但此前
// 既不回显也不落台账——用户跑完 `app start web_ui.up` 后「不知道到哪儿打开」。
// 由 Flow 的收尾步（如 web_ui.done）把地址打印成 `GEAR_OPEN_URL=<url>`，launcher 只做
// 「把它显式报出来」这一件小事：纯读取、不改动任何现有行为，所有没打这个标记的旧 Flow
// 完全不受影响（返回 nil）。任何想要「启动即告知地址」的站点类应用都可复用这一约定。
func scanOpenURLs(logPath string) []string {
	b, err := os.ReadFile(logPath)
	if err != nil {
		return nil
	}
	const prefix = "GEAR_OPEN_URL="
	var out []string
	seen := make(map[string]bool)
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		u := normalizeOpenURL(strings.TrimSpace(strings.TrimPrefix(line, prefix)))
		if u == "" || seen[u] {
			continue
		}
		seen[u] = true
		out = append(out, u)
	}
	return out
}

// normalizeOpenURL 把「不可访问的绑定地址」归一化成可访问地址后再进台账。
//
// 为什么在**消费端**再兜一次（侧车已修，见 serve-sidecar siteURL）：台账是追加日志的
// 投影，`<appID>.log` 里已经躺着旧版本打出来的 `GEAR_OPEN_URL=http://0.0.0.0:8080/`。
// 只修生产端的话，同一个应用重启后旧行仍在，面板会同时显示一个坏链接——「修了但还是
// 打不开」比没修更难解释。这里只处理通配主机（0.0.0.0 / :: / 空），具体地址一律原样。
func normalizeOpenURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := neturl.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	var repl string
	switch u.Hostname() {
	case "", "0.0.0.0":
		repl = "127.0.0.1"
	case "::":
		repl = "::1"
	default:
		return raw
	}
	host := repl
	if strings.Contains(repl, ":") {
		host = "[" + repl + "]" // IPv6 字面量必须带方括号
	}
	if p := u.Port(); p != "" {
		host += ":" + p
	}
	u.Host = host
	return u.String()
}
