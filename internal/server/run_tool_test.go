package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"gear/internal/model"
	"gear/internal/registry"
)

// seedZeroArgTool 注册一个零入参 lua Tool —— 专门服务「空 body 也能跑」这条用例：
// 零入参 Tool（起服务、读状态、清缓存）在实际应用里很常见，端点是按「复用 Tool」设计的，
// 不该逼调用方为了「没有参数」而伪造一个空 JSON 对象。
func seedZeroArgTool(t *testing.T, reg *registry.Registry) {
	t.Helper()
	tool := &model.Tool{
		ID: "noargs", Name: "NoArgs", Runtime: model.RuntimeLua,
		Entry:        `return { ok = true }`,
		InputSchema:  map[string]interface{}{"type": "object"},
		OutputSchema: map[string]interface{}{"type": "object"},
		Versions:     []model.ToolVersion{{Version: "1.0.0", State: model.StateStable}},
	}
	if err := reg.UpsertTool(tool); err != nil {
		t.Fatalf("注册 Tool 失败: %v", err)
	}
}

// doWithOrigin 发一个带 Origin 头的请求（do 不带 Origin，无法覆盖跨站用例）。
func doWithOrigin(t *testing.T, h http.Handler, method, path, origin string, body interface{}) (*httptest.ResponseRecorder, map[string]interface{}) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("编码请求体失败: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

// TestRunToolEndpoint 锁住 2026-09-22 补上的同步执行入口：
// REST 此前只有异步 /api/executions（跑 Flow），前端想复用 Tool 只能包 Flow 再轮询，
// 或绕过 Tool 裸调 /api/capabilities/invoke —— 后者把 prompt 工程漏进前端 JS。
func TestRunToolEndpoint(t *testing.T) {
	s, reg := newTestServer(t)
	seed(t, reg)

	rec, out := do(t, s.Handler(), "POST", "/api/tools/echo/run", map[string]interface{}{"name": "ge"})
	if rec.Code != http.StatusOK {
		t.Fatalf("同步跑 Tool 应 200，实际 %d body=%s", rec.Code, rec.Body.String())
	}
	output, ok := out["output"].(map[string]interface{})
	if !ok || output["name"] != "ge-ok" {
		t.Fatalf("输出不符: %v", out)
	}
	if out["id"] != "echo" {
		t.Fatalf("响应应回显 id: %v", out)
	}
}

// TestRunToolEndpointEmptyBody：空 body = 无入参执行（零入参 Tool 的正常用法）。
func TestRunToolEndpointEmptyBody(t *testing.T) {
	s, reg := newTestServer(t)
	seedZeroArgTool(t, reg)

	rec, out := do(t, s.Handler(), "POST", "/api/tools/noargs/run", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("空 body 应被当作零入参执行，实际 %d body=%s", rec.Code, rec.Body.String())
	}
	output, _ := out["output"].(map[string]interface{})
	if output["ok"] != true {
		t.Fatalf("输出不符: %v", out)
	}
}

// TestRunToolEndpointErrors：不存在 → 404（与「跑挂了」的 400 分开），坏 body → 400。
//
// 分流的意义：engine.RunTool 对「Tool 不存在」返回的是**无 sentinel 的纯文本错误**，
// 不先查 registry 就会把「id 写错」与「Tool 执行失败」压成同一个状态码，而两者修法相反。
func TestRunToolEndpointErrors(t *testing.T) {
	s, reg := newTestServer(t)
	seed(t, reg)
	h := s.Handler()

	if rec, _ := do(t, h, "POST", "/api/tools/nope/run", map[string]interface{}{}); rec.Code != http.StatusNotFound {
		t.Fatalf("不存在的 Tool 应 404，实际 %d", rec.Code)
	}

	// 坏 JSON：直接发原始字节。
	req := httptest.NewRequest("POST", "/api/tools/echo/run", bytes.NewBufferString("{不是 JSON"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("坏 body 应 400，实际 %d body=%s", rec.Code, rec.Body.String())
	}

	// 缺必填入参：交由 engine 报错 → 400（这里是「跑失败」，不是「找不到」）。
	if rec, _ := do(t, h, "POST", "/api/tools/echo/run", map[string]interface{}{}); rec.Code != http.StatusBadRequest {
		t.Fatalf("缺必填入参应 400，实际 %d", rec.Code)
	}
}

// TestInvokeCapabilityOriginGuard 锁住「绑回环 ≠ 安全」这条：浏览器里的任意网页都能向
// 127.0.0.1 发跨站请求，而本端点可直达 secret/db 等宿主能力。
func TestInvokeCapabilityOriginGuard(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.Handler()
	body := map[string]interface{}{"name": "time", "method": "now", "args": map[string]interface{}{}}

	if rec, _ := doWithOrigin(t, h, "POST", "/api/capabilities/invoke", "http://evil.example", body); rec.Code != http.StatusForbidden {
		t.Fatalf("跨站 Origin 应 403，实际 %d body=%s", rec.Code, rec.Body.String())
	}
	// 本机来源（含反代场景：页面在 8088，Origin 是 127.0.0.1:8088）必须放行，
	// 否则「同源反代」这条唯一的建站路子会被自己的安全策略打死。
	for _, origin := range []string{"http://127.0.0.1:8088", "http://localhost:3000", "http://[::1]:8788"} {
		if rec, _ := doWithOrigin(t, h, "POST", "/api/capabilities/invoke", origin, body); rec.Code != http.StatusOK {
			t.Fatalf("本机 Origin %s 应放行，实际 %d body=%s", origin, rec.Code, rec.Body.String())
		}
	}
	// 无 Origin（curl / CLI / 服务端调用）放行——不给命令行使用加门槛。
	if rec, _ := doWithOrigin(t, h, "POST", "/api/capabilities/invoke", "", body); rec.Code != http.StatusOK {
		t.Fatalf("无 Origin 应放行，实际 %d", rec.Code)
	}
}

// TestInvokeCapabilityBlocksSecret：密钥明文只经 Flow 内部取用，不提供 HTTP 直读口。
func TestInvokeCapabilityBlocksSecret(t *testing.T) {
	s, _ := newTestServer(t)
	for _, name := range []string{"secret", "secret.get", "Secret"} {
		rec, out := do(t, s.Handler(), "POST", "/api/capabilities/invoke",
			map[string]interface{}{"name": name, "method": "get", "args": map[string]interface{}{"name": "X"}})
		if rec.Code != http.StatusForbidden {
			t.Fatalf("能力 %s 应被拒绝（403），实际 %d body=%v", name, rec.Code, out)
		}
	}
}

// TestOriginAllowed 表驱动覆盖判定本身（含畸形 Origin）。
func TestOriginAllowed(t *testing.T) {
	cases := []struct {
		origin string
		want   bool
	}{
		{"", true},
		{"http://127.0.0.1:8787", true},
		{"http://127.0.0.1", true},
		{"http://localhost:5173", true},
		{"http://LOCALHOST:5173", true},
		{"http://[::1]:8080", true},
		{"https://evil.example", false},
		{"null", false},                      // 沙箱 iframe / 本地文件的 Origin
		{"http://127.0.0.1.evil.com", false}, // 前缀伪装
		{"://bad", false},                    // 解析失败
	}
	for _, c := range cases {
		req := httptest.NewRequest("POST", "/api/capabilities/invoke", nil)
		if c.origin != "" {
			req.Header.Set("Origin", c.origin)
		}
		if got := originAllowed(req); got != c.want {
			t.Fatalf("originAllowed(%q) = %v，期望 %v", c.origin, got, c.want)
		}
	}
}
