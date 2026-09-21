// Command kafka-sidecar 是 ext.kafka 的常驻侧车（service ext）：
//
//   - 绑定声明端口（listen，127.0.0.1 only），对外暴露 RPC（/produce /consume /subscribe
//     /unsubscribe /caps）+ coord 协调端点；
//   - /consume 是 ctx.ext.kafka.consume 的直接入口，按 mode 分两种真实形态：
//     · pull（默认）：拉取最多 max_messages 条，同步返回消息数组；只有本批成功返回后
//     才提交 offset（不丢消息）；
//     · push：长驻订阅 topic，收到消息即 POST 到 gear 的 callbackURL（handle 缺省自动生成）；
//   - /subscribe 保留给通用订阅原语 ctx.subscribe（等价 push；handle/callback_url 由引擎注入）；
//   - /caps 返回本侧车已实现的方法名（供「声明 ↔ 实现」红线校验）；
//   - 维护占用列表，无人需要则自动退出（OnEmpty）。
//
// 环境变量：COORD_ADDR（默认 127.0.0.1:9094；引擎拉起时按 extensions.yaml 的 listen 注入，
// 此处默认值仅供不经引擎的手工单跑兜底）。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"gear/internal/sidecar/coord"
	"github.com/segmentio/kafka-go"
)

type Service struct {
	mu   sync.Mutex
	cons map[string]*consEntry
}

type consEntry struct {
	cancel   context.CancelFunc
	callback string
	topic    string
}

func newService() *Service {
	return &Service{cons: map[string]*consEntry{}}
}

func (s *Service) Handlers() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"/produce":     s.handleProduce,
		"/consume":     s.handleConsume, // ctx.ext.kafka.consume：mode=pull 拉一批 / mode=push 长驻订阅
		"/subscribe":   s.handleSubscribe,
		"/unsubscribe": s.handleUnsubscribe,
		"/caps":        s.handleCaps,
	}
}

func (s *Service) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.cons {
		if e.cancel != nil {
			e.cancel()
		}
	}
}

func kafkaBrokers(args map[string]interface{}) []string {
	if b, ok := args["brokers"].(string); ok && b != "" {
		return strings.Split(b, ",")
	}
	if arr, ok := args["brokers"].([]interface{}); ok {
		out := make([]string, 0, len(arr))
		for _, x := range arr {
			out = append(out, fmt.Sprint(x))
		}
		return out
	}
	return nil
}

func (s *Service) handleProduce(w http.ResponseWriter, r *http.Request) {
	args, err := decodeArgs(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	topic := strOr(args, "topic", "")
	if topic == "" {
		writeJSON(w, map[string]interface{}{"error": "topic 必填"})
		return
	}
	brokers := kafkaBrokers(args)
	if len(brokers) == 0 {
		writeJSON(w, map[string]interface{}{"error": "brokers 必填"})
		return
	}
	key := strOr(args, "key", "")
	val := argToBytes(args["value"])
	writer := &kafka.Writer{Addr: kafka.TCP(brokers...), Topic: topic}
	defer writer.Close()
	msg := kafka.Message{Value: val}
	if key != "" {
		msg.Key = []byte(key)
	}
	if err := writer.WriteMessages(context.Background(), msg); err != nil {
		writeJSON(w, map[string]interface{}{"error": "生产 Kafka 消息失败: " + err.Error()})
		return
	}
	writeJSON(w, map[string]interface{}{"result": map[string]interface{}{"produced": true, "topic": topic}})
}

// handleConsume 是 ctx.ext.kafka.consume 的直接入口：按 mode 分发到「拉取」或「长驻订阅」。
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
		"ext":     "kafka",
		"methods": []string{"produce", "consume", "unsubscribe"},
	}})
}

