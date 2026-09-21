# MCP × 智能体应用创建方案（v1）

> 目标：让**不懂 gear 的通用智能体**（以 WorkBuddy 为首个客户端）通过 MCP 一次性、可验证地创建并运行一个 gear 应用。
> 本文只设计 MCP 面的**对外契约**，不改内核语义。红线不变：MCP 仍是「协议翻译层，零重复守卫」。
>
> 现状基线：`internal/mcp/mcp.go`（19 个静态工具 + `flow__{id}` 反向暴露）、`internal/cli/mcp.go`、go-sdk v1.8.0、MCP 2025-06-18。

---

## 0. 结论先行

**当前 MCP 面是「内核 API 的镜像」，不是「面向智能体的交付接口」。** 19 个工具是内核视角的完备映射，但对智能体而言：粒度太细、名字太泛、没有引导、且**缺 3 个不补就无法完成的硬缺口**。

三处改动对应三个问题：

| 问题 | 改动 | 落点 |
|---|---|---|
| 智能体不懂 gear 的设计思路 | **L0 注入层**：`ServerOptions.Instructions` 常驻心智模型（≤900 token）+ **L1** `gear_guide` 工具与 resources 按需展开（同源） | §4 §5 |
| 工具太多、说明太长、选不准 | **L2 工具层**：19 → 12 个**意图级**工具，统一 `gear_` 前缀，列表返回摘要而非全量 | §6 |
| 「应用」在 MCP 面根本起不来 | **L3 交付物层**：Gear App Bundle（声明式多文档）+ `gear_app` 生命周期工具 + `gear_validate`/`gear_apply` 闭环 | §7 |

**硬缺口（P0，不补则「通过 MCP 创建应用」不可能成立）**：

1. **service 长驻应用无法启动**——`run_flow` 对含 `mode: service` 的 Flow 显式拒绝（`mcp.go:116-118`），反向暴露也跳过它（`mcp.go:503-507`），而 MCP 面**没有任何 app 管理工具**（REST 有 `POST/GET/DELETE /api/apps`，`internal/server/server.go:226-228`）。结论：能创建「一次性编排」，**创建不了「应用」**（应用的定义就是长驻 Flow，`web_ui.up` 即范例）。
2. **没有「先校验后落库」的路径**——`validate_flow` 只接受**已注册 Flow 的 id**（`mcp.go:347-362`）。智能体只能「先写坏的进去、再改」，与 fail-closed 精神相反；且 `create_*` 校验失败时是整段拒绝、不返回定位信息。
3. **长流程同步阻塞**——`run_flow` 同步等到结束（`mcp.go:440-448`）。跑一条要 build/抓数据/起服务的 Flow 会顶到客户端调用超时。

**成本**：改 1 个包（`internal/mcp`）+ 1 个 CLI 装配点（`internal/cli/mcp.go`）+ 1 套 guide 文本 + 1 份 bundle schema；不新增内核模型、不破坏既有守卫与执行记录。

---

## 1. 现状盘点：19 个工具对智能体的 7 个问题

| # | 问题 | 证据 | 后果 |
|---|---|---|---|
| P1 | **名字太泛，跨 server 冲突** | `list_tools` / `get_tool` / `get_execution` / `capabilities` | WorkBuddy 同时挂 agent-mail、sheetagent、utools 等多个 MCP server。`list_tools` 这种名字会被误选，或让模型误以为是 MCP 元工具 |
| P2 | **粒度太细** | 6 个读工具、3 个 delete 工具、2 个 run 工具 | 一次「看看现状」要 3–4 次调用；工具列表越长，选择准确率越低 |
| P3 | **列表返回全量定义** | `list_tools` 返回每个 Tool 的完整 `input_schema`/`output_schema`/`versions`（`mcp.go:197-208`） | 一个成熟 registry 的 `list_tools` 一次可吃掉数万 token，且 95% 信息当场无用 |
| P4 | **无「先校验后落库」** | `validate_flow` 只收 id（`mcp.go:350`）；`create_*` 失败即整段拒绝 | 只能试错式写入；错误信息无字段级定位 |
| P5 | **无应用生命周期** | 见 §0 硬缺口 1 | 「创建应用」在 MCP 面不成立 |
| P6 | **无引导层** | `mcp.NewServer(..., nil)`（`mcp.go:89`、`:143`），无 instructions/resources/prompts | 智能体只能从 19 段 description 里反推架构；已在 go-sdk v1.8.0 核实 `ServerOptions.Instructions`、`Server.AddResource`、`AddResourceTemplate`、`AddPrompt` **均可用**，是「没用」而非「不能用」 |
| P7 | **无环境自检** | 无 doctor 类工具 | `ext.serve` 起不来时（sidecar 没 build、端口被占、`COORD_ADDR` 不符），智能体只会反复重写 YAML，定位不到环境原因 |

补充两个与体验直接相关的现状：

- **`flow__{id}` 反向暴露是负资产**：启动时快照、新建 Flow 当次不可用（须重启 MCP Server）、与 `run_flow` 语义重复、且 service Flow 恰好又不暴露。它让工具列表**数量不可预测**，直接损害工具选择准确率。
- **错误不可自愈**：错误以文本进 `content` + `IsError=true`（`mcp.go` 各 handler），没有机器可读的 `code`/`field`/`hint`，智能体无法据此精确修正 YAML。

