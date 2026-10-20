package main

// PDF 文本提取（ext.pdf 的 read 面）。
//
// 为什么手写：本侧车钉了「零第三方依赖」（go.mod / go.sum 一字不变，AC-E4-4 可机械
// 验证），PDF 解析库一个都不能引。而读 PDF 这件事没法绕——ext.pdf 摆在能力目录里
// 却不读 PDF，模型面对「总结这个 PDF」只能猜（实测赌出 ext.pdf 又执行失败，
// 2026-10-20 E2E）。名字即能力，读了才不怕猜。
//
// 覆盖范围（按实测优先级，不是按 PDF 规范全书）：
//  1. 对象扫描不信任 xref：直接扫 "N 0 obj"，xref 被增量更新搞乱也能读
//     （pdfcpu 等同理；xref 只用于校验，不用于定位）。
//  2. 流解压 FlateDecode：先按 zlib（带 Adler 尾）试，失败再按 raw deflate——
//     现实中真有一批生成器漏写 zlib 头，只认一种等于把好 PDF 判死刑。
//  3. 对象流 /Type /ObjStm：现代 PDF（Word 2016+ 等）把对象塞进压缩流里，
//     不展开就看不到 Font / Page / Contents。
//  4. 内容流词法：BT/ET、Tj / TJ / ' / " 四类文本显示操作、字串转义、八进制。
//  5. 编码两条路：Type0+Identity-H（双字节码，查 ToUnicode CMap）与简单字体
//     （单字节，标准编码直接是 Latin-1 码位）。ToUnicode 解析 bfchar + bfrange。
//  6. 行结构启发：Td/TD/T*/Tm 的 y 变化插换行，TJ 数组里 < -100 的调整量当空格。
//
// 明确不覆盖且**报错不静默**的：加密 PDF（/Encrypt）、Type0 字体没有 ToUnicode
// 映射（查不出字的 PDF，吐出来只会是一串乱码——那比报错坏得多）。

import (
	"bytes"
	"compress/flate"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
)

const maxPDFBytes = 64 << 20 // 64MB：再大的 PDF 不该走这条路（先切分）

const (
	errBadPDF       = "bad_pdf"
	errPDFEncrypted = "pdf_encrypted"
	errNoText       = "no_text"
)

// pdfReadResult 是一页抽出来的文本。
type pdfReadResult struct {
	Pages []pdfPageText
	Fonts []string // 文件里出现的字体名（去重升序）——调用方看得见「读的是哪颗字体」
	CID   bool     // 是否存在 Identity-H 双字节编码的页面
	// Unreliable 是「文本流里实际掉了字」的页码（1-based）。判定只看 writeGlyphs 真实
	// 丢弃的字形数（Identity-H 无 ToUnicode、或有 ToUnicode 的字体里个别码查不到映射），
	// **不看资源表里有没有不可靠字体**——后者会把只列了字体却没真用的页误报整页标红。
	// 掉字的页仍会尽量抽其余可读文本（不会整页留空），只是文本可能不完整；由调用方
	// （尤其是 agent）据 note 决定要不要换路线（OCR / 问用户要文本）。
	// 整页完全没用任何字体（纯图片扫描件）不算 Unreliable——那种属于扫描件，看 allEmpty 判定。
	Unreliable []int
}

type pdfPageText struct {
	Page int    `json:"page"`
	Text string `json:"text"`
}

// ---- 第一层：对象池 ----

type pdfObj struct {
	id   int
	body []byte // "N 0 obj" 之后到 "endobj" 之前（流对象含 <<dict>> stream\n<data> endstream）
}

// scanPDFObjects 扫出全部间接对象，展开 ObjStm。
//
// 不用 xref 定位是刻意的：xref 偏移在增量更新（线性化 / 数字签名后的 PDF）里经常
// 与真实布局脱节，而「扫 obj」对任何合规 PDF 都成立。扫不到的对象宁缺勿错——
// 缺的对象在装配阶段以「引用落空」的形式报出来。
func scanPDFObjects(data []byte) (map[int]*pdfObj, error) {
	objs := map[int]*pdfObj{}
	rest := data
	for {
		i := bytes.Index(rest, []byte(" obj"))
		if i < 0 {
			break
		}
		// 对象头是 "N G obj"（对象号 + 生成号）。" obj" 前面贴着的是**生成号** G，
		// 再往前越过空白才是对象号 N——只看一个数字会把 G 当成 N（生成号恒 0 时
		// 全部对象被判非法而丢弃）。
		j := i
		for j > 0 && (rest[j-1] == ' ' || rest[j-1] == '\r' || rest[j-1] == '\n') {
			j--
		}
		gEnd := j
		gStart := gEnd
		for gStart > 0 && rest[gStart-1] >= '0' && rest[gStart-1] <= '9' {
			gStart--
		}
		if gStart == gEnd {
			rest = rest[i+4:]
			continue // "obj" 前面不是数字：内容流里的字面量，跳过
		}
		nEnd := gStart
		for nEnd > 0 && (rest[nEnd-1] == ' ' || rest[nEnd-1] == '\r' || rest[nEnd-1] == '\n') {
			nEnd--
		}
		nStart := nEnd
		for nStart > 0 && rest[nStart-1] >= '0' && rest[nStart-1] <= '9' {
			nStart--
		}
		if nStart == nEnd {
			rest = rest[i+4:]
			continue
		}
		id, err := strconv.Atoi(string(rest[nStart:nEnd]))
		if err != nil || id <= 0 {
			rest = rest[i+4:]
			continue
		}
		// 对象体：从 "N G obj" 之后到下一个 "endobj"
		bodyStart := i + 4
		for bodyStart < len(rest) && (rest[bodyStart] == '\r' || rest[bodyStart] == '\n') {
			bodyStart++
		}
		end := bytes.Index(rest[bodyStart:], []byte("endobj"))
		if end < 0 {
			break // 截断的文件：已有对象照用，后面的不要了
		}
		if _, dup := objs[id]; !dup {
			objs[id] = &pdfObj{id: id, body: rest[bodyStart : bodyStart+end]}
		}
		rest = rest[bodyStart+end+6:]
	}
	if len(objs) == 0 {
		return nil, fmt.Errorf("扫不到任何间接对象")
	}
	// 展开对象流：把 ObjStm 解压后的对象并进池子（就地合并，冲突时保留先扫到的）
	for _, o := range objs {
		dict, _, ok := splitObjStream(o.body)
		if !ok || !dictHas(dict, "/ObjStm") {
			continue
		}
		n := int(dictInt(dict, "/N", 0))
		first := int(dictInt(dict, "/First", 0))
		raw, ok := streamData(o.body)
		if !ok || n <= 0 || first <= 0 {
			continue
		}
		plain, err := inflateAny(raw)
		if err != nil {
			continue
		}
		if first > len(plain) {
			continue
		}
		hdr := strings.Fields(string(plain[:first]))
		if len(hdr) < 2*n {
			continue // 头不完整：这份 ObjStm 不认，别把错编号的对象混进池子
		}
		body := plain[first:]
		for idx := 0; idx < n; idx++ {
			num, err := strconv.Atoi(hdr[2*idx])
			if err != nil || num <= 0 {
				continue
			}
			start := 0
			if v, err := strconv.Atoi(hdr[2*idx+1]); err == nil {
				start = v
			}
			end := len(body)
			if idx+1 < n {
				if v, err := strconv.Atoi(hdr[2*idx+3]); err == nil && v >= start && v <= len(body) {
					end = v
				}
			}
			if start >= end {
				continue
			}
			if _, dup := objs[num]; !dup {
				objs[num] = &pdfObj{id: num, body: body[start:end]}
			}
		}
	}
	return objs, nil
}

