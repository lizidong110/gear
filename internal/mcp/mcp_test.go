package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"gear/internal/artifact"
	"gear/internal/cap"
	"gear/internal/engine"
	"gear/internal/gearhome"
	"gear/internal/mcp/guide"
	"gear/internal/registry"
)

// 端到端冒烟：MCP 客户端 → MCP Server → registry/engine/app 全链路。
// 用 SDK 的 in-memory transport 走真协议（不是直接调 Go 函数），
// 覆盖「读目录 → 校验 → 落库 → 执行 → 观察 → 起应用」的智能体闭环。
//
// Tool 用 lua runtime（沙箱内嵌，不依赖外部进程/PATH）。

// smokeFixture 是一套完整的冒烟环境（session + registry 句柄 + registry 路径）。
type smokeFixture struct {
	session *mcp.ClientSession
	reg     *registry.Registry
	engine  *engine.Engine
	server  *Server
	regPath string
}

// newSmokeFixture 装配冒烟环境。
//
// registryPath 必须显式交给 Server（WithRegistryPath）：gear_app 派生子进程时要继承它，
// 否则子进程会读默认库——测试里就是「启动了另一个库里的同名 Flow」。
func newSmokeFixture(t *testing.T, opts ...Option) *smokeFixture {
	t.Helper()
	return newSmokeFixtureWith(t, nil, opts...)
}

// newSmokeFixtureWith 同上，但允许在**启动 MCP Server 之前**先往 registry 里放数据。
//
// 为什么需要：反向暴露（`flow__{id}`）是**启动时快照**——工具面在 newMCPServer 那一刻固定，
// 之后注册的 Flow 本次进程内不会被暴露（刻意的：工具数必须可预测，见 Server.expose）。
// 所以「--expose flows 生效」的场景必须在起服务之前就有 Flow 落库。
func newSmokeFixtureWith(t *testing.T, prepopulate func(*registry.Registry), opts ...Option) *smokeFixture {
	t.Helper()
	// 执行记录库指向临时目录，避免测试污染 build/gear.db。
	t.Setenv("GEAR_EXEC_DB", filepath.Join(t.TempDir(), "exec.db"))

	dir := t.TempDir()
	regPath := filepath.Join(dir, "registry.db")
	reg, err := registry.Open(regPath)
	if err != nil {
		t.Fatalf("打开 registry 失败: %v", err)
	}
	capReg := engine.NewCapabilityRegistry()
	agent, err := engine.SetupExtensions(capReg, reg, "")
	if err != nil {
		t.Fatalf("初始化扩展失败: %v", err)
	}
	if prepopulate != nil {
		prepopulate(reg)
	}
	eng := engine.New(reg, engine.WithCapabilities(capReg), engine.WithAIAgent(agent))
	all := append([]Option{WithRegistryPath(regPath)}, opts...)
	s, err := New(reg, eng, all...)
	if err != nil {
		t.Fatalf("构造 MCP Server 失败: %v", err)
	}

	ctx := context.Background()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	go func() { _ = s.Run(ctx, serverTransport) }() // 阻塞运行，随客户端断开而退出

	client := mcp.NewClient(&mcp.Implementation{Name: "smoke-client", Version: "0.0.0"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("客户端连接失败: %v", err)
	}
	t.Cleanup(func() {
		_ = session.Close()
		_ = s.Close()
		_ = reg.Close()
	})
	return &smokeFixture{session: session, reg: reg, engine: eng, server: s, regPath: regPath}
}

// newSmokeSession 是只关心 session 的用例的快捷方式。
func newSmokeSession(t *testing.T) *mcp.ClientSession {
	t.Helper()
	return newSmokeFixture(t).session
}

// callTool 调一个 MCP 工具并断言成功，返回结构化输出（JSON 反序列化到 out）。
//
// **线格式红线集中守卫**（REQ-MCP-WIRE-OBJECT）：成功返回时 `structuredContent` 顶层
// **必须是 JSON 对象**。MCP 2025-06-18 只允许对象；go-sdk v1.8.0 依 SEP-2106 草案放行
// 数组/标量，且自带客户端同样宽松，于是「裸发数组」能在本地全绿、却在严格客户端
// （WorkBuddy 的 zod）被判协议不合规并**丢弃整条响应**——表面症状是「这个 MCP 一个工具
// 都用不了」，极难定位。断言放在这里，任何走 callTool 的工具都自动受保护。
func callTool(t *testing.T, session *mcp.ClientSession, name string, args map[string]any, out any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("工具 %s 调用失败: %v", name, err)
	}
	if res.IsError {
		cb, _ := json.Marshal(res.Content)
		sb, _ := json.Marshal(res.StructuredContent)
		t.Fatalf("工具 %s 返回错误: content=%s structured=%s", name, cb, sb)
	}
	var raw any = res.StructuredContent
	if raw == nil {
		// 结构化输出为空：无对象可校验，保留 out 的零值直接返回。
		return
	}
	if _, ok := raw.(map[string]any); !ok {
		b, _ := json.Marshal(raw)
		t.Fatalf("工具 %s 的 structuredContent 顶层必须是 JSON 对象（MCP 2025-06-18；"+
			"列表类结果须经 objectResult 包具名键），实际: %s", name, b)
	}
	b, merr := json.Marshal(raw)
	if merr != nil {
		t.Fatalf("工具 %s 输出序列化失败: %v", name, merr)
	}
	if out != nil {
		if uerr := json.Unmarshal(b, out); uerr != nil {
			t.Fatalf("工具 %s 输出解析失败: %s", name, b)
		}
	}
}

// expectToolError 断言工具调用以错误收场（守卫/校验/前置条件路径）。
//
// 新增断言：错误文本必须带 `[CODE]` 前缀（结构化错误契约）——没有码的错误会让智能体
// 只能读中文猜原因，自愈回路就断了。
func expectToolError(t *testing.T, session *mcp.ClientSession, name string, args map[string]any, wantSubstr string) string {
	t.Helper()
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("工具 %s 协议层失败: %v", name, err)
	}
	if !res.IsError {
		b, _ := json.Marshal(res.StructuredContent)
		t.Fatalf("工具 %s 应返回错误，实际成功: %s", name, b)
	}
	text := contentText(res)
	if !strings.HasPrefix(text, "[") || !strings.Contains(text, "]") {
		t.Fatalf("工具 %s 的错误文本必须带 [CODE] 前缀（结构化错误契约），实际: %s", name, text)
	}
	if wantSubstr != "" && !strings.Contains(text, wantSubstr) {
		t.Fatalf("工具 %s 错误信息应含 %q，实际: %s", name, wantSubstr, text)
	}
	return text
}

// contentText 把工具错误结果里的文本拼起来（供断言读取 code 与 hint）。
func contentText(res *mcp.CallToolResult) string {
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String()
}

