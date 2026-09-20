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

	// 建倒排统计：每篇文档的词元、文档频率 df、总词元数（算平均文档长度）。
	docs := make([][]string, len(pool))
	df := map[string]int{}
	total := 0
	for i, t := range pool {
		toks := tokenize(t.ID + " " + t.Description)
		docs[i] = toks
		total += len(toks)
		seen := map[string]bool{}
		for _, tk := range toks {
			if !seen[tk] {
				seen[tk] = true
				df[tk]++
			}
		}
	}
	terms := uniqueTokens(tokenize(query))
	if len(terms) == 0 || total == 0 {
		out := make([]ToolSummary, k)
		copy(out, pool[:k])
		return out
	}

	avgdl := float64(total) / float64(len(pool))
	const k1, b = 1.2, 0.75
	type scored struct {
		idx   int
		score float64
	}
	scores := make([]scored, len(pool))
	for i := range pool {
		tf := map[string]int{}
		for _, tk := range docs[i] {
			tf[tk]++
		}
		dl := float64(len(docs[i]))
		var s float64
		for _, term := range terms {
			n := df[term]
			if n == 0 {
				continue
			}
			idf := math.Log(1 + (float64(len(pool))-float64(n)+0.5)/(float64(n)+0.5))
			f := float64(tf[term])
			s += idf * (f * (k1 + 1)) / (f + k1*(1-b+b*dl/avgdl))
		}
		scores[i] = scored{idx: i, score: s}
	}
	sort.SliceStable(scores, func(i, j int) bool { return scores[i].score > scores[j].score })

	out := make([]ToolSummary, 0, k)
	for _, e := range scores {
		if len(out) >= k {
			break
		}
		out = append(out, pool[e.idx])
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