// splitObjStream 把一个对象体拆成「字典部分」和「stream 关键字位置之后的数据」。
func splitObjStream(body []byte) (dict []byte, streamStart int, ok bool) {
	di := bytes.Index(body, []byte("<<"))
	if di < 0 {
		return nil, 0, false
	}
	si := bytes.Index(body, []byte("stream"))
	if si < 0 || si < di {
		return body[di:], 0, false
	}
	// "stream" 必须紧邻 CRLF 或 LF；出现在字典字符串里的不算
	after := si + len("stream")
	if after < len(body) && body[after] == '\r' {
		after++
	}
	if after < len(body) && body[after] == '\n' {
		after++
	}
	return body[di:si], after, true
}

// streamData 取出流对象的原始字节（到 endstream 为止）。
func streamData(body []byte) ([]byte, bool) {
	_, start, ok := splitObjStream(body)
	if !ok || start <= 0 {
		return nil, false
	}
	rest := body[start:]
	ei := bytes.Index(rest, []byte("endstream"))
	if ei < 0 {
		return rest, true
	}
	out := rest[:ei]
	out = bytes.TrimSuffix(out, []byte("\n"))
	out = bytes.TrimSuffix(out, []byte("\r"))
	return out, true
}

// inflateAny 解压 FlateDecode 流：先 zlib（含 Adler-32 尾），失败按 raw deflate。
func inflateAny(raw []byte) ([]byte, error) {
	if zr, err := zlib.NewReader(bytes.NewReader(raw)); err == nil {
		defer zr.Close()
		var out bytes.Buffer
		if _, err := out.ReadFrom(zr); err == nil {
			return out.Bytes(), nil
		}
	}
	fr := flate.NewReader(bytes.NewReader(raw))
	defer fr.Close()
	var out bytes.Buffer
	if _, err := out.ReadFrom(fr); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// ---- 字典 / 引用的小工具 ----

// dictHas / dictInt 只认「键名作为独立 token 出现」——写在字符串里的 /ObjStm 不算。
func dictHas(dict []byte, key string) bool {
	return dictFind(dict, key) >= 0
}

func dictFind(dict []byte, key string) int {
	from := 0
	for {
		i := bytes.Index(dict[from:], []byte(key))
		if i < 0 {
			return -1
		}
		at := from + i
		prev := byte(' ')
		if at > 0 {
			prev = dict[at-1]
		}
		next := byte(' ')
		if at+len(key) < len(dict) {
			next = dict[at+len(key)]
		}
		if !isDelim(prev) && !isNameByte(next) && !isDelim(next) {
			// 前缀不是定界符 ⇒ 是更长名字的一部分（如 /ObjStmX）
			from = at + len(key)
			continue
		}
		return at
	}
}

func isDelim(c byte) bool {
	switch c {
	case ' ', '\r', '\n', '\t', '<', '>', '(', ')', '[', ']', '/', '%':
		return true
	}
	return false
}

func isNameByte(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// dictInt 取 /Key 后面的整数值（顺带跳过 R 引用里的生成号）。
func dictInt(dict []byte, key string, def int64) int64 {
	at := dictFind(dict, key)
	if at < 0 {
		return def
	}
	s := string(dict[at+len(key):])
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return def
	}
	v, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return def
	}
	return v
}

