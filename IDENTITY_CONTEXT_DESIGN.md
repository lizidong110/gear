# IDENTITY_CONTEXT_DESIGN.md — 调用上下文：身份链路 + 命名空间守卫

> 状态：**① 已实施**（2026-09-26）；**② 未实施**
> 配套：`APP_ARTIFACT_DESIGN.md`（产物）、`MCP_AGENT_DESIGN.md`（工具面）
> 顺序：**①+② 合并实施 → ④（留旧/回滚）→ ③（审计表）**
> 已确认：**允许 breaking change**（④ 统一三条写入路径时可改 CLI/REST 签名）

---

## 1. 问题陈述

「发布后让用户在 gear 上自助构建」（L2/L3）要求三件当前内核**一件都没有**的事：

| 缺什么 | 后果 |
|---|---|
| 知道**谁**发起了一次执行 | Hook 无法做鉴权；无法回答「这条数据是谁改的」 |
| 运行时能**守住命名空间** | 应用 A 的进程可以 `gear_run(kind=tool, id="B__secret")` 摸到应用 B 的对象 |
| 定义变更可回滚 | `registry.upsert` 整行覆盖，改错了只能手工重建 |

本文只解决前两项（①+②）。第三项见 §5。

---

## 2. 约束勘察（结论先行，附证据）

| 事实 | 证据 |
|---|---|
| 生产代码**零** `context.WithValue` / `ctx.Value` | 全仓 grep 仅 `context.Background()` 与测试里的 cancel |
| `engine.go` 的 ctx 入口全是 `context.Background()` | `engine.go:204`、`:416`、`:611`、`:774` |
| `RunTool` **不接收 ctx** | `engine.go:579 RunTool(toolID, input)` |
| 但 ctx 在引擎内部**已经贯通** | `applyHooks(ctx,…)` `hook.go:78`、`prepareStepInput(ctx,…)` `hook.go:252`、`runOnceStep(ctx,…)` `hook.go:319` |
| Hook payload 只有 vars / step 的 input-output | `hook.go:197-203`、`:266`、`:367` |
| ★ `cap.Context` **已有**「每次执行注入宿主视图」的先例 | `cap.go:232-236 SetCallChain` / `SetSubscriptionOwner`，均由 `engine.hostFor` 调用（`engine.go:378-382`） |
| Lua 侧已有「内核内建、无条件注入」的表 | `lua.go:270 ctx.json`、`:273 time/template/crypto/id/validate` |
| process runtime 已有「校验后注入 input.ctx」 | `process.go:202-215 withHostCapabilities` |
| 写入期 namespace 校验**已被抄了三份** | `tools_write.go:281 checkNamespace`、`artifact/artifact.go:300`、`tools_read.go:277` |
| 运行期 4 个解析点全裸 | `stepcall.go:84 resolveStepCallView`、`engine.go:410/423`、`hook.go:87/159/170` |
| `internal/engine` 全包 **appID 零命中** | grep `appID` 无 engine 结果 |
| `internal/app.AppID` 是**运行实例台账 id**，不是命名空间 | `launch.go:182-189 appID := NewRecordID(opts.FlowID, now)` → `app_<flow>-<ts>` |
| 命名空间前缀的唯一实现是 `owns()` | `artifact/artifact.go:314-316`：`HasPrefix(id, a.ID+"__") \|\| HasPrefix(id, a.ID+".")` |
| ★ go-sdk **v1.8.0 不给 tool handler 传 `*http.Request` / session** | `mcp.go:2010-2012` 只注入 `idContextKey`、`logLevelContextKey`；`CallToolParams` 无请求元信息位 |

### 2.1 最关键的一条

`cap.Context` 的 `SetCallChain` / `SetSubscriptionOwner` 说明「**每次执行的宿主视图**」这个通道**早已存在且是既有范式**。
身份不该另起炉灶，应当**复用这条通道**。

---

## 3. ① 身份链路

### 3.1 载体选择（这是本文最重要的一个决定）

| 方案 | 做法 | 为什么**不**选 |
|---|---|---|
| A. 塞进 Hook payload | `applyHooks(..., payload["__caller"]=…)` | payload 对 before_step 就是 **step 的 input**（`hook.go:266`）。塞保留键会撞 `input_schema` 的 `additionalProperties`；且 `before_flow` 的 payload 是 vars，两处语义不同 |
| B. **宿主视图 `cap.Context`** | 复刻 `SetCallChain` | ✅ 选它。Hook 的执行路径本身就要构造 host（`hook.go:207 e.hostFor(tool, rc)`），身份随之可见，**不打扰 payload** |
| C. `runContext` 字段 + 靠约定传递 | 不注入 host | Lua 脚本读不到；process 子进程更读不到 |

**决定：走 B。**

### 3.2 类型与注入

```go
// internal/cap/cap.go
type Caller struct {
    Subject string   // 主体标识（REST: bearer 解析结果；MCP: 见 §3.5）
    AppID   string   // 本次执行归属的应用命名空间（空 = 无 scope）
    Via     []string // 调用链（与现有 via 同源，复用而非另起）
}

func (c *Context) SetCaller(v Caller) { c.caller = v }
func (c *Context) Caller() Caller     { return c.caller }
```

注入只有一处：**`engine.hostFor`**（`engine.go:378`），因为它已经是「构造宿主视图的唯一入口」：

```go
c.SetCallChain(callChainOf(rc))
c.SetCaller(rc.caller)   // ← 新增，与上一行同批
```

`runContext.caller` 是**唯一写入口，且只从引擎边界的 ctx 值读取**——
**永不由 vars / 调用参数覆盖**（红线：caller 不可伪造）。因为 `rc.caller` 是 Go 结构体字段，
vars 路径压根碰不到它，这属于**结构性保证**，不需要靠纪律维持。

### 3.3 三个暴露面

| 载体 | 注入点 | 脚本里怎么写 |
|---|---|---|
| Lua | `setupLuaCapabilities` 与 `ctx.json` 同批**无条件**注入 | `ctx.caller.subject` |
| process | `withHostCapabilities` 在 `input.ctx` 上合并 | 子进程 stdin 拿到 `ctx.caller` |
| Go host Tool | `host.Caller()` | — |

**为什么要等 schema 校验之后再注入 process 的 `input.ctx`**：
`runOnceStep` 的顺序是 `prepareStepInput`(含 `validate(input_schema)`) → `stepExecutable` → `rt.Run`。
`withHostCapabilities`（`process.go:204-215`）此刻才写，所以不会破坏 `input_schema`。
`capabilities` 已经这么做了，caller 同批。

### 3.4 ctx 值通道与边界注入

key 与访问器定义在 **`internal/engine`**（mcp / server 都 import engine，无循环）：

```go
func WithCaller(ctx context.Context, c Caller) context.Context
func CallerFrom(ctx context.Context) (Caller, bool)
```

