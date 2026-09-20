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
| **Context** | 一次执行的共享数据空间（宿主原语：受门禁的 http/call/db/kv/secret/event/subscribe + 始终可用的纯函数原子 time/template/crypto） |

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
│                  协议翻译层（19 个工具 + 反向暴露 flow__{id}）       │
├──────────────────────────────────────────────────────────┤
│  internal/engine   —— 编排内核：拓扑执行 / Hook 装配 /        │
│                       runtime 分发 / 模板 / when 表达式 /    │
│                       版本灰度 / AI 扩展装配 / 健康检查       │
├──────────────────────────────────────────────────────────┤
│  internal/cap      —— 宿主能力：受门禁 7 个原语              │
│                       http/call/db/kv/secret/event/subscribe   │
│                       另有纯函数原子：time/template/crypto     │
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
| `cap` | 宿主能力层：**7 个受 capability 门禁的原语**（http/call/db/kv/secret/event/subscribe）的接口与内存实现（最小权限门禁、KV 会话作用域、事件总线、订阅生命周期）。纯函数原子（time/template/crypto）刻意不进此门禁，见 `internal/engine/purefunc.go` |
| `server` | REST 执行面：路由、并发闸门、执行记录持久化、受管 app 托管 |
| `app` | `app run` 监管器：把一个 / 多个 Flow 变成受管进程 |
| `cli` | CLI 管理面：cobra 命令树、`gear`/`exec`/`ai`/`mcp` 客户端、extensions 装配 |
| `mcp` | MCP 执行面：把 Tool/Flow/Hook 目录与执行能力暴露为 MCP tools（stdio / `--http` streamable），反向暴露已注册 Flow 为 `flow__{id}` |
| `execstore` | 执行记录持久化（SQLite），REST 与 MCP 两执行面共用同一份执行历史与「运行中不可注销」判据 |

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
}
```

- **Runtime 实现状态**：`process` / `lua` / `go-host` 已实现；`wasm` 已知未实现。**注册期守卫**：未知 / 未实现的 runtime 注册即拒，不静默降级。
- **ToolVersion 状态机**：`draft → testing → stable → deprecated → rejected`，非法迁移被 `CanPromote` 拒绝。
- **统一参数协议（Tool）**：`stdin JSON → stdout 末行 JSON`。
- **HealthProbe**：`kind`(http|tcp|cmd) + `addr`/`path`/`cmd` + `interval`(5s)/`timeout`(2s)/`threshold`(3)，用于 service 模式探针。

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
| `--registry <path>` | 注册库路径 | `~/.gear/registry.db` |
| `--addr <host:port>` | REST 执行面地址（也可用环境变量 `GEAR_ADDR` 覆盖） | `127.0.0.1:8787` |

### 命令树

**`gear tool`**

| 子命令 | 参数 | 说明 |
|---|---|---|
| `register <file.yaml>` | `-` 表示 stdin | 注册 / 更新 Tool（含误用护栏：含 steps 会提示改用 flow） |
| `run <id>` | `--input` / `--input-file` / `--arg k=v`（互斥，可管道） | 一次性执行单 Tool，触发 step 级 Hook |
| `list` | — | 列出所有 Tool |
| `show <id>` | — | 查看 Tool 详情 |
| `unregister <id>` | — | 注销（被引用 / 运行中则拒） |
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

**`gear hook`**：`register <file.yaml>` / `list` / `show <id>` / `remove <id>`。

**`gear app`**
- `app run <flow-id>...`：`--restart always|never`（默认 always）、`--max <n>`（并发闸门）。把 Flow 变成受管进程（service 长驻 + 重启策略 + 退避）。

**`gear serve`**
- `serve`：`--seed <examples.yaml>`（启动时灌入示例 Tool/Flow，如 `examples/news-top10.yaml`）。启动 REST 执行面，复用 `--addr`。

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
- `ai capabilities`：打印当前能力目录（内核原语 + 已注册 `ext.*`）。内核原语分两类：**受门禁的 builtin 7 个**（http/call/db/kv/secret/event/subscribe，需在 Tool 里声明 `capabilities`）+ **始终可用的 pure 3 个**（time/template/crypto，无需声明）。目录由代码单一来源渲染（`allBuiltinPrimitives` + `capabilityCatalog`），不再各写一份字面量清单

**`gear mcp`**（MCP 执行面，stdio / `--http`）
- `gear mcp`：默认以 stdio transport 启动 MCP Server（客户端配置 `command: gear, args: [mcp]`）。
- `gear mcp --http :8788`：改走 streamable HTTP transport，供远程智能体或 WorkBuddy 的 MCP 连接器接入。
- 暴露 19 个工具（目录读 / 配置写 / 校验 / 执行 / 查询），并把已注册 Flow 反向暴露为 `flow__{id}` 工具，外部智能体可直接调用你的编排。

> 注：根命令暂无 `version` 子命令；版本号需经 `go build -ldflags` 注入或看 `build/gear.exe`。

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

执行面特点：**异步**——`POST /api/executions` 立即返回 `202 + execution id`，结果经 `GET` 查询。

### 端点全集

| Method | Path | Handler | 说明 |
|---|---|---|---|
| GET | `/api/healthz` | `handleHealthz` | 健康检查（200 `{status,tools,flows}`） |
| GET | `/api/news` | `handleNews` | 同步跑 `news.top10` Flow 返回热搜榜（未注册示例 → 404） |
| GET | `/api/tools` | `handleListTools` | 列出 Tool |
| GET | `/api/tools/{id}` | `handleGetTool` | 取单个 Tool（404） |
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
| DELETE | `/api/apps/{id}` | `handleDeleteApp` | 停止受管 app（404） |
| GET | `/api/capabilities` | `handleCapabilities` | 实时原子能力目录（内核 builtin+pure + 已注册 `ext.*`，与 `gear ai capabilities` 同源） |
| POST | `/api/capabilities/invoke` | `handleInvokeCapability` | 直接执行一次原子能力调用（body `{name, method, args}`，与 step 直调**共用**同一套校验） |
| DELETE | `/api/hooks/{id}` | `handleDeleteHook` | 注销 Hook（不存在 404） |
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
| 受门禁内核原语（builtin） | `http`/`call`/`db`/`kv`/`secret`/`event`/`subscribe` | 硬编码 Go 闭包 | `http`/`call` 位置参数；`db`/`subscribe` 单 table `ctx.db.query({driver,dsn,sql,params})` / `ctx.subscribe.start({source,config,callback})`（多后端 sqlite/mysql/postgres）；`kv`/`secret`/`event` 子表方法。**须声明 `capabilities` 才能用** |
| 纯函数内核原语（pure） | `time`/`template`/`crypto` | 硬编码 Go 闭包（`internal/engine/purefunc.go`） | 单具名 table，如 `ctx.time.now({format,tz})` / `ctx.crypto.hash({algo,data})` / `ctx.template.render({text,vars})`。**始终可用、无需声明 capabilities**；也可作 Flow step 直调目标（`call: crypto.hash`） |
| sidecar 子进程 | `kind=sidecar`（默认 `""`） | `CommandTransport` 起子进程 | stdin `{method,args}` → stdout 末行 `{result}`/`{error}` |
| 进程内 Go agent | `kind=agent`（目前仅 `ext.ai`） | `InProcessExtension` 闭包 | 直接 Go 函数调用 |
| ext.registry | 始终进程内 | `RegistryExtension` | `ctx.ext.registry.tools/flows/capabilities` |

**加载入口**：`engine.SetupExtensions()`（`internal/engine/extensions.go:690`）在 `server.New()` 启动时被调用。
它把配置里的方法级 schema 经 `applyExtSchemas` 转存进 `CapabilityRegistry.schemas`，供**注册期**静态校验
（`checkCapabilityMethod` / `validatePrimitiveArgs`）消费——这是「`ext.*` 方法名与参数写错要在注册期就报错，
而不是拖到运行期」的落地点。

**配置方式**：把 `extensions.example.yaml` 复制为 `extensions.yaml` 放在工作目录，或用 `GEAR_EXT_CONFIG` 环境变量指定路径。

**严格规则**：
- 每个 `ext.*` 必须 `ext.` 前缀、必须声明 `schemas`、且 `methods` 与 `schemas` 一一对应，否则启动即 fail-closed 报错。
- `ext.ai`、`ext.registry` 由内核**始终注册**，配置里的 `ext.ai` 段会被忽略。
- 配置存在时「以配置为准」：未列出的 sidecar 扩展不会被注册。
- 发布后加原生能力（如 TLS）：只需在 `extensions.yaml` 加一段，不必改 gear 源码。

### sidecar 子进程协议（最小实现）

stdin 读 `{"method","args"}` → stdout 末行写 `{"result":...}` 或 `{"error":...}`。任意语言可充当 sidecar（现存示例见 `plugin/mqtt-sidecar/main.go`）。

完整 sidecar 协议示例见 `plugin/mqtt-sidecar/main.go`（任意语言可充当 sidecar，现存）。`ext.db` / `slack-sidecar` 等旧 sidecar 示例的源码 **已移除**（ext.db 能力由内置 `db` 原语覆盖，见下），`extensions.example.yaml` 的 `ext.db` 段默认**注释**，仅作「用自定义 sidecar 承载 DB」的形态参考。

> **内置 `db` 已原生支持多后端（sqlite / mysql / postgres）**：`ctx.db.query/exec/ping` 接收
> `{driver, dsn, sql, params}`，`dsn` 缺省回退引擎内存 scratch 库；mysql/postgres 需显式 `dsn`
> （含口令优先 `${secrets.*}`）。因此「连各种数据库」**不再需要 `ext.db` 侧车**——`ext.db`
> 仅作为「想用完全自定义 sidecar 承载 DB」时的参考实现保留。

---

## 7. Web 管理端（web-ui 插件）

`plugin/web-ui` 是独立的 Vue 3 + Vite + Naive UI + Pinia + vue-router 前端 SPA，**不进内核**。

- **与内核契约**：纯 REST `/api`（默认同源，因为内核无 CORS 头）。API base 默认空串（同源），可用 `localStorage['gear.apiBase']` 显式设置。
- **内核地址**：`127.0.0.1:8787`（可用 `GEAR_ADDR` 覆盖）。
- **构建**：`npm run dev`（端口 5173，Vite 反代 `/api` → `GEAR_API` 或 `http://127.0.0.1:8787`）；`npm run build` → `dist/`。
- **托管**：`dist/` 由任意静态服务器 / 反向代理托管，并把 `/api` 转发到内核——内核本身**不托管**其构建产物。

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

