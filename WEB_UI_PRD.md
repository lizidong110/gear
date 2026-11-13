# Gear Web 管理界面（UI 选项）· PRD

> **版本**：v1.1（M1 已落地）
> **日期**：2026-10-21
> **状态**：M1「出界面」已实现（2026-10-21）——`gear serve` 默认同源托管内嵌 Web UI，`gear ui` 一条命令打开界面；M2–M5 待排期
> **定位**：在「控制台优先」的既定设计之上，新增一个**开箱即用的 UI 选项**，让用户用浏览器管理 Gear；内核零侵入、仍以 CLI 为原生入口。

> **M1 落地小结（2026-10-21）**：
> ① `internal/webui` 新包用 `go:embed` 内嵌前端产物，`internal/server` 加 `WithUI` 选项 + `GET /{path...}` 兜底路由（`/api/{path...}` 更具体，工具 API 不被吞）；
> ② `gear serve` 新增 `--no-ui` / `--no-open`，起服打印 `GEAR_OPEN_URL=` 并自动开浏览器；新增 `gear ui` 子命令（探测已有 serve，否则起一个）；
> ③ `scripts/build.sh` 打通 `pnpm build → internal/webui/public → go build`；embed 目录**不叫 dist**（根 `.gitignore` 有全局 `**/dist/`），只忽略真实产物、保留 `_placeholder.html` 保证干净检出也能编译；
> ④ 真机验证：`GET /` 200 真实界面、`/assets/*` 200 正确 MIME、`/api/*` 200、SPA 深链 200 兜底、路径穿越 307、`POST /` 405、`--no-ui` 404+指引、缺产物 503+指引、`gear ui` 两条路径均通；
> ⑤ 9 条 `REQ-UI-*` 已镜像进 `REQUIREMENTS.md`（`requirements_audit` 162 行全绿），`gate.sh` full PASS。

> **文档约定**：本文档每条「需求」以 `REQ-UI-*` 编号，并给出「行为 / 优先级 / 断言测试 / 状态」。实现落地时，须把 `REQ-UI-*` 镜像进 `REQUIREMENTS.md`（与现有 `REQ-*` 同口径），使其成为可执行契约条目。凡标 `deferred` 的，实现前须显式报错而非静默忽略。

---

## 1. 背景与动机

### 1.1 现状

- Gear 当前是**控制台（CLI）+ REST** 形态。`gear serve` 只暴露 `/api`，没有任何界面。
- `plugin/web-ui/`（Vue 3 + Naive UI）已经是一个**功能较完整的管理台**：概览、Tools、Flows、Hooks、Executions、Apps、Secrets、Capabilities 八个页面，契约守卫（枚举漂移 / 类型 / 单测）全绿，真机联调已跑通。2026-11-11 扩至十页：新增「配置」（REQ-UI-RES-CONFIG）与「App 清单」（REQ-UI-RES-APPDEFS）。
- 但「用界面」的门槛很高：必须手动 `npm run build` → 再起一个静态服务器（nginx 或 `ext.serve` 的 `web_ui.up` Flow）→ 自己记住端口 → 自己保证 `/api` 反代同源。普通用户根本走不到最后一步。

### 1.2 痛点（用户视角）

| 痛点 | 后果 |
|------|------|
| 没有一键「出界面」 | 用户卡在「build + 配反代 + 记端口」，最终回到 CLI |
| 端口/路由要自己配 | 配错就是「页面空列表、静默无数据」（已知坑） |
| 内核无 CORS，跨域被浏览器拦 | 想远程/分进程访问直接失败，无提示 |
| 错误信息是机器语言 | 「500」「空列表」「connection refused」对非开发者不友好 |
| 缺首次引导 | 打开页面不知道要连谁、怎么连 |

### 1.3 目标

> **一句话**：`gear serve`（或 `gear ui`）一条命令，浏览器自动打开一个**同源托管、零配置、看得懂**的 Gear 管理界面。内核一行不改，CLI 仍是原生入口。

### 1.4 非目标（沿用既有设计边界，2026-10-02 拍板）

