package mcp

// tools_write.go：写入类工具 —— validate / apply / delete / promote，以及 Gear App Bundle 的解析。
//
// 这一组的核心是**把 fail-closed 从运行期前移到写作期**：
//   gear_validate 是智能体的「编译器」（收 YAML、不落库、可无限次重试），
//   gear_apply 是唯一的写入口（声明式、原子、幂等）。
// 旧工具面（create_tool/create_flow/create_hook + validate_flow 只收 id）做不到这件事——
// 智能体只能「先写坏的进去再改」，与 fail-closed 精神相反（§1 P4）。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"gopkg.in/yaml.v3"

	"gear/internal/cmdline"
	"gear/internal/engine"
	"gear/internal/gearhome"
	"gear/internal/model"
	"gear/internal/registry"
)

// appManifest 是 bundle 的 `kind: App` 清单。
//
// ⚠ **它不入 registry**：bundle 只是「多文档 YAML 的组织约定 + 依赖声明」，落库后仍然只有
// Tool/Flow/Hook。这保持 PRD 的非目标（「只有 Flow + app run」，没有 App 实体），
// 也避免为了「创建应用」而在内核里长出一个新模型（§7）。
type appManifest struct {
	ID          string `yaml:"id" json:"id"`
	Name        string `yaml:"name" json:"name"`
	Description string `yaml:"description" json:"description"`
	EntryFlow   string `yaml:"entry_flow" json:"entry_flow"`
	Layout      struct {
		Workdir string `yaml:"workdir" json:"workdir"`
		Files   []struct {
			Path     string `yaml:"path" json:"path"`
			Required bool   `yaml:"required" json:"required"`
		} `yaml:"files" json:"files"`
	} `yaml:"layout" json:"layout"`
	Start struct {
		Restart string `yaml:"restart" json:"restart"`
		Ports   []int  `yaml:"ports" json:"ports"`
	} `yaml:"start" json:"start"`
	Requires struct {
		Secrets      []string `yaml:"secrets" json:"secrets"`
		Sidecars     []string `yaml:"sidecars" json:"sidecars"`
		Capabilities []string `yaml:"capabilities" json:"capabilities"`
	} `yaml:"requires" json:"requires"`
}

// bundle 是一个解析后的交付物。
type bundle struct {
	Apps  []*appManifest
	Tools []*model.Tool
	Flows []*model.Flow
	Hooks []*model.Hook
}

// parseBundle 把多文档 YAML 解析成 bundle。
//
// 分类规则（**优先看 kind，无 kind 时退化为字段启发式**）：
//
//	kind: App  → 应用清单（仅 apply/validate 识别）
//	kind: Tool/Flow/Hook → 对应类型
//	无 kind    → 有 steps 视为 Flow，有 stage 视为 Hook，否则视为 Tool
//	              （后两条兼容仓库里 examples/*.yaml 的历史写法）
//	未知 kind  → 明确报错（**不猜成 Tool**：猜错会让一份合法文档报出一堆莫名其妙的
//	              「runtime 非法」错，把真正的问题埋掉）
//
// 解析失败不中断：把问题作为诊断收集起来继续解析其余文档——一次报多条才是「编译器」该有的样子。
func parseBundle(text string) (*bundle, []engine.Diagnostic) {
	b := &bundle{}
	var diags []engine.Diagnostic
	for i, doc := range registry.SplitYAMLDocs([]byte(text)) {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		docNo := i + 1
		var probe map[string]any
		if err := yaml.Unmarshal([]byte(doc), &probe); err != nil {
			diags = append(diags, engine.Diagnostic{
				Code: "BAD_YAML", Field: fmt.Sprintf("doc[%d]", docNo),
				Message: fmt.Sprintf("第 %d 份文档不是合法 YAML: %v", docNo, err),
				Hint:    "常见原因：缩进不一致、冒号后缺空格、字符串里的特殊字符没加引号",
			})
			continue
		}
		if len(probe) == 0 {
			continue // 分隔符产生的空文档
		}
		switch classifyDoc(probe) {
		case "app":
			var a appManifest
			if err := yaml.Unmarshal([]byte(doc), &a); err != nil {
				diags = append(diags, badDocDiag(docNo, "App", err))
				continue
			}
			b.Apps = append(b.Apps, &a)
		case "tool":
			var t model.Tool
			if err := yaml.Unmarshal([]byte(doc), &t); err != nil {
				diags = append(diags, badDocDiag(docNo, "Tool", err))
				continue
			}
			b.Tools = append(b.Tools, &t)
		case "flow":
			var f model.Flow
			if err := yaml.Unmarshal([]byte(doc), &f); err != nil {
				diags = append(diags, badDocDiag(docNo, "Flow", err))
				continue
			}
			b.Flows = append(b.Flows, &f)
		case "hook":
			var h model.Hook
			if err := yaml.Unmarshal([]byte(doc), &h); err != nil {
				diags = append(diags, badDocDiag(docNo, "Hook", err))
				continue
			}
			b.Hooks = append(b.Hooks, &h)
		default:
			raw, _ := probe["kind"].(string)
			diags = append(diags, engine.Diagnostic{
				Code: "UNKNOWN_KIND", Field: fmt.Sprintf("doc[%d].kind", docNo),
				Message: fmt.Sprintf("第 %d 份文档的 kind=%q 未知（只认 App / Tool / Flow / Hook）", docNo, raw),
				Hint:    "bundle 的第一份通常是 `kind: App` 清单，其余是 `kind: Tool` / `kind: Flow` / `kind: Hook`",
			})
		}
	}
	return b, diags
}

