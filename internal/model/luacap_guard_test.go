package model

// luacap_guard_test.go —— Tool 注册期「Lua entry 用了却没声明的 capability」的守卫。
//
// 与 internal/luacap 里的扫描器测试分工：那边验「扫得准不准」，这里验「**接上了没有**」。
// 扫描器再准，没接进注册路径也只是个没人调用的函数——而这正是本条需求最初的状态。

import (
	"strings"
	"testing"
)

func luaTool(entry string, caps ...string) Tool {
	return Tool{
		ID: "t1", Runtime: RuntimeLua, Entry: entry, Capabilities: caps,
		InputSchema:  map[string]interface{}{"type": "object"},
		OutputSchema: map[string]interface{}{"type": "object"},
		Versions:     []ToolVersion{{Version: "1.0.0", State: StateStable}},
	}
}

// TestToolValidateRejectsUndeclaredCapabilityInLua 是本条需求的主断言。
func TestToolValidateRejectsUndeclaredCapabilityInLua(t *testing.T) {
	err := luaTool(`local r = ctx.db.query({ sql = "select 1" })
return r`).Validate()
	if err == nil {
		t.Fatal("用了 ctx.db 却没声明 capabilities: [db]，注册期就该拒")
	}
	msg := err.Error()
	// 报错必须能自愈：说清哪个能力、脚本第几行、怎么改。
	for _, want := range []string{"db", "entry.lua:1", "capabilities: [db]"} {
		if !strings.Contains(msg, want) {
			t.Errorf("报错缺 %q: %s", want, msg)
		}
	}
}

// TestToolValidateAcceptsProperlyDeclaredLua 是**反向**断言：声明齐了必须放行。
// 守卫最容易出的错是「宁可多拒」——那会把合法应用也挡住，用户被迫乱加声明。
func TestToolValidateAcceptsProperlyDeclaredLua(t *testing.T) {
	if err := luaTool(`local r = ctx.db.query({ sql = "select 1" })
return r`, "db").Validate(); err != nil {
		t.Fatalf("声明齐了却被拒: %v", err)
	}
	// 多声明（最小权限之外的冗余）不拦——那是作者的取舍，不是错误。
	if err := luaTool(`return 1`, "db", "http", "secret").Validate(); err != nil {
		t.Fatalf("多声明不该被拒: %v", err)
	}
}

// TestToolValidateIgnoresPureFuncsInLua 锁一条口径：纯函数原子始终可用，
// **不得**因为「用了没声明」而报错（声明了反而会被拒——那是另一条守卫）。
func TestToolValidateIgnoresPureFuncsInLua(t *testing.T) {
	tool := luaTool(`local t = ctx.time.now({ format = "rfc3339" })
local h = ctx.crypto.hash({ algo = "sha256", data = "x" })
return { t = t, h = h }`)
	if err := tool.Validate(); err != nil {
		t.Fatalf("纯函数原子不该要求声明: %v", err)
	}
}

// TestToolValidateSkipsNonLuaRuntime：process runtime 的 entry 是命令行，
// 与 ctx 无关——拿它去扫会把命令行里的路径/参数误判成能力使用。
func TestToolValidateSkipsNonLuaRuntime(t *testing.T) {
	tool := luaTool("", "db")
	tool.Runtime = RuntimeProcess
	tool.Entry = "db-query --table users" // 看着像 ctx.db 但根本不是 Lua
	if err := tool.Validate(); err != nil {
		t.Fatalf("process runtime 不该被扫: %v", err)
	}
}

// TestToolValidateIgnoresLuaSyntaxError：脚本语法错误时守卫**不越权报错**——
// 语法错误由 Lua VM 在运行期报一条更准的（含出错位置），这里报第二条只会让人困惑。
func TestToolValidateIgnoresLuaSyntaxError(t *testing.T) {
	tool := luaTool("local x = = = end end", "db")
	if err := tool.Validate(); err != nil {
		t.Fatalf("语法错误不该由本守卫报: %v", err)
	}
}
