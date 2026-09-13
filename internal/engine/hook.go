package engine

import (
	"context"
	"fmt"
	"os"
	"sort"
	"time"

	"gear/internal/model"
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
func (e *Engine) applyHooks(ctx context.Context, all []model.Hook, stage, flowID, toolID string, payload map[string]interface{}) (map[string]interface{}, bool, error) {
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

		rt, err := runtimeFor(tool.Runtime, e.sandboxPolicy)
		if err != nil {
			if ferr := hookFail(h, fmt.Errorf("Hook %s: %w", h.ID, err)); ferr != nil {
				return nil, rewritten, ferr
			}
			continue
		}

		hctx, cancel := context.WithTimeout(ctx, defaultExecTimeout)
		host := e.hostFor(tool, nil)
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
func (e *Engine) notifyError(ctx context.Context, all []model.Hook, toolID string, stepErr error, input map[string]interface{}) error {
	_, _, hookErr := e.applyHooks(ctx, all, model.StageOnError, "", toolID, map[string]interface{}{
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
//       下游 step 可经 `vars.<flowID>.<stepID>.output` 引用其结果。
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
	tool, _ := e.reg.GetTool(h.Action) // 已确认非 nil（调用方解析过）
	rt, err := runtimeFor(tool.Runtime, e.sandboxPolicy)
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
	hctx, cancel := context.WithTimeout(ctx, defaultExecTimeout)
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

// prepareStepInput 解析 step.input 并应用 before_step Hook，随后按 Tool.input_schema 校验
// （含「Hook 改写后重校验」——保住单一 Schema 真理源不可绕过）。
func (e *Engine) prepareStepInput(ctx context.Context, rc *runContext, hooks []model.Hook, flowID string, step model.Step, tool *model.Tool) (map[string]interface{}, error) {
	input, err := resolveInput(step.Input, rc)
	if err != nil {
		return nil, fmt.Errorf("step %s 解析 input 失败: %w", step.ID, err)
	}

	input, rewritten, err := e.applyHooks(ctx, hooks, model.StageBeforeStep, flowID, tool.ID, input)
	if err != nil {
		return nil, err
	}

	if err := validate(tool.InputSchema, input); err != nil {
		if rewritten {
			return nil, fmt.Errorf("step %s input 经 Hook 改写后校验失败: %w", step.ID, err)
		}
		return nil, fmt.Errorf("step %s input 校验失败: %w", step.ID, err)
	}
	return input, nil
}

// runOnceStep 执行一个 once step 的完整生命周期：
// before_step Hook → 执行 → after_step Hook（下游看到改后值）→ output 重校验；
// 执行失败时触发 on_error（仅补偿，不恢复）。
func (e *Engine) runOnceStep(ctx context.Context, rc *runContext, hooks []model.Hook, flowID string, step model.Step, tool *model.Tool) (input, output map[string]interface{}, err error) {
	// when 必须**先于** input 解析：条件为假时若仍去解析 input，
	// 会去读一个可能已被跳过的上游输出，报出误导性的「尚未执行」而不是「跳过」。
	if err := e.whenHolds(rc, step); err != nil {
		return nil, nil, err
	}

	input, err = e.prepareStepInput(ctx, rc, hooks, flowID, step, tool)
	if err != nil {
		return nil, nil, err
	}

	rt, err := runtimeFor(tool.Runtime, e.sandboxPolicy)
	if err != nil {
		return nil, nil, fmt.Errorf("step %s: %w", step.ID, err)
	}

	// 日志采集（REQ-API-LOGS / #71）：把 process 子进程输出关联到本执行+本 step。
	if pr, ok := rt.(*processRuntime); ok && rc.logSink != nil {
		pr.execID, pr.stepID, pr.sink = rc.execID, step.ID, rc.logSink
	}

	stepCtx, cancel := context.WithTimeout(ctx, defaultExecTimeout)
	host := e.hostFor(tool, rc)
	output, err = rt.Run(stepCtx, tool.Entry, input, host)
	cancel()
	if err != nil {
		stepErr := fmt.Errorf("step %s 执行失败: %w", step.ID, err)
		if hookErr := e.notifyError(ctx, hooks, tool.ID, err, input); hookErr != nil {
			return nil, nil, fmt.Errorf("%w（另：%v）", stepErr, hookErr)
		}
		return nil, nil, stepErr
	}

	output, rewritten, err := e.applyHooks(ctx, hooks, model.StageAfterStep, flowID, tool.ID, output)
	if err != nil {
		return nil, nil, err
	}

	if err := validate(tool.OutputSchema, output); err != nil {
		if rewritten {
			return nil, nil, fmt.Errorf("step %s output 经 Hook 改写后校验失败: %w", step.ID, err)
		}
		return nil, nil, fmt.Errorf("step %s output 校验失败: %w", step.ID, err)
	}
	return input, output, nil
}

// startServiceStep 应用 before_step Hook + 校验后拉起 service。
// 注意：service step 永不结束，故 after_step 对其不适用（PRD §14.2）。
func (e *Engine) startServiceStep(ctx context.Context, rc *runContext, hooks []model.Hook, flowID string, step model.Step, tool *model.Tool) (*ServiceHandle, error) {
	if err := e.whenHolds(rc, step); err != nil {
		return nil, err
	}

	input, err := e.prepareStepInput(ctx, rc, hooks, flowID, step, tool)
	if err != nil {
		return nil, err
	}

	rt, err := runtimeFor(tool.Runtime, e.sandboxPolicy)
	if err != nil {
		return nil, fmt.Errorf("step %s: %w", step.ID, err)
	}
	host := e.hostFor(tool, rc)

	// 关键：service 进程是**长驻**的，不能绑在层级的 ctx 上——
	// execLevel 在层结束时会 cancel()，若沿用该 ctx，刚拉起的 service 会被立刻杀掉。
	// 因此：启动前先响应取消（fail-fast），真正拉起时用不受层级取消影响的上下文。
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("step %s 拉起 service 前已被取消: %w", step.ID, err)
	}
	proc, err := rt.Start(context.Background(), tool.Entry, input, host)
	if err != nil {
		stepErr := fmt.Errorf("step %s 拉起 service 失败: %w", step.ID, err)
		if hookErr := e.notifyError(ctx, hooks, tool.ID, err, input); hookErr != nil {
			return nil, fmt.Errorf("%w（另：%v）", stepErr, hookErr)
		}
		return nil, stepErr
	}

	return &ServiceHandle{
		StepID: step.ID, Runtime: tool.Runtime, Proc: proc, Entry: tool.Entry, Input: input, Host: host,
		Probe: tool.HealthProbe, // service 健康检查探针（REQ-APP-HEALTH / #75）；nil = 不探
		StartedAt: time.Now(), // 供 Supervisor 判定「存活时长」以决定是否重置退避
	}, nil
}
