package engine

import (
	"context"
	"fmt"
	"os"
	"sort"
	"time"

	"gear/internal/cap"
	"gear/internal/model"
	"gear/internal/sidecar/coord"
)

// ---- Hook 装配（PRD §5.3：显式装配，无隐式依赖）----

// loadHooks 读取全部 Hook 并按 Order 升序（装配顺序 / weight）。
func (e *Engine) loadHooks() ([]model.Hook, error) {
	hs, err := e.reg.ListHooks()
	if err != nil {
		return nil, err
	}
	out := make([]model.Hook, 0, len(hs))
	for _, h := range hs {
		out = append(out, *h)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Order < out[j].Order })
	return out, nil
}

// hooksFor 过滤出对 (stage, flowID, toolID) 生效的 Hook。
//
// 匹配维度由 stage 决定（单一判定来源，避免第二处匹配逻辑漂移）：
//   - Flow 级 stage（before_flow / on_success，model.IsFlowLevelStage）：Target 为 "*" 或精确 flow id
//   - step 级 stage（before_step / after_step / on_error）：Target 为 "*" 或精确 tool id
//
// 之所以把 flowID / toolID 同时传入、由本函数按 stage 选维度，而不是写两份 hooksFor：
// 「哪些 Hook 会触发」只能有一处真相（G2/M10 教训），否则加 stage 或改 Target 语义必漏一处。
func hooksFor(all []model.Hook, stage, flowID, toolID string) []model.Hook {
	flowLevel := model.IsFlowLevelStage(stage)
	var out []model.Hook
	for _, h := range all {
		if h.Stage != stage {
			continue
		}
		if flowLevel {
			if h.Target == "*" || h.Target == flowID {
				out = append(out, h)
			}
		} else {
			if h.Target == toolID || h.Target == "*" {
				out = append(out, h)
			}
		}
	}
	return out
}

// hookFail 依据 criticality 处置 Hook 失败：
//   - critical（含留空）：fail-closed，返回错误终止编排（用户决策：继续运行可能违背意图）
//   - best-effort：仅告警到 stderr，主流程继续
func hookFail(h model.Hook, cause error) error {
	if h.EffectiveCriticality() == model.CritBestEffort {
		fmt.Fprintf(os.Stderr, "⚠ Hook %s（best-effort）失败，已忽略: %v\n", h.ID, cause)
		return nil
	}
	return fmt.Errorf("Hook %s（critical）失败: %w", h.ID, cause)
}

// applyHooks 依次执行生效的 Hook，返回改写后的 payload 与「是否被改写」。
//
// 语义（PRD §5.4）：
//   - Hook 的载体是一个**已注册 Tool**（Action），引擎直接调用其 runtime.Run —— 不经 step 流程，
//     因此 Hook 执行不会再触发 Hook（结构性无递归，无需额外开关）。
//   - Hook 返回非空 object 时**整体替换** payload；返回空则不改动。
//   - Hook 的工具侧 schema 不做校验（Hook 是变换函数）；**被改写后的 payload 由调用方按
//     step 的 Tool schema 重新校验**（§9.1 不可绕过 Schema）。
func (e *Engine) applyHooks(ctx context.Context, rc *runContext, all []model.Hook, stage, flowID, toolID string, payload map[string]interface{}) (map[string]interface{}, bool, error) {
	applied := hooksFor(all, stage, flowID, toolID)
	if len(applied) == 0 {
		return payload, false, nil
	}

	cur := payload
	rewritten := false
	for _, h := range applied {
		tool, err := e.reg.GetTool(h.Action)
		if err != nil {
			return nil, rewritten, err
		}
		if tool == nil {
			if ferr := hookFail(h, fmt.Errorf("Hook %s 引用的 Tool 不存在: %s", h.ID, h.Action)); ferr != nil {
				return nil, rewritten, ferr
			}
			continue
		}
		// 运行期命名空间守卫（`--scope`）：Hook 的 action 是由别人定义的行为，
		// 比 step.call 更容易被顺手改成「顺手清一下别人应用的表」——必须同口径拦。
		if err := e.guardScope(tool.ID); err != nil {
			return nil, rewritten, err
		}

		rt, err := runtimeForCaps(tool.Runtime, e.sandboxPolicy, e.capReg)
		if err != nil {
			if ferr := hookFail(h, fmt.Errorf("Hook %s: %w", h.ID, err)); ferr != nil {
				return nil, rewritten, ferr
			}
			continue
		}

		hctx, cancel := context.WithTimeout(ctx, execTimeoutFor(tool))
		// rc 非 nil 时宿主视图带上调用上下文（IDENTITY_CONTEXT_DESIGN.md）：
		// 「谁发起的」必须能被 Hook 看到，鉴权 Hook 才有依据。传 nil 会退化成无身份，
		// 那是**静默丢身份**，不是「Hook 不需要身份」——故调用方**必须**传 rc。
		host := e.hostFor(tool, rc)
		out, err := rt.Run(hctx, tool.Entry, cur, host)
		cancel()
		if err != nil {
			if ferr := hookFail(h, fmt.Errorf("Hook %s 执行失败: %w", h.ID, err)); ferr != nil {
				return nil, rewritten, ferr
			}
			continue
		}
		if len(out) > 0 {
			cur = out
			rewritten = true
		}
	}
	return cur, rewritten, nil
}

