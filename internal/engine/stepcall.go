package engine

import (
	"fmt"
	"sort"
	"strings"

	"gear/internal/cap"
	"gear/internal/model"
)

// ---- step 调用目标的解析与执行（Tool 载体 / 原子能力直调载体）----
//
// 一个 step 的 `call` 有两种互斥载体：
//
//  1. **Tool 载体**：`call` 是已注册 Tool 的 id —— 用 Tool 自身的 runtime/entry/schema。
//  2. **原子能力直调**：`call` 写「能力.方法」（如 `db.query`、`db.exec`、`ext.ai.complete`）——
//     引擎把它编成一段 Lua entry 在沙箱里执行，宿主视图**只放行该能力**（书写即授权）。
//     能力名可以是**两级**的 `ext.<name>`（扩展命名空间），故 `ext.ai.complete` 应切成
//     能力 `ext.ai` + 方法 `complete`——切分只认 `splitToolMethod`（见其注释），
//     别在别处用一刀 `strings.Cut(call, ".")`（会把能力名切错成 `ext`）。
//     可直调的目标有三类：内核原语（http/call/db/secret/event/subscribe）、
//     **纯函数核心原子**（time/template/crypto——始终可用，无需声明 capabilities，见 purefunc.go）、
//     以及 `ext.*` 扩展。三类都按方法级 schema 做静态校验（validatePrimitiveArgs）。
//
// 解析规则必须**只有一处**：运行期 `execLevel` 与注册期 `ValidateFlowStatic` 共用
// `resolveStepCall`，否则「什么算合法调用目标」会长出第二份判定并漂移（G2/M10 教训）。
// 入参校验（`validatePrimitiveArgs`）与 Lua entry 生成（ai_agent.go 的 `buildActionEntry`）
// 也被 POST /api/capabilities/invoke 与 ext.ai.react 复用，故两条入口的严格度天然一致。

// resolvedCall 是一次 step 解析后的「可执行调用目标」。
type resolvedCall struct {
	tool   *model.Tool // 非 nil：已注册 Tool 载体
	prim   string      // 非空：原语/能力名（http/call/db/kv/secret/event/subscribe 或 ext.*）
	method string      // 非空：方法名（Request/query/put/get/complete...）
}

// isPrimitive 报告这是否是「能力.方法」直调载体（与 tool 载体互斥）。
func (c resolvedCall) isPrimitive() bool { return c.prim != "" }

// id 返回该调用目标的标识，用于 Hook 的 target 匹配与「运行中不可注销」上报：
//   - Tool 载体 → tool.ID；
//   - 原语载体 → "能力.方法"，与 step.Call 的字面量**完全同形**
//     （因此 plannedToolIDs 原样上报 step.Call 与本函数结果永远一致，不会出现第二套命名）。
func (c resolvedCall) id() string {
	if c.tool != nil {
		return c.tool.ID
	}
	return c.prim + "." + c.method
}

// resolveStepCall 把 step.Call 解析成 resolvedCall：先按已注册 Tool，再按「能力.方法」直调兜底。
// 两者都不是 → fail-closed 报错（绝不拿一个不存在的目标去执行）。
//
// Tool 载体在此**顺带解析 `use_version`**（S5 灰度）：返回的 tool 是钉版本后的生效视图，
// 运行期（execLevel）与注册期（ValidateFlowStatic）共用本函数 → 版本语义天然一致，
// 「运行期跑 v2、注册期只认 Tool 级」这类漂移不可能出现。版本解析只有这一处调用点。
//
// 直调原语**不支持 `use_version` 与 `mode: service`**：前者只对「有版本列表的已注册 Tool」有意义，
// 原语无版本可言；后者需要长驻，而原语在进程内跑一次性 Lua。两条都在**此处（注册期）显式拒绝**——
// 不能静默忽略：静默 no-op 会让用户以为钉了版本 / 起了长驻服务，实际都没发生（M1「when 被静默忽略」、
// M6「use_version 被静默忽略」修掉的正是这一类漂移）。运行期另一入口 startServiceStep 有同款兜底。
func (e *Engine) resolveStepCall(step model.Step) (resolvedCall, error) {
	return e.resolveStepCallView(step, nil)
}

