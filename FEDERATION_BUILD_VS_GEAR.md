# 数据联邦：从零实现 vs 改 Gear 支撑 —— 权衡分析

> 文档定位：本文**先抛开 Gear 现有能力**，纯从「面向对象数据联邦」要解决的问题出发，给出从零实现所需的完整架构；
> 再回头看 Gear 能做什么改动来支撑；最后对「独立自建 / 基于 Gear 做 App / 改 Gear 内核」三条路线做利弊权衡。
> 配套 PRD：`ONTOLOGY_FEDERATION_PRD.md`（v0.3，已收敛到 App 形态）。本文是**架构决策层**，不重复 PRD 的细节。
>
> 关键事实（已核查）：Gear 的 `Tool` 支持 `runtime: process`（`internal/engine/engine.go` 的 process runtime），
> 配合 `mode: service` 可**长驻运行任意二进制**；Gear 通过 stdin/stdout 的 JSON-RPC 把 `ctx`（含 capability 声明、secret 引用、sandbox 信息）注入子进程
> （`internal/engine/callctx_test.go:115` 验证 process 子进程能从 stdin 读到 ctx）。这意味着**联邦引擎可以是一个独立的常驻进程，Gear 只做宿主**。

---

## 0. 结论先行

1. **联邦引擎本质是一个独立的「数据平台服务」**，核心 IP 是*查询规划器（决定下推/合并/扇出）+ 多源连接器 + 治理层*。这部分无论基于什么宿主都要自己写，与 Gear 无关。
2. **Gear 不需要为联邦做任何内核改动即可充当合格宿主**。`runtime: process` + `mode: service` + ctx 注入 + `static_site` 反代 + `ext.redis` 已覆盖「进程托管 / 凭据透传 / UI 托管鉴权 / 缓存」四件事。
3. **唯一值得做的两个 Gear 小改都是「宿主体验」层面**（bundle 携带二进制、service 健康检查），不是平台能力。改内核加「联邦查询原语」会直接撞 7 受门禁原语 / 15 MCP 工具 / 单 exe 三条红线，否决。
4. **推荐路线 = 在 Gear 上做 App（路线 A）**：引擎是独立二进制，技术栈自由，复用 Gear 的进程生命周期与 UI 托管，内核零改动。这与 v0.3 PRD 的收敛方向一致，本文把它坐实并关闭「改内核」选项。

---

## 1. 抛开 Gear：从零实现需要什么

把诉求拆成 8 个硬需求，它们决定子系统划分：

| # | 诉求 | 对应子系统 |
|---|------|-----------|
| R1 | 用户定义对象及属性 | 元数据/本体层 |
| R2 | 属性映射到各种数据源 | 映射层 + 连接器层 |
| R3 | 面向对象管理孤岛数据（不搬数据） | 查询引擎（虚拟化） |
| R4 | 建立对象之间联系 | 链接类型 + 图遍历 |
| R5 | 对象分类、分类间也有联系 | 分类树 + 分类链接 |
| R6 | 分类支持树（任意深度下钻） | 分类存储（闭包表） |
| R7 | 源有访问频率/时段限制 | 治理层（限流/时间窗） |
| R8 | 是否加 cache | 治理层（缓存 + 新鲜度契约） |

### 1.1 五个子系统

```
┌─────────────────────────────────────────────────────────────┐
│  S5 暴露层  REST / GraphQL / 建模 Studio UI / 浏览 UI         │
├─────────────────────────────────────────────────────────────┤
│  S4 治理层  限流账本 · 时间窗 · 双 TTL 缓存 · 熔断 · 新鲜度契约 │
├─────────────────────────────────────────────────────────────┤
│  S3 查询引擎  Parser → Planner(下推/合并/扇出) → Executor      │
│            + 类型调和 + 结果装配(带 as_of/staleness)          │
├──────────────────────┬──────────────────────────────────────┤
│  S2 连接器层          │  S1 元数据/本体层                      │
│  SQL·Mongo·REST·ES   │  ObjectType/PropertyType/LinkType     │
│  ·File 各源 driver    │  Category树/CategoryLink/Source/Mapping│
└──────────────────────┴──────────────────────────────────────┘
        支撑：Secret 库 · 审计日志 · 调度器(warmup/刷新)
```

