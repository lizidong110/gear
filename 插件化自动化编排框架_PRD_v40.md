# 插件化自动化编排框架 · PRD

> **版本**：v4.0（持续修订；本文件随代码落地持续回填，不另起版本号）
> **日期**：2026-09-20（持续修订；以 `REQUIREMENTS.md` 为可执行契约，凡标 `implemented` 的功能均已随代码落地）
> **状态**：Draft — 用于交付 AI 协作开发
> **定位**：内核定位 · 机制优先 · AI 编排

---

> **文档约定（F5 规格审计）**：本文档每条行为以 [`REQUIREMENTS.md`](./REQUIREMENTS.md) 为**可执行契约**——每条标 `implemented` / `deferred` / `contradicted→fixed`，并有对应测试断言。凡标 `deferred` 的，代码尚未实现（注册期会显式报错，不会静默忽略）；凡 `contradicted→fixed` 的，原为文档与代码不符、已修正文档示例。请勿据未实现的章节编写配置。

## 目录

1. [产品定位与设计哲学](#1-产品定位与设计哲学)
2. [核心原语](#2-核心原语)
3. [Tool（能力）](#3-tool能力)
4. [Flow（编排）](#4-flow编排)
5. [Hook（切面）](#5-hook切面)
6. [宿主能力原语](#6-宿主能力原语)
7. [版本与灰度发布](#7-版本与灰度发布)
8. [插件注册、注销与依赖](#8-插件注册注销与依赖)
9. [AI 编排](#9-ai-编排)
10. [架构分层](#10-架构分层)
11. [API（最小集）](#11-api最小集)
12. [里程碑](#12-里程碑)
13. [待确认项](#13-待确认项)
14. [进程模型与 App 运行](#14-进程模型与-app-运行)

---

## 1. 产品定位与设计哲学

### 1.1 一句话定位

一个面向 AI 的插件化自动化编排框架。现实是：企业应用多为固定流程，由 AI 直接驱动既低效又易产生幻觉；因此本产品不为 AI 造轮子，而是先分层沉淀不变的原语（原语、Tool、Flow 三层），基于插件自由扩展，并以 Hook 提供切面监控，让 AI 在这个框架之上安全、可控地组装出属于自己的企业应用。

### 1.2 核心哲学：机制与策略分离

这是贯穿全文的最高原则，所有具体设计都由此推导：

- **框架提供机制（Mechanism）**：原语、生命周期、调度、隔离、参数协议。
- **用户提供策略（Policy）**：哪些 Tool、如何编排、挂哪些 Hook、谁可以触发、如何做权限隔离。
- **框架不内置**：发邮件、接钉钉、多租户、审批、日志格式等任何具体业务行为。这些均由插件 + 配置实现。

### 1.3 四个角色与各自诉求

| 角色 | 获得的价值 |
|------|-----------|
| 能力开发者 | 用任意语言写脚本，框架自动包装成可调用的 Tool |
| 编排者（人或 AI） | 用声明式 DSL 组合 Tool，不关心执行细节 |
| 平台管理员 | 通过 Hook 统一注入横切控制（审计、脱敏、权限、告警） |
| 终端用户 | 用自然语言描述需求，自动变成可执行流程；Web 界面也是一个可选 Flow（`app run` 启动） |

### 1.4 非目标（明确不做什么）

- ❌ 不做通用计算平台（不是 FaaS）。
- ❌ 不内置业务行为：发邮件、接钉钉/微信的实现由用户写插件。
- ❌ 不做低代码拖拽 UI（Web 界面是插件，由用户选择是否安装）。
- ❌ 不做多租户隔离：框架不内置「租户」概念；需要隔离时用户通过 Hook + Flow 配置 + 视图插件自行实现（属行为层，非内核机制）。
- ❌ 不做「app 插件 / tool 插件」分类：只有 Flow + `app run`；UI / 调度 / Cron / Event 等服务均为长驻不退出的 Flow（见 §14）。
- ❌ 不内置 Web 管理界面：管理面以 CLI 为原生入口，Web UI 以可选 Flow 形式存在（见 §14）。
- ❌ 不让 AI 在运行时自由生成/执行代码：AI 只生成 DSL。

---

## 2. 核心原语

框架提供 5 个原语，系统中一切能力均由这 5 个组合出来：

| 原语 | 是什么 | 类比 |
|------|--------|------|
| Tool | 一个可调用能力，含输入输出 Schema | 系统调用 |
| Flow | 编排多个 Tool 的执行流程（DSL） | 进程 / 脚本 |
| Hook | 在指定生命周期节点插入执行逻辑 | 信号 / 中断 |
| Runtime | Tool 的执行环境 | 执行引擎 |
| Context | 一次执行的共享数据空间 | 进程内存 |

> **关键约束**：这 5 个原语只描述「怎么做」，不描述「做什么」；具体行为完全由插件与配置决定。

---

## 3. Tool（能力）

### 3.1 定义

一个 Tool 只声明「我能做什么」，不关心谁调用它；Tool 之间互相不知道对方存在，也不感知 Hook。

```yaml
# Tool 定义（YAML；字段与 internal/model.Tool 一致：id / runtime / entry / input_schema / output_schema）
id: http.get
name: 发送 HTTP GET 请求
runtime: process          # S1 仅实现 process；go-host/lua 计划 S7，wasm 计划 V3
entry: "sh scripts/http_get.sh"   # process runtime：input JSON 经 stdin 传入，stdout 末行 JSON 为 output
input_schema:
  type: object
  properties:
    url: { type: string }
    timeout: { type: integer }
  required: [url]
output_schema:
  type: object
  properties:
    status_code: { type: integer }
    body: { type: string }
```

### 3.2 Runtime 类型

| Runtime | 说明 | 实现状态 |
|---------|------|---------|
| go-host | Go 函数直接注册（进程内 Go 函数，经 `RegisterHostFunc` 注册） | ✅ #76 已实现（once；service 显式报错） |
| lua | Lua 脚本，沙箱执行（gopher-lua / Lua 5.1，纯 Go） | ✅ S7a 已实现（once；service 不支持） |
| process | 外部进程（py/sh/node），stdin/stdout 传 JSON | ✅ S1 已实现 |
| wasm | WASM 模块，强隔离多语言 | ❌ 后期（V3） |

> 实现状态：引擎按 `Tool.Runtime` 分发（`internal/engine/runtime.go`），未实现的 runtime 显式报错，不静默降级。每条 runtime 的状态见 `REQUIREMENTS.md`。
>
> **注册期守卫（S2 修复 G1）**：`Tool.Validate` 在**注册期**即拒绝未知 runtime（如 `foobar`）与已知但未实现的 runtime（仅 wasm 仍为 V3 deferred），避免「能入库、跑时才炸」。lua 已于 S7a 实现，可注册。
>
> **分发收敛（S2 修复 M10 剩余）**：`Runtime` 接口同时暴露 `Run`（once）与 `Start`（service 长驻），once 与 service **共用唯一分发点** `runtimeFor`；「已实现 runtime」以 `model.ImplementedRuntimes` 为单一来源，引擎不再维护第二份白名单。
>
> **lua 专项（S7a）**：
> - `entry` = **inline Lua 源码**（非文件路径）；沙箱只开 base/string/table/math，不加载 os/io/package/debug，并移除 dofile/loadfile/require/module —— **脚本无法接触宿主**（实测 `os.system` 报错）。
> - 超时/取消：`L.SetContext(ctx)`，死循环会被打断（与 process 的 `defaultExecTimeout` 对称）。**注意其边界**：`SetContext` 只在 Lua 解释循环检查取消，**阻塞中的宿主调用（`ctx.ext.ai.complete`、`ctx.time.sleep`）无法被打断**——必须等它返回才判定超时，故需长耗时的调用方应给 Tool 声明 `timeout`（#77）。
> - **不支持 service**（`Start` 显式报错，REQ-LUA-SERVICE deferred，V3 前不做）：长驻请用 process。

### 3.3 参数交互统一协议

所有 Runtime 在引擎内部统一使用 JSON 传递参数，差异被 Runtime 适配层吸收：

| Runtime | 入口 | 出口 |
|---------|------|------|
| go-host | Go struct → JSON | return struct → JSON |
| lua | `entry`=inline 源码；`ctx.input`（table） | `ctx.output`（table）或 `return` 一个 table（`ctx.output` 优先） |
| process | stdin JSON / env / argv | stdout 末行 JSON |
| wasm | 线性内存（JSON bytes） | 内存写回 + 返回地址 |

---

## 4. Flow（编排）

### 4.1 参数化（input / output，可选）

Flow 可声明可选的 **input** 与 **output**。未声明时 Flow 仅依赖内部 vars 与 trigger；声明后引擎在调度前做类型/必填/默认值校验，不执行业务逻辑。

```yaml
id: health_check          # Flow 唯一 id（对应 model.Flow.ID）
name: 健康检查
vars:
  topic: world            # 内部变量，step 内用 ${vars.topic} 引用
steps:
  - id: check
    call: hello            # 指向已注册 Tool 的 id（model.Step.Call）
    input:
      name: "${vars.topic}"
  - id: notify
    call: hello
    depends_on: [check]    # 顺序依赖；拓扑排序后于 check 执行
    input:
      name: "${steps.check.output.name}-reply"
# 说明：
# - Flow 级 input/output 声明（旧稿）当前模型未支持，已移除。
# - when 条件执行（已实现） / parallel 并行（S4 起无依赖本就并行） 见 §4.3；
#   **use_version 钉版本已实现**（S5）：钉版本解析走 `resolveToolVersion`，版本级 Entry/Schema 覆盖 Tool 级；未知版本注册期即拒绝（见 REQUIREMENTS.md REQ-USEVERSION）。
# - 模板支持 ${vars.x} / ${steps.id.output.field} / ${secrets.x}（**REQ-TMPL-SECRETS / REQ-API-SECRETS 已实现**：
#   注册期放开该前缀，运行期经宿主 secret 源解析；密钥缺失运行期报错，明文不落库）。
```

### 4.2 版本层与参数层分离

> **重要**：testing / stable 是 **Tool 版本**的属性，Flow 本身不携带 `is_testing` 标记。「测试一个 Flow」= 在调用时指定该 Flow 所引用的 Tool 走 testing 版本，而非给 Flow 打标记。Flow 默认始终走 stable。

### 4.3 依赖模型

- `depends_on`：声明顺序依赖。**S4 起：无依赖的 step 默认并行**——引擎按依赖分层（`topoLevels`），同层内并发执行（`PRD §14.5` 的并发上限 `maxParallel`，默认 4）。
  - `parallel` 字段：无依赖 step 本就并行，故该字段仅为**显式标注**、不改变语义；若某 step 已声明 `depends_on` 却仍标 `parallel`，属误导性标注，注册期拒绝。
- **依赖推断（S4）**：step 的 input 中出现 `${steps.X.…}` 即视为隐式依赖 X，**无需**再写 `depends_on`。并行下若没有推断，引用方可能抢跑到被引用方之前；推断消除了这一时序陷阱。引用不存在的 step 或自引用，注册/执行期显式报错。
- `when`：条件执行（**已实现**，用表达式引擎 `expr-lang/expr`）。
  - **写法**：`when: "steps.check.output.status_code != 200"` —— 它是**表达式，不是模板**，**不要**写 `${}`（注册期会拒绝并给出正确写法）。根名与模板一致：`vars` / `steps`。
  - **类型语义**：表达式保留类型，字符串 `"200"` **不等于**数字 `200`（`${}` 插值会丢类型，故两者不复用同一套解析）。
  - **三态语义**：条件为假 → 该 step 被**跳过**（不是失败）；Flow 照常成功，跳过项在 `RunResult.Skipped` 中可见。
  - **级联**：跳过沿**有效依赖**（含推断边）传播——上游被跳过时，下游一并跳过，而不是报「尚未执行」。
  - **注册期校验**：`when` 在 `flow register` 时即编译，语法错误当场暴露。
  - **依赖推断**：`when` 里引用的 `steps.X` 同样构成依赖（先产出、再判断条件）。
- `mode`：`once`（默认，执行完返回，走 stdin/stdout 契约）/ `service`（**S2 已实现**，拉起进程不等待，Flow 借此成为长驻「服务」）。
  - service step 不能作为其他 step 的依赖（它永不结束）；service 模式当前仅 `process` runtime 支持。
  - 含 service step 的 Flow 必须用 `app run` 启动；`flow run` 会在执行前显式拒绝（见 §14.2）。
- 变量引用前缀：`${vars.x}` / `${steps.id.output.field}`。注意：旧稿的 `${step.output.x}` / `${input.x}` 不支持（`input` 非模型字段、step 输出路径需带 step id）——以本稿为准；`${secrets.x}` **已实现**（REQ-TMPL-SECRETS + REQ-API-SECRETS：注册期放开前缀、运行期经宿主 secret 源解析、明文不落库；密钥名经 REST `/api/secrets` 管理、可 `GEAR_SECRET_*` 环境变量播种）。

### 4.4 触发方式

| 触发 | 实现 |
|------|------|
| REST | `POST /api/executions`（内核执行入口） |
| app run | `app run <flow>` 将任意 Flow 作为受管进程启动（见 §14） |
| Cron | 由「定时 Flow」实现（用 timer / event Tool 循环触发执行入口），非内核内置 |
| Event | 由「监听 Flow」实现（event Tool 等待消息后触发执行入口），非内核内置 |

> 内核触发面收敛为「执行入口」（REST + app run）。Cron / Event 等触发器本身是 Flow，不进内核。

---

## 5. Hook（切面）

### 5.1 模型

Hook 是用户配置的切面规则，由框架提供机制，插件完全无感知。一条 Hook 由四要素组成：

```yaml
hooks:
  - pointcut: before_step        # 什么时候
    target: tool="dingtalk.*"    # 对谁（flow_id / tool / step_id / "*"）
    action: mask_secrets         # 做什么（本身也是一个 Tool）
    weight: 10                   # 排序，越小越靠前
```

> **实现状态（S3 + #72）与字段名对齐**：当前代码用 `stage`（= 上面的 pointcut）、`order`（= weight）、`target`、`action`、`criticality`。
> - `stage` 现支持 `before_flow` / `before_step` / `after_step` / `on_error` / `on_success` / `before_promote`，全部已实现（`before_promote` 于 S5 落地，`before_flow`/`on_success` 于 #72 在 once 模式 Flow 上生效；service 长驻模式的 Flow 级 Hook 仍 deferred，见下）。
> - `target` 匹配维度由 stage 决定：step 级 stage 按**精确 tool id** 或 `*` 匹配；Flow 级 stage（before_flow/on_success）按 **flow id** 或 `*` 匹配（`flow_id` 已落地；step_id / 通配前缀 `dingtalk.*` 仍 deferred）。
> - `action` 指向**已注册 Tool 的 id 或 Flow 的 id**（#72 起支持 Flow 载体：同步内联执行子 Flow，同 execID 上下文、结构性无递归；before_flow 时子 Flow 结果暴露进主 Flow vars）。递归守卫拦截循环调用。
> - 实际 YAML 形态见 `gear hook register` 的示例（`examples/` 与 CLI help）。

### 5.2 生命周期与 weight 排序

同一生命周期节点上挂多个 Hook 时，按 weight 升序执行；weight 相同按注册顺序。惯例：10 = 准入/校验，50 = 业务前置，100 = 默认，900 = 兜底清理。

```
提交 Flow
  → before_flow hooks (weight 升序)
  → 调度 step
      → before_step hooks (weight 升序)
      → 执行 step
      → after_step hooks (weight 升序)
  → on_success / on_error hooks
```

| pointcut | 触发时机 | 典型用途 |
|----------|---------|---------|
| before_flow | Flow 开始执行前 | 认证、参数审计 |
| before_step | 每个 step 执行前 | 脱敏、限流、注入 |
| after_step | 每个 step 完成后 | 结果加工、指标采集 |
| on_error | step 出错时 | 告警、补偿 |
| on_success | Flow 全部成功时 | 汇总通知 |
| before_promote | Tool 晋升前 | 审批、双人复核 |

### 5.3 关键规则

- **插件无感知**：Tool 不知道自己被挂了 Hook。
- **Hook 是用户配置**，不是插件的一部分。
- **Hook 的 action 本身也是一个 Tool**，因此同样走 Schema 校验与 Runtime 隔离。
- **日志不由框架产生**：是否记录、记录到哪、什么格式，完全由日志类 Hook 决定；未挂载任何日志 Hook 时，引擎静默执行、零输出。
- **容错（fail-closed）**：Hook action 失败（崩溃或显式 abort）时主流程终止（见 §5.4）。日志 / 指标类可标 `best-effort` 例外：失败仅记 stderr、主流程继续。
- **Hook 间禁止隐式依赖**：Hook 一律由 Flow 或宿主策略显式装配，不通过「插件 A 依赖插件 B 的 Hook」这种私下约定耦合。

### 5.4 Hook 的改写与失败语义（已确认；S3 已实现）

> **实现状态（S3）**：以下语义已落地并被回归锁覆盖。补充两条实现约定：
> - **Hook 的 `action` 指向一个已注册 Tool**（process / lua 均可写 Hook 逻辑，如脱敏用 lua）；引擎直接调用其 runtime，**不经 step 流程** —— 因此「Hook 不触发 Hook」是结构性的，无需额外开关。
> - Hook 载体 Tool 的自身 schema **不做校验**（Hook 是变换函数）；被改写后的 payload 由引擎按 **step 的 Tool schema** 重校验。
> - `after_step` 对 **service step 不适用**（service 永不结束，没有「执行后」时刻）；`before_step` / `on_error` 对 service 同样生效。
> - 全部 Hook 走**同一执行路径**（`run()` 与 `Supervise()` 共用 `runOnceStep` / `startServiceStep`），所以 once 与 service 的 Hook 语义一致。

- **Hook 可改写 input / output**：before_step 可改写 step 入参（注入 / 脱敏），after_step 可改写 step 出参（结果加工）；下游 step 看到的是改写后的值。
- **改写后重校验**：before_step 改写 input 后，引擎对改写结果再跑一次该 Tool 的 `input_schema` 校验，通过才执行；不通过按校验失败处理，以保住「单一 Schema 真理源」（§9.1 不可绕过 Schema）。
- **失败即终止（fail-closed）**：任何 Hook 执行失败（崩溃或显式 abort），主流程立即终止，不继续后续 step。理由：继续运行可能不符合用户意图。
- **criticality 标记（推荐）**：默认所有 Hook 为 `critical`（失败终止）。日志 / 指标类可标 `best-effort`：失败时仅记 stderr、主流程继续，避免非关键切面拖死业务 Flow。
- **on_error 不恢复**：on_error Hook 仅做告警 / 补偿，不重试、不恢复主流程（MVP 不支持自动重试 / 恢复）。
- **Hook 嵌套**：Hook 的 action（本身是 Tool）执行期间不触发 Hook，防重入无限循环。

---

## 6. 宿主能力原语

宿主（Runtime）只向 Tool 暴露 7 个跨边界通信原语。它们处于同一抽象层级——都是「插件与外界交换数据的通道」，不混杂业务。

| 能力原语 | 调用方式 | 职责 |
|---------|---------|------|
| http | `ctx.http.Request(method, url, body?, headers?)` | 任意 HTTP 请求（传输层，钉钉/微信等均基于此） |
| call | `ctx.call(target, input)` | 调用另一个 Tool / Flow（唯一插件间通信通道） |
| db | `ctx.db.query / exec` | 结构化数据读写（配置 / 状态 / 缓存也走 db） |
| secret | `ctx.secret.get(name)` | 密钥读取（只读，框架负责解密与审计） |
| event | `ctx.event.notify / wait` | 异步事件通知 / 等待 |
| subscribe | `ctx.subscribe.start(req) / stop(handle)` | 通用订阅（事件源回调 → 重调度 Tool/Flow），长驻订阅可按归属回收 |
| config | `ctx.config.get / set / delete / list` | 持久化键值配置（落点由宿主决定，应用不必知道库在哪）；内核 AI 缺省配置 `ai.*` 也读同一个库 |

> 另有 **time / template / crypto 三个纯函数原子**（内核内建、始终可用、纯计算无外部依赖）：它们刻意**不进** capability 门禁（要求声明才可用的纯函数违背直觉），故不列在上表；分类口径与实现见 `ATOM_ROADMAP.md` §0 与 `internal/engine/purefunc.go`（REQ-PURE-FUNC）。

### 6.1 设计原则

- **只提供原语，不提供业务**：发邮件、接钉钉都是普通 Tool，内部通过 http 实现。
- **capability 精确放行**：未声明的调用一律拒绝，权限遵循最小权限原则。
- **secret 只读**：插件看不到明文存储位置，只能通过 `ctx.secret.get` 获取。
- **未声明的原语不出现在 `ctx` 上**（lua）/ 不被注入 `ctx.capabilities`（process）：防御纵深——脚本连该字段都读不到，无法探测宿主能力边界；已声明但宿主未提供实现的调用时返回明确错误，而非静默成功。
- **调用形态（M5 落地）**：lua 侧 `ctx.{http,call,db,secret,event,subscribe,config}`（http/db 为对象方法 `ctx.http.Request` / `ctx.db.query`；secret/event/subscribe/config 为子表方法 `ctx.secret.get` / `ctx.event.notify` / `ctx.subscribe.start` / `ctx.config.get`；call 是**裸函数** `ctx.call(target, input)`）；process 侧运行期以 `input.ctx.capabilities`（字符串数组）与 `input.ctx.sandbox`（bool）透传声明，供进程自检。

- **引擎默认宿主装配（#69）**：`engine.New` 未显式 `WithHost` 时自动注入 HTTP/Event/DB 原语实现与 `call` 解析器；声明了对应 capability 的 Tool 在生产环境直接可用（不再恒 `ErrCapabilityUnavailable`）。secret 默认仍无密钥源（fail-closed）；Server 经 `WithSecret` 注入 `SecretStore`（REQ-API-SECRETS）。**config 也默认不装配**（`engine.WithConfigStore`，落点 `gearhome.ConfigDBPath()`）——与 secret 同口径的 fail-closed：装配后既让 `ctx.config.*` 可用，也让 `ext.ai` 的缺省配置回退到同一个库，避免「两个入口各装配一份」导致落点漂移。

### 6.2 call 与 event 的边界

| 维度 | call | event.notify |
|------|------|-------------|
| 方向 | 同步请求-响应 | 异步通知 |
| 是否等结果 | 是 | 否（配合 event.wait 可等待） |
| 目标 | 具体 Tool / Flow | 事件名（一对多） |
| 用途 | 编排内的步骤调用 | 跨编排 / 跨系统通知 |

### 6.3 能力映射示例

- 调外部 API → `http`
- 调内部能力 → `call`
- 存结构化数据 / 配置 / 状态 → `db`
- 读密码 / token → `secret`
- 触发告警 / 审批 / Webhook → `event`
- 写日志 → 不在此列，由 Hook 决定（见 5.3）
- 定时触发 → 由「定时 Flow」实现（见 §4.4 / §14），非内核内置

---

## 7. 版本与灰度发布

### 7.1 ToolVersion 状态机

```
draft → testing → stable → deprecated
                   ↘ rejected
```

### 7.2 晋升由用户显式触发

testing → stable 的晋升（Promote）必须由 **用户显式调用**，非自动、非 AI 代操作。AI 只负责生成/校验 Flow，不持有晋升权限。晋升请求携带 `operator` 字段，写入审计记录。

```http
POST /api/tools/{tool_id}/versions/{version}/promote
{ "operator": "alice", "expected_old_version": "1.0.0" }
```

> **实现状态（S5，已收口）**：晋升可由 CLI `gear tool promote <id> <v> <target>` 与 REST `POST /api/tools/{id}/versions/{v}/promote`（body `{"target":"testing"}`）两种方式触发，均走 `engine.PromoteTool`：先跑 `before_promote` 门禁再落库。状态机语义（§7.3）已在 `model.CanPromote` + `registry.PromoteToolVersion` 落地。注：当前实现以 `{target}` 表达目标状态，**未**采纳 PRD 草拟的 `operator`/`expected_old_version` 字段（`expected_old_version` 的 CAS 预期为后续可选增强，当前版本不可变 + 写锁已提供并发安全，见 §7.3）。

### 7.3 幂等与并发安全

| 当前状态 | Promote(testing) | 重复 Promote | Promote(draft) |
|---------|-----------------|-------------|---------------|
| Testing | ✅ 成功 → Stable | ErrAlreadyStable（不报错） | ErrNotTested |
| Stable | ErrAlreadyStable | ErrAlreadyStable | — |
| Draft / Rejected | ErrNotTested | ErrNotTested | ErrNotTested |
| Deprecated | 允许重新晋升（需显式允许） | ErrDeprecated | ErrNotTested |

**并发安全三条**：

1. 注册、校验、切换 `current` 在同一把写锁内（防 TOCTOU）；
2. `current` 切换用 CAS，并发 Promote 仅一个生效；
3. ToolVersion 内容不可变，运行中 Execution 持有的版本引用不随晋升突变，晋升只影响新 Execution 的默认选择。

### 7.4 灰度选择

- stable 是默认选择；testing 版本只有显式指定（`use_version: testing`）或灰度规则命中时才被调用。
- 依赖求解（`latest` / `^x.y.z`）默认只看 stable 范围，不会把 testing 拉进来，除非显式 `allow_testing`。
- 晋升前可通过 `before_promote` Hook 挂载审批（默认调用即晋升，审批为可选扩展）。

---

## 8. 插件注册、注销与依赖

### 8.1 依赖图

只有以下三类关系进入依赖图与锁图；`Flow → Tool` 只是编排引用，不产生传递边，不会因临时选择某个版本而污染其他 Flow 的解析结果。

| 关系 | 说明 | 进依赖图 |
|------|------|---------|
| Tool → Tool | A 内部通过 call 调用 B | ✅ |
| Tool → 共享库 | Lua 模块 / WASM 组件 | ✅ |
| Flow → Tool | 编排引用 | ❌（仅选择，不传递） |

### 8.2 注册与注销

- **注册**：声明 Schema + 版本 + 依赖声明，引擎校验后写入 Registry。
- **注销**：存在以下任一情况时拒绝注销并返回明确错误——
  1. 有其他 Tool 声明依赖自己；
  2. 有 Flow 精确引用该版本；
  3. 有运行中 Execution 持有该 Tool 引用 → **立即拒绝注销**（由注入的 RunningChecker 判定；待执行结束、不再报占用后方可再次尝试。与 Flow/Hook 引用守卫同源，见 REQUIREMENTS.md REQ-DEL-TOOL-RUNNING）。
- **版本解析**：显式 SemVer 约束，最小兼容版本策略以保证可复现；冲突直接报错，不自动选择；Lock 文件记录精确版本 + digest。
- **MVP 简化**：不自动下载依赖，依赖必须已注册到本地 Registry；缺失依赖在执行前报错，不启动。

### 8.3 安全：Lock + 签名 + digest + capability

四层缺一不可：Lock 保证可复现、digest 防止内容被替换、签名确认发布者、capability 限制恶意代码在允许范围内的行为；任一缺失都会留下盲区。

---

## 9. AI 编排

> **实现形态（2026-09-14 更新，去内置化 + 三层分层）**：AI 不再是引擎内建原语，而是可插拔的 `ext.ai` 扩展原语（sidecar 载体，参考 `examples/gear-ai-sidecar`）。它严格遵循本框架的分层哲学——**能力的分层、职责的分层**，拆成三层：
>
> - **LLM 层（稳定、参数化）**：`ctx.ext.ai.complete({system, prompt, provider, model, base_url, api_key, temperature, format})`——唯一与模型对话的出口，是一个带参数的 LLM 原语；任意模型经参数注入（默认 SiliconFlow，回退 Ollama；key 走 env `GEAR_AI_SILICONFLOW_KEY` / `GEAR_AI_OLLAMA_URL`），`provider="mock"` 或 `GEAR_AI_PROVIDER=mock` 走确定性离线路径（测试用）。
> - **静态原语层（Go 编译进 sidecar，稳定不可变）**：`catalog`（BM25 式域内工具检索）、`domains`（按 namespace 归类的能力域概览）、`validate`（复用引擎 `ValidateFlowStatic` 单一真相源做静态校验）。sidecar 协议（`stdin {method,args}` → `stdout` 末行 `{result}`/`{error}`）、检索、校验均为不变部分，编译期锁定。
> - **Registry 内省原语（进程内、稳定）**：`ext.registry.tools()` / `ext.registry.flows()`——Agent **自己读引擎 registry 的真实工具/流程目录**，不再由调用方喂 `tools`（旧实现要 CLI 手工注入工具清单，否则 LLM 只能基于调用方塞进来的东西规划、必幻觉）。这是易变 agent 自我锚定的根；它与 sidecar 同属 `ext.*` 命名空间、走同一套 capabilities 白名单，只是实现载体是 Go 闭包而非子进程，由引擎内建、始终装配。
> - **Agent 层（易变、Lua 实现，不编译进 Go）**：多阶段编排——**从 `ext.registry` 读真实工具目录** → 能力域概览 → LLM 拆解步骤 → 域内检索 → 组装 Flow/Tool → 校验 → 失败回灌重来（≤N 次，默认 3）→ 人工确认/注册——**全部活在 `examples/ai-create-flow.yaml` 的 `ai-gen` Tool 的 Lua entry 里**。改 agent 行为（换提示词、调阶段、改重试）只改这份 YAML，无需重编任何 Go。
>
> 入口有二：① `gear ai gen flow|tool --intent "..." [--register]` 薄壳命令（跑参考 Flow `examples/ai-create-flow.yaml`；Agent 自行经 `ext.registry` 读真实工具目录，无需调用方喂 tools）；② 任意 Flow 内声明 `capabilities:[ext.ai, ext.registry]` 后直接 `ctx.ext.*` 调用，agent 编排本身由 Lua 实现（见参考 Flow）。**扩展装配已配置化**：`extensions.yaml`（`GEAR_EXT_CONFIG` 可覆盖路径）声明各 `ext.<name>` 的 sidecar 命令与方法，无配置则回退默认 `ext.ai`（缺省 `go run ./examples/gear-ai-sidecar`，或 `GEAR_AI_SIDECAR` 指定二进制）；这**取代原先写死在启动代码的 `Register("ext.ai", ...)`**——发布后加 `ext.tls` 等原生能力只需在 `extensions.yaml` 加一段，不必改 gear 源码。`ext.registry` 由引擎内建、始终装配。
> 能力边界不变（见 9.1）：AI 只生成 DSL（Flow/Tool 定义），绝不运行任意代码、绝不触碰 secret、绝不由自身提升/访问密钥——这些仍由引擎与宿主原语把关。
>
> **两类 Agent（避免误解「Agent 能不能直接调工具」）**：本框架的 Agent 活在 Lua 里、改行为不重编 Go，但分两种角色——
> 1. *生成器* `ai-gen`（即上面的 `ext.ai` 闭环）：按 §9 边界**只产出 DSL（Flow/Tool 定义）**，不直接执行——这是「设计态」agent。
> 2. *执行器（worker）*：一个声明了 `capabilities:[http]`（或 `call`/`db`/`ext.ai`）的 Lua Tool，运行时**直接调用宿主原语完成真实任务**——抓网页（`ctx.http`）、查库（`ctx.db`）、调 LLM（`ctx.ext.ai`）、调其它 Tool（`ctx.call`）。这是「运行态」agent，与典型 ReAct agent「直接调工具干活」是同一件事。
> 二者都证明 Gear 的 Flow/Tool 本身**就是能直接调工具做真实工作的 agent**。`examples/news-top10.yaml`（`news.fetch` 直接 `ctx.http` 抓百度热搜榜 + `ctx.json` 解析）配合 `gear serve --seed examples/news-top10.yaml` 后的 `GET /api/news`，即「执行器 agent 对外服务化」的端到端示例（详见 §11、方法论 §11 第十五次）。
>
> **`ctx.json` 引擎内建稳定原语（无条件提供）**：`decode(str)` / `encode(table)` 供所有 Lua 工具解析 / 序列化结构化文本——否则 `ctx.http.Request` 只能拿到字符串 body。它属「静态能力层」，随引擎编译、不随 agent 易变。

### 9.1 AI 的职责边界

| AI 可以做 | AI 不做 |
|----------|--------|
| 理解自然语言意图 | 运行时动态修改 Flow |
| 从 Registry 选择合适的 Tool | 直接执行系统命令 |
| 生成 Flow DSL（含 input/output 映射） | 访问 secrets |
| 显式补全直接依赖声明 | 绕过 Schema 校验 / 用 latest / 自动升级版本 |
| 候选版本选择（受 allow_testing 约束） | 修改已确认的 Flow / 持有晋升权限 |

### 9.2 编排闭环

```
用户输入自然语言
  → 能力域概览（按 namespace 自动归类，无需 LLM）
  → LLM 拆解步骤（每步：子任务描述 + 所属域）
  → 按子任务在域内检索工具（BM25，仅见 tool_id + description + Schema）
  → 组装 Flow/Tool DSL（含 input/output 映射）
  → 引擎静态校验 ValidateFlowStatic（结构 / 依赖图 / 模板引用 / Tool 存在 / when 可编译）
  → 校验失败：返回原因给 AI 修正，最多 N 次（默认 3）
  → 校验通过：人工确认 → 保存（--register）→ 等待触发
```

> **实现归属（分层）**：以上闭环现由 `examples/ai-create-flow.yaml` 的 `ai-gen` Lua entry（**Agent 层**）实现——其中「LLM 拆解 / 组装」经 `ctx.ext.ai.complete` 调 **LLM 层**、「域内检索」经 `catalog`、「校验」经 `validate`（**静态原语层**）。Go 侧只提供这四个稳定原语，不含任何 agent 编排逻辑。

### 9.3 给 AI 的上下文

只提供 Tool 的 `tool_id + description + input_schema + output_schema`，不提供实现细节。同一份 Schema 同时供 AI、引擎校验、前端表单使用，不允许漂移。

### 9.4 发布后用户自助（不改 gear 源码）

`gear` 编译发布（预编译二进制）后，用户完成下列三件事**无需任何源码改动**：

**① 配 LLM 凭证——用同一个 `gear.exe` 真调模型**
```bash
GEAR_AI_API_KEY=<key> GEAR_AI_MODEL=<model> gear ai gen tool --intent "把中文翻译为英文"
# 默认 provider=siliconflow（base https://api.siliconflow.cn/v1，GEAR_AI_BASE_URL 只需给到 /v1，代码自拼 /chat/completions）；
# GEAR_AI_PROVIDER=ollama 走本地；GEAR_AI_API_KEY/GEAR_AI_MODEL 亦可由 extensions.yaml 侧 env 注入。
# 健壮性（全部流式，不靠「拍一个够大的固定总超时」）：
#   - GEAR_AI_IDLE_TIMEOUT（秒，默认 60）：超过该时长无任何新增输出（推理增量也算活动）→ 判连接假死并报错；慢推理模型只要持续吐 token 就不会被误杀。
#   - 重复输出检测：生成文本/推理尾部同一小段（≤40 rune 的块连续 ≥8 次，或同一整行连续 ≥6 次）反复出现 → 判「重复死循环」并报错。
#   - GEAR_AI_TOTAL_TIMEOUT（秒，默认 900）：兜底总时长，防止慢速滴流永远跑不完。
# GEAR_AI_MAX_TOKENS（默认 4096）、GEAR_AI_THINKING=true|false（不设则不下发，沿用模型默认——DeepSeek 推理模型即开思考）。
```

**② 加一个全新原生能力（示例：`ext.tls`）——写配置 + 任意语言 sidecar**
```yaml
# extensions.yaml（gear 读 cwd，或用 GEAR_EXT_CONFIG 指定路径）
ext.ai:  { cmd: ["./gear-ai-sidecar.exe"], methods: [complete, catalog, domains, validate] }
ext.tls: { cmd: ["python", "tls-sidecar.py"], methods: [issue, verify] }
```
sidecar 只是「读 stdin 一行 JSON `{method,args}`、写 stdout 末行 JSON `{result|error}`」的小程序（Python/Node/任意语言皆可）；Tool 声明 `capabilities: [ext.tls]` 后，Lua 里即可 `ctx.ext.tls.issue({ domain = "x.com" })`。
⚠ **配置存在时「以配置为准」**：未列出的扩展不会注册，须把用到的都列全（含 `ext.ai`，否则 AI 能力静默消失）。`ext.registry` 由引擎内建、始终装配，无需声明。

**③ 管理 Tool / Flow——REST 契约即 UI 后端**
框架不内置 UI，但把管理做成 REST（`gear serve --addr`）：`GET/POST /api/tools`、`GET/POST /api/flows`、`DELETE /api/tools/{id}`（被引用则 409）、`POST /api/flows/{id}/validate`。UI（SPA 或长驻 Flow）即这份 API 的图形前端；Tool 的 `input_schema` 可直接驱动表单渲染（单一 Schema 真相源，无需另写）。
> 持久性：Registry 由 `--registry` 指向的库（默认 `~/.gear/registry.db`）**持久化**，故 UI 管理长期生效（执行记录另存）。

**④ 连各种数据库——内置 `db` 原语（多后端）；`ext.db` 仅作自定义 sidecar 的形态参考**
> 2026-09-19 更新：内置 `db` 原语已原生支持 `sqlite`/`mysql`/`postgres`（`internal/cap/sqldb.go`），**不再需要 `ext.db` 侧车**；原示例源码 `examples/gear-db-sidecar/` 已移除，`extensions.example.yaml` 的 `ext.db` 段改为默认注释的参考形态。
与 LLM 层完全同构：`driver`（`sqlite`/`mysql`/`postgres`）+ `dsn` 是**参数**（调用传参，或 env `GEAR_DB_DRIVER` / `GEAR_DB_DSN` 回退），而 SQL / 事务编排是**易变层**（写在 Lua Tool 里）。
```yaml
# 仅当你需要自研 driver 时：把 cmd 指向你自己的 sidecar
# ext.db: { cmd: ["./your-db-sidecar"], methods: [query, exec, ping] }
```
```lua
local r = ctx.ext.db.query({ driver = "postgres", dsn = ctx.input.dsn,
                             sql = "SELECT * FROM t WHERE city = ?", params = { ctx.input.city } })
-- r.columns / r.rows / r.row_count；写用 ctx.ext.db.exec(...) → r.rows_affected；ctx.ext.db.ping(...) 探活
```
参数走预编译占位符（`params`）天然防注入；DSN 含口令时用 `${secrets.*}` 注入，切勿硬编码。加新库（Mongo/Redis…）只需在 sidecar 里 `import` 驱动 + 加一行别名，或另写一个任意语言的 sidecar。
> 已知约束：`CommandTransport` 每次调用起一个进程 → **每次会重连数据库**（SQLite 廉价；网络库高频场景建议配连接池/代理，或后续引入长驻 sidecar 传输）。

**⑤ 两个易踩点（CLI 已加护栏）**
- **`--registry` 是 gear 的「目录库」，不是你应用的数据库**。默认 `~/.gear/registry.db`；gear 会在此文件建 `tools`/`flows`/`hooks` 三张表。若你把它指向业务库（如 `--registry ./myapp.db`），gear 会把注册表建进你的库里——CLI 检测到该文件含非 gear 表时会打印警告（不阻断）。`ext.db` 要连的业务库用 `driver`/`dsn` 传给 sidecar，**与 `--registry` 是两回事**。
- **Tool 用 `gear tool register`，Flow 用 `gear flow register`**。用错子命令时（如把 Tool YAML 交给 `flow register`），CLI 会在**打开 registry 之前**就报出可操作提示，并**不创建/污染**任何库文件。

**⑥ 内置扩展原子家族（ext.* sidecar）**：除 `ext.ai`（进程内 agent）/`ext.registry`（读引擎注册表）由内核**始终装配**外，框架内置一批开箱即用的扩展原子，权威注册表是 `plugin/extensions.yaml`（用户模板见 `extensions.example.yaml`），按用途分三类：
- **P1 通知类**：`ext.email`（send/read/list）、`ext.wecom`（send）、`ext.webhook`（send/subscribe/unsubscribe）；
- **P2 消息 / 短信类**：`ext.rabbitmq`（publish/declare/consume/unsubscribe）、`ext.kafka`（produce/consume/unsubscribe）、`ext.sms`（send）；
- **P3 文件 / 网站类**：`ext.serve`（start/stop：静态目录托管 + `proxy` 反代，类 nginx）。

载体形态有两种。**once 侧车**（`ext.email` / `ext.wecom` / `ext.sms`）经 `gear ext <name>` 拉起，按 stdio 协议（`{"method","args"}` → `{"result|error"}`）逐次调用；**常驻 service ext**（`ext.webhook` / `ext.rabbitmq` / `ext.kafka` / `ext.serve`）声明 `service:true` + `listen`（另有可选 `source` / `health_probe`），被引擎自动包成虚拟 process Tool，由 flow 的 `service` step 持有并受 Supervisor 监管，能力调用走本地 HTTP 长连接、长驻订阅经 `ctx.subscribe.start` 回推（详见 `EXT_SERVICE_DESIGN.md`）。`ext.tls` / `ext.db` 仅作自定义 sidecar 的形态参考（`ext.db` 的能力已由内置 `db` 原语覆盖），`ext.mqtt` 是 service ext 的完整参考样板。Tool 声明 `capabilities:[ext.x]` 后即可在 Lua 里 `ctx.ext.x.method({...})` 调用；它们**不是内核原语**，而是「用户装好即可用」的扩展层示例。

**协调地址的单一真相源（2026-09-21）**：service ext 的 `listen` 既决定引擎连哪个地址，也由引擎以 `COORD_ADDR=<listen>` 注入侧车进程环境——侧车不再自持一份默认端口，**改 `listen` 一处即可让两侧同源**（此前 yaml 与侧车各写一份相同字面量，改任意一处都变成「引擎连 A、侧车听 B」的静默失联）；`health_probe.addr` 省略即取 `listen`，显式写出却与 `listen` 不一致则装配期 fail-closed（不留到运行期变成监管无限重启）。

**红线（`email_test.go:TestExtAtomsMustNotReadEnv` 机械守护）**：扩展原子的凭据（user/pass/key/apikey/sign）与连接配置一律从调用参数（args）传，**禁止读 `os.Getenv`**——原子契约与环境解耦，凭据由调用方负责。（`COORD_ADDR` 是侧车自身的运行位置、由引擎按 `listen` 注入，不属此列。）

**已知调优边界（2026-09-23 已解决 → #77）**：单 step 执行超时默认 30s（`internal/engine/process.go` 的 `defaultExecTimeout`），**现已支持 Tool 级声明覆盖**：Tool 写 `timeout: "5m"`（Go duration）即用声明值，未声明仍回退 30s 防挂死。起因是多轮调用远端 LLM 的 agent 贴近该边界（首轮空 registry 触发重试时会超时）；实测更硬的反例是 mindstorm 的 `mindstorm__expand` 在 lua 里同步调 `ctx.ext.ai.complete`——AI 单次 17–20s，带 JSON 的长 prompt 必然超 30s，**100% 必失败**，且因「阻塞的宿主调用无法被 ctx 打断」（gopher-lua 只在回到解释循环时检查取消）报错时长常远超 30s，症状像 key/网络故障。agent 侧的 LLM 输出容错（如 `depends_on` 归一化）属**易变层（Lua）职责**，稳定层（Go 反序列化 / 静态校验）保持严格。

---

## 10. 架构分层

```
┌─────────────────────────────────┐
│  用户配置层                       │
│  Flow / Hook / Trigger / Secret   │
├─────────────────────────────────┤
│  编排层                           │
│  DSL 解析 / 校验 / 调度           │
├─────────────────────────────────┤
│  Tool Registry                    │
│  Schema / 版本 / 依赖图 / Lock    │
├─────────────────────────────────┤
│  宿主能力层（7 个原语）            │
│  http / call / db / secret / event / subscribe / config
│  （另有纯函数原子 time / template / crypto，始终可用）
├─────────────────────────────────┤
│  Runtime 层                       │
│  go-host / lua / process / wasm   │
└─────────────────────────────────┘

UI / 调度等服务是长驻 Flow（`app run` 启动的进程），通过 REST API 与宿主交互（见 §14）。
```

---

## 11. API（最小集）

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/api/healthz` | 健康检查（返回 tools/flows 计数） |
| GET | `/api/tools` | 列出 Tool |
| POST | `/api/tools` | 注册 Tool（注册期校验，非法 runtime → 400） |
| DELETE | `/api/tools/{id}` | 注销（被 Flow/Hook 引用 → 409；**运行中引用 → 409**，执行结束后可再次尝试） |
| POST | `/api/ai/infer` | AI 闭环（旧，REST 端点）**❌ 已移除**：AI 改为 `ext.ai` 扩展原语，不经独立 REST 端点。经 `gear ai gen` 薄壳命令或 Flow 内 `ctx.ext.ai.*` 调用（见 §9） |
| GET | `/api/flows` | 列出 Flow |
| POST | `/api/flows` | 保存 Flow（**静态校验**：依赖图/Tool 存在/when 可编译） |
| POST | `/api/flows/:id/ai-gen` | （旧，合并入 `/api/ai/infer`）**❌ 已移除**：AI 生成改由 `gear ai gen` 薄壳命令（`--register` 落库）或参考 Flow 内 `ctx.ext.ai.*` 完成 |
| POST | `/api/flows/:id/validate` | 校验 Flow（返回 `{valid, error}`） |
| POST | `/api/executions` | 触发执行（**异步**：202 + execution id） |
| GET | `/api/executions` | 列出全部执行记录 ✅ |
| GET | `/api/executions/:id` | 查询执行状态（running / succeeded / failed + RunResult） |
| GET | `/api/executions/:id/logs` | 查看日志 ✅ #71（processRuntime 把 step stdout/stderr 经 `WithLogSink` 落 `ExecStore`；红线：只含运行期输出、绝不落 input/secret 明文） |
| GET / POST | `/api/hooks` | 列出 / 添加 Hook |
| POST | `/api/apps` | 启动 Flow 为受管进程（对应 `app run`） |
| GET | `/api/apps` | 列出受管 Flow 进程（含真实监管态 `state`/重启次数 `restarts`/结束原因 `exit_err`） |
| DELETE | `/api/apps/{id}` | 停止 Flow 进程（cancel ⇒ 杀掉 service 子进程） |
| POST | `/api/tools/{id}/versions/{v}/promote` | 用户显式晋升 ✅ S5（CLI `gear tool promote <id> <v> <target>` 与 REST `POST /api/tools/{id}/versions/{v}/promote` 均已实现，均走 `engine.PromoteTool` + before_promote 门禁） |

> **实现状态（S6a+S5；AI 改 `ext.ai` 扩展，2026-09-14）**：**均已实现**（含 `GET /api/executions/{id}/logs` #71；晋升经 CLI `tool promote` 与 REST promote 端点均可触发），端到端可用（`gear serve --addr`）。另见 §11 末尾「第四种执行面：MCP」。**注意**：AI 已去内置化——不再是引擎内建原语，而是 `ext.ai` 扩展（sidecar 载体，`examples/gear-ai-sidecar`，结构化多阶段 agent）；REST 端点 `/api/ai/infer` 仍不存在（AI 不经独立端点），改由 `gear ai gen` 薄壳命令或 Flow 内 `ctx.ext.ai.*` 调用（见 §9）。`internal/ai` 旧包已删除。
> - **执行是异步的**：`POST /api/executions` 立即返回 `202 {id, status}`，结果经 `GET /api/executions/{id}` 查询（三态：running / succeeded / failed，失败带 `error`）。
> - **执行记录已持久化（#70，REQ-EXEC-PERSIST）**：`ExecStore` 改为 SQLite 持久化（`GEAR_EXEC_DB`，缺省 `build/gear.db`），重启可恢复全部执行历史与日志；`Server.Shutdown` 关闭 SQLite 句柄释放 WAL 侧车。app 句柄（运行中进程）仍存活在服务进程内、重启即丢失（属进程态，非执行记录）。
>   - **2026-09-21 注记（落点变更）**：上句里的缺省 `build/gear.db` **已作废**，现缺省 `<GEAR_HOME>/exec.db`（`GEAR_HOME` 未设时 `<HOME>/.gear/exec.db`，见 `internal/gearhome/gearhome.go`）。起因是相对 cwd 的默认落点会让同一份二进制在不同工作目录下写到不同的库——宿主派生的 gear 常带不可控 cwd，执行历史看上去像「无故丢失」。历史原文保留不改写。
> - **统一错误体**：`{"error": "..."}`；状态码语义 400 参数/校验、404 不存在、409 仍被引用。
> - 实现用标准库 `net/http`（Go 1.22+ 方法路由），未引入 web 框架——保持单二进制与最小依赖。

> **第四种执行面：MCP（#16，REQ-MCP-*）**：`gear mcp` 把目录管理 / 静态校验 / 执行能力经 Model Context Protocol 暴露给任意 MCP 客户端（Claude Desktop / WorkBuddy / Cursor…）。默认 stdio（客户端以子进程拉起，`{"mcpServers":{"gear":{"command":"gear","args":["mcp"]}}}`），`--http :port` 改走 streamable HTTP（端点 `/mcp`）。已注册 Flow 反向暴露为 `flow__{id}` 工具；复用 cliEngine，密钥源/扩展/守卫与 REST 完全一致（无「MCP 少了某个装配」的漂移）。MCP 面 19 个工具，与 REST 同源守卫（删除守卫、运行中不可注销、状态机非法迁移均复用）。

---

## 12. 里程碑

| 阶段 | 范围 | 不做 |
|------|------|------|
| V1 MVP | 单节点；**process runtime（S1）+ lua 沙箱 runtime（S7a）**（go-host 已于 #76 实现；wasm 仍 V3）；Flow 顺序执行 + input/output schema 校验 + 拓扑依赖；**`step.mode: service` 长驻 step + `app run` 监管（restart/信号停止，S2）**；**Hook 切面（before_step/after_step/on_error/before_promote，改写+重校验+fail-closed+criticality，S3+S5）**；**无依赖 step 并行 + 依赖推断 + 并发上限/背压（S4）**；**`when` 条件执行 + 三态跳过（表达式引擎）**；**REST 执行面（S6a+S5+S6c：healthz/tools/flows/hooks/executions(+列表)/apps/ai-infer，`gear serve`）**；本地 Registry + 版本声明（含钉版本 use_version 与状态机 promote）；`app run` 进程模型 + CLI 管理面（含 `exec` / `tool promote` / `ai gen`） | 无 WASM（go-host 已实现）、**Hook 仍不含 service 长驻 Flow 级（#72 起 before_flow/on_success 在 once 模式生效；target 仍仅精确 id/`*`）**、ctx.* 宿主能力已落地（M5+#69；secret 需显式注入）、**健康探针（#75）+ 跨进程并发闸门（#73）已实现；step 级 maxParallel（S4）保留**、lua 不支持 service、无自动解析下载、无分布式调度 |
| V2 | AI 编排闭环；Hook 完整（含 before_promote）；依赖自动解析；灰度发布 | 无审批默认开关、无多租户 |
| V3 | WASM runtime；event 触发；Web UI 作为可选 Flow（任意版本可 `app run`） | 无租户级隔离 |
| V4 | 分布式调度、高可用、水平扩展 | — |


## 14. 进程模型与 App 运行

### 14.1 统一模型：只有 Flow + app run

框架不存在「app 插件 / tool 插件」的分类。统一运行单元为 Flow；`app run <flow>` 将任意 Flow 变成一个受管进程，负责其完整生命周期。

- **Tool**：原子能力（含 input/output Schema），注册进 Tool Registry，作为 Flow 的内部积木。
- **Flow**：通用运行单元（声明式编排）。一个 Flow 若长驻不退出，即是一个「服务」——UI 服务、调度服务、定时触发器、事件监听器都是如此，区别仅在 Flow 内部用什么 Tool 实现。
- **`app run <flow>`**：启动器。一个 `app run` = 一个进程。并发 = 启动多个进程（如 UI 多实例、调度多副本）。

内核三件套：**引擎**（跑 step / 校验 Schema / 触发 Hook）+ **宿主能力 `ctx.*`** + **`app run` 启动器**。

### 14.2 长驻 / 阻塞 step（S2 已实现）

形态定为 **`step.mode: service`**（模型 `model.Step.Mode`，取值 `once` / `service`）：

- `once`（默认）：请求-响应，stdin JSON → stdout 末行 JSON（§3.3），执行完返回。
- `service`：引擎**拉起进程但不等待**（`engine.startProcess`），Flow 借此成为长驻「服务」。返回的进程句柄由 `app run` 监管。

守卫（F3，注册/执行期显式拒绝而非静默）：
- service step **不能作为其他 step 的依赖**——它永不结束，依赖它无意义（`Engine.Supervise` 拒绝）。
- 含 service step 的 Flow **不能用 `flow run` 一次性执行**，必须 `app run`（`Engine.RunFlow` 在执行前拒绝并提示）。
- service 模式当前**仅 `process` runtime 支持**；lua / go-host 的 service 均 `Start` 显式报错「不支持 service」（REQ-LUA-SERVICE deferred，V3 前不做；原「依赖 M5」理由已随 M5 落地失效）。长驻请用 process。

### 14.3 app run 的重启与存活（S2 已实现）

`internal/app.Supervisor` 承载 `app run` 监管循环：

- 运行 Flow 的 once 步骤，拉起 service 步骤；随后**阻塞监管**直至 ctx 取消（SIGINT/SIGTERM）或（`restart=never` 时）service 退出。
- **restart 策略**：`--restart=always`（默认，service 退出即重启）/ `--restart=never`（退出即停止监管）。重启时复用已解析并校验的 input（`Engine.Relaunch`）。
- 崩溃循环**指数退避**已实现（S4）：`restartBase=50ms` 起翻倍、上限 `restartMax=5s`；存活 ≥ `stableWindow=1s` 即重置计数，避免偶发崩溃被永久惩罚（见 REQUIREMENTS.md REQ-RESTART-BACKOFF）。
- 停止时杀掉所有 service 子进程（`Supervisor.stopAll`）。

**健康检查探针已实现（#75，REQ-APP-HEALTH）**：Tool 声明 `health_probe`（kind: http/tcp/cmd，含 addr/path/cmd/interval/timeout/threshold）；Supervisor 每 service 句柄起探针 goroutine，连续失败达阈值只杀当前进程、由既有 Relaunch 路径重启（共用崩溃退避）；Proc 读写加锁避免探针与 Relaunch/停机并发竞争；注册期 Resolve 校验配置。

### 14.4 子进程归属（已定方向）

调度类 Flow 触发其他 Flow 时，**一律通过内核执行入口（REST / `call`）**，子进程归内核持有；调度 Flow 自身只做「何时触发」的判断，不持有子进程。以此避免进程树孤儿、端口冲突、崩溃清理问题。「一个 app run 一个进程」对叶子 Flow 成立；调度 Flow 不持有被触发 Flow 的进程。

### 14.5 并发上限与进程池（S4：step 级闸门已实现；跨进程闸门 #73 已实现）

高扇出（一次性 Flow 同时上千）不能逐次 fork 进程；`app run` / 执行入口需有进程池或队列 + 并发上限，超出进入排队或拒绝。

**已实现（S4）**：
- **step 级并发闸门**：单个 Flow 内同时执行的 step 数上限 `maxParallel`，默认 **4**（`engine.DefaultMaxParallel`）；`WithMaxParallel(n)` 或 CLI `flow run --max-parallel=N` 可调，`1` 退化为串行（用于需要严格顺序或怕并发副作用的场景）。
- **分层并行**：引擎按依赖分层，同层内无依赖 step 并发；配合依赖推断（§4.3）保证引用时序。
- **背压语义**：并发槽占满时后续 step 排队等待；同层某 step 失败即取消本层未启动者，后续层不执行（fail-closed）。
- `app run --max=N`：`Supervisor.maxConcurrent`，限制一个 `app run` 同时监管的 Flow 数。

**已实现（#73，REQ-CONCURRENCY-CROSS）**：`internal/server/gate.go` 的 `ConcurrencyGate` 固定 worker 池（= 最大并发执行数）+ 有界队列；worker 全忙且队列满时 `Submit` 返回 false → 入口拒绝（背压）回 `429`、执行记录置 `StatusRejected`（区别于 running/succeeded/failed，可查询）；`Server.New` 默认 64 worker / 256 队列，支持 `WithMaxConcurrent` / `WithExecQueueSize`。区别于 S4 step 级 `maxParallel`（限单 Flow 内 step 并发）与 S2 `app run --max`（限 Flow 数）。

> 注意：**service 长驻进程不绑层级上下文**。层级结束会 `cancel()`，若把 service 子进程绑在该 ctx 上会被立刻杀掉；实现上在拉起前响应取消、拉起后脱离层级 ctx。

### 14.6 CLI 管理面

CLI 是 V1 永久原生管理面（不同用户对 UI 需求不同，UI 以可选 Flow 形式存在，不内置）。命令树为 REST API 的薄客户端：

| 命令 | 说明 |
|------|------|
| `gear tool register / unregister / list / show / versions` | Tool 注册 / 注销 / 列举 / 查看 / 版本（`register` 路径写 `-` 表示从 stdin 读 YAML；**支持 `---` 分隔的多文档，一次注册全部文档**，要么全成功要么全不写） |
| `gear tool run <id>` | **一次性执行单个 Tool**（语义同「Flow 里跑一个 step」：input_schema 校验 → capability 门禁 → runtime 执行 → step 级 Hook → output_schema 校验）。输入四选一：`--input '<json>'` / `--input-file <path>` / `--arg k=v`（可重复，值按 JSON 解析）/ **stdin 管道** |
| `gear tool promote <id> <version> <target>` | 显式晋升（触发 `before_promote` 门禁，fail-closed；target ∈ draft/testing/stable/deprecated/rejected） |
| `gear flow register / list / inspect / validate` | Flow 注册 / 列举 / 查看 / 静态校验（仅校验不注册；`register`/`validate` 路径写 `-` 表示从 stdin 读 YAML；**`register` 支持多文档**，同样全成功或全不写） |
| `gear flow run <id> [--var k=v ...] [--input '<json>'] [--max-parallel=N]` | 执行 Flow。**`--var`/`--input` 运行时覆盖 Flow 变量**（覆盖 Flow 声明的默认值；只作用于本次运行、**不写回 registry**，仍会被 `before_flow` Hook 进一步改写）；也可从 **stdin 管道**读 vars |
| `gear hook register - / list / show / remove` | Hook 配置（`register` 路径写 `-` 表示从 stdin 读 YAML）。注：Hook **无独立执行命令**——它只在 Flow/step 执行时被触发 |
| `gear scaffold tool\|flow\|hook [name] [-o <file>] [--runtime process\|lua\|go-host] [--force]` | 一键生成「可直接 register」的 Tool/Flow/Hook YAML 骨架（文件内**就地标注每个字段的用途、取值与示例**；`gear scaffold tool --help` 等子命令内嵌完整字段表/取值）。默认打印 stdout，`-o` 落盘，`-f` 覆盖已存在文件。「不知 YAML 怎么写」时替代手改示例的最快路径 |
| `gear app run <flow>` | 把 Flow 作为受管进程启动（`mode: service` 长驻 step 用；`flow run` 遇 service step 会拒绝并提示改用它） |
| `gear exec <flow>` | 触发 REST 执行面执行（`--addr` 指定执行面） |
| `gear serve [--seed <yaml>] [--addr]` | 启动 REST 执行面（管理契约供 UI/程序调用，见 §11）。`--seed` 可灌入**含 Tool 与 Flow 混合**的多文档文件（按 Tool → Flow 顺序落库，因 Flow 静态校验需其 call 目标已存在）；任一对象失败则**回滚**本次全部写入 |
| `gear ai gen flow\|tool --intent "..." [--register]` | AI 生成 Flow/Tool 草稿（`ext.ai`，见 §9）；同 `ai` 命令另含 `react`（见下）与 `capabilities` |
| `gear ai react "<task>" [--max-steps N] [--allow http,secret] [--model/--provider/--base-url/--api-key]` | 跑 ReAct 智能体（LLM 逐步调内置/已注册工具完成任务，需联网 LLM；等价于 Tool entry 里 `ctx.ext.ai.react`） |
| `gear ai capabilities` | 打印当前装配的原子能力目录（内核 builtin + pure 原语 + 已注册 ext.*，与 agent 提示词同源，随 extensions 配置动态变化） |
| `gear ext <name>` | 运行内置扩展原语的 once 载体（email / sms / wecom / webhook / rabbitmq / kafka；`echo '{"method":"...","args":{...}}' \| gear ext email` 可调试）。service ext（webhook/rabbitmq/kafka/serve）的常规形态是常驻侧车 + 本地 HTTP，见 §9⑥ |
| `gear mcp [--http :port]` | 以 MCP 执行面暴露 Gear（stdio 默认；`--http` 改 streamable HTTP，端点 `/mcp`），见 §11 第四种执行面 |

**统一输入约定（Unix 管道直觉，`internal/cli/input.go`）**——JSON 入参与 YAML 文件都吃管道：

```bash
cat vars.json      | gear flow run demo-db-people          # 自动读 stdin 当 vars
echo '{"domain":"x.com"}' | gear tool run tls.issue          # 经典管道
gear tool run db.people --input - < in.json                  # 显式声明从 stdin 读
cat demo-db.yaml   | gear flow register -                    # YAML 也从 stdin 读
```
规则：① 给了显式标志（`--input`/`--input-file`/`--arg`/`--var`）→ **用标志、不读 stdin**（若检测到管道仍存在会打 warning，不静默）；② `-` 显式表示 stdin；③ 未给标志且 stdin 是管道 → 自动读；④ 都没有 → 空输入（合法性由 `input_schema` 说了算）；⑤ 显式标志之间互斥，混用即报错。终端下**绝不阻塞等待输入**。
> Hook 无独立执行命令（只在 Flow/step 执行时触发），故 JSON 入参管道对 hook 不适用；hook 需要的是「注册时从 stdin 读 YAML」，已支持。
