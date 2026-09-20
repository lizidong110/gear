// Package mcp 实现 Gear 的第四种执行面：MCP (Model Context Protocol) Server。
//
// 动机：让任何支持 MCP 的智能体（Claude Desktop、WorkBuddy 等）直接「读目录 →
// 写配置 → 校验 → 跑一把 → 看结果」，即以对话方式完成 Tool/Flow/Hook 的配置与自测。
//
// 设计取舍：
//   - MCP 只做**协议翻译**：删除守卫（PRD §8.2）、schema 校验、版本状态机全部在
//     registry/engine 层，本包不重复实现（守卫必须只有一份，否则各执行面会漂移）。
//   - 装配模式与 internal/server 保持一致：调用方（CLI）负责 registry + 引擎装配
//     （含扩展目录），本包只挂 MCP 工具面。
//   - 执行记录复用 internal/execstore（与 REST 面同一份 SQLite），因此「运行中不可
//     注销」的运行期判据（RunningToolIDs）在两个执行面下语义一致。
//   - **返回形态按规范最严口径发（事故教训）**：MCP 2025-06-18 规定 `tools/call` 的
//     `structuredContent` 顶层**必须是 JSON 对象**。go-sdk v1.8.0 依 SEP-2106 草案放行
//     数组/标量，且自带客户端同样宽松 → 本地冒烟全绿，但接**严格客户端**（WorkBuddy 用
//     zod 校验）时该字段被拒（`expected record, received array`），整条 tools/call 被
//     丢弃，表现为「一个工具都用不了」。故列表类结果一律经 `objectResult` 包一层具名键
//     （`{tools:[...]}` / `{flows:[...]}` …），**绝不裸发数组**。
//   - 默认 stdio transport：这是本地智能体接入 MCP 的标准方式（客户端配置
//     command=gear, args=[mcp]）。注意 stdio 下 stdout 只能承载协议帧，
//     任何日志/警告必须走 stderr。
package mcp

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"gear/internal/cap"
	"gear/internal/engine"
	"gear/internal/execstore"
	"gear/internal/model"
	"gear/internal/registry"
)

// Version 是 MCP Server 汇报给客户端的实现版本号。
const Version = "0.1.0"

// Server 是 MCP 执行面：持有与 REST 面同构的 registry + 引擎 + 执行记录存储。
type Server struct {
	reg    *registry.Registry
	engine *engine.Engine
	execs  *execstore.ExecStore
}

// New 构造 MCP Server。registry 与引擎由调用方装配（CLI 用 cliEngine，
// 与 REST 面同一套扩展加载），这里补上执行记录存储与运行期注销守卫。
//
// ⚠ 契约：调用方**必须先完成扩展加载**（`engine.SetupExtensions`，其副作用是把
// extensions.yaml 里的 ext.* 名并入 `cap` 包的全局合法 capability 集合）。本函数
// 用 extNamesReady 显式校验这一点并 fail-closed —— 不要依赖「cliEngine 恰好先跑过」
// 的隐式顺序：registry.UpsertTool → model.Tool.Validate → cap.GrantsFromStrings
// 会用那个全局集合判定 capability，缺了它，声明 ext.* 的 Tool 会被误判「未知 capability」。
func New(reg *registry.Registry, eng *engine.Engine) (*Server, error) {
	if err := extNamesReady(); err != nil {
		return nil, err
	}
	s := &Server{reg: reg, engine: eng, execs: execstore.NewExecStore()}
	// §8.2 第三条守卫需要「运行期引用」：与 REST 面同款注入——
	// MCP 面发起的执行同样会被 DeleteTool 守卫看见。
	reg.SetRunningChecker(s.runningToolReason)
	return s, nil
}

// extNamesReady 校验扩展名已并入全局 capability 集合（SetupExtensions 的副作用）。
//
// 判据：环境未配置任何 extensions.yaml 时，内核**始终注册 ext.ai**（默认回退路径），
// 所以「ext.ai 是否 Known」就等价于「SetupExtensions 是否已跑过」。
// 返回错误即为装配顺序错误（fail-closed，而不是等到 create_tool 时报一个误导性的
// 「未知的 capability」）。
func extNamesReady() error {
	if !cap.Primitive("ext.ai").Known() {
		return fmt.Errorf("MCP Server 装配顺序错误：扩展尚未加载（需先调用 engine.SetupExtensions 再 New）——" +
			"否则声明 ext.* capability 的 Tool 会被误判为「未知的 capability」")
	}
	return nil
}