#### S1 元数据/本体层（R1/R2/R4/R5/R6）
这是「面向对象」的落点，必须是一个**有版本、可 CRUD、可导出**的注册表。核心实体：

```yaml
DataSource:        { id, type: postgres|mysql|sqlite|mongo|rest|es|file,
                     conn: {...}, credential_ref: secret://src_x, allowed_hours?, rate_limit? }
ObjectType:        { id, name, display_name, description,
                     properties: [PropertyType], primary_key: [prop_id] }
PropertyType:      { id, name, data_type: string|int|float|bool|datetime|enum|geo|json,
                     unit?, nullable?, description }
LinkType:          { id, from: ObjectType, to: ObjectType, cardinality: 1|*,
                     join: { on: <from.pk>, eq: <to.fk> } }   # R4 对象间联系
Category:          { id, parent_id?, name, path(materialized), closure表 }  # R5/R6 树
CategoryLinkType:  { id, from_cat, to_cat, relation }          # R5 分类间联系
Mapping/Binding:   { object_type, property, source, loc: table|collection|endpoint,
                     expr: <列/字段/表达式>, strategy: pushdown|merge|fanout }   # R2
```

分类树存储用 **邻接表 + path 物化 + 闭包表** 三件套：邻接表管父子，path 物化支持 O(1) 祖先/前缀下钻，闭包表支持「取某节点下所有子孙」与多父（DAG）扩展。

#### S2 连接器层（R2/R3）
每种源一个 driver，职责是把「规范化子查询」翻译成源原生请求并取数：
- **SQL 源**：按方言（Postgres/MySQL/SQLite/SQLServer）生成带 `WHERE`/`LIMIT` 下推的 SQL。
- **Mongo**：翻译成 aggregation pipeline（`$match`/`$lookup`/`$limit`）。
- **REST**：拼 path/query/body，参数绑定（来自 filtering 条件）。
- **ES**：生成 query DSL。
- **File(CSV/Parquet/Excel)**：加载后在内存过滤（小源可行，大源需物化，超出虚拟化前提）。
- 统一返回**规范化行集**（列名 = 规范属性名，类型经 S3 类型调和）。

#### S3 查询引擎（R3/R4，最难的核心）
给定查询（对象类型 + 过滤 + 要取的属性 + 要遍历的链接），输出对象图。流程：

```
Parser      : 把请求解析成逻辑计划
              { fetch: ObjectType, filter: {...}, fields: [prop...], traverse: [LinkType...] }
Planner     : 对每个 ObjectType：
   1. 按 Mapping 把 fields 按 source 分组
   2. 若某组覆盖全部 fields 且来自单源 → 该源 emit 一条原生查询（PUSHDOWN，最优）
   3. 若跨多源 → 每源各 emit 一条「仅取 pk + 本源属性」查询，按 pk 内存 keyed-merge（MERGE）
   4. 对 traverse 的 LinkType → 解析 join(pk↔fk)，递归 Planner 目标类型（FANOUT 仅当目标是 http/tool 通道）
   5. Cost 估计：用源端统计/采样估行数，决定 pushdown vs merge（避免把大表拉回内存）
Executor    : 并行跑子查询（受治理层限流/时间窗约束）→ 合并 → 后过滤 → 分页
Assembler   : 装配对象图，逐字段打 source + as_of + staleness（R8 新鲜度契约）
```
**长杆是 Planner 的 cost 估计**：决定 pushdown 还是 fetch-merge，这是联邦引擎真正的 IP，也是从零实现最贵的部分（Trino/Calcite 花十年磨这块）。

#### S4 治理层（R7/R8，上一轮新增诉求）
对**每个 DataSource** 独立施加，且与正确性强耦合：
- **限流账本**：每源 token bucket / 日配额（Redis 或 DB 行锁 `UPDATE ... WHERE quota>0`）。
- **时间窗**：`allowed_hours` 外默认 `cache_only`；由此**强制有窗口的源必须开缓存 + 配 warmup**（在允许窗口用调度器预刷热点），否则窗口外全红。
- **双 TTL 缓存**：`fresh_ttl`（新鲜窗口）+ `max_stale`（fail-closed 硬上限）；`single-flight` 防惊群；响应必带 `as_of/staleness_sec/stale/origin`，漏带即视为缺陷。
- **熔断**：单源连续失败 → 快速失败，避免雪崩。

