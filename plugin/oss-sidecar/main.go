package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"gear/internal/sidecar/coord"
)

const (
	defaultMaxBytes     = 32 << 20
	defaultExpires      = 900
	maxExpires          = 604800 // 7 天：S3 预签名 expires 的上限
	defaultLimit        = 1000
	maxLimit            = 1000
	defaultContentType  = "application/octet-stream"
	maxKeyLen           = 1024
	maxResponseReadSize = 1 << 20 // 读响应体的上限：错误体只是个 XML，限住防被恶意响应撑爆

	// upstreamTimeout 是侧车打真 S3 的超时。
	//
	// 真正的天花板不在这儿：内核调侧车用的是 LocalHTTPTransport，其 http.Client 硬超时 10s
	// （internal/engine/extensions.go:212），内核不等人。所以**大对象一律走 presign 直传**，
	// 别指望 put 能扛住大文件 —— presign 存在的理由就是这个。
	upstreamTimeout = 30 * time.Second
)

// 错误码（错误响应的 `error` 字段形如 "<code>: <人话>"）。
// 带稳定 ASCII 标记是因为调用方（Lua / Flow）要靠它分支，而中文文案随时会改。
const (
	errBadRequest    = "bad_request"
	errMissingKey    = "missing_key"
	errBadKey        = "bad_key"
	errMissingBucket = "missing_bucket"
	errMissingEndpt  = "missing_endpoint"
	errMissingRegion = "missing_region"
	errMissingCred   = "missing_credential"
	errBadEndpoint   = "bad_endpoint"
	errTooLarge      = "payload_too_large"
	errNotFound      = "not_found"
	errAccessDenied  = "access_denied"
	errUpstream      = "upstream_failed"
)

// nowFn 是唯一的时间入口，测试里替换成固定时间后才能对签名做断言。
var nowFn = time.Now

// codedError 带稳定机器码的错误（见上面错误码注释）。
type codedError struct{ code, msg string }

func (e *codedError) Error() string { return e.code + ": " + e.msg }

func fail(code, format string, a ...interface{}) error {
	return &codedError{code, fmt.Sprintf(format, a...)}
}

// ---- 配置解析 ----

type ossConfig struct {
	scheme    string // http / https
	base      string // scheme://host，不含路径
	host      string // 待签的 Host 头（virtual-host 时是 bucket.<endpoint>）
	keyPrefix string // path-style 时是 "/"+bucket，否则为空
	creds     credentials
	region    string
	bucket    string
	maxBytes  int64
	client    *http.Client
}

// objectPath 返回该 key 的**已编码**规范路径（签名与拼 URL 都用这一份，不许两处各算一次）。
func (c ossConfig) objectPath(key string) string {
	p := "/" + key
	if c.keyPrefix != "" {
		p = c.keyPrefix + "/" + key
	}
	return canonicalURI(p)
}

func parseConfig(args map[string]interface{}) (ossConfig, error) {
	var c ossConfig

	endpoint := strArg(args, "endpoint")
	if endpoint == "" {
		return c, fail(errMissingEndpt, "endpoint 必填（如 s3.us-east-1.amazonaws.com / oss-cn-hangzhou.aliyuncs.com / 127.0.0.1:9000）")
	}
	scheme, host := "https", endpoint
	if i := strings.Index(endpoint, "://"); i >= 0 {
		scheme, host = endpoint[:i], endpoint[i+3:]
	}
	host = strings.TrimRight(host, "/")
	// endpoint 只认 host[:port]：路径那一段是 key 的地盘，混进来会让「拼 URL」和「签名」各说一套。
	if host == "" || strings.Contains(host, "/") {
		return c, fail(errBadEndpoint, "endpoint 只能是 host[:port] 或 scheme://host[:port]，不含路径：%q", endpoint)
	}
	if scheme != "http" && scheme != "https" {
		return c, fail(errBadEndpoint, "endpoint 的 scheme 只能是 http 或 https：%q", scheme)
	}

	c.region = strArg(args, "region")
	if c.region == "" {
		return c, fail(errMissingRegion, "region 必填（AWS/MinIO 通常 us-east-1；阿里云 oss-cn-hangzhou；腾讯云 ap-guangzhou）—— 它是签名作用域的一部分，猜不出来")
	}
	c.bucket = strArg(args, "bucket")
	if c.bucket == "" {
		return c, fail(errMissingBucket, "bucket 必填")
	}

	c.creds = credentials{
		AccessKeyID:     strArg(args, "access_key_id"),
		SecretAccessKey: strArg(args, "access_key_secret"),
		SessionToken:    strArg(args, "session_token"),
	}
	if !c.creds.complete() {
		return c, fail(errMissingCred, "access_key_id 与 access_key_secret 都要给（走 ${secrets.*} 由内核展开注入；侧车不读环境变量、也不存密钥）")
	}

	// path_style：不给就自动判 —— endpoint 是 IP 时用 virtual-host 会解析不到
	// （MinIO / 自建 Ceph 的默认形态就是 IP:port），这类一律走 path-style。
	pathStyle, ok := boolArg(args, "path_style")
	if !ok {
		pathStyle = hostIsIP(host)
	}
	if pathStyle {
		c.keyPrefix = "/" + c.bucket
		c.host = host
	} else {
		c.host = c.bucket + "." + host
	}
	c.scheme = scheme
	c.base = scheme + "://" + host

	if n, ok := intArg(args, "max_bytes"); ok && n > 0 {
		c.maxBytes = n
	} else {
		c.maxBytes = defaultMaxBytes
	}
	c.client = &http.Client{Timeout: upstreamTimeout}
	return c, nil
}

