package engine

// ai_llm.go 是 ext.ai 的「模型层」原语（稳定，Go 静态编译）：一次补全请求的全部可变参数
// （provider / model / base_url / api_key / temperature）都在请求里携带，缺失时回退环境变量与内置默认。
//
// 移植自 examples/gear-ai-sidecar/llm.go（去 sidecar 化、进程内承载）。换模型、换厂商、换本地
// Ollama 只改参数，不碰 agent 逻辑（agent 在 AIAgent.generate，见 ai_agent.go）。
//
// 支持的 provider：
//   - "siliconflow"（默认）：OpenAI 兼容端点 https://api.siliconflow.cn/v1，需 api_key
//   - "ollama"：本地原生 /api/chat，无需 key
//   - "openai" / 其它 OpenAI 兼容：{base_url}/chat/completions
//   - "mock"：测试/离线确定性返回（MockLLM 实现，不走网络）
//
// 健壮性（不靠「拍一个够大的总超时」这种粗暴办法）：全部走**流式**读取，用两种信号判死：
//  1. 空闲超时（idle）：idle 内没有任何新增增量 → 判定连接/生成卡死，报错；慢的推理模型只要
//     持续吐 token 就不会误杀（推理 token 也算「有活动」）。
//  2. 重复输出检测：生成的文本/推理尾部出现同一小段连续反复 ≥ 阈值 → 判定模型陷入重复死循环，报错。
//
// 两者阈值都可用环境变量调（见 resolveLLMConfig）。

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// LLMRequest 是一次补全请求。Model/BaseURL/APIKey/Temperature 可在每次调用覆盖，
// 缺失时由 resolveLLMConfig 回退到环境变量（GEAR_AI_*）与内置默认。
type LLMRequest struct {
	System      string  `json:"system"`
	Prompt      string  `json:"prompt"`
	Provider    string  `json:"provider"`
	Model       string  `json:"model"`
	BaseURL     string  `json:"base_url"`
	APIKey      string  `json:"api_key"`
	Temperature float64 `json:"temperature"`
	Format      string  `json:"format"` // "json" → 解析 JSON 并以 {data} 返回结构化结果
	MaxTokens   int     `json:"max_tokens"`

	// Messages 是完整消息序列（React 的多轮 + tool 结果回灌用）。**非空时优先于 System/Prompt**。
	//
	// 为什么必须有它：function calling 下，assistant 消息里带 tool_calls 之后**必须**跟一条
	// `{"role":"tool","tool_call_id":...}` 消息——只把观察结果拼成普通 user 文本是非法序列，
	// 模型会以为工具没被调用过、于是重复发起同一个调用（真机实测：连发三次同一调用，
	// 全被重复调用守卫拦下，任务直接跑废）。这个字段就是为了能按协议回灌。
	Messages []map[string]any `json:"messages,omitempty"`
}

// ToolSpec 是声明给模型的一个可调用工具（OpenAI function calling 的 function 形态）。
//
// 名字有字符集限制：OpenAI 兼容接口只接受 [A-Za-z0-9_-]{1,64}，而 Gear 的工具名带点
// （http.Request / ext.ai.complete）与双下划线（mindstorm__node_create）。所以**调用方负责
// 先用 SanitizeToolName 压平并自行保存「压平名 → 原名」的映射**，模型返回的是压平名。
// 本层只做协议、不做语义，避免在模型层散落两套名字。
type ToolSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// ToolCall 是模型要求的一次工具调用。Arguments 是 JSON 文本——流式下由分片拼成完整串，
// 可能为空对象（无参工具），调用方须自行 json.Unmarshal。
type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ChatResult 是一次带工具声明的补全结果。Text 与 ToolCalls 通常互斥，但模型也可能同时给，
// 故两者都保留，由调用方决定优先级。
type ChatResult struct {
	Text      string
	ToolCalls []ToolCall
}

// toolCallPart 是流式响应里 tool_calls 的一次分片：index 标识第几个调用，
// 只有首个分片带 id 与 name，arguments 是逐片累加的 JSON 片段。
type toolCallPart struct {
	index int
	id    string
	name  string
	args  string
}

// SanitizeToolName 把 Gear 工具名压成 OpenAI 函数名的合法字符集（点换成单下划线）。
// 只做字符替换、不做语义推断，因此**不可逆**——调用方必须自己留存反查映射。
func SanitizeToolName(name string) string {
	return strings.ReplaceAll(name, ".", "_")
}

