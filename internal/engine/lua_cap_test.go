package engine

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"gear/internal/cap"
	"gear/internal/model"
)

// 构造带指定 capability 与实现的宿主能力视图，供 lua 测试注入 ctx.*。
func luaHost(t *testing.T, caps []string, prov *cap.Providers) *cap.Context {
	t.Helper()
	g, err := cap.GrantsFromStrings(caps)
	if err != nil {
		t.Fatalf("非法 capability: %v", err)
	}
	return cap.New(g, prov)
}

// luaRunHost 以给定宿主能力视图运行一段 Lua 源码。
func luaRunHost(t *testing.T, entry string, input map[string]interface{}, host *cap.Context) (map[string]interface{}, error) {
	t.Helper()
	rt, err := runtimeFor(model.RuntimeLua, "")
	if err != nil {
		t.Fatalf("lua runtime 应可用: %v", err)
	}
	return rt.Run(context.Background(), entry, input, host)
}

// M5-lua-1：未声明 capability 时，ctx.kv 根本不存在，脚本访问即报错（最小权限 + 防御纵深）。
func TestLuaCapUngranted(t *testing.T) {
	host := luaHost(t, nil, &cap.Providers{}) // 无任何 capability
	_, err := luaRunHost(t, `return ctx.kv.get("x")`, nil, host)
	if err == nil {
		t.Fatal("未声明 kv 时访问 ctx.kv 应报错")
	}
}

// M5-lua-2：声明 kv 后，put/get 正常往返（宿主侧 MemKV 实现）。
func TestLuaCapKV(t *testing.T) {
	prov := &cap.Providers{KV: cap.NewMemKV()}
	host := luaHost(t, []string{"kv"}, prov)
	out, err := luaRunHost(t, `
		ctx.kv.put("k", "v")
		return { got = ctx.kv.get("k") }
	`, nil, host)
	if err != nil {
		t.Fatalf("kv 读写失败: %v", err)
	}
	if fmt.Sprint(out["got"]) != "v" {
		t.Fatalf("kv 往返异常: %v", out)
	}
}

// M5-lua-3：secret 只读读取；缺失的密钥返回错误。
func TestLuaCapSecret(t *testing.T) {
	prov := &cap.Providers{Secret: cap.MapSecret{"TOKEN": "abc"}}
	host := luaHost(t, []string{"secret"}, prov)
	out, err := luaRunHost(t, `return { t = ctx.secret.get("TOKEN") }`, nil, host)
	if err != nil {
		t.Fatalf("secret 读取失败: %v", err)
	}
	if fmt.Sprint(out["t"]) != "abc" {
		t.Fatalf("secret 读取异常: %v", out)
	}

	// 缺失密钥应报错
	if _, err := luaRunHost(t, `return ctx.secret.get("NOPE")`, nil, host); err == nil {
		t.Fatal("读取不存在的 secret 应报错")
	}
}

// M5-lua-4：http 原语通过 httptest 服务做真实请求。
func TestLuaCapHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	prov := &cap.Providers{HTTP: srv.Client()}
	host := luaHost(t, []string{"http"}, prov)
	out, err := luaRunHost(t, fmt.Sprintf(`
		local r = ctx.http.Request("GET", %q, nil, nil)
		return { status = r.status, body = r.body }
	`, srv.URL), nil, host)
	if err != nil {
		t.Fatalf("http 请求失败: %v", err)
	}
	if fmt.Sprint(out["status"]) != "200" {
		t.Fatalf("http 状态码异常: %v", out)
	}
	if fmt.Sprint(out["body"]) != `{"ok":true}` {
		t.Fatalf("http body 异常: %v", out)
	}
}

// M5-lua-5：call 原语调用 Resolver（插件间唯一通信通道）。
func TestLuaCapCall(t *testing.T) {
	prov := &cap.Providers{Resolver: func(target string, input map[string]interface{}, via []string) (map[string]interface{}, error) {
		return map[string]interface{}{"echo": target}, nil
	}}
	host := luaHost(t, []string{"call"}, prov)
	out, err := luaRunHost(t, `local r = ctx.call("other", {a=1}); return { echo = r.echo }`, nil, host)
	if err != nil {
		t.Fatalf("call 失败: %v", err)
	}
	if fmt.Sprint(out["echo"]) != "other" {
		t.Fatalf("call 返回异常: %v", out)
	}
}

// M5-lua-6：event 原语 notify/wait 同步往返。
func TestLuaCapEvent(t *testing.T) {
	prov := &cap.Providers{Event: cap.NewMemEvent()}
	host := luaHost(t, []string{"event"}, prov)
	out, err := luaRunHost(t, `
		ctx.event.notify("e", { n = 1 })
		local p = ctx.event.wait("e")
		return { n = p.n }
	`, nil, host)
	if err != nil {
		t.Fatalf("event 往返失败: %v", err)
	}
	if fmt.Sprint(out["n"]) != "1" {
		t.Fatalf("event 往返异常: %v", out)
	}
}

// M5-lua-7：db 原语已声明但未提供实现，调用返回 ErrCapabilityUnavailable（明确错误）。
func TestLuaCapDBUnavailable(t *testing.T) {
	prov := &cap.Providers{} // DB 为 nil
	host := luaHost(t, []string{"db"}, prov)
	_, err := luaRunHost(t, `return ctx.db.query("SELECT 1")`, nil, host)
	if err == nil {
		t.Fatal("未提供 DB 实现时调用 ctx.db 应报错")
	}
	if !containsSubstr(err.Error(), "尚未实现") {
		t.Fatalf("db 错误应说明尚未实现，实际: %v", err)
	}
}

// M5-lua-8：声明隔离——仅声明 kv 时访问 ctx.http 报错（不暴露未声明原语）。
func TestLuaCapIsolation(t *testing.T) {
	prov := &cap.Providers{KV: cap.NewMemKV()}
	host := luaHost(t, []string{"kv"}, prov) // 仅 kv
	_, err := luaRunHost(t, `return ctx.http.Request("GET", "http://x", nil, nil)`, nil, host)
	if err == nil {
		t.Fatal("仅声明 kv 时访问 ctx.http 应报错")
	}
}
