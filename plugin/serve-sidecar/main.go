// Command serve-sidecar 是 ext.serve 的常驻侧车（service ext）：
//
//   - 绑定声明端口（listen / COORD_ADDR，127.0.0.1 only），对外暴露 RPC
//     （/start /stop /caps）+ coord 协调端点（/register 等）；
//   - /start 在「另一个端口」起一个静态网站 HTTP 服务（类似 nginx：给定端口 + 静态目录，
//     浏览器即可访问），并可带 proxy 规则把某些前缀（如 /api）反向代理到后端——
//     静态与接口同源，vite dev proxy 的生产等价物（见 proxy.go）。
//     port / dir / host / proxy 全部从 args 传入（不读 os.Getenv，缺参即报错）；
//     port=0 表示自动分配，由内核挑端口、真实值从 ln.Addr() 回读并在返回值里带回；
//   - **多站点**（2026-09-24 重构）：一个侧车可同时托管多个网站（不同 port）——
//     这是多 app 共享同一侧车的前提（如 web_ui.up 托管 8080 的同时 mindstorm.up 托管 8090）。
//     幂等按「port + dir + host + proxy 全同」判定：全同 → 返回现状；同端口不同配置 →
//     fail-closed 报错（绝不静默吞掉新参数——那正是「8090 起不来却显示成功」的成因）。
//   - /stop 平滑停止网站服务、释放端口：带 port 只停该站点；不带 port 停全部站点；
//   - /caps 返回本侧车已实现的方法名（供「声明 ↔ 实现」红线校验）；
//   - 维护占用列表，无人需要则自动退出（OnEmpty）。
//
// 设计要点（区别于其它侧车）：本侧车有**两类端口**——
//
//	· RPC 端口（COORD_ADDR，如 127.0.0.1:8095）：Gear 经 LocalHTTPTransport 调 /start /stop /caps；
//	· 网站端口（args.port，如 8080/8090）：http.FileServer 对外服务的真实端口，浏览器访问它。
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
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"gear/internal/gearhome"
	"gear/internal/sidecar/coord"
)

// site 是一个已托管的静态网站（一个网站端口一个实例）。
type site struct {
	srv   *http.Server
	ln    net.Listener
	host  string
	dir   string
	proxy []proxyRule
}

// Service 是 serve-sidecar 的业务核心：按网站端口管理多个站点。
type Service struct {
	mu    sync.Mutex
	sites map[int]*site
}

func newService() *Service {
	return &Service{sites: map[int]*site{}}
}

// Handlers 返回 RPC 端点（与 coord 协调端点合并到同一 mux）。
func (s *Service) Handlers() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"/start": s.handleStart,
		"/stop":  s.handleStop,
		"/caps":  s.handleCaps,
	}
}

// Stop 平滑停止全部网站服务（OnEmpty / 进程退出时调用）。
func (s *Service) Stop() {
	s.mu.Lock()
	old := s.sites
	s.sites = map[int]*site{}
	s.mu.Unlock()
	for _, st := range old {
		_ = st.srv.Shutdown(context.Background())
	}
}

