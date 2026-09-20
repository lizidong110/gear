// Package server 实现 Gear 的 REST 执行面（PRD §11 最小集）。
//
// 设计取舍：
//   - 用标准库 net/http + Go 1.22+ 的「方法 + 路径变量」路由，**不引 web 框架**：
//     内核要能单二进制分发、依赖最小（与方法论 §5 的取向一致）。
//   - 执行是**异步**的：POST 立即返回 202 + execution id，结果经 GET 查询——
//     这与 PRD §11「触发执行 / 查询执行状态」两个端点的分工一致。
//   - 内存态：ExecStore / AppStore 都是进程内状态（持久性见各自注释的显式声明）。
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"gear/internal/app"
	"gear/internal/cap"
	"gear/internal/engine"
	"gear/internal/model"
	"gear/internal/registry"
)

// AppInstance 是一个由 REST 层托管的受管 Flow 进程（对应 CLI 的 `app run`）。
type AppInstance struct {
	ID        string    `json:"id"`
	FlowID    string    `json:"flow_id"`
	Restart   string    `json:"restart"`
	StartedAt time.Time `json:"started_at"`

	cancel context.CancelFunc
}

// Server 是 REST 执行面。
type Server struct {
	reg    *registry.Registry
	execs  *ExecStore
	engine *engine.Engine
	mux    *http.ServeMux

	// secrets 是进程内密钥源（REQ-API-SECRETS）：管理面经 REST /api/secrets 写入、
	// 启动期经 GEAR_SECRET_* 环境变量播种；注入引擎后运行期只读（ctx.secret.get）。
	secrets *cap.SecretStore

	appMu  sync.Mutex
	apps   map[string]*AppInstance
	appSeq []string
	appWG  sync.WaitGroup // 受管 app 的 goroutine 计数（Shutdown 需等其收敛）

	// gate 是执行入口的全局并发闸门（REQ-CONCURRENCY-CROSS / #73）：
	// 固定 worker 池 = 最大并发执行数，超额排队、再超额在入口拒绝（回 429，背压）。
	// 区别于 S4 step 级 maxParallel（限单个 Flow 内 step 并发）与 S2 app run --max（限 Flow 数）。
	gate          *ConcurrencyGate
	maxConcurrent int // worker 数（并发执行上限）；0 表示取默认
	execQueueSize int // 待执行队列长度；0 表示取默认
}

// ServerOption 调整 Server 的构造期配置（目前仅并发闸门相关）。
type ServerOption func(*Server)

// WithMaxConcurrent 设执行入口「同时跑的 Flow 执行数」上限（即 gate worker 数）。
// 影响的是跨 Flow 的全局并发，不是单 Flow 内 step 并发（那是 S4 maxParallel）。
func WithMaxConcurrent(n int) ServerOption { return func(s *Server) { s.maxConcurrent = n } }

// WithExecQueueSize 设执行入口的有界排队队列长度：worker 全忙时进入排队，
// 队列满再提交即被拒绝（回 429）。0 表示不排队、满即拒。
func WithExecQueueSize(n int) ServerOption { return func(s *Server) { s.execQueueSize = n } }

