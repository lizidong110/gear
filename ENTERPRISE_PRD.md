# ENTERPRISE_PRD.md — 企业应用场景能力需求（PRD）

> 状态：**v1.2，第一批已交付（FR-1/2a/2b/3 均 `implemented`），第二批 E1 `ext.oidc`、
> E2 `ext.filestore`、E3 `ext.cron`、E4 `ext.excel`/`ext.pdf` 均已交付（2026-09-29，零依赖手写 OOXML + 手写 TTF 子集）**
> 上游动机：`ENTERPRISE_PRIMITIVE_GAP.md`（缺口梳理与形态判定，四版收敛）
> 实现方案：GAP §6（动作清单）——**本文档只定 WHAT 与验收，不定 HOW**
>
> **视角约定（本文档的写法纪律）**：所有需求都从**调用方可见的接口面**出发描述——
> 智能体经 **MCP 15 工具**、应用开发者经 **Tool/Flow YAML + Lua 原语 + REST 面**、
> 终端用户经 **应用前门**、运维经 **CLI**。内核内部怎么实现不在本文档范围。
>
> 2026-09-28。第 0 批（纯算法族：`crypto.jwt_*` / `crypto.password_*` / `id` / `validate`）
> 已交付且 gate 全量 PASS；同日 v1.1 完成 Q1~Q5 拍板（见 §6），**FR-1 由「auth 原语」
> 改判为「Hook 鉴权样板」**；同日 v1.2 追加 Q5 的生命周期口径——`ext.cron` **硬绑定**
> 「应用退出即停、重启不接管」（§4.1），并为第二批 E1~E4 补上各自 mini-PRD 与验收（§4.2~§4.5）。

---

## 0. 一句话

企业应用写得出、活得下来缺的不是连接器，是**运行时地基**：事务写库与 schema 迁移（FR-2）、
RESTful 资源路由（FR-3）——授权是业务的事，走 Hook（FR-1 只交付样板），外加一批可并行的 ext 侧车（§4）。

## 1. 背景与动机

1. **立项原话**：「jwt、单点登录在 Lua 里做不方便」——验签已由第 0 批解决（`crypto.jwt_verify`
   纯算法）；「记住你是谁」的会话与账号按 **Q1 拍板归应用业务层**（Hook + 应用自己的 db），
   Gear 只需交付一份**官方鉴权样板**让应用不用各写一遍（FR-1）。
2. **缺口实证**（GAP 复核过的事实）：`db` 只有 `query/exec/ping`，「下单扣库存」这种
   两表一致写入**无法表达**；所有 API 都是 `POST /api/tools/{id}/run`，前端拿不到资源语义。
3. **交付节奏**：第 0 批已证明「纯算法族最便宜、零依赖、立即见效」；本 PRD 的第一批
   只剩**两件半**内核侧改动（db 事务+迁移、REST 路由）加一份样板（零内核改动）。

## 2. 调用方与现有接口面（坐标系）

### 2.1 调用方角色

| 角色 | 经什么接口 | 关心什么 |
|---|---|---|
| **智能体（构建者）** | MCP 15 工具（`gear_*`） | 发现能力（`gear_capabilities`）、写定义（`gear_apply`）、试跑（`gear_run`/`gear_invoke`）、排障（`gear_doctor`/`gear_execution`） |
| **应用开发者** | Tool/Flow YAML + Lua 原语（`ctx.*`）+ REST 面 | 用现成原语拼业务，不造账号体系、不手写事务补偿 |
| **终端用户** | 应用前门（`Authorization: Bearer <token>`） | 登录一次拿到令牌，后续请求被识别身份 |
| **运维 / 交付** | CLI + 产物（export/import） | 起停应用、备份交付 |

### 2.2 现有接口面盘点（PRD 的基线）

**MCP 工具面（15 个，硬预算锁死）**：`gear_guide` / `gear_catalog` / `gear_describe` /
`gear_capabilities` / `gear_validate` / `gear_apply` / `gear_delete` / `gear_promote` /
`gear_invoke` / `gear_run` / `gear_execution` / `gear_app` / `gear_doctor` / `gear_export` /
`gear_import`。

**能力目录（`gear_capabilities` 暴露给智能体的全部家底）**：

| 类别 | 成员 | 门禁 |
|---|---|---|
| 受门禁原语（7） | `http` / `call` / `db` / `secret` / `event` / `subscribe` / `config` | Tool 声明 `capabilities:[...]` 才可用 |
| 纯算法族（5 族 22 方法） | `time`(4) / `template`(1) / `crypto`(13) / `id`(3) / `validate`(1) | 不需声明（含第 0 批新增的 jwt / password） |
| Lua-only | `json` | 不进能力目录 |
| ext 侧车 | `ext.serve` / `ext.email` / `ext.webhook` / … | 侧车独立进程 |

**REST 面（应用前门用的关键端点）**：`POST /api/tools/{id}/run`、`POST /api/executions`、
`POST /api/capabilities/invoke`、SSE 事件流。**当前零认证**（平台面问题归 L3，本 PRD 只动业务面）。

**鉴权现状（Q1 决策的依据，已核实可用）**：REST 业务执行面已在注入 caller；Hook 读得到
`ctx.caller.subject` 且 fail-closed 能拒；承载主体从 Bearer / `X-Gear-Subject` 取。
第 0 批补齐了 `crypto.password_hash/verify`（口令）与 `crypto.jwt_verify`（外部令牌验签）。
**⇒ 应用做授权的接口件已齐，缺的是一份不用各写一遍的样板。**

### 2.3 需求落到接口面的四种方式（本 PRD 的表达框架）

