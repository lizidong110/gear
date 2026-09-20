# GEAR 工程方法 v1 · PRD → 可信代码的宪法

> **定位**：本文件是后续每个切片（S0–S6）实现代码的**交付宪法**。任何切片在"可被测、可被追溯、过质量闸门、经评审"之前，视为未完成。
> **触发原因**：上一轮直接生成代码导致 6 处 PRD 漂移/缺失（见 §9），证明"没有需求→代码→测试强制绑定，代码会按模型直觉而非 PRD 真意漂移"。本文件即用来堵这个洞。
> **使用方式**：每切片先给「测试清单 + DoD」→ 实现 → 覆盖率报告 → **用户评审过关**才进下一刀。未签批本文件前，不写任何实现代码。

---

## 0. 核心原则（不可绕过的红线）

1. **需求可追溯**：每个 PRD 条目（§x.y）至少对应一个测试或不变量；未覆盖 = 未完成（见 §3 矩阵）。
2. **红—绿节奏**：对纯逻辑（状态机、解析、调度、依赖图）先写失败测试，再实现；不靠"我觉得对"。
3. **质量闸门不可跳过**：`go build` + `go vet` + staticcheck + 覆盖率门槛全进 CI；CI 红 = 不可合并（见 §5）。
4. **待确认项显式处置**：PRD §13 的 14 条，每条必须「定 / 搁置」并写明归属切片与理由，禁止静默默认（见 §7）。
5. **切片交付、评审过关**：S0–S6 每刀独立可验证，过关才继续；绝不"一把梭端出整套"。
6. **单一真相源**：PRD 定义的 Schema / 状态机 / 触发面，引擎校验、AI 上下文、前端表单共用同一份定义，不允许漂移（对应 PRD §9.3）。
7. **控制流三态不可混淆**：一个 step 的结局必须能区分 **执行成功 / 失败 / 被跳过（skip）**。跳过**不是错误**——它必须用与失败不同的类型/通道表达（`ErrStepSkipped` + `RunResult.Skipped`），否则下游只能靠猜（见 §11 F6）。
8. **表达式与模板分离**：数据替换用 `${}` 模板；**条件判断/比较必须用带类型的表达式引擎**，禁止「先把 `${}` 插值成字符串再 eval」（见 §11 F7）。
9. **引用解析必须与该语法的字面量规则一致**：凡是新引入的「引用」语法，都要同步进依赖推断；且解析要遵守该语法的字面量规则（表达式里没有 `${}`；字符串字面量内的 `steps.x` 不算引用）（见 §11 F8）。
10. **能静态校验的，不许留到运行期**：字段枚举、语法、表达式编译等，一律在**注册期**暴露（F3 的自然延伸）。
11. **新状态必须可观测**：引入新的执行状态（如 skip）时，必须同时给出结构化字段 + 断言测试；不可见的状态既测不了、也排不了障。
12. **方法论自身可追溯**：本文件每次改动都记入 §11 变更日志，写明「触发场景 / 改了什么 / 为什么」。否则方法论会像 PRD 一样悄悄漂移。
13. **时序断言禁用绝对阈值**：只在「负载只会让它更慢」的**安全方向**用绝对下界（如「耗时应 ≥ 800ms」）；任何**上界**判断（如「必须 < 900ms」）一律改为**同一进程内的相对比较**（先量串行基线再比并行）。理由：`go test ./...` 会并行跑包，机器负载会让绝对时长抖动，制造偶发失败——而被当作「偶发」忽略的 flake，最终会侵蚀对整套测试的信任（见 §11·2026-09-13 S6a）。
14. **状态的持久性必须显式声明**：引入任何新状态（执行记录、进程句柄、缓存）时，必须在代码与文档里写明它是**内存态**还是**持久化**、以及丢失条件。理由是「静默假设」的新变种——不是字段被忽略，而是**生命周期被默认**（见 §11·2026-09-13 S6a）。
15. **枚举式声明必须逐项登记**：任何「合法取值集合」（模板前缀 vars/steps、runtime 名、hook stage、criticality、step mode 等）必须（a）在单一来源登记全部合法值，（b）在解析/校验点拒绝未知值，（c）在 PRD/矩阵同步状态；三者缺一则构成漂移隐患（见 §11·2026-09-13 第三次）。理由是本次真实漂移——PRD §4.3 把 `${secrets.x}` 列为「支持的前缀」，而引擎只实现 vars/steps 且从未登记 secrets，于是「文档说有、代码没有」长期无人察觉，直到补上注册期模板静态校验才暴露。
16. **子进程不得继承调用方的敏感 fd**：任何由本进程拉起的子进程（尤其 `sh -c` 这类会再派孙进程的包装器），其 stdout/stderr **不得直接等于调用方的 `os.Stdout`/`os.Stderr`**——否则孙进程在父进程被 kill 后成为孤儿，继续持有调用方管道 fd，导致 `go test` 报 `Test I/O incomplete`、或生产环境 fd 泄漏。正确做法：子进程输出导向受控 sink（`/dev/null` 或专属日志文件），绝不把测试/服务管道交给它。理由：Windows 下 `TerminateProcess` 不级联子进程、msys 的 `sh` 也不转发信号，「杀得干净」不可靠，不如从根上不交出敏感 fd（见 §11·2026-09-13 第五次）。
17. **用 `sh -c` 让进程吐 JSON 时，外层单引号、JSON 内双引号**：process runtime 约定「stdout 末行即 JSON」。`sh -c '<entry>'` 经 Git Bash 的 `sh` 解析时，双引号会被**词法去引号**（`echo {"v":"1"}` → `{v:1}`，非法 JSON）；仅当 JSON 整体用**单引号**包裹（`echo '{"v":"1"}'`）时 `sh` 才原样保留。任何写 process entry / 测试 fixture 的人都必须遵守此约定，否则运行期 `解析 stdout 失败` 且极难排查（见 §11·2026-09-13 第六次）。

---

## 1. 工程分层与包结构（对应 PRD §10）

```
gear/
  cmd/gear/main.go            # 入口，仅调 cli.Execute()
  internal/
    model/                    # 数据模型（唯一真相源）：Tool/Flow/Step/Hook/Version
    registry/                 # 存储层：SQLite，JSON blob；实现 Registry 接口
    engine/                   # 编排层：DSL 解析 / Schema 校验 / 调度 / 变量解析 / Hook 触发
    runtime/                  # Runtime 适配层（实现 Runtime 接口）
      gohost/                #   进程内 Go 函数
      lua/                   #   嵌入式 Lua 沙箱
      process/               #   外部进程 stdin JSON → stdout 末行 JSON
    host/                     # ctx.* 宿主能力 7 原语（实现 Host 接口）
    server/                   # REST API（PRD §11），CLI 的同源服务端
    cli/                      # CLI 管理面（PRD §14.6），server 的薄客户端
  examples/                   # 示例 Tool/Flow YAML（须可被 E2E 直接消费）
```

**结构约束（解决"结构是否合理"）**
- 依赖方向单向：`cli/server → engine → {runtime, host, registry} → model`。上层可依赖下层，禁止反向。
- **接口化以便测试**：`Registry`、`Runtime`、`Host` 均为接口；测试用 fake/in-memory 实现替换，真实 SQLite / 进程仅在集成测试启用。
- 引擎（engine）不持有 IO 副作用细节：所有外部交互经 `Runtime` / `Host` 接口，使纯逻辑可单测、可复现。
- 包名即职责边界，禁止在 `model` 写校验逻辑（`model` 只建模）、在 `registry` 写调度逻辑。

---

## 2. 测试金字塔与策略（解决"测试覆盖率"）

| 层级 | 范围 | 工具/方式 | 门槛 |
|---|---|---|---|
| **单测（Unit）** | 纯函数：Schema 校验、版本状态机、依赖图环检测、SemVer 解析、变量解析、Hook weight 排序、fail-closed 判定 | `testing` + 表驱动（table-driven） | 100% 行覆盖目标于纯逻辑包 |
| **集成（Integration）** | Registry 落库往返、Runtime 适配（process 启 `cat` 喂 JSON 取末行；lua 沙箱；go-host 注册）、Host 7 原语（含 subscribe）、Hook 改写+重校验 | 真实组件 + 临时 SQLite（`t.TempDir`） | 关键路径全通 |
| **场景/E2E** | 整 Flow DSL 跑通（顺序/并行/条件）、Hook 拦截改写、版本晋升 API、app run 生命周期 | YAML 示例直接喂引擎，断言最终输出/状态 | 每个 PRD 场景 ≥1 用例 |
| **契约（Contract）** | CLI/REST 端点签名与 PRD §11 逐条对齐；Schema 同源性 | 端点 ↔ §11 表自动比对 | 零偏差 |
| **属性/Fuzz（进阶）** | 模板解析、YAML 鲁棒性、变量路径合法性 | `testing.F` fuzz | 关键解析器覆盖 |

**约定**
- 所有校验报错用 **golden 文件**（`testdata/*.golden`）比对，保证错误信息稳定、可评审。
- 测试命名：`Test<场景>_<预期>`（实际代码不写包名前缀），例 `TestWhenFalseSkipsStep`。
- 不可达分支（如 `default` 防御）用显式注释 + `t.Error("unreachable")` 兜底，避免"假覆盖"。

---

## 3. 需求 → 代码 可追溯矩阵（解决"情景覆盖是否到位"）

> 状态列：🔴 缺失（当前 S0 无）/ 🟡 部分（骨架有但未校验）/ 🟢 已覆盖（需测试支撑）。本矩阵是 PRD 的"测试账本"，每切片更新。

| PRD 条目 | 需求摘要 | 归属包 | 测试类型 | 测试 ID 示例 | 状态 |
|---|---|---|---|---|---|
| §2 | 5 原语模型正确 | model | 单测 | T-model-01 | 🟢 |
| §3.1 | Tool 定义/字段 | model | 单测 | T-model-02 | 🟢 |
| §3.2 | Runtime 类型枚举 | model | 单测 | T-model-03 | 🟢 |
| §3.2 | Runtime 适配器实现（process/lua/go-host 已落地，wasm deferred） | engine | 集成 | T-rt-* | 🟢 process + lua（S7a）+ **go-host（#76，2026-09-13：进程内 Go 函数注册表 `engine.RegisterHostFunc`，`runtimeFor` 分发，`host` 视图透传，service 不支持）；wasm 仍 deferred（V3，需 wazero + guest 工具链）**；未知/未实现 runtime 注册期即拒（REQ-TOOL-RUNTIME-VALID / `spec_consistency_test.go`） |
| §4.1 | input/output 类型/必填/默认校验 | engine | 单测+集成 | T-eng-01 | 🟢 已落地（单一真理源 Tool.input_schema/output_schema；REQ-SCHEMA-IN/OUT / `engine_test.go`: TestRunFlowRejectsInvalidInput/Output） |
| §4.2 | 版本层/参数层分离（Flow 默认 stable） | engine | 单测 | T-eng-02 | 🟢 已落地（ToolVersion 状态机 + Flow 默认 stable；REQ-USEVERSION / `model/version_test.go`） |
| §4.3 | depends_on / when / 变量引用 `${vars.x}` 等 | engine | 单测 | T-eng-03..05 | 🟢 |
| §4.4 | 触发方式收敛为执行入口 | server | 契约 | T-srv-01 | 🟢 已落地（REST 执行面 14/15 端点 + CLI；REQ-API-REST / `server_test.go` 端点契约） |
| §5.1 | Hook 四要素模型 | model | 单测 | T-model-04 | 🟢 |
| §5.2 | 生命周期 + weight 升序（同重注册序） | engine | 单测 | T-eng-06 | 🟢 已落地（Target 过滤 + Order 升序装配；REQ-HOOK-SELECT/ORDER / `hook_test.go`: TestHookTargetFilter/Order） |
| §5.3 | 插件无感知 / action 是 Tool 或 Flow / fail-closed / 禁止隐式依赖 | engine | 集成 | T-eng-07 | 🟢 已落地（action=已注册 Tool id 或 Flow id；#72 起 Flow 载体=同步内联子 Flow，同 execID、结构性无递归、递归守卫拦截循环；fail-closed；REQ-HOOK-FAILCLOSED/NORECUR/ACTION-FLOW）；Flow 级 stage（before_flow/on_success）#72 落地 |
| §5.4 | 改写 input/output + 重校验；best-effort；on_error 不恢复；Hook 嵌套防重入 | engine | 集成 | T-eng-08..11 | 🟢 已落地（改写后按 schema 重校验、best-effort 仅告警、on_error 不恢复、无递归；REQ-HOOK-REWRITE/REVALIDATE/ONERROR/NORECUR） |
| §6 | 宿主 7 原语 http/call/db/kv/secret/event/subscribe（另有纯函数原子 time/template/crypto，始终可用、不受门禁） | host | 集成 | T-host-01..07 | 🟢 M5 已落地（`internal/cap` + `engine.luaRuntime`/`processRuntime` 注入 `ctx.*`；#69 收口：db 原语实现 SQLite、引擎默认宿主装配 KV/HTTP/Event/DB + `call` 解析器 Tool/Flow + 循环守卫，声明即生产可用） |
| §6.2 | call 与 event 边界 | host | 集成 | T-host-07 | 🟢 M5：call 经 Resolver 唯一通道、event notify/wait 同步往返，均已测 |
| §6 / §0#14 | KV 生命周期维度（ScopeGlobal 进程级持久 / ScopeSession 绑定 flow 执行、跨 step 共享 + 引用计数回收） | host/engine | 单元+集成 | T-kv-01..05, E-kvl-01..02 | 🟢 M-KV-LIFECYCLE 已落地（`internal/cap` Scope/SessionRegistry/MemSessionRegistry/scopedKV；`engine.run` 对 flow.ID 做 Acquire/defer Release；`lua` ctx.kv 增可选 scope 参数；全部内存态、重启即失，显式满足 §0 第14条） |
| §7.1 | 状态机 draft→testing→stable→deprecated/rejected | engine/registry | 单测 | T-ver-01 | 🟢 已落地（VersionTransitions 白名单 + CanPromote；`model/version_test.go` / `engine/version_test.go`） |
| §7.2 | 晋升显式 + operator 审计 | server/registry | 集成 | T-ver-02 | 🟢 已落地（晋升经 CLI `gear tool promote` 与 REST `POST /api/tools/{id}/versions/{v}/promote` 均可触发，均走 `engine.PromoteTool` + `before_promote` 门禁 fail-closed；见 REQ-API-PROMOTE） |
| §7.3 | 幂等 + 并发安全（写锁/CAS/版本不可变） | registry | 并发测试 | T-ver-03 | 🟢 已落地（Registry 写锁 + upsert 事务 + `TestConcurrentUpsert`；版本行不可变） |
| §7.4 | 灰度选择（use_version:testing / allow_testing） | engine | 单测 | T-ver-04 | 🟢 已落地（resolveToolVersion 钉版本解析；REQ-USEVERSION / `engine/version_test.go`: TestRunFlowResolvesUseVersion） |
| §8.1 | 依赖图三类（Flow→Tool 不传递） | registry | 单测 | T-reg-01 | 🟢 已落地（`registry_test.go` 依赖图三类校验） |
| §8.2 | 注销拒绝 3 条件（被 Flow/Hook 引用/运行中） | registry | 集成 | T-reg-02 | 🟢 三条件全实现（运行中由 RunningChecker 判定；见 REQ-DEL-TOOL-GUARD / REQ-DEL-TOOL-RUNNING） |
| §8.3 | Lock + digest（signature 暂 deferred） | registry | 集成 | T-reg-03 | 🟡 Lock 已做（`sync.Mutex` + 事务）；digest/signature 仍 deferred |
| §9 | AI 生成 DSL + 静态校验 + ≤3 次重试 + Schema 不漂移 + agent 自我锚定 | server/ext+cli | 集成 | T-ai-01, E-ai-01..02 | 🟢 **已实现（作为 `ext.ai` 扩展，去内置化 + 三层分层 + 配置化加载）**：2026-09-14 落地 three-layer，同日第十四轮补 **`ext.registry` 进程内原语**（agent 经 `ctx.ext.registry.tools()/flows()` 自我锚定，不再由调用方喂 tools，LLM 规划才有真实依据）+ **配置化扩展加载**（CLI/server 经 `SetupExtensions` 读 `extensions.yaml` 装配 `ext.*`，取代写死的 `Register("ext.ai", ...)`，发布后加 `ext.tls` 等只改配置不改源码）。**LLM 层**（`ctx.ext.ai.complete`，参数化 provider/model/base_url/api_key/temperature/format）+ **Agent 层**（易变 Lua，`examples/ai-create-flow.yaml` 的 `ai-gen` 入口，改行为不重编 Go）+ **静态原语层**（Go 编译进 sidecar：`catalog`/`domains`/`validate`；另 `ext.registry` 进程内）；LLM 默认 SiliconFlow、回退 Ollama、`mock` 离线；M-EXT-AI-1/2/3 + M-EXT-AI-LAYER 全部 ✅ |
| §10 | 架构分层 | （结构审查） | 评审 | — | 🟡 |
| §11 | API 最小集全量 | server | 契约 | T-srv-02 | 🟢 14 端点全实现（原 15 个含 `POST /api/ai/infer`，已随 AI 功能移除；`GET /api/executions/{id}/logs` 已随 #71 实现；GET /api/flows 列表端点补测见 TestListFlows）；**无 deferred 端点** |
| §13 | 待确认项显式处置 | 全局 | 评审 | — | 🟡 |
| §14 | app run 进程模型 + 长驻/restart/子进程归属/进程池 | server/engine | 集成 | T-app-01..04 | 🟢 已落地（restart 策略 + 信号停止 + `--max` 闸门 + 子进程归属 + 健康探针；REQ-APP-* / `app/supervisor_test.go`）；**跨进程队列/进程池已落地（#73，2026-09-13：执行入口全局并发闸门 ConcurrencyGate，worker 池 = 并发上限、超额排队、再超额回 429 拒绝，区别于 S4 step 级 maxParallel 与 S2 app --max；REQ-CONCURRENCY-CROSS `implemented`）** |

