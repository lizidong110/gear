// Package guide 是 gear 的「智能体手册」——一份内容，三处同源引用。
//
// 三处引用（MCP_AGENT_DESIGN.md §5）：
//  1. `ServerOptions.Instructions` ← `summary.md` 全文（会话级常驻）；
//  2. `gear_guide(topic)` 工具的返回值 ← `{topic}.md`（按需，用完即弃）；
//  3. MCP resources `gear://guide/{topic}` ← 同一个 embed.FS。
//
// 为什么必须同源：手册会随内核演进；若 instructions 里写一份、工具里再写一份，
// 两处迟早不一致，而智能体无法分辨哪个是对的——这正是 gear 自己「单一真相源」红线
// 要防的事。故本包是唯一出口，任何一处引用都不得自带副本。
package guide

import (
	"embed"
	"fmt"
	"sort"
	"strings"
)

//go:embed *.md
var files embed.FS

// SummaryTopic 是 instructions 用的那一篇。它不进 Topics 列表——它已被常驻注入，
// 让 `gear_guide("summary")` 再返回一遍只是浪费 token。
const SummaryTopic = "summary"

// Summary 返回 instructions 全文（guide/summary.md）。
// 读不到时返回空串：调用方（New）会据此判定「装配错误」并 fail-closed，而不是静默发空 instructions。
func Summary() string {
	b, err := files.ReadFile(SummaryTopic + ".md")
	if err != nil {
		return ""
	}
	return string(b)
}

// Topics 返回全部可选 topic 名（不含 summary），按字典序，便于稳定断言与展示。
func Topics() []string {
	entries, err := files.ReadDir(".")
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".md") {
			continue
		}
		topic := strings.TrimSuffix(name, ".md")
		if topic == SummaryTopic {
			continue
		}
		out = append(out, topic)
	}
	sort.Strings(out)
	return out
}

// Get 返回某 topic 的全文。
//
// 未知 topic **报错并列出可用项**，不返回空串——空串会让调用方以为「这个主题没有内容」，
// 从而带着错误的认知继续（不静默降级）。
func Get(topic string) (string, error) {
	topic = strings.TrimSpace(topic)
	if topic == "" {
		topic = "overview"
	}
	if topic == SummaryTopic {
		return Summary(), nil
	}
	b, err := files.ReadFile(topic + ".md")
	if err != nil {
		return "", fmt.Errorf("未知的 guide topic: %q（可用：%s）", topic, strings.Join(Topics(), ", "))
	}
	return string(b), nil
}

// All 返回全部 topic → 全文（含 summary），供注册 resources 用。
func All() map[string]string {
	out := map[string]string{SummaryTopic: Summary()}
	for _, t := range Topics() {
		if b, err := files.ReadFile(t + ".md"); err == nil {
			out[t] = string(b)
		}
	}
	return out
}
