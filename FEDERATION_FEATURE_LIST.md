# 面向对象数据联邦应用 —— 最终功能清单（路线 A）

> 本文是 `ONTOLOGY_FEDERATION_PRD.md`（v0.5）的**功能落地版**，基于路线 A（在 Gear 上做 App `apps/federation`，内核零改动）。
> 它把 PRD 的 UR/F 收敛为**可交付、可排期的功能清单**，作为实现的直接依据。
>
> **决策已拍板（v0.5 起为定论，不再待确认）**：
> - Q1 虚拟化为主、**按源可物化**（不可达/限时段源开 `mode=materialized`）。
> - Q5 分类 **严格树**（多父 DAG 延到 v2）。
> - Q6 暴露接口 **REST + GraphQL 两者**（REST 一期、GraphQL 二期）。
> - Q7 **Mongo 降二期**（一期用 REST driver 兜底）。
> - Q4 治理介质 **先 SQLite、后 Redis**（v2 切换）。
> - Q2 引擎技术栈为实现细节（建议 Go）。
> - **新增 UR-9 / F-MCP**：暴露 MCP 接口供智能体管理元数据，提案须**人工审批**后生效（智能体无审批权）。
>
> 功能编号沿用 PRD 的 `F-<模块>-<序号>`。
>
> **⚠ 不回写 `gear/REQUIREMENTS.md`（2026-10-08 东东拍板）**：本 App 是**验证 Gear 宿主设计的探针**，
> 不是给 Gear 加功能，故 F-xxx / UR-x 一律**不**进内核需求矩阵；平台侧发现另见
> `apps/federation/GEAR_DESIGN_VALIDATION.md`。
>
> **实现状态见本文 §6**（截至 2026-10-09，v1 已闭环，GraphQL 已实现）。

---

## 1. 功能总览（按模块）

