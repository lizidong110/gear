# Gear MCP 接口与协议设计评审

- 评审日期：2026-09-24
- 评审视角：白盒（已读 `internal/mcp/**`、`internal/app/launch.go`、`internal/engine/diag.go`、`internal/registry/registry.go`、`internal/server/server.go`、`internal/mcp/guide/**`、`MCP_AGENT_DESIGN.md`）
- 代码基线：`github.com/modelcontextprotocol/go-sdk v1.8.0`，`mcp.Version = "0.2.0"`，12 个 `gear_*` 工具 + 10 个 `gear://guide/*` 资源
- 标记说明：**★** 表示「只能通过 MCP 接口接入时才会暴露」的问题（REST 面不存在或表现不同），用于验证接口完备性

---

## 总体结论

Gear 的 MCP 面整体水准高于多数同类实现：**三处同源的背景注入**（Instructions / `gear_guide` / resources 共用 `go:embed`）、**错误码带上下文 + 文档锚点 + 自愈回路**、**顶层必须是对象的红线有帧级探针**、**工具命名与体量有机械守卫**——这些都是认真的设计。

但存在 **2 个阻塞级问题**，且都落在同一个主题上：**接口对智能体说的话与真实语义不一致**。接口层的「谎言」比功能缺失更危险，因为智能体会基于它向用户做出承诺。

| 等级 | 数量 | 主题 |
|---|---|---|
| 阻塞 | 2 | 启动结果如实性缺失；HTTP 暴露面无防护 |
| 重要 | 9 | ctx/超时、错误码一致性、破坏性标注、输出 schema、大 payload、能力缺口 |
| 可选 | 8 | 能力协商、资源模板、并发治理、文档补齐 |

> **修复状态（2026-09-24 收尾）**：阻塞 2 ✅、重要 9 ✅（I-1 的④孤儿识别本轮补齐）、可选 8 → **已处理 7**（O-7 合并进 I-1；「无 prompts/completions」按评审建议维持现状）。工具面由 12 增至 13（新增 `gear_invoke`，见 I-8）。

---

# 一、阻塞（Blocking）

## B-1 ★ `gear_app(start)` 不透传 `escaped_job` / `warning`，且文案**无条件承诺「跨会话存活」**

| 项 | 内容 |
|---|---|
| 位置 | `internal/mcp/tools_app.go:44-59`（start 输出）；`internal/app/launch.go:55-63`（`StartResult.EscapedJob` / `Warning`）；`launch.go:245`（Warning 的实际产生点） |
| 接口 | `gear_app(action="start")` |

**原因**
`StartResult` 明明有两个字段用于「如实上报启动质量」：

```go
// internal/app/launch.go:61,63
EscapedJob *bool  `json:"escaped_job,omitempty"`  // 派生进程是否真的脱离调用方 Job Object
Warning    string `json:"warning,omitempty"`      // 「启动成功但有已知隐患」的一句话
```

而 MCP 的 start 输出只拼了 6 个字段，**两个都没带上**：

```go
// internal/mcp/tools_app.go:46-52
out := map[string]any{
    "app_id": res.AppID, "flow_id": res.FlowID, "pid": res.PID,
    "state": res.State, "log_path": res.LogPath, "ports": res.Ports,
}
```

更严重的是，同一接口的**两处文案**做了无条件承诺：

- `tools_app.go:26` Description：「start：派生**独立后台进程**后立即返回（**跨会话存活**）」
- `tools_app.go:58` 返回值 hint：「应用是独立后台进程、**跨会话存活**」

**影响范围**
`CREATE_BREAKAWAY_FROM_JOB` 失败时会退化为普通派生，进程仍在调用方 Job 内——**调用方一退出就被连坐回收**。此时 `escaped_job=false`，「跨会话存活」不成立。

而 MCP 接口既**不给出 `escaped_job` 让智能体自检**，又**主动声明它成立**。后果是智能体把「本次会话内可访问」说成「一直能访问」，向用户做出无法兑现的承诺。

这恰恰撞在项目自己写下的红线上——`internal/mcp/guide/summary.md`（Instructions 七条铁律）：

> 智能体最贵的错误就是把「现在能访问」说成「一直能访问」

工程方法 `GEAR_工程方法_v1.md:710` 也明确：「未脱离时 `StartResult.Warning` + `escaped_job:false`…**宿主不允许脱离就必须说出口**」。**代码层做对了，MCP 接口层把它丢了**。