---

## 4. 切片路线图与每切片 DoD（解决"分阶段、可评审"）

> 每切片 DoD = 该切片矩阵条目全 🟢 + 覆盖率达标 + 评审过关。
>
> **⚠️ 2026-09-13 更正**：下面原定顺序在实施中已被调整，实际执行如下（本表随切片回填，避免方法论漂移）：
> S0 骨架 ✅ / S1 最小引擎 ✅ / **S2 app run 进程监管 ✅** / **S3 Hook 切面 ✅** / **S4 并行+依赖推断+背压 ✅** / **S7a lua runtime ✅（提前）** / **`when` 条件执行 ✅** / **`ctx.*` 宿主能力层（M5）✅（http/call/kv/secret/event 实现 + 最小权限放行；db 原语已定义但宿主未提供实现，声明即可见、调用返回 ErrCapabilityUnavailable）** / **#69 ctx.* 宿主默认装配 ✅（2026-09-13：db 实现 SQLite + 引擎默认宿主 KV/HTTP/Event/DB + call 解析器 Tool/Flow + 循环守卫，声明即生产可用）**；**S5 版本灰度 ✅（2026-09-13）** / **S6c AI 闭环 ✅（2026-09-13）** / **#73 执行入口全局并发闸门 ✅（2026-09-13，ConcurrencyGate 背压闸门，REQ-CONCURRENCY-CROSS `implemented`）** / **#76 go-host runtime ✅（2026-09-13，进程内 Go 函数注册表 `engine.RegisterHostFunc` + `runtimeFor` 分发 + host 视图透传；wasm 仍 deferred V3）**。
> 调整理由：lua 提前是因为 `ctx.*` 尚未落地时 lua 可作为**纯计算沙箱**独立成立；Hook 排在并行之前是因为 Hook 需要先抽「共享 step 执行路径」，而并行也要复用同一条路径——先建路径再改它的并发语义，风险更低。

- **S0 基线**：model/registry/cli 骨架。DoD：修正 §9 的 6 处缺陷 + model 单测 + registry 集成测试 + CLI 与 §11 对齐清单。✅ **已完成 2026-09-12**。
- **S1 引擎**：DSL 解析、Schema 校验、顺序调度、变量解析、input/output 校验。✅ **已完成**。
- **S2 app run 进程模型**：长驻 Flow（`mode: service`）、restart、信号停止、`app run --max` 闸门、service 健康检查探针。✅ **已完成**（健康检查探针 #75 已落地；**跨进程执行入口队列/进程池已于 #73 落地（2026-09-13，ConcurrencyGate 背压闸门，REQ-CONCURRENCY-CROSS `implemented`）**）。
- **S3 Hook 切面**：before_step/after_step/on_error + Order 装配 + 改写重校验 + fail-closed + criticality + 无递归。✅ **已完成**（before_promote 于 S5 落地）。
- **S4 并行与背压**：分层并行、依赖推断、`maxParallel`、并行安全、并行下 fail-closed、崩溃指数退避。✅ **已完成**。
- **S7a lua runtime**：gopher-lua 沙箱（once）+ 沙箱/超时/错误测试。✅ **已完成**；**沙箱策略可配置化（REQ-LUA-SANDBOX-CONFIG）已落地**：引擎级 `WithSandbox(strict|permissive)`，strict 仅 base/string/table/math，permissive 额外开 coroutine，文件加载全局任何策略都禁（防逃逸）。
- **`when` 条件执行**：expr 表达式引擎 + 三态跳过 + 级联传播 + 注册期编译校验。✅ **已完成**（2026-09-13）。
- **S5 版本灰度** ✅（2026-09-13）：状态机 `VersionTransitions` + `CanPromote` + 版本级 Entry/Schema 覆盖 + `resolveToolVersion`（`use_version` 钉版本解析）+ `PromoteTool` 触发 `before_promote` 门禁（fail-closed）+ `tool promote`/`tool versions` CLI。覆盖 T-ver-01..04、T-reg-01..03。
- **S6 REST API + CLI 补全 + AI 闭环**：§11 全量端点 + CLI 补全 + AI 生成/校验/重试。覆盖 T-srv-01..02、T-ai-01。
  - **S6a REST 执行面 ✅（2026-09-13）**：`internal/server`（标准库路由，无框架）+ `ExecStore`（内存态执行记录）+ `AppStore`（受管进程）+ 12 个端点 + 契约测试；CLI `gear serve`。
  - **S6b CLI 补全** ✅（2026-09-13）：CLI 管理面单测全覆盖（成功 + 错误路径，含 §8.2 注销守卫），`cli` 包覆盖 **72.8%**；新增子命令 `exec start/status/list`（对齐 REST 执行面）、`tool promote`/`tool versions`（S5）、`ai gen`（S6c）均已落地。
  - **S6c AI 闭环（built-in）** ✅→**已撤回（2026-09-13）→ 已改 `ext.ai` 扩展（2026-09-14 ✅）**：用户决定 AI 不做进内核。原 built-in `internal/ai`（Inferrer / Ollama / SiliconFlow / GenerateFlow）、`server.inferrer` 字段、`POST /api/ai/infer` 端点已全部移除；AI 改以 **`ext.ai` 扩展原语（Sidecar）** 承载（参考 Flow + 薄 CLI 壳；结构化多阶段 agent，不自由 ReAct）。详见本文件 §11 第十一次/第十二次变更与 §9 矩阵行（状态 🟢 已实现）。

- **M-KV-LIFECYCLE（KV 生命周期维度）** ✅（2026-09-13）：`kv` 原语增 `Scope`（`global` 进程级持久 / `session` 绑定 flow 执行跨 step 共享）+ `SessionRegistry`（引用计数回收，run 结束归零即删命名空间）；`engine.run` 按 flow.ID 做 `Acquire`/`defer Release`，`hostForChain` 把 sessionID 设为 flow.ID；`lua` ctx.kv 增可选 scope 参数（缺省 global，向后兼容）；全部内存态、重启即失（显式满足 §0 第14条）。覆盖 T-kv-01..05（`cap_test.go`）、E-kvl-01..02（`kv_session_test.go`）。
- **M-EXT-AI（AI 去内置化 → ext.ai 扩展）** ✅（2026-09-14）：移除 built-in `internal/ai`（包已删）；新增 `examples/gear-ai-sidecar`（Go 实现，SiliconFlow 默认 + Ollama 兜底，key 走 sidecar 环境变量）承载**稳定原语**：`complete`（参数化 LLM，provider/model/base_url/api_key/temperature/format 任意模型、`mock` 离线）、`catalog`（BM25 域内检索）、`domains`（按 namespace 归类）、`validate`（复用 `ValidateFlowStatic`）；**多阶段 agent 编排（能力域概览 → LLM 拆解 → 域内检索 → 组装 Flow/Tool → 校验 → ≤N 次重来 → 人工确认/注册）于 2026-09-14 第十三次重构全部移入 Lua（`examples/ai-create-flow.yaml` 的 `ai-gen` 入口），改 agent 行为不重编 Go**；引擎侧 `ext.ai` 经 `SidecarExtension` 接线（`engine.AIExtension` + `ctx.ext.ai.complete/catalog/domains/validate`，未声明即 `ErrCapabilityDenied`）+ 静态校验；server/cli 默认装配 `ext.ai`（sidecar 二进制缺失时 fail-closed 返回 `ErrCapabilityUnavailable`）；参考 Flow `examples/ai-create-flow.yaml`（注入 `tool_id+desc+schema`）+ 薄 CLI 壳 `gear ai gen flow|tool [--register]`；PRD §9 翻 `implemented (as ext.ai)`、REQ-API-AI 翻 `ext`；`ext.ai` 单测（`TestExtAIReachable`/`TestExtAINotDeclaredDenied`）+ sidecar 内部测试（mock LLM，覆盖成功/重试/全失败/校验拒未知工具/域概览检索/normalize）。复用 M-KV-LIFECYCLE 的 session 作用域做 agent 中间态跨 step 缓存。

- **（注，2026-09-15 整体删除）**：`examples/gear-ai-sidecar` 示例与 `gear-ai-sidecar.exe` 已于本日整体删除；ext.ai 改为**进程内扩展**（无独立 sidecar 二进制，二进制缺失时 fail-closed 回 `ErrCapabilityUnavailable`）。本文件 M-EXT-AI / M-EXT-AI-LAYER / 第十六次真机验证等小节中出现的 `examples/gear-ai-sidecar/*` 与 `gear-ai-sidecar.exe` 均为当时记录，非现状；agent 编排现活在 Lua `examples/ai-create-flow.yaml` 的 `ai-gen` 入口。

---

## 5. 质量闸门（CI，解决"怎么保证没 bug"）

| 闸门 | 命令 / 工具 | 失败处置 |
|---|---|---|
| 编译 | `go build ./...` | 阻断合并 |
| 静态 | `go vet ./...` + `staticcheck ./...`（规则取舍见根目录 `staticcheck.conf`；当前零告警） | 阻断合并 |
| 格式化 | `gofmt -l` 零差异 | 阻断合并 |
| **闸门入口** | `bash scripts/gate.sh`（一次跑完①–⑥，退出码即结果，日志落盘） | 见 §5.0 |
| **覆盖率** | `go test -coverprofile`；**引擎/内核包 ≥80%，model/registry 关键路径 100%** | 低于门槛阻断合并；PR 须附报告 |
| 并发/竞态 | `go test -race` 于 registry/engine | 竞态即阻断 |
| 契约 | §11 端点 ↔ 矩阵自动比对 | 偏差阻断 |
| 评审 | 每切片覆盖率报告 + 矩阵更新，用户签批 | 未签批不进下一刀 |

> 覆盖率门槛默认可调；如你认为某包应更高/更低，在签批时改。

