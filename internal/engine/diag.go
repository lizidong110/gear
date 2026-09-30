package engine

// diag.go：静态校验的**结构化诊断**。
//
// 为什么需要它：MCP 面的 `gear_validate` 要让智能体「靠 code → 查手册 → 改 YAML」自愈，而不是
// 拿着一段中文错误去猜。但 `ValidateFlowStatic` 的签名不能改（CLI/REST/AI 闭环都在用），
// 于是把诊断**挂在 error 链上**：`errors.As` 取得到就结构化，取不到就退化为一段文本——
// 既有调用方零改动，新调用方多得一份机器可读信息。
//
// 错误码表见 MCP_AGENT_DESIGN.md 附录 D。
import (
	"errors"
	"fmt"
)

// 诊断错误码（与 MCP_AGENT_DESIGN.md 附录 D 同步）。
const (
	CodeUnknownCallTarget = "UNKNOWN_CALL_TARGET" // step.call 既非已注册 Tool 也非「能力.方法」
	CodeUnknownCapability = "UNKNOWN_CAPABILITY"  // 能力名/方法名不存在
	CodeDirectCallLimited = "DIRECT_CALL_LIMITED" // 直调原子能力不支持 use_version / mode: service
	CodeBadTemplate       = "BAD_TEMPLATE"        // ${} 前缀非法
	CodeBadWhen           = "BAD_WHEN"            // when 不是合法表达式（或误用 ${}）
	CodeBadDependency     = "BAD_DEPENDENCY"      // 依赖不存在或成环
	CodeBadFlowStructure  = "BAD_FLOW_STRUCTURE"  // Flow 结构不变量（缺 id / 缺 call / mode 非法…）
	CodeBadPrimitiveArgs  = "BAD_PRIMITIVE_ARGS"  // 直调入参：未知参数名 / 缺必填
	CodeSchemaNotObject   = "SCHEMA_NOT_OBJECT"   // schema 顶层非 object
	CodeBadToolStructure  = "BAD_TOOL_STRUCTURE"  // Tool 结构不变量
	CodeBadHookStructure  = "BAD_HOOK_STRUCTURE"  // Hook 结构不变量
	// CodeEntryExeUnresolved = process 型 Tool 的 entry 首 token 指向的可执行文件在
	// 校验时**不存在**。存在的理由（2026-09-22 实测缺陷）：entry 写错路径原先要等到
	// 运行时才炸，且报错是 `executable file not found in %PATH%`（把病因指向 PATH，
	// 与「路径写错」无关）。静态校验能提前发现，就不该拖到运行期。
	CodeEntryExeUnresolved = "ENTRY_EXE_UNRESOLVED"
	// CodePortablePathHardcoded = 应用定义里出现了机器相关绝对路径（如 C:\xxx、/Users/xxx、
	// /d/gadern/xxx，包含 Lua 脚本体内的 dsn）。这类路径在另一台机器上不存在，是 2026-09-25
	// 实测的可移植性根因。gear_validate / gear_apply 在**写作期**即拦截，要求改用内核相对锚点：
	//   - db 的 dsn 写相对库名（如 mindstorm.db）→ 锚到 GEAR_APP_DATA（默认 <Home>/data）；
	//   - process entry 写裸名（如 "gear serve ..."）→ 锚到 gear 可执行文件所在目录；
	//   - ext.serve 的 dir 写相对安装根（如 apps/<app>/www）→ 锚到 InstallRoot。
	// 与 tools/portable_audit.py（CI 闸门第⑨项）判定口径一致——CI 是兜底，MCP 是即时反馈。
	CodePortablePathHardcoded = "PORTABLE_PATH_HARDCODED"
	// CodeScopeViolation = 运行期命名空间守卫（`--scope`）拦下的一次跨应用引用：
	// 进程 scope 只允许引用本应用名下的已注册对象，引用了别人的（或没带前缀的）
	// 一律拒绝。空 scope 时不启用，本码不出现。见 IDENTITY_CONTEXT_DESIGN.md §4.3。
	CodeScopeViolation = "SCOPE_VIOLATION"
	CodeUnclassified   = "UNCLASSIFIED" // 未分类（不假装知道原因）
	// CodeMissingServeHolder = 一条要用 `ext.serve` 侧车的 Flow，却没有一条 `mode: service`
	// 的 `ext.serve` 步去**持有**它。缺了照样能写入、能 gear_apply，只在真正起服时炸
	// connection refused——2026-09-26 垂直切片与 2026-09-27 agent 范例两次实测踩到同一个坑。
	// 只针对 ext.serve：其它侧车缺了只是少一个能力，不是「这个应用不存在」。
	CodeMissingServeHolder = "MISSING_SERVE_HOLDER"
	// CodeRouteInvalid = Tool 的 `api:` 声明形状不合法（method 不在 GET/POST/PUT/PATCH/DELETE
	// 里，或 path 不以 /api/ 开头 / 空段 / {} 没名字 / 占着内核资源根）。
	//
	// 为什么必须挡在写作期：net/http 对**同名 pattern** 的两次注册是 panic（实测 go1.27），
	// 而注册发生在起服时——一个 YAML 字段就能让整个服务起不来，这种破坏力不能留到运行期。
	// 字段级诊断（tool(x).api.method / .api.path）由 artifact.CheckToolRoutes 给出。
	CodeRouteInvalid = "ROUTE_INVALID"
	// CodeRouteConflict = 两条声明路由撞在同一 method+path 上（FR-3 的路由冲突）。
	// 与 ROUTE_INVALID 的区别：这里两边**各自都合法**，只是不能同时存在——所以诊断要
	// 点名两个 Tool，且整包拒绝（原子语义不变）。
	CodeRouteConflict = "ROUTE_CONFLICT"
)

