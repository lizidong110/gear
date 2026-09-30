package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"gear/internal/model"
	"gear/internal/registry"
)

// ① 的收尾：REST 是**真的身份面**（IDENTITY_CONTEXT_DESIGN.md §3.5）。
// 这组测试锁的是那条链路真的通：请求头 → 引擎边界 → Lua 里的 ctx.caller。

// callerPeekTool 是一个把调用主体放进返回值的 Tool
// （ctx.caller 由宿主注入，不走 input，所以这正是「谁发起的」唯一可观测点）。
func callerPeekTool() *model.Tool {
	return &model.Tool{
		ID: "peek", Name: "Peek", Runtime: model.RuntimeLua,
		Entry:        `return { subject = ctx.caller.subject }`,
		InputSchema:  map[string]interface{}{"type": "object"},
		OutputSchema: map[string]interface{}{"type": "object"},
		Versions:     []model.ToolVersion{{Version: "1.0.0", State: model.StateStable}},
	}
}

// doAuth 发一个带自定义头的请求（do 不带头，覆盖不到身份用例）。
func doAuth(t *testing.T, h http.Handler, method, path string, headers map[string]string, body interface{}) (*httptest.ResponseRecorder, map[string]interface{}) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("编码请求体失败: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

// ③ 审计（2026-09-26）：身份链路的价值在于「事后能回答『谁干的』」。
// 光把 caller 注进引擎还不够——**写库的那一步也得带上它**，否则 definition_history
// 与执行记录里的主体列永远是空串，审计面就是个空壳。
//
// 这组用例锁的是「REST 面把真主体带进了写入」，是全仓唯一给定义写入传 actor 的地方
// （CLI 侧传操作系统用户，MCP 侧**刻意**传空——见 TestMCPDeleteLeavesNoActor）。

// newAuditTool 造一个最小可落库的 Tool（只做审计用，不要求能跑）。
func newAuditTool(id string) *model.Tool {
	return &model.Tool{
		ID: id, Name: id, Runtime: model.RuntimeLua, Entry: "return 1",
		InputSchema:  map[string]interface{}{"type": "object"},
		OutputSchema: map[string]interface{}{"type": "object"},
		Versions:     []model.ToolVersion{{Version: "1.0.0", State: model.StateStable}},
	}
}

// latestActor 返回某对象最新一条历史的 actor 与 note。
func latestTrace(t *testing.T, reg *registry.Registry, id string) (string, string) {
	t.Helper()
	rows, err := reg.ListHistory("tool", id, 1)
	if err != nil {
		t.Fatalf("查历史失败: %v", err)
	}
	if len(rows) == 0 {
		t.Fatalf("对象 %s 没有任何历史——写入没留痕", id)
	}
	return rows[0].Actor, rows[0].Note
}

// TestDefinitionWriteRecordsActor：REST 建定义要记下调用主体；不带头则如实记空串。
func TestDefinitionWriteRecordsActor(t *testing.T) {
	s, reg := newTestServer(t)
	h := s.Handler()

	_, _ = doAuth(t, h, "POST", "/api/tools",
		map[string]string{"Authorization": "Bearer alice"}, newAuditTool("audit__a"))
	actor, note := latestTrace(t, reg, "audit__a")
	if actor != "alice" {
		t.Fatalf("REST 建 Tool 应把 alice 记进 actor，实际 %q", actor)
	}
	if note != "" {
		t.Fatalf("普通写入的 note 应为空，实际 %q", note)
	}

	// 负向：不带头 → 空串，不是 "anonymous"
	_, _ = doAuth(t, h, "POST", "/api/tools", nil, newAuditTool("audit__b"))
	actor, _ = latestTrace(t, reg, "audit__b")
	if actor != "" {
		t.Fatalf("无凭据写入应记成空串（「没主体」≠「anonymous」），实际 %q", actor)
	}
}

// TestDefinitionDeleteRecordsActor：删除同样要把人记下来，且 note 标成 delete。
func TestDefinitionDeleteRecordsActor(t *testing.T) {
	s, reg := newTestServer(t)
	h := s.Handler()

	_, _ = doAuth(t, h, "POST", "/api/tools",
		map[string]string{"Authorization": "Bearer alice"}, newAuditTool("audit__a"))
	if _, rec := doAuth(t, h, "DELETE", "/api/tools/audit__a",
		map[string]string{"Authorization": "Bearer alice"}, nil); rec["deleted"] != "audit__a" {
		t.Fatalf("删除未生效: %v", rec)
	}
	actor, note := latestTrace(t, reg, "audit__a")
	if actor != "alice" {
		t.Fatalf("REST 删除应把 alice 记进 actor，实际 %q", actor)
	}
	if note != "delete" {
		t.Fatalf("删除那行的 note 应为 delete，实际 %q", note)
	}
}

// TestExecutionRecordCarriesSubject：执行记录要能回答「这次是谁发起的」。
func TestExecutionRecordCarriesSubject(t *testing.T) {
	s, reg := newTestServer(t)
	if err := reg.UpsertTool(callerPeekTool()); err != nil {
		t.Fatalf("注册 Tool 失败: %v", err)
	}
	if err := reg.UpsertFlow(&model.Flow{
		ID: "who", Steps: []model.Step{{ID: "s1", Call: "peek"}},
	}); err != nil {
		t.Fatalf("注册 Flow 失败: %v", err)
	}
	h := s.Handler()

	_, out := doAuth(t, h, "POST", "/api/executions",
		map[string]string{"Authorization": "Bearer alice"}, map[string]interface{}{"flow_id": "who"})
	id, ok := out["id"].(string)
	if !ok || id == "" {
		t.Fatalf("应返回 execution id: %v", out)
	}
	// 记录落在库里要带主体（不等执行跑完）
	rec, ok := s.execs.Get(id)
	if !ok {
		t.Fatalf("查不到执行记录 %s", id)
	}
	if rec.Actor != "alice" {
		t.Fatalf("执行记录的 actor 应为 alice，实际 %q", rec.Actor)
	}
	// 且要在 REST 返回里**可见**（审计不是内部字段，要看得到才叫审计）
	_, cur := do(t, h, "GET", "/api/executions/"+id, nil)
	if cur["actor"] != "alice" {
		t.Fatalf("GET /api/executions/{id} 应带出 actor，实际 %v", cur["actor"])
	}
}

// TestSubjectOfExtraction：主体提取的各形态，含**负向**——
// 「拿不到主体时返回空串，而不是某个默认名字」是 Hook 能区分「没验过」与「验出匿名」的前提。
func TestSubjectOfExtraction(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
		want    string
	}{
		{"bearer", map[string]string{"Authorization": "Bearer alice"}, "alice"},
		{"bearer 小写前缀", map[string]string{"Authorization": "bearer alice"}, "alice"},
		{"bearer 余段原样带回", map[string]string{"Authorization": "Bearer a b"}, "a b"},
		{"显式 subject 头", map[string]string{"X-Gear-Subject": "bob"}, "bob"},
		{"bearer 优先", map[string]string{
			"Authorization": "Bearer alice", "X-Gear-Subject": "bob",
		}, "alice"},
		{"basic 不算", map[string]string{"Authorization": "Basic YWxpY2U="}, ""},
		{"空 bearer", map[string]string{"Authorization": "Bearer    "}, ""},
		{"无头", nil, ""},
	}
	for _, c := range cases {
		req := httptest.NewRequest("POST", "/api/tools/x/run", nil)
		for k, v := range c.headers {
			req.Header.Set(k, v)
		}
		if got := subjectOf(req); got != c.want {
			t.Errorf("%s：期望 %q，实际 %q", c.name, c.want, got)
		}
	}
}

