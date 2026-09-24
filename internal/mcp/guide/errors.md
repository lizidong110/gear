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
| `ENTRY_EXE_UNRESOLVED` | process 型 Tool 的 entry 首 token 指向的可执行文件**不存在** | 改路径（Windows 下正斜杠、反斜杠都可，反斜杠不会被当转义符）。只有绝对路径才报此码；相对路径与裸名字只给告警——运行时 cwd / PATH 可能与校验时不同 |
| `BAD_HOOK_STRUCTURE` | Hook 缺字段 / stage 非法 | stage ∈ before_flow/before_step/after_step/on_error/on_success/before_promote |
| `SCHEMA_NOT_OBJECT` | `input_schema`/`output_schema` 顶层非 object | 写标准 JSON Schema，不支持 `{"x":"string"}` 简写 |
| `BAD_PRIMITIVE_ARGS` | 直调入参有未知参数名或缺必填 | 以 `gear_capabilities` 里该能力的方法 schema 为准 |
| `ID_NAMESPACE_VIOLATION` | bundle 里的 id 未带应用前缀 | 全部 id 以 `<app-id>__` 或 `<app-id>.` 开头 |
| `MISSING_FILE` | `layout.files` 声明了但文件不存在 | 用你自己的文件工具先落盘 |
| `MISSING_SECRET` | `${secrets.X}` 对应密钥未播种 | 请人在 CLI 播种（MCP 不写密钥） |
| `SIDECAR_NOT_BUILT` | 声明的侧车二进制缺失 | 先构建（`build/*-sidecar.exe`），`gear_doctor` 可见 |
| `PORT_IN_USE` | 端口被占 | 换端口，或先停占用者（`gear_doctor` 看得到） |
| `IN_USE` | 删除被引用的对象 | 错误信息会指出被谁引用，先解引用 |

### 运行期

| code | 触发 | 修法 |
|---|---|---|
| `SERVICE_FLOW_SYNC_RUN` | 对含 `mode: service` 的 Flow 用 `gear_run` | 改用 `gear_app(action=start)` |
| `APP_START_FAILED` | 起应用后短等窗口内不通 | 读返回的 `log_path` 末几行；常见：端口被占、侧车没 build、registry 路径不符 |
| `APP_NOT_FOUND` | `stop`/`status` 的 `app_id` 不在台账 | 先 `gear_app(action=list)` |
| `ORPHAN_APP` | `stop` 后端口仍未释放，或 pid 与端口判据不一致 | 如实上报 pid；`gear_doctor` 看谁占着；**不要把它当成功** |
| `APP_PLATFORM_UNAVAILABLE` | `--apps` 转发模式下 REST 不可达 | 先起 `gear serve`，或去掉 `--apps` 用本地派生 |
| `APP_PRUNE_FAILED` | `gear_app(action=prune)` 清理台账失败（读目录 / 删文件） | 看返回的 `apps_dir` 是否可写；prune **只删已终止的台账记录**，不杀进程 |
| `EXECUTION_BUSY` | MCP 面在途执行已达并发上限（默认 8），`gear_run` / `flow__` 被背压拒绝 | 稍后重试，或先用 `gear_execution` 看现有执行进展。**这不是 YAML 问题**，别去改定义 |
| `PRIMITIVE_MISSING` | 所需能力在目录与 extensions.yaml 中都不存在 | **不许绕过**；按 `gear_guide("primitive-gap")` 给出原语建议 |
| `UNCLASSIFIED` | 未能归类（只能靠读 message） | 按 message 的内容人工判断；这通常说明该错误路径还没做结构化 |

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

## 一条纪律

**任何错误都不允许「绕过」**：不能因为 `UNKNOWN_CALL_TARGET` 就用 shell 外调，不能因为
`MISSING_SECRET` 就把明文写进 YAML。宁可如实报阻塞，也不伪造一个能跑的假象。