// dictRef 取 /Key N 0 R 形式的引用，返回 (N, ok)。
func dictRef(dict []byte, key string) (int, bool) {
	at := dictFind(dict, key)
	if at < 0 {
		return 0, false
	}
	s := string(dict[at+len(key):])
	fields := strings.Fields(s)
	if len(fields) < 3 || fields[1] != "0" || fields[2] != "R" {
		return 0, false
	}
	n, err := strconv.Atoi(fields[0])
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// dictRefs 取 /Key 后的引用列表：既支持 [N 0 R M 0 R] 数组，也支持单个 N 0 R。
//
// 两种形态在真实 PDF 里都常见（多内容流的页面是数组，单流是裸引用）——
// 只认一种的结果是「页面看起来没问题但一个字节都取不到」。
func dictRefs(dict []byte, key string) []int {
	at := dictFind(dict, key)
	if at < 0 {
		return nil
	}
	s := strings.TrimSpace(string(dict[at+len(key):]))
	if strings.HasPrefix(s, "[") {
		rb := strings.IndexByte(s, ']')
		if rb < 0 {
			return nil
		}
		var out []int
		fields := strings.Fields(s[1:rb])
		for i := 0; i+2 < len(fields); i += 3 {
			if fields[i+1] != "0" || fields[i+2] != "R" {
				continue
			}
			if n, err := strconv.Atoi(fields[i]); err == nil && n > 0 {
				out = append(out, n)
			}
		}
		return out
	}
	// 单个引用：N 0 R
	fields := strings.Fields(s)
	if len(fields) >= 3 && fields[1] == "0" && fields[2] == "R" {
		if n, err := strconv.Atoi(fields[0]); err == nil && n > 0 {
			return []int{n}
		}
	}
	return nil
}

// dictString 取 /Key 后面的字符串值：既认 (literal)，也认 <hex>，还认 /Name。
//
// 三形态都要：/BaseFont 最常见的是名字（/BaseFont /SimSun），而嵌入子集字体的
// 描述有时写字面串。只认前两种的结果是「字体明明在，名字取不到」。
func dictString(dict []byte, key string) string {
	at := dictFind(dict, key)
	if at < 0 {
		return ""
	}
	s := dict[at+len(key):]
	i := 0
	for i < len(s) && (s[i] == ' ' || s[i] == '\r' || s[i] == '\n' || s[i] == '\t') {
		i++
	}
	if i < len(s) && s[i] == '/' { // 名字：/Helvetica → "Helvetica"
		j := i + 1
		for j < len(s) && !isDelim(s[j]) {
			j++
		}
		return string(s[i+1 : j])
	}
	open := bytes.IndexByte(s, '(')
	if open < 0 {
		open = bytes.IndexByte(s, '<')
		if open < 0 {
			return ""
		}
		close := bytes.IndexByte(s[open:], '>')
		if close < 0 {
			return ""
		}
		return decodeHexString(s[open+1 : open+close])
	}
	// 字面串：带嵌套括号与转义
	depth := 1
	var out []byte
	for i := open + 1; i < len(s); i++ {
		c := s[i]
		if c == '\\' && i+1 < len(s) {
			i++
			out = append(out, unescapeByte(s[i])...)
			continue
		}
		if c == '(' {
			depth++
		}
		if c == ')' {
			depth--
			if depth == 0 {
				break
			}
		}
		out = append(out, c)
	}
	return string(out)
}

// unescapeByte 解 PDF 字面串转义（含八进制 \ddd）。
func unescapeByte(c byte) []byte {
	switch c {
	case 'n':
		return []byte{'\n'}
	case 'r':
		return []byte{'\r'}
	case 't':
		return []byte{'\t'}
	case 'b':
		return []byte{'\b'}
	case 'f':
		return []byte{'\f'}
	case '(', ')', '\\':
		return []byte{c}
	}
	if c >= '0' && c <= '7' {
		return []byte{c}
	}
	return nil
}

func decodeHexString(b []byte) string {
	if len(b)%2 == 1 {
		b = append(b, '0')
	}
	out := make([]byte, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		v, err := strconv.ParseUint(string(b[i:i+2]), 16, 8)
		if err != nil {
			continue
		}
		out = append(out, byte(v))
	}
	return string(out)
}

// ---- 第二层：文档装配 ----

// pdfDocView 是「已解压、已编号」的对象池视图。
type pdfDocView struct {
	objs    map[int]*pdfObj
	trailer []byte // 文件尾的 trailer 字典（或 XRef 流字典——前者不在任何对象里，必须单独取）
}

func newPDFDocView(data []byte) (*pdfDocView, error) {
	if !bytes.HasPrefix(data, []byte("%PDF-")) {
		return nil, fmt.Errorf("文件头不是 %%PDF-")
	}
	objs, err := scanPDFObjects(data)
	if err != nil {
		return nil, err
	}
	for _, o := range objs {
		dict, _, _ := splitObjStream(o.body)
		if dictHas(dict, "/Encrypt") {
			return nil, fmt.Errorf("加密 PDF：本提取器不支持（/Encrypt 存在）")
		}
	}
	view := &pdfDocView{objs: objs}
	// trailer 取文件里**最后一个**（增量更新时最后一份为准）。它在 xref 之后，
	// 不属于任何间接对象——只扫对象池永远找不到它。
	if ti := bytes.LastIndex(data, []byte("trailer")); ti >= 0 {
		view.trailer = sliceDict(data[ti+len("trailer"):])
	}
	// 兜底：交叉引用流（/Type /XRef）把 trailer 信息塞进对象字典
	if !dictHas(view.trailer, "/Root") {
		for _, o := range objs {
			dict, _, _ := splitObjStream(o.body)
			if dictHas(dict, "/XRef") && dictHas(dict, "/Root") {
				view.trailer = dict
				break
			}
		}
	}
	return view, nil
}

func (v *pdfDocView) get(id int) *pdfObj { return v.objs[id] }

func (v *pdfDocView) dictOf(id int) []byte {
	if o := v.objs[id]; o != nil {
		dict, _, _ := splitObjStream(o.body)
		return dict
	}
	return nil
}

// pages 按文档顺序列出页面对象 id（Kids 递归，/Count 只用于校验不缺页）。
func (v *pdfDocView) pages() ([]int, error) {
	rootID, ok := dictRef(v.trailer, "/Root")
	if !ok {
		return nil, fmt.Errorf("trailer 里没有 /Root")
	}
	cat := v.dictOf(rootID)
	if cat == nil {
		return nil, fmt.Errorf("Catalog 对象 %d 不存在", rootID)
	}
	pagesID, ok := dictRef(cat, "/Pages")
	if !ok {
		return nil, fmt.Errorf("Catalog 里没有 /Pages")
	}
	var walk func(id int, depth int) ([]int, error)
	walk = func(id int, depth int) ([]int, error) {
		if depth > 64 { // Pages 树病态嵌套：到此为止，别把自己绕死
			return nil, fmt.Errorf("Pages 树嵌套超过 64 层")
		}
		dict := v.dictOf(id)
		if dict == nil {
			return nil, fmt.Errorf("Pages 节点 %d 不存在", id)
		}
		kids := dictRefs(dict, "/Kids")
		if len(kids) > 0 {
			var out []int
			for _, k := range kids {
				sub, err := walk(k, depth+1)
				if err != nil {
					return nil, err
				}
				out = append(out, sub...)
			}
			return out, nil
		}
		return []int{id}, nil // 叶子：它就是 Page
	}
	return walk(pagesID, 0)
}

// sliceDict 从 data 开头（跳过空白）配平切出一个 <<...>> 字典。
// 字符串里的 >> 不算结束——字典值可以是 (...) 字面串。
func sliceDict(data []byte) []byte {
	from := 0
	for from < len(data) && (data[from] == ' ' || data[from] == '\r' || data[from] == '\n' || data[from] == '\t') {
		from++
	}
	if from+1 >= len(data) || data[from] != '<' || data[from+1] != '<' {
		return nil
	}
	depth := 0
	i := from
	for i < len(data) {
		switch {
		case data[i] == '(':
			i = skipLiteral(data, i) // 字符串内容不是字典结构
			continue
		case i+1 < len(data) && data[i] == '<' && data[i+1] == '<':
			depth++
			i += 2
			continue
		case i+1 < len(data) && data[i] == '>' && data[i+1] == '>':
			depth--
			i += 2
			if depth == 0 {
				return data[from:i]
			}
			continue
		}
		i++
	}
	return nil
}

// skipLiteral 返回 data[i]=='(' 处字面串结束后的下一个位置。
func skipLiteral(data []byte, i int) int {
	depth := 0
	for i < len(data) {
		switch data[i] {
		case '\\':
			i += 2
			continue
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i + 1
			}
		}
		i++
	}
	return i
}