只改 4 处边界，把 `context.Background()` 换成调用方传入的 ctx：

| 位置 | 现状 |
|---|---|
| `engine.go:611`（`RunToolExec`） | `e.runOnceStep(context.Background(), …)` |
| `engine.go:774`（`execLevel` 顶） | `lvlCtx, cancel := context.WithCancel(context.Background())` |
| `engine.go:204` / `:416` | 内联 `rt.Run(context.Background(), …)`（capability 直调路径） |

`RunTool(toolID, input)`（`engine.go:579`）暂不加参数——`CallerFrom(ctx)` 取不到时退化为
「无身份」而不是报错，否则会打爆 CLI 的所有调用点。**是否加显式参数，等 ② 落地**时一并决定
（`RunTool` 那时本来就要为 appID 改签名）。

### 3.5 ★ 两个执行面的身份来源：如实性声明

这是本文**必须写明**的一条，因为它决定 L3 的可信边界。

**REST 面（`internal/server`）**：身份是**真的**。
`handleRunTool`（`server.go:451`）等 handler 手里就是 `*http.Request`，
bearer token / `X-Gear-Subject` / mTLS 都能解析。照 `httpsafe.OriginAllowed`（`server/httpsafe/origin.go:26`）
的写法抽一个统一元信息提取函数，**不要在每个 handler 里各写一遍**。

**MCP 面（`internal/mcp`）**：身份**不可得**，且不假装可得。
go-sdk v1.8.0 在 `mcp.go:2010` 只往 ctx 塞了 request id 与 log level，
`CallToolParams`（`protocol.go:219-236`）也没有请求元信息位；
`ServerSessionOptions`（`server.go:1418`）不暴露 session 给 handler。
**结论：`gear_run` 的 handler 拿不到调用方。**

因此 MCP 面采用**声明式身份**，且必须默认关闭：

- `gear_run` / `gear_invoke` 增加可选入参 `as`（主体声明）。
- 仅当进程以 `--trust-caller-as` 启动时，`as` 才被采信写入 `rc.caller`；
  否则 `as` 被**显式忽略**、caller 恒为 `anonymous`，并在返回里带一条 `hint`。
- 现状补丁：`tools_run.go:53 handleRun(ctx, _ *mcp.CallToolRequest, in RunArgs)` ——
  **`req` 被显式丢弃了**。改为接收并保留参数，是为将来接真身份留的口；
  在那之前，**不要**让 `as` 悄悄变成信任源。

> 这条是「接口层如实性」红线：宁可明说「MCP 面无鉴权」，
> 也不能让智能体以为 gear 已经验过身份。

**对 L3 的含义**：最终产品的鉴权必须发生在**它自己的前门**（它自己的鉴权 → 带 token 的 REST），
gear 的 MCP 面是**管理平面**，不是信任边界。

### 3.6 拒绝通道

Hook 拿到 `ctx.caller` 后如何拒绝：**引擎侧已有 fail-closed 通道**——
`hookFail`（`hook.go:59-68`）在 Hook 执行失败时终止整次编排。
所以「身份不匹配 → 让 Hook 报错」即可，**不需要新增拒绝机制**。

### 3.7 红线自查

| 红线 | 满足方式 |
|---|---|
| caller 不可由 vars / 调用参数覆盖 | `rc.caller` 是引擎边界写入的 Go 字段，vars 路径不可达（结构性） |
| 未知值不许静默放过 | `CallerFrom` 未命中 → caller 显式为「无身份」，Hook 可见，可被拒绝 |
| 同一判定不许出现两处 | 注入只有 `hostFor` 一处；`SetCaller` 只有一个调用点 |

---

## 4. ② 命名空间守卫（运行期）

### 4.1 现状

写入期：`checkNamespace`（`tools_write.go:281`）已有一份，但**被抄成了三份**
（`artifact/artifact.go:300`、`tools_read.go:277`）——这是已知的技术债，本次**顺手收口**。

运行期：**4 个解析点全裸**，没有任何 scope 概念：

| 位置 | 解析什么 |
|---|---|
| `stepcall.go:84 resolveStepCallView` | step.Call → Tool / 原语 |
| `engine.go:410`、`engine.go:423` | 宿主视图 / call 解析 |
| `hook.go:87/159/170` | Hook 的 target 匹配 |

### 4.2 appID 从哪来（**同名不同物，最大的坑**）

| 名字 | 实际含义 | 证据 |
|---|---|---|
| `internal/app.Record.AppID` | **运行实例台账 id**（`app_<flow>-<ts>`） | `record.go:58`、`launch.go:182-189` |
| `model.App.ID` | **命名空间前缀**（其余 id 以 `<id>__` / `<id>.` 开头） | `model/app.go:40`、`artifact/artifact.go:315` |

两者**必须在文档与变量命名上彻底分开**（建议：台账侧保留 `AppID`，命名空间侧一律叫 `Scope`
或 `NamespaceID`）。混起来是「看起来对、实际错」的典型。

`internal/engine` 现在**完全不知道**自己在为哪个应用跑。`model.App.ID` 只在 registry 的 `apps` 表里。
所以要让守卫生效，必须把 scope 送进运行期——预计加在运行期入口参数上
（`RunTool` / `RunFlowExec`），这正是需要 breaking change 的地方（已获批准）。

**共享规则必须只有一份**：把 `artifact/artifact.go:314-316` 的 `owns()` 提升为
`model` 或 `registry` 的公共函数（`App.Owns(id) bool`），写入期与运行期同调。
否则就是第四份前缀判定，日后改规则必漏。

### 4.3 守卫规则与启用策略

一旦 scope 非空，运行期解析点**必须** fail-closed：

- `step.Call` 解析出的 Tool 归属必须 `App.Owns(id)`，否则拒绝；
- Hook 的 target 同理。

**默认值 = 不启用（scope 为空 = 无守卫）**。理由：现有定义是先存在、后挂 App 的
（`apps/mindstorm` 的 22 个 Tool 与 `apps` 表是两条独立路径），默认开守卫会立刻炸掉单机版。
**留一个显式的 `--scope <appID>` 开关**，开了才 fail-closed。

### 4.4 端口分配（②的附带项）

现有端口逻辑散在四处：`record.go:301-312`（300ms dial 探测）、`launch.go:213-219`（preBusy）、
`cli/app.go:133`（猜测文案）、以及 `plugin/extensions.yaml:218` 里 `ext.serve` 的 listen
**硬编码 8095**。

推荐 **(b) 自动挑空闲端口并把实际端口回给调用方**，而不是 (a) 已分配登记簿：
登记簿要处理释放/泄漏/崩溃回收，是额外状态；挑空闲端口是纯函数，无状态可泄漏。
`gear_app(start)` 的返回里带上实际端口。

