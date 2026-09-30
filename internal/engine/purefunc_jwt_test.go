package engine

// purefunc_jwt_test.go —— ctx.crypto.jwt_* 的行为与 fail-closed 边界。
//
// 重点不是「标准库算得对」，而是**接错了要报错而不是静默放过**：
//   ① alg=none 一律拒；② 算法集由密钥类型决定，**不由令牌决定**（防 RS256→HS256 混淆攻击）；
//   ③ 过期/未生效/iss/aud 不符一律报错；④ 验签失败返回 error 而不是 {valid=false}。
// 这几条是 JWT 的经典坑，漏一条就是漏洞，故在此用测试钉死。

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"
)

// ---- 密钥素材 ----

func jwtTestRSAPair(t *testing.T) (privPEM, pubPEM string, pub *rsa.PublicKey) {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("生成 RSA 密钥失败: %v", err)
	}
	pubDER, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatalf("序列化 RSA 公钥失败: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(priv)})),
		string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})),
		&priv.PublicKey
}

func jwtTestECPair(t *testing.T) (privPEM, pubPEM string) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("生成 EC 密钥失败: %v", err)
	}
	privDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatalf("序列化 EC 私钥失败: %v", err)
	}
	pubDER, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatalf("序列化 EC 公钥失败: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: privDER})),
		string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}))
}

func jwtTestEd25519Pair(t *testing.T) (privPEM, pubPEM string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("生成 Ed25519 密钥失败: %v", err)
	}
	privDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("序列化 Ed25519 私钥失败: %v", err)
	}
	pubDER, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatalf("序列化 Ed25519 公钥失败: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privDER})),
		string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}))
}

// jwtTestRSAJWKS 把 RSA 公钥拼成 JWKS（与 IdP 的 /jwks.json 同形态）。
func jwtTestRSAJWKS(t *testing.T, pub *rsa.PublicKey, kid string) map[string]interface{} {
	t.Helper()
	return map[string]interface{}{
		"keys": []interface{}{
			map[string]interface{}{
				"kty": "RSA",
				"kid": kid,
				"n":   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
				"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
			},
		},
	}
}

// ---- 往返：签 → 验 ----

func TestPureJWTSignVerifyHS256RoundTrip(t *testing.T) {
	tok, err := pureCall(t, "crypto", "jwt_sign", map[string]interface{}{
		"claims": map[string]interface{}{"sub": "alice", "role": "admin"},
		"alg":    "HS256",
		"key":    "topsecret",
	})
	if err != nil {
		t.Fatalf("jwt_sign 应成功: %v", err)
	}
	token, ok := tok.(string)
	if !ok || strings.Count(token, ".") != 2 {
		t.Fatalf("jwt_sign 应返回三段令牌，实际 %v", tok)
	}

	out, err := pureCall(t, "crypto", "jwt_verify", map[string]interface{}{"token": token, "key": "topsecret"})
	if err != nil {
		t.Fatalf("jwt_verify 应通过: %v", err)
	}
	m, ok := out.(map[string]interface{})
	if !ok {
		t.Fatalf("jwt_verify 应返回含 header/claims 的表，实际 %T", out)
	}
	if m["subject"] != "alice" {
		t.Fatalf("subject 应为 alice，实际 %v", m["subject"])
	}
}

func TestPureJWTSignVerifyRS256RoundTrip(t *testing.T) {
	privPEM, pubPEM, _ := jwtTestRSAPair(t)
	tok, err := pureCall(t, "crypto", "jwt_sign", map[string]interface{}{
		"claims": map[string]interface{}{"sub": "bob"},
		"alg":    "RS256",
		"key":    privPEM,
	})
	if err != nil {
		t.Fatalf("RS256 签发应成功: %v", err)
	}
	if _, err := pureCall(t, "crypto", "jwt_verify", map[string]interface{}{
		"token": tok.(string), "key": pubPEM,
	}); err != nil {
		t.Fatalf("RS256 验签应通过: %v", err)
	}
}

