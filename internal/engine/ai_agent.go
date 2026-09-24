package engine

// ai_agent.go 是「阶段2/3 合并」的核心：把原 ai-sidecar 的四个稳定原语 + 一个真正的
// agent 编排引擎（generate）收进进程内扩展 ext.ai，并用 github.com/cloudwego/eino 的
// compose 做「单趟编排」（decompose → catalog → assemble），重试环由 Go 持有（迭代逻辑
// 放 Go 比塞进 DAG 更稳，且与 PRD §9.3 单一校验源一致）。
//
// 分层（PRD §9 + 用户决策 2026-09-15）：
//   - 稳定层（Go，本文件）：complete / catalog / domains / validate / generate 方法
//   - 能力语义层（extensions.go）：ext.registry.capabilities() 返回 ctx.* 参数 schema
//   - 易变层（可编辑 recipe 制品，recipes/*.yaml）：ai.gen-flow / ai.gen-tool / ai.gen-hook
//     的提示词与输出约束，用户改 YAML 即可调生成细节，无需重编 gear，agent 不从头写。
//
// ext.ai 进程内承载（用户决策：进程内，不走 sidecar 子进程）。

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/cloudwego/eino/compose"
	"github.com/yuin/gopher-lua"
	"gopkg.in/yaml.v3"

	"gear/internal/cap"
	"gear/internal/model"
	"gear/internal/registry"
)

// ---- 可编辑生成配方（ai.gen-* 制品） ----

// Recipe 是一条生成配方（对应 ai.gen-flow/tool/hook 的可编辑制品）：系统提示 + 输出约束 +
// 能力提示，决定某一类制品怎么生成。改 YAML 即可调生成细节。
type Recipe struct {
	Kind             string `yaml:"kind"`
	MaxRetries       int    `yaml:"max_retries"`
	SystemDecompose  string `yaml:"system_decompose"`
	SystemAssemble   string `yaml:"system_assemble"`
	OutputHints      string `yaml:"output_hints"`
	CapabilitiesHint string `yaml:"capabilities_hint"`
}

// RecipeLoader 加载某 kind 的配方。embeddedRecipeLoader 先试**可选**覆盖目录（examples/ai/recipes，
// 默认不存在；存在则优先），再回退嵌入默认；用户改覆盖目录里的 YAML 即生效，无需重编。
type RecipeLoader interface {
	Load(kind string) (*Recipe, error)
}

// embeddedRecipes 是随二进制分发的默认配方（flow/tool/hook 三份必须齐全：Load 按 kind 直接
// ReadFile，缺任何一份都等于「该 kind 生成不了」）。覆盖目录 examples/ai/recipes（或
// GEAR_AI_RECIPES 指向的目录）**默认不存在、按需创建即可**，存在时优先于内嵌。
// 改默认措辞/约束只需改 `internal/engine/recipes/*.yaml`（内嵌即权威），不必再造覆盖副本。
//
//go:embed recipes/*.yaml
var embeddedRecipes embed.FS

// embeddedRecipeLoader 默认配方加载器：目录覆盖优先，否则用嵌入默认。
type embeddedRecipeLoader struct {
	dir string
}

// DefaultRecipeLoader 返回默认加载器；dir 非空时优先从该目录加载同名 YAML 覆盖。
func DefaultRecipeLoader(dir string) RecipeLoader {
	return embeddedRecipeLoader{dir: dir}
}

func (l embeddedRecipeLoader) Load(kind string) (*Recipe, error) {
	if l.dir != "" {
		p := filepath.Join(l.dir, kind+".yaml")
		if b, err := os.ReadFile(p); err == nil {
			return parseRecipe(b)
		}
	}
	b, err := embeddedRecipes.ReadFile("recipes/" + kind + ".yaml")
	if err != nil {
		return nil, fmt.Errorf("读取内嵌配方 recipes/%s.yaml 失败: %w", kind, err)
	}
	return parseRecipe(b)
}

func parseRecipe(b []byte) (*Recipe, error) {
	var r Recipe
	if err := yaml.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("解析配方失败: %w", err)
	}
	if r.MaxRetries <= 0 {
		r.MaxRetries = 3
	}
	return &r, nil
}

// ---- AIAgent：进程内 ext.ai 实现 ----

// AIAgent 是 ext.ai 的进程内实现（取代 sidecar 子进程）。持有：
//   - reg：工具目录来源（catalog 读真实工具）+ validate 重建工具用
//   - llm：模型层（生产 HTTPLLM / 测试 MockLLM）
//   - recipes：可编辑配方加载器（ai.gen-* 制品）
//   - extCaps：已注册 ext.* 的方法级参数 schema（供 tool/hook 生成锚定 ctx.ext.* 参数）
//   - exec：react 循环的单步工具执行器（由 Engine 经 WithAIAgent 注入；nil 时 React 显式报错）
type AIAgent struct {
	reg     *registry.Registry
	llm     LLM
	recipes RecipeLoader
	extCaps map[string]map[string]MethodSchema
	// exec 是 react 调用单步工具的原语：构建「声明全部 capability」的宿主视图并跑一段 Lua 入口
	// （与真实 Tool 同一通道），使 react 能使用内核原语（builtin + pure）+ 已注册 ext.*。由 Engine 注入。
	exec func(ctx context.Context, entry string, input map[string]interface{}) (map[string]interface{}, error)
	// validateFlow 是 Flow 静态校验的注入点：Engine 经 bindAIAgent 指向 e.ValidateFlowStatic，
	// 使 generate 的「生成 → 校验 → 修正」与 CLI/REST/MCP 的注册校验共用同一份规则，
	// 不产生第二套判定（PRD §9.2 单一真相源）。nil 时 Validate 退化为只做结构校验并显式说明。
	validateFlow func(*model.Flow) error
}

// newAIAgent 构造进程内 agent。
func newAIAgent(reg *registry.Registry, llm LLM, recipes RecipeLoader, extCaps map[string]map[string]MethodSchema) *AIAgent {
	if recipes == nil {
		recipes = DefaultRecipeLoader("")
	}
	return &AIAgent{reg: reg, llm: llm, recipes: recipes, extCaps: extCaps}
}

// ---- 稳定层方法（供 Lua ctx.ext.ai.* 与 Go 直接调用） ----

// Complete 走 LLM 层（参数化 model/url/key/provider），format=json 时解析为 {data}，否则 {text}。
func (a *AIAgent) Complete(args map[string]interface{}) (interface{}, error) {
	var req LLMRequest
	if b, err := json.Marshal(args); err == nil {
		_ = json.Unmarshal(b, &req)
	}
	text, err := a.llm.Complete(context.Background(), req)
	if err != nil {
		return nil, err
	}
	if req.Format == "json" {
		data, err := extractJSONToAny(text)
		if err != nil {
			return nil, fmt.Errorf("LLM 输出非合法 JSON: %w", err)
		}
		return map[string]interface{}{"data": data}, nil
	}
	return map[string]interface{}{"text": text}, nil
}

