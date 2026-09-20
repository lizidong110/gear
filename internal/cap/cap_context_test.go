package cap

import (
	"errors"
	"testing"
)

// 本文件覆盖 Context 的「视图注入」面：扩展原语注入、调用链透传、订阅归属绑定、
// 以及 scopedKV 的 Delete 路由。这些 setter 单独调用没有可观测后果，故一律断言
// **下游拿到的值**（Resolver 收到的 via、Subscribe.Start 收到的 Owner），
// 而不是「调过就算覆盖」。

// fakeSub 记录 Start 收到的请求，供断言 owner 是否真的被写回。
type fakeSub struct {
	startErr error
	stopErr  error
	gotReq   SubscriptionRequest
	stopped  string
}

func (f *fakeSub) Start(req SubscriptionRequest) (string, error) {
	f.gotReq = req
	if f.startErr != nil {
		return "", f.startErr
	}
	return "h1", nil
}

func (f *fakeSub) Stop(handle string) error {
	f.stopped = handle
	return f.stopErr
}

func TestContextExtensionInjection(t *testing.T) {
	c := New(Grants{PrimKV: true}, &Providers{})
	// 未声明该扩展名 → 拒绝（最小权限）
	if _, err := c.Extension("ext.db"); err != ErrCapabilityDenied {
		t.Fatalf("未声明的扩展名应 ErrCapabilityDenied，实际 %v", err)
	}
	// 已声明但引擎未注入实现 → unavailable（「文档说有、代码无」变成可见错误）
	c2 := New(Grants{"ext.db": true}, &Providers{})
	if _, err := c2.Extension("ext.db"); err != ErrCapabilityUnavailable {
		t.Fatalf("已声明未注入应 ErrCapabilityUnavailable，实际 %v", err)
	}
	// 注入后取回原对象
	c2.SetExtension("ext.db", "impl-object")
	got, err := c2.Extension("ext.db")
	if err != nil || got != "impl-object" {
		t.Fatalf("注入后应取回原对象，实际 (%v, %v)", got, err)
	}
}

func TestContextSetCallChainIsPassedToResolver(t *testing.T) {
	var gotVia []string
	prov := &Providers{Resolver: func(target string, input map[string]interface{}, via []string) (map[string]interface{}, error) {
		gotVia = via
		return map[string]interface{}{"ok": true}, nil
	}}
	c := New(Grants{PrimCall: true}, prov)
	c.SetCallChain([]string{"flowA", "flowB"})
	call, err := c.Call()
	if err != nil {
		t.Fatalf("已授权且有 Resolver 时 Call 不该报错：%v", err)
	}
	if _, err := call.Call("toolX", nil); err != nil {
		t.Fatalf("Call 失败：%v", err)
	}
	// 调用链必须原样透传给 Resolver，供引擎做 A→B→A 循环调用守卫
	if len(gotVia) != 2 || gotVia[0] != "flowA" || gotVia[1] != "flowB" {
		t.Fatalf("via 应原样透传，实际 %v", gotVia)
	}
}

func TestContextSubscribeBindingWritesOwner(t *testing.T) {
	sub := &fakeSub{}
	prov := &Providers{Subscribe: sub}
	c := New(Grants{PrimSubscribe: true}, prov)
	c.SetSubscriptionOwner(SubscriptionOwner{FlowID: "f1", ExecID: "e1", StepID: "s1"})

	s, err := c.Subscribe()
	if err != nil {
		t.Fatalf("已授权且有实现时 Subscribe 不该报错：%v", err)
	}
	handle, err := s.Start(SubscriptionRequest{Source: "mqtt"})
	if err != nil || handle != "h1" {
		t.Fatalf("Start 应返回句柄，实际 (%q, %v)", handle, err)
	}
	if sub.gotReq.Owner != (SubscriptionOwner{FlowID: "f1", ExecID: "e1", StepID: "s1"}) {
		t.Fatalf("Start 请求必须带上本次执行的 owner（供运行结束回收订阅），实际 %+v", sub.gotReq.Owner)
	}
	if err := s.Stop("h1"); err != nil {
		t.Fatalf("Stop 失败：%v", err)
	}
	if sub.stopped != "h1" {
		t.Fatalf("Stop 应把句柄透传下去，实际 %q", sub.stopped)
	}

	// 未授权 → denied；已授权但未装配实现 → unavailable（fail-closed，绝不 no-op）
	if _, err := New(Grants{}, prov).Subscribe(); err != ErrCapabilityDenied {
		t.Fatalf("未声明 subscribe 应 ErrCapabilityDenied，实际 %v", err)
	}
	if _, err := New(Grants{PrimSubscribe: true}, &Providers{}).Subscribe(); err != ErrCapabilityUnavailable {
		t.Fatalf("未装配实现应 ErrCapabilityUnavailable，实际 %v", err)
	}
}

func TestContextSubscribePropagatesStartError(t *testing.T) {
	boom := errors.New("无 server 监听，一次性执行不支持消费回调")
	sub := &fakeSub{startErr: boom}
	c := New(Grants{PrimSubscribe: true}, &Providers{Subscribe: sub})
	s, _ := c.Subscribe()
	if _, err := s.Start(SubscriptionRequest{Source: "mqtt"}); !errors.Is(err, boom) {
		t.Fatalf("Start 的错误必须原样上抛（不静默 no-op），实际 %v", err)
	}
	sub.stopErr = boom
	if err := s.Stop("h"); !errors.Is(err, boom) {
		t.Fatalf("Stop 的错误必须原样上抛，实际 %v", err)
	}
}

func TestScopedKVDeleteRoutesByScope(t *testing.T) {
	sessions := NewMemSessionRegistry()
	global := NewMemKV()
	c := New(Grants{PrimKV: true}, &Providers{KV: global, Sessions: sessions})
	c.SetSession("flow-1")
	kv, err := c.KV()
	if err != nil {
		t.Fatalf("KV 不该报错：%v", err)
	}

	kv.Put("g", "gv", ScopeGlobal)
	kv.Put("s", "sv", ScopeSession)
	// 两个命名空间隔离
	if v, ok := sessions.Session("flow-1").Get("s", ScopeSession); !ok || v != "sv" {
		t.Fatalf("session 键应落在 session 命名空间，实际 (%q,%v)", v, ok)
	}
	if _, ok := global.Get("s", ScopeGlobal); ok {
		t.Fatal("session 键不得污染 global 命名空间")
	}

	kv.Delete("s", ScopeSession)
	if _, ok := sessions.Session("flow-1").Get("s", ScopeSession); ok {
		t.Fatal("scopedKV.Delete(ScopeSession) 应删掉会话键")
	}
	kv.Delete("g", ScopeGlobal)
	if _, ok := global.Get("g", ScopeGlobal); ok {
		t.Fatal("scopedKV.Delete(ScopeGlobal) 应删掉全局键")
	}
}

func TestSessionRegistryRefCountRecycles(t *testing.T) {
	// 引用计数归零才回收：同一 flow 的并发运行互不干扰。
	r := NewMemSessionRegistry()
	r.Acquire("f1")
	r.Acquire("f1")
	r.Session("f1").Put("k", "v", ScopeSession)
	r.Release("f1")
	if _, ok := r.Session("f1").Get("k", ScopeSession); !ok {
		t.Fatal("引用计数未归零时不得回收命名空间")
	}
	r.Release("f1")
	if _, ok := r.Session("f1").Get("k", ScopeSession); ok {
		t.Fatal("引用计数归零后应回收该命名空间（内存态，重启即失）")
	}
}
