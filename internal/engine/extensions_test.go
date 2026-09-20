package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"gear/internal/cap"
	"gear/internal/model"
	lua "github.com/yuin/gopher-lua"
)

// EchoClient 是 ext.echo 的原生实现：演示 Provider 经 host 取实现、Lua 经 ctx 调用。
type EchoClient struct{}

// Say 把输入加前缀返回。
func (e *EchoClient) Say(msg string) string { return "echo:" + msg }

// TestExtNativeLuaCall 端到端：注册 ext.echo（native），真实引擎跑 Flow，lua Tool 调 ctx.ext.echo.say。
func TestExtNativeLuaCall(t *testing.T) {
	reg := NewCapabilityRegistry()
	if err := reg.Register("ext.echo", &CapabilityExtension{
		Provider: func(c *cap.Context) (interface{}, error) { return &EchoClient{}, nil },
		BuildLua: func(L *lua.LState, c *cap.Context) (lua.LValue, error) {
			tbl := L.NewTable()
			tbl.RawSetString("say", L.NewFunction(func(L *lua.LState) int {
				p, err := c.Extension("ext.echo")
				if err != nil {
					L.RaiseError("%v", err)
					return 0
				}
				msg := L.OptString(1, "")
				L.Push(lua.LString(p.(*EchoClient).Say(msg)))
				return 1
			}))
			return tbl, nil
		},
	}); err != nil {
		t.Fatalf("注册 ext.echo 失败: %v", err)
	}

	engReg := newTestRegistry(t)
	defer engReg.Close()
	tool := &model.Tool{
		ID:           "echotool",
		Name:         "echotool",
		Runtime:      model.RuntimeLua,
		Entry:        `return { out = ctx.ext.echo.say("hi") }`,
		Capabilities: []string{"ext.echo"},
		InputSchema:  map[string]interface{}{"type": "object"},
		OutputSchema: map[string]interface{}{"type": "object"},
		Versions:     []model.ToolVersion{{Version: "1.0.0", State: model.StateStable}},
	}
	if err := engReg.UpsertTool(tool); err != nil {
		t.Fatalf("注册 Tool 失败: %v", err)
	}
	flow := &model.Flow{ID: "echoflow", Steps: []model.Step{{ID: "s1", Call: "echotool"}}}
	if err := engReg.UpsertFlow(flow); err != nil {
		t.Fatalf("注册 Flow 失败: %v", err)
	}

	eng := New(engReg, WithCapabilities(reg))
	res, err := eng.RunFlowResult("echoflow")
	if err != nil {
		t.Fatalf("RunFlowResult 失败: %v", err)
	}
	stepOut, ok := res.Steps["s1"]["output"].(map[string]interface{})
	if !ok {
		t.Fatalf("step 输出异常: %v", res.Steps["s1"])
	}
	if fmt.Sprint(stepOut["out"]) != "echo:hi" {
		t.Fatalf("ext.echo 注入/调用异常，期望 echo:hi，实际 %v", stepOut["out"])
	}
}

// TestExtNotDeclaredDenied 未声明扩展名时，脚本访问 ctx.ext.xxx 应报错（最小权限，字段不存在）。
func TestExtNotDeclaredDenied(t *testing.T) {
	reg := NewCapabilityRegistry()
	if err := reg.Register("ext.echo", &CapabilityExtension{
		Provider: func(c *cap.Context) (interface{}, error) { return &EchoClient{}, nil },
		BuildLua: func(L *lua.LState, c *cap.Context) (lua.LValue, error) {
			tbl := L.NewTable()
			tbl.RawSetString("say", L.NewFunction(func(L *lua.LState) int { return 0 }))
			return tbl, nil
		},
	}); err != nil {
		t.Fatalf("注册 ext.echo 失败: %v", err)
	}

	engReg := newTestRegistry(t)
	defer engReg.Close()
	tool := &model.Tool{
		ID:           "echotool2",
		Name:         "echotool2",
		Runtime:      model.RuntimeLua,
		Entry:        `return { out = ctx.ext.echo.say("hi") }`, // 访问未声明的扩展
		Capabilities: []string{},                                // 不声明 ext.echo
		InputSchema:  map[string]interface{}{"type": "object"},
		OutputSchema: map[string]interface{}{"type": "object"},
		Versions:     []model.ToolVersion{{Version: "1.0.0", State: model.StateStable}},
	}
	if err := engReg.UpsertTool(tool); err != nil {
		t.Fatalf("注册 Tool 失败: %v", err)
	}
	flow := &model.Flow{ID: "echoflow2", Steps: []model.Step{{ID: "s1", Call: "echotool2"}}}
	if err := engReg.UpsertFlow(flow); err != nil {
		t.Fatalf("注册 Flow 失败: %v", err)
	}

	eng := New(engReg, WithCapabilities(reg))
	if _, err := eng.RunFlowResult("echoflow2"); err == nil {
		t.Fatal("未声明 ext.echo 时访问 ctx.ext.echo 应报错")
	}
}

