package engine

import (
	"strings"
	"testing"
)

// 本文件覆盖 ai_agent.go 里的「文本渲染 + 草稿规整 + Lua 字面量」这一组纯函数。
// 它们没有分支复杂度，却是**提示词与生成结果的形态真相源**：
//   - capabilitiesText / flowCapabilitiesText 决定模型看到什么（写错形态 → 模型照着写错的调）；
//   - normalizeDef 补 LLM 漏写的字段（少补一个 → 注册期校验失败，agent 反复重试）；
//   - luaLiteral 把模型给的 JSON 编成 Lua 源码（转义漏一个 → 注入/语法错）。

func TestCapabilitiesTextListsAllPrimitives(t *testing.T) {
	a := &AIAgent{extCaps: map[string]map[string]MethodSchema{
		"ext.demo": {"run": {Description: "演示", Parameters: map[string]ParamSchema{
			"x":     {Type: "string", Description: "入参", Required: true},
			"limit": {Type: "number", Description: "上限"},
			"zz":    {Type: "boolean", Description: "开关"},
			"aa":    {Type: "string", Description: "排序探针"},
		}}},
	}}
	txt := a.capabilitiesText()

	// 受门禁原语：位置参数形态
	if !strings.Contains(txt, "ctx.http.Request(") || strings.Contains(txt, "ctx.http.Request({") {
		t.Fatal("内置受门禁原语应按位置参数渲染")
	}
	// 纯函数原子：单具名 table 形态（与 ctx.ext.* 同款），否则模型会写成位置参数
	if !strings.Contains(txt, "ctx.crypto.hash({") || !strings.Contains(txt, "ctx.time.now({") {
		t.Fatal("纯函数原子应按「单具名 table」渲染")
	}
	// ext.*：具名 table + 参数明细（含必填标记）
	if !strings.Contains(txt, "ctx.ext.demo.run({") || !strings.Contains(txt, "(必填)") {
		t.Fatalf("ext.* 应列出参数明细与必填标记，实际:\n%s", txt)
	}
	// 顺序稳定：同一份目录重复渲染必须逐字一致（map 迭代顺序是随机的，
	// 不排序就会出现「同一份目录、每次提示词都不一样」的漂移）。
	for i := 0; i < 20; i++ {
		if a.capabilitiesText() != txt {
			t.Fatal("capabilitiesText 必须可复现（方法与参数都要按名排序）")
		}
	}
}

func TestFlowCapabilitiesTextUsesBareCallForm(t *testing.T) {
	a := &AIAgent{}
	txt := a.flowCapabilitiesText()
	// Flow 的 step.call 是调用目标标识，不带 ctx. 前缀（与 entry 用的形态刻意区分）
	if strings.Contains(txt, "ctx.") {
		t.Fatalf("flow 的能力清单不得带 ctx. 前缀，实际:\n%s", txt)
	}
	if !strings.Contains(txt, "- http.Request") {
		t.Fatalf("应列出「能力.方法」直调形态，实际:\n%s", txt)
	}
	if a.flowCapabilitiesText() != txt {
		t.Fatal("flowCapabilitiesText 必须可复现")
	}
}

func TestNormalizeDefFillsWhatLLMForgets(t *testing.T) {
	a := &AIAgent{}

	// tool：补 runtime/entry/versions/schema，并从 entry 推导 capabilities
	def := map[string]interface{}{"id": "t", "name": "n", "description": "d"}
	a.normalizeDef("tool", def)
	if def["runtime"] != "lua" || def["entry"] == nil || def["versions"] == nil {
		t.Fatalf("tool 草稿应补齐必填字段，实际 %v", def)
	}
	if def["input_schema"] == nil || def["output_schema"] == nil {
		t.Fatal("tool 草稿应补齐 schema")
	}

	// 已给字段不被覆盖（模型显式写的优先）
	def2 := map[string]interface{}{"id": "t", "runtime": "process", "entry": "echo hi"}
	a.normalizeDef("tool", def2)
	if def2["runtime"] != "process" || def2["entry"] != "echo hi" {
		t.Fatalf("已给的字段不得被覆盖，实际 %v", def2)
	}

	// capabilities 自动推导：entry 里用到 ctx.http / ctx.ext.ai
	def3 := map[string]interface{}{"id": "t", "entry": "local r = ctx.http.Request('GET', u)\nlocal x = ctx.ext.ai.complete({prompt='hi'})"}
	a.normalizeDef("tool", def3)
	caps := toStringSlice(def3["capabilities"])
	if len(caps) == 0 {
		t.Fatal("应能从 entry 推导出 capabilities（否则运行期会被门禁拒绝）")
	}
	joined := strings.Join(caps, ",")
	if !strings.Contains(joined, "http") || !strings.Contains(joined, "ext.ai") {
		t.Fatalf("推导结果应含 http 与 ext.ai，实际 %v", caps)
	}

	// 纯函数原子不得被推导进 capabilities（写进去会让 Tool 校验失败：cap 门禁不认它们）
	def4 := map[string]interface{}{"id": "t", "entry": "return ctx.time.now({})"}
	a.normalizeDef("tool", def4)
	if got := toStringSlice(def4["capabilities"]); len(got) != 0 {
		t.Fatalf("纯函数原子不得写进 capabilities，实际 %v", got)
	}

	// flow：补 vars
	def5 := map[string]interface{}{"id": "f"}
	a.normalizeDef("flow", def5)
	if def5["vars"] == nil {
		t.Fatal("flow 草稿应补齐 vars（不写 vars 也合法，但字段要有）")
	}
}

