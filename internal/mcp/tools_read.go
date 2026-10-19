package mcp

// tools_read.go：只读类工具 —— guide / catalog / describe / capabilities / doctor。
//
// 只读工具全部标 ReadOnlyHint，让客户端能在 UI 上区分「看看」与「会改东西」。

import (
	"context"
	"sort"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"gear/internal/app"
	"gear/internal/artifact"
	"gear/internal/doctor"
	"gear/internal/mcp/guide"
	"gear/internal/model"
)

// readOnly 标注「只读」工具。
func readOnly(title string) *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{Title: title, ReadOnlyHint: true, IdempotentHint: true}
}

// destructive 标注「可能破坏性」的工具（客户端据此在 UI 上提示风险）。
func destructive(title string) *mcp.ToolAnnotations {
	d := true
	return &mcp.ToolAnnotations{Title: title, DestructiveHint: &d}
}

// idempotent 标注「可重复执行、效果相同」的工具。
func idempotent(title string) *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{Title: title, IdempotentHint: true}
}

// registerReadTools 挂上只读类工具。
func (s *Server) registerReadTools(ms *mcp.Server) {
	mcp.AddTool(ms, &mcp.Tool{
		Name: "gear_guide",
		Description: "取 Gear 手册的某个主题。写 YAML 前看 authoring、做长驻应用看 service-app、报错后看 errors、" +
			"发现能力不足看 primitive-gap。与常驻说明同源，比翻仓库文档更准。",
		Annotations:  readOnly("Gear 手册"),
		OutputSchema: outGuide,
	}, s.handleGuide)

	mcp.AddTool(ms, &mcp.Tool{
		Name: "gear_catalog",
		Description: "列出目录里现有的对象**摘要**（tool/flow/hook/execution/app）——「先看看有什么」用这个。" +
			"结果多时用 `offset`/`limit` 翻页（truncated 为 true 时用返回的 next_offset 续取）；要看完整定义用 gear_describe。",
		Annotations:  readOnly("目录摘要"),
		OutputSchema: outCatalog,
	}, s.handleCatalog)

	mcp.AddTool(ms, &mcp.Tool{
		Name:         "gear_describe",
		Description:  "查看某个对象的完整定义（tool/flow/hook/app）。从 gear_catalog 拿到 id 后用它看细节。",
		Annotations:  readOnly("对象详情"),
		OutputSchema: outDescribe,
	}, s.handleDescribe)

	mcp.AddTool(ms, &mcp.Tool{
		Name: "gear_capabilities",
		Description: "列出可用的能力目录（内核原语 + ext.*），含方法级参数 schema；可按 name/kind 过滤以减小返回。" +
			"写 step.call 的「能力.方法」直调前先看它，不要凭记忆猜方法名与参数。",
		Annotations:  readOnly("能力目录"),
		OutputSchema: outCapabilities,
	}, s.handleCapabilities)

	mcp.AddTool(ms, &mcp.Tool{
		Name: "gear_doctor",
		Description: "环境自检：registry 计数、扩展与侧车就绪情况、AI 凭据就绪（只看有没有、不回显明文）、" +
			"台账/遗留应用、端口占用。应用起不来、侧车缺失、ext.ai 报 401 时先跑它，" +
			"能一次区分「YAML 写错」与「环境没准备好」。",
		Annotations:  readOnly("环境自检"),
		OutputSchema: outDoctor,
	}, s.handleDoctor)
}

// handleGuide 返回手册某主题的全文。
func (s *Server) handleGuide(ctx context.Context, _ *mcp.CallToolRequest, in GuideArgs) (*mcp.CallToolResult, any, error) {
	topic := strings.TrimSpace(in.Topic)
	if topic == "" {
		topic = "overview"
	}
	content, err := guide.Get(topic)
	if err != nil {
		return toolError("UNKNOWN_TOPIC", err.Error(),
			"可用主题："+strings.Join(guide.Topics(), ", ")), nil, nil
	}
	return nil, map[string]any{"topic": topic, "content": content, "topics": guide.Topics()}, nil
}

