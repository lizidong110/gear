# Gear 需求可追溯矩阵（可执行规格契约）

> **方法**：F1–F5 工程审计方法。每条 PRD 行为在此打 ID、标状态、绑定一条「断言该行为的测试」。
> 覆盖率闸门从「行覆盖」升级为「**需求 ID 覆盖**」：凡 `implemented` 的需求必须有 ≥1 条测试断言其 PRD 行为；凡 `deferred` 的需求必须有测试断言「当前被显式拒绝 / 报错」（而非静默忽略）。
> 这是 `插件化自动化编排框架_PRD_v40.md` 的配套契约，文档与代码漂移以本文件为准。

## 状态图例

| 状态 | 含义 |
|------|------|
| `implemented` | 代码已实现，且有测试断言其行为 |
| `deferred` | 计划于后续切片，代码尚未实现；注册/运行期**显式报错**（非静默忽略） |
| `contradicted→fixed` | 原 PRD 示例/声明与代码不符，已修正文档（代码不变） |
| `architecture-debt` | 架构级缺失，需后续切片建接缝（如 Hook 引擎调用点、ctx.* 能力面） |

---

## 需求矩阵（S1 范围 + 已识别缺口）

| 需求 ID | PRD 章节 | 行为 | 状态 | 断言测试 |
|---------|---------|------|------|---------|
| REQ-ENGINE-SEQ | §4.3 | 按 `depends_on` 拓扑排序顺序执行 step | `implemented` | `engine_test.go`: TestTopoOrder*、TestRunFlowEndToEnd |
| REQ-TMPL | §4.3 | `${vars.x}` / `${steps.id.output.field}` 模板解析 | `implemented` | `engine_test.go`: TestExpandString*、TestResolveValueComposite |
| REQ-TMPL-VALID | §4.3(F10) | 模板**静态校验**：注册期校验前缀合法（vars/steps/secrets 三类）、vars 顶层键存在、steps 与 secrets 都必须带名字；**未知前缀**（这三类之外）注册期拒（F3 延伸：能静态校验的不留到运行期）。密钥**是否存在**不在此校验（密钥源是运行期宿主能力） | `implemented` | `engine/template_test.go`: TestValidateTemplatesOK / AllowsSecrets / RejectsUnknownPrefix / RejectsUnknownVarKey / RejectsBareSteps / RejectsUnclosed / DelegatesStepExistence、`TestSpecSecretsTemplateAllowedAtRegister`；`validate_static.go` 接 `validateTemplates`；CLI `flow register` 前置 |
| REQ-SCHEMA-IN | §4.3/§9.1 | step input 经 Tool.input_schema 校验（单一真理源） | `implemented` | `engine_test.go`: TestValidatePass/Fail、TestRunFlowRejectsInvalidInput |
| REQ-SCHEMA-OUT | §9.1 | step output 经 Tool.output_schema 校验 | `implemented` | `engine_test.go`: TestRunFlowRejectsInvalidOutput |
| REQ-PROC | §3.3 | process runtime：stdin JSON → stdout 末行 JSON | `implemented` | `engine_test.go`: TestRunProcessEcho；`process_test.go`: TestRunProcess* |
| REQ-PROC-TIMEOUT | §3.3/§14.2 | process 单次执行超时受控（防挂死） | `implemented` | `process_test.go`: TestRunProcessTimeout/WithinTimeout |
| REQ-REG-FLOW | §8 | Flow 注册/校验/读取落库 | `implemented` | `registry_test.go`: TestFlowRoundtrip |
| REQ-RUNTIME-DISPATCH | §3.2/§3.3 | 引擎按 `Tool.Runtime` 分发适配器；**once 与 service 共用唯一点 `runtimeFor`**；未知/未实现 runtime 报错 | `implemented`（once S1；service 统一于 S2 修复 M10） | `engine_test.go`: TestRuntimeForRejectsUnimplemented、TestProcessRuntimeStart |
| REQ-TOOL-RUNTIME-VALID | §3.2(F3) | Tool.runtime 注册期校验：未知值拒；已知但未实现（go-host/wasm）也拒（G1 修复） | `implemented`（S2 修复；S7a 起 lua 转为可注册） | `model_test.go`: TestToolValidate（未知/未实现 runtime）；`engine_test.go`: TestSpecRejectsUnimplementedRuntimeTool |
| REQ-RUNTIME-IMPL-SINGLE-SOURCE | §3.2 | 「合法 runtime」与「已实现 runtime」收敛到 model 单一来源（KnownRuntimes / ImplementedRuntimes） | `implemented`（S2 修复 C） | `model_test.go`: TestRuntimeEnum、TestRuntimeKnownImplemented |
| REQ-FLOW-GUARD | §4.3(F3) | Flow 含未实现字段（`when`/`parallel`/`use_version`/空 call）注册期显式拒绝 | `implemented` | `model_test.go`: TestFlowValidate；`engine_test.go`: TestSpecRejectsOldFieldNames |
| REQ-SPEC-CONSISTENCY | §4.1(F2) | PRD 文档格式 Flow 可加载并运行 | `implemented` | `engine_test.go`: TestSpecFlowFormatLoadsAndRuns、TestRunExampleHelloFlow |
| REQ-LONGSTEP | §14.2 | `mode: service`：拉起进程不等待，Flow 借此成为长驻服务 | `implemented`（S2） | `engine/supervise_test.go`: TestSuperviseLaunchesService、TestSupervisePureOnce |
| REQ-APP-RESTART | §14.3 | `app run` 监管 service 步骤 + restart 策略（always/never）+ 信号停止 | `implemented`（S2） | `app/supervisor_test.go`: TestSuperviseRestartAlways、TestSuperviseRestartNever、TestSuperviseStaysAliveThenCancel |
| REQ-FLOW-RUN-REJECTS-SERVICE | §14.2(F3) | service step 不能用 `flow run` 一次性执行（须 `app run` 监管） | `implemented`（S2） | `engine/supervise_test.go`: TestRunFlowRejectsServiceStep |
| REQ-SVC-DEP-GUARD | §14.2(F3) | service step 不能作为其他 step 的依赖（它永不结束） | `implemented`（S2） | `engine/supervise_test.go`: TestServiceDepGuard |
| REQ-APP-MAXCONC | §14.5 | `app run` 并发闸门 `--max`（进程池/队列留 S4） | `implemented`（S2 前哨） | `app/supervisor_test.go`: TestSuperviseMaxConcurrent |
| REQ-MODE-GUARD | §4.3(F3) | `step.mode` 非 once/service 注册期显式拒绝 | `implemented`（S2） | `model_test.go`: TestFlowValidate（mode 非法） |
| REQ-RUNTIME-LUA | §3.2/§3.3 | lua runtime：`entry`=inline 源码，`ctx.input` 进 / `ctx.output` 或 return 出 | `implemented`（S7a） | `engine/lua_test.go`: TestLuaRunBasic、TestLuaCtxOutput、TestLuaArrayAndNested、TestLuaFlowEndToEnd |
| REQ-LUA-SANDBOX | §3.2 | lua 沙箱无宿主逃逸：不加载 os/io/package/debug，移除 dofile/loadfile/require/module | `implemented`（S7a） | `engine/lua_test.go`: TestLuaSandboxGlobals、TestLuaSandboxEscapeBlocked |
| REQ-LUA-TIMEOUT | §3.2/§3.3 | lua 死循环被 ctx 中断（`L.SetContext`），与 process 超时对称 | `implemented`（S7a） | `engine/lua_test.go`: TestLuaTimeout |
| REQ-LUA-ERR | §3.2 | lua 编译/运行期错误与「无输出」显式报错 | `implemented`（S7a） | `engine/lua_test.go`: TestLuaSyntaxError、TestLuaRuntimeError、TestLuaNoOutput |
| REQ-HOOK | §5/§14 | Hook 生命周期（before_flow / before_step / after_step / on_error / on_success；service 与 once 共用同一执行路径）；Flow 级 stage 按 flow id 匹配、step 级按 tool id 匹配 | `implemented`（S3 + #72） | `engine/hook_test.go`: TestHookBeforeStepRewritesInput、TestHookAppliesToServiceStep；`engine/flowhook_test.go`: TestBeforeFlowHookToolRewritesVars、TestOnSuccessHookRunsAfterSuccess、TestFlowHookTargetFilter |
| REQ-HOOK-REWRITE | §5.4 | Hook 可改写 input/output；**下游看到改后值**（after_step 改写对下游可见） | `implemented`（S3） | `engine/hook_test.go`: TestHookBeforeStepRewritesInput、TestHookAfterStepRewritesOutput |
| REQ-HOOK-REVALIDATE | §5.4/§9.1 | Hook 改写后**按 step 的 Tool schema 重新校验**（保住不可绕过） | `implemented`（S3） | `engine/hook_test.go`: TestHookBeforeStepRewriteRevalidated |
| REQ-HOOK-FAILCLOSED | §5.4 | critical 失败即终止（fail-closed）；best-effort 仅告警继续；**留空默认 critical** | `implemented`（S3） | `engine/hook_test.go`: TestHookCriticalFailureAborts、TestHookBestEffortFailureContinues；`model_test.go`: TestHookValidate |
| REQ-HOOK-ONERROR | §5.4 | on_error Hook 触发但**不能恢复/重试**主流程（输出被丢弃） | `implemented`（S3） | `engine/hook_test.go`: TestHookOnErrorDoesNotRecover |
| REQ-HOOK-NORECUR | §5.4 | Hook 执行不再触发 Hook（结构性无递归） | `implemented`（S3） | `engine/hook_test.go`: TestHookNoRecursion |
| REQ-HOOK-SELECT | §5.3 | Target（精确 tool id 或 `*`）+ Order 升序装配；无隐式依赖 | `implemented`（S3） | `engine/hook_test.go`: TestHookTargetFilter、TestHookOrder |
| REQ-HOOK-VALID | §5(F3) | Hook.Validate 注册期守卫：stage 合法、action/target/id 非空、criticality 合法；before_promote 已开放（S5 实现） | `implemented`（S5） | `model_test.go`: TestHookValidate；`registry_test.go`: TestHookRoundtrip |
| REQ-HOOK-CLI | §11/§14.6 | Hook 落库 + CLI（`gear hook register/list/show/remove`） | `implemented`（S3） | `registry_test.go`: TestHookRoundtrip（含 Get/Delete/拒绝非法） |
| REQ-RUN-RESULT | §9.1 | `RunFlowResult` 返回结构化 step input/output（补上「可观测性为零」缺口） | `implemented`（S3） | `engine/hook_test.go`（全部用例经 RunResult 断言） |
| REQ-PARALLEL | §4.3 | 无依赖 step **默认并行**调度（分层执行） | `implemented`（S4） | `engine/parallel_test.go`: TestParallelExecution、TestMaxParallelOneSerializes、TestMaxParallelTwoBatches |
| REQ-DEPS-INFER | §4.3 | 依赖推断：input 中的 `${steps.X.…}` 即隐式依赖 X（并行下保证时序） | `implemented`（S4） | `engine/deps_test.go`: TestInferDepsFromInput、TestInferDepsNested；`parallel_test.go`: TestParallelWithInferredDeps |
| REQ-PARALLEL-LIMIT | §14.5 | 并发上限 `WithMaxParallel` / `--max-parallel`（默认 4，背压） | `implemented`（S4） | `engine/parallel_test.go`: TestMaxParallelOneSerializes、TestMaxParallelTwoBatches |
| REQ-PARALLEL-SAFETY | §4.3 | 并行下 `runContext.steps` 读写安全（`-race` 无告警） | `implemented`（S4） | `engine/parallel_test.go`: TestParallelStepResultsComplete（`-race`） |
| REQ-PARALLEL-FAILCLOSED | §4.3/§5.4 | 同层某 step 失败即取消本层、后续层不执行（fail-closed） | `implemented`（S4） | `engine/parallel_test.go`: TestParallelErrorStopsLaterLevels |
| REQ-RESTART-BACKOFF | §14.3 | 崩溃循环**指数退避**（50ms 起翻倍，上限 5s；存活 ≥1s 重置） | `implemented`（S4） | `app/backoff_test.go`: TestBackoffFor、TestNextBackoffResetsWhenStable、TestSuperviseBackoffThrottlesCrashLoop |
| REQ-WHEN | §4.1/§4.3 | `when` 条件执行（表达式引擎 expr-lang/expr） | `implemented`（when 切片） | `engine/when_test.go`: TestWhenTrueRuns、TestWhenFalseSkipsStep、TestWhenUsesStepOutput |
| REQ-WHEN-THREE-STATE | §4.3(F6) | **三态语义**：跳过 ≠ 失败；`RunResult.Skipped` 可观测 | `implemented` | `engine/when_test.go`: TestWhenFalseSkipsStep（Flow 仍成功 + Skipped 记录）、TestWhenSkipsServiceStep |
| REQ-WHEN-CASCADE | §4.3(F6) | 跳过沿**有效依赖**（含推断边）级联传播 | `implemented` | `engine/when_test.go`: TestWhenCascadeSkipExplicitDep、TestWhenCascadeSkipInferredDep |
| REQ-WHEN-TYPED | §4.3(F7) | 表达式**带类型**：不使用 `${}` 插值，字符串 `"200"` ≠ 数字 `200` | `implemented` | `engine/when_test.go`: TestWhenKeepsTypes、TestWhenRejectsTemplateSyntax；`model_test.go`: TestFlowValidate（when 误用 `${}`） |
| REQ-WHEN-INFER | §4.3(F8) | `when` 里的 `steps.X` 引用同样进依赖推断（且不误判字符串字面量） | `implemented` | `engine/deps_test.go`: TestInferDepsFromWhen、TestInferDepsWhenUnknownRef |
| REQ-WHEN-COMPILE | §4.3(F3 延伸) | 注册期编译校验 `when`（语法错误不推迟到运行期） | `implemented` | `cli/flow.go` 前置 `CompileWhen`；`engine/when_test.go`: TestWhenCompileError、TestWhenNonBoolRejected |
| REQ-RUN-SKIPPED | §9.1(§0 第 11 条) | `RunResult.Skipped`：跳过原因结构化可见 | `implemented` | `engine/when_test.go`（全部跳过类用例经 Skipped 断言） |
| REQ-DEL-TOOL-GUARD | §8.2 | 注销 Tool 守卫：被 Flow step / Hook action 引用则拒绝（`ErrInUse`→409） | `implemented`（S6a） | `registry_test.go`: TestDeleteTool；`server_test.go`: TestDeleteToolInUse、TestDeleteToolUsedByHook |
| REQ-DEL-TOOL-RUNNING | §8.2 | 注销守卫第三条：**运行中** Execution 持有的 Tool 不可注销（注入 `RunningChecker`；执行结束后可再次尝试） | `implemented`（本轮） | `registry_test.go`: TestDeleteToolRunningGuard；`server_test.go`: TestDeleteToolWhileRunning（运行中 409 → 结束后 200）；`engine/planned_test.go`: TestRunFlowResultObservedReportsToolIDs（运行侧上报计划用到的 Tool，含仅作 Hook action 的） |
| REQ-FLOW-STATIC-CHECK | §9.2 | Flow 静态校验：结构 → 依赖图(环/未知) → Tool 存在 → when 可编译 → 「能力.方法」的方法名与参数可校验（CLI/REST/`ext.ai` 扩展共用） | `implemented`（S6a + M-EXT-AI 复用） | `server_test.go`: TestFlowsAndValidate；`internal/engine/validate_static.go` 承载 `engine.ValidateFlowStatic`（agent 内 ≤N 重试复用同一校验；ext.ai 现已进程内化，无独立 sidecar 二进制，sidecar 缺失时 fail-closed） |
| REQ-EXT-METHOD-STATIC | §9.2 / §0(不静默降级) | **`ext.*` 的方法级 schema 进入 step 直调静态校验**（补此前缺口：ext 只校验能力名，方法名/参数要拖到侧车才报错）：`SetupExtensions` 把 `extensions.yaml` 的 `schemas` 经 `applyExtSchemas` 转存进 `CapabilityRegistry.schemas`，`checkCapabilityMethod`/`validatePrimitiveArgs` 据此在**注册期**拒绝未知方法与未知/缺必填参数；无 `schemas` 的程序化 ext 退化为名字级校验（显式降级，非静默放过）——与内置原语、REST 注册面、`InvokeCapability`、`ext.ai` 生成校验**同源** | `implemented`（2026-09-19） | `engine/capability_validation_test.go`: TestExtMethodLevelStaticValidation（注册期 + `InvokeCapability` 运行期入口同源）、TestExtWithoutSchemasDegradesToNameCheck（退化显式）；守卫 `TestBuiltinParamOrderMatchesSchemas`（位置参数顺序表 ↔ schema 双向一致）、`TestGatedLuaBindingsCoverSchemas`（目录声明的方法在 Lua 侧真有绑定） |
| REQ-PURE-FUNC | §10（延伸） / ATOM_ROADMAP §0 | **纯函数核心原子 `time`/`template`/`crypto`**：内核内建、始终可用、**不受 capability 门禁**（声明成 capability 反而被 `GrantsFromStrings` 拒）。既是一等调用目标（Flow 可 `call: crypto.hash`、注册期静态校验、能力目录 `kind=pure`），也经 Lua 无条件注入（`ctx.time.*`，方法名由 schema 键自动生成）。要点：单具名 table 入参（避免 `builtinParamOrder` 第二份清单漂移）、实现自带入参校验（未知枚举/缺必填/非法长度一律报错，不静默兜底）、`time` 内嵌 `time/tzdata` 不依赖宿主 zoneinfo、`crypto` 密钥由调用方经 `secret` 传入（原子不读 env） | `implemented`（2026-09-19） | `engine/purefunc_test.go`（sha1/sha256/sha512/md5 国标向量、RFC 4231 HMAC 向量、AES-GCM 轮转与错误密钥/AAD 失败、随机与非随机 nonce、base64 往返、未知枚举/非法长度/缺必填全部报错）、`capability_validation_test.go`: TestPureFuncStepDirectCallEndToEnd（Flow 直调端到端 + 未知方法/多余参数注册期拒）、TestLuaPureFuncsUnconditional / TestLuaPureFuncsFailClosed、TestPurePrimitivesNotGated、TestPureFuncsNotDerivedAsCapabilities、TestCapabilityCatalogListsPureAtoms |
| REQ-API-REST | §11 | REST 执行面（14/15 端点）：healthz / tools(CRUD) / flows(CRUD+validate) / hooks / executions(+列表) / apps | `implemented`（S6a+S5） | `server_test.go`（端点契约测试，含状态码 400/404/409/422）；注：原 `ai-infer` 端点随 AI 去内置化删除，AI 入口改为 `ext.ai` 扩展（见 REQ-API-AI） |
| REQ-API-ASYNC-EXEC | §11(F10) | 异步执行：POST 202 + id，GET 查询 running/succeeded/failed + RunResult | `implemented`（S6a） | `server_test.go`: TestExecutionLifecycle、TestExecutionFailure、TestConcurrentExecutions |
| REQ-API-APPS | §11/§14 | REST 托管 Flow 进程：POST/GET/DELETE apps（cancel ⇒ Supervisor 停服务） | `implemented`（S6a） | `server_test.go`: TestAppsLifecycle |
| REQ-API-SHUTDOWN | §0 第 14 条(F11) | 优雅停机：cancel + **等待子任务收敛**（避免 registry 关闭竞态） | `implemented`（S6a） | `server_test.go`: TestAppsLifecycle（含 `Shutdown` 等待） |

