package engine

// purefunc_test.go —— 纯函数核心原子（time / template / crypto）的行为与 fail-closed 边界。
//
// 重点不是「算法算得对」（标准库负责），而是**接错了要报错而不是静默降级**：
// 未知算法/未知格式/未知时区/长度非法的密钥/缺必填/非法 base64 一律返回错误，
// 因为 Lua 直调路径不经过 validatePrimitiveArgs，实现自身就是最后一道闸。

import (
	"strings"
	"testing"
	"time"
)

// pureCall 调一次纯函数原子（测试直接进唯一调度点）。
func pureCall(t *testing.T, prim, method string, args map[string]interface{}) (interface{}, error) {
	t.Helper()
	return callPureFunc(prim, method, args)
}

// ---- time ----

func TestPureTimeFormatParseRoundTrip(t *testing.T) {
	v, err := pureCall(t, "time", "format", map[string]interface{}{
		"unix": float64(0), "format": "date", "tz": "UTC",
	})
	if err != nil {
		t.Fatalf("format 应成功: %v", err)
	}
	if v != "1970-01-01" {
		t.Fatalf("format(unix=0, date, UTC) 应为 1970-01-01，实际 %v", v)
	}

	// 与 format 对偶：把同一语义的时间串解析回 Unix 秒
	p, err := pureCall(t, "time", "parse", map[string]interface{}{
		"value": "1970-01-02", "format": "date", "tz": "UTC",
	})
	if err != nil {
		t.Fatalf("parse 应成功: %v", err)
	}
	if p != int64(86400) {
		t.Fatalf("parse(1970-01-02, date, UTC) 应为 86400，实际 %v (%T)", p, p)
	}

	// unix_ms 形态：毫秒时间戳
	ms, err := pureCall(t, "time", "format", map[string]interface{}{
		"unix": float64(1), "format": "unix_ms",
	})
	if err != nil {
		t.Fatalf("unix_ms 应成功: %v", err)
	}
	if ms != int64(1000) {
		t.Fatalf("unix=1 的 unix_ms 应为 1000，实际 %v", ms)
	}
}

func TestPureTimeNowIsCurrent(t *testing.T) {
	v, err := pureCall(t, "time", "now", map[string]interface{}{"format": "date", "tz": "UTC"})
	if err != nil {
		t.Fatalf("now 应成功: %v", err)
	}
	if v != time.Now().UTC().Format("2006-01-02") {
		t.Fatalf("now(date, UTC) 应为今天（UTC），实际 %v", v)
	}
	// 默认格式（rfc3339）+ 默认时区（Local）：能被解析回来
	raw, err := pureCall(t, "time", "now", nil)
	if err != nil {
		t.Fatalf("now 默认参数应成功: %v", err)
	}
	if _, err := time.Parse(time.RFC3339, raw.(string)); err != nil {
		t.Fatalf("now 默认格式应为 RFC3339，实际 %v（%v）", raw, err)
	}
}

// 枚举/参数非法必须报错——静默回退（比如未知时区当 UTC 用）会让时间语义悄悄跑偏。
func TestPureTimeFailClosed(t *testing.T) {
	cases := []struct {
		name string
		fn   func() (interface{}, error)
	}{
		{"未知 format", func() (interface{}, error) {
			return pureCall(t, "time", "format", map[string]interface{}{"unix": float64(0), "format": "nope"})
		}},
		{"未知时区", func() (interface{}, error) {
			return pureCall(t, "time", "now", map[string]interface{}{"tz": "Mars/Phobos"})
		}},
		{"unix 与 ms 同时给", func() (interface{}, error) {
			return pureCall(t, "time", "format", map[string]interface{}{"unix": float64(0), "ms": float64(1)})
		}},
		{"unix/ms 都没给", func() (interface{}, error) {
			return pureCall(t, "time", "format", map[string]interface{}{"format": "date"})
		}},
		{"parse 缺 value", func() (interface{}, error) {
			return pureCall(t, "time", "parse", map[string]interface{}{})
		}},
		{"parse 格式不符", func() (interface{}, error) {
			return pureCall(t, "time", "parse", map[string]interface{}{"value": "not-a-date", "format": "date", "tz": "UTC"})
		}},
		{"sleep 零秒", func() (interface{}, error) {
			return pureCall(t, "time", "sleep", map[string]interface{}{"seconds": float64(0)})
		}},
		{"sleep 超上限", func() (interface{}, error) {
			return pureCall(t, "time", "sleep", map[string]interface{}{"seconds": float64(maxSleepSeconds + 1)})
		}},
		{"sleep 缺参", func() (interface{}, error) {
			return pureCall(t, "time", "sleep", map[string]interface{}{})
		}},
		{"未知方法", func() (interface{}, error) { return pureCall(t, "time", "travel", nil) }},
	}
	for _, tc := range cases {
		if _, err := tc.fn(); err == nil {
			t.Fatalf("[%s] 期望报错，实际成功", tc.name)
		}
	}
}