// TestRunToolEndpointCarriesCaller：带凭据的 REST 调用，Tool 必须看见真身份。
func TestRunToolEndpointCarriesCaller(t *testing.T) {
	s, reg := newTestServer(t)
	if err := reg.UpsertTool(callerPeekTool()); err != nil {
		t.Fatalf("注册 Tool 失败: %v", err)
	}
	h := s.Handler()

	_, out := doAuth(t, h, "POST", "/api/tools/peek/run",
		map[string]string{"Authorization": "Bearer alice"}, map[string]interface{}{})
	o, ok := out["output"].(map[string]interface{})
	if !ok {
		t.Fatalf("无 output: %v", out)
	}
	if o["subject"] != "alice" {
		t.Fatalf(`ctx.caller.subject 应为 "alice"，实际 %v（REST 身份注入漏了？）`, o["subject"])
	}

	// 负向：不带凭据 → **空串**而不是 "anonymous"。
	_, out = doAuth(t, h, "POST", "/api/tools/peek/run", nil, map[string]interface{}{})
	o, _ = out["output"].(map[string]interface{})
	if o == nil {
		t.Fatalf("无 output: %v", out)
	}
	if o["subject"] != "" {
		t.Fatalf(`无凭据时 subject 应为空串（「没验过」≠「验出匿名」），实际 %v`, o["subject"])
	}
}

// TestCreateExecutionCarriesCaller：异步执行面同样要带上身份。
//
// 这条用例针对的是**异步**路径：身份必须在派发前抓成字符串、在 worker 里重新组装 ctx——
// 直接沿用请求的 ctx 会让执行在 handler 返回后被取消（跑到一半被判超时）。
func TestCreateExecutionCarriesCaller(t *testing.T) {
	s, reg := newTestServer(t)
	if err := reg.UpsertTool(callerPeekTool()); err != nil {
		t.Fatalf("注册 Tool 失败: %v", err)
	}
	if err := reg.UpsertFlow(&model.Flow{
		ID: "who", Steps: []model.Step{{ID: "s1", Call: "peek"}},
	}); err != nil {
		t.Fatalf("注册 Flow 失败: %v", err)
	}
	h := s.Handler()

	_, out := doAuth(t, h, "POST", "/api/executions",
		map[string]string{"Authorization": "Bearer alice"}, map[string]interface{}{"flow_id": "who"})
	id, ok := out["id"].(string)
	if !ok || id == "" {
		t.Fatalf("应返回 execution id: %v", out)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		_, cur := do(t, h, "GET", "/api/executions/"+id, nil)
		if st, _ := cur["status"].(string); st != "" && st != "running" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("执行超时未结束: %v", cur)
		}
		time.Sleep(20 * time.Millisecond)
	}

	_, cur := do(t, h, "GET", "/api/executions/"+id, nil)
	// 记录是 {status, result:{flow_id, steps:{step -> {input, output}}}}：结果嵌套在 result 里，
	// 不是平铺在记录顶层。
	res, _ := cur["result"].(map[string]interface{})
	steps, _ := res["steps"].(map[string]interface{})
	s1, _ := steps["s1"].(map[string]interface{})
	output, _ := s1["output"].(map[string]interface{})
	if output == nil {
		t.Fatalf("s1 无 output: %v", s1)
	}
	if output["subject"] != "alice" {
		t.Fatalf(`异步执行也应带上身份，实际 %v`, output["subject"])
	}
}
