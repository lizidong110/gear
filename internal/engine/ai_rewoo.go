package engine

// 本文件是 REWOO-with-recovery 的 Gear 落地：ext.ai.rewoo（REQ-REWOO）。
// 设计定稿见 .workbuddy/rewoo_agent_a_design.md §8；恢复层复用 ai_recovery.go（react 同款），
// 节点执行路径复用 react 的 buildActionEntry + a.exec（E2E 已验证的那条）。
//
// 与 react 的分工：react 是「边走边看」的逐步循环（每步一次 LLM）；rewoo 是「先画作战图再
// 并行推进」——planner 出 DAG，worker 按拓扑层并行执行（0 LLM），solver 汇总。LLM 调用
// **有界**：planner ≤2（含一次带 LastErr 修正）+ replanner ≤2×replan_budget（同样含修正）
// + solver 1，默认配置最坏 5 次、全部一次过则 3 次——修正次数由校验失败驱动，不凭空多烧
// （2026-10-19 E2E：planner 首轮写 "ctx.http.Request" 带错前缀，正是修正循环接住的）；
// 单点失败 fail-soft（兄弟节点不取消），最终带缺失作答（gaps），不静默残缺。
//
// 节点的调用对象分两类（用户建 agent 的核心诉求）：
//   - 原生能力：「能力.方法」（http.Request / db.query / ext.ai.complete …），走 ctx.* 沙箱通道；
//   - 已注册工具：Tool/Flow id（myapp__search / agent.up …），经 call 原语（ctx.call）调用。
// 两者都走 buildActionEntry 编 Lua 入口，执行通道完全同构。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// ---- 选项与默认值 ----

// RewooOpts 携带一次 rewoo 运行的可选项。零值字段全部有默认值（见 withDefaults）。
type RewooOpts struct {
	MaxNodes     int           //  planner 产出节点数上限（默认 20；超了报错不截断）
	ReplanBudget int           // 失败子图重规划的 LLM 调用上限（默认 1；0 = 显式关闭重规划；负值 = 未设置走默认）
	MaxParallel  int           // 层内并行度（默认 4）
	WallClock    time.Duration // 工具执行总墙钟（默认 5min，ctx 传导，超时中止在飞节点）
	Allow        []string      // 收窄可用调用：能力名 / 「能力.方法」/ 已注册工具 id；空 = 默认集
	AllowSet     bool          // Allow 是否由调用方显式给定（= 该 Tool 自己声明的那份）
	Provider     string        // 模型厂商（可选，覆盖 env）
	Model        string        // 模型名（可选）
	BaseURL      string        // API base（可选）
	APIKey       string        // API key（可选，优先 env）
	Temperature  float64       // 采样温度（可选，默认 0.2——规划要的是确定性不是花样）
}

// rewoo 默认值。都是「便宜的上限」：并行不烧 LLM，但也不能无限。
const (
	rewooMaxNodes      = 20
	rewooReplanBudget  = 1
	rewooMaxParallel   = 4
	rewooWallClock     = 5 * time.Minute
	rewooTemperature   = 0.2
	rewooMaxUserTools  = 40  // planner 提示词里最多列多少个已注册工具（超出注明截断，不静默）
	rewooMaxPromptTool = 120 // 能力面提示词里最多列多少个方法（同上）
)

func (o RewooOpts) withDefaults() RewooOpts {
	if o.MaxNodes <= 0 {
		o.MaxNodes = rewooMaxNodes
	}
	if o.ReplanBudget < 0 {
		// 负值 = 未设置的哨兵。0 是显式关闭重规划（设计 §8.7：预算≤0 → 不 replan 直接落
		// default 档），不能抬回默认——否则「想关关不掉」。
		o.ReplanBudget = rewooReplanBudget
	}
	if o.MaxParallel <= 0 {
		o.MaxParallel = rewooMaxParallel
	}
	if o.WallClock <= 0 {
		o.WallClock = rewooWallClock
	}
	if o.Temperature <= 0 {
		o.Temperature = rewooTemperature
	}
	return o
}

// ---- 计划模型（纯数据，不落注册表） ----

// 节点状态。一次运行内单向流转：pending → done / failed；pending → blocked（上游败）。
const (
	rewooPending = "pending"
	rewooDone    = "done"
	rewooFailed  = "failed"
	rewooBlocked = "blocked"
)

// rewooNode 是 DAG 的一个节点：id 即证据键（#E1），action 是「能力.方法」或已注册工具 id，
// args 里的 "{{#E1}}" 引用上游产出，deps 是直接依赖。运行期字段由 worker 填写。
type rewooNode struct {
	id     string
	action string
	args   map[string]interface{}
	deps   []string

	status   string
	output   map[string]interface{} // exec 原始返回（含 result 键）
	attempts int                    // 已发生的机械重试次数（可观测：重试烧的是 provider 配额）
	empty    bool                   // 成功但查无结果（semantic_empty：收尾按缺口记）
	failure  *reactFailure
}

// rewooPlan 是一张依赖图。nodes 保持 planner 输出序（可读性），byID 供 O(1) 查找。
type rewooPlan struct {
	nodes []*rewooNode
	byID  map[string]*rewooNode
}

// failedIDs 返回终态为失败或被堵的节点 id——重规划的作用域种子（反可达集的根）。
func (p *rewooPlan) failedIDs() []string {
	out := []string{}
	for _, n := range p.nodes {
		if n.status == rewooFailed || n.status == rewooBlocked {
			out = append(out, n.id)
		}
	}
	return out
}

// rewooLevels 做 Kahn 分层：同层节点互不依赖，可并行。调用方保证无环（validateRewooPlan 已拦）。
func rewooLevels(p *rewooPlan) [][]*rewooNode {
	indeg := make(map[string]int, len(p.nodes))
	for _, n := range p.nodes {
		indeg[n.id] = len(n.deps)
	}
	var levels [][]*rewooNode
	done := map[string]bool{}
	for len(done) < len(p.nodes) {
		var level []*rewooNode
		for _, n := range p.nodes {
			if done[n.id] || indeg[n.id] > 0 {
				continue
			}
			level = append(level, n)
		}
		if len(level) == 0 {
			break // 有环（理论上已被 validate 拦死）；防御性退出，不死循环
		}
		for _, n := range level {
			done[n.id] = true
		}
		// 递减只对「本层节点的直接消费者」做：按反邻接算一次即可
		for _, n := range level {
			for _, m := range p.nodes {
				for _, d := range m.deps {
					if d == n.id {
						indeg[m.id]--
					}
				}
			}
		}
		levels = append(levels, level)
	}
	return levels
}

// rewooReverseReachable 计算种子节点的反可达集：它们自己 + 所有传递依赖它们的下游。
// 这就是重规划的作用域（design §3.1）——只重生真受影响的节点，不误伤无关分支。
func rewooReverseReachable(p *rewooPlan, seeds []string) map[string]bool {
	in := map[string]bool{}
	for _, s := range seeds {
		in[s] = true
	}
	for changed := true; changed; {
		changed = false
		for _, n := range p.nodes {
			if in[n.id] {
				continue
			}
			for _, d := range n.deps {
				if in[d] {
					in[n.id] = true
					changed = true
					break
				}
			}
		}
	}
	return in
}

