package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---- 读回辅助（用标准库自读，不依赖任何第三方 xlsx 库）----

type readCell struct {
	ref  string
	kind string // ""=数字 / "inlineStr" / "b"
	text string // 文本或 <v> 的值
}

// readSheetXML 打开 zip 里的一个工作表，把行读回来。
//
// 之所以自己读而不拼第二个库：xlsx 的交叉验证（openpyxl）在 plugin/verify.py 里做——
// 那才是「别人家的解析器」；这里的自读只负责证明「写出来的结构是我们想要的形状」。
func readSheetXML(t *testing.T, zf *zip.ReadCloser, name string) [][]readCell {
	t.Helper()
	f, err := zf.Open(name)
	if err != nil {
		t.Fatalf("打开 %s 失败: %v", name, err)
	}
	defer f.Close()
	var rows [][]readCell
	var cur []readCell
	var cell *readCell
	inText := false
	dec := xml.NewDecoder(f)
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("解析 %s 失败: %v", name, err)
		}
		switch e := tok.(type) {
		case xml.StartElement:
			switch e.Name.Local {
			case "row":
				cur = nil
			case "c":
				cell = &readCell{}
				for _, a := range e.Attr {
					switch a.Name.Local {
					case "r":
						cell.ref = a.Value
					case "t":
						cell.kind = a.Value
					}
				}
			case "t", "v":
				inText = true
			}
		case xml.EndElement:
			switch e.Name.Local {
			case "row":
				rows = append(rows, cur)
			case "c":
				if cell != nil {
					cur = append(cur, *cell)
					cell = nil
				}
			case "t", "v":
				inText = false
			}
		case xml.CharData:
			if inText && cell != nil {
				cell.text += string(e)
			}
		}
	}
	return rows
}

// writeTo 走 writeFileAtomic 写一份 xlsx 并返回路径与大小。
func writeTo(t *testing.T, dir, name string, specs []sheetSpec) (string, int64) {
	t.Helper()
	target := filepath.Join(dir, name)
	size, err := writeFileAtomic(target, func(w io.Writer) error { return writeXLSX(w, specs) })
	if err != nil {
		t.Fatalf("写 %s 失败: %v", target, err)
	}
	return target, size
}

func openZip(t *testing.T, path string) *zip.ReadCloser {
	t.Helper()
	zf, err := zip.OpenReader(path)
	if err != nil {
		t.Fatalf("打开 %s 失败: %v", path, err)
	}
	t.Cleanup(func() { zf.Close() })
	return zf
}

// partExists 断言 zip 里有某个部件。
func partExists(t *testing.T, zf *zip.ReadCloser, name string) bool {
	t.Helper()
	for _, f := range zf.File {
		if f.Name == name {
			return true
		}
	}
	return false
}

func readPart(t *testing.T, zf *zip.ReadCloser, name string) string {
	t.Helper()
	f, err := zf.Open(name)
	if err != nil {
		t.Fatalf("读 %s 失败: %v", name, err)
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("读 %s 内容失败: %v", name, err)
	}
	return string(b)
}

// ---- 用例 ----

func TestWriteXLSXPartsAreComplete(t *testing.T) {
	dir := t.TempDir()
	target, _ := writeTo(t, dir, "a.xlsx", []sheetSpec{
		{name: "明细", rows: [][]cellVal{{{kind: 's', s: "产品"}, {kind: 's', s: "数量"}}}},
	})
	zf := openZip(t, target)
	for _, want := range []string{
		"[Content_Types].xml", "_rels/.rels", "xl/workbook.xml",
		"xl/_rels/workbook.xml.rels", "xl/styles.xml", "xl/worksheets/sheet1.xml",
	} {
		if !partExists(t, zf, want) {
			t.Fatalf("缺部件 %s（确实缺它就是打不开的那类 xlsx）", want)
		}
	}
	if got := readPart(t, zf, "xl/workbook.xml"); !strings.Contains(got, `<sheet name="明细"`) {
		t.Fatalf("workbook.xml 里没有工作表名，读到的是：%s", got)
	}
	// 每个部件都必须是合法 XML：坏 XML 只会表现为「Excel 说文件已损坏」，
	// 定位成本极高，所以在这里拦下来。
	for _, f := range zf.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("打开 %s 失败: %v", f.Name, err)
		}
		dec := xml.NewDecoder(rc)
		for {
			if _, err := dec.Token(); err == io.EOF {
				break
			} else if err != nil {
				t.Fatalf("%s 不是合法 XML: %v", f.Name, err)
			}
		}
		rc.Close()
	}
}

