package main

import (
	"encoding/binary"
	"fmt"
	"sort"
	"strings"
)

// 本文件是 E4 里唯一带风险的那部分（PRD §4.5 风险登记）：**手写最小 TTF 子集化**。
//
// 为什么必须子集：PDF 里要画出中文就得把字形数据塞进去。整字嵌入意味着每份对账单
// 都背着一颗 10MB 的字体；而 PDF 常见的负担其实只有几百个不同的字。
//
// 为什么只能手写：引一个字体库会让 go.mod / go.sum 全量变动
// （AC-E4-4 要的就是它俩一字不变），而它们各自还带着一整套依赖树。
//
// 三条刻意的取舍：
//  1. **只支持 TrueType 轮廓（glyf）**：CFF/OTF（sfnt 版本 OTTO）与字体集合容器
//     （ttcf，Windows 上的雅黑就是 .ttc）直接报错并说清原因，不做「尽量试试」。
//     现实影响可控：本机有 `simhei.ttf` / `simkai.ttf` / `simfang.ttf` 这类单文件 TTF。
//  2. **取不到字形的字一律报错**，绝不悄悄换成 .notdef：一份满是方框的对账单会被
//     当成「导出成功」，而它被发现的时候，往往已经是发给客户之后。
//  3. 不重建 head/maxp 里那些「上界」字段（maxPoints 等）：子集只会比原字体更小，
//     沿用原值是安全的上界，重建它们反而要引入新的算法风险。
//
// 支持的输入范围是**够用即可**：head / hhea / maxp / hmtx / cmap(4/12) / loca / glyf
// 七个表必须有；name / OS/2 有就原样带上，没有也不硬造（post 表它们都对 dontcare）。

const (
	errFontLoad      = "font_load"
	errFontBadFormat = "font_bad_format"
	errFontCorrupt   = "font_corrupt"
	errFontNoGlyph   = "font_no_glyph"
	errFontNoFont    = "font_not_found"
	errFontTooBig    = "font_too_big"
)

// maxFontBytes 是允许读进来的字体文件大小上限（128MB）。
//
// 字体紫外本进程读一次的，但 ipad-impact 在于：**绝不能让一次导出把侧车的内存顶到
// 被 OOM 杀掉**——那会把同机其他侧车一起带走。128MB 足够任何单文件 TTF。
const maxFontBytes = 128 << 20

// ttf 是解析后的字体视图。所有 slice 都只是 data 上的窗口，不额外复制。
type ttf struct {
	data   []byte
	tables map[string][]byte

	upem             uint16 // 每 em 的单位数（通常为 1000 或 2048）
	numGlyphs        int
	numberOfHMetrics int
	loca             []uint32
	sub              cmapSub
	hhea             []byte
	head             []byte
	maxp             []byte
	hmtx             []byte
	glyf             []byte
}

// cmapSub 是选定的那张 cmap 子表（一次解析，按需查码）。
type cmapSub struct {
	format uint16
	data   []byte
}

// parseTTF 解析一个内存里的 TrueType 文件。
func parseTTF(data []byte) (*ttf, error) {
	if len(data) < 12 {
		return nil, &fontErr{errFontCorrupt, fmt.Sprintf("文件只有 %d 字节，连 sfnt 头都不够", len(data))}
	}
	ver := binary.BigEndian.Uint32(data)
	switch ver {
	case 0x00010000, 0x74727565: // 'true'（部分 Apple 字体）
	default:
		return nil, unsupportedFormat(ver)
	}
	numTables := int(binary.BigEndian.Uint16(data[4:]))
	if 12+16*numTables > len(data) {
		return nil, &fontErr{errFontCorrupt, fmt.Sprintf("表目录声明 %d 张表，文件容不下", numTables)}
	}
	f := &ttf{data: data, tables: make(map[string][]byte, numTables)}
	for i := 0; i < numTables; i++ {
		e := data[12+16*i:]
		tag := string(e[0:4])
		off := binary.BigEndian.Uint32(e[8:])
		length := binary.BigEndian.Uint32(e[12:])
		if uint64(off)+uint64(length) > uint64(len(data)) {
			return nil, &fontErr{errFontCorrupt, fmt.Sprintf("表 %s 的范围（%d+%d）越出文件末尾（%d）", tag, off, length, len(data))}
		}
		f.tables[tag] = data[off : off+length]
	}
	for _, need := range []string{"head", "hhea", "maxp", "hmtx", "cmap", "loca", "glyf"} {
		if len(f.tables[need]) == 0 {
			return nil, &fontErr{errFontCorrupt, fmt.Sprintf("缺必要表 %s（TrueType 字体必须带它）", need)}
		}
	}
	if err := f.parseHead(); err != nil {
		return nil, err
	}
	if err := f.parseMetrics(); err != nil {
		return nil, err
	}
	if err := f.parseLoca(); err != nil {
		return nil, err
	}
	if err := f.parseCmap(); err != nil {
		return nil, err
	}
	return f, nil
}

