// Package procalive 提供「某 PID 对应的进程是否仍在运行」的唯一判据。
//
// 为什么单独成包，而不是各包各写一份：`internal/app`（台账探活 / stop 复检）与
// `internal/sidecar/coord`（侧车巡检）都需要它，而两处各自实现过一次**并且已经漂移**——
// app 包那一份用 `os.FindProcess` 判「句柄能不能打开」，而 Windows 上「进程已退出」与
// 「进程对象还在」是两件事：只要还有任何句柄指向它（父进程持有、或刚 kill 还没回收），
// OpenProcess 就会成功，于是已死的进程被报成存活。实测形态是 pid 在 tasklist 里查不到、
// 端口也没人监听，`gear app stop` 仍报「pid=N 仍在」，台账卡在 running 清不掉。
//
// 平台判据天然不同（Windows 走 OpenProcess + GetExitCodeProcess，Unix 走 signal 0），
// 写两遍必然再漂移一次，故收在这里，两处都调它。
//
// 语义约定（两个平台一致）：
//   - pid <= 0 → 不存活；
//   - 打不开 / 查不到（进程不存在、权限不足）→ 不存活。fail-closed：宁可把活着的进程
//     误判成已退出，也不要让台账里卡着一条永远清不掉的 running——后者会让
//     `prune` 失效、`stop` 永远报 ORPHAN_APP；
//   - 打得开且退出码仍是 STILL_ACTIVE → 存活。
package procalive

// Alive 报告 pid 对应的进程是否仍在运行。
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return alive(pid)
}
