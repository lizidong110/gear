// proxy_test.go：反向代理（proxy 配置）的行为与 fail-closed 纪律。
//
// 这些用例直接对着「UI /api 全 404」这个真实缺口写：起服时要能把 /api 转发到内核，
// 同时静态资源照常；规则写错必须报错而不是静默丢弃；后端挂了必须 502 而不是假装正常。
package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// echoBackend 返回一个把「收到的请求路径」原样回写的后端（用于断言转发路径是否正确）。
func echoBackend(t *testing.T, tag string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = fmt.Fprintf(w, "%s:%s", tag, r.URL.Path)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// startSite 在临时目录里放一个 index.html，起服（带给定 args 的 proxy），返回访问端口。
func startSite(t *testing.T, svc *Service, extra map[string]interface{}) int {
	t.Helper()
	port, _ := startSiteArgs(t, svc, extra)
	return port
}

// startSiteArgs 同 startSite，但把完整 args 一并返回（供幂等重调用构造「全同配置」）。
func startSiteArgs(t *testing.T, svc *Service, extra map[string]interface{}) (int, map[string]interface{}) {
	t.Helper()
	port := freePort(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<h1>static index</h1>"), 0o644); err != nil {
		t.Fatalf("写测试页失败: %v", err)
	}
	args := map[string]interface{}{"port": port, "dir": dir, "host": "127.0.0.1"}
	for k, v := range extra {
		args[k] = v
	}
	start := callHandler(t, svc.handleStart, args)
	if start["error"] != nil {
		t.Fatalf("/start 意外报错: %v", start)
	}
	return port, args
}

// getBody 取一个 URL 的响应体（带超时，避免后端不可达时测试挂死）。
func getBody(t *testing.T, url string) string {
	t.Helper()
	cli := &http.Client{Timeout: 5 * time.Second}
	resp, err := cli.Get(url)
	if err != nil {
		t.Fatalf("GET %s 失败: %v", url, err)
	}
	defer resp.Body.Close()
	b := make([]byte, 4096)
	n, _ := resp.Body.Read(b)
	return string(b[:n])
}

// getStatus 取一个 URL 的状态码。
func getStatus(t *testing.T, url string) int {
	t.Helper()
	cli := &http.Client{Timeout: 5 * time.Second}
	resp, err := cli.Get(url)
	if err != nil {
		t.Fatalf("GET %s 失败: %v", url, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// TestParseProxyRulesRejectsInvalid：规则不合法必须报错（fail-closed），绝不静默丢弃。
// 静默丢弃正是「proxy 写了但 /api 还是 404」的成因。
func TestParseProxyRulesRejectsInvalid(t *testing.T) {
	cases := []struct {
		name string
		in   interface{}
	}{
		{"不是数组", map[string]interface{}{"path": "/api"}},
		{"元素不是对象", []interface{}{"http://127.0.0.1:1"}},
		{"缺 path", []interface{}{map[string]interface{}{"target": "http://127.0.0.1:1"}}},
		{"path 不以斜杠开头", []interface{}{map[string]interface{}{"path": "api", "target": "http://127.0.0.1:1"}}},
		{"缺 target", []interface{}{map[string]interface{}{"path": "/api"}}},
		{"target 无 scheme", []interface{}{map[string]interface{}{"path": "/api", "target": "127.0.0.1:8787"}}},
		{"target 协议不支持", []interface{}{map[string]interface{}{"path": "/api", "target": "ftp://127.0.0.1:8787"}}},
	}
	for _, c := range cases {
		if _, err := parseProxyRules(c.in); err == nil {
			t.Fatalf("用例 %q 应报错，实际未报错", c.name)
		}
	}
}

// TestParseProxyRulesLongestPrefixFirst：多条规则按最长前缀优先排序；前缀去掉尾随斜杠。
func TestParseProxyRulesLongestPrefixFirst(t *testing.T) {
	rules, err := parseProxyRules([]interface{}{
		map[string]interface{}{"path": "/api/", "target": "http://127.0.0.1:8787"},
		map[string]interface{}{"path": "/api/v2", "target": "http://127.0.0.1:9000"},
	})
	if err != nil {
		t.Fatalf("解析合法规则不应报错: %v", err)
	}
	if len(rules) != 2 {
		t.Fatalf("应有 2 条规则，实际 %d", len(rules))
	}
	if rules[0].Path != "/api/v2" || rules[1].Path != "/api" {
		t.Fatalf("应最长前缀优先且去掉尾随斜杠，实际 %q, %q", rules[0].Path, rules[1].Path)
	}
	if rules[1].StripPrefix {
		t.Fatalf("strip_prefix 缺省应为 false")
	}
}

// TestStartWithProxyForwardsAndFallsBackToStatic：命中 /api 走反代、其余走静态；
// /apix 不能被 /api 规则吞掉（前缀边界）；/start 结果回显生效的规则（可观测）。
func TestStartWithProxyForwardsAndFallsBackToStatic(t *testing.T) {
	svc := newServiceForTest(t)
	defer svc.Stop()
	backend := echoBackend(t, "backend")

	port, startArgs := startSiteArgs(t, svc, map[string]interface{}{
		"proxy": []interface{}{
			map[string]interface{}{"path": "/api", "target": backend.URL},
		},
	})
	base := "http://127.0.0.1:" + strconv.Itoa(port)

	if got := getBody(t, base+"/api/healthz"); got != "backend:/api/healthz" {
		t.Fatalf("/api/healthz 应被转发到后端（原路径），实际 %q", got)
	}
	if got := getBody(t, base+"/api"); got != "backend:/api" {
		t.Fatalf("/api 应命中规则（整段前缀），实际 %q", got)
	}
	if got := getBody(t, base+"/index.html"); got != "<h1>static index</h1>" {
		t.Fatalf("未命中规则应回落静态文件，实际 %q", got)
	}
	// 前缀边界：/apix 不属于 /api，必须仍走静态（此处文件不存在 → 404），不能被反代吞掉。
	if got := getBody(t, base+"/apix"); got == "backend:/apix" {
		t.Fatalf("/apix 不应命中 /api 规则，实际被转发: %q", got)
	}

	// 多站点语义（2026-09-24）：同端口**换配置**（不同 dir）→ fail-closed 报错，绝不静默吞参数；
	conflict := callHandler(t, svc.handleStart, map[string]interface{}{"port": port, "dir": t.TempDir()})
	if conflict["error"] == nil || !strings.Contains(conflict["error"].(string), "已被另一站点占用") {
		t.Fatalf("同端口不同 dir 应 fail-closed 报「已被另一站点占用」，实际 %v", conflict)
	}
	// 同端口**全同配置** → 幂等分支同样回显当前规则。
	res := callHandler(t, svc.handleStart, startArgs)
	r, ok := res["result"].(map[string]interface{})
	if !ok {
		t.Fatalf("/start 幂等分支应返回 result，实际 %v", res)
	}
	pr, ok := r["proxy"].([]interface{})
	if !ok || len(pr) != 1 {
		t.Fatalf("/start 结果应回显 1 条 proxy 规则，实际 %v", r["proxy"])
	}
	first, _ := pr[0].(map[string]interface{})
	if first["path"] != "/api" {
		t.Fatalf("回显规则的 path 应为 /api，实际 %v", first)
	}
}

// TestStartWithProxyStripPrefix：strip_prefix:true → 转发前去掉匹配前缀（nginx 的 proxy_pass URI 写法）。
func TestStartWithProxyStripPrefix(t *testing.T) {
	svc := newServiceForTest(t)
	defer svc.Stop()
	backend := echoBackend(t, "backend")

	port := startSite(t, svc, map[string]interface{}{
		"proxy": []interface{}{
			map[string]interface{}{"path": "/api", "target": backend.URL, "strip_prefix": true},
		},
	})
	base := "http://127.0.0.1:" + strconv.Itoa(port)

	if got := getBody(t, base+"/api/healthz"); got != "backend:/healthz" {
		t.Fatalf("strip_prefix 应去掉 /api，实际 %q", got)
	}
	// 只剩前缀本身时，去掉后应是根路径而非空串。
	if got := getBody(t, base+"/api"); got != "backend:/" {
		t.Fatalf("去掉前缀后应为 /，实际 %q", got)
	}
}

// TestStartProxyLongestPrefixWins：/api/v2 比 /api 更具体，应优先命中。
func TestStartProxyLongestPrefixWins(t *testing.T) {
	svc := newServiceForTest(t)
	defer svc.Stop()
	oldBackend := echoBackend(t, "v1")
	newBackend := echoBackend(t, "v2")

	port := startSite(t, svc, map[string]interface{}{
		"proxy": []interface{}{
			// 故意把较短前缀写在前面（乱序），验证解析期排序生效。
			map[string]interface{}{"path": "/api", "target": oldBackend.URL},
			map[string]interface{}{"path": "/api/v2", "target": newBackend.URL},
		},
	})
	base := "http://127.0.0.1:" + strconv.Itoa(port)

	if got := getBody(t, base+"/api/v2/things"); got != "v2:/api/v2/things" {
		t.Fatalf("/api/v2/* 应命中更具体规则，实际 %q", got)
	}
	if got := getBody(t, base+"/api/other"); got != "v1:/api/other" {
		t.Fatalf("/api/other 应命中较短规则，实际 %q", got)
	}
}

// TestStartProxyBackendDownReturns502：后端不可达必须返回可见的 502（不静默 200、不挂起）。
func TestStartProxyBackendDownReturns502(t *testing.T) {
	svc := newServiceForTest(t)
	defer svc.Stop()
	// 127.0.0.1:1 基本必然拒连。
	dead := "http://127.0.0.1:1"

	port := startSite(t, svc, map[string]interface{}{
		"proxy": []interface{}{
			map[string]interface{}{"path": "/api", "target": dead},
		},
	})
	base := "http://127.0.0.1:" + strconv.Itoa(port)

	if code := getStatus(t, base+"/api/healthz"); code != http.StatusBadGateway {
		t.Fatalf("后端不可达应返回 502，实际 %d", code)
	}
	// 静态侧不受影响。
	if got := getBody(t, base+"/index.html"); got != "<h1>static index</h1>" {
		t.Fatalf("后端挂掉不影响静态托管，实际 %q", got)
	}
}