// ---- 列表类工具的返回信封 ----
//
// 列表类工具的 structuredContent 是「顶层对象 + 具名键」（见 mcp.go 的 objectResult），
// 故解码必须落到信封类型，而不能用裸切片——裸切片解码会因 JSON 里是 object、
// Go 里是 array 而直接失败（这本来就是线格式红线的另一种表现形式）。

// capsEnvelope 对应 gear_capabilities 的 {capabilities:[...]}。
type capsEnvelope struct {
	Capabilities []map[string]any `json:"capabilities"`
}

// ---- 测试数据 ----

// smokeToolYAML 是**不带 kind** 的 Tool（同时覆盖「无 kind 时按字段启发式分类」这条兼容路径）。
const smokeToolYAML = `id: hello
name: Hello Tool
description: 冒烟用问候工具（lua）
runtime: lua
entry: |
  return { greeting = "hi " .. ctx.input.name }
input_schema:
  type: object
  properties:
    name:
      type: string
  required:
    - name
output_schema:
  type: object
versions:
  - version: "1.0.0"
    state: stable
`

// smokeBundleYAML 是标准交付形态：App 清单 + Tool + Flow，id 全部带 `smoke` 命名空间。
const smokeBundleYAML = `kind: App
id: smoke
name: 冒烟应用
description: 校验 → 落库 → 运行 的最小闭环
entry_flow: smoke.up
start:
  restart: never
---
kind: Tool
id: smoke__hello
name: Hello Tool
description: 冒烟用问候工具（lua）
runtime: lua
entry: |
  return { greeting = "hi " .. ctx.input.name }
input_schema:
  type: object
  properties:
    name:
      type: string
  required:
    - name
output_schema:
  type: object
versions:
  - version: "1.0.0"
    state: stable
---
kind: Flow
id: smoke.up
name: 冒烟 Flow
description: 两步串联（第二步引用第一步输出）
vars:
  who: smoke
steps:
  - id: s1
    call: smoke__hello
    input:
      name: "${vars.who}"
  - id: s2
    call: smoke__hello
    depends_on:
      - s1
    input:
      name: "${steps.s1.output.greeting}-reply"
`

// svcFlowYAML 是含 service 长驻 step 的 Flow（不得被 gear_run 接受）。
const svcFlowYAML = `id: svc_demo
name: SVC Demo
description: 含 service 长驻 step 的 Flow
steps:
  - id: s1
    call: hello
    mode: service
    input:
      name: x
`

// ---- 主链路 ----

// 主链路：catalog（空）→ validate（不落库）→ apply → catalog/describe → run → execution
// → delete（守卫）。一个测试串完整闭环，断言中间态。
func TestMCPAgentClosedLoop(t *testing.T) {
	f := newSmokeFixture(t)
	session := f.session

	// 1) 目录初始为空。
	var env map[string]any
	callTool(t, session, "gear_catalog", map[string]any{"kind": "tool"}, &env)
	if fmt.Sprint(env["total"]) != "0" {
		t.Fatalf("初始目录应为空，实际 %v", env["total"])
	}

	// 2) validate 是「不落库」的：跑完目录仍为空。
	var verdict map[string]any
	callTool(t, session, "gear_validate", map[string]any{"yaml": smokeBundleYAML}, &verdict)
	if verdict["valid"] != true {
		t.Fatalf("bundle 应校验通过: %v", verdict)
	}
	callTool(t, session, "gear_catalog", map[string]any{}, &env)
	if fmt.Sprint(env["total"]) != "0" {
		t.Fatalf("validate 不得落库，实际目录有 %v 项", env["total"])
	}

	// 3) apply 落库（原子、幂等），返回 created 清单。
	//    顺序是**依赖顺序**（App → Tool → Flow → Hook），不是字母序：App 是聚合根先落，
	//    然后先落被引用者，中途失败时「已写入什么」才是可解释的（见 persistBundle）。
	var applied map[string]any
	callTool(t, session, "gear_apply", map[string]any{"bundle": smokeBundleYAML}, &applied)
	if applied["valid"] != true {
		t.Fatalf("apply 应成功: %v", applied)
	}
	// app: 前缀是 2026-09-26 起的变更（App 清单入 registry，此前只校验后丢弃）。
	if fmt.Sprint(applied["created"]) != "[app:smoke tool:smoke__hello flow:smoke.up]" {
		t.Fatalf("created 清单不符（应为依赖顺序 app→tool→flow）: %v", applied["created"])
	}

	// 4) 幂等：二次 apply 变成 updated，不产生重复。
	var again map[string]any
	callTool(t, session, "gear_apply", map[string]any{"bundle": smokeBundleYAML}, &again)
	if fmt.Sprint(again["created"]) != "[]" {
		t.Fatalf("二次 apply 不应再创建: %v", again["created"])
	}
	if !strings.Contains(fmt.Sprint(again["updated"]), "tool:smoke__hello") {
		t.Fatalf("二次 apply 应报告 updated: %v", again["updated"])
	}

	// 5) catalog 是摘要：不含 schema（P3 的根治）。
	callTool(t, session, "gear_catalog", map[string]any{"kind": "tool"}, &env)
	items, _ := env["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("catalog 应含 1 个 Tool: %v", env["items"])
	}
	first, _ := items[0].(map[string]any)
	if _, hasSchema := first["input_schema"]; hasSchema {
		t.Fatalf("catalog 不得返回 schema（应走 gear_describe）: %v", first)
	}

	// 6) describe 才给全量定义。
	var desc map[string]any
	callTool(t, session, "gear_describe", map[string]any{"kind": "tool", "id": "smoke__hello"}, &desc)
	obj, _ := desc["object"].(map[string]any)
	if obj["id"] != "smoke__hello" {
		t.Fatalf("describe 应返回完整 Tool: %v", obj)
	}
	if _, ok := obj["input_schema"]; !ok {
		t.Fatalf("describe 应含 input_schema: %v", obj)
	}

	// 7) gear_run 异步：返回 execution_id，wait_seconds 短等后拿到 succeeded。
	var run map[string]any
	callTool(t, session, "gear_run", map[string]any{
		"kind": "flow", "id": "smoke.up",
		"vars": map[string]any{"who": "gear"}, "wait_seconds": 15,
	}, &run)
	if run["status"] != "succeeded" {
		t.Fatalf("gear_run(wait_seconds=15) 应已完成: %v", run)
	}
	execID, _ := run["execution_id"].(string)
	if execID == "" {
		t.Fatalf("gear_run 未返回 execution_id: %v", run)
	}

	// 8) gear_execution 能取回结果与日志。
	var exec map[string]any
	callTool(t, session, "gear_execution", map[string]any{"id": execID, "with_logs": true}, &exec)
	if exec["status"] != "succeeded" {
		t.Fatalf("gear_execution 应返回 succeeded: %v", exec)
	}
	result, _ := exec["result"].(map[string]any)
	steps, _ := result["steps"].(map[string]any)
	s2, _ := steps["s2"].(map[string]any)
	s2out, _ := s2["output"].(map[string]any)
	// s1: name=${vars.who}=gear → "hi gear"；s2: name=<s1.greeting>-reply → "hi hi gear-reply"
	if fmt.Sprint(s2out["greeting"]) != "hi hi gear-reply" {
		t.Fatalf("s2 应引用 s1 输出（hi hi gear-reply）: %v", s2)
	}
	if _, ok := exec["logs"]; !ok {
		t.Fatalf("with_logs=true 时应返回 logs: %v", exec)
	}

	// 9) gear_run(kind=tool) 同步返回输出。
	var toolRun map[string]any
	callTool(t, session, "gear_run", map[string]any{
		"kind": "tool", "id": "smoke__hello", "input": map[string]any{"name": "direct"},
	}, &toolRun)
	out, _ := toolRun["output"].(map[string]any)
	if fmt.Sprint(out["greeting"]) != "hi direct" {
		t.Fatalf("run(tool) 输出不符: %v", toolRun)
	}

	// 10) 删除守卫：Tool 被 Flow 引用 → 拒绝（错误必须带码）。
	expectToolError(t, session, "gear_delete", map[string]any{"kind": "tool", "id": "smoke__hello"}, "in use")

	// 11) 删除守卫（第 4 条）：Flow 是某 App 的 entry_flow → 拒绝。
	//
	// 与第 1/2/3 条形质不同：这条不是「谁在引用我」，而是「删了我之后 App 会悬空」——
	// App.EntryFlow 只是个字符串，两表之间没有外键，注销 Flow 不留任何痕迹。
	expectToolError(t, session, "gear_delete", map[string]any{"kind": "flow", "id": "smoke.up"}, "in use")

	// 12) 清理顺序：App → Flow → Tool。**顺序本身是语义**：App 只是聚合描述（删它不带守卫），
	// 摘掉它之后 entry_flow 才允许注销，最后才是被它引用的 Tool。
	callTool(t, session, "gear_delete", map[string]any{"kind": "app", "id": "smoke"}, nil)
	callTool(t, session, "gear_delete", map[string]any{"kind": "flow", "id": "smoke.up"}, nil)
	callTool(t, session, "gear_delete", map[string]any{"kind": "tool", "id": "smoke__hello"}, nil)
}

