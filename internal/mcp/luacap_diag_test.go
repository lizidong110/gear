package mcp

// luacap_diag_test.go —— 「Lua 用了却没声明 capability」在 MCP 写作期报什么码。
//
// 为什么单独锁：这条守卫接在 model.Tool.Validate 上（所有注册路径共用），但**对外**还要
// 看诊断码对不对。码错了智能体就会按错的 hint 去改（这是 MCP 面的核心契约：
// 靠码自愈，而不是靠猜）。BAD_TOOL_STRUCTURE 与 LUA_CAP_UNDECLARED 的修法完全不同，
// 混在一起会让智能体反复改「字段表」而真正的答案就在同一段 Tool YAML 里。

import (
	"strings"
	"testing"

	"gear/internal/engine"
)

// toolBundleYAML 造一个最小 bundle：Lua Tool 用了 ctx.db，没声明 capabilities。
func toolBundleYAML(entry string, caps string) string {
	capsLine := ""
	if caps != "" {
		capsLine = "\ncapabilities: [" + caps + "]"
	}
	return `
kind: Tool
id: luacapdemo__probe
name: probe
description: 探针
runtime: lua
entry: |
` + indentLines(entry) + capsLine + `
input_schema: {type: object}
output_schema: {type: object}
versions: [{version: "1.0.0", state: stable}]
`
}

func indentLines(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = "  " + l
	}
	return strings.Join(lines, "\n")
}

// TestValidateReportsLuaCapUndeclaredWithDedicatedCode：缺声明 → 专属码 + 可自愈的 hint。
func TestValidateReportsLuaCapUndeclaredWithDedicatedCode(t *testing.T) {
	s := newSmokeFixture(t)
	res := s.server.validateBundle(toolBundleYAML(`local r = ctx.db.query({ sql = "select 1" })
return r`, ""))

	if res.Valid() {
		t.Fatal("用了 ctx.db 却没声明 capabilities，写作期就该判非法")
	}
	found := false
	for _, d := range res.Diags {
		if d.Code == engine.CodeLuaCapUndeclared {
			found = true
			if d.Field != "tool(luacapdemo__probe).capabilities" {
				t.Errorf("Field 应指向 capabilities，实际 %q", d.Field)
			}
			// hint 必须说清「加声明」而不是「对照字段表」——后者会把智能体引到错的地方。
			if !strings.Contains(d.Hint, "capabilities") {
				t.Errorf("hint 没指向 capabilities: %s", d.Hint)
			}
			// message 里要点名能力与行号（读者据此能直接定位）。
			if !strings.Contains(d.Message, "db") || !strings.Contains(d.Message, "entry.lua:1") {
				t.Errorf("message 应含能力名与行号: %s", d.Message)
			}
		}
	}
	if !found {
		t.Fatalf("应报 %s，实际诊断: %+v", engine.CodeLuaCapUndeclared, res.Diags)
	}
}

// TestValidateAcceptsDeclaredLuaCap 是**反向**断言：声明齐了必须 valid。
// 守卫最容易犯的错是「宁可多拒」——那会把合法应用也挡住，逼用户乱加声明。
func TestValidateAcceptsDeclaredLuaCap(t *testing.T) {
	s := newSmokeFixture(t)
	res := s.server.validateBundle(toolBundleYAML(`local r = ctx.db.query({ sql = "select 1" })
return r`, "db"))
	if !res.Valid() {
		t.Fatalf("声明齐了却被判非法: %+v", res.Diags)
	}
}

// TestValidatePureFuncsNeedNoDeclaration 锁一条口径：纯函数原子无需声明，
// 不得报 LUA_CAP_UNDECLARED（更不得报「声明了纯函数」——那是另一条守卫）。
func TestValidatePureFuncsNeedNoDeclaration(t *testing.T) {
	s := newSmokeFixture(t)
	res := s.server.validateBundle(toolBundleYAML(`return { t = ctx.time.now({ format = "rfc3339" }) }`, ""))
	if !res.Valid() {
		t.Fatalf("纯函数原子不该要求声明: %+v", res.Diags)
	}
}

// TestValidateOtherToolErrorsKeepBadToolStructure：确保这个新分支**没有**吃掉别的错误——
// 字段写错仍然报 BAD_TOOL_STRUCTURE（拆错码就会让这类问题被误当成「加声明就行」）。
func TestValidateOtherToolErrorsKeepBadToolStructure(t *testing.T) {
	s := newSmokeFixture(t)
	// runtime 非法（与 capability 无关）。
	res := s.server.validateBundle(`
kind: Tool
id: luacapdemo__bad
name: bad
description: 字段写错
runtime: nosuchruntime
entry: "echo hi"
input_schema: {type: object}
output_schema: {type: object}
versions: [{version: "1.0.0", state: stable}]
`)
	if res.Valid() {
		t.Fatal("runtime 非法应判非法")
	}
	sawStructure := false
	for _, d := range res.Diags {
		if d.Code == engine.CodeBadToolStructure {
			sawStructure = true
		}
		if d.Code == engine.CodeLuaCapUndeclared {
			t.Errorf("runtime 错误不该报 %s", engine.CodeLuaCapUndeclared)
		}
	}
	if !sawStructure {
		t.Fatalf("runtime 错误应报 %s，实际: %+v", engine.CodeBadToolStructure, res.Diags)
	}
}
