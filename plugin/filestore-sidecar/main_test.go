package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newTestService 构造一个 isolated 的 filestore 侧车（root 由每个用例显式传入，
// 配置库落临时目录），保证用例之间不共享状态。
func newTestService(t *testing.T) *Service {
	t.Helper()
	s := newService(filepath.Join(t.TempDir(), "config.db"))
	// 配置库持有 sqlite 连接直到进程结束，不关会让 t.TempDir 的清理在 Windows 上失败
	// （"being used by another process"）。
	t.Cleanup(func() { _ = s.cfg.Close() })
	return s
}

// put 是一次 /put 调用（args 直接是本方法的入参，内部会包成 {"args":...}）。
func put(t *testing.T, s *Service, method string, args map[string]interface{}) map[string]interface{} {
	t.Helper()
	body, err := json.Marshal(map[string]interface{}{"args": args})
	if err != nil {
		t.Fatalf("序列化 args 失败: %v", err)
	}
	rec := httptest.NewRecorder()
	s.Handlers()[method](rec, httptest.NewRequest(http.MethodPost, method, bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("%s 非 200: %d %s", method, rec.Code, rec.Body.String())
	}
	var out struct {
		Result map[string]interface{} `json:"result"`
		Error  string                 `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("%s 响应不是 JSON: %s", method, rec.Body.String())
	}
	return out.Result
}

// errOf 断言该调用应返回 error 并把它取出来。
func errOf(t *testing.T, s *Service, method string, args map[string]interface{}) string {
	t.Helper()
	body, _ := json.Marshal(map[string]interface{}{"args": args})
	rec := httptest.NewRecorder()
	s.Handlers()[method](rec, httptest.NewRequest(http.MethodPost, method, bytes.NewReader(body)))
	var out struct {
		Result map[string]interface{} `json:"result"`
		Error  string                 `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Error == "" {
		t.Fatalf("%s 本应失败，却返回了 result: %v", method, out.Result)
	}
	return out.Error
}

// b64 是测试里造二进制载体的简易构造器（故意用非 ASCII 字节，确保走的是字节而非字符串路径）。
func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// TestFilestorePutGetRoundTrip：AC-E2-1 —— 二进制 + 中文文件名往返一致，content_type 保持。
func TestFilestorePutGetRoundTrip(t *testing.T) {
	s := newTestService(t)
	root := t.TempDir()

	key := "contracts/2026/三季度.pdf"
	payload := []byte("\x00\x01\x02二进制 ¥中文 content\xff\xfe")
	res := put(t, s, "/put", map[string]interface{}{
		"root": root, "key": key, "data_base64": base64.StdEncoding.EncodeToString(payload),
		"content_type": "application/pdf",
	})
	if got := res["size"]; got != float64(len(payload)) {
		t.Fatalf("size = %v，want %d", got, len(payload))
	}
	if got := res["content_type"]; got != "application/pdf" {
		t.Fatalf("content_type = %v", got)
	}
	if got := res["mod_time"]; got == "" || got == "0001-01-01T00:00:00Z" {
		t.Fatalf("mod_time 未回读: %v", got)
	}

	got := put(t, s, "/get", map[string]interface{}{"root": root, "key": key})
	raw, err := base64.StdEncoding.DecodeString(got["data_base64"].(string))
	if err != nil {
		t.Fatalf("get 返回的不是合法 base64: %v", err)
	}
	if !bytes.Equal(raw, payload) {
		t.Fatalf("往返字节不一致: want %x got %x", payload, raw)
	}
	if got["content_type"] != "application/pdf" {
		t.Fatalf("content_type 未保持: %v", got["content_type"])
	}
	// 中文文件名的 key 原样回显（不做 URL 编码，url() 那层才编码）。
	if got["key"] != key {
		t.Fatalf("key 回显不一致: %v", got["key"])
	}
}

// TestFilestoreStatListDelete：stat 只看不取；list 默认全量且跳过索引文件；delete 后再取即失败。
func TestFilestoreStatListDelete(t *testing.T) {
	s := newTestService(t)
	root := t.TempDir()
	args := map[string]interface{}{"root": root, "key": "a/b.txt", "data_base64": b64("hello")}
	put(t, s, "/put", args)
	put(t, s, "/put", map[string]interface{}{"root": root, "key": "a/c.txt", "data_base64": b64("world!")})

	st := put(t, s, "/stat", map[string]interface{}{"root": root, "key": "a/b.txt"})
	if st["size"] != float64(5) || st["content_type"] != defaultContentType {
		t.Fatalf("stat 不对: %v", st)
	}

	list := put(t, s, "/list", map[string]interface{}{"root": root})
	keys := fmt.Sprint(list["keys"])
	if !strings.Contains(keys, "a/b.txt") || !strings.Contains(keys, "a/c.txt") {
		t.Fatalf("list 未列出对象: %v", list["keys"])
	}
	if strings.Contains(keys, indexName) {
		t.Fatalf("list 不该包含索引文件本身: %v", list["keys"])
	}
	pfx := put(t, s, "/list", map[string]interface{}{"root": root, "prefix": "a/b"})
	if strings.Contains(fmt.Sprint(pfx["keys"]), "a/c.txt") {
		t.Fatalf("prefix 过滤失效: %v", pfx["keys"])
	}

	if e := errOf(t, s, "/get", map[string]interface{}{"root": root, "key": "a/zzz"}); !strings.Contains(e, "对象不存在") {
		t.Fatalf("取不存在的对象应报「对象不存在」: %s", e)
	}
	if out := put(t, s, "/delete", map[string]interface{}{"root": root, "key": "a/b.txt"}); out["ok"] != true {
		t.Fatalf("删除已存在的对象应成功: %v", out)
	}
	if e := errOf(t, s, "/get", map[string]interface{}{"root": root, "key": "a/b.txt"}); !strings.Contains(e, "对象不存在") {
		t.Fatalf("删除后应取不到: %s", e)
	}
	// 删完索引条目也要跟着消失（否则 list 会复活已删对象）。
	list2 := put(t, s, "/list", map[string]interface{}{"root": root})
	if strings.Contains(fmt.Sprint(list2["keys"]), "a/b.txt") {
		t.Fatalf("删除后索引条目未清理: %v", list2["keys"])
	}
}

// TestFilestoreRejectsPathTraversal：AC-E2-2 —— "../" 与混用反斜杠都在写盘前被拒。
func TestFilestoreRejectsPathTraversal(t *testing.T) {
	s := newTestService(t)
	root := t.TempDir()
	for _, bad := range []string{
		"../../evil.txt",
		"a/../../evil.txt",
		"/abs.txt",
		"",
		"a//b.txt",
		"./a.txt",
		`a\b.txt`,
	} {
		if e := errOf(t, s, "/put", map[string]interface{}{
			"root": root, "key": bad, "data_base64": b64("x"),
		}); e == "" {
			t.Fatalf("key %q 本应被拒，却写入成功", bad)
		}
	}
	// 一条都没落地：目录里除了可能创建的空目录不应有文件。
	filepath.WalkDir(root, func(p string, d os.DirEntry, _ error) error {
		if !d.IsDir() && d.Name() != indexName {
			t.Fatalf("非法 key 却写了文件: %s", p)
		}
		return nil
	})
}

// TestWithinRootAndResolveTarget：AC-E2-2 的判定核心（纯函数，任何环境都能验）。
// 越界判定与「是不是符号链接」是两件事：前者是纯粹的几何判定，必须无条件绿；
// 后者依赖 OS 能否建真符号链接（见下一个用例的环境限制）。
func TestWithinRootAndResolveTarget(t *testing.T) {
	root := filepath.Join(string(filepath.Separator), "data", "store")
	cases := []struct {
		name string
		root string
		path string
		want bool
	}{
		{"root 自身算在内", root, root, true},
		{"普通子对象", root, filepath.Join(root, "a", "b.txt"), true},
		{"上级越界", root, filepath.Dir(root), false},
		{"兄弟目录越界", root, filepath.Join(filepath.Dir(root), "other", "x"), false},
		{"同级 ../ 越界", root, filepath.Join(root, "..", "secret.txt"), false},
	}
	for _, c := range cases {
		if got := withinRoot(c.path, c.root); got != c.want {
			t.Fatalf("withinRoot(%s, %s) = %v, want %v", c.path, c.root, got, c.want)
		}
	}
	// key 拼出来若越出 root，必须在写盘前就被拒。
	if _, err := resolveTarget(root, "a/../../secret.txt"); err == nil {
		t.Fatalf("resolveTarget 未拦住越界 key")
	}
	if _, err := resolveTarget(root, "a/b.txt"); err != nil {
		t.Fatalf("resolveTarget 误拒合法 key: %v", err)
	}
	// normalizeKey 的段级规则（空段 / "." / ".." / 反斜杠）——与越界判定互为两道闸。
	for _, bad := range []string{"a//b", "./a", "a/../b", "..", "a\\b"} {
		if _, err := normalizeKey(bad); err == nil {
			t.Fatalf("normalizeKey 未拦住 %q", bad)
		}
	}
	if k, err := normalizeKey("contracts/2026/三季度.pdf"); err != nil || k != "contracts/2026/三季度.pdf" {
		t.Fatalf("normalizeKey 改写了合法 key: %q %v", k, err)
	}
}

// TestFilestoreRejectsSymlinkEscape：AC-E2-2 —— 符号链接指向 root 外时，读/写/删一律拒绝。
//
// ⚠ 环境限制（2026-09-28 实测）：本机（沙箱）os.Symlink 会「成功」但建出的是一个 0 字节
// 普通文件——Lstat 没有符号链接位、EvalSymlinks 报「找不到路径」。所以这里先自检**真的**
// 符号链接建成了没有，建不成就跳过而不是假装通过（假绿比没测更坏）。
func TestFilestoreRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatalf("准备越界文件失败: %v", err)
	}
	link := filepath.Join(root, "link.txt")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("本机不允许建符号链接: %v", err)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Skipf("本机 os.Symlink 未产生真正的符号链接（Lstat=%v, mode=%v）——沙箱/权限环境限制，跳过",
			err, fi.Mode())
	}

	s := newTestService(t)
	if e := errOf(t, s, "/get", map[string]interface{}{"root": root, "key": "link.txt"}); !strings.Contains(e, "符号链接") {
		t.Fatalf("经符号链接读应被拒: %s", e)
	}
	if e := errOf(t, s, "/stat", map[string]interface{}{"root": root, "key": "link.txt"}); !strings.Contains(e, "符号链接") {
		t.Fatalf("经符号链接 stat 应被拒: %s", e)
	}
	if e := errOf(t, s, "/delete", map[string]interface{}{"root": root, "key": "link.txt"}); !strings.Contains(e, "符号链接") {
		t.Fatalf("经符号链接删应被拒（否则删的是 root 外的文件）: %s", e)
	}
	if e := errOf(t, s, "/put", map[string]interface{}{
		"root": root, "key": "link.txt", "data_base64": b64("x"),
	}); !strings.Contains(e, "符号链接") {
		t.Fatalf("写穿符号链接应被拒: %s", e)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("越界目标不该被动过: %v", err)
	}
}

