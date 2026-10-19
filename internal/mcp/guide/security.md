# security：凭据、密钥与破坏性操作

## 1. 凭据只从调用参数传

- **唯一合法写法是 `${secrets.NAME}`**（Flow 的 `input` 里）或 `ctx.secret.get("NAME")`（Lua 里，位置参数）。
- **绝不内联明文**：写进 YAML 的密钥会进 registry、进执行记录、进对话上下文。
- **扩展原子禁止读环境变量**（有机械守卫 `TestExtAtomsMustNotReadEnv`）——这是红线，不是建议。

## 2. 密钥由人播种，MCP 不写

`gear_app` / `gear_apply` **不提供密钥写入**。bundle 只声明 `requires.secrets: [NAME]`，
`gear_apply` 检查它是否已存在，**缺了就在 `warnings` 里报 `MISSING_SECRET`**（该告警里带可复制的
播种命令与密钥库落点路径），由人播种。播种有**两条**既有通道，写入同一个库：

| 通道 | 写法 | 落点 |
|---|---|---|
| CLI | `gear secret set <NAME> <值>` | `<GEAR_HOME>/secrets.db` |
| REST 执行面 | `PUT /api/secrets/<NAME>`，body `{"value":"…"}` | 同上（`gear secret path` 可查实际路径） |

这个「恰好一次人工介入」是**刻意保留的人机交接点**，不是缺陷：明文一旦经 MCP 传入，就会同时进
对话记录、模型上下文与执行记录库；而 MCP 的 `--http` 目前**没有鉴权**。所以：

- 需要密钥的任务，**恰好一次**人工介入是设计的一部分；
- 报告里要明确写出「需要人播种哪个密钥名」，而不是自己想办法绕过去；
- **不要绕**：REST 的 `PUT /api/secrets` 是给宿主/运维用的本机通道，不是给智能体的入口。

## 3. 网络暴露面

- `gear mcp --http` **只应绑 127.0.0.1**（文档已警告）；当前无鉴权，暴露公网等于把 registry 与执行面交出去。
- **`gear serve` 的 REST 执行面同样无鉴权**：只绑回环 + 校验 `Origin`，而**不带 `Origin` 的请求照常放行**
  ⇒ 同一台机器上的任意进程都能调你的执行面（读写 registry、明文 secrets、执行编排）。
  这是**设计姿态**（平台面只服务本机/受信任环境），不是待修的缺陷：
  **在这个面上开发的业务应用必须自建登录与权限**（终端用户走 `/api/tools/{id}/run` 与
  `/api/executions`，用 Hook 拦得住，fail-closed——见 `gear_guide("authoring")` 与
  `examples/auth-app`）。不要把「Gear 会管鉴权」写进你的应用设计。
- 应用起服务时默认建议 `host: 127.0.0.1`；改成 `0.0.0.0` 要**明确说明**这会让同网段可访问。
- 静态站托管的是**你自己的目录**：确认 `dir` 里没有不该对外的东西（如 `.env`、密钥文件、源码）。

## 4. 破坏性操作要先列影响面

| 操作 | 风险 | 要求 |
|---|---|---|
| `gear_delete(kind=tool/flow/hook/app)` | 被引用时会失败（`IN_USE`），但删除本身不可逆。注：`kind=flow` 还会在它是某个 App 的 `entry_flow` 时拒绝（否则 App 定义悬空） | 先 `gear_describe` 看它被谁引用，再动手 |
| `gear_app(action=stop)` | 停掉正在跑的应用 | 先 `gear_app(action=status)` 确认是哪个实例 |
| `gear_promote(target=deprecated)` | 影响后续对默认版本的引用 | 说明影响范围 |
| `gear_apply(overwrite=true)` | 覆盖人工改过的对象 | 默认 `overwrite=false`；要覆盖须显式说明改了哪些 |
| `gear_app(action=start, replace=true)` | 先停旧实例再起新的 | 会造成一次服务中断 |

## 5. 命名空间是防误覆盖的护栏

**所有 id 必须以 `<app-id>__` 或 `<app-id>.` 开头**（`gear_apply` 强制校验，不符即拒
`ID_NAMESPACE_VIOLATION`）。

理由：registry 与 CLI/REST 共享同一个库（默认 `~/.gear/registry.db`），**你的改动与人手工维护的对象
互相可见**。没有前缀约束，一次性生成很容易覆盖掉别人的东西。

### 5.1 运行期守卫：`--scope`（默认不开）

上面的前缀约束是**写入期**的，它保证「不该出现在库里的别出现」。但「库里已经有一个别人应用的
`other__x`」拦不住——运行期照样能调它。加 `--scope <app-id>` 就是在运行期补这一层：

```
gear app start mindstorm.up --scope mindstorm --port 8090 --wait 10
gear flow run --scope mindstorm mindstorm.up
```

- **默认不开**（不传就是不加限制）。应用往往先存在、后挂 App 清单（`apps/mindstorm` 的 Tool 与
  `apps` 表是两条独立路径），默认开会让存量应用立刻跑不起来。
- 开了以后 fail-closed：Flow 引用的 Tool、Hook 的 action、`ctx.call` 的 target，只要不属于该
  应用就拒绝（`SCOPE_VIOLATION`）。
- **原语直调不受影响**（`static_site.serve`、`ext.serve`、`db.query`）——它们不是 registry 对象，
  是 gear 自带的平台能力。守的语义是「别碰**别人应用的定义**」，不是「别用平台」。
- 作用域是**进程级**的，不是按请求切换的（`gear serve` 的引擎是长驻共享实例，而 `ctx.call`
  的解析器签名里没有 context）。`gear app start` 会把 `--scope` 一并传给它派生的监管进程——
  那个进程才是真正跑 Flow 的那个，漏传就等于「表面加了守卫」。

## 6. 应用台账与日志是谁的文件

`<registry 同目录>/apps/*.json` 与 `*.log` 是 **gear 自己管理的运维数据**，不是用户文件：
不要手动编辑（会与内存态不一致），也不要当业务数据引用。要看用 `gear_app(action=list/status)`。
