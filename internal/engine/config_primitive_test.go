package engine

import (
	"fmt"
	"path/filepath"
	"testing"

	"gear/internal/cap"
)

// testConfigStore 造一个临时配置库（与 cap 包测试同口径：t.Cleanup 关闭，避免句柄泄漏）。
func testConfigStore(t *testing.T) *cap.ConfigStore {
	t.Helper()
	s := cap.NewConfigStore(filepath.Join(t.TempDir(), "config.db"))
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestLuaCapConfigUngranted 负向：未声明 config 时 ctx.config 根本不存在（最小权限）。
func TestLuaCapConfigUngranted(t *testing.T) {
	host := luaHost(t, nil, &cap.Providers{})
	_, err := luaRunHost(t, `return ctx.config.get({ key = "ai.model" })`, nil, host)
	if err == nil {
		t.Fatal("未声明 config 时访问 ctx.config 应报错")
	}
}

// TestLuaCapConfigUnavailable 负向：声明了 config 但宿主没装配实现 → ErrCapabilityUnavailable，
// 而不是静默退化成「内存里存一下」（配置静默丢失比报错难查得多）。
func TestLuaCapConfigUnavailable(t *testing.T) {
	host := luaHost(t, []string{"config"}, &cap.Providers{})
	_, err := luaRunHost(t, `return ctx.config.get({ key = "ai.model" })`, nil, host)
	if err == nil {
		t.Fatal("宿主未装配 config 时应报错")
	}
}

// TestLuaCapConfig 正向：声明 + 装配后，set/get/list/delete 在 Lua 侧完整往返。
// 这是「应用读写配置到 db」这条能力的可执行证明——应用不必知道库在哪。
func TestLuaCapConfig(t *testing.T) {
	s := testConfigStore(t)
	host := luaHost(t, []string{"config"}, &cap.Providers{Config: s})
	out, err := luaRunHost(t, `
		ctx.config.set({ key = "app.demo.theme", value = "dark" })
		ctx.config.set({ key = "app.demo.lang", value = "zh" })
		local v = ctx.config.get({ key = "app.demo.theme" })
		-- list 返回 {key = value}（map，不是数组），故按下键取值而非用 # 计数
		local all = ctx.config.list({ prefix = "app.demo." })
		ctx.config.delete({ key = "app.demo.lang" })
		local left = ctx.config.list({ prefix = "app.demo." })
		return {
			v = v,
			all_theme = all["app.demo.theme"],
			all_lang = all["app.demo.lang"],
			left_lang = left["app.demo.lang"],
		}
	`, nil, host)
	if err != nil {
		t.Fatalf("ctx.config 往返失败: %v", err)
	}
	if fmt.Sprint(out["v"]) != "dark" {
		t.Fatalf("ctx.config.get = %v, 想要 dark", out["v"])
	}
	if fmt.Sprint(out["all_theme"]) != "dark" || fmt.Sprint(out["all_lang"]) != "zh" {
		t.Fatalf("list 应包含两项 app.demo.*: %v", out)
	}
	if out["left_lang"] != nil {
		t.Fatalf("delete 后 list 仍返回 app.demo.lang: %v", out)
	}
}

// TestLuaCapConfigGetMissingReturnsEmpty 锁死 ctx.config.get 的「不存在即空」语义：
// KV GET 不存在必须平返回空串（不抛错），否则像 mindstorm settings_get 这类「缺省返回空串」
// 的调用会在空库上整 Tool 失败（2026-09-25 实测）。Get 的 ErrConfigNotFound 语义（区分
// 「没配」与「用户故意留空」）是给 Go 侧 resolveLLMConfig 回退链用的，Lua 侧取值要平滑。
func TestLuaCapConfigGetMissingReturnsEmpty(t *testing.T) {
	s := testConfigStore(t)
	host := luaHost(t, []string{"config"}, &cap.Providers{Config: s})
	out, err := luaRunHost(t, `
		local v = ctx.config.get({ key = "no.such.key" })
		return { v = v }
	`, nil, host)
	if err != nil {
		t.Fatalf("config.get 不存在时应平返回空串，不应报错: %v", err)
	}
	if fmt.Sprint(out["v"]) != "" {
		t.Fatalf("config.get 不存在应返回空串，实际 %q", out["v"])
	}
}

// TestConfigLookupFrom 守住适配层语义：未配置 → (_, false)，交给下一级回退；
// 配置了空串 → ("", true)？不——空串在 resolveLLMConfig 里被当未配置，故这里只需保证
// 「存在且有值」时如实返回 true。
func TestConfigLookupFrom(t *testing.T) {
	s := testConfigStore(t)
	if err := s.Set("ai.model", "m1"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	lookup := configLookupFrom(s)
	if v, ok := lookup("ai.model"); !ok || v != "m1" {
		t.Fatalf("lookup(ai.model) = (%q,%v), 想要 (m1,true)", v, ok)
	}
	if _, ok := lookup("ai.nope"); ok {
		t.Fatal("lookup 未配置项应返回 false（让下一级回退生效）")
	}
}

// TestResolveLLMConfigConfigStoreLayer 守住新加的第三层回退：
// 调用参数 > 环境变量 > 配置库 > 内置默认。
//
// 顺序里「配置库在环境变量之后」是刻意的：env 用于临时覆盖，配置库是持久默认。
// 这条测试同时是「不改变既有行为」的证明——前两层优先级原样保留。
func TestResolveLLMConfigConfigStoreLayer(t *testing.T) {
	s := testConfigStore(t)
	for k, v := range map[string]string{
		"ai.provider":   "openai",
		"ai.model":      "cfg-model",
		"ai.api_key":    "cfg-key",
		"ai.base_url":   "http://cfg",
		"ai.max_tokens": "8192",
	} {
		if err := s.Set(k, v); err != nil {
			t.Fatalf("Set(%s): %v", k, err)
		}
	}
	lookup := configLookupFrom(s)

	// 清掉环境变量这一层，确保测的是配置库。
	for _, k := range []string{"GEAR_AI_PROVIDER", "GEAR_AI_MODEL", "GEAR_AI_API_KEY", "GEAR_AI_BASE_URL", "GEAR_AI_MAX_TOKENS"} {
		t.Setenv(k, "")
	}

	cfg := resolveLLMConfig(LLMRequest{}, lookup)
	if cfg.Model != "cfg-model" || cfg.APIKey != "cfg-key" || cfg.BaseURL != "http://cfg" || cfg.Provider != "openai" {
		t.Fatalf("配置库层未生效: %+v", cfg)
	}
	if cfg.MaxTokens != 8192 {
		t.Fatalf("MaxTokens = %d, 想要 8192（ai.max_tokens 应被读取）", cfg.MaxTokens)
	}

	// 环境变量优先于配置库。
	t.Setenv("GEAR_AI_MODEL", "env-model")
	if got := resolveLLMConfig(LLMRequest{}, lookup); got.Model != "env-model" {
		t.Fatalf("环境变量应优先于配置库, got Model = %q", got.Model)
	}

	// 调用参数最优先（覆盖前两层）。
	if got := resolveLLMConfig(LLMRequest{Model: "call-model"}, lookup); got.Model != "call-model" {
		t.Fatalf("调用参数应最优先, got Model = %q", got.Model)
	}
}

// TestResolveLLMConfigNilLookupUnchanged 负向：lookup 为 nil 时行为必须与改动前完全一致
// （未装配 config 的宿主、以及全部单测走的都是这条路径）。
func TestResolveLLMConfigNilLookupUnchanged(t *testing.T) {
	for _, k := range []string{"GEAR_AI_PROVIDER", "GEAR_AI_MODEL", "GEAR_AI_API_KEY", "GEAR_AI_BASE_URL"} {
		t.Setenv(k, "")
	}
	cfg := resolveLLMConfig(LLMRequest{}, nil)
	if cfg.Provider != "siliconflow" || cfg.Model != "Qwen/Qwen3-8B" || cfg.APIKey != "" {
		t.Fatalf("nil lookup 时不应有配置库介入: %+v", cfg)
	}
	if cfg.MaxTokens != 4096 {
		t.Fatalf("MaxTokens = %d, 想要内置默认 4096", cfg.MaxTokens)
	}
}

// TestAIAgentSetConfigStore 守住注入边界：只对 HTTPLLM 生效；MockLLM 是离线确定性实现，
// 让它读本机配置会让测试结果随开发者机器漂移——故必须不受影响。
func TestAIAgentSetConfigStore(t *testing.T) {
	s := testConfigStore(t)
	if err := s.Set("ai.model", "from-config"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	a := newAIAgent(nil, DefaultLLM(), nil, nil)
	a.SetConfigStore(s)
	h, ok := a.llm.(*HTTPLLM)
	if !ok {
		t.Fatal("DefaultLLM 应是 *HTTPLLM")
	}
	if h.Config == nil {
		t.Fatal("SetConfigStore 后 HTTPLLM 应带上配置源")
	}
	if v, ok := h.Config("ai.model"); !ok || v != "from-config" {
		t.Fatalf("配置源读取 = (%q,%v), 想要 (from-config,true)", v, ok)
	}

	m := newAIAgent(nil, &MockLLM{}, nil, nil)
	m.SetConfigStore(s)
	if _, ok := m.llm.(*MockLLM); !ok {
		t.Fatal("MockLLM 不应被替换成带配置的 HTTPLLM（测试确定性优先）")
	}
	// nil 是合法输入（宿主没装配 config 即跳过），不应 panic。
	m.SetConfigStore(nil)
}

// TestEngineWithConfigStoreBindsAI 端到端守住「装配一处、两处生效」：
// WithConfigStore 既让 ctx.config 可用，也让 ext.ai 的缺省配置落到同一个库。
// 分开装配必然漂移（「界面配了、CLI 读不到」这类故障极难排查）。
func TestEngineWithConfigStoreBindsAI(t *testing.T) {
	s := testConfigStore(t)
	if err := s.Set("ai.model", "bound-model"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	eng := New(nil, WithConfigStore(s), WithAIAgent(newAIAgent(nil, DefaultLLM(), nil, nil)))
	if eng.host == nil || eng.host.Config == nil {
		t.Fatal("WithConfigStore 未注入 host.Config")
	}
	if eng.ai == nil {
		t.Fatal("ai 未注入")
	}
	h, ok := eng.ai.llm.(*HTTPLLM)
	if !ok {
		t.Fatal("ai 的 llm 应是 *HTTPLLM")
	}
	if h.Config == nil {
		t.Fatal("装配 config 后，ext.ai 应自动以它为缺省配置源")
	}
	if v, _ := h.Config("ai.model"); v != "bound-model" {
		t.Fatalf("ext.ai 读到的 ai.model = %q, 想要 bound-model", v)
	}
}
