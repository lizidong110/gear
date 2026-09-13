package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// procDumpDir 是 process runtime 失败时，子进程原始 stdout/stderr 的落盘目录（临时文件）。
//
// 背景（用户痛点）：runProcess 把子进程输出捕进内存 bytes.Buffer，失败时只回传末行/错误信息，
// 完整原始输出随调用结束丢弃；复现「解析 stdout 失败」这类问题只能重跑测试（最长 ~90s 闸门）。
// 这里把失败时的原始输出写到 os.TempDir()/gear-proc/ 下的临时文件，并在错误中附路径，
// 后续可直接看文件定位，无需重跑。仅失败分支触发，正常路径不产生任何文件。
func procDumpDir() string {
	d := filepath.Join(os.TempDir(), "gear-proc")
	_ = os.MkdirAll(d, 0o755)
	return d
}

// dumpProcOutput 把子进程原始 stdout/stderr 合并落盘，返回文件路径。
// 落盘失败返回空串（绝不掩盖原始错误）。
func dumpProcOutput(entry string, stdout, stderr []byte) string {
	ts := time.Now().Format("20060102-150405.000000")
	name := "proc-" + ts + ".log"
	p := filepath.Join(procDumpDir(), name)
	var b strings.Builder
	b.WriteString("# entry: ")
	b.WriteString(entry)
	b.WriteString("\n# ---- stdout ----\n")
	b.Write(stdout)
	b.WriteString("\n# ---- stderr ----\n")
	b.Write(stderr)
	if err := os.WriteFile(p, []byte(b.String()), 0o644); err != nil {
		return ""
	}
	return p
}

// procFail 构造带「原始输出落盘路径」的错误。base 为原始错误文案。
// 这样调用方在报错现场即可拿到可打开的临时文件路径，跳过重跑。
func procFail(base, entry string, stdout, stderr []byte) error {
	if p := dumpProcOutput(entry, stdout, stderr); p != "" {
		return fmt.Errorf("%s [原始输出已落盘: %s]", base, p)
	}
	return fmt.Errorf("%s", base)
}
