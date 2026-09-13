package engine

import (
	"context"
	"testing"
	"time"
)

// F4：非零退出码应返回错误，且 stderr 被捕获进错误信息。
func TestRunProcessNonZeroExit(t *testing.T) {
	_, err := runProcess(context.Background(), "sh -c 'echo boom >&2; exit 3'", map[string]interface{}{})
	if err == nil {
		t.Fatal("非零退出码应返回错误")
	}
	if !containsSubstr(err.Error(), "boom") {
		t.Fatalf("stderr 应被捕获: %v", err)
	}
}

// F4：stderr 输出不应干扰 stdout 末行 JSON 解析。
func TestRunProcessStderrIgnored(t *testing.T) {
	out, err := runProcess(context.Background(), `sh -c 'echo warn >&2; echo "{\"name\":\"x\"}"'`, map[string]interface{}{})
	if err != nil {
		t.Fatalf("stderr 不应导致失败: %v", err)
	}
	if out["name"] != "x" {
		t.Fatalf("stdout 解析异常: %v", out)
	}
}

// F4：stdout 为空应报错。
func TestRunProcessEmptyStdout(t *testing.T) {
	if _, err := runProcess(context.Background(), "true", map[string]interface{}{}); err == nil {
		t.Fatal("空 stdout 应报错")
	}
}

// F4：stdout 含日志行时取末行 JSON。
func TestRunProcessDirtyStdout(t *testing.T) {
	out, err := runProcess(context.Background(), `sh -c 'echo "log line"; echo "{\"name\":\"x\"}"'`, map[string]interface{}{})
	if err != nil {
		t.Fatalf("脏 stdout 应跳过日志行: %v", err)
	}
	if out["name"] != "x" {
		t.Fatalf("末行 JSON 解析异常: %v", out)
	}
}

// F4：超时受控——挂死命令在时限内返回错误，不占住执行线程。
func TestRunProcessTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if _, err := runProcess(ctx, "sleep 2", map[string]interface{}{}); err == nil {
		t.Fatal("超时命令应返回错误")
	}
}

// F4：未超时命令正常返回。
func TestRunProcessWithinTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := runProcess(ctx, "cat", map[string]interface{}{"name": "x"})
	if err != nil {
		t.Fatalf("正常命令应成功: %v", err)
	}
	if out["name"] != "x" {
		t.Fatalf("回显异常: %v", out)
	}
}

func containsSubstr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
