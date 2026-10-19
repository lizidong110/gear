# ai：用 gear 打造自己的 agent

## 1. 先选形态：调内核的，还是自己写

|  | 调 `ctx.ext.ai.react` | 手写 Lua 循环 |
|---|---|---|
| 代码量 | 一行 | 约 20 行 |
| 循环逻辑 | 内核写死，你能调 `max_steps` / `allow` | 完全自己控制（提示词、终止条件、工具集） |
| 适合 | 任务型：给一句话，让它自己跑完 | 产品型：循环策略本身就是你的差异点 |

**第三种形态（在 Flow YAML 里声明循环）不支持**：`model.Step` 没有 `loop`/`retry` 字段，
且 Flow 的拓扑排序遇到环会直接拒绝。**agent 循环只能活在 Lua 或 Go 里**，不能活在 Flow 声明层。

## 2. 路径一：调 `ctx.ext.ai.react`（最短）

写一个 Tool，`capabilities: [ext.ai]`，entry 一行：

```lua
local r = ctx.ext.ai.react({ task = ctx.input.task, max_steps = 12 })
return { answer = r.answer }
```

CLI 等价：`gear ai react "任务" --max-steps 6 --allow http,db`。还可加 `--mode meta`（元工具模式）。

**它走原生 function calling**：工具声明由能力目录自动生成后随请求下发，模型用 `tool_calls`
回调用意图，引擎不再从自由文本里抠 JSON 围栏。两个后果：

- 工具名里的 `.` 会被压成 `_`（`http.Request` → `http_Request`）——OpenAI 的 function name
  字符集所限，引擎内部自动反查回原名，你不用管。
- 模型给了目录里不存在的工具名时，会收到「不存在 + 可用工具清单」，而不是一段 Lua 栈错误。

返回 `{answer, steps, used_tools}`——**类型别猜**：`steps` 是每一步的**明细数组**
（元素 `{thought, action, observation}`），不是步数；声明成 `number` 会在 output 校验时被拦下。

### 2.4 工具权限：默认就是你自己那份，想多给得显式声明

**react 能用哪些工具，取决于调用它的那个 Tool 声明了哪些 `capabilities`**：

| 调用方 | react 的工具集 |
|---|---|
| `capabilities: [ext.ai]`，不给 `allow` | 只有 `ext.ai.*`——**不是**全目录 |
| 一份 capability 都没声明 | 一个工具都不给（不是退回全目录） |
| 显式给 `allow`，且在自己的 capability 内 | 就按给的来（范围内照常放行） |
| 显式给 `allow`，超出自己的 capability | **直接报错**——这是后门，不给 |

```lua
-- 这个 agent 能上网，但不能碰数据库和密钥
ctx.ext.ai.react({ task = ctx.input.task, allow = { "http" } })
```

别反过来理解：能力边界写在 Tool 的 `capabilities` 上，**执行通道也照它收敛**。旧行为是
「`allow` 没传就给全部门禁原语」，于是一个自称只要 `ext.ai` 的 agent 实际能把 `secret.get`
交给模型调（2026-09-27 收口）。想让 agent 用更多能力，就在 Tool 的 `capabilities` 里声明，
或显式传 `allow`（必须是你已声明的子集）。

CLI 那条路径不受影响：`gear ai react` 由命令自己决定 `--allow`，不经过 Tool 的能力边界。

### 2.4.1 工具不是越多越好：方法级 allow 与工具数上限

能力目录实测 **28 个能力 / 115 个方法**（`gear ai capabilities` 可查）。react 会把每个被允许
的方法展开成一条 function 声明随请求下发，**每一轮都重发**。工具越多模型越容易选错——同一个
能力下几十个近义方法时尤其明显，单轮光工具定义就能吃掉上万 token。

所以「给权限」和「给工具面」是两件事，可以只放宽前者：

- **方法级 allow**：只想用 redis 的两个方法，写 `allow = { "ext.redis.get", "ext.redis.set" }`
  就够了，不必开放整个 `ext.redis`（29 个方法）。
- **工具数上限 48**：一次 react 最多下发 48 个工具，超了**直接报错**而不是静默截断——
  截断等于偷偷改了 agent 的能力边界，模型调不到某个工具却不知道为什么。报错会点名超限并
  提示用方法级 allow 收窄。

```lua
-- 能碰 redis，但只开两个方法：工具面 2 个，权限仍在自己声明的能力内
ctx.ext.ai.react({ task = ctx.input.task, allow = { "ext.redis.get", "ext.redis.set" } })
```

CLI 那条路径以前「不给 `--allow` 就是全目录」，现在也收窄了：不给 `--allow` 用一组通用能力
（http / call / db / secret / config / time / template / id / validate + ext.registry，共 23 个方法），
重型扩展（ext.redis 29 个、ext.oss 6 个）和 crypto（13 个）不在其中。要全目录请显式
`--allow-all`，但那多半会撞上 48 的上限——撞了就说明该改用方法级 `--allow`。

