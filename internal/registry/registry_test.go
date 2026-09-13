package registry

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"gopkg.in/yaml.v3"

	"gear/internal/model"
)

func mustOpen(t *testing.T) *Registry {
	t.Helper()
	path := filepath.Join(t.TempDir(), "registry.db")
	reg, err := Open(path)
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	return reg
}

func validTool(id string) *model.Tool {
	return &model.Tool{
		ID: id, Name: id, Description: "t", Runtime: model.RuntimeProcess, Entry: "cat",
		InputSchema:  map[string]interface{}{"type": "object"},
		OutputSchema: map[string]interface{}{"type": "object"},
		Versions:     []model.ToolVersion{{Version: "1.0.0", State: model.StateStable, Note: "init"}},
	}
}

// T-reg-01：UpsertTool/GetTool/ListTools 落库往返 + JSON blob 完整。
func TestToolRoundtrip(t *testing.T) {
	reg := mustOpen(t)
	defer reg.Close()

	in := validTool("hello")
	in.Versions[0].Note = "初始版本"
	if err := reg.UpsertTool(in); err != nil {
		t.Fatalf("UpsertTool 失败: %v", err)
	}

	got, err := reg.GetTool("hello")
	if err != nil {
		t.Fatalf("GetTool 失败: %v", err)
	}
	if got == nil {
		t.Fatal("GetTool 返回 nil")
	}
	if got.ID != "hello" || got.Name != "hello" || got.Runtime != model.RuntimeProcess {
		t.Fatalf("字段丢失: %+v", got)
	}
	if len(got.Versions) != 1 || got.Versions[0].Version != "1.0.0" || got.Versions[0].State != model.StateStable || got.Versions[0].Note != "初始版本" {
		t.Fatalf("version blob 不完整: %+v", got.Versions)
	}
	if got.InputSchema == nil || fmt.Sprint(got.InputSchema["type"]) != "object" {
		t.Fatalf("input_schema blob 不完整: %+v", got.InputSchema)
	}

	all, err := reg.ListTools()
	if err != nil {
		t.Fatalf("ListTools 失败: %v", err)
	}
	if len(all) != 1 || all[0].ID != "hello" {
		t.Fatalf("ListTools 结果不符: %+v", all)
	}
}

// T-reg-02：register 拒绝非法 version / 非法 state / 缺 schema（D2/D5 最小校验）。
func TestRegisterRejectsInvalid(t *testing.T) {
	reg := mustOpen(t)
	defer reg.Close()

	t.Run("非法 version", func(t *testing.T) {
		tl := validTool("bad")
		tl.Versions[0].Version = "v1"
		if err := reg.UpsertTool(tl); err == nil {
			t.Fatal("v1 应被拒绝")
		}
	})
	t.Run("非法 state", func(t *testing.T) {
		tl := validTool("bad")
		tl.Versions[0].State = model.ToolVersionState("bogus")
		if err := reg.UpsertTool(tl); err == nil {
			t.Fatal("bogus state 应被拒绝")
		}
	})
	t.Run("缺 input schema", func(t *testing.T) {
		tl := validTool("bad")
		tl.InputSchema = nil
		if err := reg.UpsertTool(tl); err == nil {
			t.Fatal("缺 input_schema 应被拒绝")
		}
	})
	t.Run("合法应通过", func(t *testing.T) {
		if err := reg.UpsertTool(validTool("ok")); err != nil {
			t.Fatalf("合法 Tool 应入库: %v", err)
		}
	})
}

// T-reg-03：同 id 并发 upsert 在 -race 下不损坏（D6）。
func TestConcurrentUpsert(t *testing.T) {
	reg := mustOpen(t)
	defer reg.Close()

	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tl := validTool("concurrent")
			tl.Name = fmt.Sprintf("v%d", i)
			_ = reg.UpsertTool(tl)
		}(i)
	}
	wg.Wait()

	got, err := reg.GetTool("concurrent")
	if err != nil {
		t.Fatalf("并发后 GetTool 失败: %v", err)
	}
	if got == nil {
		t.Fatal("并发 upsert 后 tool 丢失（数据损坏）")
	}
	if got.ID != "concurrent" {
		t.Fatalf("数据损坏: id=%q", got.ID)
	}
	if got.InputSchema == nil {
		t.Fatal("并发 upsert 后 schema blob 损坏")
	}
}

