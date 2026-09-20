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
}

// LLM 是模型层接口（稳定层契约）。complete/catalog/domains/validate 不需要它；generate 的
// assemble 节点需要它。生产用 HTTPLLM，离线测试用 MockLLM。
type LLM interface {
	Complete(ctx context.Context, req LLMRequest) (string, error)
}

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
type HTTPLLM struct{}

// DefaultLLM 返回生产默认 LLM（HTTPLLM）。env GEAR_AI_PROVIDER=mock 时仍走 mock（便于离线）。
func DefaultLLM() LLM { return &HTTPLLM{} }

// Complete 解析配置 → 选 provider → 发流式请求 → 返回拼接后的文本。
func (HTTPLLM) Complete(ctx context.Context, req LLMRequest) (string, error) {
	if req.Provider == "mock" || os.Getenv("GEAR_AI_PROVIDER") == "mock" {
		return mockChatText(req), nil
	}
	cfg := resolveLLMConfig(req)
	return postChat(ctx, cfg, req.System, req.Prompt)
}

// MockLLM 是测试/离线用的确定性 LLM：按 system 内容返回结构化 JSON 文本（不联网）。
// Respond 允许覆盖默认行为（按 system 关键词返回自定义文本）。
type MockLLM struct {
	Respond func(req LLMRequest) string
}

// Complete 调用 MockLLM.Respond（未设置则回退默认 mockChatText）。
func (m *MockLLM) Complete(_ context.Context, req LLMRequest) (string, error) {
	if m.Respond != nil {
		return m.Respond(req), nil
	}
	return mockChatText(req), nil
}

// resolveLLMConfig 合并优先级：per-call 参数 > 环境变量 > 内置默认。
func resolveLLMConfig(req LLMRequest) llmConfig {
	provider := firstNonEmpty(req.Provider, os.Getenv("GEAR_AI_PROVIDER"), "siliconflow")
	baseURL := firstNonEmpty(req.BaseURL, os.Getenv("GEAR_AI_BASE_URL"), defaultBaseURL(provider))
	apiKey := firstNonEmpty(req.APIKey, os.Getenv("GEAR_AI_API_KEY"), "")
	model := firstNonEmpty(req.Model, os.Getenv("GEAR_AI_MODEL"), defaultModel(provider))
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

// postChat 按 provider 选择协议（Ollama 原生 vs OpenAI 兼容）发流式请求。
func postChat(ctx context.Context, cfg llmConfig, system, user string) (string, error) {
	msgs := []map[string]string{}
	if system != "" {
		msgs = append(msgs, map[string]string{"role": "system", "content": system})
	}
	msgs = append(msgs, map[string]string{"role": "user", "content": user})
	if cfg.Provider == "ollama" {
		return postOllama(ctx, cfg, msgs)
	}
	return postOpenAI(ctx, cfg, msgs)
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

func postOpenAI(ctx context.Context, cfg llmConfig, msgs []map[string]string) (string, error) {
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
	raw, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(raw))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	}
	resp, err := newStreamClient(cfg.Idle).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("LLM HTTP %d: %s", resp.StatusCode, string(data))
	}
	out, err := readStream(ctx, resp.Body, cfg.Idle, cfg.Total, parseOpenAILine)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(out) == "" {
		return "", fmt.Errorf("OpenAI 兼容接口流式返回为空")
	}
	return out, nil
}

func postOllama(ctx context.Context, cfg llmConfig, msgs []map[string]string) (string, error) {
	url := strings.TrimRight(cfg.BaseURL, "/") + "/api/chat"
	body := map[string]interface{}{
		"model":    cfg.Model,
		"messages": msgs,
		"stream":   true,
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(raw))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := newStreamClient(cfg.Idle).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("Ollama HTTP %d: %s", resp.StatusCode, string(data))
	}
	out, err := readStream(ctx, resp.Body, cfg.Idle, cfg.Total, parseOllamaLine)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(out) == "" {
		return "", fmt.Errorf("Ollama 流式返回为空")
	}
	return out, nil
}

// streamDelta 是流式响应的一次增量。content 用于拼接最终结果；reasoning 仅用于「有活动」判定
// （推理模型先吐 reasoning_content，只要它在动就说明没卡死）。
type streamDelta struct {
	content   string
	reasoning string
}

// readStream 逐行读取流式响应，落地两种保护（空闲超时 + 重复输出检测）。
// parse 把一行原始数据解析为增量，返回 (delta, done, recognized)：recognized=false 表示该行与本流无关（忽略）。
func readStream(ctx context.Context, body io.Reader, idle, total time.Duration, parse func(line []byte) (streamDelta, bool, bool)) (string, error) {
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
	lastCheck := 0
	const checkEvery = 64 // 每新增这么多字节才做一次重复检测，摊薄开销
	const maxTailReport = 120

	for {
		select {
		case c, ok := <-ch:
			if !ok {
				return content.String(), nil
			}
			if c.err != nil {
				return "", c.err
			}
			// 收到任何增量（含推理增量）就算「有活动」，重置空闲计时。
			if !idleT.Stop() {
				select {
				case <-idleT.C:
				default:
				}
			}
			idleT.Reset(idle)

			if c.d.reasoning != "" {
				reason.WriteString(c.d.reasoning)
			}
			if c.d.content != "" {
				content.WriteString(c.d.content)
			}
			if grown := content.Len() + reason.Len(); grown-lastCheck >= checkEvery {
				lastCheck = grown
				if loop, frag := detectRepetition(reason.String()); loop {
					return "", fmt.Errorf("检测到模型重复输出（推理片段陷入死循环）：片段 %q 连续反复出现", clip(frag, maxTailReport))
				}
				if loop, frag := detectRepetition(content.String()); loop {
					return "", fmt.Errorf("检测到模型重复输出（疑似死循环）：片段 %q 连续反复出现", clip(frag, maxTailReport))
				}
			}
		case <-idleT.C:
			return "", fmt.Errorf("LLM 流式输出空闲超时：%s 内无任何新增（连接可能假死）", idle)
		case <-totalT.C:
			return "", fmt.Errorf("LLM 流式输出总时长超时：超过 %s（已生成 %d 字节）", total, content.Len())
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
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
			} `json:"delta"`
		} `json:"choices"`
	}
	if json.Unmarshal([]byte(payload), &ev) != nil || len(ev.Choices) == 0 {
		return streamDelta{}, false, false
	}
	return streamDelta{
		content:   ev.Choices[0].Delta.Content,
		reasoning: ev.Choices[0].Delta.ReasoningContent,
	}, false, true
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
		} `json:"message"`
		Done bool `json:"done"`
	}
	if json.Unmarshal([]byte(s), &ev) != nil {
		return streamDelta{}, false, false
	}
	return streamDelta{content: ev.Message.Content}, ev.Done, true
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
