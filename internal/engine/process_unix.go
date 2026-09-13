//go:build !windows

package engine

import (
	"os/exec"
	"syscall"
)

// configureProcessTree 在 Unix 上将子进程置入独立进程组（pgid == 子进程 pid），
// 以便 killProcessTree 用 kill(-pgid) 级联回收整棵树（含 sh 派生的 sleep 等孙进程）。
func configureProcessTree(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessTree 级联杀死进程组（Unix）。负 pid 表示对进程组发信号；
// sh 新派的孙进程同属该组，一并被杀。
func killProcessTree(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
