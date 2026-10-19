package engine

// invokeform.go —— 「某个能力方法在 Lua 侧到底怎么调」的**唯一判据**。
//
// 存在理由（2026-10-03 外部实测）：能力目录此前只给参数名与方法名，不给调用形态，
// 而 Lua 侧的形态是**混合**的：
//
//	http.Request(method, url, body?, headers?)   位置参数（可选参数按 GetTop 判缺省）
//	secret.get(name) / event.notify(name, payload?)
//	subscribe.stop(handle)
//	call(target, input?)                          **裸函数**（ctx.call，不是 ctx.call.Call）
//	db.query({ driver, dsn, sql, params })        单具名 table
//	config.get({ key }) / subscribe.start({ ... })
//	crypto.jwt_sign({ ... }) / ext.redis.set({ ... })  纯函数原子与扩展同款单具名 table
//
// 模型/作者若照「参数都是位置参数」去写，编出来的 `ctx.db.query(sql, params)` 要到 Lua
// 运行期才炸成「attempt to call a non-function object」或参数错位——排障成本全在运行期。
//
// 判据来源（刻意复用已有三张表，不另写一份字面量清单——两份副本必然漂移）：
//   - bareFunctionPrimitives：绑定成裸函数的原语（call）
//   - builtinParamOrder：受门禁原语的位置参数顺序；值为 {"table"} 的即单具名 table 形态
//   - pureFuncPrimitives：纯函数原子，一律单具名 table
//   - ext.*：一律单具名 table（见 lua.go 的 ctx.ext 装配）
//
// 无状态：形态判据是纯函数（能力名+方法名 → 形态），能力目录渲染、提示词渲染、
// react 工具声明三处共用同一份实现——三处各判一次，判据迟早漂移。

import (
	"fmt"
	"strings"
)

// 三种 Lua 调用形态。取值写进能力目录（invoke_form 字段）给模型与作者看。
const (
	// InvokeFormPositional 位置参数：ctx.http.Request(a, b, c)。
	InvokeFormPositional = "positional"
	// InvokeFormTable 单具名 table：ctx.db.query({ driver = ..., sql = ... })。
	InvokeFormTable = "table"
	// InvokeFormBare 裸函数：ctx.call(target, input)（没有中间那段方法名）。
	InvokeFormBare = "bare"
)

// invokeFormFor 是全能力（内核原语 + ext.*）的形态判据，返回形态与可复制的调用签名。
//
// 返回 ok=false 表示「这个能力.方法组合没登记」——调用方据此报错或跳过，
// **不编一个形态出来**（红线：解析点拒绝未知值，不静默降级）。
//
// extCaps 由调用方传入（各消费点本来就持有它），判据本身不持有任何状态。
func invokeFormFor(cap, method string, extCaps map[string]map[string]MethodSchema) (form string, sig string, ok bool) {
	if form, sig, ok = builtinInvokeForm(cap, method); ok {
		return form, sig, true
	}
	if !strings.HasPrefix(cap, "ext.") {
		return "", "", false
	}
	var params map[string]ParamSchema
	if ms, found := extCaps[cap]; found {
		if sch, found := ms[method]; found {
			params = sch.Parameters
		}
	}
	// 参数取不到就渲染成 `{  }`：方法名合法性由各自入口的 unknown-method 校验负责
	//（这里不重复当第二道门——两处各判一次必然漂移）。
	return InvokeFormTable, fmt.Sprintf("ctx.%s.%s({ %s })", cap, method, fieldsList(params)), true
}

// builtinInvokeForm 判定一个**内核原语**方法的 Lua 调用形态。
//
// sig 里的参数名取自 schema（builtinCapabilitySchemas），位置表的顺序只用来排位次：
// 位置表给的是「Lua 第几位的名字」，schema 给的是「这个参数叫什么、什么类型」，
// 混用会渲染出 `ctx.http.Request(driver, dsn)` 这种看着像真的东西。
func builtinInvokeForm(cap, method string) (form string, sig string, ok bool) {
	// 纯函数原子先判：它们不在 builtinParamOrder 里（那表只管受门禁原语）。
	if pureFuncPrimitives[cap] {
		ms, found := builtinCapabilitySchemas[cap]
		if !found {
			return "", "", false
		}
		sch, found := ms[method]
		if !found {
			return "", "", false
		}
		return InvokeFormTable, luaTableCall(cap, method, sch.Parameters), true
	}

	order, isBuiltin := builtinParamOrder[cap]
	if !isBuiltin {
		return "", "", false
	}
	params, found := order[method]
	if !found {
		return "", "", false
	}
	sch := builtinCapabilitySchemas[cap][method]

	// 裸函数：ctx.call(...)——**没有 .Call 这一段**，这是最容易被编错的一处
	//（「索引一个函数」要到运行期才炸，见 bareFunctionPrimitives 的注释）。
	if bareFunctionPrimitives[cap] {
		return InvokeFormBare, fmt.Sprintf("ctx.%s(%s)", cap, strings.Join(params, ", ")), true
	}

	// 位置表里写死 {"table"} 的（db.* / config.* / subscribe.start）走单具名 table：
	// 真实绑定就是这么实现的（lua.go 里 luaDBArgs / config 取 L.Get(1) 的 map），
	// 位置表用 "table" 这个占位名只是复用同一张表表达「这里不是位置参数」。
	if len(params) == 1 && params[0] == "table" {
		return InvokeFormTable, luaTableCall(cap, method, sch.Parameters), true
	}

	// 真·位置参数：按位置表顺序取名（Lua 侧按位读，次序必须对得上）。
	names := make([]string, 0, len(params))
	for i, p := range params {
		if ps, found := sch.Parameters[p]; found {
			names = append(names, fmt.Sprintf("%s = %s", p, ps.Type))
			continue
		}
		// 位置表里有名但 schema 没登记 → 如实标出来，不静默编个类型。
		names = append(names, fmt.Sprintf("%s = ?（第 %d 位，schema 未登记此参数）", p, i+1))
	}
	return InvokeFormPositional, fmt.Sprintf("ctx.%s.%s(%s)", cap, method, strings.Join(names, ", ")), true
}

// luaTableCall 渲染单具名 table 形态的签名（字段名与类型取自 schema，按名排序）。
func luaTableCall(cap, method string, params map[string]ParamSchema) string {
	return fmt.Sprintf("ctx.%s.%s({ %s })", cap, method, fieldsList(params))
}