func TestCellTypesRoundTrip(t *testing.T) {
	dir := t.TempDir()
	target, _ := writeTo(t, dir, "t.xlsx", []sheetSpec{{name: "S", rows: [][]cellVal{
		{{kind: 's', s: "品名"}, {kind: 'n', n: 12.5}, {kind: 'b', b: true}, {kind: 'b', b: false}, {kind: 'e'}, {kind: 'n', n: 0}},
		{{kind: 's', s: ""}, {kind: 'n', n: -3}},
	}}})
	rows := readSheetXML(t, openZip(t, target), "xl/worksheets/sheet1.xml")
	if len(rows) != 2 {
		t.Fatalf("行数应为 2，实际 %d", len(rows))
	}
	if got := rows[0]; len(got) != 5 {
		// 空单元格整格不写 ⇒ 第一行只剩 5 个格（第 6 列的 0 还在）
		t.Fatalf("第一行应跳过空单元格剩 5 格，实际 %d: %+v", len(got), got)
	}
	if rows[0][0].kind != "inlineStr" || rows[0][0].text != "品名" {
		t.Errorf("文本单元格不对: %+v", rows[0][0])
	}
	if rows[0][1].kind != "" || rows[0][1].text != "12.5" {
		t.Errorf("数字单元格不该带类型标记且值应为 12.5: %+v", rows[0][1])
	}
	if rows[0][2].kind != "b" || rows[0][2].text != "1" {
		t.Errorf("布尔 true 应为 t=b / <v>1</v>: %+v", rows[0][2])
	}
	if rows[0][2].ref != "C1" || rows[0][3].ref != "D1" {
		t.Errorf("单元格坐标不对（空单元格跳过后坐标仍要跟着列走）: %s %s", rows[0][2].ref, rows[0][3].ref)
	}
	if rows[0][4].ref != "F1" || rows[0][4].text != "0" {
		t.Errorf("第 6 列的 0 必须还在（ref=%s v=%s）", rows[0][4].ref, rows[0][4].text)
	}
	if len(rows[1]) != 1 {
		t.Errorf("空串应该像空单元格一样跳过: %+v", rows[1])
	}
}

func TestXMLEscaping(t *testing.T) {
	dir := t.TempDir()
	raw := `<金额> & "合计" '单价' 100% `
	target, _ := writeTo(t, dir, "esc.xlsx", []sheetSpec{{name: "S", rows: [][]cellVal{
		{{kind: 's', s: raw}, {kind: 's', s: "  合计  "}},
	}}})
	rows := readSheetXML(t, openZip(t, target), "xl/worksheets/sheet1.xml")
	if got := rows[0][0].text; got != raw {
		t.Errorf("特殊字符应原样回来，got %q want %q", got, raw)
	}
	if got := rows[0][1].text; got != "  合计  " {
		t.Errorf("首尾空格不该被挤掉（xml:space=preserve 得生效），got %q", got)
	}
}

