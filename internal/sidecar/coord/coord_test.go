// coord 包的规格测试：逐条锁住 EXT_SERVICE_DESIGN.md §12 的协议与语义。
//
// **恢复说明**：本包原有的 coord_test.go / coord_binary_test.go 在 2026-09-19 的批量误删
// 事件中丢失，且其备份记录为 0 字节（只记「新增」不存内容），无法复原。本文件是按 §12.3
// 协议表、§12.5 巡检规则、§12.9 验收条款重新编写的守卫测试——因为 coord 包本身也是重建的，
// 这些断言是「重建实现是否忠于规范」的唯一证据来源。
package coord

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ---- 测试辅助 ----

// postHandler 直接调 unexported handler（绕开网络），返回解析后的响应体。
func postHandler(t *testing.T, h http.HandlerFunc, body string) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h(rec, req)
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是 JSON: %q (err=%v)", rec.Body.String(), err)
	}
	return out
}

func rawBody(t *testing.T, h http.HandlerFunc, body string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec.Body.String()
}

func boolOf(t *testing.T, m map[string]any, key string) bool {
	t.Helper()
	v, ok := m[key].(bool)
	if !ok {
		t.Fatalf("响应缺少布尔字段 %q: %#v", key, m)
	}
	return v
}

// freeAddr 取一个当前空闲的 127.0.0.1 端口（测试内即时使用，竞态窗口可忽略）。
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("取空闲端口失败: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// serveCoord 在真实端口上起一个 coord 服务，返回其地址。
func serveCoord(t *testing.T, onEmpty func()) (string, *Coordinator) {
	t.Helper()
	addr := freeAddr(t)
	c := New(Options{CheckInterval: 50 * time.Millisecond, TTL: 2 * time.Second, OnEmpty: onEmpty})
	go func() { _ = c.Serve(addr, nil) }()
	waitListening(t, addr)
	return addr, c
}

func waitListening(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("coord 服务未在 %s 上就绪", addr)
}

// oneShotEmpty 返回一个只记录「是否被触发过」的 OnEmpty 回调与其查询函数。
func oneShotEmpty() (func(), func() bool) {
	var fired atomic.Bool
	return func() { fired.Store(true) }, fired.Load
}

// fakeProc 是可控的 Process：记录 Kill 调用次数，Pid 可指定。
type fakeProc struct {
	pid    int
	killed atomic.Int32
}

func (f *fakeProc) Pid() int    { return f.pid }
func (f *fakeProc) Kill() error { f.killed.Add(1); return nil }
func (f *fakeProc) Wait() error { return nil }

// ---- §12.3 协议 ----

// 注册 → 心跳 → 占用查询 → 注销 的完整生命周期，且注销后列表为空并触发 OnEmpty。
func TestCoordLifecycle(t *testing.T) {
	empty, fired := oneShotEmpty()
	c := New(Options{TTL: time.Second, OnEmpty: empty})
	const pid = 4242

	if got := postHandler(t, c.handleRegister, `{"pid":4242,"token":"tok-aaa","ttl":3}`); !boolOf(t, got, "ok") {
		t.Fatalf("首次注册应成功: %#v", got)
	}
	if got := postHandler(t, c.handleHeartbeat, `{"pid":4242,"token":"tok-aaa"}`); !boolOf(t, got, "ok") {
		t.Fatalf("心跳应成功: %#v", got)
	}
	if pids := occupantsOf(t, c); len(pids) != 1 || pids[0] != pid {
		t.Fatalf("占用列表应为 [%d]，实际 %v", pid, pids)
	}
	if got := postHandler(t, c.handleDeregister, `{"pid":4242,"token":"tok-aaa"}`); !boolOf(t, got, "ok") {
		t.Fatalf("注销应成功: %#v", got)
	}
	if pids := occupantsOf(t, c); len(pids) != 0 {
		t.Fatalf("注销后占用列表应为空，实际 %v", pids)
	}
	if !fired() {
		t.Fatal("最后一个占用注销后应触发 OnEmpty（sidecar 自停）")
	}
}

// /occupants 的响应体必须**恰好**是 {"pids":[...]}（§12.3 规定的字段名，probe 依赖它）。
func TestCoordOccupantsShape(t *testing.T) {
	c := New(Options{TTL: time.Second})
	_ = postHandler(t, c.handleRegister, `{"pid":7,"token":"t","ttl":3}`)

	body := rawBody(t, c.handleOccupants, ``)
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("占用响应不是 JSON 对象: %q", body)
	}
	if len(m) != 1 {
		t.Fatalf("占用响应应只有 pids 一个字段，实际 %v", keysOf(m))
	}
	if _, ok := m["pids"]; !ok {
		t.Fatalf("占用响应字段名必须是 pids（§12.3），实际 %v", keysOf(m))
	}
}

