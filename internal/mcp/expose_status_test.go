package mcp

// expose_status_test.go：`--expose` 可发现性的回归（REQ-MCP-EXPOSE-DISCOVER）。
//
// 要防的是一类**静默误导**：智能体写完 mcp.tools 却等不到 tools/list 变化，手上没有任何
// 判据，只能反复重写本来正确的 YAML。所以两条出口都必须给出「谁、改哪里、重连」。

import (
	"fmt"
	"strings"
	"testing"
)

func requireDoctorMCPStatus(t *testing.T, out map[string]any) map[string]any {
	t.Helper()
	m, ok := out["mcp"].(map[string]any)
	if !ok {
		t.Fatalf("gear_doctor 必须返回 mcp 状态块，实际: %v", out)
	}
	return m
}

// TestDoctorReportsMCPExposure 两种装配状态下，doctor 都要如实报出本会话的暴露面。
func TestDoctorReportsMCPExposure(t *testing.T) {
	// 输出 schema 必须与返回体同步列出 mcp 键（只列键、不设 required 的全仓口径）。
	props, _ := outDoctor["properties"].(map[string]any)
	if _, ok := props["mcp"]; !ok {
		t.Fatal("outDoctor 的 outputSchema 未声明 mcp 键，与实际返回体漂移")
	}

	t.Run("默认全关", func(t *testing.T) {
		f := newSmokeFixture(t)
		var out map[string]any
		callTool(t, f.session, "gear_doctor", nil, &out)
		m := requireDoctorMCPStatus(t, out)
		if m["expose"] != "none" || m["tools_enabled"] != false || m["flows_enabled"] != false {
			t.Fatalf("默认应报 expose=none 且两种暴露都关: %v", m)
		}
		if m["scope"] != "" {
			t.Fatalf("默认 scope 应为空: %v", m["scope"])
		}
		// 空集合必须是 [] 而不是 null：消费方 .length 在 null 上会拿 undefined。
		names, ok := m["curated_tools"].([]any)
		if !ok {
			t.Fatalf("curated_tools 必须是数组（不是 null）: %T %v", m["curated_tools"], m["curated_tools"])
		}
		if len(names) != 0 {
			t.Fatalf("未开启暴露时不应装配出策展工具: %v", names)
		}
		hint := fmt.Sprint(m["hint"])
		for _, want := range []string{"--expose tools", "AI 会话内无法开启", "重连", "gear_guide(\"authoring\")"} {
			if !strings.Contains(hint, want) {
				t.Fatalf("hint 缺可执行的下一步 %q: %s", want, hint)
			}
		}
	})

	t.Run("expose tools 加 scope", func(t *testing.T) {
		f := newExposeFixture(t, curatedRules(), WithExpose(ExposeTools), WithScope("study"))
		var out map[string]any
		callTool(t, f.session, "gear_doctor", nil, &out)
		m := requireDoctorMCPStatus(t, out)
		if m["expose"] != "tools" || m["tools_enabled"] != true || m["flows_enabled"] != false {
			t.Fatalf("应报 expose=tools / tools_enabled=true: %v", m)
		}
		if m["scope"] != "study" {
			t.Fatalf("scope 应是 study: %v", m["scope"])
		}
		names, ok := m["curated_tools"].([]any)
		if !ok || len(names) != 2 {
			t.Fatalf("应列出实际装配的 2 个策展工具: %v", m["curated_tools"])
		}
		joined := fmt.Sprint(names)
		for _, want := range []string{"study_plan_propose", "study_plan_preview"} {
			if !strings.Contains(joined, want) {
				t.Fatalf("curated_tools 缺 %q: %v", want, names)
			}
		}
		if hint := fmt.Sprint(m["hint"]); !strings.Contains(hint, "已按 App") {
			t.Fatalf("已装配时应给出「客户端缓存/重连」的排查方向: %s", hint)
		}
	})
}

// curationBundleYAML 拼一个含 App + Tool + Flow 的最小合法 bundle，可选插入 mcp 策展段。
func curationBundleYAML(mcpSection string) string {
	return "kind: App\nid: cur\nname: 策展自查\nentry_flow: cur.up\n" + mcpSection + `---
kind: Tool
id: cur__t
name: T
description: d
runtime: lua
entry: |
  return {}
input_schema:
  type: object
output_schema:
  type: object
versions:
  - version: "1.0.0"
    state: stable
---
kind: Flow
id: cur.up
steps:
  - id: s1
    call: cur__t
`
}

const curationToolOnlyYAML = `kind: Tool
id: lone__t
name: L
description: d
runtime: lua
entry: |
  return {}
input_schema:
  type: object
output_schema:
  type: object
versions:
  - version: "1.0.0"
    state: stable
`

// TestMCPValidateWarnsMissingCuration 缺策展区只给 advisory，不影响 valid；
// 条件要收窄：写了策展区、或根本没有 App 清单的 bundle 都不该被刷屏。
func TestMCPValidateWarnsMissingCuration(t *testing.T) {
	f := newSmokeFixture(t)
	cases := []struct {
		name string
		yaml string
		want bool
	}{
		{"有 Tool 无策展区", curationBundleYAML(""), true},
		{"有 Tool 有策展区", curationBundleYAML("mcp:\n  tools:\n    - ref: cur__t\n"), false},
		{"无 App 清单不刷屏", curationToolOnlyYAML, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out map[string]any
			callTool(t, f.session, "gear_validate", map[string]any{"yaml": c.yaml}, &out)
			if out["valid"] != true {
				t.Fatalf("advisory 不得影响 valid，且 bundle 本身应合法: %v", out)
			}
			warns, _ := out["warnings"].([]any)
			joined := fmt.Sprint(warns)
			hit := strings.Contains(joined, "MCP_CURATION_ABSENT")
			if hit != c.want {
				t.Fatalf("MCP_CURATION_ABSENT 出现=%v，期望=%v；warnings=%v", hit, c.want, warns)
			}
			if c.want {
				for _, want := range []string{"默认", "--expose tools --scope cur", "AI 会话内改不了", "仍可用 gear_run"} {
					if !strings.Contains(joined, want) {
						t.Fatalf("warning 缺关键口径 %q: %v", want, warns)
					}
				}
			}
		})
	}
}
