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
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

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
	// retryBackoff 是恢复层（ai_recovery.go）机械重试的退避函数：attempt → 睡眠时长。
	// nil = 默认（150ms 起翻倍、封顶 2s）。测试注入零值——恢复路径必须确定性，
	// 不能靠「睡一会儿看起来对了」。
	retryBackoff func(attempt int) time.Duration
}

// newAIAgent 构造进程内 agent。
func newAIAgent(reg *registry.Registry, llm LLM, recipes RecipeLoader, extCaps map[string]map[string]MethodSchema) *AIAgent {
	if recipes == nil {
		recipes = DefaultRecipeLoader("")
	}
	return &AIAgent{reg: reg, llm: llm, recipes: recipes, extCaps: extCaps}
}

// SetConfigStore 把配置库设为 AI 缺省配置的读取源（ai.provider / ai.model / ai.base_url /
// ai.api_key / ai.max_tokens）。由 Engine 在装配了 config 原语时调用（bindAIAgent）。
//
// 只对 HTTPLLM 生效：MockLLM 是离线确定性实现，让它读本机配置会让测试结果随开发者机器漂移。
// 未装配（c == nil）或 llm 不是 HTTPLLM 时静默跳过——「没配就是没配」，回退链退化为
// 「调用参数 > 环境变量 > 内置默认」，与装配前行为完全一致。
func (a *AIAgent) SetConfigStore(c cap.Config) {
	if c == nil {
		return
	}
	if h, ok := a.llm.(*HTTPLLM); ok {
		h.Config = configLookupFrom(c)
	}
}

// configLookupFrom 把 cap.Config 适配成 AI 缺省配置的读取源。
// 「该项不存在」与「读库失败」都报告未配置（false），统一交给下一级回退——
// 配置库不可用不该让 AI 调用直接失败，它只是默认值的一个来源。
func configLookupFrom(c cap.Config) ConfigLookup {
	return func(key string) (string, bool) {
		v, err := c.Get(key)
		if err != nil {
			return "", false
		}
		return v, true
	}
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

// Find 在**能力目录**里按 BM25 检索方法（ext.ai.find）。
//
// 与 Catalog 的区别必须说死：Catalog/Retrieve 检索的是 registry 里的业务 Tool（设计态
// generate 生成 Flow 用），Find 检索的是内核能力目录的**方法**（http.Request / ext.redis.set）。
// 两者语料不相交——用 Catalog 去找方法只会拿到空结果，还容易误判成「检索坏了」。
func (a *AIAgent) Find(args map[string]interface{}) (interface{}, error) {
	query, _ := args["query"].(string)
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("find 需要 query（检索词）；要看全部能力域请用 ext.ai.capabilities()")
	}
	domain, _ := args["domain"].(string)
	k := 5
	// toFloat 而非断言 float64：Lua 整数字面量出来是 int64（见 ReactMethod 同款坑）。
	if v, ok := toFloat(args["k"]); ok && int(v) > 0 {
		k = int(v)
	}
	return map[string]interface{}{"hits": a.FindCapability(query, domain, k)}, nil
}

// Describe 返回一个方法的完整参数契约（ext.ai.describe）。
// 与 Find 合成「按需展开」：先用 find 看清有哪些方法，再只取要用的那一个的 schema——
// 不用一次把 115 个方法的完整 schema 都摊开。
func (a *AIAgent) Describe(args map[string]interface{}) (interface{}, error) {
	id, _ := args["id"].(string)
	if strings.TrimSpace(id) == "" {
		return nil, fmt.Errorf("describe 需要 id（形如 http.Request 或 ext.redis.set）")
	}
	sch, ok := a.DescribeCapability(id)
	if !ok {
		// 明确报错而不是回空：空结果会让调用方以为「这个方法的参数就是空」。
		return nil, fmt.Errorf("能力 %q 不在目录里；用 ext.ai.find 检索，或 ext.ai.capabilities() 看全目录", id)
	}
	return sch, nil
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
	Allow       []string // 限制可用工具（如 ["http","secret"]）
	AllowSet    bool     // Allow 是否由调用方**显式**给定（= 该 Tool 自己声明的那份 capability）
	Provider    string   // 模型厂商（可选，覆盖 env）
	Model       string   // 模型名（可选）
	BaseURL     string   // API base（可选）
	APIKey      string   // API key（可选，优先 env）
	Temperature float64  // 采样温度（可选，默认 0.3）
	Mode        string   // 工具面模式：""（默认）= 扁平全量；"meta" = 4 个元工具按需检索
}

// ReactModeMeta 是「元工具模式」：工具面恒定为 4 个元工具（find / describe / invoke / registry），
// 与能力目录规模（实测 28 能力 / 115 方法）彻底解耦。模型先用 find 检索、describe 取 schema，
// 再用 invoke 直调——单轮请求的工具定义从 10-15k token 降到几百。flat 模式仍是默认，保证兼容。
const ReactModeMeta = "meta"

// reactAllow 把「react 能调哪些工具」从**隐式约定**变成**显式值**。
//
// 旧语义里「allow 没传 = 全部门禁原语」：一个只声明 `capabilities: [ext.ai]` 的 agent Tool
// 实际能把 secret.get / db.exec 交给模型调——能力边界写在 Tool 声明上，却不在执行通道上。
// 这里不再靠 len(names)==0 猜意图：all 为 true 才表示「未限定」，否则 names 就是白名单
// （连空名单也表示「一个都不给」。
type reactAllow struct {
	names   []string
	methods map[string]bool // "能力.方法" 精确白名单；空 = 该能力整体开放，不按方法收窄
	all     bool
}

// ReactAllowAll 是 allow 里的通配符：显式要求不设限（整个能力目录）。
//
// 默认不再是全量。能力目录实测 28 个能力 / 115 个方法，全量下发意味着每轮请求都塞进
// 115 条 function 声明——工具越多模型越选不准（工具面膨胀），且重型扩展里绝大多数方法
// 与当前任务无关。要全量必须显式写出来。
const ReactAllowAll = "*"

// maxReactTools 是单次 react 允许下发的工具数上限，超出直接报错（不静默截断）。
//
// 不截断的理由：截断等于偷偷改了 agent 的能力边界，模型调不到某个工具却不知道为什么，
// 只能在错误里瞎猜。报错能逼调用方显式收窄——换用方法级 allow，或砍掉无关能力。
const maxReactTools = 48

// defaultReactCapabilities 是「没显式给 allow」时的默认工具集（Go 直调路径：CLI / 测试 / 宿主）。
//
// 刻意排除重型扩展（ext.redis 一家 29 个方法、ext.oss 6 个）与 crypto（13 个方法）：
// 它们跟大多数任务无关，白占请求体积还干扰选择，要用就显式声明。
// ext.registry 必留：agent 靠它查 registry 里有什么 Tool / Flow，是「按需发现」的入口。
var defaultReactCapabilities = []string{
	"http", "call", "db", "secret", "config",
	"time", "template", "id", "validate",
	"ext.registry",
}

// resolveReactAllow 由调用方给定的原始 allow 与「是否显式给定」解析出允许集。
//
// set=false 不再是「不设限」，而是回到默认集——历史行为会把 115 个方法全塞进请求。
// 允许项可以是能力名（"ext.redis"，该能力所有方法）或方法名（"ext.redis.set"）。
//
// strict 区分两种来源，与 Gear 一贯的「显式从严、继承从宽」同口径：
//   - strict=true（调用方**显式**给的 allow）：认不出的项直接报错。静默忽略等于让调用方
//     以为开了权限实际没开，agent 调不到工具还不知道为什么。
//   - strict=false（从 Tool 声明的 capabilities **继承**来的）：未装配的能力跳过。那是
//     定义的问题、不是本次调用的问题，且注册期已校验过合法性——为一个没启用的扩展
//     把整个 react 打挂，代价与问题严重性不匹配。
func (a *AIAgent) resolveReactAllow(names []string, set, strict bool) (reactAllow, error) {
	if !set {
		caps := make([]string, 0, len(defaultReactCapabilities))
		for _, c := range defaultReactCapabilities {
			if a.knownCapability(c) {
				caps = append(caps, c)
			}
		}
		return reactAllow{names: caps}, nil
	}
	caps := []string{}
	methods := map[string]bool{}
	seen := map[string]bool{}
	for _, raw := range names {
		x := strings.TrimSpace(raw)
		if x == "" {
			continue
		}
		if x == ReactAllowAll {
			return reactAllow{all: true}, nil
		}
		if a.knownCapability(x) {
			if !seen[x] {
				seen[x] = true
				caps = append(caps, x)
			}
			continue
		}
		// 不是能力名，按「<能力>.<方法>」解析：方法名不含点，取最后一段为方法名
		// （ext.ai.complete → ext.ai + complete；http.Request → http + Request）。
		capName, method, ok := splitCapMethod(x)
		if !ok || !a.knownMethod(capName, method) {
			if !strict {
				continue
			}
			return reactAllow{}, fmt.Errorf("react 的 allow 含 %q：既不是已知能力，也不是「已知能力.方法」；"+
				"可用 gear ai capabilities 或 ext.ai.capabilities() 看目录", x)
		}
		if !seen[capName] {
			seen[capName] = true
			caps = append(caps, capName)
		}
		methods[x] = true
	}
	return reactAllow{names: caps, methods: methods}, nil
}

