package main

import (
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// 假 S3 端点
//
// 为什么要有它：沙箱里没有真对象存储（OSS / COS / MinIO 都连不上），而「请求发出去了」
// 跟「签名签对了」是两件事。这个假端点用**从收到的原始 HTTP 请求**反推出来的
// canonical request 重算签名再比对 —— 客户端签的东西和实际发上线的东西只要有一处
// 对不上（路径编码、query、被签的头、body 哈希），它就会 403。
//
// 它校验的是「线上一致性」；密码学本身的正确性由 sigv4_test.go 里 botocore / aws4
// 两个独立实现的黄金向量把关。两者不是一回事，别互相顶替。
// ─────────────────────────────────────────────────────────────────────────────

type storedObject struct {
	data        []byte
	contentType string
	modTime     time.Time
}

type recordedReq struct {
	method string
	path   string
	query  string
	host   string
	auth   string
	body   []byte
}

type fakeS3 struct {
	mu       sync.Mutex
	bucket   string
	ak, sk   string
	region   string
	objects  map[string]*storedObject
	requests []recordedReq
	// failWith 非空时，所有请求先按这个状态码失败（用来验上游错误映射）。
	failWith int
	failCode string
}

func newFakeS3(region string) *fakeS3 {
	return &fakeS3{
		bucket:  "mybucket",
		ak:      "AKIAIOSFODNN7EXAMPLE",
		sk:      "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY",
		region:  region,
		objects: map[string]*storedObject{},
	}
}

// verifySignature 从收到的请求重算签名，与客户端给的比对。
// 只采信客户端在 SignedHeaders 里声明的那些头 —— 这样「签了但没发」也能被抓出来。
func (f *fakeS3) verifySignature(r *http.Request, body []byte) error {
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return errors.New("缺少 Authorization 头")
	}
	cred := field(auth, "Credential=", ",")
	signed := field(auth, "SignedHeaders=", ",")
	sig := field(auth, "Signature=", "")
	if cred == "" || signed == "" || sig == "" {
		return fmt.Errorf("Authorization 头形状不对: %q", auth)
	}
	parts := strings.Split(cred, "/")
	if len(parts) != 5 {
		return fmt.Errorf("Credential 作用域形状不对: %q", cred)
	}
	if parts[0] != f.ak {
		return fmt.Errorf("AccessKeyId 不符: %q", parts[0])
	}
	region := parts[2]
	if region != f.region {
		return fmt.Errorf("region 不符: 签的是 %q，端点是 %q", region, f.region)
	}
	scopeDate := parts[1]
	amzDate := r.Header.Get("X-Amz-Date")
	if len(amzDate) < 8 || amzDate[:8] != scopeDate {
		return fmt.Errorf("x-amz-date %q 与作用域日期 %q 不一致", amzDate, scopeDate)
	}
	t, err := time.Parse(amzTimeFormat, amzDate)
	if err != nil {
		return fmt.Errorf("x-amz-date 解析不了: %v", err)
	}

	h := map[string]string{}
	for _, name := range strings.Split(signed, ";") {
		if name == "host" {
			h["host"] = r.Host
			continue
		}
		v := r.Header.Get(name)
		if v == "" {
			return fmt.Errorf("声明要签的头 %q 在请求里没有值", name)
		}
		h[name] = v
	}
	canon := canonicalRequest(r.Method, r.URL.EscapedPath(), canonicalQuery(queryPairs(r.URL.Query())), h, payloadHash(body))
	want := signature(f.sk, t, region, stringToSign(t, region, canon))
	if want != sig {
		return fmt.Errorf("签名不符\n  服务端按收到的请求重算: %s\n  客户端给的:        %s\n  canonical: %q", want, sig, canon)
	}
	return nil
}

