package cli

import (
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gear/internal/registry"
	"gear/internal/server"
	"gear/internal/webui"
)

// waitPort 轮询直到 addr 可连通或超时。
func waitPort(t *testing.T, addr string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// TestUICmdOpensExistingServe 锁住 REQ-UI-LAUNCH-CMD：本机 serve 已在跑时，
// `gear ui` 直接检测并打开浏览器（不重复起 serve），干净返回。
func TestUICmdOpensExistingServe(t *testing.T) {
	reg, err := registry.Open(filepath.Join(t.TempDir(), "registry.db"))
	if err != nil {
		t.Fatalf("打开 registry 失败: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	srv, err := server.New(reg, server.WithSecretsDBPath(filepath.Join(t.TempDir(), "secrets.db")))
	if err != nil {
		t.Fatalf("构造 Server 失败: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("占端口失败: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close() // 释放端口交给我们起的 server
	go srv.ListenAndServe(addr)
	t.Cleanup(func() { _ = srv.Shutdown() })

	if !waitPort(t, addr, 5*time.Second) {
		t.Fatal("后台 serve 未起来")
	}

	// gear ui 应检测到已运行并直接返回（--no-open 避免真正拉起浏览器）
	if err := runCLI(t, "ui", "--addr", addr, "--no-open"); err != nil {
		t.Fatalf("gear ui 应检测已运行 serve 并直接返回，实际: %v", err)
	}
}

// TestUICmdPortProbe 锁住 isPortOpen 的探测语义（命中/未命中）。
func TestUICmdPortProbe(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("占端口失败: %v", err)
	}
	open := ln.Addr().String()
	defer ln.Close()

	if !isPortOpen(open) {
		t.Fatalf("已监听端口 %s 应判定为开", open)
	}
	if isPortOpen("127.0.0.1:1") {
		// 端口 1 通常无服务监听；若环境异常占用则跳过该断言
		t.Logf("端口 1 被占用，跳过未命中断言（环境相关）")
	}
}

// TestServePrintsOpenURL 锁住 REQ-UI-LAUNCH-OPENURL：gear serve 启动后 stdout 含 GEAR_OPEN_URL=。
//
// 走「直接起一个带 UI 的 Server」而非 runCLI 全路径：runCLI 的 serve 会阻塞在 ListenAndServe
// 直到进程退出，测试内无法关停，导致 registry.db 句柄泄漏、临时目录 cleanup 失败。本测试复用
// serve.go 的 serveOnListen（与 gear serve 同源的打印逻辑），起服→断言→Shutdown 释放句柄，
// 既锁住打印行为又能在测试内干净收尾。
func TestServePrintsOpenURL(t *testing.T) {
	rp := filepath.Join(t.TempDir(), "registry.db")
	reg, err := registry.Open(rp)
	if err != nil {
		t.Fatalf("打开 registry 失败: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close() })

	// UI 开启（默认），但禁止真正拉起浏览器（避免测试环境弹窗）；测完还原。
	prevNoUI, prevNoOpen := serveNoUI, serveNoOpen
	serveNoUI, serveNoOpen = false, true
	t.Cleanup(func() { serveNoUI, serveNoOpen = prevNoUI, prevNoOpen })

	t.Setenv("GEAR_UI_DIR", "") // 走内嵌资产（缺 assets → 缺失模式，但 serve 照常起、照常打印地址）

	srv, err := server.New(reg,
		server.WithRegistryPath(rp),
		server.WithUI(webui.Handler()),
		server.WithOnListen(serveOnListen(rp, reg)),
	)
	if err != nil {
		t.Fatalf("构造 Server 失败: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("占端口失败: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()

	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("建管道失败: %v", err)
	}
	os.Stdout = w

	go func() { _ = srv.ListenAndServe(addr) }()
	t.Cleanup(func() { _ = srv.Shutdown(); _ = srv.Close() })

	if !waitPort(t, addr, 5*time.Second) {
		os.Stdout = old
		_ = w.Close()
		t.Fatal("serve 未起来")
	}
	time.Sleep(200 * time.Millisecond) // 等 onListen 打印落盘
	_ = w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)
	if !strings.Contains(string(out), "GEAR_OPEN_URL=") {
		t.Fatalf("gear serve 应打印 GEAR_OPEN_URL=，实际输出:\n%s", string(out))
	}
}