---

## 2. 设计目标与原则

**目标（可度量）**：一次通过率↑、工具调用次数↓、上下文 token↓、人工介入次数↓（度量见 §12）。

**五条原则**

1. **意图对齐**：工具按「智能体想干什么」切分（了解 / 查有什么 / 看定义 / 校验 / 落地 / 运行 / 观察 / 排障），而不是按「内核有哪些资源」切分。
2. **渐进披露**：上下文分三层——常驻（KB 级）/ 按需（工具拉取）/ 运行时（工具返回）。**动态的东西绝不写进静态提示词**。
3. **静态优先**：能离线判定的规则，写进 guide + `gear_validate`，让智能体**在写 YAML 前就知道约束**，而不是运行期撞墙。
4. **单一真相源**：instructions、`gear_guide`、resources 三处文本必须同源（一个 `go:embed`），否则必然漂移——这正是 gear 自己的红线。
5. **不破红线**：MCP 面新增的每个工具都必须落到现有的 registry/engine 函数上，不新写守卫。

---

## 3. 方案总览

```
┌─ L0 注入层 ── ServerOptions.Instructions ──────────────── 常驻 ~900 token
│    心智模型三锚点 + 六条铁律 + 标准工作流 + 工具选择表
├─ L1 上下文层 ─ gear_guide(topic) / gear://guide/* / gear://schema/* ── 按需
│    同一份 go:embed 文本，三处引用（instructions 摘要 / 工具全文 / resources）
├─ L2 工具层 ── 12 个意图级工具（gear_ 前缀）─────────────── 调用面
│    guide · catalog · describe · capabilities · validate · apply
│    delete · promote · run · execution · app · doctor
├─ L3 交付物层 ─ Gear App Bundle（多文档 YAML，kind: App + Tool/Flow/Hook）─ 交付形态
│    静态资源由智能体自身的文件工具落盘；bundle 只声明不搬运
└─ L4 验证层 ── validate(dry) → apply → app start → 探活/日志 → 证据汇报 ─ 闭环
```

---

## 4. L0：注入层 —— 让智能体先建立正确心智模型

**载体**：`ServerOptions.Instructions`（`mcp.NewServer(&mcp.Implementation{...}, &mcp.ServerOptions{Instructions: gearInstructions})`）。这是 MCP 里唯一**所有客户端都保证可达**的常驻上下文（已核实 go-sdk v1.8.0 支持）。

**预算**：≤ 900 token / ≤ 1400 字符，超出部分一律下沉到 L1。理由：instructions 每轮都在上下文里，写成 README 会既贵又漂移。

**内容骨架**（四段：是什么 → 铁律 → 工作流 → 工具选择），完整草稿见 **附录 A**：

1. **是什么**：gear 是机制内核，只有 Tool / Flow / Hook；「应用」= 含 `mode: service` 长驻 step 的 Flow。
2. **六条铁律**：先 `gear_capabilities` 再写 `ext.*`；YAML 先 `validate` 再 `apply`；service 只能 `app start`；凭据只走 `${secrets.NAME}`；id 必须带应用前缀；失败先读日志再改，禁止未验证宣称成功。
3. **标准工作流**：8 步（见 §10）。
4. **工具选择表**：一行一个工具，消除「该调哪个」的猜测。

---

## 5. L1：上下文层 —— 按需展开，三处同源

智能体不需要一次性读完全部规范；它需要**在具体动作前拿到那一页**。

**两个入口，一份内容**：

| 入口 | 可达性 | 用途 |
|---|---|---|
| `gear_guide(topic)` 工具 | 100%（只要有 tools 就能用） | 主入口：智能体主动要手册 |
| `gear://guide/*`、`gear://schema/*` resources | 取决于客户端是否支持 resources | 加分项；不支持也不影响闭环 |

> **为什么必须有 guide 工具兜底**：WorkBuddy 是否把 server instructions / resources 注入模型上下文尚未实测。工具是唯一被证明可用（`mcp_wire_probe.py` 已跑通 tools/call）的通道，所以**内容主体放工具**，instructions 只放摘要。

**Topics 清单**

| topic | 内容 | 何时拉 |
|---|---|---|
| `overview` | 三锚点 + 模型关系图 + CLI/REST/MCP 关系 | 首次接触 |
| `authoring` | Tool / Flow / Hook 的**全字段**语义 + 静态校验规则表 + 正反例 | 写 YAML 前 |
| `service-app` | service 长驻模型、anchor 排序、侧车与端口、`app start` | 做应用时 |
| `atoms` | 7 门禁原语 + 3 纯函数 + `ext.*` 的**用法约定**（参数 schema 走 `gear_capabilities`，避免静态文本过期） | 写 step.call 前 |
| `errors` | 错误码表 + 常见报错 → 修法（附录 D） | 报错后 |
| `examples` | 3 个可直接改的最小骨架（hello / service / 静态站） | 起步 |
| `security` | 凭据、密钥、绑定地址、破坏性操作约定 | 涉及凭据时 |

**实现要点（单一真相源）**：