**为什么是 ★（仅 MCP 暴露）**
REST 的 `/api/apps` 管理的是进程**内存**里的 `AppInstance`，语义上本就是「随进程退出而消失」，从来不承诺跨会话。只有 MCP 的 `gear_app(start)` 承诺了「跨会话存活」，也只有它会因此说谎。

**改进方向**
1. 立即补齐：把 `escaped_job` 与 `warning` 透传到 start 输出（`warning` 非空时必带）。
2. 文案去承诺化：Description 与 hint 改成条件表述——「通常跨会话存活；若 `escaped_job=false`（附 `warning`）则随本进程退出，不要向用户承诺长期可用」。
3. 断言 `state=running` 不足以报成功：Instructions 已提「别只看 state:running」，应补「还要看 escaped_job」。
4. 加机械守卫：测试断言 start 输出**必须**含 `escaped_job` 键，防止再次丢失。

**优先级：P0，立即修。**

---

## B-2 ★ MCP HTTP 暴露面无 Origin 校验、无超时，且文档示例绑定全网卡

| 项 | 内容 |
|---|---|
| 位置 | `internal/mcp/mcp.go:171-178`（`httpHandler`）、`mcp.go:186-198`（`ServeHTTP`）；`internal/cli/mcp.go:49`、`README.md:600` |
| 对比基线 | REST 面有 `originAllowed`（`internal/server/server.go:490-504`），并用于流式端点（`server.go:378-383`） |

**原因**
```go
// internal/mcp/mcp.go:177
return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return ms }, nil), nil
```
直接裸挂，**没有任何等价于 `originAllowed` 的检查**。同时：
```go
// internal/mcp/mcp.go:189
srv := &http.Server{Addr: addr, Handler: h}
```
无 `ReadTimeout` / `WriteTimeout` / `IdleTimeout` / `ReadHeaderTimeout`。

而文档给的示例本身就不安全：`README.md:600` 与 `cli/mcp.go:49` 都是 `gear mcp --http :8788`——**`:8788` 会 LISTEN 到所有网卡**，与代码注释「只应绑 127.0.0.1（§9.3）」自相矛盾，且 `ServeHTTP` 不做任何 bind 地址校验。

**影响范围**
- 浏览器里任意网页可向 `127.0.0.1:<port>/mcp` 发起跨站请求（MCP streamable HTTP 接受 POST + `Content-Type: application/json`，配合宽松 CORS 即可成事），**无任何阻挡**。
- MCP 工具面包含 `gear_apply`（写 registry）、`gear_delete`（删除 Tool/Flow/Hook）、`gear_run`（执行编排）——这是**可写、可删、可执行**的完整面。
- 文档示例让用户默认绑全网卡，把上述风险从「本机」放大到「局域网」。

**为什么是 ★**
REST 面有 Origin 校验，MCP 面没有——同一个进程暴露的两个面，**安全水位不一致**，这是纯粹的 MCP 侧缺口。

**改进方向**
1. 把 `originAllowed` 抽到公共包，MCP 的 `httpHandler` 与 REST 共用同一判据（`server.go:490`）。
2. `ServeHTTP` 对非回环地址：默认拒绝，或至少 stderr 显式告警；需要远程时加显式 `--allow-remote`。
3. `http.Server` 补四个超时（对齐 REST 面的 `ReadHeaderTimeout: 5s` 等）。
4. 文档示例改为 `--http 127.0.0.1:8788`，与代码注释的口径一致。

**优先级：P0，立即修。**

---

# 二、重要（Important）

## I-1 ★ 全部 12 个 handler 丢弃 `ctx`，MCP 层零超时

| 位置 | 证据 |
|---|---|
| `tools_read.go` | 5 个 handler 均为 `_ context.Context` |
| `tools_write.go` | 4 个 |
| `tools_run.go` | 2 个 |
| `tools_app.go` | 1 个 |

`internal/mcp/*.go` 非测试代码 grep `Timeout` **零命中**。

**影响**
- 客户端超时/取消**完全无法传播**；`gear_run(wait_seconds=N)` 的忙等循环（`mcp.go:339-349`）不看 `ctx.Done()`，客户端断开后仍会等满 N 秒。
- `startFlowExec`（`mcp.go:321-333`）每条执行起裸 goroutine，**无并发上限、无 WaitGroup、无优雅退出**：客户端可并发打满；进程退出时在途执行被直接杀，执行记录永久停在 `running`（`gear_execution` 里会出现永不结束的僵尸记录，而 `prune` 只清应用台账、不清执行记录）。

