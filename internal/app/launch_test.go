package app

// launch_test.go：后台启动器与台账的用例。
//
// 这一层是整个「MCP 能创建并运行应用」的最后一步，也是最容易骗过自己的地方：
// 「发了信号」不等于「收干净了」、「派生了进程」不等于「起来了」。所以这里每个用例都
// 尽量钉住**可观察的事实**（台账文件、pid、端口、日志），而不是「函数没报错」。
//
// 关于「真派生进程」：测试里把 Self 指向**测试二进制自身**。它认不得
// `--registry … app run …` 这组参数，于是会在解析参数时立刻退出——这恰好是我们想要的
// 确定性行为：既真实走完 detach + 重定向日志的路径，又不会留下长驻进程。

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTempAppsDir 造一个独立的台账目录。
func newTempAppsDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "apps")
}

// writeRecord 落一条台账（直接写，不经 Start——用于构造「已存在」的各种形态）。
func writeRecord(t *testing.T, dir string, r *Record) {
	t.Helper()
	if r.AppID == "" {
		r.AppID = "app_test"
	}
	if r.StartedAt.IsZero() {
		r.StartedAt = time.Now()
	}
	if err := SaveRecord(dir, r); err != nil {
		t.Fatalf("写台账失败: %v", err)
	}
}

// heldPort 占住一个端口并返回端口号（调用方负责 Close）。
func heldPort(t *testing.T) (int, net.Listener) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("占端口失败: %v", err)
	}
	return ln.Addr().(*net.TCPAddr).Port, ln
}

// freePort 取一个当前空闲的端口（不占住）。
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("取空闲端口失败: %v", err)
	}
	p := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return p
}

// ---- 台账 ----

func TestNewRecordIDSanitizesAndSorts(t *testing.T) {
	// 非法字符替换成 _：app_id 会当文件名用，绝不能带空格 / 分隔符。
	if got := NewRecordID("my app/up", time.Date(2026, 9, 21, 8, 5, 9, 0, time.UTC)); got != "app_my_app_up-20260921T080509" {
		t.Fatalf("NewRecordID 规范化不符: %s", got)
	}
	// 全非法 → 兜底 "app"（绝不产出空 id）。
	if got := NewRecordID("中文", time.Date(2026, 9, 21, 8, 5, 9, 0, time.UTC)); got != "app___-20260921T080509" {
		t.Fatalf("全非法字符应逐字换成 _: %s", got)
	}
	if got := NewRecordID("", time.Now()); !strings.HasPrefix(got, "app_app-") {
		t.Fatalf("空 flow id 应兜底成 app: %s", got)
	}
}

