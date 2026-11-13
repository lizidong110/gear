# atoms：能力原语与扩展原子

## 1. 四类能力（先分清，再看用法）

| 类别 | 成员 | 受 capability 门禁 | Flow 能直调 | 说明 |
|---|---|---|---|---|
| **受门禁原语** | `http` `call` `db` `secret` `event` `subscribe` `config` `fs`（8） | **是**（Tool 必须声明） | 能（如 `http.Request`） | 单一真相源 `internal/cap/cap.go` |
| **纯函数原子** | `time` `template` `crypto`（3） | **否**（始终可用，**声明了反而被拒**） | 能（如 `crypto.hash`） | 无 IO、确定性、单具名 table 入参 |
| **Lua 便利绑定** | `json`（`ctx.json.encode/decode`） | — | **不能** | **不是能力**，不在能力目录 |
| **调用上下文** | `caller`（`ctx.caller.subject/app_id/via`） | — | **不能** | **不是能力**，内核无条件注入（2026-09-26 补） |
| **扩展原子** | `ext.email` `ext.serve` `ext.kafka` … | **是**（还须在 `extensions.yaml` 注册） | 能（如 `ext.serve.start`） | 分 once 与 `service: true` 两种 |

**方法名与参数一律以 `gear_capabilities` 为准**——下面只列确信的常用形态，别凭记忆猜。

## 1.1 调用形态：`invoke_form`（写错形态是运行期才炸的那类错）

⚠ **Lua 侧的入参形态是混合的，不是「一律位置参数」**。目录里每个方法都带两个字段说明它：

| 字段 | 取值 | 含义 |
|---|---|---|
| `invoke_form` | `positional` / `table` / `bare` | 按位传参 / 单个具名 table / 裸函数 |
| `lua_call` | 如 `ctx.db.query({ sql = string })` | **可直接复制的完整调用** |

各能力的实际形态（判据唯一实现 `internal/engine/invokeform.go`，与 `lua.go` 的绑定一一对应）：

| 形态 | 成员 | 写法 |
|---|---|---|
| `positional` | `http.Request` `secret.get` `event.notify` `event.wait` `subscribe.stop` | `ctx.http.Request(method, url, body, headers)` |
| `bare` | **`call`** | `ctx.call(target, input)`——**没有 `.Call` 这一段** |
| `table` | `db.*` `config.*` `fs.*` `subscribe.start` 全部纯函数原子 全部 `ext.*` | `ctx.db.query({ driver=…, sql=…, params={…} })` |

三类典型误用（都会在 Lua 运行期才炸，报错还看不出病因）：

- `ctx.call.Call(target, input)` —— 这是**索引一个函数**，不是调用它。
- `ctx.db.query(sql, params)` —— `db.*` 收单个具名 table，不是位置参数。
- `ctx.config.get(key)` —— 同上，`config.*` 也是单 table。

**Flow 的 `step.call` 不受这套形态约束**：它写的是调用目标标识（`db.query`）+ 具名 `args`，
不带 `ctx.` 前缀、也不是 Lua 表达式（见 `gear_guide("authoring")`）。

## 2. 受门禁原语的常用方法

| call | 形态 | 参数（必填加 *） | 返回 |
|---|---|---|---|
| `http.Request` | positional | method*, url*, body, headers | `{status, body, headers}` |
| `call.Call` | **bare** | target*, input | 目标的输出 map |
| `db.query` | table | sql*, driver, dsn, params | 行集合 `[]map` |
| `db.exec` | table | sql*, driver, dsn, params | nil（失败经 err 表达） |
| `db.batch` | table | statements*, driver, dsn | `{results, rows_affected, committed}`（**全或无**） |
| `db.ping` | table | driver, dsn | 连通性 |
| `secret.get` | positional | name* | 明文（**只读取**，看不到存储位置） |
| `event.notify` | positional | name*, payload | 广播（尽力通知） |
| `event.wait` | positional | name* | 下次通知的 payload |
| `subscribe.start` | table | source*/config*/callback* | handle |
| `subscribe.stop` | positional | handle* | nil |
| `config.get` | table | key* | 配置值字符串（**不存在平返回空串**，KV 语义，便于 `or ""` 兜底） |
| `config.set` | table | key*, value* | nil（已存在则覆盖；要清除用 `delete`，不是写空串） |
| `config.delete` | table | key* | nil（不存在即报错） |
| `config.list` | table | prefix（空串 = 全部） | `{key: value}` map |
| `fs.read` | table | path* | 文件内容字符串（path 相对安全根，默认 `GEAR_APP_DATA`） |
| `fs.write` | table | path*, data*, append | 写入字节数（append=true 追加，否则截断重写；父目录自动创建） |
| `fs.list` | table | path*（空串 = 根） | `[{name, size, is_dir}]` 数组 |
| `fs.mkdir` | table | path* | nil（递归建目录，已存在不报错） |
| `fs.remove` | table | path* | nil（只删文件/空目录；**不允许删安全根本身**） |

