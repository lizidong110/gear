//go:build windows

package app

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// InitJobKillOnClose 把**当前进程**挂进一个 KILL_ON_JOB_CLOSE 的 Job Object（Windows）。
//
// 为什么必须有它（2026-11-13 实测）：detached `app run` 监管进程与调用方互不持有，
// 调用方硬杀监管进程（taskkill /F）后，它拉起的 service 子进程（如 serve-sidecar）
// 没有归宿变成孤儿继续占端口；而手动杀子进程又会被 --restart always 当崩溃重启回来
// ——「关不完」。
//
// 机制：进程进 Job 后，它此后派生的全部子孙（除非带 CREATE_BREAKAWAY_FROM_JOB）自动
// 入组；监管进程被硬杀 → Job 最后一个句柄随进程关闭 → 组内全部进程连坐终止。
// 与 `app stop` 的 killTree（taskkill /T /F）双杀无害：进程已死时 taskkill 报
// 「找不到」即幂等收尾。
//
// 失败降级（不 fatal）：若本进程已在别的 Job 里且无法进入嵌套 Job（如 harness 的 Job
// 禁止 breakaway，launchDetached 的退化路径上本进程没逃出去），assign 会失败——
// 监管功能本身不依赖 Job，只是「硬杀连坐」这层保障缺失，打印警告即可。
func InitJobKillOnClose() error {
	job, err := newKillOnCloseJob()
	if err != nil {
		return err
	}
	if err := windows.AssignProcessToJobObject(job, windows.CurrentProcess()); err != nil {
		_ = windows.CloseHandle(job)
		return fmt.Errorf("进程挂入 kill-on-close Job 失败（硬杀连坐不可用）: %w", err)
	}
	// 句柄**故意不关也不持**：裸句柄不会被 GC 回收，本进程存活期间它就一直开着
	// ——这正是 KILL_ON_JOB_CLOSE 的扳机（进程被硬杀时系统代关 → 连坐整棵树）。
	// 提前关等于自尽，故不落任何会被「顺手 Close」的变量里。
	return nil
}

// newKillOnCloseJob 创建设置了 KILL_ON_JOB_CLOSE 的 Job Object（不 assign 任何进程）。
// 独立成函数供负向验证测试复用：只对子进程句柄 assign，避免测试进程自己进组被连坐。
func newKillOnCloseJob() (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, fmt.Errorf("CreateJobObject: %w", err)
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		_ = windows.CloseHandle(job)
		return 0, fmt.Errorf("SetInformationJobObject(KILL_ON_JOB_CLOSE): %w", err)
	}
	return job, nil
}