func TestSaveLoadListRecords(t *testing.T) {
	dir := newTempAppsDir(t)

	// 目录不存在时「暂无应用」而不是错误。
	recs, err := ListRecords(dir)
	if err != nil || len(recs) != 0 {
		t.Fatalf("目录不存在应返回空列表: %v %v", recs, err)
	}

	later := time.Now().Add(time.Minute)
	writeRecord(t, dir, &Record{AppID: "app_b", FlowID: "b.up", PID: 1, StartedAt: later})
	writeRecord(t, dir, &Record{AppID: "app_a", FlowID: "a.up", PID: 2})

	// 空目录写入后立即读回：字段完整（原子写不能丢数据）。
	back, err := LoadRecord(dir, "app_a")
	if err != nil || back.FlowID != "a.up" || back.PID != 2 {
		t.Fatalf("读回台账不符: %+v %v", back, err)
	}

	// 列表按 StartedAt 升序（便于稳定断言与阅读）。
	recs, err = ListRecords(dir)
	if err != nil || len(recs) != 2 {
		t.Fatalf("应列出 2 条: %d %v", len(recs), err)
	}
	if recs[0].AppID != "app_a" || recs[1].AppID != "app_b" {
		t.Fatalf("列表应按 StartedAt 升序: %s, %s", recs[0].AppID, recs[1].AppID)
	}

	// 不存在的记录：错误必须可被 errors.Is(err, os.ErrNotExist) 判别（调用方靠它报 APP_NOT_FOUND）。
	if _, err := LoadRecord(dir, "app_nope"); !os.IsNotExist(err) {
		t.Fatalf("不存在的台账应返回 os.ErrNotExist: %v", err)
	}

	// 损坏文件只跳过它自己，不让一条坏记录遮蔽其余。
	if err := os.WriteFile(filepath.Join(dir, "app_broken.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	recs, err = ListRecords(dir)
	if err != nil || len(recs) != 2 {
		t.Fatalf("坏记录应被跳过而不是让整次列表失败: %d %v", len(recs), err)
	}
}

func TestUniqueAppIDAppendsSuffix(t *testing.T) {
	dir := newTempAppsDir(t)
	base := "app_x-20260921T000000"
	// 未占用 → 原样返回。
	if got := uniqueAppID(dir, base); got != base {
		t.Fatalf("未占用时应原样返回: %s", got)
	}
	// 占用 → 依次尝试 -2 / -3（app_id 时间戳只到秒，同一秒内重启会撞名）。
	writeRecord(t, dir, &Record{AppID: base, FlowID: "x"})
	if got := uniqueAppID(dir, base); got != base+"-2" {
		t.Fatalf("重名应加 -2 后缀: %s", got)
	}
	writeRecord(t, dir, &Record{AppID: base + "-2", FlowID: "x"})
	if got := uniqueAppID(dir, base); got != base+"-3" {
		t.Fatalf("二次重名应加 -3 后缀: %s", got)
	}
}

// ---- 双判据探活 ----

func TestProbeUsesDualCriterion(t *testing.T) {
	live := os.Getpid()

	// 未声明端口 → 退化为 pid 单判据（否则「没写 ports 的应用」永远判不出来）。
	if got := (&Record{PID: live}).Probe(); got != StateRunning {
		t.Fatalf("pid 存活应判 running: %s", got)
	}
	if got := (&Record{PID: 0}).Probe(); got != StateExited {
		t.Fatalf("pid 无效应判 exited: %s", got)
	}

	// 声明端口且两判据一致 → running / exited。
	okPort := freePort(t)
	if got := (&Record{PID: live, Ports: []int{okPort}}).Probe(); got != StateUnknown {
		// pid 在、端口没听 = 矛盾 → 不猜也不杀
		t.Fatalf("pid 在但端口没听应判 unknown: %s", got)
	}
	if got := (&Record{PID: 0, Ports: []int{okPort}}).Probe(); got != StateExited {
		t.Fatalf("pid 不在且端口空闲应判 exited: %s", got)
	}

	// 端口在听但 pid 已死（残留孙进程攥着端口）→ unknown（绝不自动接管）。
	live2, ln2 := heldPort(t)
	defer ln2.Close()
	if got := (&Record{PID: 0, Ports: []int{live2}}).Probe(); got != StateUnknown {
		t.Fatalf("端口在听但 pid 已死应判 unknown: %s", got)
	}
}

func TestPortListening(t *testing.T) {
	p, ln := heldPort(t)
	if !PortListening(p) {
		t.Fatal("占住的端口应探测为在听")
	}
	_ = ln.Close()
	if PortListening(0) || PortListening(-1) {
		t.Fatal("非法端口必须直接判 false（不把 0 当成有效探测）")
	}
}

func TestOrphansReportsWithoutJudging(t *testing.T) {
	heldP, ln := heldPort(t)
	defer ln.Close()
	freeP := freePort(t)
	stopped := time.Now()

	recs := []*Record{
		{AppID: "app_running", PID: os.Getpid()},
		{AppID: "app_gone", PID: 0},
		{AppID: "app_port_left", PID: 0, Ports: []int{heldP}},
		{AppID: "app_stopped", PID: 0, StoppedAt: &stopped},
		{AppID: "app_quiet", PID: 0, Ports: []int{freeP}},
	}
	got := map[string]string{}
	for _, o := range Orphans(recs) {
		got[o.AppID] = o.Reason
	}
	if _, ok := got["app_running"]; ok {
		t.Fatal("存活进程不是遗留项")
	}
	if got["app_gone"] != "pid-gone" {
		t.Fatalf("进程悄悄死掉应报 pid-gone: %v", got)
	}
	if got["app_port_left"] != "port-still-listening" {
		t.Fatalf("端口仍被占应报 port-still-listening: %v", got)
	}
	if _, ok := got["app_stopped"]; ok {
		t.Fatal("已显式 stop 的记录是历史，不算遗留项")
	}
	// 关键语义：判据是「从未被 stop 就没了」，而不是「端口还占着」。端口已释放也一样要报——
	// 遗留项是**要人过目**的清单，不是「出事了」的清单（只汇报、不静默接管）。
	if got["app_quiet"] != "pid-gone" {
		t.Fatalf("pid 消失且从未 stop 应报 pid-gone（哪怕端口已释放）: %v", got)
	}
}

// ---- 启动 ----

func TestStartRejectsMissingOrInvalidOptions(t *testing.T) {
	cases := []struct {
		name string
		opts StartOptions
		want string
	}{
		{"缺 FlowID", StartOptions{RegistryPath: "x.db"}, "FlowID 必填"},
		{"缺 RegistryPath", StartOptions{FlowID: "a.up"}, "RegistryPath 必填"},
		{"非法 restart", StartOptions{FlowID: "a.up", RegistryPath: "x.db", Restart: "sometimes"}, "非法的 restart"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := Start(c.opts); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("应报 %q，实际 %v", c.want, err)
			}
		})
	}
}