// allows 报告 capName.method 是否可以调用：all 时不问名单；命中能力后，只有在存在方法级
// 收窄时才逐个方法比对（没有方法级 allow 即该能力整体开放）。
func (r reactAllow) allows(capName, method string) bool {
	if r.all {
		return true
	}
	if !allowContains(r.names, capName) {
		return false
	}
	if len(r.methods) == 0 {
		return true
	}
	return r.methods[capName+"."+method]
}

// String 把允许集渲染成可读形式，用于「工具不在允许列表」的报错——报错里必须点名
// 到底允许了什么，否则调用方只能猜。
func (r reactAllow) String() string {
	if r.all {
		return "*"
	}
	if len(r.methods) > 0 {
		ms := make([]string, 0, len(r.methods))
		for m := range r.methods {
			ms = append(ms, m)
		}
		sort.Strings(ms)
		return strings.Join(ms, ", ")
	}
	return strings.Join(r.names, ", ")
}

// knownCapability 报告 name 是否是当前装配的能力名（门禁原语 / 纯函数原子 / 已注册 ext.*）。
func (a *AIAgent) knownCapability(name string) bool {
	for _, p := range allBuiltinPrimitives() {
		if p == name {
			return true
		}
	}
	_, ok := a.extCaps[name]
	return ok
}

// knownMethod 报告 capName.method 是否在能力目录里真实存在。
func (a *AIAgent) knownMethod(capName, method string) bool {
	if ms, ok := builtinCapabilitySchemas[capName]; ok {
		_, ok := ms[method]
		return ok
	}
	if ms, ok := a.extCaps[capName]; ok {
		_, ok := ms[method]
		return ok
	}
	return false
}

// knownOnly 从一批能力名里挑出当前**已装配**的那些（未装配的丢弃）。
// 只用于「从 Tool 声明继承来的能力清单」——显式给的 allow 走 resolveReactAllow 的 strict 路径。
func (a *AIAgent) knownOnly(names []string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		if a.knownCapability(n) {
			out = append(out, n)
		}
	}
	return out
}

// splitCapMethod 把 "ext.redis.set" 拆成 ("ext.redis", "set")。
func splitCapMethod(s string) (string, string, bool) {
	i := strings.LastIndex(s, ".")
	if i <= 0 || i == len(s)-1 {
		return "", "", false
	}
	return s[:i], s[i+1:], true
}