// notifyError 触发 on_error Hook（补偿 / 告警）。
// 其输出**被丢弃**：on_error 不能恢复或重试主流程（PRD §5.4）。
// 返回值仅用于把「critical on_error 自身失败」附加到原错误上，绝不改变「主流程失败」的事实。
func (e *Engine) notifyError(ctx context.Context, rc *runContext, all []model.Hook, toolID string, stepErr error, input map[string]interface{}) error {
	_, _, hookErr := e.applyHooks(ctx, rc, all, model.StageOnError, "", toolID, map[string]interface{}{
		"step_error": stepErr.Error(),
		"input":      input,
	})
	return hookErr
}

// applyFlowScopedHooks 应用 Flow 级 Hook（before_flow / on_success）。
//
// 与 step 级 Hook 共用 hooksFor（同一份匹配判定），但匹配维度是 flow id：
// Target 为 "*" 或精确 flow id 才生效。Action 解析为两类载体：
//
//  1. Tool 载体：与 step 级 Hook 同语义，直接调用其 runtime.Run；
//     返回非空 object 整体替换 payload（before_flow 时 payload 即 rc.vars；
//     on_success 时 payload 即 RunResult 的 map 表示，但输出被丢弃——on_success 只做副作用）。
//  2. Flow 载体（#72）：Action 指向一个 Flow id，引擎**同步内联**执行子 Flow
//     （复用 e.run，同 execID 上下文，结构性无递归）。
//     - before_flow：子 Flow 的 RunResult.Steps 暴露进主 Flow 的 vars（key=子 Flow id），
//     下游 step 可经 `vars.<flowID>.<stepID>.output` 引用其结果。
//     - on_success：子 Flow 仅作收尾副作用执行，其输出不影响主结果。
//
// 失败语义（fail-closed）：critical（含留空）载体失败即返回错误终止编排；
// best-effort 仅告警继续。before_flow 失败会阻止主 Flow 启动；on_success 失败会让
// 「已成功的主 Flow」在 run() 处返回错误（成功但收尾失败）。
func (e *Engine) applyFlowScopedHooks(ctx context.Context, rc *runContext, all []model.Hook, stage, flowID string) error {
	applied := hooksFor(all, stage, flowID, "")
	if len(applied) == 0 {
		return nil
	}
	for _, h := range applied {
		// 1) Tool 载体优先解析。
		tool, err := e.reg.GetTool(h.Action)
		if err != nil {
			return fmt.Errorf("Hook %s 解析 action Tool 失败: %w", h.ID, err)
		}
		if tool != nil {
			// 运行期命名空间守卫（`--scope`）：与 step 级 Hook 同口径（见 applyHooks）。
			if err := e.guardScope(tool.ID); err != nil {
				return err
			}
			if err := e.applyFlowScopedToolHook(ctx, rc, h, stage); err != nil {
				return err
			}
			continue
		}
		// 2) Flow 载体（#72）：同步内联执行子 Flow。
		sub, err := e.reg.GetFlow(h.Action)
		if err != nil {
			return fmt.Errorf("Hook %s 解析 action Flow 失败: %w", h.ID, err)
		}
		if err := e.guardScope(sub.ID); err != nil {
			return err
		}
		if sub == nil {
			if ferr := hookFail(h, fmt.Errorf("Hook %s 引用的 action 既不是已注册 Tool 也不是已注册 Flow: %s", h.ID, h.Action)); ferr != nil {
				return ferr
			}
			continue
		}
		if err := e.applyFlowScopedFlowHook(ctx, rc, h, stage, sub); err != nil {
			return err
		}
	}
	return nil
}