// resolveStepCallView 是 resolveStepCall 的「带额外可见 Tool」版本。
//
// 为什么需要它：MCP 的 `gear_validate` / `gear_apply` 要校验一个 **bundle**——里面的 Tool 与
// Flow 互相引用，而此刻那些 Tool **还没落库**。若只按 registry 解析，合法的 bundle 会被判成
// `UNKNOWN_CALL_TARGET`，「先校验后落库」这条路径就永远走不通。
//
// extra 是「本次一起提交、尚未落库」的 Tool 视图；解析顺序是 extra → registry。
// 判定规则（Tool 优先、直调兜底、方法级 schema 校验）与不带 extra 时**完全同一份代码**，
// 不存在第二套「bundle 专用解析」。
func (e *Engine) resolveStepCallView(step model.Step, extra map[string]*model.Tool) (resolvedCall, error) {
	var tool *model.Tool
	// ① 已注册 Tool 优先（「Tool 名恰好等于某能力方法名」时按 Tool 解析——
	//    plannedToolIDs 的上报口径与此一致，语义才是自洽的）。bundle 内待落库的 Tool 先看。
	if extra != nil {
		tool = extra[step.Call]
	}
	if tool == nil {
		registered, err := e.reg.GetTool(step.Call)
		if err != nil {
			return resolvedCall{}, err
		}
		tool = registered
	}
	if tool != nil {
		eff, err := resolveToolVersion(tool, step)
		if err != nil {
			return resolvedCall{}, err
		}
		// 运行期命名空间守卫（`--scope`）：只允许引用本应用名下的 Tool。
		// 原语直调不走这里（它们根本不是 registry 对象，见 guardScope 注释）。
		if err := e.guardScope(eff.ID); err != nil {
			return resolvedCall{}, err
		}
		return resolvedCall{tool: eff}, nil
	}

	// ② 「能力.方法」直调。切分规则复用 splitToolMethod——**ext 命名空间是两级**
	//    （`ext.ai.complete` → 能力 `ext.ai` + 方法 `complete`），用一刀 strings.Cut
	//    会把能力名切错成 `ext`（实测踩过：注册期报「未知的原子能力 "ext"」）。
	if prim, method := splitToolMethod(step.Call, ""); prim != "" && method != "" {
		if err := e.checkCapabilityMethod(prim, method); err != nil {
			return resolvedCall{}, diagWrap(CodeUnknownCapability, fmt.Errorf("step %s: %w", step.ID, err), step.ID, "step("+step.ID+").call",
				"能力名与方法名以 gear_capabilities 为准；ext.* 还必须在 extensions.yaml 注册")
		}
		if step.UseVersion != "" {
			return resolvedCall{}, diagWrap(CodeDirectCallLimited,
				fmt.Errorf("step %s 直调原子能力 %s 不支持 use_version（版本灰度只对已注册 Tool 的版本列表生效）", step.ID, step.Call),
				step.ID, "step("+step.ID+").use_version",
				"要去掉 use_version，或把这段逻辑包装成一个已注册 Tool（Tool 才有版本）")
		}
		if step.IsService() {
			return resolvedCall{}, diagWrap(CodeDirectCallLimited,
				fmt.Errorf("step %s 直调原子能力 %s 不支持 mode=service（长驻请用 process runtime 的 Tool）", step.ID, step.Call),
				step.ID, "step("+step.ID+").mode",
				"长驻 step 的 call 必须是已注册 Tool（用 process runtime），不能直调原子能力")
		}
		return resolvedCall{prim: prim, method: method}, nil
	}
	return resolvedCall{}, diagWrap(CodeUnknownCallTarget,
		fmt.Errorf("step %s 的 call %q 既不是已注册 Tool，也不是「能力.方法」形式的原子能力直调", step.ID, step.Call),
		step.ID, "step("+step.ID+").call",
		"先用 gear_catalog(kind=tool) 确认 Tool，或改用「能力.方法」直调（清单见 gear_capabilities）")
}

// checkCapabilityMethod 校验「能力.方法」是否为已知可调用的组合（红线 15：解析点拒绝未知值）。
//   - 内核原语（含 time/template/crypto 这类纯函数原子）：方法必须在 builtinCapabilitySchemas
//     中登记（未知方法即拒）；
//   - 扩展原语（ext.*）：能力名必须已注册；若该 ext 带方法级 schema（SetupExtensions 从
//     extensions.yaml 的 schemas 转存进能力目录），**方法名也必须命中 schema**——
//     `ext.kafka.notamethod` 这类只能靠侧车报错的误写，现在注册期即拒。
//     仅当注册方根本没提供 schema 时（程序化注册的 ext）才退化为「只校验能力名」：
//     没有 schema 就无从校验方法名，退化范围是显式的，绝不假装校验过。
func (e *Engine) checkCapabilityMethod(prim, method string) error {
	if methods, ok := builtinCapabilitySchemas[prim]; ok {
		if _, ok := methods[method]; !ok {
			return fmt.Errorf("内核原语 %s 没有方法 %q（合法方法：%s）", prim, method, strings.Join(sortedKeys(methods), ", "))
		}
		return nil
	}
	if _, ok := e.capReg.Lookup(prim); !ok {
		return fmt.Errorf("未知的原子能力 %q（既非内核原语，也不是已注册的 ext.*）", prim)
	}
	if methods, ok := e.capReg.LookupSchemas(prim); ok {
		if _, ok := methods[method]; !ok {
			return fmt.Errorf("扩展能力 %s 没有方法 %q（该 ext 声明的方法：%s）", prim, method, strings.Join(sortedKeys(methods), ", "))
		}
	}
	return nil
}

