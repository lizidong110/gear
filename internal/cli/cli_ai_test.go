package cli

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"gear/internal/engine"
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
		"--model", "m", "--base-url", "http://127.0.0.1:1", "--api-key", "k", "--allow", "db", "任务2"); err != nil {
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
		"used_tools": []string{"db.exec", "http.Request"},
		"steps": []map[string]interface{}{
			{"action": "db.exec", "observation": strings.Repeat("很长", 400)},
			{"action": "", "observation": "收尾"},
		},
	})
	if !strings.Contains(out, "最终答案") || !strings.Contains(out, "db.exec") {
		t.Fatalf("渲染应含答案与使用过的工具，实际:\n%s", out)
	}
	if !strings.Contains(out, "(Final Answer)") {
		t.Fatal("缺 action 的步骤应标注为 Final Answer")
	}
	if !strings.Contains(out, "...(truncated)") {
		t.Fatal("超长 observation 必须截断（否则撑爆终端/日志）")
	}

	// 恢复流水与缺口必须上屏（REQ-REACT-RECOVERY ⑤）：引擎记了、面不显示 = 没记。
	out2 := renderReactResult(map[string]interface{}{
		"answer":       "部分数据没拿到，如实说明",
		"recovery_log": []string{"L0 重试 http.Request（transient，第 1 次）：connection refused"},
		"gaps": []map[string]interface{}{
			{"action": "http.Request", "type": "transient", "reason": "connection refused", "human": false},
			{"action": "secret.get", "type": "auth", "reason": "secret 不存在", "human": true},
		},
	})
	if !strings.Contains(out2, "L0 重试") || !strings.Contains(out2, "恢复流水") {
		t.Fatalf("恢复流水必须渲染，实际:\n%s", out2)
	}
	if !strings.Contains(out2, "transient") || !strings.Contains(out2, "connection refused") {
		t.Fatalf("缺口清单必须渲染（类型+原因），实际:\n%s", out2)
	}
	if !strings.Contains(out2, "需人工介入") {
		t.Fatal("需人工的缺口必须显式标注（auth 类要人去配凭据，模型修不了）")
	}
	// 无缺口无流水时不得渲染空节（干净输出）。
	if out3 := renderReactResult(map[string]interface{}{"answer": "x"}); strings.Contains(out3, "缺口") || strings.Contains(out3, "恢复流水") {
		t.Fatalf("无缺口/流水时不应出现空节，实际:\n%s", out3)
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
		{"name": "db", "kind": "builtin", "methods": map[string]interface{}{"exec": nil}},
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

func TestAIRewooRequiresTask(t *testing.T) {
	err := runCLI(t, "ai", "rewoo")
	if err == nil || !strings.Contains(err.Error(), "任务描述") {
		t.Fatalf("缺任务描述时必须报错（不静默跑空规划），实际 %v", err)
	}
}

func TestAIRewooRejectsAllowAllWithAllow(t *testing.T) {
	// 与 react 同口径：两个 allow 语义冲突（不设限 vs 白名单），同给即报错，不静默挑一个
	err := runCLI(t, "ai", "rewoo", "--allow-all", "--allow", "http", "任务")
	if err == nil || !strings.Contains(err.Error(), "不能同时给") {
		t.Fatalf("--allow-all 与 --allow 同给必须报错，实际 %v", err)
	}
}

func TestAIRewooRunsWithMockProvider(t *testing.T) {
	withTempRegistry(t)
	t.Setenv("GEAR_AI_PROVIDER", "mock")
	// mock 的 planner 给一张单节点合法 DAG（template.render 是纯函数，不联网不要凭据），
	// 全链应跑通：planner → worker 执行 → solver → 结果渲染。断言命令本身正常结束。
	if err := runCLI(t, "ai", "rewoo", "调研三家供应商报价并对比"); err != nil {
		t.Fatalf("gear ai rewoo（mock）失败: %v", err)
	}
	// flag → RewooOpts 接线：模型参数 + allow 收窄 + replan-budget=0 + 并行度 + 节点上限 + 温度
	if err := runCLI(t, "ai", "rewoo", "--provider", "mock", "--model", "m",
		"--base-url", "http://127.0.0.1:1", "--api-key", "k", "--allow", "template",
		"--replan-budget", "0", "--max-parallel", "2", "--max-nodes", "5",
		"--temperature", "0.5", "任务2"); err != nil {
		t.Fatalf("带全参数的 rewoo 失败: %v", err)
	}
}

// TestAIRewooFlagTypoRejected 锁死「flag 漏 -- 被当位置参数」的防护。
// 2026-10-20 用户真实事故：`gear ai rewoo allow-all '任务'` 静默跑成 task="allow-all 任务"、
// allow 什么都没开。报错指出来比照跑便宜——这是防静默跑偏，不是防手滑。
func TestAIRewooFlagTypoRejected(t *testing.T) {
	err := runCLI(t, "ai", "rewoo", "allow-all", "帮我总结 PDF")
	if err == nil || !strings.Contains(err.Error(), "--allow-all") {
		t.Fatalf("allow-all 漏 -- 必须报错提示，实际 %v", err)
	}
	// react 同坑同防
	if err := runCLI(t, "ai", "react", "max-steps", "3", "任务"); err == nil ||
		!strings.Contains(err.Error(), "--max-steps") {
		t.Fatalf("react 的 flag 漏 -- 也必须报错，实际 %v", err)
	}
}

func TestDashFlagTypo(t *testing.T) {
	// helper 在 RunE 里收到的是**子命令**（flag 注册在它身上），测试也必须传子命令
	var cmd *cobra.Command
	for _, sub := range aiCmd().Commands() {
		if strings.HasPrefix(sub.Use, "rewoo") {
			cmd = sub
		}
	}
	if cmd == nil {
		t.Fatal("aiCmd 下找不到 rewoo 子命令")
	}
	// 带连字符的 flag 名：拦（用户真实踩的坑）
	if err := dashFlagTypo(cmd, []string{"allow-all", "任务"}); err == nil ||
		!strings.Contains(err.Error(), "--allow-all") {
		t.Fatalf("allow-all 漏 -- 必须报错，实际 %v", err)
	}
	// 单词 flag 名不拦：任务真可能以 model/temperature 这类词开头
	if err := dashFlagTypo(cmd, []string{"model", "是什么意思"}); err != nil {
		t.Fatalf("单词参数不应被误伤，实际 %v", err)
	}
	// 带连字符但非 flag 名：不拦
	if err := dashFlagTypo(cmd, []string{"re-plan", "一下"}); err != nil {
		t.Fatalf("非 flag 的连字符词不应被误伤，实际 %v", err)
	}
	// -- 分隔后整个任务是一个 token：不拦
	if err := dashFlagTypo(cmd, []string{"allow-all 帮我总结"}); err != nil {
		t.Fatalf("-- 后的整句任务不应被误伤，实际 %v", err)
	}
	// 无参数：不拦
	if err := dashFlagTypo(cmd, nil); err != nil {
		t.Fatalf("无参数不应报错，实际 %v", err)
	}
}

// TestAIRewooWallClockFlagRegistered 锁死墙钟 flag 的接线。2026-10-20 实测：重任务路径
// LLM 要跑 4~5 次（planner 1 + replanner 2 + solver 1，共 159s），默认 5 分钟墙钟在稍慢的
// 端点/模型下就被砍——而 CLI 原先没有任何 flag 能放宽，用户只能干等超时。
func TestAIRewooWallClockFlagRegistered(t *testing.T) {
	for _, sub := range aiCmd().Commands() {
		if strings.HasPrefix(sub.Use, "rewoo") {
			f := sub.Flags().Lookup("wall-clock")
			if f == nil {
				t.Fatal("rewoo 必须注册 --wall-clock：重任务/慢模型下用户唯一能放宽墙钟的口子")
			}
			if f.DefValue != "300" {
				t.Fatalf("--wall-clock 默认应为 300 秒，实际 %q", f.DefValue)
			}
			return
		}
	}
	t.Fatal("aiCmd 下找不到 rewoo 子命令")
}

// TestRenderRewooResult 覆盖 rewoo 的结果渲染：计划快照 / 节点结局 / 恢复流水 / 缺口
// 四段都必须在面上（REQ-REWOO⑨：plan 是数据，可存可 diff 可人工审——只活在内存里等于没审）。
func TestRenderRewooResult(t *testing.T) {
	out := renderRewooResult(map[string]interface{}{
		"answer": "A=100 B=120 C=90",
		"plan": []map[string]interface{}{
			{"id": "#E1", "action": "http.Request", "depends_on": []string{}, "status": "done"},
			{"id": "#E2", "action": "db.query", "depends_on": []string{"#E1"}, "status": "failed"},
			{"id": "#E3", "action": "template.render", "depends_on": []string{"#E1", "#E2"}, "status": "blocked"},
		},
		"node_outcomes": []map[string]interface{}{
			{"id": "#E1", "action": "http.Request", "status": "done", "attempts": 2},
			{"id": "#E2", "action": "db.query", "status": "failed", "attempts": 1,
				"error": strings.Repeat("连接被拒", 200), "type": "transient"},
			{"id": "#E3", "action": "template.render", "status": "blocked", "attempts": 0},
		},
		"recovery_log": []string{"L0 重试 db.query（transient，第 1 次）"},
		"gaps": []map[string]interface{}{
			{"action": "#E2（db.query）", "type": "transient", "reason": "connection refused", "human": false},
			{"action": "secret.get", "type": "auth", "reason": "secret 不存在", "human": true},
		},
	})
	for _, want := range []string{
		"REWOO 结果", "A=100 B=120 C=90",
		"#E1 [done]", "← 依赖 #E1", "#E1,#E2", // 计划快照：fan-in 依赖要看得见
		"重试 2 次", "transient", "...(truncated)", // 节点结局：重试次数 + 错误截断
		"L0 重试", "connection refused", "需人工介入", // 流水 + 缺口（含人工标注）
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("渲染应含 %q，实际:\n%s", want, out)
		}
	}
	// 四段全空时不得渲染空节（干净输出）
	if clean := renderRewooResult(map[string]interface{}{"answer": "x"}); strings.Contains(clean, "执行计划") ||
		strings.Contains(clean, "节点结局") || strings.Contains(clean, "缺口") || strings.Contains(clean, "恢复流水") {
		t.Fatalf("无内容时不应出现空节，实际:\n%s", clean)
	}
	// 非 map 形态 → 退化为 YAML（不猜结构）
	if y := renderRewooResult([]string{"a"}); !strings.Contains(y, "- a") {
		t.Fatalf("非 map 应退化为 YAML，实际 %q", y)
	}
}

