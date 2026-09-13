package engine

import (
	"context"
	"fmt"
	"math"

	lua "github.com/yuin/gopher-lua"
)

// luaRuntime 实现 Runtime 接口：嵌入式 Lua 脚本，沙箱执行（PRD §3.2/§3.3）。
//
// 契约（S7a）：
//   - entry = **Lua 源码**（inline；YAML 块标量天然支持多行）
//   - 输入：全局 `ctx.input`（table）
//   - 输出：写 `ctx.output`（table）或脚本 `return` 一个 table（`ctx.output` 优先）
//
// 沙箱：只开 base/string/table/math；不加载 os/io/package/debug/coroutine，
// 并移除 dofile/loadfile/require/module —— 脚本只能纯计算，**无法接触宿主**。
// 宿主能力（http/fs/db/kv/secret/event）将在 M5 的 `ctx.*` 落地后按 capability 白名单开放。
//
// service：**不支持**（长驻需要 ctx.* 才能有意义；见 errLuaServiceUnsupported）。
//
// deferred（REQ-LUA-SANDBOX-CONFIG，后期）：当前沙箱是**硬编码严格模式**。
// 用户已明确：拦截哪些「危险行为」应当**可配置**（有些行为可能是用户期望的），
// 由用户自己抉择。该配置化依赖 M5 的 capability 体系，故推迟到后期实现；
// 届时应把 luaAllowedLibs / luaForbiddenGlobals 改为来自策略配置（如 sandbox: strict|permissive
// 或逐项 capability 白名单），而不是现在的常量。
type luaRuntime struct{}

// luaForbiddenGlobals 是沙箱必须从全局表移除的名字（防宿主逃逸）。
var luaForbiddenGlobals = []string{"dofile", "loadfile", "require", "module"}

// errLuaServiceUnsupported 由 Start 返回：lua 不做 service，长驻请用 process runtime。
var errLuaServiceUnsupported = fmt.Errorf("lua runtime 不支持 service 模式（长驻需 ctx.* 宿主能力，计划 M5）；请改用 process runtime")

// luaAllowedLibs 是允许加载的标准库（白名单，其余一律不开）。
var luaAllowedLibs = []struct {
	name string
	fn   lua.LGFunction
}{
	{lua.BaseLibName, lua.OpenBase},
	{lua.StringLibName, lua.OpenString},
	{lua.TabLibName, lua.OpenTable},
	{lua.MathLibName, lua.OpenMath},
}

// newSandboxState 构造一个只带安全库的 Lua 状态机。
func newSandboxState() (*lua.LState, error) {
	L := lua.NewState(lua.Options{SkipOpenLibs: true})
	for _, lib := range luaAllowedLibs {
		if err := L.CallByParam(lua.P{
			Fn:      L.NewFunction(lib.fn),
			NRet:    0,
			Protect: true,
		}, lua.LString(lib.name)); err != nil {
			L.Close()
			return nil, fmt.Errorf("加载 lua 库 %q 失败: %w", lib.name, err)
		}
	}
	for _, name := range luaForbiddenGlobals {
		L.SetGlobal(name, lua.LNil)
	}
	return L, nil
}

// Run 执行一段 Lua 源码（once 模式）。
func (l *luaRuntime) Run(ctx context.Context, entry string, input map[string]interface{}) (map[string]interface{}, error) {
	L, err := newSandboxState()
	if err != nil {
		return nil, err
	}
	defer L.Close()

	// ctx.input
	ctxTable := L.NewTable()
	ctxTable.RawSetString("input", toLuaValue(L, input))
	L.SetGlobal("ctx", ctxTable)

	// 超时 / 取消：gopher-lua 会在解释循环中检查 context。
	if ctx != nil {
		L.SetContext(ctx)
	}

	fn, err := L.LoadString(entry)
	if err != nil {
		return nil, fmt.Errorf("lua 编译失败: %w", err)
	}
	L.Push(fn)
	if err := L.PCall(0, 1, nil); err != nil {
		if cErr := ctxErr(ctx); cErr != nil {
			return nil, fmt.Errorf("lua 执行被中断: %w", cErr)
		}
		return nil, fmt.Errorf("lua 执行失败: %w", err)
	}

	ret := L.Get(-1)
	L.Pop(1)

	// 输出：ctx.output 优先，其次脚本返回值。
	outVal := ctxTable.RawGetString("output")
	if outVal == lua.LNil || outVal == nil {
		outVal = ret
	}
	out, ok := fromLuaValue(outVal).(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("lua 脚本未产出 object 输出（请写 ctx.output 或 return 一个 table）")
	}
	return out, nil
}

// Start 报告 lua 不支持 service（长驻需 ctx.* 宿主能力）。
func (l *luaRuntime) Start(ctx context.Context, entry string, input map[string]interface{}) (Process, error) {
	return nil, errLuaServiceUnsupported
}

func ctxErr(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}

// toLuaValue 把 JSON 兼容的 Go 值转成 Lua 值（table 递归）。
func toLuaValue(L *lua.LState, v interface{}) lua.LValue {
	switch x := v.(type) {
	case nil:
		return lua.LNil
	case string:
		return lua.LString(x)
	case bool:
		return lua.LBool(x)
	case float64:
		return lua.LNumber(x)
	case float32:
		return lua.LNumber(float64(x))
	case int:
		return lua.LNumber(float64(x))
	case int64:
		return lua.LNumber(float64(x))
	case []interface{}:
		t := L.NewTable()
		for i, e := range x {
			t.RawSetInt(i+1, toLuaValue(L, e))
		}
		return t
	case map[string]interface{}:
		t := L.NewTable()
		for k, e := range x {
			t.RawSetString(k, toLuaValue(L, e))
		}
		return t
	default:
		return lua.LString(fmt.Sprint(x))
	}
}

// fromLuaValue 把 Lua 值转回 JSON 兼容的 Go 值。
// 整数值的 number 转 int64（让 JSON Schema 的 number 与 integer 都能通过校验）。
func fromLuaValue(v lua.LValue) interface{} {
	switch x := v.(type) {
	case *lua.LTable:
		return luaTableToGo(x)
	case lua.LString:
		return string(x)
	case lua.LNumber:
		f := float64(x)
		if f == math.Trunc(f) && !math.IsInf(f, 0) && f >= math.MinInt64 && f <= math.MaxInt64 {
			return int64(f)
		}
		return f
	case lua.LBool:
		return bool(x)
	case *lua.LNilType:
		return nil
	default:
		return nil
	}
}

// luaTableToGo 把 Lua table 转成 []interface{}（连续数字键 1..n）或 map[string]interface{}。
func luaTableToGo(t *lua.LTable) interface{} {
	n := t.Len()
	isArray := n > 0
	if isArray {
		t.ForEach(func(k, _ lua.LValue) {
			if _, ok := k.(lua.LNumber); !ok {
				isArray = false
			}
		})
	}
	if isArray {
		arr := make([]interface{}, 0, n)
		for i := 1; i <= n; i++ {
			arr = append(arr, fromLuaValue(t.RawGetInt(i)))
		}
		return arr
	}
	m := map[string]interface{}{}
	t.ForEach(func(k, v lua.LValue) {
		// 键统一字符串化（数字键也算），避免类型断言 panic。
		m[fmt.Sprint(fromLuaValue(k))] = fromLuaValue(v)
	})
	return m
}
