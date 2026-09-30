# service-app：长驻应用、侧车与端口

## 1. 什么是「应用」

**没有 App 实体。** 应用 = **一个含 `mode: service` 长驻 step 的 Flow**，由 `gear_app(action=start)`
派生一个独立后台进程（`gear app run`）来监管。写入（`gear_apply`）与运行（`gear_app start`）是**两个独立动作**。

三种 step 形态：

| 形态 | 行为 | 典型用途 |
|---|---|---|
| `once`（默认） | 执行一次即结束，返回值进 `result.steps.<id>.output` | 抓数据、写文件、调 API |
| `service`（长驻） | 拉起后常驻不退出；退出后按 `--restart` 策略重启 | 起 HTTP 服务、持有侧车 |
| `once` + 依赖 anchor | 排在长驻步之后执行 | **向已就绪的侧车下发指令** |

## 2. 为什么需要 anchor

**service step 不能作为依赖目标**（长驻不结束，依赖它就没有"完成"信号）。所以「等侧车起来再下指令」
必须靠一个 **once 锚点步**把后续步推到下一层：

```yaml
steps:
  - id: anchor                 # once：只为排序，本身干不干事都行
    call: myapp.config
    input: {}
  - id: serve                  # 长驻：持有 ext.serve 侧车
    call: ext.serve
    mode: service
    input: {}
  - id: site                   # 依赖 anchor → 落到下一层 → 此时侧车必已就绪
    call: static_site.serve
    depends_on: [anchor]
    input: {port: 8080, dir: /abs/path/www, host: 127.0.0.1}
```

删掉 `anchor` 会让 `site` 与 `serve` **同层并行**，`site` 的「下发 /start」可能抢在侧车握手完成前 → `connection refused`。

## 3. 侧车（service ext）

`ext.serve`、`ext.kafka`、`ext.mqtt` 等 `service: true` 的扩展原子，运行时是一个**常驻侧车进程**：

- 它由**引擎**在 service step 处拉起，绑定 `extensions.yaml` 里声明的 `listen` 端口（`listen` 是协调地址的唯一真相源）。
- service step「持有」它——占用列表非空时侧车不会被回收；进程退出后占用清空，侧车自动退出释放端口。
- 侧车由 `build/*-sidecar.exe` 提供，**需先构建**（`bash scripts/build.sh`）。缺了用 `gear_doctor` 一看便知。
- `listen` 端口被占会让侧车起不来 → `gear_app(start)` 返回 `startup_failed` 并附日志尾部。

## 4. `gear_app` 生命周期细节

```
gear_app(action: start|stop|status|list, flow_id?, app_id?, restart?, replace?, wait_seconds?)
```

| action | 关键行为 |
|---|---|
| `start` | 幂等（同 Flow 已有活跃实例 → `already_running`，不抢端口）；派生 detach 进程后**立即返回**；`wait_seconds>0` 时短等确认，窗口内进程即退出 → `startup_failed` + 日志尾部 |
| `status` | **双判据**：pid 存活 + 声明端口探活。两者一致才给 `running`/`exited`；不一致给 `unknown`（不猜、不杀） |
| `stop` | 杀进程树 → **端口复检**（最多等 5s）。收不干净 → `ORPHAN_APP` + pid，**不谎报 stopped** |
| `list` | 全部台账（含已停历史）+ `orphan`：`pid-gone`（进程悄悄死了） / `port-still-listening`（进程没了端口还占着） |

**start 的返回**：`{app_id, flow_id, pid, state, log_path, ports:[{port,listening}], last_logs?}`。

## 5. `start.ports` 为什么要声明

端口声明用于①台账探活、②`stop` 后复检释放。不声明则只按 pid 判存活——能用，但
「进程在、端口没起」这类半死状态就检测不出来。

**生效来源（2026-09-27 起，REQ-MCP-PORTS-ONE-TRUTH）是一条回退链**：

```
起服入参（gear_app(start, ports=[...]) / gear app start --port N）
  > App 清单的 start.ports
  > 无（只按 pid 判存活）
```

- 入参**优先**于清单：临时换端口不用改 bundle。
- 入参缺省时自动回退清单（按 entry_flow 反查；**恰好一个** App 命中才回退——多个 App
  共用同一 entry_flow 时歧义即放弃，请显式传）。
- start 的返回里 `ports_source: explicit|bundle|none` 如实说明本次用了哪份；
  排查「探活为什么没生效」先看它。

## 6. App Bundle（交付形态）

「创建应用」建议用一个多文档 YAML 交付：第一份是 `kind: App` 清单，其余是标准 Tool/Flow/Hook 文档。

```yaml
kind: App                       # 仅 apply/validate 识别，不入 registry
id: mynews
name: 我的新闻站
description: 抓热搜 + 静态站托管
entry_flow: mynews.up           # 应用入口 Flow（必填）
layout:
  workdir: apps/mynews          # 约定工作目录（你自己用文件工具落盘到此）
  files:                        # 声明需要存在的文件（apply 只校验存在性，不搬运）
    - {path: apps/mynews/www/index.html, required: true}
start:                          # 交给 gear_app(start) 的启动参数
  restart: always               # always | never
  ports: [8080]                 # 声明端口 → 台账探活 + stop 后复检
requires:
  secrets: [NEWS_API_KEY]       # 需要人播种的密钥名（apply 只检查是否已存在；MCP 不写密钥）
  sidecars: [serve]             # 依赖的侧车 → 校验 build/serve-sidecar.exe
  capabilities: [http, ext.serve]
---
kind: Tool
id: mynews__fetch
# …标准 Tool 文档
---
kind: Flow
id: mynews.up
steps:
  - {id: anchor, call: mynews__fetch, input: {}}
  - {id: serve, call: ext.serve, mode: service, input: {}}
  - {id: site, call: static_site.serve, depends_on: [anchor],
     input: {port: 8080, dir: apps/mynews/www, host: 127.0.0.1}}
```

**边界（避免期待错误）**

- **MCP 不提供文件读写**。静态资源用**你自己的文件工具**落盘；bundle 只声明 `layout.files`，`gear_apply` 校验存在性，缺了在 `warnings` 里报「文件未就绪」。
- **命名空间强制**：bundle 内所有 id 必须以 `<app id>__` 或 `<app id>.` 开头，不符即拒（`ID_NAMESPACE_VIOLATION`）——防止一次性生成覆盖别人手工维护的对象。
- **幂等**：`gear_apply` 是 upsert，响应区分 `created` / `updated`。
- **`start` 只是声明**：apply 不启动应用。
