# portability：应用定义的可移植性

应用定义（bundle YAML）里**不得硬编码机器相关绝对路径**。2026-09-25 实测：mindstorm 在
`apps/` 下硬编码了 25 处 `D:/gadern/gear/...`（db 的 dsn、process entry 的 gear 可执行文件、
ext.serve 的 dir），仓库拷到别的机器即跑不起来。

内核已提供相对锚点，应用只写相对值即可跨机器运行：

| 字段 | 错误写法（机器相关） | 正确写法（相对锚点） | 实际落点 |
|---|---|---|---|
| `db` 的 dsn | `D:/gadern/gear/build/mindstorm.db` | `mindstorm.db` | `GEAR_APP_DATA`（默认 `<Home>/data`） |
| process `entry` | `D:/gadern/gear/build/gear.exe serve ...` | `gear serve ...` | gear 可执行文件所在目录（`gearhome.ExeDir`） |
| `ext.serve` 的 `dir` | `D:/gadern/gear/apps/mindstorm/www` | `apps/mindstorm/www` | 安装根 `InstallRoot`（剥掉 build/plugin 两层） |

## 为什么是写作期拦，不是发布期拦

发布后，智能体**主要通过 MCP 接口（gear_validate / gear_apply）来编写应用**，而不是手写 YAML 文件。
所以可移植性等校验发生在 MCP 写作期，作者能立刻拿到字段级诊断去改 YAML。CI 闸门
`tools/portable_audit.py`（质量闸门第⑨项）是兜底——它扫描 `apps/`、`examples/` 下的文件，
与下面的写作期检查**同口径**，二者任一命中都会拦下。

## 触发与修法

- **触发码**：`PORTABLE_PATH_HARDCODED`（写作期，fatal，会让 `gear_validate` 的 `valid` 变
  `false`，`gear_apply` 据此拒绝落库）。
- **判定口径**（与 `tools/portable_audit.py` 对齐）：
  - Windows 盘符绝对路径（`C:\...` / `D:/...`），排除 `http://` 等协议串的假阳性；
  - 类 Unix 机器根路径（`/Users/...`、`/home/...`、`/d/...`、`/tmp/...` 等）。
- **修法**：按上表把绝对路径换成相对锚点。Lua 脚本体内的 dsn 字符串同样会被扫描，一并改掉。

## 反面教材

```yaml
# ❌ 机器相关：换台机器即废
dsn: D:/gadern/gear/build/mindstorm.db
entry: "D:/gadern/gear/build/gear.exe serve --addr 127.0.0.1:8790"
dir: D:/gadern/gear/apps/mindstorm/www
```

```yaml
# ✅ 可移植：相对锚点，跨机器即用
dsn: mindstorm.db
entry: "gear serve --addr 127.0.0.1:8790"
dir: apps/mindstorm/www
```

需求编号：`REQ-APP-PORTABLE`。

---

## 另一半可移植性：产物（`gear_export` / `gear_import`）

上面讲的是「**定义里的路径**可移植」——解决「拷过去跑不起来」。
还有另一件事：**拷什么**。定义不硬编码绝对路径，不等于「这个应用能带走」：

| 组成 | 落在哪 | 随产品走吗 |
|---|---|---|
| Tool / Flow / Hook 定义 | `registry.db` | 能（但要按 id 逐个取） |
| App 清单 | `registry.db` 的 `apps` 表（2026-09-26 起） | 能 |
| 静态资源 | 文件系统，`layout.files` 只做存在性检查 | **不能**——gear 从不持有 |
| 运行台账 | `apps/<id>.json`（pid/端口） | 无意义（机器绑定） |

整拷 `GEAR_HOME` 是**克隆机器**，不是交付。产物把「拷什么」定义成一个自包含的东西：

```
myapp/
├── gear-artifact.yaml   # 格式版本、导出时间、组成清单
├── app.yaml             # kind: App 清单
├── bundle.yaml          # 名下 Tool/Flow/Hook
└── www/…                # 静态资源（相对 layout.workdir 的原样拷贝）
```

- `gear_export(app_id, out_dir, format)` → 目录（默认，可 diff 可 git 版本化）或 `.gearapp` 包。
  **dir 形态的目标目录必须不存在或为空**：gear 不会为一个可重新生成的产物删掉一个内容未知的目录。
- `gear_import(source, overwrite, dry_run, apps_dir)` → 校验格式版本 → 落库（App→Tool→Flow→Hook）。
  冲突语义与 `gear_apply` **逐条一致**（dry_run 不写、未传 overwrite 且冲突则整批拒绝）。
- **静态资源默认不落地**（保持「gear 不持有静态资源」的边界）。传 `apps_dir` 才落地到
  `<apps_dir>/<app-id>/`，并**重写**清单里的 `layout` 指向实际位置——否则导进来的应用
  指向的路径在这台机器上根本不存在。

⚠ 产物只搬**定义与静态资源**，不搬：业务数据（你自己库里的表）、凭据（`requires.secrets`
只是声明，搬运凭据等于把安全边界一起搬走）。

需求编号：`REQ-ARTIFACT-EXPORT` / `REQ-ARTIFACT-IMPORT`。
