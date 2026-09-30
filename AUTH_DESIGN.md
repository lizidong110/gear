# AUTH_DESIGN.md — 身份 / 会话 / 权限原语 `auth`

> 状态：**已拍板否决（2026-09-28），转历史存档，未写码**
>
> ★ **拍板（ENTERPRISE_PRD.md v1.1 §6 Q1）**：不做 `auth.db` / `ctx.auth` 原语——
> 原话「不要 user.db 库，能支持应用做授权就行了，那是业务上的事，走 Hook」。
> 生效路线 = §2.1 零内核改动（Bearer / `X-Gear-Subject` + Hook fail-closed 拒绝）
> + 第 0 批已交付的 `crypto.password_hash/verify` 与 `crypto.jwt_verify`；
> 应用层交付物改为**官方鉴权样板**（`ENTERPRISE_PRD.md` v1.1 FR-1，examples + 文档）。
> 本文其余内容作为历史方案存档，供未来重新立项参考——§2.1 的证据链与 §1 的两层区分仍然有效。
>
> 前置：`IDENTITY_CONTEXT_DESIGN.md`（① 身份链路 / ② 守卫 / ③ 审计 / §9.4-§9.5 L3 评估）
> 场景来源：`ENTERPRISE_PRIMITIVE_GAP.md` P0-1
>
> **2026-09-28 第二版修订**：第一版把 auth 做成了「REST 中间件 + `--auth` 三态模式 + 端点分级」。
> 用户质疑「hook + 一些 auth 原语就够了」——**质疑成立，两处做重了**：
> 1. 漏了 `IDENTITY_CONTEXT_DESIGN.md` §3.6，那里已经定死：「身份不匹配 → 让 Hook 报错即可，
>    **不需要新增拒绝机制**」。我却在它之外又设计了一套 REST 中间件与模式开关。
> 2. 把「L3 共享安装的**平台面**访问控制」（§9.5 的问题）混进了「企业应用开发的**业务鉴权**」。
>    这是两个问题，前者要守的是 gear 自己的定义与密钥，后者要守的是业务数据。
>
> 本文档按这个区分重写为 **A / B 两层**：A 层走 Hook 路线（企业应用，现在做），
> B 层（平台面）降级为「等形态决策 + 最小解」。

---

## 1. 先把两个问题分开（本节是全文最重要的一节）

| | **A. 业务鉴权** | **B. 平台面访问控制** |
|---|---|---|
| 问的是 | 这个用户能不能跑这个 Tool / 改这条数据 | 谁能改 gear 自己的定义、密钥、起停应用 |
| 谁会碰 | 应用的**终端用户** | 管理员、运维、AI 智能体 |
| REST 路径 | `POST /api/tools/{id}/run`、`POST /api/executions` | `/api/tools` 写入、`/api/secrets*`、`/api/apps*`、`/api/flows\|hooks` 写入 |
| **经过引擎吗** | ✅ 经过 | ❌ **不经过** |
| **Hook 能拦吗** | ✅ **能** | ❌ **不能**（Hook 活在引擎执行期，不在 HTTP 中间件栈里） |
| 企业应用需要吗 | **必需** | 不需要（那是部署/运维的事） |
| L3 共享安装需要吗 | 必需 | 必需 |

**结论：对「开发企业常规应用」这件事，A 层就够了——用户的判断成立。**
B 层是另一个问题，不该绑在 auth 原语上一起做。

> 边界要如实说：Hook **拦不住**不经过引擎的端点。企业应用的终端用户只走 A 层的两条路径，
> 所以这个边界在企业应用场景下不构成问题；它只在「把 gear 的管理面直接暴露给别人」时才致命。

---

## 2. A 层：业务鉴权（企业应用，现在做）

### 2.1 今天就能用，零内核改动

证据链（已核实）：

| 事实 | 证据 |
|---|---|
| REST 业务执行面**已经在注入 caller** | `server.go:523` `engine.WithCaller(r.Context(), callerOf(r))`；`server.go:816` |
| Hook **读得到** caller | `callctx_test.go:150 TestHookCanSeeCaller`；`hook.go:115 hostFor(tool, rc)` |
| Hook **能拒绝**，且是 fail-closed | `hook.go:62-68 hookFail`：criticality 留空即 critical → 终止整次编排 |
| 承载主体从 Bearer / `X-Gear-Subject` 取 | `identity.go:44-52` |

