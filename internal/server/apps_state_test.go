package server

// apps_state_test.go：锁住 /api/apps 的 **detached 台账语义**（REQ-APP-MANAGED-DETACHED）。
//
// 背景（2026-11-13 方案 A）：POST /api/apps 从「进程内 Supervise」改为 `gear app start`
// 的 REST 包装——app 是脱离 serve 进程树的独立进程，列表 / 状态全部以落盘台账为唯一真相源。
// 这里锁定三件事：列表空台账为空数组、未知 Flow 404、台账里的 restarts / live_logs /
// state 原样透出（重启次数由被派生的监管进程经 GEAR_APP_LEDGER_* env 回写，见 app.RestartRecorderFromEnv）。

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gear/internal/app"
	"gear/internal/registry"
)

// newAppsServer 造一套隔离的 registry + 台账目录并注入 Server（/api/apps 依赖两者定位台账）。
func newAppsServer(t *testing.T) (s *Server, reg *registry.Registry, appsDir string) {
	t.Helper()
	regPath := filepath.Join(t.TempDir(), "registry.db")
	reg, err := registry.Open(regPath)
	if err != nil {
		t.Fatalf("打开 registry 失败: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	appsDir = filepath.Join(t.TempDir(), "apps")
	if err := os.MkdirAll(appsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	srv, err := New(reg,
		WithSecretsDBPath(filepath.Join(t.TempDir(), "secrets.db")),
		WithRegistryPath(regPath), WithAppsDir(appsDir))
	if err != nil {
		t.Fatalf("构造 Server 失败: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return srv, reg, appsDir
}

// TestAppsListEmptyWithoutLedger：没有台账时列表必须是**空数组**而不是 null。
func TestAppsListEmptyWithoutLedger(t *testing.T) {
	s, _, _ := newAppsServer(t)
	items := doArray(t, s.Handler(), "GET", "/api/apps")
	if len(items) != 0 {
		t.Fatalf("空台账应返回空数组，实际 %d 条", len(items))
	}
}

// TestAppsCreateUnknownFlow404：Flow 存在性预检必须在派生**之前**给出 404。
func TestAppsCreateUnknownFlow404(t *testing.T) {
	s, _, _ := newAppsServer(t)
	rec, _ := do(t, s.Handler(), "POST", "/api/apps", map[string]interface{}{"flow_id": "nope"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未知 Flow 期望 404，实际 %d %s", rec.Code, rec.Body.String())
	}
}

// seedLedgerRecord 直接种一条台账（含日志文件），绕开真实派生进程，专注列表投影逻辑。
func seedLedgerRecord(t *testing.T, dir, appID, flowID string, restarts int, liveLogs bool) *app.Record {
	t.Helper()
	logPath := filepath.Join(dir, appID+".log")
	if err := os.WriteFile(logPath, []byte("[app run] 监管 1 个 Flow\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := &app.Record{
		AppID: appID, FlowID: flowID, Registry: dir, PID: os.Getpid(), // 本测试进程必然存活 ⇒ 探活 running
		Restart: "always", StartedAt: time.Now(), CWD: ".", LogPath: logPath, LiveLogs: liveLogs, Restarts: restarts,
	}
	if err := app.SaveRecord(dir, rec); err != nil {
		t.Fatalf("写台账失败: %v", err)
	}
	return rec
}

// TestAppsListProjectsLedgerFields：GET /api/apps 必须把台账的 restarts / live_logs /
// state（双判据实时探活）透出——重启次数只存在于台账（由监管进程回写），不透出就是假健康。
func TestAppsListProjectsLedgerFields(t *testing.T) {
	s, _, dir := newAppsServer(t)
	seedLedgerRecord(t, dir, "app_e2e.up-x", "e2e.up", 3, true)

	items := doArray(t, s.Handler(), "GET", "/api/apps")
	if len(items) != 1 {
		t.Fatalf("列表应为 1 条，实际 %d", len(items))
	}
	it := items[0]
	if it["app_id"] != "app_e2e.up-x" {
		t.Fatalf("app_id 不符: %v", it["app_id"])
	}
	if n, ok := it["restarts"].(float64); !ok || n != 3 {
		t.Fatalf("restarts 应为 3（台账回写值），实际 %v", it["restarts"])
	}
	if v, ok := it["live_logs"].(bool); !ok || !v {
		t.Fatalf("live_logs 应为 true，实际 %v", it["live_logs"])
	}
	if it["state"] != "running" {
		t.Fatalf("pid 存活且未声明端口应判 running，实际 %v", it["state"])
	}
}
