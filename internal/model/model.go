// Package model 定义 Gear 内核的核心数据模型。
// 这些结构体是 Tool / Flow / Hook 在内存与磁盘(YAML/SQLite)之间的唯一真相源。
package model

import (
	"fmt"
	"strings"
	"time"
)

// Runtime 表示 Tool 的执行运行时。
type Runtime string

const (
	RuntimeProcess Runtime = "process" // 外部进程：stdin 进 JSON，stdout 末行出 JSON
	RuntimeLua     Runtime = "lua"     // 嵌入式 Lua（S7）
	RuntimeGoHost  Runtime = "go-host" // 进程内 Go 函数（S7）
	RuntimeWASM    Runtime = "wasm"    // WebAssembly（后期）
)

// KnownRuntimes 是所有合法 runtime 枚举值（单一来源，供校验与文档共用）。
var KnownRuntimes = map[Runtime]bool{
	RuntimeProcess: true,
	RuntimeLua:     true,
	RuntimeGoHost:  true,
	RuntimeWASM:    true,
}

// ImplementedRuntimes 是引擎**真正实现**了适配器的 runtime 集合（单一来源，C）。
// 新增 runtime（如 S7 的 lua、#76 的 go-host）只需：在此登记 + 在 engine.runtimeFor 加一个 case。
// 注册期校验、引擎分发、文档状态均以本表为唯一依据，避免多处列表漂移。
var ImplementedRuntimes = map[Runtime]bool{
	RuntimeProcess: true,
	RuntimeLua:     true, // S7a：嵌入式 Lua（沙箱，once 可用；service 不支持）
	RuntimeGoHost:  true, // #76：进程内 Go 函数（engine.RegisterHostFunc 登记；once 可用；service 不支持）
}

// Known 报告 r 是否为合法枚举值。
func (r Runtime) Known() bool { return KnownRuntimes[r] }

// Implemented 报告 r 是否已有引擎实现（once 路径可用）。
// 已知但未实现者（仅 wasm 现仍 deferred，计划 V3）注册期应被拒绝。
// 注意：service 能力另见 luaRuntime.Start / goHostRuntime.Start 的显式错误
// （lua / go-host 已实现但不支持 service，长驻请用 process）。
func (r Runtime) Implemented() bool { return ImplementedRuntimes[r] }

// ToolVersionState 是 Tool 版本的生命周期状态。
type ToolVersionState string

const (
	StateDraft      ToolVersionState = "draft"      // 草稿，未晋升
	StateTesting    ToolVersionState = "testing"    // 测试中（灰度）
	StateStable     ToolVersionState = "stable"     // 稳定，默认调用
	StateDeprecated ToolVersionState = "deprecated" // 已弃用，仍可调用
	StateRejected   ToolVersionState = "rejected"   // 已否决，不可调用
)

// Tool 是原子能力，带 input/output schema，可被 Flow 内 step 调用。
type Tool struct {
	ID           string                 `yaml:"id" json:"id"`
	Name         string                 `yaml:"name" json:"name"`
	Description  string                 `yaml:"description" json:"description"`
	Runtime      Runtime                `yaml:"runtime" json:"runtime"`
	Entry        string                 `yaml:"entry" json:"entry"` // process: 命令；lua/go-host: 函数引用
	InputSchema  map[string]interface{} `yaml:"input_schema" json:"input_schema"`
	OutputSchema map[string]interface{} `yaml:"output_schema" json:"output_schema"`
	Versions     []ToolVersion          `yaml:"versions" json:"versions"`
	// Capabilities 声明该 Tool 被允许使用的宿主能力原语（PRD §6，最小权限）。
	// 对应 ctx.* 的 http/call/db/secret/event；运行期未声明的原语调用一律拒绝。
	Capabilities []string `yaml:"capabilities" json:"capabilities"`
	// HealthProbe 是 service 模式 Tool 的健康检查探针（REQ-APP-HEALTH / #75）。
	// 仅当该 Tool 被 service step 调用时生效：进程「活着」≠「健康」，探针按阈值
	// 连续失败后由 Supervisor 杀掉并重启（与崩溃重启共用退避）。once 工具忽略。
	HealthProbe *HealthProbe `yaml:"health_probe,omitempty" json:"health_probe,omitempty"`
	// Timeout 声明该 Tool **单次执行**的超时（Go duration 字符串，如 "5m"、"300s"）。
	//
	// 缺省（空）→ 用引擎默认（defaultExecTimeout，30s），保持「防止挂死工具占住执行线程」的
	// 默认语义；AI 补全、长任务等天然长耗时的 Tool 必须显式声明，否则会被默认值误杀——
	// 典型症状：lua 里调用阻塞的 ctx.ext.ai.complete，AI 尚未返回就撞上默认闸门，
	// 报 `lua 执行被中断: context deadline exceeded`（且时长常远超默认值，
	// 因为阻塞的宿主调用无法被 ctx 打断，只有回到 lua 解释循环才检查取消）。
	//
	// 该值在**注册期**即解析校验（非法即拒绝注册），不留到运行期。
	Timeout string `yaml:"timeout,omitempty" json:"timeout,omitempty"`
	// API 是可选字段（ENTERPRISE_PRD FR-3）：声明了就额外挂一条 RESTful 资源路由
	// （如 `GET /api/orders/{id}`），路径参数映射进 Tool 输入、响应体即 Tool 输出。
	//
	// 未声明的 Tool **行为一字不变**——这是 AC-C4 的红线，注册期只当它不存在。
	// 声明是「加挂」而不是「替换」：`POST /api/tools/{id}/run` 照旧可用。
	API *APIRoute `yaml:"api,omitempty" json:"api,omitempty"`
}

