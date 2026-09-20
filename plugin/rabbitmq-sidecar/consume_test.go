package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// callHandler 以 Gear 的 RPC 线格式（body = {"args": {...}}）调用一个 handler，返回解析后的 JSON。
func callHandler(t *testing.T, h http.HandlerFunc, args map[string]interface{}) map[string]interface{} {
	t.Helper()
	body, _ := json.Marshal(map[string]interface{}{"args": args})
	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(string(body)))
	w := httptest.NewRecorder()
	h(w, req)
	var out map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("handler 返回非 JSON: %v（body=%q）", err, w.Body.String())
	}
	return out
}

func errText(out map[string]interface{}) string {
	if s, ok := out["error"].(string); ok {
		return s
	}
	return ""
}

// ---- consume 直接调用：参数校验（pull/push 两模） ----

// TestConsumePullMissingQueue：pull 缺 queue → fail-closed。
func TestConsumePullMissingQueue(t *testing.T) {
	s := newService()
	out := callHandler(t, s.handleConsume, map[string]interface{}{})
	if !strings.Contains(errText(out), "缺少 queue") {
		t.Fatalf("缺 queue 应报「缺少 queue」，实际 %#v", out)
	}
}

// TestConsumeUnknownMode：mode 只接受 pull|push。
func TestConsumeUnknownMode(t *testing.T) {
	s := newService()
	out := callHandler(t, s.handleConsume, map[string]interface{}{"mode": "peek"})
	if !strings.Contains(errText(out), "未知的 consume mode") {
		t.Fatalf("未知 mode 应被拒，实际 %#v", out)
	}
}

// TestConsumePushMissingCallback：push 模式缺 callback_url → fail-closed（handle 会自动生成）。
func TestConsumePushMissingCallback(t *testing.T) {
	s := newService()
	out := callHandler(t, s.handleConsume, map[string]interface{}{"mode": "push", "queue": "q"})
	if errText(out) != "缺少 callback_url" {
		t.Fatalf("push 缺 callback_url 应报「缺少 callback_url」，实际 %#v", out)
	}
}

// TestConsumePullConnectFail：broker 连不上时 pull 必须 fail-closed（端口 1 必拒）。
func TestConsumePullConnectFail(t *testing.T) {
	s := newService()
	out := callHandler(t, s.handleConsume, map[string]interface{}{
		"queue": "q", "host": "127.0.0.1", "port": 1,
	})
	if !strings.Contains(errText(out), "连接失败") {
		t.Fatalf("连不上 broker 应报「连接失败」，实际 %#v", out)
	}
}

// TestCapsListsImplementedMethods：/caps 暴露已实现方法（供「声明 ↔ 实现」红线校验）。
func TestCapsListsImplementedMethods(t *testing.T) {
	s := newService()
	out := callHandler(t, s.handleCaps, nil)
	res, ok := out["result"].(map[string]interface{})
	if !ok {
		t.Fatalf("/caps 应返回 result，实际 %#v", out)
	}
	methods, _ := res["methods"].([]interface{})
	got := map[string]bool{}
	for _, m := range methods {
		got[m.(string)] = true
	}
	for _, want := range []string{"publish", "declare", "consume", "unsubscribe"} {
		if !got[want] {
			t.Fatalf("/caps 应含 %q，实际 %#v", want, methods)
		}
	}
}
