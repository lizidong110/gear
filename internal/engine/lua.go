package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strings"
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
//   - 宿主能力：全局 `ctx.http / ctx.call / ctx.db / ctx.secret / ctx.event`
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
	// caps 是扩展原语目录（用户/插件注册的 ctx.<name>）；nil 时仅内核原语可用。
	caps *CapabilityRegistry
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

// luaDBArgs 从 Lua 第 1 个参数读取 db 调用的 {driver?, dsn?, sql, params?} 表。
// 调用方已先行 host.DB() 探过实现可用性，故此处只负责参数塑形；缺 sql 即报错（不做静默 no-op）。
func luaDBArgs(L *lua.LState) (driver, dsn, sql string, params []interface{}, err error) {
	m, ok := fromLuaValue(L, L.Get(1)).(map[string]interface{})
	if !ok {
		return "", "", "", nil, fmt.Errorf("db 调用需传入 table：{driver?, dsn?, sql, params?}")
	}
	sql, _ = m["sql"].(string)
	if sql == "" {
		return "", "", "", nil, fmt.Errorf("db 调用缺少必填参数 sql")
	}
	driver, _ = m["driver"].(string)
	dsn, _ = m["dsn"].(string)
	if p, ok := m["params"].([]interface{}); ok {
		params = p
	}
	return driver, dsn, sql, params, nil
}

// subscriptionRequestFromMap 把 Lua table（JSON 兼容 map）塑形成 Subscribe.Start 的请求体。
// 手动解析而非 JSON round-trip：保持与 stdio/HTTP 侧同一套字段语义，避开往返引入的键漂移。
func subscriptionRequestFromMap(m map[string]interface{}) (cap.SubscriptionRequest, error) {
	var req cap.SubscriptionRequest
	source, _ := m["source"].(string)
	if source == "" {
		return req, fmt.Errorf("subscribe.start 缺少必填参数 source")
	}
	req.Source = source
	switch c := m["config"].(type) {
	case nil:
	case map[string]interface{}:
		req.Config = c
	default:
		return req, fmt.Errorf("subscribe.start 的 config 必须是 object")
	}
	cm, ok := m["callback"].(map[string]interface{})
	if !ok {
		return req, fmt.Errorf("subscribe.start 缺少必填参数 callback（{type, target, input}）")
	}
	cb, err := subscriptionCallbackFromMap(cm)
	if err != nil {
		return req, err
	}
	req.Callback = cb
	return req, nil
}

// subscriptionCallbackFromMap 塑形投递动作（run/notify）。target 必填；input 为模板（run 时与事件负载合并）。
func subscriptionCallbackFromMap(m map[string]interface{}) (cap.SubscriptionCallback, error) {
	var cb cap.SubscriptionCallback
	cb.Type, _ = m["type"].(string)
	cb.Target, _ = m["target"].(string)
	if cb.Target == "" {
		return cb, fmt.Errorf("subscribe.start 的 callback 缺少必填参数 target")
	}
	if in, ok := m["input"].(map[string]interface{}); ok {
		cb.Input = in
	}
	return cb, nil
}