// rewooSignature 给一组节点算结构签名（id|action|args 排序拼接）——plan-equivalence
// 震荡守卫的判据：重规划产出与当前子图签名相同，说明「再烧一次 replanner」不会不一样。
func rewooSignature(p *rewooPlan, ids map[string]bool) string {
	parts := make([]string, 0, len(ids))
	for _, n := range p.nodes {
		if ids[n.id] {
			parts = append(parts, n.id+"|"+n.action+"|"+stableJSON(n.args))
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, "\n")
}

// ---- 动作解析：原生能力 or 已注册工具 ----

type rewooActionKind int

const (
	rewooActionCapability rewooActionKind = iota // 「能力.方法」
	rewooActionTool                              // 已注册 Tool/Flow id（经 call 原语）
)

// resolveRewooAction 把一个 action 字符串解析成可执行形态。
// 顺序即口径：先当「能力.方法」试（http.Request / ext.ai.complete / db.query …），
// 再当已注册工具/Flow id 试（agent.up 这类带点号的 id 因能力判定失败而落到这里）。
// ⚠ GetTool/GetFlow 对不存在的 id 返回 (nil, nil) 而不是错误——必须判工具非 nil，
// 否则任何一个乱写的 action 都会被当成「已注册工具」放进执行通道。
func (a *AIAgent) resolveRewooAction(action string) (rewooActionKind, string, string, error) {
	if action == "" {
		return 0, "", "", fmt.Errorf("action 不能为空")
	}
	// ctx. 前缀别名容错：模型在 Lua 语料上预训练，很自然会写 "ctx.http.Request"——
	// 这是无歧义的目标同一指代（ctx.<能力>.<方法> ≡ <能力>.<方法>），剥掉前缀再解析。
	// 工具 id 从不以 ctx. 开头，剥落不会误伤已注册工具（2026-10-19 E2E：两轮修正
	// 都扳不回来这个前缀，容错比惩罚便宜）。
	action = strings.TrimPrefix(action, "ctx.")
	if tool, method := splitToolMethod(action, ""); a.knownMethod(tool, method) {
		return rewooActionCapability, tool, method, nil
	}
	if t, err := a.reg.GetTool(action); err == nil && t != nil {
		return rewooActionTool, action, "", nil
	}
	if f, err := a.reg.GetFlow(action); err == nil && f != nil {
		return rewooActionTool, action, "", nil
	}
	return 0, "", "", fmt.Errorf("action %q 既不是已知的「能力.方法」，也不是已注册的 Tool/Flow id", action)
}

// rewooAllow 把「rewoo 节点能调什么」变成显式值：原生能力走 react 同款 reactAllow
// （能力边界跟着调用方声明走），已注册工具单独一份名单（nil = 未收窄时全部已注册）。
type rewooAllow struct {
	caps     reactAllow
	tools    map[string]bool // 显式收窄时的工具 id 白名单
	toolsAll bool            // true = 全部已注册工具都允许（未收窄或 "*"）
}

// resolveRewooAllow 解析允许集。names 里能力名/方法名进 caps，工具 id 进 tools；
// "*" 两者全放。未知项显式报错（strict）——静默丢弃会让 planner 以为能用结果执行期才炸。
func (a *AIAgent) resolveRewooAllow(names []string, set bool) (*rewooAllow, error) {
	al := &rewooAllow{}
	if !set {
		caps := make([]string, 0, len(defaultReactCapabilities))
		for _, c := range defaultReactCapabilities {
			if a.knownCapability(c) {
				caps = append(caps, c)
			}
		}
		al.caps = reactAllow{names: caps}
		al.toolsAll = true // 未收窄：已注册工具全部可见（与 generate 的 loadTools 口径一致）
		return al, nil
	}
	caps, tools := []string{}, map[string]bool{}
	for _, raw := range names {
		x := strings.TrimSpace(raw)
		if x == "" {
			continue
		}
		if x == ReactAllowAll {
			al.caps = reactAllow{all: true}
			al.toolsAll = true
			return al, nil
		}
		if a.knownCapability(x) {
			caps = append(caps, x)
			continue
		}
		if c, m, ok := splitCapMethod(x); ok && a.knownMethod(c, m) {
			caps = append(caps, x)
			continue
		}
		if t, err := a.reg.GetTool(x); err == nil && t != nil {
			tools[x] = true
			continue
		}
		if f, err := a.reg.GetFlow(x); err == nil && f != nil {
			tools[x] = true
			continue
		}
		return nil, fmt.Errorf("rewoo 的 allow 含 %q：既不是已知能力/「能力.方法」，也不是已注册的 Tool/Flow id；"+
			"可用 gear ai capabilities 或 ext.ai.capabilities() 看目录", x)
	}
	rl, err := a.resolveReactAllow(caps, true, true)
	if err != nil {
		return nil, err
	}
	al.caps = rl
	al.tools = tools
	al.toolsAll = len(tools) == 0 // 只给了能力没给工具 → 工具面不收窄（保持默认全量）
	return al, nil
}

func (al *rewooAllow) toolAllows(id string) bool {
	if al.toolsAll {
		return true
	}
	return al.tools[id]
}

// allowsAction 是节点执行前的守门：解析 action 并核对允许集。通过则返回可执行形态。
func (a *AIAgent) allowsRewooAction(al *rewooAllow, action string) (rewooActionKind, string, string, error) {
	kind, tool, method, err := a.resolveRewooAction(action)
	if err != nil {
		return 0, "", "", err
	}
	if kind == rewooActionCapability && !al.caps.allows(tool, method) {
		return 0, "", "", fmt.Errorf("调用 %q 不在允许列表 [%s] 内", action, al.caps.String())
	}
	if kind == rewooActionTool && !al.toolAllows(action) {
		return 0, "", "", fmt.Errorf("工具 %q 不在允许的工具名单内", action)
	}
	return kind, tool, method, nil
}

// ---- {{#E}} 引用解析 ----

// resolveRewooRefs 把 args 里的 "{{#E1}}" 引用替换成 evidence 里的真实产出。
// 规则刻意窄（少一层魔法）：整个字符串值恰是 "{{#E1}}" → 原样替换（保留类型）；
// 嵌在更长字符串里 → 替换为 textual 渲染。引用支持字段路径 "{{#E2.src}}"（模型很自然会
// 这么写，禁止它只会换来执行期怪异失败）。引用缺失即报错——让 planner/校验层早发现，
// 而不是把 "{{#E1}}" 当普通字符串传进工具。
func resolveRewooRefs(v interface{}, ev map[string]interface{}) (interface{}, error) {
	switch x := v.(type) {
	case map[string]interface{}:
		out := make(map[string]interface{}, len(x))
		for k, val := range x {
			r, err := resolveRewooRefs(val, ev)
			if err != nil {
				return nil, err
			}
			out[k] = r
		}
		return out, nil
	case []interface{}:
		out := make([]interface{}, len(x))
		for i, val := range x {
			r, err := resolveRewooRefs(val, ev)
			if err != nil {
				return nil, err
			}
			out[i] = r
		}
		return out, nil
	case string:
		return resolveRewooRef(x, ev)
	default:
		return v, nil
	}
}

func resolveRewooRef(s string, ev map[string]interface{}) (interface{}, error) {
	refs := rewooRefTargets(s)
	if len(refs) == 0 {
		return s, nil
	}
	if len(refs) == 1 && s == "{{"+refs[0]+"}}" {
		val, ok := rewooRefLookup(ev, refs[0])
		if !ok {
			return nil, fmt.Errorf("引用 %s 没有可用产出（上游未成功或尚未执行）", refs[0])
		}
		return val, nil
	}
	out := s
	for _, r := range refs {
		val, ok := rewooRefLookup(ev, r)
		if !ok {
			return nil, fmt.Errorf("引用 %s 没有可用产出（上游未成功或尚未执行）", r)
		}
		out = strings.ReplaceAll(out, "{{"+r+"}}", rewooRefText(val))
	}
	return out, nil
}

// rewooRefSplit 把引用目标拆成「节点 id + 字段路径」："#E2.src" → ("#E2", "src")；
// "#E1" → ("#E1", "")。字段路径按点号逐级下钻（只走 map）。
func rewooRefSplit(inner string) (id, path string) {
	if i := strings.Index(inner, "."); i > 0 {
		return inner[:i], inner[i+1:]
	}
	return inner, ""
}

// rewooRefLookup 按「节点 id[.字段路径]」取产出。任一层缺失即 not ok——
// 调用方据此报「引用没有可用产出」，不静默填空。
func rewooRefLookup(ev map[string]interface{}, inner string) (interface{}, bool) {
	id, path := rewooRefSplit(inner)
	v, ok := ev[id]
	if !ok {
		return nil, false
	}
	if path == "" {
		return v, true
	}
	cur := v
	for _, seg := range strings.Split(path, ".") {
		next, ok := rewooDrill(cur, seg)
		if !ok {
			return nil, false
		}
		cur = next
	}
	return cur, true
}

// rewooDrill 在 cur 里取一段字段。三层回退，全部确定性（不猜意图，只认形状）：
//  1. cur 是 map → 直接取键（信封键 status/body/headers 优先）；
//  2. cur 是 JSON 文本 → 解析成对象再取（body 是 JSON 字符串的链路，如 {{#E1.body.origin}}）；
//  3. cur 是 HTTP 信封 {status, body, headers} 且直接键没有 → 钻 body 里的同名字段——
//     模型会自然写 {{#E1.origin}}（数据确实在 body 里），禁止它只会换来执行期怪异失败
//     （2026-10-19 E2E 实测：planner 写 {{#E1.origin}} 撞「没有可用产出」整节点失败）。
func rewooDrill(cur interface{}, seg string) (interface{}, bool) {
	m, ok := cur.(map[string]interface{})
	if !ok {
		if s, isStr := cur.(string); isStr {
			if parsed, perr := rewooParseJSONObject(s); perr == nil {
				m, ok = parsed, true
			}
		}
		if !ok {
			return nil, false
		}
	}
	if v, exists := m[seg]; exists {
		return v, true
	}
	if body, hasBody := m["body"]; hasBody {
		if _, hasStatus := m["status"]; hasStatus { // 认 HTTP 信封形状，不认任意带 body 的 map
			if bs, isStr := body.(string); isStr {
				if parsed, perr := rewooParseJSONObject(bs); perr == nil {
					if v, exists := parsed[seg]; exists {
						return v, true
					}
				}
			}
		}
	}
	return nil, false
}

// rewooParseJSONObject 把 JSON 文本解成对象；非对象 / 坏 JSON 即错（调用方按「钻不动」处理）。
func rewooParseJSONObject(s string) (map[string]interface{}, error) {
	t := strings.TrimSpace(s)
	if !strings.HasPrefix(t, "{") {
		return nil, fmt.Errorf("不是 JSON 对象")
	}
	dec := json.NewDecoder(strings.NewReader(t))
	dec.UseNumber() // 大整数精度：float64 会把 >2^53 的 id 末位抹掉
	var m map[string]interface{}
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	return m, nil
}

// rewooRefTargets 从一个字符串里挖出全部引用目标（含字段路径，按出现序，可重复）。
func rewooRefTargets(s string) []string {
	var out []string
	rest := s
	for {
		i := strings.Index(rest, "{{")
		if i < 0 {
			return out
		}
		j := strings.Index(rest[i:], "}}")
		if j < 0 {
			return out
		}
		inner := rest[i+2 : i+j]
		if strings.HasPrefix(inner, "#E") {
			out = append(out, inner)
		}
		rest = rest[i+j+2:]
	}
}

func rewooRefText(v interface{}) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	default:
		return stableJSON(x)
	}
}