// ExecTimeout 解析 Tool 声明的执行超时。
// 返回 (0, nil) 表示「未声明，调用方应回退引擎默认值」；声明非法则返回错误。
//
// 只在 model 层解析、engine 层消费：注册期（Tool.Validate）与执行期共用本方法，
// 避免「注册期判合法、运行期又解析一次」的双口径漂移。
func (t Tool) ExecTimeout() (time.Duration, error) {
	s := strings.TrimSpace(t.Timeout)
	if s == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("timeout=%q 非法（应为 Go duration，如 \"5m\"/\"300s\"）: %w", s, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("timeout=%q 必须为正时长", s)
	}
	return d, nil
}

// HealthProbe 探针类型。
const (
	ProbeKindHTTP = "http" // GET http://addr/path，状态码 < 400 视为健康
	ProbeKindTCP  = "tcp"  // 拨号 host:port 成功视为健康
	ProbeKindCmd  = "cmd"  // 以 sh -c 执行 Cmd，退出码 0 视为健康
)

// KnownProbeKinds 是合法探针类型（单一来源）。
var KnownProbeKinds = map[string]bool{
	ProbeKindHTTP: true,
	ProbeKindTCP:  true,
	ProbeKindCmd:  true,
}

// 探针默认值（缺省字段回退）。
const (
	defaultProbeInterval  = 5 * time.Second
	defaultProbeTimeout   = 2 * time.Second
	defaultProbeThreshold = 3
	defaultProbeAddr      = "127.0.0.1"
	defaultProbePath      = "/"
)

// HealthProbe 是 service 模式 Tool 的健康检查配置（REQ-APP-HEALTH / #75）。
// 字段用字符串表达时长（YAML 友好），由 Resolve 解析为运行期结构。
type HealthProbe struct {
	Kind      string `yaml:"kind" json:"kind"`           // http | tcp | cmd（必填）
	Addr      string `yaml:"addr" json:"addr"`           // http/tcp: host:port（缺省 127.0.0.1）
	Path      string `yaml:"path" json:"path"`           // http: 请求路径（缺省 /）
	Cmd       string `yaml:"cmd" json:"cmd"`             // cmd: 健康检查命令（sh -c 执行，退出 0 = 健康）
	Interval  string `yaml:"interval" json:"interval"`   // 探测间隔（缺省 5s）
	Timeout   string `yaml:"timeout" json:"timeout"`     // 单次探测超时（缺省 2s）
	Threshold int    `yaml:"threshold" json:"threshold"` // 连续失败几次触发重启（缺省 3）
}

// ResolvedProbe 是 HealthProbe 解析后的运行期结构（时长已为 time.Duration）。
type ResolvedProbe struct {
	Kind      string
	Addr      string
	Path      string
	Cmd       string
	Interval  time.Duration
	Timeout   time.Duration
	Threshold int
}

