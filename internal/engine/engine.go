// Package engine 实现 Gear 内核引擎（S1：顺序执行 + process runtime + schema 校验 + vars 传递）。
package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v5"

	"gear/internal/cap"
	"gear/internal/model"
	"gear/internal/registry"
)

// DefaultMaxParallel 是单个 Flow 内**同时执行**的 step 数上限（S4 并发闸门/背压）。
// 防止高扇出 Flow 一次性 fork 出成百上千个进程把机器打爆（PRD §14.5）。
const DefaultMaxParallel = 4

// Lua 沙箱策略（REQ-LUA-SANDBOX-CONFIG）。「拦截哪些危险行为」由策略决定，用户自抉：
//   - SandboxStrict（默认）：只开 base/string/table/math，禁 dofile/loadfile/require/module
//     （纯计算，无法接触宿主；M5 的 ctx.* 原语仍按 Capabilities 最小权限放行）。
//   - SandboxPermissive：在 strict 基础上额外开放 coroutine/utf8（纯计算安全），仍禁文件加载逃逸。
//     不提供 none/unsafe：Lua 逃逸宿主是 P0 级风险，配置项不应能关掉沙箱本身。
const (
	SandboxStrict    = "strict"
	SandboxPermissive = "permissive"
)

// Engine 持有 registry，负责把 Flow 跑起来。
type Engine struct {
	reg         *registry.Registry
	maxParallel int
	// sandboxPolicy 是 Lua 沙箱档位（strict/permissive），经 WithSandbox 设置。
	sandboxPolicy string
	// host 是宿主能力原语的实现集合（M5，PRD §6）。nil 表示不提供任何宿主能力
	// （Tool 若声明了 capability，运行期访问会收到 ErrCapabilityUnavailable）。
	host *cap.Providers
	// logSink 是执行日志采集器（REQ-API-LOGS / #71）；nil 表示不记日志。
	logSink LogSink
}

// New 构造引擎。可用 WithMaxParallel / WithHost / WithSandbox 等选项覆盖默认行为。
func New(reg *registry.Registry, opts ...func(*Engine)) *Engine {
	e := &Engine{reg: reg, maxParallel: DefaultMaxParallel, sandboxPolicy: SandboxStrict}
	for _, o := range opts {
		o(e)
	}
	return e
}

// WithMaxParallel 设置并发上限；n < 1 时取 1（退化为串行执行）。
func WithMaxParallel(n int) func(*Engine) {
	return func(e *Engine) {
		if n < 1 {
			n = 1
		}
		e.maxParallel = n
	}
}

// WithHost 注入宿主能力原语实现（PRD §6 的 http/call/db/kv/secret/event）。
// 不调用时 Engine 无宿主能力，Tool 声明的 capability 运行期会返回 ErrCapabilityUnavailable。
func WithHost(p *cap.Providers) func(*Engine) {
	return func(e *Engine) {
		e.host = p
	}
}

// LogSink 接收一次执行中每个 step 的运行日志行（REQ-API-LOGS / #71）。
// 参数为 (execID, stepID, stream, line)；stream ∈ {stdout, stderr}。
type LogSink func(execID, stepID, stream, line string)

// WithLogSink 注入执行日志采集器；execID 非空时引擎上报 step 日志，供
// GET /api/executions/{id}/logs 查询。不调用则不记日志。
func WithLogSink(fn LogSink) func(*Engine) {
	return func(e *Engine) {
		e.logSink = fn
	}
}

// WithSandbox 设置 Lua 沙箱策略（strict / permissive）。未知策略被忽略，保持默认 strict
// （绝不静默接受一个会关掉沙箱的值）。
func WithSandbox(policy string) func(*Engine) {
	return func(e *Engine) {
		switch policy {
		case SandboxStrict, SandboxPermissive:
			e.sandboxPolicy = policy
		}
	}
}

// hostFor 为一次执行构建宿主能力视图：按 Tool.Capabilities 精确授权，从引擎持有
// 的宿主实现集中取原语。注册期已校验 Capabilities 合法（model.Tool.Validate），故此处忽略错误。
func (e *Engine) hostFor(tool *model.Tool) *cap.Context {
	grants, _ := cap.GrantsFromStrings(tool.Capabilities)
	return cap.New(grants, e.host)
}

