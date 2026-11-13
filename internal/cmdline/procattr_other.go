//go:build !windows

package cmdline

import "os/exec"

// applyNoConsole 非 Windows 没有控制台窗口概念，空实现（见 procattr_windows.go）。
func applyNoConsole(_ *exec.Cmd) {}