---

## 5. ④（本次**不做**）：留旧与回滚

`registry.upsert`（`registry.go:84-103`）是 `ON CONFLICT DO UPDATE` **整行覆盖**，无历史。
快照**必须下沉到 `registry.upsert` 内部**——否则 9 处旁路直连点
（`cli/tool.go:81`、`cli/flow.go:83`、`cli/hook.go:37`、`cli/serve.go:106/116/128/151`、
`cli/ai.go:328/337`、`engine/ai_agent.go:1161+`、`engine/extensions.go:820`、
`server.go:574/651/694`）每一处都要记得快照，漏一处就是一个静默的不可回滚点。

回滚入口倾向 `gear_apply` 加 `restore_from(seq)` 参数，**不新增工具**（工具预算已满）。

**现存不一致（顺带记下）**：MCP 有 overwrite 冲突检查（`artifact/apply.go:73-77`），
而 CLI `cli/tool.go:80-85` 与 REST 是**裸 Upsert、零检查**。④ 落地时一并统一。

---

---

## 8. 实施后补记（2026-09-26，① 已完成）

### 8.1 落地清单

| 层 | 改了什么 |
|---|---|
| `internal/cap/cap.go` | 新增 `Caller{Subject, AppID, Via}`、`Context.caller` 字段、`SetCaller`、`Caller()` |
| `internal/engine/callctx.go`（新） | `WithCaller` / `CallerFrom`，key 私有在 engine 包 |
| `internal/engine/engine.go` | `runContext.caller`；`hostFor` 注入；`runWithVars(ctx,…)` 读一次；`RunToolExec(ctx,…)`、`RunFlowExecWithVars(ctx,…)`、`RunFlowResultCtx(ctx,…)` |
| `internal/engine/stepcall.go` | `primitiveHost` 同样注入身份（原语直调不能与 Tool 口径不同） |
| `internal/engine/hook.go` | `applyHooks(ctx, rc, …)`、`notifyError(ctx, rc, …)`；Hook 的 host 由 `hostFor(tool, nil)` 改为 `hostFor(tool, rc)` |
| `internal/engine/promote.go` | 传 `nil` rc（晋升是注册期动作，没有可继承的执行上下文） |
| `internal/engine/subscriber.go` | 投递动作传 `context.Background()`（见 8.2） |
| `internal/engine/lua.go` | `ctx.caller` 无条件注入（`subject` / `app_id` / `via`） |
| `internal/engine/process.go` | `input.ctx.caller` 在 `withHostCapabilities` 合并 |
| `internal/mcp/mcp.go` | `startFlowExec` 显式传 `context.Background()` 并留标记注释 |
| `internal/mcp/guide/atoms.md` | 记入 `ctx.caller` 及其「不是能力」的定性 |

测试：`internal/engine/callctx_test.go`，6 例全绿；全量 `go test ./internal/...` 无回退。

### 8.2 与设计的三处偏差（都是实施时撞出来的）

**偏差一：`applyHooks` 必须拿 `rc`——设计里没写。**

设计的 §3.2 说「注入只有 `hostFor` 一处」。实现时发现 **step 级 Hook 的宿主是
`e.hostFor(tool, nil)` 构造的**（`hook.go` 原第 105 行），rc 传的是 nil，身份根本进不去。
这不是「再加一处注入」——否则又多一份前缀/身份判定。**正确做法是透传 rc**，让 hostFor 照旧
从 `rc.caller` 取。故 `applyHooks` / `notifyError` 都加了 `rc` 参数。
**教训：设计文档写「只有一处」之前，得先把所有建宿主视图的点列全。**

**偏差二：注入点实际是两处，不是一处。**

`hostFor`（Tool 载体）与 `primitiveHost`（原语直调）都会建宿主视图，两处都得注入，
否则「同一个 step 里，直调 `db.query` 看不到身份、走 Tool 就看得见」——这种不一致最难查。
两者都只读 `rc.caller`，**语义上仍是一个来源**。

**偏差三：订阅投递刻意不带身份。**

`subscriber.go` 投递事件时重调度目标 Tool，用的是 `context.Background()`。
这是**有意为之**：一个事件的来源不该冒充「发起投递的主体」。设计文档没提这条，
记在这里免得日后有人「顺手补上」造成身份混淆。

### 8.3 已知边界（不假装解决）

- **嵌套 `ctx.call` 不继承身份。** `cap.Resolver` 的签名是
  `func(target, input, via)`，拿不到 `rc`；身份要进嵌套调用得改 Resolver 接口。
  当前「直接调用链」（Tool 本体与其 Hook）已完整覆盖，鉴权场景够用。
- **公开入口里 `RunFlowExec` / `RunFlowWithVars` / `RunFlowResult` / `RunFlowResultObserved`
  仍传 `context.Background()`**——与改造前同语义（无身份）。缺的是**调用点**，
  不是通道；下一轮把各执行面接进来时改的是这几处调用。

### 8.4 验收进度

| # | 判据 | 状态 |
|---|---|---|
| 1 | Lua Tool 里 `ctx.caller.subject` 输出调用主体 | ✅ |
| 2 | process Tool 的 stdin 收到 `ctx.caller`，且不破坏 input_schema | ✅ |
| 3 | `--scope <appID>` 下跨应用调用被拒 | ⬜ ② |
| 4 | 不传 `--scope` 时行为与今日完全一致 | ✅（全量测试无回退） |
| 5 | caller 无法经 vars 覆盖（负向） | ✅ |
| 6 | MCP 身份接入口 | ⬜（§3.5 决策已定，待接） |
| 7 | 全闸门 `scripts/gate.sh` 绿 | 待闸门结果 |

### 8.5 端口自动分配的实施（② 的第一半，2026-09-26）

用户定调「全链路分配，就得有真实端口查询」。② 拆成两步，先做便宜且独立的那一半。
**结论：承载真实端口的通道本来就有一半现成 —— `${steps.x.output.y}` 模板语法
（`authoring.md`，写了还会自动推断 `depends_on`），所以 proxy.target 不需要新机制，
缺的只是「谁把真实端口交出来」。**

已落地（第一块：`static_site.serve` 与 `gear serve`）：

| 环节 | 做法 | 落点 |
|---|---|---|
| 挑端口 | **内核**挑（`net.Listen` 的 `port:0`），不是自己从基址顺延 | 有 TOCTOU 窗口：试成功→释放→被抢，两个应用都报成功、一个是死端口 |
| 回读 | `ln.Addr()` 回真实端口，返回值 / 键 / url **一律不信入参** | `plugin/serve-sidecar/main.go` |
| 查询 | 新增 `server.WithOnListen` 回调；`server.ListenAndServe` 改 `net.Listen`+`Serve` | `internal/server/server.go` |
| 上报 | `gear serve` 在 stdout **第一行**吐 `{"port":N}`（banner 因此后移） | `internal/cli/serve.go` |
| 传递 | 长驻子进程首行 JSON 当 step output ⇒ `${steps.api.output.port}` | `process.go` / `runtime.go`（`OutputReporter`） |
| 落库 | service step 补 `rc.setStep`（原先**整个缺失**） | `supervise.go` |

