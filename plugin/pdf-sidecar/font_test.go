package main

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// 本文件用一个**手工搭出来的最小 TTF** 当夹具。
//
// 为什么不用系统字体：单测必须能在没有 simhei.ttf 的机器上跑（CI 多半是 Linux，
// 而且没有 Windows 字体）。而手搭夹具的好处是能**故意造出**那些在真字体里很难碰到的
// 形状——比如「复合字形套复合字形」与「字形自引用」，它们正是子集化最容易出错的地方。
//
// 字体参数刻意简单：upem=1000，7 个字形。码位安排如下（后面几个用例依赖这些编号）：
//
//	gid 0  .notdef（空）
//	gid 1  'A'      简单字形
//	gid 2  'B'      简单字形
//	gid 3  ' '      空格（无轮廓，但有步进宽度）
//	gid 4  '復'     复合字形 → 依赖 gid 6
//	gid 5  '漢'     嵌套复合 → 依赖 gid 4（于是绕一圈依赖到 gid 6）
//	gid 6  （部件）简单字形，没有独立码位——它只能被别人引用
const (
	fxUpem = 1000
)

// int16YMin 是字体头里 yMin 的负值（-200）。单独拎成变量，是因为把它塞进
// PutUint16 必须走**运行时**转换才能按补码重解释成 uint16——直接在字面量上
// `uint16(-200)` 会被 Go 当作「常量 -200 不落在 uint16 区间内」而拒。
var int16YMin int16 = -200

type fixtureOpt struct {
	// selfCycle 让 gid 5 的组件指向自己（字体自引用）。合法字体不该有这种东西，
	// 但**不能因为它而挂住**：那是 DoS 而不是报错。
	selfCycle bool
}

// ---- 夹具的各个表 ----

func fxHead() []byte {
	b := make([]byte, 54)
	binary.BigEndian.PutUint32(b[0:], 0x00010000)         // version
	binary.BigEndian.PutUint32(b[8:], 0)                  // checkSumAdjustment（后面回填）
	binary.BigEndian.PutUint32(b[12:], 0x5F0F3CF5)        // magicNumber
	binary.BigEndian.PutUint16(b[18:], fxUpem)            // unitsPerEm
	binary.BigEndian.PutUint16(b[36:], 0)                 // xMin
	binary.BigEndian.PutUint16(b[38:], uint16(int16YMin)) // yMin（负数：必须走运行时转换才能按补码重解释）
	binary.BigEndian.PutUint16(b[40:], 1000)              // xMax
	binary.BigEndian.PutUint16(b[42:], 800)               // yMax
	binary.BigEndian.PutUint16(b[50:], 1)                 // indexToLocFormat = long
	return b
}

func fxHhea() []byte {
	b := make([]byte, 36)
	binary.BigEndian.PutUint32(b[0:], 0x00010000)
	binary.BigEndian.PutUint16(b[4:], 800) // ascender
	binary.BigEndian.PutUint16(b[6:], 200) // descender（正数写法：字距解释与本测试无关）
	binary.BigEndian.PutUint16(b[34:], 7)  // numberOfHMetrics
	return b
}

func fxMaxp() []byte {
	b := make([]byte, 6)
	binary.BigEndian.PutUint32(b[0:], 0x00005000) // version 0.5（只带 numGlyphs 的那种）
	binary.BigEndian.PutUint16(b[4:], 7)
	return b
}

// fxHmtx 给每个字形一个步进宽度（单位是 upem 的 1/1000）。
func fxHmtx() []byte {
	adv := []uint16{600, 600, 600, 300, 1000, 1000, 500}
	out := make([]byte, 0, 4*len(adv))
	for _, a := range adv {
		out = binary.BigEndian.AppendUint16(out, a)
		out = binary.BigEndian.AppendUint16(out, 0) // lsb
	}
	return out
}