- ❌ **不**在内核加鉴权 / 登录 / 多租户（既定设计：业务鉴权由 app 自负责，平台面不提供）。
- ❌ **不**在内核加 CORS 头（用「同源内嵌服务」规避，而不是给内核开口子）。
- ❌ **不**把 UI 耦合进 `go.mod` 业务包；UI 仍是 `plugin/web-ui/` 插件，删掉内核照常跑。
- ❌ **不**做低代码拖拽编排（PRD v40 §1.4 已否掉）。UI 是**管理/观测台**，不是 Flow 编辑器。
- ❌ **不**取代 CLI；CLI 仍是脚本化、自动化、CI 的原生入口。

---

## 2. 总体方案

### 2.1 架构总览

```
                         ┌──────────────────────────────┐
   用户  ── 浏览器 ─────► │  gear serve （单进程）         │
                         │                              │
                         │   /            → 内嵌 UI(dist) │   ← go:embed，同源
                         │   /api/*        → REST 执行面   │   ← 现有 handler，零改动
                         │   /healthz      → 健康检查      │
                         └──────────────────────────────┘
                                  ▲ 自动打开 http://127.0.0.1:<port>/
                                  │ （首次启动打印 GEAR_OPEN_URL=）
```

- **单二进制、单端口、同源**：UI 静态资源由 `gear serve` 直接托管在与 `/api` 同一 origin 下，浏览器永远同源 → 内核无需任何 CORS 头（守住红线）。
- **可选**：`gear serve` 默认起 UI；`--no-ui` 可关；开发态可用 `GEAR_UI_DIR=plugin/web-ui/dist` 指向实时 build 目录做热更新。

### 2.2 静态资源分发（REQ-UI-SERVE-*）

- 构建期：先 `pnpm build` 产出 `plugin/web-ui/dist`，再由 Go build 通过 `go:embed` 嵌入一个 `internal/webui` 包（**只 embed 构建产物，不 embed 源码，不进业务 go.mod 依赖图**）。
- 运行期：`gear serve` 注册 `GET /` 与 `GET /assets/*` 等静态路由，回退到 `index.html`（hash 路由，无需 rewrite）。`/api/*` 仍由现有 `routes()` 处理，互不影响。
- 缺产物兜底：若二进制未嵌入 dist（如纯 `go build` 漏了 embed 步骤），`GET /` 返回**明确的 503 + 指引**（「UI 未编译进本二进制，请用 make ui 或设 GEAR_UI_DIR」），而不是空白页 / 静默失败（呼应「不静默降级」红线）。

### 2.3 启动与接入（REQ-UI-LAUNCH-*）

- 新增 `gear ui [--open] [--addr :8787]`：等价于「确保目标 `gear serve` 在跑 + 打开浏览器」。若本机没 serve 则**先拉起一个本机 serve 再开浏览器**（用户完全无感）。
- 在资源管理器里**双击 `gear.exe`**（零参数）等价于 `gear ui`；从终端启动（含带任意参数）行为不变，不会把命令行调用变成起服务器。
- `gear serve` 增加：
  - `--ui` / `--no-ui`（默认 `--ui` 开）；
  - 启动后打印 `GEAR_OPEN_URL=http://127.0.0.1:<port>/` 到首行（复用现有 `servePortLine` 机器可读机制 + 人类可读 banner）；
  - 在非 CI 且有桌面环境时自动 `open` / `xdg-open` / `start`（可用 `--no-open` 关）。
- 后台 `gear app start ... --ui` 同样在 `app status` 里回显打开地址（复用 `web_ui.up` 已实现的 `GEAR_OPEN_URL=` 扫描逻辑）。

### 2.4 连接模型（REQ-UI-CONN-*）

- **local-first**：默认连本机 `gear serve`（同源，零配置）。
- **连接管理器**：可保存多个连接（本机 / 远程 / 多 registry），存浏览器 localStorage；切换连接即切换整个 UI 的数据源。
- 连接状态常驻顶部（内核在线 / 离线 / 版本 / flows·tools 计数，复用 `/healthz`）。
- 远程连接**仅作便利**：明确文档告知——远程访问须由用户在 `gear serve` 前**自配鉴权反代**（平台不提供）。UI 不在请求里塞任何凭据头。

---

## 3. 需求清单（详细）

> 优先级：P0 = 阻塞「出界面」、P1 = 用户友好核心、P2 = 增强、P3 = 可选。
> 状态：沿用 `implemented` / `deferred`。本 PRD 阶段统一标 `deferred`（待排期实现），个别已存在于 `plugin/web-ui` 的标 `partial`（已有雏形，需补齐）。

