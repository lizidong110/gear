package mcp

// common.go：工具面的公共部件——结构化错误与参数类型。
//
// 错误形态（MCP_AGENT_DESIGN.md §6.2⑨ / §9.1）：
//   - **执行失败**（Flow 跑挂了、应用没起来）不是协议错误：工具**成功返回**记录（status: failed），
//     让智能体从 status/error/diagnostics 里定位问题步骤；
//   - **前置条件不满足**（对象不存在、参数非法、被守卫拒绝）才是工具错误：IsError=true + 文本带码。
//
// 两类必须泾渭分明，否则智能体的判断逻辑会分裂（一会儿看 IsError、一会儿看 status）。

import (
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"gear/internal/engine"
)

// toolError 构造携带错误码的 MCP 工具错误。
//
// 形态：IsError=true + Content 文本 `[CODE] message` / `hint: …`。
// 为什么把码放进文本而不是 structuredContent：MCP 规范对 IsError 响应的 structuredContent
// 没有约定，严格客户端（WorkBuddy 用 zod 校验）只对成功响应做对象校验；把码写进文本是
// **所有客户端都读得到**的形态，而成功路径才用 structuredContent（顶层必须是对象）。
func toolError(code, msg string, hint ...string) *mcp.CallToolResult {
	text := "[" + code + "] " + msg
	var hs []string
	for _, h := range hint {
		if strings.TrimSpace(h) != "" {
			hs = append(hs, h)
		}
	}
	if len(hs) > 0 {
		text += "\nhint: " + strings.Join(hs, "；")
	}
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: text}},
	}
}

// toolErrorFrom 把一个 error 转成工具错误：能取出结构化诊断就带上 code 与 hint。
//
// fallbackCode 只在错误链里没有诊断时使用（例如 registry 层的守卫拒绝）——
// 不让「没做结构化」的错误变成一个没有码的裸文本。
//
// ★ 关于「只报第一条」（评审 I-2）：Gear 的诊断是**挂在 error 链上**的，而 `diagWrap`
// 刻意**不叠加**（内层已有诊断就保留内层那条，只补 step/field），因此一条 error 实际只会
// 带**一条**诊断——这里的 `diags[0]` 是「唯一那条」，不是「多条里的第一条」，不存在丢信息。
// 「一次报全」的多诊断面在 `gear_validate` / `gear_apply`（走 diagnosticsJSON，非 error 通道）。
// 为防将来真有 error 带多条诊断，这里仍把其余几条并入 hint（宁可多带，不静默丢）。
func toolErrorFrom(fallbackCode string, err error) *mcp.CallToolResult {
	if err == nil {
		return nil
	}
	if diags := engine.Diagnostics(err); len(diags) > 0 {
		d := diags[0]
		code := d.Code
		if code == "" || code == engine.CodeUnclassified {
			code = fallbackCode
		}
		hint := d.Hint
		if len(diags) > 1 {
			extra := make([]string, 0, len(diags)-1)
			for _, x := range diags[1:] {
				extra = append(extra, x.Message)
			}
			if hint != "" {
				hint += "；"
			}
			hint += fmt.Sprintf("另有 %d 条诊断：%s", len(diags)-1, strings.Join(extra, "；"))
		}
		return toolError(code, d.Message, hint)
	}
	return toolError(fallbackCode, err.Error())
}

// diagnosticsJSON 把诊断渲染成 JSON 友好的切片（用于 gear_validate / gear_apply 的返回体）。
//
// 空切片转成 `[]` 而非 null：客户端拿到 null 还要自己判空，而「没有诊断」是明确的事实。
func diagnosticsJSON(diags []engine.Diagnostic) []map[string]any {
	out := make([]map[string]any, 0, len(diags))
	for _, d := range diags {
		m := map[string]any{
			"code":    d.Code,
			"message": d.Message,
		}
		if d.Step != "" {
			m["step"] = d.Step
		}
		if d.Field != "" {
			m["field"] = d.Field
		}
		if d.Hint != "" {
			m["hint"] = d.Hint
		}
		if d.Code != "" {
			m["docs"] = "gear://guide/errors#" + d.Code
		}
		out = append(out, m)
	}
	return out
}

// diagError 把「一条诊断」包成 error，便于在解析阶段（还没到校验）就产出可自愈的错误。
func diagError(code, field, hint, format string, args ...any) error {
	return engine.Diagf(code, field, hint, format, args...)
}