| 需求 ID | PRD 章节 | 行为 | 状态 | 处置 |
|---------|---------|------|------|------|
| REQ-USEVERSION | §4.2/§7 | step 钉版本 `use_version` | `implemented`（S5） | `engine/version_test.go`: TestRunFlowResolvesUseVersion、TestRunFlowDefaultVersion、TestValidateFlowStaticUnknownVersion；`model/version_test.go`: TestFlowValidateAllowsUseVersion |
| REQ-HOOK-PROMOTE | §5.3/§7 | `before_promote` 阶段 Hook（版本晋升前） | `implemented`（S5） | `engine/version_test.go`: TestPromoteToolBeforePromoteGate；`model/version_test.go`: TestHookValidateAllowsBeforePromote |
| REQ-HOOK-ACTION-FLOW | §5.3 | Hook 的 action 指向 **Flow**（PRD §5.3 原文「引用的 Flow/Tool」） | `implemented`（#72） | `engine/flowhook_test.go`: TestBeforeFlowHookFlowInline、TestBeforeFlowHookFlowNoInfiniteRecursion、TestBeforeFlowHookFlowCycleGuarded；同步内联子 Flow（同 execID 上下文，结构性无递归），before_flow 时子 Flow 结果暴露进主 Flow vars（key=子 Flow id）；递归守卫拦截循环调用；仅 once 模式（RunFlowExec）生效，service 长驻模式 Flow 级 Hook 仍 deferred |
| REQ-CTX | §10/§14.1 | 宿主能力 `ctx.*`（受门禁 7 个：http/call/db/kv/secret/event/subscribe；另有纯函数原子 time/template/crypto，始终可用、不受门禁——见 REQ-PURE-FUNC） | `implemented`（M5，#69 收口） | `internal/cap` 包 + 引擎按 `Tool.Capabilities` 精确放行；lua 经 `setupLuaCapabilities`、process 经 `withHostCapabilities` 注入；**db 原语已实现**（`internal/cap/sqldb.go`，modernc.org/sqlite，文件或内存 DSN）；**引擎默认宿主装配（#69）**：`New` 在未显式 `WithHost` 时注入 KV/HTTP/Event/DB 原语，并始终注入 `call` 解析器（Tool/Flow 目标，复用祖先链循环守卫），声明了对应 capability 的 Tool 在生产环境真正可用，不再恒返回 `ErrCapabilityUnavailable`；测试见 `engine/lua_cap_test.go`、`engine/cap_integration_test.go`、`cap/cap_test.go`、`engine/host_default_test.go`（#69 新增 7 例）。注：secret 引擎默认仍无密钥源（nil → Unavailable，fail-closed）；Server 构造期经 `WithSecret` 注入 `cap.SecretStore`（REQ-API-SECRETS）；process/go-host 运行期绕过 capability（P0-4）仍 deferred |
| REQ-FLOW-PRIMITIVE-CALL | §4.1/§10（延伸 §11 capabilities/invoke） | Flow step 的 `call` 除 `<已注册 Tool id>` 外，可直接写**「能力.方法」**（`kv.put` / `http.Request` / `db.query` / `call.Call` / `secret.get` / `event.notify` / `ext.ai.complete` …），无需先包一层 Tool；两载体在引擎内统一为 `resolvedCall`，经同一 `resolveStepCall` 解析（**Tool 优先**，避免同名遮蔽既有语义），执行复用 `POST /api/capabilities/invoke` 的同一 Lua 通道（`buildActionEntry` + 一次性 `primitiveHost`，「写声明即授权」最小权限）。输出统一包成 `{"result":...}`（无返回值显式 `null`），故下游 `${steps.x.output.result}` 对 Tool/原语一致；`use_version`/`mode=service` 对原语注册期即拒（fail-closed；其中 `use_version` 那一半此前**漏实现**、被静默忽略——用户以为钉了版本实际没钉，2026-09-19 补齐，回归 `TestPrimitiveStepRejectsServiceAndUseVersion`）。**同源判定**：REST 注册面 / CLI / `ext.ai` 生成校验与运行期共用 `resolveStepCall` + `validatePrimitiveArgs`（缺必填、未知参数注册期即拒）——修复了 REST/`ai_validate` 曾用未装配的 `engine.New(s.reg)` 导致 `ext.*` 注册期被误判 400 的判定漂移 | `implemented`（2026-09-16） | `engine/stepcall.go`（`resolvedCall`/`resolveStepCall`/`checkCapabilityMethod`/`methodSchema`/`validatePrimitiveArgs`/`primitiveHost`；原 `engine/primitive.go` 已并入本文件）+ `builtinCapabilitySchemas`（受门禁 7 原语 + 纯函数 3 原子的方法级 schema 单一来源）与 `builtinParamOrder`（位置参数顺序）——两份表的一致由 `TestBuiltinParamOrderMatchesSchemas` 锁死，且 `TestGatedLuaBindingsCoverSchemas` 断言每个声明的方法在 Lua 侧真有绑定；`hostFor`→`hostForChain`/`withCallChain` 统一 callChain/session 注入；`stepExecutable` 分流 Tool/原语；`ai_agent.go` `buildActionEntry` 对 `call` 用**裸函数** `ctx.call(...)`（`bareFunctionPrimitives` 单一来源，守卫见下）。覆盖：`engine/primitive_step_test.go`（6 例：Tool 优先解析 / 拒 `use_version`·`mode=service`（含 Tool 载体反例）/ 原语 step 的 Hook target 匹配 / 直调宿主只放行该能力 / 端到端 `{result}` 包装 + `${steps.x.output.result}` 串联 / 未知能力·未知方法·缺必填·多余参数的可操作错误）、`capability_validation_test.go`（ext 方法级校验、纯函数原子直调、`TestGatedLuaBindingsCoverSchemas`、`TestBuiltinParamOrderMatchesSchemas`）、`host_default_test.go`（默认宿主 `ctx.call` 全路径，含循环守卫）、`server_test.go`: TestFlowsAcceptPrimitiveCalls（含 `ext.ai.capabilities` 经 REST 注册 201，即判定漂移回归）；前端 `FlowsView.vue` call 下拉新增「原子能力直调」分组 |
| REQ-RUNTIME-GOHOST | §3.2 | go-host runtime（进程内 Go 函数） | `implemented`（#76） | `internal/engine/gohost.go`：`HostFunc` 类型 + 全局注册表（`RegisterHostFunc` / `HostFuncFor`）；`goHostRuntime` 实现 `Runtime` 接口——`Run` 按 `entry` 寻址并执行注册函数（未注册明确报错）、`Start` 显式报错「不支持 service」（长驻请用 process）。`model.ImplementedRuntimes` 登记 go-host；`runtimeFor` 加 case。host 视图透传给函数（与 lua/process 一致，M5 最小权限）。覆盖：`engine/gohost_test.go`: TestGoHostRuntimeRun / TestGoHostRuntimeRunUnknownEntry / TestGoHostRuntimeStartUnsupported / TestGoHostRuntimeReceivesHost / TestGoHostRuntimeNilOutput / TestGoHostToolRunsThroughFlow |
| REQ-RUNTIME-WASM | §3.2 | wasm runtime | `deferred`（V3） | 已知但未实现：`Tool.Validate` 注册期拒 + `runtimeFor` 报错；用户确认 #76 仅做 go-host、wasm 维持 deferred（需引入 wazero + 可跑的 WASM guest 工具链才能写通测试，ROI 当前不足）。测试 TestSpecRejectsUnimplementedRuntimeTool、TestRuntimeForRejectsUnimplemented 仍覆盖其「未实现」语义 |
| REQ-LUA-SERVICE | §14.2 | lua 的 service（长驻）模式 | `deferred`（依赖 M5 ctx.*） | `luaRuntime.Start` 显式报错「不支持 service」；测试 TestLuaServiceUnsupported |
| REQ-LUA-SANDBOX-CONFIG | §3.2 | lua 沙箱策略可配置：拦截哪些「危险行为」交给用户抉择（不是硬编码严格模式） | `implemented` | 引擎级 `WithSandbox(strict\|permissive)` 透传；strict（默认）仅 base/string/table/math，permissive 额外开 coroutine；文件加载全局（dofile/loadfile/require/module）任何策略都禁（防逃逸）。`TestLuaSandboxPermissive` 覆盖。 |
| REQ-SVC-RUNTIME | §14.2 | 非 process runtime 的 service step | `deferred` | 注册期已挡住未实现者；service 与 once 共用 `runtimeFor`，runtime 实现 Start 后自动生效 |
| REQ-APP-HEALTH | §14.3 | service 健康检查（HTTP/TCP/命令探针） | `implemented`（#75） | Tool 声明 `health_probe`（kind: http/tcp/cmd，addr/path/cmd/interval/timeout/threshold）；Supervisor 每 service 句柄起探针 goroutine，连续失败达阈值只杀当前进程、由既有 Relaunch 路径重启（共用崩溃退避）；Proc 读写加锁避免探针与 Relaunch/停机并发竞争。注册期 Resolve 校验配置。覆盖：model_test: TestHealthProbeResolve/TestToolValidateHealthProbe、supervisor_test: TestProbeRestartsUnhealthyService/TestProbeNoRestartWhenHealthyNoProbe |
| REQ-CONCURRENCY-CROSS | §14.5 | 跨进程的执行入口队列 / 进程池（背压排队、拒绝策略） | `implemented`（#73） | `internal/server/gate.go` 的 `ConcurrencyGate`：固定 worker 池（= 最大并发执行数），待执行任务入有界队列，worker 全忙且队列已满时 `Submit` 返回 false → 入口拒绝（背压）回 `429`、执行记录置 `StatusRejected`（供查询可见，区别于 running/succeeded/failed）；`Server.New` 默认构造闸门（64 worker / 256 队列），支持 `WithMaxConcurrent` / `WithExecQueueSize` 调参；`Server.Shutdown` 调 `gate.Stop()` 等在途执行收敛后再关库。区别于 S4 step 级 `maxParallel`（限单 Flow 内 step 并发）与 S2 `app run --max`（限 Flow 数）。覆盖：`server/gate_test.go`: TestConcurrencyGateRunsAllUnderCapacity / TestConcurrencyGateRejectsWhenFull / TestConcurrencyGateStopWaitsForInFlight；`server/server_test.go`: TestHandleCreateExecutionRejectedWhenGateFull / TestHandleCreateExecutionAcceptedUnderCapacity |
| REQ-TMPL-SECRETS | §4.3 | 模板前缀 `${secrets.x}`（secret 原语随 ctx.* 落地，M5） | `implemented` | 执行期经宿主 secret 源解析（runContext.secretGet ← Engine.secretResolver，来自 cap.Providers.Secret）；Flow 定义只存引用、明文不落库。注册期放开该前缀（TestValidateTemplatesAllowsSecrets），密钥缺失运行期报错（TestSpecSecretsTemplateAllowedAtRegister / TestExpandStringResolvesSecrets）。注：secret 经进程 stdin/argv 暴露仍属 P0-4 风险（见 §189），本实现仅解决「引用可用」 |
| REQ-API-SECRETS | §11/§6 | 密钥管理面：REST `/api/secrets`（PUT 写入 / GET 列名 / DELETE 删除）+ `GEAR_SECRET_*` 环境变量播种；运行期 Tool 经 `ctx.secret.get` 只读消费、Flow 经 `${secrets.x}` 引用 | `implemented`（2026-09-16） | `cap/secretstore.go` 的 `SecretStore`（进程内存态、重启即失、明文绝不落库）；`Server.New` 构造 store → `SeedFromEnv()` 播种 → `engine.WithSecret` 注入（host.Secret 为 nil 时覆盖；显式 WithHost 自带 Secret 则尊重调用方）。管理面 GET 只回密钥名列表，任何响应不回明文（测试断言泄露即 fail）。**不给沙箱插件暴露写接口**：写权限是持久化污染（一次覆盖即可劫持后续所有使用者的凭据），「只写不读」防不住——写接口必然接收明文；维护属宿主/运维职责。覆盖：`server/secrets_test.go`: TestSecretsManagementAPI / TestSecretsEndToEnd / TestSecretsSeedFromEnv |
| REQ-API-PROMOTE | §11 | 版本晋升（状态机 + `before_promote` 门禁） | `implemented`（S5→REST） | CLI `gear tool promote <id> <v> <target>` 与 REST `POST /api/tools/{id}/versions/{v}/promote` 均已实现，均走 `engine.PromoteTool`（before_promote 门禁 fail-closed）；状态机语义在 `model.CanPromote` + `registry.PromoteToolVersion` 落地 |
| REQ-API-AI | §11/§9 | AI 生成 DSL（自然语言 → Flow/Tool）：以 **`ext.ai` 扩展原语（Sidecar）** 承载，不再做进内核；严格分层（LLM 层 / Agent 层[Lua] / 静态原语层[Go]）+ **`ext.registry` 进程内原语**（agent 自我锚定，读真实工具目录）+ **配置化扩展加载**（取代写死 `ext.ai`） | `ext`（2026-09-14 去内置化 + 三层分层 + 配置化加载落地，M-EXT-AI / M-EXT-AI-LAYER） | 移除 built-in：`internal/ai` 包（Inferrer 接口 / Ollama / SiliconFlow / GenerateFlow）、CLI `ai` 子命令、`server` 的 `inferrer` 字段与 `NewWithInferrer`、`POST /api/ai/infer` 端点与 `handleInfer`；改由进程内 `ext.ai` 扩展（`internal/engine/ai_extension.go` 经 `SidecarExtension` 注册 `ext.ai`，暴露 `complete/catalog/domains/validate`；sidecar 二进制缺失 fail-closed → `ErrCapabilityUnavailable`）：`complete` 参数化 LLM[provider/model/base_url/api_key/temperature/format 任意模型，`mock` 离线]、`catalog` BM25 域内检索、`domains` 按 namespace 域概览、`validate` 复用 `internal/engine/validate_static.go` 的 `ValidateFlowStatic`；SiliconFlow 默认 + Ollama 兜底，key 走 sidecar env。**agent 编排（从 `ext.registry` 读真实工具目录 → 能力域概览 → LLM 拆解 → 域内检索 → 组装 → `ValidateFlowStatic` ≤N 重试 → 人工确认/注册）于 2026-09-14 第十三次重构全部移入 Lua**（`examples/ai-create-flow.yaml` 的 `ai-gen` 入口），改 agent 行为不重编 Go；**第十四轮补 `ext.registry` 进程内原语（`ctx.ext.registry.tools()/flows()` 读引擎 registry，agent 不再由调用方喂 tools）+ `SetupExtensions` 配置化加载（`extensions.yaml` / `GEAR_EXT_CONFIG`，取代写死 `Register("ext.ai", ...)`，发布后加 `ext.tls` 等只改配置）**。不自由 ReAct；AI 只生成 DSL、不运行时代码、不访问 secret、不晋升。复用 M-KV-LIFECYCLE 的 session 作用域做 agent 中间态跨 step 缓存 |
| REQ-API-LOGS | §11 | `GET /api/executions/{id}/logs` | `implemented`（#71） | 端点已实现；processRuntime 把 step 的 stdout/stderr 行经 `Engine.WithLogSink` → `ExecStore.Logs` 落盘（仅文本、跳过空行），供查询；红线：日志只含运行期输出、绝不落 input/secret 明文。覆盖：`server_test.go`: TestExecutionLogs |
| REQ-EXEC-PERSIST | §11(§0 第 14 条) | 执行记录持久化（跨重启保留历史） | `implemented`（#70） | `ExecStore` 改为 modernc.org/sqlite 持久化（环境变量 `GEAR_EXEC_DB`，缺省 `build/gear.db`，已在 .gitignore 忽略）；重启可恢复全部执行历史与日志；`Server.Shutdown` 关闭 SQLite 句柄释放 WAL 侧车文件。覆盖：`server_test.go`: TestConcurrentExecutions、TestExecutionLogs |
| REQ-FLOW-IO | §4.1 | Flow 级 input/output 声明 | `contradicted→fixed` | 模型无此字段，PRD §4.1 示例已移除 |