**所以这条路今天就能走通**：应用前门（它自己的登录页）验完用户 →
把结果放进 `Authorization: Bearer <主体>` 或 `X-Gear-Subject` → Hook 里一行判断：

```lua
if ctx.caller.subject == "" then error("未认证") end
```

这也是 `IDENTITY_CONTEXT_DESIGN.md` §9.4 已经写下的定位——
「最终产品的鉴权必须发生在它自己的前门」。

### 2.2 那 auth 原语解决什么

2.1 那条路的洞是：**登录这件事每个应用都要自己写一遍**（存账号、发令牌、验令牌）。
auth 原语把这块收进内核，应用只写鉴权规则，不写账号体系。

### 2.3 要加的东西（很小，三块）

1. **`<GEAR_HOME>/auth.db`** —— `users` / `sessions` / `roles` 三张表
   （与 `config.db` 同级；开库照 `cap/configdb.go:66-81`，**必带 `_txlock=immediate`**）
2. **`ctx.auth.*`** —— 第 8 个受门禁原语（Tool 声明 `capabilities:[auth]`）
3. **`callerOf` 一处改造** —— `identity.go:40`：Bearer 从「不透明字符串」改成「查会话表」

**没有中间件、没有模式开关、没有端点分级。** 就这三块。

`ctx.auth` 方法集（snake_case，沿用 `ATOM_ROADMAP.md` §3）：

```
ctx.auth.login({account, password})                 → {token, expires_at, user:{id, account, roles}}
ctx.auth.verify({token})                            → {subject, user_id, roles, expires_at}
ctx.auth.logout({token})                            → {ok}
ctx.auth.user.create({account, password, roles})    → {id}
ctx.auth.user.list({status})                        → {users:[...]}
ctx.auth.user.disable({id|account})                 → {ok}
ctx.auth.perm.check({subject, action, resource})    → {allowed, reason}
```

### 2.4 开关：auth.db 存在即启用（不需要 `--auth` 参数）

| 状态 | Bearer 语义 | 结果 |
|---|---|---|
| **无 auth.db**（今日所有部署） | 不透明主体标识（**今日行为一字不改**） | 现有测试与应用全绿 |
| **有 auth.db** | 必须是有效会话令牌 | 无效 → **401**（不回落到不透明字符串，红线：不静默降级） |
| 有 auth.db 但**未带** Bearer | — | subject 空串（现状），**交给 Hook 拒绝** |

比三态模式简单，且天然零破坏——「有没有建过用户」就是开关。
（第一版的 `--auth=off\|strict` 取消。）

### 2.5 登录本身不需要新端点

★ 登录做成一个 **Tool** 即可：`POST /api/tools/{id}/run` 本来就走引擎，
Tool 声明 `capabilities:[auth]`，entry 里调 `ctx.auth.login(...)` 返回 token。
**连 REST 路由都不用新增。**

Hook 不会误伤它：Hook 的匹配维度是**具体 toolID / flowID**（`hook.go:39-57`），
没有全局通配 Hook，所以只要不给登录 Tool 绑 Hook，它天然不被拦。

### 2.6 内核自验与 Lua 调用要分开

- **内核自验**（`callerOf` 查会话表）：这是**身份链路的一部分**，不是 Tool 的能力调用，
  **不受 `capabilities` 限制**——否则没有声明 auth 的应用连 caller 都拿不到。
- **Lua 调用**（`ctx.auth.*`）：受门禁，未声明时 `ctx.auth` 不存在。
- `ctx.caller` 保持**无条件注入**（既有红线：鉴权门槛不能落在被鉴权方手里）。

### 2.7 令牌与口令

- **不透明随机令牌**（32 字节 base64url，库里查）。不做 JWT 自签——单机部署无横向扩展需求，
  而 JWT 吊销要额外黑名单，不透明令牌的吊销就是删一行。
  （`crypto.jwt_sign` 仍可作为纯算法**对偶**提供——供 gear 去对接「要求 JWT」的**第三方**接口；
   但**内核自己的会话令牌一律不透明、不自签 JWT**，故 auth 只依赖 `crypto.jwt_verify`。）