```go
// internal/mcp/guide/guide.go
//go:embed *.md
var files embed.FS
// instructions = guide/summary.md  全文
// gear_guide(topic) = guide/{topic}.md
// resources        = 同一个 embed.FS 的 gear://guide/{topic}
```

三处渲染自**同一份** `embed.FS`，杜绝「instructions 说的和 guide 说的不一样」。

---

## 6. L2：工具层 —— 19 → 12

### 6.1 迁移对照

| 现有 | 目标 | 变更性质 |
|---|---|---|
| `list_tools` / `list_flows` / `list_hooks` / `list_executions` | **`gear_catalog`**（`kind`: tool\|flow\|hook\|app\|execution\|all，返回**摘要**） | 合并 + 瘦身 |
| `get_tool` / `get_flow` / `get_execution` | **`gear_describe`**（`kind` + `id`，全量定义） | 合并 |
| `create_tool` / `create_flow` / `create_hook` | **`gear_apply`**（bundle 多文档、原子、幂等、可选 `dry_run`） | 合并 + 升级 |
| `validate_flow` | **`gear_validate`**（接收 **YAML 文本**，不落库，返回结构化诊断） | 升级（硬缺口 2） |
| `delete_tool` / `delete_flow` / `delete_hook` | **`gear_delete`**（`kind` + `id`，守卫语义不变） | 合并 |
| `promote_tool` | **`gear_promote`** | 重命名 |
| `run_tool` / `run_flow` | **`gear_run`**（`kind`: tool\|flow，**默认异步**，`wait_seconds` 可选短等） | 合并 + 改造（硬缺口 3） |
| `get_execution_logs`（+ get/list） | **`gear_execution`**（`id`，`with_logs`/`tail`） | 合并 |
| `capabilities` | **`gear_capabilities`** | 重命名 |
| `flow__{id}` | **默认关闭**，`--expose-flows` 显式开启 | 降级为可选 |
| — | **`gear_guide`** | 新增（L1） |
| — | **`gear_app`**（`action`: start\|stop\|status\|list） | 新增（硬缺口 1） |
| — | **`gear_doctor`** | 新增（问题 P7） |

**12 个 = 1 guide + 6 目录/写入 + 2 执行/观察 + 1 app + 1 doctor + 1 promote。**

### 6.2 关键设计点

**① `gear_catalog` 返回摘要，不返回全量**
`{items:[{kind,id,name,description,state?,steps_count?,running?}], total, truncated}`。要看细节走 `gear_describe`。这是把 P3（token 爆炸）一次解决的改动，并让「先看一眼再决定」变成一次调用。

**② `gear_validate` 收 YAML 文本，返回字段级诊断**
入参 `{yaml: string, kind?: "auto"}`；返回

```json
{ "valid": false, "diagnostics": [
  { "code": "UNKNOWN_CALL_TARGET", "doc": 2, "kind": "flow", "id": "myapp.up",
    "step": "site", "field": "steps[1].call", "message": "call 目标不存在: static_site.serve",
    "hint": "先 gear_catalog kind=tool 确认，或用「能力.方法」直调（gear_capabilities 查清单）",
    "docs": "gear://guide/errors#UNKNOWN_CALL_TARGET" } ] }
```

不落库、不改状态，可无限次调用——这是智能体的「编译」，把 fail-closed 从运行期前移到**写作期**。

**③ `gear_apply` 是唯一的写入入口（声明式、原子、幂等）**
入参 `{bundle: string | bundle_path?: string, dry_run?: bool, namespace?: string}`；返回 `{applied:{tools,flows,hooks}, skipped:[], warnings:[]}`。
- 落库顺序由**依赖**决定（Tool → Flow → Hook），不是由文档顺序决定；多文档混排合法。
- 沿用既有原子语义（`REQ-REG-ATOMIC`）：全量校验通过才落库，失败一个不写，并在错误里列出**未写入**与**已存在冲突**项。
- `dry_run: true` 等价于 `gear_validate`，但走完整申请路径（含覆盖检测），便于智能体「先看 diff」。

**④ `gear_run` 默认异步**
入参 `{kind, id, input?|vars?, wait_seconds?: 0}`。默认立即返回 `{execution_id, status: "running"}`；`wait_seconds>0` 时最多等这么久再返回快照。理由：MCP 调用有客户端超时，长 Flow 同步等待必然失败；REST 面早已是 202 + id 的异步模型（`POST /api/executions`），这里只是把 MCP 面对齐到同一语义。
兼容：失败仍**返回记录不抛协议错误**（`REQ-MCP-RUN` 口径不变），智能体从 `status/error` 判断。

**⑤ `gear_app`：应用生命周期（硬缺口 1 的补丁）**

```
gear_app(action: start|stop|status|list, flow_id?, name?, vars?, restart?="always")
→ status: {app_id, flow_id, pid, state: running|stopped|failed, started_at, ports[], log_path}
```

**关键决策：app 必须脱离 MCP 进程生命周期。**
若在 MCP Server 进程内托管（像 REST 的 `AppInstance`），WorkBuddy 关闭 → MCP Server 退出 → 应用被杀，"创建的应用"活不过会话。

推荐实现：`gear_app start` 以**分离进程**方式拉起 `gear app run <flow> --restart always`，并落一份 pidfile（`<registry 同级>/apps/<app_id>.json`：pid / 启动参数 / 日志路径 / 端口）。`stop` 读 pidfile 杀进程树并清理；`status` 校验 pid 存活 + 对声明端口做一次 TCP 探活。