**实施时撞出来的三点（设计阶段没预料到）**：

1. ★ **`supervise.go` 的 service 分支从不调 `rc.setStep`** —— 一次性 step 有 output，
   长驻 step 根本没有。所以「长驻进程拿不到 output」不是「gear serve 没上报」，
   而是整条输出通道对长驻 step 缺失。这个缺口比原估计大。
2. **首行有竞态**：`rt.Start` 返回时子进程往往还没打印第一行（要先 listen），
   而调用方紧接着就读 output。故 `OutputReporter.Output()` 是有界等待
   （`serviceFirstTimeout` = 2s），且「进程已退出 / 已被我们杀掉」时提前收工——
   否则每个不报声明的 service 都要白等满 2s。
3. **`kill` 后 `ProcessState` 仍是 nil**（要等 `Wait` 才填），提前收工判据必须自己记
   `killed` 标志，否则这个优化形同虚设。

**两处故意不做（不假装解决）**：

- `service ext`（coord 会话）的 Proc 是侧车进程，stdout 是协调协议而非 step 声明
  ⇒ 其 output **恒为 nil**。下游引用会报错，而不是拿到查不出原因的空串。
- 拿不到端口号时返回**空串**让调用方跳过打印，绝不写 `{"port":0}`——0 是会被下游
  （测试直接抓到过这条：`--addr :0` 未展开时 `SplitHostPort` 会给出 `"0"`）当真的假值。

### 8.6 ① 落地后：应用该拿这个身份做什么（FR-1 样板，2026-09-28）

① 只保证「**调用主体被如实送到该去看的地方**」，不保证应用一定会看。Q1 拍板把授权归回
业务层（走 Hook），于是本轮交付的是 `examples/auth-app/` ——一份**零内核改动**的官方样板，
链路是：登录 Tool（`crypto.password_verify` 验口令 + `crypto.jwt_sign` 发令牌）→
调用方把令牌当 `Authorization: Bearer <令牌>` 的主体 → 引擎原样放进 `ctx.caller.subject`
→ `before_step` 守卫 Hook（`criticality: critical`，`crypto.jwt_verify` 验签，失败即拒）
→ 受保护 Tool 执行。

这份样板**反过来钉死了 ① 的三条边界**，写任何鉴权代码前值得先读：

1. **守卫读的是 `ctx.caller.subject`，它就只等于令牌原文。** `server.identity.go` 的
   `subjectOf` 取「Bearer 之后的全部内容」并按空格**不切分**——填错了多余内容不会被静默修正，
   会变成一个奇怪的主体，守卫立刻拒绝。这是刻意的：静默修正比原样带回更难查。
2. **身份在 Flow 内的传播是「继承发起方」，不是「每次调用重新解析」。** 所以在 Flow 里内联调
   一个受保护 Tool，继承的是发起这次 Flow 的 caller；CLI / MCP 发起时那是空身份，**守卫会拒**。
   端到端验证要走 HTTP 那条路（`POST /api/tools/{id}/run` 带 Bearer），别在 Flow 里假装能过。
3. **守卫的密钥不能做入参。** 守卫由 Hook 触发，入参只能来自 Flow step 的 `input`；
   那样「直接 `POST /api/tools/<受保护>/run`」这条最常用的路永远拿不到密钥。密钥的正规去处
   只有 secret 库（`ctx.secret.get("AUTH_SIGNING_KEY")`）。缺密钥 = 拒绝，不是跳过校验。

回归保护在 `internal/artifact/auth_sample_test.go`（直接读样板的 `bundle.yaml`，不是另抄固件）。

---
### 8.6 ② 命名空间守卫的实施（2026-09-26）

§4 的设计按「默认不开 + 显式 `--scope` 开关」落地。**落点比原估算少得多**：scope 不是按请求
的（解析点 `cap.Resolver` 的签名里没有 context），而是**进程级**的引擎字段 `Engine.scope` +
`WithScope`，所以 18 处公开入口**一个都不用改签名**——只有解析点需要它。

| 环节 | 落点 |
|---|---|
| 前缀规则 | `model.App.Owns(id)` —— 全仓唯一一份，写入期的 `checkNamespace` 与运行期守卫同调 |
| 作用域 | `Engine.scope` + `WithScope(scope)`；`guardScope(id) error` 是唯一判定入口 |
| CLI | 根命令的持久 flag `--scope`；`cliEngine` 一处 `engine.WithScope(cliScope)` 即全贯通（tool run / flow run / app run / app start / serve / mcp 都经它） |
| 派生进程 | `app.StartOptions.Scope` → `deriveRunArgs` 拼进 `app run` 的 argv |

**插守卫的五个解析点**（比 §4.1 估算的「4 个全裸」多一个；第 5 个是实跑补的）：

| 解析点 | 拦什么 |
|---|---|
| `runFlow` 入口 | 本进程跑的 Flow（Flow 自身也是别的应用的对象） |
| `stepcall.go` `resolveStepCallView` | step.call 解析出的 Tool |
| `engine.go` `newResolver` | `ctx.call` 的 target —— 解析出的 Tool **与** Flow |
| `hook.go` ×3 | Hook 的 action 解析出的 Tool / Flow |
| `RunToolExec` | 直接点名执行一个 Tool（`gear tool run` / MCP `gear_run` / REST / event 订阅都汇到这里） |

（第 5 个是 2026-09-26 **实跑**时补的。详见下面第 5 点。）

**实施时撞出来的五点（§4.2/§4.3 都没预料到）**：

1. ★ **原语直调必须排除在守卫之外**，否则开了 scope 的应用连自己的入口都跑不起来。
   mindstorm 的 `mindstorm.up` 同时调 `mindstorm__config`（registry 对象）与 `static_site.serve`
   （原语）——按「非本应用即拒」，这条 Flow 一开 scope 就死。
   ⇒ 守卫只拦 **registry 对象**（能查到 id 的），**不拦原语**。守的语义是「别碰别人应用的
   定义」，不是「别用平台」。
2. **entry_flow 必须豁免**（`guardFlowScope` 增补了 `guardScope` 的 Flow 版本）。清单里声明的
   入口 Flow 按约定可以**不带前缀**（`artifact.Composition` 同样豁免它），少了这条，
   `gear app start --scope mindstorm` 连自己的入口都跑不起来。