// Diagnostic 是一条机器可读的校验诊断。
type Diagnostic struct {
	// Code 是机器可读错误码（智能体据此查 gear_guide("errors") 的修法）。
	Code string `json:"code"`
	// Message 是人类可读的完整信息（与既有 error 文本一致，便于日志对照）。
	Message string `json:"message"`
	// Step 是出错的 step id（flow 级错误为空）。
	Step string `json:"step,omitempty"`
	// Field 是字段路径（如 steps[1].call / input_schema.type）。
	Field string `json:"field,omitempty"`
	// Hint 是「怎么修」的一句话。
	Hint string `json:"hint,omitempty"`
}

// DiagError 把一条 Diagnostic 挂到 error 链上。
//
// Error() 返回 Message —— 与包装前的文本一致，所以任何既有的 `err.Error()` 打印点
// （CLI / REST / 日志）看到的内容不变，不会因为引入诊断而改变对外可见信息。
type DiagError struct {
	Diag Diagnostic
	err  error
}

// Error 实现 error：返回人类可读消息（与原始文本一致）。
func (e *DiagError) Error() string { return e.Diag.Message }

// Unwrap 保留错误链（errors.Is/As 可继续向内查找）。
func (e *DiagError) Unwrap() error { return e.err }

// diagWrap 给 err 挂上诊断。
//
// 若 err 链里**已经有**诊断，则不再套一层（保留最贴切的那条，只补齐外层才能提供的
// step / field），避免出现「外层泛泛、内层精确」时智能体看到的是外层那条。
func diagWrap(code string, err error, step, field, hint string) error {
	if err == nil {
		return nil
	}
	var de *DiagError
	if errors.As(err, &de) {
		d := de.Diag
		if d.Step == "" {
			d.Step = step
		}
		if d.Field == "" {
			d.Field = field
		}
		if d.Hint == "" {
			d.Hint = hint
		}
		return &DiagError{Diag: d, err: err}
	}
	return &DiagError{
		Diag: Diagnostic{Code: code, Message: err.Error(), Step: step, Field: field, Hint: hint},
		err:  err,
	}
}

// Diagnostics 从 error 里抽取诊断。
//
// 取不到结构化诊断时**不装作知道原因**：返回一条 UNCLASSIFIED，Message 保留原文，
// 让调用方（与智能体）明白「这条只能靠读文本」。
func Diagnostics(err error) []Diagnostic {
	if err == nil {
		return nil
	}
	var de *DiagError
	if errors.As(err, &de) {
		return []Diagnostic{de.Diag}
	}
	return []Diagnostic{{Code: CodeUnclassified, Message: err.Error()}}
}

// Diag 把任意外层错误包成带指定码的诊断（供 MCP 层包装 Tool/Hook 校验错误用：
// 那两类校验在 model 层，不经过 engine 的 ValidateFlowStatic，故由调用方给码）。
func Diag(code string, err error, field, hint string) error {
	return diagWrap(code, err, "", field, hint)
}

// Diagf 构造一条无底层 error 的诊断（用于「本身就知道原因」的拒绝）。
func Diagf(code, field, hint, format string, args ...any) error {
	return &DiagError{
		Diag: Diagnostic{Code: code, Message: fmt.Sprintf(format, args...), Field: field, Hint: hint},
	}
}