// applyFlowScopedToolHook 执行 Flow 级 Hook 的 Tool 载体（action = Tool id）。
func (e *Engine) applyFlowScopedToolHook(ctx context.Context, rc *runContext, h model.Hook, stage string) error {
	tool, _ := e.reg.GetTool(h.Action) // 已确认非 nil（调用方解析过，命名空间守卫同样在那儿判过）
	rt, err := runtimeForCaps(tool.Runtime, e.sandboxPolicy, e.capReg)
	if err != nil {
		if ferr := hookFail(h, fmt.Errorf("Hook %s: %w", h.ID, err)); ferr != nil {
			return ferr
		}
		return nil
	}
	var payload map[string]interface{}
	switch stage {
	case model.StageBeforeFlow:
		payload = rc.varsSnapshot() // 主 Flow 初始 vars（before_flow 改写后整体替换）
	default:
		// on_success：payload 是 RunResult 的 map 表示（仅副作用，输出丢弃）。
		payload = rc.lastResultMap()
	}
	hctx, cancel := context.WithTimeout(ctx, execTimeoutFor(tool))
	host := e.hostFor(tool, rc)
	out, err := rt.Run(hctx, tool.Entry, payload, host)
	cancel()
	if err != nil {
		if ferr := hookFail(h, fmt.Errorf("Hook %s 执行失败: %w", h.ID, err)); ferr != nil {
			return ferr
		}
		return nil
	}
	// before_flow 的 Tool 载体可改写 vars；on_success 的 Tool 载体输出被丢弃（见 §5.4 同 on_error）。
	if stage == model.StageBeforeFlow && len(out) > 0 {
		rc.replaceVars(out)
	}
	return nil
}

// applyFlowScopedFlowHook 执行 Flow 级 Hook 的 Flow 载体（action = Flow id）：同步内联子 Flow。
func (e *Engine) applyFlowScopedFlowHook(ctx context.Context, rc *runContext, h model.Hook, stage string, sub *model.Flow) error {
	// 递归守卫（#72 关键）：当前调用链 = 祖先链 ∪ 本 Flow id。若子 Flow 已在链上，
	// 再拉起会形成 A→B→A 循环（或被 Target="*" 的 before_flow 钩中自身）而无限递归，
	// 必须在此拦截（结构性无递归，与 Hook 不触发 Hook 同一原则）。
	chain := append(append([]string{}, rc.ancestors...), rc.flowID)
	for _, id := range chain {
		if id == sub.ID {
			if ferr := hookFail(h, fmt.Errorf("Hook %s 内联子 Flow %s 会形成循环调用（调用链 %v），已拦截", h.ID, sub.ID, chain)); ferr != nil {
				return ferr
			}
			return nil
		}
	}
	// 同步内联执行：复用 e.run，execID 透传（日志关联到同一执行），observe=nil（快照已在主 Flow 上报），
	// 并把本 Flow id 追加进祖先链，供子 Flow 的递归守卫使用。
	subRes, err := e.run(sub, nil, rc.execID, append(append([]string{}, rc.ancestors...), rc.flowID))
	if err != nil {
		if ferr := hookFail(h, fmt.Errorf("Hook %s 内联子 Flow %s 执行失败: %w", h.ID, sub.ID, err)); ferr != nil {
			return ferr
		}
		return nil
	}
	// before_flow：把子 Flow 的 steps 输出暴露给主 Flow 的 vars，供下游 step 经
	// `vars.<flowID>.<stepID>.output` 引用（on_success 仅副作用，不暴露）。
	if stage == model.StageBeforeFlow {
		rc.exposeFlowResult(sub.ID, subRes)
	}
	return nil
}