func badDocDiag(docNo int, kind string, err error) engine.Diagnostic {
	return engine.Diagnostic{
		Code: "BAD_YAML", Field: fmt.Sprintf("doc[%d]", docNo),
		Message: fmt.Sprintf("第 %d 份文档按 %s 解析失败: %v", docNo, kind, err),
		Hint:    "对照 gear_guide(\"authoring\") 的全字段语义检查字段名与类型",
	}
}

// classifyDoc 判定一份文档的类型（见 parseBundle 的规则表）。
func classifyDoc(probe map[string]any) string {
	if k, ok := probe["kind"].(string); ok {
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "app":
			return "app"
		case "tool":
			return "tool"
		case "flow":
			return "flow"
		case "hook":
			return "hook"
		default:
			return "unknown"
		}
	}
	if _, isFlow := probe["steps"]; isFlow {
		return "flow"
	}
	if _, isHook := probe["stage"]; isHook {
		return "hook"
	}
	return "tool"
}

// validateResult 是 validateBundle 的产出。
type validateResult struct {
	Bundle *bundle
	Diags  []engine.Diagnostic
	// Warnings 是「定义本身合法、但环境未就绪」的提示（文件未落盘、密钥未播种、侧车未构建）。
	// 它们**不影响 valid**——把「环境没准备好」混进「定义非法」会让智能体去改本来正确的 YAML。
	Warnings []string
}

// Valid 报告是否没有致命诊断。
func (r *validateResult) Valid() bool { return len(r.Diags) == 0 }

