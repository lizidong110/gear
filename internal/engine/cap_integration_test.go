package engine

import (
	"fmt"
	"testing"

	"gear/internal/cap"
	"gear/internal/model"
)

// capTool 是一个声明了 kv + http 能力的 process Tool，entry 用 cat 把 stdin（含注入的 ctx）原样回显。
func capTool() *model.Tool {
	return &model.Tool{
		ID:          "cap-tool",
		Name:        "cap-tool",
		Runtime:     model.RuntimeProcess,
		Entry:       "cat",
		InputSchema:  map[string]interface{}{"type": "object"},
		OutputSchema: map[string]interface{}{"type": "object"},
		Versions:     []model.ToolVersion{{Version: "1.0.0", State: model.StateStable}},
		// 声明两个 capability；运行期未声明的原语（如 secret/db）不应出现在 ctx 上。
		Capabilities: []string{"kv", "http"},
	}
}

// M5-int-1：process 模式 step 在运行期收到 ctx.capabilities / ctx.sandbox（最小权限声明透传）。
func TestRunFlowInjectsCapabilities(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	if err := reg.UpsertTool(capTool()); err != nil {
		t.Fatalf("注册 Tool 失败: %v", err)
	}
	flow := &model.Flow{
		ID:   "capflow",
		Vars: map[string]interface{}{"topic": "world"},
		Steps: []model.Step{
			{ID: "s1", Call: "cap-tool", Input: map[string]interface{}{"name": "${vars.topic}"}},
		},
	}
	if err := reg.UpsertFlow(flow); err != nil {
		t.Fatalf("注册 Flow 失败: %v", err)
	}

	// 宿主提供 kv 实现（http 走默认 client），不提供 secret/db。
	host := &cap.Providers{KV: cap.NewMemKV()}
	res, err := New(reg, WithHost(host)).RunFlowResult("capflow")
	if err != nil {
		t.Fatalf("RunFlowResult 失败: %v", err)
	}

	stepOut, ok := res.Steps["s1"]["output"].(map[string]interface{})
	if !ok {
		t.Fatalf("step 输出缺失: %v", res.Steps["s1"])
	}
	ctxObj, ok := stepOut["ctx"].(map[string]interface{})
	if !ok {
		t.Fatalf("输出未含 ctx（host 能力未注入）: %v", stepOut)
	}

	caps, ok := ctxObj["capabilities"].([]interface{})
	if !ok {
		t.Fatalf("ctx.capabilities 缺失或类型异常: %v", ctxObj)
	}
	capSet := map[string]bool{}
	for _, c := range caps {
		capSet[fmt.Sprint(c)] = true
	}
	if !capSet["kv"] || !capSet["http"] {
		t.Fatalf("ctx.capabilities 应包含 kv/http，实际: %v", caps)
	}
	if capSet["secret"] || capSet["db"] || capSet["event"] || capSet["call"] {
		t.Fatalf("未声明的 capability 不应透传: %v", caps)
	}
	if fmt.Sprint(ctxObj["sandbox"]) != "false" {
		t.Fatalf("ctx.sandbox 应为 false，实际: %v", ctxObj["sandbox"])
	}
}

// M5-int-2：未配置 WithHost 时，声明了 capability 的 Tool 仍能运行，且**声明**仍透传
// （声明≠实现：实现是否可用由调用方原语自身返回 Unavailable 决定，不影响注入与运行）。
func TestRunFlowNoHostDeclaredCap(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	if err := reg.UpsertTool(capTool()); err != nil {
		t.Fatalf("注册 Tool 失败: %v", err)
	}
	flow := &model.Flow{
		ID:   "capflow2",
		Steps: []model.Step{{ID: "s1", Call: "cap-tool", Input: map[string]interface{}{"name": "x"}}},
	}
	if err := reg.UpsertFlow(flow); err != nil {
		t.Fatalf("注册 Flow 失败: %v", err)
	}
	// 不注入 WithHost —— ctx 仍注入（capabilities 来自声明，与实现无关），原语实现缺失
	// 不影响「声明透传 + 运行不 panic」。
	res, err := New(reg).RunFlowResult("capflow2")
	if err != nil {
		t.Fatalf("未配置 Host 时不应失败: %v", err)
	}
	ctxObj := res.Steps["s1"]["output"].(map[string]interface{})["ctx"].(map[string]interface{})
	caps, ok := ctxObj["capabilities"].([]interface{})
	if !ok || len(caps) != 2 {
		t.Fatalf("未配置 Host 时声明仍应透传（kv/http），实际: %v", ctxObj["capabilities"])
	}
	if fmt.Sprint(ctxObj["sandbox"]) != "false" {
		t.Fatalf("ctx.sandbox 应为 false，实际: %v", ctxObj["sandbox"])
	}
}