// LLM 是模型层接口（稳定层契约）。complete/catalog/domains/validate 不需要它；generate 的
// assemble 节点需要它。生产用 HTTPLLM，离线测试用 MockLLM。
//
// CompleteStream 是 Complete 的增量版：底层本就走 SSE 流式读取，只是把增量折叠成完整字符串返回；
// CompleteStream 把每个 content/reasoning 增量通过 onDelta 回调出去（reasoning 在推理模型下非空，
// 普通模型为空）。onDelta 返回非 nil 即中断流。复用同一套空闲超时 / 重复检测保护。
type LLM interface {
	Complete(ctx context.Context, req LLMRequest) (string, error)
	CompleteStream(ctx context.Context, req LLMRequest, onDelta func(content, reasoning string) error) error
	// CompleteTools 是带工具声明的补全（原生 function calling）。tools 为空时等价于 Complete。
	//
	// 与 Complete 的区别只在协议层：请求体多带 tools，返回值多带模型要求的 tool_calls。
	// 走它之后「调什么工具」由模型的 tool_calls 字段直接给出，不再靠解析自由文本——
	// 这是 react 从「猜格式」换成「按协议」的那一环（判据②）。
	CompleteTools(ctx context.Context, req LLMRequest, tools []ToolSpec) (ChatResult, error)
}

// ConfigLookup 是「AI 缺省配置」的读取源：按 key 取值，第二个返回值报告该项是否已配置。
//
// 它让「配置一次、处处生效」成为可能：用户把 provider/model/base_url/api_key 写进 config 库
// （`gear config set ai.api_key ...`），此后 ext.ai 的每一次调用都自动带上，不必再给每个进程
// export GEAR_AI_*。key 固定为 ai.provider / ai.model / ai.base_url / ai.api_key / ai.max_tokens。
//
// 刻意不做成包级全局变量：解析结果必须可追溯到来路，测试才能用「注入 nil」得到确定的
// 「不读配置库」行为（AI 缺省配置若隐式生效，单机会因开发者本机的配置而漂移）。
type ConfigLookup func(key string) (string, bool)

// llmConfig 是解析后的最终配置（已合并 per-call 覆盖 + 环境变量 + 默认）。
type llmConfig struct {
	Provider    string
	BaseURL     string
	APIKey      string
	Model       string
	Temperature float64
	MaxTokens   int
	// Thinking 控制推理模型的思考开关（仅 siliconflow 的 DeepSeek/Qwen 生效）：
	// nil = 不下发该参数，沿用模型自身默认；非 nil = 显式开/关。
	Thinking *bool
	Idle     time.Duration // 空闲超时：多久没有新增输出即判死
	Total    time.Duration // 兜底总时长：防止「慢速滴流」永远跑不完
}

// HTTPLLM 是走真实 HTTP 的模型层实现（Ollama / SiliconFlow / OpenAI 兼容），全部流式。
type HTTPLLM struct {
	// Config 是 AI 缺省配置的读取源（宿主把 config 原语装配进来时注入）。
	// nil = 不读配置库，回退链退化为「调用参数 > 环境变量 > 内置默认」。
	Config ConfigLookup
}

// DefaultLLM 返回生产默认 LLM（HTTPLLM）。env GEAR_AI_PROVIDER=mock 时仍走 mock（便于离线）。
//
// 默认**不带**配置源：AI 缺省配置是宿主显式装配的结果，不是模型层的隐式全局状态。
// 需要让配置库生效时用 LLMWithConfig。
func DefaultLLM() LLM { return &HTTPLLM{} }

// LLMWithConfig 返回带 AI 缺省配置源的 HTTPLLM（宿主装配了 config 原语时用）。
func LLMWithConfig(lookup ConfigLookup) LLM { return &HTTPLLM{Config: lookup} }

// Complete 解析配置 → 选 provider → 发流式请求 → 返回拼接后的文本。
func (h HTTPLLM) Complete(ctx context.Context, req LLMRequest) (string, error) {
	if req.Provider == "mock" || os.Getenv("GEAR_AI_PROVIDER") == "mock" {
		return mockChatText(req), nil
	}
	cfg := resolveLLMConfig(req, h.Config)
	// 显式给了消息序列就原样发出（React 多轮）；否则走既有的 system/prompt 两件套。
	if len(req.Messages) > 0 {
		text, _, err := postChatMessages(ctx, cfg, req.Messages, nil, nil)
		return text, err
	}
	return postChat(ctx, cfg, req.System, req.Prompt)
}