func hostIsIP(host string) bool {
	h := host
	if hh, _, err := net.SplitHostPort(host); err == nil {
		h = hh
	}
	return net.ParseIP(h) != nil
}

// normalizeKey 校验对象 key。
//
// 刻意**不走** internal/sidecar/outpath：那是本地文件系统的囚禁（要查符号链接、要判 `..` 落到哪），
// 而远端对象存储的 key 只是一个字符串，没有符号链接语义 —— 这里只拒绝会造成歧义的那几种形状。
func normalizeKey(key string) (string, error) {
	if key == "" {
		return "", fail(errMissingKey, "key 必填")
	}
	if len(key) > maxKeyLen {
		return "", fail(errBadKey, "key 太长（>%d 字节）：%d", maxKeyLen, len(key))
	}
	if strings.HasPrefix(key, "/") {
		return "", fail(errBadKey, "key 不能以 / 开头：%q", key)
	}
	if strings.ContainsRune(key, '\\') {
		return "", fail(errBadKey, "key 不能含反斜杠：%q", key)
	}
	for _, seg := range strings.Split(key, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", fail(errBadKey, "key 含空段或 . / ..：%q", key)
		}
	}
	return key, nil
}

// normalizePrefix 校验 list 的 prefix。
//
// 刻意**不复用** normalizeKey：prefix 不是 key，"报表/" 这样的结尾斜杠是**正常写法**
// （意思是「这个目录下」），而 key 里的空段是歧义源必须拒。所以这里只挡会造成路径穿越的
// `..`、前导 `/` 与反斜杠，其余交出去。
func normalizePrefix(p string) (string, error) {
	if p == "" {
		return "", nil
	}
	if len(p) > maxKeyLen {
		return "", fail(errBadKey, "prefix 太长（>%d 字节）：%d", maxKeyLen, len(p))
	}
	if strings.HasPrefix(p, "/") {
		return "", fail(errBadKey, "prefix 不能以 / 开头：%q", p)
	}
	if strings.ContainsRune(p, '\\') {
		return "", fail(errBadKey, "prefix 不能含反斜杠：%q", p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return "", fail(errBadKey, "prefix 不能含 ..：%q", p)
		}
	}
	return p, nil
}

// ---- 请求 ----

// call 打一次签名请求。2xx 返回响应体与响应头；非 2xx 一律转成带稳定码的错误。
func (c ossConfig) call(method, key string, query [][2]string, body []byte, extra map[string]string) ([]byte, http.Header, error) {
	path := c.objectPath(key)
	u := c.base + path
	if len(query) > 0 {
		u += "?" + rawQuery(query)
	}
	req, err := http.NewRequest(method, u, bytes.NewReader(body))
	if err != nil {
		return nil, nil, fail(errBadRequest, "构造请求失败: %v", err)
	}
	for k, v := range signHeaders(c.creds, nowFn(), c.region, method, c.host, path, query, extra, payloadHash(body)) {
		req.Header.Set(k, v)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, nil, fail(errUpstream, "请求 %s 失败: %v", c.host, err)
	}
	defer func() { _ = resp.Body.Close() }()
	// 非 2xx 时错误码在 XML body 里，必须读；但限住长度，别被恶意响应撑爆。
	data, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseReadSize))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, resp.Header, upstreamErr(resp.StatusCode, data)
	}
	return data, resp.Header, nil
}

