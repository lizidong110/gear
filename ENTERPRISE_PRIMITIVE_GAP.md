# 企业常规应用场景：Gear 原语缺口梳理

> 状态：**提案/梳理**，未落地。本文不改动任何现有口径，也不改动 `ATOM_ROADMAP.md` 的排期基线；
> 它的作用是给路线图补一个「企业应用」视角的排序依据——回答「为了能顺手写企业应用，还差哪些原语」。
> 现状依据：`internal/cap/cap.go`（门禁常量）、`internal/engine/lua.go`（Lua 绑定）、
> `internal/engine/purefunc.go`（纯函数）、`plugin/extensions.yaml`（ext.* 权威 schema）、
> `ATOM_ROADMAP.md`（分期基线）。
>
> 梳理日期：2026-09-28

---

## 0. 一句话结论

**Gear 现在不缺「连接器」，缺的是「应用运行时地基」——但地基里真正必须进内核的只有两件半。**

已落地的能力是**自动化编排**视角的：能调外部系统、能连数据库、能发通知、能编排流程。
但企业常规应用（内部管理系统、CRM/OA/工单/审批/数据看板）需要的是另一套东西：
**身份与权限、事务性数据访问、文件存储、定时任务、审计**。

> **第三版修正（2026-09-28）**：第一版把这五件一律判成「必须进内核」，**判重了**。
> 复核后：**只有 `db` 事务与「身份会话收敛」非内核不可，其余都能做成 ext 侧车**
> （`blob`→`ext.filestore`、`schedule`→`ext.cron`、JWT/OIDC→`ext.oidc`、锁与队列→`ext.redis`）。
> 见 §3 开头的形态总表；判据修正见 §2（「Lua 沙箱摸不到」这条作废）。
>
> **第四版修正（2026-09-28）**：第三版把「非内核不可 vs 做 ext」当成二选一，**漏了最便宜的第三选项——纯算法族**。
> `jwt` 验签、`validate`、`id` 这类**标准冻结、无重依赖、无跨调用状态**的计算，应当直接进 `purefunc.go`
> （kind=pure），**比 ext 侧车还便宜**（不动 `cap.go`、不动 `lua.go`、不新增进程）。
> 归类用四轴判据（**A** 副作用 / **B** 跨调用状态 / **C** 依赖成本 / **D** 稳定性），见 **§3.7**。
> 连带修正：`jwt` 从「全归 `ext.oidc`」拆成「**验签归 `crypto`、握手归 `ext.oidc`**」。

次要结论：还有一批能力**不该做成原语**（审批语义、动态表单、报表 DSL），
把它们塞进内核会把 Gear 推成低代码平台，违背 `ATOM_ROADMAP.md` §0 的「判定三问」。

---

## 1. 先把「企业应用」拆成 10 件具体的事

不看抽象名词，看企业应用每天都在做的动作：

| # | 企业应用在做什么 | Gear 现状 | 缺口性质 |
|---|---|---|---|
| 1 | 用户登录、记住会话、按角色看到不同菜单/按钮 | 只有 `ctx.caller.{subject,app_id,via}`（`lua.go:273`），Bearer/`X-Gear-Subject` 注入，**无用户表、无会话、无权限模型**，鉴权靠每个 Hook 自己写 Lua | **内核** |
| 2 | 表单 CRUD：列表分页、条件筛选、模糊搜索 | `ctx.db.query/exec/ping`（`lua.go:429`），**无事务、无分页、无迁移**；mindstorm 自建 `search_cache` 表 + `LIKE` | **内核** |
| 3 | 上传附件（合同/头像/发票）、下载、导出 | Lua 沙箱禁 `os` 库，写不了文件；只有 `ext.serve` 静态托管（不是存储） | **内核** |
| 4 | 审批流转（提交/驳回/撤回） | Flow 有，**无 loop 字段、遇环即拒**，人工审批节点/待办需应用自造 | 应用层 |
| 5 | 导出 Excel / PDF 报表 | 无。`ATOM_ROADMAP` 把它排在 P4 | ext（应上调） |
| 6 | 定时任务：日报、对账、超时自动关单 | **完全没有调度能力**（`internal/` 下无 cron/schedule 实现） | **内核** |
| 7 | 通知：邮件/短信/企微 | ✅ `ext.email` / `ext.sms` / `ext.wecom` 已落地 | 已具备 |
| 8 | 对接外部系统（ERP/财务/钉钉/企微） | ✅ `http` + MQ 侧车；国内 IM（钉钉/飞书）缺 | ext |
| 9 | 审计：谁在什么时候改了什么 | registry 有 `deleteWithSnapshot`（定义层留痕），**业务数据层无审计**；execs 有历史但无 actor 过滤 | **内核** |
| 10 | 多租户 / 数据隔离 | 只有 App 命名空间前缀（`model.App.Owns`），无行级数据权限 | 待决策 |

---

## 2. 判定原则（决定「进内核 / 做 ext / 不做」）

沿用 `ATOM_ROADMAP.md` §0 的判定三问，补两条更硬的判据：

**先进纯算法族（不受门禁，最优先）—— 四轴全过才进（2026-09-28 第四版新增）：**

- **A** 不碰需授权的资源（db / 文件 / 网络 / 密钥库 / env）；
- **B** 不持跨调用状态（无句柄、无会话、无长驻缓存）；
- **C** 标准库、或**已经在 go.mod 里**的依赖就能做；
- **D** 冻结标准（RFC）而非厂商实现。