**改进**
1. 至少在 `executionSnapshot` 的忙等里加 `select { case <-ctx.Done(): ... }`。
2. handler 签名从 `_ context.Context` 改为 `ctx context.Context` 并向下传递（引擎侧 `RunFlowExec` 已接受 ctx）。
3. 给 Flow 执行加并发上限 + `sync.WaitGroup`，进程退出时等待在途执行收敛（对齐 REST 面 `ConcurrencyGate` 的既有做法）。
4. `gear_execution` 列表补一个「孤儿执行」识别（长时间 running 且进程已不存在）。

**优先级：P1。**

> ✅ **2026-09-24 已修 ①②③**：`executionSnapshot` 增 `ctx` 参数、忙等改 `select { <-ctx.Done(); <-time.After(200ms) }`（客户端断开即停等、如实返回当前快照）；12 个 handler 签名 `_ context.Context` → `ctx context.Context`；`startFlowExec` 加**有界并发**（`execSem`，满则返回 `EXECUTION_BUSY` 背压）+ `execWG` 跟踪，`Close()` 等待在途执行收敛。**④（孤儿执行识别）未做**。锁：`mcp_review_test.go` 的 `TestExecutionSnapshotStopsOnCancel` / `TestStartFlowExecBackpressure`。

---

## I-2 ★ 错误码体系：文档声明但不产生、码名不一致、兜底码未进手册

| 问题 | 证据 |
|---|---|
| **声明但不产生** | `PORT_IN_USE` / `APP_PLATFORM_UNAVAILABLE` / `PRIMITIVE_MISSING` / `DEP_CYCLE` 全仓 `.go` 产生点 **= 0**，却在 `guide/errors.md:40,51,53`、`MCP_AGENT_DESIGN.md:802,805,810,815` 有条目 |
| **码名不一致** | 附录 D 写 `DEP_CYCLE`，`guide/errors.md:29` 与 `diag.go:23` 写 `BAD_DEPENDENCY`（产生点 1 个） |
| **兜底码未进手册** | MCP 层实际使用约 30 个 fallback 码（`BAD_ARGUMENT`/`CATALOG_FAILED`/`DESCRIBE_FAILED`/`DOCTOR_FAILED`/`DELETE_FAILED`/`APPLY_FAILED`/`PROMOTE_FAILED`/`FLOW_RUN_FAILED`/`TOOL_RUN_FAILED`/`UNKNOWN_TOPIC`/`BAD_YAML`/`UNKNOWN_KIND`/`BAD_APP_MANIFEST`/`CAPABILITIES_UNAVAILABLE`/`APP_LIST_FAILED`…），`errors.md` 只收 22 个 |
| **只取第一条** | `common.go:51-57` `d := diags[0]`，而 `gear_validate` 走 `diagnosticsJSON` 是一次报全——两条路径口径不一致 |

**影响**
自愈回路（`guide/errors.md:3-6`：读 code → 查手册 → 改 → 重验）在两种情况下断裂：①按码查到「不存在的条目」；②拿到兜底码时手册无条目，而 `UNCLASSIFIED` 条目说「这通常说明该错误路径还没做结构化」，会**误导智能体以为 Gear 有 bug**，实际只是手册漏收。

**改进**
1. 加机械守卫：断言 `errors.md` / 附录 D 的每个码都有至少一个产生点（双向核对，与 `doc_audit.py` 同款思路）。
2. 统一 `DEP_CYCLE` → `BAD_DEPENDENCY`。
3. 兜底码全部进手册，至少各一条「含义 + 自助动作」。
4. `toolErrorFrom` 支持多条诊断（或明确文档化「只报首条」并解释为何）。

**优先级：P1。**

