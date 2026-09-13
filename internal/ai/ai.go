// Package ai 实现 Gear 的 AI 闭环（S6c）：自然语言 prompt → Flow 定义。
//
// 设计取舍：
//   - 内核只负责「把 LLM 的原始文本收敛成合法 Flow」这一收敛逻辑；LLM 本身是可插拔的
//     Inferrer（默认本地 Ollama，qwen3.5:4b；远端网关 / Mock 测试均可注入）。
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
	"regexp"
	"strings"
	"time"

	"gear/internal/model"
	"gopkg.in/yaml.v3"
)

// DefaultOllamaURL / DefaultModel 是 Ollama 推断器的默认值（用户本地模型偏好）。
const (
	DefaultOllamaURL = "http://localhost:11434"
	DefaultModel     = "qwen3.5:4b"
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
	body, err := json.Marshal(ollamaGenerateReq{Model: o.Model, Prompt: prompt, Stream: false})
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
