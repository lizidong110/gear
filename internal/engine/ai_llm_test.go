package engine

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 本文件覆盖 ext.ai 的模型层（ai_llm.go）。此前整文件 0% 覆盖——而它恰恰承载两条
// 健壮性红线，不测等于没测：
//  1. **判死而不靠大超时**：空闲超时（idle）与总时长（total）两道闸，慢推理模型只要
//     持续吐 token 就不该被误杀，卡死/滴流必须报错；
//  2. **重复输出检测**：模型陷入「同一小段连续反复」的死循环时报错，而不是把几 MB
//     垃圾文本当作结果返回。
// 全部用例走 httptest 本地端点或纯函数，不联网、不依赖真实 key。

// ---- 配置解析（per-call > 环境变量 > 内置默认）----

func TestResolveLLMConfigDefaults(t *testing.T) {
	cfg := resolveLLMConfig(LLMRequest{}, nil)
	if cfg.Provider != "siliconflow" || cfg.BaseURL != "https://api.siliconflow.cn/v1" {
		t.Fatalf("默认 provider/base 不符: %+v", cfg)
	}
	if cfg.Model != "Qwen/Qwen3-8B" {
		t.Fatalf("默认 model 不符: %q", cfg.Model)
	}
	if cfg.Temperature != 0.2 || cfg.MaxTokens != 4096 {
		t.Fatalf("默认 temperature/max_tokens 不符: %v/%v", cfg.Temperature, cfg.MaxTokens)
	}
	if cfg.Thinking != nil {
		t.Fatal("未设 GEAR_AI_THINKING 时应不下发该参数（沿用模型自身默认）")
	}
	if cfg.Idle != 60*time.Second || cfg.Total != 900*time.Second {
		t.Fatalf("默认 idle/total 不符: %v/%v", cfg.Idle, cfg.Total)
	}
}

func TestResolveLLMConfigPrecedenceAndEnv(t *testing.T) {
	t.Setenv("GEAR_AI_PROVIDER", "ollama")
	t.Setenv("GEAR_AI_BASE_URL", "http://env-host:1")
	t.Setenv("GEAR_AI_API_KEY", "env-key")
	t.Setenv("GEAR_AI_MODEL", "env-model")
	t.Setenv("GEAR_AI_MAX_TOKENS", "123")
	t.Setenv("GEAR_AI_THINKING", "true")
	t.Setenv("GEAR_AI_IDLE_TIMEOUT", "5")
	t.Setenv("GEAR_AI_TOTAL_TIMEOUT", "0") // 非法/非正 → 回落默认

	// 全空请求 → 全部取环境变量
	cfg := resolveLLMConfig(LLMRequest{}, nil)
	if cfg.Provider != "ollama" || cfg.BaseURL != "http://env-host:1" || cfg.APIKey != "env-key" || cfg.Model != "env-model" {
		t.Fatalf("环境变量未生效: %+v", cfg)
	}
	if cfg.MaxTokens != 123 {
		t.Fatalf("GEAR_AI_MAX_TOKENS 未生效: %d", cfg.MaxTokens)
	}
	if cfg.Thinking == nil || !*cfg.Thinking {
		t.Fatal("GEAR_AI_THINKING=true 应显式开启")
	}
	if cfg.Idle != 5*time.Second {
		t.Fatalf("GEAR_AI_IDLE_TIMEOUT 未生效: %v", cfg.Idle)
	}
	if cfg.Total != 900*time.Second {
		t.Fatalf("非正的 total 应回落默认: %v", cfg.Total)
	}

	// per-call 参数优先于环境变量
	cfg2 := resolveLLMConfig(LLMRequest{Provider: "openai", Model: "m", BaseURL: "http://call", APIKey: "k", Temperature: 0.9, MaxTokens: 7}, nil)
	if cfg2.Provider != "openai" || cfg2.Model != "m" || cfg2.BaseURL != "http://call" || cfg2.APIKey != "k" {
		t.Fatalf("per-call 参数应优先: %+v", cfg2)
	}
	if cfg2.Temperature != 0.9 || cfg2.MaxTokens != 7 {
		t.Fatalf("per-call temperature/max_tokens 应优先: %v/%v", cfg2.Temperature, cfg2.MaxTokens)
	}

	// 空白字符串视为未设置（TrimSpace 后为空即跳过）
	if got := firstNonEmpty("  ", "b", "c"); got != "b" {
		t.Fatalf("空白应视为空，实际 %q", got)
	}
}

