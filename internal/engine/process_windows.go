//go:build windows

package engine

import (
	"os/exec"
	"strconv"
)

// configureProcessTree 在 Windows 上无需预处理：级联回收由 killProcessTree 经
// `taskkill /T` 完成。Windows 没有 POSIX 进程组，job object 需处理「子进程在入组前已派生」
// 的竞态，而 taskkill /T 直接按进程树回收，简单可靠（见 §11 第五次变更日志）。
func configureProcessTree(cmd *exec.Cmd) {}

// killProcessTree 级联杀死 cmd 进程及其全部子孙（Windows）。
// taskkill /T 按进程树回收、/F 强制；即便 sh 已退出，taskkill 仅报错，忽略即可（幂等）。
func killProcessTree(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	_ = exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run()
	return nil
}