// ---- planner（1 LLM：生成 → 校验 → 一次修正） ----

// rewooPlannerPrompt 拼装规划提示词：任务 + 两类调用面 + 输出契约。
// 工具面与 react 同源（能力目录 + 已注册工具），节点规则写死「编号更小才可依赖」——
// 无环由编号次序保证，不靠模型自觉。
func (a *AIAgent) rewooPlannerPrompt(task string, al *rewooAllow, opts RewooOpts, lastErr string) string {
	var b strings.Builder
	b.WriteString("你是任务规划器（planner）。把任务拆成一张依赖图（DAG），每个节点是一次可执行的调用，由执行器并行运行。\n\n")
	b.WriteString("【可用调用】\n原生能力（action 写成「能力.方法」，args 是该方法的参数 map）：\n")
	b.WriteString(a.rewooCapabilityText(al))
	b.WriteString("已注册工具（action 直接写工具 id，args 是该工具的输入 map）：\n")
	b.WriteString(a.rewooUserToolText(al))
	b.WriteString("\n【输出契约】\n只输出一个 JSON 对象，不要 Markdown 围栏、不要解释：\n")
	b.WriteString(`{"steps":[{"id":"#E1","action":"http.Request","args":{"method":"GET","url":"..."},"depends_on":[]}, ...]}` + "\n")
	b.WriteString("规则：\n")
	b.WriteString("1. id 从 \"#E1\" 起递增编号；depends_on 只能引用编号更小的节点（保证无环）；action 原样写上面列出的「能力.方法」（如 http.Request，**不要带 ctx. 前缀**）或已注册工具 id；\n")
	b.WriteString("2. 参数里可引用上游产出：整个值写 \"{{#E1}}\"（保留原类型），嵌在字符串里则转文本；也可带字段路径 \"{{#E2.src}}\"；\n")
	b.WriteString("3. 只要引用了某节点的产出，就必须把它列进 depends_on（直接或间接）；\n")
	b.WriteString(fmt.Sprintf("4. 步骤数 ≤ %d，按「取数 → 加工 → 汇聚」的粒度拆，不要拆到字符串拼接；\n", opts.MaxNodes))
	b.WriteString("5. 只能使用上面列出的调用；任务不需要的数据不要硬凑步骤。\n")
	if lastErr != "" {
		b.WriteString("\n【上次输出未通过校验，请修正后重新输出】\n" + lastErr + "\n")
	}
	b.WriteString("\n【任务】\n" + task + "\n")
	return b.String()
}

// rewooCapabilityText 渲染原生能力面（与 react 的 reactToolsText 同源，收窄由 allow 决定）。
// 渲染形态是**调用目标**（「能力.方法」，不带 ctx. 前缀）——与 flowCapabilitiesText 给
// Flow call 用的形态同口径：rewoo 的 action 是目标标识不是 Lua 表达式。2026-10-19 E2E
// 实测：用 callSignature 的 ctx.* 形态教会模型写 "ctx.http.Request"，两轮 LastErr 修正
// 都扳不回来——提示词教错写法，不能怪模型抄。
func (a *AIAgent) rewooCapabilityText(al *rewooAllow) string {
	var b strings.Builder
	n := 0
	emit := func(capName string, methods map[string]MethodSchema) {
		names := make([]string, 0, len(methods))
		for m := range methods {
			names = append(names, m)
		}
		sort.Strings(names)
		for _, m := range names {
			if !al.caps.allows(capName, m) {
				continue
			}
			if n >= rewooMaxPromptTool {
				fmt.Fprintf(&b, "- …（共超 %d 个方法，已截断；请用 allow 收窄）\n", rewooMaxPromptTool)
				return
			}
			n++
			sch := methods[m]
			fmt.Fprintf(&b, "- %s.%s(%s)：%s\n", capName, m, paramList(sch.Parameters), sch.Description)
		}
	}
	for _, capName := range sortedCapNames(builtinCapabilitySchemas) {
		emit(capName, builtinCapabilitySchemas[capName])
	}
	for _, capName := range sortedCapNames(a.extCaps) {
		emit(capName, a.extCaps[capName])
	}
	if n == 0 {
		b.WriteString("- （无：允许集为空，一个调用都不给）\n")
	}
	return b.String()
}

