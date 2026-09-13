package engine

import (
	"testing"

	"gear/internal/model"
)

func twoVersionTool() *model.Tool {
	return &model.Tool{
		ID: "hello", Name: "hello", Runtime: model.RuntimeProcess,
		InputSchema:  map[string]interface{}{"type": "object"},
		OutputSchema: map[string]interface{}{"type": "object"},
		Versions: []model.ToolVersion{
			{Version: "1.0.0", State: model.StateStable, Entry: `echo '{"v":"1"}'`},
			{Version: "2.0.0", State: model.StateStable, Entry: `echo '{"v":"2"}'`},
		},
	}
}

// S5：step.use_version 解析到版本级 Entry——运行期真正执行被钉版本的 Entry。
func TestRunFlowResolvesUseVersion(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	if err := reg.UpsertTool(twoVersionTool()); err != nil {
		t.Fatal(err)
	}
	flow := &model.Flow{ID: "f", Steps: []model.Step{{ID: "s1", Call: "hello", UseVersion: "2.0.0"}}}
	if err := reg.UpsertFlow(flow); err != nil {
		t.Fatal(err)
	}
	res, err := New(reg).RunFlowResult("f")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	out, ok := res.Steps["s1"]["output"].(map[string]interface{})
	if !ok {
		t.Fatalf("output 类型异常: %T", res.Steps["s1"]["output"])
	}
	if out["v"] != "2" {
		t.Fatalf("use_version=2.0.0 应执行 v2 的 Entry，实际 %v", out)
	}
}

// S5：不钉版本时默认用 Tool 级 Entry（版本侧缺省则回退 Tool 级）。
func TestRunFlowDefaultVersion(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	// Tool 级 Entry 与版本 Entry 不同：验证「不钉版本走 Tool 级」。
	tl := twoVersionTool()
	tl.Entry = `echo '{"v":"tool"}'`
	if err := reg.UpsertTool(tl); err != nil {
		t.Fatal(err)
	}
	flow := &model.Flow{ID: "f", Steps: []model.Step{{ID: "s1", Call: "hello"}}}
	if err := reg.UpsertFlow(flow); err != nil {
		t.Fatal(err)
	}
	res, err := New(reg).RunFlowResult("f")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	out, ok := res.Steps["s1"]["output"].(map[string]interface{})
	if !ok {
		t.Fatalf("output 类型异常: %T", res.Steps["s1"]["output"])
	}
	if out["v"] != "tool" {
		t.Fatalf("未钉版本应走 Tool 级 Entry，实际 %v", out)
	}
}

// S5：ValidateFlowStatic 拒绝引用不存在的 use_version（注册期暴露，而非运行期崩溃）。
func TestValidateFlowStaticUnknownVersion(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	if err := reg.UpsertTool(twoVersionTool()); err != nil {
		t.Fatal(err)
	}
	flow := &model.Flow{ID: "f", Steps: []model.Step{{ID: "s1", Call: "hello", UseVersion: "9.9.9"}}}
	if err := New(reg).ValidateFlowStatic(flow); err == nil {
		t.Fatal("未知 use_version 应被注册期拒绝")
	}
}

// S5：PromoteTool 触发 before_promote Hook 门禁——critical Hook 失败则中止晋升且不改状态。
func TestPromoteToolBeforePromoteGate(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()

	// 被晋升的 Tool（draft）
	if err := reg.UpsertTool(&model.Tool{
		ID: "prom", Runtime: model.RuntimeProcess, Entry: "cat",
		InputSchema: map[string]interface{}{"type": "object"}, OutputSchema: map[string]interface{}{"type": "object"},
		Versions: []model.ToolVersion{{Version: "1.0.0", State: model.StateDraft}},
	}); err != nil {
		t.Fatal(err)
	}

	// 通过门禁：critical Hook 成功执行
	if err := reg.UpsertTool(&model.Tool{
		ID: "ok-tool", Runtime: model.RuntimeProcess, Entry: `echo '{"ok":true}'`,
		InputSchema: map[string]interface{}{"type": "object"}, OutputSchema: map[string]interface{}{"type": "object"},
		Versions: []model.ToolVersion{{Version: "1.0.0", State: model.StateStable}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := reg.UpsertHook(&model.Hook{ID: "gate-ok", Target: "prom", Stage: model.StageBeforePromote, Action: "ok-tool", Criticality: model.CritCritical}); err != nil {
		t.Fatal(err)
	}
	if err := New(reg).PromoteTool("prom", "1.0.0", model.StateTesting); err != nil {
		t.Fatalf("门禁通过时应晋升成功: %v", err)
	}
	got, _ := reg.GetTool("prom")
	if got.Versions[0].State != model.StateTesting {
		t.Fatalf("未晋升: %s", got.Versions[0].State)
	}

	// 失败门禁：critical Hook 退出码非 0 → 中止晋升
	if err := reg.UpsertTool(&model.Tool{
		ID: "bad-tool", Runtime: model.RuntimeProcess, Entry: `echo denied >&2; exit 1`,
		InputSchema: map[string]interface{}{"type": "object"}, OutputSchema: map[string]interface{}{"type": "object"},
		Versions: []model.ToolVersion{{Version: "1.0.0", State: model.StateStable}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := reg.UpsertHook(&model.Hook{ID: "gate-bad", Target: "prom", Stage: model.StageBeforePromote, Action: "bad-tool", Criticality: model.CritCritical}); err != nil {
		t.Fatal(err)
	}
	if err := New(reg).PromoteTool("prom", "1.0.0", model.StateStable); err == nil {
		t.Fatal("critical 门禁失败时应中止晋升")
	}
	got, _ = reg.GetTool("prom")
	if got.Versions[0].State != model.StateTesting {
		t.Fatalf("中止晋升不应改变状态: %s", got.Versions[0].State)
	}
}
