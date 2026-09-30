package server

import (
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"gear/internal/model"
	"gear/internal/registry"
)

// apiTool 造一个「把入参原样回出来」的 lua Tool（路径参数是否真的到了入参里，一眼可见）。
func apiTool(id, method, path string) *model.Tool {
	return &model.Tool{
		ID:           id,
		Name:         id,
		Runtime:      model.RuntimeLua,
		Entry:        "return { id = ctx.input.id, partNo = ctx.input.partNo, extra = ctx.input.extra }",
		InputSchema:  map[string]interface{}{"type": "object"},
		OutputSchema: map[string]interface{}{"type": "object"},
		Versions:     []model.ToolVersion{{Version: "1.0.0", State: model.StateStable}},
		API:          &model.APIRoute{Method: method, Path: path},
	}
}

// newTestServerWithTools 造「Tool 已注册」的服务：声明式路由的挂载发生在 New 之前，
// 所以这批用例必须自己先注册再起服（newTestServer 是起服之后才用的场合）。
func newTestServerWithTools(t *testing.T, tools ...*model.Tool) (*Server, *registry.Registry) {
	t.Helper()
	reg, err := registry.Open(filepath.Join(t.TempDir(), "registry.db"))
	if err != nil {
		t.Fatalf("打开 registry 失败: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	for _, tl := range tools {
		if err := reg.UpsertTool(tl); err != nil {
			t.Fatalf("注册 Tool %s 失败: %v", tl.ID, err)
		}
	}
	srv, err := New(reg)
	if err != nil {
		t.Fatalf("构造 Server 失败: %v", err)
	}
	return srv, reg
}

// TestToolAPIRouteRunsAndKeepsRunEndpoint 锁住 FR-3 的 AC-C1：
// 声明 `GET /api/orders/{id}` 的 Tool，资源路由与旧端点**同时可用**，且响应体就是 Tool 输出。
func TestToolAPIRouteRunsAndKeepsRunEndpoint(t *testing.T) {
	s, _ := newTestServerWithTools(t, apiTool("demo__order_get", "GET", "/api/orders/{id}"))
	h := s.Handler()

	rec, out := do(t, h, "GET", "/api/orders/42", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/orders/42 应 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if out["id"] != "42" {
		t.Fatalf("路径参数应映射进 Tool 输入，实际 %v", out)
	}
	// 「响应体即 Tool 输出」：不是 {id, output} 这种旧包装，否则前端拿个资源还得再拆一层。
	if _, wrapped := out["output"]; wrapped {
		t.Fatalf("资源路由的响应体不该带 output 包装: %v", out)
	}

	// AC-C4 / 向后兼容红线：同一个 Tool 的旧端点必须照旧可用。
	rec2, out2 := do(t, h, "POST", "/api/tools/demo__order_get/run", map[string]interface{}{"id": "42"})
	if rec2.Code != http.StatusOK {
		t.Fatalf("POST /api/tools/{id}/run 应仍可用，实际 %d: %s", rec2.Code, rec2.Body.String())
	}
	if m, _ := out2["output"].(map[string]interface{}); m == nil || m["id"] != "42" {
		t.Fatalf("旧端点入参解析被破坏: %v", out2)
	}
}

// TestToolAPIPathParamsAndBody 锁住 AC-C2：多段路径参数按名字进输入，
// 且**路径里的值压过 body 里的同名键**（资源身份不该被 body 顶掉）。
func TestToolAPIPathParamsAndBody(t *testing.T) {
	// 同一条路径上挂 GET 与 POST 两个声明：正好顺手锁住「method 是路由的一部分」。
	s, _ := newTestServerWithTools(t,
		apiTool("demo__part_get", "GET", "/api/items/{id}/parts/{partNo}"),
		apiTool("demo__part_post", "POST", "/api/items/{id}/parts/{partNo}"))
	h := s.Handler()

	rec, out := do(t, h, "GET", "/api/items/7/parts/B-1", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET 两段参数应 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if out["id"] != "7" || out["partNo"] != "B-1" {
		t.Fatalf("两个路径参数都应进输入，实际 %v", out)
	}

	rec, out = do(t, h, "POST", "/api/items/7/parts/B-1", map[string]interface{}{"id": "from-body", "extra": "x"})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST 带 body 应 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if out["id"] != "7" {
		t.Fatalf("路径参数优先级应高于 body 里的同名键，实际 id=%v", out["id"])
	}
	if out["extra"] != "x" {
		t.Fatalf("body 里的其它键应照收，实际 %v", out)
	}
}

// TestToolAPIUnmatchedIsNotFound 锁住「没声明的路由就是没有」：
// 路径对但方法不对（404 而不是执行某个 Tool），路径压根没声明（404 且点名这条路径）。
func TestToolAPIUnmatchedIsNotFound(t *testing.T) {
	s, _ := newTestServerWithTools(t, apiTool("demo__order_get", "GET", "/api/orders/{id}"))
	h := s.Handler()

	rec, _ := do(t, h, "POST", "/api/orders/42", nil) // 该 Tool 只声明了 GET
	if rec.Code != http.StatusNotFound {
		t.Fatalf("方法不对应 404，实际 %d", rec.Code)
	}
	rec, out := do(t, h, "GET", "/api/orders", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未声明的路径应 404，实际 %d", rec.Code)
	}
	if !strings.Contains(fmt.Sprint(out["error"]), "orders") {
		t.Fatalf("404 的报错应点名这条路径，实际 %v", out["error"])
	}
}

// TestToolAPIRoutesNeedNoRestart 锁住「改声明不必重启」：
// 运行期新注册（哪怕是在 New 之后）的 Tool，它的资源路由立刻可用——
// 索引按 registry 的 Tool 表指纹失效，不靠某个时刻的快照。
func TestToolAPIRoutesNeedNoRestart(t *testing.T) {
	s, reg := newTestServer(t)
	h := s.Handler()

	if err := reg.UpsertTool(apiTool("demo__order_get", "GET", "/api/orders/{id}")); err != nil {
		t.Fatalf("注册 Tool 失败: %v", err)
	}
	rec, out := do(t, h, "GET", "/api/orders/42", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("起服后注册的资源路由应立刻可用，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if out["id"] != "42" {
		t.Fatalf("入参映射不对: %v", out)
	}
}

// TestToolAPIDoesNotStealCoreRoutes 是「通配挂载点」这条设计的负向验证：
// `GET /api/{path...}` 绝不能抢走内核自己的路由（AC-C4 的零变化）。
func TestToolAPIDoesNotStealCoreRoutes(t *testing.T) {
	s, _ := newTestServerWithTools(t, apiTool("demo__order_get", "GET", "/api/orders/{id}"))
	h := s.Handler()

	rec, _ := do(t, h, "GET", "/api/tools", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/tools 应仍由内核端点处理，实际 %d: %s", rec.Code, rec.Body.String())
	}
	// /api/healthz 上没有声明路由，必须是 404 而不是被通配挂载点吞掉后报别的错。
	rec, _ = do(t, h, "GET", "/api/healthz", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/healthz 应仍由内核端点处理，实际 %d", rec.Code)
	}
}