「形态」列不是装饰——它就是 Lua 侧真实收参的方式，写错要到运行期才炸（见 §1.1）。

用 `db` 连外部库时，口令**优先写 `${secrets.*}`**，不要内联明文。

**`db.batch` = 一次调用多条语句，全或无**（2026-09-28 新增）：

```lua
local r = ctx.db.batch({ driver = "sqlite",
  statements = {
    { sql = "UPDATE stock SET n = n - 1 WHERE sku = ?", params = { "A-1" } },
    { sql = "INSERT INTO orders(sku, qty) VALUES('A-1', 1)" },
  } })
-- 全成：r.committed == true，r.rows_affected == {1, 1}
-- 任一失败：r.committed == false，r.results 里失败那条带 .error 并指明第几条
--           —— **整批已回滚，本次写入按未发生处理**，别再去比对数了几条
```

三条必须记住的口径：

- **失败不是异常**：语句失败走 `committed=false` + `results`（而不是抛错），这样应用能拿到
  「第几条、什么错」继续走自己的分支。但对应用来说**必须判 `committed`**——回滚是内核做的，
  不判也不会留下脏库，只会在业务上以为写成功了。
- **DDL 可与 DML 混批**（Q3 已拍板）：SQLite 在同一事务内二者同原子，回滚时建的表也消失；
  其它方言按各自 SQL 规矩，别默认同款。
- **没有跨调用事务**：`batch` 是单批原子的。要跨多次 `db.exec` 共享一个事务，本批没有这个能力
  （见 `ENTERPRISE_PRD.md` FR-2 的「句柄式事务」一节）。

**库演进声明在 bundle 的 `kind: Migrations`，不要在每个 Tool 里 `CREATE TABLE IF NOT EXISTS`**（2026-09-28）：

```yaml
kind: Migrations
app_id: myapp
driver: sqlite
dsn: myapp.db          # 相对库名锚 GEAR_APP_DATA；**必填**——dsn 缺省会落到内存 scratch 库
migrations:
  - {version: 1, name: init-orders, sql: "CREATE TABLE orders(...);"}
  - {version: 2, name: add-index,   sql: "CREATE INDEX ...;"}
```

四条口径，每条都有代价：

- **先改库、再落定义**：`gear_apply` / `gear_import` 里迁移跑在**所有定义落库之前**。反过来的话，
  迁移中途失败就会留下「导入成功但库里没那张表」——这种状态下游每个 Tool 都撞 `no such table`。
- **已应用的版本不重跑**：台账表 `gear_migrations` 建在**目标库**里（与 schema 同生共死）。
  重复导入、重复启动、二次 apply 都不会重跑。
- **每条迁移单事务，失败即整条回滚，并且一个定义都不落库**（fail-closed）。
  错误码 `MIGRATE_SQL_FAILED` 带 `version=N(name)` 与数据库原始错误。
- **`dry_run` 是真跑 + 全回滚**，不是跳过。代价是它只报「将生效的版本」；**MySQL 的 DDL 会隐式
  提交**，那条路上 dry run 保护不住。

`version` 从 1 起**严格递增**（重复 / 断号 → `MIGRATE_INVALID` / `MIGRATE_VERSION_GAP`，
诊断带 `migrations[i]` 字段）。声明了 `migrations:` 却拿不到 db 实现 → `MIGRATE_NO_DB`，
**拒绝整包**而不是静默跳过。

