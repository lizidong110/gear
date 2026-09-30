// Command oidc-sidecar 是 ext.oidc 的常驻侧车（service ext），只做「拿令牌」的握手：
// discovery（取 issuer 元数据）/ code_exchange（授权码换令牌，含 PKCE）/ jwks（长驻缓存）。
//
// 三条职责边界（PRD §4.2，写死的，别改回去）：
//  1. 只做握手：发现元数据、把授权码换成令牌、把 JWKS 取回来并缓存。
//  2. **不验签**：验签是第 0 批 `crypto.jwt_verify` 的职责。本侧车吐出的 id_token 原样转发，
//     既不验也不改写——它要是替调用方验了，应用就会把「侧车验过」当成「我验过」。
//  3. **不做会话**（Q1 已拍板，授权是业务的事）：state / nonce 由**调用方**生成并自行校验，
//     本侧车只把 state 原样带回，不存任何登录态、不记会话、不换令牌。
//
// 两条安全口径：
//   - **issuer 混淆**：discovery 拿回来的元数据必须回到同一个 issuer——文档里的 issuer 与入参
//     一致，且 token_endpoint 的 host 与 issuer 同 host。否则一次 MITM 就能把
//     「client_secret + 授权码」引到攻击者自己的令牌端点去（这是 OIDC 里最经典的劫持面）。
//   - **fail-closed**：授权服务器拒收、元数据缺字段、JWKS 取不到、上游 JSON 坏，一律报错。
//     宁可让调用方「换不到令牌」，也不要「换到一个看不出是错的令牌」。
//
// HTTP 超时**统一 10s**（PRD §4.2 把仓库里散在 3s/5s/10s 三处的约定拉齐到一处）。
//
// 环境变量：COORD_ADDR（默认 127.0.0.1:8098）、INBOUND_ADDR（默认 127.0.0.1:8099，
// 入站回调端口）。两者都由引擎按 extensions.yaml 注入，此处默认值仅供不经引擎的手工单跑兜底。
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"gear/internal/sidecar/coord"
)

// httpTimeout 是**所有**出网请求的超时（PRD §4.2：拉齐到 10s）。
//
// 之所以是硬常量而不是可配参数：这块的值是「安全基线」不是「性能旋钮」，
// 让它可配等于给每个调用方一次调低它的机会。
const httpTimeout = 10 * time.Second

// 错误码（错误响应的 `error` 字段形如 "<code>: <人话>"）。
//
// 之所以带稳定的 ASCII 标记：调用方（Lua / Flow）要靠它分支，而中文文案随时会改；
// 只给中文等于把分支逻辑焊死在一句措辞上。
const (
	errMissingIssuer  = "missing_issuer"
	errBadIssuerURL   = "bad_issuer"
	errMissingParam   = "missing_param"
	errDiscoveryFail  = "discovery_failed"
	errIssuerMismatch = "issuer_mismatch"
	errUpstreamFail   = "upstream_failed"
	errBadUpstream    = "bad_upstream"
	errNoToken        = "no_token"
	errNoJWKS         = "no_jwks"
	errUnknownKid     = "unknown_kid"
	errAmbiguousKid   = "ambiguous_kid"
)

// service 是本侧车的状态。除了 JWKS 缓存之外没有任何「记忆」——
// 不存登录态、不存 code、不存会话（Q1 口径）。
type service struct {
	cli   *http.Client
	cache *jwksCache
}

func newService() *service {
	return &service{
		cli:   &http.Client{Timeout: httpTimeout},
		cache: newJWKSCache(&http.Client{Timeout: httpTimeout}),
	}
}

// Handlers 是 RPC 方法表（键是方法名，与 extensions.yaml 的 methods 一一对应）。
func (s *service) Handlers() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"/discovery":     s.handleDiscovery,
		"/code_exchange": s.handleCodeExchange,
		"/jwks":          s.handleJWKS,
		"/caps":          s.handleCaps,
	}
}

func (s *service) handleCaps(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]interface{}{"result": map[string]interface{}{
		"methods": []string{"discovery", "code_exchange", "jwks"},
	}})
}

// ---- discovery ----