func TestPureTimeSleepOK(t *testing.T) {
	start := time.Now()
	v, err := pureCall(t, "time", "sleep", map[string]interface{}{"seconds": float64(0.01)})
	if err != nil {
		t.Fatalf("sleep 应成功: %v", err)
	}
	if v != true {
		t.Fatalf("sleep 应返回 true，实际 %v", v)
	}
	if time.Since(start) < 10*time.Millisecond {
		t.Fatal("sleep(0.01) 应至少等待 10ms")
	}
}

// ---- template ----

func TestPureTemplateRender(t *testing.T) {
	v, err := pureCall(t, "template", "render", map[string]interface{}{
		"text": "hello ${vars.name} / ${vars.obj.k}!",
		"vars": map[string]interface{}{"name": "world", "obj": map[string]interface{}{"k": 7}},
	})
	if err != nil {
		t.Fatalf("render 应成功: %v", err)
	}
	if v != "hello world / 7!" {
		t.Fatalf("渲染结果异常: %v", v)
	}
	// 没有引用时原样返回
	if v, err := pureCall(t, "template", "render", map[string]interface{}{"text": "plain"}); err != nil || v != "plain" {
		t.Fatalf("无引用应原样返回，实际 %v / %v", v, err)
	}
}

// 只支持 ${vars.*}：steps/secrets 前缀必须报错并提示替代写法（不静默留空）。
func TestPureTemplateRenderFailClosed(t *testing.T) {
	cases := []struct {
		name string
		args map[string]interface{}
		want string
	}{
		{"缺 text", map[string]interface{}{}, "text"},
		{"缺键", map[string]interface{}{"text": "${vars.nope}", "vars": map[string]interface{}{}}, "键不存在"},
		{"深层缺键", map[string]interface{}{"text": "${vars.a.b}", "vars": map[string]interface{}{"a": "x"}}, "不能继续下钻"},
		{"steps 前缀", map[string]interface{}{"text": "${steps.s1.out}"}, "只支持"},
		{"secrets 前缀", map[string]interface{}{"text": "${secrets.KEY}"}, "只支持"},
		{"未知前缀", map[string]interface{}{"text": "${nope.x}"}, "只支持"},
		{"未闭合", map[string]interface{}{"text": "${vars.a"}, "未闭合"},
		{"vars 非 object", map[string]interface{}{"text": "${vars.a}", "vars": "x"}, "object"},
	}
	for _, tc := range cases {
		_, err := pureCall(t, "template", "render", tc.args)
		if err == nil {
			t.Fatalf("[%s] 期望报错，实际成功", tc.name)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("[%s] 错误信息应含 %q，实际: %v", tc.name, tc.want, err)
		}
	}
}

// ---- crypto ----

// 用国标/权威测试向量校验摘要链路（不是拿同库自比）。
func TestPureCryptoHashVectors(t *testing.T) {
	cases := []struct{ algo, want string }{
		{"md5", "900150983cd24fb0d6963f7d28e17f72"},
		{"sha1", "a9993e364706816aba3e25717850c26c9cd0d89d"},
		{"sha256", "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"},
	}
	for _, tc := range cases {
		v, err := pureCall(t, "crypto", "hash", map[string]interface{}{"algo": tc.algo, "data": "abc"})
		if err != nil {
			t.Fatalf("%s 应成功: %v", tc.algo, err)
		}
		if v != tc.want {
			t.Fatalf("%s(abc) 期望 %s，实际 %v", tc.algo, tc.want, v)
		}
	}
	// sha512：长度与 hex 合法性（避免抄错长向量）
	v, err := pureCall(t, "crypto", "hash", map[string]interface{}{"algo": "sha512", "data": "abc"})
	if err != nil {
		t.Fatalf("sha512 应成功: %v", err)
	}
	if s, ok := v.(string); !ok || len(s) != 128 {
		t.Fatalf("sha512 应为 128 位 hex，实际 %v", v)
	}
	// base64 输出编码（与 hex 值可互相换算）
	b64, err := pureCall(t, "crypto", "hash", map[string]interface{}{
		"algo": "sha256", "data": "abc", "encoding": "base64",
	})
	if err != nil {
		t.Fatalf("base64 输出应成功: %v", err)
	}
	if b64 == "" || b64 == "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Fatalf("base64 编码未生效: %v", b64)
	}
}

