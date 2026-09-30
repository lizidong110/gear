package main

import (
	"bytes"
	"compress/flate"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"math"
	"strings"
)

// ---- 页面几何 ----
//
// 全部是 A4 的固定值，不做参数化：可配的页面尺寸会立刻带来「同一份应用在两台机器上
// 导出不同版式」的问题，而报表通常是要**归档**的。
const (
	pageWidth  = 595.28 // A4 宽（pt）
	pageHeight = 841.89 // A4 高（pt）
	marginX    = 48.0
	marginTop  = 48.0
	marginBot  = 48.0
)

const (
	defaultFontSize = 12.0
	lineFactor      = 1.5 // 行距 = 字号 × 1.5
	minFontSize     = 4.0
	maxFontSize     = 72.0
)

// linesPerPage 计算一页放得下几行。
func linesPerPage(fontSize float64) int {
	n := 0
	for y := pageHeight - marginTop - fontSize; y >= marginBot; y -= fontSize * lineFactor {
		n++
	}
	if n == 0 {
		// 字号大到一行都放不下时别返回 0：那个数会被写进错误文案，
		// 「一页只放得下 0 行」读起来像算法坏了，而不是参数越界。
		return 1
	}
	return n
}

// pageOverflowErr 是「文本超出一页」：报错而不是悄悄再补一页。
//
// 为什么不多补几页：调用方传进来的是 `pages`（复数），它对自己会拿到几页有预期
// （比如要盖骑缝章、要写「第 N 页 / 共 M 页」）。偷偷多出来的页会让这些预期全错，
// 而且错得非常隐蔽——翻的人以为自己已经看完了全部。
type pageOverflowErr struct {
	page     int
	need     int
	capacity int
	size     float64
}

func (e *pageOverflowErr) Error() string {
	return fmt.Sprintf("第 %d 页的文本需要 %d 行，但这一页只放得下 %d 行"+
		"（A4 / %.0fpt / 上下边距各 %.0fpt）：请把它拆成多个 page，或调小 font_size",
		e.page, e.need, e.capacity, e.size, marginTop)
}

// ---- 排版 ----

// wrap 把一页正文排成若干显示行。
func wrap(text string, plan *subsetPlan, fontSize float64) []string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	// Tab 不是可印字符（字体没有它的字形），一概按 4 个空格排版：这是唯一不丢信息的渲染方式。
	text = strings.ReplaceAll(text, "\t", "    ")
	maxWidth := pageWidth - 2*marginX

	var out []string
	for _, para := range strings.Split(text, "\n") {
		out = append(out, wrapPara(para, plan, fontSize, maxWidth)...)
	}
	return out
}

// wrapPara 对一个不含换行的段落做贪心折行。
//
// 断点规则刻意简单：① 空格之后可以断；② 两个中日韩字符之间可以断——
// 中文文段里没有空格，不许在字间断行的话，一行要放不下就永远放不下。
func wrapPara(s string, plan *subsetPlan, fontSize, maxWidth float64) []string {
	rs := []rune(s)
	if len(rs) == 0 {
		return []string{""}
	}
	var lines []string
	for i := 0; i < len(rs); {
		width := 0.0
		j := i
		lastBreak := -1
		for j < len(rs) {
			cw := advanceOf(plan, rs[j], fontSize)
			if width+cw > maxWidth && j > i {
				if lastBreak > i {
					j = lastBreak // 退到最近的断点：宁可短一点也不把一个词劈开
				}
				break
			}
			width += cw
			j++
			if j < len(rs) && canBreakBetween(rs[j-1], rs[j]) {
				lastBreak = j
			}
		}
		if j == i {
			j = i + 1 // 单个字符就超宽：硬着头皮放行，否则这里会把自己卡死
		}
		lines = append(lines, strings.TrimRight(string(rs[i:j]), " "))
		i = j
	}
	return lines
}