// rewooUserToolText 渲染已注册工具面（用户自建 agent 的第二个调用对象来源）。
func (a *AIAgent) rewooUserToolText(al *rewooAllow) string {
	var b strings.Builder
	n := 0
	emitTool := func(id, desc string, in map[string]interface{}) {
		if !al.toolAllows(id) {
			return
		}
		if n >= rewooMaxUserTools {
			fmt.Fprintf(&b, "- …（已注册工具超 %d 个，已截断）\n", rewooMaxUserTools)
			return
		}
		n++
		// 空描述的工具对模型是黑盒：它只能按 id 猜用途。2026-10-20 实测 ext.pdf
		// （name/description 全空）被 planner 选中去「提取 PDF」，而它其实只会
		// 写 PDF——猜错方向一半责任在目录没说话。显式标注，让模型知道这是
		// 无信息项，也提醒用户去补定义。
		if strings.TrimSpace(desc) == "" {
			desc = "（注册时未写描述，用途未知，谨慎选用）"
		}
		fmt.Fprintf(&b, "- %s：%s（input: %s）\n", id, desc, rewooInputBrief(in))
	}
	if tools, err := a.reg.ListTools(); err == nil {
		for _, t := range tools {
			emitTool(t.ID, t.Description, t.InputSchema)
		}
	}
	if flows, err := a.reg.ListFlows(); err == nil {
		for _, f := range flows {
			emitTool(f.ID, f.Description, nil)
		}
	}
	if n == 0 {
		b.WriteString("- （无：没有允许调用的已注册工具）\n")
	}
	return b.String()
}

// rewooInputBrief 把 input schema 压成一行（属性名: 类型），控制提示词体积。
func rewooInputBrief(in map[string]interface{}) string {
	props, ok := in["properties"].(map[string]interface{})
	if !ok || len(props) == 0 {
		return "{}"
	}
	keys := make([]string, 0, len(props))
	for k := range props {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		t := ""
		if pm, ok := props[k].(map[string]interface{}); ok {
			t, _ = pm["type"].(string)
		}
		if t == "" {
			t = "any"
		}
		parts = append(parts, k+": "+t)
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// rewooPlan_ 跑一次 planner LLM 并解析校验。不过则带 LastErr 修正一次（复用 generate 的思路）；
// 再不过返回错误——不猜，猜出来的 DAG 执行期全是诡异失败。
func (a *AIAgent) rewooPlanner(ctx context.Context, task string, al *rewooAllow, opts RewooOpts, board *reactBoard) (*rewooPlan, error) {
	lastErr := ""
	for attempt := 0; attempt < 2; attempt++ {
		prompt := a.rewooPlannerPrompt(task, al, opts, lastErr)
		text, _, _, err := a.completeReactStep(ctx, "rewoo planner", LLMRequest{
			Messages:    []map[string]any{{"role": "user", "content": prompt}},
			Provider:    opts.Provider,
			Model:       opts.Model,
			BaseURL:     opts.BaseURL,
			APIKey:      opts.APIKey,
			Temperature: opts.Temperature,
		}, nil, board, false)
		if err != nil {
			return nil, err
		}
		plan, perr := parseRewooPlan(text)
		if perr == nil {
			perr = a.validateRewooPlan(plan, al, opts)
		}
		if perr == nil {
			return plan, nil
		}
		lastErr = perr.Error()
		board.recordRecovery(fmt.Sprintf("planner 输出未通过校验（第 %d 次）：%s", attempt+1, lastErr))
	}
	return nil, fmt.Errorf("planner 两次输出均未通过校验：%s", lastErr)
}

// parseRewooPlan 解析 planner 的 JSON 输出。只认 {"steps":[...]}；缺字段按空值补，
// 类型不对由 validate 报错（解析层不猜意图）。
func parseRewooPlan(text string) (*rewooPlan, error) {
	data, err := extractJSONToAny(text)
	if err != nil {
		return nil, fmt.Errorf("输出不是合法 JSON: %w", err)
	}
	root, ok := data.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("输出应是 JSON 对象 {\"steps\":[...]}")
	}
	raw, ok := root["steps"].([]interface{})
	if !ok {
		return nil, fmt.Errorf("输出缺少 steps 数组")
	}
	plan := &rewooPlan{byID: map[string]*rewooNode{}}
	for i, item := range raw {
		m, ok := item.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("steps[%d] 不是对象", i)
		}
		n := &rewooNode{
			id:     strings.TrimSpace(strOr(m, "id")),
			action: strings.TrimSpace(strOr(m, "action")),
			args:   map[string]interface{}{},
			status: rewooPending,
		}
		if a, ok := m["args"].(map[string]interface{}); ok {
			n.args = a
		}
		n.deps = toStringSlice(m["depends_on"])
		plan.nodes = append(plan.nodes, n)
	}
	return plan, nil
}

// validateRewooPlan 是 planner 输出的 Go 侧校验（不靠模型自觉）：
// 数量 / id 形态唯一 / action 可解析且被允许 / deps 存在且无环 / 引用 ⊆ 传递依赖。
// 判重用的是局部 seen 而不是 p.byID——merge 场景里 p.byID 已被缝合方预填，
// 拿它判重会把每个节点都误判成「重复」（2026-10-19 实测踩坑）。
func (a *AIAgent) validateRewooPlan(p *rewooPlan, al *rewooAllow, opts RewooOpts) error {
	if len(p.nodes) == 0 {
		return fmt.Errorf("steps 不能为空")
	}
	if len(p.nodes) > opts.MaxNodes {
		return fmt.Errorf("步骤数 %d 超过上限 %d（请合并同类步骤）", len(p.nodes), opts.MaxNodes)
	}
	byID := make(map[string]*rewooNode, len(p.nodes))
	for _, n := range p.nodes {
		if !rewooValidID(n.id) {
			return fmt.Errorf("节点 id %q 不合法（应从 #E1 起编号）", n.id)
		}
		if _, dup := byID[n.id]; dup {
			return fmt.Errorf("节点 id %q 重复", n.id)
		}
		byID[n.id] = n
		if n.action == "" {
			return fmt.Errorf("节点 %s 缺少 action", n.id)
		}
		for _, d := range n.deps {
			if d == n.id {
				return fmt.Errorf("节点 %s 依赖自己", n.id)
			}
		}
		if _, _, _, err := a.allowsRewooAction(al, n.action); err != nil {
			return fmt.Errorf("节点 %s：%v", n.id, err)
		}
		// 规范成 canonical 形态（剥掉 ctx. 前缀）：计划快照要可审计可重放，
		// 不能留着模型抄错的写法让下游 diff 时对不上（resolveRewooAction 的别名容错
		// 保证执行一致，这里只负责把数据擦干净）。
		if canonical := strings.TrimPrefix(n.action, "ctx."); canonical != n.action {
			n.action = canonical
		}
	}
	p.byID = byID
	// 存在性 + 无环（Kahn）
	indeg := map[string]int{}
	for _, n := range p.nodes {
		indeg[n.id] = len(n.deps)
		for _, d := range n.deps {
			if _, ok := byID[d]; !ok {
				return fmt.Errorf("节点 %s 依赖不存在的 %s", n.id, d)
			}
		}
	}
	done := 0
	queue := []string{}
	for _, n := range p.nodes {
		if indeg[n.id] == 0 {
			queue = append(queue, n.id)
		}
	}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		done++
		for _, n := range p.nodes {
			for _, d := range n.deps {
				if d == cur {
					indeg[n.id]--
					if indeg[n.id] == 0 {
						queue = append(queue, n.id)
					}
				}
			}
		}
	}
	if done != len(p.nodes) {
		return fmt.Errorf("依赖图有环（depends_on 形成了循环依赖）")
	}
	// 引用 ⊆ 传递依赖：用了 #E2 的产出就必须（间接）依赖 #E2
	closure := rewooDepClosure(p)
	for _, n := range p.nodes {
		for _, ref := range rewooArgsRefs(n.args) {
			if ref == n.id || !closure[n.id][ref] {
				return fmt.Errorf("节点 %s 引用了 %s 的产出，但没有把它列进 depends_on", n.id, ref)
			}
		}
	}
	return nil
}