// CompleteStream 是 Complete 的增量版：把 SSE 增量逐条经 onDelta 回调出去，复用 readStream
// 的空闲超时 / 重复检测。onDelta 为 nil 时退化为「拼完整串后回调一次」（无害）。
func (h HTTPLLM) CompleteStream(ctx context.Context, req LLMRequest, onDelta func(content, reasoning string) error) error {
	if onDelta == nil {
		onDelta = func(string, string) error { return nil }
	}
	if req.Provider == "mock" || os.Getenv("GEAR_AI_PROVIDER") == "mock" {
		return onDelta(mockChatText(req), "")
	}
	cfg := resolveLLMConfig(req, h.Config)
	_, _, err := postChatWith(ctx, cfg, req.System, req.Prompt, nil, onDelta)
	return err
}

// CompleteTools 带上工具声明发请求，并把模型返回的 tool_calls 一并返回。
func (h HTTPLLM) CompleteTools(ctx context.Context, req LLMRequest, tools []ToolSpec) (ChatResult, error) {
	if req.Provider == "mock" || os.Getenv("GEAR_AI_PROVIDER") == "mock" {
		return ChatResult{Text: mockChatText(req)}, nil
	}
	cfg := resolveLLMConfig(req, h.Config)
	// 走 requestMessages：React 会带完整序列（含 tool 回灌），普通调用仍由 system/prompt 兜底。
	text, calls, err := postChatMessages(ctx, cfg, requestMessages(req), tools, nil)
	if err != nil {
		return ChatResult{}, err
	}
	return ChatResult{Text: text, ToolCalls: calls}, nil
}

// MockLLM 是测试/离线用的确定性 LLM：按 system 内容返回结构化 JSON 文本（不联网）。
// Respond 允许覆盖默认行为（按 system 关键词返回自定义文本）。
type MockLLM struct {
	Respond func(req LLMRequest) string
	// ToolCalls 让离线测试能断言「模型要求调工具」这条路径：非空时 CompleteTools 原样返回它，
	// 不去看 Respond（真实模型给了 tool_calls 时通常就不给正文，mock 对齐这个形态）。
	ToolCalls []ToolCall
}

// Complete 调用 MockLLM.Respond（未设置则回退默认 mockChatText）。
func (m *MockLLM) Complete(_ context.Context, req LLMRequest) (string, error) {
	if m.Respond != nil {
		return m.Respond(req), nil
	}
	return mockChatText(req), nil
}

// CompleteStream 是 MockLLM 的增量版：一次性拿到完整文本后回调一次（离线 mock 无真实流式）。
func (m *MockLLM) CompleteStream(_ context.Context, req LLMRequest, onDelta func(content, reasoning string) error) error {
	if onDelta == nil {
		onDelta = func(string, string) error { return nil }
	}
	var text string
	if m.Respond != nil {
		text = m.Respond(req)
	} else {
		text = mockChatText(req)
	}
	return onDelta(text, "")
}

// CompleteTools 是 MockLLM 的工具调用版：ToolCalls 非空则直接返回（模拟模型要求调工具），
// 否则退化为「只有文本」的普通补全。
func (m *MockLLM) CompleteTools(_ context.Context, req LLMRequest, _ []ToolSpec) (ChatResult, error) {
	if len(m.ToolCalls) > 0 {
		return ChatResult{ToolCalls: m.ToolCalls}, nil
	}
	if m.Respond != nil {
		return ChatResult{Text: m.Respond(req)}, nil
	}
	return ChatResult{Text: mockChatText(req)}, nil
}

