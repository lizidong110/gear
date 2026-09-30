// Package gearhome 是「gear 自身数据落在哪」的**单一真相源**。
//
// 存在的理由（2026-09-21 实测缺陷）：registry、执行记录库、以及 doctor 的产物查找，
// 此前各自用**相对 cwd** 的默认值（`~/.gear/registry.db` 之外，`build/gear.db` 与 `build/`）。
// 这在 CLI 手跑时看不出问题（人总是 `cd build` 再跑），但一放到「别人派生我」的场景就致命：
//
//   - WorkBuddy 派生 `gear mcp` 时 cwd 由客户端决定。执行记录于是落到 `<cwd>/build/gear.db`，
//     并在用户的任意目录里凭空造出 `build/` 文件夹；换个 cwd 再跑，同一份执行历史就「不见了」。
//   - 同一轮里 `extensions.yaml` 因按 `./extensions.yaml` 定位而读不到，7 个 ext 能力全部消失。
//
// 结论（并作为红线）：**gear 自身的落盘位置不得依赖 cwd**。cwd 是调用方的属性，
// 不是 gear 的属性；任何「默认路径」都必须锚定在「用户数据目录」或「可执行文件目录」上。
//
// 解析顺序（GEAR_HOME 优先，便于多实例隔离与测试）：
//
//	GEAR_HOME  →  <用户主目录>/.gear  →  <可执行文件同目录>/.gear  →  "."
//
// 最后一级兜底只为「极端环境下没有 HOME」时仍能给出可用路径，不追求好看。
//
// 另一条正交的传导规则（2026-09-22 新增）：**生效的 registry 必须顺着进程树传下去**。
// 见 RegistryPath 的说明。
package gearhome

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// EnvHome 是覆盖用户数据目录的环境变量。
const EnvHome = "GEAR_HOME"

// EnvExecDB 是覆盖执行记录库路径的环境变量。
const EnvExecDB = "GEAR_EXEC_DB"

// EnvConfigDB 是覆盖键值配置库路径的环境变量。
//
// config 原语的落点默认与 registry / exec 同目录（<Home>/config.db），理由与 ExecDBPath 相同：
// 三者都是「这台机器上这一套 gear」的持久面，分开只会重现「换 cwd 就换一份配置」。
// 要隔离就设 GEAR_HOME，而不是单独挪这一个库。
const EnvConfigDB = "GEAR_CONFIG_DB"

// EnvRegistry 是覆盖 registry（目录库）路径的环境变量。
//
// 它存在的理由是「**继承**」而不是「配置」：一个进程用 `--registry X` 起服后，它派生出去的子进程
// （长驻 Tool 的 entry、`gear app run` 的监管进程）必须读**同一个** X。此前没有任何传导机制，
// 子进程只能各自解析默认值——于是出现了「应用自带的 REST 面读另一个库、flows 列表空」这类
// 完全静默的故障（2026-09-22 实测：`web_ui.api` 的 entry 写死相对 `--registry`）。
// 生效的路径由发起方（CLI 的 openRegistry）绝对化后写进本变量，子进程只需读它。
const EnvRegistry = "GEAR_REGISTRY"

// Home 返回 gear 的用户数据目录（默认 `~/.gear`）。
//
// 不做缓存：Home 的调用点都在进程启动路径上（一次），而缓存会让测试无法用 t.Setenv 隔离。
func Home() string {
	if v := os.Getenv(EnvHome); v != "" {
		return v
	}
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		return filepath.Join(h, ".gear")
	}
	// 极端兜底：没有 HOME 时用可执行文件所在目录，至少是个稳定且可写的位置。
	if exe, err := os.Executable(); err == nil {
		return filepath.Join(filepath.Dir(exe), ".gear")
	}
	return ".gear"
}

// RegistryPath 返回**本进程生效的** registry（目录库）默认路径：`GEAR_REGISTRY` → `<Home>/registry.db`。
//
// 为什么默认值要读环境变量（与 ExecDBPath 同一个理由，但动机不同）：
// registry 是「一次调用选定、一整棵进程树都必须沿用」的东西。发起方（CLI 的 `openRegistry`）
// 把这次真正生效的路径绝对化后写进 `GEAR_REGISTRY`，它派生的一切子进程就自动同源——
// 子进程不必、也不该再去猜「我的 cwd 意味着哪个库」。
//
// 这是**默认值**：显式 `--registry` 仍然优先（flag 覆盖默认，见 internal/cli 的 root.go）。
// 保留这个优先级是刻意的——机制负责让「没说的时候」永远一致，不负责替人说过的决定翻案。
func RegistryPath() string {
	if v := os.Getenv(EnvRegistry); v != "" {
		return v
	}
	return filepath.Join(Home(), "registry.db")
}

