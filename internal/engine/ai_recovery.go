package engine

// 本文件是 react 的失败恢复层（REQ-REACT-RECOVERY），设计来源是 REWOO-with-recovery：
// 一次规划/决策、执行期按失败类型分层恢复、恢复尽量不烧 LLM 轮次、绝不静默吞失败。
// 与 ReAct 的对应关系：react 没有静态 DAG，「重规划」由模型自己完成（它看信号改下一步），
// 因此这里不重造 planner/worker，只把三件事搬进来——
//   ① 失败分类法（tool 错误 → 七类，每类一张声明式恢复策略表）；
//   ② 分层恢复（L0 机械重试 / L1.5 换通道 / L1 结构化信号回灌 / 兜底 gaps）；
//   ③ 黑板状态（失败、恢复流水、缺口全程可观测，直接补回 ReAct 丢掉的「逐步可视」）。

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// FailureClass 是失败的类型标签。整个恢复层的前置条件都用它写——分类法定不下来，
// 恢复策略、回灌信号、缺口判定全部悬空，所以它是这一层的「语言」。
//
// 七类沿两条轴展开：机械可恢复性 × 首用恢复档位。每类都在 failureRecovery 里有显式策略，
// 且**每类都有显式最终处置**（兜底缺口或需人工）——没有任何一类会被静默丢掉。
type FailureClass string

const (
	// FailTransient：超时 / 连接重置 / 429 / 503 这类「再来一次可能就好」的错误。首用 L0 机械重试。
	FailTransient FailureClass = "transient"
	// FailChannel：主通道不可用（能力未装配 / 侧车未起 / 端点不吃工具声明）。首用 L1.5 换通道，换不了就需人工。
	FailChannel FailureClass = "channel"
	// FailBadInput：参数或调用形态错（缺必填、非法 JSON、SQL 语法错）。机械重试同样输入毫无意义，直接 L1 信号回灌让模型改。
	FailBadInput FailureClass = "bad_input"
	// FailUpstreamBad：根因在上游产出（本步的输入来自某一步的空结果/失败）。信号要带上 scope=upstream，引导先修上游。
	FailUpstreamBad FailureClass = "upstream_bad"
	// FailSemanticEmpty：查无结果 / 空产出。可能是正确答案（确实没数据），所以不立即判缺口——连续多次才判定。
	FailSemanticEmpty FailureClass = "semantic_empty"
	// FailAuth：401/403/配额/密钥未播种。机械恢复无效，标需人工。
	FailAuth FailureClass = "auth"
	// FailUnrecoverable：404/方法不存在/解析不可能。立即记缺口，绝不让循环在它身上烧步数。
	FailUnrecoverable FailureClass = "unrecoverable"
)

// RecoverableError 是失败分类的**数据驱动钩子**：错误实现方自己声明恢复类别，优先于
// 中央分类器的字符串启发式。动机与「tool 契约声明错误类别」同源——最懂这个错误的是
// 产出它的那个实现，不该由中央一个 heuristic 猜。今天 exec 通道上还没有实现它的错误
// 类型，钩子先立在这里；中央分类器判不出一律归 unrecoverable（见 classifyToolFailure），
// 绝不因为「猜不出」就把失败吞掉。
type RecoverableError interface {
	RecoveryClass() FailureClass
}

// recoveryPlan 是某类失败的声明式恢复策略。加一种失败类型 = 加一行表，不是改 if/elif 链。
type recoveryPlan struct {
	retry bool // L0：允许机械重试（不烧 LLM 轮次）
	gap   bool // 最终是否成为缺口（true = 绝不静默吞；当前七类全为 true）
	human bool // 是否需人工介入（auth / channel）
}