每条需求必须写明它改变的是哪个接口：

1. **能力目录条目**——智能体经 `gear_capabilities` 看到、经 `gear_invoke` 可试调；
2. **YAML schema 字段**——智能体经 `gear_validate`（不落库预检）与 `gear_apply`（落库）使用；
3. **REST 面行为**——应用前端 / 终端用户直接观察（状态码、语义）；
4. **错误码与诊断**——`gear_validate` 字段级诊断与执行错误里可见，进错误码单一真相源。

**本 PRD 所有需求合计新增 MCP 工具数 = 0**（预算 15 不动，见 NFR-3）。
**Q1 拍板后合计新增受门禁原语 = 0**（第 8 个原语 `auth` 取消）。

---

## 3. 第一批需求（P0，含验收）

### FR-1 Hook 鉴权样板（v1.1 改判：不做 auth 原语）

> **拍板（Q1，2026-09-28）**：「不要 user.db 库，能支持应用做授权就行了，那是业务上的事，走 Hook」。
> 原 v1.0 的 FR-1（`auth.db` + `ctx.auth.*` 8 方法 + Bearer 查会话表）**整条取消**；
> 原方案转历史存档（`AUTH_DESIGN.md`，已加注记）。
> 前提事实（已核实）：caller 注入、Hook 可读可拒（fail-closed）、第 0 批口令/JWKS 算法——接口件已齐。

**用户故事**

- US-A1（应用开发者）：我要一份官方样板展示「登录 Tool → 发令牌 → Bearer → Hook 拒未认证」
  的完整链路，以便每个应用不用从零摸索鉴权写法（这正是原 auth 原语想解决的「各写一遍」，
  样板就够，不需要内核机制）。
- US-A2（应用开发者）：样板要同时展示**两条凭据路线**——本地口令（`crypto.password_*` + 应用 db）
  与外部 IdP 令牌（`crypto.jwt_verify`，JWKS 由调用方传），以便按企业实际情况选。

**交付物（接口面零新增，全是 examples + 文档 + 一条回归测试）**

1. **样板应用**（`examples/auth-app/`，`gear import examples/auth-app` 一次灌齐
   Tool / Flow / Hook / App）：
   - 登录 Tool：验口令（`crypto.password_verify`，哈希由 `crypto.password_hash` 生成、
     存**应用自己的 db**）→ `crypto.jwt_sign` 签发 HS256 令牌；签名密钥从 **secret 库**读
     （`ctx.secret.get`，不落 YAML、不入库）；
   - 受保护 Tool：前端带 `Authorization: Bearer <token>` 调 `POST /api/tools/{id}/run`；
   - 守卫 Hook（`before_step` + `criticality: critical`）：`ctx.caller.subject` 为空或
     `crypto.jwt_verify` 验签失败 → 报错拒绝（fail-closed）；
   - **两条凭据路线**：本地口令（上述链路）与**外部 IdP**（`crypto.jwt_verify` + 公钥/JWKS 来自
     secret，应用不存账号）。
2. **文档**：样板自带 `README.md`（装法 / 跑法 / 五条可抄走的口径），
   `examples/README.md` 增条目与「测试破例引用 examples」的说明，`IDENTITY_CONTEXT_DESIGN.md`
   增「应用鉴权样板」指引段落（**不改任何机制**，只补用法）。
3. **回归测试**：`internal/artifact/auth_sample_test.go` 直接读样板 `bundle.yaml` 锁链路
   （见下「验收状态」）。它不另抄固件——另抄只会让测试变绿而样板失效。

**验收状态（2026-09-28 全部实测通过）**：AC-A1 / A2 / A3 / A5 由上述测试锁住；
AC-A4 由交付物构成保证（能力目录、REST、YAML schema、CLI、MCP 工具均为 +0，
本条**零 Go 非测试代码改动**）。

> 实测中定下来的两个设计（都有据可查，别再改回去）：
> - **守卫的密钥不能做入参**：守卫由 Hook 触发，入参只能来自 Flow step 的 `input`，
>   那样「直接 `POST /api/tools/<受保护>/run`」这条路永远拿不到密钥——而它是最常用的一条。
> - **Hook 必须写在 `bundle.yaml` 里**：产物解析只认三个文件名，`hooks.yaml` 这种单独文件
>   在 `gear import` 下会被静默丢弃，装上去的应用就没人拦了。

**验收标准（接口级断言）**

| # | 断言 |
|---|---|
| AC-A1 | 样板 `gear_import` 导入后端到端跑通：登录 Tool 返回令牌；带 Bearer 的受保护 Tool 调用成功且 Hook 读到 `ctx.caller.subject` |
| AC-A2 | 未带 Bearer / 令牌无效 → Hook 拒绝，执行**失败**（fail-closed，不是空结果放行） |
| AC-A3 | 样板含 JWT 路线演示：`crypto.jwt_verify` 验过的外部令牌可用作 Bearer 主体 |
| AC-A4 | **零内核改动**：本条不新增能力目录条目、不新增 REST 端点、不新增 YAML 字段、不新增 MCP 工具（既有件拼装） |
| AC-A5 | 凭据不出现在可观察面：错误信息 / 执行日志 / `gear_execution` with_logs 无明文口令与令牌本体 |

**接口面影响汇总**：能力目录 +0；YAML schema +0；REST +0；CLI +0；MCP 工具 +0；
新增 examples 样板一份 + 文档指引。**Q2 随本条失效**——没有 `auth.*` 原语可拦，
`gear_invoke` 维持既有口径（仅禁 `secret.*`），权限控制走 Hook（拍板原文如此）。

