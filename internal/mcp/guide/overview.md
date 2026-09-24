# overview：gear 是什么，以及它怎么运转

## 1. 机制内核，不是业务平台

gear 只提供**机制**，不提供**策略**：

| 它提供 | 它不提供 |
|---|---|
| 能力原语的统一入口与门禁（`ctx.*`） | 具体业务逻辑（那属于你自己的 Tool） |
| Flow 的编排与执行语义（并行/条件/长驻） | 领域 DSL 或行业模板 |
| Hook 的生命周期插入点 | 内置审批流、内置调度策略 |
| registry（Tool/Flow/Hook 的唯一真相源） | 内置业务实体（**没有 App 实体**，见 §5） |

## 2. 四层模型

```
能力原语（底座）              →  执行单元            →  编排      →  切面
http/call/db/secret/event/subscribe    Tool（可复用）        Flow         Hook
time/template/crypto（免门禁纯函数）       ↳ 或 step 直调「能力.方法」
ext.*（须注册 + 须声明）
```

- **能力原语**是宿主提供的能力，Tool 通过 `capabilities` 声明「我有权用哪些」，然后在 Lua / process 里经 `ctx.*` 调用。
- **Tool** 是「能力 + 业务逻辑」的封装，注册后可被多个 Flow 复用。
- **关键取舍**：Flow 的 step **可以直接写「能力.方法」**（`db.query` / `http.Request` / `ext.ai.complete`），不必先建 Tool。
  → **可复用的才建 Tool；一次性的直接直调。** 这条决定了你该不该产出一个 Tool。
- **Flow** 编排执行单元形成业务。
- **Hook** 在生命周期节点插入执行与门禁（`before_flow` / `before_step` / `after_step` / `on_error` / `on_success` / `before_promote`）。

四类能力的**精确边界**（最容易搞错的地方）见 `gear_guide("atoms")`。

## 3. 执行语义速查

| 语义 | 规则 |
|---|---|
| 调度 | 无 `depends_on` 的 step **同层并行**（默认上限 4）；有依赖的排到下一层 |
| 数据流 | `input` 用模板：`${vars.x}` / `${steps.<id>.output.<字段>}` / `${secrets.NAME}`；写 `${steps.*}` 会**自动推断依赖**，不必再写 `depends_on` |
| 条件 | `when` 是**表达式不是模板**：写 `steps.s1.output.n > 1`，不要写 `${}`；为假则**跳过该步**（不是失败） |
| 失败 | 任一步失败 → 取消后续层，整个 Flow 失败（除非 Hook 是 `best-effort`） |
| 长驻 | `mode: service` 的 step 常驻不退出；含此类 step 的 Flow 只能由 `gear_app` / `gear app run` 监管 |

## 4. 三种执行面 / 三种启动器

**执行面**：CLI（`gear …`）、REST（`gear serve` → `/api/*`）、MCP（`gear mcp`）。三者共用同一个 registry 与引擎——**一处定义，多入口执行**。MCP 只做协议翻译，不重复实现任何守卫。

**启动器**（谁来持有那个长驻进程）：

| 入口 | 持有者 | 生命周期 |
|---|---|---|
| `gear app run <flow>` | 人 / CI（前台阻塞） | 随终端或 Ctrl+C 结束 |
| `POST /api/apps` | `gear serve` 进程 | serve 一死，应用全死 |
| `gear_app(action=start)` / `gear app start` | **被派生出来的 `app run` 自己** | **意在**跨会话存活，但取决于结果里的 `escaped_job`：`true` 才不随 MCP / 终端退出；`false` 表示没能脱离调用方的进程沙箱，会随**发起启动的那次会话**结束被回收（`start` 同时给 warning）。别只看 `state:running` |

## 5. 「应用」的定义与一个完整范式

**没有 App 实体。** 应用 = **一个含 `mode: service` 长驻 step 的 Flow**，由 `gear_app` 以后台进程监管。

一个真实的四步范式（仓库里的 `plugin/web_ui_flow.yaml`）：

```yaml
id: web_ui.up
steps:
  - id: anchor                      # ① once：排序锚点（service step 不能作为依赖目标）
    call: web_ui.anchor
    input: {}
  - id: serve                       # ② 长驻：持有 ext.serve 侧车（静态站，类 nginx）
    call: ext.serve
    mode: service
    input: {}
  - id: api                         # ③ 长驻：起 REST 后端
    call: web_ui.api
    mode: service
    input: {}
  - id: site                        # ④ once：依赖 anchor → 落到下一层 → 侧车必已就绪
    call: static_site.serve         #    再向侧车下发 /start，把 dist 托管到 :8080
    depends_on: [anchor]
    input: {port: 8080, dir: build/dist, host: 127.0.0.1}
```

