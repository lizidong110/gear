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
| REQ-TMPL-VALID | §4.3(F10) | 模板**静态校验**：注册期校验前缀合法（vars/steps）、vars 顶层键存在、steps 带 id；未知前缀（secrets）注册期拒（F3 延伸：能静态校验的不留到运行期） | `implemented`（本轮） | `engine/template_test.go`: TestValidateTemplatesOK/RejectsSecrets/RejectsUnknownPrefix/RejectsUnknownVarKey/RejectsBareSteps/RejectsUnclosed/DelegatesStepExistence；`validate_static.go` 接 `validateTemplates`；CLI `flow register` 前置 |
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
| REQ-HOOK | §5/§14 | Hook 生命周期（before_step / after_step / on_error）；once 与 service 共用同一执行路径 | `implemented`（S3） | `engine/hook_test.go`: TestHookBeforeStepRewritesInput、TestHookAppliesToServiceStep |
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
| REQ-FLOW-STATIC-CHECK | §9.2 | Flow 静态校验：结构 → 依赖图(环/未知) → Tool 存在 → when 可编译（CLI/REST/AI 闭环共用） | `implemented`（S6a） | `server_test.go`: TestFlowsAndValidate |
| REQ-API-REST | §11 | REST 执行面（14/15 端点）：healthz / tools(CRUD) / flows(CRUD+validate) / hooks / executions(+列表) / apps / ai-infer | `implemented`（S6a+S5+S6c） | `server_test.go`（端点契约测试，含状态码 400/404/409/422） |
| REQ-API-ASYNC-EXEC | §11(F10) | 异步执行：POST 202 + id，GET 查询 running/succeeded/failed + RunResult | `implemented`（S6a） | `server_test.go`: TestExecutionLifecycle、TestExecutionFailure、TestConcurrentExecutions |
| REQ-API-APPS | §11/§14 | REST 托管 Flow 进程：POST/GET/DELETE apps（cancel ⇒ Supervisor 停服务） | `implemented`（S6a） | `server_test.go`: TestAppsLifecycle |
| REQ-API-SHUTDOWN | §0 第 14 条(F11) | 优雅停机：cancel + **等待子任务收敛**（避免 registry 关闭竞态） | `implemented`（S6a） | `server_test.go`: TestAppsLifecycle（含 `Shutdown` 等待） |

