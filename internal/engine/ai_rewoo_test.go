package engine

// ai_rewoo_test.go —— ext.ai.rewoo（REQ-REWOO）的测试。分四层：
//   ① planner 校验分支（坏 DAG 各形态，Go 侧不靠模型自觉）；
//   ② worker（fan-in 证据共享 / fail-soft / 上游败堵塞 / L0 重试 / 空结果 / 震荡封禁）；
//   ③ replanner（反可达作用域 / 冻结边界 / 等价守卫 / LLM 调用预算硬上限）；
//   ④ 入口门禁（RewooMethod 的 allow 子集与 call 能力要求）。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// newRewooAgent 构造可跑 rewoo 的 agent：复用 react 的测试注册表（含 demo.echo 工具），
// 退避注入零值——恢复路径必须确定性，不能靠「睡一会儿看起来对了」。
func newRewooAgent(t *testing.T, llm LLM) (*AIAgent, *[]string) {
	t.Helper()
	a, entries := newReactAgent(t, llm)
	a.retryBackoff = func(int) time.Duration { return 0 }
	return a, entries
}

// rewooTestNode 快捷造节点。
func rewooTestNode(id, action string, args map[string]interface{}, deps ...string) *rewooNode {
	if args == nil {
		args = map[string]interface{}{}
	}
	return &rewooNode{id: id, action: action, args: args, deps: deps, status: rewooPending}
}

// recordEntry 并发安全地记一条 exec 入口。worker 层内并行执行（MaxParallel>1 时同层多节点
// 同时进 a.exec），mock 闭包裸 append 会在 -race 下让**测试自己先炸**——2026-10-19 实测：
// TestRewooWorkerFanInSharesEvidence 两个并行 goroutine 抢同一个 slice  backing array。
// 串流测试（MaxParallel=1）今天炸不到，但并行度是调大就生效的参数，五处统一走助手。
func recordEntry(mu *sync.Mutex, entries *[]string, entry string) {
	mu.Lock()
	*entries = append(*entries, entry)
	mu.Unlock()
}

// ---- ① planner 校验 ----

func TestParseRewooPlanRejectsGarbage(t *testing.T) {
	cases := map[string]string{
		"非 JSON":    "这不是 JSON",
		"缺 steps":   `{"plan": []}`,
		"steps 非数组": `{"steps": {}}`,
		"节点非对象":     `{"steps": ["x"]}`,
	}
	for name, text := range cases {
		if _, err := parseRewooPlan(text); err == nil {
			t.Fatalf("%s 应被拒绝", name)
		}
	}
	plan, err := parseRewooPlan(`{"steps":[{"id":"#E1","action":"http.Request","args":{"url":"u"},"depends_on":[]}]}`)
	if err != nil {
		t.Fatalf("合法输出应解析成功: %v", err)
	}
	if len(plan.nodes) != 1 || plan.nodes[0].id != "#E1" || plan.nodes[0].args["url"] != "u" {
		t.Fatalf("解析结果不对: %+v", plan.nodes)
	}
}

