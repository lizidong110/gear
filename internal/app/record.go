package app

// record.go 实现「应用台账」—— gear_app / gear app 的**发现机制**。
//
// 为什么需要它：派生出去的 `gear app run` 是**独立进程**（PRD §14.1：一个 app run = 一个
// 进程，谁启动谁持有），MCP 与 CLI 都不持有它。没有台账，`status` / `stop` / `list` 就无从
// 下手——不知道 pid、日志在哪、声明了哪些端口。
//
// 台账**只记录「在哪里能找到它去管它」**，不改变进程归属：进程本来就自持，这不是 v1 那种
// 「MCP 自持 + pidfile 补记归属」的方案（那种是在制造孤儿后打补丁）。区别见
// MCP_AGENT_DESIGN.md §0.2 / §6.2⑤。
//
// 落盘位置：<registry 同目录>/apps/<app_id>.json（默认 ~/.gear/apps/），可用 --apps-dir 覆盖。

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// AppState 是应用对外的运行态。
//
// running / exited 由**双判据**（pid 存活 + 端口探活）推出：两者一致才给结论，不一致一律
// unknown 且**不自动处置**（pid 复用与端口抢占的 fail-closed 兜底，见 §6.2⑤.2）。
type AppState string

const (
	// StateStarting 已派生但仍在短等窗口内，尚未确认起来。
	StateStarting AppState = "starting"
	// StateRunning pid 存活，且（声明了端口时）端口在听。
	StateRunning AppState = "running"
	// StateExited pid 已退出，且（声明了端口时）端口已释放。
	StateExited AppState = "exited"
	// StateUnknown 两个判据互相矛盾（pid 在但端口没起 / 端口在听但 pid 已死）——不猜、不杀。
	StateUnknown AppState = "unknown"
	// StateStopped 经 stop 显式停止成功。
	StateStopped AppState = "stopped"
	// StateStartupFailed 派生后短等窗口内进程即退出（端口占用、侧车缺失等）。
	StateStartupFailed AppState = "startup_failed"
	// StateAlreadyRunning 同 flow 已有活跃实例，未重复拉起（幂等）。
	StateAlreadyRunning AppState = "already_running"
	// StateRestartExhausted 重启预算耗尽：窗口内退出次数超限，监管已放弃重启并退出
	// （REQ-APP-RESTART-LIMIT）。它与 exited 的区别是**有原因**——不是碰巧没了，
	// 而是「被反复弄死/持续崩溃到不值得再拉」，UI 与排障要看得出这一步。
	StateRestartExhausted AppState = "restart_exhausted"
)

// PortState 是单个声明端口的探活结果。
type PortState struct {
	Port      int  `json:"port"`
	Listening bool `json:"listening"`
}

// Record 是一条应用台账记录（JSON 落盘，跨会话可读）。
type Record struct {
	AppID     string     `json:"app_id"`
	FlowID    string     `json:"flow_id"`
	Registry  string     `json:"registry"`
	PID       int        `json:"pid"`
	Restart   string     `json:"restart"`
	StartedAt time.Time  `json:"started_at"`
	CWD       string     `json:"cwd"`
	LogPath   string     `json:"log_path"`
	Ports     []int      `json:"ports,omitempty"`
	StoppedAt *time.Time `json:"stopped_at,omitempty"`
	// FinalState 是「结案时的状态」：StoppedAt 非空 = 已结案（不再算遗留项），
	// FinalState 说明**怎么结的案**（startup_failed / stopped）。
	//
	// 为什么要分开：结案与「正常运行后被停」是两件事。统一报 stopped 会让排障时看不到
	// 「它其实压根没起来过」；而不结案又会让每次启动失败都永久挂在遗留清单里，
	// 把真正需要人过目的条目淹掉（遗留清单一旦变成噪声就没人看了）。
	FinalState string `json:"final_state,omitempty"`
	// EscapedJob 记录派生时是否真的脱离了调用方的进程沙箱（Windows Job Object）。
	//
	// 为什么要落进台账：**进程死因必须可事后归因**。未脱离 Job 的进程会在发起它的
	// 会话结束时被连坐回收，而那看起来和应用自己崩溃一模一样。记下来，`app status`
	// 才能回答「它为什么没了」。为 nil = 旧记录或本平台无此概念。
	EscapedJob *bool `json:"escaped_job,omitempty"`
	// LiveLogs 记录派生时是否注入了实时日志回推 env（REQ-API-APP-LOGS）。
	// 为 false 时 UI 必须如实提示「该应用未接入实时日志」（通常是启动时尚无 serve，
	// 或 serve 后来重启换了地址），不能挂一个永远空白的面板让人以为是日志没产生。
	LiveLogs bool `json:"live_logs,omitempty"`
	// Restarts 是监管期间 service 的累计重启次数。
	//
	// 为什么由监管进程回写而不是 launcher 记：重启发生在 `app run`（被派生的监管进程）
	// 里，launcher 早已返回、对此不可见。Start 派生时注入台账身份 env（GEAR_APP_LEDGER_*），
	// 监管进程经 RestartRecorderFromEnv 拿到回调，每次重启后把累计次数写回本记录——
	// 否则重启次数只在监管进程内存里，UI / CLI 看到的永远是 0（假健康）。
	Restarts int `json:"restarts,omitempty"`
}

