package engine

// service ext 的传输面（EXT_SERVICE_DESIGN 阶段三）：LocalHTTPTransport 是以「本地 HTTP +
// 长连接复用」替代「每次 amqp.Dial 重连」的通道。本文件锁死四条契约：
//
//  1. 协议形状：POST /<method>，body {"args":...}，响应 {"result"|"error"}
//     —— 与 CommandTransport 同语义、同 JSON 形状（两条路径可互换是 service ext 的设计前提）；
//  2. 连接复用：多次 Call 共用同一条 TCP 连接（DisableKeepAlives=false 的全部意义）；
//  3. Lua 路由：service ext 的 ctx.ext.* 调用**真的经 HTTP 到达侧车**（而非走子进程的
//     CommandTransport）——这是「声明 listen 的 ext 走 LocalHTTPTransport」的可观测证据；
//  4. fail-closed：侧车回 error、响应非 JSON、侧车不可达，**三种都必须是显式错误**，
//     绝不把「侧车挂了」伪装成「调用成功但结果为空」（红线：不静默降级）。

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"gear/internal/model"
)

// addrOf 把 httptest 的 URL 剥掉 scheme，得到 NewLocalHTTPTransport 需要的 host:port。
func addrOf(srv *httptest.Server) string { return strings.TrimPrefix(srv.URL, "http://") }

