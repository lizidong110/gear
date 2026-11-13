// 端点封装：一一对应 internal/server/server.go:routes() 里的路由。
//
// 之前这里列过三处「内核没提供的能力」（GET /api/tools/{id}、DELETE /api/flows/{id}、
// DELETE /api/hooks/{id}），UI 侧只能置灰并写明原因。现在内核已补齐这三个端点，
// 占位全部撤掉——所以本文件里的端点 = 内核真实提供的端点，不再有缺口清单。
// 红线不变：前端只调用内核真实存在的路由；缺口要正面暴露，不能靠「列表里过滤」假装支持。
//
// 后续内核新增的管理面端点（secrets / tools run / apps prune / events 回推 / news 演示）
// 也在此补齐，保持「端点封装 = 内核路由表镜像」的单一真相源。

import { ApiClient, ApiError, extractError } from './client'
import { getApiBase } from './base'
import type {
  AppDTO,
  AppDefDTO,
  AppDefImportReqDTO,
  AppDefImportRespDTO,
  AppDefListDTO,
  AppRecordListDTO,
  CapabilityDTO,
  CapabilityInvokeReq,
  CapabilityInvokeResp,
  ConfigCatalogDTO,
  ConfigListDTO,
  ConfigSetRespDTO,
  CreateExecutionDTO,
  DeletedDTO,
  ExecutionDTO,
  FlowDTO,
  FlowValidateDTO,
  HealthzDTO,
  HookDTO,
  PruneAppsReqDTO,
  PruneAppsRespDTO,
  PromotedDTO,
  RunResultDTO,
  RunToolRespDTO,
  SecretListDTO,
  SubscriptionEventReqDTO,
  ToolDTO,
} from './types'

export const api = new ApiClient(getApiBase)

/** 执行日志行（server.go:handleGetExecutionLogs → ExecStore.Logs 的 LogEntry 形状）。
 *  step_id/stream/line 三段都在，前端据此分 stdout/stderr 着色、显示来源 step。 */
export interface ExecLog {
  exec_id: string
  step_id: string
  stream: 'stdout' | 'stderr'
  line: string
  ts?: string
}

/** 执行日志端点返回的是 ExecLog 数组（按产生顺序），不再是纯字符串。 */
export type ExecutionLogs = ExecLog[]