#### S5 暴露层
- **REST**：`GET /objects/{type}`、`/objects/{type}/{id}`、`/links/{type}/{id}/{link}`、`/categories/tree`、`/search`。
- **GraphQL**：对象+链接天然是图，GraphQL 比 REST 更贴合（前端按需取属性与链接）。
- **建模 Studio**：非工程师定义对象、画链接、搭分类树、把属性拖到源字段（R1/R2/R4/R5 的入口）。
- **浏览 UI**：探查联邦后的对象实例。

#### 支撑设施
Secret 库（不落明文）、审计/执行日志、调度器（warmup + 周期刷新）。

### 1.2 技术选型（从零的三种姿势）

| 姿势 | 做法 | 优点 | 缺点 | 适用 |
|------|------|------|------|------|
| **A 纯自研** (Go/Rust/Node) | 自己写 Planner+连接器+类型系统 | 完全可控、可单二进制、不绑重依赖 | Planner/cost 估计是数月工程 | 想守「单 exe 零重依赖」 |
| **B 站在联邦引擎上** (Trino/Calcite/**DataFusion**/Ontop) | 复用 SQL 规划与下推 | 不重造 Planner；DataFusion 是 Rust 库可嵌、单二进制友好 | JVM 系(Trino/Ontop)违背单 exe；只覆盖 SQL 源，Mongo/REST/ES 仍要自写 | 关系源为主 |
| **C 混合**（推荐） | DataFusion 管 SQL 下推 + 自研对象/链接/分类层 + 自研非 SQL 连接器 | 难的（SQL 规划）用现成，特的（对象图/治理）自己控 | 集成复杂度中等 | 大多数真实场景 |

> 现实结论：**关系源用 DataFusion 类引擎做下推，对象/链接/分类/治理层自己写**。这正是 Palantir 那一路「语义层 + 虚拟 ABox」的实现骨架，只是我们明确不引 JVM/Python 重依赖。

### 1.3 工作量估计（从零，独立团队）
- **MVP**（单 SQL 源、纯下推、无治理、基础浏览 UI）：2–3 人 · ~3 月。
- **v1**（多 SQL 源 + keyed-merge + 链接遍历 + 分类树 + REST + 基础建模 UI）：再 +4–6 月。
- **v2**（Mongo/REST/ES 连接器 + 治理层 + GraphQL + 新鲜度 + warmup）：再 +6–12 月。
- 长杆 = Planner 的 cost 估计与跨源合并的正确性/性能。

---

## 2. Gear 现状已经能提供的（无需改动）

| 联邦所需 | Gear 现成能力 | 证据/说明 |
|----------|--------------|-----------|
| 常驻后端进程（持内存状态：限流计数/连接池/缓存句柄） | `runtime: process` + `mode: service` | engine.go process runtime；mindstorm 用 `entry: "gear serve ..."` 起常驻进程 |
| 进程间安全上下文（capability 声明、secret 引用、sandbox） | ctx 经 stdin JSON-RPC 注入 | callctx_test.go:115 验证 process 子进程读到 ctx |
| UI 托管 + 反代 + 前门鉴权 | `static_site.serve` + `proxy` + Hook | mindstorm 把 www 挂 8090、`/api` 反代到 8790 |
| 缓存（跨实例 TTL/淘汰/single-flight） | `ext.redis`（已注册、产物在 build/plugin） | 只需 App `requires: [redis]` |
| 凭据不落明文 | `secret` 库（CLI `gear secret set` / REST `PUT /api/secrets`） | 引擎经 ctx 拿 secret 引用，不碰明文 |
| 声明式部署/导出 | bundle `apply`/`export` | 引擎二进制 + 定义 YAML 一体分发 |
| 进程生命周期（重启、端口分配） | App `start.restart: always` + `ports` | — |

**关键判断**：上一轮 PRD 担心的「Lua 每请求新 VM、内存状态不保留」只在**用 Lua 写引擎**时成立。一旦引擎是 `process` 常驻二进制，这个问题消失——状态在自己进程里。所以「治理状态必须落 SQLite/Redis」的回归锁，在 process 引擎下只需落 Redis/自有 DB，不必强依赖 Lua 全局表。

