//go:build windows

package app

// launch_windows.go：Windows 上的「脱离调用方进程树」派生与进程树终止。
//
// 为什么必须用构建标签：detach 的手段是平台特有的（Windows 靠 CreationFlags，
// Unix 靠 Setsid），而调用方（launch.go）只关心「派生了、返回了、我不持有」。

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// Windows 进程创建标志（winbase.h）。
const (
	detachedProcess        = 0x00000008 // 无控制台，进程在后台独立运行
	createNewProcessGroup  = 0x00000200 // 自成进程组
	createBreakawayFromJob = 0x01000000 // 从调用方所在 Job Object 中脱离
)

// launchDetached 派生一个脱离调用方进程树的后台进程并立即返回（**不 Wait**）。
//
// 先带 CREATE_BREAKAWAY_FROM_JOB 试一次：若客户端（如 WorkBuddy）用 Job Object 收整个进程
// 树，不带该标志的派生进程会被连带杀掉，「跨会话存活」失效（设计文档 R3c）。但若调用方不在
// Job 里、或 Job 不允许 breakaway，带该标志的 Start 会直接失败——此时**退化为不带该标志
// 再试一次**，并把第一次的失败原因写进退化路径的错误里（不静默，但也不因为一个环境差异
// 让整个功能不可用）。
//
// 第二个返回值 escaped 报告「这次派生到底有没有脱离调用方的 Job」。**它必须被如实上报**：
// 退化路径上的进程仍在调用方的 Job 里，调用方一退出它就会被连坐杀掉——即
// 「跨会话存活」这个承诺在退化时并不成立。静默退化会让人以为应用已经起稳了，
// 直到会话结束才发现进程没了（2026-09-21 实测：本机 harness 的 Job 禁止 breakaway，
// 派生出的应用在**当前命令返回时**就被回收，而 CLI 打印的是 running）。
func launchDetached(self string, args []string, dir string, logOut *os.File) (*os.Process, bool, error) {
	proc, err := startWithFlags(self, args, dir, logOut,
		detachedProcess|createNewProcessGroup|createBreakawayFromJob)
	if err == nil {
		return proc, true, nil
	}
	first := err
	proc, err = startWithFlags(self, args, dir, logOut, detachedProcess|createNewProcessGroup)
	if err != nil {
		return nil, false, fmt.Errorf("带 BREAKAWAY_FROM_JOB 尝试失败(%v)；退化尝试同样失败(%w)", first, err)
	}
	return proc, false, nil
}

// startWithFlags 用给定的 CreationFlags 派生进程。
func startWithFlags(self string, args []string, dir string, logOut *os.File, flags uint32) (*os.Process, error) {
	cmd := exec.Command(self, args...)
	cmd.Dir = dir
	cmd.Stdout = logOut
	cmd.Stderr = logOut
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: flags, HideWindow: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	// 不调用 cmd.Wait()：本函数是启动器而非持有者。Release 释放句柄，让子进程成为
	// 完全独立的存在——调用方何时退出与它无关。
	_ = cmd.Process.Release()
	return cmd.Process, nil
}

// killTree 结束 pid 及其子进程树。
//
// 为什么必须连树收：Windows 上 DETACHED_PROCESS 没有控制台，投不了 CTRL_BREAK，只能硬杀；
// 而应用的长驻侧车（如 serve-sidecar）是 `app run` 的**子进程**，只杀父进程会让孙进程
// 攥着端口不放——那正是「stop 返回成功但端口仍被占」的成因（设计文档 R3b）。
func killTree(pid int) error {
	if pid <= 0 {
		return nil
	}
	out, err := exec.Command("taskkill", "/PID", strconv.Itoa(pid), "/T", "/F").CombinedOutput()
	if err != nil {
		// 进程已退出时 taskkill 会报「找不到进程」——那不是失败，是已经收干净了。
		if !processAlive(pid) {
			return nil
		}
		return fmt.Errorf("taskkill 失败: %v（输出: %s）", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// processAlive 报告 pid 是否仍存活。
//
// Windows 上 os.FindProcess 走 OpenProcess，进程不存在即返回错误（Go 1.27
// src/os/exec_windows.go）——这正是我们需要的判据，无需额外系统调用。
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	_ = p.Release()
	return true
}