// fxSimpleGlyph 造一个有四个点的简单字形（轮廓形状本身不重要，能解析就行）。
func fxSimpleGlyph(base int16) []byte {
	out := binary.BigEndian.AppendUint16(nil, 1) // numberOfContours = 1
	out = binary.BigEndian.AppendUint16(out, uint16(base))
	out = binary.BigEndian.AppendUint16(out, uint16(base))
	out = binary.BigEndian.AppendUint16(out, uint16(base+500))
	out = binary.BigEndian.AppendUint16(out, uint16(base+700))
	out = binary.BigEndian.AppendUint16(out, 3) // endPtsOfContours[0] = 3（4 个点）
	out = binary.BigEndian.AppendUint16(out, 0) // instructionLength
	for i := 0; i < 4; i++ {
		out = append(out, 0x01) // flags = ON_CURVE（坐标用 int16，不是短格式）
		out = binary.BigEndian.AppendUint16(out, uint16(base+int16(i)*100))
		out = binary.BigEndian.AppendUint16(out, uint16(base+int16(i)*50))
	}
	return out
}

// fxCompositeGlyph 造一个引用 ref 的复合字形（ARGS_ARE_XY_VALUES：参数是两个字节）。
func fxCompositeGlyph(ref uint16) []byte {
	out := binary.BigEndian.AppendUint16(nil, 0xFFFF) // numberOfContours = -1 ⇒ 复合
	out = binary.BigEndian.AppendUint16(out, 0)
	out = binary.BigEndian.AppendUint16(out, 0)
	out = binary.BigEndian.AppendUint16(out, 500)
	out = binary.BigEndian.AppendUint16(out, 700)
	out = binary.BigEndian.AppendUint16(out, 0x0002) // flags: ARGS_ARE_XY_VALUES
	out = binary.BigEndian.AppendUint16(out, ref)    // glyphIndex
	out = append(out, 10, 20)                        // args（各 1 字节）
	return out
}

func fxGlyfLoca(opt fixtureOpt) (glyf []byte, loca []uint32) {
	parts := [][]byte{
		nil,                 // gid 0：空（.notdef 无轮廓即可）
		fxSimpleGlyph(100),  // gid 1
		fxSimpleGlyph(200),  // gid 2
		nil,                 // gid 3：空格，无轮廓
		fxCompositeGlyph(6), // gid 4 → gid 6
		fxCompositeGlyph(4), // gid 5 → gid 4（嵌套）
		fxSimpleGlyph(300),  // gid 6
	}
	if opt.selfCycle {
		parts[5] = fxCompositeGlyph(5) // 自引用
	}
	loca = make([]uint32, len(parts)+1)
	for i, p := range parts {
		loca[i] = uint32(len(glyf))
		glyf = append(glyf, p...)
	}
	loca[len(parts)] = uint32(len(glyf))
	return glyf, loca
}

