# AI 头脑风暴 —— 在 Gear 上的实现方案

> 本文不是纸面设计：方案里的每一步都**已经用 gear 的 MCP 面真实跑通**（除 LLM 凭据外）。
> 过程中卡住的 8 处问题在 §6 逐条列明（每条含复现路径），并**已于同日全部修复**——修复落点见 §9。
> 实证时间 2026-09-22。

## 1. 目标形态

一个**单页、可交互、带 AI 生成**的头脑风暴工具：

- 输入主题（+ 可选视角约束），一键生成 N 条想法卡片
- 对任意卡片点「深化」→ 展开 4 条具体落地动作
- 一键「聚类归纳」→ LLM 把散点想法归成 3–5 个主题簇
- 导出 Markdown

产品原则：**过程可见**（页面保留最近一帧 AI 调用明细）、**Key 归用户**（BYOK，只存本机 localStorage，不经服务端落盘）。

## 2. 在 Gear 四层模型里的落点

| Gear 概念 | 本方案的用法 |
|---|---|
| **能力 / 原子** | `ext.ai.complete`（LLM 补全，`format=json` 出结构化）、`ext.serve`（静态站 + 反代） |
| **Tool** | `brainstorm__config`（排序锚点）、`brainstorm__api`（process 长驻，起 REST 后端） |
| **Flow** | `brainstorm.up`：一个 Flow 同时管两个长驻服务 |
| **App** | `kind: App` 清单（`entry_flow` + `start.ports` + `requires`），**只是交付形态，不入 registry** |
| **Hook** | 本期未用（可用于「每次生成后自动归档」） |

## 3. 架构：为什么是「静态站 + 反代」这一条路

Gear 本身不是 Web 框架——它没有「把 HTTP 请求路由到某个 Tool 并同步返回」的能力
（2026-09-22 已补上 `POST /api/tools/{id}/run`，但**静态托管 + 同源**这条路依然是做界面的首选）。`ext.serve` 提供了 nginx 式的两件套：**托管静态目录** + **按前缀反代**。于是：

```
浏览器 ──GET /──────────────► ext.serve (127.0.0.1:8088)
        │                        └─ 静态文件：apps/brainstorm/www/
        └─POST /api/**────────► 反代 ──► gear REST 执行面 (127.0.0.1:8788)
                                          └─ POST /api/capabilities/invoke
                                             → ext.ai.complete → 模型厂商
```

**同源**：页面与接口都在 8088，天然无跨域。`/api` 前缀命中反代，其余回落静态文件。

一个 Flow 管住两个长驻服务，靠 `mode: service` + **一次锚点**排序：

```yaml
steps:
  - {id: anchor, call: brainstorm__config}        # once：只为把后续推到下一层
  - {id: serve,  call: ext.serve, mode: service} # 长驻：持有 ext.serve 侧车
  - {id: api,    call: brainstorm__api, mode: service}  # 长驻：REST 后端
  - {id: site,   call: static_site.serve, depends_on: [anchor],
     input: {port: 8088, dir: …, proxy: [{path: /api, target: http://127.0.0.1:8788}]}}
```

> service step 不能作为依赖目标（长驻没有「完成」信号），所以 `site` 必须依赖 `anchor` 落到下一层，
> 否则它可能与侧车握手抢跑 → `connection refused`。

## 4. 落地步骤（照做即可复现）

```bash
# 0) 前置：侧车已构建、端口空闲 —— 先自检（现已含 AI 凭据就绪项）
gear_doctor()

# 1) 写静态页（MCP 不管文件，用自己的文件工具落盘）
#    apps/brainstorm/www/index.html

# 2) 校验 → 落库 → 启动
gear_validate(bundle)          # valid: true（现已静态拦截不可达的 process entry）
gear_apply(bundle)             # created: tool×2, flow×1
gear_app(action=start, flow_id="brainstorm.up", ports=[8088, 8788], wait_seconds=12)
# → state: running, ports: [{8088,true},{8788,true}]

# 3) 自检链路
curl http://127.0.0.1:8088/                 # 200 + 页面
curl http://127.0.0.1:8088/api/healthz      # {"flows":10,"status":"ok","tools":27}  ← 反代通
```

AI 调用（页面里的实际请求）：

```http
POST /api/capabilities/invoke
{"name":"ext.ai","method":"complete",
 "args":{"prompt":"…","format":"json","temperature":0.9,"api_key":"sk-…"}}
```

> 该端点现已加 Origin 守卫（只接受本机来源）并禁止 `secret.*`（403），见 §6.5。

## 5. 实证结果（2026-09-22）