// New 构造 Server（不启动监听）。支持构造期选项（如 WithMaxConcurrent /
// WithExecQueueSize 调并发闸门，REQ-CONCURRENCY-CROSS / #73）。
// 扩展原语经 extensions.yaml 配置化加载（含内建 ext.registry；无配置回退默认 ext.ai）；
// 配置非法时返回 error（fail-closed，不让引擎带残缺扩展目录起来）。
func New(reg *registry.Registry, opts ...ServerOption) (*Server, error) {
	const (
		defaultMaxConcurrent = 64  // 全局部署默认：单机能同时跑 64 个 Flow 执行
		defaultQueueSize     = 256 // 排队缓冲，避免瞬时抖动直接 429
	)
	s := &Server{
		reg:           reg,
		execs:         NewExecStore(),
		mux:           http.NewServeMux(),
		apps:          map[string]*AppInstance{},
		maxConcurrent: defaultMaxConcurrent,
		execQueueSize: defaultQueueSize,
	}
	for _, o := range opts {
		o(s)
	}
	// 密钥源（REQ-API-SECRETS）：启动期先从 GEAR_SECRET_* 环境变量播种部署凭据，
	// 再注入引擎——声明了 secret capability 的 Tool 运行期 ctx.secret.get 可用，
	// ${secrets.x} 模板可解析；管理面写入经 /api/secrets，运行期只读。
	// 播种失败即 fail-closed 返回 error：GEAR_SECRET_ 前缀下的非法名字（空名/含空白/${}）
	// 会让该密钥**静默缺失**——运维「配了却读不到」且无任何提示，是最难排查的一类故障。
	// cap.SecretStore.SeedFromEnv 刻意返回 error 就是为了这个，此处不得丢弃。
	//
	// 顺序要求：必须排在并发闸门**之前**——闸门的 worker goroutine 在构造期就已启动，
	// 之后再返回 error 会留下永不退出的 goroutine（NewConcurrencyGate 无人 Shutdown）。
	s.secrets = cap.NewSecretStore()
	if err := s.secrets.SeedFromEnv(); err != nil {
		return nil, fmt.Errorf("播种环境变量密钥失败: %w", err)
	}
	// 并发闸门（#73）：执行入口的「进程池」。worker 数 = 最大并发执行数，
	// 待执行任务入有界队列，队列满且 worker 全忙时 Submit 返回 false（入口拒绝、回 429）。
	s.gate = NewConcurrencyGate(s.maxConcurrent, s.execQueueSize)
	// 日志采集（REQ-API-LOGS / #71）：引擎执行 step 时把 stdout/stderr 行经 ExecStore 落盘，
	// 供 GET /api/executions/{id}/logs 查询。
	// 扩展原语目录（PRD §9 去内置化）：配置化加载（extensions.yaml，GEAR_EXT_CONFIG 可覆盖），
	// 始终含内建 ext.registry（读引擎 registry，供 agent 自我锚定）；无配置回退默认 ext.ai。
	// 使声明了 capabilities:[ext.ai] 的 Tool 在 REST 执行面也能调用 AI 能力；sidecar 二进制
	// 缺失时调用返回 ErrCapabilityUnavailable（fail-closed），不影响其它能力。
	capReg := engine.NewCapabilityRegistry()
	agent, err := engine.SetupExtensions(capReg, reg, "")
	if err != nil {
		return nil, fmt.Errorf("初始化扩展失败: %w", err)
	}
	s.engine = engine.New(reg, engine.WithLogSink(s.execs.LogSink()), engine.WithCapabilities(capReg), engine.WithAIAgent(agent), engine.WithSecret(s.secrets))
	// §8.2 第三条守卫需要「运行期引用」，而运行期状态在 Server 手里 —— 由这里注入，
	// registry 不反向依赖执行状态。
	reg.SetRunningChecker(s.runningToolReason)
	s.routes()
	return s, nil
}

// runningToolReason 报告 toolID 是否正被运行中的执行使用（PRD §8.2 第三条守卫）。
//
// 为什么 Registry 的静态引用守卫不够：Hook 被删除、或 Flow 被重新注册去掉该 step 之后，
// Registry 已看不到任何引用，但**在跑的执行**仍持有启动时的快照 —— 此时注销会让它
// 在下次触发 Hook / 执行 step 时拿到 nil Tool 而中途失败。
func (s *Server) runningToolReason(toolID string) string {
	if id, ok := s.execs.RunningToolIDs()[toolID]; ok {
		return fmt.Sprintf("正被运行中的执行 %s 使用", id)
	}
	return ""
}

// Handler 返回可供 httptest / 反向代理使用的 handler。
func (s *Server) Handler() http.Handler { return s.mux }