// TestWaitReadyTreatsExitAsFailureEvenIfPortHeld 是 waitReady 的回归锁。
//
// 场景：派生进程**已经退出**，但台账声明的端口仍有人监听——且那个监听**不是它**。
// 真实成因：同端口已有常驻实例时引擎复用其监听，于是 app 的 service 步立即完成、
// 进程退出，而端口继续被那个常驻实例服务着（2026-09-21 在 plugin/verify.py 的 P4
// 场景实测到：served=True 却来自别人的侧车）。
//
// 为什么必须有这条锁：若判据沿用 Record.Probe()，此处得到的是 unknown（「pid 已死 +
// 端口在听」的双判据矛盾），而 unknown 不是 waitReady 那个 switch 的任何分支，于是它
// 会一路等到窗口超时并报 starting —— 调用方（智能体）据此以为「还在启动」，其实进程
// 早已不在。既有的 TestStartSpawnsDetachedAndRecords 用**空闲**端口，覆盖不到这条路径
// （端口没人占时 Probe 恰好给 exited，能收敛）。
func TestWaitReadyTreatsExitAsFailureEvenIfPortHeld(t *testing.T) {
	dir := newTempAppsDir(t)
	port, ln := heldPort(t)
	defer ln.Close()

	res, err := Start(StartOptions{
		RegistryPath: filepath.Join(t.TempDir(), "registry.db"),
		AppsDir:      dir,
		FlowID:       "held.up",
		Restart:      string(RestartNever),
		Ports:        []int{port},
		WaitSeconds:  3,
		Self:         os.Args[0], // 测试二进制：认不得 app run 的参数，立刻退出
	})
	if err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	if res.State != string(StateStartupFailed) {
		t.Fatalf("进程已退出时必须判 startup_failed，而不是等到窗口超时报 starting: %+v", res)
	}
	if len(res.Ports) != 1 || res.Ports[0].Port != port || !res.Ports[0].Listening {
		t.Fatalf("应如实报「端口在听，但那是别人的监听」: %+v", res.Ports)
	}
}

