package engine

// 本文件覆盖 react 的失败恢复层（REQ-REACT-RECOVERY，实现见 ai_recovery.go）。
// 设计来源是 REWOO-with-recovery：七类失败分类 + 分层恢复（L0 机械重试 / L1.5 换通道 /
// L1 结构化信号 / 兜底缺口）+ 黑板可观测。每条护栏的判据都是「不静默」：
//   - 失败必须分类（判不出归 unrecoverable，绝不吞）；
//   - transient 才机械重试，bad_input 重试同样输入是白烧；
//   - 连续同型失败必须封禁 + 记缺口（震荡守卫），不许烧完预算才兜底；
//   - 缺口必须进结果（gaps）并让模型在作答前知道（gap notice）。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// fakeRecoverableErr 实现 RecoverableError：验证数据驱动分类钩子优先于字符串启发式。
type fakeRecoverableErr struct {
	msg   string
	class FailureClass
}

func (e fakeRecoverableErr) Error() string               { return e.msg }
func (e fakeRecoverableErr) RecoveryClass() FailureClass { return e.class }

// TestClassifyToolFailureCoversSevenClasses：七类各至少一例 + 兜底绝不吞。
// 兜底方向是 unrecoverable 而不是 transient：把未知错误当可恢复会在确定性错误上白烧预算，
// 当不可恢复只损失一次重试机会，却保住「不静默」。
func TestClassifyToolFailureCoversSevenClasses(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want FailureClass
	}{
		{"超时归 transient", errors.New("Get \"http://x\": i/o timeout"), FailTransient},
		{"连接拒绝归 transient", errors.New("dial tcp 127.0.0.1:8096: connection refused"), FailTransient},
		{"429 归 transient（退避重试是标准解）", errors.New("429 too many requests"), FailTransient},
		{"503 归 transient", errors.New("503 service unavailable"), FailTransient},
		{"能力未装配归 channel", errors.New("capability 已授权但尚未实现"), FailChannel},
		{"缺必填参数归 bad_input", errors.New("db.exec 缺少必填参数 sql"), FailBadInput},
		{"SQL 语法错归 bad_input", errors.New("near \"SELCT\": syntax error"), FailBadInput},
		{"401 归 auth", errors.New("401 unauthorized"), FailAuth},
		{"密钥未播种归 auth（需人工）", errors.New("secret 不存在"), FailAuth},
		{"404 归 unrecoverable", errors.New("404 not found"), FailUnrecoverable},
		{"未知错误兜底 unrecoverable", errors.New("完全是没见过的错误"), FailUnrecoverable},
	}
	for _, c := range cases {
		got := classifyToolFailure("db.query", c.err)
		if got.Class != c.want {
			t.Errorf("%s：应为 %s，实际 %s（err=%v）", c.name, c.want, got.Class, c.err)
		}
		if got.Scope != "self" {
			t.Errorf("%s：scope 默认应为 self，实际 %q", c.name, got.Scope)
		}
		if got.Reason == "" {
			t.Errorf("%s：reason 必须保留原文（信号要能指原因）", c.name)
		}
	}
}

// TestClassifyToolFailureHonorsStructuredHook：错误自己声明类别时听它的（数据驱动），
// 即使文本看起来像别类——tool 实现最懂自己的错误，中央 heuristic 只做兜底。
func TestClassifyToolFailureHonorsStructuredHook(t *testing.T) {
	// 文本像 transient（"timeout"），但实现方声明是 auth——必须听实现的。
	err := fakeRecoverableErr{msg: "timeout while refreshing credentials", class: FailAuth}
	if got := classifyToolFailure("ext.demo.run", err); got.Class != FailAuth {
		t.Fatalf("结构化钩子应优先于字符串启发式，实际 %s", got.Class)
	}
	// 包一层的错误也要能探到（errors.Is/As 之外的 Unwrap 链）。
	wrapped := fmt.Errorf("step s1 执行失败: %w", err)
	if got := classifyToolFailure("ext.demo.run", wrapped); got.Class != FailAuth {
		t.Fatalf("包装后的结构化错误应仍被识别，实际 %s", got.Class)
	}
}