// verifyPresign 验预签名 URL：签名在 query 里，且只签 host、payload 恒为 UNSIGNED-PAYLOAD。
// 同样是从收到的请求反推 canonical request 重算，跟客户端算的那一份对不上就拒。
func (f *fakeS3) verifyPresign(r *http.Request) error {
	q := r.URL.Query()
	if q.Get("X-Amz-Algorithm") != sigV4Algorithm {
		return fmt.Errorf("算法不对: %q", q.Get("X-Amz-Algorithm"))
	}
	cred := q.Get("X-Amz-Credential")
	parts := strings.Split(cred, "/")
	if len(parts) != 5 {
		return fmt.Errorf("Credential 作用域形状不对: %q", cred)
	}
	if parts[0] != f.ak {
		return fmt.Errorf("AccessKeyId 不符: %q", parts[0])
	}
	if parts[2] != f.region {
		return fmt.Errorf("region 不符: 签的是 %q，端点是 %q", parts[2], f.region)
	}
	amzDate := q.Get("X-Amz-Date")
	t, err := time.Parse(amzTimeFormat, amzDate)
	if err != nil {
		return fmt.Errorf("X-Amz-Date 解析不了: %v", err)
	}
	if expires := q.Get("X-Amz-Expires"); expires == "" || expires == "0" {
		return errors.New("缺 X-Amz-Expires")
	}

	pairs := [][2]string{}
	for k, vs := range q {
		if k == "X-Amz-Signature" {
			continue
		}
		for _, v := range vs {
			pairs = append(pairs, [2]string{k, v})
		}
	}
	h := map[string]string{}
	for _, n := range strings.Split(q.Get("X-Amz-SignedHeaders"), ";") {
		if n == "host" {
			h["host"] = r.Host
			continue
		}
		h[n] = r.Header.Get(n)
	}
	canon := canonicalRequest(r.Method, r.URL.EscapedPath(), canonicalQuery(pairs), h, unsignedPayload)
	want := signature(f.sk, t, parts[2], stringToSign(t, parts[2], canon))
	if want != q.Get("X-Amz-Signature") {
		return fmt.Errorf("预签名不符\n  服务端重算: %s\n  URL 里带的: %s\n  canonical: %q",
			want, q.Get("X-Amz-Signature"), canon)
	}
	return nil
}

func field(s, prefix, sep string) string {
	i := strings.Index(s, prefix)
	if i < 0 {
		return ""
	}
	rest := s[i+len(prefix):]
	if sep == "" {
		return strings.TrimSpace(rest)
	}
	if j := strings.Index(rest, sep); j >= 0 {
		return rest[:j]
	}
	return strings.TrimSpace(rest)
}