// rewooValidID 校验 #E 编号形态（#E1、#E2…；不接受 #E0 与非数字）。
func rewooValidID(id string) bool {
	if !strings.HasPrefix(id, "#E") || len(id) <= 2 {
		return false
	}
	for _, c := range id[2:] {
		if c < '0' || c > '9' {
			return false
		}
	}
	return id != "#E0"
}

// rewooDepClosure 计算每个节点的传递依赖集合（供引用校验用）。
func rewooDepClosure(p *rewooPlan) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	var visit func(id string) map[string]bool
	visit = func(id string) map[string]bool {
		if c, ok := out[id]; ok {
			return c
		}
		c := map[string]bool{}
		out[id] = c // 先占位：无环已保证不会递归死循环
		n := p.byID[id]
		if n != nil {
			for _, d := range n.deps {
				c[d] = true
				for k := range visit(d) {
					c[k] = true
				}
			}
		}
		return c
	}
	for _, n := range p.nodes {
		visit(n.id)
	}
	return out
}

// rewooArgsRefs 收集 args 里全部引用目标（折算成节点 id：字段路径在依赖校验里不适用，
// "#E2.src" 与 "#E2" 依赖的是同一个节点）。
func rewooArgsRefs(v interface{}) []string {
	var out []string
	var walk func(x interface{})
	walk = func(x interface{}) {
		switch t := x.(type) {
		case map[string]interface{}:
			for _, val := range t {
				walk(val)
			}
		case []interface{}:
			for _, val := range t {
				walk(val)
			}
		case string:
			for _, inner := range rewooRefTargets(t) {
				id, _ := rewooRefSplit(inner)
				out = append(out, id)
			}
		}
	}
	walk(v)
	return out
}

// ---- worker（0 LLM：拓扑分层并行，fail-soft） ----

// rewooWork 跑一张计划里所有 pending 节点：按层并行（信号量限流），每节点走 react 同款
// 执行路径（allow 守卫 → 引用解析 → buildActionEntry → exec → L0 重试 → 分类）。
//
// 与引擎 execLevel 的**刻意分歧**：引擎 fail-closed（首错 cancel 整层）是 Flow 语义；
// rewoo 要带缺失作答——单节点失败只记 failure + 哨兵证据，兄弟节点照常跑，下游依赖失败
// 节点的标记 upstream_bad 不执行。任何失败都有记录（noteFailure），绝不静默。
func (a *AIAgent) rewooWork(ctx context.Context, p *rewooPlan, board *reactBoard, al *rewooAllow, opts RewooOpts, ev map[string]interface{}, evMu *sync.Mutex) {
	for _, level := range rewooLevels(p) {
		if ctx.Err() != nil {
			return
		}
		sem := make(chan struct{}, opts.MaxParallel)
		var wg sync.WaitGroup
		for _, n := range level {
			if n.status != rewooPending {
				continue // 已成功节点的证据直接复用（重规划后只跑新节点）
			}
			// 上游败/被堵 → 本节点记 upstream_bad，不执行（不在坏输入上浪费一次调用）
			var bad []string
			for _, d := range n.deps {
				if dn := p.byID[d]; dn != nil && (dn.status == rewooFailed || dn.status == rewooBlocked) {
					bad = append(bad, d)
				}
			}
			if len(bad) > 0 {
				n.status = rewooBlocked
				f := reactFailure{
					Class: FailUpstreamBad, Action: n.action, GapKey: n.id + "（" + n.action + "）",
					Reason: fmt.Sprintf("上游节点 %s 失败，本节点未执行", strings.Join(bad, ", ")), Scope: "upstream",
				}
				n.failure = &f
				board.noteFailure(f)
				board.noteEvidence(n.action, false)
				continue
			}
			wg.Add(1)
			sem <- struct{}{}
			go func(n *rewooNode) {
				defer wg.Done()
				defer func() { <-sem }()
				a.rewooExecNode(ctx, n, ev, evMu, board, al)
			}(n)
		}
		wg.Wait()
	}
}

// rewooExecNode 执行单个节点：L0 机械重试（transient 与 body 内可重试 HTTP 状态码，
// 不烧 LLM、与模型重发无关）→ 空结果记 semantic_empty → 失败分类登记。
func (a *AIAgent) rewooExecNode(ctx context.Context, n *rewooNode, ev map[string]interface{}, evMu *sync.Mutex, board *reactBoard, al *rewooAllow) {
	// 震荡封禁：同动作连续失败达阈值后，后续同动作节点直接失败（不执行、不烧配额）
	if why, ok := board.blockedNotice(n.action); ok {
		n.status = rewooFailed
		f := reactFailure{Class: FailUnrecoverable, Action: n.action, GapKey: n.id + "（" + n.action + "）",
			Reason: "震荡守卫已停止该动作：" + why, Scope: "self"}
		n.failure = &f
		board.noteFailure(f)
		board.noteEvidence(n.action, false)
		return
	}
	// 引用解析（ev 快照读：并行写下的浅拷贝，节点产出只写一次不改写，安全）
	evMu.Lock()
	snap := make(map[string]interface{}, len(ev))
	for k, v := range ev {
		snap[k] = v
	}
	evMu.Unlock()
	args, err := resolveRewooRefs(n.args, snap)
	if err != nil {
		a.rewooFailNode(n, board, reactFailure{Class: FailBadInput, Action: n.action,
			Reason: "引用解析失败：" + err.Error(), Scope: "self"})
		return
	}
	argsMap, _ := args.(map[string]interface{})
	if argsMap == nil {
		argsMap = map[string]interface{}{}
	}
	kind, tool, method, err := a.allowsRewooAction(al, n.action)
	if err != nil {
		a.rewooFailNode(n, board, reactFailure{Class: FailBadInput, Action: n.action,
			Reason: err.Error(), Scope: "self"})
		return
	}
	var entry string
	if kind == rewooActionCapability {
		entry, err = buildActionEntry(tool, method, argsMap)
	} else {
		// 用户工具：经 call 原语（ctx.call）——与 Flow step 调 Tool 同一条通道
		entry, err = buildActionEntry("call", "Call", map[string]interface{}{"target": n.action, "input": argsMap})
	}
	if err != nil {
		a.rewooFailNode(n, board, reactFailure{Class: FailBadInput, Action: n.action,
			Reason: "构建调用入口失败：" + err.Error(), Scope: "self"})
		return
	}

	attempts := 0
	for {
		out, err := a.exec(ctx, entry, map[string]interface{}{})
		if err == nil {
			// ctx.http.Request 不把非 2xx 当错误（status 在 body 里）：可重试状态码要走 L0，
			// 否则 HTTP 类工具的重试通道等于不存在。
			if code, retry := retryableHTTPStatus(out); retry {
				f := classifyToolFailure(n.action, fmt.Errorf("HTTP %d %s", code, httpStatusText(code)))
				if failureRecovery[f.Class].retry && attempts < maxToolRetries && board.spendRetry() {
					attempts++
					board.recordRecovery(fmt.Sprintf("L0 重试 %s（%s，第 %d 次）：HTTP %d", n.action, f.Class, attempts, code))
					sleepBackoff(a.retryBackoff, attempts-1)
					continue
				}
				a.rewooFailNode(n, board, reactFailure{Class: f.Class, Action: n.action,
					Reason: fmt.Sprintf("HTTP %d %s", code, httpStatusText(code)), Scope: board.scopeHint(), Attempts: attempts})
				return
			}
			if isEmptyToolResult(out) {
				// 空结果不是错误（确实没数据就是正确答案）：节点完成、证据即空产出，
				// 只登记 semantic_empty 供震荡计数与缺口收尾。
				n.status = rewooDone
				n.output = out
				n.empty = true
				n.attempts = attempts
				evMu.Lock()
				ev[n.id] = rewooEvidence(out)
				evMu.Unlock()
				board.noteFailure(reactFailure{Class: FailSemanticEmpty, Action: n.action,
					Reason: "工具返回空结果", Scope: board.scopeHint()})
				board.noteEvidence(n.action, false)
				return
			}
			n.status = rewooDone
			n.output = out
			n.attempts = attempts
			evMu.Lock()
			ev[n.id] = rewooEvidence(out)
			evMu.Unlock()
			board.noteEvidence(n.action, true)
			return
		}
		f := classifyToolFailure(n.action, err)
		if failureRecovery[f.Class].retry && attempts < maxToolRetries && board.spendRetry() {
			attempts++
			board.recordRecovery(fmt.Sprintf("L0 重试 %s（%s，第 %d 次）：%v", n.action, f.Class, attempts, err))
			sleepBackoff(a.retryBackoff, attempts-1)
			continue
		}
		n.attempts = attempts
		a.rewooFailNode(n, board, reactFailure{Class: f.Class, Action: n.action,
			Reason: err.Error(), Scope: board.scopeHint(), Attempts: attempts})
		return
	}
}