// ---- 工具输出 schema（评审 I-6）----
//
// 此前所有工具都走 `Out = any` ⇒ **完全没有 outputSchema**：客户端（尤其第三方/严格客户端）
// 无法静态知道返回形状，只能靠 gear_guide 或试调用——与 input 侧「jsonschema tag 自动生成 +
// 有守卫」的严谨度严重不对等（见 MCP_接口与协议评审_v1.md I-6）。
//
// ⚠ 两条硬约束（先读 SDK 源码才敢写）：
//  1. **不设 `required`、不设 `additionalProperties: false`**：`AddTool` 会对**每次**返回做
//     schema 校验（`server.go` 的 `toolForErr` → `applySchema(outJSON, outputResolved, true)`），
//     合法返回里缺失/多余的键都会把整次调用判失败。这里只声明「有这些键、类型如此」。
//  2. 类型不确定 / 可为 null 的字段一律用 `sAny()`（空 schema = 任意）：宁可少声明类型，
//     也不要因写错类型把正常返回拒掉（尤其 `escaped_job` 可为 null、`result` 形状随业务变）。
//
// 键名与类型以「该工具所有返回路径的并集」为准（一个工具可能分多个 action/kind 返回不同键）。
func sStr() map[string]any  { return map[string]any{"type": "string"} }
func sNum() map[string]any  { return map[string]any{"type": "number"} }
func sBool() map[string]any { return map[string]any{"type": "boolean"} }
func sArr() map[string]any  { return map[string]any{"type": "array"} }
func sObj() map[string]any  { return map[string]any{"type": "object"} }

// sAny 是「任意类型」片段（空对象 = 不加约束）。
func sAny() map[string]any { return map[string]any{} }

// outSchema 组装输出 schema：`{"type":"object","properties":{...}}`。
func outSchema(props map[string]any) map[string]any {
	return map[string]any{"type": "object", "properties": props}
}

// 各工具的输出 schema（单一处集中定义，便于与返回体逐键对照）。
var (
	outGuide = outSchema(map[string]any{
		"topic": sStr(), "content": sStr(), "topics": sArr(),
	})
	outCatalog = outSchema(map[string]any{
		"items": sArr(), "total": sNum(), "truncated": sBool(), "counts": sObj(),
		"offset": sNum(), "limit": sNum(), "next_offset": sNum(),
	})
	// ⚠ `object` 用 sAny 而不是 sObj：`gear_describe(kind=app)` 在「没起过、只有清单」时
	// 如实返回 `object: null`，而 SDK 会对**每次**返回做 outputSchema 校验——写死 "object"
	// 会让一个完全正常的响应被判失败（2026-09-26 实测：`has type "null", want "object"`）。
	// 红线仍是「类型不确定/可为 null 的一律 sAny」，这里正是那个场景。
	outDescribe = outSchema(map[string]any{
		"kind": sStr(), "object": sAny(), "long_running": sBool(),
		"manifest": sAny(), "composition": sAny(),
	})
	outCapabilities = outSchema(map[string]any{
		"capabilities": sArr(), "total": sNum(), "filter_name": sStr(), "filter_kind": sStr(),
		"hint": sStr(),
	})
	outDoctor = outSchema(map[string]any{
		"registry": sObj(), "extensions": sObj(), "ai": sObj(), "ports": sObj(),
		"apps": sArr(), "orphan": sArr(), "apps_dir": sStr(), "warnings": sArr(),
		// MCP 会话自身的暴露装配状态（expose / scope / curated_tools / hint）。
		// 只由 handleDoctor 附加，CLI `gear doctor` 不产出此键——见 expose_status.go。
		"mcp": sObj(),
	})
	outValidate = outSchema(map[string]any{
		"valid": sBool(), "diagnostics": sArr(), "warnings": sArr(), "summary": sObj(),
	})
	outApply = outSchema(map[string]any{
		"valid": sBool(), "dry_run": sBool(), "diagnostics": sArr(), "warnings": sArr(),
		"applied": sObj(), "would_apply": sObj(),
	})
	outDelete = outSchema(map[string]any{
		"deleted": sStr(),
	})
	outPromote = outSchema(map[string]any{
		"tool_id": sStr(), "version": sStr(), "state": sStr(),
	})
	outRun = outSchema(map[string]any{
		"kind": sStr(), "id": sStr(), "output": sObj(),
		"execution_id": sStr(), "flow_id": sStr(), "status": sStr(), "started_at": sAny(),
		"error": sStr(), "result": sAny(), "finished_at": sAny(), "hint": sStr(),
	})
	outExecution = outSchema(map[string]any{
		"id": sStr(), "flow_id": sStr(), "status": sStr(), "started_at": sAny(),
		"error": sStr(), "result": sAny(), "tool_ids": sArr(), "finished_at": sAny(),
		"logs": sArr(), "executions": sArr(), "total": sNum(), "limit": sNum(),
		"orphan": sBool(), "orphan_reason": sStr(), "hint": sStr(),
	})
	outApp = outSchema(map[string]any{
		"app_id": sStr(), "flow_id": sStr(), "pid": sNum(), "state": sStr(), "log_path": sStr(),
		"ports": sArr(), "escaped_job": sAny(), "warning": sStr(), "last_logs": sArr(),
		"error": sStr(), "hint": sStr(), "ports_released": sBool(), "started_at": sAny(),
		"uptime_s": sNum(), "items": sArr(), "orphan": sArr(), "total": sNum(),
		"dry_run": sBool(), "pruned": sArr(), "skipped": sArr(), "destructive": sBool(),
		"pruned_count": sNum(), "skipped_count": sNum(), "apps_dir": sStr(),
	})
	outInvoke = outSchema(map[string]any{
		"name": sStr(), "method": sStr(), "result": sAny(),
	})
	outExport = outSchema(map[string]any{
		"app_id": sStr(), "path": sStr(), "format": sStr(), "components": sObj(),
		"files": sArr(), "warnings": sArr(), "next": sStr(), "static_root": sStr(),
	})
	outImport = outSchema(map[string]any{
		"app_id": sStr(), "valid": sBool(), "dry_run": sBool(), "created": sArr(),
		"updated": sArr(), "static_landed": sArr(), "static_missing": sArr(),
		"warnings": sArr(), "next": sStr(),
	})
)