// React 跑一个 ReAct 循环：把任务交给 LLM，让它逐步「思考 + 调用内置工具」，Go 侧把工具调用
// 编成一段 Lua 入口、经与真实 Tool 完全相同的宿主能力通道（host.* / ctx.ext.*）执行，观察结果再
// 喂回 LLM，直到 LLM 给出 Final Answer 或达到最大步数。
//
// react「可以使用内置所有工具」即此意：每步 LLM 选中的工具（内核原语 http/call/db/secret/event/subscribe
// 与纯函数原子 time/template/crypto/id/validate，或已注册 ext.*）都经 exec 执行——复用引擎既有的最小权限 + Lua 沙箱，
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

	// 允许集在这里定型，不再有「没传就全给」的隐式回退。
	// Allow 非空即视为「已给定」：否则 Go 侧写 `Allow:["http"]` 却忘了 AllowSet，会静默
	// 落回默认集——那正是被收掉的同一个坑，只是换了个方向。只有「两个都没给」才是默认集。
	allow, err := a.resolveReactAllow(opts.Allow, opts.AllowSet || len(opts.Allow) > 0, true)
	if err != nil {
		return nil, err
	}
	metaMode := opts.Mode == ReactModeMeta
	tools, toolByName := a.reactToolSpecs(allow, metaMode)
	// 工具数上限（flat 模式）：宁可报错让人收窄，也不静默截断——meta 模式工具面恒定为 4 个，不在此列。
	if !metaMode && len(tools) > maxReactTools {
		return nil, fmt.Errorf("react 工具数 %d 超过上限 %d：工具越多模型越选不准；"+
			"请用方法级 allow（形如 \"ext.redis.set\"）收窄，或改用元工具模式（mode=\"meta\"，工具面恒定 4 个）", len(tools), maxReactTools)
	}
	sys := a.reactSystemPrompt(allow, opts.MaxSteps, len(tools) > 0, metaMode)
	// msgs 是本任务的完整消息序列（取代原先「把历史拼成一段长文本」的做法）。
	//
	// 必须按协议维护而不是拼文本：assistant 消息里带了 tool_calls 之后，紧跟着的必须是
	// `{"role":"tool","tool_call_id":...}`——拼成普通 user 文本的话模型看不到「工具已经跑过了」，
	// 会再次发起同一个调用（真机实测：连发三次同一调用，全被重复调用守卫拦下，任务跑废）。
	msgs := []map[string]any{
		{"role": "system", "content": sys},
		{"role": "user", "content": "任务：" + task + "\n\n请开始（先写 Thought，再输出 Action 或 Final Answer）。"},
	}
	steps := []map[string]interface{}{}
	usedTools := []string{}
	seen := map[string]bool{}
	finalAnswer := ""
	// 黑板：失败/恢复流水/缺口全程记在这里（REQ-REACT-RECOVERY）。它既是恢复决策的
	// 状态，也是可观测面——结果里的 recovery_log 与 gaps 直接由它产出。
	board := newReactBoard(opts.MaxSteps)

	// appendAssistant 把模型这一轮的回话按协议塞回序列：带 tool_calls 时必须原样带上，
	// 否则下一条 tool 结果没有可对应的 id（OpenAI 兼容端点会直接判非法）。
	appendAssistant := func(text string, calls []ToolCall) {
		m := map[string]any{"role": "assistant", "content": text}
		if len(calls) > 0 {
			tcs := make([]map[string]any, 0, len(calls))
			for _, c := range calls {
				tcs = append(tcs, map[string]any{
					"id":       c.ID,
					"type":     "function",
					"function": map[string]any{"name": c.Name, "arguments": c.Arguments},
				})
			}
			m["tool_calls"] = tcs
		}
		msgs = append(msgs, m)
	}

	// execute 跑一次工具调用并返回回灌给模型的 Observation：allow 守卫 → 震荡守卫 →
	// 重复调用守卫 → 编 Lua 入口 → 沙箱执行（带 L0 机械重试）→ 失败分类与信号回灌。
	// 抽成闭包是因为「原生 tool_calls」与「文本解析兜底」两条路径共用同一套守卫——
	// 抄两份的话，将来加一条守卫必然漏改其中一份（判据③：保持简洁）。
	//
	// 恢复分层（REQ-REACT-RECOVERY，与 REWOO-with-recovery 同构）：
	//   L0    机械重试 transient / 可重试 HTTP 状态码——不烧 LLM 轮次（重试的是同一次模型决策）；
	//   L1    结构化失败信号回灌（类型/范围/原因/建议）——模型按信号改下一步，即 react 的「重规划」；
	//   兜底  连续失败达阈值 → 封禁该动作 + 记缺口（震荡守卫）；auth/unrecoverable 立即记缺口。
	// 重复调用守卫拦的是**模型重发**；机械重试是引擎行为，不受它管（否则 transient 永远没得重试）。
	execute := func(tool, method string, args map[string]interface{}, thought string) string {
		action := tool + "." + method
		record := func(obs string) string {
			steps = append(steps, map[string]interface{}{"thought": thought, "action": action, "observation": obs})
			return obs
		}
		if !allow.allows(tool, method) {
			return record(fmt.Sprintf("工具 %q 不在允许列表 [%s] 中，请改用允许的工具", action, allow.String()))
		}
		// 震荡守卫：同一动作同类型失败已达阈值 → 不再执行，回灌封禁说明（并提示缺口作答）。
		if notice, blocked := board.blockedNotice(action); blocked {
			return record(notice)
		}
		key := action + " " + stableJSON(args)
		if seen[key] {
			return record("检测到重复调用相同工具与参数，疑似陷入循环，已停止该步")
		}
		seen[key] = true
		entry, err := buildActionEntry(tool, method, args)
		if err != nil {
			// 编不出入口 = 模型选错了方法/形态（如内置原语没有该方法）。按 bad_input 记：
			// 模型换对方法就能好，机械重试同样输入毫无意义。
			f := reactFailure{Class: FailBadInput, Action: action, Reason: err.Error(), Scope: board.scopeHint()}
			board.noteFailure(f)
			return record(failureSignal(f))
		}
		// L0：执行 + 机械重试。两类触发：exec 报 transient 错；结果带可重试 HTTP 状态码
		// （ctx.http.Request 不把非 2xx 当错误、status 在 body 里，不接住 L0 就对 HTTP 工具不存在）。
		var out map[string]interface{}
		var execErr error
		attempts := 0
		for {
			out, execErr = a.exec(ctx, entry, map[string]interface{}{})
			if execErr == nil {
				if _, retryable := retryableHTTPStatus(out); !retryable ||
					attempts >= maxToolRetries || !board.spendRetry() || ctx.Err() != nil {
					break
				}
				attempts++
				board.recordRecovery(fmt.Sprintf("L0 重试 %s（HTTP 状态码可重试，第 %d 次）", action, attempts))
				sleepBackoff(a.retryBackoff, attempts-1)
				continue
			}
			f := classifyToolFailure(action, execErr)
			if !failureRecovery[f.Class].retry || attempts >= maxToolRetries ||
				!board.spendRetry() || ctx.Err() != nil {
				// 不可机械恢复（或重试耗尽）→ 登记失败 + 回灌结构化信号，下一步交给模型（L1）。
				f.Scope = board.scopeHint()
				f.Attempts = attempts
				board.noteFailure(f)
				board.noteEvidence(action, false)
				return record(failureSignal(f))
			}
			attempts++
			board.recordRecovery(fmt.Sprintf("L0 重试 %s（%s，第 %d 次）：%v", action, f.Class, attempts, execErr))
			sleepBackoff(a.retryBackoff, attempts-1)
		}
		// 空结果：不是错误（确实没数据就是正确答案），只登记 semantic_empty 供震荡守卫计数。
		if isEmptyToolResult(out) {
			board.noteFailure(reactFailure{Class: FailSemanticEmpty, Action: action,
				Reason: "工具返回空结果", Scope: board.scopeHint()})
			board.noteEvidence(action, false)
			return record("【空结果】查询未返回任何数据。如确认目标不存在，请直接说明「未找到」，不要换参数反复查询。")
		}
		board.noteEvidence(action, true)
		var obs string
		if r, ok := out["result"]; ok {
			obs = stableJSON(r)
		} else {
			obs = stableJSON(out)
		}
		obs = truncateObservation(obs, 3000)
		usedTools = appendUnique(usedTools, action)
		return record(obs)
	}

	// runCall 执行一条原生 tool_call，返回要回灌的 Observation（消息组装由调用方按 id 负责）。
	// 与 execute 同理抽成闭包：原生协议路径和文本兜底路径共用同一套守卫，抄两份必漏改。
	runCall := func(c ToolCall, thought string) string {
		orig, ok := toolByName[c.Name]
		if !ok {
			// 模型给了目录里没有的名字：明确回绝并列出可用工具。少了这一步，它拿到的是
			// 一段 Lua 栈错误（attempt to index a nil value），只能瞎猜重试。
			// 工具多时如实截断（不静默缩水）：列全量本身就会把上下文撑爆。
			names := make([]string, 0, len(tools))
			for _, t := range tools {
				names = append(names, t.Name)
			}
			listed := names
			if len(listed) > 20 {
				listed = append(listed[:20:20], fmt.Sprintf("…（共 %d 个，其余请用检索工具查找）", len(names)))
			}
			obs := fmt.Sprintf("工具 %q 不存在，可用工具：%s", c.Name, strings.Join(listed, ", "))
			// 记 bad_input：模型选了个不存在的工具，它自己就能改（换清单里的）。
			board.noteFailure(reactFailure{Class: FailBadInput, Action: c.Name,
				Reason: "工具不存在（不在本次允许的工具面内）", Scope: "self"})
			steps = append(steps, map[string]interface{}{"thought": thought, "action": nil, "observation": obs})
			return obs
		}
		args := map[string]interface{}{}
		if s := strings.TrimSpace(c.Arguments); s != "" {
			if err := json.Unmarshal([]byte(s), &args); err != nil {
				obs := fmt.Sprintf("工具 %q 的参数不是合法 JSON：%v", orig, err)
				board.noteFailure(reactFailure{Class: FailBadInput, Action: orig,
					Reason: "tool_call 参数不是合法 JSON：" + err.Error(), Scope: "self"})
				steps = append(steps, map[string]interface{}{"thought": thought, "action": orig, "observation": obs})
				return obs
			}
			if args == nil {
				args = map[string]interface{}{}
			}
		}
		tool, method := splitToolMethod(orig, "")
		return execute(tool, method, args, thought)
	}

	// isMetaRetrieval 报告 orig（压平前的工具全名）是不是「检索类元工具」——find / describe /
	// registry。这类调用不计入执行步数预算，且绕过 allow 守卫（它们是发现机制本身，元工具
	// 模式下永远可用；若被 allow 拦住，反而让 invoke 没东西可查）。
	//
	// invoke 不在这里：它是真正的执行动作，必须受 allow 与步数预算约束。
	isMetaRetrieval := func(orig string) bool {
		return orig == "ext.ai.find" || orig == "ext.ai.describe" || orig == "ext.registry.tools"
	}

	// execInternal 与 execute 同执行通道，但**跳过 allow 守卫**——只用于元工具外壳
	// （find/describe/registry），它们本身不受 allow 约束。重复调用守卫、执行、观察截断依旧生效。
	execInternal := func(tool, method string, args map[string]interface{}, thought string) string {
		action := tool + "." + method
		record := func(obs string) string {
			steps = append(steps, map[string]interface{}{"thought": thought, "action": action, "observation": obs})
			return obs
		}
		key := action + " " + stableJSON(args)
		if seen[key] {
			return record("检测到重复调用相同工具与参数，疑似陷入循环，已停止该步")
		}
		seen[key] = true
		entry, err := buildActionEntry(tool, method, args)
		if err != nil {
			return record("构建工具调用失败：" + err.Error())
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
		obs = truncateObservation(obs, 3000)
		usedTools = appendUnique(usedTools, action)
		return record(obs)
	}

	// parseToolCallArgs 解析一条 tool_call 的 JSON 参数（容错：非法 JSON 也返回空 map 不崩）。
	parseToolCallArgs := func(s string) map[string]interface{} {
		args := map[string]interface{}{}
		if s := strings.TrimSpace(s); s != "" {
			if err := json.Unmarshal([]byte(s), &args); err != nil {
				args = map[string]interface{}{}
			}
		}
		if args == nil {
			args = map[string]interface{}{}
		}
		return args
	}

	// runMetaRetrieval 执行检索类元工具（find / describe / registry），返回要回灌的 Observation。
	// 复用 execute 的通道但跳过 allow：元工具是发现面，元模式下恒可用。
	runMetaRetrieval := func(c ToolCall, orig, thought string) string {
		args := parseToolCallArgs(c.Arguments)
		switch orig {
		case "ext.ai.find":
			return execInternal("ext.ai", "find", args, thought)
		case "ext.ai.describe":
			return execInternal("ext.ai", "describe", args, thought)
		case "ext.registry.tools":
			return execInternal("ext.registry", "tools", args, thought)
		}
		return "未知的元工具：" + orig
	}

	// runMetaInvoke 执行 ext.ai.invoke：从参数解出真正要调的能力与方法，再走 execute（受 allow 与预算约束）。
	runMetaInvoke := func(c ToolCall, thought string) string {
		args := parseToolCallArgs(c.Arguments)
		tool, _ := args["capability"].(string)
		method, _ := args["method"].(string)
		if tool == "" || method == "" {
			obs := "ext.ai.invoke 需要 capability 与 method（形如 {\"capability\":\"db\",\"method\":\"query\"}）"
			steps = append(steps, map[string]interface{}{"thought": thought, "action": "ext.ai.invoke", "observation": obs})
			return obs
		}
		callArgs, _ := args["args"].(map[string]interface{})
		if callArgs == nil {
			callArgs = map[string]interface{}{}
		}
		return execute(tool, method, callArgs, thought)
	}

	// execSteps 是「真正执行了一次能力调用」的计数；maxRounds 是 LLM 轮次的上限（防死循环）。
	// 两者在 flat 模式重合（每轮调用都算一步、且 MaxSteps 即轮次上限）；在 meta 模式分开：
	// find/describe/registry 是「检索步」、不计入预算，只有 invoke 才算一步，故轮次上限放宽松。
	execSteps := 0
	maxRounds := opts.MaxSteps
	if metaMode {
		maxRounds = opts.MaxSteps*3 + 5
	}
	for i := 0; i < maxRounds; i++ {
		// 上下文预算：每轮开工前压一次。不压的话长任务会在第 4、5 步撞上端点的上下文
		// 窗口，报出来的却是「LLM 调用失败」，看不出根因是上下文——最难的那种 bug。
		msgs = compactReactMessages(msgs)
		req := LLMRequest{
			Messages:    msgs,
			Provider:    opts.Provider,
			Model:       opts.Model,
			BaseURL:     opts.BaseURL,
			APIKey:      opts.APIKey,
			Temperature: opts.Temperature,
		}
		// LLM 调用走恢复包装（REQ-REACT-RECOVERY）：provider 侧瞬时错误机械重试（L0）；
		// 端点不吃工具声明时换通道降级为文本协议（L1.5）。降级是换通道、不是失败，
		// 所以不返回错误，只把工具面切掉、重建提示词，下一轮用新通道重问。
		resp, calls, degraded, err := a.completeReactStep(ctx, req, tools, board, !metaMode)
		if err != nil {
			return nil, err
		}
		if degraded {
			tools = nil
			msgs[0]["content"] = a.reactSystemPrompt(allow, opts.MaxSteps, false, false)
			continue
		}

		// Final Answer 判定必须**让位于**工具调用：模型这一轮还带着 tool_calls，说明它
		// 自己也没看到工具结果，正文里那句「Final Answer」只是计划或口误——照它收尾就
		// 等于让循环在第一次思考时结束，答案里一个工具都没真跑过。
		//
		// 历史缺陷（2026-10-01 修）：旧代码先判 Final Answer，且用的正则
		// `(?i)Final Answer\s*:\s*([\s\S]*)` 大小写不敏感 + 贪婪到结尾。模型在 Thought 里
		// 写一句「下一步我会输出 Final Answer:」就当场被判收尾，整个循环提前退出。
		if len(calls) == 0 {
			if fa, ok := extractFinalAnswer(resp); ok {
				finalAnswer = strings.TrimSpace(fa)
				steps = append(steps, map[string]interface{}{"thought": trimThought(resp), "action": nil, "observation": finalAnswer})
				break
			}
		}

		// 原生 tool_calls 优先：模型按协议给出的调用意图，比从自由文本里抠 JSON 可靠得多——
		// 基线 B-1（格式不遵守白扔一步）与 B-3（编造工具名只换回一段 Lua 栈错误）都出在老路径上。
		// 有 tool_calls 就不再解析正文。
		if len(calls) > 0 {
			// 模型一轮可以给多个调用（tool_calls 本就是数组，并行调用是协议允许的）。
			// 旧实现只执行 calls[0]，其余**静默丢弃**：模型以为调了、实际没调，下一轮拿不到
			// 任何结果，只能原样重发，直到撞上重复调用守卫被判「疑似陷入循环」。
			// 每个调用都必须有一条同 id 的 tool 消息回灌，少一条端点就判请求非法。
			appendAssistant(resp, calls)
			invokedThisRound := false
			thought := trimThought(resp)
			gapsBefore := len(board.gaps)
			for _, c := range calls {
				orig := toolByName[c.Name]
				switch {
				case metaMode && orig == "ext.ai.invoke":
					// invoke 是真正的能力调用：计为一步、受 allow 与预算双重约束。
					if execSteps >= opts.MaxSteps {
						msgs = append(msgs, map[string]any{"role": "tool", "tool_call_id": c.ID,
							"content": fmt.Sprintf("已达到最大调用步数 %d，请直接给出 Final Answer（不再执行工具调用）。", opts.MaxSteps)})
						invokedThisRound = true
						continue
					}
					invokedThisRound = true
					msgs = append(msgs, map[string]any{"role": "tool", "tool_call_id": c.ID, "content": runMetaInvoke(c, thought)})
				case metaMode && isMetaRetrieval(orig):
					// find / describe / registry 是「检索步」：不计入步数预算，绕过 allow 守卫
					// （它们是发现机制本身，永远可用），只复用重复调用守卫避免死循环。
					msgs = append(msgs, map[string]any{"role": "tool", "tool_call_id": c.ID, "content": runMetaRetrieval(c, orig, thought)})
				default:
					// flat 模式的真实能力调用（或文本兜底路径），按原逻辑走 execute / runCall。
					invokedThisRound = true
					msgs = append(msgs, map[string]any{"role": "tool", "tool_call_id": c.ID, "content": runCall(c, thought)})
				}
			}
			if invokedThisRound {
				execSteps++
			}
			// 本轮新产生的缺口要立刻让模型知道：不知道缺口存在的「带缺失作答」只是自欺。
			if len(board.gaps) > gapsBefore {
				msgs = append(msgs, map[string]any{"role": "user", "content": board.gapNotice()})
			}
			// 末步收敛引导：不提醒的话模型常在最后一步还要再调一次工具，于是步数用尽、
			// 拿不到 Final Answer，用户看到的是「（达到最大步数 N，未给出 Final Answer）」。
			if remaining := opts.MaxSteps - execSteps; remaining <= 1 {
				msgs = append(msgs, map[string]any{"role": "user",
					"content": "只剩最后 1 步：请直接给出 Final Answer（信息不足就说明缺什么、已完成了哪些）。"})
			}
			continue
		}

		// 文本兜底：模型没给 tool_calls（不支持 function calling，或这轮只想说话）时沿用老路径。
		// 这里没有 tool_calls，回灌就用普通 user 消息——不带 tool_call_id 才合法。
		appendAssistant(resp, nil)
		// 写了「Final Answer:」却没内容：明确要它补上。落到下面的兜底会回灌「既未输出合法
		// Action 也未输出 Final Answer」——对它来说这句话是错的（它明明写了），只能瞎重试。
		if reFinalAny.MatchString(resp) {
			obs := "你写了 Final Answer 但没给出内容，请把答案补在 Final Answer 后面。"
			steps = append(steps, map[string]interface{}{"thought": trimThought(resp), "action": nil, "observation": obs})
			msgs = append(msgs, map[string]any{"role": "user", "content": "Observation: " + obs})
			continue
		}
		tool, method, args, ok := parseReactAction(resp)
		if !ok {
			obs := "你本轮既未输出合法 Action（```json {...} ```）也未输出 Final Answer，请严格按格式继续。"
			steps = append(steps, map[string]interface{}{"thought": trimThought(resp), "action": nil, "observation": obs})
			msgs = append(msgs, map[string]any{"role": "user", "content": "Observation: " + obs})
			continue
		}
		gapsBefore := len(board.gaps)
		obs := execute(tool, method, args, trimThought(resp))
		// 同原生路径：本轮新产生的缺口立刻同步给模型（回答时才第一次知道就太晚了）。
		if len(board.gaps) > gapsBefore {
			msgs = append(msgs, map[string]any{"role": "user", "content": board.gapNotice()})
		}
		msgs = append(msgs, map[string]any{"role": "user", "content": "Observation: " + obs})
	}

	// 收尾补全：最后以失败收尾的动作统一转成缺口（finalizeGaps 的理由见其注释）。
	// 必须在兜底答案拼装之前——「步数耗尽 + 未能获取的数据」那段要含补全后的缺口。
	board.finalizeGaps()
	if finalAnswer == "" {
		finalAnswer = fmt.Sprintf("（达到最大步数 %d，未给出 Final Answer）", opts.MaxSteps)
		// 带缺失作答：步数耗尽时若有缺口，答案必须带上——静默残缺比明说缺失坏得多。
		if len(board.gaps) > 0 {
			finalAnswer += "\n\n未能获取的数据/能力：\n" + board.gapNotice()
		}
	}
	// gaps / recovery_log 始终存在（空切片而非缺字段）：调用方 `if r.gaps then` 与
	// `#r.gaps == 0` 都成立，不必先判存在性。gaps 是「带缺失作答」的事实来源，
	// recovery_log 是 L0/L1.5 恢复流水（ReAct 逐步可视的补强面）。
	result := map[string]interface{}{
		"answer":       finalAnswer,
		"steps":        steps,
		"used_tools":   usedTools,
		"gaps":         board.gaps,
		"recovery_log": board.recovery,
	}
	return result, nil
}

