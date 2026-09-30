// Package outpath 是所有「把文件写到应用目录里」的侧车共用的落点解析 ——
// 路径囚禁的**唯一实现处**（root jail）。
//
// 为什么要有这个包：ext.excel / ext.pdf 两个侧车都要在同一套规则下写文件，
// 各拧一份就会漂移（一边补了符号链接检查，另一边没补，而两边的调用方看不出区别）。
// 规则一旦不一致，最坏的情况是「用某个参数组合能写到 root 之外」。
//
// 三条守卫（顺序即优先级）：
//  1. path 里不许出现 `..`、反斜杠、盘符。`..` 的语义由 OS 决定（可以跳出 root），
//     一旦放行，Join 之后的越界检查就只是在验证「我们怎么理解 ..」，而不是在圈定范围。
//  2. Join + Rel：目标必须仍在 root 之内（escape 检查）。
//  3. root → target 路径上任何一级若是符号链接，必须能解析且落点仍在 root 内；
//     解不出的一律拒（看不清它指哪儿，就当它指外面）。
//
// 关于 EvalSymlinks：**不要**无条件对 root 调用它。Windows 上 `AppData\Local`
// 这类交接点会让 EvalSymlinks 返回「找不到路径」，于是合法写入也被拒——
// 这条不是假设，是在本机实踩过的（2026-09-29）。
package outpath

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// EscapeError 表示路径越界或被判为不安全。
type EscapeError struct {
	msg string
}

func (e *EscapeError) Error() string { return e.msg }

func escapef(format string, a ...interface{}) error {
	return &EscapeError{msg: fmt.Sprintf(format, a...)}
}

// Resolve 把相对 path 钉进 root 里，返回绝对路径。
//
// root 必须是已存在的目录；缺任何一条都返回 *EscapeError，调用方可以直接把它
// 拼进 RPC 的 error 字段（消息本身已经是给调用方看的人话）。
func Resolve(root, path string) (string, error) {
	fi, err := os.Stat(root)
	if err != nil {
		return "", escapef("root 不可用（%s）: %v", root, err)
	}
	if !fi.IsDir() {
		return "", escapef("root 不是目录: %s", root)
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", escapef("root 转绝对路径失败（%s）: %v", root, err)
	}
	if path == "" || strings.ContainsAny(path, `\:`) || strings.Contains(path, "..") {
		return "", escapef("path %q 不合法：必须是 root 内的相对路径（不允许 .. / 盘符 / 反斜杠）", path)
	}
	t := filepath.Join(rootAbs, path)
	if rel, err := filepath.Rel(rootAbs, t); err != nil || Outside(rel) {
		return "", escapef("path 越出 root: %s", path)
	}
	if err := RejectSymlinkEscape(rootAbs, t); err != nil {
		return "", err
	}
	return t, nil
}

// Outside 判断 filepath.Rel 的结果有没有爬到 root 之上。
func Outside(rel string) bool {
	return rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// RejectSymlinkEscape 逐段确认 root → target 上没有「跳出去的符号链接」，
// 顺带揪出「中间一层其实是文件」这种注定失败的路径。
//
// 只在某段**确实是链接**时才解析落点；解不出来按指向 root 之外处理（fail-closed）。
func RejectSymlinkEscape(root, target string) error {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return escapef("路径非法: %v", err)
	}
	parts := strings.Split(rel, string(filepath.Separator))
	cur := root
	for i, p := range parts {
		if p == "" || p == "." || p == ".." {
			continue
		}
		cur = filepath.Join(cur, p)
		fi, err := os.Lstat(cur)
		if err != nil {
			continue // 还不存在 ⇒ 由调用方 MkdirAll 现造，造出来就不会是链接
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			real, err := filepath.EvalSymlinks(cur)
			if err != nil {
				return escapef("%s 是符号链接但解不出落点（按指向 root 之外处理）: %v", cur, err)
			}
			if rr, err := filepath.Rel(root, real); err != nil || Outside(rr) {
				return escapef("%s 是符号链接且指向 root 之外（%s）：拒绝跟随", cur, real)
			}
			continue
		}
		// 中间一层是文件 ⇒ 目标根本放不进去。在这里说清楚，比让 MkdirAll 吐一句
		// 「file exists」强：后者看不出**哪一层的什么**是文件。
		if !fi.IsDir() && i != len(parts)-1 {
			return escapef("路径里的 %s 是文件不是目录，%s 写不进去", cur, target)
		}
	}
	return nil
}

// RejectFinalSymlink 补上「末级是符号链接」这一条。
//
// 它单独存在是因为不同侧车的态度可以不同：即便链接此刻指向 root 之内也不许写
// ——链接的指向随时能被换掉，而「这次写进去了」保证不了「下次还写进去」。
func RejectFinalSymlink(target string) error {
	if st, err := os.Lstat(target); err == nil && st.Mode()&os.ModeSymlink != 0 {
		return escapef("目标 %s 是符号链接：不许借符号链接写到别处", target)
	}
	return nil
}

// EnsureDir 造出 target 的父目录（Resolve 已确保它在 root 内）。
func EnsureDir(target string) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return escapef("创建目录 %s 失败: %v", filepath.Dir(target), err)
	}
	return nil
}
