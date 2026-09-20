package main

// mqtt-sidecar 业务核心的契约测试（EXT_SERVICE_DESIGN 阶段三）。
//
// 原 `internal/sidecar/mqttsvc/` 整包（实现 + 单测）在整树重建中丢失，实现已内聚到本文件所在包，
// 但单测**没有随之恢复**——即「service ext 的真实样板」长期无测试背书。本文件把它补回来。
//
// 之所以能纯单测：核心层刻意只依赖 `MQTTClient` 接口（不依赖 paho），测试注入 fake 即可，
// 无需真实 broker。三条关键语义在这里钉死：
//   1. **topic 计数去重**：多个 handle 订阅同一 topic 时只真正 MQTT Subscribe 一次，
//      计数归零才真 Unsubscribe——否则同 topic 的第二个订阅者会重复收到消息；
//   2. **失败不留脏状态**：Subscribe 失败必须回滚刚登记的 handle 与计数；
//   3. **回推失败不连累其它 handle**：一个 callbackURL 不可达，其余 handle 仍要收到消息。

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeMQTT 是 MQTTClient 的测试替身：记录调用、可注入错误，订阅可手动触发 onMessage。
type fakeMQTT struct {
	mu        sync.Mutex
	published []pubCall
	subs      map[string]func(topic string, payload []byte) // topic -> onMessage
	unsubs    []string
	pubErr    error
	subErr    error
}

type pubCall struct {
	topic   string
	qos     byte
	payload string
}

func newFakeMQTT() *fakeMQTT {
	return &fakeMQTT{subs: map[string]func(topic string, payload []byte){}}
}

func (f *fakeMQTT) Publish(topic string, qos byte, payload []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pubErr != nil {
		return f.pubErr
	}
	f.published = append(f.published, pubCall{topic: topic, qos: qos, payload: string(payload)})
	return nil
}

func (f *fakeMQTT) Subscribe(topic string, qos byte, onMessage func(topic string, payload []byte)) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.subErr != nil {
		return f.subErr
	}
	// 真实 MQTT 的 Subscribe 会携带 qos，这里记录 qos 便于断言透传。
	f.subs[topic] = onMessage
	f.published = append(f.published, pubCall{topic: "__subscribe__:" + topic, qos: qos})
	return nil
}

func (f *fakeMQTT) Unsubscribe(topic string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unsubs = append(f.unsubs, topic)
	delete(f.subs, topic)
	return nil
}

func (f *fakeMQTT) IsConnected() bool       { return true }
func (f *fakeMQTT) Disconnect(quiesce uint) {}

// subCount 返回「真正的 MQTT Subscribe 次数」（fake 用带前缀的伪记录计数）。
func (f *fakeMQTT) subCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, p := range f.published {
		if strings.HasPrefix(p.topic, "__subscribe__:") {
			n++
		}
	}
	return n
}

func (f *fakeMQTT) unsubCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.unsubs)
}

// postJSON 把一个 JSON body 打给 handler，返回响应码与解析后的 body。
func postJSON(t *testing.T, h http.HandlerFunc, body string) (int, map[string]interface{}) {
	t.Helper()
	req := httptest.NewRequest("POST", "/", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h(rec, req)
	var out map[string]interface{}
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
	}
	return rec.Code, out
}

// TestPublish：publish 端点把 topic/qos/payload 透传给 MQTT 客户端；客户端报错则回 error。
func TestPublish(t *testing.T) {
	f := newFakeMQTT()
	s := New(f)

	code, out := postJSON(t, s.handlePublish, `{"args":{"topic":"a/b","payload":"hi","qos":1}}`)
	if code != http.StatusOK {
		t.Fatalf("publish 期望 200，实际 %d", code)
	}
	res, _ := out["result"].(map[string]interface{})
	if res["ok"] != true {
		t.Fatalf("publish 应回 {result:{ok:true}}，实际 %v", out)
	}
	if len(f.published) != 1 {
		t.Fatalf("应恰好发布 1 条，实际 %d", len(f.published))
	}
	if got := f.published[0]; got.topic != "a/b" || got.qos != 1 || got.payload != "hi" {
		t.Fatalf("发布参数未正确透传: %+v", got)
	}

	// 客户端报错 → 响应带 error（不得伪装成功）。
	f.pubErr = errors.New("broker 未连接")
	_, out = postJSON(t, s.handlePublish, `{"args":{"topic":"a/b","payload":"x"}}`)
	if msg, _ := out["error"].(string); msg == "" {
		t.Fatalf("publish 失败应回 error，实际 %v", out)
	}

	// 坏 JSON → 400。
	if code, _ := postJSON(t, s.handlePublish, `{not json`); code != http.StatusBadRequest {
		t.Fatalf("坏 JSON 期望 400，实际 %d", code)
	}
}

