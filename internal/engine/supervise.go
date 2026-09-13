package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"gear/internal/cap"
	"gear/internal/model"
)

// ServiceHandle 是某个 service 模式 step 的长驻子进程句柄，由 app.Supervisor 监管。
type ServiceHandle struct {
	StepID    string                 // step id
	Runtime   model.Runtime          // 重启时按同一 runtime 重新分发
	Proc      Process                // 当前（或重启后的）子进程抽象句柄
	Entry     string                 // 重启时复用的命令
	Input     map[string]interface{} // 已解析并校验的 input（重启时复用）
	Host      *cap.Context           // 重启时复用的宿主能力视图（M5，按原 Tool.Capabilities 授权）
	Restarts  int                    // 已重启次数（restart=always 时 > 0）
	StartedAt time.Time              // 本次（重）启动时间；Supervisor 据此判断存活时长以重置退避
}

// SuperviseResult 是 app run 监管启动后的结果：once 步骤已执行，service 步骤已拉起。
type SuperviseResult struct {
	FlowID   string
	Services []*ServiceHandle
}

// Supervise 执行 once 步骤并拉起 service 步骤，返回句柄（不阻塞等待）。
// 实际阻塞 / 重启 / 信号处理由 app.Supervisor 负责（PRD §14.2 / §14.3）。
//
// 约束（F3 严格守卫）：
//   - service step 不能作为任何 step 的依赖（它永不「完成」，依赖它无意义）
//   - service 与 once 共用同一 runtime 分发（runtimeFor）；未实现 runtime 由 runtimeFor 统一报错，
//     本函数不再维护第二份白名单（M10 收口）
func (e *Engine) Supervise(flowID string) (*SuperviseResult, error) {
	flow, err := e.reg.GetFlow(flowID)
	if err != nil {
		return nil, err
	}
	if flow == nil {
		return nil, fmt.Errorf("Flow 不存在: %s", flowID)
	}

	svcIDs := map[string]bool{}
	for _, s := range flow.Steps {
		if s.IsService() {
			svcIDs[s.ID] = true
		}
	}
	for _, s := range flow.Steps {
		for _, d := range s.DependsOn {
			if svcIDs[d] {
				return nil, fmt.Errorf("step %s 依赖了 service 模式 step %s（service 永不结束，不能作为依赖）", s.ID, d)
			}
		}
	}

	levels, err := topoLevels(flow.Steps)
	if err != nil {
		return nil, err
	}

	hooks, err := e.loadHooks()
	if err != nil {
		return nil, err
	}

	// 这里**不**上报「本次运行用到的 Tool」（对比 run() 的 ToolObserver）：
	// app 路径下 service 进程一旦拉起就与 Registry 脱钩——重启走 ServiceHandle 缓存的
	// Entry/Runtime，不再读 Registry；once 步骤此时也已跑完。因此注销 Tool 不会打断
	// 已受管的长驻进程，§8.2 的静态引用守卫（Flow 的 step.call）已足够覆盖。
	// 若将来 service 支持「运行期重读 Tool」，此处必须改为与 run() 同样的上报。

	rc := &runContext{
		vars:      flow.Vars,
		steps:     map[string]map[string]interface{}{},
		secretGet: e.secretResolver(),
	}
	res := &SuperviseResult{FlowID: flowID}
	var svcMu sync.Mutex // 并行 goroutine 都会追加 service 句柄

	deps, err := depMap(flow.Steps)
	if err != nil {
		return nil, err
	}
	skipped := map[string]string{}
	var skMu sync.Mutex
	markSkipped := func(id, reason string) {
		skMu.Lock()
		skipped[id] = reason
		skMu.Unlock()
	}
	skipReasonFor := func(id string) (string, bool) {
		skMu.Lock()
		defer skMu.Unlock()
		for _, d := range deps[id] {
			if r, ok := skipped[d]; ok {
				return fmt.Sprintf("上游 step %s 被跳过（%s）", d, r), true
			}
		}
		return "", false
	}

	// S4：与 run() 同样走分层并行；service 与 once 共用同一执行路径。
	for _, level := range levels {
		err := e.execLevel(flow.Steps, level, func(ctx context.Context, step model.Step, tool *model.Tool) error {
			if reason, ok := skipReasonFor(step.ID); ok {
				markSkipped(step.ID, reason)
				fmt.Printf("- step %s 跳过（%s）\n", step.ID, reason)
				return nil
			}

			if step.IsService() {
				h, err := e.startServiceStep(ctx, rc, hooks, step, tool)
				if errors.Is(err, ErrStepSkipped) {
					markSkipped(step.ID, "when=false")
					fmt.Printf("- step %s 跳过（when=false）\n", step.ID)
					return nil
				}
				if err != nil {
					return err
				}
				svcMu.Lock()
				res.Services = append(res.Services, h)
				svcMu.Unlock()
				fmt.Printf("• step %s (%s) 作为 service 长驻启动 (pid=%d)\n", step.ID, step.Call, h.Proc.Pid())
				return nil
			}

			input, output, err := e.runOnceStep(ctx, rc, hooks, step, tool)
			if errors.Is(err, ErrStepSkipped) {
				markSkipped(step.ID, "when=false")
				fmt.Printf("- step %s 跳过（when=false）\n", step.ID)
				return nil
			}
			if err != nil {
				return err
			}
			rc.setStep(step.ID, input, output)
			fmt.Printf("✓ step %s (%s) -> %v\n", step.ID, step.Call, output)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}

	if len(res.Services) > 0 {
		fmt.Printf("• Flow %s 进入监管态（%d 个 service 长驻）\n", flowID, len(res.Services))
	} else {
		fmt.Printf("✓ Flow %s 执行完成（纯 once），共 %d 步\n", flowID, len(flow.Steps))
	}
	return res, nil
}

// Relaunch 重启一个已退出的 service step（restart=always 时由 Supervisor 调用）。
// 按原 runtime 重新分发，复用 Entry 与已校验的 input，Restarts 计数 +1。
func (e *Engine) Relaunch(h *ServiceHandle) error {
	rt, err := runtimeFor(h.Runtime, e.sandboxPolicy)
	if err != nil {
		return fmt.Errorf("restart step %s: %w", h.StepID, err)
	}
	proc, err := rt.Start(context.Background(), h.Entry, h.Input, h.Host)
	if err != nil {
		return fmt.Errorf("restart step %s 失败: %w", h.StepID, err)
	}
	h.Proc = proc
	h.Restarts++
	h.StartedAt = time.Now()
	return nil
}
