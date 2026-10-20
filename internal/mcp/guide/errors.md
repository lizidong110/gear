# errors：错误码与自愈回路

## 自愈回路（这是「高效」的核心）

任何一步失败 → 读 **`code`**（不是读整段中文）→ 在本文档查修法 → 改 YAML → 重新 `gear_validate`
→ 通过后 `gear_apply`。**不需要问人，也不需要猜。**

诊断的形态（`gear_validate` 返回）：

```json
{ "valid": false, "diagnostics": [
  { "code": "UNKNOWN_CALL_TARGET", "doc": 2, "kind": "flow", "id": "myapp.up",
    "step": "site", "field": "step(site).call",
    "message": "step site 的 call \"static_site.serve\" 既不是已注册 Tool，也不是「能力.方法」形式的原子能力直调",
    "hint": "先用 gear_catalog(kind=tool) 确认 Tool 是否存在；或改用「能力.方法」直调（清单见 gear_capabilities）…" } ] }
```

> **例外：缺必填参数不带码。** 缺必填参数由 **JSON-RPC 层的入参校验**直接挡下（在到达 Gear 的
> handler 之前），所以它不是 Gear 的错误、**不带 `[CODE]`**，文案形如
> `validating "arguments": … missing properties: ["kind" "id"]`。**读 `missing properties` 里的
> 字段名就能定位**——别把它当成「Gear 有 bug」。这是「客户端就地发现漏参（省一次往返）」与
> 「错误都有码」两件事之间的取舍，我们选了前者。

## 错误码表

### 写作期（`gear_validate` / `gear_apply` 会拦）