// CompleteStream 是 Complete 的流式版：把 LLM 增量逐条经 onDelta 回调出去（content 为主文本，
// reasoning 为推理增量，可能为空）。供 REST SSE 端点与 Lua 回调式 API 复用；旧的 Complete
// 同步方法保持不变，所有现存同步调用方零破坏。
func (a *AIAgent) CompleteStream(args map[string]interface{}, onDelta func(content, reasoning string) error) error {
	var req LLMRequest
	if b, err := json.Marshal(args); err == nil {
		_ = json.Unmarshal(b, &req)
	}
	return a.llm.CompleteStream(context.Background(), req, onDelta)
}

// Catalog 在指定能力域内（或全域）按「查询词 + 域」做 BM25 检索，返回 Top-K 候选工具。
func (a *AIAgent) Catalog(args map[string]interface{}) (interface{}, error) {
	tools, err := a.loadTools()
	if err != nil {
		return nil, err
	}
	query, _ := args["query"].(string)
	domain, _ := args["domain"].(string)
	k := 5
	if v, ok := args["k"].(float64); ok && int(v) > 0 {
		k = int(v)
	}
	return map[string]interface{}{"tools": Retrieve(tools, domain, query, k)}, nil
}

// Domains 按 namespace 归类出能力域概览。
func (a *AIAgent) Domains(args map[string]interface{}) (interface{}, error) {
	tools, err := a.loadTools()
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{"domains": DomainOverview(tools)}, nil
}

// Validate 暴露给 Lua：静态校验 Flow/Tool/Hook，返回 {ok} 或 {ok:false,error}。
func (a *AIAgent) ValidateMethod(args map[string]interface{}) (interface{}, error) {
	kind, _ := args["kind"].(string)
	def, _ := args["def"].(map[string]interface{})
	tools := parseToolSummaries(args["tools"])
	if err := a.Validate(kind, def, tools); err != nil {
		return map[string]interface{}{"ok": false, "error": err.Error()}, nil
	}
	return map[string]interface{}{"ok": true}, nil
}

// Capabilities 暴露给 Lua：返回内核原语（builtin + pure）+ 已注册 ext.* 的方法级参数 schema（锚定 ctx.* 语义）。
func (a *AIAgent) CapabilitiesMethod() (interface{}, error) {
	return a.capabilitySummaries(), nil
}

// Generate 是真正的 agent 编排：生成 flow/tool/hook 草稿（含「生成 → 校验 → 修正」闭环）。
// 用 Eino compose 编排单趟（decompose → catalog → assemble），重试环在 Go 侧。
func (a *AIAgent) Generate(ctx context.Context, kind, intent string, opts GenerateOpts) (*GenResult, error) {
	// 模型参数覆盖（opts.Provider/Model/BaseURL/APIKey）在**这里**生效，且必须在
	// 下面组合 Eino 链之前完成：`compose.InvokableLambda(a.decompose)` 这类方法值在
	// **取值的那一刻**就绑定了 receiver，晚于此替换 llm 对 decompose/catalog/assemble 无效。
	//
	// 这一行是 GenerateOpts 注释所承诺的「CLI `gear ai gen` 把 GEAR_AI_* 注入」的落点：
	// 缺了它，CLI 填进 opts 的模型参数会被静默丢弃（不报错、也不生效）。
	// opts 未给模型参数时返回原 agent，无额外开销。
	a = a.withLLMOverrides(opts)

	if kind != "flow" && kind != "tool" && kind != "hook" {
		return nil, fmt.Errorf("generate 的 kind 必须是 flow|tool|hook，收到 %q", kind)
	}
	recipe, err := a.recipes.Load(kind)
	if err != nil {
		return nil, fmt.Errorf("加载 %s 配方失败: %w", kind, err)
	}
	tools, err := a.loadTools()
	if err != nil {
		return nil, err
	}
	maxRetries := recipe.MaxRetries
	if opts.MaxRetries > 0 {
		maxRetries = opts.MaxRetries
	}

	// 建 Eino 链：decompose → catalog → assemble（单趟）。重试环放 Go。
	chain := compose.NewChain[*genState, *genState]()
	chain = chain.AppendLambda(compose.InvokableLambda(a.decompose))
	chain = chain.AppendLambda(compose.InvokableLambda(a.catalog))
	chain = chain.AppendLambda(compose.InvokableLambda(a.assemble))
	runnable, err := chain.Compile(ctx)
	if err != nil {
		return nil, fmt.Errorf("编译生成链失败: %w", err)
	}

	st := &genState{Kind: kind, Intent: intent, Tools: tools, Recipe: recipe}
	lastErr := ""
	for attempt := 0; attempt < maxRetries; attempt++ {
		st.LastErr = lastErr
		out, err := runnable.Invoke(ctx, st)
		if err != nil {
			return nil, fmt.Errorf("生成第 %d 趟失败: %w", attempt+1, err)
		}
		def, perr := extractJSONToAny(out.Candidate)
		if perr != nil {
			lastErr = "LLM 未返回合法 JSON：" + perr.Error()
			continue
		}
		defMap, ok := def.(map[string]interface{})
		if !ok {
			lastErr = "LLM 返回的不是对象"
			continue
		}
		a.normalizeDef(kind, defMap) // 补 LLM 漏写的默认字段
		if verr := a.Validate(kind, defMap, tools); verr != nil {
			lastErr = verr.Error()
			continue
		}
		res := &GenResult{Kind: kind, Def: defMap}
		if opts.RegisterInto != nil {
			if rerr := a.registerDef(opts.RegisterInto, kind, defMap); rerr != nil {
				return nil, fmt.Errorf("注册 %s 失败: %w", kind, rerr)
			}
		}
		return res, nil
	}
	return nil, fmt.Errorf("生成 %s 失败（重试 %d 次）：%s", kind, maxRetries, lastErr)
}

// GenerateMethod 暴露给 Lua ctx.ext.ai.generate：解析参数后调 Generate（不注册，返回草稿）。
func (a *AIAgent) GenerateMethod(args map[string]interface{}) (interface{}, error) {
	kind, _ := args["kind"].(string)
	intent, _ := args["intent"].(string)
	opts := GenerateOpts{}
	if v, ok := args["max_retries"].(float64); ok && int(v) > 0 {
		opts.MaxRetries = int(v)
	}
	res, err := a.Generate(context.Background(), kind, intent, opts)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{"kind": res.Kind, "def": res.Def}, nil
}

// ---- React：进程内 ReAct 智能体（ext.ai.react） ----

// ReactOpts 携带 react 的可选项。
type ReactOpts struct {
	MaxSteps    int      // 最大步数（默认 6）
	Allow       []string // 限制可用工具（如 ["http","secret"]）；空 = 全部内置原语 + 已注册 ext.*
	Provider    string   // 模型厂商（可选，覆盖 env）
	Model       string   // 模型名（可选）
	BaseURL     string   // API base（可选）
	APIKey      string   // API key（可选，优先 env）
	Temperature float64  // 采样温度（可选，默认 0.3）
}