func unsupportedFormat(ver uint32) error {
	switch ver {
	case 0x74746366: // 'ttcf'
		return &fontErr{errFontBadFormat, "这是字体集合容器（TTC：微软雅黑那种 .ttc），本侧车只支持单文件 TTF；请换一个 .ttf，或用 font 参数指定"}
	case 0x4F54544F: // 'OTTO'
		return &fontErr{errFontBadFormat, "这是 CFF 轮廓（OTF），本侧车只支持 TrueType 轮廓；请换一个字形数据用 glyf 表的字体"}
	default:
		return &fontErr{errFontBadFormat, fmt.Sprintf("sfnt 版本号 0x%08X 不是能处理的字体", ver)}
	}
}

func (f *ttf) parseHead() error {
	h := f.tables["head"]
	if len(h) < 54 {
		return &fontErr{errFontCorrupt, fmt.Sprintf("head 表只有 %d 字节（至少 54）", len(h))}
	}
	f.head = h
	f.upem = binary.BigEndian.Uint16(h[18:])
	if f.upem < 16 || f.upem > 16384 {
		return &fontErr{errFontCorrupt, fmt.Sprintf("unitsPerEm=%d 不像正常字体（合理区间 16~16384）", f.upem)}
	}
	return nil
}

func (f *ttf) parseMetrics() error {
	m := f.tables["maxp"]
	if len(m) < 6 {
		return &fontErr{errFontCorrupt, fmt.Sprintf("maxp 表只有 %d 字节（至少 6）", len(m))}
	}
	f.maxp = m
	f.numGlyphs = int(binary.BigEndian.Uint16(m[4:]))
	if f.numGlyphs == 0 {
		return &fontErr{errFontCorrupt, "maxp 里 numGlyphs=0（没有字形的字体画不出任何东西）"}
	}

	h := f.tables["hhea"]
	if len(h) < 36 {
		return &fontErr{errFontCorrupt, fmt.Sprintf("hhea 表只有 %d 字节（至少 36）", len(h))}
	}
	f.hhea = h
	f.numberOfHMetrics = int(binary.BigEndian.Uint16(h[34:]))
	if f.numberOfHMetrics == 0 || f.numberOfHMetrics > f.numGlyphs {
		return &fontErr{errFontCorrupt, fmt.Sprintf("hhea 的 numberOfHMetrics=%d 与 numGlyphs=%d 对不上", f.numberOfHMetrics, f.numGlyphs)}
	}

	// hmtx 的长度由上面两个数决定：前面 numberOfHMetrics 条是 (advance, lsb) 各 2 字节，
	// 剩下的每张字形只保留 lsb。表短了就是字体坏了——静默补 0 会让字挤在一起。
	need := 4*f.numberOfHMetrics + 2*(f.numGlyphs-f.numberOfHMetrics)
	f.hmtx = f.tables["hmtx"]
	if len(f.hmtx) < need {
		return &fontErr{errFontCorrupt, fmt.Sprintf("hmtx 表只有 %d 字节，按 numGlyphs=%d 推算至少 %d", len(f.hmtx), f.numGlyphs, need)}
	}
	f.glyf = f.tables["glyf"]
	return nil
}

