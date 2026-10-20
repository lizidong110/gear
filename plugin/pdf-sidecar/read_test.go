package main

// read 面（PDF 文本提取）的测试。
//
// 三个层次，由内到外：
//  1. 纯函数：CMap 解析（bfchar/bfrange）、内容流词法（TJ 间距 / 行结构）。
//  2. 整文档装配：手工构造的 PDF（简单字体 / ObjStm / 加密 / 无 ToUnicode）。
//  3. handler + round-trip：**自己 write 的 PDF 自己 read 回原文**——这是最值钱的
//     一条：write 侧生成的是 Identity-H + ToUnicode + Flate 的全套现代 PDF 形态，
//     round-trip 绿了说明提取器对「自己生态里产出的 PDF」有确定性保证。

import (
	"bytes"
	"compress/zlib"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---- 1. 纯函数 ----

func TestParseCMapBFRangeAndBFChar(t *testing.T) {
	cmap := []byte(`/CIDInit /ProcSet findresource begin
12 dict begin
begincmap
/CIDSystemInfo << /Registry (Adobe) /Ordering (UCS) /Supplement 0 >> def
1 begincodespacerange
<0000> <FFFF>
endcodespacerange
2 beginbfchar
<0003> <4E2D>
<0004> <5FA9>
endbfchar
2 beginbfrange
<0010> <0012> <0041>
<0020> <0021> [<0042> <0043>]
endbfrange
endcmap
`)
	out := map[uint32]string{}
	parseCMap(cmap, out)
	cases := map[uint32]string{0x0003: "中", 0x0004: "復", 0x0010: "A", 0x0011: "B", 0x0012: "C", 0x0020: "B", 0x0021: "C"}
	for code, want := range cases {
		if got := out[code]; got != want {
			t.Fatalf("CMap 码 %#04x 应为 %q，实际 %q", code, want, got)
		}
	}
}

func TestExtractPageTextTJSpacingAndLineBreaks(t *testing.T) {
	// 简单字体：literal string 直接是 Latin-1；TJ 数组里 -300 应变成空格，
	// Td 的 y 变化应换行。
	content := []byte("BT /F1 12 Tf 72 720 Td (Hello World) Tj 0 -20 Td [(A) -300 (B)] TJ ET")
	text, used := extractPageText(content, nil)
	if !used {
		t.Fatal("有文本操作必须 used=true")
	}
	if text != "Hello World\nA B" {
		t.Fatalf("行结构与 TJ 间距不对：%q", text)
	}
}

// ---- 2. 手工构造的 PDF ----

// craftSimplePDF 造一个最简单的单页 PDF：Helvetica 简单字体，literal string，
// 未压缩——提取器最底层能力的试金石。
func craftSimplePDF() []byte {
	content := "BT /F1 12 Tf 72 720 Td (Hello PDF) Tj ET"
	var b strings.Builder
	b.WriteString("%PDF-1.4\n")
	b.WriteString("1 0 obj\n<< /Type /Catalog /Pages 2 0 R >>\nendobj\n")
	b.WriteString("2 0 obj\n<< /Type /Pages /Kids [3 0 R] /Count 1 >>\nendobj\n")
	b.WriteString("3 0 obj\n<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] " +
		"/Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>\nendobj\n")
	b.WriteString("4 0 obj\n<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>\nendobj\n")
	fmt.Fprintf(&b, "5 0 obj\n<< /Length %d >>\nstream\n%s\nendstream\nendobj\n", len(content), content)
	b.WriteString("xref\n0 6\n0000000000 65535 f \n")
	for i := 1; i <= 5; i++ {
		fmt.Fprintf(&b, "%010d 00000 n \n", i*40) // 偏移不需要真实：提取器不依赖 xref
	}
	b.WriteString("trailer\n<< /Size 6 /Root 1 0 R >>\nstartxref\n9\n%%EOF\n")
	return []byte(b.String())
}

func TestReadHandcraftedSimpleFontPDF(t *testing.T) {
	res, err := extractPDFText(craftSimplePDF(), 10)
	if err != nil {
		t.Fatalf("读手工 PDF 失败: %v", err)
	}
	if len(res.Pages) != 1 || !strings.Contains(res.Pages[0].Text, "Hello PDF") {
		t.Fatalf("文本不对：%+v", res.Pages)
	}
	if res.CID {
		t.Fatal("简单字体不该标 CID")
	}
	if len(res.Fonts) != 1 || res.Fonts[0] != "Helvetica" {
		t.Fatalf("字体名应为 Helvetica，实际 %v", res.Fonts)
	}
}

// craftObjStmPDF 把 Font 对象塞进压缩对象流（Word 2016+ 的常见形态），
// 页面上只剩引用——不展开 ObjStm 就永远找不到字体。
func craftObjStmPDF(t *testing.T) []byte {
	t.Helper()
	fontObj := "4 0 << /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>"
	header := "4 0\n" // N=1, First=len(header)
	payload := []byte(header + fontObj)
	var zbuf bytes.Buffer
	zw := zlib.NewWriter(&zbuf)
	if _, err := zw.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	content := "BT /F1 12 Tf 72 720 Td (In ObjStm) Tj ET"
	var b strings.Builder
	b.WriteString("%PDF-1.4\n")
	b.WriteString("1 0 obj\n<< /Type /Catalog /Pages 2 0 R >>\nendobj\n")
	b.WriteString("2 0 obj\n<< /Type /Pages /Kids [3 0 R] /Count 1 >>\nendobj\n")
	b.WriteString("3 0 obj\n<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] " +
		"/Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>\nendobj\n")
	fmt.Fprintf(&b, "6 0 obj\n<< /Type /ObjStm /N 1 /First %d /Length %d /Filter /FlateDecode >>\nstream\n",
		len(header), zbuf.Len())
	b.Write(zbuf.Bytes())
	b.WriteString("\nendstream\nendobj\n")
	fmt.Fprintf(&b, "5 0 obj\n<< /Length %d >>\nstream\n%s\nendstream\nendobj\n", len(content), content)
	b.WriteString("trailer\n<< /Size 7 /Root 1 0 R >>\n%%EOF\n")
	return []byte(b.String())
}

func TestReadHandcraftedObjStm(t *testing.T) {
	res, err := extractPDFText(craftObjStmPDF(t), 10)
	if err != nil {
		t.Fatalf("读 ObjStm PDF 失败: %v", err)
	}
	if !strings.Contains(res.Pages[0].Text, "In ObjStm") {
		t.Fatalf("ObjStm 里的字体没解出来：%+v", res.Pages)
	}
}

func TestReadEncryptedPDFRejected(t *testing.T) {
	data := craftSimplePDF()
	data = bytes.Replace(data, []byte("<< /Type /Catalog"), []byte("<< /Encrypt 7 0 R /Type /Catalog"), 1)
	_, err := extractPDFText(data, 10)
	if err == nil || !strings.Contains(err.Error(), "加密") {
		t.Fatalf("加密 PDF 必须明确拒绝，实际 %v", err)
	}
}

// TestReadType0WithoutToUnicodeRejected：Identity-H 却没有 ToUnicode ⇒ 每个码都
// 查不出字，吐出来只会是 U+FFFD 串。明确报错比静默乱码好。
func TestReadType0WithoutToUnicodeRejected(t *testing.T) {
	content := "BT /F1 12 Tf 72 720 Td <00010002> Tj ET"
	var b strings.Builder
	b.WriteString("%PDF-1.4\n")
	b.WriteString("1 0 obj\n<< /Type /Catalog /Pages 2 0 R >>\nendobj\n")
	b.WriteString("2 0 obj\n<< /Type /Pages /Kids [3 0 R] /Count 1 >>\nendobj\n")
	b.WriteString("3 0 obj\n<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] " +
		"/Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>\nendobj\n")
	b.WriteString("4 0 obj\n<< /Type /Font /Subtype /Type0 /BaseFont /X /Encoding /Identity-H " +
		"/DescendantFonts [6 0 R] >>\nendobj\n")
	fmt.Fprintf(&b, "5 0 obj\n<< /Length %d >>\nstream\n%s\nendstream\nendobj\n", len(content), content)
	b.WriteString("6 0 obj\n<< /Type /Font /Subtype /CIDFontType2 /BaseFont /X >>\nendobj\n")
	b.WriteString("trailer\n<< /Size 7 /Root 1 0 R >>\n%%EOF\n")
	_, err := extractPDFText([]byte(b.String()), 10)
	if err == nil || !strings.Contains(err.Error(), "ToUnicode") {
		t.Fatalf("Type0 无 ToUnicode 必须明确报错，实际 %v", err)
	}
}

// TestReadSubsetFontToUnicode：回归测试——本用户 PDF 的真实坑。
//
// 子集 / Type3 字体用**自定义 1 字节编码**（不是 Latin-1），中文只能靠 ToUnicode
// CMap 还原。修复前 writeGlyphs 对「非 Identity」字体一律原样写字节 → 一整页乱码。
// 这里造一个 /Subtype /Type1 的 subset 字体：码 0x41→中 0x42→文 0x43→復，
// 内容流写 (ABC)，断言抽出来是「中文復」而非原始字节 "ABC"，且 CID=false。
func TestReadSubsetFontToUnicode(t *testing.T) {
	content := "BT /F1 12 Tf 72 720 Td (ABC) Tj ET"
	toUni := `begincmap
1 beginbfchar
<41> <4E2D>
<42> <6587>
<43> <590D>
endbfchar
endcmap`
	var b strings.Builder
	b.WriteString("%PDF-1.4\n")
	b.WriteString("1 0 obj\n<< /Type /Catalog /Pages 2 0 R >>\nendobj\n")
	b.WriteString("2 0 obj\n<< /Type /Pages /Kids [3 0 R] /Count 1 >>\nendobj\n")
	b.WriteString("3 0 obj\n<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] " +
		"/Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>\nendobj\n")
	b.WriteString("4 0 obj\n<< /Type /Font /Subtype /Type1 /BaseFont /SubsetFont " +
		"/ToUnicode 6 0 R >>\nendobj\n")
	fmt.Fprintf(&b, "6 0 obj\n<< /Length %d >>\nstream\n%s\nendstream\nendobj\n", len(toUni), toUni)
	fmt.Fprintf(&b, "5 0 obj\n<< /Length %d >>\nstream\n%s\nendstream\nendobj\n", len(content), content)
	b.WriteString("trailer\n<< /Size 7 /Root 1 0 R >>\n%%EOF\n")
	data := []byte(b.String())

	res, err := extractPDFText(data, 10)
	if err != nil {
		t.Fatalf("读 subset PDF 失败: %v", err)
	}
	got := res.Pages[0].Text
	if strings.Contains(got, "ABC") {
		t.Fatalf("subset 字体被当 Latin-1 原样输出（回归！）：%q", got)
	}
	if !strings.Contains(got, "中文复") {
		t.Fatalf("自定义编码未走 ToUnicode 还原，实际 %q", got)
	}
	if res.CID {
		t.Fatal("Type1 subset 字体不该标 CID")
	}
}

func TestReadNonPDFRejected(t *testing.T) {
	_, err := extractPDFText([]byte("这不是 PDF，只是一段文字"), 10)
	if err == nil || !strings.Contains(err.Error(), "%PDF-") {
		t.Fatalf("非 PDF 必须报文件头错误，实际 %v", err)
	}
}

// TestReadScannedPageNoText：图片型 PDF（无文本流）——说清楚是扫描件，
// 让调用方（和 agent）知道该走 OCR，而不是拿个空结果当成功。
func TestReadScannedPageNoText(t *testing.T) {
	var b strings.Builder
	b.WriteString("%PDF-1.4\n")
	b.WriteString("1 0 obj\n<< /Type /Catalog /Pages 2 0 R >>\nendobj\n")
	b.WriteString("2 0 obj\n<< /Type /Pages /Kids [3 0 R] /Count 1 >>\nendobj\n")
	b.WriteString("3 0 obj\n<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] " +
		"/Resources << /XObject << /Im0 4 0 R >> >> >>\nendobj\n")
	b.WriteString("4 0 obj\n<< /Type /XObject /Subtype /Image /Width 100 /Height 100 >>\nstream\n")
	b.WriteString("fake-image-bytes")
	b.WriteString("\nendstream\nendobj\n")
	b.WriteString("trailer\n<< /Size 5 /Root 1 0 R >>\n%%EOF\n")
	_, err := extractPDFText([]byte(b.String()), 10)
	if err == nil || !strings.Contains(err.Error(), "扫描件") {
		t.Fatalf("全空 PDF 必须提示扫描件，实际 %v", err)
	}
}

// ---- 3. handler 与 round-trip ----

func readReq(t *testing.T, args map[string]interface{}) *http.Request {
	t.Helper()
	body, err := json.Marshal(map[string]interface{}{"args": args})
	if err != nil {
		t.Fatal(err)
	}
	return httptest.NewRequest(http.MethodPost, "/read", strings.NewReader(string(body)))
}

// writeForRead 用 write 面产出一个 PDF，返回其路径（相对 root）。
func writeForRead(t *testing.T, root string, texts ...string) string {
	t.Helper()
	font := writeFixture(t, fixtureOpt{})
	pages := make([]interface{}, 0, len(texts))
	for _, tx := range texts {
		pages = append(pages, map[string]interface{}{"text": tx})
	}
	rec := httptest.NewRecorder()
	(&service{}).handleWrite(rec, writeReq(t, map[string]interface{}{
		"root":  root,
		"path":  "in.pdf",
		"font":  font,
		"pages": pages,
	}))
	if errStr, _ := decodeResp(t, rec); errStr != "" {
		t.Fatalf("write 失败: %s", errStr)
	}
	return "in.pdf"
}

// TestReadRoundTripCJKSelfWritten 是本文件的核心：自己写的 PDF 必须能读回原文。
// 它一次覆盖 Identity-H 双字节码、ToUnicode CMap（bfchar）、Flate 解压、
// trailer 定位、Pages 树、内容流词法——全链路确定性。
func TestReadRoundTripCJKSelfWritten(t *testing.T) {
	root := t.TempDir()
	text := "A 復 中 漢 B"
	writeForRead(t, root, text)

	rec := httptest.NewRecorder()
	(&service{}).handleRead(rec, readReq(t, map[string]interface{}{
		"root": root,
		"path": "in.pdf",
	}))
	errStr, result := decodeResp(t, rec)
	if errStr != "" {
		t.Fatalf("round-trip 读失败: %s", errStr)
	}
	pages, ok := result["pages"].([]interface{})
	if !ok || len(pages) != 1 {
		t.Fatalf("pages 形状不对: %v", result["pages"])
	}
	page := pages[0].(map[string]interface{})
	got := page["text"].(string)
	if !strings.Contains(got, "復") || !strings.Contains(got, "中") || !strings.Contains(got, "漢") {
		t.Fatalf("中文没读回来：%q（原文 %q）", got, text)
	}
	if !strings.Contains(got, "A") {
		t.Fatalf("ASCII 也没读回来：%q", got)
	}
	if !result["cid"].(bool) {
		t.Fatal("write 的产物是 Identity-H，必须标 cid=true")
	}
}

func TestReadRoundTripMultiPage(t *testing.T) {
	root := t.TempDir()
	writeForRead(t, root, "A 復 中", "B 漢 復")
	rec := httptest.NewRecorder()
	(&service{}).handleRead(rec, readReq(t, map[string]interface{}{
		"root": root,
		"path": "in.pdf",
	}))
	errStr, result := decodeResp(t, rec)
	if errStr != "" {
		t.Fatalf("多页读失败: %s", errStr)
	}
	if n := result["page_count"].(float64); n != 2 {
		t.Fatalf("应为 2 页，实际 %v", n)
	}
	p1 := result["pages"].([]interface{})[0].(map[string]interface{})["text"].(string)
	p2 := result["pages"].([]interface{})[1].(map[string]interface{})["text"].(string)
	if !strings.Contains(p1, "復") || !strings.Contains(p2, "漢") {
		t.Fatalf("分页文本串了：p1=%q p2=%q", p1, p2)
	}
}

func TestHandleReadPathEscapeRejected(t *testing.T) {
	rec := httptest.NewRecorder()
	(&service{}).handleRead(rec, readReq(t, map[string]interface{}{
		"root": t.TempDir(),
		"path": "../../etc/passwd",
	}))
	errStr, _ := decodeResp(t, rec)
	if !strings.Contains(errStr, errPathEscape) {
		t.Fatalf("路径逃逸必须拒，实际 %q", errStr)
	}
}

func TestHandleReadMissingPathRejected(t *testing.T) {
	rec := httptest.NewRecorder()
	(&service{}).handleRead(rec, readReq(t, map[string]interface{}{"root": t.TempDir()}))
	errStr, _ := decodeResp(t, rec)
	if !strings.Contains(errStr, errMissingPath) {
		t.Fatalf("缺 path 必须拒，实际 %q", errStr)
	}
}

// TestHandleReadMaxPagesCap：max_pages 护栏生效且 note 说明截断（不静默）。
func TestHandleReadMaxPagesCap(t *testing.T) {
	root := t.TempDir()
	writeForRead(t, root, "A 復 中", "B 漢 復", "A 中 漢")
	rec := httptest.NewRecorder()
	(&service{}).handleRead(rec, readReq(t, map[string]interface{}{
		"root":      root,
		"path":      "in.pdf",
		"max_pages": float64(2),
	}))
	errStr, result := decodeResp(t, rec)
	if errStr != "" {
		t.Fatalf("max_pages 读失败: %s", errStr)
	}
	if n := result["page_count"].(float64); n != 2 {
		t.Fatalf("max_pages=2 应只抽 2 页，实际 %v", n)
	}
	if !strings.Contains(result["note"].(string), "max_pages") {
		t.Fatalf("截断必须在 note 里说明，实际 %q", result["note"])
	}
}

// TestHandleCapsListsWriteAndRead：能力面声明必须与真实 handler 一致——
// caps 少写一个方法，能力目录就少一层（「描述比实现窄」是静默缺陷）。
func TestHandleCapsListsWriteAndRead(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/caps", strings.NewReader("{}"))
	(&service{}).handleCaps(rec, req)
	_, result := decodeResp(t, rec)
	methods := result["methods"].([]interface{})
	names := map[string]bool{}
	for _, m := range methods {
		names[m.(string)] = true
	}
	if !names["write"] || !names["read"] {
		t.Fatalf("caps 必须同时列 write 与 read，实际 %v", names)
	}
}

// TestReadFileSizeGuard：超过 64MB 的 PDF 直接拒（不接这种单子）。
func TestReadFileSizeGuard(t *testing.T) {
	root := t.TempDir()
	big := filepath.Join(root, "big.pdf")
	if err := os.WriteFile(big, append([]byte("%PDF-1.4\n"), make([]byte, maxPDFBytes+1)...), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readPDFFile(big, 10); err == nil || !strings.Contains(err.Error(), "上限") {
		t.Fatalf("超大 PDF 必须拒，实际 %v", err)
	}
}
