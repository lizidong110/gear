# Gear · 插件化自动化编排框架

> 一个「自动化操作系统的内核」：框架只提供原语（机制），不内置任何具体业务行为（策略）。所有业务能力（含 Web 管理界面）都以 **Flow** 形式存在，由你通过配置组装。

- **语言 / 内核**：Go（单二进制 `gear`，CLI + REST + MCP 三种执行面（MCP 默认 stdio、可选 `--http` streamable），无外部 Web 框架，REST 用标准库 `net/http` 的 Go 1.22+ 路由）
- **持久化**：SQLite（注册表 `tools/flows/hooks` 三表 + 执行记录库），纯 Go 实现（`modernc.org/sqlite`，无 cgo）
- **模块**：`go.mod` module 名为 `gear`，Go 1.27.0

---

## 目录

1. [核心概念](#1-核心概念)
2. [架构分层](#2-架构分层)
3. [核心数据模型](#3-核心数据模型)
4. [CLI 接口](#4-cli-接口)
5. [REST API 接口](#5-rest-api-接口)
6. [扩展与插件机制](#6-扩展与插件机制)
7. [Web 管理端（web-ui 插件）](#7-web-管理端web-ui-插件)
8. [配置 Schema 速查](#8-配置-schema-速查)
9. [快速开始](#9-快速开始)
10. [构建与质量闸门](#10-构建与质量闸门)
11. [文档索引](#11-文档索引)
12. [MCP 执行面](#12-mcp-执行面)

---

## 1. 核心概念

Gear 的魔法是「**机制与策略分离**」：框架提供原语 / 生命周期 / 调度 / 隔离 / 参数协议；你提供策略（哪些 Tool、如何编排、挂哪些 Hook、如何隔离）。发邮件、接钉钉、多租户、审批、日志格式等**均不内置**，由插件 + 配置实现。

5 个核心原语：

| 原语 | 语义 |
|---|---|
| **Tool** | 可调用能力，含 input/output Schema，彼此无感知、不感知 Hook |
| **Flow** | 编排多个 Tool 的声明式 DSL |
| **Hook** | 在指定生命周期节点插入执行逻辑的「切面」 |
| **Runtime** | Tool 的执行环境（process / lua / go-host / wasm） |
| **Context** | 一次执行的共享数据空间（宿主原语：受门禁的 http/call/db/secret/event/subscribe/config + 始终可用的纯函数原子 time/template/crypto/id/validate） |

---

## 2. 架构分层

```
┌──────────────────────────────────────────────────────────┐
│  CLI (cobra)  │  REST (net/http)  │  MCP (stdio / --http)  │  ← 三种执行面
├──────────────────────────────────────────────────────────┤
│  internal/cli           │  internal/server                │
│  (管理面 / 客户端)        │  (执行面: 并发闸门 + 执行记录)      │
├──────────────────────────────────────────────────────────┤
│  internal/mcp  —— MCP 执行面：与 CLI/REST 同构的 registry+引擎装配， │
│                  协议翻译层（13 个意图级 gear_* 工具 +               │
│                  Instructions 心智模型 + 可选 flow__{id} 反向暴露）    │
├──────────────────────────────────────────────────────────┤
│  internal/engine   —— 编排内核：拓扑执行 / Hook 装配 /        │
│                       runtime 分发 / 模板 / when 表达式 /    │
│                       版本灰度 / AI 扩展装配 / 健康检查       │
├──────────────────────────────────────────────────────────┤
│  internal/cap      —— 宿主能力：受门禁 7 个原语              │
│                  http/call/db/secret/event/subscribe/config  │
│                       另有纯函数原子（5 个）                   │
│                     time/template/crypto/id/validate           │
│                       始终可用、无需声明 capabilities          │
├──────────────────────────────────────────────────────────┤
│  internal/model    —— 唯一真相源：Tool/Flow/Hook/Step        │
│                       + 注册期校验（PRD §8 守卫）            │
├──────────────────────────────────────────────────────────┤
│  internal/registry —— SQLite 持久化（JSON blob 落库）        │
│  internal/app      —— app run 监管器（service 长驻+重启）     │
└──────────────────────────────────────────────────────────┘
```

`internal/` 子包职责一览：

| 包 | 职责 |
|---|---|
| `model` | 核心数据模型唯一真相源 + 校验规则 |
| `registry` | SQLite 持久化 tool/flow/hook，含删除守卫与示例 YAML 多文档加载 |
| `engine` | 编排内核：Flow 拓扑执行、Hook 装配、runtime 分发、表达式、版本灰度 |
| `cap` | 宿主能力层：**7 个受 capability 门禁的原语**（http/call/db/secret/event/subscribe/config）的接口与内存实现（最小权限门禁、事件总线、订阅生命周期、键值配置库）。纯函数原子（time/template/crypto/id/validate）刻意不进此门禁，见 `internal/engine/purefunc.go` |
| `server` | REST 执行面：路由、并发闸门、执行记录持久化、受管 app 托管 |
| `app` | `app run` 监管器：把一个 / 多个 Flow 变成受管进程 |
| `cli` | CLI 管理面：cobra 命令树、`gear`/`exec`/`ai`/`mcp` 客户端、extensions 装配 |
| `mcp` | MCP 执行面：13 个**意图级** `gear_*` 工具（手册 / 目录 / 校验 / 落库 / 能力直调 / 执行 / 应用生命周期 / 环境自检）+ 经 `ServerOptions.Instructions` 注入的心智模型；`--expose-flows` 才把已注册 Flow 反向暴露为 `flow__{id}` |
| `execstore` | 执行记录持久化（SQLite），REST 与 MCP 两执行面共用同一份执行历史与「运行中不可注销」判据 |
| `gearhome` | **gear 自身落盘位置的单一真相源**：`GEAR_HOME` → `<HOME>/.gear` → `<exeDir>/.gear` 兜底，决定 registry.db / exec.db 路径与诊断时的产物搜索锚点（见 `internal/gearhome/gearhome.go`）。**默认路径一律不随 cwd 变化**——这是「同一份 gear 在不同工作目录下行为不一致」的根治点 |

---

## 3. 核心数据模型

定义文件：`internal/model/model.go`；校验：`internal/model/validate.go`。

### 3.1 Tool（原子能力）

```go
type Tool struct {
    ID           string                 `yaml:"id" json:"id"`
    Name         string                 `yaml:"name" json:"name"`
    Description  string                 `yaml:"description" json:"description"`
    Runtime      Runtime                `yaml:"runtime" json:"runtime"`        // process | lua | go-host | wasm
    Entry        string                 `yaml:"entry" json:"entry"`            // process: 命令；lua/go-host: 函数引用
    InputSchema  map[string]interface{} `yaml:"input_schema" json:"input_schema"`
    OutputSchema map[string]interface{} `yaml:"output_schema" json:"output_schema"`
    Versions     []ToolVersion          `yaml:"versions" json:"versions"`
    Capabilities []string               `yaml:"capabilities" json:"capabilities"` // 声明宿主原语（最小权限）
    HealthProbe  *HealthProbe           `yaml:"health_probe,omitempty" json:"health_probe,omitempty"`
    Timeout      string                 `yaml:"timeout,omitempty" json:"timeout,omitempty"` // 单次执行超时（缺省 30s）
    API          *APIRoute              `yaml:"api,omitempty" json:"api,omitempty"`         // 可选：声明式 REST 路由（FR-3）
}
```

- **`api`（声明式 REST 路由）**：`{method: GET, path: /api/orders/{id}}` 让这个 Tool **额外**成为一条
  资源路由——`{id}` 就是 Tool 的输入字段，响应体即 Tool 输出（不包 `{output}`）。
  `POST /api/tools/{id}/run` 照旧可用（**加挂而非替换**，AC-C4）。**不鉴权**：gear 没有用户库，
  授权是业务的事，请给这个 Tool 挂鉴权 Hook。未声明 `api` 的 Tool 行为一字不变；
  两个 Tool 撞同一 `method`+`path` 时 `gear_validate` / `gear_apply`（含 dry_run）整包拒绝，
  跨 bundle 的第二个由 registry 入库前拒（同名 pattern 会 panic）。

- **Runtime 实现状态**：`process` / `lua` / `go-host` 已实现；`wasm` 已知未实现。**注册期守卫**：未知 / 未实现的 runtime 注册即拒，不静默降级。
- **ToolVersion 状态机**：`draft → testing → stable → deprecated → rejected`，非法迁移被 `CanPromote` 拒绝。
- **统一参数协议（Tool）**：`stdin JSON → stdout 末行 JSON`。
- **HealthProbe**：`kind`(http|tcp|cmd) + `addr`/`path`/`cmd` + `interval`(5s)/`timeout`(2s)/`threshold`(3)，用于 service 模式探针。
- **Timeout**（单次执行超时）：Go duration 字符串（如 `5m`/`300s`）。缺省 = 引擎默认 `defaultExecTimeout`(30s)，保留「防止挂死工具占住执行线程」的默认语义。**AI 补全、长任务等天然长耗时的 Tool 必须显式声明**，否则会被默认值误杀——典型症状是 lua 里调用阻塞的 `ctx.ext.ai.complete`，AI 还没返回就撞上默认闸门，报 `lua 执行被中断: context deadline exceeded`。注册期即解析校验，非法时长拒绝注册。

### 3.2 Flow（通用运行单元 / 服务）

```go
type Flow struct {
    ID          string                 `yaml:"id" json:"id"`
    Name        string                 `yaml:"name" json:"name"`
    Description string                 `yaml:"description" json:"description"`
    Vars        map[string]interface{} `yaml:"vars" json:"vars"`
    Steps       []Step                 `yaml:"steps" json:"steps"`
}

type Step struct {
    ID         string                 `yaml:"id" json:"id"`
    Call       string                 `yaml:"call" json:"call"`          // 被调用 Tool 的 id
    UseVersion string                 `yaml:"use_version" json:"use_version"` // 钉版本
    Input      map[string]interface{} `yaml:"input" json:"input"`
    DependsOn  []string               `yaml:"depends_on" json:"depends_on"`
    When       string                 `yaml:"when" json:"when"`          // 条件执行（表达式，非 ${} 模板）
    Parallel   bool                   `yaml:"parallel" json:"parallel"`  // 显式并行
    Mode       string                 `yaml:"mode" json:"mode"`          // once(默认) | service(长驻)
}
```

关键点：
- **依赖推断**：input 里出现 `${steps.X.…}` 即隐式依赖，无需写 `depends_on`。
- **并行**：无依赖 step 默认并行（`maxParallel` 默认 4）。
- **when 条件执行**：用表达式引擎（非模板），三态 skip（跳过 ≠ 失败）。
- **变量引用**：`${vars.x}` / `${steps.id.output.field}`。
- **service 模式**：`mode: service` 长驻 step 必须 `gear app run`，`flow run` 会显式拒绝。

### 3.3 Hook（横切控制）

```go
type Hook struct {
    ID          string           `yaml:"id" json:"id"`
    Target      string           `yaml:"target" json:"target"`      // step 级按 tool id；flow 级按 flow id；"*"=全部
    Stage       string           `yaml:"stage" json:"stage"`        // 见下
    Action      string           `yaml:"action" json:"action"`      // 已注册 Tool id 或 Flow id
    Criticality HookCriticality  `yaml:"criticality" json:"criticality"` // critical(默认, fail-closed) | best-effort
    Order       int              `yaml:"order" json:"order"`        // 装配顺序（升序执行）
}
```

- **Stage（触发节点）**：`before_flow` / `before_step` / `after_step` / `on_error` / `on_success` / `before_promote`。
- **匹配**：Flow 级 stage 只看 `Target=="*" || Target==flowID`；step 级只看 `Target=="*" || Target==toolID`。
- **Action 载体**：Tool（直接调其 runtime，结构性无递归）或 Flow（同步内联执行）。
- **Criticality**：空值按 `critical`（fail-closed，失败即终止编排）。改写 input/output 后按 Tool schema 重新校验，不可绕过。

---

## 4. CLI 接口

框架：`github.com/spf13/cobra`。根命令注册于 `internal/cli/root.go`。

### 全局持久 flag

| flag | 说明 | 默认 |
|---|---|---|
| `--registry <path>` | 注册库路径 | `<GEAR_HOME>/registry.db`（`GEAR_HOME` 未设时取 `<HOME>/.gear`，**不随 cwd 变化**） |
| `--addr <host:port>` | REST 执行面地址（也可用环境变量 `GEAR_ADDR` 覆盖） | `127.0.0.1:8787` |
| `--scope <app-id>` | 运行期命名空间守卫：本进程只引用该 App 名下的定义（平台级对象——cap 原语与 `ext.serve` 这类无前缀注册 Tool——不受管；`tool/flow/hook list` 同口径分组，详见 `IDENTITY_CONTEXT_DESIGN.md` §4.3） | 空（不启用） |
| `--log-file <path>` | 诊断日志档案（可选）：记录命令生命周期与内核事件；空=不写 | 空（零行为变化） |
| `--log-level <lvl>` | 诊断日志级别 `debug\|info\|warn\|error`（仅 `--log-file` 给了才生效） | `info` |

诊断日志与执行进度（`✓ step …`）是**正交两层**：进度面向盯着看的人（stdout），诊断面向事后归档（文件）——所以 `gear exec status --log-file run.log` 的 stdout 仍可直接管道给 jq。档案含命令路径/pid/cwd/registry/版本、脱敏后的 flag（`--api-key` 打码为 `<redacted>`，档案是会被随手外传的文件）、截断的位置参数、起止耗时与失败原因。两条处置红线：**level 写错直接失败**（用户输入错误）；**文件开不了则主命令照常跑 + stderr 明说「本次不留档案」**（不静默降级——对着不存在的文件排障是最坏路径）。

### 命令树

**`gear tool`**

| 子命令 | 参数 | 说明 |
|---|---|---|
| `register <file.yaml>` | `-` 表示 stdin | 注册 / 更新 Tool（含误用护栏：含 steps 会提示改用 flow） |
| `run <id>` | `--input` / `--input-file` / `--arg k=v`（互斥，可管道） | 一次性执行单 Tool，触发 step 级 Hook |
| `list` | — | 列出所有 Tool |
| `show <id>` | — | 查看 Tool 详情 |
| `unregister <id\|pattern> [...]` | `*` `?` `[]` 通配符 | 注销（被引用 / 运行中则拒；命中集为空则报错） |
| `promote <id> <version> <target>` | — | 晋升版本到目标状态 |
| `versions <id>` | — | 列出版本与状态 |

**`gear flow`**

| 子命令 | 参数 | 说明 |
|---|---|---|
| `register <file.yaml>` | — | 注册 / 更新 Flow（走 `ValidateFlowStatic`） |
| `list` | — | 列出 Flow |
| `run <id>` | `--max-parallel`(默认 4) / `--var k=v` / `--input` / `--input-file` | 执行 Flow（service step 会被拒，提示用 app run） |
| `validate <file.yaml>` | — | 仅静态校验不落库 |
| `inspect <id>` | — | 查看 Flow 详情 |
| `delete <id\|pattern> [...]` | `*` `?` `[]` 通配符 | 注销 Flow（被引用 / 运行中 / **是某 App 的 entry_flow** 则拒；命中集为空则报错；**删除会留痕、可回滚**） |

**`gear hook`**：`register <file.yaml>` / `list` / `show <id>` / `remove <id>`。

**`gear app`**
- `app run <flow-id>...`：`--restart always|never`（默认 always）、`--max <n>`（并发闸门）。把 Flow 变成受管进程（service 长驻 + 重启策略 + 退避）。
  service 退出时打印**退出码 + 输出尾部**（`✗ step <id> service 退出 (exit=N)` 后的 `│ <行>`），用于定位「拉起即秒退」类故障（端口被占、命令写错、依赖缺失）——不再只报「重启 第 N 次」。
- **共享侧车不算崩溃**：多个 app 引用同一个 service ext（如 `ext.serve`）时，后起的那个会**退让复用**已经存在的侧车（日志显示 `service 长驻启动 (pid=0)`），本进程手上没有本地进程——这是共享的正常态，不会被判成 service 退出、也不会去抢已被占用的 coord 端口。侧车挂了由 coord 看门狗重握手自愈（必要时本进程接管为 owner）。详见 `EXT_SERVICE_DESIGN.md` §12.10。**能力调用路径同样自动**：`ai rewoo` / Lua `ctx.ext.*` / REST / MCP 调 service ext 时，首次调用前也走同一 coord 握手——无人监听则本进程当 owner 拉起，无需用户手工先起 sidecar（2026-10-20）。
- **registry 沿进程树继承**：`openRegistry()` 会把本次生效的 registry **绝对化**后写进 `GEAR_REGISTRY`，派生的进程（`app start` 的监管进程、`app run` 里 engine 拉起的长驻 Tool 进程）自动读同一个库。所以应用内步骤**不该**写死 `--registry`——写了也仍以显式值为准，但会因「换个 cwd 就静默指向另一个库」在注册/校验期收到 warning。传**相对路径**的 `--registry` 会在 stderr 提示（相对值按调用时 cwd 解析，cwd 是调用方的属性、不是 gear 的属性）。
- `app start | stop | status | list`：**后台**启动器 + 台账（见下方 `gear mcp` 一节的说明）。
- `app delete <id>`：注销 App 清单。**刻意不做引用守卫**（App 只是聚合描述，它引用的 Tool/Flow 是独立实体，要求「先拆光 Tool 才能删清单」会堵死清理路径）。它存在是为了让上一条守卫的建议可执行：`flow delete` 会因「是某 App 的 entry_flow」拒绝并提示先注销该 App，而 CLI 面原本没有这个入口（只能走 Web UI / MCP）。
- `app prune`：`--dry-run`（只汇报，不动磁盘）、`--flow <id>`（只清该 Flow）、`--apps-dir`（台账目录）。清理台账里**确定已终止**的记录——只清 `exited` / `stopped` / `startup_failed` 三种终态，`running` 与 `unknown` **一律保留**（unknown 的双判据自相矛盾，删掉可能把「还在跑却再无人可查」的进程变成真孤儿）。**只删记录，不杀进程、不碰端口**（要停进程用 `app stop`）。`start` 失败一次就落一条台账，调试一轮下来很容易堆十几条，prune 就是为「按应用批量清干净」准备的。与 MCP `gear_app(action=prune)`、REST `POST /api/apps/prune` 共用 `internal/app` 同一份实现。

**`gear serve`**
- `serve`：`--seed <examples.yaml>`（启动时灌入示例 Tool/Flow，如 `examples/news-top10.yaml`）。启动 REST 执行面，复用 `--addr`。
  **非回环地址默认拒绝**（与 `gear mcp` 同一份判定；REST 面当前无鉴权，`0.0.0.0` 或空 host `:8787` 会把「读写 registry 与明文 secrets、执行编排」暴露给局域网），
  确有远程需求时显式加 `--allow-remote`（放行时 stderr 留警告）。

**`gear exec`**（REST 无状态客户端）
- `exec start <flow_id>` → `POST /api/executions`（202 + id）
- `exec status <id>` → `GET /api/executions/{id}`
- `exec list` → `GET /api/executions`

**`gear ext`**（运行内置扩展原子 sidecar 进程）
- `gear ext <name>`：按 name 起一个扩展原子进程（`email` / `sms` / `wecom` / `kafka` / `rabbitmq` / `webhook` …），
  走 stdin/stdout JSON 行协议，供引擎 `CommandTransport` 一次性调用。**一般由 `plugin/extensions.yaml`
  里的 `cmd` 自动拉起**，手写调用主要用于调试某个原子的协议。
  > 红线：这些原子的凭据与连接配置**只从调用参数传**，不读环境变量（由 `internal/ext/email_test.go:TestExtAtomsMustNotReadEnv` 守卫）。

**`gear ai`**（内置 `ext.ai` 进程内 agent）
- `ai gen flow` / `ai gen tool`：`--intent` / 位置参数、`--register`（自动落库）
- `ai capabilities`：打印当前能力目录（内核原语 + 已注册 `ext.*`）。内核原语分两类：**受门禁的 builtin 7 个**（http/call/db/secret/event/subscribe/config，需在 Tool 里声明 `capabilities`）+ **始终可用的 pure 5 个**（time/template/crypto/id/validate，无需声明）。目录由代码单一来源渲染（`allBuiltinPrimitives` + `capabilityCatalog`），不再各写一份字面量清单。每个方法另带 **`invoke_form`** 与 **`lua_call`** 两个派生字段，直接告诉你 Lua 侧怎么调（判据唯一实现在 `internal/engine/invokeform.go`，与 `lua.go` 的绑定一一对应）——位置参数（`ctx.http.Request(a,b,c)`）/ 单具名 table（`ctx.db.query({sql=…})`）/ 裸函数（`ctx.call(target, input)`，**没有 `.Call`**）三种，别凭印象猜
- `ai react "任务"`：跑一个 ReAct 循环（每步思考→调工具→看结果，直到 Final Answer）。`--max-steps`（默认 6）/ `--allow`（限制可用能力：能力名如 `http,secret`，或方法名如 `ext.redis.set`）/ `--allow-base`（原子能力组：builtin 原语 7 + pure 纯函数 5 整组开放）/ `--allow-flow`（flow 内能力组：已注册 Tool/Flow 经 `call` 调用 + `ext.registry` 发现）/ `--allow-all`（不设限开放整个能力目录，与以上白名单来源互斥）/ `--mode meta`（元工具模式：工具面恒定 4 个、按需检索，规模无关）。等价于在 Tool 的 entry 里调 `ctx.ext.ai.react({ task = "..." })`（同款 `mode` 字段）——**用它打造自己的 agent 是最短路径**
- `ai rewoo "任务"`：跑一个 REWOO 循环（planner 出依赖图 DAG → worker 按拓扑层**并行**执行、0 LLM、单点失败 fail-soft → solver 一次 LLM 汇总）。`--max-nodes`（默认 20）/ `--replan-budget`（默认 1，**0 = 关闭重规划**）/ `--max-parallel`（默认 4）/ `--wall-clock`（总墙钟秒数，默认 300；慢模型/重任务调大——耗尽时 planner 阶段报明确错误、solver 阶段带部分结果返回）/ `--allow` / `--allow-base` / `--allow-flow` / `--allow-all` / `--temperature`。⚠ flag 前必须带 `--`（`allow-all` 漏破折号会被当位置参数，命令会检测并报错）。LLM 调用**有界**：顺利 3 次、最坏 2+2×replan_budget+1（默认 5 次；planner/replanner 各带一次校验失败才烧的修正）。**节点的 action 两类都吃**：「能力.方法」（`http.Request`/`db.query`…，走 `ctx.*` 沙箱通道）或**已注册 Tool/Flow id**（经 `ctx.call` 原语）——用户自建 agent 的调用对象可以是原生能力，也可以是自己定义的 tool。节点间用 `{{#E}}` 引用上游产出（整值保留类型、嵌串转文本、支持字段路径 `{{#E2.src}}`，HTTP 信封可钻透到 body 字段）。返回 `{answer, plan, evidence, node_outcomes, gaps, recovery_log}`——plan 是数据，可存可 diff 可人工审。**多源调研 / 大批量同构 / 跨系统对账 / 成本敏感的定时任务**用它；单步或强交互任务走 react（选型表与反例见 `internal/mcp/guide/ai.md` §2.7）。**「总结 PDF」典型组合**（2026-10-20 真机）：`--allow-base --allow @noflow --allow ext.pdf --allow ext.ai`——原子能力 + 排他工具面 + pdf 读取 + LLM 消费节点；`ext.ai` 不在原子组，少了它 planner 只能拿 http 手搓 API（必 400）
- `ai complete "prompt"`：单轮补全（无记忆、无工具）。仅做「一次问答」时用，多步任务请走 `ai react`
  > **react 走原生 function calling**：工具声明由能力目录自动生成并随请求下发（`ToolSpec`），模型用 `tool_calls` 回调用意图，引擎不再从自由文本里抠 JSON 围栏。工具名里的「.」会被压成「_」（`http.Request` → `http_Request`，OpenAI 的 function name 字符集所限）。
  > **`ai react` 会先灌入持久 registry 的目录**（与 `ai gen` 同一手法）：agent 看得到真实的 Tool/Flow 定义，写操作仍隔离在内存库里。库里若有声明了不存在原语的过期定义，会被跳过并在 stderr 点名（不会让整个命令失败）。
  > **工具不是越多越好**：能力目录有 28 个能力 / 115 个方法，全量下发会让模型选不准。授权粒度三档（`react`/`rewoo` 同口径，可叠加）：`--allow <列表>`（细粒度：能力名 /「能力.方法」/ rewoo 还吃已注册 Tool/Flow id）→ `--allow-base`（原子能力组：引擎自带 12 个整组）→ `--allow-flow`（flow 内能力组：已注册 Tool/Flow 整组）；`--allow-all` 是全开放、与前三者互斥。不给 `--allow` 时用一组通用能力（23 个方法），重型扩展（ext.redis 29 个方法）和 crypto（13 个）需显式声明；一次最多下发 48 个工具，超了报错而不是静默截断——请用方法级 `--allow ext.redis.set` 收窄。
  > **失败分层恢复**（2026-10-19）：工具错误按七类分类（transient / channel / bad_input / upstream_bad / semantic_empty / auth / unrecoverable）分层处置——瞬时的机械重试（不占步数）、端点不吃工具声明时降级文本协议、其余回灌结构化信号由模型重决策、同一动作连续失败达阈值即封禁并记缺口。结果新增 `gaps`（缺口清单）与 `recovery_log`（恢复流水，两者始终存在），步数耗尽的兜底答案带缺口清单——**带缺失作答，不静默残缺**。详见 `internal/mcp/guide/ai.md` §2.6。

**`gear version` / `gear doctor`**（自查，2026-10-03 新增）
- `gear version`（等价 `gear --version`）：版本身份（版本号 / commit / 构建时间）+ **工作落点**
  （`GEAR_HOME`、registry、执行记录库、配置库、密钥库）+ 侧车产物清单（带构建时间）。
  显示 `dev` 表示构建时未注入版本号（**不是**发布构建，重建 `bash scripts/build.sh` 即可）。
- `gear doctor`：环境自检——registry 计数、扩展声明与侧车是否就绪、AI 凭据有没有配（只看有无，
  不回显明文）、应用台账遗留进程与端口占用。**有告警时退出码为 1**（便于脚本判断）。
  判据与 MCP 的 `gear_doctor` 是**同一份实现**（`internal/doctor`），两面结论必然一致。
- 出现「命令不存在 / 参数不认识」时先跑这两个：可区分「功能真的没有」与「二进制旧 / PATH 指错」。

**`gear config`**（持久化键值配置，落点 `<GEAR_HOME>/config.db`，`GEAR_CONFIG_DB` 可覆盖）
- `config get <key>` / `config set <key> <value>` / `config delete <key>` / `config list [前缀]` / `config path`
- **AI 缺省配置**：写 `ai.provider` / `ai.model` / `ai.base_url` / `ai.api_key` / `ai.max_tokens`，此后 `ext.ai` 的所有调用自动带上——**不必再给每个进程 `export GEAR_AI_*`**（MCP 客户端、app 监管进程这类「被人派生」的场景根本传不进去环境变量）。
  优先级链：**调用参数 > 环境变量 > 配置库 > 内置默认**——环境变量仍可临时覆盖，既有行为不变。
  ```bash
  gear config set ai.provider siliconflow
  gear config set ai.base_url https://api.siliconflow.cn/v1
  gear config set ai.model deepseek-ai/DeepSeek-V4-Flash
  gear config set ai.api_key sk-xxx
  gear config list ai.          # api_key 等敏感键默认脱敏，--show-secrets 看明文
  ```
- **应用配置**：声明 `capabilities: [config]` 的 Tool 可在 Lua 里 `ctx.config.get({key="..."})` / `set` / `delete` / `list`。
  `ctx.config.get` 对不存在的 key **平返回空串**（KV 语义，方便 `or ""` 兜底）；`delete` 不存在则报错。
  落点由宿主决定，应用不必（也**读不到**）`GEAR_HOME`——Lua 沙箱禁 `os` 库、模板无 `${env}` 前缀，
  这正是过去应用只能把 SQLite 绝对路径硬编码进 YAML 的原因。

  **应用定义可移植性（2026-09-25 起）**：apps/ 与 examples/ 下的 YAML **禁止写机器相关绝对路径**
  （如 `D:/gadern/gear/...`，仓库拷到别的机器即跑不起来）。内核提供相对锚点，应用直接写相对值即可：
  - **db 的 dsn 写相对库名**：`local DSN = "mindstorm.db"` → 锚到 `GEAR_APP_DATA`（默认 `<Home>/data`），与 cwd、机器都无关；
  - **process entry 写裸名**：`entry: "gear serve --addr 127.0.0.1:8790"` → 锚到 gear 可执行文件所在目录，起的就是本仓这一个 gear；
  - **ext.serve 的 `dir` 写相对安装根**：`dir: apps/mindstorm/www` → 锚到 `InstallRoot`（剥掉 build/plugin 两层）。
  违反会被 `tools/portable_audit.py`（质量闸门第⑨项）拦下，且**发布后智能体主要通过 MCP 接口（`gear_validate` / `gear_apply`）编写应用**——`gear_validate` 会在写作期直接报 `PORTABLE_PATH_HARDCODED` 并拒绝 `gear_apply` 落库，给作者即时反馈（CI 是兜底）。详见 REQ-APP-PORTABLE 与 `gear_guide("portability")`。

**`gear secret`**（持久化密钥，落点 `<GEAR_HOME>/secrets.db`，`GEAR_SECRETS_DB` 可覆盖）
- `secret set <name> <value>` / `secret get <name>` / `secret delete <name>` / `secret list [前缀]` / `secret path`
- **与 REST 面同一个 secrets.db**（`gearhome.SecretsDBPath`）：CLI 写进去、`gear serve` 读得到，反之亦然。
  写入即持久，重启仍在；Flow/Tool 经 `${secrets.<NAME>}` 或 `ctx.secret.get("NAME")` 读取。
- **只有 `get` 回明文，`list` 只回名字**（不回显、不回值）——与「运行期面不回显明文」的红线一致。
  运行期（Lua 沙箱）只读且**不可枚举**，本子命令是宿主管理面。
- **播种通道只有两条，都不在 MCP 面上**：本子命令，或 `PUT /api/secrets/{name}`（需 `gear serve` 常驻）。
  这是刻意设计——MCP 面（远程智能体直连）不提供密钥写入口。缺失时报错会**指名这条命令**，
  见 `internal/mcp/guide/security.md` §2。
  ```bash
  gear secret set MY_TOKEN sk-xxx && gear secret path
  gear secret list MY_              # 按前缀过滤
  ```

**`gear export` / `gear import`**（产物：应用的可移植形态，2026-09-26）
- `gear export <app-id> --out <dir> [--format dir|zip] [--root <dir>] [--no-static]`：把一个应用导出成自包含产物
  ——`gear-artifact.yaml`（格式版本与组成）+ `app.yaml`（App 清单）+ `bundle.yaml`（名下 Tool/Flow/Hook）+ 静态资源。
  **dir 形态（默认）可 diff 可 git 版本化**；`--format zip` 打成 `.gearapp` 便于发送。
  `--out` 在 dir 形态下**必须是不存在的路径或空目录**（gear 不会为一个可重新生成的产物删掉内容未知的目录）。
  静态资源按 `layout.files` 声明的路径去找：`--root` 指定基准目录；不指定则**从当前目录逐级向上找**
  能解析声明路径的目录（清单里的路径通常相对仓库根写，而你常在 `build/` 这类子目录里跑）。
  绝对路径的声明与基准目录无关。
  `<app-id>` 优先取 App 清单；**只有 Flow 没有清单的应用也能导**（把 id 当入口 Flow：`<id>` → `<id>.up`
  → 唯一 `<id>.` 前缀，临时合成清单且**不写库**，warnings 里会说明；静态资源按该 Flow 的
  `static_site.serve` 的 `dir` 推断）。
- `gear import <path> [--overwrite] [--dry-run] [--apps-dir <dir>]`：把产物导回 registry，
  冲突语义与 `gear_apply` 一致（未传 `--overwrite` 且内容不同则整批拒绝）。
  **静态资源默认不落地**（gear 从不持有它们）；传 `--apps-dir` 才落地到 `<dir>/<app-id>/`，
  并重写清单里的 `layout` 指向实际位置。
  ```bash
  gear export mindstorm --out /tmp/mindstorm && ls /tmp/mindstorm   # gear-artifact.yaml app.yaml bundle.yaml www/
  gear import /tmp/mindstorm --dry-run                              # 先看会动什么
  ```
- 为什么需要它：只有 Tool/Flow 落在 registry 里**不等于「你的产品能带走」**——静态资源不在库里、
  App 清单过去根本不入库，换台机器只剩四处散落的状态，整拷 `GEAR_HOME` 是克隆机器而不是交付。
  产物是「用户构建的东西能备份、能换机器、能交付给别人」的载体。
  ⚠ 只搬**定义与静态资源**：业务数据与凭据不在产物里（`requires.secrets` 仍只是声明）。

**`gear history`**（定义变更历史：留旧与回滚，2026-09-26）
- `gear history list [--kind tool|flow|hook|app] [--id <id>] [--limit N]`：按时间倒序列出某对象的历次状态（seq / 时间 / 类别 / 主体 / 备注）。
- 回滚走 `gear_apply(restore_from=<seq>)`（MCP 面），**CLI 只读不写**：写入口必须只有一个，两个实现各写一份回滚，迟早漂移。
- 快照在 registry 的唯一写入口（`upsert`）里采集——**每一次 upsert 都留痕**，不靠各个调用点自觉记快照（漏一处就是一个静默的不可回滚点）。
  一条历史的含义是「那时它是这样的」，所以想回到哪一步就把那一步的 `seq` 填进 `restore_from`；
  回到「当时还不存在」的那条 = 撤回这次新建。回滚本身也会留痕（`note=restore from N`），链路始终线性、可连续回滚；
  写回的内容会**重新校验**，规则收紧过的旧快照宁可原子失败，也不给你一个回滚成功却跑不起来的定义。
- **删除也留痕**：删除走另一条路（不经过 `upsert`），所以专门把快照下沉进删除事务（`note=delete`），历史里查得到每一次删除；被引用拒绝的删除**不留痕**（守卫与留痕必须原子，否则时间线会凭空长出一格）。删除行保留被删内容，因此把 `note=delete` 那条的 `seq` 填进 `restore_from` 就是「撤销这次删除」。

**`gear mcp`**（MCP 执行面，stdio / `--http`）
- `gear mcp`：默认以 stdio transport 启动 MCP Server（客户端配置 `command: gear, args: [mcp]`）。
- `gear mcp --http 127.0.0.1:8788`：改走 streamable HTTP transport，供远程智能体或 WorkBuddy 的 MCP 连接器接入。
  **非回环地址默认拒绝**（MCP 面当前无鉴权，绑全网卡会把「写 registry / 删对象 / 执行编排」暴露给局域网），
  确有远程需求时显式加 `--allow-remote`；带 Origin 的跨站请求一律按同源判定拒绝（与 REST 面同一判据）。
- `gear mcp --registry <path> --apps-dir <dir>`：显式交给 MCP 面 registry 路径与台账目录。**`gear_app(start)` 依赖它**——派生出去的子进程要继承 `--registry`，否则会读默认库、可能启动另一个库里的同名 Flow（未配置时 start 直接拒绝执行，不「尽力而为」）。
- `gear mcp --expose-flows`：把已注册 Flow 额外反向暴露为 `flow__{id}` 工具（**默认关闭**：工具数必须可预测；且是启动时快照，之后新建的 Flow 本次进程内不会出现；service 长驻 Flow 永不暴露）。
- 暴露 **15** 个意图级工具（手册 / 目录 / 校验 / 落库 / 删除 / 晋升 / 能力直调 / 执行 / 执行记录 / 应用生命周期 / 环境自检 / **产物导出** / **产物导入**），工具名统一 `gear_` 前缀；心智模型（四层模型、八条铁律、工具选路）只写在 `ServerOptions.Instructions` 里**一次**，工具描述只留「做什么 + 何时用 + 歧义消解」（单条 ≤240 字符、合计 ≤1,800 字符，有机械守卫）。
- `gear app start/stop/status/list/prune`：**后台**应用生命周期（与 MCP 的 `gear_app` 共用 `internal/app` 同一份实现）。`start` 派生后台进程后立即返回；`stop` 收不干净会如实报 `ORPHAN_APP` 而非谎报成功；台账落在 `<registry 同目录>/apps/`。`prune` 清理台账里**确定已终止**的记录（`running`/`unknown` 一律保留，只删记录不杀进程；`--dry-run` 先预览、`--flow` 限定单个 Flow）。

> **`start` 的「跨会话存活」是有条件的，不承诺无条件成立（2026-09-21 修正）**
> Windows 上 `start` 会以 `CREATE_BREAKAWAY_FROM_JOB` 尝试让子进程脱离调用方的 **Job Object**；
> 宿主（IDE / CI / 沙箱）若不允许脱离，进程仍留在 Job 内 —— 它会在**发起本次启动的那个会话/命令结束时被一并回收**。
> 这种情况不是启动失败（应用在会话内照常可用），但 `start` 会如实同时给出两处信号：
> 结果里的 `escaped_job: false`，以及 stderr 的 warning。**判断它能否跨会话存活必须看这个字段，不能只看 `state: running`。**
> 需要真正长期存活时，改用本就能脱离 Job 的方式拉起（独立 `Run detached` / 服务化 / 计划任务）。
> 「起了就能一直活着」这类无条件承诺属谎报，在本项目里是被禁止的——宁可明说不能存活，也不让用户在很久之后才发现进程没了。

> 注：`gear version`（等价 `gear --version`）是排查「命令不存在 / 二进制旧」的第一手工具——它同时打印版本身份与工作落点。见上文 §4。

### 命令树（续）：`gear scaffold`（生成骨架）

**`gear scaffold`** 一键生成「可直接 register」的 Tool / Flow / Hook YAML 模板，并在文件内**就地标注每个字段的用途、取值与示例**（YAML 注释），解决「不知道 YAML 怎么写」的痛点。

| 子命令 | 参数 | 说明 |
|---|---|---|
| `tool <name>` | `-o/--output <file>`、`-f/--force`、`--runtime process\|lua\|go-host` | 生成 Tool 骨架（默认 `runtime: process`） |
| `flow <name>` | `-o`、`-f` | 生成 Flow 骨架（起步 step 用纯函数原子 `crypto.hash`，注册即可跑通） |
| `hook <name>` | `-o`、`-f` | 生成 Hook 骨架 |

默认打印到 stdout（管道友好）；`-o` 落盘，`-f` 覆盖已存在文件。每个子命令的 `--help` 内含该资源的**完整字段表、取值与示例**（等于把本 §8 搬进命令行）。

```bash
gear scaffold tool my-tool -o my-tool.yaml && gear tool register my-tool.yaml
gear scaffold flow my-flow -o my-flow.yaml && gear flow register my-flow.yaml
gear scaffold hook my-hook -o my-hook.yaml && gear hook register my-hook.yaml
# 想看某类资源的完整字段说明：
gear scaffold tool --help
gear scaffold flow --help
gear scaffold hook --help
```

---

## 5. REST API 接口

路由注册于 `internal/server/server.go`（`s.routes()`），使用 `http.ServeMux` 的 `METHOD /path` 模式。**无 gin/echo。**

执行面特点：**两种语义并存**——`POST /api/executions`（跑 Flow）立即返回 `202 + execution id`，
结果经 `GET /api/executions/{id}` 查询；而 `POST /api/tools/{id}/run`（跑单个 Tool）**同步**返回输出（200）。
  > 此前 `gear serve --help` 只写「执行是异步的，POST 返回 202」，与 Tool 执行的实测行为不符（2026-10-03 修正）。
每条执行记录带 `actor`（发起主体）：REST 带 Bearer 时是解析出的 subject，CLI 是操作系统用户名；
**MCP 面恒为空**——拿不到传输层身份就不编一个出来。

> ⚠ **执行面无鉴权**（设计姿态，不是待修缺陷）：默认只绑回环 + 校验 `Origin`，而**不带 `Origin` 的请求照常放行**
> ⇒ 同一台机器上的任意进程都能调执行面（读写 registry、明文 secrets、执行编排）。
> **在这个面上开发的业务应用必须自建登录与权限**（终端用户走 `/api/tools/{id}/run` 与 `/api/executions`，
> 用 Hook 拦得住且 fail-closed，见 `examples/auth-app`）。别把「Gear 会管鉴权」写进应用设计。

### 端点全集

| Method | Path | Handler | 说明 |
|---|---|---|---|
| GET | `/api/healthz` | `handleHealthz` | 健康检查（200 `{status,tools,flows}`） |
| GET | `/api/news` | `handleNews` | 同步跑 `news.top10` Flow 返回热搜榜（未注册示例 → 404） |
| GET | `/api/tools` | `handleListTools` | 列出 Tool |
| GET | `/api/tools/{id}` | `handleGetTool` | 取单个 Tool（404） |
| POST | `/api/tools/{id}/run` | `handleRunTool` | **同步**执行单个 Tool（body=入参 map，返回 `{id,output}`；空 body=零入参执行） |
| POST | `/api/tools` | `handleCreateTool` | 创建 / 更新 Tool（201） |
| DELETE | `/api/tools/{id}` | `handleDeleteTool` | 注销 Tool（404/409） |
| POST | `/api/tools/{id}/versions/{v}/promote` | `handlePromoteVersion` | 晋升版本（body `{target}`） |
| GET | `/api/flows` | `handleListFlows` | 列出 Flow |
| POST | `/api/flows` | `handleCreateFlow` | 创建 / 更新 Flow（含 `ValidateFlowStatic`，校验失败 400） |
| POST | `/api/flows/{id}/validate` | `handleValidateFlow` | 仅校验不落库 |
| DELETE | `/api/flows/{id}` | `handleDeleteFlow` | 注销 Flow（404/409） |
| GET | `/api/hooks` | `handleListHooks` | 列出 Hook |
| POST | `/api/hooks` | `handleCreateHook` | 创建 / 更新 Hook（201） |
| GET | `/api/secrets` | `handleListSecrets` | 列出密钥名（**只回名字，绝不回明文**） |
| PUT | `/api/secrets/{name}` | `handleSetSecret` | 写入/覆盖密钥（body `{value}`；空值/非法名 400） |
| DELETE | `/api/secrets/{name}` | `handleDeleteSecret` | 删除密钥（不存在 404） |
| POST | `/api/executions` | `handleCreateExecution` | 触发异步执行（body `{flow_id}`，**202**；繁忙 429） |
| GET | `/api/executions` | `handleListExecutions` | 列出执行记录 |
| GET | `/api/executions/{id}` | `handleGetExecution` | 查询执行结果（404） |
| GET | `/api/executions/{id}/logs` | `handleGetExecutionLogs` | 执行日志（stdout/stderr） |
| POST | `/api/apps` | `handleCreateApp` | 托管一个受管 Flow 进程（201 `AppInstance`） |
| GET | `/api/apps` | `handleListApps` | 列出受管 app |
| POST | `/api/apps/prune` | `handlePruneApps` | 清理**落盘台账**里确定已终止的记录（body `{flow_id?, dry_run?}`，空 body=全量真删）。只清 `exited`/`stopped`/`startup_failed`；`running`/`unknown` 一律保留；**只删记录不杀进程**。⚠ 与同前缀的 `GET/POST/DELETE /api/apps` **不是同一个模型**：那三个管本进程内存里的 `AppInstance`（等价 `gear app run`，无台账无孤儿），本端点动的是 `gear app start` 落下的持久台账 |
| DELETE | `/api/apps/{id}` | `handleDeleteApp` | 停止受管 app（404） |
| GET | `/api/capabilities` | `handleCapabilities` | 实时原子能力目录（内核 builtin+pure + 已注册 `ext.*`，与 `gear ai capabilities` 同源）。每个方法带 **`invoke_form`**（`positional`/`table`/`bare`）与 **`lua_call`**（可直接复制的完整调用）——Lua 侧入参形态是混合的，只给参数名会让人编出运行期才炸的调用 |
| POST | `/api/capabilities/invoke` | `handleInvokeCapability` | 直接执行一次原子能力调用（body `{name, method, args}`，与 step 直调**共用**同一套校验）。**已收紧**：只接受本机 Origin，且 `secret.*` 一律 403（密钥明文不提供 HTTP 直读口） |
| POST | `/api/capabilities/invoke/stream` | `handleInvokeCapabilityStream` | `invoke` 的 **SSE 流式版**：目前仅支持 `ext.ai.complete_stream`，以 `text/event-stream` 逐增量推送 `{delta, reasoning}`（结束 `data: {"done":true}`、出错 `event:error`）。同源与禁 secret 约束同上 |
| DELETE | `/api/hooks/{id}` | `handleDeleteHook` | 注销 Hook（不存在 404） |

**错误响应是结构化的**（2026-10-03；此前只有 `{"error": 一整段文本}`，调用方只能正则剥）：

```json
{ "error": "step wms__list 执行失败: lua 执行失败: <string>:5: attempt to call ...",
  "code": "TOOL_RUN_FAILED",
  "message": "step wms__list 执行失败: lua 执行失败: <string>:5: attempt to call ...",
  "detail": "\nstack traceback:\n\t[G]: in function 'call'...",
  "hint": "检测到你把入参包了一层 {\"input\": {...}}，而本端点要的是扁平 body……" }
```

- `code` 与 MCP 面**同源**（引擎诊断优先，无诊断才按 HTTP 状态兜底，都没有则 `UNCLASSIFIED`），
  错误码表见 `internal/mcp/guide/errors.md`。
- `message` + `detail` 与旧文本**逐字相同**（拆分只是让人读的主句与技术噪音分开，不丢信息）。
- `error` 是**兼容位**（= `message`）：既有前端读它，新代码读 `code`/`message`。
- `POST /api/tools/{id}/run` 与资源路由都要**扁平 body**；包一层 `{"input":{...}}` 会在 `hint` 里收到明确提示
  （只提示不兼容——自动兼容会让真叫 `input` 的字段产生歧义）。
| POST | `/api/events/{handle}` | `handleSubscriptionEvent` | 长驻订阅的事件**回推入口**：侧车/外部源把订阅到的消息 POST 到此处，引擎投递给对应 handle（404=handle 不存在），见 EXT_SERVICE_DESIGN |
| GET | `/api/news` | `handleNews` | demo 端点（聚合示例数据源），仅 `--seed examples/news-top10.yaml` 时有意义 |

### 关键结构

- **Execution**：`id, flow_id, status(running|succeeded|failed|rejected), error, tool_ids[], result(*RunResult), started_at, finished_at`
- **RunResult**：`{flow_id, steps: {stepID:{input,output}}, skipped: {stepID:reason}}`
- **删除语义**：`ErrNotFound→404`、`ErrInUse→409`、其余 →400。
- **密钥管理（REQ-API-SECRETS）**：`GEAR_SECRET_<NAME>` 环境变量启动期播种 + `/api/secrets` 管理面写入；运行期 Tool 经 `ctx.secret.get(name)` **只读**消费（需声明 `capabilities: [secret]`），Flow 写 `${secrets.x}` 引用。密钥为进程内存态、重启即失，明文绝不落库、任何端点不回明文。写接口刻意不暴露给沙箱插件：写权限是持久化污染（一次覆盖即可劫持后续所有使用者的凭据），维护属宿主/运维职责。
- **并发闸门**：worker 池默认 64，队列默认 256；满则入口回 429（背压）。可通过 `server.WithMaxConcurrent` / `WithExecQueueSize` 调整。

---

## 6. 扩展与插件机制

Gear 以「**能力原语**」为单位扩展：内核原语分两类（受门禁的 builtin / 始终可用的 pure），
外部能力经 `ext.*` 接入（sidecar 子进程 / 进程内 Go agent / 内建 `ext.registry`）：

| 类型 | kind / 标识 | 载体 | 调用协议 |
|---|---|---|---|
| 受门禁内核原语（builtin） | `http`/`call`/`db`/`secret`/`event`/`subscribe`/`config` | 硬编码 Go 闭包 | `http`/`call` 位置参数；`db`/`subscribe`/`config` 单 table `ctx.db.query({driver,dsn,sql,params})` / `ctx.subscribe.start({source,config,callback})` / `ctx.config.get({key})`（db 多后端 sqlite/mysql/postgres；config 落点由宿主决定）；`secret`/`event` 子表方法。**须声明 `capabilities` 才能用** |
| 纯函数内核原语（pure） | `time`/`template`/`crypto`/`id`/`validate` | 硬编码 Go 闭包（`internal/engine/purefunc.go`） | 单具名 table，如 `ctx.time.now({format,tz})` / `ctx.crypto.hash({algo,data})` / `ctx.crypto.jwt_verify({token,alg,key})` / `ctx.id.uuid()` / `ctx.validate.json_schema({schema,data})` / `ctx.template.render({text,vars})`。**始终可用、无需声明 capabilities**；也可作 Flow step 直调目标（`call: crypto.hash`） |
| sidecar 子进程 | `kind=sidecar`（默认 `""`） | `CommandTransport` 起子进程 | stdin `{method,args}` → stdout 末行 `{result}`/`{error}` |
| 进程内 Go agent | `kind=agent`（目前仅 `ext.ai`） | `InProcessExtension` 闭包 | 直接 Go 函数调用 |
| ext.registry | 始终进程内 | `RegistryExtension` | `ctx.ext.registry.tools/flows/capabilities` |

**加载入口**：`engine.SetupExtensions()`（`internal/engine/extensions.go:690`）在 `server.New()` 启动时被调用。
它把配置里的方法级 schema 经 `applyExtSchemas` 转存进 `CapabilityRegistry.schemas`，供**注册期**静态校验
（`checkCapabilityMethod` / `validatePrimitiveArgs`）消费——这是「`ext.*` 方法名与参数写错要在注册期就报错，
而不是拖到运行期」的落地点。

**配置方式**：把 `extensions.example.yaml` 复制为 `extensions.yaml`，放在 gear 可执行文件同目录（次选 `<exeDir>/plugin/`、`<repo>/plugin/`、最后是 cwd —— 完整候选顺序见 `internal/engine/extensionpath.go`）；或用 `GEAR_EXT_CONFIG` 显式指定路径（**指定了却读不到会报错**，不会静默退回「无扩展」）。

**严格规则**：
- 每个 `ext.*` 必须 `ext.` 前缀、必须声明 `schemas`、且 `methods` 与 `schemas` 一一对应，否则启动即 fail-closed 报错。
- `ext.ai`、`ext.registry` 由内核**始终注册**，配置里的 `ext.ai` 段会被忽略。
- 配置存在时「以配置为准」：未列出的 sidecar 扩展不会被注册。
- 发布后加原生能力（如 TLS）：只需在 `extensions.yaml` 加一段，不必改 gear 源码。

### sidecar 子进程协议（最小实现）

stdin 读 `{"method","args"}` → stdout 末行写 `{"result":...}` 或 `{"error":...}`。任意语言可充当 sidecar（现存示例见 `plugin/mqtt-sidecar/main.go`）。

完整 sidecar 协议示例见 `plugin/mqtt-sidecar/main.go`（任意语言可充当 sidecar，现存）。`ext.db` / `slack-sidecar` 等旧 sidecar 示例的源码 **已移除**（ext.db 能力由内置 `db` 原语覆盖，见下），`extensions.example.yaml` 的 `ext.db` 段默认**注释**，仅作「用自定义 sidecar 承载 DB」的形态参考。

### `ext.filestore`（附件存取，2026-09-28 交付）

`plugin/filestore-sidecar` 是 service ext 的常驻侧车：`put` / `get` / `stat` / `list` / `delete` / `url`
（`listen` 默认 `127.0.0.1:8096`）。四条口径（见 `internal/mcp/guide/atoms.md` §4 与 PRD §4.3）：

- **root 不硬编码**：省略 `root` 参数时读配置库的 `filestore.root`（`gear config set filestore.root <目录>`，
  与 `ctx.config` 同源）；调用也没传且库里没配 → **fail-closed**（不会退化成「写到当前目录」）。
- **不跟随符号链接**：key 里的空段 / `.` / `..` / 反斜杠一律拒，拼接后必须仍在 root 内；
  路径上任何一级是指向 root 外的符号链接即拒（`delete` 顺链接删会删到 root 外的文件）。
- **大小上限默认 32 MiB**（配置库 `filestore.max_bytes` 可放宽），超限在**落盘之前**拒，不留半截文件。
- **二进制走 base64**（Lua 侧没有字节类型）；不做 MIME 嗅探；`url()` 只回相对路径 `/_filestore/<key>`，
  由应用自己挂反代——内核 REST 面因此是 +0。

真机判据：`plugin/verify.py` 的 P1b 三项（往返 / 越界 fail-closed 无残留 / 超限 fail-closed）。

### `ext.oidc`（单点登录握手，2026-09-28 交付）

`plugin/oidc-sidecar` 是 service ext 的常驻侧车：`discovery` / `code_exchange` / `jwks`
（`listen` 默认 `127.0.0.1:8098`，入站回调 `127.0.0.1:8099`）。五条口径（PRD §4.2）：

- **只做握手，不验签、不做会话**：它负责把「授权码换成令牌」「把 JWKS 拿回来」这两件事做对，
  **令牌真伪归 `crypto.jwt_verify`**（纯算法族，不受门禁），会话归业务。因此样例 Tool
  `plugin/oidc_demo.yaml` 把两截接起来跑通：`discovery → code_exchange → jwks → jwt_verify`
  （输出 `subject` / `alg`），证明「吐出的 id_token 确实能被验」而不是各跑各的。
- **issuer 混淆两道防线**：① discovery 返回的 `issuer` 必须与入参一致；② `token_endpoint`
  的 host 必须与 issuer 同主机——挡的是「凭据被送到另一台机器」，那台机器能收到
  `client_secret` 与授权码就等于能全记下来。
- **JWKS 长驻缓存**：命中不发 HTTP；`jwks` 给 `kid` 且缓存里没有则**强制刷新一次**；
  刷新失败退回旧缓存并置 `stale: true`（fail-soft 但不静默）。**必须把 id_token 头里的
  `kid` 交回去**：不给的话拿回的是上次抓的那份，IdP 换过钥匙也照样返回旧的——缓存行为完全
  正确，可校验却发生在旧钥匙上，表现为「签名不对」，是最难查的一类假故障。
- **拒收不吐令牌**：授权服务器拒绝该授权码时，只回结构化错误，**绝不回吐任何令牌字段**；
  `redirect_uri` 就算 IdP 允许省略也要求显式给（省略了，换到的令牌可能是拿另一个回调地址
  换来的，这种「悄悄 succeed」的歧义比少一个参数危险得多）。
- **入站只回报**：浏览器从 IdP 弹到 `/in/oidc/{label}?code=&state=` 时，侧车只回「收到了什么」，
  不换令牌、不建会话、不记状态。

真机判据：`plugin/verify.py` 的 P1d 段（混淆两道 / PKCE / JWKS 缓存与刷新 / 链路打通 /
**反向验证**：IdP 把 `aud` 写错即 fail-closed，换成假钥匙则报 `crypto/rsa: verification error`
——没这两条反向，「验签通过」可能只是「压根没验」）。

### `ext.cron`（定时触发，2026-09-28 交付）

`plugin/cron-sidecar` 是 service ext 的常驻侧车：`add` / `at` / `list` / `cancel`（`listen` 默认
`127.0.0.1:8097`）。到点把事件 POST 回 `callback_url`，与 webhook / mqtt / kafka 同一条回推线，
因此它可以作为事件源被 `ctx.subscribe` 订阅（`extensions.yaml` 里声明了 `source: cron`）。
四条口径（PRD §4.1 / §4.2）：

- **生命周期硬绑定（2026-09-28 拍板）**：侧车随应用起、随进程死而亡——**进程一停调度立即停止且
  不接管**，重启后不会接回间隔期内错过的触发。系统只有「有进程在跑」与「没进程」两态，不存在
  「表在、进程没了、谁在跑」的第三态。cron 表落盘的唯一用途是让重启后的新进程**重建同一张时间表**，
  **它不是跨进程任务队列**，不搬运没跑完的触发。
- **幂等**：去重键 = `name` + 触发时刻（分钟粒度），同一分钟内的重复触发静默丢弃。
- **misfire 默认不补**；显式 `catch_up` 也**只补最近一次**——补一整个窗口会让停三天的进程一启动
  就打出几千次触发，那叫风暴不叫补偿。
- **overlap 默认跳过**：上一轮没跑完又到点就跳过并点名日志，只有显式 `open` 才并发。
- 表达式自己解析（5 段标准 cron + `@daily` 等五个宏），**零新增依赖**；非法表达式在 `add`/`at`
  **注册期**就拒——等到点才炸的调度器没人敢用。

真机判据：`plugin/verify.py` 的 P1c 段（注册期 fail-closed / `list` 不摊开 payload 与回调地址 /
**硬绑定**：真 kill 侧车进程后等到触发时刻，断言零回推；再起新进程读同一张表，断言仍不接管）。

### `ext.excel`（零依赖 Excel 写出，2026-09-29 交付）

`plugin/xlsx-sidecar` 是 service ext 的常驻侧车：`write`（`listen` 默认 `127.0.0.1:8100`）。
零第三方依赖——只用手写最小 OOXML（`archive/zip` + XML），因此 **零 go.mod / go.sum 变动**（AC-E4-4，`git diff` 可机械验证）。
接口（`ctx.ext.excel.write`）：

```
ctx.ext.excel.write({path, sheets:[{name, rows:[[cell]]}], root?}) → {path, size}
```

口径（PRD §4.5）：

- **cell 多类型**：字符串 / 数字 / 布尔 / 空（`nil`）四类，分别落为 inlineStr / number / boolean / 空单元格；类型由值自身决定，不靠调用方标注。
- **root 不硬编码**：省略 `root` 时取调用参数或配置库；越界（含 `..` / 盘符 / 反斜杠）一律拒，**fail-closed 无残留**（与 filestore 同一条 outpath 囚禁）。
- **不自己验自己**：写出后由 Python openpyxl **独立读回**交叉验证（verify.py P1e），值一致才算数（AC-E4-1）。
- **万行不爆**：顺序流式写 zip，不把全表读进内存，不超 Tool 超时（AC-E4-3）。

真机判据：`plugin/verify.py` 的 P1e 段（多 sheet 混合类型写出 → openpyxl 读回一致 / 路径越界 fail-closed）。
样例 Tool `plugin/report_demo.yaml` 把 excel / pdf 两条链路接起来跑通（`gear tool register` +
`gear tool run report.export`），**走的是内核门禁 + 能力目录 + 侧车拉起的全链路**。

### `ext.pdf`（零依赖 PDF 写出 + CJK 子集，2026-09-29 交付）

`plugin/pdf-sidecar` 是 service ext 的常驻侧车：`write` / `read`（`listen` 默认 `127.0.0.1:8101`）。
同样零第三方依赖——手写最小 PDF 写出器 + **手写 TTF 子集化**（AC-E4-2 的 CJK 嵌入要求）。
接口（`ctx.ext.pdf.write`）：

```
ctx.ext.pdf.write({path, pages:[{text}], root?}) → {path, size}
```

另提供 `read`（2026-10-20 补上，闭环「agent 总结 PDF 不再猜」——此前 planner 在能力面只看到 write、只能赌 `ext.pdf` 能读，结果选错方向）：

```
ctx.ext.pdf.read({path, root?, max_pages?}) → {path, page_count, pages:[{page, text}], fonts, cid, note}
```

`read` 同样零第三方依赖——**手写 PDF 解析器**（go.mod 一字不变）：对象扫描不信任 xref（增量更新/xref 偏移脱节的 PDF 用「扫 `N 0 obj`」照样成立）、展开 ObjStm 对象流；内容流词法（`Tj`/`TJ`/`'`/`"`）+ 行结构启发；字体层覆盖简单字体（`WinAnsi`/`StandardEncoding` 原样即可读）+ **子集/Type3 字体（自定义 1 字节编码，必须走 ToUnicode 还原——真实 PDF 里中文就藏在这种字体里，原样写字节会整页乱码，已修）** + Type0/CIDFontType2（`Identity-H` 双字节码 + ToUnicode CMap 的 `bfchar`/`bfrange`，UTF-16 码元按 4 hex 解；未压缩 ToUnicode 也兜底）；FlateDecode 解压（zlib + raw deflate 双试）。

**三类「读不出真文本」明确报错、绝不吐乱码**——加密 PDF（`/Encrypt`）、Type0 无 ToUnicode（CID 落空）、扫描件（解出文本为空且未用任何字体）：agent 拿到结构化错误能换路线（例如先 OCR 或问用户要文本），拿到乱码只会照着胡说。`max_pages` 默认 100（超出截断并在 `note` 说明），文件大小护栏默认 50MB 超出拒绝。

**路径三形态都吃**（2026-10-20 真机事故后补）：`root` + 相对 `path` 是标准形态；只给绝对路径（`D:/x/y.pdf` 或 `/home/x/y.pdf`）时侧车**自动拆**成 root+文件名——机械能判的拆分不让模型猜（此前 planner 把用户的绝对路径剥成纯文件名、root 缺省成 gear 的工作目录，于是「找不到文件」而调用方看不出为什么）；两者同给则明确报错（以谁为准说不清，不静默挑一个）。`root` 缺省是侧车进程 CWD（gear 的工作目录），通常不是目标文件所在目录，别依赖缺省。

**侧车自动拉起（2026-10-20）**：service ext 的**能力调用路径**（planner 选 `ext.pdf.read`、Lua `ctx.ext.pdf.read`、REST/MCP 调用）与 Flow 的 service step 走同一套 coord 握手——首次调用前无人监听则本 gear 当 owner 经 processRuntime 拉起侧车并持有占用（心跳保活、崩溃自愈重拉），已有实例则退让复用（同主机多 gear 共享单实例，不抢端口）。用户跑 `gear ai rewoo '总结这个 PDF'` 不再需要先手工起 sidecar；短命令退出即停心跳，侧车自行退场不留孤儿。启动命令 argv[0] 的相对路径按「当前目录 → gear 可执行文件所在目录」解析，任意 CWD 下都能拉起。

口径（PRD §4.5 + §6 Q6 字体方案）：

- **CJK 嵌入字体子集**：运行时读取系统 TTF（候选首推 `simhei.ttf`），按页面实际用到的字符做**最小子集**嵌入（只搬用到的字形，复合字形算依赖闭包），用 CIDFontType2 + ToUnicode + Identity-H——阅读器能正确选字、文本可复制（AC-E4-2）。
- **缺字即报错，绝不画方框**：字体里没有的字符（如 `𝄞` U+1D11E）直接 `font_no_glyph` 报结构化错误，**不静默降级**（fail-closed，见 verify.py P1e）。
- **字体方案已拍板（替代 §4.5 登记的降级方案）**：运行时读系统 TTF + 按字符最小子集嵌入，而不是内嵌固定字体；TTC / OTF 直接报错（不假装支持）。
- **路径囚禁同出path**：越界 `..` / 盘符 / 反斜杠拒写，fail-closed 无残留。
- A4 几何、贪心折行（CJK 间可断）、页码溢出即 `pageOverflowErr`、不写时间戳（可复现）。

真机判据：`plugin/verify.py` 的 P1e 段（A4 + CJK 子集写出 → 结构合法 / 缺字 fail-closed / 越界 fail-closed）。
两条链路都**经内核再跑一遍**（样例 Tool `plugin/report_demo.yaml`）——只直连侧车 HTTP 不算数：
那只能证明侧车写对了，证明不了 `ctx.ext.*` 真接进了能力目录（2026-09-30 就在这里漏过一次）。

### `ext.oss`（S3 兼容对象存储，2026-09-30 交付）

`plugin/oss-sidecar` 是 service ext 的常驻侧车：`put` / `get` / `stat` / `list` / `delete` / `presign`
（`listen` 默认 `127.0.0.1:8102`）。同样零第三方依赖——**手写 AWS Signature V4**
（`crypto/hmac` + `crypto/sha256`），不引 `aws-sdk-go`。接口：

```
ctx.ext.oss.put({key*, data_base64*, endpoint*, region*, bucket*, access_key_id*,
                 access_key_secret*, content_type?, session_token?, path_style?, max_bytes?})
                                                       → {key, bucket, size, etag}
ctx.ext.oss.get({key*, …连接参数})     → {key, size, content_type, mod_time, etag, data_base64}
ctx.ext.oss.stat({key*, …})            → {key, size, content_type, mod_time, etag}
ctx.ext.oss.list({prefix?, limit?, …}) → {bucket, objects:[{key,size,etag,mod_time}], count, truncated}
ctx.ext.oss.delete({key*, …})          → {key, deleted}
ctx.ext.oss.presign({key*, method?, expires?, …}) → {key, url, expires, method}
```

口径（PRD §8 第三批 / E5）：

- **只做 S3 协议一套**：阿里云 OSS / 腾讯云 COS / MinIO / R2 / Ceph 都兼容 S3，靠 `endpoint` 区分；
  不做第二套协议——每多一套就要多引一个 SDK，一引就破「`go.mod` / `go.sum` 零变动」这条红线。
- **凭据只从入参进**：`access_key_id` / `access_key_secret` 每次传（应用侧写 `${secrets.oss_ak}`，
  内核执行前展开成明文注入）；侧车不读环境变量、不落库。`endpoint` / `bucket` / `region` 同理，
  **没有配置库回退**——与 `ext.filestore` 不同，是有意的选择：省掉「配置与入参不一致时听谁的」
  这个歧义源，侧车也就成了无状态进程。
- **key 校验**：禁 `..` / 空段 / 前导 `/` / 反斜杠，长度 ≤1024。远端没有符号链接语义，
  所以**不走** `internal/sidecar/outpath`（别照抄 filestore 那份本地囚禁）。
- **大对象走 `presign` 直传**：内核调侧车的硬超时是 10s（`internal/engine/extensions.go:212`），
  `put` / `get` 另有 32 MiB 上限（可调 `max_bytes`），再大只能 `presign` 让客户端直连对象存储。
- **`path_style` 自动判**：`endpoint` 是 IP 时自动走 path-style（MinIO / 自建 Ceph 的默认形态），
  否则 virtual-host（bucket 打进 host，AWS / 阿里云的默认形态）；可显式指定。
- **32 MiB 上限在解码之前判**：先按 base64 长度估算，超限立刻拒——先解进内存再判等于允许打满内存。

真机判据：`plugin/verify.py` 的 P1f 段（六方法真实往返 / 坏 key 与连不上都 fail-closed /
样例 Tool `plugin/oss_demo.yaml` 经内核再跑一遍）。签名正确性另由 Go 单测把关：
假 S3 端点**从收到的原始请求反推 canonical request 重算签名**再比对，且 SigV4 的黄金向量来自
botocore 与 aws4 **两个独立实现**（对同一请求给出同一签名，不是凭记忆写的常量）。

### `ext.redis`（Redis 数据结构 + 分布式锁 + 延迟队列，2026-09-30 交付）

`plugin/redis-sidecar` 是 service ext 的常驻侧车：KV / Hash / List / Set / ZSet 全数据结构 +
分布式锁 + 延迟队列（`listen` 默认 `127.0.0.1:8103`）。**零第三方依赖——手写 RESP2 协议**
（`plugin/redis-sidecar/resp.go` 自编自解码 `+ - : $ *` 五种类型，不引 `go-redis`）。接口（每个方法
都带 `host` / `port` / `db` / `password` 连接参数；`host` 不含端口、`password` 无则不带 AUTH）：

```
ctx.ext.redis.set({key*, value*, ex?, host*, port?, db?, password?})        → {ok}
ctx.ext.redis.get({key*, …})                                               → {value}
ctx.ext.redis.del({keys* | key*, …})                                       → {deleted}
ctx.ext.redis.expire({key*, seconds*, …})                                  → {set}
ctx.ext.redis.ttl({key*, …})                                               → {ttl}
ctx.ext.redis.exists({keys* | key*, …})                                    → {exists}
ctx.ext.redis.incr({key*, by?, …})                                         → {value}
ctx.ext.redis.hset({key*, fields? | field*, value*, …})                    → {added}
ctx.ext.redis.hget({key*, field*, …})                                      → {value}
ctx.ext.redis.hgetall({key*, …})                                           → {fields:[{field,value}]}
ctx.ext.redis.hdel({key*, fields* | field*, …})                            → {deleted}
ctx.ext.redis.lpush/rpush({key*, values* | value*, …})                     → {length}
ctx.ext.redis.lpop/rpop({key*, count?, …})                                 → {value | values}
ctx.ext.redis.llen({key*, …})                                              → {length}
ctx.ext.redis.lrange({key*, start?, stop?, …})                             → {values}
ctx.ext.redis.sadd({key*, members* | member*, …})                          → {added}
ctx.ext.redis.smembers({key*, …})                                          → {members}
ctx.ext.redis.srem({key*, members* | member*, …})                          → {deleted}
ctx.ext.redis.scard({key*, …})                                             → {cardinality}
ctx.ext.redis.zadd({key*, members*:[{score,value}], …})                    → {added}
ctx.ext.redis.zrange({key*, start?, stop?, with_scores?, …})               → {values | items:[{value,score}]}
ctx.ext.redis.zscore({key*, member*, …})                                   → {score}
ctx.ext.redis.zrem({key*, members* | member*, …})                          → {deleted}
ctx.ext.redis.lock({key*, token*, ttl_seconds*, …})                        → {acquired}
ctx.ext.redis.unlock({key*, token*, …})                                    → {released}
ctx.ext.redis.enqueue({key*, items*:[{payload,delay_seconds}], …})         → {enqueued}
ctx.ext.redis.dequeue({key*, batch?, …})                                   → {items:[{payload}]}
```

口径（PRD §8 第三批 / E5）：

- **只做 Redis 协议一套**：不引 `go-redis` / `redis-py`——service 侧车虽是独立 binary，但 `import`
  了 `gear/internal/sidecar/coord`，Go 的 `internal` 规则把它绑死在根 module，**任何侧车加第三方依赖
  都等于写进根 `go.mod`**，一引就破「`go.mod` / `go.sum` 零变动」这条红线。RESP2 比 oss 的 SigV4 还简单
  一个量级（纯文本协议），手写代价可接受。
- **凭据只从入参进**：`host` / `port` / `db` / `password` 每次传（应用侧写 `${secrets.redis_pw}` 等）；
  侧车不读环境变量（唯一例外 `COORD_ADDR`）、不落库，是无状态常驻进程。
- **单连接串行 = 原子**：redis 单线程，侧车借一个连接、连发多条命令再归还（`Pool.With`），故锁的
  `GET+DEL`、延迟队列的 `ZRANGEBYSCORE+ZREM` **不用 Lua** 也天然原子——假端点同样成立。
- **key 校验**：redis key 不是文件路径（与 oss 的 S3 key 不同），不做 `..`/前导`/`/反斜杠那套路径语义
  拒绝，但挡掉 `\0` / `\n` / `\r` 并限长 2048（这些字符会在 RESP 边界处理上出错）；`field` / `member` /
  `payload` 同此规矩。
- **内核调侧车硬超时 10s**（`internal/engine/extensions.go:212`）：单条命令 deadline 与 `With` 总时长都
  钳在 10s，超时即 `redis_unreachable` fail-closed。

真机判据：`plugin/verify.py` 的 P1g 段（全数据结构 + 锁 + 队列真实往返 / 坏 key fail-closed /
样例 Tool `plugin/redis_demo.yaml` 经内核再跑一遍）；协议正确性另由 Go 单测把关（`main_test.go` 里的
内存假 Redis 覆盖全部数据结构、锁、延迟队列、fail-closed）。

> **内置 `db` 已原生支持多后端（sqlite / mysql / postgres）**：`ctx.db.query/exec/batch/ping` 接收
> `{driver, dsn, sql, params}`，`dsn` 缺省回退引擎内存 scratch 库；mysql/postgres 需显式 `dsn`
> （含口令优先 `${secrets.*}`）。因此「连各种数据库」**不再需要 `ext.db` 侧车**——`ext.db`
> 仅作为「想用完全自定义 sidecar 承载 DB」时的参考实现保留。
>
> **`ctx.db.batch({driver, dsn, statements})` 是「一次调用多条语句」的原子写法**：多条语句落在
> 同一个事务里，全或无——任一条失败整批回滚（DDL 效果同样不残留），返回 `{results, rows_affected, committed}`
> 指明第几条、什么错。适合「扣库存 + 写订单」这类必须同生共死的写入。`db` 只在这一批内保证原子，
> 跨多次 `db.exec` 的共享事务本批不做。
>
> **库演进声明在 bundle 里的 `kind: Migrations`（不用每个应用自己 `CREATE TABLE IF NOT EXISTS`）**：
>
> ```yaml
> kind: Migrations
> app_id: myapp
> driver: sqlite          # 整份的默认目标，单条可用 driver/dsn 覆盖
> dsn: myapp.db           # 相对库名锚 GEAR_APP_DATA，与 ctx.db.* 同口径
> migrations:
>   - {version: 1, name: init-orders, sql: "CREATE TABLE orders(...);"}
>   - {version: 2, name: add-index,   sql: "CREATE INDEX ...;"}
> ```
>
> `gear_apply` / `gear_import` 会**先改库、再落定义**：已应用的版本不重跑（台账表
> `gear_migrations` 建在目标库里），每条迁移单事务执行、失败即整条回滚，并且**一个定义都不落库**
> ——绝不以「上一代 schema」启动应用。`version` 从 1 起严格递增，重复或断号在 `gear_validate`
> 就报（错误码 `MIGRATE_INVALID` / `MIGRATE_VERSION_GAP`）。`--dry-run` 是真跑 + 全回滚，
> 所以 SQL 写错在写作期就会被抓出来（MySQL 的 DDL 会隐式提交，那条路上 dry run 兜不住）。

---

## 7. Web 管理端（web-ui 插件）

`plugin/web-ui` 是独立的 Vue 3 + Vite + Naive UI + Pinia + vue-router 前端 SPA，**不进内核**。

- **与内核契约**：纯 REST `/api`（默认同源，因为内核无 CORS 头）。API base 默认空串（同源），可用 `localStorage['gear.apiBase']` 显式设置。
- **内核地址**：`127.0.0.1:8787`（可用 `GEAR_ADDR` 覆盖）。
- **构建**：`npm run dev`（端口 5173，Vite 反代 `/api` → `GEAR_API` 或 `http://127.0.0.1:8787`）；`npm run build` → `dist/`。
- **托管**：`dist/` 由任意静态服务器 / 反向代理托管，并把 `/api` 转发到内核——内核本身**不托管**其构建产物。
- **用 Gear 自己的能力托管（静态 + 反代，推荐）**：`ext.serve`（类 nginx）既托管静态目录，也支持 `proxy` 规则把 `/api` 反代到内核——**静态与接口同源**，正是 dev 下 Vite 代理的生产等价物。仓库自带 Flow `web_ui.up`（`plugin/web_ui_tools.yaml` + `plugin/web_ui_flow.yaml`），用**一个 Flow 起两个长驻服务**（`ext.serve` 静态站 + `/api` 反代 + `process` 起 gear REST 后端）：`GEAR_EXT_CONFIG=plugin/extensions.yaml gear app run web_ui.up`（**在仓库根执行**；**不要**给应用自带的 REST 面写 `--registry`，见下）。详见 `plugin/web-ui/README.md`。**启动后 `web_ui.up` 会用收尾步把「打开地址」显式打印出来**（`app run` 实时进终端；`app start` 后台启动则经 launcher 在 `app start`/`app status` 输出里报「打开地址：…」），不再需要回看文档才知道端口。
- **多站点（2026-09-24）**：`ext.serve` 侧车可同时托管多个网站（不同 `port`），这是多 app 共享同一侧车的直接收益——`web_ui.up` 托管 8080 的同时 `mindstorm.up` 托管 8090，互不干扰。幂等按「port + dir + host + proxy 全同」判定，同端口换配置 `start` 会 **fail-closed 报错**（绝不静默吞参数——旧单站点代码曾把第二个应用的 `/start` 静默吞掉，页面「成功」却 404）。`stop` 带 `port` 只停该站，省略则停全部（旧语义）。end-to-end 判据：`plugin/verify.py` P3「多站点」项。

> **应用自带的 REST 面为什么不写 `--registry`**（2026-09-22）：起服进程会把本次生效的 registry **绝对化**后写进环境变量 `GEAR_REGISTRY`，应用内步骤（含长驻 Tool 进程）直接继承——于是 UI 里的列表与你的 `gear flow list` **必然同源**。此前 `web_ui.api` 的 entry 写死了相对 `--registry ./ui-registry.db`，子进程按**自己的 cwd** 解析，读到另一次实验留下的空库：`http://127.0.0.1:8080/api/flows` 返回 `[]`、日志一个字都没有（同一个根因此前在 `gear app start` 的 launcher 上发作过一次）。两条配套护栏：`gear serve` 的 banner 现在带 **`flows=N, tools=N`**（指向空库一眼可见）；`--registry` 传相对路径会在 stderr 收到提示，entry 里的相对路径取值会在注册/校验期作为 warning 回报。显式 `--registry` **仍然优先**——机制只保证「没说的时候永远一致」，不替人翻案。

---

## 8. 配置 Schema 速查

### Tool（`internal/model/model.go`）

| 字段 | 类型 | 必填 | 含义 |
|---|---|---|---|
| `id` | string | 是 | 唯一 ID（蛇形小写） |
| `name` | string | 是 | 显示名 |
| `description` | string | 是 | 用途 |
| `runtime` | string | 是 | `process`/`lua`/`go-host`/`wasm`（仅前三者已实装） |
| `entry` | string | 是 | process=命令；lua/go-host=函数引用（Lua 时即脚本本体） |
| `input_schema` | map | 是 | JSON Schema（object） |
| `output_schema` | map | 是 | JSON Schema（object） |
| `versions` | `[]ToolVersion` | 是 | 版本列表（`version`/`state`/`digest`/`note`） |
| `capabilities` | `[]string` | 否 | 允许使用的宿主原语，最小权限 |
| `health_probe` | object | 否 | service 模式健康检查（`kind` http/tcp/cmd） |
| `timeout` | string | 否 | 单次执行超时（Go duration，如 `5m`），缺省引擎默认 30s；长耗时（AI/长任务）Tool 需显式声明 |

> **`runtime` 补充说明**：`process`=外部命令（stdin 进 JSON、stdout 末行出 JSON）；`lua`=嵌入式 Lua 脚本（`entry` 即脚本本体，沙箱，不支持 service）；`go-host`=进程内 Go 函数（`entry` 为函数名，须在 Go 代码 `engine.RegisterHostFunc("myFunc", ...)` 登记，否则注册期即拒；不支持 service）；`wasm` 已知未实现，注册期即拒。`capabilities` 合法值见上「通用写法」一节：受门禁 7 原语须声明才可用、纯函数 5 原子无需声明、`ext.*` 须 `extensions.yaml` 登记。

### Flow / Step

| 字段 | 类型 | 必填 | 含义 |
|---|---|---|---|
| `id`/`name`/`description` | string | 是 | 标识与说明 |
| `vars` | map | 否 | Flow 级变量，step 用 `${vars.x}` 引用 |
| `steps` | `[]Step` | 是 | 步骤列表 |
| `steps[].call` | string | 是 | 被调用 Tool 的 id |
| `steps[].depends_on` | `[]string` | 否 | 上游 step id |
| `steps[].when` | string | 否 | 条件执行（表达式） |
| `steps[].parallel` | bool | 否 | 显式并行 |
| `steps[].mode` | string | 否 | `once`(默认) / `service`(长驻) |
| `steps[].use_version` | string | 否 | 钉版本 |

### Hook（`internal/model/model.go`）

| 字段 | 类型 | 必填 | 含义 |
|---|---|---|---|
| `id` | string | 是 | 唯一 ID |
| `target` | string | 是 | step 级按 tool id、flow 级按 flow id；`*` = 全部 |
| `stage` | string | 是 | `before_flow`/`before_step`/`after_step`/`on_error`/`on_success`/`before_promote` |
| `action` | string | 是 | 已注册 Tool id 或 Flow id |
| `criticality` | string | 否 | `critical`(默认, 失败即终止) / `best-effort` |
| `order` | int | 否 | 装配顺序（升序） |

### 通用写法：step.call / 模板引用 / when 条件

这些写法在 Flow 的 step 与 Hook 中通用，是写完一个「能跑」的 Flow 必须知道的（YAML 速查之外，也是 `gear scaffold flow --help` 的内嵌说明）：

**step.call 两种写法**（解析规则见 `engine.resolveStepCall`，已注册 Tool 优先）：
1. 已注册 Tool 的 id（如 `my-tool`）；
2. 原子能力直调「能力.方法」（如 `crypto.hash` / `http.Request` / `db.exec` / `db.query` / `ext.ai.complete` / `call.Call`）——免去为用一次原语先注册一层 Tool 包装；此时 `input` 即该方法的参数表，输出统一为 `{result: <返回值>}`。

   直调原子分三类（与方法级 schema 共用同一份静态校验，见 `builtinCapabilitySchemas`）：
   - 受门禁内核原语（http/call/db/secret/event/subscribe/config）：须 Tool 声明 `capabilities` 才可用；
   - 纯函数原子（time/template/crypto/id/validate）：始终可用、无需声明；
   - 扩展原语 `ext.*`：须 `extensions.yaml` 登记，方法名/参数写错**注册期即拒**（不拖到运行期）。

**模板引用**（只出现在 `step.input` 的值里；合法前缀仅 `vars`/`steps`/`secrets`，注册期校验）：
- `${vars.x}`：Flow 变量（x 须是 `Flow.Vars` 顶层键；`flow run --var` 可覆盖）；
- `${steps.id.output.field}`：上游 step 出参（id 须存在；出现即隐式 `depends_on`）；
- `${secrets.name}`：密钥（运行期从 `GEAR_SECRET_<NAME>` 或 `/api/secrets` 取，**明文绝不落库、任何端点不回明文**）。

**when 条件执行**（step 级，表达式而非模板）：用表达式引擎求值，三态 skip（跳过 ≠ 失败）。
例如 `when: "steps.s1.output.n > 1"`、`when: "steps.hash.output.result != ''"`。
红线：`when` 里**不能**写 `${...}` 模板——表达式与模板是两套语法，必须分离。

### 关键环境变量

| 变量 | 默认 | 作用 |
|---|---|---|
| `GEAR_HOME` | `<HOME>/.gear`（取不到 HOME 时退化到 `<exeDir>/.gear`） | gear 自身落盘根目录，决定 registry.db 与 exec.db 的默认位置（见 `internal/gearhome/gearhome.go`） |
| `GEAR_ADDR` | `127.0.0.1:8787` | REST 执行面地址 |
| `GEAR_EXT_CONFIG` | 未显式设置时按 **exe 锚优先**自动定位：`<exeDir>/extensions.yaml` → `<exeDir>/plugin/extensions.yaml` → `<repo>/plugin/extensions.yaml` → `<repo>/extensions.yaml` → `<cwd>/…`（详见 `internal/engine/extensionpath.go`） | 扩展配置文件路径。**显式指定却读不到会报错**（fail-closed），不静默退回「无扩展」 |
| `GEAR_AI_RECIPES` | `examples/ai/recipes` | AI 配方**覆盖目录**（可选）：存在同名 `flow/tool/hook.yaml` 时优先于内嵌默认 `internal/engine/recipes/`；目录不存在则自动回退内嵌 |
| `GEAR_EXEC_DB` | `<GEAR_HOME>/exec.db` | 执行记录 SQLite 库。**不随 cwd 变化**（2026-09-21 前为相对 cwd 的 `build/gear.db`，已废） |
| `GEAR_CONFIG_DB` | `<GEAR_HOME>/config.db` | 键值配置库（`gear config` 与 `ctx.config.*` 的落点）。AI 缺省配置（`ai.*`）也存这里，与 CLI/REST 同源 |
| `GEAR_DB_DRIVER` / `GEAR_DB_DSN` | 空 | `ext.db` 驱动 / 连接串回退 |
| `GEAR_API` | `http://127.0.0.1:8787` | web-ui 开发期 Vite 反代目标 |

---

## 9. 快速开始

### 最小上手（基于 `examples/hello_tool.yaml`、`hello_flow.yaml`）

```bash
# 1) 构建主二进制（注意：go build ./... 不会产出根目录 gear.exe）
go build -o build/gear.exe ./cmd/gear

# 2)（可选）启用扩展：复制 extensions.example.yaml → extensions.yaml

# 3) 注册原子能力 + 编排
gear tool register examples/hello_tool.yaml
gear flow register examples/hello_flow.yaml

# 4) 本地同步执行（触发 step 级 Hook）
gear flow run hello_flow --var name=world

# 5) 启动 REST 执行面（--seed 把示例灌入 registry 开箱即用）
gear serve --addr 127.0.0.1:8787 --seed examples/hello_flow.yaml

# 6) 异步触发执行
curl -X POST http://127.0.0.1:8787/api/executions -d '{"flow_id":"hello_flow"}'   # -> 202 {id}
curl http://127.0.0.1:8787/api/executions/<id>                                    # -> 结果

# 7) 启动常驻服务（含 service 模式 step 的 Flow 必须这样跑）
gear app run service_flow --restart always
```

**不会写 YAML？用 `gear scaffold` 从零生成骨架**：它产出一份带字段注释、且能直接 register 的模板，不用先去翻文档：

```bash
# 生成三类骨架（默认打印到 stdout，-o 落盘，-f 覆盖已存在文件）
gear scaffold tool my-tool -o my-tool.yaml        # 默认 runtime=process；--runtime lua|go-host 切换
gear scaffold flow my-flow -o my-flow.yaml        # 起步 step 用 crypto.hash，注册即可跑通
gear scaffold hook my-hook -o my-hook.yaml

# 填好 TODO 注释后直接注册、运行
gear tool register my-tool.yaml
gear flow register my-flow.yaml
gear flow run my-flow --var topic=world

# 每个子命令 --help 内含该资源的完整字段表、取值与示例
gear scaffold tool --help
``````

**统一输入约定（Unix 管道）**：
```bash
cat vars.json | gear flow run demo
echo '{"domain":"x.com"}' | gear tool run tls.issue
cat demo.yaml | gear flow register -
```

**多文档注册（`---` 分隔）**：
`gear tool register` / `gear flow register` 都接受多文档 YAML，**并会注册文件里的全部文档**：

```bash
# 一次注册多个 Tool（文件里用 --- 分隔）
gear tool register examples/multi_tools.yaml

# 一个文件同时含 Tool 和 Flow 时，register 只接受「纯」文件，会报错并提示出路：
#   - 拆分成两个文件分别注册，或
#   - 用 `gear serve --seed <file>` 一次灌入（内部按 Tool → Flow 顺序落库）
gear serve --seed examples/news-top10.yaml
```

> 两个 `register` 子命令都是**要么全成功、要么全不写**：文件里任一文档校验失败，
> 本次不会留下部分写入。`--seed` 走「落库 Tool → 校验 Flow → 落库 Flow」，
> 中途失败会把已写入的对象**回滚**到调用前的状态（含恢复被覆盖的原值）。
> 解析规则只有一处实现（`registry.ParseYAMLDocs`），CLI 与 MCP 共用，不会漂移。

### 真实例子

- **真实抓取百度热搜 Top10**（零 LLM、零 mock，用内置示例 `examples/news-top10.yaml`；旧 `examples/news-demo` Go 示例**已移除**）：
  ```bash
  gear serve --seed examples/news-top10.yaml && curl http://127.0.0.1:8787/api/news
  ```
- **连各种数据库**：直接用内置 `db` 原语，**不需要 sidecar**——Tool 声明 `capabilities:[db]`，Lua 内：
  ```lua
  local r = ctx.db.query({driver="postgres", dsn=ctx.input.dsn, sql="SELECT * FROM t WHERE id=?", params={ctx.input.id}})
  ```
- **AI 生成**：`ext.ai` 默认已注册，`gear ai gen flow --intent "整理百度新闻 top10" --register`。

# 8) 让智能体接管配置：启动 MCP 执行面（stdio），在 Claude Desktop / WorkBuddy 的
#    mcpServers 里配 {"command":"gear","args":["mcp"]}，即可用自然语言
#    「读目录 → 写配置 → 校验 → 跑一把 → 看结果」；远程接入用 `gear mcp --http 127.0.0.1:8788`
gear mcp

### 踩坑提醒

- 含 `mode: service` 长驻 step 的 Flow **必须** `gear app run`，`flow run` 会显式拒绝。
- `--var` / `--input` 的运行时覆盖**只作用于本次运行，不写回 registry**。
- `build/` 下是本地编译产物（不入库）：`gear.exe` 用 `go build -o build/gear.exe ./cmd/gear`。`examples/` 下的旧 Go 示例（`news-demo` / `slack-sidecar` / `gear-ai-sidecar`）**已整体移除**，无需再 build。

---

## 10. 构建与质量闸门

- **构建**：无 Makefile。
  ```bash
  go build -o build/gear.exe ./cmd/gear
  ```
- **质量闸门**（`scripts/gate.sh`）：一次跑完①–⑩（编译 / vet / gofmt / staticcheck / `go test -race -cover` / 覆盖率门槛 / 四项机械审计），日志落盘 `build/testlogs/gate-<时间戳>.log`，退出码即闸门结果（0 = PASS，CI 红 = 不可合并）。
  ```bash
  bash scripts/gate.sh              # 全套：提交前 / CI 用这档
  bash scripts/gate.sh --fast       # 只跑本次改动影响到的包（迭代中用）
  bash scripts/gate.sh --serial     # 测试强制 -p 1 串行
  ```
  测试默认并行（失败自动回退串行重跑确认）；`--fast` 用依赖闭包算影响面，拿不准时自动升级为全套。详见 `GEAR_工程方法_v1.md §5.3`。
- **查看日志**（`scripts/gatelog.sh`）：`gatelog.sh latest`（最近一次）或 `gatelog.sh list`（历史 20 份）。
- **覆盖率门槛**：engine/model/registry/app/server ≥ 80%（实测 engine 87.7% / model 98.6% / registry 83.4% / server 83.9% / cli 76.5%）。
- **贡献代码前必读**：`GEAR_工程方法_v1.md`（17 条不可绕过的红线：需求可追溯、红—绿节奏、质量闸门不可跳过、单一真相源、控制流三态不可混淆、`${}` 模板与表达式分离、能静态校验的不留到运行期、枚举式声明必须逐项登记等）。

---

## 11. 文档索引

| 文件 | 用途 |
|---|---|
| `插件化自动化编排框架_PRD_v40.md` | 产品需求文档（核心概念、模型语义、架构、用例），文档与代码漂移时以 `REQUIREMENTS.md` 为准 |
| `REQUIREMENTS.md` | 需求可追溯矩阵（可执行规格契约），为每条 PRD 行为打需求 ID + 状态 + 断言测试 |
| `GEAR_工程方法_v1.md` | 交付宪法：17 条红线与质量闸门，贡献代码前必读 |
| `extensions.example.yaml` | 扩展原语配置模板（复制为 `extensions.yaml` 启用自定义扩展） |
| `examples/` | 面向用户的演示 / 种子 YAML（`hello_tool` / `hello_flow` / `sleeper_tool` / `service_flow` / `news-top10` / `ai-create-flow`），可 `gear serve --seed examples/<file>.yaml` 灌入；旧 Go 示例（slack-sidecar / news-demo / gear-ai-sidecar）**已移除**。AI 配方**默认内嵌**在 `internal/engine/recipes/`，`examples/ai/recipes/` 只是可选覆盖目录（默认不存在） |

---

---

## 12. MCP 执行面（第四种执行面）

`gear mcp` 把内核的 Tool/Flow/Hook 管理能力通过 **Model Context Protocol** 暴露给任何 MCP 客户端（Claude Desktop、WorkBuddy、Cursor 等），让智能体以标准协议帮你「读目录 → 写配置 → 校验 → 执行 → 看结果」并自测。

### 12.1 接入方式

**stdio（默认，本地智能体）**——客户端配置：

```json
{
  "mcpServers": {
    "gear": { "command": "gear", "args": ["mcp"] }
  }
}
```

**streamable HTTP（远程 / 连接器）**：

```bash
gear mcp --http 127.0.0.1:8788
```

之后在支持 MCP 的客户端里填 `http://127.0.0.1:8788`（或 WorkBuddy 的 MCP 连接器填该地址）。

### 12.2 工具面（13 个意图级工具 + 可选反向暴露）

工具面按**意图**划分而不是按资源划分：一个「校验后落库」的动作只有一个入口（`gear_apply`），
不需要模型在 `create_tool` / `create_flow` / `create_hook` 之间做资源级选择。
名称统一 `gear_` 前缀，避免与其他 MCP server 的同名工具误选（跨 server 冲突）。

| 阶段 | MCP tool | 映射到内核 | 关键语义 |
|---|---|---|---|
| 读 | `gear_guide` | `internal/mcp/guide`（embed） | 按主题取手册。**可用主题以返回体里的 `topics` 字段为准**（单一真相源是磁盘上的 `guide/*.md`，schema 文案刻意不枚举，见评审 I-5）。与 `Instructions`、`gear://guide/{topic}` 资源**同源**，杜绝「三处说法不一致」 |
| 读 | `gear_catalog` | `reg.List*` | **摘要**（id/name/description/runtime/版本状态/是否长驻），不返回 schema 与脚本源码——成熟目录一次能吃掉数万 token，其中 95% 当场无用。支持 `offset`/`limit` 翻页（`truncated` 为 true 时用 `next_offset` 续取） |
| 读 | `gear_describe` | `reg.Get*`、`app.Status` | 单个对象的完整定义（按需取，替代旧 `list_*` 的批量吐全量） |
| 读 | `gear_capabilities` | `agent.CapabilitiesMethod` | 能力目录（内核 **7** 个受门禁原语 + 5 个 pure 纯函数原子 + 已注册 `ext.*`），含方法级参数 schema；支持 `name`/`kind` 过滤以减小返回 |
| 读 | `gear_doctor` | `app.List` + 侧车/扩展探测 | 环境自检：registry 计数、扩展与侧车就绪、台账与遗留应用、端口占用。用于区分「YAML 写错」与「环境没准备好」 |
| 写 | `gear_validate` | `parseBundle` + `ValidateFlowStaticDiagsWith` | **不落库**的编译器：收 bundle YAML、一次报出**全部**字段级诊断（`code`/`step`/`field`/`hint`/`docs`），可无限次重试 |
| 写 | `gear_apply` | `reg.Upsert*`（Tool→Flow→Hook） | **唯一写入口**：原子（任一非法则一个字节都不写）、幂等（二次 apply 报 `updated`）、`dry_run`、`overwrite`（默认 false：已存在且内容不同即拒，防覆盖人工维护的定义）。**不支持密钥写入**（见 `gear_guide("security")`） |
| 写 | `gear_delete` | `reg.Delete*` | 守卫语义与 REST/CLI 完全一致；`NOT_FOUND` 与 `IN_USE` 分码，错误信息指出被谁引用。**破坏性** |
| 写 | `gear_promote` | `engine.PromoteTool` | 版本状态机（draft→testing→stable→deprecated），非法迁移被拒 |
| 跑 | `gear_invoke` | `engine.InvokeCapability` | **能力直调**：试一个原子方法（如 `db.query` / `http.Request` / `ext.ai.complete`）不必先写 Flow。**禁 `secret.*`**（与 REST 面同一条禁则 `cap.BlockedDirectInvoke`）。破坏性（可发 HTTP / 写 db / 发事件） |
| 跑 | `gear_run` | `engine.RunTool` / `RunFlowExecWithVars` | `kind=tool` 同步返回输出；`kind=flow` **异步**返回 `execution_id`（`wait_seconds` 短等）。service 长驻 Flow 一律拒（`SERVICE_FLOW_SYNC_RUN`），须走 `gear_app(start)` |
| 跑 | `gear_execution` | `execstore` | 给 id 取单次执行（可带日志、`tail`）；不给 id 列出最近执行（默认最近 20 条，`limit` 可调）。**标出孤儿记录**（状态 running 但发起它的进程已不存在，见评审 I-1④） |
| 跑 | `gear_app` | `internal/app` | 长驻应用生命周期：`start`（派生**独立后台进程**后立即返回，**是否跨会话存活看返回的 `escaped_job`**：true/false/null 三选一，false 时随本进程退出）/ `stop`（终止 + 端口复检，收不干净报 `ORPHAN_APP`）/ `status`（pid + 端口双判据）/ `list`（台账 + 遗留项）/ `prune`（清台账里确定已终止的项，`running`/`unknown` 一律保留、只删记录不杀进程）。含破坏性动作，故标 `DestructiveHint`。与 `gear app start\|stop\|status\|list\|prune` 同一份实现 |
| 反向暴露 | `flow__{id}`（`--expose-flows`，service 长驻 Flow 不暴露） | `startFlowExec(id, vars)` | 等价于 `gear_run(kind=flow, id=…)` 的便捷入口，少一次参数拼装 |

> **返回形态是硬约束，不是风格问题**：MCP 规范（2025-06-18）规定 `tools/call` 的
> `structuredContent` **顶层必须是 JSON 对象**。Go SDK（v1.8.0）依 SEP-2106 草案也放行
> 数组/标量，但严格客户端（WorkBuddy 等用 zod 校验的）会直接判协议违规并**丢弃整条调用**：
> `{"expected":"record","path":["structuredContent"],"message":"Invalid input: expected record, received array"}`。
> 故列表结果一律经 `internal/mcp.objectResult` 包一层具名键；红线由
> `mcp_test.go: TestMCPAllToolsReturnObjectStructuredContent` 逐工具锁住——
> 该用例的覆盖度反查由**显式登记表** `toolSurface`（生产侧常量，`internal/mcp/tools_surface.go`）驱动
> （不再靠 `list_` 命名约定猜），新增工具不登记即 FAIL，工具数预算 `toolBudget`（13）同处声明。诊断线上真实帧用 `python plugin/mcp_wire_probe.py`（stdio）或
> `python plugin/mcp_wire_probe.py --http`（streamable HTTP，与 WorkBuddy 连接器同一条路）：
> 它用**原始 JSON-RPC**（不经 SDK 的宽松客户端）抓帧并落盘 `build/mcp_wire_frames.<mode>.jsonl`，
> 再用**严格客户端同款判据**逐帧判，最后**自检判据**（喂一个合成的裸数组帧，必须判违规——
> 否则「全绿」可能只是判据空转）。自带临时 registry/执行库/台账、探完即杀；
> 退出码 `0`=全绿 / `1`=有 FAIL / `2`=环境失败，可直接进 CI。

> **错误形态分两类，界线要清**：**执行失败**（Flow 跑挂了、应用没起来）是**成功返回**一条
> 记录（`status: failed` + `error`），因为它不是协议错误、智能体要从记录里定位到具体 step；
> **前置条件不满足**（对象不存在、参数非法、被守卫拒绝、service Flow 被同步调用）才是工具错误
> （`IsError=true` + 文本以 `[CODE]` 开头）。错误码表见 `MCP_AGENT_DESIGN.md` 附录 D，
> 机械守卫是 `TestMCPErrorsAreStructured` / `TestMCPErrorPathsReportInsteadOfSilentSuccess`。

### 12.3 设计要点

- **协议翻译零重复守卫**：删除守卫（§8.2）、schema 校验、版本状态机全在 registry/engine 层，MCP 面只做协议翻译，与 REST 面共用同一份守卫逻辑（不会漂移）。
- **背景信息只写一次**：心智模型（四层模型、八条铁律、工具选路、常见误解）进 `ServerOptions.Instructions`，全字段语义进 `gear_guide` 的返回值；工具 `Description` 只留「做什么 + 何时用 + 歧义消解」。若把背景下沉到每个描述，`tools/list` 会随工具数**线性膨胀**且同一段话有 N 份副本（必然漂移）。上限（单条 ≤240 / 合计 ≤1,800 字符）由 `TestMCPToolDescriptionsStayLean` 机械守卫。
- **长驻应用的真启动**：`gear_app(start)` 派生**脱离调用方进程树**的后台进程（Windows `DETACHED_PROCESS|CREATE_NEW_PROCESS_GROUP`（+ `CREATE_BREAKAWAY_FROM_JOB`，失败则退化重试）/ Unix `Setsid`）后立即返回，**从不持有**它——判据是「长驻子进程不得出现在 MCP 进程树内」。因此它跨会话存活，也正因如此**必须提供 `stop`**：能活过会话却关不掉才是真缺陷。
- **台账只负责「找到它去管它」**：进程本来就自持（PRD §14.1），台账不改变归属。存活判据是**双判据**（pid 存活 + 端口探活；两者矛盾给 `unknown`，既不谎报 running 也不自动杀）；确认退出的记录会**结案**并保留结案原因（`final_state`），避免每次启动失败都变成一条永久遗留项。遗留项只**如实汇报**，不静默清理、不静默接管。
- **写之前先校验**：`gear_validate` 收 YAML、不落库、一次报全；`gear_apply` 是唯一写入口且原子幂等。契约是「能静态查出的问题绝不留到运行期」。
- **能力不足要上报而不是绕过**：缺能力时不许用 `process` runtime 跑外部脚本绕过门禁，须按 `gear_guide("primitive-gap")` 给出「基础、通用、无业务名词」的原语建议（六条判据）。
- **密钥不由 MCP 写**：`gear_apply` 只报 `[MISSING_SECRET]` 告警（不影响 `valid`），密钥请人在 CLI 播种 —— 把「谁能写凭据」收敛到人这一侧。
- **structuredContent 顶层只发对象**：列表类工具经 `objectResult` 包具名键，兼容按 2025-06-18 规范严格校验的客户端（SDK 的 SEP-2106 宽松行为会造成「本地冒烟全绿、接严格客户端全废」的错位）。**新增列表类工具时必须把它登记进 `toolSurface`**（`internal/mcp/tools_surface.go`），否则 `TestMCPToolSurfaceMatchesRegistry` 直接 FAIL。协议侧同时**钉死** `SupportedProtocolVersions=["2025-06-18"]`，并显式关掉 `logging` 与 `listChanged`（不声明自己做不到的能力）。诊断线上真实帧用 `plugin/mcp_wire_probe.py`（stdio，默认）或 `plugin/mcp_wire_probe.py --http`（streamable HTTP，与 WorkBuddy 连接器同一条路；自带临时 registry/执行库，探完即杀、不留常驻进程）。
- **执行记录同一份 SQLite**：MCP 与 REST 的执行历史互通，「运行中不可注销」跨执行面生效（MCP 发起的执行也会被 `gear_delete` 的 runningChecker 看见）。
- **默认 stdio 的约束**：stdio 下 stdout **只承载协议帧**，日志/警告一律走 stderr，不污染协议。
  这不只是提示语：引擎默认把执行进度（`✓ step …`）打到 `os.Stdout`，所以 `gear mcp` 必须用
  `Engine.SetProgressWriter(os.Stderr)` 把它改道——否则 `gear_run` 一跑进度就混进帧流
  （**2026-09-21 由 `mcp_wire_probe.py` 抓到的真实事故**：本地 Go 用例全绿，接严格客户端却会解析失败）。
  守卫两条：`internal/engine/progress_writer_test.go`（单测，进闸门）与探针的「协议卫生」判据（线上帧级）。

---

> 本 README 基于源码静态探查生成。接口细节以 `internal/`、`plugin/web-ui/src/` 实际代码与 `extensions.example.yaml` 为准。
