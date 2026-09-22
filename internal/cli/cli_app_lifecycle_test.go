package cli

// cli_app_lifecycle_test.go：`gear app start/stop/status/list` 四条后台生命周期命令的用例。
//
// 这四条命令与 MCP 面的 gear_app 共用 internal/app 的同一份实现（CLI 只是薄壳），所以这里
// 重点不在重复验证启停逻辑，而在钉住**薄壳自身**的三件事：
//  1. flag → 选项的接线（--port / --wait / --apps-dir / --tail / --replace）；
//  2. 每条 action 的**输出与退出码语义**：already_running 不是错误、startup_failed 是错误、
//     ORPHAN_APP 必须以非 0 退出（否则脚本会以为停干净了）；
//  3. 参数守卫（少给 app-id / flow-id 必须报错，不猜）。
//
// 启动的进程是**测试二进制自身**：它认不得 `app run` 的参数而立刻退出，于是
// 「启动即失败」这条路径是确定性的——正好是脚本最需要看清楚的那条。

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"gear/internal/app"
)

// appTestDirs 造一套隔离的目录：temp registry + 独立台账目录。
func appTestDirs(t *testing.T) (appsDir string) {
	t.Helper()
	withTempRegistry(t)
	dir := filepath.Join(t.TempDir(), "apps")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// appIDsIn 读出台账里现有的 app_id（按 StartedAt 升序）。
func appIDsIn(t *testing.T, dir string) []string {
	t.Helper()
	res, err := app.List("", dir)
	if err != nil {
		t.Fatalf("读台账失败: %v", err)
	}
	out := make([]string, 0, len(res.Items))
	for _, it := range res.Items {
		out = append(out, it.AppID)
	}
	return out
}

func TestAppListEmptyPrintsPlaceholder(t *testing.T) {
	dir := appTestDirs(t)
	if err := runCLI(t, "app", "list", "--apps-dir", dir); err != nil {
		t.Fatalf("空台账时 app list 应成功退出（空不是错误）: %v", err)
	}
}

func TestAppLifecycleCommands(t *testing.T) {
	dir := appTestDirs(t)
	port := freePortFor(t)

	// ① start：派生后台进程 → 短等窗口内即退出 → 命令必须**非 0 退出**并说明原因。
	err := runCLI(t, "app", "start", "spawn.up",
		"--apps-dir", dir, "--port", strconv.Itoa(port), "--wait", "5", "--restart", "never")
	if err == nil {
		t.Fatal("派生后进程即退出时 app start 必须报错（否则脚本以为服务起来了）")
	}
	if !strings.Contains(err.Error(), "启动即退出") {
		t.Fatalf("错误信息应说清「启动即退出」并指向日志，实际 %v", err)
	}

	ids := appIDsIn(t, dir)
	if len(ids) != 1 {
		t.Fatalf("应落一条台账: %v", ids)
	}
	appID := ids[0]

	// ② status：能按 app_id 查（--tail 接线 + 结案原因被保留）。
	if err := runCLI(t, "app", "status", appID, "--apps-dir", dir, "--tail", "3"); err != nil {
		t.Fatalf("app status 失败: %v", err)
	}

	// ③ list：含 1 条记录（并覆盖 items 打印分支）。
	if err := runCLI(t, "app", "list", "--apps-dir", dir); err != nil {
		t.Fatalf("app list 失败: %v", err)
	}

	// ④ stop：收干净 → 成功退出（进程早已退出，没什么可杀，但必须确认端口已释放）。
	if err := runCLI(t, "app", "stop", appID, "--apps-dir", dir); err != nil {
		t.Fatalf("app stop 应成功: %v", err)
	}
	rec, err := app.LoadRecord(dir, appID)
	if err != nil || rec.StoppedAt == nil {
		t.Fatalf("stop 应把台账标记为已停: %+v %v", rec, err)
	}
}

func TestAppStartIsIdempotentForLiveFlow(t *testing.T) {
	dir := appTestDirs(t)
	// 造一条「活跃实例」：pid 用**本进程**，探活必然 running。
	// 注意：绝不能对这个 app_id 调 stop（会杀掉测试自己）。
	rec := &app.Record{
		AppID: "app_live", FlowID: "live.up", PID: os.Getpid(),
		StartedAt: time.Now().Add(-10 * time.Second),
	}
	if err := app.SaveRecord(dir, rec); err != nil {
		t.Fatal(err)
	}

	// already_running **不是错误**：命令成功退出（脚本里重复调用 start 是常见操作）。
	if err := runCLI(t, "app", "start", "live.up", "--apps-dir", dir); err != nil {
		t.Fatalf("同 flow 已有活跃实例时 app start 应成功返回: %v", err)
	}
	if ids := appIDsIn(t, dir); len(ids) != 1 {
		t.Fatalf("幂等返回不得新增台账: %v", ids)
	}
	// status 覆盖 running + uptime 分支。
	if err := runCLI(t, "app", "status", "app_live", "--apps-dir", dir); err != nil {
		t.Fatalf("app status(running) 失败: %v", err)
	}
}

func TestAppStopReportsOrphanAsError(t *testing.T) {
	dir := appTestDirs(t)
	port, ln := holdPortFor(t)
	defer ln.Close()
	// pid 已死但端口仍被占（残留孙进程攥着端口）→ 必须报 ORPHAN_APP 且**非 0 退出**。
	if err := app.SaveRecord(dir, &app.Record{
		AppID: "app_leak", FlowID: "leak.up", Ports: []int{port}, StartedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	err := runCLI(t, "app", "stop", "app_leak", "--apps-dir", dir)
	if err == nil || !strings.Contains(err.Error(), "ORPHAN_APP") {
		t.Fatalf("没收干净必须报 ORPHAN_APP 并非 0 退出，实际 %v", err)
	}

	// list 也要把这条遗留项打出来（覆盖 orphan 打印分支）。
	if err := runCLI(t, "app", "list", "--apps-dir", dir); err != nil {
		t.Fatalf("app list 失败: %v", err)
	}
}

func TestAppCommandsArgGuards(t *testing.T) {
	dir := appTestDirs(t)
	cases := [][]string{
		{"app", "start"},  // 缺 flow-id
		{"app", "stop"},   // 缺 app-id
		{"app", "status"}, // 缺 app-id
	}
	for _, args := range cases {
		if err := runCLI(t, append(args, "--apps-dir", dir)...); err == nil {
			t.Fatalf("%v 缺参数必须报错（不猜）", args)
		}
	}
	// 不存在的 app_id：status / stop 都必须报错。
	if err := runCLI(t, "app", "status", "app_nope", "--apps-dir", dir); err == nil {
		t.Fatal("查不存在的应用必须报错")
	}
	if err := runCLI(t, "app", "stop", "app_nope", "--apps-dir", dir); err == nil {
		t.Fatal("停不存在的应用必须报错")
	}
	// 非法 restart：拒绝而不是静默降级成 always。
	if err := runCLI(t, "app", "start", "x.up", "--apps-dir", dir, "--restart", "sometimes"); err == nil {
		t.Fatal("非法 --restart 必须报错")
	}
}

func TestAppListPrintsUptimeAndPorts(t *testing.T) {
	dir := appTestDirs(t)
	logPath := filepath.Join(dir, "app_full.log")
	if err := os.WriteFile(logPath, []byte("first\nsecond\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 一条「看起来健康的」记录：pid 在（本进程）、有端口、有日志、有 uptime →
	// 让 printAppStatus 的四个分支都走到。
	if err := app.SaveRecord(dir, &app.Record{
		AppID: "app_full", FlowID: "full.up", PID: os.Getpid(),
		Ports: []int{freePortFor(t)}, LogPath: logPath,
		StartedAt: time.Now().Add(-30 * time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	if err := runCLI(t, "app", "list", "--apps-dir", dir); err != nil {
		t.Fatalf("app list 失败: %v", err)
	}
	if err := runCLI(t, "app", "status", "app_full", "--apps-dir", dir, "--tail", "2"); err != nil {
		t.Fatalf("app status 失败: %v", err)
	}
}

// TestAppPruneCommand 锁住 `gear app prune`（2026-09-22 补的人工入口）。
//
// 缘起：prune 此前只在 MCP 面，于是「智能体能清僵尸台账、人不能清」——而台账正是人排障时
// 最想清干净的东西。本用例钉住三件事：--dry-run 不动磁盘、--flow 只清目标 flow、
// 以及**实时判据不敢确定已终止的记录绝不能被删**（otherwise 会把「还在跑却再无人可查」
// 的进程变成真孤儿，而台账存在的全部意义就是避免这个）。
func TestAppPruneCommand(t *testing.T) {
	dir := appTestDirs(t)

	// ① 确定已终止：无端口 ⇒ pid 单判据即可定案（pid=0 必判死）。
	if err := app.SaveRecord(dir, &app.Record{
		AppID: "app_dead", FlowID: "x.up", StartedAt: time.Now(),
		FinalState: string(app.StateStartupFailed),
	}); err != nil {
		t.Fatal(err)
	}
	// ② 结案了、但实时判据不敢删：端口仍被占而 pid 已死 ⇒ probe=unknown。
	port, ln := holdPortFor(t)
	defer ln.Close()
	if err := app.SaveRecord(dir, &app.Record{
		AppID: "app_ambig", FlowID: "x.up", Ports: []int{port}, StartedAt: time.Now(),
		FinalState: string(app.StateStartupFailed),
	}); err != nil {
		t.Fatal(err)
	}

	// --dry-run：只汇报，台账不动。
	if err := runCLI(t, "app", "prune", "--apps-dir", dir, "--dry-run"); err != nil {
		t.Fatalf("app prune --dry-run 失败: %v", err)
	}
	if ids := appIDsIn(t, dir); len(ids) != 2 {
		t.Fatalf("dry-run 不得动台账: %v", ids)
	}

	// --flow 过滤：不匹配的 flow 一条都不动。
	if err := runCLI(t, "app", "prune", "--apps-dir", dir, "--flow", "other.up"); err != nil {
		t.Fatalf("app prune --flow 失败: %v", err)
	}
	if ids := appIDsIn(t, dir); len(ids) != 2 {
		t.Fatalf("其它 flow 的台账不该被清: %v", ids)
	}

	// 真删：终态那条走掉，实时判据 unknown 的那条留下。
	if err := runCLI(t, "app", "prune", "--apps-dir", dir); err != nil {
		t.Fatalf("app prune 失败: %v", err)
	}
	ids := appIDsIn(t, dir)
	if len(ids) != 1 || ids[0] != "app_ambig" {
		t.Fatalf("应只剩实时判据不敢删的那条，实际 %v", ids)
	}

	// 台账已干净时再跑一次：**不是错误**（脚本里重复调用是常见操作）。
	if err := runCLI(t, "app", "prune", "--apps-dir", dir); err != nil {
		t.Fatalf("台账已干净时 app prune 应成功退出: %v", err)
	}
	// 参数守卫：prune 不吃位置参数。
	if err := runCLI(t, "app", "prune", "extra", "--apps-dir", dir); err == nil {
		t.Fatal("app prune 不接受位置参数")
	}
}

// ---- 小工具 ----

// freePortFor 取一个当前空闲的端口。
func freePortFor(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("取空闲端口失败: %v", err)
	}
	p := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return p
}

// holdPortFor 占住一个端口（调用方负责 Close），用来构造「收不干净」的场景。
func holdPortFor(t *testing.T) (int, net.Listener) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("占端口失败: %v", err)
	}
	return ln.Addr().(*net.TCPAddr).Port, ln
}