// serverBaseURL 计算 gear 自身回推基址（供 subscribe 机制拼回推端点 /api/events/{handle}）。
// addr 是 ListenAndServe 的监听地址：":port" 补 127.0.0.1，空回退默认 8787，已带 scheme 前缀则原样。
func serverBaseURL(addr string) string {
	if strings.HasPrefix(addr, "http://") || strings.HasPrefix(addr, "https://") {
		return strings.TrimRight(addr, "/")
	}
	if addr == "" {
		addr = ":8787"
	}
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	return "http://" + addr
}

// ListenAndServe 启动 HTTP 服务（阻塞）。
func (s *Server) ListenAndServe(addr string) error {
	// subscribe 机制的回推回调 URL 由 gear 自身监听地址派生（每个 gear 实例独立），
	// 而非经 env 传给 sidecar（多 gear 并行时单一 env 会串扰）。
	s.engine.SetSubscribeCallbackBase(serverBaseURL(addr))
	srv := &http.Server{
		Addr:              addr,
		Handler:           s.mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	return srv.ListenAndServe()
}

// Shutdown 停止所有受管 app 并**等待其收敛**（供测试与优雅退出使用）。
//
// 必须先等 goroutine 退出再让调用方关闭 registry：否则 Supervisor 可能还在读库，
// 导致 `sql: database is closed` 这类停机竞态。
func (s *Server) Shutdown() {
	s.appMu.Lock()
	for _, a := range s.apps {
		a.cancel()
	}
	s.apps = map[string]*AppInstance{}
	s.appSeq = nil
	s.appMu.Unlock()
	s.appWG.Wait()
	// 先收敛执行入口闸门：关闭队列并等全部 worker 退出，确保「在途执行」已落终态
	// （RunFlowExec → execs.Finish）后再释放 ExecStore 句柄，避免 Finish 写到已关的库。
	if s.gate != nil {
		s.gate.Stop()
	}
	// 释放执行记录库句柄（#70 SQLite 持久化）：停机后不再查询，关闭以解锁 WAL 侧车文件。
	_ = s.execs.Close()
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /api/healthz", s.handleHealthz)
	s.mux.HandleFunc("GET /api/news", s.handleNews)
	s.mux.HandleFunc("GET /api/capabilities", s.handleCapabilities)
	s.mux.HandleFunc("POST /api/capabilities/invoke", s.handleInvokeCapability)

	s.mux.HandleFunc("GET /api/secrets", s.handleListSecrets)
	s.mux.HandleFunc("PUT /api/secrets/{name}", s.handleSetSecret)
	s.mux.HandleFunc("DELETE /api/secrets/{name}", s.handleDeleteSecret)

	s.mux.HandleFunc("GET /api/tools", s.handleListTools)
	s.mux.HandleFunc("GET /api/tools/{id}", s.handleGetTool)
	s.mux.HandleFunc("POST /api/tools", s.handleCreateTool)
	s.mux.HandleFunc("DELETE /api/tools/{id}", s.handleDeleteTool)
	s.mux.HandleFunc("POST /api/tools/{id}/versions/{v}/promote", s.handlePromoteVersion)

	s.mux.HandleFunc("GET /api/flows", s.handleListFlows)
	s.mux.HandleFunc("POST /api/flows", s.handleCreateFlow)
	s.mux.HandleFunc("DELETE /api/flows/{id}", s.handleDeleteFlow)
	s.mux.HandleFunc("POST /api/flows/{id}/validate", s.handleValidateFlow)

	s.mux.HandleFunc("GET /api/hooks", s.handleListHooks)
	s.mux.HandleFunc("POST /api/hooks", s.handleCreateHook)
	s.mux.HandleFunc("DELETE /api/hooks/{id}", s.handleDeleteHook)

	s.mux.HandleFunc("POST /api/executions", s.handleCreateExecution)
	s.mux.HandleFunc("GET /api/executions", s.handleListExecutions)
	s.mux.HandleFunc("GET /api/executions/{id}", s.handleGetExecution)
	s.mux.HandleFunc("GET /api/executions/{id}/logs", s.handleGetExecutionLogs)

	s.mux.HandleFunc("POST /api/events/{handle}", s.handleSubscriptionEvent)

	s.mux.HandleFunc("POST /api/apps", s.handleCreateApp)
	s.mux.HandleFunc("GET /api/apps", s.handleListApps)
	s.mux.HandleFunc("DELETE /api/apps/{id}", s.handleDeleteApp)
}

// ---- 通用响应helper ----

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func decodeJSON(r *http.Request, v interface{}) error {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		return fmt.Errorf("请求体不是合法 JSON: %w", err)
	}
	return nil
}

