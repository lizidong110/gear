// 纯逻辑单测：依赖推断与分层（必须与 internal/engine/deps.go 的语义一致）。
//
// 这些断言不是「跑通就行」—— 它们锁的是**内核语义**：
// 漏扫 when 的引用、把推断边丢掉、环检测失效，任何一条都会让 UI 显示的编排图
// 与内核实际调度不一致，而「图不对」比「没有图」更糟。
import { describe, expect, it } from 'vitest'
import {
  FlowGraphError,
  collectWhenRefs,
  effectiveDeps,
  inferredEdges,
  scanTemplateRefs,
  serviceGuardErrors,
  structureErrors,
  topoLevels,
  type StepLike,
} from '@/logic/levels'

const step = (over: Partial<StepLike> & { id: string }): StepLike => ({ call: 't', ...over })

describe('scanTemplateRefs（模板解析器）', () => {
  it('切分出前缀与路径，并去掉首尾空白', () => {
    const refs = scanTemplateRefs('前 ${ steps.s1.output.n } 后')
    expect(refs).toHaveLength(1)
    expect(refs[0].prefix).toBe('steps')
    expect(refs[0].path).toEqual(['s1', 'output', 'n'])
    expect(refs[0].raw).toBe('steps.s1.output.n')
  })

  it('一个字符串里多个引用', () => {
    expect(scanTemplateRefs('${vars.a}-${vars.b}').map((r) => r.raw)).toEqual(['vars.a', 'vars.b'])
  })

  it('未闭合必须报错（与运行期展开同一解释）', () => {
    expect(() => scanTemplateRefs('${vars.a')).toThrowError(/模板未闭合/)
  })

  it('空引用 token 的前缀为空串', () => {
    const refs = scanTemplateRefs('${}')
    expect(refs[0].prefix).toBe('')
    expect(refs[0].path).toEqual([])
  })
})

describe('stripQuoted / collectWhenRefs', () => {
  it('字符串字面量里的 steps.x 不算引用', () => {
    const seen = new Set<string>()
    collectWhenRefs('name == "steps.nope"', seen)
    expect([...seen]).toEqual([])
  })

  it('真实引用会被收集，且工具名不误判（mysteps.x 不算）', () => {
    const seen = new Set<string>()
    collectWhenRefs('steps.a.output.n > 1 && mysteps.b.x == 0', seen)
    expect([...seen]).toEqual(['a'])
  })

  it('空 when 直接跳过', () => {
    const seen = new Set<string>()
    collectWhenRefs('', seen)
    collectWhenRefs(undefined, seen)
    expect(seen.size).toBe(0)
  })
})

describe('effectiveDeps（有效依赖 = 显式 ∪ 推断）', () => {
  it('显式 depends_on 顺序被保留', () => {
    const deps = effectiveDeps([step({ id: 'a' }), step({ id: 'b' }), step({ id: 'c', depends_on: ['b', 'a'] })])
    expect(deps.c).toEqual(['b', 'a'])
  })

  it('input 里的 ${steps.X} 变成隐式依赖（并行下的时序保障）', () => {
    const deps = effectiveDeps([
      step({ id: 'a' }),
      step({ id: 'b', input: { v: '${steps.a.output.n}' } }),
    ])
    expect(deps.b).toEqual(['a'])
  })

  it('嵌套结构里的引用同样被扫到', () => {
    const deps = effectiveDeps([
      step({ id: 'a' }),
      step({ id: 'b', input: { list: [{ deep: '${steps.a.output.k}' }] } }),
    ])
    expect(deps.b).toEqual(['a'])
  })

  it('when 里的 steps.X 也进依赖（内核曾因此踩坑，是本文件最重要的断言）', () => {
    const deps = effectiveDeps([step({ id: 'a' }), step({ id: 'b', when: 'steps.a.output.n > 1' })])
    expect(deps.b).toEqual(['a'])
  })

  it('显式 + 推断去重，推断边按字典序稳定', () => {
    const deps = effectiveDeps([
      step({ id: 'a' }),
      step({ id: 'b' }),
      step({ id: 'c', depends_on: ['a'], input: { x: '${steps.b.output.v}', y: '${steps.a.output.v}' } }),
    ])
    expect(deps.c).toEqual(['a', 'b'])
  })

  it('引用不存在的 step 报错', () => {
    expect(() => effectiveDeps([step({ id: 'a', input: { v: '${steps.ghost.output.x}' } })])).toThrowError(
      /step a 引用不存在的 step: ghost/,
    )
  })

  it('显式依赖不存在的 step 报错（带「依赖」字样以区分来源）', () => {
    expect(() => effectiveDeps([step({ id: 'a', depends_on: ['ghost'] })])).toThrowError(
      /step a 依赖不存在的 step: ghost/,
    )
  })

  it('自引用报错', () => {
    expect(() => effectiveDeps([step({ id: 'a', when: 'steps.a.output.n > 1' })])).toThrowError(
      /step a 不能依赖\/引用自身/,
    )
  })

  it('input 模板未闭合时，错误被包上 step 上下文', () => {
    expect(() => effectiveDeps([step({ id: 'a', input: { v: '${steps.b' } })])).toThrowError(
      /step a input 模板解析失败: 模板未闭合/,
    )
  })
})

