package mcp

// expose_tools_test.go：App 清单 mcp.tools 策展区的端到端回归（REQ-MCP-EXPOSE-TOOLS /
// REQ-APP-MCP-CURATE）。走真协议（in-memory transport），不直接调 Go 函数。
//
// 覆盖点按「最容易静默失守」排序：① 固定参数注入且客户端改不掉；② 输出顶层是对象；
// ③ 没有策展区/作用域时 fail-closed；④ 清单字段真的被解析并落库（非 KnownFields 会静默丢弃）；
// ⑤ 跨 App ref 与 schema 映射错误在写作期就报出来。

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"gear/internal/engine"
	"gear/internal/model"
	"gear/internal/registry"
)

// curatedToolYAML 的 session 入参专用于验证「清单固定注入、AI 不必懂」：
// Tool 会把它原样回显，于是能断言服务端覆盖了客户端传值。
const curatedToolYAML = `kind: Tool
id: study__plan_propose
name: 提计划
description: 冒烟用：回显入参
runtime: lua
entry: |
  return { echo = ctx.input.name, session = ctx.input.session }
input_schema:
  type: object
  properties:
    name: {type: string}
    session: {type: string}
  required: [name, session]
output_schema:
  type: object
versions:
  - version: "1.0.0"
    state: stable
`

const curatedDesc = "三段式①：先给家长看计划。禁止直接调用 confirm，必须经过家长确认这一步。"

func seedCuratedTool(t *testing.T, reg *registry.Registry) {
	t.Helper()
	b, diags := parseBundle(curatedToolYAML)
	if len(diags) > 0 {
		t.Fatalf("策展 Tool 应可解析: %v", diags)
	}
	for _, tl := range b.Tools {
		if err := reg.UpsertTool(tl); err != nil {
			t.Fatalf("预置策展 Tool 失败: %v", err)
		}
	}
}

func seedCuratedApp(t *testing.T, reg *registry.Registry, rules []model.AppMCPTool) {
	t.Helper()
	if err := reg.UpsertApp(&model.App{
		ID: "study", Name: "学习陪练", EntryFlow: "study.up",
		MCP: model.AppMCP{Tools: rules},
	}); err != nil {
		t.Fatalf("预置 App 失败: %v", err)
	}
}

func curatedRules() []model.AppMCPTool {
	return []model.AppMCPTool{
		{
			Ref: "study__plan_propose", Alias: "study_plan_propose",
			Description: curatedDesc, Args: map[string]any{"session": "parent"},
		},
		{Ref: "study__plan_propose", Alias: "study_plan_preview"},
	}
}

func newExposeFixture(t *testing.T, rules []model.AppMCPTool, opts ...Option) *smokeFixture {
	t.Helper()
	return newSmokeFixtureWith(t, func(reg *registry.Registry) {
		seedCuratedTool(t, reg)
		seedCuratedApp(t, reg, rules)
	}, opts...)
}

// newAssemblyServer 只装配、不启动 transport：fail-closed 是**装配期**错误，
// 若走 newSmokeFixtureWith，错误会被 `go s.Run(...)` 里的 `_ =` 吞掉，测不到。
func newAssemblyServer(t *testing.T, rules []model.AppMCPTool, opts ...Option) *Server {
	t.Helper()
	t.Setenv("GEAR_EXEC_DB", filepath.Join(t.TempDir(), "exec.db"))
	reg, err := registry.Open(filepath.Join(t.TempDir(), "registry.db"))
	if err != nil {
		t.Fatalf("打开 registry 失败: %v", err)
	}
	capReg := engine.NewCapabilityRegistry()
	agent, err := engine.SetupExtensions(capReg, reg, "")
	if err != nil {
		t.Fatalf("初始化扩展失败: %v", err)
	}
	if rules != nil {
		seedCuratedTool(t, reg)
		seedCuratedApp(t, reg, rules)
	}
	eng := engine.New(reg, engine.WithCapabilities(capReg), engine.WithAIAgent(agent))
	s, err := New(reg, eng, opts...)
	if err != nil {
		t.Fatalf("构造 MCP Server 失败: %v", err)
	}
	t.Cleanup(func() {
		_ = s.Close()
		_ = reg.Close()
	})
	return s
}

func findTool(t *testing.T, tools []*mcp.Tool, name string) *mcp.Tool {
	t.Helper()
	for _, tt := range tools {
		if tt.Name == name {
			return tt
		}
	}
	t.Fatalf("工具面里找不到 %s", name)
	return nil
}

