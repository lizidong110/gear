# App 注册手册：Flow、App 与 registry 三张表

> 回答三个问题：**Flow 和 App 什么关系？怎么注册 App？只导入了 Flow 为什么还不是 App？**
> 行为规格以 `REQUIREMENTS.md` 为准（REQ-APP-SCOPE / REQ-APP-SCOPE-LISTVIEW / REQ-APP-ARTIFACT）；
> 本手册讲「怎么用、为什么」。

## 0. 一分钟版

- **Flow = 怎么跑**（steps 的有向图）；**App = 这是什么**（一份清单：户主 id、入口 Flow、端口、静态资源、依赖）。
- 两者在 registry 里**各写各行**：Flow 进 `flows` 表，App 进 `apps` 表，互不产生。
- **App 行只有一个写入口**：`gear apply`（CLI）/ `gear_apply`（MCP）。`gear tool register` / `gear flow register`
  **永远不写 App 行**——这就是「我导入了一个 flow，为什么还不是 app」的直接原因。
  （`gear import` 只吃 `gear export` 的产物，**不接受单份 YAML**，别把 App 清单喂给它。）
- 没有 App 行的应用**能跑**（`flow run` / `app start` 都收 Flow id），但它的 tools **没有户主**：
  会被算成「平台级」，任何 `--scope` 都能引用，命名空间守卫对它失效。

## 1. 两个概念，三张表

registry（默认 `~/.gear/registry.db`，一个 sqlite 库）有三张定义表：

| 表 | 装什么 | 谁写它 |
|---|---|---|
| `tools` | Tool 定义（能力原子） | `gear tool register` / apply / import |
| `flows` | Flow 定义（steps DAG） | `gear flow register` / apply / import |
| `apps` | **App 清单**（户主元数据） | **只有 `gear apply`（CLI）/ `gear_apply`（MCP）/ `gear import`（仅 export 产物）** |

```yaml
# 同一个应用，两种文档，两张表：
kind: Flow          # → flows 表。gear flow run myapp.up 直接能跑
id: myapp.up
steps:
  - {id: anchor, call: myapp__config, input: {}}
  - {id: serve, call: ext.serve, mode: service, input: {}}
---
kind: App           # → apps 表。本身不「跑」，是「这是什么」的声明
id: myapp
entry_flow: myapp.up
```

## 2. App 这一行有什么用

它不是摆设——registry 里有四处判据读它：

| # | 消费者 | 没有 App 行时 |
|---|---|---|
| 1 | **命名空间户主**：`--scope` 守卫与 `tool/flow/hook list` 分组靠 `App.Owns(id)`（前缀 `<id>__` / `<id>.`）判「这是谁家的定义」（`internal/engine/engine.go` `guardScope`、`internal/cli/scope_view.go`） | 你的 tools 无主 ⇒ 算「平台级」⇒ 任何 `--scope` 都能跑，守卫失效 |
| 2 | **起服端口回退**：`gear app start <flow>` 不传 `--port` 时，按 `entry_flow` 反查 App 清单的 `start.ports` 做探活（`internal/app/ports_lookup.go`；恰好一个 App 命中才用，歧义即放弃） | `ports_source: none`，探活退化成只看 pid，「进程在、端口没起」测不出来 |
| 3 | **导出/交付**：`gear export <app-id>` 按清单打包 App + 名下定义 + 静态资源，对面 `gear import` 恢复（`internal/artifact/artifact.go`） | export 只认入口 Flow id 并**临时合成**清单（不写库，warnings 说明） |
| 4 | **入口 Flow 豁免**：`--scope <app>` 时，入口 Flow（`app.EntryFlow`，如 `myapp.up`）不带前缀也放行（`engine.go` `guardFlowScope`） | 入口 Flow 也按前缀判，`--scope` 下可能跑不了自己的起服 Flow |

## 3. 怎么注册：三条路

### 路线 A：一整套 bundle 一次 apply（新建应用的标准路径）

写一个多文档 YAML（`kind: App` + Tools + Flow 都在里面），用 CLI `gear apply` 或 MCP `gear_apply` 落库。

- **一个 bundle 只能带一份 App 清单**——带两份报 `BAD_APP_MANIFEST`；多应用分多次 apply。
- `entry_flow` 可指向**同一 bundle 里的 Flow**，也可指向**已注册的 Flow**。
- 落库顺序 App → Tool → Flow → Hook（被依赖者先写），任一非法整包不写（原子）。
- 静态资源 **MCP 不搬**：`layout.files` 只声明 + 校验存在性，文件用你自己的文件工具落盘。

### 路线 B：tools/flows 已注册，只补 App 行

顺序无所谓——App 行后补完全合法（2026-10-21 给 mindstorm / federation / wms 补注册实测通过）。
写一份**只有 App** 的文档，用 `gear apply` 落库即可，`entry_flow` 指向已注册的那个 flow。

### 路线 C：从产物导入

`gear export` 打出的目录 / `.gearapp` → 另一台机器 `gear import`（连静态资源一起落地，
并重写清单里的 layout 指向实际位置）。传 `--apps-dir` 才落地静态资源，否则只落定义。

### 走不通的路

`gear tool register` / `gear flow register` 只收**纯 Tool/Flow 文档**（混合文档会被拒），
且按设计不产生 App 行。