3. **scope 必须是进程级的，不能是「按请求」的**。`gear serve` 的引擎是长驻共享实例，而
   `cap.Resolver` 的签名 `func(target string, input map[string]any, via []string)` 里没有
   context ⇒ 按请求切换作用域在那个面上**做不到**。这与推荐拓扑一致（每租户一个进程 +
   独立 GEAR_HOME，一进程一应用）。将来真要「一进程多应用」，该改的方向是把 scope 迁到
   `cap.Caller.AppID`（① 的身份链路里已有这个字段，只是水位为零），而不是补第二个字段。
4. **派生进程的 argv 必须带 `--scope`，否则守卫在最该生效的那一层失效**。
   `gear app start --scope x` 的父进程只做「登记」，真正跑 Flow 的是它 `app run` 派生出去的
   监管进程；漏传 `--scope` 不会报错，父进程照常打印「已启动」。故把 argv 拼接收成
   `deriveRunArgs` 并单测（`TestDeriveRunArgsScopePropagated`）——「必须传」变成被测试盯住的约定。

5. ★ **落点要按「代码路径」数，不能按「语义」数。** 前四处都在解析链路上，而 `RunToolExec`
   **自己拼 `resolvedCall`，从 `resolveStepCallView` 直接跳过** —— 实跑一测，
   `gear tool run --scope appa appb__ping` 居然执行成功了，与「一处注入即全贯通」的判断
   直接冲突（CLI 测试当时只跑了 Flow 面，没测 `tool run`，所以分包测试全绿）。
   补法：守卫放进 `RunToolExec` **内部**，于是 MCP `gear_run`、REST、event 订阅三条调用方
   一并覆盖——**收口在「谁执行」而不是「谁解析」**。
   ⇒ 教训：凡是「自己造 out 的调用点」都得单独数一遍，凡是加了 `resolvedCall{...}` 字面量
   的地方都得回头看一眼解析点清单。

**已知边界（不假装解决）**：

- **`ctx.call` 路径上的 `SCOPE_VIOLATION` 码会丢。** 越过 Lua 边界时错误被
  `L.RaiseError("call 失败: %v", …)` 拍成字符串，结构化诊断只剩文本（`TestScopeGuardBlocksForeignCallTarget`
  因此只断言文本）。拒绝发生了、原因看得到，但智能体没法按码查手册自愈。修它需要在 Lua 层
  保留错误对象再回传——独立的一条工作。
- **原语的注册表本身不在守卫范围内**：加了 `--scope` 也挡不住 `db.query` 读到别的库的内容。
  那条是「平台能力」，不是「别的应用的定义」，两者不能混为一谈。
- 作用域是进程级的 ⇒ **`gear serve --scope x` 对这个进程的所有 REST 请求一律生效**，
  不能按请求分不同应用。要按请求分，得先把 `cap.Resolver` 的契约加上 context（breaking change）。

对应需求条目：`REQ-APP-SCOPE`（`REQUIREMENTS.md`）。

---

## 6. 验收（② 的判据，尚未开展）

| # | 判据 |
|---|---|
| 1 | Lua Tool 里 `print(ctx.caller.subject)` 输出调用主体 |
| 2 | process Tool 的 stdin 收到 `ctx.caller`，且**不破坏 input_schema** |
| 3 | `--scope <appID>` 下 `gear_run(kind=tool, id="其他应用__x")` 被拒 |
| 4 | 不传 `--scope` 时行为与今日**完全一致**（现有测试不改） |
| 5 | caller 无法经 vars 覆盖（负向测试） |
| 6 | MCP 不带 `--trust-caller-as` 时 `as` 被忽略且 caller 为 anonymous |
| 7 | 全闸门 `scripts/gate.sh` 绿 |

> **2026-09-26 追记**：本表是 ② 立项时写的。其中第 1、2、5、6 项属 ①（已结论化，见 §3）；
> 第 3、4 项随 ② 的实施结论化——见 **§8.6**；第 7 项即闸门本身。2 与 6 这两条判据设计于
> 「`@trust-caller-as` 是 MCP 面的显式开关」的假设下，实施时该开关并未实现，故两条的**判据对象**
> 与最终口径不同（现为 `TestMCPWritesLeaveActorEmpty` 一类「口径变了要显式改」的测试）。

## 7. 文档同步（改代码时必做）

- `MCP_AGENT_DESIGN.md` §6.1：`toolBudget` 13→15 已是**当前值**，但 `internal/mcp/mcp.go:1-33`
  的包注释仍写「工具面（13 个…）」——这处已经过期，本次一并修正。
- `internal/mcp/guide/errors.md`：新增错误码必须走 `engine.Diagf(`，
  **不能**自造 `diag()` 短名包装（`doc_audit.py` 靠调用形态反查，包一层就从审计里消失了）。
- `REQUIREMENTS.md`：回填 `REQ-APP-SCOPE` 一类条目为 `implemented`。

---

## 9. 重新评估（2026-09-26，用户问「都做完了吗」）

### 9.1 状态盘点

| 项 | 状态 | 备注 |
|---|---|---|
| ① 身份链路 | 通道完成，水位为零（见 9.2） | `callctx_test.go` 6 例绿 |
| 端口分配（两块） | ✅ 完成 | 侧车 `port:0` + service 长驻上报真实端口 |
| ② 命名空间守卫 | ❌ 未动 | 但优先级下调，见 9.2 |
| `gear_apply` 落库 App 清单 | ✅ 已实现 | `apply.go:43-47` → `reg.UpsertApp(a)`；任务状态陈旧未翻转 |
| ③ 审计表 | ❌ 未动（等 ① 有调用方才有意义） | |
| ④ 留旧/回滚 | ❌ 未动 | 优先级上调，见 9.2 |
| 全闸门 | ✅ 绿 | `build/testlogs/gate-20260926-100858.log`：七包覆盖率 81-91% 全过 |

> **2026-09-26 追记**：§9.1 表中 ① 已全部落地（见 §3），③ 与 ④ 亦已实施
>（③ 见 **§10**、④ 见 §5），保留原表格文字以存当时的判断过程。② 仍未动。
> §8.4 验收进度表中第 3、6 项亦已随后续改动结论化。

> **再追记（同日稍晚）：② 亦已实施**（见 **§8.6**），上表「未动」二字过期。
> §9.2 之「二」当时判断「除非改成多应用共享进程，否则 ② 推后」——那个前提现在依然成立
> （仍是「每租户独立 GEAR_HOME」拓扑），只是 ② 以「默认不开、显式开关」的形态落地了，
> 成本远低于当初的估算。
### 9.2 三处新发现（改变了优先级）

