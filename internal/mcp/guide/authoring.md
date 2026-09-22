# authoring：写 Tool / Flow / Hook 的全字段语义

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
```

- **runtime 语义**：`lua` 走内嵌沙箱（`entry` 是脚本本体，用 `ctx.input` 读入参、`return {...}` 出参）；
  `process` 走外部命令（stdin 进 JSON、stdout 末行出 JSON）；`go-host` 走宿主注册的实现。
- **capabilities 门禁**：用了 `ext.*` 或 7 个受门禁原语就**必须**声明；`crypto`/`time`/`template` 声明了反而被拒。
- **`versions` 是必需的**——因为 Flow 的 `use_version` 只能钉已注册 Tool 的版本；原子能力直调没有版本概念。

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
```

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

## 4. 正反例

**✅ 一次性编排：直调能力，不建 Tool**

```yaml
id: myapp.cacheit
steps:
  - id: put
    call: kv.put                 # 直调：能力.方法（先 gear_capabilities 确认方法名与参数）
    input: {key: "greeting", value: "hi"}
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
