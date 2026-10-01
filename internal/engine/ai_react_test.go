package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gear/internal/model"
	"gear/internal/registry"
)

// 本文件覆盖 ext.ai 的 agent 层（ai_agent.go 的 Generate/React + ai_validate.go）。
// 重点是 ReAct 循环：它把 LLM 的自由文本变成「调用宿主能力」的动作，每一步都可能被
// 模型带偏，因此每条护栏各测一次（不静默降级、不无限循环、不越权）：
//   - 执行器未装配 / 任务为空 → 显式报错（不假装跑过）；
//   - allow 名单外的工具 → 拒绝并回灌提示（不越权）；
//   - 相同工具+参数的重复调用 → 拦住（防死循环烧钱）；
//   - 输出既非 Action 也非 Final Answer → 回灌格式要求继续（不硬失败）；
//   - 达到最大步数仍未收尾 → 明说「未给出 Final Answer」，绝不编造答案。

// scriptedLLM 按脚本逐轮返回文本，并记录收到的请求（用于断言 opts 是否真的透传）。
type scriptedLLM struct {
	texts []string
	// calls 让测试能驱动「模型要求调工具」这条路径：calls[i] 非空时，第 i 轮除 texts[i]
	// 外还带上这些 tool_calls。与 texts 共用下标，保证「第 i 轮说了什么 + 要求调什么」是写死的。
	calls [][]ToolCall
	i     int
	got   []LLMRequest
	err   error
}

func (s *scriptedLLM) Complete(_ context.Context, req LLMRequest) (string, error) {
	s.got = append(s.got, req)
	if s.err != nil {
		return "", s.err
	}
	if s.i >= len(s.texts) {
		return "", fmt.Errorf("脚本已耗尽（第 %d 次调用）", s.i+1)
	}
	t := s.texts[s.i]
	s.i++
	return t, nil
}

// CompleteTools 复用同一份脚本：texts[i] 作正文，calls[i]（若有）作该轮的工具调用。
// 走它与走 Complete 消耗同一个下标，因此「先调工具、再给答案」这类多轮脚本可以一次写全。
func (s *scriptedLLM) CompleteTools(_ context.Context, req LLMRequest, _ []ToolSpec) (ChatResult, error) {
	s.got = append(s.got, req)
	if s.err != nil {
		return ChatResult{}, s.err
	}
	if s.i >= len(s.texts) {
		return ChatResult{}, fmt.Errorf("脚本已耗尽（第 %d 次调用）", s.i+1)
	}
	t := s.texts[s.i]
	var calls []ToolCall
	if s.i < len(s.calls) {
		calls = s.calls[s.i]
	}
	s.i++
	return ChatResult{Text: t, ToolCalls: calls}, nil
}

func (s *scriptedLLM) CompleteStream(_ context.Context, req LLMRequest, onDelta func(content, reasoning string) error) error {
	s.got = append(s.got, req)
	if s.err != nil {
		return s.err
	}
	if s.i >= len(s.texts) {
		return fmt.Errorf("脚本已耗尽（第 %d 次调用）", s.i+1)
	}
	t := s.texts[s.i]
	s.i++
	if onDelta != nil {
		return onDelta(t, "")
	}
	return nil
}

func seedTool(t *testing.T, reg *registry.Registry) {
	t.Helper()
	if err := reg.UpsertTool(&model.Tool{
		ID: "demo.echo", Name: "回声", Description: "回显输入", Runtime: model.RuntimeLua,
		Entry:        "return ctx.input",
		InputSchema:  map[string]interface{}{"type": "object"},
		OutputSchema: map[string]interface{}{"type": "object"},
		Versions:     []model.ToolVersion{{Version: "1.0.0", State: model.StateStable}},
	}); err != nil {
		t.Fatalf("注册工具失败: %v", err)
	}
}

// newReactAgent 构造一个可跑 React 的 agent：exec 记录每次调用，返回固定 observation。
func newReactAgent(t *testing.T, llm LLM) (*AIAgent, *[]string) {
	t.Helper()
	reg := newTestRegistry(t)
	t.Cleanup(func() { _ = reg.Close() })
	seedTool(t, reg)
	a := newAIAgent(reg, llm, stubRecipes{}, nil)
	entries := &[]string{}
	a.exec = func(_ context.Context, entry string, _ map[string]interface{}) (map[string]interface{}, error) {
		*entries = append(*entries, entry)
		return map[string]interface{}{"result": "ok"}, nil
	}
	return a, entries
}

func actionJSON(tool, args string) string {
	return "Thought: 我需要先做一步\nAction:\n```json\n{\"tool\": \"" + tool + "\", \"args\": " + args + "}\n```"
}

// ---- React ----

func TestReactFailsClosedWithoutExecutorOrTask(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	a := newAIAgent(reg, &scriptedLLM{}, stubRecipes{}, nil)

	if _, err := a.React(context.Background(), "任务", ReactOpts{}); err == nil ||
		!strings.Contains(err.Error(), "未装配执行器") {
		t.Fatalf("未注入 exec 时必须显式报错（不能静默跑空循环），实际 %v", err)
	}

	a.exec = func(context.Context, string, map[string]interface{}) (map[string]interface{}, error) {
		return nil, nil
	}
	if _, err := a.React(context.Background(), "", ReactOpts{}); err == nil ||
		!strings.Contains(err.Error(), "task") {
		t.Fatalf("空 task 必须报错，实际 %v", err)
	}
	if _, err := a.ReactMethod(map[string]interface{}{}); err == nil {
		t.Fatal("ReactMethod 缺 task 应报错")
	}
}

func TestReactHappyPathExecutesToolThenAnswers(t *testing.T) {
	llm := &scriptedLLM{texts: []string{
		actionJSON("db.exec", `{"sql":"SELECT 1"}`),
		"Thought: 已经拿到结果\nFinal Answer: 答案是 42",
	}}
	a, entries := newReactAgent(t, llm)

	res, err := a.React(context.Background(), "查一下", ReactOpts{})
	if err != nil {
		t.Fatalf("React 失败: %v", err)
	}
	m, _ := res.(map[string]interface{})
	if m["answer"] != "答案是 42" {
		t.Fatalf("answer 应为 Final Answer 的内容，实际 %q", m["answer"])
	}
	used, _ := m["used_tools"].([]string)
	if len(used) != 1 || used[0] != "db.exec" {
		t.Fatalf("used_tools 应记录实际调用过的工具，实际 %v", used)
	}
	steps, _ := m["steps"].([]map[string]interface{})
	if len(steps) != 2 {
		t.Fatalf("应记录 2 步（工具步 + 收尾步），实际 %d", len(steps))
	}
	if len(*entries) != 1 || !strings.Contains((*entries)[0], "ctx.db.exec") {
		t.Fatalf("工具调用必须经宿主能力通道执行，实际 entry=%v", *entries)
	}
	// 缺省值：未给温度/步数时按约定值，不能退化成 0（否则一次都不跑）
	if llm.got[0].Temperature != 0.3 {
		t.Fatalf("默认 temperature 应为 0.3，实际 %v", llm.got[0].Temperature)
	}
}

func TestReactAllowListBlocksUnlistedTool(t *testing.T) {
	llm := &scriptedLLM{texts: []string{
		actionJSON("db.exec", `{"sql":"SELECT 1"}`),
		"Final Answer: 放弃",
	}}
	a, entries := newReactAgent(t, llm)

	res, _ := a.React(context.Background(), "任务", ReactOpts{Allow: []string{"http"}, MaxSteps: 2})
	steps, _ := res.(map[string]interface{})["steps"].([]map[string]interface{})
	if len(*entries) != 0 {
		t.Fatal("allow 名单外的工具不得被执行（不越权）")
	}
	if !strings.Contains(fmt.Sprint(steps[0]["observation"]), "不在允许列表") {
		t.Fatalf("应回灌「不在允许列表」的提示，实际 %v", steps[0]["observation"])
	}
	if used, _ := res.(map[string]interface{})["used_tools"].([]string); len(used) != 0 {
		t.Fatalf("被拦下的工具不得计入 used_tools，实际 %v", used)
	}
}

func TestReactDuplicateCallIsStopped(t *testing.T) {
	// 同一工具 + 同一参数连续两次：第二次必须被拦（防模型陷入同一动作的死循环）。
	llm := &scriptedLLM{texts: []string{
		actionJSON("db.exec", `{"sql":"SELECT 1"}`),
		actionJSON("db.exec", `{"sql":"SELECT 1"}`),
	}}
	a, entries := newReactAgent(t, llm)

	res, _ := a.React(context.Background(), "任务", ReactOpts{MaxSteps: 2})
	steps, _ := res.(map[string]interface{})["steps"].([]map[string]interface{})
	if len(*entries) != 1 {
		t.Fatalf("重复调用不得重复执行，实际执行 %d 次", len(*entries))
	}
	if !strings.Contains(fmt.Sprint(steps[1]["observation"]), "重复调用") {
		t.Fatalf("第二次应回灌重复调用提示，实际 %v", steps[1]["observation"])
	}
}

