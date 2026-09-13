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

// hooksFor 过滤出对 (stage, toolID) 生效的 Hook：Target 为精确 tool id 或 "*"。
func hooksFor(all []model.Hook, stage, toolID string) []model.Hook {
	var out []model.Hook
	for _, h := range all {
		if h.Stage != stage {
			continue
		}
		if h.Target == toolID || h.Target == "*" {
			out = append(out, h)
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
func (e *Engine) applyHooks(ctx context.Context, all []model.Hook, stage, toolID string, payload map[string]interface{}) (map[string]interface{}, bool, error) {
	applied := hooksFor(all, stage, toolID)
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

		rt, err := runtimeFor(tool.Runtime)
		if err != nil {
			if ferr := hookFail(h, fmt.Errorf("Hook %s: %w", h.ID, err)); ferr != nil {
				return nil, rewritten, ferr
			}
			continue
		}

		hctx, cancel := context.WithTimeout(ctx, defaultExecTimeout)
		out, err := rt.Run(hctx, tool.Entry, cur)
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
	_, _, hookErr := e.applyHooks(ctx, all, model.StageOnError, toolID, map[string]interface{}{
		"step_error": stepErr.Error(),
		"input":      input,
	})
	return hookErr
}

// ---- step 生命周期的共享执行路径（run 与 Supervise 共用，保证 Hook 语义一致）----

// prepareStepInput 解析 step.input 并应用 before_step Hook，随后按 Tool.input_schema 校验
// （含「Hook 改写后重校验」——保住单一 Schema 真理源不可绕过）。
func (e *Engine) prepareStepInput(ctx context.Context, rc *runContext, hooks []model.Hook, step model.Step, tool *model.Tool) (map[string]interface{}, error) {
	input, err := resolveInput(step.Input, rc)
	if err != nil {
		return nil, fmt.Errorf("step %s 解析 input 失败: %w", step.ID, err)
	}

	input, rewritten, err := e.applyHooks(ctx, hooks, model.StageBeforeStep, tool.ID, input)
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
func (e *Engine) runOnceStep(ctx context.Context, rc *runContext, hooks []model.Hook, step model.Step, tool *model.Tool) (input, output map[string]interface{}, err error) {
	// when 必须**先于** input 解析：条件为假时若仍去解析 input，
	// 会去读一个可能已被跳过的上游输出，报出误导性的「尚未执行」而不是「跳过」。
	if err := e.whenHolds(rc, step); err != nil {
		return nil, nil, err
	}

	input, err = e.prepareStepInput(ctx, rc, hooks, step, tool)
	if err != nil {
		return nil, nil, err
	}

	rt, err := runtimeFor(tool.Runtime)
	if err != nil {
		return nil, nil, fmt.Errorf("step %s: %w", step.ID, err)
	}

	stepCtx, cancel := context.WithTimeout(ctx, defaultExecTimeout)
	output, err = rt.Run(stepCtx, tool.Entry, input)
	cancel()
	if err != nil {
		stepErr := fmt.Errorf("step %s 执行失败: %w", step.ID, err)
		if hookErr := e.notifyError(ctx, hooks, tool.ID, err, input); hookErr != nil {
			return nil, nil, fmt.Errorf("%w（另：%v）", stepErr, hookErr)
		}
		return nil, nil, stepErr
	}

	output, rewritten, err := e.applyHooks(ctx, hooks, model.StageAfterStep, tool.ID, output)
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
func (e *Engine) startServiceStep(ctx context.Context, rc *runContext, hooks []model.Hook, step model.Step, tool *model.Tool) (*ServiceHandle, error) {
	if err := e.whenHolds(rc, step); err != nil {
		return nil, err
	}

	input, err := e.prepareStepInput(ctx, rc, hooks, step, tool)
	if err != nil {
		return nil, err
	}

	rt, err := runtimeFor(tool.Runtime)
	if err != nil {
		return nil, fmt.Errorf("step %s: %w", step.ID, err)
	}

	// 关键：service 进程是**长驻**的，不能绑在层级的 ctx 上——
	// execLevel 在层结束时会 cancel()，若沿用该 ctx，刚拉起的 service 会被立刻杀掉。
	// 因此：启动前先响应取消（fail-fast），真正拉起时用不受层级取消影响的上下文。
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("step %s 拉起 service 前已被取消: %w", step.ID, err)
	}
	proc, err := rt.Start(context.Background(), tool.Entry, input)
	if err != nil {
		stepErr := fmt.Errorf("step %s 拉起 service 失败: %w", step.ID, err)
		if hookErr := e.notifyError(ctx, hooks, tool.ID, err, input); hookErr != nil {
			return nil, fmt.Errorf("%w（另：%v）", stepErr, hookErr)
		}
		return nil, stepErr
	}

	return &ServiceHandle{
		StepID: step.ID, Runtime: tool.Runtime, Proc: proc, Entry: tool.Entry, Input: input,
		StartedAt: time.Now(), // 供 Supervisor 判定「存活时长」以决定是否重置退避
	}, nil
}
