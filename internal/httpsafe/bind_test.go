package httpsafe

import (
	"io"
	"os"
	"strings"
	"testing"
)

// captureStderr 捕获 f 期间写入 stderr 的内容（放行警告走 stderr，需截住才能断言）。
func captureStderr(t *testing.T, f func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	f()
	os.Stderr = old
	_ = w.Close()
	out, _ := io.ReadAll(r)
	return string(out)
}

// B-bind-1：回环地址放行；非回环（0.0.0.0、具体网卡 IP、**空 host**）默认拒绝。
// 空 host（`:8787`）在 net.Listen 里绑全网卡，与 0.0.0.0 等价，不算回环——
// 这是比 0.0.0.0 更顺手也更危险的写法。
func TestCheckBindAddrRejectsNonLoopbackByDefault(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:8787", ":8787", "192.168.1.5:8787", "[::]:8787"} {
		if err := CheckBindAddr("测试面", addr, "读写 secrets", false); err == nil {
			t.Fatalf("非回环地址 %q 默认应拒绝，实际放行", addr)
		}
	}
	for _, addr := range []string{"127.0.0.1:8787", "localhost:8787", "[::1]:8787"} {
		if err := CheckBindAddr("测试面", addr, "读写 secrets", false); err != nil {
			t.Fatalf("回环地址 %q 应放行，实际拒绝: %v", addr, err)
		}
	}
}

// B-bind-2：allowRemote 放行但必须在 stderr 留警告——静默放行等于替使用者做了
// 一个他们不知情的决定。
func TestCheckBindAddrAllowRemoteWarns(t *testing.T) {
	warn := captureStderr(t, func() {
		if err := CheckBindAddr("测试面", "0.0.0.0:8787", "读写明文 secrets", true); err != nil {
			t.Fatalf("allowRemote 应放行，实际拒绝: %v", err)
		}
	})
	if !strings.Contains(warn, "读写明文 secrets") {
		t.Fatalf("放行警告必须说清暴露了什么，实际 %q", warn)
	}
}

// B-bind-3：拒绝文案必须包含面名与改法（127.0.0.1 / --allow-remote），
// 否则使用者只看到「拒绝」不知道怎么救。
func TestCheckBindAddrErrorMessageIsActionable(t *testing.T) {
	err := CheckBindAddr("REST 执行面", ":8787", "读写 registry", false)
	if err == nil {
		t.Fatal("空 host 默认应拒绝")
	}
	msg := err.Error()
	for _, want := range []string{"REST 执行面", "127.0.0.1", "--allow-remote"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("拒绝文案应包含 %q，实际 %q", want, msg)
		}
	}
}

// B-bind-4：非 host:port 形态直接拒绝（不进入回环判定）。
func TestCheckBindAddrRejectsMalformedAddr(t *testing.T) {
	for _, addr := range []string{"127.0.0.1", "http://127.0.0.1:8787"} {
		if err := CheckBindAddr("测试面", addr, "读写 secrets", false); err == nil {
			t.Fatalf("非法地址 %q 应报错", addr)
		}
	}
}
