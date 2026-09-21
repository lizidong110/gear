// Command serve-sidecar 是 ext.serve 的常驻侧车（service ext）：
//
//   - 绑定声明端口（listen / COORD_ADDR，127.0.0.1 only），对外暴露 RPC
//     （/start /stop /caps）+ coord 协调端点（/register 等）；
//   - /start 在「另一个端口」起一个静态网站 HTTP 服务（类似 nginx：给定端口 + 静态目录，
//     浏览器即可访问），并可带 proxy 规则把某些前缀（如 /api）反向代理到后端——
//     静态与接口同源，vite dev proxy 的生产等价物（见 proxy.go）。
//     port / dir / host / proxy 全部从 args 传入（不读 os.Getenv，缺参即报错）；
//   - /stop 平滑关闭静态网站服务、释放端口；
//   - /caps 返回本侧车已实现的方法名（供「声明 ↔ 实现」红线校验）；
//   - 维护占用列表，无人需要则自动退出（OnEmpty）。
//
// 设计要点（区别于其它侧车）：本侧车有**两个端口**——
//
//	· RPC 端口（COORD_ADDR，如 127.0.0.1:8095）：Gear 经 LocalHTTPTransport 调 /start /stop /caps；
//	· 网站端口（args.port，如 8080）：http.FileServer 对外服务的真实端口，浏览器访问它。
//
// 两者必须分开：RPC 路由是固定的 /start /stop，会和网站根路径 '/' 上托管的静态资源冲突。
//
// 环境变量：COORD_ADDR（默认 127.0.0.1:8095；引擎拉起时按 extensions.yaml 的 listen 注入，
// 此处默认值仅供不经引擎的手工单跑兜底）。注意这只是「侧车自己的协调端口」，
// 与网站端口无关——网站端口永远来自 /start 的 args.port（遵循 ext 原子红线：配置从参数传）。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"gear/internal/sidecar/coord"
)

// Service 是 serve-sidecar 的业务核心。
type Service struct {
	mu      sync.Mutex
	srv     *http.Server // 网站服务（静态 + 可选反代；复用 srv.Serve 绑定 ln）
	ln      net.Listener // 静态网站监听套接字
	port    int          // 当前网站端口
	dir     string       // 当前网站根目录
	proxy   []proxyRule  // 当前反向代理规则（空 = 纯静态）
	serving bool         // 是否正在服务
}

func newService() *Service {
	return &Service{}
}

// Handlers 返回 RPC 端点（与 coord 协调端点合并到同一 mux）。
func (s *Service) Handlers() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"/start": s.handleStart,
		"/stop":  s.handleStop,
		"/caps":  s.handleCaps,
	}
}

// Stop 平滑停止网站服务。
func (s *Service) Stop() {
	s.mu.Lock()
	srv := s.srv
	s.serving = false
	s.proxy = nil
	s.mu.Unlock()
	if srv != nil {
		_ = srv.Shutdown(context.Background())
	}
}