func TestValidateRewooPlanBranches(t *testing.T) {
	a, _ := newRewooAgent(t, &scriptedLLM{})
	al, err := a.resolveRewooAllow(nil, false)
	if err != nil {
		t.Fatalf("resolveRewooAllow: %v", err)
	}
	cases := []struct {
		name string
		json string
		want string // 期望错误里出现的片段；空 = 应通过
	}{
		{"合法 fan-in", `{"steps":[{"id":"#E1","action":"http.Request","args":{},"depends_on":[]},{"id":"#E2","action":"db.query","args":{},"depends_on":[]},{"id":"#E3","action":"template.render","args":{"t":"{{#E1}}"},"depends_on":["#E1","#E2"]}]}`, ""},
		{"空 steps", `{"steps":[]}`, "steps 不能为空"},
		{"id 形态错", `{"steps":[{"id":"E1","action":"http.Request"}]}`, "不合法"},
		{"id 重复", `{"steps":[{"id":"#E1","action":"http.Request"},{"id":"#E1","action":"db.query"}]}`, "重复"},
		{"action 未知", `{"steps":[{"id":"#E1","action":"nope.Nope"}]}`, "既不是已知"},
		{"有环", `{"steps":[{"id":"#E1","action":"http.Request","depends_on":["#E2"]},{"id":"#E2","action":"db.query","depends_on":["#E1"]}]}`, "有环"},
		{"悬空依赖", `{"steps":[{"id":"#E1","action":"http.Request","depends_on":["#E9"]}]}`, "不存在"},
		{"自依赖", `{"steps":[{"id":"#E1","action":"http.Request","depends_on":["#E1"]}]}`, "依赖自己"},
		{"引用未列依赖", `{"steps":[{"id":"#E1","action":"http.Request"},{"id":"#E2","action":"db.query","args":{"sql":"{{#E1}}"}}]}`, "列进 depends_on"},
		{"超节点上限", `{"steps":[{"id":"#E1","action":"http.Request"},{"id":"#E2","action":"http.Request"},{"id":"#E3","action":"http.Request"}]}`, "超过上限"},
	}
	for _, c := range cases {
		plan, err := parseRewooPlan(c.json)
		if err != nil {
			t.Fatalf("%s：解析应成功（校验错误归 validate）: %v", c.name, err)
		}
		maxNodes := 5
		if c.name == "超节点上限" {
			maxNodes = 2
		}
		err = a.validateRewooPlan(plan, al, RewooOpts{MaxNodes: maxNodes})
		if c.want == "" {
			if err != nil {
				t.Fatalf("%s 应通过校验，实际 %v", c.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s 应报含 %q 的错误，实际 %v", c.name, c.want, err)
		}
	}
}

// TestRewooCtxPrefixAliasIsToleratedAndCanonicalized —— 模型在 Lua 语料上预训练，
// 很自然会写 "ctx.http.Request"（2026-10-19 E2E：两轮 LastErr 修正都扳不回来）。
// 无歧义的前缀别名要容错（resolveRewooAction 剥前缀），且计划快照规范成 canonical
// 形态——可审计可重放的数据不能留着抄错的写法。
func TestRewooCtxPrefixAliasIsToleratedAndCanonicalized(t *testing.T) {
	a, _ := newRewooAgent(t, &scriptedLLM{})
	al, err := a.resolveRewooAllow(nil, false)
	if err != nil {
		t.Fatalf("resolveRewooAllow: %v", err)
	}
	plan, err := parseRewooPlan(`{"steps":[{"id":"#E1","action":"ctx.http.Request","args":{},"depends_on":[]}]}`)
	if err != nil {
		t.Fatalf("解析应成功: %v", err)
	}
	if err := a.validateRewooPlan(plan, al, RewooOpts{MaxNodes: 5}); err != nil {
		t.Fatalf("ctx. 前缀别名应被容错，实际 %v", err)
	}
	if plan.nodes[0].action != "http.Request" {
		t.Fatalf("计划应规范成 canonical 形态，实际 %q", plan.nodes[0].action)
	}
	// negcheck：剥了前缀仍然认得的才算别名——"ctx.nope.Nope" 照样拒
	plan2, _ := parseRewooPlan(`{"steps":[{"id":"#E1","action":"ctx.nope.Nope"}]}`)
	if err := a.validateRewooPlan(plan2, al, RewooOpts{MaxNodes: 5}); err == nil {
		t.Fatal("剥前缀后仍未知的 action 必须拒")
	}
}

func TestValidateRewooPlanEnforcesAllow(t *testing.T) {
	a, _ := newRewooAgent(t, &scriptedLLM{})
	al, err := a.resolveRewooAllow([]string{"http"}, true)
	if err != nil {
		t.Fatalf("resolveRewooAllow: %v", err)
	}
	plan, _ := parseRewooPlan(`{"steps":[{"id":"#E1","action":"db.query"}]}`)
	if err := a.validateRewooPlan(plan, al, RewooOpts{MaxNodes: 5}); err == nil ||
		!strings.Contains(err.Error(), "不在允许列表") {
		t.Fatalf("allow 外的能力应被拒，实际 %v", err)
	}
}

// ---- ② worker ----

func TestRewooWorkerFanInSharesEvidence(t *testing.T) {
	a, entries := newRewooAgent(t, &scriptedLLM{})
	var entMu sync.Mutex
	a.exec = func(_ context.Context, entry string, _ map[string]interface{}) (map[string]interface{}, error) {
		recordEntry(&entMu, entries, entry)
		if strings.Contains(entry, "ctx.http.Request") {
			return map[string]interface{}{"result": map[string]interface{}{"src": "http"}}, nil
		}
		return map[string]interface{}{"result": map[string]interface{}{"src": "db"}}, nil
	}
	plan := &rewooPlan{byID: map[string]*rewooNode{}}
	n1 := rewooTestNode("#E1", "http.Request", nil)
	n2 := rewooTestNode("#E2", "db.query", nil)
	// #E3 同时吃两个上游（fan-in）：整个值引用 + 字符串内嵌引用两种形态都要验
	n3 := rewooTestNode("#E3", "template.render", map[string]interface{}{
		"text": "A={{#E1}} B={{#E2.src}}",
	}, "#E1", "#E2")
	plan.nodes = []*rewooNode{n1, n2, n3}
	for _, n := range plan.nodes {
		plan.byID[n.id] = n
	}
	board := newReactBoard(0)
	ev := map[string]interface{}{}
	var evMu sync.Mutex
	a.rewooWork(context.Background(), plan, board, mustAllowAll(t, a), RewooOpts{MaxParallel: 4}, ev, &evMu)

	if n1.status != rewooDone || n2.status != rewooDone || n3.status != rewooDone {
		t.Fatalf("fan-in 三节点应全部成功，实际 %s/%s/%s", n1.status, n2.status, n3.status)
	}
	if ev["#E3"] == nil {
		t.Fatal("#E3 应拿到上游证据")
	}
	// 引用解析发生在**入口参数**上（mock 的返回值与引用无关）：#E3 的入口里应含
	// 两个上游产出解析后的值——整个值引用 {{#E1}} 与字段路径引用 {{#E2.src}}
	e3entry := ""
	for _, e := range *entries {
		if strings.Contains(e, "ctx.template.render") {
			e3entry = e
		}
	}
	if !strings.Contains(e3entry, "http") || !strings.Contains(e3entry, "db") {
		t.Fatalf("#E3 入口应解析出两个上游产出（http / db），实际 %q", e3entry)
	}
	if len(*entries) != 3 {
		t.Fatalf("应执行 3 次，实际 %d", len(*entries))
	}
}

func TestRewooWorkerFailSoftKeepsSiblings(t *testing.T) {
	a, entries := newRewooAgent(t, &scriptedLLM{})
	var entMu sync.Mutex
	a.exec = func(_ context.Context, entry string, _ map[string]interface{}) (map[string]interface{}, error) {
		recordEntry(&entMu, entries, entry)
		if strings.Contains(entry, "ctx.http.Request") {
			return nil, errors.New("404 not found")
		}
		return map[string]interface{}{"result": "db-ok"}, nil
	}
	plan := &rewooPlan{byID: map[string]*rewooNode{}}
	n1 := rewooTestNode("#E1", "http.Request", nil)    // 失败
	n2 := rewooTestNode("#E2", "db.query", nil)        // 兄弟：照常成功（fail-soft）
	n3 := rewooTestNode("#E3", "db.query", nil, "#E1") // 下游：被堵
	plan.nodes = []*rewooNode{n1, n2, n3}
	for _, n := range plan.nodes {
		plan.byID[n.id] = n
	}
	board := newReactBoard(0)
	ev := map[string]interface{}{}
	var evMu sync.Mutex
	a.rewooWork(context.Background(), plan, board, mustAllowAll(t, a), RewooOpts{MaxParallel: 4}, ev, &evMu)

	if n1.status != rewooFailed {
		t.Fatalf("#E1 应失败，实际 %s", n1.status)
	}
	if n2.status != rewooDone {
		t.Fatalf("#E2 是兄弟节点，应不受影响照常成功（fail-soft），实际 %s", n2.status)
	}
	if n3.status != rewooBlocked || n3.failure == nil || n3.failure.Class != FailUpstreamBad {
		t.Fatalf("#E3 应记 upstream_bad 且不执行，实际 status=%s failure=%+v", n3.status, n3.failure)
	}
	if len(*entries) != 2 {
		t.Fatalf("失败节点与其下游各只跑一次：应执行 2 次（#E1 首发 + #E2），实际 %d", len(*entries))
	}
	if len(board.gapsSnapshot()) == 0 {
		t.Fatal("失败与被堵都必须有缺口记录（不静默）")
	}
}

func TestRewooWorkerRetriesTransientMechanically(t *testing.T) {
	a, entries := newRewooAgent(t, &scriptedLLM{})
	var entMu sync.Mutex
	calls := 0
	a.exec = func(_ context.Context, _ string, _ map[string]interface{}) (map[string]interface{}, error) {
		calls++
		recordEntry(&entMu, entries, "x")
		if calls == 1 {
			return nil, errors.New("connectex: No connection could be made because the target machine actively refused it.")
		}
		return map[string]interface{}{"result": "recovered"}, nil
	}
	plan := &rewooPlan{byID: map[string]*rewooNode{}}
	n1 := rewooTestNode("#E1", "http.Request", nil)
	plan.nodes = []*rewooNode{n1}
	plan.byID["#E1"] = n1
	board := newReactBoard(0)
	ev := map[string]interface{}{}
	var evMu sync.Mutex
	a.rewooWork(context.Background(), plan, board, mustAllowAll(t, a), RewooOpts{MaxParallel: 1}, ev, &evMu)

	if n1.status != rewooDone || n1.attempts != 1 {
		t.Fatalf("瞬时失败应机械重试 1 次后成功，实际 status=%s attempts=%d", n1.status, n1.attempts)
	}
	log := strings.Join(board.recoverySnapshot(), "\n")
	if !strings.Contains(log, "L0 重试") {
		t.Fatalf("恢复流水应记 L0 重试，实际 %q", log)
	}
}

func TestRewooWorkerEmptyResultMarksSemanticEmpty(t *testing.T) {
	a, _ := newRewooAgent(t, &scriptedLLM{})
	a.exec = func(_ context.Context, _ string, _ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"result": []interface{}{}}, nil
	}
	plan := &rewooPlan{byID: map[string]*rewooNode{}}
	n1 := rewooTestNode("#E1", "db.query", nil)
	plan.nodes = []*rewooNode{n1}
	plan.byID["#E1"] = n1
	board := newReactBoard(0)
	ev := map[string]interface{}{}
	var evMu sync.Mutex
	a.rewooWork(context.Background(), plan, board, mustAllowAll(t, a), RewooOpts{MaxParallel: 1}, ev, &evMu)

	if n1.status != rewooDone || !n1.empty {
		t.Fatalf("空结果节点应完成且标记 empty，实际 status=%s empty=%v", n1.status, n1.empty)
	}
	board.clearGaps()
	board.addGap(reactFailure{Class: FailSemanticEmpty, Action: n1.action, GapKey: n1.id, Reason: "工具返回空结果"})
	if len(board.gapsSnapshot()) != 1 {
		t.Fatal("空结果应收尾成缺口（查询无结果也要如实呈现）")
	}
}

func TestRewooWorkerOscillationBlocksSameAction(t *testing.T) {
	a, entries := newRewooAgent(t, &scriptedLLM{})
	var entMu sync.Mutex
	// 404 = unrecoverable：不重试。四个**同层独立**节点 + MaxParallel=1 保证顺序执行——
	// 注意不能用依赖链：依赖失败节点的下游会先被堵（upstream_bad），根本执行不到。
	a.exec = func(_ context.Context, entry string, _ map[string]interface{}) (map[string]interface{}, error) {
		recordEntry(&entMu, entries, entry)
		return nil, errors.New("404 not found")
	}
	plan := &rewooPlan{byID: map[string]*rewooNode{}}
	nodes := []*rewooNode{
		rewooTestNode("#E1", "http.Request", nil),
		rewooTestNode("#E2", "http.Request", nil),
		rewooTestNode("#E3", "http.Request", nil),
		rewooTestNode("#E4", "http.Request", nil),
	}
	plan.nodes = nodes
	for _, n := range nodes {
		plan.byID[n.id] = n
	}
	board := newReactBoard(0)
	ev := map[string]interface{}{}
	var evMu sync.Mutex
	a.rewooWork(context.Background(), plan, board, mustAllowAll(t, a), RewooOpts{MaxParallel: 1}, ev, &evMu)

	// 第 4 个同动作节点应在震荡封禁前被拦下（不执行）
	if len(*entries) != 3 {
		t.Fatalf("同动作连续失败 3 次后第 4 个应被封禁不执行，实际执行 %d 次", len(*entries))
	}
	if _, blocked := board.blockedNotice("http.Request"); !blocked {
		t.Fatal("http.Request 应被震荡守卫封禁")
	}
	if n4 := plan.byID["#E4"]; n4.status != rewooFailed {
		t.Fatalf("#E4 应以封禁失败收尾，实际 %s", n4.status)
	}
}

func TestRewooUserToolNodeGoesThroughCallPrimitive(t *testing.T) {
	a, entries := newRewooAgent(t, &scriptedLLM{})
	var entMu sync.Mutex
	var gotEntry string
	a.exec = func(_ context.Context, entry string, _ map[string]interface{}) (map[string]interface{}, error) {
		gotEntry = entry
		recordEntry(&entMu, entries, entry)
		return map[string]interface{}{"result": map[string]interface{}{"echo": "hi"}}, nil
	}
	plan := &rewooPlan{byID: map[string]*rewooNode{}}
	// demo.echo 是测试注册表里的已注册工具：action 直接写工具 id（用户自建 agent 的调用对象）
	n1 := rewooTestNode("#E1", "demo.echo", map[string]interface{}{"msg": "hi"})
	plan.nodes = []*rewooNode{n1}
	plan.byID["#E1"] = n1
	board := newReactBoard(0)
	ev := map[string]interface{}{}
	var evMu sync.Mutex
	a.rewooWork(context.Background(), plan, board, mustAllowAll(t, a), RewooOpts{MaxParallel: 1}, ev, &evMu)

	if n1.status != rewooDone {
		t.Fatalf("用户工具节点应成功，实际 %s failure=%+v", n1.status, n1.failure)
	}
	if !strings.Contains(gotEntry, `ctx.call("demo.echo"`) {
		t.Fatalf("用户工具应经 call 原语调用，实际入口 %q", gotEntry)
	}
}

// TestRewooRefLookupDrillsHTTPEnvelopeBody —— http.Request 的证据是
// {status, body(JSON 文本), headers} 信封（lua.go:242）。模型自然写 {{#E1.origin}}
// （数据在 body 里），三种写法都必须活；钻不透要如实报错不静默填空。
// 2026-10-19 E2E 实测：planner 写 {{#E1.origin}} 撞「没有可用产出」整节点失败。
func TestRewooRefLookupDrillsHTTPEnvelopeBody(t *testing.T) {
	ev := map[string]interface{}{
		"#E1": map[string]interface{}{
			"status":  200,
			"body":    `{"origin":"1.2.3.4","ua":"curl"}`,
			"headers": map[string]interface{}{"Content-Type": "application/json"},
		},
	}
	cases := []struct{ ref, want string }{
		{"{{#E1.origin}}", "1.2.3.4"},                        // 信封钻透（E2E 撞过的写法）
		{"{{#E1.body}}", `{"origin":"1.2.3.4","ua":"curl"}`}, // 信封直接键仍在
		{"{{#E1.body.origin}}", "1.2.3.4"},                   // JSON 文本链路
		{"ip={{#E1.origin}}", "ip=1.2.3.4"},                  // 嵌串引用同样钻透
	}
	for _, c := range cases {
		got, err := resolveRewooRefs(c.ref, ev)
		if err != nil {
			t.Fatalf("引用 %s 应解析成功，实际 %v", c.ref, err)
		}
		if fmt.Sprint(got) != c.want {
			t.Fatalf("引用 %s 应得 %q，实际 %v", c.ref, c.want, got)
		}
	}
	if v, ok := rewooRefLookup(ev, "#E1.status"); !ok || fmt.Sprint(v) != "200" {
		t.Fatalf("信封直接键 status 应可取，实际 ok=%v v=%v", ok, v)
	}
	// negcheck：body 非 JSON（纯文本）时钻不透必须报错——不静默填空
	evText := map[string]interface{}{"#E1": map[string]interface{}{"status": 200, "body": "<html>404</html>"}}
	if _, err := resolveRewooRefs("{{#E1.origin}}", evText); err == nil {
		t.Fatal("body 非 JSON 时字段引用必须报错")
	}
	// negcheck：信封与 body 里都没有的字段同样报错
	if _, err := resolveRewooRefs("{{#E1.nope}}", ev); err == nil {
		t.Fatal("两层都没有的字段必须报错")
	}
}

// TestRewooReplanPromptListsAvailableCalls —— replanner 与 planner 是两次**无状态**
// LLM 调用，模型不会「记得」planner 那轮看到的目录：提示词必须自带可用调用段。
// 2026-10-19 E2E 实测：漏了这段，真实模型两轮各幻觉一个不存在的能力
// （code.run / json_parse），重规划全程无效、只能落缺口。
func TestRewooReplanPromptListsAvailableCalls(t *testing.T) {
	a, _ := newRewooAgent(t, &scriptedLLM{})
	al, err := a.resolveRewooAllow(nil, false)
	if err != nil {
		t.Fatalf("resolveRewooAllow 失败: %v", err)
	}
	plan := &rewooPlan{byID: map[string]*rewooNode{}}
	n1 := rewooTestNode("#E1", "http.Request", nil)
	n2 := rewooTestNode("#E2", "db.query", nil, "#E1")
	plan.nodes = []*rewooNode{n1, n2}
	plan.byID["#E1"], plan.byID["#E2"] = n1, n2
	var evMu sync.Mutex
	prompt := a.rewooReplanPrompt("任务", plan, map[string]bool{"#E1": true, "#E2": true}, nil, nil, &evMu, al)
	if !strings.Contains(prompt, "【可用调用】") {
		t.Fatalf("replanner 提示词必须带可用调用段，实际:\n%s", prompt)
	}
	if !strings.Contains(prompt, "http.Request") {
		t.Fatalf("replanner 提示词必须列出能力方法（如 http.Request），实际:\n%s", prompt)
	}
	if !strings.Contains(prompt, "已注册工具") {
		t.Fatalf("replanner 提示词必须带已注册工具段（用户自建 agent 的第二个调用对象来源），实际:\n%s", prompt)
	}
	// 2026-10-20 实测：模型两轮输出 #S1/#N1 都被校验拒——规则文字没有正例、没说编号
	// 怎么延续，模型按 planner 的「从 #E1 起」惯性重头数。非法前缀清单、编号延续规则与
	// JSON 正例必须锁在提示词里。
	for _, want := range []string{"均非法", "不要从 #E1 重新数", "\"steps\":"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("replanner 提示词必须含 %q（正例/编号延续规则），实际:\n%s", want, prompt)
		}
	}
}

