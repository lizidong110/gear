package app

// ready_test.go：就绪判定（waitReady）与「派生是否真的脱离了调用方沙箱」的回归锁。
//
// 这两件事都属于「CLI/智能体据此决定下一步」的结论，所以判错的代价不是「日志难看」，
// 而是**行动方向错了**：把「派生即崩」报成 running，智能体会去写业务代码而不再看日志；
// 把「未脱离 Job」报成「跨会话存活」，用户会在会话结束后才发现进程没了。

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestWaitReadyWaitsFullWindowWhenNoPortsDeclared 锁住「未声明端口时不抢答 running」。
//
// 语义：未声明端口 ⇒ 结构上不存在就绪信号，能给出的最强结论只有「它在窗口内一直活着」。
// 因此必须把窗口等满，而不是在第 0 毫秒看到 pid 活着就宣布 running。
//
// 用**当前测试进程自己的 pid** 当被测对象：它必然全程存活，于是「等满窗口」这条性质
// 可以被确定性地验到（不需要任何 sleep 竞态）。
func TestWaitReadyWaitsFullWindowWhenNoPortsDeclared(t *testing.T) {
	rec := &Record{
		AppID:   "self",
		PID:     os.Getpid(), // 我们自己：必然全程存活
		LogPath: filepath.Join(t.TempDir(), "self.log"),
	}
	const window = 1 // 秒
	start := time.Now()
	state, ports, _ := waitReady(rec, window, nil)
	elapsed := time.Since(start)

	if state != string(StateRunning) {
		t.Fatalf("全程存活的进程在窗口结束后应判 running，实际 %s", state)
	}
	if len(ports) != 0 {
		t.Fatalf("未声明端口时不应凭空造出端口结论，实际 %v", ports)
	}
	// 下界取窗口的一半：契约是「等满窗口」，不是「等一个大致的时间」。
	// 用下界而非上界比较（方法论红线 13：负载只会让机器更慢，所以只对慢的方向做绝对断言）。
	if elapsed < window*time.Second/2 {
		t.Fatalf("未声明端口时必须观察到窗口结束才下结论，实际只用了 %v（窗口 %ds）", elapsed, window)
	}
}