### 5.0 闸门怎么跑（别手工拼命令）

上表六项**全部**由 `bash scripts/gate.sh` 一条命令跑完（编译 / vet / gofmt / staticcheck / `-race -cover` 全量测试 / 覆盖率门槛），
失败项不中断后续、跑完统一汇总，退出码即闸门结果（0=PASS），日志落盘见 §5.1。

> 2026-09-20 之前 `gate.sh` **只跑 `go test`**，上表其余五项无人执行——
> 于是出现了「宪法写着 gofmt 零差异、staticcheck 必跑、覆盖率 ≥80%，实际三项全红却没人知道」的情况。
> 现已补齐；`staticcheck` 的规则取舍写在仓库根目录 `staticcheck.conf`（内附理由，只关了中文语境不适用的 ST1005）。

**当前闸门实况**（2026-09-20 补测后实测）：**①–⑥ 全绿**（`GATE RESULT: PASS`）。
其中 ⑥ 覆盖率：engine 83.8% / cap 90.7% / mcp 83.6% / cli 84.2% / server 85.8% / model 97.2% / registry 84.5%。
（补测前为 engine 61.6% / cap 63.0% / mcp 69.7% / cli 54.9% —— 成因是整树重建丢失了一批 `*_test.go`
与大函数分支无人走到；按上表，红即不可合并，故补齐后才算过闸。）

### 5.2 补覆盖率的两条做法（别靠「多写点测试」这种口号）

1. **按「未覆盖语句数」排序定位，而不是按文件/按百分比**。
   `go tool cover -func` 只给每个函数的百分比，看不出大函数的哪个分支没跑到。
   用 `python tools/cov_report.py <coverprofile> [topN]`：先按**未覆盖语句数**倒序列文件与函数，
   加 `lines` 参数还能直接打印未覆盖块的源码行。本轮就是这样定位到
   `ai_llm.go`（265 条全 0%）、`cap/secretstore.go`（56 条全 0%）、`mcp.ServeHTTP`（12 条全 0%）等单点。
2. **每条新测试都要做负向验证**（改坏实现 → 测试必须变红 → 还原 → 必须变绿）。
   只跑「绿灯」证明不了测试有效：断言写反、分支写错的测试同样显示 PASS。
   用 `python tools/negcheck.py <文件> <旧片段> <新片段> <包> [测试名...]` 一条命令完成，
   并强制校验文件已逐字节还原（防「改坏了没还原」）。本轮 12 条新测试全部经此验证。

> 环境坑（会伪装成代码回归）：Windows 的 Git-Bash / CI 沙箱里 PATH 常不含 `sh`，
> 而引擎 process runtime 的用例**真的会 exec `sh`** —— 缺了会整片假红
> （`exec: "sh": executable file not found in %PATH%`）。`gate.sh` 已按需补齐 PATH（只在目录存在时前置）。

### 5.1 测试日志落盘（避免「管道日志用完就扔」）

痛点：闸门输出走管道、退出即丢；复现失败只能重跑（全量串行闸门约 90s）。两处落盘：

1. **闸门全量日志**：一律用 `bash scripts/gate.sh` 跑闸门——`go test -race -cover -p 1 -v ./...` 经 `tee` 同时落盘 `build/testlogs/gate-<时间戳>.log`，并复制 `build/testlogs/latest.log`（最近一次）。失败复现直接打开 `latest.log`，**无需重跑**。查看：`bash scripts/gatelog.sh`（cat latest）/ `bash scripts/gatelog.sh list`（历史 20 份）。`build/testlogs/` 已进 `.gitignore`。
2. **子进程原始输出**：引擎 `runProcess` 在 `cmd.Run` 错误 / stdout 为空 / JSON 解析失败三类分支，把完整原始 `stdout`+`stderr` 落盘 `os.TempDir()/gear-proc/proc-<时间戳>.log`，并在返回错误中附 `[原始输出已落盘: <路径>]`。复现「解析 stdout 失败」这类问题直接看该文件，不必重跑进程。

> 仅失败分支产生文件；`service` 模式子进程输出仍按红线 16 导向 `os.DevNull`（fd 不继承调用方管道），不在此列。

---

## 6. 评审节奏（节奏规则）

```
切片启动 → 给「测试清单 + DoD」给用户审
        → 红绿实现 → 本地跑通 -race + 覆盖率
        → 提交覆盖率报告 + 矩阵状态更新
        → 【用户评审过关】→ 下一刀
        → 不过关 → 回到实现，不累积
```

禁止：跨切片累积未测代码、静默默认待确认项、用"手动 smoke"替代断言测试。

---

## 7. PRD §13 待确认项 处置（显式定/搁置）

| # | 问题 | 本次处置 | 归属切片 | 理由 |
|---|---|---|---|---|
| 1 | 同 tool_id 多主版本共存 | **搁置**（默认禁止） | — | PRD 已定默认禁止，side-by-side 显式允许留后期 |
| 2 | 0.x 版本精确 pin | **定**：MVP 强制精确 pin | S5 | 保证可复现，不许 latest |
| 3 | 系统包管理 | **定**：只声明前提并验证，不自动安装 | S3 | 安全责任边界 |
| 4 | 灰度粒度 | **定**：调用方级显式选择 | S5 | PRD §7.4 已定 |
| 5 | 晋升审批前置 | **定**：默认调用即晋升；before_promote 可扩展 | S5/S4 | PRD §7.2/5.2 已定 |
| 6 | 测试版审计 | **定**：同表记录仅标记 stage | S5 | PRD §13-6 已定 |
| 7 | CAS 严格预期版本 | **定**：宽松策略（§7.3） | S5 | PRD 已定 |
| 8 | Web UI 首批 | **定**：否，UI 为可选 Flow | — | PRD §1.4 已定 |
| 9 | 长驻/阻塞 step 形态 | **定**：`daemon: true` 标记 | S2 | 采用 server Tool 思路 |
| 10 | app run restart/健康检查 | **定**：`--restart=always` + 可选健康检查 | S2 | PRD §14.3 |
| 11 | 执行入口并发上限 | **定**：内核进程池 + 上限（默认 64，可调） | S2 | PRD §14.5 |
| 12 | CLI 鉴权到 daemon | **搁置**（deferred） | — | 安全类，与 §13-14 同批 |
| 13 | Hook 默认 criticality | **定**：认证/脱敏=**critical**，日志类=**best-effort** | S4 | PRD §5.4 建议 |
| 14 | process/go-host 绕过 capability、secret 经 argv | **搁置**（deferred） | — | 用户明确暂不想，P0-4 |

> 任何"定"的项若实现中发现 PRD 自相矛盾，回到本文件 §7 修订并通知用户，不私自改。

---

## 8. 命名与语法一致性（从 S0 缺陷吸取）

- 变量引用统一 **`${vars.x}` / `${step.output.x}` / `${secrets.x}` / `${input.x}`**（PRD §4.3）；示例 YAML 须同步，禁止 `{{...}}`。
- 版本号强制 **SemVer**（`1.0.0`），禁止 `v1` 这类非标；`model.ToolVersion.Version` 用 `semver.Version` 类型并在 register 校验。
- Step 字段名对齐 PRD：`call`（非 `tool`）、`depends_on`、`when`、`use_version`。
- Hook `stage` 取值对齐 §5.2：`before_flow/before_step/after_step/on_error/on_success/before_promote`。

---

## 9. 当前 S0 已发现的 6 处缺陷（重做基线时修复）

| # | 缺陷 | 证据 | 修复动作 | S0 状态 | 实际测试 ID |
|---|---|---|---|---|---|
| D1 | 模板语法分叉 | `examples/hello_flow.yaml` 用 `{{vars.topic}}`，PRD 规定 `${vars.x}` | 示例改 `${vars.topic}`；新增回归测试锁死 | ✅ 已修复 | `TestExampleFlowUsesDollarBrace`（T-s0-example） |
| D2 | 版本 SemVer 未校验 | `hello_tool.yaml` 写 `version: v1`；`Version` 为裸 string 无校验 | 新增 `model.ParseSemVer` + `ToolVersion.Validate`（零依赖正则）+ `UpsertTool` 接入校验；示例改 `1.0.0` | ✅ 已修复 | `TestParseSemVer`、`TestToolVersionStateValid`、`TestToolValidate`（T-ver-parse / T-state-valid） |
| D3 | 零测试 | 全工程无 `_test.go` | 补 `internal/model/model_test.go` + `internal/registry/registry_test.go` | ✅ 已修复 | 见 §9.1 |
| D4 | CLI 与 §11 不对齐 | 缺 unregister/promote/validate/ai-gen/hook CRUD/app ls-stop/exec/secret | 仅列缺口清单（见 §9.2）；完整补全归 S6 | ⏭ 归 S6 | — |
| D5 | Schema 校验缺失 | register 仅 `yaml.Unmarshal`+upsert，不校验 schema/state/依赖 | `UpsertTool` 接入 `Tool.Validate`：version 合法 SemVer + state 枚举 + input/output schema 存在且 `type:object`（完整 JSON Schema 深校验留 S1） | 🟡 部分（最小真实校验已做） | `TestToolValidate`、`TestRegisterRejectsInvalid`（T-reg-02） |
| D6 | 并发/事务空白 | `sql.Open` 无事务；§7.3 写锁/CAS 未体现 | `Registry` 加 `sync.Mutex` 串行化写 + upsert 包事务（`BEGIN/COMMIT`）；`-race` 并发用例 | 🟡 部分（写锁+事务已做；CAS/版本不可变留 S5） | `TestConcurrentUpsert`（T-reg-03） |

### 9.1 S0 覆盖率报告（2026-09-12，`CGO_ENABLED=1 go test -race -cover ./...`）

| 包 | 覆盖率 | 目标 | 结论 |
|---|---|---|---|
| `internal/model` | **100.0%** | 关键路径 100% | ✅ 达标 |
| `internal/registry` | **82.8%** | ≥80% | ✅ 达标 |
| `internal/cli` / `cmd/gear` | cli **76.5%**（S6b）/ cmd 0.0% | cli 管理面命令全覆盖；cmd 仅 `main()` 入口 | ✅ cli 达标（S6b）；cmd ⏭ 收尾 |

- `go build ./...` ✅ · `go vet ./...` ✅ · `go test -race` ✅（无竞态）
- registry 未覆盖的 ~17% 全部是 DB 错误防御分支（`Open`/`migrate`/`upsert`/`list`/`get` 的错误返回），属难触发且非核心路径，留待注入式故障测试（进阶）。
- model 100%：含 SemVer 解析、状态枚举、Tool/Hook/YAML 往返、Validate 全分支。

### 9.2 S0 已交付测试清单（对应 §6 节奏）

**model（单测，100%）**
- `T-model-01` YAML↔struct 往返（Tool/Flow/Hook/Step）
- `T-model-02` Runtime 枚举取值
- `T-model-03` ToolVersion 字段
- `T-model-04` Hook 四要素 + criticality
- `T-ver-parse` SemVer 合法/非法
- `T-state-valid` 5 状态枚举
- `T-model-05` Tool.Validate 全分支（非法 version/state/缺 schema/schema 类型错/空 id/无 version）

**registry（集成，82.8%）**
- `T-reg-01` Tool 落库往返 + blob 完整
- `T-reg-02` register 拒绝非法 version/state/缺 schema
- `T-reg-03` 同 id 并发 upsert 不损坏（`-race`）
- `T-reg-04` Flow 落库往返
- `T-reg-05` Hook 落库往返
- `T-reg-06` 不存在 id 返回 (nil,nil)
- `T-s0-example` 示例 YAML 用 `${vars.topic}` 而非 `{{...}}`（锁死 D1 回归）


### 9.3 S6b 覆盖率报告（2026-09-13，`CGO_ENABLED=1 go test -race -cover ./...`）

| 包 | 覆盖率 | 目标 | 结论 |
|---|---|---|---|
| `internal/model` | **98.6%** | 关键路径 100% | ✅ 达标 |
| `internal/engine` | **87.7%** | ≥80% | ✅ 达标 |
| `internal/registry` | **83.4%** | ≥80% | ✅ 达标 |
| `internal/app` | **86.0%** | ≥80% | ✅ 达标 |
| `internal/server` | **83.9%** | ≥80% | ✅ 达标 |
| `internal/cli` | **76.5%** | 管理面命令全覆盖 | ✅ S6b 达标（本轮新增 cli_test.go） |
| `cmd/gear` | 0.0% | 仅入口 | ⏭ 收尾 |

- `go build ./...` ✅ · `go vet ./...` ✅ · `go test -race` ✅（无竞态）。
- cli 本轮从 0% → 75.2%：覆盖 tool/flow/hook 全生命周期命令与 app 非法 `--restart` 错误分支；serve 执行路径（阻塞 HTTP）与 flow run 成功路径（拉起进程）因需集成环境未覆盖，归 S6b 收尾。

---

## 11. 方法论变更日志（边做边改；每条都写清「为什么」）

> 本文件不是一次成型的蓝图，而是**随切片演进**的宪法。每次改动必须回答三件事：触发场景 / 改了什么 / 为什么必须改。
> 规则：新规则必须来自**真实踩到的坑**，不接受纸上推演出的规则（避免方法论自我膨胀）。

### 2026-09-13（第四次）· 实现 M5：`ctx.*` 宿主能力层