// ---- ③ replanner 与全链 ----

func TestRewooReverseReachableAndFrozenBoundary(t *testing.T) {
	plan := &rewooPlan{byID: map[string]*rewooNode{}}
	// #E1 败 → #E2 依赖它 → #E3 也依赖它；#E4 独立
	nodes := []*rewooNode{
		rewooTestNode("#E1", "http.Request", nil),
		rewooTestNode("#E2", "db.query", nil, "#E1"),
		rewooTestNode("#E3", "db.query", nil, "#E1"),
		rewooTestNode("#E4", "db.query", nil),
	}
	plan.nodes = nodes
	for _, n := range nodes {
		plan.byID[n.id] = n
	}
	scope := rewooReverseReachable(plan, []string{"#E1"})
	for _, id := range []string{"#E1", "#E2", "#E3"} {
		if !scope[id] {
			t.Fatalf("反可达集应含 %s", id)
		}
	}
	if scope["#E4"] {
		t.Fatal("独立分支不应进作用域")
	}
	// 分层：#E1/#E4 同层，#E2/#E3 下一层
	levels := rewooLevels(plan)
	if len(levels) != 2 || len(levels[0]) != 2 || len(levels[1]) != 2 {
		t.Fatalf("分层应把无依赖节点放同层，实际 %v", levels)
	}
}

