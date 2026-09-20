//go:build windows

package coord

import "syscall"

// PROCESS_QUERY_LIMITED_INFORMATION：最小权限的进程查询权（能拿退出码/存活，不能读内存）。
// syscall 包未导出该常量，这里按 Win32 SDK 定义补上。
const processQueryLimitedInformation = 0x1000

// STILL_ACTIVE：GetExitCodeProcess 返回该值表示进程仍在运行（Win32 定义 259）。
const stillActive = 259

// processAlive 真实探测某 PID 是否存活（EXT_SERVICE_DESIGN.md §12.5）。
//
// **必须**走 OpenProcess + GetExitCodeProcess：Go 的 os.FindProcess 在 Windows 上
// 只是构造一个句柄占位，**对不存在的 PID 不报错**，直接拿它判存活会把已死 gear
// 误判为存活 → 占用列表永不清理 → sidecar 永不退出。这是 §12.5 明确点名的坑。
//
// 语义约定：
//   - OpenProcess 失败（进程不存在 / 无权限）→ 视为不存活。
//     注：极少数情况下权限不足会把存活进程判死；此处的调用方是 sidecar 巡检，
//     探测对象是同主机、同用户下由本 SDK 拉起的 gear，权限足够，故按「打不开即死」处理。
//   - 打开成功但退出码 != STILL_ACTIVE → 已退出 → 不存活。
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
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
