// 本文件是 ext.cron 的表达式核心：5 段标准 cron 的解析与「下一次触发时刻」计算。
//
// 为什么自己写而不用第三方库：本仓库对 ext 侧车的态度是「零新增依赖」（见 E4 的取舍），
// 而 5 段标准 cron 的全部语法（* / */step / a-b / a-b/step / 逗号列表 + 五个 @ 宏 + dow 与 dom
// 的非星号按「或」）用百来行就能表达完整；引入一个调度库会顺带拖进它自己的仓储与语义，
// 反而是「多一份会漂移的真相」。
//
// 精度：**分钟**级（与 PRD §4.1 的「去重键 = name + 触发时刻分钟粒度」一致）。
// 秒级触发不在本批范围——需要秒级请用 Flow 里的 sleep 链。
package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// cronSpec 是解析后的表达式（每个字段一个 bitset，下标即取值）。
//
// 字段刻意用切片而不是定长数组：parseField 直接产出 []bool，定长数组得再拷一遍；
// 本包内 cronSpec 只在解析成功后才存在，nil 切片不会被匹配到（parseCron 五个字段全填）。
type cronSpec struct {
	minute, hour, month, dom, dow []bool
	domStar, dowSt                bool // 记「是不是字面 *」：dom/dow 合流规则要靠它
}

// parseCron 解析 5 段标准 cron 表达式或 @ 宏。任何越界 / 反范围 / 非法步长都在**解析期**报错
// （fail-closed 前移到注册期：等到点才炸的调度器没人敢用）。
func parseCron(expr string) (cronSpec, error) {
	var c cronSpec
	s := strings.TrimSpace(expr)
	if s == "" {
		return c, fmt.Errorf("cron 表达式为空")
	}
	// @ 宏展开成等价的 5 段（宏只做文本替换，后面只认 5 段，解析路径只有一条）。
	switch strings.ToLower(s) {
	case "@yearly", "@annually":
		s = "0 0 1 1 *"
	case "@monthly":
		s = "0 0 1 * *"
	case "@weekly":
		s = "0 0 * * 0"
	case "@daily", "@midnight":
		s = "0 0 * * *"
	case "@hourly":
		s = "0 * * * *"
	}
	fields := strings.Fields(s)
	if len(fields) != 5 {
		return c, fmt.Errorf("cron 表达式必须是 5 段（分 时 日 月 周），实际 %d 段: %q", len(fields), expr)
	}
	var err error
	if c.minute, err = parseField(fields[0], 0, 59); err != nil {
		return c, fmt.Errorf("分钟位: %w", err)
	}
	if c.hour, err = parseField(fields[1], 0, 23); err != nil {
		return c, fmt.Errorf("小时位: %w", err)
	}
	if c.dom, c.domStar, err = parseDayField(fields[2], 1, 31); err != nil {
		return c, fmt.Errorf("日位: %w", err)
	}
	if c.month, err = parseField(fields[3], 1, 12); err != nil {
		return c, fmt.Errorf("月位: %w", err)
	}
	if c.dow, c.dowSt, err = parseDayField(fields[4], 0, 6); err != nil {
		return c, fmt.Errorf("周位: %w", err)
	}
	return c, nil
}

// parseField 解析一个字段（返回 bitset，下标即取值）。
func parseField(field string, lo, hi int) ([]bool, error) {
	bits := make([]bool, hi+1)
	hit := false
	for _, item := range strings.Split(field, ",") {
		item = strings.TrimSpace(item)
		step := 1
		if i := strings.Index(item, "/"); i >= 0 {
			n, err := parseInt(item[i+1:])
			if err != nil {
				return nil, fmt.Errorf("步长非法: %q", item[i+1:])
			}
			if n < 1 {
				return nil, fmt.Errorf("步长必须 ≥1，实际 %d", n)
			}
			step = n
			item = strings.TrimSpace(item[:i])
		}
		var start, end int
		switch {
		case item == "" || item == "*":
			start, end = lo, hi
		case strings.Contains(item, "-"):
			a, b, err := parseRange(item)
			if err != nil {
				return nil, err
			}
			if a > b {
				return nil, fmt.Errorf("范围反了（%s 的前半大于后半）", item)
			}
			start, end = a, b
		default:
			v, err := parseInt(item)
			if err != nil {
				return nil, err
			}
			start, end = v, v
		}
		if start < lo || end > hi {
			return nil, fmt.Errorf("取值越界（%d..%d 超出 %d..%d）: %s", start, end, lo, hi, item)
		}
		for v := start; v <= end; v += step {
			bits[v] = true
		}
		hit = true
	}
	if !hit {
		return nil, fmt.Errorf("字段没有任何取值: %q", field)
	}
	return bits, nil
}

