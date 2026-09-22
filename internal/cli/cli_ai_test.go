package cli

import (
	"strings"
	"testing"
)

// 本文件覆盖 `gear ai` 子命令（ai.go）——此前整文件只有 seedRefRegistry 一个用例，
// 命令主体（react / capabilities / gen）几乎 0% 覆盖。
//
// 全部走 **mock provider**（GEAR_AI_PROVIDER=mock）：模型层在 provider=mock 时返回确定性
// 文本、不联网（见 internal/engine/ai_llm.go），因此这些用例在离线环境也能跑，
// 不依赖任何真实 key。

// withTempRegistry 把本次测试用到的 registry 指向临时库，避免读写用户目录 ~/.gear/registry.db。
//
// ⚠ 它必须经 testRegistryOverride 生效（见其注释）：只改全局 registryPath 是**无效**的，
// newRootCmd 的 StringVar 会用默认值把它覆盖回去。
func withTempRegistry(t *testing.T) string {
	t.Helper()
	db := tempDB(t)
	oldOverride, oldPath := testRegistryOverride, registryPath
	testRegistryOverride, registryPath = db, db
	t.Cleanup(func() { testRegistryOverride, registryPath = oldOverride, oldPath })
	return db
}

func TestAICapabilitiesPrintsCatalog(t *testing.T) {
	// capabilities 是纯目录渲染（不需要 LLM）：必须能直接跑通，
	// 这是「Agent 与 CLI 看同一份能力目录」的落脚点。
	if err := runCLI(t, "ai", "capabilities"); err != nil {
		t.Fatalf("gear ai capabilities 失败: %v", err)
	}
}

func TestAIReactRequiresTask(t *testing.T) {
	err := runCLI(t, "ai", "react")
	if err == nil || !strings.Contains(err.Error(), "任务描述") {
		t.Fatalf("缺任务描述时必须报错（不静默跑空循环），实际 %v", err)
	}
}

func TestAIReactRunsWithMockProvider(t *testing.T) {
	t.Setenv("GEAR_AI_PROVIDER", "mock")
	// mock 模型给不出合法 Action/Final Answer → 走「回灌格式要求」分支并跑满步数，
	// 命令本身应正常结束并打印结果（不把模型问题当命令失败）。
	if err := runCLI(t, "ai", "react", "--max-steps", "1", "做个任务"); err != nil {
		t.Fatalf("gear ai react 失败: %v", err)
	}
	// 显式给模型参数也应跑通（flag → opts 的接线）
	if err := runCLI(t, "ai", "react", "--max-steps", "1", "--provider", "mock",
		"--model", "m", "--base-url", "http://127.0.0.1:1", "--api-key", "k", "--allow", "kv", "任务2"); err != nil {
		t.Fatalf("带模型参数的 react 失败: %v", err)
	}
}

func TestAIGenRequiresIntent(t *testing.T) {
	withTempRegistry(t)
	err := runCLI(t, "ai", "gen", "flow")
	if err == nil || !strings.Contains(err.Error(), "意图") {
		t.Fatalf("缺意图时必须报错，实际 %v", err)
	}
}

func TestAIGenFlowAndToolWithMockProvider(t *testing.T) {
	withTempRegistry(t)
	t.Setenv("GEAR_AI_PROVIDER", "mock")

	// mock 模型的输出不保证通过静态校验，故这里只断言「命令跑完且错误（若有）是生成链路
	// 自身的问题」，重点是把 runAIGen 的装配链路（持久 registry → 内存 refReg → 引擎 →
	// agent → 草稿渲染）走通——这一段此前完全没人测。
	err := runCLI(t, "ai", "gen", "flow", "--intent", "把结果发到 Slack")
	if err != nil && !strings.Contains(err.Error(), "AI 生成失败") {
		t.Fatalf("gear ai gen flow 的失败应来自生成链路本身，实际 %v", err)
	}
	_ = runCLI(t, "ai", "gen", "tool", "-i", "生成一个抓取网页的工具")
	_ = runCLI(t, "ai", "gen", "flow", "-i", "带 --register 的流程", "--register")
}

func TestRenderHelpers(t *testing.T) {
	// renderReactResult：map 形态 → 分段渲染（answer / used_tools / steps）
	out := renderReactResult(map[string]interface{}{
		"answer":     "最终答案",
		"used_tools": []string{"kv.put", "http.Request"},
		"steps": []map[string]interface{}{
			{"action": "kv.put", "observation": strings.Repeat("很长", 400)},
			{"action": "", "observation": "收尾"},
		},
	})
	if !strings.Contains(out, "最终答案") || !strings.Contains(out, "kv.put") {
		t.Fatalf("渲染应含答案与使用过的工具，实际:\n%s", out)
	}
	if !strings.Contains(out, "(Final Answer)") {
		t.Fatal("缺 action 的步骤应标注为 Final Answer")
	}
	if !strings.Contains(out, "...(truncated)") {
		t.Fatal("超长 observation 必须截断（否则撑爆终端/日志）")
	}

	// 非 map 形态 → 退化为 YAML（不猜结构）
	yamlOut := renderReactResult([]string{"a", "b"})
	if !strings.Contains(yamlOut, "- a") {
		t.Fatalf("非 map 应退化为 YAML，实际 %q", yamlOut)
	}

	// renderCapabilities：非 []map 形态 → 退化为 YAML
	if got := renderCapabilities("not a list"); !strings.Contains(got, "not a list") {
		t.Fatalf("非列表形态应退化为 YAML，实际 %q", got)
	}
	if got := renderCapabilities([]map[string]interface{}{
		{"name": "kv", "kind": "builtin", "methods": map[string]interface{}{"put": nil}},
		{"name": "crypto", "kind": "pure", "methods": map[string]interface{}{"hash": nil}},
		{"name": "ext.ai", "kind": "ext", "methods": map[string]interface{}{"complete": nil}},
	}); !strings.Contains(got, "1 个门禁原语") || !strings.Contains(got, "1 个纯函数原子") || !strings.Contains(got, "1 个扩展原语") {
		t.Fatalf("能力目录统计不符，实际:\n%s", got)
	}
}

func TestTruncateForCLI(t *testing.T) {
	if got := truncateForCLI("短", 10); got != "短" {
		t.Fatalf("未超长不应截断，实际 %q", got)
	}
	got := truncateForCLI(strings.Repeat("汉", 10), 3)
	if len([]rune(got)) != 3+len("...(truncated)") || !strings.HasSuffix(got, "...(truncated)") {
		t.Fatalf("应按 rune 截断并标注，实际 %q", got)
	}
}
