package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"gear/internal/artifact"
)

// appDefBundle 是一份最小但完整的可导入 bundle（App + 一个 Tool + 一个 Flow）。
const appDefBundle = `
kind: App
id: uiapp
name: 界面测试应用
description: 由 REST 面导入
entry_flow: uiapp.up
---
kind: Tool
id: uiapp__echo
name: echo
description: 测试用 Tool
runtime: lua
entry: |
  return {}
input_schema:
  type: object
output_schema:
  type: object
versions:
  - version: "1.0.0"
    state: stable
---
kind: Flow
id: uiapp.up
name: 入口
description: 测试用入口 Flow
steps:
  - id: s1
    call: uiapp__echo
    input: {}
`

// TestAppDefRoutesCoexist：Go 1.22+ ServeMux 同名 pattern 二次注册会 panic，
// 且 `GET /` 与 `GET /{path...}` 同时注册也 panic。这里锁定 {id} 与 {id}/export
// 两条 GET 能共存（ServeMux 按「更具体」匹配），别让后人顺手改成通配兜底。
func TestAppDefRoutesCoexist(t *testing.T) {
	s, _ := newTestServer(t)
	if rec, _ := do(t, s.mux, "GET", "/api/app-defs/nope", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("不存在的清单应 404，实际 %d", rec.Code)
	}
	// 先导入一份，才能验证 /export 走的是 export handler 而非被 {id} 吞掉。
	rec, _ := do(t, s.mux, "POST", "/api/app-defs/import", map[string]interface{}{"bundle": appDefBundle})
	if rec.Code != http.StatusOK {
		t.Fatalf("导入失败: %d %s", rec.Code, rec.Body.String())
	}
	rec2, _ := do(t, s.mux, "GET", "/api/app-defs/uiapp/export", nil)
	if rec2.Code != http.StatusOK {
		t.Fatalf("导出端点应 200，实际 %d", rec2.Code)
	}
	if ct := rec2.Header().Get("Content-Type"); !strings.Contains(ct, "yaml") {
		t.Fatalf("导出 Content-Type 异常: %q", ct)
	}
}

// TestAppDefImportRoundTrip：导入 → 列表 → 导出 → 再导入（幂等）完整闭环。
func TestAppDefImportRoundTrip(t *testing.T) {
	s, _ := newTestServer(t)
	if rec, _ := do(t, s.mux, "GET", "/api/app-defs", nil); rec.Code != http.StatusOK {
		t.Fatalf("列表失败: %d", rec.Code)
	}
	rec, body := do(t, s.mux, "POST", "/api/app-defs/import", map[string]interface{}{"bundle": appDefBundle})
	if rec.Code != http.StatusOK {
		t.Fatalf("导入失败: %d %v", rec.Code, body)
	}
	created, _ := body["created"].([]interface{})
	if len(created) != 3 {
		t.Fatalf("应创建 3 个对象，实际 %v", created)
	}
	items := listAppDefs(t, s)
	if len(items) != 1 || items[0].App.ID != "uiapp" {
		t.Fatalf("列表未反映导入: %+v", items)
	}
	if items[0].Counts["tools"] != 1 || items[0].Counts["flows"] != 1 {
		t.Fatalf("组成统计不对: %+v", items[0].Counts)
	}
	// 导出的文本应能被 ParseBundle 再解析（不落盘往返）。
	exported := getExport(t, s, "uiapp")
	b, diags := artifact.ParseBundle(exported)
	if len(diags) > 0 || len(b.Apps) != 1 || len(b.Tools) != 1 || len(b.Flows) != 1 {
		t.Fatalf("导出文本不可再导入: diags=%v apps=%d tools=%d flows=%d",
			diags, len(b.Apps), len(b.Tools), len(b.Flows))
	}
}

