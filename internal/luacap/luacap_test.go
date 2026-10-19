package luacap

// luacap_test.go —— 「Lua entry 用了受门禁能力却没声明」的静态扫描。
//
// 为什么用 AST 而不是正则（这是本文件所有断言的前提）：
// 注释与字符串里的 `ctx.db` **不是**能力使用。正则做法必须在「宁可漏报」与
// 「误报阻塞合法应用」之间二选一——两个方向都会坏（漏报 = 白做；误报 = 用户被迫加
// 假声明来绕过，capability 门禁从此名存实亡）。故断言里专门放了注释与字符串的负例。

import (
	"strings"
	"testing"
)

// TestScanLuaCapabilitiesFindsRealUses 是主用例：各形态都要认出来，且**给对行号**
// （行号是错误信息里最有用的部分——只说「db 未声明」会让人满脚本找）。
func TestScanLuaCapabilitiesFindsRealUses(t *testing.T) {
	script := `
local r = ctx.http.Request("GET", "http://x")
ctx.db.query({ sql = "select 1" })
ctx.secret.get("TOKEN")
local s = ctx.event.notify("evt", { a = 1 })
return { r = r, s = s }
`
	got := Scan(script)
	want := map[string]int{"http": 2, "db": 3, "secret": 4, "event": 5}
	if len(got) != len(want) {
		t.Fatalf("应认出 4 个能力，实际 %d: %+v", len(want), got)
	}
	for _, u := range got {
		line, ok := want[u.Capability]
		if !ok {
			t.Errorf("多认了能力 %q", u.Capability)
			continue
		}
		if u.Line != line {
			t.Errorf("%s 的行号=%d，期望 %d", u.Capability, u.Line, line)
		}
		if u.Expr != "ctx."+u.Capability {
			t.Errorf("%s 的 Expr=%q，应是 ctx.%s", u.Capability, u.Expr, u.Capability)
		}
	}
}

// TestScanLuaCapabilitiesIgnoresCommentsAndStrings 是**误报防线**，也是选 AST 的理由。
func TestScanLuaCapabilitiesIgnoresCommentsAndStrings(t *testing.T) {
	script := `
-- 这里注释里写了 ctx.db.query，故意不该被认出来
--[[ 多行注释里也有 ctx.http.Request ]]
local sql = "select * from ctx.secret.get"     -- 字符串里的更不该算
local other = ctx2.db.query({})                -- 别的变量，不算
ctx.output = { note = "ctx.event.notify" }
`
	if got := Scan(script); len(got) != 0 {
		t.Fatalf("注释/字符串/别的变量里的 ctx.* 不该被认成能力使用: %+v", got)
	}
}

// TestScanLuaCapabilitiesIgnoresPureFuncs 锁一条重要口径：纯函数原子**始终可用、无需声明**
// （写进 capabilities 反而会被 GrantsFromStrings 拒），故这里绝不能报「用了没声明」。
func TestScanLuaCapabilitiesIgnoresPureFuncs(t *testing.T) {
	script := `
local t = ctx.time.now({ format = "rfc3339" })
local h = ctx.crypto.hash({ algo = "sha256", data = "x" })
local i = ctx.id.uuid()
ctx.output = { t = t, h = h, i = i }
`
	if got := Scan(script); len(got) != 0 {
		t.Fatalf("纯函数原子不该被要求声明: %+v", got)
	}
}

// TestScanLuaCapabilitiesFindsInsideNestedBlocks：能力使用藏在
// 条件分支 / 循环 / 闭包 / 匿名函数里也必须认出来——只扫顶层等于没扫。
func TestScanLuaCapabilitiesFindsInsideNestedBlocks(t *testing.T) {
	script := `
local function helper()
  return ctx.secret.get("K")
end
for _, v in ipairs(input.items) do
  if v.use_db then
    ctx.db.query({ sql = v.sql })
  elseif v.other then
    ctx.http.Request("POST", v.url)
  end
end
while true do ctx.event.wait("e") break end
local anon = function() ctx.config.get({ key = "k" }) end
`
	got := Scan(script)
	want := map[string]bool{"secret": true, "db": true, "http": true, "event": true, "config": true}
	if len(got) != len(want) {
		t.Fatalf("应认出 %d 个能力，实际 %d: %+v", len(want), len(got), got)
	}
	for _, u := range got {
		if !want[u.Capability] {
			t.Errorf("多认了 %q", u.Capability)
		}
	}
}

