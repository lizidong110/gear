package server

// errshape_test.go —— REST 错误响应的形状（2026-10-03）。
//
// 此前 REST 只有 `{"error": "一整段文本"}`，实测里调用方只能正则剥：
// `replace(/^step \S+ 执行失败: lua 执行失败: /,'')`。文本形状一改就崩，
// 而 MCP 面早就是 `{code, message, hint}`——同一个内核两套口径。
//
// 这组用例锁三件事：① 结构化字段真的在；② **兼容位 `error` 仍在且与 message 同值**
// （删它就是破坏性变更：web-ui 的 api client、mindstorm 前端、全部既有测试都在读）；
// ③ 栈回溯进 `detail` 而不是混在 message 里。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gear/internal/engine"
	"gear/internal/model"
)

// seed 出来的 echo Tool 的入参是 {name}（必填），用它制造两类错误：
// 入参缺字段（schema 校验失败）与脚本里崩（Lua 错误）。
func TestRestErrorsAreStructuredAndKeepErrorField(t *testing.T) {
	s, reg := newTestServer(t)
	seed(t, reg)

	// ① 入参校验失败：必须有 code / message，且 error 兼容位同值。
	rec, out := do(t, s.Handler(), http.MethodPost, "/api/tools/echo/run", map[string]interface{}{})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("缺必填入参期望 400，实际 %d %s", rec.Code, rec.Body.String())
	}
	code, _ := out["code"].(string)
	if code == "" {
		t.Fatalf("错误响应缺 code: %s", rec.Body.String())
	}
	msg, _ := out["message"].(string)
	if msg == "" {
		t.Fatalf("错误响应缺 message: %s", rec.Body.String())
	}
	if legacy, _ := out["error"].(string); legacy != msg {
		t.Fatalf("兼容字段 error 必须与 message 同值（老客户端只读它），实际 error=%q message=%q", legacy, msg)
	}
	if _, has := out["hint"]; has && strings.TrimSpace(toString(out["hint"])) == "" {
		t.Fatal("hint 不该是空串（不写就不出现，不塞占位）")
	}
}

// TestRestErrorPutsLuaStackTracebackInDetail 是本条需求的核心断言。
//
// 实测样本：REST 返回一整段带 `stack traceback:` 的文本，前端只能正则剥。
// 现在 message 只给人读的主句，栈进 detail —— **两者合起来与旧文本逐字相同**（不丢信息）。
func TestRestErrorPutsLuaStackTracebackInDetail(t *testing.T) {
	s, reg := newTestServer(t)
	// 崩在运行期（不是入参校验）的 Tool：能触发 Lua 运行错误。
	boom := &model.Tool{
		ID: "boom", Name: "Boom", Runtime: model.RuntimeLua,
		Entry:        `error("内部炸了")`,
		InputSchema:  map[string]interface{}{"type": "object"},
		OutputSchema: map[string]interface{}{"type": "object"},
		Versions:     []model.ToolVersion{{Version: "1.0.0", State: model.StateStable}},
	}
	if err := reg.UpsertTool(boom); err != nil {
		t.Fatalf("注册 Tool 失败: %v", err)
	}
	rec, out := do(t, s.Handler(), http.MethodPost, "/api/tools/boom/run", map[string]interface{}{})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("脚本崩期望 400，实际 %d %s", rec.Code, rec.Body.String())
	}
	msg := toString(out["message"])
	detail := toString(out["detail"])
	if detail == "" {
		t.Fatalf("Lua 栈回溯应进 detail，实际: %s", rec.Body.String())
	}
	if !strings.Contains(detail, "stack traceback") {
		t.Fatalf("detail 应含 stack traceback，实际: %q", detail)
	}
	if strings.Contains(msg, "stack traceback") {
		t.Fatalf("message 不该再混入栈回溯（那正是要拆开的东西）: %q", msg)
	}
	// 不丢信息：message + detail == 旧的一整段
	if !strings.Contains(msg+"\n"+detail, "内部炸了") {
		t.Fatalf("原始错误文本必须在 message+detail 里可复原: %s", rec.Body.String())
	}
}

// TestRestErrorCodePrefersDiagnostics：带诊断的错误用**诊断里的真码**，不按状态兜底。
//
// 这是「REST 与 MCP 同源」的关键：同一个内核错误，两面报同一个 code，
// 客户端才能只维护一份错误码表。
func TestRestErrorCodePrefersDiagnostics(t *testing.T) {
	// 用内核的公开入口造一个带诊断的错误（DiagError 的 err 字段未导出，
	// 外部包只能经 Diag/Diagf 产生——这正是「诊断只由内核产生」的设计）。
	base := fmt.Errorf("结构不合法")
	err := engine.Diag("BAD_TOOL_STRUCTURE", base, "input_schema.type", "改成 object")
	w := httptest.NewRecorder()
	writeErr(w, http.StatusBadRequest, err)
	var out map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["code"] != "BAD_TOOL_STRUCTURE" {
		t.Fatalf("有诊断时应报诊断码，实际 %v", out["code"])
	}
	// 无诊断时按状态兜底，且**不假装知道原因**
	plain := httptest.NewRecorder()
	writeErr(plain, http.StatusNotFound, fmt.Errorf("某某不存在"))
	var out2 map[string]interface{}
	if err := json.Unmarshal(plain.Body.Bytes(), &out2); err != nil {
		t.Fatal(err)
	}
	if out2["code"] != "NOT_FOUND" {
		t.Fatalf("无诊断时应按状态兜底为 NOT_FOUND，实际 %v", out2["code"])
	}
	odd := httptest.NewRecorder()
	writeErr(odd, http.StatusTeapot, fmt.Errorf("不知道算什么"))
	var out3 map[string]interface{}
	if err := json.Unmarshal(odd.Body.Bytes(), &out3); err != nil {
		t.Fatal(err)
	}
	if out3["code"] != "UNCLASSIFIED" {
		t.Fatalf("没有诊断也没有状态映射时应明说 UNCLASSIFIED，实际 %v", out3["code"])
	}
}