// ---- step 生命周期的共享执行路径（run 与 Supervise 共用，保证 Hook 语义一致）----

// prepareStepInput 解析 step.input 并应用 before_step Hook，随后按调用载体校验：
//   - Tool 载体：按 Tool.input_schema 校验；
//   - 原语载体：按能力的方法级参数 schema 校验（缺必填 / 未知参数即拒）。
//
// 两条分支都保留「Hook 改写后重校验」——保住单一 Schema 真理源不可绕过。
func (e *Engine) prepareStepInput(ctx context.Context, rc *runContext, hooks []model.Hook, flowID string, step model.Step, call resolvedCall) (map[string]interface{}, error) {
	input, err := resolveInput(step.Input, rc)
	if err != nil {
		return nil, fmt.Errorf("step %s 解析 input 失败: %w", step.ID, err)
	}

	input, rewritten, err := e.applyHooks(ctx, rc, hooks, model.StageBeforeStep, flowID, call.id(), input)
	if err != nil {
		return nil, err
	}

	if call.isPrimitive() {
		// 模板与 before_step Hook 都可能改写 input，故这一层是最终把关（不只依赖注册期校验）。
		if err := e.validatePrimitiveArgs(call.prim, call.method, input); err != nil {
			if rewritten {
				return nil, fmt.Errorf("step %s input 经 Hook 改写后校验失败: %w", step.ID, err)
			}
			return nil, fmt.Errorf("step %s input 校验失败: %w", step.ID, err)
		}
		return input, nil
	}

	if err := validate(call.tool.InputSchema, input); err != nil {
		if rewritten {
			return nil, fmt.Errorf("step %s input 经 Hook 改写后校验失败: %w", step.ID, err)
		}
		return nil, fmt.Errorf("step %s input 校验失败: %w", step.ID, err)
	}
	return input, nil
}

// stepExecutable 按调用载体给出「本次执行三件套」(runtime, entry, host)：
//   - Tool 载体：自身 runtime / entry，宿主视图按 Tool.Capabilities 最小授权；
//   - 原语载体：Lua runtime + buildActionEntry 编出的入口（与 ext.ai.react /
//     POST /api/capabilities/invoke 同一条通道），宿主视图只放行该能力（书写即授权）。
func (e *Engine) stepExecutable(call resolvedCall, input map[string]interface{}, rc *runContext) (Runtime, string, *cap.Context, error) {
	if !call.isPrimitive() {
		rt, err := runtimeForCaps(call.tool.Runtime, e.sandboxPolicy, e.capReg)
		if err != nil {
			return nil, "", nil, err
		}
		return rt, call.tool.Entry, e.hostFor(call.tool, rc), nil
	}
	entry, err := buildActionEntry(call.prim, call.method, input)
	if err != nil {
		return nil, "", nil, err
	}
	rt, err := runtimeForCaps(model.RuntimeLua, e.sandboxPolicy, e.capReg)
	if err != nil {
		return nil, "", nil, err
	}
	host, err := e.primitiveHost(call.prim, rc)
	if err != nil {
		return nil, "", nil, err
	}
	return rt, entry, host, nil
}