// React 跑一个 ReAct 循环：把任务交给 LLM，让它逐步「思考 + 调用内置工具」，Go 侧把工具调用
// 编成一段 Lua 入口、经与真实 Tool 完全相同的宿主能力通道（host.* / ctx.ext.*）执行，观察结果再
// 喂回 LLM，直到 LLM 给出 Final Answer 或达到最大步数。
//
// react「可以使用内置所有工具」即此意：每步 LLM 选中的工具（内核原语 http/call/db/secret/event/subscribe
// 与纯函数原子 time/template/crypto，或已注册 ext.*）都经 exec 执行——复用引擎既有的最小权限 + Lua 沙箱，
// react 不会越出宿主能力边界。
//
// 用户可经 ctx.ext.ai.react 直接调用（"用户爱用就用，不爱用就自己写"）：写一个 Tool 的 entry 调
// ctx.ext.ai.react({ task = "..." }) 即可；也可在生成阶段让 agent 把 react 当成一个现成能力锚定。
func (a *AIAgent) React(ctx context.Context, task string, opts ReactOpts) (interface{}, error) {
	if a.exec == nil {
		return nil, fmt.Errorf("ext.ai.react 未装配执行器（需经 engine.WithAIAgent 注入且引擎已配置宿主能力）")
	}
	if task == "" {
		return nil, fmt.Errorf("react 需要 task 参数")
	}
	if opts.MaxSteps <= 0 {
		opts.MaxSteps = 6
	}
	if opts.Temperature == 0 {
		opts.Temperature = 0.3
	}

	sys := a.reactSystemPrompt(opts.Allow, opts.MaxSteps)
	conv := "任务：" + task + "\n\n请开始（先写 Thought，再输出 Action 或 Final Answer）。"
	steps := []map[string]interface{}{}
	usedTools := []string{}
	seen := map[string]bool{}
	finalAnswer := ""

	for i := 0; i < opts.MaxSteps; i++ {
		resp, err := a.llm.Complete(ctx, LLMRequest{
			System:      sys,
			Prompt:      conv,
			Provider:    opts.Provider,
			Model:       opts.Model,
			BaseURL:     opts.BaseURL,
			APIKey:      opts.APIKey,
			Temperature: opts.Temperature,
		})
		if err != nil {
			return nil, fmt.Errorf("react 第 %d 步 LLM 调用失败: %w", i+1, err)
		}

		// Final Answer 优先于 Action。
		if fa, ok := extractFinalAnswer(resp); ok {
			finalAnswer = strings.TrimSpace(fa)
			steps = append(steps, map[string]interface{}{"thought": trimThought(resp), "action": nil, "observation": finalAnswer})
			break
		}

		tool, method, args, ok := parseReactAction(resp)
		if !ok {
			obs := "你本轮既未输出合法 Action（```json {...} ```）也未输出 Final Answer，请严格按格式继续。"
			conv += "\n\n" + resp + "\n\nObservation: " + obs
			steps = append(steps, map[string]interface{}{"thought": trimThought(resp), "action": nil, "observation": obs})
			continue
		}
		// 工具可用性守卫（allow 限制）。
		if len(opts.Allow) > 0 && !allowContains(opts.Allow, tool) {
			obs := fmt.Sprintf("工具 %q 不在允许列表 %v 中，请改用允许的工具", tool, opts.Allow)
			conv += "\n\n" + resp + "\n\nObservation: " + obs
			steps = append(steps, map[string]interface{}{"thought": trimThought(resp), "action": tool + "." + method, "observation": obs})
			continue
		}
		// 重复调用守卫（防止模型陷入同一工具+参数的死循环）。
		key := tool + "." + method + " " + stableJSON(args)
		if seen[key] {
			obs := "检测到重复调用相同工具与参数，疑似陷入循环，已停止该步"
			conv += "\n\n" + resp + "\n\nObservation: " + obs
			steps = append(steps, map[string]interface{}{"thought": trimThought(resp), "action": tool + "." + method, "observation": obs})
			continue
		}
		seen[key] = true

		entry, err := buildActionEntry(tool, method, args)
		if err != nil {
			obs := "构建工具调用失败：" + err.Error()
			conv += "\n\n" + resp + "\n\nObservation: " + obs
			steps = append(steps, map[string]interface{}{"thought": trimThought(resp), "action": tool + "." + method, "observation": obs})
			continue
		}
		out, err := a.exec(ctx, entry, map[string]interface{}{})
		var obs string
		if err != nil {
			obs = "工具执行错误：" + err.Error()
		} else if out != nil {
			if r, ok := out["result"]; ok {
				obs = stableJSON(r)
			} else {
				obs = stableJSON(out)
			}
		} else {
			obs = "(无输出)"
		}
		obs = truncateRunes(obs, 3000)
		conv += "\n\n" + resp + "\n\nObservation: " + obs
		steps = append(steps, map[string]interface{}{"thought": trimThought(resp), "action": tool + "." + method, "observation": obs})
		usedTools = appendUnique(usedTools, tool+"."+method)
	}

	if finalAnswer == "" {
		finalAnswer = fmt.Sprintf("（达到最大步数 %d，未给出 Final Answer）", opts.MaxSteps)
	}
	return map[string]interface{}{
		"answer":     finalAnswer,
		"steps":      steps,
		"used_tools": usedTools,
	}, nil
}

// ReactMethod 暴露给 Lua ctx.ext.ai.react：解析参数后跑 React 循环。
func (a *AIAgent) ReactMethod(args map[string]interface{}) (interface{}, error) {
	task, _ := args["task"].(string)
	opts := ReactOpts{}
	if v, ok := args["max_steps"].(float64); ok && int(v) > 0 {
		opts.MaxSteps = int(v)
	}
	opts.Allow = toStringSlice(args["allow"])
	if s, ok := args["provider"].(string); ok && s != "" {
		opts.Provider = s
	}
	if s, ok := args["model"].(string); ok && s != "" {
		opts.Model = s
	}
	if s, ok := args["base_url"].(string); ok && s != "" {
		opts.BaseURL = s
	}
	if s, ok := args["api_key"].(string); ok && s != "" {
		opts.APIKey = s
	}
	if v, ok := args["temperature"].(float64); ok && v > 0 {
		opts.Temperature = v
	}
	return a.React(context.Background(), task, opts)
}

// reactSystemPrompt 拼装 ReAct 的系统提示：可用工具（来自能力目录，可经 allow 过滤）+ 输出格式 + 规则。
func (a *AIAgent) reactSystemPrompt(allow []string, maxSteps int) string {
	tools := a.reactToolsText(allow)
	var b strings.Builder
	b.WriteString("你是一个运行在 Gear 引擎内的 ReAct（推理 + 行动）智能体。你可以通过调用「内置工具」逐步推理与行动，直到能回答用户任务。\n\n")
	b.WriteString("## 可用工具（只能使用下列工具，不要编造）\n")
	b.WriteString(tools)
	b.WriteString("\n\n## 每轮输出格式（严格遵守）\n")
	b.WriteString("先写思考，再决定「调用一个工具」或「给出最终答案」：\n\n")
	b.WriteString("Thought: <这一步你在想什么>\n")
	b.WriteString("Action:\n```json\n{\"tool\": \"http.Request\", \"args\": {\"method\": \"GET\", \"url\": \"https://example.com\", \"body\": null, \"headers\": {}}}\n```\n\n")
	b.WriteString("当你认为信息已足够、可以回答用户时，输出：\n\n")
	b.WriteString("Thought: <总结>\n")
	b.WriteString("Final Answer: <给用户的完整回答>\n\n")
	b.WriteString("## 规则\n")
	b.WriteString("- 每轮只能调用一个工具。Action 必须是合法 JSON（放在 ```json ... ``` 围栏内）。\n")
	b.WriteString("- tool 字段为「<能力名>.<方法>」：内置原语如 http.Request / db.query / secret.get；扩展如 ext.ai.complete。\n")
	b.WriteString("- 内置原语按「位置参数」调用，args 的键对应上面列出的参数名（可选参数缺省传 null）。\n")
	b.WriteString("- 扩展（ext.*）按「单个具名 table」调用，args 整体作为 { ... } 传入 ctx.ext.<name>.<method>({...})。\n")
	b.WriteString("- Observation（工具返回）会由系统填回作为下一步输入；不要自己编造 Observation。\n")
	b.WriteString(fmt.Sprintf("- 最多 %d 步。若卡住，请尽快给出 Final Answer。\n", maxSteps))
	return b.String()
}

