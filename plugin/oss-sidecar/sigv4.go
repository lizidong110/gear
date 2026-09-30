// Command oss-sidecar 是 ext.oss 的常驻侧车（service ext）：桥接 S3 兼容的对象存储。
//
// 三条口径（写死的，别改成「更方便」）：
//  1. **零第三方依赖**：AWS Signature V4 全手写（`crypto/hmac` + `crypto/sha256`），
//     不引 aws-sdk-go —— 一引就牵动 go.sum 全量变动、GOPROXY 离线即挂，
//     而核心价值（能读写对象）跟用哪个库无关（与 ext.excel / ext.pdf 同一条路）。
//  2. **只做 S3 协议一套**：阿里云 OSS / 腾讯云 COS / MinIO / R2 / Ceph 都兼容 S3，
//     靠 endpoint 区分；不做第二套协议（每多一套协议就要多引一个 SDK）。
//  3. **凭据只从调用参数进**：PRD v40 §495 定死 ext 侧车禁 os.Getenv（COORD_ADDR 是唯一例外）。
//     endpoint / bucket / region 也**不做配置库回退**（与 ext.filestore 不同，是有意的选择）：
//     它们跟 ak/sk 一样每次从入参传，省掉「配置与入参不一致时听谁的」这个歧义源，
//     侧车也就成了无状态进程。
package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	sigV4Algorithm  = "AWS4-HMAC-SHA256"
	sigV4Service    = "s3"
	sigV4Terminator = "aws4_request"
	// unsignedPayload 是预签名（以及任何「签名时不算 body」的情形）用的 payload 占位串。
	// S3 预签名必须用它：客户端发起请求时服务端不会再算一遍 body 哈希。
	unsignedPayload = "UNSIGNED-PAYLOAD"

	amzTimeFormat = "20060102T150405Z"
	amzDateFormat = "20060102"
)

// credentials 是签名用的凭据。刻意不提供任何「从环境变量读」的入口 —— 见包注释第 3 条。
type credentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string // STS 临时凭据；非空时进 x-amz-security-token
}

func (c credentials) complete() bool {
	return c.AccessKeyID != "" && c.SecretAccessKey != ""
}

// ---- RFC 3986 编码 ----

const hexUpper = "0123456789ABCDEF"

// uriEncode 只保留 RFC 3986 的非保留字符 A-Za-z0-9-_.~，其余一律 %XX（大写十六进制）。
//
// isPath 决定 '/' 的命运：路径里 '/' 是段分隔符，必须原样留着；
// 查询串里 '/' 只是个普通字符，必须编成 %2F（X-Amz-Credential 里的斜杠就是例子）。
//
// 刻意不用 url.PathEscape：它放过 : @ & = + $ ! ' ( ) * , ; 这些 sub-delims，
// 而 SigV4 的 canonical URI / canonical query 要求它们必须编码 —— botocore（AWS 官方客户端）
// 与 aws4（Node 侧实现）都这么做，且两者对同一请求给出**同一个签名**（已交叉验证，
// 向量来源见 sigv4_test.go）。用 PathEscape 会在 key 含 & = : 时签出服务端不认的串。
func uriEncode(s string, isPath bool) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		case c == '/' && isPath:
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(hexUpper[c>>4])
			b.WriteByte(hexUpper[c&0x0F])
		}
	}
	return b.String()
}