// ExecDBPath 返回执行记录库的默认路径：`GEAR_EXEC_DB` 优先，否则 `<Home>/exec.db`。
//
// 与 registry 同目录是**有意为之**：两者是「这台机器上这一套 gear」的两个持久面，
// 分开放只会让「换一个 cwd 就换一份历史」这类问题重现。要隔离就设 GEAR_HOME。
func ExecDBPath() string {
	if p := os.Getenv(EnvExecDB); p != "" {
		return p
	}
	return filepath.Join(Home(), "exec.db")
}

// ConfigDBPath 返回键值配置库（config 原语 + 内核 AI 缺省配置）的默认路径：
// `GEAR_CONFIG_DB` 优先，否则 `<Home>/config.db`。
//
// 它是「应用配置不用每次靠环境变量灌进进程」这一诉求的落点：应用声明 `capabilities: [config]`
// 即可经 ctx.config.get/set 读写，Lua 沙箱里读不到 GEAR_HOME 也无妨——宿主知道库在哪。
func ConfigDBPath() string {
	if p := os.Getenv(EnvConfigDB); p != "" {
		return p
	}
	return filepath.Join(Home(), "config.db")
}

// EnvAppData 是覆盖「应用持久数据目录」的环境变量。
const EnvAppData = "GEAR_APP_DATA"

// AppDataDir 返回应用持久数据的根目录：`GEAR_APP_DATA` 优先，否则 `<Home>/data`。
//
// 存在的理由（2026-09-25 实测缺陷）：db 原语的 dsn 缺省回退「引擎内存 scratch 库，
// 重启即丢且不报错」，应用想持久化就必须显式传 dsn；而相对 dsn 由 SQLite 按**子进程
// cwd** 解析，Lua 沙箱又禁了 os 库、模板也没有 ${env} 前缀，脚本根本读不到 GEAR_HOME
// ——于是应用只能把绝对路径硬编码进 YAML（mindstorm_data.yaml 里 18 处
// `local DSN = "D:/gadern/gear/build/mindstorm.db"`），整个仓库拷到别的机器就跑不起来。
//
// 由宿主把「相对库名」锚定到这里之后，应用只需写 `dsn = "mindstorm.db"`：落点与 cwd 无关、
// 与机器无关。要隔离就设 GEAR_HOME（或单独设 GEAR_APP_DATA），与 ExecDBPath 同口径。
func AppDataDir() string {
	if v := os.Getenv(EnvAppData); v != "" {
		return v
	}
	return filepath.Join(Home(), "data")
}

// ExeDir 返回本进程可执行文件所在目录；os.Executable 失败时返回空串。
//
// 调用方必须判空：拿空串去拼路径等于退化成「相对 cwd」，正是本包要消灭的行为。
//
// 为什么收口在这里：此前 internal/engine/extensionpath.go、internal/app/launch.go 与本包
// 各自算一遍 os.Executable——同一件事五处实现，改一处漏四处。
func ExeDir() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return filepath.Dir(exe)
}

// InstallRoot 返回 gear 的**安装根目录**（apps/、plugin/、README.md 所在的那一层）。
//
// 布局约定（scripts/build.sh 的产物形态）：`<root>/build/gear.exe`、
// `<root>/build/plugin/*-sidecar.exe`。于是规则是「从 exe 所在目录向上剥掉 plugin / build
// 这两层已知的名字」：静态站点这类**随仓库发布**的资源（apps/mindstorm/www）相对安装根
// 表达才是可移植的，相对 cwd 表达只在「恰好从仓库根启动」时成立。
//
// 未知布局（`go run` 时 exe 落在临时目录）不猜测：返回 exe 所在目录，由调用方的存在性
// 判断决定成败（fail-closed，绝不静默拼一个不存在的路径出来）。
func InstallRoot() string {
	dir := ExeDir()
	if dir == "" {
		return ""
	}
	return installRootFrom(dir)
}