### 3.1 启动与接入（UI 选项本体）

| 需求 ID | 行为 | 优先级 | 断言测试 | 状态 |
|---------|------|--------|---------|------|
| REQ-UI-LAUNCH-SERVE | `gear serve` 默认同源托管内嵌 UI，`GET /` 返回 `index.html`，`/assets/*` 命中静态文件 | P0 | `internal/server/serve_ui_test.go`：TestServeUIIndexServesRealUI、TestServeUIRouteIsolatedFromAPI；`internal/webui/ui_test.go`：TestUIHandlerExternalReal | **implemented**（2026-10-21，见 `REQUIREMENTS.md`） |
| REQ-UI-LAUNCH-NOUI | `gear serve --no-ui` 关闭 UI 路由，`GET /` 返回 404/指引，不影响 `/api` | P1 | `internal/server/serve_ui_test.go`：TestServeUINoUIDisablesRoot | **implemented**（2026-10-21，见 `REQUIREMENTS.md`） |
| REQ-UI-LAUNCH-OPENURL | 启动后 banner + 首行机器可读行均含 `GEAR_OPEN_URL=http://.../` | P0 | `internal/cli/ui_test.go`：TestServePrintsOpenURL（复用 `serveOnListen`，与真实 serve 同一段打印逻辑） | **implemented**（2026-10-21，见 `REQUIREMENTS.md`） |
| REQ-UI-LAUNCH-AUTOOPEN | 非 CI 且有桌面时自动打开浏览器；`--no-open` 可关 | P1 | `internal/cli/openbrowser_test.go`：TestOpenBrowserSkippedInCI、TestAutoOpenDisabledHonorsCIEnv、TestOpenBrowserDoesNotFailServer | **implemented**（2026-10-21，见 `REQUIREMENTS.md`） |
| REQ-UI-LAUNCH-CMD | 新增 `gear ui`：未起 serve 则先起本机 serve 再开浏览器 | P1 | `internal/cli/ui_test.go`：TestUICmdOpensExistingServe、TestUICmdPortProbe | **implemented**（2026-10-21，见 `REQUIREMENTS.md`） |
| REQ-UI-LAUNCH-EXPLORER | 资源管理器双击 `gear.exe`（零参数）等价于 `gear ui`；终端启动与带参启动行为不变 | P1 | `internal/cli/ui_test.go`：TestExplorerLaunchRewritesToUI、TestExplorerLaunchRunsUICmd、TestExplorerLaunchTerminalNoArgsKeepsHelp | **implemented**（2026-11-11，见 `REQUIREMENTS.md`） |
| REQ-UI-LAUNCH-MISSING | 二进制未嵌入 dist 时 `GET /` 返回 503 + 明确指引，不空白 | P0 | `internal/server/serve_ui_test.go`：TestServeUIMissingDistReturnsGuidance；`internal/webui/ui_test.go`：TestUIHandlerMissingMode、TestHandlerEmbeddedFollowsBuildState、TestUIHandlerExternalMissingAssets | **implemented**（2026-10-21，见 `REQUIREMENTS.md`） |
| REQ-UI-LAUNCH-UIOVERRIDE | `GEAR_UI_DIR=<dir>` 指向外部 dist 目录（开发热更新），覆盖内嵌 | P2 | `internal/webui/ui_test.go`：TestUIHandlerExternalReal、TestUIHandlerExternalMissingAssets | **implemented**（2026-10-21，见 `REQUIREMENTS.md`） |
| REQ-UI-LAUNCH-BACKGROUND | `gear app start <flow> --ui` 在 `app status` 回显打开地址 | P2 | 复用 openurl_test 扩展 | deferred |

### 3.2 连接与多环境