// TestMCPValidateReportsAllDiagnostics 「编译器」必须一次报多条：把 5 处改坏，
// 拿到 5 条各自带 code/step 的诊断，且**注册表一个字节都没变**。
func TestMCPValidateReportsAllDiagnostics(t *testing.T) {
	f := newSmokeFixture(t)
	session := f.session

	const brokenBundle = `kind: App
id: diag
entry_flow: diag.up
---
kind: Tool
id: diag__bad
name: Bad Tool
description: runtime 未实现
runtime: wasm
entry: x
input_schema:
  type: object
output_schema:
  type: object
versions:
  - version: "1.0.0"
    state: stable
---
kind: Flow
id: diag.up
steps:
  - id: s1
    call: no_such_tool
  - id: s2
    call: bogus.method
  - id: s3
    call: db.exec
    when: "${vars.flag}"
    input:
      sql: "SELECT 1"
  - id: s4
    call: db.exec
    input:
      sql: "${bogus.x}"
`
	var out map[string]any
	callTool(t, session, "gear_validate", map[string]any{"yaml": brokenBundle}, &out)
	if out["valid"] != false {
		t.Fatalf("坏 bundle 应判 invalid: %v", out)
	}
	diags, _ := out["diagnostics"].([]any)
	if len(diags) < 5 {
		b, _ := json.Marshal(diags)
		t.Fatalf("应一次报出 ≥5 条诊断（不 fail-fast），实际 %d 条: %s", len(diags), b)
	}
	want := map[string]bool{
		"BAD_TOOL_STRUCTURE":  false,
		"UNKNOWN_CALL_TARGET": false,
		"UNKNOWN_CAPABILITY":  false,
		"BAD_WHEN":            false,
		"BAD_TEMPLATE":        false,
	}
	stepsSeen := map[string]bool{}
	for _, d := range diags {
		m, _ := d.(map[string]any)
		code, _ := m["code"].(string)
		if _, ok := want[code]; ok {
			want[code] = true
		}
		if s, _ := m["step"].(string); s != "" {
			stepsSeen[s] = true
		}
		for _, k := range []string{"code", "message", "hint"} {
			if _, ok := m[k]; !ok {
				t.Fatalf("诊断缺 %s 字段: %v", k, m)
			}
		}
	}
	for code, seen := range want {
		if !seen {
			t.Fatalf("应报出 %s 码的诊断，实际诊断: %v", code, diags)
		}
	}
	for _, st := range []string{"s1", "s2", "s3", "s4"} {
		if !stepsSeen[st] {
			t.Fatalf("step %s 的问题应被定位到（step 字段）: %v", st, diags)
		}
	}

	// 关键：validate 绝不落库——注册表计数必须不变（全为 0）。
	tools, err := f.reg.ListTools()
	if err != nil {
		t.Fatal(err)
	}
	flows, err := f.reg.ListFlows()
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 0 || len(flows) != 0 {
		t.Fatalf("validate 不得改动注册表，实际 tools=%d flows=%d", len(tools), len(flows))
	}
}

// TestMCPApplyIsAtomic 回归锁（REQ-REG-ATOMIC）：多文档里任一非法 → 整体不写入。
//
// 曾因逐个 Upsert 边校验边落库：第 1 个合法写入、第 2 个非法报错，调用方收到 error
// 却已有产物落库；智能体重试整段 YAML 时行为与首次不一致（不静默降级红线）。
func TestMCPApplyIsAtomic(t *testing.T) {
	f := newSmokeFixture(t)
	session := f.session

	const mixed = `kind: Tool
id: good_one
name: Good
description: 合法
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
kind: Tool
id: bad_one
name: Bad
description: runtime 未实现
runtime: wasm
entry: x
input_schema:
  type: object
output_schema:
  type: object
versions:
  - version: "1.0.0"
    state: stable
`
	var out map[string]any
	callTool(t, session, "gear_apply", map[string]any{"bundle": mixed}, &out)
	if out["valid"] != false {
		t.Fatalf("含非法文档的 bundle 应判 invalid: %v", out)
	}
	// valid:false 是**成功返回**（定义有问题不是协议错误），但一个都不该写进去。
	if applied, _ := out["applied"].(map[string]any); applied != nil {
		if ts, _ := applied["tools"].([]any); len(ts) != 0 {
			t.Fatalf("apply 失败时不得写入任何 Tool: %v", applied)
		}
	}
	tools, err := f.reg.ListTools()
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 0 {
		t.Fatalf("apply 失败时不得留下部分写入，实际库中有: %v", tools)
	}
}

