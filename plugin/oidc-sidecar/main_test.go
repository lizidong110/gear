package main

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// 本文件用一个**真签 RS256** 的假 IdP 打侧车：钥匙是现造的 RSA 密钥对，id_token 是真签名，
// JWKS 按 IdP 该有的形状吐出来。这样「拿回来的令牌能不能被验签通过」才问得出口——
// 拿一坨 base64 拼假令牌去测，验签必然失败，测了等于没测。
//
// 假 IdP 是**内存版**（一个 http.RoundTripper，不监听端口）。这样单测既不开端口也不走协议栈：
// 端口在 CI 沙箱里常常没得占，而「用真 socket 才能证明的事」在 OIDC 这里其实只有
// 「HTTP 通不通」，那部分交给 plugin/verify.py 的真机端到端。

// ---- 假 IdP ----

// 以下是「故障注入」开关，零值即「正常服务」。
type fakeIDPState struct {
	docIssuer          string // 非空 ⇒ discovery 文档里 issuer 写成这个值（造 issuer 混淆）
	tokenEndpoint      string // 非空 ⇒ 覆盖 token_endpoint（造 host 不一致）
	docError           string // 非空 ⇒ discovery 返回 **200 + 这个 error 载荷**（造「状态码看不出错」）
	dropTokenEndpoint  bool   // ⇒ discovery 文档里不写 token_endpoint
	dropRequiredFields bool   // ⇒ discovery 文档里不写 authorization_endpoint / jwks_uri
	rejectCode         string // 收到的 code 等于它 ⇒ 令牌端点 400
	requireVerifier    string // 非空 ⇒ code_verifier 必须等于它，否则 400
	grantWithoutAT     bool   // ⇒ 返回 200 但没 access_token
	badDiscoveryJSON   bool   // ⇒ discovery 返回一段非 JSON 文本
	badTokenJSON       bool   // ⇒ 令牌端点返回一段非 JSON 文本
	jwksBody           string // 非空 ⇒ 原样返回这段文本（用于造坏载荷）
	jwksStatus         int    // 非 0 ⇒ /jwks 用这个状态码回
	jwksDelay          time.Duration
	jwksHits           int
	lastForm           url.Values
}

// errTransport 是个「出网就断」的替身：用于证明「端点不可达」这条分支走的是
// 结构化错误而不是 panic。
type errTransport struct{ err error }

func (e errTransport) RoundTrip(*http.Request) (*http.Response, error) { return nil, e.err }

type fakeIDPServer struct {
	mu   sync.Mutex
	key  *rsa.PrivateKey
	pub  *rsa.PublicKey
	st   fakeIDPState
	kid  string
	keys []interface{} // 非空 ⇒ 覆盖 /jwks 的返回（转钥匙场景）
}

func newFakeIDP(t *testing.T) *fakeIDPServer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("造测试 RSA 密钥失败: %v", err)
	}
	return &fakeIDPServer{key: key, pub: &key.PublicKey, kid: "key-1"}
}

// transport 返回可直接塞进 http.Client 的 RoundTripper。
func (f *fakeIDPServer) transport() http.RoundTripper { return f }