**一、① 现在是「管道通了、水位是零」。**
全仓 `WithCaller` 的调用点**只在 `callctx_test.go`**；`internal/server` 与 `internal/mcp` 都没有写入方
（`mcp.go:503` 只有一行「接身份时改这里」的标记注释）。
⇒ 今天 `ctx.caller.subject` **恒为空串**。Hook 鉴权代码现在写上去，拿不到人。
缺的不是通道能力，是**调用方**——最小的一步是 REST 的 `handleRunTool` 里补一行
`ctx = engine.WithCaller(ctx, ...)`（存在 `*http.Request`，真身份就在手边）。

**二、② 的必要性被高估了。**
路线甲是「每租户一个 gear 进程 + 独立 `GEAR_HOME`」，而 registry 落在 `<GEAR_HOME>/registry.db`
（`guide/overview.md:101`）。**不同 `GEAR_HOME` = 不同库 ⇒ 命名空间天然不共享**，
「应用 A 摸到应用 B 的对象」在这个拓扑下根本不成立。
②（进程内 fail-closed 守卫）只对**一个进程内跑多个应用**有意义。
**结论：除非改成多应用共享进程，否则 ② 推后；别让「安全」这个词把它排在 ④ 前面。**

**三、④ 应提到 ② 前面。**
发布后自助构建 ⇒ 智能体必然会改崩某个应用，而 `registry.upsert` 是整行覆盖、无历史快照
（§5）。没有退路时，「构建者能力」等于「一键毁掉自己」。
这个故障**一定会发生**（L3 交付给别人之后，租户自己改崩是常态），跨租户越权则是 rare。
按发生频率排序：④ > ③ > ②。

### 9.3 对 L3（Palantir 式多用户产品）的重新评估

L3 的判据是「交付给别人之后，租户自己多人共用同一套 gear」。三处新发现合起来
改的不是「L3 要不要」，而是**L3 的前置条件顺序**：

1. **可回滚是共用的前提，不是共用的装饰。** 原以为「多人共用 ⇒ 先要守卫（②）」，
   实际是：一个改崩了找不回的系统，多人共用的第一天就会有人删掉别人的东西。
   ⇒ ④ 排在 ② 前面（§9.2 之三），也就是 L3 真正的门槛。
2. **② 只在「一个进程内跑多个应用」时成立。** 路线甲（每租户独立 `GEAR_HOME`）
   下命名空间天然不共享，② 的 fail-closed 守卫等于是给不存在的问题上锁。
   ⇒ 若 L3 走路线甲，② 可以**永远不做**，L3 不受影响。
3. **③ 的价值随拓扑而变。** 路线甲下每个租户一份历史，actor 恒定是本人，
   审计价值为零（成本仍为正，因为要 ALTER + 接真）。只有「多身份落进同一进程、
   同一 `GEAR_HOME`」时，「谁改的」才有追责意义。
   ⇒ ③ 现在做，图的是**将来接可信身份时不用改表**；不图它今天的审计价值。

**结论：L3 不在本轮。** 本轮交付的是「① 通道 + ④ 退路 + ③ 记录」——这是 L3 的地基，
不是 L3 本身。把 L3 拆成两个可独立验收的问题：

| 问题 | 本轮 | 判据 |
|---|---|---|
| (a) 单用户自助构建的安全性 | ✅ 完成 | 改崩可从历史还原（`TestRestore*` / `TestDeleteCanBeRolledBack`） |
| (b) 多用户共用时的责任归属 | ⚠️ 只留位置 | actor 能如实记录；**追责留给将来接可信身份**，不由本轮伪造 |

(b) 的诚实边界要写清楚：MCP 面永远不参与追责，CLI 面是操作系统用户名（不是 gear 身份），
REST 未授权时是空串。**这些不是「漏接」，是「如实」**——见 §10.3。

### 9.4 第三次评估：地基铺完之后（2026-09-26 晚）

用户问「现在重新评估是否适合做 Palantir 了」。与 §9.3 相隔半天，① ②③④ 与 App 清单、
产物交付**全部落地**。重新盘了一遍全仓，结论**没变，但理由完全变了**——这一点值得写下来，
因为它说明了「L3 的门槛不在我们以为的地方」。

**一、卡点从「机制」转移到了「信任边界」。**
本轮做完的四件事（身份通道、命名空间守卫、留旧回滚、审计留痕）**都是机制层**，
而机制层的共同前提是：**能连到 `127.0.0.1:8787` 的就是主人**。这个前提在「单用户、自己的
机器、自己起服务」下成立；一旦「交付给别人、多人共用同一套安装」，它**直接不成立**——
这个前提本身，就是 L3 里第一件要拆掉的东西。（§9.3 以为 L3 的门槛是「④ 可回滚 / ③ 追责」，
做完才发现：那两件解决的是「改崩了怎么办」，不是「门外的人是谁」。）

**二、最硬的阻塞是「零认证」，而不是「没有权限模型」。**

| 缺口 | 事实 | 对应 §9.3 的哪条 |
|---|---|---|
| ① 30 个端点零中间件，`PUT /api/secrets/{name}` 写入即得全库明文 | `server.go:266-304` | §9.3 未提及 |
| ② 无 TLS（`http.Server{}` 无 `TLSConfig`，`serverBaseURL` 只拼 `http://`） | `server.go:221-237` | §9.3 未提及 |
| ③ 同源判定无条件放行所有 `localhost` origin ⇒ 本机任意网页可代用户操作 | `httpsafe/origin.go:35-38` | §9.3 未提及 |
| ④ 执行记录与日志无归属过滤（`execs.List()` 全表返回，`WHERE` 里没有 actor） | `server.go:837/849/853/858`、`execstore.go:164/285` | §9.3 未提及 |
| ⑤ MCP 面恒无身份、产物交付链路不写 actor；`Meta` 结构**没有 exporter 字段** | `mcp.go:497-508`、`artifact/apply.go:30` | §9.3 未提及 |
| ⑥ 无 RBAC：`registry.Upsert*`/`Delete*` 对身份零检查 | `registry.go:268/333/483/524/…` | §9.3 部分覆盖 |

第 ③ 条最反直觉：`localhost` 放行在单用户下是**合理的**（本机即用户本人），但它意味着
**任何托管在 `http://localhost:3000` 的恶意页面都能以用户身份调 gear**。多用户产品是浏览器产品，
用户在同一个浏览器里开 gear 和一个本地开发服务器，这条防线就在真实攻击面上。

**三、因此：现在适合做什么、不适合做什么。**

| 形态 | 需要做什么 | 判断 |
|---|---|---|
| **路线甲**（每真人一份 GEAR_HOME + 独立进程） | 主要还是**运维分发**：隔离由部署拓扑承担，B1/B4 多数可不碰 | ✅ 可以做，代价≈部署复杂度 |
| **共享安装 + 授权模型**（≈ Palantir 那个量级） | 认证（本地账号 / OIDC）+ 授权粒度 + 每请求执行身份 + 归属过滤 + TLS | ❌ 现在做工程量大，且撞 §4.3 已定的「scope 是进程级」 |

