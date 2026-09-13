# 插件化自动化编排框架 · PRD

> **版本**：v4.0（最终版）
> **日期**：2026-09-12
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

一个自动化操作系统的内核。框架只提供原语（机制），不内置任何具体业务行为（策略）；所有业务能力——包括 Web 管理界面——都以 Flow 形式存在（`app run` 启动），由用户通过配置组装出自己的自动化系统。

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
| go-host | Go 函数直接注册 | ⏳ 计划 S7 后续 |
| lua | Lua 脚本，沙箱执行（gopher-lua / Lua 5.1，纯 Go） | ✅ S7a 已实现（once；service 不支持） |
| process | 外部进程（py/sh/node），stdin/stdout 传 JSON | ✅ S1 已实现 |
| wasm | WASM 模块，强隔离多语言 | ❌ 后期（V3） |

> 实现状态：引擎按 `Tool.Runtime` 分发（`internal/engine/runtime.go`），未实现的 runtime 显式报错，不静默降级。每条 runtime 的状态见 `REQUIREMENTS.md`。
>
> **注册期守卫（S2 修复 G1）**：`Tool.Validate` 在**注册期**即拒绝未知 runtime（如 `foobar`）与已知但未实现的 runtime（go-host/wasm），避免「能入库、跑时才炸」。lua 已于 S7a 实现，可注册。
>
> **分发收敛（S2 修复 M10 剩余）**：`Runtime` 接口同时暴露 `Run`（once）与 `Start`（service 长驻），once 与 service **共用唯一分发点** `runtimeFor`；「已实现 runtime」以 `model.ImplementedRuntimes` 为单一来源，引擎不再维护第二份白名单。
>
> **lua 专项（S7a）**：
> - `entry` = **inline Lua 源码**（非文件路径）；沙箱只开 base/string/table/math，不加载 os/io/package/debug，并移除 dofile/loadfile/require/module —— **脚本无法接触宿主**（实测 `os.system` 报错）。
> - 超时/取消：`L.SetContext(ctx)`，死循环会被打断（与 process 的 `defaultExecTimeout` 对称）。
> - **不支持 service**（`Start` 显式报错）：长驻需 `ctx.*` 宿主能力（M5），否则 lua 服务只能空转。

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
#   **use_version 钉版本仍未实现**（计划 S5），含该字段的 Flow 注册期即显式拒绝（见 REQUIREMENTS.md）。
# - 模板支持 ${vars.x} / ${steps.id.output.field}；**${secrets.x} 尚未实现**
#   （secret 原语随 ctx.* 落地，见 REQ-TMPL-SECRETS），注册期显式拒绝而非执行期才报错。
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
- 变量引用前缀：`${vars.x}` / `${steps.id.output.field}`。注意：旧稿的 `${step.output.x}` / `${input.x}` 不支持（`input` 非模型字段、step 输出路径需带 step id）——以本稿为准；`${secrets.x}` 当前**未实现**（secret 原语随 ctx.* 落地，注册期显式拒绝，见 REQUIREMENTS.md REQ-TMPL-SECRETS）。

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

> **实现状态（S3）与字段名对齐**：当前代码用 `stage`（= 上面的 pointcut）、`order`（= weight）、`target`、`action`、`criticality`。
> - `stage` 现仅支持 `before_step` / `after_step` / `on_error`；`before_flow`、`on_success`、`before_promote` **尚未实现**（`before_promote` 计划 S5，注册期会被显式拒绝）。
> - `target` 现支持**精确 tool id** 或 `*`；`flow_id` / step_id / 通配前缀（`dingtalk.*`）尚未实现。
> - `action` 指向**已注册 Tool 的 id**（PRD 原意「Flow/Tool」，Flow 载体待评估，见 REQUIREMENTS.md REQ-HOOK-ACTION-FLOW）。
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

宿主（Runtime）只向 Tool 暴露 6 个跨边界通信原语。它们处于同一抽象层级——都是「插件与外界交换数据的通道」，不混杂业务。