| 需求 ID | 行为 | 优先级 | 断言测试 | 状态 |
|---------|------|--------|---------|------|
| REQ-UI-CONN-STATUS | 顶部常驻连接状态（在线/离线/内核版本/flows·tools 计数），基于 `/healthz` 轮询（仅可见时） | P0 | `plugin/web-ui/src/logic/conn.spec.ts`：TestConnHealthPolling | partial |
| REQ-UI-CONN-MANAGER | 连接管理器：增/删/改/选多个连接（本机/远程/多 registry），存 localStorage | P1 | TestConnManagerCRUD（vitest） | partial |
| REQ-UI-CONN-SWITCH | 切换连接即刷新全站数据源；切换中显 loading，失败显可重连提示 | P1 | TestConnSwitchRefetch | deferred |
| REQ-UI-CONN-REMOTE-WARN | 远程/非回环连接显示「平台不提供鉴权，请自配反代」横幅 | P2 | TestConnRemoteWarning | deferred |
| REQ-UI-CONN-OFFLINE | 离线时全站进入「只读 + 提示」态，不假装能写 | P1 | TestConnOfflineReadOnly | deferred |

### 3.3 资源管理（Tools / Flows / Hooks / Secrets / Apps）

> 底层 REST 已齐全（`endpoints.ts` 已镜像）。本组需求聚焦「在 UI 里把能力用顺手」。

| 需求 ID | 行为 | 优先级 | 断言测试 | 状态 |
|---------|------|--------|---------|------|
| REQ-UI-RES-TOOLS | Tools 列表/详情/创建/删除/运行/版本晋升；删除走守卫（被引用→拒，提示引用方） | P0 | `plugin/web-ui/src/views/ToolsView.spec.ts`（含删除守卫交互） | partial |
| REQ-UI-RES-FLOWS | Flows 列表/创建/删除/校验；单 Flow 详情（DAG 分层图 + 步骤参数预览） | P0 | TestFlowsViewCRUD、TestFlowValidateFeedback | partial |
| REQ-UI-RES-HOOKS | Hooks 列表/创建/删除；创建表单按 stage/target/criticality 引导 | P1 | TestHooksViewCRUD | partial |
| REQ-UI-RES-SECRETS | Secrets 列表（只名）/ 写入 / 删除；**写入后绝不回显明文**，列表掩码 `••••` | P0 | TestSecretsViewMasked、TestSecretWriteNoEcho | partial |
| REQ-UI-RES-CONFIG | 配置页按内核 `/api/config/catalog` 渲染中文表单（不硬编码 `ai.*` key）：显示说明/默认值/是否已配置/环境变量覆盖；敏感项只显掩码且**留空保存＝不修改**；支持保存与恢复默认 | P0 | TestConfigCatalogEndpoint、TestConfigSecretNeverLeaks、TestConfigEnvOverrideReported | **implemented**（2026-11-11，见 `REQUIREMENTS.md` REQ-API-CONFIG-CATALOG） |
| REQ-UI-RES-APPS | Apps（长驻进程）列表/启动/停止/清理台账（prune）；状态实时刷新 | P1 | TestAppsViewLifecycle | partial |
| REQ-UI-RES-APPDEFS | **App 清单定义**管理页：列出 registry 里的 `kind: App`（含 Flow/Tool/Hook 组成）、查看详情；**导出**为 YAML 文本下载、**导入**从粘贴/上传的 bundle YAML（先 `dry_run` 预检再写，冲突需显式 `overwrite`）；删除走确认；页面须讲清「清单定义 ≠ 受管进程」，并如实说明静态资源不在文本导出内 | P0 | TestAppDefImportRoundTrip、TestAppDefDryRunWritesNothing、TestAppDefConflictRequiresOverwrite、TestAppDefRejectsBadBundle、TestAppDefDelete | **implemented**（2026-11-11，`AppDefsView.vue`；内核 REST 见 REQ-API-APP-DEFS） |
| REQ-UI-RES-CREATE-WIZARD | 创建 Tool/Flow 用**向导 + YAML 编辑器双模式**：向导出结构、高级切 YAML；提交前跑前端静态预检（复用 `template.ts`/`tool.ts`） | P1 | TestCreateWizardPrecheck、TestYAMLEditorRoundTrip | deferred |
| REQ-UI-RES-IMPORT | 粘贴/上传 YAML 直接注册（对应 CLI `register`），错误定位到行 | P1 | TestImportYAMLErrorLine | deferred |
| REQ-UI-RES-SEARCH | 所有列表支持名称/标签搜索 + 状态筛选（不依赖后端新端点，本地索引） | P1 | TestListSearchFilter | deferred |

### 3.4 执行与可观测性