// aiSubCmd 取 aiCmd 下指定前缀的子命令（flag 注册在它身上，helper 测试必须传它）。
func aiSubCmd(t *testing.T, use string) *cobra.Command {
	t.Helper()
	for _, sub := range aiCmd().Commands() {
		if strings.HasPrefix(sub.Use, use) {
			return sub
		}
	}
	t.Fatalf("aiCmd 下找不到 %q 子命令", use)
	return nil
}

// TestAIAllowNamesExpandsGroups 钉住三个 allow 维度的组装语义：
// --allow（细列表）可叠加在两个组开关上，最终汇成一份白名单（组是糖不是新机制）。
func TestAIAllowNamesExpandsGroups(t *testing.T) {
	rewoo := aiSubCmd(t, "rewoo")
	// 只给组：base 12 个原子能力 + flow 2 个（call/ext.registry）
	if err := rewoo.Flags().Set("allow-base", "true"); err != nil {
		t.Fatal(err)
	}
	if err := rewoo.Flags().Set("allow-flow", "true"); err != nil {
		t.Fatal(err)
	}
	names, all, err := aiAllowNames(rewoo)
	if err != nil || all {
		t.Fatalf("组开关不应触发 allowAll，实际 all=%v err=%v", all, err)
	}
	// 组开关在此只翻 token（展开逻辑在引擎层）：base+flow → 恰 2 个 token，顺序固定
	if len(names) != 2 || names[0] != engine.ReactAllowBase || names[1] != engine.ReactAllowFlow {
		t.Fatalf("base+flow 应翻成 [%s %s] 两个 token，实际 %v",
			engine.ReactAllowBase, engine.ReactAllowFlow, names)
	}
	// 组 + 细列表叠加：ext.pdf 一个不少地进来
	if err := rewoo.Flags().Set("allow", "ext.pdf"); err != nil {
		t.Fatal(err)
	}
	names, _, err = aiAllowNames(rewoo)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, n := range names {
		if n == "ext.pdf" {
			found = true
		}
	}
	if !found {
		t.Fatalf("--allow 的细列表项应叠加进组展开结果，实际 %v", names)
	}
	// 什么都不给：空名单（走默认集），不是全开放
	rewoo2 := aiSubCmd(t, "rewoo")
	names, all, err = aiAllowNames(rewoo2)
	if err != nil || all || len(names) != 0 {
		t.Fatalf("无 allow 来源应返回空名单+非全开放，实际 names=%v all=%v err=%v", names, all, err)
	}
}