| 需求 ID | PRD 章节 | 行为 | 状态 | 处置 |
|---------|---------|------|------|------|
| REQ-USEVERSION | §4.2/§7 | step 钉版本 `use_version` | `implemented`（S5） | `engine/version_test.go`: TestRunFlowResolvesUseVersion、TestRunFlowDefaultVersion、TestValidateFlowStaticUnknownVersion；`model/version_test.go`: TestFlowValidateAllowsUseVersion |
| REQ-HOOK-PROMOTE | §5.3/§7 | `before_promote` 阶段 Hook（版本晋升前） | `implemented`（S5） | `engine/version_test.go`: TestPromoteToolBeforePromoteGate；`model/version_test.go`: TestHookValidateAllowsBeforePromote |
| REQ-HOOK-ACTION-FLOW | §5.3 | Hook 的 action 指向 **Flow**（PRD §5.3 原文「引用的 Flow/Tool」，当前仅支持 Tool） | `deferred`（后续；需递归调引擎 + 完整嵌套守卫） | 现以「action = 已注册 Tool id」实现（用户选定方案 A），Flow 载体待评估 |
| REQ-CTX | §10/§14.1 | 宿主能力 `ctx.*`（http/call/db/kv/secret/event） | `implemented`（M5） | `internal/cap` 包 + 引擎按 `Tool.Capabilities` 精确放行；lua 经 `setupLuaCapabilities`、process 经 `withHostCapabilities` 注入；db 原语已定义但宿主未实现，调用返回 `ErrCapabilityUnavailable`；测试见 `engine/lua_cap_test.go`、`engine/cap_integration_test.go`、`cap/cap_test.go`。注：process/go-host 运行期绕过 capability（P0-4）仍 deferred |
| REQ-RUNTIME-GOHOST | §3.2 | go-host runtime | `deferred`（S7 后续） | `Tool.Validate` 注册期拒 + `runtimeFor` 报错；测试 TestSpecRejectsUnimplementedRuntimeTool、TestRuntimeForRejectsUnimplemented |
| REQ-RUNTIME-WASM | §3.2 | wasm runtime | `deferred`（V3） | 同上 |
| REQ-LUA-SERVICE | §14.2 | lua 的 service（长驻）模式 | `deferred`（依赖 M5 ctx.*） | `luaRuntime.Start` 显式报错「不支持 service」；测试 TestLuaServiceUnsupported |
| REQ-LUA-SANDBOX-CONFIG | §3.2 | lua 沙箱策略可配置：拦截哪些「危险行为」交给用户抉择（不是硬编码严格模式） | `deferred`（后期，依赖 M5 capability） | 用户 2026-09-12 明确要求：有些行为可能就是用户想要的，应由用户决定。当前 `luaAllowedLibs`/`luaForbiddenGlobals` 为硬编码常量；配置化后改为策略驱动（如 `sandbox: strict\|permissive` 或逐项 capability 白名单） |
| REQ-SVC-RUNTIME | §14.2 | 非 process runtime 的 service step | `deferred` | 注册期已挡住未实现者；service 与 once 共用 `runtimeFor`，runtime 实现 Start 后自动生效 |
| REQ-APP-HEALTH | §14.3 | service 健康检查（HTTP/命令探针） | `deferred`（S2+） | 当前仅存活（进程退出即视为不健康）；探针未实现 |
| REQ-CONCURRENCY-CROSS | §14.5 | 跨进程的执行入口队列 / 进程池（背压排队、拒绝策略） | `deferred`（后续） | step 级并发闸门已落地（REQ-PARALLEL / REQ-APP-MAXCONC）；跨进程池尚未实现 |
| REQ-TMPL-SECRETS | §4.3 | 模板前缀 `${secrets.x}`（secret 原语随 ctx.* 落地，M5） | `deferred`（M5） | 注册期显式拒绝未知前缀（`validateTemplates`）；测试 `engine/template_test.go`: TestValidateTemplatesRejectsSecrets、TestSpecSecretsTemplateRejectedAtRegister |
| REQ-API-PROMOTE | §11 | 版本晋升（状态机 + `before_promote` 门禁） | `implemented`（S5→REST） | CLI `gear tool promote <id> <v> <target>` 与 REST `POST /api/tools/{id}/versions/{v}/promote` 均已实现，均走 `engine.PromoteTool`（before_promote 门禁 fail-closed）；状态机语义在 `model.CanPromote` + `registry.PromoteToolVersion` 落地 |
| REQ-API-AI | §11/§9 | AI 闭环（自然语言 → Flow）：`POST /api/ai/infer` + CLI `gear ai gen [--register]` | `implemented`（S6c） | `internal/ai`（Inferrer 接口 + Ollama 客户端 + GenerateFlow 收敛重试≤3）；`server_test.go`: TestInferFlow；`cli/ai_test.go`: TestAIGenPrintsYAML、TestAIGenRegister；LLM 接入方式定为本地 Ollama（qwen3.5:4b，可配置） |
| REQ-API-LOGS | §11 | `GET /api/executions/{id}/logs` | `deferred`（需日志 Hook 产出） | 端点未实现；当前执行产物只有 RunResult |
| REQ-EXEC-PERSIST | §11(§0 第 14 条) | 执行记录持久化（跨重启保留历史） | `deferred`（有意选择内存态） | `ExecStore` 为进程内 map，重启即失；已在代码注释显式声明 |
| REQ-FLOW-IO | §4.1 | Flow 级 input/output 声明 | `contradicted→fixed` | 模型无此字段，PRD §4.1 示例已移除 |

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
| M5 | `ctx.*` 宿主能力层不存在 | 架构债 → 已解决 | M5 已实现：`internal/cap` 包（6 原语 + 最小权限放行）+ 引擎 `Runtime` 接口透传 `host *cap.Context` + lua `ctx.*` 暴露 + process `ctx.capabilities` 注入；db 原语 deferred 实现（声明可见、调用返回 `ErrCapabilityUnavailable`） |
| M11 | PRD §4.3 把 `${secrets.x}` 列为「支持的变量引用前缀」，引擎仅实现 vars/steps，secrets 未实现 | 文档超前于代码（漂移） | 引擎 `validateTemplates` 注册期显式拒绝未知前缀（secrets）；PRD §4.3 改为标注「未实现（REQ-TMPL-SECRETS）」；测试 `TestValidateTemplatesRejectsSecrets` / `TestSpecSecretsTemplateRejectedAtRegister` 锁死 |

