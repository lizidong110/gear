// Package mcp 实现 Gear 的第四种执行面：MCP (Model Context Protocol) Server。
//
// 动机：让任何支持 MCP 的智能体（WorkBuddy、Claude Desktop、Cursor…）以对话方式完成
// 「读目录 → 写配置 → 校验 → 跑起来 → 看结果」——即**创建并运行一个应用**。
//
// 工具面（12 个，统一 gear_ 前缀，意图级而非资源级；设计见 MCP_AGENT_DESIGN.md §6）：
//
//	gear_guide        按主题取手册（L1 上下文层，与其他两处同源于 guide 包的 embed）
//	gear_catalog      看现有什么（**摘要**，不吐全量 schema）
//	gear_describe     看某对象完整定义
//	gear_capabilities 能力目录（内核原语 + ext.*，含方法级参数 schema）
//	gear_validate     校验 bundle YAML（不落库，字段级诊断）
//	gear_apply        声明式落库（原子、幂等、可选 dry_run）
//	gear_delete       删对象（守卫语义不变）
//	gear_promote      晋升 Tool 版本
//	gear_run          跑一次性编排（flow 异步 + wait_seconds）或单个 Tool
//	gear_execution    执行记录与日志
//	gear_app          长驻应用生命周期（start/stop/status/list）
//	gear_doctor       环境自检（侧车/端口/台账/遗留）
//
// 工具实现分文件：tools_read.go（只读 5 个）/ tools_write.go（写 4 个）/ tools_run.go（执行 2 个）/
// tools_app.go（应用 1 个）；公共部件（结构化错误、入参类型、输出 schema）在 common.go。
//
// 设计取舍：
//   - MCP 只做**协议翻译**：删除守卫（PRD §8.2）、schema 校验、版本状态机、静态校验全部在
//     registry/engine/app 层，本包不重复实现（守卫必须只有一份，否则各执行面会漂移）。
//   - **能力原语的边界**（§9.2 四类）：受门禁 6（http/call/db/secret/event/subscribe）、
//     免门禁纯函数 3（time/template/crypto，声明了反而被拒）、`json` 只是 Lua 绑定不是能力、
//     `ext.*` 须注册且须声明。这四条最容易写错，`gear_guide("atoms")` 有完整表。
//   - **长驻子进程不得出现在本进程树内**（§9.1 第 5 条）：`gear_app(start)` 派生 detach 的
//     `gear app run` 后立即返回，**从不持有**；进程归属那个 app run 自己（PRD §14.1）。
//   - **背景信息只写一次**：心智模型进 ServerOptions.Instructions（guide/summary.md），
//     全字段语义进 `gear_guide` 的返回值，工具 Description 只留「做什么 + 何时用 + 歧义消解」
//     （单条 ≤240 字符、合计 ≤1,800 字符，有机械守卫）。见 §4.1。
//   - **返回形态按规范最严口径发（事故教训）**：MCP 2025-06-18 规定 `tools/call` 的
//     `structuredContent` 顶层**必须是 JSON 对象**。go-sdk v1.8.0 依 SEP-2106 草案放行
//     数组/标量，且自带客户端同样宽松 → 本地冒烟全绿，但接**严格客户端**（WorkBuddy 用
//     zod 校验）时该字段被拒（`expected record, received array`），整条 tools/call 被
//     丢弃，表现为「一个工具都用不了」。故列表类结果一律经 `objectResult` 包一层具名键，
//     **绝不裸发数组**。
//   - 默认 stdio transport：客户端配置 command=gear, args=[mcp]。stdio 下 stdout 只能承载
//     协议帧，任何日志/警告必须走 stderr。
package mcp

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"gear/internal/app"
	"gear/internal/cap"
	"gear/internal/engine"
	"gear/internal/execstore"
	"gear/internal/httpsafe"
	"gear/internal/mcp/guide"
	"gear/internal/model"
	"gear/internal/registry"
)

// Version 是 MCP Server 汇报给客户端的实现版本号。
//
// 0.2.0：工具面由 19 个资源级工具收敛为 12 个意图级工具并统一 `gear_` 前缀（**破坏性变更**，
// 见 MCP_AGENT_DESIGN.md §6.2⑦）。升版本号让客户端能分辨新旧工具面。
const Version = "0.2.0"

