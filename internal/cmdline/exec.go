package cmdline

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"gear/internal/gearhome"
)

// Command 把一行命令翻译成可直接 Start/Run 的 *exec.Cmd，**默认不经 shell**。
//
// 两条路径：
//  1. 原生（默认）：Parse 切词后直接 exec，`NAME=value` 前缀落到 cmd.Env。
//     这是 gear 自身全部 entry 的走法，也是「Windows 系统 PATH 里没有 sh」的解药。
//  2. shell 兜底：仅当命令行含未加引号的 shell 元字符时启用。找不到 shell 则返回
//     包装了 ErrNoShell 的错误（fail-closed，不换成别的解释器）。
//
// 调用方仍需自行设置 Stdin/Stdout/Stderr —— 本函数**不**碰它们，
// 以免无意间把调用方的敏感 fd 交给子进程（GEAR_工程方法 红线 16）。
func Command(ctx context.Context, line string) (*exec.Cmd, error) {
	spec, err := Parse(line)
	if err != nil {
		return nil, err
	}
	sh, shErr := FindShell() // 找不到不影响原生路径

	if spec.NeedsShell {
		if shErr != nil {
			return nil, fmt.Errorf("命令 %q 含 shell 元字符 %s：%w",
				line, strings.Join(spec.Meta, " "), shErr)
		}
		cmd := exec.CommandContext(ctx, sh.Path, append(append([]string{}, sh.Args...), line)...)
		cmd.Env = childEnv(nil, sh)
		return cmd, nil
	}

	if len(spec.Argv) == 0 {
		return nil, fmt.Errorf("命令行为空: %q", line)
	}
	path, err := LookExecutable(spec.Argv[0], sh)
	if err != nil {
		return nil, fmt.Errorf("命令 %q 的可执行文件 %q 不可用: %w", line, spec.Argv[0], err)
	}
	cmd := exec.CommandContext(ctx, path, spec.Argv[1:]...)
	cmd.Env = childEnv(spec.Env, sh)
	return cmd, nil
}

// LookExecutable 解析 argv[0]：
//   - 含路径分隔符 → 原样返回，交给 os/exec 的平台语义（Windows 会按 PATHEXT 补扩展名）；
//   - 裸名字 → 依次在「gear 可执行文件所在目录」「进程 PATH」「shell 所在目录」里找。
//
// 裸名的解析顺序是 2026-09-25 定的：**exe 同目录优先于 PATH**。
// 理由是应用的 process entry 写 `gear serve --addr ...` 时，要起的是**这一个** gear，
// 而不是 PATH 里可能存在的另一个版本；这条也是应用定义能跨机器可移植的前提——
// 此前 entry 只能写成 `D:/gadern/gear/build/gear.exe`，换台机器即废。
// PATH 与 shell 同目录两条保留给 `cat` / `echo` / `sleep` 这类随 Git 安装的 POSIX 工具
// （它们的 usr\bin 常常不在 PATH 里）。
func LookExecutable(name string, sh *Shell) (string, error) {
	if name == "" {
		return "", errors.New("可执行文件名为空")
	}
	if strings.ContainsAny(name, `/\`) {
		return name, nil
	}
	if d := gearhome.ExeDir(); d != "" {
		for _, cand := range exeCandidates(filepath.Join(d, name)) {
			if fileExists(cand) {
				return cand, nil
			}
		}
	}
	if p, err := exec.LookPath(name); err == nil {
		return p, nil
	}
	if sh != nil && sh.Dir != "" {
		for _, cand := range exeCandidates(filepath.Join(sh.Dir, name)) {
			if fileExists(cand) {
				return cand, nil
			}
		}
	}
	return "", exec.ErrNotFound
}

// ResolveExecutable 与 LookExecutable 同序，但**对含路径分隔符的名字额外做存在性判断**。
//
// 为什么需要它：LookExecutable 对含分隔符的路径一律原样返回（把平台语义留给 os/exec），
// 这对「真要执行」是对的，但对「写 YAML 时提前校验」毫无用处——写错路径也照样通过。
// 静态校验（gear_validate）用它把「entry 指向的可执行文件不存在」提前到注册前发现。
//
// 返回的错误包装了 exec.ErrNotFound，调用方可用 errors.Is 判定。
func ResolveExecutable(name string, sh *Shell) (string, error) {
	if name == "" {
		return "", errors.New("可执行文件名为空")
	}
	if strings.ContainsAny(name, `/\`) {
		for _, cand := range exeCandidates(name) {
			if fileExists(cand) {
				return cand, nil
			}
		}
		return "", fmt.Errorf("可执行文件不存在: %s: %w", name, exec.ErrNotFound)
	}
	return LookExecutable(name, sh)
}

// exeCandidates 在 Windows 上按 PATHEXT 展开候选（`cat` → `cat.exe` …），
// 其它平台原样返回一个候选。
func exeCandidates(base string) []string {
	if runtime.GOOS != "windows" {
		return []string{base}
	}
	out := []string{base}
	pathext := os.Getenv("PATHEXT")
	if pathext == "" {
		pathext = ".COM;.EXE;.BAT;.CMD"
	}
	for _, ext := range strings.Split(pathext, ";") {
		ext = strings.TrimSpace(ext)
		if ext == "" {
			continue
		}
		want := strings.ToLower(ext)
		if strings.HasSuffix(strings.ToLower(base), want) {
			continue
		}
		out = append(out, base+strings.ToLower(ext))
	}
	return out
}

// childEnv 组装子进程环境：继承本进程 + 行首 `NAME=value` 赋值 + 必要的 PATH 补充。
//
// 显式构造（而不是让 cmd.Env 保持 nil）是为了把「entry 里的赋值」与「PATH 补充」
// 这两件事都收在一处，避免调用方各自拼环境导致漂移。
func childEnv(extra []string, sh *Shell) []string {
	env := os.Environ()
	if sh != nil {
		if cur := os.Getenv("PATH"); augmentPath(cur, sh.Dir) != cur {
			env = setEnv(env, "PATH", augmentPath(cur, sh.Dir))
		}
	}
	return append(env, extra...)
}

// setEnv 就地替换 env 里的某个键（Windows 的环境变量名不区分大小写）。
func setEnv(env []string, key, val string) []string {
	prefix := key + "="
	for i, kv := range env {
		name := kv
		if idx := strings.IndexByte(kv, '='); idx >= 0 {
			name = kv[:idx]
		}
		if strings.EqualFold(name, key) {
			env[i] = prefix + val
			return env
		}
	}
	return append(env, prefix+val)
}
