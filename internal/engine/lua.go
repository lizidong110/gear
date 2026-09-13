package engine

import (
	"context"
	"fmt"
	"math"
	"time"

	"gear/internal/cap"
	lua "github.com/yuin/gopher-lua"
)

// luaRuntime 实现 Runtime 接口：嵌入式 Lua 脚本，沙箱执行（PRD §3.2/§3.3）。
//
// 契约（S7a）：
//   - entry = **Lua 源码**（inline；YAML 块标量天然支持多行）
//   - 输入：全局 `ctx.input`（table）
//   - 输出：写 `ctx.output`（table）或脚本 `return` 一个 table（`ctx.output` 优先）
//   - 宿主能力：全局 `ctx.http / ctx.call / ctx.db / ctx.kv / ctx.secret / ctx.event`
//     （M5，按 Tool.Capabilities 白名单开放；未声明的访问返回 lua 错误，而非静默成功）
//
// 沙箱：只开 base/string/table/math；不加载 os/io/package/debug/coroutine，
// 并移除 dofile/loadfile/require/module —— 脚本只能纯计算，**无法接触宿主**，
// 除非经由上述 capability 原语（且经最小权限放行）。
//
// service：**不支持**（长驻需要 ctx.* 才能有意义；见 errLuaServiceUnsupported）。
//
// 沙箱策略可配置（REQ-LUA-SANDBOX-CONFIG，已落地）：引擎级 `WithSandbox(strict|permissive)`
// 决定 Lua 开放哪些标准库。strict（默认）仅 base/string/table/math；permissive 额外开 coroutine。
// 文件加载类全局（dofile/loadfile/require/module）无论何种策略一律移除 —— 配置项只能「开更多纯计算库」，
// 不能「关掉沙箱本身」（Lua 逃逸宿主是 P0 级风险）。
type luaRuntime struct {
	// policy 是 Lua 沙箱档位（strict/permissive），由引擎 WithSandbox 透传（REQ-LUA-SANDBOX-CONFIG）。
	policy string
}

// luaForbiddenGlobals 是沙箱必须从全局表移除的名字（防宿主逃逸）。
// 无论 strict 还是 permissive，文件加载类全局一律禁止 —— 这是 Lua 逃逸宿主的主通道，
// 配置项只允许「开更多纯计算库」，不允许「关掉沙箱本身」。
var luaForbiddenGlobals = []string{"dofile", "loadfile", "require", "module"}

// errLuaServiceUnsupported 由 Start 返回：lua 不做 service，长驻请用 process runtime。
var errLuaServiceUnsupported = fmt.Errorf("lua runtime 不支持 service 模式（长驻需 ctx.* 宿主能力，计划 M5）；请改用 process runtime")

// luaLib 是沙箱允许加载的标准库条目（白名单，其余一律不开）。
type luaLib struct {
	name string
	fn   lua.LGFunction
}

// luaAllowedLibs 是允许加载的标准库（白名单，其余一律不开）。
// strict（默认）：base/string/table/math（纯计算，无法接触宿主）。
// permissive：在 strict 基础上额外开放 coroutine/utf8（仍是纯计算，无文件/网络/os 逃逸）。
func luaAllowedLibs(policy string) []luaLib {
	libs := []luaLib{
		{lua.BaseLibName, lua.OpenBase},
		{lua.StringLibName, lua.OpenString},
		{lua.TabLibName, lua.OpenTable},
		{lua.MathLibName, lua.OpenMath},
	}
	if policy == SandboxPermissive {
		libs = append(libs,
			luaLib{lua.CoroutineLibName, lua.OpenCoroutine},
		)
	}
	return libs
}

