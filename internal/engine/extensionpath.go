package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// extensionConfigEnv 是显式指定扩展配置路径的环境变量名。
const extensionConfigEnv = "GEAR_EXT_CONFIG"

// ResolveExtensionConfigPath 定位 extensions.yaml，返回（路径, 错误）。
//
// 返回空路径表示「本机没有扩展配置」，调用方据此回退默认 ext.ai（既有语义，不变）。
//
// 为什么不能只看 cwd（2026-09-21 实测缺陷）：宿主（如 WorkBuddy）派生 gear 时 cwd 不可控，
// 而 `./extensions.yaml` 的约定要求 cwd 恰好是 `<repo>/build`。cwd 一旦不同，配置读不到
// ⇒ 7 个 ext 能力全部消失 ⇒ 声明 `ext.*` 的 Tool 一律被拒（`未知的 capability "ext.serve"`）
// ⇒ MCP 面下根本注册不了带扩展能力的应用。相对 cwd 定位一个「随二进制走」的配置文件，
// 本身就是把部署细节泄漏成了运行前提。
//
// 因此改为**以可执行文件为锚**的候选序列（见 extensionConfigCandidates），cwd 降级为兜底。
//
// fail-closed：GEAR_EXT_CONFIG 显式指了路径却读不到 → 报错而不是静默回退默认 ext.ai。
// 「配了却等于没配」是本项目反复吃亏的故障形态（能力静默缺失、错在很久之后才显形）。
func ResolveExtensionConfigPath() (string, error) {
	exeDir := ""
	if exe, err := os.Executable(); err == nil {
		exeDir = filepath.Dir(exe)
	}
	cwd, _ := os.Getwd()
	return resolveExtensionConfigPath(os.Getenv(extensionConfigEnv), exeDir, cwd)
}

// resolveExtensionConfigPath 是 ResolveExtensionConfigPath 的纯函数内核（便于单测：
// 显式路径 / exe 目录 / cwd 全部由参数给定，不读全局状态）。
func resolveExtensionConfigPath(explicit, exeDir, cwd string) (string, error) {
	if explicit != "" {
		if !fileExists(explicit) {
			return "", fmt.Errorf("%s=%q 指向的文件不存在或不是文件；"+
				"要么修正该环境变量，要么删掉它让 gear 按「可执行文件同目录 → plugin/ → cwd」自动定位",
				extensionConfigEnv, explicit)
		}
		return explicit, nil
	}
	for _, c := range extensionConfigCandidates(exeDir, cwd) {
		if fileExists(c) {
			return c, nil
		}
	}
	return "", nil
}

// extensionConfigCandidates 按优先级列出候选路径。
//
// 顺序即语义：先「跟着二进制走」的稳定位置，再源码仓布局，最后才是不可控的 cwd。
//  1. <exeDir>/extensions.yaml          —— 发布/构建目录布局（build/gear.exe + build/extensions.yaml）
//  2. <exeDir>/plugin/extensions.yaml   —— 只把 plugin/ 子目录一起拷出去的部署
//  3. <exeDir>/../plugin/extensions.yaml —— 源码仓：<repo>/build/gear.exe ↔ <repo>/plugin/extensions.yaml
//  4. <exeDir>/../extensions.yaml       —— 源码仓的另一半：<repo>/extensions.yaml
//  5. <cwd>/extensions.yaml             —— 兜底：历史行为（只在此前都落空时生效）
//  6. <cwd>/plugin/extensions.yaml      —— 兜底：在仓库根目录手工跑 gear 时
func extensionConfigCandidates(exeDir, cwd string) []string {
	var out []string
	add := func(p string) {
		if p != "" {
			out = append(out, p)
		}
	}
	if exeDir != "" {
		up := filepath.Dir(exeDir)
		add(filepath.Join(exeDir, "extensions.yaml"))
		add(filepath.Join(exeDir, "plugin", "extensions.yaml"))
		add(filepath.Join(up, "plugin", "extensions.yaml"))
		add(filepath.Join(up, "extensions.yaml"))
	}
	if cwd != "" {
		add(filepath.Join(cwd, "extensions.yaml"))
		add(filepath.Join(cwd, "plugin", "extensions.yaml"))
	}
	return out
}

// extensionCmdRoots 给出解析 `cmd` 相对路径时使用的基准目录，按优先级排列：
// 配置文件所在目录 → gear 可执行文件所在目录 → cwd。
//
// 首项是「配置即真相」：cmd 写 `plugin/serve-sidecar.exe` 就该相对配置文件解析，
// 这样 extensions.yaml 与 plugin/ 可以整体搬走而不改一个字。
func extensionCmdRoots(cfgPath string) []string {
	var out []string
	add := func(p string) {
		if p == "" {
			return
		}
		if abs, err := filepath.Abs(p); err == nil {
			p = abs
		}
		for _, e := range out {
			if e == p {
				return
			}
		}
		out = append(out, p)
	}
	if cfgPath != "" {
		add(filepath.Dir(cfgPath))
	}
	if exe, err := os.Executable(); err == nil {
		add(filepath.Dir(exe))
	}
	if cwd, err := os.Getwd(); err == nil {
		add(cwd)
	}
	return out
}

// resolveExtensionCmd 把 cmd[0] 的相对路径按 roots 解析成**绝对路径**。
//
// 只处理「带路径分隔符的相对路径」（如 `plugin/serve-sidecar.exe` / `./gear.exe`）：
//   - 绝对路径原样返回（尊重配置作者写死的意图）；
//   - 裸名字（`cat` / `node`）原样返回，交给 PATH 查找 —— 这是可执行文件的常规语义，不是文件路径；
//   - 所有 root 都落空时原样返回：宁可让运行期报「找不到命令」，也不猜一个别的路径。
//
// 它带来的直接收益：entry 里的命令不再随 cwd 变化。此前 `cd build && ./gear.exe` 能用、
// 换个 cwd 就整条链路失效（相对路径 + 缺 sh 两个缺陷叠加成「应用起不来」）。
func resolveExtensionCmd(cmd []string, roots []string) []string {
	if len(cmd) == 0 || cmd[0] == "" {
		return cmd
	}
	if filepath.IsAbs(cmd[0]) || !strings.ContainsAny(cmd[0], `/\`) {
		return cmd
	}
	for _, root := range roots {
		cand := filepath.Join(root, cmd[0])
		if !fileExists(cand) {
			continue
		}
		if abs, err := filepath.Abs(cand); err == nil {
			cand = abs
		}
		out := append([]string(nil), cmd...)
		out[0] = cand
		return out
	}
	return cmd
}

// fileExists 判定路径是否指向存在的普通文件（目录不算）。
func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}