// ---- 工具入参类型（SDK 据此自动生成 input schema）----

// IDArgs 是「按 id 操作」类工具的公共入参。
type IDArgs struct {
	ID string `json:"id" jsonschema:"对象 id（Tool id / Flow id / Hook id / 执行 id）"`
}

// DescribeArgs 是 gear_describe 的入参。
type DescribeArgs struct {
	Kind string `json:"kind" jsonschema:"对象类型：tool | flow | hook | app（app = 含 mode:service step 的 Flow，见 gear_app(action=list)）"`
	ID   string `json:"id" jsonschema:"对象 id"`
}

// CatalogArgs 是 gear_catalog 的入参。
type CatalogArgs struct {
	Kind   string `json:"kind,omitempty" jsonschema:"要看哪类：tool | flow | hook | execution | app | all（默认 all；传其它值会报 BAD_ARGUMENT 并列出合法值，不静默返回空列表）"`
	Limit  int    `json:"limit,omitempty" jsonschema:"最多返回条数（默认 100）"`
	Offset int    `json:"offset,omitempty" jsonschema:"跳过前 N 条再返回（默认 0，用于翻页：结果里 truncated 为 true 时，用返回的 next_offset 续取）"`
}

// GuideArgs 是 gear_guide 的入参。
//
// ⚠ 描述**不枚举 topic**（评审 I-5）：此前把 8 个 topic 硬编码进文案，而磁盘上实际有 9 个
// （漏了 rest-api），且任何一次往 guide/ 放 md 都会让文案静默过期。单一真相源是磁盘，
// 故改为「见返回的 topics 字段」——客户端调一次就能拿到权威清单。
type GuideArgs struct {
	Topic string `json:"topic,omitempty" jsonschema:"手册主题；留空取默认主题。可用主题**见返回值里的 topics 字段**，或 resources/list 的 gear://guide/{topic}——此处刻意不枚举，避免与磁盘失同步"`
}