func (f *ttf) parseLoca() error {
	raw := f.tables["loca"]
	want := f.numGlyphs + 1
	format := int16(binary.BigEndian.Uint16(f.head[50:]))
	f.loca = make([]uint32, want)
	switch format {
	case 0:
		if len(raw) < 2*want {
			return &fontErr{errFontCorrupt, fmt.Sprintf("loca（short 格式）需要 %d 字节，实际 %d", 2*want, len(raw))}
		}
		for i := 0; i < want; i++ {
			f.loca[i] = uint32(binary.BigEndian.Uint16(raw[2*i:])) * 2 // short 格式存的是 offset/2
		}
	case 1:
		if len(raw) < 4*want {
			return &fontErr{errFontCorrupt, fmt.Sprintf("loca（long 格式）需要 %d 字节，实际 %d", 4*want, len(raw))}
		}
		for i := 0; i < want; i++ {
			f.loca[i] = binary.BigEndian.Uint32(raw[4*i:])
		}
	default:
		return &fontErr{errFontCorrupt, fmt.Sprintf("head.indexToLocFormat=%d 只能是 0 或 1", format)}
	}
	for i := 1; i < len(f.loca); i++ {
		if f.loca[i] < f.loca[i-1] || uint64(f.loca[i]) > uint64(len(f.glyf)) {
			return &fontErr{errFontCorrupt, fmt.Sprintf("loca[%d]=%d 不单调或越出 glyf（%d 字节）", i, f.loca[i], len(f.glyf))}
		}
	}
	return nil
}

func (f *ttf) parseCmap() error {
	raw := f.tables["cmap"]
	if len(raw) < 4 {
		return &fontErr{errFontCorrupt, fmt.Sprintf("cmap 表只有 %d 字节", len(raw))}
	}
	n := int(binary.BigEndian.Uint16(raw[2:]))
	if 4+8*n > len(raw) {
		return &fontErr{errFontCorrupt, fmt.Sprintf("cmap 声明 %d 张子表，文件容不下", n)}
	}
	type cand struct {
		score int
		off   uint32
	}
	var best *cand
	for i := 0; i < n; i++ {
		e := raw[4+8*i:]
		platform, encoding := binary.BigEndian.Uint16(e), binary.BigEndian.Uint16(e[2:])
		off := binary.BigEndian.Uint32(e[4:])
		if off >= uint32(len(raw)) {
			continue
		}
		sub := raw[off:]
		if len(sub) < 2 {
			continue
		}
		format := binary.BigEndian.Uint16(sub)
		if format != 4 && format != 12 {
			continue // 其余格式（0/2/6/13…）本侧车不认，跳过即可
		}
		score := 0
		switch {
		case platform == 3 && encoding == 10:
			score = 5 // UCS-4：覆盖最广
		case platform == 3 && encoding == 1:
			score = 4 // BMP：Windows 字体的常态
		case platform == 0 && (encoding == 3 || encoding == 4):
			score = 3
		default:
			score = 1
		}
		if best == nil || score > best.score {
			best = &cand{score, off}
		}
	}
	if best == nil {
		return &fontErr{errFontCorrupt, "cmap 里没有本侧车认得的字形映射表（只支持格式 4 / 12）"}
	}
	sub := raw[best.off:]
	f.sub = cmapSub{format: binary.BigEndian.Uint16(sub), data: sub}
	return nil
}

// lookup 查一个码位对应的字形号。查不到返回 false（由调用方决定是报错还是跳过）。
func (f *ttf) lookup(r rune) (uint16, bool) {
	switch f.sub.format {
	case 4:
		return f.lookup4(uint32(r))
	case 12:
		return f.lookup12(uint32(r))
	}
	return 0, false
}

func (f *ttf) lookup4(code uint32) (uint16, bool) {
	d := f.sub.data
	if code > 0xFFFF || len(d) < 16 {
		return 0, false
	}
	segX2 := int(binary.BigEndian.Uint16(d[6:]))
	if segX2%2 != 0 || len(d) < 14+segX2 {
		return 0, false
	}
	seg := segX2 / 2
	end := func(i int) uint16 { return binary.BigEndian.Uint16(d[14+2*i:]) }
	start := func(i int) uint16 { return binary.BigEndian.Uint16(d[16+segX2+2*i:]) }
	delta := func(i int) int16 { return int16(binary.BigEndian.Uint16(d[16+2*segX2+2*i:])) }
	rangeOff := func(i int) uint16 { return binary.BigEndian.Uint16(d[16+3*segX2+2*i:]) }
	c := uint16(code)
	for i := 0; i < seg; i++ {
		if end(i) < c {
			continue
		}
		if start(i) > c {
			return 0, false
		}
		if ro := rangeOff(i); ro == 0 {
			g := uint16(int32(c) + int32(delta(i)))
			if g == 0 {
				return 0, false
			}
			return g, true
		} else {
			// idRangeOffset：字形号从 glyphIdArray 里取，地址相对 **&idRangeOffset[i]**
			// 起算（不是相对 glyphIdArray 的起点）——差了一个 i 就会串到隔壁段的字形号上，
			// 症状是「个别字显示成另一个字」，极难定位。
			gia := 16 + 3*segX2 + 2*i + int(ro) + 2*int(c-start(i))
			if gia+2 > len(d) {
				return 0, false
			}
			g := binary.BigEndian.Uint16(d[gia:])
			if g == 0 {
				return 0, false
			}
			return uint16(int32(g)+int32(delta(i))) & 0xFFFF, true
		}
	}
	return 0, false
}