// Server 是 MCP 执行面：持有与 REST 面同构的 registry + 引擎 + 执行记录存储。
type Server struct {
	reg    *registry.Registry
	engine *engine.Engine
	execs  *execstore.ExecStore

	// registryPath 是 registry 数据库路径：gear_app 派生子进程时必须显式继承它
	// （否则子进程读默认 ~/.gear/registry.db，可能启动另一个库里的同名 Flow）。
	registryPath string
	// appsDir 是应用台账目录；空 = <registry 同目录>/apps。
	appsDir string
	// exposeFlows 控制是否把已注册 Flow 反向暴露为 flow__{id} 工具（默认关闭）。
	// 关闭理由：启动时快照（新建 Flow 本次不可用）、与 gear_run 语义重复、工具数不可预测，
	// 三者都损害工具选择准确率（§1「负资产」）。
	exposeFlows bool
	// allowRemote 允许监听非回环地址（默认 false = 只许 127.0.0.1）。
	// MCP 面当前无鉴权，绑全网卡等于把「写 registry / 删对象 / 执行编排」的能力暴露给局域网，
	// 故默认 fail-closed，需要远程接入必须显式打开（评审 B-2）。
	allowRemote bool

	// execSem 是**执行的有界并发闸门**（评审 I-1）：每条 gear_run / flow__ 执行占一个槽，
	// 满了直接拒绝（背压）而不是排无限长的队——否则一个客户端能瞬间打满 goroutine。
	execSem chan struct{}
	// execWG 追踪在途执行，Close 时等待收敛：否则进程退出会强杀在途 Flow，执行记录永久
	// 停在 running（gear_execution 里出现永不结束的僵尸记录）。
	execWG sync.WaitGroup
}

// maxConcurrentExecs 是 MCP 面同时在跑的 Flow 执行上限（对齐 REST 面 ConcurrencyGate 的思路：
// 有界并发 + 满了背压）。取值与 REST 的默认闸门同量级即可，MCP 不是吞吐主战场。
const maxConcurrentExecs = 8

// Option 定制 MCP Server（保持 New 的向后兼容：既有调用方 New(reg, eng) 不变）。
type Option func(*Server)

// WithRegistryPath 指定 registry 路径（gear_app 派生进程时继承它）。
func WithRegistryPath(p string) Option { return func(s *Server) { s.registryPath = p } }

// WithAppsDir 指定应用台账目录（默认 <registry 同目录>/apps）。
func WithAppsDir(d string) Option { return func(s *Server) { s.appsDir = d } }

// WithExposeFlows 开启 Flow 反向暴露（flow__{id}）。
func WithExposeFlows(b bool) Option { return func(s *Server) { s.exposeFlows = b } }

// WithAllowRemote 允许 MCP 的 streamable HTTP transport 监听非回环地址。
//
// 默认 false：无鉴权地把写/删/执行能力绑到 0.0.0.0 是不负责任的默认项，
// 需要远程接入时必须由使用者显式打开这层开关（并自行承担暴露面）。
func WithAllowRemote(b bool) Option { return func(s *Server) { s.allowRemote = b } }

