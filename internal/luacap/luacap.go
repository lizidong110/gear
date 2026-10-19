// Package luacap 扫Lua entry 脚本里「用了却没声明」的受门禁 capability。
package luacap

// luacap.go —— Lua entry 脚本的**静态** capability 使用扫描（注册期拦住「用了却没声明」）。
//
// 存在理由（2026-10-03 外部实测）：未声明的 capability 在 ctx 上**压根不注入**（值为 nil，
// 见 lua.go 的 grants 判定，这是「宿主能力边界不可探测」的刻意设计）。于是脚本里写了
// `ctx.db.query(...)` 而 Tool 的 `capabilities` 没写 `db` 时，报错是
// `attempt to call a non-function object`——**运行期**、且完全看不出病因是「少声明了一项」。
// 而这恰恰是静态可判的（红线：能静态校验的不留到运行期）。
//
// 判据：**扫 AST，不扫文本**。gopher-lua 的 parse.Parse 给出完整语法树，于是注释里的
// `ctx.db`、字符串里的 "ctx.db.query" 都不会误报——用正则做这件事必然要在「宁可漏报」和
// 「误报阻塞合法应用」之间二选一，两头都坏。
//
// 明确的边界（宁可漏报，不误报）：
//   - 只认**标识符**形态 `ctx.<name>`，不认 `ctx["db"]`（Lua 里这两种等价，但后者少见）；
//   - 只在 Tool 的 Lua runtime 上扫（process runtime 的 entry 是命令行，与 ctx 无关）；
//   - 扫不出来的（解析失败）**不报错**——语法错误有 Lua VM 在运行期报一条更准的，
//     这里越权报错只会给出第二套更难懂的说法。

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/yuin/gopher-lua/ast"
	"github.com/yuin/gopher-lua/parse"

	"gear/internal/cap"
)

// Use 是一条「脚本用了某个受门禁能力」的记录。
type Use struct {
	Capability string
	Line       int
	// Expr 是脚本里的原文片段（如 `ctx.db.query`），报错时原样给用户看——
	// 只说「db 未声明」会让人回头满脚本找，而那行可能压根不长这样（局部变量、别名）。
	Expr string
}

// Scan 扫 Lua 脚本，返回它用到的**受门禁** capability（按出现位置排序）。
//
// 只收受门禁原语（http/call/db/secret/event/subscribe/config）：
//   - 纯函数原子（time/template/crypto/id/validate）始终可用，**声明了反而被拒**，
//     写进 capabilities: [] 是另一条守卫的事，不该由这里报「用了没声明」；
//   - `ctx.ext.*` 的门禁是**另一条判据**（ext 名要对得上 extensions.yaml 的登记），
//     由 GrantsFromStrings 在注册期拒；这里不重复判，免得两处口径不一致。
//
// 解析失败返回 nil（不报错）：语法错误交给 Lua VM 报，它的位置信息更准。
func Scan(script string) []Use {
	stmts, err := parse.Parse(strings.NewReader(script), "entry.lua")
	if err != nil {
		return nil
	}
	w := &capWalker{seen: map[string]bool{}}
	w.stmts(stmts)
	sort.SliceStable(w.uses, func(i, j int) bool { return w.uses[i].Line < w.uses[j].Line })
	return w.uses
}

// Missing 返回「脚本用到但未声明」的受门禁能力（按能力名排序）。
//
// 判据只看**用没用**，不看声明够不够——多声明是另一回事（最小权限是作者的取舍，
// 内核不该替他收紧；而少声明一定会运行期炸，故必须报）。
func Missing(script string, declared []string) []Use {
	have := map[string]bool{}
	for _, d := range declared {
		have[d] = true
	}
	var out []Use
	for _, u := range Scan(script) {
		if !have[u.Capability] {
			out = append(out, u)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Capability != out[j].Capability {
			return out[i].Capability < out[j].Capability
		}
		return out[i].Line < out[j].Line
	})
	return out
}