func (f *ttf) lookup12(code uint32) (uint16, bool) {
	d := f.sub.data
	if len(d) < 16 {
		return 0, false
	}
	n := int(binary.BigEndian.Uint32(d[12:]))
	if 16+12*n > len(d) {
		n = (len(d) - 16) / 12 // 声明的比实际多：按实际能读到的处理，其余当不存在
	}
	for i := 0; i < n; i++ {
		g := d[16+12*i:]
		start, end := binary.BigEndian.Uint32(g), binary.BigEndian.Uint32(g[4:])
		if code < start || code > end {
			continue
		}
		gid := binary.BigEndian.Uint32(g[8:]) + (code - start)
		if gid == 0 || gid > 0xFFFF {
			return 0, false
		}
		return uint16(gid), true
	}
	return 0, false
}

// glyphBytes 取一个字形的原始 glyf 数据（可能是空：比如空格这种无轮廓的字形）。
func (f *ttf) glyphBytes(gid uint16) []byte {
	if int(gid) >= f.numGlyphs {
		return nil
	}
	a, b := f.loca[gid], f.loca[gid+1]
	if b > uint32(len(f.glyf)) {
		return nil
	}
	return f.glyf[a:b]
}

// isComposite 判断字形是不是「复合字形」（由别的字形拼出来的那种）。
func (f *ttf) isComposite(gid uint16) bool {
	d := f.glyphBytes(gid)
	if len(d) < 10 {
		return false
	}
	return int16(binary.BigEndian.Uint16(d)) < 0
}

// compositePlan 记一个复合字形里各个 component 的 glyphIndex 落在数据的哪个字节上——
// 子集化后要把那里的旧字形号改写成新字形号。
type compositePlan struct {
	indexOffsets []int // 相对 glyphBytes(gid)[0] 的偏移
	components   []uint16
}

// planComposite 解析复合字形的组件列表。
//
// 组件的影响必须由 flags 逐个算：每个组件先是 flags(2) + glyphIndex(2)，随后是
// 「参数」(2 或 4 字节) 与「变换」(0/2/4/8 字节)——长度不一样，跳过必须走同一套规则。
func (f *ttf) planComposite(gid uint16) (*compositePlan, error) {
	d := f.glyphBytes(gid)
	if len(d) < 10 {
		return nil, &fontErr{errFontCorrupt, fmt.Sprintf("字形 %d 连 10 字节头都没有", gid)}
	}
	p := &compositePlan{}
	for off := 10; off+2 <= len(d); {
		flags := binary.BigEndian.Uint16(d[off:])
		if off+4 > len(d) {
			return nil, &fontErr{errFontCorrupt, fmt.Sprintf("字形 %d 的组件在 %d 处被截断", gid, off)}
		}
		p.indexOffsets = append(p.indexOffsets, off+2)
		p.components = append(p.components, binary.BigEndian.Uint16(d[off+2:]))
		off += 4
		switch {
		case flags&0x0001 != 0: // ARG_1_AND_2_ARE_WORDS
			off += 4
		default:
			off += 2
		}
		switch {
		case flags&0x0008 != 0: // WE_HAVE_A_SCALE
			off += 2
		case flags&0x0040 != 0: // WE_HAVE_AN_X_AND_Y_SCALE
			off += 4
		case flags&0x0080 != 0: // WE_HAVE_A_TWO_BY_TWO
			off += 8
		}
		if off+2 > len(d) {
			break
		}
		if flags&0x0020 == 0 { // MORE_COMPONENTS
			break
		}
	}
	return p, nil
}