⇒ 四轴全过即进 `purefunc.go`（kind=pure）：只改 5 处、零新进程、Lua 绑定自动生成，
**比 ext 侧车还便宜**。完整归类与全表见 **§3.7**。
（口径澄清：时钟与熵算「环境事实」不算「资源」——`time.now` / `crypto.random` 已在族内，
故 `jwt.verify` 校验 `exp` 依赖时钟，**不因此被踢出纯算法**。）

**进内核原语（受门禁）当且仅当：**
- 跨宿主边界（读写宿主磁盘/进程外资源）→ 需显式授权，如 `db` `config` `secret`；**或**
- 需要引擎保证语义一致性（事务、锁、调度、幂等），ext 侧车给不了；**或**
- 被鉴权链路依赖（身份必须内核统一，否则每个应用一套）；**或**
- ~~Lua 沙箱摸不到（禁 `os` 库 → 文件操作只能内核给）~~ —— **此条作废（2026-09-28）**：
  Lua 沙箱禁 `os` 不假，但 **ext 侧车是独立进程，不受沙箱限制**（`ext.serve` 起得 HTTP 服务、
  `ext.email` 连得上 SMTP，都是明证）。所以「Lua 干不了」**不等于**「必须进内核」，
  正确问法是「侧车能不能干」——文件、定时、协议对接，侧车都能干。
- 被**内核代码路径**依赖（如 `callerOf`（`identity.go:40`）解析 Bearer 只能内核做）。

**做 ext 侧车：** 桥接某个具体外部产品/协议，且是幂等单操作 → 对象存储、LDAP/OIDC、ES/Meili、Excel/PDF、支付、钉钉/飞书。

**不做：** 含业务语义/多步组合/领域规则 → 审批语义、动态表单、报表 DSL、业务状态机、工作流设计器。
这些应当由应用 + Flow + Tool 模板承担。**这条划界要写死，否则 Gear 会被撑成低代码平台。**

---

## 3. 缺口清单（分级）

### 形态总表（先看这张表，再看下面的条目）

> 形态代号：**P** 纯算法族（kind=pure）／**K** 内核受门禁原语／**O** 内核内建非纯／
> **E** ext 侧车／**N** 不需要原语。归类判据见 **§3.7**。

| 形态 | 项 | 优先级 | 一句话 |
|---|---|---|---|
| **P 纯算法** ✅ | `crypto` 扩容：`jwt_verify` / `jwt_sign` / `jwt_decode` | **P0** | ★ 用户立项动机的一半；标准库即可，**比侧车还便宜** |
| **P 纯算法** ✅ | `crypto` 扩容：`password_hash` / `password_verify` | P0 | `auth` 存口令的前置；pbkdf2 可零依赖 |
| **P 纯算法** ✅ | 新族 `id`（`uuid` / `nanoid` / `code`） | P1 | `google/uuid` 已在 go.mod |
| **P 纯算法** ✅ | 新族 `validate`（`json_schema`） | P1 | `santhosh-tekuri/jsonschema` 已是直接依赖 → 零新增 |
| **K 内核** | `db` 事务 + migrate | P0 | 事务语义只能引擎保证（用户确认） |
| **K 内核** | `auth` **会话收敛** | P0 | `callerOf` 是内核代码路径；只做「把任何来源的身份换成一个 Gear 会话」 |
| **K 模型层** | Tool 声明式 REST 路由 | P0 | 不是原语，是模型改造 |
| **O 内建非纯** | `log` | P1 | 有副作用故不进纯算法族；照 `ctx.json` 先例无声注入 |
| **E ext** | `ext.oidc`（单点登录握手） | **P0** | ★ 用户立项动机的另一半（**验签已归 crypto**，侧车只剩握手） |
| **E ext** | `ext.filestore`（文件仓库） | P0 | 侧车不受 Lua 沙箱限制 |
| **E ext** | `ext.cron`（定时调度） | P0 | service ext 回推事件即可 |
| **E ext** | `ext.excel` / `ext.pdf` | P1 | 从 P4 上调 |
| **E ext** | `ext.oss` / `ext.s3` / `ext.minio` | P1 | 桥接企业已有对象存储 |
| **E ext** | `ext.redis` | ✅ P1（2026-09-30 已交付） | 锁 / 队列 / 缓存一把梭 |
| **E ext** | `ext.dingtalk` / `ext.feishu` | P1 | 国内通知通道 |
| **N 不需要原语** | 队列、锁、审计、分页、业务单号 | P1~P2 | db + Lua + `ext.redis` 就够，见 §3.6 |

### P0 —— 缺了就写不出企业应用

#### P0-1 `auth`：身份 / 会话 / 权限（内核原语，新第 8 个受门禁原语）

> ★ **已拍板否决（2026-09-28，ENTERPRISE_PRD.md v1.1 §6 Q1）**：不做 `auth.db` / `ctx.auth` 原语——
> 「能支持应用做授权就行了，那是业务上的事，走 Hook」。生效路线 = Hook fail-closed +
> 第 0 批 `crypto.password_*`/`jwt_verify`；交付物改为**官方鉴权样板**（PRD FR-1，零内核改动）。
> 原方案存档于 `AUTH_DESIGN.md`。以下原文保留作历史判定记录。

- 方法：`login({account, credential})` → 会话令牌；`verify({token})` → 用户 + 声明；
  `user({id})`；`session.revoke`；`perm.check({user, action, resource})`
