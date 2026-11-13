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
//	POST /api/apps/{id}/logs           子进程批量回推日志行（token 校验）
//	GET  /api/apps/{id}/logs/stream    SSE，只推订阅后新产生的行，不回放历史
//	（列台账走 GET /api/apps：REQ-APP-MANAGED-DETACHED 后与受管 app 是同一份台账。）
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
// --no-ui 的 serve **不写**（2026-11-13 实测缺口）：server.addr 的语义是「管理面的日志
// 回推地址」，但它是「最后一个绑定的 gear serve 说了算」的单文件——而托管应用的后端
// （如 study.api = `gear serve --addr 127.0.0.1:8081 --no-ui`）也是一个 gear serve，
// 绑定后会覆写它，把之后启动的所有 app 的日志回推目标毒化成这个无人订阅的应用后端
// （面板静默空白，live_logs 却仍显示已接入）。--no-ui 明确声明自己不是管理面，理应回避。
//
// best-effort：写失败只告警、绝不阻断起服。实时日志是排障增强，不该让「日志目录不可写」
// 变成「REST 面起不来」；台账里的 live_logs=false 会让 UI 如实说明未接入。
func (s *Server) publishAppLogEndpoint(realAddr string) {
	if s.uiHandler == nil {
		return
	}
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

type appLogIngestLine struct {
	StepID string `json:"step_id,omitempty"`
	Stream string `json:"stream"`
	Line   string `json:"line"`
	TS     int64  `json:"ts"`
}

// toolCallDetailMarker 是「直接调用详情」的行内标记：后接**单行** JSON
// （{"kind","id","ok","ms","input","output"[,"error"]}）。UI 解析它把日志行变成
// 可点击展开的调用详情（入参 + 返回结果）；控制台人看的行不带，保持干净。
//
// json.Marshal 的输出不含字面制表符（控制字符一律转义），所以用 \t 作分隔不会撞上内容。
const toolCallDetailMarker = "\tGEAR_TOOL_CALL="

// maxToolCallDetailBytes 限制单条详情 JSON 的体积：ingest 批量请求体上限 1MB，
// 一条超大输出（如整棵知识图谱）不该把整批日志挤爆。
const maxToolCallDetailBytes = 8192

// marshalToolCallDetail 编码调用详情 JSON。
//
// 只在「回推上下文」启用（GEAR_APP_LOG_ID 已注入 = 这份 stdout 正被 app run 抽干进
// app 日志通道）；前台 serve 的 stdout 是人看的控制台，不带详情。
// 超限时把 input/output 降级为截断字符串预览再试一次，仍超限则放弃详情——
// 宁可少一条详情，不能让一条详情把整个 ingest 批次顶爆（ingest 失败 = 整批静默丢）。
func marshalToolCallDetail(detail map[string]any) ([]byte, bool) {
	if os.Getenv(app.EnvAppLogID) == "" {
		return nil, false // 非回推上下文：控制台行不带 JSON
	}
	b, err := json.Marshal(detail)
	if err != nil {
		return nil, false
	}
	if len(b) <= maxToolCallDetailBytes {
		return b, true
	}
	for _, k := range []string{"input", "output"} {
		if v, ok := detail[k]; ok && v != nil {
			if vb, merr := json.Marshal(v); merr == nil && len(vb) > 2048 {
				detail[k] = string(vb[:2048]) + "…(已截断，原始共 " + fmt.Sprint(len(vb)) + " 字节)"
			}
		}
	}
	if b, err := json.Marshal(detail); err == nil && len(b) <= maxToolCallDetailBytes {
		return b, true
	}
	return nil, false
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