// secretResolver 返回执行期 ${secrets.x} 的解析器（REQ-TMPL-SECRETS）。
// 宿主未注入 secret 实现时返回 nil —— 模板解析会据此显式报错，而非用空值静默替换。
// 设计要点：Flow 定义只存 `${secrets.api_key}` 引用，明文密钥始终留在宿主 secret 源
// （如环境变量注入的 map），绝不落库，规避「secret 经注册期定义泄露」。
func (e *Engine) secretResolver() func(name string) (string, error) {
	if e.host == nil || e.host.Secret == nil {
		return nil
	}
	return func(name string) (string, error) {
		return e.host.Secret.Get(name)
	}
}

// logSinkFor 返回绑定了 execID 的 step 级日志回调；execID 为空或无 sink 时返回 nil
// （不记日志，兼容无日志采集的调用方）。
func (e *Engine) logSinkFor(execID string) func(stepID, stream, line string) {
	if e.logSink == nil || execID == "" {
		return nil
	}
	return func(stepID, stream, line string) {
		e.logSink(execID, stepID, stream, line)
	}
}

// RunResult 是一次 Flow 执行的结构化结果。
// Steps：step id → {input, output}（**仅已执行**的 step）。
// Skipped：step id → 跳过原因（when 为假或上游被跳过）——「没跑」与「跑失败」必须分开表达（F6）。
//
// JSON tag 是**对外契约的一部分**（REST /api/executions 直接序列化它），故显式声明
// snake_case，避免默认导出 Go 字段名（FlowID/Steps）泄漏到 API 形状里。
type RunResult struct {
	FlowID  string                            `json:"flow_id"`
	Steps   map[string]map[string]interface{} `json:"steps,omitempty"`
	Skipped map[string]string                 `json:"skipped,omitempty"`
}

// RunFlow 一次性执行（flow run）：所有 step 必须为 once 模式。
// 遇到 service 模式 step 报错，提示改用 app run（F3：service 长驻不能一次性执行）。
func (e *Engine) RunFlow(flowID string) error {
	_, err := e.RunFlowResult(flowID)
	return err
}

// RunFlowResult 是 RunFlow 的带结果版本，返回每个 step 的最终 input/output。
func (e *Engine) RunFlowResult(flowID string) (*RunResult, error) {
	return e.RunFlowResultObserved(flowID, nil)
}

// ToolObserver 接收一次运行「可能用到」的 Tool id 集合（已去重、升序）。
type ToolObserver func(toolIDs []string)

// RunFlowResultObserved 与 RunFlowResult 相同，但在**加载完 Flow 与 Hook 快照之后**
// 回调一次 observe，告知本次运行可能用到的 Tool。
//
// 为什么需要「运行期引用」可观测（PRD §8.2 第三条守卫）：
// Registry 的注销守卫只能看到**静态**引用（Flow 的 step.call / Hook 的 action）。
// 一旦 Hook 被删除、或 Flow 被重新注册去掉了该 step，静态引用就消失了，
// 但**正在跑的执行**仍持有这些 Tool（它在启动时已把 Hook 快照读进内存）——
// 此时若允许注销，正在跑的执行会在下次触发 Hook 时拿到 nil Tool 而中途失败。
// 唯一可靠的判据来自运行侧，所以由引擎在快照点上报，而不是让 Registry 去猜。
func (e *Engine) RunFlowResultObserved(flowID string, observe ToolObserver) (*RunResult, error) {
	return e.RunFlowExec(flowID, "", observe)
}

// RunFlowExec 是 RunFlowResultObserved 的带执行 id 版本：execID 非空时，引擎会把每个
// step 的运行日志（stdout/stderr 行）经 logSink 上报（REQ-API-LOGS / #71），供
// `GET /api/executions/{id}/logs` 查询。execID 为空则行为与 RunFlowResultObserved 完全一致
// （不记日志，兼容历史调用点与测试）。
func (e *Engine) RunFlowExec(flowID, execID string, observe ToolObserver) (*RunResult, error) {
	flow, err := e.reg.GetFlow(flowID)
	if err != nil {
		return nil, err
	}
	if flow == nil {
		return nil, fmt.Errorf("Flow 不存在: %s", flowID)
	}
	for _, s := range flow.Steps {
		if s.IsService() {
			return nil, fmt.Errorf("step %s 是 service 模式（长驻），不能用 flow run 一次性执行；请用 `app run %s` 监管", s.ID, flowID)
		}
	}
	return e.run(flow, observe, execID)
}