func queryPairs(v url.Values) [][2]string {
	keys := make([]string, 0, len(v))
	for k := range v {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := [][2]string{}
	for _, k := range keys {
		for _, val := range v[k] {
			out = append(out, [2]string{k, val})
		}
	}
	return out
}

func writeS3Err(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, xml.Header+fmt.Sprintf("<Error><Code>%s</Code><Message>%s</Message></Error>", code, msg))
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.requests = append(f.requests, recordedReq{
		method: r.Method, path: r.URL.EscapedPath(), query: r.URL.RawQuery,
		host: r.Host, auth: r.Header.Get("Authorization"), body: body,
	})
	forceFail, failCode := f.failWith, f.failCode
	f.mu.Unlock()

	if forceFail != 0 {
		writeS3Err(w, forceFail, failCode, "测试注入的失败")
		return
	}
	var verifyErr error
	if r.URL.Query().Get("X-Amz-Signature") != "" {
		verifyErr = f.verifyPresign(r)
	} else {
		verifyErr = f.verifySignature(r, body)
	}
	if verifyErr != nil {
		writeS3Err(w, http.StatusForbidden, "SignatureDoesNotMatch", verifyErr.Error())
		return
	}

	// path-style：/bucket/key（本测试的 endpoint 是 127.0.0.1:port，自动走 path-style）
	trimmed := strings.TrimPrefix(r.URL.EscapedPath(), "/")
	slash := strings.Index(trimmed, "/")
	bucket, key := trimmed, ""
	if slash >= 0 {
		bucket, key = trimmed[:slash], trimmed[slash+1:]
	}
	if bucket != f.bucket {
		writeS3Err(w, http.StatusNotFound, "NoSuchBucket", "没有这个 bucket: "+bucket)
		return
	}
	key, _ = url.PathUnescape(key)

	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == "PUT":
		ct := r.Header.Get("Content-Type")
		if ct == "" {
			ct = defaultContentType
		}
		f.objects[key] = &storedObject{data: body, contentType: ct, modTime: time.Now().UTC()}
		w.Header().Set("ETag", `"fake-etag"`)
		w.WriteHeader(http.StatusOK)
	case r.Method == "GET" && r.URL.Query().Get("list-type") == "2":
		prefix := r.URL.Query().Get("prefix")
		type item struct {
			Key          string `xml:"Key"`
			LastModified string `xml:"LastModified"`
			ETag         string `xml:"ETag"`
			Size         int64  `xml:"Size"`
		}
		keys := make([]string, 0, len(f.objects))
		for k := range f.objects {
			if strings.HasPrefix(k, prefix) {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		items := make([]item, 0, len(keys))
		for _, k := range keys {
			o := f.objects[k]
			items = append(items, item{
				Key: k, LastModified: o.modTime.Format(time.RFC1123), ETag: `"e"`, Size: int64(len(o.data)),
			})
		}
		out := struct {
			XMLName     xml.Name `xml:"ListBucketResult"`
			IsTruncated bool     `xml:"IsTruncated"`
			Contents    []item   `xml:"Contents"`
		}{Contents: items}
		w.Header().Set("Content-Type", "application/xml")
		_ = xml.NewEncoder(w).Encode(out)
	case r.Method == "GET":
		o, ok := f.objects[key]
		if !ok {
			writeS3Err(w, http.StatusNotFound, "NoSuchKey", "没有这个 key: "+key)
			return
		}
		w.Header().Set("Content-Type", o.contentType)
		w.Header().Set("ETag", `"fake-etag"`)
		w.Header().Set("Last-Modified", o.modTime.Format(time.RFC1123))
		_, _ = w.Write(o.data)
	case r.Method == "HEAD":
		o, ok := f.objects[key]
		if !ok {
			writeS3Err(w, http.StatusNotFound, "NoSuchKey", "没有这个 key: "+key)
			return
		}
		w.Header().Set("Content-Type", o.contentType)
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(o.data)))
		w.Header().Set("ETag", `"fake-etag"`)
		w.Header().Set("Last-Modified", o.modTime.Format(time.RFC1123))
		w.WriteHeader(http.StatusOK)
	case r.Method == "DELETE":
		if _, ok := f.objects[key]; !ok {
			writeS3Err(w, http.StatusNotFound, "NoSuchKey", "没有这个 key: "+key)
			return
		}
		delete(f.objects, key)
		w.WriteHeader(http.StatusNoContent)
	default:
		writeS3Err(w, http.StatusBadRequest, "MethodNotAllowed", r.Method)
	}
}

// ---- 测试辅助 ----

func startFakeS3(t *testing.T, f *fakeS3) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return srv
}

// endpointOf 把 http://127.0.0.1:port 变成侧车要的 endpoint（只留 host:port）。
func endpointOf(srv *httptest.Server) string {
	return strings.TrimPrefix(srv.URL, "http://")
}

func baseArgs(srv *httptest.Server, f *fakeS3) map[string]interface{} {
	return map[string]interface{}{
		"endpoint":          "http://" + endpointOf(srv),
		"region":            f.region,
		"bucket":            f.bucket,
		"access_key_id":     f.ak,
		"access_key_secret": f.sk,
	}
}

func call(t *testing.T, path string, args map[string]interface{}) map[string]interface{} {
	t.Helper()
	body, err := json.Marshal(map[string]interface{}{"args": args})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest("POST", path, strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	h, ok := (&service{}).Handlers()[path]
	if !ok {
		t.Fatalf("没有这个端点: %s", path)
	}
	h(rec, req)
	var out map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是 JSON: %v / %s", err, rec.Body.String())
	}
	return out
}