// TestRecoveryHelpers：空结果判定、可重试 HTTP 状态码、工具不支持判定。
func TestRecoveryHelpers(t *testing.T) {
	// 空结果：查无结果不是错误，只登记供震荡守卫计数。
	if !isEmptyToolResult(nil) {
		t.Error("nil 输出应判空")
	}
	if !isEmptyToolResult(map[string]interface{}{"result": []interface{}{}}) {
		t.Error("空数组应判空")
	}
	if !isEmptyToolResult(map[string]interface{}{"result": "  "}) {
		t.Error("空白字符串应判空")
	}
	if isEmptyToolResult(map[string]interface{}{"result": false}) {
		t.Error("false 是有效结果，不得判空")
	}
	if isEmptyToolResult(map[string]interface{}{"result": 0}) {
		t.Error("0 是有效结果，不得判空")
	}
	if isEmptyToolResult(map[string]interface{}{"result": "ok"}) {
		t.Error("非空字符串不得判空")
	}
	// 可重试状态码：ctx.http.Request 不把非 2xx 当错误，status 在 body 里。
	for _, code := range []float64{429, 502, 503, 504} {
		out := map[string]interface{}{"result": map[string]interface{}{"status": code, "body": "x"}}
		if _, ok := retryableHTTPStatus(out); !ok {
			t.Errorf("HTTP %d 应可重试", int(code))
		}
	}
	for _, code := range []float64{200, 400, 404, 500} {
		out := map[string]interface{}{"result": map[string]interface{}{"status": code}}
		if _, ok := retryableHTTPStatus(out); ok {
			t.Errorf("HTTP %d 不应机械重试", int(code))
		}
	}
	// 工具不支持判定刻意保守：缺工具语义词就不算（普通 400 不能被误判成降级）。
	if reactToolsUnsupported(errors.New("400 bad request: invalid parameter")) {
		t.Error("不含工具语义不得判「不支持工具」")
	}
	if reactToolsUnsupported(errors.New("timeout")) {
		t.Error("普通超时不得判「不支持工具」")
	}
	if !reactToolsUnsupported(errors.New("400: tools field is not supported by this endpoint")) {
		t.Error("明确的「不支持工具」应能识别")
	}
}

// TestReactRetriesTransientToolFailureMechanically：transient 失败先机械重试（L0），
// 不烧 LLM 轮次；重试成功则照常回灌结果。重复调用守卫拦的是模型重发，不是引擎重试。
func TestReactRetriesTransientToolFailureMechanically(t *testing.T) {
	llm := &scriptedLLM{texts: []string{
		actionJSON("db.exec", `{"sql":"SELECT 1"}`),
		"Final Answer: 重试后成功",
	}}
	a, entries := newReactAgent(t, llm)
	n := 0
	a.exec = func(_ context.Context, entry string, _ map[string]interface{}) (map[string]interface{}, error) {
		*entries = append(*entries, entry)
		n++
		if n <= 2 {
			return nil, errors.New("i/o timeout")
		}
		return map[string]interface{}{"result": "rows"}, nil
	}
	res, err := a.React(context.Background(), "任务", ReactOpts{MaxSteps: 3})
	if err != nil {
		t.Fatalf("React 失败: %v", err)
	}
	if n != 3 {
		t.Fatalf("transient 应机械重试到成功（1+2 次），实际执行 %d 次", n)
	}
	if len(*entries) != 3 {
		t.Fatalf("entries 应记录 3 次真实执行，实际 %v", *entries)
	}
	log, _ := res.(map[string]interface{})["recovery_log"].([]string)
	if len(log) != 2 {
		t.Fatalf("recovery_log 应记录 2 次 L0 重试，实际 %v", log)
	}
	// 模型侧只经历了 1 轮工具调用（重试不消耗它的步数/轮次）。
	if len(llm.got) != 2 {
		t.Fatalf("LLM 只应被问 2 轮（调用 + 收尾），实际 %d", len(llm.got))
	}
	steps, _ := res.(map[string]interface{})["steps"].([]map[string]interface{})
	if obs := fmt.Sprint(steps[0]["observation"]); !strings.Contains(obs, "rows") {
		t.Fatalf("重试成功后应回灌真实结果，实际 %q", obs)
	}
}