func TestReactMalformedOutputAndMaxSteps(t *testing.T) {
	// 两轮都给不出合法格式 → 跑满 MaxSteps 后明确收尾（脚本要够长，否则第 2 步会因
	// 「脚本耗尽」直接中止，测不到「步数用尽」这条路径）。
	llm := &scriptedLLM{texts: []string{"嗯，我不知道该干什么", "还是不知道"}}
	a, _ := newReactAgent(t, llm)

	res, _ := a.React(context.Background(), "任务", ReactOpts{MaxSteps: 2})
	steps, _ := res.(map[string]interface{})["steps"].([]map[string]interface{})
	if !strings.Contains(fmt.Sprint(steps[0]["observation"]), "严格按格式") {
		t.Fatalf("格式不对时应回灌格式要求（不硬失败），实际 %v", steps[0]["observation"])
	}
	ans, _ := res.(map[string]interface{})["answer"].(string)
	if !strings.Contains(ans, "未给出 Final Answer") {
		t.Fatalf("跑满步数仍未收尾时必须明说，不得编造答案，实际 %q", ans)
	}
}

func TestReactPropagatesExecutorErrorAsObservation(t *testing.T) {
	llm := &scriptedLLM{texts: []string{actionJSON("db.exec", `{"sql":"SELECT 1"}`)}}
	a, _ := newReactAgent(t, llm)
	a.exec = func(context.Context, string, map[string]interface{}) (map[string]interface{}, error) {
		return nil, errors.New("插件崩了")
	}
	res, _ := a.React(context.Background(), "任务", ReactOpts{MaxSteps: 1})
	steps, _ := res.(map[string]interface{})["steps"].([]map[string]interface{})
	if !strings.Contains(fmt.Sprint(steps[0]["observation"]), "工具执行错误") {
		t.Fatalf("执行器错误应作为 observation 回灌（循环继续，不当场崩），实际 %v", steps[0]["observation"])
	}
}

func TestReactBuildActionEntryFailureIsSurfaced(t *testing.T) {
	// 编不出 Lua 入口（例如内置原语没有该方法）→ 作为 observation 回灌，不能 panic。
	llm := &scriptedLLM{texts: []string{actionJSON("db.nope", `{}`)}}
	a, entries := newReactAgent(t, llm)
	res, _ := a.React(context.Background(), "任务", ReactOpts{MaxSteps: 1})
	if len(*entries) != 0 {
		t.Fatal("构建失败的调用不得执行")
	}
	steps, _ := res.(map[string]interface{})["steps"].([]map[string]interface{})
	if !strings.Contains(fmt.Sprint(steps[0]["observation"]), "构建工具调用失败") {
		t.Fatalf("构建失败必须回灌给模型，实际 %v", steps[0]["observation"])
	}
}

func TestReactLLMFailureAborts(t *testing.T) {
	llm := &scriptedLLM{err: errors.New("连接不上模型")}
	a, _ := newReactAgent(t, llm)
	if _, err := a.React(context.Background(), "任务", ReactOpts{}); err == nil ||
		!strings.Contains(err.Error(), "LLM 调用失败") {
		t.Fatalf("LLM 失败必须中止并说明是第几步，实际 %v", err)
	}
}

func TestReactMethodPassesOptsThrough(t *testing.T) {
	llm := &scriptedLLM{texts: []string{"Final Answer: done"}}
	a, _ := newReactAgent(t, llm)
	if _, err := a.ReactMethod(map[string]interface{}{
		"task":        "任务",
		"max_steps":   float64(3),
		"allow":       []interface{}{"db"},
		"provider":    "p",
		"model":       "m",
		"base_url":    "http://x",
		"api_key":     "k",
		"temperature": float64(0.9),
	}); err != nil {
		t.Fatalf("ReactMethod 失败: %v", err)
	}
	got := llm.got[0]
	if got.Provider != "p" || got.Model != "m" || got.BaseURL != "http://x" || got.APIKey != "k" || got.Temperature != 0.9 {
		t.Fatalf("ReactMethod 的 opts 必须透传给 LLM，实际 %+v", got)
	}
}

func TestReactPromptAndToolsText(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	a := newAIAgent(reg, &scriptedLLM{}, stubRecipes{}, map[string]map[string]MethodSchema{
		"ext.demo": {"run": {Description: "演示扩展方法", Parameters: map[string]ParamSchema{"x": {Type: "string"}}}},
	})

	all := a.reactSystemPrompt(reactAllow{all: true}, 4, false, false)
	if !strings.Contains(all, "最多 4 步") || !strings.Contains(all, "Final Answer") {
		t.Fatal("系统提示须含步数上限与输出格式约定")
	}
	if !strings.Contains(all, "ctx.ext.demo.run") {
		t.Fatal("已注册的 ext.* 应出现在可用工具清单里")
	}
	// 走 function calling 时必须**不再**要求手写 ```json 围栏：协议已经给了调用通道，
	// 再让模型手打一遍 JSON 就是基线 B-1 那种「两头打架」。
	withTools := a.reactSystemPrompt(reactAllow{all: true}, 4, true, false)
	if strings.Contains(withTools, "```json") {
		t.Fatal("下发 function 声明后不应再要求模型手写 json 围栏")
	}
	if !strings.Contains(withTools, "工具调用") {
		t.Fatal("走协议时应提示用工具调用机制")
	}
	// brief 模式只给「能力 → 方法名」索引：参数细节已在 function 声明里，
	// 再抄一遍就是同一份信息付两遍钱（全目录 115 个方法，双重下发单轮 10-15k token）。
	if strings.Contains(withTools, "ctx.ext.demo.run({") {
		t.Fatal("下发 function 声明时提示词里不应重复列出方法参数")
	}
	if !strings.Contains(withTools, "- ext.demo:") {
		t.Fatal("brief 模式应列出能力与允许的方法名索引")
	}
	only := a.reactToolsText(reactAllow{names: []string{"db"}}, false)
	if strings.Contains(only, "ctx.http") {
		t.Fatal("allow 过滤后不得列出未授权的原语")
	}
	if !strings.Contains(only, "ctx.db.exec") {
		t.Fatal("allow 内的原语应列出")
	}
}

// TestReactAllowMethodLevel：allow 必须支持方法级（"ext.redis.set"）。
//
// 动机是工具面膨胀：只想要 ext.redis 的两个方法，旧语义也得把 29 个方法全塞进请求，
// 工具越多模型越选不准。方法级 allow 让「给权限」与「给工具面」解耦。
func TestReactAllowMethodLevel(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	a := newAIAgent(reg, &scriptedLLM{}, stubRecipes{}, map[string]map[string]MethodSchema{
		"ext.demo": {
			"run":  {Description: "跑", Parameters: map[string]ParamSchema{"x": {Type: "string"}}},
			"stop": {Description: "停", Parameters: map[string]ParamSchema{"y": {Type: "string"}}},
		},
	})

	// 只允许一个方法：另一个不得进入工具声明。
	specs, byName := a.reactToolSpecs(reactAllow{names: []string{"ext.demo"}, methods: map[string]bool{"ext.demo.run": true}}, false)
	if len(specs) != 1 {
		t.Fatalf("方法级 allow 应只产出 1 个工具，实际 %d：%v", len(specs), byName)
	}
	if _, ok := byName[SanitizeToolName("ext.demo.run")]; !ok {
		t.Fatalf("应保留 ext.demo.run，实际 %v", byName)
	}
	// 守卫层同样按方法收窄：调未开放的方法要被拒。
	al := reactAllow{names: []string{"ext.demo"}, methods: map[string]bool{"ext.demo.run": true}}
	if !al.allows("ext.demo", "run") {
		t.Fatal("已开放的方法应放行")
	}
	if al.allows("ext.demo", "stop") {
		t.Fatal("未开放的方法必须被拒")
	}
}

// TestReactAllowUnknownRejected：显式 allow 里认不出的项必须报错，不能静默忽略——
// 静默忽略等于让调用方以为开了权限实际没开，agent 调不到工具还不知道为什么。
func TestReactAllowUnknownRejected(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	a := newAIAgent(reg, &scriptedLLM{}, stubRecipes{}, nil)
	if _, err := a.resolveReactAllow([]string{"ext.nope"}, true, true); err == nil {
		t.Fatal("未知能力必须报错")
	}
	if _, err := a.resolveReactAllow([]string{"ext.ai.no_such_method"}, true, true); err == nil {
		t.Fatal("未知方法必须报错")
	}
	// 通配符仍可用，但必须显式给。
	al, err := a.resolveReactAllow([]string{ReactAllowAll}, true, true)
	if err != nil || !al.all {
		t.Fatalf("显式 %q 应得到不设限，实际 %+v err=%v", ReactAllowAll, al, err)
	}
	// 从 Tool 声明继承来的能力清单走宽松口径：没装配的能力跳过而不是打挂整个 react
	// （那是定义层面的问题，注册期已校验过合法性）。
	got, err := a.resolveReactAllow([]string{"ext.nope", "http"}, true, false)
	if err != nil || len(got.names) != 1 || got.names[0] != "http" {
		t.Fatalf("继承来的清单应跳过未装配项，实际 %+v err=%v", got, err)
	}
}

