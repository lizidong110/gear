package engine

// invokeform_test.go —— 「Lua 调用形态」这条判据的锁定测试。
//
// 为什么值得锁：这条判据错了不会编译失败、不会在注册期报错，只会在**运行期**炸成
// 「attempt to call a non-function object」或参数错位——排障成本全在运行期。
// 而形态本身是混合的（位置参数 / 单具名 table / 裸函数），最容易在「新增一个原语」
// 或「改一处渲染」时被无声改错。

import (
	"strings"
	"testing"
)

// TestInvokeFormCoversTheThreeShapes 把三种形态各自的代表钉住。
//
// 每组都带上**反例**：db 必须是 table（历史上被渲染成位置参数），
// call 必须是 bare 且签名里**不许出现 .Call**（历史上渲染成 ctx.call.Call）。
func TestInvokeFormCoversTheThreeShapes(t *testing.T) {
	cases := []struct {
		cap, method string
		wantForm    string
		wantContain []string
		wantAbsent  []string
	}{
		// 位置参数：真·按位传参的三个原语。
		{"http", "Request", InvokeFormPositional,
			[]string{"ctx.http.Request(", "method = string", "url = string"}, nil},
		{"secret", "get", InvokeFormPositional,
			[]string{"ctx.secret.get(", "name = string"}, nil},
		{"subscribe", "stop", InvokeFormPositional,
			[]string{"ctx.subscribe.stop(", "handle = string"}, nil},
		{"event", "notify", InvokeFormPositional,
			[]string{"ctx.event.notify(", "name = string", "payload = object"}, nil},

		// 裸函数：ctx.call(target, input)。反例是「出现 .Call」——那是「索引一个函数」，
		// 要到 Lua 运行期才炸。
		{"call", "Call", InvokeFormBare,
			[]string{"ctx.call(target, input)"}, []string{".Call("}},

		// 单具名 table：位置表里写死 {"table"} 的三组 + 纯函数原子。
		{"db", "query", InvokeFormTable,
			[]string{"ctx.db.query({", "sql = string", "driver = string"}, nil},
		{"config", "get", InvokeFormTable,
			[]string{"ctx.config.get({", "key = string"}, nil},
		{"subscribe", "start", InvokeFormTable,
			[]string{"ctx.subscribe.start({", "source = string"}, nil},
		{"crypto", "jwt_sign", InvokeFormTable,
			[]string{"ctx.crypto.jwt_sign({"}, nil},
		{"time", "now", InvokeFormTable,
			[]string{"ctx.time.now({"}, nil},
	}
	for _, c := range cases {
		form, sig, ok := builtinInvokeForm(c.cap, c.method)
		if !ok {
			t.Fatalf("%s.%s：形态判不出来（builtinInvokeForm 返回 ok=false）", c.cap, c.method)
		}
		if form != c.wantForm {
			t.Errorf("%s.%s：形态=%s，期望 %s", c.cap, c.method, form, c.wantForm)
		}
		for _, w := range c.wantContain {
			if !strings.Contains(sig, w) {
				t.Errorf("%s.%s：签名 %q 缺 %q", c.cap, c.method, sig, w)
			}
		}
		for _, w := range c.wantAbsent {
			if strings.Contains(sig, w) {
				t.Errorf("%s.%s：签名 %q 不该含 %q", c.cap, c.method, sig, w)
			}
		}
	}
}

// TestInvokeFormCoversEveryBuiltinMethod 是「不漏」锁：
// 能力目录里登记的每一个内核方法都必须能判出形态——判不出来的会在能力目录里缺字段，
// 目录与实际绑定就此对不上（而这种缺口只在用到时才暴露）。
func TestInvokeFormCoversEveryBuiltinMethod(t *testing.T) {
	for _, p := range allBuiltinPrimitives() {
		ms, ok := builtinCapabilitySchemas[p]
		if !ok {
			t.Fatalf("原语 %s 在 allBuiltinPrimitives 里却没有 schema", p)
		}
		for _, m := range sortedKeys(ms) {
			if _, _, ok := builtinInvokeForm(p, m); !ok {
				t.Errorf("%s.%s 判不出调用形态（能力目录会缺 invoke_form）", p, m)
			}
		}
	}
}

// TestInvokeFormRejectsUnknown：未知能力/方法必须 ok=false（红线：解析点拒绝未知值）。
// 编一个形态出来比不编更坏——模型会照抄。
func TestInvokeFormRejectsUnknown(t *testing.T) {
	if _, _, ok := builtinInvokeForm("db", "nosuch"); ok {
		t.Error("db.nosuch 不该判出形态")
	}
	if _, _, ok := builtinInvokeForm("nosuchcap", "x"); ok {
		t.Error("未知能力名不该判出形态")
	}
	// ext.* 走 invokeFormFor 的分支：能力名未注册也给形态（方法名合法性由各入口自己拒）。
	form, sig, ok := invokeFormFor("ext.redis", "set", map[string]map[string]MethodSchema{})
	if !ok || form != InvokeFormTable || !strings.Contains(sig, "ctx.ext.redis.set(") {
		t.Errorf("ext.* 应判为 table 形态，实际 form=%s sig=%q ok=%v", form, sig, ok)
	}
	// 非 ext 的未知能力不得被 ext 分支兜住。
	if _, _, ok := invokeFormFor("nope", "x", nil); ok {
		t.Error("非 ext 的未知能力不该被判出形态")
	}
}

