package cli

import (
	"testing"

	"gear/internal/model"
	"gear/internal/registry"
)

// TestSeedRefRegistry 验证 runAIGen 的 grounding 机制：把持久 registry 的真实
// 工具/流程灌入内存 refReg，使 agent 经 ctx.ext.registry 读到真实目录（不再由调用方喂 tools）。
func TestSeedRefRegistry(t *testing.T) {
	reg, err := registry.Open(tempDB(t))
	if err != nil {
		t.Fatalf("open registry 失败: %v", err)
	}
	defer reg.Close()
	tl := &model.Tool{
		ID:           "db.query",
		Name:         "db.query",
		Description:  "查询数据库",
		Runtime:      model.RuntimeLua,
		Entry:        "return {}",
		InputSchema:  map[string]interface{}{"type": "object"},
		OutputSchema: map[string]interface{}{"type": "object"},
		Versions:     []model.ToolVersion{{Version: "1.0.0", State: model.StateStable}},
	}
	if err := reg.UpsertTool(tl); err != nil {
		t.Fatalf("注册 Tool 失败: %v", err)
	}
	fl := &model.Flow{ID: "etl", Steps: []model.Step{{ID: "s1", Call: "db.query"}}}
	if err := reg.UpsertFlow(fl); err != nil {
		t.Fatalf("注册 Flow 失败: %v", err)
	}

	refReg, err := registry.Open(tempDB(t))
	if err != nil {
		t.Fatalf("open refReg 失败: %v", err)
	}
	defer refReg.Close()
	if err := seedRefRegistry(refReg, reg); err != nil {
		t.Fatalf("seedRefRegistry 失败: %v", err)
	}
	tools, err := refReg.ListTools()
	if err != nil {
		t.Fatalf("ListTools 失败: %v", err)
	}
	if len(tools) != 1 || tools[0].ID != "db.query" {
		t.Fatalf("refReg 应含 db.query，实际 %v", tools)
	}
	flows, err := refReg.ListFlows()
	if err != nil {
		t.Fatalf("ListFlows 失败: %v", err)
	}
	if len(flows) != 1 || flows[0].ID != "etl" {
		t.Fatalf("refReg 应含 etl，实际 %v", flows)
	}
}