// capWalker 遍历 AST，认出「以 ctx 为根的字段访问链」。
type capWalker struct {
	uses []Use
	seen map[string]bool // "cap@line" 去重：同一行用两次只报一次
}

func (w *capWalker) add(capName string, line int, expr string) {
	key := capName + "@" + strconv.Itoa(line)
	if w.seen[key] {
		return
	}
	w.seen[key] = true
	w.uses = append(w.uses, Use{Capability: capName, Line: line, Expr: expr})
}

// stmts 遍历语句序列。
func (w *capWalker) stmts(list []ast.Stmt) {
	for _, s := range list {
		w.stmt(s)
	}
}

func (w *capWalker) stmt(s ast.Stmt) {
	switch v := s.(type) {
	case *ast.AssignStmt:
		for _, e := range v.Lhs {
			w.expr(e)
		}
		for _, e := range v.Rhs {
			w.expr(e)
		}
	case *ast.LocalAssignStmt:
		for _, e := range v.Exprs {
			w.expr(e)
		}
	case *ast.FuncCallStmt:
		w.expr(v.Expr)
	case *ast.DoBlockStmt:
		w.stmts(v.Stmts)
	case *ast.WhileStmt:
		w.expr(v.Condition)
		w.stmts(v.Stmts)
	case *ast.RepeatStmt:
		w.stmts(v.Stmts)
		w.expr(v.Condition)
	case *ast.IfStmt:
		w.expr(v.Condition)
		w.stmts(v.Then)
		// elseif 在 gopher-lua 的 AST 里就是 Else 里嵌套的 IfStmt，
		// 没有独立的 ElseIfs 字段（踩过：照别的方言的 AST 写会编译不过）。
		w.stmts(v.Else)
	case *ast.NumberForStmt:
		w.expr(v.Init)
		w.expr(v.Limit)
		w.expr(v.Step)
		w.stmts(v.Stmts)
	case *ast.GenericForStmt:
		for _, e := range v.Exprs {
			w.expr(e)
		}
		w.stmts(v.Stmts)
	case *ast.FuncDefStmt:
		w.funcBody(v.Func)
	case *ast.ReturnStmt:
		for _, e := range v.Exprs {
			w.expr(e)
		}
	case *ast.BreakStmt, *ast.LabelStmt, *ast.GotoStmt:
		// 无子表达式。
	}
}

// funcBody 遍历函数体。
func (w *capWalker) funcBody(fn *ast.FunctionExpr) {
	if fn == nil {
		return
	}
	w.stmts(fn.Stmts)
}

// expr 遍历一个表达式。列表与单表达式都从这里进（调用参数、返回列表等）。
func (w *capWalker) expr(e ast.Expr) {
	switch v := e.(type) {
	case nil:
		return
	case *ast.AttrGetExpr:
		w.attrChain(v)
		w.expr(v.Key)
	case *ast.FuncCallExpr:
		w.expr(v.Func)
		w.expr(v.Receiver)
		for _, a := range v.Args {
			w.expr(a)
		}
	case *ast.FunctionExpr:
		w.funcBody(v)
	case *ast.TableExpr:
		for _, f := range v.Fields {
			if f != nil {
				w.expr(f.Key)
				w.expr(f.Value)
			}
		}
	case *ast.LogicalOpExpr:
		w.expr(v.Lhs)
		w.expr(v.Rhs)
	case *ast.RelationalOpExpr:
		w.expr(v.Lhs)
		w.expr(v.Rhs)
	case *ast.StringConcatOpExpr:
		w.expr(v.Lhs)
		w.expr(v.Rhs)
	case *ast.ArithmeticOpExpr:
		w.expr(v.Lhs)
		w.expr(v.Rhs)
	case *ast.UnaryMinusOpExpr:
		w.expr(v.Expr)
	case *ast.UnaryNotOpExpr:
		w.expr(v.Expr)
	case *ast.UnaryLenOpExpr:
		w.expr(v.Expr)
	case *ast.Comma3Expr:
		// Comma3Expr 只是「多返回值的位置标记」，自身不带子表达式。
	default:
		// 常量表达式（Number/String/True/False/Nil/Ident）不含 ctx 访问，无需下钻。
	}
}