---

## MCP 执行面需求（第四种执行面，2026-09-16）

| 需求 ID | PRD 章节 | 行为 | 状态 | 断言测试 |
|---------|---------|------|------|---------|
| REQ-MCP-SERVER | §11(扩展) | 第四种执行面：MCP Server（stdio 默认 / `--http` streamable HTTP）暴露 Gear 目录与执行能力 | `implemented` | `internal/mcp/mcp_test.go`: TestMCPAgentClosedLoop（in-memory 走真协议全闭环）、TestMCPServeHTTP（httptest + StreamableClientTransport 端到端） |
| REQ-MCP-TOOLS | §11(扩展) | 19 个 MCP 工具（目录读 / 配置写 / 校验 / 执行 / 查询），命名望文生义，与 REST 同源守卫 | `implemented` | `mcp_test.go`: TestMCPAgentClosedLoop（list/create/validate/run/delete 全走通）、TestMCPPromoteRejectsIllegalJump（状态机守卫）、TestMCPCapabilitiesTool（能力目录含内核 7 个受门禁原语——含 subscribe——以及 3 个 `kind=pure` 纯函数原子 time/template/crypto） |
| REQ-MCP-GUARD | §8.2 | MCP 面复用删除守卫与「运行中不可注销」：MCP 发起的执行被 `delete_tool` 的 runningChecker 看见 | `implemented` | `mcp_test.go`: TestMCPAgentClosedLoop（delete_tool 因引用被拒 in use）；runningChecker 在 `mcp.New` 注入，与 REST 面同款判据（`registry_test.go`/`server_test.go` 已锁） |
| REQ-MCP-RUN | §11(扩展) | MCP 执行类工具（run_tool/run_flow）同步返回执行记录，失败时返回记录（不抛错），vars 可覆盖 Flow vars | `implemented` | `mcp_test.go`: TestMCPAgentClosedLoop（run_flow 模板串联、run_tool 单工具）、TestMCPServeHTTP |
| REQ-MCP-REVERSE | §11(扩展) | 已注册 Flow 反向暴露为 `flow__{id}` MCP 工具，service 长驻 Flow 不暴露（须 `gear app run`） | `implemented` | `mcp_test.go`: TestMCPReverseExposeFlow（flow__ 可调、svc Flow 不暴露）、TestMCPServeHTTP（HTTP 下同一工具面） |
| REQ-REG-MULTIDOC | §0(单一真相源) | 多文档 YAML（`---` 分隔）只有**一处**解析实现 `registry.ParseYAMLDocs`，CLI `tool/flow register`、`serve --seed`、MCP `create_tool/create_flow` 全部复用；每个文档都要被解析，不得静默丢弃 | `implemented`（本轮） | `registry/loader_test.go`: TestParseYAMLDocsMultiDocLeadsWithComment / NoLeadingSeparator / SplitsToolAndFlow / ReportsBadDoc / SplitYAMLDocsExported；CLI 侧 `TestToolRegisterMultiDoc`、`TestFlowRegisterMultiDoc` |
| REQ-REG-ATOMIC | §0(不静默降级) | 批量注册/灌入**要么全成功、要么全不写**：`tool/flow register` 先全量校验再落库（失败报「未写入任何」）；`serve --seed` 按 Tool→Flow 落库并在中途失败时**回滚**（含恢复被覆盖的原值） | `implemented`（本轮） | CLI: TestToolRegisterMultiDocAtomic / TestFlowRegisterMultiDocAtomic / TestSeedExamplesRollbackOnBadFlow / TestSeedExamplesRollbackRestoresOverwritten；MCP: TestMCPCreateIsAtomic |
| REQ-MCP-ZEROVAL | §0(不静默降级) | MCP 工具返回的零值结构（nil map/slice）不得触发 SDK 输出 schema 校验失败：数据透传类工具的 `Out` 用 `any`（SDK 跳过推断校验）+ nil→空集合归一 | `implemented`（本轮） | `mcp_test.go`: TestMCPListFlowsWithoutVars（无 vars 的 Flow）、TestMCPCreateIsAtomic；MCP 面 28 项 stdio 冒烟 |
| REQ-MCP-ASSEMBLY | §0(装配顺序) | MCP Server 要求扩展名已并入全局 capability 集合，`mcp.New` 用 `extNamesReady` **fail-closed** 校验，不依赖「cliEngine 恰好先跑过」的隐式顺序 | `implemented`（本轮） | `mcp_test.go`: TestMCPNewRequiresExtensionsLoaded |
| REQ-MCP-WIRE-OBJECT | §11(扩展) / §0(不静默降级) | MCP `tools/call` 的 `structuredContent` 顶层**必须是 JSON 对象**（MCP 2025-06-18）：列表类结果经 `objectResult` 包具名键（`{tools:[...]}`/`{flows:[...]}`/`{hooks:[...]}`/`{executions:[...]}`/`{logs:[...]}`/`{capabilities:[...]}`）。SDK v1.8.0 依 SEP-2106 放行数组，严格客户端（WorkBuddy）会判 `expected record, received array` 并丢弃整条调用 | `implemented`（本轮，2026-09-19） | `mcp_test.go`: TestMCPAllToolsReturnObjectStructuredContent（逐工具真调 + ListTools 反查覆盖度）、callTool 内的对象形态断言；线上取证 `plugin/mcp_wire_probe.py` |