// Resolve 校验并解析 HealthProbe 为 ResolvedProbe（注册期即可调用，错误即拒绝，不留到运行期）。
// p 为 nil 时返回 (nil, nil)（表示无探针）。
func (p *HealthProbe) Resolve() (*ResolvedProbe, error) {
	if p == nil {
		return nil, nil
	}
	if !KnownProbeKinds[p.Kind] {
		return nil, fmt.Errorf("health_probe.kind=%q 非法（应为 http/tcp/cmd）", p.Kind)
	}
	interval := defaultProbeInterval
	if p.Interval != "" {
		d, err := time.ParseDuration(p.Interval)
		if err != nil {
			return nil, fmt.Errorf("health_probe.interval=%q 非法: %w", p.Interval, err)
		}
		interval = d
	}
	timeout := defaultProbeTimeout
	if p.Timeout != "" {
		d, err := time.ParseDuration(p.Timeout)
		if err != nil {
			return nil, fmt.Errorf("health_probe.timeout=%q 非法: %w", p.Timeout, err)
		}
		timeout = d
	}
	threshold := defaultProbeThreshold
	if p.Threshold > 0 {
		threshold = p.Threshold
	}
	addr := p.Addr
	if addr == "" {
		addr = defaultProbeAddr
	}
	switch p.Kind {
	case ProbeKindHTTP, ProbeKindTCP:
		if !strings.Contains(addr, ":") {
			return nil, fmt.Errorf("health_probe.addr=%q 缺少端口（http/tcp 需 host:port）", addr)
		}
	case ProbeKindCmd:
		if p.Cmd == "" {
			return nil, fmt.Errorf("health_probe.cmd 为空：cmd 探针需提供健康检查命令")
		}
	}
	path := p.Path
	if path == "" {
		path = defaultProbePath
	}
	return &ResolvedProbe{
		Kind:      p.Kind,
		Addr:      addr,
		Path:      path,
		Cmd:       p.Cmd,
		Interval:  interval,
		Timeout:   timeout,
		Threshold: threshold,
	}, nil
}

// ToolVersion 是 Tool 的一个版本条目。
// Entry / InputSchema / OutputSchema 为**可选**的版本级覆盖：钉版本（step.use_version）时，
// 若版本提供了这些字段则覆盖 Tool 级实现，否则回退 Tool 级（S5 灰度：同一 Tool 多版本并存）。
type ToolVersion struct {
	Version      string                 `yaml:"version" json:"version"`
	State        ToolVersionState       `yaml:"state" json:"state"`
	Digest       string                 `yaml:"digest" json:"digest"`
	Note         string                 `yaml:"note" json:"note"`
	CreatedAt    time.Time              `yaml:"created_at" json:"created_at"`
	Entry        string                 `yaml:"entry,omitempty" json:"entry,omitempty"`
	InputSchema  map[string]interface{} `yaml:"input_schema,omitempty" json:"input_schema,omitempty"`
	OutputSchema map[string]interface{} `yaml:"output_schema,omitempty" json:"output_schema,omitempty"`
}

// Step 是 Flow 内的一个执行单元。
//
// Call 支持两种写法（解析规则见 engine.resolveStepCall，已注册 Tool 优先）：
//   - 已注册 Tool 的 id（如 "translate_zh_en"）；
//   - 原子能力直调「能力.方法」（如 "http.Request" / "db.query" / "ext.ai.complete"）——
//     免去为用一次原语先注册一层 Tool 包装；此时 Input 即该方法的参数表，
//     输出统一为 {result: <返回值>}。
type Step struct {
	ID         string                 `yaml:"id" json:"id"`
	Call       string                 `yaml:"call" json:"call"`               // Tool id，或「能力.方法」直调
	UseVersion string                 `yaml:"use_version" json:"use_version"` // 可选：钉版本（仅 Tool 载体）
	Input      map[string]interface{} `yaml:"input" json:"input"`
	DependsOn  []string               `yaml:"depends_on" json:"depends_on"` // 上游 step id
	When       string                 `yaml:"when" json:"when"`             // 条件执行（S1 后）
	Parallel   bool                   `yaml:"parallel" json:"parallel"`     // 是否并行（S4）
	Mode       string                 `yaml:"mode" json:"mode"`             // once（默认，执行完返回）/ service（长驻，拉起不等待；仅 Tool 载体）
}

// StepMode 是 Step 的执行模式。
type StepMode string

const (
	// ModeOnce 一次性执行：进程退出后捕获 stdout 末行 JSON 作为 output（默认）。
	ModeOnce StepMode = "once"
	// ModeService 长驻执行：拉起进程不等待，Flow 借此成为「服务」（见 PRD §14.2）。
	ModeService StepMode = "service"
)

// IsService 报告该 step 是否为长驻/服务模式（空 mode 视为 once）。
func (s Step) IsService() bool { return StepMode(s.Mode) == ModeService }