// TestReactDefaultCapabilitySetIsNarrow：Go 直调没给 allow 时必须是收窄的默认集，
// 不再是整个能力目录（115 个方法会让模型选不准）。
func TestReactDefaultCapabilitySetIsNarrow(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	a := newAIAgent(reg, &scriptedLLM{}, stubRecipes{}, nil)

	def, err := a.resolveReactAllow(nil, false, true)
	if err != nil {
		t.Fatalf("默认集解析失败: %v", err)
	}
	if def.all {
		t.Fatal("默认集不得是不设限")
	}
	specs, _ := a.reactToolSpecs(def, false)
	if len(specs) == 0 {
		t.Fatal("默认集不得为空")
	}
	// 默认集必须远小于全目录，且不超工具数上限。
	allSpecs, _ := a.reactToolSpecs(reactAllow{all: true}, false)
	if len(specs) >= len(allSpecs) {
		t.Fatalf("默认集(%d) 必须小于全目录(%d)", len(specs), len(allSpecs))
	}
	if len(specs) > maxReactTools {
		t.Fatalf("默认集(%d) 不得超过工具数上限 %d", len(specs), maxReactTools)
	}
}

// TestReactToolBudgetRejected：工具数超上限必须报错，绝不静默截断——
// 截断等于偷偷改了 agent 的能力边界，模型调不到却不知道为什么。
func TestReactToolBudgetRejected(t *testing.T) {
	a, _ := newReactAgent(t, &scriptedLLM{texts: []string{"Final Answer: x"}})

	// 造一个方法数超过上限的能力（真实目录里 ext.redis 一家 29 个方法，全目录 115 个，
	// 撞上限就是这个道理；测试不依赖真实目录规模，免得装了新扩展就变绿）。
	big := map[string]MethodSchema{}
	for i := 0; i < maxReactTools+5; i++ {
		big[fmt.Sprintf("m%02d", i)] = MethodSchema{Description: "占位"}
	}
	if a.extCaps == nil {
		a.extCaps = map[string]map[string]MethodSchema{}
	}
	a.extCaps["ext.big"] = big

	_, err := a.React(context.Background(), "任务", ReactOpts{Allow: []string{"ext.big"}, AllowSet: true})
	if err == nil {
		t.Fatal("工具数超过上限必须报错")
	}
	if !strings.Contains(err.Error(), "超过上限") {
		t.Fatalf("报错须点名「超过上限」并给出收窄办法，实际 %v", err)
	}

	// 方法级 allow 能把同一个能力收窄到上限内：这是「不砍能力、只收窄工具面」的出口。
	_, err = a.React(context.Background(), "任务", ReactOpts{
		Allow:    []string{"ext.big.m01", "ext.big.m02"},
		AllowSet: true,
	})
	if err != nil {
		t.Fatalf("方法级 allow 收窄后应能正常跑，实际 %v", err)
	}
}

// TestReactUsesNativeToolCalls：模型给 tool_calls 时，react 必须按协议执行——
// 不再去正文里抠 ```json 围栏。这是从「猜格式」换成「按协议」的判据。
func TestReactUsesNativeToolCalls(t *testing.T) {
	llm := &scriptedLLM{
		// 第一轮模型只给了 tool_calls、正文为空；第二轮给 Final Answer。
		texts: []string{"", "Thought: 已拿到结果\nFinal Answer: 答案是 42"},
		calls: [][]ToolCall{
			{{ID: "c1", Name: SanitizeToolName("db.exec"), Arguments: `{"sql":"SELECT 1"}`}},
			nil,
		},
	}
	a, entries := newReactAgent(t, llm)

	res, err := a.React(context.Background(), "查一下", ReactOpts{})
	if err != nil {
		t.Fatalf("React 失败: %v", err)
	}
	m, _ := res.(map[string]interface{})
	if m["answer"] != "答案是 42" {
		t.Fatalf("answer 应为 Final Answer，实际 %q", m["answer"])
	}
	// 压平名必须被反查回 db.exec，并编出真实调用入口。
	if len(*entries) != 1 {
		t.Fatalf("应只执行一次工具调用，实际 %v", *entries)
	}
	if !strings.Contains((*entries)[0], "ctx.db.exec") {
		t.Fatalf("应还原成 ctx.db.exec 的调用入口，实际 %q", (*entries)[0])
	}
	// 空正文不能被误判成「格式不遵守」——那正是基线 B-1 白扔一步的成因。
	steps, _ := m["steps"].([]map[string]interface{})
	for _, s := range steps {
		if obs, _ := s["observation"].(string); strings.Contains(obs, "既未输出合法") {
			t.Fatalf("走 tool_calls 的轮次不应报格式错误，实际 %q", obs)
		}
	}
}

// TestReactExecutesEveryToolCallInATurn：模型一轮给多个 tool_calls 时必须**全部**执行。
// 旧实现只跑 calls[0]、其余静默丢弃：模型以为调了、实际没调，下一轮拿不到任何结果，
// 只能原样重发，直到撞上重复调用守卫被判「疑似陷入循环」——一个不存在的死循环。
func TestReactExecutesEveryToolCallInATurn(t *testing.T) {
	llm := &scriptedLLM{
		texts: []string{"", "Final Answer: 完成"},
		calls: [][]ToolCall{
			{
				{ID: "c1", Name: SanitizeToolName("db.exec"), Arguments: `{"sql":"SELECT 1"}`},
				{ID: "c2", Name: SanitizeToolName("db.exec"), Arguments: `{"sql":"SELECT 2"}`},
			},
			nil,
		},
	}
	a, entries := newReactAgent(t, llm)
	if _, err := a.React(context.Background(), "查两件事", ReactOpts{}); err != nil {
		t.Fatalf("React 失败: %v", err)
	}
	if len(*entries) != 2 {
		t.Fatalf("一轮给了 2 个 tool_call 必须都执行，实际 %d 次：%v", len(*entries), *entries)
	}
	// 协议层：每个 tool_call 都要有同 id 的 tool 结果，少一条端点就判请求非法。
	next := llm.got[len(llm.got)-1]
	ids := map[string]bool{}
	for _, m := range next.Messages {
		if tc, ok := m["tool_calls"].([]map[string]any); ok {
			for _, c := range tc {
				if id, _ := c["id"].(string); id != "" {
					ids[id] = true
				}
			}
		}
	}
	if len(ids) != 2 {
		t.Fatalf("assistant 消息应带 2 个 tool_call，实际 %v", ids)
	}
	paired := 0
	for _, m := range next.Messages {
		if m["role"] == "tool" {
			if id, _ := m["tool_call_id"].(string); ids[id] {
				paired++
			}
		}
	}
	if paired != 2 {
		t.Fatalf("应有 2 条与 tool_call 配对的 tool 结果，实际 %d", paired)
	}
}

// TestReactFinalAnswerInThoughtDoesNotEndRun：正文里提到 Final Answer 不等于要收尾。
// 旧正则 `(?i)Final Answer\s*:\s*([\s\S]*)` 大小写不敏感且贪婪到结尾，模型在 Thought 里写
// 「下一步我会输出 Final Answer: ...」就当场被判收尾——循环一步没跑，答案里没有工具结果。
func TestReactFinalAnswerInThoughtDoesNotEndRun(t *testing.T) {
	llm := &scriptedLLM{
		texts: []string{"Thought: 我先调工具，下一步输出 Final Answer: 待定", "Final Answer: 真正答案"},
		calls: [][]ToolCall{
			{{ID: "c1", Name: SanitizeToolName("db.exec"), Arguments: `{"sql":"SELECT 1"}`}},
			nil,
		},
	}
	a, entries := newReactAgent(t, llm)
	res, err := a.React(context.Background(), "查一下", ReactOpts{})
	if err != nil {
		t.Fatalf("React 失败: %v", err)
	}
	m, _ := res.(map[string]interface{})
	// 本轮带 tool_calls 就绝不能判收尾：模型自己都还没看到工具结果。
	if m["answer"] != "真正答案" {
		t.Fatalf("带 tool_calls 的轮次不应判收尾，answer 应为第二轮的真正答案，实际 %q", m["answer"])
	}
	if len(*entries) != 1 {
		t.Fatalf("工具应真的执行了一次，实际 %v", *entries)
	}
}

// TestReactFinalAnswerTakesLastOccurrence：一处以上出现 Final Answer 时取**最后一次**。
// 模型常先起草再改口，取第一次会拿到被自己推翻的草稿。
func TestReactFinalAnswerTakesLastOccurrence(t *testing.T) {
	llm := &scriptedLLM{texts: []string{"Thought: 计划\nFinal Answer: 草稿\n\n重新想了想\nFinal Answer: 定稿"}}
	a, _ := newReactAgent(t, llm)
	res, err := a.React(context.Background(), "直接答", ReactOpts{})
	if err != nil {
		t.Fatalf("React 失败: %v", err)
	}
	if m, _ := res.(map[string]interface{}); m["answer"] != "定稿" {
		t.Fatalf("应取最后一次 Final Answer，实际 %q", m["answer"])
	}
}