**`config` 是「应用配置落盘」的正解**（2026-09-25 新增）：它读写宿主的配置库（`<GEAR_HOME>/config.db`），
应用**不必知道库在哪**——Lua 沙箱禁 `os` 库、模板无 `${env}` 前缀，脚本根本读不到 `GEAR_HOME`，
过去用 `db` 自己建表只能把绝对路径硬编码进 YAML。现在 `db` 的 dsn 写相对库名（如 `mindstorm.db`）
会锚到 `GEAR_APP_DATA`（默认 `<Home>/data`），无需绝对路径；但**应用设置**仍建议用 `config`
（list 前缀、敏感键脱敏；Go 侧 `Get` 保留 `ErrConfigNotFound` 以区分「没配」与「配了空串」，
供回退链用；Lua 侧 `ctx.config.get` 为取值方便，不存在时平返回空串），别再去 `db` 建一张 settings 表。

**`fs` 是「文件读写」的唯一正道**（2026-11-11 新增，第 8 个受门禁原语）：Lua 沙箱**仍禁 io/os**（不变），
文件能力走宿主侧原语 `ctx.fs.read/write/list/mkdir/remove`。路径全部收口在安全根内：相对路径锚定主根
（默认 `GEAR_APP_DATA`，与 db 相对 dsn 同口径），越界（`..`、根外绝对路径、符号链接/junction 逃逸、删根）
一律报「路径逃出安全目录」——收口在宿主侧每次调用都走，脚本无从绕过。
需要读写默认根之外的业务目录时，用 config 原语配附加根：`fs.roots`（`;` 或换行分隔多个目录，
`gear config set fs.roots "D:/shared/docs"` 或 Config 页配置），**重启生效**——根集合在引擎装配期固定。

**「对外是 REST」靠 Tool 的 `api:` 声明，不靠新建 MCP 工具**（2026-09-28，FR-3）：
Tool YAML 里写 `api: {method: GET, path: /api/nodes/{id}}`，引擎就给它**额外**挂一条资源路由，
路径参数 `{id}` 即 Tool 的输入字段、响应体即 Tool 输出；`POST /api/tools/{id}/run` 照旧可用。

三条口径值得记牢：

- **加挂而非替换**：声明了资源路由的 Tool 两条路都在（AC-C4 的向后兼容红线）。
- **不鉴权**：gear 没有用户库，授权是业务的事（Hook）。资源路由只负责「长得像 REST」。
- **同名 method+path 只能有一个**：net/http 对同名 pattern 的第二次注册是 **panic**（实测 go1.27），
  而注册发生在起服那一刻。所以 `gear_validate` / `gear_apply` 在写作期就拒（`ROUTE_INVALID` /
  `ROUTE_CONFLICT`，后者点名两个 Tool）；跨 bundle 的第二个由 registry 入库前拒。
  `gen-client.mjs` 会为声明了 `api` 的 Tool 生成真 REST 方法（前端与内核同口径）。

**AI 缺省配置也存在同一个库**：`ai.provider` / `ai.model` / `ai.base_url` / `ai.api_key` / `ai.max_tokens`。
写了之后 `ext.ai` 的所有调用自动带上（回退链：调用参数 > 环境变量 > 配置库 > 内置默认），
**不必再给每个进程 export `GEAR_AI_*`**。用 `gear config set ai.api_key ...` 写。

## 3. 纯函数原子的常用方法

| call | 参数（必填加 *） |
|---|---|
| `time.now` | format（rfc3339 等枚举）, tz（如 `Asia/Shanghai`） |
| `time.parse` | value*, format, tz → Unix 秒（数字，便于比较） |
| `time.format` | unix 或 ms（二选一）*, format, tz |
| `time.sleep` | seconds*（0 < s ≤ 60，超限即报错，避免挂死编排） |
| `template.render` | text*, vars（只解析 `${vars.*}`） |
| `crypto.hash` | algo*（md5/sha1/sha256/sha512）, data*, encoding（hex/base64） |
| `crypto.hmac` | algo*, key*, data*, encoding |
| `crypto.hmac_verify` | algo*, key*, data* → bool（恒定时比较） |

`crypto.hmac` 的 key 应经 `ctx.secret.get` 或 `${secrets.x}` 取——**别写明文**。

## 4. 扩展原子 `ext.*`

