//go:build !windows

package coord

import (
	"gear/internal/procalive"
)

// processAlive 真实探测某 PID 是否存活。
//
// 判据收在 internal/procalive（Unix 走 signal 0：nil / EPERM 为存活，ESRCH 为已退出），
// 与 internal/app 共用一份，避免两个包各写一份后漂移。
func processAlive(pid int) bool { return procalive.Alive(pid) }
