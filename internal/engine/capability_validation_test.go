package engine

// capability_validation_test.go —— 「能力.方法」在**注册期/运行期两个入口**的判定与守卫。
//
// 覆盖两件事：
//  1. ext.* 的**方法级**校验（本次补齐的缺口）：此前只校验能力名，`ext.tls.nope` 能一路穿过
//     注册期与运行期，直到侧车才报错；schema 来自 extensions.yaml（SetupExtensions 转存进目录）。
//  2. 纯函数核心原子（time/template/crypto）是一等调用目标：Flow 直调 + Lua 无条件注入，
//     且**不**进 capability 门禁（声明了反而应当被拒）。
//
// 外加几条「防漂移守卫」：门禁清单 ↔ cap 枚举、ext.ai 方法名 ↔ schema、ext.registry 方法 ↔ schema、
// 纯函数原子不被 deriveCapabilities 自动写成 Tool.Capabilities。

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gear/internal/cap"
	"gear/internal/model"
)

// writeExtConfig 写一份临时 extensions.yaml，返回路径。
func writeExtConfig(t *testing.T, yaml string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "extensions.yaml")
	if err := os.WriteFile(p, []byte(yaml), 0644); err != nil {
		t.Fatal(err)
	}
	return p
}

const tlsExtYAML = `ext.tls:
  cmd: ["./tls-sidecar"]
  methods: [issue, revoke]
  schemas:
    issue:
      description: 签发证书
      parameters:
        domain: {type: string, description: 域名, required: true}
        days: {type: number, description: 有效期}
    revoke:
      description: 吊销证书
      parameters:
        domain: {type: string, required: true}
`

// 缺口 A 核心回归：ext.* 的**方法名与参数**在注册期即被拒，
// 且运行期入口（InvokeCapability，即 REST POST /api/capabilities/invoke）判定同源。
func TestExtMethodLevelStaticValidation(t *testing.T) {
	engReg := newTestRegistry(t)
	defer engReg.Close()
	capReg := NewCapabilityRegistry()
	if _, err := SetupExtensions(capReg, engReg, writeExtConfig(t, tlsExtYAML)); err != nil {
		t.Fatalf("扩展注册失败: %v", err)
	}
	e := New(engReg, WithCapabilities(capReg))

	// schema 真的被转存进目录了（否则下面的正例是「退化为名字级」的假绿）
	if ms, ok := capReg.LookupSchemas("ext.tls"); !ok || len(ms) != 2 {
		t.Fatalf("ext.tls 的方法 schema 应已登记进能力目录，实际 %v / %v", ms, ok)
	}

	good := []struct {
		name string
		call string
		in   map[string]interface{}
	}{
		{"合法方法+合法参数", "ext.tls.issue", map[string]interface{}{"domain": "a.com", "days": float64(90)}},
		{"只给必填", "ext.tls.revoke", map[string]interface{}{"domain": "a.com"}},
		{"ext.registry.capabilities（内建 ext，无参数）", "ext.registry.capabilities", nil},
		{"ext.ai.complete（内建 ext）", "ext.ai.complete", map[string]interface{}{"prompt": "hi"}},
	}
	for i, tc := range good {
		f := &model.Flow{ID: fmt.Sprintf("ok%d", i), Steps: []model.Step{{ID: "s1", Call: tc.call, Input: tc.in}}}
		if err := e.ValidateFlowStatic(f); err != nil {
			t.Fatalf("[%s] 应通过静态校验，实际 %v", tc.name, err)
		}
	}

	bad := []struct {
		name string
		call string
		in   map[string]interface{}
		want string
	}{
		{"未知方法", "ext.tls.nope", map[string]interface{}{"domain": "a.com"}, "没有方法"},
		{"缺必填", "ext.tls.issue", map[string]interface{}{"days": float64(1)}, "缺少必填参数"},
		{"多余参数", "ext.tls.issue", map[string]interface{}{"domain": "a.com", "oops": "x"}, "不接受参数"},
		{"内建 ext.registry 未知方法", "ext.registry.nope", nil, "没有方法"},
		{"内建 ext.ai 未知方法", "ext.ai.nope", nil, "没有方法"},
		{"ext.ai 缺必填", "ext.ai.complete", map[string]interface{}{}, "缺少必填参数"},
		{"内建 ext.ai 多余参数", "ext.ai.complete", map[string]interface{}{"prompt": "hi", "oops": 1}, "不接受参数"},
	}
	for i, tc := range bad {
		f := &model.Flow{ID: fmt.Sprintf("bad%d", i), Steps: []model.Step{{ID: "s1", Call: tc.call, Input: tc.in}}}
		err := e.ValidateFlowStatic(f)
		if err == nil {
			t.Fatalf("[%s] 期望注册期拒绝，实际通过", tc.name)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("[%s] 错误信息应含 %q，实际: %v", tc.name, tc.want, err)
		}
	}

	// 运行期入口必须同源（同一份 schema，绝不出现「注册期放过、运行期才炸」）
	if _, err := e.InvokeCapability("ext.tls", "nope", nil); err == nil || !strings.Contains(err.Error(), "没有方法") {
		t.Fatalf("InvokeCapability 应拒绝未知方法，实际 %v", err)
	}
	if _, err := e.InvokeCapability("ext.tls", "issue", map[string]interface{}{}); err == nil {
		t.Fatal("InvokeCapability 应拒绝缺必填")
	}
}