func TestIllegalCharactersAreRejected(t *testing.T) {
	cases := []struct {
		name string
		s    string
		want string
	}{
		{"控制字符 NUL", "a\x00b", "U+0000"},
		{"控制字符 ESC", "a\x1bb", "U+001B"},
		{"控制字符 DEL", "a" + string(rune(0x7F)) + "b", "U+007F"},
		{"非字符 FFFE", "a" + string(rune(0xFFFE)) + "b", "U+FFFE"},
		// 孤代理项刻意不测：Go 的 encoding/json 会先把不配对的 \udXXX 换成 U+FFFD，
		// 入参到这一步时它已经不存在了（validateXMLText 里也没写那条分支）。
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateXMLText(c.s)
			if err == nil {
				t.Fatalf("应拒绝：%q", c.s)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("报错该点名字符 %s，实际：%v", c.want, err)
			}
		})
	}
	// \t \n \r 必须放行：它们是 XML 合法字符，Excel 里也都能正常显示
	if err := validateXMLText("a\tb\nc\r\nd"); err != nil {
		t.Fatalf("制表符/换行应该放行: %v", err)
	}
}

func TestSheetNameRules(t *testing.T) {
	cases := []struct {
		name  string
		input string
		bad   bool
	}{
		{"正常", "明细表", false},
		{"空", "", true},
		{"纯空格", "   ", true},
		{"超 31 字符", strings.Repeat("x", 32), true},
		{"含冒号", "to:day", true},
		{"含斜杠", "a/b", true},
		{"含方括号", "a[1]", true},
		{"含星号", "a*b", true},
		{"含问号", "a?b", true},
		{"含反斜杠", `a\b`, true},
		{"首尾空白", " 合计 ", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateSheetName(c.input)
			if c.bad && err == nil {
				t.Fatalf("%q 应被拒", c.input)
			}
			if !c.bad && err != nil {
				t.Fatalf("%q 不该被拒: %v", c.input, err)
			}
		})
	}
}

func TestParseSheetsNegativeCases(t *testing.T) {
	cases := []struct {
		name  string
		input interface{}
		want  string
	}{
		{"缺 sheets", nil, "sheets 必填"},
		{"不是数组", "x", "sheets 必填"},
		{"空数组", []interface{}{}, "sheets 必填"},
		{"元素不是对象", []interface{}{"x"}, "sheets[0] 不是对象"},
		{"缺 rows", []interface{}{map[string]interface{}{"name": "S"}}, "rows 不是数组"},
		{"rows 不是数组", []interface{}{map[string]interface{}{"name": "S", "rows": "x"}}, "rows 不是数组"},
		{"行不是数组", []interface{}{map[string]interface{}{"name": "S", "rows": []interface{}{"x"}}}, "第 1 行不是数组"},
		{"表名重复", []interface{}{
			map[string]interface{}{"name": "S", "rows": []interface{}{}},
			map[string]interface{}{"name": "s", "rows": []interface{}{}},
		}, "重复"},
		{"单元格类型不支持", []interface{}{map[string]interface{}{"name": "S", "rows": []interface{}{
			[]interface{}{map[string]interface{}{"a": 1}},
		}}}, "不支持的单元格类型"},
		{"工作表名非法", []interface{}{map[string]interface{}{"name": "a/b", "rows": []interface{}{}}}, "sheets[0]"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := parseSheets(c.input)
			if err == nil {
				t.Fatalf("应被拒：%v", c.input)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("报错应含 %q，实际 %v", c.want, err)
			}
		})
	}
}

func TestNumbersNeverBecomeScientificNotation(t *testing.T) {
	if got, err := numCell(1e21); err != nil || strings.ContainsAny(got, "eE") {
		t.Fatalf("1e21 不该写成科学计数法（那样 xlsx 会打不开）：%q err=%v", got, err)
	}
	if got, err := numCell(0.1); err != nil || got != "0.1" {
		t.Fatalf("0.1 应保持十进制: %q %v", got, err)
	}
	if _, err := numCell(math.NaN()); err == nil {
		t.Fatal("NaN 必须被拒")
	}
	if _, err := numCell(math.Inf(1)); err == nil {
		t.Fatal("Inf 必须被拒")
	}
}

