// 依赖推断与分层 —— **镜像** internal/engine/deps.go 的语义。
//
// 为什么要在前端重实现一遍（而不是等内核报错）：
// 红线 10「能静态校验的，不许留到运行期」。用户在表单里改一个 step 的 input，
// 就应当立刻看到「这条引用会让 X 变成 Y 的隐式依赖」，而不是提交后被 POST /api/flows 打回。
//
// 与 Go 的差异（**仅此一处，显式声明**）：Go 的错误是「遇到第一个即返回」，
// 本文件同样在 effectiveDeps / topoLevels 里抛出第一个错误（保持一致）；
// 但 precheck.ts 的汇总接口会尽可能多收集，供 UI 一次列全（纯展示增强，不改变判定）。

/** step 的最小形状（Flow 内我们只依赖这些字段做图论计算）。 */
export interface StepLike {
  id: string
  call?: string
  depends_on?: string[] | null
  when?: string
  input?: unknown
  use_version?: string
  mode?: string
  parallel?: boolean
}

export class FlowGraphError extends Error {
  constructor(message: string) {
    super(message)
    this.name = 'FlowGraphError'
  }
}

export interface TemplateRef {
  raw: string
  prefix: string
  path: string[]
  start: number
  end: number
}

/**
 * 扫描字符串里的全部 `${...}`（镜像 engine/template.go scanTemplateRefs）。
 * 未闭合（有 `${` 无 `}`）直接抛错 —— 与运行期展开、注册期校验保持同一解释，避免
 * 「注册期放过、运行期报错」的错位。
 */
export function scanTemplateRefs(s: string): TemplateRef[] {
  const out: TemplateRef[] = []
  let i = 0
  while (i < s.length) {
    const rel = s.indexOf('${', i)
    if (rel < 0) break
    const start = rel
    const relEnd = s.indexOf('}', start)
    if (relEnd < 0) throw new FlowGraphError(`模板未闭合: ${s}`)
    const raw = s.slice(start + 2, relEnd).trim()
    const parts = raw.split('.')
    out.push({ raw, prefix: parts[0], path: parts.slice(1), start, end: relEnd })
    i = relEnd + 1
  }
  return out
}

/** 递归访问值里所有字符串（map 键先排序，保证报错顺序稳定，镜像 Go walkStrings）。 */
export function walkStrings(v: unknown, fn: (s: string) => void): void {
  if (typeof v === 'string') {
    fn(v)
    return
  }
  if (Array.isArray(v)) {
    for (const item of v) walkStrings(item, fn)
    return
  }
  if (v !== null && typeof v === 'object') {
    const obj = v as Record<string, unknown>
    for (const k of Object.keys(obj).sort()) walkStrings(obj[k], fn)
  }
}

/** 把字符串字面量内容替换为空格（保留引号），避免 `when: 'x == "steps.nope"'` 被误判为引用。 */
export function stripQuoted(s: string): string {
  const out = Array.from(s)
  let quote: string | null = null
  for (let i = 0; i < out.length; i++) {
    const r = out[i]
    if (quote === null && (r === '"' || r === "'" || r === '`')) quote = r
    else if (quote !== null && r === quote) quote = null
    else if (quote !== null) out[i] = ' '
  }
  return out.join('')
}

/** when 里的裸 `steps.<id>` 引用（when 是表达式，没有 ${} 包裹）。 */
const WHEN_STEP_REF_RE = /(?:^|[^A-Za-z0-9_.])steps\.([A-Za-z0-9_-]+)/g

export function collectWhenRefs(when: string | undefined | null, seen: Set<string>): void {
  if (!when) return
  const src = stripQuoted(when)
  WHEN_STEP_REF_RE.lastIndex = 0
  let m: RegExpExecArray | null
  while ((m = WHEN_STEP_REF_RE.exec(src)) !== null) seen.add(m[1])
}

/** 收集模板值里 `${steps.<id>.…}` 引用的 step id。 */
export function collectStepRefsFromValue(v: unknown, seen: Set<string>): void {
  walkStrings(v, (s) => {
    for (const r of scanTemplateRefs(s)) {
      if (r.prefix === 'steps' && r.path.length > 0) seen.add(r.path[0])
    }
  })
}

/**
 * 有效依赖 = 显式 depends_on ∪ 从 input 模板推断 ∪ 从 when 表达式推断。
 *
 * 为什么必须包含 when：并行调度下，条件判断本身要读 `steps.X.output`，
 * 若 X 不是 Y 的依赖，Y 可能与 X 同层并发，读到「尚未执行」的输出。
 * （这正是内核曾经踩过的坑，见方法论 when 切片的回归锁。）
 */
export function effectiveDeps(steps: StepLike[]): Record<string, string[]> {
  const idset = new Set(steps.map((s) => s.id))
  const out: Record<string, string[]> = {}

  for (const s of steps) {
    const seen = new Set<string>()
    const order: string[] = []

    const add = (d: string, kind: string) => {
      if (!idset.has(d)) throw new FlowGraphError(`step ${s.id} ${kind}不存在的 step: ${d}`)
      if (d === s.id) throw new FlowGraphError(`step ${s.id} 不能依赖/引用自身`)
      if (!seen.has(d)) {
        seen.add(d)
        order.push(d)
      }
    }

    for (const d of s.depends_on ?? []) add(d, '依赖')

    const refs = new Set<string>()
    try {
      collectStepRefsFromValue(s.input, refs)
    } catch (e) {
      throw new FlowGraphError(`step ${s.id} input 模板解析失败: ${(e as Error).message}`)
    }
    collectWhenRefs(s.when, refs)

    for (const r of Array.from(refs).sort()) add(r, '引用')
    out[s.id] = order
  }
  return out
}