// handleCatalog 返回对象摘要列表。
//
// 这是 P3（token 爆炸）的根治：旧 list_tools 返回每个 Tool 的完整 input_schema/output_schema/
// versions，一个成熟 registry 一次能吃掉数万 token，而 95% 当场无用。摘要 + 按需 describe
// 把「看一眼」的成本压到常数级。
func (s *Server) handleCatalog(ctx context.Context, _ *mcp.CallToolRequest, in CatalogArgs) (*mcp.CallToolResult, any, error) {
	kind := strings.ToLower(strings.TrimSpace(in.Kind))
	if kind == "" {
		kind = "all"
	}
	// 未知 kind **报错**，不静默返回空列表（2026-10-03 外部实测）：
	// 传错值时旧行为是「空数组」，写脚本的人会以为「这个对象真的不存在」——
	// 一条「查错了」的报错能省掉整轮排查。合法值同时写在入参描述里（两处同源）。
	if !knownCatalogKinds[kind] {
		return toolError("BAD_ARGUMENT", "gear_catalog 的 kind="+strconv.Quote(in.Kind)+" 不是合法值",
			"合法值："+kindsHint(knownCatalogKinds)), nil, nil
	}
	limit := in.Limit
	if limit <= 0 {
		limit = 100
	}

	items := []map[string]any{}
	counts := map[string]int{}
	add := func(k string, m map[string]any) {
		counts[k]++
		items = append(items, m)
	}

	if kind == "all" || kind == "tool" {
		tools, err := s.reg.ListTools()
		if err != nil {
			return toolErrorFrom("CATALOG_FAILED", err), nil, nil
		}
		for _, t := range tools {
			states := make([]string, 0, len(t.Versions))
			for _, v := range t.Versions {
				states = append(states, v.Version+":"+string(v.State))
			}
			add("tool", map[string]any{
				"kind": "tool", "id": t.ID, "name": t.Name, "description": t.Description,
				"runtime": string(t.Runtime), "capabilities": t.Capabilities, "versions": states,
			})
		}
	}
	if kind == "all" || kind == "flow" {
		flows, err := s.reg.ListFlows()
		if err != nil {
			return toolErrorFrom("CATALOG_FAILED", err), nil, nil
		}
		for _, f := range flows {
			add("flow", map[string]any{
				"kind": "flow", "id": f.ID, "name": f.Name, "description": f.Description,
				"steps": len(f.Steps), "long_running": hasServiceStep(f),
			})
		}
	}
	if kind == "all" || kind == "hook" {
		hooks, err := s.reg.ListHooks()
		if err != nil {
			return toolErrorFrom("CATALOG_FAILED", err), nil, nil
		}
		for _, h := range hooks {
			add("hook", map[string]any{
				"kind": "hook", "id": h.ID, "target": h.Target, "stage": h.Stage,
				"action": h.Action, "criticality": string(h.Criticality),
			})
		}
	}
	if kind == "all" || kind == "execution" {
		for _, e := range s.execs.List() {
			m := map[string]any{
				"kind": "execution", "id": e.ID, "flow_id": e.FlowID,
				"status": e.Status, "started_at": e.StartedAt,
			}
			if e.Error != "" {
				m["error"] = e.Error
			}
			add("execution", m)
		}
	}
	if kind == "all" || kind == "app" {
		res, err := s.appList()
		if err == nil {
			for _, it := range res.Items {
				add("app", map[string]any{
					"kind": "app", "id": it.AppID, "flow_id": it.FlowID,
					"state": it.State, "pid": it.PID, "ports": it.Ports,
				})
			}
		}
	}

	total := len(items)
	offset := in.Offset
	if offset < 0 {
		offset = 0
	}
	if offset > total {
		offset = total
	}
	items = items[offset:]
	truncated := false
	nextOffset := 0
	if len(items) > limit {
		items = items[:limit]
		truncated = true
		nextOffset = offset + limit
	}
	return nil, map[string]any{
		"items": items, "total": total, "truncated": truncated, "counts": counts,
		"offset": offset, "limit": limit, "next_offset": nextOffset,
	}, nil
}

// knownCatalogKinds / knownDescribeKinds 是两个只读工具的 kind 白名单。
//
// 为什么要显式白名单而不靠 switch 的 default 兜底：catalog 的旧行为是**静默返回空列表**
// （2026-10-03 外部实测）——传错 kind 的人会以为「这个对象真的不存在」。
// 「查错了」与「没有」必须能被区分，否则一次笔误就要耗掉一整轮排查。
var (
	knownCatalogKinds = map[string]bool{
		"all": true, "tool": true, "flow": true, "hook": true, "execution": true, "app": true,
	}
	knownDescribeKinds = map[string]bool{
		"tool": true, "flow": true, "hook": true, "app": true,
	}
)

// kindsHint 把白名单拼成 "a | b | c"（错误信息用；map 迭代无序，故先排序保证可复现）。
func kindsHint(known map[string]bool) string {
	names := make([]string, 0, len(known))
	for k := range known {
		names = append(names, k)
	}
	sort.Strings(names)
	return strings.Join(names, " | ")
}