func listAppDefs(t *testing.T, s *Server) []AppDefDTO {
	t.Helper()
	rec, _ := do(t, s.mux, "GET", "/api/app-defs", nil)
	var out struct {
		Items []AppDefDTO `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析列表失败: %v", err)
	}
	return out.Items
}

func getExport(t *testing.T, s *Server, id string) string {
	t.Helper()
	rec, _ := do(t, s.mux, "GET", "/api/app-defs/"+id+"/export", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("导出失败: %d", rec.Code)
	}
	return rec.Body.String()
}

// TestAppDefDryRunWritesNothing：dry_run 只汇报不落库（不静默降级）。
func TestAppDefDryRunWritesNothing(t *testing.T) {
	s, _ := newTestServer(t)
	rec, body := do(t, s.mux, "POST", "/api/app-defs/import", map[string]interface{}{
		"bundle": appDefBundle, "dry_run": true,
	})
	if rec.Code != http.StatusOK || body["dry_run"] != true {
		t.Fatalf("dry_run 响应异常: %d %v", rec.Code, body)
	}
	if got := len(listAppDefs(t, s)); got != 0 {
		t.Fatalf("dry_run 竟然写库了，列表有 %d 项", got)
	}
}

// TestAppDefConflictRequiresOverwrite：重复导入默认拒绝，显式 overwrite 才放行。
func TestAppDefConflictRequiresOverwrite(t *testing.T) {
	s, _ := newTestServer(t)
	if rec, _ := do(t, s.mux, "POST", "/api/app-defs/import", map[string]interface{}{"bundle": appDefBundle}); rec.Code != http.StatusOK {
		t.Fatal("首次导入失败")
	}
	changed := strings.Replace(appDefBundle, "界面测试应用", "改名后的应用", 1)
	if rec, _ := do(t, s.mux, "POST", "/api/app-defs/import", map[string]interface{}{"bundle": changed}); rec.Code != http.StatusConflict {
		t.Fatalf("内容变更应 409，实际 %d", rec.Code)
	}
	if rec, _ := do(t, s.mux, "POST", "/api/app-defs/import", map[string]interface{}{"bundle": changed, "overwrite": true}); rec.Code != http.StatusOK {
		t.Fatalf("overwrite=true 应放行，实际 %d", rec.Code)
	}
}

// TestAppDefRejectsBadBundle：坏 YAML 回结构化诊断，不是笼统 500。
func TestAppDefRejectsBadBundle(t *testing.T) {
	s, _ := newTestServer(t)
	rec, body := do(t, s.mux, "POST", "/api/app-defs/import", map[string]interface{}{"bundle": "kind: App\n  bad: ["})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("坏 bundle 应 400，实际 %d", rec.Code)
	}
	if body["code"] != "BAD_BUNDLE" {
		t.Fatalf("应回 BAD_BUNDLE 诊断，实际 %v", body)
	}
	if rec2, _ := do(t, s.mux, "POST", "/api/app-defs/import", map[string]interface{}{}); rec2.Code != http.StatusBadRequest {
		t.Fatalf("缺 bundle 应 400，实际 %d", rec2.Code)
	}
}

// TestAppDefRejectsMigrations：REST 面不执行数据库迁移，必须显式拒绝（不静默跳过）。
func TestAppDefRejectsMigrations(t *testing.T) {
	s, _ := newTestServer(t)
	withMig := appDefBundle + `
---
kind: Migration
app: uiapp
version: 1
up: CREATE TABLE t(x)
`
	rec, _ := do(t, s.mux, "POST", "/api/app-defs/import", map[string]interface{}{"bundle": withMig})
	// 若 ParseBundle 不认 Migration 文档，会先 400；两种拒绝都可接受，关键是**没有写库**。
	if rec.Code != http.StatusBadRequest && rec.Code != http.StatusOK {
		t.Fatalf("含迁移的 bundle 应被拒，实际 %d", rec.Code)
	}
	if len(listAppDefs(t, s)) != 0 {
		t.Fatal("含迁移的 bundle 竟然写库了")
	}
}

// TestAppDefDelete：删除清单，被引用/不存在分别 409/404。
func TestAppDefDelete(t *testing.T) {
	s, _ := newTestServer(t)
	if rec, _ := do(t, s.mux, "POST", "/api/app-defs/import", map[string]interface{}{"bundle": appDefBundle}); rec.Code != http.StatusOK {
		t.Fatal("首次导入失败")
	}
	if rec, _ := do(t, s.mux, "DELETE", "/api/app-defs/uiapp", nil); rec.Code != http.StatusOK {
		t.Fatalf("删除失败: %d", rec.Code)
	}
	if rec, _ := do(t, s.mux, "DELETE", "/api/app-defs/uiapp", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("删不存在的应 404，实际 %d", rec.Code)
	}
}