// reactToolsText 把可用工具压成给 LLM 看的文本（与 capabilitiesText 同源，但支持 allow 过滤）。
func (a *AIAgent) reactToolsText(allow []string) string {
	allowed := map[string]bool{}
	for _, x := range allow {
		allowed[x] = true
	}
	filter := func(name string) bool {
		return len(allowed) == 0 || allowed[name]
	}
	var sb strings.Builder
	for _, p := range []string{"http", "call", "db", "secret", "event"} {
		if !filter(p) {
			continue
		}
		if ms, ok := builtinCapabilitySchemas[p]; ok {
			for m, sch := range ms {
				sb.WriteString(fmt.Sprintf("- ctx.%s.%s(%s): %s\n", p, m, paramList(sch.Parameters), sch.Description))
			}
		}
	}
	if len(a.extCaps) > 0 {
		sb.WriteString("# ctx.ext.<name>.<method> 只接受「单个具名 table」参数：\n")
		for name, ms := range a.extCaps {
			if !filter(name) {
				continue
			}
			for m, sch := range ms {
				sb.WriteString(fmt.Sprintf("- ctx.%s.%s({ %s }): %s\n", name, m, fieldsList(sch.Parameters), sch.Description))
			}
		}
	}
	return sb.String()
}

// bareFunctionPrimitives 是「在 Lua 侧绑定成**裸函数**、而非带方法的表」的原语——目前只有 call
// （lua.go：ctx.call(target, input)，插件间唯一通信通道；其余原语都是 ctx.<name>.<method> 形态）。
//
// 能力目录仍以方法名 "Call" 描述它（与 MethodSchema 一致，供 capabilities() 展示与校验），
// 但**生成调用入口时必须用裸调用形态**——否则会编出 `ctx.call.Call(...)`，
// 那是「索引一个函数」，要到 Lua 运行期才炸（这个缺口此前一直没被 react/invoke 覆盖到）。
var bareFunctionPrimitives = map[string]bool{"call": true}

// builtinParamOrder 是**受 capability 门禁的内核原语**各方法的「位置参数名」顺序（与 lua.go 的
// ctx.* 绑定一一对应），供 buildActionEntry 把 LLM 提供的 args map 编成位置调用。
// 纯函数原子（time/template/crypto）与扩展（ext.*）都走单具名 table，不在此列。
var builtinParamOrder = map[string]map[string][]string{
	"http":      {"Request": {"method", "url", "body", "headers"}},
	"call":      {"Call": {"target", "input"}},
	"db":        {"query": {"table"}, "exec": {"table"}, "ping": {"table"}},
	"secret":    {"get": {"name"}},
	"event":     {"notify": {"name", "payload"}, "wait": {"name"}},
	"subscribe": {"start": {"table"}, "stop": {"handle"}},
}

// buildActionEntry 把一次解析出的工具调用编成一段 Lua 入口（复用真实 Tool 的 ctx.* 通道）：
//   - 内置原语：位置参数调用 ctx.<tool>.<method>(a, b, ...)，缺失可选参数传 nil（Lua 绑定按 GetTop 判缺省）；
//     裸函数绑定的原语（call）用 ctx.<tool>(a, b, ...)。
//   - 纯函数原子（time/template/crypto）：单具名 table 调用 ctx.<tool>.<method>({ ... })（与 ctx.ext.* 同款）。
//   - 扩展 ext.*：单具名 table 调用 ctx.<tool>.<method>({ ... })。
func buildActionEntry(tool, method string, args map[string]interface{}) (string, error) {
	// 纯函数原子先拦「方法名不存在」：react / invoke 路径直接调本函数，
	// 若放任它编出 `ctx.crypto.nope(...)`，要拖到 Lua 运行期才炸成「尝试调用 nil」——
	// 比注册期报错难查得多（红线：解析点拒绝未知值）。
	if pureFuncPrimitives[tool] {
		ms, ok := builtinCapabilitySchemas[tool]
		if !ok {
			return "", fmt.Errorf("纯函数原子 %q 没有登记方法 schema", tool)
		}
		if _, ok := ms[method]; !ok {
			return "", fmt.Errorf("纯函数原子 %q 没有方法 %q（合法方法：%s）", tool, method, strings.Join(sortedKeys(ms), ", "))
		}
		return fmt.Sprintf("local r = ctx.%s.%s(%s)\nctx.output = { result = r }\n", tool, method, luaLiteral(args)), nil
	}
	if order, ok := builtinParamOrder[tool]; ok {
		params, ok := order[method]
		if !ok {
			return "", fmt.Errorf("内置原语 %q 无方法 %q（仅支持能力目录已声明的方法）", tool, method)
		}
		// db 系列（driver/dsn/sql/params）走单 table 形态：ctx.db.query({driver=, dsn=, sql=, params=})，
		// 与 lua.go 的 ctx.db.* 绑定一致（见 builtinCapabilitySchemas["db"]）。
		if len(params) == 1 && params[0] == "table" {
			return fmt.Sprintf("local r = ctx.%s.%s(%s)\nctx.output = { result = r }\n", tool, method, luaLiteral(args)), nil
		}
		parts := make([]string, 0, len(params))
		for _, p := range params {
			parts = append(parts, luaLiteral(args[p]))
		}
		callee := "ctx." + tool + "." + method
		if bareFunctionPrimitives[tool] {
			callee = "ctx." + tool
		}
		return fmt.Sprintf("local r = %s(%s)\nctx.output = { result = r }\n", callee, strings.Join(parts, ", ")), nil
	}
	// ext.* 单 table（tool 形如 "ext.ai"，method 如 "complete"）。
	return fmt.Sprintf("local r = ctx.%s.%s(%s)\nctx.output = { result = r }\n", tool, method, luaLiteral(args)), nil
}