// plannedToolIDs 计算一次运行可能用到的 Tool id：step 的 call ∪ 生效 Hook 的 action。
//
// Hook 的选择规则**复用 hooksFor**（applyHooks 用的同一个函数），不在此重写匹配逻辑——
// 否则「哪些 Hook 会触发」就出现了第二处判定，日后加 stage 或改 target 语义时必漏一处
// （G2/M10 的教训：同一判定出现两处即漂移）。
func plannedToolIDs(flow *model.Flow, hooks []model.Hook) []string {
	seen := map[string]bool{}
	for _, s := range flow.Steps {
		seen[s.Call] = true
		for _, stage := range []string{model.StageBeforeStep, model.StageAfterStep, model.StageOnError} {
			for _, h := range hooksFor(hooks, stage, s.Call) {
				seen[h.Action] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Strings(out) // 稳定输出，便于测试与日志断言
	return out
}

func (e *Engine) run(flow *model.Flow, observe ToolObserver, execID string) (*RunResult, error) {
	levels, err := topoLevels(flow.Steps)
	if err != nil {
		return nil, err
	}

	hooks, err := e.loadHooks()
	if err != nil {
		return nil, err
	}

	// 在快照点上报「本次运行可能用到的 Tool」——这是上层实现 §8.2「运行中不可注销」
	// 守卫的唯一可靠依据（静态引用会在 Hook 被删 / Flow 被改写后消失）。
	if observe != nil {
		observe(plannedToolIDs(flow, hooks))
	}

	rc := &runContext{
		vars:      flow.Vars,
		steps:     map[string]map[string]interface{}{},
		secretGet: e.secretResolver(),
		execID:    execID,
		logSink:   e.logSinkFor(execID),
	}

	// when 跳过需要沿**有效依赖**（含推断边）传播：上游被跳过 ⇒ 下游也跳过。
	deps, err := depMap(flow.Steps)
	if err != nil {
		return nil, err
	}
	skipped := map[string]string{} // stepID -> 跳过原因
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

	// S4：分层执行——同一层内无依赖的 step 并行调度（PRD §4.3），受 maxParallel 限流。
	for _, level := range levels {
		err := e.execLevel(flow.Steps, level, func(ctx context.Context, step model.Step, tool *model.Tool) error {
			if reason, ok := skipReasonFor(step.ID); ok {
				markSkipped(step.ID, reason)
				fmt.Printf("- step %s 跳过（%s）\n", step.ID, reason)
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

	fmt.Printf("✓ Flow %s 执行完成，共 %d 步", flow.ID, len(flow.Steps))
	if n := len(skipped); n > 0 {
		fmt.Printf("，跳过 %d 步", n)
	}
	fmt.Println()
	return &RunResult{FlowID: flow.ID, Steps: rc.steps, Skipped: skipped}, nil
}

// execLevel 并发执行一层 step（受 maxParallel 限制）。
//
// 失败语义（fail-closed）：首个错误即取消本层上下文（阻止未启动的 step 起跑，
// 并让在跑的子进程随 ctx 被杀），等待本层全部 goroutine 收尾后返回该错误；
// 后续层不再执行。
func (e *Engine) execLevel(steps []model.Step, level []int, fn func(ctx context.Context, step model.Step, tool *model.Tool) error) error {
	lvlCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sem := make(chan struct{}, e.maxParallel)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error

	record := func(err error) {
		mu.Lock()
		if firstErr == nil {
			firstErr = err
		}
		mu.Unlock()
		cancel()
	}

loop:
	for _, idx := range level {
		select {
		case <-lvlCtx.Done():
			break loop
		case sem <- struct{}{}:
		}
		wg.Add(1)
		step := steps[idx]
		go func(step model.Step) {
			defer wg.Done()
			defer func() { <-sem }()

			tool, err := e.reg.GetTool(step.Call)
			if err != nil {
				record(err)
				return
			}
			if tool == nil {
				record(fmt.Errorf("step %s 引用的 Tool 不存在: %s", step.ID, step.Call))
				return
			}
			resolved, err := resolveToolVersion(tool, step)
			if err != nil {
				record(err)
				return
			}
			if err := fn(lvlCtx, step, resolved); err != nil {
				record(err)
			}
		}(step)
	}
	wg.Wait()
	return firstErr
}

// 拓扑排序 / 依赖推断见 deps.go（topoLevels / topoOrder / effectiveDeps）。

// runContext 保存跨 step 的数据流上下文。
//
// S4 并发安全：同一层的 step 会**并行**执行并写入 steps，故所有访问都经 mu 保护。
// vars 是 Flow 级只读（并行 step 不写 vars），无并发写问题。
type runContext struct {
	mu    sync.Mutex
	vars  map[string]interface{}
	steps map[string]map[string]interface{}
	// secretGet 是执行期 secret 解析器（REQ-TMPL-SECRETS）：从宿主 secret 源取密钥明文。
	// nil 表示宿主未注入 secret 实现，${secrets.x} 解析时显式报错（不静默失败）。
	secretGet func(name string) (string, error)
	// execID 是当前执行的 id（供日志采集关联）；空表示不记日志。
	execID string
	// logSink 是 step 级日志回调（REQ-API-LOGS / #71）；nil 表示不记日志。
	logSink func(stepID, stream, line string)
}

// setStep 记录一个 step 的最终 input/output（并发安全）。
func (c *runContext) setStep(id string, input, output map[string]interface{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.steps[id] = map[string]interface{}{"input": input, "output": output}
}

// snapshot 返回 vars 与 steps 的**浅拷贝**，供 when 求值等只读场景使用。
// 直接把 map 交给外部读取会在并行写入时触发 data race，故这里在锁内拷贝外层 map
// （内层 map 由 setStep 整体替换、从不原地修改，故浅拷贝足够）。
func (c *runContext) snapshot() (map[string]interface{}, map[string]map[string]interface{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	steps := make(map[string]map[string]interface{}, len(c.steps))
	for k, v := range c.steps {
		steps[k] = v
	}
	return c.vars, steps
}

// resolveInput 对 step.input 做模板替换（vars / steps 引用）。
// 持有读锁期间完成解析，避免并发读到「写了一半」的 steps。
func resolveInput(in map[string]interface{}, ctx *runContext) (map[string]interface{}, error) {
	ctx.mu.Lock()
	defer ctx.mu.Unlock()

	out := make(map[string]interface{}, len(in))
	for k, v := range in {
		rv, err := resolveValue(v, ctx)
		if err != nil {
			return nil, err
		}
		out[k] = rv
	}
	return out, nil
}

func resolveValue(v interface{}, ctx *runContext) (interface{}, error) {
	switch x := v.(type) {
	case string:
		return expandString(x, ctx)
	case map[string]interface{}:
		m := make(map[string]interface{}, len(x))
		for k, vv := range x {
			rv, err := resolveValue(vv, ctx)
			if err != nil {
				return nil, err
			}
			m[k] = rv
		}
		return m, nil
	case []interface{}:
		s := make([]interface{}, len(x))
		for i, vv := range x {
			rv, err := resolveValue(vv, ctx)
			if err != nil {
				return nil, err
			}
			s[i] = rv
		}
		return s, nil
	default:
		return v, nil
	}
}

// 模板语法（expandString / lookup / walk / scanTemplateRefs / validateTemplates）见 template.go——
// 解析是一次实现、三方共用（运行期展开 / 依赖推断 / 注册期静态校验），不再放在本文件里
// 以免又长出第二份「什么算引用」的判定。

// validate 用 JSON Schema 校验 instance；schema 为空则跳过。
func validate(schema map[string]interface{}, instance interface{}) error {
	if len(schema) == 0 {
		return nil
	}
	schemaBytes, err := json.Marshal(schema)
	if err != nil {
		return fmt.Errorf("序列化 schema 失败: %w", err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("schema.json", bytes.NewReader(schemaBytes)); err != nil {
		return fmt.Errorf("编译 schema 失败: %w", err)
	}
	sch, err := c.Compile("schema.json")
	if err != nil {
		return fmt.Errorf("编译 schema 失败: %w", err)
	}
	if err := sch.Validate(instance); err != nil {
		return fmt.Errorf("%w", err)
	}
	return nil
}
