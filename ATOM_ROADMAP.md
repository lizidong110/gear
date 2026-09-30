# Gear 原子能力进化路线图（核心 / 可扩展分期）

> 目标：把 Gear 做成「万能场景基础支持平台」。原子能力尽量偏向原始能力（raw primitives），
> 用 Tool 承载基础业务，用 Flow 编排业务。本文档是**能力扩充的排期基线**——本期已落地
> 内置 `db` 多后端，可扩展原子按重要程度分 Phase 1–4 逐个补充（不一次性塞进内核）。

---

## 0. 三类原子能力（分类基线，单一真相源）

| 类别 | 定义 | 是否进 capability 门禁 | 示例 |
|---|---|---|---|
| **核心原子** | 内核内建、协议/产品无关、无业务语义的通用计算/IO 原语 | 是（`KnownPrimitives`） | `http` `call` `db` `secret` `event` `subscribe` `config` |
| **纯函数核心原子** | 内核内建、始终可用、无外部依赖的纯计算工具（不应进门禁，否则违背纯函数直觉） | 否（文档化口径） | `time` `template` `crypto`（2026-09-19 落地，一等调用目标）；`json`（Lua 绑定，见下方说明） |
| **可扩展原子** | 桥接某具体外部系统/协议的单操作连接器，以 `ext.*` 插拔 | 是（`ext.` 前缀 + 显式声明） | `ext.email` `ext.wecom` `ext.rabbitmq` … |

**判定三问**：通用计算/基础 IO、不绑具体产品 → 核心；桥接具体外部系统/协议、仅暴露原生单操作 → 可扩展；含业务规则/多步组合/条件/领域语义 → **不是原子**（是领域工具 Lua 组合或 Flow）。

> **2026-09-25 新增 `config`（第 7 个受门禁原语）**：持久化键值配置，落点由**宿主**决定（`<GEAR_HOME>/config.db`）。
> 它进门禁的理由与 db 相同（跨越宿主边界、需显式授权）；它不是 db 的替代品——`db` 面向「任意外部库的结构化读写」，
> `config` 面向「这一套 gear 自己的配置」。新增它的直接动因：Lua 沙箱禁 `os` 库、模板无 `${env}` 前缀，
> 应用想用 `db` 存配置只能把绝对路径硬编码进 YAML（mindstorm 曾有 18 处 `local DSN = "D:/…/mindstorm.db"`）。
> 内核 AI 缺省配置（`ai.*`）也读同一个库，使「配一次、所有进程生效」成立。

> 红线（与 GEAR 工程方法一致）：每个 `ext.*` 必须 `ext.` 前缀、必声明 `schemas`、methods 与 schemas 一一对应，否则启动 fail-closed；调用遵循最小权限；未知枚举即拒（不静默）。

> **口径澄清（`json` 与三个新纯函数原子的差异，别说成一样）**：
> - `time` / `template` / `crypto` 是**一等调用目标**——进 `builtinCapabilitySchemas`（方法级 schema 与实现同文件
>   `internal/engine/purefunc.go`），因此 Flow step 可写 `call: crypto.hash`、能力目录（CLI `ai capabilities` /
>   `ext.registry.capabilities` / REST / MCP）列为 `kind=pure`、Lua 侧无条件注入 `ctx.time.*` 等，且注册期即静态校验。
> - `json` 目前**只是 Lua 便利绑定**（`ctx.json.encode/decode`，`lua.go` 无条件注入）：不在能力目录、**不能**作 Flow 直调目标。
>   它用的是位置参数（`ctx.json.decode(str)`），若按新原子口径改成单具名 table 会破坏所有既有 Lua 工具 → 属破坏性变更，
>   与下方「方法命名统一」同类，**暂缓**（见 §3）。文档口径统一为「纯函数原子」，但落地形态的差异在此明说，避免又一处「文档说有、实际没有」。

---

## 1. 本期已落地：内置 `db` 多后端（sqlite / mysql / postgres）

- `cap.DB` 接口升级为 `Query/Exec/Ping(driver, dsn, query, args...)`；`internal/cap/sqldb.go`
  由单 DSN `SQLiteDB` 改为 `MultiDB` 路由器（按 `driver/dsn` 懒开并缓存连接）。
- `driver` 别名：`sqlite/sqlite3`→modernc；`mysql/mariadb`→go-sql-driver；`postgres/postgresql/pg`→pgx。
  三者驱动已在 `go.mod`，零新依赖。