// reactContextBudget 是 react 消息序列的软上限（字符数，含 system 提示与全部观察）。
//
// 按字符数而不是 token 数：内核不该为一个提示词去引分词器，粗粒度够用；真要精确就得按
// 模型算，那是 ai_llm.go 的职责。48000 字符约合中文 48k token，够 max_steps 跑完一轮。
const reactContextBudget = 48000

// compactReactMessages 超预算时把**最早的观察**折叠成一行摘要；前两条（system + 任务）永不折叠。
//
// 折叠而非丢弃：删条目会破坏「assistant(tool_calls) ↔ tool(tool_call_id)」的配对，端点直接
// 判请求非法。折叠只换 content，条数与 id 都不动，协议层面始终自洽。
//
// 折叠而非截断：截断把一条观察砍成半句，模型读到残缺 JSON 比没有这条更容易出错。
// 折叠保留「第几条、原本多长」，它要细节可以重新调一次工具——这是可恢复的，截断不是。
func compactReactMessages(msgs []map[string]any) []map[string]any {
	if len(msgs) <= 2 {
		return msgs
	}
	total := 0
	for _, m := range msgs {
		total += len(fmt.Sprint(m["content"]))
	}
	if total <= reactContextBudget {
		return msgs
	}
	folded := 0
	// 从最早开始折：早期观察被后续步骤取代的概率最高。
	for i := 2; i < len(msgs) && total > reactContextBudget; i++ {
		role, _ := msgs[i]["role"].(string)
		if role != "tool" && role != "user" {
			continue
		}
		body, _ := msgs[i]["content"].(string)
		if len(body) <= 32 {
			continue // 太短，折了也省不出什么
		}
		folded++
		summary := fmt.Sprintf("（第 %d 条观察已折叠，原 %d 字符；需要细节请重新调用该工具）", folded, len(body))
		msgs[i]["content"] = summary
		total += len(summary) - len(body)
	}
	return msgs
}

