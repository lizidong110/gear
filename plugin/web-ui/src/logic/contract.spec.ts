// 契约与错误映射单测。
import { describe, expect, it } from 'vitest'
import { ApiError, extractError, hintForStatus } from '@/api/client'
import {
  BUILTIN_CAPABILITIES,
  FLOW_LEVEL_STAGES,
  HOOK_STAGES,
  IMPLEMENTED_RUNTIMES,
  PROBE_KINDS,
  RUNTIMES,
  STEP_MODES,
  VERSION_STATES,
  VERSION_TRANSITIONS,
} from '@/logic/generated/enums'
import { CAPABILITY_LABEL } from '@/logic/enums'
import { capabilityKnown, hookErrors, isSemver, toolErrors } from '@/logic/tool'
import type { ToolDTO } from '@/api/types'

describe('hintForStatus（状态码 → 行动指引）', () => {
  it('每个内核语义码都有专门说明，不落进兜底', () => {
    for (const code of [0, 400, 404, 409, 422, 429, 500]) {
      expect(hintForStatus(code)).not.toBe('请求失败')
    }
  })

  it('409 必须点明「被引用不可注销」——这是内核 §8.2 守卫，不是故障', () => {
    expect(hintForStatus(409)).toContain('不可注销')
  })

  it('429 必须点明背压语义（并发闸门拒绝）', () => {
    expect(hintForStatus(429)).toContain('背压')
  })

  it('未知码走兜底', () => {
    expect(hintForStatus(418)).toBe('请求失败')
  })
})

describe('extractError（内核错误体 {"error": "..."}）', () => {
  it('取 error 字段', () => {
    expect(extractError('{"error":"step s1 引用的 Tool 不存在: nope"}')).toBe(
      'step s1 引用的 Tool 不存在: nope',
    )
  })

  it('非 JSON 回退原文', () => {
    expect(extractError('plain failure')).toBe('plain failure')
  })

  it('空体返回 null', () => {
    expect(extractError('')).toBeNull()
  })

  it('超长原文被截断（避免把整个页面塞进通知）', () => {
    const out = extractError('x'.repeat(900))
    expect(out).not.toBeNull()
    expect((out as string).length).toBeLessThanOrEqual(501)
  })
})

describe('ApiError', () => {
  it('保留内核原文并附带 hint', () => {
    const e = new ApiError(400, 'mode=daemon 非法')
    expect(e.message).toBe('mode=daemon 非法')
    expect(e.status).toBe(400)
    expect(e.hint).toContain('注册期')
  })
})

describe('生成枚举的结构不变量（防漂移，对应内核 TestRuntimeKnownImplemented）', () => {
  it('已实现 runtime ⊆ 已知 runtime', () => {
    for (const r of IMPLEMENTED_RUNTIMES) expect(RUNTIMES).toContain(r)
  })

  it('wasm 已知但未实现（deferred V3）', () => {
    expect(RUNTIMES).toContain('wasm')
    expect(IMPLEMENTED_RUNTIMES).not.toContain('wasm')
  })

  it('Flow 级 stage ⊆ 已实现 stage', () => {
    for (const s of FLOW_LEVEL_STAGES) expect(HOOK_STAGES).toContain(s)
  })

  it('枚举值与内核一致（改 Go 不同步本产物时 check:contract 会失败）', () => {
    expect([...RUNTIMES]).toEqual(['process', 'lua', 'go-host', 'wasm'])
    expect([...VERSION_STATES]).toEqual(['draft', 'testing', 'stable', 'deprecated', 'rejected'])
    expect([...HOOK_STAGES]).toEqual([
      'before_flow',
      'before_step',
      'after_step',
      'on_error',
      'on_success',
      'before_promote',
    ])
    expect([...PROBE_KINDS]).toEqual(['http', 'tcp', 'cmd'])
    expect([...STEP_MODES]).toEqual(['once', 'service'])
    // 刻意**不**再把完整清单写死在这里：清单由内核 cap.KnownPrimitives 生成，
    // 与内核的一致性由 scripts/check-contract.mjs 守（本文件写死只会每次加原语都红一次）。
    // 这里只断言「该有的都在」，真正要守的是下面那条——每个原语都得有中文标签。
    for (const c of ['http', 'call', 'db', 'secret', 'event', 'subscribe', 'config']) {
      expect([...BUILTIN_CAPABILITIES]).toContain(c)
    }
    // 新增原语而忘了补 CAPABILITY_LABEL，UI 上就是一片空白——这条替人拦住它。
    for (const c of BUILTIN_CAPABILITIES) expect(CAPABILITY_LABEL[c]).toBeTruthy()
  })

  it('版本状态机：rejected 仅可重开为 draft（终态语义）', () => {
    expect(VERSION_TRANSITIONS.rejected).toEqual(['draft'])
    expect(VERSION_TRANSITIONS.draft).toContain('stable')
    expect(VERSION_TRANSITIONS.stable).toContain('deprecated')
  })
})

describe('isSemver（镜像 Go semverRegex）', () => {
  it('接受 x.y.z / 前置 v / 预发布 / 构建元数据', () => {
    for (const v of ['1.0.0', 'v0.1.2', '1.2.3-beta.1', '1.0.0+build.7']) expect(isSemver(v)).toBe(true)
  })

  it('拒绝两位段、缺段、前导零', () => {
    for (const v of ['1.0', '1.0.0.0', '01.0.0', 'v1', 'latest', '']) expect(isSemver(v)).toBe(false)
  })
})