/**
 * Kahn 分层：同层内 step 互不依赖 ⇒ 可并行执行（镜像 engine/topoLevels）。
 * 层内按原下标升序，保证结果稳定（否则 UI 每次刷新布局都在跳）。
 */
export function topoLevels(steps: StepLike[]): string[][] {
  const deps = effectiveDeps(steps)
  const n = steps.length
  const id2idx = new Map<string, number>()
  steps.forEach((s, i) => id2idx.set(s.id, i))

  const indeg = new Array<number>(n).fill(0)
  const adj: number[][] = Array.from({ length: n }, () => [])
  steps.forEach((s, i) => {
    for (const d of deps[s.id] ?? []) {
      const di = id2idx.get(d)
      if (di === undefined) continue
      adj[di].push(i)
      indeg[i] += 1
    }
  })

  let cur: number[] = []
  for (let i = 0; i < n; i++) if (indeg[i] === 0) cur.push(i)

  const levels: string[][] = []
  let done = 0
  while (cur.length > 0) {
    cur.sort((a, b) => a - b)
    levels.push(cur.map((i) => steps[i].id))
    done += cur.length
    const next: number[] = []
    for (const u of cur) {
      for (const v of adj[u]) {
        indeg[v] -= 1
        if (indeg[v] === 0) next.push(v)
      }
    }
    cur = next
  }
  if (done !== n) throw new FlowGraphError('Flow 存在循环依赖')
  return levels
}

/** 推断出来的（未显式声明的）依赖边，供 UI 高亮「这条边是算出来的」。 */
export function inferredEdges(steps: StepLike[]): Array<{ from: string; to: string }> {
  const out: Array<{ from: string; to: string }> = []
  for (const s of steps) {
    const explicit = new Set(s.depends_on ?? [])
    const refs = new Set<string>()
    collectStepRefsFromValue(s.input, refs)
    collectWhenRefs(s.when, refs)
    for (const r of Array.from(refs).sort()) {
      if (!explicit.has(r) && r !== s.id) out.push({ from: r, to: s.id })
    }
  }
  return out
}

/** 结构不变量检查（镜像 model.Flow.Validate；isService 由 mode 推断）。 */
export function structureErrors(flow: { id?: string; steps?: StepLike[] | null }): string[] {
  const errs: string[] = []
  const steps = flow.steps ?? []
  if (!flow.id) errs.push('flow.id 不能为空')
  if (steps.length === 0) errs.push(`flow ${flow.id ?? ''} 至少需要一个 step`.trim())

  const ids = new Set(steps.map((s) => s.id))
  steps.forEach((s, i) => {
    if (!s.id) errs.push(`flow ${flow.id} 第 ${i} 个 step 缺少 id`)
    if (s.id && !s.call) errs.push(`step ${s.id} 的 call 不能为空（需指向已注册的 Tool）`)
    if (s.mode && s.mode !== 'once' && s.mode !== 'service') {
      errs.push(`step ${s.id} 的 mode=${JSON.stringify(s.mode)} 非法（应为 once 或 service）`)
    }
    if (s.when && s.when.includes('${')) {
      errs.push(
        `step ${s.id} 的 when 是表达式、不使用 \${} 模板语法：去掉 \${} 直接写变量名，如 \`steps.s1.output.n > 1\``,
      )
    }
    if (s.parallel && (s.depends_on ?? []).length > 0) {
      errs.push(
        `step ${s.id} 标记了 parallel 但同时声明了 depends_on：无依赖 step 本就并行调度，该标注在此处无意义且易误导`,
      )
    }
    for (const dep of s.depends_on ?? []) {
      if (!ids.has(dep)) errs.push(`step ${s.id} 依赖不存在的 step: ${dep}`)
    }
  })

  // 前端补充检查（非内核规则，故单独标注）：内核不强查 step id 重复，
  // 但重复 id 会让分层/落库语义含混，这里提示而非断言。
  const seen = new Set<string>()
  for (const s of steps) {
    if (s.id && seen.has(s.id)) errs.push(`[前端补充] step id 重复: ${s.id}（内核未校验，建议改掉）`)
    seen.add(s.id)
  }
  return errs
}

/**
 * service 依赖守卫（镜像 engine 的 REQ-SVC-DEP-GUARD）：
 * service step 永不结束，不能作为其它 step 的依赖，也不能混进 `flow run` 一次性执行。
 */
export function serviceGuardErrors(steps: StepLike[]): string[] {
  const errs: string[] = []
  const isService = (s: StepLike) => s.mode === 'service'
  let deps: Record<string, string[]> = {}
  try {
    deps = effectiveDeps(steps)
  } catch {
    return errs // 依赖图本身有问题，交给上层报错，这里不重复
  }
  const byId = new Map(steps.map((s) => [s.id, s]))
  for (const s of steps) {
    for (const d of deps[s.id] ?? []) {
      const dep = byId.get(d)
      if (dep && isService(dep)) {
        errs.push(`step ${s.id} 依赖了 service step ${d}：service 永不结束，不可被依赖`)
      }
    }
  }
  return errs
}
