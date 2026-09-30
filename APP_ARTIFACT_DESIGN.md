# gear 产物可移植性设计（`gear_export` / `gear_import`）

> 2026-09-26。目标背景：gear 作为**发布出去的平台**交付，构建者是**用户自己的智能体**（经 MCP 面），
> 终极目标 **L3** = 用户能构建出类 PalantirLite 的多用户产品并**交付给别人用**。

---

## 0. 一句话结论

**缺的不是原语，是「产物」这个对象。**

Tool/Flow/Hook/ext/AI/静态托管这套够用（`apps/mindstorm` 是活证）。真正的断点是：
gear 里**不存在「用户构建的产品」这个东西**，于是没有导出、没有组成清单、没有可交付物，
用户在本机构建的一切本质上只是"这台机器上的一堆状态"。

本设计先把这个对象造出来（App 清单入 registry，已完成），再给它一个可搬运的形态。

---

## 1. 现状（2026-09-26 之前）

| 组成 | 落在哪 | 能不能随产品走 |
|---|---|---|
| Tool / Flow / Hook 定义 | `registry.db`（JSON blob） | 能，但只按 id 逐个读，无归属分组 |
| **App 清单** | **只在客户端 YAML 里，落库即丢弃** | **不能** |
| 静态前端资源 | 文件系统，`layout.files` 只做 `os.Stat` | **不能**，gear 不持有 |
| 运行台账 | `apps/<id>.json`（pid/端口/日志路径） | 无意义（机器绑定） |
| 凭据 | `secret` 进程内存 / `config.db` | 不能 |

结论（2026-09-25 审计过的原话）：
> 「用户构建的东西」**不是一个对象，是四处散落的状态**；
> A 机器 → B 机器当前只有一条路：整拷 `GEAR_HOME`。那不是交付，是克隆机器。

**注意区分两件都被叫作"可移植"的事**：

- 2026-09-25 已修的是**定义里的路径可移植**（`AppDataDir()` / `ExeDir()` / `InstallRoot()` +
  相对 dsn 锚 `GEAR_APP_DATA` + `PORTABLE_PATH_HARDCODED` 前移到 `gear_validate`）。
  它解决"拷过去就跑不起来"，**不解决"拷什么"**。
- 本设计解决的是后者：把"拷什么"定义成一个自包含的产物。

---

## 2. 已完成：App 清单入 registry（本设计的前提）

`kind: App` 从 MCP 层的匿名结构体提升为 `model.App`（`internal/model/app.go`），落 `apps` 表，
与 `tools`/`flows`/`hooks` 同构（id + data blob + updated_at），复用同一套 CRUD 原语。

- MCP 层 `type appManifest = model.App` 类型别名 → 解析 / 校验 / warnings 逻辑**零改动**
- `persistBundle` 先落 App（聚合根），再 Tool → Flow → Hook；`overwrite` 冲突语义同款
- `gear_delete(kind=app)` 给出口子，避免改 id 留下孤儿清单
- `gear_describe(kind=app)` 一次返回**运行态 + 清单 + 组成**（归属判定复用 namespace 规则）

**为什么值得为此破一次"PRD 非目标"**：旧注释写的是"保持 PRD 的非目标（只有 Flow + app run，
没有 App 实体）"。这个取舍在 gear 自己用时成立；一旦目标是"让别人在 gear 上构建产品"，
"没有 App 实体"就等价于"没有产品"。

---

## 3. 产物格式 v1（本设计的主体，定死）

### 3.1 目录形态（默认）

```
myapp/
├── gear-artifact.yaml   # 元信息（本格式的版本、导出者、导出时间）
├── app.yaml             # kind: App 清单原样
├── bundle.yaml          # 该 App 名下全部 Tool/Flow/Hook（多文档 YAML，与 gear_apply 入参同形）
└── www/                 # 静态资源（按 layout.files 相对路径原样拷贝）
```

选择**目录**而不是单文件作为主形态的理由：可 diff、可 `git` 版本化、可直接改完再导入——
"用户构建的产物"首先得能被人看懂和比对。

### 3.2 `gear-artifact.yaml`

```yaml
kind: GearArtifact
version: 1                 # 格式版本；导入端不认识的 version 直接拒
app_id: myapp
gear_version: "0.2.0"      # 导出方的 gear 版本，仅记录 + 兼容提示
exported_at: "2026-09-26T05:00:00+08:00"
components:                # 与 gear_describe(kind=app).composition 同源、同一份判定
  entry_flow: myapp.up
  tools: [myapp__list, myapp__get]
  flows: [myapp.up]
  hooks: []
  static_files: ["www/index.html", "www/app.js"]
```

`components` 是**冗余但有价值**的：它让一个没装 gear 的人也能看懂这个产品由什么组成，
也让导入端能在落库前先做一次自洽性检查（声明了但没带的 Tool 要报出来）。

### 3.3 包形态（可选，`--format=zip`）

同一份目录打成 `.gearapp`（zip）。用于"发给别人"。导入端两种形态都吃（按扩展名/内容嗅探）。

### 3.4 静态资源：导出拷贝，**导入默认不落地**

这是本设计里唯一一个需要明说取舍的地方。

