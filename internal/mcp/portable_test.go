package mcp

import (
	"testing"

	"gear/internal/engine"
)

// TestValidateRejectsHardcodedAbsolutePath：把「可移植性」检查前移到写作期——
// 智能体经 MCP 编写的应用若硬编码了机器绝对路径，gear_validate 必须立即报
// PORTABLE_PATH_HARDCODED 且判 valid:false，而不是等到发布后拷贝到别的机器才炸。
func TestValidateRejectsHardcodedAbsolutePath(t *testing.T) {
	f := newSmokeFixture(t)
	yaml := `kind: Tool
id: bad__tool
name: Bad
description: 硬编码绝对路径
runtime: lua
entry: |
  local DSN = "D:/gadern/gear/build/bad.db"
  return { ok = true }
input_schema: {type: object}
output_schema: {type: object}
versions:
  - version: "1.0.0"
    state: stable
`
	var out struct {
		Valid       bool                `json:"valid"`
		Diagnostics []engine.Diagnostic `json:"diagnostics"`
	}
	callTool(t, f.session, "gear_validate", map[string]any{"yaml": yaml}, &out)
	if out.Valid {
		t.Fatalf("含机器绝对路径的 bundle 应判 invalid: %v", out.Diagnostics)
	}
	found := false
	for _, d := range out.Diagnostics {
		if d.Code == engine.CodePortablePathHardcoded {
			found = true
		}
	}
	if !found {
		t.Fatalf("应报 %s，实际诊断 %+v", engine.CodePortablePathHardcoded, out.Diagnostics)
	}
}

// TestValidateAcceptsPortableBundle：相对写法（锚点）必须零诊断——
// 否则这条检查会变成噪声，智能体很快学会无视它。
func TestValidateAcceptsPortableBundle(t *testing.T) {
	f := newSmokeFixture(t)
	yaml := `kind: Tool
id: good__tool
name: Good
description: 只用相对锚点
runtime: lua
entry: |
  local DSN = "mindstorm.db"
  return { ok = true }
input_schema: {type: object}
output_schema: {type: object}
versions:
  - version: "1.0.0"
    state: stable
`
	var out struct {
		Valid       bool                `json:"valid"`
		Diagnostics []engine.Diagnostic `json:"diagnostics"`
	}
	callTool(t, f.session, "gear_validate", map[string]any{"yaml": yaml}, &out)
	for _, d := range out.Diagnostics {
		if d.Code == engine.CodePortablePathHardcoded {
			t.Fatalf("纯相对锚点不应报可移植性错误: %+v", d)
		}
	}
}