// Close 释放执行记录库句柄。
func (s *Server) Close() error { return s.execs.Close() }

// Run 在给定 transport 上运行 MCP Server（阻塞直到客户端断开）。
func (s *Server) Run(ctx context.Context, transport mcp.Transport) error {
	ms := mcp.NewServer(&mcp.Implementation{Name: "gear", Version: Version}, nil)
	if err := s.register(ms); err != nil {
		return err
	}
	return ms.Run(ctx, transport)
}

// runningToolReason 报告 toolID 是否正被运行中的执行使用（与 server 包同款，
// 判据来自共享的 execstore——REST 与 MCP 两个执行面的在途执行都算数）。
func (s *Server) runningToolReason(toolID string) string {
	if id, ok := s.execs.RunningToolIDs()[toolID]; ok {
		return fmt.Sprintf("正被运行中的执行 %s 使用", id)
	}
	return ""
}

// runFlowByID 同步运行一个 Flow 并返回执行记录（run_flow 与 flow__ 工具共用）。
// 失败时也返回记录（不抛 MCP 错误），由调用方从 result/error 字段定位问题步骤。
// service 长驻 Flow 显式拒绝（须 gear app run 监管）。
func (s *Server) runFlowByID(id string, vars map[string]any) (*execstore.Execution, error) {
	f, err := s.reg.GetFlow(id)
	if err != nil {
		return nil, err
	}
	if f == nil {
		return nil, fmt.Errorf("Flow 不存在: %s", id)
	}
	if hasServiceStep(f) {
		return nil, fmt.Errorf("Flow %s 含 service 长驻 step，须用 `gear app run` 监管，MCP 不暴露同步运行", id)
	}
	rec := s.execs.Create(id)
	res, runErr := s.engine.RunFlowExecWithVars(id, rec.ID, func(toolIDs []string) {
		s.execs.SetTools(rec.ID, toolIDs)
	}, vars)
	s.execs.Finish(rec.ID, res, runErr)
	e, ok := s.execs.Get(rec.ID)
	if !ok {
		return nil, fmt.Errorf("执行记录丢失: %s", rec.ID)
	}
	return &e, nil
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

// httpHandler 构造 streamable HTTP transport 的 http.Handler（与 stdio 共用同一套工具面）。
func (s *Server) httpHandler() (http.Handler, error) {
	ms := mcp.NewServer(&mcp.Implementation{Name: "gear", Version: Version}, nil)
	if err := s.register(ms); err != nil {
		return nil, err
	}
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return ms }, nil), nil
}