// §12.6 核心：同一 PID 换 token 注册必须被拒（taken），且不覆盖原占用。
func TestCoordRejectsPIDReuseByToken(t *testing.T) {
	c := New(Options{TTL: time.Second})
	if got := postHandler(t, c.handleRegister, `{"pid":99,"token":"first","ttl":3}`); !boolOf(t, got, "ok") {
		t.Fatalf("首次注册应成功: %#v", got)
	}
	got := postHandler(t, c.handleRegister, `{"pid":99,"token":"second","ttl":3}`)
	if boolOf(t, got, "ok") {
		t.Fatal("同 PID 不同 token 的注册必须被拒（防 PID 复用幽灵占用）")
	}
	if got["reason"] != "taken" {
		t.Fatalf("拒绝原因应为 taken，实际 %#v", got["reason"])
	}
	// 原占用仍在，且原 token 的心跳依旧有效。
	if pids := occupantsOf(t, c); len(pids) != 1 {
		t.Fatalf("被拒的注册不得覆盖原占用，实际 %v", pids)
	}
	if hb := postHandler(t, c.handleHeartbeat, `{"pid":99,"token":"first"}`); !boolOf(t, hb, "ok") {
		t.Fatalf("原 token 心跳应继续有效: %#v", hb)
	}
}

// 同 PID 同 token 重复注册是幂等的（看门狗重握手会无脑重发）。
func TestCoordRegisterIsIdempotent(t *testing.T) {
	c := New(Options{TTL: time.Second})
	for i := 0; i < 2; i++ {
		if got := postHandler(t, c.handleRegister, `{"pid":11,"token":"same","ttl":3}`); !boolOf(t, got, "ok") {
			t.Fatalf("第 %d 次相同注册应成功: %#v", i+1, got)
		}
	}
	if pids := occupantsOf(t, c); len(pids) != 1 {
		t.Fatalf("重复注册不应产生重复条目: %v", pids)
	}
}

// 心跳/注销的 token 不匹配必须被拒；且错误 token 的注销不得删除他人占用（§12.6）。
func TestCoordTokenMismatchGuards(t *testing.T) {
	c := New(Options{TTL: time.Second})
	_ = postHandler(t, c.handleRegister, `{"pid":55,"token":"right","ttl":3}`)

	if got := postHandler(t, c.handleHeartbeat, `{"pid":55,"token":"wrong"}`); boolOf(t, got, "ok") {
		t.Fatal("错误 token 的心跳必须被拒")
	}
	got := postHandler(t, c.handleDeregister, `{"pid":55,"token":"wrong"}`)
	if boolOf(t, got, "ok") {
		t.Fatal("错误 token 的注销必须被拒")
	}
	if pids := occupantsOf(t, c); len(pids) != 1 {
		t.Fatalf("错误 token 的注销不得删除他人占用，实际 %v", pids)
	}
}

// 缺字段/坏 JSON 一律 ok:false（fail-closed），不 panic。
func TestCoordBadRequestFailClosed(t *testing.T) {
	c := New(Options{TTL: time.Second})
	for _, body := range []string{``, `{`, `{"pid":0,"token":"t"}`, `{"pid":5}`, `{"pid":5,"token":""}`} {
		got := postHandler(t, c.handleRegister, body)
		if boolOf(t, got, "ok") {
			t.Fatalf("非法注册请求必须被拒: %q", body)
		}
	}
}

// ---- §12.5 巡检 ----

// 心跳超时的占用项会被巡检剔除；剔除到空则触发 OnEmpty。
func TestCoordPollEvictsTTLExpired(t *testing.T) {
	empty, fired := oneShotEmpty()
	c := New(Options{TTL: 20 * time.Millisecond, OnEmpty: empty})
	_ = postHandler(t, c.handleRegister, `{"pid":8,"token":"t","ttl":1}`)

	c.mu.Lock()
	c.occupants[8].LastSeen = time.Now().Add(-time.Hour) // 手工把 lastSeen 推老
	c.mu.Unlock()

	if !c.poll() {
		t.Fatal("唯一占用超时被剔除后，poll 应报告「已变空并自停」")
	}
	if pids := occupantsOf(t, c); len(pids) != 0 {
		t.Fatalf("超时占用应被剔除，实际 %v", pids)
	}
	if !fired() {
		t.Fatal("列表变空应触发 OnEmpty")
	}
}