// TestReactHTTPStatusIsRetriedMechanically：HTTP 429/503 在 body 里（不当错误），
// 不接住的话 L0 对 HTTP 类工具等于不存在。
func TestReactHTTPStatusIsRetriedMechanically(t *testing.T) {
	llm := &scriptedLLM{texts: []string{
		actionJSON("http.Request", `{"url":"https://x"}`),
		"Final Answer: 拿到",
	}}
	a, entries := newReactAgent(t, llm)
	n := 0
	a.exec = func(_ context.Context, entry string, _ map[string]interface{}) (map[string]interface{}, error) {
		*entries = append(*entries, entry)
		n++
		if n <= 2 {
			return map[string]interface{}{"result": map[string]interface{}{"status": float64(503), "body": "busy"}}, nil
		}
		return map[string]interface{}{"result": map[string]interface{}{"status": float64(200), "body": "payload"}}, nil
	}
	res, err := a.React(context.Background(), "任务", ReactOpts{MaxSteps: 3})
	if err != nil {
		t.Fatalf("React 失败: %v", err)
	}
	if len(*entries) != 3 {
		t.Fatalf("503 应机械重试后成功，实际执行 %d 次", len(*entries))
	}
	log, _ := res.(map[string]interface{})["recovery_log"].([]string)
	if len(log) != 2 || !strings.Contains(log[0], "HTTP 状态码可重试") {
		t.Fatalf("recovery_log 应记录 HTTP 状态码重试，实际 %v", log)
	}
}

// TestReactBadInputIsNotRetriedMechanically：bad_input 机械重试同样输入毫无意义
// （REWOO 分类法的核心判据）——必须直接回灌信号让模型改，一次都不多执行。
func TestReactBadInputIsNotRetriedMechanically(t *testing.T) {
	llm := &scriptedLLM{texts: []string{
		actionJSON("db.exec", `{"sql":"SELECT 1"}`),
		"Final Answer: 放弃",
	}}
	a, entries := newReactAgent(t, llm)
	a.exec = func(_ context.Context, entry string, _ map[string]interface{}) (map[string]interface{}, error) {
		*entries = append(*entries, entry)
		return nil, errors.New("db.exec 缺少必填参数 sql")
	}
	res, err := a.React(context.Background(), "任务", ReactOpts{MaxSteps: 3})
	if err != nil {
		t.Fatalf("React 失败: %v", err)
	}
	if len(*entries) != 1 {
		t.Fatalf("bad_input 不得机械重试，实际执行 %d 次", len(*entries))
	}
	log, _ := res.(map[string]interface{})["recovery_log"].([]string)
	if len(log) != 0 {
		t.Fatalf("bad_input 不应产生 L0 重试记录，实际 %v", log)
	}
	steps, _ := res.(map[string]interface{})["steps"].([]map[string]interface{})
	obs := fmt.Sprint(steps[0]["observation"])
	if !strings.Contains(obs, "类型=bad_input") || !strings.Contains(obs, "范围=self") {
		t.Fatalf("信号须带类型与范围，实际 %q", obs)
	}
	if !strings.Contains(obs, "缺少必填参数 sql") {
		t.Fatalf("信号须带原因原文，实际 %q", obs)
	}
}