// parseDayField 类似 parseField，但额外回报「是否字面 *」（dom 与 dow 的合流规则要用）。
func parseDayField(field string, lo, hi int) ([]bool, bool, error) {
	bits, err := parseField(field, lo, hi)
	if err != nil {
		return nil, false, err
	}
	star := strings.TrimSpace(field) == "*"
	return bits, star, nil
}

func parseInt(s string) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("不是数字: %q", s)
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("不是数字: %q", s)
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("数字过大: %q", s)
	}
	return n, nil
}

func parseRange(s string) (int, int, error) {
	parts := strings.SplitN(s, "-", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("范围写法错误（应形如 1-5）: %q", s)
	}
	a, err := parseInt(parts[0])
	if err != nil {
		return 0, 0, err
	}
	b, err := parseInt(parts[1])
	if err != nil {
		return 0, 0, err
	}
	return a, b, nil
}

// matches 判断某一时刻是否命中本表达式。
//
// 日/周位的合流沿用 Vixie cron 口径：两个都是 * 时看日；只有一个是 * 时看另一个；
// 两个都不是 * 时**取或**（"每月 1 号 或 每周一"）。这条最容易被自己发明，写死在此。
func (c cronSpec) matches(t time.Time) bool {
	if !c.minute[t.Minute()] || !c.hour[t.Hour()] {
		return false
	}
	if !c.month[int(t.Month())] {
		return false
	}
	dom := c.dom[t.Day()]
	dow := c.dow[int(t.Weekday())]
	switch {
	case c.domStar && c.dowSt:
		return dom
	case c.domStar:
		return dow
	case c.dowSt:
		return dom
	default:
		return dom || dow
	}
}

// maxScanSteps 是 nextAfter 的扫描上限（分钟数）。
//
// 取 8 年而不是 1 年，是因为「2 月 29 日」这类表达式的真实间隔可以到 8 年
// （2096-02-29 → 2104-02-29，中间那四个平年没有 2 月 29 日）。上限定在 1 年的话，
// 一个**合法**的闰日表达式会被判成「无解」，运行结果就是任务永远不触发。
// 到达上限才回报 false——**不静默返回一个「看起来合理」的 far-future 时刻**。
const maxScanSteps = 8 * 366 * 24 * 60

// nextAfter 返回 t 之后的第一次触发（分钟级，逐分推进）。
func nextAfter(t time.Time, c cronSpec) (time.Time, bool) {
	cur := t.Truncate(time.Minute).Add(time.Minute)
	for i := 0; i < maxScanSteps; i++ {
		if c.matches(cur) {
			return cur, true
		}
		cur = cur.Add(time.Minute)
	}
	return time.Time{}, false
}

// prevBefore 返回 t **之前**的最后一次触发（nextAfter 的镜像）。
//
// 存在它的理由是一个实测到的事故：nextAfter 只会返回未来时刻，所以「用 nextAfter 的结果
// 反推它是不是已经过去了」这种写法恒为假——catch_up 会变成一段永远走不到的死代码。
// 判断「错过了哪一次」必须独立地往回扫。
func prevBefore(t time.Time, c cronSpec) (time.Time, bool) {
	cur := t.Truncate(time.Minute).Add(-time.Minute)
	for i := 0; i < maxScanSteps; i++ {
		if c.matches(cur) {
			return cur, true
		}
		cur = cur.Add(-time.Minute)
	}
	return time.Time{}, false
}