// 无 schema 的程序化扩展退化为「只校验能力名」——退化是显式的（不是假装校验过），
// 但「能力名根本没注册」仍必须拒。
func TestExtWithoutSchemasDegradesToNameCheck(t *testing.T) {
	engReg := newTestRegistry(t)
	defer engReg.Close()
	capReg := NewCapabilityRegistry()
	ext := InProcessExtension("ext.custom", []string{"ping"}, func(string, map[string]interface{}) (interface{}, error) {
		return "pong", nil
	})
	if err := capReg.Register("ext.custom", ext); err != nil {
		t.Fatal(err)
	}
	e := New(engReg, WithCapabilities(capReg))

	if err := e.ValidateFlowStatic(&model.Flow{ID: "f", Steps: []model.Step{{ID: "s1", Call: "ext.custom.whatever"}}}); err != nil {
		t.Fatalf("无 schema 的 ext 应退化为名字级校验，实际 %v", err)
	}
	if err := e.ValidateFlowStatic(&model.Flow{ID: "f2", Steps: []model.Step{{ID: "s1", Call: "ext.unregistered.x"}}}); err == nil {
		t.Fatal("未注册的能力名必须拒绝")
	}
}

// 纯函数原子在 Flow 里可**直接**调用（无需声明 capabilities），端到端跑通：
// 注册期静态校验 → buildActionEntry 单 table → primitiveHost 放行 → Lua ctx.<atom>.<method>。
func TestPureFuncStepDirectCallEndToEnd(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	flow := &model.Flow{
		ID:   "pure",
		Vars: map[string]interface{}{"user": "东东"},
		Steps: []model.Step{
			{ID: "h", Call: "crypto.hash", Input: map[string]interface{}{"algo": "sha256", "data": "abc"}},
			{ID: "d", Call: "time.format", Input: map[string]interface{}{"unix": float64(0), "format": "date", "tz": "UTC"}},
			{ID: "r", Call: "template.render", Input: map[string]interface{}{"text": "${vars.user}", "vars": map[string]interface{}{"user": "东东"}}},
		},
	}
	if err := reg.UpsertFlow(flow); err != nil {
		t.Fatal(err)
	}
	e := New(reg)
	if err := e.ValidateFlowStatic(flow); err != nil {
		t.Fatalf("纯函数原子直调应通过静态校验: %v", err)
	}
	res, err := e.RunFlowResult("pure")
	if err != nil {
		t.Fatalf("Flow 执行失败: %v", err)
	}
	want := map[string]string{
		"h": "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad",
		"d": "1970-01-01",
		"r": "东东",
	}
	for id, w := range want {
		out, ok := res.Steps[id]["output"].(map[string]interface{})
		if !ok {
			t.Fatalf("step %s 输出类型异常: %T", id, res.Steps[id]["output"])
		}
		if got := fmt.Sprint(out["result"]); got != w {
			t.Fatalf("step %s 期望 %q，实际 %q", id, w, got)
		}
	}

	// 未知方法：注册期即拒（不是拖到 Lua 运行期报「尝试调用 nil」）
	if err := e.ValidateFlowStatic(&model.Flow{ID: "bad", Steps: []model.Step{{ID: "s1", Call: "crypto.nope"}}}); err == nil {
		t.Fatal("未知的纯函数方法应在注册期被拒")
	}
	// 参数拼错：同样注册期拒绝
	if err := e.ValidateFlowStatic(&model.Flow{ID: "bad2", Steps: []model.Step{{ID: "s1", Call: "crypto.hash", Input: map[string]interface{}{"algo": "sha256", "data": "x", "oops": 1}}}}); err == nil {
		t.Fatal("多余参数应在注册期被拒")
	}
}