// luaLiteral 把 JSON 兼容的 Go 值序列化成安全的内联 Lua 字面量（供生成工具调用入口）。
func luaLiteral(v interface{}) string {
	switch x := v.(type) {
	case nil:
		return "nil"
	case string:
		return luaQuote(x)
	case bool:
		if x {
			return "true"
		}
		return "false"
	case float64:
		if x == math.Trunc(x) && !math.IsInf(x, 0) {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'g', -1, 64)
	case float32:
		return luaLiteral(float64(x))
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case int32:
		return strconv.FormatInt(int64(x), 10)
	case map[string]interface{}:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		ps := make([]string, 0, len(keys))
		for _, k := range keys {
			ps = append(ps, "["+luaQuote(k)+"] = "+luaLiteral(x[k]))
		}
		return "{" + strings.Join(ps, ", ") + "}"
	case []interface{}:
		ps := make([]string, 0, len(x))
		for _, e := range x {
			ps = append(ps, luaLiteral(e))
		}
		return "{" + strings.Join(ps, ", ") + "}"
	default:
		if b, err := json.Marshal(x); err == nil {
			return luaQuote(string(b))
		}
		return "nil"
	}
}

// luaQuote 转义并包裹成双引号 Lua 字符串字面量。
func luaQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString("\\\"")
		case '\\':
			b.WriteString("\\\\")
		case '\n':
			b.WriteString("\\n")
		case '\t':
			b.WriteString("\\t")
		case '\r':
			b.WriteString("\\r")
		case '\a':
			b.WriteString("\\a")
		case '\b':
			b.WriteString("\\b")
		case '\f':
			b.WriteString("\\f")
		case '\v':
			b.WriteString("\\v")
		case 0:
			b.WriteString("\\0")
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

var (
	reActionJSON = regexp.MustCompile("(?s)```json\\s*(\\{.*?\\})\\s*```")
	reFinal      = regexp.MustCompile(`(?i)Final Answer\s*:\s*([\s\S]*)`)
)

// parseReactAction 从 LLM 输出里解析 Action：在 ```json ... ``` 围栏内找 {tool,method?,args}。
// tool 形如 "http.Request"（内置）或 "ext.ai.complete"（扩展）；若 tool 无点且单独给了 method 则合并。
func parseReactAction(text string) (tool, method string, args map[string]interface{}, ok bool) {
	m := reActionJSON.FindStringSubmatch(text)
	if m == nil {
		return "", "", nil, false
	}
	var act struct {
		Tool   string                 `json:"tool"`
		Method string                 `json:"method"`
		Args   map[string]interface{} `json:"args"`
	}
	if err := json.Unmarshal([]byte(m[1]), &act); err != nil {
		return "", "", nil, false
	}
	if act.Tool == "" {
		return "", "", nil, false
	}
	tool, method = splitToolMethod(act.Tool, act.Method)
	args = act.Args
	if args == nil {
		args = map[string]interface{}{}
	}
	return tool, method, args, true
}

// splitToolMethod 把 tool 字段拆成 (能力名, 方法)：
//   - "ext.ai.complete" → ("ext.ai", "complete")（ext 命名空间两级）
//   - "http.Request" → ("http", "Request")
//   - "http"（无点，且 method="Request"）→ ("http", "Request")
func splitToolMethod(tool, method string) (string, string) {
	if strings.HasPrefix(tool, "ext.") {
		parts := strings.SplitN(tool, ".", 3)
		if len(parts) == 3 {
			return parts[0] + "." + parts[1], parts[2]
		}
		return tool, method
	}
	if i := strings.Index(tool, "."); i >= 0 {
		return tool[:i], tool[i+1:]
	}
	return tool, method
}