| code | 触发 | 修法 |
|---|---|---|
| `UNKNOWN_CALL_TARGET` | `step.call` 既非已注册 Tool 也非「能力.方法」 | `gear_catalog(kind=tool)` 确认；或改直调（`gear_capabilities` 查清单）；确认能力不存在 → 走上报流程 |
| `UNKNOWN_CAPABILITY` | 能力名/方法名不存在 | 以 `gear_capabilities` 为准；`ext.*` 还须在 `extensions.yaml` 注册 |
| `DIRECT_CALL_LIMITED` | 直调原子能力却写了 `use_version` 或 `mode: service` | 去掉其一；长驻必须用 process runtime 的 Tool |
| `BAD_TEMPLATE` | `${}` 前缀非法 | 只允许 `${vars.x}` / `${steps.<id>.output.x}` / `${secrets.NAME}` |
| `BAD_WHEN` | `when` 里写了 `${}`，或表达式不可编译 | 去掉 `${}` 直接写变量名，如 `steps.s1.output.n > 1` |
| `BAD_DEPENDENCY` | 依赖不存在，或成环 | 检查 `depends_on` 与 `${steps.*}` 推断出的隐式依赖 |
| `BAD_FLOW_STRUCTURE` | 缺 id / 缺 call / mode 非法 / 并行标注冲突 | 对照 `gear_guide("authoring")` 的全字段语义 |
| `BAD_TOOL_STRUCTURE` | Tool 缺 id / runtime 未实现 / 无 versions | runtime 只支持 process / lua / go-host |
| `LUA_CAP_UNDECLARED` | **Lua entry 用了某个受门禁能力，而 `capabilities:` 没声明它**（诊断会点名能力与 `entry.lua:行号`） | 给该 Tool 加 `capabilities: [db, http, …]`，或删掉 entry 里那次调用。⚠ 这类错误原先只在**运行期**炸成 `attempt to call a non-function object`（看着像函数写错了，真因是漏声明），现在写作期即拒。**纯函数原子（time/template/crypto/id/validate）无需声明**；`ctx.json` / `ctx.caller` 不是能力，不在声明范围内 |
| `ROUTE_INVALID` | `api:` 声明的形状非法（`method` 不在 GET/POST/PUT/PATCH/DELETE 里，或 `path` 不以 `/api/` 开头 / 空段 / `{}` 没名字 / 占着内核资源根如 `/api/tools`） | 方法写大写；path 以 `/api/` 开头并换一棵不属于内核的子树（如 `/api/orders`）；占位符写 `{id}` 这种带名字的。**为什么必须挡在写作期**：net/http 对同名 pattern 的第二次注册是 panic，而注册发生在起服那一刻——一个 YAML 字段就能让服务起不来 |
| `ROUTE_CONFLICT` | 两个 Tool 声明了同一个 `method`+`path`（两边各自都合法，只是不能共存） | 诊断会点名两个 Tool，改任一即可。同一 method+path 只能有一个：**两处声明同一资源等于让起服时 panic** |
| `BAD_HOOK_STRUCTURE` | Hook 缺字段 / stage 非法 | stage ∈ before_flow/before_step/after_step/on_error/on_success/before_promote |
| `SCHEMA_NOT_OBJECT` | `input_schema`/`output_schema` 顶层非 object | 写标准 JSON Schema，不支持 `{"x":"string"}` 简写 |
| `BAD_PRIMITIVE_ARGS` | 直调入参有未知参数名或缺必填 | 以 `gear_capabilities` 里该能力的方法 schema 为准 |
| `MISSING_SERVE_HOLDER` | 这条 Flow 要用 `ext.serve` 侧车，却没有一条 `mode: service` 的 `ext.serve` 步持有它。**能写入、能 `gear_apply`，只在 `gear_app(start)` 时才炸 connection refused** | 加一条 `call: ext.serve` + `mode: service` 的步持有侧车；service step 不能作为依赖目标，故用一个 `mode: once` 的锚点把后续步推到下一层（见 `examples/agent-app/bundle.yaml`） |
| `BAD_YAML` | 某份文档不是合法 YAML | 常见原因：缩进不一致、冒号后缺空格、字符串里的特殊字符没加引号 |
| `UNKNOWN_KIND` | 某份文档的 `kind` 不是 App / Tool / Flow / Hook / Migrations | 第一份通常是 `kind: App` 清单，其余是 `kind: Tool` / `kind: Flow` / `kind: Hook`，还有可选的 `kind: Migrations`（库怎么演进，见 `gear_guide("authoring")` §4） |
| `BAD_APP_MANIFEST` | App 清单缺 id / 缺 `entry_flow` / 出现多份 App | 一个 bundle 只放一份 `kind: App`，并给上 id 与 entry_flow |
| `ID_NAMESPACE_VIOLATION` | bundle 里的 id 未带应用前缀 | 全部 id 以 `<app-id>__` 或 `<app-id>.` 开头 |
| `SCOPE_VIOLATION` | 运行期命名空间守卫（`--scope`）拦下的跨应用引用：本进程 scope 只认自己名下的已注册对象（Tool / Flow / Hook 的 action） | 把被拒的 id 改成 `<app-id>__` 或 `<app-id>.` 开头；或去掉 `--scope` 关闭守卫。**平台级对象不会被此码拦**：cap 原语与无前缀注册 Tool（`ext.serve` / `static_site.serve` 这类共享基建）第一段不归属任何 App，照常可用——若被拦，说明它的第一段确实撞了某个已注册 App 的 id。**注意**：这一码只在 CLI / Flow 层能查到，经 `ctx.call` 触发时错误已被 Lua 边界拍成文本 |
| `APPLY_CONFLICT` | 对象已存在且内容不同，而 `overwrite=false` | 确需覆盖就显式传 `overwrite=true`；否则换个 id（带应用前缀） |
| `ENTRY_EXE_UNRESOLVED` | process 型 Tool 的 entry 首 token 指向的可执行文件**不存在** | 改路径（Windows 下正斜杠、反斜杠都可，反斜杠不会被当转义符）。只有绝对路径才报此码；相对路径与裸名字只给告警——运行时 cwd / PATH 可能与校验时不同 |
| `PORTABLE_PATH_HARDCODED` | 应用定义里出现机器相关绝对路径（`C:\...` / `/Users/...` / `/d/gadern/...`，**含 Lua 脚本体内的 dsn**） | 改用相对锚点：db dsn 用相对库名（`mindstorm.db`→GEAR_APP_DATA）、process entry 用裸名（`gear serve ...`→exe 目录）、ext.serve dir 用相对安装根（`apps/<app>/www`→InstallRoot）；详见 `gear_guide("portability")` 与 REQ-APP-PORTABLE |
| `IN_USE` | 删除被引用的对象 | 错误信息会指出被谁引用，先解引用 |
| `MISSING_FILE` | `layout.files` 声明了但文件不存在 | 用你自己的文件工具先落盘（这是 `warnings` 里的提示，不影响 `valid`） |
| `MISSING_SECRET` | `${secrets.X}` 对应密钥未播种 | 请人在 CLI 播种（MCP 不写密钥）（`warnings` 里的提示，不影响 `valid`） |
| `SIDECAR_NOT_BUILT` | 声明的侧车二进制缺失 | 先构建（`build/*-sidecar.exe`），`gear_doctor` 可见（`warnings` 里的提示） |