// New 构造 MCP Server。registry 与引擎由调用方装配（CLI 用 cliEngine，
// 与 REST 面同一套扩展加载），这里补上执行记录存储与运行期注销守卫。
//
// ⚠ 契约：调用方**必须先完成扩展加载**（`engine.SetupExtensions`，其副作用是把
// extensions.yaml 里的 ext.* 名并入 `cap` 包的全局合法 capability 集合）。本函数
// 用 extNamesReady 显式校验这一点并 fail-closed —— 不要依赖「cliEngine 恰好先跑过」
// 的隐式顺序：registry.UpsertTool → model.Tool.Validate → cap.GrantsFromStrings
// 会用那个全局集合判定 capability，缺了它，声明 ext.* 的 Tool 会被误判「未知 capability」。
func New(reg *registry.Registry, eng *engine.Engine, opts ...Option) (*Server, error) {
	if err := extNamesReady(); err != nil {
		return nil, err
	}
	// instructions 是 L0 注入层的内容主体。它读不到就说明构建产物缺了 embed，
	// 此时**必须硬失败**——静默发一份没有 instructions 的工具面，等于让智能体
	// 退回「从 12 段描述里反推架构」的状态，而这正是本方案要解决的问题。
	if guide.Summary() == "" {
		return nil, fmt.Errorf("MCP Server 装配失败：guide/summary.md 未嵌入（instructions 为空）")
	}
	s := &Server{reg: reg, engine: eng, execs: execstore.NewExecStore(), execSem: make(chan struct{}, maxConcurrentExecs)}
	for _, o := range opts {
		o(s)
	}
	// 执行进度改道 stderr —— **在这里做，不在 CLI**。
	//
	// 理由：sink 是「MCP 面的 stdout 只能承载协议帧」这条**协议约束**的落实，不是 CLI 的
	// 使用偏好。放在 `gear mcp` 里等于把保证押在「每个 embedding 都记得补这一行」上，
	// 而引擎默认写 os.Stdout（CLI `gear exec` 的用户体验，不能动）——stdio transport 下
	// 一行 `✓ step …` 就足以让严格客户端解析失败（2026-09-21 真实事故，见
	// REQ-MCP-STDIO-PURE）。谁拥有协议通道，谁负责保持它干净。
	eng.SetProgressWriter(os.Stderr)
	// §8.2 第三条守卫需要「运行期引用」：与 REST 面同款注入——
	// MCP 面发起的执行同样会被 DeleteTool 守卫看见。
	reg.SetRunningChecker(s.runningToolReason)
	return s, nil
}

// extNamesReady 校验扩展名已并入全局 capability 集合（SetupExtensions 的副作用）。
//
// 判据：环境未配置任何 extensions.yaml 时，内核**始终注册 ext.ai**（默认回退路径），
// 所以「ext.ai 是否 Known」就等价于「SetupExtensions 是否已跑过」。
// 返回错误即为装配顺序错误（fail-closed，而不是等到 gear_apply 时报一个误导性的
// 「未知的 capability」）。
func extNamesReady() error {
	if !cap.Primitive("ext.ai").Known() {
		return fmt.Errorf("MCP Server 装配顺序错误：扩展尚未加载（需先调用 engine.SetupExtensions 再 New）——" +
			"否则声明 ext.* capability 的 Tool 会被误判为「未知的 capability」")
	}
	return nil
}

// Close 释放执行记录库句柄。
//
// ★ 先等在途执行收敛再关库（评审 I-1）：否则在途 Flow 的 Finish 会写到一个已关闭的库上，
// 且执行记录会永久停在 running。等待上界由各 step 的执行超时（默认 30s）自然给出。
func (s *Server) Close() error {
	s.execWG.Wait()
	return s.execs.Close()
}

// newMCPServer 构造底层 SDK server（stdio 与 HTTP 两个 transport 共用同一套工具面与
// instructions——否则「本地 stdio 能用、HTTP 接进来的智能体看不到手册」）。
func (s *Server) newMCPServer() (*mcp.Server, error) {
	ms := mcp.NewServer(&mcp.Implementation{Name: "gear", Version: Version},
		&mcp.ServerOptions{Instructions: guide.Summary()})
	if err := s.register(ms); err != nil {
		return nil, err
	}
	s.registerResources(ms)
	return ms, nil
}

// Run 在给定 transport 上运行 MCP Server（阻塞直到客户端断开）。
func (s *Server) Run(ctx context.Context, transport mcp.Transport) error {
	ms, err := s.newMCPServer()
	if err != nil {
		return err
	}
	return ms.Run(ctx, transport)
}

// httpHandler 构造 streamable HTTP transport 的 http.Handler（与 stdio 共用同一套工具面）。
//
// ★ Origin 校验（2026-09-24 补，评审 B-2）：与 REST 面**共用** httpsafe.OriginAllowed。
//
//	此前这里裸挂 handler、没有任何来源校验，而 MCP 工具面含 gear_apply（写 registry）、
//	gear_delete（删对象）、gear_run（执行编排）——同一个进程暴露的两个面**安全水位不一致**。
//	关键认知：「只绑 127.0.0.1」挡不住浏览器——任意网页都能向回环地址发请求，且浏览器
//	会自动带上 Origin 头。没有这条判定，回环绑定在浏览器面前形同虚设。
func (s *Server) httpHandler() (http.Handler, error) {
	ms, err := s.newMCPServer()
	if err != nil {
		return nil, err
	}
	inner := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return ms }, nil)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !httpsafe.OriginAllowed(r) {
			http.Error(w, "cross-origin request denied: MCP 面只接受本机来源", http.StatusForbidden)
			return
		}
		inner.ServeHTTP(w, r)
	}), nil
}