// runOnceStep 执行一个 once step 的完整生命周期：
// when → before_step Hook + 校验 → 执行 → after_step Hook（下游看到改后值）→ output 重校验；
// 执行失败时触发 on_error（仅补偿，不恢复）。
//
// 两种载体（Tool / 直调原子能力）共用本路径，因此 Hook 的织入点、失败语义、日志关联完全一致。
func (e *Engine) runOnceStep(ctx context.Context, rc *runContext, hooks []model.Hook, flowID string, step model.Step, call resolvedCall) (input, output map[string]interface{}, err error) {
	// when 必须**先于** input 解析：条件为假时若仍去解析 input，
	// 会去读一个可能已被跳过的上游输出，报出误导性的「尚未执行」而不是「跳过」。
	if err := e.whenHolds(rc, step); err != nil {
		return nil, nil, err
	}

	input, err = e.prepareStepInput(ctx, rc, hooks, flowID, step, call)
	if err != nil {
		return nil, nil, err
	}

	rt, entry, host, err := e.stepExecutable(call, input, rc)
	if err != nil {
		return nil, nil, fmt.Errorf("step %s: %w", step.ID, err)
	}

	// 日志采集（REQ-API-LOGS / #71）：把 process 子进程输出关联到本执行+本 step。
	if pr, ok := rt.(*processRuntime); ok && rc.logSink != nil {
		pr.execID, pr.stepID, pr.sink = rc.execID, step.ID, rc.logSink
	}

	// 生效超时：Tool 声明的 timeout 优先，未声明回退 defaultExecTimeout（见 execTimeoutFor）。
	// 直调原子能力（call.tool == nil）恒用默认值。
	stepCtx, cancel := context.WithTimeout(ctx, execTimeoutFor(call.tool))
	output, err = rt.Run(stepCtx, entry, input, host)
	cancel()
	if err != nil {
		stepErr := fmt.Errorf("step %s 执行失败: %w", step.ID, err)
		if hookErr := e.notifyError(ctx, rc, hooks, call.id(), err, input); hookErr != nil {
			return nil, nil, fmt.Errorf("%w（另：%v）", stepErr, hookErr)
		}
		return nil, nil, stepErr
	}
	if call.isPrimitive() {
		// 原语返回值统一包成 {result: ...}（与 POST /api/capabilities/invoke 同形），
		// 下游按 ${steps.<id>.output.result} 引用。
		// Lua 表里 `result = nil` 会让该键消失，此处补一个显式 null——否则「返回 nil」
		// 与「返回空表」在 output 里同形，下游无法区分。
		if _, ok := output["result"]; !ok {
			output["result"] = nil
		}
	}

	output, rewritten, err := e.applyHooks(ctx, rc, hooks, model.StageAfterStep, flowID, call.id(), output)
	if err != nil {
		return nil, nil, err
	}

	// 原语直调没有 output_schema（能力目录只声明**入参** schema，不声明返回值形状）→ 跳过校验。
	// Tool 载体照旧：§9.1 不可绕过 Schema。
	if !call.isPrimitive() {
		if err := validate(call.tool.OutputSchema, output); err != nil {
			if rewritten {
				return nil, nil, fmt.Errorf("step %s output 经 Hook 改写后校验失败: %w", step.ID, err)
			}
			return nil, nil, fmt.Errorf("step %s output 校验失败: %w", step.ID, err)
		}
	}
	return input, output, nil
}

