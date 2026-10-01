package engine

import (
	"testing"
)

// maxPropDepth 统计 JSON Schema 里嵌套 "properties" 的最大层数（含顶层），用于验证递归深度守卫。
func maxPropDepth(v any) int {
	m, ok := v.(map[string]any)
	if !ok {
		return 0
	}
	best := 0
	if props, ok := m["properties"].(map[string]any); ok {
		for _, p := range props {
			if d := maxPropDepth(p) + 1; d > best {
				best = d
			}
		}
	}
	if items, ok := m["items"].(map[string]any); ok {
		if d := maxPropDepth(items); d > best {
			best = d
		}
	}
	return best
}

// TestMethodSchemaToJSONNestedObject 验证 object 参数的子字段（Properties）被递归展开成
// JSON Schema 的 properties + 子级 required。
func TestMethodSchemaToJSONNestedObject(t *testing.T) {
	ms := MethodSchema{Parameters: map[string]ParamSchema{
		"cfg": {Type: "object", Required: true, Properties: map[string]ParamSchema{
			"topic": {Type: "string", Required: true, Description: "主题"},
			"qos":   {Type: "number", Description: "等级"},
		}},
	}}
	out := methodSchemaToJSON(ms)
	props, ok := out["properties"].(map[string]any)
	if !ok {
		t.Fatalf("顶层 properties 缺失: %#v", out)
	}
	cfg, ok := props["cfg"].(map[string]any)
	if !ok {
		t.Fatalf("cfg 参数缺失: %#v", props)
	}
	if cfg["type"] != "object" {
		t.Errorf("cfg 应为 object，实得 %v", cfg["type"])
	}
	sub, ok := cfg["properties"].(map[string]any)
	if !ok {
		t.Fatalf("cfg 未展开嵌套 properties: %#v", cfg)
	}
	if _, ok := sub["topic"]; !ok {
		t.Errorf("cfg 嵌套缺少 topic: %#v", sub)
	}
	req, ok := cfg["required"].([]string)
	if !ok || len(req) != 1 || req[0] != "topic" {
		t.Errorf("cfg 子级 required 应为 [topic]，实得 %#v", cfg["required"])
	}
}

// TestMethodSchemaToJSONNestedArrayItems 验证 array 参数的元素 schema（Items）被递归展开成 items。
func TestMethodSchemaToJSONNestedArrayItems(t *testing.T) {
	ms := MethodSchema{Parameters: map[string]ParamSchema{
		"rows": {Type: "array", Required: true, Items: &ParamSchema{
			Type: "object",
			Properties: map[string]ParamSchema{
				"sql":    {Type: "string", Required: true},
				"params": {Type: "array"},
			},
		}},
	}}
	out := methodSchemaToJSON(ms)
	props := out["properties"].(map[string]any)
	rows := props["rows"].(map[string]any)
	if rows["type"] != "array" {
		t.Errorf("rows 应为 array，实得 %v", rows["type"])
	}
	items, ok := rows["items"].(map[string]any)
	if !ok {
		t.Fatalf("rows 未展开 items: %#v", rows)
	}
	sub, ok := items["properties"].(map[string]any)
	if !ok {
		t.Fatalf("items 未展开嵌套 properties: %#v", items)
	}
	if _, ok := sub["sql"]; !ok {
		t.Errorf("items 嵌套缺少 sql: %#v", sub)
	}
	req := items["required"].([]string)
	if len(req) != 1 || req[0] != "sql" {
		t.Errorf("items 子级 required 应为 [sql]，实得 %#v", items["required"])
	}
}

// TestMethodSchemaToJSONDepthGuardNoInfiniteLoop 验证超深嵌套会被 schemaMaxDepth 截断、
// 不死循环（即便输入是 10 层深的链）。
func TestMethodSchemaToJSONDepthGuardNoInfiniteLoop(t *testing.T) {
	// 构造 10 层深的 object→Properties 链
	leaf := ParamSchema{Type: "string"}
	chain := ParamSchema{Type: "object", Properties: map[string]ParamSchema{"d0": leaf}}
	for i := 1; i < 10; i++ {
		chain = ParamSchema{Type: "object", Properties: map[string]ParamSchema{
			"d" + string(rune('a'+i)): chain,
		}}
	}
	ms := MethodSchema{Parameters: map[string]ParamSchema{"root": chain}}
	out := methodSchemaToJSON(ms) // 若死循环，这里会超时
	depth := maxPropDepth(out)
	if depth > schemaMaxDepth+1 {
		t.Errorf("递归未受深度守卫限制：嵌套层数 %d 超过上限 %d", depth, schemaMaxDepth+1)
	}
	if depth < 1 {
		t.Errorf("递归结果异常：嵌套层数 %d", depth)
	}
}

// TestMethodSchemaToJSONLeavesFreeMapAlone 验证自由 map 型参数（无 Properties）不会画蛇添足加 properties。
func TestMethodSchemaToJSONLeavesFreeMapAlone(t *testing.T) {
	req := builtinCapabilitySchemas["http"]["Request"]
	out := methodSchemaToJSON(req)
	props := out["properties"].(map[string]any)
	headers, ok := props["headers"].(map[string]any)
	if !ok {
		t.Fatalf("headers 参数缺失: %#v", props)
	}
	if _, hasProps := headers["properties"]; hasProps {
		t.Errorf("自由 map 型参数 headers 不应带 properties: %#v", headers)
	}
}

// TestCapabilityCatalogNestedSchemaEmitted 验证真实能力目录里固定结构参数已带嵌套 schema。
func TestCapabilityCatalogNestedSchemaEmitted(t *testing.T) {
	// db.batch.statements 元素 {sql(required), params}
	batch := builtinCapabilitySchemas["db"]["batch"]
	stmts := batch.Parameters["statements"]
	if stmts.Items == nil {
		t.Fatalf("db.batch.statements 应有 Items")
	}
	if stmts.Items.Type != "object" {
		t.Errorf("statements.Items 应为 object，实得 %v", stmts.Items.Type)
	}
	sqlSchema, ok := stmts.Items.Properties["sql"]
	if !ok {
		t.Fatalf("statements.Items 缺少 sql 子字段")
	}
	if !sqlSchema.Required {
		t.Errorf("statements.Items.sql 应为必填")
	}

	// subscribe.start.config {topic(required), qos}
	cfg := builtinCapabilitySchemas["subscribe"]["start"].Parameters["config"]
	if len(cfg.Properties) == 0 {
		t.Fatalf("subscribe.start.config 应有 Properties")
	}
	if !cfg.Properties["topic"].Required {
		t.Errorf("subscribe.start.config.topic 应为必填")
	}

	// subscribe.start.callback {type(required), target, input}
	cb := builtinCapabilitySchemas["subscribe"]["start"].Parameters["callback"]
	if len(cb.Properties) == 0 {
		t.Fatalf("subscribe.start.callback 应有 Properties")
	}
	if !cb.Properties["type"].Required {
		t.Errorf("subscribe.start.callback.type 应为必填")
	}

	// db.query.params 应有 Items（标量说明）
	if builtinCapabilitySchemas["db"]["query"].Parameters["params"].Items == nil {
		t.Errorf("db.query.params 应有 Items")
	}
}