**M5 的设计原则（来自 PRD §6，已落进 `internal/cap`）**
- **规则**：宿主只向 Tool 暴露 7 个跨边界原语（http/call/db/kv/secret/event/subscribe；另有 time/template/crypto 三个**纯函数原子**，始终可用、无需声明——见 ATOM_ROADMAP §0）；**capability 精确放行**——未声明的调用一律拒绝，权限遵循最小权限；secret 只读；原语只提供「通道」不提供「业务」（发邮件/接钉钉都是普通 Tool，内部用 http 实现）。
- **为什么**：这是之前反复强调的「枚举式声明必须逐项登记（红线 15）」+「注册期拒绝未知值」在能力面上的延伸。能力枚举若两处维护必漂移，故 `cap.KnownPrimitives` 是**单一来源**，`cap.GrantsFromStrings` 在解析点拒绝未知值（与 `model.Runtime` / `model.HookStage` 同一套路）。
- **落地**：
  - `internal/cap`：Primitive 枚举单一来源；`Context` 每个访问器先查 `Grants` 再取实现——未授权 `ErrCapabilityDenied`，已授权但宿主未提供 `ErrCapabilityUnavailable`（把「文档说有、代码无」变成可见错误）。
  - `Runtime` 接口加 `host *cap.Context` 参数；引擎 `Engine.hostFor(tool)` 按 `tool.Capabilities` 逐次执行构建视图并沿 `runOnceStep`/`startServiceStep`/`applyHooks`/`Relaunch` 透传。
  - **lua**：`setupLuaCapabilities` 仅注入**已声明**的原语（未声明的连字段都不出现在 `ctx` 上——比「存在但报错」更严格的防御纵深），闭包捕获授权后的实现，脚本无法绕过沙箱。
  - **process**：`withHostCapabilities` 把 `ctx.capabilities` / `ctx.sandbox` 注入 step input 的 JSON，进程侧可做最小权限自检（进程内实时强制留待后续切片）。
  - **db**：原语已实现（`internal/cap/sqldb.go`，modernc.org/sqlite，文件或内存 DSN）；引擎 `New` 未显式 `WithHost` 时注入默认宿主（KV/HTTP/Event/DB），声明 `db` 的 Tool 在 `gear serve` 下即可读写 SQLite，不再返回 `ErrCapabilityUnavailable`（#69 收口）。

**M5 顺手修的真实坑（值得记一笔）**
- `cap.Context.HTTP()` 原先在 `Providers` 为 nil 时直接 `c.prov.HTTP` → **nil 解引用 panic**。其他访问器都有 `c.prov == nil` 守卫，唯独 HTTP 漏了。修复为：无宿主即 `ErrCapabilityUnavailable`（绝不静默回退到 `http.DefaultClient` 去访问公网——最小权限的「安全默认」）。新增 `TestHTTPUnavailableWhenNoProvider` 锁死。

**覆盖率闸门实证（再次验证 F-覆盖率闸门）**
- M5 把 `cap` 包从「无」补到 `86.7%`、engine 维持 `86.8%`，全量 `-race -cover` 闸门在 `-p 1`（串行）下绿灯；kernel 包（engine/model/registry/server）均 ≥80%。
- 一个环境性观察（**已纠错，见第五次**）：`internal/app` 的 Supervisor 测试会拉起长驻 `sh -c` 服务子进程，在 `-race` 全量闸门下出现 `Test I/O incomplete`。最初误判为「并行争用 stdout 管道的环境噪声、串行 `-p 1` 即通过」。实则根因是 **fd 继承**：`startProcess` 让 service 子进程 `cmd.Stdout/Stderr = os.Stdout`，`sh -c` 的孙进程（如 `sleep`）在父 `sh` 被 `Kill()` 后成为孤儿并继续持有继承来的管道 fd，导致 go test 管道在二进制退出后 60s（`WaitDelay`）仍被占用。修复见第五次变更日志（红线 16）。

### 2026-09-13（第五次）· 修复 service 子进程 fd 继承导致 `Test I/O incomplete`

**根因（真·不是环境噪声）**
- `internal/engine/process.go` 的 `startProcess`（service 模式，`sh -c <entry>`）原把 `cmd.Stdout = os.Stdout; cmd.Stderr = os.Stderr`。其孙进程（如 `sleep`）在父 `sh` 被 `Process.Kill()` 后**不会被一起杀掉**（Windows 下 `TerminateProcess` 不级联子进程；msys 的 `sh` 也不转发信号），成为孤儿并继续持有调用方的 stdout/stderr fd。
- 当该调用方是 `go test` 时，孤儿进程长期占用测试管道 → 二进制退出 60s 后 go test 仍报 `Test I/O incomplete`。该问题只在 `-race` 全量闸门（进程类测试密集、调度变慢、取消窗口被拉宽）下暴露，单独跑 `app` 包甚至 `-race` 单独跑都「碰巧」干净，故极具迷惑性。

**修复（红线 16：service 子进程不得继承调用方 stdout/stderr）**
- `process.go` 新增包级 `serviceOut`（init 时 `os.OpenFile(os.DevNull, O_WRONLY, 0)`），`startProcess` 的 `cmd.Stdout/Stderr` 统一导向 `serviceOut`。孤儿孙进程即便残留，持有的是 `/dev/null` 而非测试管道 → 泄漏消除。
- 结构化 service 日志（按 service 落盘 / 上报 Supervisor）留待后续日志切片；本处只保证生命周期与 fd 正确性。
- 配套收敛测试用长驻命令：`server_test.go` `Entry: "sleep 30"` → `"sleep 3"`、`supervisor_test.go` 两处 `sleep 30` → `sleep 3`（此前已改），统一缩短孤儿窗口。

**结果**
- 全量 `go test -race -cover -p 1 ./...` **全绿**，无 `Test I/O incomplete`、无 FAIL；覆盖率 engine **86.7%** / app 86.0% / cap 86.7% / model 98.6% / registry 83.4% / server 83.9%。
- 教训：进程树级联 kill 在 Windows 上不可靠（无 POSIX 进程组），**与其依赖「杀得干净」，不如从根上不把敏感 fd 交给子进程**——输出重定向比进程树回收更稳、更跨平台。
- 仍 deferred（非本次范围）：`Kill()` 只杀 `sh` 不杀孙进程会导致 **crash-loop 下孤儿进程短暂累积**（每个 ≤其 sleep 时长，自然消亡）；若未来要完全杜绝，需引入 job object / `taskkill /T` 级联回收，单独切片处理。

### 2026-09-13（第六次）· 落地 S5 版本灰度 + #57 exec CLI/根 --addr + #58 S6c AI 闭环

**S5 版本灰度（状态机 + use_version + promote + before_promote 门禁）**
- 触发：用户「三个都做」授权把 S5 / Fork3 / S6c 一并收口。
- 改了什么：
  - `model`：`ToolVersion` 增版本级 `Entry`/`InputSchema`/`OutputSchema`；`KnownHookStages` 纳入 `before_promote`。
  - `validate`：`VersionTransitions` 状态机 + `CanPromote`；放开 `use_version` 与 `before_promote` 的注册期拒绝。
  - `registry`：`PromoteToolVersion` 按状态机落库（testing/stable/deprecated 写 `CreatedAt`）。
  - `engine`：`resolveToolVersion` 钉版本解析；`execLevel` 走解析后 Tool；`ValidateFlowStatic` 校验 `use_version` 存在；`PromoteTool` 触发 `before_promote` 门禁（fail-closed）。
  - `cli`：`tool promote <id> <v> <target>` / `tool versions <id>`。
- 为什么：版本灰度是「可复现 + 可回滚」的硬需求（PRD §7）；`before_promote` 把晋升审批前置成可扩展 Hook 门禁，而非硬编码流程。

**#57 REST 执行面 CLI（exec 组）+ 根 --addr 持久标志 + 执行列表路由**
- 根命令加持久 `--addr`（默认 `127.0.0.1:8787`，可用 `GEAR_ADDR` 覆盖）；新增 `exec start/status/list`（无状态 HTTP 客户端，走 REST `/api/executions`）。
- `server` 补 `GET /api/executions` 列表路由（此前只有 202 触发 + 单条 GET）。
- `cli` 输出统一改走 `cmd` 缓冲（不再用 `fmt.Println`），便于测试断言与管道消费。

**#58 S6c AI 闭环（自然语言 → Flow）**
- 新包 `internal/ai`：`Inferrer` 接口 + Ollama HTTP 客户端（默认 `localhost:11434` / `qwen3.5:4b`，可配置）+ `GenerateFlow`（去 yaml 围栏 → `yaml.Unmarshal` → `Flow.Validate` → 重试≤3）。
- `server` 新增 `POST /api/ai/infer`（推断器可注入，收敛失败返回 422）；`Server` 增 `inferrer` 字段与 `NewWithInferrer`。
- `cli` 新增 `ai gen <prompt>`（可选 `--register` 落地本地 registry）。
- 为什么：LLM 接入方式默认线上 SiliconFlow（用户本地无 Ollama，线上才是能跑通的路径）——`GenerateFlow` 只负责「把 LLM 原始文本收敛成合法 Flow」，后端可插拔，换 LLM 是纯配置（设 `SILICONFLOW_API_KEY` 即走线上、否则回退本地 Ollama）。

**关键教训（本会话）**
- 全量闸门 `go test -race -cover -p 1 ./...` 在 Bash 工具 2 分钟默认超时下**被掐断**，表现为「exit 1、日志全 `ok`、`server` 包整包缺失」——`server` 在依赖序最末、串行跑完 engine(31s)+其他后超时。修复：用 `run_in_background` 或显式 `timeout=600000`。**闸门超时 ≠ 测试失败**，须看完整日志。
- 重磅坑：`engine/version_test.go` 的 process entry 原写 `sh -c 'echo {"v":"2"}'`，Git Bash 的 `sh` 把双引号**词法去引号**成 `{v:2}`（非法 JSON）→ 运行期 `解析 stdout 失败`。修复：entry 改为 `echo '{"v":"2"}'`（**外层单引号、JSON 内双引号**）。凡用 `sh -c` 让进程吐 JSON，此约定是铁律（红线清单待补第 17 条）。

**全量闸门结果**：engine 86.2% / model 98.7% / registry 84.8% / server 85.1% / ai 85.5% / cap 86.7% / app 86.0% / cli 72.8%（cli 为管理面客户端非内核，exec/ai 均配集成测试）。

### 2026-09-13（第七次）· 测试日志落盘（闸门脚本 + 引擎失败输出落盘）

**触发**：用户痛点——测试输出走管道、退出即丢；复现失败只能重跑全量串行闸门（约 90s），浪费时间。

**改了什么**
- 新增 `scripts/gate.sh`：封装 `go test -race -cover -p 1 -v ./...`，经 `tee` 同时落盘 `build/testlogs/gate-<时间戳>.log` 并复制 `build/testlogs/latest.log`（最近一次）；保留最近 20 份防膨胀；退出码即闸门结果。`-v` 保证 `t.Log` 也进日志。
- 新增 `scripts/gatelog.sh`：`cat latest` / `list` 历史 20 份。
- `internal/engine/proclog.go` + `process.go`：`runProcess` 在 `cmd.Run` 错误 / stdout 为空 / JSON 解析失败三类分支，把完整原始 `stdout`+`stderr` 落盘 `os.TempDir()/gear-proc/proc-<时间戳>.log`，并在错误中附 `[原始输出已落盘: <路径>]`。仅失败分支触发，正常路径零文件。
- `.gitignore` 增加 `/build/testlogs/`。

**为什么**
- 闸门/子进程输出是「用完就扔」的管道日志；失败时把原始证据留在磁盘，复现问题直接看文件，省掉重跑。
- 与红线 16 不冲突：落盘的是一次性 `runProcess` 已捕获进内存 buffer 的字节（无 fd 继承问题）；`service` 模式子进程仍按红线 16 导向 `os.DevNull`，不在落盘范围内。
- 注意：`procFail` 用 `fmt.Sprintf` 承载底层错误，`%w` 不适用（只有 `fmt.Errorf` 支持 `%w`）——此处只做字符串拼接，无需错误链。

**结果**：单测验证 `gear-proc/` 下生成带 entry/stdout/stderr 的临时文件；全量闸门经 `gate.sh` 跑通并落盘（见 `build/testlogs/latest.log`）。

### 2026-09-13（第八次）· 功能完成度审计 + 补 `GET /api/flows` 测试 + 翻新 §11 测试账本

**触发**：用户问「所有功能都完成了吗？还有哪些未实现/未测试」。

**改了什么**
- 补 `internal/server/server_test.go: TestListFlows`：打 `GET /api/flows`（先 POST 建一条），断言 200 + 返回含该 flow；消除 `handleListFlows`（server.go:224）**0% 覆盖**的真缺口。未覆盖的仅 DB 报错→500 与 `flows==nil` 归一化两个防御分支（单测难触发，主路径已测）。
- 翻新 §11 测试账本：把已实际落地+已测、却仍标 🔴 的过期行翻 🟢（§3.3 / §4.1 / §4.2 / §4.4 / §5.2 / §5.3 / §5.4 / §7.1 / §7.2 / §7.3 / §7.4 / §8.1 / §9 / §14），每条附注 REQ-ID 与测试位置；§8.3 标 🟡（Lock 已做、digest/signature deferred）。刷新后矩阵**零 🔴**。

**为什么**
- 审计用「代码 + 文档 + 覆盖率逐函数」三查，而非凭记忆：真缺口只有 `GET /api/flows`（已实现挂路由却从没被测）；0% 函数全是入口/OS/胶水（main/Execute/ListenAndServe/ContextWithSignals/configureProcessTree/Relaunch/IsService），符合项目覆盖策略；cli/server 的 <80% 是错误分支未全。
- **文档漂移比代码缺口更隐蔽**：方法论 §11 大量 🔴 实为已实现，会让人误判「未测试」。矩阵必须随切片回查——这正是 §0 第 12 条「方法论自身可追溯」的本意。

**未实现（deferred，REQUIREMENTS 权威，本次未动）**：go-host/wasm runtime；lua service + 沙箱策略可配置；非 process runtime 的 service step；service 健康探针；跨进程队列/进程池；`${secrets.x}`；`GET /api/executions/{id}/logs`；执行记录持久化（有意内存态）；Hook action 指向 Flow；Hook stage `before_flow`/`on_success` + target 通配；ctx.* 宿主 db/kv/secret/event/call（仅声明，返回 ErrCapabilityUnavailable）；promote REST 端点（CLI 已做）。见 REQUIREMENTS.md 与各 REQ-ID。