func TestRewooFullRunBudgetGapsAndResult(t *testing.T) {
	// planner 出 3 节点（#E1 败 404 → #E2 被堵 → replanner 原样返回 → 等价守卫停 → solver）
	planJSON := `{"steps":[{"id":"#E1","action":"http.Request","args":{},"depends_on":[]},` +
		`{"id":"#E2","action":"db.query","args":{},"depends_on":["#E1"]}]}`
	llm := &scriptedLLM{texts: []string{planJSON, planJSON, "最终答案：部分数据未获取"}}
	a, _ := newRewooAgent(t, llm)
	a.exec = func(_ context.Context, entry string, _ map[string]interface{}) (map[string]interface{}, error) {
		if strings.Contains(entry, "ctx.http.Request") {
			return nil, errors.New("404 not found")
		}
		return map[string]interface{}{"result": "ok"}, nil
	}
	res, err := a.Rewoo(context.Background(), "调研任务", RewooOpts{ReplanBudget: 1, MaxParallel: 2, WallClock: 30 * time.Second})
	if err != nil {
		t.Fatalf("Rewoo 失败: %v", err)
	}
	m, ok := res.(map[string]interface{})
	if !ok {
		t.Fatalf("返回应是 map，实际 %T", res)
	}
	// LLM 有界（本场景全部一次过）：planner 1 + replanner 1 + solver 1 = 3。
	// 带修正的最坏上限是 2 + 2×replan_budget + 1（默认 5），见文件头与设计文档 §8.6。
	if llm.i != 3 {
		t.Fatalf("LLM 调用应恰为 3（planner+replanner+solver 各一次过），实际 %d", llm.i)
	}
	if m["answer"] != "最终答案：部分数据未获取" {
		t.Fatalf("answer 应来自 solver，实际 %v", m["answer"])
	}
	gaps, _ := m["gaps"].([]map[string]interface{})
	if len(gaps) == 0 {
		t.Fatal("失败节点必须有缺口（带缺失作答）")
	}
	found := false
	for _, g := range gaps {
		if strings.Contains(fmt.Sprint(g["action"]), "#E1") {
			found = true
		}
	}
	if !found {
		t.Fatalf("缺口应点名失败节点 #E1，实际 %v", gaps)
	}
	// gaps / recovery_log 始终存在（空切片语义：调用方不必先判存在性）
	if _, ok := m["recovery_log"].([]string); !ok {
		t.Fatalf("recovery_log 应始终存在，实际 %T", m["recovery_log"])
	}
	if _, ok := m["plan"].([]map[string]interface{}); !ok {
		t.Fatalf("plan 快照应存在，实际 %T", m["plan"])
	}
	if _, ok := m["node_outcomes"].([]map[string]interface{}); !ok {
		t.Fatalf("node_outcomes 应存在，实际 %T", m["node_outcomes"])
	}
	log := strings.Join(m["recovery_log"].([]string), "\n")
	if !strings.Contains(log, "等价") {
		t.Fatalf("等价守卫应留恢复流水，实际 %q", log)
	}
}

