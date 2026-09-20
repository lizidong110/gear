package engine

import (
	"errors"
	"testing"
)

// 本文件覆盖通用事件源工厂（source.go）——此前整文件 0% 覆盖。
// 两条口径刻意各测一次：
//  1. **未知源名在装配期就拒**（fail-closed）：宁可起服务时暴露 source 名拼错，
//     也不要拖到业务 Flow 跑到一半时 ctx.subscribe.start 才失败；
//  2. **引擎生成的 handle/callback_url 后写、覆盖用户配置里的同名键**——否则用户
//     配置里一个手滑的 handle 会顶掉真实句柄，订阅再也回收不掉。

// stubTransport 记录侧车收到的 RPC 调用。
type stubTransport struct {
	calls    []string
	gotArgs  []map[string]interface{}
	failOn   string
	failWith error
}

func (s *stubTransport) Call(method string, args map[string]interface{}) (interface{}, error) {
	s.calls = append(s.calls, method)
	s.gotArgs = append(s.gotArgs, args)
	if s.failOn != "" && s.failOn == method {
		return nil, s.failWith
	}
	return map[string]interface{}{"ok": true}, nil
}

func TestNewSourceFactoryRoutesKnownSources(t *testing.T) {
	tst := &stubTransport{}
	for _, name := range []string{"mqtt", "kafka", "rabbitmq", "webhook"} {
		f, err := newSourceFactory(name, tst)
		if err != nil || f == nil {
			t.Fatalf("已实现的事件源 %q 应能构造工厂，实际 (%v, %v)", name, f, err)
		}
	}
	if _, err := newSourceFactory("bogus", tst); err == nil {
		t.Fatal("未知事件源必须报错（装配期 fail-closed）")
	}
	if _, err := newSourceFactory("", tst); err == nil {
		t.Fatal("空源名必须报错")
	}
}

func TestSidecarSourceStartSendsSubscribeAndStopSendsUnsubscribe(t *testing.T) {
	tst := &stubTransport{}
	f, err := newSourceFactory("kafka", tst)
	if err != nil {
		t.Fatalf("构造工厂失败: %v", err)
	}
	src, err := f(map[string]interface{}{"topic": "t1", "group": "g"}, "http://cb/h1", "h1")
	if err != nil {
		t.Fatalf("构造事件源失败: %v", err)
	}
	stop, err := src.Start(nil)
	if err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	if len(tst.calls) != 1 || tst.calls[0] != "subscribe" {
		t.Fatalf("Start 应发一次 subscribe，实际 %v", tst.calls)
	}
	args := tst.gotArgs[0]
	if args["topic"] != "t1" || args["group"] != "g" {
		t.Fatalf("源专属配置应原样透传（引擎不维护第二份字段清单），实际 %v", args)
	}
	if args["handle"] != "h1" || args["callback_url"] != "http://cb/h1" {
		t.Fatalf("handle/callback_url 应一并发给侧车，实际 %v", args)
	}
	stop()
	if len(tst.calls) != 2 || tst.calls[1] != "unsubscribe" {
		t.Fatalf("stop 应发一次 unsubscribe，实际 %v", tst.calls)
	}
	if tst.gotArgs[1]["handle"] != "h1" {
		t.Fatalf("unsubscribe 应带句柄，实际 %v", tst.gotArgs[1])
	}
}

func TestSidecarSourceEngineKeysOverrideUserConfig(t *testing.T) {
	// 用户配置里塞了同名的 handle/callback_url：必须以引擎生成的为准。
	tst := &stubTransport{}
	f, _ := newSourceFactory("rabbitmq", tst)
	src, _ := f(map[string]interface{}{"queue": "q", "handle": "用户乱写的", "callback_url": "http://evil"},
		"http://real/h9", "h9")
	if _, err := src.Start(nil); err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	args := tst.gotArgs[0]
	if args["handle"] != "h9" || args["callback_url"] != "http://real/h9" {
		t.Fatalf("引擎生成的 handle/callback_url 必须覆盖用户配置里的同名键，实际 %v", args)
	}
	if args["queue"] != "q" {
		t.Fatalf("非同名键应保留，实际 %v", args)
	}
}

func TestSidecarSourceFailsClosed(t *testing.T) {
	// 未装配 transport（该 ext 未声明 listen）→ 显式报错，不静默「订阅成功」。
	f, _ := newSourceFactory("webhook", nil)
	src, _ := f(nil, "http://cb/h", "h")
	if _, err := src.Start(nil); err == nil {
		t.Fatal("未装配 sidecar transport 时必须报错")
	}

	// 侧车 subscribe 失败 → 上抛（不假成功：订阅没建立却返回句柄，事件永远不来）
	boom := errors.New("侧车拒绝订阅")
	tst := &stubTransport{failOn: "subscribe", failWith: boom}
	f2, _ := newSourceFactory("kafka", tst)
	src2, _ := f2(map[string]interface{}{"topic": "t"}, "http://cb/h", "h")
	if _, err := src2.Start(nil); err == nil {
		t.Fatal("subscribe 失败必须上抛（不静默假成功）")
	}

	// unsubscribe 失败无法补救 → 忽略（不因清理失败而 panic）
	tst2 := &stubTransport{failOn: "unsubscribe", failWith: boom}
	f3, _ := newSourceFactory("kafka", tst2)
	src3, _ := f3(nil, "http://cb/h", "h")
	stop, err := src3.Start(nil)
	if err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	stop()
}