// resolveLLMConfig 合并优先级：per-call 参数 > 环境变量 > 配置库（lookup）> 内置默认。
//
// 配置库插在环境变量**之后**是刻意的：env 的用途是「这一次换个模型/换个 key 试试」的临时覆盖，
// 配置库是「配一次、处处生效」的持久默认。这个顺序既让新能力成立，又**不改变任何既有行为**——
// 此前唯一生效的两层（调用参数、环境变量）优先级原样保留，没配过的人感知不到这一层存在。
//
// lookup 为 nil 时退化为原先的三层链（测试与未装配 config 的宿主走这条）。
func resolveLLMConfig(req LLMRequest, lookup ConfigLookup) llmConfig {
	// cfg 读配置库的一项：未装配 lookup、该项不存在、或值为空白 → 空串，交给下一级回退。
	cfg := func(key string) string {
		if lookup == nil {
			return ""
		}
		if v, ok := lookup(key); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
		return ""
	}
	provider := firstNonEmpty(req.Provider, os.Getenv("GEAR_AI_PROVIDER"), cfg("ai.provider"), "siliconflow")
	baseURL := firstNonEmpty(req.BaseURL, os.Getenv("GEAR_AI_BASE_URL"), cfg("ai.base_url"), defaultBaseURL(provider))
	apiKey := firstNonEmpty(req.APIKey, os.Getenv("GEAR_AI_API_KEY"), cfg("ai.api_key"), "")
	model := firstNonEmpty(req.Model, os.Getenv("GEAR_AI_MODEL"), cfg("ai.model"), defaultModel(provider))
	temp := req.Temperature
	if temp == 0 {
		temp = 0.2
	}
	mt := req.MaxTokens
	if mt == 0 {
		if n, ok := intFromEnv("GEAR_AI_MAX_TOKENS"); ok {
			mt = n
		}
	}
	if mt == 0 {
		if n, err := strconv.Atoi(cfg("ai.max_tokens")); err == nil && n > 0 {
			mt = n
		}
	}
	if mt == 0 {
		mt = 4096 // 缺省上限：给推理模型留出「思考 + 结构化 JSON」的余量，同时兜住失控生成
	}
	// Thinking 默认「不下发」——沿用模型自身默认（DeepSeek 推理模型即开启思考）。
	// 流式+空闲超时已能容纳慢推理，故无需为速度强行关思考；确有需要再显式关闭。
	var thinking *bool
	if v := strings.TrimSpace(os.Getenv("GEAR_AI_THINKING")); v != "" {
		b := strings.EqualFold(v, "true") || v == "1"
		thinking = &b
	}
	return llmConfig{
		Provider:    provider,
		BaseURL:     baseURL,
		APIKey:      apiKey,
		Model:       model,
		Temperature: temp,
		MaxTokens:   mt,
		Thinking:    thinking,
		Idle:        durFromEnvSec("GEAR_AI_IDLE_TIMEOUT", 60*time.Second),
		Total:       durFromEnvSec("GEAR_AI_TOTAL_TIMEOUT", 900*time.Second),
	}
}

func defaultBaseURL(p string) string {
	if p == "ollama" {
		if v := os.Getenv("GEAR_AI_OLLAMA_URL"); v != "" {
			return v
		}
		return "http://localhost:11434"
	}
	return "https://api.siliconflow.cn/v1"
}

func defaultModel(p string) string {
	if p == "ollama" {
		if v := os.Getenv("GEAR_AI_OLLAMA_MODEL"); v != "" {
			return v
		}
		return "qwen3:latest"
	}
	if v := os.Getenv("GEAR_AI_SILICONFLOW_MODEL"); v != "" {
		return v
	}
	return "Qwen/Qwen3-8B"
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

func intFromEnv(name string) (int, bool) {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n, true
		}
	}
	return 0, false
}

// durFromEnvSec 读「秒」为单位的环境变量；缺省/非法则用 def。
func durFromEnvSec(name string, def time.Duration) time.Duration {
	if n, ok := intFromEnv(name); ok && n > 0 {
		return time.Duration(n) * time.Second
	}
	return def
}

// postChat 是 postChatWith 的同步版：onDelta 为 nil（供 Complete 使用），签名保持稳定，既有测试无需改动。
func postChat(ctx context.Context, cfg llmConfig, system, user string) (string, error) {
	text, _, err := postChatWith(ctx, cfg, system, user, nil, nil)
	return text, err
}