| 需求 ID | 行为 | 优先级 | 断言测试 | 状态 |
|---------|------|--------|---------|------|
| REQ-UI-EXEC-RUN | 任意 Flow/Tool 一键运行；入参表单由 schema 动态生成（必填校验、类型提示） | P0 | TestRunFormSchemaDriven、TestRunFormRequiredValidation | partial |
| REQ-UI-EXEC-LIVE | 运行中实时刷新状态；running 时详情 1s 轮询（仅 visible），终态自动停。「运行日志」tab 另提供「实时」开关：开启走 SSE 流（GET /api/executions/{id}/logs/stream，只推订阅后新行，历史仍先拉一次），取代 1s 日志轮询 | P0 | TestExecLivePollingStopsOnTerminal | partial |
| REQ-UI-EXEC-LOGS | 执行日志页：按行展示、自动滚底、可搜关键词、可下载 | P1 | TestExecLogsDownload | partial |
| REQ-UI-EXEC-GRAPH | 编排图（FlowGraph）可读：节点状态着色（待执行/执行中/成功/跳过/失败）、点节点看 input/output | P0 | TestFlowGraphNodeStates | partial |
| REQ-UI-EXEC-HISTORY | 执行历史列表（时间/流/状态/耗时/用到的 Tool），可重跑/查看 | P1 | TestExecHistoryRerun | partial |
| REQ-UI-EXEC-RETRY | 失败执行一键重跑（同入参） | P2 | TestExecRetrySameInput | deferred |
| REQ-UI-EXEC-EXPORT | 执行结果/日志导出 JSON 或复制为可分享文本 | P2 | TestExecExportJSON | deferred |
| REQ-UI-APP-LOGS | Apps 页实时日志抽屉（台账应用，REQ-APP-MANAGED-DETACHED 后与托管启动同表）：每条可打开**实时日志**抽屉，开关默认开、可随时断开；只显示订阅后新产生的 process/service 输出，**不读日志文件、不回放历史**；`live_logs=false` 时显式提示未接入及恢复办法（重启该应用） | P0 | TestStreamAppLogsReceivesLines、TestListAppRecordsReportsLiveLogs、TestIngestAppLogsRequiresTokenAndKnownApp | **implemented**（2026-11-11 见 REQ-API-APP-LOGS；2026-11-13 两表合一，`GET /api/apps/records` 移除） |

### 3.5 能力目录与调用

| 需求 ID | 行为 | 优先级 | 断言测试 | 状态 |
|---------|------|--------|---------|------|
| REQ-UI-CAP-BROWSE | 能力目录树（builtin / pure / ext.*），展开看方法签名与参数 schema | P1 | TestCapBrowserTree | partial |
| REQ-UI-CAP-INVOKE | 选中方法直接构造入参并调用，结果高亮展示 | P1 | TestCapInvokeForm | partial |
| REQ-UI-CAP-STREAM | AI `complete_stream` 走 SSE，逐字流式渲染（delta/reasoning/done/error） | P1 | TestCapStreamRender（复用 `streamInvokeCapability`） | partial |
| REQ-UI-CAP-FORBID-SECRET | 调用面**禁止**传 `secret.*` 相关入参（与 MCP `gear_invoke` 同口径），UI 直接禁用并提示 | P1 | TestCapInvokeBlocksSecret | deferred |

### 3.6 用户体验（用户友好专项 — 本 PRD 重点）

> 这一组是「保证用户友好」的核心。每项都要落到可感知的细节。