骨架从命令生成：`gear scaffold app my-app -o my-app.yaml`（2026-10-21 新增）产出带字段
注释的 App 清单，`--entry-flow` 可覆盖默认 `<name>.up`；文件头注释写死了落库路径
（`gear apply` / `gear_apply`，不是 register，也不是 `gear import`），生成即过 `model.App.Validate()`。

## 4. 最小 App 文档

```yaml
kind: App
id: myapp                      # 必填。命名空间户主：所有 tool/flow id 必须 myapp__x 或 myapp.x
name: 我的应用
description: 一句话说明这是什么
entry_flow: myapp.up           # 必填。入口 Flow：同 bundle 或已注册，否则 UNKNOWN_CALL_TARGET
layout:                        # 可选。静态资源声明（apply 只校验存在性，不搬运）
  workdir: apps/myapp
  files:
    - {path: apps/myapp/www/index.html, required: true}
start:                         # 可选。app start 不传 --port 时的回退 + 重启策略
  restart: always              # always | never
  ports: [8080]
requires:                      # 可选。依赖声明
  secrets: [MY_KEY]            # 需要人在 CLI 播种的密钥名（MCP 不写密钥）
  sidecars: [serve]            # 依赖的侧车（build/serve-sidecar.exe）
  capabilities: [ext.serve]    # 依赖的扩展能力
mcp:                           # 可选。MCP 策展区：--expose tools 时只暴露这份白名单
  tools:
    - ref: myapp__plan_propose
    - ref: myapp__plan_preview
      alias: myapp_plan_preview
      args: {session: parent}  # 服务端固定注入；从对外 schema 删掉，AI 改不了
      description: "只预览，禁止自动确认。"
```

## 5. 规则与坑

1. **id 就是前缀，先定 id 再注册 tools**。tools 叫 `myapp__x`，App 的 id 就必须是 `myapp`——
   对不上时 tools 重新注册（id 不可改），或把 App 的 id 改成与前缀一致。
2. **`app start` 收 Flow id，`app stop` / `app status` 收 app id**。台账按 app 记；
   start 时靠 `entry_flow` 反查清单拿端口，**多个 App 共用同一 entry_flow 时歧义即放弃**，请显式传 `--port`。
3. **apply 只登记、不运行**。写完 App 行，跑还是 `gear app start <flow>` / `gear flow run <flow>`。
4. **覆盖语义**：`gear_apply` 默认拒绝「已存在且内容不同」的覆盖（整批拒绝，原子）；
   确需覆盖显式传 `overwrite=true`。
5. **删 App 用 `gear_delete(kind=app)`**（注销清单，不做引用守卫）；私有 Tool 不会自动清，孤儿只能靠人发现。
6. **`--scope` 打错名字不再静默**（2026-10-21 起）：scope 指向一个未注册的 App 时，
   stderr 会显式告警并点名字 + 说清后果——守卫将把非平台级对象（包括你本想保护的
   本应用定义）判为「别人的应用」误拦，仅无主 id 照常放行。只告警不拒绝：
   「先用起来、App 清单后补」是合法用法。运行期与 `list` 目录面共用同一份判定。
7. **`mcp.tools` 是白名单，不是自动发现**：`ref` 须属于本 App 且 Tool 已注册；`args` 的键须出现在
   该 Tool 的 `input_schema.properties` 里。没写策展区时 `gear mcp --expose tools --scope myapp`
   **直接拒绝启动**（不退回裸暴露全量 Tool）。见 README §12 与 `gear_guide("authoring")`。
   若这个端点只给业务调用方用，建议再加 `--only-exposed`：那样连 15 个常驻 `gear_*` 管理工具
   都不挂（须配 `--expose`），instructions 也会换成应用专用说明；排障需另连标准 `gear mcp` 实例。

## 6. 验证注册成功

```bash
gear tool list --scope myapp       # 你的 tools 出现在「myapp 名下」组，不再是平台级
gear app start myapp.up --wait 10  # 不传 --port 也能探活（ports_source: bundle）
gear export myapp                  # 能打出含 App 清单的完整包
```

## 7. FAQ

**Q：我写了一个 flow 导入进去，现在他还是不是 app？**
不是。你只有 `flows` 表的一行。gear 眼里它是一个「可运行的 Flow」，不是一个「应用」——
没有户主、没有端口声明、打不了包。补一份 `kind: App` 文档 apply 即可（路线 B）。

**Q：没有 App 行，应用能跑吗？**
能。`flow run` / `app start` 都收 Flow id，长驻监管与 App 行无关。受影响的是上面第 2 节的四处判据。

**Q：先注册 App 还是先注册 tools？**
都行。同 bundle apply 时顺序由 apply 层保证（App 先写）；分步注册时 tools 先行、App 后补也合法
（`entry_flow` 可指向已注册的 Flow）。但 **id 前缀要先想好**——它是 App.id 的来源。

**Q：`gear app list` 是空的，是不是说明没有应用？**
两回事。`gear app list` 看的是**进程台账**（`~/.gear/apps/`，`app start` 起的监管进程），
`--scope` 和本手册说的 App 行在 **registry 的 `apps` 表**。前者空 = 没有在跑的应用；
后者决定「定义归谁」。
