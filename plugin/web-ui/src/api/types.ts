// REST 契约的 TS 投影 —— 形状必须与 Go 的 json tag 一致（内核是唯一真相源）。
// 对应实现：internal/model/model.go、internal/engine/engine.go:RunResult、
//          internal/server/execstore.go:Execution、internal/server/server.go:AppInstance。
//
// 凡标 `| null` 的字段，对应 Go 侧是无 omitempty 的切片/map（nil 会序列化成 null），
// 表单里必须容忍 null，否则 `.map` 会炸。

export type JsonObject = Record<string, unknown>

/** Tool 版本条目（model.ToolVersion）。 */
export interface ToolVersionDTO {
  version: string
  state: string
  digest: string
  note: string
  created_at: string
  entry?: string
  input_schema?: JsonObject
  output_schema?: JsonObject
}

/** service 模式健康探针（model.HealthProbe；时长字段是字符串，如 "5s"）。 */
export interface HealthProbeDTO {
  kind: string
  addr: string
  path: string
  cmd: string
  interval: string
  timeout: string
  threshold: number
}

/** Tool 的声明式 REST 路由（model.APIRoute）。 */
export interface ToolApiDTO {
  method: string
  path: string
}

/** 原子能力（model.Tool）。 */
export interface ToolDTO {
  id: string
  name: string
  description: string
  runtime: string
  entry: string
  input_schema: JsonObject | null
  output_schema: JsonObject | null
  versions: ToolVersionDTO[] | null
  capabilities: string[] | null
  health_probe?: HealthProbeDTO | null
  /** 声明式 REST 路由（model.Tool.API）；未声明即不挂资源路由。 */
  api?: ToolApiDTO | null
}

/** Flow 内执行单元（model.Step）。 */
export interface StepDTO {
  id: string
  call: string
  use_version: string
  input: JsonObject | null
  depends_on: string[] | null
  when: string
  parallel: boolean
  mode: string
}

/** 通用运行单元（model.Flow）。 */
export interface FlowDTO {
  id: string
  name: string
  description: string
  vars: JsonObject | null
  steps: StepDTO[] | null
}

/** 横切控制（model.Hook）。 */
export interface HookDTO {
  id: string
  target: string
  stage: string
  action: string
  criticality: string
  order: number
}

/** 一次 Flow 执行的结构化结果（engine.RunResult）。 */
export interface RunResultDTO {
  flow_id: string
  steps?: Record<string, Record<string, unknown>>
  skipped?: Record<string, string>
}

/** 执行记录（server.Execution）。 */
export interface ExecutionDTO {
  id: string
  flow_id: string
  status: string
  error?: string
  tool_ids?: string[]
  result?: RunResultDTO
  started_at: string
  finished_at?: string | null
}

/** 受管长驻进程（server.AppInstance）。 */
export interface AppDTO {
  id: string
  flow_id: string
  restart: string
  started_at: string
}

/** GET /api/healthz。 */
export interface HealthzDTO {
  status: string
  tools: number
  flows: number
}

/** POST /api/flows/{id}/validate 的响应（注意：校验失败也是 200，靠 valid 字段判定）。 */
export interface FlowValidateDTO {
  valid: boolean
  error?: string
}

/** POST /api/executions 的 202 响应。 */
export interface CreateExecutionDTO {
  id: string
  status: string
}

export interface DeletedDTO {
  deleted?: string
  stopped?: string
}

export interface PromotedDTO {
  tool_id: string
  version: string
  state: string
}

/** 单个方法参数的 schema（扩展原语方法级参数；与 Go ParamSchema 一致）。 */
export interface CapParamDTO {
  type: string
  description?: string
  enum?: string[]
  required?: boolean
}

/** 一个原子能力的方法 schema（Go MethodSchema）。 */
export interface CapMethodDTO {
  description?: string
  parameters?: Record<string, CapParamDTO> | null
}

/** 原子能力目录条目：内置 6 原语（kind=builtin）+ 已注册 ext.*（kind=ext）。
 *  与 CLI `gear ai capabilities`、REST `GET /api/capabilities` 同源（engine.AIAgent().CapabilitiesMethod）。 */
export interface CapabilityDTO {
  name: string
  kind: 'builtin' | 'ext'
  methods: Record<string, CapMethodDTO> | null
}

/** POST /api/capabilities/invoke 的请求体：直接执行一个原子能力的方法。
 *  args 以「参数名为键」的 map（与 capabilities() 的 schema 对齐）——内置原语经引擎重排为
 *  位置参数、ext.* 原样作为单 table 传入。 */
export interface CapabilityInvokeReq {
  name: string
  method: string
  args: Record<string, unknown>
}

/** POST /api/capabilities/invoke 的响应：result 即该能力方法的返回值（任意 JSON）。 */
export interface CapabilityInvokeResp {
  result: unknown
}

/** GET /api/secrets：只回密钥名（不回明文、不回值；写入/读出都走运行期，管理面只列名）。 */
export interface SecretListDTO {
  names: string[]
}

/** POST /api/tools/{id}/run 的响应：同步执行单个 Tool 并直接返回输出（与 MCP `gear_run` 对齐）。 */
export interface RunToolRespDTO {
  id: string
  output: unknown
}

/** POST /api/apps/prune 的请求体（body 空 = 全量、非 dry-run）。 */
export interface PruneAppsReqDTO {
  flow_id?: string
  dry_run?: boolean
}

/** POST /api/apps/prune 的响应：清理落盘台账里确定已终止的应用记录。
 *  pruned/skipped 是应用 id 列表；hint 仅当存在 skipped 时给出（说明保守边界）。 */
export interface PruneAppsRespDTO {
  dry_run: boolean
  apps_dir: string
  pruned: string[]
  skipped: string[]
  pruned_count: number
  skipped_count: number
  hint?: string
}

/** POST /api/events/{handle} 的回推体（subscribe 机制：sidecar 收到消息后 POST 到这里）。 */
export interface SubscriptionEventReqDTO {
  payload: unknown
}
