//go:build !windows

package app

// launch_unix.go：类 Unix 上的「脱离调用方会话」派生与进程组终止。

import (
	"os"
	"os/exec"
	"syscall"
	"time"
)

// launchDetached 派生一个自成会话的后台进程并立即返回（**不 Wait**）。
//
// Setsid 让子进程脱离调用方的会话与进程组：调用方（MCP Server / CLI）退出后，子进程
// 不会被 SIGHUP 带走，从而实现「跨会话存活」（设计文档 §6.2⑤）。
//
// 第二个返回值 escaped 在 Unix 上恒为 true：Setsid 没有「请求被拒绝」的退化路径
// （对比 Windows 的 CREATE_BREAKAWAY_FROM_JOB，见 launch_windows.go）。
func launchDetached(self string, args []string, dir string, logOut *os.File) (*os.Process, bool, error) {
	cmd := exec.Command(self, args...)
	cmd.Dir = dir
	cmd.Stdout = logOut
	cmd.Stderr = logOut
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, false, err
	}
	_ = cmd.Process.Release()
	return cmd.Process, true, nil
}

// killTree 结束 pid 及其整个进程组（Setsid 使 pgid == pid，负号即「发给整组」）。
//
// 先 SIGTERM 给一次优雅退出的机会；超时仍不退则 SIGKILL。收不干净与否由 Stop 的端口
// 复检如实上报，本函数不假装成功。
func killTree(pid int) error {
	if pid <= 0 {
		return nil
	}
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil && err != syscall.ESRCH {
		// 进程组不可用时退化为按 pid 单杀（例如子进程自己又 setsid 了）。
		if err2 := syscall.Kill(pid, syscall.SIGTERM); err2 != nil && err2 != syscall.ESRCH {
			return err
		}
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	_ = syscall.Kill(pid, syscall.SIGKILL)
	return nil
}

// processAlive 报告 pid 是否仍存活：信号 0 的投递结果即判据（Go 1.27 src/os/exec.go）。
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}