// metrics 返回字形的步进宽度（advance）与左跨（lsb）。
func (f *ttf) metrics(gid uint16) (advance uint16, lsb int16) {
	g := int(gid)
	if g < f.numberOfHMetrics {
		return binary.BigEndian.Uint16(f.hmtx[4*g:]), int16(binary.BigEndian.Uint16(f.hmtx[4*g+2:]))
	}
	// 超过 numberOfHMetrics 的字形共用最后一条 advance，lsb 另取一张表
	last := binary.BigEndian.Uint16(f.hmtx[4*(f.numberOfHMetrics-1):])
	i := 4*f.numberOfHMetrics + 2*(g-f.numberOfHMetrics)
	if i+2 > len(f.hmtx) {
		return last, 0
	}
	return last, int16(binary.BigEndian.Uint16(f.hmtx[i:]))
}

// subsetPlan 是「这次要用到的字」到子集字形号之间的映射，也是排版量宽度时的字典。
type subsetPlan struct {
	upem     uint16
	gidOf    map[rune]uint16 // rune → 子集里的**新**字形号（绘制就用它）
	gidByOld map[rune]uint16 // rune → 原字体里的字形号（只用于度量与闭包）
	advOf    map[rune]uint16 // 步进宽度（字体单位）
	order    []rune          // 升序，便于确定性地写 ToUnicode / cmap
}

// subset 按 runes 造一个只含所需字形的字体，并返回排排版要用的度量。
func (f *ttf) subset(runes []rune) (*subsetPlan, []byte, error) {
	plan := &subsetPlan{upem: f.upem, gidOf: map[rune]uint16{}, gidByOld: map[rune]uint16{}, advOf: map[rune]uint16{}}
	need := map[uint16]bool{0: true} // 第 0 个永远是 .notdef
	for _, r := range runes {
		gid, ok := f.lookup(r)
		if !ok {
			return nil, nil, &fontErr{errFontNoGlyph,
				fmt.Sprintf("字体里没有 U+%04X（%q）的字形：换个能覆盖它的字体（用 font 参数指定），"+
					"或先把这个字符替换掉——本侧车不许悄咪咪画成方框", r, string(r))}
		}
		if _, seen := plan.gidByOld[r]; seen {
			continue
		}
		plan.gidByOld[r] = gid
		need[gid] = true
	}

	// 复合字形的**依赖闭包**：不把组件一起带上，拼出来的汉字会缺笔画，
	// 而且缺得毫无规律（同一个字在不同字号下缺的地方还可能不一样）。
	queue := make([]uint16, 0, len(need))
	for g := range need {
		queue = append(queue, g)
	}
	composites := map[uint16]*compositePlan{}
	for len(queue) > 0 {
		g := queue[0]
		queue = queue[1:]
		if !f.isComposite(g) {
			continue
		}
		p, err := f.planComposite(g)
		if err != nil {
			return nil, nil, err
		}
		composites[g] = p
		for _, c := range p.components {
			if !need[c] {
				need[c] = true
				queue = append(queue, c) // 已在 need 里的不会重复入队 ⇒ 自引用不会挂住
			}
		}
	}

	olds := make([]uint16, 0, len(need))
	olds = append(olds, 0)
	for g := range need {
		if g != 0 {
			olds = append(olds, g)
		}
	}
	sort.Slice(olds[1:], func(i, j int) bool { return olds[i+1] < olds[j+1] })
	newOf := make(map[uint16]uint16, len(olds))
	for i, g := range olds {
		newOf[g] = uint16(i)
	}
	for r, old := range plan.gidByOld {
		plan.gidOf[r] = newOf[old]
		adv, _ := f.metrics(old)
		plan.advOf[r] = adv
	}
	plan.order = make([]rune, 0, len(plan.gidOf))
	for r := range plan.gidOf {
		plan.order = append(plan.order, r)
	}
	sort.Slice(plan.order, func(i, j int) bool { return plan.order[i] < plan.order[j] })

	pairs := make([]runeGID, 0, len(plan.gidOf))
	for _, r := range plan.order {
		pairs = append(pairs, runeGID{r, plan.gidOf[r]})
	}
	out, err := f.buildSubset(olds, newOf, composites, pairs)
	if err != nil {
		return nil, nil, err
	}
	return plan, out, nil
}