> 备选方案：要求常驻一个 `gear serve` 守护进程，MCP 的 app 工具转发到 `POST /api/apps`。优点是复用现成实现与端口；缺点是强加一个守护进程依赖。**推荐分离进程**，因为它既省依赖，又天然跨会话存活。

**⑥ `gear_doctor`：环境自检（问题 P7）**

```
{ registry: {path, tools, flows, hooks},
  extensions: {config, loaded: ["ext.serve", ...], missing_sidecars: ["build/serve-sidecar.exe"]},
  ports: {8095: "free|in-use(pid)"},
  apps: [{app_id, state, pid}],
  warnings: ["检测到 extensions.yaml 声明 ext.kafka 但 build/kafka-sidecar.exe 不存在 → 该能力不可用"] }
```

智能体排障的第一站；`ext.serve` 起不来时，一次调用就能区分「YAML 写错」与「环境没准备好」。

**⑦ `gear_` 前缀与破坏性变更**
MCP 无别名机制，一次性重命名。代价可控：唯一已知客户端是 WorkBuddy + Claude Desktop + 仓库文档/测试。**Server `Version` 从 `0.1.0` 升到 `0.2.0`**，README §12 与 `REQUIREMENTS.md` 同步。
工具增加 `Annotations`（`ReadOnlyHint`/`DestructiveHint`/`IdempotentHint`）：`gear_validate`/`gear_catalog`/`gear_describe`/`gear_capabilities`/`gear_doctor` = 只读；`gear_delete`/`gear_app(stop)` = 破坏性；`gear_apply` = 幂等。这让客户端能在 UI 上正确提示风险。

**⑧ 协议红线照旧（不许忘）**
新增列表类工具（`gear_catalog`）**必须**经 `objectResult` 包具名键，并登记进 `TestMCPAllToolsReturnObjectStructuredContent` 的 calls 表——该用例会反查覆盖度，漏登记直接 FAIL（README §12.2）。

---

## 7. L3：交付物层 —— Gear App Bundle

「创建应用」需要一个**明确的交付形态**。定义 Gear App Bundle：一个多文档 YAML，第一份是 `kind: App` 清单，其余是标准 Tool/Flow/Hook 文档。

> **不引入 App 实体**：bundle 只是**多文档 YAML 的组织约定 + 依赖声明**，落到 registry 后仍然只有 Tool/Flow/Hook。这保持了 PRD 的非目标（「只有 Flow + `app run`」，`PRD_v40.md:60`）与 `GEAR_工程方法_v1.md` 的单一真相源原则。

```yaml
kind: App                       # ← 唯一新增的文档类型（仅 apply/validate 识别，不入 registry）
id: mynews
name: 我的新闻站
description: 抓热搜 + 静态站托管
entry_flow: mynews.up           # 应用入口（必填，须为本 bundle 或已注册的 Flow）
layout:
  workdir: apps/mynews          # 约定工作目录（智能体自身文件工具落盘到此）
  files:                        # 需要落盘的文件（apply 只校验存在性，不搬运）
    - {path: apps/mynews/www/index.html, required: true}
requires:
  secrets: [NEWS_API_KEY]       # 需要人播种的密钥名（apply 只检查是否已存在）
  sidecars: [serve]             # 依赖的侧车 → 校验 build/serve-sidecar.exe
  ports: [8080]                 # 端口占用检查（doctor/apply 都会看）
  capabilities: [http, ext.serve]
---
kind: Tool
id: mynews__fetch
...                             # 标准 Tool 文档
---
kind: Flow
id: mynews.up
steps:
  - {id: anchor, call: mynews__fetch, input: {}}
  - {id: serve, call: ext.serve, mode: service, input: {}}
  - {id: site, call: static_site.serve, depends_on: [anchor],
     input: {port: 8080, dir: apps/mynews/www, host: 127.0.0.1}}
```

**它同时回答了「创建应用需要哪些上下文」**：id 命名空间、文件清单、密钥清单、侧车与端口、能力声明——一份清单式契约，人机都读得懂。

**边界（写清楚，避免智能体期待错误）**
- **MCP 不提供文件读写**。静态资源由智能体**自身的文件工具**落盘；bundle 只声明 `layout.files`，`gear_apply` 校验其存在性（缺了就在 `warnings` 里报「文件未就绪」）。理由：MCP 不该重复客户端已有的文件能力，且能减少一大类权限面。
- **命名空间**：`gear_apply` 强制 bundle 内所有 id 以 `<app id>__` 或 `<app id>.` 开头（不符即拒），避免智能体覆盖你手工维护的 Tool/Flow。这是防「一次性生成污染 registry」的关键约束。
- **幂等**：apply 是 upsert；响应里区分 `created` / `updated`，让智能体知道自己改动了什么。

---

## 8. 需要向 WorkBuddy 提供的上下文信息（清单）

