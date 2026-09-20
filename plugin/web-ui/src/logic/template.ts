// 前端静态预检 —— **镜像** internal/engine/validate_static.go 的校验层次与文案。
//
// 目的（红线 10：能静态校验的不许留到运行期）：用户在表单里敲完就能看到内核会怎么判，
// 而不是提交后被打回。文案刻意与内核保持一致，便于对照（同一条规则只应有一套说法）。
//
// 分层（与内核 ValidateFlowStatic 同序；内核遇错即返回，故这里也按序短路取首批错误）：
//   1. 结构不变量（model.Flow.Validate）
//   2. 依赖图：有效依赖无环、无未知/自引用（engine.topoLevels）
//   3. 模板引用（engine.validateTemplates）
//   4. when 可编译 —— **前端不做**（expr-lang 在 Go 侧），仅提示
//   5. 引用的 Tool 必须已注册 + 钉版本必须存在
//
// 诚实标注：内核在**运行期**才判的规则（service 依赖守卫）在这里只作 warning，
// 不冒充注册期错误——否则会教出「以为内核会拦」的错误心智模型。

import {
  FlowGraphError,
  effectiveDeps,
  scanTemplateRefs,
  structureErrors,
  topoLevels,
  walkStrings,
  type StepLike,
} from './levels'

export interface TemplateRefError {
  step: string
  message: string
}

/** 模板引用静态校验（镜像 engine/template.go validateTemplates）。 */
export function validateTemplates(flow: {
  vars?: Record<string, unknown> | null
  steps?: StepLike[] | null
}): TemplateRefError[] {
  const errs: TemplateRefError[] = []
  const vars = flow.vars ?? {}
  for (const s of flow.steps ?? []) {
    walkStrings(s.input, (str) => {
      let refs
      try {
        refs = scanTemplateRefs(str)
      } catch (e) {
        errs.push({ step: s.id, message: (e as Error).message })
        return
      }
      for (const r of refs) {
        if (r.prefix === 'vars') {
          if (r.path.length === 0) {
            errs.push({ step: s.id, message: `vars 引用缺少键名: ${r.raw}` })
            continue
          }
          if (!(r.path[0] in vars)) {
            errs.push({ step: s.id, message: `引用 \${${r.raw}} 未定义的 vars 键: ${r.path[0]}` })
          }
        } else if (r.prefix === 'steps') {
          if (r.path.length === 0) errs.push({ step: s.id, message: `steps 引用缺少 step id: ${r.raw}` })
        } else if (r.prefix === 'secrets') {
          if (r.path.length === 0) errs.push({ step: s.id, message: `secrets 引用缺少密钥名: ${r.raw}` })
        } else {
          errs.push({ step: s.id, message: `未知引用前缀: ${r.prefix}` })
        }
      }
    })
  }
  return errs
}

export interface PrecheckResult {
  /** 内核**注册期**会拒绝的错误（POST /api/flows 会打回）。 */
  errors: string[]
  /** 内核运行期才判、或前端补充的提示（不阻断注册）。 */
  warnings: string[]
}

export interface CatalogLike {
  toolIds: Set<string>
  toolVersions: Map<string, string[]>
}

/**
 * 完整预检：errors 与内核 ValidateFlowStatic 同规则同序；warnings 是超出注册期范围的提示。
 * 注意 errors[0] 即内核会返回的那一条（内核遇错即返回）。
 */
export function precheckFlow(
  flow: { id?: string; vars?: Record<string, unknown> | null; steps?: StepLike[] | null },
  catalog?: CatalogLike,
): PrecheckResult {
  const steps = flow.steps ?? []
  const errors: string[] = []
  const warnings: string[] = []

  // 1. 结构不变量
  const structural = structureErrors({ id: flow.id, steps })
  if (structural.length > 0) {
    return { errors: structural, warnings }
  }

  // 2. 依赖图（环 / 未知 / 自引用）
  try {
    topoLevels(steps)
  } catch (e) {
    if (e instanceof FlowGraphError) return { errors: [e.message], warnings }
    throw e
  }

  // 3. 模板引用
  const tmplErrs = validateTemplates({ vars: flow.vars, steps })
  if (tmplErrs.length > 0) {
    return { errors: tmplErrs.map((e) => `step ${e.step}: ${e.message}`), warnings }
  }

  // 4. Tool 存在性 + 钉版本存在性
  if (catalog) {
    for (const s of steps) {
      if (!s.call) continue
      if (!catalog.toolIds.has(s.call)) {
        return { errors: [`step ${s.id} 引用的 Tool 不存在: ${s.call}`], warnings }
      }
      if (s.use_version) {
        const vs = catalog.toolVersions.get(s.call) ?? []
        if (!vs.includes(s.use_version)) {
          return {
            errors: [`step ${s.id} 钉了 use_version=${s.use_version}，但 Tool ${s.call} 不存在该版本`],
            warnings,
          }
        }
      }
    }
  } else {
    warnings.push('[前端补充] 未加载 Tool 目录，跳过了「Tool 存在性 / 钉版本存在性」检查')
  }

  // 运行期守卫（内核不在注册期判，故只提示）
  const deps = effectiveDeps(steps)
  const byId = new Map(steps.map((s) => [s.id, s]))
  for (const s of steps) {
    for (const d of deps[s.id] ?? []) {
      if (byId.get(d)?.mode === 'service') {
        warnings.push(`[运行期守卫] step ${s.id} 依赖了 service step ${d}：service 永不结束，内核会在运行时报错`)
      }
    }
  }
  if (steps.some((s) => s.mode === 'service')) {
    warnings.push(
      '[运行期守卫] 含 service step 的 Flow 不能用 `flow run` 一次性执行，须经 `app run` / POST /api/apps 监管',
    )
  }

  warnings.push('[内核规则] when 表达式的编译校验由内核 expr-lang 完成，前端只能校验「不含 ${}」与依赖引用')

  return { errors, warnings }
}
