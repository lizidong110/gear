// Command webhook-sidecar 是 ext.webhook 的常驻侧车（service ext）：
//
//   - 绑定两个端口：
//     · listen（COORD_ADDR，默认 127.0.0.1:8089）：coord 协调 + RPC（/subscribe /unsubscribe）；
//     · webhook_addr（WEBHOOK_ADDR，默认 :8088）：外部系统 POST 进来的入站回调地址。
//   - /subscribe 登记一条入站路由（path -> handle -> callback_url）；外部 POST 到 /in/{path}
//     时，侧车把（handle, path, headers, body）POST 回 gear 的 callbackURL，由引擎回推事件；
//   - 维护占用列表，无人需要则自动退出（OnEmpty）。
//
// 说明：webhook 的「监听一个 URL」= 外部系统向本侧车的 webhook_addr 发 POST；这与
// mqtt/rabbitmq/kafka 由 sidecar 主动连 broker 不同，故 webhook 侧车多绑定一个入站端口。
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"gear/internal/sidecar/coord"
)

type Service struct {
	mu       sync.Mutex
	byPath   map[string]string // inbound path -> handle
	byHandle map[string]string // handle -> callbackURL
}

func newService() *Service {
	return &Service{byPath: map[string]string{}, byHandle: map[string]string{}}
}

func (s *Service) Handlers() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"/send":        s.handleSend, // outbound webhook（复用长连接，对应 ctx.ext.webhook.send）
		"/subscribe":   s.handleSubscribe,
		"/unsubscribe": s.handleUnsubscribe,
		"/caps":        s.handleCaps,
	}
}

// handleCaps 返回本侧车已实现的方法名（供红线校验：extensions.yaml 的 methods ⊆ caps）。
func (s *Service) handleCaps(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]interface{}{"result": map[string]interface{}{
		"ext":     "webhook",
		"methods": []string{"send", "subscribe", "unsubscribe"},
	}})
}

func (s *Service) Stop() {
	s.mu.Lock()
	s.byPath = map[string]string{}
	s.byHandle = map[string]string{}
	s.mu.Unlock()
}

// inboundMux 是外部入站回调的 HTTP mux（监听 webhook_addr）。
func (s *Service) inboundMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/in/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "仅支持 POST", http.StatusMethodNotAllowed)
			return
		}
		path := strings.TrimPrefix(r.URL.Path, "/in/")
		s.mu.Lock()
		handle := s.byPath[path]
		callback := s.byHandle[handle]
		s.mu.Unlock()
		if handle == "" || callback == "" {
			http.Error(w, "未知入站路径", http.StatusNotFound)
			return
		}
		body, _ := io.ReadAll(r.Body)
		headers := map[string]interface{}{}
		for k := range r.Header {
			headers[k] = r.Header.Get(k)
		}
		payload := map[string]interface{}{
			"handle":  handle,
			"path":    path,
			"method":  r.Method,
			"headers": headers,
			"body":    string(body),
		}
		if err := pushToGear(callback, payload); err != nil {
			log.Printf("[webhook] 回推 Gear(%s) 失败: %v", callback, err)
			http.Error(w, "回推失败", http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
	})
	return mux
}