// handleDescribe 返回单个对象的完整定义。
func (s *Server) handleDescribe(ctx context.Context, _ *mcp.CallToolRequest, in DescribeArgs) (*mcp.CallToolResult, any, error) {
	kind := strings.ToLower(strings.TrimSpace(in.Kind))
	id := strings.TrimSpace(in.ID)
	if id == "" {
		return toolError("BAD_ARGUMENT", "id 不能为空", "先用 gear_catalog 看有哪些对象"), nil, nil
	}
	switch kind {
	case "tool":
		t, err := s.reg.GetTool(id)
		if err != nil {
			return toolErrorFrom("DESCRIBE_FAILED", err), nil, nil
		}
		if t == nil {
			return toolError("NOT_FOUND", "Tool 不存在: "+id, "先用 gear_catalog(kind=tool) 确认 id"), nil, nil
		}
		return nil, map[string]any{"kind": "tool", "object": t}, nil
	case "flow":
		f, err := s.reg.GetFlow(id)
		if err != nil {
			return toolErrorFrom("DESCRIBE_FAILED", err), nil, nil
		}
		if f == nil {
			return toolError("NOT_FOUND", "Flow 不存在: "+id, "先用 gear_catalog(kind=flow) 确认 id"), nil, nil
		}
		return nil, map[string]any{"kind": "flow", "object": f, "long_running": hasServiceStep(f)}, nil
	case "hook":
		h, err := s.reg.GetHook(id)
		if err != nil {
			return toolErrorFrom("DESCRIBE_FAILED", err), nil, nil
		}
		if h == nil {
			return toolError("NOT_FOUND", "Hook 不存在: "+id, "先用 gear_catalog(kind=hook) 确认 id"), nil, nil
		}
		return nil, map[string]any{"kind": "hook", "object": h}, nil
	case "app":
		// 2026-09-26：App 清单入 registry 之后，「一个 app」变成三样互不包含的东西——
		//
		//	运行态（台账 apps/<id>.json）：起没起、pid、端口
		//	清单   （registry.apps）：entry_flow、静态资源、依赖声明
		//	组成   （按 namespace 前缀反查）：它名下有哪些 Tool/Flow/Hook
		//
		// 三者各自可以单独存在（只起过没清单 / 有清单没起过 都是合法状态），
		// 所以**任一份存在就不报 NOT_FOUND**，缺的那份如实为 null。
		// `object` 保持运行态快照不变（既有调用方与测试依赖它）。
		regPath, appsDir := s.appEnv()
		st, stErr := app.Status(app.StatusOptions{RegistryPath: regPath, AppsDir: appsDir, AppID: id, TailLogs: 20})
		a, aErr := s.reg.GetApp(id)
		if aErr != nil {
			return toolErrorFrom("DESCRIBE_FAILED", aErr), nil, nil
		}
		if stErr != nil && a == nil {
			// 报错误导（2026-10-03 外部实测）：原文只是一句「台账里没有这个 app_id」，
			// 使用者会以为 Gear 没有「应用」这种对象，转而去建 Tool/Flow 心智模型。
			// 这里把「应用是哪三份东西」一次说清，并给出两条正确路径。
			return toolError("APP_NOT_FOUND", stErr.Error(),
				"应用 = 含 mode:service step 的 Flow，由三份互不包含的东西组成：运行态（台账 apps/<id>.json）、"+
					"清单（registry.apps 的 entry_flow）、组成（按 id 前缀反查的 Tool/Flow/Hook）；"+
					"本条说明三份都没有。只想看定义用 gear_describe(kind=flow)，"+
					"要建运行态用 gear_app(action=start, flow_id=…)，已有应用用 gear_app(action=list)",
			), nil, nil
		}
		out := map[string]any{"kind": "app", "object": nil, "manifest": nil, "composition": nil}
		if stErr == nil {
			out["object"] = st
		}
		if a != nil {
			out["manifest"] = a
			out["composition"] = s.appComposition(a)
		}
		return nil, out, nil
	default:
		return toolError("BAD_ARGUMENT", "kind 必须是 "+kindsHint(knownDescribeKinds)+"（实际: "+in.Kind+"）",
			"先用 gear_catalog 看有哪些对象"), nil, nil
	}
}