### 运行期

| code | 触发 | 修法 |
|---|---|---|
| `SERVICE_FLOW_SYNC_RUN` | 对含 `mode: service` 的 Flow 用 `gear_run` | 改用 `gear_app(action=start)` |
| `EXECUTION_BUSY` | MCP 面在途执行已达并发上限（默认 8），`gear_run` / `flow__` 被背压拒绝 | 稍后重试，或先用 `gear_execution` 看现有执行进展。**这不是 YAML 问题**，别去改定义 |
| `FLOW_RUN_FAILED` | `gear_run(kind=flow)` 启动失败（非 service 拒绝类） | 读 `gear_execution` 定位；必要时 `gear_doctor` 看环境 |
| `TOOL_RUN_FAILED` | `gear_run(kind=tool)` 同步执行单个 Tool 失败 | 看返回的 message；入参按 `gear_capabilities` 的 schema 核对 |
| `INVOKE_FAILED` | `gear_invoke` 直调原子能力失败 | 方法名/参数以 `gear_capabilities` 的 schema 为准；`ext.*` 需已注册 |
| `INVOKE_BLOCKED` | `gear_invoke` 调用被禁能力（`secret.*`） | **不许绕过**：密钥只在 Flow 内部用 `${secrets.NAME}` / `ctx.secret.get`；播种走 CLI |
| `TX_BEGIN_FAILED` | `db.batch` 开启事务失败 | 同 `APP_START_FAILED` 的排查面：库不可写 / 连接被拒；先确认库路径可写再重试 |
| `TX_BATCH_FAILED` | `db.batch` 某条语句失败（属业务结果，**不是**异常：整批已回滚） | 读 `results` 定位第几条、什么错；`committed=false` 意味着**这一次写入按未发生处理**，修正后整批重跑（batch 无跨调用状态） |
| `TX_COMMIT_FAILED` | `db.batch` 提交失败（事务未生效） | 未提交即未写入：直接整批重跑；反复出现去 `gear_doctor` 看库权限/磁盘 |
| `TX_ROLLBACK_FAILED` | `db.batch` 回滚本身失败（**异常路径**：事务可能部分生效） | 判定「写入是否发生过」**不要只看本次返回**；先 `gear_doctor` 看库权限/磁盘，必要时人工核对库状态 |
| `APP_START_FAILED` | 起应用后短等窗口内不通 | 读返回的 `log_path` 末几行；常见：端口被占、侧车没 build、registry 路径不符 |
| `APP_NOT_FOUND` | `stop`/`status` 的 `app_id` 不在台账 | 先 `gear_app(action=list)` |
| `APP_LIST_FAILED` | 读应用台账失败 | 先 `gear_doctor` 看 `apps_dir` 是否可读 |
| `ORPHAN_APP` | `stop` 后端口仍未释放，或 pid 与端口判据不一致 | 如实上报 pid；`gear_doctor` 看谁占着；**不要把它当成功** |
| `APP_PRUNE_FAILED` | `gear_app(action=prune)` 清理台账失败（读目录 / 删文件） | 看返回的 `apps_dir` 是否可写；prune **只删已终止的台账记录**，不杀进程 |
| `UNCLASSIFIED` | 未能归类（只能靠读 message） | 按 message 的内容人工判断；这通常说明该错误路径还没做结构化 |

### MCP 工具调用层（通用码）

这些是「前置条件不满足」时各工具共用的兜底码（与 `gear_validate` 的字段级诊断不同，它们没有 `field`）。

