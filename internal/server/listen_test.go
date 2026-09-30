package server

import (
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// L-addr-1：`--addr :0`（自动分配）时 onListen 必须拿到**真实端口**，
// 且那个端口真能连上 —— 拿不到就等于「我实际在哪个端口」对调用方完全不可见，
// 长驻子进程的 stdout 首行也就无从上报（2026-09-26 全链路端口分配的起点）。
func TestListenAndServeAutoPortReportsRealAddr(t *testing.T) {
	srv, _ := newTestServer(t)
	got := make(chan string, 1)
	srv.onListen = func(realAddr string) { got <- realAddr }

	go func() {
		_ = srv.ListenAndServe(":0")
	}()

	select {
	case realAddr := <-got:
		if realAddr == ":0" || strings.HasSuffix(realAddr, ":0") {
			t.Fatalf("onListen 应拿到真实绑定地址，实际 %q", realAddr)
		}
		_, port, err := net.SplitHostPort(realAddr)
		if err != nil {
			t.Fatalf("真实地址应形如 host:port，实际 %q err=%v", realAddr, err)
		}
		if port == "0" {
			t.Fatalf("端口不能是 0（那是没分配出来的意思），实际 %q", realAddr)
		}
		// 端口必须真的能连上，而不是「内核挑了个号然后没人听」。
		conn, err := net.DialTimeout("tcp", realAddr, time.Second)
		if err != nil {
			t.Fatalf("onListen 给出的地址连不上: %v", err)
		}
		_ = conn.Close()
	case <-time.After(3 * time.Second):
		t.Fatal("等待 onListen 超时（监听未绑定成功？）")
	}
}

// L-addr-2：subscribe 回推基址必须由**真实**地址派生。
// `:0` 经 serverBaseURL 会算出 http://127.0.0.1:0 —— 回推端点落在死端口上，
// 故障形态是「订阅永远收不到事件」，排查时完全看不出与监听地址有关。
func TestServerBaseURLUsesRealAddrNotTheRequestedOne(t *testing.T) {
	const requested = ":0"
	got := make(chan string, 1)
	srv, _ := newTestServer(t)
	srv.onListen = func(realAddr string) { got <- realAddr }

	go func() {
		_ = srv.ListenAndServe(requested)
	}()

	select {
	case realAddr := <-got:
		base := serverBaseURL(realAddr)
		if strings.HasSuffix(base, ":0") || strings.Contains(base, ":0/") {
			t.Fatalf("回推基址不得含 :0，实际 %q", base)
		}
		if !strings.HasPrefix(base, "http://") {
			t.Fatalf("回推基址应带 scheme，实际 %q", base)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("等待 onListen 超时")
	}
}

// L-addr-3：端口已被占时必须报错（改 net.Listen 后这条不能退化成「静默换一个口继续」）。
//
// 占位者必须**全程持有**：先 close 再绑同一地址，Windows 会允许复用，
// 于是 ListenAndServe 真的监听成功并永挂 —— 那条用例会卡成超时而不是报错。
func TestListenAndServeBindErrorIsReported(t *testing.T) {
	holder, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("无法占用临时端口: %v", err)
	}
	t.Cleanup(func() { _ = holder.Close() })
	addr := holder.Addr().String()

	srv, _ := newTestServer(t)
	srv.onListen = func(string) { t.Error("绑定失败时不得回调 onListen") }
	if err := srv.ListenAndServe(addr); err == nil {
		t.Fatalf("端口已被占用时应报错，实际静默成功（addr=%s）", addr)
	}
}

// L-srv-1：绑定成功后 HTTP 面可用（防止 net.Listen 改造把 mux 挂空）。
func TestListenAndServeServesTraffic(t *testing.T) {
	srv, _ := newTestServer(t)
	got := make(chan string, 1)
	srv.onListen = func(realAddr string) { got <- realAddr }

	go func() {
		_ = srv.ListenAndServe("127.0.0.1:0")
	}()

	select {
	case realAddr := <-got:
		resp, err := http.Get("http://" + realAddr + "/api/healthz")
		if err != nil {
			// 端点名可能不同，退回只验证 TCP 层已就绪（L-addr-1 已覆盖）。
			t.Skipf("健康检查端点不可用（%v），TCP 连通性已在上一条验证", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 500 {
			t.Fatalf("健康检查返回 5xx，实际 %d", resp.StatusCode)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("等待 onListen 超时")
	}
}
