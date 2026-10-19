package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"gear/internal/model"
	"gear/internal/registry"
)

func newTestServer(t *testing.T) (*Server, *registry.Registry) {
	t.Helper()
	reg, err := registry.Open(filepath.Join(t.TempDir(), "registry.db"))
	if err != nil {
		t.Fatalf("打开 registry 失败: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	// secrets.db 注入临时库，避免 New 默认落真实 <GEAR_HOME>/secrets.db。
	secretsDB := filepath.Join(t.TempDir(), "secrets.db")
	srv, err := New(reg, WithSecretsDBPath(secretsDB))
	if err != nil {
		t.Fatalf("构造 Server 失败: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return srv, reg
}

func do(t *testing.T, h http.Handler, method, path string, body interface{}) (*httptest.ResponseRecorder, map[string]interface{}) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("编码请求体失败: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var out map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

func doArray(t *testing.T, h http.Handler, method, path string) []map[string]interface{} {
	t.Helper()
	rec, _ := do(t, h, method, path, nil)
	var out []map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("期望 JSON 数组，实际 %s", rec.Body.String())
	}
	return out
}

// 注册一个 lua echo Tool + 使用它的 Flow。
func seed(t *testing.T, reg *registry.Registry) {
	t.Helper()
	tool := &model.Tool{
		ID: "echo", Name: "Echo", Runtime: model.RuntimeLua,
		Entry:        `return { name = ctx.input.name .. "-ok" }`,
		InputSchema:  map[string]interface{}{"type": "object", "properties": map[string]interface{}{"name": map[string]interface{}{"type": "string"}}, "required": []interface{}{"name"}},
		OutputSchema: map[string]interface{}{"type": "object"},
		Versions:     []model.ToolVersion{{Version: "1.0.0", State: model.StateStable}},
	}
	if err := reg.UpsertTool(tool); err != nil {
		t.Fatalf("注册 Tool 失败: %v", err)
	}
	flow := &model.Flow{ID: "f1", Vars: map[string]interface{}{"who": "gear"}, Steps: []model.Step{
		{ID: "s1", Call: "echo", Input: map[string]interface{}{"name": "${vars.who}"}},
	}}
	if err := reg.UpsertFlow(flow); err != nil {
		t.Fatalf("注册 Flow 失败: %v", err)
	}
}

// A-1：healthz。
func TestHealthz(t *testing.T) {
	s, reg := newTestServer(t)
	seed(t, reg)
	rec, out := do(t, s.Handler(), "GET", "/api/healthz", nil)
	if rec.Code != http.StatusOK || out["status"] != "ok" {
		t.Fatalf("healthz 异常: code=%d body=%v", rec.Code, out)
	}
	if fmt.Sprint(out["tools"]) != "1" || fmt.Sprint(out["flows"]) != "1" {
		t.Fatalf("计数不符: %v", out)
	}
}

// A-1b：GET /api/capabilities 返回实时原子能力目录（含内核原语（builtin + pure）+ 默认 ext.ai）。
func TestCapabilitiesEndpoint(t *testing.T) {
	s, _ := newTestServer(t)
	caps := doArray(t, s.Handler(), "GET", "/api/capabilities")

	byName := map[string]map[string]interface{}{}
	for _, c := range caps {
		if n, ok := c["name"].(string); ok {
			byName[n] = c
		}
	}
	for _, want := range []string{"http", "call", "db", "secret", "event", "subscribe"} {
		c, ok := byName[want]
		if !ok || c["kind"] != "builtin" {
			t.Fatalf("缺内置能力 %s: %v", want, caps)
		}
	}
	ai, ok := byName["ext.ai"]
	if !ok || ai["kind"] != "ext" {
		t.Fatalf("缺默认 ext.ai: %v", caps)
	}
	methods, ok := ai["methods"].(map[string]interface{})
	if !ok || methods["complete"] == nil {
		t.Fatalf("ext.ai 应暴露 complete 方法 schema: %v", ai)
	}
}

// A-1c：自定义扩展经 GEAR_EXT_CONFIG 注入后，GET /api/capabilities 自动带上它
// （数据驱动，不复活写死的 ext.ai 清单；与 CLI `gear ai capabilities` 同源）。
func TestCapabilitiesEndpointIncludesCustomExt(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "extensions.yaml")
	if err := os.WriteFile(cfg, []byte(`ext.slack:
  kind: sidecar
  cmd: ["echo"]
  methods: ["send"]
  schemas:
    send:
      description: 发消息到 Slack 频道
      parameters:
        channel: { type: string, description: 频道名, required: true }
        text: { type: string, description: 消息内容, required: true }
`), 0o644); err != nil {
		t.Fatalf("写扩展配置失败: %v", err)
	}
	t.Setenv("GEAR_EXT_CONFIG", cfg)

	reg, err := registry.Open(filepath.Join(dir, "registry.db"))
	if err != nil {
		t.Fatalf("打开 registry 失败: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	srv, err := New(reg)
	if err != nil {
		t.Fatalf("构造 Server 失败: %v", err)
	}

	caps := doArray(t, srv.Handler(), "GET", "/api/capabilities")
	byName := map[string]bool{}
	for _, c := range caps {
		if n, ok := c["name"].(string); ok {
			byName[n] = true
		}
	}
	if !byName["ext.slack"] {
		t.Fatalf("自定义扩展 ext.slack 未出现在能力目录: %v", caps)
	}
}

// A-1d：POST /api/capabilities/invoke 直接执行原子能力（内置走位置参数、ext.* 走单 table）。
// 覆盖：内置 db.exec→db.query 状态共享、ext.* 单 table 通道、未知方法 fail-closed、缺参 fail-closed。
func TestInvokeCapabilityEndpoint(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.Handler()

	// 1) 内置原语 db.exec → db.query 取回同一行（默认宿主 sqlite scratch 跨调用共享）。
	rec, _ := do(t, h, "POST", "/api/capabilities/invoke", map[string]interface{}{
		"name": "db", "method": "exec", "args": map[string]interface{}{"sql": "CREATE TABLE IF NOT EXISTS kv_demo(k TEXT PRIMARY KEY, v TEXT)"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("db.exec(建表) 应 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	rec, _ = do(t, h, "POST", "/api/capabilities/invoke", map[string]interface{}{
		"name": "db", "method": "exec", "args": map[string]interface{}{"sql": "INSERT INTO kv_demo(k,v) VALUES('k','v')"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("db.exec(插入) 应 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	rec, out := do(t, h, "POST", "/api/capabilities/invoke", map[string]interface{}{
		"name": "db", "method": "query", "args": map[string]interface{}{"sql": "SELECT v FROM kv_demo WHERE k='k'"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("db.query 应 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	rows, ok := out["result"].([]interface{})
	if !ok || len(rows) != 1 {
		t.Fatalf("db.query 应返回 1 行，实际: %v", out)
	}
	row, ok := rows[0].(map[string]interface{})
	if !ok || row["v"] != "v" {
		t.Fatalf("db.query 应返回 v，实际: %v", out)
	}

	// 2) ext.* 单 table 通道：ext.ai.capabilities 返回能力数组。
	rec, out = do(t, h, "POST", "/api/capabilities/invoke", map[string]interface{}{
		"name": "ext.ai", "method": "capabilities", "args": map[string]interface{}{},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("ext.ai.capabilities 应 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if _, ok := out["result"].([]interface{}); !ok {
		t.Fatalf("ext.ai.capabilities result 应为数组: %v", out)
	}

	// 3) 未知内置方法 fail-closed → 400（不静默执行一个不存在的东西）。
	rec, _ = do(t, h, "POST", "/api/capabilities/invoke", map[string]interface{}{
		"name": "http", "method": "Boom", "args": map[string]interface{}{},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("未知方法应 400，实际 %d", rec.Code)
	}

	// 4) 缺 name/method → 400。
	rec, _ = do(t, h, "POST", "/api/capabilities/invoke", map[string]interface{}{"name": "db"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("缺 method 应 400，实际 %d", rec.Code)
	}

	// 5) AC-B6：db.batch 经同一条 gear_invoke 通道可用（禁的是 secret.*，db 不在禁列），
	//    且失败批在这条路径上也是「返回结构」而非抛断——回滚必须可观测。
	rec, _ = do(t, h, "POST", "/api/capabilities/invoke", map[string]interface{}{
		"name": "db", "method": "exec", "args": map[string]interface{}{
			"sql": "CREATE TABLE IF NOT EXISTS batch_demo(k TEXT PRIMARY KEY)",
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("db.exec(建表) 应 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	rec, out = do(t, h, "POST", "/api/capabilities/invoke", map[string]interface{}{
		"name": "db", "method": "batch", "args": map[string]interface{}{
			"statements": []interface{}{
				map[string]interface{}{"sql": "INSERT INTO batch_demo VALUES('a')"},
				map[string]interface{}{"sql": "INSERT INTO no_such_table VALUES(1)"},
			},
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("db.batch 应 200（失败走返回结构），实际 %d: %s", rec.Code, rec.Body.String())
	}
	res, ok := out["result"].(map[string]interface{})
	if !ok {
		t.Fatalf("db.batch result 应为对象: %v", out)
	}
	if res["committed"] != false {
		t.Fatalf("含失败语句的批应 committed=false，实际 %v", res["committed"])
	}
	results, ok := res["results"].([]interface{})
	if !ok || len(results) != 2 {
		t.Fatalf("results 应有 2 条，实际 %v", res["results"])
	}
	// 部分写入必须查不到（同一条通道再查一次）。
	_, out = do(t, h, "POST", "/api/capabilities/invoke", map[string]interface{}{
		"name": "db", "method": "query", "args": map[string]interface{}{
			"sql": "SELECT k FROM batch_demo",
		},
	})
	rows, ok = out["result"].([]interface{})
	if !ok || len(rows) != 0 {
		t.Fatalf("回滚后应查不到写入，实际 %v", out["result"])
	}
}

// A-2：tools CRUD + 注销。
func TestToolsCRUD(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.Handler()

	tool := map[string]interface{}{
		"id": "t1", "name": "T1", "runtime": "process", "entry": "cat",
		"input_schema":  map[string]interface{}{"type": "object"},
		"output_schema": map[string]interface{}{"type": "object"},
		"versions":      []map[string]interface{}{{"version": "1.0.0", "state": "stable"}},
	}
	rec, _ := do(t, h, "POST", "/api/tools", tool)
	if rec.Code != http.StatusCreated {
		t.Fatalf("注册 Tool 期望 201，实际 %d %s", rec.Code, rec.Body.String())
	}
	if got := len(doArray(t, h, "GET", "/api/tools")); got != 1 {
		t.Fatalf("列出应有 1 个，实际 %d", got)
	}
	rec, _ = do(t, h, "DELETE", "/api/tools/t1", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("注销期望 200，实际 %d", rec.Code)
	}
	if got := len(doArray(t, h, "GET", "/api/tools")); got != 0 {
		t.Fatalf("注销后应为 0，实际 %d", got)
	}
	// 再删 → 404
	rec, _ = do(t, h, "DELETE", "/api/tools/t1", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("删不存在的期望 404，实际 %d", rec.Code)
	}
}

// A-3b：GET /api/tools/{id} —— 单查 Tool。
// 此前 UI 只能「拉全量列表再本地索引」；补上单查后可按 id 直取，不存在 → 404。
func TestGetToolEndpoint(t *testing.T) {
	s, reg := newTestServer(t)
	seed(t, reg)
	h := s.Handler()

	rec, out := do(t, h, "GET", "/api/tools/echo", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("单查已注册 Tool 期望 200，实际 %d %s", rec.Code, rec.Body.String())
	}
	if out["id"] != "echo" {
		t.Fatalf("返回体应是被查的那个 Tool，实际 %v", out)
	}
	// 详情端点的意义就在于把 versions 一并带回来（列表里也能看到，但这里是契约的一部分）。
	if _, ok := out["versions"]; !ok {
		t.Fatalf("单查应含 versions，实际 %v", out)
	}

	rec, _ = do(t, h, "GET", "/api/tools/nope", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("单查不存在的 Tool 期望 404，实际 %d", rec.Code)
	}
}

// A-2b：REST promote 端点（对应 CLI `tool promote`）：合法晋升 / 非法 target / 不存在版本 / 不存在 Tool。
func TestPromoteVersion(t *testing.T) {
	s, reg := newTestServer(t)
	h := s.Handler()
	tool := &model.Tool{
		ID: "prom", Name: "Prom", Runtime: model.RuntimeProcess, Entry: "cat",
		InputSchema:  map[string]interface{}{"type": "object"},
		OutputSchema: map[string]interface{}{"type": "object"},
		Versions:     []model.ToolVersion{{Version: "1.0.0", State: model.StateDraft}},
	}
	if err := reg.UpsertTool(tool); err != nil {
		t.Fatalf("注册 Tool 失败: %v", err)
	}

	// 合法晋升 draft -> testing
	rec, _ := do(t, h, "POST", "/api/tools/prom/versions/1.0.0/promote", promoteReq{Target: "testing"})
	if rec.Code != http.StatusOK {
		t.Fatalf("晋升期望 200，实际 %d %s", rec.Code, rec.Body.String())
	}
	// 验证状态已落库
	got, err := reg.GetTool("prom")
	if err != nil || got == nil {
		t.Fatalf("取回 Tool 失败: %v", err)
	}
	if got.Versions[0].State != model.StateTesting {
		t.Fatalf("晋升后状态应为 testing，实际 %s", got.Versions[0].State)
	}

	// 非法 target -> 400
	rec, _ = do(t, h, "POST", "/api/tools/prom/versions/1.0.0/promote", promoteReq{Target: "bogus"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非法 target 期望 400，实际 %d %s", rec.Code, rec.Body.String())
	}

	// target 为空 -> 400
	rec, _ = do(t, h, "POST", "/api/tools/prom/versions/1.0.0/promote", promoteReq{Target: ""})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("空 target 期望 400，实际 %d %s", rec.Code, rec.Body.String())
	}

	// 不存在版本 -> 404
	rec, _ = do(t, h, "POST", "/api/tools/prom/versions/9.9.9/promote", promoteReq{Target: "stable"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("不存在版本期望 404，实际 %d %s", rec.Code, rec.Body.String())
	}

	// 不存在 Tool -> 404
	rec, _ = do(t, h, "POST", "/api/tools/nope/versions/1.0.0/promote", promoteReq{Target: "stable"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("不存在 Tool 期望 404，实际 %d %s", rec.Code, rec.Body.String())
	}
}

// A-3：非法 runtime 的 Tool → 400（注册期校验经由 REST 生效）。
func TestCreateToolInvalid(t *testing.T) {
	s, _ := newTestServer(t)
	tool := map[string]interface{}{
		"id": "bad", "runtime": "foobar", "entry": "x",
		"input_schema":  map[string]interface{}{"type": "object"},
		"output_schema": map[string]interface{}{"type": "object"},
		"versions":      []map[string]interface{}{{"version": "1.0.0", "state": "stable"}},
	}
	rec, _ := do(t, s.Handler(), "POST", "/api/tools", tool)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非法 runtime 期望 400，实际 %d %s", rec.Code, rec.Body.String())
	}
}

// A-4：被 Flow 引用的 Tool 注销 → 409（§8.2 守卫）。
func TestDeleteToolInUse(t *testing.T) {
	s, reg := newTestServer(t)
	seed(t, reg)
	rec, out := do(t, s.Handler(), "DELETE", "/api/tools/echo", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("被引用的 Tool 注销期望 409，实际 %d", rec.Code)
	}
	if msg, _ := out["error"].(string); msg == "" {
		t.Fatal("应返回错误原因")
	}
}

// A-5：被 Hook action 引用的 Tool 注销 → 409。
func TestDeleteToolUsedByHook(t *testing.T) {
	s, reg := newTestServer(t)
	if err := reg.UpsertTool(&model.Tool{
		ID: "mask", Name: "M", Runtime: model.RuntimeLua, Entry: `return {}`,
		InputSchema:  map[string]interface{}{"type": "object"},
		OutputSchema: map[string]interface{}{"type": "object"},
		Versions:     []model.ToolVersion{{Version: "1.0.0", State: model.StateStable}},
	}); err != nil {
		t.Fatalf("注册 Tool 失败: %v", err)
	}
	if err := reg.UpsertHook(&model.Hook{ID: "h1", Target: "*", Stage: model.StageBeforeStep, Action: "mask"}); err != nil {
		t.Fatalf("注册 Hook 失败: %v", err)
	}
	rec, _ := do(t, s.Handler(), "DELETE", "/api/tools/mask", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("被 Hook 引用的 Tool 注销期望 409，实际 %d", rec.Code)
	}
}

// A-6：flows 保存 + 静态校验（含引用不存在 Tool 的拒绝）。
func TestFlowsAndValidate(t *testing.T) {
	s, reg := newTestServer(t)
	seed(t, reg)
	h := s.Handler()

	// 合法 Flow
	okFlow := map[string]interface{}{
		"id": "f2", "steps": []map[string]interface{}{
			{"id": "s1", "call": "echo", "when": "true", "input": map[string]interface{}{"name": "x"}},
		},
	}
	if rec, _ := do(t, h, "POST", "/api/flows", okFlow); rec.Code != http.StatusCreated {
		t.Fatalf("保存 Flow 期望 201，实际 %d %s", rec.Code, rec.Body.String())
	}
	rec, out := do(t, h, "POST", "/api/flows/f2/validate", nil)
	if rec.Code != http.StatusOK || out["valid"] != true {
		t.Fatalf("校验应为 valid=true，实际 %d %v", rec.Code, out)
	}

	// 引用不存在的 Tool → 400
	badFlow := map[string]interface{}{
		"id": "f3", "steps": []map[string]interface{}{{"id": "s1", "call": "nope", "input": map[string]interface{}{}}},
	}
	if rec, _ := do(t, h, "POST", "/api/flows", badFlow); rec.Code != http.StatusBadRequest {
		t.Fatalf("引用不存在 Tool 期望 400，实际 %d", rec.Code)
	}

	// 校验不存在的 Flow → 404
	if rec, _ := do(t, h, "POST", "/api/flows/ghost/validate", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("校验不存在 Flow 期望 404，实际 %d", rec.Code)
	}
}

// TestFlowsAcceptPrimitiveCalls：REST 注册面必须与运行期「同样的『call 判定』」——
// step.call 可以是「能力.方法」（db.exec / call.Call / ext.ai.capabilities），无需先包一层 Tool。
//
// 回归点（真实 bug）：handleCreateFlow/handleValidateFlow 曾用 engine.New(s.reg)（一次性、
// 丢弃 capReg/agent）做静态校验，导致 ext.* 这类需要 capReg 才能识别的方法在 POST 阶段被
// 误判为「既不是已注册的 Tool」而 400——注册期判定比运行期更严，两处判定漂移。
// 这里断言：内建原语 + ext.* 原语都能过注册；且缺参数/未知方法仍在注册期 fail-closed 拒绝。
func TestFlowsAcceptPrimitiveCalls(t *testing.T) {
	s, reg := newTestServer(t)
	seed(t, reg) // echo Tool（用于 call.Call 的 target 与 Tool-优先对照）
	h := s.Handler()

	cases := []struct {
		name string
		call string
		in   map[string]interface{}
	}{
		{"内建原语 db.exec", "db.exec", map[string]interface{}{"sql": "SELECT 1"}},
		{"内建原语 db.query", "db.query", map[string]interface{}{"sql": "SELECT 1"}},
		{"内建原语 call.Call（调 Tool）", "call.Call", map[string]interface{}{"target": "echo"}},
		// 关键回归：ext.* 需要 capReg 才能识别；修复前此处 400。
		{"扩展原语 ext.ai.capabilities", "ext.ai.capabilities", map[string]interface{}{}},
	}
	for i, tc := range cases {
		id := fmt.Sprintf("pf%d", i)
		flow := map[string]interface{}{
			"id": id, "steps": []map[string]interface{}{
				{"id": "s1", "call": tc.call, "input": tc.in},
			},
		}
		rec, _ := do(t, h, "POST", "/api/flows", flow)
		if rec.Code != http.StatusCreated {
			t.Fatalf("[%s] 注册应 201，实际 %d %s", tc.name, rec.Code, rec.Body.String())
		}
		rec, out := do(t, h, "POST", "/api/flows/"+id+"/validate", nil)
		if rec.Code != http.StatusOK || out["valid"] != true {
			t.Fatalf("[%s] 校验应 valid=true，实际 %d %v", tc.name, rec.Code, out)
		}
	}

	// 注册期 fail-closed：以下都应在 POST 阶段被拒（与运行期同源判定）。
	bad := []struct {
		name string
		call string
		in   map[string]interface{}
	}{
		{"未知能力", "nope.put", map[string]interface{}{"key": "k"}},
		{"已知能力但未知方法", "db.nope", map[string]interface{}{"sql": "SELECT 1"}},
		{"缺必填参数", "db.exec", map[string]interface{}{}}, // 缺 sql
		{"多余未知参数", "db.query", map[string]interface{}{"sql": "SELECT 1", "oops": "x"}},
	}
	for i, tc := range bad {
		flow := map[string]interface{}{
			"id": fmt.Sprintf("pbad%d", i), "steps": []map[string]interface{}{
				{"id": "s1", "call": tc.call, "input": tc.in},
			},
		}
		if rec, _ := do(t, h, "POST", "/api/flows", flow); rec.Code != http.StatusBadRequest {
			t.Fatalf("[%s] 期望注册期 400，实际 %d %s", tc.name, rec.Code, rec.Body.String())
		}
	}
}

// A-6b：GET /api/flows 列出已注册 Flow（消除 handleListFlows 0% 覆盖）。
func TestListFlows(t *testing.T) {
	s, reg := newTestServer(t)
	seed(t, reg)
	h := s.Handler()

	// 空库时仍返回 200 + 空数组（seed 已注册 f1）。
	list := doArray(t, h, "GET", "/api/flows")
	found := map[string]bool{}
	for _, fl := range list {
		if id, ok := fl["id"].(string); ok {
			found[id] = true
		}
	}
	if !found["f1"] {
		t.Fatalf("GET /api/flows 应含 seed 的 f1，实际 %v", found)
	}

	// 再注册一条，列表应增长且包含新 id。
	newFlow := map[string]interface{}{
		"id": "f-extra", "steps": []map[string]interface{}{
			{"id": "s1", "call": "echo", "input": map[string]interface{}{"name": "x"}},
		},
	}
	if rec, _ := do(t, h, "POST", "/api/flows", newFlow); rec.Code != http.StatusCreated {
		t.Fatalf("保存 Flow 期望 201，实际 %d %s", rec.Code, rec.Body.String())
	}
	list2 := doArray(t, h, "GET", "/api/flows")
	if len(list2) <= len(list) {
		t.Fatalf("注册新 Flow 后列表应增长：前 %d 后 %d", len(list), len(list2))
	}
	ids := map[string]bool{}
	for _, fl := range list2 {
		if id, ok := fl["id"].(string); ok {
			ids[id] = true
		}
	}
	if !ids["f-extra"] {
		t.Fatalf("GET /api/flows 应含新注册的 f-extra，实际 %v", ids)
	}
}

// A-6b：DELETE /api/flows/{id} —— 注销 Flow。
// 覆盖三种结果：不存在 404 / 成功 200 / 被引用 409（§8.2 守卫在 registry 层判定，REST 只映射状态码）。
func TestDeleteFlowEndpoint(t *testing.T) {
	s, reg := newTestServer(t)
	seed(t, reg) // echo Tool + Flow f1
	h := s.Handler()

	// 不存在 → 404
	if rec, _ := do(t, h, "DELETE", "/api/flows/ghost", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("删不存在的 Flow 期望 404，实际 %d", rec.Code)
	}

	// 无引用 → 200，且库里真的没了
	if rec, _ := do(t, h, "DELETE", "/api/flows/f1", nil); rec.Code != http.StatusOK {
		t.Fatalf("注销 Flow 期望 200，实际 %d", rec.Code)
	}
	if got, _ := reg.GetFlow("f1"); got != nil {
		t.Fatal("注销后不应再读到 f1")
	}

	// 被 Flow 级 Hook 的 action 引用 → 409（ErrInUse）
	if err := reg.UpsertFlow(&model.Flow{ID: "sub", Steps: []model.Step{
		{ID: "s1", Call: "echo", Input: map[string]interface{}{"name": "x"}},
	}}); err != nil {
		t.Fatalf("注册 Flow 失败: %v", err)
	}
	if err := reg.UpsertHook(&model.Hook{ID: "hbf", Target: "*", Stage: model.StageBeforeFlow, Action: "sub"}); err != nil {
		t.Fatalf("注册 Flow 级 Hook 失败: %v", err)
	}
	rec, out := do(t, h, "DELETE", "/api/flows/sub", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("被引用的 Flow 应回 409，实际 %d %s", rec.Code, rec.Body.String())
	}
	// 拒绝原因必须透传（排障第一手线索：告诉用户是谁挡住了）。
	if msg, _ := out["error"].(string); !strings.Contains(msg, "hbf") {
		t.Fatalf("409 应指明引用者 hbf，实际 %v", out)
	}

	// 清掉引用后可删
	if err := reg.DeleteHook("hbf"); err != nil {
		t.Fatalf("删 hbf 失败: %v", err)
	}
	if rec, _ := do(t, h, "DELETE", "/api/flows/sub", nil); rec.Code != http.StatusOK {
		t.Fatalf("引用清除后应可注销，实际 %d", rec.Code)
	}
}

// A-7：hooks 列出/添加。
func TestHooks(t *testing.T) {
	s, reg := newTestServer(t)
	if err := reg.UpsertTool(&model.Tool{
		ID: "mask", Name: "M", Runtime: model.RuntimeLua, Entry: `return {}`,
		InputSchema:  map[string]interface{}{"type": "object"},
		OutputSchema: map[string]interface{}{"type": "object"},
		Versions:     []model.ToolVersion{{Version: "1.0.0", State: model.StateStable}},
	}); err != nil {
		t.Fatalf("注册 Tool 失败: %v", err)
	}
	h := s.Handler()
	hook := map[string]interface{}{
		"id": "h1", "target": "*", "stage": "before_step", "action": "mask", "criticality": "best-effort",
	}
	if rec, _ := do(t, h, "POST", "/api/hooks", hook); rec.Code != http.StatusCreated {
		t.Fatalf("添加 Hook 期望 201，实际 %d %s", rec.Code, rec.Body.String())
	}
	if got := len(doArray(t, h, "GET", "/api/hooks")); got != 1 {
		t.Fatalf("Hook 列表应为 1，实际 %d", got)
	}
	// 非法 stage → 400
	bad := map[string]interface{}{"id": "h2", "target": "*", "stage": "whenever", "action": "mask"}
	if rec, _ := do(t, h, "POST", "/api/hooks", bad); rec.Code != http.StatusBadRequest {
		t.Fatalf("非法 Hook 期望 400，实际 %d", rec.Code)
	}
}

// A-7c：DELETE /api/hooks/{id} —— 删除 Hook。
// Hook 不被别的定义引用，所以没有引用守卫；但「不存在」必须回 404 而非静默成功。
func TestDeleteHookEndpoint(t *testing.T) {
	s, reg := newTestServer(t)
	seed(t, reg)
	h := s.Handler()

	body := map[string]interface{}{
		"id": "h1", "target": "*", "stage": "before_step", "action": "echo", "criticality": "best-effort",
	}
	if rec, _ := do(t, h, "POST", "/api/hooks", body); rec.Code != http.StatusCreated {
		t.Fatalf("注册 Hook 期望 201，实际 %d %s", rec.Code, rec.Body.String())
	}
	if rec, _ := do(t, h, "DELETE", "/api/hooks/h1", nil); rec.Code != http.StatusOK {
		t.Fatalf("删 Hook 期望 200，实际 %d", rec.Code)
	}
	if got, _ := reg.GetHook("h1"); got != nil {
		t.Fatal("删除后不应再读到 h1")
	}
	// 再删 → 404（不静默 no-op）
	if rec, _ := do(t, h, "DELETE", "/api/hooks/h1", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("删不存在的 Hook 期望 404，实际 %d", rec.Code)
	}
}

// A-8：执行闭环——POST 202 → 轮询成功 → 结果含 step 输出。
func TestExecutionLifecycle(t *testing.T) {
	s, reg := newTestServer(t)
	seed(t, reg)
	h := s.Handler()

	rec, out := do(t, h, "POST", "/api/executions", map[string]interface{}{"flow_id": "f1"})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("触发执行期望 202，实际 %d %s", rec.Code, rec.Body.String())
	}
	id, _ := out["id"].(string)
	if id == "" {
		t.Fatal("应返回 execution id")
	}

	final := waitStatus(t, h, id, StatusSucceeded)
	result, _ := final["result"].(map[string]interface{})
	steps, _ := result["steps"].(map[string]interface{})
	if _, ok := steps["s1"]; !ok {
		t.Fatalf("结果应包含 s1：%v", final)
	}
}

// A-9：失败的执行 → status=failed 且带 error。
func TestExecutionFailure(t *testing.T) {
	s, reg := newTestServer(t)
	if err := reg.UpsertTool(&model.Tool{
		ID: "boom", Name: "B", Runtime: model.RuntimeProcess, Entry: "exit 3",
		InputSchema:  map[string]interface{}{"type": "object"},
		OutputSchema: map[string]interface{}{"type": "object"},
		Versions:     []model.ToolVersion{{Version: "1.0.0", State: model.StateStable}},
	}); err != nil {
		t.Fatalf("注册 Tool 失败: %v", err)
	}
	if err := reg.UpsertFlow(&model.Flow{ID: "bad", Steps: []model.Step{
		{ID: "s1", Call: "boom", Input: map[string]interface{}{}},
	}}); err != nil {
		t.Fatalf("注册 Flow 失败: %v", err)
	}

	h := s.Handler()
	_, out := do(t, h, "POST", "/api/executions", map[string]interface{}{"flow_id": "bad"})
	final := waitStatus(t, h, out["id"].(string), StatusFailed)
	if msg, _ := final["error"].(string); msg == "" {
		t.Fatal("失败执行应带 error 说明")
	}
}

// A-10：触发不存在的 Flow → 404；查询不存在的执行 → 404。
func TestExecutionNotFound(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.Handler()
	if rec, _ := do(t, h, "POST", "/api/executions", map[string]interface{}{"flow_id": "ghost"}); rec.Code != http.StatusNotFound {
		t.Fatalf("不存在 Flow 期望 404，实际 %d", rec.Code)
	}
	if rec, _ := do(t, h, "GET", "/api/executions/exec_nope", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("不存在执行记录期望 404，实际 %d", rec.Code)
	}
}

// A-13（REQ-API-LOGS / #71）：执行日志端点——step 的 stdout/stderr 行被采集，
// 且按 (exec, step, stream) 关联可查；不存在的执行 → 404。
func TestExecutionLogs(t *testing.T) {
	s, reg := newTestServer(t)
	seed(t, reg)
	h := s.Handler()

	// 注册一个会打印多行 stdout + 一行 stderr 的 process 工具。
	objSchema := map[string]interface{}{"type": "object"}
	if err := reg.UpsertTool(&model.Tool{
		ID: "logt", Name: "logt", Runtime: model.RuntimeProcess,
		Entry:       "echo 'boot-line'; echo '{\"ok\":true}'; echo 'warn-stderr' >&2",
		InputSchema: objSchema, OutputSchema: objSchema,
		Versions: []model.ToolVersion{{Version: "1.0.0", State: model.StateStable}},
	}); err != nil {
		t.Fatalf("注册 logt 失败: %v", err)
	}
	if err := reg.UpsertFlow(&model.Flow{ID: "flog", Steps: []model.Step{{ID: "s1", Call: "logt"}}}); err != nil {
		t.Fatalf("注册 flog 失败: %v", err)
	}

	rec, out := do(t, h, "POST", "/api/executions", map[string]interface{}{"flow_id": "flog"})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("触发执行期望 202，实际 %d", rec.Code)
	}
	id := out["id"].(string)
	waitStatus(t, h, id, StatusSucceeded)

	logs := s.execs.Logs(id)
	var gotStdout, gotStderr bool
	for _, l := range logs {
		if l.Stream == "stdout" && l.Line == "boot-line" {
			gotStdout = true
		}
		if l.Stream == "stderr" && l.Line == "warn-stderr" {
			gotStderr = true
		}
	}
	if !gotStdout {
		t.Fatalf("日志应含 stdout 行 boot-line，实际: %+v", logs)
	}
	if !gotStderr {
		t.Fatalf("日志应含 stderr 行 warn-stderr，实际: %+v", logs)
	}

	// 端点：直接 GET 也应返回 200（内容已通过 s.execs.Logs 校验）。
	rec2, _ := do(t, h, "GET", "/api/executions/"+id+"/logs", nil)
	if rec2.Code != http.StatusOK {
		t.Fatalf("日志端点期望 200，实际 %d", rec2.Code)
	}

	// 不存在的执行 → 404。
	if rec3, _ := do(t, h, "GET", "/api/executions/exec_nope/logs", nil); rec3.Code != http.StatusNotFound {
		t.Fatalf("不存在执行日志期望 404，实际 %d", rec3.Code)
	}
}

// A-11：apps 生命周期——启动 service Flow、列出、停止。
func TestAppsLifecycle(t *testing.T) {
	s, reg := newTestServer(t)
	if err := reg.UpsertTool(&model.Tool{
		ID: "sleepy", Name: "S", Runtime: model.RuntimeProcess, Entry: "sleep 3",
		InputSchema:  map[string]interface{}{"type": "object"},
		OutputSchema: map[string]interface{}{"type": "object"},
		Versions:     []model.ToolVersion{{Version: "1.0.0", State: model.StateStable}},
	}); err != nil {
		t.Fatalf("注册 Tool 失败: %v", err)
	}
	if err := reg.UpsertFlow(&model.Flow{ID: "svcf", Steps: []model.Step{
		{ID: "keep", Call: "sleepy", Mode: "service", Input: map[string]interface{}{}},
	}}); err != nil {
		t.Fatalf("注册 Flow 失败: %v", err)
	}

	h := s.Handler()
	rec, out := do(t, h, "POST", "/api/apps", map[string]interface{}{"flow_id": "svcf"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("启动 app 期望 201，实际 %d %s", rec.Code, rec.Body.String())
	}
	id, _ := out["id"].(string)
	if id == "" {
		t.Fatal("应返回 app id")
	}
	if got := len(doArray(t, h, "GET", "/api/apps")); got != 1 {
		t.Fatalf("app 列表应为 1，实际 %d", got)
	}
	if rec, _ := do(t, h, "DELETE", "/api/apps/"+id, nil); rec.Code != http.StatusOK {
		t.Fatalf("停止 app 期望 200，实际 %d", rec.Code)
	}
	s.Shutdown()
	if got := len(doArray(t, h, "GET", "/api/apps")); got != 0 {
		t.Fatalf("停止后 app 列表应为 0，实际 %d", got)
	}
	// 重复停止 → 404
	if rec, _ := do(t, h, "DELETE", "/api/apps/"+id, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("停止不存在的 app 期望 404，实际 %d", rec.Code)
	}
}

// A-12：并发触发同一个 Flow——store 必须并发安全（-race 下运行）。
func TestConcurrentExecutions(t *testing.T) {
	s, reg := newTestServer(t)
	seed(t, reg)
	h := s.Handler()

	const n = 8
	ids := make([]string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, out := do(t, h, "POST", "/api/executions", map[string]interface{}{"flow_id": "f1"})
			if id, ok := out["id"].(string); ok {
				ids[i] = id
			}
		}(i)
	}
	wg.Wait()

	for _, id := range ids {
		if id == "" {
			t.Fatal("并发触发有请求未拿到 id")
		}
		waitStatus(t, h, id, StatusSucceeded)
	}
	// 持久化后 List 会包含历史执行（同进程内多次测试共享 SQLite 文件），
	// 故只断言本次并发触发的 n 个 id 都在列表中，而非精确计数。
	list := s.execs.List()
	present := map[string]bool{}
	for _, e := range list {
		present[e.ID] = true
	}
	for _, id := range ids {
		if !present[id] {
			t.Fatalf("并发触发的执行 %s 未出现在执行列表中（列表共 %d 条）", id, len(list))
		}
	}
}

// waitStatus 轮询执行状态直到达到期望值（超时即失败）。
func waitStatus(t *testing.T, h http.Handler, id, want string) map[string]interface{} {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rec, out := do(t, h, "GET", "/api/executions/"+id, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("查询执行状态失败: %d", rec.Code)
		}
		if out["status"] == want {
			return out
		}
		if out["status"] == StatusFailed && want == StatusSucceeded {
			t.Fatalf("执行失败: %v", out["error"])
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待状态 %s 超时", want)
	return nil
}

// A-13：§8.2 第三条守卫——「运行中」引用不可注销（静态引用已消失的窄缝）。
//
// 刻意构造静态引用守卫**看不见**的情形：
//  1. 运行中的执行在启动时把 Hook h1（action=mask）读进内存快照；
//  2. 删掉 h1 ⇒ Registry 再也看不到对 mask 的引用（也没有任何 Flow 引用它）；
//  3. 此时若放行注销，正在跑的执行会在下次触发 Hook 时拿到 nil Tool 而中途失败；
//  4. 执行结束后必须恢复可注销 —— 守卫只约束「运行中」，不能变成永久锁死。
func TestDeleteToolWhileRunning(t *testing.T) {
	s, reg := newTestServer(t)
	h := s.Handler()

	obj := func() map[string]interface{} { return map[string]interface{}{"type": "object"} }
	mkTool := func(id, rt, entry string) *model.Tool {
		return &model.Tool{
			ID: id, Name: id, Runtime: model.Runtime(rt), Entry: entry,
			InputSchema: obj(), OutputSchema: obj(),
			Versions: []model.ToolVersion{{Version: "1.0.0", State: model.StateStable}},
		}
	}
	// 慢工具：为「执行仍在跑」留出可观测窗口。断言全部基于轮询状态，
	// 不使用固定时长阈值（绝对时长在多包并行跑时会抖动）。
	if err := reg.UpsertTool(mkTool("slow", "process", `sleep 1.5; echo '{"ok":true}'`)); err != nil {
		t.Fatalf("注册 Tool 失败: %v", err)
	}
	if err := reg.UpsertTool(mkTool("mask", "lua", `return {}`)); err != nil {
		t.Fatalf("注册 Tool 失败: %v", err)
	}
	if err := reg.UpsertHook(&model.Hook{ID: "h1", Target: "*", Stage: model.StageBeforeStep, Action: "mask"}); err != nil {
		t.Fatalf("注册 Hook 失败: %v", err)
	}
	if err := reg.UpsertFlow(&model.Flow{ID: "f", Steps: []model.Step{
		{ID: "s1", Call: "slow", Input: map[string]interface{}{}},
	}}); err != nil {
		t.Fatalf("注册 Flow 失败: %v", err)
	}

	rec, out := do(t, h, "POST", "/api/executions", map[string]interface{}{"flow_id": "f"})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("触发执行期望 202，实际 %d %s", rec.Code, rec.Body.String())
	}
	id, _ := out["id"].(string)

	// 等引擎回报 tool_ids：这是「已加载快照、确实在运行」的唯一可靠信号。
	waitToolID(t, h, id, "mask")

	// 删 Hook ⇒ mask 在 Registry 里再无静态引用。
	if err := reg.DeleteHook("h1"); err != nil {
		t.Fatalf("删 Hook 失败: %v", err)
	}
	drec, dout := do(t, h, "DELETE", "/api/tools/mask", nil)
	if drec.Code != http.StatusConflict {
		t.Fatalf("运行中使用的 Tool 注销期望 409，实际 %d %s", drec.Code, drec.Body.String())
	}
	if msg, _ := dout["error"].(string); !strings.Contains(msg, "运行中") {
		t.Fatalf("拒绝原因应说明「运行中」，实际 %q", msg)
	}

	// 执行结束后恢复可注销。
	waitStatus(t, h, id, StatusSucceeded)
	if rec, _ := do(t, h, "DELETE", "/api/tools/mask", nil); rec.Code != http.StatusOK {
		t.Fatalf("执行结束后注销期望 200，实际 %d %s", rec.Code, rec.Body.String())
	}
}

// waitToolID 轮询执行记录，直到 tool_ids 出现指定 Tool（超时即失败）。
func waitToolID(t *testing.T, h http.Handler, id, toolID string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rec, out := do(t, h, "GET", "/api/executions/"+id, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("查询执行状态失败: %d", rec.Code)
		}
		if ids, ok := out["tool_ids"].([]interface{}); ok {
			for _, v := range ids {
				if v == toolID {
					return
				}
			}
		}
		if out["status"] != StatusRunning {
			t.Fatalf("执行在回报 tool_ids 前已结束: %v", out)
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待执行回报 tool_ids 含 %s 超时", toolID)
}

// A-14（回归锁）：保存**不含 when** 的 Flow 必须成功。
//
// 修复前的行为：`ValidateFlowStatic` 对空 when 报 `unexpected token EOF`，
// 于是 REST 的保存入口对「普通 Flow」一律 400 —— 而既有用例恰好都写了 `when: "true"`，
// 覆盖不到这条主干路径。此处直接锁住主干：不带 when 的 Flow → 201。
func TestCreateFlowWithoutWhen(t *testing.T) {
	s, reg := newTestServer(t)
	seed(t, reg)

	rec, out := do(t, s.Handler(), "POST", "/api/flows", map[string]interface{}{
		"id": "nowhen",
		"steps": []map[string]interface{}{
			{"id": "s1", "call": "echo", "input": map[string]interface{}{"name": "x"}},
		},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("不含 when 的 Flow 保存期望 201，实际 %d %s", rec.Code, rec.Body.String())
	}
	_ = out
}

// A-15：执行列表端点——POST 触发后，GET /api/executions 应返回含该 Flow 的记录。
func TestListExecutions(t *testing.T) {
	s, reg := newTestServer(t)
	seed(t, reg)

	do(t, s.Handler(), "POST", "/api/executions", map[string]interface{}{"flow_id": "f1"})

	list := doArray(t, s.Handler(), "GET", "/api/executions")
	if len(list) == 0 {
		t.Fatal("执行列表不应为空")
	}
	found := false
	for _, e := range list {
		if e["flow_id"] == "f1" {
			found = true
		}
	}
	if !found {
		t.Fatal("执行列表应含 f1 的执行记录")
	}
}

// mockInferrer 已随 AI 去内置化（M-EXT-AI，2026-09-14）移除：server 不再持有 inferrer 字段、
// /api/ai/infer 端点已删除；AI 改为以 ext.ai Sidecar 扩展承载（examples/gear-ai-sidecar）。

// TestNewsEndpoint 验证 GET /api/news：用本地 mock 百度接口（Baidu 形状 JSON）跑真实示例
// testdata/news-top10.yaml（news.fetch 直接调 ctx.http + ctx.json），确定性、不依赖外网。
func TestNewsEndpoint(t *testing.T) {
	// 本地 mock 百度热搜榜接口，返回与真实接口同构的 JSON。
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"success":true,"data":{"cards":[{"component":"tabTextList","content":[{"content":[`+
			`{"index":1,"word":"测试热点一","url":"https://example.com/1","hotTag":"3"},`+
			`{"index":2,"word":"测试热点二","url":"https://example.com/2","hotTag":"0"},`+
			`{"index":3,"word":"测试热点三","url":"https://example.com/3","hotTag":"1"}`+
			`]}]}]}}`)
	}))
	defer mock.Close()

	_, reg := newTestServer(t)
	tools, flows, err := registry.LoadExamples(filepath.Join(repoRoot(t), "testdata", "news-top10.yaml"))
	if err != nil {
		t.Fatalf("加载示例失败: %v", err)
	}
	// 把 news.top10 的 step input 指向 mock 服务（覆盖默认百度 URL），使测试不依赖外网。
	for _, f := range flows {
		if f.ID == "news.top10" {
			f.Steps[0].Input = map[string]interface{}{"url": mock.URL}
		}
		if err := reg.UpsertFlow(f); err != nil {
			t.Fatalf("灌入 Flow %s 失败: %v", f.ID, err)
		}
	}
	for _, tl := range tools {
		if err := reg.UpsertTool(tl); err != nil {
			t.Fatalf("灌入 Tool %s 失败: %v", tl.ID, err)
		}
	}

	srv, err := New(reg)
	if err != nil {
		t.Fatalf("构造 Server 失败: %v", err)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/news", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/news = %d，body=%s", rec.Code, rec.Body.String())
	}
	var out map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应非 JSON: %v", err)
	}
	items, ok := out["items"].([]interface{})
	if !ok || len(items) != 3 {
		t.Fatalf("期望 3 条 items，实际 %v", out["items"])
	}
	if _, ok := out["served_at"]; !ok {
		t.Fatalf("响应应含 served_at 时间戳，实际 %v", out)
	}
}

// repoRoot 由测试源文件位置向上回溯到仓库根（internal/server → internal → 仓库根）。
func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatalf("runtime.Caller 失败")
	}
	return filepath.Dir(filepath.Dir(filepath.Dir(file)))
}
