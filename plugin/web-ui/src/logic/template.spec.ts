// 前端预检单测：断言它**与内核同规则**（文案也刻意一致，便于对照内核报错）。
import { describe, expect, it } from 'vitest'
import { precheckFlow, validateTemplates } from '@/logic/template'
import type { StepLike } from '@/logic/levels'

const step = (over: Partial<StepLike> & { id: string }): StepLike => ({ call: 't', ...over })

describe('validateTemplates（镜像 engine.validateTemplates）', () => {
  it('vars 键存在则通过', () => {
    expect(validateTemplates({ vars: { a: 1 }, steps: [step({ id: 's1', input: { v: '${vars.a}' } })] })).toEqual([])
  })

  it('vars 键不存在 → 报「未定义的 vars 键」', () => {
    const errs = validateTemplates({ vars: {}, steps: [step({ id: 's1', input: { v: '${vars.missing}' } })] })
    expect(errs).toEqual([{ step: 's1', message: '引用 ${vars.missing} 未定义的 vars 键: missing' }])
  })

  it('只校验首段：深层路径留给运行期（与内核边界一致）', () => {
    expect(validateTemplates({ vars: { a: 1 }, steps: [step({ id: 's1', input: { v: '${vars.a.b.c}' } })] })).toEqual([])
  })

  it('${vars} 缺键名被拒', () => {
    const errs = validateTemplates({ vars: { a: 1 }, steps: [step({ id: 's1', input: { v: '${vars}' } })] })
    expect(errs[0].message).toBe('vars 引用缺少键名: vars')
  })

  it('${steps} 缺 step id 被拒', () => {
    const errs = validateTemplates({ vars: {}, steps: [step({ id: 's1', input: { v: '${steps}' } })] })
    expect(errs[0].message).toBe('steps 引用缺少 step id: steps')
  })

  it('${secrets} 缺密钥名被拒', () => {
    const errs = validateTemplates({ vars: {}, steps: [step({ id: 's1', input: { v: '${secrets}' } })] })
    expect(errs[0].message).toBe('secrets 引用缺少密钥名: secrets')
  })

  it('secrets 前缀本身是允许的（密钥缺失属运行期，不在注册期判）', () => {
    expect(validateTemplates({ vars: {}, steps: [step({ id: 's1', input: { v: '${secrets.token}' } })] })).toEqual([])
  })

  it('未知前缀被拒（红线 15：枚举不得静默扩展）', () => {
    const errs = validateTemplates({ vars: {}, steps: [step({ id: 's1', input: { v: '${env.HOME}' } })] })
    expect(errs[0].message).toBe('未知引用前缀: env')
  })

  it('模板未闭合被拒', () => {
    const errs = validateTemplates({ vars: {}, steps: [step({ id: 's1', input: { v: '${vars.a' } })] })
    expect(errs[0].message).toMatch(/模板未闭合/)
  })

  it('嵌套值与数组里的模板都会被扫到', () => {
    const errs = validateTemplates({
      vars: {},
      steps: [step({ id: 's1', input: { list: [{ deep: '${vars.nope}' }] } })],
    })
    expect(errs).toHaveLength(1)
  })
})

describe('precheckFlow（镜像 ValidateFlowStatic 的分层与短路顺序）', () => {
  it('结构错误优先（第一层就先返回，不继续查图/模板/Tool）', () => {
    const r = precheckFlow({ id: '', steps: [] })
    expect(r.errors).toEqual(['flow.id 不能为空', 'flow  至少需要一个 step'])
  })

  it('结构 OK 但依赖图有环 → 只报环', () => {
    const r = precheckFlow({
      id: 'f',
      steps: [step({ id: 'a', depends_on: ['b'] }), step({ id: 'b', depends_on: ['a'] })],
    })
    expect(r.errors).toEqual(['Flow 存在循环依赖'])
  })

  it('图 OK 但模板引用坏 → 报模板（带 step 上下文）', () => {
    const r = precheckFlow({ id: 'f', vars: {}, steps: [step({ id: 's1', input: { v: '${vars.nope}' } })] })
    expect(r.errors).toEqual(['step s1: 引用 ${vars.nope} 未定义的 vars 键: nope'])
  })

  it('给了 Tool 目录 → 校验 Tool 存在性', () => {
    const catalog = { toolIds: new Set(['t']), toolVersions: new Map<string, string[]>() }
    const r = precheckFlow({ id: 'f', steps: [step({ id: 's1', call: 'ghost' })] }, catalog)
    expect(r.errors).toEqual(['step s1 引用的 Tool 不存在: ghost'])
  })

  it('给了 Tool 目录 → 校验钉版本存在性', () => {
    const catalog = { toolIds: new Set(['t']), toolVersions: new Map([['t', ['1.0.0']]]) }
    const r = precheckFlow({ id: 'f', steps: [step({ id: 's1', call: 't', use_version: '9.9.9' })] }, catalog)
    expect(r.errors).toEqual(['step s1 钉了 use_version=9.9.9，但 Tool t 不存在该版本'])
  })

  it('没有 Tool 目录时降级为 warning，而不是假装通过', () => {
    const r = precheckFlow({ id: 'f', steps: [step({ id: 's1', call: 'ghost' })] })
    expect(r.errors).toEqual([])
    expect(r.warnings.some((w) => w.includes('未加载 Tool 目录'))).toBe(true)
  })

  it('含 service step → 运行期守卫提示（不是注册期错误）', () => {
    const r = precheckFlow({ id: 'f', steps: [step({ id: 'svc', mode: 'service' })] })
    expect(r.errors).toEqual([])
    expect(r.warnings.some((w) => w.includes('service step 的 Flow 不能用'))).toBe(true)
  })

  it('依赖 service step → 运行期守卫提示', () => {
    const r = precheckFlow({
      id: 'f',
      steps: [step({ id: 'svc', mode: 'service' }), step({ id: 'b', depends_on: ['svc'] })],
    })
    expect(r.warnings.some((w) => w.includes('依赖了 service step svc'))).toBe(true)
  })

  it('干净定义：无 errors（warnings 只含内核侧说明）', () => {
    const catalog = { toolIds: new Set(['t']), toolVersions: new Map([['t', ['1.0.0']]]) }
    const r = precheckFlow(
      {
        id: 'f',
        vars: { n: 1 },
        steps: [
          step({ id: 'a', call: 't', input: { v: '${vars.n}' } }),
          step({ id: 'b', call: 't', when: 'steps.a.output.ok', input: { v: '${steps.a.output.v}' } }),
        ],
      },
      catalog,
    )
    expect(r.errors).toEqual([])
    expect(r.warnings.some((w) => w.includes('expr-lang'))).toBe(true)
  })
})