// failureRecovery 是七类失败的策略表。顺序即文档：transient 先重试；
// bad_input/upstream_bad 直接跳重试档（重试同样输入没意义）；auth/unrecoverable 不指望模型自愈。
var failureRecovery = map[FailureClass]recoveryPlan{
	FailTransient:     {retry: true, gap: true},
	FailChannel:       {retry: false, gap: true, human: true},
	FailBadInput:      {retry: false, gap: true},
	FailUpstreamBad:   {retry: false, gap: true},
	FailSemanticEmpty: {retry: false, gap: true},
	FailAuth:          {retry: false, gap: true, human: true},
	FailUnrecoverable: {retry: false, gap: true},
}

// reactFailure 是一次失败的完整记录：类型 + 动作 + 原因 + 范围 + 已机械重试几次。
// Scope 是给 replanner（这里是模型）的 scope_hint：self = 本步自己错；upstream = 根因在上游产出。
// GapKey 非空时缺口以它为键（REWOO 的节点级缺口：同一动作可被多个节点使用，动作级键会互相
// 抵消/串味；震荡守卫等其余判据仍用 Action）。
type reactFailure struct {
	Class    FailureClass
	Action   string // 「能力.方法」；LLM 调用失败记 "llm"
	GapKey   string // 非空时缺口以此为键（默认同 Action）
	Reason   string
	Scope    string // "self" | "upstream"
	Attempts int    // 已发生的机械重试次数
}

// classifyToolFailure 把一次工具/LLM 错误分到七类里。
//
// 判据顺序（先匹配先赢，**顺序即口径**，调整会改变分类结果）：
//  1. 数据驱动钩子：错误自己实现 RecoverableError → 听它的；
//  2. auth：凭据/授权/配额类——机械重试无意义，需人工；
//  3. channel：能力已授权但未实现/未装配；
//  4. transient：超时/连接类/429/503——重试有用；
//  5. unrecoverable：404/不存在/未知方法——重来还一样；
//  6. bad_input：参数/形态/SQL 错——模型改参数就能好；
//  7. 兜底 unrecoverable：判不出的**绝不吞**，按不可恢复走「缺口 + 如实作答」。
//
// 为什么兜底是 unrecoverable 而不是 transient：把未知错误当可恢复重试，会在一个
// 确定性错误上白烧预算；当不可恢复则只会损失一次重试机会，却保住「不静默」。
func classifyToolFailure(action string, err error) reactFailure {
	f := reactFailure{Class: FailUnrecoverable, Action: action, Scope: "self"}
	if err == nil {
		return f
	}
	f.Reason = err.Error()
	// ① 数据驱动：错误自己声明类别（tool 契约的落点，见 RecoverableError）。
	var re RecoverableError
	if asRecoverable(err, &re) {
		f.Class = re.RecoveryClass()
		return f
	}
	s := strings.ToLower(err.Error())
	switch {
	// ② auth：401/403/未授权/配额/密钥或配置未就位（secret 不存在 = 要人播种，不是模型能修的）
	case containsAny(s, "401", "403", "未授权", "unauthorized", "forbidden",
		"permission denied", "api key", "apikey", "quota", "secret 不存在", "配置项不存在"):
		f.Class = FailAuth
	// ③ channel：能力在目录里、实现没装配（ErrCapabilityUnavailable 的文案）
	case containsAny(s, "已授权但尚未实现", "尚未实现", "not implemented", "未装配"):
		f.Class = FailChannel
	// ④ transient：网络抖动与限流——重试（带退避）是标准解。
	// ⚠ Windows 的 syscall 错误文案与 Unix 完全不同：ECONNREFUSED 在 Unix 是
	// "connection refused"，在 Windows 是 "connectex: No connection could be made
	// because the target machine actively refused it"；ECONNRESET 是 "wsarecv: An
	// existing connection was forcibly closed by the remote peer"。2026-10-19 E2E
	// 实测（Windows 真机跑死端口）：只写 Unix 味关键词时，整个网络分支在这台机器上
	// 全是死的——连接拒绝掉到 unrecoverable 兜底，L0 重试从未发生。两边都要覆盖。
	case containsAny(s, "timeout", "timed out", "deadline exceeded", "connection refused",
		"connection reset", "broken pipe", "no route to host", "temporary failure",
		"too many requests", "rate limit", "429", "502", "503", "504",
		"service unavailable", "bad gateway", "gateway timeout", "eof",
		"actively refused", "no connection could be made", "connection attempt failed",
		"did not properly respond", "failed to respond", "forcibly closed", "unreachable",
		"wsarecv", "wsasend", "wsaeconnreset", "wsaeconnaborted", "wsaeconnrefused"):
		f.Class = FailTransient
	// ⑤ unrecoverable：资源/方法不存在，重来还一样
	case containsAny(s, "404", "not found", "不存在", "no such method", "unknown method",
		"unsupported", "unknown tool", "无法识别"):
		f.Class = FailUnrecoverable
	// ⑥ bad_input：调用方（模型）能自愈的那类——缺参数、形态不对、SQL 写错。
	// DNS 硬失败（no such host）也归这里：URL 是模型自己选的，最常见原因是域名
	// 写错——改对域名就能好，重试同一个域名毫无意义；DNS 真故障时由震荡守卫兜底
	// （同动作连续失败即封禁+缺口），代价有界。区分于「temporary failure in name
	// resolution」——那是软故障，已由 ④ 接走，重试确实有用。
	case containsAny(s, "缺少必填参数", "需传入", "不是合法 json", "invalid json", "cannot unmarshal",
		"参数", "argument", "syntax error", "no such table", "no such column",
		"类型不符", "type mismatch", "必填", "no such host"):
		f.Class = FailBadInput
	}
	return f
}