// T-s0-example：加载 examples/hello_flow.yaml，
// 断言变量引用是 ${vars.topic} 而非 {{...}}（锁死 D1 回归）。
func TestExampleFlowUsesDollarBrace(t *testing.T) {
	path := filepath.Join("..", "..", "examples", "hello_flow.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取示例失败: %v", err)
	}
	var f model.Flow
	if err := yaml.Unmarshal(raw, &f); err != nil {
		t.Fatalf("解析示例失败: %v", err)
	}
	if len(f.Steps) == 0 {
		t.Fatal("示例 Flow 无 steps")
	}
	joined := string(raw)
	if containsDoubleBrace(joined) {
		t.Fatalf("示例仍使用 {{...}} 模板语法，违反 PRD §4.3：\n%s", joined)
	}
	if !containsDollarBrace(joined) {
		t.Fatalf("示例未使用 ${vars.x} 模板语法（PRD §4.3）：\n%s", joined)
	}
}

func containsDoubleBrace(s string) bool {
	return find(s, "{{") >= 0
}

func containsDollarBrace(s string) bool {
	return find(s, "${") >= 0
}

func find(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// T-reg-04：Flow 的 Upsert/Get/List 落库往返。
func TestFlowRoundtrip(t *testing.T) {
	reg := mustOpen(t)
	defer reg.Close()

	in := &model.Flow{ID: "demo", Name: "Demo", Description: "两步",
		Vars: map[string]interface{}{"topic": "world"},
		Steps: []model.Step{{ID: "s1", Call: "hello", Input: map[string]interface{}{"name": "${vars.topic}"}}}}
	if err := reg.UpsertFlow(in); err != nil {
		t.Fatalf("UpsertFlow 失败: %v", err)
	}

	got, err := reg.GetFlow("demo")
	if err != nil {
		t.Fatalf("GetFlow 失败: %v", err)
	}
	if got == nil || got.ID != "demo" || len(got.Steps) != 1 || got.Steps[0].ID != "s1" {
		t.Fatalf("Flow 字段丢失: %+v", got)
	}
	if fmt.Sprint(got.Vars["topic"]) != "world" {
		t.Fatalf("Flow.vars 丢失: %+v", got.Vars)
	}

	all, err := reg.ListFlows()
	if err != nil {
		t.Fatalf("ListFlows 失败: %v", err)
	}
	if len(all) != 1 || all[0].ID != "demo" {
		t.Fatalf("ListFlows 结果不符: %+v", all)
	}
}

// T-reg-05：Hook 的 Upsert/List 落库往返。
func TestHookRoundtrip(t *testing.T) {
	reg := mustOpen(t)
	defer reg.Close()

	in := &model.Hook{ID: "h1", Target: "*", Stage: "before_step", Action: "auth",
		Criticality: model.CritCritical, Order: 10}
	if err := reg.UpsertHook(in); err != nil {
		t.Fatalf("UpsertHook 失败: %v", err)
	}

	all, err := reg.ListHooks()
	if err != nil {
		t.Fatalf("ListHooks 失败: %v", err)
	}
	if len(all) != 1 || all[0].ID != "h1" || all[0].Criticality != model.CritCritical || all[0].Order != 10 {
		t.Fatalf("Hook 字段丢失: %+v", all)
	}

	// GetHook
	got, err := reg.GetHook("h1")
	if err != nil || got == nil || got.Action != "auth" {
		t.Fatalf("GetHook 异常: h=%+v err=%v", got, err)
	}
	if miss, err := reg.GetHook("nope"); err != nil || miss != nil {
		t.Fatalf("GetHook 不存在应返回 (nil,nil)，实际 h=%+v err=%v", miss, err)
	}

	// UpsertHook 拒绝非法（F3）
	bad := &model.Hook{ID: "bad", Target: "*", Stage: "whenever", Action: "a"}
	if err := reg.UpsertHook(bad); err == nil {
		t.Fatal("非法 stage 的 Hook 应被拒绝")
	}
	// before_promote 自 S5 起允许注册（由 engine.PromoteTool 触发）
	ok2 := &model.Hook{ID: "ok2", Target: "*", Stage: model.StageBeforePromote, Action: "a"}
	if err := reg.UpsertHook(ok2); err != nil {
		t.Fatalf("before_promote Hook 应允许注册（S5 已实现）: %v", err)
	}

	// DeleteHook
	if err := reg.DeleteHook("h1"); err != nil {
		t.Fatalf("DeleteHook 失败: %v", err)
	}
	if err := reg.DeleteHook("h1"); err == nil {
		t.Fatal("删除不存在的 Hook 应报错（不静默 no-op）")
	}
}

// T-reg-07：DeleteTool 的注销守卫（PRD §8.2）——
// 不存在 → ErrNotFound；被 Flow/Hook 引用 → ErrInUse；无引用 → 删除成功。
func TestDeleteTool(t *testing.T) {
	reg := mustOpen(t)
	defer reg.Close()

	// 1) 不存在
	if err := reg.DeleteTool("ghost"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("删不存在的 Tool 应返回 ErrNotFound，实际 %v", err)
	}

	// 2) 无引用 → 成功
	if err := reg.UpsertTool(validTool("solo")); err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	if err := reg.DeleteTool("solo"); err != nil {
		t.Fatalf("无引用的 Tool 应可删除: %v", err)
	}
	if got, _ := reg.GetTool("solo"); got != nil {
		t.Fatal("删除后不应再读到")
	}

	// 3) 被 Flow 的 step 引用 → ErrInUse
	if err := reg.UpsertTool(validTool("refd")); err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	if err := reg.UpsertFlow(&model.Flow{ID: "f", Steps: []model.Step{
		{ID: "s1", Call: "refd", Input: map[string]interface{}{}},
	}}); err != nil {
		t.Fatalf("注册 Flow 失败: %v", err)
	}
	if err := reg.DeleteTool("refd"); !errors.Is(err, ErrInUse) {
		t.Fatalf("被 Flow 引用的 Tool 应返回 ErrInUse，实际 %v", err)
	}

	// 4) 被 Hook 的 action 引用 → ErrInUse
	if err := reg.UpsertTool(validTool("hooked")); err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	if err := reg.UpsertHook(&model.Hook{ID: "h", Target: "*", Stage: model.StageBeforeStep, Action: "hooked"}); err != nil {
		t.Fatalf("注册 Hook 失败: %v", err)
	}
	if err := reg.DeleteTool("hooked"); !errors.Is(err, ErrInUse) {
		t.Fatalf("被 Hook 引用的 Tool 应返回 ErrInUse，实际 %v", err)
	}
}

// T-reg-06：不存在的 id，Get 返回 (nil, nil) 而非错误（ErrNoRows→nil 分支）。
func TestGetNotFound(t *testing.T) {
	reg := mustOpen(t)
	defer reg.Close()

	tl, err := reg.GetTool("nope")
	if err != nil {
		t.Fatalf("GetTool 不存在应返回 nil 错误: %v", err)
	}
	if tl != nil {
		t.Fatalf("GetTool 不存在应返回 nil tool")
	}
	fl, err := reg.GetFlow("nope")
	if err != nil {
		t.Fatalf("GetFlow 不存在应返回 nil 错误: %v", err)
	}
	if fl != nil {
		t.Fatalf("GetFlow 不存在应返回 nil flow")
	}
}

// T-reg-07：§8.2 第三条守卫——「正被运行中的执行使用」时拒绝注销。
//
// 为什么需要它：静态引用守卫（Flow step / Hook action）会在 Hook 被删、Flow 被改写后失效，
// 而正在跑的执行仍持有启动时的快照。运行期引用只能由上层（server）注入的检查器提供。
func TestDeleteToolRunningGuard(t *testing.T) {
	reg := mustOpen(t)
	defer reg.Close()
	if err := reg.UpsertTool(validTool("live")); err != nil {
		t.Fatalf("注册失败: %v", err)
	}

	// 1) 未注入检查器 → 不做运行中校验，可注销。
	if err := reg.DeleteTool("live"); err != nil {
		t.Fatalf("未启用运行中守卫时应可注销: %v", err)
	}

	// 2) 注入「在运行」的检查器 → 拒绝，且必须是 ErrInUse（供 HTTP 映射 409）。
	if err := reg.UpsertTool(validTool("live")); err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	reg.SetRunningChecker(func(toolID string) string {
		if toolID == "live" {
			return "正被运行中的执行 exec_1 使用"
		}
		return ""
	})
	err := reg.DeleteTool("live")
	if !errors.Is(err, ErrInUse) {
		t.Fatalf("运行中的 Tool 应返回 ErrInUse，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "exec_1") {
		t.Fatalf("拒绝原因应指出是哪个执行在用，实际: %v", err)
	}
	if tl, _ := reg.GetTool("live"); tl == nil {
		t.Fatal("被拒绝后 Tool 不应被删除")
	}

	// 3) 检查器报告「不在运行」→ 恢复可注销（守卫只约束运行中，不能永久锁死）。
	reg.SetRunningChecker(func(string) string { return "" })
	if err := reg.DeleteTool("live"); err != nil {
		t.Fatalf("不在运行时应可注销: %v", err)
	}
}
