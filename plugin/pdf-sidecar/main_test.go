package main

import (
	"bytes"
	"compress/zlib"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 本文件测 handler 层与排版纯函数。
//
// handler 直接以 `s.handleWrite(w, r)` 调用，不起真 HTTP 服务：这样既能覆盖
// 入参解析 / 路径囚禁 / 字体供给 / 排版 / 落盘整条链路，又不必去抢 COORD_ADDR 端口。

// writeReq 把 args 包成侧车约定的 `{args: ...}` 请求体。
func writeReq(t *testing.T, args map[string]interface{}) *http.Request {
	t.Helper()
	body, err := json.Marshal(map[string]interface{}{"args": args})
	if err != nil {
		t.Fatal(err)
	}
	return httptest.NewRequest(http.MethodPost, "/write", strings.NewReader(string(body)))
}

// decodeResp 把 handler 的响应拆成 error / result 两截。
func decodeResp(t *testing.T, rec *httptest.ResponseRecorder) (string, map[string]interface{}) {
	t.Helper()
	var resp struct {
		Error  string                 `json:"error"`
		Result map[string]interface{} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应不是合法 JSON: %v（body=%s）", err, rec.Body.String())
	}
	return resp.Error, resp.Result
}

func TestHandleWriteBasic(t *testing.T) {
	font := writeFixture(t, fixtureOpt{})
	root := t.TempDir()
	rec := httptest.NewRecorder()
	(&service{}).handleWrite(rec, writeReq(t, map[string]interface{}{
		"root": root,
		"path": "out.pdf",
		"font": font,
		"pages": []interface{}{
			map[string]interface{}{"text": "A 復 B"},
		},
	}))
	if errStr, _ := decodeResp(t, rec); errStr != "" {
		t.Fatalf("正常请求不应报错: %s", errStr)
	}
	target := filepath.Join(root, "out.pdf")
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("PDF 没落盘: %v", err)
	}
	if !bytes.HasPrefix(data, []byte("%PDF-1.4")) {
		t.Fatalf("产物不是 PDF：%q", data[:min(20, len(data))])
	}
	if !bytes.HasSuffix(trimTrailing(data), []byte("%%EOF")) {
		t.Fatalf("PDF 没有以 %%EOF 收尾")
	}
	if !xrefLooksSane(data) {
		t.Fatalf("PDF 的 xref 偏移对不上（阅读器会拒收）")
	}
}

func TestHandleWritePagesArrayForm(t *testing.T) {
	font := writeFixture(t, fixtureOpt{})
	root := t.TempDir()
	rec := httptest.NewRecorder()
	// PRD §4.5 也允许 `[\"第一页\", \"第二页\"]` 这种裸字符串写法
	(&service{}).handleWrite(rec, writeReq(t, map[string]interface{}{
		"root":  root,
		"path":  "two.pdf",
		"font":  font,
		"pages": []interface{}{"A", "B 中"},
	}))
	errStr, result := decodeResp(t, rec)
	if errStr != "" {
		t.Fatalf("裸字符串 pages 不应报错: %s", errStr)
	}
	if int(result["pages"].(float64)) != 2 {
		t.Fatalf("应识别为 2 页，实际 %v", result["pages"])
	}
}

func TestHandleWriteMissingPath(t *testing.T) {
	rec := httptest.NewRecorder()
	(&service{}).handleWrite(rec, writeReq(t, map[string]interface{}{"root": t.TempDir()}))
	errStr, _ := decodeResp(t, rec)
	if !strings.Contains(errStr, errMissingPath) {
		t.Fatalf("缺 path 应报 %s，实际 %q", errMissingPath, errStr)
	}
}

func TestHandleWriteBadPages(t *testing.T) {
	cases := []interface{}{
		nil,
		[]interface{}{},
		"notarray",
		[]interface{}{map[string]interface{}{}}, // 缺 text
	}
	for _, p := range cases {
		rec := httptest.NewRecorder()
		(&service{}).handleWrite(rec, writeReq(t, map[string]interface{}{
			"root": t.TempDir(), "path": "x.pdf", "font": writeFixture(t, fixtureOpt{}), "pages": p,
		}))
		errStr, _ := decodeResp(t, rec)
		if !strings.Contains(errStr, errNoPages) {
			t.Fatalf("非法 pages=%v 应报 %s，实际 %q", p, errNoPages, errStr)
		}
	}
}

func TestHandleWriteFontSizeRange(t *testing.T) {
	for _, fs := range []float64{1.0, 100.0} {
		rec := httptest.NewRecorder()
		(&service{}).handleWrite(rec, writeReq(t, map[string]interface{}{
			"root": t.TempDir(), "path": "x.pdf", "font": writeFixture(t, fixtureOpt{}),
			"pages": []interface{}{"hi"}, "font_size": fs,
		}))
		errStr, _ := decodeResp(t, rec)
		if !strings.Contains(errStr, errBadSize) {
			t.Fatalf("font_size=%.1f 应报 %s，实际 %q", fs, errBadSize, errStr)
		}
	}
}

