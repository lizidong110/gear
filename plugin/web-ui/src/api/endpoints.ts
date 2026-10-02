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
  CapabilityDTO,
  CapabilityInvokeReq,
  CapabilityInvokeResp,
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

/** 执行日志是**按行**返回的字符串数组（server.go:handleGetExecutionLogs → ExecStore.Logs）。 */
export type ExecutionLogs = string[]

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
