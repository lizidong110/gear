package webui

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWebUIEmbedHasPlaceholder 锁住「embed 总能编译」：public/ 下必须有 _placeholder.html，
// 否则 //go:embed all:public 在干净检出里会因目录为空而编译失败。
func TestWebUIEmbedHasPlaceholder(t *testing.T) {
	f, err := embedded.Open("public/_placeholder.html")
	if err != nil {
		t.Fatalf("embed 必须含 public/_placeholder.html（保证编译）: %v", err)
	}
	_ = f.Close()
}

// TestUIHandlerMissingMode 锁住「未构建不静默降级」：缺失态下任何 GET 返回 503 + 指引。
//
// ⚠ 为什么用 MissingHandler() 而不是内嵌：内嵌是否「已构建」取决于**仓库当前有没有前端产物**
// （`internal/webui/public/assets/` 由 build.sh 生成、被 gitignore）——
// 干净检出是缺失态，跑过一次 build.sh 就是完整态。测内嵌等于测仓库状态，
// 后果是「构建一次就红」这种莫名其妙的失败。这里用确定性构造入口锁行为。
func TestUIHandlerMissingMode(t *testing.T) {
	h := MissingHandler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("缺失模式 GET / 应 503，实际 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "未构建") {
		t.Fatalf("缺失模式应返回指引文案，实际: %s", rec.Body.String())
	}

	// 未知路径同样走缺失模式，不泄露空白
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/assets/missing.js", nil))
	if rec2.Code != http.StatusServiceUnavailable {
		t.Fatalf("缺失模式 GET /assets/missing.js 应 503，实际 %d", rec2.Code)
	}
}

// TestHandlerEmbeddedFollowsBuildState 锁住「内嵌源跟随构建状态」：
// 内嵌源下 Handler() 的 missing 判定与 embed 内容一致。
//
// 这条**不断言具体是哪种状态**（干净检出=缺失、构建后=完整，两种都合法），
// 只断言「判定自洽」：内嵌有 assets/ 时不是缺失态，没有时是。
// 目的是把「仓库状态」这个变量从失败原因里摘出去——状态变了也不该红。
func TestHandlerEmbeddedFollowsBuildState(t *testing.T) {
	t.Setenv(envUIDir, "") // 走内嵌
	h := Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	hasRealAssets := hasAssets(embedded, "public")
	if hasRealAssets && rec.Code == http.StatusServiceUnavailable {
		t.Fatalf("内嵌已有前端产物时不应是缺失态，实际 503")
	}
	if !hasRealAssets && rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("内嵌无前端产物时应是缺失态 503，实际 %d", rec.Code)
	}
}

// TestUIHandlerExternalReal 锁住「GEAR_UI_DIR 外部目录托管 + SPA 兜底 + 内容类型」。
func TestUIHandlerExternalReal(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html><title>Gear UI</title></html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "assets", "app.js"), []byte("console.log(1)"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envUIDir, dir)
	h := Handler()

	// GET / → index.html
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / 应 200，实际 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "<title>Gear UI</title>") {
		t.Fatalf("GET / 应返回 index.html，实际: %s", rec.Body.String())
	}

	// GET /assets/app.js → 正确的 JS 内容类型
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/assets/app.js", nil))
	if rec2.Code != http.StatusOK {
		t.Fatalf("GET /assets/app.js 应 200，实际 %d", rec2.Code)
	}
	if !strings.Contains(rec2.Header().Get("Content-Type"), "javascript") {
		t.Fatalf("GET /assets/app.js 内容类型应为 javascript，实际 %q", rec2.Header().Get("Content-Type"))
	}

	// 未知路径走 SPA 兜底 → index.html
	rec3 := httptest.NewRecorder()
	h.ServeHTTP(rec3, httptest.NewRequest(http.MethodGet, "/some/deep/link", nil))
	if rec3.Code != http.StatusOK || !strings.Contains(rec3.Body.String(), "<title>Gear UI</title>") {
		t.Fatalf("未知路径应兜底到 index.html，实际 %d: %s", rec3.Code, rec3.Body.String())
	}
}

// TestUIHandlerExternalMissingAssets 锁住「外部目录也没有 assets/ 时同样进入缺失模式」。
func TestUIHandlerExternalMissingAssets(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html></html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envUIDir, dir)
	h := Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("外部目录缺 assets/ 时应 503，实际 %d", rec.Code)
	}
}

// TestUIHandlerContentType 锁住各扩展名的内容类型判定（不依赖系统 mime 表）。
func TestUIHandlerContentType(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "index.html"), []byte("x"), 0o644)
	os.MkdirAll(filepath.Join(dir, "assets"), 0o755)
	for name, want := range map[string]string{
		"a.css":  "text/css",
		"b.json": "application/json",
		"c.svg":  "image/svg+xml",
	} {
		os.WriteFile(filepath.Join(dir, "assets", name), []byte("x"), 0o644)
		_ = want
	}
	t.Setenv(envUIDir, dir)
	h := Handler()
	for name, want := range map[string]string{
		"a.css":  "text/css",
		"b.json": "application/json",
		"c.svg":  "image/svg+xml",
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/assets/"+name, nil))
		ct := rec.Header().Get("Content-Type")
		if !strings.Contains(ct, want) {
			t.Fatalf("/assets/%s 内容类型应为含 %q，实际 %q", name, want, ct)
		}
	}
}