// TestRestSplitStackKeepsEverything：拆分不得丢内容，也不得凭空造内容。
func TestRestSplitStackKeepsEverything(t *testing.T) {
	plain := "step s1 执行失败: 正常错误"
	if m, d := splitStack(plain); m != plain || d != "" {
		t.Fatalf("无栈时应原样返回，m=%q d=%q", m, d)
	}
	withStack := "step s1 执行失败: lua 执行失败: bad\nstack traceback:\n\t[G]: in function 'call'"
	m, d := splitStack(withStack)
	if m+d != withStack {
		t.Fatalf("拆分后拼回必须与原文逐字相同：\n%q\n%q", m, d)
	}
	if strings.Contains(m, "stack traceback") || !strings.Contains(d, "stack traceback") {
		t.Fatalf("栈应只在 detail 侧：m=%q d=%q", m, d)
	}
}

// TestWrappedInputHintOnlyWhenClearlyWrapped：包一层的提示**只**在无歧义时给。
//
// 三条判据缺一不可：body 只有一个 input 键、值是对象、且 schema 里没有 input 属性。
// 误报的代价（教用户去改一个本来就对的请求）比漏报高。
func TestWrappedInputHintOnlyWhenClearlyWrapped(t *testing.T) {
	schema := func(props ...string) map[string]interface{} {
		pm := map[string]interface{}{}
		for _, p := range props {
			pm[p] = map[string]interface{}{"type": "string"}
		}
		return map[string]interface{}{"type": "object", "properties": pm}
	}
	tool := &model.Tool{ID: "t", InputSchema: schema("name", "password")}

	// 正例：包了一层且 schema 无 input 属性 → 提示
	hint := wrappedInputHint(tool, map[string]interface{}{
		"input": map[string]interface{}{"username": "a", "password": "b"},
	})
	if hint == "" {
		t.Fatal("包了一层时应给提示")
	}
	for _, want := range []string{"扁平", "username", "password"} {
		if !strings.Contains(hint, want) {
			t.Fatalf("提示缺 %q: %s", want, hint)
		}
	}
	// 负向 1：Tool 真有 input 字段 → 那是正当用法，不提示
	withInput := &model.Tool{ID: "t2", InputSchema: schema("input")}
	if got := wrappedInputHint(withInput, map[string]interface{}{
		"input": map[string]interface{}{"a": 1},
	}); got != "" {
		t.Fatalf("Tool 有 input 字段时不该提示，实际: %s", got)
	}
	// 负向 2：body 不止一个键（那就是普通入参，报错另有原因）
	if got := wrappedInputHint(tool, map[string]interface{}{
		"input": map[string]interface{}{"a": 1}, "name": "x",
	}); got != "" {
		t.Fatalf("多键 body 不该提示，实际: %s", got)
	}
	// 负向 3：input 的值不是对象
	if got := wrappedInputHint(tool, map[string]interface{}{"input": "x"}); got != "" {
		t.Fatalf("input 非对象时不该提示，实际: %s", got)
	}
	// 负向 4：Tool 未知（不该凭空提示）
	if got := wrappedInputHint(nil, map[string]interface{}{
		"input": map[string]interface{}{"a": 1},
	}); got != "" {
		t.Fatalf("Tool 为 nil 时不该提示，实际: %s", got)
	}
}

// TestRunToolHintsWrappedInputEndToEnd 是端到端那一半：提示真的出现在响应里。
func TestRunToolHintsWrappedInputEndToEnd(t *testing.T) {
	s, reg := newTestServer(t)
	seed(t, reg)
	// echo 的 schema 只要 name；把入参包一层 → 校验失败 + 应带「扁平」提示。
	_, out := do(t, s.Handler(), http.MethodPost, "/api/tools/echo/run", map[string]interface{}{
		"input": map[string]interface{}{"name": "x"},
	})
	hint := toString(out["hint"])
	if !strings.Contains(hint, "扁平") {
		t.Fatalf("包一层的请求应在 hint 里点名「扁平 body」，实际 hint=%q", hint)
	}
	// 负向对照：扁平且合法 → 不该有这类提示
	_, ok := do(t, s.Handler(), http.MethodPost, "/api/tools/echo/run", map[string]interface{}{"name": "x"})
	if strings.Contains(toString(ok["hint"]), "扁平") {
		t.Fatalf("合法扁平请求不该收到包层提示: %v", ok["hint"])
	}
}

func toString(v interface{}) string {
	s, _ := v.(string)
	return s
}