// RFC 4231 Test Case 1：HMAC-SHA256(key=0x0b*20, data="Hi There")。
const (
	rfc4231Key  = "\x0b\x0b\x0b\x0b\x0b\x0b\x0b\x0b\x0b\x0b\x0b\x0b\x0b\x0b\x0b\x0b\x0b\x0b\x0b\x0b"
	rfc4231Data = "Hi There"
	rfc4231Sum  = "b0344c61d8db38535ca8afceaf0bf12b881dc200c9833da726e9376c2e32cff7"
)

func TestPureCryptoHMAC(t *testing.T) {
	sig, err := pureCall(t, "crypto", "hmac", map[string]interface{}{
		"algo": "sha256", "key": rfc4231Key, "data": rfc4231Data,
	})
	if err != nil {
		t.Fatalf("hmac 应成功: %v", err)
	}
	if sig != rfc4231Sum {
		t.Fatalf("HMAC-SHA256 期望 %s，实际 %v", rfc4231Sum, sig)
	}
	ok, err := pureCall(t, "crypto", "hmac_verify", map[string]interface{}{
		"algo": "sha256", "key": rfc4231Key, "data": rfc4231Data, "signature": rfc4231Sum,
	})
	if err != nil {
		t.Fatalf("hmac_verify 应成功: %v", err)
	}
	if ok != true {
		t.Fatalf("正确签名应验签通过，实际 %v", ok)
	}
	bad, err := pureCall(t, "crypto", "hmac_verify", map[string]interface{}{
		"algo": "sha256", "key": rfc4231Key, "data": rfc4231Data + "x", "signature": rfc4231Sum,
	})
	if err != nil {
		t.Fatalf("hmac_verify 应成功（结果 false）: %v", err)
	}
	if bad != false {
		t.Fatalf("被篡改的数据应验签失败，实际 %v", bad)
	}
	// base64 形态的签名也要能验
	raw, err := pureCall(t, "crypto", "hmac", map[string]interface{}{
		"algo": "sha256", "key": rfc4231Key, "data": rfc4231Data, "encoding": "base64",
	})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := pureCall(t, "crypto", "hmac_verify", map[string]interface{}{
		"algo": "sha256", "key": rfc4231Key, "data": rfc4231Data, "signature": raw, "encoding": "base64",
	}); err != nil || ok != true {
		t.Fatalf("base64 签名应验签通过，实际 %v / %v", ok, err)
	}
}