// handleStart 在指定端口起「静态网站 + 可选反向代理」服务（类似 nginx）。
// args：port（必填，数字）、dir（必填，本地目录）、host（可选，默认 0.0.0.0）、
// proxy（可选，反向代理规则数组：[{path,target,strip_prefix}]，见 proxy.go）。
//
// 多站点语义：同端口 + 同配置 → 幂等返回现状；同端口 + 不同配置 → 报错
// （fail-closed：静默吞掉新参数会让第二个应用「显示成功却没起服」）。
func (s *Service) handleStart(w http.ResponseWriter, r *http.Request) {
	args, err := decodeArgs(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// port=0 是「自动分配」：交给内核挑（listen "host:0"），真实端口从 ln.Addr() 回读。
	// 为什么用内核挑而不是「自己从某基址逐个试」：内核挑到的一定可用，且没有
	// 「试成功→释放→被抢」的 TOCTOU 窗口——自己顺延时，两个应用可能挑到同一个空闲口，
	// 表现为「两个都报成功、一个其实是死端口」，排查成本极高。
	// 真实端口是本文件的**唯一真相**：下面所有用端口的地方（幂等键、url、返回体）
	// 一律读 realPort，不再信任入参 port，避免「入参写 0 却返回 0」这种自欺。
	autoPort := false
	port := intOr(args, "port", 0)
	if port < 0 {
		writeJSON(w, map[string]interface{}{"error": "port 不能为负（0 表示自动分配）"})
		return
	}
	if port == 0 {
		autoPort = true
	}
	dir := strOr(args, "dir", "")
	if dir == "" {
		writeJSON(w, map[string]interface{}{"error": "dir 必填（静态网站根目录，本地路径，传给 ext.serve）"})
		return
	}
	// 可移植：相对 dir 依次按「安装根 / 当前目录」解析（详见 resolveSiteDir）。
	// 绝对 dir 原样校验。fail-closed：解析后仍不存在即报错，绝不静默起空服务。
	resolved, ok := resolveSiteDir(dir, wd(), gearhome.InstallRoot())
	if !ok {
		writeJSON(w, map[string]interface{}{"error": "dir 不存在或不是目录: " + dir})
		return
	}
	dir = resolved
	host := strOr(args, "host", "0.0.0.0")
	// fail-closed：规则写错就报错（缺 path/target、target 非 http(s) URL…），绝不静默丢弃——
	// 静默丢弃正是「UI 的 /api 全 404」这类问题的成因。放在幂等判断之前，保证错误必被报出。
	proxyRules, err := parseProxyRules(args["proxy"])
	if err != nil {
		writeJSON(w, map[string]interface{}{"error": err.Error()})
		return
	}
	proxyJSON := mustJSON(proxyRulesJSON(proxyRules)) // 仅用于幂等/冲突比较
	proxyEcho := proxyRulesJSON(proxyRules)           // 回显给调用方：保持数组形状（verify.py/flow 输出依赖）

	// 从「查重 → 绑定 → 登记」全程持锁：并发 /start 同一端口时只有一个能成功，
	// 另一个要么命中幂等、要么报冲突，不会双双绑定成功或泄露监听套接字。
	s.mu.Lock()
	if !autoPort {
		if old, ok := s.sites[port]; ok {
			if old.dir == dir && old.host == host && mustJSON(proxyRulesJSON(old.proxy)) == proxyJSON {
				s.mu.Unlock()
				writeJSON(w, map[string]interface{}{"result": siteResult(host, port, dir, proxyEcho)})
				return
			}
			conflict := fmt.Sprintf("端口 %d 已被另一站点占用（配置不同）: 当前 dir=%s host=%s proxy=%s；同端口换配置请先 /stop 该端口",
				port, old.dir, old.host, mustJSON(proxyRulesJSON(old.proxy)))
			s.mu.Unlock()
			writeJSON(w, map[string]interface{}{"error": conflict})
			return
		}
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", host, port))
	if err != nil {
		s.mu.Unlock()
		writeJSON(w, map[string]interface{}{"error": fmt.Sprintf("端口 %d 绑定失败: %v", port, err)})
		return
	}
	// 回读真实端口：入参可能是 0（自动分配），而调用方要靠这个数去拼反代 target、
	// 写台账、打印访问地址。返回 0 等于让所有人去猜。
	realPort := port
	if tcp, ok := ln.Addr().(*net.TCPAddr); ok && tcp.Port > 0 {
		realPort = tcp.Port
	}
	st := &site{ln: ln, host: host, dir: dir, proxy: proxyRules, srv: &http.Server{Handler: newSiteHandler(dir, proxyRules)}}
	s.sites[realPort] = st
	s.mu.Unlock()

	go func() {
		if e := st.srv.Serve(ln); e != nil && e != http.ErrServerClosed {
			log.Printf("[serve] 网站服务(端口 %d)异常退出: %v", port, e)
		}
	}()

	writeJSON(w, map[string]interface{}{"result": siteResult(host, realPort, dir, proxyEcho)})
}

// siteURL 返回**可访问地址**——即浏览器/UI 该点的那个 url。
//
// ⚠ host 是「绑定地址」，不是「访问地址」。0.0.0.0 / :: 是 INADDR_ANY（监听所有网卡），
// 不是可路由的目的地址：study 绑 0.0.0.0:8080 时，旧实现返回 http://0.0.0.0:8080/，
// 管理面板把它当链接渲染，点过去打不开（2026-11-13 实测）。通配绑定一律降级为回环；
// 显式写的具体地址（如 192.168.1.23）原样保留——那是用户主动声明的访问入口。
func siteURL(host string, port int) string {
	return fmt.Sprintf("http://%s:%d/", accessHost(host), port)
}

// accessHost 把绑定地址映射为可访问主机名（仅处理通配形态，其余原样返回）。
func accessHost(host string) string {
	switch strings.TrimSpace(host) {
	case "", "0.0.0.0":
		return "127.0.0.1"
	case "::", "[::]":
		return "[::1]"
	}
	return strings.TrimSpace(host)
}

// listenAddr 返回真实绑定地址（如 0.0.0.0:8080），只作展示——它本身不可点。
// 它与 url 必须分开报：合成一个字段就只能二选一（可点 vs 真实监听），
// 而排障与「手机要连」两个场景都要。
func listenAddr(host string, port int) string {
	h := strings.TrimSpace(host)
	if h == "" {
		h = "0.0.0.0"
	}
	if strings.Contains(h, ":") && !strings.HasPrefix(h, "[") {
		h = "[" + h + "]" // IPv6 字面量
	}
	return fmt.Sprintf("%s:%d", h, port)
}

// lanURLs 返回局域网内其它设备（手机）可访问的地址。
//
// 只收私有网段 IPv4：169.254 是链路本地自配（连不上）、公网地址不该暴露、
// 虚拟网卡（VMware/Hyper-V）会列出一串点了没用的假地址。结果去重并排序，
// 多网卡时顺序稳定（面板刷新不会跳来跳去）。拿不到网卡不算错误：返回空，
// 调用方至少还有回环 url 可用。
func lanURLs(port int) []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return []string{}
	}
	seen := map[string]bool{}
	out := []string{}
	for _, ifi := range ifaces {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifi.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipnet.IP.To4()
			if ip == nil || !ip.IsPrivate() {
				continue
			}
			u := fmt.Sprintf("http://%s:%d/", ip.String(), port)
			if seen[u] {
				continue
			}
			seen[u] = true
			out = append(out, u)
		}
	}
	sort.Strings(out)
	return out
}