// TestReactEmptyFinalAnswerDoesNotEndRun：写了「Final Answer:」却没内容，不算回答。
// 收一个空答案比多跑一步更难查——用户只看到 answer 是空串，不知道卡在哪。
func TestReactEmptyFinalAnswerDoesNotEndRun(t *testing.T) {
	llm := &scriptedLLM{texts: []string{"Final Answer:   \n", "Final Answer: 有内容了"}}
	a, _ := newReactAgent(t, llm)
	res, err := a.React(context.Background(), "答一下", ReactOpts{})
	if err != nil {
		t.Fatalf("React 失败: %v", err)
	}
	m, _ := res.(map[string]interface{})
	if m["answer"] != "有内容了" {
		t.Fatalf("空 Final Answer 不应收尾，实际 %q", m["answer"])
	}
	steps, _ := m["steps"].([]map[string]interface{})
	if len(steps) != 2 {
		t.Fatalf("空 Final Answer 那轮应被当作「未收尾」继续跑，实际 steps=%v", steps)
	}
	// 回灌的反馈必须自洽：它写了 Final Answer，就不能说它「未输出 Final Answer」。
	obs, _ := steps[0]["observation"].(string)
	if !strings.Contains(obs, "没给出内容") {
		t.Fatalf("应点明「写了 Final Answer 但没内容」，实际 %q", obs)
	}
	if strings.Contains(obs, "既未输出合法") {
		t.Fatalf("不得回灌自相矛盾的格式提示，实际 %q", obs)
	}
}

// TestReactNudgesFinalAnswerOnLastStep：倒数第二步用完后必须提醒「只剩最后 1 步」。
// 不提醒的话模型常在最后一步还要再调一次工具，于是步数用尽、拿不到 Final Answer，
// 用户看到的是「（达到最大步数 N，未给出 Final Answer）」。
func TestReactNudgesFinalAnswerOnLastStep(t *testing.T) {
	llm := &scriptedLLM{
		texts: []string{"", "Final Answer: 收尾"},
		calls: [][]ToolCall{{{ID: "c1", Name: SanitizeToolName("db.exec"), Arguments: `{}`}}, nil},
	}
	a, _ := newReactAgent(t, llm)
	if _, err := a.React(context.Background(), "任务", ReactOpts{MaxSteps: 2}); err != nil {
		t.Fatalf("React 失败: %v", err)
	}
	last := llm.got[len(llm.got)-1]
	for _, m := range last.Messages {
		if s, _ := m["content"].(string); strings.Contains(s, "只剩最后 1 步") {
			return
		}
	}
	t.Fatal("末步前应追加收敛引导，实际没有")
}

// TestReactCompactsOverBudgetContext：超预算折叠最早的观察，但**绝不删条目**——
// 删条目会破坏 assistant(tool_calls) ↔ tool(tool_call_id) 的配对，端点直接判请求非法。
func TestReactCompactsOverBudgetContext(t *testing.T) {
	msgs := []map[string]any{
		{"role": "system", "content": "系统提示"},
		{"role": "user", "content": "任务"},
		{"role": "assistant", "content": "", "tool_calls": []map[string]any{{"id": "c1"}}},
		{"role": "tool", "tool_call_id": "c1", "content": strings.Repeat("x", 40000)},
		{"role": "tool", "tool_call_id": "c2", "content": strings.Repeat("y", 40000)},
	}
	got := compactReactMessages(msgs)
	if len(got) != len(msgs) {
		t.Fatalf("折叠不得改变条数（协议配对会断），实际 %d → %d", len(msgs), len(got))
	}
	if s, _ := got[3]["content"].(string); !strings.Contains(s, "已折叠") {
		t.Fatalf("最早的观察应被折叠，实际 %q", s)
	}
	// 折叠后总长必须落到预算内，否则等于没压。
	total := 0
	for _, m := range got {
		total += len(fmt.Sprint(m["content"]))
	}
	if total > reactContextBudget {
		t.Fatalf("折叠后仍超预算：%d > %d", total, reactContextBudget)
	}
	// 负向：预算内不得动手。
	same := compactReactMessages([]map[string]any{{"role": "system", "content": "短"}})
	if len(same) != 1 {
		t.Fatal("未超预算时不应改动消息序列")
	}
}

// TestReactRejectsUnknownToolNameFromToolCall：模型给了目录里没有的工具名时，
// 必须明确回绝并列出可用工具，而不是丢一段 Lua 栈错误让它猜（基线 B-3）。
func TestReactRejectsUnknownToolNameFromToolCall(t *testing.T) {
	llm := &scriptedLLM{
		texts: []string{"", "Thought: 换个工具\nFinal Answer: 放弃"},
		calls: [][]ToolCall{
			{{ID: "c1", Name: "no_such_tool", Arguments: `{}`}},
			nil,
		},
	}
	a, entries := newReactAgent(t, llm)

	res, err := a.React(context.Background(), "随便查点什么", ReactOpts{})
	if err != nil {
		t.Fatalf("React 失败: %v", err)
	}
	if len(*entries) != 0 {
		t.Fatalf("未知工具名不应被执行，实际 %v", *entries)
	}
	m, _ := res.(map[string]interface{})
	steps, _ := m["steps"].([]map[string]interface{})
	if len(steps) == 0 {
		t.Fatal("应记录一步（被拒的那一步）")
	}
	obs, _ := steps[0]["observation"].(string)
	if !strings.Contains(obs, "不存在") {
		t.Fatalf("观察应明确说工具不存在，实际 %q", obs)
	}
	if !strings.Contains(obs, "可用工具") {
		t.Fatalf("回绝时应列出可用工具，实际 %q", obs)
	}
	if strings.Contains(obs, "attempt to index") {
		t.Fatalf("不应把 Lua 栈错误丢给模型，实际 %q", obs)
	}
}

// TestReactToolSpecsCoverCapabilityCatalog：工具声明必须与能力目录同源，
// 且压平名能反查回「能力.方法」（压平不可逆，反查表是唯一还原手段）。
func TestReactToolSpecsCoverCapabilityCatalog(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	a := newAIAgent(reg, &scriptedLLM{}, stubRecipes{}, map[string]map[string]MethodSchema{
		"ext.demo": {"run": {Description: "演示扩展方法", Parameters: map[string]ParamSchema{
			"x": {Type: "string", Required: true},
		}}},
	})

	specs, byName := a.reactToolSpecs(reactAllow{all: true}, false)
	if len(specs) == 0 {
		t.Fatal("能力目录非空时工具声明不得为空")
	}
	for _, s := range specs {
		if strings.Contains(s.Name, ".") {
			t.Fatalf("function name 不得含点（OpenAI 字符集限制）：%q", s.Name)
		}
		orig, ok := byName[s.Name]
		if !ok {
			t.Fatalf("压平名 %q 必须能反查回原名", s.Name)
		}
		if !strings.Contains(orig, ".") {
			t.Fatalf("反查结果应是「能力.方法」，实际 %q", orig)
		}
		if s.Parameters == nil {
			t.Fatalf("工具 %q 必须带 parameters schema", s.Name)
		}
	}
	if _, ok := byName[SanitizeToolName("ext.demo.run")]; !ok {
		t.Fatal("已注册的 ext.demo.run 应进入工具目录")
	}
	// required 要传下去，否则模型不知道哪个参数必填。
	var found bool
	for _, s := range specs {
		if s.Name == SanitizeToolName("ext.demo.run") {
			found = true
			req, _ := s.Parameters["required"].([]string)
			if len(req) != 1 || req[0] != "x" {
				t.Fatalf("必填参数应传进 schema，实际 %v", req)
			}
		}
	}
	if !found {
		t.Fatal("ext.demo.run 未出现在 specs 里")
	}

	// allow 过滤后只剩该能力名下的方法。
	specs2, byName2 := a.reactToolSpecs(reactAllow{names: []string{"ext.demo"}}, false)
	if len(specs2) == 0 {
		t.Fatal("allow 内应保留工具")
	}
	for _, s := range specs2 {
		if !strings.HasPrefix(byName2[s.Name], "ext.demo") {
			t.Fatalf("allow 过滤后不得出现 %q", byName2[s.Name])
		}
	}
}

// ---- Generate ----

func TestGenerateRejectsUnknownKind(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	a := newAIAgent(reg, &scriptedLLM{}, stubRecipes{}, nil)
	if _, err := a.Generate(context.Background(), "bogus", "意图", GenerateOpts{}); err == nil ||
		!strings.Contains(err.Error(), "flow|tool|hook") {
		t.Fatalf("非法 kind 必须被拒（未知枚举即拒），实际 %v", err)
	}
	if _, err := a.GenerateMethod(map[string]interface{}{"kind": "bogus", "intent": "x"}); err == nil {
		t.Fatal("GenerateMethod 同样应拒绝非法 kind")
	}
}

func TestGenerateExhaustsRetriesOnGarbageOutput(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	seedTool(t, reg)
	// 每趟生成链会多次调用 LLM（decompose/catalog/assemble），脚本要给够，
	// 否则会先撞上「脚本耗尽」而测不到「重试到上限」这条路径。
	garbage := make([]string, 12)
	for i := range garbage {
		garbage[i] = "这不是 JSON"
	}
	llm := &scriptedLLM{texts: garbage}
	a := newAIAgent(reg, llm, stubRecipes{}, nil)

	_, err := a.Generate(context.Background(), "flow", "做个流程", GenerateOpts{MaxRetries: 2})
	if err == nil || !strings.Contains(err.Error(), "重试") {
		t.Fatalf("LLM 一直给不出合法 JSON 时应重试到上限后报错（不静默返回空草稿），实际 %v", err)
	}
	if llm.i < 2 {
		t.Fatalf("应真的重试了多次，实际调用 %d 次", llm.i)
	}
}