> **`runtime` 补充说明**：`process`=外部命令（stdin 进 JSON、stdout 末行出 JSON）；`lua`=嵌入式 Lua 脚本（`entry` 即脚本本体，沙箱，不支持 service）；`go-host`=进程内 Go 函数（`entry` 为函数名，须在 Go 代码 `engine.RegisterHostFunc("myFunc", ...)` 登记，否则注册期即拒；不支持 service）；`wasm` 已知未实现，注册期即拒。`capabilities` 合法值见上「通用写法」一节：受门禁 7 原语须声明才可用、纯函数 3 原子无需声明、`ext.*` 须 `extensions.yaml` 登记。

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
2. 原子能力直调「能力.方法」（如 `crypto.hash` / `http.Request` / `kv.put` / `db.query` / `ext.ai.complete` / `call.Call`）——免去为用一次原语先注册一层 Tool 包装；此时 `input` 即该方法的参数表，输出统一为 `{result: <返回值>}`。

   直调原子分三类（与方法级 schema 共用同一份静态校验，见 `builtinCapabilitySchemas`）：
   - 受门禁内核原语（http/call/db/kv/secret/event/subscribe）：须 Tool 声明 `capabilities` 才可用；
   - 纯函数原子（time/template/crypto）：始终可用、无需声明；
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
| `GEAR_ADDR` | `127.0.0.1:8787` | REST 执行面地址 |
| `GEAR_EXT_CONFIG` | cwd `extensions.yaml` | 扩展配置文件路径 |
| `GEAR_AI_RECIPES` | `examples/ai/recipes` | AI 可编辑配方目录 |
| `GEAR_EXEC_DB` | `build/gear.db` | 执行记录 SQLite 库 |
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
#    「读目录 → 写配置 → 校验 → 跑一把 → 看结果」；远程接入用 `gear mcp --http :8788`
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
- **质量闸门**（`scripts/gate.sh`）：封装 `go test -race -cover -p 1 -v ./...`，日志落盘 `build/testlogs/gate-<时间戳>.log`，退出码即闸门结果（0 = PASS，CI 红 = 不可合并）。
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
| `examples/` | 示例 YAML（hello / sleeper / service / news-top10）、AI 配方（`examples/ai/recipes/*.yaml`）；旧 Go 示例（slack-sidecar / news-demo / gear-ai-sidecar）**已移除** |

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
gear mcp --http :8788
```

之后在支持 MCP 的客户端里填 `http://127.0.0.1:8788`（或 WorkBuddy 的 MCP 连接器填该地址）。