| 环节 | 结果 |
|---|---|
| `ext.ai` 能力可用 | ✅ `complete` / `generate` / `react` / `catalog` 等 7 个方法在能力目录里 |
| Tool（lua 调 `ctx.ext.ai.complete`）校验 + 落库 | ✅ `brainstorm__ideate` 一次通过 |
| App bundle 校验 + 落库 | ✅ 一次通过 |
| `gear_app start` | ✅ `running`，双端口均 LISTENING，**跨工具调用仍存活**（真脱离了 Job） |
| 静态页 | ✅ HTTP 200 / 13350 字节 |
| `/api` 反代 → REST | ✅ `8088/api/healthz` 返回 `{"status":"ok"}` |
| 反代 → `ext.ai.complete` → 厂商 | ✅ 请求真打到厂商（返回 `401 Token is invalid`，因用的是假 Key） |
| 端到端 AI 生成 | ⛔ **缺 LLM API Key**，见 §6.4 |

**结论：架构完全跑通，只差一把 Key。**给了 Key 就能真正生成想法。

## 6. 卡住的地方 = MCP / 内核面的缺陷清单

按严重度排序，每条都有复现路径。**8 条全部已于 2026-09-22 修复**（§6.9 是边界说明，非缺陷）。

| # | 严重度 | 问题 | 状态 |
|---|---|---|---|
| 6.1 | P0 | 命令解析器吃掉反斜杠，Windows 绝对路径必失败 | ✅ 已修 |
| 6.2 | P1 | `gear_guide` 手册里没有 REST 端点契约 | ✅ 已修 |
| 6.3 | P1 | REST 没有「同步执行单个 Tool」的端点 | ✅ 已修 |
| 6.4 | P1 | AI 凭据：MCP 既不能配置，也不做诊断 | ✅ 已修（诊断） |
| 6.5 | P1 | 能力直调端点无鉴权 | ✅ 已修 |
| 6.6 | P2 | `gear_doctor` 的 `missing_sidecars` 返回空对象 | ✅ 已修 |
| 6.7 | P2 | 台账僵尸堆积，无批量清理入口 | ✅ 已修 |
| 6.8 | P2 | `db.query` 空结果回程变 `{}` | ✅ 已修 |

### 6.1 【P0】命令解析器吃掉反斜杠 —— Windows 绝对路径必失败

```
process entry: "D:\gadern\gear\build\gear.exe serve --addr 127.0.0.1:8788"
实际解析为：     D:gaderngearbuildgear.exe
报错：          executable file not found in %PATH%   ← 误导：根本不是 PATH 的问题
```

