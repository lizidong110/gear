package cli

import (
	"net"
	"strings"
	"testing"
	"time"
)

// 本文件补两条「会常驻」的命令路径：app run 与 serve。
// 它们的风险恰恰在于**卡住**：端口起不来却静默成功、或者监管流程永不返回，
// 用户看到的是「命令没报错但服务不在」。故用例都带超时兜底，且断言「失败要报错」。

func TestAppRunReportsUnknownFlow(t *testing.T) {
	withTempRegistry(t)
	err := runCLI(t, "app", "run", "no_such_flow")
	if err == nil {
		t.Fatal("监管不存在的 Flow 必须报错（不静默「监管 0 个」）")
	}
}

// TestAppRunSupervisesThenReturns：非 service 的普通 Flow 跑完即退出，
// `app run` 应正常收尾并打印停止（若它不返回，说明 Supervise 没有正确结束条件）。
func TestAppRunSupervisesThenReturns(t *testing.T) {
	withTempRegistry(t)
	if err := runCLI(t, "tool", "register", writeTempYAML(t, luaTool)); err != nil {
		t.Fatalf("注册 Tool 失败: %v", err)
	}
	if err := runCLI(t, "flow", "register", writeTempYAML(t, `id: once
name: once
description: 一次性 Flow（无 service step）
steps:
  - id: s1
    call: lua.echo
    input:
      name: hi
`)); err != nil {
		t.Fatalf("注册 Flow 失败: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- runCLI(t, "app", "run", "once", "--restart", "never") }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("app run 应在 Flow 跑完后正常收尾，实际 %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("app run 未返回（Supervise 缺少结束条件 → 进程永久残留）")
	}
}

// TestServeReportsBindFailure：`gear serve` 端口被占用时必须报错退出，
// 绝不「监听失败但退出码 0」——那会让调用方以为 REST 面已经起来。
func TestServeReportsBindFailure(t *testing.T) {
	withTempRegistry(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("占端口失败: %v", err)
	}
	defer ln.Close()

	err = runCLI(t, "serve", "--addr", ln.Addr().String())
	if err == nil {
		t.Fatal("端口已被占用时 gear serve 必须报错退出")
	}
	if !strings.Contains(err.Error(), ln.Addr().String()) || strings.TrimSpace(err.Error()) == "" {
		t.Fatalf("错误信息应说明是哪个地址起不来，实际 %q", err)
	}
}

// TestEmptyListCommandsPrintPlaceholder：空目录时各 list 应打印占位提示并成功退出
// （空不是错误，不能把「没有数据」当失败）。
func TestEmptyListCommandsPrintPlaceholder(t *testing.T) {
	withTempRegistry(t)
	// 注：exec list 不在其中——它是 REST 客户端子命令，需要执行面在跑（由 exec_test.go
	// 起 httptest 服务端覆盖），空目录不是它的「无数据」场景。
	for _, args := range [][]string{
		{"flow", "list"},
		{"hook", "list"},
		{"tool", "list"},
	} {
		if err := runCLI(t, args...); err != nil {
			t.Fatalf("%v 在空目录下应成功退出，实际 %v", args, err)
		}
	}
}

// TestFlowRunWithVars：flow run 支持 --var 覆盖 Flow 的 vars（模板 ${vars.x} 引用）。
func TestFlowRunWithVars(t *testing.T) {
	withTempRegistry(t)
	if err := runCLI(t, "tool", "register", writeTempYAML(t, luaTool)); err != nil {
		t.Fatalf("注册 Tool 失败: %v", err)
	}
	if err := runCLI(t, "flow", "register", writeTempYAML(t, `id: varsflow
name: vars flow
description: 带变量的 Flow
vars:
  who: world
steps:
  - id: s1
    call: lua.echo
    input:
      name: "${vars.who}"
`)); err != nil {
		t.Fatalf("注册 Flow 失败: %v", err)
	}
	if err := runCLI(t, "flow", "run", "varsflow", "--var", "who=gear"); err != nil {
		t.Fatalf("flow run 失败: %v", err)
	}
	// 不存在的 Flow 必须报错
	if err := runCLI(t, "flow", "run", "no_such_flow"); err == nil {
		t.Fatal("运行不存在的 Flow 必须报错")
	}
}
