package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
				if m, ok := fromLuaValue(L, L.Get(1)).(map[string]interface{}); ok {
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

// TestRegistryToolsPaginates 目录类观察的可选分页参数（2026-09-27）。
//
// 背景：ext.registry.tools() 全量吐出会把观察顶过截断线，模型只数得到看得见的那部分就下
// 结论（实测答 12，真实 34）。prefix/limit 是「让它能分次看」的唯一手段——光在提示里写
// 「请缩小范围」而没有参数可传，等于一句空话。
func TestRegistryToolsPaginates(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	call := func(args map[string]interface{}) []map[string]interface{} {
		t.Helper()
		ext := RegistryExtension(reg, nil)
		prov, err := ext.Provider(nil)
		if err != nil {
			t.Fatal(err)
		}
		res, err := prov.(*inProcessProvider).Call("tools", args)
		if err != nil {
			t.Fatalf("调用 ext.registry.tools(%v) 失败: %v", args, err)
		}
		arr, ok := res.([]map[string]interface{})
		if !ok {
			t.Fatalf("返回值必须是数组（examples/ai-create-flow.yaml 用 ipairs 消费），实际 %T", res)
		}
		return arr
	}
	for _, id := range []string{"alpha__a", "alpha__b", "beta__c"} {
		if err := reg.UpsertTool(&model.Tool{ID: id, Runtime: model.RuntimeLua, Entry: "return {}",
			InputSchema:  map[string]interface{}{"type": "object"},
			OutputSchema: map[string]interface{}{"type": "object"},
			Versions:     []model.ToolVersion{{Version: "1.0.0", State: model.StateStable}}}); err != nil {
			t.Fatal(err)
		}
	}

	if got := call(nil); len(got) != 3 {
		t.Fatalf("不传参数应返回全部 3 条，实际 %d", len(got))
	}
	if got := call(map[string]interface{}{"prefix": "alpha__"}); len(got) != 2 {
		t.Fatalf("prefix 过滤应得 2 条，实际 %d", len(got))
	}
	if got := call(map[string]interface{}{"prefix": "zzz__"}); len(got) != 0 {
		t.Fatalf("无匹配的 prefix 应得 0 条，实际 %d", len(got))
	}
	// limit 小于条数：截断要发生在「条数」上，而不是让观察被 rune 截断后模型数不准
	if got := call(map[string]interface{}{"limit": float64(2)}); len(got) != 2 {
		t.Fatalf("limit=2 应得 2 条，实际 %d", len(got))
	}
	// limit 大于条数不应报错，也不会在列表里造出行
	if got := call(map[string]interface{}{"limit": float64(99)}); len(got) != 3 {
		t.Fatalf("limit 大于总数应得全部 3 条，实际 %d", len(got))
	}
	// 负向：limit=0 / 负数 = 「没传」（默认上限），不能当成「要 0 条」
	if got := call(map[string]interface{}{"limit": float64(0)}); len(got) != 3 {
		t.Fatalf("limit=0 应回落到默认（3 条），实际 %d", len(got))
	}
	if got := call(map[string]interface{}{"limit": float64(-5)}); len(got) != 3 {
		t.Fatalf("limit 负数应回落到默认（3 条），实际 %d", len(got))
	}
}

// TestRegistrySchemasDeclareListParams 参数 schema 必须与实际接受的参数一致。
// 少声明了，模型就不知道有 prefix/limit 可用，分页形同虚设。
func TestRegistrySchemasDeclareListParams(t *testing.T) {
	sch := registrySchemas()
	for _, m := range []string{"tools", "flows"} {
		params := sch[m].Parameters
		if params["prefix"].Type != "string" || params["limit"].Type != "number" {
			t.Fatalf("ext.registry.%s 应声明 prefix(string) 与 limit(number)，实际 %v", m, params)
		}
	}
	if len(sch["capabilities"].Parameters) != 0 {
		t.Fatalf("capabilities 无参数，不该被塞参数 schema: %v", sch["capabilities"].Parameters)
	}
}

// ───────── service ext：协调地址的唯一真相源（extensions.yaml 的 listen） ─────────

// serviceExtYAML 生成一段最小可用的 service ext 配置（schemas 必填是红线，不能省）。
// probeAddr 为空 = 不写 health_probe.addr，用于验证「省略即取 listen」。
func serviceExtYAML(name, listen, probeAddr string) string {
	y := name + ":\n" +
		"  cmd: [\"echo\"]\n" +
		"  methods: [ping]\n" +
		"  service: true\n" +
		"  listen: \"" + listen + "\"\n" +
		"  health_probe:\n    kind: tcp\n"
	if probeAddr != "" {
		y += "    addr: \"" + probeAddr + "\"\n"
	}
	return y + "  schemas:\n    ping:\n      parameters: {}\n"
}

// setupServiceExt 装配一个 service ext，返回它被包成的虚拟 process Tool（装配失败返回 err）。
func setupServiceExt(t *testing.T, name, listen, probeAddr string) (*model.Tool, error) {
	t.Helper()
	capReg := NewCapabilityRegistry()
	reg := newTestRegistry(t)
	t.Cleanup(func() { reg.Close() })
	if _, err := SetupExtensions(capReg, reg, writeExtConfig(t, serviceExtYAML(name, listen, probeAddr))); err != nil {
		return nil, err
	}
	tool, err := reg.GetTool(name)
	if err != nil {
		t.Fatalf("service ext 应包成虚拟 process Tool: %v", err)
	}
	return tool, nil
}

// TestServiceExtEntryInjectsCoordAddr：引擎拉起 service ext 时必须把 extensions.yaml 的
// listen 以 COORD_ADDR 注入侧车进程环境——这是「listen 是协调地址唯一真相源」的落点。
// 没有它，yaml 的 listen 与侧车 os.Getenv("COORD_ADDR") 的默认值就是两份独立定义：
// 改任意一处都会变成「引擎连 A、侧车听 B」，进程都活着却永远连不上（静默失联）。
func TestServiceExtEntryInjectsCoordAddr(t *testing.T) {
	tool, err := setupServiceExt(t, "ext.svc", "127.0.0.1:8090", "127.0.0.1:8090")
	if err != nil {
		t.Fatalf("合法 service ext 应装配成功: %v", err)
	}
	want := "COORD_ADDR='127.0.0.1:8090'"
	if !strings.HasPrefix(tool.Entry, want) {
		t.Fatalf("Entry 必须以 %s 作前缀（否则该赋值不属于本条命令），实际 %q", want, tool.Entry)
	}
	if !strings.Contains(tool.Entry, "echo") {
		t.Fatalf("Entry 应保留原 cmd，实际 %q", tool.Entry)
	}
}

// TestServiceExtProbeAddrDefaultsToListen：探针 addr 省略即取 listen（侧车静态可知的
// 监听地址只有协调端口这一处）。堵的是「改了 listen 忘了改探针地址」——那会让监管
// 把活着的侧车判死并无限重启（与 service 重启循环同一故障形态）。
func TestServiceExtProbeAddrDefaultsToListen(t *testing.T) {
	tool, err := setupServiceExt(t, "ext.svc", "127.0.0.1:8091", "")
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	if tool.HealthProbe == nil || tool.HealthProbe.Addr != "127.0.0.1:8091" {
		t.Fatalf("探针 addr 应自动取 listen，实际 %+v", tool.HealthProbe)
	}
}

// TestServiceExtProbeAddrMismatchRejected：探针地址与 listen 显式不一致 → 装配期即拒，
// 不留到运行期变成「无限重启」。
func TestServiceExtProbeAddrMismatchRejected(t *testing.T) {
	if _, err := setupServiceExt(t, "ext.svc", "127.0.0.1:8092", "127.0.0.1:9999"); err == nil {
		t.Fatal("探针 addr 与 listen 不一致应被拒绝")
	}
}

// TestServiceExtProbeAddrEquivalentHostAccepted：写法不同但等价（localhost ↔ 127.0.0.1）
// 不应被误判为不一致。
func TestServiceExtProbeAddrEquivalentHostAccepted(t *testing.T) {
	if _, err := setupServiceExt(t, "ext.svc", "127.0.0.1:8093", "localhost:8093"); err != nil {
		t.Fatalf("等价主机名不应被拒绝: %v", err)
	}
}

// TestPrepareServiceExtWithoutListenIsNoop：无 listen（纯 daemon 守护，引擎不主动调它）
// 时不注入 COORD_ADDR、也不改探针，Entry 原样拼接 cmd。
func TestPrepareServiceExtWithoutListenIsNoop(t *testing.T) {
	probe := &model.HealthProbe{Kind: model.ProbeKindTCP, Addr: "127.0.0.1:1"}
	entry, got, err := prepareServiceExt(ExtSpec{Cmd: []string{"echo", "hi"}, HealthProbe: probe})
	if err != nil {
		t.Fatalf("无 listen 不应报错: %v", err)
	}
	if entry != "echo hi" {
		t.Fatalf("Entry 应原样拼接 cmd，实际 %q", entry)
	}
	if got != probe {
		t.Fatal("无 listen 时探针应原样透传")
	}
}

// TestCoordAddrKeyNormalizes：地址键归一——等价主机不误判，端口差异必须判出，
// 不可解析时退回小写字面量。
func TestCoordAddrKeyNormalizes(t *testing.T) {
	if coordAddrKey("127.0.0.1:8090") != coordAddrKey("localhost:8090") {
		t.Fatal("127.0.0.1 与 localhost 应视为同一目标")
	}
	if coordAddrKey("127.0.0.1:8090") == coordAddrKey("127.0.0.1:8091") {
		t.Fatal("端口不同不应视为同一目标")
	}
	if got := coordAddrKey("NoPort"); got != "noport" {
		t.Fatalf("不可解析的地址应退回小写字面量，实际 %q", got)
	}
}
