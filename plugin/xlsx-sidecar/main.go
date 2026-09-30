// Command xlsx-sidecar 是 ext.excel 的常驻侧车（service ext）：把行数据写成 .xlsx。
//
// 三条口径（PRD §4.5，写死的，别改成「更方便」）：
//  1. **零第三方依赖**：手写最小 OOXML（`archive/zip` + 手写 XML），go.mod / go.sum 一字不变
//     （AC-E4-4 可 `git diff` 机械验证）。不是炫技——加一个依赖就要牵动 go.sum 全量变动、
//     GOPROXY 离线即挂，而核心价值（能导出）跟用哪个库无关。
//  2. **只做写出**：读回（解析既有 xlsx）不在本批（PRD 已登记为副作用）。
//  3. **流式写出**（AC-E4-3）：sheet XML 直写进 zip.Writer，不在内存里拼整张表。
//     行数据随请求体一次性进来是**调用方**的成本；写出这一侧的额外内存是常量，与行数无关。
//
// 乱码不做猜测：单元格里出现 XML 1.0 不允许的字符（控制字符、孤代理项）一律报错并
// 指名它在第几个 sheet 第几行第几列——静默丢弃一个单元格，报表就会少一行钱却看不出来。
//
// 环境变量：COORD_ADDR（默认 127.0.0.1:8100），由引擎按 extensions.yaml 注入，
// 此处默认值仅供不经引擎的手工单跑兜底。
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"gear/internal/sidecar/coord"
	"gear/internal/sidecar/outpath"
)

// 错误码（错误响应的 `error` 字段形如 "<code>: <人话>"）。
//
// 带稳定 ASCII 标记是因为调用方（Lua / Flow）要靠它分支，而中文文案随时会改；
// 只给中文等于把分支逻辑焊死在一句措辞上。
const (
	errBadRequest  = "bad_request"
	errMissingPath = "missing_path"
	errNoSheets    = "no_sheets"
	errBadSheet    = "bad_sheet"
	errBadCell     = "bad_cell"
	errPathEscape  = "path_escape"
	errIOFailed    = "io_failed"
)

type service struct{}

func (s *service) Handlers() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"/write": s.handleWrite,
		"/caps":  s.handleCaps,
	}
}

func (s *service) handleCaps(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]interface{}{"result": map[string]interface{}{
		"methods": []string{"write"},
	}})
}

// ---- write ----

func (s *service) handleWrite(w http.ResponseWriter, r *http.Request) {
	args, err := decodeArgs(r)
	if err != nil {
		writeErr(w, errBadRequest, "请求体不是合法 JSON: %v", err)
		return
	}
	path := strArg(args, "path")
	if path == "" {
		writeErr(w, errMissingPath, "path 必填（相对 root 的文件名，如 reports/2026.xlsx）")
		return
	}
	specs, err := parseSheets(args["sheets"])
	if err != nil {
		writeErr(w, errBadSheet, "%s", err.Error())
		return
	}

	// root 缺省取**进程当前工作目录**：侧车由应用进程拉起，CWD 就是应用的 workdir
	// （layout.workdir），那正是「产物写到应用自己的目录里」的意思。
	root := strArg(args, "root")
	if root == "" {
		root, err = os.Getwd()
		if err != nil {
			writeErr(w, errIOFailed, "取不到进程工作目录: %v", err)
			return
		}
	}
	target, err := resolveTarget(root, path)
	if err != nil {
		writeErr(w, errPathEscape, "%s", err.Error())
		return
	}

	rows := 0
	for _, sp := range specs {
		rows += len(sp.rows)
	}
	size, err := writeFileAtomic(target, func(w io.Writer) error { return writeXLSX(w, specs) })
	if err != nil {
		writeErr(w, errIOFailed, "写 %s 失败: %v", target, err)
		return
	}
	writeJSON(w, map[string]interface{}{"result": map[string]interface{}{
		"path":   target,
		"size":   size,
		"sheets": len(specs),
		"rows":   rows,
	}})
}

// ---- root / path 解析 ----
//
// 越界与符号链接的判定全部落在 internal/sidecar/outpath 里：**不在此处重写一遍**。
// 落点安全这种东西，两份实现只会各自漂移成两种口径，而调用方看不出区别。
func resolveTarget(root, path string) (string, error) {
	t, err := outpath.Resolve(root, path)
	if err != nil {
		return "", err
	}
	// 末级若是符号链接一律拒绝：哪怕它此刻仍指向 root 内，它的指向随时能被换掉，
	// 而「这次写进 root 里」保证不了「下一次也写进 root 里」。
	if err := outpath.RejectFinalSymlink(t); err != nil {
		return "", err
	}
	return t, nil
}

// ---- 入参解析 ----

// sheetSpec 是解析后的一个工作表。
type sheetSpec struct {
	name string
	rows [][]cellVal
}

// cellVal 是一个单元格。kind 取 's' / 'n' / 'b' / 'e'（空）。
type cellVal struct {
	kind byte
	s    string
	n    float64
	b    bool
}

