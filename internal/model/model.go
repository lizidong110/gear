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
// 新增 runtime（如 S7 的 lua）只需：在此登记 + 在 engine.runtimeFor 加一个 case。
// 注册期校验、引擎分发、文档状态均以本表为唯一依据，避免多处列表漂移。
var ImplementedRuntimes = map[Runtime]bool{
	RuntimeProcess: true,
	RuntimeLua:     true, // S7a：嵌入式 Lua（沙箱，once 可用；service 不支持）
}

// Known 报告 r 是否为合法枚举值。
func (r Runtime) Known() bool { return KnownRuntimes[r] }

// Implemented 报告 r 是否已有引擎实现（once 路径可用）。
// 已知但未实现者（go-host/wasm）注册期应被拒绝。
// 注意：service 能力另见 luaRuntime.Start 的显式错误（lua 已实现但不支持 service）。
func (r Runtime) Implemented() bool { return ImplementedRuntimes[r] }

// ToolVersionState 是 Tool 版本的生命周期状态。
type ToolVersionState string

const (
	StateDraft     ToolVersionState = "draft"     // 草稿，未晋升
	StateTesting   ToolVersionState = "testing"   // 测试中（灰度）
	StateStable    ToolVersionState = "stable"    // 稳定，默认调用
	StateDeprecated ToolVersionState = "deprecated" // 已弃用，仍可调用
	StateRejected  ToolVersionState = "rejected"  // 已否决，不可调用
)

// Tool 是原子能力，带 input/output schema，可被 Flow 内 step 调用。
type Tool struct {
	ID          string                 `yaml:"id" json:"id"`
	Name        string                 `yaml:"name" json:"name"`
	Description string                 `yaml:"description" json:"description"`
	Runtime     Runtime                `yaml:"runtime" json:"runtime"`
	Entry       string                 `yaml:"entry" json:"entry"` // process: 命令；lua/go-host: 函数引用
	InputSchema map[string]interface{} `yaml:"input_schema" json:"input_schema"`
	OutputSchema map[string]interface{} `yaml:"output_schema" json:"output_schema"`
	Versions    []ToolVersion          `yaml:"versions" json:"versions"`
	// Capabilities 声明该 Tool 被允许使用的宿主能力原语（PRD §6，最小权限）。
	// 对应 ctx.* 的 http/call/db/kv/secret/event；运行期未声明的原语调用一律拒绝。
	Capabilities []string `yaml:"capabilities" json:"capabilities"`
	// HealthProbe 是 service 模式 Tool 的健康检查探针（REQ-APP-HEALTH / #75）。
	// 仅当该 Tool 被 service step 调用时生效：进程「活着」≠「健康」，探针按阈值
	// 连续失败后由 Supervisor 杀掉并重启（与崩溃重启共用退避）。once 工具忽略。
	HealthProbe *HealthProbe `yaml:"health_probe,omitempty" json:"health_probe,omitempty"`
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
	Kind     string `yaml:"kind" json:"kind"`         // http | tcp | cmd（必填）
	Addr     string `yaml:"addr" json:"addr"`         // http/tcp: host:port（缺省 127.0.0.1）
	Path     string `yaml:"path" json:"path"`         // http: 请求路径（缺省 /）
	Cmd      string `yaml:"cmd" json:"cmd"`           // cmd: 健康检查命令（sh -c 执行，退出 0 = 健康）
	Interval string `yaml:"interval" json:"interval"` // 探测间隔（缺省 5s）
	Timeout  string `yaml:"timeout" json:"timeout"`   // 单次探测超时（缺省 2s）
	Threshold int   `yaml:"threshold" json:"threshold"` // 连续失败几次触发重启（缺省 3）
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
	Version       string                 `yaml:"version" json:"version"`
	State         ToolVersionState       `yaml:"state" json:"state"`
	Digest        string                 `yaml:"digest" json:"digest"`
	Note          string                 `yaml:"note" json:"note"`
	CreatedAt     time.Time              `yaml:"created_at" json:"created_at"`
	Entry         string                 `yaml:"entry,omitempty" json:"entry,omitempty"`
	InputSchema   map[string]interface{} `yaml:"input_schema,omitempty" json:"input_schema,omitempty"`
	OutputSchema  map[string]interface{} `yaml:"output_schema,omitempty" json:"output_schema,omitempty"`
}

// Step 是 Flow 内的一个执行单元。
type Step struct {
	ID         string                 `yaml:"id" json:"id"`
	Call       string                 `yaml:"call" json:"call"`               // 被调用的 Tool id
	UseVersion string                 `yaml:"use_version" json:"use_version"` // 可选：钉版本
	Input      map[string]interface{} `yaml:"input" json:"input"`
	DependsOn  []string               `yaml:"depends_on" json:"depends_on"` // 上游 step id
	When       string                 `yaml:"when" json:"when"`             // 条件执行（S1 后）
	Parallel   bool                   `yaml:"parallel" json:"parallel"`     // 是否并行（S4）
	Mode       string                 `yaml:"mode" json:"mode"`             // once（默认，执行完返回）/ service（长驻，拉起不等待）
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
	CritCritical    HookCriticality = "critical"    // 失败即终止主流程（认证/脱敏/限流）
	CritBestEffort HookCriticality = "best-effort" // 失败仅记 stderr，主流程继续（日志/指标）
)

// Hook 阶段（PRD §5.3）。
const (
	StageBeforeStep   = "before_step"    // 步骤执行前：可改写 input
	StageAfterStep    = "after_step"     // 步骤执行后：可改写 output
	StageOnError      = "on_error"       // 步骤失败时：仅补偿/告警，不能恢复主流程
	StageBeforePromote = "before_promote" // 版本晋升前（deferred：S5）
)

// KnownHookStages 是当前**已实现**的 Hook 阶段（S5 起 before_promote 由 PromoteTool 触发）。
var KnownHookStages = map[string]bool{
	StageBeforeStep:    true,
	StageAfterStep:     true,
	StageOnError:       true,
	StageBeforePromote: true,
}

// Hook 是横切控制（认证/脱敏/限流/日志），由宿主策略显式装配（不通过插件间私下约定耦合）。
//
// Action 指向**已注册的 Tool id**：Hook 逻辑本身就是一个 Tool（process / lua 均可写），
// 由引擎直接调用其 runtime 执行 —— 因此 Hook 执行**不会**再触发 Hook（天然无递归）。
type Hook struct {
	ID         string           `yaml:"id" json:"id"`
	Target     string           `yaml:"target" json:"target"`     // 作用对象：tool id 或 "*"
	Stage      string           `yaml:"stage" json:"stage"`       // before_step / after_step / on_error
	Action     string           `yaml:"action" json:"action"`     // 已注册 Tool 的 id
	Criticality HookCriticality `yaml:"criticality" json:"criticality"`
	Order      int              `yaml:"order" json:"order"`       // 装配顺序（weight，升序执行）
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