---

## 文档↔代码错位修正记录（F5）

| 编号 | 错位 | 性质 | 修正 |
|------|------|------|------|
| M3 | PRD §3.1 用 `"url": "string"` 简写 schema，代码只认 `type:object` 的 JSON Schema | 示例不可加载 | PRD §3.1 改为标准 JSON Schema；测试 TestSpecShorthandSchemaRejected 锁死 |
| M8 | PRD §4.1 用 `flow_id`/`version`，模型字段为 `id`（无 version） | 示例不可加载 | PRD §4.1 改为 `id`；测试 TestSpecRejectsOldFieldNames 断言 `flow_id` 被忽略 |
| M9 | PRD §4.1 用 `tool:`，模型字段为 `call` | 示例不可加载 | PRD §4.1 改为 `call:`；测试 TestSpecRejectsOldFieldNames 断言 `tool:` 被忽略 + 空 call 被拒 |
| M1 | `when` 条件执行被引擎静默忽略 | 静默 no-op | 转为 `deferred` + `Flow.Validate` 显式拒绝（RC-D 反模式消除） |
| M2 | 「无依赖并行」实为纯串行 | 静默 no-op | PRD §4.3 注明 S1 串行、并行属 S4；未实现字段注册期被拒 |
| M6 | `use_version` 钉版本被静默忽略 | 静默 no-op | 转为 `deferred` + `Flow.Validate` 显式拒绝 |
| M10 | Runtime 仅 process，引擎硬编码 `runProcess`，无分发 | 架构债 | 抽 `Runtime` 接口 + `runtimeFor` 分发（`internal/engine/runtime.go`） |
| G1 | `Tool.Validate` 不校验 `Runtime`：`runtime: foobar`（拼写错）、`runtime: lua` 均能注册入库，错误推迟到执行期 | 违反 F3（注册期暴露） | `Tool.Validate` 加 runtime 校验（未知/未实现均拒）；`REQ-TOOL-RUNTIME-VALID` |
| G2 | M10 只完成一半：`Runtime` 接口只有 `Run`，service 路径硬编码 `tool.Runtime != process` 白名单 + 直调 `startProcess`，绕过接口 | 架构债（两处判定、接口缺 Start） | 接口加 `Start`+`Process` 抽象；service/once 共用 `runtimeFor` 唯一点；删硬编码白名单 |
| G3 | 「已实现 runtime」列表散落多处（model 枚举、runtimeFor、supervise 白名单） | 列表漂移风险 | 收敛到 `model.KnownRuntimes` / `ImplementedRuntimes` 单一来源（C） |
| M4 | Hook 机制零实现、引擎无接缝 | 架构债 | 标 `architecture-debt`，S3 建调用点 |
| M5 | `ctx.*` 宿主能力层不存在 | 架构债 → 已解决 | M5 已实现：`internal/cap` 包（7 个受门禁原语 + 最小权限放行）+ 引擎 `Runtime` 接口透传 `host *cap.Context` + lua `ctx.*` 暴露 + process `ctx.capabilities` 注入；**#69 收口**：db 原语实现（SQLite，见 `internal/cap/sqldb.go`）、引擎默认宿主装配（KV/HTTP/Event/DB）+ `call` 解析器（Tool/Flow + 循环守卫），生产环境声明即可用 |
| M11 | PRD §4.3 把 `${secrets.x}` 列为「支持的变量引用前缀」，而当时引擎仅实现 vars/steps | 曾为文档超前于代码（漂移）；**已随 REQ-TMPL-SECRETS + REQ-API-SECRETS 落地而闭合** | 三类前缀（vars/steps/secrets）现已全部实现：模板解析 `internal/engine/template.go`（`prefixSecrets` + `ctx.secretGet`）、密钥源 `internal/cap/secretstore.go`、管理面 REST `internal/server/secrets.go`。回归：`TestValidateTemplatesAllowsSecrets` / `TestSpecSecretsTemplateAllowedAtRegister`（注册期接受该前缀）、`TestSecretsEndToEnd`（REST 写入 → `${secrets.x}` 运行期展开）、`TestValidateTemplatesRejectsUnknownPrefix`（未知前缀仍拒）。**注**：本条原写「secrets 未实现」并引用两名**从未存在**的测试（`TestValidateTemplatesRejectsSecrets` / `TestSpecSecretsTemplateRejectedAtRegister`，实际是 `...AllowsSecrets` / `...AllowedAtRegister`），2026-09-19 一并修正 |
| M12 | `use_version` 写在**直调原子能力**的 step 上被引擎**静默忽略**（只有 `mode=service` 被拒）——用户以为钉了版本，实际跑的是无版本语义的原语 | 静默 no-op（与 M1/M6 同类） | `resolveStepCall` 的直调分支补显式拒绝（注册期即报错，运行期共用同一解析点）；回归 `TestPrimitiveStepRejectsServiceAndUseVersion`（含 Tool 载体 `use_version` 仍合法的反例） |
| M13 | 本文档与 README 均声称「双表漂移由 `TestBuiltinParamOrderMatchesSchemas` 锁死」，该测试**并不存在**（`builtinParamOrder` 当时无任何守卫） | 文档声称的守卫缺失（漂移被文档掩盖） | 补 `TestBuiltinParamOrderMatchesSchemas`（位置参数顺序表 ↔ 方法级 schema 双向一致）并做「破坏后必失败」负向验证；另补 `TestGatedLuaBindingsCoverSchemas`（目录声明的方法在 Lua 侧真有绑定——顺带抓出 `call` 的裸函数形态，据此明确其断言口径） |
| M14 | 本文档引用 `engine/primitive.go` 与 `TestBuildActionEntryUsesBareCallForCallPrimitive`、`engine/primitive_step_test.go`（14 例）——前者已并入 `stepcall.go`，后两者不存在（整树重建时丢失） | 文档↔代码漂移 + 需求覆盖缺口 | 引用改为 `stepcall.go`；裸函数形态由 `bareFunctionPrimitives`（单一来源）+ `TestGatedLuaBindingsCoverSchemas` 守住；重建 `engine/primitive_step_test.go`（6 例聚焦用例），补齐「Tool 优先 / 拒 use_version·service / 原语 Hook target / 单能力授权 / `{result}` 包装串联 / 可操作错误」这几条此前**全无覆盖**的语义 |
| M15 | `build/ai_head.go` 是旧版 `cli/ai.go` 残片（调用已删除的 `/api/ai/infer`，引用不存在的 `openRegistry`/`execBaseURL`/`serverAddr`/`respStatus`），落在被 gitignore 的构建目录里，导致 `go build ./...` 与 `go vet ./...` **整体失败** | 构建闸门被残片污染 | 归档改名为 `build/ai_head.go.stale-relic-of-old-cli-ai`；`go build ./...` / `go vet ./...` 恢复零错误 |