func result(t *testing.T, out map[string]interface{}) map[string]interface{} {
	t.Helper()
	if e, ok := out["error"].(string); ok {
		t.Fatalf("期望成功，却拿到错误: %s", e)
	}
	r, ok := out["result"].(map[string]interface{})
	if !ok {
		t.Fatalf("响应没有 result: %v", out)
	}
	return r
}

func errCode(out map[string]interface{}) string {
	e, _ := out["error"].(string)
	if i := strings.Index(e, ":"); i > 0 {
		return e[:i]
	}
	return e
}

// ---- 用例 ----

func TestRoundTripPutGetStatListDelete(t *testing.T) {
	f := newFakeS3("us-east-1")
	srv := startFakeS3(t, f)

	put := result(t, call(t, "/put", mergeArgs(baseArgs(srv, f), map[string]interface{}{
		"key": "报表/2026 09.txt", "data_base64": base64Of("你好 oss"), "content_type": "text/plain",
	})))
	if put["size"].(float64) != float64(len("你好 oss")) {
		t.Fatalf("put 返回的 size 不对: %v", put["size"])
	}

	got := result(t, call(t, "/get", mergeArgs(baseArgs(srv, f), map[string]interface{}{"key": "报表/2026 09.txt"})))
	if got["data_base64"].(string) != base64Of("你好 oss") {
		t.Fatalf("get 读回的内容不对: %v", got["data_base64"])
	}
	if got["content_type"].(string) != "text/plain" {
		t.Fatalf("content_type 没保住: %v", got["content_type"])
	}

	st := result(t, call(t, "/stat", mergeArgs(baseArgs(srv, f), map[string]interface{}{"key": "报表/2026 09.txt"})))
	if st["size"].(float64) != float64(len("你好 oss")) {
		t.Fatalf("stat 的 size 不对: %v", st["size"])
	}
	if !strings.Contains(st["mod_time"].(string), "T") {
		t.Fatalf("mod_time 没归一成 RFC3339: %v", st["mod_time"])
	}

	ls := result(t, call(t, "/list", baseArgs(srv, f)))
	if ls["count"].(float64) != 1 {
		t.Fatalf("list 数量不对: %v", ls)
	}
	objs := ls["objects"].([]interface{})
	if objs[0].(map[string]interface{})["key"].(string) != "报表/2026 09.txt" {
		t.Fatalf("list 的 key 不对: %v", objs[0])
	}

	del := result(t, call(t, "/delete", mergeArgs(baseArgs(srv, f), map[string]interface{}{"key": "报表/2026 09.txt"})))
	if del["deleted"].(bool) != true {
		t.Fatalf("delete 没返回 deleted: %v", del)
	}
	if out := call(t, "/get", mergeArgs(baseArgs(srv, f), map[string]interface{}{"key": "报表/2026 09.txt"})); errCode(out) != errNotFound {
		t.Fatalf("删完再 get 应报 not_found，实得: %v", out)
	}
}

// TestFakeServerRejectsTamperedSecret 是这条链路的反向验证：
// 密钥错一位，服务端必须拒 —— 说明服务端真在验签，而不是「有 Authorization 就放行」。
func TestFakeServerRejectsTamperedSecret(t *testing.T) {
	f := newFakeS3("us-east-1")
	srv := startFakeS3(t, f)
	args := mergeArgs(baseArgs(srv, f), map[string]interface{}{
		"key": "k.txt", "data_base64": base64Of("x"),
		"access_key_secret": f.sk + "WRONG",
	})
	out := call(t, "/put", args)
	if errCode(out) != errAccessDenied {
		t.Fatalf("密钥错应报 access_denied，实得: %v", out)
	}
}

func TestWrongRegionIsRejectedByServer(t *testing.T) {
	f := newFakeS3("us-east-1")
	srv := startFakeS3(t, f)
	args := mergeArgs(baseArgs(srv, f), map[string]interface{}{"key": "k.txt", "data_base64": base64Of("x")})
	args["region"] = "oss-cn-hangzhou" // 与端点声明的 region 不符
	out := call(t, "/put", args)
	if errCode(out) != errAccessDenied {
		t.Fatalf("region 不符应被服务端拒，实得: %v", out)
	}
}

