package engine

// purefunc_id_test.go —— 纯算法族 ctx.id.*（标识符生成）。
//
// 重点：① UUID v4 必须真的置上 version/variant 位（不是「16 随机字节转 hex」就算数）；
// ② 随机取字符**无模偏倚**（用 rand.Int 而非取模）；③ 字母表/长度非法一律报错。

import (
	"regexp"
	"strings"
	"testing"
)

var uuidV4Pattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestPureIDUUIDIsV4(t *testing.T) {
	// 跑多次：UUID 是随机的，一次通过说明不了 version/variant 位被正确置上。
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		out, err := pureCall(t, "id", "uuid", map[string]interface{}{})
		if err != nil {
			t.Fatalf("id.uuid 应成功: %v", err)
		}
		s, ok := out.(string)
		if !ok {
			t.Fatalf("id.uuid 应返回字符串，实际 %T", out)
		}
		if !uuidV4Pattern.MatchString(s) {
			t.Fatalf("不是合法的 UUID v4（version/variant 位可能没置上）: %q", s)
		}
		seen[s] = true
	}
	if len(seen) != 50 {
		t.Fatalf("50 次生成应互不相同，实际只有 %d 个不同值", len(seen))
	}
}

func TestPureIDUUIDFormats(t *testing.T) {
	cases := []struct {
		format string
		prefix string
		suffix string
		len    int
	}{
		{"plain", "", "", 36},
		{"braced", "{", "}", 38},
		{"urn", "urn:uuid:", "", 45},
		{"hex", "", "", 32},
	}
	for _, c := range cases {
		out, err := pureCall(t, "id", "uuid", map[string]interface{}{"format": c.format})
		if err != nil {
			t.Fatalf("format=%s 应成功: %v", c.format, err)
		}
		s := out.(string)
		if len(s) != c.len {
			t.Fatalf("format=%s 长度应为 %d，实际 %d（%q）", c.format, c.len, len(s), s)
		}
		if !strings.HasPrefix(s, c.prefix) || !strings.HasSuffix(s, c.suffix) {
			t.Fatalf("format=%s 的前后缀不符：%q", c.format, s)
		}
		if c.format != "hex" && !uuidV4Pattern.MatchString(strings.TrimSuffix(strings.TrimPrefix(s, c.prefix), c.suffix)) {
			t.Fatalf("format=%s 去掉前后缀后应是合法 UUID v4：%q", c.format, s)
		}
	}
}

func TestPureIDNanoID(t *testing.T) {
	out, err := pureCall(t, "id", "nanoid", map[string]interface{}{})
	if err != nil {
		t.Fatalf("id.nanoid 应成功: %v", err)
	}
	s := out.(string)
	if len(s) != 21 {
		t.Fatalf("默认长度应为 21，实际 %d（%q）", len(s), s)
	}
	for _, r := range s {
		if !strings.ContainsRune(idNanoIDAlphabet, r) {
			t.Fatalf("默认字母表不含 %q：%q", r, s)
		}
	}

	// 自定义长度与字母表
	custom, err := pureCall(t, "id", "nanoid", map[string]interface{}{
		"size": float64(8), "alphabet": "ABC",
	})
	if err != nil {
		t.Fatalf("自定义 nanoid 应成功: %v", err)
	}
	if custom.(string) == "" || len(custom.(string)) != 8 {
		t.Fatalf("自定义长度应为 8，实际 %v", custom)
	}
	for _, r := range custom.(string) {
		if r != 'A' && r != 'B' && r != 'C' {
			t.Fatalf("应只用自定义字母表，实际 %q", custom)
		}
	}
}