// fxCmap 手搭一张 format 4 的 cmap。
//
// 其中 'A' / 'B' 那一段特意走 **idRangeOffset**（指向 glyphIdArray）而不是 idDelta：
// 两条路读取哨兵与原字体都可能出现，只测一条等于另一条永远是盲区。
func fxCmap() []byte {
	type seg struct {
		start, end uint16
		delta      int16
		viaArray   bool
		gids       []uint16
	}
	segs := []seg{
		{start: 0x20, end: 0x20, delta: int16(3 - 0x20)},               // 空格 → gid 3
		{start: 0x41, end: 0x42, viaArray: true, gids: []uint16{1, 2}}, // A/B → gid 1/2
		{start: 0x4E2D, end: 0x4E2D, delta: int16(6 - 0x4E2D)},         // 中 → gid 6（无独立码位的那个部件）
		{start: 0x5FA9, end: 0x5FA9, delta: int16(4 - 0x5FA9)},         // 復 → gid 4（注意是 U+5FA9 这个「復」，别写成简化字 复 U+590D）
		{start: 0x6F22, end: 0x6F22, delta: int16(5 - 0x6F22)},         // 漢 → gid 5
	}
	// Sentinel段：0xFFFF → 0（碰撞约定）
	count := len(segs) + 1

	sub := make([]byte, 0, 16+8*count+16)
	sub = binary.BigEndian.AppendUint16(sub, 4)                      // format
	sub = binary.BigEndian.AppendUint16(sub, uint16(16+8*count+2*2)) // length（含 glyphIdArray）
	sub = binary.BigEndian.AppendUint16(sub, 0)                      // language
	sub = binary.BigEndian.AppendUint16(sub, uint16(2*count))
	sub = binary.BigEndian.AppendUint16(sub, 2) // searchRange（够法定即可）
	sub = binary.BigEndian.AppendUint16(sub, 0) // entrySelector
	sub = binary.BigEndian.AppendUint16(sub, 0) // rangeShift

	endCodes := make([]byte, 0, 2*count)
	for _, s := range segs {
		endCodes = binary.BigEndian.AppendUint16(endCodes, s.end)
	}
	endCodes = binary.BigEndian.AppendUint16(endCodes, 0xFFFF)
	sub = append(sub, endCodes...)
	sub = binary.BigEndian.AppendUint16(sub, 0) // reservedPad

	for _, s := range segs {
		sub = binary.BigEndian.AppendUint16(sub, s.start)
	}
	sub = binary.BigEndian.AppendUint16(sub, 0xFFFF)

	for _, s := range segs {
		sub = binary.BigEndian.AppendUint16(sub, uint16(s.delta))
	}
	sub = binary.BigEndian.AppendUint16(sub, 1)

	// idRangeOffset：先算好每段的值再落笔。规范里的地址是相对 **&idRangeOffset[i]**
	// 起算的（不是相对 glyphIdArray 起点），故每段要减掉自己的 2*i。
	rangeOffStart := len(sub)
	arrayPos := rangeOffStart + 2*count
	offs := make([]uint16, count)
	for i, s := range segs {
		if s.viaArray {
			offs[i] = uint16(arrayPos - (rangeOffStart + 2*i))
			arrayPos += 2 * len(s.gids)
		}
	}
	for i := range offs {
		sub = binary.BigEndian.AppendUint16(sub, offs[i])
	}
	// 注意：上面这个循环已经把哨兵段（count-1 那个 0xFFFF→0 段）的 idRangeOffset
	// 一起写进去了，绝不能再补一个——多写一个字会把 glyphIdArray 整体推后 2 字节，
	// 于是 idRangeOffset 分支读到的永远差一格（症状是 'A' 落到 gid 0、'B' 落到 gid 1）。
	for _, s := range segs {
		for _, g := range s.gids {
			sub = binary.BigEndian.AppendUint16(sub, g)
		}
	}

	out := binary.BigEndian.AppendUint16(nil, 0) // version
	out = binary.BigEndian.AppendUint16(out, 1)  // numTables
	out = binary.BigEndian.AppendUint16(out, 3)  // platformID = Windows
	out = binary.BigEndian.AppendUint16(out, 1)  // encodingID = BMP
	out = binary.BigEndian.AppendUint32(out, 12)
	return append(out, sub...)
}