| 能力原语 | 调用方式 | 职责 |
|---------|---------|------|
| http | `ctx.http.request(...)` | 任意 HTTP 请求（传输层，钉钉/微信等均基于此） |
| call | `ctx.call(target, input)` | 调用另一个 Tool / Flow（唯一插件间通信通道） |
| db | `ctx.db.query / exec` | 结构化数据读写 |
| kv | `ctx.kv.get / put / delete` | 简单键值存储（配置 / 状态 / 缓存） |
| secret | `ctx.secret.get(name)` | 密钥读取（只读，框架负责解密与审计） |
| event | `ctx.event.notify / wait` | 异步事件通知 / 等待 |

### 6.1 设计原则

- **只提供原语，不提供业务**：发邮件、接钉钉都是普通 Tool，内部通过 http 实现。
- **capability 精确放行**：未声明的调用一律拒绝，权限遵循最小权限原则。
- **secret 只读**：插件看不到明文存储位置，只能通过 `ctx.secret.get` 获取。

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
- 存结构化数据 → `db`；存配置/状态 → `kv`
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
  → 意图解析
  → 从 Registry 匹配 Tool（仅见 tool_id + description + Schema）
  → 生成 Flow DSL（含 input/output）
  → 引擎静态校验（tool_id 存在 / Schema 匹配 / depends_on 无环 / 变量路径合法）
  → 校验失败：返回原因给 AI 修正，最多 3 次
  → 校验通过：人工确认 → 保存 → 等待触发