// attrChain 处理 `ctx.a.b.c`：把**紧跟 ctx 的那一段**（即真正的能力名）记下来，
// 更深的段是方法名/字段名，不是能力。
//
// 只取第一段是刻意的：`ctx.ext.redis.set` 的能力名是 `ext.redis`（两段），
// 但它不在受门禁原语名单里（由 GrantsFromStrings 判），故这里拿到 `ext` 后匹配不上就放过；
// `ctx.db.query` 拿到 `db` → 命中。
func (w *capWalker) attrChain(v *ast.AttrGetExpr) {
	name, ok := attrKey(v.Key)
	if !ok {
		return
	}
	// 往上回溯到链根，看是不是 ctx。
	if !isCtxRoot(v.Object) {
		// 不是 ctx 开头：不认。但仍要下钻——`x.db` 里若嵌着别的 ctx 链不能漏。
		w.expr(v.Object)
		return
	}
	// IsBuiltin（而非 Known）：纯函数原子不在此列，而它们**始终可用、无需声明**——
	// 把它们报成「用了没声明」是错的（声明了反而会被拒，见 GrantsFromStrings）。
	if cap.IsBuiltin(cap.Primitive(name)) {
		w.add(name, v.Line(), "ctx."+name)
	}
	w.expr(v.Object)
}

// isCtxRoot 报告这个表达式是不是标识符 `ctx`。
func isCtxRoot(e ast.Expr) bool {
	id, ok := e.(*ast.IdentExpr)
	return ok && id.Value == "ctx"
}

// attrKey 取 `x.y` 里的 `y`（标识符形态；`x["y"]` 不认——见文件头「明确的边界」）。
func attrKey(e ast.Expr) (string, bool) {
	k, ok := e.(*ast.StringExpr)
	if !ok {
		return "", false
	}
	return k.Value, true
}

// ToolCapsError 把「用了却没声明」的缺口编成一条注册期错误。
//
// 文案口径：说清**三件事**——哪个能力、脚本第几行、怎么修（给出可直接复制的 YAML 片段）。
// 只说「未声明的 capability: db」是不够的：读者还得自己数行号、自己猜要加哪一行字段。
// 行号给的是 `entry.lua:N` 而不是裸数字：Tool 的 entry 不是文件路径，写成 `entry.lua`
// 是「entry 脚本第 N 行」的简写，读者一看就懂指的是哪段代码（不假装它是个真实路径）。
func ToolCapsError(toolID string, missing []Use) error {
	if len(missing) == 0 {
		return nil
	}
	names := make([]string, 0, len(missing))
	seen := map[string]bool{}
	where := make([]string, 0, len(missing))
	for _, u := range missing {
		if !seen[u.Capability] {
			seen[u.Capability] = true
			names = append(names, u.Capability)
		}
		where = append(where, u.Expr+"（entry.lua:"+strconv.Itoa(u.Line)+"）")
	}
	// YAML 片段按能力名排序（names 已排序，见 MissingCapabilities）。
	yamlList := ""
	for i, n := range names {
		if i > 0 {
			yamlList += ", "
		}
		yamlList += n
	}
	return fmt.Errorf("tool %s 的 entry 用了未声明的 capability: %s（出现在 %s）。"+
		"未声明的能力在 Lua 里**压根不存在**（ctx.%s 是 nil），运行期会报 "+
		"`attempt to call a non-function object`——看着像函数写错了，其实是漏声明。"+
		"修法：给这个 Tool 加 capabilities: [%s]（声明完立刻生效，不需要重建任何东西）",
		toolID, strings.Join(names, ", "), strings.Join(where, "、"),
		names[0], yamlList)
}