func inputSchemaMap(t *testing.T, tt *mcp.Tool) map[string]any {
	t.Helper()
	raw, err := json.Marshal(tt.InputSchema)
	if err != nil {
		t.Fatalf("序列化 inputSchema 失败: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("inputSchema 不是对象: %v", err)
	}
	return m
}

// 策展暴露的主链路：alias / description 覆盖、session 从 schema 里消失、
// 调用时服务端补 session=parent、输出是 {"kind","id","output"} 顶层对象。
func TestMCPExposeToolsCurated(t *testing.T) {
	f := newExposeFixture(t, curatedRules(), WithExpose(ExposeTools), WithScope("study"))
	tools, err := f.session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	exposed := 0
	for _, tt := range tools.Tools {
		if tt.Name == "study_plan_propose" || tt.Name == "study_plan_preview" {
			exposed++
		}
		if strings.HasPrefix(tt.Name, "study__") {
			t.Fatalf("策展工具应按 alias 暴露，不应出现裸 ref %s", tt.Name)
		}
	}
	if exposed != 2 {
		t.Fatalf("工具数应等于清单条数 2，实际 %d", exposed)
	}

	tt := findTool(t, tools.Tools, "study_plan_propose")
	if tt.Description != curatedDesc {
		t.Fatalf("description 覆盖未生效: %q", tt.Description)
	}
	schema := inputSchemaMap(t, tt)
	props, _ := schema["properties"].(map[string]any)
	if _, ok := props["session"]; ok {
		t.Fatalf("args 固定参数 session 不应出现在对外 schema 里: %v", props)
	}
	if _, ok := props["name"]; !ok {
		t.Fatalf("其余参数必须保留: %v", props)
	}
	for _, r := range requiredKeys(t, schema) {
		if r == "session" {
			t.Fatalf("args 固定参数 session 不应留在 required 里: %v", schema["required"])
		}
	}

	var out map[string]any
	callTool(t, f.session, "study_plan_propose", map[string]any{"name": "东东"}, &out)
	if out["kind"] != "tool" || out["id"] != "study__plan_propose" {
		t.Fatalf("输出形态应复用 gear_run(kind=tool) 的包装: %v", out)
	}
	output, ok := out["output"].(map[string]any)
	if !ok {
		t.Fatalf("output 应为对象: %v", out["output"])
	}
	if output["session"] != "parent" {
		t.Fatalf("服务端应自动补 session=parent: %v", output)
	}
	if output["echo"] != "东东" {
		t.Fatalf("客户端参数应正常透传: %v", output)
	}
}

func requiredKeys(t *testing.T, schema map[string]any) []string {
	t.Helper()
	items, _ := schema["required"].([]any)
	out := make([]string, 0, len(items))
	for _, it := range items {
		s, ok := it.(string)
		if !ok {
			t.Fatalf("required 含非字符串: %v", it)
		}
		out = append(out, s)
	}
	return out
}

// 客户端就算硬传 session=child，也改不掉护栏参数（服务端后写覆盖）。
func TestMCPExposeToolsArgsCannotBeOverridden(t *testing.T) {
	f := newExposeFixture(t, curatedRules(), WithExpose(ExposeTools), WithScope("study"))
	var out map[string]any
	callTool(t, f.session, "study_plan_propose", map[string]any{
		"name": "东东", "session": "child",
	}, &out)
	output := out["output"].(map[string]any)
	if output["session"] != "parent" {
		t.Fatalf("客户端不得覆盖固定参数 session: %v", output)
	}
}

// --expose all：Flow 快照与策展工具同时出现。
func TestMCPExposeAllIncludesBoth(t *testing.T) {
	f := newSmokeFixtureWith(t, func(reg *registry.Registry) {
		applySmokeBundle(t, reg) // 提供 smoke.up Flow 快照
		seedCuratedTool(t, reg)
		seedCuratedApp(t, reg, curatedRules())
	}, WithExpose(ExposeAll), WithScope("study"))
	tools, err := f.session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	names := map[string]bool{}
	for _, tt := range tools.Tools {
		names[tt.Name] = true
	}
	if !names["flow__smoke.up"] || !names["study_plan_propose"] {
		t.Fatalf("--expose all 应同时暴露 Flow 快照与策展工具: %v", names)
	}
}

// fail-closed 三连：缺 --scope / App 未注册 / 没有策展区，一律拒绝装配。
func TestMCPExposeToolsFailClosed(t *testing.T) {
	cases := []struct {
		name  string
		rules []model.AppMCPTool
		opts  []Option
		want  string
	}{
		{
			name: "缺 scope", rules: curatedRules(),
			opts: []Option{WithExpose(ExposeTools)}, want: "--scope",
		},
		{
			name: "App 未注册", rules: nil,
			opts: []Option{WithExpose(ExposeTools), WithScope("study")}, want: "未注册",
		},
		{
			name: "无策展区", rules: []model.AppMCPTool{},
			opts: []Option{WithExpose(ExposeTools), WithScope("study")}, want: "策展区",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newAssemblyServer(t, tc.rules, tc.opts...)
			if _, err := s.newMCPServer(); err == nil {
				t.Fatal("缺少护栏时应拒绝装配 MCP Server（不许退回裸暴露）")
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误信息应含 %q，实际: %v", tc.want, err)
			}
		})
	}
}