func (s *service) handleDiscovery(w http.ResponseWriter, r *http.Request) {
	args, err := decodeArgs(r)
	if err != nil {
		writeErr(w, errBadUpstream, "请求体不是合法 JSON: %v", err)
		return
	}
	issuer := strings.TrimSpace(strOr(args, "issuer", ""))
	if issuer == "" {
		writeErr(w, errMissingIssuer, "issuer 必填（侧车不会替你猜 IdP 是谁）")
		return
	}

	doc, err := discover(s.cli, issuer)
	if err != nil {
		writeErr(w, errCode(err), "%s", errMsg(err))
		return
	}
	writeJSON(w, map[string]interface{}{"result": map[string]interface{}{
		"issuer":                 strings.TrimRight(issuer, "/"),
		"authorization_endpoint": doc.authorizationEndpoint,
		"token_endpoint":         doc.tokenEndpoint,
		"jwks_uri":               doc.jwksURI,
	}})
}

// issuerDoc 是 discovery 文档里本侧车真正用到的四个字段。
//
// 只挑这四个而不是整份文档回传：PRD §4.2 的接口契约就是这四个键，多给一个就等于
// 悄悄扩展对外契约，而这些额外字段（response_modes_supported 之类）没有任何消费者。
type issuerDoc struct {
	authorizationEndpoint string
	tokenEndpoint         string
	jwksURI               string
}

// discover 抓 <issuer>/.well-known/openid-configuration 并做完 issuer 混淆检查。
func discover(cli *http.Client, issuer string) (issuerDoc, error) {
	u, err := url.Parse(issuer)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return issuerDoc{}, &oidcErr{code: errBadIssuerURL,
			msg: fmt.Sprintf("issuer 必须是带 scheme 的绝对 URL，实际 %q", issuer)}
	}
	docURL := *u
	docURL.Path = strings.TrimRight(u.Path, "/") + "/.well-known/openid-configuration"
	docURL.RawQuery = ""
	docURL.Fragment = ""

	body, err := get(cli, docURL.String())
	if err != nil {
		return issuerDoc{}, &oidcErr{code: errDiscoveryFail,
			msg: fmt.Sprintf("discovery 拉取失败（issuer=%s）: %v", issuer, err)}
	}

	var m map[string]interface{}
	if err := json.Unmarshal(body, &m); err != nil {
		return issuerDoc{}, &oidcErr{code: errDiscoveryFail,
			msg: fmt.Sprintf("discovery 返回的不是合法 JSON（issuer=%s）: %v", issuer, err)}
	}
	// OIDC 规定 discovery 出错时文档里会出现 `error`（而不是 HTTP 状态码承载）。
	// 只看状态码会漏掉它——很多 IdP 就是 200 + {"error":"..."}。
	if e, ok := m["error"].(string); ok {
		return issuerDoc{}, &oidcErr{code: errDiscoveryFail,
			msg: fmt.Sprintf("issuer=%s 的 discovery 返回 error=%q%s", issuer, e, descOf(m["error_description"]))}
	}

	// ① issuer 必须与入参一致（防「把 A 的授权码送到 B」）。
	// 有些部署不返回 issuer（RFC 8414 是要求返回的），此时不强制——但见 ② 的 host 检查，
	// 那一支独立就能挡住换端点的攻击。
	if got, ok := m["issuer"].(string); ok {
		if normOrigin(got) != normOrigin(issuer) {
			// 拿规范化后的值比，否则 "https://id/" 与 "https://id" 会被误判成不一致
			return issuerDoc{}, &oidcErr{code: errIssuerMismatch,
				msg: fmt.Sprintf("discovery 返回的 issuer 与入参不一致（文档 %q != 入参 %q）",
					got, issuer)}
		}
	}
	// ② token_endpoint 必须与 issuer 同 host。
	// 这条挡的是「元数据被换掉」：只要 host 不是 issuer 自己的，那台机器能收到
	// client_secret 与授权码，就等于能把它们全记下来。issuer 字段本身可以被攻击者
	// 从上游文档里改掉，但 host 这一条没法绕过——除非攻击者已经能改 DNS，那时候
	// 改不改元数据就没区别了。
	te, _ := m["token_endpoint"].(string)
	if te == "" {
		return issuerDoc{}, &oidcErr{code: errDiscoveryFail,
			msg: fmt.Sprintf("discovery 文档缺少 token_endpoint（issuer=%s）", issuer)}
	}
	tu, err := url.Parse(te)
	if err != nil || normOrigin(te) != normOrigin(issuer) {
		return issuerDoc{}, &oidcErr{code: errIssuerMismatch,
			msg: fmt.Sprintf("token_endpoint 的 host 与 issuer 不一致（%q vs %q）："+
				"把凭据送到别的主机是 issuer 混淆攻击，拒绝", te, issuer)}
	}
	if tu.Scheme == "" || tu.Host == "" {
		return issuerDoc{}, &oidcErr{code: errDiscoveryFail,
			msg: fmt.Sprintf("token_endpoint 不是绝对 URL（%q）", te)}
	}

	ae, _ := m["authorization_endpoint"].(string)
	ju, _ := m["jwks_uri"].(string)
	if ae == "" || ju == "" {
		return issuerDoc{}, &oidcErr{code: errDiscoveryFail,
			msg: fmt.Sprintf("discovery 文档缺少 authorization_endpoint 或 jwks_uri（issuer=%s）", issuer)}
	}
	return issuerDoc{authorizationEndpoint: ae, tokenEndpoint: te, jwksURI: ju}, nil
}