### 2026-09-13（第九次）· AI 闭环改走线上 SiliconFlow（默认），强化生成 prompt

**触发**：用户发现 `ai` 生成 Flow 太简单、且默认走本地 Ollama——但本机没装 Ollama，质疑「测试怎么过的」。

**澄清（测试为何能过）**：`ai_test.go` 从不真连 Ollama——`TestOllamaInfer` 用 `httptest` 在内存起假 Ollama；其余 `GenerateFlow` 测试全用 `fakeInferrer`/`mockInferrer` Mock。闸门绿与「本机是否装 Ollama」无关（本就是好的测试隔离），但也因此掩盖了「默认路径实际没跑通」。

**改了什么**
- `internal/ai/ai.go` 新增 `SiliconFlowClient`（OpenAI 兼容 `/v1/chat/completions`）：默认 `deepseek-ai/DeepSeek-V4-Flash`、base `https://api.siliconflow.cn/v1`、Bearer 鉴权；API Key 取环境变量 `SILICONFLOW_API_KEY`。新增 `DefaultInferrer()`：设了 Key 走线上 SiliconFlow，否则回退本地 Ollama。
- `server.New` 改用 `ai.DefaultInferrer()`（此前硬编码 `ai.NewOllama("", "")`），故 `gear serve` 启动即按环境选推断器。
- 强化系统提示 `systemPromptText`：约束输出为合法/完整/可注册 Flow——必须有 id+≥1 step、call 用 kebab-case、用 `depends_on` 表达先后、`parallel: true` 标可并行、`step.input` 给具体参数值、除非单步否则给 2-4 步体现真实编排。Ollama 路径也前置该提示。
- 兼容 DeepSeek 可能的 `<think>…</think>` 思考块：`stripThink` 先剥离再收敛。
- 测试：`TestSiliconFlowInfer`（httptest 模拟，不联网不依赖 Key，验证请求形态+解析）；`TestSiliconFlowInferLive`（env 守卫，仅设了 Key 才真连，CI 默认跳过）。

**为什么**
- 用户本地无 Ollama，线上 AI 才是真能跑的通路；把默认从「本地优先」翻成「线上优先、本地兜底」，避免默认路径永远沉默失败。
- 模型只保证「结构合法」，不保证「够用」——靠系统提示把"单步空 input"的退化输出抬到"多步带参数"的真实编排。

**验证（真实联网）**：`SILICONFLOW_API_KEY=… go test ./internal/ai/ -run TestSiliconFlowInferLive` 实测返回 3 步 Flow（`fetch-unshipped-orders → generate-csv → send-email`，含 `depends_on` 与具体 `input`），`parseAndValidateFlow` 一次收敛成功——"太简单"问题已解决。

**注意（key 不入库）**：API Key 仅经环境变量传入，绝不写入源码/配置；本机运行前需 `export SILICONFLOW_API_KEY=sk-…`（或 Windows `set`），再 `gear serve`。

### 2026-09-13（第十次）· 补 REST promote 端点（REQ-API-PROMOTE 收口）

**触发**：待完成清单 #78（deferred 项）挑最轻量的先做。此前晋升仅 CLI `gear tool promote` 可触发，PRD §11 与 REQ-API-PROMOTE 都把它标成「非 REST 端点」。

**改了什么**
- `internal/server/server.go` 新增路由 `POST /api/tools/{id}/versions/{v}/promote` + handler `handlePromoteVersion`：解析 `{target}`，校验 `model.ValidStates`，走 `engine.PromoteTool`（before_promote 门禁 fail-closed + `registry.PromoteToolVersion` 状态校验）。错误经 `statusForErr` 映射：Tool/版本不存在→404，非法 target/迁移→400。
- `internal/server/server_test.go` 新增 `TestPromoteVersion`：合法晋升 draft→testing（断言落库）、非法 target→400、不存在版本→404、不存在 Tool→404。
- REQ-API-PROMOTE：`partial` → `implemented (S5→REST)`；PRD §11 端点表 promote 行、§7.3 实现状态块同步标 ✅；方法论 §7.2（T-ver-02）/ §11 矩阵收口（REST promote 端点已实现，仅 `executions/{id}/logs` 仍 deferred）。

**为什么**：晋升是一个「受 Hook 守卫的边界动作」，REST 暴露后运维可直接经 HTTP 灰度，不必依赖 CLI；逻辑复用既有的 `engine.PromoteTool`，无新分支风险。

**验证**：`TestPromoteVersion` 通过；全量 `gate.sh` 仍绿（server 包覆盖率见闸门输出）。

### 2026-09-13 · 实现 `when` 条件执行，新增 F6–F8 与 §0 第 10–12 条

**F6 · 控制流三态（skip / fail / empty）** → 对应 §0 第 7 条
- **规则**：step 结局必须能区分「执行成功 / 失败 / 被跳过」；`skip` 沿**有效依赖**（含推断边）级联传播；`skip` 不计入失败，也不得让下游读到 nil 或误报「尚未执行」。
- **为什么**：`when` 一引入，「这个 step 没跑」就成了第三种结局，而在此之前系统只有「成功 / 失败」二态，代码默认「没有输出 = 出错」。如果不把 skip 建模成一等状态，下游要么**静默拿到 nil 继续跑**（最危险，符合我们一直在消除的 RC-D 反模式），要么报出与真实原因无关的「step 尚未执行」（把排障引向错误方向）。
- **落地**：`ErrStepSkipped` 哨兵 + `RunResult.Skipped`（step → 原因）+ 依赖级联跳过 + 6 条断言测试。

**F7 · 表达式 ≠ 模板** → 对应 §0 第 8 条
- **规则**：`${}` 模板只做数据替换；条件/比较一律走带类型的表达式引擎；两种语法**不许混用**（`when` 里出现 `${` 直接拒绝并教正确写法）。
- **为什么**：插值再 eval 会**丢类型**。`steps.x.output.status` 实际是字符串 `"200"`，插值成字符串后 `!= 200` 这类比较会在「字符串世界」里恒真/恒假。这种 bug 的特征是「看起来很对」——测试如果只测 happy path 就永远发现不了。
- **落地**：引入 `expr-lang/expr`（`Knetic/govaluate` 在语料库代理上 404，排除）；`whenEnv` 用 expr tag 固定根名 `vars`/`steps`，写错根名在**编译期**就报错。专门的类型保持测试：字符串 `"200"` 与数字 `200` 比较必须为假。

**F8 · 引用解析与语法一致** → 对应 §0 第 9 条
- **规则**：每种引用语法都必须进依赖推断；解析必须遵守该语法的**字面量规则**（模板用 `\$\{steps\.`；表达式用裸 `steps\.`，且**先剔除字符串字面量**）。
- **为什么**：这条是本次**连续两次翻车**换来的，不是推演出来的：
  1. 只扫 `input` 不扫 `when` ⇒ when 里引用上游的 step 与上游**落在同一层并发执行**，求值时上游还没产出（报 `cannot fetch n from <nil>`）。→ **新语法引入的引用，必须同步进依赖推断**。
  2. 修 (1) 时直接复用了模板正则 ⇒ 匹配不到（表达式里没有 `${}`）；若简单换成裸正则，又会把 `when: 'name == "steps.nope"'` 里的**字符串字面量**当成依赖，制造假报错。→ **解析器必须理解该语法的字面量边界**。
- **落地**：`whenStepRefRe`（带前导断言排除 `mysteps.x`）+ `stripQuoted`（剔除 `"…"`/`'…'`/`` `…` ``）+ 3 条回归测试（when 产生依赖 / when 未知引用报错 / 字面量不误判）。

**§0 第 10 条（F3 延伸）· 静态校验前移到注册期**
- **为什么**：`when` 的语法错误若留到运行期，可能是在某个 Flow 恰好走到该分支时才炸；而编译一次表达式的成本几乎为零。与 runtime/mode/hook 的注册期校验保持同一标准，才不会出现「有的字段注册期管、有的运行期管」这种不一致。
- **落地**：`engine.CompileWhen` + `flow register` 前置校验（实测 `when: "1 +"` 注册即拒，并带 caret 定位）。

**§0 第 11 条（F1/F4 延伸）· 新状态必须可观测**
- **为什么**：若 `skip` 只打一行日志，测试就无法断言「它确实没跑」，排障时也看不到「为什么没跑」。可观测性是**可测试性的前提**——看不见的状态等于不存在。

**§0 第 12 条 · 方法论自身可追溯**
- **为什么**：本次顺手发现 §4 切片路线图已与真实执行顺序漂移（文档写 S3=runtime/ctx、S4=Hook；实际是 S3=Hook、S4=并行/背压、S7a=lua）。**方法论漂移就是方法论失效的开始**——它和 PRD 漂移是同一种病，得用同一味药：变更日志 + 定期回查（已修 §4）。

### 2026-09-13（第二次）· 做 S6a（REST 执行面），新增 §0 第 13–14 条与三条接口层规则

**S6a 的范围切分决定（本身也是方法论实践）**
- **触发**：S6 在路线图里写作「REST API + CLI 补全 + AI 闭环」，对应 PRD §11 的 15 个端点 + §9 的生成闭环。一次性做完会违反 §0 第 5 条（绝不一把梭）。
- **改法**：拆成 S6a（REST 执行面）/ S6b（CLI 补全）/ S6c（AI 闭环，且**需先定 LLM 接入方式**——属于决策而非编码量），本次只交付 S6a。
- **为什么**：切片的价值在于「每片独立可验证 + 可评审」。混在一起做，出问题时无法定位是哪一层的问题；而 AI 闭环还卡在一个未决策点上，把它捆进来只会拖慢可验证的部分。

**F9 · 接口层的「表示与实现」必须逐条对账**
- **规则**：PRD §11 这类端点表是**契约**，不是文档。实现时必须在需求矩阵里对**每一条**标注 `implemented` / `deferred`；未实现的不许沉默。
- **为什么**：接口层最容易漂移——端点表看起来像说明文档，实际是别人对接的依据。S6a 15 个端点里只做了 12 个（`promote` 属 S5、`ai-infer`/`ai-gen` 属 S6c、`executions/{id}/logs` 需日志 Hook），如果不在矩阵里显式标 deferred，就是「PRD 说有、代码没有」的老病复发。

**F10 · 异步任务的可见状态必须有明确归属**
- **规则**：引入异步（REST 触发执行 → 202 + id → 轮询）时，必须同时给出：状态枚举、状态存储、以及**存储的生命周期声明**。
- **为什么**：异步把「一次调用」拆成了「触发」和「查询」两个动作，中间那段状态如果没有明确的存放位置和生命期，就会出现「接口返回了 id，但没人能说清这个 id 活多久」。我们用内存 `ExecStore` 是有意选择（MVP），但**必须写出来**，否则将来会被误当作已持久化（→ §0 第 14 条）。

**F11 · 优雅停机 = 取消 + 等待收敛**
- **规则**：`Shutdown` 必须 `cancel()` **并等待** 受管子任务退出（`appWG.Wait()`），不能只发信号就走。
- **为什么**：这是本次真实观察到的——测试退出时日志出现 `! Flow svcf 监管结束: sql: database is closed`：registry 已关闭而 Supervisor 还在读库。停机竞态在生产上表现为「关停时偶发报错 / 丢数据」，在测试上表现为「偶发噪音」，两者都会钝化对异常的敏感度。

**§0 第 13 条 · 时序断言禁用绝对阈值**
- **触发**：全量 `go test ./...` 时 `TestParallelExecution` **偶发**失败——断言「3 个 0.4s step 并行应 < 900ms」，实测 901.6ms。单包跑必过，全量跑由于各包并行、机器负载升高而越界。
- **改法**：改为同一进程内先量**串行基线**再比并行，断言比值（`parallel*4 < serial*3`）。连跑 3 轮全量验证稳定。
- **为什么**：绝对耗时阈值的失败**与代码无关**，只与当时机器多忙有关。这类 flake 一旦被贴上「偶发」标签就会被忽略，随后真故障也被忽略——它污染的是**人们对红灯的信任**。规则要写成方向性的：**下界安全（负载只会更慢），上界必须相对化**。

**§0 第 14 条 · 状态持久性显式声明**
- **触发**：新增 `ExecStore`（执行记录）与 `AppStore`（受管进程句柄）都是进程内 map。
- **为什么**：这是「静默假设」的新形态——过去我们防的是「字段被静默忽略」，现在要防的是「**生命周期被默认**」。没人会在代码里写「我以为它持久化了」，但所有人都可能这么默认。解法成本极低：注释 + 文档里写一句「内存态，进程重启即失」。

**新增共享状态必须补并发用例**
- **为什么**：`ExecStore` 是新的并发共享状态（HTTP 天然并发）。`Get` 若返回结构指针，调用方会在锁外读到写一半的结构——故返回**副本**。已用 8 路并发触发的用例在 `-race` 下固化。

**覆盖率闸门的价值实证**
- 加完 `DeleteTool`（含守卫分支）后，**registry 覆盖率从 82.4% 掉到 66.4%**——因为它的分支只在 server 的 HTTP 测试里被间接覆盖，包内没有单测。闸门当场报警，补上 `TestDeleteTool` 后回到 82.1%。
- **这条经历说明**：集成测试的覆盖**不回流**到被调用包；「行为在别处测过」不能替代包内的单元验证。覆盖率闸门正是用来抓这种「测试债」的。

---

### 2026-09-13（第三次）· 模板静态校验 + 运行中注销守卫 + 枚举式声明逐项登记

