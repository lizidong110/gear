package server

// applogs_test.go：锁住 detached App 实时日志的三条端点契约（REQ-API-APP-LOGS）。
//
// 特别盯住两条容易「看起来能用其实没用」的失效：
//   ① ingest 没有 token 校验 → 任何人都能往别人面板里塞内容；
//   ② SSE 端点退回去读 <appID>.log → 就变成用户明确拒绝的「从日志文件拉取」。
// 本测试只从**内存 broker**验证实时行，全程不创建任何 .log 文件。

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gear/internal/app"
)

func newAppLogServer(t *testing.T) (*Server, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "apps")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	regPath := filepath.Join(t.TempDir(), "registry.db")
	s, _ := newTestServer(t, WithRegistryPath(regPath), WithAppsDir(dir))
	return s, dir
}

func writeAppLogRecord(t *testing.T, dir, id string, live bool) {
	t.Helper()
	if err := app.SaveRecord(dir, &app.Record{
		AppID: id, FlowID: "demo.up", PID: os.Getpid(), Restart: "always",
		StartedAt: time.Now(), LogPath: filepath.Join(dir, id+".log"), LiveLogs: live,
	}); err != nil {
		t.Fatalf("写台账失败: %v", err)
	}
}

func publishEndpoint(t *testing.T, dir string) string {
	t.Helper()
	token, err := app.PublishServerEndpoint(dir, "http://127.0.0.1:8787")
	if err != nil {
		t.Fatalf("写发现文件失败: %v", err)
	}
	return token
}

func ingestBody(token string, lines ...string) map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(lines))
	for _, l := range lines {
		out = append(out, map[string]interface{}{"step_id": "s1", "stream": "stdout", "line": l})
	}
	return map[string]interface{}{"token": token, "lines": out}
}

func TestAppLogRoutesCoexist(t *testing.T) {
	s, dir := newAppLogServer(t)
	// Go 1.22+ ServeMux：同名 pattern 二次注册会 panic，能构造成功本身就是一条断言。
	if rec, _ := do(t, s.Handler(), "GET", "/api/apps/records", nil); rec.Code != http.StatusOK {
		t.Fatalf("台账列表端点异常: %d", rec.Code)
	}
	if rec, _ := do(t, s.Handler(), "GET", "/api/apps/nope/logs/stream", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("不存在的台账 SSE 应 404，实际 %d", rec.Code)
	}
	if rec, _ := do(t, s.Handler(), "POST", "/api/apps/nope/logs", ingestBody("x")); rec.Code == http.StatusNotFound {
		t.Fatal("ingest 应比 /api/{path...} 兜底更具体，不能被吞成 404")
	}
	_ = dir
}

func TestListAppRecordsReportsLiveLogs(t *testing.T) {
	s, dir := newAppLogServer(t)
	writeAppLogRecord(t, dir, "app-live", true)
	writeAppLogRecord(t, dir, "app-offline", false)

	rec, out := do(t, s.Handler(), "GET", "/api/apps/records", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("列表失败: %d %s", rec.Code, rec.Body.String())
	}
	items, ok := out["items"].([]interface{})
	if !ok || len(items) != 2 {
		t.Fatalf("应列 2 条台账，实际 %v", out)
	}
	byID := map[string]map[string]interface{}{}
	for _, raw := range items {
		m := raw.(map[string]interface{})
		byID[m["app_id"].(string)] = m
	}
	if byID["app-live"]["live_logs"] != true || byID["app-offline"]["live_logs"] != false {
		t.Fatalf("live_logs 必须如实反映派生时是否注入回推 env: %v", byID)
	}
	if byID["app-live"]["flow_id"] != "demo.up" || byID["app-live"]["state"] == "" {
		t.Fatalf("台账字段缺失: %v", byID["app-live"])
	}
	// 端点只报路径、绝不回日志内容（「不从日志文件拉取」的硬约束）。
	if strings.Contains(rec.Body.String(), "\"line\"") {
		t.Fatal("台账列表不该包含任何日志行")
	}
}

