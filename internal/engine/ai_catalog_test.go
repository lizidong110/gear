package engine

import (
	"strings"
	"testing"
)

// 本文件覆盖 ext.ai 的「静态原语层」（ai_catalog.go）：归类、检索、JSON 截取。
// 它们全部是**确定性、不依赖 LLM** 的纯函数，本该是最容易测的一层，此前却几乎 0% 覆盖。
// 三条口径刻意各测一次（都是「模型凭空臆造工具」这个幻觉主因的防线）：
//  1. 域过滤不得把候选集清空（域内无工具 → 回退全域）；
//  2. 结果可复现（同分保持目录原序，sort.SliceStable）；
//  3. JSON 截取**字符串感知**（花括号出现在字符串/模板里不得算错边界）。

func cat(id, desc string) ToolSummary { return ToolSummary{ID: id, Description: desc} }

func TestDomainOf(t *testing.T) {
	cases := map[string]string{
		"db.query":        "db",
		"ext.ai.complete": "ext",
		"echo":            "echo",
		"":                "general",
		".leading":        ".leading", // 首字符即 "." → 无「第一段」可截，原样返回（只有空串归 general）
	}
	for in, want := range cases {
		if got := domainOf(in); got != want {
			t.Fatalf("domainOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDomainOverviewGroupsAndSorts(t *testing.T) {
	got := DomainOverview([]ToolSummary{
		cat("db.exec", "执行 SQL"), cat("db.query", "查询"), cat("http.get", "取网页"), cat("solo", "独立"),
	})
	// 域名升序、域内 id 升序（结果稳定可复现）
	want := []struct {
		name string
		ids  string
	}{{"db", "db.exec,db.query"}, {"http", "http.get"}, {"solo", "solo"}}
	if len(got) != len(want) {
		t.Fatalf("域数不符: %d, %+v", len(got), got)
	}
	for i, w := range want {
		if got[i].Name != w.name || strings.Join(got[i].ToolIDs, ",") != w.ids {
			t.Fatalf("域 %d 不符: %+v", i, got[i])
		}
		if !strings.Contains(got[i].Summary, "个工具") {
			t.Fatalf("Summary 应含工具数: %q", got[i].Summary)
		}
	}
	if s := summarizeDomains(got); !strings.Contains(s, "- db 域：db.exec, db.query") {
		t.Fatalf("summarizeDomains 格式须与 Lua 侧逐字一致，实际:\n%s", s)
	}
}

func TestRetrieveDomainFilterNeverEmptiesPool(t *testing.T) {
	catalog := []ToolSummary{
		cat("db.query", "查询数据库"), cat("db.exec", "执行 SQL"), cat("http.get", "抓取网页"),
	}
	// 域内无工具 → 回退全域（宁可多给候选，也不要给空列表让模型臆造）
	got := Retrieve(catalog, "nosuch", "查询", 2)
	if len(got) == 0 {
		t.Fatal("限定域无命中时必须回退全域检索，不得返回空候选集")
	}
	// general / all / 空 均表示不限域（LLM 拆解时未知域会填 general，不能因此清零）
	for _, d := range []string{"", "general", "all", "  "} {
		if len(Retrieve(catalog, d, "查询", 2)) == 0 {
			t.Fatalf("domain=%q 应视为不限域", d)
		}
	}
	// 限域命中：只回该域工具
	got = Retrieve(catalog, "http", "数据库", 5)
	if len(got) != 1 || got[0].ID != "http.get" {
		t.Fatalf("限域应只回该域工具（且域内无命中词也不越域），实际 %+v", got)
	}
}

func TestRetrieveShapes(t *testing.T) {
	catalog := []ToolSummary{cat("db.query", "查询数据库"), cat("db.exec", "执行 SQL"), cat("http.get", "抓取网页")}
	if got := Retrieve(nil, "", "q", 3); len(got) != 0 {
		t.Fatalf("空目录应返回空切片（不是 nil 之类让 Lua 侧崩的形态）: %+v", got)
	}
	if got := Retrieve(catalog, "", "", 0); len(got) != 3 {
		t.Fatalf("k<=0 应回落默认 5（目录 3 个即全给），实际 %d", len(got))
	}
	// pool <= k：全给（不打分、不截断）
	if got := Retrieve(catalog, "", "查询", 10); len(got) != 3 {
		t.Fatalf("候选不足 k 时应全给，实际 %d", len(got))
	}
	// 空查询（无有效词元）→ 按目录原序取前 k
	got := Retrieve(catalog, "", "  ", 2)
	if len(got) != 2 || got[0].ID != "db.query" || got[1].ID != "db.exec" {
		t.Fatalf("空查询应保持目录原序取前 k，实际 %+v", got)
	}
}

func TestRetrieveRanksByBM25(t *testing.T) {
	catalog := []ToolSummary{
		cat("http.get", "抓取网页内容"),
		cat("db.query", "查询数据库记录"),
		cat("db.exec", "执行 SQL 语句"),
	}
	got := Retrieve(catalog, "db", "查询数据库", 2)
	if len(got) != 2 {
		t.Fatalf("Top-2 应返回 2 个，实际 %d", len(got))
	}
	if got[0].ID != "db.query" {
		t.Fatalf("最相关的应是 db.query，实际首位 %q", got[0].ID)
	}
	// 同分保持目录原序 → 结果可复现（换顺序跑两次结果一致）
	a := Retrieve(catalog, "db", "查询数据库", 2)
	b := Retrieve(catalog, "db", "查询数据库", 2)
	for i := range a {
		if a[i].ID != b[i].ID {
			t.Fatal("检索结果必须可复现（同分保持目录原序）")
		}
	}
}

func TestTokenize(t *testing.T) {
	got := tokenize("db.Query-1 查询数据库")
	joined := strings.Join(got, "|")
	for _, want := range []string{"db", "query", "1", "查", "查询", "询", "询数", "数", "数据", "据", "据库", "库"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("tokenize 应含 %q，实际 %v", want, got)
		}
	}
	// ASCII 小写归一
	if !strings.Contains(strings.Join(tokenize("HTTP"), "|"), "http") {
		t.Fatal("ASCII 词元应小写归一")
	}
	// 单字兜底：极短中文查询
	if len(tokenize("库")) != 1 {
		t.Fatalf("单字查询应产出一个词元，实际 %v", tokenize("库"))
	}
	if len(tokenize("")) != 0 {
		t.Fatal("空串应无词元")
	}
}

func TestUniqueTokens(t *testing.T) {
	got := uniqueTokens([]string{"a", "b", "a", "c", "b"})
	if strings.Join(got, ",") != "a,b,c" {
		t.Fatalf("去重应保持首次出现顺序，实际 %v", got)
	}
	if len(uniqueTokens(nil)) != 0 {
		t.Fatal("nil 输入应返回空（非 nil 亦可用）")
	}
}

func TestExtractJSONIsStringAware(t *testing.T) {
	// 围栏 + 前后废话
	b, err := extractJSON("好的：```json\n{\"a\":1}\n``` 以上")
	if err != nil || string(b) != `{"a":1}` {
		t.Fatalf("应容忍围栏与前后废话: (%s, %v)", b, err)
	}
	// 数组（首个 JSON 值是数组）
	b, err = extractJSON("结果 [1, {\"x\":2}] 结束")
	if err != nil || string(b) != `[1, {"x":2}]` {
		t.Fatalf("数组截取不符: (%s, %v)", b, err)
	}
	// **红线**：字符串内的花括号（模板引用）不得被当作边界
	b, err = extractJSON(`{"entry":"return {a=1}","tpl":"${steps.s1.output.rows}"}`)
	if err != nil || !strings.Contains(string(b), `${steps.s1.output.rows}`) {
		t.Fatalf("字符串内的 {} 不得影响括号匹配: (%s, %v)", b, err)
	}
	// 转义引号不得提前结束字符串（否则会把半个字符串当成对象边界）
	if _, err := extractJSON(`{"desc":"he said \"{\" ok"}`); err != nil {
		t.Fatalf("转义引号应被正确跳过: %v", err)
	}
	if _, err := extractJSON("没有 JSON"); err == nil {
		t.Fatal("找不到 JSON 必须报错（不静默返回空）")
	}
	if _, err := extractJSON(`{"a":1`); err == nil {
		t.Fatal("未闭合的 JSON 必须报错")
	}
}

func TestExtractJSONToAny(t *testing.T) {
	v, err := extractJSONToAny("noise {\"a\":1} tail")
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if m, ok := v.(map[string]interface{}); !ok || m["a"].(float64) != 1 {
		t.Fatalf("对象应解析为 map，实际 %#v", v)
	}
	v, err = extractJSONToAny("[1,2]")
	if err != nil {
		t.Fatalf("数组解析失败: %v", err)
	}
	if _, ok := v.([]interface{}); !ok {
		t.Fatalf("数组应解析为 []interface{}，实际 %#v", v)
	}
	if _, err := extractJSONToAny("{bad json}"); err == nil {
		t.Fatal("非法 JSON 必须报错（由上层回灌提示词重试，不静默猜值）")
	}
}

// ---- 方法层检索（能力目录） ----
//
// 与 Tool 层（Retrieve）是两个层面：这里检索的是内核能力目录的**方法**（http.Request /
// ext.redis.set），不是 registry 里的业务 Tool。react 下发的是方法层，所以这一层的召回质量
// 直接决定「按需检索」能不能取代「全量平铺」。

func newCatalogAgent(t *testing.T) *AIAgent {
	t.Helper()
	reg := newTestRegistry(t)
	t.Cleanup(func() { _ = reg.Close() })
	return newAIAgent(reg, &scriptedLLM{}, stubRecipes{}, nil)
}

// TestCapabilityKeywordsReferToRealEntries：关键词表的 key 必须指向真实存在的能力或方法。
//
// 拼错一个 key 的后果是**静默失效**——该方法从此检索不到，而没有任何报错。
// 这正是这里最难查的故障形态：词表看着没错，召回悄悄变差。
func TestCapabilityKeywordsReferToRealEntries(t *testing.T) {
	a := newCatalogAgent(t)
	ents := a.capabilityEntries()
	byID := map[string]CapabilityEntry{}
	caps := map[string]bool{}
	for _, e := range ents {
		byID[e.ID] = e
		caps[e.Capability] = true
	}
	// 纯函数与门禁原语的能力名恒在目录里；ext.* 是否装配取决于运行环境，
	// 故 ext 的 key 只校验「方法级 key 的能力部分已被本目录收录」这一条可行约束。
	for k := range capabilityKeywords {
		if _, ok := byID[k]; ok {
			continue // 方法级：真实方法
		}
		if strings.HasPrefix(k, "ext.") {
			continue // ext 域级：装配与否随环境变，不做存在性断言
		}
		if !caps[k] {
			t.Fatalf("关键词表的 key %q 既不是已知方法也不是已知能力（拼错即静默失效）", k)
		}
	}
	// 负向：方法级 key 拼错必须被上面那条抓到。
	if _, ok := byID["db.quer"]; ok {
		t.Fatal("测试前提不成立：db.quer 不该是真实方法")
	}
}

// TestFindCapabilityRecallsOnRealCatalog：在**真实能力目录**上测召回。
// 只造 3 个假工具的自造小目录测不出任何问题——BM25 的 IDF 依赖整个目录的词频分布。
func TestFindCapabilityRecallsOnRealCatalog(t *testing.T) {
	a := newCatalogAgent(t)
	if n := len(a.capabilityEntries()); n < 20 {
		t.Fatalf("测试目录太小（%d 个方法），测不出真实召回", n)
	}
	cases := []struct {
		query string
		want  string // 期望的 Top-1
	}{
		{"发起一个 HTTP 请求", "http.Request"},
		{"查询数据库", "db.query"},
		{"加密一段文本", "crypto.encrypt"},
		{"生成 uuid", "id.uuid"},
		{"读取密钥", "secret.get"},
		{"渲染模板", "template.render"},
		{"当前时间", "time.now"},
		{"发布事件", "event.notify"},
		{"校验 JSON 是否符合 schema", "validate.json_schema"},
	}
	for _, c := range cases {
		hits := a.FindCapability(c.query, "", 1)
		if len(hits) != 1 {
			t.Fatalf("query=%q 应有 1 个命中，实际 %d", c.query, len(hits))
		}
		if hits[0].ID != c.want {
			t.Fatalf("query=%q Top-1 应为 %s，实际 %s（%.2f 分）", c.query, c.want, hits[0].ID, hits[0].Score)
		}
	}
}

// TestFindCapabilityDomainFilterAndFallback：限域检索 + 限域无命中回退全域。
// 回退是刻意的：给空列表会让模型去编工具名，比多给几个无关候选糟得多。
func TestFindCapabilityDomainFilterAndFallback(t *testing.T) {
	a := newCatalogAgent(t)
	// domain 按**能力名**匹配，不是 id 首段：方法 id 形如 ext.redis.set，按首段截会全落进
	// "ext" 一个域，限定等于没限定（domainOf 是给 Tool 层点分 id 设计的）。
	inDB := a.FindCapability("随便查点什么", "db", 50)
	if len(inDB) == 0 {
		t.Fatal("限域 db 应有结果")
	}
	for _, h := range inDB {
		if h.Capability != "db" {
			t.Fatalf("限域后不得出现其它能力，实际 %s", h.Capability)
		}
	}
	// 限域无命中 → 回退全域（不返回空）
	all := a.FindCapability("渲染模板", "db", 5)
	if len(all) == 0 {
		t.Fatal("限域无命中必须回退全域，不得返回空列表")
	}
	if all[0].ID != "template.render" {
		t.Fatalf("回退全域后应仍能命中正确方法，实际 %s", all[0].ID)
	}
}

// TestDescribeCapabilityReturnsFullSchema：describe 必须给出**完整**参数契约
// （类型 / 必填 / 枚举），而 find 只给名字与参数名——这是「按需展开」的第二级。
func TestDescribeCapabilityReturnsFullSchema(t *testing.T) {
	a := newCatalogAgent(t)
	sch, ok := a.DescribeCapability("http.Request")
	if !ok {
		t.Fatal("http.Request 应在能力目录里")
	}
	url, ok := sch.Parameters["url"]
	if !ok {
		t.Fatalf("describe 应给出参数表，实际 %+v", sch.Parameters)
	}
	if !url.Required {
		t.Fatal("url 是必填参数，describe 必须如实标出必填")
	}
	if url.Type != "string" {
		t.Fatalf("url 类型应为 string，实际 %q", url.Type)
	}
	// 负向：目录外的方法必须明确说不存在，而不是返回一个空 schema。
	if _, ok := a.DescribeCapability("db.no_such_method"); ok {
		t.Fatal("不存在的 method 必须返回 ok=false，不得返回空 schema 冒充命中")
	}
}

// TestCapabilityDomainsGroupMethods：域概览把方法按能力聚合，方法名升序。
// 这是「分类」在提示词里的落点：模型先扫一眼有哪些域，再决定去哪个域里检索。
func TestCapabilityDomainsGroupMethods(t *testing.T) {
	a := newCatalogAgent(t)
	ds := a.CapabilityDomains()
	if len(ds) < 5 {
		t.Fatalf("域数太少（%d），测不出聚合", len(ds))
	}
	prev := ""
	for _, d := range ds {
		if d.Name <= prev && prev != "" {
			t.Fatalf("域应按名字升序，实际 %q 在 %q 之后", d.Name, prev)
		}
		prev = d.Name
		if len(d.Methods) == 0 {
			t.Fatalf("域 %q 不该是空的", d.Name)
		}
		if d.Summary == "" {
			t.Fatalf("域 %q 应带一句摘要（否则模型只能看名字猜）", d.Name)
		}
	}
}
