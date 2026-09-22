package cmdline

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// Shell 描述一个可用于 `-c` 执行命令行的 shell。
type Shell struct {
	// Path 是 shell 可执行文件的路径（能找到时给绝对路径）。
	Path string
	// Dir 是其所在目录。用途有二：把 `cat` / `echo` / `sleep` 这类 POSIX 工具
	// 作为 argv[0] 查找时的补充目录；以及在必要时把它前置进子进程 PATH。
	Dir string
	// Args 是命令行本体之前的前缀参数（POSIX shell 为 ["-c"]）。
	Args []string
}

// ErrNoShell 表示这行命令必须由 shell 解释（含未加引号的元字符），但本机找不到可用的
// POSIX shell。它是 fail-closed 的落点：绝不静默换成另一种解释器（那会让
// `echo '{"v":1}'` 这类行在 cmd.exe 下变成另一种语义）。
var ErrNoShell = errors.New("未找到可用的 POSIX shell（sh）")

var (
	shellOnce sync.Once
	shellVal  *Shell
	shellErr  error
)

// FindShell 返回本机可用于 `-c` 的 POSIX shell，找不到时返回错误（原因见 ErrNoShell）。
//
// 进程内只解析一次（结果缓存）：shell 的位置在一次运行里不会变，反复 stat 无意义。
// 需要临时改用的场景走 GEAR_SHELL 环境变量（显式逃生门，只在首次生效）。
func FindShell() (*Shell, error) {
	shellOnce.Do(func() {
		shellVal, shellErr = resolveShell(os.Getenv("GEAR_SHELL"), exec.LookPath, shellCandidates())
	})
	return shellVal, shellErr
}

// resolveShell 是 FindShell 的可测内核：lookup 与候选目录都由调用方注入。
//
// 查找顺序（先精确后启发）：
//  1. GEAR_SHELL 显式指定（尊重调用方意志，不做存在性校验之外的加工）；
//  2. PATH 里的 `sh`（Git Bash / Linux / macOS 的常规形态）；
//  3. **由 git 反推**：`<gitRoot>/cmd/git.exe` ⇒ `<gitRoot>/usr/bin/sh.exe`。
//     这条覆盖了「装过 Git for Windows 但 usr\bin 不在 PATH」这一最常见的缺 sh 场景，
//     且不写死安装路径（Git for Windows 与 PortableGit 都适用）。
//  4. 平台已知安装位置的兜底清单。
func resolveShell(envShell string, lookup func(string) (string, error), candidates []string) (*Shell, error) {
	if envShell != "" {
		if !fileExists(envShell) {
			return nil, fmt.Errorf("%w：GEAR_SHELL=%q 指向的文件不存在", ErrNoShell, envShell)
		}
		return newShell(envShell), nil
	}
	if p, err := lookup("sh"); err == nil {
		return newShell(p), nil
	}
	if p := gitDerivedShell(lookup); p != "" {
		return newShell(p), nil
	}
	for _, c := range candidates {
		if fileExists(c) {
			return newShell(c), nil
		}
	}
	return nil, fmt.Errorf("%w：命令行含 shell 元字符（管道 / 重定向 / && / 通配 / 变量展开等），"+
		"这类语义无法用参数数组表达。两条出路：(1) 把命令改写成不依赖 shell 特性的形式"+
		"（环境变量前缀 `NAME=value cmd` 与引号都由 gear 原生支持）；"+
		"(2) 装 Git for Windows 或用 GEAR_SHELL 指定一个 sh 的绝对路径", ErrNoShell)
}

// newShell 由解析到的路径补出 Shell（含所在目录，供 PATH 补充与 argv[0] 兜底查找）。
func newShell(p string) *Shell {
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = p
	}
	return &Shell{Path: abs, Dir: filepath.Dir(abs), Args: []string{"-c"}}
}

// gitDerivedShell 由 `git` 的位置反推 Git 自带的 sh ——
// `<gitRoot>/cmd/git.exe` 与 `<gitRoot>/bin/git.exe` 都对应 `<gitRoot>/usr/bin/sh.exe`。
// 仅 Windows 需要这条（类 Unix 的 sh 一定在 PATH 里，第 2 步就命中了）。
func gitDerivedShell(lookup func(string) (string, error)) string {
	if runtime.GOOS != "windows" {
		return ""
	}
	p, err := lookup("git")
	if err != nil {
		return ""
	}
	root := filepath.Dir(filepath.Dir(p))
	for _, rel := range []string{
		filepath.Join("usr", "bin", "sh.exe"),
		filepath.Join("usr", "bin", "sh"),
	} {
		cand := filepath.Join(root, rel)
		if fileExists(cand) {
			return cand
		}
	}
	return ""
}

// shellCandidates 返回平台已知的 sh 落点（最后兜底，仅在 PATH 与 git 反推都失败时用）。
func shellCandidates() []string {
	if runtime.GOOS != "windows" {
		return []string{"/bin/sh", "/usr/bin/sh"}
	}
	var out []string
	for _, env := range []string{"ProgramFiles", "ProgramFiles(x86)"} {
		if v := os.Getenv(env); v != "" {
			out = append(out, filepath.Join(v, "Git", "usr", "bin", "sh.exe"))
		}
	}
	if v := os.Getenv("LOCALAPPDATA"); v != "" {
		out = append(out, filepath.Join(v, "Programs", "Git", "usr", "bin", "sh.exe"))
	}
	// 保底字面量：上面几个环境变量在精简环境里可能都不存在。
	out = append(out,
		`C:\Program Files\Git\usr\bin\sh.exe`,
		`C:\Program Files (x86)\Git\usr\bin\sh.exe`,
	)
	return out
}

// fileExists 判定路径是否指向一个存在的**普通文件**（目录不算）。
func fileExists(p string) bool {
	if p == "" {
		return false
	}
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// resetShellCache 清空 FindShell() 的进程级缓存。仅供测试使用
// （生产路径没有「运行期换 shell」的语义，缓存是有意为之）。
func resetShellCache() {
	shellOnce = sync.Once{}
	shellVal, shellErr = nil, nil
}

// augmentPath 在子进程 PATH 前补上 shell 所在目录，让 shell 兜底路径也能找到
// `cat` / `echo` / `sleep` 这类 POSIX 工具（它们随 Git 装在同一个 usr\bin 下）。
//
// 只在**安全**时补：
//   - 目录已在 PATH 里（子串命中）→ 不补；
//   - Windows 上 PATH 是 POSIX 风格（不含 `;`）→ 不补：那种形态只可能来自 MSYS 宿主，
//     此时 sh 本就是靠 PATH 找到的，硬塞一个 `C:\...` 进去反而会把整条 PATH 拆错。
func augmentPath(cur, dir string) string {
	if dir == "" || cur == "" {
		return cur
	}
	if strings.Contains(cur, dir) {
		return cur
	}
	sep := string(os.PathListSeparator)
	if runtime.GOOS == "windows" && !strings.Contains(cur, ";") {
		return cur
	}
	if strings.HasPrefix(cur, dir+sep) {
		return cur
	}
	return dir + sep + cur
}