// RoundTrip 在内存里把侧车当客户端来服务。
//
// 它不校验 host：所有请求都落到同一个假 IdP 上。 issuer 混淆那几条用例靠**内容开关**
// （docIssuer / tokenEndpoint）制造差异，而不是靠两个不同域名——那条路径本来也该由内容决定。
func (f *fakeIDPServer) RoundTrip(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	st := f.st
	keys := f.keys
	f.mu.Unlock()

	p := req.URL.Path
	var status int = http.StatusOK
	var body []byte
	switch {
	case strings.HasSuffix(p, "/.well-known/openid-configuration"):
		if st.badDiscoveryJSON {
			body = []byte("<html>502 Bad Gateway</html>")
			break
		}
		if st.docError != "" {
			// OIDC 的错误载荷走文档里的 `error` 字段，HTTP 仍是 200。
			// 只看状态码的实现会在这里整条漏掉拒绝路径。
			body = mustJSON(map[string]interface{}{
				"error": st.docError, "error_description": "issuer 不提供该服务"})
			break
		}
		body = f.discoveryJSON(st)
	case p == "/jwks":
		// 延迟必须**尊重 req.Context()**：真正的 http.Transport 会在客户端超时后掉线，
		// 一个只顾睡到点、不看取消信号的假服务，会让人误以为「超时没生效」——
		// 那正是这个用例要证伪的错觉。
		if d := st.jwksDelay; d > 0 {
			select {
			case <-time.After(d):
			case <-req.Context().Done():
				return nil, req.Context().Err()
			}
		}
		if st.jwksStatus != 0 {
			status = st.jwksStatus
			body = []byte(`{"error":"nope"}`)
		} else if st.jwksBody != "" {
			body = []byte(st.jwksBody)
		} else {
			if len(keys) == 0 {
				keys = []interface{}{rsaJWK(f.pub, f.kid)}
			}
			body = mustJSON(map[string]interface{}{"keys": keys})
		}
		f.mu.Lock()
		f.st.jwksHits++
		f.mu.Unlock()
	case p == "/token":
		form, err := readForm(req)
		if err != nil {
			return nil, err
		}
		f.mu.Lock()
		f.st.lastForm = form
		f.mu.Unlock()
		if st.rejectCode != "" && form.Get("code") == st.rejectCode {
			status = http.StatusBadRequest
			body = mustJSON(map[string]interface{}{
				"error": "invalid_grant", "error_description": "授权码已使用或失效"})
		} else if st.requireVerifier != "" && form.Get("code_verifier") != st.requireVerifier {
			status = http.StatusBadRequest
			body = mustJSON(map[string]interface{}{
				"error": "invalid_grant", "error_description": "code_verifier 不匹配（PKCE 校验失败）"})
		} else if st.badTokenJSON {
			body = []byte("<html>this is not json</html>")
		} else if st.grantWithoutAT {
			body = mustJSON(map[string]interface{}{"token_type": "Bearer"})
		} else {
			claims := map[string]interface{}{
				"iss": "http://idp.test",
				"sub": "alice",
				"aud": form.Get("client_id"),
				"exp": time.Now().Add(time.Hour).Unix(),
				"iat": time.Now().Unix(),
			}
			body = mustJSON(map[string]interface{}{
				"access_token": "AT-" + form.Get("code"),
				"id_token":     signRS256(f.key, map[string]interface{}{"alg": "RS256", "typ": "JWT", "kid": f.kid}, claims),
				"token_type":   "Bearer",
				"expires_in":   3600,
				"scope":        "openid profile",
			})
		}
	default:
		status = http.StatusNotFound
		body = []byte("{}")
	}
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(body)),
		Request:    req,
	}, nil
}

// issuer 是「当前这个假 IdP 自己」的 issuer 标识（用于要求 discovery 的 issuer 与之相同）。
var testIssuer = "http://idp.test"

func (f *fakeIDPServer) discoveryJSON(st fakeIDPState) []byte {
	te := testIssuer + "/token"
	if st.tokenEndpoint != "" {
		te = st.tokenEndpoint
	}
	iss := testIssuer
	if st.docIssuer != "" {
		iss = st.docIssuer
	}
	doc := map[string]interface{}{
		"issuer":                 iss,
		"authorization_endpoint": testIssuer + "/authorize",
		"token_endpoint":         te,
		"jwks_uri":               testIssuer + "/jwks",
	}
	if st.dropTokenEndpoint {
		delete(doc, "token_endpoint")
	}
	if st.dropRequiredFields {
		delete(doc, "authorization_endpoint")
		delete(doc, "jwks_uri")
	}
	return mustJSON(doc)
}

func (f *fakeIDPServer) hits() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.st.jwksHits
}

func (f *fakeIDPServer) form() url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.st.lastForm
}

func readForm(req *http.Request) (url.Values, error) {
	if req.Body == nil {
		return url.Values{}, nil
	}
	b, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	return url.ParseQuery(string(b))
}

func rsaJWK(pub *rsa.PublicKey, kid string) map[string]interface{} {
	return map[string]interface{}{
		"kty": "RSA",
		"kid": kid,
		"use": "sig",
		"n":   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}
}

// signRS256 造一个真签名的 RS256 JWT（header.payload.signature）。
func signRS256(key *rsa.PrivateKey, header, claims map[string]interface{}) string {
	b64 := func(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
	h, _ := json.Marshal(header)
	p, _ := json.Marshal(claims)
	input := b64(h) + "." + b64(p)
	sum := sha256.Sum256([]byte(input))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		panic(err) // 测试夹具里失败即 ProgrammingError，不需要优雅降级
	}
	return input + "." + b64(sig)
}

func mustJSON(v interface{}) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// ---- 调用侧车的助手 ----

