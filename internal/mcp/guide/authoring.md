# authoring：写 Tool / Flow / Hook / Migrations 的全字段语义

## 1. Tool

```yaml
id: myapp__fetch            # 必填。命名空间：<app-id>__ 或 <app-id>.
name: 抓取 URL              # 必填
description: 抓一个 URL 的响应体   # 必填
runtime: lua                # 必填：process | lua | go-host（wasm 未实现，注册即拒）
entry: |                    # 必填。lua：直接写脚本本体；process：写命令
  local r = ctx.http.Request("GET", ctx.input.url)
  return { status = r.status, body = r.body }
capabilities:               # 可选但关键：声明可用哪些宿主能力（受门禁原语 + ext.*）
  - http
input_schema:               # 必填：标准 JSON Schema，顶层 type 必须为 object
  type: object
  properties:
    url: {type: string}
  required: [url]
output_schema:              # 必填（同上）
  type: object
versions:                   # 必填且非空
  - version: "1.0.0"        # SemVer
    state: stable           # draft | testing | stable | deprecated | rejected
api:                        # 可选：声明式 REST 路由（FR-3），不写就完全没有这个行为
  method: GET               #   GET | POST | PUT | PATCH | DELETE（必须大写，小写会被拒）
  path: /api/orders/{id}    #   以 /api/ 开头；{id} 是路径参数，**名字即 Tool 的输入字段**
```

- **runtime 语义**：`lua` 走内嵌沙箱（`entry` 是脚本本体，用 `ctx.input` 读入参、`return {...}` 出参）；
  `process` 走外部命令（stdin 进 JSON、stdout 末行出 JSON）；`go-host` 走宿主注册的实现。
- **capabilities 门禁**：用了 `ext.*` 或 6 个受门禁原语就**必须**声明；`crypto`/`time`/`template` 声明了反而被拒。
- **`versions` 是必需的**——因为 Flow 的 `use_version` 只能钉已注册 Tool 的版本；原子能力直调没有版本概念。
- **`api` 是「加挂」不是「替换」**：声明了资源路由的 Tool 仍然挂在 `POST /api/tools/{id}/run` 上，
  两条路同时可用（AC-C4 要的就是这个向后兼容）。
  - `GET /api/orders/42` 的响应体**就是** Tool 输出本身（不包 `{id, output}`）；
  - 路径参数按名字进输入：`{id}` → `input["id"]`，值是**字符串**（需要数字就在 Tool 里转）；
  - `POST/PUT/PATCH` 还会把 JSON 请求体并入输入，**路径参数优先**于 body 里的同名键；
  - **不鉴权**。gear 没有用户库（Q1 已拍板：授权是业务的事），资源路由只保证「长得像 REST」。
    真前门请给这个 Tool 挂一条鉴权 Hook。

### 1.1 Lua 沙箱：可用与不可用清单（2026-10-03 补）

沙箱是**白名单**制（`SkipOpenLibs: true` 后逐个重开），实测踩过的坑列在这里：

| 类别 | 可用 | 说明 |
|---|---|---|
| 标准库 | `base` `string` `table` `math`（+ `coroutine`，仅 permissive 策略） | `os` / `io` / `package` / `debug` **永不开**，配置项只能「开更多纯计算库」，关不掉沙箱 |
| 全局函数 | 常规 Lua | `dofile` / `loadfile` / `require` / `module` 一律被置为 `nil`（任何策略下都是） |
| 时间 | `ctx.time.now({format="unix"})` 等 | **没有 `os.date` / `os.time`**——要时间必须走 `time` 纯函数能力 |
| 宿主能力（受门禁） | `http` `call` `db` `secret` `event` `subscribe` `config` `fs` | **必须在 `capabilities:` 里声明**才能用 |
| 纯函数能力（免声明） | `time` `template` `crypto` `id` `validate` | 始终可用、无需声明；**声明了反而被拒**（注册期即报错） |

**未声明的能力 = 在 `ctx` 上根本不存在**（取值是 `nil`，不是空表）。这是刻意设计：脚本连
宿主的能力边界都探测不到。