// TestExtUnknownNameRejected 声明从未注册的 ext 名时，注册期 GrantsFromStrings 应报错（红线15 仍生效）。
func TestExtUnknownNameRejected(t *testing.T) {
	if _, err := cap.GrantsFromStrings([]string{"ext.ghost"}); err == nil {
		t.Fatal("声明未注册的 ext.ghost 应被 GrantsFromStrings 拒绝")
	}
}

// fakeTransport 是 SidecarTransport 的测试桩，验证转发协议（不经真实子进程）。
type fakeTransport struct{ lastMethod string }

func (f *fakeTransport) Call(method string, args map[string]interface{}) (interface{}, error) {
	f.lastMethod = method
	return map[string]interface{}{"method": method}, nil
}

// TestExtSidecarForward 验证 sidecar 载体的 BuildLua 注入 + 经 Extension 取 Provider 转发整链路。
func TestExtSidecarForward(t *testing.T) {
	fake := &fakeTransport{}
	prov := &SidecarProvider{t: fake}
	reg := NewCapabilityRegistry()
	if err := reg.Register("ext.side", &CapabilityExtension{
		Provider: func(c *cap.Context) (interface{}, error) { return prov, nil },
		BuildLua: func(L *lua.LState, c *cap.Context) (lua.LValue, error) {
			tbl := L.NewTable()
			tbl.RawSetString("ping", L.NewFunction(func(L *lua.LState) int {
				p, err := c.Extension("ext.side")
				if err != nil {
					L.RaiseError("%v", err)
					return 0
				}
				args := map[string]interface{}{}
				if m, ok := fromLuaValue(L.Get(1)).(map[string]interface{}); ok {
					args = m
				}
				res, err := p.(*SidecarProvider).Call("ping", args)
				if err != nil {
					L.RaiseError("%v", err)
					return 0
				}
				L.Push(toLuaValue(L, res))
				return 1
			}))
			return tbl, nil
		},
	}); err != nil {
		t.Fatalf("注册 ext.side 失败: %v", err)
	}

	engReg := newTestRegistry(t)
	defer engReg.Close()
	tool := &model.Tool{
		ID:           "sidetool",
		Name:         "sidetool",
		Runtime:      model.RuntimeLua,
		Entry:        `local r = ctx.ext.side.ping({x=1}); return { m = r.method }`,
		Capabilities: []string{"ext.side"},
		InputSchema:  map[string]interface{}{"type": "object"},
		OutputSchema: map[string]interface{}{"type": "object"},
		Versions:     []model.ToolVersion{{Version: "1.0.0", State: model.StateStable}},
	}
	if err := engReg.UpsertTool(tool); err != nil {
		t.Fatalf("注册 Tool 失败: %v", err)
	}
	engReg.UpsertFlow(&model.Flow{ID: "sideflow", Steps: []model.Step{{ID: "s1", Call: "sidetool"}}})

	eng := New(engReg, WithCapabilities(reg))
	res, err := eng.RunFlowResult("sideflow")
	if err != nil {
		t.Fatalf("sidecar 转发失败: %v", err)
	}
	stepOut := res.Steps["s1"]["output"].(map[string]interface{})
	if fmt.Sprint(stepOut["m"]) != "ping" {
		t.Fatalf("sidecar 转发结果异常: %v", stepOut)
	}
	if fake.lastMethod != "ping" {
		t.Fatalf("sidecar 未转发到 ping 方法，实际 %q", fake.lastMethod)
	}
}