- 为什么必须内核（**注意：只为「业务鉴权」，不为「平台面访问控制」**——后者见 `AUTH_DESIGN.md` B 层）：
  ① 登录/账号体系是横切关注点，每个应用各写一套必然漂移（靠 Hook 自己写只解决「判断」，
  不解决「账号从哪来」）；② `ctx.caller` 已是内核概念，身份库必须与它同源；
  ③ 承载主体要能被内核验真，否则 `X-Gear-Subject` 谁都能填。
  ⚠ 更正（2026-09-28）：「REST 面 30 端点零认证」属于**平台面**问题，是 L3 共享安装的门槛，
  **不是本条的理由**——企业应用的终端用户只走 `/api/tools/{id}/run` 与 `/api/executions`，
  这两条经过引擎，Hook 拦得住。
- 口径（第二版已定，见 `AUTH_DESIGN.md` §2.4）：**auth.db 存在即启用**——有库则 Bearer 必须是有效会话令牌
  （无效 401，不回落）；无库则维持「Bearer = 不透明主体」的今日行为。`X-Gear-Subject` 在启用后
  降级为「已认证网关」专用（它可伪造，见 `identity.go:38`）。
- 依赖：`crypto.password_hash/verify`（存口令）+ `crypto.jwt_verify`（验外部 IdP 令牌）——
  **二者均属纯算法族**（第四版改判，见 P1-4 与 §3.7）。内核自身仍只发不透明会话令牌，**不自签 JWT**。
- 验收：未登录 → `ctx.caller.subject` 空串 → Hook 拒绝（不是空结果放行）；无效令牌 → 401；
  Hook 里 `ctx.auth.perm.check` 可用；`app run` 与 `gear serve` 共用同一身份库。
- 工作量：**S~M**（第二版下调：不做中间件、不做端点分级；MCP 的 actor 恒空口径锁
  `TestMCPWritesLeaveActorEmpty` **不破**）。
- **实施方案见 `AUTH_DESIGN.md`**（第二版 2026-09-28，**已按「业务鉴权 / 平台面访问控制」拆成两层**）：
  - **业务鉴权走 Hook，不需要 REST 中间件**——`IDENTITY_CONTEXT_DESIGN.md` §3.6 已定
    「身份不匹配 → 让 Hook 报错即可，不新增拒绝机制」，`hookFail` 本就 fail-closed。
    且 `server.go:523/816` 已在注入 caller、`callctx_test.go:150` 证明 Hook 读得到 ⇒ **今天就能用**。
  - auth 原语只补一件事：**应用不用各写一遍登录**。三块：`auth.db` + `ctx.auth.*` + `callerOf` 一处改造。
    开关 = auth.db 是否存在（无需 `--auth` 参数）；登录做成 Tool，**不新增端点**。
  - **平台面（registry 写 / secrets / apps）是另一个问题**：Hook 够不着（不经过引擎），
    最小解是应用只反代业务端点（改一行 yaml），完整中间件推到 L3 形态决策之后。

#### P0-2 `db` 事务 + schema 迁移（扩展现有原语，不新增 capability）

- 现状痛点：「下单扣库存」「审批流转改两张表」在 Gear 里**无法表达**——`db` 只有 `query/exec/ping`，
  一次调用一条 SQL，中间崩溃就脏数据。这是企业应用的硬伤。
- 提案（**两段走，先易后难**）：
  1. `db.batch({driver, dsn, statements:[{sql, params}]})` —— 单调用内多语句同事务提交，
     无跨调用状态，安全且够用（覆盖 80% 场景）。
  2. `db.begin/commit/rollback` 句柄式 —— 覆盖跨调用事务，但要解决句柄生命周期与进程重启，留后。
- 配套：`migrate` 机制（应用声明版本化迁移文件，引擎启动/升级时执行，取代现在
  每个应用自己 `CREATE TABLE IF NOT EXISTS` 满天飞）。
- 坑（项目记忆已实踩）：SQLite DSN 必须带 `_txlock=immediate`，否则读锁升写锁直接
  `database is locked` 且不走 busy handler；**禁止「先 SELECT 再写」的事务写法**。
- 工作量：**M**。

#### P0-3 文件 / 对象存储 → **`ext.filestore` 侧车**（第三版改形态：内核原语 → ext）

- **改判理由（用户指出，成立）**：原判「Lua 沙箱禁 `os` → 应用写不了文件 → 只能内核给」是错的。
  **侧车是独立进程，不受沙箱限制**——`ext.serve` 能起 HTTP、`ext.email` 能连 SMTP，
  文件读写同理。所以这是 ext 能干的事，不该占内核原语名额。
- 方法（侧车）：`put({name, content_base64, content_type})` / `get` / `stat` / `list` / `delete` / `url({name, ttl})`
- ★ **路径不能硬编码**：`root` 从 `ctx.config.get("...")` 取（config 原语已存在），
  别重演 mindstorm 那 18 处 `local DSN = "D:/…/mindstorm.db"` 的覆辙。
- 与 `ext.oss/s3/minio` 并存：前者是「这一套 gear 自己的文件仓库」，后者是「企业已有的对象存储」。
- 验收：put→get 二进制一致（含非 UTF-8）；超限 fail-closed；`root` 非法即报错不静默创建。
- 工作量：**S~M**（比内核原语便宜一个档：不碰 `cap.go` / `purefunc.go` / 前端枚举 / 门禁测试）。

#### P0-4 定时调度 → **`ext.cron` service ext**（第三版改形态：内核原语 → ext）

- **改判理由（用户指出，成立）**：原判「ext 侧车没有回调引擎的能力」是**事实错误**。
  service ext 本来就是靠**回推事件**工作的——`ext.webhook` 就是「外部 POST → 侧车 → 回推 → 触发 Flow」。
  cron 只是把「外部触发」换成「时间触发」，机制完全同源。