| 层 | 内容 | 体量 | 来源（单一真相源） | 加载时机 |
|---|---|---|---|---|
| **常驻** | 心智模型 3 锚点、6 铁律、8 步工作流、工具选择表 | ≤900 token | `guide/summary.md`（embed） | instructions，会话建立时 |
| **按需** | 全字段语义、静态校验规则、service 模型、错误码表、3 个骨架示例、安全约定 | 每个 topic 0.5–2 KB | `guide/*.md`（同一 embed） | `gear_guide(topic)` |
| **运行时** | 能力目录（含参数 schema）、对象摘要/全量、校验诊断、执行记录与日志、环境自检 | 按需，可能较大 | `gear_capabilities` / `catalog` / `describe` / `validate` / `execution` / `doctor` | 动作前 |
| **仓库内**（由智能体自己的文件工具读） | `examples/*.yaml`、`plugin/static_site_demo.yaml`、`plugin/web_ui_flow.yaml`、`README.md` §12 | 大，按需 grep | 磁盘 | 需要范例或排查文档漂移时 |

**明确不要放进上下文的东西**：

| 不放 | 原因 | 替代 |
|---|---|---|
| `capabilities` 全文 | 随 `extensions.yaml` 变化，静态写必然过期；体量大 | `gear_capabilities` 工具实时取 |
| `extensions.yaml` 全文 | 含环境相关配置，且能力清单已有权威出口 | `gear_doctor` + `gear_capabilities` |
| 完整 README / PRD | 数万 token，与任务无直接关系 | `gear_guide` topic + 需要时读文件 |
| 全部 Tool/Flow 定义 | token 爆炸 | `gear_catalog` 摘要 → `gear_describe` 单选 |

---

## 9. 约束条件

### 9.1 协议硬约束（MCP 面）

1. `structuredContent` **顶层必须是 JSON 对象**；列表类经 `objectResult` 包具名键；新工具必须登记进覆盖度反查用例。
2. 执行失败**返回记录（`status: failed`）而非协议错误**；只有「对象不存在 / 参数非法 / 前置条件不满足」才 `IsError=true`。两类形态必须一致，否则智能体判断逻辑会分裂。
3. stdio 下 stdout 只承载协议帧，日志走 stderr。
4. 工具粒度约束：**一个工具最多一件事**；描述首行必须是「做什么 + 何时用」，示例不超过 8 行（避免把 description 写成文档）。

### 9.2 领域硬约束（智能体必须遵守，`gear_validate` 会逐条拦）

| 约束 | 具体规则 | 现有守卫 |
|---|---|---|
| runtime 白名单 | `process` / `lua` / `go-host`；`wasm` 未实现，注册即拒 | `model.ImplementedRuntimes` |
| capability 门禁 | 用 `ext.*` 必须在 Tool 里声明；7 原语外的能力不可声明 | `cap.GrantsFromStrings` |
| call 目标必须存在 | Tool id 或「能力.方法」，否则拒 | `engine.ValidateFlowStatic` |
| `when` 是表达式不是模板 | 写 `steps.s1.output.n > 1`，**不能写 `${}`** | 静态编译期 |
| 模板前缀三选一 | `${vars.x}` / `${steps.id.output.x}` / `${secrets.NAME}` | 明文白名单校验 |
| 依赖必须无环 | `depends_on` + 隐式依赖合成图无环 | `topoLevels` |
| service step 必须 app 托管 | 含 `mode: service` 的 Flow 不能 `flow run` | `REQ-FLOW-RUN-REJECTS-SERVICE` |
| 「能力.方法」直调的限制 | 不支持 `use_version` 与 `mode: service` | `resolveStepCall` |
| service step 不能作为依赖目标 | 需要 once 步做「锚点」把后续步推下一层 | `web_ui.up` 的 `anchor` |
| schema 必须是标准 JSON Schema | 顶层 `type: object`；不支持 `{"x":"string"}` 简写 | `Tool.Validate` |

### 9.3 安全约束

- MCP 的 `--http` **只绑 127.0.0.1**，文档明确警告不要暴露公网（当前无鉴权）。
- **密钥不回显**：任何工具都不返回密钥明文；bundle 只声明 `requires.secrets` 的名字。
- **凭据只从调用参数传**：`ext.*` 原子禁止读环境变量（`TestExtAtomsMustNotReadEnv` 机械守卫）；智能体必须用 `${secrets.NAME}` 而非内联明文。
- **破坏性操作**（`gear_delete`、`gear_app stop`、`gear_promote → deprecated`）在 description 中标注，并要求智能体先列出影响面。
- **不做「智能体自动播种密钥」**（§13 开放问题）：密钥值由人在 CLI 播种，这是设计上保留的人机交接点。

### 9.4 环境约束

- 侧车须先构建（`bash scripts/build.sh` → `build/*-sidecar.exe`）；`gear_doctor` 负责报告缺失。
- 端口必须唯一且未被占用；`listen` 是协调地址的唯一真相源（别另写 addr）。
- registry 默认 `~/.gear/registry.db`，MCP 与 CLI/REST 共享 —— 智能体的改动会与人的改动**互相可见**，所以命名空间约束（§7）是必须的。
- 相对路径依赖 cwd：`web_ui.up` 的 `dir: build/dist` 要求从仓库根运行。bundle 应尽量用**绝对路径或 `layout.workdir` 下的相对路径**，`gear_validate` 对「相对路径 + cwd 不明」给 warning。

### 9.5 交付纪律（写进 instructions 的铁律）