func TestKeyValidationHappensBeforeAnyRequest(t *testing.T) {
	f := newFakeS3("us-east-1")
	srv := startFakeS3(t, f)
	cases := []struct{ name, key, want string }{
		{"空", "", errMissingKey},
		{"前导斜杠", "/a.txt", errBadKey},
		{"反斜杠", `a\b.txt`, errBadKey},
		{"父目录", "../secret.txt", errBadKey},
		{"当前目录", "a/./b.txt", errBadKey},
		{"空段", "a//b.txt", errBadKey},
		{"超长", strings.Repeat("a", maxKeyLen+1), errBadKey},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := call(t, "/get", mergeArgs(baseArgs(srv, f), map[string]interface{}{"key": c.key}))
			if errCode(out) != c.want {
				t.Fatalf("key=%q 期望 %s，实得 %v", c.key, c.want, out)
			}
		})
	}
	if n := len(f.requests); n != 0 {
		t.Fatalf("坏 key 不应发出任何请求，实发 %d 个", n)
	}
}

func TestMissingParametersAreRejected(t *testing.T) {
	f := newFakeS3("us-east-1")
	srv := startFakeS3(t, f)
	cases := []struct{ name, drop, want string }{
		{"缺 endpoint", "endpoint", errMissingEndpt},
		{"缺 region", "region", errMissingRegion},
		{"缺 bucket", "bucket", errMissingBucket},
		{"缺 ak", "access_key_id", errMissingCred},
		{"缺 sk", "access_key_secret", errMissingCred},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			args := mergeArgs(baseArgs(srv, f), map[string]interface{}{"key": "k.txt"})
			delete(args, c.drop)
			out := call(t, "/get", args)
			if errCode(out) != c.want {
				t.Fatalf("期望 %s，实得 %v", c.want, out)
			}
		})
	}
	if n := len(f.requests); n != 0 {
		t.Fatalf("缺参数不应发出请求，实发 %d 个", n)
	}
}

func TestBadEndpointShapes(t *testing.T) {
	f := newFakeS3("us-east-1")
	srv := startFakeS3(t, f)
	for _, ep := range []string{"http://127.0.0.1:9000/bucket", "", "ftp://oss.example.com", "http://"} {
		out := call(t, "/get", mergeArgs(baseArgs(srv, f), map[string]interface{}{"key": "k.txt", "endpoint": ep}))
		if got := errCode(out); got != errBadEndpoint && got != errMissingEndpt {
			t.Fatalf("endpoint=%q 期望端点类错误，实得 %v", ep, out)
		}
	}
}

// TestPayloadLimitIsCheckedBeforeDecoding 锁住「先按 base64 长度估算再拒」这条：
// 真解一个 100MB 的 base64 进来再判超限，等于允许一次请求把内存打满。
func TestPayloadLimitIsCheckedBeforeDecoding(t *testing.T) {
	f := newFakeS3("us-east-1")
	srv := startFakeS3(t, f)
	big := strings.Repeat("A", 4<<20) // 约 3MB 明文
	out := call(t, "/put", mergeArgs(baseArgs(srv, f), map[string]interface{}{
		"key": "big.bin", "data_base64": big, "max_bytes": 1024,
	}))
	if errCode(out) != errTooLarge {
		t.Fatalf("超限应报 payload_too_large，实得 %v", out)
	}
	if n := len(f.requests); n != 0 {
		t.Fatalf("超限不应发出请求，实发 %d 个", n)
	}
}