func TestGenerateReturnsDraftWhenValidationPasses(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	seedTool(t, reg)
	// 每轮都给同一份合法 flow 草稿；校验注入为「通过」（本例测的是接线，不是校验规则）。
	llm := &scriptedLLM{texts: []string{
		`{"id":"gen.flow","name":"n","description":"d","steps":[{"id":"s1","call":"demo.echo","input":{}}]}`,
		`{"id":"gen.flow","name":"n","description":"d","steps":[{"id":"s1","call":"demo.echo","input":{}}]}`,
		`{"id":"gen.flow","name":"n","description":"d","steps":[{"id":"s1","call":"demo.echo","input":{}}]}`,
	}}
	a := newAIAgent(reg, llm, stubRecipes{}, nil)
	a.validateFlow = func(*model.Flow) error { return nil }

	res, err := a.Generate(context.Background(), "flow", "做个流程", GenerateOpts{RegisterInto: reg})
	if err != nil {
		t.Fatalf("Generate 应成功: %v", err)
	}
	if res.Kind != "flow" || res.Def["id"] != "gen.flow" {
		t.Fatalf("应返回草稿，实际 %+v", res)
	}
	if f, _ := reg.GetFlow("gen.flow"); f == nil {
		t.Fatal("RegisterInto 给定时草稿必须落库")
	}
}

func TestGeneratePropagatesRecipeLoadError(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	a := newAIAgent(reg, &scriptedLLM{}, failingRecipes{}, nil)
	if _, err := a.Generate(context.Background(), "flow", "x", GenerateOpts{}); err == nil ||
		!strings.Contains(err.Error(), "配方") {
		t.Fatalf("配方加载失败必须上抛（不静默用空提示词继续），实际 %v", err)
	}
}

type failingRecipes struct{}

func (failingRecipes) Load(string) (*Recipe, error) { return nil, errors.New("配方目录不可读") }

func TestGenerateMethodReturnsKindAndDef(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	seedTool(t, reg)
	def := `{"id":"gen.flow","name":"n","description":"d","steps":[{"id":"s1","call":"demo.echo","input":{}}]}`
	a := newAIAgent(reg, &scriptedLLM{texts: []string{def, def, def, def}}, stubRecipes{}, nil)
	a.validateFlow = func(*model.Flow) error { return nil }

	out, err := a.GenerateMethod(map[string]interface{}{"kind": "flow", "intent": "做个流程", "max_retries": float64(1)})
	if err != nil {
		t.Fatalf("GenerateMethod 失败: %v", err)
	}
	m, _ := out.(map[string]interface{})
	if m["kind"] != "flow" || m["def"] == nil {
		t.Fatalf("GenerateMethod 应返回 {kind, def}，实际 %v", m)
	}
}

func TestRegisterDefRoutesByKind(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	seedTool(t, reg)
	a := newAIAgent(reg, &scriptedLLM{}, stubRecipes{}, nil)

	flowDef := map[string]interface{}{
		"id": "gen.flow", "name": "n", "description": "d",
		"steps": []interface{}{map[string]interface{}{"id": "s1", "call": "demo.echo", "input": map[string]interface{}{}}},
	}
	if err := a.registerDef(reg, "flow", flowDef); err != nil {
		t.Fatalf("注册 flow 草稿失败: %v", err)
	}
	if f, _ := reg.GetFlow("gen.flow"); f == nil {
		t.Fatal("flow 草稿应落库")
	}
	hookDef := map[string]interface{}{"id": "gen.hook", "target": "*", "stage": "before_step", "action": "demo.echo"}
	if err := a.registerDef(reg, "hook", hookDef); err != nil {
		t.Fatalf("注册 hook 草稿失败: %v", err)
	}
	toolDef := map[string]interface{}{
		"id": "gen.tool", "name": "n", "description": "d", "runtime": "lua", "entry": "return {}",
		"input_schema": map[string]interface{}{"type": "object"}, "output_schema": map[string]interface{}{"type": "object"},
		"versions": []interface{}{map[string]interface{}{"version": "1.0.0", "state": "draft"}},
	}
	if err := a.registerDef(reg, "tool", toolDef); err != nil {
		t.Fatalf("注册 tool 草稿失败: %v", err)
	}
	if err := a.registerDef(reg, "tool", map[string]interface{}{"id": "bad"}); err == nil {
		t.Fatal("非法 Tool 草稿不得落库")
	}
}

// ---- Complete / Catalog / Domains / Capabilities / Validate ----

func TestCompleteTextJSONAndErrors(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	llm := &scriptedLLM{texts: []string{"纯文本", `{"a":1}`, "不是 JSON"}}
	a := newAIAgent(reg, llm, stubRecipes{}, nil)

	out, _ := a.Complete(map[string]interface{}{"prompt": "p"})
	if got := out.(map[string]interface{})["text"]; got != "纯文本" {
		t.Fatalf("非 json 模式应返回 {text}，实际 %v", out)
	}
	out, _ = a.Complete(map[string]interface{}{"prompt": "p", "format": "json"})
	if _, ok := out.(map[string]interface{})["data"].(map[string]interface{}); !ok {
		t.Fatalf("json 模式应返回 {data} 且解析为对象，实际 %v", out)
	}
	if _, err := a.Complete(map[string]interface{}{"format": "json"}); err == nil {
		t.Fatal("format=json 但模型没给合法 JSON 时必须报错（不静默回退成文本）")
	}
	llm.err = errors.New("模型不可用")
	if _, err := a.Complete(map[string]interface{}{}); err == nil {
		t.Fatal("LLM 错误必须上抛")
	}
}

func TestCatalogDomainsCapabilities(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	seedTool(t, reg)
	a := newAIAgent(reg, &scriptedLLM{}, stubRecipes{}, nil)

	out, err := a.Catalog(map[string]interface{}{"query": "echo", "k": float64(3)})
	if err != nil {
		t.Fatalf("Catalog 失败: %v", err)
	}
	if tools, _ := out.(map[string]interface{})["tools"].([]ToolSummary); len(tools) == 0 {
		t.Fatal("Catalog 应能检索到已注册工具")
	}
	out, err = a.Domains(map[string]interface{}{})
	if err != nil {
		t.Fatalf("Domains 失败: %v", err)
	}
	if ds, _ := out.(map[string]interface{})["domains"].([]DomainSummary); len(ds) == 0 {
		t.Fatal("Domains 应归类出能力域")
	}
	caps, err := a.CapabilitiesMethod()
	if err != nil {
		t.Fatalf("CapabilitiesMethod 失败: %v", err)
	}
	list, _ := caps.([]map[string]interface{})
	kinds := map[string]int{}
	for _, c := range list {
		kinds[fmt.Sprint(c["kind"])]++
	}
	if kinds["builtin"] == 0 || kinds["pure"] == 0 {
		t.Fatalf("能力目录应同时含受门禁内核原语与纯函数原子，实际 %v", kinds)
	}
}

func TestValidateRoutesByKind(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	seedTool(t, reg)
	a := newAIAgent(reg, &scriptedLLM{}, stubRecipes{}, nil)

	if err := a.Validate("bogus", nil, nil); err == nil {
		t.Fatal("非法 kind 必须被拒")
	}
	okDef := map[string]interface{}{
		"id": "f", "name": "n", "description": "d",
		"steps": []interface{}{map[string]interface{}{"id": "s1", "call": "demo.echo", "input": map[string]interface{}{}}},
	}
	if err := a.Validate("flow", okDef, nil); err != nil {
		t.Fatalf("合法 flow 草稿应通过退化校验: %v", err)
	}
	badDef := map[string]interface{}{
		"id": "f", "name": "n", "description": "d",
		"steps": []interface{}{map[string]interface{}{"id": "s1", "call": "不存在.的.东西", "input": map[string]interface{}{}}},
	}
	err := a.Validate("flow", badDef, nil)
	if err == nil || !strings.Contains(err.Error(), "退化校验") {
		t.Fatalf("call 目标不存在必须报错，且口径变窄时要在错误里明说（不静默降级），实际 %v", err)
	}
	// 引擎注入的静态校验优先（单一真相源，不产生第二套判定）
	called := false
	a.validateFlow = func(*model.Flow) error { called = true; return nil }
	if err := a.Validate("flow", badDef, nil); err != nil || !called {
		t.Fatalf("注入 validateFlow 后应走引擎静态校验，实际 err=%v called=%v", err, called)
	}

	toolDef := map[string]interface{}{
		"id": "t", "name": "n", "description": "d", "runtime": "lua", "entry": "return {}",
		"input_schema": map[string]interface{}{"type": "object"}, "output_schema": map[string]interface{}{"type": "object"},
		"versions": []interface{}{map[string]interface{}{"version": "1.0.0", "state": "draft"}},
	}
	if err := a.Validate("tool", toolDef, nil); err != nil {
		t.Fatalf("合法 tool 草稿应通过: %v", err)
	}
	if err := a.Validate("tool", map[string]interface{}{"id": "t"}, nil); err == nil {
		t.Fatal("缺字段的 tool 草稿必须被拒")
	}
	hookDef := map[string]interface{}{"id": "h", "target": "*", "stage": "before_step", "action": "demo.echo"}
	if err := a.Validate("hook", hookDef, nil); err != nil {
		t.Fatalf("合法 hook 草稿应通过: %v", err)
	}
	if err := a.Validate("hook", map[string]interface{}{"id": "h"}, nil); err == nil {
		t.Fatal("缺字段的 hook 草稿必须被拒")
	}
}