> ✅ **2026-09-24 已修 ①②③④**：① 新增机械守卫 `tools/doc_audit.py` 的「③ MCP 错误码双向核对」（`guide/errors.md` ⇄ 代码产生点，附录 D 亦须 ⊆ 产生点；已做负向验证：改坏一个码 → exit 1，还原 → exit 0）并**进闸门**；② 统一为 `BAD_DEPENDENCY`（附录 D 的 `DEP_CYCLE` 已改并注明旧名）；③ **删除三个从不产生的码**（`PRIMITIVE_MISSING`/`PORT_IN_USE`/`APP_PLATFORM_UNAVAILABLE`——留着只会让智能体查到不存在的条目），并把 **17 个实际在用却漏收的兜底码**全部补进手册（新增「MCP 工具调用层（通用码）」一节）；④ `toolErrorFrom` 现把多余诊断并入 hint，并在代码里写明「一条 error 至多带一条诊断（`diagWrap` 不叠加）」所以 `diags[0]` 不会丢信息。锁：`tools/doc_audit.py`。

---

## I-3 ★ 必填参数错误没有 `[CODE]` 前缀，与错误契约不一致

- 位置：`mcp_errorpaths_test.go:167-186` 明确记录此行为；文案形如 `validating "arguments": … missing properties: ["kind" "id"]`

**原因**：校验发生在 SDK 层，压根到不了 Gear 的 handler，因此走不到 `toolError()`。

**影响**：而 `mcp_test.go:149-167` 的 `expectToolError` 断言错误文本**必须以 `[` 开头**——契约在一条高频路径上不成立。智能体学到「错误都有码」，遇到这条只能读英文猜。文档标为「取舍不是缺口」，但严格说是**契约不一致**，且它是入门阶段最可能遇到的错误（参数写错）。

**改进**：在 `guide/errors.md` 与 Instructions 里显式说明「JSON-RPC 层的参数校验错误不带码，读 `missing properties` 即可定位字段」，或考虑包一层自定义参数校验让错误走 handler。

**优先级：P1。**

> ✅ **2026-09-24 已修（采纳「显式说明」方案）**：`guide/errors.md` 开头新增「例外：缺必填参数不带码」小节（形如 `missing properties: [...]`，读字段名即可定位），`guide/summary.md`（Instructions）也加了同一句提醒——比「额外包一层自定义校验」更小的改动，且不牺牲「客户端就地发现漏参」的好处。已有回归用例 `mcp_errorpaths_test.go: TestMCPSchemaRequiredArgsFailLoudly` 钉住该行为。

---

## I-4 ★ `gear_app` 含 `stop` / `prune` 破坏性动作，却只标 `IdempotentHint`

- 位置：`tools_app.go:29` `Annotations: idempotent("应用生命周期")`
- 对照：`tools_app.go:27` Description 自己写了 `stop` 是「**破坏性**」；`gear_delete` 正确标了 `DestructiveHint`（`tools_write.go:475`）

**影响**：客户端（如 IDE 的工具确认 UI）依赖 annotations 判断风险。标注与实际不符 → `stop`/`prune` 不会触发风险提示，而 `gear_delete` 会。标准是**按动作粒度**标，但 MCP 的 annotations 是**工具级**的，无法表达「同一工具内部分动作破坏性」。

**改进**：近期——改为 `destructive`（宁可过度提示）；中期——考虑把 `stop`/`prune` 拆成独立工具，或至少在返回值里对破坏性动作回显 `destructive: true` 供客户端识别。

**优先级：P1。**

> ✅ **2026-09-24 已修（近期方案）**：`gear_app` 的 annotations 由 `idempotent` 改为 **`destructive`**（宁可过度提示），并在 `stop` / `prune` 的**返回值**里回显 `destructive: true`（`prune(dry_run)` 不标——没实际改动）供只读结果的客户端识别。中期方案（拆工具）未做。锁：`mcp_review_test.go` 的 `TestMCPAppToolMarkedDestructive` / `TestMCPAppStopEchoesDestructive`。

---

## I-5 ★ `gear_guide` 的 topic 清单漏 `rest-api`，且新增主题无守卫

- 位置：`common.go:115` 硬编码 8 个 topic（`overview/authoring/service-app/atoms/errors/primitive-gap/examples/security`），而 `internal/mcp/guide/` 实际有 9 个（多 `rest-api`）
- 机制：`guide/guide.go:38-57` 读目录自动发现，无白名单

**影响**
- 只看 schema 的客户端**永远不知道 `rest-api` 主题存在**（只有真调一次 `gear_guide` 拿到 `topics` 字段才发现）。
- 反向风险：任何一次往 `guide/` 放 md，都会静默改变 `resources/list` 与 `gear_guide` 的 topic 集合，而 schema 文案会立刻过期——**现状已经过期**。

