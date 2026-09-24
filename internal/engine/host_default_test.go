package engine

import (
	"fmt"
	"strings"
	"testing"

	"gear/internal/cap"
	"gear/internal/model"
)

// luaToolWithCaps 构造一个声明了指定 capability 的 lua Tool（db/secret/event/call/http）。
func luaToolWithCaps(id string, caps []string, entry string) *model.Tool {
	return &model.Tool{
		ID:           id,
		Name:         id,
		Runtime:      model.RuntimeLua,
		Entry:        entry,
		InputSchema:  map[string]interface{}{"type": "object"},
		OutputSchema: map[string]interface{}{"type": "object"},
		Versions:     []model.ToolVersion{{Version: "1.0.0", State: model.StateStable}},
		Capabilities: caps,
	}
}

// H9-1：默认宿主装配（#69）——未显式 WithHost 时，声明 db 的 Tool 也能真正读写 SQLite，
// 不再恒返回 ErrCapabilityUnavailable。
func TestDefaultHostDBWorks(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	if err := reg.UpsertTool(luaToolWithCaps("dbtool", []string{"db"}, `
		ctx.db.exec({ driver = "sqlite", sql = "CREATE TABLE t(id INTEGER, name TEXT)" })
		ctx.db.exec({ driver = "sqlite", sql = "INSERT INTO t VALUES(1, 'alice')" })
		local rows = ctx.db.query({ driver = "sqlite", sql = "SELECT id, name FROM t" })
		return { count = #rows, first = rows[1].name }
	`)); err != nil {
		t.Fatalf("注册 Tool 失败: %v", err)
	}
	flow := &model.Flow{ID: "dbflow", Steps: []model.Step{{ID: "s1", Call: "dbtool"}}}
	if err := reg.UpsertFlow(flow); err != nil {
		t.Fatalf("注册 Flow 失败: %v", err)
	}

	res, err := New(reg).RunFlowResult("dbflow")
	if err != nil {
		t.Fatalf("RunFlowResult 失败: %v", err)
	}
	out := res.Steps["s1"]["output"].(map[string]interface{})
	if fmt.Sprint(out["first"]) != "alice" {
		t.Fatalf("db 查询首行 name 应为 alice，实为 %v", out["first"])
	}
	if fmt.Sprint(out["count"]) != "1" {
		t.Fatalf("db 查询行数应为 1，实为 %v", out["count"])
	}
}

// H9-2：默认宿主下 event 原语可用（无需 WithHost）。
func TestDefaultHostEventWorks(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	if err := reg.UpsertTool(luaToolWithCaps("evttool", []string{"event"}, `
		ctx.event.notify("e", { n = 1 })
		local p = ctx.event.wait("e")
		return { got = p.n }
	`)); err != nil {
		t.Fatalf("注册 Tool 失败: %v", err)
	}
	flow := &model.Flow{ID: "evtflow", Steps: []model.Step{{ID: "s1", Call: "evttool"}}}
	if err := reg.UpsertFlow(flow); err != nil {
		t.Fatalf("注册 Flow 失败: %v", err)
	}
	res, err := New(reg).RunFlowResult("evtflow")
	if err != nil {
		t.Fatalf("RunFlowResult 失败: %v", err)
	}
	if fmt.Sprint(res.Steps["s1"]["output"].(map[string]interface{})["got"]) != "1" {
		t.Fatalf("event 读写异常")
	}
}

// H9-3：ctx.call 调用另一个已注册 Tool（插件间唯一通信通道，默认宿主即注入解析器）。
func TestCallToolToTool(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	callee := luaToolWithCaps("callee", nil, `return { echo = ctx.input.x .. "!" }`)
	caller := luaToolWithCaps("caller", []string{"call"}, `
		local r = ctx.call("callee", { x = "hi" })
		return { got = r.echo }
	`)
	if err := reg.UpsertTool(callee); err != nil {
		t.Fatalf("注册 callee 失败: %v", err)
	}
	if err := reg.UpsertTool(caller); err != nil {
		t.Fatalf("注册 caller 失败: %v", err)
	}
	flow := &model.Flow{ID: "cf", Steps: []model.Step{{ID: "s1", Call: "caller"}}}
	if err := reg.UpsertFlow(flow); err != nil {
		t.Fatalf("注册 Flow 失败: %v", err)
	}
	res, err := New(reg).RunFlowResult("cf")
	if err != nil {
		t.Fatalf("RunFlowResult 失败: %v", err)
	}
	if fmt.Sprint(res.Steps["s1"]["output"].(map[string]interface{})["got"]) != "hi!" {
		t.Fatalf("call Tool→Tool 返回异常")
	}
}