// Flow 是通用运行单元：声明式编排；长驻不退出的 Flow 即"服务"。
type Flow struct {
	ID          string                 `yaml:"id" json:"id"`
	Name        string                 `yaml:"name" json:"name"`
	Description string                 `yaml:"description" json:"description"`
	Vars        map[string]interface{} `yaml:"vars" json:"vars"`
	Steps       []Step                 `yaml:"steps" json:"steps"`
}

// HookCriticality 决定 Hook 失败时的处置。
type HookCriticality string

const (
	CritCritical   HookCriticality = "critical"    // 失败即终止主流程（认证/脱敏/限流）
	CritBestEffort HookCriticality = "best-effort" // 失败仅记 stderr，主流程继续（日志/指标）
)

// Hook 阶段（PRD §5.3）。
const (
	StageBeforeFlow    = "before_flow"    // Flow 开始执行前（认证/参数审计/初始化）：可改写 Flow 初始 vars
	StageBeforeStep    = "before_step"    // 步骤执行前：可改写 input
	StageAfterStep     = "after_step"     // 步骤执行后：可改写 output
	StageOnError       = "on_error"       // 步骤失败时：仅补偿/告警，不能恢复主流程
	StageOnSuccess     = "on_success"     // Flow 全部成功时（汇总通知/收尾）：仅副作用，不改主结果
	StageBeforePromote = "before_promote" // 版本晋升前（S5 由 PromoteTool 触发）
)

// FlowLevelStages 是「Flow 级」Hook 阶段：匹配维度是 flow id（而非 tool id）。
// run() 在 Flow 级挂载它们时按 flow.ID 过滤 Target；step 级 Hook 则按 tool.ID 过滤。
var FlowLevelStages = map[string]bool{
	StageBeforeFlow: true,
	StageOnSuccess:  true,
}

// IsFlowLevelStage 报告 stage 是否为 Flow 级（按 flow id 匹配 Target）。
func IsFlowLevelStage(stage string) bool { return FlowLevelStages[stage] }

// KnownHookStages 是当前**已实现**的 Hook 阶段（S5 起 before_promote 由 PromoteTool 触发；
// #72 起 before_flow/on_success 在 once 模式 Flow 上生效）。
var KnownHookStages = map[string]bool{
	StageBeforeFlow:    true,
	StageBeforeStep:    true,
	StageAfterStep:     true,
	StageOnError:       true,
	StageOnSuccess:     true,
	StageBeforePromote: true,
}

// Hook 是横切控制（认证/脱敏/限流/日志/汇总），由宿主策略显式装配（不通过插件间私下约定耦合）。
//
// Action 指向**已注册的 Tool id 或 Flow id**：
//   - Tool 载体：Hook 逻辑本身是一个 Tool（process / lua 均可写），引擎直接调用其 runtime 执行，
//     不经 step 流程 —— 因此 Hook 执行**不会**再触发 Hook（天然无递归）。
//   - Flow 载体（#72）：Action 指向一个 Flow id，引擎**同步内联**执行该子 Flow（同 execID 上下文，
//     结构性无递归），before_flow 时其 RunResult 暴露给主 Flow 的 vars（key=子 Flow id）供下游引用。
//
// Target 匹配维度由 Stage 决定：step 级 stage（before_step/after_step/on_error）按 step 的 tool id 匹配，
// Flow 级 stage（before_flow/on_success）按 Flow id 匹配；两者均支持 `*` 表示「全部」。
type Hook struct {
	ID          string          `yaml:"id" json:"id"`
	Target      string          `yaml:"target" json:"target"` // 匹配对象：step 级按 tool id、flow 级按 flow id；"*" 表示全部
	Stage       string          `yaml:"stage" json:"stage"`   // before_flow / before_step / after_step / on_error / on_success / before_promote
	Action      string          `yaml:"action" json:"action"` // 已注册 Tool 的 id 或 Flow 的 id
	Criticality HookCriticality `yaml:"criticality" json:"criticality"`
	Order       int             `yaml:"order" json:"order"` // 装配顺序（weight，升序执行）
}

// EffectiveCriticality 返回生效的 criticality：空值按 critical 处理。
// 依据用户决策「Hook 失败即终止编排（继续运行可能和用户意图不符）」——默认 fail-closed，
// 需要「失败不影响主流程」的日志/指标类 Hook 必须显式声明 best-effort。
func (h Hook) EffectiveCriticality() HookCriticality {
	if h.Criticality == "" {
		return CritCritical
	}
	return h.Criticality
}