// checkBindAddr 校验 MCP 的监听地址：非回环地址必须显式开 allowRemote。
//
// 为什么 fail-closed：MCP 面无鉴权，绑到 0.0.0.0 等于把「写 registry / 删对象 / 执行编排」
// 的能力暴露给局域网。此前代码注释写着「只应绑 127.0.0.1」、文档给的示例却是 `--http :8788`
// （绑全网卡），而 ServeHTTP 从不校验——安全建议形同虚设（评审 B-2）。
func checkBindAddr(addr string, allowRemote bool) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("MCP 监听地址 %q 非法（应为 host:port）: %w", addr, err)
	}
	if isLoopbackHost(host) {
		return nil
	}
	if allowRemote {
		// 放行，但把代价说清楚——静默放行等于替使用者做了一个他们不知情的决定。
		fmt.Fprintf(os.Stderr,
			"gear mcp: ⚠ 监听非回环地址 %s（当前无鉴权：任何能访问该地址的人都可调用 gear_apply / gear_delete / gear_run）\n", addr)
		return nil
	}
	return fmt.Errorf("拒绝监听非回环地址 %q：MCP 面当前无鉴权，绑全网卡会把「写 registry / 删对象 / 执行编排」"+
		"暴露给局域网；请改用 127.0.0.1:<port>（确有远程需求时显式加 --allow-remote）", addr)
}