func TestValidateMethodReturnsOkFlag(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	seedTool(t, reg)
	a := newAIAgent(reg, &scriptedLLM{}, stubRecipes{}, nil)

	ok, err := a.ValidateMethod(map[string]interface{}{"kind": "hook", "def": map[string]interface{}{
		"id": "h", "target": "*", "stage": "before_step", "action": "demo.echo"}})
	if err != nil {
		t.Fatalf("ValidateMethod 不该抛错: %v", err)
	}
	if ok.(map[string]interface{})["ok"] != true {
		t.Fatalf("合法定义应返回 ok:true，实际 %v", ok)
	}
	bad, err := a.ValidateMethod(map[string]interface{}{"kind": "bogus"})
	if err != nil {
		t.Fatalf("非法定义应返回 ok:false 而非抛错（供 Lua 侧分支处理），实际 %v", err)
	}
	if bad.(map[string]interface{})["ok"] != false {
		t.Fatalf("非法定义应返回 ok:false（不抛错，供 Lua 侧分支处理），实际 %v", bad)
	}
	if _, ok := bad.(map[string]interface{})["error"].(string); !ok {
		t.Fatal("ok:false 时必须带上 error 文案")
	}
}

func TestCheckCallTargetsAcceptsCapabilityDirectCall(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	seedTool(t, reg)
	a := newAIAgent(reg, &scriptedLLM{}, stubRecipes{}, nil)

	f := model.Flow{ID: "f", Name: "n", Description: "d", Steps: []model.Step{{ID: "s1", Call: "db.exec"}}}
	if err := a.checkCallTargets(f, nil); err != nil {
		t.Fatalf("「能力.方法」直调应合法: %v", err)
	}
	f.Steps[0].Call = "db.nope"
	if err := a.checkCallTargets(f, nil); err == nil {
		t.Fatal("内置原语不存在的方法必须被拒")
	}
	f.Steps[0].Call = "裸名字"
	if err := a.checkCallTargets(f, nil); err == nil {
		t.Fatal("没有点的裸名字必须被拒")
	}
	a.extCaps = map[string]map[string]MethodSchema{"ext.d": {"run": {}}}
	f.Steps[0].Call = "ext.d.fly"
	if err := a.checkCallTargets(f, nil); err == nil {
		t.Fatal("扩展未声明的方法必须被拒（退化路径不能比主路径更松）")
	}
	f.Steps[0].Call = "ext.d.run"
	if err := a.checkCallTargets(f, nil); err != nil {
		t.Fatalf("扩展已声明的方法应放行: %v", err)
	}
}

// ---- 配方加载 ----

func TestParseRecipeDefaultsMaxRetries(t *testing.T) {
	r, err := parseRecipe([]byte("kind: flow\nsystem_decompose: x\n"))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if r.MaxRetries != 3 {
		t.Fatalf("max_retries 缺失应回落到 3，实际 %d", r.MaxRetries)
	}
	r2, _ := parseRecipe([]byte("max_retries: 7\n"))
	if r2.MaxRetries != 7 {
		t.Fatalf("显式值应保留，实际 %d", r2.MaxRetries)
	}
	if _, err := parseRecipe([]byte("\t bad: [")); err == nil {
		t.Fatal("非法 YAML 必须报错")
	}
}

func TestRecipeLoaderDirOverrideAndEmbeddedFallback(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "flow.yaml"), []byte("kind: flow\nmax_retries: 9\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	l := DefaultRecipeLoader(dir)
	r, err := l.Load("flow")
	if err != nil || r.MaxRetries != 9 {
		t.Fatalf("目录覆盖应优先: (%v, %v)", r, err)
	}
	if _, err := l.Load("tool"); err != nil {
		t.Fatalf("内嵌回退失败: %v", err)
	}
	if _, err := l.Load("不存在的kind"); err == nil {
		t.Fatal("不存在的 kind 必须报错（缺哪一份就等于该 kind 生成不了）")
	}
}

// ---- 小工具 ----

func TestParseReactAction(t *testing.T) {
	tool, method, args, ok := parseReactAction(actionJSON("db.exec", `{"sql":"SELECT 1"}`))
	if !ok || tool != "db" || method != "exec" || args["sql"] != "SELECT 1" {
		t.Fatalf("解析不符: %q/%q/%v/%v", tool, method, args, ok)
	}
	tool, method, _, ok = parseReactAction(actionJSON("ext.ai.complete", `{}`))
	if !ok || tool != "ext.ai" || method != "complete" {
		t.Fatalf("ext 命名空间应切成两级能力名 + 方法，实际 %q/%q", tool, method)
	}
	tool, method, _, ok = parseReactAction("Action:\n```json\n{\"tool\":\"http\",\"method\":\"Request\"}\n```")
	if !ok || tool != "http" || method != "Request" {
		t.Fatalf("tool 无点时应与 method 合并，实际 %q/%q", tool, method)
	}
	if _, _, _, ok := parseReactAction("没有围栏"); ok {
		t.Fatal("无 json 围栏应解析失败")
	}
	if _, _, _, ok := parseReactAction("```json\n{不是json}\n```"); ok {
		t.Fatal("非法 JSON 应解析失败")
	}
	if _, _, _, ok := parseReactAction("```json\n{\"method\":\"x\"}\n```"); ok {
		t.Fatal("缺 tool 字段应解析失败")
	}
	if _, _, args, ok := parseReactAction("```json\n{\"tool\":\"db.query\"}\n```"); !ok || args == nil {
		t.Fatal("缺 args 时应补成空 map（而不是 nil 让后续 panic）")
	}
}

func TestSplitToolMethod(t *testing.T) {
	if p, m := splitToolMethod("ext.ai.complete", ""); p != "ext.ai" || m != "complete" {
		t.Fatalf("ext 三级切分不符: %q/%q", p, m)
	}
	if p, m := splitToolMethod("ext.ai", "complete"); p != "ext.ai" || m != "complete" {
		t.Fatalf("ext 两级 + method 应原样: %q/%q", p, m)
	}
	if p, m := splitToolMethod("http.Request", ""); p != "http" || m != "Request" {
		t.Fatalf("内置切分不符: %q/%q", p, m)
	}
	if p, m := splitToolMethod("http", "Request"); p != "http" || m != "Request" {
		t.Fatalf("无点应保留 method: %q/%q", p, m)
	}
}

func TestReactTextHelpers(t *testing.T) {
	if fa, ok := extractFinalAnswer("Thought: x\nFinal Answer: 结果在这里"); !ok || strings.TrimSpace(fa) != "结果在这里" {
		t.Fatalf("Final Answer 提取不符: %q/%v", fa, ok)
	}
	if _, ok := extractFinalAnswer("没有结论"); ok {
		t.Fatal("无 Final Answer 时 ok 应为 false")
	}
	if got := trimThought("想了一下\nAction:\n```json\n{}```"); got != "想了一下" {
		t.Fatalf("trimThought 应取 Action 之前的部分，实际 %q", got)
	}
	if got := trimThought("  纯思考  "); got != "纯思考" {
		t.Fatalf("无 Action 时应取全文并去空白，实际 %q", got)
	}
	if !allowContains([]string{"http", "db"}, "db") {
		t.Fatal("allowContains 应命中显式名单")
	}
	if !allowContains([]string{"ext.*"}, "ext.anything") {
		t.Fatal("ext.* 通配必须放行所有扩展")
	}
	if allowContains([]string{"ext.*"}, "http") {
		t.Fatal("ext.* 不得放行内核原语")
	}
	if allowContains([]string{"http"}, "db") {
		t.Fatal("名单外的工具必须拒绝")
	}
	if got := stableJSON(map[string]interface{}{"b": 1, "a": 2}); got != `{"a":2,"b":1}` {
		t.Fatalf("stableJSON 应输出键有序的稳定 JSON，实际 %s", got)
	}
	if got := stableJSON(make(chan int)); got == "" {
		t.Fatal("无法序列化的值也要给出可读文本（不得返回空串）")
	}
	if got := truncateRunes(strings.Repeat("x", 10), 5); !strings.HasSuffix(got, "...(truncated)") {
		t.Fatalf("超长应截断并标注，实际 %q", got)
	}
	if got := truncateRunes("短", 5); got != "短" {
		t.Fatal("未超长不应改动")
	}

	// ── truncateObservation：截断必须「如实告知」，只截不说等于把假数字喂给模型 ──
	// 实测（2026-09-27）：模型拿一份带 ...(truncated) 的目录数出 12 条就回答「共 12 个 Tool」，
	// 真实 34。截断本身没错，错在截完之后模型无从知道「看到的不是全部」，于是凑一个合理的数。
	big := `[{"id":"a"},` + strings.Repeat(`{"id":"x"},`, 30) + `{"id":"z"}]` // 32 条
	obs := truncateObservation(big, 200)
	if !strings.Contains(obs, "共 32 条") {
		t.Fatalf("截断提示必须带清单的真实条数，否则模型只能去数半截清单:\n%s", obs)
	}
	if !strings.Contains(obs, "共 ") {
		t.Fatalf("截断提示必须带原文总长:\n%s", obs)
	}
	if strings.HasSuffix(obs, "...(truncated)") {
		t.Fatal("截断提示不得退回无信息的旧形态")
	}
	// 负向：非数组（-map 序列化结果）也要如实报总长，不能因为解析不出来就吞掉提示
	if got := truncateObservation("{\"k\":\"", 3); !strings.Contains(got, "原文共 ") {
		t.Fatalf("非数组的超长观察也要报总长，实际 %q", got)
	}
	// 未超长必须原样返回，不许在末尾追加提示
	if got := truncateObservation(`[{"id":"a"}]`, 500); got != `[{"id":"a"}]` {
		t.Fatalf("未超长不应改动，实际 %q", got)
	}
	// jsonArrayLen：读不出条数时必须如实报「不知道」，不许猜 0
	if n, ok := jsonArrayLen("not json at all"); ok || n != 0 {
		t.Fatalf("非 JSON 应返回 (0,false)，实际 %d,%v", n, ok)
	}
	if n, ok := jsonArrayLen(`[{"a":1},{"b":2}]`); !ok || n != 2 {
		t.Fatalf("应读出 2 条，实际 %d,%v", n, ok)
	}
	if got := appendUnique([]string{"a"}, "a"); len(got) != 1 {
		t.Fatal("重复元素不应追加")
	}
	if got := appendUnique([]string{"a"}, "b"); len(got) != 2 {
		t.Fatal("新元素应追加")
	}
}