func rawQuery(pairs [][2]string) string {
	parts := make([]string, 0, len(pairs))
	for _, p := range pairs {
		parts = append(parts, uriEncode(p[0], false)+"="+uriEncode(p[1], false))
	}
	return strings.Join(parts, "&")
}

// s3Error 是 S3 的 XML 错误体（阿里云/腾讯云/MinIO 也同形状）。
type s3Error struct {
	XMLName xml.Name `xml:"Error"`
	Code    string   `xml:"Code"`
	Message string   `xml:"Message"`
}

func upstreamErr(status int, body []byte) error {
	code, msg := "", ""
	var e s3Error
	if err := xml.Unmarshal(body, &e); err == nil && e.Code != "" {
		code, msg = e.Code, e.Message
	}
	detail := strings.TrimSpace(code)
	if detail == "" {
		detail = strings.TrimSpace(string(body))
	}
	if detail == "" {
		detail = http.StatusText(status)
	}
	if msg != "" {
		detail += " (" + msg + ")"
	}
	switch {
	case status == http.StatusNotFound, code == "NoSuchKey", code == "NoSuchBucket":
		return fail(errNotFound, "HTTP %d %s", status, detail)
	case status == http.StatusForbidden, status == http.StatusUnauthorized,
		code == "AccessDenied", code == "InvalidAccessKeyId", code == "SignatureDoesNotMatch":
		return fail(errAccessDenied, "HTTP %d %s —— 凭据不对、没权限，或 region/endpoint 配错", status, detail)
	default:
		return fail(errUpstream, "HTTP %d %s", status, detail)
	}
}

// ---- handlers ----

type service struct{}

func (s *service) Handlers() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"/put":     s.handlePut,
		"/get":     s.handleGet,
		"/stat":    s.handleStat,
		"/list":    s.handleList,
		"/delete":  s.handleDelete,
		"/presign": s.handlePresign,
		"/caps":    s.handleCaps,
	}
}

func (s *service) handleCaps(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]interface{}{"result": map[string]interface{}{
		"methods": []string{"put", "get", "stat", "list", "delete", "presign"},
	}})
}

func (s *service) handlePut(w http.ResponseWriter, r *http.Request) {
	args, cfg, key, ok := begin(w, r, true)
	if !ok {
		return
	}
	b64 := strArg(args, "data_base64")
	if b64 == "" {
		writeCodedErr(w, fail(errBadRequest, "data_base64 必填（对象内容的 base64）"))
		return
	}
	// 先按 base64 长度估解码后大小，超限立刻拒：
	// 先解进内存再判，等于允许一次请求把内存打满（这是本方法唯一能被打爆的地方）。
	if est := int64(len(b64)) * 3 / 4; est > cfg.maxBytes {
		writeCodedErr(w, fail(errTooLarge, "对象约 %d 字节，超过上限 %d 字节（可调 max_bytes；再大请走 presign 直传）", est, cfg.maxBytes))
		return
	}
	data, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		writeCodedErr(w, fail(errBadRequest, "data_base64 不是合法 base64: %v", err))
		return
	}
	if int64(len(data)) > cfg.maxBytes {
		writeCodedErr(w, fail(errTooLarge, "对象 %d 字节，超过上限 %d 字节", len(data), cfg.maxBytes))
		return
	}
	ct := strArg(args, "content_type")
	if ct == "" {
		ct = defaultContentType
	}
	_, hdr, err := cfg.call("PUT", key, nil, data, map[string]string{"content-type": ct})
	if err != nil {
		writeCodedErr(w, err)
		return
	}
	writeJSON(w, map[string]interface{}{"result": map[string]interface{}{
		"key": key, "bucket": cfg.bucket, "size": len(data), "etag": hdr.Get("ETag"),
	}})
}

