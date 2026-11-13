package server

// applogs.go —— detached App（gear app start 台账侧）的实时日志回推与订阅（REQ-API-APP-LOGS）。
//
// 与 /api/executions/{id}/logs/stream 的区别：
//   - 执行记录面：日志产生在**本进程**引擎里，组合 sink 直接 Publish 到 broker；
//   - App 面：日志产生在**另一个进程**（detached app run），经 POST ingest 回推后
//     才 Publish 到同一个 broker。两端都不读 <appsDir>/<appID>.log。
//
// 端点：
//
//	GET  /api/apps/records             列出落盘台账 App（供 UI 选订阅对象）
//	POST /api/apps/{id}/logs           子进程批量回推日志行（token 校验）
//	GET  /api/apps/{id}/logs/stream    SSE，只推订阅后新产生的行，不回放历史
//
// 安全口径：执行面整体无鉴权（既定设计，只绑回环），但 ingest 是**写**入口且能往任意
// App 的面板注入内容，所以比读端点多两道：token 常量时间比对 + OriginAllowed（浏览器
// 跨站不能伪造；detached 子进程不带 Origin，自动放行）。token 来自 0600 的 server.addr。

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"

	"gear/internal/app"
	"gear/internal/gearlog"
	"gear/internal/httpsafe"
)

// appLogKey 给 App 日志的 broker key 加前缀，避免与执行记录 id 撞车（两者共用一个 broker）。
func appLogKey(appID string) string { return "app:" + appID }

func (s *Server) appsDirOf() (string, error) {
	return app.ResolveAppsDir(s.registryPath, s.appsDir)
}

// loopbackBaseURL 把 realAddr 规范化成「本机回推用的」回环基址。
//
// 不能直接拿 serverBaseURL(realAddr)：`--allow-remote` 时 realAddr 可能是 0.0.0.0:8787，
// 那不是可连接的目标地址；而 discovery.go 又只放行回环 host。统一收敛到 127.0.0.1
// （IPv6 回环监听时保留 ::1），保证「本机内部回推」与对外监听地址解耦。
func loopbackBaseURL(realAddr string) (string, error) {
	host, port, err := net.SplitHostPort(realAddr)
	if err != nil {
		return "", err
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return (&url.URL{Scheme: "http", Host: net.JoinHostPort(host, port)}).String(), nil
}

// publishAppLogEndpoint 在监听绑定成功（拿到真实端口）后写 <appsDir>/server.addr。
//
// best-effort：写失败只告警、绝不阻断起服。实时日志是排障增强，不该让「日志目录不可写」
// 变成「REST 面起不来」；台账里的 live_logs=false 会让 UI 如实说明未接入。
func (s *Server) publishAppLogEndpoint(realAddr string) {
	dir, err := s.appsDirOf()
	if err != nil {
		gearlog.L().Warn("实时日志地址发现未启用：定位台账目录失败", slog.String("err", fmt.Sprint(err)))
		return
	}
	base, err := loopbackBaseURL(realAddr)
	if err != nil {
		gearlog.L().Warn("实时日志地址发现未启用：监听地址无法解析", slog.String("err", fmt.Sprint(err)))
		return
	}
	if _, err := app.PublishServerEndpoint(dir, base); err != nil {
		gearlog.L().Warn("实时日志地址发现写入失败", slog.String("err", fmt.Sprint(err)))
	}
}

// appRecordDTO 是台账记录在 UI 侧的投影。刻意**不返回日志内容**：
// 页面要的是「实时」，历史在文件里但本端点不读、不回放（用户的硬要求）。
type appRecordDTO struct {
	AppID      string          `json:"app_id"`
	FlowID     string          `json:"flow_id"`
	PID        int             `json:"pid"`
	Restart    string          `json:"restart"`
	StartedAt  time.Time       `json:"started_at"`
	State      string          `json:"state"`
	LogPath    string          `json:"log_path"`
	LiveLogs   bool            `json:"live_logs"`
	Ports      []app.PortState `json:"ports,omitempty"`
	FinalState string          `json:"final_state,omitempty"`
}

// handleListAppRecords 列出 `gear app start` / MCP gear_app(start) 留下的台账应用。
//
// ⚠ 与 GET /api/apps（内存态受管进程）不是同一模型：那一列只活在本进程里，
// 本端点读的是跨会话存活的落盘台账。UI 的「Apps 长驻」页此前只有前者，
// 所以用户真正跑着的后台应用一个都看不到 —— 这是「日志在 UI 上没看见」的根因之一。
func (s *Server) handleListAppRecords(w http.ResponseWriter, r *http.Request) {
	dir, err := s.appsDirOf()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	recs, err := app.ListRecords(dir)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	out := make([]appRecordDTO, 0, len(recs))
	for _, rec := range recs {
		out = append(out, appRecordDTO{
			AppID: rec.AppID, FlowID: rec.FlowID, PID: rec.PID,
			Restart: rec.Restart, StartedAt: rec.StartedAt,
			State: string(rec.Probe()), LogPath: rec.LogPath,
			LiveLogs: rec.LiveLogs, Ports: rec.PortStates(),
			FinalState: rec.FinalState,
		})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"items": out})
}