// asRecoverable 在不引 errors 的情况下探一侧 RecoverableError（exec 通道的错误可能被包了
// 多层 fmt.Errorf，Unwrap 链要走完；今天没有实现者，走空返回 false）。
func asRecoverable(err error, target *RecoverableError) bool {
	for err != nil {
		if r, ok := err.(RecoverableError); ok {
			*target = r
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// reactBudget 是 react 一次运行的全局预算：步数（既有 max_steps）+ 机械重试总次数。
// 重试预算存在的理由：没有它，「每类失败各重试几次」会在长任务里复利成可观的浪费；
// 有它，最坏情况有上界（震荡守卫再兜底，见 reactBoard）。
type reactBudget struct {
	maxSteps    int
	retriesLeft int
}

// reactBoard 是 react 循环的黑板：失败、恢复流水、缺口、降级、震荡计数集中在一处。
// 它同时是**可观测面**——每轮 KS（这里是循环体）触发后的 diff 直接进 steps / recovery_log /
// gaps，UI 或调用方拿到的就是完整恢复史，不必从 observation 文本里抠。
//
// 并发说明：react 自己是单线程循环，但 REWOO（ai_rewoo.go）的 worker 按拓扑层并行执行节点、
// 多 goroutine 共写这一块黑板——所以全部读写方法都过 mu。锁只在这些短临界区上，不持有锁
// 做 LLM/工具调用（那会串行化整层并行）。内部 *Locked 版本供已持锁的路径复用，避免自锁。
type reactBoard struct {
	mu sync.Mutex

	budget reactBudget

	failures []reactFailure           // 全部失败（含已恢复的），按发生序
	gaps     []map[string]interface{} // 缺口清单（带缺失作答的事实来源）
	recovery []string                 // 恢复动作流水（L0/L1.5 的人类可读记录）

	sigCount       map[string]int    // 失败签名（动作|类型）→ 次数：震荡守卫的判据
	blocked        map[string]string // 被震荡守卫封禁的动作 → 封禁原因
	lastEvidenceOK bool              // 最近一次证据是否可用（空/失败 → false）：scope=upstream 的判据
	outcome        map[string]bool   // 动作 → 最后一次的结局（收尾补全缺口的判据，见 finalizeGaps）
}

// 恢复层常量。都是「便宜的上限」：机械动作不烧 LLM，但也不能无限。
const (
	// maxToolRetries 是单次工具调用允许的机械重试上限（transient 与可重试 HTTP 状态码）。
	maxToolRetries = 2
	// maxLLMRetries 是单次 LLM 调用的机械重试上限（provider 侧瞬时错误）。
	maxLLMRetries = 2
	// maxTotalRetries 是整场 react 的机械重试总预算（工具 + LLM 共用）。
	maxTotalRetries = 8
	// maxSameSignature 是震荡守卫阈值：同一动作同一类型失败这么多次，就封禁该动作并记缺口。
	// 对应 REWOO 设计里的 plan-equivalence 守卫——反复得到同一个失败，说明「再试一次」
	// 不会不一样，继续烧步数只会把预算耗光然后给个更差的答案。
	maxSameSignature = 3
)

func newReactBoard(maxSteps int) *reactBoard {
	return &reactBoard{
		budget:         reactBudget{maxSteps: maxSteps, retriesLeft: maxTotalRetries},
		sigCount:       map[string]int{},
		blocked:        map[string]string{},
		lastEvidenceOK: true,
	}
}

// scopeHint 报告新失败的 scope：上一步证据就不可用时，根因大概率在上游。
// 这只是提示（模型仍可能自己判断错），但比一律标 self 强——「修本节点」与「修上游」
// 是两种完全不同的下一步。
func (b *reactBoard) scopeHint() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.lastEvidenceOK {
		return "self"
	}
	return "upstream"
}

// noteEvidence 记一次动作的结果走向：lastEvidenceOK 供 scopeHint 判「根因在上游还是本节点」，
// outcome 供收尾补全判「这个动作最后成没成」。
func (b *reactBoard) noteEvidence(action string, ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lastEvidenceOK = ok
	if b.outcome == nil {
		b.outcome = map[string]bool{}
	}
	b.outcome[action] = ok
}

// finalizeGaps 收尾补全：跑完后仍以失败收尾的动作，统一转成缺口。
//
// 为什么需要它：即时缺口只覆盖 auth/unrecoverable（首败即记）与震荡阈值（连续 3 次）。
// 单次 transient 失败后模型干脆放弃、改用别的路子作答时，两条路都不触发——gaps 是空的，
// 而答案里缺的那块数据只有模型正文提过。按「静默残缺比明说缺失坏得多」，结构化缺口必须
// 收口：**最后结局是失败的动作就得在 gaps 里有一笔**（2026-10-19 E2E 实测发现：
// Windows 死端口单次失败后 gaps=[]，调用方无从知道数据没拿到）。
//
// 判据是「该动作最后的结局」而非「历史上失败过」：失败后换参数重试成功的动作不记
// （数据拿到了，没有缺口）。模型绕路用别的动作拿到等效数据时，这里仍会记一笔——
// 但那笔录的是「这个动作没成」这个事实，不是「答案不完整」的断言，排查时正是要看的。
func (b *reactBoard) finalizeGaps() {
	b.mu.Lock()
	defer b.mu.Unlock()
	// 每个动作只保留最后一次失败（reason 取最新的，与 outcome 的结局对应）。
	last := map[string]reactFailure{}
	for _, f := range b.failures {
		last[f.Action] = f
	}
	for action, f := range last {
		if b.outcome[action] {
			continue // 最后成功了——没有缺口
		}
		b.addGapLocked(f)
	}
}

// spendRetry 消耗一次全局机械重试预算；预算耗尽返回 false（调用方不得再重试）。
func (b *reactBoard) spendRetry() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.budget.retriesLeft <= 0 {
		return false
	}
	b.budget.retriesLeft--
	return true
}

// recordRecovery 记一条恢复流水（L0/L1.5 的动作），供 recovery_log 与 UI 展示。
func (b *reactBoard) recordRecovery(s string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.recovery = append(b.recovery, s)
}

// noteFailure 登记一次失败并跑恢复决策：立即可缺口的类型（auth/unrecoverable）当场记缺口；
// 任何类型累计到震荡阈值都封禁 + 记缺口。绝不出现「失败了但没有任何记录」的路径。
// 注意「立即记缺口」不替代震荡守卫：缺口让模型**知道**拿不到，封禁让循环**停止**在同一个
// 动作上烧步数——模型拿不同参数反复撞同一个 404 是常见形态，两件事都要做。
func (b *reactBoard) noteFailure(f reactFailure) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.noteFailureLocked(f)
}