**但注册期会替你查（2026-10-03 新增）**：`gear_validate` / `gear_apply` / `gear tool register`
都会扫 Lua entry 的 **AST**（不是文本——注释与字符串里的 `ctx.db` 不算），发现「用了受门禁
能力却没声明」就报 `LUA_CAP_UNDECLARED`，并点名是哪个能力、脚本第几行、该怎么改：

```yaml
kind: Tool
id: myapp__fetch
runtime: lua
capabilities: [http]        # ← 诊断会点名要补什么，照抄即可
entry: |
  local r = ctx.db.query({ sql = "select 1" })   # ← 诊断里写的就是 entry.lua:1
  return ctx.http.Request("GET", r.url)
```

所以不必再靠运行期的报错反推：以前这错只表现成 `attempt to call a non-function object`
（看着像函数写错了，真因是漏声明），而且 `if ctx.call then … end` 会静默走 else 分支。
**仍然建议把 `capabilities` 一次写全**——静态校验只覆盖 entry 里**字面**出现的 `ctx.<能力>`，
经变量/别名间接引用的（`local f = ctx.db; f.query(…)`）扫不出来。

## 2. Flow

```yaml
id: myapp.up                # 必填
name: myapp 起服            # 必填
description: 托管静态目录     # 必填（catalog 摘要里显示它，写清楚很有用）
vars:                       # 可选；不声明就整段省略（留空是合法的）
  topic: world
steps:                      # 必填，至少一个
  - id: s1                  # 必填，Flow 内唯一
    call: myapp__fetch      # 必填：已注册 Tool 的 id，或「能力.方法」直调
    input:                  # 可选：step 入参（值可写模板）
      url: "${vars.topic}"
    depends_on: [anchor]    # 可选：显式上游；写 ${steps.x.output.y} 会自动推断，不必重复写
    when: "steps.s1.output.n > 1"   # 可选：表达式（不是模板）
    parallel: true          # 可选：仅作显式标注（无依赖的 step 本就并行）
    use_version: "1.2.0"    # 可选：钉 Tool 版本（仅 Tool 载体；直调原子能力不支持）
    mode: service           # 可选：once（默认）| service（长驻，须 gear_app 监管）
                            #   长驻进程可在 stdout **第一行**吐一次 JSON 作声明
                            #   （如 {"port":8787}），下游即可用 ${steps.<id>.output.port}
```

**长驻进程的「首行声明」**：`mode: service` 的 step 同样能被 `${steps.<id>.output.<k>}` 引用，
载体是子进程 stdout 的**第一行非空行**（必须是合法 JSON object）。于是端口可以不用写死：

```yaml
steps:
  - id: api                      # entry: "gear serve --addr 127.0.0.1:0" → 内核分配
    call: mindstorm__api
    mode: service
  - id: site                     # proxy target 直接用上游报出来的真实端口
    call: mindstorm__site
    input:
      proxy:
        - path: /api
          target: "http://127.0.0.1:${steps.api.output.port}"
```

首行不是 JSON（普通日志 / 启动横幅）时该 step 的 `output` 为 **`null`**，下游引用会报错——
不会静默串成 `http://127.0.0.1:` 这种查不出原因的错串。引用 `service ext` 的 step，其 output
恒为 `null`（侧车 stdout 是协调协议，无从解析）。

**硬规则（`gear_validate` 会逐条拦）**

| 规则 | 说明 |
|---|---|
| `call` 必须可解析 | 要么是已注册 Tool，要么是「能力.方法」；否则 `UNKNOWN_CALL_TARGET` |
| 「能力.方法」直调不支持 `use_version` 与 `mode: service` | 前者只对 Tool 版本有意义，后者长驻必须用 process Tool |
| `mode` 只能是 `once` / `service` | 未知 mode 显式拒绝，不静默降级 |
| `when` 不含 `${}` | 它是表达式；写了会丢类型（`"200"` vs `200`） |
| 依赖无环 | `depends_on` + 隐式依赖合成的图必须无环 |
| 标记 `parallel` 就不能同时有 `depends_on` | 无依赖 step 本就并行，两者同写是误导性标注 |
| service step **不能作为依赖目标** | 需要 once 步做「锚点」把后续步推下一层（见 `overview` §5） |
| 模板前缀三选一 | `${vars.x}` / `${steps.<id>.output.x}` / `${secrets.NAME}` |
| `steps.<id>.output` 对长驻 step 也有效 | 取子进程 stdout **首行** JSON；首行非 JSON 则该字段为 `null`，下游引用即报错（不静默串空） |
| **不得硬编码机器绝对路径** | db 的 dsn / process entry / ext.serve 的 dir 等不得写 `C:/...`、`/Users/...`、`/d/gadern/...`（含 Lua 脚本体内的 dsn）；换台机器即废 | 改用相对锚点：`dsn: mindstorm.db`（→ GEAR_APP_DATA）、`entry: "gear serve ..."`（→ 可执行文件目录）、`dir: apps/<app>/www`（→ 安装根）；`gear_validate` 报 `PORTABLE_PATH_HARDCODED`（详见 `gear_guide("portability")`） |

