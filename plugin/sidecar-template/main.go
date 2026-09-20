// Command sidecar-template 是「常驻 ext 侧车」的参考骨架（EXT_SERVICE_DESIGN.md §3/§12）。
//
// 它演示一个用户侧车如何用 gear 提供的 coord 包：
//   - 绑定声明端口（127.0.0.1 only），对外暴露业务 RPC 端点（/publish 等）；
//   - 维护占用列表（各 gear 的 PID + token），定期巡检，PID 已死或心跳超时即移除；
//   - 占用列表空 → 自动退出（无人需要该服务）；
//   - 端口被占用（EADDRINUSE）→ 退让退出（其 owner gear 会改以 client 复用本实例）。
//
// 真实场景（如 mqtt-sidecar）把 /publish 换成真正的 MQTT publish，把后台 goroutine
// 换成真正的 MQTT subscribe（收到消息经 Gear REST / event 回调）。本骨架不含真实协议，
// 仅把"常驻 + 协调"的骨架跑通。
package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"time"

	"gear/internal/sidecar/coord"
)

func main() {
	addr := os.Getenv("COORD_ADDR")
	if addr == "" {
		addr = "127.0.0.1:1884"
	}

	// 业务 RPC 端点：Gear 经 LocalHTTPTransport 调用（POST /<method>，body {"args":...}）。
	business := map[string]http.HandlerFunc{
		"/publish": func(w http.ResponseWriter, r *http.Request) {
			var req struct {
				Args struct {
					Topic   string `json:"topic"`
					Payload string `json:"payload"`
					Qos     int    `json:"qos"`
				} `json:"args"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			// TODO: 真实场景在此经内部长连接 publish 到 MQTT。
			log.Printf("[publish] topic=%s qos=%d payload=%q", req.Args.Topic, req.Args.Qos, req.Args.Payload)
			writeJSON(w, map[string]interface{}{"result": "ok"})
		},
		"/subscribe": func(w http.ResponseWriter, r *http.Request) {
			// subscribe 是"长驻消费"语义：侧车内部持续订阅，此端点仅用于登记意图。
			writeJSON(w, map[string]interface{}{"result": "subscribing"})
		},
	}

	c := coord.New(coord.Options{
		CheckInterval: 5 * time.Second,
		TTL:           15 * time.Second,
		OnEmpty: func() {
			// 占用列表空 → 无人需要该服务 → 自动退出（见 §12.5）。
			log.Printf("占用列表为空，sidecar 自动退出")
			os.Exit(0)
		},
	})

	// 演示 daemon 语义：后台持续运行（真实场景替换为 MQTT subscribe 长驻消费）。
	go func() {
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for range t.C {
			log.Printf("[daemon] 持续运行（真实场景：消费订阅消息并经 Gear REST/event 回调）")
		}
	}()

	log.Printf("sidecar 监听 %s", addr)
	if err := c.Serve(addr, business); err != nil {
		if coord.IsAddrInUse(err) {
			// 端口已被其它 sidecar 占有（竞态失败者）→ 退让退出，其 owner gear 会复用本实例。
			log.Printf("端口 %s 已被占用，sidecar 退让退出", addr)
			os.Exit(coord.ExitAddrInUse)
		}
		log.Fatalf("sidecar 异常退出: %v", err)
	}
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