// buildSubset 把挑好的字形重排成一个新的 sfnt 文件。
func (f *ttf) buildSubset(olds []uint16, newOf map[uint16]uint16, composites map[uint16]*compositePlan, pairs []runeGID) ([]byte, error) {
	var glyf []byte
	loca := make([]uint32, len(olds)+1)
	for i, old := range olds {
		d := f.glyphBytes(old)
		if p, ok := composites[old]; ok {
			patched := append([]byte(nil), d...)
			for j, off := range p.indexOffsets {
				if off+2 > len(patched) {
					return nil, &fontErr{errFontCorrupt, fmt.Sprintf("字形 %d 的组件 #%d 越出自身数据", old, j)}
				}
				binary.BigEndian.PutUint16(patched[off:], newOf[p.components[j]])
			}
			d = patched
		}
		loca[i] = uint32(len(glyf))
		glyf = append(glyf, d...)
	}
	loca[len(olds)] = uint32(len(glyf))

	hmtx := make([]byte, 0, 4*len(olds))
	for _, old := range olds {
		adv, lsb := f.metrics(old)
		hmtx = binary.BigEndian.AppendUint16(hmtx, adv)
		hmtx = binary.BigEndian.AppendUint16(hmtx, uint16(lsb))
	}

	head := append([]byte(nil), f.head...)
	binary.BigEndian.PutUint16(head[50:], 1) // 统一用 long 格式的 loca：短格式要求偶对齐，自找麻烦
	binary.BigEndian.PutUint32(head[8:], 0)  // checkSumAdjustment 最后再算（此处必须是 0）

	hhea := append([]byte(nil), f.hhea...)
	binary.BigEndian.PutUint16(hhea[34:], uint16(len(olds))) // 每条宽度都显式给 ⇒ 不再需要尾部 lsb 表

	maxp := append([]byte(nil), f.maxp...)
	binary.BigEndian.PutUint16(maxp[4:], uint16(len(olds)))

	locaBytes := make([]byte, 0, 4*len(loca))
	for _, v := range loca {
		locaBytes = binary.BigEndian.AppendUint32(locaBytes, v)
	}

	tables := []struct {
		tag  string
		data []byte
	}{
		{"cmap", buildCmap(pairs, false)},
		{"glyf", glyf},
		{"head", head},
		{"hhea", hhea},
		{"hmtx", hmtx},
		{"loca", locaBytes},
		{"maxp", maxp},
		{"post", syntheticPost()},
	}
	// 可选表：有就原样带上。它们不与字形号相关（缺了也合法），带上能让
	// 拷出来的字体在别的场合也认得出名字。
	for _, tag := range []string{"name", "OS/2"} {
		if b := f.tables[tag]; len(b) > 0 {
			tables = append(tables, struct {
				tag  string
				data []byte
			}{tag, b})
		}
	}
	return assembleSFNT(tables)
}

type fontErr struct {
	code string
	msg  string
}

func (e *fontErr) Error() string { return e.code + ": " + e.msg }

// syntheticPost 造一张 post 表（version 3.0：不含字形名），长度固定 32 字节。
//
// 不沿用原表的理由是它常常是 format 2（按字形号索引的名字数组），子集化后索引全乱了；
// 重排名字要再写一套数组逻辑，而 PDF 根本不用它——直接给一张「声明过」的空表最稳。
func syntheticPost() []byte {
	b := make([]byte, 0, 32)
	b = binary.BigEndian.AppendUint32(b, 0x00030000) // version 3.0
	b = binary.BigEndian.AppendUint32(b, 0)          // italicAngle（Fixed）
	b = binary.BigEndian.AppendUint16(b, 0xFF9C)     // underlinePosition = -100
	b = binary.BigEndian.AppendUint16(b, 50)         // underlineThickness
	b = binary.BigEndian.AppendUint32(b, 0)          // isFixedPitch
	for i := 0; i < 4; i++ {                         // min/max memory Type42 + Type1
		b = binary.BigEndian.AppendUint32(b, 0)
	}
	return b
}

