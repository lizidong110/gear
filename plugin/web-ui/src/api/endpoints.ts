// 端点封装：一一对应 internal/server/server.go:routes() 里的路由。
//
// 之前这里列过三处「内核没提供的能力」（GET /api/tools/{id}、DELETE /api/flows/{id}、
// DELETE /api/hooks/{id}），UI 侧只能置灰并写明原因。现在内核已补齐这三个端点，
// 占位全部撤掉——所以本文件里的端点 = 内核真实提供的端点，不再有缺口清单。
// 红线不变：前端只调用内核真实存在的路由；缺口要正面暴露，不能靠「列表里过滤」假装支持。

import { ApiClient } from './client'
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
  PromotedDTO,
  ToolDTO,
} from './types'

export const api = new ApiClient(getApiBase)

/** 执行日志是**按行**返回的字符串数组（server.go:handleGetExecutionLogs → ExecStore.Logs）。 */
export type ExecutionLogs = string[]

export const endpoints = {
  // ---- 健康 / 演示 ----
  healthz: (signal?: AbortSignal) => api.get<HealthzDTO>('/healthz', signal),

  // ---- 原子能力目录（只读，与 CLI `gear ai capabilities` 同源） ----
  capabilities: (signal?: AbortSignal) => api.get<CapabilityDTO[]>('/capabilities', signal),
  // 直接执行一个原子能力的方法（「能力目录」页调用测试用）。
  invokeCapability: (req: CapabilityInvokeReq) =>
    api.post<CapabilityInvokeResp>('/capabilities/invoke', req),

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

  // ---- Flows ----
  listFlows: (signal?: AbortSignal) => api.get<FlowDTO[]>('/flows', signal),
  createFlow: (f: FlowDTO) => api.post<FlowDTO>('/flows', f),
  /**
   * 注销 Flow。内核有 §8.2 三条守卫：被其它 Flow 的 step、被 Flow 级 Hook 的 action 引用，
   * 或正被运行中的执行当 Hook 载体用时 → **409**（响应体里写明是谁挡住了）；不存在 → 404。
   */
  deleteFlow: (id: string) => api.del<DeletedDTO>(`/flows/${encodeURIComponent(id)}`),
  validateFlow: (id: string) => api.post<FlowValidateDTO>(`/flows/${encodeURIComponent(id)}/validate`),

  // ---- Hooks ----
  listHooks: (signal?: AbortSignal) => api.get<HookDTO[]>('/hooks', signal),
  createHook: (h: HookDTO) => api.post<HookDTO>('/hooks', h),
  /** 删除 Hook。Hook 不被别的定义引用，故无引用守卫；不存在 → 404（不静默 no-op）。 */
  deleteHook: (id: string) => api.del<DeletedDTO>(`/hooks/${encodeURIComponent(id)}`),

  // ---- 执行 ----
  listExecutions: (signal?: AbortSignal) => api.get<ExecutionDTO[]>('/executions', signal),
  getExecution: (id: string, signal?: AbortSignal) =>
    api.get<ExecutionDTO>(`/executions/${encodeURIComponent(id)}`, signal),
  createExecution: (flowId: string) => api.post<CreateExecutionDTO>('/executions', { flow_id: flowId }),
  executionLogs: (id: string, signal?: AbortSignal) =>
    api.get<ExecutionLogs>(`/executions/${encodeURIComponent(id)}/logs`, signal),

  // ---- 受管长驻进程 ----
  listApps: (signal?: AbortSignal) => api.get<AppDTO[]>('/apps', signal),
  createApp: (flowId: string, restart: string) => api.post<AppDTO>('/apps', { flow_id: flowId, restart }),
  deleteApp: (id: string) => api.del<DeletedDTO>(`/apps/${encodeURIComponent(id)}`),
}
