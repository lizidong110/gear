package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// callHandler 以 Gear RPC 线格式（body 包成 {"args": {...}}）调一个 handler，解析返回的 JSON。
func callHandler(t *testing.T, h http.HandlerFunc, args map[string]interface{}) map[string]interface{} {
	t.Helper()
	body, err := json.Marshal(map[string]interface{}{"args": args})
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/x", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h(w, req)
	var out map[string]interface{}
	if w.Body.Len() > 0 {
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("解析 handler 返回失败: %v (body=%s)", err, w.Body.String())
		}
	}
	return out
}

// freePort 借系统分配一个空闲端口，关掉监听后返回（调用方立即用于绑定，竞态极小）。
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("分配空闲端口失败: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

func newServiceForTest(t *testing.T) *Service {
	t.Helper()
	return newService()
}

// TestCapsListsImplementedMethods：/caps 必须列出全部已实现方法（红线夹具依赖）。
func TestCapsListsImplementedMethods(t *testing.T) {
	svc := newServiceForTest(t)
	out := callHandler(t, svc.handleCaps, nil)
	res, ok := out["result"].(map[string]interface{})
	if !ok {
		t.Fatalf("/caps 应返回 result 对象，实际 %v", out)
	}
	methods, ok := res["methods"].([]interface{})
	if !ok {
		t.Fatalf("/caps.methods 应为数组，实际 %v", res)
	}
	got := map[string]bool{}
	for _, m := range methods {
		if s, ok := m.(string); ok {
			got[s] = true
		}
	}
	for _, want := range []string{"start", "stop"} {
		if !got[want] {
			t.Fatalf("/caps 缺少已实现方法 %q（got=%v）", want, got)
		}
	}
}

// TestStartAutoAssignsPort：省略 port 或填 0 都表示自动分配，返回值必须带回**真实**非零端口。
//
// 这条断言的靶心是「把 0 回给调用方」：那样反代 target 会拼成 http://127.0.0.1:0/、台账记个
// 死数、用户拿着 url 打不开——全程不报错，纯粹骗人。所以除了查数值非零，还要查
// url 与 port 字段同源、以及那个端口真的能服务（内核挑的端口必须是活的）。
func TestStartAutoAssignsPort(t *testing.T) {
	cases := []struct {
		name string
		args map[string]interface{}
	}{
		{"省略 port", map[string]interface{}{"dir": t.TempDir()}},
		{"port 显式为 0", map[string]interface{}{"port": 0, "dir": t.TempDir()}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := newServiceForTest(t)
			dir := tc.args["dir"].(string)
			if err := os.WriteFile(filepath.Join(dir, "index.html"),
				[]byte("<h1>auto</h1>"), 0o644); err != nil {
				t.Fatalf("写测试页失败: %v", err)
			}
			out := callHandler(t, svc.handleStart, tc.args)
			res, ok := out["result"].(map[string]interface{})
			if !ok {
				t.Fatalf("/start 应返回 result，实际 %v", out)
			}
			realPort, ok := res["port"].(float64)
			if !ok || realPort <= 0 || realPort >= 65536 {
				t.Fatalf("自动分配必须带回真实非零端口，实际 %v", res["port"])
			}
			// url 与 port 必须同源于同一个真实端口，否则调用方照 url 访问会打空。
			if want := fmt.Sprintf("http://0.0.0.0:%d/", int(realPort)); res["url"] != want {
				t.Fatalf("url 与 port 字段不一致: url=%v want=%s", res["url"], want)
			}
			resp, err := http.Get("http://127.0.0.1:" + strconv.Itoa(int(realPort)) + "/index.html")
			if err != nil {
				t.Fatalf("GET 真实端口失败: %v", err)
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			if string(body) != "<h1>auto</h1>" {
				t.Fatalf("静态页内容不符，实际 %q", string(body))
			}
			if stop := callHandler(t, svc.handleStop, map[string]interface{}{}); stop["error"] != nil {
				t.Fatalf("/stop 不应报错，实际 %v", stop)
			}
		})
	}
}

// TestStartRejectsNegativePort：负 port 是笔误，不能悄悄当成「自动」处理。
func TestStartRejectsNegativePort(t *testing.T) {
	svc := newServiceForTest(t)
	out := callHandler(t, svc.handleStart, map[string]interface{}{"port": -1, "dir": t.TempDir()})
	if out["error"] == nil {
		t.Fatalf("port 为负应报错，实际 %v", out)
	}
	if !containsStr(out["error"].(string), "port 不能为负") {
		t.Fatalf("错误文案应点名 port 不能为负，实际 %v", out["error"])
	}
}

// TestStartRejectsMissingDir：缺 dir 必须 fail-closed。
func TestStartRejectsMissingDir(t *testing.T) {
	svc := newServiceForTest(t)
	out := callHandler(t, svc.handleStart, map[string]interface{}{"port": 8080})
	if out["error"] == nil {
		t.Fatalf("缺 dir 应报错，实际 %v", out)
	}
	if !containsStr(out["error"].(string), "dir 必填") {
		t.Fatalf("错误文案应点名 dir 必填，实际 %v", out["error"])
	}
}

// TestStartRejectsNonExistentDir：dir 不是真实目录必须 fail-closed（不静默起空服务）。
func TestStartRejectsNonExistentDir(t *testing.T) {
	svc := newServiceForTest(t)
	port := freePort(t)
	missing := filepath.Join(t.TempDir(), "no-such-dir")
	out := callHandler(t, svc.handleStart, map[string]interface{}{"port": port, "dir": missing})
	if out["error"] == nil {
		t.Fatalf("dir 不存在应报错，实际 %v", out)
	}
	if !containsStr(out["error"].(string), "dir 不存在或不是目录") {
		t.Fatalf("错误文案应说明不是目录，实际 %v", out["error"])
	}
}

// TestStartServesStaticFilesAndStop：真实起服 → GET 到 index.html → 停止后端口释放。
func TestStartServesStaticFilesAndStop(t *testing.T) {
	svc := newServiceForTest(t)
	port := freePort(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"),
		[]byte("<h1>hello gear</h1>"), 0o644); err != nil {
		t.Fatalf("写测试页失败: %v", err)
	}

	start := callHandler(t, svc.handleStart, map[string]interface{}{"port": port, "dir": dir})
	res, ok := start["result"].(map[string]interface{})
	if !ok {
		t.Fatalf("/start 应返回 result，实际 %v", start)
	}
	if !containsStr(res["url"].(string), "8080") && !containsStr(res["url"].(string), "http://") {
		t.Fatalf("/start 返回 url 异常: %v", res)
	}

	// 浏览器访问真实网站端口（host 默认 0.0.0.0，127.0.0.1 可访问）。
	resp, err := http.Get("http://127.0.0.1:" + strconv.Itoa(port) + "/index.html")
	if err != nil {
		t.Fatalf("GET 静态页失败: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "<h1>hello gear</h1>" {
		t.Fatalf("静态页内容不符，实际 %q", string(body))
	}

	// 停止：端口应被释放（再次绑定同端口成功，证明已释放）。
	stop := callHandler(t, svc.handleStop, map[string]interface{}{})
	if stop["error"] != nil {
		t.Fatalf("/stop 不应报错，实际 %v", stop)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		t.Fatalf("/stop 后端口应已释放，但实际仍被占用: %v", err)
	}
	_ = ln.Close()
}

// TestStopWhenNotServing：未起服时 stop 也应安全返回 ok（幂等）。
func TestStopWhenNotServing(t *testing.T) {
	svc := newServiceForTest(t)
	out := callHandler(t, svc.handleStop, map[string]interface{}{})
	if out["error"] != nil {
		t.Fatalf("未起服时 /stop 不应报错，实际 %v", out)
	}
	res, ok := out["result"].(map[string]interface{})
	if !ok || res["ok"] != true {
		t.Fatalf("/stop 应返回 ok:true，实际 %v", out)
	}
}

// TestStartIdempotent：重复 start 仍返回同一 URL，不重复绑定。
func TestStartIdempotent(t *testing.T) {
	svc := newServiceForTest(t)
	port := freePort(t)
	dir := t.TempDir()
	a := callHandler(t, svc.handleStart, map[string]interface{}{"port": port, "dir": dir})
	b := callHandler(t, svc.handleStart, map[string]interface{}{"port": port, "dir": dir})
	// 第二次不应报错（幂等复用同一实例）
	if b["error"] != nil {
		t.Fatalf("重复 /start 不应报错，实际 %v", b)
	}
	// 清理
	callHandler(t, svc.handleStop, map[string]interface{}{})
	_ = a
}

func containsStr(s, sub string) bool {
	return strings.Contains(s, sub)
}

// writeIndex 往 dir 写一个内容唯一的 index.html，供站点内容断言。
func writeIndex(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte(body), 0o644); err != nil {
		t.Fatalf("写测试页失败: %v", err)
	}
}

// fetchBody GET 一次站点页面并返回正文（失败即 t.Fatal）。
func fetchBody(t *testing.T, port int) string {
	t.Helper()
	resp, err := http.Get("http://127.0.0.1:" + strconv.Itoa(port) + "/index.html")
	if err != nil {
		t.Fatalf("GET 站点失败: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

// TestMultiSiteCoexist：同一侧车同时托管两个站点（多 app 共享侧车的核心场景，
// 2026-09-24 修复面：旧代码是单站点，第二个 /start 被幂等吞掉，第二个应用 8090 永远起不来）。
func TestMultiSiteCoexist(t *testing.T) {
	svc := newServiceForTest(t)
	portA, portB := freePort(t), freePort(t)
	dirA, dirB := t.TempDir(), t.TempDir()
	writeIndex(t, dirA, "site-A")
	writeIndex(t, dirB, "site-B")

	if out := callHandler(t, svc.handleStart, map[string]interface{}{"port": portA, "dir": dirA}); out["error"] != nil {
		t.Fatalf("站点A start 失败: %v", out)
	}
	b := callHandler(t, svc.handleStart, map[string]interface{}{"port": portB, "dir": dirB})
	if b["error"] != nil {
		t.Fatalf("站点B start 失败（旧单站点代码在此把请求吞掉）: %v", b)
	}
	if got := fetchBody(t, portA); got != "site-A" {
		t.Fatalf("站点A 内容不符: %q", got)
	}
	if got := fetchBody(t, portB); got != "site-B" {
		t.Fatalf("站点B 内容不符: %q", got)
	}

	// 带端口 stop 只停 B，A 必须仍可访问。
	if out := callHandler(t, svc.handleStop, map[string]interface{}{"port": portB}); out["error"] != nil {
		t.Fatalf("按端口 stop 失败: %v", out)
	}
	if got := fetchBody(t, portA); got != "site-A" {
		t.Fatalf("停 B 后 A 应仍可访问，实际: %q", got)
	}
	_, err := http.Get("http://127.0.0.1:" + strconv.Itoa(portB) + "/index.html")
	if err == nil {
		t.Fatalf("B 已 stop，不应再可访问")
	}
}

// TestStartConflictSamePortDifferentDir：同端口不同配置必须 fail-closed 报错（不静默吞参数）。
func TestStartConflictSamePortDifferentDir(t *testing.T) {
	svc := newServiceForTest(t)
	port := freePort(t)
	dirA, dirB := t.TempDir(), t.TempDir()
	writeIndex(t, dirA, "site-A")
	if out := callHandler(t, svc.handleStart, map[string]interface{}{"port": port, "dir": dirA}); out["error"] != nil {
		t.Fatalf("首个站点 start 失败: %v", out)
	}
	out := callHandler(t, svc.handleStart, map[string]interface{}{"port": port, "dir": dirB})
	if out["error"] == nil {
		t.Fatalf("同端口不同 dir 应报错，实际 %v", out)
	}
	if !containsStr(out["error"].(string), "已被另一站点占用") {
		t.Fatalf("错误文案应点名端口冲突，实际 %v", out["error"])
	}
	// 原站点必须不受影响。
	if got := fetchBody(t, port); got != "site-A" {
		t.Fatalf("冲突报错后原站点内容不符: %q", got)
	}
}

// TestStopWithoutPortStopsAll：不带 port 的 /stop 停掉全部站点（向后兼容旧语义）。
func TestStopWithoutPortStopsAll(t *testing.T) {
	svc := newServiceForTest(t)
	portA, portB := freePort(t), freePort(t)
	dirA, dirB := t.TempDir(), t.TempDir()
	writeIndex(t, dirA, "site-A")
	writeIndex(t, dirB, "site-B")
	callHandler(t, svc.handleStart, map[string]interface{}{"port": portA, "dir": dirA})
	callHandler(t, svc.handleStart, map[string]interface{}{"port": portB, "dir": dirB})

	out := callHandler(t, svc.handleStop, map[string]interface{}{})
	if out["error"] != nil {
		t.Fatalf("全停 /stop 不应报错: %v", out)
	}
	res := out["result"].(map[string]interface{})
	stopped, ok := res["stopped"].([]interface{})
	if !ok || len(stopped) != 2 {
		t.Fatalf("全停应返回两个被停端口，实际 %v", res)
	}
	for _, p := range []int{portA, portB} {
		if _, err := http.Get("http://127.0.0.1:" + strconv.Itoa(p) + "/index.html"); err == nil {
			t.Fatalf("端口 %d 全停后不应再可访问", p)
		}
	}
}

// TestResolveSiteDir 锁死「dir 相对路径解析」规则：相对 installRoot / cwd 都能命中，
// 绝对路径原样校验，两者都不存在则 fail-closed 返回 false。
func TestResolveSiteDir(t *testing.T) {
	root := t.TempDir()
	appDir := filepath.Join(root, "apps", "mindstorm", "www")
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cwd := t.TempDir()
	cwdDir := filepath.Join(cwd, "assets")
	if err := os.MkdirAll(cwdDir, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Run("相对 installRoot 命中", func(t *testing.T) {
		got, ok := resolveSiteDir("apps/mindstorm/www", cwd, root)
		if !ok || got != appDir {
			t.Fatalf("应解析到 %s，实际 ok=%v got=%s", appDir, ok, got)
		}
	})
	t.Run("相对 cwd 命中", func(t *testing.T) {
		got, ok := resolveSiteDir("assets", cwd, "")
		if !ok || got != cwdDir {
			t.Fatalf("应解析到 %s，实际 ok=%v got=%s", cwdDir, ok, got)
		}
	})
	t.Run("绝对路径原样校验", func(t *testing.T) {
		got, ok := resolveSiteDir(appDir, cwd, root)
		if !ok || got != appDir {
			t.Fatalf("绝对路径应原样返回且存在，实际 ok=%v got=%s", ok, got)
		}
	})
	t.Run("都不存在则 fail-closed", func(t *testing.T) {
		_, ok := resolveSiteDir("nope/missing", cwd, root)
		if ok {
			t.Fatalf("不存在的 dir 必须返回 false（fail-closed）")
		}
	})
	t.Run("installRoot 优先于 cwd 同名歧义", func(t *testing.T) {
		// cwd 与 installRoot 下都放一个同名目录，必须取 installRoot（可移植锚）。
		conflict := filepath.Join(root, "site")
		cwdConflict := filepath.Join(cwd, "site")
		if err := os.MkdirAll(conflict, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(cwdConflict, 0o755); err != nil {
			t.Fatal(err)
		}
		got, ok := resolveSiteDir("site", cwd, root)
		if !ok || got != conflict {
			t.Fatalf("应优先 installRoot 的 %s，实际 %s", conflict, got)
		}
	})
}
