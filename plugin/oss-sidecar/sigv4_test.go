package main

import (
	"net/url"
	"strings"
	"testing"
	"time"
)

// 本文件的黄金值**不是**凭记忆写出来的，是两个独立实现现算后交叉验证过的：
//
//   - botocore（AWS 官方 Python 客户端，`botocore.auth.SigV4Auth`）
//   - aws4（Node 侧实现，`aws4.RequestSigner`）
//
// 两者对「GET /a/b c.txt?prefix=…&marker=…&max-keys=2」给出完全相同的
// canonical request、string-to-sign 与签名 `50e5677cdba71d1c88555c88de3c13b4cda2cee8e14f7ef70a5cba29d3e735e7`；
// CJK key 的编码两者也一致（`/%E7%9B%AE%E5%BD%95/%E4%B8%AD%E6%96%87%20%E6%96%87%E4%BB%B6.txt`）。
// 生成脚本留在仓库里（plugin/oss-sidecar/testdata/）：sigv4_vectors.py（botocore）与
// sigv4_vectors.js（aws4），随时可重跑复核——黄金值不是一次性手抄的死常量。
//
// 所以这里的常量是**外部事实**：改实现导致它们对不上，是实现错了，不许反过来改常量。

var (
	vecAK     = "AKIAIOSFODNN7EXAMPLE"
	vecSK     = "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
	vecTime   = time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)
	vecHost   = "mybucket.s3.amazonaws.com"
	vecRegion = "us-east-1"
	vecCreds  = credentials{AccessKeyID: vecAK, SecretAccessKey: vecSK}

	emptySHA = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
)

func TestURIEncodeKeepsOnlyUnreserved(t *testing.T) {
	cases := []struct {
		in     string
		isPath bool
		want   string
	}{
		{"a-b_c.d~e09AZ", false, "a-b_c.d~e09AZ"},
		{"a/b", true, "a/b"},
		{"a/b", false, "a%2Fb"},
		{"&=+$,:;!?'()*@", false, "%26%3D%2B%24%2C%3A%3B%21%3F%27%28%29%2A%40"},
		{"中文", false, "%E4%B8%AD%E6%96%87"},
		{"a b", false, "a%20b"},
	}
	for _, c := range cases {
		if got := uriEncode(c.in, c.isPath); got != c.want {
			t.Errorf("uriEncode(%q, isPath=%v) = %q, want %q", c.in, c.isPath, got, c.want)
		}
	}
}

// TestURIEncodeIsNotPathEscape 锁住「不许用 url.PathEscape 顶替」这条口径：
// PathEscape 会放过 & = : 这些 sub-delims，签出来的串服务端不认。
func TestURIEncodeIsNotPathEscape(t *testing.T) {
	trap := "&a=b:c"
	if got := uriEncode(trap, false); got != "%26a%3Db%3Ac" {
		t.Fatalf("uriEncode(%q) = %q；必须连 & = : 一起编码", trap, got)
	}
	if url.PathEscape(trap) == uriEncode(trap, false) {
		t.Fatal("uriEncode 与 url.PathEscape 结果相同，说明把 sub-delims 放过了")
	}
}

