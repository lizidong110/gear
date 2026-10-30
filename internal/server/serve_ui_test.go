package server

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gear/internal/registry"
	"gear/internal/webui"
)

// writeUITempDir 造一个「真实前端构建产物」目录（index.html + 非空 assets/），供 UI handler 托管。
func writeUITempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html><title>Gear UI</title><body>hi</body></html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "assets", "app.js"), []byte("console.log(1)"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// newTestServerUI 起一个带 Web UI 的 Server；uiDir 为空字符串表示「内嵌」。
//
// ⚠ 内嵌是否为「缺失态」取决于仓库当前有没有前端产物（build.sh 生成、被 gitignore），
// 所以需要确定性的缺失态时用 newTestServerMissingUI（走 webui.MissingHandler）。
func newTestServerUI(t *testing.T, uiDir string) *Server {
	t.Helper()
	reg, err := registry.Open(filepath.Join(t.TempDir(), "registry.db"))
	if err != nil {
		t.Fatalf("打开 registry 失败: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	t.Setenv("GEAR_UI_DIR", uiDir) // 空串 = 走内嵌
	srv, err := New(reg, WithSecretsDBPath(filepath.Join(t.TempDir(), "secrets.db")), WithUI(webui.Handler()))
	if err != nil {
		t.Fatalf("构造 Server 失败: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return srv
}

// newTestServerMissingUI 起一个「前端未构建」状态的 Server（确定性，不依赖仓库状态）。
func newTestServerMissingUI(t *testing.T) *Server {
	t.Helper()
	reg, err := registry.Open(filepath.Join(t.TempDir(), "registry.db"))
	if err != nil {
		t.Fatalf("打开 registry 失败: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	srv, err := New(reg, WithSecretsDBPath(filepath.Join(t.TempDir(), "secrets.db")), WithUI(webui.MissingHandler()))
	if err != nil {
		t.Fatalf("构造 Server 失败: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return srv
}

// TestServeUIIndexServesRealUI 锁住「UI 启用时 GET / 返回真实界面、/assets 命中静态文件」。
func TestServeUIIndexServesRealUI(t *testing.T) {
	srv := newTestServerUI(t, writeUITempDir(t))
	h := srv.Handler()

	rec, _ := do(t, h, http.MethodGet, "/", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / 应 200，实际 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "<title>Gear UI</title>") {
		t.Fatalf("GET / 应返回真实 index.html，实际: %s", rec.Body.String())
	}

	rec2, _ := do(t, h, http.MethodGet, "/assets/app.js", nil)
	if rec2.Code != http.StatusOK {
		t.Fatalf("GET /assets/app.js 应 200，实际 %d", rec2.Code)
	}
}

// TestServeUIRouteIsolatedFromAPI 锁住「UI 静态路由不吞 /api/*」（REQ-UI-NF-NO-KERNEL）。
func TestServeUIRouteIsolatedFromAPI(t *testing.T) {
	srv := newTestServerUI(t, writeUITempDir(t))
	h := srv.Handler()

	// 健康检查与 tools 列表必须仍由 REST 执行面处理（200），不被 /{path...} 兜底吞掉。
	rec, _ := do(t, h, http.MethodGet, "/api/healthz", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/healthz 应 200，实际 %d（UI 路由吞掉了 API）", rec.Code)
	}
	rec2, _ := do(t, h, http.MethodGet, "/api/tools", nil)
	if rec2.Code != http.StatusOK {
		t.Fatalf("GET /api/tools 应 200，实际 %d（UI 路由吞掉了 API）", rec2.Code)
	}
	// 未知非 API 路径走 SPA 兜底 → 200 index.html，不干扰既有 API。
	rec3, _ := do(t, h, http.MethodGet, "/flows/news", nil)
	if rec3.Code != http.StatusOK {
		t.Fatalf("未知路径应 SPA 兜底 200，实际 %d", rec3.Code)
	}
}

// TestServeUIMissingDistReturnsGuidance 锁住「未构建时 / 返回 503 指引，且 /api 仍可用」。
func TestServeUIMissingDistReturnsGuidance(t *testing.T) {
	srv := newTestServerMissingUI(t)
	h := srv.Handler()

	rec, _ := do(t, h, http.MethodGet, "/", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("缺失模式 GET / 应 503，实际 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "未构建") {
		t.Fatalf("缺失模式应返回指引，实际: %s", rec.Body.String())
	}
	// API 不受缺失模式影响
	rec2, _ := do(t, h, http.MethodGet, "/api/healthz", nil)
	if rec2.Code != http.StatusOK {
		t.Fatalf("缺失模式下 /api/healthz 仍应 200，实际 %d", rec2.Code)
	}
}

// TestServeUINoUIDisablesRoot 锁住「--no-ui（WithUI(nil)）时 GET / 返回 404 指引，不影响 /api」。
func TestServeUINoUIDisablesRoot(t *testing.T) {
	reg, err := registry.Open(filepath.Join(t.TempDir(), "registry.db"))
	if err != nil {
		t.Fatalf("打开 registry 失败: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	srv, err := New(reg, WithSecretsDBPath(filepath.Join(t.TempDir(), "secrets.db")), WithUI(nil))
	if err != nil {
		t.Fatalf("构造 Server 失败: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	h := srv.Handler()

	rec, _ := do(t, h, http.MethodGet, "/", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("--no-ui 时 GET / 应 404，实际 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "未启用") {
		t.Fatalf("--no-ui 应给出指引，实际: %s", rec.Body.String())
	}
	rec2, _ := do(t, h, http.MethodGet, "/api/healthz", nil)
	if rec2.Code != http.StatusOK {
		t.Fatalf("--no-ui 时 /api/healthz 仍应 200，实际 %d", rec2.Code)
	}
}
