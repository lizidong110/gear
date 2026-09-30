//go:build windows

package procalive_test

import (
	"os"
	"testing"
	"time"

	"gear/internal/procalive"
)

// TestAliveReportsUnreapedButExitedProcessAsDead 锁死这次修的那个 bug。
//
// 现场形态（`gear app stop` 误报）：pid 在 tasklist 里查不到、端口也没人监听，
// 但 stop 仍报「pid=N 仍在」，台账卡在 running 无法 prune。
//
// 成因是 Windows 上「进程已退出」与「进程对象还在」是两件事：只要还有**任何句柄**指向该
// 进程对象（父进程持有、或刚被 kill 还没回收），内核就不销毁它，于是 OpenProcess 照样
// 成功。只看「句柄打不打得开」的实现（本包修掉的那份）会把死进程判成活的。
//
// 构造方式：起一个立刻退出的子进程，并且**不 Release、不 Wait** —— 句柄一直握在我们手上，
// 僵尸条件就成立。真实生产路径同理：`app run` 的监管进程持有子进程句柄，子进程死了但
// 监管进程还没回收的那段时间，台账里的 pid 就是这种形态。
func TestAliveReportsUnreapedButExitedProcessAsDead(t *testing.T) {
	cmd := spawnHelper(t, "exit")
	pid := cmd.Process.Pid

	deadline := time.Now().Add(10 * time.Second)
	for procalive.Alive(pid) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}

	// 判据自检：此刻必须**仍然打得开**句柄。若打不开，说明进程对象已被销毁，
	// 那么正确实现与「只看句柄」的旧实现结论一致，这条用例就失去判别力了——宁可跳过，
	// 也不要留下一条「两种实现都绿」的假锁。
	if p, err := os.FindProcess(pid); err != nil {
		t.Skipf("僵尸条件不成立（OpenProcess 已失败: %v），本用例在此环境下无判别力", err)
	} else {
		_ = p.Release()
	}

	if procalive.Alive(pid) {
		t.Fatalf("进程已退出、仅剩句柄未回收时必须判死：pid=%d 仍被判为存活", pid)
	}
	_ = cmd.Wait() // 归还句柄，别把僵尸留到别的用例里
}