// TestMCPApplyRejectsNamespaceViolation 命名空间约束：bundle 内 id 不带应用前缀即拒。
//
// 理由：registry 与 CLI/REST 共享（默认 ~/.gear/registry.db），无前缀约束时一次性生成
// 很容易覆盖掉人手工维护的对象。
func TestMCPApplyRejectsNamespaceViolation(t *testing.T) {
	f := newSmokeFixture(t)
	const bad = `kind: App
id: myapp
entry_flow: other.up
---
kind: Flow
id: other.up
steps:
  - id: s1
    call: time.now
    input: {}
`
	var out map[string]any
	callTool(t, f.session, "gear_validate", map[string]any{"yaml": bad}, &out)
	if out["valid"] != false {
		t.Fatalf("无前缀 id 应被判非法: %v", out)
	}
	joined, _ := json.Marshal(out["diagnostics"])
	if !strings.Contains(string(joined), "ID_NAMESPACE_VIOLATION") {
		t.Fatalf("应报 ID_NAMESPACE_VIOLATION: %s", joined)
	}
	tools, _ := f.reg.ListTools()
	flows, _ := f.reg.ListFlows()
	if len(tools) != 0 || len(flows) != 0 {
		t.Fatalf("校验失败不得落库，实际 tools=%d flows=%d", len(tools), len(flows))
	}
}

// TestMCPApplyConflictNeedsOverwrite overwrite 默认 false：已存在且内容不同的对象拒绝覆盖。
func TestMCPApplyConflictNeedsOverwrite(t *testing.T) {
	f := newSmokeFixture(t)
	session := f.session

	first := `kind: Tool
id: t1
name: 原始
description: 人工维护的版本
runtime: lua
entry: |
  return { v = 1 }
input_schema:
  type: object
output_schema:
  type: object
versions:
  - version: "1.0.0"
    state: stable
`
	callTool(t, session, "gear_apply", map[string]any{"bundle": first}, nil)

	second := strings.Replace(first, "return { v = 1 }", "return { v = 2 }", 1)
	expectToolError(t, session, "gear_apply", map[string]any{"bundle": second}, "APPLY_CONFLICT")

	// 显式 overwrite 才允许覆盖。
	var out map[string]any
	callTool(t, session, "gear_apply", map[string]any{
		"bundle": second, "overwrite": true, "dry_run": true,
	}, &out)
	if out["valid"] != true {
		t.Fatalf("dry_run + overwrite 应通过: %v", out)
	}
	// dry_run 不落库：库里仍是 v=1。
	cur, err := f.reg.GetTool("t1")
	if err != nil || cur == nil {
		t.Fatalf("Tool t1 应仍存在: %v", err)
	}
	if !strings.Contains(cur.Entry, "v = 1") {
		t.Fatalf("dry_run 不得改库，实际 entry=%q", cur.Entry)
	}
}

// TestMCPGuideAndInstructions guide 工具与 instructions 必须同源且都可用。
func TestMCPGuideAndInstructions(t *testing.T) {
	f := newSmokeFixture(t)

	// instructions：guide/summary.md 必须非空（New 已 fail-closed 校验，这里断言内容质量）。
	sum := guide.Summary()
	if !strings.Contains(sum, "gear_app") || !strings.Contains(sum, "八条铁律") {
		t.Fatalf("instructions 内容不像心智模型: %q", sum[:min(120, len(sum))])
	}

	// 每个 topic 都非空，且都是**真实文档**（不是占位符）。
	topics := guide.Topics()
	if len(topics) < 9 {
		t.Fatalf("guide 应有 ≥9 个 topic，实际 %d: %v", len(topics), topics)
	}
	for _, topic := range topics {
		var out map[string]any
		callTool(t, f.session, "gear_guide", map[string]any{"topic": topic}, &out)
		content, _ := out["content"].(string)
		if len(content) < 200 {
			t.Fatalf("topic %s 的内容过短（%d 字符），像是占位符", topic, len(content))
		}
	}

	// 未知 topic 必须报错并列出可用项（不静默返回空串）。
	text := expectToolError(t, f.session, "gear_guide", map[string]any{"topic": "no_such_topic"}, "overview")
	if !strings.Contains(text, "UNKNOWN_TOPIC") {
		t.Fatalf("未知 topic 应带 UNKNOWN_TOPIC 码: %s", text)
	}
}

// TestMCPCapabilitiesTool gear_capabilities 应返回含内核原语（门禁 + 纯函数原子）的能力目录。
func TestMCPCapabilitiesTool(t *testing.T) {
	session := newSmokeSession(t)
	var env capsEnvelope
	callTool(t, session, "gear_capabilities", nil, &env)
	names := map[string]bool{}
	kinds := map[string]string{}
	for _, c := range env.Capabilities {
		n, _ := c["name"].(string)
		k, _ := c["kind"].(string)
		names[n] = true
		kinds[n] = k
	}
	for _, want := range []string{"http", "call", "db", "secret", "event", "subscribe", "config", "fs"} {
		if !names[want] {
			t.Fatalf("能力目录应含内核门禁原语 %s: %v", want, names)
		}
	}
	// 纯函数原子（time/template/crypto/id/validate）：始终可用、无需声明，但必须出现在目录里
	//（否则 agent 根本不知道它们存在——这正是补它们时要顺手堵的缺口）。
	// 2026-09-28 新增 id（uuid/nanoid/code）与 validate（json_schema）：每个新族都必须同步这里，
	// 否则「目录里看得到但这里没锁」= 新族可能悄悄从能力目录掉出去。
	for _, want := range []string{"time", "template", "crypto", "id", "validate"} {
		if kinds[want] != "pure" {
			t.Fatalf("能力目录应含纯函数原子 %s（kind=pure），实际 kind=%q: %v", want, kinds[want], names)
		}
	}
}

// TestMCPPromoteRejectsIllegalJump draft → deprecated 的非法迁移应被状态机拒绝。
func TestMCPPromoteRejectsIllegalJump(t *testing.T) {
	session := newSmokeSession(t)
	draft := `kind: Tool
id: dtool
name: Draft Tool
description: 版本状态机冒烟
runtime: lua
entry: |
  return {}
input_schema:
  type: object
output_schema:
  type: object
versions:
  - version: "1.0.0"
    state: draft
`
	var created map[string]any
	callTool(t, session, "gear_apply", map[string]any{"bundle": draft}, &created)
	expectToolError(t, session, "gear_promote", map[string]any{
		"tool_id": "dtool", "version": "1.0.0", "target": "deprecated",
	}, "")
	// 合法迁移应当成功（确认拒绝的是「跳级」而不是「整个功能」）。
	var ok map[string]any
	callTool(t, session, "gear_promote", map[string]any{
		"tool_id": "dtool", "version": "1.0.0", "target": "testing",
	}, &ok)
	if ok["state"] != "testing" {
		t.Fatalf("draft→testing 应成功: %v", ok)
	}
}