// doPull 拉取最多 max_messages 条消息并同步返回；只有本批成功返回后才提交 offset（不丢消息）。
// 支持 group（消费组，默认 gear-consume）、start（无已提交 offset 时的起始位点 earliest|latest）、
// timeout（本批等待时长，默认 5s）。
func (s *Service) doPull(args map[string]interface{}) map[string]interface{} {
	topic := strOr(args, "topic", "")
	if topic == "" {
		return map[string]interface{}{"error": "topic 必填"}
	}
	brokers := kafkaBrokers(args)
	if len(brokers) == 0 {
		return map[string]interface{}{"error": "brokers 必填"}
	}
	group := strOr(args, "group", "gear-consume")
	max := intOr(args, "max_messages", 1)
	if max <= 0 {
		max = 1
	}
	timeout := durationOr(args, "timeout", 5*time.Second)
	var startOffset int64
	switch strings.ToLower(strOr(args, "start", "latest")) {
	case "earliest", "first", "oldest":
		startOffset = kafka.FirstOffset
	case "latest", "last", "newest":
		startOffset = kafka.LastOffset
	default:
		return map[string]interface{}{"error": "未知的 start: " + strOr(args, "start", "") + "（合法：earliest|latest）"}
	}
	// 预检 broker 连通性：连不上必须 fail-closed。否则「broker 挂了」与「topic 暂无消息」
	// 都表现为超时 0 条，调用方无法区分（这正是要避免的假成功）。
	probeTimeout := timeout
	if probeTimeout > 3*time.Second {
		probeTimeout = 3 * time.Second
	}
	dialCtx, dialCancel := context.WithTimeout(context.Background(), probeTimeout)
	probe, derr := kafka.DialContext(dialCtx, "tcp", brokers[0])
	dialCancel()
	if derr != nil {
		return map[string]interface{}{"error": "无法连接 Kafka broker(" + brokers[0] + "): " + derr.Error()}
	}
	_ = probe.Close()

	reader := kafka.NewReader(kafka.ReaderConfig{
		Brokers:        brokers,
		Topic:          topic,
		GroupID:        group,
		StartOffset:    startOffset,
		CommitInterval: 0, // 手动提交：读完并成功返回本批后才提交
		MaxWait:        500 * time.Millisecond,
	})
	defer reader.Close()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	raw := make([]kafka.Message, 0, max)
	msgs := make([]map[string]interface{}, 0, max)
	for len(msgs) < max {
		m, err := reader.ReadMessage(ctx)
		if err != nil {
			// 首条就失败且非超时 → 真错误（如 broker 连不上），fail-closed 返回 error。
			if len(msgs) == 0 && !errors.Is(err, context.DeadlineExceeded) {
				return map[string]interface{}{"error": "消费 Kafka 消息失败: " + err.Error()}
			}
			break // 超时或中途出错：返回已拉到的部分
		}
		raw = append(raw, m)
		msgs = append(msgs, map[string]interface{}{
			"topic":     m.Topic,
			"partition": m.Partition,
			"offset":    m.Offset,
			"key":       string(m.Key),
			"body":      string(m.Value),
		})
	}
	if group != "" && len(raw) > 0 {
		_ = reader.CommitMessages(context.Background(), raw...)
	}
	return map[string]interface{}{"result": map[string]interface{}{
		"topic": topic, "group": group, "count": len(msgs), "messages": msgs,
	}}
}

// doSubscribe 登记一条长驻订阅：侧车常驻消费 topic，收到消息即 POST 回推 callbackURL。
func (s *Service) doSubscribe(args map[string]interface{}) map[string]interface{} {
	handle := strOr(args, "handle", "")
	callback := strOr(args, "callback_url", "")
	topic := strOr(args, "topic", "")
	if handle == "" {
		return map[string]interface{}{"error": "缺少 handle"}
	}
	if callback == "" {
		return map[string]interface{}{"error": "缺少 callback_url"}
	}
	if topic == "" {
		return map[string]interface{}{"error": "缺少 topic"}
	}
	brokers := kafkaBrokers(args)
	if len(brokers) == 0 {
		return map[string]interface{}{"error": "brokers 必填"}
	}
	group := strOr(args, "group", "gear-"+handle)
	reader := kafka.NewReader(kafka.ReaderConfig{
		Brokers: brokers,
		Topic:   topic,
		GroupID: group,
	})
	ctx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	s.cons[handle] = &consEntry{cancel: cancel, callback: callback, topic: topic}
	s.mu.Unlock()
	go func() {
		for {
			m, err := reader.ReadMessage(ctx)
			if err != nil {
				if ctx.Err() != nil {
					_ = reader.Close()
					return
				}
				log.Printf("[kafka] 读取消息失败: %v", err)
				continue
			}
			payload := map[string]interface{}{
				"handle":    handle,
				"topic":     topic,
				"partition": m.Partition,
				"offset":    m.Offset,
				"key":       string(m.Key),
				"body":      string(m.Value),
			}
			if err := pushToGear(callback, payload); err != nil {
				log.Printf("[kafka] 回推 Gear(%s) 失败: %v", callback, err)
			}
		}
	}()
	return map[string]interface{}{"result": map[string]interface{}{"handle": handle, "topic": topic, "group": group}}
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

func decodeArgs(r *http.Request) (map[string]interface{}, error) {
	var req struct {
		Args map[string]interface{} `json:"args"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return nil, err
	}
	return req.Args, nil
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

// durationOr 解析时长参数：字符串按 Go duration（如 "5s"）；数字按秒。
func durationOr(m map[string]interface{}, k string, def time.Duration) time.Duration {
	if s, ok := m[k].(string); ok && s != "" {
		if d, err := time.ParseDuration(s); err == nil {
			return d
		}
	}
	if n, ok := m[k].(float64); ok && n > 0 {
		return time.Duration(n) * time.Second
	}
	return def
}

func main() {
	addr := os.Getenv("COORD_ADDR")
	if addr == "" {
		addr = "127.0.0.1:9094"
	}
	svc := newService()
	c := coord.New(coord.Options{
		CheckInterval: 5 * time.Second,
		TTL:           15 * time.Second,
		OnEmpty: func() {
			log.Printf("占用列表为空，kafka-sidecar 自动退出")
			svc.Stop()
			os.Exit(0)
		},
	})
	log.Printf("kafka-sidecar 监听 %s", addr)
	if err := c.Serve(addr, svc.Handlers()); err != nil {
		if coord.IsAddrInUse(err) {
			log.Printf("端口 %s 已被占用，kafka-sidecar 退让退出", addr)
			os.Exit(coord.ExitAddrInUse)
		}
		log.Fatalf("kafka-sidecar 异常退出: %v", err)
	}
}
