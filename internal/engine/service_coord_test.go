package engine

// 本文件锁住「多 app 共享同一个 service ext 侧车」的监管语义（2026-09-23 修复）。
//
// 背景：同主机跑两个 gear app run，第二个的 ext.serve 走 coord 握手时会**退让为 client**
// 复用第一个已起的 sidecar（§12.4 设计如此）。此时本 gear 手上没有本地进程
//（Proc 是 noopProcess，Wait() 立即返回 nil）。旧实现据此判定「service 已退出」，
// 监管器随即 Relaunch，而 Relaunch 直接 rt.Start 会硬抢已被占用的 coord 端口、
// 拿到 ExitAddrInUse(2) 速退 —— 于是「共享成功」表现为无限重启。

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"gear/internal/sidecar/coord"
)

// startCoordForTest 在空闲端口上起一个真实 coord 服务（不拉任何 sidecar 进程）。
func startCoordForTest(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("取空闲端口失败: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	c := coord.New(coord.Options{CheckInterval: 50 * time.Millisecond, TTL: 2 * time.Second, OnEmpty: func() {}})
	go func() { _ = c.Serve(addr, nil) }()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		conn, derr := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if derr == nil {
			_ = conn.Close()
			return addr
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("coord 服务未在 %s 上就绪", addr)
	return ""
}

// sharedSessionForTest 返回一个「退让为 client」的会话：本 gear 复用他人 sidecar，
// 手上没有本地进程。start 一旦被调用即证明误走了「本地拉起」路径。
func sharedSessionForTest(t *testing.T, addr string) *coord.Session {
	t.Helper()
	cli, err := coord.NewClient(addr, time.Second)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	sess, err := cli.RegisterOrStart(func() (coord.Process, error) {
		return nil, errors.New("已有实例时不应本地拉起 sidecar")
	})
	if err != nil {
		t.Fatalf("RegisterOrStart: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

// waitExitFakeProc 是可控退出的 Process：Wait 阻塞直到 exit 关闭。
type waitExitFakeProc struct {
	exit chan struct{}
}

func (p *waitExitFakeProc) Pid() int    { return 4242 }
func (p *waitExitFakeProc) Kill() error { return nil }
func (p *waitExitFakeProc) Wait() error { <-p.exit; return nil }

// 核心守卫：共享 sidecar（client 退让）没有本地进程可等，WaitExit 不得关闭 channel。
//
// 负向验证：把 HasLocalProc 的 coord 分支去掉（退化成「只要 Proc 非 nil 就算有本地进程」），
// noopProcess.Wait() 会立即返回 → 本测试必红（channel 立刻关闭）。
func TestWaitExitDoesNotFireForSharedSession(t *testing.T) {
	addr := startCoordForTest(t)
	sess := sharedSessionForTest(t, addr)

	// Proc 按 startServiceStep 的真实装配填入 sess.Proc()（client 场景即 noopProcess）：
	// 句柄里**有**一个 Proc 对象，只是它不代表任何本地进程 —— 这正是旧实现被骗的地方。
	h := &ServiceHandle{StepID: "serve", Coord: sess, Proc: sess.Proc()}
	if h.HasLocalProc() {
		t.Fatal("共享 sidecar（client 退让）本 gear 手上没有本地进程，HasLocalProc 应为 false")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	select {
	case <-h.WaitExit(ctx):
		t.Fatal("共享 sidecar 没有本地进程，不得判定为 service 退出 —— 否则监管器会把「复用成功」当成崩溃并无限重启")
	case <-time.After(3 * waitExitPoll):
	}
}

// 对照组：process 型 service 有真实本地进程，进程退出必须被感知（否则崩溃后不会被重启）。
//
// 负向验证：把 WaitExit 的轮询改成无条件阻塞（共享场景修好了但普通场景瘫了），本测试必红。
func TestWaitExitFiresWhenLocalProcExits(t *testing.T) {
	proc := &waitExitFakeProc{exit: make(chan struct{})}
	h := &ServiceHandle{StepID: "api", Proc: proc}
	if !h.HasLocalProc() {
		t.Fatal("process 型 service 持有本地进程，HasLocalProc 应为 true")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := h.WaitExit(ctx)
	close(proc.exit)
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("本地进程退出后 WaitExit 必须关闭 channel，否则崩溃的 service 永远等不到重启")
	}
}

// service ext 的重启必须走 coord 重握手，不得拿 Entry 直接 rt.Start ——
// Entry 带着 COORD_ADDR=<listen>，端口被他人 sidecar 占用时会拿到 ExitAddrInUse(2) 速退。
//
// 判据设计：Runtime 故意设成一个不存在的 runtime。走老路径（runtimeForCaps）必然报错；
// 走 coord 分支根本不碰 runtime。
//
// 负向验证：删掉 Relaunch 里的 coord 分支，本测试必红（runtimeForCaps 报未知 runtime）。
func TestRelaunchUsesCoordHandshakeNotRuntime(t *testing.T) {
	addr := startCoordForTest(t)
	sess := sharedSessionForTest(t, addr)

	h := &ServiceHandle{StepID: "serve", Runtime: "no-such-runtime", Coord: sess, Proc: sess.Proc()}
	if err := (&Engine{}).Relaunch(h); err != nil {
		t.Fatalf("service ext 重启应走 coord 重握手（不碰 runtime），实际报错: %v", err)
	}
	if h.Restarts != 1 {
		t.Fatalf("重启计数应为 1，实际 %d", h.Restarts)
	}
	if h.CurrentProc() == nil {
		t.Fatal("重握手后应持有 coord 返回的进程句柄（client 场景为 no-op，但不得为 nil）")
	}
}