- **口令**：`crypto.password_hash/verify`（**纯算法族**，见 §3.7 归类）。默认 `bcrypt`（cost 12，
  需 `golang.org/x/crypto`）；若要零新增依赖，可先只做 pbkdf2-hmac（标准库 HMAC 迭代即可）。
  绝不自己写哈希，`secretstore` 是明文内存**不能复用**，日志绝不打令牌/口令。
- **subject = `account`**（唯一且不可改；改名走 `display_name`）。审计要可读+稳定，
  与 CLI 侧 actor = OS 用户名同类。

---

### 2.8 协议层交给 `ext.oidc`：内核只管会话（2026-09-28 补，回应用户立项动机）

用户的原话是「**jwt、单点登录在 lua 里做不方便**」。这条诉求成立，但落点是 ext 不是内核：

> **2026-09-28 再次修正**：上一版把「RS/ES 非对称验签」也归给侧车，**判错了**。
> 验签是冻结标准（RFC 7518）、标准库 `crypto/rsa|ecdsa|ed25519` 就有、无需长驻 ⇒ 归**纯算法族**。
> 归类判据见 `ENTERPRISE_PRIMITIVE_GAP.md` §3.7（四轴：A 副作用 / B 跨调用状态 / C 依赖成本 / D 稳定性）。

| 环节 | 谁做 | 理由 |
|---|---|---|
| **RS/ES/EdDSA 验签** | **`crypto.jwt_verify` 纯算法** | 标准库即可；D 冻结（RFC 7518）。**不进侧车** |
| **claims 校验 exp/nbf/iss/aud** | **`crypto.jwt_verify` 纯算法** | 最容易写漏的部分，不该留给每个应用各写一遍 |
| JWKS 拉取 + 缓存 + 轮换 | `ext.oidc` | B✓ 需要长驻与缓存；Lua Tool 每次执行都是新环境 |
| OIDC 三步 + `state`/`nonce` | `ext.oidc` | D✓ 厂商实现，多步协议不是单操作原子 |
| redirect_uri 回调端点 | `ext.webhook` 入站路由 `/in/{path}` | Lua runtime 不支持 service（`lua.go:49`），监听不了端口；webhook 侧车已有入站能力 |
| **把身份变成会话** | **内核 `ctx.auth`** | `callerOf`（`identity.go:40`）是内核代码路径，只有它能填 `cap.Caller` |

**接口（两条路径，都不需要侧车也能跑第一条）**：

1. **只验令牌**（最常见）：前端拿 IdP 签发的 id_token 直接打到 gear →
   `crypto.jwt_verify({token, jwks, iss, aud})` → `ctx.auth.session.grant(...)`。
   **JWKS 由调用方传入**（既有口径：密钥由调用方传，不读 env），可自行落 `ctx.config` 做缓存。
2. **完整登录跳转**：`ext.oidc` 走 discovery → 授权 → code 换 token → 回调 → 同样 `session.grant`。

★ 两条路径**汇流成同一个 subject**，Hook 只需判断一个东西。

> 分工一句话：`crypto` 负责「**验**令牌」，`ext.oidc` 负责「**拿**令牌」，`ctx.auth` 负责「**记住**你是谁」。
> 计算在内核（标准冻结、零依赖），协议在侧车（厂商各异、要长驻），会话在内核（稳定、要被 `callerOf` 读到）。

（`session.grant` 的调用权限细节——谁能代表谁建会话——等 `ext.oidc` 落地时定，不在此预判。）

---

## 3. B 层：平台面访问控制（降级，等形态决策）

Hook 够不着的那几条端点。企业应用**不需要**它（终端用户只走 A 层两条路径）。
按代价从小到大：

| # | 做法 | 代价 | 说明 |
|---|---|---|---|
| 1 | **部署约定**：`ext.serve` 的 `proxy` 只反代 `/api/tools/{id}/run` 与 `/api/executions`，**不反代整个 `/api`** | **零** | 管理端点根本不暴露。mindstorm 现在反代的是 `/api` 全前缀（`mindstorm_flow.yaml:33`），改成两条明确规则即可 |
| 2 | 注册表写入 + secrets 端点加一层 admin 守卫 | 小 | 只守这两类，不是完整 RBAC |
| 3 | 完整中间件 + 端点分级 + `--auth` 模式 | 中~大 | **只在 L3 共享安装时才做**，等 §9.5 的形态决策 |

