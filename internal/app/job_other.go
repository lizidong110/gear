//go:build !windows

package app

// InitJobKillOnClose 非 Windows 没有 Job Object，空实现（见 job_windows.go）。
// Unix 上 detached 派生用 Setsid，硬杀连坐靠调用方 kill(-pgid)，无需本机制。
func InitJobKillOnClose() error { return nil }