// newTestService 造一个「出口全指向假 IdP」的侧车（含同一 client 的 JWKS 缓存）。
func newTestService(idp *fakeIDPServer) *service {
	s := newService()
	s.cli.Transport = idp.transport()
	s.cache = newJWKSCache(s.cli) // 与 cli 共用 client，超时口径不变
	return s
}

// call 直接打 handler（不走网络），返回解析后的响应体。
func call(t *testing.T, svc *service, method string, args map[string]interface{}) map[string]interface{} {
	t.Helper()
	h := svc.Handlers()[method]
	if h == nil {
		t.Fatalf("侧车没有方法 %s", method)
	}
	body, _ := json.Marshal(map[string]interface{}{"args": args})
	r := httptest.NewRequest(http.MethodPost, method, strings.NewReader(string(body)))
	w := httptest.NewRecorder()
	h(w, r)
	var out map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("%s 的响应不是合法 JSON: %v / %s", method, err, w.Body.String())
	}
	return out
}

func result(t *testing.T, out map[string]interface{}) map[string]interface{} {
	t.Helper()
	r, ok := out["result"].(map[string]interface{})
	if !ok {
		t.Fatalf("响应没有 result 字段: %v", out)
	}
	return r
}

func strField(m map[string]interface{}, k string) string {
	v, _ := m[k].(string)
	return v
}

func errText(out map[string]interface{}) string {
	v, _ := out["error"].(string)
	return v
}

// ---- 测试 ----

func TestOIDCDiscoveryReturnsMetadata(t *testing.T) {
	idp := newFakeIDP(t)
	out := call(t, newTestService(idp), "/discovery", map[string]interface{}{"issuer": testIssuer})
	r := result(t, out)
	// 契约只有这四个键：多给一个就是悄悄扩了对外接口。
	for _, k := range []string{"issuer", "authorization_endpoint", "token_endpoint", "jwks_uri"} {
		if _, ok := r[k]; !ok {
			t.Fatalf("discovery 缺少 %s: %v", k, r)
		}
	}
	if got := strField(r, "token_endpoint"); got != testIssuer+"/token" {
		t.Fatalf("token_endpoint = %q", got)
	}
	if len(r) != 4 {
		t.Fatalf("discovery 返回了多余的键: %v", r)
	}
	// 尾部斜杠不该改变 issuer 的识别
	out2 := call(t, newTestService(idp), "/discovery", map[string]interface{}{"issuer": testIssuer + "/"})
	if strField(result(t, out2), "issuer") != testIssuer {
		t.Fatalf("issuer 应抹掉尾部斜杠: %v", out2)
	}
}

func TestOIDCIssuerMismatchIsRejected(t *testing.T) {
	cases := []struct {
		name          string
		mutate        func(*fakeIDPState)
		wantInError   string
		wantErrCodeOK bool
	}{
		{
			// 文档里 issuer 被换成另一个 IdP：拿 A 的授权码到 B 去换，经典的 issuer 混淆。
			"文档里的 issuer 与入参不一致",
			func(s *fakeIDPState) { s.docIssuer = "https://evil.example" },
			"issuer 与入参不一致", true,
		},
		{
			// host 不一致：即使 issuer 字段没动，把令牌端点换到别的主机，
			// 那台机器就能收到 client_secret 与授权码。
			"token_endpoint 换了主机",
			func(s *fakeIDPState) { s.tokenEndpoint = "https://evil.example/token" },
			"host 与 issuer 不一致", true,
		},
		{
			"token_endpoint 不是绝对 URL",
			func(s *fakeIDPState) { s.tokenEndpoint = "/token" },
			"token_endpoint", false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			idp := newFakeIDP(t)
			c.mutate(&idp.st)
			out := call(t, newTestService(idp), "/discovery", map[string]interface{}{"issuer": testIssuer})
			if _, ok := out["result"]; ok {
				t.Fatalf("不该返回元数据: %v", out)
			}
			if !strings.Contains(errText(out), c.wantInError) {
				t.Fatalf("错误文案应含 %q，实际 %q", c.wantInError, errText(out))
			}
			if c.wantErrCodeOK && !strings.Contains(errText(out), errIssuerMismatch) {
				t.Fatalf("错误码应是 %s，实际 %q", errIssuerMismatch, errText(out))
			}
		})
	}
}

