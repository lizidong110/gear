package main

import (
	"archive/zip"
	"bufio"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ---- 产物落盘 ----

// writeFileAtomic 先把内容写进同目录的临时文件，再改名到 target。
//
// 之所以绕这一步：导出报表时进程被中断（超时、下游报错）会留下一个**写了一半的 .xlsx**，
// 而它看起来和好的一模一样（扩展名对、大小非 0）——业务把它递出去才会发现打不开。
// 改名是原子的：要么看到旧文件，要么看到完整的新文件。
func writeFileAtomic(target string, fn func(io.Writer) error) (int64, error) {
	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, fmt.Errorf("创建目录 %s 失败: %v", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".gear-write-*")
	if err != nil {
		return 0, fmt.Errorf("在 %s 下建临时文件失败: %v", dir, err)
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()

	bw := bufio.NewWriterSize(tmp, 64*1024)
	cw := &countWriter{w: bw}
	if err := fn(cw); err != nil {
		return 0, err
	}
	if err := bw.Flush(); err != nil {
		return 0, fmt.Errorf("刷盘失败: %v", err)
	}
	if err := tmp.Close(); err != nil {
		return 0, fmt.Errorf("关闭 %s 失败: %v", tmpName, err)
	}
	// Windows 的 os.Rename 不允许目标存在（不像 POSIX 直接覆盖），所以先删旧的。
	// 删掉到改名之间有一瞬间没有文件——那正是「原子」的代价，比半份文件强。
	if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
		return 0, fmt.Errorf("替换既有文件 %s 失败: %v", target, err)
	}
	if err := os.Rename(tmpName, target); err != nil {
		return 0, fmt.Errorf("落到 %s 失败: %v", target, err)
	}
	cleanup = false
	return cw.n, nil
}

type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// ---- xlsx 写出（最小 OOXML）----

// writeXLSX 把 sheets 写成 xlsx 字节流。
//
// 全程**流式**：每个 sheet 直接写进 zip 的一个 entry，中间不在内存里堆 XML 字符串
// （AC-E4-3 的万行不爆内存就靠这一条）。
func writeXLSX(w io.Writer, specs []sheetSpec) error {
	zw := zip.NewWriter(w)
	names := make([]string, len(specs))
	for i, sp := range specs {
		names[i] = sp.name
	}
	for _, p := range staticParts(names) {
		if err := writePart(zw, p.name, p.body); err != nil {
			return err
		}
	}
	for i, sp := range specs {
		f, err := zw.Create(fmt.Sprintf("xl/worksheets/sheet%d.xml", i+1))
		if err != nil {
			return fmt.Errorf("建 sheet%d 失败: %v", i+1, err)
		}
		bw := bufio.NewWriterSize(f, 64*1024)
		if err := writeSheet(bw, sp); err != nil {
			return fmt.Errorf("写工作表「%s」失败: %v", sp.name, err)
		}
		if err := bw.Flush(); err != nil {
			return err
		}
	}
	if err := zw.Close(); err != nil {
		return fmt.Errorf("收尾 zip 失败: %v", err)
	}
	return nil
}

type part struct {
	name string
	body string
}

func writePart(zw *zip.Writer, name, body string) error {
	w, err := zw.Create(name)
	if err != nil {
		return fmt.Errorf("建 %s 失败: %v", name, err)
	}
	_, err = io.WriteString(w, body)
	return err
}

// staticParts 产出与行数无关的那几个部件（表名要写进 workbook 里，那是唯一跟数据有关的部分）。
func staticParts(names []string) []part {
	overrides := &strings.Builder{}
	rels := &strings.Builder{}
	sheets := &strings.Builder{}
	for i, name := range names {
		id := i + 1
		fmt.Fprintf(overrides, `<Override PartName="/xl/worksheets/sheet%d.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/>`, id)
		fmt.Fprintf(rels, `<Relationship Id="rId%d" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet%d.xml"/>`, id, id)
		// 表名同样要转义：它出现在 XML 属性里，`&` / `"` 未转义会让整份文件不可解析。
		fmt.Fprintf(sheets, `<sheet name="%s" sheetId="%d" r:id="rId%d"/>`, xmlEscaper.Replace(name), id, id)
	}
	return []part{
		{"[Content_Types].xml", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/><Default Extension="xml" ContentType="application/xml"/><Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/><Override PartName="/xl/styles.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.styles+xml"/>` + overrides.String() + `</Types>`},
		{"_rels/.rels", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rIdMain" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/></Relationships>`},
		{"xl/workbook.xml", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheets>` + sheets.String() + `</sheets></workbook>`},
		{"xl/_rels/workbook.xml.rels", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` + rels.String() + `<Relationship Id="rIdStyles" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/styles" Target="styles.xml"/></Relationships>`},
		{"xl/styles.xml", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<styleSheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><fonts count="1"><font><sz val="11"/><name val="Calibri"/></font></fonts><fills count="1"><fill><patternFill patternType="none"/></fill></fills><borders count="1"><border/></borders><cellStyleXfs count="1"><xf numFmtId="0" fontId="0" fillId="0" borderId="0"/></cellStyleXfs><cellXfs count="1"><xf numFmtId="0" fontId="0" fillId="0" borderId="0" xfId="0"/></cellXfs></styleSheet>`},
	}
}

// writeSheet 流式写出一个工作表的 XML。
//
// 字符串单元格用 inlineStr（`<is><t>`）而不是 sharedStrings：共享串要先把**全部**字符串
// 收齐才能写 relying 索引，那等于把整张表先在内存里攒一遍——正是要避开的。
func writeSheet(w *bufio.Writer, sp sheetSpec) error {
	esc := xmlEscaper.Replace
	if _, err := io.WriteString(w, `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>`); err != nil {
		return err
	}
	if _, err := io.WriteString(w, `<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData>`); err != nil {
		return err
	}
	for r, row := range sp.rows {
		if _, err := fmt.Fprintf(w, `<row r="%d">`, r+1); err != nil {
			return err
		}
		for c, v := range row {
			ref := colRef(c) + strconv.Itoa(r+1)
			switch v.kind {
			case 'e':
				// 空单元格整个不写：写了 `<c r=".."/>` 也算单元格，会让 openpyxl
				// 读出 None 而不是跳过——两者差别在「这行是不是空的」上。
				continue
			case 's':
				if v.s == "" {
					continue
				}
				// xml:space="preserve" 不能少：少了它，首尾空格会被读表的人挤掉
				// （" 合计 " 变成 "合计"，肉眼分不出）。
				if _, err := fmt.Fprintf(w, `<c r="%s" t="inlineStr"><is><t xml:space="preserve">%s</t></is></c>`,
					ref, esc(v.s)); err != nil {
					return err
				}
			case 'n':
				s, err := numCell(v.n)
				if err != nil {
					return fmt.Errorf("第 %d 行第 %d 列: %v", r+1, c+1, err)
				}
				if _, err := fmt.Fprintf(w, `<c r="%s"><v>%s</v></c>`, ref, s); err != nil {
					return err
				}
			case 'b':
				i := 0
				if v.b {
					i = 1
				}
				if _, err := fmt.Fprintf(w, `<c r="%s" t="b"><v>%d</v></c>`, ref, i); err != nil {
					return err
				}
			}
		}
		if _, err := io.WriteString(w, `</row>`); err != nil {
			return err
		}
	}
	_, err := io.WriteString(w, `</sheetData></worksheet>`)
	return err
}

// numCell 把数字写成 xlsx 能接受的十进制字面量。
//
// 用 'f' 而不是 %v：后者会对极大/极小的数字产出 `1e+21` 这样的科学计数法，
// 而 OOXML 的 `<v>` 不接受它，表会打不开。
func numCell(v float64) (string, error) {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return "", fmt.Errorf("数字是 %v（xlsx 只允许有限浮点数）", v)
	}
	return strconv.FormatFloat(v, 'f', -1, 64), nil
}

// xmlEscaper 转义 XML 里的五个特殊字符。
var xmlEscaper = strings.NewReplacer(
	"&", "&amp;",
	"<", "&lt;",
	">", "&gt;",
	`"`, "&quot;",
	"'", "&apos;",
)

// colRef 列序号转 Excel 列名（0 → A，26 → AA）。
func colRef(idx int) string {
	idx++ // Excel 的列从 1 开始
	s := ""
	for idx > 0 {
		i := (idx - 1) % 26
		s = string(rune('A'+i)) + s
		idx = (idx - 1) / 26
	}
	return s
}