func TestUpstreamFailureMapping(t *testing.T) {
	f := newFakeS3("us-east-1")
	srv := startFakeS3(t, f)
	f.mu.Lock()
	f.failWith, f.failCode = http.StatusInternalServerError, "InternalError"
	f.mu.Unlock()
	out := call(t, "/get", mergeArgs(baseArgs(srv, f), map[string]interface{}{"key": "k.txt"}))
	if errCode(out) != errUpstream {
		t.Fatalf("5xx 应报 upstream_failed，实得 %v", out)
	}
	if !strings.Contains(out["error"].(string), "InternalError") {
		t.Fatalf("错误信息应带上 S3 的 Code: %v", out)
	}
}

func TestUpstreamUnreachable(t *testing.T) {
	f := newFakeS3("us-east-1")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	port := endpointOf(srv)
	srv.Close() // 故意关掉，制造连不上
	out := call(t, "/get", map[string]interface{}{
		"endpoint": "http://" + port, "region": f.region, "bucket": f.bucket,
		"access_key_id": f.ak, "access_key_secret": f.sk, "key": "k.txt",
	})
	if errCode(out) != errUpstream {
		t.Fatalf("连不上应报 upstream_failed，实得 %v", out)
	}
}

// TestPathStyleAutoAndExplicit 用预签名 URL 观察寻址形态：
// virtual-host 形态在 127.0.0.1 上根本连不通（没有 bucket.127.0.0.1 这个 DNS 名），
// 所以这里**不走网络**——预签名 URL 里带着 host 与路径，足以判定用哪种形态。
// 真跑通的往返（默认 path-style）由其它用例覆盖。
func TestPathStyleAutoAndExplicit(t *testing.T) {
	f := newFakeS3("us-east-1")
	srv := startFakeS3(t, f)
	host := endpointOf(srv)

	// 不给 path_style：endpoint 是 IP，应自动走 path-style
	out := result(t, call(t, "/presign", mergeArgs(baseArgs(srv, f), map[string]interface{}{"key": "a.txt"})))
	if u, _ := out["url"].(string); !strings.HasPrefix(u, "http://"+host+"/mybucket/a.txt?") {
		t.Fatalf("IP endpoint 应自动 path-style，实得 %q", u)
	}

	// 显式 path_style=false：bucket 必须打进 host（AWS / 阿里云的默认形态）
	out = result(t, call(t, "/presign", mergeArgs(baseArgs(srv, f), map[string]interface{}{
		"key": "b.txt", "path_style": false,
	})))
	if u, _ := out["url"].(string); !strings.HasPrefix(u, "http://mybucket."+host+"/b.txt?") {
		t.Fatalf("virtual-host 形态不对: %q", u)
	}

	// 显式 path_style=true：bucket 回到路径里
	out = result(t, call(t, "/presign", mergeArgs(baseArgs(srv, f), map[string]interface{}{
		"key": "c.txt", "path_style": true,
	})))
	if u, _ := out["url"].(string); !strings.HasPrefix(u, "http://"+host+"/mybucket/c.txt?") {
		t.Fatalf("path-style 形态不对: %q", u)
	}
}