**本轮交付（代码+测试已于上一轮完成，本次补文档回填）**
- **模板静态校验（REQ-TMPL-VALID）**：`internal/engine/template.go` 收敛 `${...}` 为**唯一解析器** `scanTemplateRefs`；`validateTemplates` 在注册期校验「前缀合法（vars/steps）/ vars 顶层键存在 / steps 带 id」，未知前缀（secrets）注册期拒。CLI `flow register` 与 REST `POST /api/flows`（经 `ValidateFlowStatic`）共用。
- **运行中注销守卫（REQ-DEL-TOOL-RUNNING）**：registry 注入 `RunningChecker`，注销第三条「运行中 Execution 持有的 Tool 不可注销」落地；REST `DELETE /api/tools/{id}` 运行中 409、结束后 200。`engine.RunFlowResultObserved` 在快照点回报计划用到的 Tool id（含仅作 Hook action 的），使运行侧引用对守卫可见（静态引用看不到这部分）。
- **修复既有 bug**：`when` 空串被 `compileWhen("")` 判为 `unexpected token EOF` → 所有不含 `when` 的 Flow 都无法过静态校验（REST 一律 400）。`CompileWhen` 空串返回 nil 修复；`TestStaticValidateAcceptsFlowWithoutWhen` 锁死。

**F12 · 枚举式声明必须逐项登记** → 对应 §0 第 15 条
- **规则**：任何「合法取值集合」必须（a）在**单一来源**登记全部合法值；（b）在解析/校验点**拒绝未知值**；（c）在 PRD/矩阵**同步状态**。三者缺一则构成漂移隐患。
- **触发（真实漂移 M11）**：PRD §4.3 把 `${secrets.x}` 列为「支持的变量引用前缀」，而引擎只实现 `vars`/`steps` 且从未登记 `secrets`。代码侧「没有这个前缀」与文档侧「列了它」长期共存、无人察觉——因为既没有单一来源登记、也没有解析点拒绝（未知前缀被静默忽略），PRD/矩阵也没标 deferred。直到补上 `validateTemplates`（未知前缀注册期拒）才暴露。
- **为什么必须写成规则**：模板前缀只是这一类问题的代表；runtime 名、hook stage、criticality、step mode 都是「枚举式取值」。本次之前 `Tool.Validate` 不校验 runtime（G1）也是同一种病。统一成一条红线后，任何新枚举都得走「登记 + 校验 + 文档同步」三步，漂移从根上失去藏身之处。
- **落地**：`template.go` 注释写明「新增语法（如未来的 `${secrets.x}`）须在此登记合法前缀并同步 REQ」；§0 第 15 条 + 本条目共同锁死。

**§0 第 10 条（F3 延伸）的实证补充**
- 模板静态校验是「能静态校验的不留到运行期」的又一次落地：此前 `${secrets.x}` 即使写错也只在**某次执行恰好走到该 step** 才炸，注册期完全放行。校验前移后，错误在 `flow register` / `POST /api/flows` 当场暴露，并指向 `REQ-TMPL-SECRETS` 告知使用者「是等待还是改写」。

**覆盖率闸门再实证**
- 本轮补的 `TestDeleteToolRunningGuard` / `TestDeleteToolWhileRunning` 是**包内单测**：registry 从 82.1%→83.4%、server 从 82.9%→83.9%——印证 S6a 已证结论（集成测试的覆盖不回流到被调用包，「行为在别处测过」不能替代包内单元验证）。`template_test.go` / `planned_test.go` 使 engine 85.8%→87.7%。

### 2026-09-13（第十一次）· M-KV-LIFECYCLE：KV 增生命周期维度 + §9 AI 决策修正

**M-KV-LIFECYCLE（KV 生命周期维度）**
- **触发**：用户要求「考虑一个生命周期为 flow 的会话级 env，允许 tool 缓存信息，考虑改造现有 kv 接口、增加生命周期机制」——直接对应 §0 第 14 条（状态持久性必须显式声明）。原来 `kv` 只有进程级全局命名空间，跨 step / 跨 flow 都共享同一份，无法支撑「一次 flow 执行内工具间缓存、执行完即清」的语义。
- **改了什么**：
  - `internal/cap`：`Scope` 类型（`ScopeGlobal`/`ScopeSession`）；`KV` 接口三方法增 `scope` 形参；新增 `SessionRegistry`（`Session(id)`/`Acquire(id)`/`Release(id)` 引用计数回收）；`Context.KV()` 在 `sessionID!=""` 且宿主提供 `Sessions` 时返回按 Scope 路由的 `scopedKV`，否则 session 键降级为 global（不报错、向后兼容）；`MemSessionRegistry`（内存态、引用计数归零即删命名空间）。
  - `internal/engine`：`defaultProviders()` 注入 `Sessions: cap.NewMemSessionRegistry()`；`hostForChain()` 在 `via` 非空时 `SetSession(via[末])=flow.ID`；`run()` 对 `flow.ID` 做 `Acquire`/`defer Release`（并发同 flow 互不干扰，最后一个结束才回收）。
  - `internal/engine/lua.go`：`ctx.kv` 的 get/put/delete 增可选 scope 参数（第 2/3 位），缺省 `ScopeGlobal` 向后兼容。
  - 测试：`cap_test.go` 新增 T-kv-01..05（隔离/释放/引用计数/路由/降级）；`kv_session_test.go` 新增 E-kvl-01（跨 step 共享 + run 结束回收）、E-kvl-02（session 与 global 互不污染）。`go build ./...` + 全量 `cap`/`engine` 测试绿灯。
- **为什么**：这是 §0 第 14 条的最小代价落地——只加一个显式维度，不引入落盘依赖；所有会话态内存态、重启即失，生命周期写在类型与接口里而非「默认假设」。它直接为后续 `ext.ai` 多阶段 agent（同一次 flow 内检索结果/中间态跨 step 复用）与「web-ui-as-Flow 管理面」复用。

**§9 AI 决策修正（去内置化 → 扩展化）**
- **触发**：用户本轮明确「对齐工程方法，去掉 ai 作为内置，作为扩展能力」，并参考此前 AI 能力梳理——**推翻**此前「整体去除 AI 功能」的结论（该结论本身也已与后续「built-in AI 闭环」的实现记录自相矛盾）。真实意图是：**AI 不该进内核，但也不该消失**——把它从 built-in 重定位为「用户/插件可注册的 `ext.ai` 扩展原语」（Sidecar 承载），经既有的 `CapabilityRegistry` 接入，`ctx.ext.ai.*` 暴露给 Lua，引擎侧只做接线 + `ValidateFlowStatic` 校验。
- **影响面（待 M-EXT-AI 切片收批）**：原 built-in `internal/ai`（SiliconFlow/Ollama 推断器、`POST /api/ai/infer`、`cli ai gen`）将移除，改由 `examples/gear-ai-sidecar` 以 Sidecar 实现；AI 入口点 = 参考 Flow（ref Flow 注入 `tool_id+desc+schema`）+ 薄 CLI 壳（`gear ai gen`）；agent 形态 = 结构化多阶段（能力域概览 → LLM 拆解 → 域内检索 → 组装 Flow → ValidateFlowStatic 校验 → ≤N 次重来 → 人工确认/注册），不自由 ReAct。
- **矩阵同步**：§9 行状态由「🔴 已移除（整体去除）」翻为「🟡 去内置化 → 改 `ext.ai` 扩展能力（规划中）」，REQ-API-AI 翻 `ext`；本切片（M-KV-LIFECYCLE）已 🟢，§9 随 M-EXT-AI 签批后翻 🟢。

### 2026-09-14（第十二次）· M-EXT-AI：AI 去内置化 → `ext.ai` 扩展（全切片 ✅）

**M-EXT-AI-1（引擎接线）**
- **改了什么**：`internal/engine/extensions.go` 抽出底层 `sidecarExtension(name, transport, methods)` 供 `SidecarExtension` 与新增 `AIExtension(cmd)` 复用；`engine.AIExtension` 注册 `ext.ai`（`generate_flow`/`generate_tool`/`complete` 三方法，`AIMethods` 常量）；`engine.DefaultAISidecarCmd()` 取 `GEAR_AI_SIDECAR` 或回退 `go run ./examples/gear-ai-sidecar`。`server.go` 与 `cli` 经 `engine.WithCapabilities` 默认装配 `ext.ai`（sidecar 二进制缺失时 fail-closed 返回 `ErrCapabilityUnavailable`）。（注：`generate_flow`/`generate_tool` 于**第十三次**重构移除，仅留 `complete/catalog/domains/validate`，agent 编排移入 Lua——见本文件 §11 第十三次。）
- **测试**：`extensions_test.go` 新增 `TestExtAIReachable`（Lua `ctx.ext.ai.complete` 正确转发）、`TestExtAINotDeclaredDenied`（未声明即报错的门禁）。

**M-EXT-AI-2（gear-ai-sidecar 结构化多阶段 agent）**
- **为什么**：AI 能力全部活在 sidecar 子进程里——内核只认内核原语（受门禁 7 + 纯函数 3），agent 逻辑可自由演进且不污染引擎；sidecar 协议复用 process runtime（stdin `{method,args}` → stdout 末行 `{result}`/`{error}`）。
- **改了什么**（`examples/gear-ai-sidecar/`）：`catalog.go`（ToolSummary 最小上下文 + `DomainOverview` 按 namespace 归类 + `Retrieve` BM25 式域内检索）、`llm.go`（`LLM` 抽象，SiliconFlow 默认 `+` Ollama 兜底，key 走 env）、`validate.go`（把 LLM 产出经 `engine.ValidateFlowStatic` 校验，复用单一真相源）、`agent.go`（生成 Flow/Tool 的多阶段管线 + ≤N 重试 + `extractJSON` 字符串感知容错解析）、`main.go`（协议分发）。
- **测试**：`agent_test.go` 用 `queueLLM` mock + mock catalog 覆盖 成功 / 校验失败→重试 / 全失败 / Tool 生成 / normalize 补默认 / 静态校验拒未知工具 / 域概览+检索排名，全绿。

**M-EXT-AI-3（参考 Flow + 薄 CLI 壳 + 文档收口）**
- **改了什么**：`examples/ai-create-flow.yaml`（两篇文档：ai-gen Tool `capabilities:[ext.ai]` + ai-create-flow Flow，注入 `intent/tools/mode`）；`internal/cli/ai.go` 新增 `gear ai gen flow|tool`（嵌入参考 Flow，注入工具目录 + 意图 → 内存 registry 跑一遍 → 打印草稿，`--register` 落库）；`root.go` 注册 `ai` 命令；`internal/cli/ai_test.go` 锁死嵌入 YAML 解析/校验 + `buildCatalog` 最小上下文。
- **清理**：原 built-in `internal/ai` 包此前（S6c 撤回时）已删除，本轮确认无残留（仅 `server_test.go` 一处历史注释，已保留上下文）。
- **文档**：PRD §9 翻「已实现（作为 ext.ai 扩展）」+ §9.2 编排闭环改为结构化多阶段 + REST 端点行说明改由 `ext.ai` 承载；REQ-API-AI 翻 `ext`；§9 矩阵行（🟡→🟢）、§4 路线图（⏳→✅）、本文件 S6c 行（已撤回→已改 ext.ai ✅）同步。
- **矩阵同步**：§9 行最终翻 🟢；§11 API 表中 `/api/ai/infer`、`:id/ai-gen` 标为「REST 端点已移除、AI 改 ext.ai 扩展」，状态说明与 PRD §9 一致。

### 2026-09-14（第十三次）· M-EXT-AI-LAYER：AI 三层分层重构（推翻 M-EXT-AI 的 Go 单体 agent）

**触发（用户设计批评）**
- 用户在 M-EXT-AI 落地后指出：实现的 AI 是「笼统的一坨」，违背了本框架的底层哲学——**分层**，即能力的分层、职责的分层。要求：**LLM 做一层**（支持各种参数，比如模型、url、key）；**agent 分一层**（易变、有各种可能和需求的用 Lua）；**不变的用 Go 静态编译进去**。
- 此前 `examples/gear-ai-sidecar/agent.go`（Go 单体多阶段编排）正是「笼统一坨」的反例，必须拆开。

**改了什么（按三层重切）**
- **删除 Go 单体**：`agent.go` + `agent_test.go` 删除（原 `GenerateFlow`/`GenerateTool` 的 Go 编排）；`engine/ai_extension.go` 的 `AIMethods` 由 `generate_flow/generate_tool/complete` 改为稳定原语 `complete/catalog/domains/validate`，`AIExtension` 不再承载任何 agent 逻辑。
- **LLM 层（稳定、参数化）**：`llm.go` 重写为参数化 LLM 原语——`LLMRequest{System,Prompt,Provider,Model,BaseURL,APIKey,Temperature,Format}`、`Chat()` 按优先级 调用参数 > env(`GEAR_AI_*`) > 默认；默认 provider `siliconflow`/`https://api.siliconflow.cn/v1`/`Qwen/Qwen3-8B`，ollama `http://localhost:11434`/`qwen3:latest`；`format="json"` 经 `jsonx.go` 的 `extractJSONToAny`（字符串感知括号匹配）解析；`provider="mock"` / `GEAR_AI_PROVIDER=mock` 走确定性离线 `mockChat`（planner→steps、tool→tool、else→flow），供测试。
- **静态原语层（Go 编译）**：`catalog.go`（BM25 域内检索 + 按 namespace 域概览）、`validate.go`（`Validate(kind,def,tools)`，flow 模式重建内存 registry 跑 `engine.New(reg).ValidateFlowStatic`、tool 模式跑 `model.Tool.Validate`，单一真相源）、`main.go` 分发 `complete/catalog/domains/validate`（原 `agent.go` 的 `GenerateFlow`/`GenerateTool`/`extractJSONTo` 迁移到 `jsonx.go`/`validate.go`）。
- **Agent 层（易变、Lua）**：`examples/ai-create-flow.yaml` 的 `ai-gen` Tool `entry` 重写为完整多阶段 Lua agent（Flow 模式 + Tool 模式双实现）：① `ctx.ext.ai.domains({tools})` 域概览 → ② 包装 `llm_json`（`ctx.ext.ai.complete({system,prompt,format="json"})` 取 `.data`）拆解步骤 → ③ 每步 `ctx.ext.ai.catalog({query,domain,tools,k=5})` 检索 → ④ 组装 → ⑤ `ctx.ext.ai.validate({kind,def,tools})` 循环、失败回灌、`max_retries`（默认 3）重试。注释明示「易变层，改 agent 只改这里、不重编 Go」。