// rewooFailNode 标记节点失败并登记：哨兵证据进 ev（solver 与缺口有据可查），
// GapKey 用节点 id（同一动作可被多节点使用，动作级键会互相抵消）。
func (a *AIAgent) rewooFailNode(n *rewooNode, board *reactBoard, f reactFailure) {
	n.status = rewooFailed
	if f.GapKey == "" {
		f.GapKey = n.id + "（" + n.action + "）"
	}
	f.Action = n.action
	n.failure = &f
	board.noteFailure(f)
	board.noteEvidence(n.action, false)
}

// rewooEvidence 取节点产出的「结果」部分（与 react 的 observation 口径一致）。
func rewooEvidence(out map[string]interface{}) interface{} {
	if r, ok := out["result"]; ok {
		return r
	}
	return out
}

// httpStatusText 给可重试状态码一个人话文案——分类器按文案匹配（"bad gateway"/"service
// unavailable"/"gateway timeout"），光有数字 "HTTP 502" 匹配不上 transient 关键词。
func httpStatusText(code int) string {
	switch code {
	case 429:
		return "too many requests"
	case 502:
		return "bad gateway"
	case 503:
		return "service unavailable"
	case 504:
		return "gateway timeout"
	default:
		return fmt.Sprintf("status %d", code)
	}
}

// ---- replanner（≤1 LLM：frozen-boundary 重规划失败子图） ----

// rewooReplanPrompt 拼装重规划提示词：失败信号 + 当前子图 + 冻结契约 + 可复用产出。
// **可用调用段必须带上**（2026-10-19 E2E 实测教训）：replanner 与 planner 是无状态的两
// 次独立 LLM 调用，模型不会「记得」 planner 那一轮看到的目录——第一次落地漏了这段，
// 真实模型两轮各幻觉一个不存在的能力（code.run / json_parse），重规划全程无效。
func (a *AIAgent) rewooReplanPrompt(task string, p *rewooPlan, scope map[string]bool, boundary []string, kept map[string]interface{}, evMu *sync.Mutex, al *rewooAllow) string {
	var b strings.Builder
	b.WriteString("你是重规划器（replanner）。执行器并行跑完后，下图中一部分节点失败了。请**只对失败子图重新规划**，不要改动已成功的部分。\n\n")
	b.WriteString("【可用调用】\n原生能力（action 写成「能力.方法」，args 是该方法的参数 map）：\n")
	b.WriteString(a.rewooCapabilityText(al))
	b.WriteString("已注册工具（action 直接写工具 id，args 是该工具的输入 map）：\n")
	b.WriteString(a.rewooUserToolText(al))
	b.WriteString("\n【任务】\n" + task + "\n\n")
	b.WriteString("【失败信号】（类型 / 范围 / 原因）\n")
	for _, n := range p.nodes {
		if !scope[n.id] || n.failure == nil {
			continue
		}
		fmt.Fprintf(&b, "- %s %s：类型=%s 范围=%s 原因=%s\n", n.id, n.action, n.failure.Class, n.failure.Scope, n.failure.Reason)
	}
	b.WriteString("\n【当前子图定义】（需重新规划的节点）\n")
	for _, n := range p.nodes {
		if !scope[n.id] {
			continue
		}
		fmt.Fprintf(&b, "- %s action=%s args=%s deps=%v\n", n.id, n.action, stableJSON(n.args), n.deps)
	}
	b.WriteString("\n【冻结契约】以下节点有下游消费者，必须用**同样的 id 与同样的产出形态**重新给出：" + strings.Join(boundary, ", ") + "\n")
	b.WriteString("\n【可复用的上游产出】（已成功节点的证据，新节点可引用）\n")
	evMu.Lock()
	keys := make([]string, 0, len(kept))
	for k := range kept {
		keys = append(keys, k)
	}
	evMu.Unlock()
	sort.Strings(keys)
	if len(keys) == 0 {
		b.WriteString("（无）\n")
	}
	for _, k := range keys {
		evMu.Lock()
		v := kept[k]
		evMu.Unlock()
		fmt.Fprintf(&b, "- %s → %s\n", k, truncateObservation(stableJSON(v), 600))
	}
	b.WriteString("\n【输出契约】只输出 JSON {\"steps\":[...]}。**只输出需要重新规划的节点**（即上面【当前子图定义】列出的那些），已成功节点不要输出。规则：\n")
	b.WriteString("1. id 一律以 \"#E\" 开头跟数字（#E1、#E2…）；#S1、#N1、step1、E1 等写法均非法；\n")
	b.WriteString("2. 冻结契约里的 id 必须原样沿用；其余新节点从当前计划最大编号之后继续（已有 #E1~#E3 就用 #E4、#E5），**不要从 #E1 重新数**；\n")
	b.WriteString("3. 引用可复用的产出（含已成功节点）时，同样要列进 depends_on；\n")
	b.WriteString("4. 同一堵墙（同一个失败原因）不要原样再撞——换工具、换参数或明确放弃该数据。\n")
	b.WriteString("正例（【当前子图定义】含失败的 #E2 与 #E3，#E1 已成功故不出现）：\n")
	b.WriteString(`{"steps":[{"id":"#E2","action":"template.render","args":{"text":"改用文字说明"},"depends_on":["#E1"]},{"id":"#E3","action":"http.Request","args":{"url":"https://example.com"},"depends_on":["#E2"]}]}` + "\n")
	return b.String()
}

// rewooReplanner 对失败子图做一次重规划。返回新计划与「是否真有变化」——
// 与当前子图签名等价（plan-equivalence 震荡守卫）时返回 changed=false，调用方停止重规划。
func (a *AIAgent) rewooReplanner(ctx context.Context, task string, p *rewooPlan, board *reactBoard, al *rewooAllow, opts RewooOpts, failed []string, ev map[string]interface{}, evMu *sync.Mutex) (*rewooPlan, bool, error) {
	scope := rewooReverseReachable(p, failed)
	// 冻结边界：scope 内、有 scope 外消费者的节点——其 id 与产出契约必须原样保留
	var boundary []string
	for _, n := range p.nodes {
		if !scope[n.id] {
			continue
		}
		for _, m := range p.nodes {
			if scope[m.id] {
				continue
			}
			for _, d := range m.deps {
				if d == n.id {
					boundary = append(boundary, n.id)
					break
				}
			}
		}
	}
	sort.Strings(boundary)
	kept := map[string]interface{}{}
	evMu.Lock()
	for _, n := range p.nodes {
		if scope[n.id] {
			continue
		}
		if v, ok := ev[n.id]; ok {
			kept[n.id] = v
		}
	}
	evMu.Unlock()

	lastErr := ""
	for attempt := 0; attempt < 2; attempt++ {
		prompt := a.rewooReplanPrompt(task, p, scope, boundary, kept, evMu, al)
		text, _, _, err := a.completeReactStep(ctx, "rewoo replanner", LLMRequest{
			Messages:    []map[string]any{{"role": "user", "content": prompt}},
			Provider:    opts.Provider,
			Model:       opts.Model,
			BaseURL:     opts.BaseURL,
			APIKey:      opts.APIKey,
			Temperature: opts.Temperature,
		}, nil, board, false)
		if err != nil {
			return nil, false, err
		}
		newNodes, perr := parseRewooPlan(text)
		if perr == nil {
			merged, merr := a.mergeRewooSubplan(p, newNodes, scope, boundary, al, opts.MaxNodes)
			if merr != nil {
				perr = merr
			} else {
				// plan-equivalence：边界契约与节点数都没变 → 再烧一次 replanner 不会不一样
				if rewooSignature(p, scope) == rewooSignature(merged, scope) {
					return nil, false, nil
				}
				return merged, true, nil
			}
		}
		lastErr = perr.Error()
		board.recordRecovery(fmt.Sprintf("replanner 输出未通过校验（第 %d 次）：%s", attempt+1, lastErr))
	}
	return nil, false, fmt.Errorf("replanner 两次输出均未通过校验：%s", lastErr)
}