func (b *reactBoard) noteFailureLocked(f reactFailure) {
	b.failures = append(b.failures, f)
	sig := f.Action + "|" + string(f.Class)
	b.sigCount[sig]++
	plan := failureRecovery[f.Class]
	// 机械恢复注定无效的两类（auth 需人工 / unrecoverable 重来还一样）：立即记缺口，
	// 不等阈值——等的只是让循环在它身上白烧步数。
	if plan.human || f.Class == FailUnrecoverable {
		b.addGapLocked(f)
	}
	if b.sigCount[sig] >= maxSameSignature {
		why := fmt.Sprintf("连续 %d 次失败（类型=%s）", b.sigCount[sig], f.Class)
		if f.Class == FailSemanticEmpty {
			why = fmt.Sprintf("连续 %d 次查询无结果", b.sigCount[sig])
		}
		b.blocked[f.Action] = why
		b.addGapLocked(f)
		b.recovery = append(b.recovery, fmt.Sprintf("震荡守卫：%s %s，已停止调用并记缺口", f.Action, why))
	}
}

// addGap 记一个缺口（按 动作|类型 去重）。需人工与否由策略表推导（auth/channel）。
func (b *reactBoard) addGap(f reactFailure) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.addGapLocked(f)
}

func (b *reactBoard) addGapLocked(f reactFailure) {
	key := f.Action + "|" + string(f.Class)
	action := f.Action
	if f.GapKey != "" {
		key = f.GapKey + "|" + string(f.Class)
		action = f.GapKey // 缺口记录带上 GapKey（REWOO 的节点身份）：同动作多节点时，
		// 只记 "http.Request" 说不出是哪个 #E 败了，排查与作答都要点名到节点
	}
	for _, g := range b.gaps {
		if fmt.Sprint(g["action"], "|", g["type"]) == key {
			return
		}
	}
	b.gaps = append(b.gaps, map[string]interface{}{
		"action": action,
		"type":   string(f.Class),
		"reason": f.Reason,
		"scope":  f.Scope,
		"human":  failureRecovery[f.Class].human,
	})
}

