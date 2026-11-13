//go:build windows

package cmdline

import (
	"os/exec"
	"syscall"
)

// createNoWindow 阻止 Windows 给子进程分配新的控制台窗口（winbase.h CREATE_NO_WINDOW）。
const createNoWindow = 0x08000000

// applyNoConsole 给子进程设置「无控制台」创建标志。
//
// 为什么必须做（2026-11-13 实测）：detached `app run` 监管进程自身没有控制台，它经本包
// 派生控制台程序（如 serve-sidecar）时，Windows 会**新开一个控制台黑窗**——用户看到的
// 「后台应用拖着一口黑窗口」就是这么来的。CREATE_NO_WINDOW 让子进程跑在无窗口的
// 空控制台里；stdout/stderr 本就由调用方重定向到管道/日志（红线 16），不受影响。
//
// 只影响本包派生的直接子进程；孙进程由子进程自己负责。
func applyNoConsole(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
}