// CapabilitiesArgs 是 gear_capabilities 的入参（评审 I-7：大 payload 需可裁剪）。
type CapabilitiesArgs struct {
	Name string `json:"name,omitempty" jsonschema:"只看名字等于该值的能力（如 db / ext.ai；留空=全部）"`
	// ⚠ `kind` 必须列出代码真正支持的**全部三种**类别。此前只写 builtin | ext，漏了 pure，
	// 而照 schema 描述办事的调用方会因此整层漏掉纯函数原子（time/template/crypto/id/validate，
	// 5 能力 22 方法）——它们**不用声明 capabilities 就能调**，看不见就等于不存在。
	// 此处**刻意不枚举能力名**（名字会随 purefunc.go 新增而漂移），只给类别名与一句定性：
	// 具体有哪些能力由返回值说话，与 GuideArgs 不枚举 topic 同一条纪律。
	// 防再次漏写：`TestCapabilitiesKindDescCoversEveryKind` 从代码反查描述必须覆盖全部类别。
	Kind string `json:"kind,omitempty" jsonschema:"按类别过滤：builtin（受门禁内核原语）| pure（纯函数原子，免 capabilities 声明）| ext（扩展原子）；留空=全部"`
}

// InvokeArgs 是 gear_invoke 的入参。
//
// ⚠ 标签文案不得以 `WORD=` 开头（见 RunArgs 的说明），故写成「name 为…」而不是「name=…」。
type InvokeArgs struct {
	Name   string         `json:"name" jsonschema:"能力名：http/call/db/event/subscribe 或已注册的 ext.*（先 gear_capabilities 查）"`
	Method string         `json:"method" jsonschema:"方法名（如 query / exec / Request / complete；参数 schema 见 gear_capabilities）"`
	Args   map[string]any `json:"args,omitempty" jsonschema:"方法入参（以参数名为键的对象，与 gear_capabilities 的 schema 对齐）"`
}

// ValidateArgs 是 gear_validate 的入参。
type ValidateArgs struct {
	YAML string `json:"yaml" jsonschema:"Gear bundle YAML 文本（多文档：kind: App 清单 + Tool/Flow/Hook）"`
}

// ApplyArgs 是 gear_apply 的入参。
type ApplyArgs struct {
	// omitempty 是必需的：`restore_from` 单独使用时 bundle 为空，
	// 而缺 omitempty 会让 generated schema 把 bundle 标成 required，纯回滚直接被验参挡掉。
	Bundle    string `json:"bundle,omitempty" jsonschema:"Gear bundle YAML 文本（多文档）"`
	DryRun    bool   `json:"dry_run,omitempty" jsonschema:"只校验与检查冲突，不落库（等价于 gear_validate + 覆盖检测）"`
	Overwrite bool   `json:"overwrite,omitempty" jsonschema:"允许覆盖内容不同但 id 已存在的对象（默认 false，仅更新内容相同的定义）"`

	// RestoreFrom 是「回滚到某次变更之前」的入口：值为 definition_history 的 seq（用
	// `gear history list` 查）。> 0 时**忽略 bundle**，仅把该历史点的定义写回；
	// 该点对应「对象当时还不存在」时执行删除（撤回一次新建）。
	RestoreFrom int `json:"restore_from,omitempty" jsonschema:"回滚到某个历史点（definition_history.seq，见 gear history list）；大于 0 时忽略 bundle"`
}

// ExportArgs 是 gear_export 的入参。
//
// ⚠ 标签文案不得以 `WORD=` 开头（见 RunArgs 的说明）。
type ExportArgs struct {
	AppID         string `json:"app_id" jsonschema:"要导出的应用 id：优先取 registry 里的 App 清单；没有清单时按入口 Flow 解析（id / <id>.up / 唯一 <id>. 前缀）并临时合成一份清单，所以只有 Flow 的应用也能导"`
	OutDir        string `json:"out_dir" jsonschema:"产物输出路径：dir 形态为要创建的目录（须不存在或为空，gear 不会覆盖非空目录）；zip 形态为包文件路径（扩展名留空会自动补 .gearapp）"`
	Format        string `json:"format,omitempty" jsonschema:"产物形态：dir（默认，可 diff 可 git 版本化）| zip（打成单个包，便于发送）"`
	IncludeStatic *bool  `json:"include_static,omitempty" jsonschema:"是否把 layout.files 声明的静态资源拷进产物（默认 true；关掉后产物不自包含，不能当交付物）"`
	StaticRoot    string `json:"static_root,omitempty" jsonschema:"解析 layout.files 相对路径的根目录；留空表示**从当前目录逐级向上找**能解析声明路径的目录（绝对路径的声明不受它影响）。gear 不持有静态资源，它在哪只能由调用方告诉"`
}