// clearGaps 清空缺口清单。REWOO 用它在收尾前重建缺口：运行中 noteFailure 记的是动作级
// 即时缺口（供震荡守卫等判据），而重规划会把失败节点换成新节点——旧节点的即时缺口可能已经
// 过时（数据最终拿到了）。最终缺口只认**最后计划状态**，所以在收尾时清空重算，避免陈账。
func (b *reactBoard) clearGaps() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.gaps = nil
}

// blockedNotice 报告某动作是否已被震荡守卫封禁；封禁时返回要给模型的说明。
func (b *reactBoard) blockedNotice(action string) (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	why, ok := b.blocked[action]
	if !ok {
		return "", false
	}
	return fmt.Sprintf("工具 %s 已被停止调用（%s）。请改用其它方式完成任务；"+
		"若该数据不可或缺，请在 Final Answer 中明确说明缺口，不要换参数反复重试。", action, why), true
}

// gapNotice 是给模型的缺口通知：回答时必须如实说明、不得编造。
func (b *reactBoard) gapNotice() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var sb strings.Builder
	sb.WriteString("已知缺口（以下数据/能力未能获取）：回答时必须如实说明这些缺口，不得编造：\n")
	for i, g := range b.gaps {
		human := ""
		if h, _ := g["human"].(bool); h {
			human = "，需人工介入"
		}
		fmt.Fprintf(&sb, "%d. [%s] %s：%s%s\n", i+1, g["type"], g["action"], g["reason"], human)
	}
	return sb.String()
}