// startServiceStep 应用 before_step Hook + 校验后拉起 service。
// 注意：service step 永不结束，故 after_step 对其不适用（PRD §14.2）。
func (e *Engine) startServiceStep(ctx context.Context, rc *runContext, hooks []model.Hook, flowID string, step model.Step, call resolvedCall) (*ServiceHandle, error) {
	// 直调原子能力是进程内 Lua，没有「长驻」形态：注册期已由 resolveStepCall 拦截，
	// 此处再兜一层，避免任何旁路把它送到 rt.Start。
	if call.isPrimitive() {
		return nil, fmt.Errorf("step %s 直调原子能力 %s 不支持 mode=service（长驻请用 process runtime 的 Tool）", step.ID, step.Call)
	}
	tool := call.tool

	if err := e.whenHolds(rc, step); err != nil {
		return nil, err
	}

	input, err := e.prepareStepInput(ctx, rc, hooks, flowID, step, call)
	if err != nil {
		return nil, err
	}

	rt, err := runtimeForCaps(tool.Runtime, e.sandboxPolicy, e.capReg)
	if err != nil {
		return nil, fmt.Errorf("step %s: %w", step.ID, err)
	}
	host := e.hostFor(tool, rc)

	// 日志采集（REQ-API-LOGS-STREAM）：与 once step 同口径，把 service 子进程输出关联到本执行+本 step。
	// 此前 service 路径漏设 sink，导致长驻进程输出既不落盘也不实时可见——只在崩溃退出时由
	// serviceTail 兜底到监管日志。这里补上后，service 实时输出经组合 sink 同时落盘 + 推 broker。
	if pr, ok := rt.(*processRuntime); ok && rc.logSink != nil {
		pr.execID, pr.stepID, pr.sink = rc.execID, step.ID, rc.logSink
	}

	// 关键：service 进程是**长驻**的，不能绑在层级的 ctx 上——
	// execLevel 在层结束时会 cancel()，若沿用该 ctx，刚拉起的 service 会被立刻杀掉。
	// 因此：启动前先响应取消（fail-fast），真正拉起时用不受层级取消影响的上下文。
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("step %s 拉起 service 前已被取消: %w", step.ID, err)
	}

	// service ext：走 coord 握手（注册优先 / 竞态退让 / 看门狗心跳）。
	// 优先向侧车 Register 自身 PID；若无人监听则本 gear 当 owner 拉起 sidecar，
	// 否则退让为 client 复用同一实例（同主机多 gear 共享单个 sidecar，避免端口冲突）。
	if cli := e.capReg.SvcExt[tool.ID]; cli != nil {
		sess, err := cli.RegisterOrStart(func() (coord.Process, error) {
			return rt.Start(context.Background(), tool.Entry, input, host)
		})
		if err != nil {
			stepErr := fmt.Errorf("step %s 启动 service ext 协调失败: %w", step.ID, err)
			if hookErr := e.notifyError(ctx, rc, hooks, tool.ID, err, input); hookErr != nil {
				return nil, fmt.Errorf("%w（另：%v）", stepErr, hookErr)
			}
			return nil, stepErr
		}
		return &ServiceHandle{
			StepID: step.ID, Runtime: tool.Runtime, Proc: sess.Proc(), Entry: tool.Entry, Input: input, Host: host,
			Probe:     tool.HealthProbe,
			StartedAt: time.Now(),
			Coord:     sess,
			// Output 恒为 nil：coord 会话下的 Proc 是侧车进程，它的 stdout 是协调协议不是
			// step 声明，无从解析。**不伪造**：下游引用 `steps.<id>.output.*` 会吃到未知字段
			// 报错，而不是拿到一个查不出原因的空串。
			Output: nil,
		}, nil
	}

	proc, err := rt.Start(context.Background(), tool.Entry, input, host)
	if err != nil {
		stepErr := fmt.Errorf("step %s 拉起 service 失败: %w", step.ID, err)
		if hookErr := e.notifyError(ctx, rc, hooks, tool.ID, err, input); hookErr != nil {
			return nil, fmt.Errorf("%w（另：%v）", stepErr, hookErr)
		}
		return nil, stepErr
	}

	return &ServiceHandle{
		StepID: step.ID, Runtime: tool.Runtime, Proc: proc, Entry: tool.Entry, Input: input, Host: host,
		Probe:     tool.HealthProbe, // service 健康检查探针（REQ-APP-HEALTH / #75）；nil = 不探
		StartedAt: time.Now(),       // 供 Supervisor 判定「存活时长」以决定是否重置退避
		Output:    outputOf(proc),   // 阻塞等首行声明（有界）；只有 process runtime 报得出，其余 nil
	}, nil
}