// validateBundle 全量校验一个 bundle（不落库、不改状态、可无限次调用）。
func (s *Server) validateBundle(text string) *validateResult {
	b, diags := parseBundle(text)
	res := &validateResult{Bundle: b, Diags: diags, Warnings: []string{}}

	// 命名空间约束：bundle 含 App 清单时，其余对象的 id 必须带该应用的前缀。
	// 这是防「一次性生成污染 registry」的关键——registry 与 CLI/REST 共享（§7）。
	ns := ""
	if len(b.Apps) == 1 {
		ns = b.Apps[0].ID
	}
	if ns != "" {
		res.Diags = append(res.Diags, checkNamespace(b, ns)...)
	} else if len(b.Apps) > 1 {
		res.Diags = append(res.Diags, engine.Diagnostic{
			Code: "BAD_APP_MANIFEST", Field: "kind: App",
			Message: fmt.Sprintf("一个 bundle 里出现了 %d 份 App 清单（应只有一份）", len(b.Apps)),
			Hint:    "一个 bundle 描述一个应用：只保留一份 `kind: App`",
		})
	}

	// Tool / Hook 用 model 层校验（与注册期同一份规则）。
	for _, t := range b.Tools {
		if err := t.Validate(); err != nil {
			res.Diags = append(res.Diags, wrapModelDiag(engine.CodeBadToolStructure, t.ID, "tool 定义非法", err, "tool",
				"对照 gear_guide(\"authoring\") 的 Tool 字段表；runtime 只支持 process / lua / go-host")...)
		}
		// process 型 Tool 的 entry 可达性：把「路径写错」从运行期提前到写作期。
		ed, ew := checkToolEntryResolvable(t)
		res.Diags = append(res.Diags, ed...)
		res.Warnings = append(res.Warnings, ew...)
	}
	for _, h := range b.Hooks {
		if err := h.Validate(); err != nil {
			res.Diags = append(res.Diags, wrapModelDiag(engine.CodeBadHookStructure, h.ID, "hook 定义非法", err, "hook",
				"stage 只认 before_flow / before_step / after_step / on_error / on_success / before_promote")...)
		}
	}
	// Flow 用 engine 静态校验，并把 bundle 内待落库的 Tool 一并视为可见——
	// 否则「Tool 与 Flow 互相引用」的 bundle 会被自己判成 UNKNOWN_CALL_TARGET。
	for _, f := range b.Flows {
		res.Diags = append(res.Diags, s.engine.ValidateFlowStaticDiagsWith(f, b.Tools)...)
	}
	// App 清单：入口 Flow 必须可解析（bundle 内或已注册）。
	for _, a := range b.Apps {
		res.Diags = append(res.Diags, s.checkAppManifest(a, b)...)
		res.Warnings = append(res.Warnings, s.appManifestWarnings(a)...)
	}
	return res
}

// checkToolEntryResolvable 对 process 型 Tool 的 entry 做**静态可达性检查**。
//
// 存在的理由（2026-09-22 实测缺陷）：entry 指向的可执行文件不存在（或路径被解析坏）时，
// 原先要等运行期才炸，报错还是 `executable file not found in %PATH%`——把病因指向 PATH，
// 与「路径写错」无关。提前到校验期发现，智能体才能靠 code 改 YAML 自愈。
//
// 两件事：① 首 token 的可执行文件在不在（checkToolEntryExe，分层给结论）；
// ② entry 有没有把**相对路径**传给路径类 flag（checkToolEntryRelPathFlags）。
// 后者与前者同源——都是「这个 entry 换个位置跑就会静默指向别处」。
func checkToolEntryResolvable(t *model.Tool) ([]engine.Diagnostic, []string) {
	if t == nil || !strings.EqualFold(strings.TrimSpace(string(t.Runtime)), "process") {
		return nil, nil
	}
	diags, warns := checkToolEntryExe(t)
	return diags, append(warns, checkToolEntryRelPathFlags(t)...)
}

// checkToolEntryRelPathFlags 把「entry 里给 --registry / --apps-dir 传了相对路径」报成 warning。
//
// 判据与措辞都由 internal/cmdline 提供（与 CLI 的 tool register 共用一份，避免两处漂移），
// 这里只补上对象名。⚠ **只 warning 不 error**：相对值在「就地手跑」时是合法且常见的写法，
// 不能因为「换个 cwd 会坏」就拒绝注册一个眼下完全可用的 Tool——护栏的职责是把代价说清楚，
// 不是替人做决定（同「绝对路径才敢下 error」的分层口径）。
func checkToolEntryRelPathFlags(t *model.Tool) []string {
	flags := cmdline.RelPathFlagsInLine(t.Entry)
	if len(flags) == 0 {
		return nil
	}
	out := make([]string, 0, len(flags))
	for _, f := range flags {
		out = append(out, cmdline.RelPathFlagHint("tool "+t.ID, f))
	}
	return out
}