// methodSchema 返回「能力.方法」的参数 schema：
//   - 内核原语（含纯函数原子）→ builtinCapabilitySchemas（硬编码单一来源）；
//   - ext.* → 能力目录里由 SetupExtensions 转存的 extensions.yaml schemas（applyExtSchemas）。
//
// 两处都查不到时返回 false：调用方据此退化为「名字级」校验（无 schema 可校验，不假装校验）。
func (e *Engine) methodSchema(prim, method string) (MethodSchema, bool) {
	if methods, ok := builtinCapabilitySchemas[prim]; ok {
		ms, ok := methods[method]
		return ms, ok
	}
	if methods, ok := e.capReg.LookupSchemas(prim); ok {
		ms, ok := methods[method]
		return ms, ok
	}
	return MethodSchema{}, false
}

// validatePrimitiveArgs 按「能力的方法级参数 schema」校验一次直调入参：
// 未知参数名、缺必填一律拒绝——把「拼错参数名」「漏必填」挡在注册期与运行期两个入口。
//
// schema 单一真相源：内核原语取 builtinCapabilitySchemas，ext.* 取能力目录里由
// SetupExtensions 转存的 extensions.yaml schemas（同一份 schema 也喂给 agent 的
// capabilities()，故「agent 看到的参数」与「引擎接受的参数」必然一致）。
// 只有注册方未提供 schema 的 ext 才落到名字级校验（见 methodSchema）。
//
// 注意：入参的**值**可能还是模板串（如 `${vars.k}`）——本函数只校验**键名与必填**，
// 值的内容由运行期的模板展开与能力实现负责（键名此刻已确定，这已足以查出最常见的两类误写）。
func (e *Engine) validatePrimitiveArgs(prim, method string, args map[string]interface{}) error {
	if err := e.checkCapabilityMethod(prim, method); err != nil {
		return err
	}
	ms, ok := e.methodSchema(prim, method)
	if !ok {
		return nil // 无 schema 可校验（未提供 schema 的程序化 ext）：已确认能力/方法名
	}
	for name := range args {
		if _, ok := ms.Parameters[name]; !ok {
			return fmt.Errorf("%s.%s 不接受参数 %q（合法参数：%s）", prim, method, name, strings.Join(paramNames(ms), ", "))
		}
	}
	for name, p := range ms.Parameters {
		if !p.Required {
			continue
		}
		if v, ok := args[name]; ok && v != nil {
			continue
		}
		return fmt.Errorf("%s.%s 缺少必填参数 %q", prim, method, name)
	}
	return nil
}

// paramNames 返回某方法 schema 的参数名（升序，供错误信息稳定复现）。
func paramNames(ms MethodSchema) []string {
	out := make([]string, 0, len(ms.Parameters))
	for n := range ms.Parameters {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// primitiveHost 为一次「原子能力直调」构建宿主能力视图：**只放行该能力**（书写即授权）。
//
// 与 hostFor（Tool 载体）共用同一套守卫口径：调用链（ctx.call 循环守卫 + session 命名空间）、
// 订阅归属、扩展原语的 Provider 注入，全部走同一批辅助函数，两条路径不会漂移。
// prim 为 ext.* 时，经目录取 Provider 注入原生实现（go-host entry 亦可经 host.Extension 取用）。
//
// 纯函数核心原子（time/template/crypto）不在 cap.KnownPrimitives 里（不受 capability 门禁），
// 故此处额外放行——它们是「始终可用」的一等调用目标，Lua 侧由 lua.go 无条件注入。
func (e *Engine) primitiveHost(prim string, rc *runContext) (*cap.Context, error) {
	p := cap.Primitive(prim)
	if !p.Known() && !pureFuncPrimitives[prim] {
		return nil, fmt.Errorf("未知的原子能力 %q", prim)
	}
	c := withCallChain(cap.New(cap.Grants{p: true}, e.host), callChainOf(rc))
	if rc != nil {
		c.SetSubscriptionOwner(cap.SubscriptionOwner{FlowID: rc.flowID, ExecID: rc.execID})
		c.SetCaller(rc.caller) // 与 hostFor 同口径（原语直调也必须是同一份身份）
	}
	if ext, ok := e.capReg.Lookup(prim); ok {
		if v, err := ext.Provider(c); err == nil {
			c.SetExtension(prim, v)
		}
	}
	return c, nil
}