func TestColRef(t *testing.T) {
	cases := map[int]string{0: "A", 1: "B", 25: "Z", 26: "AA", 27: "AB", 51: "AZ", 52: "BA", 701: "ZZ", 702: "AAA"}
	for idx, want := range cases {
		if got := colRef(idx); got != want {
			t.Errorf("colRef(%d)=%s want %s", idx, got, want)
		}
	}
}

func TestPathJail(t *testing.T) {
	root := t.TempDir()
	if _, err := resolveTarget(root, "a/b.xlsx"); err != nil {
		t.Fatalf("root 内的正常路径不该被拦: %v", err)
	}
	for _, bad := range []string{"../out.xlsx", "a/../../out.xlsx", "a\\b.xlsx", "C:/Windows/x.xlsx", ""} {
		if _, err := resolveTarget(root, bad); err == nil {
			t.Errorf("path %q 不该被放行", bad)
		}
	}
	if _, err := resolveTarget(filepath.Join(root, "不存在"), "a.xlsx"); err == nil {
		t.Error("root 不存在时应报错，而不是照 mkdir 一个把 root 移过去")
	}
}

func TestTargetIsSymlinkIsRejected(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("原文件内容"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "report.xlsx")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("本机不允许建符号链接（Windows 无特权常见）: %v", err)
	}
	// 沙箱里 os.Symlink 会「创建成功」但落地的并不是链接（Lstat 也说不出口），
	// 那这条用例就会变成假绿：先自证夹具真是个链接，做不了就明说跳过。
	fi, err := os.Lstat(link)
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Skipf("本机建的 %s 不是真符号链接（mode=%v），跳过穿透用例", link, fi.Mode())
	}
	if _, err := resolveTarget(root, "report.xlsx"); err == nil {
		t.Fatal("末级是符号链接时必须拒，否则导出会写到 root 之外")
	}
}

func TestWriteFileAtomicKeepsOldFileOnFailure(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "old.xlsx")
	if err := os.WriteFile(target, []byte("原内容"), 0o644); err != nil {
		t.Fatal(err)
	}
	want := fmt.Errorf("写一半炸了")
	if _, err := writeFileAtomic(target, func(w io.Writer) error {
		w.Write([]byte("半截"))
		return want
	}); err == nil || !strings.Contains(err.Error(), want.Error()) {
		t.Fatalf("应把错误原样带回，实际 %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "原内容" {
		t.Fatalf("失败时旧文件必须原封不动，实际 %q", got)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".gear-write-*")); len(left) != 0 {
		t.Fatalf("临时文件应清理干净，残留：%v", left)
	}
}

func TestWriteFileAtomicReplacesExisting(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "r.xlsx")
	if err := os.WriteFile(target, []byte("旧"), 0o644); err != nil {
		t.Fatal(err)
	}
	size, err := writeFileAtomic(target, func(w io.Writer) error {
		_, err := io.WriteString(w, "新内容更长一点")
		return err
	})
	if err != nil {
		t.Fatalf("Windows 上 os.Rename 不允许目标存在，这里必须处理好: %v", err)
	}
	if got, _ := os.ReadFile(target); string(got) != "新内容更长一点" {
		t.Fatalf("应被替换，实际 %q", got)
	}
	if size != int64(len("新内容更长一点")) {
		t.Fatalf("size 应为 %d，实际 %d", len("新内容更长一点"), size)
	}
}