// handleStart 在指定端口起「静态网站 + 可选反向代理」服务（类似 nginx）。
// args：port（必填，数字）、dir（必填，本地目录）、host（可选，默认 0.0.0.0）、
// proxy（可选，反向代理规则数组：[{path,target,strip_prefix}]，见 proxy.go）。
func (s *Service) handleStart(w http.ResponseWriter, r *http.Request) {
	args, err := decodeArgs(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	port := intOr(args, "port", 0)
	if port <= 0 {
		writeJSON(w, map[string]interface{}{"error": "port 必填（对外服务的监听端口，数字）"})
		return
	}
	dir := strOr(args, "dir", "")
	if dir == "" {
		writeJSON(w, map[string]interface{}{"error": "dir 必填（静态网站根目录，本地路径，传给 ext.serve）"})
		return
	}
	// fail-closed：目录必须真实存在且是目录，否则不静默起一个空服务。
	if fi, e := os.Stat(dir); e != nil || !fi.IsDir() {
		writeJSON(w, map[string]interface{}{"error": "dir 不存在或不是目录: " + dir})
		return
	}
	host := strOr(args, "host", "0.0.0.0")
	// fail-closed：规则写错就报错（缺 path/target、target 非 http(s) URL…），绝不静默丢弃——
	// 静默丢弃正是「UI 的 /api 全 404」这类问题的成因。放在幂等判断之前，保证错误必被报出。
	proxyRules, err := parseProxyRules(args["proxy"])
	if err != nil {
		writeJSON(w, map[string]interface{}{"error": err.Error()})
		return
	}

	s.mu.Lock()
	if s.serving {
		// 幂等：已经在服务则直接返回当前 URL，避免重复绑定端口冲突。
		url := s.currentURL()
		curDir, curProxy := s.dir, s.proxy
		s.mu.Unlock()
		writeJSON(w, map[string]interface{}{"result": map[string]interface{}{
			"url": url, "port": s.port, "dir": curDir, "serving": true, "proxy": proxyRulesJSON(curProxy),
		}})
		return
	}
	s.mu.Unlock()

	ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", host, port))
	if err != nil {
		writeJSON(w, map[string]interface{}{"error": fmt.Sprintf("端口 %d 绑定失败: %v", port, err)})
		return
	}

	handler := newSiteHandler(dir, proxyRules)
	srv := &http.Server{Handler: handler}

	s.mu.Lock()
	s.ln, s.srv, s.port, s.dir, s.proxy, s.serving = ln, srv, port, dir, proxyRules, true
	s.mu.Unlock()

	go func() {
		if e := srv.Serve(ln); e != nil && e != http.ErrServerClosed {
			log.Printf("[serve] 网站服务异常退出: %v", e)
		}
	}()

	writeJSON(w, map[string]interface{}{"result": map[string]interface{}{
		"url":     fmt.Sprintf("http://%s:%d/", host, port),
		"port":    port,
		"dir":     dir,
		"serving": true,
		"proxy":   proxyRulesJSON(proxyRules),
	}})
}

func (s *Service) currentURL() string {
	return fmt.Sprintf("http://0.0.0.0:%d/", s.port)
}

// handleStop 平滑停止静态网站服务。
func (s *Service) handleStop(w http.ResponseWriter, r *http.Request) {
	// 排空请求体：否则未读的 JSON body 会让 Go http 服务端向（可能复用连接的）客户端发 RST，
	// 表现为「WinError 10054 连接被重置」的偶发失败（handleStart 因 decodeArgs 已读体，无此问题）。
	_, _ = decodeArgs(r)

	s.mu.Lock()
	wasServing := s.serving
	srv := s.srv
	s.serving = false
	s.proxy = nil
	s.mu.Unlock()

	if !wasServing {
		writeJSON(w, map[string]interface{}{"result": map[string]interface{}{"ok": true, "running": false}})
		return
	}
	if srv != nil {
		_ = srv.Shutdown(context.Background())
	}
	writeJSON(w, map[string]interface{}{"result": map[string]interface{}{"ok": true, "running": false}})
}

// handleCaps 返回本侧车已实现的方法名（供红线校验：extensions.yaml 的 methods ⊆ caps）。
func (s *Service) handleCaps(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]interface{}{"result": map[string]interface{}{
		"ext":     "serve",
		"methods": []string{"start", "stop"},
	}})
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

func main() {
	addr := os.Getenv("COORD_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8095"
	}

	svc := newService()
	c := coord.New(coord.Options{
		CheckInterval: 5 * time.Second,
		TTL:           15 * time.Second,
		OnEmpty: func() {
			// 占用列表为空 → 无人需要该服务 → 自动退出（先停静态网站释放端口）。
			log.Printf("占用列表为空，serve-sidecar 自动退出")
			svc.Stop()
			os.Exit(0)
		},
	})

	log.Printf("serve-sidecar 监听 %s（RPC/coord 端口；网站端口由 /start 的 args.port 决定）", addr)
	if err := c.Serve(addr, svc.Handlers()); err != nil {
		if coord.IsAddrInUse(err) {
			// 端口已被其它 sidecar 占有（竞态失败者）→ 退让退出，其 owner gear 会复用本实例。
			log.Printf("端口 %s 已被占用，serve-sidecar 退让退出", addr)
			os.Exit(coord.ExitAddrInUse)
		}
		log.Fatalf("serve-sidecar 异常退出: %v", err)
	}
}