- 形态：`extensions.yaml` 加 `ext.cron`：`service: true` + `listen` + **`source: cron`** + `health_probe`；
  引擎侧只需把 `cron` 加进 `internal/engine/source.go:18 knownSources`（**两处同步**，该文件注释已写明）。
  触发形态与 webhook 一致：`ctx.subscribe.start({source="cron", …})` 或直接 `cron.add({expr, name})`。
- 方法：`add({name, expr, payload})` / `at({name, at})`（一次性）/ `list` / `cancel({name})`
- 持久化与重启恢复由**侧车自己**存（它长驻，自持 cron 表）——这正是侧车形态的好处。
- 三个必须答对的语义问题（否则上线就是事故，无论内核还是侧车都要答）：
  1. **幂等**：同一时刻重复触发怎么办（建议：按 `name` + 触发时刻唯一键去重）。
  2. **补偿（misfire）**：进程没开时错过的触发，起来后补不补（建议：默认不补，显式 `catch_up=true` 才补）。
  3. **并发**：上次没跑完下次又到点（建议：默认跳过，显式 `overlap=true` 才并发）。
- 与 `app run` 生命周期绑定：应用进程退出后调度是否继续，需明确（牵涉 Job Object）。
- 工作量：**M~L**。

#### P0-5（非原语，同期必办）Tool 声明式 REST 路由

- 现状：所有 API 都是 `POST /api/tools/{id}/run`（mindstorm 客户端 `www/api.js:11` 就是这么写的），
  企业前端拿不到 RESTful 资源语义，只能拿 `gear serve` 起第二个进程再靠 `ext.serve` 反代兜（`mindstorm_flow.yaml:33`）。
- 提案：Tool 增加可选 `api: {method, path}` 声明 → 引擎挂成 `/api/orders`、`/api/orders/{id}` 等资源路由，
  **不新增原语**，是模型层改造 + 生成客户端。
- 价值：企业前端可直接按资源写客户端；`gen-client.mjs` 能生成真正的 REST 方法名。
- 工作量：**M**。

---

### P1 —— 写得出但每家都在重复造

| # | 原语 | 形态 | 理由 / 验收 | 量 |
|---|---|---|---|---|
| **P1-0** | `ext.oidc`（JWT / 单点登录） | **ext，service** | ★ **用户立项的最初动机**：JWT 与 SSO 在 Lua 里做不方便。详见 **§3.5**。**优先级从 P2 提到 P1 首位** | M |
| P1-1 | `queue` | **ext.redis / db 表** | 改判：不进内核。轻量场景「db 表 + Flow 轮询」即可；要真队列用 `ext.redis`（P1-9）或已有的 `ext.rabbitmq/kafka` | S |
| P1-2 | `lock` | **ext.redis 或 db 唯一键** | 改判：不进内核。单机用 db 唯一键（Lua 里一条 SQL），多实例用 `ext.redis` | S |
| P1-3 | `audit` | **不需要原语** | 改判：业务数据审计 = Lua 里 `db.exec` 插一行；定义层留痕与 actor 已有（`IDENTITY_CONTEXT_DESIGN.md` §10）。真缺的是「execs 按 actor 过滤」——那是 REST handler 加 `WHERE`，不是原语 | S |
| P1-4 | `jwt` | **拆两半：验签归 `crypto` 纯算法，握手归 `ext.oidc`** | 第四版再改判（§3.7）：**验签本身是纯算法**——RS/ES/EdDSA 全在标准库，JWKS 由调用方当参数传（密钥由调用方传是既有口径），校验 exp 依赖时钟但不破坏「纯」（与 `time.now` 同类）。只有 **OIDC 握手**（discovery / code 换 token / 回调端点 / JWKS 长驻缓存）才需要侧车。⇒ 不新增 `jwt` 族，扩 `crypto` 即可 | S |

> ✅ **2026-09-28 已落地**：`crypto.jwt_decode` / `jwt_verify` / `jwt_sign` 已进纯算法族（P0 第 0 批），
> 含防算法混淆、禁 `alg=none`、JWKS 按 kid 选、claims 校验。见 `REQUIREMENTS.md` 的 `REQ-PURE-JWT`。
| P1-5 | `log` | **先核实再定** | Lua 侧确实无 `ctx.log`（`lua.go` 无），但**先确认 Lua 的 `print` 是否已进执行日志**——若进，则不需原语，只差文档说明 | S |
| P1-6 | `db.query_page` | **Lua 片段库** | 改判：不进原语。每个应用都在写 `LIMIT/OFFSET` + `count(*)` ⇒ 发官方 Lua 片段 / Tool 模板即可 | S |
| P1-7 | `ext.excel` / `ext.pdf` | ext | **优先级从 P4 上调**。导出 Excel 是企业管理系统的标配（不是长尾），现在完全没法做。 | M |
| P1-8 | `ext.oss` / `ext.s3` / `ext.minio` | ext | 桥接企业已有对象存储（`ATOM_ROADMAP` P3 已列，维持）。 | M |
| P1-9 | `ext.redis` | ✅ ext（2026-09-30 已交付） | 缓存 + 分布式锁 + 延迟队列一把梭，很多企业已有 Redis。能部分覆盖 P1-1/P1-2，但引入外部依赖，**不替代内核版**（内核版仍按「有真实诉求再做」）。**29 方法**：KV/Hash/List/Set/ZSet 全数据结构 + 分布式锁 + 延迟队列；零依赖手写 RESP2（不引 `go-redis`，保住 `go.mod` 零变动）；单连接串行即原子（锁/队列不用 Lua）；`plugin/verify.py` P1g 覆盖全结构 + 锁 + 队列真实往返与 fail-closed。 | M |
| P1-10 | `ext.dingtalk` / `ext.feishu` | ext | 国内企业的实际通知通道，比 `ext.slack` 更贴合（slack 在 P2 已列）。 | S |