// ServeHTTP 在 addr（host:port）以 streamable HTTP transport 暴露 MCP Server，
// 阻塞直到退出信号（SIGINT/SIGTERM）或 ctx 取消。
func (s *Server) ServeHTTP(ctx context.Context, addr string) error {
	h, err := s.httpHandler()
	if err != nil {
		return err
	}
	srv := &http.Server{Addr: addr, Handler: h}
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

// objectResult 把列表类结果包成 MCP 规范要求的「顶层对象」形态（见包注释「返回形态」红线）。
//
// 为什么不直接返回切片：MCP 2025-06-18 要求 `structuredContent` 顶层**必须是对象**；
// 列表类工具裸发数组会被严格客户端（WorkBuddy 的 zod 校验）判协议不合规并**丢弃整条
// 响应**——用户侧表现为「这个 MCP 一个工具都用不了」，而不是某个工具报错，极难从表面
// 定位。故此处集中兜底，而不是指望每个 handler 自觉。
//
// key 是与工具语义一致的具名键（tools / flows / hooks / executions / logs / capabilities）；
// nil 切片统一成空数组，免得客户端拿到 null 还要自己判空。
func objectResult[T any](key string, v []T) map[string]any {
	if v == nil {
		v = []T{}
	}
	return map[string]any{key: v}
}

// register 把全部 MCP 工具挂到 server 上。
//
// 工具面分三组：目录读（list/get）、配置写（create/delete/promote/validate）、
// 执行（run + 查询）。命名用动词_宾语，MCP 客户端的智能体可直接望文生义。
//
// 约定：**列表类结果的返回值一律经 objectResult 包具名键**（见其注释的线格式红线），
// 且工具 Description 末尾标注返回形态（如「返回 {tools:[...]}」），便于智能体直接消费。
func (s *Server) register(ms *mcp.Server) error {
	// ---- 目录读 ----
	mcp.AddTool(ms, &mcp.Tool{
		Name:        "list_tools",
		Description: "列出 Gear 目录中全部 Tool（含 runtime、input/output schema、版本状态）。写 Flow 前先调它确认可用的 step call 目标。返回 {tools:[...]}。",
	}, func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		tools, err := s.reg.ListTools()
		if err != nil {
			return nil, nil, err
		}
		// 用 any 而非 []*model.Tool：SDK 对 Out == any 跳过输出 schema 校验。
		// 必需——Tool 的 output_schema/capabilities 等 map/slice 字段在零值时是 nil，
		// 会被推断出的 object/array schema 判为 "null" 而整条调用失败（见包注释「零值陷阱」）。
		// 顶层必须是对象（objectResult），否则严格客户端丢弃整条响应。
		return nil, objectResult("tools", tools), nil
	})

	mcp.AddTool(ms, &mcp.Tool{
		Name:        "get_tool",
		Description: "按 id 查看单个 Tool 的完整定义（含全部版本与状态）。",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in IDArgs) (*mcp.CallToolResult, any, error) {
		t, err := s.reg.GetTool(in.ID)
		if err != nil {
			return nil, nil, err
		}
		if t == nil {
			return nil, nil, fmt.Errorf("Tool 不存在: %s", in.ID)
		}
		return nil, t, nil
	})

	mcp.AddTool(ms, &mcp.Tool{
		Name:        "list_flows",
		Description: "列出 Gear 目录中全部 Flow（含 steps、vars、依赖关系）。返回 {flows:[...]}。",
	}, func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		flows, err := s.reg.ListFlows()
		if err != nil {
			return nil, nil, err
		}
		return nil, objectResult("flows", flows), nil
	})

	mcp.AddTool(ms, &mcp.Tool{
		Name:        "get_flow",
		Description: "按 id 查看单个 Flow 的完整定义。",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in IDArgs) (*mcp.CallToolResult, any, error) {
		f, err := s.reg.GetFlow(in.ID)
		if err != nil {
			return nil, nil, err
		}
		if f == nil {
			return nil, nil, fmt.Errorf("Flow 不存在: %s", in.ID)
		}
		return nil, f, nil
	})

	mcp.AddTool(ms, &mcp.Tool{
		Name:        "list_hooks",
		Description: "列出全部 Hook（stage/target/action/criticality/order）。返回 {hooks:[...]}。",
	}, func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		hooks, err := s.reg.ListHooks()
		if err != nil {
			return nil, nil, err
		}
		return nil, objectResult("hooks", hooks), nil
	})

	// ---- 配置写 ----
	mcp.AddTool(ms, &mcp.Tool{
		Name: "create_tool",
		Description: "注册（或更新）Tool：body 为 Gear Tool YAML，支持 `---` 分隔的多文档一次注册多个" +
			"（**先全部校验、全部通过才逐个落库**，任一非法则整体不写入，不会部分成功）。\n" +
			"必填字段：id / name / description / runtime / entry / input_schema / output_schema / versions。\n" +
			"  - runtime：process（entry=命令，stdin 进 JSON、stdout 末行出 JSON）| lua（entry=内联 Lua 源码）| go-host。\n" +
			"  - entry：Lua 时**直接写脚本本体**，用 `ctx.input` 读入参、`return { ... }` 出参。\n" +
			"  - input_schema/output_schema：**标准 JSON Schema，且 type 必须是 object**（不支持 {\"x\":\"string\"} 简写）。\n" +
			"  - versions：非空数组，元素含 version(SemVer 如 \"1.0.0\") 与 state(draft|testing|stable|deprecated|rejected)；新 Tool 建议 state: stable。\n" +
			"  - capabilities：可选，声明可用宿主原语。**动手前先调 `capabilities` 工具拿完整清单与参数 schema**，不要凭记忆猜——内置 http/call/db/kv/secret/event 之外还有 extensions.yaml 注册的 ext.*。\n" +
			"格式示例（Lua Tool，声明 http 能力后用 ctx.http.Request 发请求）：\n" +
			"id: fetch_url\nname: Fetch URL\ndescription: 抓取一个 URL 的响应体\nruntime: lua\n" +
			"entry: |\n  local r = ctx.http.Request(\"GET\", ctx.input.url)\n  return { status = r.status, body = r.body }\n" +
			"capabilities:\n  - http\ninput_schema:\n  type: object\n  properties:\n    url:\n      type: string\n  required:\n    - url\n" +
			"output_schema:\n  type: object\nversions:\n  - version: \"1.0.0\"\n    state: stable\n" +
			"提示：只想调一次原子能力、不打算复用的话，Flow 的 step.call 可直接写「能力.方法」（如 kv.put），无需先建 Tool。",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in YAMLArgs) (*mcp.CallToolResult, any, error) {
		tools, flows, err := parseGearYAML([]byte(in.YAML))
		if err != nil {
			return nil, nil, err
		}
		if len(flows) > 0 {
			return nil, nil, fmt.Errorf("这段 YAML 含 Flow 定义（有 steps 字段），请改用 create_flow")
		}
		if len(tools) == 0 {
			return nil, nil, fmt.Errorf("没有解析出任何 Tool 定义")
		}
		// 两阶段：先全量校验（阶段一），全通过才落库（阶段二）。
		// 否则多文档里「第 1 个成功、第 2 个失败」会留下部分写入，而调用方收到的是
		// 一个 error —— 智能体重试整段 YAML 时行为与首次不一致（不静默降级）。
		for _, t := range tools {
			if err := t.Validate(); err != nil {
				return nil, nil, fmt.Errorf("Tool %s 校验失败（本次未写入任何 Tool）: %w", t.ID, err)
			}
		}
		registered := []string{}
		for _, t := range tools {
			if err := s.reg.UpsertTool(t); err != nil {
				return nil, nil, fmt.Errorf("注册 Tool %s 失败（已成功写入 %v，请先处理再重试）: %w", t.ID, registered, err)
			}
			registered = append(registered, t.ID)
		}
		return nil, map[string]any{"registered_tools": registered}, nil
	})

	mcp.AddTool(ms, &mcp.Tool{
		Name: "create_flow",
		Description: "注册（或更新）Flow：body 为 Gear Flow YAML，支持 `---` 分隔的多文档" +
			"（**先全部校验、全通过才逐个落库**，任一非法则整体不写入，不会部分成功）。\n" +
			"入库前做静态校验：step 的 call 目标是否存在、when 表达式可否编译、input 是否符合 Tool 的 input_schema。\n" +
			"必填字段：id / name / description / steps（至少一个）。\n" +
			"  - step.call：**已注册 Tool 的 id**，或「能力.方法」直调（如 kv.put / http.Request / ext.ai.complete，免建 Tool；可用清单先调 `capabilities`）。\n" +
			"  - step.input：入参，模板写 \"${vars.x}\" 或 \"${steps.<id>.output.<字段>}\"（后者自动推断依赖，无需写 depends_on）。\n" +
			"  - step.depends_on：显式上游 step id 数组；step.when：条件表达式（**不是模板**，别写 ${}）；step.parallel：bool；step.mode：once(默认)|service(长驻，须用 gear app run 跑)。\n" +
			"  - 不想声明变量就不写 vars 字段（留空即合法）。\n" +
			"格式示例：\n" +
			"id: demo\nname: Demo Flow\ndescription: 两步串联示例\nvars:\n  topic: world\nsteps:\n  - id: s1\n    call: hello\n    input:\n      name: \"${vars.topic}\"\n  - id: s2\n    call: hello\n    depends_on: [s1]\n    input:\n      name: \"${steps.s1.output.name}-reply\"\n",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in YAMLArgs) (*mcp.CallToolResult, any, error) {
		tools, flows, err := parseGearYAML([]byte(in.YAML))
		if err != nil {
			return nil, nil, err
		}
		if len(tools) > 0 {
			return nil, nil, fmt.Errorf("这段 YAML 含 Tool 定义，请改用 create_tool")
		}
		if len(flows) == 0 {
			return nil, nil, fmt.Errorf("没有解析出任何 Flow 定义")
		}
		// 两阶段：先全量静态校验，全通过才落库（同 create_tool，避免部分成功）。
		for _, f := range flows {
			// 与 REST handleCreateFlow 同一条规则：必须用已装配的引擎校验
			// （扩展目录挂在它上面，否则 ext.* 判定与运行期漂移）。
			if err := s.engine.ValidateFlowStatic(f); err != nil {
				return nil, nil, fmt.Errorf("Flow %s 静态校验失败（本次未写入任何 Flow）: %w", f.ID, err)
			}
		}
		registered := []string{}
		for _, f := range flows {
			if err := s.reg.UpsertFlow(f); err != nil {
				return nil, nil, fmt.Errorf("注册 Flow %s 失败（已成功写入 %v，请先处理再重试）: %w", f.ID, registered, err)
			}
			registered = append(registered, f.ID)
		}
		return nil, map[string]any{"registered_flows": registered}, nil
	})

	mcp.AddTool(ms, &mcp.Tool{
		Name:        "validate_flow",
		Description: "对已注册的 Flow 做静态校验（call 目标存在性、when 可编译、input schema），返回 {valid, error}。",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in IDArgs) (*mcp.CallToolResult, map[string]any, error) {
		f, err := s.reg.GetFlow(in.ID)
		if err != nil {
			return nil, nil, err
		}
		if f == nil {
			return nil, nil, fmt.Errorf("Flow 不存在: %s", in.ID)
		}
		if err := s.engine.ValidateFlowStatic(f); err != nil {
			return nil, map[string]any{"valid": false, "error": err.Error()}, nil
		}
		return nil, map[string]any{"valid": true}, nil
	})

	mcp.AddTool(ms, &mcp.Tool{
		Name:        "delete_tool",
		Description: "注销 Tool。被 Flow step / Hook action / 运行中执行引用时会被守卫拒绝（409 语义），错误信息会说明被谁引用。",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in IDArgs) (*mcp.CallToolResult, map[string]any, error) {
		if err := s.reg.DeleteTool(in.ID); err != nil {
			return nil, nil, err
		}
		return nil, map[string]any{"deleted": in.ID}, nil
	})

	mcp.AddTool(ms, &mcp.Tool{
		Name:        "delete_flow",
		Description: "注销 Flow。被其它 Flow / Hook / 运行中执行引用时会被守卫拒绝。",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in IDArgs) (*mcp.CallToolResult, map[string]any, error) {
		if err := s.reg.DeleteFlow(in.ID); err != nil {
			return nil, nil, err
		}
		return nil, map[string]any{"deleted": in.ID}, nil
	})

	mcp.AddTool(ms, &mcp.Tool{
		Name:        "delete_hook",
		Description: "删除一个 Hook（Hook 不被引用，无删除守卫；不存在则报错，不静默）。",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in IDArgs) (*mcp.CallToolResult, map[string]any, error) {
		if err := s.reg.DeleteHook(in.ID); err != nil {
			return nil, nil, err
		}
		return nil, map[string]any{"deleted": in.ID}, nil
	})

	mcp.AddTool(ms, &mcp.Tool{
		Name: "promote_tool",
		Description: "晋升 Tool 某版本到目标状态（draft→testing→stable→deprecated，非法迁移会被 model.CanPromote 拒绝）。" +
			"触发 before_promote Hook 门禁（fail-closed）。stable 后才能被 step 的 use_version 之外的默认引用使用。",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in PromoteArgs) (*mcp.CallToolResult, map[string]any, error) {
		target := model.ToolVersionState(in.Target)
		if !model.ValidStates[target] {
			return nil, nil, fmt.Errorf("非法目标状态: %q（应为 draft/testing/stable/deprecated/rejected）", in.Target)
		}
		if err := s.engine.PromoteTool(in.ToolID, in.Version, target); err != nil {
			return nil, nil, err
		}
		return nil, map[string]any{"tool_id": in.ToolID, "version": in.Version, "state": in.Target}, nil
	})

	mcp.AddTool(ms, &mcp.Tool{
		Name: "create_hook",
		Description: "注册 Hook：在 stage（before_flow/before_step/after_step/on_error/on_success/before_promote）对 target " +
			"（Tool id、Flow id 或 \"*\"）触发 action（Tool id 或 Flow id）。criticality=critical（默认，失败终止）或 best-effort。",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in HookArgs) (*mcp.CallToolResult, any, error) {
		h := &model.Hook{
			ID: in.ID, Target: in.Target, Stage: in.Stage, Action: in.Action,
			Criticality: model.HookCriticality(in.Criticality), Order: in.Order,
		}
		if err := s.reg.UpsertHook(h); err != nil {
			return nil, nil, err
		}
		return nil, h, nil
	})

	// ---- 执行 ----
	mcp.AddTool(ms, &mcp.Tool{
		Name: "run_tool",
		Description: "同步执行单个 Tool（走完整链路：schema 校验 → capability 门禁 → runtime 执行 → Hook）。" +
			"input 需符合该 Tool 的 input_schema；返回 Tool 的 output。",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in RunToolArgs) (*mcp.CallToolResult, any, error) {
		out, err := s.engine.RunTool(in.ID, in.Input)
		if err != nil {
			return nil, nil, err
		}
		if out == nil {
			out = map[string]any{}
		}
		return nil, out, nil
	})

	mcp.AddTool(ms, &mcp.Tool{
		Name: "run_flow",
		Description: "触发一次 Flow 执行并**同步等待结束**（vars 覆盖 Flow 的 vars，模板写作 ${vars.x}）。" +
			"返回执行记录：status(succeeded|failed)、result（各 step 的 output）、error。" +
			"失败时也返回记录（不抛错），从 result 与 error 字段定位问题步骤。",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in RunFlowArgs) (*mcp.CallToolResult, any, error) {
		e, err := s.runFlowByID(in.ID, in.Vars)
		return nil, e, err
	})

	mcp.AddTool(ms, &mcp.Tool{
		Name:        "list_executions",
		Description: "列出执行记录（含 status/result/error/tool_ids；SQLite 持久化，重启可查）。返回 {executions:[...]}。",
	}, func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		return nil, objectResult("executions", s.execs.List()), nil
	})

	mcp.AddTool(ms, &mcp.Tool{
		Name:        "get_execution",
		Description: "按 id 查看单次执行的完整记录（含各 step 的 result）。",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in IDArgs) (*mcp.CallToolResult, any, error) {
		e, ok := s.execs.Get(in.ID)
		if !ok {
			return nil, nil, fmt.Errorf("执行记录不存在: %s", in.ID)
		}
		return nil, &e, nil
	})

	mcp.AddTool(ms, &mcp.Tool{
		Name:        "get_execution_logs",
		Description: "查看某次执行各 step 的 stdout/stderr 日志行（排障用）。返回 {logs:[...]}。",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in IDArgs) (*mcp.CallToolResult, any, error) {
		if _, ok := s.execs.Get(in.ID); !ok {
			return nil, nil, fmt.Errorf("执行记录不存在: %s", in.ID)
		}
		return nil, objectResult("logs", s.execs.Logs(in.ID)), nil
	})

	mcp.AddTool(ms, &mcp.Tool{
		Name: "capabilities",
		Description: "列出当前可用的原子能力目录：内核原语（受门禁 builtin 7 + 纯函数 pure 3）+ ext.*，" +
			"每个能力含方法级参数 schema。写 step 的「能力.方法」直调（如 kv.put）前先看这个。返回 {capabilities:[...]}。",
	}, func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		agent := s.engine.AIAgent()
		if agent == nil {
			return nil, nil, fmt.Errorf("扩展目录未初始化")
		}
		caps, err := agent.CapabilitiesMethod()
		if err != nil {
			return nil, nil, err
		}
		list, _ := caps.([]map[string]any)
		return nil, objectResult("capabilities", list), nil
	})

	// ---- 反向暴露：把已注册 Flow 暴露为可直接调用的 MCP 工具 ----
	// 外部智能体可像调原生 MCP 工具一样调你的编排（flow__{id}），不必理解 run_flow
	// 的参数形态。service 长驻 Flow 不暴露（会常驻不结束），提示用 `gear app run`
	// （与 REQ-FLOW-RUN-REJECTS-SERVICE 同口径）。
	flows, err := s.reg.ListFlows()
	if err != nil {
		return err
	}
	for _, f := range flows {
		if hasServiceStep(f) {
			// service 长驻 Flow 不暴露为同步工具（会常驻不结束），提示用 gear app run。
			continue
		}
		flowID := f.ID
		desc := f.Description
		if desc == "" {
			desc = "（无描述）"
		}
		mcp.AddTool(ms, &mcp.Tool{
			Name:        "flow__" + flowID,
			Description: fmt.Sprintf("运行 Gear Flow 「%s」：%s。可选 vars 覆盖 Flow 的 vars（模板 ${vars.x} 引用）。", flowID, desc),
		}, func(ctx context.Context, req *mcp.CallToolRequest, in FlowExposeArgs) (*mcp.CallToolResult, any, error) {
			// 反向暴露的工具固定运行 flowID，忽略入参 id。
			e, err := s.runFlowByID(flowID, in.Vars)
			return nil, e, err
		})
	}
	return nil
}

