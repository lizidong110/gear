package app

import (
	"context"
	"os"
	"testing"

	"gear/internal/engine"
)

// 2026-09-21：service 秒退时监管必须打印**退出原因**（退出码 + 输出尾部），
// 而不是只报「↻ 重启 第 N 次」。用 RestartNever 让监管在首次退出后收尾，
// 就能在稳定时刻断言输出（不必等待重启事件）。
func TestSuperviseReportsServiceExitReason(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	// 模拟真实故障：进程把失败信息打到 stderr 后以非零码退出（对应真实场景「端口被占」）。
	if err := reg.UpsertTool(svcTool("echo bind-failed-8787 1>&2; exit 7")); err != nil {
		t.Fatalf("注册 Tool 失败: %v", err)
	}
	registerSvcFlow(t, reg, "crash")

	// 监管把退出原因打到 os.Stdout：换成临时文件捕获（顺带确认 service 输出不会绕进
	// 调用方 stdout，即红线 16）。
	f, err := os.CreateTemp(t.TempDir(), "sup-out")
	if err != nil {
		t.Fatalf("建临时文件失败: %v", err)
	}
	defer f.Close()
	saved := os.Stdout
	os.Stdout = f

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sup := New(engine.New(reg), RestartNever, 0)
	runErr := sup.Supervise(ctx, []string{"crash"})
	os.Stdout = saved

	if runErr != nil {
		t.Fatalf("Supervise 失败: %v", runErr)
	}
	data, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatalf("读捕获输出失败: %v", err)
	}
	out := string(data)
	if !hasSubstr(out, "service 退出 (exit=7)") {
		t.Fatalf("应打印退出码 (exit=7)，实际输出:\n%s", out)
	}
	if !hasSubstr(out, "bind-failed-8787") {
		t.Fatalf("应打印输出尾部（失败原因），实际输出:\n%s", out)
	}
}

// hasSubstr 判断 s 是否含 sub。
func hasSubstr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