// ReactMethod 暴露给 Lua ctx.ext.ai.react：解析参数后跑 React 循环。
//
// callerCaps 是**调用方 Tool 自己声明的 capability**（由 Lua 绑定从 cap.Context 取来），
// 是「没显式给 allow」时的默认工具集。这一层是能力边界的落点：不收口的话，一个只声明
// `capabilities: [ext.ai]` 的 agent Tool 会拿到全部门禁原语，等于把凭据通道交给了模型。
func (a *AIAgent) ReactMethod(args map[string]interface{}, callerCaps ...string) (interface{}, error) {
	task, _ := args["task"].(string)
	opts := ReactOpts{}
	// toFloat 而不是直接断言 float64：Lua 侧的整数字面量（max_steps = 8）经 fromLuaValue
	// 出来是 **int64**（lua.go 的整数收敛规则），只认 float64 会让这两个参数静默失效——
	// `max_steps = 8` 一直按默认 6 步跑，肉眼看不出来。
	if v, ok := toFloat(args["max_steps"]); ok && int(v) > 0 {
		opts.MaxSteps = int(v)
	}
	if s, ok := args["mode"].(string); ok && s != "" {
		opts.Mode = s
	}
	// 显式给 allow 时必须是调用方自己声明的子集——否则「给个 allow 绕过门禁」就成了后门。
	explicit := toStringSlice(args["allow"])
	if len(explicit) > 0 && len(callerCaps) > 0 {
		for _, name := range explicit {
			// allow 项可以是「能力」（ext.redis）或「能力.方法」（ext.redis.set）：
			// 子集校验比的是**能力部分**——在自己能力内再收窄到方法不是越权。
			capName := name
			if !a.knownCapability(name) {
				if c, _, ok := splitCapMethod(name); ok {
					capName = c
				}
			}
			if !containsString(callerCaps, capName) {
				return nil, fmt.Errorf("react 的 allow 含 %q，但本 Tool 只声明了 %v——allow 不能超出自己的 capability；"+
					"要用更多能力，请在本 Tool 的 capabilities 里声明", name, callerCaps)
			}
		}
	}
	if len(explicit) > 0 {
		opts.Allow = explicit
		opts.AllowSet = true
	} else {
		// 缺省 = 调用方声明的那一份；一份都没声明就一个工具都不给（而不是退回全目录）。
		// 这里过一遍 knownOnly：声明了但当前没装配的能力（扩展没启用）不该让整个 react
		// 报错——那是定义层面的问题，注册期已经校验过合法性。
		opts.Allow = a.knownOnly(callerCaps)
		opts.AllowSet = true
	}
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
	if v, ok := toFloat(args["temperature"]); ok && v > 0 {
		opts.Temperature = v
	}
	return a.React(context.Background(), task, opts)
}

// reactSystemPrompt 拼装 ReAct 的系统提示：可用工具（来自能力目录，可经 allow 过滤）+ 输出格式 + 规则。
// useTools 表示本轮会随请求下发 function 声明（原生 function calling）。为 true 时不再要求模型
// 手写 ```json 围栏——协议已经给了调用通道，再要求它手打一遍 JSON 只会两头打架（基线 B-1 的
// 「格式不遵守白扔一步」就是这么来的）。
func (a *AIAgent) reactSystemPrompt(al reactAllow, maxSteps int, useTools bool, meta bool) string {
	if meta {
		return a.reactMetaSystemPrompt(maxSteps)
	}
	// useTools=true 时协议侧已随请求下发完整 function 声明，提示词里只给「能力 → 方法名」
	// 索引（brief），不把参数再抄一遍——同一份信息付两遍钱，且每轮都要重发。
	tools := a.reactToolsText(al, useTools)
	var b strings.Builder
	b.WriteString("你是一个运行在 Gear 引擎内的 ReAct（推理 + 行动）智能体。你可以通过调用「内置工具」逐步推理与行动，直到能回答用户任务。\n\n")
	if useTools {
		b.WriteString("## 可用能力（方法名索引，参数见工具声明；只能使用下列工具，不要编造）\n")
	} else {
		b.WriteString("## 可用工具（只能使用下列工具，不要编造）\n")
	}
	b.WriteString(tools)
	b.WriteString("\n\n## 每轮输出格式（严格遵守）\n")
	if useTools {
		b.WriteString("先写思考，然后用**工具调用**发起一次调用（不要把参数手写成 JSON 文本）：\n\n")
		b.WriteString("Thought: <这一步你在想什么>\n")
		b.WriteString("（随后发起一次工具调用）\n")
	} else {
		b.WriteString("先写思考，再决定「调用一个工具」或「给出最终答案」：\n\n")
		b.WriteString("Thought: <这一步你在想什么>\n")
		b.WriteString("Action:\n```json\n{\"tool\": \"http.Request\", \"args\": {\"method\": \"GET\", \"url\": \"https://example.com\", \"body\": null, \"headers\": {}}}\n```\n")
	}
	b.WriteString("当你认为信息已足够、可以回答用户时，输出：\n\n")
	b.WriteString("Thought: <总结>\n")
	b.WriteString("Final Answer: <给用户的完整回答>\n\n")
	b.WriteString("## 规则\n")
	if useTools {
		b.WriteString("- 每轮只能调用一个工具，用工具调用发起；参数是结构化的，不要手写 JSON 文本。\n")
		b.WriteString("- 工具名形如 http_Request / ext_ai_complete：下划线处原本是「.」（能力名与方法名的分隔），字符集所限才压平。\n")
	} else {
		b.WriteString("- 每轮只能调用一个工具。Action 必须是合法 JSON（放在 ```json ... ``` 围栏内）。\n")
		b.WriteString("- tool 字段为「<能力名>.<方法>」：内置原语如 http.Request / db.query / secret.get；扩展如 ext.ai.complete。\n")
	}
	b.WriteString("- 内置原语按「位置参数」调用，args 的键对应工具声明里的参数名（可选参数缺省传 null）。\n")
	b.WriteString("- 扩展（ext.*）按「单个具名 table」调用，args 整体作为 { ... } 传入 ctx.ext.<name>.<method>({...})。\n")
	b.WriteString("- Observation（工具返回）会由系统填回作为下一步输入；不要自己编造 Observation。\n")
	b.WriteString("- 工具失败时会回灌「失败信号」（类型/范围/原因/建议）：按信号修正调用，不要原样重发同一调用；已被判为缺口的数据，在 Final Answer 里如实说明，不得编造。\n")
	b.WriteString(fmt.Sprintf("- 最多 %d 步。若卡住，请尽快给出 Final Answer。\n", maxSteps))
	return b.String()
}

// reactMetaSystemPrompt 是元工具模式的系统提示：工具面恒定 4 个，提示词里把「怎么用这 4 个工具」
// 说清楚，而不是列能力目录（那样又回到全量平铺的老路）。核心是一句话工作流：find → describe → invoke。
func (a *AIAgent) reactMetaSystemPrompt(maxSteps int) string {
	var b strings.Builder
	b.WriteString("你是一个运行在 Gear 引擎内的 ReAct（推理 + 行动）智能体。Gear 的能力目录有上百个方法，但你不一次全看，而是**按需检索**——你手上只有 4 个工具：\n\n")
	b.WriteString("## 你的工具（恒定 4 个，不要编造别的）\n")
	b.WriteString("- `ext_ai_find(query, domain?, limit?)`：按自然语言检索能力目录里的方法（BM25）。不确定能做什么、或不知道方法名叫什么时先调它。返回方法全名 + 一句话说明 + 参数名。\n")
	b.WriteString("- `ext_ai_describe(id)`：拿到方法全名后，查它的完整参数契约（类型 / 必填 / 枚举）。调 invoke 之前务必先看清楚参数怎么填。\n")
	b.WriteString("- `ext_ai_invoke(capability, method, args)`：真正执行一个方法。capability 是能力名（如 db / http / ext.redis），method 是方法名（如 query / set / encrypt），args 是 describe 给的参数对象。\n")
	b.WriteString("- `ext_registry_tools(prefix?, limit?)`：列出用户在 registry 里定义的业务 Tool / Flow，需要调用用户自建工具时用它发现。\n\n")
	b.WriteString("## 工作流（严格遵守）\n")
	b.WriteString("1. 先用 `ext_ai_find` 找到匹配任务的方法；一次找不到就换个说法或加 domain 再找。\n")
	b.WriteString("2. 对要调用的每个方法，先 `ext_ai_describe` 看参数，再 `ext_ai_invoke` 执行——**不要凭猜测填参数**，参数结构以 describe 为准。\n")
	b.WriteString("3. 信息足够后输出 Final Answer。find / describe / registry 是「检索步」、不计入下面的步数预算；只有 invoke 才算一步。\n\n")
	b.WriteString("## 每轮输出格式（严格遵守）\n")
	b.WriteString("先写思考，然后用**工具调用**发起调用（不要把参数手写成 JSON 文本）：\n\n")
	b.WriteString("Thought: <这一步你在想什么>\n")
	b.WriteString("（随后发起一次工具调用）\n\n")
	b.WriteString("当你认为信息已足够、可以回答用户时，输出：\n\n")
	b.WriteString("Thought: <总结>\n")
	b.WriteString("Final Answer: <给用户的完整回答>\n\n")
	b.WriteString("## 规则\n")
	b.WriteString("- 工具名形如 ext_ai_find：下划线处原本是「.」（ext.ai.find 压平而来），字符集所限。\n")
	b.WriteString("- 每轮只能调用一个工具，用工具调用发起；参数是结构化的，不要手写 JSON 文本。\n")
	b.WriteString("- 不要编造方法名或参数；拿不准就 find / describe，找不到就说明做不到。\n")
	b.WriteString("- Observation（工具返回）会由系统填回作为下一步输入；不要自己编造 Observation。\n")
	b.WriteString("- 调用失败时会回灌「失败信号」（类型/范围/原因/建议）：按信号修正调用，不要原样重发同一调用；已被判为缺口的数据，在 Final Answer 里如实说明，不得编造。\n")
	b.WriteString(fmt.Sprintf("- 最多 %d 步调用（invoke 计数；find/describe 不计）。若卡住，请尽快给出 Final Answer。\n", maxSteps))
	return b.String()
}