智能体宣称"完成"必须附**三段证据**：① `gear_validate` 的 `valid: true`；② `gear_apply` 的 `applied` 清单；③ `gear_app status` 的 `running` + 端口探活结果（或 `gear_execution` 的 `status: succeeded` + 关键日志行）。禁止「看起来跑通了」。

---

## 10. 标准交互时序（创建应用 8 步）

```
① gear_doctor                      → 环境是否就绪（sidecar/端口/registry）
② gear_guide("service-app")        → 拿操作手册
   gear_capabilities               → 确认可用原子与参数（尤其 ext.*）
③ 【自身文件工具】落盘静态资源到 layout.workdir
④ gear_validate(bundle)            → valid:true 才继续（失败→按 hint 改→回到 ④，可无限次）
⑤ gear_apply(bundle)               → applied: {tools, flows}；冲突/缺文件在 warnings
⑥ gear_app(start, flow_id)         → app_id + pid + state
⑦ gear_app(status) / gear_execution(with_logs) / TCP 探活 → 确认真的在跑
⑧ 汇报：bundle 摘要 + 访问地址 + 三段证据 + 停止方法
```

**自愈回路**：任何一步失败 → 读 `diagnostics.code` → `gear_guide("errors")` 查修法 → 改 YAML → 回 ④。这条回路是「高效」的核心：智能体不需要问人，也不需要猜。

---

## 11. 落地路线

### P0 —— 让「创建应用」成立（无此三项，任务不可能完成）

| 项 | 改动点 | 验收 |
|---|---|---|
| `gear_app`（start/stop/status/list，分离进程 + pidfile） | `internal/mcp/mcp.go`、新增 `internal/mcp/app.go`；复用 `internal/app.Supervisor` 或 spawn `gear app run` | 新测试：起一个含 `service` step 的 Flow → status running → 端口可连 → stop 后端口释放；**MCP Server 退出后 app 仍存活**（跨会话存活用例） |
| `gear_validate`（收 YAML、字段级诊断、不落库） | 抽 `engine.ValidateFlowStatic` 的诊断结构（现为 error）；新增 `gear_validate` | 用 `plugin/web_ui_flow.yaml` 故意改坏 5 处 → 5 条诊断各自带 `field`+`code`；注册表未被改动（断言 count 不变） |
| `gear_apply`（bundle 原子 + namespace 校验 + dry_run） | `internal/mcp/mcp.go`，复用 `registry.ParseYAMLDocs` / `Upsert*` / `ValidateFlowStatic` | 多文档混排（App+Tool+Flow+Hook）一次落库；第 3 份非法 → 前两份也不写入；id 不带前缀 → 拒 |

**同步交付**：`ServerOptions.Instructions`（附录 A）+ `gear_guide` 工具与 embed（附录 B 为起点）。

### P1 —— 让「高效」成立

- 工具重命名 + 合并（19 → 12）+ `Annotations`，`Version → 0.2.0`。
- `gear_catalog` 摘要化（P3 的根治）。
- `gear_run` 异步化（`wait_seconds`）。
- `gear_doctor`。
- resources（`gear://guide/*`、`gear://schema/*`）。
- `flow__{id}` 默认关闭（保留 `--expose-flows`）。
- 结构化错误契约（附录 D）覆盖全部工具。

### P2 —— 让「可度量」成立

- 智能体 eval 场景集（§12）纳入 `plugin/verify.py` 或新增 `plugin/agent_eval.py`。
- 恢复 **`plugin/mcp_wire_probe.py`**（见 §12 注，当前文档引用但文件缺失）。
- 可选：`gear_secrets(action: list)`（只回名字）、`ext.ai` 与 apply 的联动（生成 → 校验 → 落地闭环）。

### 文档同步点（改代码必同步，项目约定）

| 文件 | 同步内容 |
|---|---|
| `README.md` §12 | 工具表（19 → 12）、instructions 说明、`--expose-flows`、bundle 示例 |
| `REQUIREMENTS.md` | 新增 `REQ-MCP-APP` / `REQ-MCP-APPLY` / `REQ-MCP-VALIDATE-YAML` / `REQ-MCP-INSTR` / `REQ-MCP-NAME` / `REQ-MCP-ERR` / `REQ-MCP-DOCTOR`，各带测试锚点 |
| `internal/mcp/mcp_test.go` | 新工具全部登记进 `TestMCPAllToolsReturnObjectStructuredContent`（反查覆盖度，漏登记即红） |
| `MCP_AGENT_DESIGN.md`（本文） | 实现落地后回填状态 |
| `plugin/verify.py` | 端到端红线追加「app start / app 跨会话存活」场景 |

---

## 12. 度量与验收

**6 个 eval 场景**（每个都从空 registry 起，记录工具调用次数与 token）

| # | 场景 | 通过判据 |
|---|---|---|
| E1 | 最小一次性编排（hello） | `run` 成功，无人工介入 |
| E2 | 静态站点应用（`ext.serve` + 静态目录） | 端口可访问，返回页面内容 |
| E3 | 常驻应用（service flow，两个长驻 step + anchor） | `app status: running`；**关闭 MCP Server 后仍在跑** |
| E4 | 带密钥的通知 Flow（`ext.email` + `${secrets}`） | `validate` 能指出未播种的密钥；人播种后跑通 |
| E5 | 断言失败自愈（故意给坏 YAML） | 智能体在 ≤3 次迭代内自行修好（靠 `code`+`hint`，不靠问人） |
| E6 | 幂等与命名空间 | 同 bundle 二次 apply → `updated` 而非重复创建；无前缀 id → 被拒 |