### 2.4.2 元工具模式（`mode = "meta"`）：工具面恒定 4 个，与目录规模解耦

上面的「工具越多越降智」还有个更彻底的治法，对应你之前问的「MCP 面为什么只挂 15 个意图级工具、
而不是把 115 个方法平铺」——react 之前恰恰踩进了 MCP 面刻意避开的那个坑：把 115 个方法平铺进
工具面、每轮重发。元工具模式把这个坑填了：

```lua
ctx.ext.ai.react({ task = ctx.input.task, mode = "meta" })
```

工具面**恒定 4 个**，不再随能力数增长：

| 元工具 | 作用 |
|---|---|
| `ext_ai_find(query, domain?, limit?)` | 在能力目录里按语义检索方法（BM25），如「加密一段文本」「发邮件」 |
| `ext_ai_describe(id)` | 拿到方法全名后，查它的完整参数契约（类型 / 必填 / 枚举） |
| `ext_ai_invoke(capability, method, args)` | 真正执行一个方法（find→describe→invoke 的最后一跳） |
| `ext_registry_tools(prefix?, limit?)` | 发现调用方在 registry 里定义的业务 Tool / Flow |

工作流是 **find → describe → invoke**：模型先检索定位方法、看清楚参数怎么填，再直调。
**检索步（find/describe/registry）不计入步数预算**，只有 invoke 算一步——所以 `max_steps`
约束的是「真正动手几次」，而不是「想了几步」。单轮请求的工具定义从 10-15k token 降到几百，
且 `--allow-all` 在元模式下**不再撞上限**（工具面永远 4 个）。

> 内置检索靠 `ext.ai.find` / `ext.ai.describe`（BM25 + 受控关键词表，按能力域归类）；
> 它检索的是**能力目录的方法层**，与 `ext.ai.catalog` 检索的 registry 业务 Tool 是两套语料，别混。

## 2.5 一个装好就能跑的完整范例

`examples/agent-app/` 就是本文的最小实证形态，整套定义只有一份 bundle：

| 对象 | 作用 |
|---|---|
| `agent__ask` | 上面那一行 react（配 `capabilities: [ext.ai]` 与 `timeout: 15m`）——**这就是「agent」的全部** |
| `agent__api` | process 长驻起一个 `gear serve`，作为应用自己的后台执行面 |
| `agent__site` | 用内核的 `ext.serve` 托管 `www/`，并把 `/api` 反代到后台 |
| `agent.up` | anchor → serve(service) → api(service) → site 四步 |

装上：`gear import examples/agent-app --apps-dir examples/agent-app`
起服：`gear app start agent.up --port 8110 --wait 10`

四个 step 各有其存在理由，来源都是实测踩过的坑（bundle 里的注释更完整）：

- **`site` 步刻意不复用 `static_site.serve`**：那是一个历史遗留的无主 Tool，只存在于当年
  注册它的那个库里，换一个干净的 registry 装本范例会直接报「未知的原子能力 `static_site`」。
  它做的事本来就是内核的 `ext.serve` 能力，自己写十几行即可，别去库里「借」。
- **后台端口在范例里是写死的**：想用内核分配端口（`--addr 127.0.0.1:0`）就得让站点步引用
  `${steps.api.output.port}`，而这要求 site 排在 api 之后——偏偏「service 永不结束、不能作为
  依赖」是引擎硬约束。于是只剩「写死」或「赌同层并行谁先跑」两条路，前者可复现。
  （这条缺口已登记为 `REQ-APP-SITE-DEPS`。）

## 2.6 失败恢复层：出错了谁先兜

react 的每一步都可能失败：工具报错、provider 限流、模型给了坏参数、查询空手而归。
原先把一切错误当同一句 Observation 丢回给模型，恢复全靠「模型自己再说一遍」，provider 一出
瞬时错误整个 react 直接死。现在的分层是（`internal/engine/ai_recovery.go`）：

| 层 | 触发条件 | 动作 | 烧 LLM 轮次 |
|---|---|---|---|
| L0 机械重试 | transient（超时 / 429 / 连接重置，含 body 里的 429/502/503/504） | 引擎直接重试，退避 150ms 起翻倍封顶 2s | 否 |
| L1.5 换通道 | 端点不吃工具声明 | 降级为文本 Action 协议（重建系统提示词）；元工具模式刻意不降级 | 否 |
| L1 结构化信号 | 其余可恢复失败（bad_input / upstream_bad / semantic_empty） | 回灌分类 + 范围 + 原因 + 建议，由模型重决策 | 信号本身不烧，模型下一步才烧 |
| 震荡守卫 | 同一动作同类型失败连续达阈值 | 封禁该动作 + 记缺口，不再烧预算 | 否 |
| 兜底 | auth / unrecoverable 立即记缺口；其余阈值到了记 | 步数耗尽的答案带缺口清单（带缺失作答） | 否 |
| 收尾补全 | 循环结束 | 最后以失败收尾的动作统一转缺口——单次失败后模型放弃作答也留痕（`gaps=[]` 但答案缺了数据 = 静默残缺） | 否 |

