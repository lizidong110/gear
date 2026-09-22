# rest-api：REST 执行面 —— 做「带界面的应用」时的对外契约

MCP 面管**你自己**（智能体）怎么操作 Gear；REST 面管**你造出来的东西**（网页、脚本、别的程序）
怎么操作 Gear。做带 UI 的应用时，前端只能走 REST，所以这份契约是必需品而不是参考。

## 1. 地址与启动

- 默认 `127.0.0.1:8787`，用 `--addr` 或 `GEAR_ADDR` 覆盖。
- 两种起法：`gear serve --addr 127.0.0.1:8787`（前台），或写成一个 `runtime: process` +
  `mode: service` 的 Tool，让应用自己把它当后端拉起来（参考 registry 里的 `web_ui.api`）。
- **`gear_app(start)` 起的应用与 `gear serve` 是两个进程**：前者跑 Flow，后者才是 REST 面。
  一个 Flow 里同时管住「静态站 + REST 后端」两个长驻服务是标准做法（见 §4）。
- **应用内的步骤不要写 `--registry`**（2026-09-22）：起服进程会把本次生效的 registry
  **绝对化**后写进环境变量 `GEAR_REGISTRY`，应用里的一切进程（含长驻 Tool）自动继承 ⇒
  **应用自带的 REST 面与你 CLI 的库必然同源**。写死相对值（如 `--registry ./ui-registry.db`）
  的子进程按**自己的 cwd** 解析，会读到另一个库——症状是 `/api/flows` 返回 `[]` 而
  `gear flow list` 有数据，且**完全静默**（页面只是空列表，日志一个字都没有）。
  真要在应用内固定一个库就写**绝对路径**：显式值仍然优先，机制只保证「没说的时候永远一致」。
- **`gear serve` 的 banner 带 `flows=N, tools=N`**：`flows=0` 就是「指错库了」的一眼判据
  ——从前只打 registry 路径，空库与满库长得一模一样。

## 2. 端点表

执行：

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/api/tools/{id}/run` | **同步**执行单个 Tool，body = 入参 map，返回 `{id, output}` |
| POST | `/api/executions` | **异步**跑 Flow，返回 `202 + {id}`；繁忙 429 |
| GET | `/api/executions` / `/{id}` / `/{id}/logs` | 执行列表 / 结果 / 日志 |
| POST | `/api/capabilities/invoke` | 直接执行一次原子能力，body `{name, method, args}` |
| GET | `/api/capabilities` | 能力目录（与 `gear_capabilities` 同源） |

**同步还是异步，一句话**：跑单个 Tool 用 `/api/tools/{id}/run`（像调函数）；跑 Flow 用
`/api/executions`（长编排、多步、要日志，异步 + 轮询）。前者是 2026-09-22 补上的——此前
REST 只有异步入口，前端想复用 Tool 只能把它包成 Flow 再轮询，或干脆绕过 Tool 裸调原子。

注册面（与 MCP 的 apply/delete 等价）：

| 方法 | 路径 |
|---|---|
| GET / POST / DELETE | `/api/tools`、`/api/tools/{id}` |
| POST | `/api/tools/{id}/versions/{v}/promote`（body `{target}`） |
| GET / POST / DELETE | `/api/flows`、`/api/flows/{id}` |
| POST | `/api/flows/{id}/validate`（只校验不落库） |
| GET / POST / DELETE | `/api/hooks`、`/api/hooks/{id}` |
| GET | `/api/healthz`（`{status,tools,flows}`） |

进程与密钥：

| 方法 | 路径 | 说明 |
|---|---|---|
| POST / GET / DELETE | `/api/apps` | 托管长驻 Flow 进程（等价 `gear app run`，**本进程内存态**，进程一退即消失） |
| POST | `/api/apps/prune` | 清理**落盘台账**里确定已终止的记录（body `{flow_id?, dry_run?}`，空 body=全量真删）。等价 `gear_app(action=prune)`；只清 `exited`/`stopped`/`startup_failed`，`running`/`unknown` 保留，**只删记录不杀进程**。⚠ 它动的是 `gear app start` 的持久台账，与上面那行的内存 `AppInstance` **不是同一个模型**，别混用 |
| GET / PUT / DELETE | `/api/secrets`、`/api/secrets/{name}` | **只回名字，绝不回明文**；写入 body `{value}` |
| POST | `/api/events/{handle}` | 长驻订阅的事件回推入口（侧车/外部源用） |

## 3. 安全边界（默认就能挡，别绕过）

REST 面**只绑回环**，但「绑回环 ≠ 安全」：浏览器里任意网页都能向 `127.0.0.1` 发跨站请求。
所以：

1. **`Origin` 非本机来源一律 403**（`127.0.0.1` / `localhost` / `::1` 放行）。
   不带 `Origin` 的请求（curl、CLI、服务端调用）照常放行——零配置挡跨站，不给命令行加门槛。
2. **`secret.*` 禁止经 `/api/capabilities/invoke` 调用（403）**。它只能经 Flow 内部的
   `${secrets.NAME}` / `ctx.secret.get` 使用。密钥的写入走 `/api/secrets/{name}` 或
   `GEAR_SECRET_*`（人来做），MCP 面不写密钥。
3. 这**不是完备鉴权**：本机任意进程仍可调用。要把 REST 面暴露到回环之外时，先自己加一层
   鉴权（Gear 不提供 token 体系）。

## 4. 做带界面的应用：同源反代（唯一可行架构）

Gear 没有「把 HTTP 请求路由到某个 Tool 并同步返回」的能力，所以前端**不能**直接挂到 Gear 上。
可行解是 `ext.serve` 的「静态托管 + 按前缀反代」：

```yaml
steps:
  - {id: anchor, call: myapp__config}                       # once：只为排序
  - {id: serve,  call: ext.serve, mode: service}            # 持有侧车
  - {id: api,    call: myapp__api, mode: service}           # 起 REST 后端（process 长驻）
  - {id: site,   call: static_site.serve, depends_on: [anchor],
     input:
       port: 8088
       dir: /abs/path/to/apps/myapp/www                     # 必须绝对路径
       host: 127.0.0.1
       proxy: [{path: "/api", target: "http://127.0.0.1:8788"}]}
```

于是页面与接口**同源**（都在 8088），无跨域；`/api/**` 命中反代，其余回落静态文件。
前端就写 `fetch('/api/tools/myapp__ideate/run', {method:'POST', body:'{"topic":"…"}'})`。

> 把 REST 后端与静态站放进**同一个 Flow**，好处是 stop 一次全停；坏处是「后端没起来」与
> 「页面 404」会混在一条日志里 —— 排障时先看 `gear_doctor` 的端口段。

## 5. 静态资源不归 MCP 管

MCP 不提供文件读写：`www/` 下的文件要用你自己的文件工具落盘，bundle 里的
`layout.files` 只做**存在性校验**，不搬运。「纯 MCP 建一个应用」做不到，这不是漏了工具，
是刻意的职责边界。