- 影响：`runtime: process` 的 Tool 在 Windows 上**无法使用绝对路径**，只能用 `./gear.exe` 或正斜杠。
- 连带：`gear_validate` / `gear_apply` **都不拦**，要到运行时才炸，且错误信息指向错误方向，排查成本高。
- 建议：① 命令解析在 Windows 上不把 `\` 当转义符；② `gear_validate` 对 process entry 做**可执行文件可达性**静态检查，给独立诊断码（如 `ENTRY_EXE_UNRESOLVED`）而不是等到运行时。
- ✅ **已修**：`internal/cmdline/parse.go` 引号外只对「空白 / 引号 / `$` / 反引号」转义，`\` 不再被无条件吃掉（`\\` 刻意不折半，保 UNC 路径；行末孤立反斜杠保留字面量）；新增 `ResolveExecutable` + 诊断码 `ENTRY_EXE_UNRESOLVED`，`gear_validate` 对**绝对路径** entry 做存在性检查（相对路径/裸名字只给 warning，因解析基准可能随 cwd 变）。

### 6.2 【P1】`gear_guide` 手册里没有 REST 端点契约

做带界面的应用必然要让前端调 `/api/**`，但 8 个手册主题（overview/authoring/service-app/atoms/errors/examples/security/primitive-gap）
**没有一个给出 REST 端点表**。`/api/capabilities/invoke` 这种关键端点，智能体只能去翻仓库 README 才知道。

- 建议：新增 `gear_guide(topic="rest-api")`，或直接把端点表纳入 `internal/mcp/guide/`（与 Instructions 同源，不重复维护）。
- ✅ **已修**：新增第 9 个主题 `internal/mcp/guide/rest-api.md`（go:embed 自动可达，无需映射表），含地址启动、端点总表、`/api/tools/{id}/run` 与 `/api/executions` 的取舍、安全边界、同源反代架构、以及「静态资源不归 MCP 管」的边界说明。

### 6.3 【P1】REST 没有「同步执行单个 Tool」的端点

- MCP 有 `gear_run(kind=tool)` —— 同步执行 Tool 并直接返回输出。
- REST 只有 `POST /api/executions`（**异步**，跑 Flow，202 + 轮询）。

于是前端想复用一个 Tool（比如封装好 prompt 的 `brainstorm__ideate`）只剩两条路：
包成 Flow + 202 + 轮询（笨重），或**裸调原子能力**（绕过 Tool 封装，prompt 只能写在前端 JS 里）。
本方案被迫选了后者。

- 建议：加 `POST /api/tools/{id}/run`（同步），让 REST 与 MCP 能力对齐。
- ✅ **已修**：新增 `POST /api/tools/{id}/run`（body = 入参 map，空 body = 零参执行，返回 `{id, output}`；先查 registry 判存在性，缺 id 404、入参非法 400）。**本页待 §7.3 改造后可改走 Tool**，prompt 收回 Tool 封装。

### 6.4 【P1】AI 凭据：MCP 既不能配置，也不做诊断

- MCP 不写密钥（设计如此，认同）；但 `gear_doctor` **也不检查** `GEAR_AI_API_KEY` / secrets / provider 是否就绪。
- 结果：只能等业务跑起来撞到厂商原始报错 —— `LLM HTTP 401: {"code":30014,"message":"Token is invalid."}`。
  这个错误既没说「缺哪个变量」，也没说「去哪配」。

- 建议：`gear_doctor` 增 AI 就绪项（provider / model / key **是否存在**，**不回显明文**），并给出「用 `gear secret set` 播种或设 `GEAR_AI_API_KEY`」的明确指引。
- ✅ **已修**：`gear_doctor` 响应新增 `ai` 块（`provider` / `model` / `key_from_env` / `ready` / `blocked` / `how_to_fix`），**只报存在与否、绝不回显明文**；未就绪时加一条 warning。写密钥的边界不变（MCP 不写，人在 CLI/UI 播种）。

### 6.5 【P1】能力直调端点无鉴权 —— 做 UI 应用时这个面直接暴露给浏览器

`POST /api/capabilities/invoke` 可以调**任意**能力，实测：

- `{"name":"secret","method":"get","args":{"name":"…"}}` → 端点存在（本次无密钥故报「secret 不存在」，**一旦播种了密钥即可读出明文**）
- `{"name":"db","method":"query","args":{"sql":"…"}}` → 200，可执行任意 SQL
- 任意 Origin 的请求均 200 放行，无鉴权、无 CORS 白名单

绑 `127.0.0.1` 只防外部网络，不防本机任意进程，也不防浏览器跨站简单请求。而「做一个 Web 应用」恰恰会把这个端点暴露给页面。

- 建议：① invoke 端点加鉴权或能力白名单；② **禁止 `secret.*` 经 REST invoke 调用**；③ 至少加 Origin 校验。
- ✅ **已修**：③ Origin 守卫（带 Origin 且非本机 127.0.0.1 / localhost / `::1` → 403；**无 Origin 的 curl / 服务端调用放行**，不误伤）；② `secret.*` 一律 403（密钥只能经 Flow 内 `${secrets.N}` / `ctx.secret.get` 使用，写入走 `/api/secrets/{name}`）。① 完整鉴权未做——**本机 127.0.0.1 绑定仍是唯一防线**，这是有意的取舍（详见 §7 待办）。

### 6.6 【P2】`gear_doctor` 的 `missing_sidecars` 返回空对象

```json
"missing_sidecars": [{}, {}, {}]
```

结构化字段丢了内容（真正的信息在 `warnings` 文本里，机器不可消费）。上一轮给它加的 `searched` 字段也没出现在输出里。

- ✅ **已修**：`missing_sidecars` 字段导出并加 json tag，现返回 `{ext, expected, searched}`，机器可消费。

### 6.7 【P2】台账僵尸堆积，无批量清理入口

本次 `gear_doctor` 列出 **12 个 `smoke.up` 遗留应用**（`startup_failed` / `unknown` / `exited` 混杂），
`gear_app` 只有按 `app_id` 单个 `stop` 的入口。清理 12 次要 12 次调用，且无从按 state 过滤。

- 建议：`gear_app(action="prune", …)` 或 `list` 支持按 state 过滤 + 批量 stop。
- ✅ **已修**：新增 `gear_app(action="prune")`，支持 `dry_run`（先看会删什么）与 `flow_id` 过滤。**只清 `exited` / `stopped` / `startup_failed` 三种终态，`running` / `unknown` 一律保留**（保守边界：`unknown` 可能还活着，宁可留）；只删台账记录，**不杀进程**。
  同日补齐三面对齐（见 §9 的 6.7 行）：CLI `gear app prune [--dry-run] [--flow <id>]`、REST `POST /api/apps/prune`，三者共用 `internal/app` 同一份 `Prune`。**注意 REST 的 `GET/POST/DELETE /api/apps` 管的是进程内存里的实例（等价 `gear app run`），与本端点动的落盘台账不是同一个模型**。

### 6.8 【P2】`db.query` 空结果回程变 `{}`

`{"result":{}}` —— 空行集被压成空对象（Lua 空数组回程的老坑），调用方无法区分「查到 0 行」与「出错」。

- ✅ **已修**：`internal/engine/lua.go` 给 Go→Lua 的 slice 挂**显式数组标记**（放 metatable，不污染数据），回程判定改为「显式标记优先」。空行集现回 `[]`，`{}` 仍如实表示空 map。

### 6.9 【边界，非缺陷】MCP 不提供文件读写

静态资源必须用自己的文件工具落盘，bundle 的 `layout.files` **只校验存在性、不搬运**。
所以「纯 MCP 建一个应用」做不到——这是设计选择，但应在手册里写明，免得智能体以为漏了工具。
（已在 `gear_guide(topic="rest-api")` §静态资源一段写明。）

## 7. 要变成「真产品」还差什么

当前是可用的单会话工具，若要长期使用还缺（按优先级）：

1. **会话持久化**：想法存下来（用 `db` 或 `kv` 原子落 SQLite），刷新不丢、可回溯历史。
2. **服务端持有 Key**：把 `api_key` 从前端挪到 `${secrets.GEAR_AI_API_KEY}` 引用，页面不再接触明文。
3. **走 Tool 而非裸调原子**：§6.3 的同步端点已补上，可把 prompt 工程收进 `brainstorm__ideate` 等 Tool，
   前端只传主题（`POST /api/tools/brainstorm__ideate/run`）。**本页尚未改造**。
4. **流式输出**：现在是一次等完整响应；`ext.ai` 暂无流式接口，长生成体验偏"卡"。
5. **多人/多会话**：`kv` 的 `scope: session` 已提供隔离基础，但缺会话列表 UI。
6. **完整鉴权**：§6.5 只做了 Origin 守卫 + `secret.*` 禁调。本机多用户 / 多进程场景下
   invoke 面仍需 token 或能力白名单。

## 8. 相关文件

| 文件 | 说明 |
|---|---|
| `apps/brainstorm/www/index.html` | 单页界面（无构建、无依赖，纯静态） |
| `apps/brainstorm/DESIGN.md` | 本文档 |
| registry 内对象 | `brainstorm__config`、`brainstorm__api`、`brainstorm__ideate`、`brainstorm.up` |

停止应用：

```
gear_app(action=stop, app_id="app_brainstorm.up-20260921T234948")
```

## 9. 修复落地（2026-09-22 同日）

| # | 代码落点 | 测试 / 验证 |
|---|---|---|
| 6.1 | `internal/cmdline/parse.go`（+`escableOutsideQuote`）、`exec.go`（`ResolveExecutable`）、`engine/diag.go`（`ENTRY_EXE_UNRESOLVED`）、`mcp/tools_write.go`（`checkToolEntryResolvable`） | `parse_test.go` 新增盘符/尾反斜杠用例、`resolve_test.go`、`mcp/entry_resolvable_test.go` |
| 6.2 | `internal/mcp/guide/rest-api.md`（新）、`guide/guide.go` 注释 | `rest-api` 成为第 9 个可达主题 |
| 6.3 | `internal/server/server.go`（`handleRunTool` + 路由） | `internal/server/run_tool_test.go`（成功 / 空 body / 404 / 400） |
| 6.4 | `internal/mcp/tools_read.go`（`aiReadiness`） | `mcp/entry_resolvable_test.go` 的 `DoctorReportsAIReadiness` |
| 6.5 | `internal/server/server.go`（`originAllowed` / `blockedInvokeCapability`） | `run_tool_test.go` 的 Origin 表驱动 + secret 403 |
| 6.6 | `internal/mcp/tools_read.go`（`sidecarIssue` 导出 + tag） | `DoctorMissingSidecarsAreStructured` |
| 6.7 | `internal/app/record.go`（`Prune` / `prunableState` / `PrunedItem.ProbeState`）、`mcp/tools_app.go`（`prune`）、`cli/app.go`（`app prune`）、`server/server.go`（`POST /api/apps/prune` + `WithRegistryPath`/`WithAppsDir`）、`cli/serve.go`（注入 registry 路径） | `internal/app/prune_test.go`、`mcp/app_prune_test.go`、`server/apps_prune_test.go`、`cli/cli_app_lifecycle_test.go`（`TestAppPruneCommand`）；负向验证 5/5 |
| 6.8 | `internal/engine/lua.go`（metatable 数组标记） | `internal/engine/lua_conv_test.go` |

同步更新的文档：`README.md`（REST 端点表）、`MCP_AGENT_DESIGN.md`（附录 D 诊断码 / 常见误解）、
`internal/mcp/guide/errors.md`、`internal/mcp/guide/overview.md`、`GEAR_工程方法_v1.md` §11（第二十三次）。

验证口径：8 项逐条**负向验证**（改坏 → 必红 → 逐字节还原 → 必绿，8/8）；全量 `go test ./...` PASS；
`bash scripts/gate.sh` GATE PASS；`python plugin/verify.py` 31/31。
