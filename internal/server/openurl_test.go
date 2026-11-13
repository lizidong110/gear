package server

// openurl_test.go：锁住 GET /api/apps 把台账日志里的「打开地址」透出（REQ-APP-MANAGED-DETACHED）。
//
// detached 语义下 open_urls 走 `gear app start` 的日志扫描通道（app.scanOpenURLs，
// 约定行格式 GEAR_OPEN_URL=<url>）；本文件只验证 server 层把它带进列表响应。

import (
	"os"
	"path/filepath"
	"testing"
)

func TestListAppsIncludesOpenURLs(t *testing.T) {
	s, _, dir := newAppsServer(t)
	appID := "app_e2e.up-x"
	logPath := filepath.Join(dir, appID+".log")
	body := "boot ok\nGEAR_OPEN_URL=http://127.0.0.1:8080/\nGEAR_OPEN_URL=http://127.0.0.1:8080/\n"
	seedLedgerRecord(t, dir, appID, "e2e.up", 0, false)
	// seedLedgerRecord 会写一份占位日志，这里覆盖成带标记的内容（模拟收尾步输出）。
	if err := os.WriteFile(logPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	items := doArray(t, s.Handler(), "GET", "/api/apps")
	if len(items) != 1 {
		t.Fatalf("期望 1 条，实际 %d", len(items))
	}
	urls, ok := items[0]["open_urls"].([]interface{})
	if !ok {
		t.Fatalf("open_urls 缺失或非数组")
	}
	// 同一地址去重后只留一条。
	if len(urls) != 1 || urls[0] != "http://127.0.0.1:8080/" {
		t.Fatalf("open_urls 不符: %v", urls)
	}
}