// 跨 App ref：写作期诊断 + 装配期拒绝，两道都要有。
func TestMCPExposeToolsRejectsForeignRef(t *testing.T) {
	rules := []model.AppMCPTool{{Ref: "other__plan_propose", Alias: "steal"}}
	s := newAssemblyServer(t, rules, WithExpose(ExposeTools), WithScope("study"))
	if _, err := s.newMCPServer(); err == nil || !strings.Contains(err.Error(), "不属于 App") {
		t.Fatalf("跨 App ref 应在装配期被拒: %v", err)
	}

	session := newSmokeSession(t)
	var out map[string]any
	callTool(t, session, "gear_validate", map[string]any{"yaml": `kind: App
id: study
entry_flow: study.up
mcp:
  tools:
    - ref: other__plan_propose
      alias: steal
`}, &out)
	if out["valid"] != false {
		t.Fatalf("跨 App ref 应判非法: %v", out)
	}
	if !diagnosticsContain(t, out, "BAD_APP_MANIFEST") {
		t.Fatalf("应报 BAD_APP_MANIFEST: %v", out["diagnostics"])
	}
}

// args 键不在 input_schema.properties 里 = 清单写错，必须在校验期报出来。
func TestMCPExposeToolsRejectsUnknownArgs(t *testing.T) {
	session := newSmokeSession(t)
	var out map[string]any
	callTool(t, session, "gear_validate", map[string]any{"yaml": `kind: App
id: study
entry_flow: study.up
mcp:
  tools:
    - ref: study__plan_propose
      args: {tenant: x}
---
` + curatedToolYAML}, &out)
	if out["valid"] != false || !diagnosticsContain(t, out, "BAD_APP_MANIFEST") {
		t.Fatalf("未知 args 键应报 BAD_APP_MANIFEST: %v", out)
	}
}

func diagnosticsContain(t *testing.T, out map[string]any, code string) bool {
	t.Helper()
	items, _ := out["diagnostics"].([]any)
	for _, it := range items {
		m, ok := it.(map[string]any)
		if ok && m["code"] == code {
			return true
		}
	}
	return false
}

// ★ ParseBundle 用 yaml.Unmarshal（非 KnownFields）：如果 model.App 漏了 mcp 字段，
// 清单会「写进去、不报错、但完全不生效」——最典型的静默降级。这里钉死解析与落库往返。
func TestMCPAppMCPSurvivesParseAndRegistry(t *testing.T) {
	const appYAML = `kind: App
id: study
entry_flow: study.up
mcp:
  tools:
    - ref: study__plan_propose
      alias: study_plan_preview
      description: 三段式②：只预览，禁止自动 confirm。
      args: {session: parent}
`
	b, diags := parseBundle(appYAML)
	if len(diags) > 0 {
		t.Fatalf("含 mcp 区的 App 应可解析: %v", diags)
	}
	if len(b.Apps) != 1 || len(b.Apps[0].MCP.Tools) != 1 {
		t.Fatalf("mcp.tools 解析丢失: %+v", b.Apps)
	}
	rule := b.Apps[0].MCP.Tools[0]
	if rule.Alias != "study_plan_preview" || rule.Args["session"] != "parent" {
		t.Fatalf("策展字段解析不完整: %+v", rule)
	}
	if rule.ExposedName() != "study_plan_preview" {
		t.Fatalf("alias 应优先作为对外名字: %+v", rule)
	}

	reg, err := registry.Open(filepath.Join(t.TempDir(), "registry.db"))
	if err != nil {
		t.Fatalf("打开 registry 失败: %v", err)
	}
	defer reg.Close()
	if err := reg.UpsertApp(b.Apps[0]); err != nil {
		t.Fatalf("UpsertApp 失败: %v", err)
	}
	got, err := reg.GetApp("study")
	if err != nil {
		t.Fatalf("GetApp 失败: %v", err)
	}
	if len(got.MCP.Tools) != 1 || got.MCP.Tools[0].Args["session"] != "parent" {
		t.Fatalf("mcp 区落库往返丢失: %+v", got.MCP)
	}
}

// --expose 的值必须校验：拼错不能静默当成「不暴露」。
func TestParseExposeMode(t *testing.T) {
	for in, want := range map[string]ExposeMode{
		"": ExposeNone, "flows": ExposeFlows, " TOOLS ": ExposeTools, "ALL": ExposeAll,
	} {
		got, err := ParseExposeMode(in)
		if err != nil || got != want {
			t.Fatalf("ParseExposeMode(%q) = %q, %v；想要 %q", in, got, err, want)
		}
	}
	if _, err := ParseExposeMode("everything"); err == nil {
		t.Fatal("未知 --expose 值必须报错")
	}
	if ExposeAll.wantsFlows() != true || ExposeAll.wantsTools() != true ||
		ExposeFlows.wantsTools() || ExposeTools.wantsFlows() || ExposeNone.wantsFlows() {
		t.Fatal("ExposeMode 的组合判定错了")
	}
}

// WithExposeFlows 是给仓内旧调用方留的兼容别名，必须仍只写同一份状态（不产生第二套口径）。
func TestWithExposeFlowsAlias(t *testing.T) {
	var s Server
	WithExposeFlows(true)(&s)
	if s.expose != ExposeFlows {
		t.Fatalf("别名应等价于 WithExpose(ExposeFlows)，实际 %q", s.expose)
	}
	WithExposeFlows(false)(&s)
	if s.expose != ExposeNone {
		t.Fatalf("别名关闭应回到 ExposeNone，实际 %q", s.expose)
	}
}
