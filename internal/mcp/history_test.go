package mcp

import (
	"fmt"
	"strings"
	"testing"
)

// ④「留旧/回滚」的 MCP 面用例：改崩了能不能一键退回。
//
// 这里锁的是**入口**（gear_apply 的 restore_from）与**写入口唯一性**：
// 回滚只能有一个实现（registry.RestoreSnapshot），MCP 只是把它暴露成一个参数。
// registry 层的快照语义（线性留痕、撤销新建、非法快照拒绝）在 internal/registry 里守。

// histToolYAML 造一份只含一个 lua Tool 的 bundle，entry 由调用方给。
func histToolYAML(entry string) string {
	return fmt.Sprintf(`kind: Tool
id: demo__a
name: Demo A
description: 回滚用例用的 Tool
runtime: lua
entry: |
  %s
input_schema:
  type: object
output_schema:
  type: object
versions:
  - version: "1.0.0"
    state: stable
`, entry)
}

// TestApplyRestoreFromRollsBack 走一遍真实场景：写好 → 改坏 → 回滚 → 内容回到改坏前。
func TestApplyRestoreFromRollsBack(t *testing.T) {
	f := newSmokeFixture(t)

	callTool(t, f.session, "gear_apply",
		map[string]any{"bundle": histToolYAML("return { v = 1 }")}, nil)
	// 改坏（内容不同 ⇒ 必须显式 overwrite，这条冲突检查本身也是回滚前的护栏）。
	callTool(t, f.session, "gear_apply", map[string]any{
		"bundle": histToolYAML("return { v = 2 }"), "overwrite": true,
	}, nil)
	cur, err := f.reg.GetTool("demo__a")
	if err != nil || cur == nil || !strings.Contains(fmt.Sprint(cur.Entry), "v = 2") {
		t.Fatalf("改坏后应当是 v = 2，实际 %v", cur)
	}

	// 挑那条「记录着 v = 1」的历史（ListHistory 新的在前 ⇒ 第一条就是）。
	rows, err := f.reg.ListHistory("tool", "demo__a", 0)
	if err != nil {
		t.Fatalf("查历史失败: %v", err)
	}
	if len(rows) < 2 {
		t.Fatalf("应有「新建 + 改坏」两条历史，实际 %d", len(rows))
	}
	seq := rows[0].Seq

	var res map[string]any
	callTool(t, f.session, "gear_apply", map[string]any{"restore_from": seq}, &res)

	cur, err = f.reg.GetTool("demo__a")
	if err != nil || cur == nil {
		t.Fatalf("回滚后 Tool 应当还在: %v", cur)
	}
	if !strings.Contains(fmt.Sprint(cur.Entry), "v = 1") {
		t.Fatalf("回滚后应回到 v = 1，实际 %q", cur.Entry)
	}
}

// TestApplyRestoreFromUnknownSeq：seq 不存在要报错（NOT_FOUND），而不是静默 no-op。
// 回滚是救火动作，静默失败等于救不了火。
func TestApplyRestoreFromUnknownSeq(t *testing.T) {
	f := newSmokeFixture(t)
	expectToolError(t, f.session, "gear_apply",
		map[string]any{"restore_from": 987654}, "NOT_FOUND")
}

// ③ 审计的**负向对照**（2026-09-26）：MCP 面改出来的东西，actor 必须如实是空串。
//
// 这里刻意**不**写「补一个匿名主体让它好看点」的分支：REST 有真身份、MCP 没有，
// 两者混成一个值，审计就再也分不清「没人验过」和「验出来是个匿名用户」。
// 一旦将来 MCP 侧接入了可信身份，把这条用例的期望从 "" 改成真实 subject 即可，
// 它不是「不该通过」的测试，是「口径变了要显式改」的测试。
func TestMCPWritesLeaveActorEmpty(t *testing.T) {
	f := newSmokeFixture(t)
	callTool(t, f.session, "gear_apply",
		map[string]any{"bundle": histToolYAML("return 1")}, nil)
	rows, err := f.reg.ListHistory("tool", "demo__a", 0)
	if err != nil {
		t.Fatalf("查历史失败: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("应有 1 条历史，实际 %d", len(rows))
	}
	if rows[0].Actor != "" {
		t.Fatalf("MCP 建的 Tool 应记成无身份（空串），实际 %q", rows[0].Actor)
	}

	// 删除同样：留痕（note=delete）但主体为空。
	var out map[string]any
	callTool(t, f.session, "gear_delete",
		map[string]any{"kind": "tool", "id": "demo__a"}, &out)
	rows, err = f.reg.ListHistory("tool", "demo__a", 0)
	if err != nil {
		t.Fatalf("查历史失败: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("删除应新增一条历史，实际共 %d 条", len(rows))
	}
	if rows[0].Note != "delete" {
		t.Fatalf("删除那行的 note 应为 delete，实际 %q", rows[0].Note)
	}
	if rows[0].Actor != "" {
		t.Fatalf("MCP 删的 Tool 应记成无身份（空串），实际 %q", rows[0].Actor)
	}
	// 对象确实没了（GetTool 未命中返回 (nil, nil)，err 恒 nil，只能看值）
	if tool, _ := f.reg.GetTool("demo__a"); tool != nil {
		t.Fatalf("删除后应查不到该 Tool，实际 %v", tool)
	}
}