// canonicalURI 逐段编码路径：'/' 是段分隔符，不编码（编码了就不是一个路径了）。
// 空 path 的 canonical URI 是 "/"。
func canonicalURI(path string) string {
	if path == "" {
		return "/"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	segs := strings.Split(path, "/")
	for i := range segs {
		segs[i] = uriEncode(segs[i], true)
	}
	return strings.Join(segs, "/")
}

// canonicalQuery 按「编码后的 key、再编码后的 value」排序拼 k=v。
// 传进来的 k/v 都是未编码原文；query 里的 '/' 必须编成 %2F（X-Amz-Credential 就是例子）。
func canonicalQuery(pairs [][2]string) string {
	if len(pairs) == 0 {
		return ""
	}
	enc := make([][2]string, 0, len(pairs))
	for _, p := range pairs {
		enc = append(enc, [2]string{uriEncode(p[0], false), uriEncode(p[1], false)})
	}
	sort.Slice(enc, func(i, j int) bool {
		if enc[i][0] != enc[j][0] {
			return enc[i][0] < enc[j][0]
		}
		return enc[i][1] < enc[j][1]
	})
	parts := make([]string, 0, len(enc))
	for _, e := range enc {
		parts = append(parts, e[0]+"="+e[1])
	}
	return strings.Join(parts, "&")
}

// canonicalHeaderBlock 返回 canonical headers 块（每行 "名:值\n"，末尾带换行）
// 与 signedHeaders 串（小写名按序用 ';' 连）。
//
// 只签显式传进来的那几个头：Go 的 transport 会自己补 Content-Length / User-Agent / Accept-Encoding，
// 签名面必须是我们自己决定、可复现的集合，不能取决于 transport 当时补了什么。
func canonicalHeaderBlock(h map[string]string) (block string, signed string) {
	names := make([]string, 0, len(h))
	for k := range h {
		names = append(names, strings.ToLower(k))
	}
	sort.Strings(names)
	var b strings.Builder
	for _, n := range names {
		b.WriteString(n)
		b.WriteByte(':')
		b.WriteString(strings.TrimSpace(h[n]))
		b.WriteByte('\n')
	}
	return b.String(), strings.Join(names, ";")
}

// canonicalRequest 拼六段规范请求（段间 \n；headers 块自带末尾换行，所以第 4、5 段之间会空一行）。
func canonicalRequest(method, uri, query string, headers map[string]string, payloadHash string) string {
	block, signed := canonicalHeaderBlock(headers)
	return strings.Join([]string{method, uri, query, block, signed, payloadHash}, "\n")
}

// credentialScope 形如 20130524/us-east-1/s3/aws4_request。
func credentialScope(t time.Time, region string) string {
	return strings.Join([]string{
		t.UTC().Format(amzDateFormat), region, sigV4Service, sigV4Terminator,
	}, "/")
}

func stringToSign(t time.Time, region, canonReq string) string {
	sum := sha256.Sum256([]byte(canonReq))
	return strings.Join([]string{
		sigV4Algorithm,
		t.UTC().Format(amzTimeFormat),
		credentialScope(t, region),
		hex.EncodeToString(sum[:]),
	}, "\n")
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	_, _ = m.Write([]byte(data))
	return m.Sum(nil)
}

// signingKey 是四段 HMAC 链：kDate → kRegion → kService → kSigning。
func signingKey(secret string, t time.Time, region string) []byte {
	k := hmacSHA256([]byte("AWS4"+secret), t.UTC().Format(amzDateFormat))
	k = hmacSHA256(k, region)
	k = hmacSHA256(k, sigV4Service)
	return hmacSHA256(k, sigV4Terminator)
}

func signature(secret string, t time.Time, region, sts string) string {
	return hex.EncodeToString(hmacSHA256(signingKey(secret, t, region), sts))
}

// payloadHash 算 body 的 sha256 十六进制；body 为空即空串哈希（不是 UNSIGNED-PAYLOAD）。
func payloadHash(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// ---- 头签名 ----

// signHeaders 生成一次普通（非预签名）调用要打的全部 AWS 头。
// canonPath 必须是已编码的规范路径（canonicalURI 的产物），query 是未编码的 k/v 原文。
// extra 是要一起签的业务头（如 content-type），key 大小写随意。
func signHeaders(c credentials, t time.Time, region, method, host, canonPath string,
	query [][2]string, extra map[string]string, bodyHash string) map[string]string {

	h := map[string]string{
		"host":                 host,
		"x-amz-date":           t.UTC().Format(amzTimeFormat),
		"x-amz-content-sha256": bodyHash,
	}
	for k, v := range extra {
		h[strings.ToLower(k)] = v
	}
	if c.SessionToken != "" {
		h["x-amz-security-token"] = c.SessionToken
	}

	sts := stringToSign(t, region, canonicalRequest(method, canonPath, canonicalQuery(query), h, bodyHash))
	sig := signature(c.SecretAccessKey, t, region, sts)

	out := map[string]string{
		"X-Amz-Date":           h["x-amz-date"],
		"X-Amz-Content-Sha256": bodyHash,
		"Authorization": sigV4Algorithm +
			" Credential=" + c.AccessKeyID + "/" + credentialScope(t, region) +
			", SignedHeaders=" + canonicalSignedHeaders(h) +
			", Signature=" + sig,
	}
	for k, v := range extra {
		out[k] = v
	}
	if c.SessionToken != "" {
		out["X-Amz-Security-Token"] = c.SessionToken
	}
	return out
}

func canonicalSignedHeaders(h map[string]string) string {
	_, signed := canonicalHeaderBlock(h)
	return signed
}

// ---- 预签名 ----

// presignURL 生成带 X-Amz-Signature 的完整 URL（S3 语义：只签 host，payload 用 UNSIGNED-PAYLOAD）。
//
// 为什么只签 host：预签名 URL 是给浏览器/第三方直连用的，发起方不会带我们算的那些头，
// 多签一个头就等于要求对方必须带上它，实际就是签了也传不过来（aws4 / botocore 同样只签 host）。
func presignURL(c credentials, t time.Time, region, method, scheme, host, canonPath string, expires int) string {
	query := [][2]string{
		{"X-Amz-Algorithm", sigV4Algorithm},
		{"X-Amz-Credential", c.AccessKeyID + "/" + credentialScope(t, region)},
		{"X-Amz-Date", t.UTC().Format(amzTimeFormat)},
		{"X-Amz-Expires", strconv.Itoa(expires)},
		{"X-Amz-SignedHeaders", "host"},
	}
	if c.SessionToken != "" {
		query = append(query, [2]string{"X-Amz-Security-Token", c.SessionToken})
	}

	headers := map[string]string{"host": host}
	sts := stringToSign(t, region, canonicalRequest(method, canonPath, canonicalQuery(query), headers, unsignedPayload))
	sig := signature(c.SecretAccessKey, t, region, sts)

	query = append(query, [2]string{"X-Amz-Signature", sig})
	parts := make([]string, 0, len(query))
	for _, p := range query {
		parts = append(parts, uriEncode(p[0], false)+"="+uriEncode(p[1], false))
	}
	return scheme + "://" + host + canonPath + "?" + strings.Join(parts, "&")
}
