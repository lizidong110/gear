// Command rabbitmq-sidecar 是 ext.rabbitmq 的常驻侧车（service ext）：
//
//   - 绑定声明端口（listen，127.0.0.1 only），对外暴露 RPC（/publish /declare /consume
//     /subscribe /unsubscribe /caps）+ coord 协调端点（/register 等）；
//   - /consume 是 ctx.ext.rabbitmq.consume 的直接入口，按 mode 分两种真实形态：
//     · pull（默认）：basic.get 拉取最多 max_messages 条并同步返回（取出后 ack）；
//     · push：长驻订阅队列，收到投递即 POST 回推 callbackURL；只有回推成功才 ack，
//     失败则 nack 重入队（不丢消息）；handle 缺省自动生成；
//   - /subscribe 保留给通用订阅原语 ctx.subscribe（等价 push；handle/callback_url 由引擎注入）；
//   - /caps 返回本侧车已实现的方法名（供「声明 ↔ 实现」红线校验）；
//   - 维护占用列表，无人需要则自动退出（OnEmpty）。
//
// 环境变量：RABBITMQ_URL（默认 amqp://guest:guest@127.0.0.1:5672/）、COORD_ADDR（默认 127.0.0.1:5674；
// 引擎拉起时按 extensions.yaml 的 listen 注入，此处默认值仅供不经引擎的手工单跑兜底）。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"gear/internal/sidecar/coord"
	amqp "github.com/rabbitmq/amqp091-go"
)

// Service 是 rabbitmq-sidecar 的业务核心。
type Service struct {
	mu      sync.Mutex
	conn    *amqp.Connection
	channel *amqp.Channel
	cons    map[string]*consEntry // handle -> 消费登记
}

type consEntry struct {
	cancel   context.CancelFunc
	callback string
	queue    string
}

func newService() *Service {
	return &Service{cons: map[string]*consEntry{}}
}

// Handlers 返回 RPC 端点（与 coord 协调端点合并到同一 mux）。
func (s *Service) Handlers() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"/publish":     s.handlePublish,
		"/declare":     s.handleDeclare,
		"/consume":     s.handleConsume, // ctx.ext.rabbitmq.consume：mode=pull 拉一批 / mode=push 长驻订阅
		"/subscribe":   s.handleSubscribe,
		"/unsubscribe": s.handleUnsubscribe,
		"/caps":        s.handleCaps,
	}
}

// Stop 平滑停止：取消所有消费并断开连接。
func (s *Service) Stop() {
	s.mu.Lock()
	for _, e := range s.cons {
		if e.cancel != nil {
			e.cancel()
		}
	}
	conn := s.conn
	s.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
}

func (s *Service) ensureConn(args map[string]interface{}) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn != nil && !s.conn.IsClosed() {
		return nil
	}
	conn, err := amqp.Dial(amqpURL(args))
	if err != nil {
		return err
	}
	ch, err := conn.Channel()
	if err != nil {
		_ = conn.Close()
		return err
	}
	s.conn, s.channel = conn, ch
	return nil
}

