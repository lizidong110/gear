# GEAR 工程方法 v1 · PRD → 可信代码的宪法

> **定位**：本文件是后续每个切片（S0–S6）实现代码的**交付宪法**。任何切片在"可被测、可被追溯、过质量闸门、经评审"之前，视为未完成。
> **触发原因**：上一轮直接生成代码导致 6 处 PRD 漂移/缺失（见 §9），证明"没有需求→代码→测试强制绑定，代码会按模型直觉而非 PRD 真意漂移"。本文件即用来堵这个洞。
> **使用方式**：每切片先给「测试清单 + DoD」→ 实现 → 覆盖率报告 → **用户评审过关**才进下一刀。未签批本文件前，不写任何实现代码。

---

## 0. 核心原则（不可绕过的红线）

1. **需求可追溯**：每个 PRD 条目（§x.y）至少对应一个测试或不变量；未覆盖 = 未完成（见 §3 矩阵）。
2. **红—绿节奏**：对纯逻辑（状态机、解析、调度、依赖图）先写失败测试，再实现；不靠"我觉得对"。
3. **质量闸门不可跳过**：`go build` + `go vet` + staticcheck + 覆盖率门槛全进 CI；CI 红 = 不可合并（见 §5）。
4. **待确认项显式处置**：PRD §13 的 14 条，每条必须「定 / 搁置」并写明归属切片与理由，禁止静默默认（见 §7）。
5. **切片交付、评审过关**：S0–S6 每刀独立可验证，过关才继续；绝不"一把梭端出整套"。
6. **单一真相源**：PRD 定义的 Schema / 状态机 / 触发面，引擎校验、AI 上下文、前端表单共用同一份定义，不允许漂移（对应 PRD §9.3）。
7. **控制流三态不可混淆**：一个 step 的结局必须能区分 **执行成功 / 失败 / 被跳过（skip）**。跳过**不是错误**——它必须用与失败不同的类型/通道表达（`ErrStepSkipped` + `RunResult.Skipped`），否则下游只能靠猜（见 §11 F6）。
8. **表达式与模板分离**：数据替换用 `${}` 模板；**条件判断/比较必须用带类型的表达式引擎**，禁止「先把 `${}` 插值成字符串再 eval」（见 §11 F7）。
9. **引用解析必须与该语法的字面量规则一致**：凡是新引入的「引用」语法，都要同步进依赖推断；且解析要遵守该语法的字面量规则（表达式里没有 `${}`；字符串字面量内的 `steps.x` 不算引用）（见 §11 F8）。
10. **能静态校验的，不许留到运行期**：字段枚举、语法、表达式编译等，一律在**注册期**暴露（F3 的自然延伸）。
11. **新状态必须可观测**：引入新的执行状态（如 skip）时，必须同时给出结构化字段 + 断言测试；不可见的状态既测不了、也排不了障。
12. **方法论自身可追溯**：本文件每次改动都记入 §11 变更日志，写明「触发场景 / 改了什么 / 为什么」。否则方法论会像 PRD 一样悄悄漂移。
13. **时序断言禁用绝对阈值**：只在「负载只会让它更慢」的**安全方向**用绝对下界（如「耗时应 ≥ 800ms」）；任何**上界**判断（如「必须 < 900ms」）一律改为**同一进程内的相对比较**（先量串行基线再比并行）。理由：`go test ./...` 会并行跑包，机器负载会让绝对时长抖动，制造偶发失败——而被当作「偶发」忽略的 flake，最终会侵蚀对整套测试的信任（见 §11·2026-09-13 S6a）。
14. **状态的持久性必须显式声明**：引入任何新状态（执行记录、进程句柄、缓存）时，必须在代码与文档里写明它是**内存态**还是**持久化**、以及丢失条件。理由是「静默假设」的新变种——不是字段被忽略，而是**生命周期被默认**（见 §11·2026-09-13 S6a）。
15. **枚举式声明必须逐项登记**：任何「合法取值集合」（模板前缀 vars/steps、runtime 名、hook stage、criticality、step mode 等）必须（a）在单一来源登记全部合法值，（b）在解析/校验点拒绝未知值，（c）在 PRD/矩阵同步状态；三者缺一则构成漂移隐患（见 §11·2026-09-13 第三次）。理由是本次真实漂移——PRD §4.3 把 `${secrets.x}` 列为「支持的前缀」，而引擎只实现 vars/steps 且从未登记 secrets，于是「文档说有、代码没有」长期无人察觉，直到补上注册期模板静态校验才暴露。

---

## 1. 工程分层与包结构（对应 PRD §10）