// ---- 第三层：字体表 ----

// pdfFont 把一个 resource 名字（/F1）映到解码函数。
type pdfFont struct {
	name     string
	identity bool              // Identity-H：双字节码
	toUni    map[uint32]string // 码 → Unicode（bfchar / bfrange 展开）
}

// buildFonts 解析一页 /Resources << /Font << /F1 ... >> >> 下的全部字体。
func (v *pdfDocView) buildFonts(pageDict []byte) (map[string]*pdfFont, error) {
	out := map[string]*pdfFont{}
	fi := dictFind(pageDict, "/Font")
	if fi < 0 {
		return out, nil // 没字体的页面：无文本可抽，但不是错误
	}
	// 只取 /Font 紧随其后的 <<...>> 子字典——不能贪心扫到 page dict 的其余部分
	// （/Contents N 0 R 等同级键会被 fontEntries 误当成字体条目，污染 Fonts 列表，
	// 且把内容流对象当字体解析，纯属浪费）。
	seg := sliceDict(pageDict[fi+len("/Font"):])
	if seg == nil {
		return out, nil
	}
	// 注意起点在 /Font 子字典**之内**：fontEntries 按 "/名字 N 0 R" 切条，
	// 从键名开始会把 "/Font << /F1 4 0 R" 当成一条，名字解析成 "/Font"。
	for _, entry := range fontEntries(seg) {
		name := fontEntryName(entry)
		if name == "" {
			continue
		}
		// entry 形如 "/F1 5 0 R"：名字与引用号连写，直接按字段取（不能走
		// dictRef(entry, "/F")——那会把 "/F" 匹配到 "/F1" 上且字段切分错位）。
		fields := strings.Fields(strings.TrimPrefix(string(entry), name))
		if len(fields) < 3 || fields[1] != "0" || fields[2] != "R" {
			continue
		}
		id, err := strconv.Atoi(fields[0])
		if err != nil || id <= 0 {
			continue
		}
		fdict := v.dictOf(id)
		if fdict == nil {
			continue
		}
		f := &pdfFont{toUni: map[uint32]string{}}
		f.name = strings.TrimPrefix(dictString(fdict, "/BaseFont"), "/")
		if f.name == "" {
			f.name = strings.TrimPrefix(name, "/") // Type3 等无 /BaseFont：用资源名兜底，报告不空
		}
		f.identity = dictHas(fdict, "/Identity-H")
		if tu, ok := dictRef(fdict, "/ToUnicode"); ok {
			if data, ok := streamData(v.get(tu).body); ok {
				if plain, err := inflateAny(data); err == nil {
					parseCMap(plain, f.toUni)
				} else {
					// 未压缩的 ToUnicode（无 /Filter）：直接当明文 CMap 解析，
					// 否则 subset 字体的码→字映射整张丢空，中文又变乱码。
					parseCMap(data, f.toUni)
				}
			}
		}
		out[name] = f
	}
	return out, nil
}

// fontEntries 把资源字典段切成 "/F1 N 0 R /F2 M 0 R" 的若干条。
func fontEntries(seg []byte) [][]byte {
	var out [][]byte
	rest := seg
	for {
		at := bytes.IndexByte(rest, '/')
		if at < 0 {
			break
		}
		end := bytes.Index(rest[at:], []byte(" 0 R"))
		if end < 0 {
			break
		}
		out = append(out, rest[at:at+end+4])
		rest = rest[at+end+4:]
	}
	return out
}

