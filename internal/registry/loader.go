package registry

import (
	"fmt"
	"os"
	"strings"

	"gear/internal/model"
	"gopkg.in/yaml.v3"
)

// LoadExamples 解析 Gear 示例 YAML（多文档，每篇一个 Tool 或 Flow，以单独一行的 `---` 分隔），
// 按是否含 `steps` 字段区分 Flow / Tool，返回解析出的两类对象列表。
//
// 用于 `examples/news-demo` 独立演示与 `gear serve --seed` 把示例灌入注册表，使
// GET /api/news 等服务端点开箱即用。不做校验（校验在 UpsertTool/UpsertFlow 注册期完成）。
func LoadExamples(path string) ([]*model.Tool, []*model.Flow, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	return ParseYAMLDocs(b)
}

// ParseYAMLDocs 解析 Gear YAML 文本（多文档，每篇一个 Tool 或 Flow），
// 按是否含 `steps` 字段区分 Flow / Tool，返回解析出的两类对象列表。
//
// 这是**唯一**的多文档解析实现（单一真相源，§0）：`LoadExamples`（文件）、
// CLI `tool/flow register`、MCP `create_tool/create_flow` 全部走这里，
// 以免出现「某条路径只读第一个文档、其余静默丢弃」这类分叉。
//
// 不做校验——校验在 UpsertTool/UpsertFlow/ValidateFlowStatic 注册期完成。
func ParseYAMLDocs(b []byte) ([]*model.Tool, []*model.Flow, error) {
	var tools []*model.Tool
	var flows []*model.Flow
	for _, doc := range splitYAMLDocs(b) {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		var probe map[string]interface{}
		if err := yaml.Unmarshal([]byte(doc), &probe); err != nil {
			return nil, nil, fmt.Errorf("解析 YAML 文档失败: %w", err)
		}
		if len(probe) == 0 {
			continue // 跳过多文档分隔产生的空文档（如首个 `---` 前的注释块）
		}
		if _, isFlow := probe["steps"]; isFlow {
			var f model.Flow
			if err := yaml.Unmarshal([]byte(doc), &f); err != nil {
				return nil, nil, fmt.Errorf("解析 Flow 失败: %w", err)
			}
			flows = append(flows, &f)
		} else {
			var t model.Tool
			if err := yaml.Unmarshal([]byte(doc), &t); err != nil {
				return nil, nil, fmt.Errorf("解析 Tool 失败: %w", err)
			}
			tools = append(tools, &t)
		}
	}
	return tools, flows, nil
}

// splitYAMLDocs 是 SplitYAMLDocs 的内部别名（包内调用保持简短）。
func splitYAMLDocs(b []byte) []string { return SplitYAMLDocs(b) }

// SplitYAMLDocs 按「单独成行的 ---」切分多文档 YAML（与 examples/*.yaml 的写法一致）。
// 首尾可能产出空文档，由调用方跳过。
//
// 导出原因：internal/mcp 需要同一套切分规则（此前它自带一份副本，属重复实现）。
func SplitYAMLDocs(b []byte) []string {
	lines := strings.Split(string(b), "\n")
	var docs []string
	var cur []string
	flush := func() {
		docs = append(docs, strings.Join(cur, "\n"))
		cur = nil
	}
	for _, l := range lines {
		if strings.TrimSpace(l) == "---" {
			flush()
			continue
		}
		cur = append(cur, l)
	}
	flush()
	return docs
}
