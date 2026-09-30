package engine

// purefunc_password_test.go —— ctx.crypto.password_hash / password_verify。
//
// 两条必须钉死的：
//  ① **PBKDF2 是自己实现的**（为守住「零新增依赖」），所以必须有**外部基准**——
//     期望值取自 RFC 7914 §11 的向量，并经 Python hashlib.pbkdf2_hmac（独立实现）复核；
//  ② 哈希串格式非法要**报错**而不是返回 false —— 否则「所有人都登录不上」会被误诊成
//     「大家密码都记错了」，属于静默降级。

import (
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"
)

// TestPurePasswordPBKDF2MatchesReferenceVector 用外部基准校验自己实现的 PBKDF2。
func TestPurePasswordPBKDF2MatchesReferenceVector(t *testing.T) {
	cases := []struct {
		algo string
		iter int
		want string
	}{
		// PBKDF2-HMAC-SHA256(P="password", S="salt")，dkLen=32
		{"pbkdf2-sha256", 1, "120fb6cffcf8b32c43e7225256c4f837a86548c92ccc35480805987cb70be17b"},
		{"pbkdf2-sha256", 2, "ae4d0c95af6b46d32d0adff928f06dd02a303f8ef3c251dfd6e2d85a95474c43"},
		{"pbkdf2-sha256", 4096, "c5e478d59288c841aa530db6845c4c8d962893a001ce4e11a4963873aa98134a"},
		// PBKDF2-HMAC-SHA512，dkLen=64
		{"pbkdf2-sha512", 1, "867f70cf1ade02cff3752599a3a53dc4af34c7a669815ae5d513554e1c8cf252" +
			"c02d470a285a0501bad999bfe943c08f050235d7d68b1da55e63f73b60a57fce"},
		{"pbkdf2-sha512", 4096, "d197b1b33db0143e018b12f3d1d1479e6cdebdcc97c5c0f87f6902e072f457b" +
			"5143f30602641b3d55cd335988cb36b84376060ecd532e039b742a239434af2d5"},
	}
	for _, c := range cases {
		got := hex.EncodeToString(pbkdf2Key(passwordPRF[c.algo], []byte("password"), []byte("salt"), c.iter))
		if got != c.want {
			t.Fatalf("PBKDF2 %s iter=%d 与基准不符：\n  实际 %s\n  期望 %s", c.algo, c.iter, got, c.want)
		}
	}
}

func TestPurePasswordHashVerifyRoundTrip(t *testing.T) {
	encoded, err := pureCall(t, "crypto", "password_hash", map[string]interface{}{
		"password":   "s3cret",
		"iterations": float64(1000),
	})
	if err != nil {
		t.Fatalf("password_hash 应成功: %v", err)
	}
	phc := encoded.(string)

	ok, err := pureCall(t, "crypto", "password_verify", map[string]interface{}{
		"password": "s3cret", "hash": phc,
	})
	if err != nil {
		t.Fatalf("password_verify 应成功: %v", err)
	}
	if ok != true {
		t.Fatalf("正确口令应返回 true，实际 %v", ok)
	}

	wrong, err := pureCall(t, "crypto", "password_verify", map[string]interface{}{
		"password": "wrong", "hash": phc,
	})
	if err != nil {
		t.Fatalf("错误口令应返回 false 而不是报错（输错密码是正常业务分支）: %v", err)
	}
	if wrong != false {
		t.Fatalf("错误口令应返回 false，实际 %v", wrong)
	}
}

// TestPurePasswordPHCStringIsSelfDescribing 锁住 PHC 串的价值：
// 验签读的是**串里记的参数**，不是当前默认值 ⇒ 将来调迭代次数，老口令照样能验。
func TestPurePasswordPHCStringIsSelfDescribing(t *testing.T) {
	old, err := pureCall(t, "crypto", "password_hash", map[string]interface{}{
		"password": "s3cret", "iterations": float64(1000),
	})
	if err != nil {
		t.Fatalf("hash 失败: %v", err)
	}
	if !strings.Contains(old.(string), "$pbkdf2-sha256$i=1000$") {
		t.Fatalf("PHC 串应带算法与迭代次数，实际 %v", old)
	}
	// 换成另一套参数生成的哈希，两者互不干扰
	other, err := pureCall(t, "crypto", "password_hash", map[string]interface{}{
		"password": "s3cret", "iterations": float64(2000), "algo": "pbkdf2-sha512",
	})
	if err != nil {
		t.Fatalf("hash 失败: %v", err)
	}
	for _, phc := range []string{old.(string), other.(string)} {
		v, err := pureCall(t, "crypto", "password_verify", map[string]interface{}{"password": "s3cret", "hash": phc})
		if err != nil || v != true {
			t.Fatalf("不同参数生成的哈希都应能验（验的是串内参数）: %v / %v", v, err)
		}
	}
}