// TestAIAllowAllConflictsWithGroups：--allow-all 与三个白名单来源任一互斥。
// 全开放 vs 白名单语义冲突，同给必须报错——静默挑一个会让调用方以为开了/没开。
func TestAIAllowAllConflictsWithGroups(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  map[string]string
	}{
		{"与 --allow", map[string]string{"allow": "http"}},
		{"与 --allow-base", map[string]string{"allow-base": "true"}},
		{"与 --allow-flow", map[string]string{"allow-flow": "true"}},
	} {
		cmd := aiSubCmd(t, "rewoo")
		if err := cmd.Flags().Set("allow-all", "true"); err != nil {
			t.Fatal(err)
		}
		for k, v := range tc.set {
			if err := cmd.Flags().Set(k, v); err != nil {
				t.Fatal(err)
			}
		}
		_, _, err := aiAllowNames(cmd)
		if err == nil || !strings.Contains(err.Error(), "不能同时给") {
			t.Fatalf("%s 同给必须报错，实际 %v", tc.name, err)
		}
	}
	// react 同口径（两个子命令共用 helper，行为必须一致）
	cmd := aiSubCmd(t, "react")
	if err := cmd.Flags().Set("allow-all", "true"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Flags().Set("allow-base", "true"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := aiAllowNames(cmd); err == nil || !strings.Contains(err.Error(), "不能同时给") {
		t.Fatalf("react 的 --allow-all 与 --allow-base 同给必须报错，实际 %v", err)
	}
}

// TestAIAllowGroupsFlagTypoGuard：新 flag 名带连字符，同样纳入 dashFlagTypo 防护
// （漏 -- 会被 cobra 当位置参数静默吃进 task——2026-10-20 用户真实事故同型）。
func TestAIAllowGroupsFlagTypoGuard(t *testing.T) {
	cmd := aiSubCmd(t, "rewoo")
	for _, flag := range []string{"allow-base", "allow-flow"} {
		if err := dashFlagTypo(cmd, []string{flag, "任务"}); err == nil ||
			!strings.Contains(err.Error(), "--"+flag) {
			t.Fatalf("%s 漏 -- 必须报错，实际 %v", flag, err)
		}
	}
}

// TestAIRewooRunsWithAllowGroups：mock provider 下端到端验证组开关接线
// （flag → aiAllowNames → RewooOpts.Allow → resolveRewooAllow 全链不断）。
// 细列项用 template（pure 恒装配）而非 ext.pdf：CLI 测试的 CWD 是 internal/cli，
// 找不到 plugin/extensions.yaml，ext.pdf 这类侧车能力在该环境确实未装配——
// strict 解析报错是正确行为（见 resolveRewooAllow 注释），不是缺陷。
func TestAIRewooRunsWithAllowGroups(t *testing.T) {
	withTempRegistry(t)
	t.Setenv("GEAR_AI_PROVIDER", "mock")
	if err := runCLI(t, "ai", "rewoo", "--allow-base", "--allow-flow", "--allow", "template", "任务"); err != nil {
		t.Fatalf("gear ai rewoo --allow-base --allow-flow --allow template 失败: %v", err)
	}
}