// ReplanBudget=0 = 显式关闭重规划：失败节点直接落缺口，不得触发 replanner。
// 与 TestRewooFullRunBudgetGapsAndResult（budget=1 → LLM=3）互为 negcheck：
// 抬 0 回默认会让「想关关不掉」，烧一次不该烧的 LLM。
func TestRewooFullRunReplanBudgetZeroDisablesReplanner(t *testing.T) {
	planJSON := `{"steps":[{"id":"#E1","action":"http.Request","args":{},"depends_on":[]},` +
		`{"id":"#E2","action":"db.query","args":{},"depends_on":["#E1"]}]}`
	// 脚本只有 2 条：若触发 replanner，scriptedLLM 会耗尽报错——测试随即失败。
	llm := &scriptedLLM{texts: []string{planJSON, "无重规划作答"}}
	a, _ := newRewooAgent(t, llm)
	a.exec = func(_ context.Context, entry string, _ map[string]interface{}) (map[string]interface{}, error) {
		if strings.Contains(entry, "ctx.http.Request") {
			return nil, errors.New("404 not found")
		}
		return map[string]interface{}{"result": "ok"}, nil
	}
	res, err := a.Rewoo(context.Background(), "关重规划的任务", RewooOpts{ReplanBudget: 0, WallClock: 30 * time.Second})
	if err != nil {
		t.Fatalf("Rewoo 失败: %v", err)
	}
	m := res.(map[string]interface{})
	if llm.i != 2 {
		t.Fatalf("replan_budget=0 时 LLM 调用应恰为 2（planner+solver，无 replanner），实际 %d", llm.i)
	}
	if len(m["gaps"].([]map[string]interface{})) == 0 {
		t.Fatal("关闭重规划不改变带缺失作答：失败节点仍必须留缺口")
	}
}