// TestReactUnrecoverableFailureRecordsGapImmediately：auth/unrecoverable 机械恢复
// 注定无效，立即记缺口（不等震荡阈值）——等的只是让循环在它身上白烧步数。
func TestReactUnrecoverableFailureRecordsGapImmediately(t *testing.T) {
	llm := &scriptedLLM{texts: []string{
		actionJSON("db.query", `{"sql":"SELECT * FROM t"}`),
		"Final Answer: 表没了，只能说明情况",
	}}
	a, entries := newReactAgent(t, llm)
	a.exec = func(_ context.Context, entry string, _ map[string]interface{}) (map[string]interface{}, error) {
		*entries = append(*entries, entry)
		return nil, errors.New("no such table: t（404）")
	}
	res, err := a.React(context.Background(), "任务", ReactOpts{MaxSteps: 3})
	if err != nil {
		t.Fatalf("React 失败: %v", err)
	}
	if len(*entries) != 1 {
		t.Fatalf("unrecoverable 不得重试，实际执行 %d 次", len(*entries))
	}
	gaps, _ := res.(map[string]interface{})["gaps"].([]map[string]interface{})
	if len(gaps) != 1 || gaps[0]["type"] != "unrecoverable" || gaps[0]["action"] != "db.query" {
		t.Fatalf("unrecoverable 应立即记缺口，实际 %v", gaps)
	}
	// 模型必须在作答前知道缺口（回答时第一次知道就太晚了）。
	noticeFound := false
	for _, m := range llm.got[1].Messages {
		if s, _ := m["content"].(string); strings.Contains(s, "已知缺口") {
			noticeFound = true
		}
	}
	if !noticeFound {
		t.Fatal("第二轮请求应带上缺口通知（带缺失作答的前提）")
	}
}

// TestReactOscillationGuardBlocksAndMarksGap：同一动作同类型失败连续达到阈值 →
// 封禁该动作 + 记缺口（震荡守卫）。没有它，模型会拿不同参数反复撞同一堵墙，
// 烧完预算才兜底——那正是 REWOO 设计里 plan-equivalence 守卫要防的。
func TestReactOscillationGuardBlocksAndMarksGap(t *testing.T) {
	llm := &scriptedLLM{texts: []string{
		actionJSON("db.query", `{"sql":"SELECT * FROM a"}`),
		actionJSON("db.query", `{"sql":"SELECT * FROM b"}`),
		actionJSON("db.query", `{"sql":"SELECT * FROM c"}`),
		actionJSON("db.query", `{"sql":"SELECT * FROM d"}`),
		"Final Answer: 数据都取不到，如实说明",
	}}
	a, entries := newReactAgent(t, llm)
	a.exec = func(_ context.Context, entry string, _ map[string]interface{}) (map[string]interface{}, error) {
		*entries = append(*entries, entry)
		return nil, errors.New("上游服务 404 not found")
	}
	res, err := a.React(context.Background(), "任务", ReactOpts{MaxSteps: 6})
	if err != nil {
		t.Fatalf("React 失败: %v", err)
	}
	if len(*entries) != 3 {
		t.Fatalf("第 4 次应被震荡守卫拦下（不执行），实际执行 %d 次", len(*entries))
	}
	steps, _ := res.(map[string]interface{})["steps"].([]map[string]interface{})
	obs := fmt.Sprint(steps[3]["observation"])
	if !strings.Contains(obs, "已被停止调用") || !strings.Contains(obs, "连续 3 次失败") {
		t.Fatalf("第 4 步应回灌封禁说明，实际 %q", obs)
	}
	log, _ := res.(map[string]interface{})["recovery_log"].([]string)
	guardFound := false
	for _, l := range log {
		if strings.Contains(l, "震荡守卫") {
			guardFound = true
		}
	}
	if !guardFound {
		t.Fatalf("recovery_log 应记录震荡守卫动作，实际 %v", log)
	}
	gaps, _ := res.(map[string]interface{})["gaps"].([]map[string]interface{})
	if len(gaps) != 1 || gaps[0]["action"] != "db.query" {
		t.Fatalf("震荡封禁必须落成缺口，实际 %v", gaps)
	}
}