// ---- code_exchange ----

func (s *service) handleCodeExchange(w http.ResponseWriter, r *http.Request) {
	args, err := decodeArgs(r)
	if err != nil {
		writeErr(w, errBadUpstream, "请求体不是合法 JSON: %v", err)
		return
	}
	issuer := strings.TrimSpace(strOr(args, "issuer", ""))
	code := strings.TrimSpace(strOr(args, "code", ""))
	clientID := strings.TrimSpace(strOr(args, "client_id", ""))
	redirectURI := strings.TrimSpace(strOr(args, "redirect_uri", ""))
	// client_secret 与 code_verifier 都**可选**：client_secret 要给公开客户端（PKCE）留活路，
	// code_verifier 缺省就是「不带」——侧车不替调用方编一个（编了等于把 PKCE 变成摆设）。
	clientSecret := strOr(args, "client_secret", "")
	codeVerifier := strOr(args, "code_verifier", "")

	switch {
	case issuer == "":
		writeErr(w, errMissingIssuer, "issuer 必填")
		return
	case code == "":
		writeErr(w, errMissingParam, "code 必填")
		return
	case clientID == "":
		writeErr(w, errMissingParam, "client_id 必填")
		return
	case redirectURI == "":
		// redirect_uri 就算 IdP 允许省略也**要求显式给**：省略了，换到的令牌可能是拿
		// 另一个回调地址换来的，这种「悄悄 succeed」的歧义比少一个参数危险得多。
		writeErr(w, errMissingParam, "redirect_uri 必填（侧车不猜你把回调放在哪）")
		return
	}

	// 走一遍发现，issuer 混淆检查对换令牌这一步同样生效
	meta, err := discover(s.cli, issuer)
	if err != nil {
		writeErr(w, errCode(err), "%s", errMsg(err))
		return
	}

	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)
	form.Set("client_id", clientID)
	if clientSecret != "" {
		form.Set("client_secret", clientSecret)
	}
	if codeVerifier != "" {
		form.Set("code_verifier", codeVerifier) // PKCE：有就透传，没有就不带
	}
	req, err := http.NewRequest(http.MethodPost, meta.tokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		writeErr(w, errUpstreamFail, "构造令牌请求失败: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// client_secret 走请求体而不是 Basic 头：绝大多数 IdP 两者都收，走体更简单也更好查；
	// 换成 Basic 反而要在「没有 secret 时」编一个空密码头，属于自找麻烦。

	resp, err := s.cli.Do(req)
	if err != nil {
		writeErr(w, errUpstreamFail, "令牌端点不可达（%s）: %v", meta.tokenEndpoint, err)
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// 授权服务器拒收 → **结构化错误、绝不回吐任何令牌字段**。
		// 这里刻意不解析上游 body 里的 access_token（若被夹带），只回上游的 error 标记。
		writeErr(w, errUpstreamFail, "授权服务器拒收该授权码（HTTP %d%s）",
			resp.StatusCode, upstreamErrOf(body))
		return
	}
	var tok struct {
		AccessToken string `json:"access_token"`
		IDToken     string `json:"id_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int    `json:"expires_in"`
		Scope       string `json:"scope"`
		Error       string `json:"error"`
		ErrorDesc   string `json:"error_description"`
	}
	if err := json.Unmarshal(body, &tok); err != nil {
		writeErr(w, errBadUpstream, "令牌端点的响应不是合法 JSON: %v", err)
		return
	}
	if tok.AccessToken == "" {
		// 上游 2xx 但没有 access_token：这是「成功但没给东西」，比失败更容易被漏判，
		// 所以单列一条错——调用方会因此明确知道换令牌这一步什么都没拿到。
		writeErr(w, errNoToken, "令牌端点返回成功但没有 access_token%s", descOf(&tok.ErrorDesc))
		return
	}
	// 原样转发 IdP 给的东西：一个字段都不补、都不改。token_type 缺省也不自己填 "Bearer"
	// ——填了就是在替 IdP 说话，而本侧车没有替 IdP 说话的资格。
	out := map[string]interface{}{"access_token": tok.AccessToken}
	if tok.IDToken != "" {
		out["id_token"] = tok.IDToken
	}
	if tok.TokenType != "" {
		out["token_type"] = tok.TokenType
	}
	if tok.ExpiresIn > 0 {
		out["expires_in"] = tok.ExpiresIn
	}
	if tok.Scope != "" {
		out["scope"] = tok.Scope
	}
	writeJSON(w, map[string]interface{}{"result": out})
}

// ---- jwks ----

func (s *service) handleJWKS(w http.ResponseWriter, r *http.Request) {
	args, err := decodeArgs(r)
	if err != nil {
		writeErr(w, errBadUpstream, "请求体不是合法 JSON: %v", err)
		return
	}
	issuer := strings.TrimSpace(strOr(args, "issuer", ""))
	if issuer == "" {
		writeErr(w, errMissingIssuer, "issuer 必填")
		return
	}
	// kid 可选：带了就按「键集里**必须**有这把钥匙」来办（未知 ⇒ 强制刷新一次，见 AC-E1-3）；
	// 没带就是「把键集原样给我」，不猜、不挑（挑等于替 IdP 改了密钥集）。
	keys, stale, err := s.cache.getOrVerifyKid(issuer, strings.TrimSpace(strOr(args, "kid", "")))
	if err != nil {
		writeErr(w, errCode(err), "%s", errMsg(err))
		return
	}
	// stale 永远跟着 keys 一起出去：宁可让调用方看到「可能是旧的」，
	// 也不要让「缓存挂了」表现为「签名的钥匙不存在」。
	writeJSON(w, map[string]interface{}{"result": map[string]interface{}{
		"keys":  keys,
		"stale": stale,
	}})
}

// ---- 入站（OAuth 回调落地）----

// inboundMux 是入站回调端口的路由（/in/{path} 形态，同 ext.webhook）。
//
// 浏览器在 IdP 授权后会被弹到这里，带 ?code=&state=。本侧车**只回报**它收到了什么：
// 不换令牌、不建会话、不记状态（Q1：授权是业务的事）。真要接着换令牌，由应用/守卫 Hook
// 拿这个 code 自己去调 code_exchange。
func (s *service) inboundMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/in/oidc/", s.handleInbound)
	return mux
}

func (s *service) handleInbound(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query()
	code := q.Get("code")
	state := q.Get("state")
	if code == "" {
		// 只有错误回调（?error=access_denied）时 IdP 不会给 code。这种情况**也要**看
		// error 参数并原样带回，否则调用方拿到「没有 code」就以为流程干净结束了。
		writeJSON(w, map[string]interface{}{"result": map[string]interface{}{
			"path":   r.URL.Path,
			"error":  q.Get("error"),
			"reason": q.Get("error_description"),
			"state":  state,
		}})
		return
	}
	// state 原样带回，供调用方（业务侧）比对——侧车不记得它，所以没法替你比。
	writeJSON(w, map[string]interface{}{"result": map[string]interface{}{
		"path":  r.URL.Path,
		"code":  code,
		"state": state,
		"iss":   q.Get("iss"),
	}})
	// 日志里**不写 code**（它此刻还有效）：日志是长期留存的，令牌不是。
	log.Printf("oidc 入站回调 %s 收到一次授权（state 长度 %d）", r.URL.Path, len(state))
}

// ---- 工具 ----

// get 发一个 GET，返回响应体（上限 1MB）。
//
// 超时**只靠 client.Timeout**（10s）——不加第二个 ctx 超时：两个超时叠加时，
// 到底是哪个先到没人看得出来，排查时只会得到「大概 10 秒」这种没法断言的答案。
func get(cli *http.Client, u string) ([]byte, error) {
	resp, err := cli.Get(u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

// ---- 错误 ----

// oidcErr 是结构化错误：稳定的机器码 + 人话说明。
type oidcErr struct {
	code string
	msg  string
}

func (e *oidcErr) Error() string { return e.code + ": " + e.msg }

// errCode / errMsg 把 error 拆回去，供 handler 统一写响应（避免每个分支各写一遍格式）。
func errCode(err error) string {
	if e, ok := err.(*oidcErr); ok {
		return e.code
	}
	return "internal_error"
}

func errMsg(err error) string {
	if e, ok := err.(*oidcErr); ok {
		return e.msg
	}
	return err.Error()
}

func writeErr(w http.ResponseWriter, code, format string, a ...interface{}) {
	writeJSON(w, map[string]interface{}{"error": code + ": " + fmt.Sprintf(format, a...)})
}

// descOf 把上游可能给的 error_description 拼成人话的一部分（没有就返回空串）。
func descOf(v interface{}) string {
	s, _ := v.(string)
	if s == "" {
		return ""
	}
	return "（" + s + "）"
}

// upstreamErrOf 从上游拒绝响应体里挑 error / error_description。
//
// 只挑这两个键，**不解析别的**（尤其不碰 access_token）：响应体里出现 access_token
// 但 HTTP 是 4xx，那是上游的怪癖，不是我们的通行证。
func upstreamErrOf(body []byte) string {
	var m map[string]interface{}
	if err := json.Unmarshal(body, &m); err != nil {
		return ""
	}
	return descOf(m["error"]) + descOf(m["error_description"])
}

// normOrigin 取 URL 的「 scheme://host 」，并抹掉大小写差异——
// issuer 比对要忽略 host 的大小写（DNS 不区分大小写），但保留 path 与 query。
func normOrigin(u string) string {
	p, err := url.Parse(u)
	if err != nil {
		return strings.ToLower(strings.TrimRight(u, "/"))
	}
	return strings.ToLower(p.Scheme) + "://" + strings.ToLower(p.Host)
}

func decodeArgs(r *http.Request) (map[string]interface{}, error) {
	var req struct {
		Args map[string]interface{} `json:"args"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return nil, err
	}
	return req.Args, nil
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func strOr(m map[string]interface{}, k, def string) string {
	if v, ok := m[k].(string); ok && v != "" {
		return v
	}
	return def
}

func main() {
	addr := os.Getenv("COORD_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8098"
	}
	inboundAddr := os.Getenv("INBOUND_ADDR")
	if inboundAddr == "" {
		inboundAddr = "127.0.0.1:8099"
	}
	svc := newService()

	// 入站回调端口与 coord 端口正交：入站监听失败（端口被占）只记日志、不自杀——
	// coord 端口仍可用，ctx.ext.oidc.* 三个出站方法不受影响（自杀会拖垮整个侧车）。
	go func() {
		log.Printf("oidc 入站监听 %s（浏览器回调落在 /in/oidc/{label}）", inboundAddr)
		srv := &http.Server{Addr: inboundAddr, Handler: svc.inboundMux()}
		if err := srv.ListenAndServe(); err != nil {
			log.Printf("oidc 入站端口监听失败（回调不可用，coord 端口不受影响）: %v", err)
		}
	}()

	c := coord.New(coord.Options{
		CheckInterval: 5 * time.Second,
		TTL:           15 * time.Second,
		OnEmpty: func() {
			log.Printf("占用列表为空，oidc-sidecar 自动退出")
			os.Exit(0)
		},
	})

	log.Printf("oidc-sidecar 协调监听 %s", addr)
	if err := c.Serve(addr, svc.Handlers()); err != nil {
		if coord.IsAddrInUse(err) {
			log.Printf("端口 %s 已被占用，oidc-sidecar 退让退出", addr)
			os.Exit(coord.ExitAddrInUse)
		}
		log.Fatalf("oidc-sidecar 异常退出: %v", err)
	}
}