**结论：现在适合做「路线甲形态的小规模多用户」（几个真人、各自一份 home），
不适合做 Palantir 式共享安装。** 后者要做的第一件事不是加守卫，是**先把「谁在敲门」这件事
立起来（认证）**——而认证正是 L3 里唯一需要真做产品决策的一环：账号体系走本地口令、
外部 IdP，还是「每个 Unix 用户一个 home」的零账号路线。这个决策没定，写代码就是返工。

**四、现在就能动手的三件便宜事**（都不需要先做认证决策）：

1. **归属过滤**（B5）：`executions` 已有 `actor` 列，只缺三个 handler 加一层 `WHERE actor = ?`——
   「数据隔离」里最便宜的一块。
2. **产物溯源**（B7）：给 `artifact.Meta` 补 exporter、并让 `ApplyBundle` 写 actor（当前签名
   `ApplyBundle(reg, b, overwrite)` 三个参数里没有 caller——**导进来的东西与自己造的在库里完全
   无法区分**）。顺带修 `artifact.go:24` 那句「元信息：…**导出者**」——注释承诺了，结构体里没有。
3. **同源策略收紧**（B3）：不再无条件放行 `localhost`，改成「无 Origin 放行 + 有 Origin 必须
   注册白名单」或给状态改变端点加一次性令牌。

三件加起来远小于②的工程量，但**它们做完后，多用户才第一次有「隔离」这回事**——
在此之前，多用户只是「同机多了一份 home，而 API 门是敞的」。

9.1 表里 ③ 写的是「未动（等 ① 有调用方才有意义）」。① 落地后这个前提成立，③ 随即实施。

### 9.5 对 §9.4 的反驳：暴露面收敛到单个端口（2026-09-26 晚）

用户提出：*「最后提供服务的端口是可以映射出去的，用户访问本地端口我认为应该是可控的，
例如服务在 A 端口服务，我就只暴露 A 端口。」* 这条针对的是 §9.4 排在首位的「零认证」。
**反驳成立，§9.4 的排序理由要改。**

**（A）被削弱的部分。** §9.4 的论证起点是「能连到 `127.0.0.1:8787` 的就是主人」
（`root.go:55` 默认 `127.0.0.1:8787`）——这句话预设了「服务默认对可路由网络可达」。
改成暴露面收敛到单个端口后，默认姿态就从「谁找到端口谁就是主人」变成「默认关闭，开口是主动决定」。
于是认证从「拆一扇 hinge 早掉了的门」降为「给一扇你主动决定打开的门装锁」——必要性与工作量都下调一档。

而且这条纪律仓库里**已经有了，只是只执行了一半**：MCP 面有 `checkBindAddr`（`mcp.go:265-281`），
非回环地址默认拒绝、必须显式 `--allow-remote`，放行时还往 stderr 打一行「当前无鉴权」的警告。
REST 面**没有**等价物——`gear serve --addr 0.0.0.0:8787` 会静默把 30 个端点（含
`PUT /api/secrets/{name}`）铺到全网卡。写 registry 与读明文库的那一面恰好没装闸，
写操作更少的那一面装了。这让「零认证」从一句判断变成**一个能立刻修的口子**：
给 `serveCmd` 补上与 MCP 同源的 `checkBindAddr` / `--allow-remote`，约 20 行。

> **已修（2026-09-26）**：REST 面的闸补上了。判定提升为共享函数 `httpsafe.CheckBindAddr`
> （MCP / serve 单一真相源），`gear serve` 默认拒绝非回环地址（**含空 host `:8787`**，
> 在 `net.Listen` 里等价 0.0.0.0），`--allow-remote` 显式放行并留 stderr 警告。
> 测试：`httpsafe/bind_test.go`（B-bind-1..4）+ `TestServeRefusesNonLoopbackBind`
> + `TestServeCmdConstruction` 的 flag 断言。本段其余文字为修复前论证，保留原样。

**（B）不受影响的部分。** 端口策略答不了这四条：

1. **同机邻居**：端口映射解决「网络可达性」，不解决「同机同身份」。同一台机器上的任何进程、
   别的用户会话、任何一次恶意安装包安装，都能连 `127.0.0.1:A`。
2. **浏览器跨站**：`httpsafe.OriginAllowed`（`origin.go:26-40`）无条件放行所有 `localhost` origin。
   暴露面一旦收敛到 A 端口，这条反而更突出——既然大家都从 A 端口进，那么一个被托管在
   `http://localhost:3000` 的普通页面就能以「本机来源」的身份调 gear。
3. **actor 退化（最重要）**：A 端口一旦对他人可达，访问者必然共享同一个 OS 身份，
   `executions.actor` 与 `definition_history` 的 actor 全是同一个 `user.Current().Username`
   ——「多人」在数据模型里根本表达不出来。不是「少了认证」，是「没有字段能装答案」。
4. **单点失败代价变大**：攻击面收成一个点，同时也意味着「打通 A 端口」一次即得全部凭据。
   收窄暴露面同时收窄了失败的上限与下限。

**（C）修订后的排序。** 认证不再是「L3 的唯一硬阻塞」，而是「一旦决定多人就必做、且很便宜」。
§9.4 那三件便宜事里，**第 3 件（同源策略收紧）升到最先做**——它做的正是「给 A 端口加访问门槛」
这一层，也就是用户说的「暴露是可控的」，而它现在是最薄的一环；把 `checkBindAddr` 补进 REST 面
是同一件事的另一个面。

**（D）仍未决的。** 暴露范围本身就是形态决策，两种答案给出的结论完全不同：
- **A 端口只对本机可达 + 每人一份 `GEAR_HOME` 独立进程（§9.4 路线甲）**：端口策略直接闭合了
  整个安全论题，**认证可以永远不做**。用户这条判断在该形态下是完整答案，不是折中。
- **A 端口要给别人访问（共享安装）**：访问者必然共享同一个 OS 身份，`actor` 必然退化，
  认证从「可选项」升为「数据模型的前置条件」。

所以这一步不用推：先说清 A 端口最后要不要对他人可达，认证要不要做就自然定了。
**落点只有两处，都不是新建一张表**：
- `definition_history`（④ 的留旧表）补上「删除」这一半——④ 只把快照下沉进了 `upsert`，
  而**删除根本不走 `upsert`**，历史里查不到任何一次删除。
- `executions` 补 `actor` 列——执行记录原本只有 `owner_pid`（哪个进程），没有「谁」。
---


## 10. ③ 审计表（2026-09-26，已实施）

引导段里说的两处落点，实施结果如下。**落点不是一张新表**，而是把一个已存在的表补完整、
再把一列现有的空列接上真值。理由：审计信息的消费方已经是 `gear history list` 与
`GET /api/history`，新建一张平行表只会把「改崩了往哪回滚」变成一道选择题。