- Lua 调用：`ctx.db.query({driver, dsn, sql, params})` / `exec` / `batch` / `ping`；`dsn` 缺省回退引擎内存 scratch 库。
  `batch` 于 2026-09-28 补齐（FR-2）：同事务执行多条语句、全或无，返回 `{results, rows_affected, committed}`。
- 验证：`internal/cap` 新增 `TestMultiDBBackendsRouting`——真实 SQLite 文件跨实例落库读回、
  mysql/postgres/mariadb 驱动已链接（Ping 连不上即报错，非「驱动未注册」）、未知 driver 被拒。
- 同批补齐的还有 `MultiDB.Tx(driver, dsn, fn)`（单事务闭包，失败即回滚）：迁移需要「一整段
  可能含多条语句的 SQL」原子执行，而 `Batch` 只能吃已拆好的语句数组——两者不是替代关系。
- 影响：原 `ext.db` sidecar 已被取代，仅作「自定义 sidecar 承载 DB」参考保留（`extensions.example.yaml` 已标注）。

## 1.5 本期已落地：bundle 迁移（`kind: Migrations`，2026-09-28）

- 声明位置：bundle 里一份 `kind: Migrations` 文档，条目 `{version, name, sql}`，目标库由
  `driver` + `dsn`（相对库名锚 `GEAR_APP_DATA`，与 `ctx.db.*` 同口径）指定。**不进 registry**：
  迁移是副作用，不是定义。
- 执行时机：`gear_apply` / `gear_import` 里**先改库、再落定义**。顺序不是审美问题——先落定义
  再迁移，中途失败就会留下「导入成功但库里没那张表」的合法状态，下游每个 Tool 都撞
  `no such table`。
- 三条口径：已应用的版本不重跑（台账 `gear_migrations` 在目标库里）；每条迁移单事务、失败即整条
  回滚；`dry_run` 真跑 + 全回滚（坏 SQL 在写作期被抓出），但只报「将生效」不报「已生效」。
- 验证：`internal/artifact/migrate_test.go` 七条（含「坏迁移时新 Tool 不得落库」「dry run 不留痕」）。

## 1.6 本期已落地：Tool 声明式 REST 路由（`api:`，2026-09-28）

- 声明位置：Tool YAML 的可选字段 `api: {method, path}`（`path` 支持 `{id}` 这类路径参数，
  **名字即 Tool 的输入字段**）。`POST/PUT/PATCH` 还会把 JSON 请求体并入输入，路径参数优先。
- 语义：声明了就**额外**挂一条资源路由，`GET /api/nodes/42` 的响应体就是该 Tool 的输出
  （不包 `{output}`）；`POST /api/tools/{id}/run` 同时仍可用。未声明的 Tool 零变化。
- **不鉴权**：gear 没有用户库（Q1 拍板：授权是业务的事，走 Hook）。资源路由只保证「长得像 REST」。
- 为什么路由校验必须前移到写作期：net/http 对同名 pattern 的第二次注册是 **panic**（实测 go1.27），
  而注册发生在起服那一刻——一个 YAML 字段就能让整个服务起不来。故 `ROUTE_INVALID`（形状）与
  `ROUTE_CONFLICT`（同名，诊断点名两个 Tool）在 `gear_validate` / `gear_apply`（含 dry_run）里整包拒绝，
  跨 bundle 的第二个由 registry 入库前拒。
- 挂载方式：每方法一条 `METHOD /api/{path...}` 通配挂载点 + 请求期按段匹配（而不是「每条声明注册一条」）。
  代价是拿路径参数要自己按段取（`r.PathValue` 只认挂载点那个 `path`），换来的是「改声明、删 Tool、
  运行期新建 Tool」都不必重启、也不会 panic。
- 验证：`internal/server/toolapi_test.go` 五条（含「旧端点仍可用」「通配挂载点不抢内核路由」）、
  `internal/artifact/routes_test.go` 四条、`internal/model/route_test.go` 三条、
  `internal/mcp/routes_test.go` 一条（接口面：字段级诊断 + apply 拒写）。

---

## 2. 可扩展原子分期路线图（按重要程度）

排序依据：**跨场景普适性 × 解锁业务逻辑的杠杆 × 出现频率**。越靠前越「每个真实业务系统都需要」。