```
gear/
  cmd/gear/main.go            # 入口，仅调 cli.Execute()
  internal/
    model/                    # 数据模型（唯一真相源）：Tool/Flow/Step/Hook/Version
    registry/                 # 存储层：SQLite，JSON blob；实现 Registry 接口
    engine/                   # 编排层：DSL 解析 / Schema 校验 / 调度 / 变量解析 / Hook 触发
    runtime/                  # Runtime 适配层（实现 Runtime 接口）
      gohost/                #   进程内 Go 函数
      lua/                   #   嵌入式 Lua 沙箱
      process/               #   外部进程 stdin JSON → stdout 末行 JSON
    host/                     # ctx.* 宿主能力 6 原语（实现 Host 接口）
    server/                   # REST API（PRD §11），CLI 的同源服务端
    cli/                      # CLI 管理面（PRD §14.6），server 的薄客户端
  examples/                   # 示例 Tool/Flow YAML（须可被 E2E 直接消费）
```

**结构约束（解决"结构是否合理"）**
- 依赖方向单向：`cli/server → engine → {runtime, host, registry} → model`。上层可依赖下层，禁止反向。
- **接口化以便测试**：`Registry`、`Runtime`、`Host` 均为接口；测试用 fake/in-memory 实现替换，真实 SQLite / 进程仅在集成测试启用。
- 引擎（engine）不持有 IO 副作用细节：所有外部交互经 `Runtime` / `Host` 接口，使纯逻辑可单测、可复现。
- 包名即职责边界，禁止在 `model` 写校验逻辑（`model` 只建模）、在 `registry` 写调度逻辑。

---

## 2. 测试金字塔与策略（解决"测试覆盖率"）

| 层级 | 范围 | 工具/方式 | 门槛 |
|---|---|---|---|
| **单测（Unit）** | 纯函数：Schema 校验、版本状态机、依赖图环检测、SemVer 解析、变量解析、Hook weight 排序、fail-closed 判定 | `testing` + 表驱动（table-driven） | 100% 行覆盖目标于纯逻辑包 |
| **集成（Integration）** | Registry 落库往返、Runtime 适配（process 启 `cat` 喂 JSON 取末行；lua 沙箱；go-host 注册）、Host 6 原语、Hook 改写+重校验 | 真实组件 + 临时 SQLite（`t.TempDir`） | 关键路径全通 |
| **场景/E2E** | 整 Flow DSL 跑通（顺序/并行/条件）、Hook 拦截改写、版本晋升 API、app run 生命周期 | YAML 示例直接喂引擎，断言最终输出/状态 | 每个 PRD 场景 ≥1 用例 |
| **契约（Contract）** | CLI/REST 端点签名与 PRD §11 逐条对齐；Schema 同源性 | 端点 ↔ §11 表自动比对 | 零偏差 |
| **属性/Fuzz（进阶）** | 模板解析、YAML 鲁棒性、变量路径合法性 | `testing.F` fuzz | 关键解析器覆盖 |

**约定**
- 所有校验报错用 **golden 文件**（`testdata/*.golden`）比对，保证错误信息稳定、可评审。
- 测试命名：`Test<包>_<场景>_<预期>`，例 `TestEngine_WhenFalse_SkipStep`。
- 不可达分支（如 `default` 防御）用显式注释 + `t.Error("unreachable")` 兜底，避免"假覆盖"。

---

## 3. 需求 → 代码 可追溯矩阵（解决"情景覆盖是否到位"）

> 状态列：🔴 缺失（当前 S0 无）/ 🟡 部分（骨架有但未校验）/ 🟢 已覆盖（需测试支撑）。本矩阵是 PRD 的"测试账本"，每切片更新。