---

## 3. Gear 可以做的改动（候选清单 + 裁决）

逐条给出成本/收益/裁决。口径：**凡是「数据平台能力」都拒绝进内核**（撞 7 原语 / 15 工具 / 单 exe 红线）；**凡是「宿主体验」可酌情加**。

| # | 候选改动 | 收益 | 成本/风险 | 裁决 |
|---|----------|------|-----------|------|
| C1 | **让 bundle 携带可执行二进制**，`runtime: process` 可引用随包分发的 exe | 引擎 App 自包含部署，不依赖目标机预装 | 二进制跨 os/arch、签名、体积 | **值得做（宿主体验）** |
| C2 | **service 模式健康检查 + 稳定端口/重启退避** | 引擎进程挂了能被探活重启，韧性↑ | 小，需定义 health endpoint 约定 | **值得做（宿主体验）** |
| C3 | 内核加「数据源注册表」（中心化 Source + 凭据） | 多 App 共享源定义 | 把数据平台配置塞进工具运行时；与「每用户一份 GEAR_HOME」口径冲突 | **不做，App 层自有 SQLite 解决** |
| C4 | 内核加「对象/链接/分类」一类 registry 对象 | 复用删除守卫/版本/export | 等于在内核造第二个产品（Planner/连接器/治理全要进 Go） | **不做** |
| C5 | 新增「联邦查询」门禁原语 `ctx.fed.*` | MCP 面直接查对象 | 撞 7 受门禁原语红线；查询应走引擎自身 REST | **不做（P0 否决）** |
| C6 | 让 `gear_invoke` 变轻（高频读通道） | 误以为能拿 MCP 当查询 API | 每次 invoke = 执行+日志，千次读=千事务；且 MCP 工具锁 15 个 | **不做** |
| C7 | 内核内置 GraphQL/REST 自动生成 | 对象定义即端点 | 把暴露层做进内核 = 内核变 data platform | **不做** |
| C8 | 加通用 `ext.cache` 原语 | 统一缓存 | `ext.redis` 已覆盖；多一个原语多一份审计 | **不做（用 ext.redis）** |

**裁决汇总**：只有 **C1、C2** 值得做，且都是宿主质量，不碰内核语义。其余一律否决——理由不是「做不到」，是「做了就把 Gear 从工具运行时变成数据平台，与定位冲突且代价远超收益」。

---

## 4. 三条路线权衡

注意：**路线 A 与 B 里，联邦引擎都是你自己写的独立进程**（Planner/连接器/治理不变）。差异只在「引擎放哪、Gear 改不改」。

### 路线 A：基于 Gear 做 App（推荐）
引擎 = `runtime: process` 常驻二进制；定义存引擎自有 SQLite；UI 走 `static_site`；缓存走 `ext.redis`；凭据走 `secret`。

- ✅ 内核零改动，不碰三条红线。
- ✅ 引擎技术栈自由（Go/Rust + DataFusion），不被 Lua 无状态束缚。
- ✅ 复用 Gear 进程生命周期、ctx 凭据透传、UI 托管+鉴权反代、bundle 部署。
- ✅ 与 v0.3 PRD 收敛方向一致。
- ⚠ 多 App 共享同一批源时需各自配（或引擎自带注册表）——但这是 data-platform 诉求，Gear 本就不该管。
- ⚠ 引擎二进制分发靠 C1 才自包含。
- ⚠ Gear 与引擎集成面（stdin JSON-RPC + HTTP 反代）要自己写，但已验证可行。

### 路线 B：改 Gear 内核加联邦能力
把 S1–S4 做进 Gear（新增 registry 对象 + 查询原语 + 连接器框架 + 治理）。

- ✅ 数据源/对象成为一等 Gear 对象，export/import、删除守卫、版本天然复用。
- ❌ 要往内核塞 Planner+连接器+治理+类型系统 = 第二个产品，与「工具运行时」定位冲突。
- ❌ 直接撞 7 受门禁原语 / 15 MCP 工具 / 单 exe 三条红线。
- ❌ 引擎逻辑锁进 Go 内核，无法用 DataFusion 等现成部件，演进慢。
- ❌ 改内核 = 每次 gate.sh 全套 + 回归面爆炸（见工程记忆里那堆闸门教训）。
- **裁决：不划算，除非 Gear 路线图自己想变成 data platform（不在其列）。**

