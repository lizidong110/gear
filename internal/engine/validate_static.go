package engine

import (
	"fmt"
	"strings"

	"gear/internal/model"
)

// ValidateFlowStatic 对 Flow 做**静态校验**（不执行任何 step）。
//
// 校验层次（PRD §9.2「引擎静态校验」）：
//  1. 结构不变量（`model.Flow.Validate`：id/step/call/mode/未实现字段）
//  2. 依赖图：有效依赖（显式 + 推断）无环、无未知/自引用
//  3. step 的调用目标必须可解析：已注册 Tool，或「能力.方法」直调的原子能力
//     （解析规则与运行期共用 resolveStepCall，只有一处真相）；直调还额外校验
//     input 的参数名与必填项（把拼错参数名这类错误挡在注册那一刻）
//  4. `when` 表达式必须可编译
//  5. 模板引用：前缀必须是已实现的 vars/steps、vars 键存在、`${steps…}` 必须带 step id
//     （模板语法与其边界见 template.go）
//
// 供 CLI `flow register/validate`、REST `POST /api/flows`、以及 AI 生成闭环的
// 「生成 → 校验 → 修正」循环复用——共用同一份校验，避免规则漂移（§0 单一真相源）。
//
// 每条错误都挂上结构化诊断（diag.go）：签名与错误文本都不变，但 MCP 面的 gear_validate
// 可以用 errors.As 取出 code/step/field/hint，让智能体靠码自愈而不是靠猜。
//
// 本函数是 `ValidateFlowStaticDiags` 的**首条**——两者共用同一份判定，不存在第二套规则。
// 保留 fail-fast 语义是因为既有调用方（CLI / REST / AI 闭环）只关心「过不过 + 为什么不过」。
func (e *Engine) ValidateFlowStatic(f *model.Flow) error {
	if diags := e.ValidateFlowStaticDiags(f); len(diags) > 0 {
		return &DiagError{Diag: diags[0]}
	}
	return nil
}

// ValidateFlowStaticDiags 收集**全部**可报告的静态问题（不 fail-fast）。
//
// 为什么需要「一次报多条」：`gear_validate` 是智能体的「编译器」——一次只告诉他一个错，
// 他就得改一处、重跑一次，5 处错误要 5 轮往返。这里把能独立判定的问题一次收齐，
// 把「编译」的收益做实（MCP_AGENT_DESIGN.md §6.2②）。
//
// 唯一仍然 fail-fast 的是**整体性**不变量（缺 id / 一个 step 都没有）：缺了它们后续判定都
// 无意义，硬报会产出一堆派生噪声。这是刻意的取舍，不是遗漏。
// 反过来，「某个 step 的 when 用了 ${}」「某个 step 的 input 模板前缀非法」这类**局部**问题
// **不再**整体短接——否则第 1 个 step 的小毛病会把后面 step 的问题全部掩盖（见 validateFlowDiags ③）。
func (e *Engine) ValidateFlowStaticDiags(f *model.Flow) []Diagnostic {
	return e.validateFlowDiags(f, nil)
}

// ValidateFlowStaticDiagsWith 在「bundle 内尚未落库的 Tool」也可解析的前提下校验 Flow。
//
// 这是「先校验后落库」能成立的关键：一个 bundle 里的 Tool 与 Flow 互相引用，而校验发生在
// 落库之前——pending 就是那些「本次一起提交、还没进 registry」的 Tool。
// 校验规则与不带 pending 时完全同一份（见 resolveStepCallView）。
func (e *Engine) ValidateFlowStaticDiagsWith(f *model.Flow, pending []*model.Tool) []Diagnostic {
	if len(pending) == 0 {
		return e.validateFlowDiags(f, nil)
	}
	extra := make(map[string]*model.Tool, len(pending))
	for _, t := range pending {
		if t != nil && t.ID != "" {
			extra[t.ID] = t
		}
	}
	return e.validateFlowDiags(f, extra)
}

// flowStructureHint 是 Flow 结构不变量类诊断的统一「怎么修」。
const flowStructureHint = "Flow 结构不变量：id 非空、至少一个 step、每个 step 有 id 与 call、mode ∈ {once, service}"

// whenHint 是 when 误用模板语法时的「怎么修」（与 model 层那条拒绝同源）。
const whenHint = "when 是表达式、不是模板：不要写 ${}，直接写变量名，如 `steps.s1.output.n > 1`"

