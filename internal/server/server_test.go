package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
	return New(reg), reg
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
	if got := len(s.execs.List()); got != n {
		t.Fatalf("应记录 %d 次执行，实际 %d", n, got)
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

// mockInferrer 是 ai.Inferrer 的 Mock（server 测试用）。
type mockInferrer struct {
	text string
	err  error
}

func (m *mockInferrer) Infer(ctx context.Context, prompt string) (string, error) {
	return m.text, m.err
}

// A-16：AI 闭环端点——合法收敛返回 200 + Flow；空 prompt 400；收敛失败（非法 YAML）422。
func TestInferFlow(t *testing.T) {
	reg, err := registry.Open(filepath.Join(t.TempDir(), "registry.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reg.Close() })

	inf := &mockInferrer{text: `id: ai-flow
steps:
  - id: s1
    call: t1
`}
	s := NewWithInferrer(reg, inf)

	rec, out := do(t, s.Handler(), "POST", "/api/ai/infer", map[string]interface{}{"prompt": "make a flow"})
	if rec.Code != http.StatusOK {
		t.Fatalf("合法收敛应 200，实为 %d: %s", rec.Code, rec.Body.String())
	}
	if out["id"] != "ai-flow" {
		t.Fatalf("生成的 flow id 错误: %v", out["id"])
	}

	// 空 prompt → 400
	rec2, _ := do(t, s.Handler(), "POST", "/api/ai/infer", map[string]interface{}{"prompt": ""})
	if rec2.Code != http.StatusBadRequest {
		t.Fatalf("空 prompt 应 400，实为 %d", rec2.Code)
	}

	// 收敛失败（非法 YAML）→ 422
	infBad := &mockInferrer{text: "这根本不是 YAML"}
	sBad := NewWithInferrer(reg, infBad)
	rec3, _ := do(t, sBad.Handler(), "POST", "/api/ai/infer", map[string]interface{}{"prompt": "x"})
	if rec3.Code != http.StatusUnprocessableEntity {
		t.Fatalf("收敛失败应 422，实为 %d: %s", rec3.Code, rec3.Body.String())
	}
}