### Phase 1 — 基础设施连接器（几乎每个真实系统必备，优先做）
> ✅ `ext.email` / `ext.webhook` / `ext.wecom` 已于 2026-09-17 落地，**并以 Go 单二进制载体 `gear ext <name>` 实现**
> （取代原先 Python sidecar——免去 pip 装镜像包、分发带解释器的痛点；引擎零改动，仅 `extensions.yaml`
> 的 `cmd` 切到 `["build/gear.exe","ext",<name>]`）。`plugin/verify.py` 端到端 **24/24 全绿**（2026-09-19）：
> webhook 真实接收 / webhook.subscribe 监听入站 URL 回推 / email log 落盘 / email read·list（IMAP）/
> wecom 失败 fail-closed / Gear 链路跑通 / rabbitmq·kafka 无 broker fail-closed / sms 真实 POST /
> **consume 直接调用（pull/push）** / 红线校验（含 `/caps` 声明↔实现）/ 注册表含 6 ext。
>
> **2026-09-18 架构升级：webhook / rabbitmq / kafka 已是「常驻 service ext」**——`extensions.yaml` 声明
> `service:true` + `listen`（本地端口）+ `source` + `health_probe`；引擎把它们包成虚拟 process Tool、注册 `coord.Client`、
> 能力调用走 `LocalHTTPTransport`（POST `/{method}`，body `{"args":...}`，复用长连接）。`listen`/`consume` 经通用订阅原语
> `ctx.subscribe` 回推事件（与 `ext.mqtt` 同机制）。email/wecom/sms 仍是一次型（once）原子，保持原 stdin/stdout 验证。
>
> **2026-09-19 P2 补全（consume 直接可调，不再只经 ctx.subscribe 一条特例）**：
> - `ctx.ext.kafka.consume` / `ctx.ext.rabbitmq.consume` 现在是**一等方法、直接可调**，`mode` 双形态：
>   `pull`（默认，拉取最多 `max_messages` 条同步返回）/ `push`（长驻订阅、事件回推）。
> - `pull` 语义按真实场景做：Kafka **先预检 broker 连通性**（连不上 fail-closed，避免与「暂无消息」混淆）、
>   **本批成功返回后才提交 offset**；RabbitMQ 取走后 ack。
> - `push` 修掉丢消息：RabbitMQ **仅回推成功才 ack**，失败 `nack` 重入队；Kafka 保留原语义。
> - 新增 `unsubscribe`（按 handle 停长驻订阅），长驻能力形成 start/stop 闭环。
> - 新增侧车 `/caps` 端点 + verify.py 断言「`extensions.yaml` 声明的方法 ⊆ 侧车实现」，堵住
>   「yaml 能声明一个侧车根本没实现的方法」的缺口。
> - 说明：`internal/ext/*.go`（如 `internal/ext/kafka.go` 只有 `produce`）是**非 service（once/CommandTransport）路径**的
>   handler；service ext 声明了 `listen`，能力调用全走侧车（`internal/engine/extensions.go`），故进程内 handler 不参与——不是缺口，但易误读。
>
> **2026-09-21 协调地址收敛为单一真相源（`listen`）**：service ext 的 `listen` 此前有两份独立定义——
> yaml 的 `listen`（引擎用它去连）与侧车 `os.Getenv("COORD_ADDR")` 的默认值（侧车用它 bind），
> 改任意一处都变成「引擎连 A、侧车听 B」的静默失联。现在引擎在拉起侧车时按 `listen` 注入
> `COORD_ADDR`（侧车默认值降级为手工单跑兜底），且 `health_probe.addr` 省略即取 `listen`、
> 显式不一致则装配期 fail-closed（探错地址 → 监管无限重启）。实现：
> `internal/engine/extensions.go:prepareServiceExt`；守卫：`extensions_test.go` 6 例（含探针一致性）；
> 权威/示例 yaml 的 service ext 段已不再写 probe 的 `addr`。