| 需求 ID | 行为 | 优先级 | 断言测试 | 状态 |
|---------|------|--------|---------|------|
| REQ-UI-UX-ONBOARD | **首次引导**：打开即检测连接；未连时给「连本机 / 连远程 / 我看文档」三步卡片，不丢给用户一个空壳 | P0 | TestOnboardConnectLocal、TestOnboardEmptyState | deferred |
| REQ-UI-UX-PLAINTEXT-ERR | 所有错误转**人话**：把 `400/404/409/500` + 内核 `errorBody` 映射成「为什么 + 下一步」（如「删除失败：该 Tool 正被 Flow <x> 引用，先去解绑」） | P0 | `plugin/web-ui/src/api/client.spec.ts`：TestErrorHumanMapping、TestDeleteInUseMessage | partial |
| REQ-UI-UX-CONFIRM | **破坏性操作一律确认**：删除 Tool/Flow/Hook/Secret、停止 App、prune——弹窗列清「将影响什么」，二次确认 | P0 | TestDestructiveConfirmDialog | deferred |
| REQ-UI-UX-THEME | 明/暗主题切换，记忆偏好；默认跟随系统 | P1 | TestThemeTogglePersist | partial |
| REQ-UI-UX-RESPONSIVE | 桌面/平板可用；侧栏在窄屏收起为抽屉 | P1 | TestResponsiveSidebar | deferred |
| REQ-UI-UX-SHORTCUT | 键盘快捷键（/ 搜索、g+t 跳 Tools、g+f 跳 Flows、? 帮助） | P2 | TestKeyboardShortcuts | deferred |
| REQ-UI-UX-I18N | **界面文案默认中文（zh-CN）**；预留 i18n 框架（en 至少占位），不硬编码中文串 | P1 | TestI18nZhDefault、TestI18nSwitchEn | deferred |
| REQ-UI-UX-EMPTY | 所有空列表有**引导性空态**（做什么、怎么开始），不显示裸「无数据」 | P1 | TestEmptyStates | deferred |
| REQ-UI-UX-STATUS | 统一状态语义色（成功/失败/跳过/背压拒/否决），复用 `StatusTag`，不重复键 | P0 | TestStatusTagNoKeyClash（已有类型检查兜底） | partial |
| REQ-UI-UX-FEEDBACK | 长操作（运行/注册）有进度与可取消；不阻塞界面 | P1 | TestLongOpCancellable | deferred |
| REQ-UI-UX-A11Y | 基本可达性：焦点可见、ARIA 角色、对比度达标 | P2 | TestA11yBasics | deferred |
| REQ-UI-UX-DASHBOARD | 概览页：内核健康、资源计数、最近执行、热门 Flow 快捷运行——一屏看懂「Gear 现在咋样」 | P0 | TestDashboardWidgets | partial |

### 3.7 安全边界（明确不越界）

| 需求 ID | 行为 | 优先级 | 断言测试 | 状态 |
|---------|------|--------|---------|------|
| REQ-UI-SEC-MASK | Secret 在列表/详情**永远掩码**；明文只存在于「写入时的一次性输入框」，不进 localStorage、不进日志 | P0 | TestSecretNeverPersistedPlaintext | partial |
| REQ-UI-SEC-NOAUTH | UI **不**实现任何登录/会话；信任级别等同本机 CLI（local-first） | P0 | 设计约束，TestUINoAuthHeaderSent | deferred |
| REQ-UI-SEC-REMOTE-DOC | 远程访问文档明确写「自配鉴权反代」，UI 远程横幅复用 REQ-UI-CONN-REMOTE-WARN | P1 | 文档 + TestConnRemoteWarning | deferred |
| REQ-UI-SEC-AUDIT | 破坏性操作在 UI 留**本地操作记录**（不依赖内核审计），便于回溯 | P2 | TestLocalAuditLog | deferred |

### 3.8 非功能 / 工程

| 需求 ID | 行为 | 优先级 | 断言测试 | 状态 |
|---------|------|--------|---------|------|
| REQ-UI-NF-EMBED-PIPE | 构建管线：`pnpm build` → `go:embed` → `go build` 一条龙（Makefile / CI），缺 dist 即失败不静默 | P0 | `internal/webui/build_pipeline_test.go`：TestBuildPipelineEmbedsWebUI、TestWebUIEmbedDirIsNotGitIgnored；`internal/webui/ui_test.go`：TestWebUIEmbedHasPlaceholder | **implemented**（2026-10-21，见 `REQUIREMENTS.md`） |
| REQ-UI-NF-CONTRACT | 沿用 `check:contract`（枚举契约漂移守卫）：Go 枚举改动必须 `gen:enums`，UI 端到端契约与内核路由表一致 | P0 | TestContractNoDrift（复用现有 gate） | partial |
| REQ-UI-NF-PERF | 概览/列表首屏 < 1.5s（本地同源）；万级列表不卡（虚拟滚动或分页） | P2 | TestListVirtualScroll | deferred |
| REQ-UI-NF-OFFLINE-ASSET | UI 静态资源全内嵌，断网可用（数据请求才需内核） | P1 | TestUIAssetsNoExternalDep | deferred |
| REQ-UI-NF-NO-KERNEL | 不修改 `internal/server` 的 `routes()` 业务语义；新增仅 UI 静态路由，可独立开关 | P0 | `internal/server/serve_ui_test.go`：TestServeUIRouteIsolatedFromAPI、TestServeUINoUIDisablesRoot | **implemented**（2026-10-21，见 `REQUIREMENTS.md`） |