// AppsDir 返回台账目录：<registry 同目录>/apps。
func AppsDir(registryPath string) string {
	return filepath.Join(filepath.Dir(registryPath), "apps")
}

// NewRecordID 生成可读、可排序、不与 pid 绑定的 app_id：app_<flow_id 规范化>-<UTC 时间戳>。
func NewRecordID(flowID string, now time.Time) string {
	return "app_" + sanitizeID(flowID) + "-" + now.UTC().Format("20060102T150405")
}

// sanitizeID 把 flow id 规范成文件名安全的形式（字母数字与 -_. 保留，其余换成 _）。
func sanitizeID(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "app"
	}
	return b.String()
}

// recordPath 返回某条记录的台账文件路径。
func recordPath(dir, appID string) string {
	return filepath.Join(dir, appID+".json")
}

// SaveRecord 原子写台账（先写临时文件再 rename），避免读到半截 JSON。
func SaveRecord(dir string, r *Record) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	tmp := recordPath(dir, r.AppID) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, recordPath(dir, r.AppID))
}

// LoadRecord 读取一条台账。不存在时返回的错误可用 errors.Is(err, os.ErrNotExist) 判别
// （调用方据此报 APP_NOT_FOUND，而不是含糊的「读取失败」）。
func LoadRecord(dir, appID string) (*Record, error) {
	b, err := os.ReadFile(recordPath(dir, appID))
	if err != nil {
		return nil, err
	}
	var r Record
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("台账 %s 解析失败: %w", appID, err)
	}
	return &r, nil
}

// ListRecords 列出全部台账（按 StartedAt 升序，便于稳定断言与阅读）。
// 目录不存在视为「暂无应用」而非错误。单个文件损坏只跳过该文件，不让一条坏记录遮蔽其余。
func ListRecords(dir string) ([]*Record, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []*Record
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		r, err := LoadRecord(dir, strings.TrimSuffix(e.Name(), ".json"))
		if err != nil {
			continue
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.Before(out[j].StartedAt) })
	return out, nil
}

// PruneOptions 是 Prune 的入参。
type PruneOptions struct {
	// AppsDir 是台账目录（调用方经 ResolveAppsDir 解析后传入）。
	AppsDir string
	// FlowID 非空时只处理该 Flow 的台账项（按应用批量清理）。
	FlowID string
	// DryRun 为真时只汇报「会删哪些」，不动磁盘。
	DryRun bool
}

