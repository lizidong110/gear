# security：凭据、密钥与破坏性操作

## 1. 凭据只从调用参数传

- **唯一合法写法是 `${secrets.NAME}`**（Flow 的 `input` 里）或 `secret.get{name}`（Lua 里）。
- **绝不内联明文**：写进 YAML 的密钥会进 registry、进执行记录、进对话上下文。
- **扩展原子禁止读环境变量**（有机械守卫 `TestExtAtomsMustNotReadEnv`）——这是红线，不是建议。

## 2. 密钥由人播种，MCP 不写

`gear_app` / `gear_apply` **不提供密钥写入**。bundle 只声明 `requires.secrets: [NAME]`，
`gear_apply` 检查它是否已存在，**缺了就在 `warnings` 里报 `MISSING_SECRET`**，由人在 CLI 播种。

这是**刻意保留的人机交接点**：明文一旦经 MCP 传入，就会同时进对话记录、模型上下文与执行记录库；
而 MCP 的 `--http` 目前**没有鉴权**。所以：

- 需要密钥的任务，**恰好一次**人工介入是设计的一部分（不是缺陷）；
- 报告里要明确写出「需要人播种哪个密钥名」，而不是自己想办法绕过去。

## 3. 网络暴露面

- `gear mcp --http` **只应绑 127.0.0.1**（文档已警告）；当前无鉴权，暴露公网等于把 registry 与执行面交出去。
- 应用起服务时默认建议 `host: 127.0.0.1`；改成 `0.0.0.0` 要**明确说明**这会让同网段可访问。
- 静态站托管的是**你自己的目录**：确认 `dir` 里没有不该对外的东西（如 `.env`、密钥文件、源码）。

## 4. 破坏性操作要先列影响面

| 操作 | 风险 | 要求 |
|---|---|---|
| `gear_delete(kind=tool/flow/hook)` | 被引用时会失败（`IN_USE`），但删除本身不可逆 | 先 `gear_describe` 看它被谁引用，再动手 |
| `gear_app(action=stop)` | 停掉正在跑的应用 | 先 `gear_app(action=status)` 确认是哪个实例 |
| `gear_promote(target=deprecated)` | 影响后续对默认版本的引用 | 说明影响范围 |
| `gear_apply(overwrite=true)` | 覆盖人工改过的对象 | 默认 `overwrite=false`；要覆盖须显式说明改了哪些 |
| `gear_app(action=start, replace=true)` | 先停旧实例再起新的 | 会造成一次服务中断 |

## 5. 命名空间是防误覆盖的护栏

**所有 id 必须以 `<app-id>__` 或 `<app-id>.` 开头**（`gear_apply` 强制校验，不符即拒
`ID_NAMESPACE_VIOLATION`）。

理由：registry 与 CLI/REST 共享同一个库（默认 `~/.gear/registry.db`），**你的改动与人手工维护的对象
互相可见**。没有前缀约束，一次性生成很容易覆盖掉别人的东西。

## 6. 应用台账与日志是谁的文件

`<registry 同目录>/apps/*.json` 与 `*.log` 是 **gear 自己管理的运维数据**，不是用户文件：
不要手动编辑（会与内存态不一致），也不要当业务数据引用。要看用 `gear_app(action=list/status)`。