- 命名是**两级**的：`ext.<name>` 是能力名，方法另算。`ext.ai.complete` = 能力 `ext.ai` + 方法 `complete`。
- 用之前确认三件事：① `gear_capabilities` 里有没有；② `gear_doctor` 里侧车在不在；③ Tool 有没有声明该 `ext.*`。
- `service: true` 的扩展（`ext.serve` / `ext.kafka` / `ext.mqtt` / `ext.rabbitmq` / `ext.webhook` / `ext.filestore` / `ext.cron` / `ext.oidc` / `ext.excel` / `ext.pdf` / `ext.oss`）是**常驻侧车**：
  必须由 `mode: service` 的 step 持有，后续指令 step 用 anchor 排到下一层（见 `gear_guide("service-app")`）。
- 常见方法形态：`ext.serve.start{port*,dir*,host,proxy}` / `ext.serve.stop{port?}` / `ext.email.send{to*,host*,user*,pass*,…}` / `ext.filestore.put{key*,data_base64*,content_type?}` / `ext.cron.add{name*,expr?,at?,callback_url*,payload?}` / `ext.excel.write{path*,sheets*,root?}` / `ext.pdf.write{path*,pages*,root?,font?,font_size?}` /
  `ext.oss.put{key*,data_base64*,endpoint*,region*,bucket*,access_key_id*,access_key_secret*,…}` /
  `ext.oss.presign{key*,method?,expires?,…}`（另含 `get` / `stat` / `list` / `delete`）。
- `ext.serve` 是**多站点**的：同一侧车可同时托管多个网站（不同 port）。`stop` 带 `port` 只停该站
  （多 app 共享侧车时用它，避免误杀别人的站）；省略 `port` 停全部。同端口不同配置再 `start` 会 fail-closed 报错。
- **`ext.filestore`（2026-09-28）**：root 缺省读配置库的 `filestore.root`，没配即 fail-closed；
  key 不许含 `..` / 空段 / 反斜杠，**不跟随符号链接**（含 `delete`——顺链接删会删到 root 外）；
  大小上限默认 32 MiB，超限在落盘前拒；二进制走 base64；`url()` 只回相对路径，不新增内核 REST 端点。
- **`ext.cron`（2026-09-28）**：定时触发，到点 POST `callback_url`（也能作 `ctx.subscribe` 的事件源）。
  **生命周期硬绑定**——进程一停调度立即停止且**不接管**，重启不接回错过的触发（表落盘只用于重建同一张表，
  它不是跨进程任务队列）；幂等键 = name + 触发时刻（分钟粒度）；misfire 默认不补，显式 `catch_up`
  也只补最近一次；overlap 默认跳过；表达式自己解析（5 段 cron + `@` 宏），非法式在注册期就拒。
  查询调度状态的下一步是 `gear_guide("cron-app")`。
- **`ext.oidc`（2026-09-28）**：只做握手——`discovery` / `code_exchange` / `jwks`，**不验签、不做会话**
  （真伪归 `crypto.jwt_verify`，会话归业务）。issuer 混淆两道防线：文档 `issuer` 须与入参一致，
  `token_endpoint` 的 host 须与 issuer 同主机（凭据不能送给别人）。JWKS **长驻缓存**：命中不发 HTTP，
  给 `kid` 且缓存里没有就强制刷新一次，刷新失败退回旧缓存并置 `stale: true`。
  ⚠️ **拿 JWKS 务必把 id_token 头里的 `kid` 交回去**：不给就拿到上次抓的那份，IdP 换过钥匙也照返旧的，
  校验于是发生在旧钥匙上，报出来是「签名不对」。入站回调 `/in/oidc/{label}` 只回报收到了什么，
  不换令牌、不建会话（PRD §4.2）。
- **`ext.excel`（2026-09-29）**：零依赖写出 `.xlsx`——`write{path*, sheets*, root?}`。`sheets` 是
  `[{name, rows}]`（`rows` 二维数组），cell 支持 字符串/数字/布尔/空 四类，**类型由值自身决定，不用标注**。
  `root` 缺省取应用 workdir；`path` 必须是 root 内的相对路径，`..` / 盘符 / 反斜杠一律拒（fail-closed）。
  **只做写出，不做读回**（读回留后批）。
