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

// ---- consume 直接调用：参数校验（决定 pull/push 两模是否能被正确区分） ----

// TestConsumePullMissingTopic：pull 缺 topic → fail-closed。
func TestConsumePullMissingTopic(t *testing.T) {
	s := newService()
	out := callHandler(t, s.handleConsume, map[string]interface{}{"brokers": "127.0.0.1:9092"})
	if errText(out) != "topic 必填" {
		t.Fatalf("缺 topic 应报「topic 必填」，实际 %#v", out)
	}
}

// TestConsumePullMissingBrokers：pull 缺 brokers → fail-closed。
func TestConsumePullMissingBrokers(t *testing.T) {
	s := newService()
	out := callHandler(t, s.handleConsume, map[string]interface{}{"topic": "hr"})
	if errText(out) != "brokers 必填" {
		t.Fatalf("缺 brokers 应报「brokers 必填」，实际 %#v", out)
	}
}

// TestConsumeBadStart：start 只接受 earliest|latest，未知值必须被拒（不静默）。
func TestConsumeBadStart(t *testing.T) {
	s := newService()
	out := callHandler(t, s.handleConsume, map[string]interface{}{
		"topic": "hr", "brokers": "127.0.0.1:9092", "start": "middle",
	})
	if !strings.Contains(errText(out), "未知的 start") {
		t.Fatalf("未知 start 应被拒，实际 %#v", out)
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
	out := callHandler(t, s.handleConsume, map[string]interface{}{
		"mode": "push", "topic": "hr", "brokers": "127.0.0.1:9092",
	})
	if errText(out) != "缺少 callback_url" {
		t.Fatalf("push 缺 callback_url 应报「缺少 callback_url」，实际 %#v", out)
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
	for _, want := range []string{"produce", "consume", "unsubscribe"} {
		if !got[want] {
			t.Fatalf("/caps 应含 %q，实际 %#v", want, methods)
		}
	}
}