---

### FR-2 db 事务与 schema 迁移

> 「先易后难」两段走的第一段（GAP P0-2）：单调用多语句同事务，**无跨调用状态**。
> 句柄式 `db.begin/commit/rollback`（跨调用事务）明确**不在本批**，留后。
> **拍板（Q3）**：`db.batch` 接受 DDL（`CREATE/ALTER` 可与 DML 混批），文档写明方言差异。
> **拍板（Q4）**：迁移声明走 **bundle 新字段 `migrations:`**（`gear_apply` / `gear_import` 入口一致）。
>
> **进度（2026-09-28）**：FR-2 两段均已落地。
>
> 上半段 `db.batch` 通过 AC-B1~B4、B6（`internal/cap/sqldb.go` 的 `MultiDB.Batch` + `ctx.db.batch`
> Lua 绑定 + `TX_*` 错误码）。
>
> 下半段（AC-B5 迁移）按 Q4 落在 bundle 新字段：`kind: Migrations` 文档声明 `version/name/sql`，
> 由 `gear_apply`（含 dry_run）与 `gear_import` 在**落任何定义之前**执行（`internal/artifact/migrate.go`
> 的 `ApplyMigrations`）。三条硬口径：**已应用的版本不重跑**（台账 `gear_migrations` 建在目标库里）、
> **每条迁移单事务、失败即整条回滚且定义一个都不落库**、**dry_run 是真跑 + 全回滚**——
> 最后一条是特意选的：dry run 若只是跳过迁移，坏 SQL 就一路走到真导入，而真导入已经把前半截
> schema 写进了库。代价是 MySQL 的 DDL 会隐式提交，那条路上 dry run 兜不住，已在文档与
> `MIGRATE_SQL_FAILED` 的修法里写明。

**用户故事**

- US-B1（应用开发者）：我要用一次 `db.batch` 把「扣库存 + 写订单」两条 SQL 原子提交，
  以便中途失败不留脏数据（这是现在 `db.exec` 两次调用做不到的）。
- US-B2（应用开发者）：我要在应用 bundle 里声明版本化迁移（`migrations:` 字段），
  以便升级时 schema 自动演进，而不是每个应用自己 `CREATE TABLE IF NOT EXISTS` 满天飞。
- US-B3（智能体）：迁移随 `gear_apply` / `gear_import` 一起走原子语义，以便我交付的应用
  「导入即可用」，不要求用户手工跑 SQL。

**接口契约（调用方可见）**

```
ctx.db.batch({driver, dsn, statements:[{sql, params}, ...]})
  → {results:[每条语句的结果或错误], rows_affected:[n...], committed: true|false}
```

- **全或无**：任一语句失败 → 整批回滚，`committed:false`，`results` 指明第几条、什么错；
  调用方再 `db.query` **查不到任何部分写入**（这是可观察的验收口径，不是实现细节）。
- **DDL 混批合法**（Q3）：SQLite 同事务内 DDL+DML 原子；非 SQLite 方言差异在文档标注。
- **迁移声明（Q4，bundle 新字段）**：

```yaml
# bundle.yaml 可选顶层字段
migrations:
  - version: 1        # 严格递增，重复或断号 → 拒
    name: init-orders
    sql: |
      CREATE TABLE ...;
  - version: 2
    name: add-index
    sql: ...
```

- 迁移语义（需求级）：按 version 严格递增执行；**已应用的版本不重跑**（幂等，
  重复导入 / 重复启动 alike）；失败 → fail-closed，应用不以上一代 schema 启动；
  `gear_apply`（含 `dry_run`）与 `gear_import` 对坏迁移整包拒绝并点名版本号。

**验收标准（接口级断言）**

| # | 断言 |
|---|---|
| AC-B1 | 两条语句、第二条 SQL 语法错 → `committed:false`；随后 `db.query` 查不到第一条的写入 |
| AC-B2 | 全部成功 → `committed:true`，`rows_affected` 与语句一一对应 |
| AC-B3 | DDL+DML 混批：失败回滚时 DDL 效果同样不残留（SQLite 验证） |
| AC-B4 | 空语句数组 / 非法 driver 或 DSN → 结构化错误（错误码，非 panic）；DSN 不进错误明文输出（凭据保护，与既有 `db` 口径一致） |
| AC-B5 | bundle 带 `migrations:`：首次导入建表；二次导入（含 `dry_run` 与真跑）不重复执行；坏迁移（SQL 错 / version 断号）→ 整包拒绝并点名版本号 |
| AC-B6 | `gear_invoke` 直调 `db.batch` 可用（禁的是 `secret.*`，db 不在禁列）；`gear_capabilities` 的 db 族方法列表含 `batch` |

**接口面影响汇总**：能力目录 db 族 +1 方法（`batch`）；YAML schema：bundle +`migrations:`
可选顶层字段（进 `gear_validate` 校验）；REST / MCP 工具 +0；错误码 +`TX_*` / `MIGRATE_*` 系。

---

### FR-3 Tool 声明式 REST 路由

> 模型层改造，**不是原语**（GAP P0-5）。现状：所有 API 都是 `POST /api/tools/{id}/run`，
> 企业前端拿不到 RESTful 资源语义。

**用户故事**

- US-C1（前端开发者）：后端同事把 Tool 声明成 `GET /api/orders`，我就能按资源写标准客户端，
  以便不用记「所有东西都 POST 到 tools/{id}/run」这套内部约定。
- US-C2（智能体）：我 `gear_apply` 一份带 `api:` 字段的 Tool YAML 就能获得资源路由，
  以便构建的应用对外是正常 REST API。