### 12.2 工具面（19 个 + 反向暴露）

| 类别 | MCP tool | 映射到内核 | 返回形态（structuredContent） |
|---|---|---|---|
| 目录读 | `list_tools` / `get_tool` / `list_flows` / `get_flow` / `list_hooks` / `capabilities` | `reg.List*/Get*`、`agent.CapabilitiesMethod` | 列表类包一层具名键：`{tools:[...]}` / `{flows:[...]}` / `{hooks:[...]}` / `{capabilities:[...]}`；单体类直接是对象 |
| 配置写 | `create_tool` / `create_flow`（YAML，入库前静态校验）、`validate_flow`、`create_hook`、`promote_tool`、`delete_*` | `reg.Upsert*/Delete*`、`engine.ValidateFlowStatic`/`PromoteTool` | 对象 |
| 执行 | `run_tool` / `run_flow`（同步等结果，vars 可覆盖）、`list_executions` / `get_execution` / `get_execution_logs` | `engine.RunTool`/`RunFlowExecWithVars`、`execstore` | 单体类对象；列表类 `{executions:[...]}` / `{logs:[...]}` |
| 反向暴露 | `flow__{id}`（每个已注册 Flow 一个，service 长驻 Flow 不暴露） | 固定调用 `runFlowByID(id, vars)` | 执行记录对象 |

