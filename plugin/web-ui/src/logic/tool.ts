// Tool / Hook 的前端预检 —— **镜像** internal/model/validate.go 的文案。
// 目的同上：表单里敲完就知道内核会不会收，不必等 POST 打回。

import {
  BUILTIN_CAPABILITIES,
  HOOK_STAGES,
  IMPLEMENTED_RUNTIMES,
  PROBE_KINDS,
  RUNTIMES,
  VERSION_STATES,
} from './enums'
import type { HealthProbeDTO, HookDTO, ToolDTO, ToolVersionDTO } from '@/api/types'

// 与 Go 的 semverRegex 等价（Go 用 RE2，该模式与 JS 正则语义一致）。
const SEMVER_RE =
  /^v?(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-((?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*)(?:\.(?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*))*))?(?:\+([0-9a-zA-Z-]+(?:\.[0-9a-zA-Z-]+)*))?$/

/** Go time.ParseDuration 接受的形态（简化：数字 + 单位）。 */
const DURATION_RE = /^\d+(\.\d+)?(ns|us|µs|ms|s|m|h)$/

export function isSemver(s: string): boolean {
  return SEMVER_RE.test(s)
}

function schemaError(s: unknown, field: string): string | null {
  if (s === null || s === undefined) return `${field} 不能为空`
  if (typeof s !== 'object' || Array.isArray(s)) return `${field} 必须是 JSON 对象`
  const t = (s as Record<string, unknown>).type
  if (String(t) !== 'object') return `${field}.type 必须是 object`
  return null
}

/** capability 是否被内核接受：6 个内置原语，或启动期注册的 ext.* 扩展名。 */
export function capabilityKnown(name: string): boolean {
  return (BUILTIN_CAPABILITIES as readonly string[]).includes(name) || name.startsWith('ext.')
}

/** 镜像 model.Tool.Validate（含 runtime「已知」与「已实现」两道闸门）。 */
export function toolErrors(t: Partial<ToolDTO>): string[] {
  const errs: string[] = []
  if (!t.id) errs.push('tool.id 不能为空')

  const rt = t.runtime ?? ''
  if (!(RUNTIMES as readonly string[]).includes(rt)) {
    errs.push(`tool ${t.id} 的 runtime=${JSON.stringify(rt)} 非法（应为 ${RUNTIMES.join('/')}）`)
  } else if (!(IMPLEMENTED_RUNTIMES as readonly string[]).includes(rt)) {
    errs.push(
      `tool ${t.id} 的 runtime=${JSON.stringify(rt)} 尚未实现（已实现：${IMPLEMENTED_RUNTIMES.join(
        ' / ',
      )}；wasm 计划 V3）；请勿注册跑不起来的 Tool`,
    )
  }

  const versions = t.versions ?? []
  if (versions.length === 0) errs.push(`tool ${t.id} 至少要有一个 version`)
  versions.forEach((v: ToolVersionDTO, i: number) => {
    const sub: string[] = []
    if (!isSemver(v.version ?? '')) {
      sub.push(`非法的 SemVer 版本: ${JSON.stringify(v.version)}（应为 x.y.z，可带前置 v 与预发布/构建元数据）`)
    }
    if (!(VERSION_STATES as readonly string[]).includes(v.state)) {
      sub.push(`非法的版本状态: ${JSON.stringify(v.state)}（应为 ${VERSION_STATES.join('/')}）`)
    }
    if (sub.length > 0) errs.push(`tool ${t.id} 第 ${i} 个 version 校验失败: ${sub.join('；')}`)
  })

  for (const c of t.capabilities ?? []) {
    if (!capabilityKnown(c)) {
      errs.push(
        `tool ${t.id}: 未知的能力原语 ${JSON.stringify(c)}（应为内置 ${BUILTIN_CAPABILITIES.join(
          '/',
        )}，或 ext.* 扩展名）`,
      )
    }
  }

  const si = schemaError(t.input_schema, 'input_schema')
  if (si) errs.push(si)
  const so = schemaError(t.output_schema, 'output_schema')
  if (so) errs.push(so)

  errs.push(...probeErrors(t.health_probe, t.id ?? ''))
  return errs
}

/** 镜像 model.HealthProbe.Resolve。 */
export function probeErrors(p: HealthProbeDTO | null | undefined, toolId: string): string[] {
  if (!p || !p.kind) return []
  const errs: string[] = []
  if (!(PROBE_KINDS as readonly string[]).includes(p.kind)) {
    errs.push(`tool ${toolId} 的 health_probe.kind=${JSON.stringify(p.kind)} 非法（应为 ${PROBE_KINDS.join('/')}）`)
  }
  if (p.interval && !DURATION_RE.test(p.interval)) {
    errs.push(`tool ${toolId} 的 health_probe.interval=${JSON.stringify(p.interval)} 非法（如 5s / 500ms）`)
  }
  if (p.timeout && !DURATION_RE.test(p.timeout)) {
    errs.push(`tool ${toolId} 的 health_probe.timeout=${JSON.stringify(p.timeout)} 非法（如 2s）`)
  }
  const addr = p.addr || '127.0.0.1'
  if ((p.kind === 'http' || p.kind === 'tcp') && !addr.includes(':')) {
    errs.push(`tool ${toolId} 的 health_probe.addr=${JSON.stringify(addr)} 缺少端口（http/tcp 需 host:port）`)
  }
  if (p.kind === 'cmd' && !p.cmd) {
    errs.push(`tool ${toolId} 的 health_probe.cmd 为空：cmd 探针需提供健康检查命令`)
  }
  return errs
}

/** 镜像 model.Hook.Validate。 */
export function hookErrors(h: Partial<HookDTO>): string[] {
  const errs: string[] = []
  if (!h.id) errs.push('hook.id 不能为空')
  if (!h.target) errs.push(`hook ${h.id} 的 target 不能为空（应为 tool id / flow id 或 "*"）`)
  if (!h.action) errs.push(`hook ${h.id} 的 action 不能为空（应指向已注册的 Tool id 或 Flow id）`)
  if (h.stage && !(HOOK_STAGES as readonly string[]).includes(h.stage)) {
    errs.push(
      `hook ${h.id} 的 stage=${JSON.stringify(h.stage)} 非法（应为 ${HOOK_STAGES.join(' / ')}）`,
    )
  }
  if (!h.stage) errs.push(`hook ${h.id} 的 stage 不能为空`)
  if (h.criticality && h.criticality !== 'critical' && h.criticality !== 'best-effort') {
    errs.push(
      `hook ${h.id} 的 criticality=${JSON.stringify(h.criticality)} 非法（应为 critical / best-effort，留空默认 critical）`,
    )
  }
  return errs
}
