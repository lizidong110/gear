package mcp

// errmsg_actionable_test.go —— 报错必须**可据以行动**（2026-10-03 外部实测后的一批收口）。
//
// 触发这份用例的那次实测里，使用者花在「排障方向错了」上的时间比写业务代码还多：
//   - 能力目录 kind 传错 → 静默返回空列表，被当成「这个对象真的不存在」；
//   - describe(app) 报 APP_NOT_FOUND → 被理解成「Gear 没有 App 这种对象」；
//   - 缺密钥只说「请人在 CLI 播种」→ 没人知道命令怎么写、落在哪个库。
//
// 三条用例各锁一个方向：**空 ≠ 无、查错了要说出正确路径、缺什么要给可复制的命令**。

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestCatalogRejectsUnknownKind：kind 非法必须报错并列出合法值。
//
// 旧行为是静默返回空列表——那是最坏的一种误导：它把「我查错了」说成「它不存在」。
func TestCatalogRejectsUnknownKind(t *testing.T) {
	f := newSmokeFixture(t)
	for _, bad := range []string{"tools", "flow-", "应用", "flow,tool", "appp", "TOOL1"} {
		res, err := f.session.CallTool(context.Background(), &mcp.CallToolParams{
			Name:      "gear_catalog",
			Arguments: map[string]any{"kind": bad},
		})
		if err != nil {
			t.Fatalf("kind=%q 协议层失败: %v", bad, err)
		}
		if !res.IsError {
			b, _ := json.Marshal(res.StructuredContent)
			t.Fatalf("kind=%q 应报错（空 ≠ 无），实际成功: %s", bad, b)
		}
		txt := contentText(res)
		if !strings.Contains(txt, "BAD_ARGUMENT") {
			t.Fatalf("kind=%q 的错误码应是 BAD_ARGUMENT，实际: %s", bad, txt)
		}
		if !strings.Contains(txt, "合法值") {
			t.Fatalf("kind=%q 的报错应列出合法值，实际: %s", bad, txt)
		}
		// 合法值必须真的合法（防止文案与实现漂移）
		for _, ok := range []string{"tool", "app", "execution"} {
			if !strings.Contains(txt, ok) {
				t.Fatalf("kind=%q 的报错没列出真正的合法值 %q: %s", bad, ok, txt)
			}
		}
	}
	// 负向对照：合法值仍然照常工作，且不报错。
	for _, ok := range []string{"all", "tool", "flow", "hook", "execution", "app"} {
		callTool(t, f.session, "gear_catalog", map[string]any{"kind": ok}, &struct {
			Items []map[string]any `json:"items"`
		}{})
	}
	// 大小写与首尾空白**有**归一（实现里 ToLower+TrimSpace）——这条锁住它不被
	// 「严格校验」改掉：CLI/MCP 用户手写大写的 Tool 很常见，报错只会添乱。
	for _, ok := range []string{"Tool", " APP ", "Flow"} {
		callTool(t, f.session, "gear_catalog", map[string]any{"kind": ok}, &struct {
			Items []map[string]any `json:"items"`
		}{})
	}
}

// TestDescribeUnknownKindListsLegals：describe 的 kind 校验与 catalog 同源。
func TestDescribeUnknownKindListsLegals(t *testing.T) {
	f := newSmokeFixture(t)
	txt := expectToolError(t, f.session, "gear_describe",
		map[string]any{"kind": "applcation", "id": "x"}, "BAD_ARGUMENT")
	// 四个合法值都要在（顺序由 kindsHint 排序给出，不锁顺序——那是实现细节）。
	for _, want := range []string{"tool", "flow", "hook", "app"} {
		if !strings.Contains(txt, want) {
			t.Fatalf("describe 的 kind 报错缺合法值 %q，实际: %s", want, txt)
		}
	}
}

// TestDescribeAppNotFoundExplainsWhatAppIs：APP_NOT_FOUND 必须解释「应用是什么」。
//
// 语义本身是对的（app_id 不在台账），但原文只有「台账里没有这个 app_id」，
// 使用者会以为 Gear 没有「应用」这种对象，转而去建 Tool/Flow 心智模型。
func TestDescribeAppNotFoundExplainsWhatAppIs(t *testing.T) {
	f := newSmokeFixture(t)
	txt := expectToolError(t, f.session, "gear_describe",
		map[string]any{"kind": "app", "id": "nope"}, "APP_NOT_FOUND")
	for _, want := range []string{"mode:service", "gear_describe(kind=flow)", "gear_app(action=start"} {
		if !strings.Contains(txt, want) {
			t.Fatalf("APP_NOT_FOUND 的 hint 缺 %q，实际: %s", want, txt)
		}
	}
}

// TestApplyWarnsMissingSecretWithCopyableCommand：缺密钥要给**可复制**的播种命令与落点。
//
// MCP 不写密钥是刻意设计（明文会进对话记录与执行记录库），但「由人播种」这句话本身
// 不够：没给命令、没给落点，使用者会以为无路可走，转而去 rebuild 二进制碰运气。
// 而 REST 的 PUT /api/secrets 与 CLI 两条通道都真实存在，必须一并给出。
func TestApplyWarnsMissingSecretWithCopyableCommand(t *testing.T) {
	f := newSmokeFixture(t)
	bundle := `kind: App
id: wmsapp
name: wms
entry_flow: wmsapp.up
requires:
  secrets:
    - WMS_JWT_KEY
---
kind: Tool
id: wmsapp__t
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
id: wmsapp.up
name: up
description: 示例
steps:
  - id: s1
    call: wmsapp__t
`
	var out struct {
		Valid    bool     `json:"valid"`
		Warnings []string `json:"warnings"`
	}
	callTool(t, f.session, "gear_validate", map[string]any{"yaml": bundle}, &out)
	joined := strings.Join(out.Warnings, "\n")
	if !strings.Contains(joined, "MISSING_SECRET") {
		t.Fatalf("缺密钥应报 MISSING_SECRET，实际: %s", joined)
	}
	for _, want := range []string{"gear secret set WMS_JWT_KEY", "PUT /api/secrets/WMS_JWT_KEY", "secrets.db"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("MISSING_SECRET 告警缺 %q（要给可复制的命令与落点），实际: %s", want, joined)
		}
	}
}

// TestApplyNextSaysStaticFilesAreNotWrittenByMCP：apply 的 next 必须点明静态资源要自己落盘。
//
// 「纯 MCP 建应用」走到一半才发现缺一环，是编排式智能体最典型的中途卡死；
// apply 只登记不落地，而 next 此前一句没提。
func TestApplyNextSaysStaticFilesAreNotWrittenByMCP(t *testing.T) {
	f := newSmokeFixture(t)
	const bundle = `kind: Tool
id: nextapp__t
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
`
	var out struct {
		Valid bool   `json:"valid"`
		Next  string `json:"next"`
	}
	// 不用 dry_run：dry_run 的 next 会被换成「一个字节未写」，那是有意的另一条提示。
	callTool(t, f.session, "gear_apply", map[string]any{"bundle": bundle}, &out)
	if !strings.Contains(out.Next, "MCP") || !strings.Contains(out.Next, "文件") {
		t.Fatalf("apply 的 next 应说明「MCP 不提供文件读写」，实际: %s", out.Next)
	}
}
