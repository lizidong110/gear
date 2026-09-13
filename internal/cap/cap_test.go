package cap

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func testProviders() *Providers {
	return &Providers{
		KV:     NewMemKV(),
		Secret: MapSecret{"api_token": "s3cr3t"},
		HTTP:   http.DefaultClient,
		Event:  NewMemEvent(),
		Resolver: func(target string, input map[string]interface{}) (map[string]interface{}, error) {
			return map[string]interface{}{"echo": target}, nil
		},
	}
}

func TestGrantsFromStrings(t *testing.T) {
	g, err := GrantsFromStrings([]string{"kv", "secret"})
	if err != nil {
		t.Fatalf("合法列表应成功: %v", err)
	}
	if !g.Has(PrimKV) || !g.Has(PrimSecret) || g.Has(PrimHTTP) {
		t.Fatal("Grants 内容错误")
	}
	if _, err := GrantsFromStrings([]string{"kv", "bogus"}); err == nil {
		t.Fatal("期望报错：未知 capability")
	}
}

func TestContextGateDeniesUngranted(t *testing.T) {
	g, _ := GrantsFromStrings([]string{"kv", "secret"})
	c := New(g, testProviders())

	if _, err := c.KV(); err != nil {
		t.Fatalf("kv 已授权应通过: %v", err)
	}
	if _, err := c.Secret(); err != nil {
		t.Fatalf("secret 已授权应通过: %v", err)
	}
	// 未授权的原语必须被拒（最小权限）。
	for _, fn := range []func() error{
		func() error { _, e := c.HTTP(); return e },
		func() error { _, e := c.Event(); return e },
		func() error { _, e := c.Call(); return e },
		func() error { _, e := c.DB(); return e },
	} {
		if err := fn(); err != ErrCapabilityDenied {
			t.Fatalf("未授权原语应返回 ErrCapabilityDenied，实为 %v", err)
		}
	}
}

func TestMemKV(t *testing.T) {
	k := NewMemKV()
	k.Put("a", "1")
	v, ok := k.Get("a")
	if !ok || v != "1" {
		t.Fatalf("Get 失败: %v %v", v, ok)
	}
	k.Delete("a")
	if _, ok := k.Get("a"); ok {
		t.Fatal("Delete 后应不存在")
	}
}

func TestMapSecret(t *testing.T) {
	s := MapSecret{"api_token": "s3cr3t"}
	v, err := s.Get("api_token")
	if err != nil || v != "s3cr3t" {
		t.Fatalf("Get 失败: %v %v", v, err)
	}
	if _, err := s.Get("missing"); err != ErrSecretNotFound {
		t.Fatalf("缺失应返回 ErrSecretNotFound，实为 %v", err)
	}
}

func TestHTTPViaHttptest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	g, _ := GrantsFromStrings([]string{"http"})
	c := New(g, testProviders())
	h, err := c.HTTP()
	if err != nil {
		t.Fatalf("HTTP 应通过: %v", err)
	}
	resp, err := h.Request("GET", srv.URL, nil, nil)
	if err != nil {
		t.Fatalf("Request 失败: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("状态码应为 200，实为 %d", resp.StatusCode)
	}
	if string(resp.Body) != `{"ok":true}` {
		t.Fatalf("响应体错误: %s", resp.Body)
	}
}

func TestMemEvent(t *testing.T) {
	e := NewMemEvent()
	done := make(chan interface{}, 1)
	go func() {
		v, err := e.Wait("tick")
		if err != nil {
			t.Errorf("Wait 失败: %v", err)
			return
		}
		done <- v
	}()
	e.Notify("tick", map[string]interface{}{"n": 1})
	v := <-done
	m, ok := v.(map[string]interface{})
	if !ok || m["n"] != 1 {
		t.Fatalf("事件负载错误: %v", v)
	}
}

func TestDBNilUnavailable(t *testing.T) {
	// 授权 db，但 Providers.DB 为 nil → ErrCapabilityUnavailable。
	g, _ := GrantsFromStrings([]string{"db"})
	c := New(g, testProviders())
	if _, err := c.DB(); err != ErrCapabilityUnavailable {
		t.Fatalf("db 未实现应返回 ErrCapabilityUnavailable，实为 %v", err)
	}
}