// ImportArgs 是 gear_import 的入参。
type ImportArgs struct {
	Source    string `json:"source" jsonschema:"产物路径：gear_export 产出的目录，或 .gearapp / zip 文件"`
	Overwrite bool   `json:"overwrite,omitempty" jsonschema:"允许覆盖已存在且内容不同的对象（默认 false：整批拒绝，保持原子）"`
	DryRun    bool   `json:"dry_run,omitempty" jsonschema:"只汇报将创建/更新什么，一个字节不写"`
	AppsDir   string `json:"apps_dir,omitempty" jsonschema:"静态资源落地根目录；留空表示不落地（只登记清单，符合 gear 不持有静态资源的边界）。落地时会重写 layout 指向实际位置"`
}

// DeleteArgs 是 gear_delete 的入参。
type DeleteArgs struct {
	Kind string `json:"kind" jsonschema:"对象类型：tool | flow | hook"`
	ID   string `json:"id" jsonschema:"对象 id"`
}

// PromoteArgs 是 gear_promote 的入参。
type PromoteArgs struct {
	ToolID  string `json:"tool_id" jsonschema:"Tool id"`
	Version string `json:"version" jsonschema:"目标版本号"`
	Target  string `json:"target" jsonschema:"目标状态：draft/testing/stable/deprecated/rejected"`
}

// RunArgs 是 gear_run 的入参。
//
// ⚠ 标签文案的硬约束：jsonschema 库用 `^[^ \t\n]*=` 判定「保留写法」（不许以 `WORD=` 开头，
// 那是给未来的关键字留的位），所以描述里**不能以 `xxx=` 开头**——写成「kind 为 tool 时…」
// 而不是「kind=tool 时…」，否则 AddTool 在注册期直接 panic（踩过一次）。
type RunArgs struct {
	Kind        string         `json:"kind" jsonschema:"跑什么：tool（同步返回结果）| flow（异步返回 execution_id）"`
	ID          string         `json:"id" jsonschema:"Tool id 或 Flow id"`
	Input       map[string]any `json:"input,omitempty" jsonschema:"kind 为 tool 时的入参"`
	Vars        map[string]any `json:"vars,omitempty" jsonschema:"kind 为 flow 时覆盖 Flow vars"`
	WaitSeconds int            `json:"wait_seconds,omitempty" jsonschema:"kind 为 flow 时最多等这么多秒再返回结果快照；0 表示立即返回，建议 15-30"`
}

// ExecutionArgs 是 gear_execution 的入参。
type ExecutionArgs struct {
	ID       string `json:"id,omitempty" jsonschema:"执行 id；留空 = 列出最近执行"`
	Limit    int    `json:"limit,omitempty" jsonschema:"列出时最多返回条数（默认 20，取最近 N 条；0 表示用默认）"`
	WithLogs bool   `json:"with_logs,omitempty" jsonschema:"附带各 step 的 stdout/stderr 日志（仅给 id 时有效）"`
	Tail     int    `json:"tail,omitempty" jsonschema:"with_logs 时只取末 N 行（0=全部）"`
}

// AppArgs 是 gear_app 的入参。
type AppArgs struct {
	Action      string `json:"action" jsonschema:"start | stop | status | list | prune"`
	FlowID      string `json:"flow_id,omitempty" jsonschema:"start：要监管的入口 Flow（须含 mode: service 的长驻 step）；prune：只清该 Flow 的台账（留空=全部）"`
	AppID       string `json:"app_id,omitempty" jsonschema:"stop/status：应用 id（来自 start 或 list）"`
	Restart     string `json:"restart,omitempty" jsonschema:"start：restart 策略 always | never（默认 always）"`
	Replace     bool   `json:"replace,omitempty" jsonschema:"start：同 flow 已有活跃实例时先停再启"`
	WaitSeconds int    `json:"wait_seconds,omitempty" jsonschema:"start：短等秒数确认启动结果（默认 0）"`
	Ports       []int  `json:"ports,omitempty" jsonschema:"start：声明的服务端口（用于台账探活与 stop 后复检）"`
	DryRun      bool   `json:"dry_run,omitempty" jsonschema:"prune：只汇报将清理哪些台账项，不实际删除"`
}

// FlowExposeArgs 是 flow__{id} 反向暴露工具的入参（只收可选 vars，id 固定为 Flow id）。
type FlowExposeArgs struct {
	Vars        map[string]any `json:"vars,omitempty" jsonschema:"覆盖 Flow 的 vars（模板 ${vars.x} 引用）"`
	WaitSeconds int            `json:"wait_seconds,omitempty" jsonschema:"最多等这么多秒再返回结果快照；0 表示立即返回"`
}