func TestResolveLLMConfigOllamaDefaultsAndOverrides(t *testing.T) {
	cfg := resolveLLMConfig(LLMRequest{Provider: "ollama"}, nil)
	if cfg.BaseURL != "http://localhost:11434" || cfg.Model != "qwen3:latest" {
		t.Fatalf("ollama 默认不符: %+v", cfg)
	}
	t.Setenv("GEAR_AI_OLLAMA_URL", "http://my-ollama:11434")
	t.Setenv("GEAR_AI_OLLAMA_MODEL", "llama3:8b")
	cfg2 := resolveLLMConfig(LLMRequest{Provider: "ollama"}, nil)
	if cfg2.BaseURL != "http://my-ollama:11434" || cfg2.Model != "llama3:8b" {
		t.Fatalf("ollama 环境变量覆盖未生效: %+v", cfg2)
	}
	t.Setenv("GEAR_AI_SILICONFLOW_MODEL", "deepseek-ai/DeepSeek-R1")
	if got := defaultModel("siliconflow"); got != "deepseek-ai/DeepSeek-R1" {
		t.Fatalf("siliconflow model 覆盖未生效: %q", got)
	}
}

func TestEnvHelpers(t *testing.T) {
	if n, ok := intFromEnv("GEAR_TEST_INT_UNSET"); ok || n != 0 {
		t.Fatalf("未设置的 env 应返回 (0,false)，实际 (%d,%v)", n, ok)
	}
	t.Setenv("GEAR_TEST_INT_UNSET", "42")
	if n, ok := intFromEnv("GEAR_TEST_INT_UNSET"); !ok || n != 42 {
		t.Fatalf("合法整数应解析，实际 (%d,%v)", n, ok)
	}
	t.Setenv("GEAR_TEST_INT_UNSET", "not-a-number")
	if n, ok := intFromEnv("GEAR_TEST_INT_UNSET"); ok || n != 0 {
		t.Fatalf("非法整数应回落 (0,false)，实际 (%d,%v)", n, ok)
	}
	if d := durFromEnvSec("GEAR_TEST_DUR_UNSET", 7*time.Second); d != 7*time.Second {
		t.Fatalf("未设置应回落默认: %v", d)
	}
	t.Setenv("GEAR_TEST_DUR_UNSET", "3")
	if d := durFromEnvSec("GEAR_TEST_DUR_UNSET", 7*time.Second); d != 3*time.Second {
		t.Fatalf("秒级 env 应生效: %v", d)
	}
	t.Setenv("GEAR_TEST_DUR_UNSET", "-1")
	if d := durFromEnvSec("GEAR_TEST_DUR_UNSET", 7*time.Second); d != 7*time.Second {
		t.Fatalf("非正数应回落默认（否则会造出 0 超时）: %v", d)
	}
}

// ---- 流式协议解析 ----

func TestParseOpenAILine(t *testing.T) {
	if _, done, rec := parseOpenAILine([]byte("event: ping")); rec || done {
		t.Fatal("非 data: 前缀的行应被忽略")
	}
	if _, done, rec := parseOpenAILine([]byte("data: ")); rec || done {
		t.Fatal("空 payload 应被忽略")
	}
	if _, done, rec := parseOpenAILine([]byte("data: [DONE]")); !rec || !done {
		t.Fatal("data: [DONE] 必须是「已识别且结束」")
	}
	if _, _, rec := parseOpenAILine([]byte("data: {not json")); rec {
		t.Fatal("非法 JSON 应被忽略（recognized=false）")
	}
	if _, _, rec := parseOpenAILine([]byte(`data: {"choices":[]}`)); rec {
		t.Fatal("无 choices 的行应被忽略")
	}
	d, done, rec := parseOpenAILine([]byte(`data: {"choices":[{"delta":{"content":"hi","reasoning_content":"think"}}]}`))
	if !rec || done || d.content != "hi" || d.reasoning != "think" {
		t.Fatalf("增量解析不符: %+v done=%v rec=%v", d, done, rec)
	}
}

func TestParseOllamaLine(t *testing.T) {
	if _, _, rec := parseOllamaLine([]byte("   ")); rec {
		t.Fatal("空行应被忽略")
	}
	if _, _, rec := parseOllamaLine([]byte("{oops")); rec {
		t.Fatal("非法 JSON 应被忽略")
	}
	d, done, rec := parseOllamaLine([]byte(`{"message":{"content":"hi"},"done":false}`))
	if !rec || done || d.content != "hi" {
		t.Fatalf("增量解析不符: %+v done=%v rec=%v", d, done, rec)
	}
	d2, done2, _ := parseOllamaLine([]byte(`{"message":{"content":"bye"},"done":true}`))
	if !done2 || d2.content != "bye" {
		t.Fatal("done=true 应结束且保留最后增量")
	}
}