// TestSubscribe：登记 handle + topic 计数去重（同 topic 只真订阅一次）+ 失败回滚。
func TestSubscribe(t *testing.T) {
	f := newFakeMQTT()
	s := New(f)

	// 首次订阅：真正向 broker Subscribe。
	code, out := postJSON(t, s.handleSubscribe,
		`{"args":{"topic":"a/+","qos":1,"handle":"h1","callback_url":"http://127.0.0.1:1/x"}}`)
	if code != http.StatusOK {
		t.Fatalf("subscribe 期望 200，实际 %d", code)
	}
	res, _ := out["result"].(map[string]interface{})
	if res["handle"] != "h1" || res["topic"] != "a/+" {
		t.Fatalf("subscribe 应回 handle/topic，实际 %v", out)
	}
	if n := f.subCount(); n != 1 {
		t.Fatalf("首次订阅应真订阅 1 次，实际 %d", n)
	}

	// 同 topic 的第二个 handle：复用已有订阅（**不得**重复 MQTT Subscribe）。
	if _, out = postJSON(t, s.handleSubscribe,
		`{"args":{"topic":"a/+","qos":1,"handle":"h2","callback_url":"http://127.0.0.1:1/y"}}`); out["error"] != nil {
		t.Fatalf("第二个 handle 订阅同 topic 应成功，实际 %v", out)
	}
	if n := f.subCount(); n != 1 {
		t.Fatalf("同 topic 二次订阅应去重（仍 1 次），实际 %d", n)
	}

	// 缺 handle / 缺 callback_url → error（不静默登记半个条目）。
	if _, out = postJSON(t, s.handleSubscribe, `{"args":{"topic":"t","callback_url":"http://x"}}`); out["error"] == nil {
		t.Fatalf("缺 handle 应报错，实际 %v", out)
	}
	if _, out = postJSON(t, s.handleSubscribe, `{"args":{"topic":"t","handle":"h3"}}`); out["error"] == nil {
		t.Fatalf("缺 callback_url 应报错，实际 %v", out)
	}

	// Subscribe 失败 → 回滚刚登记的 handle（不留脏状态，否则后来者以为已订阅）。
	f.subErr = errors.New("broker 未连接")
	if _, out = postJSON(t, s.handleSubscribe,
		`{"args":{"topic":"fresh","handle":"h4","callback_url":"http://x"}}`); out["error"] == nil {
		t.Fatalf("Subscribe 失败应回 error，实际 %v", out)
	}
	s.mu.Lock()
	_, leaked := s.handles["h4"]
	_, topicLeaked := s.topics["fresh"]
	s.mu.Unlock()
	if leaked || topicLeaked {
		t.Fatalf("Subscribe 失败后应回滚 handle 与 topic 计数（handle=%v topic=%v）", leaked, topicLeaked)
	}
}

// TestUnsubscribe：计数归零才真正取消 MQTT 订阅；未知 handle 幂等成功。
func TestUnsubscribe(t *testing.T) {
	f := newFakeMQTT()
	s := New(f)

	for _, h := range []string{"h1", "h2"} {
		postJSON(t, s.handleSubscribe,
			`{"args":{"topic":"a/b","handle":"`+h+`","callback_url":"http://127.0.0.1:1/x"}}`)
	}

	// 取消 h1：仍剩 h2 → 不得真取消。
	postJSON(t, s.handleUnsubscribe, `{"args":{"handle":"h1"}}`)
	if n := f.unsubCount(); n != 0 {
		t.Fatalf("仍有 handle 订阅时不得真取消，实际 %d 次", n)
	}
	// 取消 h2：计数归零 → 真取消。
	postJSON(t, s.handleUnsubscribe, `{"args":{"handle":"h2"}}`)
	if n := f.unsubCount(); n != 1 {
		t.Fatalf("计数归零后应真取消 1 次，实际 %d", n)
	}

	// 未知 handle：幂等成功（不报错，避免 gear 回收订阅时因重复取消而卡住）。
	code, out := postJSON(t, s.handleUnsubscribe, `{"args":{"handle":"ghost"}}`)
	if code != http.StatusOK {
		t.Fatalf("未知 handle 期望 200，实际 %d", code)
	}
	res, _ := out["result"].(map[string]interface{})
	if res["ok"] != true {
		t.Fatalf("未知 handle 应回 ok（幂等），实际 %v", out)
	}
	// 缺 handle → error。
	if _, out = postJSON(t, s.handleUnsubscribe, `{"args":{}}`); out["error"] == nil {
		t.Fatalf("缺 handle 应报错，实际 %v", out)
	}
}