func TestParseToolSummariesAndMisc(t *testing.T) {
	got := parseToolSummaries([]interface{}{
		map[string]interface{}{"id": "a", "description": "d",
			"input_schema":  map[string]interface{}{"type": "object"},
			"output_schema": map[string]interface{}{"type": "object"}},
		"not-a-map",
	})
	if len(got) != 1 || got[0].ID != "a" || got[0].InputSchema == nil || got[0].OutputSchema == nil {
		t.Fatalf("解析不符: %+v", got)
	}
	if len(parseToolSummaries("不是数组")) != 0 {
		t.Fatal("非数组输入应返回空切片")
	}
	if got := toStringSlice([]interface{}{"a", 1, "b"}); len(got) != 2 || got[0] != "a" {
		t.Fatalf("非字符串元素应跳过，实际 %v", got)
	}
	if got := toStringSlice([]string{"x"}); len(got) != 1 {
		t.Fatalf("[]string 应原样返回，实际 %v", got)
	}
	if got := toStringSlice(42); got != nil {
		t.Fatal("其它类型应返回 nil")
	}
	if strOr(map[string]interface{}{"a": "v"}, "a") != "v" || strOr(map[string]interface{}{"a": 1}, "a") != "" {
		t.Fatal("strOr 只取字符串值")
	}
}

func TestBuildActionEntryShapes(t *testing.T) {
	entry, err := buildActionEntry("db", "exec", map[string]interface{}{"sql": "SELECT 1"})
	if err != nil || !strings.Contains(entry, "ctx.db.exec(") || !strings.Contains(entry, `"SELECT 1"`) {
		t.Fatalf("内置原语应编成位置参数调用: (%s, %v)", entry, err)
	}
	entry, err = buildActionEntry("crypto", "hash", map[string]interface{}{"alg": "sha256"})
	if err != nil || !strings.Contains(entry, "ctx.crypto.hash({") {
		t.Fatalf("纯函数原子应编成单 table 调用: (%s, %v)", entry, err)
	}
	if _, err := buildActionEntry("crypto", "nope", nil); err == nil {
		t.Fatal("纯函数原子不存在的方法必须被拒（解析点拒绝未知值）")
	}
	if _, err := buildActionEntry("db", "nope", nil); err == nil {
		t.Fatal("内置原语不存在的方法必须被拒")
	}
	entry, err = buildActionEntry("ext.ai", "complete", map[string]interface{}{"prompt": "hi"})
	if err != nil || !strings.Contains(entry, "ctx.ext.ai.complete({") {
		t.Fatalf("ext 应编成单 table 调用: (%s, %v)", entry, err)
	}
}

// ───────── react 的默认工具权限：能力边界的落点 ─────────
//
// 背景（2026-09-27）：旧语义里「allow 没传 = 全部门禁原语」，于是一个只声明
// `capabilities: [ext.ai]` 的 agent Tool 实际能把 secret.get / db.exec 交给模型——
// 能力边界写在 Tool 声明上，却不在执行通道上。下面几条把这个边界钉死。

// reactToolSection 取系统提示里「## 可用工具」那一小节，用来断言「模型被声明有哪些工具」。
// 只取这一小节：提示的「规则」段里有 `http.Request / db.query / secret.get` 这类**静态样例**，
// 拿整篇提示做子串判断会把样例当成工具清单（第一次写出来就是这样误报的）。
func reactToolSection(t *testing.T, llm *scriptedLLM) string {
	t.Helper()
	if len(llm.got) == 0 {
		t.Fatal("模型没被询问过")
	}
	var prompt string
	for _, m := range llm.got[0].Messages {
		if role, _ := m["role"].(string); role == "system" {
			prompt, _ = m["content"].(string)
		}
	}
	const marker = "## 可用工具"
	i := strings.Index(prompt, marker)
	if i < 0 {
		t.Fatalf("系统提示里找不到「%s」小节：\n%s", marker, prompt)
	}
	rest := prompt[i+len(marker):]
	if j := strings.Index(rest, "\n## "); j >= 0 {
		rest = rest[:j]
	}
	return rest
}

// ① 负向核心：没传 allow 时，工具集 = 调用方自己声明的那份，不是全目录。
func TestReactMethodDefaultsToCallerCapabilitiesNotWholeCatalog(t *testing.T) {
	llm := &scriptedLLM{texts: []string{
		actionJSON("secret.get", `{"name":"db.password"}`),
		"Final Answer: 拿到密码了",
	}}
	a, entries := newReactAgent(t, llm)

	// 调用方只声明了 ext.ai：agent 不该看得见 secret / db / http。
	_, err := a.ReactMethod(map[string]interface{}{"task": "去把密码取出来"}, "ext.ai")
	if err != nil {
		t.Fatalf("ReactMethod 失败: %v", err)
	}
	if len(*entries) != 0 {
		t.Fatalf("调用方未声明的能力不得被执行，实际 entry=%v", *entries)
	}
	section := reactToolSection(t, llm)
	for _, want := range []string{"secret", "db.", "http."} {
		if strings.Contains(section, want) {
			t.Errorf("调用方只声明了 [ext.ai] 却把 %q 也带进了工具清单：\n%s", want, section)
		}
	}
}

// ② 调用方一份都没声明时，**一个工具都不给**——而不是退回全目录。
func TestReactMethodWithoutAnyCapabilityGrantsNoTool(t *testing.T) {
	llm := &scriptedLLM{texts: []string{
		actionJSON("db.exec", `{"sql":"SELECT 1"}`),
		"Final Answer: 放弃",
	}}
	a, entries := newReactAgent(t, llm)

	_, err := a.ReactMethod(map[string]interface{}{"task": "查一下"})
	if err != nil {
		t.Fatalf("ReactMethod 失败: %v", err)
	}
	if len(*entries) != 0 {
		t.Fatalf("未声明任何 capability 的 Tool 不该能调 db.exec，实际 entry=%v", *entries)
	}
	if section := reactToolSection(t, llm); strings.Contains(section, "db") {
		t.Errorf("未声明任何 capability 时工具清单应为空，实际：\n%s", section)
	}
}

// ③ 显式 allow 可以**放宽到**调用方声明的范围（在范围内就该放行），但不能超出。
func TestReactMethodRejectsAllowBeyondCallerCapabilities(t *testing.T) {
	llm := &scriptedLLM{texts: []string{"Final Answer: 无"}}
	a, _ := newReactAgent(t, llm)

	_, err := a.ReactMethod(map[string]interface{}{
		"task":  "查一下",
		"allow": []interface{}{"http", "secret"},
	}, "http")
	if err == nil {
		t.Fatal("allow 超出自己的 capability 必须报错，否则就是绕过门禁的后门")
	}
	if !strings.Contains(err.Error(), "只声明了") {
		t.Errorf("报错要说清「超出自己的 capability」，实际 %v", err)
	}
}