// buildCmap 造 cmap 表。BMP 内的字用格式 4（绝大多数情况），有非 BMP 字则改用格式 12。
//
// PDF 其实用不到这张表（它按 Identity-H + ToUnicode 取字），留它是为了让子集出来的
// 文件在别的工具眼里仍是一个**完整合法**的字体。
func buildCmap(pairs []runeGID, force12 bool) []byte {
	use12 := force12
	for _, p := range pairs {
		if p.r > 0xFFFF {
			use12 = true
			break
		}
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].r < pairs[j].r })
	if use12 {
		return buildCmap12(pairs)
	}
	return buildCmap4(pairs)
}

type runeGID struct {
	r   rune
	gid uint16
}

func buildCmap4(pairs []runeGID) []byte {
	// 先把连续且「码距 = 形距」的码位并成一个 segment（同一个 idDelta 即可覆盖）
	type seg struct {
		start, end rune
		delta      int
	}
	var segs []seg
	for _, p := range pairs {
		if p.r > 0xFFFF {
			continue
		}
		d := int(p.gid) - int(p.r)
		if n := len(segs); n > 0 && segs[n-1].end+1 == p.r && segs[n-1].delta == d {
			segs[n-1].end = p.r
			continue
		}
		segs = append(segs, seg{start: p.r, end: p.r, delta: d})
	}
	segs = append(segs, seg{start: 0xFFFF, end: 0xFFFF, delta: 1}) // 收尾哨兵
	count := len(segs)

	hdr := make([]byte, 0, 16+8*count)
	hdr = binary.BigEndian.AppendUint16(hdr, 4) // format
	hdr = binary.BigEndian.AppendUint16(hdr, uint16(16+8*count))
	hdr = binary.BigEndian.AppendUint16(hdr, 0) // language
	hdr = binary.BigEndian.AppendUint16(hdr, uint16(2*count))
	// searchRange / entrySelector / rangeShift：二分查找用的三个数，值固定写法
	sr, es := 2, 0
	for sr*2 <= count {
		sr *= 2
		es++
	}
	sr *= 2
	hdr = binary.BigEndian.AppendUint16(hdr, uint16(sr))
	hdr = binary.BigEndian.AppendUint16(hdr, uint16(es))
	hdr = binary.BigEndian.AppendUint16(hdr, uint16(2*count-sr))

	body := make([]byte, 0, 8*count+16)
	endCode := make([]byte, 0, 2*count)
	for _, s := range segs {
		endCode = binary.BigEndian.AppendUint16(endCode, uint16(s.end))
	}
	body = append(body, endCode...)
	body = binary.BigEndian.AppendUint16(body, 0) // reservedPad
	for _, s := range segs {
		body = binary.BigEndian.AppendUint16(body, uint16(s.start))
	}
	for _, s := range segs {
		body = binary.BigEndian.AppendUint16(body, uint16(int16(s.delta)))
	}
	for range segs {
		body = binary.BigEndian.AppendUint16(body, 0) // idRangeOffset：全 0 ⇒ 走 idDelta 分支
	}

	sub := append(hdr, body...)
	out := binary.BigEndian.AppendUint16(nil, 0) // version
	out = binary.BigEndian.AppendUint16(out, 1)  // numTables
	out = binary.BigEndian.AppendUint16(out, 3)  // platformID = Windows
	out = binary.BigEndian.AppendUint16(out, 1)  // encodingID = BMP
	out = binary.BigEndian.AppendUint32(out, 12)
	return append(out, sub...)
}

func buildCmap12(pairs []runeGID) []byte {
	type grp struct {
		start, end rune
		startGID   uint32
	}
	var groups []grp
	for _, p := range pairs {
		if n := len(groups); n > 0 && groups[n-1].end+1 == p.r &&
			groups[n-1].startGID+uint32(p.r-groups[n-1].start) == uint32(p.gid) {
			groups[n-1].end = p.r
			continue
		}
		groups = append(groups, grp{start: p.r, end: p.r, startGID: uint32(p.gid)})
	}
	sub := make([]byte, 0, 16+12*len(groups))
	sub = binary.BigEndian.AppendUint16(sub, 12)
	sub = binary.BigEndian.AppendUint16(sub, 0) // reserved
	sub = binary.BigEndian.AppendUint32(sub, uint32(16+12*len(groups)))
	sub = binary.BigEndian.AppendUint32(sub, 0) // language
	sub = binary.BigEndian.AppendUint32(sub, uint32(len(groups)))
	for _, g := range groups {
		sub = binary.BigEndian.AppendUint32(sub, uint32(g.start))
		sub = binary.BigEndian.AppendUint32(sub, uint32(g.end))
		sub = binary.BigEndian.AppendUint32(sub, g.startGID)
	}
	out := binary.BigEndian.AppendUint16(nil, 0)
	out = binary.BigEndian.AppendUint16(out, 1)
	out = binary.BigEndian.AppendUint16(out, 3)
	out = binary.BigEndian.AppendUint16(out, 10) // encodingID = UCS-4
	out = binary.BigEndian.AppendUint32(out, 12)
	return append(out, sub...)
}