**接口契约（调用方可见）**

```yaml
# Tool YAML 可选字段
api:
  method: GET|POST|PUT|PATCH|DELETE
  path: /api/orders            # 支持路径参数：/api/orders/{id}
```

- 声明了 `api` 的 Tool：引擎同时挂**资源路由**与既有 `POST /api/tools/{id}/run`（后者不变，向后兼容）；
  路径参数 `{id}` 映射进 Tool 输入；响应体即 Tool 输出；执行错误沿用既有错误码体系。
- 未声明 `api` 的 Tool：行为**一字不变**。
- 路由冲突（两个 Tool 声明同 method+path）→ `gear_validate` 与 `gear_apply` 均拒绝（fail-closed 前移到写作期）。
- `gen-client.mjs` 为声明了 `api` 的 Tool 生成**真 REST 方法名**（替代现在的内部 run 约定）。

**验收标准（接口级断言）**

| # | 断言 |
|---|---|
| AC-C1 | 声明 `GET /api/orders` 的 Tool：`curl GET` 返回该 Tool 输出；`POST /api/tools/{id}/run` 同时仍可用 |
| AC-C2 | 路径参数 `{id}` 正确映射进 Tool 输入（入参可见）；非法 method / path 模板 → `gear_validate` 字段级诊断 |
| AC-C3 | 路由冲突：`gear_apply`（含 `dry_run`）拒绝整包（原子语义不变），诊断点名两个冲突 Tool |
| AC-C4 | 未声明 `api` 的既有 Tool 与全量既有测试零变化 |
| AC-C5 | `gen-client.mjs` 对声明 `api` 的 Tool 生成 REST 方法（可被 `apps/mindstorm` 冒烟验证） |

**接口面影响汇总**：YAML schema +`api` 可选字段；REST 面 +资源路由（按声明数量）；
`gear_validate` +路由诊断；MCP 工具 +0；前端生成器升级。

**落地状态（2026-09-28）：已实现，接口面口径如下**

- `api` 是**加挂**：声明了资源路由的 Tool 仍然挂在 `POST /api/tools/{id}/run`（AC-C4 的红线）。
- 路径参数按名字进输入，值为字符串（不做类型转换）；`POST/PUT/PATCH` 的 JSON 请求体并入输入，
  且**路径参数优先**于 body 里的同名键。
- 响应体即 Tool 输出（不包 `{id, output}`）；执行错误沿用既有错误码体系（统一 400 面）。
- **不鉴权**：gear 没有用户库（Q1 已拍板：授权是业务的事，走 Hook）。资源路由只保证「长得像 REST」，
  真前门由业务 Hook 守。
- 路由校验前移到写作期的原因（不是审美）：net/http 对同名 pattern 的第二次注册是 **panic**，
  而注册发生在起服那一刻——一个 YAML 字段就能让整个服务起不来。故 `ROUTE_INVALID`（形状，
  字段级）与 `ROUTE_CONFLICT`（撞名，点名两个 Tool）在 `gear_validate` / `gear_apply`（含 dry_run）
  整包拒绝；跨 bundle 的第二个由 registry 入库前拒。
- 挂载方式取「每方法一条 `/api/{path...}` 通配挂载点 + 请求期按段匹配」而不是「每条声明注册一条」：
  后者会因「改声明、删 Tool、运行期新建 Tool」反复踩到同名 panic。代价是路径参数要自己按段取
  （`r.PathValue` 只认挂载点那个 `path`），好处是这些动作都不必重启。

---

## 4. 第二 / 三批需求（feature 级，验收后补详）

| # | 需求 | 用户故事（一句话） | 接口落点 | 批次 |
|---|---|---|---|---|
| E1 | `ext.oidc`（SSO 握手） | 用户从企业 IdP 单点登录我的 Gear 应用 | 侧车回调端点（复用 `/in/{path}` 入站能力）+ `crypto.jwt_verify`；**Q1 后会话也归应用业务层**，ext.oidc 只补「拿令牌」的握手（discovery / code 换 token / JWKS 长驻缓存）；**不改内核** | 二 |
| E2 | `ext.filestore` | 应用上传/下载附件（合同扫描件、头像） | 侧车方法 `put/get/stat/list/delete/url`；`root` 从 `ctx.config` 取，不硬编码（**2026-09-28 已实现**） | 二 |
| E3 | `ext.cron`（**Q5 已定默认语义**，见 §4.1；2026-09-28 已实现） | 每天凌晨对账、每月 1 号出报表 | `extensions.yaml` + `source: cron`；方法 `add/at/list/cancel`（`plugin/cron-sidecar`，硬绑定 + 幂等 + 只补最近一次 + overlap 默认跳过） | 二 |
| E4 | `ext.excel` / `ext.pdf`（2026-09-29 已实现，零依赖手写） | 管理系统导出报表是标配，现在完全没法做 | 侧车方法 `write`（`plugin/xlsx-sidecar` / `plugin/pdf-sidecar`，均 service ext，MCP 工具 +0）；CJK 嵌入字体子集；AC-E4-1~4 全过 | ✅ 2026-09-29 |
| E5 | `ext.redis` / `ext.oss` / `ext.dingtalk·feishu` | 桥接企业已有基础设施 | 侧车；**有外部依赖才做**。其中 **`ext.oss` 已于 2026-09-30 交付**（见上 §6 / README）：只做 S3 协议一套，**手写 AWS SigV4**，方法 `put/get/stat/list/delete/presign`。**`ext.redis` 已于 2026-09-30 交付**：KV/Hash/List/Set/ZSet 全数据结构 + 分布式锁 + 延迟队列（29 方法），**手写 RESP2**（不引 `go-redis`，保住 `go.mod` 零变动），单连接串行即原子（锁/队列不用 Lua）；`ext.dingtalk·feishu` / IM 仍按「有真实诉求再做」 | 三（oss ✅ 2026-09-30 · redis ✅ 2026-09-30） |

