//go:build windows

package app

import (
	"os/exec"
	"syscall"
	"testing"
	"time"

	"gear/internal/procalive"

	"golang.org/x/sys/windows"
)

// createNoWindow 同 cmdline 包的 CREATE_NO_WINDOW（winbase.h），测试内自持一份。
const createNoWindow = 0x08000000

// TestJobKillOnCloseKillsAssignedChildren 断言 KILL_ON_JOB_CLOSE 的连坐语义
// （REQ-APP-JOB-KILLONCLOSE）：子进程挂进 Job 后，Job 句柄一关——等价于监管进程
// 被硬杀（taskkill /F）时系统代关句柄——子进程必须死。
//
// 不在测试里调 InitJobKillOnClose：那会把**测试进程自己**挂进 kill-on-close Job，
// 关句柄时连坐自杀。这里只对子进程句柄 assign，机制等价（子进程随 Job 句柄关闭而死）。
func TestJobKillOnCloseKillsAssignedChildren(t *testing.T) {
	job, err := newKillOnCloseJob()
	if err != nil {
		t.Fatalf("创建 Job 失败: %v", err)
	}

	// 与 launchDetached 同理：先试 breakaway（测试进程可能已在 harness 的 Job 里），
	// 失败退化不带标志再试。
	start := func(flags uint32) *exec.Cmd {
		cmd := exec.Command("cmd", "/c", "ping", "-n", "30", "127.0.0.1") // 长活子进程
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: flags}
		return cmd
	}
	cmd := start(createBreakawayFromJob | createNoWindow)
	if err := cmd.Start(); err != nil {
		cmd = start(createNoWindow)
		if err := cmd.Start(); err != nil {
			t.Fatalf("启动长活子进程失败: %v", err)
		}
	}
	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()

	// os.Process 不暴露原生句柄；assign 需要带 PROCESS_SET_QUOTA|PROCESS_TERMINATE 权限另开。
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err != nil {
		_ = cmd.Process.Kill()
		t.Fatalf("OpenProcess 失败: %v", err)
	}
	defer windows.CloseHandle(h)
	if err := windows.AssignProcessToJobObject(job, h); err != nil {
		_ = cmd.Process.Kill()
		<-waitDone
		t.Skipf("环境不允许把子进程挂进新 Job（可能已在别的 Job 且禁 breakaway）: %v", err)
	}

	// 先确认子进程确实活着（否则「死了」断言是假绿）。
	time.Sleep(300 * time.Millisecond)
	if !procalive.Alive(cmd.Process.Pid) {
		t.Fatal("子进程未进 Job 就自行退出了，测试前提不成立")
	}

	// 关 Job 句柄 → 连坐：子进程必须在 5s 内死。
	if err := windows.CloseHandle(job); err != nil {
		_ = cmd.Process.Kill()
		t.Fatalf("关闭 Job 句柄失败: %v", err)
	}
	select {
	case <-waitDone: // 子进程已死——不管退出码是什么（被杀非 0 属预期）
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		<-waitDone
		t.Fatal("Job 句柄已关闭 5s，子进程仍活着——KILL_ON_JOB_CLOSE 未生效")
	}
	if procalive.Alive(cmd.Process.Pid) {
		t.Fatal("Wait 已返回但进程仍判活")
	}
}