// assembleSFNT 把若干张表拼成一个合法 sfnt：目录按 tag 升序、4 字节对齐、
// 每张表算 checksum、最后回填 head.checkSumAdjustment。
func assembleSFNT(tables []struct {
	tag  string
	data []byte
}) ([]byte, error) {
	sort.Slice(tables, func(i, j int) bool { return tables[i].tag < tables[j].tag })
	n := len(tables)

	// 先量出总长，一次性分配，避免逐个 append 反复拷贝（字体文件动辄几 MB）
	total := 12 + 16*n
	pads := make([]int, n)
	for i, t := range tables {
		pads[i] = (4 - len(t.data)%4) % 4
		total += len(t.data) + pads[i]
	}
	out := make([]byte, 0, total)
	out = binary.BigEndian.AppendUint32(out, 0x00010000) // sfnt version
	out = binary.BigEndian.AppendUint16(out, uint16(n))
	sr, es := 16, 0
	for sr*2 <= n {
		sr *= 2
		es++
	}
	out = binary.BigEndian.AppendUint16(out, uint16(sr))
	out = binary.BigEndian.AppendUint16(out, uint16(es))
	out = binary.BigEndian.AppendUint16(out, uint16(16*n-sr))

	dirEnd := len(out)
	out = append(out, make([]byte, 16*n)...) // 目录区占位，待会儿回填
	for i, t := range tables {
		off := len(out)
		out = append(out, t.data...)
		out = append(out, make([]byte, pads[i])...)
		ent := out[dirEnd+16*i:]
		copy(ent[0:4], t.tag)
		binary.BigEndian.PutUint32(ent[4:], checksum(out[off:off+len(t.data)+pads[i]]))
		binary.BigEndian.PutUint32(ent[8:], uint32(off))
		binary.BigEndian.PutUint32(ent[12:], uint32(len(t.data)))
	}

	// head 的 checksumAdjustment：标准做法是 0xB1B0AFBA 减「整个文件的校验和」。
	// 少了这一步，挑剔的阅读器（和一些字体校验工具）会判定字体损坏。
	var headOff, headLen int
	for i, t := range tables {
		if t.tag == "head" {
			headOff = int(binary.BigEndian.Uint32(out[dirEnd+16*i+8:]))
			headLen = int(binary.BigEndian.Uint32(out[dirEnd+16*i+12:]))
		}
	}
	if headLen == 0 {
		return nil, &fontErr{errFontCorrupt, "子集化结果里没有 head 表（不该发生）"}
	}
	var sum uint32
	for i := 0; i+4 <= len(out); i += 4 {
		sum += binary.BigEndian.Uint32(out[i:])
	}
	binary.BigEndian.PutUint32(out[headOff+8:], 0xB1B0AFBA-sum)
	return out, nil
}

// checksum 按 4 字节大端字累加（不足补零）。
func checksum(b []byte) uint32 {
	var sum uint32
	for i := 0; i+4 <= len(b); i += 4 {
		sum += binary.BigEndian.Uint32(b[i:])
	}
	if rest := len(b) % 4; rest > 0 {
		var tail [4]byte
		copy(tail[:], b[len(b)-rest:])
		sum += binary.BigEndian.Uint32(tail[:])
	}
	return sum
}

// familyName 猜一个能写进 PDF 的字体名（只留 ASCII 字母数字，PDF 名字不接受中文与空格）。
func familyName(base string) string {
	base = strings.TrimSuffix(base, ".ttf")
	base = strings.TrimSuffix(base, ".ttc")
	b := &strings.Builder{}
	for _, r := range strings.ToUpper(base) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "SUBSET"
	}
	return b.String()
}