// resultServer 起一个「协议合规」的假侧车：把请求路径与 args 记进观测变量，回 {"result":...}。
func resultServer(t *testing.T, path *string, args *map[string]interface{}, hits *int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			atomic.AddInt32(hits, 1)
		}
		if path != nil {
			*path = r.URL.Path
		}
		var body struct {
			Args map[string]interface{} `json:"args"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("侧车侧解析请求体失败: %v", err)
		}
		if args != nil {
			*args = body.Args
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"result": map[string]interface{}{"method": strings.TrimPrefix(r.URL.Path, "/"), "pong": true},
		})
	}))
}

// TestLocalHTTPTransport_CallOK：方法名编进路径、args 装在 {"args":...} 信封里、result 原样解出。
func TestLocalHTTPTransport_CallOK(t *testing.T) {
	var path string
	var args map[string]interface{}
	srv := resultServer(t, &path, &args, nil)
	defer srv.Close()

	tr := NewLocalHTTPTransport(addrOf(srv))
	res, err := tr.Call("ping", map[string]interface{}{"n": float64(7)})
	if err != nil {
		t.Fatalf("Call 应成功: %v", err)
	}
	if path != "/ping" {
		t.Fatalf("方法名应编进路径 /ping，实际 %q", path)
	}
	if fmt.Sprint(args["n"]) != "7" {
		t.Fatalf("args 应经 {\"args\":...} 信封透传，实际 %v", args)
	}
	m, ok := res.(map[string]interface{})
	if !ok || m["pong"] != true || m["method"] != "ping" {
		t.Fatalf("result 未按侧车协议解出: %#v", res)
	}
}

// TestLocalHTTPTransport_CallError：三种失败路径都必须报错，且错误信息可操作。
func TestLocalHTTPTransport_CallError(t *testing.T) {
	// ① 侧车回 error 字段 → 报错并透出侧车原因（不得当成功）。
	errSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"error": "broker 未连接"})
	}))
	defer errSrv.Close()
	_, err := NewLocalHTTPTransport(addrOf(errSrv)).Call("produce", nil)
	if err == nil {
		t.Fatal("侧车返回 error 字段时必须报错（不得静默返回空结果）")
	}
	if !strings.Contains(err.Error(), "broker 未连接") {
		t.Fatalf("错误信息应透出侧车原因，实际: %v", err)
	}

	// ② 响应不是合法 JSON（例如中间有代理塞了 HTML 502 页）→ 报错。
	htmlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html>502 Bad Gateway</html>"))
	}))
	defer htmlSrv.Close()
	if _, err := NewLocalHTTPTransport(addrOf(htmlSrv)).Call("x", nil); err == nil {
		t.Fatal("响应非 JSON 时必须报错")
	}

	// ③ 侧车不可达（端口已释放）→ 报错且带上目标地址，便于定位是哪个侧车。
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadAddr := addrOf(dead)
	dead.Close()
	_, err = NewLocalHTTPTransport(deadAddr).Call("x", nil)
	if err == nil {
		t.Fatal("侧车不可达时必须报错（不得静默成功）")
	}
	if !strings.Contains(err.Error(), deadAddr) {
		t.Fatalf("不可达错误应带上目标地址，实际: %v", err)
	}
}

// TestLocalHTTPTransport_ConnectionReuse：5 次 Call 只应新建 1 条 TCP 连接。
// 这是 service ext 选用 LocalHTTPTransport 的**唯一理由**（消除 rabbitmq 那种每次重连），
// 所以它必须被测试钉住——否则某次改动把 DisableKeepAlives 打开也不会有人发现。
func TestLocalHTTPTransport_ConnectionReuse(t *testing.T) {
	var opened int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"result": "ok"})
	}))
	srv.Config.ConnState = func(_ net.Conn, st http.ConnState) {
		if st == http.StateNew {
			atomic.AddInt32(&opened, 1)
		}
	}
	srv.Start()
	defer srv.Close()

	tr := NewLocalHTTPTransport(addrOf(srv))
	for i := 0; i < 5; i++ {
		if _, err := tr.Call("ping", map[string]interface{}{"i": float64(i)}); err != nil {
			t.Fatalf("第 %d 次 Call 失败: %v", i, err)
		}
	}
	if n := atomic.LoadInt32(&opened); n != 1 {
		t.Fatalf("5 次调用应复用同一条连接（期望新建 1 条，实际 %d 条）", n)
	}
}

// TestLocalHTTPTransport_LuaRouting：注册一个以 LocalHTTPTransport 为载体的 service ext，
// 真引擎跑 Flow，Lua 里 ctx.ext.fake.ping 应经 HTTP 打到侧车。
// （CommandTransport 需要子进程，这里给的是个不存在的命令，能打通即证明走的是 HTTP 路径。）
func TestLocalHTTPTransport_LuaRouting(t *testing.T) {
	var hits int32
	var path string
	srv := resultServer(t, &path, nil, &hits)
	defer srv.Close()

	capReg := NewCapabilityRegistry()
	if err := capReg.Register("ext.fake", sidecarExtension("ext.fake", NewLocalHTTPTransport(addrOf(srv)), []string{"ping"})); err != nil {
		t.Fatalf("注册 ext.fake 失败: %v", err)
	}

	engReg := newTestRegistry(t)
	defer engReg.Close()
	tool := &model.Tool{
		ID: "httptool", Name: "httptool", Runtime: model.RuntimeLua,
		Entry:        `local r = ctx.ext.fake.ping({ n = 1 }); return { m = r.method }`,
		Capabilities: []string{"ext.fake"},
		InputSchema:  map[string]interface{}{"type": "object"},
		OutputSchema: map[string]interface{}{"type": "object"},
		Versions:     []model.ToolVersion{{Version: "1.0.0", State: model.StateStable}},
	}
	if err := engReg.UpsertTool(tool); err != nil {
		t.Fatalf("注册 Tool 失败: %v", err)
	}
	if err := engReg.UpsertFlow(&model.Flow{ID: "hf", Steps: []model.Step{{ID: "s1", Call: "httptool"}}}); err != nil {
		t.Fatalf("注册 Flow 失败: %v", err)
	}

	res, err := New(engReg, WithCapabilities(capReg)).RunFlowResult("hf")
	if err != nil {
		t.Fatalf("经 LocalHTTPTransport 的 ext 调用失败: %v", err)
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Fatalf("侧车应恰好收到 1 次 HTTP 调用，实际 %d 次", n)
	}
	if path != "/ping" {
		t.Fatalf("应打到 /ping，实际 %q", path)
	}
	out, ok := res.Steps["s1"]["output"].(map[string]interface{})
	if !ok || fmt.Sprint(out["m"]) != "ping" {
		t.Fatalf("Lua 未拿到侧车返回的方法名: %v", res.Steps["s1"])
	}
}

// transportOf 加载一份最小 extensions.yaml（给定 ext 段正文）并取回该 ext 的 transport，
// 供下面两条用例断言「装配到哪条传输路径」。
func transportOf(t *testing.T, name, spec string) (*CapabilityRegistry, SidecarTransport) {
	t.Helper()
	cfg := filepath.Join(t.TempDir(), "extensions.yaml")
	if err := os.WriteFile(cfg, []byte(spec), 0o644); err != nil {
		t.Fatalf("写临时配置失败: %v", err)
	}
	capReg := NewCapabilityRegistry()
	reg := newTestRegistry(t)
	t.Cleanup(func() { reg.Close() })
	if _, err := SetupExtensions(capReg, reg, cfg); err != nil {
		t.Fatalf("加载扩展配置失败: %v", err)
	}
	ext := capReg.exts[name]
	if ext == nil {
		t.Fatalf("%s 应已注册", name)
	}
	got, err := ext.Provider(nil) // 同包直接取 Provider（不 deref ctx）
	if err != nil {
		t.Fatalf("取 Provider 失败: %v", err)
	}
	sp, ok := got.(*SidecarProvider)
	if !ok {
		t.Fatalf("Provider 应返回 *SidecarProvider，实际 %T", got)
	}
	// 方法级 schema 必须同时进目录，否则静态校验会静默退化成「只校验能力名」。
	if len(capReg.schemas[name]) != 1 {
		t.Fatalf("%s 的方法 schema 应进目录，实际 %v", name, capReg.schemas[name])
	}
	return capReg, sp.t
}

// TestSidecarExtension_FallsBackToCommandTransport：**非 service** 的 ext（不声明 service/listen）
// 必须走 CommandTransport——每次调用起一个子进程的 once 语义。这条回退是
// 「service ext 那套长连接特性不改变存量 ext 行为」的保证。
//
// 注：service:true 而不声明 listen 会在装配期被 coord.NewClient 直接拒绝（fail-closed），
// 因此「service ext 缺 listen 时回退」是**不可达**路径——回退实际只发生在非 service 分支。
func TestSidecarExtension_FallsBackToCommandTransport(t *testing.T) {
	_, tr := transportOf(t, "ext.demo",
		"ext.demo:\n"+
			"  cmd: [\"echo\"]\n"+
			"  methods: [ping]\n"+
			"  schemas:\n"+
			"    ping:\n"+
			"      parameters: {}\n")
	if _, isCmd := tr.(*CommandTransport); !isCmd {
		t.Fatalf("非 service ext 应走 CommandTransport，实际 %T", tr)
	}
}

// TestServiceExtWithListenUsesLocalHTTPTransport：声明 service:true + listen 的 ext
// 必须装配「coord 保活载体 + LocalHTTPTransport」——底层仍是 keep-alive 长连接
// （service ext 的全部意义：消除 rabbitmq 那种每次重连），外层多了首次调用前的
// RegisterOrStart 自动拉起（2026-10-20：能力调用路径不再要求用户手工起 sidecar）。
func TestServiceExtWithListenUsesLocalHTTPTransport(t *testing.T) {
	_, tr := transportOf(t, "ext.svc",
		"ext.svc:\n"+
			"  cmd: [\"echo\"]\n"+
			"  methods: [ping]\n"+
			"  service: true\n"+
			"  listen: \"127.0.0.1:18099\"\n"+
			"  schemas:\n"+
			"    ping:\n"+
			"      parameters: {}\n")
	svc, isSvc := tr.(*serviceHTTPTransport)
	if !isSvc {
		t.Fatalf("service ext 声明 listen 后应走 serviceHTTPTransport（coord 保活+HTTP），实际 %T", tr)
	}
	if svc.base == nil {
		t.Fatal("底层传输应是 LocalHTTPTransport（长连接复用），实际为 nil")
	}
	if svc.cli == nil {
		t.Fatal("coord 客户端必须随装载一起构造（保活与共享复用都靠它）")
	}
}
