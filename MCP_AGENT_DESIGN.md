# MCP × 智能体应用创建方案（v1.2）

> **v1.2 修订要点（三项已定，按东东拍板）**
> ① **MCP 恢复「能启动应用」**——但实现方式改为**派生独立后台进程（detach）+ 立即返回**：MCP 是启动器调用者，**永远不是进程持有者**（§0.2 / §6.2⑤）。
> ② 新增 **原语缺口上报**机制：缺能力时**不许绕过**，必须按「基础通用、不参杂业务」的判据给出原语建议（§9.6 / 附录 G）。
> ③ 三项拍板落定：app = 后台进程（A′）、工具名一次性统一 `gear_` 前缀、密钥不开放写入（§13.1）。

> **落地状态（2026-09-21 回填）：v1.2 的 P0 + P1 已全部实现，`mcp.Version` 0.1.0 → 0.2.0。**
>
> | 面 | 状态 | 代码锚点 |
> |---|---|---|
> | L0 注入层：`ServerOptions.Instructions`（背景信息**只写一次**） | ✅ | `internal/mcp/guide/`（embed 同源：summary + 8 个 topic 的 `.md`）、`internal/mcp/guide/guide.go` |
> | L1 上下文层：`gear_guide(topic)` + `gear://guide/*` resources | ✅ | `internal/mcp/tools_read.go`、`internal/mcp/mcp.go`（`AddResource`） |
> | L2 工具层：19 动词级 → **12 意图级 `gear_*`**（读 5 / 写 4 / 跑 2 / 应用 1） | ✅ | `internal/mcp/tools_read.go`、`tools_write.go`、`tools_run.go`、`tools_app.go`、`common.go` |
> | `ServerOptions.Instructions` + `Tool.Annotations` | ✅ | 每个工具有 `Title` + `ReadOnly/Destructive/Idempotent` 提示 |
> | `gear_validate`（收 YAML、字段级诊断、不落库）+ 「一次报全」 | ✅ | `internal/engine/validate_static.go`（`ValidateFlowStaticDiags`，**签名不变的 fail-fast 包装保留**）、`internal/engine/diag.go` |
> | `gear_apply`（声明式、原子、幂等、`dry_run`、namespace 校验、冲突需 `overwrite`） | ✅ | `internal/mcp/tools_write.go`（`handleApply` / `persistBundle`） |
> | **`gear_app` + CLI `app start\|stop\|status\|list`（派生后台进程 + 台账）** | ✅ | 新增 `internal/app/launch.go`+`record.go`+`launch_windows.go`+`launch_unix.go`；`internal/cli/app.go` |
> | 台账双判据存活检测 + 孤儿只报告不清理 | ✅ | `internal/app/record.go`（`Probe` / `Orphans`） |
> | `gear_doctor`（含台账/孤儿检查） | ✅ | `internal/mcp/tools_read.go`（`handleDoctor`，回填真实的 `apps_dir`） |
> | `gear_catalog` 摘要化（不再吐全量 schema） | ✅ | `internal/mcp/tools_read.go`（`versions` 只给紧凑串；有机械守卫） |
> | `gear_run` 带 `wait_seconds`（异步化） | ✅ | `internal/mcp/tools_run.go` |
> | 结构化错误契约（附录 D）覆盖全部工具 | ✅ | `internal/mcp/common.go`（`toolError` / `toolErrorFrom`）、`mcp_errorpaths_test.go` |
> | `flow__{id}` 默认关闭（保留 `--expose-flows`） | ✅ | `internal/mcp/mcp.go`（`WithExposeFlows`） |
> | 描述体量红线（单 ≤240 / 合计 ≤1,800 rune） | ✅ | `mcp_test.go`: `TestMCPToolDescriptionsStayLean` |
> | 原语缺口上报契约（附录 G） | ✅ | `internal/mcp/guide/primitive-gap.md` + `UNKNOWN_CALL_TARGET` 诊断的 hint 直接指向它 |
>
> **仍未做（P2，与 v1.2 设计本身无关，属「可度量」范畴）**：① 智能体 eval 场景集（§12 的 E1–E7）尚未纳入 `plugin/verify.py`；② ~~`plugin/mcp_wire_probe.py` 仍然缺失~~ → **2026-09-21 已恢复**（stdio + `--http` 双模式，33 条判据全绿）；③ 原语缺口持久化 sink（§11 P2 已论证「默认不加」）；④ `gear_secrets(action:list)`（§11 P2 可选，与「密钥不经 MCP 写」的拍板不冲突，但非必需）。
>
> **恢复探针的当天就抓到一条真事故**（这条正是「文档声称的红线没人守」的典型）：`gear_run` 一执行，引擎的 `✓ step …` 进度经 `fmt.Printf` 落到 **stdout**，而 README §12.3 明写「stdio 下 stdout 只承载协议帧」——本地全绿、接严格客户端全废。修法：`Engine.SetProgressWriter`（默认仍是 `os.Stdout`，CLI 体验不变；`gear mcp` 改指 `os.Stderr`，见 `internal/cli/mcp.go`）。守卫两条：`engine/progress_writer_test.go`（单测，进闸门）+ 探针的「协议卫生」判据（线上帧级）——两条都做了「破坏→必红→还原→必绿」的负向验证。
>
> **本轮修掉的代码语义错位**（详见 `REQUIREMENTS.md` 2026-09-21 回归锁条目）：`lockFlowForRun` 的 `APP_NOT_FOUND`→`NOT_FOUND`；`gear_delete` 的哨兵分流（不再把「不存在」报成 `IN_USE`）；`gear_run(kind=tool)` 的预检（不再把「不存在」报成 `TOOL_RUN_FAILED`）；`app_id` 同秒重启的 `-2`/`-3` 后缀避让。

> 目标：让**不懂 gear 的通用智能体**（以 WorkBuddy 为首个客户端）通过 MCP 一次性、可验证地**创建并运行**一个 gear 应用。
> 本文只设计 MCP 面的**对外契约**与启动器归属，不改内核语义。红线不变：MCP 仍是「协议翻译层，零重复守卫」。
>
> 现状基线：`internal/mcp/mcp.go`（19 个静态工具 + `flow__{id}` 反向暴露）、`internal/cli/mcp.go`、go-sdk v1.8.0、MCP 2025-06-18。

---

## 0. 结论先行

**当前 MCP 面是「内核 API 的镜像」，不是「面向智能体的交付接口」。** 19 个工具是内核视角的完备映射，但对智能体而言：粒度太细、名字太泛、没有引导，且**不能把应用真正跑起来**。

### 0.1 五处改动（对应五类真问题）

| 问题 | 改动 | 落点 |
|---|---|---|
| 智能体不懂 gear 的设计思路 | **L0 注入层**：`ServerOptions.Instructions` 常驻心智模型（≤900 token）+ **L1** `gear_guide` 工具与 resources 按需展开（同源） | §4 §5 |
| 工具太多、说明太长、选不准 | **L2 工具层**：工具面收敛为**意图级**工具，统一 `gear_` 前缀，列表返回摘要而非全量 | §6 |
| 无法「先校验后落库」 | **L2**：`gear_validate`（收 YAML、字段级诊断、不落库）+ `gear_apply`（声明式原子落库） | §6 §7 |
| **智能体建不出「跑着的」应用** | **L2/C′**：`gear_app(action=start)` **派生后台进程**（detach）并立即返回 + 台账记账；`stop/status/list` 可管可查 | §6.2⑤ |
| **缺能力时只能瞎凑** | **L5 上报契约**：缺原语不许绕过，必须按「通用、最小、无业务名词」判据给出可落地的原语建议 | §9.6 附录 G |

### 0.2 MCP 的边界：**能启动，但从不持有**（两次修正后的最终口径）

这是全文最需要说清的一条，也是我两次判断的落点：

| 版本 | 主张 | 判定 |
|---|---|---|
| v1 | MCP 自持：在 **MCP 进程内**跑 Supervisor，写 pidfile 记账 | ❌ **错**。MCP 随客户端会话生死（stdio 下客户端断开即退出）→ 应用活不过会话；且这是「先制造孤儿、再用 pidfile 打补丁」 |
| v1.1 | 撤销自持 → 推出「MCP 不启动应用」，把运行完全交给 `gear app run` | ❌ **同样错**。把「不持有」错推成「不启动」，结果是智能体永远差最后一步，「通过 MCP 创建应用」不成立 |
| **v1.2** | **MCP 可以启动，但从不持有**：派生**独立后台进程**后立即返回，不保留任何句柄 | ✅ **正解**（东东拍板） |

**为什么 v1.2 不违反 PRD §14.1 / §14.4（这两条是我 v1 撞墙的地方，这里逐条对齐）：**

1. **PRD §14.1「一个 `app run` = 一个进程」**：v1.2 派生的子进程**就是一个 `gear app run`**。它是自己的持有者，负责整个 Flow 生命周期。**归属唯一，且正是 PRD 定义的那个进程**。
2. **PRD §14.4「子进程归属」的原意**是两条：**① 同一个长驻进程不能有两个归属者；② 不能留下无人知晓、无人能回收的进程树**。v1.2 满足两条——归属唯一（`app run` 自持），且**三处可见**（`gear_app status` / `gear app status`（CLI）/ `gear_doctor`），`stop` 可显式回收。
3. **§14.4 说的「一律通过内核执行入口」管的是「运行期 Flow→Flow 触发」**（那时唯一在跑的是内核，所以必须走内核入口，避免两条进程树并行）。而「第 0 号动作：把一个应用从零拉起来」**只能由外部发起**——CLI 的 `app run`、Web UI 的 `/api/apps`、MCP 的 `gear_app start` 是**三个等价的启动器入口**，差别只在谁持有：
   - `gear app run`（前台）→ 由**人/CI** 持有；
   - `POST /api/apps` → 由 **`gear serve`** 持有（serve 一死应用全死）；
   - `gear_app start`（MCP）→ 由**被派生的 `app run` 自己**持有（不挂 MCP、也不挂 serve，**意在**跨会话存活）。
     **该能力是有条件的（2026-09-21 实测修正）**：Windows 上派生时会尝试 `CREATE_BREAKAWAY_FROM_JOB`；宿主若不允许脱离，进程仍留在调用方的 Job Object 内，
     会在发起启动的那次会话/命令结束时被一并回收。这种情况 `start` 会同时给出 `escaped_job:false` 与 warning——**结论必须如实上报，不得简化成「已在后台常驻」**。
4. **与 v1 的 pidfile 的本质区别**（不是同一方案换个说法）：
   - v1：MCP **自己**持有监督循环 + pidfile 补记一个「MCP 关不掉、别人也找不到」的进程；
   - v1.2：进程**本来就独立自持**，台账（`~/.gear/apps/<app_id>.json`）**只是为了能找到它去管它**——没有台账，`status/stop` 无从下手。
   台账是**发现机制**，不是**孤儿补救**。

**顺带一个诚实结论**：v1.2 的形态比 REST 的 `/api/apps` **更健壮**——后者的应用寿命被 `gear serve` 绑架。所以 MCP 侧不采用「转发 REST」作为默认（§6.2⑤ 给了 `--apps` 作为已存在 serve 时的显式选项）。

---

## 1. 现状盘点：19 个工具对智能体的 7 个问题