// gapsSnapshot / recoverySnapshot 是结果装配用的快照读（拷贝，避免调用方与并发写入共享底层数组）。
func (b *reactBoard) gapsSnapshot() []map[string]interface{} {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]map[string]interface{}, len(b.gaps))
	copy(out, b.gaps)
	return out
}

func (b *reactBoard) recoverySnapshot() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, len(b.recovery))
	copy(out, b.recovery)
	return out
}

// failureSignal 是把失败回灌给模型（L1）的结构化信号：类型 / 范围 / 动作 / 原因 / 已重试几次 /
// 按类型的下一步建议。取代旧实现里一句裸的「工具执行错误：<原文>」——那句话把分类工作
// 整个推给模型，而它最常见的反应是原样重发（直到撞上重复调用守卫）。
func failureSignal(f reactFailure) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "【失败信号】类型=%s 范围=%s 动作=%s\n", f.Class, f.Scope, f.Action)
	fmt.Fprintf(&sb, "原因：%s\n", f.Reason)
	fmt.Fprintf(&sb, "已机械重试 %d 次。", f.Attempts)
	switch {
	case f.Class == FailTransient:
		sb.WriteString("瞬时错误重试后仍失败：可稍后重试、换参数或换工具；实在不行就在 Final Answer 里说明。")
	case f.Class == FailChannel:
		sb.WriteString("该能力当前不可用（未装配/未启动/端点不支持）：任务可绕过请继续，不可绕过请说明缺口，不要反复重试。")
	case f.Class == FailBadInput:
		sb.WriteString("参数或调用形态不对：按原因修正后重试（修正后的参数与上次不同，不算重复调用）。")
	case f.Class == FailUpstreamBad:
		sb.WriteString("根因在上游：上一步产出为空或失败，先重新获取上游数据，或改走不依赖它的路径。")
	case f.Class == FailSemanticEmpty:
		sb.WriteString("查询无结果。如确认数据不存在，请直接说明「未找到」，不要换参数反复查询。")
	case f.Class == FailAuth:
		sb.WriteString("需要人工介入（凭据/配额/授权）：不要反复重试，请在 Final Answer 中说明该数据未能获取。")
	default: // FailUnrecoverable
		sb.WriteString("该调用无法成功（资源不存在/方法不存在/无法解析）：换其它方式完成，或在 Final Answer 中说明缺口。")
	}
	return sb.String()
}

// retryableHTTPStatus 检查工具结果里是否带着「重试有意义」的 HTTP 状态码。
// ctx.http.Request 不把非 2xx 当错误（status 进 body），所以这类瞬时失败在 err 路径上
// 看不见——不在这里接住，L0 重试对 HTTP 类工具等于不存在。
func retryableHTTPStatus(out map[string]interface{}) (int, bool) {
	if out == nil {
		return 0, false
	}
	r, ok := out["result"]
	if !ok {
		return 0, false
	}
	m, ok := r.(map[string]interface{})
	if !ok {
		return 0, false
	}
	switch v := m["status"].(type) {
	case float64:
		return int(v), isRetryableStatus(int(v))
	case int:
		return v, isRetryableStatus(v)
	case int64:
		return int(v), isRetryableStatus(int(v))
	}
	return 0, false
}

func isRetryableStatus(code int) bool {
	switch code {
	case 429, 502, 503, 504:
		return true
	}
	return false
}

// isEmptyToolResult 判断一次成功执行是否「查无结果」。空结果不是错误（确实没数据
// 就是正确答案），所以不当失败立即处置，只登记 semantic_empty 供震荡守卫计数。
func isEmptyToolResult(out map[string]interface{}) bool {
	if out == nil {
		return true
	}
	r, ok := out["result"]
	if !ok {
		return len(out) == 0
	}
	switch v := r.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(v) == ""
	case []interface{}:
		return len(v) == 0
	case map[string]interface{}:
		return len(v) == 0
	}
	return false
}

