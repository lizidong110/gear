package engine

// ai_catalog.go 实现 ext.ai 的「静态原语层」（PRD §9 的三层划分：LLM 层 / Agent 层[Lua] / 静态原语层[Go]）。
//
// 本文件只做**确定性、不依赖 LLM** 的活：
//   - ToolSummary：喂给 LLM 的最小工具上下文（§9.3，仅 id + 描述 + schema，不含实现）；
//   - DomainOverview / summarizeDomains：按 namespace 归类的能力域概览；
//   - Retrieve：域内 BM25 式检索（Top-K 候选工具）；
//   - extractJSONToAny：从 LLM 文本里截取第一个完整 JSON 值（容忍前后废话、代码围栏）。
//
// 这是**稳定层**：换模型只改 ai_llm.go，调提示词只改 recipes/*.yaml 与 Lua agent，
// 改检索/归类算法只改这里——三层互不牵连（PRD §9｜M-EXT-AI 分层决策）。
//
// 与 examples/gear-ai-sidecar 同源：侧车把本文件的导出面（ToolSummary / DomainOverview /
// Retrieve）直接复用，不再各写一份（避免算法与口径漂移）。

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"unicode"
)

// ToolSummary 是喂给 LLM 的最小工具上下文（PRD §9.3）：仅 id + 描述 + 输入/输出 schema。
// 不含 runtime/entry/versions 等实现细节——LLM 选工具只需要「叫什么、干什么、要什么参数」；
// 把实现塞进提示词既烧上下文，也给模型提供了臆造实现的素材。
//
// 字段名即 JSON 契约：Lua 侧读 tools[i].id / .input_schema（examples/ai-create-flow.yaml），
// 侧车的 parseTools 也按 input_schema/output_schema 回读（examples/gear-ai-sidecar/main.go）。
type ToolSummary struct {
	ID           string                 `json:"id"`
	Description  string                 `json:"description"`
	InputSchema  map[string]interface{} `json:"input_schema,omitempty"`
	OutputSchema map[string]interface{} `json:"output_schema,omitempty"`
}

// DomainSummary 是一个能力域的概览。
// 字段名即 JSON 契约：Lua 侧读 d.name / d.tool_ids（逐字对应 examples/ai-create-flow.yaml 的
// domain_brief 拼装），summary 供人读与后续扩展。
type DomainSummary struct {
	Name    string   `json:"name"`
	ToolIDs []string `json:"tool_ids"`
	Summary string   `json:"summary"`
}

// domainOf 取工具 id 的第一个 "." 段作为能力域（无 "." 则整个 id 即域名）。
// 约定：工具 id 用小写点分命名（db.query、ext.ai.complete、demo.echo），故 namespace 即域。
func domainOf(id string) string {
	if i := strings.Index(id, "."); i > 0 {
		return id[:i]
	}
	if id == "" {
		return "general"
	}
	return id
}

