package server

// openurl_test.go：锁住 REST 面把「打开地址」回填进 GET /api/apps 的响应。
//
// 引擎侧收集逻辑由 internal/engine/openurl_test.go 覆盖；这里只验证「server 从引擎取数并写入
// AppInstance 响应」这一层（含 engine 为 nil 时的防御分支不 panic）。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestListAppsIncludesOpenURLs(t *testing.T) {
	s, _ := newPruneServer(t)
	// 直接种入某 flow 的打开地址：绕过完整 Flow 执行，专注 REST 回填逻辑
	// （引擎收集本身由 engine/openurl_test.go 覆盖）。
	s.engine.AddOpenURL("e2e.up", "http://127.0.0.1:8080/")

	s.appMu.Lock()
	s.apps["app_e2e.up-x"] = &AppInstance{ID: "app_e2e.up-x", FlowID: "e2e.up", StartedAt: time.Now()}
	s.appSeq = append(s.appSeq, "app_e2e.up-x")
	s.appMu.Unlock()

	rec := httptest.NewRecorder()
	s.handleListApps(rec, httptest.NewRequest(http.MethodGet, "/api/apps", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	var out []map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是合法 JSON 数组: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("期望 1 条，实际 %d: %s", len(out), rec.Body.String())
	}
	urls, ok := out[0]["open_urls"].([]interface{})
	if !ok {
		t.Fatalf("open_urls 缺失或非数组: %s", rec.Body.String())
	}
	if len(urls) != 1 || urls[0] != "http://127.0.0.1:8080/" {
		t.Fatalf("open_urls 不符: %v", urls)
	}
}

// 引擎为 nil（某些测试构造的 Server 不带 engine）时，handler 不应 panic，open_urls 留空。
func TestListAppsNilEngineNoPanic(t *testing.T) {
	s, _ := newPruneServer(t)
	s.appMu.Lock()
	// 模拟无引擎场景
	eng := s.engine
	s.engine = nil
	s.apps["app_x"] = &AppInstance{ID: "app_x", FlowID: "x.up", StartedAt: time.Now()}
	s.appSeq = append(s.appSeq, "app_x")
	s.appMu.Unlock()

	rec := httptest.NewRecorder()
	s.handleListApps(rec, httptest.NewRequest(http.MethodGet, "/api/apps", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d", rec.Code)
	}
	var out []map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是合法 JSON 数组: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("期望 1 条，实际 %d", len(out))
	}
	// 引擎为 nil ⇒ open_urls 不应被设置（保持零值 null），且未 panic。
	if _, present := out[0]["open_urls"]; !present {
		t.Fatalf("open_urls 字段应存在（零值），实际缺字段")
	}
	// 还原，避免影响同包其他测试
	s.appMu.Lock()
	s.engine = eng
	s.appMu.Unlock()
}