describe('capabilityKnown', () => {
  it('内置原语放行', () => {
    for (const c of BUILTIN_CAPABILITIES) expect(capabilityKnown(c)).toBe(true)
  })
  it('ext.* 放行（启动期注册的扩展原语）', () => {
    expect(capabilityKnown('ext.ai')).toBe(true)
    expect(capabilityKnown('ext.db')).toBe(true)
  })
  it('其它一律拒（未知枚举即拒，红线 15）', () => {
    expect(capabilityKnown('fs')).toBe(false)
    expect(capabilityKnown('ext')).toBe(false)
  })
})

const baseTool = (over: Partial<ToolDTO>): ToolDTO => ({
  id: 't',
  name: '',
  description: '',
  runtime: 'process',
  entry: 'echo',
  input_schema: { type: 'object' },
  output_schema: { type: 'object' },
  versions: [{ version: '1.0.0', state: 'draft', digest: '', note: '', created_at: '' }],
  capabilities: [],
  health_probe: null,
  ...over,
})

describe('toolErrors（镜像 model.Tool.Validate）', () => {
  it('合法 Tool 无错', () => {
    expect(toolErrors(baseTool({}))).toEqual([])
  })

  it('未知 runtime 被拒', () => {
    expect(toolErrors(baseTool({ runtime: 'foobar' }))[0]).toMatch(/runtime="foobar" 非法/)
  })

  it('已实现集之外的 runtime（wasm）被拒，并说明原因', () => {
    const errs = toolErrors(baseTool({ runtime: 'wasm' }))
    expect(errs[0]).toMatch(/尚未实现/)
    expect(errs[0]).toMatch(/wasm 计划 V3/)
  })

  it('没有 version 被拒', () => {
    expect(toolErrors(baseTool({ versions: [] }))[0]).toMatch(/至少要有一个 version/)
  })

  it('非法 SemVer / 非法状态被拒（带第几个 version）', () => {
    const errs = toolErrors(
      baseTool({ versions: [{ version: '1.0', state: 'ok', digest: '', note: '', created_at: '' }] }),
    )
    expect(errs[0]).toMatch(/第 0 个 version 校验失败/)
    expect(errs[0]).toMatch(/非法的 SemVer/)
    expect(errs[0]).toMatch(/非法的版本状态/)
  })

  it('schema 必须是 type:object', () => {
    expect(toolErrors(baseTool({ input_schema: { type: 'string' } }))[0]).toBe('input_schema.type 必须是 object')
    expect(toolErrors(baseTool({ output_schema: null }))[0]).toBe('output_schema 不能为空')
  })

  it('未知能力原语被拒', () => {
    expect(toolErrors(baseTool({ capabilities: ['fs'] }))[0]).toMatch(/未知的能力原语/)
  })

  it('health_probe：tcp 缺端口 / cmd 缺命令', () => {
    const e1 = toolErrors(
      baseTool({ health_probe: { kind: 'tcp', addr: '127.0.0.1', path: '', cmd: '', interval: '', timeout: '', threshold: 0 } }),
    )
    expect(e1.some((e) => e.includes('缺少端口'))).toBe(true)

    const e2 = toolErrors(
      baseTool({ health_probe: { kind: 'cmd', addr: '', path: '', cmd: '', interval: '', timeout: '', threshold: 0 } }),
    )
    expect(e2.some((e) => e.includes('health_probe.cmd 为空'))).toBe(true)
  })

  it('health_probe：时长格式非法被拒', () => {
    const errs = toolErrors(
      baseTool({ health_probe: { kind: 'http', addr: 'a:1', path: '/', cmd: '', interval: '5 seconds', timeout: '', threshold: 0 } }),
    )
    expect(errs.some((e) => e.includes('health_probe.interval'))).toBe(true)
  })

  it('api：method 大小写 / path 前缀 / 占位符 / 内核资源根', () => {
    expect(toolErrors(baseTool({ api: { method: 'GET', path: '/api/nodes/{id}' } }))).toEqual([])

    const lower = toolErrors(baseTool({ api: { method: 'get', path: '/api/nodes/{id}' } }))
    expect(lower.some((e) => e.includes('api.method'))).toBe(true)

    const noPrefix = toolErrors(baseTool({ api: { method: 'GET', path: 'nodes/{id}' } }))
    expect(noPrefix.some((e) => e.includes('必须以 "/api/" 开头'))).toBe(true)

    const noName = toolErrors(baseTool({ api: { method: 'GET', path: '/api/nodes/{}' } }))
    expect(noName.some((e) => e.includes('占位符要写名字'))).toBe(true)

    const reserved = toolErrors(baseTool({ api: { method: 'GET', path: '/api/tools/{id}' } }))
    expect(reserved.some((e) => e.includes('内核已占用'))).toBe(true)
  })
})

describe('hookErrors（镜像 model.Hook.Validate）', () => {
  it('合法 Hook 无错', () => {
    expect(hookErrors({ id: 'h', target: '*', stage: 'before_step', action: 'auth', order: 0 })).toEqual([])
  })

  it('id / target / action 为空分别报错', () => {
    expect(hookErrors({})).toContain('hook.id 不能为空')
    expect(hookErrors({ id: 'h' }).some((e) => e.includes('target 不能为空'))).toBe(true)
    expect(hookErrors({ id: 'h', target: '*' }).some((e) => e.includes('action 不能为空'))).toBe(true)
  })

  it('非法 stage / criticality 被拒', () => {
    expect(hookErrors({ id: 'h', target: '*', action: 'a', stage: 'before_all' })[0]).toMatch(/stage=/)
    expect(
      hookErrors({ id: 'h', target: '*', action: 'a', stage: 'before_step', criticality: 'ignore' })[0],
    ).toMatch(/criticality=/)
  })

  it('留空 criticality 是合法的（按 critical 处理）', () => {
    expect(hookErrors({ id: 'h', target: '*', action: 'a', stage: 'before_step', criticality: '' })).toEqual([])
  })
})
