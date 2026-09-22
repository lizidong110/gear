package app

import (
	"errors"
	"net"
	"os"
	"testing"
	"time"
)

// deadPID 是一个**确定不存在**的 pid（最大值量级，真实 pid 不会到这里）。
const deadPID = 1 << 30

// TestPruneRemovesOnlyTerminalRecords 锁住 prune 的**保守边界**：
// 只清「确定已终止」的台账项，running 与 **unknown 一律保留**。
//
// unknown 这条是关键护栏：双判据互相矛盾时（pid 在但端口没起）不敢断言它已死，
// 删台账会把「可能还在跑、却再无人可查」的进程变成真孤儿——正是台账存在的意义。
// 2026-09-22 实测场景：一轮调试下来台账里堆了 12 条 startup_failed/unknown/exited，
// 而此前只有按 app_id 单个 stop 的入口，清理要调 12 次。
func TestPruneRemovesOnlyTerminalRecords(t *testing.T) {
	dir := t.TempDir()
	if processAlive(deadPID) {
		t.Skipf("pid %d 在本机被判为存活，构造不出「已终止」场景", deadPID)
	}

	liveLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("占用一个端口失败: %v", err)
	}
	defer func() { _ = liveLn.Close() }()
	livePort := liveLn.Addr().(*net.TCPAddr).Port
	idlePort := freePort(t)

	save := func(id, flow string, pid int, ports []int, final string) {
		t.Helper()
		r := &Record{
			AppID: id, FlowID: flow, PID: pid, Ports: ports,
			StartedAt: time.Now(), FinalState: final,
		}
		if err := SaveRecord(dir, r); err != nil {
			t.Fatalf("写台账失败: %v", err)
		}
	}
	save("app_run", "f1", os.Getpid(), []int{livePort}, "")     // 在跑：pid 活 + 端口在听
	save("app_unknown", "f1", os.Getpid(), []int{idlePort}, "") // 不确定：pid 活 + 端口没听
	save("app_dead", "f1", deadPID, nil, "")                    // 已终止：pid 没了、无端口
	save("app_failed", "f2", deadPID, nil, string(StateStartupFailed))

	res, err := Prune(PruneOptions{AppsDir: dir})
	if err != nil {
		t.Fatalf("Prune 失败: %v", err)
	}
	if res.DryRun {
		t.Fatal("默认应是真删（DryRun 为显式选项）")
	}

	prunedState := map[string]string{}
	for _, it := range res.Pruned {
		prunedState[it.AppID] = it.State
	}
	if len(res.Pruned) != 2 || prunedState["app_dead"] == "" || prunedState["app_failed"] == "" {
		t.Fatalf("应只清 app_dead 与 app_failed，实际 %+v", res.Pruned)
	}
	// 展示用状态取台账里的结案结论（比当下 Probe 出的 exited 更能说明「这是什么」）。
	if prunedState["app_failed"] != string(StateStartupFailed) {
		t.Fatalf("app_failed 的展示状态应保留 startup_failed，实际 %q", prunedState["app_failed"])
	}

	skipped := map[string]bool{}
	for _, it := range res.Skipped {
		skipped[it.AppID] = true
		if it.Reason == "" {
			t.Fatalf("被跳过的项必须说明原因，否则调用方会以为台账已干净: %+v", it)
		}
	}
	if !skipped["app_run"] || !skipped["app_unknown"] {
		t.Fatalf("running 与 unknown 都必须保留，实际 skipped=%v", skipped)
	}

	// 磁盘核对：该删的没了、该留的还在。
	if _, err := LoadRecord(dir, "app_dead"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("app_dead 的台账应已删除，实际 err=%v", err)
	}
	if _, err := LoadRecord(dir, "app_run"); err != nil {
		t.Fatalf("app_run 的台账必须保留，实际 err=%v", err)
	}
	if _, err := LoadRecord(dir, "app_unknown"); err != nil {
		t.Fatalf("app_unknown 的台账必须保留（unknown 不敢删），实际 err=%v", err)
	}
}

// TestPruneDryRunAndFlowFilter：dry_run 只汇报不动盘；flow_id 过滤只影响目标集合。
func TestPruneDryRunAndFlowFilter(t *testing.T) {
	dir := t.TempDir()
	if processAlive(deadPID) {
		t.Skipf("pid %d 在本机被判为存活", deadPID)
	}
	for _, id := range []string{"app_a", "app_b"} {
		flow := "f1"
		if id == "app_b" {
			flow = "f2"
		}
		if err := SaveRecord(dir, &Record{AppID: id, FlowID: flow, PID: deadPID, StartedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}

	res, err := Prune(PruneOptions{AppsDir: dir, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Pruned) != 2 || !res.DryRun {
		t.Fatalf("dry_run 应汇报 2 条待清，实际 %+v", res)
	}
	if _, err := LoadRecord(dir, "app_a"); err != nil {
		t.Fatalf("dry_run 不得动磁盘，实际 %v", err)
	}

	res, err = Prune(PruneOptions{AppsDir: dir, FlowID: "f2"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Pruned) != 1 || res.Pruned[0].AppID != "app_b" {
		t.Fatalf("flow_id 过滤应只命中 app_b，实际 %+v", res.Pruned)
	}
	if _, err := LoadRecord(dir, "app_a"); err != nil {
		t.Fatalf("未命中的 app_a 应保留，实际 %v", err)
	}
}

// TestPrunableState 表驱动锁住「哪些状态算已终止」——多一个都不行。
func TestPrunableState(t *testing.T) {
	cases := map[AppState]bool{
		StateExited:         true,
		StateStopped:        true,
		StateStartupFailed:  true,
		StateRunning:        false,
		StateStarting:       false,
		StateUnknown:        false,
		StateAlreadyRunning: false,
	}
	for st, want := range cases {
		if got := prunableState(st); got != want {
			t.Fatalf("prunableState(%s) = %v，期望 %v", st, got, want)
		}
	}
}
