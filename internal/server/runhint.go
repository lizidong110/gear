package server

// runhint.go：REST 执行面的**可据以行动的提示**。
//
// 存在的理由（2026-10-03 外部实测）：`POST /api/tools/{id}/run` 要**扁平 body**，
// 而把入参包一层 `{"input":{...}}`（照 MCP/RPC 的习惯）会得到
//
//	input 校验失败: jsonschema: '' does not validate with ...#/required: missing properties: 'username','password'
//
// 这条报错本身是**对的**，但它把病因指错了方向：读起来像「我明明传了字段，怎么说没传」，
// 于是排查去翻字段名与类型，而真因只是**层级多了一层**。
//
// ⚠ 这里**只提示、不兼容**：自动兼容 `{"input":{...}}` 与扁平两种写法，会让
// 「Tool 真有一个叫 input 的字段」和「包了一层」变得无法区分——而静默的歧义比
// 一个明确的报错贵得多。需要包一层的调用方请显式展平。

import (
	"sort"
	"strings"

	"gear/internal/model"
)

// wrappedInputHint 在「body 被包了一层 input」时给出提示，其余情况返回空串。
//
// 判据刻意保守，三条同时成立才提示（宁可漏提示，不可乱提示）：
//  1. body 只有一个键 `input`；
//  2. 它的值是对象（`{"input": {...}}` 而不是 `{"input": "x"}`）；
//  3. **这个 Tool 的 input_schema 里没有 `input` 属性**——有的话那就是正当字段名，不该提示。
func wrappedInputHint(tool *model.Tool, input map[string]interface{}) string {
	if tool == nil || len(input) != 1 {
		return ""
	}
	inner, ok := input["input"].(map[string]interface{})
	if !ok {
		return ""
	}
	if schemaHasProperty(tool.InputSchema, "input") {
		return ""
	}
	keys := make([]string, 0, len(inner))
	for k := range inner {
		keys = append(keys, k)
	}
	sort.Strings(keys) // map 迭代无序：提示里的字段列表必须可复现，否则测试与截图都对不上
	return "检测到你把入参包了一层 {\"input\": {...}}，而本端点要的是**扁平 body**：请把内层字段直接放到顶层" +
		"（当前内层字段：" + strings.Join(keys, ", ") + "）。这是提示，不会自动兼容——自动兼容会让真叫 input 的字段产生歧义。"
}

// schemaHasProperty 报 schema 的 properties 里有没有这个键。
//
// 判据用 properties 而不是 required：required 只说明「必填」，一个可选的 input 字段
// 同样会让包一层的写法合法，此时提示就是错的。
func schemaHasProperty(schema map[string]interface{}, key string) bool {
	if schema == nil {
		return false
	}
	props, ok := schema["properties"].(map[string]interface{})
	if !ok {
		return false
	}
	_, has := props[key]
	return has
}