// isLoopbackHost 判定 host 是否回环。空 host（`:8788`）视为**全网卡**，不算回环。
func isLoopbackHost(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	switch h {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// ServeHTTP 在 addr（host:port）以 streamable HTTP transport 暴露 MCP Server，
// 阻塞直到退出信号（SIGINT/SIGTERM）或 ctx 取消。
//
// ⚠ 当前无鉴权：默认只接受回环地址（非回环需 WithAllowRemote 显式打开），
// 并对带 Origin 的跨站请求做同源判定（见 httpHandler 的论证）。
func (s *Server) ServeHTTP(ctx context.Context, addr string) error {
	// 先挡住「绑全网卡」——这一步必须在 ListenAndServe 之前，否则等它监听上了再说就晚了。
	if err := checkBindAddr(addr, s.allowRemote); err != nil {
		return err
	}
	h, err := s.httpHandler()
	if err != nil {
		return err
	}
	// 超时只设「读」方向：**不设 WriteTimeout**，否则会截断 streamable HTTP 的长连接与
	// 流式响应（SSE 一条流可能要跑几十秒）；IdleTimeout 同理从宽。
	srv := &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
	}
	ctx, cancel := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer cancel()
	go func() {
		<-ctx.Done()
		_ = srv.Shutdown(context.Background())
	}()
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// registerResources 注册 `gear://guide/{topic}` 资源。
//
// 它与 gear_guide 工具、instructions 渲染自**同一个 embed.FS**（guide 包）——
// 三处同源，杜绝「instructions 说的和 guide 说的不一样」（§5）。
// 客户端不支持 resources 也不影响闭环：内容主体在 gear_guide 工具里。
func (s *Server) registerResources(ms *mcp.Server) {
	for topic, content := range guide.All() {
		uri, text := "gear://guide/"+topic, content
		ms.AddResource(&mcp.Resource{
			URI:         uri,
			Name:        topic,
			Description: "Gear 手册：" + topic + "（与 gear_guide 工具同源）",
			MIMEType:    "text/markdown",
		}, func(_ context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			return &mcp.ReadResourceResult{
				Contents: []*mcp.ResourceContents{{URI: req.Params.URI, MIMEType: "text/markdown", Text: text}},
			}, nil
		})
	}
}

// objectResult 把列表类结果包成 MCP 规范要求的「顶层对象」形态（见包注释「返回形态」红线）。
//
// key 是与工具语义一致的具名键（items / flows / hooks / executions / logs / capabilities）；
// nil 切片统一成空数组，免得客户端拿到 null 还要自己判空。
func objectResult[T any](key string, v []T) map[string]any {
	if v == nil {
		v = []T{}
	}
	return map[string]any{key: v}
}

// register 把全部 MCP 工具挂到 server 上（12 个意图级工具，见包注释的工具表）。
func (s *Server) register(ms *mcp.Server) error {
	s.registerReadTools(ms)  // guide / catalog / describe / capabilities / doctor
	s.registerWriteTools(ms) // validate / apply / delete / promote
	s.registerRunTools(ms)   // run / execution
	s.registerAppTools(ms)   // app

	// 反向暴露：默认关闭（见 Server.exposeFlows 的说明），--expose-flows 显式开启。
	// service 长驻 Flow 即使开启也不暴露（常驻不结束，会挂死调用方）——与
	// REQ-FLOW-RUN-REJECTS-SERVICE 同口径。
	if !s.exposeFlows {
		return nil
	}
	flows, err := s.reg.ListFlows()
	if err != nil {
		return err
	}
	for _, f := range flows {
		if hasServiceStep(f) {
			continue
		}
		flowID := f.ID
		desc := f.Description
		if desc == "" {
			desc = "（无描述）"
		}
		mcp.AddTool(ms, &mcp.Tool{
			Name:        "flow__" + flowID,
			Description: fmt.Sprintf("运行 Gear Flow「%s」：%s。等效于 gear_run(kind=flow, id=\"%s\")。", flowID, desc, flowID),
		}, func(ctx context.Context, req *mcp.CallToolRequest, in FlowExposeArgs) (*mcp.CallToolResult, any, error) {
			id, err := s.startFlowExec(flowID, in.Vars)
			if err != nil {
				return toolErrorFrom("FLOW_RUN_FAILED", err), nil, nil
			}
			return nil, s.executionSnapshot(ctx, id, in.WaitSeconds), nil
		})
	}
	return nil
}

// runningToolReason 报告 toolID 是否正被运行中的执行使用（与 server 包同款，
// 判据来自共享的 execstore——REST 与 MCP 两个执行面的在途执行都算数）。
func (s *Server) runningToolReason(toolID string) string {
	if id, ok := s.execs.RunningToolIDs()[toolID]; ok {
		return fmt.Sprintf("正被运行中的执行 %s 使用", id)
	}
	return ""
}

// hasServiceStep 报告 Flow 是否含长驻 step（service 模式）。
func hasServiceStep(f *model.Flow) bool {
	for _, st := range f.Steps {
		if st.Mode == "service" {
			return true
		}
	}
	return false
}

// lockFlowForRun 取出 Flow 并挡掉 service 长驻 Flow。
//
// 挡它**不是缺陷而是设计**（REQ-FLOW-RUN-REJECTS-SERVICE）：长驻 Flow 不会结束，
// 同步等待必然超时；正确路径是 gear_app(action=start)，那条路会派生独立后台进程。
// 错误里带上 `SERVICE_FLOW_SYNC_RUN` 码与修法，让智能体能自愈而不是反复重试。
func (s *Server) lockFlowForRun(flowID string) (*model.Flow, error) {
	f, err := s.reg.GetFlow(flowID)
	if err != nil {
		return nil, err
	}
	if f == nil {
		// 用 NOT_FOUND 而不是 APP_NOT_FOUND：后者是「台账里没有这个 app_id」，混用会让
		// 智能体按 errors 手册去查应用台账，而真正该做的是 gear_catalog(kind=flow)。
		return nil, diagError("NOT_FOUND", "flow_id", "先用 gear_catalog(kind=flow) 看有哪些 Flow", "Flow 不存在: %s", flowID)
	}
	if hasServiceStep(f) {
		return nil, diagError("SERVICE_FLOW_SYNC_RUN", "flow_id",
			"长驻应用用 gear_app(action=start, flow_id=…)，它会派生后台进程并立即返回",
			"Flow %s 含 mode: service 长驻 step，gear_run 只跑一次性编排", flowID)
	}
	return f, nil
}

// startFlowExec 在后台启动一次一次性 Flow 执行并立即返回执行 id。
//
// 为什么异步：MCP 工具调用受客户端超时约束，而一条要 build / 抓数据 / 计算的 Flow 很容易顶到
// 超时；REST 面早已是「202 + execution id」（POST /api/executions），这里对齐同一语义（§6.2④）。
// 调用方用 gear_execution(id) 轮询结果，或先给 wait_seconds 短等。
//
// 治理（评审 I-1）：执行**有界并发**（execSem，满了返回 EXECUTION_BUSY 背压），并被 execWG
// 追踪，Close 时等待收敛——此前每条执行都裸起 goroutine，既无上限也无收敛。
func (s *Server) startFlowExec(flowID string, vars map[string]any) (string, error) {
	if _, err := s.lockFlowForRun(flowID); err != nil {
		return "", err
	}
	select {
	case s.execSem <- struct{}{}:
	default:
		return "", diagError("EXECUTION_BUSY", "",
			"稍后重试；或先 gear_execution 看现有执行的进展",
			"并发执行已达上限（%d），请稍后重试", maxConcurrentExecs)
	}
	rec := s.execs.Create(flowID)
	s.execWG.Add(1)
	go func() {
		defer s.execWG.Done()
		defer func() { <-s.execSem }()
		res, runErr := s.engine.RunFlowExecWithVars(flowID, rec.ID, func(toolIDs []string) {
			s.execs.SetTools(rec.ID, toolIDs)
		}, vars)
		s.execs.Finish(rec.ID, res, runErr)
	}()
	return rec.ID, nil
}

// executionSnapshot 返回一次执行的快照（供 gear_run 与 flow__ 工具复用）。
//
// waitSeconds>0 时先短等：避免了「刚起就查、必然 running」的空转（§13 R5）；
// 等不到也不报错——如实返回 running，让调用方决定继续等还是先干别的。
//
// ★ ctx 取消（评审 I-1）：客户端断开/超时就**立刻停止忙等**——此前不看 ctx.Done()，
// 客户端早走了这里还会白等满 N 秒。停止忙碌时如实返回**当前**快照（不报错）。
func (s *Server) executionSnapshot(ctx context.Context, id string, waitSeconds int) map[string]any {
	if waitSeconds > 0 {
		deadline := time.Now().Add(time.Duration(waitSeconds) * time.Second)
		for time.Now().Before(deadline) {
			e, ok := s.execs.Get(id)
			if !ok || e.FinishedAt != nil {
				break
			}
			select {
			case <-ctx.Done():
				return s.executionSnapshotNow(id)
			case <-time.After(200 * time.Millisecond):
			}
		}
	}
	return s.executionSnapshotNow(id)
}

// executionSnapshotNow 取一次当前执行快照（不等待）。
func (s *Server) executionSnapshotNow(id string) map[string]any {
	e, ok := s.execs.Get(id)
	if !ok {
		return map[string]any{"execution_id": id, "status": "unknown"}
	}
	out := map[string]any{
		"execution_id": e.ID,
		"flow_id":      e.FlowID,
		"status":       e.Status,
		"started_at":   e.StartedAt,
	}
	if e.Error != "" {
		out["error"] = e.Error
	}
	if e.Result != nil {
		out["result"] = e.Result
	}
	if e.FinishedAt != nil {
		out["finished_at"] = *e.FinishedAt
	}
	return out
}

// appEnv 组装 app 包的调用参数（registry 路径 + 台账目录统一从 Server 取，
// 保证 MCP 面与 CLI 面用同一套定位规则，不各写一份默认值）。
func (s *Server) appEnv() (registryPath, appsDir string) {
	return s.registryPath, s.appsDir
}

// appList 返回台账与遗留项（gear_app(list) 与 gear_doctor 共用）。
func (s *Server) appList() (*app.ListResult, error) {
	regPath, appsDir := s.appEnv()
	return app.List(regPath, appsDir)
}

// appStart 派生后台进程监管 Flow（见 app.Start 的语义说明）。
func (s *Server) appStart(a AppArgs) (*app.StartResult, error) {
	regPath, appsDir := s.appEnv()
	if regPath == "" {
		// 没拿到 registry 路径就不能派生：子进程会去读默认库，可能启动另一个库里的同名 Flow。
		// 这是**拒绝执行**而不是「尽力而为」——启动错的对象比启动失败更难排查。
		return nil, diagError("APP_START_FAILED", "registry", "MCP Server 未配置 registry 路径（启动时加 --registry）",
			"无法派生应用：未配置 registry 路径")
	}
	return app.Start(app.StartOptions{
		RegistryPath: regPath,
		AppsDir:      appsDir,
		FlowID:       a.FlowID,
		Restart:      a.Restart,
		Ports:        a.Ports,
		WaitSeconds:  a.WaitSeconds,
		Replace:      a.Replace,
	})
}