// TestReactEmptyResultsEscalateToGap：连续空结果（换参数也查不到）→ 判定为缺口。
// 空结果不是错误（确实没数据就是正确答案），所以不立即判、只计数——连续 N 次才说「别翻了」。
func TestReactEmptyResultsEscalateToGap(t *testing.T) {
	llm := &scriptedLLM{texts: []string{
		actionJSON("db.query", `{"sql":"SELECT * FROM a"}`),
		actionJSON("db.query", `{"sql":"SELECT * FROM b"}`),
		actionJSON("db.query", `{"sql":"SELECT * FROM c"}`),
		actionJSON("db.query", `{"sql":"SELECT * FROM d"}`),
		"Final Answer: 未找到相关数据",
	}}
	a, entries := newReactAgent(t, llm)
	a.exec = func(_ context.Context, entry string, _ map[string]interface{}) (map[string]interface{}, error) {
		*entries = append(*entries, entry)
		return map[string]interface{}{"result": []interface{}{}}, nil
	}
	res, err := a.React(context.Background(), "任务", ReactOpts{MaxSteps: 6})
	if err != nil {
		t.Fatalf("React 失败: %v", err)
	}
	if len(*entries) != 3 {
		t.Fatalf("第 4 次应被拦下，实际执行 %d 次", len(*entries))
	}
	steps, _ := res.(map[string]interface{})["steps"].([]map[string]interface{})
	if obs := fmt.Sprint(steps[0]["observation"]); !strings.Contains(obs, "【空结果】") {
		t.Fatalf("空结果应回灌明确提示（含「未找到」引导），实际 %q", obs)
	}
	if obs := fmt.Sprint(steps[3]["observation"]); !strings.Contains(obs, "连续 3 次查询无结果") {
		t.Fatalf("第 4 步应被告知连续无结果，实际 %q", obs)
	}
	gaps, _ := res.(map[string]interface{})["gaps"].([]map[string]interface{})
	if len(gaps) != 1 || gaps[0]["type"] != "semantic_empty" {
		t.Fatalf("连续空结果应记 semantic_empty 缺口，实际 %v", gaps)
	}
}

// TestReactLLMTransientFailureIsRetried：provider 侧瞬时错误（超时/429）机械重试，
// 不再一出错就整个 react 中止。auth/unrecoverable 仍然上抛（机械恢复无效不硬撑）。
// 注意脚本按**调用序号**消费且失败那次不产文本：第 1 次调用瞬时失败（其 texts/calls 作废），
// 重试落在第 2 条——工具调用必须编在 index 1，编在 index 0 会被失败吞掉。
func TestReactLLMTransientFailureIsRetried(t *testing.T) {
	llm := &scriptedLLM{
		texts: []string{"", "让我查一下", "Final Answer: 重试后完成"},
		calls: [][]ToolCall{nil, {{ID: "c1", Name: SanitizeToolName("db.exec"), Arguments: `{"sql":"SELECT 1"}`}}, nil},
		errs:  []error{errors.New("connection reset by peer"), nil, nil},
	}
	a, entries := newReactAgent(t, llm)
	res, err := a.React(context.Background(), "任务", ReactOpts{MaxSteps: 3})
	if err != nil {
		t.Fatalf("瞬时 LLM 错误应被重试吸收，实际 %v", err)
	}
	if m, _ := res.(map[string]interface{}); m["answer"] != "重试后完成" {
		t.Fatalf("answer 应为最终答案，实际 %v", m)
	}
	if len(*entries) != 1 {
		t.Fatalf("工具应执行一次，实际 %v", *entries)
	}
	log, _ := res.(map[string]interface{})["recovery_log"].([]string)
	if len(log) != 1 || !strings.Contains(log[0], "L0 重试 LLM 调用") {
		t.Fatalf("recovery_log 应记录 LLM 重试，实际 %v", log)
	}
	if len(llm.got) != 3 {
		t.Fatalf("LLM 应被调用 3 次（失败1+工具1+收尾1），实际 %d", len(llm.got))
	}
}