// TestPresignedURLActuallyWorks 是预签名最硬的一条证据：
// 拿侧车签出来的 URL，用一个**完全不带任何 AWS 头**的普通 http.Client 去取，
// 假端点必须认 —— 这说明签名是服务端认可的，而不是我们自己算着玩。
func TestPresignedURLActuallyWorks(t *testing.T) {
	f := newFakeS3("us-east-1")
	srv := startFakeS3(t, f)
	_ = result(t, call(t, "/put", mergeArgs(baseArgs(srv, f), map[string]interface{}{
		"key": "secret/报表.txt", "data_base64": base64Of("直连下载"),
	})))

	fixed := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	old := nowFn
	nowFn = func() time.Time { return fixed }
	t.Cleanup(func() { nowFn = old })

	out := result(t, call(t, "/presign", mergeArgs(baseArgs(srv, f), map[string]interface{}{
		"key": "secret/报表.txt", "expires": 600,
	})))
	u, _ := out["url"].(string)
	if !strings.Contains(u, "X-Amz-Signature=") {
		t.Fatalf("预签名 URL 缺签名: %q", u)
	}
	if out["expires"].(float64) != 600 {
		t.Fatalf("expires 未透传: %v", out["expires"])
	}

	// 假端点也要认预签名：它不走 Authorization 头，走 query
	resp, err := http.Get(u)
	if err != nil {
		t.Fatalf("拿预签名 URL 直连失败: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("预签名 URL 被拒: %d %s", resp.StatusCode, body)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "直连下载" {
		t.Fatalf("直连下载的内容不对: %q", body)
	}
}

func TestPresignMethodAndBadMethod(t *testing.T) {
	f := newFakeS3("us-east-1")
	srv := startFakeS3(t, f)
	out := result(t, call(t, "/presign", mergeArgs(baseArgs(srv, f), map[string]interface{}{
		"key": "up.txt", "method": "PUT", "expires": 99999999,
	})))
	if out["method"].(string) != "PUT" {
		t.Fatalf("method 未透传: %v", out["method"])
	}
	if out["expires"].(float64) != float64(maxExpires) {
		t.Fatalf("expires 应被钳到 %d，实得 %v", maxExpires, out["expires"])
	}
	bad := call(t, "/presign", mergeArgs(baseArgs(srv, f), map[string]interface{}{"key": "up.txt", "method": "DELETE"}))
	if errCode(bad) != errBadRequest {
		t.Fatalf("非法 method 应报 bad_request，实得 %v", bad)
	}
}

func TestCaps(t *testing.T) {
	out := result(t, call(t, "/caps", map[string]interface{}{}))
	methods, ok := out["methods"].([]interface{})
	if !ok || len(methods) != 6 {
		t.Fatalf("caps 应列出 6 个方法: %v", out)
	}
}

func TestBadRequestBody(t *testing.T) {
	req := httptest.NewRequest("POST", "/put", strings.NewReader("{not json"))
	rec := httptest.NewRecorder()
	(&service{}).handlePut(rec, req)
	if !strings.Contains(rec.Body.String(), errBadRequest) {
		t.Fatalf("坏请求体应报 bad_request: %s", rec.Body.String())
	}
}

func TestListPrefixIsValidated(t *testing.T) {
	f := newFakeS3("us-east-1")
	srv := startFakeS3(t, f)
	out := call(t, "/list", mergeArgs(baseArgs(srv, f), map[string]interface{}{"prefix": "../.."}))
	if errCode(out) != errBadKey {
		t.Fatalf("坏 prefix 应报 bad_key，实得 %v", out)
	}
	if n := len(f.requests); n != 0 {
		t.Fatalf("坏 prefix 不应发请求，实发 %d 个", n)
	}
}

// TestListPrefixAllowsTrailingSlash 锁住 prefix 与 key 的规矩差异：
// "报表/" 这种结尾斜杠是「这个目录下」的正常写法，不能按 key 的空段规矩拒掉。
func TestListPrefixAllowsTrailingSlash(t *testing.T) {
	f := newFakeS3("us-east-1")
	srv := startFakeS3(t, f)
	_ = result(t, call(t, "/put", mergeArgs(baseArgs(srv, f), map[string]interface{}{
		"key": "dir/a.txt", "data_base64": base64Of("x"),
	})))
	ls := result(t, call(t, "/list", mergeArgs(baseArgs(srv, f), map[string]interface{}{"prefix": "dir/"})))
	if ls["count"].(float64) != 1 {
		t.Fatalf("带尾斜杠的 prefix 应被接受并命中，实得 %v", ls)
	}
}

func TestListLimitIsClamped(t *testing.T) {
	f := newFakeS3("us-east-1")
	srv := startFakeS3(t, f)
	_ = result(t, call(t, "/list", mergeArgs(baseArgs(srv, f), map[string]interface{}{"limit": 99999})))
	q := f.requests[0].query
	if !strings.Contains(q, "max-keys=1000") {
		t.Fatalf("limit 应被钳到 1000: %q", q)
	}
}

// ---- 小工具 ----

func mergeArgs(a, b map[string]interface{}) map[string]interface{} {
	out := map[string]interface{}{}
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

func base64Of(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}