| 原子 | 桥接 | 状态 | 验收标 |
|---|---|---|---|
| `ext.email` | SMTP（含 TLS） | ✅ 已落地 | `ctx.ext.email.send({to,subject,body})` 真实发信（SSL/STARTTLS，dev `log` 后端落盘自测）；`ctx.ext.email.read` / `ctx.ext.email.list`（IMAP 一次型，连取即关）补齐收件能力 |
| `ext.webhook` | 任意 HTTP 回调 | ✅ 已落地 | `ctx.ext.webhook.send({url, payload, headers})` 真实 POST JSON；`ctx.ext.webhook.subscribe({path, callback_url})` 经常驻侧车监听入站 URL 并回推事件 |
| `ext.wecom` | 企业微信（群机器人） | ✅ 已落地 | `ctx.ext.wecom.send({key, content, msgtype})` text/markdown |
| `ext.ai`（即 LLM 原子） | 大模型（进程内 Eino agent） | ✅ 内核已内建 | **无需新建 `ext.llm`**——`ext.ai` 始终注册，`ctx.ext.ai.complete/catalog/validate/generate/capabilities/react` 直接可用 |

### Phase 2 — 事件/异步骨架与高频通知
| 原子 | 桥接 | 理由 | 验收标 |
|---|---|---|---|
| `ext.rabbitmq` | AMQP 消息队列 | 接入企业 MQ，支撑异步/削峰编排 | `publish`/`declare`/`consume`/`unsubscribe`。consume 直接可调：`mode=pull` 拉一批返回（取出后 ack）/ `mode=push` 长驻订阅，**仅回推成功才 ack**（失败 nack 重入队） |
| `ext.kafka` | Kafka | 同上，日志/事件流主流 | `produce`/`consume`/`unsubscribe`。consume 直接可调：`mode=pull` 拉一批返回（**broker 先预检 fail-closed**、成功返回后才提交 offset）/ `mode=push` 长驻订阅回推 |
| `ext.slack` | Slack | 国际团队/DevOps 通知 | `send({channel, text})` |
| `ext.sms` | 短信网关 | 关键告警通道 | `send({phone, text})` 接至少一家网关 |

