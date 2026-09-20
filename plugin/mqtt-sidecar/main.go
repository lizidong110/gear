// Command mqtt-sidecar 是「常驻 ext（service ext）」的首个真实样板（EXT_SERVICE_DESIGN.md §3/§12）。
//
// 它把一条 MQTT 长连接经 Gear 提供的 coord 包暴露为本地侧车：
//   - 绑定声明端口（127.0.0.1 only），对外暴露业务 RPC 端点（/publish /subscribe）；
//   - 维护占用列表（各 gear 的 PID + token），定期巡检，PID 已死或心跳超时即移除；
//   - 占用列表空 → 自动退出（无人需要该 MQTT 桥接服务）；
//   - 端口被占用（EADDRINUSE）→ 退让退出（其 owner gear 会改以 client 复用本实例）。
//
// 环境变量：MQTT_BROKER（默认 tcp://test.mosquitto.org:1883）、COORD_ADDR（默认 127.0.0.1:1884）。
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"gear/internal/sidecar/coord"
)

// ---------------------------------------------------------------------------
// MQTT 业务核心（原 internal/sidecar/mqttsvc，已内聚到本侧车）
// ---------------------------------------------------------------------------

// MQTTClient 是「MQTT 长连接」的最小抽象。真实实现由 pahoClient 适配，
// 测试可注入 fake（无需真实 broker）。
type MQTTClient interface {
	Publish(topic string, qos byte, payload []byte) error
	Subscribe(topic string, qos byte, onMessage func(topic string, payload []byte)) error
	Unsubscribe(topic string) error
	IsConnected() bool
	Disconnect(quiesce uint)
}

// subEntry 是一条订阅登记：handle 由 gear 分配（唯一句柄），callbackURL 是该 handle 的回推地址。
// 多个 handle 可能订阅同一 topic（topic 计数去重，仅首次真正 MQTT Subscribe）。
type subEntry struct {
	topic       string
	qos         byte
	callbackURL string
	handle      string
}

// Service 是 mqtt-sidecar 的业务核心。
type Service struct {
	cli MQTTClient

	mu      sync.Mutex
	handles map[string]subEntry // handle -> 订阅条目
	topics  map[string]int      // topic -> 订阅计数（去重，计数归零才真正 Unsubscribe）
}

// New 构造 Service。
func New(cli MQTTClient) *Service {
	return &Service{cli: cli, handles: map[string]subEntry{}, topics: map[string]int{}}
}

// Handlers 返回业务 RPC 端点（coord 的 Serve 会把它们与 /register 等协调端点合并到同一 mux）。
func (s *Service) Handlers() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"/publish":     s.handlePublish,
		"/subscribe":   s.handleSubscribe,
		"/unsubscribe": s.handleUnsubscribe,
	}
}

// Stop 平滑停止：断开 MQTT 长连接（占用列表空时由 coord 的 OnEmpty 调用）。
func (s *Service) Stop() {
	s.cli.Disconnect(250)
}

type pubReq struct {
	Args struct {
		Topic   string `json:"topic"`
		Payload string `json:"payload"`
		Qos     int    `json:"qos"`
	} `json:"args"`
}

// handlePublish 经 MQTT 长连接发布一条消息（Gear 经 LocalHTTPTransport 调用）。
func (s *Service) handlePublish(w http.ResponseWriter, r *http.Request) {
	var req pubReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var qos byte
	if req.Args.Qos > 0 {
		qos = byte(req.Args.Qos)
	}
	if err := s.cli.Publish(req.Args.Topic, qos, []byte(req.Args.Payload)); err != nil {
		writeJSON(w, map[string]interface{}{"error": err.Error()})
		return
	}
	writeJSON(w, map[string]interface{}{"result": map[string]interface{}{"ok": true}})
}

type subReq struct {
	Args struct {
		Topic       string `json:"topic"`
		Qos         int    `json:"qos"`
		Handle      string `json:"handle"`
		CallbackURL string `json:"callback_url"`
	} `json:"args"`
}