func TestOIDCDiscoveryFailClosed(t *testing.T) {
	t.Run("文档里带 error 但 HTTP 200（状态码看不出的错误）", func(t *testing.T) {
		idp := newFakeIDP(t)
		idp.st.docError = "invalid_request"
		out := call(t, newTestService(idp), "/discovery", map[string]interface{}{"issuer": testIssuer})
		if _, ok := out["result"]; ok {
			t.Fatalf("不该返回元数据: %v", out)
		}
		// 错误码与上游原文两样都要有：前者给调用方做分支，后者给排查的人看
		if !strings.Contains(errText(out), errDiscoveryFail) ||
			!strings.Contains(errText(out), "invalid_request") {
			t.Fatalf("应报 discovery 失败并带上上游的 error: %q", errText(out))
		}
	})
	t.Run("缺 token_endpoint", func(t *testing.T) {
		idp := newFakeIDP(t)
		idp.st.dropTokenEndpoint = true
		out := call(t, newTestService(idp), "/discovery", map[string]interface{}{"issuer": testIssuer})
		if _, ok := out["result"]; ok {
			t.Fatalf("不该返回元数据: %v", out)
		}
		if !strings.Contains(errText(out), "缺少 token_endpoint") {
			t.Fatalf("应点名缺哪个字段: %q", errText(out))
		}
	})
	t.Run("缺 authorization_endpoint / jwks_uri 也拒", func(t *testing.T) {
		idp := newFakeIDP(t)
		idp.st.dropRequiredFields = true
		out := call(t, newTestService(idp), "/discovery", map[string]interface{}{"issuer": testIssuer})
		if _, ok := out["result"]; ok {
			t.Fatalf("不该返回元数据: %v", out)
		}
		if !strings.Contains(errText(out), "authorization_endpoint") {
			t.Fatalf("应点名缺 authorization_endpoint: %q", errText(out))
		}
	})
	t.Run("issuer 不是绝对 URL", func(t *testing.T) {
		out := call(t, newService(), "/discovery", map[string]interface{}{"issuer": "idp.example"})
		if !strings.Contains(errText(out), errBadIssuerURL) {
			t.Fatalf("应报 URL 非法: %q", errText(out))
		}
	})
	t.Run("issuer 缺失即拒（不猜 IdP 是谁）", func(t *testing.T) {
		out := call(t, newService(), "/discovery", map[string]interface{}{})
		if !strings.Contains(errText(out), errMissingIssuer) {
			t.Fatalf("应报缺 issuer: %q", errText(out))
		}
	})
}

func TestOIDCCodeExchangeHappyPath(t *testing.T) {
	idp := newFakeIDP(t)
	out := call(t, newTestService(idp), "/code_exchange", map[string]interface{}{
		"issuer": testIssuer, "code": "the-code", "client_id": "my-app",
		"client_secret": "s3cret", "redirect_uri": "http://localhost:9999/cb",
	})
	r := result(t, out)
	if got := strField(r, "access_token"); got != "AT-the-code" {
		t.Fatalf("access_token = %q", got)
	}
	if got := strField(r, "token_type"); got != "Bearer" {
		t.Fatalf("token_type = %q（不该替 IdP 填默认值）", got)
	}
	if _, ok := r["expires_in"].(float64); !ok {
		t.Fatalf("expires_in 缺失: %v", r)
	}
	if got := strField(r, "id_token"); got == "" {
		t.Fatalf("id_token 应原样转发: %v", r)
	}
	// 表单必须是 OIDC 要求的那几个键
	f := idp.form()
	if f.Get("grant_type") != "authorization_code" || f.Get("code") != "the-code" ||
		f.Get("redirect_uri") != "http://localhost:9999/cb" || f.Get("client_id") != "my-app" ||
		f.Get("client_secret") != "s3cret" {
		t.Fatalf("令牌端点收到的表单不对: %v", f)
	}
	// 没给 code_verifier 就**不带**这个键：凭空编一个等于把 PKCE 变成摆设
	if f.Get("code_verifier") != "" {
		t.Fatalf("没给 code_verifier 却带了: %v", f)
	}
}