要点：**两个长驻 step 自己会阻塞**，所以「持有侧车」和「向侧车下指令」必须分成两类 step；
`anchor` 这个看似多余的 once 步是**必要的**——它把 `site` 推到下一层，保证下发指令时侧车已就绪。

## 6. `gear_app` 生命周期

| action | 行为 |
|---|---|
| `start` | 幂等检查 → 派生后台进程（**尝试**脱离本进程树）→ 立即返回；**结果里的 `escaped_job` 说明它是否真的脱离了**（`false` 时带 warning，进程只活到本次会话结束）；同 Flow 已有活跃实例时返回 `already_running`（不重复拉起） |
| `status` | 台账 + 存活判定 + 端口探活 → `running` / `exited` / `unknown` |
| `stop` | 终止进程树 → **端口复检**；收不干净返回 `ORPHAN_APP` + pid，**不谎报 stopped** |
| `list` | 全部台账（含历史）+ 遗留（`orphan`）项 |
| `prune` | 清理**确定已终止**的台账项（`exited` / `stopped` / `startup_failed`）。`running` 与 **`unknown` 一律保留**——unknown 的双判据互相矛盾，删掉可能把「还在跑却再无人可查」的进程变成真孤儿。只删记录，不杀进程；`dry_run: true` 可先预览 |

> 调试一轮下来台账会堆很多失败记录，`prune` 就是为「按应用批量清干净」准备的；
> 它不会碰还在跑的应用，所以想停应用仍然只能用 `stop`。
> 同一个能力在另外两个面也开着（同一份实现）：CLI `gear app prune [--dry-run] [--flow <id>]`、
> REST `POST /api/apps/prune`（body `{flow_id?, dry_run?}`）——人排障时不必绕道 MCP。
> 返回里 `state` 是**结案状态**（台账里的 `final_state`），`probe_state` 是**当下实时双判据**；
> 两者不一致是正常的（pid 会被系统复用），**判定一律看 `probe_state`**。

台账落在 `<registry 同目录>/apps/`。registry 默认 `<GEAR_HOME>/registry.db`（`GEAR_HOME` 未设时 `<HOME>/.gear`），故台账默认 `~/.gear/apps/`——**此路径与 cwd 无关**，gear 从哪个目录被拉起都一样。

## 7. 常见误解（直接省掉一轮返工）

| 误解 | 事实 |
|---|---|
| gear 有 App 实体，可以「创建应用」 | 没有。应用 = 含 `mode: service` step 的 Flow，用 `gear_app(start)` 运行 |
| `gear_run` 可以起长驻服务 | 不行，会被拒（`SERVICE_FLOW_SYNC_RUN`）；长驻只能用 `gear_app(action=start)` |
| `gear_app(start)` 会阻塞到应用结束 | 不会。派生后立即返回；`wait_seconds` 只是短等确认 |
| 应用会随会话结束而停止 | 不一定。它是独立进程，**原则上跨会话存活**——但 Windows 下若没能脱离调用方的 Job Object，结果会给 `escaped_job:false`，表示它随发起启动的那次会话结束被回收。此时**必须如实告知用户**，不要报「已在后台常驻」；无论哪种情况都要给出如何 stop |
| `stop` 返回 `stopped` 就一定收干净了 | 以 `ports_released` 为准；不一致会返回 `ORPHAN_APP` |
| `crypto`/`time`/`template` 要声明 capability | 不用，它们是免门禁纯函数；**声明了反而被拒** |
| `json` 是原子能力，可以直调 | 不是。`json` 只是 Lua 里的 `ctx.json.encode/decode`，Flow 不能直调 |
| `ext.*` 不用声明就能用 | 不行，`ext.` 前缀同样受门禁，且该 ext 须在 `extensions.yaml` 注册 |
| 能力不够就用 process runtime 跑脚本调外部程序 | 不行，那是绕过门禁。必须走 `gear_guide("primitive-gap")` 的上报流程 |
| 我可以「建议」一个带业务含义的原语 | 不合格（如 `ext.shipOrder`）。原语必须基础、通用、无业务名词；业务逻辑归 Tool |
| 扩展原子的凭据可以从环境变量读 | 不行，只从调用参数传；用 `${secrets.NAME}` |
| 写 `{"x": "string"}` 就算 schema | 不行，必须标准 JSON Schema 且顶层 `type: object` |
| `when` 里写 `${...}` | 不行，`when` 是表达式；`${}` 只用于 `input` |
| 侧车会自动被拉起 | 需要 `mode: service` 的 step 持有它；后续 step 经 `anchor` 排到下一层 |
| 相对路径随便写 | 依赖进程 cwd。**派生的应用继承的是 MCP/CLI 服务端的 cwd**，请用绝对路径或 `layout.workdir` 下的路径 |