// postChatWith 按 provider 选择协议（Ollama 原生 vs OpenAI 兼容）发流式请求，
// 并把每个 SSE 增量经 onDelta 回调出去（增量版 CompleteStream 用；nil 时仅拼完整串）。
//
// tools 非空时随请求下发 function 声明，返回值多带模型要求的 tool_calls；
// msgs 放宽到 map[string]any 是为了让调用方能塞 `{"role":"tool",...}` 这类带
// tool_call_id 的消息——function calling 的回灌必须能带得上这个 id。
func postChatWith(ctx context.Context, cfg llmConfig, system, user string, tools []ToolSpec, onDelta func(content, reasoning string) error) (string, []ToolCall, error) {
	msgs := []map[string]any{}
	if system != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": system})
	}
	msgs = append(msgs, map[string]any{"role": "user", "content": user})
	return postChatMessages(ctx, cfg, msgs, tools, onDelta)
}

// postChatMessages 是 postChatWith 的下层：直接收完整消息序列（React 的多轮与 tool 回灌走它）。
func postChatMessages(ctx context.Context, cfg llmConfig, msgs []map[string]any, tools []ToolSpec, onDelta func(content, reasoning string) error) (string, []ToolCall, error) {
	if cfg.Provider == "ollama" {
		return postOllama(ctx, cfg, msgs, tools, onDelta)
	}
	return postOpenAI(ctx, cfg, msgs, tools, onDelta)
}

// requestMessages 决定一次请求真正发出去的消息序列：显式给了 Messages 就用它，
// 否则由 System/Prompt 兜底构造（既有行为不变）。
func requestMessages(req LLMRequest) []map[string]any {
	if len(req.Messages) > 0 {
		return req.Messages
	}
	msgs := []map[string]any{}
	if req.System != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": req.System})
	}
	return append(msgs, map[string]any{"role": "user", "content": req.Prompt})
}

// newStreamClient 构造「无总超时」的客户端：可靠性交给 readStream 的空闲/总时长保护，
// 但连接建立与响应头仍需在 idle 内完成（防止 TCP/TLS 卡死时无限等待）。
func newStreamClient(idle time.Duration) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   15 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			TLSHandshakeTimeout:   15 * time.Second,
			ResponseHeaderTimeout: idle,
			ExpectContinueTimeout: 1 * time.Second,
		},
	}
}

func postOpenAI(ctx context.Context, cfg llmConfig, msgs []map[string]any, tools []ToolSpec, onDelta func(content, reasoning string) error) (string, []ToolCall, error) {
	url := strings.TrimRight(cfg.BaseURL, "/") + "/chat/completions"
	body := map[string]interface{}{
		"model":       cfg.Model,
		"messages":    msgs,
		"temperature": cfg.Temperature,
		"max_tokens":  cfg.MaxTokens,
		"stream":      true,
	}
	// SiliconFlow 推理模型（DeepSeek/Qwen）用 chat_template_kwargs 控制思考；未显式设置则不下发，沿用模型默认。
	if cfg.Provider == "siliconflow" && cfg.Thinking != nil {
		body["chat_template_kwargs"] = map[string]interface{}{"enable_thinking": *cfg.Thinking}
	}
	if len(tools) > 0 {
		body["tools"] = toolSpecsToWire(tools)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return "", nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(raw))
	if err != nil {
		return "", nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	}
	resp, err := newStreamClient(cfg.Idle).Do(req)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", nil, fmt.Errorf("LLM HTTP %d: %s", resp.StatusCode, string(data))
	}
	out, calls, err := readStreamWith(ctx, resp.Body, cfg.Idle, cfg.Total, parseOpenAILine, onDelta)
	if err != nil {
		return "", nil, err
	}
	// function calling 下模型可能只给 tool_calls、正文为空——那是合法结果，不算「返回为空」。
	if strings.TrimSpace(out) == "" && len(calls) == 0 {
		return "", nil, fmt.Errorf("OpenAI 兼容接口流式返回为空")
	}
	return out, calls, nil
}

func postOllama(ctx context.Context, cfg llmConfig, msgs []map[string]any, tools []ToolSpec, onDelta func(content, reasoning string) error) (string, []ToolCall, error) {
	url := strings.TrimRight(cfg.BaseURL, "/") + "/api/chat"
	body := map[string]interface{}{
		"model":    cfg.Model,
		"messages": msgs,
		"stream":   true,
	}
	if len(tools) > 0 {
		// Ollama 原生协议直接收 function 对象（不套 {"type":"function"} 这层）。
		body["tools"] = ollamaTools(tools)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return "", nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(raw))
	if err != nil {
		return "", nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := newStreamClient(cfg.Idle).Do(req)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", nil, fmt.Errorf("Ollama HTTP %d: %s", resp.StatusCode, string(data))
	}
	out, calls, err := readStreamWith(ctx, resp.Body, cfg.Idle, cfg.Total, parseOllamaLine, onDelta)
	if err != nil {
		return "", nil, err
	}
	if strings.TrimSpace(out) == "" && len(calls) == 0 {
		return "", nil, fmt.Errorf("Ollama 流式返回为空")
	}
	return out, calls, nil
}

