package mcp

import (
	"strings"
	"testing"

	"gear/internal/mcp/guide"
)

// TestInstructionsCoverCuratedExpose 常驻 instructions 必须让智能体知道「怎么把应用 Tool 暴露成 MCP 工具」。
//
// 背景（2026-11-10）：`--expose tools` + App 清单 `mcp.tools` 落地时 README / 设计文档都同步了，
// 但 guide/summary.md（= ServerOptions.Instructions，会话级常驻、AI 每次连接必读）只字未提。
// 结果：AI 通过 MCP 造应用时根本不知道有这个能力，会继续手搓 server.js。这是「描述比实现窄」
// 的静默缺陷——编译与功能测试全绿，只有引导面漏了。三条口径缺一不可：
// ① 有这个功能（mcp.tools）；② 怎么启用（--expose，且须说明是服务端启动参数、会话内改不了）；
// ③ 去哪查写法（gear_guide("authoring")）。
func TestInstructionsCoverCuratedExpose(t *testing.T) {
	sum := guide.Summary()
	for _, want := range []string{"mcp.tools", "--expose", "启动参数", `gear_guide("authoring")`} {
		if !strings.Contains(sum, want) {
			t.Fatalf("instructions 缺 %q：智能体通过 MCP 造应用时不会知道 --expose tools / 清单策展区", want)
		}
	}
	if strings.Contains(sum, "--expose-flows") {
		t.Fatal("instructions 复活了已移除的 --expose-flows（新口径是 --expose flows）")
	}
}

// TestGuideDocsCoverCuratedExpose 三份 AI 会按需读的手册必须带新口径，且 App 清单字段全表不过期。
//
// service-app.md 曾把字段全表写成「id / entry_flow / start.ports / layout / requires」，
// 加了 mcp 字段后没同步——AI 读它得到的是一份**少一个字段的权威列表**，比没有更坏。
func TestGuideDocsCoverCuratedExpose(t *testing.T) {
	cases := []struct {
		topic string
		wants []string
	}{
		{"overview", []string{"mcp.tools", "--expose"}},
		{"service-app", []string{"mcp.tools", "--expose"}},
		{"authoring", []string{"mcp.tools", "--expose", "启动参数"}},
	}
	for _, c := range cases {
		doc, err := guide.Get(c.topic)
		if err != nil {
			t.Fatalf("读取 guide/%s.md: %v", c.topic, err)
		}
		for _, want := range c.wants {
			if !strings.Contains(doc, want) {
				t.Fatalf("guide/%s.md 缺 %q：引导面与实现漂移", c.topic, want)
			}
		}
	}

	// 整个 guide 包都不许复活旧 flag（包括日后新增的 topic）。
	for _, topic := range guide.Topics() {
		doc, err := guide.Get(topic)
		if err != nil {
			t.Fatalf("读取 guide/%s.md: %v", topic, err)
		}
		if strings.Contains(doc, "--expose-flows") {
			t.Fatalf("guide/%s.md 复活了已移除的 --expose-flows（新口径是 --expose flows）", topic)
		}
	}
}