// DomainOverview 按 namespace 把工具目录归类成能力域概览（域名升序、域内 id 升序，结果稳定可复现）。
// 纯规则、无需 LLM——这是「先告诉模型有哪些域，再让它归步」的第一步。
func DomainOverview(catalog []ToolSummary) []DomainSummary {
	byName := map[string][]string{}
	for _, t := range catalog {
		d := domainOf(t.ID)
		byName[d] = append(byName[d], t.ID)
	}
	out := make([]DomainSummary, 0, len(byName))
	for name, ids := range byName {
		sort.Strings(ids)
		out = append(out, DomainSummary{
			Name:    name,
			ToolIDs: ids,
			Summary: fmt.Sprintf("%d 个工具", len(ids)),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// summarizeDomains 把域概览压成一段短文本喂给 LLM。
// 格式与 Lua 侧（examples/ai-create-flow.yaml 的 domain_brief）**逐字一致**：
//
//   - <域名> 域：id1, id2
//
// 两侧同款是刻意的——Go agent 与 Lua agent 是同一套提示词的两种载体，措辞漂移会让
// 「换载体」变成换行为，无法用同一批测试对照。
func summarizeDomains(domains []DomainSummary) string {
	var b strings.Builder
	for _, d := range domains {
		b.WriteString("- " + d.Name + " 域：" + strings.Join(d.ToolIDs, ", ") + "\n")
	}
	return b.String()
}

// Retrieve 在指定能力域内按 BM25 检索与 query 最相关的 Top-K 工具。
//
// 域过滤口径（防「候选集被清空 → 模型凭空臆造工具」这个幻觉主因）：
//   - domain 为空 / "general" / "all" → 不限域（LLM 拆解时对未知域会填 general，不能因此清零）；
//   - 限定了域但域内无工具 → 回退为全域检索（宁可多给候选，也不要给空列表）。
//
// 评分：对「工具 id + 描述」做词元化（ASCII 词 + CJK 单字/双字），按 BM25（k1=1.2 / b=0.75）打分，
// 分数相同保持目录原序（sort.SliceStable），结果可复现。目录不足 k 个就全给；
// query 无有效词元（空查询）时按目录原序取前 k 个。
func Retrieve(catalog []ToolSummary, domain, query string, k int) []ToolSummary {
	if k <= 0 {
		k = 5
	}
	if len(catalog) == 0 {
		return []ToolSummary{}
	}
	pool := catalog
	if d := strings.TrimSpace(domain); d != "" && d != "general" && d != "all" {
		filtered := make([]ToolSummary, 0, len(catalog))
		for _, t := range catalog {
			if domainOf(t.ID) == d {
				filtered = append(filtered, t)
			}
		}
		if len(filtered) > 0 {
			pool = filtered
		}
	}
	if len(pool) <= k {
		out := make([]ToolSummary, len(pool))
		copy(out, pool)
		return out
	}

	docs := make([][]string, len(pool))
	total := 0
	for i, t := range pool {
		docs[i] = tokenize(t.ID + " " + t.Description)
		total += len(docs[i])
	}
	terms := uniqueTokens(tokenize(query))
	if len(terms) == 0 || total == 0 {
		// 查不出词元（空查询）或整个目录都没词元：无分可打，按目录原序取前 k。
		out := make([]ToolSummary, k)
		copy(out, pool[:k])
		return out
	}

	out := make([]ToolSummary, 0, k)
	for _, s := range bm25Rank(docs, terms) {
		if len(out) >= k {
			break
		}
		out = append(out, pool[s.Idx])
	}
	return out
}

// bm25Score 是一篇文档的打分结果：下标 + 分数。
type bm25Score struct {
	Idx   int
	Score float64
}

// bm25Rank 对一批**已词元化**的文档按查询词元打分，返回按分数降序的结果。
//
// 抽出来是因为 Tool 层（Retrieve）与方法层（FindCapability）共用同一套打分：抄两份的话，
// 将来调 k1/b 或改长度归一化必然漏改其中一份，两个检索器的排序口径会悄悄漂移。
// 同分保持文档原序（SliceStable），结果可复现。
func bm25Rank(docs [][]string, terms []string) []bm25Score {
	df := map[string]int{}
	total := 0
	for _, toks := range docs {
		total += len(toks)
		seen := map[string]bool{}
		for _, tk := range toks {
			if !seen[tk] {
				seen[tk] = true
				df[tk]++
			}
		}
	}
	avgdl := float64(total)
	if len(docs) > 0 {
		avgdl /= float64(len(docs))
	}
	if avgdl == 0 {
		avgdl = 1 // 防除零：空文档时长度归一化项失去意义，退化为纯 IDF·TF 打分
	}
	const k1, b = 1.2, 0.75
	scores := make([]bm25Score, len(docs))
	for i, toks := range docs {
		tf := map[string]int{}
		for _, tk := range toks {
			tf[tk]++
		}
		dl := float64(len(toks))
		var s float64
		for _, term := range terms {
			n := df[term]
			if n == 0 {
				continue
			}
			// IDF 加了 +1 并做平滑：目录小时不至于出现负分（负分会让「无命中」看起来像命中）。
			idf := math.Log(1 + (float64(len(docs))-float64(n)+0.5)/(float64(n)+0.5))
			f := float64(tf[term])
			s += idf * (f * (k1 + 1)) / (f + k1*(1-b+b*dl/avgdl))
		}
		scores[i] = bm25Score{Idx: i, Score: s}
	}
	sort.SliceStable(scores, func(i, j int) bool { return scores[i].Score > scores[j].Score })
	return scores
}

// ---- 方法层检索（能力目录） ----
//
// 与上面的 Tool 层是**两个层面，语料不相交**：
//   - Tool 层：registry 里用户定义的业务 Tool（Retrieve 的输入，供 ext.ai.generate 设计态用）；
//   - 方法层：内核能力目录（http.Request / ext.redis.set …，实测 28 能力 / 115 方法）。
//
// 分层之前这里没有索引：react 下发的是方法层，而唯一能检索的 Retrieve 只认 Tool 层，
// 于是「给 react 做按需检索」无从下手——只能全量平铺，工具越多模型越选不准。

// CapabilityEntry 是方法层的一条语料（一个方法）。
// 这里的「分类维度」是**能力名本身**（Capability 字段）：`ext.redis.set` 属于 `ext.redis` 域。
// 刻意不引入自由 tags——标签会漂移（db/database/数据库 三个同义标签谁维护？），
// 而能力名是装配时就定死的结构化层级，零维护成本，且天然与权限边界（capabilities）同构。
type CapabilityEntry struct {
	ID          string   `json:"id"`               // http.Request / ext.redis.set——与 react 工具名同源
	Capability  string   `json:"capability"`       // http / ext.redis
	Method      string   `json:"method"`           // Request / set
	Kind        string   `json:"kind"`             // builtin / pure / ext
	Description string   `json:"description"`      //
	Params      []string `json:"params,omitempty"` // 参数名：检索信号最强的一部分
	// InvokeForm / LuaCall 把「在 Lua 侧到底怎么调」带进 find/describe 的结果
	// （2026-10-03）。find 是 meta 模式与 gear_capabilities(mode=find) 的唯一出口，
	// 而此前它只给参数名——于是「db.query 有哪些参数」看得见，「它收 table 还是收位置参数」看不见，
	// 模型只能猜，猜错要到运行期才炸。字段值由 invokeFormFor 统一判（见 invokeform.go）。
	InvokeForm string `json:"invoke_form,omitempty"`
	LuaCall    string `json:"lua_call,omitempty"`
}

// CapabilityHit 是 find 的一个命中：比条目多一个分数，让调用方（与模型）看得出「为什么是这条」。
// 有分数也能让模型分辨「检索到了但都不太像」和「检索到明确的一条」。
type CapabilityHit struct {
	CapabilityEntry
	Score float64 `json:"score"`
}

// CapabilitySchema 是 describe 的返回：一个方法的完整参数契约。
// find 只给摘要（名字 + 一句话 + 参数名），describe 才给类型与必填——
// 这是「按需展开」的两级：模型先看清有哪些、再只取要用的那一个的完整契约。
type CapabilitySchema struct {
	CapabilityEntry
	Parameters map[string]ParamSchema `json:"parameters,omitempty"`
}

// CapabilityDomain 是方法层的一个能力域概览，给模型「先看清有哪些域」用。
type CapabilityDomain struct {
	Name    string   `json:"name"`
	Kind    string   `json:"kind"`
	Methods []string `json:"methods"`
	Summary string   `json:"summary"`
}

// capabilityEntries 由能力目录构造方法层索引（能力名升序 + 方法名升序，顺序稳定可复现）。
func (a *AIAgent) capabilityEntries() []CapabilityEntry {
	out := []CapabilityEntry{}
	for _, p := range allBuiltinPrimitives() {
		ms, ok := builtinCapabilitySchemas[p]
		if !ok {
			continue
		}
		for _, m := range sortedKeys(ms) {
			e := CapabilityEntry{
				ID: p + "." + m, Capability: p, Method: m, Kind: primitiveKind(p),
				Description: ms[m].Description, Params: sortedKeys(ms[m].Parameters),
			}
			e.InvokeForm, e.LuaCall, _ = invokeFormFor(p, m, a.extCaps)
			out = append(out, e)
		}
	}
	// ext 名来自 map，遍历顺序不稳定——排序后再进数组。顺序不稳会让「同一 query 的 Top-K」
	// 在不同进程里不一样，线上的检索问题将无法复现。
	extNames := make([]string, 0, len(a.extCaps))
	for name := range a.extCaps {
		extNames = append(extNames, name)
	}
	sort.Strings(extNames)
	for _, name := range extNames {
		for _, m := range sortedKeys(a.extCaps[name]) {
			e := CapabilityEntry{
				ID: name + "." + m, Capability: name, Method: m, Kind: primitiveKind(name),
				Description: a.extCaps[name][m].Description, Params: sortedKeys(a.extCaps[name][m].Parameters),
			}
			e.InvokeForm, e.LuaCall, _ = invokeFormFor(name, m, a.extCaps)
			out = append(out, e)
		}
	}
	// 全表按（能力名, 方法名）排序：allBuiltinPrimitives 的顺序是给「目录展示」用的
	// （门禁原语在前、纯函数在后），检索索引不该继承它——否则同分时的兜底顺序取决于
	// 展示顺序，换一次展示口径检索结果就跟着变。
	sort.Slice(out, func(i, j int) bool {
		if out[i].Capability != out[j].Capability {
			return out[i].Capability < out[j].Capability
		}
		return out[i].Method < out[j].Method
	})
	return out
}

// capabilityKeywords 是能力名的检索同义词——**受控词表**，不是自由标签。
//
// 为什么必须有：方法描述是英文 + 代码味的（db.exec 的描述里根本没有「数据库」三个字），
// 而用户的检索词是中文的。两侧词元不相交，BM25 再准也打不出分。实测「写一条数据库记录」
// 排第一的是 validate.json_schema——只因为它那句长描述里「数据」出现了两次。
//
// 为什么不做成自由 tags：标签由谁填、填几个、同义词怎么收敛（db / database / 数据库）全靠
// 自觉，必然漂移成一堆同义标签。这里与**能力**绑定：一处定义、该能力下所有方法共享，
// 由内核或插件作者在能力定义处维护，是可审计、可校验的受控词表——
// 分类维度仍然是能力名本身，关键词只是给它补上「用户会怎么说」。
//
// 只收录「描述里不出现、但用户一定会用」的词；描述已有的词不必重复，BM25 会自己算到。
//
// key 可以是**能力名**（该能力下没有更细词条时兜底）也可以是**方法全名**（优先匹配）。
// 方法级必须存在：crypto 一家 13 个方法，共享「加密」会让「加密一段文本」排到 crypto.hash 上
// （就因为 hash 的描述里恰好有「文本」二字）——域越大、语义越杂，越要按方法拆。
//
// 每个 key 都必须是真实存在的能力名或方法全名，由 TestCapabilityKeywordsReferToRealEntries 守着：
// 拼错一个就静默失效，而静默失效正是这里最难查的故障形态。
var capabilityKeywords = map[string][]string{
	// ---- 能力级（兜底） ----
	"http":      {"http", "请求", "接口", "api", "网页", "下载", "上传", "post", "get", "调接口"},
	"call":      {"工具", "流程", "调别的工具", "编排"},
	"db":        {"数据库", "sql", "查表", "记录", "读库", "存表"},
	"secret":    {"密钥", "凭据", "凭证", "密码", "token", "证书"},
	"event":     {"事件", "消息", "广播", "通知"},
	"subscribe": {"订阅", "监听", "推送"},
	"config":    {"配置", "设置项", "偏好"},
	"time":      {"时间", "日期", "时间戳"},
	"template":  {"模板", "渲染", "占位符", "变量替换"},
	"crypto":    {"密码学", "加解密", "签名", "哈希"},
	"id":        {"标识", "随机数"},
	"validate":  {"校验", "验证", "合法", "schema"},

	// ---- 方法级（优先；大而杂的域必须拆到这一层） ----
	"db.query":               {"查询", "读取", "select", "查数据", "检索"},
	"db.exec":                {"写入", "插入", "更新", "删除", "写操作", "insert", "update", "delete"},
	"db.batch":               {"批量", "事务", "多条", "原子"},
	"db.ping":                {"探活", "连通", "健康检查"},
	"event.notify":           {"发布", "广播", "发送事件", "触发"},
	"event.wait":             {"等待", "接收", "阻塞"},
	"subscribe.start":        {"订阅", "监听", "mqtt"},
	"subscribe.stop":         {"取消订阅", "停止监听"},
	"config.get":             {"读取配置", "获取配置"},
	"config.set":             {"写入配置", "保存配置", "修改配置"},
	"config.delete":          {"删除配置", "清除配置"},
	"config.list":            {"列出配置", "配置列表"},
	"time.now":               {"当前时间", "现在", "今天"},
	"time.parse":             {"解析时间", "字符串转时间"},
	"time.format":            {"格式化时间", "时间戳转字符串"},
	"time.sleep":             {"等待", "延时", "休眠", "阻塞"},
	"crypto.encrypt":         {"加密", "aes", "密文", "encode"},
	"crypto.decrypt":         {"解密", "aes", "明文"},
	"crypto.hash":            {"摘要", "哈希", "md5", "sha", "散列"},
	"crypto.hmac":            {"hmac", "签名", "消息认证"},
	"crypto.hmac_verify":     {"hmac", "验签", "校验签名"},
	"crypto.jwt_sign":        {"jwt", "签发", "签发令牌"},
	"crypto.jwt_verify":      {"jwt", "验签", "校验令牌"},
	"crypto.jwt_decode":      {"jwt", "解码", "解析令牌"},
	"crypto.password_hash":   {"口令", "密码", "存密码"},
	"crypto.password_verify": {"口令", "密码", "校验密码", "登录"},
	"crypto.random":          {"随机数", "随机", "nonce", "盐"},
	"crypto.base64_encode":   {"base64", "编码"},
	"crypto.base64_decode":   {"base64", "解码"},
	"id.uuid":                {"uuid", "唯一标识", "guid"},
	"id.nanoid":              {"nanoid", "短标识", "唯一标识"},
	"id.code":                {"验证码", "随机码", "邀请码", "短信"},

	// ---- ext 域级（方法名是英文，靠域名把中文查询接进来） ----
	"ext.ai":        {"智能体", "大模型", "llm", "对话", "生成"},
	"ext.registry":  {"注册表", "工具列表", "能力目录", "元数据"},
	"ext.redis":     {"redis", "缓存", "键值", "kv"},
	"ext.email":     {"邮件", "邮箱", "发信", "smtp"},
	"ext.oss":       {"对象存储", "文件上传", "下载", "oss", "s3"},
	"ext.filestore": {"文件", "存储", "上传", "下载"},
	"ext.kafka":     {"消息队列", "mq", "kafka", "消费", "投递"},
	"ext.rabbitmq":  {"消息队列", "mq", "rabbitmq", "消费", "投递"},
	"ext.cron":      {"定时", "调度", "计划任务", "cron"},
	"ext.sms":       {"短信", "验证码", "通知"},
	"ext.webhook":   {"webhook", "回调", "钩子"},
	"ext.wecom":     {"企业微信", "机器人", "群通知"},
	"ext.pdf":       {"pdf", "文档", "导出"},
	"ext.excel":     {"excel", "表格", "导出"},
	"ext.oidc":      {"oidc", "单点登录", "身份认证"},
	"ext.serve":     {"服务", "http 服务", "监听端口"},
}

// keywordsFor 取一条方法的检索关键词：**方法级优先，能力级兜底**。
// 没有对应词条时返回空串（BM25 仍能按 id / 描述 / 参数名打分，不是零召回）。
func keywordsFor(e CapabilityEntry) string {
	if kw, ok := capabilityKeywords[e.ID]; ok {
		return strings.Join(kw, " ")
	}
	return strings.Join(capabilityKeywords[e.Capability], " ")
}

// firstSentence 取描述的第一句（到首个句号 / 分号 / 换行为止）。
//
// 长描述尾部的「★ 口径」说明会把词频堆高，让无关方法靠长度和重复词胜出——
// 检索只需要知道它「大概是干什么的」，完整描述由 describe 返回。
func firstSentence(s string) string {
	if i := strings.IndexAny(s, "。；;\n"); i >= 0 {
		return s[:i]
	}
	return s
}

// capabilityCorpus 是一条方法的检索语料：id 两份 + 能力关键词两份 + 描述首句 + 参数名。
//
// 加权（重复进语料）的两项：
//   - id：同时含能力名与方法名，是「我要找什么」最强的信号；
//   - 关键词：跨中英文的桥，权重必须高于描述，否则长描述仍会用词频压过它。
//     BM25 有 TF 饱和，重复一次只是抬升，不会压过其它词。
//
// 参数名必须进语料：搜 "sql" 该命中 db.query，而它的描述里未必出现这个词——
// 只按描述检索会让「按参数名找方法」这条路整个失效。
func capabilityCorpus(e CapabilityEntry) string {
	kw := keywordsFor(e)
	return e.ID + " " + e.ID + " " + kw + " " + kw + " " +
		firstSentence(e.Description) + " " + strings.Join(e.Params, " ")
}

// FindCapability 在能力目录里按 BM25 检索方法，返回 Top-K。
//
// domain 按**能力名**匹配而不是 id 首段：方法层 id 形如 "ext.redis.set"，按首段截会全部
// 落进 "ext" 一个域（domainOf 是给 Tool 层的点分 id 设计的），限定域等于没限定。
func (a *AIAgent) FindCapability(query, domain string, k int) []CapabilityHit {
	if k <= 0 {
		k = 5
	}
	pool0 := a.capabilityEntries()
	if len(pool0) == 0 {
		return []CapabilityHit{}
	}
	pool := pool0
	limited := false
	if d := strings.TrimSpace(domain); d != "" && d != "general" && d != "all" {
		filtered := make([]CapabilityEntry, 0, len(pool))
		for _, e := range pool {
			if e.Capability == d {
				filtered = append(filtered, e)
			}
		}
		// 限域无命中回退全域，与 Retrieve 同口径：给空列表会让模型自己去编工具名，
		// 比多给几个无关候选糟得多。
		if len(filtered) > 0 {
			pool = filtered
			limited = true
		}
	}
	terms := uniqueTokens(tokenize(query))
	hits := rankCapability(pool, terms, k)
	// 限域后**一个词都没命中**（最高分 0）：说明这个域选错了，回退全域重检。
	// 判据是「有没有命中」而不是「域内够不够 k 个」——后者会让限域变成装饰：
	// 只要域里有 k 个方法就照给，哪怕全是无关项。
	// 空查询（terms 为空）不回退：那是「没给检索词」，本来就该按目录原序取域内前 k。
	if limited && len(terms) > 0 && len(hits) > 0 && hits[0].Score == 0 {
		return rankCapability(pool0, terms, k)
	}
	return hits
}

// rankCapability 对一个候选池按查询词元打分取 Top-K。
//
// 不做「候选不足 k 个就全给」的短路：限域后池子常常很小，短路会跳过打分，
// 于是「域内谁更相关」这件事完全没排——限域就只剩过滤作用了。
func rankCapability(pool []CapabilityEntry, terms []string, k int) []CapabilityHit {
	if len(pool) == 0 {
		return []CapabilityHit{}
	}
	docs := make([][]string, len(pool))
	for i, e := range pool {
		docs[i] = tokenize(capabilityCorpus(e))
	}
	out := make([]CapabilityHit, 0, k)
	for _, s := range bm25Rank(docs, terms) {
		if len(out) >= k {
			break
		}
		out = append(out, CapabilityHit{CapabilityEntry: pool[s.Idx], Score: s.Score})
	}
	return out
}

// DescribeCapability 返回某个方法的完整参数契约；id 不在目录里时 ok=false。
// 与 FindCapability 的合成即「按需展开」：先检索到名字，再只取这一个的 schema。
func (a *AIAgent) DescribeCapability(id string) (CapabilitySchema, bool) {
	for _, e := range a.capabilityEntries() {
		if e.ID != id {
			continue
		}
		sch := CapabilitySchema{CapabilityEntry: e}
		if ms, ok := builtinCapabilitySchemas[e.Capability]; ok {
			sch.Parameters = ms[e.Method].Parameters
		} else if ms, ok := a.extCaps[e.Capability]; ok {
			sch.Parameters = ms[e.Method].Parameters
		}
		return sch, true
	}
	return CapabilitySchema{}, false
}

// CapabilityDomains 给方法层的域概览（能力名升序）：每个域含哪些方法 + 一句摘要。
// 这是「分类」在提示词里的落点——模型先扫一眼 28 个域，再决定去哪个域里检索，
// 比在 115 个方法里盲检准得多。
func (a *AIAgent) CapabilityDomains() []CapabilityDomain {
	byName := map[string]*CapabilityDomain{}
	order := []string{}
	for _, e := range a.capabilityEntries() {
		d, ok := byName[e.Capability]
		if !ok {
			d = &CapabilityDomain{Name: e.Capability, Kind: e.Kind, Summary: e.Description}
			byName[e.Capability] = d
			order = append(order, e.Capability)
		}
		d.Methods = append(d.Methods, e.Method)
	}
	out := make([]CapabilityDomain, 0, len(order))
	for _, n := range order {
		out = append(out, *byName[n])
	}
	return out
}

// tokenize 把文本切成检索词元：ASCII 词元（小写；点号/连字符/斜杠等作分隔）+ CJK 单字与双字。
//
// 中文没有空格分词，用 bigram 近似（「查询数据库」→ 查询/询数/数据/据库），同时保留单字兜底
// 极短查询（「库」）。工具描述与 id 都很短，bigram + BM25 已足够区分候选，不引入分词词表依赖。
func tokenize(s string) []string {
	var out []string
	var word []rune
	var cjk []rune
	flushWord := func() {
		if len(word) > 0 {
			out = append(out, strings.ToLower(string(word)))
			word = word[:0]
		}
	}
	flushCJK := func() {
		for i := range cjk {
			out = append(out, string(cjk[i]))
			if i+1 < len(cjk) {
				out = append(out, string(cjk[i:i+2]))
			}
		}
		cjk = cjk[:0]
	}
	for _, r := range s {
		switch {
		case r < unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_'):
			flushCJK()
			word = append(word, r)
		case r >= unicode.MaxASCII:
			flushWord()
			cjk = append(cjk, r)
		default:
			flushWord()
			flushCJK()
		}
	}
	flushWord()
	flushCJK()
	return out
}

// uniqueTokens 去重并保持首次出现顺序（查询词元只需一份）。
func uniqueTokens(toks []string) []string {
	seen := make(map[string]bool, len(toks))
	out := make([]string, 0, len(toks))
	for _, t := range toks {
		if seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}

// extractJSON 从 LLM 输出里截取第一个完整 JSON 值（对象或数组），容忍前后多余文字与 ```json 围栏。
//
// 做**字符串感知**的括号匹配：双引号内的 { } 不计入深度、转义字符正确跳过——否则工具描述或
// 模板引用（`${steps.s1.output.rows}`）里的花括号会把边界算错，截出半个 JSON。
// （与 examples/gear-ai-sidecar/agent.go 的 extractJSON 同款算法。）
func extractJSON(text string) ([]byte, error) {
	start := -1
	var open, closeCh byte
	for i := 0; i < len(text); i++ {
		switch text[i] {
		case '{':
			start, open, closeCh = i, '{', '}'
		case '[':
			if start == -1 {
				start, open, closeCh = i, '[', ']'
			}
		}
		if start != -1 {
			break
		}
	}
	if start == -1 {
		return nil, fmt.Errorf("未找到 JSON")
	}
	depth := 0
	inStr := false
	for i := start; i < len(text); i++ {
		c := text[i]
		if inStr {
			if c == '\\' {
				i++ // 跳过转义序列
				continue
			}
			if c == '"' {
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case open:
			depth++
		case closeCh:
			depth--
			if depth == 0 {
				return []byte(text[start : i+1]), nil
			}
		}
	}
	return nil, fmt.Errorf("JSON 未闭合")
}

// extractJSONToAny 把 extractJSON 的结果反序列化成任意 JSON 值：对象 → map[string]interface{}，
// 数组 → []interface{}/nil，非 JSON → error。
//
// Complete(format=json) 与 generate 链的各节点统一经它解析 LLM 输出——解析失败即视为
// 「模型没按格式给」，由上层回灌提示词重试，而不是静默猜一个值出来。
func extractJSONToAny(text string) (interface{}, error) {
	b, err := extractJSON(text)
	if err != nil {
		return nil, err
	}
	var v interface{}
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, err
	}
	return v, nil
}
