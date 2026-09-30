package mcp

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestMCPValidateReportsRouteDiagnostics 锁住 FR-3 的**接口面**：
// `gear_validate` 对 `api:` 的形状错误给**字段级**诊断，对撞名给点名两个 Tool 的诊断，
// 且后者连 `gear_apply` 都过不去（AC-C2 / AC-C3）。
func TestMCPValidateReportsRouteDiagnostics(t *testing.T) {
	f := newSmokeFixture(t)
	session := f.session

	const bundle = `kind: App
id: route
entry_flow: route.up
---
kind: Tool
id: route__bad_method
name: Bad method
description: api.method 小写
runtime: lua
entry: "return {}"
input_schema: {type: object}
output_schema: {type: object}
versions: [{version: "1.0.0", state: stable}]
api:
  method: get
  path: /api/orders
---
kind: Tool
id: route__dupe_a
name: A
description: 与 B 撞同一路由
runtime: lua
entry: "return {}"
input_schema: {type: object}
output_schema: {type: object}
versions: [{version: "1.0.0", state: stable}]
api:
  method: GET
  path: /api/orders
---
kind: Tool
id: route__dupe_b
name: B
description: 与 A 撞同一路由
runtime: lua
entry: "return {}"
input_schema: {type: object}
output_schema: {type: object}
versions: [{version: "1.0.0", state: stable}]
api:
  method: GET
  path: /api/orders
---
kind: Flow
id: route.up
steps:
  - id: s1
    call: route__dupe_a
`
	var out map[string]any
	callTool(t, session, "gear_validate", map[string]any{"yaml": bundle}, &out)
	if out["valid"] != false {
		t.Fatalf("坏 bundle 应判 invalid: %v", out)
	}
	raw, _ := json.Marshal(out["diagnostics"])
	diags := string(raw)
	// 三条：小写 method、两条撞名（点名两个 Tool）。
	for _, want := range []string{
		"ROUTE_INVALID",
		"tool(route__bad_method).api.method",
		"ROUTE_CONFLICT",
		"tool(route__dupe_b).api", // 冲突诊断挂在「后一个」上，字段路径同样要可机器定位
		"route__dupe_a",           // 诊断必须点名冲突的**两个** Tool，只报一个等于没给修法
		"route__dupe_b",
	} {
		if !strings.Contains(diags, want) {
			t.Fatalf("诊断里应有 %q，实际: %s", want, diags)
		}
	}

	// AC-C3：apply 的原子语义不变——校验不过就一个字节都不写。
	var applied map[string]any
	callTool(t, session, "gear_apply", map[string]any{"bundle": bundle}, &applied)
	if applied["valid"] != false {
		t.Fatalf("冲突 bundle 不该 apply 成功: %v", applied)
	}
	if got, _ := applied["applied"].(map[string]any); got != nil {
		if tools, _ := got["tools"].([]any); len(tools) != 0 {
			t.Fatalf("冲突 bundle 不该写入任何 Tool，实际 %v", tools)
		}
	}
}