func TestTenThousandRows(t *testing.T) {
	dir := t.TempDir()
	const n, cols = 10000, 10
	rows := make([][]cellVal, n)
	for i := range rows {
		row := make([]cellVal, cols)
		row[0] = cellVal{kind: 's', s: fmt.Sprintf("物料-%d", i)}
		for c := 1; c < cols; c++ {
			row[c] = cellVal{kind: 'n', n: float64(i*c) + 0.5}
		}
		rows[i] = row
	}
	start := time.Now()
	target, size := writeTo(t, dir, "big.xlsx", []sheetSpec{{name: "明细", rows: rows}})
	elapsed := time.Since(start)
	rowsBack := readSheetXML(t, openZip(t, target), "xl/worksheets/sheet1.xml")
	if len(rowsBack) != n {
		t.Fatalf("应写回 %d 行，实际 %d", n, len(rowsBack))
	}
	if len(rowsBack[n-1]) != cols {
		t.Fatalf("最后一行列数应为 %d，实际 %d", cols, len(rowsBack[n-1]))
	}
	if size == 0 {
		t.Fatal("size 不该为 0")
	}
	if elapsed > 20*time.Second {
		t.Fatalf("万行耗时 %v，超过 Tool 默认的 30s 就该工具侧声明超时了", elapsed)
	}
	t.Logf("万行 %dx%d：%v，产物 %d 字节", n, cols, elapsed, size)
}

// ---- handler 层 ----

func callWrite(t *testing.T, args map[string]interface{}) (map[string]interface{}, map[string]interface{}) {
	t.Helper()
	body, err := json.Marshal(map[string]interface{}{"args": args})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/write", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	(&service{}).handleWrite(rec, req)
	var out map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是 JSON: %s", rec.Body.String())
	}
	res, _ := out["result"].(map[string]interface{})
	errObj, _ := out["error"].(string)
	if errObj != "" {
		return nil, map[string]interface{}{"error": errObj}
	}
	return res, out
}

// failWriter 是每次写都失败的 Writer：用来走出「写到一半炸了」的那些分支
// （它们平时是打不到的，但每一条都对应一次真实的磁盘故障）。
type failWriter struct{}

func (failWriter) Write(p []byte) (int, error) {
	return 0, fmt.Errorf("磁盘满了（测试注入）")
}

func TestWriteErrorsPropagate(t *testing.T) {
	if err := writeXLSX(failWriter{}, []sheetSpec{{name: "S", rows: [][]cellVal{{{kind: 's', s: "x"}}}}}); err == nil {
		t.Fatal("写出失败必须报错，不许把半份文件当成功")
	}
	// 数字 NaN 走的是「写的过程中发现格子不对」这条路径
	if err := writeXLSX(io.Discard, []sheetSpec{{name: "S", rows: [][]cellVal{{{kind: 'n', n: math.NaN()}}}}}); err == nil {
		t.Fatal("NaN 单元格必须在写的时候就报错")
	}
	if _, err := writeFileAtomic(filepath.Join(t.TempDir(), "x.xlsx"), func(w io.Writer) error {
		return writeXLSX(w, []sheetSpec{{name: "S", rows: [][]cellVal{{{kind: 's', s: "x"}}}}})
	}); err != nil {
		t.Fatalf("正常路径不该报错: %v", err)
	}
}

func TestResolveTargetNegativeCases(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveTarget(file, "a.xlsx"); err == nil || !strings.Contains(err.Error(), "不是目录") {
		t.Fatalf("root 是文件时应报错并说明原因，实际 %v", err)
	}
	if _, err := resolveTarget(filepath.Join(dir, "missing"), "a.xlsx"); err == nil {
		t.Fatal("root 不存在时应报错，而不是照 mkdir 一个把 root 移过去")
	}
	// 中间一层其实是同名文件 ⇒ 目标放不进去，必须在这里报清楚
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveTarget(dir, "not-a-dir/a.xlsx"); err == nil {
		t.Fatal("路径中间一层其实是文件时必须报错（否则会写出一个永远读不回来的东西）")
	}
}

