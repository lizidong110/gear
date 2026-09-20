package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// ---- 统一输入：Unix 管道直觉（cat x.json | gear xxx） ----
//
// 「输入类」命令（tool run / flow run）的 JSON 入参支持四种来源，规则如下：
//  1. 给了显式标志（--input / --input-file / --arg|--var）→ 用标志，**不再读 stdin**
//     （避免管道被意外消费；标志是显式意图，优先级最高）；
//  2. `--input -` / `--input-file -` → **显式**声明从 stdin 读；
//  3. 未给任何标志、且 stdin 是管道/重定向 → **自动**读 stdin 当 JSON
//     （`cat vars.json | gear flow run demo`）
//  4. 都没有 → 空输入（是否合法由 input_schema 说了算，不在这里放宽）
//
// 显式标志之间**互斥**（同时给多个即报错，避免「以谁为准」的歧义）。
//
// 「文件类」命令（register / validate）支持把路径写成 `-` 从 stdin 读 YAML
// （`cat demo-db.yaml | gear flow register -`），同为 Unix 管道直觉。

// inputSpec 描述一次 JSON 入参解析的来源与标志名（标志名只用于报错信息）。
type inputSpec struct {
	jsonFlag string // 如 "--input"
	fileFlag string // 如 "--input-file"
	kvFlag   string // 如 "--arg" / "--var"
	jsonVal  string
	fileVal  string
	kv       []string
}

// resolveInput 按上述规则解析出最终输入 map。
func resolveInput(s inputSpec) (map[string]interface{}, error) {
	given := 0
	if s.jsonVal != "" {
		given++
	}
	if s.fileVal != "" {
		given++
	}
	if len(s.kv) > 0 {
		given++
	}
	if given > 1 {
		return nil, fmt.Errorf("%s / %s / %s 三选一，不要混用", s.jsonFlag, s.fileFlag, s.kvFlag)
	}

	// 「标志优先、管道被忽略」时提示一句（不静默）——否则用户以为管道生效了。
	if given > 0 && s.jsonVal != "-" && s.fileVal != "-" && stdinIsPiped() {
		fmt.Fprintf(os.Stderr, "⚠ 检测到 stdin 有管道输入，但已给显式输入标志（%s / %s / %s 之一），管道内容被忽略\n",
			s.jsonFlag, s.fileFlag, s.kvFlag)
	}

	switch {
	case s.jsonVal == "-" || s.fileVal == "-":
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return nil, fmt.Errorf("读取 stdin 失败: %w", err)
		}
		return decodeJSONObject(string(b), "stdin")

	case s.fileVal != "":
		b, err := readFileOrStdin(s.fileVal)
		if err != nil {
			return nil, err
		}
		return decodeJSONObject(string(b), s.fileVal)

	case s.jsonVal != "":
		return decodeJSONObject(s.jsonVal, s.jsonFlag)

	case len(s.kv) > 0:
		return parseKVFlags(s.kvFlag, s.kv)
	}

	// 无显式标志：stdin 是管道就自动读（Unix 直觉），否则空输入。
	if raw, ok := readPipedStdin(); ok {
		return decodeJSONObject(raw, "stdin")
	}
	return map[string]interface{}{}, nil
}

// readFileOrStdin 读文件内容；path 为 "-" 时从 stdin 读（供 register / validate 用）。
func readFileOrStdin(path string) ([]byte, error) {
	if path == "-" {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return nil, fmt.Errorf("读取 stdin 失败: %w", err)
		}
		return b, nil
	}
	return os.ReadFile(path)
}

// stdinIsPiped 报告 stdin 是否为管道/重定向（非终端）。**只做 Stat，不消费内容**，
// 故可用于「管线已接上但被忽略」的提示。
func stdinIsPiped() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice == 0 // 字符设备 = 终端，没有管道
}

// readPipedStdin 在 stdin 是管道/重定向时读出全部内容；终端下返回 ("", false)，
// **绝不阻塞等待输入**。
func readPipedStdin() (string, bool) {
	if !stdinIsPiped() {
		return "", false
	}
	b, err := io.ReadAll(os.Stdin)
	if err != nil {
		return "", false
	}
	return string(b), true
}

// decodeJSONObject 把原始文本解析成 JSON 对象（顶层必须是 object）；
// 空文本视为空对象（方便 `: | gear ...` 或空管道）。
func decodeJSONObject(raw, src string) (map[string]interface{}, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return map[string]interface{}{}, nil
	}
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(trimmed), &m); err != nil {
		return nil, fmt.Errorf("%s 不是合法 JSON 对象: %w", src, err)
	}
	return m, nil
}