// installRootFrom 是 InstallRoot 的纯函数内核：把「从 exe 目录反推安装根」这一步单独
// 拎出来，测试才能在不摆真实 build/ 布局的前提下锁死剥层规则。
func installRootFrom(dir string) string {
	clean := filepath.Clean(dir)
	cur := clean
	for i := 0; i < 2; i++ {
		if strings.EqualFold(filepath.Base(cur), "build") {
			return filepath.Dir(cur)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
	}
	return clean
}

// ArtifactDirs 返回「构建产物可能待在哪些目录」的候选序列，按可信度排序、已去重。
//
// 用于回答「某个 *-sidecar 二进制在不在」这类问题（doctor 的能力自检）。它同样**不能依赖 cwd**：
// 客户端以任意 cwd 派生 `gear mcp` 时，此前只看 `./build/plugin` 与 `./build`，于是 doctor
// 会对着一台侧车齐全的机器报「能力可用但未找到侧车二进制」——把智能体引去重编译，
// 而真正的问题是它在错误的目录里找。
//
// 顺序与理由：
//  1. `<exe 同目录>`：发布态可能把 gear 与侧车平铺在一起；
//  2. `<exe 同目录>/plugin`：本仓 scripts/build.sh 的布局（`build/gear.exe` + `build/plugin/*`）；
//  3. `<exe 同目录>/../build`、`<exe 同目录>/../build/plugin`：`go run` 时 exe 在临时目录，
//     用它的父目录兜一层；
//  4. `cwd` 侧的两个名字：保留给「exe 与产物都被人为分开」的开发场景，排在最后（最低可信）。
func ArtifactDirs() []string {
	var out []string
	seen := map[string]bool{}
	add := func(p string) {
		if p == "" {
			return
		}
		p = filepath.Clean(p)
		if seen[p] {
			return
		}
		seen[p] = true
		out = append(out, p)
	}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		add(dir)
		add(filepath.Join(dir, "plugin"))
		add(filepath.Join(dir, "..", "build"))
		add(filepath.Join(dir, "..", "build", "plugin"))
	}
	add(filepath.Join("build", "plugin"))
	add("build")
	return out
}

// PosixPathHint 报告「在 Windows 上收到了一个 POSIX 绝对路径」这一误用，并给出真实落点。
//
// 为什么值得一条护栏（2026-09-21 实测，同一个坑连着踩了两次）：
//
//	`go build -o /d/gadern/gear/build/gear.exe`  → 产物静静落到 `D:\d\gadern\gear\build\`，
//	                                             返回 0，脚本报 PASS，跑的还是旧二进制；
//	`gear app start --registry /d/x/reg.db`      → 台账与注册库落到 `D:\d\x\reg.db`。
//
// 根因是 Windows 把 `/d/gadern/...` 当成「当前盘符根下的绝对路径」（`\d\gadern\...`）——
// `filepath.IsAbs` 在 Windows 上对它返回 true，于是**任何校验都拦不住**，只能靠人看出来。
// Git Bash / MSYS 通常会把这类参数转换成 `D:\...`，但转换并非在所有调用形态下都发生
// （实测 `--flag /d/...` 就没转），所以这不是「用户手滑」，而是平台边界上的真实陷阱。
//
// 判据刻意收窄到「单个 ASCII 字母」作为首段：`\d\...`、`\c\...` 这种形态几乎只可能来自
// 未转换的 MSYS 路径；而合法的驱动器相对路径（`\Windows\System32`）首段是词，不会误报。
//
// 返回空串表示没有问题。**只用于提示，不改变任何行为**：路径仍然按调用方的字面值使用，
// 因为「你传什么就是什么」是更可预期的语义，护栏的职责只是让人看见真实落点。
func PosixPathHint(p string) string {
	if runtime.GOOS != "windows" || p == "" {
		return ""
	}
	s := filepath.ToSlash(p)
	// 只认「以单个 / 开头、首段是单个字母、后面还有内容」：即 /d/xxx 形态。
	if !strings.HasPrefix(s, "/") {
		return ""
	}
	rest := s[1:]
	seg, tail, found := strings.Cut(rest, "/")
	if !found || len(seg) != 1 {
		return ""
	}
	c := seg[0]
	if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') {
		return ""
	}
	drive := strings.ToUpper(seg)
	native := drive + ":\\" + filepath.FromSlash(tail)
	return "路径 " + p + " 看着像 POSIX 形式；Windows 版 gear 会把它当成「当前盘符根下的绝对路径」，" +
		"真实落点是 " + native + "。若是从 Git Bash 传参，请改用 " + native + " 形式。"
}
