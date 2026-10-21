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
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"gear/internal/artifact"
	"gear/internal/cmdline"
	"gear/internal/engine"
	"gear/internal/gearhome"
	"gear/internal/luacap"
	"gear/internal/model"
	"gear/internal/registry"
)

// appManifest 是 bundle 的 `kind: App` 清单，等价于 model.App。
//
// ## 2026-09-26 变更：App 清单**入 registry**（此前刻意不入）
//
// 旧注释是「保持 PRD 的非目标（只有 Flow + app run，没有 App 实体），避免为了创建应用
// 而在内核里长出一个新模型」。这个取舍在 gear 自己用时成立；但当目标是**发布后让用户
// 在 gear 上自助构建**时，它的代价是：gear 里没有「用户构建的产品」这个对象——定义散在
// registry.db、静态资源游离在文件系统、清单只在客户端 YAML 里，于是没有导出、没有组成
// 清单、没有可交付物。
//
// 因此把清单提升为 `model.App`（与 Tool/Flow/Hook 同列），落 apps 表。
// 这里保留**类型别名**而不是改所有引用：解析、校验、warnings 逻辑一行不动，
// 改动面收敛到「多写一张表」这一件事上。
//
// 动机与后果的完整论证见 `internal/model/app.go` 的包注释。
type appManifest = model.App

// bundle 是解析后的交付物。
//
// 2026-09-26：定义**上提**到 internal/artifact（这里改为类型别名）。
// 原因是加了 gear_import 之后有两个「写入口」（本文件的 gear_apply 与 artifact.Import），
// 若各持有一份结构，「导入进来的东西与 apply 进去的东西形态不一致」只是时间问题。
// 别名让所有既有代码（b.Tools / b.Apps …）一行不改。
type bundle = artifact.Bundle