// handleSubscribe 登记一条订阅（handle 由 gear 分配并随请求传入；真实收消息走 onMessage 回推
// 到 callback_url）。同一 topic 只真正 MQTT Subscribe 一次（计数去重），多条 handle 共享一条连接。
func (s *Service) handleSubscribe(w http.ResponseWriter, r *http.Request) {
	var req subReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Args.Handle == "" {
		writeJSON(w, map[string]interface{}{"error": "缺少 handle"})
		return
	}
	if req.Args.CallbackURL == "" {
		writeJSON(w, map[string]interface{}{"error": "缺少 callback_url"})
		return
	}
	var qos byte
	if req.Args.Qos > 0 {
		qos = byte(req.Args.Qos)
	}

	s.mu.Lock()
	_, alreadySubscribed := s.topics[req.Args.Topic]
	s.topics[req.Args.Topic]++
	s.handles[req.Args.Handle] = subEntry{
		topic:       req.Args.Topic,
		qos:         qos,
		callbackURL: req.Args.CallbackURL,
		handle:      req.Args.Handle,
	}
	s.mu.Unlock()

	// topic 首次出现才真正向 broker 订阅（去重）；后续 handle 复用已有订阅。
	if !alreadySubscribed {
		if err := s.cli.Subscribe(req.Args.Topic, qos, s.onMessage); err != nil {
			s.mu.Lock()
			delete(s.handles, req.Args.Handle)
			s.topics[req.Args.Topic]--
			if s.topics[req.Args.Topic] == 0 {
				delete(s.topics, req.Args.Topic)
			}
			s.mu.Unlock()
			writeJSON(w, map[string]interface{}{"error": err.Error()})
			return
		}
	}
	writeJSON(w, map[string]interface{}{"result": map[string]interface{}{"handle": req.Args.Handle, "topic": req.Args.Topic}})
}

type unsubReq struct {
	Args struct {
		Handle string `json:"handle"`
	} `json:"args"`
}

// handleUnsubscribe 撤销一条订阅：移除 handle，topic 计数归零时真正取消 MQTT 订阅。
func (s *Service) handleUnsubscribe(w http.ResponseWriter, r *http.Request) {
	var req unsubReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Args.Handle == "" {
		writeJSON(w, map[string]interface{}{"error": "缺少 handle"})
		return
	}

	s.mu.Lock()
	entry, ok := s.handles[req.Args.Handle]
	if ok {
		delete(s.handles, req.Args.Handle)
		s.topics[entry.topic]--
		if s.topics[entry.topic] <= 0 {
			delete(s.topics, entry.topic)
		}
	}
	s.mu.Unlock()

	if !ok {
		writeJSON(w, map[string]interface{}{"result": map[string]interface{}{"ok": true}})
		return
	}
	// 该 topic 已无任何 handle 订阅 → 真正取消 MQTT 订阅。
	s.mu.Lock()
	_, stillSubscribed := s.topics[entry.topic]
	s.mu.Unlock()
	if !stillSubscribed {
		if err := s.cli.Unsubscribe(entry.topic); err != nil {
			log.Printf("[mqtt] 取消订阅 topic=%s 失败: %v", entry.topic, err)
		}
	}
	writeJSON(w, map[string]interface{}{"result": map[string]interface{}{"ok": true}})
}

// onMessage 收到订阅消息时，按 topic 通配符匹配到相应 handle，逐个 POST 到各 handle 的 callbackURL。
// 回调 URL 由每个 gear 实例独立传入（handle -> callbackURL 映射），不再依赖单一 GEAR_WEBHOOK env。
func (s *Service) onMessage(topic string, payload []byte) {
	type deliver struct {
		handle string
		url    string
	}
	s.mu.Lock()
	targets := make([]deliver, 0)
	for _, e := range s.handles {
		if topicMatches(e.topic, topic) {
			targets = append(targets, deliver{handle: e.handle, url: e.callbackURL})
		}
	}
	s.mu.Unlock()

	for _, t := range targets {
		body, _ := json.Marshal(map[string]interface{}{
			"handle":  t.handle,
			"topic":   topic,
			"payload": string(payload),
		})
		resp, err := http.Post(t.url, "application/json", bytes.NewReader(body))
		if err != nil {
			log.Printf("[mqtt] 回推 Gear(%s) 失败: %v", t.url, err)
			continue
		}
		_ = resp.Body.Close()
	}
}

