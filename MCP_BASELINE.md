# MCP 垂直切片记录（2026-09-26）

> **目的**：不靠读代码推测「MCP 面够不够用」，而是让一个通用智能体（WorkBuddy）用**真 MCP 客户端**
> 调 `gear_*` 工具，从零建出一个**可访问**的应用。全程不手写 registry、不改 CLI、不绕过工具写库。
>
> 结论先行：**够用。链路 7 步一步通，端口机制甚至比现有应用更优雅。但暴露出三处小口子，
> 都是「文档/校验/诊断不一致」类，都能单独修。**

## 1. 链路（全部通过）

| # | 步骤 | 工具 | 结果 |
|---|---|---|---|
| 0 | 读手册 | `gear_guide(authoring)` | 返回全字段语义、命名规范、回滚退路 |
| 1 | 校验定义 | `gear_validate` | `valid:true`，**0 diagnostics / 0 warnings** |
| 2 | 落库 | `gear_apply` | `created: [app:slice, tool:slice__config, tool:slice__api, tool:slice__site, flow:slice.up]` |
| 3 | 起服 | `gear_app(start, flow_id=slice.up, ports=[8100])` | `state:running`、`escaped_job:true`、端口 `listening:true` |
| 4 | 访问静态站 | `gear_invoke(http.Request, GET /)` | **200**，返回页面正文 |
| 5 | 验证反代 | `gear_invoke(http.Request, GET /api/flows)` | **200**，返回真实 registry 内容 |
| 6 | 状态与停止 | `gear_app(status)` / `gear_app(stop)` | `running`(uptime 17s) / `stopped`，`ports_released:true` |

被建出来的应用 `slice`（4 步 Flow + 3 个 Tool + 1 个 App 清单），结构：

```
anchor(once) → serve(ext.serve, service) → api(slice__api, service) → site(once, depends_on: anchor)
```

其中 `api` 的 entry 是 `gear serve --addr 127.0.0.1:0`（**让内核分配端口**），`site` 的 proxy target
写成 `http://127.0.0.1:${steps.api.output.port}`。起服日志显示该端口被解析成 `32328` 并成功连通——
**「端口不写死」这条路是真通的**，而 `mindstorm.up` 目前是写死 `8790`。

## 2. 过程中真实踩到的三处口子

### A. `gear_validate` 不拦「缺 ext.serve 持有步」

第一版 bundle **没有** `serve`（`mode: service` 的 `ext.serve`）那一步。结果是：
`gear_validate` 通过、`gear_apply` 也成功写入，**只在 `gear_app(start)` 时才炸**：

```
Error: step site 执行失败: ... 调用 sidecar(http://127.0.0.1:8095) 失败: ... connectex:
No connection could be made because the target machine actively refused it.
```

规则本身在手册里写得清清楚楚（`service-app.md` §2：「删掉 anchor 会让 site 与 serve **同层并行**，
site 的『下发 /start』可能抢在侧车握手完成前 → connection refused」）。**手册有规则，校验器没有对应诊断。**

### B. 起服失败时，诊断指向了错误方向

`gear_app(start)` 返回 `startup_failed`，hint 是「先用 gear_doctor 看环境」。而 `gear_doctor` 报的
三条 `missing_sidecars` 是 **email / sms / wecom**——与本次故障**一个都不相关**；真正相关的 `ext.serve`
侧车是**找到的**（`build/plugin/serve-sidecar.exe` 存在），所以没进缺失清单。

于是「按提示去做自检」这条路会把人带到三条无关项上，只有去读 `last_logs` 才能看到真正的
`connection refused`。**失败归因不在错误里，在日志里。**

### C. `start.ports` 写了没有效果，只有显式入参才生效 → **已修复（2026-09-27）**

当时：bundle 里写了 `start.ports: [8100]`，但 `AppStart.Ports` 在 **全仓没有任何消费点**（只有
`internal/app/launch.go` 消费 `StartOptions.Ports`，即 `gear_app(start, ports=…)`
这一个入参）。漏传 `ports` 时 `gear_app(status)` 的端口判据就失效。而手册
`service-app.md` §5 写的是「bundle 里把端口写进 `start.ports`」——两边说法不一致。

**收口**：回退链固定为「起服入参 > App 清单 `start.ports` > 无」，入参缺省时按 entry_flow
反查 App 清单（恰好一个 App 命中才回退，歧义即放弃），生效来源经 start 返回的
`ports_source: explicit|bundle|none` 如实上报。实现见 `internal/app/ports_lookup.go`。

## 3. 已知边界（不是缺陷，是刻意设计）

- **MCP 不落静态资源文件**。`slice` 的 `www/index.html` 是用文件工具写的（`service-app.md` 明写这条边界）。
  所以「智能体通过 MCP 建应用」当前只能建出**后端 + 定义**，界面这一半要靠别的通道。
  这是本轮最值得注意的一条：它决定了「MCP 建应用」的成品形态上限。
- **没有 HTTP 探活工具**，`gear_invoke(http.Request)` 手工 GET 是唯一 MCP 内的验证路径（可用，够了）。
- **密钥只能声明不能写**（`gear_apply` 明拒），播种在 CLI。

## 4. 判定

| 维度 | 判定 |
|---|---|
| 意图级工具面（15 个 `gear_*`） | **够**。覆盖 查→写→验→起→跑→看→停 全环 |
| 手册能否把人教明白 | **够**，A 那次失败是没按 §2 写，不是手册没说 |
| 端口机制 | **够，且优于现有应用**（`${steps.*.output.port}` 不写死） |
| 错误诊断 | **有缺口**：B 的归因错位、A 的校验缺失、C 的文档漂移 |
| 端到端生产一个**带界面**的应用 | **不够**——界面文件不在 MCP 的能力范围内，需先就这条拍板 |

三处口子里，**C 最容易埋雷**（文档让人以为写了就生效），**A 最影响首次成功率**，
**B 最费排查时间**。三者都可单独修，A+B 加起来不到半天，C 是三行代码加一条测试。