// toolSpecsToWire 把工具声明压成 OpenAI 兼容的 tools 数组（外层 {"type":"function"}）。
func toolSpecsToWire(tools []ToolSpec) []map[string]any {
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		out = append(out, map[string]any{"type": "function", "function": t.functionObject()})
	}
	return out
}

// ollamaTools 是 Ollama 原生协议的工具数组：直接是 function 对象，不套 type 层。
func ollamaTools(tools []ToolSpec) []map[string]any {
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		out = append(out, t.functionObject())
	}
	return out
}

// functionObject 是两种协议共用的 function 描述体。parameters 缺省补一个空 object——
// 无参工具也必须给 schema，否则部分厂商直接 400。
func (t ToolSpec) functionObject() map[string]any {
	fn := map[string]any{"name": t.Name}
	if t.Description != "" {
		fn["description"] = t.Description
	}
	if t.Parameters != nil {
		fn["parameters"] = t.Parameters
	} else {
		fn["parameters"] = map[string]any{"type": "object"}
	}
	return fn
}

// streamDelta 是流式响应的一次增量。content 用于拼接最终结果；reasoning 仅用于「有活动」判定
// （推理模型先吐 reasoning_content，只要它在动就说明没卡死）。
// toolParts 是本行携带的 tool_calls 分片（可能多个，按 index 区分）。
type streamDelta struct {
	content   string
	reasoning string
	toolParts []toolCallPart
}

// readStream 是 readStreamWith 的同步版：onDelta 为 nil，把增量折叠成完整字符串返回（供 Complete 使用）。
// 原函数签名保持稳定，既有测试无需改动。
func readStream(ctx context.Context, body io.Reader, idle, total time.Duration, parse func(line []byte) (streamDelta, bool, bool)) (string, error) {
	text, _, err := readStreamWith(ctx, body, idle, total, parse, nil)
	return text, err
}