| 模块 | 功能ID | 功能名 | 一句话说明 | 分期 |
|------|--------|--------|-----------|------|
| 数据源 | F-DS-1 | 注册数据源 | 填类型+连接+凭据引用，支持 SQL / Excel / Mongo | MVP(SQL+Excel) / v2(+Mongo) |
| 数据源 | F-DS-2 | 连通性测试 | 保存前探测可达性与凭据正确性，区分语法错/权限不足 | MVP |
| 数据源 | F-DS-3 | Excel 源接入 | 上传或指定路径，选 sheet、表头行、数据范围 | MVP |
| 数据源 | F-DS-4 | 凭据不落明文 | 账号/密钥走 secret 引用，界面与存储均不显示明文 | MVP |
| 数据源 | F-DS-5 | 源治理策略配置 | 每源独立配限流/时段/缓存 | v1 |
| 建模 | F-MODEL-1 | 定义对象与属性 | 建对象类型，声明属性名/类型/单位/描述，标主键 | MVP |
| 建模 | F-MODEL-2 | 可视化属性映射 | 属性拖到源字段/表达式；同源多属性自动合成一条查询（下推） | MVP |
| 建模 | F-MODEL-3 | 定义链接类型 | 对象间联系，声明 join 条件与基数 | MVP |
| 建模 | F-MODEL-4 | 定义分类树与分类联系 | 分类父子成树；分类之间也可建联系 | MVP |
| 建模 | F-MODEL-5 | 导入期静态校验 | 主键唯一/类型合法/映射可解析/策略不冲突，不通过拒保存 | MVP(v1 强化) |
| 查询 | F-QUERY-1 | 对象读取 | 按类型取实例，过滤/分页/排序，跨源属性自动合并 | MVP |
| 查询 | F-QUERY-2 | 链接遍历 | 沿 LinkType 取关联对象 | MVP |
| 查询 | F-QUERY-3 | 分类树下钻 | 取分类下对象、祖先路径、子孙 | MVP |
| 查询 | F-QUERY-4 | 检索 | 跨对象/属性关键词检索 | v1 |
| 查询 | F-QUERY-5 | 新鲜度元数据 | 结果带 `as_of`/`staleness_sec`/`stale`/`origin` | MVP(基础) / v1(完整) |
| 治理 | F-GOV-1 | 限流 | 每源日配额账本+瞬时令牌桶，超限明确报错 | v1 |
| 治理 | F-GOV-2 | 时间窗 | 窗外默认 `cache_only`；有窗口源强制开缓存+预热 | v1 |
| 治理 | F-GOV-3 | 双 TTL 缓存 + single-flight | 新鲜窗口+硬上限；并发同查询只回源一次 | v1 |
| 治理 | F-GOV-4 | 熔断 | 单源连续失败转快速失败，可恢复 | v2 |
| 浏览 | F-EXPLORE-1 | 实例视图 | 实例列表/详情，显示每字段的源与新鲜度 | MVP |
| 浏览 | F-EXPLORE-2 | 关系图 | 图展示对象/链接、分类/分类联系 | v1 |
| 浏览 | F-EXPLORE-3 | 分类导航 | 左侧分类树点击下钻 | MVP |
| 外部 | F-EXT-1 | Redis 介质 | 缓存/计数/锁跨实例共享（v2 切换） | v2 |
| 外部 | F-EXT-2 | Mongo 源 | 企业 Mongo 孤岛作为被联邦源 | v2 |
| 外部 | F-EXT-3 | Excel 源 | 见 F-DS-3 | MVP |
| 运维 | F-OPS-1 | 声明式部署 | 定义写成 bundle YAML 一键导入 | MVP |
| 运维 | F-OPS-2 | 自检 doctor | 报告「0 引用定义/策略冲突/实际缓存介质」 | v1 |
| 运维 | F-OPS-3 | 导出/导入 | 定义可备份/git 管理/再导入 | MVP |
| 智能体 | F-MCP-1 | MCP 管理面暴露 | 引擎独立端口暴露自有 MCP（与 gear 15 工具面隔离），智能体连入 | v1 |
| 智能体 | F-MCP-2 | 仅提案不改活 | 智能体写入只产生草稿(draft)，不影响线上查询 | v1 |
| 智能体 | F-MCP-3 | 元数据提案工具 | fed_propose_object_type/property/link/category/source/mapping | v1 |
| 智能体 | F-MCP-4 | 草稿可见 | 列待审草稿、看与现行 diff | v1 |
| 智能体 | F-MCP-5 | 审批权仅限人工 | 审批走 UI/受鉴权端点，智能体无审批权 | v1 |
| 智能体 | F-MCP-6 | 审计留痕 | 谁提案/何时/谁审批/结果 全程记录 | v1 |

---

## 2. 分模块详述

### 2.1 数据源管理（F-DS）
- **F-DS-1 注册数据源**：用户选类型填连接。SQL 填 host/db/user（密码走 F-DS-4）；Excel 填路径或上传；Mongo 填 URI。
- **F-DS-2 连通性测试**：点「测试」5s 内返回「可达/不可达+原因」；权限不足与凭据错误提示不同。
- **F-DS-3 Excel 源**：上传 `.xlsx/.xls` 后列出工作表、预览前 N 行；可指定表头行（默认 1）与范围（如 `A2:Z1000`）；映射列不越界；非 Excel 拒绝。
- **F-DS-4 凭据不落明文**：所有密钥经 secret 引用注入引擎，UI 与存储只显示引用名。
- **F-DS-5 源治理策略配置**：每源独立配日配额、瞬时 QPS、允许时段、缓存 TTL——驱动 F-GOV。