// TestInvokeFormForUsesExtParams：ext.* 的签名里要带真实字段名（从 extCaps 取），
// 否则等于告诉模型「随便传」。
func TestInvokeFormForUsesExtParams(t *testing.T) {
	extCaps := map[string]map[string]MethodSchema{
		"ext.demo": {
			"go": {Parameters: map[string]ParamSchema{
				"target": {Type: "string"}, "retries": {Type: "number"},
			}},
		},
	}
	_, sig, ok := invokeFormFor("ext.demo", "go", extCaps)
	if !ok {
		t.Fatal("ext.demo.go 应判出形态")
	}
	for _, w := range []string{"ctx.ext.demo.go({", "retries = number", "target = string"} {
		if !strings.Contains(sig, w) {
			t.Errorf("ext 签名 %q 缺 %q", sig, w)
		}
	}
}

// TestWithInvokeFormDoesNotMutateSharedSchemas 是「派生不污染声明」锁。
//
// builtinCapabilitySchemas 是进程级共享的声明真相源；若 withInvokeForm 原地改它，
// extensions.yaml 读出来的 MethodSchema 也带上形态，第二个消费方再判一次就开始漂移。
func TestWithInvokeFormDoesNotMutateSharedSchemas(t *testing.T) {
	before := builtinCapabilitySchemas["db"]["query"]
	if before.InvokeForm != "" || before.LuaCall != "" {
		t.Fatalf("声明源不该带派生字段（说明被原地改过）: form=%q call=%q", before.InvokeForm, before.LuaCall)
	}
	_ = builtinCapabilitySummaries()
	after := builtinCapabilitySchemas["db"]["query"]
	if after.InvokeForm != before.InvokeForm || after.LuaCall != before.LuaCall {
		t.Fatalf("builtinCapabilitySummaries 之后声明源被改了: %+v", after)
	}
}

// TestBuiltinSummariesCarryInvokeForm：能力目录（REST /api/capabilities 与 MCP
// gear_capabilities 共用同一份）必须带上形态字段——这正是本档对外交付的那件事。
func TestBuiltinSummariesCarryInvokeForm(t *testing.T) {
	for _, s := range builtinCapabilitySummaries() {
		for m, sch := range s.Methods {
			if sch.InvokeForm == "" || sch.LuaCall == "" {
				t.Errorf("%s.%s 在能力目录里缺 invoke_form/lua_call（%+v）", s.Name, m, sch)
			}
		}
	}
}

// TestCapabilitiesTextShowsRealForms 锁「给模型看的那份文本」也修好了：
// db 必须是单 table、call 必须是裸函数。这是外部实测里最常被照抄错的两处。
func TestCapabilitiesTextShowsRealForms(t *testing.T) {
	a := &AIAgent{extCaps: map[string]map[string]MethodSchema{}}
	txt := a.capabilitiesText()
	if !strings.Contains(txt, "ctx.db.query({ ") {
		t.Errorf("db.query 应渲染成单具名 table，实际文本里没有：\n%s", txt)
	}
	if strings.Contains(txt, "ctx.call.Call(") {
		t.Errorf("call 被渲染成 ctx.call.Call（那是索引函数，运行期才炸）：\n%s", txt)
	}
	if !strings.Contains(txt, "ctx.call(target, input)") {
		t.Errorf("call 应渲染成裸函数签名：\n%s", txt)
	}
	if !strings.Contains(txt, "ctx.http.Request(") {
		t.Errorf("http.Request 应保持位置参数形态：\n%s", txt)
	}
	// config.* 与 db.* 同形态，也必须是 table。
	if strings.Contains(txt, "ctx.config.get(key: string)") {
		t.Errorf("config.get 被渲染成位置参数了：\n%s", txt)
	}
}

// TestReactToolsTextShowsRealForms 同上，换 react 那条渲染路径。
//
// 两条路径此前是两份独立的 if/else（都会把 db 渲成位置参数），现在共用 callSignature——
// 这个测试锁住「两条路径看到的形态一致」。
func TestReactToolsTextShowsRealForms(t *testing.T) {
	a := &AIAgent{extCaps: map[string]map[string]MethodSchema{}}
	txt := a.reactToolsText(reactAllow{all: true}, false)
	if !strings.Contains(txt, "ctx.db.query({ ") {
		t.Errorf("react 工具清单里 db.query 应是单具名 table：\n%s", txt)
	}
	if strings.Contains(txt, "ctx.call.Call(") {
		t.Errorf("react 工具清单里 call 被渲染成 ctx.call.Call：\n%s", txt)
	}
	if !strings.Contains(txt, "ctx.call(target, input)") {
		t.Errorf("react 工具清单里 call 应是裸函数签名：\n%s", txt)
	}
}

// TestCapabilityEntriesCarryInvokeForm：find/describe 的出口也带形态
// （meta 模式与 gear_capabilities(mode=find) 的唯一数据源）。
func TestCapabilityEntriesCarryInvokeForm(t *testing.T) {
	a := &AIAgent{extCaps: map[string]map[string]MethodSchema{
		"ext.demo": {
			"go": {Description: "d", Parameters: map[string]ParamSchema{"x": {Type: "string"}}},
		},
	}}
	entries := a.capabilityEntries()
	if len(entries) == 0 {
		t.Fatal("能力目录条目为空")
	}
	byID := map[string]CapabilityEntry{}
	for _, e := range entries {
		byID[e.ID] = e
	}
	for id, wantForm := range map[string]string{
		"db.query":     InvokeFormTable,
		"call.Call":    InvokeFormBare,
		"http.Request": InvokeFormPositional,
		"ext.demo.go":  InvokeFormTable,
	} {
		e, ok := byID[id]
		if !ok {
			t.Fatalf("目录里没有 %s", id)
		}
		if e.InvokeForm != wantForm {
			t.Errorf("%s：invoke_form=%s，期望 %s", id, e.InvokeForm, wantForm)
		}
		if e.LuaCall == "" {
			t.Errorf("%s：缺 lua_call", id)
		}
	}
}