func (s *Service) handlePublish(w http.ResponseWriter, r *http.Request) {
	args, err := decodeArgs(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	conn, err := amqp.Dial(amqpURL(args))
	if err != nil {
		writeJSON(w, map[string]interface{}{"error": "连接 RabbitMQ 失败: " + err.Error()})
		return
	}
	defer conn.Close()
	ch, err := conn.Channel()
	if err != nil {
		writeJSON(w, map[string]interface{}{"error": err.Error()})
		return
	}
	defer ch.Close()
	exchange := strOr(args, "exchange", "")
	routingKey := strOr(args, "routing_key", "")
	body := argToBytes(args["body"])
	contentType := strOr(args, "content_type", "")
	if contentType == "" {
		if isJSON(body) {
			contentType = "application/json"
		} else {
			contentType = "text/plain"
		}
	}
	pub := amqp.Publishing{ContentType: contentType, Body: body, DeliveryMode: amqp.Transient}
	if boolOr(args, "persistent", true) {
		pub.DeliveryMode = amqp.Persistent
	}
	if hdr := mapOr(args, "headers"); len(hdr) > 0 {
		pub.Headers = amqp.Table(hdr)
	}
	if err := ch.PublishWithContext(context.Background(), exchange, routingKey, false, false, pub); err != nil {
		writeJSON(w, map[string]interface{}{"error": err.Error()})
		return
	}
	writeJSON(w, map[string]interface{}{"result": map[string]interface{}{"delivered": true, "exchange": exchange, "routing_key": routingKey}})
}

func (s *Service) handleDeclare(w http.ResponseWriter, r *http.Request) {
	args, err := decodeArgs(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	kind := strOr(args, "kind", "")
	if kind != "queue" && kind != "exchange" {
		writeJSON(w, map[string]interface{}{"error": "kind 必填 queue|exchange"})
		return
	}
	name := strOr(args, "name", "")
	if name == "" {
		writeJSON(w, map[string]interface{}{"error": "name 必填"})
		return
	}
	conn, err := amqp.Dial(amqpURL(args))
	if err != nil {
		writeJSON(w, map[string]interface{}{"error": "连接 RabbitMQ 失败: " + err.Error()})
		return
	}
	defer conn.Close()
	ch, err := conn.Channel()
	if err != nil {
		writeJSON(w, map[string]interface{}{"error": err.Error()})
		return
	}
	defer ch.Close()
	durable := boolOr(args, "durable", true)
	if kind == "queue" {
		if _, err := ch.QueueDeclare(name, durable, false, false, false, nil); err != nil {
			writeJSON(w, map[string]interface{}{"error": err.Error()})
			return
		}
	} else {
		et := strOr(args, "exchange_type", "direct")
		if err := ch.ExchangeDeclare(name, et, durable, false, false, false, nil); err != nil {
			writeJSON(w, map[string]interface{}{"error": err.Error()})
			return
		}
	}
	writeJSON(w, map[string]interface{}{"result": map[string]interface{}{"declared": kind, "name": name}})
}

// handleConsume 是 ctx.ext.rabbitmq.consume 的直接入口：按 mode 分发到「拉取」或「长驻订阅」。
func (s *Service) handleConsume(w http.ResponseWriter, r *http.Request) {
	args, err := decodeArgs(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	mode := strOr(args, "mode", "pull")
	switch mode {
	case "pull":
		writeJSON(w, s.doPull(args))
	case "push":
		if strOr(args, "handle", "") == "" {
			args["handle"] = fmt.Sprintf("consume_%d", time.Now().UnixNano()) // 缺省自动生成句柄，返回给调用方
		}
		writeJSON(w, s.doSubscribe(args))
	default:
		writeJSON(w, map[string]interface{}{"error": "未知的 consume mode: " + mode + "（合法：pull|push）"})
	}
}

// handleSubscribe 供通用订阅原语 ctx.subscribe 使用（等价 consume mode=push；handle 由引擎注入）。
func (s *Service) handleSubscribe(w http.ResponseWriter, r *http.Request) {
	args, err := decodeArgs(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, s.doSubscribe(args))
}

// handleCaps 返回本侧车已实现的方法名（供红线校验：extensions.yaml 的 methods ⊆ caps）。
func (s *Service) handleCaps(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]interface{}{"result": map[string]interface{}{
		"ext":     "rabbitmq",
		"methods": []string{"publish", "declare", "consume", "unsubscribe"},
	}})
}

// doPull 用 basic.get 拉取最多 max_messages 条消息并同步返回；取出后 ack（拉取语义：取走即确认）。
func (s *Service) doPull(args map[string]interface{}) map[string]interface{} {
	queue := strOr(args, "queue", "")
	if queue == "" {
		return map[string]interface{}{"error": "缺少 queue（消费目标队列）"}
	}
	max := intOr(args, "max_messages", 1)
	if max <= 0 {
		max = 1
	}
	if err := s.ensureConn(args); err != nil {
		return map[string]interface{}{"error": "RabbitMQ 连接失败: " + err.Error()}
	}
	msgs := make([]map[string]interface{}, 0, max)
	for len(msgs) < max {
		d, ok, err := s.channel.Get(queue, false) // autoAck=false：取出后手动 ack
		if err != nil {
			if len(msgs) == 0 {
				return map[string]interface{}{"error": "拉取队列消息失败: " + err.Error()}
			}
			break
		}
		if !ok {
			break // 队列已空
		}
		msgs = append(msgs, map[string]interface{}{
			"queue":        queue,
			"body":         string(d.Body),
			"headers":      map[string]interface{}(d.Headers),
			"content_type": d.ContentType,
		})
		_ = d.Ack(false)
	}
	return map[string]interface{}{"result": map[string]interface{}{
		"queue": queue, "count": len(msgs), "messages": msgs,
	}}
}

// doSubscribe 登记一条长驻订阅：侧车常驻消费队列，收到投递即 POST 回推 callbackURL。
// 关键：只有回推成功才 ack；回推失败则 nack 重入队，避免丢消息。
func (s *Service) doSubscribe(args map[string]interface{}) map[string]interface{} {
	handle := strOr(args, "handle", "")
	callback := strOr(args, "callback_url", "")
	queue := strOr(args, "queue", "")
	if handle == "" {
		return map[string]interface{}{"error": "缺少 handle"}
	}
	if callback == "" {
		return map[string]interface{}{"error": "缺少 callback_url"}
	}
	if queue == "" {
		return map[string]interface{}{"error": "缺少 queue（订阅目标队列）"}
	}
	if err := s.ensureConn(args); err != nil {
		return map[string]interface{}{"error": "RabbitMQ 连接失败: " + err.Error()}
	}
	deliveries, err := s.channel.Consume(queue, handle, false, false, false, false, nil)
	if err != nil {
		return map[string]interface{}{"error": "订阅队列失败: " + err.Error()}
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	s.cons[handle] = &consEntry{cancel: cancel, callback: callback, queue: queue}
	s.mu.Unlock()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case d, ok := <-deliveries:
				if !ok {
					return
				}
				payload := map[string]interface{}{
					"handle":  handle,
					"queue":   queue,
					"body":    string(d.Body),
					"headers": map[string]interface{}(d.Headers),
				}
				if err := pushToGear(callback, payload); err != nil {
					log.Printf("[rabbitmq] 回推 Gear(%s) 失败: %v（nack 重入队，不丢消息）", callback, err)
					_ = d.Nack(false, true) // 回推失败 → 重新入队
					time.Sleep(200 * time.Millisecond)
					continue
				}
				_ = d.Ack(false) // 仅回推成功才确认
			}
		}
	}()
	return map[string]interface{}{"result": map[string]interface{}{"handle": handle, "queue": queue}}
}

