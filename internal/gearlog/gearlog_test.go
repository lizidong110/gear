package gearlog

// gearlog_test.go 锁住三条设计红线：不启用=零变化、不静默降级、敏感值不回显。
// 每条红线都对应一类「静默缺陷」——测试是它们唯一的机械守卫。

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSetupEmptyPathIsNoop —— 不传 --log-file 时 L() 必须是 discard：
// 诊断日志是选配，默认开启会让每条命令都多个文件副作用。
func TestSetupEmptyPathIsNoop(t *testing.T) {
	ResetForTest()
	t.Cleanup(ResetForTest)

	c, err := Setup("", "")
	if err != nil || c != nil {
		t.Fatalf("空 path 应返回 (nil, nil)，实际 (%v, %v)", c, err)
	}
	// discard logger：记录不 panic、不落任何文件。
	L().Info("这条不该出现在任何地方")
}

// TestSetupWritesStructuredLog —— 启用后日志落文件、带时间戳与 key=value。
func TestSetupWritesStructuredLog(t *testing.T) {
	ResetForTest()
	t.Cleanup(ResetForTest)

	path := filepath.Join(t.TempDir(), "sub", "gear.log")
	c, err := Setup(path, "info")
	if err != nil {
		t.Fatalf("Setup 失败: %v", err)
	}
	if c == nil {
		t.Fatal("启用时必须返回可 close 的对象")
	}
	t.Cleanup(func() { _ = c.Close() })

	L().Info("命令开始", Attr("cmd", "ai rewoo"), slog.Int("pid", os.Getpid()))
	if err := c.Close(); err != nil {
		t.Fatalf("close 失败: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("日志文件应已创建（含不存在的子目录）: %v", err)
	}
	got := string(raw)
	for _, want := range []string{"level=INFO", "msg=命令开始", "cmd=\"ai rewoo\"", "pid=", "time="} {
		if !strings.Contains(got, want) {
			t.Fatalf("日志缺 %q，实际:\n%s", want, got)
		}
	}
}

// TestSetupLevelFilters —— 级别阈值生效：info 档收不到 debug。
func TestSetupLevelFilters(t *testing.T) {
	ResetForTest()
	t.Cleanup(ResetForTest)

	path := filepath.Join(t.TempDir(), "gear.log")
	c, err := Setup(path, "warn")
	if err != nil {
		t.Fatalf("Setup 失败: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	L().Debug("debug 不该出现")
	L().Info("info 也不该出现")
	L().Warn("warn 该出现")

	raw, _ := os.ReadFile(path)
	got := string(raw)
	if strings.Contains(got, "debug 不该出现") || strings.Contains(got, "info 也不该出现") {
		t.Fatalf("低于阈值的记录不该落盘:\n%s", got)
	}
	if !strings.Contains(got, "warn 该出现") {
		t.Fatalf("达到阈值的记录必须落盘:\n%s", got)
	}
}

// TestSetupRejectsBadLevel —— 非法级别必须报错（阈值是明确意图，不许静默兜底）。
func TestSetupRejectsBadLevel(t *testing.T) {
	ResetForTest()
	t.Cleanup(ResetForTest)

	if _, err := Setup("", "verbose"); err == nil {
		t.Fatal("非法 level 应报错")
	}
}

// TestAttrRedactsSensitive —— 敏感字段值必须打码，非敏感字段照原样
// （过度打码会让档案失去排障价值，打码不够等于把密钥写进会外传的文件）。
func TestAttrRedactsSensitive(t *testing.T) {
	for _, k := range []string{"api_key", "API-KEY", "Token", "secret", "password"} {
		if got := Attr(k, "sk-super-secret"); got.Value.String() != RedactedMark {
			t.Fatalf("%q 的值必须打码，实际 %q", k, got.Value.String())
		}
	}
	for _, k := range []string{"model", "flow_id", "cmd", "addr"} {
		if got := Attr(k, "gpt-4"); got.Value.String() != "gpt-4" {
			t.Fatalf("%q 不是敏感字段，值应照原样，实际 %q", k, got.Value.String())
		}
	}
}

// TestSetupAppendsAcrossRuns —— 同一文件多次 Setup 是追加不是覆盖：
// 排障档案跨命令拼接才有意义（后一次启动把前一次冲掉 = 自毁历史）。
func TestSetupAppendsAcrossRuns(t *testing.T) {
	ResetForTest()
	t.Cleanup(ResetForTest)

	path := filepath.Join(t.TempDir(), "gear.log")
	c1, err := Setup(path, "info")
	if err != nil {
		t.Fatalf("第一次 Setup 失败: %v", err)
	}
	L().Info("第一次")
	_ = c1.Close()

	c2, err := Setup(path, "info")
	if err != nil {
		t.Fatalf("第二次 Setup 失败: %v", err)
	}
	L().Info("第二次")
	_ = c2.Close()

	raw, _ := os.ReadFile(path)
	got := string(raw)
	if !strings.Contains(got, "第一次") || !strings.Contains(got, "第二次") {
		t.Fatalf("两次记录都应保留（追加语义）:\n%s", got)
	}
}