## 3. Hook

```yaml
id: myapp__audit            # 必填
target: myapp__fetch        # 必填：Tool id / Flow id / "*"（全部）
stage: before_step          # 必填：before_flow|before_step|after_step|on_error|on_success|before_promote
action: myapp__log          # 必填：Tool id 或 Flow id（须已注册）
criticality: best-effort    # 可选：critical（默认，失败终止）| best-effort（失败仅记录）
order: 10                   # 可选：同 stage 内的执行顺序
```

注意：`action` 指向的对象是否存在**不在注册期校验**（Hook 与 Tool/Flow 独立注册，存在性在运行期解析）。
所以 Hook 注册成功 ≠ 它引用的 Tool 一定在——`gear_apply` 时把 Hook 放在 Tool/Flow 之后**更稳**。

## 4. Migrations（bundle 里的第四类文档：库怎么演进入）

Tool / Flow / Hook 描述「行为」，Migrations 描述**这个应用的库长什么样**。它是 bundle 里独立的一份
文档（`kind: Migrations`），不注册进 registry——**迁移是副作用，不是定义**。

```yaml
app_id: myapp         # 可选：只标归属，gear 不校验它与 App 清单是否一致
driver: sqlite        # 可选：本组默认驱动；单条可覆盖
dsn: myapp.db         # 必填：默认目标库（相对锚点 → GEAR_APP_DATA，与 ctx.db.* 同口径）
migrations:           # 必填且非空
  - version: 1        # 必填，正整数，从 1 起依次 +1
    name: create_nodes          # 可选，纯给人看的标签（报错时用版本号顶上）
    driver: sqlite              # 可选，覆盖文档级 driver
    dsn: myapp.db               # 可选，覆盖文档级 dsn（同一 dsn 下同名 version 才算重复）
    sql: |                      # 必填非空；一段文本，可含多条语句
      CREATE TABLE nodes (id INTEGER PRIMARY KEY, name TEXT);
      CREATE INDEX idx_nodes_name ON nodes(name);
  - version: 2
    name: add_owner
    sql: "ALTER TABLE nodes ADD COLUMN owner TEXT"
```

**执行时机（`gear_apply` / `gear_import` 共用）**：先跑完所有迁移，**再**写任何一条定义。
所以「导入成功但表不存在」在 gear 里不是一个合法状态——迁移失败即整包拒绝，一个定义都不落库。

| 规则 | 说明 |
|---|---|
| 台账在**目标库**里 | `gear_migrations` 表建在被迁移的那个库（与 schema 同生共死），不在 `registry.db` |
| 已应用的不重跑 | version 相同即跳过；实测是「第二次 `gear_apply` 不重复建表」 |
| 每条迁移一个事务 | SQL 与台账 INSERT 同事务；SQL 成功而台账写不进去 = 整条回滚 + `MIGRATE_SQL_FAILED`（宁可失败，也不留「生效了却记不上」） |
| version 严格递增 | 重复 → `MIGRATE_INVALID`；断号 → `MIGRATE_VERSION_GAP`（错误码点名缺的那个版本） |
| `dsn` 必填 | 不许隐式落到内存 scratch 库（进程级、重启即丢，那样「迁移成功」连自己都验证不了） |
| 声明了却拿不到 db 实现 | `MIGRATE_NO_DB`，fail-closed：宁可拒绝，也不让「声明了迁移却没跑」成为合法状态 |
| **dry-run = 真跑 + 全回滚** | 不是跳过。坏 SQL 在写作期就暴露，而不是等你 apply 到生产 |
| 不提供反向迁移 | 要换库就换库（新 dsn + 新版本号）；schema 一旦进过生产库，自动回滚比不回滚危险 |
| ⚠ MySQL 上 DDL 隐式提交 | 回滚不保证生效，只能靠 `--dry-run` 提前验；这条是数据库的语义，gear 不会替你假装 |

