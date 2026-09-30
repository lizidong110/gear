package procalive_test

import (
	"os"
	"os/exec"
	"testing"
	"time"

	"gear/internal/procalive"
)

// TestAliveRejectsNonPositivePID 覆盖「pid 不合法」这条最短路径。
//
// 0 与负数在台账里真实存在（派生失败留下的记录、空值），判活必须闭掉它们——
// 否则 `pid <= 0` 的记录会被当成「进程还在」，台账永远清不掉。
func TestAliveRejectsNonPositivePID(t *testing.T) {
	for _, pid := range []int{0, -1, -42} {
		if procalive.Alive(pid) {
			t.Fatalf("pid=%d 必须判为不存活", pid)
		}
	}
}

// TestAliveReportsRunningProcess 是正判据：活着的进程必须判活。
//
// 没有它，「一律返回 false」这种退化实现也能骗过判死的用例。
func TestAliveReportsRunningProcess(t *testing.T) {
	cmd := spawnHelper(t, "sleep")
	defer stopHelper(cmd)
	// 给子进程一点时间真正跑起来（Start 返回时进程可能还没被调度）。
	deadline := time.Now().Add(5 * time.Second)
	for !procalive.Alive(cmd.Process.Pid) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !procalive.Alive(cmd.Process.Pid) {
		t.Fatalf("活着的进程被判成已退出：pid=%d", cmd.Process.Pid)
	}
}

// TestAliveReportsExitedProcessAsDead 是负判据：退出后的进程必须判死。
func TestAliveReportsExitedProcessAsDead(t *testing.T) {
	cmd := spawnHelper(t, "exit")
	// 等它真的退出：轮询而不 sleep 固定时长，避免慢机器上偶发。
	deadline := time.Now().Add(10 * time.Second)
	for procalive.Alive(cmd.Process.Pid) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if procalive.Alive(cmd.Process.Pid) {
		t.Fatalf("已退出的进程仍被判存活：pid=%d", cmd.Process.Pid)
	}
	_ = cmd.Wait()
}

// spawnHelper 起一个「辅助子进程」（本测试二进制的复刻），mode 决定它活着还是立刻退出。
//
// 用测试二进制自身而不是 `sleep` / `timeout` 之类的外部命令：外部命令在不同平台上名字与
// 参数都不一样，判活这种平台相关的东西不该再叠一层环境差异。
func spawnHelper(t *testing.T, mode string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestProcAliveHelper")
	cmd.Env = append(os.Environ(), "GEAR_PROCALIVE_HELPER=1", "GEAR_PROCALIVE_MODE="+mode)
	if err := cmd.Start(); err != nil {
		t.Fatalf("起辅助子进程失败: %v", err)
	}
	return cmd
}

func stopHelper(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
}

// TestProcAliveHelper 不是被测对象，而是上面几条用例的「进程替身」：
// sleep 模式活 30 秒等着被杀，exit 模式立刻退出。
func TestProcAliveHelper(t *testing.T) {
	if os.Getenv("GEAR_PROCALIVE_HELPER") != "1" {
		return
	}
	if os.Getenv("GEAR_PROCALIVE_MODE") == "sleep" {
		time.Sleep(30 * time.Second)
	}
	os.Exit(0)
}