### Phase 3 — 数据/文件/身份/检索
| 原子 | 桥接 | 理由 | 验收标 |
|---|---|---|---|
| `ext.serve` | 静态网站 + 反向代理（类 nginx） | 给定一个端口 + 静态目录，起 HTTP 服务即可在浏览器访问；带 `proxy` 规则时把某些前缀（如 `/api`）反向代理到后端，**静态与接口同源**（dev 下 Vite 代理的生产等价物） | `start({port,dir,host,proxy})` 起服并返回 URL（含生效规则回显）/ `stop()` 释放端口；`proxy=[{path,target,strip_prefix}]` 最长前缀优先、`path` 非法或 `target` 非 http(s) 则 fail-closed、后端不可达返回 502；目录不存在、缺参 fail-closed；`plugin/verify.py` 已覆盖起服→访问→停止 + 反代转发/静态回落/非法规则拒绝 |
| **`ext.filestore`**（2026-09-28 已交付） | 本地文件系统（附件存取） | 附件/文件类业务（合同扫描件、头像）的**第一步**：先解决「路径不硬编码 + 路径穿越 + 大小上限」这三条安全基线；对象存储留给下一批 | `put({key,data_base64,content_type?})` / `get` / `stat` / `list({prefix?})` / `delete` / `url`。root 取调用参数或配置库 `filestore.root`，都没有即 fail-closed；**不跟随符号链接**、超限在落盘前拒；`plugin/verify.py` P1b 三项覆盖 |
| **`ext.cron`**（2026-09-28 已交付） | 本地定时器（分钟级） | 定时任务（对账、日报、过期清理、定时导出）的**第一步**。刻意先做「零依赖 + 硬绑定」版：先把「时间表落在哪、进程死了算什么、重复触发如何去重」这三条问清楚，再谈分布式调度 | `add({name,expr,callback_url*,payload?,catch_up?,overlap?})` / `at({name,at,callback_url*})` / `list` / `cancel({name})`；**生命周期硬绑定**（进程退出即停且不接管，重启不接回错过的触发）；去重键 = name + 触发时刻（分钟粒度）；misfire 默认不补、`catch_up` 也只补最近一次；overlap 默认跳过；5 段 cron + `@` 宏；`plugin/verify.py` P1c 覆盖（含真 kill 进程证「到点无回推」） |
| **`ext.oidc`**（2026-09-28 已交付） | 企业身份（握手） | 单点登录的**第一步**：只把「发现元数据 / 授权码换令牌 / 取 JWKS」这三件事做对并防住 issuer 混淆，不碰验签与会话 | `discovery({issuer*})` / `code_exchange({issuer*,code*,client_id*,redirect_uri*,client_secret?,code_verifier?})` / `jwks({issuer*,kid?})`。**只做握手**：不验签（归 `crypto.jwt_verify`）、不做会话；issuer 混淆两道防线（文档 issuer 须一致、`token_endpoint` 的 host 须同主机）；JWKS 长驻缓存（命中不发 HTTP、未知 `kid` 强制刷新、刷新失败退回旧缓存并置 `stale: true`）；授权服务器拒收时**不吐任何令牌字段**；入站 `/in/oidc/{label}` 只回报。样例 Tool `plugin/oidc_demo.yaml` 跑通 discovery→code_exchange→jwks→jwt_verify；`plugin/verify.py` P1d 覆盖（含反向验证：aud 写错即失败、换假钥匙报 `crypto/rsa: verification error`） |
| **`ext.excel` / `ext.pdf`**（2026-09-29 已交付） | 办公文档写出（报表导出） | 管理系统导出报表/对账单是标配；零依赖手写 OOXML + 手写 TTF 子集（CJK 嵌入），不引入第三方库（`go.mod` 零变动，AC-E4-4） | `excel.write({path, sheets:[{name, rows:[[cell]]}], root?})` / `pdf.write({path, pages:[{text}], root?})`；cell 支持 字符串/数字/布尔/空；root 取调用参数或配置库、越界 fail-closed（同 outpath 囚禁）；PDF CJK 缺字即 `font_no_glyph` 报错、绝不画方框；`plugin/verify.py` P1e 覆盖（openpyxl 独立读回交叉验证 / 缺字 fail-closed / 越界 fail-closed）；样例 Tool `plugin/report_demo.yaml` 经内核 register+run 跑通两条链路（只直连侧车不算数） |
| **`ext.oss`**（2026-09-30 已交付） | 对象存储（S3 兼容） | 附件/文件类业务（合同/头像/导出）要落到**企业已有的**对象存储，而不是 gear 自己的本地仓库（后者是 `ext.filestore`）。只做 S3 协议一套：阿里云 OSS / 腾讯云 COS / MinIO / R2 / Ceph 靠 `endpoint` 区分，不引 SDK | `put({key*,data_base64*,endpoint*,region*,bucket*,ak*,sk*,content_type?})` / `get` / `stat` / `list({prefix?,limit?})` / `delete` / `presign({key*,method?,expires?})`。**手写 AWS SigV4**（`crypto/hmac`+`crypto/sha256`，不引 `aws-sdk-go`，`go.mod` 零变动）；凭据只从入参进、**无配置库回退**（与 filestore 有意不同）；key 禁 `..`/空段/前导 `/`/反斜杠（远端无符号链接语义，**不走 outpath**）；32 MiB 上限在解 base64 **之前**判；大对象一律 `presign` 直传（内核调侧车硬超时 10s）；`path_style` 自动判（IP 即 path-style）。签名正确性：假 S3 端点从收到的原始请求反推 canonical request 重算比对 + botocore 与 aws4 两个独立实现的黄金向量；`plugin/verify.py` P1f 覆盖六方法往返与 fail-closed；样例 Tool `plugin/oss_demo.yaml` 经内核 register+run 跑通 |
| **`ext.redis`**（2026-09-30 已交付） | Redis 数据结构 + 分布式锁 + 延迟队列 | 企业已有 Redis 的缓存/队列/分布式锁场景；零依赖手写 RESP2（不引 `go-redis`，保住 `go.mod` 零变动） | KV（`set/get/del/expire/ttl/exists/incr`）+ Hash（`hset/hget/hgetall/hdel`）+ List（`lpush/rpush/lpop/rpop/llen/lrange`）+ Set（`sadd/smembers/srem/scard`）+ ZSet（`zadd/zrange/zscore/zrem`）+ 分布式锁（`lock/unlock`，`SET NX PX` + 安全释放）+ 延迟队列（`enqueue/dequeue`，基于 zset） | **单连接串行即原子**：redis 单线程 + 侧车借一个连接连发多条命令，`GET+DEL` / `ZRANGEBYSCORE+ZREM` 不用 Lua 也天然原子（假端点同样成立）；key 禁 `\0/\n/\r`、限长 2048；`password` 无则不带 AUTH；内核调侧车硬超时 10s | `plugin/verify.py` P1g 覆盖全数据结构 + 锁 + 队列真实往返与 fail-closed；样例 Tool `plugin/redis_demo.yaml` 经内核 register+run 跑通；协议正确性由 `main_test.go` 内存假 Redis 把关 |
| `ext.ldap` / `ext.oauth` / `ext.sso` | 企业身份 | 对接现有账号体系、做审批人校验 | 查用户/校验令牌 |
| `ext.embedding` / `ext.vector` | 向量库 | RAG/语义检索，配合 `ext.llm` | embed / upsert / search |