// brief=true 只列「能力 → 方法名」索引，不含参数细节。协议侧已经下发了完整 function 声明，
// 再在提示词里抄一遍参数等于同一份信息付两遍钱——全目录实测 115 个方法，双重下发时单轮
// 光工具定义就 10-15k token，且每一轮都重发。brief 模式下模型先看索引定位能力，参数看
// 工具声明，两边互补而不是重复。
//
// brief=false（没有 function calling 可依赖的文本兜底路径）必须给全：那时模型只能从这段
// 文本知道有什么工具、每个方法收什么参数。
func (a *AIAgent) reactToolsText(al reactAllow, brief bool) string {
	// allowedMethods 按序取出该能力下被允许的方法名。
	allowedMethods := func(capName string, ms map[string]MethodSchema) []string {
		out := []string{}
		for _, m := range sortedKeys(ms) {
			if al.allows(capName, m) {
				out = append(out, m)
			}
		}
		return out
	}
	var sb strings.Builder
	// 与 capabilitiesText 同源（allBuiltinPrimitives 单一来源），不再写死 6+1 个名字——
	// 这样 subscribe / 纯函数 time/template/crypto/id/validate 都会自然进入 ReAct 工具清单，
	// 且裸函数原语（call）按 ctx.call(...) 形态渲染（见 bareFunctionPrimitives）。
	for _, p := range allBuiltinPrimitives() {
		ms, ok := builtinCapabilitySchemas[p]
		if !ok {
			continue
		}
		names := allowedMethods(p, ms)
		if len(names) == 0 {
			continue
		}
		if brief {
			sb.WriteString(fmt.Sprintf("- %s: %s\n", p, methodIndex(names)))
			continue
		}
		for _, m := range names {
			sch := ms[m]
			sb.WriteString(fmt.Sprintf("- %s: %s\n", a.callSignature(p, m, sch), sch.Description))
		}
	}
	if len(a.extCaps) > 0 {
		// ext 名来自 map，遍历顺序不稳定——排序后再渲染，保证同一份输入总是同一份提示词
		// （顺序不稳会让「模型看到的工具顺序」在不同进程里不一样，问题难复现）。
		extNames := make([]string, 0, len(a.extCaps))
		for name := range a.extCaps {
			extNames = append(extNames, name)
		}
		sort.Strings(extNames)
		for _, name := range extNames {
			names := allowedMethods(name, a.extCaps[name])
			if len(names) == 0 {
				continue
			}
			if brief {
				sb.WriteString(fmt.Sprintf("- %s: %s\n", name, methodIndex(names)))
				continue
			}
			sb.WriteString("# ctx.ext.<name>.<method> 只接受「单个具名 table」参数：\n")
			for _, m := range names {
				sch := a.extCaps[name][m]
				sb.WriteString(fmt.Sprintf("- %s: %s\n", a.callSignature(name, m, sch), sch.Description))
			}
		}
	}
	if brief {
		return "（下面只列能力与方法名，参数以工具声明为准，不要编造）\n" + sb.String()
	}
	return sb.String()
}

// reactMethodIndexLimit 是 brief 模式下单个能力最多列出的方法名个数。
const reactMethodIndexLimit = 8

// methodIndex 把方法名压成一行索引，超出上限只列前几个并**如实给出总数**——
// 与 truncateObservation 同一口径：截断必须告知总数，不能让模型按数得着的条目下结论。
func methodIndex(ms []string) string {
	if len(ms) <= reactMethodIndexLimit {
		return strings.Join(ms, ", ")
	}
	return strings.Join(ms[:reactMethodIndexLimit], ", ") +
		fmt.Sprintf(" …等共 %d 个方法", len(ms))
}

