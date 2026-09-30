// 枚举单一来源（TS 侧）。
//
// 红线 15（枚举式声明必须逐项登记）在前端的落点：本文件是所有「合法取值集合」的
// 唯一登记处，scripts/check-contract.mjs 会把它与 internal/model/model.go 逐项比对，
// 任何一侧新增/改名而未同步另一侧，守卫即失败（防「文档说有、代码无」漂移）。
//
// 注意：本文件的**值**必须与 Go 侧完全一致（大小写、连字符）；**标签**仅是展示文案。

import { RUNTIMES, IMPLEMENTED_RUNTIMES, BUILTIN_CAPABILITIES } from './generated/enums'

export { RUNTIMES, IMPLEMENTED_RUNTIMES, BUILTIN_CAPABILITIES }

export type Runtime = (typeof RUNTIMES)[number]

/** 已知但引擎仍未实现的 runtime（注册期必被内核拒绝，UI 里显式置灰）。 */
export const UNIMPLEMENTED_RUNTIMES: readonly string[] = RUNTIMES.filter(
  (r) => !(IMPLEMENTED_RUNTIMES as readonly string[]).includes(r),
)

export const RUNTIME_LABEL: Record<string, string> = {
  process: 'process · 外部进程（stdin JSON → stdout 末行 JSON）',
  lua: 'lua · 嵌入式沙箱（once 可用，不支持 service）',
  'go-host': 'go-host · 进程内 Go 函数（once 可用，不支持 service）',
  wasm: 'wasm · WebAssembly（未实现，注册会被拒）',
}

/** Tool 版本生命周期状态（PRD §7）。 */
export { VERSION_STATES, VERSION_TRANSITIONS } from './generated/enums'

export const STATE_LABEL: Record<string, string> = {
  draft: '草稿',
  testing: '测试中',
  stable: '稳定',
  deprecated: '已弃用',
  rejected: '已否决',
}

/** Hook 阶段（PRD §5.3）。 */
export { HOOK_STAGES, FLOW_LEVEL_STAGES } from './generated/enums'

export const STAGE_LABEL: Record<string, string> = {
  before_flow: 'before_flow · Flow 前（可改写初始 vars）',
  before_step: 'before_step · 步骤前（可改写 input）',
  after_step: 'after_step · 步骤后（可改写 output）',
  on_error: 'on_error · 步骤失败（不可恢复主流程）',
  on_success: 'on_success · Flow 全成功（仅副作用）',
  before_promote: 'before_promote · 版本晋升前（门禁）',
}

export const CRITICALITIES = ['critical', 'best-effort'] as const

export const CRITICALITY_LABEL: Record<string, string> = {
  critical: 'critical · 失败即终止（留空默认）',
  'best-effort': 'best-effort · 失败仅告警，主流程继续',
}

/** service 模式 Tool 的健康探针类型（REQ-APP-HEALTH / #75）。 */
export { PROBE_KINDS, STEP_MODES } from './generated/enums'

export const PROBE_KIND_LABEL: Record<string, string> = {
  http: 'http · GET addr/path，状态码 < 400 视为健康',
  tcp: 'tcp · 拨号 host:port 成功视为健康',
  cmd: 'cmd · sh -c 执行命令，退出码 0 视为健康',
}

export const STEP_MODE_LABEL: Record<string, string> = {
  once: 'once · 执行完返回（默认）',
  service: 'service · 长驻不等待（仅 process runtime 支持）',
}

/**
 * 宿主能力原语（PRD §6，最小权限）。
 * 以下 7 个是内核内置（由 cap.KnownPrimitives 生成）；`ext.*` 是启动期由 extensions.yaml
 * 注册的扩展原语（如 ext.ai / ext.db），名字不固定，故 UI 用可输入标签而非固定下拉。
 *
 * 每个受门禁原语都必须有中文说明——漏一个，UI 上就是一片空白（由 contract.spec.ts 守卫）。
 */
export const CAPABILITY_LABEL: Record<string, string> = {
  http: 'http · HTTP 请求',
  call: 'call · 调用其它 Tool / Flow',
  db: 'db · 数据库原语',
  secret: 'secret · 密钥读取（只读）',
  event: 'event · 事件收发',
  subscribe: 'subscribe · 订阅事件源',
  config: 'config · 持久化键值配置',
}

/** 执行状态（server/execstore.go）。rejected = 入口繁忙被背压拒绝，未真正运行。 */
export const EXEC_STATUSES = ['running', 'succeeded', 'failed', 'rejected'] as const

export const EXEC_STATUS_LABEL: Record<string, string> = {
  running: '运行中',
  succeeded: '成功',
  failed: '失败',
  rejected: '被拒绝（入口繁忙）',
}
