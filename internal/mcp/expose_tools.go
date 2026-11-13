package mcp

// expose_tools.go：`--expose {flows,tools,all}` 里「暴露注册 Tool」那一半，
// 以及 App 清单 `mcp:` 策展区的注册期校验（REQ-MCP-EXPOSE-TOOLS / REQ-APP-MCP-CURATE）。
//
// 与 flow__{id} 的差别不是「多一个循环」，而是**暴露面由谁决定**：
//   - flow__{id} 是「registry 里有什么就吐什么」——默认关闭，因为工具数不可预测；
//   - 这里的工具面来自 App 清单的**白名单**：ref / alias / description / args 都由应用作者
//     策展，工具数启动期即确定，--scope <appId> 把它按应用隔离。
//
// 执行走 `s.engine.RunTool`（与 gear_run(kind=tool)、CLI gear tool run 同一条进程内路径），
// 不为每次 tools/call 派生进程。注意 Tool 自身若声明 `runtime: process`，那是 Tool 的定义，
// 引擎仍会按定义启动它——本层只保证「不额外起一个 gear 子进程」。
//
// 三条红线：
//   - **fail-closed**：开 --expose=tools 但没有 --scope、App 不存在、或 App 没写 mcp.tools
//     ⇒ 直接拒绝启动 MCP Server，绝不退回「裸暴露全量 Tool」（那会丢产品护栏）；
//   - **structuredContent 顶层必须是对象**（REQ-MCP-WIRE-OBJECT，严格客户端真实事故）：
//     输出一律包成 {"kind","id","output"}，与 gear_run(kind=tool) 同形；
//   - **固定参数 AI 不可见也不可改**：args 键从对外 inputSchema 里删掉，handler 无条件覆盖。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"gear/internal/engine"
	"gear/internal/model"
)

// ExposeMode 是 `--expose` 的取值。空值 = 全部反向暴露都关闭（保持今日默认）。
type ExposeMode string

const (
	ExposeNone  ExposeMode = ""
	ExposeFlows ExposeMode = "flows"
	ExposeTools ExposeMode = "tools"
	ExposeAll   ExposeMode = "all"
)

// ParseExposeMode 把 CLI 值归一化并校验。未知值必须**报错**而非当成 ""（不静默降级）。
func ParseExposeMode(s string) (ExposeMode, error) {
	m := ExposeMode(strings.ToLower(strings.TrimSpace(s)))
	switch m {
	case ExposeNone, ExposeFlows, ExposeTools, ExposeAll:
		return m, nil
	default:
		return ExposeNone, fmt.Errorf("--expose 只认 flows | tools | all（实际 %q）", s)
	}
}

func (m ExposeMode) wantsFlows() bool { return m == ExposeFlows || m == ExposeAll }
func (m ExposeMode) wantsTools() bool { return m == ExposeTools || m == ExposeAll }

// registerExposedTools 把 --scope 指向的 App 的策展区挂成 MCP 工具。
//
// 返回 error 而不是「跳过」：装配期的错误若被吞掉，客户端只会看到一个工具数不对的
// tools/list，却不知道自己的护栏根本没生效——这是最难排查的一类静默降级。
func (s *Server) registerExposedTools(ms *mcp.Server) error {
	if !s.expose.wantsTools() {
		return nil
	}
	scope := strings.TrimSpace(s.scope)
	if scope == "" {
		return fmt.Errorf("--expose 含 tools 时必须同时给 --scope <appId>：工具面按 App 隔离，" +
			"没有作用域就等于让整库 Tool 决定工具数")
	}
	a, err := s.reg.GetApp(scope)
	if err != nil {
		return fmt.Errorf("读取 App %q 失败: %w", scope, err)
	}
	if a == nil {
		return fmt.Errorf("--scope %q 对应的 App 未注册：App 行的唯一写入口是 gear_apply / gear_import，"+
			"只注册 Tool/Flow 不会生成 App 行", scope)
	}
	if len(a.MCP.Tools) == 0 {
		return fmt.Errorf("App %q 没有 mcp.tools 策展区：拒绝裸暴露该 App 下的全部 Tool"+
			"（产品护栏会丢失、工具数不可预测）。请在 kind: App 清单里写 mcp.tools 白名单", scope)
	}
	for _, rule := range a.MCP.Tools {
		if err := s.addExposedTool(ms, a, rule); err != nil {
			return fmt.Errorf("App %q 的 mcp.tools 规则 %q: %w", scope, rule.ExposedName(), err)
		}
	}
	return nil
}