### Phase 4 — 长尾行业连接器（按需，不阻塞平台化）
| 原子 | 桥接 | 备注 |
|---|---|---|
| `ext.exchange` / `ext.google-calendar` | 日历 | 会议/排期类场景 |
| `ext.stripe` / `ext.alipay` / `ext.wechat-pay` | 支付 | 交易/订阅类场景 |
| `ext.sheet` / `ext.docx` | 办公文档（`ext.excel` / `ext.pdf` 写出已交付，见 Phase 3；解析类仍待补） | 报表生成/解析类场景 |
| `ext.redis-stream` | Redis Stream | 轻量事件流替代 |

**每补一个 Phase 原子的动作清单**（统一流程，避免重蹈 `ext.oa` 覆辙）：
1. 在 `extensions.example.yaml` 加 `ext.<x>` 段（cmd/methods/schemas 必填）；
2. sidecar 用任意语言实现 stdin/stdout JSON 协议，错误真实 `{"error":...}`；
3. 在 `builtinCapabilitySchemas` 之外——`ext.*` 的方法 schema 走 `capReg`（启动期收集，
   经 `applyExtSchemas` 转存进 `CapabilityRegistry.schemas`），确保 `checkCapabilityMethod` 与
   `validatePrimitiveArgs` 能**在注册期**做方法级静态校验（**此缺口已修，2026-09-19**：
   此前 ext 只校验能力名、方法名与参数要到运行期才炸；现与内置原语同源判定，
   由 `TestExtMethodLevelStaticValidation` 锁死；无 `schemas` 的 ext 退化为名字级校验，是显式行为而非静默放过）；
4. 加一个引用该原子的示例 Flow + 单测（含 fail-closed 路径）。

---

## 3. 核心原子：已补 / 待补

**已落地（2026-09-19）**：

- **`time` / `template` / `crypto`**——内核纯函数原子，实现与 schema 同文件
  （`internal/engine/purefunc.go`，`callPureFunc` 是唯一调度点：Lua 绑定与 Flow 直调都落它，
  故「目录声明的方法」与「真能跑的方法」由构造保证一致）。要点：
  - 单具名 table 入参（`ctx.time.now({ format = "date" })`）——**刻意不用位置参数**：
    位置参数要在 `ai_agent.go` 的 `builtinParamOrder` 再存一份「顺序」副本，两份清单必然漂移
    （`subscribe` 曾因漏登记而缺席能力目录，就是这个坑）；
  - 实现自带入参校验（Lua 直调不经过 `validatePrimitiveArgs`，实现即最后一道闸）：缺必填 / 未知枚举 /
    非法长度一律报错，**不静默兜底**（`hmacSum` 初版曾对未知 algo 回退 sha256，已改为显式报错）；
  - 内嵌 `time/tzdata`（约 450KB），使 `tz="Asia/Shanghai"` 不依赖宿主是否装了 zoneinfo——
    避免「环境决定语义」；
  - `crypto` 密钥由调用方传入（`ctx.secret.get` / `${secrets.x}`），原子本身不读 env、不自带口令。
  - 能力目录里以 `kind=pure` 与受门禁的 `kind=builtin` 区分；**不**登记进 `cap.KnownPrimitives`，
    因此把它写进 `Tool.Capabilities` 会被 `GrantsFromStrings` 拒绝（口径由 `TestPurePrimitivesNotGated` 锁死）。
- **`json`**：口径统一为「纯函数核心原子（始终可用）」，但落地形态仍是 **Lua-only 便利绑定**——
  详见 §0 的口径澄清。若要把它提升为与上面三者同款的一等调用目标（进目录 + 可 Flow 直调），
  需接受「入参改单具名 table」的破坏性变更，与「方法命名统一」一并排期。

**仍暂缓**：