func (s *Service) handleSubscribe(w http.ResponseWriter, r *http.Request) {
	args, err := decodeArgs(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	handle := strOr(args, "handle", "")
	callback := strOr(args, "callback_url", "")
	path := strOr(args, "path", "")
	if handle == "" {
		writeJSON(w, map[string]interface{}{"error": "缺少 handle"})
		return
	}
	if callback == "" {
		writeJSON(w, map[string]interface{}{"error": "缺少 callback_url"})
		return
	}
	if path == "" {
		writeJSON(w, map[string]interface{}{"error": "缺少 path（入站路由，外部 POST /in/{path}）"})
		return
	}
	s.mu.Lock()
	s.byPath[path] = handle
	s.byHandle[handle] = callback
	s.mu.Unlock()
	writeJSON(w, map[string]interface{}{"result": map[string]interface{}{"handle": handle, "path": path}})
}

func (s *Service) handleUnsubscribe(w http.ResponseWriter, r *http.Request) {
	args, err := decodeArgs(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	handle := strOr(args, "handle", "")
	s.mu.Lock()
	path, ok := reverseLookup(s.byHandle, handle)
	if ok {
		delete(s.byPath, path)
	}
	delete(s.byHandle, handle)
	s.mu.Unlock()
	writeJSON(w, map[string]interface{}{"result": map[string]interface{}{"ok": true, "removed": ok}})
}

// handleSend 是 outbound webhook：向任意外部 URL POST JSON（对应 ctx.ext.webhook.send）。
// 与 inbound /in/{path} 方向相反——这里侧车是 HTTP 客户端，把 payload 推给外部系统。
func (s *Service) handleSend(w http.ResponseWriter, r *http.Request) {
	args, err := decodeArgs(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	url := strOr(args, "url", "")
	if url == "" {
		writeJSON(w, map[string]interface{}{"error": "url 必填"})
		return
	}
	method := strings.ToUpper(strOr(args, "method", "POST"))
	payload := args["payload"]
	if payload == nil {
		writeJSON(w, map[string]interface{}{"error": "payload 必填"})
		return
	}
	body, err := json.Marshal(payload)
	if err != nil {
		writeJSON(w, map[string]interface{}{"error": "payload 序列化失败: " + err.Error()})
		return
	}
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		writeJSON(w, map[string]interface{}{"error": err.Error()})
		return
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range mapOr(args, "headers") {
		req.Header.Set(k, fmt.Sprint(v))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		writeJSON(w, map[string]interface{}{"error": "POST " + url + " 失败: " + err.Error()})
		return
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	writeJSON(w, map[string]interface{}{
		"result": map[string]interface{}{
			"status": resp.StatusCode,
			"body":   string(respBody),
		},
	})
}

func reverseLookup(m map[string]string, val string) (string, bool) {
	for k, v := range m {
		if v == val {
			return k, true
		}
	}
	return "", false
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

func mapOr(m map[string]interface{}, k string) map[string]interface{} {
	if v, ok := m[k].(map[string]interface{}); ok {
		return v
	}
	return map[string]interface{}{}
}

func main() {
	coordAddr := os.Getenv("COORD_ADDR")
	if coordAddr == "" {
		coordAddr = "127.0.0.1:8089"
	}
	webhookAddr := os.Getenv("WEBHOOK_ADDR")
	if webhookAddr == "" {
		webhookAddr = ":8088"
	}
	svc := newService()
	c := coord.New(coord.Options{
		CheckInterval: 5 * time.Second,
		TTL:           15 * time.Second,
		OnEmpty: func() {
			log.Printf("占用列表为空，webhook-sidecar 自动退出")
			svc.Stop()
			os.Exit(0)
		},
	})

	// 入站回调端口（外部系统 POST 进来）独立监听，与 coord 端口正交。
	// 注意：入站监听失败（如端口被占用）只记日志、不自杀——coord 端口仍可用，
	// 对应 ctx.ext.webhook.send 等出站能力不受影响（自杀会拖垮整个侧车）。
	go func() {
		log.Printf("webhook 入站监听 %s（外部 POST 到 /in/{path}）", webhookAddr)
		if err := http.ListenAndServe(webhookAddr, svc.inboundMux()); err != nil {
			log.Printf("webhook 入站监听失败（listen 能力不可用，coord 端口不受影响）: %v", err)
		}
	}()

	log.Printf("webhook-sidecar 协调监听 %s", coordAddr)
	if err := c.Serve(coordAddr, svc.Handlers()); err != nil {
		if coord.IsAddrInUse(err) {
			log.Printf("端口 %s 已被占用，webhook-sidecar 退让退出", coordAddr)
			os.Exit(coord.ExitAddrInUse)
		}
		log.Fatalf("webhook-sidecar 异常退出: %v", err)
	}
}