func canBreakBetween(prev, next rune) bool {
	if prev == ' ' || prev == 0x3000 { // 空格 / 表意空格之后
		return true
	}
	return isCJK(prev) || isCJK(next)
}

// isCJK 粗判一个字符是否属于「没有空格也会按字断行」的文字系统。
func isCJK(r rune) bool {
	switch {
	case r >= 0x2E80 && r <= 0x9FFF: // CJK 部首…汉字
		return true
	case r >= 0x3000 && r <= 0x303F: // 中文标点
		return true
	case r >= 0x3040 && r <= 0x30FF: // 日文假名
		return true
	case r >= 0xAC00 && r <= 0xD7A3: // 韩文音节
		return true
	case r >= 0xF900 && r <= 0xFAFF: // 兼容汉字
		return true
	case r >= 0xFF00 && r <= 0xFFEF: // 全角字符
		return true
	}
	return false
}

// advanceOf 是一个字符在当前字号下的步进宽度（pt）。
func advanceOf(plan *subsetPlan, r rune, fontSize float64) float64 {
	if plan == nil || plan.upem == 0 {
		return fontSize / 2
	}
	adv, ok := plan.advOf[r]
	if !ok {
		return float64(plan.upem) / 2 * fontSize / float64(plan.upem)
	}
	return float64(adv) * fontSize / float64(plan.upem)
}

// ---- PDF 对象机 ----

type pdfDoc struct {
	buf  bytes.Buffer
	offs []int64 // offs[id] 是该对象的字节偏移（id 从 1 起，offs[0] 不用）
}

func newPDFDoc() *pdfDoc { return &pdfDoc{offs: make([]int64, 1)} }

// object 按顺序写入一个间接对象并记录它的偏移。
//
// 对象必须按 id 从 1 递增地写——xref 表本来就要求偏移量按对象号排序，
// 顺手写的好处是「写忘了某个 id」会以 xref 错位的形式立刻暴露，而不是留在文件里。
func (d *pdfDoc) object(body string) {
	d.offs = append(d.offs, int64(d.buf.Len()))
	fmt.Fprintf(&d.buf, "%d 0 obj\n%s\nendobj\n", len(d.offs)-1, body)
}

func flateCompress(b []byte) []byte {
	var out bytes.Buffer
	zw, err := zlib.NewWriterLevel(&out, flate.DefaultCompression)
	if err != nil {
		return b // zlib 的 NewWriterLevel 只对非法 level 报错；这里用默认级别，不可能走到
	}
	_, _ = zw.Write(b)
	_ = zw.Close()
	return out.Bytes()
}

// streamObj 拼一个带 FlateDecode 的流对象体。
//
// /Length 是**压缩后**的字节数；FontFile2 额外要 /Length1（解压后的长度），
// 少给它是字体嵌不进去的经典原因之一。
func streamObj(payload []byte, extra string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<< /Length %d /Filter /FlateDecode%s >>\nstream\n", len(payload), extra)
	b.Write(payload)
	b.WriteString("\nendstream")
	return b.String()
}