// newSandboxState 按沙箱策略构造一个只带安全库的 Lua 状态机。
// policy 为空或非法时退化为 strict（与引擎默认一致）。
func newSandboxState(policy string) (*lua.LState, error) {
	L := lua.NewState(lua.Options{SkipOpenLibs: true})
	for _, lib := range luaAllowedLibs(policy) {
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

// setupLuaCapabilities 把 Tool.Capabilities 声明的宿主能力原语注入 Lua 的 ctx 表（M5，PRD §6）。
//
// 安全模型（最小权限 + 防御纵深）：
//   - **仅注入已声明的原语**：未声明的原语根本不出现在 ctx 上（比「存在但报错」更严格，
//     脚本侧连该字段都读不到，无法探测宿主能力边界）。
//   - 已声明但未提供实现者（如 db，宿主未注入 Providers.DB），调用时由 cap.Context 返回
//     ErrCapabilityUnavailable ——「文档说有、代码无」变成可见的明确错误，而非静默成功。
//   - 所有原语函数都在宿主侧执行，闭包捕获的是经过授权的实现；脚本**无法绕过沙箱**直接接触宿主。
func setupLuaCapabilities(L *lua.LState, ctxTable *lua.LTable, host *cap.Context) {
	if host == nil {
		return
	}
	declared := map[string]bool{}
	for _, c := range host.Capabilities() {
		declared[c] = true
	}

	// ctx.kv: get(k) -> string|nil；put(k,v)；delete(k)
	if declared["kv"] {
		if kv, err := host.KV(); err == nil {
			k := kv
			t := L.NewTable()
			t.RawSetString("get", L.NewFunction(func(L *lua.LState) int {
				v, ok := k.Get(L.CheckString(1))
				if !ok {
					L.Push(lua.LNil)
					return 1
				}
				L.Push(lua.LString(v))
				return 1
			}))
			t.RawSetString("put", L.NewFunction(func(L *lua.LState) int {
				k.Put(L.CheckString(1), L.CheckString(2))
				return 0
			}))
			t.RawSetString("delete", L.NewFunction(func(L *lua.LState) int {
				k.Delete(L.CheckString(1))
				return 0
			}))
			ctxTable.RawSetString("kv", t)
		}
	}

	// ctx.secret: get(name) -> string（只读，看不到存储位置）
	if declared["secret"] {
		if s, err := host.Secret(); err == nil {
			sec := s
			t := L.NewTable()
			t.RawSetString("get", L.NewFunction(func(L *lua.LState) int {
				v, err := sec.Get(L.CheckString(1))
				if err != nil {
					L.RaiseError("secret.get 失败: %v", err)
					return 0
				}
				L.Push(lua.LString(v))
				return 1
			}))
			ctxTable.RawSetString("secret", t)
		}
	}

	// ctx.http: Request(method, url, body?, headers?) -> {status, body, headers}
	if declared["http"] {
		if h, err := host.HTTP(); err == nil {
			cli := h
			t := L.NewTable()
			t.RawSetString("Request", L.NewFunction(func(L *lua.LState) int {
				method := L.CheckString(1)
				url := L.CheckString(2)
				var body []byte
				if L.GetTop() >= 3 && L.Get(3) != lua.LNil {
					body = []byte(L.OptString(3, ""))
				}
				headers := map[string]string{}
				if L.GetTop() >= 4 && L.Get(4) != lua.LNil {
					if m, ok := fromLuaValue(L.Get(4)).(map[string]interface{}); ok {
						for k, v := range m {
							headers[k] = fmt.Sprint(v)
						}
					}
				}
				resp, err := cli.Request(method, url, body, headers)
				if err != nil {
					L.RaiseError("http 请求失败: %v", err)
					return 0
				}
				out := L.NewTable()
				out.RawSetString("status", lua.LNumber(resp.StatusCode))
				out.RawSetString("body", lua.LString(string(resp.Body)))
				hm := L.NewTable()
				for k, v := range resp.Headers {
					hm.RawSetString(k, lua.LString(v))
				}
				out.RawSetString("headers", hm)
				L.Push(out)
				return 1
			}))
			ctxTable.RawSetString("http", t)
		}
	}

	// ctx.call: Call(target, input_table?) -> result_table（插件间唯一通信通道）
	if declared["call"] {
		if c, err := host.Call(); err == nil {
			caller := c
			ctxTable.RawSetString("call", L.NewFunction(func(L *lua.LState) int {
				target := L.CheckString(1)
				var input map[string]interface{}
				if m, ok := fromLuaValue(L.Get(2)).(map[string]interface{}); ok {
					input = m
				}
				out, err := caller.Call(target, input)
				if err != nil {
					L.RaiseError("call 失败: %v", err)
					return 0
				}
				L.Push(toLuaValue(L, out))
				return 1
			}))
		}
	}

	// ctx.event: notify(name, payload?)；wait(name) -> payload（同步，带 10s 超时防挂死）
	if declared["event"] {
		if ev, err := host.Event(); err == nil {
			e := ev
			t := L.NewTable()
			t.RawSetString("notify", L.NewFunction(func(L *lua.LState) int {
				name := L.CheckString(1)
				var payload interface{}
				if L.GetTop() >= 2 && L.Get(2) != lua.LNil {
					payload = fromLuaValue(L.Get(2))
				}
				e.Notify(name, payload)
				return 0
			}))
			t.RawSetString("wait", L.NewFunction(func(L *lua.LState) int {
				name := L.CheckString(1)
				type res struct {
					v   interface{}
					err error
				}
				ch := make(chan res, 1)
				go func() {
					v, err := e.Wait(name)
					ch <- res{v, err}
				}()
				select {
				case r := <-ch:
					if r.err != nil {
						L.RaiseError("event.wait 失败: %v", r.err)
						return 0
					}
					L.Push(toLuaValue(L, r.v))
					return 1
				case <-time.After(10 * time.Second):
					L.RaiseError("event.wait 超时（10s 无 notify）")
					return 0
				}
			}))
			ctxTable.RawSetString("event", t)
		}
	}

	// ctx.db: query/exec（原语已定义；若宿主未提供实现，调用返回 ErrCapabilityUnavailable）
	if declared["db"] {
		t := L.NewTable()
		t.RawSetString("query", L.NewFunction(func(L *lua.LState) int {
			d, err := host.DB()
			if err != nil {
				L.RaiseError("db.query 失败: %v", err)
				return 0
			}
			rows, err := d.Query(L.CheckString(1))
			if err != nil {
				L.RaiseError("db.query 失败: %v", err)
				return 0
			}
			L.Push(toLuaValue(L, rows))
			return 1
		}))
		t.RawSetString("exec", L.NewFunction(func(L *lua.LState) int {
			d, err := host.DB()
			if err != nil {
				L.RaiseError("db.exec 失败: %v", err)
				return 0
			}
			if err := d.Exec(L.CheckString(1)); err != nil {
				L.RaiseError("db.exec 失败: %v", err)
				return 0
			}
			return 0
		}))
		ctxTable.RawSetString("db", t)
	}
}

// Run 执行一段 Lua 源码（once 模式）。host 是本次执行的宿主能力视图（M5）：
// 未声明的 capability 访问会返回 lua 错误（最小权限），详见 setupLuaCapabilities 的 ctx.* 暴露。
func (l *luaRuntime) Run(ctx context.Context, entry string, input map[string]interface{}, host *cap.Context) (map[string]interface{}, error) {
	L, err := newSandboxState(l.policy)
	if err != nil {
		return nil, err
	}
	defer L.Close()

	// ctx.input（宿主能力原语 ctx.http/call/... 在 M5 注入，见 setupLuaCapabilities）
	ctxTable := L.NewTable()
	ctxTable.RawSetString("input", toLuaValue(L, input))
	setupLuaCapabilities(L, ctxTable, host)
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
func (l *luaRuntime) Start(ctx context.Context, entry string, input map[string]interface{}, host *cap.Context) (Process, error) {
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