| # | 问题 | 证据 | 后果 |
|---|---|---|---|
| P1 | **名字太泛，跨 server 冲突** | `list_tools` / `get_tool` / `get_execution` / `capabilities` | WorkBuddy 同时挂 agent-mail、sheetagent、utools 等多个 MCP server。`list_tools` 这种名字会被误选，或让模型误以为是 MCP 元工具 |
| P2 | **粒度太细** | 6 个读工具、3 个 delete 工具、2 个 run 工具 | 一次「看看现状」要 3–4 次调用；工具列表越长，选择准确率越低 |
| P3 | **列表返回全量定义** | `list_tools` 返回每个 Tool 的完整 `input_schema`/`output_schema`/`versions`（`mcp.go:197-208`） | 一个成熟 registry 的 `list_tools` 一次可吃掉数万 token，且 95% 信息当场无用 |
| P4 | **无「先校验后落库」** | `validate_flow` 只收 id（`mcp.go:350`）；`create_*` 失败即整段拒绝 | 只能试错式写入；错误信息无字段级定位 |
| P5 | **应用既不可启动、也不可见** | `run_flow` 显式拒绝 service Flow（`mcp.go:116-118`）、反向暴露也跳过（`mcp.go:503-507`）、无任何 app 工具 | 智能体能写出一份正确的 Flow，却**永远无法让它跑起来**——这是「通过 MCP 创建应用」的**唯一硬阻塞**（v1.1 曾误判为「不需补」，见 §0.2） |
| P6 | **无引导层** | `mcp.NewServer(..., nil)`（`mcp.go:89`、`:143`），无 instructions/resources/prompts | 智能体只能从 19 段 description 里反推架构；已在 go-sdk v1.8.0 核实 `ServerOptions.Instructions`、`Server.AddResource`、`AddResourceTemplate`、`AddPrompt` **均可用**，是「没用」而非「不能用」 |
| P7 | **无环境自检** | 无 doctor 类工具 | `ext.serve` 起不来时（sidecar 没 build、端口被占、`COORD_ADDR` 不符），智能体只会反复重写 YAML，定位不到环境原因 |

补充两个与体验直接相关的现状：

- **`flow__{id}` 反向暴露是负资产**：启动时快照、新建 Flow 当次不可用（须重启 MCP Server）、与 `run_flow` 语义重复、且 service Flow 恰好又不暴露。它让工具列表**数量不可预测**，直接损害工具选择准确率。
- **错误不可自愈**：错误以文本进 `content` + `IsError=true`（`mcp.go` 各 handler），没有机器可读的 `code`/`field`/`hint`，智能体无法据此精确修正 YAML。

---

## 2. 设计目标与原则

**目标（可度量）**：一次通过率↑、工具调用次数↓、上下文 token↓、人工介入次数↓（度量见 §12）。

**七条原则**

1. **意图对齐**：工具按「智能体想干什么」切分（了解 / 查有什么 / 看定义 / 校验 / 落地 / 运行 / 观察 / 排障），而不是按「内核有哪些资源」切分。
2. **渐进披露**：上下文分三层——常驻（KB 级）/ 按需（工具拉取）/ 运行时（工具返回）。**动态的东西绝不写进静态提示词**。
3. **静态优先**：能离线判定的规则，写进 guide + `gear_validate`，让智能体**在写 YAML 前就知道约束**，而不是运行期撞墙。
4. **单一真相源**：instructions、`gear_guide`、resources 三处文本必须同源（一个 `go:embed`），否则必然漂移——这正是 gear 自己的红线。
5. **不破红线**：MCP 面新增的每个工具都必须落到现有的 registry/engine/app 函数上，不新写守卫。
6. **不持有进程，但可以启动进程**：MCP **不得**在自己的进程内托管长驻执行（§0.2 v1 的教训），但**必须**能派生一个独立的受管进程并立即返回（否则智能体差最后一步）。判据很简单——**长驻子进程不得出现在 MCP 进程树内**。
7. **缺能力不许凑**：能力不足时必须上报原语缺口，且建议必须满足「基础、通用、无业务语义」六条判据（§9.6）。**宁可声明失败，不可静默降级**——这条直接来自 gear 自己的红线。

---

## 3. 方案总览

```
┌─ L0 注入层 ── ServerOptions.Instructions ──────────────── 常驻 ~900 token
│    心智模型四层 + 八条铁律 + 标准工作流 + 工具选择表
├─ L1 上下文层 ─ gear_guide(topic) / gear://guide/* / gear://schema/* ── 按需
│    同一份 go:embed 文本，三处引用（instructions 摘要 / 工具全文 / resources）
├─ L2 工具层 ── 意图级工具（gear_ 前缀，共 15 个）─────────── 调用面
│    guide · catalog · describe · capabilities · validate · apply
│    delete · promote · invoke · run · execution · app · doctor
├─ L3 交付物层 ─ Gear App Bundle（多文档 YAML，kind: App + Tool/Flow/Hook）─ 交付形态
│    静态资源由智能体自身的文件工具落盘；bundle 只声明不搬运
├─ L4 验证层 ── validate(dry) → apply → app start(派生后台进程) → 探活/日志 → 证据汇报 ─ 闭环
└─ L5 上报层 ── 原语缺口报告（缺能力时的唯一合法出口，禁止绕过门禁）─────────── 附录 G
```

---

## 4. L0：注入层 —— 让智能体先建立正确心智模型

**载体**：`ServerOptions.Instructions`（`mcp.NewServer(&mcp.Implementation{...}, &mcp.ServerOptions{Instructions: gearInstructions})`）。这是 MCP 里唯一**所有客户端都保证可达**的常驻上下文（已核实 go-sdk v1.8.0 支持）。

**预算**：≤ 900 token / ≤ 1400 字符，超出部分一律下沉到 L1。理由：instructions 每轮都在上下文里，写成 README 会既贵又漂移。

### 4.1 背景信息**只写一次**——三档预算（v1.2 补：这是本项目最易被误解的一处）

**误解**：把那段背景写进**每个**工具的 `Description` 里，让模型无论点开哪个工具都能看到全貌。
**为什么不行**：`tools/list` 是全量上报的，每个 description 都会被客户端塞进上下文，且**无法按需省略**——工具数一涨就是线性膨胀；更糟的是同一段话有 N 份副本，改一处要改 N 处，**必然漂移**（违反 §2 原则 4）。

| 内容 | 放哪 | 份数 | 体量 |
|---|---|---|---|
| 心智模型 + 7 铁律 + 8 步工作流 + 工具选择表 | `ServerOptions.Instructions` | **1 份**（会话级，不随工具数增长） | ≤900 token |
| 全字段语义、错误码→修法、原语缺口模板 | `gear_guide(topic)` 的**返回值** | 按需 1 份，用完即弃 | 每页 0.5–2 KB |
| 「做什么 + 何时用」+ 歧义消解 | 每个工具的 `Description` | 12 份，各一行 | **单个 ≤240 字符** |
| 入参/返回结构 | `InputSchema`（机器生成） | 12 份 | 必需项，不算重复 |

**实测对照**（`internal/mcp/mcp.go` 现量，中文字符按 0.7 token 粗估）：

| 方案 | 上线体量 | 说明 |
|---|---|---|
| 现状（19 工具，正常描述） | 工具定义 4,797 字符 ≈ **3.4k token** | 描述合计 3,514 字符（`create_tool` 一个就 1,203 —— 内联了 YAML 示例） |
| **反例：每个描述塞 900 token 背景** | 19 × 900 + 3.4k ≈ **20.5k token** | **现状的 6 倍，目标的 6.8 倍**；且**每增一个工具 +900 token** |
| v1.2 目标（12 意图级工具） | 工具定义 ≈2.1k（描述 ≤1,800 字符 ≈1.3k + schema ≈0.8k）**+ instructions 0.9k ≈ 3.0k** | 与现状**基本持平（略降 12%）** |

> **所以别把这层的收益算成「省 token」**——我们一边把描述瘦下来，一边新增了 instructions，两笔抵消。真正的收益是另外两笔：
> ① **常驻账**：背景从 N 份降到 **1 份**，工具数增长时**不再线性膨胀**（反例每加一个工具 +900 token，分层方案 +0）；
> ② **调用账**（这才是大头）：P3 那条「`list_tools` 返回全量定义」一次可达**数万 token**，与本节同源——都是「把必须常驻/一次性的东西塞进了高频通道」。
> 换个说法：分层的价值是**让每一类信息只出现在它该出现的档位**，token 是结果，不是目的。

**唯一允许在每个描述里重复的东西**是**歧义消解**——一句话说清「什么时候用我而不是隔壁那个」（例：`gear_run` = 一次性编排；`gear_app` = 长驻应用，且对 service Flow 必拒）。这不是背景，是**选择器**：模型是逐工具比对着选的，这类判断**必须**出现在每个候选旁边。
**机械守卫**（把「不许膨胀」变成红线，见 §9.1）：`TestMCPToolDescriptionsStayLean` —— 单个 `Description` ≤240 字符、全部合计 ≤1,800 字符，超了即红。

**内容骨架**（五段，完整草稿见 **附录 A**）：

1. **是什么（四层模型）**：

   ```
   能力（原语）          →  执行单元            →  编排        →  切面
   http/call/db/…          Tool（可复用）          Flow          Hook
   time/template/crypto    ↳ 或 step 直调「能力.方法」          （生命周期插入/门禁）
   ext.*（扩展原子）
   ```

   - **原语/能力是底座**：宿主提供的能力，Tool 靠 `capabilities` 声明「我有权用哪些」，在 Lua/process 里经 `ctx.*` 调用。
   - **Tool 是执行单元**：能力 + 业务逻辑（Lua / process / go-host），注册进 registry 后可被复用。
   - **关键取舍**：**Flow 的 step 可以直接写「能力.方法」（如 `db.exec` / `http.Request` / `ext.ai.complete`），不必先建 Tool**——所以「可复用的才建 Tool，一次性的直接直调」。这条决定了智能体该不该产出一个 Tool。
   - **Flow 编排执行单元形成业务**：「应用」= 含 `mode: service` 长驻 step 的 Flow。
   - **Hook 是切面**：在 `before_flow`/`before_step`/`after_step`/`on_error`/`on_success`/`before_promote` 插入执行与门禁。

2. **八条铁律**（v1.2 补了第 3 条与第 7 条；v1.3 补了第 8 条——指向 `gear_guide("security")` 的破坏性清单与密钥规则）：
   1. 写 `step.call` 的「能力.方法」直调前先 `gear_capabilities`，不许猜方法名与参数；
      写 Tool 的 Lua `entry` 前还要看同一份输出里的 **`invoke_form` / `lua_call`**（2026-10-03 新增）——
      Lua 侧入参形态是混合的（位置参数 / 单具名 table / 裸函数），只给参数名会编出
      `ctx.db.query(sql, params)` 或 `ctx.call.Call(...)` 这类**运行期才炸**的调用；
      用 `kind` 缩小时注意它有**三个**值：`builtin`（受门禁原语，需在 Tool 里声明
      `capabilities`）/ `pure`（纯函数原子 time/template/crypto/id/validate，**免声明即可调**）/
      `ext`（扩展原子）——只查 `builtin` 会静默少一层能力；
   2. YAML 先 `gear_validate` 再 `gear_apply`，不要「先写进去再改」；
   3. **起长驻应用只能用 `gear_app(action=start)`**——它会派生后台进程并立即返回；`gear_run` 对 service Flow 一定会拒（不是 bug，是设计）；
   4. 凭据只走 `${secrets.NAME}`，绝不内联明文；扩展原子禁止依赖环境变量；
   5. 所有 id 必须以 `<app-id>__` 或 `<app-id>.` 开头；
   6. 失败先读 `gear_execution` / `gear_app` 的日志定位到具体 step 再改，**禁止未验证就宣称成功**；
   7. **缺能力不许凑**（不许 shell 出去绕过、不许自造 `ext.*`、不许读 env）——按 `gear_guide("primitive-gap")` 的模板在答复里给出原语建议；
   8. **动手前先读 `gear_guide("security")`**——破坏性清单（`gear_delete` / `gear_app(stop, prune)` / `gear_promote(deprecated)` / `gear_apply(overwrite)`）与密钥规则（MCP 不写密钥，由人在 CLI 播种）都在那里。