func TestPureCryptoAESGCMRoundTrip(t *testing.T) {
	key := strings.Repeat("k", 32) // 32 字节 → AES-256
	ct, err := pureCall(t, "crypto", "encrypt", map[string]interface{}{
		"key": key, "plaintext": "机密内容", "aad": "tenant=1",
	})
	if err != nil {
		t.Fatalf("encrypt 应成功: %v", err)
	}
	// 每次加密的 nonce 随机：同明文两次密文必须不同（否则等于没加随机性）
	ct2, err := pureCall(t, "crypto", "encrypt", map[string]interface{}{
		"key": key, "plaintext": "机密内容", "aad": "tenant=1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if ct == ct2 {
		t.Fatal("两次加密结果相同：nonce 未随机化")
	}
	pt, err := pureCall(t, "crypto", "decrypt", map[string]interface{}{
		"key": key, "ciphertext": ct, "aad": "tenant=1",
	})
	if err != nil {
		t.Fatalf("decrypt 应成功: %v", err)
	}
	if pt != "机密内容" {
		t.Fatalf("解密结果异常: %v", pt)
	}
	// AAD 不匹配 → 解密失败（GCM 认证失败必须报错，不能返回部分明文）
	if _, err := pureCall(t, "crypto", "decrypt", map[string]interface{}{
		"key": key, "ciphertext": ct, "aad": "tenant=2",
	}); err == nil {
		t.Fatal("AAD 不匹配应解密失败")
	}
	// 换密钥 → 失败
	if _, err := pureCall(t, "crypto", "decrypt", map[string]interface{}{
		"key": strings.Repeat("x", 32), "ciphertext": ct, "aad": "tenant=1",
	}); err == nil {
		t.Fatal("错误密钥应解密失败")
	}
}

func TestPureCryptoFailClosed(t *testing.T) {
	cases := []struct {
		name string
		prim string
		meth string
		args map[string]interface{}
	}{
		{"未知摘要算法", "crypto", "hash", map[string]interface{}{"algo": "crc32", "data": "x"}},
		{"未知 HMAC 算法", "crypto", "hmac", map[string]interface{}{"algo": "md5", "key": "k", "data": "x"}},
		{"缺 data", "crypto", "hash", map[string]interface{}{"algo": "sha256"}},
		{"缺 key", "crypto", "hmac", map[string]interface{}{"algo": "sha256", "data": "x"}},
		{"未知 encoding", "crypto", "hash", map[string]interface{}{"algo": "sha256", "data": "x", "encoding": "rot13"}},
		{"密钥长度非法", "crypto", "encrypt", map[string]interface{}{"key": "short", "plaintext": "x"}},
		{"key_encoding 非法", "crypto", "encrypt", map[string]interface{}{"key": strings.Repeat("k", 32), "plaintext": "x", "key_encoding": "rot13"}},
		{"hex key 非法", "crypto", "encrypt", map[string]interface{}{"key": "zz", "plaintext": "x", "key_encoding": "hex"}},
		{"密文非 base64", "crypto", "decrypt", map[string]interface{}{"key": strings.Repeat("k", 32), "ciphertext": "!!!not-base64!!!"}},
		{"密文过短", "crypto", "decrypt", map[string]interface{}{"key": strings.Repeat("k", 32), "ciphertext": "AAAA"}},
		{"签名非 hex", "crypto", "hmac_verify", map[string]interface{}{"algo": "sha256", "key": "k", "data": "x", "signature": "zz"}},
		{"random 超上限", "crypto", "random", map[string]interface{}{"bytes": float64(maxRandomBytes + 1)}},
		{"random 非整数", "crypto", "random", map[string]interface{}{"bytes": float64(1.5)}},
		{"base64_decode 非法", "crypto", "base64_decode", map[string]interface{}{"data": "!!!not-base64!!!"}},
		{"base64 variant 非法", "crypto", "base64_encode", map[string]interface{}{"data": "x", "variant": "rot13"}},
		{"未知方法", "crypto", "sign", map[string]interface{}{}},
		{"未知原子", "nope", "x", map[string]interface{}{}},
	}
	for _, tc := range cases {
		if _, err := pureCall(t, tc.prim, tc.meth, tc.args); err == nil {
			t.Fatalf("[%s] 期望报错，实际成功", tc.name)
		}
	}
}

func TestPureCryptoRandomAndBase64(t *testing.T) {
	v, err := pureCall(t, "crypto", "random", map[string]interface{}{"bytes": float64(8)})
	if err != nil {
		t.Fatalf("random 应成功: %v", err)
	}
	if s, ok := v.(string); !ok || len(s) != 16 {
		t.Fatalf("8 字节应输出 16 位 hex，实际 %v", v)
	}
	// 默认 32 字节
	if v, err := pureCall(t, "crypto", "random", nil); err != nil {
		t.Fatal(err)
	} else if len(v.(string)) != 64 {
		t.Fatalf("默认应输出 64 位 hex，实际 %v", v)
	}
	// base64 往返（含 URL-safe 变体）
	for _, variant := range []string{"std", "url"} {
		enc, err := pureCall(t, "crypto", "base64_encode", map[string]interface{}{"data": "中文+/-?", "variant": variant})
		if err != nil {
			t.Fatalf("base64_encode(%s) 应成功: %v", variant, err)
		}
		dec, err := pureCall(t, "crypto", "base64_decode", map[string]interface{}{"data": enc, "variant": variant})
		if err != nil {
			t.Fatalf("base64_decode(%s) 应成功: %v", variant, err)
		}
		if dec != "中文+/-?" {
			t.Fatalf("base64 往返异常(%s): %v", variant, dec)
		}
	}
}