---

## 测试文件缺口：已闭合 / 已收敛（2026-09-19）

> 本节由一次「文档引用的 Test 名 / 文件路径 ↔ 代码实际存在」的机械审计得出（`tools/doc_audit.py`）。
> 整树重建曾丢失若干测试文件，而本文档与 EXT_SERVICE_DESIGN.md 仍引用它们——即当时若干 `implemented`
> 需求**没有测试断言背书**，违反「凡 implemented 必须 ≥1 条测试」的闸门语义。
> 登记后已按「能补的补、能收敛的收敛」处理完毕，**没有靠改状态或删引用掩盖**。

### 一、已补回（测试重建；每条守卫都做过「破坏 → 断言必失败 → 还原」的负向验证）

| 重建的文件 | 受影响需求 | 覆盖 |
|---|---|---|
| `internal/server/secrets_test.go`（6 例） | **REQ-API-SECRETS** | `TestSecretsManagementAPI`（PUT/GET/DELETE 全生命周期 + 覆盖写 + 删除不存在 404）、`TestSecretsManagementRejectsBadInput`（缺/空 value、坏 JSON、含 `${}` 的名字 → 400 且零写入）、`TestSecretsEndToEnd`（REST 写入 → 运行期 `ctx.secret.get` 与 `${secrets.x}` 都取到明文）、`TestSecretsSeedFromEnv`（`GEAR_SECRET_*` 播种）、`TestSecretsNotInjectedFailsClosed`（三端点 503）、`TestServerRejectsIllegalSecretEnvName`（非法 env 名 → 启动失败） |
| `internal/registry/loader_test.go`（7 例） | REQ-REG-MULTIDOC（ATOMIC 由 CLI 侧 `TestSeedExamplesRollback*` 覆盖） | `TestParseYAMLDocsMultiDocLeadsWithComment`（前导注释块被跳过 + 两篇各归其类）、`TestParseYAMLDocsNoLeadingSeparator`（首篇无前导 `---` 的单文档写法）、`TestParseYAMLDocsSplitsToolAndFlow`、`TestParseYAMLDocsReportsBadDoc`（坏一篇即整体报错、**不返回半批**）、`TestParseYAMLDocsEmptyInput`、`TestSplitYAMLDocsExported`（只认单独成行的 `---`）、`TestParseYAMLDocsRealExample`（真实示例可解析**且**可注册） |
| `internal/engine/localhttp_test.go`（6 例） | EXT_SERVICE_DESIGN（service ext 传输面） | `TestLocalHTTPTransport_CallOK`（`POST /<method>` + `{"args":...}` 信封）、`TestLocalHTTPTransport_CallError`（侧车 error / 非 JSON 响应 / 不可达 三种 fail-closed）、`TestLocalHTTPTransport_ConnectionReuse`（5 次调用只新建 1 条 TCP 连接）、`TestLocalHTTPTransport_LuaRouting`（Lua `ctx.ext.*` 真的经 HTTP 打到侧车）、`TestSidecarExtension_FallsBackToCommandTransport`（非 service ext 仍走 once 子进程）、`TestServiceExtWithListenUsesLocalHTTPTransport`（声明 listen → 长连接） |
| `plugin/mqtt-sidecar/main_test.go`（6 例） | EXT_SERVICE_DESIGN 阶段三 | `TestPublish`、`TestSubscribe`（topic 计数去重 + 缺参数报错 + Subscribe 失败回滚）、`TestUnsubscribe`（计数归零才真取消 + 未知 handle 幂等）、`TestOnMessageWebhook`（按通配符匹配 handle 并 POST 回推）、`TestOnMessageFailureDoesNotAbortOthers`、`TestTopicMatches`（`+`/`#` 通配符语义） |