- **`ext.pdf`（2026-09-29）**：零依赖写出 PDF——`write{path*, pages*, root?, font?, font_size?}`，
  零第三方依赖（`go.mod` 一字未动）。CJK 自动**嵌入字体子集**（运行时读系统 TTF、首推 `simhei.ttf`，
  按页面实际用到的字符最小化），故中文不会画成方框且**文本可复制**。
  ⚠️ **字体里没有的字直接报 `font_no_glyph` 失败**，绝不静默降级成方框——换字体（`font` 参数）
  或先把该字符替换掉。A4 版面，超出一页自动分页，字号 4~72（默认 12）。

## 5. 四条最容易写错的

1. **`crypto`/`time`/`template` 不用声明，声明了会被拒**（`GrantsFromStrings` 只认门禁原语与 `ext.*`）。
2. **`json` 不是能力**：Flow 的 `step.call` 写 `json.encode` 一定失败；它只在 Lua 里以 `ctx.json.encode/decode` 出现。
3. **`ext.*` 必须声明**：`capabilities: [ext.serve]` 不能省，否则注册期就报未知 capability。
4. **`ctx.caller` 也不是能力，且不用声明**（2026-09-26 补）：它告诉脚本「这次是谁发起的」
   ——`ctx.caller.subject` / `ctx.caller.app_id` / `ctx.caller.via`。
   **做鉴权的 Hook 靠它判身份**：不匹配就让 Hook `error(...)`，引擎会 fail-closed 终止整次编排。
   注意 `subject` 为空表示「**没验过身份**」，不是「验出来是匿名」——两者要分开处理。
   身份从哪来（2026-09-26 定稿）：
   - **REST 面（`/api/tools/{id}/run`、`POST /api/executions`）是真的**：读 `Authorization: Bearer <主体>`
     或 `X-Gear-Subject: <主体>`，把「谁持有凭据」当成「谁」。带 token 的 REST 才是应用做鉴权该走的口。
   - **MCP 面恒为空**：go-sdk 不把 HTTP 请求交给工具处理器，这里拿不到调用方。
     所以 gear 的 MCP 是**管理平面，不是信任边界**——要用 MCP 构建出来的产品做多用户鉴权，
     必须给产品配一个自己的前门（带 token 的 REST），别指望 gear 的 MCP 替你验身份。
   详见 design 文档 `IDENTITY_CONTEXT_DESIGN.md` §3.5。

## 6. 数组与数字能经 `gear_invoke` 直接传（2026-10-03 实测锁）

`gear_invoke` 的 `args` 走标准 JSON，**数组与数字都原样穿过 MCP → Lua，不用绕道**：

```jsonc
// 这三种都实测通过（REQ-INVOKE-TYPE-KEEP，回归锁在 internal/mcp/invoke_type_keep_test.go）
{"name": "db", "method": "query", "args": {"sql": "SELECT ?", "params": ["A-001"]}}
{"name": "db", "method": "batch", "args": {"statements": [{"sql": "CREATE TABLE t(k)"}]}}
{"name": "crypto", "method": "jwt_sign", "args": {"claims": {...}, "key": "...", "alg": "HS256", "ttl": 3600}}
```

- 数字是 JSON 数字（`3600` 不是 `"3600"`）。传成字符串会得到
  「参数 "ttl" 必须是数字，实际 string」——**那是调用方自己传错了类型**，不是桥接层压平的。
- 数组元素按位置读；`statements` 元素可写 `{"sql":..., "params":[...]}` 或裸 SQL 字符串（无参 DDL）。
- 空数组 `[]` 也能传，但**回程**里空结果与空对象都可能是 `{}`（Lua table 无类型之分的固有边界，
  已在 `internal/engine/lua.go` 的 `luaArrayMarker` 注释里写明）。

⚠ 若你看到「必须是数字，实际 string」或「缺少必填参数 statements」这类报错，
**先核自己传的 JSON 类型与形态**（`gear_capabilities` 看 schema + 本文 §1.1 看形态），
不要归因成「gear 的 MCP 压平了类型」——2026-10-03 已端到端核实过那是误判。

## 7. 缺能力时怎么办

**不许凑**。不允许：
- ❌ 用 `process` runtime 里 `sh -c` / `cmd /c` 调外部程序绕开能力门禁；
- ❌ 自造 `ext.xxx` 名字（未注册的能力，声明了也被拒，还污染口径）；
- ❌ 读环境变量取凭据（机械守卫会拒）。

要按 `gear_guide("primitive-gap")` 的模板给出**基础、通用、无业务名词**的原语建议，并说清哪些步骤被阻塞。