// ---- 输入参数类型（SDK 据此自动生成 input schema）----

// IDArgs 是「按 id 操作」类工具的公共入参。
type IDArgs struct {
	ID string `json:"id" jsonschema:"对象 id（Tool id / Flow id / Hook id / 执行 id）"`
}

// YAMLArgs 是 create_tool / create_flow 的入参。
type YAMLArgs struct {
	YAML string `json:"yaml" jsonschema:"Gear YAML 定义文本，支持 --- 分隔的多文档"`
}

// PromoteArgs 是 promote_tool 的入参。
type PromoteArgs struct {
	ToolID  string `json:"tool_id" jsonschema:"Tool id"`
	Version string `json:"version" jsonschema:"目标版本号"`
	Target  string `json:"target" jsonschema:"目标状态：draft/testing/stable/deprecated/rejected"`
}

// HookArgs 是 create_hook 的入参。
type HookArgs struct {
	ID          string `json:"id" jsonschema:"Hook id"`
	Target      string `json:"target" jsonschema:"匹配目标：Tool id / Flow id / *（全部）"`
	Stage       string `json:"stage" jsonschema:"触发阶段：before_flow/before_step/after_step/on_error/on_success/before_promote"`
	Action      string `json:"action" jsonschema:"触发动作：Tool id 或 Flow id"`
	Criticality string `json:"criticality,omitempty" jsonschema:"critical（默认，失败终止）或 best-effort"`
	Order       int    `json:"order,omitempty" jsonschema:"同 stage 内的执行顺序"`
}