// checkToolEntryExe 是 checkToolEntryResolvable 的第一件事：entry 首 token 的可执行文件可达性。
//
// 分层给结论（宁缺勿误杀）：
//   - **绝对路径**：运行时与 cwd 无关，不存在即诊断（error）；
//   - **含分隔符的相对路径 / 裸名字**：运行时 cwd、PATH 都可能与当前进程不同，
//     找不到只给 warning——把「可能错」喊出来，但不假装确定。
func checkToolEntryExe(t *model.Tool) ([]engine.Diagnostic, []string) {
	if t == nil || !strings.EqualFold(strings.TrimSpace(string(t.Runtime)), "process") {
		return nil, nil
	}
	entry := strings.TrimSpace(t.Entry)
	if entry == "" {
		return nil, nil // 缺 entry 由 model.Validate 以 BAD_TOOL_STRUCTURE 报
	}
	spec, err := cmdline.Parse(entry)
	if err != nil {
		return []engine.Diagnostic{{
			Code:    engine.CodeEntryExeUnresolved,
			Message: fmt.Sprintf("tool %s 的 entry 无法解析: %v", t.ID, err),
			Field:   "tool(" + t.ID + ").entry",
			Hint:    `entry 是「程序 + 参数」的一行命令；引号与反斜杠写法见 gear_guide("authoring")`,
		}}, nil
	}
	if spec.NeedsShell || len(spec.Argv) == 0 {
		return nil, nil // 交给 shell 的行，程序由 shell 在 PATH 里找，静态判不了
	}
	argv0 := spec.Argv[0]
	sh, _ := cmdline.FindShell()
	if _, err := cmdline.ResolveExecutable(argv0, sh); err == nil {
		return nil, nil
	}
	field := "tool(" + t.ID + ").entry"
	if filepath.IsAbs(argv0) {
		hint := "确认路径拼写与构建产物位置：Windows 路径用正斜杠或反斜杠都可（反斜杠不再被当转义符）"
		if ph := gearhome.PosixPathHint(argv0); ph != "" {
			hint = ph
		}
		return []engine.Diagnostic{{
			Code:    engine.CodeEntryExeUnresolved,
			Message: fmt.Sprintf("tool %s 的 entry 首 token 指向的可执行文件不存在: %s", t.ID, argv0),
			Field:   field,
			Hint:    hint,
		}}, nil
	}
	if ph := gearhome.PosixPathHint(argv0); ph != "" {
		return nil, []string{fmt.Sprintf("tool %s 的 entry 疑似 MSYS 风格路径 %q：%s", t.ID, argv0, ph)}
	}
	return nil, []string{fmt.Sprintf(
		"tool %s 的 entry 首 token %q 在当前工作目录与 PATH 下都找不到。"+
			"运行时 cwd/PATH 可能与校验时不同，故只作提示；要消除歧义请改写成绝对路径", t.ID, argv0)}
}

// wrapModelDiag 把 model 层的校验错误包成诊断（带对象类型与 id，方便智能体定位）。
func wrapModelDiag(code, id, what string, err error, kind, hint string) []engine.Diagnostic {
	d := engine.Diagnostic{
		Code:    code,
		Message: fmt.Sprintf("%s %s: %v", kind, id, err),
		Field:   kind + "(" + id + ")",
		Hint:    hint,
	}
	if id == "" {
		d.Message = fmt.Sprintf("%s 定义缺少 id: %v", kind, err)
	}
	return []engine.Diagnostic{d}
}

// checkNamespace 校验 bundle 内所有 id 都带应用前缀。
func checkNamespace(b *bundle, appID string) []engine.Diagnostic {
	var out []engine.Diagnostic
	ok := func(id string) bool {
		return strings.HasPrefix(id, appID+"__") || strings.HasPrefix(id, appID+".")
	}
	check := func(kind, id string) {
		if id == "" || ok(id) {
			return
		}
		prefix := appID + "__（或 " + appID + ".）"
		out = append(out, engine.Diagnostic{
			Code:    "ID_NAMESPACE_VIOLATION",
			Message: fmt.Sprintf("%s 的 id %q 未带应用前缀 %s", kind, id, prefix),
			Field:   kind + ".id",
			Hint:    fmt.Sprintf("registry 与 CLI/REST 共享，无前缀会覆盖他人维护的对象；全部 id 改成 %s 开头", prefix),
		})
	}
	for _, t := range b.Tools {
		check("tool", t.ID)
	}
	for _, f := range b.Flows {
		check("flow", f.ID)
	}
	for _, h := range b.Hooks {
		check("hook", h.ID)
	}
	return out
}