导出的产物**会**带这份文档，导回来时迁移照跑（因为它是副作用不是定义，不进 definition_history）。

## 5. 命名规范（ registry 会乱，先管住 id ）

Tool / Flow / Hook 一旦多起来，「这是谁的、谁在用」全靠 id 自解释。约定如下：

| 对象 | 命名 | 例 |
|---|---|---|
| 业务 Tool（属于某应用） | `<app-id>__<动词短语>`（snake_case） | `mindstorm__node_create`、`mindstorm__expand` |
| 应用内子域 Tool | `<app-id>.<sub>.<verb>` | `web_ui.api`、`web_ui.anchor` |
| 起服 Flow | `<app-id>.up` | `mindstorm.up`、`web_ui.up` |
| 排序锚点 Tool | `<app-id>__config` 或 `<app-id>.anchor`，description 必须写明「排序锚点」 | `mindstorm__config` |
| 共享基建（跨应用复用） | 独立命名，不挂任何应用前缀 | `static_site.serve`、`ext.serve` |
| 测试 / 临时 Tool | `fixture_` 前缀，且测试必须用隔离的 GEAR_HOME（`gearhome.IsolateForTest()`），**不许注册进真实 registry** | `fixture_echo` |

**三条硬约束 + 一条自觉**

- bundle 含 `kind: App` 清单时，其余 id 必须以 `<app-id>__` / `<app-id>.` 开头——这是引擎强制的（`ID_NAMESPACE_VIOLATION`）。
- 运行期另有一层 `gear ... --scope <app-id>`：`--scope` 非空时，引擎拒绝引用不属于该应用的
  已注册对象（`SCOPE_VIOLATION`）。默认不传 = 不启用。**平台级对象不受它管**——cap 原语
  （`db.query` 这类）与命名空间第一段不归属任何已注册 App 的注册 Tool（`static_site.serve` /
  `ext.serve` / `pdf.read.demo` 这类共享基建）照用，所以开了 scope 的应用仍能跑自己的入口 Flow。⚠ 2026-10-21 前后者
  （平台级**注册 Tool**）只写在注释里没实现——列得出来、调用被毙，本次已补齐（S-11 钉住）。
- `gear_apply` / `gear_validate` 对**无 App 清单**的裸注册会给 warnings 提醒补前缀（合法但会提示）。
- 删除一个应用 / Flow 时，**连同它的私有 Tool 一起删**——引擎目前不做孤儿检测，孤儿只能靠人发现。
- description 要写清「谁在用」（被哪条 Flow 引用 / 被前端 REST 调用），`gear_catalog` 的摘要里全靠它辨认。

**App 清单的 `mcp:` 策展区（可选）**

`kind: App` 可以写 `mcp.tools`，声明这个应用愿意把哪些注册 Tool 暴露给 MCP 客户端（工具数启动期即确定，不必再手搓 server.js）：

```yaml
kind: App
id: study
entry_flow: study.up
mcp:
  tools:
    - ref: study__plan_propose
    - ref: study__plan_confirm
      args: {session: parent}          # 服务端固定注入，AI 看不到也改不掉
    - ref: study__plan_preview
      alias: study_plan_preview
      description: "三段式②：只预览，禁止自动 confirm。"   # 护栏进清单
```