// Lua 侧：纯函数原子**无条件**注入（不声明任何 capability 也能用），与 ctx.json 同款口径。
func TestLuaPureFuncsUnconditional(t *testing.T) {
	host := luaHost(t, nil, &cap.Providers{}) // 无任何 capability
	out, err := luaRunHost(t, `
		return {
			h = ctx.crypto.hash({ algo = "sha256", data = "abc" }),
			d = ctx.time.format({ unix = 0, format = "date", tz = "UTC" }),
			r = ctx.template.render({ text = "hi ${vars.a}", vars = { a = "b" } }),
			b = ctx.crypto.base64_encode({ data = "x" }),
			j = ctx.json.encode({ ok = true }),
		}
	`, nil, host)
	if err != nil {
		t.Fatalf("纯函数原子应无条件可用: %v", err)
	}
	if out["h"] != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Fatalf("crypto.hash 结果异常: %v", out["h"])
	}
	if out["d"] != "1970-01-01" {
		t.Fatalf("time.format 结果异常: %v", out["d"])
	}
	if out["r"] != "hi b" {
		t.Fatalf("template.render 结果异常: %v", out["r"])
	}
	if out["b"] != "eA==" {
		t.Fatalf("base64_encode 结果异常: %v", out["b"])
	}
}

// Lua 侧 fail-closed：非 table 参数 / 未知枚举 / 未声明的方法都必须报错。
func TestLuaPureFuncsFailClosed(t *testing.T) {
	host := luaHost(t, nil, &cap.Providers{})
	bad := []string{
		`return ctx.crypto.hash("abc")`,                          // 参数不是 table
		`return ctx.crypto.hash({ algo = "crc32", data = "x" })`, // 未知枚举
		`return ctx.crypto.nope({})`,                             // 未声明的方法（未注入）
		`return ctx.time.now({ format = "nope" })`,               // 未知格式
		`return ctx.time.travel({})`,                             // 不存在的方法
	}
	for _, script := range bad {
		if _, err := luaRunHost(t, script, nil, host); err == nil {
			t.Fatalf("应报错: %s", script)
		}
	}
}

// 守卫：门禁原语展示清单 ↔ cap.KnownPrimitives 必须逐项一致（此前手写 6 元素列表漏了 subscribe）。
func TestGatedPrimitiveOrderMatchesCapEnums(t *testing.T) {
	if len(gatedPrimitiveOrder) != len(cap.KnownPrimitives) {
		t.Fatalf("门禁清单（%d）与 cap.KnownPrimitives（%d）条目数不一致: %v",
			len(gatedPrimitiveOrder), len(cap.KnownPrimitives), gatedPrimitiveOrder)
	}
	seen := map[string]bool{}
	for _, p := range gatedPrimitiveOrder {
		if !cap.IsBuiltin(cap.Primitive(p)) {
			t.Fatalf("%q 不是 cap 内置原语（清单漂移）", p)
		}
		if _, ok := builtinCapabilitySchemas[p]; !ok {
			t.Fatalf("门禁原语 %q 缺方法 schema", p)
		}
		if primitiveKind(p) != "builtin" {
			t.Fatalf("%q 的 kind 应为 builtin", p)
		}
		seen[p] = true
	}
	for p := range cap.KnownPrimitives {
		if !seen[string(p)] {
			t.Fatalf("cap.KnownPrimitives 里的 %q 漏在展示清单外（能力目录会看不到它）", p)
		}
	}
}