// TestFilestoreSizeLimitFailClosed：AC-E2-3 —— 超限拒写且磁盘不留残留文件。
func TestFilestoreSizeLimitFailClosed(t *testing.T) {
	s := newTestService(t)
	root := t.TempDir()
	e := errOf(t, s, "/put", map[string]interface{}{
		"root": root, "key": "big.bin", "data_base64": b64("0123456789"), "max_bytes": 4,
	})
	if !strings.Contains(e, "大小上限") {
		t.Fatalf("超限应报「大小上限」: %s", e)
	}
	// 上限**之内**的必须能写（否则上限形同虚设）。
	put(t, s, "/put", map[string]interface{}{
		"root": root, "key": "small.bin", "data_base64": b64("0123"), "max_bytes": 4,
	})
	// 残留检查：除索引外磁盘上只应有 small.bin。
	filepath.WalkDir(root, func(p string, d os.DirEntry, _ error) error {
		name := d.Name()
		if d.IsDir() || name == indexName || name == indexName+".tmp" || name == "small.bin" {
			return nil
		}
		t.Fatalf("超限写入留下了残留文件: %s", p)
		return nil
	})
}

// TestFilestoreRootUnconfiguredFailClosed：AC-E2-4 —— 既不传 root 也没配 filestore.root 时拒绝
// （绝不退化成「写到当前目录」）。
func TestFilestoreRootUnconfiguredFailClosed(t *testing.T) {
	s := newTestService(t) // 配置库是空的临时库：没有 filestore.root
	e := errOf(t, s, "/put", map[string]interface{}{"key": "a.txt", "data_base64": b64("x")})
	if !strings.Contains(e, cfgKeyRoot) {
		t.Fatalf("应提示如何配置 root: %s", e)
	}
	// 配上了就能用（同一条回退链的反向证明）。
	if err := s.cfg.Set(cfgKeyRoot, t.TempDir()); err != nil {
		t.Fatalf("写配置失败: %v", err)
	}
	if out := put(t, s, "/put", map[string]interface{}{"key": "a.txt", "data_base64": b64("x")}); out["size"] != float64(1) {
		t.Fatalf("配置 root 后应可用: %v", out)
	}
}