- `ref` 必须是**本应用命名空间下**、已注册的 Tool；`alias` 是对外工具名（不写则用 ref，须符合 `[A-Za-z0-9_.-]`、≤128）；`description` 覆盖 Tool 描述。
- `args` 的键必须是该 Tool `input_schema.properties` 里的真参数：注册时从对外 schema 删掉（含 `required`），调用时服务端无条件覆盖客户端传值。
- 生效方式 `gear mcp --expose tools --scope <app-id>`（`--expose all` 则连 Flow 快照一起）。**这是 MCP 服务端的启动参数，不是 MCP 工具**：智能体无法在会话内开启，写完清单要由人改 MCP 客户端配置并重连。**没有 `--scope`、App 未注册、或没写 `mcp.tools` 时拒绝启动**，不会退回裸暴露全量 Tool；该失败发生在服务端装配期，客户端连不上，不会以 `[CODE]` 形式回到你的会话里。详见 README §12。
- 只想给业务调用方这几个工具、不想连 15 个常驻 `gear_*`（含 registry 写面）一起交出去时，由人在启动命令加
  `--only-exposed`（须配 `--expose`）。该端点会换用 `guide/app-only.md` 作 instructions、不挂手册资源；
  也因此**没有** `gear_doctor` 可自查，排障要另连一个标准 `gear mcp` 实例。见 README §12。
- 两个自查出口：`gear_doctor` 的 `mcp` 块报本会话 `expose` / `scope` / `curated_tools`（实际装配名）与下一步；
  `gear_validate` 对「bundle 里有 Tool 但清单没写 `mcp.tools`」给 `MCP_CURATION_ABSENT` advisory（**不影响 valid**，
  不写策展区也合法）。看到「工具没出现」先查 doctor，别改 YAML。

**❌ 反例（真实事故：46 个 Tool 里 18 个是这种残渣）**

```yaml
id: m_t1            # ✗ 无前缀无语义，三个月后没人知道它是什么
id: ext_probe       # ✗ 冒充 ext.* 的测试残留，污染能力口径
id: ok_first        # ✗ 测试产物直接进了真实 registry
```

## 6. 正反例

**✅ 一次性编排：直调能力，不建 Tool**

```yaml
id: myapp.cacheit
steps:
  - id: put
    call: db.query               # 直调：能力.方法（先 gear_capabilities 确认方法名与参数）
    input: {sql: "SELECT 1"}
```

**✅ 可复用逻辑：建 Tool**

```yaml
id: myapp__slugify
name: Slug 化
runtime: lua
entry: "return { slug = string.lower(ctx.input.text) }"
```

**❌ 常见错误**

```yaml
steps:
  - id: s1
    call: crypto.hash            # ✗ 直调可以，但 capabilities 里声明 crypto 会被拒（免门禁纯函数）
  - id: s2
    call: json.encode            # ✗ json 不是能力，Flow 不能直调（只在 Lua 里用 ctx.json）
  - id: s3
    call: static_site.serve      # ✗ 若该 Tool 未注册 → UNKNOWN_CALL_TARGET
  - id: s4
    when: "${vars.flag}"         # ✗ when 是表达式；写 vars.flag 即可
  - id: s5
    call: ext.serve
    mode: service
    depends_on: [s3]             # ✗ 依赖一个不存在的 step → BAD_DEPENDENCY
```

## 7. 写错了怎么退回来（2026-09-26）

智能体改崩自己的定义是**常态**，不是意外，所以入参里就有退路：

```yaml
# 回滚到某个历史点（definition_history 的 seq）
gear_apply(restore_from: 17)
```

- `seq` 从哪来：`gear history list --kind tool --id myapp__slugify`（新的在前，一行 =「那时它是这样的」）。
- 回滚到「当时还不存在」的那一条 = 撤回这次新建（对象被删，不是留个空壳）。
- 回滚**本身也会留痕**（`note=restore from N`），所以能一路往前滚，不存在「滚过去就回不来」。
- **删除也留痕**：删除不经过 `upsert`，快照是单独在删除事务里采集的（`note=delete`），
  所以历史里查得到每一次删除。被引用拒绝的删除**不留痕**——守卫与留痕必须原子。
- **撤销一次删除**不需要新入口：把 `note=delete` 那条（**不是它后面的那条**）的 `seq` 填进 `restore_from`，
  被删的对象连同内容会被一起带回来。
- 写回的内容会重新校验：旧快照在当前规则下已不合法时，**整次回滚原子失败**（一个字节都不写），
  宁可失败也不给你一个回滚成功却跑不起来的定义。
- ⚠ 历史只覆盖**定义**（Tool/Flow/Hook/App 清单）。业务数据、静态资源、密钥都不在里面。