**改进**
1. 立即补 `rest-api`（并修正 schema 文案）。
2. 加机械守卫：断言 `common.go` 的 topic 文案集合 == `guide.Topics()`（单一真相源应是磁盘，不是硬编码字符串）。更好：schema 描述里不枚举 topic，改为「见 `gear_guide` 返回的 `topics` 字段」。

**优先级：P1。**

> ✅ **2026-09-24 已修**：采纳「更好」的方案——`GuideArgs` 的 schema 文案**不再枚举 topic**（改为「见返回值里的 `topics` 字段，或 `resources/list` 的 `gear://guide/{topic}`」），单一真相源回到磁盘。守卫：`mcp_review_test.go` 的 `TestMCPGuideTopicsSingleSource`（断言 schema 文案里不出现任何 topic 名 + 每个磁盘 topic 都真能取到且返回集合与磁盘逐项一致），并保留 `rest-api` 存在的显式断言。

---

## I-6 ★ 完全没有 `outputSchema`（`Out = any`）

- 位置：所有 `mcp.AddTool[In, any]`，`internal/mcp/*.go` grep `outputSchema` 零命中
- 对照：input 侧由 `jsonschema` tag 自动生成，且有 `TestMCPSchemaRequiredArgsFailLoudly` 守卫

**影响**：客户端**无法静态知道返回形状**，只能靠 `gear_guide` 文档或试调用。与 input 侧的严谨度严重不对等。返回结构虽有 `objectResult` 保证顶层是对象，但键名、字段类型全靠文档。

**改进**：为高频工具（`gear_run` / `gear_app` / `gear_validate` / `gear_catalog`）定义输出结构体并让 SDK 推断 `outputSchema`；至少把返回键名写进工具 Description 或 `guide/`。

**优先级：P1（对第三方接入者而言这是 DX 最大短板之一）。**

> ✅ **2026-09-24 已修**：为**全部 12 个工具**显式声明 `OutputSchema`（`internal/mcp/common.go` 的 `out*` 变量，逐键列名+类型；动态/可空字段用空 schema）。**未采用**「改成 typed `Out` 让 SDK 推断」——因多数 handler 按 action/kind 返回不同形状的动态 map，typed Out 会牵动多路返回体且易与真实返回键漂移；显式 schema 集中一处、可逐键对照。两条硬约束（**不设 `required`、不禁 `additionalProperties`**）是必须的：SDK 对**每次**返回做 schema 校验，过严会把合法返回拒掉。锁：`mcp_review_test.go` 的 `TestMCPToolsAdvertiseOutputSchema`（负向验证：去掉任一 `OutputSchema` 即红）。

---

## I-7 大 payload：三处无分页 / 无裁剪

| 工具 | 现状 | 位置 |
|---|---|---|
| `gear_catalog` | 有 `limit`（默认 100）+ `truncated`/`counts`，但**无 `offset`**，超过部分完全看不到 | `tools_read.go:178-186` |
| `gear_execution`（列表） | **无任何限制**，`s.execs.List()` 全表 | `tools_run.go:85` + `internal/server/execstore.go:250-264`（SQL 无 LIMIT） |
| `gear_capabilities` | **无裁剪**，一次全量；而它是 Instructions 铁律要求「写 step.call 前必看」的高频工具 | `tools_read.go:238-249` |

**影响**：`gear_execution` 随运行时间无限增长；`gear_capabilities` 随 `ext.*` 注册数线性膨胀。**★ 放大效应**：MCP 的每次工具返回都占用模型上下文，而 REST 客户端可以只取字段——大 payload 在 MCP 场景的代价远高于 REST。

**改进**：`gear_catalog` 加 `offset`；`gear_execution` 加 `limit`；`gear_capabilities` 支持 `name`/`kind` 过滤（必要时只返回方法名清单，详情用 `gear_describe` 取）。

**优先级：P1。**

> ✅ **2026-09-24 已修**：三处都做了——`gear_catalog` 加 `offset`（并回 `next_offset` 续取位置）；`gear_execution` 加 `limit`（默认 20，`total` 仍为全量计数，被截断时给 `hint`）；`gear_capabilities` 支持 `name`/`kind` 过滤（无匹配时返回空数组 + 可用能力名 `hint`，不是错误）。锁：`mcp_review_test.go` 的 `TestMCPPaginationAndFilters`。

---