**4 个指标**：一次通过率、平均工具调用次数、单任务 token 消耗、人工介入次数（目标：E1–E3、E6 为 0）。

**红线**：任何一次「未验证就宣称成功」直接记失败（对应 §9.5）。

> **注（现存文档漂移，需一并修）**：`README.md:597/603` 与 `REQUIREMENTS.md:116` 引用 `plugin/mcp_wire_probe.py`（MCP 线上帧取证，stdio + `--http` 双模式），但该文件**当前不存在**（磁盘仅 `plugin/verify.py`）。它是验证「WorkBuddy 连接器是否真能收下响应」的唯一工具，P2 必须恢复。另 `README.md:550` 提到 `examples/ai/recipes/*.yaml`，该目录亦不存在。

---

## 13. 风险与开放问题

| # | 问题 | 影响 | 建议 |
|---|---|---|---|
| R1 | **WorkBuddy 是否消费 instructions / resources 未实测** | 若都不消费，L0/L1 只剩 `gear_guide` 工具一条路 | 先用 `plugin/mcp_wire_probe.py` 抓真实握手帧确认；**内容主体始终放工具**（不把可达性押在客户端行为上） |
| R2 | 工具重命名是破坏性变更 | 既有客户端配置失效 | 一次性做完并升 `Version`；README 顶部加迁移说明 |
| R3 | app 分离进程的孤儿治理 | MCP Server 崩溃后应用仍在跑，无人知晓 | pidfile + `gear_app status` + `gear_doctor` 可见；启动时检测上次遗留并汇报（不静默杀） |
| R4 | `gear_apply` 幂等的边界 | 人工改过的 Flow 被智能体覆盖 | 响应区分 created/updated；`force` 默认 false，覆盖已存在且内容不同者需显式 `overwrite: true` |
| R5 | 异步 `gear_run` 带来的轮询成本 | 智能体可能空转 | `wait_seconds` 折中（默认 0，建议 agent 用 15–30s 短等）+ `gear_execution` 一次返回 `with_logs` |
| R6 | 是否开放密钥写入 | 安全 vs 自助闭环 | **建议不开放**；bundle 声明 `requires.secrets`，人用 CLI 播种，形成明确的交接点 |
| R7 | 工具数继续膨胀 | 准确率再次下降 | 把「工具数 ≤ 12」写成 MCP 面的硬预算：新增必须合并或先删一个 |

**待你拍板的 3 件事**：① app 用分离进程还是复用 `gear serve` 转发（§6.2⑤）；② 是否接受一次性的破坏性重命名（§6.2⑦）；③ 密钥是否坚持人工播种（R6）。

---

## 附录 A：`Instructions` 草稿（可直接落地，≤900 token）

```
Gear 是一个「机制内核」：只提供 Tool（原子能力）、Flow（编排）、Hook（切面），不内置业务策略。
一个「应用」= 一个含 mode: service 长驻 step 的 Flow，由 gear_app 托管运行。

三句话锚点
1. 模型只有三者：Tool=能做什么、Flow=谁先谁后/何时跑/是否长驻、Hook=生命周期插入与门禁。
2. 一处定义、多入口执行：registry 是唯一真相源，CLI/REST/MCP 只是同一执行面的不同协议入口。
3. 能静态校验的绝不留到运行期：未知 runtime、未知能力、when 语法、模板前缀、依赖环都在注册期拒绝（fail-closed，不静默降级）。

创建应用的标准流程
① gear_guide 读手册 → ② gear_capabilities 确认可用原子 → ③ 用你自己的文件工具落盘静态资源
→ ④ gear_validate 校验 bundle（不落库）→ ⑤ gear_apply 原子落库 → ⑥ gear_app start 起应用
→ ⑦ 探活 + 读日志 → ⑧ 汇报（含 execution id / 日志 / 探活结果）。

六条铁律
1. 写 step.call 的「能力.方法」直调前，必须先 gear_capabilities，不许凭记忆猜方法名或参数。
2. 任何 YAML 先 gear_validate，再 gear_apply；不要「先写进去再改」。
3. 含 mode: service 的 Flow 只能用 gear_app start 运行；gear_run 会拒绝它。
4. 凭据只走 ${secrets.NAME}，绝不内联明文；扩展原子禁止依赖环境变量。
5. 所有 id 必须以 <app-id>__ 或 <app-id>. 开头，避免覆盖他人已注册的 Tool/Flow。
6. 失败先读 gear_execution 的日志定位到具体 step，再改 YAML 重跑；禁止未验证就宣称成功。

工具选择
- 想了解 gear 怎么用        → gear_guide
- 想看现有什么（摘要）      → gear_catalog
- 想看某对象完整定义        → gear_describe
- 有哪些原子能力与参数       → gear_capabilities
- 校验 YAML（不改动任何东西）→ gear_validate
- 落地/更新定义（原子、幂等）→ gear_apply
- 跑一次性编排 / 看结果      → gear_run / gear_execution
- 起停查长驻应用            → gear_app
- 环境不对（侧车缺失、端口占用）→ gear_doctor
```