// ---- readStream 的两道闸 ----

func TestReadStreamJoinsDeltas(t *testing.T) {
	body := strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"he\"}}]}\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"llo\"}}]}\n" +
		"data: [DONE]\n")
	got, err := readStream(context.Background(), body, 2*time.Second, 5*time.Second, parseOpenAILine)
	if err != nil || got != "hello" {
		t.Fatalf("流式拼接不符: (%q, %v)", got, err)
	}
}

func TestReadStreamPropagatesScannerError(t *testing.T) {
	// io.Reader 返回错误 → 必须上抛，不能「读到一半当成功」。
	got, err := readStream(context.Background(), &errReader{}, time.Second, 5*time.Second, parseOpenAILine)
	if err == nil {
		t.Fatalf("底层读取错误必须上抛，实际返回 %q", got)
	}
}

func TestReadStreamIdleTimeoutKillsStalledStream(t *testing.T) {
	// 卡死判据：idle 内没有任何新增增量即判死（连接假死的典型症状）。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"start\"}}]}\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(3 * time.Second) // 之后再也不吐
	}))
	defer srv.Close()

	cfg := llmConfig{Provider: "openai", BaseURL: srv.URL, Idle: 300 * time.Millisecond, Total: 10 * time.Second}
	_, err := postChat(context.Background(), cfg, "", "hi")
	if err == nil || !strings.Contains(err.Error(), "空闲超时") {
		t.Fatalf("卡死流必须触发空闲超时，实际 %v", err)
	}
}

func TestReadStreamTotalTimeoutKillsTrickle(t *testing.T) {
	// 滴流判据：持续吐 token 不该被 idle 误杀，但超过总时长必须终止。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		f, _ := w.(http.Flusher)
		for i := 0; i < 1000; i++ {
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n"))
			f.Flush()
			time.Sleep(20 * time.Millisecond)
		}
	}))
	defer srv.Close()

	cfg := llmConfig{Provider: "openai", BaseURL: srv.URL, Idle: 2 * time.Second, Total: 300 * time.Millisecond}
	_, err := postChat(context.Background(), cfg, "", "hi")
	if err == nil || !strings.Contains(err.Error(), "总时长超时") {
		t.Fatalf("滴流必须触发总时长超时，实际 %v", err)
	}
}

func TestReadStreamRejectsRepetitionLoop(t *testing.T) {
	// 重复死循环：内容尾部同一小段连续反复 → 报错，而不是把垃圾当结果。
	frag := "同样的一句话反复说"
	var sb strings.Builder
	for i := 0; i < 20; i++ {
		sb.WriteString("data: {\"choices\":[{\"delta\":{\"content\":\"" + frag + "\"}}]}\n")
	}
	_, err := readStream(context.Background(), strings.NewReader(sb.String()), 2*time.Second, 10*time.Second, parseOpenAILine)
	if err == nil || !strings.Contains(err.Error(), "重复输出") {
		t.Fatalf("重复输出必须被检出，实际 %v", err)
	}

	// 推理片段（reasoning_content）陷入死循环同样要检出——它不进最终结果，
	// 但会一直烧 token，属于必须判死的一类。
	var rb strings.Builder
	for i := 0; i < 20; i++ {
		rb.WriteString("data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"" + frag + "\"}}]}\n")
	}
	if _, err := readStream(context.Background(), strings.NewReader(rb.String()), 2*time.Second, 10*time.Second, parseOpenAILine); err == nil {
		t.Fatal("推理片段死循环也必须被检出")
	}
}

func TestReadStreamHonorsContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		f, _ := w.(http.Flusher)
		for i := 0; i < 500; i++ {
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"y\"}}]}\n"))
			f.Flush()
			time.Sleep(10 * time.Millisecond)
		}
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(150 * time.Millisecond); cancel() }()
	cfg := llmConfig{Provider: "openai", BaseURL: srv.URL, Idle: 5 * time.Second, Total: 60 * time.Second}
	if _, err := postChat(ctx, cfg, "", "hi"); err == nil {
		t.Fatal("ctx 取消必须让流式读取退出")
	}
}

// ---- 两个 provider 的协议面 ----