// reactToolSpecs 生成给模型的工具声明（原生 function calling 用），并带回「压平名 → 原名」的反查表。
//
// 为什么要压平：OpenAI 的 function name 只接受 [A-Za-z0-9_-]，而 Gear 的工具名带点
// （http.Request / ext.ai.complete）。压平不可逆，所以反查表必须一起返回——模型回的是压平名，
// 执行前要还原成「<能力>.<方法>」。
//
// 与 reactToolsText 同源（都是能力目录的另一种渲染），两边都受 allow 过滤，保证
// 「prompt 里写了的工具」与「协议里声明的工具」是同一批——不然模型会调到一个 prompt 里没有的。
// reactMetaToolSpecs 是「元工具模式」的工具面：恒定 4 个工具（find / describe / invoke /
// registry），与能力目录规模（实测 28 能力 / 115 方法）彻底解耦。模型先 find 检索、describe
// 取 schema，再 invoke 直调真正的方法；registry 工具用于发现调用方在 registry 里定义的业务 Tool。
//
// 返回 byName：压平名 → orig（压平前的全名）。orig 在主循环里用来区分「这到底是哪种元工具」。
func reactMetaToolSpecs() ([]ToolSpec, map[string]string) {
	str := func(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
	num := func(desc string) map[string]any { return map[string]any{"type": "number", "description": desc} }
	obj := func(desc string) map[string]any { return map[string]any{"type": "object", "description": desc} }
	specs := []ToolSpec{
		{
			Name:        "ext_ai_find",
			Description: "在能力目录里按语义检索可用方法（BM25）。当你不确定该调哪个能力/方法、或想知道某件事能不能做时先调它。",
			Parameters: map[string]any{"type": "object", "properties": map[string]any{
				"query":  str("自然语言检索词，如「查询数据库」「加密一段文本」「发邮件」"),
				"domain": str("（可选）限定到某个能力域，如 db / ext.redis / crypto，缩小范围"),
				"limit":  num("（可选）返回几条，默认 5"),
			}, "required": []string{"query"}},
		},
		{
			Name:        "ext_ai_describe",
			Description: "返回某个方法的完整参数契约（参数名/类型/是否必填/枚举）。先用 find 拿到方法全名（如 db.query），再 describe 看怎么填参数。",
			Parameters: map[string]any{"type": "object", "properties": map[string]any{
				"id": str("方法全名，如 http.Request / ext.redis.set / db.query"),
			}, "required": []string{"id"}},
		},
		{
			Name:        "ext_ai_invoke",
			Description: "真正执行一个能力方法。capability 是能力名（如 db / http / ext.redis），method 是方法名（如 query / set / encrypt），args 是该方法的参数对象（结构见 describe 的返回）。",
			Parameters: map[string]any{"type": "object", "properties": map[string]any{
				"capability": str("能力名，如 db / http / ext.redis"),
				"method":     str("方法名，如 query / set / Request"),
				"args":       obj("该方法所需的参数对象（结构见 ext_ai_describe 的返回）"),
			}, "required": []string{"capability", "method"}},
		},
		{
			Name:        "ext_registry_tools",
			Description: "列出调用方在 registry 里定义的业务 Tool / Flow（id/name/description）。当你需要调用用户自建的工具时用它发现。",
			Parameters: map[string]any{"type": "object", "properties": map[string]any{
				"prefix": str("（可选）按 id 前缀过滤"),
				"limit":  num("（可选）返回几条，默认 10"),
			}},
		},
	}
	byName := map[string]string{
		"ext_ai_find":        "ext.ai.find",
		"ext_ai_describe":    "ext.ai.describe",
		"ext_ai_invoke":      "ext.ai.invoke",
		"ext_registry_tools": "ext.registry.tools",
	}
	return specs, byName
}

// reactToolSpecs 生成给模型的工具声明（原生 function calling 用），并带回「压平名 → 原名」的反查表。
//
// 与 reactToolsText 同源（都是能力目录的另一种渲染），两边都受 allow 过滤，保证
// 「prompt 里写了的工具」与「协议里声明的工具」是同一批——不然模型会调到一个 prompt 里没有的。
func (a *AIAgent) reactToolSpecs(al reactAllow, meta bool) ([]ToolSpec, map[string]string) {
	if meta {
		return reactMetaToolSpecs()
	}
	specs := []ToolSpec{}
	byName := map[string]string{}
	add := func(orig string, sch MethodSchema) {
		flat := SanitizeToolName(orig)
		if _, dup := byName[flat]; dup {
			return // 压平后撞名：保留先登记的，避免一个名字指向两个不同能力
		}
		desc := sch.Description
		if desc == "" {
			desc = orig + "（能力目录未给描述）"
		}
		specs = append(specs, ToolSpec{Name: flat, Description: desc, Parameters: methodSchemaToJSON(sch)})
		byName[flat] = orig
	}
	for _, p := range allBuiltinPrimitives() {
		ms, ok := builtinCapabilitySchemas[p]
		if !ok {
			continue
		}
		for _, m := range sortedKeys(ms) {
			if !al.allows(p, m) {
				continue
			}
			add(p+"."+m, ms[m])
		}
	}
	// ext 名来自 map，遍历顺序不稳定——排序后再进数组，保证 tools 顺序可复现
	// （顺序不稳会让「模型第一轮看到的工具」在不同进程里不一样，难复现问题）。
	extNames := make([]string, 0, len(a.extCaps))
	for name := range a.extCaps {
		extNames = append(extNames, name)
	}
	sort.Strings(extNames)
	for _, name := range extNames {
		for _, m := range sortedKeys(a.extCaps[name]) {
			if !al.allows(name, m) {
				continue
			}
			add(name+"."+m, a.extCaps[name][m])
		}
	}
	return specs, byName
}

// schemaMaxDepth 是 methodSchemaToJSON 递归下钻的层数上限（防自引用死循环 / 超深嵌套）。
const schemaMaxDepth = 6

// methodSchemaToJSON 把方法级参数 schema 转成 JSON Schema（function calling 的 parameters）。
// 顶层是 object（properties = 具名字段），并递归展开嵌套 object（Properties）/ array（Items）——
// 这样模型在字段级就能拿到约束，避免「传错子字段 → 拖到 Lua 运行期才炸」（REQ-AI-SCHEMA-NESTED）。
// 只转能力目录已声明的东西；自由 map 型参数不画蛇添足（见 ParamSchema 注释）。
func methodSchemaToJSON(ms MethodSchema) map[string]any {
	return schemaObjectToJSON(ms.Parameters, 0)
}

// schemaObjectToJSON 把一组具名字段编成 JSON Schema 的 object（type + properties + required 包装）。
func schemaObjectToJSON(params map[string]ParamSchema, depth int) map[string]any {
	out := map[string]any{"type": "object", "properties": fieldsToJSON(params, depth)}
	if req := requiredNames(params); len(req) > 0 {
		out["required"] = req
	}
	return out
}

// fieldsToJSON 把具名字段编成 map[字段名]字段schema——即 JSON Schema 里 "properties" 的**值本身**
// （纯字段 map，不带 type/required 外层）。嵌套 object 参数的子字段就直接挂在这里，不再整段重包一层 object。
func fieldsToJSON(params map[string]ParamSchema, depth int) map[string]any {
	props := map[string]any{}
	for name, p := range params {
		props[name] = paramSchemaToJSON(p, depth)
	}
	return props
}

func requiredNames(params map[string]ParamSchema) []string {
	req := []string{}
	for name, p := range params {
		if p.Required {
			req = append(req, name)
		}
	}
	return req
}

// paramSchemaToJSON 编单个参数的 JSON Schema，递归处理嵌套 object（Properties）与 array（Items）。
func paramSchemaToJSON(p ParamSchema, depth int) map[string]any {
	prop := map[string]any{}
	if p.Type != "" {
		prop["type"] = p.Type
	}
	if p.Description != "" {
		prop["description"] = p.Description
	}
	if len(p.Enum) > 0 {
		prop["enum"] = p.Enum
	}
	if depth < schemaMaxDepth && len(p.Properties) > 0 {
		prop["properties"] = fieldsToJSON(p.Properties, depth+1)
		if req := requiredNames(p.Properties); len(req) > 0 {
			prop["required"] = req
		}
	}
	if depth < schemaMaxDepth && p.Items != nil {
		prop["items"] = paramSchemaToJSON(*p.Items, depth+1)
	}
	return prop
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
// 纯函数原子（time/template/crypto/id/validate）与扩展（ext.*）都走单具名 table，不在此列。
var builtinParamOrder = map[string]map[string][]string{
	"http":      {"Request": {"method", "url", "body", "headers"}},
	"call":      {"Call": {"target", "input"}},
	"db":        {"query": {"table"}, "exec": {"table"}, "batch": {"table"}, "ping": {"table"}},
	"secret":    {"get": {"name"}},
	"event":     {"notify": {"name", "payload"}, "wait": {"name"}},
	"subscribe": {"start": {"table"}, "stop": {"handle"}},
	// config 全部走单具名 table（与 db.* 同形态）：ctx.config.get({key=...})。
	"config": {"get": {"table"}, "set": {"table"}, "delete": {"table"}, "list": {"table"}},
}

// buildActionEntry 把一次解析出的工具调用编成一段 Lua 入口（复用真实 Tool 的 ctx.* 通道）：
//   - 内置原语：位置参数调用 ctx.<tool>.<method>(a, b, ...)，缺失可选参数传 nil（Lua 绑定按 GetTop 判缺省）；
//     裸函数绑定的原语（call）用 ctx.<tool>(a, b, ...)。
//   - 纯函数原子（time/template/crypto/id/validate）：单具名 table 调用 ctx.<tool>.<method>({ ... })（与 ctx.ext.* 同款）。
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
	case json.Number:
		// json.Number 底层是 string，落进 default 会被 json.Marshal 再包一层双引号编成
		// Lua **字符串**（"3600"），而调用方给的是数字。只有开了 UseNumber 的解码器才产出它——
		// 今天 MCP/REST 两条路都不开（实测解出 float64），但换解码器就会静默失真，故显式兜住。
		if i, err := x.Int64(); err == nil {
			return strconv.FormatInt(i, 10)
		}
		if f, err := x.Float64(); err == nil {
			return luaLiteral(f)
		}
		// 不是合法数字：如实当字符串交给下游，不猜、不静默丢弃。
		return luaQuote(x.String())
	default:
		// switch 未列出的**整数**种类（int8/int16/uint*/uintptr）同样必须编成数字字面量，
		// 否则落进下面的 marshal 兜底会被包成字符串，Lua 侧拿到 string 而不是 number。
		if lit, ok := unlistedIntegerLiteral(x); ok {
			return lit
		}
		// 兜底：任何 JSON 可序列化的值（struct / []string / map[string]string / map[any]any …）
		// 先编成 JSON 再解回通用类型递归渲染，**而不是把它的 JSON 文本当字符串包起来**——
		// 后者是静默类型丢失：Lua 侧收到 string，报错要拖到调用深处才炸，甚至根本不炸。
		if b, err := json.Marshal(x); err == nil {
			var generic interface{}
			if err := json.Unmarshal(b, &generic); err == nil {
				return luaLiteral(generic)
			}
			return luaQuote(string(b))
		}
		// 表达不了的值一律编成 nil（不猜一个字面量出来，避免编出语法错的 Lua）。
		return "nil"
	}
}

// unlistedIntegerLiteral 只兜上面类型 switch **没有列出**的整数种类。
// 刻意不重复覆盖 int/int32/int64/float32/float64——同一类型有两条渲染路径必然漂移。
// 无符号一律用 FormatUint：uint64 超过 2^53 时经 JSON 会丢精度，那是静默数据损坏。
func unlistedIntegerLiteral(v interface{}) (string, bool) {
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Int8, reflect.Int16:
		return strconv.FormatInt(rv.Int(), 10), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return strconv.FormatUint(rv.Uint(), 10), true
	}
	return "", false
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
	// reFinal 的三处口径都是踩过坑才定下来的：
	//   - `(?:^|\n)` 要求 Final Answer 是**一行的开头内容**，而不是正文里随便冒出来的一句话；
	//   - 前置贪婪 `.*` 让匹配落在**最后一次**出现处——模型往往先在 Thought 里写
	//     「下一步我会输出 Final Answer: ...」，真正的答案在末尾；
	//   - 不再用一个捕获组吞到结尾（`[\s\S]*`），而是交给 extractFinalAnswer 判空，
	//     写了「Final Answer:」却不给内容不算收尾。
	// 历史正则 `(?i)Final Answer\s*:\s*([\s\S]*)` 三条全犯：大小写不敏感、不限位置、贪婪到结尾。
	reFinal = regexp.MustCompile(`(?s)(?i).*(?:^|\n)[ \t]*Final Answer[ \t]*:[ \t]*(.*)`)
	// reFinalAny 只判「提没提过 Final Answer」，不限定格式与内容。用来区分两种失败：
	// 写了标记但没内容 → 要它补答案；压根没写 → 才是格式不遵守。混成一句回灌会让模型
	// 读到自相矛盾的反馈（它明明写了），只能瞎重试。
	reFinalAny = regexp.MustCompile(`(?i)final\s*answer`)
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
	ans := strings.TrimSpace(m[1])
	if ans == "" {
		// 写了「Final Answer:」却没内容，等于没回答。按未收尾处理，让循环再走一步——
		// 收一个空答案比多跑一步更难查（用户只看到 answer 是空字符串，不知道卡在哪）。
		return "", false
	}
	return ans, true
}

// trimThought 取 LLM 输出中 Action: 之前的部分作为「思考」（无 Action 则取全文）。
func trimThought(resp string) string {
	if i := strings.Index(strings.ToLower(resp), "action:"); i >= 0 {
		return strings.TrimSpace(resp[:i])
	}
	return strings.TrimSpace(resp)
}

// toFloat 宽容地把数值型入参转成 float64。
// Lua → Go 的整数收敛在 lua.go 的 fromLuaValue 里（整数字面量出 int64），调用方不该
// 猜自己拿到的到底是哪个类型——猜错的表现是「参数写了但没生效」，最难查。
func toFloat(v interface{}) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case float32:
		return float64(t), true
	case int64:
		return float64(t), true
	case int32:
		return float64(t), true
	case int:
		return float64(t), true
	}
	return 0, false
}

