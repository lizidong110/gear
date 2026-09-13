// Package model 定义 Gear 内核的核心数据模型。
// 这些结构体是 Tool / Flow / Hook 在内存与磁盘(YAML/SQLite)之间的唯一真相源。
package model

import "time"

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
}

// ToolVersion 是 Tool 的一个版本条目。
type ToolVersion struct {
	Version   string           `yaml:"version" json:"version"`
	State     ToolVersionState `yaml:"state" json:"state"`
	Digest    string           `yaml:"digest" json:"digest"`
	Note      string           `yaml:"note" json:"note"`
	CreatedAt time.Time        `yaml:"created_at" json:"created_at"`
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

// KnownHookStages 是当前**已实现**的 Hook 阶段（before_promote 计划 S5，注册期拒绝）。
var KnownHookStages = map[string]bool{
	StageBeforeStep: true,
	StageAfterStep:  true,
	StageOnError:    true,
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