func TestPostOpenAISendsExpectedRequestAndJoins(t *testing.T) {
	var gotBody map[string]any
	var gotAuth, gotAccept, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotAccept = r.Header.Get("Accept")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ab\"}}]}\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"cd\"}}]}\n"))
		_, _ = w.Write([]byte("data: [DONE]\n"))
	}))
	defer srv.Close()

	cfg := llmConfig{Provider: "siliconflow", BaseURL: srv.URL, APIKey: "k1", Model: "m1",
		Temperature: 0.7, MaxTokens: 99, Idle: 2 * time.Second, Total: 10 * time.Second}
	out, err := postChat(context.Background(), cfg, "sys", "user")
	if err != nil || out != "abcd" {
		t.Fatalf("OpenAI 兼容流拼接不符: (%q, %v)", out, err)
	}
	if gotPath != "/chat/completions" {
		t.Fatalf("路径应为 /chat/completions，实际 %q", gotPath)
	}
	if gotAuth != "Bearer k1" || gotAccept != "text/event-stream" {
		t.Fatalf("请求头不符: auth=%q accept=%q", gotAuth, gotAccept)
	}
	if gotBody["model"] != "m1" || gotBody["stream"] != true || gotBody["max_tokens"].(float64) != 99 {
		t.Fatalf("请求体不符: %v", gotBody)
	}
	if _, ok := gotBody["chat_template_kwargs"]; ok {
		t.Fatal("未显式设置 Thinking 时不得下发 chat_template_kwargs（沿用模型默认）")
	}
	msgs, _ := gotBody["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("system + user 两条消息，实际 %d", len(msgs))
	}
}

func TestPostOpenAISendsThinkingOnlyWhenExplicit(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"z\"}}]}\ndata: [DONE]\n"))
	}))
	defer srv.Close()

	yes := true
	cfg := llmConfig{Provider: "siliconflow", BaseURL: srv.URL, Model: "m", Thinking: &yes,
		Idle: 2 * time.Second, Total: 10 * time.Second}
	if _, err := postChat(context.Background(), cfg, "", "u"); err != nil {
		t.Fatalf("postChat 失败: %v", err)
	}
	kw, ok := gotBody["chat_template_kwargs"].(map[string]any)
	if !ok || kw["enable_thinking"] != true {
		t.Fatalf("siliconflow 且显式 Thinking=true 时应下发 chat_template_kwargs，实际 %v", gotBody)
	}
}

func TestPostRejectsHTTPErrorAndEmptyStream(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		_, _ = w.Write([]byte("bad key"))
	}))
	defer bad.Close()
	cfg := llmConfig{Provider: "openai", BaseURL: bad.URL, Idle: time.Second, Total: 5 * time.Second}
	if _, err := postChat(context.Background(), cfg, "", "u"); err == nil || !strings.Contains(err.Error(), "LLM HTTP 401") {
		t.Fatalf("非 2xx 必须报错并带状态码，实际 %v", err)
	}

	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("data: [DONE]\n"))
	}))
	defer empty.Close()
	cfg.BaseURL = empty.URL
	if _, err := postChat(context.Background(), cfg, "", "u"); err == nil || !strings.Contains(err.Error(), "为空") {
		t.Fatalf("空流必须报错（不得返回空文本当成功），实际 %v", err)
	}
}

func TestPostOllamaNativeProtocol(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		_, _ = w.Write([]byte(`{"message":{"content":"he"},"done":false}` + "\n"))
		_, _ = w.Write([]byte(`{"message":{"content":"llo"},"done":true}` + "\n"))
	}))
	defer srv.Close()

	cfg := llmConfig{Provider: "ollama", BaseURL: srv.URL, Model: "qwen3:latest",
		Idle: 2 * time.Second, Total: 10 * time.Second}
	out, err := postChat(context.Background(), cfg, "", "u")
	if err != nil || out != "hello" {
		t.Fatalf("Ollama 原生流拼接不符: (%q, %v)", out, err)
	}
	if gotPath != "/api/chat" {
		t.Fatalf("ollama 应走 /api/chat，实际 %q", gotPath)
	}
	if gotBody["model"] != "qwen3:latest" || gotBody["stream"] != true {
		t.Fatalf("ollama 请求体不符: %v", gotBody)
	}

	// ollama 的 HTTP 错误与空流同样要 fail-closed
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte("boom"))
	}))
	defer bad.Close()
	cfg.BaseURL = bad.URL
	if _, err := postChat(context.Background(), cfg, "", "u"); err == nil || !strings.Contains(err.Error(), "Ollama HTTP 500") {
		t.Fatalf("ollama 非 2xx 必须报错，实际 %v", err)
	}
	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("{\"done\":true}\n"))
	}))
	defer empty.Close()
	cfg.BaseURL = empty.URL
	if _, err := postChat(context.Background(), cfg, "", "u"); err == nil {
		t.Fatal("ollama 空流必须报错")
	}
}