// TestExtAIReachable 端到端：注册 ext.ai（sidecar 载体，fake transport），真实引擎跑 Flow，
// Lua 调 ctx.ext.ai.complete（稳定原语之一）应正确转发到 complete 方法。
func TestExtAIReachable(t *testing.T) {
	fake := &fakeTransport{}
	reg := NewCapabilityRegistry()
	if err := reg.Register("ext.ai", sidecarExtension("ext.ai", fake, AIMethods)); err != nil {
		t.Fatalf("注册 ext.ai 失败: %v", err)
	}

	engReg := newTestRegistry(t)
	defer engReg.Close()
	tool := &model.Tool{
		ID:           "aitool",
		Name:         "aitool",
		Runtime:      model.RuntimeLua,
		Entry:        `local r = ctx.ext.ai.complete({ system = "x", prompt = "y" }); return { m = "complete" }`,
		Capabilities: []string{"ext.ai"},
		InputSchema:  map[string]interface{}{"type": "object"},
		OutputSchema: map[string]interface{}{"type": "object"},
		Versions:     []model.ToolVersion{{Version: "1.0.0", State: model.StateStable}},
	}
	if err := engReg.UpsertTool(tool); err != nil {
		t.Fatalf("注册 Tool 失败: %v", err)
	}
	if err := engReg.UpsertFlow(&model.Flow{ID: "aiflow", Steps: []model.Step{{ID: "s1", Call: "aitool"}}}); err != nil {
		t.Fatalf("注册 Flow 失败: %v", err)
	}

	eng := New(engReg, WithCapabilities(reg))
	res, err := eng.RunFlowResult("aiflow")
	if err != nil {
		t.Fatalf("ext.ai 调用失败: %v", err)
	}
	stepOut, ok := res.Steps["s1"]["output"].(map[string]interface{})
	if !ok {
		t.Fatalf("step 输出异常: %v", res.Steps["s1"])
	}
	if fmt.Sprint(stepOut["m"]) != "complete" {
		t.Fatalf("ext.ai 方法未正确转发: %v", stepOut)
	}
	if fake.lastMethod != "complete" {
		t.Fatalf("ext.ai 未转发到 complete，实际 %q", fake.lastMethod)
	}
}

// TestExtAINotDeclaredDenied 未声明 ext.ai 时，脚本访问 ctx.ext.ai 应报错（最小权限门禁）。
func TestExtAINotDeclaredDenied(t *testing.T) {
	fake := &fakeTransport{}
	reg := NewCapabilityRegistry()
	if err := reg.Register("ext.ai", sidecarExtension("ext.ai", fake, AIMethods)); err != nil {
		t.Fatalf("注册 ext.ai 失败: %v", err)
	}

	engReg := newTestRegistry(t)
	defer engReg.Close()
	tool := &model.Tool{
		ID:           "aitool2",
		Name:         "aitool2",
		Runtime:      model.RuntimeLua,
		Entry:        `return { m = ctx.ext.ai.complete({}) }`, // 访问未声明的扩展
		Capabilities: []string{},                               // 不声明 ext.ai
		InputSchema:  map[string]interface{}{"type": "object"},
		OutputSchema: map[string]interface{}{"type": "object"},
		Versions:     []model.ToolVersion{{Version: "1.0.0", State: model.StateStable}},
	}
	if err := engReg.UpsertTool(tool); err != nil {
		t.Fatalf("注册 Tool 失败: %v", err)
	}
	if err := engReg.UpsertFlow(&model.Flow{ID: "aiflow2", Steps: []model.Step{{ID: "s1", Call: "aitool2"}}}); err != nil {
		t.Fatalf("注册 Flow 失败: %v", err)
	}

	eng := New(engReg, WithCapabilities(reg))
	if _, err := eng.RunFlowResult("aiflow2"); err == nil {
		t.Fatal("未声明 ext.ai 时访问 ctx.ext.ai 应报错")
	}
}

// TestSetupExtensionsRequiresSchemas 严格校验：extensions.yaml 中 ext.* 缺 schemas 时 SetupExtensions 应报错
// （用户决策 2026-09-15：所有 ext.* 必须声明方法级参数 schema，供 agent capabilities() 锚定）。
func TestSetupExtensionsRequiresSchemas(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "extensions.yaml")
	// ext.tls 声明 methods 但缺 schemas → 应报错
	if err := os.WriteFile(p, []byte("ext.tls:\n  cmd: [\"./tls-sidecar\"]\n  methods: [issue]\n"), 0644); err != nil {
		t.Fatal(err)
	}
	capReg := NewCapabilityRegistry()
	engReg := newTestRegistry(t)
	defer engReg.Close()
	if _, err := SetupExtensions(capReg, engReg, p); err == nil {
		t.Fatal("ext.* 缺 schemas 应被 SetupExtensions 拒绝")
	}
}