func TestParseCellTypes(t *testing.T) {
	cases := []struct {
		name string
		in   interface{}
		kind byte
	}{
		{"nil", nil, 'e'},
		{"bool", true, 'b'},
		{"字符串", "x", 's'},
		{"数字", float64(3), 'n'},
		{"json 数字", json.Number("4.5"), 'n'},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseCell(c.in)
			if err != nil || got.kind != c.kind {
				t.Fatalf("parseCell(%v) = %+v, %v", c.in, got, err)
			}
		})
	}
	if _, err := parseCell(json.Number("not-a-number")); err == nil {
		t.Fatal("坏 json.Number 必须报错")
	}
	// 控制字符是从 parseCell 这条路上来的（单元格值），必须连带 backdrop 点名位置
	if _, err := parseCell("a\x01b"); err == nil || !strings.Contains(err.Error(), "U+0001") {
		t.Fatalf("控制字符应被拒并指名字符，实际 %v", err)
	}
}

func TestHandlerWriteHappyPath(t *testing.T) {
	root := t.TempDir()
	res, _ := callWrite(t, map[string]interface{}{
		"path": "reports/2026.xlsx",
		"root": root,
		"sheets": []interface{}{
			map[string]interface{}{"name": "明细", "rows": []interface{}{
				[]interface{}{"产品", 1.5, true, nil},
			}},
		},
	})
	if res == nil {
		t.Fatalf("正常调用应成功")
	}
	if filepath.Ext(res["path"].(string)) != ".xlsx" {
		t.Fatalf("path 应是绝对里的文件名: %v", res["path"])
	}
	if !strings.HasPrefix(res["path"].(string), root) {
		t.Fatalf("产物必须落在 root 下，实际 %v", res["path"])
	}
	if st, err := os.Stat(res["path"].(string)); err != nil || st.Size() != int64(res["size"].(float64)) {
		t.Fatalf("返回的 size 与磁盘上的不一致: %v %v", err, res["size"])
	}
	if res["rows"].(float64) != 1 || res["sheets"].(float64) != 1 {
		t.Fatalf("统计不对: %v", res)
	}
}

func TestHandlerNegativeCases(t *testing.T) {
	root := t.TempDir()
	cases := []struct {
		name string
		args map[string]interface{}
		code string
	}{
		{"缺 path", map[string]interface{}{"sheets": []interface{}{}}, errMissingPath},
		{"缺 sheets", map[string]interface{}{"path": "a.xlsx", "root": root}, errBadSheet},
		{"越出 root", map[string]interface{}{"path": "../a.xlsx", "root": root,
			"sheets": []interface{}{map[string]interface{}{"name": "S", "rows": []interface{}{}}}}, errPathEscape},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := []byte("{不是 JSON")
			if len(c.args) > 0 {
				b, _ := json.Marshal(map[string]interface{}{"args": c.args})
				body = b
			}
			req := httptest.NewRequest(http.MethodPost, "/write", bytes.NewReader(body))
			rec := httptest.NewRecorder()
			(&service{}).handleWrite(rec, req)
			var out map[string]interface{}
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatalf("响应不是 JSON: %s", rec.Body.String())
			}
			e, _ := out["error"].(string)
			if e == "" {
				t.Fatalf("应失败，实际 %s", rec.Body.String())
			}
			if !strings.HasPrefix(e, c.code) {
				t.Fatalf("错误码应为 %s，实际 %s", c.code, e)
			}
		})
	}
}

func TestHandlerBadRequestBody(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/write", strings.NewReader("{ 坏 json"))
	rec := httptest.NewRecorder()
	(&service{}).handleWrite(rec, req)
	var out map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是 JSON: %s", rec.Body.String())
	}
	if e, _ := out["error"].(string); !strings.HasPrefix(e, errBadRequest) {
		t.Fatalf("应为 %s，实际 %q", errBadRequest, e)
	}
}

func TestCaps(t *testing.T) {
	rec := httptest.NewRecorder()
	(&service{}).handleCaps(rec, httptest.NewRequest(http.MethodGet, "/caps", nil))
	var out map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	res, _ := out["result"].(map[string]interface{})
	methods, _ := res["methods"].([]interface{})
	if len(methods) != 1 || methods[0] != "write" {
		t.Fatalf("/caps 应只声明 write，实际 %v", res)
	}
}