3. **标准工作流**：8 步（见 §10）。
4. **工具选择表**：一行一个工具，消除「该调哪个」的猜测。
5. **完成的三段证据**：validate / apply / 运行态（§9.5）。

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
| `overview` | 四层模型 + 关系图 + CLI/REST/MCP 关系 | 首次接触 |
| `authoring` | Tool / Flow / Hook 的**全字段**语义 + 静态校验规则表 + 正反例 | 写 YAML 前 |
| `service-app` | service 长驻模型、anchor 排序、侧车与端口、`gear_app` 生命周期（start/stop/status/list） | 做应用时 |
| `atoms` | 7 门禁原语 + 5 纯函数 + `ext.*` 的**用法约定**（参数 schema 走 `gear_capabilities`，避免静态文本过期） | 写 step.call 前 |
| `errors` | 错误码表 + 常见报错 → 修法（附录 D） | 报错后 |
| `primitive-gap` | **原语准入六条判据 + 四类形态归属 + 报告模板**（附录 G） | 发现能力不足时 |
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

## 6. L2：工具层 —— 19 → 15（v1.2 收敛为 12，v1.3 因评审 I-8 补 `gear_invoke` 至 13，v1.4 补 `gear_export`/`gear_import` 至 15）

### 6.1 迁移对照

| 现有 | 目标 | 变更性质 |
|---|---|---|
| `list_tools` / `list_flows` / `list_hooks` / `list_executions` | **`gear_catalog`**（`kind`: tool\|flow\|hook\|app\|execution\|all，返回**摘要**；**非法 kind 报 `BAD_ARGUMENT` 并列出合法值，不静默返回空列表**——2026-10-03 收口，「查错了」必须能说出口） | 合并 + 瘦身 |
| `get_tool` / `get_flow` / `get_execution` | **`gear_describe`**（`kind` + `id`，全量定义） | 合并 |
| `create_tool` / `create_flow` / `create_hook` | **`gear_apply`**（bundle 多文档、原子、幂等、可选 `dry_run`） | 合并 + 升级 |
| `validate_flow` | **`gear_validate`**（接收 **YAML 文本**，不落库，返回结构化诊断） | 升级 |
| `delete_tool` / `delete_flow` / `delete_hook` | **`gear_delete`**（`kind` + `id`，守卫语义不变） | 合并 |
| `promote_tool` | **`gear_promote`** | 重命名 |
| `run_tool` / `run_flow` | **`gear_run`**（`kind`: tool\|flow，**默认异步**，`wait_seconds` 可选短等） | 合并 + 改造 |
| `get_execution_logs`（+ get/list） | **`gear_execution`**（`id`，`with_logs`/`tail`） | 合并 |
| `capabilities` | **`gear_capabilities`** | 重命名 |
| `flow__{id}` | **默认关闭**，`--expose-flows` 显式开启 | 降级为可选 |
| — | **`gear_guide`** | 新增（L1） |
| — | **`gear_app`**（`action`: start\|stop\|status\|list）**默认开启**（v1.2 由「可选」升为「必需」，见 §6.2⑤） | 新增（C′） |
| — | **`gear_doctor`** | 新增（问题 P7） |
| — | **`gear_invoke`**（`name`/`method`/`args`，能力直调；禁 `secret.*`） | 新增（v1.3，评审 I-8：等价 REST 的 `POST /api/capabilities/invoke`） |
| — | **`gear_export`**（`app_id`/`out_dir`/`format`/`include_static`/`static_root`，导出可移植产物；`app_id` 可为入口 Flow id，无 App 清单时临时合成清单） | 新增（v1.4，产物可交付性，见下） |
| — | **`gear_import`**（`source`/`overwrite`/`dry_run`/`apps_dir`，导回产物） | 新增（v1.4，产物可交付性，见下） |

**15 个 = 1 guide + 6 目录/写入/直调 + 2 执行/观察 + 1 app + 1 doctor + 1 promote + 1 invoke + 2 产物。**

> **工具数硬预算 15**（v1.2 为 12 → v1.3 补 `gear_invoke` 至 13 → v1.4 补 `gear_export`/`gear_import` 至 15）：
> 新增必须合并或先删一个。原语缺口上报（§9.6）**刻意不新增工具**，走输出契约，就是为了守住这个预算。
>
> **v1.4 这次放宽两位，理由与 I-8 同型**：它们不是「又一个操作」，而是把 L3 的必经路径接上——
> 在此之前「用户构建的产品」在 gear 里**没有可搬运的形态**（定义散在 registry、静态资源游离在
> 文件系统、App 清单此前根本不入库），换台机器只能整拷 `GEAR_HOME`（克隆机器，不是交付）。
> 没有它们，「在 gear 上构建」会退化成「在这台机器上配置」。详见 `APP_ARTIFACT_DESIGN.md`。

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
**service Flow 一定被拒**：返回 `SERVICE_FLOW_SYNC_RUN` + hint「用 `gear_app(action=start)`」。

**⑤ `gear_app`：应用生命周期（v1.2 重写 —— 派生后台进程 + 台账）**

**核心机制一句话**：`start` 把 `gear app run` **派生为一个独立后台进程**（脱离 MCP 进程树），**立即返回**；进程由它自己持有（PRD §14.1），MCP 只留一份**台账**用于发现与管理（§0.2）。

```
gear_app(action: start|stop|status|list,
         flow_id?, app_id?, restart?="always", replace?=false, wait_seconds?=5)
```

| action | 行为 | 返回 |
|---|---|---|
| `start` | 幂等检查 → 派生 detach 进程 → 可选短等 → 返回 | `{app_id, flow_id, pid, state: starting\|running\|startup_failed, log_path, ports:[{port,listening}], ports_source: explicit\|bundle\|none}`；入参 ports 缺省时回退 App 清单 `start.ports`（REQ-MCP-PORTS-ONE-TRUTH） |
| `status` | 台账 + 存活判定 + 端口探活 | `{app_id, flow_id, pid, state: running\|exited\|failed\|unknown, started_at, uptime_s, ports, log_path, last_logs?}` |
| `stop` | 发终止信号 → **端口复检** | `{app_id, state: stopped, ports_released: true}`；收不干净则 `ORPHAN_APP` + pid，**不谎报成功** |
| `list` | 全部台账（含已退出的历史记录） | `{items:[…], orphan: [app_id…]}` |

**实现要点（全部标准库，零新依赖 —— 已核实）**

1. **派生**：`exec.Command(self, "--registry", <abs registry path>, "app", "run", flowID, "--restart", policy)`
   - `--registry` **必须显式传**：派生出去的 `app run` **不知道 MCP 服务端启动时用了什么值**。不传它就按自己的默认值（`gearhome.RegistryPath()`）去找库，于是可能启动**另一个库里的同名 Flow**。
     （两边的默认值现已统一为同一个函数，但这只在「双方都没显式指定」时成立——所以「必须显式传」这条不变，变的只是它不再依赖某个写死的路径字符串。）
   - **应用内的步骤继承这件事**（2026-09-22 补）：`openRegistry()` 会把本次生效的 registry
     **绝对化**后写进 `GEAR_REGISTRY`；`cmdline` 的子进程环境以 `os.Environ()` 为基座，
     于是**应用自己拉起的进程**（长驻 Tool，如把 REST 面当后端起的 `gear serve`）也读同一个库。
     这才让「应用自带的 REST 面」与「人的 CLI」必然同源——此前没有这条信道，应用只能在 entry 里
     写死 `--registry`，而写死的相对值按子进程自己的 cwd 解析，会**静默**指到另一个库
     （症状：`/api/flows` 返回 `[]` 而 `gear flow list` 有 10 条，日志一个字都没有）。
     显式 flag 仍然优先——机制只负责让「没说的时候」永远一致。
   - 日志重定向：父进程 `os.OpenFile(<apps_dir>/<app_id>.log)` → 赋给 `cmd.Stdout`/`cmd.Stderr`。**`gear app run` 无需任何改动**。
   - 脱离进程树（构建标签，`internal/app/launch_{windows,unix}.go`）：
     - Unix：`SysProcAttr{Setsid: true}`（自成会话/进程组，父进程退出不受影响）
     - Windows：`SysProcAttr{CreationFlags: DETACHED_PROCESS(0x8)|CREATE_NEW_PROCESS_GROUP(0x200), HideWindow: true}`；若客户端用 Job Object 收整个进程树，再加 `CREATE_BREAKAWAY_FROM_JOB(0x01000000)`，**失败则退化为不带并记 warning**（不静默）
   - **不调用 `cmd.Wait()`**：父进程立即返回并 `Release()`。
2. **存活判定**（跨平台，双判据）：
   - pid 存活：Windows 走 `os.FindProcess`→`OpenProcess`，进程不存在即返回错误（已核实 Go 1.27 `src/os/exec_windows.go:84-92`）；Unix `p.Signal(syscall.Signal(0))`（`src/os/exec.go:250-253`）
   - 端口探活：对台账里声明的端口做 TCP 连接
   - **两者一致才给 `running`/`exited`；不一致（如 pid 在但端口没起、端口有但 pid 不符）一律 `unknown`，且不自动杀、不谎报**——这是 pid 复用与端口抢占的 fail-closed 兜底。
3. **台账**：`<dir(registry)>/apps/<app_id>.json`（默认 `~/.gear/apps/`，可用 `--apps-dir` 覆盖）

   ```json
   { "app_id": "app_mynews-20260921T154203", "flow_id": "mynews.up",
     "registry": "/home/u/.gear/registry.db", "pid": 48213, "restart": "always",
     "started_at": "2026-09-21T15:42:03+08:00", "cwd": "/srv/mynews",
     "log_path": "/home/u/.gear/apps/app_mynews-20260921T154203.log",
     "ports": [8080] }
   ```
   - `app_id` 生成规则：`app_<flow_id 规范化>-<UTC 时间戳>`（可读、可排序、不与 pid 绑定）。
4. **幂等**：`start` 前先按 `flow_id` 查活跃台账 → 命中则直接返回 `state: already_running`（**不重复拉起**，避免端口冲突）；要换新实例需显式 `replace: true`（先 stop 再 start）。
5. **遗留/孤儿**：`list` 与 `gear_doctor` 会把「台账有记录但进程已死」「台账无记录但声明端口被占」列为 `orphan` 并**汇报**（含 pid 与日志路径），**不静默删除、不静默接管**。这条正是对 §14.4「避免无人知晓的进程树」的兑现方式。
6. **CLI 对等**：新增 `gear app start|stop|status|list` 子命令，与 `gear_app` **共用同一份内部实现**（`internal/app` 的 `Start/Stop/Status/List`），保证「MCP 只是协议翻译层、零重复守卫」。`gear app run`（前台阻塞式，`cli/app.go:21-65`）保持不变，`start` 就是它的后台启动器 + 台账。
7. **`--apps <rest-addr>`（可选，非默认）**：若你已有常驻 `gear serve` 并希望应用归它持有，可用此开关让 `gear_app` 改为**纯转发** `/api/apps`（进程归 serve）。**二选一、启动时确定、不做运行期双路回退**——避免同一个 Flow 出现两个归属者（§14.4 的红线）。此时 REST 不可达返回 `APP_PLATFORM_UNAVAILABLE` + hint，**不静默降级为本地派生**。