// readStreamWith 逐行读取流式响应，落地两种保护（空闲超时 + 重复输出检测）。
// parse 把一行原始数据解析为增量，返回 (delta, done, recognized)：recognized=false 表示该行与本流无关（忽略）。
// onDelta 非 nil 时，每个增量会经它回调出去（增量版 CompleteStream 用）；nil 时仅拼完整串。
//
// 第二个返回值是模型要求的 tool_calls（按 index 聚合后的完整列表，无则 nil）。
// 流式下 tool_calls 是分片到达的：只有首个分片带 id/name，arguments 是逐片累加的 JSON 片段，
// 所以聚合放在这里而不是 parse 里——parse 只管一行，跨行的拼接必须落在读循环上。
func readStreamWith(ctx context.Context, body io.Reader, idle, total time.Duration, parse func(line []byte) (streamDelta, bool, bool), onDelta func(content, reasoning string) error) (string, []ToolCall, error) {
	type chunk struct {
		d   streamDelta
		err error
	}
	ch := make(chan chunk, 32)
	stop := make(chan struct{})
	defer close(stop)

	go func() {
		defer close(ch) // 所有退出路径（含 [DONE] 早退）都要关闭，主循环靠它判定「正常结束」
		sc := bufio.NewScanner(body)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for sc.Scan() {
			d, done, recognized := parse(sc.Bytes())
			if recognized {
				select {
				case ch <- chunk{d: d}:
				case <-stop:
					return
				}
			}
			if done {
				return
			}
		}
		if err := sc.Err(); err != nil {
			select {
			case ch <- chunk{err: err}:
			case <-stop:
			}
		}
	}()

	idleT := time.NewTimer(idle)
	defer idleT.Stop()
	totalT := time.NewTimer(total)
	defer totalT.Stop()

	var content, reason strings.Builder
	calls := map[int]*ToolCall{} // index → 累积中的工具调用（流式分片按 index 归拢）
	lastCheck := 0
	const checkEvery = 64 // 每新增这么多字节才做一次重复检测，摊薄开销
	const maxTailReport = 120

	for {
		select {
		case c, ok := <-ch:
			if !ok {
				return content.String(), finishToolCalls(calls), nil
			}
			if c.err != nil {
				return "", nil, c.err
			}
			for _, p := range c.d.toolParts {
				mergeToolPart(calls, p)
			}
			// 收到任何增量（含推理增量与 tool_calls 分片）就算「有活动」，重置空闲计时。
			if !idleT.Stop() {
				select {
				case <-idleT.C:
				default:
				}
			}
			idleT.Reset(idle)

			if c.d.content != "" || c.d.reasoning != "" {
				if onDelta != nil {
					if err := onDelta(c.d.content, c.d.reasoning); err != nil {
						return "", nil, err
					}
				}
			}
			if c.d.reasoning != "" {
				reason.WriteString(c.d.reasoning)
			}
			if c.d.content != "" {
				content.WriteString(c.d.content)
			}
			if grown := content.Len() + reason.Len(); grown-lastCheck >= checkEvery {
				lastCheck = grown
				if loop, frag := detectRepetition(reason.String()); loop {
					return "", nil, fmt.Errorf("检测到模型重复输出（推理片段陷入死循环）：片段 %q 连续反复出现", clip(frag, maxTailReport))
				}
				if loop, frag := detectRepetition(content.String()); loop {
					return "", nil, fmt.Errorf("检测到模型重复输出（疑似死循环）：片段 %q 连续反复出现", clip(frag, maxTailReport))
				}
			}
		case <-idleT.C:
			return "", nil, fmt.Errorf("LLM 流式输出空闲超时：%s 内无任何新增（连接可能假死）", idle)
		case <-totalT.C:
			return "", nil, fmt.Errorf("LLM 流式输出总时长超时：超过 %s（已生成 %d 字节）", total, content.Len())
		case <-ctx.Done():
			return "", nil, ctx.Err()
		}
	}
}

// mergeToolPart 把一个 tool_calls 分片并进对应 index 的调用：
// 首片带 id/name，后续片只带 arguments 片段（逐片累加成完整 JSON）。
func mergeToolPart(calls map[int]*ToolCall, p toolCallPart) {
	c, ok := calls[p.index]
	if !ok {
		c = &ToolCall{}
		calls[p.index] = c
	}
	if p.id != "" {
		c.ID = p.id
	}
	if p.name != "" {
		c.Name = p.name
	}
	c.Arguments += p.args
}