- **方法命名统一**（`http.Request` PascalCase vs `db.exec` snake_case）：已接受但**暂缓**——
  改名跨 `cap` 接口 + 多 runtime + 会破坏 `news-top10`/`news-demo` 示例，属破坏性变更，
  需单独迁移方案（建议：保留 `http.Request` 别名、新写法统一 snake_case，文档化而非强改）。

---

## 4. 平台化达标判据（可对照验收）

- [x] 内置 `db` 支持 sqlite/mysql/postgres（零改业务代码即可换库）
- [x] 可扩展原子 P1 三件套（`ext.email`/`ext.webhook`/`ext.wecom`）已落地样板（`plugin/`，`plugin/verify.py` 全量通过）
- [x] 可扩展原子 P2 三件套（`ext.rabbitmq`/`ext.kafka`/`ext.sms`，不含 slack）已落地；consume 为「直接可调」的一等方法（pull/push 双模，修掉「回推失败仍 ack」丢消息与「broker 挂掉假成功」）；`plugin/verify.py` 24/24 全量通过
- [x] 服务型 ext「声明 ↔ 实现」红线：侧车 `/caps` 端点 + `plugin/verify.py` 断言 `extensions.yaml` 的 methods ⊆ 侧车实现
- [x] 每个扩展独立文件夹归入 `plugin/`，统一注册入口 `plugin/extensions.yaml`（目录约定生效）
- [x] LLM 原子确认为内核内置 `ext.ai`（无需 `ext.llm` 重复建设）
- [x] 可扩展原子 P3 之文件类已落地：`ext.filestore`（2026-09-28，第二批 E2；`plugin/verify.py` P1b）
- [x] 可扩展原子 P3 之调度类已落地：`ext.cron`（2026-09-28，第二批 E3；`plugin/verify.py` P1c）。
  生命周期口径按 2026-09-28 拍板**修正为硬绑定**（初稿的「重启后按 misfire 处理」作废）：
  进程退出即停且不接管，只有「有进程在跑 / 没进程」两态；cron 表落盘只用于重建同一张时间表，
  **不是跨进程任务队列**
- [x] 可扩展原子 P3 之身份类已落地：`ext.oidc`（2026-09-28，第二批 E1；`plugin/verify.py` P1d）。
  只做握手不验签不建会话；issuer 混淆两道防线；JWKS 长驻缓存 + 未知 kid 强制刷新 + 刷新失败置 stale
- [x] 可扩展原子 P3 之对象存储已落地：`ext.oss`（2026-09-30，第三批 E5；`plugin/verify.py` P1f，手写 SigV4，`go.mod` 零变动）
- [x] 可扩展原子 P3 之 Redis 已落地：`ext.redis`（2026-09-30，第三批 E5；`plugin/verify.py` P1g，手写 RESP2 零依赖，KV/Hash/List/Set/ZSet + 分布式锁 + 延迟队列 29 方法）
- [ ] 可扩展原子 P3–P4 逐个落地（向量 → 日历/支付/文档/IM）
- [x] `ext.*` 方法级 schema 进入 step 直调静态校验（2026-09-19 修掉红线冲突缺口：`CapabilityRegistry.schemas` + `applyExtSchemas`，注册期即拒未知方法/未知参数，`validatePrimitiveArgs` 与 `InvokeCapability` 同源）
- [x] 核心纯函数原子（time/template/crypto）补齐（2026-09-19：`internal/engine/purefunc.go`，`kind=pure`、Lua 无条件注入、Flow 可 `call: crypto.hash` 直调、注册期静态校验）
- [x] **纯函数族扩容至 5 个**（2026-09-28）：新增族 `id`（uuid/nanoid/code）与 `validate`（json_schema），
  并把 JWT 三件套（`crypto.jwt_decode/verify/sign`）+ 口令哈希（`crypto.password_hash/verify`）补进 `crypto` 族。
  零新增依赖（jsonschema 与 uuid 本就在 `go.mod`）。**业务价值**：企业应用最常见的「前端拿 IdP 的 id_token 直接验签」
  这条路径根本不需要侧车（见 `ENTERPRISE_PRIMITIVE_GAP.md` §3.5）。口径锁：`internal/mcp/mcp_test.go`
  的 `TestMCPCapabilities*` 已把 5 个族一并锁住。
- [ ] 新增任意场景 = 拼 Flow（最多写几个 Lua 薄工具），**不改任何 Go 二进制**