func (s *service) handleGet(w http.ResponseWriter, r *http.Request) {
	_, cfg, key, ok := begin(w, r, true)
	if !ok {
		return
	}
	body, hdr, err := cfg.call("GET", key, nil, nil, nil)
	if err != nil {
		writeCodedErr(w, err)
		return
	}
	size := int64(len(body))
	if size > cfg.maxBytes {
		writeCodedErr(w, fail(errTooLarge, "对象 %d 字节，超过上限 %d 字节（大对象请用 presign 让客户端直连下载）", size, cfg.maxBytes))
		return
	}
	ct := hdr.Get("Content-Type")
	if ct == "" {
		ct = defaultContentType
	}
	writeJSON(w, map[string]interface{}{"result": map[string]interface{}{
		"key": key, "bucket": cfg.bucket, "size": size,
		"content_type": ct, "mod_time": modTime(hdr.Get("Last-Modified")),
		"etag":        hdr.Get("ETag"),
		"data_base64": base64.StdEncoding.EncodeToString(body),
	}})
}

func (s *service) handleStat(w http.ResponseWriter, r *http.Request) {
	_, cfg, key, ok := begin(w, r, true)
	if !ok {
		return
	}
	_, hdr, err := cfg.call("HEAD", key, nil, nil, nil)
	if err != nil {
		writeCodedErr(w, err)
		return
	}
	ct := hdr.Get("Content-Type")
	if ct == "" {
		ct = defaultContentType
	}
	writeJSON(w, map[string]interface{}{"result": map[string]interface{}{
		"key": key, "bucket": cfg.bucket, "size": headerInt(hdr, "Content-Length"),
		"content_type": ct, "mod_time": modTime(hdr.Get("Last-Modified")), "etag": hdr.Get("ETag"),
	}})
}

func (s *service) handleDelete(w http.ResponseWriter, r *http.Request) {
	_, cfg, key, ok := begin(w, r, true)
	if !ok {
		return
	}
	if _, _, err := cfg.call("DELETE", key, nil, nil, nil); err != nil {
		writeCodedErr(w, err)
		return
	}
	writeJSON(w, map[string]interface{}{"result": map[string]interface{}{
		"key": key, "bucket": cfg.bucket, "deleted": true,
	}})
}

// listBucketResult 是 ListObjectsV2 的响应体（只取用得上的字段）。
type listBucketResult struct {
	XMLName               xml.Name `xml:"ListBucketResult"`
	IsTruncated           bool     `xml:"IsTruncated"`
	NextContinuationToken string   `xml:"NextContinuationToken"`
	Contents              []struct {
		Key          string `xml:"Key"`
		LastModified string `xml:"LastModified"`
		ETag         string `xml:"ETag"`
		Size         int64  `xml:"Size"`
	} `xml:"Contents"`
}

func (s *service) handleList(w http.ResponseWriter, r *http.Request) {
	args, cfg, _, ok := begin(w, r, false)
	if !ok {
		return
	}
	limit := defaultLimit
	if n, ok := intArg(args, "limit"); ok && n > 0 {
		limit = int(n)
		if limit > maxLimit {
			limit = maxLimit
		}
	}
	query := [][2]string{{"list-type", "2"}, {"max-keys", strconv.Itoa(limit)}}
	if p := strArg(args, "prefix"); p != "" {
		if _, err := normalizePrefix(p); err != nil {
			writeCodedErr(w, err)
			return
		}
		query = append(query, [2]string{"prefix", p})
	}
	body, _, err := cfg.call("GET", "", query, nil, nil)
	if err != nil {
		writeCodedErr(w, err)
		return
	}
	var out listBucketResult
	if err := xml.Unmarshal(body, &out); err != nil {
		writeCodedErr(w, fail(errUpstream, "解析 list 响应失败: %v", err))
		return
	}
	objs := make([]map[string]interface{}, 0, len(out.Contents))
	for _, c := range out.Contents {
		objs = append(objs, map[string]interface{}{
			"key": c.Key, "size": c.Size, "etag": c.ETag, "mod_time": modTime(c.LastModified),
		})
	}
	writeJSON(w, map[string]interface{}{"result": map[string]interface{}{
		"bucket": cfg.bucket, "objects": objs, "count": len(objs), "truncated": out.IsTruncated,
	}})
}