// addExposedTool 注册一条策展规则。
func (s *Server) addExposedTool(ms *mcp.Server, a *model.App, rule model.AppMCPTool) error {
	// 跨 App ref 必须挡在装配期：--scope 是隔离边界，允许引用别的应用的 Tool 等于把
	// 隔离变成摆设（运行期 guardScope 会拒，但那时客户端已经看见了这个工具）。
	if !a.Owns(rule.Ref) {
		return fmt.Errorf("ref %q 不属于 App %q 的命名空间（须以 %q__ 或 %q. 开头）",
			rule.Ref, a.ID, a.ID, a.ID)
	}
	t, err := s.reg.GetTool(rule.Ref)
	if err != nil {
		return err
	}
	if t == nil {
		return fmt.Errorf("ref %q 对应的 Tool 未注册", rule.Ref)
	}
	inSchema, err := exposedInputSchema(t, rule)
	if err != nil {
		return err
	}
	desc := strings.TrimSpace(rule.Description)
	if desc == "" {
		desc = strings.TrimSpace(t.Description)
	}
	if desc == "" {
		desc = "（无描述）"
	}
	name, ref, args := rule.ExposedName(), rule.Ref, rule.Args
	// outputSchema 沿用全仓「只列键、不设约束」口径（见 common.go 的 outSchema）：
	// Tool 自己的 output_schema 由引擎在运行期校验，MCP 层不重复实现第二份校验，
	// 否则版本覆盖 / 动态输出一来，两处 schema 必然漂移。
	outSchemaSpec := outSchema(map[string]any{
		"kind": sStr(), "id": sStr(), "output": sAny(),
	})
	mcp.AddTool[map[string]any, any](ms, &mcp.Tool{
		Name:         name,
		Description:  desc,
		InputSchema:  inSchema,
		OutputSchema: outSchemaSpec,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in map[string]any) (*mcp.CallToolResult, any, error) {
		input := make(map[string]any, len(in)+len(args))
		for k, v := range in {
			input[k] = v
		}
		// 服务端后写 = 客户端传什么都改不掉护栏参数。
		for k, v := range args {
			input[k] = v
		}
		out, err := s.engine.RunTool(ref, input)
		if err != nil {
			return toolErrorFrom("TOOL_RUN_FAILED", err), nil, nil
		}
		if out == nil {
			out = map[string]any{}
		}
		return nil, map[string]any{"kind": "tool", "id": ref, "output": out}, nil
	})
	// 登记"真的挂上去了"的名字（不是清单里写了几个），gear_doctor 据此回答
	// 「我的策展工具为什么没出现」——两处口径同源，不另读一遍清单重算。
	s.exposedToolNames = append(s.exposedToolNames, name)
	return nil
}