> 二批共同点：**不改内核**（`ext.cron` 仅加一个 source 枚举），四项互不依赖可并行。

### 4.1 E3 `ext.cron` 默认语义（Q5 拍板，选定的最优方案）

| 语义问题 | 选定默认 | 理由 |
|---|---|---|
| **幂等**（重复触发） | 按 `name` + 触发时刻（分钟粒度）唯一键去重，重复触发静默丢弃 | 调度器重启、时钟抖动下的重复是常态，去重是唯一不会出错的方向 |
| **misfire**（进程没开错过的触发） | **默认不补**，显式 `catch_up: true` 才补 | 补偿语义因业务而异（对账要补、通知不必补）；默认补可能造成触发风暴，默认不补最安全 |
| **overlap**（上次没跑完又到点） | **默认跳过**本次，显式 `overlap: true` 才并发 | 慢任务叠跑是企业定时任务最常见的事故源；跳过 + 日志点名是保守正确 |
| **生命周期（拍板：**硬绑定**）** | **应用进程退出 ⇒ 调度立即停止，一次都不接管**：侧车随 `app run` 起、随应用进程死而亡；应用重启后**不续跑**间隔期内错过的触发（要补须显式 `catch_up: true`） | 与 service ext 既有生命周期一致；「退出即停、重启不接管」把系统压成「有进程在跑」与「没进程」两态，不存在「表在、进程没了、谁在跑」的第三态；避免「应用死了调度还在打不存在的 Flow」 |

> **持久化表 ≠ 跨进程任务队列**。cron 表仍然落盘（记的是「注册了哪些任务」与去重键），
> 但它的唯一用途是让重启后的**新进程重建同一张时间表**，而不是把没跑完的触发搬过去。
> 硬绑定 + 默认不补（misfire）组合起来，才让「跑没跑过」这件事只有一个答案。

### 4.2 E1 `ext.oidc` — SSO 握手（mini-PRD）

**用户故事**

- US-E1a（应用开发者）：用户从企业 IdP 单点登录后，我的应用能拿到令牌与 issuer 元数据，
  而不必自己实现一套 OIDC 握手。
- US-E1b（智能体）：我调 `ctx.ext.oidc.code_exchange(...)` 就能把授权码换成令牌，
  以便 Flow 里几行完成接入。

**接口契约（调用方可见）**

```
ctx.ext.oidc.discovery({issuer})
      → {issuer, authorization_endpoint, token_endpoint, jwks_uri}
ctx.ext.oidc.code_exchange({issuer, code, redirect_uri,
                            client_id, client_secret, code_verifier?})
      → {access_token, id_token, token_type, expires_in, scope}
ctx.ext.oidc.jwks({issuer})   # 长驻缓存，命中不发起 HTTP
      → {keys: [{kid, alg, kty, ...}], stale: bool}
```

**三条职责边界（写死的，别改回去）**

1. **只做「拿令牌」的握手**：discovery / code 换 token / JWKS 长驻缓存与自动刷新。
2. **不验签**：验签是第 0 批 `crypto.jwt_verify` 的职责——ext.oidc 吐出的 `id_token`
   交给应用或守卫 Hook 去验。两个方向都别越界。
3. **不做会话**：Q1 已拍板（授权是业务的事）。`state`/`nonce` 由**调用方**生成并自行校验，
   侧车只把 state 原样带回；侧车不记任何登录态。

**其它口径**：issuer / client_id / client_secret / redirect_uri 一律**从调用参数传**
（ext 原子既有红线：配置从参数传，侧车不读环境变量）；入站回调端点复用既有的 `/in/{path}`
入站形态（同 `ext.webhook`）；HTTP 超时**统一 10s**（现有约定散在 3s/5s/10s 三处，本批拉齐）。

**验收标准**

| # | 断言 |
|---|---|
| AC-E1-1 | `discovery` 返回 issuer 元数据；`token_endpoint` 的 issuer 与入参不一致 → 拒绝（防 issuer 混淆） |
| AC-E1-2 | `code_exchange` 支持 PKCE（`code_verifier` 缺省透传）；授权服务器拒收 → 结构化错误、不吐令牌 |
| AC-E1-3 | `jwks` 长驻缓存：二次调用同一 issuer **不发 HTTP**；遇到未知 `kid` 自动刷新；刷新失败返回旧缓存并置 `stale:true`（fail-soft，但不静默）。**落地补口径**：`jwks` 因此需要可选的 `kid` 入参——不给就是「原样返回键集」，给了才保证「键集里真有这把」，否则「未知 kid 自动刷新」无处触发 |
| AC-E1-4 | issuer 不可达 / 超时 → 结构化错误而非 panic；超时严格 10s |
| AC-E1-5 | 拿回的 `id_token` 能用 `crypto.jwt_verify` 验签通过（样例里走通这条链路） |
| AC-E1-6 | **零内核改动**：能力目录只多 `ext.oidc` 一个能力名，不新增原语、不新增 REST 端点、MCP 工具 +0 |

---

### 4.3 E2 `ext.filestore` — 附件存取（mini-PRD）

**用户故事**：US-E2a（应用开发者）：我要上传/下载附件（合同扫描件、头像），
而不必每个应用各写一遍「文件路径 + 穿越防护 + 大小限制」。

