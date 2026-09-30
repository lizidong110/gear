//go:build !windows

package procalive

import (
	"errors"
	"syscall"
)

// alive 真实探测某 PID 是否存活。
//
// Unix 用 `signal(pid, 0)` 的惯例做法：
//   - nil / EPERM：进程存在（EPERM 表示有进程但无权限发信号，仍算存活）；
//   - ESRCH：进程不存在（含 zombie 被回收后）→ 不存活。
func alive(pid int) bool {
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	return errors.Is(err, syscall.EPERM)
}
