// Package ai 实现 Gear 的 AI 闭环（S6c）：自然语言 prompt → Flow 定义。
//
// 设计取舍：
//   - 内核只负责「把 LLM 的原始文本收敛成合法 Flow」这一收敛逻辑；LLM 本身是可插拔的
//     Inferrer。默认走线上 SiliconFlow（DeepSeek-V4-Flash，OpenAI 兼容 chat/completions），
//     未配置 API Key 时回退本地 Ollama（qwen3.5:4b）；远端网关 / Mock 测试均可注入。
//   - 推断结果经「去 ```yaml 围栏 → yaml.Unmarshal → Flow.Validate」三步校验；
//     任一步失败即重试（默认 ≤3 次），彻底失败的 prompt 返回错误而非污染 registry。
//   - 不在此处做「Tool 是否存在」的运行期校验——那是 engine.ValidateFlowStatic 的职责，
//     AI 只保证 Flow 结构体合法，引用解析留给注册 / 执行期（与 CLI、REST 共用同一份规则）。
package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"gear/internal/model"
	"gopkg.in/yaml.v3"
)

// DefaultOllamaURL / DefaultModel 是 Ollama 推断器的默认值（本地回退用）。
const (
	DefaultOllamaURL = "http://localhost:11434"
	DefaultModel     = "qwen3.5:4b"
)

// SiliconFlow（硅基流动）线上推断器的默认值 + 环境变量名。
// 默认走线上 AI：用户本地无需装 Ollama，直接调用 DeepSeek 等模型出 Flow。
const (
	DefaultSiliconFlowBaseURL = "https://api.siliconflow.cn/v1"
	DefaultSiliconFlowModel   = "deepseek-ai/DeepSeek-V4-Flash"
	// EnvSiliconFlowKey 是 SiliconFlow API Key 的环境变量；未设置时回退本地 Ollama。
	EnvSiliconFlowKey = "SILICONFLOW_API_KEY"
)

// Inferrer 把自然语言 prompt 变成原始文本（通常是含 ```yaml 围栏的 Flow 定义）。
// 抽成接口便于测试注入 Mock，也让「换 LLM 后端」成为纯配置问题。
type Inferrer interface {
	Infer(ctx context.Context, prompt string) (string, error)
}

// OllamaClient 是 Inferrer 的默认实现：打到本地 Ollama 的 /api/generate。
type OllamaClient struct {
	BaseURL string
	Model   string
	HTTP    *http.Client
}

// NewOllama 构造 Ollama 客户端；baseURL / model 为空时回退默认值
// （默认 localhost:11434 + qwen3.5:4b，与用户本地模型偏好一致）。
func NewOllama(baseURL, model string) *OllamaClient {
	if baseURL == "" {
		baseURL = DefaultOllamaURL
	}
	if model == "" {
		model = DefaultModel
	}
	return &OllamaClient{
		BaseURL: baseURL,
		Model:   model,
		HTTP:    &http.Client{Timeout: 120 * time.Second},
	}
}

type ollamaGenerateReq struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
	Stream bool   `json:"stream"`
}

type ollamaGenerateResp struct {
	Response string `json:"response"`
	Done     bool   `json:"done"`
}