// 进程已死的占用项会被巡检剔除——即使心跳尚未超时（§12.5 的第一个条件）。
func TestCoordPollEvictsDeadProcess(t *testing.T) {
	empty, _ := oneShotEmpty()
	c := New(Options{TTL: time.Hour, OnEmpty: empty}) // TTL 很长，排除「超时」这条路径
	deadPID := 0x7FFFFFF0                             // 几乎不可能存在的 PID
	_ = postHandler(t, c.handleRegister, `{"pid":2147483632,"token":"t","ttl":3600}`)

	if processAlive(deadPID) {
		t.Skipf("PID %d 意外存活，跳过（环境异常）", deadPID)
	}
	c.mu.Lock()
	c.occupants[deadPID].LastSeen = time.Now() // 心跳新鲜，只能靠进程探测剔除
	c.mu.Unlock()

	c.poll()
	if pids := occupantsOf(t, c); len(pids) != 0 {
		t.Fatalf("已死进程的占用应被剔除，实际 %v", pids)
	}
}

// 本进程 PID 必定存活——processAlive 不能把它误判为死（否则 sidecar 会误判自停）。
func TestProcessAliveSelf(t *testing.T) {
	if !processAlive(os.Getpid()) {
		t.Fatalf("processAlive 不应把当前进程（PID %d）判为死", os.Getpid())
	}
	if processAlive(-1) || processAlive(0) {
		t.Fatal("非法 PID 应判为不存活")
	}
}

// ---- §12.4 / §12.9 握手与竞态 ----

// 端口已被占用时 Serve 返回可识别的「退让」错误（sidecar main 据此 ExitAddrInUse）。
func TestCoordServeYieldsWhenAddrInUse(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("占位监听失败: %v", err)
	}
	defer ln.Close()

	c := New(Options{})
	err = c.Serve(ln.Addr().String(), nil)
	if err == nil {
		t.Fatal("端口被占用时 Serve 应返回错误")
	}
	if !IsAddrInUse(err) {
		t.Fatalf("Serve 错误应可被 IsAddrInUse 识别，实际 %v", err)
	}
}

// 已有 sidecar 在跑 → 握手必须退让为 client，且**不得**本地拉起进程。
func TestClientReusesExistingSidecar(t *testing.T) {
	empty, _ := oneShotEmpty()
	addr, _ := serveCoord(t, empty)

	cli, err := NewClient(addr, time.Second)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	var started atomic.Int32
	sess, err := cli.RegisterOrStart(func() (Process, error) {
		started.Add(1)
		return &fakeProc{pid: os.Getpid()}, nil
	})
	if err != nil {
		t.Fatalf("RegisterOrStart: %v", err)
	}
	defer sess.Close()

	if started.Load() != 0 {
		t.Fatal("已有实例时不应本地拉起 sidecar")
	}
	if p := sess.Proc().Pid(); p != 0 {
		t.Fatalf("client 角色的 Proc 应为 no-op（Pid=0），实际 %d", p)
	}
}

// 无监听 → 本 gear 成为 owner，Proc 返回真实进程句柄；Close 会注销并回收进程。
func TestClientBecomesOwnerWhenNoListener(t *testing.T) {
	addr := freeAddr(t)
	cli, err := NewClient(addr, time.Second)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	proc := &fakeProc{pid: os.Getpid()}
	var srv *Coordinator
	sess, err := cli.RegisterOrStart(func() (Process, error) {
		srv = New(Options{CheckInterval: 50 * time.Millisecond, TTL: 2 * time.Second, OnEmpty: func() {}})
		go func() { _ = srv.Serve(addr, nil) }()
		return proc, nil
	})
	if err != nil {
		t.Fatalf("RegisterOrStart: %v", err)
	}

	if !isOwner(sess) {
		t.Fatal("无人监听时应成为 owner")
	}
	if p := sess.Proc().Pid(); p != os.Getpid() {
		t.Fatalf("owner 的 Proc 应返回真实进程（Pid=%d），实际 %d", os.Getpid(), p)
	}
	if pids := occupantsOf(t, srv); len(pids) != 1 || pids[0] != os.Getpid() {
		t.Fatalf("owner 注册后占用列表应为本进程，实际 %v", pids)
	}

	if err := sess.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if proc.killed.Load() == 0 {
		t.Fatal("owner Close 应回收自己拉起的 sidecar 进程")
	}
	if pids := occupantsOf(t, srv); len(pids) != 0 {
		t.Fatalf("Close 后应注销占用，实际 %v", pids)
	}
}

// Close 幂等：重复调用不得 panic（重复 close channel）。
func TestClientCloseIdempotent(t *testing.T) {
	addr, _ := serveCoord(t, func() {})
	cli, err := NewClient(addr, time.Second)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	sess, err := cli.RegisterOrStart(func() (Process, error) { return noopProcess{}, nil })
	if err != nil {
		t.Fatalf("RegisterOrStart: %v", err)
	}
	if err := sess.Close(); err != nil {
		t.Fatalf("首次 Close 出错: %v", err)
	}
	if err := sess.Close(); err != nil {
		t.Fatalf("重复 Close 应无害，实际: %v", err)
	}
}