// TestReactLLMAuthFailureAborts：auth 类 LLM 错误（key 无效）机械重试无意义，上抛。
func TestReactLLMAuthFailureAborts(t *testing.T) {
	llm := &scriptedLLM{errs: []error{errors.New("401 unauthorized: invalid api key")}}
	a, _ := newReactAgent(t, llm)
	_, err := a.React(context.Background(), "任务", ReactOpts{MaxSteps: 2})
	if err == nil || !strings.Contains(err.Error(), "LLM 调用失败") {
		t.Fatalf("auth 类 LLM 错误必须中止，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "auth") {
		t.Fatalf("报错应带分类（auth），实际 %v", err)
	}
	if len(llm.got) != 1 {
		t.Fatalf("auth 不得重试，实际调用 %d 次", len(llm.got))
	}
}

// TestReactDegradesToTextProtocolWhenToolsUnsupported：端点不吃工具声明时换通道
// （L1.5）——降级为文本 Action 协议，而不是整个 react 死掉。降级后提示词随之重建，
// 模型拿到的是文本协议说明。
func TestReactDegradesToTextProtocolWhenToolsUnsupported(t *testing.T) {
	llm := &scriptedLLM{
		texts: []string{"", actionJSON("db.exec", `{"sql":"SELECT 1"}`), "Final Answer: 降级后完成"},
		errs:  []error{errors.New("LLM 请求失败: 400 Bad Request: tools field is not supported by this endpoint"), nil, nil},
	}
	a, entries := newReactAgent(t, llm)
	res, err := a.React(context.Background(), "任务", ReactOpts{MaxSteps: 3})
	if err != nil {
		t.Fatalf("不支持工具声明时应降级继续，实际 %v", err)
	}
	if len(*entries) != 1 {
		t.Fatalf("降级后应经文本协议执行一次调用，实际 %v", *entries)
	}
	log, _ := res.(map[string]interface{})["recovery_log"].([]string)
	if len(log) != 1 || !strings.Contains(log[0], "换通道") {
		t.Fatalf("recovery_log 应记录换通道降级，实际 %v", log)
	}
	// 第二次请求的系统提示必须是文本协议版（含 ```json 围栏说明）——证明提示词重建了。
	var sys string
	for _, m := range llm.got[1].Messages {
		if role, _ := m["role"].(string); role == "system" {
			sys, _ = m["content"].(string)
		}
	}
	if !strings.Contains(sys, "```json") {
		t.Fatalf("降级后系统提示应重建为文本协议，实际 %q", sys)
	}
}

// TestReactMetaModeRejectsToolsUnsupported：元工具模式的工具面本身就是 4 个元工具，
// 文本协议表达不了 find/describe/invoke 工作流——明确报错换模式，不硬降级。
func TestReactMetaModeRejectsToolsUnsupported(t *testing.T) {
	llm := &scriptedLLM{
		texts: []string{""},
		errs:  []error{errors.New("400: tools not supported")},
	}
	a, _ := newReactAgent(t, llm)
	_, err := a.React(context.Background(), "任务",
		ReactOpts{MaxSteps: 2, Allow: []string{"db"}, AllowSet: true, Mode: ReactModeMeta})
	if err == nil || !strings.Contains(err.Error(), "元工具模式") {
		t.Fatalf("元工具模式 + 不支持工具声明应明确报错，实际 %v", err)
	}
}

// TestReactRetryBudgetIsBounded：全局机械重试预算必须有上界。
// 没有它，「每类各重试几次」会在长任务里复利。场景刻意让每轮换一个工具：
// 同一工具连续失败会先被震荡守卫拦下（那是另一条线），预算封顶管的是
// 「失败分散在不同工具上」时复利累计的那部分。
func TestReactRetryBudgetIsBounded(t *testing.T) {
	tools := []struct{ tool, method, args string }{
		{"db.exec", "", `{"sql":"SELECT 1"}`},
		{"db.query", "", `{"sql":"SELECT 2"}`},
		{"time.now", "", `{}`},
		{"id.uuid", "", `{}`},
		{"config.get", "", `{"key":"a"}`},
		{"template.render", "", `{"template":"x"}`},
	}
	texts := []string{}
	for _, tl := range tools {
		texts = append(texts, actionJSON(tl.tool, tl.args))
	}
	texts = append(texts, "Final Answer: 结束")
	llm := &scriptedLLM{texts: texts}
	a, entries := newReactAgent(t, llm)
	a.exec = func(_ context.Context, entry string, _ map[string]interface{}) (map[string]interface{}, error) {
		*entries = append(*entries, entry)
		return nil, errors.New("connection refused")
	}
	res, err := a.React(context.Background(), "任务", ReactOpts{MaxSteps: 8})
	if err != nil {
		t.Fatalf("React 失败: %v", err)
	}
	log, _ := res.(map[string]interface{})["recovery_log"].([]string)
	retries := 0
	for _, l := range log {
		if strings.HasPrefix(l, "L0 重试") {
			retries++
		}
	}
	if retries != maxTotalRetries {
		t.Fatalf("机械重试总数应恰好在全局预算 %d 处封顶，实际 %d", maxTotalRetries, retries)
	}
	// 执行次数 = 6 次首发 + 预算内的重试，且全部有界。
	if len(*entries) != len(tools)+maxTotalRetries {
		t.Fatalf("执行次数应有界（%d+%d），实际 %d", len(tools), maxTotalRetries, len(*entries))
	}
	// 分散失败不触发震荡守卫（每种签名只失败一两次）。
	for _, l := range log {
		if strings.Contains(l, "震荡守卫") {
			t.Fatalf("本场景不应触发震荡守卫，实际 %v", log)
		}
	}
}

// TestReactResultAlwaysCarriesGapsAndRecoveryLog：两个新字段始终存在（空切片而非缺字段），
// 调用方不必先判存在性；正常路径下都为空。
func TestReactResultAlwaysCarriesGapsAndRecoveryLog(t *testing.T) {
	llm := &scriptedLLM{texts: []string{
		actionJSON("db.exec", `{"sql":"SELECT 1"}`),
		"Final Answer: 完成",
	}}
	a, _ := newReactAgent(t, llm)
	res, err := a.React(context.Background(), "任务", ReactOpts{MaxSteps: 3})
	if err != nil {
		t.Fatalf("React 失败: %v", err)
	}
	m, _ := res.(map[string]interface{})
	gaps, ok := m["gaps"].([]map[string]interface{})
	if !ok {
		t.Fatalf("gaps 必须始终存在且为数组，实际 %T", m["gaps"])
	}
	if len(gaps) != 0 {
		t.Fatalf("无失败时 gaps 应为空，实际 %v", gaps)
	}
	log, ok := m["recovery_log"].([]string)
	if !ok {
		t.Fatalf("recovery_log 必须始终存在且为数组，实际 %T", m["recovery_log"])
	}
	if len(log) != 0 {
		t.Fatalf("无恢复动作时 recovery_log 应为空，实际 %v", log)
	}
	// 旧字段形状不变（兼容既有消费方）。
	if _, ok := m["answer"].(string); !ok {
		t.Error("answer 形状不得变")
	}
	if _, ok := m["steps"].([]map[string]interface{}); !ok {
		t.Error("steps 形状不得变")
	}
	if _, ok := m["used_tools"].([]string); !ok {
		t.Error("used_tools 形状不得变")
	}
}

// TestReactMaxStepsFallbackCarriesGaps：步数耗尽时的兜底答案必须带缺口——
// 静默残缺比明说缺失坏得多（这是「带缺失作答」的底线）。
func TestReactMaxStepsFallbackCarriesGaps(t *testing.T) {
	llm := &scriptedLLM{texts: []string{
		actionJSON("db.query", `{"sql":"SELECT 1"}`),
		actionJSON("db.query", `{"sql":"SELECT 2"}`),
	}}
	a, entries := newReactAgent(t, llm)
	a.exec = func(_ context.Context, entry string, _ map[string]interface{}) (map[string]interface{}, error) {
		*entries = append(*entries, entry)
		return nil, errors.New("404 not found")
	}
	res, _ := a.React(context.Background(), "任务", ReactOpts{MaxSteps: 2})
	ans, _ := res.(map[string]interface{})["answer"].(string)
	if !strings.Contains(ans, "未给出 Final Answer") {
		t.Fatalf("步数耗尽必须明说，实际 %q", ans)
	}
	if !strings.Contains(ans, "未能获取的数据") || !strings.Contains(ans, "db.query") {
		t.Fatalf("兜底答案必须带上缺口清单，实际 %q", ans)
	}
}