// RunToolArgs 是 run_tool 的入参。
type RunToolArgs struct {
	ID    string         `json:"id" jsonschema:"Tool id"`
	Input map[string]any `json:"input,omitempty" jsonschema:"符合该 Tool input_schema 的入参"`
}

// RunFlowArgs 是 run_flow 的入参。
type RunFlowArgs struct {
	ID   string         `json:"id" jsonschema:"Flow id"`
	Vars map[string]any `json:"vars,omitempty" jsonschema:"覆盖 Flow vars 的变量（模板 ${vars.x} 引用）"`
}

// FlowExposeArgs 是 flow__{id} 反向暴露工具的入参（只收可选 vars，id 固定为 Flow id）。
type FlowExposeArgs struct {
	Vars map[string]any `json:"vars,omitempty" jsonschema:"覆盖 Flow 的 vars（模板 ${vars.x} 引用）"`
}

// parseGearYAML 解析 Gear YAML（多文档，按是否含 steps 字段区分 Flow/Tool）。
// 直接复用 registry 包的单一实现（§0 单一真相源）——此前本包自带一份副本，
// 与 registry/loader.go 的规则可能漂移；CLI `tool/flow register` 也曾因各写各的
// 而出现「只读第一个文档」的分叉。
// 不做校验——校验在 UpsertTool/UpsertFlow/ValidateFlowStatic 注册期完成。
func parseGearYAML(b []byte) ([]*model.Tool, []*model.Flow, error) {
	return registry.ParseYAMLDocs(b)
}
