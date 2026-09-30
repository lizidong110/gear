package mcp

// tools_read.go：只读类工具 —— guide / catalog / describe / capabilities / doctor。
//
// 只读工具全部标 ReadOnlyHint，让客户端能在 UI 上区分「看看」与「会改东西」。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"gopkg.in/yaml.v3"

	"gear/internal/app"
	"gear/internal/artifact"
	"gear/internal/cap"
	"gear/internal/engine"
	"gear/internal/gearhome"
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
			return toolErrorFrom("APP_NOT_FOUND", stErr), nil, nil
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
		return toolError("BAD_ARGUMENT", "kind 必须是 tool | flow | hook | app（实际: "+in.Kind+")",
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
func (s *Server) handleDoctor(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
	warnings := []string{}

	tools, err := s.reg.ListTools()
	if err != nil {
		return toolErrorFrom("DOCTOR_FAILED", err), nil, nil
	}
	flows, err := s.reg.ListFlows()
	if err != nil {
		return toolErrorFrom("DOCTOR_FAILED", err), nil, nil
	}
	hooks, err := s.reg.ListHooks()
	if err != nil {
		return toolErrorFrom("DOCTOR_FAILED", err), nil, nil
	}

	loaded := []string{}
	if agent := s.engine.AIAgent(); agent != nil {
		if caps, err := agent.CapabilitiesMethod(); err == nil {
			if list, ok := caps.([]map[string]any); ok {
				for _, c := range list {
					if n, _ := c["name"].(string); strings.HasPrefix(n, "ext.") {
						loaded = append(loaded, n)
					}
				}
			}
		}
	}
	sort.Strings(loaded)

	missing := missingSidecars(loaded)
	for _, m := range missing {
		warnings = append(warnings, fmt.Sprintf(
			"能力 %s 可用但未找到对应侧车二进制 %s（已找过：%s）—— 若用到该能力，先构建（scripts/build.sh）",
			m.Ext, m.Expected, strings.Join(m.Searched, "; ")))
	}

	// 「声明 ↔ 实现」一致性校验：这条判据此前只有 plugin/verify.py 有，gear 自身与 MCP
	// 都拿不到，于是写完 extensions.yaml 只能盲发、等运行期调用才炸。这里在动手前说清。
	extCfgPath := extensionConfigPath()
	caps := []capsIssue{}
	for _, spec := range extensionServiceSpecs(extCfgPath) {
		c := probeServiceCaps(spec)
		caps = append(caps, c)
		switch c.State {
		case "mismatch":
			warnings = append(warnings, fmt.Sprintf(
				"能力 %s 声明了方法 %v，但侧车 %s 的 /caps 未实现其中的 %v —— 调用会报「没有方法」；修侧车或改声明",
				c.Ext, c.Declared, c.Listen, c.Missing))
		case "probe_failed":
			warnings = append(warnings, fmt.Sprintf(
				"能力 %s 的侧车在 %s 已起，但 /caps 探测失败 —— service ext 必须实现 /caps（见 gear_guide(\"sidecar\") §3）",
				c.Ext, c.Listen))
		}
		// sidecar_not_running 不告警：service ext 只在应用运行时才被拉起，
		// doctor 常在应用没跑时被调用——报成错误会淹掉真问题。
	}

	regPath, appsDir := s.appEnv()
	// 报**解析后**的台账目录（不是入参里的空串）：智能体要拿它去 find / 看日志，
	// 给一个空路径等于让他自己猜（AppDir 的默认规则见 app.ResolveAppsDir）。
	resolvedAppsDir := appsDir
	if d, derr := app.ResolveAppsDir(regPath, appsDir); derr == nil {
		resolvedAppsDir = d
	}
	appsOut := []map[string]any{}
	orphanOut := []map[string]any{}
	portsOut := map[string]any{}
	// 台账只读一次：doctor 是自检，不该为了多一处展示而重复触碰运行期状态。
	if list, listErr := s.appList(); listErr == nil {
		for _, it := range list.Items {
			appsOut = append(appsOut, map[string]any{
				"app_id": it.AppID, "flow_id": it.FlowID, "state": it.State,
				"pid": it.PID, "ports": it.Ports, "log_path": it.LogPath,
			})
			for _, p := range it.Ports {
				state := "free"
				if p.Listening {
					state = fmt.Sprintf("in-use(pid=%d)", it.PID)
				}
				portsOut[fmt.Sprint(p.Port)] = state
			}
		}
		for _, o := range list.Orphan {
			orphanOut = append(orphanOut, map[string]any{
				"app_id": o.AppID, "reason": o.Reason, "pid": o.PID, "log_path": o.LogPath,
			})
			warnings = append(warnings, fmt.Sprintf("台账里有遗留应用 %s（%s，pid=%d）——已如实汇报，未自动清理；确认后可 gear_app(action=stop, app_id=%s)",
				o.AppID, o.Reason, o.PID, o.AppID))
		}
	} else {
		warnings = append(warnings, "读取应用台账失败: "+listErr.Error())
	}

	ai := aiReadiness()
	if ready, _ := ai["ready"].(bool); !ready {
		warnings = append(warnings, fmt.Sprintf(
			"AI 能力未就绪（%s）：%v —— 修法：%v", ai["provider"], ai["blocked"], ai["how_to_fix"]))
	}

	return nil, map[string]any{
		"registry": map[string]any{"path": regPath, "tools": len(tools), "flows": len(flows), "hooks": len(hooks)},
		"extensions": map[string]any{
			"config":           extCfgPath,
			"declared":         extensionDeclared(extCfgPath),
			"loaded":           loaded,
			"missing_sidecars": missing,
			// caps_check 是「声明 ↔ 实现」的实测结果（含 ok / 未运行 / 失败 全部状态，
			// 不只列不匹配的——只给异常清单会让人以为「没列出=已校验通过」）。
			"caps_check": caps,
		},
		"ai":       ai,
		"ports":    portsOut,
		"apps":     appsOut,
		"orphan":   orphanOut,
		"apps_dir": resolvedAppsDir,
		"warnings": warnings,
	}, nil
}

// sidecarIssue 是一条「声明了某扩展、但对应侧车二进制不存在」的记录。
//
// ⚠ 字段必须**导出且带 json tag**：本结构体被 handleDoctor 直接序列化进响应，
// 而未导出字段 Marshal 出来是 `{}`——2026-09-22 实测响应里 `missing_sidecars` 是
// `[{},{},{}]`，真正的内容只藏在 warnings 的文本里，机器读不到（智能体要解析清单，
// 不该逼它去正则匹配中文告警）。
type sidecarIssue struct {
	Ext      string `json:"ext"`
	Expected string `json:"expected"`
	// Searched 是**实际找过的目录**，随告警一并报出：只说「没找到」会让智能体去猜
	// 「是不是没编译」，而它真正需要知道的是「我在这些位置找过」——据此才能判断是
	// 「产物缺失」还是「找错地方」。
	Searched []string `json:"searched"`
}

// missingSidecars 按产物命名约定检查各 ext.* 的侧车是否就绪。
//
// 约定：`ext.<name>` → `<name>-sidecar(.exe)`，落点由 gearhome.ArtifactDirs() 给出
// （**以可执行文件目录为主锚点，不依赖 cwd** —— 客户端以任意 cwd 派生 `gear mcp` 时，
// 此前只看 `./build/plugin` 会把一台侧车齐全的机器误报成缺侧车）。
// 只对在产物目录里找不到的报缺——检查不到就**报缺**而不是假装它就绪
// （宁可多提示一句，也不要让智能体把「侧车缺失」误判成「YAML 写错」）。
//
// 注意：不是每个 ext 都需要侧车（`ext.ai` 是进程内实现），所以这里只报「找到了命名约定但
// 文件不在」的情况，不做「少了谁」的穷举——穷举必然误报。
func missingSidecars(loaded []string) []sidecarIssue {
	dirs := gearhome.ArtifactDirs()
	out := []sidecarIssue{}
	for _, name := range loaded {
		base := strings.TrimPrefix(name, "ext.")
		if base == "" || base == "ai" || base == "registry" {
			continue // 进程内扩展，无独立侧车
		}
		candidates := []string{}
		for _, dir := range dirs {
			candidates = append(candidates, filepath.Join(dir, base+"-sidecar"))
			candidates = append(candidates, filepath.Join(dir, base+"-sidecar.exe"))
		}
		found := false
		for _, c := range candidates {
			if st, err := os.Stat(c); err == nil && !st.IsDir() {
				found = true
				break
			}
		}
		if !found {
			// 只在**该 ext 确实有独立侧车**的情况下报缺：判据是产物目录里存在
			// 「其它」*-sidecar 产物（说明本项目确实用侧车形态），否则不猜。
			if len(hasAnySidecarArtifact(dirs)) > 0 {
				out = append(out, sidecarIssue{
					Ext:      name,
					Expected: filepath.Join(dirs[0], base+"-sidecar"),
					Searched: dirs,
				})
			}
		}
	}
	return out
}

// hasAnySidecarArtifact 返回「确实存在 *-sidecar 产物」的目录列表（空 = 本机走的是非侧车形态）。
func hasAnySidecarArtifact(dirs []string) []string {
	var out []string
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() && strings.Contains(e.Name(), "-sidecar") {
				out = append(out, dir)
				break
			}
		}
	}
	return out
}