---

## 4. 用户旅程（典型友好路径）

**场景 A：本机用户第一次用**
1. 装好 gear，终端敲 `gear serve`（或 `gear ui`）。
2. 浏览器自动弹开 `http://127.0.0.1:8787/`。
3. 首次引导卡片：检测到本机 serve 在线，点「开始使用」。
4. 概览页一屏看见：内核在线、3 个 Tool、2 个 Flow、最近 5 次执行。
5. 想跑 `news.top10`：点进去 → 入参表单（无入参则一个「运行」按钮）→ 点运行 → 实时看日志滚动 + 编排图节点变绿 → 出结果。

**场景 B：远程运维**
1. 运维在跳板机 `gear serve --allow-remote` 前面自配了带鉴权的反代。
2. 本地浏览器打开反代地址，连接管理器填「远程」连接。
3. 顶部出现黄色横幅：「远程连接，平台不提供鉴权，请确认反代已加登录」。
4. 其余操作同本机。

**场景 C：出错也不懵**
1. 用户想删一个 Tool，但它被某 Flow 引用。
2. 点删除 → 确认弹窗列出「将被影响的 Flow：x, y」→ 用户取消。
3. 改去解绑 Flow 后再删 → 成功。全程没有「500 空白」。

---

## 5. 里程碑

| 里程碑 | 范围 | 退出标准 |
|--------|------|---------|
| **M1 出界面** ✅ 已落地 2026-10-21 | REQ-UI-LAUNCH-*、REQ-UI-NF-EMBED-PIPE、REQ-UI-NF-NO-KERNEL、REQ-UI-LAUNCH-MISSING | `gear serve` 一条命令同源出界面；缺产物有指引；构建管线可复现 |
| **M2 能管资源** | REQ-UI-RES-*（除 WIZARD/IMPORT）、REQ-UI-EXEC-RUN/LIVE/LOGS/GRAPH、REQ-UI-UX-DASHBOARD | 八页面资源可 CRUD + 运行 + 看结果；概览一屏看懂 |
| **M3 连接与友好底座** | REQ-UI-CONN-*、REQ-UI-UX-ONBOARD/PLAINTEXT-ERR/CONFIRM/STATUS/EMPTY/THEME | 首次引导、人话错误、破坏性确认、主题、空态齐全 |
| **M4 能力面与增强** | REQ-UI-CAP-*、REQ-UI-RES-CREATE-WIZARD/IMPORT/SEARCH、REQ-UI-EXEC-HISTORY/RETRY/EXPORT | 能力目录可调用+流式；创建向导+YAML 双模；历史可重跑 |
| **M5 打磨（可选）** | REQ-UI-UX-SHORTCUT/I18N/A11Y、REQ-UI-NF-PERF/OFFLINE-ASSET、REQ-UI-SEC-AUDIT | 快捷键、i18n、可达性、性能达标 |

---

## 6. 风险与开放问题

1. **embed 体积**：dist 约数百 KB~数 MB，进二进制可接受；若未来膨胀，退回 `GEAR_UI_DIR` 外部模式。
2. **双产物同步**：Go 枚举改了但忘跑 `gen:enums` → 契约守卫必须挡在 CI（沿用现有 `check:contract`）。
3. **远程安全**：UI 不鉴权是设计红线；远程场景只能靠用户反代。PRD 已用横幅 + 文档兜底，**不**在内核加鉴权。是否要提供「反代模板（nginx/Caddy）」作为发行物？→ 待定（建议 M5 附带）。
4. **`gear ui` 自起 serve 的生命周期**：前台 `app run` 受 Job Object 约束；`gear ui` 自起的 serve 在命令退出时要干净回收（复用现有 serve 停机逻辑）。
5. **与既有 `web_ui.up` Flow 的关系**：M1 落地后，`plugin/web_ui_*.yaml` 那套「手动起站」流程可标 deprecated，但保留不删（作为「不想嵌二进制、想要独立静态站」的旁路）。