func fontEntryName(entry []byte) string {
	s := string(entry)
	sp := strings.IndexByte(s, ' ')
	if sp < 0 {
		return ""
	}
	return s[:sp] // "/F1"
}

// ---- 第四层：ToUnicode CMap ----

// parseCMap 解析 ToUnicode CMap 的 bfchar / bfrange，码 → Unicode 字符串。
//
// 只认本侧车生成与主流阅读器都写的形态：`beginbfchar\n<src> <dst>\nendbfchar`
// 与 `beginbfrange\n<lo> <hi> <dst>\n` / `[<d1> <d2> ...]`。dst 是 UTF-16BE
// 十六进制（非 BMP 是代理对）——utf16 解码后拼成一个 Go string。
func parseCMap(data []byte, out map[uint32]string) {
	text := string(data)
	lines := strings.Split(text, "\n")
	section := ""
	for _, ln := range lines {
		t := strings.TrimSpace(ln)
		switch {
		// 用 Contains 不用 HasPrefix：规范里 begin/end 标记行首带条目计数
		// （"2 beginbfchar"），HasPrefix 会整段匹配不上——CMap 静默解析成空表。
		case strings.Contains(t, "beginbfchar"):
			section = "char"
			continue
		case strings.Contains(t, "beginbfrange"):
			section = "range"
			continue
		case strings.Contains(t, "endbfchar"), strings.Contains(t, "endbfrange"), strings.Contains(t, "endcmap"):
			section = ""
			continue
		}
		if section == "" || t == "" || strings.HasPrefix(t, "%") {
			continue
		}
		fields := strings.Fields(t)
		switch section {
		case "char":
			if len(fields) < 2 {
				continue
			}
			src, ok1 := parseHexCode(fields[0])
			if ok1 {
				out[src] = decodeUTF16Units(parseHexUnits(fields[1]))
			}
		case "range":
			if len(fields) < 3 {
				continue
			}
			lo, ok1 := parseHexCode(fields[0])
			hi, ok2 := parseHexCode(fields[1])
			if !ok1 || !ok2 || hi < lo {
				continue
			}
			if strings.HasPrefix(fields[2], "[") {
				// 数组形式：逐个给目标
				rest := strings.TrimSuffix(strings.TrimPrefix(t[strings.Index(t, "["):], "["), "]")
				for i, hx := range strings.Fields(rest) {
					out[lo+uint32(i)] = decodeUTF16Units(parseHexUnits(hx))
				}
				continue
			}
			// 增量形式：目标码依次 +1（按 UTF-16 单元走，非 BMP 边界由 utf16 合成）
			dst := parseHexUnits(fields[2])
			if len(dst) == 0 {
				continue
			}
			for c := lo; c <= hi && c-lo < 0x10000; c++ {
				u := make([]uint16, len(dst))
				copy(u, dst)
				u[len(u)-1] += uint16(c - lo)
				out[c] = decodeUTF16Units(u)
			}
		}
	}
}

// parseHexCode 解 "<0001>" 或 "0001" 形式的十六进制码（src 码 / bfrange 边界）。
func parseHexCode(s string) (uint32, bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "<")
	s = strings.TrimSuffix(s, ">")
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseUint(s, 16, 32)
	if err != nil {
		return 0, false
	}
	return uint32(v), true
}

// parseHexUnits 把 UTF-16BE 十六进制串（如 "4E2D" 或代理对 "D840DC00"）解成码元序列。
//
// 步进是 **4 个 hex 字符一个码元**（16 位）——按 2 字符步进会把 "0042" 切成
// [0x00, 0x42]，产出的字符串前面多一个 NUL，而且整段文字全歪。
func parseHexUnits(s string) []uint16 {
	s = strings.TrimPrefix(strings.TrimSpace(s), "<")
	s = strings.TrimSuffix(s, ">")
	out := make([]uint16, 0, len(s)/4)
	for i := 0; i+3 < len(s); i += 4 {
		v, err := strconv.ParseUint(s[i:i+4], 16, 32)
		if err != nil {
			continue
		}
		out = append(out, uint16(v))
	}
	return out
}

// decodeUTF16Units 把 UTF-16 码元序列转成 Go string（代理对由 utf16 标准库合成）。
func decodeUTF16Units(u []uint16) string {
	if len(u) == 0 {
		return ""
	}
	return string(utf16.Decode(u))
}

// ---- 第五层：内容流词法 ----

// pdfLexer 是内容流的 token 机。
type pdfLexer struct {
	data []byte
	pos  int
}

type lexItem struct {
	kind byte // 's'=string 'n'=number 'v'=name 'A'=array 'd'=dict
	raw  []byte
}

// lexContent 把内容流切成 token 序列（含 [] 数组与 <<>> 字典的嵌套）。
func lexContent(data []byte) []lexItem {
	lx := &pdfLexer{data: data}
	var out []lexItem
	for {
		it, ok := lx.next()
		if !ok {
			return out
		}
		out = append(out, it)
	}
}

func (lx *pdfLexer) next() (lexItem, bool) {
	for lx.pos < len(lx.data) {
		c := lx.data[lx.pos]
		switch {
		case c == ' ' || c == '\r' || c == '\n' || c == '\t' || c == 0:
			lx.pos++
			continue
		case c == '%':
			for lx.pos < len(lx.data) && lx.data[lx.pos] != '\n' {
				lx.pos++
			}
			continue
		case c == '(':
			return lexItem{kind: 's', raw: lx.readLiteral()}, true
		case c == '<':
			if lx.pos+1 < len(lx.data) && lx.data[lx.pos+1] == '<' {
				return lx.readDict(), true
			}
			return lexItem{kind: 's', raw: lx.readHexStr()}, true
		case c == '>':
			if lx.pos+1 < len(lx.data) && lx.data[lx.pos+1] == '>' {
				lx.pos += 2
				return lexItem{kind: 'd'}, true // 字典结束标记（内容流里用不到内容）
			}
			lx.pos++
			continue
		case c == '[':
			lx.pos++
			return lexItem{kind: 'a'}, true
		case c == ']':
			lx.pos++
			return lexItem{kind: 'm'}, true // 数组结束标记
		case c == '/' || c >= '0' && c <= '9' || c == '+' || c == '-' || c == '.':
			return lx.readAtom(), true
		default:
			// 操作符或关键字：读到下一个定界符
			start := lx.pos
			for lx.pos < len(lx.data) && !isContentDelim(lx.data[lx.pos]) {
				lx.pos++
			}
			if lx.pos == start {
				lx.pos++ // 单字符定界符的兜底：别死循环
			}
			return lexItem{kind: 'o', raw: lx.data[start:lx.pos]}, true
		}
	}
	return lexItem{}, false
}

