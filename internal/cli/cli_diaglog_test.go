package cli

// cli_diaglog_test.go 锁住诊断日志选型（REQ-CLI-DIAG-LOG）的 CLI 面契约：
// 档案必须落、密钥必须打码、失败必须记、stdout 必须干净、默认必须零行为变化。
// 每条都对应一类「静默缺陷」——没有机械守卫时它们不会报错，只会在排障时缺席。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gear/internal/gearlog"
)

// runCLIWithDiag 跑一次带 --log-file 的命令，返回档案内容。
// 每个用例独立文件 + gearlog 复位：CLI 测试共享进程，包级 logger 不隔离会串味。
//
// 注册顺序有讲究：t.Cleanup 是 LIFO——gearlog 的 ResetForTest（关文件句柄）必须
// **晚于** t.TempDir() 注册，才能先于 TempDir 的 RemoveAll 执行；反了的话 Windows
// 上「文件被占用」会让 TempDir 清理失败（实测踩过）。
func runCLIWithDiag(t *testing.T, extraArgs ...string) string {
	t.Helper()
	gearlog.ResetForTest()

	path := filepath.Join(t.TempDir(), "diag.log")
	t.Cleanup(gearlog.ResetForTest)

	args := append([]string{"--log-file", path}, extraArgs...)
	if err := runCLI(t, args...); err != nil {
		t.Fatalf("命令应成功: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("档案文件应已创建: %v", err)
	}
	return string(raw)
}

// TestDiagLogRecordsLifecycle —— 档案记命令开始与完成，带 pid/cwd/registry/版本。
func TestDiagLogRecordsLifecycle(t *testing.T) {
	got := runCLIWithDiag(t, "version")
	for _, want := range []string{
		`msg=命令开始`, `cmd="gear version"`, "pid=",
		"registry=", "version=", "cwd=",
		`msg=命令完成`, "elapsed_ms=",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("档案缺 %q，实际:\n%s", want, got)
		}
	}
}

// TestDiagLogRedactsAPIKey —— --api-key 的值必须打码：档案是「随手就会贴出去」的文件，
// 原样记密钥等于把密钥从命令行扩散到聊天记录/工单/截图。
// 任务注定失败（无有效 key），无所谓——本用例只断言档案里的脱敏，不要求命令成功。
func TestDiagLogRedactsAPIKey(t *testing.T) {
	gearlog.ResetForTest()
	path := filepath.Join(t.TempDir(), "diag.log")
	t.Cleanup(gearlog.ResetForTest)

	_ = runCLI(t, "--log-file", path, "ai", "react", "任意任务", "--api-key", "sk-do-not-leak-me")

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("档案应已创建: %v", err)
	}
	got := string(raw)
	if !strings.Contains(got, gearlog.RedactedMark) {
		t.Fatalf("api-key 必须打码，实际:\n%s", got)
	}
	if strings.Contains(got, "sk-do-not-leak-me") {
		t.Fatalf("档案出现明文密钥:\n%s", got)
	}
}

// TestDiagLogRecordsFailure —— 失败命令也要落 ERROR 档案行。
// 「命令失败了但档案里只有开始」= 事后回看时最查不动的一类。
func TestDiagLogRecordsFailure(t *testing.T) {
	gearlog.ResetForTest()
	path := filepath.Join(t.TempDir(), "diag.log")
	t.Cleanup(gearlog.ResetForTest)

	err := runCLI(t, "--log-file", path, "ai", "react", "") // 空任务：必失败
	if err == nil {
		t.Fatal("空任务应失败")
	}
	raw, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatalf("失败命令也必须留档案: %v", rerr)
	}
	got := string(raw)
	if !strings.Contains(got, "level=ERROR") || !strings.Contains(got, "命令失败") {
		t.Fatalf("档案必须含 ERROR 行，实际:\n%s", got)
	}
}

// TestDiagLogDefaultOff —— 不带 --log-file 时命令照常成功（零行为变化）。
func TestDiagLogDefaultOff(t *testing.T) {
	gearlog.ResetForTest()
	t.Cleanup(gearlog.ResetForTest)
	if err := runCLI(t, "version"); err != nil {
		t.Fatalf("不带 --log-file 的存量用法必须照常: %v", err)
	}
}

// TestDiagLogRejectsBadLevel —— 非法级别是用户输入错误，尽早失败（不许静默兜底成 info）。
func TestDiagLogRejectsBadLevel(t *testing.T) {
	gearlog.ResetForTest()
	path := filepath.Join(t.TempDir(), "x.log")
	t.Cleanup(gearlog.ResetForTest)
	err := runCLI(t, "--log-file", path, "--log-level", "chatty", "version")
	if err == nil || !strings.Contains(err.Error(), "log-level") {
		t.Fatalf("非法 level 必须报错，实际 %v", err)
	}
}

// TestDiagLogDegradesLoudlyOnBadPath —— 路径开不了时主命令照常成功，且 stderr 明说
// （不静默降级：否则用户会对着一个不存在的档案排障）。
func TestDiagLogDegradesLoudlyOnBadPath(t *testing.T) {
	gearlog.ResetForTest()
	dir := t.TempDir() // 目录当路径传：必然开不了
	t.Cleanup(gearlog.ResetForTest)

	if err := runCLI(t, "--log-file", dir, "version"); err != nil {
		t.Fatalf("日志开不了不该拖垮主命令: %v", err)
	}
	// stderr 断言由真机覆盖（runCLI 不接管 stderr）；这里至少确认没有假成功副作用：
	// 目录里不应冒出任何 .log 文件。
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".log") {
			t.Fatalf("开不了日志时不该留下半个文件: %s", e.Name())
		}
	}
}

// TestDiagLogTruncatesLongPositional —— 位置参数（任务描述）截断 200 字符：
// 那是内容不是标识，全量进档案只会淹掉关键行。命令本身失败无所谓（无有效 key），
// 断言只看 invocation 的长度。
func TestDiagLogTruncatesLongPositional(t *testing.T) {
	gearlog.ResetForTest()
	path := filepath.Join(t.TempDir(), "diag.log")
	t.Cleanup(gearlog.ResetForTest)

	long := strings.Repeat("任务", 300)
	_ = runCLI(t, "--log-file", path, "ai", "react", long, "--max-steps", "1")

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("档案应已创建: %v", err)
	}
	got := string(raw)
	if strings.Count(got, "任务") > 120 {
		t.Fatalf("长位置参数应被截断，实际档案含 %d 个「任务」:\n%.200s...", strings.Count(got, "任务"), got)
	}
}