---

## 7. 验收标准（合入前）

- [ ] `gear serve` 启动后浏览器同源可访问 UI，`/api` 与 `/` 同 origin 无 CORS 报错（浏览器 Network 面板验证）。
- [ ] 二进制未嵌 dist 时 `GET /` 返回 503 + 指引，而非空白。
- [ ] 八页面资源 CRUD + 运行 + 实时日志 + 编排图着色全可用（真机联调，非 mock）。
- [ ] 删除被引用资源时 UI 弹确认并提示引用方；Secret 明文不落 localStorage/日志。
- [ ] 契约守卫（`check:contract` + `build` + 单测）在 UI 改动后全绿。
- [ ] `REQ-UI-*` 全部镜像进 `REQUIREMENTS.md` 并有对应断言测试。

---

## 附录 A：REST 端点 ↔ UI 需求映射（真实来源：`plugin/web-ui/src/api/endpoints.ts` + `internal/server/server.go:routes()`）

| REST 端点 | 方法 | UI 用途 | 对应需求 |
|-----------|------|---------|---------|
| `/healthz` | GET | 连接状态 | REQ-UI-CONN-STATUS |
| `/news` | GET | 演示入口 | REQ-UI-UX-DASHBOARD |
| `/capabilities` | GET | 能力目录树 | REQ-UI-CAP-BROWSE |
| `/capabilities/invoke` | POST | 调用能力 | REQ-UI-CAP-INVOKE |
| `/capabilities/invoke/stream` | POST(SSE) | AI 流式 | REQ-UI-CAP-STREAM |
| `/secrets` | GET | 密钥列表（名） | REQ-UI-RES-SECRETS |
| `/secrets/{name}` | PUT/DELETE | 写/删密钥 | REQ-UI-RES-SECRETS |
| `/tools` `/tools/{id}` | GET/POST/DELETE | 工具管理 | REQ-UI-RES-TOOLS |
| `/tools/{id}/run` | POST | 运行工具 | REQ-UI-EXEC-RUN |
| `/tools/{id}/versions/{v}/promote` | POST | 版本晋升 | REQ-UI-RES-TOOLS |
| `/flows` `/flows/{id}` | GET/POST/DELETE | 流管理 | REQ-UI-RES-FLOWS |
| `/flows/{id}/validate` | POST | 静态校验 | REQ-UI-RES-FLOWS |
| `/hooks` | GET/POST/DELETE | 钩子管理 | REQ-UI-RES-HOOKS |
| `/executions` `/executions/{id}` | GET/POST | 执行与历史 | REQ-UI-EXEC-* |
| `/executions/{id}/logs` | GET | 日志（历史） | REQ-UI-EXEC-LOGS |
| `/executions/{id}/logs/stream` | GET(SSE) | 实时日志（只推订阅后新行） | REQ-API-LOGS-STREAM |
| `/events/{handle}` | POST | 订阅回推 | （内核机制，UI 一般不直接调） |
| `/apps` `/apps/{id}` `/apps/prune` | GET/POST/DELETE/POST | 长驻进程（内存态） | REQ-UI-RES-APPS |
| `/apps/records` | GET | 后台应用落盘台账（选订阅对象） | REQ-UI-APP-LOGS |
| `/apps/{id}/logs` | POST | 子进程批量回推日志行（token） | REQ-API-APP-LOGS |
| `/apps/{id}/logs/stream` | GET(SSE) | 台账应用实时日志（不读文件） | REQ-UI-APP-LOGS |
| `/app-defs` `/app-defs/{id}` | GET/DELETE | App 清单定义 | REQ-UI-RES-APPDEFS |
| `/app-defs/{id}/export` | GET | 导出多文档 YAML 文本 | REQ-UI-RES-APPDEFS |
| `/app-defs/import` | POST | 导入（`dry_run` / `overwrite`） | REQ-UI-RES-APPDEFS |
| `/config/catalog` `/config` | GET | 配置键目录与当前值 | REQ-UI-RES-CONFIG |
| `/config/{key}` | PUT/DELETE | 写入 / 恢复默认 | REQ-UI-RES-CONFIG |

> 内核路由表是单一真相源；UI 的 `endpoints.ts` 与本文附录须与其逐条对齐，漂移由 `check:contract` 兜底。