// H9-4：ctx.call 调用另一个 Flow，返回其 step 输出集合（r.<stepID>.output）。
func TestCallToolToFlow(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	leaf := luaToolWithCaps("leaf", nil, `return { v = 42 }`)
	if err := reg.UpsertTool(leaf); err != nil {
		t.Fatalf("注册 leaf 失败: %v", err)
	}
	sub := &model.Flow{ID: "sub", Steps: []model.Step{{ID: "ls", Call: "leaf"}}}
	if err := reg.UpsertFlow(sub); err != nil {
		t.Fatalf("注册 sub 失败: %v", err)
	}
	caller := luaToolWithCaps("caller", []string{"call"}, `
		local r = ctx.call("sub", {})
		return { v = r.ls.output.v }
	`)
	if err := reg.UpsertTool(caller); err != nil {
		t.Fatalf("注册 caller 失败: %v", err)
	}
	flow := &model.Flow{ID: "cf", Steps: []model.Step{{ID: "s1", Call: "caller"}}}
	if err := reg.UpsertFlow(flow); err != nil {
		t.Fatalf("注册 Flow 失败: %v", err)
	}
	res, err := New(reg).RunFlowResult("cf")
	if err != nil {
		t.Fatalf("RunFlowResult 失败: %v", err)
	}
	if fmt.Sprint(res.Steps["s1"]["output"].(map[string]interface{})["v"]) != "42" {
		t.Fatalf("call Tool→Flow 返回异常")
	}
}

// H9-5：ctx.call 自身（Flow 调用自身）必须被循环守卫拦截，且报错含「循环」，不挂死。
func TestCallFlowRecursionGuarded(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	recall := luaToolWithCaps("recall", []string{"call"}, `
		ctx.call("F2", {})
		return {}
	`)
	if err := reg.UpsertTool(recall); err != nil {
		t.Fatalf("注册 recall 失败: %v", err)
	}
	sub := &model.Flow{ID: "F2", Steps: []model.Step{{ID: "s", Call: "recall"}}}
	if err := reg.UpsertFlow(sub); err != nil {
		t.Fatalf("注册 F2 失败: %v", err)
	}
	_, err := New(reg).RunFlowResult("F2")
	if err == nil {
		t.Fatal("应检测到循环调用并失败")
	}
	if !strings.Contains(err.Error(), "循环") {
		t.Fatalf("应报「循环」调用，实为: %v", err)
	}
}

// H9-6：ctx.call 目标既非 Tool 也非 Flow 时报明确错误（不 panic、不静默成功）。
func TestCallTargetNotFound(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	caller := luaToolWithCaps("caller", []string{"call"}, `
		ctx.call("nope", {})
		return {}
	`)
	if err := reg.UpsertTool(caller); err != nil {
		t.Fatalf("注册 caller 失败: %v", err)
	}
	flow := &model.Flow{ID: "cf", Steps: []model.Step{{ID: "s1", Call: "caller"}}}
	if err := reg.UpsertFlow(flow); err != nil {
		t.Fatalf("注册 Flow 失败: %v", err)
	}
	if _, err := New(reg).RunFlowResult("cf"); err == nil {
		t.Fatal("call 不存在目标应失败")
	}
}

// H9-7：WithHost 显式注入时不会被默认宿主覆盖，但 call 解析器仍被补注入（除非自带）。
func TestWithHostPreservedButResolverInjected(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	// 显式宿主只给 secret，不给 db（验证默认宿主未覆盖它 → db 仍 Unavailable）。
	host := &cap.Providers{Secret: cap.MapSecret{"TOKEN": "x"}}
	eng := New(reg, WithHost(host))
	if eng.host.DB != nil {
		t.Fatalf("显式 WithHost 的 DB 不应被默认宿主覆盖（应保持 nil）")
	}
	if eng.host.Resolver == nil {
		t.Fatalf("call 解析器应被自动注入")
	}
}
