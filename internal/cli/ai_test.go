package cli

import (
	"bytes"
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"gear/internal/registry"
	"gear/internal/server"
)

// mockInferrer 是 ai.Inferrer 的 Mock（cli 测试用；结构满足接口即可，无需显式声明实现）。
type mockInferrer struct {
	text string
	err  error
}

func (m *mockInferrer) Infer(ctx context.Context, prompt string) (string, error) {
	return m.text, m.err
}

// TestAIGenPrintsYAML 覆盖「ai gen 默认打印生成的 Flow YAML」分支（不入库）。
func TestAIGenPrintsYAML(t *testing.T) {
	reg, err := registry.Open(tempDB(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reg.Close() })

	inf := &mockInferrer{text: "```yaml\nid: ai-flow\nsteps:\n  - id: s1\n    call: t1\n```"}
	ts := httptest.NewServer(server.NewWithInferrer(reg, inf).Handler())
	t.Cleanup(ts.Close)
	addr := strings.TrimPrefix(ts.URL, "http://")

	var buf bytes.Buffer
	root := newRootCmd()
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"ai", "gen", "make a flow", "--addr", addr})
	if err := root.Execute(); err != nil {
		t.Fatalf("ai gen: %v", err)
	}
	if !strings.Contains(buf.String(), "ai-flow") {
		t.Fatalf("输出应含生成的 flow id：%s", buf.String())
	}
}

// TestAIGenRegister 覆盖「ai gen --register 把生成的 Flow 落到本地 registry」分支。
func TestAIGenRegister(t *testing.T) {
	reg, err := registry.Open(tempDB(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reg.Close() })

	inf := &mockInferrer{text: "```yaml\nid: reg-flow\nsteps:\n  - id: s1\n    call: t1\n```"}
	ts := httptest.NewServer(server.NewWithInferrer(reg, inf).Handler())
	t.Cleanup(ts.Close)
	addr := strings.TrimPrefix(ts.URL, "http://")

	cliDB := tempDB(t)
	var buf bytes.Buffer
	root := newRootCmd()
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"ai", "gen", "make a flow", "--addr", addr, "--register", "--registry", cliDB})
	if err := root.Execute(); err != nil {
		t.Fatalf("ai gen --register: %v", err)
	}

	// 断言 flow 已注册进 CLI 自己的 registry
	out, err := registry.Open(cliDB)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	got, err := out.GetFlow("reg-flow")
	if err != nil {
		t.Fatalf("读取注册结果失败: %v", err)
	}
	if got == nil || got.ID != "reg-flow" {
		t.Fatalf("生成的 Flow 未注册成功: %+v", got)
	}
}

// TestAIGenInferError 覆盖「推断失败 → ai gen 报错（不入库、不打印）」。
func TestAIGenInferError(t *testing.T) {
	reg, err := registry.Open(tempDB(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reg.Close() })

	inf := &mockInferrer{err: context.DeadlineExceeded}
	ts := httptest.NewServer(server.NewWithInferrer(reg, inf).Handler())
	t.Cleanup(ts.Close)
	addr := strings.TrimPrefix(ts.URL, "http://")

	var buf bytes.Buffer
	root := newRootCmd()
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"ai", "gen", "make a flow", "--addr", addr})
	if err := root.Execute(); err == nil {
		t.Fatal("推断失败应报错")
	}
}