// extractFinalAnswer 从 LLM 输出里取 Final Answer 之后的全部内容。
func extractFinalAnswer(text string) (string, bool) {
	m := reFinal.FindStringSubmatch(text)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// trimThought 取 LLM 输出中 Action: 之前的部分作为「思考」（无 Action 则取全文）。
func trimThought(resp string) string {
	if i := strings.Index(strings.ToLower(resp), "action:"); i >= 0 {
		return strings.TrimSpace(resp[:i])
	}
	return strings.TrimSpace(resp)
}

// allowContains 报告工具是否在 allow 列表内（支持 "ext.*" 通配）。
func allowContains(allow []string, tool string) bool {
	for _, a := range allow {
		if a == tool {
			return true
		}
		if a == "ext.*" && strings.HasPrefix(tool, "ext.") {
			return true
		}
	}
	return false
}

// stableJSON 把任意值稳定序列化成 JSON 字符串（供 Observation 文本与重复守卫键）。
func stableJSON(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

// truncateRunes 按 rune 数截断字符串（避免超长 Observation 撑爆上下文）。
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "...(truncated)"
}

// appendUnique 向切片追加不重复的元素。
func appendUnique(s []string, v string) []string {
	for _, x := range s {
		if x == v {
			return s
		}
	}
	return append(s, v)
}

// ---- Generate 的内部状态与 Eino 节点 ----

// genState 是单趟生成链在节点间传递的共享状态（指针，原地改写）。
type genState struct {
	Kind      string
	Intent    string
	Tools     []ToolSummary
	Recipe    *Recipe
	Plans     []planStep
	LastErr   string
	Candidate string
}

// planStep 是拆解出的一个子任务 + 其域内候选工具。
type planStep struct {
	Subtask string
	Domain  string
	Tools   []ToolSummary
}

// decompose 节点：flow 调 LLM 拆成有序子任务；tool/hook 直接把意图当单步。
func (a *AIAgent) decompose(ctx context.Context, s *genState) (*genState, error) {
	if s.Kind != "flow" {
		s.Plans = []planStep{{Subtask: s.Intent, Domain: "general"}}
		return s, nil
	}
	dom := summarizeDomains(DomainOverview(s.Tools))
	prompt := "用户意图：" + s.Intent + "\n\n可选能力域：\n" + dom
	text, err := a.llm.Complete(ctx, LLMRequest{System: s.Recipe.SystemDecompose, Prompt: prompt, Format: "json"})
	if err != nil {
		return nil, err
	}
	if raw, err := extractJSONToAny(text); err == nil {
		if arr, ok := raw.([]interface{}); ok && len(arr) > 0 {
			plans := make([]planStep, 0, len(arr))
			for _, it := range arr {
				if m, ok := it.(map[string]interface{}); ok {
					plans = append(plans, planStep{Subtask: strOr(m, "subtask"), Domain: strOr(m, "domain")})
				}
			}
			if len(plans) > 0 {
				s.Plans = plans
				return s, nil
			}
		}
	}
	s.Plans = []planStep{{Subtask: s.Intent, Domain: "general"}}
	return s, nil
}

// catalog 节点：每步在域内 BM25 检索 Top-K 候选工具。
func (a *AIAgent) catalog(_ context.Context, s *genState) (*genState, error) {
	for i := range s.Plans {
		s.Plans[i].Tools = Retrieve(s.Tools, s.Plans[i].Domain, s.Plans[i].Subtask, 5)
	}
	return s, nil
}

// assemble 节点：按配方拼装提示，调 LLM 产出候选 JSON 文本。
func (a *AIAgent) assemble(ctx context.Context, s *genState) (*genState, error) {
	prompt := a.buildAssemblePrompt(s)
	text, err := a.llm.Complete(ctx, LLMRequest{System: s.Recipe.SystemAssemble, Prompt: prompt, Format: "json"})
	if err != nil {
		return nil, err
	}
	s.Candidate = text
	return s, nil
}

// buildAssemblePrompt 拼装 assemble 阶段的提示：子任务+候选工具 +（tool/hook）能力参数 + 输出约束 + 上次错误。
func (a *AIAgent) buildAssemblePrompt(s *genState) string {
	var sb strings.Builder
	sb.WriteString("用户意图：" + s.Intent + "\n")
	sb.WriteString("\n子任务与候选工具：\n")
	for i, p := range s.Plans {
		sb.WriteString(fmt.Sprintf("%d. 子任务：%s（域：%s）\n   候选工具：", i+1, p.Subtask, p.Domain))
		if len(p.Tools) == 0 {
			sb.WriteString("（无）\n")
		} else {
			ids := make([]string, 0, len(p.Tools))
			for _, t := range p.Tools {
				ids = append(ids, t.ID)
			}
			sb.WriteString(strings.Join(ids, " ") + "\n")
		}
	}
	if s.Kind == "flow" {
		// Flow 的 step 也能直调原子能力（call 写成「能力.方法」，见 engine.resolveStepCall）——
		// 因此 flow 生成同样要给出能力目录：否则 capabilities_hint 让模型「见 capabilities」
		// 却根本没有 capabilities 可看，且模型无从知道有哪些方法可用、要什么参数。
		sb.WriteString("\n可直接调用的原子能力（capabilities；把 step 的 call 写成「能力.方法」，input 即该方法参数）：\n")
		sb.WriteString(a.flowCapabilitiesText() + "\n")
	} else {
		sb.WriteString("\n可用能力与参数（capabilities，entry/action 只能使用列出的方法与参数）：\n")
		sb.WriteString(a.capabilitiesText() + "\n")
	}
	sb.WriteString("\n输出约束：\n" + s.Recipe.OutputHints + "\n")
	if s.Recipe.CapabilitiesHint != "" {
		sb.WriteString("\n能力提示：\n" + s.Recipe.CapabilitiesHint + "\n")
	}
	if s.LastErr != "" {
		sb.WriteString("\n上次静态校验错误：" + s.LastErr + "\n请修正后重新生成（call 必须是候选工具或 capabilities 里列出的「能力.方法」，依赖与模板引用合法）。\n")
	}
	return sb.String()
}

// capabilitySummaries 返回内核原语（builtin + pure）+ 已注册 ext.* 的方法级参数 schema
// （供 Lua 与 LLM 锚定）。渲染与 ext.registry.capabilities 共用 capabilityCatalog——
// REST / MCP 的能力目录与 Lua 侧自我锚定看到的是同一份，不会各列一套。
func (a *AIAgent) capabilitySummaries() []map[string]interface{} {
	return capabilityCatalog(a.extCaps)
}

// capabilitiesText 把能力参数压成给 LLM 看的文本（方法签名 + 参数 + 描述）。
// 顺序与内容取 allBuiltinPrimitives 单一来源；纯函数原子按「单具名 table」渲染
// （与 ctx.ext.* 同款），避免模型写成位置参数。
func (a *AIAgent) capabilitiesText() string {
	var sb strings.Builder
	for _, p := range allBuiltinPrimitives() {
		ms, ok := builtinCapabilitySchemas[p]
		if !ok {
			continue
		}
		for _, m := range sortedKeys(ms) {
			sch := ms[m]
			if pureFuncPrimitives[p] {
				sb.WriteString(fmt.Sprintf("- ctx.%s.%s({ %s }): %s\n", p, m, fieldsList(sch.Parameters), sch.Description))
				continue
			}
			sb.WriteString(fmt.Sprintf("- ctx.%s.%s(%s): %s\n", p, m, paramList(sch.Parameters), sch.Description))
		}
	}
	if len(a.extCaps) > 0 {
		sb.WriteString("# ctx.ext.<name>.<method> 只接受「单个具名 table」参数（字段名见下，" +
			"按当前已注册 ext.* 实时列出，非写死；新增扩展会自动出现在这里）：\n")
		for name, ms := range a.extCaps {
			for m, sch := range ms {
				sb.WriteString(fmt.Sprintf("- ctx.%s.%s({ %s }): %s\n", name, m, fieldsList(sch.Parameters), sch.Description))
				// 参数明细按名排序：map 迭代顺序是随机的，提示词必须可复现
				// （同一份目录渲染两次不一致 → 生成结果与测试都会随机漂移）。
				pkeys := make([]string, 0, len(sch.Parameters))
				for k := range sch.Parameters {
					pkeys = append(pkeys, k)
				}
				sort.Strings(pkeys)
				for _, k := range pkeys {
					ps := sch.Parameters[k]
					req := ""
					if ps.Required {
						req = " (必填)"
					}
					sb.WriteString(fmt.Sprintf("    · %s: %s — %s%s\n", k, ps.Type, ps.Description, req))
				}
			}
		}
	}
	return sb.String()
}

// flowCapabilitiesText 把能力目录压成「Flow step 直调」的形式：
// call 写成 "<能力>.<方法>"（**不带 ctx. 前缀**——flow 的 call 是调用目标标识，不是 Lua 表达式，
// 与 capabilitiesText 给 entry 用的 ctx.* 形态刻意区分开，避免模型写串）。
//
// 与 capabilitiesText 同源（同一份 builtinCapabilitySchemas + extCaps），只是渲染形态不同。
// 排序输出：提示词必须可复现，否则生成结果与测试都会随 map 迭代顺序漂移。
func (a *AIAgent) flowCapabilitiesText() string {
	var sb strings.Builder
	for _, p := range allBuiltinPrimitives() {
		ms, ok := builtinCapabilitySchemas[p]
		if !ok {
			continue
		}
		for _, m := range sortedKeys(ms) {
			sb.WriteString(fmt.Sprintf("- %s.%s  (参数：%s): %s\n", p, m, paramList(ms[m].Parameters), ms[m].Description))
		}
	}
	extNames := make([]string, 0, len(a.extCaps))
	for name := range a.extCaps {
		extNames = append(extNames, name)
	}
	sort.Strings(extNames)
	for _, name := range extNames {
		ms := a.extCaps[name]
		for _, m := range sortedKeys(ms) {
			sb.WriteString(fmt.Sprintf("- %s.%s  (参数：%s): %s\n", name, m, paramList(ms[m].Parameters), ms[m].Description))
		}
	}
	return sb.String()
}

// sortedKeys 返回方法 schema 的方法名（升序），供提示词稳定渲染。
func sortedKeys(ms map[string]MethodSchema) []string {
	out := make([]string, 0, len(ms))
	for m := range ms {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// fieldsList 把参数 schema 压成 Lua「单 table 具名参数」形式 "k = type, ..."，
// 供 ctx.ext.*.<method>({...}) 展示（与内置原语的位置参数区别开，避免模型写成位置参数）。
func fieldsList(params map[string]ParamSchema) string {
	if len(params) == 0 {
		return ""
	}
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	for i := 0; i < len(keys); i++ {
		for j := i + 1; j < len(keys); j++ {
			if keys[j] < keys[i] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+" = "+params[k].Type)
	}
	return strings.Join(parts, ", ")
}

// paramList 把参数 schema 压成 "k:type,..." 形式。
func paramList(params map[string]ParamSchema) string {
	if len(params) == 0 {
		return ""
	}
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	// 稳定顺序便于测试/可读
	for i := 0; i < len(keys); i++ {
		for j := i + 1; j < len(keys); j++ {
			if keys[j] < keys[i] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		req := ""
		if params[k].Required {
			req = "?"
		}
		parts = append(parts, k+req+":"+params[k].Type)
	}
	return strings.Join(parts, ", ")
}

// normalizeDef 补 LLM 漏写的默认字段（容错放易变层，稳定层校验保持严格）。
func (a *AIAgent) normalizeDef(kind string, def map[string]interface{}) {
	switch kind {
	case "tool":
		if def["runtime"] == nil {
			def["runtime"] = "lua"
		}
		if def["entry"] == nil {
			def["entry"] = "-- TODO: 实现入口"
		}
		if def["versions"] == nil {
			def["versions"] = []interface{}{map[string]interface{}{"version": "1.0.0", "state": "draft"}}
		}
		if def["input_schema"] == nil {
			def["input_schema"] = map[string]interface{}{"type": "object"}
		}
		if def["output_schema"] == nil {
			def["output_schema"] = map[string]interface{}{"type": "object"}
		}
		// 自动从 entry 里推导 capabilities（最小权限白名单）：模型经常忘记声明，
		// 而引擎在运行期会按声明的白名单拒绝未授权能力。此处按 entry 中实际用到的 ctx.* 补齐。
		if entry, _ := def["entry"].(string); entry != "" {
			if caps := deriveCapabilities(entry, toStringSlice(def["capabilities"])); len(caps) > 0 {
				def["capabilities"] = caps
			}
		}
	case "flow":
		if def["vars"] == nil {
			def["vars"] = map[string]interface{}{}
		}
	}
}

// builtinCapabilityNames 是**受 capability 门禁**的内核原语名（与 cap.KnownPrimitives 一致，
// 用于从 entry 推导 capabilities）。
//
// ⚠️ 纯函数原子（time/template/crypto）**刻意不在此表**：它们无需声明 capabilities
// （见 purefunc.go）。若把它们也算进来，deriveCapabilities 会把 "time" 之类写进 Tool.Capabilities，
// 而 GrantsFromStrings 只认 cap.KnownPrimitives ∪ 已注册 ext.* → 生成出来的 Tool 会在
// 「未知的 capability "time"」上校验失败（一句话：自动推导只登记真需要授权的原语）。
var builtinCapabilityNames = map[string]bool{
	"http": true, "call": true, "db": true, "secret": true, "event": true, "subscribe": true,
}

var (
	reCtxExtCall     = regexp.MustCompile(`ctx\.ext\.([A-Za-z0-9_]+)\.`)
	reCtxBuiltinCall = regexp.MustCompile(`ctx\.([A-Za-z0-9_]+)\.`)
)

// deriveCapabilities 从生成的 Lua entry 自动推导需要声明的 capabilities：
//   - ctx.ext.<x>.<m>  -> "ext.<x>"；
//   - ctx.<builtin>.<m>-> "<builtin>"（仅受门禁的内核原语；纯函数原子不在此列，见 builtinCapabilityNames）；
//
// 并与模型已声明的做并集。空则返回 nil（不写空字段）。
func deriveCapabilities(entry string, existing []string) []string {
	set := map[string]bool{}
	for _, c := range existing {
		if s := strings.TrimSpace(c); s != "" {
			set[s] = true
		}
	}
	for _, m := range reCtxExtCall.FindAllStringSubmatch(entry, -1) {
		set["ext."+m[1]] = true
	}
	for _, m := range reCtxBuiltinCall.FindAllStringSubmatch(entry, -1) {
		if builtinCapabilityNames[m[1]] {
			set[m[1]] = true
		}
	}
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for c := range set {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// toStringSlice 把 YAML/JSON 解出的 []interface{} 或 []string 归一成 []string。
func toStringSlice(v interface{}) []string {
	switch t := v.(type) {
	case []string:
		return t
	case []interface{}:
		out := make([]string, 0, len(t))
		for _, x := range t {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// registerDef 把生成的草稿落库（kind 决定落到哪个表）。
func (a *AIAgent) registerDef(reg *registry.Registry, kind string, def map[string]interface{}) error {
	b, err := json.Marshal(def)
	if err != nil {
		return err
	}
	switch kind {
	case "tool":
		var t model.Tool
		if err := json.Unmarshal(b, &t); err != nil {
			return fmt.Errorf("Tool 草稿结构非法: %w", err)
		}
		return reg.UpsertTool(&t)
	case "hook":
		var h model.Hook
		if err := json.Unmarshal(b, &h); err != nil {
			return fmt.Errorf("Hook 草稿结构非法: %w", err)
		}
		return reg.UpsertHook(&h)
	default:
		var f model.Flow
		if err := json.Unmarshal(b, &f); err != nil {
			return fmt.Errorf("Flow 草稿结构非法: %w", err)
		}
		return reg.UpsertFlow(&f)
	}
}

// ---- 辅助 ----

// loadTools 从 registry 读出工具目录（仅 id+描述+schema，遵循 §9.3 最小上下文）。
func (a *AIAgent) loadTools() ([]ToolSummary, error) {
	ts, err := a.reg.ListTools()
	if err != nil {
		return nil, err
	}
	out := make([]ToolSummary, 0, len(ts))
	for _, t := range ts {
		out = append(out, ToolSummary{ID: t.ID, Description: t.Description, InputSchema: t.InputSchema, OutputSchema: t.OutputSchema})
	}
	return out, nil
}

// parseToolSummaries 把注入的 tools（[]interface{} of map）转成 []ToolSummary。
func parseToolSummaries(v interface{}) []ToolSummary {
	out := []ToolSummary{}
	arr, ok := v.([]interface{})
	if !ok {
		return out
	}
	for _, item := range arr {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		ts := ToolSummary{ID: strOr(m, "id"), Description: strOr(m, "description")}
		if s, ok := m["input_schema"].(map[string]interface{}); ok {
			ts.InputSchema = s
		}
		if s, ok := m["output_schema"].(map[string]interface{}); ok {
			ts.OutputSchema = s
		}
		out = append(out, ts)
	}
	return out
}

func strOr(m map[string]interface{}, k string) string {
	if s, ok := m[k].(string); ok {
		return s
	}
	return ""
}

// ---- 进程内 ext.ai 扩展注册（ctx.ext.ai.*） ----

// GenerateOpts 携带 generate 的可选项。
type GenerateOpts struct {
	MaxRetries   int
	RegisterInto *registry.Registry // 非空则在生成成功后把草稿落库
	// 模型参数覆盖（可选）：非空字段会填进本次生成**每一次** LLM 调用的空参数位，
	// 优先级为「调用点显式给的参数 > 这里的覆盖 > 环境变量 > 内置默认」（见 ai_llm.go 的
	// resolveLLMConfig）。CLI `gear ai gen` 用它把 GEAR_AI_* 显式注入，使
	// 「agent 本体只认调用参数」与「命令行能配模型」两件事同时成立。
	Provider string
	Model    string
	BaseURL  string
	APIKey   string
}

// withLLMOverrides 返回一份「LLM 已按 opts 包装」的浅拷贝 agent；opts 未给模型参数时原样返回。
//
// 浅拷贝是安全的：AIAgent 其余字段都是只读依赖（reg / recipes / extCaps / validateFlow / exec），
// 唯一被替换的是 llm 接口值本身。不原地改写 agent——它由引擎长期持有，并发生成会互相串味。
func (a *AIAgent) withLLMOverrides(opts GenerateOpts) *AIAgent {
	if opts.Provider == "" && opts.Model == "" && opts.BaseURL == "" && opts.APIKey == "" {
		return a
	}
	cp := *a
	cp.llm = overrideLLM{base: a.llm, opts: opts}
	return &cp
}

// overrideLLM 把 opts 的模型参数填进请求的空字段后转发给底层 LLM。
// **只填空字段**：调用点显式传了 api_key/model 的（如 Lua 侧 ctx.ext.ai.complete 带参数）
// 优先级更高，不被这里的覆盖顶掉。
type overrideLLM struct {
	base LLM
	opts GenerateOpts
}

func (o overrideLLM) Complete(ctx context.Context, req LLMRequest) (string, error) {
	if req.Provider == "" {
		req.Provider = o.opts.Provider
	}
	if req.Model == "" {
		req.Model = o.opts.Model
	}
	if req.BaseURL == "" {
		req.BaseURL = o.opts.BaseURL
	}
	if req.APIKey == "" {
		req.APIKey = o.opts.APIKey
	}
	return o.base.Complete(ctx, req)
}

// CompleteStream 与 Complete 同款：先填 opts 的空字段，再转发给底层 LLM 的流式实现。
func (o overrideLLM) CompleteStream(ctx context.Context, req LLMRequest, onDelta func(content, reasoning string) error) error {
	if req.Provider == "" {
		req.Provider = o.opts.Provider
	}
	if req.Model == "" {
		req.Model = o.opts.Model
	}
	if req.BaseURL == "" {
		req.BaseURL = o.opts.BaseURL
	}
	if req.APIKey == "" {
		req.APIKey = o.opts.APIKey
	}
	return o.base.CompleteStream(ctx, req, onDelta)
}

// GenResult 是 generate 的产物。
type GenResult struct {
	Kind string
	Def  map[string]interface{}
}

// AIExtensionInProcess 把 AIAgent 暴露为进程内扩展原语 ctx.ext.ai。
// 取代原 sidecar 子进程载体（用户决策：ext.ai 进程内）。
//
// 方法分两类：
//   - 同步方法（aiSyncMethods）：走 callAIMethod，与原 InProcessExtension 行为一致；
//   - complete_stream：回调式，Lua 侧以 ctx.ext.ai.complete_stream(args, function(token, reasoning) ...)
//     调用，每个 LLM 增量触发一次回调（REST SSE 端点走同一条 AIAgent.CompleteStream 通道）。
//
// aiMethodNames 是「注册方法清单」（用于 capabilities 目录与 schema 守卫），含 complete_stream；
// aiSyncMethods 是「走同步分发的子集」。两者不一致会让静态校验出错，由
// TestAIMethodNamesMatchDefaultSchemas 守着。
var aiMethodNames = []string{"complete", "catalog", "domains", "validate", "generate", "capabilities", "react", "complete_stream"}

// aiSyncMethods 是 ext.ai 里「同步分发」的方法（不含回调式的 complete_stream）。
var aiSyncMethods = []string{"complete", "catalog", "domains", "validate", "generate", "capabilities", "react"}

// callAIMethod 按方法名分发到 AIAgent 的稳定层方法（同步路径），供 Lua / go-host Provider 共用。
func callAIMethod(a *AIAgent, method string, args map[string]interface{}) (interface{}, error) {
	switch method {
	case "complete":
		return a.Complete(args)
	case "catalog":
		return a.Catalog(args)
	case "domains":
		return a.Domains(args)
	case "validate":
		return a.ValidateMethod(args)
	case "capabilities":
		return a.CapabilitiesMethod()
	case "generate":
		return a.GenerateMethod(args)
	case "react":
		return a.ReactMethod(args)
	default:
		return nil, fmt.Errorf("ext.ai 不支持的方法 %q（仅 complete/catalog/domains/validate/generate/capabilities/react/complete_stream）", method)
	}
}

// aiInProcessProvider 是 ext.ai 的 go-host 原生实现（host.Extension("ext.ai") 取用）。
// complete_stream 无法在同步 Provider 接口上表达流式回调，故显式报错引导到 Lua / REST 流式面。
type aiInProcessProvider struct{ a *AIAgent }

func (p *aiInProcessProvider) Call(method string, args map[string]interface{}) (interface{}, error) {
	if method == "complete_stream" {
		return nil, fmt.Errorf("ext.ai.complete_stream 仅支持 Lua 回调式调用（ctx.ext.ai.complete_stream(args, function)）或 REST SSE 端点，无法通过同步 Provider 调用")
	}
	return callAIMethod(p.a, method, args)
}

// AIExtensionInProcess 构造 ext.ai 的进程内扩展定义：同步方法 + complete_stream 回调式。
func AIExtensionInProcess(a *AIAgent) *CapabilityExtension {
	return &CapabilityExtension{
		Provider: func(c *cap.Context) (interface{}, error) {
			return &aiInProcessProvider{a: a}, nil
		},
		BuildLua: func(L *lua.LState, c *cap.Context) (lua.LValue, error) {
			tbl := L.NewTable()
			// 同步方法：与旧 InProcessExtension 同款（args table → 同步结果）。
			for _, m := range aiSyncMethods {
				method := m
				tbl.RawSetString(m, L.NewFunction(func(L *lua.LState) int {
					args := map[string]interface{}{}
					if mt, ok := fromLuaValue(L, L.Get(1)).(map[string]interface{}); ok {
						args = mt
					}
					res, err := callAIMethod(a, method, args)
					if err != nil {
						L.RaiseError("%v", err)
						return 0
					}
					L.Push(toLuaValue(L, res))
					return 1
				}))
			}
			// complete_stream：arg1 = args table，arg2 = callback function（每个增量触发一次）。
			tbl.RawSetString("complete_stream", L.NewFunction(func(L *lua.LState) int {
				var args map[string]interface{}
				if mt, ok := fromLuaValue(L, L.Get(1)).(map[string]interface{}); ok {
					args = mt
				}
				cb := L.Get(2)
				cbfn, ok := cb.(*lua.LFunction)
				if !ok {
					L.RaiseError("ext.ai.complete_stream 需要两个参数：args(table) 与 callback(function)")
					return 0
				}
				err := a.CompleteStream(args, func(content, reasoning string) error {
					if err := L.CallByParam(lua.P{Fn: cbfn, NRet: 0, Protect: true}, lua.LString(content), lua.LString(reasoning)); err != nil {
						return err
					}
					return nil
				})
				if err != nil {
					L.RaiseError("%v", err)
					return 0
				}
				return 0
			}))
			return tbl, nil
		},
	}
}