// parseBundle 把多文档 YAML 解析成 bundle。
//
// 分类规则（**优先看 kind，无 kind 时退化为字段启发式**）：
//
//	kind: App  → 应用清单（apply/import 落 registry apps 表，命名空间户主）
//	kind: Tool/Flow/Hook → 对应类型
//	无 kind    → 有 steps 视为 Flow，有 stage 视为 Hook，否则视为 Tool
//	              （后两条兼容仓库里 examples/*.yaml 的历史写法）
//	未知 kind  → 明确报错（**不猜成 Tool**：猜错会让一份合法文档报出一堆莫名其妙的
//	              「runtime 非法」错，把真正的问题埋掉）
//
// 解析失败不中断：把问题作为诊断收集起来继续解析其余文档——一次报多条才是「编译器」该有的样子。
//
// 2026-09-26：实现委托给 internal/artifact.ParseBundle（产物导入复用同一份解析）。
// 规则与文案的**完整说明在 artifact 包**，此处只保留调用点：改解析规则必须改那里，
// 否则「gear_apply 认的 YAML」与「gear_import 认的产物」会分叉。
func parseBundle(text string) (*bundle, []engine.Diagnostic) {
	return artifact.ParseBundle(text)
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

	// 可移植性：应用定义不得硬编码机器相关绝对路径（与 CI 闸门第⑨项同口径）。
	// 应用主要由智能体经 MCP 编写，这里即时拦截比发布后移植失败更省事——
	// 一旦命中即 fatal 诊断，gear_apply 会据此拒绝落库。
	res.Diags = append(res.Diags, engine.CheckPortablePaths(text)...)

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

	// 命名规范引导（2026-09-25）：无 App 清单的裸注册没有引擎级命名空间约束，
	// registry 曾被测试残留（m_t1 / ext_probe 一类）塞满过，事后只能人工考古归属。
	// 这里**不拦**（定义本身合法、不影响 valid），只在 warnings 里提醒补前缀——
	// 定位与「环境未就绪」警告同级，不该升级成 fatal 诊断。
	if len(b.Apps) == 0 {
		var offenders []string
		for _, t := range b.Tools {
			if !hasAppNamespace(t.ID) {
				offenders = append(offenders, "tool:"+t.ID)
			}
		}
		for _, f := range b.Flows {
			if !hasAppNamespace(f.ID) {
				offenders = append(offenders, "flow:"+f.ID)
			}
		}
		for _, h := range b.Hooks {
			if !hasAppNamespace(h.ID) {
				offenders = append(offenders, "hook:"+h.ID)
			}
		}
		if len(offenders) > 0 {
			res.Warnings = append(res.Warnings,
				fmt.Sprintf("以下 id 无应用命名空间前缀（<app-id>__ / <app-id>.），registry 会越来越难辨认归属：%s。"+
					"建议改为应用前缀命名；测试/临时对象用 fixture_ 前缀并隔离 GEAR_HOME。规范见 gear_guide(\"authoring\") 的「命名规范」一节",
					strings.Join(offenders, ", ")))
		}
	}

	// Tool / Hook 用 model 层校验（与注册期同一份规则）。
	for _, t := range b.Tools {
		if err := t.Validate(); err != nil {
			// 「Lua 用了却没声明 capability」单列一个码（2026-10-03）：它的修法是加一行
			// capabilities，与「字段表写错」毫无关系——混在 BAD_TOOL_STRUCTURE 里，
			// 智能体会照着「对照字段表 / runtime 取值」的 hint 反复改错地方。
			if luacap.Missing(t.Entry, t.Capabilities) != nil && t.Runtime == model.RuntimeLua {
				res.Diags = append(res.Diags, engine.Diagnostic{
					Code:    engine.CodeLuaCapUndeclared,
					Field:   fmt.Sprintf("tool(%s).capabilities", t.ID),
					Message: err.Error(),
					Hint: "给这个 Tool 加 capabilities（列出 entry 里用到的受门禁原语），" +
						"或者删掉 entry 里那次调用。纯函数原子（time/template/crypto/id/validate）无需声明；" +
						"ctx.* 与 ctx.caller / ctx.json 也不是能力，不在声明范围内",
				})
			} else {
				res.Diags = append(res.Diags, wrapModelDiag(engine.CodeBadToolStructure, t.ID, "tool 定义非法", err, "tool",
					"对照 gear_guide(\"authoring\") 的 Tool 字段表；runtime 只支持 process / lua / go-host")...)
			}
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
	// 迁移声明（FR-2b）：version 断号 / 重复 / 空 sql / 缺 dsn 都在**这一层**挡掉——
	// 它们是写作期的错误，等到 apply 才报，等于把「校验」的功劳让给「写入」。
	res.Diags = append(res.Diags, checkMigrations(b)...)
	if len(b.Migrations) > 0 && s.dbm == nil {
		res.Warnings = append(res.Warnings,
			"本 bundle 声明了 migrations，但当前 MCP 会话没有 db 实现：apply 会按 fail-closed 拒绝"+
				"（不会静默跳过）。请用 gear serve 起的同一个内核，或确认引擎宿主已装配 db 原语")
	}
	// 声明式 REST 路由（FR-3）：形状非法与「两个 Tool 撞同一 method+path」都在这一层挡掉——
	// 前者写错一个字段就能让服务起不来（net/http 同名 pattern 会 panic），必须给字段级诊断。
	res.Diags = append(res.Diags, artifact.CheckToolRoutes(b.Tools)...)
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

// hasAppNamespace 判断 id 是否带应用命名空间前缀：<app-id>__ 或至少含一个「.」。
// fixture_ 前缀是测试/临时对象的合法豁免（它们本就不该进真实 registry，见 authoring §5 命名规范）。
func hasAppNamespace(id string) bool {
	if strings.HasPrefix(id, "fixture_") {
		return true
	}
	return strings.Contains(id, "__") || strings.Contains(id, ".")
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
//
// 前缀规则只有 `model.App.Owns` 一份（写入期与运行期同调，见 IDENTITY_CONTEXT_DESIGN.md §4.2）。
func checkNamespace(b *bundle, appID string) []engine.Diagnostic {
	var out []engine.Diagnostic
	app := &model.App{ID: appID}
	check := func(kind, id string) {
		if id == "" || app.Owns(id) {
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

// checkMigrations 校验 bundle 里的迁移声明（静态部分）。
//
// 与 artifact.ApplyMigrations 共用 model.MigrationSet.Validate——校验规则只能有一份，
// 否则「gear_validate 说没问题、gear_apply 却拒了」这种最劝退的组合迟早出现。
func checkMigrations(b *bundle) []engine.Diagnostic {
	var out []engine.Diagnostic
	for i, set := range b.Migrations {
		if err := set.Validate(); err != nil {
			out = append(out, engine.Diagnostic{
				Code:    artifact.CodeMigrateInvalid,
				Field:   fmt.Sprintf("migrations[%d].version", i),
				Message: fmt.Sprintf("migrations 声明非法: %v", err),
				Hint: "version 必须严格递增且从 1 起；每条要有非空 sql 与 dsn" +
					"（dsn 不能省：迁移的目标库不能是内存 scratch 库）",
			})
		}
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
		// 报错误导（2026-10-03 外部实测）：原文只说「请人在 CLI 播种」，
		// 而播种点有两条、落点路径也没给——使用者会以为无路可走，转而去 rebuild 二进制碰运气。
		// 这里给**可复制的命令**（CLI 与 REST 各一条，因为两者都真实可用）与密钥库落点。
		out = append(out, fmt.Sprintf("[MISSING_SECRET] %s 需要密钥 %s，但密钥库里没有它。"+
			"播种命令（任选一条，都写入同一个库 %s）：`gear secret set %s <值>`（CLI）；"+
			"或向 gear serve 的执行面发 `PUT /api/secrets/%s`（默认 127.0.0.1:8787）。"+
			"MCP 面刻意不提供播种入口（明文会进对话记录与执行记录库，见 gear_guide(\"security\")）；"+
			"Flow 里用 ${secrets.%s} 或 ctx.secret.get(\"%s\") 引用",
			a.ID, name, gearhome.SecretsDBPath(), name, name, name, name))
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
		Description: "把 bundle 落库（**唯一写入口**）：原子、幂等。多文档按 Tool→Flow→Hook 顺序写入，" +
			"任一非法则整体不写。dry_run=true 等价于先 validate 一遍（含冲突检测）。" +
			"改崩了可用 restore_from=<seq> 回滚（seq 见 gear history）。" +
			"**不支持密钥写入**：bundle 只声明 requires.secrets，密钥由人在 CLI 播种。",
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

	mcp.AddTool(ms, &mcp.Tool{
		Name: "gear_export",
		Description: "把应用导出成可移植产物（目录/.gearapp）：清单 + 名下 Tool/Flow/Hook + 静态资源，" +
			"用于备份、换机器、交付。目录可 diff 可版本化；只有 Flow 应用也能导。",
		Annotations:  &mcp.ToolAnnotations{Title: "导出应用产物"},
		OutputSchema: outExport,
	}, s.handleExport)

	mcp.AddTool(ms, &mcp.Tool{
		Name: "gear_import",
		Description: "把产物导进 registry。契约同 gear_apply：dry_run 先看、冲突则整批拒绝。" +
			"默认不落地静态资源，传 apps_dir 才落地。",
		Annotations:  idempotent("导入应用产物"),
		OutputSchema: outImport,
	}, s.handleImport)
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
	// 回滚先于 bundle：它压根不看 bundle（seq 已经说明要回到哪）。放在最前面是为了让
	// 「bundle 为空 + restore_from」这种最常见的救火姿势直接能用，不必先补一份 YAML。
	if in.RestoreFrom > 0 {
		row, rerr := s.reg.RestoreSnapshot(in.RestoreFrom)
		if rerr != nil {
			return toolErrorFrom("NOT_FOUND", rerr), nil, nil
		}
		return nil, map[string]any{
			"valid":       true,
			"dry_run":     false,
			"diagnostics": diagnosticsJSON(nil),
			"warnings":    []string{fmt.Sprintf("已回滚到 seq=%d：%s %s", in.RestoreFrom, row.Kind, row.ID)},
			// 刻意复用 applied 而不是加一个顶层键：outApply 的输出 schema 已定死，
			// 多一个键会让 SDK 的 outputSchema 校验在每次返回时炸（类型不确定一律 sAny 的红线）。
			"applied": map[string]any{"restored": row.Kind + ":" + row.ID},
			"next":    "回滚本身也会留痕（note=`restore from N`），可继续往前滚；跑起来前先 gear_app(action=start)",
		}, nil
	}
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
		// 迁移在 dry run 里是**真跑 + 全回滚**（artifact.ApplyMigrations 的 dryRun 语义）：
		// 值不值这个代价？值——它让 SQL 语法错在写作期被抓出来，而不是等真导入时才发现
		// schema 已经改了一半。MySQL 的 DDL 会隐式提交，那条路上 dry run 兜不住，文档里写明。
		var wouldMigrate []int
		if m, err := artifact.ApplyMigrations(s.dbm, res.Bundle, true); err != nil {
			// dry run 的迁移失败**不改 valid**：这一趟本来就要回滚，把它当「定义非法」
			// 会让智能体去改一份正常的 YAML。它是一条 warning，把原因如实带上。
			res.Warnings = append(res.Warnings, "dry-run 迁移校验失败（未写入）："+err.Error())
		} else {
			wouldMigrate = m
		}
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
				// 空数组而不是省略键：智能体按「有这个键」分支时，键不存在与「没有迁移」是两回事。
				"migrations": wouldMigrate,
			},
		}, nil
	}

	created, updated, migrations, err := s.persistBundle(res.Bundle, in.Overwrite)
	if err != nil {
		return toolErrorFrom("APPLY_FAILED", err), nil, nil
	}
	applied := map[string]any{
		"tools": ids(res.Bundle.Tools),
		"flows": flowIDs(res.Bundle.Flows),
		"hooks": hookIDs(res.Bundle.Hooks),
		"app":   appID(res.Bundle),
	}
	// 迁移是「副作用」而不是「定义」：单列一处，别塞进 created/updated 里装成定义。
	if len(migrations) > 0 {
		applied["migrations"] = migrations
	}
	return nil, map[string]any{
		"valid":       true,
		"diagnostics": diagnosticsJSON(nil),
		"warnings":    res.Warnings,
		"applied":     applied,
		"created":     created,
		"updated":     updated,
		"next": "接着用 gear_app(action=start, flow_id=…, ports=[…]) 起应用（写入 ≠ 运行）。" +
			"⚠ 静态资源（应用清单 layout.files 里的 www/ 等）**必须由你自己的文件工具落盘**——" +
			"MCP 面不提供文件读写，纯 MCP 建应用时这一环要你自己接上（apply 只登记、不落地）",
	}, nil
}

// persistBundle 按依赖顺序落库：App → Tool → Flow → Hook。
//
// 2026-09-26：实现委托给 artifact.ApplyBundle（与 gear_import 共用唯一一份）。
// 顺序语义、冲突语义、错误文案的**完整说明在 artifact/apply.go**——改那边，
// 不要让两个写入口各写一套。
func (s *Server) persistBundle(b *bundle, overwrite bool) (created, updated []string, migrations []int, err error) {
	return artifact.ApplyBundle(s.reg, b, s.dbm, overwrite)
}

// diffExisting 列出 bundle 里「已存在」的对象（updated=true）或「新增」的对象。
//
// 同样委托给 artifact（与 import 的 dry_run 同一份判定）。
func (s *Server) diffExisting(b *bundle, wantExisting bool) []string {
	return artifact.DiffExisting(s.reg, b, wantExisting)
}

// handleDelete 删对象（守卫语义与 REST/CLI 完全一致，本层只做协议翻译）。
//
// **不**传 actor：MCP 面拿不到传输层身份（§3.5），所以这次删除在 definition_history 里
// 的 actor 就是空串 = 无身份。**不要**在这里填一个推断出来的主体来「让它好看点」——
// 审计要的是真实，编出来的主体比空白更危险。
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
	case "app":
		// 2026-09-26：App 清单已入 registry，给个删除口子，否则改了 id 会留下孤儿清单。
		err = s.reg.DeleteApp(in.ID)
	default:
		return toolError("BAD_ARGUMENT", "kind 必须是 tool | flow | hook | app（实际: "+in.Kind+")", ""), nil, nil
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
	if err := s.engine.PromoteTool(in.ToolID, in.Version, target, registry.WithActor("")); err != nil {
		return toolErrorFrom("PROMOTE_FAILED", err), nil, nil
	}
	return nil, map[string]any{"tool_id": in.ToolID, "version": in.Version, "state": in.Target}, nil
}

// handleExport 导出一个应用为可移植产物。
//
// 这是「用户构建的东西能带走」的那一半（另一半是 gear_import）。它只读 registry 与文件系统，
// 不改 registry——导出失败不会留下任何半成品（产物先写 staging 再整体落地）。
func (s *Server) handleExport(ctx context.Context, _ *mcp.CallToolRequest, in ExportArgs) (*mcp.CallToolResult, any, error) {
	if strings.TrimSpace(in.AppID) == "" {
		return toolError("BAD_ARGUMENT", "app_id 不能为空", "先用 gear_catalog(kind=app) 看有哪些应用"), nil, nil
	}
	if strings.TrimSpace(in.OutDir) == "" {
		return toolError("BAD_ARGUMENT", "out_dir 不能为空",
			"dir 形态给一个**不存在的目录**（gear 不覆盖非空目录）；zip 形态给 .gearapp 文件路径"), nil, nil
	}
	include := true // 缺省自包含：产物不能交付的导出没有意义
	if in.IncludeStatic != nil {
		include = *in.IncludeStatic
	}
	res, err := artifact.Export(s.reg, artifact.ExportOptions{
		AppID:       in.AppID,
		Out:         in.OutDir,
		Format:      in.Format,
		NoStatic:    !include,
		StaticRoot:  in.StaticRoot,
		GearVersion: Version,
	})
	if err != nil {
		return toolErrorFrom("EXPORT_FAILED", err), nil, nil
	}
	return nil, map[string]any{
		"app_id":      res.AppID,
		"path":        res.Path,
		"format":      res.Format,
		"components":  res.Components,
		"files":       res.Files,
		"warnings":    res.Warnings,
		"static_root": res.StaticRoot,
		"next": "产物已落地。zip 形态可直接发给别人；目录形态可 git 版本化。" +
			"到另一台机器用 gear_import(source=<path>) 导回（静态资源默认不落地，传 apps_dir 才落地）",
	}, nil
}

// handleImport 把一个产物导进 registry。
//
// 语义与 gear_apply **逐条对齐**（都走 artifact.ApplyBundle）：dry_run 一个字节不写、
// 未传 overwrite 且内容冲突则整批拒绝。智能体不需要为它学第二套规则。
func (s *Server) handleImport(ctx context.Context, _ *mcp.CallToolRequest, in ImportArgs) (*mcp.CallToolResult, any, error) {
	if strings.TrimSpace(in.Source) == "" {
		return toolError("BAD_ARGUMENT", "source 不能为空", "给 gear_export 产出的目录或 .gearapp 文件路径"), nil, nil
	}
	res, err := artifact.Import(s.reg, artifact.ImportOptions{
		Source:    in.Source,
		Overwrite: in.Overwrite,
		DryRun:    in.DryRun,
		AppsDir:   in.AppsDir,
	})
	if err != nil {
		return toolErrorFrom("IMPORT_FAILED", err), nil, nil
	}
	next := "导入完成。接着 gear_describe(kind=app, id=…) 确认组成，再用 gear_app(action=start) 起应用"
	if res.DryRun {
		next = "dry_run：一个字节未写。确认 created/updated 后再去掉 dry_run 真导入"
	}
	return nil, map[string]any{
		"app_id":         res.AppID,
		"valid":          true,
		"dry_run":        res.DryRun,
		"created":        res.Created,
		"updated":        res.Updated,
		"static_landed":  res.StaticLanded,
		"static_missing": res.StaticMissing,
		"warnings":       res.Warnings,
		"next":           next,
	}, nil
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

// 2026-09-26：sameJSON 已随 persistBundle 一起上提到 internal/artifact（本包不再需要）。
// 「已存在且内容是否相同」的判定只允许有一处——两个写入口各判一次迟早分叉。