func TestPureJWTSignVerifyES256RoundTrip(t *testing.T) {
	privPEM, pubPEM := jwtTestECPair(t)
	tok, err := pureCall(t, "crypto", "jwt_sign", map[string]interface{}{
		"claims": map[string]interface{}{"sub": "carol"},
		"alg":    "ES256",
		"key":    privPEM,
	})
	if err != nil {
		t.Fatalf("ES256 签发应成功: %v", err)
	}
	if _, err := pureCall(t, "crypto", "jwt_verify", map[string]interface{}{
		"token": tok.(string), "key": pubPEM,
	}); err != nil {
		t.Fatalf("ES256 验签应通过（裸 r||s 签名需正确转 DER）: %v", err)
	}
}

func TestPureJWTSignVerifyEdDSARoundTrip(t *testing.T) {
	privPEM, pubPEM := jwtTestEd25519Pair(t)
	tok, err := pureCall(t, "crypto", "jwt_sign", map[string]interface{}{
		"claims": map[string]interface{}{"sub": "dave"},
		"alg":    "EdDSA",
		"key":    privPEM,
	})
	if err != nil {
		t.Fatalf("EdDSA 签发应成功: %v", err)
	}
	if _, err := pureCall(t, "crypto", "jwt_verify", map[string]interface{}{
		"token": tok.(string), "key": pubPEM,
	}); err != nil {
		t.Fatalf("EdDSA 验签应通过: %v", err)
	}
}

// ---- 负向：签名与算法 ----

// TestPureJWTVerifyRejectsAlgConfusion 是**最重要的一条**：
// 攻击者把 RS256 令牌改成 HS256，并用**公开的公钥**当作 HMAC 密钥签名 —— 若实现拿
// header.alg 决定验签算法，这一步就过了。算法集必须由密钥类型决定（安全口径 1）。
func TestPureJWTVerifyRejectsAlgConfusion(t *testing.T) {
	_, pubPEM, _ := jwtTestRSAPair(t)
	// 拿公钥当 HMAC 密钥签一个 HS256 令牌（攻击者的全部操作，公钥本来就是公开的）
	tok, err := pureCall(t, "crypto", "jwt_sign", map[string]interface{}{
		"claims": map[string]interface{}{"sub": "admin"},
		"alg":    "HS256",
		"key":    pubPEM,
	})
	if err != nil {
		t.Fatalf("构造攻击令牌（拿公钥当 HMAC 密钥）应能签出: %v", err)
	}
	_, err = pureCall(t, "crypto", "jwt_verify", map[string]interface{}{
		"token": tok.(string), "key": pubPEM,
	})
	if err == nil {
		t.Fatalf("★ 算法混淆攻击被放过了：公钥是非对称密钥，只允许 RS*/ES*/EdDSA，绝不能用它验 HS256")
	}
	// ★ 拒因必须是「算法口径」而不是碰巧签名不匹配 —— 后者说明防线没建起来，
	// 只是这次运气好。这条断言保证将来实现漂移时测试会红。
	if !strings.Contains(err.Error(), "不在允许的算法") {
		t.Fatalf("★ 拒因应是算法集限制（公钥不允许 HS*），实际是 %q —— 防线可能没建在正确的位置", err)
	}
}