### P2 —— 按需，不阻塞

- ~~`ext.oidc` / `ext.oauth` / `ext.ldap`~~ → **已提到 P1-0，见 §3.5**（用户立项动机，`ATOM_ROADMAP` P3 曾列）。
  它与 `ctx.auth` 的关系是**上游**不是补充：负责「证明你是谁」，然后把身份交给内核建会话。
  （★ 2026-09-28 Q1 拍板后：会话也归应用业务层，`ext.oidc` 只做「拿令牌」的握手，
  验签已由 `crypto.jwt_verify` 承担——见 ENTERPRISE_PRD.md §4 E1。）
- `ext.es` / `ext.meili`：全局搜索，替代 mindstorm 那套 `LIKE` 土办法。
- `ext.wechat-pay` / `ext.alipay`：交易类场景。
- WebSocket 双向推送：**不是 Lua 原语**（Lua runtime 明确不支持 service，`lua.go:49`），只能由内核 REST 面或 ext 侧车提供。SSE 已有（`server.go:428`），实时看板够用，协作类才需要 WS。
- `validate` 纯函数：JSON Schema 校验，表单校验高频。
- `id` 纯函数：UUID / 雪花 / 业务单号生成（订单号、流水号）——企业应用高频且各家都自己拼字符串。
- 多租户与行级数据权限：**需先决策**（是模型层能力还是原语），暂不写码。
- i18n：有海外业务才要。

---

### ★ 3.5 JWT / 单点登录：**哪一部分**不该在 Lua 里做（**用户立项的最初动机**）

> 用户原话：「jwt、单点登录在 lua 里做不方便，这是我让你加的最初原意」。
> 这条诉求**成立**。落点分两处：**验签进 `crypto` 纯算法族，握手进 `ext.oidc` 侧车**。

**逐条拆开，看 Lua 到底卡在哪（第四版加了「归哪」一列）：**

| 环节 | Lua 能做？ | 卡点 | **归哪** |
|---|---|---|---|
| base64url 编解码 | 能 | `crypto.base64_*`（`variant=url`）已有 | 已有，不用加 |
| HS256 验签 | 能 | `crypto.hmac_verify` 已有 | 已有，不用加 |
| **RS / ES / EdDSA 验签** | **不能** | `ctx.crypto` 只有 hash / hmac / 对称加解密，**没有非对称验签** | **`crypto` 纯算法**（A✗B✗；标准库 `rsa`/`ecdsa`/`ed25519`；D 冻结 RFC 7518） |
| **claims 校验 exp/nbf/iss/aud** | 能做但易漏 | 手写时钟与 issuer 比对，漏一个就是漏洞 | **`crypto.jwt_verify` 一并做掉**（最容易写错的部分，不该留给每个应用） |
| **JWKS 拉取 + 缓存 + 轮换** | **不能** | 需要长驻与缓存；Lua Tool 每次执行都是新环境 ⇒ 每次重拉 | **`ext.oidc`**（B✓）。但验签接口**接受调用方传入的 JWKS**，所以没有侧车也能跑 |
| OIDC 三步 + `state`/`nonce` | 能做但脆弱 | 防 CSRF 的状态要自己存 | **`ext.oidc`**（D✓ 厂商实现） |
| **redirect_uri 回调端点** | **不能** | Lua runtime 不支持 service（`lua.go:49`），监听不了端口 | **`ext.oidc`** |

**结论（第四版修正：原来是 0 : 3，正确拆法是 1 : 2）**

上一版把「长驻 + 非对称验签 + 回调端点」三样**一律**判给侧车，其中**非对称验签判错了**：
它是冻结标准、标准库就有、无需长驻，只是**现在没接进 `ctx.crypto`**——
「现在没有」和「该放侧车」是两回事（这正是 §3.7 的 D 轴）。真正需要侧车的只有**长驻**与**回调端点**两样。

**这条修正的实际收益**：企业里最常见的那条路径**根本不需要侧车**——
前端拿 IdP 签发的 id_token 直接打到 gear，gear 用 `crypto.jwt_verify` 验签即可
（JWKS 由调用方传入，可自行落 `ctx.config` 做缓存）。
只有**要做完整登录跳转**（discovery → 授权 → code 换 token）时才需要 `ext.oidc`。

**落点：`ext.oidc`（service ext）**

- 方法：`authorize_url({issuer, client_id, redirect_uri, state})` / `exchange({code})` /
  `verify({token})` / `userinfo({token})` / `jwks.refresh`
- **回调复用 `ext.webhook` 的入站能力**（`/in/{path}`），不必新造 HTTP 服务端
- 不承载事件源（`source` 只在侧车主动推事件时才需要）
- 拿到的身份交给内核 `ctx.auth` 建会话 ⇒ **与本地口令汇流成同一个 subject**

**与 `ctx.auth` 的关系（一句话，也是内核只做「会话收敛」的理由）：**

> `ext.oidc` 负责「**证明**你是谁」，`ctx.auth` 负责「**记住**你是谁」。
> 前者是协议（多变、要长驻、要密码学），后者是会话（稳定、要被 `callerOf` 读到）。
> 协议交给侧车，会话留在内核——这正是「判定三问」的正确答案，也回答了最初动机。

### 3.6 因此不需要原语的（db + Lua + 侧车就够）