## I-8 ★ MCP 面缺少「能力直调」与「流式」工具（REST 两者都有）

- REST 有：`POST /api/capabilities/invoke`、`POST /api/capabilities/invoke/stream`（`internal/server/server.go:220` 附近）
- MCP 只有：`gear_capabilities`（**只读目录**）

**影响**
1. 智能体想「试一下 `db.exec` 能不能用」，MCP 面只能写一个 Flow 再 `gear_run`——为一次原子调用付出一轮编排的代价。
2. 想「看着日志跑」，MCP 面只能 `gear_run(wait_seconds=N)` 阻塞忙等（**且不响应取消**，见 I-1）。
3. 隐含的安全口径不一致：REST 的 `blockedInvokeCapability`（禁 `secret.*` 直调，`server.go:506-510`）在 MCP 面无从谈起——**将来若补 invoke 工具，必须同步搬这条禁则**。

**改进**：补 `gear_invoke`（能力直调），必要时补流式变体；同时把 REST 的禁则搬过来。

**优先级：P1（能力缺口 + 潜在安全口径不一致）。**

> ✅ **2026-09-24 已修（直调部分；流式未做）**：新增第 13 个工具 **`gear_invoke`**（`internal/mcp/tools_invoke.go`），走与 REST 完全相同的引擎入口 `engine.InvokeCapability`。
> 关键点：**禁则搬到了共享真相源** `cap.BlockedDirectInvoke`（REST 的 `blockedInvokeCapability` 与 MCP 的 `gear_invoke` 都调它），从根上消除「两个执行面安全水位不一致」。
> 流式变体**未做**（MCP 的 tools/call 无 SSE 语义；`wait_seconds` + `gear_execution` 已够用，故不引入）。
> 工具数预算随之上调 12→13（`tools_surface.go` 的 `toolBudget`），并同步 README / MCP_AGENT_DESIGN。锁：`mcp_review_test.go` 的 `TestMCPInvokeCapability`。

---

## I-9 权限边界不可枚举（不看 guide 无从得知禁止项）

- 现状：禁止项分散在 `guide/security.md:26-34`（破坏性清单）、`tools_write.go:474`（仅 `gear_delete` 写了「破坏性操作」）、`tools_write.go:431`（`[MISSING_SECRET]` 提示）
- Instructions 七条铁律（`guide/summary.md:23-30`）中只有第 4 条涉及凭据，**没有指向破坏性清单**

**影响**：「哪些操作被禁止 / 必须走 CLI」只能通过主动读 `gear_guide("security")` 得知。工具描述与 schema 层完全不可见——**密钥写入不支持**这条尤其关键：智能体可能在不知情的情况下反复尝试，或误以为能配。

**改进**：
1. Instructions 加一条铁律：「先读 `gear_guide("security")`，尤其是破坏性清单与密钥规则」。
2. `gear_apply` 的 Description 里直接写明「不支持密钥写入，见 `gear_guide("security")`」。

**优先级：P1。**

> ✅ **2026-09-24 已修**：Instructions 由「七条铁律」扩为**八条**——第 8 条即「动手前先读 `gear_guide("security")`：破坏性清单与密钥规则都在那里」；`gear_apply` 的 Description 也写明「**不支持密钥写入**，见 `gear_guide("security")`」。`guide/summary.md` 与 `MCP_AGENT_DESIGN.md`（§4 / 附录 A 快照）同步。

---

# 三、可选（Optional）