// setupLuaCapabilities 把 Tool.Capabilities 声明的宿主能力原语注入 Lua 的 ctx 表（M5，PRD §6）。
//
// 安全模型（最小权限 + 防御纵深）：
//   - **仅注入已声明的原语**：未声明的原语根本不出现在 ctx 上（比「存在但报错」更严格，
//     脚本侧连该字段都读不到，无法探测宿主能力边界）。
//   - 已声明但未提供实现者（如 db，宿主未注入 Providers.DB），调用时由 cap.Context 返回
//     ErrCapabilityUnavailable ——「文档说有、代码无」变成可见的明确错误，而非静默成功。
//   - 所有原语函数都在宿主侧执行，闭包捕获的是经过授权的实现；脚本**无法绕过沙箱**直接接触宿主。
//   - 末尾额外注入用户/插件注册的扩展原语（reg 非空时）：遍历已声明且非内置的名字，查目录
//     注入 ctx.<name>；未声明的扩展名同样不出现在 ctx 上（脚本无法探测）。
func setupLuaCapabilities(L *lua.LState, ctxTable *lua.LTable, host *cap.Context, reg *CapabilityRegistry) {
	if host == nil {
		return
	}
	declared := map[string]bool{}
	for _, c := range host.Capabilities() {
		declared[c] = true
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
					if m, ok := fromLuaValue(L, L.Get(4)).(map[string]interface{}); ok {
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

	// ctx.json: decode(str) -> table|nil,err ；encode(table) -> string。
	// 纯函数、无外部依赖，故无条件提供（不属 capabilities 门禁）。所有需要解析 HTTP
	// 响应体 / 结构化文本的 Lua 工具都复用它——否则 ctx.http.Request 只能拿到字符串 body。
	{
		t := L.NewTable()
		t.RawSetString("decode", L.NewFunction(func(L *lua.LState) int {
			s := L.CheckString(1)
			var v interface{}
			if err := json.Unmarshal([]byte(s), &v); err != nil {
				L.Push(lua.LNil)
				L.Push(lua.LString(err.Error()))
				return 2
			}
			L.Push(toLuaValue(L, v))
			return 1
		}))
		t.RawSetString("encode", L.NewFunction(func(L *lua.LState) int {
			v := fromLuaValue(L, L.Get(1))
			b, err := json.Marshal(v)
			if err != nil {
				L.Push(lua.LString(""))
				L.Push(lua.LString(err.Error()))
				return 2
			}
			L.Push(lua.LString(string(b)))
			return 1
		}))
		ctxTable.RawSetString("json", t)
	}

	// 纯函数核心原子（time/template/crypto）：内核内建、始终可用，故与 ctx.json 一样**无条件**
	// 注入（不查 grants——它们没有「宿主能力边界」可言，要求声明 capabilities 反而违背直觉，
	// 见 ATOM_ROADMAP.md §0 与 purefunc.go）。
	// 方法名**由 schema 的键生成**（单一来源）：新增/改名方法只改 purefunc.go 的 schema，
	// Lua 绑定自动跟随，不会出现「能力目录里有、ctx 上却没有」的漂移。
	// 统一「单具名 table」入参（与 ctx.ext.* 同款），调用落到唯一调度点 callPureFunc。
	for _, prim := range purePrimitiveOrder {
		funcs := builtinCapabilitySchemas[prim]
		if len(funcs) == 0 {
			continue
		}
		t := L.NewTable()
		for _, method := range sortedKeys(funcs) {
			p, m := prim, method
			sig := fieldsList(funcs[m].Parameters)
			t.RawSetString(m, L.NewFunction(func(L *lua.LState) int {
				args := map[string]interface{}{}
				if L.GetTop() >= 1 && L.Get(1) != lua.LNil {
					mt, ok := fromLuaValue(L, L.Get(1)).(map[string]interface{})
					if !ok {
						L.RaiseError("%s.%s 的参数必须是 table：{ %s }", p, m, sig)
						return 0
					}
					args = mt
				}
				res, err := callPureFunc(p, m, args)
				if err != nil {
					L.RaiseError("%s.%s 失败: %v", p, m, err)
					return 0
				}
				L.Push(toLuaValue(L, res))
				return 1
			}))
		}
		ctxTable.RawSetString(prim, t)
	}

	// ctx.call: Call(target, input_table?) -> result_table（插件间唯一通信通道）
	if declared["call"] {
		if c, err := host.Call(); err == nil {
			caller := c
			ctxTable.RawSetString("call", L.NewFunction(func(L *lua.LState) int {
				target := L.CheckString(1)
				var input map[string]interface{}
				if m, ok := fromLuaValue(L, L.Get(2)).(map[string]interface{}); ok {
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
					payload = fromLuaValue(L, L.Get(2))
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

	// ctx.subscribe: start(table) -> handle；stop(handle)。通用订阅原语（与 event 平级）：
	// Source（事件源，首批 mqtt）+ Deliver（投递动作 run/notify）经 cap.Subscribe 装配；
	// 消费回调仅 serve/监管模式可用（需引擎监听 HTTP 回推端点），一次性执行 Start 报明确错误。
	if declared["subscribe"] {
		if sub, err := host.Subscribe(); err == nil {
			ss := sub
			t := L.NewTable()
			t.RawSetString("start", L.NewFunction(func(L *lua.LState) int {
				m, ok := fromLuaValue(L, L.Get(1)).(map[string]interface{})
				if !ok {
					L.RaiseError("subscribe.start 需传入 table：{source, config, callback}")
					return 0
				}
				req, err := subscriptionRequestFromMap(m)
				if err != nil {
					L.RaiseError("%v", err)
					return 0
				}
				handle, err := ss.Start(req)
				if err != nil {
					L.RaiseError("subscribe.start 失败: %v", err)
					return 0
				}
				L.Push(lua.LString(handle))
				return 1
			}))
			t.RawSetString("stop", L.NewFunction(func(L *lua.LState) int {
				if err := ss.Stop(L.CheckString(1)); err != nil {
					L.RaiseError("subscribe.stop 失败: %v", err)
					return 0
				}
				return 0
			}))
			ctxTable.RawSetString("subscribe", t)
		}
	}

	// ctx.db: query/exec/ping（多后端：sqlite/mysql/postgres；dsn 缺省回退引擎 scratch 库）。
	// 调用形态：ctx.db.query({ driver="sqlite", dsn="", sql="...", params={...} })；
	// 参数形态与 builtinCapabilitySchemas["db"] 一一对应（见 extensions.go）。
	if declared["db"] {
		t := L.NewTable()
		t.RawSetString("query", L.NewFunction(func(L *lua.LState) int {
			d, err := host.DB()
			if err != nil {
				L.RaiseError("db.query 失败: %v", err)
				return 0
			}
			driver, dsn, sql, params, err := luaDBArgs(L)
			if err != nil {
				L.RaiseError("db.query 失败: %v", err)
				return 0
			}
			rows, err := d.Query(driver, dsn, sql, params...)
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
			driver, dsn, sql, params, err := luaDBArgs(L)
			if err != nil {
				L.RaiseError("db.exec 失败: %v", err)
				return 0
			}
			if err := d.Exec(driver, dsn, sql, params...); err != nil {
				L.RaiseError("db.exec 失败: %v", err)
				return 0
			}
			return 0
		}))
		t.RawSetString("ping", L.NewFunction(func(L *lua.LState) int {
			d, err := host.DB()
			if err != nil {
				L.RaiseError("db.ping 失败: %v", err)
				return 0
			}
			driver, dsn, _, _, err := luaDBArgs(L)
			if err != nil {
				L.RaiseError("db.ping 失败: %v", err)
				return 0
			}
			if err := d.Ping(driver, dsn); err != nil {
				L.RaiseError("db.ping 失败: %v", err)
				return 0
			}
			return 0
		}))
		ctxTable.RawSetString("db", t)
	}

	// 扩展原语（用户/插件注册）：遍历已声明且非内置的名字，查目录注入 ctx.<name>。
	// 扩展名形如 "ext.xxx"，按命名空间嵌套注入为 ctx.ext.xxx（两级），与脚本的
	// `ctx.ext.slack.send(...)` 自然对应；未声明（不在此循环）的扩展名根本不出现在 ctx 上
	// （比「存在但报错」更严格，脚本无法探测边界）。
	if reg != nil {
		for _, name := range host.Capabilities() {
			if cap.IsBuiltin(cap.Primitive(name)) {
				continue
			}
			if !strings.HasPrefix(name, "ext.") {
				continue
			}
			ext, ok := reg.Lookup(name)
			if !ok {
				continue
			}
			lv, err := ext.BuildLua(L, host)
			if err != nil || lv == lua.LNil {
				continue
			}
			// 嵌套：确保 ctx.ext 表存在，再把 ext.<suffix> 设为其字段。
			suffix := strings.TrimPrefix(name, "ext.")
			extTbl := ctxTable.RawGetString("ext")
			var et *lua.LTable
			if extTbl == lua.LNil {
				et = L.NewTable()
				ctxTable.RawSetString("ext", et)
			} else {
				et = extTbl.(*lua.LTable)
			}
			et.RawSetString(suffix, lv)
		}
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
	setupLuaCapabilities(L, ctxTable, host, l.caps)
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
	out, ok := fromLuaValue(L, outVal).(map[string]interface{})
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
		t := newLuaArrayTable(L)
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
		// 兜底：任意 slice/map（含 []map[string]interface{} 这类具名元素类型）也递归展开，
		// 否则会掉进 fmt.Sprint 被转成字符串（与 resolveValue 字符串化是同一类 bug）。
		rv := reflect.ValueOf(v)
		switch rv.Kind() {
		case reflect.Slice, reflect.Array:
			t := newLuaArrayTable(L)
			for i := 0; i < rv.Len(); i++ {
				t.RawSetInt(i+1, toLuaValue(L, rv.Index(i).Interface()))
			}
			return t
		case reflect.Map:
			t := L.NewTable()
			for _, k := range rv.MapKeys() {
				if ks, ok := k.Interface().(string); ok {
					t.RawSetString(ks, toLuaValue(L, rv.MapIndex(k).Interface()))
				}
			}
			return t
		default:
			return lua.LString(fmt.Sprint(x))
		}
	}
}

// fromLuaValue 把 Lua 值转回 JSON 兼容的 Go 值。
// 整数值的 number 转 int64（让 JSON Schema 的 number 与 integer 都能通过校验）。
func fromLuaValue(L *lua.LState, v lua.LValue) interface{} {
	switch x := v.(type) {
	case *lua.LTable:
		return luaTableToGo(L, x)
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

// luaArrayMarker 是挂在 metatable 上的标记键：表示「这个 table 由 Go 的 slice/array 转来」。
//
// 存在的理由（2026-09-22 实测缺陷）：Lua 的 table 没有数组/对象之分，**空表尤其无法自证**——
// luaTableToGo 只能按「非空且全数字键」判数组，于是 `db.query` 查到 0 行时回程变成 `{}`
// 而不是 `[]`，调用方无法区分「查到 0 行」与「出错」（同一个 `{}` 两种含义）。
//
// 做法：Go→Lua 时给数组 table 挂一个只含标记的 metatable，回程据此定夺。
// 标记放在 metatable 而非表内容里，所以 `pairs` / `ipairs` / `#` 都看不到它，不污染数据；
// 也不设 `__index`，不影响取值语义。
//
// 已知边界：Lua 侧若把数组 table **复制**成新表（如 `{table.unpack(rows)}`），标记不会跟着走，
// 空数组仍会退化成 `{}`；而 `ctx.json.encode` 是 Lua 侧序列化，同样看不到本标记。
// 本标记只保证「Go 产出 → Lua 原样带回」这条最常见路径上类型不丢。
const luaArrayMarker = "__gear_array"

// newLuaArrayTable 建一个带数组标记的 Lua table。
func newLuaArrayTable(L *lua.LState) *lua.LTable {
	t := L.NewTable()
	mt := L.NewTable()
	mt.RawSetString(luaArrayMarker, lua.LTrue)
	L.SetMetatable(t, mt)
	return t
}

// markedAsArray 判定 table 是否带数组标记（来自 Go 的 slice/array）。
//
// 必须经 **L.GetMetatable** 取元表：gopher-lua 的 *LTable 不导出 metatable 字段，
// 也没有 Metatable() getter（2026-09-22 实测踩到：`t.Metatable()` 编译不过）。
func markedAsArray(L *lua.LState, t *lua.LTable) bool {
	mt := L.GetMetatable(t)
	if mt == nil || mt == lua.LNil {
		return false
	}
	mtb, ok := mt.(*lua.LTable)
	if !ok {
		return false
	}
	return lua.LVAsBool(mtb.RawGetString(luaArrayMarker))
}

// luaTableToGo 把 Lua table 转成 []interface{}（连续数字键 1..n）或 map[string]interface{}。
//
// 判定顺序：**显式标记优先**（来自 Go 的数组，含空数组），其次按「非空且全数字键」推断。
// 前者是本次修复新增的：没有它，空数组只能落进 map 分支变成 `{}`。
func luaTableToGo(L *lua.LState, t *lua.LTable) interface{} {
	n := t.Len()
	marked := markedAsArray(L, t)
	isArray := marked || n > 0
	if isArray && !marked {
		t.ForEach(func(k, _ lua.LValue) {
			if _, ok := k.(lua.LNumber); !ok {
				isArray = false
			}
		})
	}
	if isArray {
		arr := make([]interface{}, 0, n)
		for i := 1; i <= n; i++ {
			arr = append(arr, fromLuaValue(L, t.RawGetInt(i)))
		}
		return arr
	}
	m := map[string]interface{}{}
	t.ForEach(func(k, v lua.LValue) {
		// 键统一字符串化（数字键也算），避免类型断言 panic。
		m[fmt.Sprint(fromLuaValue(L, k))] = fromLuaValue(L, v)
	})
	return m
}