### 路线 C：完全独立（不基于 Gear）
引擎独立部署成服务，Gear 不参与。

- ✅ 完全自由，可引 Trino/DataFusion/Ontop，可做多租户 SaaS。
- ❌ 失去 Gear 已给的进程托管/secret/UI 鉴权/bundle 部署，全要自己造。
- ❌ 与用户「就在 gear.exe 上做应用」的诉求背离。
- **裁决：仅当用户想要独立 SaaS 产品时才选。**

### 权衡矩阵

| 维度 | A 基于 Gear 做 App | B 改 Gear 内核 | C 完全独立 |
|------|-------------------|---------------|-----------|
| 内核改动 | 0（仅可选 C1/C2） | 大（新 registry+原语+连接器） | 0 |
| 撞红线风险 | 无 | 高（7 原语/15 工具/单 exe） | 无 |
| 引擎技术自由度 | 高 | 低（锁 Go 内核） | 最高 |
| 复用 Gear 宿主能力 | 全复用 | 天然一体 | 无 |
| 多 App 共享源 | 需自管 | 天然 | 天然 |
| 交付速度 | 快（宿主现成） | 慢（造内核） | 中（自造宿主） |
| 符合用户诉求 | ✅ gear.exe 上做应用 | 过度工程 | ❌ 背离 |

---

## 5. 推荐与待拍板

**推荐：路线 A。** Gear 内核零改动（仅建议顺手做 C1 bundle 携带二进制、C2 service 健康检查两个宿主小改）。联邦引擎是独立常驻进程，技术栈自由，复用 Gear 的进程托管/凭据透传/UI 托管/缓存。

**待拍板（在写代码前需你定）：**

| ID | 问题 | 选项 |
|----|------|------|
| Q1 | 虚拟化跨 3 源延迟能否接受 1–3s？不能则必须物化，整体转向 M1（成本数量级上升） | A 接受虚拟化 / B 要求 <500ms→物化 |
| Q2 | 引擎技术栈 | A Go（与 Gear 同语言，易调试）/ B Rust+DataFusion（SQL 下推强）/ C Node（UI 同栈） |
| Q3 | 关系源为主还是含 Mongo/REST/ES | A 仅 SQL（MVP）/ B 含非 SQL（v2） |
| Q4 | 治理介质 | A 引擎自有 SQLite / B Redis（跨实例配额+single-flight）/ C 先 A 后 B |
| Q5 | 分类严格树还是 DAG（多父） | A 严格树（默认，path 下钻简单）/ B DAG（邻接+闭包双表+环检测） |
| Q6 | 暴露接口 | A REST / B GraphQL（对象图更贴合）/ C 两者 |
| Q7 | Mongo 作为「被联邦的源」是否一期要 | A 仅作源（需造 `ext.mongodb` 或走 http 兜底）/ B 一期不接 |

**一句话收口**：你需要的联邦，内核层面 Gear 已经够用了；真正要写的是那个独立引擎（Planner+连接器+治理），它跑在 Gear 上就像 mindstorm 跑在 Gear 上一样自然。改 Gear 内核是过度工程。

---

## 附录：关键事实核对（已验证，非推测）
- `runtime: process` 存在，`model.RuntimeProcess`（`internal/engine/engine.go`），`entry` 为任意命令。
- process 子进程经 stdin 收到 `ctx`（含 capability/sandbox/caller），`internal/engine/callctx_test.go:115` 验证。
- `static_site.serve` 的 `proxy` 把 `/api` 反代到常驻进程（mindstorm bundle.yaml）。
- `ext.redis` 已注册且产物在 `build/plugin/`（`ENTERPRISE_PRIMITIVE_GAP.md` 与 v0.3 PRD 已核）。
- `secret` 库只写不回明文（`internal/mcp` 面禁 `secret.*` 是刻意设计）。
- 7 受门禁原语红线 + 15 MCP 工具锁（`internal/mcp/tools_surface.go:18-32`、`ATOM_ROADMAP.md` §0）。