`queue` / `lock` / `audit` / `log` / `db.query_page` / 业务单号——
都是「有 db 就能自己写」或「侧车能干」的东西，**不占内核原语名额**（见 P1 表）。

> ⚠ **第四版修正（2026-09-28）**：`validate` 与 `id` 从这里**移入纯算法族**（见 §3.7）。
> 它们不是「不需要」，恰恰相反——**不该让每个应用各写一遍**，而且标准库 / 既有依赖就能做。

---

### 3.7 形态归类：哪些并入纯算法族、哪些必须独立（2026-09-28 第四版）

> 前三版一直用「进内核 / 做 ext / 不做」三分法。三分法回答不了一个具体问题：
> **同样是密码学，为什么 JWT 验签能进内核、OIDC 握手却不能？**
> 本节给一条四轴判据，把全部候选一次性归类。

#### 3.7.1 先把「纯算法」的口径说准（这一步不做，后面全站不住）

`purefunc.go` 现有的 time / template / crypto **不是数学意义上的纯函数**：
`time.now` 读时钟、`crypto.random` 读熵、`time.sleep` 有副作用。

所以真实口径是这两条，不是「无副作用」：

1. **不碰「可授权的资源」**（db / 文件 / 网络 / 密钥库 / env）——既然没有宿主能力边界，
   要求 Tool 声明 capability 就违背直觉（`purefunc.go:8-10` 原文）；
2. **不持跨调用状态**——手递手，没有句柄、没有会话、没有长驻缓存。

时钟与熵被算作「**环境事实**」而非「资源」，故被容忍。

⇒ 这条澄清直接决定一个边界案例的归属：**`jwt.verify` 要校验 `exp`/`nbf`，依赖当前时间，
但它仍然算纯算法**（与 `time.now` 同类），不该因为「读时钟」被踢去侧车。

#### 3.7.2 四轴判据

| 轴 | 问句 | 不满足 → 去哪 |
|---|---|---|
| **A 副作用** | 要不要碰「需要授权的世界」（db / 文件 / 网络 / 密钥库 / env）？ | 内核门禁原语 或 ext |
| **B 跨调用状态** | 要不要跨调用记住东西（会话 / 句柄 / 锁 / 队列 / 长驻缓存）？ | 内核原语（要一致性）或 ext（可长驻） |
| **C 依赖成本** | 标准库、或**已经在 go.mod 里**的依赖能否搞定？ | 重依赖 → ext |
| **D 稳定性** | 是**冻结标准**（RFC）还是**厂商实现**（SDK / 协议细节）？ | 厂商实现 → ext |

**纯算法 = A✗ B✗ C轻 D标准冻结。任一条不成立就独立。**

> **D 轴是前三版缺的那一条**，也正是开头那个问题的答案：
> JWT 的算法集是 **RFC 7518 冻结**的（HS/RS/ES/EdDSA），十年不变 → 内核实现一次即可；
> OIDC 的 discovery / PKCE / 各家 provider 差异是**厂商实现**，年年变 → 只能放侧车。

#### 3.7.3 归类结果（全表）

**① 纯算法族（kind=pure，进 `purefunc.go`）**

| 落点 | 方法 | 四轴核对 |
|---|---|---|
| `crypto` 扩容 ✅ | `jwt_decode` / `jwt_verify`（HS/RS/ES/EdDSA + `exp/nbf/iss/aud` + leeway）/ `jwt_sign` | A✗（密钥由调用方传，既有口径 `purefunc.go:21`）；**C 轻：`crypto/rsa`+`ecdsa`+`ed25519` 全是标准库**；D 冻结（RFC 7518）。**2026-09-28 已落地** |
| `crypto` 扩容 ✅ | `password_hash` / `password_verify`（pbkdf2-hmac，零依赖手写；`bcrypt`/`argon2` 需 `x/crypto`，`AUTH_DESIGN.md` §2.4 已定 bcrypt） | A✗ B✗；D 冻结（RFC 7914）。**2026-09-28 已落地**，与 Python `hashlib.pbkdf2_hmac` 交叉验证 |
| ~~`crypto` 扩容 ✗~~ | ~~`sign` / `verify`（裸非对称验签，PEM 密钥）~~ **不做**：`jwt_verify` 已按算法分派接受 PEM/JWKS，裸验签是它的子集，单列只会再加两个方法稀释提示词 | — |
| **新族 `id`** ✅ | `uuid` / `nanoid` / `code`（验证码、邀请码） | **C 轻：`github.com/google/uuid` 已在 go.mod（indirect）**；D 冻结（RFC 4122）。**2026-09-28 已落地** |
| **新族 `validate`** ✅ | `json_schema({schema, data})` | **C 轻：`santhosh-tekuri/jsonschema/v5` 已是 go.mod 直接依赖 → 零新增依赖**；D 冻结。**2026-09-28 已落地** |
| 新族 `text`（P2） | `csv_encode/csv_decode`、`url_*`、`query_*` | 标准库 `encoding/csv` + `net/url`，零依赖 |

> ★ **最反直觉的一条：纯算法族比 ext 侧车还便宜。**
> 侧车要写独立进程 + `extensions.yaml` 注册 + 生命周期 + 健康探针 + 构建产物；
> 纯算法族只改 5 处（§6.1），**Lua 绑定由 schema 键自动生成**（`lua.go:298`），
> 不碰 `cap.go`、不碰 `lua.go`、不碰前端枚举脚本。
> ⇒ 判据的应用顺序应当是：**能纯算法化的一律纯算法化，剩下的才谈 ext。**