// containsString 报告一个字符串是否在列表里（纯标量比较，不含 "ext.*" 通配——通配只在
// react 的工具名判定里才有意义）。
func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
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

// jsonArrayLen 尝试读出「字符串里顶层 JSON 数组的条数」。观察回灌给模型前会被序列化成
// JSON，而目录类返回值（ext.registry.tools 等）恰好是数组——条数是模型最需要的那个数字。
// 读出来写进截断提示，模型就不必去数一份被截断的半截清单（2026-09-27 实测：数出 12，真实 34）。
func jsonArrayLen(s string) (int, bool) {
	i := strings.Index(s, "[")
	if i < 0 {
		return 0, false
	}
	var arr []json.RawMessage
	if err := json.Unmarshal([]byte(s[i:]), &arr); err != nil {
		return 0, false
	}
	return len(arr), true
}

// truncateObservation 截断观察并**把「截掉了多少」如实写回给模型**。
//
// 只截断不给信息是这次实踩的坑：模型拿到带 `...(truncated)` 的目录后数出 12 条就回答「共 12 个
// Tool」（真实 34）。截断本身没错，错在截完之后模型无从知道自己看到的不是全部，只能凑一个
// 看起来合理的数字。所以提示里必须带**原文总长**（任何情况都能给）与**数组条数**（能解析出来时），
// 并告诉它分次查询的手段——`... 请你缩小范围` 如果没有参数可传，就是一句空话。
func truncateObservation(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	head := string(r[:n])
	if cnt, ok := jsonArrayLen(s); ok {
		return head + fmt.Sprintf("\n\n...(已截断：原文共 %d rune，以上只是开头。这是长度为 %d 的清单，"+
			"请按「共 %d 条」理解，不要按上面能数出的条目下结论——"+
			"换用 prefix/limit 分次查询可看到完整清单)", len(r), cnt, cnt)
	}
	return head + fmt.Sprintf("\n\n...(已截断：原文共 %d rune，以上只是开头)", len(r))
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
// 顺序与内容取 allBuiltinPrimitives 单一来源。
//
// **调用形态直接取 invokeFormFor**（不另判）：此前这里是「纯函数走 table、其余一律走位置参数」
// 的二分判据，于是 db.* / config.* / subscribe.start 被渲染成位置参数、call 被渲染成
// ctx.call.Call(...)——模型照抄编出的代码要到 Lua 运行期才炸（外部实测 2026-10-03 的头号误用来源）。
func (a *AIAgent) capabilitiesText() string {
	var sb strings.Builder
	for _, p := range allBuiltinPrimitives() {
		ms, ok := builtinCapabilitySchemas[p]
		if !ok {
			continue
		}
		for _, m := range sortedKeys(ms) {
			sch := ms[m]
			sb.WriteString(fmt.Sprintf("- %s: %s\n", a.callSignature(p, m, sch), sch.Description))
		}
	}
	if len(a.extCaps) > 0 {
		sb.WriteString("# ctx.ext.<name>.<method> 只接受「单个具名 table」参数（字段名见下，" +
			"按当前已注册 ext.* 实时列出，非写死；新增扩展会自动出现在这里）：\n")
		for name, ms := range a.extCaps {
			for m, sch := range ms {
				sb.WriteString(fmt.Sprintf("- %s: %s\n", a.callSignature(name, m, sch), sch.Description))
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

// callSignature 返回一个方法的**可复制调用签名**（如 `ctx.db.query({ driver = string, sql = string })`）。
//
// 判据来自 invokeFormFor（全能力唯一实现）。判不出来时**不给签名**，只留一个明确的占位——
// 编一个看着像真的形态比不编更坏：模型会照抄，而错误要到运行期才暴露。
func (a *AIAgent) callSignature(cap, method string, sch MethodSchema) string {
	if _, sig, ok := invokeFormFor(cap, method, a.extCaps); ok {
		return sig
	}
	return cap + "." + method + "（调用形态未登记，先用 gear_capabilities 查证后再写）: " + paramList(sch.Parameters)
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
// sortedKeys 取 map 的键并升序返回。泛型：方法目录（MethodSchema）与参数表（ParamSchema）
// 都要用——写成具体类型就得抄两份，抄两份必漂移。
func sortedKeys[T any](ms map[string]T) []string {
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
		// `?` 是「可选」标记（Lua 惯用法 name?）。必填参数**不加**标记——此前写反成
		// `if Required { req = "?" }`，会把必填项显示成可选，模型据此漏传参数导致调用失败。
		if !params[k].Required {
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
	"http": true, "call": true, "db": true, "secret": true, "event": true, "subscribe": true, "config": true,
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
	o.apply(&req)
	return o.base.Complete(ctx, req)
}

// CompleteStream 与 Complete 同款：先填 opts 的空字段，再转发给底层 LLM 的流式实现。
func (o overrideLLM) CompleteStream(ctx context.Context, req LLMRequest, onDelta func(content, reasoning string) error) error {
	o.apply(&req)
	return o.base.CompleteStream(ctx, req, onDelta)
}

// CompleteTools 同款转发：工具声明本身不改，只补请求里空着的模型参数。
func (o overrideLLM) CompleteTools(ctx context.Context, req LLMRequest, tools []ToolSpec) (ChatResult, error) {
	o.apply(&req)
	return o.base.CompleteTools(ctx, req, tools)
}

// apply 把 opts 的模型参数填进请求的空字段。抽出来是因为三个方法都要做同一件事——
// 复制三份的话，将来加一个参数就会漏改其中一份。
func (o overrideLLM) apply(req *LLMRequest) {
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
var aiMethodNames = []string{"complete", "catalog", "domains", "find", "describe", "validate", "generate", "capabilities", "react", "complete_stream"}

// aiSyncMethods 是 ext.ai 里「同步分发」的方法（不含回调式的 complete_stream）。
var aiSyncMethods = []string{"complete", "catalog", "domains", "find", "describe", "validate", "generate", "capabilities", "react"}

// callAIMethod 按方法名分发到 AIAgent 的稳定层方法（同步路径），供 Lua / go-host Provider 共用。
// callerCaps 是调用方 Tool 声明的 capability，只有 react 用它——见 ReactMethod。
func callAIMethod(a *AIAgent, method string, args map[string]interface{}, callerCaps ...string) (interface{}, error) {
	switch method {
	case "complete":
		return a.Complete(args)
	case "catalog":
		return a.Catalog(args)
	case "domains":
		return a.Domains(args)
	case "find":
		return a.Find(args)
	case "describe":
		return a.Describe(args)
	case "validate":
		return a.ValidateMethod(args)
	case "capabilities":
		return a.CapabilitiesMethod()
	case "generate":
		return a.GenerateMethod(args)
	case "react":
		return a.ReactMethod(args, callerCaps...)
	default:
		return nil, fmt.Errorf("ext.ai 不支持的方法 %q（仅 complete/catalog/domains/find/describe/validate/generate/capabilities/react/complete_stream）", method)
	}
}

// aiInProcessProvider 是 ext.ai 的 go-host 原生实现（host.Extension("ext.ai") 取用）。
// complete_stream 无法在同步 Provider 接口上表达流式回调，故显式报错引导到 Lua / REST 流式面。
type aiInProcessProvider struct {
	a    *AIAgent
	caps []string // 宿主上下文声明的 capability（react 的默认工具集）
}

func (p *aiInProcessProvider) Call(method string, args map[string]interface{}) (interface{}, error) {
	if method == "complete_stream" {
		return nil, fmt.Errorf("ext.ai.complete_stream 仅支持 Lua 回调式调用（ctx.ext.ai.complete_stream(args, function)）或 REST SSE 端点，无法通过同步 Provider 调用")
	}
	return callAIMethod(p.a, method, args, p.caps...)
}

// AIExtensionInProcess 构造 ext.ai 的进程内扩展定义：同步方法 + complete_stream 回调式。
func AIExtensionInProcess(a *AIAgent) *CapabilityExtension {
	return &CapabilityExtension{
		Provider: func(c *cap.Context) (interface{}, error) {
			var caps []string
			if c != nil {
				caps = c.Capabilities()
			}
			return &aiInProcessProvider{a: a, caps: caps}, nil
		},
		BuildLua: func(L *lua.LState, c *cap.Context) (lua.LValue, error) {
			tbl := L.NewTable()
			// c 是**这一次执行**的宿主上下文（见 lua.go 的 ext.BuildLua(L, host)），
			// 故 c.Capabilities() 就是调用方 Tool 自己声明的那份 capability。react 拿它当
			// 默认工具集——这是「能力边界」的落点，不能省。
			// 判空：有调用路径（含测试）会用 nil 上下文构造扩展，直接调会 nil panic。
			var callerCaps []string
			if c != nil {
				callerCaps = c.Capabilities()
			}
			// 同步方法：与旧 InProcessExtension 同款（args table → 同步结果）。
			for _, m := range aiSyncMethods {
				method := m
				tbl.RawSetString(m, L.NewFunction(func(L *lua.LState) int {
					args := map[string]interface{}{}
					if mt, ok := fromLuaValue(L, L.Get(1)).(map[string]interface{}); ok {
						args = mt
					}
					res, err := callAIMethod(a, method, args, callerCaps...)
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