// checkAppManifest 校验 App 清单的硬约束（入口 Flow 必须能解析）。
func (s *Server) checkAppManifest(a *appManifest, b *bundle) []engine.Diagnostic {
	var out []engine.Diagnostic
	if a.ID == "" {
		out = append(out, engine.Diagnostic{
			Code: "BAD_APP_MANIFEST", Field: "kind: App.id",
			Message: "App 清单缺少 id",
			Hint:    "id 是命名空间来源（其余 id 须以 <id>__ 或 <id>. 开头），必须有",
		})
		return out
	}
	if a.EntryFlow == "" {
		out = append(out, engine.Diagnostic{
			Code: "BAD_APP_MANIFEST", Field: "kind: App.entry_flow",
			Message: fmt.Sprintf("App %s 缺少 entry_flow", a.ID),
			Hint:    "entry_flow 指向应用入口 Flow（须含 mode: service 的长驻 step，否则应用起来就退出）",
		})
		return out
	}
	inBundle := false
	for _, f := range b.Flows {
		if f.ID == a.EntryFlow {
			inBundle = true
			break
		}
	}
	if inBundle {
		return out
	}
	f, err := s.reg.GetFlow(a.EntryFlow)
	if err != nil {
		out = append(out, engine.Diagnostic{
			Code: "BAD_APP_MANIFEST", Field: "kind: App.entry_flow",
			Message: fmt.Sprintf("校验 entry_flow %s 时读 registry 失败: %v", a.EntryFlow, err),
			Hint:    "先 gear_doctor 确认 registry 可读",
		})
		return out
	}
	if f == nil {
		out = append(out, engine.Diagnostic{
			Code: "UNKNOWN_CALL_TARGET", Field: "kind: App.entry_flow",
			Message: fmt.Sprintf("entry_flow %s 既不在本 bundle 里，也不是已注册的 Flow", a.EntryFlow),
			Hint:    "把它一起写进 bundle（id 须带应用前缀），或先用 gear_catalog(kind=flow) 确认 id",
		})
	}
	return out
}

// appManifestWarnings 报「定义合法但环境未就绪」的提示（不影响 valid）。
func (s *Server) appManifestWarnings(a *appManifest) []string {
	var out []string
	for _, f := range a.Layout.Files {
		if f.Path == "" {
			continue
		}
		if _, err := os.Stat(f.Path); err != nil {
			mark := "（required）"
			if !f.Required {
				mark = ""
			}
			out = append(out, fmt.Sprintf("[MISSING_FILE] %s 声明了文件 %s%s 但当前不存在——用你自己的文件工具先落盘；"+
				"相对路径依赖进程 cwd（派生进程继承 MCP 服务端的 cwd）", a.ID, f.Path, mark))
		}
	}
	for _, name := range a.Requires.Secrets {
		out = append(out, fmt.Sprintf("[MISSING_SECRET] %s 需要密钥 %s：MCP 不写密钥，请人在 CLI 播种（"+
			"Flow 里用 ${secrets.%s} 引用）", a.ID, name, name))
	}
	for _, sc := range a.Requires.Sidecars {
		base := strings.TrimPrefix(sc, "ext.")
		found := false
		for _, c := range []string{
			"build/plugin/" + base + "-sidecar", "build/plugin/" + base + "-sidecar.exe",
			"build/" + base + "-sidecar", "build/" + base + "-sidecar.exe",
		} {
			if st, err := os.Stat(c); err == nil && !st.IsDir() {
				found = true
				break
			}
		}
		if !found {
			out = append(out, fmt.Sprintf("[SIDECAR_NOT_BUILT] %s 依赖侧车 %s，但未在 build/ 下找到对应二进制——"+
				"先构建（bash scripts/build.sh）；缺了它应用起不来", a.ID, sc))
		}
	}
	return out
}

// ---- 工具注册与处理 ----