func TestPureIDCode(t *testing.T) {
	out, err := pureCall(t, "id", "code", map[string]interface{}{})
	if err != nil {
		t.Fatalf("id.code 应成功: %v", err)
	}
	s := out.(string)
	if len(s) != 6 {
		t.Fatalf("默认长度应为 6，实际 %d（%q）", len(s), s)
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			t.Fatalf("默认字符集应为数字，实际 %q", s)
		}
	}

	for _, cs := range idCharsetNames {
		v, err := pureCall(t, "id", "code", map[string]interface{}{"charset": cs, "length": float64(12)})
		if err != nil {
			t.Fatalf("charset=%s 应成功: %v", cs, err)
		}
		got := v.(string)
		want := idCharsets[cs]
		if len(got) != 12 {
			t.Fatalf("charset=%s 长度应为 12，实际 %d", cs, len(got))
		}
		for _, r := range got {
			if !strings.ContainsRune(want, r) {
				t.Fatalf("charset=%s 生成了字母表外的字符 %q：%q", cs, r, got)
			}
		}
	}
}

// TestPureIDRandomHasNoModuloBias 用「大字母表 + 大量采样」观察分布是否明显偏斜。
//
// ★ 字符集必须选 **`upper`**（26 个字符）：256 % 26 = 22 ≠ 0，取模实现（`randByte % n`）
// 会明显偏斜、能被抓到。若用 `hex`（16 个，256 % 16 == 0）这类「整除」的字母表，
// 取模恰好也不偏——测试会假绿，等于没锁。
func TestPureIDRandomHasNoModuloBias(t *testing.T) {
	const n = 26 // 字母表大小，256 % 26 != 0 ⇒ 取模实现会明显偏斜
	const samples = 26000
	counts := map[rune]int{}
	for i := 0; i < samples; i++ {
		v, err := pureCall(t, "id", "code", map[string]interface{}{
			"length": float64(1), "charset": "upper",
		})
		if err != nil {
			t.Fatalf("id.code 应成功: %v", err)
		}
		for _, r := range v.(string) {
			counts[r]++
		}
	}
	if len(counts) != n {
		t.Fatalf("upper 字符集有 %d 个字符，采样应覆盖全部，实际覆盖 %d 个", n, len(counts))
	}
	// ★ 用卡方检验，不用「每个字符 ±x%」的区间判断：
	// 取模实现在 26 字母表下偏斜是 10:9（约 +17%/-5%），±40% 的宽区间**根本抓不到**（会假绿）。
	// 卡方 Σ(obs-exp)²/exp 在 df=25 下，取 41.6 约等于 p=0.01 的临界值（留够余量）。
	want := float64(samples) / float64(n)
	var chi2 float64
	for _, c := range counts {
		d := float64(c) - want
		chi2 += d * d / want
	}
	if chi2 > 41.6 {
		t.Fatalf("字符分布卡方值 %.1f 超过阈值 41.6（df=%d，期望每字符 %.0f 次）→ 分布偏斜，可能用了取模而非拒绝采样",
			chi2, n-1, want)
	}
}

func TestPureIDFailClosedOnBadInput(t *testing.T) {
	cases := []struct {
		name   string
		method string
		args   map[string]interface{}
	}{
		{"uuid 未知格式", "uuid", map[string]interface{}{"format": "xml"}},
		{"nanoid size=0", "nanoid", map[string]interface{}{"size": float64(0)}},
		{"nanoid size 非整数", "nanoid", map[string]interface{}{"size": float64(2.5)}},
		{"nanoid size 过大", "nanoid", map[string]interface{}{"size": float64(9999)}},
		{"nanoid 字母表过短", "nanoid", map[string]interface{}{"alphabet": "A"}},
		{"nanoid 字母表重复", "nanoid", map[string]interface{}{"alphabet": "AAB"}},
		{"code length=0", "code", map[string]interface{}{"length": float64(0)}},
		{"code length 过大", "code", map[string]interface{}{"length": float64(999)}},
		{"code 未知字符集", "code", map[string]interface{}{"charset": "emoji"}},
		{"未知方法", "uuid7", map[string]interface{}{}},
	}
	for _, c := range cases {
		if _, err := pureCall(t, "id", c.method, c.args); err == nil {
			t.Fatalf("%s：应报错（fail-closed），实际通过", c.name)
		}
	}
}
