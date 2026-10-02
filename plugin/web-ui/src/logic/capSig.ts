// 能力方法的 Lua 调用签名。
//
// 关键：cap.name **已带命名空间**——内置是 http / db / call…，扩展是 ext.ai / ext.redis…
// （extensions.go 把 extCaps 的 map 键 "ext.ai" 直接当能力名；ext.* 本就要求以 "ext." 前缀）。
// 所以拼签名时直接 ctx.<name>.<method>，**绝不能再补一个 ext.**——
// 否则 ext.ai.generate 会被拼成 ctx.ext.ext.ai.generate（历史 bug：抽屉标题与「复制签名」都错过）。
//
// 实参形态按 kind 分：ext.* 走单个具名 table（ctx.ext.ai.generate({...})），
// 内置原语走位置参数（ctx.http.Request(...)）——与 recipes/tool.yaml 钉死的入口契约一致。
import type { CapabilityDTO } from '@/api/types'

/** 只给调用基名 ctx.<name>.<method>（目录里的「复制签名」用它）。 */
export function capCallBase(name: string, method: string): string {
  return `ctx.${name}.${method}`
}

/** 带实参样式的完整签名；withArgs=false 时等价于 capCallBase（只给基名）。 */
export function capCallSig(
  cap: Pick<CapabilityDTO, 'name' | 'kind'>,
  method: string,
  withArgs = true,
): string {
  const base = capCallBase(cap.name, method)
  if (!withArgs) return base
  return cap.kind === 'ext' ? `${base}({...})` : `${base}(...)`
}