func (s *Server) registerWriteTools(ms *mcp.Server) {
	mcp.AddTool(ms, &mcp.Tool{
		Name: "gear_validate",
		Description: "校验 bundle YAML **但不落库**，返回字段级诊断（code/step/field/hint）。" +
			"写配置后、apply 前用它；失败可无限次重试，直到 valid:true。",
		Annotations:  readOnly("校验 YAML"),
		OutputSchema: outValidate,
	}, s.handleValidate)

	mcp.AddTool(ms, &mcp.Tool{
		Name: "gear_apply",
		Description: "把 bundle 落库（**唯一写入口**）：原子、幂等。多文档按依赖顺序 Tool→Flow→Hook 写入，" +
			"任一非法则整体不写。dry_run=true 等价于先用 validate 看一遍含冲突检测。",
		Annotations:  idempotent("落库定义"),
		OutputSchema: outApply,
	}, s.handleApply)

	mcp.AddTool(ms, &mcp.Tool{
		Name: "gear_delete",
		Description: "删除一个对象（tool/flow/hook）。被其它对象或运行中的执行引用时会被守卫拒绝，" +
			"错误信息会说明被谁引用。**破坏性操作**：先想清楚影响面。",
		Annotations:  destructive("删除对象"),
		OutputSchema: outDelete,
	}, s.handleDelete)

	mcp.AddTool(ms, &mcp.Tool{
		Name: "gear_promote",
		Description: "晋升 Tool 某个版本的状态（draft→testing→stable→deprecated，非法迁移会被状态机拒绝），" +
			"触发 before_promote 门禁。stable 后才能被默认引用使用。",
		Annotations:  idempotent("晋升版本"),
		OutputSchema: outPromote,
	}, s.handlePromote)
}

// handleValidate：收 YAML、返回诊断、**不落库**。
func (s *Server) handleValidate(ctx context.Context, _ *mcp.CallToolRequest, in ValidateArgs) (*mcp.CallToolResult, any, error) {
	if strings.TrimSpace(in.YAML) == "" {
		return toolError("BAD_ARGUMENT", "yaml 不能为空",
			"把 bundle 的多文档 YAML 传进来（第一份通常是 kind: App）"), nil, nil
	}
	res := s.validateBundle(in.YAML)
	return nil, map[string]any{
		"valid":       res.Valid(),
		"diagnostics": diagnosticsJSON(res.Diags),
		"warnings":    res.Warnings,
		"summary": map[string]any{
			"apps": len(res.Bundle.Apps), "tools": len(res.Bundle.Tools),
			"flows": len(res.Bundle.Flows), "hooks": len(res.Bundle.Hooks),
		},
	}, nil
}

// handleApply：唯一的写入口（声明式、原子、幂等）。
func (s *Server) handleApply(ctx context.Context, _ *mcp.CallToolRequest, in ApplyArgs) (*mcp.CallToolResult, any, error) {
	if strings.TrimSpace(in.Bundle) == "" {
		return toolError("BAD_ARGUMENT", "bundle 不能为空", "先用 gear_guide(\"examples\") 拿一个骨架"), nil, nil
	}
	res := s.validateBundle(in.Bundle)
	if !res.Valid() {
		// 校验不过就**一个字节都不写**（原子语义 REQ-REG-ATOMIC）：响应是成功返回 + valid:false，
		// 不是工具错误——「定义有问题」是正常结果，智能体据此改 YAML 再来。
		return nil, map[string]any{
			"valid":       false,
			"diagnostics": diagnosticsJSON(res.Diags),
			"warnings":    res.Warnings,
			"applied":     map[string]any{"tools": []string{}, "flows": []string{}, "hooks": []string{}},
		}, nil
	}
	if in.DryRun {
		return nil, map[string]any{
			"valid":       true,
			"dry_run":     true,
			"diagnostics": diagnosticsJSON(nil),
			"warnings":    res.Warnings,
			"would_apply": map[string]any{
				"tools":   ids(res.Bundle.Tools),
				"flows":   flowIDs(res.Bundle.Flows),
				"hooks":   hookIDs(res.Bundle.Hooks),
				"app":     appID(res.Bundle),
				"created": s.diffExisting(res.Bundle, false),
				"updated": s.diffExisting(res.Bundle, true),
			},
		}, nil
	}

	created, updated, err := s.persistBundle(res.Bundle, in.Overwrite)
	if err != nil {
		return toolErrorFrom("APPLY_FAILED", err), nil, nil
	}
	return nil, map[string]any{
		"valid":       true,
		"diagnostics": diagnosticsJSON(nil),
		"warnings":    res.Warnings,
		"applied": map[string]any{
			"tools": ids(res.Bundle.Tools),
			"flows": flowIDs(res.Bundle.Flows),
			"hooks": hookIDs(res.Bundle.Hooks),
			"app":   appID(res.Bundle),
		},
		"created": created,
		"updated": updated,
		"next":    "接着用 gear_app(action=start, flow_id=…, ports=[…]) 起应用（写入 ≠ 运行）",
	}, nil
}

