package model

import (
	"fmt"
	"regexp"
	"strings"

	"gear/internal/cap"
)

// semverRegex 匹配 SemVer：x.y.z，可选前置 v、预发布与构建元数据。
// 参考 https://semver.org ：主版本.次版本.修订号 三段数字为必填。
var semverRegex = regexp.MustCompile(`^v?(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-((?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*)(?:\.(?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*))*))?(?:\+([0-9a-zA-Z-]+(?:\.[0-9a-zA-Z-]+)*))?$`)

// ParseSemVer 校验 s 是否合法 SemVer，非法返回错误。
func ParseSemVer(s string) error {
	if !semverRegex.MatchString(s) {
		return fmt.Errorf("非法的 SemVer 版本: %q（应为 x.y.z，可带前置 v 与预发布/构建元数据）", s)
	}
	return nil
}

// ValidStates 是合法的 ToolVersionState 集合（PRD §7 状态机）。
var ValidStates = map[ToolVersionState]bool{
	StateDraft:      true,
	StateTesting:    true,
	StateStable:     true,
	StateDeprecated: true,
	StateRejected:   true,
}

// VersionTransitions 定义版本状态机的合法迁移（S5 灰度）。
// key 为当前状态，value 为允许迁移到的目标状态集合。任何不在此表中的迁移均非法。
//
// 设计：灰度典型路径 draft→testing→stable；stable 可降级回 testing 复测、或 deprecated 后
// 再 stable（复活）；rejected 为终态，仅可重开为 draft。非法迁移（如 stable→draft 直接跳级、
// testing→deprecated 跳过 stable）一律拒绝，杜绝状态混乱。
var VersionTransitions = map[ToolVersionState][]ToolVersionState{
	StateDraft:      {StateTesting, StateStable, StateRejected},
	StateTesting:    {StateStable, StateRejected, StateDraft},
	StateStable:     {StateDeprecated, StateTesting, StateRejected},
	StateDeprecated: {StateStable, StateRejected},
	StateRejected:   {StateDraft},
}

// CanPromote 报告从 from 迁移到 to 是否为合法状态迁移。
func CanPromote(from, to ToolVersionState) bool {
	for _, t := range VersionTransitions[from] {
		if t == to {
			return true
		}
	}
	return false
}

// Validate 校验单个 ToolVersion：version 必须合法 SemVer，state 必须枚举值。
func (v ToolVersion) Validate() error {
	if err := ParseSemVer(v.Version); err != nil {
		return err
	}
	if !ValidStates[v.State] {
		return fmt.Errorf("非法的版本状态: %q（应为 draft/testing/stable/deprecated/rejected）", v.State)
	}
	return nil
}

// Validate 校验 Tool 级不变量：
//   - id 非空
//   - runtime 必须合法且**已实现**（F3：未知/未实现 runtime 注册期拒绝，不推迟到执行期）
//   - 至少一个 version，且每个 version 合法（D2）
//   - input/output schema 存在且 type 为 object（D5 最小真实校验，完整 JSON Schema 校验留 S1）
func (t Tool) Validate() error {
	if t.ID == "" {
		return fmt.Errorf("tool.id 不能为空")
	}
	if !t.Runtime.Known() {
		return fmt.Errorf("tool %s 的 runtime=%q 非法（应为 process/lua/go-host/wasm）", t.ID, t.Runtime)
	}
	if !t.Runtime.Implemented() {
		return fmt.Errorf("tool %s 的 runtime=%q 尚未实现（已实现：process / lua；go-host 计划后续，wasm 计划 V3）；请勿注册跑不起来的 Tool", t.ID, t.Runtime)
	}
	if len(t.Versions) == 0 {
		return fmt.Errorf("tool %s 至少要有一个 version", t.ID)
	}
	for i, v := range t.Versions {
		if err := v.Validate(); err != nil {
			return fmt.Errorf("tool %s 第 %d 个 version 校验失败: %w", t.ID, i, err)
		}
	}
	// capability 精确放行：注册期即拒绝未知原语，杜绝「文档说有、代码无」的漂移（红线 15）。
	if _, err := cap.GrantsFromStrings(t.Capabilities); err != nil {
		return fmt.Errorf("tool %s: %w", t.ID, err)
	}
	if err := validateSchema(t.InputSchema, "input_schema"); err != nil {
		return err
	}
	if err := validateSchema(t.OutputSchema, "output_schema"); err != nil {
		return err
	}
	// 健康检查探针（REQ-APP-HEALTH / #75）：注册期即解析校验，杜绝运行期才发现配置非法。
	if t.HealthProbe != nil {
		if _, err := t.HealthProbe.Resolve(); err != nil {
			return fmt.Errorf("tool %s 的 health_probe: %w", t.ID, err)
		}
	}
	return nil
}