func (s *Service) handleUnsubscribe(w http.ResponseWriter, r *http.Request) {
	args, err := decodeArgs(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	handle := strOr(args, "handle", "")
	s.mu.Lock()
	e, ok := s.cons[handle]
	if ok {
		if e.cancel != nil {
			e.cancel()
		}
		delete(s.cons, handle)
	}
	s.mu.Unlock()
	writeJSON(w, map[string]interface{}{"result": map[string]interface{}{"ok": true, "removed": ok}})
}

// ---- 共享小工具 ----

func amqpURL(args map[string]interface{}) string {
	if u := strOr(args, "url", ""); u != "" {
		return u
	}
	host := strOr(args, "host", "localhost")
	port := intOr(args, "port", 5672)
	user := strOr(args, "user", "guest")
	pass := strOr(args, "pass", "guest")
	return fmt.Sprintf("amqp://%s:%s@%s:%d/", user, pass, host, port)
}

func argToBytes(v interface{}) []byte {
	switch x := v.(type) {
	case nil:
		return nil
	case string:
		return []byte(x)
	case []byte:
		return x
	default:
		b, _ := json.Marshal(x)
		return b
	}
}

func isJSON(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	return b[0] == '{' || b[0] == '['
}

func decodeArgs(r *http.Request) (map[string]interface{}, error) {
	var req struct {
		Args map[string]interface{} `json:"args"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return nil, err
	}
	return req.Args, nil
}

func pushToGear(callbackURL string, payload map[string]interface{}) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	resp, err := http.Post(callbackURL, "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func strOr(m map[string]interface{}, k, def string) string {
	if v, ok := m[k].(string); ok && v != "" {
		return v
	}
	return def
}

func intOr(m map[string]interface{}, k string, def int) int {
	switch v := m[k].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	default:
		return def
	}
}

func boolOr(m map[string]interface{}, k string, def bool) bool {
	if v, ok := m[k].(bool); ok {
		return v
	}
	return def
}

func mapOr(m map[string]interface{}, k string) map[string]interface{} {
	if v, ok := m[k].(map[string]interface{}); ok {
		return v
	}
	return map[string]interface{}{}
}

func main() {
	addr := os.Getenv("COORD_ADDR")
	if addr == "" {
		addr = "127.0.0.1:5674"
	}
	svc := newService()
	c := coord.New(coord.Options{
		CheckInterval: 5 * time.Second,
		TTL:           15 * time.Second,
		OnEmpty: func() {
			log.Printf("占用列表为空，rabbitmq-sidecar 自动退出")
			svc.Stop()
			os.Exit(0)
		},
	})
	log.Printf("rabbitmq-sidecar 监听 %s", addr)
	if err := c.Serve(addr, svc.Handlers()); err != nil {
		if coord.IsAddrInUse(err) {
			log.Printf("端口 %s 已被占用，rabbitmq-sidecar 退让退出", addr)
			os.Exit(coord.ExitAddrInUse)
		}
		log.Fatalf("rabbitmq-sidecar 异常退出: %v", err)
	}
}