// mergeRewooSubplan 把重规划出的新子图缝回原计划：scope 内节点整体替换，scope 外
// （已成功）节点原样保留。缝合不变式：冻结边界的 id 与产出契约不动 → 下游引用不断。
func (a *AIAgent) mergeRewooSubplan(old *rewooPlan, fresh *rewooPlan, scope map[string]bool, boundary []string, al *rewooAllow, maxNodes int) (*rewooPlan, error) {
	merged := &rewooPlan{byID: map[string]*rewooNode{}}
	for _, n := range old.nodes {
		if scope[n.id] {
			continue
		}
		merged.nodes = append(merged.nodes, n)
		merged.byID[n.id] = n
	}
	keptIDs := map[string]bool{}
	for _, n := range merged.nodes {
		keptIDs[n.id] = true
	}
	for _, n := range fresh.nodes {
		if keptIDs[n.id] {
			return nil, fmt.Errorf("新子图 id %s 与已成功节点冲突", n.id)
		}
		n.status = rewooPending
		n.output = nil
		n.failure = nil
		merged.nodes = append(merged.nodes, n)
		merged.byID[n.id] = n
	}
	for _, id := range boundary {
		if _, ok := merged.byID[id]; !ok {
			return nil, fmt.Errorf("冻结契约节点 %s 未在重规划输出中重新给出", id)
		}
	}
	if err := a.validateRewooPlan(merged, al, RewooOpts{MaxNodes: maxNodes}); err != nil {
		return nil, err
	}
	return merged, nil
}

// ---- solver（1 LLM：带缺失作答） ----

func (a *AIAgent) rewooSolverPrompt(task string, p *rewooPlan, board *reactBoard, ev map[string]interface{}, evMu *sync.Mutex) string {
	var b strings.Builder
	b.WriteString("你是结果汇总器（solver）。根据任务与各节点的执行产出给出最终答案。要求：如实、简洁、直接回答任务；" +
		"对缺口清单里的内容必须明确说明「未获取」及原因，**不得编造**数据。\n\n")
	b.WriteString("【任务】\n" + task + "\n\n")
	b.WriteString("【节点产出】\n")
	for _, n := range p.nodes {
		if n.status != rewooDone {
			continue
		}
		evMu.Lock()
		v := ev[n.id]
		evMu.Unlock()
		fmt.Fprintf(&b, "- %s（%s）→ %s\n", n.id, n.action, truncateObservation(stableJSON(v), 1200))
	}
	if len(board.gapsSnapshot()) > 0 {
		b.WriteString("\n【" + board.gapNotice() + "】\n")
	} else {
		b.WriteString("\n（无缺口）\n")
	}
	return b.String()
}

// rewooSolve 跑一次 solver LLM。正文即答案（无步数协议）；LLM 侧瞬时错误走恢复层重试，
// auth 等上抛——不硬撑。
func (a *AIAgent) rewooSolver(ctx context.Context, task string, p *rewooPlan, board *reactBoard, opts RewooOpts, ev map[string]interface{}, evMu *sync.Mutex) (string, error) {
	prompt := a.rewooSolverPrompt(task, p, board, ev, evMu)
	text, _, _, err := a.completeReactStep(ctx, "rewoo solver", LLMRequest{
		Messages:    []map[string]any{{"role": "user", "content": prompt}},
		Provider:    opts.Provider,
		Model:       opts.Model,
		BaseURL:     opts.BaseURL,
		APIKey:      opts.APIKey,
		Temperature: opts.Temperature,
	}, nil, board, false)
	if err != nil {
		return "", err
	}
	answer := strings.TrimSpace(text)
	if answer == "" {
		return "", fmt.Errorf("solver 返回空答案")
	}
	return answer, nil
}

// ---- 编排 ----

// Rewoo 跑一次完整的 REWOO-with-recovery：planner(≤2 LLM) → worker(0 LLM, 分层并行,
// fail-soft) → [replanner(≤2×budget LLM, frozen-boundary)] → solver(1 LLM, 带缺失作答)。
//
// LLM 调用**有界**：顺利 3 次、含一次重规划 4 次、最坏 2 + 2×opts.ReplanBudget + 1
// （默认 5）——修正尝试由校验失败驱动，不凭空多烧（测试用调用计数断言，不信口号）。
// 任何失败都有记录：noteFailure 全程登记，收尾按**最后计划状态**重建 gaps
// （重规划换掉的旧失败不留陈账）。
func (a *AIAgent) Rewoo(ctx context.Context, task string, opts RewooOpts) (interface{}, error) {
	if task == "" {
		return nil, fmt.Errorf("rewoo 需要 task（要交给 agent 的完整任务描述）")
	}
	if a.exec == nil {
		return nil, fmt.Errorf("rewoo 需要执行器（Engine 未注入 AIAgent.exec）")
	}
	opts = opts.withDefaults()
	al, err := a.resolveRewooAllow(opts.Allow, opts.AllowSet)
	if err != nil {
		return nil, err
	}
	board := newReactBoard(0) // maxSteps 不用于 rewoo：预算 = 节点数 / 墙钟 / 重试总额
	ctx, cancel := context.WithTimeout(ctx, opts.WallClock)
	defer cancel()

	plan, err := a.rewooPlanner(ctx, task, al, opts, board)
	if err != nil {
		// planner 阶段还没有计划可展示，只能报错——但「context deadline exceeded」本身
		// 不说是谁的 deadline、能不能调、该查什么。墙钟耗尽必须落到可行动文案上。
		return nil, rewooCtxNote(err, ctx, opts.WallClock, "planner")
	}
	ev := map[string]interface{}{}
	var evMu sync.Mutex
	replansLeft := opts.ReplanBudget
	for {
		a.rewooWork(ctx, plan, board, al, opts, ev, &evMu)
		failed := plan.failedIDs()
		if len(failed) == 0 || replansLeft <= 0 {
			break
		}
		merged, changed, rerr := a.rewooReplanner(ctx, task, plan, board, al, opts, failed, ev, &evMu)
		if rerr != nil {
			board.recordRecovery("重规划失败，转为缺口作答：" + rerr.Error())
			break
		}
		if !changed {
			board.recordRecovery("震荡守卫：重规划结果与当前计划等价，停止重规划（转为缺口作答）")
			break
		}
		replansLeft--
		oldCount := len(plan.nodes)
		plan = merged
		// 成功的重规划也必须留痕：只记失败不记成功，排查「它为什么这么答」时看不到
		//「计划被换过」这一半事实（2026-10-19 E2E 场景 3：失败的 http 节点被重规划成
		// template 节点，recovery_log 里却只有两条 L0 重试，像从没重规划过）。
		board.recordRecovery(fmt.Sprintf("重规划生效：失败子图已替换（计划 %d → %d 节点），重跑执行", oldCount, len(plan.nodes)))
	}
	// 收尾：清掉运行中的动作级即时缺口（可能已被重规划作废），按最后计划状态重建——
	// 每个失败/被堵节点一笔，空结果节点一笔（semantic_empty），成功节点不留痕。
	board.clearGaps()
	for _, n := range plan.nodes {
		switch {
		case n.failure != nil:
			board.addGap(*n.failure)
		case n.empty:
			board.addGap(reactFailure{Class: FailSemanticEmpty, Action: n.action,
				GapKey: n.id + "（" + n.action + "）", Reason: "工具返回空结果", Scope: "self"})
		}
	}
	answer, err := a.rewooSolver(ctx, task, plan, board, opts, ev, &evMu)
	if err != nil {
		if ctx.Err() != nil && errors.Is(err, context.DeadlineExceeded) {
			// 墙钟砍在 solver：计划、节点结局、证据、缺口都已就绪——**带部分结果作答**，
			// 不把整轮执行的上下文丢干净（fail-soft 的同一精神：失败也要留下证据）。
			// 直接 return error 的话，用户看到一句「context deadline exceeded」，
			// 完全不知道计划跑到哪、哪些节点成了、该调什么——那是把 5 分钟埋掉。
			board.recordRecovery(fmt.Sprintf("墙钟耗尽于 solver 阶段（总墙钟 %s），以下为部分结果", opts.WallClock))
			board.addGap(reactFailure{Class: FailUnrecoverable, Action: "solver",
				GapKey: "solver", Reason: err.Error(), Scope: "run"})
			answer = "REWOO 总墙钟已用尽：执行阶段已跑完但汇总未得出。下面附各节点结局与缺口；" +
				"慢模型或重任务可调大 wall_clock（CLI --wall-clock 秒数）后重试。"
			return rewooResult(plan, board, ev, &evMu, answer), nil
		}
		return nil, rewooCtxNote(err, ctx, opts.WallClock, "solver")
	}
	return rewooResult(plan, board, ev, &evMu, answer), nil
}