func (e *Engine) validateFlowDiags(f *model.Flow, extra map[string]*model.Tool) []Diagnostic {
	// ① 整体不变量（缺 id / 一个 step 都没有）：后续一切判定都无意义 → 仍然 fail-fast。
	//    硬报只会产出一堆派生噪声，这是刻意的取舍。
	if f.ID == "" || len(f.Steps) == 0 {
		err := f.Validate()
		return Diagnostics(diagWrap(flowStructureCode(err), err, "", "", flowStructureHint))
	}

	var out []Diagnostic
	// usesServe 是整条 Flow 级的事实（④ 要用），在 ③ 逐 step 的循环里顺带累积。
	var usesServe bool
	collect := func(err error) {
		if err == nil {
			return
		}
		out = append(out, Diagnostics(err)...)
	}

	// ② 依赖图是**跨 step** 的事实，只能在整条 Flow 上判定，故单独一次。
	collect(diagWrap(CodeBadDependency, chainTopoErr(f.Steps), "", "steps",
		"依赖图必须无环：检查 depends_on，以及 ${steps.<id>.output.*} 自动推断出来的隐式依赖"))

	// ③ 逐 step 收集：结构不变量 → 模板 → when → 调用目标 → 直调入参。
	//
	//    为什么层层「单 step 化」：结构/模板/when 都是 **step 局部**的事实，而
	//    model.Flow.Validate 与 validateTemplates 都是 fail-fast 的。直接拿它们的结果做整体
	//    判定，「第 1 个 step 的 when 多写了 ${}」就会把后面几个 step 的问题全部掩盖——智能体
	//    得改一处、重跑一次，而 gear_validate 的全部价值恰恰是「一次报全」。
	//    复用同一份判定函数（不在这里重写 mode/when/parallel/template 规则）是硬约束。
	for i, s := range f.Steps {
		field := fmt.Sprintf("steps[%d]", i)
		if err := singleStepStructureErr(f, s); err != nil {
			collect(diagWrap(flowStructureCode(err), err, s.ID, field, structureHintFor(err)))
			continue // 结构都不成立时，后续判定只会产噪声
		}
		// 顺带记下这条 Flow 是否要用 ext.serve 侧车（整条 Flow 级判定，见 ④）。
		//
		// **刻意放在解析调用目标之前**：调用目标解析失败（UNKNOWN_CALL_TARGET /
		// UNKNOWN_CAPABILITY）会 continue，若把本判定跟在解析之后，站点 Tool 还没落库时
		// 就只剩一条 UNKNOWN_CALL_TARGET，而「压根没有步持有侧车」这个更符合病因的问题
		// 被它掩盖——那正是初始实现被真机打回的原因。
		if stepUsesServeHelp(s.Call, extra) {
			usesServe = true
		}
		collect(diagWrap(CodeBadTemplate, singleStepTemplateErr(f, s), s.ID, field+".input",
			"模板只允许三种前缀：${vars.x} / ${steps.<id>.output.x} / ${secrets.NAME}"))
		if _, err := compileWhen(s.When); err != nil {
			// 保留 "step %s: " 前缀：错误文本与引入诊断前逐字一致（既有测试与日志依赖它）。
			collect(diagWrap(CodeBadWhen, fmt.Errorf("step %s: %w", s.ID, err), s.ID, "step("+s.ID+").when", whenHint))
		}
		call, err := e.resolveStepCallView(s, extra)
		if err != nil {
			collect(diagWrap(CodeUnknownCallTarget, err, s.ID, "step("+s.ID+").call",
				"先用 gear_catalog(kind=tool) 确认 Tool 是否存在；或改用「能力.方法」直调（清单见 gear_capabilities）。"+
					"若确认该能力不存在 → 走原语缺口上报（gear_guide(\"primitive-gap\")），不要绕过门禁"))
			continue
		}
		if !call.isPrimitive() {
			continue
		}
		// 直调原子能力：参数校验。input 里的值可能是模板串（如 ${vars.k}），
		// 但**键名**此刻已确定，足以查出「拼错参数名 / 漏必填」——这正是最常见的两类误写。
		collect(diagWrap(CodeBadPrimitiveArgs, wrapStepErr(s.ID, e.validatePrimitiveArgs(call.prim, call.method, s.Input)),
			s.ID, "step("+s.ID+").input", "参数名与必填项以 gear_capabilities 里该能力的方法 schema 为准"))
	}

	// ④ 侧车持有不变量（整条 Flow 级事实，只能在整条 Flow 上判定，故单独一次）。
	//
	// ext.serve 是唯一一个「缺了则这个应用对外根本不存在」的侧车：没有一条 `mode: service`
	// 的 `ext.serve` 步去持有它，任何要用它的步都会拿到 connection refused。这类定义能写入、
	// 能 gear_apply，只在真起服时才炸——而校验期「一次报全」的全部价值，就是把这次炸提前。
	if usesServe && !hasServiceServeStep(f) {
		collect(diagWrap(CodeMissingServeHolder,
			fmt.Errorf("Flow %s 要用 ext.serve 侧车，但没有一条 `mode: service` 的 `ext.serve` 步持有它", f.ID),
			"", "steps", missingServeHolderHint))
	}
	return out
}