---

## 覆盖率闸门（宪法）

- `go test -race -cover ./...` 各包 ≥ 80%：engine / model / registry / app / **server** 已达标；**cli 在 S6b 补到 76.5%**（管理面命令的成功/错误路径全覆盖，含 tool/flow/hook 全生命周期、tool unregister §8.2 守卫、flow validate/inspect、app 非法 --restart）；cmd/gear 仍 0%（仅 `main()` 调 `cli.Execute()` 的入口，无独立逻辑，归 S6b 收尾）。
- 最新实测（S6b·CLI 单测）：engine **87.7%** / app 86.0% / model 98.6% / registry **83.4%** / server **83.9%** / **cli 76.5%**（`-race` 全绿）。registry/server 较 S6a 上行因运行中守卫包内单测（`TestDeleteToolRunningGuard` / `TestDeleteToolWhileRunning`）；engine 上行因 `template_test.go` / `planned_test.go`；cli 上行因本轮新增 `cli_test.go`（S6b）。
- 闸门语义：`deferred` 需求靠「断言被拒 / 显式报错」的测试计入覆盖，确保漂移可见。
- 回归锁：`registry_test.go` TestExampleFlowUsesDollarBrace 锁死 `${...}` 模板语法（D1）。
- S2 新增回归锁：`supervise_test.go` TestRunFlowRejectsServiceStep（service 不得混入 flow run）、TestServiceDepGuard（service 不得被依赖）。
- M10 修复新增回归锁：`TestSpecRejectsUnimplementedRuntimeTool`（未实现/未知 runtime 注册期必拒）、`TestRuntimeKnownImplemented`（Implemented ⊆ Known 不变量，防列表漂移）。
- S7a 新增回归锁：`TestLuaSandboxGlobals` / `TestLuaSandboxEscapeBlocked`（lua 沙箱不得接触宿主）、`TestLuaTimeout`（死循环必被 ctx 打断）、`TestLuaServiceUnsupported`（lua 不做 service）。
- S3 新增回归锁：`TestHookBeforeStepRewriteRevalidated`（Hook 改写后 schema 不可绕过）、`TestHookOnErrorDoesNotRecover`（on_error 不得恢复主流程）、`TestHookNoRecursion`（Hook 不得递归触发 Hook）。
- S4 新增回归锁：`TestMaxParallelOneSerializes`（并发闸门不可退化）、`TestParallelErrorStopsLaterLevels`（并行下失败仍 fail-closed）、`TestInferDepsFromInput`（隐式依赖不得因并行而乱序）。
- S4 踩坑回归锁：`TestSuperviseLaunchesService` —— service 进程**不得**绑定层级 ctx（否则层结束 `cancel()` 会立刻杀掉长驻进程）。
- when 切片新增回归锁：`TestInferDepsFromWhen` / `TestInferDepsWhenUnknownRef`（**when 的引用必须进依赖推断**——曾因漏扫导致同层并发读到未产出输出）、`TestWhenKeepsTypes`（表达式不得因插值丢类型）、`TestWhenCascadeSkipInferredDep`（跳过必须沿推断边传播）。
- S6a 新增回归锁：`TestDeleteToolInUse` / `TestDeleteToolUsedByHook`（§8.2 注销守卫不可绕过）、`TestConcurrentExecutions`（ExecStore 并发安全，`-race`）、`TestAppsLifecycle`（Shutdown 必须等待子任务收敛）。
- 本轮（模板静态校验 + 运行中注销守卫）新增回归锁：`TestValidateTemplatesRejectsSecrets` / `TestValidateTemplatesRejectsUnknownPrefix` / `TestSpecSecretsTemplateRejectedAtRegister`（模板语法唯一解析器 `scanTemplateRefs`，前缀不得静默扩展——曾因 PRD 列了 `${secrets.x}` 而引擎未实现导致漂移）、`TestStaticValidateAcceptsFlowWithoutWhen`（修 `when` 空串被 `compileWhen` 误判 EOF 的既有 bug：无 when 的 Flow 必须能通过静态校验）、`TestDeleteToolRunningGuard` / `TestDeleteToolWhileRunning`（§8.2 第三条守卫：运行中不可注销，结束后方可）。