// buildFixtureTTF 拼一个完整的 sfnt（独立实现：不复用被测代码里的 assembler，
// 否则「夹具是对的」这件事本身就依赖被测代码）。
func buildFixtureTTF(opt fixtureOpt) []byte {
	glyf, loca := fxGlyfLoca(opt)
	locaBytes := make([]byte, 0, 4*len(loca))
	for _, v := range loca {
		locaBytes = binary.BigEndian.AppendUint32(locaBytes, v)
	}
	tables := []struct {
		tag  string
		data []byte
	}{
		{"cmap", fxCmap()},
		{"glyf", glyf},
		{"head", fxHead()},
		{"hhea", fxHhea()},
		{"hmtx", fxHmtx()},
		{"loca", locaBytes},
		{"maxp", fxMaxp()},
	}
	sort.Slice(tables, func(i, j int) bool { return tables[i].tag < tables[j].tag })

	out := binary.BigEndian.AppendUint32(nil, 0x00010000)
	out = binary.BigEndian.AppendUint16(out, uint16(len(tables)))
	out = binary.BigEndian.AppendUint16(out, 16*2)
	out = binary.BigEndian.AppendUint16(out, 1)
	out = binary.BigEndian.AppendUint16(out, 0)
	dirEnd := len(out)
	out = append(out, make([]byte, 16*len(tables))...)
	for i, t := range tables {
		off := len(out)
		out = append(out, t.data...)
		out = append(out, make([]byte, (4-len(t.data)%4)%4)...)
		ent := out[dirEnd+16*i:]
		copy(ent[0:4], t.tag)
		binary.BigEndian.PutUint32(ent[8:], uint32(off))
		binary.BigEndian.PutUint32(ent[12:], uint32(len(t.data)))
	}
	return out
}