func validateSchema(s map[string]interface{}, field string) error {
	if s == nil {
		return fmt.Errorf("%s 不能为空", field)
	}
	if t, ok := s["type"]; !ok || fmt.Sprint(t) != "object" {
		return fmt.Errorf("%s.type 必须是 object", field)
	}
	return nil
}

// Hook.Validate 校验 Hook 级不变量（F3 严格守卫）：
//   - id / target / action 非空（action 指向已注册的 Tool id 或 Flow id）
//   - stage 必须是已实现阶段（before_step/after_step/on_error/before_promote 自 S5；
//     before_flow/on_success 自 #72）
//   - criticality 为空（按 critical 处理）或必须是 critical / best-effort
//
// 注：Action 指向的 Tool / Flow 是否存在**不在注册期校验**——Hook 与 Tool/Flow 独立注册，
// 存在性在运行期 aplicar 时解析（Tool 缺失按 criticality 处置，Flow 缺失报错）；此处只校验
// 形态不变量，避免引入「跨实体存在性」的注册期耦合。
func (h Hook) Validate() error {
	if h.ID == "" {
		return fmt.Errorf("hook.id 不能为空")
	}
	if h.Target == "" {
		return fmt.Errorf("hook %s 的 target 不能为空（应为 tool id / flow id 或 \"*\"）", h.ID)
	}
	if h.Action == "" {
		return fmt.Errorf("hook %s 的 action 不能为空（应指向已注册的 Tool id 或 Flow id）", h.ID)
	}
	if !KnownHookStages[h.Stage] {
		return fmt.Errorf("hook %s 的 stage=%q 非法（应为 before_flow / before_step / after_step / on_error / on_success / before_promote）", h.ID, h.Stage)
	}
	switch h.Criticality {
	case "", CritCritical, CritBestEffort:
	default:
		return fmt.Errorf("hook %s 的 criticality=%q 非法（应为 critical / best-effort，留空默认 critical）", h.ID, h.Criticality)
	}
	return nil
}

// Flow.Validate 校验 Flow 级不变量（F3 严格模型守卫）：
//   - id 非空、至少一个 step、每个 step 的 call 非空
//   - depends_on 引用的 step 必须存在
//   - mode 必须是 once / service 之一（未知 mode 显式拒绝，不静默降级）
//   - 不支持的字段显式报错，而非静默忽略（when/parallel 已分别由 S1/S4 实现；
//     use_version 由 S5 实现，此处放行并在 ValidateFlowStatic 做版本存在性校验）。
//
// 显式拒绝能把「文档说有、代码无」的漂移变成注册期可见错误，而非悄悄 no-op（RC-D）。
func (f Flow) Validate() error {
	if f.ID == "" {
		return fmt.Errorf("flow.id 不能为空")
	}
	if len(f.Steps) == 0 {
		return fmt.Errorf("flow %s 至少需要一个 step", f.ID)
	}
	ids := make(map[string]bool, len(f.Steps))
	for _, s := range f.Steps {
		ids[s.ID] = true
	}
	for i, s := range f.Steps {
		if s.ID == "" {
			return fmt.Errorf("flow %s 第 %d 个 step 缺少 id", f.ID, i)
		}
		if s.Call == "" {
			return fmt.Errorf("step %s 的 call 不能为空（需指向已注册的 Tool）", s.ID)
		}
		if s.Mode != "" && StepMode(s.Mode) != ModeOnce && StepMode(s.Mode) != ModeService {
			return fmt.Errorf("step %s 的 mode=%q 非法（应为 once 或 service）", s.ID, s.Mode)
		}
		// when 已实现（表达式引擎）：它是**表达式**，不是模板——
		// 用 ${} 包裹会丢类型（"200" vs 200），故注册期即拒绝并教正确写法。
		if strings.Contains(s.When, "${") {
			return fmt.Errorf("step %s 的 when 是表达式、不使用 ${} 模板语法：去掉 ${} 直接写变量名，如 `steps.s1.output.n > 1`", s.ID)
		}
		// S4：无依赖的 step **默认并行**（PRD §4.3），故 `parallel` 只是显式标注，不改变语义。
		// 若某 step 已声明 depends_on 却仍标 parallel，属误导性标注，显式拒绝（RC-D）。
		if s.Parallel && len(s.DependsOn) > 0 {
			return fmt.Errorf("step %s 标记了 parallel 但同时声明了 depends_on：无依赖 step 本就并行调度，该标注在此处无意义且易误导", s.ID)
		}
		for _, dep := range s.DependsOn {
			if !ids[dep] {
				return fmt.Errorf("step %s 依赖不存在的 step: %s", s.ID, dep)
			}
		}
	}
	return nil
}