| PRD 条目 | 需求摘要 | 归属包 | 测试类型 | 测试 ID 示例 | 状态 |
|---|---|---|---|---|---|
| §2 | 5 原语模型正确 | model | 单测 | T-model-01 | 🟢 |
| §3.1 | Tool 定义/字段 | model | 单测 | T-model-02 | 🟢 |
| §3.2 | Runtime 类型枚举 | model | 单测 | T-model-03 | 🟢 |
| §3.3 | 参数统一 JSON 协议 | runtime/* | 集成 | T-rt-01..03 | 🔴 |
| §4.1 | input/output 类型/必填/默认校验 | engine | 单测+集成 | T-eng-01 | 🔴 |
| §4.2 | 版本层/参数层分离（Flow 默认 stable） | engine | 单测 | T-eng-02 | 🔴 |
| §4.3 | depends_on / when / 变量引用 `${vars.x}` 等 | engine | 单测 | T-eng-03..05 | 🟢 |
| §4.4 | 触发方式收敛为执行入口 | server | 契约 | T-srv-01 | 🔴 |
| §5.1 | Hook 四要素模型 | model | 单测 | T-model-04 | 🟢 |
| §5.2 | 生命周期 + weight 升序（同重注册序） | engine | 单测 | T-eng-06 | 🔴 |
| §5.3 | 插件无感知 / action 是 Tool / fail-closed / 禁止隐式依赖 | engine | 集成 | T-eng-07 | 🔴 |
| §5.4 | 改写 input/output + 重校验；best-effort；on_error 不恢复；Hook 嵌套防重入 | engine | 集成 | T-eng-08..11 | 🔴 |
| §6 | 宿主 6 原语 http/call/db/kv/secret/event | host | 集成 | T-host-01..06 | 🔴 |
| §6.2 | call 与 event 边界 | host | 集成 | T-host-07 | 🔴 |
| §7.1 | 状态机 draft→testing→stable→deprecated/rejected | engine/registry | 单测 | T-ver-01 | 🔴 |
| §7.2 | 晋升显式 + operator 审计 | server/registry | 集成 | T-ver-02 | 🔴 |
| §7.3 | 幂等 + 并发安全（写锁/CAS/版本不可变） | registry | 并发测试 | T-ver-03 | 🔴 |
| §7.4 | 灰度选择（use_version:testing / allow_testing） | engine | 单测 | T-ver-04 | 🔴 |
| §8.1 | 依赖图三类（Flow→Tool 不传递） | registry | 单测 | T-reg-01 | 🔴 |
| §8.2 | 注销拒绝 3 条件（被 Flow/Hook 引用/运行中） | registry | 集成 | T-reg-02 | 🟢 三条件全实现（运行中由 RunningChecker 判定；见 REQ-DEL-TOOL-GUARD / REQ-DEL-TOOL-RUNNING） |
| §8.3 | Lock + digest（signature 暂 deferred） | registry | 集成 | T-reg-03 | 🔴 |
| §9 | AI 生成 DSL + 静态校验 + ≤3 次重试 + Schema 不漂移 | server | 集成 | T-ai-01 | 🔴 |
| §10 | 架构分层 | （结构审查） | 评审 | — | 🟡 |
| §11 | API 最小集全量 | server | 契约 | T-srv-02 | 🟡 15 端点中 12 个已实现（healthz/tools CRUD/flows CRUD+validate/hooks/executions/apps）；deferred：promote(S5)、ai-infer/ai-gen(S6c)、executions/{id}/logs |
| §13 | 待确认项显式处置 | 全局 | 评审 | — | 🟡 |
| §14 | app run 进程模型 + 长驻/restart/子进程归属/进程池 | server/engine | 集成 | T-app-01..04 | 🔴 |

---

## 4. 切片路线图与每切片 DoD（解决"分阶段、可评审"）

> 每切片 DoD = 该切片矩阵条目全 🟢 + 覆盖率达标 + 评审过关。
>
> **⚠️ 2026-09-13 更正**：下面原定顺序在实施中已被调整，实际执行如下（本表随切片回填，避免方法论漂移）：
> S0 骨架 ✅ / S1 最小引擎 ✅ / **S2 app run 进程监管 ✅** / **S3 Hook 切面 ✅** / **S4 并行+依赖推断+背压 ✅** / **S7a lua runtime ✅（提前）** / **`when` 条件执行 ✅（本次）**；S5 版本灰度、S6 REST+AI 仍待做；`ctx.*` 宿主能力（M5）仍为架构债。
> 调整理由：lua 提前是因为 `ctx.*` 尚未落地时 lua 可作为**纯计算沙箱**独立成立；Hook 排在并行之前是因为 Hook 需要先抽「共享 step 执行路径」，而并行也要复用同一条路径——先建路径再改它的并发语义，风险更低。

- **S0 基线**：model/registry/cli 骨架。DoD：修正 §9 的 6 处缺陷 + model 单测 + registry 集成测试 + CLI 与 §11 对齐清单。✅ **已完成 2026-09-12**。
- **S1 引擎**：DSL 解析、Schema 校验、顺序调度、变量解析、input/output 校验。✅ **已完成**。
- **S2 app run 进程模型**：长驻 Flow（`mode: service`）、restart、信号停止、`app run --max` 闸门。✅ **已完成**（健康探针仍 deferred）。
- **S3 Hook 切面**：before_step/after_step/on_error + Order 装配 + 改写重校验 + fail-closed + criticality + 无递归。✅ **已完成**（before_promote 待 S5）。
- **S4 并行与背压**：分层并行、依赖推断、`maxParallel`、并行安全、并行下 fail-closed、崩溃指数退避。✅ **已完成**。
- **S7a lua runtime**：gopher-lua 沙箱（once）+ 沙箱/超时/错误测试。✅ **已完成**；**沙箱策略可配置化（REQ-LUA-SANDBOX-CONFIG）已记为后期项**。
- **`when` 条件执行**：expr 表达式引擎 + 三态跳过 + 级联传播 + 注册期编译校验。✅ **已完成**（2026-09-13）。
- **S5 版本灰度**：状态机 + promote + 并发安全 + 灰度 + SemVer + lock/digest。覆盖 T-ver-01..04、T-reg-01..03。
- **S6 REST API + CLI 补全 + AI 闭环**：§11 全量端点 + CLI 补全 + AI 生成/校验/重试。覆盖 T-srv-01..02、T-ai-01。
  - **S6a REST 执行面 ✅（2026-09-13）**：`internal/server`（标准库路由，无框架）+ `ExecStore`（内存态执行记录）+ `AppStore`（受管进程）+ 12 个端点 + 契约测试；CLI `gear serve`。
  - **S6b CLI 测试补全**（部分完成，2026-09-13）：现有 CLI 命令（tool/flow/hook 的 register·list·show·run·remove、app run）已补单测，`cli` 包覆盖 **75.2%**（成功 + 错误路径全覆盖）。新增子命令 `tool unregister` / `flow validate·inspect` / `exec status` / `tool promote` 仍待做（需先对齐 REST 已有能力）。
  - **S6c AI 闭环**（待做，**需先决策 LLM 接入方式**）：生成 → `ValidateFlowStatic` 校验 → ≤3 次重试 → 人工确认。

---

## 5. 质量闸门（CI，解决"怎么保证没 bug"）

| 闸门 | 命令 / 工具 | 失败处置 |
|---|---|---|
| 编译 | `go build ./...` | 阻断合并 |
| 静态 | `go vet ./...` + `staticcheck ./...` | 阻断合并 |
| 格式化 | `gofmt -l` 零差异 | 阻断合并 |
| **覆盖率** | `go test -coverprofile`；**引擎/内核包 ≥80%，model/registry 关键路径 100%** | 低于门槛阻断合并；PR 须附报告 |
| 并发/竞态 | `go test -race` 于 registry/engine | 竞态即阻断 |
| 契约 | §11 端点 ↔ 矩阵自动比对 | 偏差阻断 |
| 评审 | 每切片覆盖率报告 + 矩阵更新，用户签批 | 未签批不进下一刀 |

> 覆盖率门槛默认可调；如你认为某包应更高/更低，在签批时改。

---

## 6. 评审节奏（节奏规则）

```
切片启动 → 给「测试清单 + DoD」给用户审
        → 红绿实现 → 本地跑通 -race + 覆盖率
        → 提交覆盖率报告 + 矩阵状态更新
        → 【用户评审过关】→ 下一刀
        → 不过关 → 回到实现，不累积
```

禁止：跨切片累积未测代码、静默默认待确认项、用"手动 smoke"替代断言测试。

---

## 7. PRD §13 待确认项 处置（显式定/搁置）

| # | 问题 | 本次处置 | 归属切片 | 理由 |
|---|---|---|---|---|
| 1 | 同 tool_id 多主版本共存 | **搁置**（默认禁止） | — | PRD 已定默认禁止，side-by-side 显式允许留后期 |
| 2 | 0.x 版本精确 pin | **定**：MVP 强制精确 pin | S5 | 保证可复现，不许 latest |
| 3 | 系统包管理 | **定**：只声明前提并验证，不自动安装 | S3 | 安全责任边界 |
| 4 | 灰度粒度 | **定**：调用方级显式选择 | S5 | PRD §7.4 已定 |
| 5 | 晋升审批前置 | **定**：默认调用即晋升；before_promote 可扩展 | S5/S4 | PRD §7.2/5.2 已定 |
| 6 | 测试版审计 | **定**：同表记录仅标记 stage | S5 | PRD §13-6 已定 |
| 7 | CAS 严格预期版本 | **定**：宽松策略（§7.3） | S5 | PRD 已定 |
| 8 | Web UI 首批 | **定**：否，UI 为可选 Flow | — | PRD §1.4 已定 |
| 9 | 长驻/阻塞 step 形态 | **定**：`daemon: true` 标记 | S2 | 采用 server Tool 思路 |
| 10 | app run restart/健康检查 | **定**：`--restart=always` + 可选健康检查 | S2 | PRD §14.3 |
| 11 | 执行入口并发上限 | **定**：内核进程池 + 上限（默认 64，可调） | S2 | PRD §14.5 |
| 12 | CLI 鉴权到 daemon | **搁置**（deferred） | — | 安全类，与 §13-14 同批 |
| 13 | Hook 默认 criticality | **定**：认证/脱敏=**critical**，日志类=**best-effort** | S4 | PRD §5.4 建议 |
| 14 | process/go-host 绕过 capability、secret 经 argv | **搁置**（deferred） | — | 用户明确暂不想，P0-4 |

> 任何"定"的项若实现中发现 PRD 自相矛盾，回到本文件 §7 修订并通知用户，不私自改。

---

## 8. 命名与语法一致性（从 S0 缺陷吸取）

- 变量引用统一 **`${vars.x}` / `${step.output.x}` / `${secrets.x}` / `${input.x}`**（PRD §4.3）；示例 YAML 须同步，禁止 `{{...}}`。
- 版本号强制 **SemVer**（`1.0.0`），禁止 `v1` 这类非标；`model.ToolVersion.Version` 用 `semver.Version` 类型并在 register 校验。
- Step 字段名对齐 PRD：`call`（非 `tool`）、`depends_on`、`when`、`use_version`。
- Hook `stage` 取值对齐 §5.2：`before_flow/before_step/after_step/on_error/on_success/before_promote`。

---

## 9. 当前 S0 已发现的 6 处缺陷（重做基线时修复）

| # | 缺陷 | 证据 | 修复动作 | S0 状态 | 实际测试 ID |
|---|---|---|---|---|---|
| D1 | 模板语法分叉 | `examples/hello_flow.yaml` 用 `{{vars.topic}}`，PRD 规定 `${vars.x}` | 示例改 `${vars.topic}`；新增回归测试锁死 | ✅ 已修复 | `TestExampleFlowUsesDollarBrace`（T-s0-example） |
| D2 | 版本 SemVer 未校验 | `hello_tool.yaml` 写 `version: v1`；`Version` 为裸 string 无校验 | 新增 `model.ParseSemVer` + `ToolVersion.Validate`（零依赖正则）+ `UpsertTool` 接入校验；示例改 `1.0.0` | ✅ 已修复 | `TestParseSemVer`、`TestToolVersionStateValid`、`TestToolValidate`（T-ver-parse / T-state-valid） |
| D3 | 零测试 | 全工程无 `_test.go` | 补 `internal/model/model_test.go` + `internal/registry/registry_test.go` | ✅ 已修复 | 见 §9.1 |
| D4 | CLI 与 §11 不对齐 | 缺 unregister/promote/validate/ai-gen/hook CRUD/app ls-stop/exec/secret | 仅列缺口清单（见 §9.2）；完整补全归 S6 | ⏭ 归 S6 | — |
| D5 | Schema 校验缺失 | register 仅 `yaml.Unmarshal`+upsert，不校验 schema/state/依赖 | `UpsertTool` 接入 `Tool.Validate`：version 合法 SemVer + state 枚举 + input/output schema 存在且 `type:object`（完整 JSON Schema 深校验留 S1） | 🟡 部分（最小真实校验已做） | `TestToolValidate`、`TestRegisterRejectsInvalid`（T-reg-02） |
| D6 | 并发/事务空白 | `sql.Open` 无事务；§7.3 写锁/CAS 未体现 | `Registry` 加 `sync.Mutex` 串行化写 + upsert 包事务（`BEGIN/COMMIT`）；`-race` 并发用例 | 🟡 部分（写锁+事务已做；CAS/版本不可变留 S5） | `TestConcurrentUpsert`（T-reg-03） |

### 9.1 S0 覆盖率报告（2026-09-12，`CGO_ENABLED=1 go test -race -cover ./...`）

| 包 | 覆盖率 | 目标 | 结论 |
|---|---|---|---|
| `internal/model` | **100.0%** | 关键路径 100% | ✅ 达标 |
| `internal/registry` | **82.8%** | ≥80% | ✅ 达标 |
| `internal/cli` / `cmd/gear` | cli **75.2%**（S6b）/ cmd 0.0% | cli 管理面命令全覆盖；cmd 仅 `main()` 入口 | ✅ cli 达标（S6b）；cmd ⏭ 收尾 |

- `go build ./...` ✅ · `go vet ./...` ✅ · `go test -race` ✅（无竞态）
- registry 未覆盖的 ~17% 全部是 DB 错误防御分支（`Open`/`migrate`/`upsert`/`list`/`get` 的错误返回），属难触发且非核心路径，留待注入式故障测试（进阶）。
- model 100%：含 SemVer 解析、状态枚举、Tool/Hook/YAML 往返、Validate 全分支。

### 9.2 S0 已交付测试清单（对应 §6 节奏）

**model（单测，100%）**
- `T-model-01` YAML↔struct 往返（Tool/Flow/Hook/Step）
- `T-model-02` Runtime 枚举取值
- `T-model-03` ToolVersion 字段
- `T-model-04` Hook 四要素 + criticality
- `T-ver-parse` SemVer 合法/非法
- `T-state-valid` 5 状态枚举
- `T-model-05` Tool.Validate 全分支（非法 version/state/缺 schema/schema 类型错/空 id/无 version）

**registry（集成，82.8%）**
- `T-reg-01` Tool 落库往返 + blob 完整
- `T-reg-02` register 拒绝非法 version/state/缺 schema
- `T-reg-03` 同 id 并发 upsert 不损坏（`-race`）
- `T-reg-04` Flow 落库往返
- `T-reg-05` Hook 落库往返
- `T-reg-06` 不存在 id 返回 (nil,nil)
- `T-s0-example` 示例 YAML 用 `${vars.topic}` 而非 `{{...}}`（锁死 D1 回归）


### 9.3 S6b 覆盖率报告（2026-09-13，`CGO_ENABLED=1 go test -race -cover ./...`）

| 包 | 覆盖率 | 目标 | 结论 |
|---|---|---|---|
| `internal/model` | **98.6%** | 关键路径 100% | ✅ 达标 |
| `internal/engine` | **87.7%** | ≥80% | ✅ 达标 |
| `internal/registry` | **83.4%** | ≥80% | ✅ 达标 |
| `internal/app` | **86.0%** | ≥80% | ✅ 达标 |
| `internal/server` | **83.9%** | ≥80% | ✅ 达标 |
| `internal/cli` | **75.2%** | 管理面命令全覆盖 | ✅ S6b 达标（本轮新增 cli_test.go） |
| `cmd/gear` | 0.0% | 仅入口 | ⏭ 收尾 |

- `go build ./...` ✅ · `go vet ./...` ✅ · `go test -race` ✅（无竞态）。
- cli 本轮从 0% → 75.2%：覆盖 tool/flow/hook 全生命周期命令与 app 非法 `--restart` 错误分支；serve 执行路径（阻塞 HTTP）与 flow run 成功路径（拉起进程）因需集成环境未覆盖，归 S6b 收尾。

---

## 11. 方法论变更日志（边做边改；每条都写清「为什么」）

> 本文件不是一次成型的蓝图，而是**随切片演进**的宪法。每次改动必须回答三件事：触发场景 / 改了什么 / 为什么必须改。
> 规则：新规则必须来自**真实踩到的坑**，不接受纸上推演出的规则（避免方法论自我膨胀）。

### 2026-09-13 · 实现 `when` 条件执行，新增 F6–F8 与 §0 第 10–12 条

**F6 · 控制流三态（skip / fail / empty）** → 对应 §0 第 7 条
- **规则**：step 结局必须能区分「执行成功 / 失败 / 被跳过」；`skip` 沿**有效依赖**（含推断边）级联传播；`skip` 不计入失败，也不得让下游读到 nil 或误报「尚未执行」。
- **为什么**：`when` 一引入，「这个 step 没跑」就成了第三种结局，而在此之前系统只有「成功 / 失败」二态，代码默认「没有输出 = 出错」。如果不把 skip 建模成一等状态，下游要么**静默拿到 nil 继续跑**（最危险，符合我们一直在消除的 RC-D 反模式），要么报出与真实原因无关的「step 尚未执行」（把排障引向错误方向）。
- **落地**：`ErrStepSkipped` 哨兵 + `RunResult.Skipped`（step → 原因）+ 依赖级联跳过 + 6 条断言测试。

**F7 · 表达式 ≠ 模板** → 对应 §0 第 8 条
- **规则**：`${}` 模板只做数据替换；条件/比较一律走带类型的表达式引擎；两种语法**不许混用**（`when` 里出现 `${` 直接拒绝并教正确写法）。
- **为什么**：插值再 eval 会**丢类型**。`steps.x.output.status` 实际是字符串 `"200"`，插值成字符串后 `!= 200` 这类比较会在「字符串世界」里恒真/恒假。这种 bug 的特征是「看起来很对」——测试如果只测 happy path 就永远发现不了。
- **落地**：引入 `expr-lang/expr`（`Knetic/govaluate` 在语料库代理上 404，排除）；`whenEnv` 用 expr tag 固定根名 `vars`/`steps`，写错根名在**编译期**就报错。专门的类型保持测试：字符串 `"200"` 与数字 `200` 比较必须为假。

**F8 · 引用解析与语法一致** → 对应 §0 第 9 条
- **规则**：每种引用语法都必须进依赖推断；解析必须遵守该语法的**字面量规则**（模板用 `\$\{steps\.`；表达式用裸 `steps\.`，且**先剔除字符串字面量**）。
- **为什么**：这条是本次**连续两次翻车**换来的，不是推演出来的：
  1. 只扫 `input` 不扫 `when` ⇒ when 里引用上游的 step 与上游**落在同一层并发执行**，求值时上游还没产出（报 `cannot fetch n from <nil>`）。→ **新语法引入的引用，必须同步进依赖推断**。
  2. 修 (1) 时直接复用了模板正则 ⇒ 匹配不到（表达式里没有 `${}`）；若简单换成裸正则，又会把 `when: 'name == "steps.nope"'` 里的**字符串字面量**当成依赖，制造假报错。→ **解析器必须理解该语法的字面量边界**。
- **落地**：`whenStepRefRe`（带前导断言排除 `mysteps.x`）+ `stripQuoted`（剔除 `"…"`/`'…'`/`` `…` ``）+ 3 条回归测试（when 产生依赖 / when 未知引用报错 / 字面量不误判）。

**§0 第 10 条（F3 延伸）· 静态校验前移到注册期**
- **为什么**：`when` 的语法错误若留到运行期，可能是在某个 Flow 恰好走到该分支时才炸；而编译一次表达式的成本几乎为零。与 runtime/mode/hook 的注册期校验保持同一标准，才不会出现「有的字段注册期管、有的运行期管」这种不一致。
- **落地**：`engine.CompileWhen` + `flow register` 前置校验（实测 `when: "1 +"` 注册即拒，并带 caret 定位）。

**§0 第 11 条（F1/F4 延伸）· 新状态必须可观测**
- **为什么**：若 `skip` 只打一行日志，测试就无法断言「它确实没跑」，排障时也看不到「为什么没跑」。可观测性是**可测试性的前提**——看不见的状态等于不存在。

**§0 第 12 条 · 方法论自身可追溯**
- **为什么**：本次顺手发现 §4 切片路线图已与真实执行顺序漂移（文档写 S3=runtime/ctx、S4=Hook；实际是 S3=Hook、S4=并行/背压、S7a=lua）。**方法论漂移就是方法论失效的开始**——它和 PRD 漂移是同一种病，得用同一味药：变更日志 + 定期回查（已修 §4）。

### 2026-09-13（第二次）· 做 S6a（REST 执行面），新增 §0 第 13–14 条与三条接口层规则

**S6a 的范围切分决定（本身也是方法论实践）**
- **触发**：S6 在路线图里写作「REST API + CLI 补全 + AI 闭环」，对应 PRD §11 的 15 个端点 + §9 的生成闭环。一次性做完会违反 §0 第 5 条（绝不一把梭）。
- **改法**：拆成 S6a（REST 执行面）/ S6b（CLI 补全）/ S6c（AI 闭环，且**需先定 LLM 接入方式**——属于决策而非编码量），本次只交付 S6a。
- **为什么**：切片的价值在于「每片独立可验证 + 可评审」。混在一起做，出问题时无法定位是哪一层的问题；而 AI 闭环还卡在一个未决策点上，把它捆进来只会拖慢可验证的部分。

**F9 · 接口层的「表示与实现」必须逐条对账**
- **规则**：PRD §11 这类端点表是**契约**，不是文档。实现时必须在需求矩阵里对**每一条**标注 `implemented` / `deferred`；未实现的不许沉默。
- **为什么**：接口层最容易漂移——端点表看起来像说明文档，实际是别人对接的依据。S6a 15 个端点里只做了 12 个（`promote` 属 S5、`ai-infer`/`ai-gen` 属 S6c、`executions/{id}/logs` 需日志 Hook），如果不在矩阵里显式标 deferred，就是「PRD 说有、代码没有」的老病复发。

**F10 · 异步任务的可见状态必须有明确归属**
- **规则**：引入异步（REST 触发执行 → 202 + id → 轮询）时，必须同时给出：状态枚举、状态存储、以及**存储的生命周期声明**。
- **为什么**：异步把「一次调用」拆成了「触发」和「查询」两个动作，中间那段状态如果没有明确的存放位置和生命期，就会出现「接口返回了 id，但没人能说清这个 id 活多久」。我们用内存 `ExecStore` 是有意选择（MVP），但**必须写出来**，否则将来会被误当作已持久化（→ §0 第 14 条）。

**F11 · 优雅停机 = 取消 + 等待收敛**
- **规则**：`Shutdown` 必须 `cancel()` **并等待** 受管子任务退出（`appWG.Wait()`），不能只发信号就走。
- **为什么**：这是本次真实观察到的——测试退出时日志出现 `! Flow svcf 监管结束: sql: database is closed`：registry 已关闭而 Supervisor 还在读库。停机竞态在生产上表现为「关停时偶发报错 / 丢数据」，在测试上表现为「偶发噪音」，两者都会钝化对异常的敏感度。

**§0 第 13 条 · 时序断言禁用绝对阈值**
- **触发**：全量 `go test ./...` 时 `TestParallelExecution` **偶发**失败——断言「3 个 0.4s step 并行应 < 900ms」，实测 901.6ms。单包跑必过，全量跑由于各包并行、机器负载升高而越界。
- **改法**：改为同一进程内先量**串行基线**再比并行，断言比值（`parallel*4 < serial*3`）。连跑 3 轮全量验证稳定。
- **为什么**：绝对耗时阈值的失败**与代码无关**，只与当时机器多忙有关。这类 flake 一旦被贴上「偶发」标签就会被忽略，随后真故障也被忽略——它污染的是**人们对红灯的信任**。规则要写成方向性的：**下界安全（负载只会更慢），上界必须相对化**。

**§0 第 14 条 · 状态持久性显式声明**
- **触发**：新增 `ExecStore`（执行记录）与 `AppStore`（受管进程句柄）都是进程内 map。
- **为什么**：这是「静默假设」的新形态——过去我们防的是「字段被静默忽略」，现在要防的是「**生命周期被默认**」。没人会在代码里写「我以为它持久化了」，但所有人都可能这么默认。解法成本极低：注释 + 文档里写一句「内存态，进程重启即失」。

**新增共享状态必须补并发用例**
- **为什么**：`ExecStore` 是新的并发共享状态（HTTP 天然并发）。`Get` 若返回结构指针，调用方会在锁外读到写一半的结构——故返回**副本**。已用 8 路并发触发的用例在 `-race` 下固化。

**覆盖率闸门的价值实证**
- 加完 `DeleteTool`（含守卫分支）后，**registry 覆盖率从 82.4% 掉到 66.4%**——因为它的分支只在 server 的 HTTP 测试里被间接覆盖，包内没有单测。闸门当场报警，补上 `TestDeleteTool` 后回到 82.1%。
- **这条经历说明**：集成测试的覆盖**不回流**到被调用包；「行为在别处测过」不能替代包内的单元验证。覆盖率闸门正是用来抓这种「测试债」的。

---

### 2026-09-13（第三次）· 模板静态校验 + 运行中注销守卫 + 枚举式声明逐项登记

**本轮交付（代码+测试已于上一轮完成，本次补文档回填）**
- **模板静态校验（REQ-TMPL-VALID）**：`internal/engine/template.go` 收敛 `${...}` 为**唯一解析器** `scanTemplateRefs`；`validateTemplates` 在注册期校验「前缀合法（vars/steps）/ vars 顶层键存在 / steps 带 id」，未知前缀（secrets）注册期拒。CLI `flow register` 与 REST `POST /api/flows`（经 `ValidateFlowStatic`）共用。
- **运行中注销守卫（REQ-DEL-TOOL-RUNNING）**：registry 注入 `RunningChecker`，注销第三条「运行中 Execution 持有的 Tool 不可注销」落地；REST `DELETE /api/tools/{id}` 运行中 409、结束后 200。`engine.RunFlowResultObserved` 在快照点回报计划用到的 Tool id（含仅作 Hook action 的），使运行侧引用对守卫可见（静态引用看不到这部分）。
- **修复既有 bug**：`when` 空串被 `compileWhen("")` 判为 `unexpected token EOF` → 所有不含 `when` 的 Flow 都无法过静态校验（REST 一律 400）。`CompileWhen` 空串返回 nil 修复；`TestStaticValidateAcceptsFlowWithoutWhen` 锁死。

**F12 · 枚举式声明必须逐项登记** → 对应 §0 第 15 条
- **规则**：任何「合法取值集合」必须（a）在**单一来源**登记全部合法值；（b）在解析/校验点**拒绝未知值**；（c）在 PRD/矩阵**同步状态**。三者缺一则构成漂移隐患。
- **触发（真实漂移 M11）**：PRD §4.3 把 `${secrets.x}` 列为「支持的变量引用前缀」，而引擎只实现 `vars`/`steps` 且从未登记 `secrets`。代码侧「没有这个前缀」与文档侧「列了它」长期共存、无人察觉——因为既没有单一来源登记、也没有解析点拒绝（未知前缀被静默忽略），PRD/矩阵也没标 deferred。直到补上 `validateTemplates`（未知前缀注册期拒）才暴露。
- **为什么必须写成规则**：模板前缀只是这一类问题的代表；runtime 名、hook stage、criticality、step mode 都是「枚举式取值」。本次之前 `Tool.Validate` 不校验 runtime（G1）也是同一种病。统一成一条红线后，任何新枚举都得走「登记 + 校验 + 文档同步」三步，漂移从根上失去藏身之处。
- **落地**：`template.go` 注释写明「新增语法（如未来的 `${secrets.x}`）须在此登记合法前缀并同步 REQ」；§0 第 15 条 + 本条目共同锁死。

**§0 第 10 条（F3 延伸）的实证补充**
- 模板静态校验是「能静态校验的不留到运行期」的又一次落地：此前 `${secrets.x}` 即使写错也只在**某次执行恰好走到该 step** 才炸，注册期完全放行。校验前移后，错误在 `flow register` / `POST /api/flows` 当场暴露，并指向 `REQ-TMPL-SECRETS` 告知使用者「是等待还是改写」。

**覆盖率闸门再实证**
- 本轮补的 `TestDeleteToolRunningGuard` / `TestDeleteToolWhileRunning` 是**包内单测**：registry 从 82.1%→83.4%、server 从 82.9%→83.9%——印证 S6a 已证结论（集成测试的覆盖不回流到被调用包，「行为在别处测过」不能替代包内单元验证）。`template_test.go` / `planned_test.go` 使 engine 85.8%→87.7%。

## 10. 签批页

- [ ] 可追溯矩阵（§3）条目与测试 ID 认可
- [ ] 测试策略与覆盖率门槛（§2/§5）认可
- [ ] 切片划分与 DoD（§4）认可
- [ ] §13 待确认项处置（§7）认可
- [ ] S0 六处缺陷修复计划（§9）认可

> 签批后，从「S0 重做基线」开始，逐切片按本宪法推进。
