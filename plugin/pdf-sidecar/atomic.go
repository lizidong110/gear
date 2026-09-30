package main

import (
	"fmt"
	"os"
	"path/filepath"

	"gear/internal/sidecar/outpath"
)

// writeBytesAtomic 先把内容写进同目录的临时文件，再改名到 target。
//
// 为什么不直接 Create(target)：导出到一半失败会留下一个写了一半的 PDF，
// 而它看起来和好的一模一样（扩展名对、大小非 0）。改名是原子的——
// 要么看到旧文件，要么看到完整的新文件。
//
// 目录（root 内的部分）由 EnsureDir 现造：**落点已经过 outpath.Resolve 越界检查**，
// 所以这里的 MkdirAll 不会把监狱捅穿。
func writeBytesAtomic(target string, data []byte) (int64, error) {
	if err := outpath.EnsureDir(target); err != nil {
		return 0, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), ".gear-write-*")
	if err != nil {
		return 0, fmt.Errorf("建临时文件失败: %v", err)
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()

	n, err := tmp.Write(data)
	if err != nil {
		return 0, fmt.Errorf("写临时文件失败: %v", err)
	}
	if err := tmp.Close(); err != nil {
		return 0, fmt.Errorf("关闭临时文件失败: %v", err)
	}
	// Windows 的 os.Rename 不允许目标已存在（POSIX 是直接覆盖），故先删旧的。
	// 删到改名之间有一瞬间没有文件——那是「原子」的代价，比留半份文件强。
	if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
		return 0, fmt.Errorf("替换既有文件 %s 失败: %v", target, err)
	}
	if err := os.Rename(tmpName, target); err != nil {
		return 0, fmt.Errorf("落到 %s 失败: %v", target, err)
	}
	cleanup = false
	return int64(n), nil
}