// 回归锁（BUG-1）：无 vars 的 Flow 不得让 catalog / describe 失败。
//
// 曾因 SDK 从 Go 结构体推断输出 schema，而 model.Flow.Vars 是 map[string]any，
// 无 vars 时零值 nil → marshals 成 null → 与推断的 type:object 冲突 → SDK 输出校验
// 拒绝整条调用（JSON-RPC 层错误，不是 tool isError）。而「没写 vars 的 Flow」是
// 绝大多数真实形态，等于读目录第一步就断。
//
// ⚠ 测试取样教训：原 smokeFlowYAML 恰好写了 vars，导致该 bug 长期漏检。
// 本用例**刻意不写 vars**，反向覆盖「缺省值」路径。
func TestMCPListFlowsWithoutVars(t *testing.T) {
	f := newSmokeFixture(t)

	// 无 vars 的 Flow（刻意留空 —— 这是真实场景里最常见的形态）。
	const noVars = `kind: Tool
id: hello
name: Hello
description: 冒烟
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
id: novars
name: NoVars Flow
description: 不声明 vars 的 Flow
steps:
  - id: s1
    call: hello
`
	var created map[string]any
	callTool(t, f.session, "gear_apply", map[string]any{"bundle": noVars}, &created)

	// 关键：这两个调用必须成功返回（此前会抛 JSON-RPC 错误）。
	var env map[string]any
	callTool(t, f.session, "gear_catalog", map[string]any{"kind": "flow"}, &env)
	items, _ := env["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("catalog 应返回含 novars 的列表: %v", env)
	}
	var one map[string]any
	callTool(t, f.session, "gear_describe", map[string]any{"kind": "flow", "id": "novars"}, &one)
	obj, _ := one["object"].(map[string]any)
	if obj["id"] != "novars" {
		t.Fatalf("describe 应返回 novars: %v", one)
	}
}

// TestMCPCatalogIsSummary 目录摘要不得吐 schema / 脚本源码（token 爆炸的根治）。
//
// 允许保留 `versions`——它是**压缩形态**（["1.0.0:stable"]），一个成熟 Tool 动辄十几个版本、
// 每个版本带完整 schema；摘要只报「有哪些版本、什么状态」。真正吃 token 的是
// input_schema / output_schema / entry（lua 源码），这三样必须走 gear_describe 按需取。
func TestMCPCatalogIsSummary(t *testing.T) {
	f := newSmokeFixture(t)
	callTool(t, f.session, "gear_apply", map[string]any{"bundle": smokeBundleYAML}, nil)
	var env map[string]any
	callTool(t, f.session, "gear_catalog", map[string]any{}, &env)

	raw, _ := json.Marshal(env)
	for _, forbidden := range []string{"input_schema", "output_schema", "entry"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("catalog 摘要不得包含 %s（应走 gear_describe 按需取）: %s", forbidden, raw)
		}
	}
	counts, _ := env["counts"].(map[string]any)
	if counts["tool"] == nil || counts["flow"] == nil {
		t.Fatalf("catalog 应给出分类计数: %v", env["counts"])
	}
	// versions 只能是紧凑串列表，不能是版本对象。
	items, _ := env["items"].([]any)
	for _, it := range items {
		m, _ := it.(map[string]any)
		vs, ok := m["versions"].([]any)
		if !ok {
			continue
		}
		for _, v := range vs {
			if _, isObj := v.(map[string]any); isObj {
				t.Fatalf("catalog 的 versions 应是紧凑串列表，不得返回版本对象: %v", m)
			}
		}
	}
}

// TestMCPDoctor doctor 要能报出环境现状（registry 计数、扩展、台账）。
func TestMCPDoctor(t *testing.T) {
	f := newSmokeFixture(t)
	callTool(t, f.session, "gear_apply", map[string]any{"bundle": smokeBundleYAML}, nil)

	var out map[string]any
	callTool(t, f.session, "gear_doctor", nil, &out)
	reg, _ := out["registry"].(map[string]any)
	if fmt.Sprint(reg["tools"]) != "1" || fmt.Sprint(reg["flows"]) != "1" {
		t.Fatalf("doctor 应报出 registry 计数: %v", reg)
	}
	if folder := fmt.Sprint(out["apps_dir"]); folder == "" {
		t.Fatalf("doctor 应报出台账目录: %v", out)
	}
	if _, ok := out["warnings"].([]any); !ok {
		t.Fatalf("doctor 的 warnings 应是数组（即使是空的）: %v", out["warnings"])
	}
}

// TestMCPAppToolArgumentGuards gear_app 的参数守卫与应用台账为空时的表现。
//
// 真实起停应用的用例在 internal/app 包（那里能构造「假 gear」来验证派生与存活）；
// 这里只保证 MCP 层的参数校验与错误码正确。
func TestMCPAppToolArgumentGuards(t *testing.T) {
	f := newSmokeFixture(t)

	// 台账为空时 list 应成功且为空。
	var list map[string]any
	callTool(t, f.session, "gear_app", map[string]any{"action": "list"}, &list)
	if fmt.Sprint(list["total"]) != "0" {
		t.Fatalf("初始台账应为空: %v", list)
	}
	if _, ok := list["orphan"].([]any); !ok {
		t.Fatalf("list 应给出 orphan 数组: %v", list)
	}

	// start 缺 flow_id → 参数错误。
	expectToolError(t, f.session, "gear_app", map[string]any{"action": "start"}, "BAD_ARGUMENT")
	// status/stop 缺 app_id → 参数错误。
	expectToolError(t, f.session, "gear_app", map[string]any{"action": "status"}, "BAD_ARGUMENT")
	// 未知 action → 参数错误。
	expectToolError(t, f.session, "gear_app", map[string]any{"action": "bogus"}, "BAD_ARGUMENT")
	// 不存在的 app_id → APP_NOT_FOUND。
	expectToolError(t, f.session, "gear_app", map[string]any{"action": "status", "app_id": "app_nope"}, "APP_NOT_FOUND")
}

// TestMCPRunFlowRejectsServiceFlow Flow 含 service 长驻 step 时，gear_run 必须拒绝
// （须 gear_app(start) 监管），与 REQ-FLOW-RUN-REJECTS-SERVICE 同口径。
//
// 「拒绝」不是缺陷：长驻 Flow 不结束，同步等待必然超时。错误里必须带
// SERVICE_FLOW_SYNC_RUN，让智能体知道该换哪条路。
func TestMCPRunFlowRejectsServiceFlow(t *testing.T) {
	f := newSmokeFixture(t)
	// svc_demo 的 call 指向 hello，把它一起落库（否则静态校验会先报 UNKNOWN_CALL_TARGET）。
	callTool(t, f.session, "gear_apply", map[string]any{"bundle": smokeToolYAML}, nil)
	callTool(t, f.session, "gear_apply", map[string]any{"bundle": svcFlowYAML}, nil)

	text := expectToolError(t, f.session, "gear_run", map[string]any{"kind": "flow", "id": "svc_demo"}, "service")
	if !strings.Contains(text, "SERVICE_FLOW_SYNC_RUN") {
		t.Fatalf("service Flow 被拒时应带 SERVICE_FLOW_SYNC_RUN 码: %s", text)
	}
}