> **跨会话存活的正确表述**（v1 那条用例的前提**这次大部分成立，但不是无条件的**）：
> 进程是**独立的 `app run`**，WorkBuddy 关闭 → MCP Server 退出 → **只要它成功脱离了调用方的进程沙箱**，应用就继续运行；
> 下次会话 `gear_app(action=status)` 仍能看到它（台账在磁盘上）。这也意味着**用户必须能显式停掉它**——所以 `gear_app(stop)` 与 `gear app stop` 是必需品，不是可选项。
>
> **2026-09-21 实测修正（§11 第二十二次）**：WorkBuddy 这类宿主自身跑在一个 **Job Object** 里，Windows 默认情况下**不允许子进程 breakaway**。此时 `start` 派生出的进程仍在 Job 内，
> WorkBuddy 会话一结束就被连同整个 Job 回收——上面的「跨会话存活」在该环境下**不成立**。这不是设计退化而是宿主约束，无从在其内部绕过；
> 正确的应对是**把这件事暴露出来而不是粉饰**：`StartResult.EscapedJob` / `StatusResult.EscapedJob` 与 stderr warning 同步给出（`escaped_job:false`），
> 由调用方据此如实回报用户。 `"跨会话存活" 是目的、不是承诺`——这正是本设计里反复强调的「不谎报」原则在进程生命周期上的一次兑现。

**⑥ `gear_doctor`：环境自检（问题 P7）**

```
{ registry: {path, tools, flows, hooks},
  extensions: {config, loaded: ["ext.serve", ...], missing_sidecars: ["build/serve-sidecar.exe"]},
  ports: {8095: "free|in-use(pid)"},
  apps: [{app_id, state, pid, ports_released}],
  orphan: [{app_id, reason: "pid-gone|record-missing", pid?}],
  warnings: ["检测到 extensions.yaml 声明 ext.kafka 但 build/kafka-sidecar.exe 不存在 → 该能力不可用"] }
```

智能体排障的第一站；`ext.serve` 起不来时，一次调用就能区分「YAML 写错」与「环境没准备好」。

> **2026-10-03：判据本体已抽到 `internal/doctor`，CLI 侧 `gear doctor` 用的是同一份**（`Report.Map()` 保持上面这个形状逐字不变，`Report.Text()` 是 CLI 的渲染）。此前 doctor 只有 MCP 侧，于是「从命令行排查」的人看不到它，而能在 CLI 做的事（看路径、重建、改配置）偏偏都在 CLI 侧——一处真实的不对称。两份实现必然漂移，而 doctor 一旦漂移就再也不能被信任。