// 守卫：纯函数原子不进 capability 门禁；声明成 capability 反而应被拒。
func TestPurePrimitivesNotGated(t *testing.T) {
	if len(purePrimitiveOrder) != len(pureFuncPrimitives) {
		t.Fatalf("展示清单与 pureFuncPrimitives 条目数不一致: %v vs %v", purePrimitiveOrder, pureFuncPrimitives)
	}
	for _, p := range purePrimitiveOrder {
		if !pureFuncPrimitives[p] {
			t.Fatalf("%q 应在 pureFuncPrimitives 中", p)
		}
		if cap.IsBuiltin(cap.Primitive(p)) || cap.Primitive(p).Known() {
			t.Fatalf("纯函数原子 %q 不应进 capability 门禁枚举（cap.KnownPrimitives / 已注册扩展名）", p)
		}
		if _, ok := builtinCapabilitySchemas[p]; !ok {
			t.Fatalf("纯函数原子 %q 缺方法 schema", p)
		}
		if primitiveKind(p) != "pure" {
			t.Fatalf("纯函数原子 %q 的 kind 应为 pure", p)
		}
		if _, err := cap.GrantsFromStrings([]string{p}); err == nil {
			t.Fatalf("把纯函数原子 %q 声明为 capability 应被拒（它本就无需声明）", p)
		}
	}
}

// 守卫：ext.ai 的「注册方法清单」与「schema」必须一致——
// 不一致会让静态校验拒掉真能跑的方法，或放过会 500 的方法。
func TestAIMethodNamesMatchDefaultSchemas(t *testing.T) {
	sch := defaultAISchemas()
	if len(sch) != len(aiMethodNames) {
		t.Fatalf("defaultAISchemas(%d) 与 aiMethodNames(%d) 条目数不一致: %v vs %v",
			len(sch), len(aiMethodNames), sortedKeys(sch), aiMethodNames)
	}
	for _, m := range aiMethodNames {
		if _, ok := sch[m]; !ok {
			t.Fatalf("ext.ai 方法 %q 缺 schema（静态校验会拒掉真能跑的方法）", m)
		}
	}
}

// 守卫：ext.registry 的「注册方法清单」与「schema」一致。
func TestRegistryMethodsMatchSchemas(t *testing.T) {
	sch := registrySchemas()
	if len(sch) != len(registryMethods) {
		t.Fatalf("registrySchemas(%d) 与 registryMethods(%d) 条目数不一致", len(sch), len(registryMethods))
	}
	for _, m := range registryMethods {
		if _, ok := sch[m]; !ok {
			t.Fatalf("ext.registry 方法 %q 缺 schema", m)
		}
	}
}

// 守卫：deriveCapabilities 不能把纯函数原子写进 Tool.Capabilities
// （写了就会在 GrantsFromStrings 上炸「未知的 capability」）。
func TestPureFuncsNotDerivedAsCapabilities(t *testing.T) {
	// 先像 SetupExtensions 那样把 ext.slack 并入合法 capability 集合（生产中由注册流程完成），
	// 否则下面的 GrantsFromStrings 断言会被「未注册的扩展名」拦住，测不到本意。
	capReg := NewCapabilityRegistry()
	ext := InProcessExtension("ext.slack", []string{"send"}, func(string, map[string]interface{}) (interface{}, error) {
		return nil, nil
	})
	if err := capReg.Register("ext.slack", ext); err != nil {
		t.Fatal(err)
	}

	entry := `local h = ctx.crypto.hash({ algo = "sha256", data = "x" })
local n = ctx.time.now({})
local s = ctx.template.render({ text = "x" })
local r = ctx.http.Request("GET", "http://x", nil, nil)
local m = ctx.ext.slack.send({ text = "hi" })`
	got := deriveCapabilities(entry, nil)
	want := []string{"ext.slack", "http"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("推导的 capabilities 应为 %v，实际 %v", want, got)
	}
	if _, err := cap.GrantsFromStrings(got); err != nil {
		t.Fatalf("推导出的 capabilities 必须合法: %v", err)
	}
}