// TestSetupExtensionsSchemasMismatch methods 与 schemas 不一致也应报错（methods 含 schemas 没有的方法）。
func TestSetupExtensionsSchemasMismatch(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "extensions.yaml")
	yaml := "ext.tls:\n  cmd: [\"./tls-sidecar\"]\n  methods: [issue, revoke]\n  schemas:\n    issue:\n      description: 签发\n      parameters:\n        domain: {type: string, required: true}\n"
	if err := os.WriteFile(p, []byte(yaml), 0644); err != nil {
		t.Fatal(err)
	}
	capReg := NewCapabilityRegistry()
	engReg := newTestRegistry(t)
	defer engReg.Close()
	if _, err := SetupExtensions(capReg, engReg, p); err == nil {
		t.Fatal("methods 与 schemas 不一致应被拒绝")
	}
}

// TestSetupExtensionsWithSchemasOK 带合法 schemas 时应成功注册，且 capabilities() 含该 ext。
func TestSetupExtensionsWithSchemasOK(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "extensions.yaml")
	yaml := "ext.tls:\n  cmd: [\"./tls-sidecar\"]\n  methods: [issue]\n  schemas:\n    issue:\n      description: 签发证书\n      parameters:\n        domain: {type: string, description: 域名, required: true}\n"
	if err := os.WriteFile(p, []byte(yaml), 0644); err != nil {
		t.Fatal(err)
	}
	capReg := NewCapabilityRegistry()
	engReg := newTestRegistry(t)
	defer engReg.Close()
	if _, err := SetupExtensions(capReg, engReg, p); err != nil {
		t.Fatalf("合法 schemas 应注册成功，实际 %v", err)
	}
	if _, ok := capReg.Lookup("ext.tls"); !ok {
		t.Fatal("ext.tls 应已注册")
	}
}

// TestRegistryCapabilities 验证 ext.registry.capabilities() 返回内核原语（kind=builtin 的门禁原语
// + kind=pure 的纯函数原子）+ 已注册 ext.* 的方法级参数 schema，打通「capability 级参数语义」缺口（选项2）。
func TestRegistryCapabilities(t *testing.T) {
	engReg := newTestRegistry(t)
	defer engReg.Close()
	extCaps := map[string]map[string]MethodSchema{
		"ext.db": {
			"query": {Description: "参数化查询", Parameters: map[string]ParamSchema{
				"sql":  {Type: "string", Required: true},
				"args": {Type: "array"},
			}},
		},
	}
	ext := RegistryExtension(engReg, extCaps)
	prov, err := ext.Provider(nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := prov.(*inProcessProvider).Call("capabilities", nil)
	if err != nil {
		t.Fatal(err)
	}
	list, ok := res.([]map[string]interface{})
	if !ok {
		t.Fatalf("capabilities 返回类型异常: %T", res)
	}
	// 内核原语（allBuiltinPrimitives：门禁 7 + 纯函数 3）+ 1 个 ext = 11 条
	want := len(allBuiltinPrimitives()) + 1
	if len(list) != want {
		t.Fatalf("期望 %d 条能力摘要，实际 %d: %v", want, len(list), list)
	}
	byName := map[string]map[string]interface{}{}
	for _, s := range list {
		byName[s["name"].(string)] = s
	}
	// 门禁原语 subscribe 必须出现（此前手写的 6 元素列表把它漏在目录外）
	if byName["subscribe"]["kind"] != "builtin" {
		t.Fatalf("缺少内置 subscribe 能力: %v", byName)
	}
	// 内置 http 必须含 Request 方法及其参数
	httpSum, ok := byName["http"]
	if !ok || httpSum["kind"] != "builtin" {
		t.Fatalf("缺少内置 http 能力: %v", byName)
	}
	httpMethods, ok := httpSum["methods"].(map[string]MethodSchema)
	if !ok {
		t.Fatalf("http methods 类型异常: %T", httpSum["methods"])
	}
	req, ok := httpMethods["Request"]
	if !ok || req.Parameters["url"].Type != "string" {
		t.Fatalf("http.Request 参数 schema 异常: %v", httpMethods)
	}
	// 纯函数原子必须被标成 kind=pure（与门禁原语区分：它们无需声明 capabilities）
	for _, p := range purePrimitiveOrder {
		sum, ok := byName[p]
		if !ok || sum["kind"] != "pure" {
			t.Fatalf("纯函数原子 %s 应出现在目录里且 kind=pure: %v", p, byName)
		}
	}
	// ext.db 必须含 query 方法且 sql 必填
	dbSum := byName["ext.db"]
	if dbSum["kind"] != "ext" {
		t.Fatalf("ext.db kind 应为 ext: %v", dbSum)
	}
	dbMethods := dbSum["methods"].(map[string]MethodSchema)
	if q, ok := dbMethods["query"]; !ok || !q.Parameters["sql"].Required {
		t.Fatalf("ext.db.query.sql 应必填: %v", dbMethods)
	}
}