// rewooResult 装配返回结构。gaps / recovery_log 始终存在（空切片）——调用方不必先判存在性
// （react 已确立的口径）。
func rewooResult(p *rewooPlan, board *reactBoard, ev map[string]interface{}, evMu *sync.Mutex, answer string) map[string]interface{} {
	plan := make([]map[string]interface{}, 0, len(p.nodes))
	outcomes := make([]map[string]interface{}, 0, len(p.nodes))
	for _, n := range p.nodes {
		plan = append(plan, map[string]interface{}{
			"id": n.id, "action": n.action, "depends_on": n.deps, "status": n.status,
		})
		oc := map[string]interface{}{
			"id": n.id, "action": n.action, "status": n.status, "attempts": n.attempts,
		}
		if n.failure != nil {
			oc["error"] = n.failure.Reason
			oc["type"] = string(n.failure.Class)
		}
		outcomes = append(outcomes, oc)
	}
	evMu.Lock()
	evidence := make(map[string]interface{}, len(ev))
	for k, v := range ev {
		evidence[k] = v
	}
	evMu.Unlock()
	return map[string]interface{}{
		"answer":        answer,
		"plan":          plan,
		"evidence":      evidence,
		"node_outcomes": outcomes,
		"gaps":          board.gapsSnapshot(),
		"recovery_log":  board.recoverySnapshot(),
	}
}

// rewooCtxNote 把「墙钟耗尽」类错误改写成可行动的文案。
//
// 用户原话级别的痛点：跑 5 分钟后只看到 "context deadline exceeded"——不知道是谁的
// deadline、能不能调、该查什么。2026-10-20 实测：重任务路径 LLM 要跑 4~5 次（planner 1 +
// replanner 2 + solver 1），默认墙钟 5 分钟在稍慢的端点/模型下就会被砍，而 CLI 原先没有
// 任何 flag 能放宽。文案必须同时给出：① 这是总墙钟 ② 怎么调 ③ 最可能的两个原因
// （模型慢 / 端点不可达都会等满整个墙钟）。
func rewooCtxNote(err error, ctx context.Context, wall time.Duration, phase string) error {
	if err == nil || ctx.Err() == nil {
		return err
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("REWOO 总墙钟（%s）在 %s 阶段用尽：%w。"+
		"慢模型或重任务可用 wall_clock 参数（CLI --wall-clock 秒数）放宽；"+
		"LLM 端点不可达或响应过慢时会等满整个墙钟——先检查 network / base_url / api_key", wall, phase, err)
}

// RewooMethod 暴露给 Lua ctx.ext.ai.rewoo：解析参数后跑 Rewoo。
// allow 子集校验与 react 同构：能力项必须是调用方声明的子集；工具 id 项要求调用方声明过
// call 能力——ctx.call 是插件间唯一通信通道，工具节点的门籍就在那里。
func (a *AIAgent) RewooMethod(args map[string]interface{}, callerCaps ...string) (interface{}, error) {
	task, _ := args["task"].(string)
	if task == "" {
		return nil, fmt.Errorf("rewoo 需要 task（要交给 agent 的完整任务描述）")
	}
	opts := RewooOpts{}
	if v, ok := toFloat(args["max_nodes"]); ok && int(v) > 0 {
		opts.MaxNodes = int(v)
	}
	if v, ok := toFloat(args["replan_budget"]); ok && int(v) >= 0 {
		opts.ReplanBudget = int(v)
	}
	if v, ok := toFloat(args["wall_clock"]); ok && v > 0 {
		opts.WallClock = time.Duration(v) * time.Second
	}
	explicit := toStringSlice(args["allow"])
	if len(explicit) > 0 && len(callerCaps) > 0 {
		hasCall := containsString(callerCaps, "call")
		for _, name := range explicit {
			capName := name
			if !a.knownCapability(name) {
				if c, _, ok := splitCapMethod(name); ok {
					capName = c
				}
			}
			if t, err := a.reg.GetTool(name); err == nil && t != nil {
				if !hasCall {
					return nil, fmt.Errorf("rewoo 的 allow 含工具 %q，但本 Tool 没有声明 call 能力——"+
						"调用已注册工具的通道是 ctx.call，请在本 Tool 的 capabilities 里声明 call", name)
				}
				continue
			}
			if f, err := a.reg.GetFlow(name); err == nil && f != nil {
				if !hasCall {
					return nil, fmt.Errorf("rewoo 的 allow 含 Flow %q，但本 Tool 没有声明 call 能力——"+
						"调用 Flow 的通道是 ctx.call，请在本 Tool 的 capabilities 里声明 call", name)
				}
				continue
			}
			if !containsString(callerCaps, capName) {
				return nil, fmt.Errorf("rewoo 的 allow 含 %q，但本 Tool 只声明了 %v——allow 不能超出自己的 capability；"+
					"要用更多能力，请在本 Tool 的 capabilities 里声明", name, callerCaps)
			}
		}
	}
	if len(explicit) > 0 {
		opts.Allow = explicit
		opts.AllowSet = true
	} else {
		opts.Allow = a.knownOnly(callerCaps)
		opts.AllowSet = true
	}
	if s, ok := args["provider"].(string); ok && s != "" {
		opts.Provider = s
	}
	if s, ok := args["model"].(string); ok && s != "" {
		opts.Model = s
	}
	if s, ok := args["base_url"].(string); ok && s != "" {
		opts.BaseURL = s
	}
	if s, ok := args["api_key"].(string); ok && s != "" {
		opts.APIKey = s
	}
	if v, ok := toFloat(args["temperature"]); ok && v > 0 {
		opts.Temperature = v
	}
	return a.Rewoo(context.Background(), task, opts)
}

// ---- 小工具 ----

func sortedCapNames(m map[string]map[string]MethodSchema) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