### 2.2 对象建模 Studio（F-MODEL）
- **F-MODEL-1**：建 `ObjectType`，加属性（名称/类型/可空/单位/描述），指定主键（且唯一源）。
- **F-MODEL-2**：可视化把属性拖到源字段；**同源 ≥2 属性时引擎只发 1 条该源查询**（下推，验收看查询日志）。
- **F-MODEL-3**：建 `LinkType`，from→to，写 join（如 `Customer.id = Order.customer_id`）与基数（1 / *）。
- **F-MODEL-4**：建 `Category`（父子成树，路径物化+闭包表）；建 `CategoryLinkType`（分类间联系）。
- **F-MODEL-5**：保存时校验——主键非唯一源 / 映射在原库不可解析 / 有窗口源却未开缓存且无预热 → 拒保存并指因。

### 2.3 联邦查询服务（F-QUERY）
- **F-QUERY-1**：`GET /objects/{type}?filter&page&sort`；跨源属性按主键合并，无重复主键行。
- **F-QUERY-2**：`GET /objects/{type}/{id}/{link}` 沿 LinkType 取关联对象。
- **F-QUERY-3**：`GET /categories/{id}/tree` 取祖先/子孙；`GET /categories/{id}/objects` 取分类下对象。
- **F-QUERY-4**（v1）：`GET /search?q=` 跨对象/属性检索。
- **F-QUERY-5**：响应 JSON 必含 `as_of`/`staleness_sec`/`stale`/`origin`；`stale=true` 前端显著标识。

### 2.4 源访问治理（F-GOV）
- **F-GOV-1**：日配额账本（原子写+`rows_affected` 判定，禁 SELECT-then-UPDATE）+ 瞬时令牌桶；超限返回明确「配额耗尽」错误，不静默返回空/旧。
- **F-GOV-2**：`allowed_hours` 外默认 `cache_only`；有窗口源**必须**开缓存且**必须**配预热（ext.cron 在窗口内刷热点），否则 F-MODEL-5 拒保存。
- **F-GOV-3**：双 TTL（fresh_ttl + max_stale 默认 fail-closed）；同一查询并发 100 次源只访问 1 次。
- **F-GOV-4**（v2）：单源连续失败转快速失败，避免雪崩，可手动/自动恢复。

### 2.5 浏览与探查 UI（F-EXPLORE）
- **F-EXPLORE-1**：实例列表/详情，每字段显示来源与新鲜度标记。
- **F-EXPLORE-2**（v1）：关系图视图（对象+链接、分类+分类联系）。
- **F-EXPLORE-3**：左侧分类树导航下钻。

### 2.6 外部组件接入（F-EXT）
- **F-EXT-1**（v2）：Redis 作缓存/计数/锁介质，跨实例共享配额、原生 TTL、single-flight。
- **F-EXT-2**（v2）：Mongo 作为被联邦源（聚合管道翻译；Q7 已决二期）。
- **F-EXT-3**：Excel 源（同 F-DS-3）；引擎用自有 xlsx 读取器（如 excelize）打高频读，`ext.excel` 侧车可作备选。

### 2.7 部署与运维（F-OPS）
- **F-OPS-1**：数据源/对象/分类/映射写成 bundle YAML，一键导入起服。
- **F-OPS-2**（v1）：`doctor` 报「有定义 0 引用」「策略冲突」「实际缓存介质」。
- **F-OPS-3**：定义可导出备份、git 管理、再导入。

---

## 3. 分期路线图

### MVP（可演示的概念验证，~3 月）
**范围**：SQL + Excel 双源；对象/属性/链接/分类定义；基础查询（读取/链接/分类下钻）；基础浏览 UI；本地 SQLite 元数据；声明式部署+导出导入。
**退出标准**：把「MySQL 客户表 + Excel 订单表」合成统一 Customer 对象，能下钻分类、看关联订单，结果带 `as_of`。
**不做**：治理、检索、Mongo、GraphQL、Redis、关系图、MCP 管理面。