**⑦ `gear_` 前缀与破坏性变更（已拍板：一次性全量统一）**
MCP 无别名机制，一次性重命名。**Server `Version` 从 `0.1.0` 升到 `0.2.0`**，README §12 与 `REQUIREMENTS.md` 同步。
代价说明（为什么「全量」反而更便宜）：19 → 12 是**重构而非改名**，被合并的工具本来就换新名；剩下**功能存活但换名**的也只有 `capabilities`、`delete_*`、`promote_tool`、`execution` 系列。真正的成本只有三处：
1. ~37 处测试调用点（机械替换，`internal/mcp/*_test.go`、`internal/cli/cli_mcp_test.go`）；
2. `TestMCPAllToolsReturnObjectStructuredContent` 的覆盖度反查启发式（现为 `strings.HasPrefix(tt.Name, "list_")`）**必须重写为「registry 驱动」**：断言「每个注册的工具都在登记表里」——改完这条红线比原来**更强**（不再依赖命名约定）；
3. README §12 工具表 + REQUIREMENTS 2 条。
工具增加 `Annotations`（`ReadOnlyHint`/`DestructiveHint`/`IdempotentHint`）：`gear_validate`/`gear_catalog`/`gear_describe`/`gear_capabilities`/`gear_doctor`/`gear_execution` = 只读；`gear_delete`/**`gear_app`**（含 stop/prune，故整体标破坏性，见 §6.2 与评审 I-4）/`gear_invoke` = 破坏性；`gear_apply` = 幂等。这让客户端能在 UI 上正确提示风险。

**⑧ 协议红线照旧（不许忘）**
新增列表类工具（`gear_catalog`）**必须**经 `objectResult` 包具名键，并登记进 `TestMCPAllToolsReturnObjectStructuredContent` 的 calls 表——该用例会反查覆盖度，漏登记直接 FAIL（README §12.2）。

**⑨ 结构化错误（附录 D）覆盖全部工具，且错误必须可自愈**
每个错误至少给 `code` + `hint`；能定位到字段的给 `field`，能定位到 step 的给 `step`。目标是 §10 的自愈回路成立：**智能体靠 `code` → `gear_guide("errors")` → 改 YAML 收敛，不靠问人**。

---

## 7. L3：交付物层 —— Gear App Bundle

「创建应用」需要一个**明确的交付形态**。定义 Gear App Bundle：一个多文档 YAML，第一份是 `kind: App` 清单，其余是标准 Tool/Flow/Hook 文档。

> ⚠ **本节决策已被推翻（2026-09-26，见 `APP_ARTIFACT_DESIGN.md` §2）**：App 实体已入 registry
> （`apps` 表，`model.App`），`gear_apply` / `gear import` 会写这一行——它是命名空间户主
> （`App.id` 是 tool/flow id 的前缀来源），`--scope` 守卫、列表分组、`app start` 端口回退、
> `gear export` 四处都读它。当时「不引入 App 实体」的取舍在 gear 自用时成立，但
> 「没有 App 实体」等价于「没有产品」——别人无法在 gear 上交付应用。以下保留为设计史。
>
> **（原决策，2026-09-26 前有效）不引入 App 实体**：bundle 只是**多文档 YAML 的组织约定 + 依赖声明**，落到 registry 后仍然只有 Tool/Flow/Hook。这保持了 PRD 的非目标（「只有 Flow + `app run`」，`PRD_v40.md:60`）与 `GEAR_工程方法_v1.md` 的单一真相源原则。

```yaml
kind: App                       # ← 唯一新增的文档类型（当时仅 apply/validate 识别；2026-09-26 起落 registry apps 表）
id: mynews
name: 我的新闻站
description: 抓热搜 + 静态站托管
entry_flow: mynews.up           # 应用入口（必填，须为本 bundle 或已注册的 Flow）
layout:
  workdir: apps/mynews          # 约定工作目录（智能体自身文件工具落盘到此）
  files:                        # 需要落盘的文件（apply 只校验存在性，不搬运）
    - {path: apps/mynews/www/index.html, required: true}
start:                          # 交给 gear_app(start) 的启动参数（v1.2 新增）
  restart: always               # always | never
  ports: [8080]                 # 声明端口 → 用于台账探活与 stop 后复检
requires:
  secrets: [NEWS_API_KEY]       # 需要人播种的密钥名（apply 只检查是否已存在）
  sidecars: [serve]             # 依赖的侧车 → 校验 build/serve-sidecar.exe
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

**它同时回答了「创建应用需要哪些上下文」**：id 命名空间、文件清单、密钥清单、侧车与端口、能力声明、启动参数——一份清单式契约，人机都读得懂。

**边界（写清楚，避免智能体期待错误）**
- **MCP 不提供文件读写**。静态资源由智能体**自身的文件工具**落盘；bundle 只声明 `layout.files`，`gear_apply` 校验其存在性（缺了就在 `warnings` 里报「文件未就绪」）。理由：MCP 不该重复客户端已有的文件能力，且能减少一大类权限面。
- **命名空间**：`gear_apply` 强制 bundle 内所有 id 以 `<app id>__` 或 `<app id>.` 开头（不符即拒），避免智能体覆盖你手工维护的 Tool/Flow。这是防「一次性生成污染 registry」的关键约束。
- **幂等**：apply 是 upsert；响应里区分 `created` / `updated`，让智能体知道自己改动了什么。
- **`start` 只是声明**：`gear_apply` 不启动应用（写入 ≠ 运行）；启动是 `gear_app(action=start)` 的独立动作。这条分离让「改配置」与「重启应用」互不牵连。

---

## 8. 需要向 WorkBuddy 提供的上下文信息（清单）

| 层 | 内容 | 体量 | 来源（单一真相源） | 加载时机 |
|---|---|---|---|---|
| **常驻** | 四层模型、7 铁律、8 步工作流、工具选择表、三段证据 | ≤900 token | `guide/summary.md`（embed） | instructions，会话建立时 |
| **按需** | 全字段语义、静态校验规则、service 模型与 `gear_app` 生命周期、错误码表、**原语准入判据与缺口模板**、3 个骨架示例、安全约定 | 每个 topic 0.5–2 KB | `guide/*.md`（同一 embed） | `gear_guide(topic)` |
| **运行时** | 能力目录（含参数 schema）、对象摘要/全量、校验诊断、执行记录与日志、**应用台账与探活**、环境自检 | 按需，可能较大 | `gear_capabilities` / `catalog` / `describe` / `validate` / `execution` / `app` / `doctor` | 动作前 |
| **仓库内**（由智能体自己的文件工具读） | `examples/*.yaml`、`plugin/static_site_demo.yaml`、`plugin/web_ui_flow.yaml`、`README.md` §12 | 大，按需 grep | 磁盘 | 需要范例或排查文档漂移时 |

**明确不要放进上下文的东西**：

| 不放 | 原因 | 替代 |
|---|---|---|
| `capabilities` 全文 | 随 `extensions.yaml` 变化，静态写必然过期；体量大 | `gear_capabilities` 工具实时取 |
| `extensions.yaml` 全文 | 含环境相关配置，且能力清单已有权威出口 | `gear_doctor` + `gear_capabilities` |
| 完整 README / PRD | 数万 token，与任务无直接关系 | `gear_guide` topic + 需要时读文件 |
| 全部 Tool/Flow 定义 | token 爆炸 | `gear_catalog` 摘要 → `gear_describe` 单选 |
| 应用台账全量 | 随运行期变化 | `gear_app(action=list/status)` 实时取 |

---

## 9. 约束条件

### 9.1 协议硬约束（MCP 面）

1. `structuredContent` **顶层必须是 JSON 对象**；列表类经 `objectResult` 包具名键；新工具必须登记进覆盖度反查用例。
2. 执行失败**返回记录（`status: failed`）而非协议错误**；只有「对象不存在 / 参数非法 / 前置条件不满足」才 `IsError=true`。两类形态必须一致，否则智能体判断逻辑会分裂。
3. stdio 下 stdout 只承载协议帧，日志走 stderr。
4. 工具粒度约束：**一个工具最多一件事**；描述首行必须是「做什么 + 何时用」，示例不超过 8 行（避免把 description 写成文档）。
   **体量红线（机械守卫）**：单个 `Description` ≤240 字符、全部工具 `Description` 合计 ≤1,800 字符 —— `TestMCPToolDescriptionsStayLean` 断言，超出即 FAIL。背景信息一律不下沉到描述里（见 §4.1）。
5. **长驻子进程不得出现在 MCP 进程树内**（原则 6 的机械判据）。任何工具都不得阻塞等待长驻目标结束。

### 9.2 领域硬约束（智能体必须遵守，`gear_validate` 会逐条拦）

**先分清「能力」的四类——这是智能体最容易搞错的地方：**

| 类别 | 成员 | 是否受 capability 门禁 | 能否被 Flow step 直调 | 说明 |
|---|---|---|---|---|
| 受门禁原语 | `http` `call` `db` `secret` `event` `subscribe` `config`（7 个） | **是**（Tool 必须声明） | 能（如 `http.Request`） | 单一真相源 `internal/cap/cap.go` 的 `KnownPrimitives` |
| 纯函数原子 | `time` `template` `crypto` `id` `validate`（5 个） | **否**（始终可用，声明了反而被拒） | 能（如 `crypto.hash`、`id.uuid`、`validate.json_schema`） | 单具名 table 入参，`internal/engine/purefunc.go` |
| Lua 便利绑定 | `json`（`ctx.json.encode/decode`） | — | **不能** | **不是能力**，不在能力目录；`ATOM_ROADMAP.md:21-27` 刻意如此 |
| 可扩展原子 | `ext.email` `ext.serve` `ext.kafka` … | **是**（`ext.` 前缀 + 必须在 `extensions.yaml` 注册） | 能（如 `ext.ai.complete`） | 含 once 与 `service: true` 两种载体 |

> 常见误解（写进 guide）：把 `crypto` 当受门禁原语去声明（会被拒）、把 `json` 当能力去直调（不存在）、以为 `ext.*` 可以不声明就用（门禁拒绝）。

**约束清单：**

| 约束 | 具体规则 | 现有守卫 |
|---|---|---|
| runtime 白名单 | `process` / `lua` / `go-host`；`wasm` 未实现，注册即拒 | `model.ImplementedRuntimes` |
| capability 门禁 | 用 `ext.*` 必须在 Tool 里声明；7 原语外的能力不可声明 | `cap.GrantsFromStrings` |
| call 目标必须存在 | Tool id 或「能力.方法」，否则拒 | `engine.ValidateFlowStatic` |
| `when` 是表达式不是模板 | 写 `steps.s1.output.n > 1`，**不能写 `${}`** | 静态编译期 |
| 模板前缀三选一 | `${vars.x}` / `${steps.id.output.x}` / `${secrets.NAME}` | 明文白名单校验 |
| 依赖必须无环 | `depends_on` + 隐式依赖合成图无环 | `topoLevels` |
| service step 必须 app 托管 | 含 `mode: service` 的 Flow 不能 `flow run` / `gear_run` | `REQ-FLOW-RUN-REJECTS-SERVICE` |
| 「能力.方法」直调的限制 | 不支持 `use_version` 与 `mode: service` | `resolveStepCall` |
| service step 不能作为依赖目标 | 需要 once 步做「锚点」把后续步推下一层 | `web_ui.up` 的 `anchor` |
| schema 必须是标准 JSON Schema | 顶层 `type: object`；不支持 `{"x":"string"}` 简写 | `Tool.Validate` |
| **缺能力不得绕过** | 不许用 process runtime shell 出去调外部程序、不许自造 `ext.*` 名、不许读 env 取凭据 | 门禁 + `TestExtAtomsMustNotReadEnv` + §9.6 输出契约 |

### 9.3 安全约束

- MCP 的 `--http` **只绑 127.0.0.1**，文档明确警告不要暴露公网（当前无鉴权）。
- **密钥不回显**：任何工具都不返回密钥明文；bundle 只声明 `requires.secrets` 的名字。
- **凭据只从调用参数传**：`ext.*` 原子禁止读环境变量（`TestExtAtomsMustNotReadEnv` 机械守卫）；智能体必须用 `${secrets.NAME}` 而非内联明文。
- **破坏性操作**（`gear_delete`、`gear_app(stop)`、`gear_promote → deprecated`）在 description 中标注，并要求智能体先列出影响面。
- **密钥不开放写入（已拍板）**：bundle 只声明名字，人在 CLI 播种。这是**刻意保留的人机交接点**——明文一旦经 MCP 传入，就会进对话记录、模型上下文与 SQLite 执行记录，而 `--http` 目前无鉴权（§13.1 决策 3）。

### 9.4 环境约束

- 侧车须先构建（`bash scripts/build.sh` → `build/*-sidecar.exe`）；`gear_doctor` 负责报告缺失。
- 端口必须唯一且未被占用；`listen` 是协调地址的唯一真相源（别另写 addr）。
- registry 默认落在 `gearhome.RegistryPath()` = `<GEAR_HOME>/registry.db`（`GEAR_HOME` 未设时 `<HOME>/.gear/registry.db`；单一真相源见 `internal/gearhome/gearhome.go`），MCP 与 CLI/REST **共享同一份**——智能体的改动会与人的改动**互相可见**，所以命名空间约束（§7）是必须的。
  （2026-09-21 前默认值写在 `internal/cli/root.go` 里且与另外几处各自维护；现在 registry / exec.db / doctor 产物查找三处默认值统一从 `gearhome` 取，**且一律不随 cwd 变化**。）
- **应用台账目录默认 `<registry 同目录>/apps/`**（即 `~/.gear/apps/`），`--apps-dir` 可覆盖；台账与日志均由 gear 自己管理，不是用户文件。
- **应用 YAML 不得写机器相关绝对路径**（如 `D:/gadern/gear/...`、`/Users/...`、`/d/gadern/...`）：`dir` 写**相对安装根**的路径（如 `apps/myapp/www`、`build/dist`），由 `InstallRoot` 锚定；`entry` 写裸名（如 `gear serve ...`）锚到可执行文件目录；`db` 的 dsn 写相对库名（如 `mindstorm.db`）锚到 `GEAR_APP_DATA`。`gear_validate` 对机器绝对路径直接报 `PORTABLE_PATH_HARDCODED`（fatal，会让 `valid` 变 `false`、`gear_apply` 据此拒绝落库），与 CI 闸门 `tools/portable_audit.py` 第⑨项**同口径**——CI 是兜底，MCP 是即时反馈（见下条）。
- **发布后智能体主要通过 MCP 接口编写应用**：`gear_validate`（编译器，不落库、字段级诊断）+ `gear_apply`（唯一写入口，声明式原子幂等）是智能体落地应用定义的标准路径，而非手写 YAML 文件。因此「可移植性 / 命名空间 / 结构合法性」等校验都发生在 MCP 写作期，作者能立刻拿到 `code`+`hint` 去改 YAML；任何想加的硬约束都应挂进 `validateBundle`，让智能体在写作期自愈，而不是等到发布后拷贝到别的机器才炸。
- **派生的 app 进程继承的是 MCP 服务端的 cwd**（`cmd.Dir` 显式设为其值）——这条要写进 guide，否则 `dir: build/dist` 在 WorkBuddy 里会解析到别处。

### 9.5 交付纪律（写进 instructions 的铁律）

智能体宣称"完成"必须附**三段证据**：
① `gear_validate` 的 `valid: true`；
② `gear_apply` 的 `applied` 清单（created/updated）；
③ **运行证据**——长驻应用：`gear_app(action=status)` 的 `state: running` + 端口 `listening: true` + 关键日志行；一次性编排：`gear_execution` 的 `status: succeeded` + 关键日志行。
**另附**：应用启动后必须告知用户**如何停止**（`gear_app(action=stop, app_id=…)` 或 `gear app stop <app_id>`）——因为它是独立后台进程，不会随会话结束。
禁止「看起来跑通了」。

### 9.6 原语缺口上报契约（v1.2 新增）

**触发条件**（出现任一，智能体必须上报而不是绕过）：
1. `gear_capabilities` 里没有能完成该动作的能力，且 `extensions.yaml` 也没有对应 ext；
2. 现有能力只能靠「拼凑」实现（例如用 `http` 轮询模拟队列、用 `process` shell 出去调外部工具）；
3. `gear_validate` 返回 `UNKNOWN_CALL_TARGET` / `UNKNOWN_CAPABILITY`，且确认不是笔误。

**四条禁止**（比建议更重要）：
1. **禁止用 `process` runtime 里 `sh -c` / `cmd /c` 调外部程序**绕过能力门禁——那是把门禁关掉；
2. **禁止自造 `ext.xxx` 名字**（未在 `extensions.yaml` 注册的能力，声明了也会被拒，且污染能力口径）；
3. **禁止读环境变量取凭据**（`TestExtAtomsMustNotReadEnv` 机械守卫）；
4. **禁止把业务逻辑伪装成原语建议**（如「建议加 `ext.shipOrder`」——那不是原语）。

**原语准入判据（六条全中才算原语，这是「基础通用、不参杂业务」的机械形式）**：

| # | 判据 | ✅ 合格 | ❌ 不合格 |
|---|---|---|---|
| 1 | **通用**：与具体业务无关，任何应用都可能用 | `ext.pdf.extract` | `ext.newsFetch` |
| 2 | **最小**：单一动作，不含编排与决策 | `http.request` | `fetchCleanAndStore` |
| 3 | **可参数化**：行为差异全部由入参表达，无内置业务默认值 | `smtp.send(to,subject,body)` | `smtp.sendDailyReport()` |
| 4 | **无业务名词**：能力名里不出现行业/产品/流程名词 | `sql.query` | `erp.syncOrders` |
| 5 | **归属清晰**：能判定落在下表哪一类 | — | 「说不清是原语还是 Tool」 |
| 6 | **可测**：能写出确定性夹具（本地 endpoint / 可注入 transport） | — | 依赖真实外部系统才能测 |

**归属决策表**（判据 5 的展开）：

| 形态 | 判据 | 落点 | 是否进门禁 |
|---|---|---|---|
| 纯函数原语 | **不碰需授权的资源**（无 IO、无凭据、不读密钥库/env）+ **不持跨调用状态**（`hash`/`base64`/`jwt_verify`/`uuid`；读时钟与熵算环境事实，不算资源，故 `time.now`/`crypto.random` 仍属此列） | `internal/engine/purefunc.go`，单具名 table 入参 | 否（声明了反而被拒） |
| 受门禁原语 | 有外发 IO 或副作用（网络/DB/事件/密钥） | `internal/cap` 的 `KnownPrimitives` | **是** |
| 扩展原子 `ext.*` | 需握手/配置/第三方 SDK/侧车，或需长驻 | `plugin/extensions.yaml` 注册，可选 `service: true` | **是** |
| **不申请原语** | 只在某一个应用里复用 | 直接建 Tool（Lua/process），或 step 直调已有能力 | —（这才是大多数情况的答案） |

**交付方式**：报告写在**给用户的答复里**（人能看到并决定是否加原语），**不新增 MCP 工具**——守住 §6.1 的工具硬预算（15）。模板见**附录 G**。

---

## 10. 标准交互时序（创建并运行应用 8 步）

```
① gear_doctor                      → 环境是否就绪（sidecar/端口/registry/遗留 app）
② gear_guide("service-app")        → 拿操作手册
   gear_capabilities               → 确认可用原子与参数（尤其 ext.*）
③ 【自身文件工具】落盘静态资源到 layout.workdir
④ gear_validate(bundle)            → valid:true 才继续（失败→按 hint 改→回到 ④，可无限次）
⑤ gear_apply(bundle)               → applied: {tools, flows}；冲突/缺文件在 warnings
⑥ gear_app(action=start, flow_id)  → 派生后台进程，返回 {app_id, pid, state, ports}（不阻塞）
⑦ gear_app(action=status) / 端口探活 / 关键日志行 → 确认真的在跑
⑧ 汇报：bundle 摘要 + 访问地址 + 三段证据 + 【如何停止】
```

**自愈回路**：任何一步失败 → 读 `diagnostics.code` → `gear_guide("errors")` 查修法 → 改 YAML → 回 ④。这条回路是「高效」的核心：智能体不需要问人，也不需要猜。
**缺能力分支**：④ 报 `UNKNOWN_CALL_TARGET` 且确认能力不存在 → 走 §9.6，产出原语缺口报告（附录 G），并在答复中明确「哪些步骤被阻塞 / 有无无损降级方案」。

---

## 11. 落地路线

### P0 —— 让「创建并运行应用」成立

| 项 | 改动点 | 验收 |
|---|---|---|
| `ServerOptions.Instructions` + `gear_guide`（embed 同源） | `internal/mcp/mcp.go`、新增 `internal/mcp/guide.go` + `guide/*.md` | instructions 非空且随 initialize 下发；`gear_guide` 对 8 个 topic 各返回非空文本（含 `primitive-gap`）；三处（instructions/工具/resources）渲染自同一 embed |
| `gear_validate`（收 YAML、字段级诊断、不落库） | `internal/engine/validate_static.go` 给错误包 code/step/field（**不改签名**，用 `errors.As` 取）；`internal/mcp` 新增工具 | 用 `plugin/web_ui_flow.yaml` 故意改坏 5 处 → 诊断各自带 `code`+`step`；注册表未被改动（断言 count 不变） |
| `gear_apply`（bundle 原子 + namespace 校验 + `dry_run`） | `internal/mcp/mcp.go`，复用 `registry.ParseYAMLDocs` / `Upsert*` / `ValidateFlowStatic` | 多文档混排（App+Tool+Flow+Hook）一次落库；第 3 份非法 → 前两份也不写入；id 不带前缀 → 拒 |
| **`gear_app` + CLI `app start\|stop\|status\|list`（派生后台进程 + 台账）** | 新增 `internal/app/launch.go`（detach，构建标签 `_windows.go`/`_unix.go`）、`internal/app/record.go`（台账读写 + 存活/端口判定）；`internal/cli/app.go` 加 4 个子命令；`internal/mcp/tools_app.go` 新增工具 | ✅ 已实现，验收映射到**真实用例名**（设计期提出的 `TestAppStartReturnsFast` 等在此不存在，勿再引用）：① 快速返回 + 不持有进程 → `app`: `TestStartSpawnsDetachedAndRecords`（真派生子进程、父进程不 wait）、`mcp`: `TestMCPAppLifecycleEndToEnd`；② 跨会话存活 → 由「父进程不 wait」+ detach 标志保证（in-memory 用例派生的子进程是测试二进制自身，无法真跑 `app run`，故此项由 CLI 实机路径覆盖：`cli`: `TestAppLifecycleCommands`）；③ stop 释放端口 → `app`: `TestStopMarksStoppedAndReleasesPorts`、`cli`: `TestAppLifecycleCommands`；④ 同 flow 二次 start 幂等且 pid 不变 → `app`: `TestStartIsIdempotentForRunningFlow`、`cli`: `TestAppStartIsIdempotentForLiveFlow`；⑤ 孤儿**只报告不删除** → `app`: `TestOrphansReportsWithoutJudging`、`TestStopReportsOrphanWhenPortHeld`、`cli`: `TestAppStopReportsOrphanAsError`；⑥ pid 与端口判据不一致 → `unknown`/孤儿而非 `running` → `app`: `TestProbeUsesDualCriterion` + `TestStopReportsOrphanWhenPortHeld`。另：同秒重启的台账覆盖风险由 `TestUniqueAppIDAppendsSuffix` 锁住 |

**同步交付**：`ServerOptions.Instructions`（附录 A）+ `gear_guide` 与 embed（附录 B 起手）+ 原语缺口 guide topic（附录 G）。

### P1 —— 让「高效」成立

- 工具重命名 + 合并（19 → 12）+ `Annotations`，`Version → 0.2.0`；**覆盖度反查启发式重写为 registry 驱动**；新增 **`TestMCPToolDescriptionsStayLean`**（单描述 ≤240 字符、合计 ≤1,800 字符，见 §4.1 / §9.1）。
- `gear_catalog` 摘要化（P3 的根治）；`gear_run` 异步化（`wait_seconds`）。
- `gear_doctor`（含台账/孤儿检查）。
- resources（`gear://guide/*`、`gear://schema/*`）。
- `flow__{id}` 默认关闭（保留 `--expose-flows`）。
- 结构化错误契约（附录 D）覆盖全部工具。
- 可选：`--apps <rest-addr>` 纯转发模式（供已有 `gear serve` 的场景）。

### P2 —— 让「可度量」成立

- 智能体 eval 场景集（§12）纳入 `plugin/verify.py` 或新增 `plugin/agent_eval.py`。（未做）
- ✅ **`plugin/mcp_wire_probe.py` 已恢复**（2026-09-21）：原始 JSON-RPC 抓帧落盘 + 严格客户端同款判据 + 判据自检；stdio/`--http` 双模式各 33 条判据全绿。**它自己也曾有一处 HTTP 判据空转（stdout 被 `DEVNULL` 掉），是被负向验证抓出来的**——见 §12 注。
- 可选：原语缺口报告的**持久化 sink**（如 `gear_guide` 之外的 `~/.gear/primitive-gaps.jsonl`）——**仅当**你希望把缺口积累成待办清单时才加；默认不加（守住 12 工具预算）。
- 可选：`gear_secrets(action: list)`（只回名字）。

### 文档同步点（改代码必同步，项目约定）

| 文件 | 同步内容 |
|---|---|
| `README.md` §12 | 工具表（19 → 12）、instructions 说明、`gear_app` 与后台进程口径、`--expose-flows`、bundle 示例 |
| `REQUIREMENTS.md` | 新增 `REQ-MCP-APP`（派生后台进程 + 台账 + 幂等）、`REQ-APP-LAUNCH`（detach 跨会话存活）、`REQ-MCP-APPLY` / `REQ-MCP-VALIDATE-YAML` / `REQ-MCP-INSTR` / `REQ-MCP-NAME` / `REQ-MCP-BUDGET`（描述体量红线：单 ≤240、合计 ≤1,800 字符）/ `REQ-MCP-ERR` / `REQ-MCP-DOCTOR` / `REQ-PRIMITIVE-GAP`（缺能力必须上报、禁止绕过），各带测试锚点 |
| `internal/cli/root.go` / `scaffold.go` | `app` 子命令列表、service Flow 必须 `app run/start` 的提示口径 |
| `internal/mcp/mcp_test.go` | 新工具全部登记进 `TestMCPAllToolsReturnObjectStructuredContent`（反查覆盖度，漏登记即红） |
| `MCP_AGENT_DESIGN.md`（本文） | 实现落地后回填状态 |
| `plugin/verify.py` | 端到端红线追加「bundle 校验→落库→`gear_app start`→端口可连→`stop` 后端口释放→父进程退出后仍存活」场景 |

---

## 12. 度量与验收

**7 个 eval 场景**（每个都从空 registry 起，记录工具调用次数与 token）

| # | 场景 | 通过判据 |
|---|---|---|
| E1 | 最小一次性编排（hello） | `run` 成功，无人工介入 |
| E2 | 静态站点应用（`ext.serve` + 静态目录） | 端口可访问，返回页面内容 |
| E3 | 常驻应用（service flow，两个长驻 step + anchor） | `gear_app(start)` → `status: running` → 端口可连 → `stop` 后端口释放；**且 MCP Server 退出后应用仍在跑**（v1.2 前提成立） |
| E4 | 带密钥的通知 Flow（`ext.email` + `${secrets}`） | `validate` 能指出未播种的密钥；人播种后跑通 |
| E5 | 断言失败自愈（故意给坏 YAML） | 智能体在 ≤3 次迭代内自行修好（靠 `code`+`hint`，不靠问人） |
| E6 | 幂等与命名空间 | 同 bundle 二次 apply → `updated` 而非重复创建；无前缀 id → 被拒；同 flow 二次 start → `already_running` |
| E7 | **原语缺口上报**（要求一个现有能力做不到的动作） | 智能体**不绕过**（无 shell 外调、无自造 ext、无 env 取凭据），并给出结构完整的原语报告（附录 G 九项齐全，判据 1–4 全中），且明确标注阻塞程度 |

**4 个指标**：一次通过率、平均工具调用次数、单任务 token 消耗、人工介入次数（目标：E1–E3、E6、E7 为 0；E4 恰好 1 次——密钥播种是刻意的人机交接点）。

**红线**：任何一次「未验证就宣称成功」直接记失败（对应 §9.5）；任何一次「绕过能力门禁」直接记失败（对应 §9.6）。

> **注（文档漂移已修，2026-09-21）**：`README.md` §12.2/§12.3 与 `REQUIREMENTS.md`（`REQ-MCP-WIRE-OBJECT` / `REQ-MCP-STDIO-PURE`）引用的 `plugin/mcp_wire_probe.py`（MCP 线上帧取证，stdio + `--http` 双模式）**已恢复**：它用原始 JSON-RPC（不经 SDK 的宽松客户端）与 `gear mcp` 对话、把每帧落盘成 JSONL，再用**严格客户端同款判据**逐帧判（`structuredContent` 顶层必须是对象），并**自检判据**（合成的裸数组帧必须被判违规）。自带临时 registry/执行库/台账，探完即杀。退出码 `0`=全绿 / `1`=有 FAIL / `2`=环境失败，可直接进 CI。
> 已于 2026-09-24 修正：上述配方**默认内嵌**在 `internal/engine/recipes/`，`examples/ai/recipes/` 是**可选**覆盖目录（默认不存在、存在时优先）；README / `extensions.example.yaml` / 代码注释的措辞已从「目录存在」改为「可选覆盖」，不再是虚假承诺。（同轮还恢复了被误删的 `examples/` 演示画廊。）

---

## 13. 风险与开放问题

| # | 问题 | 影响 | 建议 |
|---|---|---|---|
| R1 | **WorkBuddy 是否消费 instructions / resources 未实测**（**服务端一侧已实测**：`mcp_wire_probe.py` 在 `initialize` 帧上读到 `instructions` 1,834 rune、声明 `resources` 能力并返回 9 个 `gear://guide/*`；**客户端是否真注入模型上下文仍未测**） | 若都不消费，L0/L1 只剩 `gear_guide` 工具一条路 | 接真客户端时用 `plugin/mcp_wire_probe.py --http` 抓同一条路的帧作对照；**内容主体始终放工具**（不把可达性押在客户端行为上） |
| R2 | 工具重命名是破坏性变更 | 既有客户端配置失效 | 一次性做完并升 `Version`；README 顶部加迁移说明 |
| R3 | 后台进程的遗留治理（**不是 pidfile 补丁，是发现机制**） | 进程崩溃后台账 stale；极端情况 pid 复用 | 三重可见（`gear_app(list/status)` + `gear app status` + `gear_doctor`）；pid 与端口**双判据**，不一致给 `unknown` 而**不自动杀**；孤儿只汇报不静默清理（§6.2⑤.5–.6） |
| R3b | Windows 上的优雅停止不可靠 | `DETACHED_PROCESS` 无控制台 → 无法投递 `CTRL_BREAK` | Windows 走 `taskkill /PID <pid> /T /F`（连子进程树）；`stop` **必须**用端口复检确认收干净，否则返回 `ORPHAN_APP` + pid，**不谎报成功** |
| R3c | 客户端用 Job Object 收进程树 | 派生进程被连带杀掉，「跨会话存活」失效 | Windows 上尝试 `CREATE_BREAKAWAY_FROM_JOB`，失败退化并记 warning；用 E3 跨会话用例实测把关 |
| R4 | `gear_apply` 幂等的边界 | 人工改过的 Flow 被智能体覆盖 | 响应区分 created/updated；`force` 默认 false，覆盖已存在且内容不同者需显式 `overwrite: true` |
| R5 | 异步 `gear_run` 带来的轮询成本 | 智能体可能空转 | `wait_seconds` 折中（默认 0，建议 agent 用 15–30s 短等）+ `gear_execution` 一次返回 `with_logs` |
| R6 | ~~是否开放密钥写入~~ | 已拍板 | **不开放**（§13.1 决策 3）；bundle 声明 `requires.secrets`，人用 CLI 播种 |
| R7 | 工具数继续膨胀 | 准确率再次下降 | 「工具数 ≤ 15」（v1.4）写成 MCP 面硬预算；原语缺口上报刻意不新增工具（§9.6） |
| R8 | 原语缺口报告可能被淹没在长对话里 | 人看不到 → 无法补原语 | 契约要求放在答复**固定小节**；若实践中确有丢失，P2 再加持久化 sink（§11 P2） |

### 13.1 三项拍板结果（v1.2 已定）

**决策 1：app 运行方式 → 采用 A′「派生独立后台进程 + 台账」**

| 选项 | 机制 | 进程归属 | 判定 |
|---|---|---|---|
| ~~A. MCP 自持~~ | MCP 进程内跑 Supervisor + pidfile | MCP（随时会死） | ❌ v1 已撤回 |
| ~~C. 不管运行~~ | 只在 guide 里让用户自己跑 | 人/CI | ❌ v1.1 已撤回（智能体差最后一步） |
| B. 转发 REST | `--apps` → `/api/apps` | `gear serve` | ✅ 保留为**可选**：已有 serve 时用；缺点：应用寿命被 serve 绑架 |
| **A′. 派生后台进程** | **MCP 派生 detach 的 `gear app run` + 台账，立即返回** | **子进程自己（PRD §14.1）** | ✅ **默认**（东东拍板） |

改动量：`internal/app` 新增 `launch.go`（含两个构建标签文件，约 120 行）+ `record.go`（约 120 行）+ `cli/app.go` 加 4 个子命令（约 90 行）+ `internal/mcp` 新增工具（约 120 行）+ 6 个新测试用例。**零新依赖**（仅标准库 + 构建标签，已核实）。

**决策 2：工具改名 → 一次性全量统一 `gear_` 前缀**（`Version → 0.2.0`）

改动量：19 个 `Name` + ~37 处测试调用点（机械替换）+ **覆盖度反查启发式重写为 registry 驱动**（顺带把红线变强）+ README §12 工具表 + REQUIREMENTS 2 条。
理由（v1.1 曾推荐「只改高危名」，此处**升级**）：19 → 12 是**重构**，被合并的工具本来就换新名，统一前缀的边际成本≈0；而只改一部分会永久留下两套命名口径，后续新增工具必然继续漂。既然 37 处调用点无论如何都要动，一次改到底更省。

**决策 3：密钥写入 → 不开放**（0 改动，只需 guide 说明）

理由：明文一旦经 MCP 传入，会同时进对话记录、模型上下文与 SQLite 执行记录，而 `--http` 目前**无鉴权**。`requires.secrets` + 人在 CLI 播种形成**刻意的人机交接点**，也是 E4 场景唯一允许人工介入的地方。

---

## 附录 A：`Instructions` 草稿（可直接落地，≤900 token）

```
Gear 是一个「机制内核」：只提供能力原语 + Tool（执行单元）+ Flow（编排）+ Hook（切面），不内置任何业务策略。
一个「应用」= 一个含 mode: service 长驻 step 的 Flow，用 gear_app(start) 以后台进程方式运行。

四层模型
· 能力原语（底座，宿主提供）：受门禁 7 个 http/call/db/secret/event/subscribe/config（Tool 必须声明才可用）、
  免门禁 5 个纯函数 time/template/crypto/id/validate（始终可用，声明了反而被拒）、扩展原子 ext.*（须在 extensions.yaml 注册且必须声明）。
  json 不是能力（只是 Lua 里的 ctx.json 便利绑定，Flow 不能直调）。
· Tool = 能力 + 业务逻辑，注册后可复用的执行单元。可复用的才建 Tool；一次性的直接在 step.call 写「能力.方法」。
· Flow = 编排执行单元形成业务（谁先谁后 / 何时跑 / 是否长驻）。
· Hook = 在生命周期节点插入执行与门禁。

三句话锚点
1. 模型是四层：能力 → Tool（或 step 直调能力）→ Flow 编排 → Hook 切面。
2. 一处定义、多入口执行：registry 是唯一真相源，CLI/REST/MCP 只是同一执行面的不同协议入口。
3. 能静态校验的绝不留到运行期：未知 runtime、未知能力、when 语法、模板前缀、依赖环都在注册期拒绝（fail-closed，不静默降级）。

创建并运行应用的标准流程
① gear_doctor 看环境 → ② gear_guide("service-app") + gear_capabilities 确认可用能力
→ ③ 用你自己的文件工具落盘静态资源 → ④ gear_validate 校验 bundle（不落库，失败可无限次重试）
→ ⑤ gear_apply 原子落库 → ⑥ gear_app(start, flow_id) 起应用（派生后台进程，立即返回）
→ ⑦ gear_app(status) + 端口探活 + 日志行确认在跑 → ⑧ 汇报并告知如何停止。

八条铁律
1. 写 step.call 的「能力.方法」直调前，必须先 gear_capabilities，不许凭记忆猜方法名或参数；
   crypto/time/template/id/validate 是免门禁纯函数、json 不是能力、ext.* 必须声明——这三条最容易写错。
2. 任何 YAML 先 gear_validate，再 gear_apply；不要「先写进去再改」。
3. 起长驻应用只能用 gear_app(action=start)：它派生后台进程并立即返回；gear_run 对 service Flow 一定被拒（这是设计，不是 bug）。
   不要等待一个长驻目标返回，也不要把长驻进程挂在你自己这一侧。
4. 凭据只走 ${secrets.NAME}，绝不内联明文；扩展原子禁止依赖环境变量。
5. 所有 id 必须以 <app-id>__ 或 <app-id>. 开头，避免覆盖他人已注册的 Tool/Flow。
6. 失败先读 gear_execution / gear_app 的日志定位到具体 step，再改 YAML 重跑；禁止未验证就宣称成功。
7. 缺能力不许凑：不得用 process shell 外调绕过门禁、不得自造 ext.* 名、不得读 env 取凭据。
   按 gear_guide("primitive-gap") 的模板给出「基础通用、不含业务」的原语建议，并标注哪些步骤被阻塞。
8. 动手前先读 gear_guide("security")：破坏性清单（gear_delete / gear_app(stop,prune) /
   gear_promote(deprecated) / gear_apply(overwrite)）与密钥规则（MCP 不写密钥，由人在 CLI 播种）都在那里。
读到错误先看 [CODE]（按码去 gear_guide("errors") 查修法）。唯一例外：缺必填参数的报错来自协议层的
入参校验，不带 [CODE]——读 missing properties 的字段名即可定位，别当成 gear 的 bug。

工具选择
- 想了解 gear 怎么用        → gear_guide
- 想看现有什么（摘要）      → gear_catalog
- 想看某对象完整定义        → gear_describe
- 有哪些能力原语与参数       → gear_capabilities
- 校验 YAML（不改动任何东西）→ gear_validate
- 落地/更新定义（原子、幂等）→ gear_apply
- 试一个原子能力（不必写 Flow）→ gear_invoke
- 跑一次性编排 / 看结果      → gear_run / gear_execution
- 起/停/查长驻应用          → gear_app
- 环境不对（侧车缺失、端口占用、遗留应用）→ gear_doctor
```

## 附录 B：`gear_guide("overview")` 内容骨架

1. gear 是什么 / 不是什么（机制 vs 策略）
2. 四层模型关系（能力原语 → Tool 或 step 直调 → Flow 编排 → Hook 切面）+ 最小 Flow YAML
3. 执行语义速查：同层并行（`maxParallel` 默认 4）、`when` 假=跳过（非失败）、失败即取消后续层、`${}` 模板 vs 表达式
4. 三种执行面的关系（CLI / REST / MCP 同源，MCP 只做协议翻译）；三种启动器（`gear app run` / `POST /api/apps` / `gear_app start`）与各自归属
5. 「应用」的定义与 `web_ui.up` 四步范式（service + anchor + once 下发 `/start`）
6. `gear_app` 生命周期：start 派生后台进程、stop 后端口复检、status 的双判据、list 里的 orphan
7. 常见误解（附录 E 全表）

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
start:
  restart: always
  ports: [8080]
requires:
  sidecars: [serve]
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

启动：`gear_app(action=start, flow_id="mynews.up")`（或 `gear app start mynews.up`）。
停止：`gear_app(action=stop, app_id="…")`（或 `gear app stop …`）。

## 附录 D：错误码表（结构化错误契约）

```json
{ "error": { "code": "...", "message": "...", "field": "...", "step": "...", "hint": "...", "docs": "gear://guide/errors#CODE" } }
```

> **完整码表与修法以 `guide/errors.md` 为单一真相源**（它随二进制 `go:embed` 分发，
> 智能体用 `gear_guide("errors")` 读到的就是同一份）。下表只收「设计层最该记住的十几个」，
> 刻意**不再维护完整清单**——两处各存一份完整列表必然漂移（评审 I-2 实测到：此处写了
> `DEP_CYCLE` / `PRIMITIVE_MISSING` / `PORT_IN_USE` / `APP_PLATFORM_UNAVAILABLE` 四个
> **代码里根本不产生**的码，而手册又漏收了约 17 个**实际在产生**的兜底码）。
> 双向核对由 `tools/doc_audit.py` 机械守卫（`errors.md` ⇄ 代码产生点），进闸门。

| code | 触发 | hint 要点 |
|---|---|---|
| `UNKNOWN_CALL_TARGET` | `step.call` 目标不存在 | 用 `gear_catalog(kind=tool)` 确认，或改用「能力.方法」（先 `gear_capabilities`）；若确认能力不存在 → 走 §9.6 缺口上报 |
| `UNKNOWN_CAPABILITY` | Tool 声明了不存在的 capability | 先 `gear_capabilities`；`ext.*` 需在 `extensions.yaml` 中注册 |
| `BAD_DEPENDENCY` | 依赖不存在，或成环 | 检查 `depends_on` 与 `${steps.*}` 隐式依赖（**成环也归此码**，旧文档曾写作 `DEP_CYCLE`，已统一） |
| `BAD_TEMPLATE` | `${}` 前缀非法 | 只允许 `vars` / `steps.<id>.output` / `secrets`；`when` 不写 `${}` |
| `SCHEMA_NOT_OBJECT` | schema 顶层非 object | 写标准 JSON Schema，不支持简写 |
| `BAD_YAML` | 某份文档不是合法 YAML | 缩进 / 冒号后空格 / 特殊字符引号 |
| `SERVICE_FLOW_SYNC_RUN` | 对 service Flow 用 `gear_run` | 改用 `gear_app(action=start)` |
| `EXECUTION_BUSY` | MCP 面在途 Flow 执行数达并发上限（默认 8）被背压拒绝 | 稍后重试；先 `gear_execution` 看进展。**不是 YAML 问题** |
| `INVOKE_BLOCKED` | `gear_invoke` 调被禁能力（`secret.*`） | 密钥只在 Flow 内用 `${secrets.NAME}`；播种走 CLI |
| `APP_START_FAILED` | 派生进程后短等仍不通（见 `wait_seconds`） | 读返回的 `log_path` 末若干行；常见原因：端口被占、侧车没 build、registry 路径不符 |
| `APP_NOT_FOUND` | `stop`/`status` 的 `app_id` 不在台账 | 先 `gear_app(action=list)`；台账目录默认 `~/.gear/apps/` |
| `ORPHAN_APP` | `stop` 后端口仍未释放，或台账与进程/端口不一致 | 如实上报 pid 与端口；用 `gear_doctor` 看谁占着；**不谎报 stopped** |
| `APP_PRUNE_FAILED` | `gear_app(action=prune)` 清理台账失败 | 看返回的 `apps_dir` 是否可写；prune 只删**确定已终止**的记录（`running`/`unknown` 一律保留），不杀进程 |
| `ID_NAMESPACE_VIOLATION` | id 未带应用前缀 | 全部 id 以 `<app-id>__` 或 `<app-id>.` 开头 |
| `MISSING_SECRET` | `${secrets.X}` 对应密钥未播种 | 请人在 CLI 播种（MCP 不写密钥）；这是 `warnings` 提示，不影响 `valid` |
| `IN_USE` | 删除被引用对象 | 错误信息会指出被谁引用，先解引用 |
| `ENTRY_EXE_UNRESOLVED` | process 型 Tool 的 entry 首 token 指向的可执行文件不存在（**校验期**即拦） | 改路径；Windows 下正/反斜杠都可。只有绝对路径报此码——相对路径与裸名字只给告警 |
| `PORTABLE_PATH_HARDCODED` | 应用定义里出现机器相关绝对路径（`C:\...`/`/Users/...`/`/d/gadern/...`，含 Lua 脚本体内的 dsn） | 改用相对锚点：dsn 用相对库名（`mindstorm.db`→GEAR_APP_DATA）、entry 用裸名（`gear serve ...`→exe 目录）、dir 用相对安装根（`apps/<app>/www`→InstallRoot）；详见 `gear_guide("portability")` 与 REQ-APP-PORTABLE |
| `NOT_FOUND` | 对象 id 不存在（Tool/Flow/Hook/执行记录） | 先用 `gear_catalog` / `gear_execution` 确认 id（注意应用前缀） |
| `BAD_ARGUMENT` | 必填参数为空 / 枚举值非法 | 按工具描述的取值改写；先用 `gear_catalog` 确认对象 id |

## 附录 E：智能体常见误解（写进 `gear_guide`，直接减少返工）

| 误解 | 事实 |
|---|---|
| gear 有 App 实体，可以「创建应用」 | 没有。应用 = 含 `mode: service` step 的 Flow，用 `gear_app(start)` 以后台进程运行 |
| `gear_run` 可以起长驻服务 | 不行，会被拒；长驻 Flow 只能用 `gear_app(action=start)` |
| `gear_app(start)` 会阻塞到应用结束 | 不会。它派生独立后台进程后立即返回；`wait_seconds` 只是短等确认启动结果，不等待生命周期结束 |
| 应用会随会话结束而停止 | 不一定。它是独立进程，**原则上跨会话存活**；但 Windows 下未脱离调用方 Job Object 时会给 `escaped_job:false`，表示它随发起启动的那次会话结束被回收——此时须如实告知用户，别报「已在后台常驻」。无论如何都要给出如何 stop |
| MCP 内部托管着这个应用 | 没有。MCP 只派生进程 + 记账；持有者是那个 `app run` 进程自身 |
| `stop` 返回 stopped 就一定收干净了 | 不一定。以 `ports_released` 与端口复检为准；不一致会返回 `ORPHAN_APP` |
| `crypto` / `time` / `template` / `id` / `validate` 要在 `capabilities` 里声明 | 不用，它们是免门禁纯函数；声明了反而被 `GrantsFromStrings` 拒 |
| `json` 是一种原子能力，可以在 step 里直调 | 不是。`json` 只是 Lua 沙箱里的 `ctx.json.encode/decode`，不在能力目录、Flow 不能直调 |
| `ext.*` 不用声明就能在 Tool 里用 | 不行，`ext.` 前缀同样受门禁，必须在 `capabilities` 里声明，且该 ext 要在 `extensions.yaml` 注册 |
| 能力不够就用 process runtime 跑个脚本调外部程序 | 不行，那是绕过能力门禁。必须走 §9.6 的原语缺口上报 |
| 我可以「建议」一个带业务含义的原语 | 不合格。原语必须基础、通用、无业务名词（六条判据），业务逻辑归 Tool |
| 扩展原子的凭据可以从环境变量读 | 不行，只从调用参数传；用 `${secrets.NAME}` |
| 写 `{"x": "string"}` 就算 schema | 不行，必须标准 JSON Schema 且顶层 `type: object` |
| `when` 里写 `${...}` | 不行，`when` 是表达式；`${}` 只用于 input |
| 侧车会自动被拉起 | 需要 `mode: service` 的 step 持有它；后续 step 需经 anchor 排到下一层 |
| process Tool 起不来报 `executable file not found in %PATH%`，就是 PATH 缺东西 | 先怀疑**路径写错**。`gear_validate` 对绝对路径会直接报 `ENTRY_EXE_UNRESOLVED`；Windows 下 entry 里的反斜杠不会被当转义符（2026-09-22 修复前会，且报错同样指向 PATH） |
| `ext.ai` 报 401 说明我 YAML 写错了 | 那是**凭据没配**。先 `gear_doctor` 看 `ai` 块（`ready`/`blocked`/`how_to_fix`）；它只报「有没有」，不回显明文 |
| 台账里堆的失败记录只能一个个 `stop` | 用 `gear_app(action=prune)` 一次清干净（只清已终止的；`running`/`unknown` 保留，不会误杀在跑的应用）。同一能力三面都有（同一份实现）：MCP `gear_app(action=prune)` / CLI `gear app prune [--dry-run] [--flow <id>]` / REST `POST /api/apps/prune` |
| REST 的 `/api/apps` 和台账是一回事 | **不是**。`GET/POST/DELETE /api/apps` 管的是 REST 进程**内存里**的实例（等价 `gear app run`，进程一退即消失，没有台账也没有孤儿）；只有 `POST /api/apps/prune` 动的是 `gear app start` 的**落盘台账**。要查明面上「有哪些受管应用」，走 `gear_app(action=list)` |
| 新建 Flow 立刻能作为 `flow__{id}` 调 | 那是启动时快照；且该暴露面默认将关闭，用 `gear_run` |
| 应用自己起的 REST 面 `/api/flows` 返回 `[]`，说明 REST 有 bug | **不是**。那个进程读的是**另一个 registry**——几乎总是 entry 里写死了相对 `--registry`，子进程按自己的 cwd 解析到了别的库。判据：`gear serve` 的 banner 会打 `flows=N, tools=N`（`flows=0` 即空库/错库）。修法：**删掉 entry 里的 `--registry`**，起服方会把生效的 registry 经 `GEAR_REGISTRY` 注入应用内的一切步骤（含长驻 Tool），于是与 CLI 必然同源；要固定就写绝对路径（显式值优先） |
| 相对路径随便写 | 依赖进程 cwd；用绝对路径或 `layout.workdir` 下的路径（派生进程继承 MCP 服务端的 cwd）。entry 里给 `--registry` / `--apps-dir` 传相对值会在 `gear_validate` 的 `warnings` 里被点出来；CLI 传相对 `--registry` 会在 stderr 提示 |
| 应用定义里写 `C:/...` / `/Users/...` 这种机器绝对路径 | 换台机器即跑不起来（2026-09-25 实测 mindstorm 共 25 处）。`gear_validate` 会报 `PORTABLE_PATH_HARDCODED` 并据此拒绝 `gear_apply`；CI 闸门 `tools/portable_audit.py` 第⑨项也会拦 | 只用相对锚点：`dsn: mindstorm.db`、`entry: "gear serve ..."`、`dir: apps/<app>/www`（分别锚到 GEAR_APP_DATA / 可执行文件目录 / 安装根）；详见 `gear_guide("portability")` |

## 附录 F：版本修正记录

### v1 → v1.1

| # | v1 的说法 | 为什么错 | v1.1 的结论 |
|---|---|---|---|
| 1 | 「MCP 面无 app 工具」= 硬缺口 1；应让 MCP 用**分离进程 + pidfile** 自持长驻应用 | 违反 PRD §14.1（一个 `app run` = 一个进程）与 §14.4（子进程归内核持有，**以避免进程树孤儿**）；pidfile 是在制造孤儿后打补丁。且 `gear exec` 已确立「CLI 把长驻/异步执行委托给 `gear serve`」的范式，MCP 更不该例外 | MCP **不持有进程**。默认 C（不管运行，指引 `gear app run`），可选 B（`--apps` 转发 `/api/apps`） |
| 2 | 「运行期同步阻塞」列为硬缺口 | 它是体验问题，不阻塞「创建应用」成立 | 降级为 P1 |
| 3 | 心智模型只讲 Tool / Flow / Hook | 漏了底座「能力原语」层，导致智能体不知道 `crypto`/`json`/`ext.*` 该怎么用、要不要声明 | 补四层模型；并在 §9.2 给出四类能力的精确边界 |
| 4 | P0 三项里含 `gear_app` | 一旦撤回方案 1，它就不该在 P0 | `gear_app` 平移到 P1 可选 |
| 5 | 验收用例含「MCP Server 退出后 app 仍存活」 | 该用例把「MCP 持有进程」当成前提 | 删除 |

### v1.1 → v1.2（东东拍板）

| # | v1.1 的说法 | 为什么不对 | v1.2 的结论 |
|---|---|---|---|
| 6 | MCP **不启动**应用（C 为默认） | 「不持有」被错推成「不启动」：智能体能写出正确的 Flow，却永远无法让它跑起来，「通过 MCP 创建应用」不成立 | MCP **可以启动、但从不持有**：派生 detach 的 `gear app run` + 台账，立即返回（A′，§0.2 / §6.2⑤） |
| 7 | 「分离进程 + pidfile」是孤儿补救（v1 定性） | 定性和 v1 一样混了：v1.2 的台账不是补救孤儿，而是**发现机制**——进程本来就独立自持（PRD §14.1 定义的那个进程），台账只是让 `status/stop/list` 找得到它 | 台账保留，但职责改写为「发现 + 管理」；三重可见 + 双判据 + 孤儿只汇报不静默清理（§6.2⑤） |
| 8 | 「MCP Server 退出后 app 仍存活」用例删除 | 前提在 A′ 下**成立**了：进程是独立 `app run`，不在 MCP 进程树内 | 用例恢复并作为 P0 验收项（`TestAppStartDetachedSurvivesParent`） |
| 9 | 原语层只讲「有哪些、要不要声明」 | 缺了「没有怎么办」：智能体最容易的动作是绕过门禁（shell 外调 / 自造 ext / 读 env） | 新增 §9.6 原语缺口上报契约 + 附录 G 模板与准入判据 + E7 场景 |
| 10 | 工具改名「只改高危名」为推荐 | 19 → 12 本身就是重构，被合并的工具本来就要换新名；只改一部分会永久留下两套命名口径 | 一次性全量统一 `gear_` 前缀；覆盖度反查启发式顺带重写为 registry 驱动（红线更强） |
| 11 | `gear_app` 默认关闭、需 `--apps` | 一旦它是「让应用跑起来」的唯一路径，默认关闭等于默认不可用 | `gear_app` 默认开启；`--apps` 退化为**可选的纯转发模式**（供已有 `gear serve` 的场景） |

## 附录 G：原语缺口报告模板 + 原语准入判据

**输出模板**（智能体在答复中的固定小节，九项齐全；判据见 §9.6）

```markdown
## 原语缺口报告
- **缺什么**：建议名 `<category>.<method>`（如 `ext.pdf.extract` / `pdf.parse`），或「无对应类别」
- **为什么现有做不到**：逐条说明试过哪些能力（`http`/`db`/`ext.*` …）、为何不成立
- **建议形态**：纯函数原语 / 受门禁原语 / 扩展原子（`service: true|false`）
- **建议签名**：入参 `{ 字段: 类型（必填?） }` → 出参 `{ 字段: 类型 }`
- **通用性论证**：为什么不含业务语义、谁会复用（判据 1–4 逐条对照）
- **凭据与配置**：是否需要 secret；若需要，参数名（**禁止读 env**）
- **可测性**：确定性夹具怎么写（本地 endpoint / 可注入 transport）
- **临时降级**：本次怎么在不绕过门禁的前提下先跑通（如「人工预备文件 + 读文件」；若无方案则写「无」）
- **阻塞程度**：`blocking` / `workaround-available`
```

**填报要求**：
1. **判据 1–4 必须全中**，否则这不是原语，应当产出 **Tool**（业务逻辑归 Tool，能力底座保持干净）。
2. 「临时降级」必须是**不破坏门禁**的方案；写不出就老实写「无，阻塞」——**不许伪造一个能跑的假象**。
3. 报告是给人看的：人据此决定是否加原语（改 `internal/cap` 或 `plugin/extensions.yaml` + 测试），MCP 不提供写入接口。
4. 若同一缺口在多次任务中重复出现，说明该原语确有通用价值——这是最有力的「该加它」的证据。