func TestRewooFullRunHappyPathNoGaps(t *testing.T) {
	planJSON := `{"steps":[{"id":"#E1","action":"http.Request","args":{},"depends_on":[]},` +
		`{"id":"#E2","action":"db.query","args":{"sql":"{{#E1}}"},"depends_on":["#E1"]}]}`
	llm := &scriptedLLM{texts: []string{planJSON, "一切正常"}}
	a, _ := newRewooAgent(t, llm)
	a.exec = func(_ context.Context, _ string, _ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"result": "data"}, nil
	}
	res, err := a.Rewoo(context.Background(), "正常任务", RewooOpts{WallClock: 30 * time.Second})
	if err != nil {
		t.Fatalf("Rewoo 失败: %v", err)
	}
	m := res.(map[string]interface{})
	if len(m["gaps"].([]map[string]interface{})) != 0 {
		t.Fatalf("全成功不应有缺口，实际 %v", m["gaps"])
	}
	if llm.i != 2 {
		t.Fatalf("无失败时 LLM 调用应恰为 2（planner+solver），实际 %d", llm.i)
	}
}

func TestRewooReplannerRebuildsFailedSubgraph(t *testing.T) {
	// planner：#E1 败（404）；#E2 依赖 #E1。replanner 换动作重生 #E1（同 id，冻结它其实无下游——
	// 这里 #E2 在 scope 内，边界为空，replanner 可自由改 #E1 的 action）。solver 收尾。
	planJSON := `{"steps":[{"id":"#E1","action":"http.Request","args":{},"depends_on":[]},` +
		`{"id":"#E2","action":"db.query","args":{},"depends_on":["#E1"]}]}`
	replanJSON := `{"steps":[{"id":"#E1","action":"db.query","args":{"sql":"SELECT 1"},"depends_on":[]},` +
		`{"id":"#E2","action":"template.render","args":{},"depends_on":["#E1"]}]}`
	llm := &scriptedLLM{texts: []string{planJSON, replanJSON, "重规划后完成"}}
	a, _ := newRewooAgent(t, llm)
	a.exec = func(_ context.Context, entry string, _ map[string]interface{}) (map[string]interface{}, error) {
		if strings.Contains(entry, "ctx.http.Request") {
			return nil, errors.New("404 not found")
		}
		return map[string]interface{}{"result": "ok"}, nil
	}
	res, err := a.Rewoo(context.Background(), "会重规划的任务", RewooOpts{ReplanBudget: 1, WallClock: 30 * time.Second})
	if err != nil {
		t.Fatalf("Rewoo 失败: %v", err)
	}
	m := res.(map[string]interface{})
	if m["answer"] != "重规划后完成" {
		t.Fatalf("solver 应汇总重规划后的结果，实际 %v", m["answer"])
	}
	gaps := m["gaps"].([]map[string]interface{})
	if len(gaps) != 0 {
		t.Fatalf("重规划补上后不应留缺口，实际 %v", gaps)
	}
	if llm.i != 3 {
		t.Fatalf("LLM 调用应为 3（planner+replanner+solver），实际 %d", llm.i)
	}
	// 成功的重规划也必须留痕（E2E 场景 3：只记失败不记成功时，恢复流水像从没重规划过）
	log := strings.Join(m["recovery_log"].([]string), "\n")
	if !strings.Contains(log, "重规划生效") {
		t.Fatalf("recovery_log 应记重规划生效，实际 %q", log)
	}
}