// ④ 范围内的 allow 照样能跑通：别把收紧做成「一刀切不给」。
func TestReactMethodAllowsWithinCallerCapabilities(t *testing.T) {
	llm := &scriptedLLM{texts: []string{
		actionJSON("db.exec", `{"sql":"SELECT 1"}`),
		"Final Answer: 拿到",
	}}
	a, entries := newReactAgent(t, llm)

	res, err := a.ReactMethod(map[string]interface{}{
		"task":  "查一下",
		"allow": []interface{}{"db"},
	}, "db")
	if err != nil {
		t.Fatalf("范围内的 allow 应放行: %v", err)
	}
	if len(*entries) != 1 || !strings.Contains((*entries)[0], "ctx.db.exec") {
		t.Fatalf("范围内的工具应经宿主通道执行，实际 entry=%v", *entries)
	}
	if used, _ := res.(map[string]interface{})["used_tools"].([]string); len(used) != 1 {
		t.Fatalf("used_tools 应记录 db.exec，实际 %v", used)
	}
}

// react 的数值入参（max_steps / temperature）经 Lua 传进来是 **int64** 而不是 float64
// （见 lua.go 的 fromLuaValue 整数收敛）。只认 float64 的话这两个参数会静默失效：
// 范例里写 `max_steps = 8` 实际按默认 6 步跑，肉眼看不出来。
func TestReactMethodReadsIntegerNumbersFromLua(t *testing.T) {
	for _, v := range []interface{}{int64(8), float64(8), 8} {
		llm := &scriptedLLM{texts: []string{"Final Answer: 无"}}
		a, _ := newReactAgent(t, llm)

		_, err := a.ReactMethod(map[string]interface{}{"task": "查", "max_steps": v, "temperature": v}, "ext.ai")
		if err != nil {
			t.Fatalf("入参 %#v 应被接受: %v", v, err)
		}
		// 给了 8 就该是 8（不是「被丢掉后落到默认的 0.3」）。
		if len(llm.got) == 0 || llm.got[0].Temperature != 8 {
			t.Fatalf("入参 %#v 未被解析，got=%+v", v, llm.got)
		}
	}

	// 真的把步数带进去：脚本第 3 轮才给答案，2 步看不完，9 步够。
	scripts := []struct {
		maxSteps interface{}
		wantDone bool
	}{
		{int64(2), false},
		{int64(9), true},
	}
	for _, c := range scripts {
		llm := &scriptedLLM{texts: []string{
			"Thought: 一步", actionJSON("db.exec", `{"sql":"SELECT 1"}`), "Final Answer: 完成",
		}}
		a, _ := newReactAgent(t, llm)
		res, err := a.ReactMethod(map[string]interface{}{"task": "查", "max_steps": c.maxSteps}, "db")
		if err != nil {
			t.Fatalf("max_steps=%v: %v", c.maxSteps, err)
		}
		answer, _ := res.(map[string]interface{})["answer"].(string)
		if got := strings.Contains(answer, "完成"); got != c.wantDone {
			t.Errorf("max_steps=%v 时 answer 应含「完成」=%v，实际 %q", c.maxSteps, c.wantDone, answer)
		}
	}
}

// ---- React 元工具模式（Mode=meta）----
//
// 工具面恒定为 4 个元工具（find/describe/invoke/registry），与能力目录规模（实测 28 能力 /
// 115 方法）彻底解耦。这是把「115 个方法平铺进工具面导致工具越多越降智」这个问题的治本办法：
// 模型先检索、再按需取 schema、最后直调，单轮请求的工具定义从 10-15k token 降到几百。

func TestReactMetaToolSpecsAreConstantFour(t *testing.T) {
	a := newCatalogAgent(t)
	// 用不同的 allow 都能得到同一份 4 元工具面：证明规模无关（这是 meta 模式存在的意义）。
	for _, al := range []reactAllow{{names: []string{"db"}}, {all: true}} {
		specs, byName := a.reactToolSpecs(al, true)
		if len(specs) != 4 {
			t.Fatalf("元工具面应恒为 4 个，实际 %d（allow=%+v）", len(specs), al)
		}
		want := map[string]string{
			"ext_ai_find":        "ext.ai.find",
			"ext_ai_describe":    "ext.ai.describe",
			"ext_ai_invoke":      "ext.ai.invoke",
			"ext_registry_tools": "ext.registry.tools",
		}
		for name, orig := range want {
			if byName[name] != orig {
				t.Errorf("元工具 %q 反查应为 %q，实际 %q", name, orig, byName[name])
			}
		}
	}
}

func TestReactMetaBypassesFlatToolBudget(t *testing.T) {
	// flat 模式 Allow=* 必撞 maxReactTools（115 个方法）；meta 模式工具面恒 4，不受此约束。
	a, _ := newReactAgent(t, &scriptedLLM{texts: []string{"Final Answer: 直接答"}})
	if _, err := a.React(context.Background(), "任务",
		ReactOpts{MaxSteps: 1, Allow: []string{ReactAllowAll}, AllowSet: true, Mode: ReactModeMeta}); err != nil {
		t.Fatalf("元工具模式不应受工具数上限约束，实际报错: %v", err)
	}
}

func TestReactMetaRetrievalStepsAreFree(t *testing.T) {
	// 核心不变量：find/describe/registry 是「检索步」、不计入 MaxSteps 预算；只有 invoke 才算。
	// 这里 MaxSteps=1，模型仍可在 invoke 之前自由地 find 一次——证明检索步没吃掉执行预算。
	a, entries := newReactAgent(t, &scriptedLLM{
		texts: []string{
			"Thought: 先用 find 检索数据库方法",
			"Thought: 用 db.query 查 users 表",
			"Final Answer: 已查到 users 表数据。",
		},
		calls: [][]ToolCall{
			{{ID: "c1", Name: "ext_ai_find", Arguments: `{"query":"查询数据库"}`}},
			{{ID: "c2", Name: "ext_ai_invoke", Arguments: `{"capability":"db","method":"query","args":{"table":"users"}}`}},
			nil,
		},
	})
	out, err := a.React(context.Background(), "查 users 表",
		ReactOpts{MaxSteps: 1, Allow: []string{"db"}, AllowSet: true, Mode: ReactModeMeta})
	if err != nil {
		t.Fatalf("React 失败: %v", err)
	}
	m := out.(map[string]interface{})
	if m["answer"] != "已查到 users 表数据。" {
		t.Errorf("answer 应为最终答案，实际 %v", m["answer"])
	}
	if len(*entries) < 2 {
		t.Fatalf("find 与 invoke 都应真正执行（分别 1 次），实际 entries=%v", *entries)
	}
}

func TestReactMetaInvokeRespectsBudget(t *testing.T) {
	// MaxSteps=1 时只允许 1 次 invoke；第二次 invoke 应被预算拦下（不真正执行、回灌提示）。
	a, entries := newReactAgent(t, &scriptedLLM{
		texts: []string{
			"Thought: 调一次 db.query",
			"Thought: 想再调一次",
			"Final Answer: 完成。",
		},
		calls: [][]ToolCall{
			{{ID: "c1", Name: "ext_ai_invoke", Arguments: `{"capability":"db","method":"query","args":{"table":"a"}}`}},
			{{ID: "c2", Name: "ext_ai_invoke", Arguments: `{"capability":"db","method":"query","args":{"table":"b"}}`}},
			nil,
		},
	})
	if _, err := a.React(context.Background(), "任务",
		ReactOpts{MaxSteps: 1, Allow: []string{"db"}, AllowSet: true, Mode: ReactModeMeta}); err != nil {
		t.Fatalf("React 失败: %v", err)
	}
	if len(*entries) != 1 {
		t.Errorf("MaxSteps=1 只应执行 1 次 invoke，实际 %d 次: %v", len(*entries), *entries)
	}
}

func TestReactMetaInvokeHonorsAllow(t *testing.T) {
	// invoke 解析出的能力必须在 allow 内：即便工具面只有 4 个元工具，越权调用仍要被拦。
	a, entries := newReactAgent(t, &scriptedLLM{
		texts: []string{
			"Thought: 想调 secret.get",
			"Final Answer: 没调成。",
		},
		calls: [][]ToolCall{
			{{ID: "c1", Name: "ext_ai_invoke", Arguments: `{"capability":"secret","method":"get","args":{"name":"x"}}`}},
			nil,
		},
	})
	_, err := a.React(context.Background(), "任务",
		ReactOpts{MaxSteps: 2, Allow: []string{"db"}, AllowSet: true, Mode: ReactModeMeta})
	if err != nil {
		t.Fatalf("React 失败: %v", err)
	}
	joined := strings.Join(*entries, "\n")
	if strings.Contains(joined, "secret.get") {
		t.Errorf("invoke 越权调用 secret 应被 allow 拦下，实际 entries=%v", *entries)
	}
}

func TestReactMetaSystemPromptDescribesWorkflow(t *testing.T) {
	p := (&AIAgent{}).reactMetaSystemPrompt(4)
	for _, need := range []string{"ext_ai_find", "ext_ai_describe", "ext_ai_invoke", "ext_registry_tools"} {
		if !strings.Contains(p, need) {
			t.Errorf("元工具系统提示须列出 %q", need)
		}
	}
	if !strings.Contains(p, "find") || !strings.Contains(p, "describe") || !strings.Contains(p, "invoke") {
		t.Error("元工具系统提示须说明 find → describe → invoke 的工作流")
	}
}