func (s *service) handlePresign(w http.ResponseWriter, r *http.Request) {
	args, cfg, key, ok := begin(w, r, true)
	if !ok {
		return
	}
	expires := defaultExpires
	if n, ok := intArg(args, "expires"); ok && n > 0 {
		expires = int(n)
		if expires > maxExpires {
			expires = maxExpires
		}
	}
	method := strings.ToUpper(strArg(args, "method"))
	if method == "" {
		method = "GET"
	}
	if method != "GET" && method != "PUT" {
		writeCodedErr(w, fail(errBadRequest, "method 只能是 GET（下载）或 PUT（直传），收到 %q", method))
		return
	}
	url := presignURL(cfg.creds, nowFn(), cfg.region, method, cfg.scheme, cfg.host, cfg.objectPath(key), expires)
	writeJSON(w, map[string]interface{}{"result": map[string]interface{}{
		"key": key, "bucket": cfg.bucket, "url": url, "expires": expires, "method": method,
	}})
}

// ---- 通用辅助 ----

// begin 做每个方法都要做的三件事：解 args → 解析配置 → 校验 key。
// needKey 为 false 时（list）跳过 key 校验。
func begin(w http.ResponseWriter, r *http.Request, needKey bool) (map[string]interface{}, ossConfig, string, bool) {
	args, err := decodeArgs(r)
	if err != nil {
		writeErr(w, errBadRequest, "请求体不是合法 JSON: %v", err)
		return nil, ossConfig{}, "", false
	}
	cfg, err := parseConfig(args)
	if err != nil {
		writeCodedErr(w, err)
		return nil, ossConfig{}, "", false
	}
	key := ""
	if needKey {
		if key, err = normalizeKey(strArg(args, "key")); err != nil {
			writeCodedErr(w, err)
			return nil, ossConfig{}, "", false
		}
	}
	return args, cfg, key, true
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

func writeErr(w http.ResponseWriter, code, format string, a ...interface{}) {
	writeJSON(w, map[string]interface{}{"error": code + ": " + fmt.Sprintf(format, a...)})
}

func writeCodedErr(w http.ResponseWriter, err error) {
	if ce, ok := err.(*codedError); ok {
		writeJSON(w, map[string]interface{}{"error": ce.Error()})
		return
	}
	writeErr(w, errBadRequest, "%v", err)
}

// strArg 取字符串参数并 Trim（" " 这种填了等于没填）；类型不对也算没传。
func strArg(m map[string]interface{}, k string) string {
	if v, ok := m[k].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

func intArg(m map[string]interface{}, k string) (int64, bool) {
	switch v := m[k].(type) {
	case float64:
		return int64(v), true
	case string:
		if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
			return n, true
		}
	}
	return 0, false
}

func boolArg(m map[string]interface{}, k string) (bool, bool) {
	switch v := m[k].(type) {
	case bool:
		return v, true
	case string:
		if b, err := strconv.ParseBool(strings.TrimSpace(v)); err == nil {
			return b, true
		}
	case float64:
		return v != 0, true
	}
	return false, false
}

func headerInt(h http.Header, k string) int64 {
	if n, err := strconv.ParseInt(strings.TrimSpace(h.Get(k)), 10, 64); err == nil {
		return n
	}
	return 0
}

// modTime 把 S3 的时间串归一成 RFC3339；解析不了就原样返回（宁可给原文也不给假时间）。
func modTime(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	for _, layout := range []string{time.RFC1123, time.RFC1123Z, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC().Format(time.RFC3339)
		}
	}
	return s
}

func main() {
	addr := os.Getenv("COORD_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8102"
	}
	svc := &service{}
	c := coord.New(coord.Options{
		CheckInterval: 5 * time.Second,
		TTL:           15 * time.Second,
		OnEmpty: func() {
			log.Printf("占用列表为空，oss-sidecar 自动退出")
			os.Exit(0)
		},
	})
	log.Printf("oss-sidecar 协调监听 %s", addr)
	if err := c.Serve(addr, svc.Handlers()); err != nil {
		if coord.IsAddrInUse(err) {
			log.Printf("端口 %s 已被占用，oss-sidecar 退让退出", addr)
			os.Exit(coord.ExitAddrInUse)
		}
		log.Fatalf("oss-sidecar 异常退出: %v", err)
	}
}