// TestWaitReadyReportsFailureForProcessDyingInsideWindow 是这次修正的**负向锁**。
//
// 复现的真实形态：应用进程在派生后活了很短一段时间（起侧车、跑 step），随后因 Flow
// 失败而退出。旧实现（`len(ports)==0` 直接 return running）会把这种情况报成 running
// ——2026-09-21 在 WorkBuddy 里连中两次，智能体据此认为应用已就绪。
//
// 为什么必须用「起一个会自己退出的子进程」而不是别的构造：只有真实的「先活后死」才能
// 区分「抢答」与「观察」。若子进程在第一次探测前就已退出，两条实现都会给 startup_failed，
// 这条锁就空转了（判据自检的思路，见 tools/negcheck.py 的纪律）。
func TestWaitReadyReportsFailureForProcessDyingInsideWindow(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=TestAppShortLivedHelper")
	cmd.Env = append(os.Environ(), "GEAR_APP_SHORTLIVED=1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("起辅助子进程失败: %v", err)
	}
	pid := cmd.Process.Pid
	// 保持与生产一致的一步：释放我们持有的进程句柄。
	//
	// 2026-09-28 注：这一步**曾经**是本用例能否成立的必要条件——那时 processAlive 在 Windows
	// 上只看「句柄打不打得开」，而进程退出后只要还有句柄指向它，内核就不销毁进程对象，
	// 于是 pid 探测依旧返回「存活」，本例会看到子进程永远活着（实测踩过）。
	// 判据修好后（改走 OpenProcess + GetExitCodeProcess，见 internal/procalive）已退出即判死，
	// 释放句柄不再是判别力的前提；仍然保留它，是因为生产路径同样这么干
	// （launchDetached 派生后立即 Release，它只是启动器，不是持有者）。
	_ = cmd.Process.Release()

	// 日志尾部要能取到东西：startup_failed 必须附病因（否则调用方只知道「没起来」）。
	logPath := filepath.Join(t.TempDir(), "short.log")
	if err := os.WriteFile(logPath, []byte("探针：这是子进程的日志尾部\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	rec := &Record{AppID: "short", PID: pid, LogPath: logPath}
	state, _, logs := waitReady(rec, 5, nil)
	if state != string(StateStartupFailed) {
		t.Fatalf("进程在窗口内退出时必须报 startup_failed，实际 %s（抢答 running 会让智能体不再看日志）", state)
	}
	if len(logs) == 0 {
		t.Fatalf("startup_failed 必须附日志尾部，实际为空")
	}
}

// TestAppShortLivedHelper 不是被测对象，而是上面那条用例的「应用进程」替身：
// 活 250ms 再退出，精确复刻「起得来一小会儿、然后失败退出」的时间形状。
func TestAppShortLivedHelper(t *testing.T) {
	if os.Getenv("GEAR_APP_SHORTLIVED") != "1" {
		return
	}
	time.Sleep(250 * time.Millisecond)
	os.Exit(0)
}

// TestStartReportsEscapedJob 锁住「派生是否脱离调用方沙箱」必须被如实上报。
//
// 为什么值得一条锁：`CREATE_BREAKAWAY_FROM_JOB` 会失败并退化为普通派生，此时进程仍在
// 调用方的 Job 里，**发起本次启动的会话一结束它就被连坐回收**——「跨会话存活」并不成立。
// 此前这条退化是静默的，于是「应用被我这条命令杀掉了」会被归因成「应用自己崩了」。
//
// 判据刻意不写死 true/false（那取决于当前机器/会话是否允许 breakaway），而是钉住
// **两者的对应关系**：报未脱离就必须给 warning，报已脱离就必须不给——这样在任何环境下
// 都能验，且一旦有人把 warning 去掉、或把 bool 反过来，用例立刻红。
func TestStartReportsEscapedJob(t *testing.T) {
	dir := newTempAppsDir(t)
	res, err := Start(StartOptions{
		RegistryPath: filepath.Join(t.TempDir(), "registry.db"),
		AppsDir:      dir,
		FlowID:       "escaped.up",
		Restart:      string(RestartNever),
		Ports:        []int{freePort(t)},
		WaitSeconds:  3,
		Self:         os.Args[0], // 认不得 app run 的参数，立即退出
	})
	if err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	if res.EscapedJob == nil {
		t.Fatal("start 结果必须带上「是否脱离调用方 Job」——静默意味着无法事后归因")
	}
	if *res.EscapedJob && res.Warning != "" {
		t.Fatalf("已脱离沙箱却给了 warning：%q", res.Warning)
	}
	if !*res.EscapedJob && res.Warning == "" {
		t.Fatal("未脱离沙箱（进程会随调用方会话被回收）却没有任何提示——这正是要堵的误导")
	}

	// 台账要落盘同一事实：进程死因必须能**事后**从磁盘上归因，不能只在当次返回值里。
	rec, err := LoadRecord(dir, res.AppID)
	if err != nil {
		t.Fatalf("台账应已落盘: %v", err)
	}
	if rec.EscapedJob == nil || *rec.EscapedJob != *res.EscapedJob {
		t.Fatalf("台账里的 escaped_job 应与返回值一致: 台账=%v 返回=%v", rec.EscapedJob, res.EscapedJob)
	}

	// status 也必须带上它：`app status` 是事后排障的入口。
	st, err := Status(StatusOptions{AppsDir: dir, AppID: res.AppID})
	if err != nil {
		t.Fatalf("Status 失败: %v", err)
	}
	if st.EscapedJob == nil || *st.EscapedJob != *res.EscapedJob {
		t.Fatalf("status 应透出 escaped_job: %v", st.EscapedJob)
	}
}