// writePDF 把排好的页写成 PDF 字节流。
//
// 刻意**不写时间戳**（CreationDate 之类）：同样的输入必须得到同样的字节。
// 这让导出的产物可以比对哈希——归档场景下这是一条很值钱的确定性，而不是洁癖。
func writePDF(pages []string, plan *subsetPlan, fontBytes []byte, src *ttf, fontSize float64, family string) ([]byte, int, error) {
	capacity := linesPerPage(fontSize)
	laid := make([][]string, 0, len(pages))
	total := 0
	for i, text := range pages {
		lines := wrap(text, plan, fontSize)
		if len(lines) > capacity {
			return nil, 0, &pageOverflowErr{i + 1, len(lines), capacity, fontSize}
		}
		laid = append(laid, lines)
		total += len(lines)
	}

	fontSub := flateCompress(fontBytes)
	toUni := flateCompress(toUnicodeCMap(plan))
	contents := make([][]byte, len(laid))
	for i, lines := range laid {
		contents[i] = flateCompress(pageContent(lines, plan, fontSize))
	}

	n := len(laid)
	const base = 3 // 1=Catalog, 2=Pages
	pagesEnd := base + 2*n
	fontType0 := pagesEnd
	fontCID := fontType0 + 1
	toUniID := fontType0 + 2
	descID := fontType0 + 3
	filesID := fontType0 + 4

	d := newPDFDoc()
	kids := make([]string, n)
	for i := range kids {
		kids[i] = fmt.Sprintf("%d 0 R", base+2*i)
	}
	d.object("<< /Type /Catalog /Pages 2 0 R >>")
	d.object(fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), n))
	for i := 0; i < n; i++ {
		d.object(fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 %s %s] "+
			"/Resources << /Font << /F1 %d 0 R >> >> /Contents %d 0 R >>",
			num(pageWidth), num(pageHeight), fontType0, base+2*i+1))
		d.object(streamObj(contents[i], ""))
	}

	widths := make([]string, 0, len(plan.order))
	for _, r := range plan.order {
		widths = append(widths, fmt.Sprintf("%d", width1000(plan, plan.gidOf[r])))
	}
	d.object(fmt.Sprintf("<< /Type /Font /Subtype /Type0 /BaseFont /%s "+
		"/Encoding /Identity-H /DescendantFonts [%d 0 R] /ToUnicode %d 0 R >>",
		family, fontCID, toUniID))
	d.object(fmt.Sprintf("<< /Type /Font /Subtype /CIDFontType2 /BaseFont /%s "+
		"/CIDSystemInfo << /Registry (Adobe) /Ordering (Identity) /Supplement 0 >> "+
		"/FontDescriptor %d 0 R /DW 1000 /CIDToGIDMap /Identity /W [0 [%s]] >>",
		family, descID, strings.Join(widths, " ")))
	d.object(streamObj(toUni, ""))
	d.object(fontDescriptor(src, family, filesID))
	d.object(streamObj(fontSub, fmt.Sprintf(" /Length1 %d", len(fontBytes))))

	var trailer bytes.Buffer
	fmt.Fprintf(&trailer, "%%PDF-1.4\n")
	trailer.Write(d.buf.Bytes())
	start := trailer.Len()
	fmt.Fprintf(&trailer, "xref\n0 %d\n", len(d.offs))
	fmt.Fprintf(&trailer, "0000000000 65535 f \n")
	for id := 1; id < len(d.offs); id++ {
		fmt.Fprintf(&trailer, "%010d 00000 n \n", d.offs[id])
	}
	fmt.Fprintf(&trailer, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(d.offs), start)
	return trailer.Bytes(), total, nil
}

// num 把浮点数写成 PDF 坐标（最多两位小数，去掉无意义的 0）。
func num(v float64) string {
	s := fmt.Sprintf("%.2f", v)
	s = strings.TrimRight(s, "0")
	return strings.TrimSuffix(s, ".")
}

// width1000 把步进宽度换算到 PDF 的 1/1000 文本单位（CIDFontType2 的 W 就是这套单位）。
func width1000(plan *subsetPlan, gid uint16) int {
	for r, g := range plan.gidOf {
		if g == gid {
			return int(math.Round(float64(plan.advOf[r]) * 1000 / float64(plan.upem)))
		}
	}
	return 0
}

// pageContent 生成一页的内容流。
func pageContent(lines []string, plan *subsetPlan, fontSize float64) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "BT\n/F1 %s Tf\n", num(fontSize))
	y := pageHeight - marginTop - fontSize
	for _, line := range lines {
		if strings.TrimSpace(line) != "" {
			fmt.Fprintf(&b, "1 0 0 1 %s %s Tm <%s> Tj\n", num(marginX), num(y), hexCodes(line, plan))
		}
		y -= fontSize * lineFactor
	}
	b.WriteString("ET\n")
	return []byte(b.String())
}