```

### 9.3 给 AI 的上下文

只提供 Tool 的 `tool_id + description + input_schema + output_schema`，不提供实现细节。同一份 Schema 同时供 AI、引擎校验、前端表单使用，不允许漂移。

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
│  宿主能力层（6 个原语）            │
│  http / call / db / kv / secret / event
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
| POST | `/api/tools/.../ai-infer` | AI 从脚本推断 Schema ⏳ S6c |
| GET | `/api/flows` | 列出 Flow |
| POST | `/api/flows` | 保存 Flow（**静态校验**：依赖图/Tool 存在/when 可编译） |
| POST | `/api/flows/:id/ai-gen` | AI 生成 Flow ⏳ S6c |
| POST | `/api/flows/:id/validate` | 校验 Flow（返回 `{valid, error}`） |
| POST | `/api/executions` | 触发执行（**异步**：202 + execution id） |
| GET | `/api/executions/:id` | 查询执行状态（running / succeeded / failed + RunResult） |
| GET | `/api/executions/:id/logs` | 查看日志 ⏳ 需日志 Hook 产出 |
| GET / POST | `/api/hooks` | 列出 / 添加 Hook |
| POST | `/api/apps` | 启动 Flow 为受管进程（对应 `app run`） |
| GET | `/api/apps` | 列出运行中的 Flow 进程 |
| DELETE | `/api/apps/{id}` | 停止 Flow 进程（cancel ⇒ 杀掉 service 子进程） |
| POST | `/api/tools/{id}/versions/{v}/promote` | 用户显式晋升 ⏳ S5（版本状态机） |

> **实现状态（S6a）**：除标 ⏳ 的四项外均已实现，端到端可用（`gear serve --addr`）。
> - **执行是异步的**：`POST /api/executions` 立即返回 `202 {id, status}`，结果经 `GET /api/executions/{id}` 查询（三态：running / succeeded / failed，失败带 `error`）。
> - **状态是内存态**（显式声明）：执行记录与 app 句柄都存活在服务进程内，**进程重启即丢失**。持久化执行历史属后续切片，勿当作已持久化。
> - **统一错误体**：`{"error": "..."}`；状态码语义 400 参数/校验、404 不存在、409 仍被引用。
> - 实现用标准库 `net/http`（Go 1.22+ 方法路由），未引入 web 框架——保持单二进制与最小依赖。

---

## 12. 里程碑

| 阶段 | 范围 | 不做 |
|------|------|------|
| V1 MVP | 单节点；**process runtime（S1）+ lua 沙箱 runtime（S7a）**（go-host 计划后续）；Flow 顺序执行 + input/output schema 校验 + 拓扑依赖；**`step.mode: service` 长驻 step + `app run` 监管（restart/信号停止，S2）**；**Hook 切面（before_step/after_step/on_error，改写+重校验+fail-closed+criticality，S3）**；**无依赖 step 并行 + 依赖推断 + 并发上限/背压（S4）**；**`when` 条件执行 + 三态跳过（表达式引擎）**；**REST 执行面（S6a：healthz/tools/flows/hooks/executions/apps，`gear serve`）**；本地 Registry + 版本声明；`app run` 进程模型 + CLI 管理面 | 无 WASM、无 go-host、无钉版本（S5）、**REST 四端点待做（promote/ai-infer/ai-gen/logs）**、**Hook 仅 before_step/after_step/on_error（before_promote 属 S5；target 仅精确 id 或 `*`）**、无 ctx.* 宿主能力（M5）、**无健康探针 / 队列背压（S2+/S4）**、lua 不支持 service、无自动解析下载、无分布式调度 |
| V2 | AI 编排闭环；Hook 完整（含 before_promote）；依赖自动解析；灰度发布 | 无审批默认开关、无多租户 |
| V3 | WASM runtime；event 触发；Web UI 作为可选 Flow（任意版本可 `app run`） | 无租户级隔离 |
| V4 | 分布式调度、高可用、水平扩展 | — |

---

## 13. 待确认项

| # | 问题 | 当前默认 |
|---|------|---------|
| 1 | 同 tool_id 允许多主版本共存吗？ | 默认禁止，仅 side-by-side SDK 显式允许 |
| 2 | 0.x 版本是精确 pin 还是项目级 opt-in？ | MVP 强制精确 pin |
| 3 | 系统包（git、libxml2）由谁管理？ | 只声明前提并验证，不自动安装 |
| 4 | 灰度粒度：全局切换 vs 按调用方细粒度？ | 调用方级显式选择 |
| 5 | 晋升是否需要审批前置（双人复核）？ | 默认调用即晋升，可通过 before_promote Hook 扩展 |
| 6 | 测试版运行结果是否独立审计？ | 同表记录，仅标记 stage |
| 7 | CAS 是否要求严格预期版本匹配？ | 宽松策略（见 §7.3） |
| 8 | Web UI 插件是否纳入首批 MVP？ | 否；UI 为可选 Flow，管理面靠 CLI（见 §14） |
| 9 | 长驻 / 阻塞 step 的形态（daemon 标记 or server Tool）？ | **已定（S2）**：`step.mode: service`（§14.2） |
| 10 | `app run` 默认 restart 策略与健康检查机制？ | restart **已定（S2）**：默认 `always`，`--restart=never` 可关；健康检查**待定**（当前仅存活，探针 deferred） |
| 11 | 执行入口的并发上限 / 进程池大小？ | **step 级已定（S4）**：`maxParallel` 默认 4（`--max-parallel` 可调，`app run --max` 管 Flow 数）；**跨进程统一队列/进程池仍待定**（§14.5，REQ-CONCURRENCY-CROSS） |
| 12 | CLI 如何鉴权到 daemon、是否走同一套 capability？ | 待定 |
| 13 | Hook 默认 criticality（日志类是否默认 best-effort）？ | 建议日志类 best-effort，待定（§5.4） |
| 14 | 安全：process / go-host 绕过 capability、secret 经 argv 的处理？ | 暂搁置（deferred） |

---

> **开发建议**：按 Tool Schema + Registry → Flow DSL 引擎 → Runtime 适配层（先 go-host + process）→ Hook + 灰度晋升 的顺序推进，每步可独立验证。

---

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
- service 模式当前**仅 `process` runtime 支持**；lua 的 service 依赖 `ctx.*` 宿主能力落地（M5，架构债），当前 `Start` 显式报错「不支持 service」（见 REQUIREMENTS.md REQ-LUA-SERVICE）；go-host 的 service 随 go-host runtime（S7）。

### 14.3 app run 的重启与存活（S2 已实现）

`internal/app.Supervisor` 承载 `app run` 监管循环：

- 运行 Flow 的 once 步骤，拉起 service 步骤；随后**阻塞监管**直至 ctx 取消（SIGINT/SIGTERM）或（`restart=never` 时）service 退出。
- **restart 策略**：`--restart=always`（默认，service 退出即重启）/ `--restart=never`（退出即停止监管）。重启时复用已解析并校验的 input（`Engine.Relaunch`）。
- 崩溃循环**指数退避**已实现（S4）：`restartBase=50ms` 起翻倍、上限 `restartMax=5s`；存活 ≥ `stableWindow=1s` 即重置计数，避免偶发崩溃被永久惩罚（见 REQUIREMENTS.md REQ-RESTART-BACKOFF）。
- 停止时杀掉所有 service 子进程（`Supervisor.stopAll`）。

仍 deferred：**健康检查探针**（HTTP / 命令探针）。当前仅以「进程是否退出」判活；主动探针未实现（见 REQUIREMENTS.md REQ-APP-HEALTH）。

### 14.4 子进程归属（已定方向）

调度类 Flow 触发其他 Flow 时，**一律通过内核执行入口（REST / `call`）**，子进程归内核持有；调度 Flow 自身只做「何时触发」的判断，不持有子进程。以此避免进程树孤儿、端口冲突、崩溃清理问题。「一个 app run 一个进程」对叶子 Flow 成立；调度 Flow 不持有被触发 Flow 的进程。

### 14.5 并发上限与进程池（S4：step 级闸门已实现；跨进程池待定）

高扇出（一次性 Flow 同时上千）不能逐次 fork 进程；`app run` / 执行入口需有进程池或队列 + 并发上限，超出进入排队或拒绝。

**已实现（S4）**：
- **step 级并发闸门**：单个 Flow 内同时执行的 step 数上限 `maxParallel`，默认 **4**（`engine.DefaultMaxParallel`）；`WithMaxParallel(n)` 或 CLI `flow run --max-parallel=N` 可调，`1` 退化为串行（用于需要严格顺序或怕并发副作用的场景）。
- **分层并行**：引擎按依赖分层，同层内无依赖 step 并发；配合依赖推断（§4.3）保证引用时序。
- **背压语义**：并发槽占满时后续 step 排队等待；同层某 step 失败即取消本层未启动者，后续层不执行（fail-closed）。
- `app run --max=N`：`Supervisor.maxConcurrent`，限制一个 `app run` 同时监管的 Flow 数。

**仍未实现**：跨进程/执行入口级别的**统一队列与进程池**（多 `app run` 之间的全局配额、排队拒绝策略）——见 `REQUIREMENTS.md` REQ-CONCURRENCY-CROSS。

> 注意：**service 长驻进程不绑层级上下文**。层级结束会 `cancel()`，若把 service 子进程绑在该 ctx 上会被立刻杀掉；实现上在拉起前响应取消、拉起后脱离层级 ctx。

### 14.6 CLI 管理面

CLI 是 V1 永久原生管理面（不同用户对 UI 需求不同，UI 以可选 Flow 形式存在，不内置）。命令树为 REST API 的薄客户端：

| 命令 | 说明 |
|------|------|
| `gear tool register / unregister / inspect` | Tool 注册 / 注销 / 查看 |
| `gear tool promote --operator` | 显式晋升 testing→stable（写审计） |
| `gear flow save / validate / ai-gen / inspect` | Flow 保存 / 校验 / AI 生成 / 查看 |
| `gear hook add / list / remove` | Hook 配置 |
| `gear app run / ls / stop <flow>` | 启动 / 列出 / 停止 Flow 进程 |
| `gear exec trigger / status / logs` | 触发 / 查状态 / 查日志 |
| `gear secret set / get` | 密钥管理 |