func TestStartSpawnsDetachedAndRecords(t *testing.T) {
	dir := newTempAppsDir(t)
	port := freePort(t)

	// Self 指向测试二进制自身：它会因认不得 app run 的参数而立刻退出——
	// 正好覆盖「派生成功但进程起不来」这条最需要如实汇报的路径。
	res, err := Start(StartOptions{
		RegistryPath: filepath.Join(t.TempDir(), "registry.db"),
		AppsDir:      dir,
		FlowID:       "spawn.up",
		Restart:      string(RestartNever),
		Ports:        []int{port},
		WaitSeconds:  5,
		Self:         os.Args[0],
	})
	if err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	if res.PID <= 0 {
		t.Fatalf("应返回真实 pid: %+v", res)
	}
	if res.State != string(StateStartupFailed) {
		t.Fatalf("进程在短等窗口内退出时应如实报 startup_failed: %+v", res)
	}
	if len(res.LastLogs) == 0 {
		t.Fatalf("startup_failed 必须附日志尾部（否则无从排障）: %+v", res)
	}
	if len(res.Ports) != 1 || res.Ports[0].Port != port || res.Ports[0].Listening {
		t.Fatalf("应返回声明端口的探活结果: %+v", res.Ports)
	}

	// 台账必须落盘，且内容与返回一致（发现机制不能只在内存里）。
	rec, err := LoadRecord(dir, res.AppID)
	if err != nil {
		t.Fatalf("台账应已落盘: %v", err)
	}
	if rec.PID != res.PID || rec.FlowID != "spawn.up" || rec.LogPath != res.LogPath {
		t.Fatalf("台账与返回不一致: %+v vs %+v", rec, res)
	}
	if _, err := os.Stat(res.LogPath); err != nil {
		t.Fatalf("子进程输出应重定向到 %s: %v", res.LogPath, err)
	}

	// 启动失败 = **已结案**：我们确知它不在跑，就不该让它永久留在遗留清单里
	// （否则每次启动失败都堆一条噪声，真正需要人过目的条目会被淹掉）。
	// 但结案原因要留住——status 之后仍能报出 startup_failed 而不是笼统的 stopped。
	if rec.StoppedAt == nil || rec.FinalState != string(StateStartupFailed) {
		t.Fatalf("启动失败应结案并记录原因: %+v", rec)
	}
	list, err := List("", dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Orphan) != 0 {
		t.Fatalf("已结案的启动失败不该算遗留项: %+v", list.Orphan)
	}
	st, err := Status(StatusOptions{AppsDir: dir, AppID: res.AppID})
	if err != nil {
		t.Fatal(err)
	}
	if st.State != string(StateStartupFailed) {
		t.Fatalf("结案原因应被 status 保留（不是笼统的 stopped）: %+v", st)
	}
}

func TestStartIsIdempotentForRunningFlow(t *testing.T) {
	dir := newTempAppsDir(t)
	// 用**本进程 pid** 造一条「活跃实例」：探活必然 running。
	// 注意：绝不能对这个 app_id 调 Stop（那会杀掉测试自己）。
	writeRecord(t, dir, &Record{AppID: "app_live", FlowID: "live.up", PID: os.Getpid()})

	res, err := Start(StartOptions{
		RegistryPath: filepath.Join(t.TempDir(), "registry.db"),
		AppsDir:      dir,
		FlowID:       "live.up",
		Self:         os.Args[0],
	})
	if err != nil {
		t.Fatalf("Start 应幂等返回而不是报错: %v", err)
	}
	if res.State != string(StateAlreadyRunning) || res.AppID != "app_live" {
		t.Fatalf("同 Flow 已有活跃实例时应返回 already_running: %+v", res)
	}
	if res.PID != os.Getpid() {
		t.Fatalf("应沿用已有实例的 pid（不重复拉起）: %+v", res)
	}

	// 台账里仍只有这一条（没有因为重复 start 而多出记录）。
	recs, _ := ListRecords(dir)
	if len(recs) != 1 {
		t.Fatalf("幂等返回不得新增台账: %d", len(recs))
	}
}