| code | 触发 | 修法 |
|---|---|---|
| `BAD_ARGUMENT` | 必填参数为空 / 枚举值非法（如 `kind`、`action`、`target`） | 按工具描述里的取值改写；先用 `gear_catalog` 确认对象 id |
| `NOT_FOUND` | 对象 id 不存在（Tool / Flow / Hook / 执行记录） | 先用 `gear_catalog` / `gear_execution` 确认 id（注意应用前缀） |
| `UNKNOWN_TOPIC` | `gear_guide` 的 topic 不存在 | 用返回体里的 `topics` 字段（或 `resources/list` 的 `gear://guide/{topic}`） |
| `CATALOG_FAILED` | 读目录失败 | 先 `gear_doctor` 看 registry 是否可读 |
| `DESCRIBE_FAILED` | 读单个对象定义失败 | 先 `gear_doctor` 看 registry 是否可读 |
| `DOCTOR_FAILED` | 环境自检本身失败（读 registry 出错） | 检查 registry 路径与权限 |
| `DELETE_FAILED` | 删除失败（非 NOT_FOUND / IN_USE） | 看 message 与 hint |
| `APPLY_FAILED` | 落库写入失败（非校验类，如写库错误） | 看 message；先 `gear_doctor` 确认 registry 可写 |
| `PROMOTE_FAILED` | 版本晋升失败（非非法目标状态） | 看 message；确认 Tool 与版本号存在 |
| `EXPORT_FAILED` | `gear_export` 失败（应用不存在 / 目标目录非空 / 写文件失败） | 应用不存在先 `gear_catalog(kind=app)` 确认；**只有 Flow 没有 App 清单的应用直接用入口 Flow id 也能导**（`<id>` / `<id>.up`）；**dir 形态的目标必须不存在或为空**（gear 不会为一个可重新生成的产物删未知目录） |
| `IMPORT_FAILED` | `gear_import` 失败（格式不认识 / 产物损坏 / 落库冲突） | 看具体码：`ARTIFACT_VERSION_UNSUPPORTED` 与 `ARTIFACT_INVALID` 都从这里报出 |
| `ARTIFACT_VERSION_UNSUPPORTED` | 产物的 `gear-artifact.yaml` 里 `version` 不是本 gear 支持的版本 | 用同版本 gear 重新导出，或升级 gear 再导入。**不猜、不静默降级**——导一半的库比拒绝难收拾 |
| `ARTIFACT_INVALID` | 产物缺文件 / 元信息损坏 / 定义解析不过 / `entry_flow` 无法解析 | 重新 `gear_export` 一份；确认传的是产物**根目录**（含 `gear-artifact.yaml`/`app.yaml`/`bundle.yaml`/`kind: Migrations` 那份若带迁移） |
| `MIGRATE_INVALID` | bundle 的 `kind: Migrations` 声明不合法（version 未严格递增 / sql 空 / 缺 dsn / version 非正） | 版本号从 1 起**依次 +1**；每条都要有非空 `sql`，`dsn` 必须写（迁移的目标库不能是内存 scratch 库）。`gear_validate` 会在这条报出并带 `migrations[i]` 字段 |
| `MIGRATE_VERSION_GAP` | 声明的 version 与库里已应用的接不上（中间缺号） | 例：库里已应用到 1，本次声明里出现 3 → 补上 2 或重新编号。**不要**为了让导入通过而跳号：跳号意味着那条中间迁移从未执行过 |
| `MIGRATE_SQL_FAILED` | 某条迁移的 SQL 执行失败（该条已整条回滚） | 错误信息里带 `version=N(name)` 与数据库原始错误；修 SQL 后整条重跑（`gear_apply` 会拒掉整包，定义一个都不落库）。MySQL 上 DDL 会隐式提交，**回滚不保证生效**，先拿 `--dry-run` 验 |
| `MIGRATE_NO_DB` | bundle 声明了 `migrations:` 但本次导入没有 db 实现 | 换用带宿主 db 的入口（如 `gear serve` 起的 MCP / `gear import`）；本码刻意 fail-closed：宁可拒绝，也不让「声明了迁移却没跑」成为合法状态 |
| `CAPABILITIES_UNAVAILABLE` | 能力目录取不到（引擎未装配 AIAgent，或读取失败） | `gear_doctor` 看 `extensions` 块 |

### REST 面（HTTP 状态映射的兜底码）

REST 错误响应是**结构化**的（2026-10-03，此前只有 `{"error": "一整段文本"}`）：

```json
{ "error": "step wms__list 执行失败: lua 执行失败: <string>:5: attempt to call ...",
  "code": "TOOL_RUN_FAILED",
  "message": "step wms__list 执行失败: lua 执行失败: <string>:5: attempt to call ...",
  "detail": "stack traceback:\n\t[G]: in function 'call'...",
  "hint": "检测到你把入参包了一层 {\"input\": {...}}……" }
```