**接口契约**

```
put({key, data_base64, content_type?, root?})  → {key, size, content_type, mod_time}
get({key, root?})                              → {data_base64, content_type, size, mod_time}
stat({key, root?})  list({prefix?, root?})  delete({key, root?})
url({key, root?})                              → {path}
```

**六条口径**

1. **root 不硬编码**：缺省读内核配置库（`gear config set filestore.root=...`，
   与 `ctx.config` 同源——侧车是独立进程，`ctx.config` 在侧车侧不存在，故直接取配置库）；
   调用参数显式传 `root` 优先。两者都没有 → fail-closed。
2. **路径穿越防护**：key 规范化后必须仍在 root 之下，否则拒；**不跟随符号链接**。
3. **大小上限 fail-closed**（默认 32 MiB，可被 root 下的配置放宽）：超限拒写，不落半截文件。
4. **二进制走 base64**：Lua 侧没有字节类型，`put` 收 `data_base64`、`get` 返回 `data_base64`。
5. **不做 MIME 嗅探**：content_type 信任调用方声明，缺省 `application/octet-stream`。
6. **`url` 只返回相对路径**（如 `/_filestore/<key>`），不新增内核 REST 端点——
   应用拿它去拼自己的反代前缀。**这条是「接口面 +0」红线逼出来的形状，别改成下发绝对 URL。**

**验收标准**

| # | 断言 |
|---|---|
| AC-E2-1 | put → get 往返一致（二进制 + 中文文件名）；`content_type` 保持 |
| AC-E2-2 | `../../etc/passwd` 形式的 key → 拒绝；root 外的既有文件经符号链接也取不到 |
| AC-E2-3 | 超过大小上限 → 拒写且磁盘上不留残留文件（fail-closed） |
| AC-E2-4 | root 未配置且调用未传 → 拒绝（fail-closed，不是落到 cwd） |
| AC-E2-5 | 零内核改动：能力目录 +`ext.filestore` 一个名，MCP 工具 +0 |

**落地状态（2026-09-28）：已实现，接口面口径如下**

- `root` 三档回退：调用参数 > 配置库 `filestore.root`（`gear config set`，与 `ctx.config` 同源）
  > fail-closed。侧车是独立进程，`ctx.config` 在侧车侧不存在，故取的是同一份 `config.db`。
- **安全基线三条**（ext 原子里第一次做路径安全）：key 段级校验 + 拼接后在 root 内
  + 不跟随符号链接；末一级是符号链接时 `delete` 也拒——跟着链接删会删到 root 外。
- 超限在**解码后、落盘前**判断，且写临时文件再改名，故超限不留残file。
- `content_type` 存在 `root/.filestore-index.json`（唯一需要跨调用记住的元数据），
  索引损坏时点名报错而非静默当空（否则已上传对象的类型会悄悄退化）。
- 真机判据：`plugin/verify.py` P1b 三项（往返 / 越界 fail-closed 无残留 / 超限 fail-closed）
  + 侧车单测 11 例。其中**符号链接那条在本机会跳过**：实测本机（沙箱）`os.Symlink` 会「成功」
  但建出的是一个 0 字节普通文件（`Lstat` 无符号链接位、`EvalSymlinks` 报错），建不出真符号链接；
  越界判定的几何核心（`withinRoot` / `resolveTarget`）已由纯函数用例无条件覆盖
  ——**假绿比没测更坏，所以这里如实跳过而不是假通过**。

---

### 4.4 E3 `ext.cron` — 定时触发（mini-PRD，默认语义见 §4.1）

**接口契约**

```
add({name, expr|at, payload, callback_url, timezone?, catch_up?, overlap?}) → {name, next_fire}
list({})                       → 已注册任务（含 next_fire / last_fire）
cancel({name})                 → 取消
```

**接口面影响**：`internal/engine/source.go` 的 `knownSources` 增加 `cron`（第二处：`extensions.yaml`
的 `source: cron`），错误文案同步——**三处必须同时改，漏一处就是「配置认、内核不认」的漂移**。

**验收标准**

| # | 断言 |
|---|---|
| AC-E3-1 | 5 段标准 cron + 一次性 `at`；非法表达式 → `add` 即拒（fail-closed 在注册期，不是等到点才炸） |
| AC-E3-2 | 幂等：同一 `name` 同一触发时刻的重复回推**静默丢弃**（去重键 = name + 触发时刻分钟粒度） |
| AC-E3-3 | misfire 默认不补：`catch_up` 缺省 false，进程重启后不补跑间隔期内的触发 |
| AC-E3-4 | overlap 默认跳过：上一轮未结束又到点 → 跳过并在日志点名；`overlap:true` 才并发 |
| AC-E3-5 | **硬绑定**：应用/侧车进程退出后，到点**没有任何回推**（测试以「kill 后等待触发时刻」证明）；重启后不接管 |
| AC-E3-6 | `source: cron` 进 `knownSources`；错误文案列出 cron |

**落地状态（2026-09-28）：已实现**，`plugin/cron-sidecar`（`main.go` + `schedule.go`，端口
`127.0.0.1:8097`）。三条语义都落进了代码而不是注释：`dispatch` 的去重键与 `overlap` 默认跳过、
`reschedule` 里 catch_up「只补最近一次」。硬绑定落在 `Stop()` / `stopped()` 两处：停掉之后
`loop` 与 `tick` 都据此早退，所以「停了之后哪怕时间走到触发点也不派发」是可断言的，不是靠约定。

实测中改掉的两个真 bug（都写进代码注释，别改回去）：