// persistBundle 按依赖顺序落库：Tool → Flow → Hook。
//
// 顺序由**依赖**决定而不是文档顺序：Hook 的 action 常指向 Tool/Flow，Flow 的 step 常指向 Tool；
// 先落被依赖者，任何一步失败时前面已写的部分是可解释的（并在错误里列出）。
//
// overwrite=false 时，对「已存在且内容不同」的对象**拒绝覆盖**——防止智能体一次性生成把
// 人手工维护的定义悄悄改掉（§13 R4）。内容相同则视为幂等更新（updated 里如实列出）。
func (s *Server) persistBundle(b *bundle, overwrite bool) (created, updated []string, err error) {
	created, updated = []string{}, []string{}

	for _, t := range b.Tools {
		existing, gerr := s.reg.GetTool(t.ID)
		if gerr != nil {
			return created, updated, gerr
		}
		if existing != nil && !overwrite && !sameJSON(existing, t) {
			return created, updated, diagError("APPLY_CONFLICT", "tool("+t.ID+")",
				"确需覆盖就显式传 overwrite=true；否则换个 id（带应用前缀）",
				"Tool %s 已存在且内容不同，已拒绝覆盖（本次未写入；已写入：%v）", t.ID, created)
		}
		if err := s.reg.UpsertTool(t); err != nil {
			return created, updated, fmt.Errorf("写入 Tool %s 失败（已写入 %v）: %w", t.ID, created, err)
		}
		if existing == nil {
			created = append(created, "tool:"+t.ID)
		} else {
			updated = append(updated, "tool:"+t.ID)
		}
	}
	for _, f := range b.Flows {
		existing, gerr := s.reg.GetFlow(f.ID)
		if gerr != nil {
			return created, updated, gerr
		}
		if existing != nil && !overwrite && !sameJSON(existing, f) {
			return created, updated, diagError("APPLY_CONFLICT", "flow("+f.ID+")",
				"确需覆盖就显式传 overwrite=true；否则换个 id（带应用前缀）",
				"Flow %s 已存在且内容不同，已拒绝覆盖（本次未写入；已写入：%v）", f.ID, created)
		}
		if err := s.reg.UpsertFlow(f); err != nil {
			return created, updated, fmt.Errorf("写入 Flow %s 失败（已写入 %v）: %w", f.ID, created, err)
		}
		if existing == nil {
			created = append(created, "flow:"+f.ID)
		} else {
			updated = append(updated, "flow:"+f.ID)
		}
	}
	for _, h := range b.Hooks {
		existing, gerr := s.reg.GetHook(h.ID)
		if gerr != nil {
			return created, updated, gerr
		}
		if existing != nil && !overwrite && !sameJSON(existing, h) {
			return created, updated, diagError("APPLY_CONFLICT", "hook("+h.ID+")",
				"确需覆盖就显式传 overwrite=true；否则换个 id（带应用前缀）",
				"Hook %s 已存在且内容不同，已拒绝覆盖（本次未写入；已写入：%v）", h.ID, created)
		}
		if err := s.reg.UpsertHook(h); err != nil {
			return created, updated, fmt.Errorf("写入 Hook %s 失败（已写入 %v）: %w", h.ID, created, err)
		}
		if existing == nil {
			created = append(created, "hook:"+h.ID)
		} else {
			updated = append(updated, "hook:"+h.ID)
		}
	}
	return created, updated, nil
}

// diffExisting 列出 bundle 里「已存在」的对象（updated=true）或「新增」的对象。
func (s *Server) diffExisting(b *bundle, wantExisting bool) []string {
	out := []string{}
	note := func(kind, id string, exists bool) {
		if exists == wantExisting {
			out = append(out, kind+":"+id)
		}
	}
	for _, t := range b.Tools {
		if cur, err := s.reg.GetTool(t.ID); err == nil {
			note("tool", t.ID, cur != nil)
		}
	}
	for _, f := range b.Flows {
		if cur, err := s.reg.GetFlow(f.ID); err == nil {
			note("flow", f.ID, cur != nil)
		}
	}
	for _, h := range b.Hooks {
		if cur, err := s.reg.GetHook(h.ID); err == nil {
			note("hook", h.ID, cur != nil)
		}
	}
	sort.Strings(out)
	return out
}