func TestCallResolver(t *testing.T) {
	g, _ := GrantsFromStrings([]string{"call"})
	c := New(g, testProviders())
	cl, err := c.Call()
	if err != nil {
		t.Fatalf("Call 应通过: %v", err)
	}
	out, err := cl.Call("toolX", map[string]interface{}{"x": 1})
	if err != nil {
		t.Fatalf("Call 失败: %v", err)
	}
	if out["echo"] != "toolX" {
		t.Fatalf("Resolver 输出错误: %v", out)
	}
}

// 授权但宿主未注入对应实现（Providers 字段为 nil）→ ErrCapabilityUnavailable（而非 panic/静默成功）。
func TestContextUnavailableWhenProviderNil(t *testing.T) {
	// Providers 整体存在，但各原语实现缺失。
	prov := &Providers{}
	g, _ := GrantsFromStrings([]string{"kv", "secret", "event", "call"})
	c := New(g, prov)
	if _, err := c.KV(); err != ErrCapabilityUnavailable {
		t.Fatalf("kv 未实现应 ErrCapabilityUnavailable，实为 %v", err)
	}
	if _, err := c.Secret(); err != ErrCapabilityUnavailable {
		t.Fatalf("secret 未实现应 ErrCapabilityUnavailable，实为 %v", err)
	}
	if _, err := c.Event(); err != ErrCapabilityUnavailable {
		t.Fatalf("event 未实现应 ErrCapabilityUnavailable，实为 %v", err)
	}
	if _, err := c.Call(); err != ErrCapabilityUnavailable {
		t.Fatalf("call 未实现应 ErrCapabilityUnavailable，实为 %v", err)
	}
}

// Providers 整体为 nil（未配置 WithHost）时，已授权原语亦返回 Unavailable（修复潜在 nil panic）。
func TestHTTPUnavailableWhenNoProvider(t *testing.T) {
	g, _ := GrantsFromStrings([]string{"http"})
	c := New(g, nil) // 宿主未注入
	if _, err := c.HTTP(); err != ErrCapabilityUnavailable {
		t.Fatalf("无宿主时 http 应 ErrCapabilityUnavailable，实为 %v", err)
	}
}

// Capabilities / Sandbox 是注入到进程 input.ctx 的字段，需被覆盖。
func TestContextCapabilitiesAndSandbox(t *testing.T) {
	g, _ := GrantsFromStrings([]string{"kv", "event"})
	c := New(g, testProviders())
	caps := c.Capabilities()
	if len(caps) != 2 {
		t.Fatalf("Capabilities 应含 2 项，实为 %v", caps)
	}
	if c.Sandbox() != false {
		t.Fatalf("默认 sandbox 应为 false")
	}
}

// New(nil) 应得到空 Grants，Capabilities 返回空切片（不 panic）。
func TestNewNilGrants(t *testing.T) {
	c := New(nil, nil)
	if len(c.Capabilities()) != 0 {
		t.Fatalf("nil grants 的 Capabilities 应为空，实为 %v", c.Capabilities())
	}
	if c.Sandbox() != false {
		t.Fatalf("sandbox 应为 false")
	}
}

// WaitCtx 带取消的等待路径（Wait 的无 ctx 版本由 TestMemEvent 覆盖）。
func TestMemEventWaitCtx(t *testing.T) {
	e := NewMemEvent()
	done := make(chan interface{}, 1)
	go func() {
		v, err := e.WaitCtx("tick2", context.Background())
		if err != nil {
			t.Errorf("WaitCtx 失败: %v", err)
			return
		}
		done <- v
	}()
	e.Notify("tick2", 42)
	if v := <-done; v != 42 {
		t.Fatalf("WaitCtx 负载错误: %v", v)
	}
}

// httpCap.Request 在目标不可达时返回错误（覆盖错误分支）。
func TestHTTPRequestError(t *testing.T) {
	g, _ := GrantsFromStrings([]string{"http"})
	c := New(g, testProviders())
	h, err := c.HTTP()
	if err != nil {
		t.Fatalf("HTTP 应通过: %v", err)
	}
	// 关闭的端口 + 超时 client，确保快速失败。
	_, err = h.Request("GET", "http://127.0.0.1:1/", nil, nil)
	if err == nil {
		t.Fatal("不可达地址应返回错误")
	}
}