**② 内核内建 · 非纯 · 无门禁（走 `ctx.json` 那条路，不进能力目录）**

| 项 | 为什么 |
|---|---|
| `log` | 写执行记录 = 有副作用 ⇒ **不纯**（塞进 `pureFuncPrimitives` 会让 `primitiveKind` 的 `"pure"` 撒谎）；但「能不能打日志」不需要授权 ⇒ 不该进门禁。**最省做法：照 `ctx.json`（`lua.go:243`）与 `ctx.caller`（`lua.go:273`）的先例，在 `lua.go` 里无条件注入**——不进 `pureFuncPrimitives`、不进能力目录，于是不必给 `primitiveKind` 加第三种值。 |

**③ 内核受门禁原语（A 或 B 成立）**

| 项 | 哪条轴不成立 |
|---|---|
| `db` 事务 + migrate | A✓（写库）+ B✓（跨调用句柄）——用户确认的唯一不可替代项 |
| `auth` 会话 | B✓（会话表）+ 被内核代码路径 `callerOf`（`identity.go:40`）依赖 |

**④ ext 侧车（C 或 D 成立，或需长驻 / 监听端口）**

| 项 | 哪条轴不成立 |
|---|---|
| `ext.oidc` | **D✓（厂商实现）+ B✓（JWKS 长驻缓存）+ 需监听 `redirect_uri` 回调端口**。⚠ **职责比第三版缩小**：验签已归 `crypto.jwt_verify`，侧车只剩「协议握手」 |
| `ext.cron` | B✓（长驻） |
| `ext.filestore` | A✓（文件系统） |
| `ext.excel` / `ext.pdf` | C✓（OOXML 重依赖） |
| `ext.redis` / `oss` / `es` / 支付 / `dingtalk` / `feishu` | D✓（厂商各异） |
| WebSocket | B✓（长驻连接） |

**⑤ 不需要原语**：`queue` / `lock` / `audit` / `db.query_page` / 业务单号 —— 见 §3.6。

#### 3.7.4 抗膨胀：纯算法族也要设闸门

纯算法族的方法会进 `capabilitiesText`（AI 提示词）与能力目录 ——
**每加一个方法都在稀释 AI 选工具的注意力**。两条硬闸：

- **分族标准是「语义内聚」，数字只是膨胀信号**：族 ≤ 6 个、每族方法 ≤ 10 个；
  超限时先问「是不是语义该拆了」，而不是「要不要硬塞进来」。
  ⚠ **2026-09-28 实测：5 族 22 方法**（`time` 4 / `template` 1 / `crypto` 13 / `id` 3 / `validate` 1），
  `crypto` 已超「≤10」——**这是登记在册的例外，不是漏网**：crypto 是「算法集合型族」
  （`hash`/`hmac`/`encrypt`/`decrypt` 本来就在），jwt 与 password 属同子域的自然增长；
  为守数字把它拆成 `credential` + `codec`，族数反会从 5 涨到 7（又超「族 ≤6」），
  两个上限一起破，且 agent 更难理解。**再涨到 >16 才拆**（按 `token` = jwt+password、`codec` = base64 切）。
- **`sql.page` 不进纯算法族** —— 它不是「不纯」（纯字符串拼接、零依赖），
  而是**做了就会被要求加 WHERE 构造与方言适配，一路滑向 ORM**。
  这是 §4 划界在纯算法族上的延伸：**纯，不等于该进。**

---

## 4. 明确不做（划界，防止被撑成低代码平台）

这些会被反复提议，按 `ATOM_ROADMAP` §0 判定三问，它们**含业务语义**，应停留在应用层：

| 常被提议的「原语」 | 为什么不做 |
|---|---|
| 审批流语义（会签/或签/撤回/加签） | 领域规则，每家公司不同 → Flow + 数据模型 |
| 动态表单 / 字段级权限 | 业务元数据 → 应用层 |
| 报表 / 图表 DSL | 表现层 → 应用层 + 前端 |
| 业务状态机 | 领域语义 → 应用层（Flow 已能表达顺序） |
| 工作流设计器 | 前端产品，不是原语 |

> 例外提醒：如果哪天 Flow 支持 loop + 人工节点（`ATOM_ROADMAP` 未列），
> 「审批流」可以在 Flow 层做出官方模板，但那仍然是**模板**不是原语。

---

## 5. 排期建议（四批，第 0 批最便宜）

| 批次 | 内容 | 判据 |
|---|---|---|
| **第 0 批（纯算法，最便宜）** | `crypto.jwt_*` + `crypto.password_*`、新族 `id`、新族 `validate` | 只改 5 处（§6.1）、**零新依赖**、不动门禁、不新增进程；**做完立刻可用，不依赖任何形态决策**。<br>✅ **2026-09-28 已完成**：5 族 22 方法，engine 包绿灯（`scripts/gate.sh` 全量复核中） |
| **第一批（碰内核门禁）** | ~~`auth` **会话收敛**~~（Q1 拍板否决，改 Hook 鉴权样板，零内核）、`db` 事务+migrate（batch 接受 DDL、迁移走 bundle `migrations:` 字段）、REST 路由 | db 事务是唯一「不可替代」；REST 路由是模型层不是原语；鉴权走 Hook（详见 `ENTERPRISE_PRD.md` v1.1） |
| **第二批（ext，可并行插空）** | ★ `ext.oidc`（握手）、`ext.filestore`、`ext.cron`、`ext.excel/pdf` | **不改内核**（`ext.cron` 只加一个 source 枚举），四项互不依赖，可并行做 |
| **第三批（有外部依赖才做）** | `ext.redis`、`ext.oss/s3/minio`、`ext.dingtalk/feishu`、`ext.es/meili` | 按需 |