七类失败：`transient` / `channel` / `bad_input` / `upstream_bad` / `semantic_empty` / `auth` /
`unrecoverable`，每类一张声明式策略表；判不出的一律 `unrecoverable`——**方向选不可恢复**：
当可恢复会在确定性错误上白烧预算，当不可恢复只损失一次重试机会。错误自己实现
`RecoverableError` 接口时听实现方的（tool 最懂自己的错误），中央 heuristic 只兜底。

**对调用方的新增输出**（始终存在、空切片，不必先判存在性）：

- `gaps`：缺口清单（哪个动作、哪类失败、原因）。auth 与 unrecoverable 立即进；其余震荡后才进。
- `recovery_log`：恢复流水（重试几次、换了什么通道、封禁了什么），排查「它为什么这么答」用。

四个反直觉点，都是踩出来的：

- **`ctx.http.Request` 不把非 2xx 当错误**，status 在 body 里。不接住 body，L0 对 HTTP 类工具
  等于不存在——这是七类里 transient 最常见来源，却最容易漏。
- **引擎重试与模型重发必须分开**：重复调用守卫只拦模型（同工具同参数），不拦引擎的 L0——否则
  重试第一步就被自己人拦下，恢复层形同虚设。
- **`max_steps` 计数的是 invoke**，机械重试不占步数。烧的是 provider 配额，不是执行步预算。
- **错误文案跨平台不一样**：Windows 的连接拒绝是
  `connectex: No connection could be made because the target machine actively refused it`，
  不含 "connection refused" 子串。分类器的关键词表两套文案都收了（2026-10-19 E2E 实测：
  只认 Unix 文案时，Windows 上死端口拒绝会被误判成 unrecoverable，L0 重试从不发生）。
  自己的 tool 要精确分类，实现 `RecoverableError` 接口——实现方最懂自己的错误。

## 3. 路径二：手写 Lua 循环（真正自建）

```lua
local history = ""
for i = 1, 10 do
  local r = ctx.ext.ai.complete({
    system = "你是 agent。只输出 JSON：{\"action\":{...}} 或 {\"done\":\"答案\"}",
    prompt  = ctx.input.task .. "\n\n历史:\n" .. history,
    format  = "json",
  })
  if r.data and r.data.done then
    return { answer = r.data.done }
  end
  local out = ctx.call(r.data.action.tool, r.data.action.args)
  history = history .. "\n工具 " .. r.data.action.tool .. " → " .. ctx.json.encode(out)
end
return { answer = "达到上限" }
```

**五件事必须自己做**，每一件漏了都会以诡异报错的形式回来：

| 你得做的事 | 不做会怎样 |
|---|---|
| 声明 `capabilities` | 权限只按**你自己 Tool 的那份**算。react 走的是全放行宿主，自建循环没这个待遇 |
| 填 `timeout` | 空值撞引擎默认 30s，报 `context deadline exceeded`（mindstorm 实测填 `15m`） |
| 写深度计数 | `ctx.call` 的 Tool 分支**没有环检测**（只有 Flow 分支有），自调用会无限递归 |
| 拼对话历史 | `complete` 无多轮，请求里只有 system + user 两条 |
| 记住 `ctx.call` 是裸函数 | 是 `ctx.call(target, input)`，不是 `ctx.call.Call(...)` |

## 4. 已知边界

- **没有 embedding**：语义检索暂不支持。工具在几十量级时，全量 schema 下发就够选得对；
  超过百级、或真要检索业务文档（RAG）时再引入。
- **`complete` 无 function calling**：只有 `react` 走 `tool_calls`。手写循环要自己定 JSON 协议并自己解析。
- **观察结果截断到 3000 rune**：一次列出大量条目（比如 registry 里所有 Tool）时，模型看到的
  是截断后的内容。截断会**如实告知**：写清原文总长、清单真实条数，并指向可缩小范围的手段。
  所以问「一共有多少个」仍可能答错——它看到的是清单的前 3000 rune；
  要精确计数请自己分次查：`ext.registry.tools({prefix="mindstorm__", limit=10})`。
  默认最多返回 50 条，prefix 前缀匹配 id。
- **`gear ai react` 的写隔离**：先把持久 registry 的目录灌进内存库再跑，agent 看得到真实定义，
  但执行产生的写不会落进持久库。库里若有声明了不存在原语的过期定义，会被跳过并在 stderr 点名
  （修法：改对 `capabilities` 重新注册——`mindstorm__ui_set`/`ui_get` 当年就卡在声明了内核已删除的
  `kv` 上，2026-09-27 已改用 `config` 原语 + `ctx.config.set/get`）。
- **维护应用请走 MCP 的 `gear_apply`**，不要在内核里给 AI 开 registry 写通道：
  `gear_apply` 有校验、有回滚、有冲突提示，直接写库会把这三样全绕过去。