### 10.1 三个现状

| # | 现状 | 后果 |
|---|---|---|
| 1 | `registry.WithActor` 全仓只有测试在用——`cli/history.go` 里只剩一行 `var _ = registry.WithActor` 压 staticcheck 告警 | actor 列**生产恒空**：表在、字段在、永远是 `''` |
| 2 | `executions` 有 `owner_pid`（哪个进程写的），没有「谁」 | 执行记录能回答「哪个服务」，回答不了「哪个用户」 |
| 3 | 删除**根本不走 `upsert`**（`DeleteTool/Flow/Hook/App` 各自直接 `DELETE`） | ④ 把快照下沉进了 `upsert`，而删除绕开了它 ⇒ 历史里**查不到任何一次删除** |

第 3 条最隐蔽：写有历史、删没历史，于是「时间线」是一个残缺投影——
`restore_from=<seq>` 能把改崩的对象救回来，却救不了被删掉的对象。

### 10.2 做什么

**一、删除也留痕（同一个事务）。** 新增 `deleteWithSnapshot(table, id, opts)`：
在一个 `withTx` 里先 `snapshotBefore(tx, …, noteDelete)` 再 `DELETE`。

- 四个 `Delete*` 改为收 `opts ...UpsertOption` 并转调它；`PromoteToolVersion`
  同样收 `opts ...UpsertOption` 透传给 `UpsertTool`。
- **删除行的 `data` 保留被删内容**（`snapshotBefore` 落的是整行，不清理）。
  于是 `RestoreSnapshot(seq)` 恰好把对象连同内容一起带回来——
  **「撤销一次删除」不需要新代码**。
- **守卫与留痕必须原子**：`snapshotBefore` 与 `DELETE` 在同一事务 ⇒ 被引用拒绝的删除
  历史行数不变（`TestGuardedDeleteLeavesNoTrace` 盯的就是这条）。
  否则时间线会凭空长出一格，那一格对应的对象却从未存在过。

**二、`executions` 补 `actor` 列。** `Execution.Actor` 字段 + `migrate` 里
`ALTER TABLE executions ADD COLUMN actor TEXT NOT NULL DEFAULT ''`。
老记录补空串是**如实**——那时确实没有主体，填 `-` 等于撒谎。

**三、actor 接真。** 全部写入侧带 `registry.WithActor(...)`：

| 面 | 取值 |
|---|---|
| REST 三个定义写入面 + 三个删除面 + promote 面 | `subjectOf(r)`（Bearer 解析出的 subject） |
| CLI（`tool.go` / `flow.go` / `hook.go` / `serve.go` / `history.go`） | `cliActor()` = `user.Current().Username` |
| MCP（`tools_write.go` / `mcp.go`） | 恒空串 |

CLI 侧做成短函数 `act()`，是为了让十几个写入点不至于有人偷懒漏写——
漏写的代价不是少记一个字，而是整条审计链路降级成「系统账号」。

**实施时撞出来的两点（设计阶段没预料到）**：

1. ★ **下沉进事务后，「删除不存在」的判据必须自己拿回来。** 原实现是
   `res, err := db.Exec(DELETE…)` 后查 `RowsAffected()==0 ⇒ ErrNotFound`；换成
   `deleteWithSnapshot`（快照与删除同一事务）后**没人再查影响行数** ⇒
   DELETE 对不存在的行「成功」却影响 0 行 ⇒ 重复删除变成静默 no-op
   （`TestHookLifecycle` 在闸门里逮到：`期望报错：重复 remove 不存在的 Hook`）。
   修法是查 `RowsAffected`，返回 `ErrNotFound` 并**让 `withTx` 回滚**——
   连快照都不落，否则「我删过两次」会在时间线上变成假事件
   （`TestRepeatedDeleteErrorsAndLeavesNoTrace`）。
2. **`definition_history` 漏出 CLI 的「非 gear 表」白名单。**
   `cli/root.go` 的 `gearRegistryTables` 只在里面认得 tools/flows/hooks/apps，
   于是每个跑过 history 的库，每次 CLI 调用都刷一行
   「已含非 gear 表 [definition_history]」。这张表是 gear 自建的，与那四张同级。
   （同一个坑 2026-09-26 已经踩过一次：加 `apps` 时漏同步。见 §8 的成例。）

### 10.3 三条口径（已写进代码注释的部分）

1. **空串 = 发起方本来就没有身份，不是「查不到」。**
   所以 `gear history list` 显示 `-`（区分「无」与「未记录」），
   REST 响应 `omitempty` 省略该字段。**不要**在未授权时回退到某个默认主体。
2. **不传 actor 的写入仍然合法。** 这是本设计**接受**的代价（变长参数带来的）。
   想收紧就得加签名，而不是让调用方凭空编一个。
3. **MCP 面永远不参与追责。** `mcp.go` 里 `s.execs.Create(flowID, "")` 是刻意的：
   拿不到传输层身份就不许编一个出来。
   ⇒ `TestMCPWritesLeaveActorEmpty` 不是「不该通过」的测试，而是
   「口径变了要显式改」的测试。

### 10.4 不做什么

- **不新建 audit 表**——`definition_history` 已经是这张表，补完整即可。
- **actor 只记录，不鉴权**——加了 actor 不等于加了权限，这个决定没做。
- **不伪造主体**：不给 `owner_pid` 编一个用户名，不给老执行记录填 `-`。
- **不把 `owner_pid` 改成 actor**——「哪个进程」和「谁」是两个问题，都有答案时更有用。

### 10.5 验收

| 层 | 测试 | 盯什么 |
|---|---|---|
| registry | `TestDeleteLeavesTraceWithActor` | `note=delete` + actor 落库 + 对象确实不在了 |
| registry | `TestDeleteCanBeRolledBack` | 回滚删除把对象与内容一起带回来 |
| registry | `TestGuardedDeleteLeavesNoTrace` | 负向：被引用拒绝时历史行数不变 |
| registry | `TestRestoreLeavesTraceWithNote` | 恢复自己也是一次带 actor 的留痕 |
| server | `TestDefinitionWriteRecordsActor` | 带 `Bearer alice` → `alice`；无头 → 空串 |
| server | `TestDefinitionDeleteRecordsActor` | 删除带 `note=delete` 且带 actor |
| server | `TestExecutionRecordCarriesSubject` | 执行记录与 REST 响应都带主体 |
| mcp | `TestMCPWritesLeaveActorEmpty` | 负向对照 |

手工判据：`gear history list` 里能看到删除与恢复（`note` 分别为 `delete` / `restore`），
且 `actor` 列是操作系统用户名。

对应需求条目：`REQ-REG-DELETE-TRACE`、`REQ-AUDIT-ACTOR`（`REQUIREMENTS.md` §11 扩展）。