func TestOIDCPKCE(t *testing.T) {
	t.Run("给了就透传", func(t *testing.T) {
		idp := newFakeIDP(t)
		out := call(t, newTestService(idp), "/code_exchange", map[string]interface{}{
			"issuer": testIssuer, "code": "c1", "client_id": "app",
			"redirect_uri": "http://x/cb", "code_verifier": "verifier-abc",
		})
		result(t, out)
		if got := idp.form().Get("code_verifier"); got != "verifier-abc" {
			t.Fatalf("code_verifier = %q", got)
		}
		// 没给 client_secret 就不带这个键（公开客户端靠 PKCE 自证）
		if idp.form().Get("client_secret") != "" {
			t.Fatalf("不该带 client_secret: %v", idp.form())
		}
	})
	t.Run("授权服务器拒收 PKCE → 结构化错误、不吐令牌", func(t *testing.T) {
		idp := newFakeIDP(t)
		idp.st.requireVerifier = "the-right-one"
		out := call(t, newTestService(idp), "/code_exchange", map[string]interface{}{
			"issuer": testIssuer, "code": "c1", "client_id": "app",
			"redirect_uri": "http://x/cb", "code_verifier": "wrong-verifier",
		})
		if _, ok := out["result"]; ok {
			t.Fatalf("不该换到令牌: %v", out)
		}
		err := errText(out)
		if !strings.Contains(err, errUpstreamFail) {
			t.Fatalf("错误码应是 %s，实际 %q", errUpstreamFail, err)
		}
		if !strings.Contains(err, "code_verifier 不匹配") {
			t.Fatalf("应把上游的 error_description 带出来: %q", err)
		}
		// 最关键的一条：错误体里绝不能出现令牌。这里把整份响应序列化成字符串看，
		// 只要哪天有人图省事写 `{"error": body}`（body 里含上游令牌），这条就会红。
		raw, _ := json.Marshal(out)
		if strings.Contains(string(raw), "access_token") || strings.Contains(string(raw), "id_token") {
			t.Fatalf("错误响应里泄漏了令牌字段: %s", raw)
		}
	})
}

func TestOIDCCodeExchangeMissingParams(t *testing.T) {
	idp := newFakeIDP(t)
	for _, c := range []struct{ key, want string }{
		{"code", "code 必填"},
		{"client_id", "client_id 必填"},
		{"redirect_uri", "redirect_uri 必填"},
	} {
		args := map[string]interface{}{"issuer": testIssuer, "client_id": "app",
			"code": "c", "redirect_uri": "http://x/cb"}
		delete(args, c.key)
		out := call(t, newTestService(idp), "/code_exchange", args)
		if !strings.Contains(errText(out), c.want) {
			t.Fatalf("缺 %s 应报 %q，实际 %q", c.key, c.want, errText(out))
		}
	}
}

func TestOIDCUpstreamWithoutAccessToken(t *testing.T) {
	idp := newFakeIDP(t)
	idp.st.grantWithoutAT = true
	out := call(t, newTestService(idp), "/code_exchange", map[string]interface{}{
		"issuer": testIssuer, "code": "c1", "client_id": "app", "redirect_uri": "http://x/cb",
	})
	if !strings.Contains(errText(out), errNoToken) {
		t.Fatalf("2xx 但没有 access_token 应单列一条错: %q", errText(out))
	}
}

func TestOIDCIssuerMismatchBlocksExchange(t *testing.T) {
	// 换令牌这一步同样走 discovery，所以 issuer 混淆在这一步也必须被挡住——
	// 「发现时挡住、换令牌时放行」是最常见也最致命的一种漏法。
	idp := newFakeIDP(t)
	idp.st.tokenEndpoint = "https://evil.example/token"
	out := call(t, newTestService(idp), "/code_exchange", map[string]interface{}{
		"issuer": testIssuer, "code": "c1", "client_id": "app", "redirect_uri": "http://x/cb",
	})
	if _, ok := out["result"]; ok {
		t.Fatalf("不该换到令牌: %v", out)
	}
	if !strings.Contains(errText(out), "host 与 issuer 不一致") {
		t.Fatalf("应报 issuer 混淆: %q", errText(out))
	}
}

func TestOIDCJWKSCacheServesFromMemory(t *testing.T) {
	idp := newFakeIDP(t)
	svc := newTestService(idp)
	args := map[string]interface{}{"issuer": testIssuer}

	first := result(t, call(t, svc, "/jwks", args))
	keys, _ := first["keys"].([]interface{})
	if len(keys) != 1 {
		t.Fatalf("第一次应拿到 1 把钥匙: %v", first)
	}
	after1 := idp.hits()
	if after1 != 1 {
		t.Fatalf("第一次调用后 jwks 被拉了 %d 次（应为 1）", after1)
	}
	// 关键断言：第二次**一次 HTTP 都不发**。「长驻缓存」不是文档里的一句话，
	// 是这个计数。
	time.Sleep(5 * time.Millisecond)
	second := result(t, call(t, svc, "/jwks", args))
	if idp.hits() != after1 {
		t.Fatalf("缓存命中竟然又发了 HTTP（%d → %d）", after1, idp.hits())
	}
	if fmt.Sprint(second["keys"]) != fmt.Sprint(keys) {
		t.Fatalf("两次返回的键集不一致")
	}
	if stale, _ := second["stale"].(bool); stale {
		t.Fatalf("命中缓存不该是 stale")
	}
}