// writeFixture 把夹具落到临时文件里（loadFont 需要一个真实路径）。
func writeFixture(t *testing.T, opt fixtureOpt) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "fixture.ttf")
	if err := os.WriteFile(p, buildFixtureTTF(opt), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// lookupRune 是测试里图省事的写法（不关心「有没有」，只要号）。
func (f *ttf) lookupRune(r rune) uint16 {
	g, _ := f.lookup(r)
	return g
}

// ---- 造坏字体的几个手术刀 ----
//
// 这些改动全部发生在**副本**上：夹具是全局共享的，就地改会让用例之间互相污染。

func clone(b []byte) []byte {
	out := append([]byte(nil), b...)
	return out
}

// dirEntry 找到某张表的目录项（偏移是针对整份文件的）。
func dirEntry(b []byte, tag string) []byte {
	n := int(binary.BigEndian.Uint16(b[4:]))
	for i := 0; i < n; i++ {
		e := b[12+16*i:]
		if string(e[0:4]) == tag {
			return e
		}
	}
	return nil
}

func withVersion(b []byte, ver uint32) []byte {
	c := clone(b)
	binary.BigEndian.PutUint32(c, ver)
	return c
}

// patch16 改某张表内部第 off 字节处的 uint16。
func patch16(b []byte, tag string, off int, v uint16) []byte {
	c := clone(b)
	e := dirEntry(c, tag)
	if e == nil {
		panic("夹具里没有表 " + tag)
	}
	data := c[binary.BigEndian.Uint32(e[8:]):]
	binary.BigEndian.PutUint16(data[off:], v)
	return c
}

// patch16File 改整份文件第 off 字节处的 uint16（用于 sfnt 头上的字段）。
func patch16File(b []byte, off int, v uint16) []byte {
	c := clone(b)
	binary.BigEndian.PutUint16(c[off:], v)
	return c
}

// shortenTable 把某张表的长度改小（模拟「表被截断」）。
func shortenTable(b []byte, tag string, n int) []byte {
	c := clone(b)
	e := dirEntry(c, tag)
	if e == nil {
		panic("夹具里没有表 " + tag)
	}
	binary.BigEndian.PutUint32(e[12:], uint32(n))
	return c
}

// dropTable 把某张表的 tag 改成不存在的值 ⇒ 效果等于「少了这张表」。
func dropTable(b []byte, tag string) []byte {
	c := clone(b)
	e := dirEntry(c, tag)
	if e == nil {
		panic("夹具里没有表 " + tag)
	}
	copy(e[0:4], "zzzz")
	return c
}

// ---- 用例 ----

func TestParseFixtureFont(t *testing.T) {
	f, err := parseTTF(buildFixtureTTF(fixtureOpt{}))
	if err != nil {
		t.Fatalf("解析夹具失败: %v", err)
	}
	if f.upem != fxUpem || f.numGlyphs != 7 || f.numberOfHMetrics != 7 {
		t.Fatalf("头信息不对: upem=%d numGlyphs=%d metrics=%d", f.upem, f.numGlyphs, f.numberOfHMetrics)
	}
	cases := map[rune]uint16{
		' ': 3, 'A': 1, 'B': 2, '中': 6, '復': 4, '漢': 5,
	}
	for r, want := range cases {
		// 'A' / 'B' 走 idRangeOffset 分支，其余走 idDelta：两条路都要绿
		if got, ok := f.lookup(r); !ok || got != want {
			t.Errorf("lookup(%q) = %d, %v，want %d", r, got, ok, want)
		}
	}
	if _, ok := f.lookup('Z'); ok {
		t.Error("字体里没有 Z，lookup 却给了字形")
	}
	if f.isComposite(4) != true || f.isComposite(5) != true || f.isComposite(1) {
		t.Error("复合字形判定不对")
	}
	if adv, _ := f.metrics(3); adv != 300 {
		t.Errorf("空格的步进宽度应为 300，实际 %d", adv)
	}
}

func TestSubsetPullsCompositeClosure(t *testing.T) {
	f, err := parseTTF(buildFixtureTTF(fixtureOpt{}))
	if err != nil {
		t.Fatal(err)
	}
	// 只相要「復」一个字：它是复合字形，依赖 gid 6；gid 6 没有独立码位，
	// 不靠闭包根本不会被带上——带不上就是缺笔画。
	plan, out, err := f.subset([]rune{'復'})
	if err != nil {
		t.Fatalf("子集化失败: %v", err)
	}
	got, err := parseTTF(out)
	if err != nil {
		t.Fatalf("子集出来的不是合法字体: %v", err)
	}
	if got.numGlyphs != 3 { // .notdef + 復 + 部件
		t.Fatalf("子集应有 3 个字形，实际 %d", got.numGlyphs)
	}
	if g, ok := got.lookup('復'); !ok || g == 0 {
		t.Fatalf("子集里查不到 復")
	}
	// 组件的字形号必须被改写成子集里的新编号（夹具里旧号是 6，子集里是 2）
	p, err := got.planComposite(got.lookupRune('復'))
	if err != nil {
		t.Fatalf("读回复合字形失败: %v", err)
	}
	if len(p.components) != 1 || p.components[0] != 2 {
		t.Fatalf("复合字形的组件编号没被改写：%v（应为 [2]）", p.components)
	}
	if adv, ok := plan.advOf['復']; !ok || adv != 1000 {
		t.Errorf("度量没跟着子集计划: %v %v", adv, ok)
	}
}

func TestSubsetHandlesNestedComposite(t *testing.T) {
	f, err := parseTTF(buildFixtureTTF(fixtureOpt{}))
	if err != nil {
		t.Fatal(err)
	}
	// 漢 → 復（复合）→ 部件：两跳闭包，少一跳就会缺笔画
	_, out, err := f.subset([]rune{'漢'})
	if err != nil {
		t.Fatalf("子集化失败: %v", err)
	}
	got, err := parseTTF(out)
	if err != nil {
		t.Fatalf("子集不是合法字体: %v", err)
	}
	if got.numGlyphs != 4 { // .notdef + 漢 + 復 + 部件
		t.Fatalf("嵌套闭包应带出 4 个字形，实际 %d", got.numGlyphs)
	}
	if g, ok := got.lookup('漢'); !ok || g == 0 {
		t.Fatal("嵌套复合字形的码位映射丢了")
	}
}

func TestSubsetDoesNotHangOnSelfReference(t *testing.T) {
	f, err := parseTTF(buildFixtureTTF(fixtureOpt{selfCycle: true}))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, _, err := f.subset([]rune{'漢'})
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Logf("自引用被判为错误（可接受）: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("自引用的字体把子集化卡住了——那是 DoS 而不是功能")
	}
}

func TestSubsetMissingGlyphIsAnError(t *testing.T) {
	f, err := parseTTF(buildFixtureTTF(fixtureOpt{}))
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = f.subset([]rune{'Z'})
	if err == nil {
		t.Fatal("字体里没有的字必须报错")
	}
	if !strings.Contains(err.Error(), errFontNoGlyph) || !strings.Contains(err.Error(), "U+005A") {
		t.Fatalf("报错应带错误码并点名是哪个字，实际: %v", err)
	}
}

// independentChecksum 按 sfnt 规范重算整个文件的校验和（不复用被测代码),
// 用来证明「写出来的 head.checkSumAdjustment 是照规范算的」。
func independentChecksum(b []byte) uint32 {
	var sum uint32
	for i := 0; i+4 <= len(b); i += 4 {
		sum += binary.BigEndian.Uint32(b[i:])
	}
	return sum
}

func TestSubsetChecksumFollowsSpec(t *testing.T) {
	f, err := parseTTF(buildFixtureTTF(fixtureOpt{}))
	if err != nil {
		t.Fatal(err)
	}
	_, out, err := f.subset([]rune{'A', '復', ' '})
	if err != nil {
		t.Fatal(err)
	}
	// 规范要求：整个文件按 uint32 累加后必须等于 0xB1B0AFBA（负数按无符号回卷）
	if got := independentChecksum(out); got != 0xB1B0AFBA {
		t.Fatalf("文件的校验和应为 0xB1B0AFBA，实际 0x%08X：挑别的阅读器会判定字体损坏", got)
	}
	// 目录里的每张表也要各自对得上
	n := int(binary.BigEndian.Uint16(out[4:]))
	for i := 0; i < n; i++ {
		e := out[12+16*i:]
		off, length := binary.BigEndian.Uint32(e[8:]), binary.BigEndian.Uint32(e[12:])
		want := binary.BigEndian.Uint32(e[4:])
		var got uint32
		body := out[off : off+length]
		// head 表的目录校验和是按「checkSumAdjustment 字段为 0」算的（sfnt 标准做法，
		// 否则校验和会依赖自身）；而最终文件里那个字段已被 assembleSFNT 回填，故比对前
		// 要把它清零再算，否则 head 这一张必然对不上。
		if string(e[0:4]) == "head" && length >= 12 {
			hc := make([]byte, length)
			copy(hc, body)
			hc[8], hc[9], hc[10], hc[11] = 0, 0, 0, 0
			body = hc
		}
		for j := 0; j+4 <= len(body); j += 4 {
			got += binary.BigEndian.Uint32(body[j:])
		}
		if rest := len(body) % 4; rest > 0 {
			var tail [4]byte
			copy(tail[:], body[len(body)-rest:])
			got += binary.BigEndian.Uint32(tail[:])
		}
		if got != want {
			t.Errorf("表 %s 的 checksum 对不上（算得 0x%08X，写着 0x%08X）", string(e[0:4]), got, want)
		}
	}
}

func TestSubsetIncludesOnlyUsedGlyphs(t *testing.T) {
	f, err := parseTTF(buildFixtureTTF(fixtureOpt{}))
	if err != nil {
		t.Fatal(err)
	}
	_, out, err := f.subset([]rune{'A', 'B', ' '})
	if err != nil {
		t.Fatal(err)
	}
	got, err := parseTTF(out)
	if err != nil {
		t.Fatal(err)
	}
	// 三个简单字形 + .notdef；没有复合字形的组件（它们压根没被引用）
	if got.numGlyphs != 4 {
		t.Fatalf("应是 4 个字形，实际 %d", got.numGlyphs)
	}
	if _, ok := got.lookup('復'); ok {
		t.Error("没用到的字不该出现在子集里")
	}
}

func TestCmap12RoundTrip(t *testing.T) {
	// 非 BMP 字符（emoji）只能用 format 12 表示：造一张并自己读回来
	raw := buildCmap12([]runeGID{{r: 0x1F600, gid: 1}, {r: 0x41, gid: 2}})
	f := &ttf{sub: cmapSub{format: 12, data: raw[12:]}}
	if g, ok := f.lookup(0x1F600); !ok || g != 1 {
		t.Fatalf("format 12 查不到非 BMP 字符: %d %v", g, ok)
	}
	if g, ok := f.lookup('A'); !ok || g != 2 {
		t.Fatalf("format 12 查 BMP 字符失败: %d %v", g, ok)
	}
	if _, ok := f.lookup('Z'); ok {
		t.Error("不在 group 里的字符不该有结果")
	}
}

func TestCmap4RoundTrip(t *testing.T) {
	raw := buildCmap4([]runeGID{{r: 'A', gid: 1}, {r: 'B', gid: 2}, {r: 'Z', gid: 9}})
	f := &ttf{sub: cmapSub{format: 4, data: raw[12:]}}
	for r, want := range map[rune]uint16{'A': 1, 'B': 2, 'Z': 9} {
		if g, ok := f.lookup(r); !ok || g != want {
			t.Errorf("lookup(%q) = %d,%v want %d", r, g, ok, want)
		}
	}
	if _, ok := f.lookup('Y'); ok {
		t.Error("不连续的码位不该被误命中")
	}
}

func TestParseRejectsBadFonts(t *testing.T) {
	good := buildFixtureTTF(fixtureOpt{})
	cases := []struct {
		name string
		data []byte
		want string
	}{
		{"空文件", []byte{}, errFontCorrupt},
		{"不到 12 字节", []byte("TrueTy"), errFontCorrupt},
		{"TTC 集合容器", withVersion(good, 0x74746366), errFontBadFormat},
		{"OTF/CFF 轮廓", withVersion(good, 0x4F54544F), errFontBadFormat},
		{"不认识的 sfnt 版本", withVersion(good, 0x12345678), errFontBadFormat},
		{"表目录越界", patch16File(good, 4, 5000), errFontCorrupt},
		{"少了 glyf 表", dropTable(good, "glyf"), errFontCorrupt},
		{"head 太短", shortenTable(good, "head", 20), errFontCorrupt},
		{"unitsPerEm 离谱", patch16(good, "head", 18, 1), errFontCorrupt},
		{"maxp 太短", shortenTable(good, "maxp", 3), errFontCorrupt},
		{"numGlyphs=0", patch16(good, "maxp", 4, 0), errFontCorrupt},
		{"hhea 太短", shortenTable(good, "hhea", 10), errFontCorrupt},
		{"hmtx 太短", shortenTable(good, "hmtx", 8), errFontCorrupt},
		{"loca 太短", shortenTable(good, "loca", 4), errFontCorrupt},
		{"indexToLocFormat 非法", patch16(good, "head", 50, 7), errFontCorrupt},
		{"cmap 没有可用子表", shortenTable(good, "cmap", 3), errFontCorrupt},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := parseTTF(c.data)
			if err == nil {
				t.Fatalf("应被拒")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("错误码应为 %s，实际 %v", c.want, err)
			}
		})
	}
}

func TestFamilyName(t *testing.T) {
	cases := map[string]string{
		"simhei.ttf":     "SIMHEI",
		"MSMHei.ttf":     "MSMHEI",
		"wqy-zenhei.ttc": "WQYZENHEI",
		"黑体.ttf":         "SUBSET",
		".ttf":           "SUBSET",
	}
	for in, want := range cases {
		if got := familyName(in); got != want {
			t.Errorf("familyName(%q) = %q want %q", in, got, want)
		}
	}
}