export const endpoints = {
  // ---- 健康 / 演示 ----
  healthz: (signal?: AbortSignal) => api.get<HealthzDTO>('/healthz', signal),
  /** 演示端点：经引擎同步运行 news.top10 Flow（抓取并整理百度热搜 Top10），直接返回 RunResultDTO。
   *  未注册示例（未用 `gear serve --seed examples/news-top10.yaml`）→ 404。 */
  getNews: (signal?: AbortSignal) => api.get<RunResultDTO>('/news', signal),

  // ---- 原子能力目录（只读，与 CLI `gear ai capabilities` 同源） ----
  capabilities: (signal?: AbortSignal) => api.get<CapabilityDTO[]>('/capabilities', signal),
  // 直接执行一个原子能力的方法（「能力目录」页调用测试用）。
  invokeCapability: (req: CapabilityInvokeReq) =>
    api.post<CapabilityInvokeResp>('/capabilities/invoke', req),

  // ---- 密钥（REQ-API-SECRETS：管理面经 REST 写入，运行期只读；响应不回显明文） ----
  listSecrets: (signal?: AbortSignal) => api.get<SecretListDTO>('/secrets', signal),
  setSecret: (name: string, value: string) =>
    api.put<{ ok: boolean; name: string }>(`/secrets/${encodeURIComponent(name)}`, { value }),
  deleteSecret: (name: string) =>
    api.del<{ ok: boolean; name: string }>(`/secrets/${encodeURIComponent(name)}`),

  // ---- Tools ----
  listTools: (signal?: AbortSignal) => api.get<ToolDTO[]>('/tools', signal),
  /** 单查一个 Tool（含 versions）。不存在 → 404。此前只能全量拉列表再本地索引。 */
  getTool: (id: string, signal?: AbortSignal) =>
    api.get<ToolDTO>(`/tools/${encodeURIComponent(id)}`, signal),
  createTool: (t: ToolDTO) => api.post<ToolDTO>('/tools', t),
  deleteTool: (id: string) => api.del<DeletedDTO>(`/tools/${encodeURIComponent(id)}`),
  promoteTool: (id: string, version: string, target: string) =>
    api.post<PromotedDTO>(`/tools/${encodeURIComponent(id)}/versions/${encodeURIComponent(version)}/promote`, {
      target,
    }),
  /** 同步执行单个 Tool 并直接返回输出 `{id, output}`（POST /api/tools/{id}/run）。
   *  与 MCP `gear_run` 对齐；body 为入参 map，可留空 = 无入参执行。 */
  runTool: (id: string, input: Record<string, unknown> = {}) =>
    api.post<RunToolRespDTO>(`/tools/${encodeURIComponent(id)}/run`, input),

  // ---- Flows ----
  listFlows: (signal?: AbortSignal) => api.get<FlowDTO[]>('/flows', signal),
  createFlow: (f: FlowDTO) => api.post<FlowDTO>('/flows', f),
  deleteFlow: (id: string) => api.del<DeletedDTO>(`/flows/${encodeURIComponent(id)}`),
  validateFlow: (id: string) => api.post<FlowValidateDTO>(`/flows/${encodeURIComponent(id)}/validate`),

  // ---- Hooks ----
  listHooks: (signal?: AbortSignal) => api.get<HookDTO[]>('/hooks', signal),
  createHook: (h: HookDTO) => api.post<HookDTO>('/hooks', h),
  deleteHook: (id: string) => api.del<DeletedDTO>(`/hooks/${encodeURIComponent(id)}`),

  // ---- 执行 ----
  listExecutions: (signal?: AbortSignal) => api.get<ExecutionDTO[]>('/executions', signal),
  getExecution: (id: string, signal?: AbortSignal) =>
    api.get<ExecutionDTO>(`/executions/${encodeURIComponent(id)}`, signal),
  createExecution: (flowId: string) => api.post<CreateExecutionDTO>('/executions', { flow_id: flowId }),
  executionLogs: (id: string, signal?: AbortSignal) =>
    api.get<ExecutionLogs>(`/executions/${encodeURIComponent(id)}/logs`, signal),
  /** 实时日志流（SSE）：GET /api/executions/{id}/logs/stream（REQ-API-LOGS-STREAM）。
   *  内核只推「订阅后新产生」的行（历史由 executionLogs 提供），所以调用方通常先拉历史再接流。
   *  帧格式：`data: <LiveLog JSON>`（含 step_id/stream/line/ts）；`event: done` 表示执行已终态，流关闭。 */
  streamExecutionLogs: (id: string, signal?: AbortSignal) =>
    streamExecutionLogs(id, signal),
  /** 订阅回推端点（subscribe 机制：sidecar 收到消息后 POST 到 /api/events/{handle}）。
   *  body 为 {payload}；按 handle 定位订阅交引擎投递。前端一般不直接调，保留封装以对齐内核。 */
  postSubscriptionEvent: (handle: string, payload: unknown) =>
    api.post<{ ok: boolean }>(`/events/${encodeURIComponent(handle)}`, { payload } satisfies SubscriptionEventReqDTO),

  // ---- 受管长驻进程 ----
  listApps: (signal?: AbortSignal) => api.get<AppDTO[]>('/apps', signal),
  createApp: (flowId: string, restart: string) => api.post<AppDTO>('/apps', { flow_id: flowId, restart }),
  deleteApp: (id: string) => api.del<DeletedDTO>(`/apps/${encodeURIComponent(id)}`),
  /** 清理落盘台账里确定已终止的应用记录（POST /api/apps/prune；与内存侧 /api/apps 不是同一模型）。
   *  body 空 = 全量、非 dry-run；只删 exited/stopped/startup_failed 的记录，不杀进程、不碰端口。 */
  pruneApps: (req: PruneAppsReqDTO = {}) => api.post<PruneAppsRespDTO>('/apps/prune', req),
  /** detached 台账侧应用（REQ-API-APP-LOGS）：列 `gear app start` 的落盘台账。
   *  ⚠ 与 listApps 不是同一模型；UI 靠它选择「订阅哪个 App 的实时日志」。 */
  listAppRecords: (signal?: AbortSignal) => api.get<AppRecordListDTO>('/apps/records', signal),
  /** SSE 实时日志流（薄封装；实际调用请用导出的 streamAppLogs 以传回调）。 */
  streamAppLogs: (id: string, signal?: AbortSignal) => streamAppLogs(id, signal),

  // ---- 配置键（REQ-API-CONFIG-CATALOG）：目录由内核提供，页面按中文名渲染，不必记 key ----
  configCatalog: (signal?: AbortSignal) => api.get<ConfigCatalogDTO>('/config/catalog', signal),
  /** 当前值；敏感项只回掩码。env_override=true 表示环境变量压过配置库，页面改动不生效。 */
  listConfig: (signal?: AbortSignal) => api.get<ConfigListDTO>('/config', signal),
  /** 写入一项。空串是合法值（显式置空回退下一级）；要彻底删除用 deleteConfig。 */
  setConfig: (key: string, value: string) =>
    api.put<ConfigSetRespDTO>(`/config/${encodeURIComponent(key)}`, { value }),
  /** 删除一项，回到内置默认；不存在 → 404。 */
  deleteConfig: (key: string) =>
    api.del<{ ok: boolean; key: string }>(`/config/${encodeURIComponent(key)}`),

  // ---- App 清单定义（REQ-API-APP-DEFS）：registry 里的 kind: App，与 /apps 的受管进程分开 ----
  listAppDefs: (signal?: AbortSignal) => api.get<AppDefListDTO>('/app-defs', signal),
  getAppDef: (id: string, signal?: AbortSignal) =>
    api.get<AppDefDTO>(`/app-defs/${encodeURIComponent(id)}`, signal),
  /** 导出为多文档 YAML **文本**（Content-Type: application/x-yaml，非 JSON）。
   *  文本可直接粘回 importAppDef 的 bundle 字段完成往返；静态资源不含文件内容。 */
  exportAppDef: (id: string, signal?: AbortSignal) =>
    api.getText(`/app-defs/${encodeURIComponent(id)}/export`, signal),
  /** 导入。先传 dry_run=true 预检（只汇报不写），确认后再真正写入；
   *  id 已存在且内容不同时内核回 409，须显式 overwrite=true。 */
  importAppDef: (req: AppDefImportReqDTO) =>
    api.post<AppDefImportRespDTO>('/app-defs/import', req),
  /** 删除清单定义（不删其引用的 Tool/Flow/Hook）；不存在 404 / 被引用 409。 */
  deleteAppDef: (id: string) =>
    api.del<{ ok: boolean; id: string }>(`/app-defs/${encodeURIComponent(id)}`),
}