// parseSheets 把入参的 sheets 解析成 sheetSpec 列表。
//
// 形状问题是**调用最可能出错的地方**（Lua 里嵌套表常常少一层），所以报错一律带上
// 「第几个 sheet、第几行、第几列」——只说「sheets 不合法」的话，调用方得自己二分排除。
func parseSheets(v interface{}) ([]sheetSpec, error) {
	list, ok := v.([]interface{})
	if !ok || len(list) == 0 {
		return nil, fmt.Errorf("sheets 必填，且是非空数组：[{name=\"明细\", rows={{...},{...}}}]")
	}
	out := make([]sheetSpec, 0, len(list))
	seen := make(map[string]int, len(list))
	for i, raw := range list {
		m, ok := raw.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("sheets[%d] 不是对象（缺 name / rows）", i)
		}
		name := strArg(m, "name")
		if err := validateSheetName(name); err != nil {
			return nil, fmt.Errorf("sheets[%d]: %v", i, err)
		}
		// Excel 的工作表名不区分大小写，同名的会被它自己判重复。
		key := strings.ToLower(name)
		if j, dup := seen[key]; dup {
			return nil, fmt.Errorf("sheets[%d] 的工作表名 %q 与 sheets[%d] 重复（Excel 不区分大小写）", i, name, j)
		}
		seen[key] = i

		rowsRaw, ok := m["rows"].([]interface{})
		if !ok {
			return nil, fmt.Errorf("sheets[%d]（%s）的 rows 不是数组", i, name)
		}
		rows := make([][]cellVal, 0, len(rowsRaw))
		for r, rowRaw := range rowsRaw {
			cells, ok := rowRaw.([]interface{})
			if !ok {
				return nil, fmt.Errorf("sheets[%d]（%s）第 %d 行不是数组", i, name, r+1)
			}
			row := make([]cellVal, len(cells))
			for c, cv := range cells {
				v, err := parseCell(cv)
				if err != nil {
					return nil, fmt.Errorf("sheets[%d]（%s）第 %d 行第 %d 列: %v", i, name, r+1, c+1, err)
				}
				row[c] = v
			}
			rows = append(rows, row)
		}
		out = append(out, sheetSpec{name: name, rows: rows})
	}
	return out, nil
}

// validateSheetName 拦 Excel 自己也会拒绝的工作表名。
func validateSheetName(name string) error {
	switch {
	case strings.TrimSpace(name) == "":
		return fmt.Errorf("工作表名必填")
	case len([]rune(name)) > 31:
		return fmt.Errorf("工作表名 %q 超过 31 个字符（Excel 上限）", name)
	case strings.ContainsAny(name, "[]:*?/\\"):
		return fmt.Errorf("工作表名 %q 含 Excel 禁止的字符（[ ] : * ? / \\）", name)
	case strings.TrimSpace(name) != name:
		return fmt.Errorf("工作表名 %q 首尾有空白（Excel 会存成另一个名字）", name)
	}
	return nil
}

// parseCell 把 Lua 侧的一个标量变成单元格。
//
// 不支持的类型一律报错而不是「当字符串ToString」——把一张 Lua 表（json object）
// 悄悄写成 "[table: 0x...]" 会让报表带着垃圾值通过，这是最难发现的一类假成功。
func parseCell(v interface{}) (cellVal, error) {
	switch t := v.(type) {
	case nil:
		return cellVal{kind: 'e'}, nil
	case bool:
		return cellVal{kind: 'b', b: t}, nil
	case string:
		if err := validateXMLText(t); err != nil {
			return cellVal{}, err
		}
		return cellVal{kind: 's', s: t}, nil
	case float64:
		return cellVal{kind: 'n', n: t}, nil
	case json.Number:
		f, err := t.Float64()
		if err != nil {
			return cellVal{}, fmt.Errorf("数字解析失败 %q: %v", t.String(), err)
		}
		return cellVal{kind: 'n', n: f}, nil
	default:
		return cellVal{}, fmt.Errorf("不支持的单元格类型 %T（只接受字符串 / 数字 / 布尔 / nil）", v)
	}
}

// validateXMLText 拦 XML 1.0 不允许的字符。
//
// \t \n \r 是合法的（Excel 里也能正常显示换行），其余 C0 控制字符与 0x7F、孤代理项
// 会让整个 XML 变得无法解析——那时候报出来的将是「文件损坏」，而不是「哪个格子有问题」。
func validateXMLText(s string) error {
	for i, r := range s {
		switch {
		case r == '\t' || r == '\n' || r == '\r':
		case r < 0x20 || r == 0x7F:
			return fmt.Errorf("含 XML 不允许的控制字符 U+%04X（第 %d 个字符处）", r, i+1)
		case r == 0xFFFE || r == 0xFFFF:
			return fmt.Errorf("含 XML 不允许的字符 U+%04X", r)
		}
	}
	return nil
}

// 刻意**不查**孤代理项：入参走 encoding/json 解出来，它会把不配对的 \udXXX 换成 U+FFFD，
// 所以到这里的一定是合法 UTF-8。写一个打不到的分支只会让口径看起来比实际更严。

// ---- 通用辅助 ----

func decodeArgs(r *http.Request) (map[string]interface{}, error) {
	var req struct {
		Args map[string]interface{} `json:"args"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return nil, err
	}
	return req.Args, nil
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code, format string, a ...interface{}) {
	writeJSON(w, map[string]interface{}{"error": code + ": " + fmt.Sprintf(format, a...)})
}

// strArg 取字符串参数并 Trim（" " 这种填了等于没填）；类型不对也算没传。
func strArg(m map[string]interface{}, k string) string {
	if v, ok := m[k].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

func main() {
	addr := os.Getenv("COORD_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8100"
	}
	svc := &service{}
	c := coord.New(coord.Options{
		CheckInterval: 5 * time.Second,
		TTL:           15 * time.Second,
		OnEmpty: func() {
			log.Printf("占用列表为空，xlsx-sidecar 自动退出")
			os.Exit(0)
		},
	})
	log.Printf("xlsx-sidecar 协调监听 %s", addr)
	if err := c.Serve(addr, svc.Handlers()); err != nil {
		if coord.IsAddrInUse(err) {
			log.Printf("端口 %s 已被占用，xlsx-sidecar 退让退出", addr)
			os.Exit(coord.ExitAddrInUse)
		}
		log.Fatalf("xlsx-sidecar 异常退出: %v", err)
	}
}
