# examples：三个可直接改的骨架

## 1. 最小一次性编排（不建 Tool，直调能力）

```yaml
kind: Flow
id: demo.hello
name: 最小一次性编排
description: 写一个键再读回来（演示直调能力与隐式依赖）
vars:
  who: world
steps:
  - id: put
    call: kv.put            # 直调：能力.方法（先 gear_capabilities 确认参数）
    input:
      key: greeting
      value: "hello ${vars.who}"
  - id: get
    call: kv.get
    depends_on: [put]       # 也可省略：这里没有 ${steps.*} 引用，故显式声明
    input:
      key: greeting
```

运行：`gear_run(kind=flow, id="demo.hello")` → 看 `gear_execution` 的 `result.steps.get.output`。
**注意**：这只是"编排"，不是"应用"——它跑完就结束。

## 2. 带 Tool 的两步串联（可复用逻辑才建 Tool）

```yaml
kind: Tool
id: demo__greet
name: 问候
description: 生成本地化问候语
runtime: lua
entry: |
  return { greeting = "hi " .. ctx.input.name }
input_schema:
  type: object
  properties:
    name: {type: string}
  required: [name]
output_schema:
  type: object
versions:
  - version: "1.0.0"
    state: stable
---
kind: Flow
id: demo.greet
name: 两步串联
description: 第二步引用第一步的输出（演示跨 step 数据流）
vars:
  who: gear
steps:
  - id: s1
    call: demo__greet
    input:
      name: "${vars.who}"
  - id: s2
    call: demo__greet
    depends_on: [s1]
    input:
      name: "${steps.s1.output.greeting}-reply"
```

## 3. 静态站应用（service + anchor，**这是真正的「应用」**）

前置：`static_site.serve` 这个 Tool 需已注册（见仓库 `plugin/static_site_demo.yaml`）；侧车需已构建
（`build/serve-sidecar.exe`，先跑 `bash scripts/build.sh`；不确定就 `gear_doctor`）。

```yaml
kind: App
id: mysite
name: 我的静态站
description: 用 ext.serve 托管一个静态目录
entry_flow: mysite.up
layout:
  workdir: apps/mysite
  files:
    - {path: apps/mysite/www/index.html, required: true}   # 你自己用文件工具落盘
start:
  restart: always
  ports: [8080]              # 声明端口 → 台账探活 + stop 后复检
requires:
  sidecars: [serve]
  capabilities: [ext.serve]
---
kind: Tool
id: mysite__config
name: mysite 配置锚点
description: 排序锚点（service step 不能作为依赖目标，用它把 site 推到下一层）
runtime: lua
entry: |
  return {}
input_schema: {type: object}
output_schema: {type: object}
versions:
  - version: "1.0.0"
    state: stable
---
kind: Flow
id: mysite.up
name: mysite 起服
description: 持有 serve 侧车，并把 www 目录托管到 8080
steps:
  - id: anchor
    call: mysite__config
    input: {}
  - id: serve                # 长驻：持有 ext.serve 侧车，使其不被回收
    call: ext.serve
    mode: service
    input: {}
  - id: site                 # 一次：依赖 anchor → 下一层 → 侧车必已就绪
    call: static_site.serve
    depends_on: [anchor]
    input: {port: 8080, dir: apps/mysite/www, host: 127.0.0.1}
```

四步走完的完整流程：

1. 用自己的文件工具写 `apps/mysite/www/index.html`；
2. `gear_validate(bundle)` → `valid: true`；
3. `gear_apply(bundle)` → `applied: {tools:[mysite__config], flows:[mysite.up]}`；
4. `gear_app(action=start, flow_id="mysite.up", wait_seconds=5)` → `state: running`，`ports:[{port:8080,listening:true}]`；
5. 汇报时**必须告知如何停止**：`gear_app(action=stop, app_id="…")`。

## 4. 常用改法

| 想做什么 | 改哪里 |
|---|---|
| 换端口 | Flow 里 `site` 的 `input.port` **与** `App.start.ports` 两处一起改 |
| 绑定到所有网卡 | `input.host` 从 `127.0.0.1` 改为 `0.0.0.0`（注意安全边界，默认只绑本机） |
| 加反向代理 | `site` 的 `input.proxy: [{path: /api, target: http://127.0.0.1:8787}]` |
| 让应用崩溃后不重启 | `App.start.restart: never` |
| 加定时/抓取步骤 | 在 Flow 里加 once step，`depends_on: [anchor]`，call 用 `http.Request` 或自建 Tool |
