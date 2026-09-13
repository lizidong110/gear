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
	"sync"
	"time"

	"gear/internal/ai"
	"gear/internal/app"
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
	reg   *registry.Registry
	execs *ExecStore
	mux   *http.ServeMux

	// inferrer 是 AI 闭环（S6c）的推断器：默认线上 SiliconFlow（无 Key 回退 Ollama），测试 / 远端网关可注入。
	inferrer ai.Inferrer

	appMu  sync.Mutex
	apps   map[string]*AppInstance
	appSeq []string
	appWG  sync.WaitGroup // 受管 app 的 goroutine 计数（Shutdown 需等其收敛）
}

// New 构造 Server（不启动监听）。AI 推断默认走线上 SiliconFlow（S6c）；无 Key 回退 Ollama。
func New(reg *registry.Registry) *Server {
	return NewWithInferrer(reg, ai.DefaultInferrer())
}

// NewWithInferrer 与 New 相同，但可注入自定义 Inferrer（测试 / 远端 LLM 网关用）。
func NewWithInferrer(reg *registry.Registry, inf ai.Inferrer) *Server {
	s := &Server{
		reg:      reg,
		execs:    NewExecStore(),
		mux:      http.NewServeMux(),
		apps:     map[string]*AppInstance{},
		inferrer: inf,
	}
	// §8.2 第三条守卫需要「运行期引用」，而运行期状态在 Server 手里 —— 由这里注入，
	// registry 不反向依赖执行状态。
	reg.SetRunningChecker(s.runningToolReason)
	s.routes()
	return s
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

// ListenAndServe 启动 HTTP 服务（阻塞）。
func (s *Server) ListenAndServe(addr string) error {
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
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /api/healthz", s.handleHealthz)

	s.mux.HandleFunc("GET /api/tools", s.handleListTools)
	s.mux.HandleFunc("POST /api/tools", s.handleCreateTool)
	s.mux.HandleFunc("DELETE /api/tools/{id}", s.handleDeleteTool)

	s.mux.HandleFunc("GET /api/flows", s.handleListFlows)
	s.mux.HandleFunc("POST /api/flows", s.handleCreateFlow)
	s.mux.HandleFunc("POST /api/flows/{id}/validate", s.handleValidateFlow)

	s.mux.HandleFunc("GET /api/hooks", s.handleListHooks)
	s.mux.HandleFunc("POST /api/hooks", s.handleCreateHook)

	s.mux.HandleFunc("POST /api/executions", s.handleCreateExecution)
	s.mux.HandleFunc("GET /api/executions", s.handleListExecutions)
	s.mux.HandleFunc("GET /api/executions/{id}", s.handleGetExecution)

	s.mux.HandleFunc("POST /api/ai/infer", s.handleInfer)

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
	// 静态校验（含 Tool 存在性 / when 可编译）——与 CLI、AI 闭环共用同一份规则。
	if err := engine.New(s.reg).ValidateFlowStatic(&f); err != nil {
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
	if err := engine.New(s.reg).ValidateFlowStatic(f); err != nil {
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

type createExecutionReq struct {
	FlowID string `json:"flow_id"`
}

// inferReq 是 POST /api/ai/infer 的请求体（S6c）。
type inferReq struct {
	Prompt string `json:"prompt"`
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
	go func() {
		// 引擎在加载完 Flow + Hook 快照后回报「本次可能用到的 Tool」，
		// 作为 §8.2「运行中不可注销」守卫的判据（静态引用可能已被删/改）。
		res, runErr := engine.New(s.reg).RunFlowResultObserved(rec.FlowID, func(toolIDs []string) {
			s.execs.SetTools(rec.ID, toolIDs)
		})
		s.execs.Finish(rec.ID, res, runErr)
	}()

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

// handleListExecutions 返回全部执行记录（内存态，重启即失；见 ExecStore 注释）。
func (s *Server) handleListExecutions(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.execs.List())
}

// handleInfer 是 AI 闭环（S6c）的推断入口：自然语言 prompt → Flow 定义。
// 推断器可注入（默认本地 Ollama）；生成失败（解析 / 校验不过，重试 3 次仍失败）
// 返回 422，由调用方决定降级策略。
func (s *Server) handleInfer(w http.ResponseWriter, r *http.Request) {
	var req inferReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.Prompt == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("prompt 不能为空"))
		return
	}
	flow, err := ai.GenerateFlow(r.Context(), s.inferrer, req.Prompt)
	if err != nil {
		writeErr(w, http.StatusUnprocessableEntity, err)
		return
	}
	writeJSON(w, http.StatusOK, flow)
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
		sup := app.New(engine.New(s.reg), policy, 0)
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