func TestCanonicalURI(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "/"},
		{"test.txt", "/test.txt"},
		{"/a/b c.txt", "/a/b%20c.txt"},
		{"/目录/中文 文件.txt", "/%E7%9B%AE%E5%BD%95/%E4%B8%AD%E6%96%87%20%E6%96%87%E4%BB%B6.txt"},
	}
	for _, c := range cases {
		if got := canonicalURI(c.in); got != c.want {
			t.Errorf("canonicalURI(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestCanonicalQuerySortsAndEncodes(t *testing.T) {
	got := canonicalQuery([][2]string{
		{"prefix", "somePrefix"},
		{"marker", "someMarker"},
		{"max-keys", "2"},
	})
	want := "marker=someMarker&max-keys=2&prefix=somePrefix"
	if got != want {
		t.Fatalf("canonicalQuery = %q, want %q", got, want)
	}
	// credential 里的 '/' 必须编成 %2F
	got = canonicalQuery([][2]string{{"X-Amz-Credential", vecAK + "/20130524/us-east-1/s3/aws4_request"}})
	if !strings.Contains(got, "%2F") {
		t.Fatalf("credential 里的 / 未编码: %q", got)
	}
	if canonicalQuery(nil) != "" {
		t.Fatal("空 query 必须是空串")
	}
}

// TestSignAgainstReferenceVectors 拿 botocore / aws4 的向量比对头签名。
func TestSignAgainstReferenceVectors(t *testing.T) {
	cases := []struct {
		name      string
		method    string
		canonPath string
		query     [][2]string
		body      string
		wantSTS   string // string-to-sign 的第 4 段（canonical request 的 sha256）
		wantSig   string
		wantCanon string
	}{
		{
			name:      "GET 无 query",
			method:    "GET",
			canonPath: "/test.txt",
			wantSTS:   "e705d61f9b76543dc28453995ca3a94e62fd696815541db481bb28b669f21cd5",
			wantSig:   "dc81c991e2a15956cb5d356dfeea56f51f0d8784106408757244c8a4ad4d01c9",
			wantCanon: "GET\n/test.txt\n\nhost:mybucket.s3.amazonaws.com\nx-amz-content-sha256:" + emptySHA +
				"\nx-amz-date:20130524T000000Z\n\nhost;x-amz-content-sha256;x-amz-date\n" + emptySHA,
		},
		{
			name:      "GET 带空格与 query",
			method:    "GET",
			canonPath: "/a/b%20c.txt",
			query:     [][2]string{{"prefix", "somePrefix"}, {"marker", "someMarker"}, {"max-keys", "2"}},
			wantSTS:   "6586116e5246497ae91ac1da677e0a891df8f4c27fc567d4fab2ff686c947efa",
			wantSig:   "50e5677cdba71d1c88555c88de3c13b4cda2cee8e14f7ef70a5cba29d3e735e7",
			wantCanon: "GET\n/a/b%20c.txt\nmarker=someMarker&max-keys=2&prefix=somePrefix\n" +
				"host:mybucket.s3.amazonaws.com\nx-amz-content-sha256:" + emptySHA +
				"\nx-amz-date:20130524T000000Z\n\nhost;x-amz-content-sha256;x-amz-date\n" + emptySHA,
		},
		{
			name:      "PUT 带 body",
			method:    "PUT",
			canonPath: "/a/b%20c.txt",
			body:      "hello world",
			wantSTS:   "64a0de4bb4d3355277776198c130a085fc36335c3eb064c6074ef9df113fc98a",
			wantSig:   "307bc49a4b3dea30f44b8a797cc54c9712b1f53009e6212b2734d0d0e570255d",
			wantCanon: "PUT\n/a/b%20c.txt\n\nhost:mybucket.s3.amazonaws.com\nx-amz-content-sha256:" +
				"b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9" +
				"\nx-amz-date:20130524T000000Z\n\nhost;x-amz-content-sha256;x-amz-date\n" +
				"b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9",
		},
		{
			name:      "GET CJK key",
			method:    "GET",
			canonPath: "/%E7%9B%AE%E5%BD%95/%E4%B8%AD%E6%96%87%20%E6%96%87%E4%BB%B6.txt",
			wantSTS:   "823594453cab9777f0f62d18005c779a2c116d307eac5d577ae08a7dd4bf99d6",
			wantSig:   "d581b5f5307d2d071c73f00b80111fd4487c25bb20836a3e967356472f3cd7ae",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			bodyHash := payloadHash([]byte(c.body))
			h := map[string]string{
				"host":                 vecHost,
				"x-amz-date":           vecTime.UTC().Format(amzTimeFormat),
				"x-amz-content-sha256": bodyHash,
			}
			canon := canonicalRequest(c.method, c.canonPath, canonicalQuery(c.query), h, bodyHash)
			if c.wantCanon != "" && canon != c.wantCanon {
				t.Fatalf("canonical request 不符\n got: %q\nwant: %q", canon, c.wantCanon)
			}
			sts := stringToSign(vecTime, vecRegion, canon)
			if !strings.HasSuffix(sts, c.wantSTS) {
				t.Fatalf("string-to-sign 末段不符\n got: %q\nwant 末段: %q", sts, c.wantSTS)
			}
			if got := signature(vecSK, vecTime, vecRegion, sts); got != c.wantSig {
				t.Fatalf("signature = %q, want %q", got, c.wantSig)
			}
			// 顺带锁住 signHeaders 走的是同一条路径（Authorization 里带的就是这个签名）
			out := signHeaders(vecCreds, vecTime, vecRegion, c.method, vecHost, c.canonPath, c.query, nil, bodyHash)
			if !strings.Contains(out["Authorization"], "Signature="+c.wantSig) {
				t.Fatalf("signHeaders 的 Authorization 未带预期签名: %q", out["Authorization"])
			}
		})
	}
}

// TestPresignAgainstReferenceVectors 拿 aws4 的向量比对预签名。
func TestPresignAgainstReferenceVectors(t *testing.T) {
	cases := []struct {
		name      string
		canonPath string
		wantSTS   string
		wantSig   string
	}{
		{
			name:      "presign /test.txt",
			canonPath: "/test.txt",
			wantSTS:   "be97a7e9a85cea0cf7f945e9d83a2ffd30c80075be6386c012ec6ef8caa5f4e9",
			wantSig:   "a88d613e5378581a3fb9764dfa4ab9ffeb8a55d339865567e7ef413885a5fa90",
		},
		{
			name:      "presign CJK",
			canonPath: "/%E7%9B%AE%E5%BD%95/%E4%B8%AD%E6%96%87%20%E6%96%87%E4%BB%B6.txt",
			wantSTS:   "26bed6da37c119fc6d6bdd01a8bd24234b252e98e109f1b9408594ddb93d7311",
			wantSig:   "2bd43e96b900c60ec8672a7b34d530e2b523d838d97670c9a284d482cc6d4ebf",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// 复刻 presignURL 内部的规范请求，确认它算的就是向量那一份
			query := [][2]string{
				{"X-Amz-Algorithm", sigV4Algorithm},
				{"X-Amz-Credential", vecAK + "/" + credentialScope(vecTime, vecRegion)},
				{"X-Amz-Date", vecTime.UTC().Format(amzTimeFormat)},
				{"X-Amz-Expires", "86400"},
				{"X-Amz-SignedHeaders", "host"},
			}
			canon := canonicalRequest("GET", c.canonPath, canonicalQuery(query),
				map[string]string{"host": vecHost}, unsignedPayload)
			sts := stringToSign(vecTime, vecRegion, canon)
			if !strings.HasSuffix(sts, c.wantSTS) {
				t.Fatalf("string-to-sign 末段不符\n got: %q\nwant 末段: %q", sts, c.wantSTS)
			}
			if got := signature(vecSK, vecTime, vecRegion, sts); got != c.wantSig {
				t.Fatalf("signature = %q, want %q", got, c.wantSig)
			}

			got := presignURL(vecCreds, vecTime, vecRegion, "GET", "https", vecHost, c.canonPath, 86400)
			if !strings.Contains(got, "X-Amz-Signature="+c.wantSig) {
				t.Fatalf("presignURL = %q\nwant 含 X-Amz-Signature=%s", got, c.wantSig)
			}
			u, err := url.Parse(got)
			if err != nil {
				t.Fatalf("产出的 URL 解析不了: %v", err)
			}
			for _, k := range []string{"X-Amz-Algorithm", "X-Amz-Credential", "X-Amz-Date", "X-Amz-Expires", "X-Amz-SignedHeaders", "X-Amz-Signature"} {
				if u.Query().Get(k) == "" {
					t.Fatalf("预签名 URL 缺 %s: %q", k, got)
				}
			}
			if u.Query().Get("X-Amz-Expires") != "86400" {
				t.Fatalf("expires 透传不对: %q", u.Query().Get("X-Amz-Expires"))
			}
		})
	}
}

// TestPresignWithSessionToken 锁住临时凭据：token 进 query 且参与签名（改它签名就变）。
func TestPresignWithSessionToken(t *testing.T) {
	withTok := credentials{AccessKeyID: vecAK, SecretAccessKey: vecSK, SessionToken: "FwoGZXIvYXdz"}
	plain := presignURL(vecCreds, vecTime, vecRegion, "GET", "https", vecHost, "/k.txt", 3600)
	tok := presignURL(withTok, vecTime, vecRegion, "GET", "https", vecHost, "/k.txt", 3600)
	if !strings.Contains(tok, "X-Amz-Security-Token=") {
		t.Fatalf("带 session token 的预签名 URL 没带上它: %q", tok)
	}
	if plain == tok {
		t.Fatal("带不带 token 产出同一个 URL，说明 token 没进签名面")
	}
}