func isContentDelim(c byte) bool {
	switch c {
	case ' ', '\r', '\n', '\t', '(', ')', '<', '>', '[', ']', '/', '%', 0:
		return true
	}
	return false
}

func (lx *pdfLexer) readAtom() lexItem {
	start := lx.pos
	if lx.data[lx.pos] == '/' {
		lx.pos++
		for lx.pos < len(lx.data) && !isContentDelim(lx.data[lx.pos]) {
			lx.pos++
		}
		return lexItem{kind: 'v', raw: lx.data[start:lx.pos]} // /Name：Tf 的字体名走这条
	}
	for lx.pos < len(lx.data) && !isContentDelim(lx.data[lx.pos]) {
		lx.pos++
	}
	return lexItem{kind: 'n', raw: lx.data[start:lx.pos]}
}

func (lx *pdfLexer) readLiteral() []byte {
	lx.pos++ // '('
	var out []byte
	depth := 1
	for lx.pos < len(lx.data) {
		c := lx.data[lx.pos]
		if c == '\\' {
			lx.pos++
			if lx.pos >= len(lx.data) {
				break
			}
			e := lx.data[lx.pos]
			if e >= '0' && e <= '7' {
				oct := make([]byte, 0, 3)
				for len(oct) < 3 && lx.pos < len(lx.data) && lx.data[lx.pos] >= '0' && lx.data[lx.pos] <= '7' {
					oct = append(oct, lx.data[lx.pos])
					lx.pos++
				}
				v, _ := strconv.ParseUint(string(oct), 8, 16)
				out = append(out, byte(v))
				continue
			}
			if e == '\r' || e == '\n' { // 行延续：吃掉换行不产字
				lx.pos++
				continue
			}
			out = append(out, unescapeByte(e)...)
			lx.pos++
			continue
		}
		if c == '(' {
			depth++
		}
		if c == ')' {
			depth--
			if depth == 0 {
				lx.pos++
				return out
			}
		}
		out = append(out, c)
		lx.pos++
	}
	return out
}

func (lx *pdfLexer) readHexStr() []byte {
	lx.pos++ // '<'
	start := lx.pos
	for lx.pos < len(lx.data) && lx.data[lx.pos] != '>' {
		lx.pos++
	}
	raw := lx.data[start:min(lx.pos, len(lx.data))]
	lx.pos++ // '>'
	return []byte(decodeHexString(raw))
}

func (lx *pdfLexer) readDict() lexItem {
	depth := 0
	for lx.pos < len(lx.data) {
		if lx.pos+1 < len(lx.data) && lx.data[lx.pos] == '<' && lx.data[lx.pos+1] == '<' {
			depth++
			lx.pos += 2
			continue
		}
		if lx.pos+1 < len(lx.data) && lx.data[lx.pos] == '>' && lx.data[lx.pos+1] == '>' {
			depth--
			lx.pos += 2
			if depth == 0 {
				return lexItem{kind: 'd'}
			}
			continue
		}
		if lx.data[lx.pos] == '(' {
			lx.readLiteral() // 字典值里的字符串：跳过内容
			continue
		}
		lx.pos++
	}
	return lexItem{kind: 'd'}
}

// ---- 第六层：文本执行 ----