// 守卫：能力目录（REST/MCP 与 Lua 共用同一份）必须列出纯函数原子，且 kind=pure。
func TestCapabilityCatalogListsPureAtoms(t *testing.T) {
	list := capabilityCatalog(nil)
	byName := map[string]string{}
	for _, s := range list {
		byName[s["name"].(string)] = s["kind"].(string)
	}
	for _, p := range purePrimitiveOrder {
		if byName[p] != "pure" {
			t.Fatalf("能力目录缺 %q（或 kind 非 pure）: %v", p, byName)
		}
	}
	for _, p := range gatedPrimitiveOrder {
		if byName[p] != "builtin" {
			t.Fatalf("能力目录缺门禁原语 %q: %v", p, byName)
		}
	}
	if len(list) != len(allBuiltinPrimitives()) {
		t.Fatalf("能力目录条数应为 %d，实际 %d", len(allBuiltinPrimitives()), len(list))
	}
}

// 守卫：门禁原语的「位置参数顺序表」（`builtinParamOrder`）与「方法级 schema」必须逐项一致。
//
// 这两份表是 buildActionEntry 拼 Lua 入口的两个输入：schema 决定「什么算合法方法/参数」，
// 顺序表决定「怎么把 args 摆成位置调用」。任一处漏改都会**静默**出错：
//   - schema 有方法、顺序表没有 → buildActionEntry 落到 ext.* 单 table 分支，编出
//     `ctx.db.nope({...})`：注册期放行，运行期才炸成「尝试调用 nil」；
//   - 顺序表有参数、schema 没有 → validatePrimitiveArgs 放行未知参数；
//   - schema 有参数、顺序表没有 → 该参数被**静默丢弃**（调用方传了值，却像没传）。
//
// 注：这条锁此前只写在文档里（REQUIREMENTS 与 README 都声称有 TestBuiltinParamOrderMatchesSchemas），
// 实际并不存在——本轮补上，把「文档声称的守卫」变成真守卫（不再有「文档说有、代码没有」）。
func TestBuiltinParamOrderMatchesSchemas(t *testing.T) {
	for _, prim := range gatedPrimitiveOrder {
		order, ok := builtinParamOrder[prim]
		if !ok {
			t.Fatalf("门禁原语 %q 缺位置参数顺序表（buildActionEntry 会误走 ext.* 单 table 分支）", prim)
		}
		sch := builtinCapabilitySchemas[prim]
		for m := range sch {
			if _, ok := order[m]; !ok {
				t.Fatalf("%s.%s 有 schema 但缺位置参数顺序——buildActionEntry 会编出 ctx.%s.%s({...})，运行期才炸", prim, m, prim, m)
			}
		}
		for m, params := range order {
			ms, ok := sch[m]
			if !ok {
				t.Fatalf("%s.%s 有位置参数顺序但缺 schema（静态校验会拒掉真能跑的方法）", prim, m)
			}
			// 单 table 形态（哨兵 "table"）：参数名不逐个映射，schema 非空即可。
			if len(params) == 1 && params[0] == "table" {
				if len(ms.Parameters) == 0 {
					t.Fatalf("%s.%s 声明为单 table 形态，schema 却没有任何参数", prim, m)
				}
				continue
			}
			inOrder := map[string]bool{}
			for _, p := range params {
				inOrder[p] = true
				if _, ok := ms.Parameters[p]; !ok {
					t.Fatalf("%s.%s 的位置参数 %q 不在 schema 里（静态校验会放行未知参数）", prim, m, p)
				}
			}
			for p := range ms.Parameters {
				if !inOrder[p] {
					t.Fatalf("%s.%s 的 schema 参数 %q 不在位置参数顺序里（调用方传的值会被静默丢弃；若该方法是单 table 形态，顺序表应写成 [\"table\"]）", prim, m, p)
				}
			}
		}
	}
	// 纯函数原子走显式单 table 分支（buildActionEntry 开头就拦），不得混进位置参数顺序表——
	// 混进去会让「单 table」与「位置参数」两套口径并存，正是当初 subscribe 漏登记的同类隐患。
	for _, p := range purePrimitiveOrder {
		if _, ok := builtinParamOrder[p]; ok {
			t.Fatalf("纯函数原子 %q 不应出现在 builtinParamOrder（它走单具名 table，见 buildActionEntry）", p)
		}
	}
	// 兜底：每个门禁方法都必须真能编出入口（上面的集合比对已覆盖主要路径，这里再确认一次行为）。
	for prim, order := range builtinParamOrder {
		for m := range order {
			if _, err := buildActionEntry(prim, m, nil); err != nil {
				t.Fatalf("buildActionEntry(%s, %s) 失败: %v", prim, m, err)
			}
		}
	}
}