// TestOnMessageWebhook：收到消息时按通配符匹配 handle，POST 回推 handle/topic/payload。
func TestOnMessageWebhook(t *testing.T) {
	type push struct {
		Handle  string `json:"handle"`
		Topic   string `json:"topic"`
		Payload string `json:"payload"`
	}
	var (
		mu    sync.Mutex
		got   []push
		paths []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p push
		_ = json.NewDecoder(r.Body).Decode(&p)
		mu.Lock()
		got = append(got, p)
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	f := newFakeMQTT()
	s := New(f)
	postJSON(t, s.handleSubscribe,
		`{"args":{"topic":"a/+","handle":"h1","callback_url":"`+srv.URL+`/api/events/h1"}}`)

	// 匹配的 topic → 回推。
	f.mu.Lock()
	onMsg := f.subs["a/+"]
	f.mu.Unlock()
	if onMsg == nil {
		t.Fatal("fake 未记录 onMessage 回调")
	}
	onMsg("a/1", []byte("hello"))

	// 不匹配的 topic → 不回推。
	onMsg("b/1", []byte("nope"))

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("只有匹配 topic 应回推，实际 %d 条", len(got))
	}
	if got[0].Handle != "h1" || got[0].Topic != "a/1" || got[0].Payload != "hello" {
		t.Fatalf("回推内容不符: %+v", got[0])
	}
	if paths[0] != "/api/events/h1" {
		t.Fatalf("应 POST 到该 handle 的 callbackURL，实际 %q", paths[0])
	}
}

// TestOnMessageFailureDoesNotAbortOthers：一条 callbackURL 不可达，不得连累其它 handle。
func TestOnMessageFailureDoesNotAbortOthers(t *testing.T) {
	var mu sync.Mutex
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	f := newFakeMQTT()
	s := New(f)
	// dead 指向一个已释放端口（回推必失败）。
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	postJSON(t, s.handleSubscribe, `{"args":{"topic":"t","handle":"dead","callback_url":"`+deadURL+`"}}`)
	postJSON(t, s.handleSubscribe, `{"args":{"topic":"t","handle":"live","callback_url":"`+srv.URL+`"}}`)

	f.mu.Lock()
	onMsg := f.subs["t"]
	f.mu.Unlock()
	onMsg("t", []byte("x"))

	mu.Lock()
	defer mu.Unlock()
	if hits != 1 {
		t.Fatalf("不可达的 handle 不得中断循环：可用的那个应收到 1 次，实际 %d", hits)
	}
}

// TestTopicMatches：MQTT 通配符语义（`+` 单层、`#` 多层且只能在末段）。
func TestTopicMatches(t *testing.T) {
	cases := []struct {
		pattern, topic string
		want           bool
	}{
		{"a/b", "a/b", true},
		{"a/b", "a/c", false},
		{"a/+", "a/b", true},
		{"a/+", "a/b/c", false},
		{"a/#", "a/b/c", true},
		{"a/#", "a", true}, // '#' 匹配零层
		{"#", "any/thing", true},
		{"+/b", "a/b", true},
		{"a/+/c", "a/b/d", false},
		{"a/b", "a/b/c", false},
	}
	for _, c := range cases {
		if got := topicMatches(c.pattern, c.topic); got != c.want {
			t.Fatalf("topicMatches(%q, %q) = %v，期望 %v", c.pattern, c.topic, got, c.want)
		}
	}
}