// ---- ④ 入口门禁 ----

func TestRewooMethodGuards(t *testing.T) {
	a, _ := newRewooAgent(t, &scriptedLLM{texts: []string{"x"}})
	if _, err := a.RewooMethod(map[string]interface{}{}); err == nil {
		t.Fatal("缺 task 应报错")
	}
	// allow 超出调用方声明的能力
	if _, err := a.RewooMethod(map[string]interface{}{"task": "x", "allow": []interface{}{"secret"}}, "ext.ai"); err == nil ||
		!strings.Contains(err.Error(), "allow 不能超出") {
		t.Fatalf("越权 allow 应被拒，实际 %v", err)
	}
	// 工具 id 进 allow 但调用方没声明 call
	if _, err := a.RewooMethod(map[string]interface{}{"task": "x", "allow": []interface{}{"demo.echo"}}, "ext.ai"); err == nil ||
		!strings.Contains(err.Error(), "call") {
		t.Fatalf("无 call 声明时工具 id 应被拒，实际 %v", err)
	}
	// 声明了 call：工具 id 放行（随即因 LLM 脚本耗尽报错——但错误不再是门禁错误）
	_, err := a.RewooMethod(map[string]interface{}{"task": "x", "allow": []interface{}{"demo.echo"}}, "ext.ai", "call")
	if err != nil && strings.Contains(err.Error(), "call 能力") {
		t.Fatalf("有 call 声明时工具 id 不应被门禁拦下，实际 %v", err)
	}
}