// 守卫：门禁原语的 **Lua 绑定**必须覆盖能力目录声明的每个方法。
//
// 与纯函数原子不同（它们的 Lua 绑定由 schema 的键自动生成，构造上漏不了），门禁原语的
// `ctx.<prim>.<method>` 是**手写**绑定（lua.go）。手写就会漏——而静态校验依据 schema 又会放行它
// （schema 说该方法合法），于是「能力目录里有、ctx 上没有」只在运行期炸。此处用真引擎的默认宿主
// 全量授权，逐个方法探 `type(...) == "function"`，不实际调用（避免副作用）。
func TestGatedLuaBindingsCoverSchemas(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	e := New(reg) // 默认宿主装配 KV/HTTP/Event/DB/call/subscribe（#69）

	entry := "local missing = {}\n"
	for _, prim := range gatedPrimitiveOrder {
		// 裸函数绑定的原语（call）：Lua 侧是 `ctx.call(target, input)`，没有 `ctx.call.Call` 这个成员。
		// 能力目录仍以方法名 "Call" 描述它（与 schema 一致），但**调用形态**是裸函数——
		// 因此这里探的是 `ctx.<prim>` 本身，且要求它只能有一个方法（否则多出来的方法无处寻址）。
		if bareFunctionPrimitives[prim] {
			if n := len(builtinCapabilitySchemas[prim]); n != 1 {
				t.Fatalf("裸函数原语 %q 有 %d 个方法——裸调用形态无法区分方法名，只能是 1 个（schema=%v）",
					prim, n, sortedKeys(builtinCapabilitySchemas[prim]))
			}
			entry += fmt.Sprintf("if type(ctx.%s) ~= \"function\" then missing[#missing+1] = %q .. \"(裸函数)\" end\n", prim, prim)
			continue
		}
		for _, m := range sortedKeys(builtinCapabilitySchemas[prim]) {
			entry += fmt.Sprintf("if type(ctx.%s.%s) ~= \"function\" then missing[#missing+1] = %q end\n", prim, m, prim+"."+m)
		}
	}
	entry += "return { missing = missing }\n"

	g, err := cap.GrantsFromStrings(gatedPrimitiveOrder)
	if err != nil {
		t.Fatalf("门禁原语清单应全部是合法 capability: %v", err)
	}
	// 探绑定需要「每个原语都有实现」：引擎默认宿主刻意**不**装配密钥源
	// （无密钥时声明 secret 的 Tool 收到 ErrCapabilityUnavailable，fail-closed），
	// 那是运行期语义、不是绑定缺失，故这里补一个空密钥源，避免把正确行为误判成缺口。
	prov := *e.host
	if prov.Secret == nil {
		prov.Secret = cap.MapSecret{}
	}
	out, err := luaRunHost(t, entry, nil, cap.New(g, &prov))
	if err != nil {
		t.Fatalf("探针脚本执行失败: %v", err)
	}
	if miss, ok := out["missing"].([]interface{}); ok && len(miss) > 0 {
		t.Fatalf("能力目录声明了但 Lua ctx 上没有绑定的方法: %v", miss)
	}
}