// applySmokeBundle 在**起服务之前**把 smoke bundle 直接落库。
//
// 用 registry 直连而不是 gear_apply：这里要的正是「服务启动时库里已有对象」，
// 而 gear_apply 只能在服务起来之后用（于是永远赶不上启动时快照）。
func applySmokeBundle(t *testing.T, reg *registry.Registry) {
	t.Helper()
	b, diags := parseBundle(smokeBundleYAML)
	if len(diags) > 0 {
		t.Fatalf("smoke bundle 应可解析: %v", diags)
	}
	for _, tl := range b.Tools {
		if err := reg.UpsertTool(tl); err != nil {
			t.Fatalf("预置 Tool 失败: %v", err)
		}
	}
	for _, fl := range b.Flows {
		if err := reg.UpsertFlow(fl); err != nil {
			t.Fatalf("预置 Flow 失败: %v", err)
		}
	}
}

// 反向暴露默认关闭：flow__{id} 不出现；显式 WithExpose(ExposeFlows) 才暴露。
//
// 两者都要在**起服务之前**把 Flow 落库：工具面是启动时快照（见 newSmokeFixtureWith）。
func TestMCPReverseExposeFlow(t *testing.T) {
	// 默认：不暴露（工具数必须可预测 → 模型选择准确率）。
	fDefault := newSmokeFixtureWith(t, func(reg *registry.Registry) { applySmokeBundle(t, reg) })
	tools, err := fDefault.session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools 失败: %v", err)
	}
	for _, tt := range tools.Tools {
		if strings.HasPrefix(tt.Name, "flow__") {
			t.Fatalf("默认不得反向暴露 Flow（工具数不可预测），实际有 %s", tt.Name)
		}
	}
	// 不暴露 ≠ 不能跑：gear_run 才是主入口。
	var plain map[string]any
	callTool(t, fDefault.session, "gear_run", map[string]any{
		"kind": "flow", "id": "smoke.up", "vars": map[string]any{"who": "noexpose"}, "wait_seconds": 15,
	}, &plain)
	if plain["status"] != "succeeded" {
		t.Fatalf("默认（不暴露）时 gear_run 应照常可用: %v", plain)
	}

	// 开启后：暴露，且可直接调用（等价于 gear_run，只是少一次参数拼装）。
	fOn := newSmokeFixtureWith(t, func(reg *registry.Registry) { applySmokeBundle(t, reg) }, WithExpose(ExposeFlows))
	tools, err = fOn.session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools 失败: %v", err)
	}
	names := map[string]bool{}
	for _, tt := range tools.Tools {
		names[tt.Name] = true
	}
	if !names["flow__smoke.up"] {
		t.Fatalf("--expose flows 下应暴露 flow__smoke.up: %v", names)
	}
	var exec map[string]any
	callTool(t, fOn.session, "flow__smoke.up", map[string]any{
		"vars": map[string]any{"who": "exposed"}, "wait_seconds": 15,
	}, &exec)
	if exec["status"] != "succeeded" {
		t.Fatalf("flow__ 应成功: %v", exec)
	}

	// service 长驻 Flow 即使开启也不暴露（会挂死调用方）——见 register() 的 hasServiceStep 过滤。
	fSvc := newSmokeFixtureWith(t, func(reg *registry.Registry) {
		applySmokeBundle(t, reg)
		b, diags := parseBundle(svcFlowYAML)
		if len(diags) > 0 {
			t.Fatalf("svc flow 应可解析: %v", diags)
		}
		for _, fl := range b.Flows {
			if err := reg.UpsertFlow(fl); err != nil {
				t.Fatalf("预置 service Flow 失败: %v", err)
			}
		}
	}, WithExpose(ExposeFlows))
	tools, err = fSvc.session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools 失败: %v", err)
	}
	for _, tt := range tools.Tools {
		if tt.Name == "flow__svc_demo" {
			t.Fatal("service 长驻 Flow 不得反向暴露（会常驻不结束，挂死调用方）")
		}
	}
}

// HTTP transport：streamable HTTP 下同一套工具面应可用。
func TestMCPServeHTTP(t *testing.T) {
	f := newSmokeFixture(t)
	callTool(t, f.session, "gear_apply", map[string]any{"bundle": smokeBundleYAML}, nil)

	h, err := f.server.httpHandler()
	if err != nil {
		t.Fatalf("httpHandler 失败: %v", err)
	}
	ts := httptest.NewServer(h)
	defer ts.Close()

	ctx := context.Background()
	client := mcp.NewClient(&mcp.Implementation{Name: "c", Version: "0"}, nil)
	sess, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: ts.URL}, nil)
	if err != nil {
		t.Fatalf("HTTP 客户端连接失败: %v", err)
	}
	defer sess.Close()

	mcpTools, err := sess.ListTools(ctx, &mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("HTTP ListTools 失败: %v", err)
	}
	have := map[string]bool{}
	for _, tt := range mcpTools.Tools {
		have[tt.Name] = true
	}
	if !have["gear_catalog"] || !have["gear_run"] {
		t.Fatalf("HTTP transport 应暴露同一套工具面: %v", have)
	}

	var exec map[string]any
	callTool(t, sess, "gear_run", map[string]any{
		"kind": "flow", "id": "smoke.up", "vars": map[string]any{"who": "http"}, "wait_seconds": 15,
	}, &exec)
	if exec["status"] != "succeeded" {
		t.Fatalf("HTTP gear_run 应成功: %v", exec)
	}
}

// 回归锁：装配顺序契约。New 必须在扩展加载之后调用（否则声明 ext.* 的 Tool
// 会被误判「未知的 capability」）—— 断言契约被显式检查，而非隐式依赖 cliEngine 副作用。
//
// 注意：ext.* 名集合是 cap 包的**进程级全局**，包内其它用例已加载过扩展后本用例
// 无法在同一进程内复现「未装配」态（Go 的 t.Skip 在子测试外仍会标记本函数）。
// 故这里只在确实处于未装配态时才做断言，否则静默通过。
func TestMCPNewRequiresExtensionsLoaded(t *testing.T) {
	if cap.Primitive("ext.ai").Known() {
		// 本进程已加载过扩展，无法复现未装配态；契约本身由 New 内的 extNamesReady 保证。
		return
	}
	t.Setenv("GEAR_EXEC_DB", filepath.Join(t.TempDir(), "exec.db"))
	reg, err := registry.Open(filepath.Join(t.TempDir(), "registry.db"))
	if err != nil {
		t.Fatalf("打开 registry 失败: %v", err)
	}
	defer reg.Close()
	eng := engine.New(reg)
	if _, err := New(reg, eng); err == nil {
		t.Fatal("扩展未加载时 New 应当 fail-closed 报错")
	}
}

// ---- 工具面红线 ----

// mcpToolRegistry 是工具面的**权威登记表**——已提升为生产侧常量 `toolSurface`
// （见 tools_surface.go，评审 O-8）。这里保留一个同义别名，让阅读本文件的用例不用跳文件；
// 语义与守卫完全不变：**任何新工具不登记即 FAIL**，并钉住工具数预算 `toolBudget`。
var mcpToolRegistry = toolSurface