func TestRewooMethodMissingTaskIsRejected(t *testing.T) {
	a, _ := newRewooAgent(t, &scriptedLLM{})
	if _, err := a.Rewoo(context.Background(), "", RewooOpts{}); err == nil {
		t.Fatal("空 task 应报错")
	}
}

// TestRewooCtxNoteRewritesDeadline 锁死墙钟耗尽的文案口径。
// 2026-10-20 用户实战：跑 5 分钟只看到 "context deadline exceeded"——不知道是谁的
// deadline、能不能调、该查什么。改写必须同时给出三件事：总墙钟、怎么调、最可能的原因。
func TestRewooCtxNoteRewritesDeadline(t *testing.T) {
	dead, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-dead.Done()
	live := context.Background()
	plain := fmt.Errorf("llm: %w", context.DeadlineExceeded)

	got := rewooCtxNote(plain, dead, 5*time.Minute, "planner")
	if got == nil || !errors.Is(got, context.DeadlineExceeded) {
		t.Fatalf("墙钟耗尽必须仍可被 errors.Is 识别，实际 %v", got)
	}
	for _, want := range []string{"总墙钟", "planner", "wall_clock", "network"} {
		if !strings.Contains(got.Error(), want) {
			t.Fatalf("墙钟文案必须含 %q，实际 %q", want, got.Error())
		}
	}
	// ctx 没超时（LLM 自身超时/网络错）：原样返回，不偷改成墙钟口径
	if other := rewooCtxNote(plain, live, 5*time.Minute, "planner"); other == nil || other.Error() != plain.Error() {
		t.Fatalf("非墙钟错误必须原样返回，实际 %v", other)
	}
	// nil 错误不过手
	if rewooCtxNote(nil, dead, 5*time.Minute, "planner") != nil {
		t.Fatal("nil 错误应原样返回 nil")
	}
}

// mustAllowAll 造一个不设限的允许集（工具测试用：焦点在执行语义不在门禁）。
func mustAllowAll(t *testing.T, a *AIAgent) *rewooAllow {
	t.Helper()
	al, err := a.resolveRewooAllow([]string{ReactAllowAll}, true)
	if err != nil {
		t.Fatalf("resolveRewooAllow(*): %v", err)
	}
	return al
}