// ---- 小工具 ----

func occupantsOf(t *testing.T, c *Coordinator) []int {
	t.Helper()
	body := rawBody(t, c.handleOccupants, ``)
	var out struct {
		PIDs []int `json:"pids"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("解析占用响应失败: %q (%v)", body, err)
	}
	sort.Ints(out.PIDs)
	return out.PIDs
}

func keysOf(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func isOwner(s *Session) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.owner
}

// ---- 共享 sidecar 下的角色判定 / 重握手（2026-09-23：多 app 共享单侧车不再被误判为崩溃）----

// IsOwner 必须如实反映角色：client 退让时 false（本 gear 手上没有本地进程），owner 时 true。
// 监管器据此决定「等不等这个进程」——判错就会把共享成功当成 service 退出并无限重启。
func TestSessionIsOwnerMatchesRole(t *testing.T) {
	empty, _ := oneShotEmpty()
	addr, _ := serveCoord(t, empty)

	cli, err := NewClient(addr, time.Second)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	sess, err := cli.RegisterOrStart(func() (Process, error) { return &fakeProc{pid: os.Getpid()}, nil })
	if err != nil {
		t.Fatalf("RegisterOrStart: %v", err)
	}
	defer sess.Close()
	if sess.IsOwner() {
		t.Fatal("已有实例时应退让为 client（IsOwner=false）")
	}
	if isOwner(sess) != sess.IsOwner() {
		t.Fatal("IsOwner 与内部 owner 字段不一致")
	}

	// 对照：无监听 → 本 gear 成为 owner。
	addr2 := freeAddr(t)
	cli2, err := NewClient(addr2, time.Second)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	var srv *Coordinator
	sess2, err := cli2.RegisterOrStart(func() (Process, error) {
		srv = New(Options{CheckInterval: 50 * time.Millisecond, TTL: 2 * time.Second, OnEmpty: func() {}})
		go func() { _ = srv.Serve(addr2, nil) }()
		return &fakeProc{pid: os.Getpid()}, nil
	})
	if err != nil {
		t.Fatalf("RegisterOrStart: %v", err)
	}
	defer sess2.Close()
	if !sess2.IsOwner() {
		t.Fatal("无人监听时应成为 owner（IsOwner=true）")
	}
}

// 本地 sidecar 仍存活时，重握手不得降级为 client、更不得误杀它——
// 否则会把一个健康的共享实例打掉，影响其它已注册的 gear。
func TestSessionRehandshakeKeepsOwnerWhenLocalSidecarAlive(t *testing.T) {
	addr := freeAddr(t)
	cli, err := NewClient(addr, time.Second)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	proc := &fakeProc{pid: os.Getpid()} // 本进程必然存活 → processAlive 为真
	var srv *Coordinator
	sess, err := cli.RegisterOrStart(func() (Process, error) {
		srv = New(Options{CheckInterval: 50 * time.Millisecond, TTL: 2 * time.Second, OnEmpty: func() {}})
		go func() { _ = srv.Serve(addr, nil) }()
		return proc, nil
	})
	if err != nil {
		t.Fatalf("RegisterOrStart: %v", err)
	}
	defer sess.Close()
	_ = srv

	if err := sess.Rehandshake(); err != nil {
		t.Fatalf("本地进程存活时重握手不应报错: %v", err)
	}
	if !sess.IsOwner() {
		t.Fatal("本地 sidecar 仍存活，重握手后必须保留 owner 角色")
	}
	if sess.Proc().Pid() != proc.Pid() {
		t.Fatalf("不应换掉存活的本地进程句柄: 期望 pid=%d，实际 %d", proc.Pid(), sess.Proc().Pid())
	}
	if proc.killed.Load() != 0 {
		t.Fatal("不得误杀存活的本地 sidecar")
	}
}

// 重握手失败（无人监听且拉不起 sidecar）必须把错误交回调用方，
// 由监管层决定退避重试——不能静默当成「重启成功」。
func TestSessionRehandshakePropagatesError(t *testing.T) {
	cli, err := NewClient(freeAddr(t), time.Second) // 无人监听
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	// 直接构造会话（不起看门狗）：start 恒失败，模拟 sidecar 拉不起来（二进制缺失等）。
	sess := &Session{
		cli:   cli,
		pid:   os.Getpid(),
		owner: false,
		proc:  noopProcess{},
		stop:  make(chan struct{}),
		start: func() (Process, error) { return nil, errors.New("sidecar 启动失败") },
	}
	if err := sess.Rehandshake(); err == nil {
		t.Fatal("重握手失败必须返回 error（监管层据此退避重试，不能当成重启成功）")
	}
	if sess.IsOwner() {
		t.Fatal("拉不起 sidecar 却成了 owner：状态与事实不符")
	}
}