// ---- 「声明 ↔ 实现」一致性校验（/caps 探测）----
//
// 存在理由：这条判据此前**只活在 plugin/verify.py**（一个 Python 测试脚本）里，gear 自身
// 与 MCP 都拿不到——于是「extensions.yaml 声明了某方法、侧车其实没实现」只能等运行期
// 调用才炸，而智能体写完声明后**没有任何手段自验**。doctor 是唯一能在动手前说清这事的地方。
//
// 只校验 service ext：它长驻且按约定暴露 `/caps`，可直接探测。once 型侧车要起进程才能问、
// 且没有 /caps 约定，故不在范围内——**如实标注未校验，不假装校验过**（不静默降级）。

// serviceExtSpec 是一条 service ext 声明的最小投影（只取校验需要的两个字段）。
type serviceExtSpec struct {
	Name    string
	Listen  string
	Methods []string
}

// extensionServiceSpecs 读扩展声明文件里 service ext 的 listen 与 methods。
//
// 解析失败**返回空**：与 extensionDeclared 同款口径——配置文件的语法问题会在装配期以
// 更准确的方式暴露，doctor 的价值是「尽量说清现状」而不是再报一遍语法错。
func extensionServiceSpecs(path string) []serviceExtSpec {
	if path == "" {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var cfg map[string]struct {
		Methods []string `yaml:"methods"`
		Service bool     `yaml:"service"`
		Listen  string   `yaml:"listen"`
	}
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return nil
	}
	out := []serviceExtSpec{}
	for k, v := range cfg {
		if !strings.HasPrefix(k, "ext.") || !v.Service || v.Listen == "" {
			continue // once 型 / 未声明 listen 的，不在探测范围
		}
		out = append(out, serviceExtSpec{Name: k, Listen: v.Listen, Methods: v.Methods})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// capsIssue 是一条「声明的方法与侧车真实实现不一致」的记录。
//
// ⚠ 字段必须**导出且带 json tag**：理由同 sidecarIssue（2026-09-22 实测：未导出字段
// Marshal 出来是 `{}`，智能体只能去正则匹配中文告警才能拿到内容）。
type capsIssue struct {
	Ext         string   `json:"ext"`
	Listen      string   `json:"listen"`
	State       string   `json:"state"` // ok / mismatch / sidecar_not_running / probe_failed
	Declared    []string `json:"declared"`
	Implemented []string `json:"implemented"`
	// Missing 是「声明了但侧车没实现」的方法；为空即一致。
	Missing []string `json:"missing"`
}

// capsProbeTimeout 是单次 /caps 探测的超时。doctor 是诊断工具，不该被一个没响应的
// 侧车挂住（侧车未运行时 connect 会立刻 refused，超时是给「连上了但不应答」兜底）。
const capsProbeTimeout = 2 * time.Second

// probeServiceCaps 探测一个 service ext 的 /caps，比对「声明的 methods ⊆ 实现的 methods」。
//
// 四种状态必须分清（不许合并成一句「校验失败」——那会让智能体无从下手）：
//   - sidecar_not_running：连不上。**不是错误**——service ext 只在应用运行时才被拉起，
//     doctor 常在应用没跑的时候被调用；报成错误会淹掉真问题。
//   - probe_failed：连上了但拿不到合法 /caps。**真问题**：service ext 必须实现 /caps。
//   - mismatch：拿到了，但声明的方法有缺。**真问题**：调用会报「没有方法」。
//   - ok：一致。
func probeServiceCaps(spec serviceExtSpec) capsIssue {
	issue := capsIssue{Ext: spec.Name, Listen: spec.Listen, State: "sidecar_not_running"}
	client := &http.Client{Timeout: capsProbeTimeout}
	resp, err := client.Get("http://" + spec.Listen + "/caps")
	if err != nil {
		return issue // 连不上 = 侧车没起，不是配置错
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		issue.State = "probe_failed"
		return issue
	}
	var body struct {
		Result struct {
			Methods []string `json:"methods"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		issue.State = "probe_failed"
		return issue
	}
	issue.Implemented = body.Result.Methods
	issue.Declared = spec.Methods
	got := map[string]bool{}
	for _, m := range issue.Implemented {
		got[m] = true
	}
	for _, m := range spec.Methods {
		if !got[m] {
			issue.Missing = append(issue.Missing, m)
		}
	}
	if len(issue.Missing) > 0 {
		issue.State = "mismatch"
		return issue
	}
	issue.State = "ok"
	return issue
}

// aiReadiness 汇报 ext.ai 的就绪情况（**只报存在与否，绝不回显密钥明文**）。
//
// 存在的理由（2026-09-22 实测缺陷）：没配 Key 时，只能等业务跑起来撞到厂商的
// `LLM HTTP 401 {"code":30014,"message":"Token is invalid."}` —— 那句话既没说缺哪个
// 环境变量，也没说去哪配。doctor 的职责正是「把环境问题与 YAML 问题一次分开」，
// 而 AI 凭据是这块拼图里此前唯一没被覆盖的。
//
// 优先级链与 engine 的 resolveLLMConfig 保持一致：调用参数 > 环境变量 > 配置库 > 内置默认；
// 这里看不到「调用参数」这一层（那是每次调用才有的），但环境变量与配置库都能看到，
// 故 ready=false 才真正意味着「这两层都没有 key」——不代表用户不能靠 api_key 参数跑通，
// 文案里写清楚这一点。
func aiReadiness() map[string]any {
	pick := func(env, cfgKey, def string) string {
		if v := strings.TrimSpace(os.Getenv(env)); v != "" {
			return v
		}
		if v := configValue(cfgKey); v != "" {
			return v
		}
		return def
	}
	provider := pick("GEAR_AI_PROVIDER", "ai.provider", "siliconflow")
	keyFromConfig := strings.TrimSpace(configValue("ai.api_key")) != ""

	out := map[string]any{
		"provider": provider,
		// base_url / model 留空即用厂商默认，故空串是**有意**的合法值（不是缺失）。
		"base_url":     pick("GEAR_AI_BASE_URL", "ai.base_url", ""),
		"model":        pick("GEAR_AI_MODEL", "ai.model", ""),
		"key_env":      "GEAR_AI_API_KEY",
		"key_from_env": os.Getenv("GEAR_AI_API_KEY") != "",
		// 配置库层：用 `gear config set ai.api_key ...` 配过即为 true（与环境变量同效）。
		"key_from_config": keyFromConfig,
		"note_priority":   "调用参数 > 环境变量 > 配置库（gear config set ai.*）> 内置默认（见 engine.resolveLLMConfig）",
	}

	if strings.EqualFold(provider, "mock") {
		out["ready"] = true
		out["note"] = "provider=mock：离线桩实现，不需要 API Key"
		return out
	}
	if !out["key_from_env"].(bool) && !keyFromConfig {
		out["ready"] = false
		out["blocked"] = "环境变量 GEAR_AI_API_KEY 与配置库 ai.api_key 都没有：任何 ext.ai.* 调用都会以厂商 401 失败（报错文本是厂商原文，不含病因）"
		out["how_to_fix"] = strings.Join([]string{
			"① 用 gear config set ai.api_key <key> 写进配置库（推荐：配一次，所有进程生效，不必再 export）",
			"② 或设环境变量 GEAR_AI_API_KEY（经 MCP/stdio 使用时写进客户端的 env 块）",
			"③ 或让 Flow 用 ${secrets.NAME} 引用密钥源（密钥由人在 CLI 播种，MCP 不写密钥）",
			"④ 或在每次调用时显式传 api_key 参数（适合 BYOK 的浏览器端场景）",
		}, "；")
		return out
	}
	out["ready"] = true
	return out
}

// configValue 读配置库里的一项（AI 缺省配置层）。
//
// 与 engine.resolveLLMConfig 的第三层**同源同一个库**（gearhome.ConfigDBPath），否则 doctor
// 会指着一份「用户明明配过」的配置报未配置——那正是 doctor 最不该犯的错（假红比不报更坏）。
// 库不存在 / 打不开 / 该项未配置一律返回空串：doctor 只如实汇报，不替用户创建任何东西。
func configValue(key string) string {
	store := cap.NewConfigStore(gearhome.ConfigDBPath())
	defer func() { _ = store.Close() }()
	v, err := store.Get(key)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(v)
}

// extensionConfigPath 返回扩展配置文件路径（与装配期同一解析器：GEAR_EXT_CONFIG 优先，
// 否则按「可执行文件同目录 → plugin/ → cwd」候选序列定位）。
//
// 与 engine.ResolveExtensionConfigPath 共用一份实现是有意为之：doctor 说的位置必须就是
// 装配期真正读的那个文件，两套候选序列必然漂移（「doctor 说有配置、实际没加载」这类假绿
// 极难排查）。显式路径不存在时返回空串——doctor 只负责如实汇报，报错留给装配期。
func extensionConfigPath() string {
	p, err := engine.ResolveExtensionConfigPath()
	if err != nil {
		return ""
	}
	return p
}

// extensionDeclared 读扩展声明文件里的 ext 名清单（供 doctor 展示「声明了哪些」）。
//
// 解析失败**不报错**：doctor 的价值是「尽量说清现状」，而配置文件的语法问题会在装配期
// 以更准确的方式暴露；这里返回空并让调用方如实标注即可。
func extensionDeclared(path string) []string {
	if path == "" {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var cfg map[string]struct {
		Cmd []string `yaml:"cmd"`
	}
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return nil
	}
	out := make([]string, 0, len(cfg))
	for k := range cfg {
		if strings.HasPrefix(k, "ext.") {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}