// appComposition 反查「这个应用由什么组成」。
//
// 前缀规则的唯一实现是 `model.App.Owns`（写入期 run 期的 `checkNamespace` 与运行期守卫
// 都调它，见 IDENTITY_CONTEXT_DESIGN.md §4.2）：id 以 `<appID>__` 或 `<appID>.` 开头即算
// 该应用名下；entry_flow 另外单算（它的 id 可能不带前缀，而它就是这个应用的入口）。
//
// 这是 2026-09-25 那条「没有任何端点能回答一个 App 由哪些 Tool/Flow/静态资源/数据表组成」
// 缺口的补法之一：先让 describe(app) 说清楚组成，导出能力（gear_export）再复用同一份判定。
//
// 静态资源只报**清单里声明的路径**：gear 从不落地这些文件（由调用方自己的文件工具写），
// 所以这里给的是「应该有哪些」，不是「磁盘上有哪些」——后者要用 `gear doctor`。
// 归属判定的实现在 internal/artifact.Composition（导出与 describe 同一份判定——
// 「describe 说这个应用有 3 个 Tool、导出出来只有 2 个」这类不一致必须不可能发生）。
func (s *Server) appComposition(a *model.App) map[string]any {
	c := artifact.Composition(s.reg, a)
	return map[string]any{
		"entry_flow":   c.EntryFlow,
		"tools":        c.Tools,
		"flows":        c.Flows,
		"hooks":        c.Hooks,
		"static_files": c.StaticFiles,
		"requires":     a.Requires,
	}
}

// handleCapabilities 返回内核原语 + 已注册 ext.* 的能力目录（含方法级 schema）。
//
// 支持 name/kind 过滤（评审 I-7）：它是 Instructions 铁律要求「写 step.call 前必看」的
// 高频工具，而一次全量会随 ext.* 注册数线性膨胀——每个返回都占模型上下文（MCP 场景的
// 代价远高于 REST）。加两个可选过滤键，让「只想确认 db 有哪些方法」不必付全量的账。
func (s *Server) handleCapabilities(ctx context.Context, _ *mcp.CallToolRequest, in CapabilitiesArgs) (*mcp.CallToolResult, any, error) {
	agent := s.engine.AIAgent()
	if agent == nil {
		return toolError("CAPABILITIES_UNAVAILABLE", "扩展目录未初始化（引擎未装配 AIAgent）"), nil, nil
	}
	caps, err := agent.CapabilitiesMethod()
	if err != nil {
		return toolErrorFrom("CAPABILITIES_UNAVAILABLE", err), nil, nil
	}
	list, _ := caps.([]map[string]any)

	wantName := strings.TrimSpace(in.Name)
	wantKind := strings.ToLower(strings.TrimSpace(in.Kind))
	allNames := make([]string, 0, len(list))
	for _, c := range list {
		if n, _ := c["name"].(string); n != "" {
			allNames = append(allNames, n)
		}
	}
	sort.Strings(allNames)

	if wantName != "" || wantKind != "" {
		filtered := make([]map[string]any, 0, len(list))
		for _, c := range list {
			if wantName != "" {
				if n, _ := c["name"].(string); n != wantName {
					continue
				}
			}
			if wantKind != "" {
				if k, _ := c["kind"].(string); !strings.EqualFold(k, wantKind) {
					continue
				}
			}
			filtered = append(filtered, c)
		}
		list = filtered
	}
	out := objectResult("capabilities", list)
	out["total"] = len(list)
	out["filter_name"] = wantName
	out["filter_kind"] = wantKind
	if len(list) == 0 {
		// 过滤后为空**不是**错误：如实返回空清单 + 可用能力名，免得智能体以为「环境没装东西」。
		out["hint"] = "没有匹配的能力；可用能力：" + strings.Join(allNames, ", ")
	}
	return nil, out, nil
}

// handleDoctor 做环境自检。
//
// 它回答的是一个**智能体自己查不到**的问题：起不来到底是 YAML 写错了，还是环境没准备好
// （侧车没 build、端口被占、应用台账里有遗留进程）。没有它，智能体只会反复重写 YAML。
//
// 判据本体在 internal/doctor（**唯一一份实现**）：同一份检查也由 `gear doctor` 在 CLI 侧使用，
// 两面结论必须一致——两份实现必然漂移，而 doctor 一旦漂移就再也不能被信任。
func (s *Server) handleDoctor(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
	rep, err := doctor.Check(doctor.Options{
		Registry:     s.reg,
		Engine:       s.engine,
		RegistryPath: s.registryPath,
		AppsDir:      s.appsDir,
	})
	if err != nil {
		return toolErrorFrom("DOCTOR_FAILED", err), nil, nil
	}
	return nil, rep.Map(), nil
}