func TestOIDCUnknownKidForcesRefresh(t *testing.T) {
	idp := newFakeIDP(t)
	// 第一次只给一把不带 key-2 的钥匙
	idp.keys = []interface{}{rsaJWK(idp.pub, "key-1")}
	svc := newTestService(idp)

	if _, _, err := svc.cache.getForKid(testIssuer, "key-2"); err == nil {
		t.Fatalf("第一次不该找到 kid=key-2")
	}
	// IdP 转钥匙了：现在有两把
	idp.keys = []interface{}{rsaJWK(idp.pub, "key-1"), rsaJWK(idp.pub, "key-2")}
	hitsBefore := idp.hits()
	keys, stale, err := svc.cache.getForKid(testIssuer, "key-2")
	if err != nil {
		t.Fatalf("未知 kid 应触发自动刷新而不是报错: %v", err)
	}
	if stale {
		t.Fatalf("刷新成功不该是 stale")
	}
	if idp.hits() <= hitsBefore {
		t.Fatalf("未知 kid 应真的重拉一次 JWKS（%d → %d）", hitsBefore, idp.hits())
	}
	if !kidIn(keys, "key-2") {
		t.Fatalf("刷新后仍没有 kid=key-2: %v", keys)
	}
}

func TestOIDCStaleOnRefreshFailure(t *testing.T) {
	idp := newFakeIDP(t)
	svc := newTestService(idp)

	if _, _, err := svc.cache.get(testIssuer); err != nil {
		t.Fatalf("预热失败: %v", err)
	}
	// 之后 IdP 的 JWKS 端点挂了，而令牌里带着一个当前钥匙集里没有的 kid
	idp.st.jwksStatus = http.StatusInternalServerError
	idp.kid = "rotated-key"
	keys, stale, err := svc.cache.getForKid(testIssuer, "rotated-key")
	if err != nil {
		t.Fatalf("刷新失败应 fail-soft 退回旧缓存，而不是报错: %v", err)
	}
	if !stale {
		t.Fatalf("退回旧缓存必须带 stale=true（不然就是静默）")
	}
	if len(keys) == 0 {
		t.Fatalf("退回的旧缓存不该是空的")
	}
}

func TestOIDCRefreshFailureWithoutCacheIsFatal(t *testing.T) {
	idp := newFakeIDP(t)
	idp.st.jwksStatus = http.StatusBadGateway
	_, _, err := newTestService(idp).cache.get(testIssuer)
	if err == nil {
		t.Fatalf("没有缓存且拉不到 ⇒ 必须报错（返回空集会被当成「IdP 没钥匙」）")
	}
}

func TestOIDCAmbiguousKidRejected(t *testing.T) {
	idp := newFakeIDP(t)
	idp.kid = ""
	idp.keys = []interface{}{rsaJWK(idp.pub, "key-1"), rsaJWK(idp.pub, "key-2")}
	// 空缓存 + 多把钥匙 + 无 kid 也必须拒（这一支原先只在「缓存命中」时才拦，
	// 于是「第一次调用放行、第二次报错」这种随调用次数变化的行为出现了）
	if _, _, err := newTestService(idp).cache.getForKid(testIssuer, ""); err == nil {
		t.Fatalf("多把钥匙 + 无 kid 应报错（不猜用哪把）")
	}
	// 反之：单把钥匙 + 无 kid 是明确的，应当放行（口径与 crypto.jwt_verify 一致）
	idp.keys = []interface{}{rsaJWK(idp.pub, "key-1")}
	svc := newTestService(idp)
	if _, _, err := svc.cache.getForKid(testIssuer, ""); err != nil {
		t.Fatalf("只有一把钥匙时不该报错: %v", err)
	}
	// 再调一次走「缓存命中」那条路：两条路都必须得出同一个结论，
	// 否则「同一个令牌验两次，一次过一次不过」就出现了。
	if _, _, err := svc.cache.getForKid(testIssuer, ""); err != nil {
		t.Fatalf("缓存命中那条路结论不一致: %v", err)
	}
	// 缓存命中但钥匙集没变时，也不该每次都去刷新
	// 同一枚 IdP 换成两把钥匙：首次拉取要歧义，缓存起来之后**也不能**悄悄刷新再多刷一次
	idp.keys = []interface{}{rsaJWK(idp.pub, "key-1"), rsaJWK(idp.pub, "key-9")}
	svc2 := newTestService(idp)
	if _, _, err := svc2.cache.getForKid(testIssuer, ""); err == nil {
		t.Fatalf("两把钥匙 + 无 kid 应报歧义")
	}
	hits := idp.hits()
	if _, _, err := svc2.cache.getForKid(testIssuer, ""); err == nil {
		t.Fatalf("缓存命中那条路也得报歧义")
	}
	if idp.hits() != hits {
		t.Fatalf("「已报错」不该触发 JWKS 刷新（%d → %d）", hits, idp.hits())
	}
}