**推荐先做第 1 条**（改一行 yaml），把第 3 条整个推到形态决策之后。

---

## 4. 对照第一版改了什么

| 项 | 第一版 | 第二版 | 为什么 |
|---|---|---|---|
| 拒绝机制 | REST 中间件 + 端点分级 | **Hook 报错**（`hookFail` 已有） | §3.6 已定「不需要新增拒绝机制」 |
| 模式开关 | `--auth=off\|strict` + `--allow-remote` 强制 | **auth.db 存在即启用** | 天然零破坏，少一个参数 |
| 登录入口 | 新增 `POST /api/auth/login` 白名单端点 | **做成 Tool**，不新增端点 | 走既有执行面即可 |
| 平台面 | 与 auth 绑定，同一期做 | **拆成 B 层**，最小解是改 yaml | 那是另一个问题（§1） |
| MCP 面 | `as` = 会话令牌 | 不变（§3.5 已定） | — |
| 工作量 | L | **S~M** | — |

---

## 5. 剩下的真问题（不假装解决）

- **`X-Gear-Subject` 可伪造**（`identity.go:38`，同机任意进程可填）。
  有 auth.db 时建议**只认 Bearer**，subject 头降级为「网关已认证」场景专用。
- **MCP 面**仍按 §3.5：`as` 传会话令牌，verify 过才采信；
  **不传仍恒空 → `TestMCPWritesLeaveActorEmpty` 一字不用改**。
- **`cap.Caller.AppID` 仍为空** ⇒ 一进程多应用的身份隔离仍然没有（§8.6 之 3，独立工作）。
- **`/api/events/{handle}`** 外部回推在 B 层之外，仍是未认证可达；机器身份 `svc:<name>` 二期。
- **自举**：auth.db 无用户时 `user.create` 免鉴权且**只允许一次**（创建后自动带 admin），
  单测锁死「第二次必须 admin」。

---

## 6. 分阶段

| 阶段 | 内容 | 破坏性 | 依赖形态决策 |
|---|---|---|---|
| **A1** | `auth.db` + `ctx.auth` 原语 + CLI `gear auth *` | **零**（默认无 auth.db，行为不变） | ❌ |
| **A2** | `callerOf` 改造（有 auth.db 时 Bearer 查会话，无效 401） | 零 | ❌ |
| **A3** | `perm.check` + role 三档（admin/editor/viewer，未知即拒） | 零 | ❌ |
| **B1** | 应用侧：只反代业务端点（改 yaml）+ Hook 鉴权样板 | 零 | ❌ |
| **B2** | 平台面 admin 守卫 | 小 | ✅ |
| **B3** | 完整中间件 + 端点分级 | 中~大 | ✅（L3 才做） |

> **A1–A3 + B1 全部零破坏、不依赖任何形态决策，可以立刻做。**
> 这也是「企业应用」场景下真正需要的全部内容。

---

## 7. 不做

- **不做 OAuth2/OIDC 服务端**：Gear 不向第三方发行令牌。
- **不做完整 RBAC / 权限管理 UI**：role 是代码里的枚举，入库即滑向低代码平台。
- **不做行级数据权限**（「只能看本部门数据」）：业务语义，留给应用 Hook。
- **不改** `ctx.caller` 无条件注入、actor 空串口径、`TestMCPWritesLeaveActorEmpty`。
- **不新建 audit 表**：`IDENTITY_CONTEXT_DESIGN.md` §10.4 已定，`definition_history` 就是那张表。

---

## 8. 待拍板

只剩一个：**A1–A3 现在开工，还是先做一个企业应用样例（用 2.1 的零改动路线）验证 Hook 鉴权够用再补原语？**

- 选前者：直接把账号体系收进内核，应用只写规则。
- 选后者：先用现有 `callerOf` + Hook 跑通一个真实应用的登录鉴权，
  把「到底缺哪几个方法」摸准了再落原语——**更省，且不会做出没人用的方法**。

我倾向**后者**：2.1 已经证明 Hook 路线今天可用，先拿真实应用压一遍，
`ctx.auth` 的方法集就会由实际需求决定，而不是由我这份文档猜。
