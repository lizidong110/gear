package engine

// purefunc_validate_test.go —— 纯算法族 ctx.validate.*。
//
// ★ 核心是三种「失败」的口径区分（与 jwt/password 都不一样，别写混）：
//   数据不合法 → valid=false（正常业务结果，要展示给用户）；
//   schema 非法 → **报错**（应用的 bug，不能静默成「用户填错了」）；
//   缺参数 → 报错（与其它纯函数原子同款 fail-closed）。

import (
	"strings"
	"testing"
)

func TestPureValidateJSONSchemaAcceptsValidData(t *testing.T) {
	out, err := pureCall(t, "validate", "json_schema", map[string]interface{}{
		"schema": map[string]interface{}{
			"type":     "object",
			"required": []interface{}{"name"},
			"properties": map[string]interface{}{
				"name": map[string]interface{}{"type": "string"},
				"age":  map[string]interface{}{"type": "integer", "minimum": float64(0)},
			},
		},
		"data": map[string]interface{}{"name": "alice", "age": float64(30)},
	})
	if err != nil {
		t.Fatalf("合法数据不应报错: %v", err)
	}
	m := out.(map[string]interface{})
	if m["valid"] != true {
		t.Fatalf("合法数据应 valid=true，实际 %v（errors=%v）", m["valid"], m["errors"])
	}
	if errs, ok := m["errors"].([]string); !ok || len(errs) != 0 {
		t.Fatalf("合法数据的 errors 应为空数组，实际 %v", m["errors"])
	}
}

func TestPureValidateJSONSchemaReportsInvalidData(t *testing.T) {
	out, err := pureCall(t, "validate", "json_schema", map[string]interface{}{
		"schema": map[string]interface{}{
			"type":     "object",
			"required": []interface{}{"name", "email"},
			"properties": map[string]interface{}{
				"name":  map[string]interface{}{"type": "string"},
				"email": map[string]interface{}{"type": "string"},
			},
		},
		"data": map[string]interface{}{"name": "alice"},
	})
	if err != nil {
		t.Fatalf("★ 数据不合法**不是错误**，应返回 valid=false: %v", err)
	}
	m := out.(map[string]interface{})
	if m["valid"] != false {
		t.Fatalf("缺必填字段应 valid=false，实际 %v", m["valid"])
	}
	errs, ok := m["errors"].([]string)
	if !ok || len(errs) == 0 {
		t.Fatalf("应给出错误明细（给用户看哪里填错），实际 %v", m["errors"])
	}
	if !strings.Contains(strings.Join(errs, " "), "email") {
		t.Fatalf("错误明细应指出缺失的字段 email，实际 %v", errs)
	}
}

func TestPureValidateJSONSchemaTypeMismatch(t *testing.T) {
	out, err := pureCall(t, "validate", "json_schema", map[string]interface{}{
		"schema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"age": map[string]interface{}{"type": "integer"},
			},
		},
		"data": map[string]interface{}{"age": "thirty"},
	})
	if err != nil {
		t.Fatalf("类型不符应返回 valid=false 而不是报错: %v", err)
	}
	if out.(map[string]interface{})["valid"] != false {
		t.Fatalf("字符串当 integer 应 valid=false，实际 %v", out)
	}
}

// TestPureValidateScalarsAndArrays：`data` 是**任意类型**，不能只支持对象。
func TestPureValidateScalarsAndArrays(t *testing.T) {
	cases := []struct {
		name  string
		schem map[string]interface{}
		data  interface{}
		valid bool
	}{
		{"字符串符合", map[string]interface{}{"type": "string", "minLength": float64(3)}, "abc", true},
		{"字符串过短", map[string]interface{}{"type": "string", "minLength": float64(5)}, "abc", false},
		{"数字符合", map[string]interface{}{"type": "number", "maximum": float64(10)}, float64(5), true},
		{"数字超限", map[string]interface{}{"type": "number", "maximum": float64(10)}, float64(50), false},
		{"数组元素符合", map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}}, []interface{}{"a", "b"}, true},
		{"数组元素不符", map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}}, []interface{}{"a", float64(1)}, false},
	}
	for _, c := range cases {
		out, err := pureCall(t, "validate", "json_schema", map[string]interface{}{"schema": c.schem, "data": c.data})
		if err != nil {
			t.Fatalf("%s：不应报错: %v", c.name, err)
		}
		if out.(map[string]interface{})["valid"] != c.valid {
			t.Fatalf("%s：期望 valid=%v，实际 %v", c.name, c.valid, out)
		}
	}
}

// TestPureValidateBadSchemaErrors：schema 本身非法必须报错——
// 若静默成 valid=false，「所有数据都校验不过」会被误诊成「用户都填错了」。
func TestPureValidateBadSchemaErrors(t *testing.T) {
	bad := []struct {
		name   string
		schema map[string]interface{}
	}{
		{"未知 type", map[string]interface{}{"type": "not-a-type"}},
		{"minimum 不是数字", map[string]interface{}{"type": "object", "minimum": "abc"}},
	}
	for _, c := range bad {
		if _, err := pureCall(t, "validate", "json_schema", map[string]interface{}{
			"schema": c.schema, "data": map[string]interface{}{},
		}); err == nil {
			t.Fatalf("%s：schema 非法必须报错，不能返回 valid=false", c.name)
		}
	}
}

func TestPureValidateFailClosedOnBadInput(t *testing.T) {
	cases := []struct {
		name string
		args map[string]interface{}
	}{
		{"缺 schema", map[string]interface{}{"data": map[string]interface{}{}}},
		{"缺 data", map[string]interface{}{"schema": map[string]interface{}{"type": "object"}}},
		{"schema 类型错", map[string]interface{}{"schema": "type=object", "data": map[string]interface{}{}}},
		{"未知方法", map[string]interface{}{"schema": map[string]interface{}{}, "data": nil}},
	}
	for i, c := range cases {
		method := "json_schema"
		if c.name == "未知方法" {
			method = "yaml_schema"
		}
		if _, err := pureCall(t, "validate", method, c.args); err == nil {
			t.Fatalf("case %d（%s）：应报错（fail-closed），实际通过", i, c.name)
		}
	}
}

// TestPureValidateNullDataIsAccepted 确认显式传 nil（即 JSON null）不会误判成「缺参数」。
func TestPureValidateNullDataIsAccepted(t *testing.T) {
	out, err := pureCall(t, "validate", "json_schema", map[string]interface{}{
		"schema": map[string]interface{}{"type": "null"},
		"data":   nil,
	})
	if err != nil {
		t.Fatalf("显式传 null 应能校验（不该被当成缺参数）: %v", err)
	}
	if out.(map[string]interface{})["valid"] != true {
		t.Fatalf("null 对 type=null 应合法，实际 %v", out)
	}
}