func TestIngestAppLogsRequiresTokenAndKnownApp(t *testing.T) {
	s, dir := newAppLogServer(t)
	// 没写 server.addr = 实时日志未启用，回推必须被拒而不是「接受后丢掉」。
	if rec, _ := do(t, s.Handler(), "POST", "/api/apps/app1/logs", ingestBody("x", "a")); rec.Code != http.StatusForbidden {
		t.Fatalf("未启用时应 403，实际 %d", rec.Code)
	}
	token := publishEndpoint(t, dir)
	writeAppLogRecord(t, dir, "app1", true)

	if rec, _ := do(t, s.Handler(), "POST", "/api/apps/app1/logs", ingestBody("", "a")); rec.Code != http.StatusUnauthorized {
		t.Fatalf("缺 token 应 401，实际 %d", rec.Code)
	}
	if rec, _ := do(t, s.Handler(), "POST", "/api/apps/app1/logs", ingestBody("wrong", "a")); rec.Code != http.StatusUnauthorized {
		t.Fatalf("错 token 应 401，实际 %d", rec.Code)
	}
	if rec, _ := do(t, s.Handler(), "POST", "/api/apps/ghost/logs", ingestBody(token, "a")); rec.Code != http.StatusNotFound {
		t.Fatalf("未知 app 应 404，实际 %d", rec.Code)
	}
	rec, out := do(t, s.Handler(), "POST", "/api/apps/app1/logs", ingestBody(token, "a", "b"))
	if rec.Code != http.StatusOK || out["accepted"] != float64(2) {
		t.Fatalf("正常回推失败: %d %v", rec.Code, out)
	}
}

func TestIngestAppLogsRejectsCrossOrigin(t *testing.T) {
	s, dir := newAppLogServer(t)
	publishEndpoint(t, dir)
	writeAppLogRecord(t, dir, "app1", true)
	body, _ := json.Marshal(ingestBody("x", "a"))
	req := httptest.NewRequest("POST", "/api/apps/app1/logs", strings.NewReader(string(body)))
	req.Header.Set("Origin", "https://evil.example.com")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("跨站伪造回推应 403，实际 %d", rec.Code)
	}
}

// TestStreamAppLogsReceivesLines：先订阅、再由「另一个进程」POST 一行，SSE 应只推这一行。
// 全程没有创建 <appID>.log —— 若有人把端点改成读文件，这条测试立刻拿不到数据。
func TestStreamAppLogsReceivesLines(t *testing.T) {
	s, dir := newAppLogServer(t)
	token := publishEndpoint(t, dir)
	writeAppLogRecord(t, dir, "app1", true)

	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	type getRes struct {
		resp *http.Response
		err  error
	}
	ch := make(chan getRes, 1)
	go func() {
		r, e := http.Get(ts.URL + "/api/apps/app1/logs/stream")
		ch <- getRes{r, e}
	}()
	// 等订阅真正挂上 broker 再回推：否则 Publish 无订阅者会丢弃，测试变成假绿。
	deadline := time.Now().Add(3 * time.Second)
	for {
		s.logBroker.mu.Lock()
		n := len(s.logBroker.subs[appLogKey("app1")])
		s.logBroker.mu.Unlock()
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("SSE 订阅未建立")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if rec, _ := do(t, s.Handler(), "POST", "/api/apps/app1/logs", ingestBody(token, "live-A")); rec.Code != http.StatusOK {
		t.Fatal("回推行失败")
	}

	select {
	case gr := <-ch:
		if gr.err != nil {
			t.Fatalf("建流失败: %v", gr.err)
		}
		defer gr.resp.Body.Close()
		if gr.resp.StatusCode != http.StatusOK || gr.resp.Header.Get("Content-Type") != "text/event-stream" {
			t.Fatalf("SSE 响应异常: %d %q", gr.resp.StatusCode, gr.resp.Header.Get("Content-Type"))
		}
		sc := bufio.NewScanner(gr.resp.Body)
		found := false
		done := make(chan bool, 1)
		go func() {
			for sc.Scan() {
				if strings.HasPrefix(sc.Text(), "data: ") && strings.Contains(sc.Text(), "live-A") {
					done <- true
					return
				}
			}
			done <- false
		}()
		select {
		case found = <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("5s 内没收到实时日志行")
		}
		if !found {
			t.Fatal("SSE 没推回「订阅后新产生」的日志行")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SSE 连接迟迟未返回响应头")
	}
}

func TestLoopbackBaseURL(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"127.0.0.1:8787", "http://127.0.0.1:8787"},
		{":8787", "http://127.0.0.1:8787"},
		{"0.0.0.0:8787", "http://127.0.0.1:8787"},
		{"[::]:8787", "http://127.0.0.1:8787"},
		{"[::1]:8787", "http://[::1]:8787"},
	} {
		got, err := loopbackBaseURL(tc.in)
		if err != nil || got != tc.want {
			t.Fatalf("loopbackBaseURL(%q) = %q, %v；期望 %q", tc.in, got, err, tc.want)
		}
	}
}