**引擎修复（连带）**
- `internal/engine/engine.go` 的 `resolveValue`：对整串模板引用（如 `${vars.tools}`）直接返回原始复杂值，不再经 `expandString` 字符串化——否则 catalog 切片会被打成字符串。此修复同时让真实 `gear ai gen` 能正确把工具目录传给 Lua agent。

**测试**
- 新增 `internal/engine/ai_agent_test.go`：进程内 `mockAI`（实现 `complete/catalog/domains/validate` 的 `ext.ai` stub，`complete` 按 system 文本返回不同 JSON、`validate` 首校验故意失败以触发 Lua 重试循环）→ `loadAIAgentEntry` 从 `examples/ai-create-flow.yaml` 抽取真实 Lua entry（单一真相源）→ `runAIAgent` 装配 ai-gen Tool + ai-create-flow Flow（vars: intent/tools/mode/max_retries）跑 `RunFlowResult`。`TestLuaAgentFlowCompilesPrimitives` 断言 `kind=="flow"`、`out["flow"]` 存在、`completeCalls>=3`、`validateCalls>=2`（重试生效）；`TestLuaAgentToolCompilesPrimitives` 断言 `kind=="tool"` + 重试计数。
- 更新 `extensions_test.go`：`TestExtAIReachable`/`TestExtAINotDeclaredDenied` 改用 `ctx.ext.ai.complete(...)`。
- 新增 sidecar 单测：`catalog_test.go`（域概览分组 + 域内排名 + 跨域回退）、`validate_test.go`（好/坏 Flow + 拒未知工具 + 好 Tool）、`llm_test.go`（配置优先级 per-call>env>default + mock + JSON 提取）。

**验证**
- `go build ./...` 通过；`go test ./internal/cap/ ./internal/engine/ ./internal/cli/ ./examples/gear-ai-sidecar/ ./internal/server/ -count=1` 全绿（engine 含新 Lua agent 组合测试）；gofmt 已应用于本次改动的全部 Go 文件。

**矩阵同步**：§9 行测试 ID 增补 `E-ai-01..02`（Lua agent 组合测试），描述补三层架构；本文件 §4 M-EXT-AI 里程碑行与 PRD §9 同步翻三层分层。

### 2026-09-14（第十四次）· 配置化扩展加载 + `ext.registry` 自我锚定
- **触发**：用户指出两点——① 扩展装配仍写死在启动代码（`cli/engine.go` 与 `server/server.go` 各一行 `Register("ext.ai", ...)`，`GEAR_AI_SIDECAR` 只是这一个 sidecar 的单点 env 覆盖，并非通用机制）；② `catalog`/`domains`/`validate` 三个原语都把 `tools` 当入参、没有任何原语/扩展能读引擎 registry，agent 永远「等调用方喂工具清单」，LLM 没法自我锚定、必幻觉。
- **改了什么**
  - **配置化扩展加载（取代写死 ext.ai）**：`internal/engine/extensions.go` 新增 `ExtSpec` + `LoadExtensionConfig`（读 `extensions.yaml`，优先 `GEAR_EXT_CONFIG`，否则 cwd 的 `extensions.yaml`；文件不存在回退默认 `ext.ai`）+ `SetupExtensions(capReg, reg, cfgPath)`（始终注册内建 `ext.registry`，有配置则逐个 sidecar 注册、否则回退默认 `ext.ai`）。`cliEngine` 与 `server.New` 改为调用 `SetupExtensions`（二者签名各加 `error` 返回，fail-closed：配置非法即中断启动）。新增 `extensions.example.yaml` 模板（`extensions.yaml` 已加 `.gitignore`）。**发布后加 `ext.tls` 等原生能力只需在 `extensions.yaml` 加一段，不必改 gear 源码**。
  - **`ext.registry` 进程内原语（自我锚定）**：`extensions.go` 新增 `InProcessExtension`（进程内 Go 原语通用构造，与 `SidecarExtension` 并列，同样 `ext.*` 命名空间 + capabilities 白名单）+ `RegistryExtension(reg)`（`ctx.ext.registry.tools()` / `ctx.ext.registry.flows()` 读引擎 registry 真实目录）。`ai-gen` 的 `capabilities` 由 `[ext.ai]` 扩为 `[ext.ai, ext.registry]`，入口去掉 `tools` 入参、改为 `local tools = ctx.ext.registry.tools()` 自行读取；`ai-create-flow` 去掉 `tools` var/step-input，`cli/ai.go` 不再注入工具清单（`runAIGen` 改为把持久 registry 的真实工具/流程灌入内存 `refReg`，使 `ext.registry` 读到真实目录——隔离运行但不隔离「读」），删除死代码 `buildCatalog`/`orSchema`。
  - **连带修复 `toLuaValue` 字符串化（同类 bug）**：`internal/engine/lua.go` 的 `toLuaValue` 原只处理 `[]interface{}`，具名元素类型切片（如 `[]map[string]interface{}`，正是 `RegistryExtension` 返回类型）会掉进 `default` 被 `fmt.Sprint` 成字符串，致 Lua `ipairs` 报「got string」。改为在 `default` 用 `reflect` 递归展开任意 slice/map，与 `resolveValue` 整串引用修复是同一类问题，一并杜绝。
- **测试**
  - `ai_agent_test.go`：`runAIAgent` 注册真实 `RegistryExtension(engReg)`（先灌 `demo.echo` 工具），ai-gen `capabilities` 加 `ext.registry`，去掉 `tools` 注入；断言 agent 经 `ctx.ext.registry` 自我锚定 + 重试闭环。
  - `cli/ai_test.go`：`TestBuildCatalog` 删除（测已删的 `buildCatalog`），改 `TestSeedRefRegistry` 验 `seedRefRegistry` 把真实工具/流程灌入 refReg；`TestLoadAICatalog` 同步 `ext.registry` 扩展名注册与 capabilities 断言。
- **验证**：`go build ./...` 通过；`go test ./internal/engine/ ./internal/cli/ ./internal/server/ ./internal/cap/ ./internal/app/ ./examples/gear-ai-sidecar/ -count=1` 全绿；gofmt 已应用于改动文件。
- **矩阵同步**：§9 矩阵行补「扩展装配配置化 + `ext.registry` 自我锚定」；PRD §9 实现形态 callout 同步（加 Registry 内省原语 bullet、入口改配置化说明）；REQ-API-AI 补 `ext.registry` 与配置化加载。

### 2026-09-14（第十五次）· 真实 worker agent：`news.top10` + `GET /api/news` + `ctx.json` 原语
- **触发**：用户质疑「我看到的 agent 都有直接调工具的能力，你这个 flow-agent 是不是不行」+ 要求「不用 mock，弄真实例子看他干活」+ 例「整理百度新闻 top10，在 server 的 /api/news 里提供服务」。
- **澄清（关键认知）**：GEAR 的 agent 分两类——*生成器* `ai-gen`（PRD §9 边界，只产出 DSL、不直接执行）与*执行器（worker）*（声明 `capabilities:[http]` 等的 Lua Tool，运行时直接调 `ctx.http`/`ctx.call`/`ctx.db`/`ctx.ext.ai` 干活）。前者是「设计态」、后者是「运行态」，都活在 Lua、改行为不重编 Go；典型 ReAct「直接调工具」就是后者。
- **改了什么**
  - **`ctx.json` 引擎内建稳定原语**（`internal/engine/lua.go`）：`decode(str) → table|nil,err`、`encode(table) → string`。gopher-lua 不带 json，原先 `ctx.http.Request` 返回字符串 body 无法结构化——这是所有 http 工具的命脉，故作为随引擎编译的静态能力无条件提供（不属 capabilities 门禁）。复用既有 `toLuaValue`/`fromLuaValue`。
  - **真实 worker 示例 `examples/news-top10.yaml`**：Tool `news.fetch`（Lua + `capabilities:[http]`，`ctx.http.Request` 抓百度热搜榜真实接口 `top.baidu.com/api/board?platform=wise&tab=realtime`，`ctx.json.decode` 解析 `data.data.cards[].content[].content[]` 取 Top10：`rank/title/url/hot`）+ Flow `news.top10`（单步调 `news.fetch`）。**零 LLM、零 mock、真实网络**。
  - **独立演示 `examples/news-demo/main.go`（示例目录已移除）**：开内存 registry → `registry.LoadExamples` 解析 YAML → 注册 → `engine.RunFlowResult("news.top10")` → 打印真实 Top10（当时 `go run ./examples/news-demo` 实测抓到真实榜单；该示例现已移除，等效行为由 `examples/news-top10.yaml` + `gear serve --seed` 承载）。
  - **`GET /api/news` 端点**（`internal/server/server.go`）：同步 `RunFlowResult("news.top10")` 取 step `gen` 的 `output`，附 `served_at` 时间戳返回；Flow 未注册 → 404（提示用 `--seed`）。
  - **`gear serve --seed <yaml>`**（`internal/cli/serve.go`）：启动时把示例 Tool/Flow 灌入注册表，使 `/api/news` 开箱即用；共享解析器 `registry.LoadExamples`（多文档 YAML，按 `steps` 区分 Flow/Tool）。
  - **复用既有能力**：`defaultProviders()` 已自带 HTTP 客户端，故 `ctx.http` 在 `engine.New(reg)` 下即可用；`RunResult.Steps[stepID]["output"]` 取工具返回值。
- **测试**：`internal/server/server_test.go` 新增 `TestNewsEndpoint`（本地 mock 百度接口返回 Baidu 形状 JSON，指向真实示例文件，确定性验证 `/api/news` 返回 3 条 + `served_at`，不依赖外网）；`internal/registry/loader_test.go` 可选。`go build ./...` 通过；受影响包（engine/cli/server/cap/registry/app/gear-ai-sidecar）全绿；gofmt 已应用。
- **矩阵同步**：PRD §9 实现形态 callout 补「两类 agent 区分」+「`ctx.json` 内建稳定原语」；本文件 §9 行增补 worker agent + `/api/news` + `ctx.json` 说明。

### 2026-09-14（第十六次）· 发布后「用户自助」真机验证 + 修 4 处阻塞 bug
- **触发**：用户明确视角——「**编译好发布之后**，用户如何实现这些功能（加扩展 / 配凭证 / 管 tool·flow）」，并提供真实 SiliconFlow 端点 + 模型（`deepseek-ai/DeepSeek-V4-Flash`）+ key 要求「完成效果给我看」。
- **验证结论（真机，非 mock）**：预编译 `gear.exe` + `gear-ai-sidecar.exe` 后，**零源码改动**即可三件事全通——① 两条环境变量（`GEAR_AI_API_KEY` / `GEAR_AI_MODEL`，provider/base 走默认）即让 `gear ai gen` 真调 SiliconFlow，产出合法 Tool 与 Flow 草稿；② 用户自写 **Python** sidecar + 一段 `extensions.yaml`（`ext.tls`）即让全新原生能力 `ctx.ext.tls.*` 可用（`tls.issue` 工具真跑出 PEM）；③ `gear serve` 的 REST（`GET /api/tools`·`/api/flows`、`POST /api/flows/:id/validate`）即 UI 可对接的管理契约。**这三条就是「发布后用户自助」的完整答案。**
- **修了什么（先前实现里会让发布后用户必然撞墙的 4 处）**
  1. **`runAIGen` 注册顺序**（`internal/cli/ai.go`）：`refReg.UpsertTool(ai-gen)` 跑在 `cliEngine(refReg)` **之前**，而 ai-gen 声明 `ext.registry`；全局合法 capability 集合由 `SetupExtensions` 填充，故注册期校验必报「未知的 capability "ext.registry"」。改为**先建引擎、再做任何注册**——且建引擎须前置于 `seedRefRegistry`（灌入工具同样过 capability 校验，若工具声明 `ext.tls` 亦然）。顺带去掉了原先手动补 `cap.RegisterExtensionName("ext.ai")` 的补丁（`SetupExtensions` 已负责）。
  2. **`tool register` 未初始化扩展名**（`internal/cli/tool.go`）：该命令直接 `reg.UpsertTool`，从不建引擎 → 全局扩展名表为空 → 用户配好 `ext.tls` 后注册 `capabilities:[ext.tls]` 的 Tool 必被拒。新增 `initExtensionNames(reg)`（`cli/engine.go`，借 `SetupExtensions` 的副作用并入全局名集合），在 `toolRegisterCmd` 校验前调用。（`flow register` 本就先 `cliEngine` 再校验，无此问题。）
  3. **agent 层 schema 容错**（`examples/ai-create-flow.yaml` + `internal/cli/ai.go` 嵌入副本）：真实 LLM 会把 `steps[].depends_on` 写成对象（如 `{"s1":true}`），Go 侧 `json.Unmarshal` 进 `model.Flow`（`[]string`）即失败。**容错放在易变层（Lua）**：新增 `norm_flow`（对象取键 / 标量包数组 → 字符串数组），并在组装 prompt 明写「`depends_on` 必须是字符串数组或省略，绝不能是对象」；**稳定层（Go 反序列化/静态校验）保持严格**——这正是「易变/稳定分层」的教科书用例。
  4. **（记录，未改）step 超时硬编码 30s**：`internal/engine/process.go` 的 `defaultExecTimeout = 30 * time.Second` 无 flag/env 可调。多轮调用远端 LLM 的 agent 贴近该边界（首轮空 registry 触发重试时会超时）。属「调优缺口」，建议后续开一个 per-step `timeout` 字段或 CLI flag，本轮不改。
- **文档**：`extensions.example.yaml` 补警告——配置存在时「以配置为准」，须把用到的扩展（含 `ext.ai`）列全，否则能力静默消失。
- **验证**：`go build ./...` 通过；`go test ./internal/engine/ ./internal/cli/ -count=1` 全绿；gofmt 已应用。真机 demo 产物在 `.postpublish-demo/`（用户侧文件，非 gear 源码）。