// TestStartReplaceLeavesStaleRecordAlone Replace **只对活跃旧实例生效**。
//
// 一条 pid 早已消失、却从未被 stop 的记录是「要人过目的遗留项」：它可能意味着某个孙进程
// 还攥着端口。所以 Replace 既不去 Stop 它（没什么可停的），也不替它写 StoppedAt
// （那会把「端口仍被占」这个信号抹掉）。新实例照常拉起，两条记录并存。
func TestStartReplaceLeavesStaleRecordAlone(t *testing.T) {
	dir := newTempAppsDir(t)
	writeRecord(t, dir, &Record{AppID: "app_old", FlowID: "rep.up", PID: 0})

	res, err := Start(StartOptions{
		RegistryPath: filepath.Join(t.TempDir(), "registry.db"),
		AppsDir:      dir,
		FlowID:       "rep.up",
		Replace:      true,
		WaitSeconds:  3,
		Self:         os.Args[0],
	})
	if err != nil {
		t.Fatalf("带 Replace 的 Start 失败: %v", err)
	}
	if res.AppID == "app_old" {
		t.Fatalf("Replace 应产生新实例而不是复用已死的旧记录: %+v", res)
	}
	old, err := LoadRecord(dir, "app_old")
	if err != nil {
		t.Fatal(err)
	}
	if old.StoppedAt != nil {
		t.Fatal("遗留记录不得被静默结案（会抹掉「端口可能仍被占」的信号）")
	}
	// 两条并存：新实例 + 待过目的旧记录。
	recs, _ := ListRecords(dir)
	if len(recs) != 2 {
		t.Fatalf("应同时保留新实例与旧记录: %d", len(recs))
	}
	// 新实例因进程立刻退出已被结案 → 不污染遗留清单；旧记录仍如实报出。
	list, err := List("", dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Orphan) != 1 || list.Orphan[0].AppID != "app_old" {
		t.Fatalf("遗留清单应只剩旧记录: %+v", list.Orphan)
	}
}

// ---- 停止 ----

func TestStopUnknownApp(t *testing.T) {
	dir := newTempAppsDir(t)
	if _, err := Stop(StopOptions{AppsDir: dir, AppID: "app_nope"}); err == nil ||
		!strings.Contains(err.Error(), "应用不存在") {
		t.Fatalf("停止不存在的应用必须报错（不静默成功）: %v", err)
	}
	if _, err := Stop(StopOptions{AppsDir: dir}); err == nil {
		t.Fatal("缺 AppID 必须报错")
	}
}

func TestStopMarksStoppedAndReleasesPorts(t *testing.T) {
	dir := newTempAppsDir(t)
	port := freePort(t)
	writeRecord(t, dir, &Record{AppID: "app_dead", FlowID: "dead.up", PID: 0, Ports: []int{port}})

	res, err := Stop(StopOptions{AppsDir: dir, AppID: "app_dead"})
	if err != nil {
		t.Fatalf("Stop 失败: %v", err)
	}
	if res.Code != "" || res.State != string(StateStopped) {
		t.Fatalf("进程已退且端口空闲时应报 stopped: %+v", res)
	}
	if !res.PortsReleased {
		t.Fatalf("应确认端口已释放: %+v", res)
	}
	// 台账被标记为已停（供 list/orphan 把它当历史）。
	rec, err := LoadRecord(dir, "app_dead")
	if err != nil || rec.StoppedAt == nil {
		t.Fatalf("stop 应写回 StoppedAt: %+v %v", rec, err)
	}
}

func TestStopReportsOrphanWhenPortHeld(t *testing.T) {
	dir := newTempAppsDir(t)
	port, ln := heldPort(t)
	defer ln.Close()
	writeRecord(t, dir, &Record{AppID: "app_leak", FlowID: "leak.up", PID: 0, Ports: []int{port}})

	res, err := Stop(StopOptions{AppsDir: dir, AppID: "app_leak"})
	if err != nil {
		t.Fatalf("Stop 失败: %v", err)
	}
	if res.Code != "ORPHAN_APP" {
		t.Fatalf("端口仍被占时必须报 ORPHAN_APP（绝不谎报 stopped）: %+v", res)
	}
	if res.PortsReleased || res.State == string(StateStopped) {
		t.Fatalf("未收干净时不得说 stopped / ports_released: %+v", res)
	}
	if !strings.Contains(res.Hint, "gear_doctor") {
		t.Fatalf("ORPHAN_APP 的提示应指向 gear_doctor: %s", res.Hint)
	}
	// 未收干净 → **不写** StoppedAt（否则它就被当成历史，问题被抹掉）。
	rec, _ := LoadRecord(dir, "app_leak")
	if rec.StoppedAt != nil {
		t.Fatal("没收干净的实例不得标记为已停")
	}
}