1. **`catch_up` 原本是死代码**。`nextAfter` 只会返回未来时刻，所以「用它的结果反推是否已经过去」
   恒为假——周期任务的补跑永远走不到。判断「错过了哪一次」必须独立往回扫，故新增与 `nextAfter`
   对称的 `prevBefore`。
2. **「at 落在当前这一分钟」被当成仍在等待，于是重启会补打**。这与硬绑定直接冲突：进程在触发
   时刻是死的，就该算错过（`catch_up` 未开 → 标 `expired`，不补）。这条是 `plugin/verify.py`
   真 kill 侧车再重启时跑出来的——端到端测试的价值就在这里。

`maxScanSteps` 取 8 年而非 1 年：闰日表达式的真实间隔可以到 8 年（2096-02-29 → 2104-02-29），
上限定 1 年会把**合法**表达式误判成无解，任务永远不触发。

---

### 4.5 E4 `ext.excel` / `ext.pdf` — 报表导出（mini-PRD）

> **已交付（2026-09-29）**：`plugin/xlsx-sidecar` + `plugin/pdf-sidecar` 两个 service ext 落地，
> 均 **MCP 工具 +0**（红线口径继续成立）。零第三方依赖：`go.mod` / `go.sum` 零变动（AC-E4-4，
> `git diff` 机械验证）；手写最小 OOXML 与 PDF 写出器；PDF 的 CJK 嵌入走**手写 TTF 子集化**。
> 验收 AC-E4-1~4 全过（`plugin/verify.py` P1e 段）。

**用户故事**：US-E4a（应用开发者）：我要导出一张 Excel 报表 / 一份 PDF 对账单，
而不用在应用里塞一个重量级依赖。

**关键决策：零第三方依赖**。手写最小 xlsx（`archive/zip` + XML）与最小 PDF 写出器。
理由不是癖好：`go.mod` 一变，`go.sum` 全量变动、`scripts/gate.sh` 的 GOPROXY 离线即挂，
而核心价值（能导出）与用哪个库无关。**副作用要如实登记**：只做「写出」，读回留后批。

**接口契约**

```
ctx.ext.excel.write({path, sheets:[{name, rows:[[...]]}], root?}) → {path, size}
ctx.ext.pdf.write({path, pages:[{text}], root?})                  → {path, size}
```

**验收标准**

| # | 断言 |
|---|---|
| AC-E4-1 | 生成的 `.xlsx` 能被第三方库（Python openpyxl 交叉验证，手法同第 0 批 PBKDF2 的交叉验证）正确读出——**不自己验自己** |
| AC-E4-2 | 生成的 `.pdf` 能被浏览器/阅读器打开；**CJK 需嵌入字体子集** |
| AC-E4-3 | 万行级表格不爆内存、不超 Tool 超时 |
| AC-E4-4 | **零 go.mod / go.sum 变动**（可 `git diff` 机械验证） |

**风险登记（已落地，非降级）**：CJK 字体子集化是 E4 里唯一可能失控的部分（零依赖手写要自己处理 TTF 子集）。
**已拍板方案（替代原「内嵌固定字体」降级项）**：运行时读取系统 TTF（候选首推 `simhei.ttf`），按页面实际用到的字符做**最小子集**嵌入
（只搬用到的字形、复合字形算依赖闭包），用 CIDFontType2 + ToUnicode + Identity-H——阅读器能正确选字、文本可复制；
字体缺字**直接报错、绝不画方框**（fail-closed）；TTC / OTF 直接报错（不假装支持）。该决策已在 §6 拍板记录 Q6 入册。

---

## 5. 非功能需求（横切，全部需求共用）

| # | 需求 | 验收口径 |
|---|---|---|
| NFR-1 | **零破坏默认** | Tool 未声明 `api`、bundle 无 `migrations:` 声明——两种「未选用」状态下，既有部署与全量测试一字不变（v1.1：`auth.db` 已取消，此条不再涉及） |
| NFR-2 | **fail-closed** | 批处理全或无；迁移失败应用不启动；路由冲突整包拒绝；Hook 鉴权拒绝即失败 |
| NFR-3 | **口径锁不破** | MCP 工具预算 15；`TestMCPWritesLeaveActorEmpty`；`ctx.caller` 无条件注入；capability 门禁语义；错误码单一真相源 |
| NFR-4 | **凭据不出现在可观察面** | 口令、令牌、DSN 不进错误信息、执行日志（`gear_execution` with_logs）、CLI 回显 |
| NFR-5 | **抗膨胀** | 能力目录进 AI 提示词：新增方法须过「语义内聚」判定，族 ≤6、每族方法 ≤10，例外须在 GAP §3.7.4 在册 |

## 6. 拍板记录（2026-09-28，v1.0 → v1.1）

