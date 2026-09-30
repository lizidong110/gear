package mcp

// 命名规范引导测试：无 App 清单的裸注册（无 <app>__ / <app>. 前缀）不拦，
// 但必须在 warnings 里提醒——registry 曾被测试残留塞满（2026-09-25 清理 18 个孤儿），
// 事后只能人工考古归属，把提醒前移到写作期。
// 规范本体在 internal/mcp/guide/authoring.md 的「命名规范」一节（§5）。

import (
	"strings"
	"testing"
)

const namingToolYAML = `kind: Tool
id: %s
name: 测试 Tool
description: 测试
runtime: lua
entry: "return {}"
input_schema: {type: object}
output_schema: {type: object}
versions: [{version: "1.0.0", state: stable}]
`

type validateOut struct {
	Valid       bool     `json:"valid"`
	Warnings    []string `json:"warnings"`
	Diagnostics []any    `json:"diagnostics"`
}

func validateNaming(t *testing.T, id string) validateOut {
	t.Helper()
	f := newSmokeFixture(t)
	var out validateOut
	callTool(t, f.session, "gear_validate", map[string]any{"yaml": strings.ReplaceAll(namingToolYAML, "%s", id)}, &out)
	return out
}

func TestNamingGuidanceWarnsOnBareID(t *testing.T) {
	out := validateNaming(t, "m_t1")
	if !out.Valid {
		t.Fatalf("裸 id 定义本身合法，不应影响 valid: %+v", out.Diagnostics)
	}
	if !hasWarningWith(out.Warnings, "m_t1") || !hasWarningWith(out.Warnings, "命名空间前缀") {
		t.Fatalf("无前缀 id 应出现在命名引导 warnings 里: %v", out.Warnings)
	}
}

func TestNamingGuidanceSilentForPrefixedID(t *testing.T) {
	for _, id := range []string{"myapp__t1", "myapp.t1", "fixture_echo"} {
		out := validateNaming(t, id)
		if !out.Valid {
			t.Fatalf("带前缀 id %s 应 valid: %+v", id, out.Diagnostics)
		}
		if hasWarningWith(out.Warnings, "命名空间前缀") {
			t.Fatalf("id %s 已带前缀/豁免，不应再给命名引导: %v", id, out.Warnings)
		}
	}
}
