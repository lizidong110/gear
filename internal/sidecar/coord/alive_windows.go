//go:build windows

package coord

import (
	"gear/internal/procalive"
)

// processAlive 真实探测某 PID 是否存活（EXT_SERVICE_DESIGN.md §12.5）。
//
// 判据收在 internal/procalive，与 internal/app 共用一份：Windows 上必须走
// OpenProcess + GetExitCodeProcess——`os.FindProcess` 只判「句柄打不打得开」，而进程已退出
// 但仍有句柄指向它时照样打开成功，会把死进程判成活的（§12.5 点名的坑）。两处各写一份已经
// 漂移过一次（app 包那一份就是这么错的），故统一到这里。
//
// 语义沿用 procalive 的约定：打不开 / 查不到即视为不存活（fail-closed）。
func processAlive(pid int) bool { return procalive.Alive(pid) }