### v1（生产可用，~+5 月）
**新增**：源治理（限流/时间窗/双 TTL/single-flight）；检索；关系图 UI；doctor；REST API 正式化；导入期校验强化（策略冲突）；**MCP 管理接口 + 人工审批工作流（F-MCP）**。
**退出标准**：某 REST/限频源在非允许时段用缓存兜底、窗外无缓存明确报错；有窗口源强制预热；跨 3 源查询正确合并。

### v2（增强，~+6 月）
**新增**：Redis 介质（跨实例）；熔断；GraphQL；**Mongo 源**；分类多父 DAG（若 Q5 确认）；缓存预热调度器；多实例部署。
**退出标准**：多实例共享配额不超发；单源故障不雪崩；GraphQL 按需取属性与链接。

---

## 4. 明确不做（范围边界）

- **写回业务系统（ActionType）**：幂等/事务/补偿/审计四件套齐了再做。
- **Elasticsearch 作为联邦引擎**：JVM 重依赖违「单 exe+零重依赖」；物化已由本地 DuckDB/Parquet 提供，一期不引。
- **分类多父 DAG**：默认严格树；多父需邻接+闭包双表+环检测，v2 再议（Q5）。
- **跨源事务 / 复杂物化 JOIN**：跨源合并仍在内存 keyed-merge；单源物化已支持，不做跨源事务。
- **把定义放进 Redis/外部组件**：定义（几 KB 元数据）永远在本地 SQLite，外部组件只装可再生的缓存/计数/锁。
- **智能体自审批**：MCP 智能体面只能提案(draft)，审批权仅限人工（UI/受鉴权端点）。

---

## 5. 仍待你确认的产品决策（影响清单）

| ID | 决策 | 结论 |
|----|------|------|
| Q1 | 虚拟化/物化 | 虚拟化为主、按源可物化（不可达源开 materialized） |
| Q5 | 分类树/DAG | 严格树（DAG 二期） |
| Q6 | REST/GraphQL | 两者（REST 一期、GraphQL 二期） |
| Q7 | Mongo 是否一期 | 二期 |

> 已全部决议，无遗留待确认。新增 **UR-9/F-MCP**（智能体 MCP 管理 + 人工审批）已纳入 v1。
> 技术栈（Q2）、治理介质节奏（Q4）、引擎托管（runtime: process）为实现细节，不阻塞。

---

## 6. 实现状态（截至 2026-10-10，App `apps/federation`，33 个 Tool）

> 落点文件：`federation_tools.yaml`（引擎）/ `federation_flow.yaml`（起服）/ `bundle.yaml`（App 定义）/ `www/index.html`（UI）。
> 状态口径：**已实现**＝端到端验证过；**部分**＝主干通、边界未做；**未实现**＝代码里没有。

### 6.1 按功能 ID