// TestMCPToolSurfaceMatchesRegistry **常驻工具面**必须与登记表完全一致（不多不少）。
//
// 三条红线一次断言：① 新增常驻工具必须登记（否则覆盖度反查失守）；② 常驻工具数 ≤ toolBudget
// 的硬预算；③ 常驻工具统一 gear_ 前缀（跨 MCP server 冲突的根治，§6.2⑦）。
//
// ⚠ 口径（REQ-MCP-EXPOSE-TOOLS）：预算、登记表、前缀这三条只管 **15 个常驻意图级工具**。
// `--expose` 带进来的 flow__ / 策展工具是**按 App 白名单动态生成**的，名字由应用作者给
// （alias 可能不带前缀）、数量由清单决定——把它们纳入 15 预算等于让「开一个可选开关」必然
// 变红。它们另有独立断言（expose_tools_test.go：数量=清单条数、名字合法、schema 映射、
// 输出线格式）。下面按 gear_ 前缀把常驻面单独筛出来断言，不降低常驻面的守卫强度。
func TestMCPToolSurfaceMatchesRegistry(t *testing.T) {
	session := newSmokeSession(t)
	tools, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	got := map[string]bool{}
	for _, tt := range tools.Tools {
		if !strings.HasPrefix(tt.Name, "gear_") {
			continue // 动态暴露工具不属于常驻面
		}
		got[tt.Name] = true
		if _, ok := mcpToolRegistry[tt.Name]; !ok {
			t.Fatalf("工具 %s 未登记进 tools_surface.go 的 toolSurface——新增工具必须登记，"+
				"否则线格式覆盖度红线会静默失守；若有意新增，同时确认工具数预算（≤%d）", tt.Name, toolBudget)
		}
	}
	for name := range mcpToolRegistry {
		if !got[name] {
			t.Fatalf("登记表里的 %s 不在实际工具面中——登记表已过期", name)
		}
	}
	if n := len(got); n > toolBudget {
		t.Fatalf("工具数 %d 超过硬预算 %d（每多一个，模型选错概率就上升一分）", n, toolBudget)
	}
	// 工具名必须落在 SDK 允许的字符集内（`[A-Za-z0-9_.-]`，≤128）。
	//
	// 为什么值得单独断言：SDK 对非法名字**只打日志、仍然注册**（server.go AddTool），
	// 于是一个带空格/中文的 flow id 会做出一个「本地看着正常、严格客户端拒绝」的工具名——
	// 与「裸发数组 structuredContent」是同一类事故（见 callTool 的注释）。
	for _, tt := range tools.Tools {
		if len(tt.Name) > 128 {
			t.Fatalf("工具名 %s 超过 128 字符", tt.Name)
		}
		for _, r := range tt.Name {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			case r == '_', r == '-', r == '.':
			default:
				t.Fatalf("工具名 %s 含非法字符 %q（SDK 只记日志不报错，严格客户端会拒）", tt.Name, string(r))
			}
		}
	}
}

// TestArtifactGearVersionMatchesMCP 版本常量的**漂移锁**。
//
// internal/artifact 不能 import 本包（本包 import 它，反向会成环），于是它自己声明了一份
// `GearVersion` 写进产物元信息。同一个值两份声明正是漂移的起点，故用测试钉死：
// 升版本时忘了改另一处，这里立刻红。
func TestArtifactGearVersionMatchesMCP(t *testing.T) {
	if artifact.GearVersion != Version {
		t.Fatalf("internal/artifact.GearVersion=%q 与 mcp.Version=%q 不一致——"+
			"升版本必须两处同改（产物元信息与 MCP Implementation 报的是同一个 gear）",
			artifact.GearVersion, Version)
	}
}

// TestMCPArgTagsAvoidReservedPrefix 机械守卫：jsonschema 标签**不得以 `WORD=` 开头**。
//
// 踩过的坑：`jsonschema:"kind=tool 时的入参"` 让 AddTool 在**注册期直接 panic**
// ——google/jsonschema-go 用 `^[^ \t\n]*=` 判定「保留写法」（那是给未来关键字留的位）。
// 后果很重：整个 MCP server 起不来，而暴毙点在一个跟「参数文案」毫不相干的调用栈上。
// 所以这里用反射把每个入参类型过一遍：改文案时立刻红，而不是等到起服务时崩。
func TestMCPArgTagsAvoidReservedPrefix(t *testing.T) {
	reserved := regexp.MustCompile("^[^ \t\n]*=")
	types := []reflect.Type{
		reflect.TypeOf(IDArgs{}), reflect.TypeOf(DescribeArgs{}), reflect.TypeOf(CatalogArgs{}),
		reflect.TypeOf(GuideArgs{}), reflect.TypeOf(CapabilitiesArgs{}), reflect.TypeOf(ValidateArgs{}),
		reflect.TypeOf(ApplyArgs{}), reflect.TypeOf(DeleteArgs{}), reflect.TypeOf(PromoteArgs{}),
		reflect.TypeOf(InvokeArgs{}), reflect.TypeOf(RunArgs{}),
		reflect.TypeOf(ExecutionArgs{}), reflect.TypeOf(AppArgs{}), reflect.TypeOf(FlowExposeArgs{}),
		reflect.TypeOf(ExportArgs{}), reflect.TypeOf(ImportArgs{}),
	}
	for _, rt := range types {
		for i := 0; i < rt.NumField(); i++ {
			tag, ok := rt.Field(i).Tag.Lookup("jsonschema")
			if !ok {
				continue
			}
			if tag == "" {
				t.Fatalf("%s.%s 的 jsonschema 标签为空——SDK 会直接报错", rt.Name(), rt.Field(i).Name)
			}
			if reserved.MatchString(tag) {
				t.Fatalf("%s.%s 的 jsonschema 标签以 `WORD=` 开头，AddTool 会在注册期 panic: %q",
					rt.Name(), rt.Field(i).Name, tag)
			}
		}
	}
}