// extractPageText 跑一页内容流，按显示操作拼文本。
//
// 行结构用 y 坐标启发：文本定位（Td/TD/T*/Tm）把 y 挪出当前行时插 '\n'。
// TJ 数组里小于 -100 的水平调整量当空格——这是提取器的通行做法
// （poppler 对 kerning 大负值的处理同源），词与词之间没有它就粘成一团。
// extractPageText 跑一页内容流，按显示操作拼文本。
//
// 行结构用 y 坐标启发：文本定位（Td/TD/T*/Tm）把 y 挪出当前行时插 '\n'。
// TJ 数组里小于 -100 的水平调整量当空格——这是提取器的通行做法
// （poppler 对 kerning 大负值的处理同源），词与词之间没有它就粘成一团。
//
// 架构：token 顺序进操作数栈，'[' 压标记、']' 把标记之后的元素收成数组操作数；
// 操作符消费整个栈（PDF 的操作数不跨操作符累积）。**不能**边 range 边截断
// 底层切片——range 绑定的是原切片的头/长度，截断会让遍历跳位（第一版就这么
// 错的：TJ 数组整段被跳过，测试里 usedFont 直接是 false）。
func extractPageText(content []byte, fonts map[string]*pdfFont) (string, bool, int, int) {
	if len(content) == 0 {
		return "", false, 0, 0
	}
	tokens := lexContent(content)

	var sb strings.Builder  // 全文
	var cur strings.Builder // 当前行缓冲
	lastY := 0.0
	haveY := false
	curFont := ""
	usedFont := false
	dropped := 0 // 本页实际丢弃的字形数（见 writeGlyphs）
	shown := 0   // 本页试图显示的字形总数（用于算掉字占比）

	flushLine := func() {
		line := strings.TrimRight(cur.String(), " ")
		if line != "" {
			sb.WriteString(line)
			sb.WriteString("\n")
		}
		cur.Reset()
	}

	type operand struct {
		kind byte // 's'=字符串 'n'=数字 'v'=名字 'A'=数组 'a'=数组开始标记
		raw  []byte
		arr  []operand
	}
	stack := []operand{}
	// lastOf 从栈顶往下找指定种类的操作数（操作数栈里没有操作符，无需边界判断）。
	lastOf := func(kind byte) *operand {
		for i := len(stack) - 1; i >= 0; i-- {
			if stack[i].kind == kind {
				return &stack[i]
			}
		}
		return nil
	}
	// numAt(1) 是最后一个数字，numAt(2) 是倒数第二个（Tm 的 f 是第 6 个操作数）。
	numAt := func(fromEnd int) (float64, bool) {
		var vals []float64
		for i := len(stack) - 1; i >= 0; i-- {
			if stack[i].kind == 'n' {
				if v, err := strconv.ParseFloat(string(stack[i].raw), 64); err == nil {
					vals = append(vals, v)
				}
			}
		}
		if len(vals) < fromEnd {
			return 0, false
		}
		return vals[fromEnd-1], true
	}
	nameTop := func() string {
		for i := len(stack) - 1; i >= 0; i-- {
			if stack[i].kind == 'v' {
				return string(stack[i].raw)
			}
		}
		return ""
	}
	writeRaw := func(raw []byte) {
		d, s := writeGlyphs(&cur, raw, fonts, curFont)
		dropped += d
		shown += s
		usedFont = true
	}

	for _, t := range tokens {
		switch t.kind {
		case 's', 'n', 'v':
			stack = append(stack, operand{kind: t.kind, raw: t.raw})
			continue
		case 'a':
			stack = append(stack, operand{kind: 'a'})
			continue
		case 'm':
			// 收数组：找最近的 'a' 标记，把它之后的元素并成数组
			at := -1
			for i := len(stack) - 1; i >= 0; i-- {
				if stack[i].kind == 'a' {
					at = i
					break
				}
			}
			if at < 0 {
				continue // 没有匹配的 '['：病态内容流，丢弃这个 ']'
			}
			items := append([]operand(nil), stack[at+1:]...)
			stack = append(stack[:at], operand{kind: 'A', arr: items})
			continue
		case 'd':
			continue // 内容流里的字典：跳过
		}

		switch string(t.raw) {
		case "BT":
			// 文本对象开始：y 基线重置，防止上一对象的行尾状态串到下一个文本对象
			haveY = false
		case "ET":
			flushLine()
		case "Tf":
			if n := nameTop(); n != "" {
				curFont = n
			}
		case "Td", "TD":
			if y, ok := numAt(1); ok {
				if haveY && y != lastY {
					flushLine()
				}
				lastY, haveY = y, true
			}
		case "T*":
			flushLine()
		case "Tm":
			if y, ok := numAt(2); ok { // Tm 是 a b c d e f：e=第5, f=第6 → 倒数第 2 个
				if haveY && y != lastY {
					flushLine()
				}
				lastY, haveY = y, true
			}
		case "Tj", "'", "\"":
			if s := lastOf('s'); s != nil {
				writeRaw(s.raw)
			}
			if string(t.raw) == "'" {
				flushLine() // ' = 换行后显示
			}
		case "TJ":
			if a := lastOf('A'); a != nil {
				for _, el := range a.arr {
					switch el.kind {
					case 's':
						writeRaw(el.raw)
					case 'n':
						if v, err := strconv.ParseFloat(string(el.raw), 64); err == nil && v < -100 {
							cur.WriteByte(' ') // 大负调整量 = 词间空格
						}
					}
				}
			}
		}
		stack = stack[:0] // 操作数不跨操作符
	}
	flushLine()
	if sb.Len() == 0 {
		return "", usedFont, dropped, shown
	}
	return strings.TrimRight(sb.String(), "\n"), true, dropped, shown
}

// writeGlyphs 把一段显示字符串（raw）按字体解码写入 sb，返回 (dropped, shown)：
// dropped = 实际丢弃（抽不出可靠 Unicode）的字形数；shown = 本段试图显示的字形总数。
//
// 丢弃只发生在两种「抽不出可靠 Unicode」的情形：
//  1. Identity-H 双字节码但无 ToUnicode——整段吞掉，按 CID 码元计掉字；
//  2. 有 ToUnicode 的字体里个别码查不到映射（子集字体漏映的码元）——落成 0xFFFD 记一次。
//
// shown 用来算「掉字占比」：掉一两个字（shown 很大）不该把整页判不可靠，掉了半页才该标。
// 这是「该页确实掉字了吗、掉了几成」的唯一真相来源：Unreliable 据此判断，
// 不再看「资源表里有没有不可靠字体」——后者会把只列了字体却没真用的页误报整页标红。
func writeGlyphs(sb *strings.Builder, raw []byte, fonts map[string]*pdfFont, fontName string) (dropped int, shown int) {
	f := fonts[fontName]
	if f == nil {
		f = fonts["/F1"]
	}
	if f == nil {
		// 字体未知（没在资源表里）：没有可靠解码手段，按 Latin-1 兜底（数字/英文/标点
		// 一般 1:1，中文会失真），至少不崩；下游看到非预期字符也能察觉而非空白。
		sb.Write(raw)
		return 0, len(raw)
	}
	if f.identity {
		// 2 字节 CID（Identity-H）：必须有 ToUnicode 才解码，否则吞掉不吐乱码。
		units := len(raw) / 2
		if len(f.toUni) == 0 {
			return units, units // 整段按 CID 码元计掉字
		}
		dropped := 0
		for i := 0; i+1 < len(raw); i += 2 {
			code := uint32(binary.BigEndian.Uint16(raw[i:]))
			if s, ok := f.toUni[code]; ok {
				sb.WriteString(s)
			} else {
				sb.WriteRune(0xFFFD)
				dropped++
			}
		}
		return dropped, units
	}
	// 1 字节编码（Type3 / 简单字体 / 子集字体都走这）：
	// 有 ToUnicode 就逐字节查（子集/Type3 自定义编码，原样是乱码——本用户 PDF 就是这个坑）；
	// 没有 ToUnicode 的标准字体（WinAnsi/Latin-1）才原样即可读。
	if len(f.toUni) > 0 {
		dropped := 0
		for _, b := range raw {
			if s, ok := f.toUni[uint32(b)]; ok {
				sb.WriteString(s)
			} else if b < 0x80 {
				sb.WriteByte(b) // 查不到但属 ASCII：数字/标点通常 1:1，照写
			} else {
				sb.WriteRune(0xFFFD) // 子集自定义编码里查不到的高字节：替换，绝不污染 UTF-8 流
				dropped++
			}
		}
		return dropped, len(raw)
	}
	sb.Write(raw) // 标准字体：WinAnsi ≈ Latin-1
	return 0, len(raw)
}