func TestHandleWritePathEscape(t *testing.T) {
	rec := httptest.NewRecorder()
	(&service{}).handleWrite(rec, writeReq(t, map[string]interface{}{
		"root": t.TempDir(), "path": "../escape.pdf", "font": writeFixture(t, fixtureOpt{}),
		"pages": []interface{}{"x"},
	}))
	errStr, _ := decodeResp(t, rec)
	if !strings.Contains(errStr, errPathEscape) {
		t.Fatalf("越界路径应报 %s，实际 %q", errPathEscape, errStr)
	}
}

func TestHandleWriteFontNotFound(t *testing.T) {
	rec := httptest.NewRecorder()
	(&service{}).handleWrite(rec, writeReq(t, map[string]interface{}{
		"root": t.TempDir(), "path": "x.pdf", "font": filepath.Join(t.TempDir(), "nope.ttf"),
		"pages": []interface{}{"x"},
	}))
	errStr, _ := decodeResp(t, rec)
	if !strings.Contains(errStr, errFontNoFont) {
		t.Fatalf("字体缺失应报 %s，实际 %q", errFontNoFont, errStr)
	}
}

func TestHandleWriteMissingGlyph(t *testing.T) {
	// 夹具里没有 'Z'：必须报错而不是画出方框
	rec := httptest.NewRecorder()
	(&service{}).handleWrite(rec, writeReq(t, map[string]interface{}{
		"root": t.TempDir(), "path": "x.pdf", "font": writeFixture(t, fixtureOpt{}),
		"pages": []interface{}{"Z"},
	}))
	errStr, _ := decodeResp(t, rec)
	if !strings.Contains(errStr, errFontNoGlyph) {
		t.Fatalf("缺字应报 %s，实际 %q", errFontNoGlyph, errStr)
	}
}

func TestHandleWritePageOverflow(t *testing.T) {
	// 50 段各一行，超过 A4 / 12pt 一页的 41 行上限 ⇒ 报错而非偷偷加页
	rec := httptest.NewRecorder()
	(&service{}).handleWrite(rec, writeReq(t, map[string]interface{}{
		"root": t.TempDir(), "path": "x.pdf", "font": writeFixture(t, fixtureOpt{}),
		"pages": []interface{}{strings.Repeat("AB\n", 50)},
	}))
	errStr, _ := decodeResp(t, rec)
	if !strings.Contains(errStr, errOverflow) {
		t.Fatalf("超出一页应报 %s，实际 %q", errOverflow, errStr)
	}
}

func TestHandleCaps(t *testing.T) {
	rec := httptest.NewRecorder()
	(&service{}).handleCaps(rec, httptest.NewRequest(http.MethodGet, "/caps", nil))
	_, result := decodeResp(t, rec)
	methods, ok := result["methods"].([]interface{})
	if !ok || len(methods) == 0 {
		t.Fatalf("/caps 应返回 methods 列表，实际 %v", result)
	}
}

func TestLoadFontExplicit(t *testing.T) {
	font := writeFixture(t, fixtureOpt{})
	data, name, err := loadFont(font)
	if err != nil {
		t.Fatalf("读取显式字体失败: %v", err)
	}
	if len(data) == 0 || name != "fixture.ttf" {
		t.Fatalf("loadFont 返回异常: name=%q len=%d", name, len(data))
	}
}

func TestLoadFontMissing(t *testing.T) {
	_, _, err := loadFont(filepath.Join(t.TempDir(), "gone.ttf"))
	if err == nil {
		t.Fatal("不存在的字体必须报错")
	}
}

// ---- 排版纯函数 ----

func TestIsCJK(t *testing.T) {
	yes := []rune{'中', 'あ', '가', '　', '，', 'Ａ'}
	no := []rune{'A', '1', ' ', 'é'}
	for _, r := range yes {
		if !isCJK(r) {
			t.Errorf("%q 应判为 CJK", r)
		}
	}
	for _, r := range no {
		if isCJK(r) {
			t.Errorf("%q 不应判为 CJK", r)
		}
	}
}

func TestCanBreakBetween(t *testing.T) {
	if !canBreakBetween(' ', 'A') {
		t.Error("空格之后应能断行")
	}
	if !canBreakBetween('中', '文') {
		t.Error("中日韩之间应能断行")
	}
	if canBreakBetween('A', 'B') {
		t.Error("两个 ASCII 之间不应断行（要守住词）")
	}
}

func TestLinesPerPage(t *testing.T) {
	if n := linesPerPage(12); n <= 0 {
		t.Fatalf("linesPerPage 应 >0，实际 %d", n)
	}
	if n := linesPerPage(72); n < 1 { // 极大字号：至少给 1 行
		t.Fatalf("极大字号也应 ≥1 行，实际 %d", n)
	}
}

func TestAdvanceOf(t *testing.T) {
	if got := advanceOf(nil, 'A', 12); got != 6 {
		t.Fatalf("nil plan 应退化为字号一半 (=6)，实际 %v", got)
	}
	plan := &subsetPlan{upem: 1000, advOf: map[rune]uint16{'A': 600}}
	if got := advanceOf(plan, 'A', 12); got != 7.2 {
		t.Fatalf("进阶宽度应 = 600*12/1000 = 7.2，实际 %v", got)
	}
	if got := advanceOf(plan, 'Z', 12); got != 6 {
		t.Fatalf("计划里没有的字符应退化为 6，实际 %v", got)
	}
}

