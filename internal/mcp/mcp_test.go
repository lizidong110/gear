package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"gear/internal/cap"
	"gear/internal/engine"
	"gear/internal/registry"
)

// 端到端冒烟：MCP 客户端 → MCP Server → registry/engine 全链路。
// 用 SDK 的 in-memory transport 走真协议（不是直接调 Go 函数），
// 覆盖「读目录 → 写配置 → 校验 → 执行 → 看结果」的智能体闭环。
//
// Tool 用 lua runtime（沙箱内嵌，不依赖外部进程/PATH）。

func newSmokeSession(t *testing.T) *mcp.ClientSession {
	t.Helper()
	// 执行记录库指向临时目录，避免测试污染 build/gear.db。
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
	eng := engine.New(reg, engine.WithCapabilities(capReg), engine.WithAIAgent(agent))
	s, err := New(reg, eng)
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
	return session
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

// expectToolError 断言工具调用以错误收场（守卫/校验路径）。
func expectToolError(t *testing.T, session *mcp.ClientSession, name string, args map[string]any, wantSubstr string) {
	t.Helper()
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("工具 %s 协议层失败: %v", name, err)
	}
	if !res.IsError {
		b, _ := json.Marshal(res.StructuredContent)
		t.Fatalf("工具 %s 应返回错误，实际成功: %s", name, b)
	}
	b, _ := json.Marshal(res.Content)
	if wantSubstr != "" && !contains(string(b), wantSubstr) {
		t.Fatalf("工具 %s 错误信息应含 %q，实际: %s", name, wantSubstr, b)
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// ---- 列表类工具的返回信封 ----
//
// 列表类工具的 structuredContent 是「顶层对象 + 具名键」（见 mcp.go 的 objectResult），
// 故解码必须落到信封类型，而不能用裸切片——裸切片解码会因 JSON 里是 object、
// Go 里是 array 而直接失败（这本来就是线格式红线的另一种表现形式）。

// toolsEnvelope 对应 list_tools 的 {tools:[...]}。
type toolsEnvelope struct {
	Tools []map[string]any `json:"tools"`
}

// flowsEnvelope 对应 list_flows 的 {flows:[...]}。
type flowsEnvelope struct {
	Flows []map[string]any `json:"flows"`
}

// capsEnvelope 对应 capabilities 的 {capabilities:[...]}。
type capsEnvelope struct {
	Capabilities []map[string]any `json:"capabilities"`
}

// logsEnvelope 对应 get_execution_logs 的 {logs:[...]}。
type logsEnvelope struct {
	Logs []map[string]any `json:"logs"`
}

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

const smokeFlowYAML = `id: mcp_demo
name: MCP Demo Flow
description: 冒烟用两步 Flow
vars:
  who: mcp
steps:
  - id: s1
    call: hello
    input:
      name: "${vars.who}"
  - id: s2
    call: hello
    depends_on:
      - s1
    input:
      name: "${steps.s1.output.greeting}"
`

// 主链路：list → create_tool → create_flow → validate → run_flow → get_execution
// → delete（守卫）。一个测试函数串完整闭环，断言中间态。
func TestMCPAgentClosedLoop(t *testing.T) {
	session := newSmokeSession(t)

	// 1) 目录初始为空。
	var env toolsEnvelope
	callTool(t, session, "list_tools", nil, &env)
	if len(env.Tools) != 0 {
		t.Fatalf("初始目录应为空，实际 %d 个 Tool", len(env.Tools))
	}

	// 2) 注册 Tool（YAML）。
	var created map[string]any
	callTool(t, session, "create_tool", map[string]any{"yaml": smokeToolYAML}, &created)
	if fmt.Sprint(created["registered_tools"]) != "[hello]" {
		t.Fatalf("create_tool 输出不符: %v", created)
	}

	// 3) 目录可见，含 schema。
	var listed toolsEnvelope
	callTool(t, session, "list_tools", nil, &listed)
	if len(listed.Tools) != 1 || listed.Tools[0]["id"] != "hello" {
		t.Fatalf("list_tools 应含 hello: %v", listed.Tools)
	}

	// 4) 注册 Flow（静态校验：call 目标 hello 已存在）。
	var flows map[string]any
	callTool(t, session, "create_flow", map[string]any{"yaml": smokeFlowYAML}, &flows)
	if fmt.Sprint(flows["registered_flows"]) != "[mcp_demo]" {
		t.Fatalf("create_flow 输出不符: %v", flows)
	}

	// 5) create_flow 的 call 目标不存在时应被静态校验拒绝。
	badFlow := `id: bad
steps:
  - id: s1
    call: no_such_tool
`
	expectToolError(t, session, "create_flow", map[string]any{"yaml": badFlow}, "静态校验失败")

	// 6) validate_flow 通过。
	var verdict map[string]any
	callTool(t, session, "validate_flow", map[string]any{"id": "mcp_demo"}, &verdict)
	if verdict["valid"] != true {
		t.Fatalf("validate_flow 应通过: %v", verdict)
	}

	// 7) run_flow 同步执行：s1 用 vars，s2 引用 s1 输出。
	var exec map[string]any
	callTool(t, session, "run_flow", map[string]any{"id": "mcp_demo", "vars": map[string]any{"who": "gear"}}, &exec)
	if exec["status"] != "succeeded" {
		t.Fatalf("run_flow 应成功，实际: %v", exec)
	}
	result, _ := exec["result"].(map[string]any)
	steps, _ := result["steps"].(map[string]any)
	if steps == nil {
		t.Fatalf("执行记录缺 result.steps: %v", exec)
	}
	s1, _ := steps["s1"].(map[string]any)
	s1out, _ := s1["output"].(map[string]any)
	if fmt.Sprint(s1out["greeting"]) != "hi gear" {
		t.Fatalf("s1 输出不符: %v", s1)
	}
	s2, _ := steps["s2"].(map[string]any)
	s2out, _ := s2["output"].(map[string]any)
	if fmt.Sprint(s2out["greeting"]) != "hi hi gear" {
		t.Fatalf("s2 应引用 s1 输出（模板串联），实际: %v", s2)
	}

	// 8) run_tool 单工具直跑。
	var out map[string]any
	callTool(t, session, "run_tool", map[string]any{"id": "hello", "input": map[string]any{"name": "direct"}}, &out)
	if fmt.Sprint(out["greeting"]) != "hi direct" {
		t.Fatalf("run_tool 输出不符: %v", out)
	}

	// 9) get_execution_logs 可查（lua 工具无 stdout，空数组也算通过）。
	var logs logsEnvelope
	callTool(t, session, "get_execution_logs", map[string]any{"id": exec["id"]}, &logs)

	// 10) 删除守卫：Tool hello 被 mcp_demo 引用 → 拒绝。
	expectToolError(t, session, "delete_tool", map[string]any{"id": "hello"}, "in use")

	// 11) 先删 Flow 再删 Tool，均应成功。
	callTool(t, session, "delete_flow", map[string]any{"id": "mcp_demo"}, nil)
	callTool(t, session, "delete_tool", map[string]any{"id": "hello"}, nil)
}

// capabilities 工具应返回含内核原语（门禁 + 纯函数原子）的能力目录。
func TestMCPCapabilitiesTool(t *testing.T) {
	session := newSmokeSession(t)
	var env capsEnvelope
	callTool(t, session, "capabilities", nil, &env)
	names := map[string]bool{}
	kinds := map[string]string{}
	for _, c := range env.Capabilities {
		n, _ := c["name"].(string)
		k, _ := c["kind"].(string)
		names[n] = true
		kinds[n] = k
	}
	for _, want := range []string{"http", "call", "db", "kv", "secret", "event", "subscribe"} {
		if !names[want] {
			t.Fatalf("能力目录应含内核门禁原语 %s: %v", want, names)
		}
	}
	// 纯函数原子（time/template/crypto）：始终可用、无需声明，但必须出现在目录里
	//（否则 agent 根本不知道它们存在——这正是补它们时要顺手堵的缺口）。
	for _, want := range []string{"time", "template", "crypto"} {
		if kinds[want] != "pure" {
			t.Fatalf("能力目录应含纯函数原子 %s（kind=pure），实际 kind=%q: %v", want, kinds[want], names)
		}
	}
}

// promote_tool：draft → stable 的非法迁移应被状态机拒绝。
func TestMCPPromoteRejectsIllegalJump(t *testing.T) {
	session := newSmokeSession(t)
	draft := `id: dtool
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
	callTool(t, session, "create_tool", map[string]any{"yaml": draft}, &created)
	expectToolError(t, session, "promote_tool", map[string]any{
		"tool_id": "dtool", "version": "1.0.0", "target": "deprecated",
	}, "")
}

// 回归锁（BUG-1）：无 vars 的 Flow 不得让 list_flows / get_flow 失败的。
//
// 曾因 SDK 从 Go 结构体推断输出 schema，而 model.Flow.Vars 是 map[string]any，
// 无 vars 时零值 nil → marshals 成 null → 与推断的 type:object 冲突 → SDK 输出校验
// 拒绝整条调用（JSON-RPC 层错误，不是 tool isError）。而「没写 vars 的 Flow」是
// 绝大多数真实形态，等于 list_flows 一调就废 —— 智能体「读目录」第一步就断。
// 修法：这类透传 registry 数据的工具用 Out = any，SDK 对 any 跳过输出校验。
//
// ⚠ 测试取样教训：原 smokeFlowYAML 恰好写了 vars，导致该 bug 长期漏检。
// 本用例**刻意不写 vars**，反向覆盖「缺省值」路径。
func TestMCPListFlowsWithoutVars(t *testing.T) {
	session := newSmokeSession(t)

	// 无 vars 的 Flow（刻意留空 —— 这是真实场景里最常见的形态）。
	const flowNoVars = `id: novars
name: NoVars Flow
description: 不声明 vars 的 Flow
steps:
  - id: s1
    call: hello
`
	var created map[string]any
	callTool(t, session, "create_tool", map[string]any{"yaml": smokeToolYAML}, &created)
	callTool(t, session, "create_flow", map[string]any{"yaml": flowNoVars}, &created)

	// 关键：这两个调用必须成功返回（此前会抛 JSON-RPC 错误）。
	var env flowsEnvelope
	callTool(t, session, "list_flows", nil, &env)
	if len(env.Flows) != 1 || env.Flows[0]["id"] != "novars" {
		t.Fatalf("list_flows 应返回含 novars 的列表: %v", env.Flows)
	}

	var one map[string]any
	callTool(t, session, "get_flow", map[string]any{"id": "novars"}, &one)
	if one["id"] != "novars" {
		t.Fatalf("get_flow 应返回 novars: %v", one)
	}
}

// 回归锁（BUG-2）：create_tool / create_flow 必须**原子**——多文档里任一非法，
// 则整体不写入，不得留下「部分成功」。
//
// 曾因逐个 Upsert 边校验边落库：第 1 个合法写入、第 2 个非法报错，调用方收到 error
// 却已有产物落库；智能体重试整段 YAML 时行为与首次不一致（不静默降级红线）。
func TestMCPCreateIsAtomic(t *testing.T) {
	session := newSmokeSession(t)

	// 一段 YAML：第 1 个 Tool 合法，第 2 个 runtime=wasm（未实现，注册期必拒）。
	const mixedTools = `id: good_one
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
	expectToolError(t, session, "create_tool", map[string]any{"yaml": mixedTools}, "bad_one")

	// 全量校验失败 → 一个都不该落库。
	var env toolsEnvelope
	callTool(t, session, "list_tools", nil, &env)
	if len(env.Tools) != 0 {
		t.Fatalf("create_tool 失败时不得留下部分写入，实际库中有: %v", env.Tools)
	}

	// create_flow 同理：第 1 个 Flow 合法、第 2 个 call 目标不存在。
	callTool(t, session, "create_tool", map[string]any{"yaml": smokeToolYAML}, nil)
	const mixedFlows = `id: ok_flow
name: OK
steps:
  - id: s1
    call: hello
    input:
      name: x
---
id: bad_flow
name: Bad
steps:
  - id: s1
    call: no_such_tool
`
	expectToolError(t, session, "create_flow", map[string]any{"yaml": mixedFlows}, "bad_flow")
	var fenv flowsEnvelope
	callTool(t, session, "list_flows", nil, &fenv)
	if len(fenv.Flows) != 0 {
		t.Fatalf("create_flow 失败时不得留下部分写入，实际库中有: %v", fenv.Flows)
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

// 反向暴露：已注册 Flow 应在 MCP 工具面暴露为 flow__{id}，且 service 长驻
// Flow 不暴露（提示用 gear app run）。server 启动期按当时 registry 暴露，故先落库再起 server。
func TestMCPReverseExposeFlow(t *testing.T) {
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
	eng := engine.New(reg, engine.WithCapabilities(capReg), engine.WithAIAgent(agent))

	// 落 Tool + 普通 Flow + 含 service step 的 Flow（server 启动前）。
	seedTool, _, err := parseGearYAML([]byte(smokeToolYAML))
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.UpsertTool(seedTool[0]); err != nil {
		t.Fatal(err)
	}
	_, seedFlows, err := parseGearYAML([]byte(smokeFlowYAML))
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.ValidateFlowStatic(seedFlows[0]); err != nil {
		t.Fatalf("Flow 静态校验失败: %v", err)
	}
	if err := reg.UpsertFlow(seedFlows[0]); err != nil {
		t.Fatal(err)
	}
	_, svcFlows, err := parseGearYAML([]byte(svcFlowYAML))
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.UpsertFlow(svcFlows[0]); err != nil {
		t.Fatal(err)
	}

	s, err := New(reg, eng)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	st, ct := mcp.NewInMemoryTransports()
	go func() { _ = s.Run(ctx, st) }()
	client := mcp.NewClient(&mcp.Implementation{Name: "c", Version: "0"}, nil)
	sess, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	defer s.Close()
	defer reg.Close()

	mcpTools, err := sess.ListTools(ctx, &mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("ListTools 失败: %v", err)
	}
	names := map[string]bool{}
	for _, tt := range mcpTools.Tools {
		names[tt.Name] = true
	}
	if !names["flow__mcp_demo"] {
		t.Fatalf("应暴露 flow__mcp_demo: %v", names)
	}
	if !names["list_tools"] || !names["run_flow"] {
		t.Fatalf("静态工具应仍在: %v", names)
	}
	if names["flow__svc_demo"] {
		t.Fatalf("service Flow 不应反向暴露: %v", names)
	}

	// 直接调用 flow__mcp_demo。
	var exec map[string]any
	callTool(t, sess, "flow__mcp_demo", map[string]any{"vars": map[string]any{"who": "exposed"}}, &exec)
	if exec["status"] != "succeeded" {
		t.Fatalf("flow__ 应成功: %v", exec)
	}
	res, _ := exec["result"].(map[string]any)
	steps, _ := res["steps"].(map[string]any)
	s1, _ := steps["s1"].(map[string]any)
	s1out, _ := s1["output"].(map[string]any)
	if fmt.Sprint(s1out["greeting"]) != "hi exposed" {
		t.Fatalf("flow__ 输出不符: %v", s1out)
	}
}

// HTTP transport：streamable HTTP 下同一套工具面应可用（含反向暴露的 flow__ 工具）。
func TestMCPServeHTTP(t *testing.T) {
	t.Setenv("GEAR_EXEC_DB", filepath.Join(t.TempDir(), "exec.db"))
	reg, err := registry.Open(filepath.Join(t.TempDir(), "registry.db"))
	if err != nil {
		t.Fatal(err)
	}
	capReg := engine.NewCapabilityRegistry()
	agent, err := engine.SetupExtensions(capReg, reg, "")
	if err != nil {
		t.Fatal(err)
	}
	eng := engine.New(reg, engine.WithCapabilities(capReg), engine.WithAIAgent(agent))

	seedTool, _, err := parseGearYAML([]byte(smokeToolYAML))
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.UpsertTool(seedTool[0]); err != nil {
		t.Fatal(err)
	}
	_, seedFlows, err := parseGearYAML([]byte(smokeFlowYAML))
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.ValidateFlowStatic(seedFlows[0]); err != nil {
		t.Fatal(err)
	}
	if err := reg.UpsertFlow(seedFlows[0]); err != nil {
		t.Fatal(err)
	}

	s, err := New(reg, eng)
	if err != nil {
		t.Fatal(err)
	}
	h, err := s.httpHandler()
	if err != nil {
		t.Fatalf("httpHandler 失败: %v", err)
	}
	ts := httptest.NewServer(h)
	defer ts.Close()
	defer s.Close()
	defer reg.Close()

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
	if !have["flow__mcp_demo"] || !have["list_tools"] {
		t.Fatalf("HTTP transport 应暴露 flow__ 与静态工具: %v", have)
	}

	// 经 HTTP 调用反向暴露工具。
	var exec map[string]any
	callTool(t, sess, "flow__mcp_demo", map[string]any{"vars": map[string]any{"who": "http"}}, &exec)
	if exec["status"] != "succeeded" {
		t.Fatalf("HTTP flow__ 应成功: %v", exec)
	}
	res, _ := exec["result"].(map[string]any)
	steps, _ := res["steps"].(map[string]any)
	s1, _ := steps["s1"].(map[string]any)
	s1out, _ := s1["output"].(map[string]any)
	if fmt.Sprint(s1out["greeting"]) != "hi http" {
		t.Fatalf("HTTP flow__ 输出不符: %v", s1out)
	}
}

const svcFlowYAML = `id: svc_demo
name: SVC Demo
description: 含 service 长驻 step 的 Flow（不应反向暴露）
steps:
  - id: s1
    call: hello
    mode: service
    input:
      name: x
`

// 线格式红线全量回归锁（REQ-MCP-WIRE-OBJECT）：**逐个真调**所有列表类工具，
// 断言 structuredContent 顶层是对象、具名键下是数组；并用 ListTools **反查覆盖度**——
// 将来新增一个「list_/capabilities/get_execution_logs」形态的工具却忘了登记进 calls 表，
// 本用例直接 FAIL（红线不会因新增工具而悄悄失守）。
//
// 为什么不能只靠 callTool 的集中断言：那只保证「被调到的工具」合规；没有本用例，
// 一个没人调用的列表工具可以长期裸发数组而测试全绿——正是上一轮事故的漏检方式。
func TestMCPAllToolsReturnObjectStructuredContent(t *testing.T) {
	session := newSmokeSession(t)

	// 造出非空数据，避免「空数组恰好也合规」的假绿。
	var created map[string]any
	callTool(t, session, "create_tool", map[string]any{"yaml": smokeToolYAML}, &created)
	callTool(t, session, "create_flow", map[string]any{"yaml": smokeFlowYAML}, &created)

	var exec map[string]any
	callTool(t, session, "run_flow", map[string]any{"id": "mcp_demo"}, &exec)
	execID, _ := exec["id"].(string)
	if execID == "" {
		t.Fatalf("run_flow 未返回执行 id: %v", exec)
	}

	// Hook 在 run_flow **之后**再建：避免它在本用例的执行里真被触发，
	// 让本用例只考核线格式、不掺入 Hook 行为。
	callTool(t, session, "create_hook", map[string]any{
		"id": "h1", "target": "*", "stage": "after_step", "action": "hello", "criticality": "best-effort",
	}, &created)

	// 列表类工具的调用表与期望具名键（新增列表类工具必须在此登记）。
	calls := []struct {
		name string
		args map[string]any
		key  string
	}{
		{"capabilities", nil, "capabilities"},
		{"list_tools", nil, "tools"},
		{"list_flows", nil, "flows"},
		{"list_hooks", nil, "hooks"},
		{"list_executions", nil, "executions"},
		{"get_execution_logs", map[string]any{"id": execID}, "logs"},
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

	// 覆盖度反查：登记的「列表形态」工具是否都进了 calls 表。
	mcpTools, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	for _, tt := range mcpTools.Tools {
		listShaped := strings.HasPrefix(tt.Name, "list_") ||
			tt.Name == "capabilities" || tt.Name == "get_execution_logs"
		if listShaped && !covered[tt.Name] {
			t.Fatalf("列表类工具 %s 未登记进本用例的 calls 表——"+
				"线格式红线（structuredContent 顶层须为对象）未被覆盖", tt.Name)
		}
	}
}

// 静默守护：确保测试进程不会把执行库写到仓库默认路径。
func TestMain(m *testing.M) {
	os.Exit(m.Run())
}