// Infer 调用 Ollama /api/generate（非流式），返回模型原始输出文本。
func (o *OllamaClient) Infer(ctx context.Context, prompt string) (string, error) {
	body, err := json.Marshal(ollamaGenerateReq{Model: o.Model, Prompt: systemPromptText + "\n\n用户需求：" + prompt, Stream: false})
	if err != nil {
		return "", fmt.Errorf("序列化请求失败: %w", err)
	}
	url := strings.TrimRight(o.BaseURL, "/") + "/api/generate"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("构造请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := o.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("调用 Ollama 失败（%s）: %w", url, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("读取响应失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("Ollama 返回 %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	var out ollamaGenerateResp
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("解析 Ollama 响应失败: %w", err)
	}
	return out.Response, nil
}

// SiliconFlowClient 是 Inferrer 的线上实现：打到 SiliconFlow 的 OpenAI 兼容
// /v1/chat/completions（默认 deepseek-ai/DeepSeek-V4-Flash）。相比本地 Ollama，
// 它不需要本机装模型，适合「线上 AI 直接出 Flow」的场景（用户本地无 Ollama 时首选）。
type SiliconFlowClient struct {
	APIKey  string
	Model   string
	BaseURL string
	HTTP    *http.Client
}

// NewSiliconFlow 构造 SiliconFlow 客户端；任一参数为空时回退默认值/环境变量：
//   - APIKey 回退 SILICONFLOW_API_KEY（必填，否则 Infer 会因 401 失败）
//   - Model 回退 deepseek-ai/DeepSeek-V4-Flash
//   - BaseURL 回退 https://api.siliconflow.cn/v1
func NewSiliconFlow(apiKey, model, baseURL string) *SiliconFlowClient {
	if apiKey == "" {
		apiKey = os.Getenv(EnvSiliconFlowKey)
	}
	if model == "" {
		model = DefaultSiliconFlowModel
	}
	if baseURL == "" {
		baseURL = DefaultSiliconFlowBaseURL
	}
	return &SiliconFlowClient{
		APIKey:  apiKey,
		Model:   model,
		BaseURL: baseURL,
		HTTP:    &http.Client{Timeout: 120 * time.Second},
	}
}

type sfChatReq struct {
	Model       string      `json:"model"`
	Messages    []sfMessage `json:"messages"`
	Temperature float64     `json:"temperature"`
	Stream      bool        `json:"stream"`
}

type sfMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type sfChatResp struct {
	Choices []struct {
		Message struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
		} `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Infer 调用 SiliconFlow /v1/chat/completions（非流式），用系统提示约束输出为合法 Flow YAML。
func (c *SiliconFlowClient) Infer(ctx context.Context, prompt string) (string, error) {
	if c.APIKey == "" {
		return "", fmt.Errorf("SiliconFlow 需要 API Key（设环境变量 %s 或传参）", EnvSiliconFlowKey)
	}
	body, err := json.Marshal(sfChatReq{
		Model: c.Model,
		Messages: []sfMessage{
			{Role: "system", Content: systemPromptText},
			{Role: "user", Content: prompt},
		},
		Temperature: 0.3,
		Stream:      false,
	})
	if err != nil {
		return "", fmt.Errorf("序列化请求失败: %w", err)
	}
	url := strings.TrimRight(c.BaseURL, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("构造请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.APIKey)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("调用 SiliconFlow 失败（%s）: %w", url, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("读取响应失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("SiliconFlow 返回 %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	var out sfChatResp
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("解析 SiliconFlow 响应失败: %w", err)
	}
	if len(out.Choices) == 0 {
		msg := ""
		if out.Error != nil {
			msg = out.Error.Message
		}
		return "", fmt.Errorf("SiliconFlow 返回空 choices（%s）: %s", msg, strings.TrimSpace(string(raw)))
	}
	content := out.Choices[0].Message.Content
	if content == "" {
		content = out.Choices[0].Message.ReasoningContent
	}
	return stripThink(content), nil
}

// systemPromptText 是发给 LLM 的系统提示：约束输出为合法、完整、可注册的 Gear Flow YAML。
// 不写代码块围栏、不写解释，直接出 YAML（stripYAMLFence 同时兼容围栏写法）。
const systemPromptText = `你是 Gear 自动化编排框架的 Flow 设计助手。根据用户的自然语言需求，输出一个合法、完整、可注册的 Gear Flow YAML（不要包裹代码块、不要加解释文字）。

Gear Flow 合法字段只有：id、name、description、vars（可选）、steps。
每个 step 的字段：id、call（Tool id）、input（具体参数值）、depends_on（可选，上游 step id 列表）、parallel（可选 bool，有 depends_on 时不要设）、when（可选，条件表达式不要写 ${}）、mode（可选 once/service）。

硬性要求：
1. 必须有 id 与至少一个 step；每个 step 必须有 id 与 call。
2. call 用语义清晰的 kebab-case 命名（如 fetch-order、send-email）；Tool 未注册时用户会自行补。
3. 用 depends_on 表达先后依赖；可并行的步骤设 parallel: true。
4. 每个 step.input 给具体、合理的参数值，不要空 map、不要占位符。
5. 除非需求真的只做一件事，否则给出 2-4 个步骤，体现真实编排（例如：取数->处理->落库/通知）。
6. 输出纯 YAML。`

// thinkRE 去掉部分 DeepSeek 模型在 content 中夹带的 <think>...</think> 思考块。
var thinkRE = regexp.MustCompile("(?s)<think>.*?</think>")

func stripThink(s string) string {
	return strings.TrimSpace(thinkRE.ReplaceAllString(s, ""))
}

// DefaultInferrer 按环境选择默认推断器：配置了 SILICONFLOW_API_KEY 走线上 SiliconFlow，
// 否则回退本地 Ollama（便于无 Key 的本地开发）。测试 / 远端网关可改用 NewWithInferrer 注入。
func DefaultInferrer() Inferrer {
	if os.Getenv(EnvSiliconFlowKey) != "" {
		return NewSiliconFlow("", "", "")
	}
	return NewOllama("", "")
}

// GenerateFlow 用 inferrer 把 prompt 收敛成合法 Flow：失败重试 ≤ maxRetries 次。
// 收敛步骤：去 ```yaml 围栏 → yaml.Unmarshal → Flow.Validate。
func GenerateFlow(ctx context.Context, inf Inferrer, prompt string) (*model.Flow, error) {
	const maxRetries = 3
	var lastErr error
	for attempt := 1; attempt <= maxRetries; attempt++ {
		raw, err := inf.Infer(ctx, prompt)
		if err != nil {
			lastErr = fmt.Errorf("推断第 %d 次失败: %w", attempt, err)
			continue
		}
		flow, err := parseAndValidateFlow(raw)
		if err != nil {
			lastErr = fmt.Errorf("收敛第 %d 次失败: %w", attempt, err)
			continue
		}
		return flow, nil
	}
	return nil, fmt.Errorf("AI 生成 Flow 失败（已重试 %d 次）: %w", maxRetries, lastErr)
}

// parseAndValidateFlow 把 LLM 原始文本解析为 Flow 并校验结构合法性。
func parseAndValidateFlow(raw string) (*model.Flow, error) {
	clean := stripYAMLFence(raw)
	var f model.Flow
	if err := yaml.Unmarshal([]byte(clean), &f); err != nil {
		return nil, fmt.Errorf("解析 YAML 失败: %w", err)
	}
	if err := f.Validate(); err != nil {
		return nil, fmt.Errorf("Flow 校验失败: %w", err)
	}
	return &f, nil
}

// fenceRE 匹配 ```yaml ... ``` 或 ``` ... ``` 围栏（S6c：LLM 常见输出包装）。
var fenceRE = regexp.MustCompile("(?s)```(?:yaml)?\\s*(.*?)```")

// stripYAMLFence 去掉 LLM 输出外层的 Markdown 代码围栏；若无围栏则原样返回去空白。
func stripYAMLFence(s string) string {
	if m := fenceRE.FindStringSubmatch(s); m != nil {
		return strings.TrimSpace(m[1])
	}
	return strings.TrimSpace(s)
}