// ---- 顶层 ----

// extractPDFText 抽取整份 PDF 的文本。
//
// maxPages 上限之外的页不抽（巨 PDF 的护栏）；截断时 Note 里说明，不静默。
func extractPDFText(data []byte, maxPages int) (*pdfReadResult, error) {
	view, err := newPDFDocView(data)
	if err != nil {
		return nil, err
	}
	pageIDs, err := view.pages()
	if err != nil {
		return nil, err
	}
	res := &pdfReadResult{}
	fontSet := map[string]bool{}
	for idx, pid := range pageIDs {
		if idx >= maxPages {
			break
		}
		dict := view.dictOf(pid)
		if dict == nil {
			continue
		}
		fonts, ferr := view.buildFonts(dict)
		if ferr != nil {
			return nil, ferr
		}
		// 资源表里是否有 Identity-H 字体（仅用于 res.CID 这个**信息性**标记，
		// 不影响 Unreliable 判定——判定只看「文本流里实际掉了几个字」）。
		for _, f := range fonts {
			if f.name != "" {
				fontSet[f.name] = true
			}
			if f.identity {
				res.CID = true
			}
		}
		var content []byte
		for _, cid := range dictRefs(dict, "/Contents") {
			if raw, ok := streamData(view.get(cid).body); ok {
				if plain, err := inflateAny(raw); err == nil {
					content = append(content, plain...)
					content = append(content, '\n')
				} else {
					content = append(content, raw...)
					content = append(content, '\n')
				}
			}
		}
		if len(content) == 0 {
			res.Pages = append(res.Pages, pdfPageText{Page: idx + 1, Text: ""})
			continue
		}
		text, _, dropped, shown := extractPageText(content, fonts)
		// 掉字占比：dropped 占 shown 的比例。少数几个字形掉（比如一页几百字里掉两三个）
		// 不该把整页判不可靠——那会让调用方（尤其是 agent）把已抽出的好文本一并丢弃。
		// 掉了「过半」才算整页不可靠；少量缺字则保留文本、不标红。
		ratio := 0.0
		if shown > 0 {
			ratio = float64(dropped) / float64(shown)
		}
		const unreliableRatio = 0.5
		isUnreliable := dropped > 0 && (shown == 0 || ratio >= unreliableRatio)
		if strings.TrimSpace(text) == "" {
			// 文本全空：要么字体用了却抽不出（dropped>0），要么压根没有文本操作
			// （纯图片扫描件）。前者标 Unreliable，后者交给 allEmpty
			// 分支按扫描件说明——两者都不吐乱码。
			if isUnreliable {
				res.Unreliable = append(res.Unreliable, idx+1)
			}
			res.Pages = append(res.Pages, pdfPageText{Page: idx + 1, Text: ""})
			continue
		}
		// 抽出了大量可读文本，但本页掉字占比过半 → 标这一页供调用方知悉文本不完整；
		// 仅少量缺字则不标（保留好文本，agent 可照常总结）。
		if isUnreliable {
			res.Unreliable = append(res.Unreliable, idx+1)
		}
		res.Pages = append(res.Pages, pdfPageText{Page: idx + 1, Text: text})
	}
	if len(res.Pages) == 0 {
		return nil, fmt.Errorf("一个页面都没解析出来")
	}
	for name := range fontSet {
		res.Fonts = append(res.Fonts, name)
	}
	sort.Strings(res.Fonts)
	allEmpty := true
	for _, p := range res.Pages {
		if strings.TrimSpace(p.Text) != "" {
			allEmpty = false
			break
		}
	}
	if allEmpty {
		if len(res.Unreliable) > 0 {
			// 文档用了字体、但每页都抽不出（无 ToUnicode 的 CID 落不到 Unicode）。这是 fail-closed 的
			// 硬失败：返回空文本会让调用方以为「读到了一份空白文档」而拿空文本硬总结，比明确报错更糟。
			return nil, fmt.Errorf("所有页面都用了无 ToUnicode 的字体（CID 落不到 Unicode），无法可靠提取" +
				"文本（硬抽是一串乱码）；请用带文本层的 PDF，或先做 OCR / 问对方要文本版")
		}
		return nil, fmt.Errorf("所有页面都没抽出文本：可能是扫描件（图片型 PDF，需 OCR），或文本被特殊编码遮蔽")
	}
	return res, nil
}

// readPDFFile 读文件并提取文本（带大小护栏）。
func readPDFFile(path string, maxPages int) (*pdfReadResult, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if fi.Size() > maxPDFBytes {
		return nil, fmt.Errorf("%s 有 %d 字节，超过 %d 上限：请先切分", path, fi.Size(), maxPDFBytes)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return extractPDFText(data, maxPages)
}
