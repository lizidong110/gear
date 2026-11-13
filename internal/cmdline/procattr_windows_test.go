//go:build windows

package cmdline

import (
	"context"
	"testing"
)

// TestCommandHidesConsole 断言本包派生的子进程带 CREATE_NO_WINDOW + HideWindow
// （REQ-PROC-NO-CONSOLE）：detached `app run` 自身无控制台，不给子进程设该标志的话，
// Windows 会给每个 service/once 子进程新开一个控制台黑窗。
func TestCommandHidesConsole(t *testing.T) {
	cmd, err := Command(context.Background(), "echo hi")
	if err != nil {
		t.Fatalf("Command 失败: %v", err)
	}
	if cmd.SysProcAttr == nil {
		t.Fatal("SysProcAttr 未设置——子进程会拖出控制台黑窗")
	}
	if cmd.SysProcAttr.CreationFlags&createNoWindow == 0 {
		t.Fatalf("缺 CREATE_NO_WINDOW 标志: CreationFlags=%#x", cmd.SysProcAttr.CreationFlags)
	}
	if !cmd.SysProcAttr.HideWindow {
		t.Fatal("缺 HideWindow")
	}
}