// PrunedItem 是一条被（或将被）清理的台账项。
type PrunedItem struct {
	AppID  string `json:"app_id"`
	FlowID string `json:"flow_id"`
	// State 是**展示**用状态：优先取台账里的 FinalState（「当初怎么结的案」，如 startup_failed），
	// 它比实时判据更能说明这条记录是什么。
	State string `json:"state"`
	// ProbeState 是**判定**用的实时双判据结论（Prune 到底敢不敢删，看的是它）。
	//
	// 必须与 State 分开报：pid 会被系统复用，于是 FinalState=startup_failed 的记录
	// 实时判据可能给 unknown。只报一个字段就会输出「state: startup_failed 但 reason: 仍在运行」
	// 这种自相矛盾的话（2026-09-22 实测踩到，连开发者都要停下来查代码才敢确认不是 bug）。
	ProbeState string `json:"probe_state,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

// PruneResult 是 Prune 的结果。Skipped 刻意一并返回：只说「清了 8 条」而不说
// 「还有 4 条没敢动、为什么」，调用方就会以为台账已经干净了。
type PruneResult struct {
	DryRun  bool         `json:"dry_run"`
	Pruned  []PrunedItem `json:"pruned"`
	Skipped []PrunedItem `json:"skipped"`
}

// Prune 清理**确定已终止**的台账项。
//
// 存在的理由（2026-09-22 实测）：一轮调试下来台账里堆了 12 条 startup_failed / unknown /
// exited，而 gear_app 此前只有按 app_id 单个 stop 的入口——清理要调 12 次，且无从按状态过滤。
//
// 边界（保守优先，宁可少删）：
//   - 只清 Probe() 判为 exited / stopped / startup_failed 的项（进程确实不在了）；
//   - running / starting / already_running 跳过；
//   - **unknown 一律跳过**：双判据互相矛盾时不敢断言它已死，删台账会把「可能还在跑、
//     却再无人可查」的进程变成真孤儿——台账存在的全部意义就是避免这个。
//
// 注意：Prune 只删**台账记录**，不杀进程、不复检端口。要停进程请用 Stop。
func Prune(opts PruneOptions) (PruneResult, error) {
	recs, err := ListRecords(opts.AppsDir)
	if err != nil {
		return PruneResult{}, err
	}
	res := PruneResult{DryRun: opts.DryRun, Pruned: []PrunedItem{}, Skipped: []PrunedItem{}}
	for _, r := range recs {
		if opts.FlowID != "" && r.FlowID != opts.FlowID {
			continue
		}
		st := r.Probe()
		// 展示用状态优先取台账里的 FinalState（结案时的结论，如 startup_failed），
		// 它比当下 Probe 出来的 exited 更能说明「这条记录到底是什么」；判定仍用 Probe
		// ——那才是「现在还敢不敢删」的依据。两者一并回传（State / ProbeState），
		// 免得调用方看到「state=startup_failed 却被保留」时以为工具坏了。
		shown := string(st)
		if r.FinalState != "" {
			shown = r.FinalState
		}
		item := PrunedItem{AppID: r.AppID, FlowID: r.FlowID, State: shown, ProbeState: string(st)}
		if !prunableState(st) {
			item.Reason = "实时判据为 " + string(st) + "（不能确定已终止），保留台账——删掉可能变成无人可查的孤儿"
			res.Skipped = append(res.Skipped, item)
			continue
		}
		item.Reason = "已终止"
		if !opts.DryRun {
			if derr := os.Remove(recordPath(opts.AppsDir, r.AppID)); derr != nil && !errors.Is(derr, os.ErrNotExist) {
				item.Reason = "删除失败: " + derr.Error()
				res.Skipped = append(res.Skipped, item)
				continue
			}
		}
		res.Pruned = append(res.Pruned, item)
	}
	return res, nil
}

// prunableState 判定某个运行态是否「确定已终止、台账可清」。
func prunableState(st AppState) bool {
	switch st {
	case StateExited, StateStopped, StateStartupFailed:
		return true
	}
	return false
}

// PortStates 返回声明端口的逐项探活结果。
func (r *Record) PortStates() []PortState {
	out := make([]PortState, 0, len(r.Ports))
	for _, p := range r.Ports {
		out = append(out, PortState{Port: p, Listening: PortListening(p)})
	}
	return out
}

// Probe 按双判据推出运行态。
//
// 未声明端口时退化 pid 单判据（不然「没写 ports 的应用」永远判不出来）；
// 声明了端口时，pid 与端口必须**方向一致**才给结论——不一致给 unknown，既不谎报 running
// 也不自动杀（可能是 pid 复用，杀错代价远大于多问一句）。
func (r *Record) Probe() AppState {
	alive := processAlive(r.PID)
	if len(r.Ports) == 0 {
		if alive {
			return StateRunning
		}
		return StateExited
	}
	listening := 0
	for _, p := range r.Ports {
		if PortListening(p) {
			listening++
		}
	}
	switch {
	case alive && listening > 0:
		return StateRunning
	case !alive && listening == 0:
		return StateExited
	default:
		return StateUnknown
	}
}

// PortListening 对 127.0.0.1:port 做一次短超时 TCP 连接探测。
func PortListening(port int) bool {
	if port <= 0 {
		return false
	}
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 300*time.Millisecond)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// Orphan 是一条「需要人过目」的遗留记录。
//
// 只汇报，**不静默删除、不静默接管**——运行期失控的进程必须有明确的处置人（§6.2⑤.5）。
type Orphan struct {
	AppID   string `json:"app_id"`
	Reason  string `json:"reason"` // pid-gone | port-still-listening
	PID     int    `json:"pid,omitempty"`
	LogPath string `json:"log_path,omitempty"`
}

// Orphans 从台账里挑出遗留记录：
//   - pid-gone：pid 已退出却从未被 stop（进程悄悄死了，例如崩在启动期）；
//   - port-still-listening：pid 已退出但端口仍被占（残留的子进程/孙进程把端口攥着）。
//
// 已被显式 stop 的记录（StoppedAt 非空）视为历史，不再算 orphan。
// 注意判据是「从未结案」，不是「端口还占着」——遗留清单是**要人过目**的清单，不是「出事了」的清单。
func Orphans(recs []*Record) []Orphan {
	var out []Orphan
	for _, r := range recs {
		if r.StoppedAt != nil {
			continue
		}
		if processAlive(r.PID) {
			continue
		}
		reason := "pid-gone"
		for _, p := range r.Ports {
			if PortListening(p) {
				reason = "port-still-listening"
				break
			}
		}
		out = append(out, Orphan{AppID: r.AppID, Reason: reason, PID: r.PID, LogPath: r.LogPath})
	}
	return out
}