/** SSE 流式调用：POST /api/capabilities/invoke/stream 的客户端。
 *  内核目前仅支持 ext.ai.complete_stream，以 SSE 逐增量推送 delta/reasoning/done/error。
 *  沿用 /invoke 的同源与禁 secret 约束（server.go:handleInvokeCapabilityStream）。
 *  逐帧回调 onChunk；调用方负责累积 delta 并渲染。 */
export interface StreamChunk {
  delta?: string
  reasoning?: string
  done?: boolean
  error?: string
}

export async function streamInvokeCapability(
  req: CapabilityInvokeReq,
  onChunk: (c: StreamChunk) => void,
  signal?: AbortSignal,
): Promise<void> {
  const base = getApiBase().replace(/\/+$/, '')
  const url = `${base}/api/capabilities/invoke/stream`
  const res = await fetch(url, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(req),
    signal,
  })
  if (!res.ok || !res.body) {
    const text = await res.text().catch(() => '')
    throw new ApiError(res.status, extractError(text) ?? `HTTP ${res.status}`, text)
  }
  const reader = res.body.getReader()
  const decoder = new TextDecoder()
  let buf = ''
  for (;;) {
    const { done, value } = await reader.read()
    if (done) break
    buf += decoder.decode(value, { stream: true })
    let idx: number
    while ((idx = buf.indexOf('\n\n')) >= 0) {
      const raw = buf.slice(0, idx)
      buf = buf.slice(idx + 2)
      for (const line of raw.split('\n')) {
        if (!line.startsWith('data:')) continue
        const json = line.slice(5).trim()
        if (!json) continue
        try {
          onChunk(JSON.parse(json) as StreamChunk)
        } catch {
          /* 忽略无法解析的 SSE 事件帧 */
        }
      }
    }
  }
}