// missingServeHolderHint 是 CodeMissingServeHolder 的「怎么修」。
const missingServeHolderHint = "加一条 `call: ext.serve` + `mode: service` 的步持有侧车；service step 不能作为依赖目标，" +
	"故用一个 `mode: once` 的锚点步把它后面的站点步推到下一层（见 examples/agent-app/bundle.yaml）"

// stepUsesServeHelp 报告这个 step 的调用目标是否要用 ext.serve 侧车：
// 直调 `ext.serve`，或调用目标（bundle 内 Tool 优先于已注册 Tool）声明了 ext.serve capability。
//
// 只查 bundle 内的 extra：一个尚未落库、也不在本 bundle 里的 Tool，无从判断它的能力——
// 那种情形会由 UNKNOWN_CALL_TARGET 单独报出，不在本检查里重复猜。
func stepUsesServeHelp(callName string, extra map[string]*model.Tool) bool {
	if callName == "ext.serve" || strings.HasPrefix(callName, "ext.serve.") {
		return true
	}
	t := extra[callName]
	if t == nil {
		return false
	}
	for _, c := range t.Capabilities {
		if c == "ext.serve" {
			return true
		}
	}
	return false
}

// hasServiceServeStep 报告这条 Flow 里有没有一条 `mode: service` 的 `ext.serve` 步持有侧车。
func hasServiceServeStep(f *model.Flow) bool {
	for _, s := range f.Steps {
		if s.IsService() && s.Call == "ext.serve" {
			return true
		}
	}
	return false
}

// singleStepStructureErr 取出某个 step **局部**的结构错误。
//
// 用「只含这一个 step 的 Flow」跑 model.Flow.Validate —— 规则只有一份，不在 engine 里重写
// mode / when / parallel 的判定。唯一的例外是「依赖不存在的 step」：单 step 视图必然误报
// （它看不到兄弟 step），而这条本身由 chainTopoErr 在整条 Flow 上负责，故此处排除。
func singleStepStructureErr(f *model.Flow, s model.Step) error {
	err := model.Flow{ID: f.ID, Steps: []model.Step{s}}.Validate()
	if err == nil || strings.Contains(err.Error(), "依赖不存在的 step") {
		return nil
	}
	return err
}

// singleStepTemplateErr 同款：只在「这一个 step」上跑模板校验。
// Vars 仍取整条 Flow 的——vars 是 Flow 级事实，不是 step 局部事实。
func singleStepTemplateErr(f *model.Flow, s model.Step) error {
	return validateTemplates(&model.Flow{ID: f.ID, Vars: f.Vars, Steps: []model.Step{s}})
}

// structureHintFor 按结构错误的具体类型给更贴切的「怎么修」。
func structureHintFor(err error) string {
	if flowStructureCode(err) == CodeBadWhen {
		return whenHint
	}
	return flowStructureHint
}

// chainTopoErr 把依赖图校验包成 error（保持「无错则 nil」的形态，供 collect 直接使用）。
func chainTopoErr(steps []model.Step) error {
	if _, err := topoLevels(steps); err != nil {
		return err
	}
	return nil
}

// wrapStepErr 给 step 级错误加 "step <id>: " 前缀（err 为 nil 时返回 nil）。
func wrapStepErr(stepID string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("step %s: %w", stepID, err)
}

// flowStructureCode 把 model.Flow.Validate 的结构性错误细分到更精确的码。
//
// 为什么在这里做文本匹配：错误产生点在 model 层（model 不依赖 engine 的诊断类型，保持其
// 零依赖），而 model 的每条消息都是固定字面量并有测试锁定。这里只做「同一份事实的再分类」，
// 不改变任何判定——分类偏保守最坏是码不够精确，绝不会因此放过一个非法输入。
func flowStructureCode(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "when 是表达式"):
		return CodeBadWhen
	case strings.Contains(msg, "依赖不存在的 step"):
		return CodeBadDependency
	default:
		return CodeBadFlowStructure
	}
}