// TestOIDCTimeoutIsStrictTenSeconds 花 11s 买「超时真的是 10s」这件事本身。
//
// 若把超时写成一个大得多的常量，所有超时类断言都会安静失效，而超时失效在 OIDC 握手里
// 的表现是「应用线程被拖死」——没人会去看日志。
func TestOIDCTimeoutIsStrictTenSeconds(t *testing.T) {
	idp := newFakeIDP(t)
	idp.st.jwksDelay = 11 * time.Second
	svc := newTestService(idp)

	start := time.Now()
	out := call(t, svc, "/jwks", map[string]interface{}{"issuer": testIssuer})
	elapsed := time.Since(start)
	if _, ok := out["result"]; ok {
		t.Fatalf("挂住的 JWKS 端点不该返回内容: %v", out)
	}
	if !strings.Contains(errText(out), errNoJWKS) {
		t.Fatalf("超时应是结构化错误: %q", errText(out))
	}
	if elapsed < 9*time.Second {
		t.Fatalf("%v 就返回了，10s 超时没生效？", elapsed)
	}
	if elapsed > 15*time.Second {
		t.Fatalf("耗时 %v，超时远大于 10s", elapsed)
	}
}

func TestOIDCBadRequestBodies(t *testing.T) {
	// 请求体不是合法 JSON：三个方法都得给结构化错误。
	// 这条看着琐碎，但它是「侧车不吞错」的底线——静默返回一个空 result 会让
	// 调用方以为「握手成功了，只是没拿到东西」。
	for _, m := range []string{"/discovery", "/code_exchange", "/jwks"} {
		h := newService().Handlers()[m]
		r := httptest.NewRequest(http.MethodPost, m, strings.NewReader("not-json-at-all"))
		w := httptest.NewRecorder()
		h(w, r)
		out := w.Body.String()
		if !strings.Contains(out, "error") || strings.Contains(out, "result") {
			t.Fatalf("%s 对畸形请求体的响应不对: %s", m, out)
		}
	}
}

func TestOIDCMalformedUpstreamPayloads(t *testing.T) {
	t.Run("discovery 返回的不是 JSON", func(t *testing.T) {
		idp := newFakeIDP(t)
		idp.st.badDiscoveryJSON = true
		out := call(t, newTestService(idp), "/discovery", map[string]interface{}{"issuer": testIssuer})
		if _, ok := out["result"]; ok {
			t.Fatalf("不该返回元数据: %v", out)
		}
		if !strings.Contains(errText(out), errDiscoveryFail) {
			t.Fatalf("应报 discovery 失败: %q", errText(out))
		}
	})
	t.Run("令牌端点返回的不是 JSON", func(t *testing.T) {
		idp := newFakeIDP(t)
		idp.st.badTokenJSON = true
		out := call(t, newTestService(idp), "/code_exchange", map[string]interface{}{
			"issuer": testIssuer, "code": "c1", "client_id": "app", "redirect_uri": "http://x/cb",
		})
		if !strings.Contains(errText(out), errBadUpstream) {
			t.Fatalf("应报上游响应不可解析: %q", errText(out))
		}
	})
	t.Run("JWKS 里没有 keys", func(t *testing.T) {
		idp := newFakeIDP(t)
		idp.st.jwksBody = `{"nothing":true}`
		out := call(t, newTestService(idp), "/jwks", map[string]interface{}{"issuer": testIssuer})
		if !strings.Contains(errText(out), "缺少 keys") {
			t.Fatalf("应报 keys 缺失: %q", errText(out))
		}
	})
	t.Run("JWKS 的 keys 是空数组", func(t *testing.T) {
		idp := newFakeIDP(t)
		idp.st.jwksBody = `{"keys":[]}`
		out := call(t, newTestService(idp), "/jwks", map[string]interface{}{"issuer": testIssuer})
		if !strings.Contains(errText(out), errNoJWKS) {
			t.Fatalf("空钥匙集要报错，不能当成「IdP 没钥匙」返回: %q", errText(out))
		}
	})
}