/** SSE 实时日志帧（内核 LiveLog，server.go:logbroker.go）。 */
export interface LiveLog {
  step_id: string
  stream: 'stdout' | 'stderr'
  line: string
  ts: number
}

/** SSE 流式拉取执行日志：GET /api/executions/{id}/logs/stream。
 *  onLog 每收到一行实时日志回调一次；onDone 在收到 `event: done`（执行终态）或连接关闭时回调。
 *  注意：内核只推「订阅后新产生」的行（不回放历史），调用方一般先 executionLogs 拉历史再接流。 */
export async function streamExecutionLogs(
  id: string,
  signal?: AbortSignal,
  onLog?: (l: LiveLog) => void,
  onDone?: (status?: string) => void,
): Promise<void> {
  const base = getApiBase().replace(/\/+$/, '')
  const url = `${base}/api/executions/${encodeURIComponent(id)}/logs/stream`
  const res = await fetch(url, { method: 'GET', signal })
  await readLogSSE(res, onLog, onDone)
}

/** detached 台账侧 App 的实时日志流（REQ-API-APP-LOGS）：GET /api/apps/{id}/logs/stream。
 *  内核只推**订阅后新产生**的行：不读 <appID>.log、不回放历史（用户的硬要求）；
 *  App 结案（stop / startup_failed）时发 `event: done` 并关闭连接。 */
export async function streamAppLogs(
  id: string,
  signal?: AbortSignal,
  onLog?: (l: LiveLog) => void,
  onDone?: (status?: string) => void,
): Promise<void> {
  const base = getApiBase().replace(/\/+$/, '')
  const url = `${base}/api/apps/${encodeURIComponent(id)}/logs/stream`
  const res = await fetch(url, { method: 'GET', signal })
  await readLogSSE(res, onLog, onDone)
}

/** 两条日志流共用同一套 SSE 帧解析（执行记录面 / detached App 面）。
 *  抽出来的理由：帧格式、保活注释帧、`event: done`、断连判定都是同一份协议；
 *  各写一遍就会「给一边修了粘包处理、另一边留着 bug」，而两边各自的测试都还是绿的。 */
async function readLogSSE(
  res: Response,
  onLog?: (l: LiveLog) => void,
  onDone?: (status?: string) => void,
): Promise<void> {
  if (!res.ok || !res.body) {
    const text = await res.text().catch(() => '')
    throw new ApiError(res.status, extractError(text) ?? `HTTP ${res.status}`, text)
  }
  const reader = res.body.getReader()
  const decoder = new TextDecoder()
  let buf = ''
  for (;;) {
    const { done, value } = await reader.read()
    if (done) break
    buf += decoder.decode(value, { stream: true })
    let idx: number
    while ((idx = buf.indexOf('\n\n')) >= 0) {
      const raw = buf.slice(0, idx)
      buf = buf.slice(idx + 2)
      let data = ''
      let ev = ''
      for (const line of raw.split('\n')) {
        if (line.startsWith('event:')) ev = line.slice(6).trim()
        else if (line.startsWith('data:')) data += (data ? ' ' : '') + line.slice(5).trim()
      }
      if (ev === 'done') {
        onDone?.(parseStatus(data))
        continue
      }
      if (!data) continue
      try {
        const obj = JSON.parse(data) as LiveLog
        if (obj && typeof obj.line === 'string') onLog?.(obj)
      } catch {
        /* 忽略无法解析的 SSE 事件帧（含 `: ping` 保活注释帧） */
      }
    }
  }
  // 读到 EOF（连接被内核关闭）：若没有显式 done 帧，也视为结束。
  onDone?.()
}

function parseStatus(data: string): string | undefined {
  try {
    const o = JSON.parse(data) as { status?: string }
    return o.status
  } catch {
    return undefined
  }
}
