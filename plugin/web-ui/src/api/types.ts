// REST 契约的 TS 投影 —— 形状必须与 Go 的 json tag 一致（内核是唯一真相源）。
// 对应实现：internal/model/model.go、internal/engine/engine.go:RunResult、
//          internal/server/execstore.go:Execution、internal/app/record.go:Record（App 台账）。
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

/** 受管长驻 App：`gear app start` 落盘台账的实时探活投影（server 端 app.StatusResult；REQ-APP-MANAGED-DETACHED）。 */
export interface AppDTO {
  app_id: string
  flow_id: string
  pid: number
  /** 双判据（pid 存活 + 端口探活）实时算出：running / exited / starting / unknown / stopped / startup_failed；
   *  restart_exhausted = 重启预算耗尽（窗口内被反复弄死到放弃重启，监管进程已退出）。 */
  state: string
  started_at: string
  /** 已运行秒数（仅 running 时非零）。 */
  uptime_s?: number
  restart?: string
  /** 监管期间 service 累计重启次数（由被派生的监管进程回写台账；0 = 从未重启）。 */
  restarts?: number
  /** 派生时是否注入了实时日志回推 env；false = 面板不会有数据（不是「暂时没日志」）。 */
  live_logs?: boolean
  ports?: { port: number; listening: boolean }[]
  log_path: string
  /** 日志里捞出的「打开地址」（GEAR_OPEN_URL= 标记，如 web_ui.done 收尾步），前端渲染成可点击链接。 */
  open_urls?: string[]
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

// ---- 配置键管理面（REQ-API-CONFIG-CATALOG）----
// 目录由内核 cap.ConfigCatalog() 提供，前端不硬编码 key 清单：新增键只需改内核。

/** 内置关键配置键的展示元数据（GET /api/config/catalog）。 */
export interface ConfigKeySpec {
  key: string
  label: string
  group: string
  description: string
  default: string
  default_note?: string
  env?: string
  sensitive: boolean
  options?: string[]
  value_type: 'string' | 'number' | 'enum' | 'secret'
}

export interface ConfigCatalogDTO {
  items: ConfigKeySpec[]
  groups: string[]
  priority: string
}

/** GET /api/config 的单项当前状态。敏感项 value 是掩码，不是明文。 */
export interface ConfigItemDTO {
  spec: ConfigKeySpec
  value: string
  set: boolean
  masked: boolean
  env_override: boolean
  env_name?: string
  known: boolean
}

export interface ConfigListDTO {
  items: ConfigItemDTO[]
}

/** PUT /api/config/{key} 的响应（不回显 value，敏感项尤其不能回）。 */
export interface ConfigSetRespDTO {
  ok: boolean
  key: string
  known: boolean
}

// ---- App 清单管理面（REQ-API-APP-DEFS）----
// ⚠ 与上面的 AppDTO（/api/apps 的**运行中进程台账**）不是同一层：
//    这里是 registry 里 `kind: App` 的清单定义，即「这个应用由什么组成」。

/** App 清单的 MCP 策展区（model.AppMCP）；一条规则 = 一个对外工具。 */
export interface AppDefMCPDTO {
  tools: { ref: string; alias?: string; description?: string; args?: JsonObject }[] | null
}

/** App 清单本体（model.App 的 TS 投影）。UI 只展示头部字段，其余区段用宽松类型兜住。 */
export interface AppDefBodyDTO {
  id: string
  name: string
  description: string
  entry_flow: string
  layout?: JsonObject | null
  start?: JsonObject | null
  requires?: JsonObject | null
  mcp?: AppDefMCPDTO | null
}

/** 组成清单（artifact.Components）：导出时会按这些 id 收集定义文档。 */
export interface AppDefComponentsDTO {
  entry_flow: string
  tools: string[] | null
  flows: string[] | null
  hooks: string[] | null
  static_files: string[] | null
}

/** GET /api/app-defs 与 GET /api/app-defs/{id} 的单项。 */
export interface AppDefDTO {
  app: AppDefBodyDTO
  components: AppDefComponentsDTO | null
  counts: Record<string, number>
  synth_note?: string
}

export interface AppDefListDTO {
  items: AppDefDTO[]
}

/** POST /api/app-defs/import 的请求体：bundle 是多文档 YAML **文本**（不是服务器路径）。 */
export interface AppDefImportReqDTO {
  bundle: string
  dry_run?: boolean
  overwrite?: boolean
}

/** bundle 解析诊断（engine.Diagnostic），含「怎么修」的 hint。 */
export interface BundleDiagnosticDTO {
  code: string
  message: string
  step?: string
  field?: string
  hint?: string
}

/** POST /api/app-defs/import 的响应。dry_run=true 时额外给 conflicts（冲突上界预告）。 */
export interface AppDefImportRespDTO {
  dry_run: boolean
  created?: string[]
  updated?: string[]
  conflicts?: string[]
  migrations_applied?: number[]
}

/** 坏 bundle 的 400 响应体（HTTP 错误，经 ApiError.body 解析后展示结构化诊断）。 */
export interface BadBundleRespDTO {
  code: string
  message: string
  diagnostics: BundleDiagnosticDTO[]
}

// （GET /api/apps/records 已移除：REQ-APP-MANAGED-DETACHED 后 AppDTO 即台账投影，一张表。）
