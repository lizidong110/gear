package server

// apps_state_test.go：锁住 GET /api/apps 的**真实监管态**（REQ-APP-MANAGED-STATE）。
//
// 背景（2026-11-13 审出）：监管 goroutine 自己结束的 app（restart=never 退出 / once Flow
// 跑完 / 启动即失败）不会从内存列表消失，旧实现永远顶着创建时的 running——
// gear UI 长驻页显示正常，实际 app 已停止工作（假绿）。
// 这里锁定三件事：state 随 done 关闭实时重算、重启次数经回调透出、exited 后仍在列表中。

import (
	"net/http"
	"testing"
	"time"

	"gear/internal/model"
	"gear/internal/registry"
)

// onceEchoBundle 的最小 Lua Tool + 纯 once Flow：跑完监管即结束，是最典型的「假绿」输入。
func registerOnceEcho(t *testing.T, reg *registry.Registry) {
	t.Helper()
	if err := reg.UpsertTool(&model.Tool{
		ID: "stateecho", Name: "stateecho", Runtime: model.RuntimeLua, Entry: "return {}",
		InputSchema:  map[string]interface{}{"type": "object"},
		OutputSchema: map[string]interface{}{"type": "object"},
		Versions:     []model.ToolVersion{{Version: "1.0.0", State: model.StateStable}},
	}); err != nil {
		t.Fatalf("注册 Tool 失败: %v", err)
	}
	if err := reg.UpsertFlow(&model.Flow{ID: "stateonce", Steps: []model.Step{
		{ID: "s1", Call: "stateecho", Input: map[string]interface{}{}},
	}}); err != nil {
		t.Fatalf("注册 Flow 失败: %v", err)
	}
}

// TestAppsListReportsExitedAfterSupervisionEnds：once Flow 跑完后，列表里的该 app
// 必须从 running 变 exited（而不是永远 running 的假绿），且条目仍在列表中可查。
func TestAppsListReportsExitedAfterSupervisionEnds(t *testing.T) {
	s, reg := newTestServer(t)
	registerOnceEcho(t, reg)
	h := s.Handler()

	rec, out := do(t, h, "POST", "/api/apps", map[string]interface{}{"flow_id": "stateonce", "restart": "never"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("启动 app 期望 201，实际 %d %s", rec.Code, rec.Body.String())
	}
	if out["state"] != "running" {
		t.Fatalf("创建响应应报 running，实际 %v", out["state"])
	}
	if _, ok := out["restarts"]; !ok {
		t.Fatal("创建响应应含 restarts 字段")
	}

	// 事件驱动轮询（红线 13）：等监管 goroutine 真正退出（done 关闭 → state=exited）。
	deadline := time.Now().Add(15 * time.Second)
	items := doArray(t, h, "GET", "/api/apps")
	for time.Now().Before(deadline) {
		items = doArray(t, h, "GET", "/api/apps")
		if len(items) > 0 && items[0]["state"] == "exited" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(items) != 1 {
		t.Fatalf("监管自结束的 app 仍应留在列表（这正是假绿场景的输入），实际 %d 条", len(items))
	}
	if items[0]["state"] != "exited" {
		t.Fatalf("监管结束后应报 exited，实际 %v", items[0]["state"])
	}
	if got := items[0]["restarts"]; got != float64(0) {
		t.Fatalf("once Flow 不应有重启，restarts=%v", got)
	}
}

// TestAppsListReportsRestartCount：restart=always 的崩溃循环里，/api/apps 应透出
// 累计重启次数（回调回填），且监管仍在跑 → state 保持 running。
func TestAppsListReportsRestartCount(t *testing.T) {
	s, reg := newTestServer(t)
	if err := reg.UpsertTool(&model.Tool{
		ID: "quitter", Name: "Q", Runtime: model.RuntimeProcess, Entry: "sleep 0.2",
		InputSchema:  map[string]interface{}{"type": "object"},
		OutputSchema: map[string]interface{}{"type": "object"},
		Versions:     []model.ToolVersion{{Version: "1.0.0", State: model.StateStable}},
	}); err != nil {
		t.Fatalf("注册 Tool 失败: %v", err)
	}
	if err := reg.UpsertFlow(&model.Flow{ID: "crashf", Steps: []model.Step{
		{ID: "keep", Call: "quitter", Mode: "service", Input: map[string]interface{}{}},
	}}); err != nil {
		t.Fatalf("注册 Flow 失败: %v", err)
	}
	h := s.Handler()

	rec, out := do(t, h, "POST", "/api/apps", map[string]interface{}{"flow_id": "crashf"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("启动 app 期望 201，实际 %d %s", rec.Code, rec.Body.String())
	}

	// 事件驱动轮询：等 restarts ≥ 1（而非固定时长窗口，红线 13）。
	deadline := time.Now().Add(15 * time.Second)
	items := doArray(t, h, "GET", "/api/apps")
	for time.Now().Before(deadline) {
		items = doArray(t, h, "GET", "/api/apps")
		if len(items) > 0 && items[0]["restarts"] != nil && items[0]["restarts"].(float64) >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(items) != 1 {
		t.Fatalf("app 列表应为 1，实际 %d", len(items))
	}
	if n, ok := items[0]["restarts"].(float64); !ok || n < 1 {
		t.Fatalf("崩溃循环应透出 restarts ≥ 1，实际 %v", items[0]["restarts"])
	}
	if items[0]["state"] != "running" {
		t.Fatalf("崩溃循环中监管仍在跑，应报 running，实际 %v", items[0]["state"])
	}

	// 收尾：停止 app，避免泄漏 service 子进程。
	if id, _ := out["id"].(string); id != "" {
		if rec, _ := do(t, h, "DELETE", "/api/apps/"+id, nil); rec.Code != http.StatusOK {
			t.Fatalf("停止 app 期望 200，实际 %d", rec.Code)
		}
	}
}