// handleDelete 删对象（守卫语义与 REST/CLI 完全一致，本层只做协议翻译）。
func (s *Server) handleDelete(ctx context.Context, _ *mcp.CallToolRequest, in DeleteArgs) (*mcp.CallToolResult, any, error) {
	kind := strings.ToLower(strings.TrimSpace(in.Kind))
	if in.ID == "" {
		return toolError("BAD_ARGUMENT", "id 不能为空", "先用 gear_catalog 确认要删的对象"), nil, nil
	}
	var err error
	switch kind {
	case "tool":
		err = s.reg.DeleteTool(in.ID)
	case "flow":
		err = s.reg.DeleteFlow(in.ID)
	case "hook":
		err = s.reg.DeleteHook(in.ID)
	default:
		return toolError("BAD_ARGUMENT", "kind 必须是 tool | flow | hook（实际: "+in.Kind+")", ""), nil, nil
	}
	if err != nil {
		// 守卫拒绝（被引用 / 运行中）与「对象本来就不存在」是**两种不同的失败**：
		// 混成一个码会让智能体在「id 打错」时去解引用，白折腾一轮。用 registry 的
		// sentinel（ErrNotFound / ErrInUse）分流。
		//
		// 直接用 err.Error() 而不是 toolErrorFrom：registry 层没做结构化诊断，走
		// toolErrorFrom 会把「被 Flow f 的 step s 引用」这句关键信息换成空 hint。
		switch {
		case errors.Is(err, registry.ErrNotFound):
			return toolError("NOT_FOUND", err.Error(), "先用 gear_catalog 确认 id（可能带了应用前缀）"), nil, nil
		case errors.Is(err, registry.ErrInUse):
			return toolError("IN_USE", err.Error(),
				"错误信息里指出了被谁引用：先删/改引用它的对象，或停掉正在用它的执行"), nil, nil
		default:
			return toolErrorFrom("DELETE_FAILED", err), nil, nil
		}
	}
	return nil, map[string]any{"deleted": kind + ":" + in.ID}, nil
}

// handlePromote 晋升 Tool 版本（状态机与 Hook 门禁都在 engine 层，本层不重复实现）。
func (s *Server) handlePromote(ctx context.Context, _ *mcp.CallToolRequest, in PromoteArgs) (*mcp.CallToolResult, any, error) {
	target := model.ToolVersionState(in.Target)
	if !model.ValidStates[target] {
		return toolError("BAD_ARGUMENT",
			fmt.Sprintf("非法目标状态: %q（应为 draft/testing/stable/deprecated/rejected）", in.Target), ""), nil, nil
	}
	if err := s.engine.PromoteTool(in.ToolID, in.Version, target); err != nil {
		return toolErrorFrom("PROMOTE_FAILED", err), nil, nil
	}
	return nil, map[string]any{"tool_id": in.ToolID, "version": in.Version, "state": in.Target}, nil
}

// ---- 小工具 ----

func ids(ts []*model.Tool) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.ID)
	}
	return out
}

func flowIDs(fs []*model.Flow) []string {
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		out = append(out, f.ID)
	}
	return out
}

func hookIDs(hs []*model.Hook) []string {
	out := make([]string, 0, len(hs))
	for _, h := range hs {
		out = append(out, h.ID)
	}
	return out
}

func appID(b *bundle) string {
	if len(b.Apps) > 0 {
		return b.Apps[0].ID
	}
	return ""
}

// sameJSON 比较两个对象的内容是否等价（用 YAML/JSON 往返，忽略结构体零值与字段顺序差异）。
//
// 用「序列化后比较」而不是 reflect.DeepEqual：后者对 map 的 nil/空切片差异敏感，
// 会把「人工填的标准定义」与「智能体重发的等价定义」判为不同，从而误报冲突。
func sameJSON(a, b any) bool {
	ja, err := yaml.Marshal(a)
	if err != nil {
		return false
	}
	jb, err := yaml.Marshal(b)
	if err != nil {
		return false
	}
	return string(ja) == string(jb)
}