### 二、已收敛（文档口径改为现实；载体已被取代，不恢复）

| 项目 | 现实 | 收敛动作 |
|---|---|---|
| `internal/sidecar/mqttsvc/`（整包） | 实现已内聚进 `plugin/mqtt-sidecar/main.go`（`MQTTClient` 接口 + `Service`，不依赖 paho 故可纯单测）；单测**未随之迁移**，已在 `plugin/mqtt-sidecar/main_test.go` 重建 | EXT_SERVICE_DESIGN 阶段三的引用改指向 `plugin/mqtt-sidecar` |
| `examples/gear-db-sidecar/` | 源码示例已移除：内置 `db` 原语已原生支持 sqlite/mysql/postgres 多后端（`internal/cap/sqldb.go`） | README §6/§9、PRD §9.4、`extensions.example.yaml` 的 `ext.db` 段改为「示例已移除，用内置 db 原语；该段仅作自定义 sidecar 的形态参考（默认注释）」 |
| `internal/engine/jsonx.go` | `ctx.json` 实现已并入 `internal/engine/lua.go`（`decode`/`encode` 无条件注入） | 文档引用改为 `internal/engine/lua.go`；`ctx.json` 只是 Lua 侧无条件绑定，**不在**能力目录、也不是 Flow 直调目标（见 ATOM_ROADMAP §0 的口径澄清） |