func TestPureJWTVerifyRejectsAlgNone(t *testing.T) {
	// 手工拼一个 alg=none、签名段为空的令牌
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	claims := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"admin"}`))
	token := header + "." + claims + "."
	if _, err := pureCall(t, "crypto", "jwt_verify", map[string]interface{}{
		"token": token, "key": "whatever",
	}); err == nil {
		t.Fatalf("alg=none 必须被拒（未签名令牌一律不接受）")
	}
}

func TestPureJWTVerifyRejectsTamperedSignature(t *testing.T) {
	tok, err := pureCall(t, "crypto", "jwt_sign", map[string]interface{}{
		"claims": map[string]interface{}{"sub": "alice"},
		"alg":    "HS256",
		"key":    "topsecret",
	})
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	parts := strings.Split(tok.(string), ".")
	// 把签名的首字节翻转（同为合法 base64url 字符，故不会在解码阶段被拦）
	b, _ := base64.RawURLEncoding.DecodeString(parts[2])
	b[0] ^= 0x01
	parts[2] = base64.RawURLEncoding.EncodeToString(b)
	if _, err := pureCall(t, "crypto", "jwt_verify", map[string]interface{}{
		"token": strings.Join(parts, "."), "key": "topsecret",
	}); err == nil {
		t.Fatalf("篡改签名后必须验签失败")
	}
}

func TestPureJWTVerifyRejectsWrongKey(t *testing.T) {
	tok, err := pureCall(t, "crypto", "jwt_sign", map[string]interface{}{
		"claims": map[string]interface{}{"sub": "alice"},
		"alg":    "HS256",
		"key":    "topsecret",
	})
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	if _, err := pureCall(t, "crypto", "jwt_verify", map[string]interface{}{
		"token": tok.(string), "key": "wrongkey",
	}); err == nil {
		t.Fatalf("密钥不符必须验签失败")
	}
}

// ---- 负向：claims ----

func TestPureJWTVerifyRejectsExpired(t *testing.T) {
	tok, err := pureCall(t, "crypto", "jwt_sign", map[string]interface{}{
		"claims": map[string]interface{}{"sub": "alice", "exp": float64(time.Now().Unix() - 60)},
		"alg":    "HS256",
		"key":    "topsecret",
	})
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	if _, err := pureCall(t, "crypto", "jwt_verify", map[string]interface{}{
		"token": tok.(string), "key": "topsecret",
	}); err == nil || !strings.Contains(err.Error(), "过期") {
		t.Fatalf("过期令牌必须报「过期」，实际 %v", err)
	}
}

func TestPureJWTVerifyLeewayToleratesSmallSkew(t *testing.T) {
	tok, err := pureCall(t, "crypto", "jwt_sign", map[string]interface{}{
		"claims": map[string]interface{}{"sub": "alice", "exp": float64(time.Now().Unix() - 30)},
		"alg":    "HS256",
		"key":    "topsecret",
	})
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	// leeway 默认 0，故先确认严格模式下被拒
	if _, err := pureCall(t, "crypto", "jwt_verify", map[string]interface{}{
		"token": tok.(string), "key": "topsecret",
	}); err == nil {
		t.Fatalf("leeway 缺省应为 0（不宽容），过期令牌必须被拒")
	}
	if _, err := pureCall(t, "crypto", "jwt_verify", map[string]interface{}{
		"token": tok.(string), "key": "topsecret", "leeway": float64(60),
	}); err != nil {
		t.Fatalf("leeway=60 应容忍 30 秒的偏移: %v", err)
	}
}

func TestPureJWTVerifyRejectsNotYetValid(t *testing.T) {
	tok, err := pureCall(t, "crypto", "jwt_sign", map[string]interface{}{
		"claims": map[string]interface{}{"sub": "alice", "nbf": float64(time.Now().Unix() + 600)},
		"alg":    "HS256",
		"key":    "topsecret",
	})
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	if _, err := pureCall(t, "crypto", "jwt_verify", map[string]interface{}{
		"token": tok.(string), "key": "topsecret",
	}); err == nil || !strings.Contains(err.Error(), "尚未生效") {
		t.Fatalf("未生效令牌必须报「尚未生效」，实际 %v", err)
	}
}

func TestPureJWTVerifyIssuerAndAudience(t *testing.T) {
	tok, err := pureCall(t, "crypto", "jwt_sign", map[string]interface{}{
		"claims": map[string]interface{}{"sub": "alice", "iss": "https://idp.example", "aud": "gear"},
		"alg":    "HS256",
		"key":    "topsecret",
	})
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	token := tok.(string)
	if _, err := pureCall(t, "crypto", "jwt_verify", map[string]interface{}{
		"token": token, "key": "topsecret", "iss": "https://idp.example", "aud": "gear",
	}); err != nil {
		t.Fatalf("iss/aud 均相符应通过: %v", err)
	}
	if _, err := pureCall(t, "crypto", "jwt_verify", map[string]interface{}{
		"token": token, "key": "topsecret", "iss": "https://evil.example",
	}); err == nil {
		t.Fatalf("iss 不符必须被拒")
	}
	if _, err := pureCall(t, "crypto", "jwt_verify", map[string]interface{}{
		"token": token, "key": "topsecret", "aud": "other-app",
	}); err == nil {
		t.Fatalf("aud 不符必须被拒")
	}
}

func TestPureJWTVerifyAudienceArrayContains(t *testing.T) {
	tok, err := pureCall(t, "crypto", "jwt_sign", map[string]interface{}{
		"claims": map[string]interface{}{"sub": "alice", "aud": []interface{}{"a", "gear", "b"}},
		"alg":    "HS256",
		"key":    "topsecret",
	})
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	if _, err := pureCall(t, "crypto", "jwt_verify", map[string]interface{}{
		"token": tok.(string), "key": "topsecret", "aud": "gear",
	}); err != nil {
		t.Fatalf("aud 为数组时应判断「包含」而非「相等」: %v", err)
	}
}

// ---- JWKS ----

func TestPureJWTVerifyJWKSByKid(t *testing.T) {
	privPEM, _, pub := jwtTestRSAPair(t)
	tok, err := pureCall(t, "crypto", "jwt_sign", map[string]interface{}{
		"claims": map[string]interface{}{"sub": "alice"},
		"alg":    "RS256",
		"key":    privPEM,
		"kid":    "key-1",
	})
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	if _, err := pureCall(t, "crypto", "jwt_verify", map[string]interface{}{
		"token": tok.(string), "jwks": jwtTestRSAJWKS(t, pub, "key-1"),
	}); err != nil {
		t.Fatalf("JWKS 按 kid 验签应通过: %v", err)
	}
}

func TestPureJWTVerifyJWKSAmbiguousRejects(t *testing.T) {
	privPEM, _, pub := jwtTestRSAPair(t)
	tok, err := pureCall(t, "crypto", "jwt_sign", map[string]interface{}{
		"claims": map[string]interface{}{"sub": "alice"},
		"alg":    "RS256",
		"key":    privPEM,
	})
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	// 两把钥匙且令牌没带 kid ⇒ 不猜，直接报错
	jwks := jwtTestRSAJWKS(t, pub, "key-1")
	jwks["keys"] = append(jwks["keys"].([]interface{}), jwtTestRSAJWKS(t, pub, "key-2")["keys"].([]interface{})[0])
	if _, err := pureCall(t, "crypto", "jwt_verify", map[string]interface{}{
		"token": tok.(string), "jwks": jwks,
	}); err == nil {
		t.Fatalf("多把钥匙且无 kid 时必须报错（不猜用哪把）")
	}
}

// ---- 入参校验（fail-closed）----

func TestPureJWTFailClosedOnBadInput(t *testing.T) {
	cases := []struct {
		name    string
		method  string
		args    map[string]interface{}
		wantErr string
	}{
		{"缺 token", "jwt_decode", map[string]interface{}{}, "token"},
		{"段数不对", "jwt_decode", map[string]interface{}{"token": "a.b"}, "三段"},
		{"验签缺密钥", "jwt_verify", map[string]interface{}{"token": "a.b.c"}, "密钥"},
		{"验签缺 token", "jwt_verify", map[string]interface{}{"key": "k"}, "token"},
		{"签发缺 claims", "jwt_sign", map[string]interface{}{"alg": "HS256", "key": "k"}, "claims"},
		{"签发未知算法", "jwt_sign", map[string]interface{}{"claims": map[string]interface{}{}, "alg": "XX999", "key": "k"}, "alg"},
		{"签发缺 key", "jwt_sign", map[string]interface{}{"claims": map[string]interface{}{}, "alg": "HS256"}, "key"},
		{"ttl 非正", "jwt_sign", map[string]interface{}{"claims": map[string]interface{}{}, "alg": "HS256", "key": "k", "ttl": float64(0)}, "ttl"},
		{"algs 类型错", "jwt_verify", map[string]interface{}{"token": "a.b.c", "key": "k", "algs": "HS256"}, "数组"},
	}
	for _, c := range cases {
		if _, err := pureCall(t, "crypto", c.method, c.args); err == nil {
			t.Fatalf("%s：应报错（fail-closed），实际通过", c.name)
		}
	}
}

// TestPureJWTVerifyRejectsCrossFamilyAlgs：显式 algs 只能收窄、不能跨族。
func TestPureJWTVerifyRejectsCrossFamilyAlgs(t *testing.T) {
	tok, err := pureCall(t, "crypto", "jwt_sign", map[string]interface{}{
		"claims": map[string]interface{}{"sub": "alice"},
		"alg":    "HS256",
		"key":    "topsecret",
	})
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	if _, err := pureCall(t, "crypto", "jwt_verify", map[string]interface{}{
		"token": tok.(string), "key": "topsecret", "algs": []interface{}{"RS256"},
	}); err == nil {
		t.Fatalf("共享密钥却要求 RS256 必须报错（algs 与密钥类型无交集）")
	}
	// 收窄是允许的：指定 HS256 时，HS256 令牌仍应通过
	if _, err := pureCall(t, "crypto", "jwt_verify", map[string]interface{}{
		"token": tok.(string), "key": "topsecret", "algs": []interface{}{"HS256", "HS512"},
	}); err != nil {
		t.Fatalf("algs 收窄到同族应通过: %v", err)
	}
}

// ---- decode 与 verify 的分工 ----

// TestPureJWTDecodeDoesNotVerify 锁住语义：decode **不验签**（改了签名照样解得出），
// 所以它只能用来看内容，判断真伪必须走 verify。
func TestPureJWTDecodeDoesNotVerify(t *testing.T) {
	tok, err := pureCall(t, "crypto", "jwt_sign", map[string]interface{}{
		"claims": map[string]interface{}{"sub": "alice"},
		"alg":    "HS256",
		"key":    "topsecret",
	})
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	parts := strings.Split(tok.(string), ".")
	b, _ := base64.RawURLEncoding.DecodeString(parts[2])
	b[0] ^= 0x01
	parts[2] = base64.RawURLEncoding.EncodeToString(b)
	tampered := strings.Join(parts, ".")

	out, err := pureCall(t, "crypto", "jwt_decode", map[string]interface{}{"token": tampered})
	if err != nil {
		t.Fatalf("jwt_decode 不验签，篡改后仍应解析成功: %v", err)
	}
	m := out.(map[string]interface{})
	claims := m["claims"].(map[string]interface{})
	if claims["sub"] != "alice" {
		t.Fatalf("decode 应返回 claims，实际 %v", claims)
	}
	if _, err := pureCall(t, "crypto", "jwt_verify", map[string]interface{}{
		"token": tampered, "key": "topsecret",
	}); err == nil {
		t.Fatalf("同一令牌 verify 必须失败（decode 不验签，别拿它判真伪）")
	}
}

// ---- TTL ----

func TestPureJWTSignTTLWritesExp(t *testing.T) {
	tok, err := pureCall(t, "crypto", "jwt_sign", map[string]interface{}{
		"claims": map[string]interface{}{"sub": "alice"},
		"alg":    "HS256",
		"key":    "topsecret",
		"ttl":    float64(3600),
	})
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	out, err := pureCall(t, "crypto", "jwt_decode", map[string]interface{}{"token": tok.(string)})
	if err != nil {
		t.Fatalf("decode 失败: %v", err)
	}
	claims := out.(map[string]interface{})["claims"].(map[string]interface{})
	exp, ok := claims["exp"].(float64)
	if !ok {
		t.Fatalf("ttl 应写入 exp，实际 claims=%v", claims)
	}
	if exp <= float64(time.Now().Unix()) {
		t.Fatalf("exp 应在未来，实际 %v", exp)
	}
}

// ---- 密钥编码 ----

func TestPureJWTSignVerifyHexKeyEncoding(t *testing.T) {
	// 共享密钥以 hex 传入（16 字节）
	tok, err := pureCall(t, "crypto", "jwt_sign", map[string]interface{}{
		"claims":       map[string]interface{}{"sub": "alice"},
		"alg":          "HS256",
		"key":          "00112233445566778899aabbccddeeff",
		"key_encoding": "hex",
	})
	if err != nil {
		t.Fatalf("hex 密钥签发失败: %v", err)
	}
	if _, err := pureCall(t, "crypto", "jwt_verify", map[string]interface{}{
		"token": tok.(string), "key": "00112233445566778899aabbccddeeff", "key_encoding": "hex",
	}); err != nil {
		t.Fatalf("hex 密钥验签应通过: %v", err)
	}
}