// TestFilestoreRejectsBadBase64：非法载荷要报结构化错误，而不是把空字节写进库。
func TestFilestoreRejectsBadBase64(t *testing.T) {
	s := newTestService(t)
	root := t.TempDir()
	if e := errOf(t, s, "/put", map[string]interface{}{
		"root": root, "key": "a.txt", "data_base64": "!!!not-base64!!!",
	}); !strings.Contains(e, "base64") {
		t.Fatalf("非法 base64 未被识别: %s", e)
	}
	if _, err := os.Stat(filepath.Join(root, "a.txt")); !os.IsNotExist(err) {
		t.Fatalf("非法载荷不该落盘")
	}
}

// TestFilestoreURLIsRelative：url 只回相对路径（内核 REST 面 +0 的红线）。
func TestFilestoreURLIsRelative(t *testing.T) {
	s := newTestService(t)
	root := t.TempDir()
	out := put(t, s, "/url", map[string]interface{}{"root": root, "key": "a/b.txt"})
	p, _ := out["path"].(string)
	if p != "/_filestore/a/b.txt" {
		t.Fatalf("url 应返回相对路径，got %q", p)
	}
}

// TestFilestoreCapsRedLine：/caps 必须覆盖 extensions.yaml 声明的方法（红线在 verify.py，
// 这里只锁「侧车侧报出来的名单本身是齐的」，防止改方法时忘改 /caps）。
func TestFilestoreCapsRedLine(t *testing.T) {
	s := newTestService(t)
	rec := httptest.NewRecorder()
	s.Handlers()["/caps"](rec, httptest.NewRequest(http.MethodGet, "/caps", nil))
	var out struct {
		Result struct {
			Ext     string   `json:"ext"`
			Methods []string `json:"methods"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("/caps 响应不是 JSON: %s", rec.Body.String())
	}
	if out.Result.Ext != "filestore" {
		t.Fatalf("ext 名不对: %s", out.Result.Ext)
	}
	want := map[string]bool{"put": false, "get": false, "stat": false, "list": false, "delete": false, "url": false}
	for _, m := range out.Result.Methods {
		if _, ok := want[m]; ok {
			want[m] = true
		}
	}
	for m, ok := range want {
		if !ok {
			t.Fatalf("/caps 漏报方法 %s（改了实现却没改 /caps）", m)
		}
	}
}