**结论**：REQ-API-SECRETS、REQ-REG-MULTIDOC 与 service ext 传输面**已重新有测试背书**；
全仓不再有「文档声称有测试、实际没有」的条目（审计脚本 `tools/doc_audit.py` 可复跑核验）。

---

## 覆盖率闸门（宪法）

- `go test -race -cover ./...` 各包 ≥ 80%（宪法门槛，低于即阻断合并）。
  **实测（2026-09-20 补齐后，scripts/gate.sh ⑥ 全绿）**：engine **83.9%** ✅ / cap **90.7%** ✅ / mcp **83.6%** ✅ / cli **84.2%** ✅；server 85.8% ✅ / model 97.2% ✅ / registry 84.5% ✅。
  → **闸门 ⑥ 已由红转绿**：补齐前为 engine 61.6% ❌ / cap 63.0% ❌ / mcp 69.7% ❌ / cli 54.9% ❌。
  缺口形态不是「整个文件没人测」，而是「大函数里没人走到的分支」——按**未覆盖语句数**排序定位最划算
  （工具见 `tools/cov_report.py`，用法见 GEAR_工程方法_v1.md §5.2）。
- 关于历史数字：下一行记录的 S6b 实测（engine 87.7% / cli 76.5% 等）是**当时的快照**；整树重建丢失了一批 `*_test.go`
  （已补回 `server/secrets_test.go`、`registry/loader_test.go`、`engine/localhttp_test.go`、`plugin/mqtt-sidecar/main_test.go`，
  并新增 `internal/ext/email_test.go`、`engine/ai_opts_test.go`）；2026-09-20 本轮按「未覆盖语句数」逐块补测后，
  四个包全部越过 80%（新增文件见本节的回归锁条目）。cmd/gear 仍 0%（仅 `main()` 调 `cli.Execute()` 的入口，无独立逻辑）。
- 历史实测（S6b·CLI 单测，**仅供参考、已过期**）：engine 87.7% / app 86.0% / model 98.6% / registry 83.4% / server 83.9% / cli 76.5%（`-race` 全绿）。**以本轮实测为准**——数字只会因测试文件丢失而下降，不会因补测试以外的原因上行。registry/server 较 S6a 上行因运行中守卫包内单测（`TestDeleteToolRunningGuard` / `TestDeleteToolWhileRunning`）；engine 上行因 `template_test.go` / `planned_test.go`；cli 上行因本轮新增 `cli_test.go`（S6b）。
- 闸门语义：`deferred` 需求靠「断言被拒 / 显式报错」的测试计入覆盖，确保漂移可见。
- 回归锁：`registry_test.go` TestExampleFlowUsesDollarBrace 锁死 `${...}` 模板语法（D1）。
- S2 新增回归锁：`supervise_test.go` TestRunFlowRejectsServiceStep（service 不得混入 flow run）、TestServiceDepGuard（service 不得被依赖）。
- M10 修复新增回归锁：`TestSpecRejectsUnimplementedRuntimeTool`（未实现/未知 runtime 注册期必拒）、`TestRuntimeKnownImplemented`（Implemented ⊆ Known 不变量，防列表漂移）。
- S7a 新增回归锁：`TestLuaSandboxGlobals` / `TestLuaSandboxEscapeBlocked`（lua 沙箱不得接触宿主）、`TestLuaTimeout`（死循环必被 ctx 打断）、`TestLuaServiceUnsupported`（lua 不做 service）。
- S3 新增回归锁：`TestHookBeforeStepRewriteRevalidated`（Hook 改写后 schema 不可绕过）、`TestHookOnErrorDoesNotRecover`（on_error 不得恢复主流程）、`TestHookNoRecursion`（Hook 不得递归触发 Hook）。
- S4 新增回归锁：`TestMaxParallelOneSerializes`（并发闸门不可退化）、`TestParallelErrorStopsLaterLevels`（并行下失败仍 fail-closed）、`TestInferDepsFromInput`（隐式依赖不得因并行而乱序）。
- S4 踩坑回归锁：`TestSuperviseLaunchesService` —— service 进程**不得**绑定层级 ctx（否则层结束 `cancel()` 会立刻杀掉长驻进程）。
- 2026-09-19 补回丢失测试后的回归锁：`TestSecretsManagementAPI` / `TestSecretsEndToEnd` / `TestSecretsSeedFromEnv`（REQ-API-SECRETS：管理面全生命周期、`${secrets.x}` 端到端、`GEAR_SECRET_*` 播种；明文不回显为红线断言）、`TestParseYAMLDocs*` / `TestSplitYAMLDocsExported`（REQ-REG-MULTIDOC：多文档切分与 Tool/Flow 判别，坏 YAML 不返回半批）、`TestLocalHTTPTransport_CallOK/CallError/ConnectionReuse/LuaRouting` + `TestSidecarExtension_FallsBackToCommandTransport`（service ext 传输面：协议形状、三类 fail-closed、连接复用、Lua 路由、非 service 回退）、`plugin/mqtt-sidecar` 的 `TestPublish/TestSubscribe/TestUnsubscribe/TestOnMessageWebhook`（topic 计数去重与订阅失败回滚）。
- when 切片新增回归锁：`TestInferDepsFromWhen` / `TestInferDepsWhenUnknownRef`（**when 的引用必须进依赖推断**——曾因漏扫导致同层并发读到未产出输出）、`TestWhenKeepsTypes`（表达式不得因插值丢类型）、`TestWhenCascadeSkipInferredDep`（跳过必须沿推断边传播）。
- S6a 新增回归锁：`TestDeleteToolInUse` / `TestDeleteToolUsedByHook`（§8.2 注销守卫不可绕过）、`TestConcurrentExecutions`（ExecStore 并发安全，`-race`）、`TestAppsLifecycle`（Shutdown 必须等待子任务收敛）。
- 本轮（模板静态校验 + 运行中注销守卫）新增回归锁：`TestValidateTemplatesAllowsSecrets` / `TestValidateTemplatesRejectsUnknownPrefix` / `TestSpecSecretsTemplateAllowedAtRegister`（模板语法唯一解析器 `scanTemplateRefs`，前缀集合不得静默扩展——**注**：这三条建立时 secrets 尚未实现，故断言是「拒绝 secrets 前缀」；secrets 落地（REQ-TMPL-SECRETS）后语义翻转为「接受 vars/steps/secrets 三类、仍拒未知前缀」，测试名同步 Rejects→Allows。本条原写的 `TestValidateTemplatesRejectsSecrets` / `TestSpecSecretsTemplateRejectedAtRegister` **从未存在**，2026-09-19 修正）、`TestStaticValidateAcceptsFlowWithoutWhen`（修 `when` 空串被 `compileWhen` 误判 EOF 的既有 bug：无 when 的 Flow 必须能通过静态校验）、`TestDeleteToolRunningGuard` / `TestDeleteToolWhileRunning`（§8.2 第三条守卫：运行中不可注销，结束后方可）。
- 本轮（2026-09-20 全量核对）新增回归锁：`TestSendSMTPSSLNewClientErrorPropagates`（**SMTP SSL/465 路径的变量遮蔽 bug**：`conn, err := tls.Dial(...)` 在 if 块内新建了遮蔽外层的 err，使 `smtp.NewClient` 的错误写进出块即失效的变量 → 外层 `if err != nil` 恒为假、c 为 nil 仍继续，`defer c.Quit()` 直接 nil panic；破坏复辟时本用例会真的 panic，是「负向验证有效」的实证）、`TestExtAtomsMustNotReadEnv`（**红线守卫**：`internal/ext/*.go` 不得出现 `os.Getenv`——扩展原子凭据只从 args 传；本轮据此清掉了 sms/wecom 的 `GEAR_SMS_*` / `GEAR_WECOM_KEY` 回退与死代码 `intOrEnv`）、`TestGenerateAppliesLLMOverridesFromOpts`（`GenerateOpts` 的模型覆盖**必须真的生效**：CLI 填进 opts 的 `GEAR_AI_*` 曾因 `Generate` 未接线 `withLLMOverrides` 而静默失效；用例同时锁住「必须在组合 Eino 链**之前**替换 receiver」，因为 `compose.InvokableLambda(a.decompose)` 这类方法值在取值那一刻就绑定了 receiver）、`TestWithLLMOverridesReturnsSameAgentWhenEmpty` / `TestOverrideLLMOnlyFillsEmptyFields`（覆盖优先级：调用点显式传的 > opts 覆盖 > env > 默认）。
- MCP 执行面（第四种执行面）新增回归锁：`TestMCPAgentClosedLoop`（in-memory 走真协议覆盖「读目录→写配置→校验→执行→看结果」闭环 + 删除守卫拒绝 in use + 清理）、`TestMCPReverseExposeFlow`（flow__ 反向暴露可调、service 长驻 Flow 不暴露）、`TestMCPServeHTTP`（streamable HTTP transport 端到端，同一工具面）、`TestMCPPromoteRejectsIllegalJump`（版本状态机非法迁移被拒）、`TestMCPCapabilitiesTool`（能力目录含内核 7 个受门禁原语 http/call/db/kv/secret/event/subscribe + 3 个 `kind=pure` 纯函数原子）。
- 本轮（ext 方法级静态校验 + 纯函数原子 time/template/crypto）新增回归锁：`TestExtMethodLevelStaticValidation`（`ext.*` 未知方法与未知/缺必填参数**注册期**即拒，且与 `InvokeCapability` 运行期入口同源）、`TestExtWithoutSchemasDegradesToNameCheck`（无 schema 的 ext 显式退化为名字级，不是假装校验过）、`TestBuiltinParamOrderMatchesSchemas`（位置参数顺序表 ↔ 方法级 schema **双向**一致：schema 有方法而顺序表没有 → `buildActionEntry` 会编出 `ctx.kv.nope({...})`，注册期放行、运行期才炸；schema 有参数而顺序表没有 → 调用方传的值被静默丢弃）、`TestGatedLuaBindingsCoverSchemas`（门禁原语的 Lua 绑定是手写的，目录声明的每个方法在 `ctx.<prim>` 上必须真有绑定；裸函数原语 `call` 按裸形态断言，并要求它只能有一个方法）、`TestPureFuncStepDirectCallEndToEnd` / `TestLuaPureFuncsUnconditional` / `TestLuaPureFuncsFailClosed` / `TestPurePrimitivesNotGated` / `TestCapabilityCatalogListsPureAtoms`（纯函数原子**始终可用且不受门禁**，声明成 capability 反被拒；未知枚举/非 table 参数必须报错；目录里 `kind=pure`）、`TestPrimitiveStepToolTakesPrecedence`（同名字面量时 Tool 优先，原语不得静默遮蔽既有 Tool）、`TestPrimitiveStepRejectsServiceAndUseVersion`（原语载体不得静默忽略 `use_version`/`mode=service`）。
  > 本轮同时修掉三处**文档↔代码漂移**（都是「文档声称有、实际没有」）：① 本文档与 README 都声称存在 `TestBuiltinParamOrderMatchesSchemas`，实际不存在（本轮补上，并做了「破坏后必失败」的负向验证）；② 本文档引用 `engine/primitive.go`（文件已并入 `stepcall.go`）与 `TestBuildActionEntryUsesBareCallForCallPrimitive`（不存在的测试名）；③ `engine/primitive_step_test.go` 在整树重建中丢失、文档仍声称其 14 例覆盖（本轮重建为 6 例聚焦用例，只覆盖原文件确有的那几条语义）。另：`build/ai_head.go` 是旧版 `cli/ai.go` 残片（调用已删除的 `/api/ai/infer`），会让 `go build ./...` 整体失败，已归档改名（见下）。