type appLogIngestLine struct {
	StepID string `json:"step_id,omitempty"`
	Stream string `json:"stream"`
	Line   string `json:"line"`
	TS     int64  `json:"ts"`
}

type appLogIngestReq struct {
	Token string             `json:"token"`
	Lines []appLogIngestLine `json:"lines"`
}

const maxAppLogIngestLines = 200

// handleIngestAppLogs 接收 detached 子进程的批量日志行并 Publish 给订阅者。
//
// 任何失败都直接回错（子进程侧会静默降级，本地日志仍完整），绝不「接受但丢掉」——
// 那会让 UI 上出现一个看似成功、实则永远空白的实时面板。
func (s *Server) handleIngestAppLogs(w http.ResponseWriter, r *http.Request) {
	if !httpsafe.OriginAllowed(r) {
		writeErr(w, http.StatusForbidden, fmt.Errorf("仅允许同源请求回推应用日志"))
		return
	}
	dir, err := s.appsDirOf()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	ep, err := app.ReadServerEndpoint(dir)
	if err != nil {
		writeErr(w, http.StatusForbidden, fmt.Errorf("实时日志回推未启用（server.addr 不可用）: %w", err))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req appLogIngestReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if subtle.ConstantTimeCompare([]byte(req.Token), []byte(ep.Token)) != 1 {
		writeErr(w, http.StatusUnauthorized, fmt.Errorf("实时日志 token 不匹配"))
		return
	}
	id := r.PathValue("id")
	rec, err := app.LoadRecord(dir, id)
	if err != nil {
		if os.IsNotExist(err) {
			writeErr(w, http.StatusNotFound, fmt.Errorf("应用台账不存在: %s", id))
			return
		}
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	_ = rec // 存在性即校验目的；内容不需要。
	if len(req.Lines) > maxAppLogIngestLines {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("单次最多回推 %d 行，实际 %d", maxAppLogIngestLines, len(req.Lines)))
		return
	}
	accepted := 0
	for _, l := range req.Lines {
		if l.Line == "" {
			continue
		}
		stream := l.Stream
		if stream == "" {
			stream = "stdout"
		}
		s.logBroker.Publish(appLogKey(id), l.StepID, stream, l.Line)
		accepted++
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "accepted": accepted})
}

// handleStreamAppLogs 以 SSE 推送一个台账应用的实时日志。
//
// **只推订阅后新产生的行**：不读 <appID>.log、不回放历史（用户明确要求「只展示实时的，
// 不从日志文件拉取」）。数据全部来自内存 LogBroker，来源是该 App 进程 POST 回来的行。
// 应用已结案（StoppedAt / FinalState）时立即发 `event: done` 并关闭，避免客户端空等。
func (s *Server) handleStreamAppLogs(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	dir, err := s.appsDirOf()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	rec, err := app.LoadRecord(dir, id)
	if err != nil {
		if os.IsNotExist(err) {
			writeErr(w, http.StatusNotFound, fmt.Errorf("应用台账不存在: %s", id))
			return
		}
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, fmt.Errorf("当前传输层不支持流式响应"))
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")

	doneFrame := func(status string) {
		fmt.Fprint(w, "event: done\ndata: {\"status\":")
		fmt.Fprintf(w, "%q}\n\n", status)
		flusher.Flush()
	}
	if rec.StoppedAt != nil || rec.FinalState != "" {
		doneFrame("app_closed")
		return
	}

	ch, unsub := s.logBroker.Subscribe(appLogKey(id))
	defer unsub()

	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case ll, ok := <-ch:
			if !ok {
				return
			}
			b, err := json.Marshal(ll)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "data: %s\n\n", b)
			flusher.Flush()
		case <-tick.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
			// 顺带复查台账是否已结案：应用退出后别让 SSE 永远挂着。
			if cur, err := app.LoadRecord(dir, id); err == nil && (cur.StoppedAt != nil || cur.FinalState != "") {
				doneFrame("app_closed")
				return
			}
		}
	}
}