| # | 问题 | 位置 | 说明与建议 |
|---|---|---|---|
| O-1 ★ | 声明 `logging` capability 但从不发送日志 | SDK 默认 `caps.Logging`（`go-sdk@mcp/server.go:668-670`）；`internal/mcp` 无任何日志通知调用 | 客户端以为能 `logging/setLevel` 拿到服务端日志，实际永远收不到。**建议**：把已改道 stderr 的进度日志桥接成 MCP 日志通知（对 stdio 排障价值很大），或直接关掉该 capability |
| O-2 ★ | `listChanged: true` 是死代码 | 工具/资源只在 `newMCPServer` 注册一次（`mcp.go:155-158`），运行期不变 | 声明会诱导客户端订阅。建议显式置 false 或注明 |
| O-3 ★ | 协议版本范围完全不受控 | `ServerOptions.SupportedProtocolVersions` 未设，SDK 默认放行 2024-11-05 ~ 2026-07-28 | 而「顶层必须是对象」的红线是按 2025-06-18 定的。建议显式钉支持范围 |
| O-4 ★ | resources 未声明 URI 模板 | 只 `AddResource`，无 `AddResourceTemplate` | 文档按 `gear://guide/{topic}` 模板宣传（`README.md:613`），实现是 10 个静态 URI——**文档与实现口径不一致**。建议补 `AddResourceTemplate` |
| O-5 ★ | resources 无 Go 测试覆盖 | `internal/mcp/*_test.go` grep `ListResources`/`ReadResource` 零命中，只有 Python 探针 | resources 不在 `go test` 闸门内，改名/删除不会被测到。建议补单测 |
| O-6 ★ | `summary` 被注册为资源，与设计意图矛盾 | `guide/guide.go:80` 注册了 `summary`，而 `guide.go:23-25` 说「summary 不进 Topics 列表」 | 工具侧刻意排除、资源侧又注册。建议统一 |
| O-7 | Flow 执行 goroutine 无治理 | `mcp.go:326-331` 裸 `go func()` | 见 I-1，合并处理 |
| O-8 | 新增工具需同步改 5 处，无生产侧清单 | `mcpToolRegistry` 只在 `mcp_test.go:1030-1049` | 好处是漏改会 CI 红；建议把这份登记表提升为生产侧常量，供 `gear_capabilities` 之类自描述接口复用 |

> ✅ **2026-09-24 已处理的 7 条**（除 O-7 合并进 I-1、以及「补充观察」按建议维持现状外）：
> - **O-1**：显式**不声明 `logging`**（`ServerOptions.Capabilities` 里不带 `Logging`）。理由：本包从不发日志通知，声明它就是骗客户端；该特性本身也已在 2026-07-28 版规范弃用（SEP-2577）。要日志请读 `gear_execution`。
> - **O-2**：`ToolCapabilities{ListChanged:false}` / `ResourceCapabilities{ListChanged:false}`（非 nil 才不会被 SDK 的 augment 分支覆盖回 true）。工具面与资源面运行期确实从不变化。
> - **O-3**：`SupportedProtocolVersions: ["2025-06-18"]`（`mcp.go` 的 `supportedProtocolVersion`）——只声明被那套「顶层必须是对象」红线验证过的版本；协商是优雅降级，不会造成旧客户端连不上。
> - **O-4**：补 `AddResourceTemplate("gear://guide/{topic}")`，与文档口径一致；读取走同一份 embed。
> - **O-5**：补 Go 单测 `mcp_review_test.go: TestMCPResourcesExposeGuideTopics`（列全 topic、不含 summary、静态资源可读、模板可读、模板已声明）。
> - **O-6**：resources 只注册 `guide.Topics()`（**不含 summary**），与 `gear_guide` 工具口径统一；`guide.All()` 随之删除（唯一消费者是本处注册）。
> - **O-8**：登记表提升为**生产侧常量** `toolSurface` + `toolBudget`（`internal/mcp/tools_surface.go`），测试改用它做双向核对。
> 另：**补充观察**里的「README 工具表只列到 `gear_describe`」已补齐（现 13 行齐全，含 `gear_invoke`）。

补充观察（不列条目）：**无 prompts、无 completions**——「创建应用」这类高频流程没有可复用提示模板，全靠 Instructions 文本，与设计文档把 resources 标为「加分项」的定位一致，可维持现状。`README.md:604-615` 的工具表只列到 `gear_describe`（后 9 个缺失），作为第三方第一入口应补齐。

---

# 四、「仅 MCP 接入才暴露」问题汇总（接口完备性验证清单）

以下 12 项是**只有走 MCP 才会遇到**的——REST 面要么不存在该语义，要么已有对应防护。这条清单可作为「第三方纯 MCP 接入」的验收用例：