- 本轮（2026-09-20 覆盖率补齐）新增回归锁，按包归口：
  - `cap`：`TestSecretStoreRejectsInvalidName` / `TestSecretStoreSeedFromEnvFailsClosedOnBadName`（密钥名非法即拒、环境变量播种**遇非法名必须返回 error**，绝不静默忽略）、`TestSecretStoreNamesSortedNamesOnly`（Names 只回名字、绝不回明文）、`TestContextSetCallChainIsPassedToResolver` / `TestContextSubscribeBindingWritesOwner`（setter 类不是「调过就算覆盖」——断言**下游拿到的值**：Resolver 收到的 via、Subscribe.Start 收到的 Owner）、`TestRegisterExtensionNameRejectsBuiltinAndUnprefixed`（扩展名不得遮蔽内置原语、必须带 `ext.` 前缀）、`TestScopedKVDeleteRoutesByScope` / `TestSessionRegistryRefCountRecycles`（session 键不污染 global、引用计数归零才回收）。
  - `mcp`：`TestServeHTTPListensAndShutsDownOnCtxCancel` / `TestServeHTTPRejectsUnusableAddr`（`ServeHTTP` 此前 **0% 覆盖**，而它正是 `gear mcp --http` 的真实入口：监听 + 信号优雅退出、端口被占必须报错）、`TestMCPErrorPathsReportInsteadOfSilentSuccess`（11 个工具的「不存在」入参一律报错）、`TestMCPRunFlowRejectsServiceFlow`（service 长驻 Flow 不暴露同步运行，且与 `REQ-FLOW-RUN-REJECTS-SERVICE` 同口径；service Flow 也不进反向暴露工具面）。
  - `cli`：`TestAICapabilitiesPrintsCatalog` / `TestAIReactRunsWithMockProvider` / `TestAIGenFlowAndToolWithMockProvider`（`gear ai` 三个子命令走 **mock provider** 离线跑通，不依赖真实 key）、`TestToolRunExecutesAndPrintsOutput` / `TestToolVersionsListsAndReportsMissing` / `TestToolPromoteRejectsIllegalStateThenPromotes`（`tool run/versions/promote` 此前无人执行）、`TestMCPHTTPTransportReportsBindFailure` / `TestMCPStdioTransportRunsUntilInputEOF`（两种 transport 的装配链路各测一次；stdio 在输入 EOF 后必须退出，否则客户端断开后进程残留）、`TestServeReportsBindFailure` / `TestAppRunReportsUnknownFlow` / `TestAppRunSupervisesThenReturns`（常驻命令的两种失败形态：端口起不来必须报错、监管不存在的 Flow 必须报错；普通 Flow 跑完应正常收尾）、`TestExtCmdReportsUnknownExtensionOnWire`（`gear ext` 未知名以**协议帧**报错，不靠退出码）。
  - `engine`：`TestReadStreamIdleTimeoutKillsStalledStream` / `TestReadStreamTotalTimeoutKillsTrickle` / `TestReadStreamRejectsRepetitionLoop` / `TestReadStreamHonorsContextCancel`（模型层两道判死闸：空闲超时卡死、总时长防滴流、重复输出检测含**推理片段**、ctx 取消）、`TestPostOpenAISendsExpectedRequestAndJoins` / `TestPostOllamaNativeProtocol` / `TestPostRejectsHTTPErrorAndEmptyStream`（两个 provider 的协议面：路径/请求头/请求体、非 2xx 与空流 fail-closed）、`TestDetectRepetition` / `TestExtractJSONIsStringAware`（重复检测不误报正常文本；JSON 截取**字符串感知**，模板 `${...}` 里的花括号不得算错边界）、`TestReactHappyPathExecutesToolThenAnswers` / `TestReactAllowListBlocksUnlistedTool` / `TestReactDuplicateCallIsStopped` / `TestReactMalformedOutputAndMaxSteps`（ReAct 四条护栏：不越权、不重复烧钱、格式不对回灌继续、跑满步数明说未收尾**不编造答案**）、`TestNewSourceFactoryRoutesKnownSources` / `TestSidecarSourceEngineKeysOverrideUserConfig`（事件源未知即拒；引擎生成的 handle/callback_url **后写覆盖用户同名键**，否则订阅再也回收不掉）、`TestCapabilitiesTextListsAllPrimitives` / `TestFlowCapabilitiesTextUsesBareCallForm`（提示词形态 + **可复现**：本轮据此修掉 ext 参数明细按 map 迭代输出导致的提示词漂移）。