// reactToolsUnsupported 判断 LLM 错误是否是「端点不吃工具声明」。判据刻意保守：
// 必须同时出现工具语义词与不支持语义词，否则宁可中止也不误降级——把普通 400 当成
// 「不支持工具」会让一次真实的参数错误被降级路径掩盖。
func reactToolsUnsupported(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	if !strings.Contains(s, "tool") && !strings.Contains(s, "function") {
		return false
	}
	return containsAny(s, "not support", "unsupported", "does not support",
		"not enabled", "unknown field", "unrecognized")
}

// sleepBackoff 机械重试之间的退避。生产默认 150ms 起翻倍、封顶 2s；测试经
// AIAgent.retryBackoff 注入零值，保证确定性不拖慢。
func sleepBackoff(fn func(int) time.Duration, attempt int) {
	var d time.Duration
	if fn != nil {
		d = fn(attempt)
	} else {
		d = time.Duration(150<<attempt) * time.Millisecond
		if d > 2*time.Second {
			d = 2 * time.Second
		}
	}
	if d > 0 {
		time.Sleep(d)
	}
}

// completeReactStep 跑一次 LLM 补全，外面包一层恢复（REQ-REACT-RECOVERY 的 provider 侧）：
//   - L0：transient（超时/连接/429/503）机械重试，上限 maxLLMRetries，与工具重试共享全局预算；
//   - L1.5：端点不吃工具声明时换通道——降级为文本 Action 协议（返回 degraded=true，
//     调用方切掉工具面、重建提示词，下一轮用新通道重问，不额外消耗执行步）；
//   - 其余（auth/unrecoverable/重试耗尽）返回带分类的错误上抛——机械恢复无效时不硬撑。
//
// label 只进错误文案（"react"/"rewoo"），让调用方从报错就能看出是哪条 agent 路径败了。
// allowDegrade=false（元工具模式）时刻意不降级：meta 的工具面本身就是 4 个元工具，
// 文本协议表达不了 find→describe→invoke 工作流，硬降级只会得到一个跑偏的循环，
// 不如明确报错让人换模式或换模型。
func (a *AIAgent) completeReactStep(ctx context.Context, label string, req LLMRequest, tools []ToolSpec,
	board *reactBoard, allowDegrade bool) (string, []ToolCall, bool, error) {
	for attempt := 0; ; attempt++ {
		var (
			res ChatResult
			err error
		)
		if len(tools) > 0 {
			res, err = a.llm.CompleteTools(ctx, req, tools)
		} else {
			var text string
			text, err = a.llm.Complete(ctx, req)
			res = ChatResult{Text: text}
		}
		if err == nil {
			return res.Text, res.ToolCalls, false, nil
		}
		if ctx.Err() != nil {
			return "", nil, false, fmt.Errorf("%s 执行上下文已结束（第 %d 次 LLM 调用）: %w", label, attempt+1, ctx.Err())
		}
		f := classifyToolFailure("llm", err)
		if len(tools) > 0 && reactToolsUnsupported(err) {
			if !allowDegrade {
				return "", nil, false, fmt.Errorf("%s：模型端点不支持工具声明（function calling）：%v；"+
					"元工具模式（mode=\"meta\"）强依赖它，请改用默认 flat 模式或换一个支持工具调用的模型", label, err)
			}
			board.recordRecovery(fmt.Sprintf("L1.5 换通道：端点不支持工具声明（%v），降级为文本 Action 协议", err))
			return "", nil, true, nil
		}
		if f.Class == FailTransient && attempt < maxLLMRetries && board.spendRetry() {
			board.recordRecovery(fmt.Sprintf("L0 重试 LLM 调用（%s，第 %d 次）：%v", f.Class, attempt+1, err))
			sleepBackoff(a.retryBackoff, attempt)
			continue
		}
		return "", nil, false, fmt.Errorf("%s LLM 调用失败（%s，第 %d 次尝试）: %w", label, f.Class, attempt+1, err)
	}
}