// ---- 重复检测 / 小工具 ----

func TestDetectRepetition(t *testing.T) {
	if loop, _ := detectRepetition("短文本"); loop {
		t.Fatal("短文本不该被判重复（否则正常 JSON 的 {} 也会误报）")
	}
	if loop, _ := detectRepetition(strings.Repeat("甲乙丙丁", 60)); !loop {
		t.Fatal("字符块级重复（≥8 次且总长 ≥120）应被检出")
	}
	long := "这一整行句子长度足够长不会触发字符块级判定"
	if loop, _ := detectRepetition(strings.Repeat(long+"\n", 10)); !loop {
		t.Fatal("行级重复（同一整行 ≥6 次）应被检出")
	}
	var sb strings.Builder
	for i := 0; i < 40; i++ {
		sb.WriteString("第" + string(rune('a'+i%26)) + "段各不相同的正常文本。")
	}
	if loop, _ := detectRepetition(sb.String()); loop {
		t.Fatal("正常长文本不得被误判为重复")
	}
}

func TestRunesEqualAndClip(t *testing.T) {
	if !runesEqual([]rune("abc"), []rune("abc")) {
		t.Fatal("相同 rune 应相等")
	}
	if runesEqual([]rune("ab"), []rune("abc")) || runesEqual([]rune("abd"), []rune("abc")) {
		t.Fatal("长度或内容不同应不等")
	}
	if got := clip("短", 10); got != "短" {
		t.Fatalf("短文本不该截断: %q", got)
	}
	long := strings.Repeat("x", 20)
	if got := clip(long, 5); len([]rune(got)) != 6 || !strings.HasSuffix(got, "…") {
		t.Fatalf("长文本应截断并加省略号: %q", got)
	}
}

// ---- mock provider（离线/确定性）----

func TestMockChatTextBranches(t *testing.T) {
	if got := mockChatText(LLMRequest{System: "你是规划器"}); !strings.HasPrefix(got, "[") {
		t.Fatalf("规划器 system 应返回数组文本，实际 %q", got)
	}
	if got := mockChatText(LLMRequest{System: "原子能力清单"}); !strings.Contains(got, "runtime") {
		t.Fatalf("Tool 生成分支应返回工具定义，实际 %q", got)
	}
	if got := mockChatText(LLMRequest{System: "其它"}); !strings.Contains(got, "steps") {
		t.Fatalf("默认分支应返回 Flow 定义，实际 %q", got)
	}
}

func TestHTTPLLMCompleteUsesMockWithoutNetwork(t *testing.T) {
	// provider=mock：请求里声明即可，不必依赖环境变量。
	got, err := (&HTTPLLM{}).Complete(context.Background(), LLMRequest{Provider: "mock", System: "原子能力"})
	if err != nil || !strings.Contains(got, "runtime") {
		t.Fatalf("mock provider 应确定性返回: (%q, %v)", got, err)
	}
	// 真实 provider 指向不可达地址：必须报错（不静默返回空串）
	if _, err := (&HTTPLLM{}).Complete(context.Background(), LLMRequest{
		Provider: "openai", BaseURL: "http://127.0.0.1:1",
	}); err == nil {
		t.Fatal("不可达端点必须报错")
	}
}

func TestMockLLMRespectsRespondOverride(t *testing.T) {
	m := &MockLLM{}
	if got, _ := m.Complete(context.Background(), LLMRequest{System: "原子能力"}); !strings.Contains(got, "runtime") {
		t.Fatalf("未设 Respond 时应回退默认 mock 文本: %q", got)
	}
	m.Respond = func(req LLMRequest) string { return "custom:" + req.Prompt }
	if got, _ := m.Complete(context.Background(), LLMRequest{Prompt: "p"}); got != "custom:p" {
		t.Fatalf("Respond 覆盖未生效: %q", got)
	}
	if got := DefaultLLM(); got == nil {
		t.Fatal("DefaultLLM 不得返回 nil")
	}
}

// errReader 让 Scanner 立刻读到错误，用于验证「底层读错误必须上抛」。
type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