// statusForErr 把 registry 的语义化错误映射为 HTTP 状态码。
func statusForErr(err error) int {
	switch {
	case errors.Is(err, registry.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, registry.ErrInUse):
		return http.StatusConflict
	default:
		return http.StatusBadRequest
	}
}

// ---- handlers ----

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	tools, err := s.reg.ListTools()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	flows, err := s.reg.ListFlows()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status": "ok",
		"tools":  len(tools),
		"flows":  len(flows),
	})
}

// handleCapabilities 返回当前实时「原子能力目录」：内核原语（builtin + pure）+ 已注册 ext.*，
// 每个能力含方法级参数 schema（type/description/required）。人与 web-ui 用同一份
// engine.AIAgent().CapabilitiesMethod() 数据源——即 CLI `gear ai capabilities` 的 REST 入口，
// 直接回答「有哪些原子能力 / 扩展了哪些 / 各能力要什么参数」（用户决策：能力必须可查询、可扩展）。
// 能力目录只读、无副作用，失败仅回 500。
func (s *Server) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	agent := s.engine.AIAgent()
	if agent == nil {
		writeErr(w, http.StatusInternalServerError, fmt.Errorf("扩展目录未初始化"))
		return
	}
	caps, err := agent.CapabilitiesMethod()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if caps == nil {
		caps = []map[string]interface{}{}
	}
	writeJSON(w, http.StatusOK, caps)
}

// invokeCapabilityReq 是 POST /api/capabilities/invoke 的请求体：直接执行一个原子能力的方法。
// args 以「参数名为键」的 map（与 capabilities() 的 schema 对齐）——内置原语经
// builtinParamOrder 重排为位置参数、ext.* 原样作为单 table 传入。
type invokeCapabilityReq struct {
	Name   string                 `json:"name"`
	Method string                 `json:"method"`
	Args   map[string]interface{} `json:"args"`
}

// handleInvokeCapability 直接执行一个原子能力的方法（「能力目录」页调用测试用）。
// 与 react 同权限（全量宿主授权 + Lua 沙箱）：db/kv 为内存态、secret 需内核已注入密钥源、
// http 可访问任意 URL。未知能力名 / 方法在引擎侧 fail-closed 报错，不静默执行。
func (s *Server) handleInvokeCapability(w http.ResponseWriter, r *http.Request) {
	var req invokeCapabilityReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.Name == "" || req.Method == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("name 与 method 均不能为空"))
		return
	}
	res, err := s.engine.InvokeCapability(req.Name, req.Method, req.Args)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"result": res})
}

// handleNews 经引擎同步运行 news.top10 Flow（抓取并整理百度热搜榜 Top10），
// 直接把结果作为 HTTP 响应返回——这是「Flow + Lua 工具直接调用 ctx.http 干活」的对外服务化示例。
// Flow 未注册（如未用 `gear serve --seed examples/news-top10.yaml`）→ 404。
func (s *Server) handleNews(w http.ResponseWriter, r *http.Request) {
	res, err := s.engine.RunFlowResult("news.top10")
	if err != nil {
		writeErr(w, http.StatusNotFound, fmt.Errorf("运行 news.top10 失败（是否用 `gear serve --seed examples/news-top10.yaml` 注册了示例?）: %w", err))
		return
	}
	gen, ok := res.Steps["gen"]
	if !ok {
		writeErr(w, http.StatusInternalServerError, fmt.Errorf("news.top10 缺少 step gen 输出"))
		return
	}
	out, _ := gen["output"].(map[string]interface{})
	if out == nil {
		writeErr(w, http.StatusInternalServerError, fmt.Errorf("step gen 无 output: %v", gen))
		return
	}
	if errMsg, _ := out["error"].(string); errMsg != "" {
		writeErr(w, http.StatusBadGateway, fmt.Errorf("news.fetch 失败: %s", errMsg))
		return
	}
	out["served_at"] = time.Now().UTC().Format(time.RFC3339)
	writeJSON(w, http.StatusOK, out)
}

