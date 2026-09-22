package cmdline

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestResolveExecutableChecksExistenceForPaths 锁住 ResolveExecutable 与 LookExecutable
// 的**分工**：LookExecutable 对含分隔符的路径一律原样返回（把平台语义留给 os/exec），
// 这对「真要执行」是对的，但对「写 YAML 时提前校验」毫无用处——写错路径也照样通过。
func TestResolveExecutableChecksExistenceForPaths(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "present.exe")
	writeFile(t, exe, "stub")

	if got, err := ResolveExecutable(exe, nil); err != nil || got != exe {
		t.Fatalf("存在的绝对路径应解析成功: got=%q err=%v", got, err)
	}
	missing := filepath.Join(dir, "absent.exe")
	if _, err := ResolveExecutable(missing, nil); !errors.Is(err, exec.ErrNotFound) {
		t.Fatalf("不存在的路径应报 ErrNotFound，实际 %v", err)
	}
	// 对照：LookExecutable 对同一个不存在的路径**照样放行**——这正是要新加函数的原因。
	if _, err := LookExecutable(missing, nil); err != nil {
		t.Fatalf("LookExecutable 应原样返回含分隔符的路径（对照用），实际 %v", err)
	}
	if _, err := ResolveExecutable("", nil); err == nil {
		t.Fatal("空名字应报错")
	}
	if _, err := ResolveExecutable("definitely-not-a-real-cmd-xyz", nil); !errors.Is(err, exec.ErrNotFound) {
		t.Fatalf("裸名字找不到应报 ErrNotFound，实际 %v", err)
	}
}

// TestCommandRunsWindowsPathWithoutQuotes 是 2026-09-22 缺陷的**端到端回归锁**。
//
// 修复前：引号外的 `\x` 被无条件折成字面量 x，于是不带引号的 Windows 绝对路径
// `D:\gadern\gear\build\gear.exe` 被吃成 `D:gaderngearbuildgear.exe`，真跑起来时报
// `executable file not found in %PATH%`（把病因指向 PATH，与「路径写坏」无关）。
//
// 用法：拿**测试二进制自己**当那个外部程序（与包内其他用例同一手法：不引入任何外部工具），
// 路径不加引号传进去，要求它照样跑起来。
func TestCommandRunsWindowsPathWithoutQuotes(t *testing.T) {
	disableShell(t)
	self := os.Args[0]
	if strings.Contains(self, " ") {
		t.Skip("测试二进制路径含空格，不带引号的形态本就不适用（需引号）")
	}
	if strings.Contains(self, `"`) {
		t.Skip("测试二进制路径含引号，跳过")
	}
	t.Setenv("GEAR_CMDLINE_HELPER", "1")

	// 刻意**不加引号**：这正是被反斜杠转义吃掉的那种写法。
	cmd, err := Command(context.Background(), self+" -test.run=TestCmdlineHelperProcess")
	if err != nil {
		t.Fatalf("不带引号的绝对路径应能构造命令（反斜杠不得被当转义符）: %v", err)
	}
	out := runHelper(t, cmd)
	if got := out["argv"]; got == nil {
		t.Fatalf("子进程应正常回传 argv，实际 %v", out)
	}
}

// TestParseKeepsDriveLetterPathIntact 是上面那条的**纯解析层**对照：
// 不需要真跑进程，直接看切词结果有没有被反斜杠污染。
func TestParseKeepsDriveLetterPathIntact(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("本用例覆盖 Windows 盘符路径形态")
	}
	spec, err := Parse(`D:\gadern\gear\build\gear.exe serve --addr 127.0.0.1:8788`)
	if err != nil {
		t.Fatalf("Parse 报错: %v", err)
	}
	if spec.Argv[0] != `D:\gadern\gear\build\gear.exe` {
		t.Fatalf("argv[0] 被反斜杠转义污染: %q", spec.Argv[0])
	}
}