// TestScanCoversEveryExpressionForm 是**覆盖率锁**：把每种表达式形态都用一遍。
//
// 为什么单独一条：capability 使用可以藏在任何表达式里（表构造字段、三元逻辑、算术、
// 字符串拼接、比较……），而漏掉某一种就意味着「那类写法漏报」——漏报不会让测试变红，
// 只会让守卫在真实应用上失效。故这里逐条点名，确保 walker 的每个分支都被走到。
func TestScanCoversEveryExpressionForm(t *testing.T) {
	script := `
local t = { q = ctx.db.query({ sql = "1" }) }          -- 表构造字段
local ok = ctx.secret.get("K") ~= nil and #t > 0        -- 逻辑 + 比较 + 长度
local msg = "x=" .. ctx.config.get({ key = "k" })       -- 字符串拼接
local n = -1 + 0
local neg = not ok
local pick = ok and ctx.event.notify("e", {}) or nil   -- and/or 混用
local fn = function(p) return ctx.http.Request("GET", p) end
local arrowless = ctx.subscribe.start({ source = "s", config = {}, callback = {} })
local nxt = fn("http://x")
local v = t.q or pick or nxt or arrowless or msg or n or neg
return { v = v }
`
	got := Scan(script)
	want := map[string]bool{"db": true, "secret": true, "config": true, "event": true, "http": true, "subscribe": true}
	for _, u := range got {
		if !want[u.Capability] {
			t.Errorf("多认了 %q", u.Capability)
		}
		delete(want, u.Capability)
	}
	for name := range want {
		t.Errorf("漏认了 %q", name)
	}
}

// TestScanLuaCapabilitiesDedupsSameLine：同一行用两次只报一次（否则错误信息里同一项
// 重复出现，看起来像两个不同的漏声明）。
func TestScanLuaCapabilitiesDedupsSameLine(t *testing.T) {
	script := `local a = ctx.db.query({ sql = "1" }) local b = ctx.db.exec({ sql = "2" })`
	if got := Scan(script); len(got) != 1 {
		t.Fatalf("同一行的重复使用应只报一次，实际 %+v", got)
	}
}

// TestScanLuaCapabilitiesGivesUpOnSyntaxError：解析失败返回 nil 而**不**报错——
// 语法错误由 Lua VM 在运行期报一条更准的，越权报第二条只会让人困惑。
func TestScanLuaCapabilitiesGivesUpOnSyntaxError(t *testing.T) {
	if got := Scan("local x = = = end end"); got != nil {
		t.Fatalf("语法错误应返回 nil（不越权报错），实际 %+v", got)
	}
}

// TestMissingCapabilities 对照 declared 列表给结论，并按能力名排序（错误信息的稳定性）。
func TestMissing(t *testing.T) {
	script := `
ctx.db.query({ sql = "1" })
ctx.http.Request("GET", "http://x")
ctx.secret.get("K")
`
	got := Missing(script, []string{"db", "http"})
	if len(got) != 1 || got[0].Capability != "secret" {
		t.Fatalf("应只报 secret，实际 %+v", got)
	}
	if got[0].Line != 4 {
		t.Fatalf("secret 的行号=%d，期望 4", got[0].Line)
	}

	// 全部声明齐了 → 无缺口（这是最常见的正常路径，不许误报）。
	if got := Missing(script, []string{"db", "http", "secret"}); len(got) != 0 {
		t.Fatalf("都声明了却报缺口: %+v", got)
	}
	// 多声明不报（多声明是作者的最小权限取舍，内核不该替他收紧）。
	if got := Missing(script, []string{"db", "http", "secret", "event"}); len(got) != 0 {
		t.Fatalf("多声明不该报错: %+v", got)
	}
}

// TestMissingCapabilitiesSortsByName：多条缺口按能力名排序，
// 保证同一份脚本每次跑出来的错误顺序一致（否则测试与提示词都会随机漂移）。
func TestMissingCapabilitiesSortsByName(t *testing.T) {
	script := "ctx.secret.get('a') ctx.db.query({}) ctx.http.Request('GET','u')"
	got := Missing(script, nil)
	if len(got) != 3 {
		t.Fatalf("应有 3 条缺口: %+v", got)
	}
	want := []string{"db", "http", "secret"}
	for i, u := range got {
		if u.Capability != want[i] {
			t.Fatalf("第 %d 条=%q，期望 %q（应按能力名排序）", i, u.Capability, want[i])
		}
	}
}

// TestMissingCapabilitiesHintNamesDeclaration 是错误文案的判据：
// 提示必须**给出可复制的修法**（照抄即可），而不是只说「没声明」。
func TestMissingCapabilitiesHintNamesDeclaration(t *testing.T) {
	err := ToolCapsError("t1", Missing("ctx.db.query({})", nil))
	if err == nil {
		t.Fatal("应返回错误")
	}
	msg := err.Error()
	// 断言的是「读者能不能照着改」：能力名、行号、可复制的 capabilities 片段、以及
	// 运行期那条误导性报错的原文（让他搜到这条报错时知道真因在这）。
	for _, want := range []string{"db", "capabilities: [db]", "entry.lua:1", "attempt to call a non-function object"} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误信息缺 %q: %s", want, msg)
		}
	}
}