func TestDeriveCapabilitiesMergesDeclared(t *testing.T) {
	got := deriveCapabilities(
		"ctx.http.Request('GET','u'); ctx.ext.ai.complete({})",
		[]string{"db", "  ", "http"},
	)
	joined := strings.Join(got, ",")
	for _, want := range []string{"http", "db", "ext.ai"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("应与已声明的取并集并去空白，实际 %v", got)
		}
	}
	if len(got) != 3 {
		t.Fatalf("并集后应恰好 3 项（去重），实际 %v", got)
	}
	if got := deriveCapabilities("没有用到任何能力", nil); got != nil {
		t.Fatalf("无命中应返回 nil（不写空字段），实际 %v", got)
	}
}

func TestLuaLiteralRendersJSONValuesAsLua(t *testing.T) {
	cases := []struct {
		in   interface{}
		want string
	}{
		{nil, "nil"},
		{"hi", `"hi"`},
		{true, "true"},
		{false, "false"},
		{float64(3), "3"},
		{float64(3.5), "3.5"},
		{int(7), "7"},
		{int64(8), "8"},
		{int32(9), "9"},
		{float32(2.5), "2.5"},
		{[]interface{}{"a", float64(1)}, `{"a", 1}`},
	}
	for _, c := range cases {
		if got := luaLiteral(c.in); got != c.want {
			t.Fatalf("luaLiteral(%#v) = %s, want %s", c.in, got, c.want)
		}
	}
	// map：键有序（保证同一份输入编出的 Lua 源码逐字一致）
	if got := luaLiteral(map[string]interface{}{"b": 1, "a": "x"}); got != `{["a"] = "x", ["b"] = 1}` {
		t.Fatalf("map 应按键排序渲染，实际 %s", got)
	}
	// 兜底：无法表达成 JSON 的值一律编成 nil（不猜一个字面量出来，避免编出语法错的 Lua）
	if got := luaLiteral(make(chan int)); got != "nil" {
		t.Fatalf("不可序列化的值应编成 nil，实际 %s", got)
	}
}

func TestLuaQuoteEscapes(t *testing.T) {
	// 引号/反斜杠/换行必须转义，否则编出的 Lua 源码会被注入或语法错。
	got := luaQuote("a\"b\\c\nd")
	if !strings.Contains(got, `\"`) || !strings.Contains(got, `\\`) {
		t.Fatalf(" Lua 字符串未正确转义: %s", got)
	}
	if strings.Contains(got, "\n") {
		t.Fatalf("换行必须转义为 \\n，实际: %s", got)
	}
	if !strings.HasPrefix(got, `"`) || !strings.HasSuffix(got, `"`) {
		t.Fatalf("应为带引号的 Lua 字符串，实际 %s", got)
	}
}

func TestCapabilitySummariesCoverBuiltinPureExt(t *testing.T) {
	a := &AIAgent{extCaps: map[string]map[string]MethodSchema{
		"ext.demo": {"run": {Description: "d"}},
	}}
	rows := a.capabilitySummaries()
	kinds := map[string]int{}
	for _, r := range rows {
		kinds[fmtKind(r["kind"])]++
	}
	if kinds["builtin"] == 0 || kinds["pure"] == 0 || kinds["ext"] == 0 {
		t.Fatalf("能力目录应含 builtin/pure/ext 三类，实际 %v", kinds)
	}
}

func fmtKind(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// TestReactToolsTextCoversAllPrimitives 锁死 ReAct 工具清单必须覆盖全部内核原语：
// 此前写死的「6+1」名单漏了 subscribe，也漏了纯函数 time/template/crypto；且 call 被渲染成
// 非法的 ctx.call.Call(...) 形态（Lua 侧 call 是裸函数 ctx.call(target, input)，运行期才炸）。
func TestReactToolsTextCoversAllPrimitives(t *testing.T) {
	txt := (&AIAgent{}).reactToolsText(reactAllow{all: true}, false)

	// 受门禁原语：位置参数形态必须齐全（含此前遗漏的 subscribe / config）
	for _, want := range []string{
		"ctx.http.Request(", "ctx.db.query(", "ctx.secret.get(",
		"ctx.event.notify(", "ctx.subscribe.start(", "ctx.config.get(",
	} {
		if !strings.Contains(txt, want) {
			t.Fatalf("ReAct 清单缺受门禁原语 %q:\n%s", want, txt)
		}
	}
	// call 是裸函数：必须是 ctx.call(target, input)，绝不能是 ctx.call.Call(...)
	if !strings.Contains(txt, "ctx.call(") || strings.Contains(txt, "ctx.call.Call") {
		t.Fatalf("call 必须按裸函数形态渲染（ctx.call(...)，不得 ctx.call.Call(...)）：\n%s", txt)
	}
	// 纯函数原子：单具名 table 形态（与 ctx.ext.* 同款），模型才不会写成位置参数
	for _, want := range []string{"ctx.time.now({", "ctx.template.render({", "ctx.crypto.hash({"} {
		if !strings.Contains(txt, want) {
			t.Fatalf("ReAct 清单缺纯函数 %q:\n%s", want, txt)
		}
	}
}

// TestParamListOptionalMarker 锁死 paramList 的 `?` 语义：必填参数**不带** `?`，可选参数带 `?`。
// 此前写反（Required 时标 `?`），模型据此把必填项当可选、漏传参数导致调用失败。
func TestParamListOptionalMarker(t *testing.T) {
	if got := paramList(map[string]ParamSchema{
		"url":  {Type: "string", Required: true},
		"body": {Type: "string"},
	}); got != "body?:string, url:string" {
		t.Fatalf("paramList `?` 语义错误（必填不带?、可选带?），实际 %q", got)
	}
}