// ---- 状态与列表 ----

func TestStatusAndList(t *testing.T) {
	dir := newTempAppsDir(t)
	logPath := filepath.Join(dir, "app_run.log")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	lines := []string{}
	for i := 1; i <= 5; i++ {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	if err := os.WriteFile(logPath, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	// pid = 本进程 → running，从而覆盖 uptime 分支。
	writeRecord(t, dir, &Record{AppID: "app_run", FlowID: "run.up", PID: os.Getpid(), LogPath: logPath})

	st, err := Status(StatusOptions{AppsDir: dir, AppID: "app_run", TailLogs: 2})
	if err != nil {
		t.Fatalf("Status 失败: %v", err)
	}
	if st.State != string(StateRunning) || st.UptimeS < 0 {
		t.Fatalf("存活实例应报 running 且带 uptime: %+v", st)
	}
	if len(st.LastLogs) != 2 || st.LastLogs[1] != "line 5" {
		t.Fatalf("TailLogs 应只给末 N 行: %+v", st.LastLogs)
	}

	if _, err := Status(StatusOptions{AppsDir: dir, AppID: "app_nope"}); err == nil ||
		!strings.Contains(err.Error(), "应用不存在") {
		t.Fatalf("查询不存在的应用必须报错: %v", err)
	}

	// List：item 齐全 + 遗留项如实汇总。
	writeRecord(t, dir, &Record{AppID: "app_lost", FlowID: "lost.up", PID: 0})
	list, err := List("", dir)
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	if len(list.Items) != 2 {
		t.Fatalf("List 应含 2 条: %+v", list.Items)
	}
	if len(list.Orphan) != 1 || list.Orphan[0].AppID != "app_lost" {
		t.Fatalf("List 应报出 1 条遗留项: %+v", list.Orphan)
	}
}

// ---- 目录解析与日志尾巴 ----

func TestResolveAppsDirPrefersExplicit(t *testing.T) {
	if _, err := ResolveAppsDir("", ""); err == nil {
		t.Fatal("两个都为空时必须报错（不能默默用相对路径 .）")
	}
	if got, _ := ResolveAppsDir("", "explicit"); got != "explicit" {
		t.Fatalf("显式 AppsDir 优先: %s", got)
	}
	got, err := ResolveAppsDir(filepath.Join("root", "registry.db"), "")
	if err != nil || got != filepath.Join("root", "apps") {
		t.Fatalf("默认应为 <registry 同目录>/apps: %s %v", got, err)
	}
}

func TestTailLogsTolerant(t *testing.T) {
	dir := t.TempDir()
	// 读不到日志不是错误（应用可能还没输出任何东西）。
	if got := tailLogs(filepath.Join(dir, "nope.log"), 5); got != nil {
		t.Fatalf("读不到日志应返回 nil: %v", got)
	}
	// 空文件同理。
	empty := filepath.Join(dir, "empty.log")
	if err := os.WriteFile(empty, []byte("\n\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := tailLogs(empty, 5); got != nil {
		t.Fatalf("空日志应返回 nil: %v", got)
	}
	full := filepath.Join(dir, "full.log")
	if err := os.WriteFile(full, []byte("a\nb\nc\n\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := tailLogs(full, 2)
	if len(got) != 2 || got[0] != "b" || got[1] != "c" {
		t.Fatalf("应取末 2 行且忽略尾部空行: %v", got)
	}
	if all := tailLogs(full, 99); len(all) != 3 {
		t.Fatalf("n 大于行数时应全给: %v", all)
	}
}