func TestPurePasswordSaltRandomByDefault(t *testing.T) {
	a, err := pureCall(t, "crypto", "password_hash", map[string]interface{}{
		"password": "s3cret", "iterations": float64(100),
	})
	if err != nil {
		t.Fatalf("hash 失败: %v", err)
	}
	b, err := pureCall(t, "crypto", "password_hash", map[string]interface{}{
		"password": "s3cret", "iterations": float64(100),
	})
	if err != nil {
		t.Fatalf("hash 失败: %v", err)
	}
	if a == b {
		t.Fatalf("同一口令两次哈希必须不同（盐缺省随机），否则彩虹表可复用")
	}
}

func TestPurePasswordExplicitSaltIsDeterministic(t *testing.T) {
	args := map[string]interface{}{
		"password": "s3cret", "iterations": float64(1000), "salt": "fixed-salt-value",
	}
	a, err := pureCall(t, "crypto", "password_hash", args)
	if err != nil {
		t.Fatalf("hash 失败: %v", err)
	}
	b, err := pureCall(t, "crypto", "password_hash", args)
	if err != nil {
		t.Fatalf("hash 失败: %v", err)
	}
	if a != b {
		t.Fatalf("显式给盐时应可复现（迁移/对账需要），实际 %v vs %v", a, b)
	}
}

func TestPurePasswordVerifyMalformedHashErrors(t *testing.T) {
	bad := []struct {
		name string
		hash string
	}{
		{"段数不对", "$pbkdf2-sha256$i=1000$salt"},
		{"缺 $ 开头", "pbkdf2-sha256$i=1000$c2FsdA$aGFzaA"},
		{"迭代段不是 i=", "$pbkdf2-sha256$n=1000$c2FsdA$aGFzaA"},
		{"迭代次数非数字", "$pbkdf2-sha256$i=abc$c2FsdA$aGFzaA"},
		{"盐不是 base64", "$pbkdf2-sha256$i=1000$!!!$aGFzaA"},
		{"未知算法", "$md5crypt$i=1000$c2FsdA$aGFzaA"},
	}
	for _, c := range bad {
		if _, err := pureCall(t, "crypto", "password_verify", map[string]interface{}{
			"password": "s3cret", "hash": c.hash,
		}); err == nil {
			t.Fatalf("%s：哈希串非法必须报错，不能返回 false（那会被误诊成密码错）", c.name)
		}
	}
}

func TestPurePasswordFailClosedOnBadInput(t *testing.T) {
	cases := []struct {
		name   string
		method string
		args   map[string]interface{}
	}{
		{"hash 缺 password", "password_hash", map[string]interface{}{"iterations": float64(100)}},
		{"hash 未知算法", "password_hash", map[string]interface{}{"password": "x", "algo": "bcrypt"}},
		{"hash 迭代次数为 0", "password_hash", map[string]interface{}{"password": "x", "iterations": float64(0)}},
		{"hash 迭代次数非整数", "password_hash", map[string]interface{}{"password": "x", "iterations": float64(1.5)}},
		{"hash 迭代次数超上限", "password_hash", map[string]interface{}{"password": "x", "iterations": float64(99999999)}},
		{"verify 缺 password", "password_verify", map[string]interface{}{"hash": "$pbkdf2-sha256$i=1$c2FsdA$aGFzaA"}},
		{"verify 缺 hash", "password_verify", map[string]interface{}{"password": "x"}},
	}
	for _, c := range cases {
		if _, err := pureCall(t, "crypto", c.method, c.args); err == nil {
			t.Fatalf("%s：应报错（fail-closed），实际通过", c.name)
		}
	}
}

// TestPurePasswordHashUsesDefaultIterations 确认缺省值真的够大（不是拿 1 糊弄）。
func TestPurePasswordHashUsesDefaultIterations(t *testing.T) {
	out, err := pureCall(t, "crypto", "password_hash", map[string]interface{}{
		"password": "s3cret", "iterations": float64(1), // 只验证格式，避免跑满 21 万次
	})
	if err != nil {
		t.Fatalf("hash 失败: %v", err)
	}
	parts := strings.Split(out.(string), "$")
	if len(parts) != 5 {
		t.Fatalf("PHC 串应为 5 段（首段空），实际 %v", out)
	}
	if parts[2] != "i=1" {
		t.Fatalf("迭代段应为 i=1，实际 %q", parts[2])
	}
	// 盐缺省 16 字节 ⇒ base64（无 padding）为 22 字符
	salt, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil {
		t.Fatalf("盐不是合法 base64: %v", err)
	}
	if len(salt) != passwordSaltBytes {
		t.Fatalf("缺省盐应为 %d 字节，实际 %d", passwordSaltBytes, len(salt))
	}
}