// topicMatches 判断 topic 是否匹配订阅主题（MQTT 通配符语义：+ 单层、# 多层零或多个、须在末段）。
func topicMatches(pattern, topic string) bool {
	pp := strings.Split(pattern, "/")
	tp := strings.Split(topic, "/")
	for i := 0; i < len(pp); i++ {
		if pp[i] == "#" {
			// '#' 必须且只能在末段，匹配零或多层剩余。
			return i == len(pp)-1
		}
		if i >= len(tp) {
			return false
		}
		if pp[i] != "+" && pp[i] != tp[i] {
			return false
		}
	}
	return len(tp) == len(pp)
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// ---------------------------------------------------------------------------
// paho 适配
// ---------------------------------------------------------------------------

// pahoClient 适配 MQTTClient 接口（真实 paho 实现）。
type pahoClient struct {
	c mqtt.Client
}

func (p *pahoClient) Publish(topic string, qos byte, payload []byte) error {
	token := p.c.Publish(topic, qos, false, payload)
	if !token.WaitTimeout(5 * time.Second) {
		return fmt.Errorf("mqtt publish 超时")
	}
	return token.Error()
}

func (p *pahoClient) Subscribe(topic string, qos byte, h func(topic string, payload []byte)) error {
	token := p.c.Subscribe(topic, qos, func(_ mqtt.Client, msg mqtt.Message) {
		h(msg.Topic(), msg.Payload())
	})
	if !token.WaitTimeout(5 * time.Second) {
		return fmt.Errorf("mqtt subscribe 超时")
	}
	return token.Error()
}

func (p *pahoClient) Unsubscribe(topic string) error {
	token := p.c.Unsubscribe(topic)
	if !token.WaitTimeout(5 * time.Second) {
		return fmt.Errorf("mqtt unsubscribe 超时")
	}
	return token.Error()
}

func (p *pahoClient) IsConnected() bool { return p.c.IsConnected() }

func (p *pahoClient) Disconnect(quiesce uint) {
	if p.c.IsConnected() {
		p.c.Disconnect(quiesce)
	}
}

// ---------------------------------------------------------------------------
// main
// ---------------------------------------------------------------------------

func main() {
	addr := os.Getenv("COORD_ADDR")
	if addr == "" {
		addr = "127.0.0.1:1884"
	}
	broker := os.Getenv("MQTT_BROKER")
	if broker == "" {
		broker = "tcp://test.mosquitto.org:1883"
	}

	// paho 客户端（不显式 SetClientID → paho 自动生成随机 ID，避免公共 broker 碰撞）。
	opts := mqtt.NewClientOptions().AddBroker(broker)
	opts.SetAutoReconnect(true)
	opts.SetConnectRetry(true)
	opts.SetConnectRetryInterval(3 * time.Second)
	cli := mqtt.NewClient(opts)
	// 不阻塞失败：SetAutoReconnect + SetConnectRetry 会在后台持续重连；broker 暂不可达时
	// 侧车仍绑定端口并服务 RPC（publish 在未连上时返回 error，由调用方感知），避免被 Supervisor
	// 反复重启（RestartAlways 下 Fatalf 会陷入重启风暴）。
	if token := cli.Connect(); !token.WaitTimeout(10 * time.Second) {
		log.Printf("MQTT broker(%s) 暂未连上（将后台自动重连）", broker)
	} else if err := token.Error(); err != nil {
		log.Printf("MQTT broker(%s) 连接出错（将后台自动重连）：%v", broker, err)
	} else {
		log.Printf("已连接 MQTT broker %s", broker)
	}

	svc := New(&pahoClient{c: cli})

	c := coord.New(coord.Options{
		CheckInterval: 5 * time.Second,
		TTL:           15 * time.Second,
		OnEmpty: func() {
			// 占用列表空 → 无人需要该 MQTT 桥接 → 平滑停止并自动退出。
			log.Printf("占用列表为空，mqtt-sidecar 自动退出")
			svc.Stop()
			os.Exit(0)
		},
	})

	log.Printf("mqtt-sidecar 监听 %s", addr)
	if err := c.Serve(addr, svc.Handlers()); err != nil {
		if coord.IsAddrInUse(err) {
			// 端口已被其它 mqtt-sidecar 占有（竞态失败者）→ 退让退出，其 owner gear 会复用本实例。
			log.Printf("端口 %s 已被占用，mqtt-sidecar 退让退出", addr)
			os.Exit(coord.ExitAddrInUse)
		}
		log.Fatalf("mqtt-sidecar 异常退出: %v", err)
	}
}