| 功能ID | 状态 | 落点 / 说明 |
|--------|------|-------------|
| F-DS-1 注册数据源 | 已实现（SQL+Excel） | `federation__source_add`；Mongo 未做（见 6.3） |
| F-DS-2 连通性测试 | 已实现 | `federation__source_test`（POST `/api/fed/source_test`）：`SELECT 1` 探活 + 可选探测表（列名/行数/列存在性）与列清单；**注册时不自动探活**（注册与探测是两件事）。表名走标识符白名单，防注入 |
| F-DS-3 Excel 源接入 | 部分 | `federation__file_import`：浏览器 SheetJS 解析后 POST 建表+索引；**不做 sheet 选择/表头行/范围**（Lua 沙箱禁 os/io，只能浏览器侧解析） |
| F-DS-4 凭据不落明文 | **未实现** | `secret_ref` 列存在但无注入通道（MCP 面禁 secret 是内核刻意设计） |
| F-DS-5 源治理策略 | 已实现 | `source_add` 的 `quota_per_day`/`rate_limit`/`rate_window`/`cache_ttl`/`max_stale`/`window` |
| F-MODEL-1 定义对象与属性 | 已实现 | `federation__object_add` |
| F-MODEL-2 属性映射 | 已实现（无可视化） | 属性→(source,tbl,col)；同源多属性合成一条查询（keyed-merge 分组下推） |
| F-MODEL-3 链接类型 | 已实现 | `federation__link_add`；`object_query` 的 `traverse` 做一层下钻 |
| F-MODEL-4 分类树与分类联系 | 已实现 | 层级：`federation__category_add`/`category_tree`（严格树，path 物化）。**非层级联系**：`federation__category_link`（`/api/fed/category-links`，`add/list/delete`）——kind 白名单 `related`/`maps_to`/`synonym`/`derived_from`（非法显式拒）、两端必须存在、禁自环、`status` 走审批、返回 `relation` 说明与层级树的关系；横向关系**不塞进 parent**，否则树不再是树。对象可挂 `category`（须存在，`ONT_OBJ_CAT_MISSING` 阻断审批） |
| F-MODEL-5 导入期静态校验 | 已实现 | `federation__validate`（POST `/api/fed/validate`）：引用完整性 + 治理策略一致性。**挂在审批前**：`approve` 先跑校验，有 error 直接拒（`ONT_VALIDATION_FAILED`），`force=true` 才放行。核心约束「有窗口源必须开缓存」由此真正生效。不做可达性探测（避免校验动作打到生产库） |
| F-QUERY-1 对象读取 | 已实现 | `object_query`（单实例）+ `federation__instance_list`（`/api/fed/instances`，过滤/排序/分页）。模型=**驱动源下推 + 其余源按主键批量回填**；跨源过滤/排序显式报错（`ONT_CROSS_SOURCE_FILTER`/`_ORDER`）。`limit` 硬夹 1..1000 |
| F-QUERY-2 链接遍历 | 已实现 | `object_query{traverse:true}`（一层） |
| F-QUERY-3 分类树下钻 | 已实现 | `federation__category_objects`（POST `/api/fed/category/objects`）：按分类取对象，`include_descendants`（默认 true）走 path 前缀含整棵子树，返回 `subtree[]`（含每节点 `object_count`）+ `objects[]` + `counts`。可 `with_instances` 顺带取实例（每对象夹 200 条，单对象失败只点名不拖垮视图）。**分类不存在（`ONT_CATEGORY_NOT_FOUND` + `available_categories`）与分类下没对象（空清单 + hint）严格区分** |
| F-QUERY-4 检索 | 已实现 | `federation__search`（按对象名/描述模糊匹配，不检索实例数据） |
| F-QUERY-5 新鲜度元数据 | 已实现 | `as_of`/`staleness_sec`/`stale`/`origin` + per-source `freshness{}`（含 `served_from`/`in_window`/`mode`） |
| F-GOV-1 限流 | 部分 | 日配额（原子自增）+ **固定窗口**瞬时计数；**非令牌桶**，窗口切换瞬间可能放过约 2x |
| F-GOV-2 时间窗 | 已实现 | `window{start,end,tz,on_outside}`：`deny`/`cache_only`/`allow` + `cache_warm` 预热 |
| F-GOV-3 双 TTL 缓存 | 部分 | `cache_ttl` + `max_stale` 容忍陈旧；**single-flight 未做**（详见 6.3） |
| F-GOV-4 熔断 | 已实现 | `federation__breaker` + 读路径内建状态机 `closed→open→half_open→closed`：连续失败达 `breaker_threshold`（默认 3）跳闸，`breaker_cooldown`（默认 60s）内不打源；冷却结束自动半开放**一次**探测。跳闸前先喂可用缓存（`breaker_on_open=cache`）。计数与跳闸在同一 `ctx.db.batch` 事务内 |
| F-EXPLORE-1 实例视图 | 已实现 | UI「查询」tab：单实例 + 新鲜度，以及实例列表（过滤/排序/分页，见 F-QUERY-1） |
| F-EXPLORE-2 关系图 | 已实现 | `federation__graph`（POST `/api/fed/graph`）+ UI「关系图」tab（SVG，可拖动）。四种视图：`objects`/`categories`/`sources`/`all`。分类视图画 `parent` 与 `cat_link` 两种边（非层级边单独 kind，混成父边会让树不再是树）。**悬空引用不画假节点**，只在 `stats.dangling_links` / `stats.dangling_category_links` 里报；`stats` 只统计当前视图真画出来的那类边 |
| F-EXPLORE-3 分类导航 | 已实现 | **左侧常驻分类树**（`<aside id="sidebar">`）：分类按 `parent` 递归成树 + 分类下对象叶子（未挂分类归「未分类」组），点分类下钻到「分类」tab 的 `category_objects`、点对象进「查询」tab 列表实例，折叠状态跨 `refresh()` 保留。另 UI「分类」tab 仍有分类树表格（含 path，每行「查对象」下钻）、分类下对象面板（可勾选含子树/带实例）、非层级联系增删表（含 relation/status/悬空标注）；关系图分类视图同时画 `parent` 与 `cat_link` 两种边 |
| F-EXT-1 Redis 介质 | **未实现** | v2；本机无 Redis 服务，未做（详见 6.3） |
| F-EXT-2 Mongo 源 | **未实现** | v2；平台无 `ext.mongo`（详见 6.3） |
| F-EXT-3 Excel 源 | 同 F-DS-3 | — |
| F-OPS-1 声明式部署 | 已实现 | `gear tool register` + `gear flow register` + `gear app start` |
| F-OPS-2 自检 doctor | 已实现 | `federation__doctor`（POST `/api/fed/doctor`）：元数据计数 + 转调 `validate` + 可选 `deep=true` 连源探测（默认不连）。转调而非复制，口径只有一处 |
| F-OPS-3 导出/导入 | 已实现 | `federation__export`（POST `/api/fed/export`）+ `federation__import_defs`（POST `/api/fed/import_defs`）＋ UI「导出 / 导入」tab。定义包 `federation-defs` v1；**默认 dsn 脱敏**，保留明文需 `redact:false`。导入 `merge`（默认）/ `replace`（须 `confirm:true`），支持 `dry_run`、导入后审批（仍过静态校验）。**缺 id 整体拒**；merge 不带 status 保持原状不降级；审批留痕默认不回放。**不暴露给 MCP**（导入能改活定义，属人工运维动作） |
| F-MCP-1 MCP 面暴露 | 已实现 | `federation__mcp`（`POST /api/fed/mcp`，Streamable HTTP/JSON-RPC） |
| F-MCP-2 仅提案不改活 | 已实现 | 提案落 `draft`；`object_query` 对非 approved 对象直接拒 |
| F-MCP-3 元数据提案工具 | 已实现 | 5 提案（`propose_source`/`object`/`link`/`category`/`category_link`）+ 12 只读（`list_sources`/`list_objects`/`get_object`/`list_categories`/`list_category_links`/`category_objects`/`search`/`query_object`/`list_instances`/`govern_stats`/`breaker_list`/`list_drafts`） |
| F-MCP-4 草稿可见 | 已实现 | `list_drafts` + `GET /api/fed/drafts` + UI 审批 tab |
| F-MCP-5 审批权仅限人工 | 已实现 | MCP 面**刻意不暴露 approve**；审批走 REST/UI |
| F-MCP-6 审计留痕 | 已实现 | `fed_approvals`（proposed_by/approved_by/decided_at） |