> **返回形态是硬约束，不是风格问题**：MCP 规范（2025-06-18）规定 `tools/call` 的
> `structuredContent` **顶层必须是 JSON 对象**。Go SDK（v1.8.0）依 SEP-2106 草案也放行
> 数组/标量，但严格客户端（WorkBuddy 等用 zod 校验的）会直接判协议违规并**丢弃整条调用**：
> `{"expected":"record","path":["structuredContent"],"message":"Invalid input: expected record, received array"}`。
> 故列表结果一律经 `internal/mcp.objectResult` 包一层具名键；红线由
> `mcp_test.go: TestMCPAllToolsReturnObjectStructuredContent` 逐工具锁住——
> 该用例还会用 `ListTools` **反查覆盖度**：新增一个列表类工具却忘了登记进它的 calls 表就判 FAIL，
> 红线不会因「新工具没人调用」而悄悄失守。诊断线上真实帧用
> `python plugin/mcp_wire_probe.py`（stdio）或 `python plugin/mcp_wire_probe.py --http`
> （streamable HTTP，与 WorkBuddy 连接器同一条路）。

### 12.3 设计要点

- **协议翻译零重复守卫**：删除守卫（§8.2）、schema 校验、版本状态机全在 registry/engine 层，MCP 面只做协议翻译，与 REST 面共用同一份守卫逻辑（不会漂移）。
- **structuredContent 顶层只发对象**：列表类工具经 `objectResult` 包具名键，兼容按 2025-06-18 规范严格校验的客户端（SDK 的 SEP-2106 宽松行为会造成「本地冒烟全绿、接严格客户端全废」的错位）。**新增列表类工具时必须把它登记进 `TestMCPAllToolsReturnObjectStructuredContent` 的 calls 表**，否则该用例直接 FAIL。诊断线上真实帧用 `plugin/mcp_wire_probe.py`（stdio，默认）或 `plugin/mcp_wire_probe.py --http`（streamable HTTP，与 WorkBuddy 连接器同一条路；自带临时 registry/执行库，探完即杀、不留常驻进程）。
- **执行记录同一份 SQLite**：MCP 与 REST 的执行历史互通，「运行中不可注销」跨执行面生效（MCP 发起的执行也会被 `delete_tool` 的 runningChecker 看见）。
- **service 不被同步暴露**：含 `mode: service` 长驻 step 的 Flow 不生成 `flow__{id}` 工具（否则会常驻不结束），须用 `gear app run` 监管。
- **默认 stdio 的约束**：stdio 下 stdout 只承载协议帧，日志/警告一律走 stderr，不污染协议。

---

> 本 README 基于源码静态探查生成。接口细节以 `internal/`、`plugin/web-ui/src/` 实际代码与 `extensions.example.yaml` 为准。