// siteResult 组装 /start 的返回体（幂等分支与新建分支共用，防两处漂移）。
func siteResult(host string, port int, dir string, proxy interface{}) map[string]interface{} {
	return map[string]interface{}{
		"url":      siteURL(host, port),    // 可访问（通配降级回环）
		"listen":   listenAddr(host, port), // 真实绑定，仅展示
		"lan_urls": lanURLs(port),          // 局域网设备用
		"port":     port,
		"dir":      dir,
		"serving":  true,
		"proxy":    proxy,
	}
}

// mustJSON 序列化为稳定字符串（仅用于「幂等/冲突判定」的比较与展示，失败返回 "null"）。
func mustJSON(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "null"
	}
	return string(b)
}

// handleStop 平滑停止静态网站服务：带 port 只停该站点；不带 port 停全部站点。
func (s *Service) handleStop(w http.ResponseWriter, r *http.Request) {
	// 排空请求体：否则未读的 JSON body 会让 Go http 服务端向（可能复用连接的）客户端发 RST，
	// 表现为「WinError 10054 连接被重置」的偶发失败（handleStart 因 decodeArgs 已读体，无此问题）。
	args, _ := decodeArgs(r)
	port := intOr(args, "port", 0)

	s.mu.Lock()
	var toStop map[int]*site
	if port > 0 {
		if st, ok := s.sites[port]; ok {
			toStop = map[int]*site{port: st}
			delete(s.sites, port)
		}
	} else {
		toStop = s.sites
		s.sites = map[int]*site{}
	}
	s.mu.Unlock()

	stopped := make([]int, 0, len(toStop))
	for p, st := range toStop {
		_ = st.srv.Shutdown(context.Background())
		stopped = append(stopped, p)
	}
	sort.Ints(stopped)
	writeJSON(w, map[string]interface{}{"result": map[string]interface{}{
		"ok": true, "running": len(stopped) > 0, "stopped": stopped,
	}})
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

// wd 返回进程当前工作目录；取不到时退化为 "."（解析相对路径的兜底，fail-closed 由调用方保证）。
func wd() string {
	d, err := os.Getwd()
	if err != nil {
		return "."
	}
	return d
}

// resolveSiteDir 把 /start 收到的 dir 解析为真实存在的目录（可移植锚点核心）。
//
// 规则：
//   - 绝对路径 → 原样校验（存在且是目录才通过）；
//   - 相对路径 → 依次按 installRoot（gear 安装根，apps/ 与 plugin/ 所在的那一层）、
//     cwd 解析，取第一个真实存在的目录。优先 installRoot：应用发布的静态站点随仓库走，
//     相对安装根表达才不依赖「恰好从仓库根启动」。两种都不存在 → 返回 (dir, false)，
//     由调用方报「dir 不存在」错误（fail-closed，绝不拼一个不存在的路径出来）。
//
// 为什么不做「相对 cwd 直接接收」：此前 dir 必须写绝对路径（如 D:/gadern/gear/apps/...），
// 仓库拷到别的机器就跑不起来——这正是 2026-09-25 修复的可移植性根因之一。
func resolveSiteDir(dir, cwd, installRoot string) (string, bool) {
	if dir == "" {
		return "", false
	}
	if filepath.IsAbs(dir) {
		if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
			return dir, true
		}
		return dir, false
	}
	seen := make(map[string]bool)
	for _, base := range []string{installRoot, cwd} {
		if base == "" {
			continue
		}
		cand := filepath.Join(base, dir)
		if seen[cand] {
			continue
		}
		seen[cand] = true
		if fi, err := os.Stat(cand); err == nil && fi.IsDir() {
			return cand, true
		}
	}
	return dir, false
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
			// 占用列表为空 → 无人需要该服务 → 自动退出（先停全部网站释放端口）。
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