| # | 问题 | 严重度 |
|---|---|---|
| 1 | `gear_app(start)` 虚假承诺「跨会话存活」且不给 `escaped_job` 自检手段 | 阻塞 |
| 2 | MCP HTTP 无 Origin 校验（REST 有）+ 无超时 | 阻塞 |
| 3 | 全部 handler 丢弃 ctx，客户端取消/超时完全无效 | 重要 |
| 4 | 必填参数错误无 `[CODE]` 前缀，与错误契约不一致 | 重要 |
| 5 | 无 `outputSchema`，返回形状不可静态获知 | 重要 |
| 6 | `gear_guide` topic 清单与磁盘失同步（漏 `rest-api`） | 重要 |
| 7 | 无能力直调 / 无流式工具（REST 两者都有） | 重要 |
| 8 | `gear_app` 破坏性动作未标 `DestructiveHint` | 重要 |
| 9 | 大 payload 无分页，而 MCP 返回直接占用模型上下文 | 重要 |
| 10 | 声明 `logging` capability 但从不发日志 | 可选 |
| 11 | `listChanged` 声明为 true 但运行期从不变化 | 可选 |
| 12 | 协议版本未钉、resources 无 URI 模板、resources 无 Go 测试 | 可选 |

> **2026-09-24 状态**：上表 12 项**已全部处理**（1/2 早前已修，3~9 本次修，10~12 本次修）；验收闭环见下方 `TestMCPAgentAcceptanceFlow`。

**验证方法建议**：写一个「纯 MCP 接入者」脚本，在不读任何 Go 源码、只读 `tools/list` + `resources/list` + `gear_guide` 的前提下完成「创建并启动一个应用 → 确认它真的跨会话存活 → 读取执行日志」全流程；上面每一条都会在该流程中真实撞到。

> ✅ **2026-09-24 已落地为 Go 验收用例** `internal/mcp/mcp_review_test.go: TestMCPAgentAcceptanceFlow`（走完「自描述 outputSchema → doctor → capabilities(过滤) → guide(security) → invoke → validate → apply → run → execution(含日志) → initialize 元信息」），**进闸门**（`go test`）。表中第 1/2 项另有 `mcp_app_test.go` 的 `escaped_job` 硬断言与 `TestServeHTTPListensAndShutsDownOnCtxCancel`。

---

# 五、改进路线建议

> ✅ **2026-09-24：三步的 9 条全部完成**（B-1/B-2 早前已修；I-1①②③ 与 I-6 早前已修；本次补齐 I-1④ / I-2 / I-3 / I-4 / I-5 / I-7 / I-8 / I-9 与可选级 7 条）。闸门 `bash scripts/gate.sh` PASS。

**第一步（本周，消除接口层谎言与安全缺口）**
1. B-1：`gear_app(start)` 补 `escaped_job` / `warning`，文案去承诺化，加守卫测试。
2. B-2：MCP HTTP 复用 `originAllowed`、补 http 超时、非回环地址告警/拒绝、文档示例改 `127.0.0.1`。
3. I-5：`gear_guide` topic 补 `rest-api` 并加磁盘 ↔ schema 一致性守卫。

**第二步（两周内，可诊断性与契约）**
4. I-2：错误码双向核对守卫 + 统一 `DEP_CYCLE` + 兜底码进手册。
5. I-3：把「参数校验错误不带码」写进手册与 Instructions。
6. I-9：Instructions 指向 `gear_guide("security")`；`gear_apply` 描述写明不支持密钥写入。
7. I-4：`gear_app` 改为 `destructive`。

**第三步（一个月内，能力与扩展性）**
8. I-1：ctx 贯通 + 忙等可取消 + 执行并发治理。
9. I-6 / I-7 / I-8：`outputSchema`、分页裁剪、能力直调（含搬 REST 的 secret 禁则）。

---

## 附：做得好的地方（建议保持并有守卫）

- **三处同源**（`guide/guide.go:1-10`）：Instructions / `gear_guide` / resources 共用 embed.FS，减少漂移。
- **顶层必须是对象**的红线（`mcp.go:227-232`）+ 帧级探针 `plugin/mcp_wire_probe.py` 的 33 条判据（含「合成的裸数组帧必须被判违规」的自检）。
- **错误带 `field` + `hint` + `docs: gear://guide/errors#CODE`**（`common.go:65-87`），自愈回路闭环。
- **失败 vs 错误的分界契约**（`common.go:5-10`）：执行失败是成功返回值里的 `status: failed`，前置条件不满足才 `IsError`。
- **工具命名/体量/数量预算的机械守卫**（`mcp_test.go` + `tools_surface.go`）：`gear_` 前缀、单条 ≤240 rune、工具数 ≤ `toolBudget`（13）。
- **`jsonschema` tag 不能以 `WORD=` 开头**的踩坑记录 + 守卫（`common.go:144-147`、`mcp_test.go:1106`）。