// hexCodes 把一行文本变成 Identity-H 的十六进制字符串（每个字形一个 16 位码）。
func hexCodes(line string, plan *subsetPlan) string {
	var b strings.Builder
	for _, r := range line {
		fmt.Fprintf(&b, "%04X", plan.gidOf[r])
	}
	return b.String()
}

// toUnicodeCMap 造 ToUnicode 映射：阅读器靠它把字形码还原成可选可搜的文字。
//
// 没有它 PDF 也能「显示」，但复制出来是一串乱码、搜索也搜不到——
// 对账单这种东西，用户几乎一定会先试着搜金额。
func toUnicodeCMap(plan *subsetPlan) []byte {
	var rows []string
	for _, r := range plan.order {
		uni := utf16Hex(r)
		rows = append(rows, fmt.Sprintf("<%04X> <%s>\n", plan.gidOf[r], uni))
	}
	return []byte(fmt.Sprintf(
		"/CIDInit /ProcSet findresource begin\n"+
			"12 dict begin\n"+
			"begincmap\n"+
			"/CIDSystemInfo << /Registry (Adobe) /Ordering (UCS) /Supplement 0 >> def\n"+
			"/CMapName /Adobe-Identity-UCS def\n"+
			"/CMapType 2 def\n"+
			"1 begincodespacerange\n<0000> <FFFF>\nendcodespacerange\n"+
			"%d beginbfchar\n%sendbfchar\n"+
			"endcmap\n"+
			"CMapName currentdict /CMap defineresource pop\n"+
			"end\nend\n", len(rows), strings.Join(rows, "")))
}

// utf16Hex 把一个字符编码成 UTF-16BE 的十六进制（非 BMP 字符是代理对，共 8 位）。
func utf16Hex(r rune) string {
	if r <= 0xFFFF {
		return fmt.Sprintf("%04X", r)
	}
	r -= 0x10000
	hi := 0xD800 + (r >> 10)
	lo := 0xDC00 + (r & 0x3FF)
	return fmt.Sprintf("%04X%04X", hi, lo)
}

// fontDescriptor 从**原字体**的量表里取数值（cloned 到 1/1000 单位）。
func fontDescriptor(src *ttf, family string, fontFileID int) string {
	scale := 1000.0 / float64(src.upem)
	xMin := int16(binary.BigEndian.Uint16(src.head[36:]))
	yMin := int16(binary.BigEndian.Uint16(src.head[38:]))
	xMax := int16(binary.BigEndian.Uint16(src.head[40:]))
	yMax := int16(binary.BigEndian.Uint16(src.head[42:]))
	ascender := int16(binary.BigEndian.Uint16(src.hhea[4:]))
	descender := int16(binary.BigEndian.Uint16(src.hhea[6:]))
	capHeight := float64(src.upem) * 0.7
	if os2 := src.tables["OS/2"]; len(os2) >= 90 {
		if v := int16(binary.BigEndian.Uint16(os2[88:])); v > 0 {
			capHeight = float64(v)
		}
	}
	italic := 0.0
	if post := src.tables["post"]; len(post) >= 8 {
		italic = float64(int32(binary.BigEndian.Uint32(post[4:]))) / 65536
	}
	return fmt.Sprintf("<< /Type /FontDescriptor /FontName /%s /Flags 4 "+
		"/FontBBox [%s %s %s %s] /ItalicAngle %s /Ascent %s /Descent %s /CapHeight %s "+
		"/StemV 80 /FontFile2 %d 0 R >>",
		family,
		num(float64(xMin)*scale), num(float64(yMin)*scale),
		num(float64(xMax)*scale), num(float64(yMax)*scale),
		num(italic),
		num(float64(ascender)*scale), num(float64(descender)*scale), num(capHeight*scale),
		fontFileID)
}