// finishToolCalls 按 index 升序落成最终列表。必须排序：分片可能乱序到达，
// 而调用方按返回顺序执行时，顺序应当稳定可复现（否则同名多调用不好断言）。
func finishToolCalls(calls map[int]*ToolCall) []ToolCall {
	if len(calls) == 0 {
		return nil
	}
	idx := make([]int, 0, len(calls))
	for i := range calls {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	out := make([]ToolCall, 0, len(idx))
	for _, i := range idx {
		out = append(out, *calls[i])
	}
	return out
}

// parseOpenAILine 解析 OpenAI 兼容 SSE：形如 `data: {...}`，结束于 `data: [DONE]`。
func parseOpenAILine(line []byte) (streamDelta, bool, bool) {
	s := strings.TrimSpace(string(line))
	if !strings.HasPrefix(s, "data:") {
		return streamDelta{}, false, false
	}
	payload := strings.TrimSpace(s[len("data:"):])
	if payload == "" {
		return streamDelta{}, false, false
	}
	if payload == "[DONE]" {
		return streamDelta{}, true, true
	}
	var ev struct {
		Choices []struct {
			Delta struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				ToolCalls        []struct {
					Index    int    `json:"index"`
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if json.Unmarshal([]byte(payload), &ev) != nil || len(ev.Choices) == 0 {
		return streamDelta{}, false, false
	}
	d := streamDelta{
		content:   ev.Choices[0].Delta.Content,
		reasoning: ev.Choices[0].Delta.ReasoningContent,
	}
	for _, tc := range ev.Choices[0].Delta.ToolCalls {
		d.toolParts = append(d.toolParts, toolCallPart{
			index: tc.Index,
			id:    tc.ID,
			name:  tc.Function.Name,
			args:  tc.Function.Arguments,
		})
	}
	return d, false, true
}

// parseOllamaLine 解析 Ollama 原生 NDJSON：每行一个对象，done=true 表示结束。
func parseOllamaLine(line []byte) (streamDelta, bool, bool) {
	s := strings.TrimSpace(string(line))
	if s == "" {
		return streamDelta{}, false, false
	}
	var ev struct {
		Message struct {
			Content string `json:"content"`
			// Ollama 的 tool_calls 挂在 message 上（非流式字段，流式下一般一次性给全）。
			ToolCalls []struct {
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
		Done bool `json:"done"`
	}
	if json.Unmarshal([]byte(s), &ev) != nil {
		return streamDelta{}, false, false
	}
	d := streamDelta{content: ev.Message.Content}
	// Ollama 不给 index，按下标补一个——保证与 OpenAI 侧一样可以按 index 归拢。
	for i, tc := range ev.Message.ToolCalls {
		d.toolParts = append(d.toolParts, toolCallPart{
			index: i,
			id:    tc.ID,
			name:  tc.Function.Name,
			args:  tc.Function.Arguments,
		})
	}
	return d, ev.Done, true
}

// detectRepetition 判定文本尾部是否陷入「同一小段连续反复」的死循环。
// 两类模式：
//   - 字符块级：长度 ≤ 40 rune 的片段连续重复 ≥ 8 次；
//   - 行级：同一整行连续重复 ≥ 6 次（覆盖比 40 rune 更长的重复句/行）。
//
// 两者都要求重复总长度 ≥ 120 rune，避免正常短重复（如 JSON 的 `{}`、空行）误报。
func detectRepetition(s string) (bool, string) {
	const (
		maxBlock   = 40
		minRepeats = 8
		minTotal   = 120
		maxTail    = 8000
	)
	r := []rune(s)
	n := len(r)
	if n < minTotal {
		return false, ""
	}
	start := 0
	if n > maxTail {
		start = n - maxTail
	}
	tail := r[start:]
	m := len(tail)

	// 1) 字符块级重复
	for w := 1; w <= maxBlock; w++ {
		if w*minRepeats > m {
			break
		}
		block := tail[m-w:]
		if strings.TrimSpace(string(block)) == "" {
			continue // 纯空白不算
		}
		cnt := 1
		for i := m - w; i-w >= 0 && runesEqual(tail[i-w:i], block); i -= w {
			cnt++
		}
		if cnt >= minRepeats && cnt*w >= minTotal {
			return true, string(block)
		}
	}

	// 2) 行级重复（同一整行连续出现）
	text := strings.TrimRight(string(tail), "\n\r")
	lines := strings.Split(text, "\n")
	if len(lines) >= 6 {
		last := strings.TrimRight(lines[len(lines)-1], "\r")
		if lr := len([]rune(strings.TrimSpace(last))); lr >= 4 {
			cnt := 0
			for i := len(lines) - 1; i >= 0; i-- {
				if strings.TrimRight(lines[i], "\r") == last {
					cnt++
				} else {
					break
				}
			}
			if cnt >= 6 && cnt*lr >= minTotal {
				return true, strings.TrimSpace(last)
			}
		}
	}
	return false, ""
}

func runesEqual(a, b []rune) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// clip 截断过长的片段用于错误信息（避免把整段重复文本倒进日志）。
func clip(s string, maxRunes int) string {
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	return string(r[:maxRunes]) + "…"
}

// mockChatText 测试/离线用的确定性 provider：按 system 内容返回结构化 JSON 文本。
func mockChatText(req LLMRequest) string {
	var text string
	switch {
	case strings.Contains(req.System, "规划器"):
		text = `[{"subtask":"执行步骤","domain":"general"}]`
	case strings.Contains(req.System, "原子能力") || strings.Contains(req.System, "Tool"):
		text = `{"id":"demo.generated","name":"生成的工具","description":"示例","runtime":"lua","entry":"--","input_schema":{"type":"object"},"output_schema":{"type":"object"},"versions":[{"version":"1.0.0","state":"draft"}]}`
	default:
		text = `{"id":"demo.flow","name":"生成的流程","description":"示例","steps":[{"id":"s1","call":"demo.echo","input":{}}]}`
	}
	return text
}