// TestMCPToolDescriptionsStayLean 描述体量红线（§4.1 / §9.1 第 4 条）。
//
// 为什么是红线而不只是约定：背景信息若下沉到每个 Description，tools/list 会**线性膨胀**
// 且同一段话有 N 份副本（改一处要改 N 处，必然漂移）。本用例把它变成机械守卫：
// 单条 ≤240 字符、合计 ≤1,800 字符——超出即 FAIL。
//
// 计数按 **rune**（中文 1 个字算 1 个字符），与文档口径一致。
func TestMCPToolDescriptionsStayLean(t *testing.T) {
	const perToolLimit = 240
	const totalLimit = 1800
	session := newSmokeSession(t)
	tools, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	total := 0
	for _, tt := range tools.Tools {
		// 与 TestMCPToolSurfaceMatchesRegistry 同口径：1,800 字符预算是**常驻齿轮面**
		// 的上下文成本上限；App 策展工具的 description 是产品护栏载体（三段式红线、
		// 禁止自动 confirm），由 model.AppMCP.Validate 单独限长 2,000 字符。
		if !strings.HasPrefix(tt.Name, "gear_") {
			continue
		}
		n := utf8.RuneCountInString(tt.Description)
		total += n
		if n > perToolLimit {
			t.Fatalf("工具 %s 的描述 %d 字符，超过单条上限 %d——"+
				"背景信息应放 instructions 或 gear_guide，不要下沉到描述", tt.Name, n, perToolLimit)
		}
		if !strings.Contains(tt.Description, "。") && !strings.Contains(tt.Description, ".") {
			t.Fatalf("工具 %s 的描述应是一句完整的话（做什么 + 何时用）: %q", tt.Name, tt.Description)
		}
	}
	if total > totalLimit {
		t.Fatalf("工具描述合计 %d 字符，超过上限 %d（换算：常驻上下文随工具数线性膨胀）", total, totalLimit)
	}
}

// 线格式红线全量回归锁（REQ-MCP-WIRE-OBJECT）：**逐个真调**所有列表类工具，
// 断言 structuredContent 顶层是对象、具名键下是数组。
func TestMCPAllToolsReturnObjectStructuredContent(t *testing.T) {
	f := newSmokeFixture(t)
	session := f.session

	// 造出非空数据，避免「空数组恰好也合规」的假绿。
	callTool(t, session, "gear_apply", map[string]any{"bundle": smokeBundleYAML}, nil)
	var run map[string]any
	callTool(t, session, "gear_run", map[string]any{"kind": "flow", "id": "smoke.up", "wait_seconds": 15}, &run)
	execID, _ := run["execution_id"].(string)
	if execID == "" {
		t.Fatalf("gear_run 未返回 execution_id: %v", run)
	}
	// 台账造一条记录，让 gear_app(list) 与 gear_catalog 的 app 分支都返回非空。
	recDir := filepath.Dir(f.regPath) + string(filepath.Separator) + "apps"
	if err := os.MkdirAll(recDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(recDir, "app_fake.json"),
		[]byte(`{"app_id":"app_fake","flow_id":"smoke.up","pid":0,"started_at":"2026-01-01T00:00:00Z"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	// 列表类工具的调用表与期望具名键（与 mcpToolRegistry 的 true 项一一对应）。
	// 2026-09-26：补两个产物工具。export 落到临时目录（须不存在），import 用 dry_run
	// 读回来——同一个路径前后两次用，正好顺带验「导出的产物能被导回」。
	artifactDir := filepath.Join(t.TempDir(), "artifact")
	calls := []struct {
		name string
		args map[string]any
		key  string
	}{
		{"gear_catalog", nil, "items"},
		{"gear_capabilities", nil, "capabilities"},
		{"gear_execution", nil, "executions"},
		{"gear_execution", map[string]any{"id": execID, "with_logs": true}, "logs"},
		{"gear_app", map[string]any{"action": "list"}, "items"},
		{"gear_export", map[string]any{"app_id": "smoke", "out_dir": artifactDir}, "files"},
		{"gear_import", map[string]any{"source": artifactDir, "dry_run": true}, "created"},
	}
	covered := map[string]bool{}
	for _, c := range calls {
		covered[c.name] = true

		var got map[string]any
		callTool(t, session, c.name, c.args, &got) // callTool 内已断言顶层是对象
		v, ok := got[c.key]
		if !ok {
			t.Fatalf("工具 %s 应返回具名键 %q，实际: %v", c.name, c.key, got)
		}
		if _, ok := v.([]any); !ok {
			t.Fatalf("工具 %s 的 %q 应是数组，实际 %T", c.name, c.key, v)
		}
	}

	// 覆盖度反查（registry 驱动）：登记为「列表形态」的工具都必须被上面调到。
	for name, listShaped := range mcpToolRegistry {
		if listShaped && !covered[name] {
			t.Fatalf("列表类工具 %s 未登记进本用例的 calls 表——"+
				"线格式红线（structuredContent 顶层须为对象）未被覆盖", name)
		}
	}
}

// TestMCPErrorsAreStructured 前置条件错误必须**带码**（自愈回路的前提）：
// 智能体靠 code 查 gear_guide("errors") 的修法，而不是猜中文。
//
// 与 mcp_errorpaths_test.go 的分工：那里穷举「每种入参错都报错」，这里只钉**同一个
// 场景下码不能混**——尤其 NOT_FOUND 与 IN_USE 必须分开（id 打错 vs 删不掉，修法完全不同）。
func TestMCPErrorsAreStructured(t *testing.T) {
	f := newSmokeFixture(t)
	session := f.session

	// 造一个「被 Flow 引用」的 Tool：删它必须报 IN_USE 而不是 NOT_FOUND。
	callTool(t, session, "gear_apply", map[string]any{"bundle": smokeBundleYAML}, nil)

	cases := []struct {
		tool string
		args map[string]any
		code string
	}{
		{"gear_describe", map[string]any{"kind": "tool", "id": "no_such_tool"}, "NOT_FOUND"},
		{"gear_execution", map[string]any{"id": "no_such_exec"}, "NOT_FOUND"},
		{"gear_delete", map[string]any{"kind": "tool", "id": "no_such_tool"}, "NOT_FOUND"},
		{"gear_delete", map[string]any{"kind": "tool", "id": "smoke__hello"}, "IN_USE"},
		{"gear_promote", map[string]any{"tool_id": "x", "version": "1.0.0", "target": "bogus"}, "BAD_ARGUMENT"},
		{"gear_run", map[string]any{"kind": "bogus", "id": "x"}, "BAD_ARGUMENT"},
	}
	for _, c := range cases {
		text := expectToolError(t, session, c.tool, c.args, "")
		if !strings.Contains(text, "["+c.code+"]") {
			t.Fatalf("工具 %s 的错误应带 [%s]，实际: %s", c.tool, c.code, text)
		}
	}

	// 错误文本必须给出 hint（可操作的一句话），否则智能体只能重启猜测。
	text := expectToolError(t, session, "gear_delete", map[string]any{"kind": "tool", "id": "smoke__hello"}, "")
	if !strings.Contains(text, "hint:") {
		t.Fatalf("被守卫拒绝的错误应带 hint 说明被谁引用: %s", text)
	}
}

// TestMain 保证测试进程不会把执行库写到仓库/用户默认路径。
//
// 此前这里只是 `os.Exit(m.Run())`，靠各用例自己 t.Setenv——但**没写 t.Setenv 的用例会漏**，
// 而执行记录库的默认落点已从「包内 build/gear.db」改为用户数据目录（见 gearhome 包注释）。
// 这层兜底与 internal/server、internal/cli 的同名函数一致：包级隔离，不依赖用例自觉。
func TestMain(m *testing.M) {
	cleanup := gearhome.IsolateForTest()
	code := m.Run()
	cleanup()
	os.Exit(code)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