// exposedInputSchema 把 Tool 的 input_schema 映射成对外 MCP inputSchema。
//
// 做两件事：① 深拷贝后校验顶层 type=object（go-sdk 的 AddTool 对非 object 会 panic，
// 必须在装配期拦住）；② 删掉 args 里的键（properties + required），让 AI 看不到也
// 不必填这些由服务端固定的参数。args 键若不在 properties 里直接报错——「注入了一个
// 工具根本不认识的参数」是清单写错，静默放行会让问题拖到运行期。
func exposedInputSchema(t *model.Tool, rule model.AppMCPTool) (map[string]any, error) {
	if t == nil {
		return nil, fmt.Errorf("Tool 为空，无法映射成 MCP 工具")
	}
	if t.InputSchema == nil {
		return nil, fmt.Errorf("Tool %s 没有 input_schema，无法映射成 MCP 工具（MCP 规范要求顶层 object）", t.ID)
	}
	raw, err := json.Marshal(t.InputSchema)
	if err != nil {
		return nil, fmt.Errorf("Tool %s 的 input_schema 不是合法 JSON: %w", t.ID, err)
	}
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		return nil, fmt.Errorf("Tool %s 的 input_schema 不是 JSON 对象: %w", t.ID, err)
	}
	if schema["type"] != "object" {
		return nil, fmt.Errorf("Tool %s 的 input_schema 顶层 type 必须是 object（实际 %v）", t.ID, schema["type"])
	}
	props, _ := schema["properties"].(map[string]any)
	for k := range rule.Args {
		if _, ok := props[k]; !ok {
			return nil, fmt.Errorf("args.%s 不在 Tool %s 的 input_schema.properties 里", k, t.ID)
		}
		delete(props, k)
		schema["required"] = removeRequired(schema["required"], k)
	}
	if props != nil {
		schema["properties"] = props
	}
	return schema, nil
}

// removeRequired 从 required 数组里去掉一个键；nil/非数组原样返回。
func removeRequired(v any, key string) any {
	items, ok := v.([]any)
	if !ok {
		return v
	}
	out := make([]any, 0, len(items))
	for _, it := range items {
		if s, ok := it.(string); ok && s == key {
			continue
		}
		out = append(out, it)
	}
	return out
}

// checkAppMCP 是 `gear_validate` / `gear_apply` 前置校验里属于「bundle 级」的那一半：
// ref 存在性、命名空间归属、schema 可映射性都需要 registry 或 bundle 上下文，
// 故不能塞进 model.App.Validate（那边只校验不依赖上下文的部分）。
//
// 错误码沿用已登记的 BAD_APP_MANIFEST（错误码表见 guide/errors.md），不另造新码。
func (s *Server) checkAppMCP(a *appManifest, b *bundle) []engine.Diagnostic {
	if len(a.MCP.Tools) == 0 {
		return nil
	}
	var out []engine.Diagnostic
	for i, rule := range a.MCP.Tools {
		field := fmt.Sprintf("kind: App.mcp.tools[%d]", i)
		if !a.Owns(rule.Ref) {
			out = append(out, engine.Diagnostic{
				Code: "BAD_APP_MANIFEST", Field: field + ".ref",
				Message: fmt.Sprintf("ref %q 不属于 App %q 的命名空间", rule.Ref, a.ID),
				Hint:    fmt.Sprintf("改用本应用前缀（%s__ 或 %s.），或把规则挪进拥有它的 App 清单", a.ID, a.ID),
			})
			continue
		}
		t := findBundleTool(b, rule.Ref)
		if t == nil {
			var err error
			t, err = s.reg.GetTool(rule.Ref)
			if err != nil {
				out = append(out, engine.Diagnostic{
					Code: "BAD_APP_MANIFEST", Field: field + ".ref",
					Message: fmt.Sprintf("校验 ref %q 时读 registry 失败: %v", rule.Ref, err),
					Hint:    "先 gear_doctor 确认 registry 可读",
				})
				continue
			}
		}
		if t == nil {
			out = append(out, engine.Diagnostic{
				Code: "BAD_APP_MANIFEST", Field: field + ".ref",
				Message: fmt.Sprintf("ref %q 既不在本 bundle 里，也不是已注册的 Tool", rule.Ref),
				Hint:    "把 Tool 一起写进 bundle，或先用 gear_catalog(kind=tool) 确认 id",
			})
			continue
		}
		if _, err := exposedInputSchema(t, rule); err != nil {
			out = append(out, engine.Diagnostic{
				Code: "BAD_APP_MANIFEST", Field: field,
				Message: err.Error(),
				Hint:    "input_schema 顶层须为 object；args 的键须出现在 properties 里",
			})
		}
	}
	return out
}

func findBundleTool(b *bundle, id string) *model.Tool {
	for _, t := range b.Tools {
		if t.ID == id {
			return t
		}
	}
	return nil
}
