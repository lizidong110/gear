package engine

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"gear/internal/sidecar/coord"
)

// ───────── 测试助手 ─────────

// svcFakeProc 实现 coord.Process：把「本进程」包装成侧车进程句柄（测试专用）。
type svcFakeProc struct{ pid int }

func (p *svcFakeProc) Pid() int    { return p.pid }
func (p *svcFakeProc) Kill() error { return nil }
func (p *svcFakeProc) Wait() error { return nil }

// svcFreeAddr 找一个空闲端口（监听后立刻关闭，返回地址字符串）。
func svcFreeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("找空闲端口失败: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// svcPingHandler 回显收到的信封体，用来断言「args 被包在 {"args":...} 里」这一契约。
func svcPingHandler(t *testing.T, seen *atomic.Value) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var env struct {
			Args map[string]interface{} `json:"args"`
		}
		if err := json.NewDecoder(r.Body).Decode(&env); err != nil {
			t.Errorf("解析信封失败: %v", err)
			writeJSONTest(w, map[string]any{"error": "bad_request"})
			return
		}
		seen.Store(env.Args)
		writeJSONTest(w, map[string]any{"result": map[string]any{"echo": env.Args["x"]}})
	}
}

func writeJSONTest(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// ───────── 用例 ─────────

// TestServiceHTTPTransportAutoStartsSidecar：无监听时首次 Call 必须自动拉起侧车
// （owner 握手），且信封契约与 LocalHTTPTransport 一致；随后复用不再拉。
// 这是 2026-10-20 真机缺口的直接回归：planner 选 ext.pdf.read 执行期连接拒绝。
func TestServiceHTTPTransportAutoStartsSidecar(t *testing.T) {
	addr := svcFreeAddr(t)
	cli, err := coord.NewClient(addr, 2*time.Second)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	var started atomic.Int32
	var seen atomic.Value
	start := func() (coord.Process, error) {
		started.Add(1)
		srv := coord.New(coord.Options{CheckInterval: 50 * time.Millisecond, TTL: 2 * time.Second, OnEmpty: func() {}})
		go func() { _ = srv.Serve(addr, map[string]http.HandlerFunc{"/ping": svcPingHandler(t, &seen)}) }()
		return &svcFakeProc{pid: os.Getpid()}, nil
	}
	tr := newServiceHTTPTransport("ext.pdf", "COORD_ADDR="+addr+" plugin/pdf-sidecar.exe",
		cli, NewLocalHTTPTransport(addr), start)
	defer tr.Close()

	got, err := tr.Call("ping", map[string]interface{}{"x": 1.0})
	if err != nil {
		t.Fatalf("首次 Call 应自动拉起侧车并成功，实际 %v", err)
	}
	res, _ := got.(map[string]interface{})
	if res == nil || fmt.Sprint(res["echo"]) != "1" {
		t.Fatalf("回显结果不对: %v", got)
	}
	if started.Load() != 1 {
		t.Fatalf("无监听时应本 gear 当 owner 拉起一次，实际 %d", started.Load())
	}
	if args, _ := seen.Load().(map[string]interface{}); args == nil {
		t.Fatal("侧车应收到 {\"args\":...} 信封（与 LocalHTTPTransport 同契约）")
	}

	// 第二次 Call：复用同一 session，不再拉起。
	if _, err := tr.Call("ping", map[string]interface{}{"x": 2.0}); err != nil {
		t.Fatalf("第二次 Call 应复用已拉起的侧车，实际 %v", err)
	}
	if started.Load() != 1 {
		t.Fatalf("已拉起后不得重复 start，实际 %d", started.Load())
	}
}

// TestServiceHTTPTransportReusesRunningSidecar：已有实例（别的 gear 拉的）时必须退让为
// client——不本地拉起、不抢端口，直接复用。同主机多 gear 共享单个侧车（§12.4）。
func TestServiceHTTPTransportReusesRunningSidecar(t *testing.T) {
	addr := svcFreeAddr(t)
	owner := coord.New(coord.Options{CheckInterval: 50 * time.Millisecond, TTL: 2 * time.Second, OnEmpty: func() {}})
	var seen atomic.Value
	go func() { _ = owner.Serve(addr, map[string]http.HandlerFunc{"/ping": svcPingHandler(t, &seen)}) }()
	waitSvcServe(t, addr)

	cli, err := coord.NewClient(addr, 2*time.Second)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	var started atomic.Int32
	tr := newServiceHTTPTransport("ext.pdf", "COORD_ADDR="+addr+" plugin/pdf-sidecar.exe",
		cli, NewLocalHTTPTransport(addr),
		func() (coord.Process, error) {
			started.Add(1)
			return &svcFakeProc{pid: os.Getpid()}, nil
		})
	defer tr.Close()

	got, err := tr.Call("ping", map[string]interface{}{"x": 9.0})
	if err != nil {
		t.Fatalf("复用已有侧车应成功，实际 %v", err)
	}
	res, _ := got.(map[string]interface{})
	if res == nil || fmt.Sprint(res["echo"]) != "9" {
		t.Fatalf("回显结果不对: %v", got)
	}
	if started.Load() != 0 {
		t.Fatalf("已有实例时不得本地拉起，实际 %d", started.Load())
	}
}

// TestServiceHTTPTransportStartFailureIsActionable：owner 拉起失败（二进制缺失等）时，
// Call 必须返回**可操作**的错误——点名 ext 与 listen，而不是裸 connection refused。
func TestServiceHTTPTransportStartFailureIsActionable(t *testing.T) {
	addr := svcFreeAddr(t)
	cli, err := coord.NewClient(addr, 2*time.Second)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	tr := newServiceHTTPTransport("ext.pdf", "plugin/pdf-sidecar.exe",
		cli, NewLocalHTTPTransport(addr),
		func() (coord.Process, error) {
			return nil, fmt.Errorf("exec: \"plugin/pdf-sidecar.exe\": executable file not found")
		})
	defer tr.Close()

	_, err = tr.Call("read", nil)
	if err == nil {
		t.Fatal("拉起失败时 Call 必须报错")
	}
	msg := err.Error()
	if !containsAll(msg, "ext.pdf", "侧车未就绪", "executable file not found") {
		t.Fatalf("错误必须点名 ext/根因/可操作性，实际 %q", msg)
	}
	// 失败结果缓存：不反复重拉（持久性原因重试无意义）。
	if _, err2 := tr.Call("read", nil); err2 == nil || err2.Error() != msg {
		t.Fatalf("失败应缓存（once 语义），两次错误应一致，实际 %q vs %q", msg, err2)
	}
}

// TestResolveSidecarArgv0：启动命令 argv[0] 的解析规则——CWD 有就原样、没有按 exe 目录兜底、
// 都没有原样返回（让运行期报错指向「二进制未构建」）。
func TestResolveSidecarArgv0(t *testing.T) {
	t.Chdir(t.TempDir())

	// 1) CWD 下存在 → 原样。
	rel := "plugin/pdf-sidecar.exe"
	if err := os.MkdirAll(filepath.Dir(rel), 0o755); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	if err := os.WriteFile(rel, []byte("x"), 0o644); err != nil {
		t.Fatalf("写探针文件失败: %v", err)
	}
	if got := resolveSidecarArgv0(rel); got != rel {
		t.Fatalf("CWD 下存在应原样返回，实际 %q", got)
	}

	// 2) CWD 下没有、exe 目录（gear 二进制所在目录）有 → 换成 exeDir 下的绝对路径。
	missing := "plugin/definitely-not-here-20261020.exe"
	_ = os.Remove(missing)
	if d := exeDirForTest(t); d != "" {
		probe := filepath.Join(d, missing)
		if err := os.MkdirAll(filepath.Dir(probe), 0o755); err != nil {
			t.Fatalf("exe 目录下建目录失败: %v", err)
		}
		if err := os.WriteFile(probe, []byte("x"), 0o644); err != nil {
			t.Skipf("无法在 exe 目录写探针文件: %v", err)
		}
		defer os.Remove(probe)
		got := resolveSidecarArgv0(missing)
		if got != probe {
			t.Fatalf("应兜底到 exe 目录: got=%q want=%q", got, probe)
		}
	} else {
		t.Skip("拿不到 exe 目录")
	}

	// 3) 都没有 → 原样返回（运行期报错指向二进制未构建）。
	if got := resolveSidecarArgv0("plugin/nowhere-at-all.exe"); got != "plugin/nowhere-at-all.exe" {
		t.Fatalf("都不存在应原样返回，实际 %q", got)
	}

	// 4) 裸名 → 原样（交给 cmdline.LookExecutable 的 exe 目录优先规则）。
	if got := resolveSidecarArgv0("pdf-sidecar.exe"); got != "pdf-sidecar.exe" {
		t.Fatalf("裸名应原样返回，实际 %q", got)
	}
}

// exeDirForTest 取测试二进制的目录（gearhome.ExeDir 的薄包装，便于测试替身）。
func exeDirForTest(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return filepath.Dir(exe)
}

// waitSvcServe 等 coord 端口可连（侧车 bind 完成），避免注册撞上启动窗口。
func waitSvcServe(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("等待 coord 服务就绪超时: %s", addr)
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		found := false
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