## 附录 B：`gear_guide("overview")` 内容骨架

1. gear 是什么 / 不是什么（机制 vs 策略）
2. 三模型关系（Tool 声明能力、Flow 编排、Hook 切面）+ 最小 Flow YAML
3. 执行语义速查：同层并行（`maxParallel` 默认 4）、`when` 假=跳过（非失败）、失败即取消后续层、`${}` 模板 vs 表达式
4. 三种执行面的关系（CLI / REST / MCP 同源，MCP 只做协议翻译）
5. 「应用」的定义与 `web_ui.up` 四步范式（service + anchor + once 下发 `/start`）
6. 常见误解 7 条（附录 E 的「常见误解」小节）

## 附录 C：`gear.app.yaml` 完整示例（可跑的最小静态站应用）

```yaml
kind: App
id: mynews
name: 我的新闻站
entry_flow: mynews.up
layout:
  workdir: apps/mynews
  files:
    - {path: apps/mynews/www/index.html, required: true}
requires:
  sidecars: [serve]
  ports: [8080]
  capabilities: [ext.serve]
---
kind: Tool
id: mynews.config
name: mynews 配置锚点
description: 应用配置锚点（占位，保证 depends_on 有稳定上游）
runtime: lua
input_schema: {type: object, properties: {}}
output_schema: {type: object}
versions: [{version: "1.0.0", state: stable}]
entry: |
  return {}
---
kind: Flow
id: mynews.up
name: mynews 起服
description: 用 ext.serve 托管静态目录
steps:
  - id: anchor
    call: mynews.config
    input: {}
  - id: serve                      # 长驻：持有 serve-sidecar，使其不被回收
    call: ext.serve
    mode: service
    input: {}
  - id: site                       # 一次：依赖 anchor → 落下一层 → 侧车必已就绪
    call: static_site.serve        # 需先注册该 Tool（见 plugin/static_site_demo.yaml）
    depends_on: [anchor]
    input: {port: 8080, dir: apps/mynews/www, host: 127.0.0.1}
```

## 附录 D：错误码表（结构化错误契约）

```json
{ "error": { "code": "...", "message": "...", "field": "...", "hint": "...", "docs": "gear://guide/errors#CODE" } }
```

| code | 触发 | hint 要点 |
|---|---|---|
| `UNKNOWN_CALL_TARGET` | `step.call` 目标不存在 | 用 `gear_catalog(kind=tool)` 确认，或改用「能力.方法」（先 `gear_capabilities`） |
| `UNKNOWN_CAPABILITY` | Tool 声明了不存在的 capability | 先 `gear_capabilities`；`ext.*` 需在 `extensions.yaml` 中注册 |
| `BAD_TEMPLATE` | `${}` 前缀非法 | 只允许 `vars` / `steps.<id>.output` / `secrets`；`when` 不写 `${}` |
| `SCHEMA_NOT_OBJECT` | schema 顶层非 object | 写标准 JSON Schema，不支持简写 |
| `DEP_CYCLE` | 依赖成环 | 检查 `depends_on` 与 `${steps.*}` 隐式依赖 |
| `SERVICE_FLOW_SYNC_RUN` | 对 service Flow 用 `gear_run` | 改用 `gear_app(action=start)` |
| `ID_NAMESPACE_VIOLATION` | id 未带应用前缀 | 全部 id 以 `<app-id>__` 或 `<app-id>.` 开头 |
| `MISSING_FILE` | `layout.files` 声明但不存在 | 用你的文件工具先落盘 |
| `MISSING_SECRET` | `${secrets.X}` 对应密钥未播种 | 请人在 CLI 播种（MCP 不写密钥） |
| `SIDECAR_NOT_BUILT` | 声明的侧车二进制缺失 | 先构建（`build/*-sidecar.exe`） |
| `PORT_IN_USE` | 端口被占 | 换端口或先停占用应用（`gear_doctor` 可见） |
| `IN_USE` | 删除被引用对象 | 错误信息会指出被谁引用，先解引用 |

## 附录 E：智能体常见误解（写进 `gear_guide`，直接减少返工）

| 误解 | 事实 |
|---|---|
| gear 有 App 实体，可以「创建应用」 | 没有。应用 = 含 `mode: service` step 的 Flow，由托管器运行 |
| `gear_run` 可以起长驻服务 | 不行，会被拒；必须 `gear_app start` |
| 扩展原子的凭据可以从环境变量读 | 不行，只从调用参数传；用 `${secrets.NAME}` |
| 写 `{"x": "string"}` 就算 schema | 不行，必须标准 JSON Schema 且顶层 `type: object` |
| `when` 里写 `${...}` | 不行，`when` 是表达式；`${}` 只用于 input |
| 侧车会自动被拉起 | 需要 `mode: service` 的 step 持有它；后续 step 需经 anchor 排到下一层 |
| 新建 Flow 立刻能作为 `flow__{id}` 调 | 那是启动时快照；且该暴露面默认将关闭，用 `gear_run` |
| 相对路径随便写 | 依赖进程 cwd；用绝对路径或 `layout.workdir` 下的路径 |