### 6.2 v2 已提前落地

| 项 | 状态 | 说明 |
|----|------|------|
| GraphQL（原 v2 / Q6） | 已实现 | `federation__graphql`（`POST /api/fed/graphql`）：严格子集解析器，不支持的语法**显式报错**不做静默容错 |
| 物化模式定时刷新 | 已实现 | `federation__refresh` + `federation__schedule`（`ext.cron` 到点回调 `/api/fed/refresh`） |

### 6.3 未做的三条与原因（不是漏做，是平台/环境约束）

- **Mongo 源**：平台能力目录里**没有 `ext.mongo`**（现有 ext：email/webhook/wecom/rabbitmq/kafka/sms/serve/filestore/cron/oidc/excel/pdf/oss/redis）。
  要支持得新造一个侧车——那是给 Gear 加能力，超出「App 作为验证探针」的范围，不做。
- **single-flight（F-GOV-3 的一半）**：Lua 侧**做不了**。每次执行新建 VM ⇒ 无共享内存放 in-flight 标记；
  沙箱无 `os` 库 ⇒ 没有 sleep/等待手段，无法「等前一个请求填完缓存再读」。用 DB 记标记只能做到
  「标记存在就返回 miss」，那不是合并是拒绝。防击穿当前靠 TTL + `max_stale` 兜。已记入验证报告 §2.10。