// handleSubscriptionEvent 是 subscribe 机制的回推端点（mqtt-sidecar 收到消息后 POST 到
// /api/events/{handle}）。按 handle 定位订阅，取出 payload 交引擎投递（run/notify）。
// 这是「回调 URL 不走 sidecar env」的关键：每个 gear 用自己的 --addr 派生回推地址，
// sidecar 只维护 handle -> callbackURL 映射，多 gear 并行互不串扰。
func (s *Server) handleSubscriptionEvent(w http.ResponseWriter, r *http.Request) {
	handle := r.PathValue("handle")
	var body struct {
		Payload interface{} `json:"payload"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.engine.Deliver(handle, body.Payload); err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleListTools(w http.ResponseWriter, r *http.Request) {
	tools, err := s.reg.ListTools()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if tools == nil {
		tools = []*model.Tool{}
	}
	writeJSON(w, http.StatusOK, tools)
}

// handleGetTool 返回单个 Tool 的完整定义（含 versions）。不存在 → 404。
//
// 此前 UI 只能「拉全量列表再本地索引」——那时内核确实没有单查端点。现在补上，
// 供按 id 直取（如执行详情里点某个 tool 想看完整定义）而不必先取全表。
func (s *Server) handleGetTool(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	t, err := s.reg.GetTool(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if t == nil {
		writeErr(w, http.StatusNotFound, fmt.Errorf("Tool 不存在: %s: %w", id, registry.ErrNotFound))
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (s *Server) handleCreateTool(w http.ResponseWriter, r *http.Request) {
	var t model.Tool
	if err := decodeJSON(r, &t); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.reg.UpsertTool(&t); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, t)
}

func (s *Server) handleDeleteTool(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.reg.DeleteTool(id); err != nil {
		writeErr(w, statusForErr(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"deleted": id})
}

// promoteReq 是 POST /api/tools/{id}/versions/{v}/promote 的请求体（S5 灰度 REST 端点）。
type promoteReq struct {
	Target string `json:"target"`
}

// handlePromoteVersion 晋升某 Tool 版本到目标状态（S5 灰度 REST 端点，对应 CLI `tool promote`）。
// 走 engine.PromoteTool：先触发 before_promote Hook 门禁（fail-closed），再落库；
// 状态迁移合法性由 model.CanPromote 在 registry 层校验。
func (s *Server) handlePromoteVersion(w http.ResponseWriter, r *http.Request) {
	toolID := r.PathValue("id")
	version := r.PathValue("v")
	var req promoteReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.Target == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("target 不能为空"))
		return
	}
	target := model.ToolVersionState(req.Target)
	if !model.ValidStates[target] {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("非法目标状态: %q（应为 draft/testing/stable/deprecated/rejected）", req.Target))
		return
	}
	if err := s.engine.PromoteTool(toolID, version, target); err != nil {
		writeErr(w, statusForErr(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"tool_id": toolID,
		"version": version,
		"state":   string(target),
	})
}

func (s *Server) handleListFlows(w http.ResponseWriter, r *http.Request) {
	flows, err := s.reg.ListFlows()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if flows == nil {
		flows = []*model.Flow{}
	}
	writeJSON(w, http.StatusOK, flows)
}

func (s *Server) handleCreateFlow(w http.ResponseWriter, r *http.Request) {
	var f model.Flow
	if err := decodeJSON(r, &f); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	// 静态校验（含 Tool/原子能力存在性、when 可编译）——与 CLI、AI 闭环共用同一份规则。
	// 必须用**已装配的** s.engine：扩展目录（capReg）与 ext.ai 都挂在它上面，
	// 若在此新建一次性 engine，注册期就看不到任何 ext.*，判定会比运行期更宽/更严（两处漂移）。
	if err := s.engine.ValidateFlowStatic(&f); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.reg.UpsertFlow(&f); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, f)
}

func (s *Server) handleValidateFlow(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	f, err := s.reg.GetFlow(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if f == nil {
		writeErr(w, http.StatusNotFound, fmt.Errorf("Flow 不存在: %s: %w", id, registry.ErrNotFound))
		return
	}
	if err := s.engine.ValidateFlowStatic(f); err != nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{"valid": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"valid": true})
}

func (s *Server) handleListHooks(w http.ResponseWriter, r *http.Request) {
	hooks, err := s.reg.ListHooks()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if hooks == nil {
		hooks = []*model.Hook{}
	}
	writeJSON(w, http.StatusOK, hooks)
}

func (s *Server) handleCreateHook(w http.ResponseWriter, r *http.Request) {
	var h model.Hook
	if err := decodeJSON(r, &h); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.reg.UpsertHook(&h); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, h)
}

// handleDeleteFlow 注销 Flow。守卫全部在 registry.DeleteFlow 里（PRD §8.2 三条）：
// 被其它 Flow 的 step 引用、被 Flow 级 Hook 的 action 引用、或正被运行中的执行作为
// Hook 载体使用 → 拒绝（409）；不存在 → 404。REST 层只做「错误 → 状态码」映射，
// 不重复实现判定（守卫必须只有一份，否则 CLI 与 REST 会漂移）。
func (s *Server) handleDeleteFlow(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.reg.DeleteFlow(id); err != nil {
		writeErr(w, statusForErr(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"deleted": id})
}

// handleDeleteHook 删除一个 Hook。与 Tool/Flow 不同，Hook 不被其它定义引用
// （它只是 stage+target 的匹配关系），所以没有引用守卫；但「不存在」仍报 404，
// 不静默 no-op。
func (s *Server) handleDeleteHook(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.reg.DeleteHook(id); err != nil {
		writeErr(w, statusForErr(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"deleted": id})
}

type createExecutionReq struct {
	FlowID string `json:"flow_id"`
}

func (s *Server) handleCreateExecution(w http.ResponseWriter, r *http.Request) {
	var req createExecutionReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.FlowID == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("flow_id 不能为空"))
		return
	}
	f, err := s.reg.GetFlow(req.FlowID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if f == nil {
		writeErr(w, http.StatusNotFound, fmt.Errorf("Flow 不存在: %s: %w", req.FlowID, registry.ErrNotFound))
		return
	}

	rec := s.execs.Create(req.FlowID)
	// 经并发闸门派发（REQ-CONCURRENCY-CROSS / #73）：worker 全忙且队列已满 → 拒绝（背压），
	// 执行记录置 rejected 并回 429，由调用方决定退避重试，而非放任高扇出打爆机器。
	job := func() {
		// 引擎在加载完 Flow + Hook 快照后回报「本次可能用到的 Tool」，
		// 作为 §8.2「运行中不可注销」守卫的判据（静态引用可能已被删/改）。
		// RunFlowExec 带上 exec id，使 step 运行日志经 logSink 落盘（REQ-API-LOGS / #71）。
		res, runErr := s.engine.RunFlowExec(rec.FlowID, rec.ID, func(toolIDs []string) {
			s.execs.SetTools(rec.ID, toolIDs)
		})
		s.execs.Finish(rec.ID, res, runErr)
	}
	if !s.gate.Submit(job) {
		const reason = "执行入口繁忙：并发已达上限且队列已满"
		s.execs.Reject(rec.ID, reason)
		writeJSON(w, http.StatusTooManyRequests, map[string]string{
			"id":     rec.ID,
			"status": StatusRejected,
			"error":  reason + "，请稍后重试",
		})
		return
	}

	writeJSON(w, http.StatusAccepted, map[string]string{"id": rec.ID, "status": rec.Status})
}

func (s *Server) handleGetExecution(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rec, ok := s.execs.Get(id)
	if !ok {
		writeErr(w, http.StatusNotFound, fmt.Errorf("执行记录不存在: %s: %w", id, registry.ErrNotFound))
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

// handleGetExecutionLogs 返回某次执行的 step 运行日志（stdout/stderr 行，按产生顺序）。
// 执行记录不存在 → 404；存在但无日志 → 返回空数组（200）。
func (s *Server) handleGetExecutionLogs(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.execs.Get(id); !ok {
		writeErr(w, http.StatusNotFound, fmt.Errorf("执行记录不存在: %s: %w", id, registry.ErrNotFound))
		return
	}
	writeJSON(w, http.StatusOK, s.execs.Logs(id))
}

// handleListExecutions 返回全部执行记录（持久化于 SQLite，重启可恢复；见 ExecStore）。
func (s *Server) handleListExecutions(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.execs.List())
}

type createAppReq struct {
	FlowID  string `json:"flow_id"`
	Restart string `json:"restart"` // always（默认）/ never
}

func (s *Server) handleCreateApp(w http.ResponseWriter, r *http.Request) {
	var req createAppReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.FlowID == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("flow_id 不能为空"))
		return
	}
	f, err := s.reg.GetFlow(req.FlowID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if f == nil {
		writeErr(w, http.StatusNotFound, fmt.Errorf("Flow 不存在: %s: %w", req.FlowID, registry.ErrNotFound))
		return
	}
	policy := app.RestartAlways
	switch app.RestartPolicy(req.Restart) {
	case app.RestartNever:
		policy = app.RestartNever
	case app.RestartAlways, "":
		policy = app.RestartAlways
	default:
		writeErr(w, http.StatusBadRequest, fmt.Errorf("非法的 restart=%q（应为 always 或 never）", req.Restart))
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	inst := &AppInstance{
		ID: newID("app_"), FlowID: req.FlowID,
		Restart: string(policy), StartedAt: time.Now(), cancel: cancel,
	}
	s.appMu.Lock()
	s.apps[inst.ID] = inst
	s.appSeq = append(s.appSeq, inst.ID)
	s.appMu.Unlock()

	s.appWG.Add(1)
	go func() {
		defer s.appWG.Done()
		// 复用已装配的 s.engine：service step 同样需要扩展目录 / 密钥源 / 宿主原语，
		// 新建一次性 engine 会让「经 Apps 拉起的 Flow」比「经 /api/executions 跑的同一个 Flow」能力更弱。
		sup := app.New(s.engine, policy, 0)
		if err := sup.Supervise(ctx, []string{req.FlowID}); err != nil {
			fmt.Fprintf(os.Stderr, "app %s(%s) 结束: %v\n", inst.ID, req.FlowID, err)
		}
	}()

	writeJSON(w, http.StatusCreated, inst)
}

func (s *Server) handleListApps(w http.ResponseWriter, r *http.Request) {
	s.appMu.Lock()
	out := make([]*AppInstance, 0, len(s.appSeq))
	for _, id := range s.appSeq {
		if a, ok := s.apps[id]; ok {
			out = append(out, a)
		}
	}
	s.appMu.Unlock()
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleDeleteApp(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.appMu.Lock()
	a, ok := s.apps[id]
	if ok {
		delete(s.apps, id)
	}
	s.appMu.Unlock()
	if !ok {
		writeErr(w, http.StatusNotFound, fmt.Errorf("app 不存在: %s: %w", id, registry.ErrNotFound))
		return
	}
	a.cancel() // 取消 ctx ⇒ Supervisor 停止并杀掉所有 service 子进程
	writeJSON(w, http.StatusOK, map[string]string{"stopped": id})
}
