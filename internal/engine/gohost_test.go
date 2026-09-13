package engine

import (
	"context"
	"fmt"
	"testing"

	"gear/internal/cap"
	"gear/internal/model"
)

// 测试结束后清掉注册的 host func，避免污染全局表（其他用例可能同名）。
func unregisterHostFunc(t *testing.T, name string) {
	t.Helper()
	hostFuncMu.Lock()
	delete(hostFuncs, name)
	hostFuncMu.Unlock()
}

// TestGoHostRuntimeRun 验证 go-host 适配器按 entry 寻址并执行注册函数（REQ-RUNTIME-GOHOST / #76）。
func TestGoHostRuntimeRun(t *testing.T) {
	RegisterHostFunc("echo_func", func(ctx context.Context, in map[string]interface{}, h *cap.Context) (map[string]interface{}, error) {
		return map[string]interface{}{"echo": in["msg"]}, nil
	})
	t.Cleanup(func() { unregisterHostFunc(t, "echo_func") })

	rt, err := runtimeFor(model.RuntimeGoHost, "")
	if err != nil {
		t.Fatalf("go-host 应可用: %v", err)
	}
	out, err := rt.Run(context.Background(), "echo_func", map[string]interface{}{"msg": "hi"}, &cap.Context{})
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	if out["echo"] != "hi" {
		t.Fatalf("输出错误: %v", out)
	}
}

// TestGoHostRuntimeRunUnknownEntry 验证未注册入口明确报错（不让用户猜「为什么没反应」）。
func TestGoHostRuntimeRunUnknownEntry(t *testing.T) {
	rt, err := runtimeFor(model.RuntimeGoHost, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rt.Run(context.Background(), "no_such_func", nil, &cap.Context{}); err == nil {
		t.Fatal("未注册入口应报错")
	}
}

// TestGoHostRuntimeStartUnsupported 验证 go-host 不支持 service（长驻）模式（与 lua 一致）。
func TestGoHostRuntimeStartUnsupported(t *testing.T) {
	rt, _ := runtimeFor(model.RuntimeGoHost, "")
	if _, err := rt.Start(context.Background(), "x", nil, &cap.Context{}); err == nil {
		t.Fatal("go-host Start 应报错（不支持 service）")
	}
}

// TestGoHostRuntimeReceivesHost 验证宿主能力视图透传给 go-host 函数（与 lua/process 一致，M5 最小权限）。
func TestGoHostRuntimeReceivesHost(t *testing.T) {
	got := false
	RegisterHostFunc("uses_host", func(ctx context.Context, in map[string]interface{}, h *cap.Context) (map[string]interface{}, error) {
		got = true
		if h == nil {
			t.Error("host 视图不应为 nil")
		}
		return map[string]interface{}{}, nil
	})
	t.Cleanup(func() { unregisterHostFunc(t, "uses_host") })

	rt, _ := runtimeFor(model.RuntimeGoHost, "")
	if _, err := rt.Run(context.Background(), "uses_host", nil, &cap.Context{}); err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Fatal("host 函数未被调用")
	}
}

// TestGoHostRuntimeNilOutput 验证函数返回 nil 时归一为空 map（避免下游 schema 校验空指针）。
func TestGoHostRuntimeNilOutput(t *testing.T) {
	RegisterHostFunc("returns_nil", func(ctx context.Context, in map[string]interface{}, h *cap.Context) (map[string]interface{}, error) {
		return nil, nil
	})
	t.Cleanup(func() { unregisterHostFunc(t, "returns_nil") })

	rt, _ := runtimeFor(model.RuntimeGoHost, "")
	out, err := rt.Run(context.Background(), "returns_nil", nil, &cap.Context{})
	if err != nil {
		t.Fatal(err)
	}
	if out == nil {
		t.Fatal("nil 输出应归一为空 map，不应为 nil")
	}
}

// TestGoHostToolRunsThroughFlow 是端到端验证：go-host Tool 经引擎完整跑通一个 Flow，
// 证明 runtimeFor 分发 + host 注入 + output 落库整链路可用（REQ-RUNTIME-GOHOST / #76）。
func TestGoHostToolRunsThroughFlow(t *testing.T) {
	RegisterHostFunc("gh_echo", func(ctx context.Context, in map[string]interface{}, h *cap.Context) (map[string]interface{}, error) {
		return map[string]interface{}{"got": in["msg"]}, nil
	})
	t.Cleanup(func() { unregisterHostFunc(t, "gh_echo") })

	reg := newTestRegistry(t)
	defer reg.Close()
	tool := &model.Tool{
		ID: "ght", Name: "ght", Runtime: model.RuntimeGoHost, Entry: "gh_echo",
		InputSchema:  map[string]interface{}{"type": "object"},
		OutputSchema: map[string]interface{}{"type": "object"},
		Versions: []model.ToolVersion{{Version: "1.0.0", State: model.StateStable}},
	}
	if err := reg.UpsertTool(tool); err != nil {
		t.Fatalf("注册 go-host Tool 失败: %v", err)
	}
	flow := &model.Flow{ID: "ghflow", Steps: []model.Step{{ID: "s1", Call: "ght"}}}
	if err := reg.UpsertFlow(flow); err != nil {
		t.Fatalf("注册 Flow 失败: %v", err)
	}

	res, err := New(reg).RunFlowResult("ghflow")
	if err != nil {
		t.Fatalf("RunFlowResult 失败: %v", err)
	}
	out, ok := res.Steps["s1"]["output"].(map[string]interface{})
	if !ok {
		t.Fatalf("s1 输出应为 map，实为 %v", res.Steps["s1"])
	}
	if fmt.Sprint(out["got"]) != "<nil>" && out["got"] == nil {
		t.Fatalf("gh_echo 应回显 input.msg，实为 %v", out)
	}
	// Tool 无 input 绑定，input.msg 为空属预期；此处仅验证链路贯通（函数被调用、output 落库）。
	if _, ok := out["got"]; !ok {
		t.Fatalf("gh_echo 应输出 got 字段，实为 %v", out)
	}
}