**不排期的**（不需要原语）：`queue` / `lock` / `audit` / `db.query_page` / 业务单号——
随应用现写（db + Lua + 侧车），见 §3.6。
（`log` 属 **O 类**：不进纯算法族，照 `ctx.json` 先例在 `lua.go` 无声注入，**先核实 Lua 的 `print` 是否已进执行日志**。）

> 排期观感的变化：第一版把五件都排进内核，看起来是「内核大改」；第三版变成
> 「3 项内核改动 + 一批侧车」；**第四版多出第 0 批——纯算法族，它最便宜、零依赖、立即见效，
> 而且把 `ext.oidc` 的工作量砍掉一半（验签不用侧车做了）**。

`ext.*` 项（P1-7 ~ P1-10）与上述批次正交，可并行插空做——它们不改内核。

---

## 6. 新增内核能力的动作清单

### 6.1 新增 / 扩容一个**纯算法族**（5 步 —— 对应 §3.7 ① 组）

适用判据：A✗ B✗ C轻 D冻结（无副作用、无跨调用状态、依赖轻、标准冻结）。

1. `internal/engine/purefunc.go`：`pureFuncPrimitives` 加族名 + `purePrimitiveOrder` 加展示位
   （**两张表必须同步**，有测试守着不漂移：`capability_validation_test.go:263`）；
2. 同文件：写 `pure<X>Schemas`（方法级 schema）+ 实现函数 + `callPureFunc` 的 switch 分支；
   入参校验沿用 `pureStr` / `pureEnum` / `pureNum` —— **实现即最后一道闸**
   （Lua 直调不经过 `validatePrimitiveArgs`，见 `purefunc.go:19-20`）；
3. `internal/engine/extensions.go`：`builtinCapabilitySchemas` 加一行（在 `extensions.go:644` 附近）；
4. 测试：`capability_validation_test.go`（族表 + 顺序 + `primitiveKind=="pure"`）、
   `extensions_test.go:354`（`len(allBuiltinPrimitives())` 条数断言）、
   `primitive_step_test.go:134`（Flow 直调），**并配负向验证**（`tools/negcheck.py`）；
5. 文档：族计数从 3 改到新数字（README 多处、`MCP_AGENT_DESIGN.md`、`EXT_SERVICE_DESIGN.md`、
   `IDENTITY_CONTEXT_DESIGN.md`、`ATOM_ROADMAP.md`、`REQUIREMENTS.md`），
   以及**代码里的用户可见文本**（`internal/cap/cap.go` 的未知 capability 报错、`internal/cli/ai.go`
   的 `gear ai capabilities` 说明、`internal/cli/scaffold.go` 生成的 YAML 注释）——漏了这几处
   CLI 输出会与目录里的内容自相矛盾。历史归档（`docs/requirements-archive.md`、`GEAR_工程方法_v1.md`）不改原文。

**不动**（这就是它比 ext 便宜的原因）：`cap.go`（不进门禁）、`lua.go`（绑定由 schema 键自动生成，
`lua.go:298`）、`plugin/web-ui/scripts/gen-enums.mjs`（没有 capability 就没有枚举）。

> ⚠ 唯一的坑：`purePrimitiveOrder` 与 `pureFuncPrimitives` 是**两份清单**（与 `builtinParamOrder`
> 同款历史坑），顺序还会进 AI 提示词 —— 改一处必须同步另一处，测试会拦但不该靠测试兜。

### 6.2 新增一个**受门禁原语**（10 步 —— 对应 §3.7 ③ 组）

沿用既有流程（与 `plugin/` 下新增 ext 完全不同）：

1. `internal/cap/cap.go`：常量 → `KnownPrimitives` → 接口 → `Providers` 字段 → 访问器；
2. `internal/engine/purefunc.go`：`gatedPrimitiveOrder`（有测试守着顺序不漂移）；
3. 实现文件（仿 `internal/cap/sqldb.go` / `configdb.go` / `secretstore.go`）；
4. `internal/engine/extensions.go`：`builtinCapabilitySchemas` 加方法级 schema；
5. `internal/engine/lua.go`：`ctx.<name>` 绑定（受门禁类手写，Lua 侧未声明即不存在）；
6. 引擎装配 `Providers` + Flow 直调路径（`stepcall.go` 的 `primitiveHost`）；
7. 前端枚举：`plugin/web-ui/scripts/gen-enums.mjs` 重新生成 + `CAPABILITY_LABEL` 补中文名（`contract.spec.ts` 会拦）；
8. 测试：`cap_test.go` / `lua_cap_test.go` / `capability_validation_test.go`，**必须配负向验证**（`tools/negcheck.py`）；
9. 文档同步：`README.md`、`ATOM_ROADMAP.md`、`REQUIREMENTS.md`（`tools/doc_audit.py` 在 `gate.sh` 里机械核对）；
10. 跑全闸门：`bash scripts/gate.sh`。

---

## 7. 与 `ATOM_ROADMAP.md` 的关系

本文**不覆盖**路线图的分期基线，只补两件事：
① 把「企业应用」场景下的缺口按 P0/P1/P2 重排（路线图是按「连接器重要程度」排的，两者的排序维度不同）；
② 指出**内核原语层**的缺口（路线图 P3/P4 主要是 ext 连接器，内核侧只有「方法命名统一」一项待办）。

落地时：ext 类条目并入 `ATOM_ROADMAP.md` §2 的 Phase 表；
内核原语类条目并入 §3「核心原子：已补 / 待补」。