- **Redis 介质（F-EXT-1）**：`ext.redis` 侧车存在且已构建，但**本机没有 Redis 服务**，
  装了也端到端验证不了。按「不静默降级」先不落地——等有 Redis 实例再做。

### 6.4 Tool 全表（33 个）

| Tool | 路由 | 作用 |
|------|------|------|
| `federation__config` | — | anchor（占位） |
| `federation__api` | — | process 长驻：`gear serve --addr 127.0.0.1:8792` |
| `federation__migrate` | POST /api/fed/migrate | 建表（含老库补 `status` 列） |
| `federation__source_add` / `source_list` | POST / GET /api/fed/sources | 源注册 / 列举 |
| `federation__object_add` / `object_list` / `object_get` | POST / GET / GET{id} /api/fed/objects | 对象定义 |
| `federation__object_query` | POST /api/fed/query | 跨源实例查询（治理管线上） |
| `federation__link_add` | POST /api/fed/links | 链接定义 |
| `federation__category_add` / `category_tree` | POST / GET /api/fed/categories | 分类严格树 |
| `federation__category_objects` | POST /api/fed/category/objects | 分类下对象查询（子树下钻 + 可选带实例） |
| `federation__category_link` | POST /api/fed/category-links | 分类间非层级联系（add/list/delete） |
| `federation__approve` | POST /api/fed/approve | 人工审批 |
| `federation__file_import` | POST /api/fed/import | 文件摄取（浏览器解析后 POST） |
| `federation__search` | POST /api/fed/search | 检索 |
| `federation__seed_demo` | POST /api/fed/seed | 演示数据 |
| `federation__govern_stats` | GET /api/fed/govern | 治理状态 |
| `federation__cache_warm` | POST /api/fed/warm | 预热 |
| `federation__drafts` | GET /api/fed/drafts | 待审批队列 |
| `federation__mcp` | POST /api/fed/mcp | MCP 端点（12 工具） |
| `federation__refresh` | POST /api/fed/refresh | 物化刷新 |
| `federation__schedule` | POST /api/fed/schedule | 定时调度（`ext.cron`） |
| `federation__graphql` | POST /api/fed/graphql | GraphQL 端点 |
| `federation__source_test` | POST /api/fed/source_test | 连通性测试（只读，不扣配额） |
| `federation__validate` | POST /api/fed/validate | 静态校验（引用完整性 + 策略一致性） |
| `federation__doctor` | POST /api/fed/doctor | 自检（计数 + 转调 validate + 可选连源） |
| `federation__instance_list` | POST /api/fed/instances | 实例列表（过滤/排序/分页下推） |
| `federation__breaker` | POST /api/fed/breaker | 熔断器查看/复位/跳闸 |
| `federation__export` | POST /api/fed/export | 导出定义包（默认脱敏） |
| `federation__import_defs` | POST /api/fed/import_defs | 导入定义包（merge/replace/dry_run） |
| `federation__graph` | POST /api/fed/graph | 关系图数据 |