| | 行为 | 理由 |
|---|---|---|
| 导出 | **拷贝**静态资源进产物目录 | 产物必须自包含，否则"交付"是假的 |
| 导入（默认） | **只登记清单 + 校验路径存在性**（报 warning 不阻断） | 保持既有边界：gear 从不落地静态资源，那是调用方文件工具的职责 |
| 导入（传 `--apps-dir`） | 落地到该目录 | 给需要"一键还原"的场景留口子 |

**不**在 v1 里默认落地的理由：落哪需要"应用工作目录"这个概念，而它全仓**零命中**，
是一个独立的设计决策（见 REQ-APP-WORKSPACE）。把它夹带进 export/import 会让两件事一起变复杂。

---

## 4. 接口契约

### 4.1 MCP

```
gear_export(app_id, out_dir, format="dir"|"zip", include_static=true)
  → { app_id, out_dir|archive, components, files, warnings[] }

gear_import(source, overwrite=false, dry_run=false, apps_dir="")
  → { valid, would_apply|{created[], updated[]}, warnings[], next }
```

契约沿用 `gear_apply` 的既有约定，不新造语义：

- `dry_run` → 返回 `would_apply.created/updated`，一个字节不写
- `overwrite=false` 且"已存在但内容不同" → `APPLY_CONFLICT` 整批拒绝（原子）
- 列表结果一律经 `objectResult` 包具名键（裸数组是红线）
- 错误码：`NOT_FOUND`（app 不存在）/ `BAD_ARGUMENT` / `APPLY_CONFLICT` / `ARTIFACT_VERSION_UNSUPPORTED`（新增）

### 4.2 CLI

```
gear export <app-id> --out <dir> [--format dir|zip] [--no-static]
gear import <path>   [--overwrite] [--dry-run] [--apps-dir <dir>]
```

工具预算影响：`gear_export` / `gear_import` 会让 MCP 工具从 13 → 15，
**必须同步** `internal/mcp/tools_surface.go` 的 `toolBudget`、
`MCP_AGENT_DESIGN.md` §6.1 的论证与算式、`REQUIREMENTS.md` 的 REQ-MCP-TOOLS 条目
（否则 `tools/doc_audit.py` 的文档↔代码核对会报漂移，`TestMCPToolSurfaceMatchesRegistry` 也会红）。

---

## 5. 实施计划（分三步，每步可独立验收）

| 步 | 内容 | 验收 |
|---|---|---|
| **①**（已完成 2026-09-26） | App 清单入 registry + describe 给组成 | `registry/app_test.go` 5 例 + negcheck 通过；mcp/cli/server/app 四包全绿 |
| **②**（已完成 2026-09-26） | `gear_export`：按 `appComposition` 组装目录/zip，拷静态资源，写 `gear-artifact.yaml` | `artifact_test.go` TestExportDirIsSelfContained / TestExportZipRoundTrip / TestExportWarnsOnMissingStatic / TestExportRefusesNonEmptyDir / TestExportNoStaticOnlyDefinitions |
| **③**（已完成 2026-09-26） | `gear_import`：校验格式版本 → 落库（App→Tool→Flow→Hook）→ 可选落地静态资源 | TestImportLandsStaticAndRewritesLayout / TestImportDryRunWritesNothing / TestImportConflictRequiresOverwrite / TestImportRejectsUnsupportedVersion / TestImportRejectsInvalidDefinitions；zip 导进**全新空库**即「换机器可行」 |

### 实施后补记（与文档的偏差）

三处按实测调整，文档正文以**本段为准**：

1. **静态资源开关改为 opt-out**（`NoStatic`）而非设计里写的 `include_static`：漏设时
   「拷」是安全侧——忘了设就得到一个不能交付的产物，代价太大。MCP 侧仍是 `include_static`（默认 true）。
2. **`gear_describe` 的 `static_files` 改为相对 `layout.workdir`**（原样路径 → 剥前缀）。
   与产物内路径统一：`apps/demo/www/index.html` + workdir `apps/demo` ⇒ `www/index.html`。
   导出方的仓库摆放习惯不是应用的一部分。
3. **导入落地时会重写 `layout`**（指向 `<apps_dir>/<app_id>/…`），否则导进来的应用
   指向的路径在这台机器上不存在——「app.yaml 原样」这一条让位给了「导进来能用」。

**不进 v1 的**：schema 迁移、定义变更历史（`UpsertTool` 覆盖不留旧）、跨 App 隔离、
身份链路。这些是 L2/L3 的下一批，混进来会让第一步做不完。

---

## 6. 非目标（明确不做）

- **不做** Schema 迁移：导出的是定义，不是数据；用户业务表的变化仍需自己管
- **不做** 版本历史/回滚：产物本身可被 `git` 版本化，gear 不重复造
- **不做** 依赖解析：`requires.secrets` 只做声明与检查，不自动搬运凭据
  （凭据是宿主的，搬运凭据等于把安全边界一起搬走，见 `guide/security.md`）
- **不做** 静态资源的内容校验：只保证"拷了哪些、路径对得上"，不做完整性校验
  （v1 先不加 checksum，等真出现"拷坏了没人知道"再说）