func TestNum(t *testing.T) {
	cases := map[float64]string{595.28: "595.28", 48.0: "48", 1000.0: "1000", 0.5: "0.5"}
	for in, want := range cases {
		if got := num(in); got != want {
			t.Errorf("num(%v) = %q want %q", in, got, want)
		}
	}
}

func TestHexCodes(t *testing.T) {
	plan := &subsetPlan{gidOf: map[rune]uint16{'中': 3, 'A': 1}}
	if got := hexCodes("中A", plan); got != "00030001" {
		t.Fatalf("hexCodes 应拼成 Identity-H 码，实际 %q", got)
	}
}

func TestUtf16Hex(t *testing.T) {
	if got := utf16Hex('A'); got != "0041" {
		t.Fatalf("BMP 字符应是 4 位 hex，实际 %q", got)
	}
	if got := utf16Hex(0x1F600); got != "D83DDE00" { // 代理对
		t.Fatalf("非 BMP 字符应是 8 位代理对 hex，实际 %q", got)
	}
}

func TestToUnicodeCMap(t *testing.T) {
	plan := &subsetPlan{order: []rune{'A', '中'}, gidOf: map[rune]uint16{'A': 1, '中': 2}}
	out := toUnicodeCMap(plan)
	s := string(out)
	if !strings.Contains(s, "beginbfchar") || !strings.Contains(s, "<0001> <0041>") || !strings.Contains(s, "<0002> <4E2D>") {
		t.Fatalf("ToUnicode 没把字形码映射回字符：%s", s)
	}
}

func TestFlateRoundTrip(t *testing.T) {
	raw := []byte("the quick brown fox jumps over the lazy dog 中文测试")
	comp := flateCompress(raw)
	if len(comp) == 0 {
		t.Fatal("压缩结果不应为空")
	}
	// 反解一下确认无损
	dec, err := inflate(comp)
	if err != nil {
		t.Fatalf("反解失败: %v", err)
	}
	if string(dec) != string(raw) {
		t.Fatalf("压缩不是无损的")
	}
}

func TestWritePDFValidity(t *testing.T) {
	// 直接用 fixture 字体 + 一份真实子集计划，验证 writePDF 产物的结构。
	src, err := parseTTF(buildFixtureTTF(fixtureOpt{}))
	if err != nil {
		t.Fatal(err)
	}
	plan, fontBytes, err := src.subset([]rune{'A', '復', ' '})
	if err != nil {
		t.Fatal(err)
	}
	out, _, err := writePDF([]string{"A 復"}, plan, fontBytes, src, 12, "FIXTURE")
	if err != nil {
		t.Fatalf("writePDF 失败: %v", err)
	}
	if !bytes.HasPrefix(out, []byte("%PDF-1.4")) {
		t.Fatal("PDF 头不对")
	}
	if !xrefLooksSane(out) {
		t.Fatal("PDF xref 偏移对不上")
	}
}

func TestPageContentAndDescriptor(t *testing.T) {
	src, _ := parseTTF(buildFixtureTTF(fixtureOpt{}))
	plan, fontBytes, _ := src.subset([]rune{'A'})
	out, _, _ := writePDF([]string{"A"}, plan, fontBytes, src, 12, "FIXTURE")
	s := string(out)
	if !strings.Contains(s, "/Type /FontDescriptor") || !strings.Contains(s, "/FontFile2") {
		t.Fatal("缺失 FontDescriptor / FontFile2（字体没嵌进去）")
	}
	if !strings.Contains(s, "/Subtype /Type0") || !strings.Contains(s, "/Identity-H") {
		t.Fatal("缺失 Type0 / Identity-H 编码")
	}
}

// ---- 小工具 ----

// inflate 是 flateCompress 的反操作，仅用于单测里验证压缩无损。
func inflate(b []byte) ([]byte, error) {
	zr, err := zlib.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	return io.ReadAll(zr)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// trimTrailing 去掉末尾的空白再比对 %%EOF（部分阅读器在 EOF 前留换行）。
func trimTrailing(b []byte) []byte {
	return bytes.TrimRight(b, "\r\n \t")
}

// xrefLooksSane 顺着 startxref 找到 xref 表，核对每条偏移都指向 "N 0 obj"。
func xrefLooksSane(b []byte) bool {
	idx := bytes.LastIndex(b, []byte("startxref"))
	if idx < 0 {
		return false
	}
	rest := b[idx+9:]
	var off int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(rest)), "%d", &off); err != nil {
		return false
	}
	if off < 0 || off >= len(b) {
		return false
	}
	if !bytes.HasPrefix(b[off:], []byte("xref")) {
		return false
	}
	// 抽查：第一段 "0 1" 之后应是 "0000000000 65535 f"，再之后是任意 "NNNNNNNNNN 00000 n"
	return bytes.Contains(b[off:], []byte("65535 f"))
}