| # | 问题 | 拍板结果 | 落点 |
|---|---|---|---|
| Q1 | auth 原语现在开工，还是先 Hook 路线样例验证？ | **不做 auth.db / `ctx.auth` 原语**：「能支持应用做授权就行了，那是业务上的事，走 Hook」 | FR-1 改判为「Hook 鉴权样板」；`AUTH_DESIGN.md` 转历史存档（已加注记）；GAP P0-1 / §5 已同步注记 |
| Q2 | `gear_invoke` 对 `auth.*` 的直调口径 | **放行**（「权限控制可以走 Hook」）——随 Q1 失效：没有 `auth.*` 可拦，维持既有「仅禁 `secret.*`」口径 | 既有口径不动 |
| Q3 | `db.batch` 接受 DDL 吗 | **接受**（DDL 可与 DML 混批），文档写明方言差异 | FR-2 契约 + AC-B3 |
| Q4 | 迁移声明放哪 | **bundle 新字段 `migrations:`**（`gear_apply` / `gear_import` 入口一致，产物格式随之演进） | FR-2 契约 + AC-B5 |
| Q5 | `ext.cron` 三语义 + 生命周期 | **选定最优**：name+时刻去重 / 默认不补（`catch_up` 显式）/ 默认跳过（`overlap` 显式）/ **生命周期硬绑定：应用退出即停、重启不接管**（2026-09-28 追加拍板，覆盖初稿的「重启后按 misfire 处理」） | §4.1 |
| Q6 | E4 `ext.pdf` 的 CJK 字体方案 | **运行时读系统 TTF + 按字符最小子集嵌入**（候选首推 `simhei.ttf`），替代原「内嵌固定字体」降级项——零依赖手写 TTF 子集化已验证可行（cmap 重排 / 复合字形闭包 / CIDFontType2 + ToUnicode + Identity-H）；**缺字即报错、绝不画方框**（fail-closed），TTC / OTF 直接报错 | §4.5 风险登记 |

## 7. 明确不做（划界，防滑向低代码平台）

- **auth 原语整套**（Q1 拍板）：`auth.db`、`ctx.auth.*`、Bearer 查会话表、CLI `gear auth *`——
  授权是业务的事，走 Hook + 应用自己的 db；原方案存档于 `AUTH_DESIGN.md`。
- 审批流语义、动态表单、报表 DSL、业务状态机、工作流设计器（含业务语义，留应用层）。
- OAuth2/OIDC **服务端**、完整 RBAC / 权限管理 UI、行级数据权限。
- `queue` / `lock` / `audit` / `db.query_page` 不做原语（GAP §3.6：db + Lua + 侧车就够）。
- 句柄式跨调用事务 `db.begin/commit/rollback`（本批不做，见 FR-2 开头）。

## 8. 里程碑

| 批次 | 内容 | 入口条件 | 状态 |
|---|---|---|---|
| 第 0 批 | 纯算法族扩容（jwt / password / id / validate） | — | ✅ 2026-09-28 交付，gate 全量 PASS |
| **第一批** | FR-1 Hook 鉴权样板（零内核）+ FR-2 db.batch+migrations + FR-3 REST 路由 | ✅ Q1~Q5 已拍板（本 v1.1） | ✅ 2026-09-28 交付：FR-1（`REQ-AUTH-SAMPLE`）/ FR-2a（`REQ-DB-BATCH`）/ FR-2b（`REQ-MIGRATE`）/ FR-3（`REQ-ROUTE`）四条需求均 `implemented`，gate 全量 PASS |
| 第二批 | E1–E4（ext.oidc / filestore / cron / excel·pdf） | ✅ mini-PRD 已补（§4.2~§4.5，验收 AC-E1~E4 四组） | ✅ 全部交付（2026-09-29 收口）：**E1 `ext.oidc`**（2026-09-28，`REQ-OIDC`）、**E2 `ext.filestore`**（2026-09-28，`REQ-FILESTORE`）、**E3 `ext.cron`**（2026-09-28，`REQ-CRON`，含 §4.1 硬绑定口径落地）、**E4 `ext.excel`/`ext.pdf`**（2026-09-29，零依赖手写 OOXML + 手写 TTF 子集，AC-E4-1~4 全过）；四项均 **MCP 工具 +0** |
| 第三批 | E5（redis / oss / IM） | 有真实外部依赖诉求 | 进行中：**`ext.oss` 已交付**（2026-09-30，listen 8102，六方法；verify P1f 覆盖六方法往返 + 坏 key/连不上 fail-closed + 样例 Tool `plugin/oss_demo.yaml` 经内核再跑一遍；签名正确性由 botocore 与 aws4 两个独立实现的黄金向量把关）。**`ext.redis` 已交付**（2026-09-30，listen 8103，29 方法；verify P1g 覆盖全数据结构 + 锁 + 延迟队列真实往返 + 坏 key fail-closed + 样例 Tool `plugin/redis_demo.yaml` 经内核再跑一遍；协议正确性由 `main_test.go` 内存假 Redis 把关）。**IM（钉钉/飞书）未开工**——仍按「有真实外部依赖诉求才做」 |

---

> **开工顺序建议**：FR-1 样板最便宜（零内核，先做能立刻验证 Hook 路线成色）→
> FR-2 `db.batch`（独立、验收清晰）→ FR-2 迁移 + FR-3（都要动 bundle / 模型层，收尾时一起过
> `gear_validate` 诊断面）。Q1~Q5 全部拍板，无剩余阻塞项。
>
> **第二批（开工顺序按成本排序，不按 E1~E4 表序）**：
> `ext.filestore`（最便宜，且顺带验证「service ext 新骨架」是否顺手）→ `ext.cron`（中等，
> 内核只动一行枚举但要单独评审）→ `ext.oidc`（中等）→ `ext.excel/pdf`（最贵，零依赖手写，
> 风险见 §4.5 登记）。四条互不依赖，可分批交付、逐条过闸门。
> **2026-09-29 收口**：E4（excel/pdf）已交付，第二批四条全部到位；E4 的 CJK 字体方案（运行时读系统 TTF + 子集化）见 §6 Q6。
>
> **已交付的 E1 / E2 / E3 / E4 均 MCP 工具 +0**（红线口径继续成立）：四个都是 service ext，只往能力目录
> 加 `ext.oidc` / `ext.filestore` / `ext.cron` / `ext.excel` / `ext.pdf` 五个能力名，不新增 MCP 工具、不新增内核 REST 端点。