### 2026-09-14（第十七次）· `ext.db`：连接各种数据库的扩展（driver/dsn 参数化，与 LLM 层同构）
- **触发**：用户问「是否可以提供一个连接各种数据库的扩展」。
- **澄清前提**：内置 `ctx.db` 只是 `internal/cap/sqldb.go` 的**内存 Scratch SQLite**（每引擎唯一 DSN、重启即丢，M5 scratch 语义），**不能连真实外部库**——所以「连各种数据库」正该由 `ext.*` 补，且落在「发布后用户自助」那条线上。
- **设计（复用分层，不发明新范式）**：与 LLM 层严格同构——`query({driver, dsn, sql, params})` 对应 `complete({provider, model, base_url, api_key})`。稳定层＝Go 静态编译的 sidecar（协议 + `database/sql` 通用管道 + 驱动注册）；参数层＝`driver`/`dsn`（每次调用携带，env 回退）；易变层＝具体 SQL / 事务编排（写在 Lua Tool 里，改查询不重编 sidecar）。
- **交付**
  - `examples/gear-db-sidecar/main.go`：sidecar 协议（stdin 一行 `{method,args}` → stdout 末行 `{result|error}`）+ 方法分发（`query`/`exec`/`ping`）。
  - `examples/gear-db-sidecar/db.go`：`driverAliases`（`sqlite`/`sqlite3`/`mysql`/`mariadb`/`postgres`/`postgresql`/`pg` → `database/sql` 注册名）+ `resolveDB`（**调用参数 > `GEAR_DB_DRIVER`/`GEAR_DB_DSN` > 报错**）+ `dbQuery`（返回 `columns`/`rows`/`row_count`）/`dbExec`（返回 `rows_affected`）/`dbPing`。参数走预编译占位符，天然防注入。
  - 驱动：`sqlite`＝既有 `modernc.org/sqlite`（纯 Go 无 cgo）；`mysql`＝`github.com/go-sql-driver/mysql` v1.10.1；`postgres`＝`github.com/jackc/pgx/v5` v5.11.0 的 `stdlib` 适配（GOPROXY=`goproxy.cn` 可拉）。加新库＝`import` 驱动 + `driverAliases` 加一行，或另写任意语言 sidecar（如 Python）。
  - `examples/gear-db-sidecar/db_test.go`：临时 SQLite 文件走通建表→插入→参数化查询→ping，并覆盖「缺 driver / 缺 dsn / 未知 driver」三条错误分支。
- **验证（真机）**
  - sidecar 单测全绿（`ok gear/examples/gear-db-sidecar`）。（2026-09-19 注：该示例源码随后被内置 `db` 多后端原语取代并移除，本行为当时的验证记录，现已不可复现。）
  - 驱动接线：`ping` 打不存在的地址时，PG 报错来自 `pgx`、MySQL 报错来自 `mysql` 驱动、未知 driver 明确拒绝 → 三种驱动确已注册（无需真库即可证）。
  - 「发布后用户」闭环（`.postpublish-demo`）：`extensions.yaml` 加 `ext.db` 段 → `db.add` INSERT（`rows_affected:1`）→ `db.people` 参数化 SELECT 返回上海 3 行（含刚插入者），`depends_on:["add"]` 保证先写后读。
- **已知约束（记录）**：`CommandTransport` 一次调用起一个进程 → **每次调用重连数据库**。SQLite 廉价；网络库高频场景建议配连接池/代理，或后续引入**长驻 sidecar 传输**（需给扩展加生命周期/关闭钩子，本轮未做）。
- **文档**：PRD §9.4 加「④ 连各种数据库」；`extensions.example.yaml` 加 `ext.db` 段 + 用法注释。

### 2026-09-14（第十八次）· CLI 误用护栏：`--registry` 混淆 + 注册子命令用错
- **触发**：用户在 `.postpublish-demo` 执行 `gear flow register .\db-people.yaml --registry .\demo-people.db`，报 `flow db.people 至少需要一个 step`。**实为两个错**：① `db-people.yaml` 是 **Tool**，该用 `tool register`；② `--registry` 应是 gear 的**目录库**（`./demo.db`），却指向了 `ext.db` 要连的**业务库** `demo-people.db`。实测后果：gear 把注册表建进了用户业务库——`demo-people.db` 变成 `['flows','hooks','people','tools']`（仅 `people` 是业务表，其余三张是 gear 建进去的）。
- **改了什么（三处，均不阻断）**
  1. **注册子命令前置守卫**（`internal/cli/flow.go` / `tool.go`）：解析后、**打开 registry 之前**检查——`flow register` 若发现 YAML 无 `steps` 且含 `runtime`/`entry`（像 Tool），报「请改用 `gear tool register <file>`」；`tool register` 若发现 YAML 含 `steps`（像 Flow），报「请改用 `gear flow register <file>`」。**先于 `openRegistry()` → 误用不再创建/污染任何库文件**。
  2. **`--registry` 误指向护栏**（`internal/cli/root.go`）：`openRegistry()` 打开前扫 `sqlite_master`，若该文件含非 gear 表（gear 注册表为 `tools`/`flows`/`hooks`），向 stderr 警告并列出这些表——提示「`--registry` 应是 gear 的目录库，不是你应用的数据库」。只警告不阻断（存在有意共用的合理场景）。
  3. **测试**（`internal/cli/cli_test.go`）：新增 `TestRegisterSubcommandMismatch`——两个方向各误用一次，断言错误信息含正确替代子命令，并断言**该 registry 文件未被创建**（把「守卫在 openRegistry 之前」这一不变量钉死）。
- **用户侧修正**：清理 `demo-people.db`（DROP `flows`/`hooks`/`tools`，只留 `people`）；正确命令为 `gear tool register ./db-people.yaml --registry ./demo.db` + `gear flow register ./demo-db.yaml --registry ./demo.db`，跑通得到上海 4 行。
- **文档**：PRD §9.4 加「⑤ 两个易踩点（CLI 已加护栏）」。
- **验证**：`go build ./...` 通过；`go test ./internal/cli/ -count=1` 全绿（含新增用例）；gofmt 已应用。

### 2026-09-14（第十九次）· 运行时传参：`flow run --var` + 新增 `tool run`
- **触发**：用户问「`gear flow run demo-db-people` 怎么传参数」，继而要求「为 tool 也增加一个执行功能」。
- **先查后答（结论）**：`flow run` 此前**没有任何传参入口**——`RunFlow`/`RunFlowResultObserved` 签名里没有 input/vars，`model.Flow` 也没有 `input_schema`；`vars` 只能来自注册进 registry 的那份 YAML。模板前缀只有 `vars`/`steps`/`secrets`（无 `env`）。设计上「运行时注入」的官方通路本是 `before_flow` Hook（Tool 载体返回的非空 object **整体替换** `rc.vars`），但偏重。
- **改了什么（两块）**
  1. **`flow run --var k=v`（可重复）**：
     - `internal/engine/engine.go`：`run` 拆为「薄包装 `run` + 执行内核 `runWithVars(..., varsOverride)`」；新增 `mergeVars(base, override)`（覆盖优先、返回新 map，**不改写 `flow.Vars` 快照**）；新增公共 API `RunFlowWithVars(flowID, vars)`；把 `RunFlowExec` 与它共用的前置（取 Flow → 拒 service step）抽成 `runFlow(...)`。
     - `internal/cli/flow.go`：新增 `--var` 标志 + `parseKVFlags(flagName, pairs)`（值先按 JSON 解析 → 数字/布尔/数组/对象，失败则原样作字符串；缺 `=` 或空 key 即报错）。
     - 语义：覆盖**只作用于本次运行、不写回 registry**；合并结果仍会被 `before_flow` Hook 进一步改写。
  2. **`gear tool run <id>`（新增子命令，Tool 也有执行入口）**：
     - `internal/engine/engine.go`：新增 `RunTool(toolID, input)` / `RunToolExec(toolID, input, execID)`——**复用 `runOnceStep`**，故语义与「Flow 里跑一个 step」逐项一致：input_schema 校验 → capability 门禁 → runtime 执行（`defaultExecTimeout` 限时）→ step 级 Hook（before_step/after_step，按 tool id 匹配）→ output_schema 校验。session 命名空间用 tool id（等价「只含一步的 Flow」），结束回收；会触发 step 级 Hook，**不**触发 Flow 级 Hook（无 Flow）。
     - `internal/cli/tool.go`：`toolRunCmd` + `buildToolInput`——输入三选一（**互斥，混用即报错**）：`--input '<json>'` / `--input-file <path>` / `--arg k=v`（可重复，复用 `parseKVFlags`）。执行成功打印 `✓ Tool <id> 执行成功` + 输出 YAML。
- **验证（真机，`.postpublish-demo`）**
  - `gear tool run tls.issue --arg domain=x.com` → 真跑用户自写的 **Python** `ext.tls` sidecar，返回 PEM。
  - `gear tool run db.people --input '{"dsn":"./demo-people.db","city":"北京"}'` → 参数化查询返回 1 行。
  - 缺必填字段 → `input 校验失败: ... missing properties: 'dsn'`（fail-closed，不静默）。
  - `gear flow run demo-db-people --var city=北京` → INSERT+SELECT 均按「北京」执行；随后 `flow inspect` 确认 registry 里仍是 `city: 上海`（**未写回**）。
  - 测试：`internal/engine/runtool_test.go`（`TestRunTool` / `TestRunToolRejectsInvalidInput` / `TestRunToolUnknown` / `TestRunFlowWithVars`，后者额外断言「不写回 registry」）+ `internal/cli/cli_tool_test.go` 的 `TestToolRunExecutesAndPrintsOutput`（三种给法 `--input`/`--input-file`/`--arg` 均覆盖）+ `internal/cli/cli_test.go` 的 `TestResolveInput`（解析层：混用/非法 JSON/非法 k=v/空输入）。
- **文档**：PRD §14.6 CLI 管理面表按**实际命令树**修正（原表把 `tool unregister/inspect`、`flow save`、`gear secret` 等写错或已不存在），并补 `tool run` 与 `flow run --var` 两行。
- **验证**：`go build ./...` 通过；受影响包测试全绿；gofmt 已应用。

### 2026-09-15（第二十次）· 统一输入层：JSON 入参与 YAML 文件都吃 Unix 管道
- **触发**：用户建议「为 json 入参增加管道方式，就像 `cat xxx | findstr yyy`」，并问「tool / flow / hook 是不是都可以加」。
- **先查后答**：`gear hook` 只有 `list/register/show/remove`——**没有"执行 hook"命令**（hook 只在 Flow/step 执行时被触发），故「JSON 入参管道」对 hook 不适用；hook 真正需要的是**注册时从 stdin 读 YAML**。于是本轮把「输入」统一成两类：**执行入参（JSON）** 与 **注册文件（YAML）**，都遵循 Unix 管道直觉。
- **改了什么**
  1. **新增 `internal/cli/input.go`（统一输入层）**：`resolveInput(inputSpec)` 支持四种来源——显式标志（`--input`/`--input-file`/`--arg|--var`）> `-`（显式 stdin）> **未给标志且 stdin 是管道时自动读** > 空输入；显式标志互斥（混用即报错）。辅助：`readFileOrStdin(path)`（`-` 走 stdin）、`stdinIsPiped()`（只 Stat 不消费）、`readPipedStdin()`、`decodeJSONObject()`。
     - **不静默**：给了标志又接了管道时，打 stderr warning 告知「管道内容被忽略」（用 `Stat` 判定，不消费管道）。
     - 终端下**绝不阻塞**等待输入。
  2. **`tool run`**：`buildToolInput` 删除，改走 `resolveInput`；`--input`/`--input-file` 支持写 `-`。
  3. **`flow run`**：新增 `--input`（整体 JSON）/`--input-file`，与既有 `--var` 一并走 `resolveInput`（stdin 管道即 vars）。
  4. **`register`/`validate` 支持 `-`**：`tool register` / `flow register` / `flow validate` / `hook register` 均改用 `readFileOrStdin`（`cat x.yaml | gear xxx register -`）。
- **验证（真机，`.postpublish-demo`）**
  - `cat vars.json | gear flow run demo-db-people` → 按 stdin 的 vars 执行（city=北京）。
  - `echo '{"domain":"pipe.com"}' | gear tool run tls.issue` → 真跑 Python sidecar 出 PEM。
  - `gear tool run db.people --input - < in.json` → 成功。
  - `cat db-add.yaml | gear tool register -` / `cat demo-db.yaml | gear flow validate -` / `cat h.yaml | gear hook register -` → 全部成功。
  - `cat full.json | gear tool run db.people --arg city=北京` → 标志优先，管道被忽略并**打出 warning**。
  - 测试：`TestResolveInput`（含 `--input -` 与自动 stdin 两条路径，用 `os.Pipe` 替换 `os.Stdin`）、`TestReadFileOrStdin`、`TestRegisterFromStdin`（端到端 `tool register -`）。
- **踩坑记录**：`go build ./...` **不会**产出根目录 `gear.exe`；只跑它会导致「拿旧二进制验证新功能」——真机验证前须 `go build -o gear.exe ./cmd/gear`。
- **文档**：PRD §14.6 表补 stdin 说明 + 「统一输入约定」小节（含 5 条规则与 `hook` 不适用 JSON 管道的说明，因 hook 无执行命令）。
- **验证**：`go build ./...` 通过；受影响包测试全绿；gofmt 已应用。

## 10. 签批页

- [ ] 可追溯矩阵（§3）条目与测试 ID 认可
- [ ] 测试策略与覆盖率门槛（§2/§5）认可
- [ ] 切片划分与 DoD（§4）认可
- [ ] §13 待确认项处置（§7）认可
- [ ] S0 六处缺陷修复计划（§9）认可

> 签批后，从「S0 重做基线」开始，逐切片按本宪法推进。