func TestOIDCUnreachableEndpoints(t *testing.T) {
	// 端点不可达 ⇒ 结构化错误而不是 panic（AC-E1-4）
	bad := &http.Client{Timeout: httpTimeout}
	bad.Transport = errTransport{err: fmt.Errorf("connection refused")}
	svc := &service{cli: bad, cache: newJWKSCache(bad)}
	for _, c := range []struct {
		method string
		args   map[string]interface{}
	}{
		{"/discovery", map[string]interface{}{"issuer": testIssuer}},
		{"/jwks", map[string]interface{}{"issuer": testIssuer}},
		{"/code_exchange", map[string]interface{}{"issuer": testIssuer, "code": "c",
			"client_id": "app", "redirect_uri": "http://x/cb"}},
	} {
		out := call(t, svc, c.method, c.args)
		if _, ok := out["result"]; ok {
			t.Fatalf("%s 对不可达的端点不该返回内容: %v", c.method, out)
		}
		if errText(out) == "" {
			t.Fatalf("%s 对不可达的端点应给结构化错误", c.method)
		}
	}
}

func TestOIDCCapsRedLine(t *testing.T) {
	out := call(t, newService(), "/caps", map[string]interface{}{})
	r := result(t, out)
	got := fmt.Sprint(r["methods"])
	for _, want := range []string{"discovery", "code_exchange", "jwks"} {
		if !strings.Contains(got, want) {
			t.Fatalf("caps 应含 %s: %v", want, r)
		}
	}
}

func TestOIDCInboundEchoesCallback(t *testing.T) {
	svc := newService()
	// 直接打 handler，不启监听：入站端口被占属于部署问题，不归单测管。
	r := httptest.NewRequest(http.MethodGet, "/in/oidc/demo?code=THE_CODE&state=STATE&iss=http%3A%2F%2Fidp.test", nil)
	w := httptest.NewRecorder()
	svc.inboundMux().ServeHTTP(w, r)
	var out map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("入站响应不是 JSON: %v", err)
	}
	res := result(t, out) // 与其它方法一样包一层 result（这里的「结果」就是收到了什么）
	if strField(res, "code") != "THE_CODE" || strField(res, "state") != "STATE" {
		t.Fatalf("入站未原样带回 code/state: %v", res)
	}
	// 侧车不建会话（Q1），只回「收到了什么」
	for _, k := range []string{"session", "token", "access_token"} {
		if _, ok := res[k]; ok {
			t.Fatalf("入站不许产生 %s: %v", k, res)
		}
	}
}

func TestOIDCInboundErrorCallback(t *testing.T) {
	svc := newService()
	r := httptest.NewRequest(http.MethodGet, "/in/oidc/demo?error=access_denied&error_description=x", nil)
	w := httptest.NewRecorder()
	svc.inboundMux().ServeHTTP(w, r)
	var out map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if strField(result(t, out), "error") != "access_denied" {
		t.Fatalf("拒绝回调也要把 error 带回: %v", out)
	}
}

// TestOIDCSecretsNeverSurface 把一次完整握手的所有响应摊开看一遍：
// client_secret 一个字都不许出现（它只该待在 IdP 那头）。
func TestOIDCSecretsNeverSurface(t *testing.T) {
	idp := newFakeIDP(t)
	svc := newTestService(idp)
	args := map[string]interface{}{
		"issuer": testIssuer, "code": "c1", "client_id": "app",
		"client_secret": "SUPER_SECRET", "redirect_uri": "http://x/cb",
	}
	blob := ""
	for _, m := range []string{"/discovery", "/jwks", "/code_exchange"} {
		b, _ := json.Marshal(call(t, svc, m, args))
		blob += string(b) + "\n"
	}
	for _, s := range []string{"SUPER_SECRET", "client_secret", "grant_type"} {
		if strings.Contains(blob, s) {
			t.Fatalf("响应里泄漏了 %q:\n%s", s, blob)
		}
	}
}