describe('topoLevels（Kahn 分层）', () => {
  it('无依赖的 step 落在同一层（可并行）', () => {
    const levels = topoLevels([step({ id: 'a' }), step({ id: 'b' }), step({ id: 'c', depends_on: ['a', 'b'] })])
    expect(levels).toEqual([['a', 'b'], ['c']])
  })

  it('推断出的依赖也影响分层', () => {
    const levels = topoLevels([
      step({ id: 'a' }),
      step({ id: 'b', input: { v: '${steps.a.output.n}' } }),
      step({ id: 'c', when: 'steps.b.output.ok' }),
    ])
    expect(levels).toEqual([['a'], ['b'], ['c']])
  })

  it('层内按原下标升序（布局稳定，刷新不跳动）', () => {
    const levels = topoLevels([step({ id: 'z' }), step({ id: 'm' }), step({ id: 'a' })])
    expect(levels[0]).toEqual(['z', 'm', 'a'])
  })

  it('长链按依赖逐层展开', () => {
    const levels = topoLevels([
      step({ id: 'a' }),
      step({ id: 'b', depends_on: ['a'] }),
      step({ id: 'c', depends_on: ['b'] }),
    ])
    expect(levels).toEqual([['a'], ['b'], ['c']])
  })

  it('检测到环时报错', () => {
    expect(() =>
      topoLevels([step({ id: 'a', depends_on: ['b'] }), step({ id: 'b', depends_on: ['a'] })]),
    ).toThrowError(/Flow 存在循环依赖/)
  })

  it('抛出的错误类型是 FlowGraphError（调用方据以区分「图的问题」）', () => {
    try {
      topoLevels([step({ id: 'a', depends_on: ['a'] })])
      throw new Error('应当抛错')
    } catch (e) {
      expect(e).toBeInstanceOf(FlowGraphError)
    }
  })
})

describe('inferredEdges（图里要画成虚线的边）', () => {
  it('只返回未显式声明的引用边', () => {
    const edges = inferredEdges([
      step({ id: 'a' }),
      step({ id: 'b' }),
      step({ id: 'c', depends_on: ['a'], input: { x: '${steps.b.output.v}' } }),
    ])
    expect(edges).toEqual([{ from: 'b', to: 'c' }])
  })

  it('when 的边也算推断边', () => {
    const edges = inferredEdges([step({ id: 'a' }), step({ id: 'b', when: 'steps.a.output.ok' })])
    expect(edges).toEqual([{ from: 'a', to: 'b' }])
  })
})

describe('structureErrors（镜像 model.Flow.Validate）', () => {
  it('缺 id / 无 step / step 缺 call', () => {
    expect(structureErrors({ id: '', steps: [] })).toEqual([
      'flow.id 不能为空',
      'flow  至少需要一个 step',
    ])
    expect(structureErrors({ id: 'f', steps: [{ id: 's1', call: '' }] })).toEqual([
      'step s1 的 call 不能为空（需指向已注册的 Tool）',
    ])
  })

  it('非法 mode 被拒', () => {
    const errs = structureErrors({ id: 'f', steps: [step({ id: 's1', mode: 'daemon' })] })
    expect(errs.some((e) => e.includes('mode=') && e.includes('非法'))).toBe(true)
  })

  it('when 里出现 ${} 被拒（并教正确写法）', () => {
    const errs = structureErrors({ id: 'f', steps: [step({ id: 's1', when: '${steps.a.output.n} > 1' })] })
    expect(errs.some((e) => e.includes('when 是表达式') && e.includes('不使用 ${}'))).toBe(true)
  })

  it('parallel 与 depends_on 并存属误导性标注', () => {
    const errs = structureErrors({ id: 'f', steps: [step({ id: 's1', parallel: true, depends_on: ['s2'] }), step({ id: 's2' })] })
    expect(errs.some((e) => e.includes('parallel') && e.includes('易误导'))).toBe(true)
  })

  it('depends_on 指向不存在的 step', () => {
    const errs = structureErrors({ id: 'f', steps: [step({ id: 's1', depends_on: ['ghost'] })] })
    expect(errs).toContain('step s1 依赖不存在的 step: ghost')
  })

  it('step id 重复被标为「前端补充」（内核不查这一条）', () => {
    const errs = structureErrors({ id: 'f', steps: [step({ id: 's1' }), step({ id: 's1' })] })
    expect(errs.some((e) => e.startsWith('[前端补充] step id 重复'))).toBe(true)
  })
})

describe('serviceGuardErrors（运行期守卫的提示）', () => {
  it('依赖 service step 时给出提示', () => {
    const errs = serviceGuardErrors([step({ id: 'svc', mode: 'service' }), step({ id: 'b', depends_on: ['svc'] })])
    expect(errs).toEqual(['step b 依赖了 service step svc：service 永不结束，不可被依赖'])
  })

  it('依赖图本身有错时不重复报错（交给上层）', () => {
    expect(serviceGuardErrors([step({ id: 'a', depends_on: ['ghost'] })])).toEqual([])
  })
})