- `code` 的取法与 MCP 面**同源**：错误链里有结构化诊断就用诊断里的真码，没有才按
  HTTP 状态兜底（下面这张表），都没有就明说 `UNCLASSIFIED`（不假装知道原因）。
- `message` 与 `detail` 拼起来与旧的一整段文本**逐字相同**——拆分只是让人读的主句与技术噪音分开，**不丢信息**。
- `error` 是**兼容位**（= `message`）：既有前端与脚本读它，删掉是破坏性变更。新代码读 `code`/`message`。
- `hint` 只在真能给下一招时出现（如「你的入参包了一层 `input`，本端点要扁平 body」），不塞占位文本。

| code | 对应 HTTP | 触发 |
|---|---|---|
| `BAD_REQUEST` | 400 | 请求体不合法、入参校验失败、Tool 执行失败等**无诊断**的 4xx |
| `NOT_FOUND` | 404 | 对象不存在（Tool / Flow / Hook / 执行记录） |
| `METHOD_NOT_ALLOWED` | 405 | 方法不匹配 |
| `CONFLICT` | 409 | 引用守卫（被 Flow/Hook 引用、被 App 占用）拒绝删除 |
| `EXECUTION_BUSY` | 429 | 并发达上限且队列已满（同 MCP 面同名码） |
| `INTERNAL` | 500 | 读库/写库失败等内部错误 |
| `UNAVAILABLE` | 503 | 依赖未就绪（密钥源未初始化等） |

> 有诊断的错误**不会**落到这张表：例如 `ROUTE_INVALID`、`TX_ROLLBACK_FAILED` 一律用诊断里的真码。

### 三个「不是错误码，但必须先看」的坑

- **`ext.ai.*` 报厂商 401**（`LLM HTTP 401 ... Token is invalid.`）：这是**凭据没配**，不是你的
  YAML 写错了。厂商的报错文本里没有病因，先跑 `gear_doctor` 看 `ai` 块（`ready` / `blocked` /
  `how_to_fix`）——它只说密钥**在不在**，不回显明文。
- **process Tool 起不来报 `executable file not found in %PATH%`**：先怀疑**路径写错**而不是 PATH。
  `gear_validate` 对绝对路径会直接报 `ENTRY_EXE_UNRESOLVED`；它没报却仍起不来，才去看 PATH 与 cwd。
- **应用自带的 REST 面 `/api/flows` 返回 `[]`，而 CLI `flow list` 有数据**（2026-09-22）：
  **不是 REST 坏了，是那个进程读的是另一个 registry**。判据按顺序看两条——
  ① `gear serve` 起服 banner 的 `flows=N, tools=N`（`flows=0` 即指错库）；
  ② `gear_doctor` 报的 `missing_sidecars` / 端点 `apps_dir` 会暴露它实际用的目录。
  病因几乎总是 entry 里**写死了相对 `--registry`**：子进程按自己的 cwd 解析，读到另一个库。
  修法：**删掉 entry 里的 `--registry`**——起服进程会把生效的 registry 经 `GEAR_REGISTRY`
  注入应用内的一切步骤，于是它与 CLI 必然同源。要固定就写绝对路径（显式值优先）。

## 拿不到现场时：CLI 诊断日志（--log-file）

MCP 面上跑挂的执行，**服务端现场**在 CLI 侧：让用户在（或自己经 shell）跑

```
gear <子命令> --log-file /tmp/gear-diag.log
```

档案里记：命令路径 / pid / cwd / **生效的 registry** / 版本、脱敏后的 flag
（`--api-key` 打码）、截断的位置参数、起止耗时、失败原因；内核侧还有 Flow 执行
（flow+exec_id+步数+耗时）与 REST 面监听/停止（主动停机 vs 异常退出分开记）。

三条使用纪律：**空 `--log-file` = 不写**（默认零行为变化）；`--log-level` 写错会
直接失败（别静默兜底）；**文件开不了时主命令照常跑、stderr 明说没有档案**——此时
不要假装有现场。注意它与执行进度（`✓ step …`，stdout）是两层：stdout 仍可管道。

## 一条纪律

**任何错误都不允许「绕过」**：不能因为 `UNKNOWN_CALL_TARGET` 就用 shell 外调，不能因为
`MISSING_SECRET` 就把明文写进 YAML。宁可如实报阻塞，也不伪造一个能跑的假象。
