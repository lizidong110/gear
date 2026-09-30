//go:build windows

package procalive

import (
	"syscall"
)

// PROCESS_QUERY_LIMITED_INFORMATION：最小权限的进程查询权（能拿退出码/存活，不能读内存）。
// syscall 包未导出该常量，这里按 Win32 SDK 定义补上。
const processQueryLimitedInformation = 0x1000

// STILL_ACTIVE：GetExitCodeProcess 返回该值表示进程仍在运行（Win32 定义 259）。
const stillActive = 259

// alive 真实探测某 PID 是否存活（EXT_SERVICE_DESIGN.md §12.5）。
//
// **必须**走 OpenProcess + GetExitCodeProcess：Go 的 `os.FindProcess` 在 Windows 上只是
// 打开一个句柄，**对已退出但仍有句柄指向它的进程照样成功**——直接拿它判存活会把已死进程
// 误判为存活，占用列表永不清理。这是 §12.5 明确点名的坑。
//
// 权限不足时 OpenProcess 也会失败，此时按「打不开即死」处理：本包的调用方探测的都是
// 同主机、同用户下由 gear 自己拉起的进程，权限足够；而反方向（把活进程判死）的代价是
// 多一次重启/多一条 orphan，远小于把死进程判活。
func alive(pid int) bool {
	h, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		return false
	}
	defer syscall.CloseHandle(h)

	var code uint32
	if err := syscall.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code == stillActive
}
