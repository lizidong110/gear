package engine

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"gear/internal/cap"
	"gear/internal/model"
	"gear/internal/registry"
	"gear/internal/sidecar/coord"
	lua "github.com/yuin/gopher-lua"
	"gopkg.in/yaml.v3"
)

// CapabilityExtension 是一个用户/插件注册的「宿主能力原语」（暴露为 Lua 的 ctx.<name>）。
//
// BuildLua 决定 Lua 侧如何把 ctx.<name> 暴露给脚本（返回一个表或函数）；Provider 决定
// go-host Tool 经 host.Extension(name) 取到的原生实现对象。两者都经 Tool.Capabilities
// 白名单约束：未声明该扩展名的 Tool 根本拿不到（最小权限，与内置原语同款门禁）。
//
// 安全约定：需要 I/O 的 Provider 一律走 host.*（HTTP/Secret/...）经宿主 Provider 取实现，
// 而不是在闭包里直接 os/network 操作——这样 capability 白名单与路径白名单仍能真正生效。
type CapabilityExtension struct {
	// BuildLua 把 ctx.<name> 注入 Lua 状态机；返回的值会 RawSet 到 ctx 表上。
	BuildLua func(L *lua.LState, c *cap.Context) (lua.LValue, error)
	// Provider 返回该扩展的原生实现对象（go-host Tool 经 host.Extension(name) 取用）。
	Provider func(c *cap.Context) (interface{}, error)
}

// CapabilityRegistry 是引擎持有的「扩展原语目录」：名字 -> 扩展定义。
//
// 内核原语（受门禁 7 + 纯函数 3）仍是硬编码的最小集（引擎受信核心不膨胀）；用户/插件的能力在此动态注册，
// 权限/校验/注入管道完全复用，安全模型零退化。
type CapabilityRegistry struct {
	mu   sync.Mutex
	exts map[string]*CapabilityExtension
	// schemas 是 ext 的「方法级参数 schema」（key=ext 名，如 "ext.kafka"）。
	// 由 SetupExtensions 在注册 ext 时就地登记——**单一来源仍是 extensions.yaml 的 schemas
	// 字段**（目录只存不造，不存在第二份方法清单）。引擎据此把「方法名」也纳入静态校验：
	// 此前只校验能力名，`ext.kafka.notamethod` 会一路通过注册期与运行期，
	// 到侧车才报错（见 stepcall.go checkCapabilityMethod 的历史注释）。
	schemas map[string]map[string]MethodSchema
	// SvcExt 是 service ext 的协调客户端（key=ext 名，如 "ext.mqtt"）。仅当该 ext 声明
	// service:true 时存在。引擎在 startServiceStep 命中 key 时走 coord 握手
	//（注册优先 / 竞态退让 / 看门狗心跳），停机时 Deregister（见 EXT_SERVICE_DESIGN.md §12）。
	SvcExt map[string]*coord.Client
	// Sources 是通用订阅原语（ctx.subscribe）的事件源工厂目录（key=源名，如 "mqtt"）。
	// service ext 声明 source 时在 SetupExtensions 登记；Subscriber 在装配时拷贝进自身。
	Sources map[string]SourceFactory
}

// NewCapabilityRegistry 构造一个空目录。
func NewCapabilityRegistry() *CapabilityRegistry {
	return &CapabilityRegistry{
		exts:    map[string]*CapabilityExtension{},
		schemas: map[string]map[string]MethodSchema{},
		SvcExt:  map[string]*coord.Client{},
		Sources: map[string]SourceFactory{},
	}
}

// SetSchemas 登记某 ext 的「方法级参数 schema」（SetupExtensions 在注册 ext 时调用）。
//
// 未登记的 ext（例如测试里直接 Register 的程序化扩展）在静态校验中退化为「只校验能力名」：
// 没有 schema 就无从校验方法名，退化范围是**显式**的，绝不假装校验过（不静默降级）。
func (r *CapabilityRegistry) SetSchemas(name string, schemas map[string]MethodSchema) {
	if len(schemas) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.schemas[name] = schemas
}

// LookupSchemas 按 ext 名取方法级参数 schema（静态校验用）。
func (r *CapabilityRegistry) LookupSchemas(name string) (map[string]MethodSchema, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ms, ok := r.schemas[name]
	return ms, ok
}

// Register 把名为 name 的扩展能力登记进目录，并并入 cap 的合法 capability 集合
// （如此 GrantsFromStrings 才会放行它）。约束：
//   - 名字必须以 "ext." 开头（命名空间隔离，防与内置名冲突）；
//   - 不能覆盖内置原语名（委托 cap.RegisterExtensionName 把关）；
//   - 同名覆盖（后注册者胜，与 go-host 的单一入口表取向一致）。
func (r *CapabilityRegistry) Register(name string, ext *CapabilityExtension) error {
	if ext == nil {
		return fmt.Errorf("扩展 %q 的定义不能为空", name)
	}
	if !strings.HasPrefix(name, "ext.") {
		return fmt.Errorf("扩展原语名 %q 必须以 ext. 开头", name)
	}
	if err := cap.RegisterExtensionName(cap.Primitive(name)); err != nil {
		return err
	}
	r.mu.Lock()
	r.exts[name] = ext
	r.mu.Unlock()
	return nil
}

// Lookup 按名查扩展定义（引擎注入 Lua / 取原生实现时用）。
func (r *CapabilityRegistry) Lookup(name string) (*CapabilityExtension, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.exts[name]
	return e, ok
}

// ---- Sidecar 载体：让用户无需写 Go 即可提供自己的原语 ----

// SidecarTransport 是扩展原语方法调用的实际转发通道（子进程 / 远程 / 测试桩）。
type SidecarTransport interface {
	Call(method string, args map[string]interface{}) (interface{}, error)
}

// SidecarProvider 把扩展原语的方法调用经 SidecarTransport 转发（默认 CommandTransport 跑子进程）。
// go-host Tool 经 host.Extension(name) 取到的是 *SidecarProvider，直接调其 Call 方法。
type SidecarProvider struct {
	t SidecarTransport
}

// Call 经底层 transport 转发一次方法调用。
func (s *SidecarProvider) Call(method string, args map[string]interface{}) (interface{}, error) {
	return s.t.Call(method, args)
}

// CommandTransport 是 SidecarTransport 的默认实现：每次调用起一个子进程，复用 process runtime
// 的 stdin/stdout JSON 协议——stdin 写 {method,args}，stdout 末行读 {result} 或 {error}。
// 用户用任意语言写一个读 stdin JSON、写 stdout JSON 的小程序即可提供自己的原语。
type CommandTransport struct {
	cmd []string
}

// Call 启动子进程、写请求、读末行响应并解析。
func (t *CommandTransport) Call(method string, args map[string]interface{}) (interface{}, error) {
	if len(t.cmd) == 0 {
		return nil, fmt.Errorf("sidecar 命令为空")
	}
	cmd := exec.Command(t.cmd[0], t.cmd[1:]...)
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("启动 sidecar 失败: %w", err)
	}
	_ = json.NewEncoder(in).Encode(map[string]interface{}{"method": method, "args": args})
	_ = in.Close()

	sc := bufio.NewScanner(out)
	var last string
	for sc.Scan() {
		last = sc.Text()
	}
	if err := cmd.Wait(); err != nil {
		return nil, fmt.Errorf("sidecar 退出异常: %w", err)
	}
	if last == "" {
		return nil, fmt.Errorf("sidecar 无响应")
	}
	var resp struct {
		Result interface{} `json:"result"`
		Error  string      `json:"error"`
	}
	if err := json.Unmarshal([]byte(last), &resp); err != nil {
		return nil, fmt.Errorf("解析 sidecar 响应失败: %w", err)
	}
	if resp.Error != "" {
		return nil, fmt.Errorf("sidecar 返回错误: %s", resp.Error)
	}
	return resp.Result, nil
}

// LocalHTTPTransport 是 SidecarTransport 的「长连接复用」实现：经本地 HTTP 调用侧车的
// 业务 RPC 端点（POST /<method>，body {"args":...}，响应 {"result"|"error"}）。
// 与 CommandTransport 同语义、同 JSON 形状，但复用一条已绑定的 sidecar 长连接——
// 直接消除 rabbitmq 那种"每次 amqp.Dial 重连"的浪费。仅 service ext 声明 listen 时启用
// （见 SetupExtensions）；非 service ext 仍走 CommandTransport（§5/§12）。
type LocalHTTPTransport struct {
	baseURL string
	client  *http.Client
}

// NewLocalHTTPTransport 构造指向 addr（如 "127.0.0.1:1884"）的本地 HTTP 传输。
// 显式开启 keep-alive 连接池（DisableKeepAlives:false + 合理 MaxIdleConnsPerHost），
// 保证多次方法调用复用同一条 TCP 长连接——直接消除 rabbitmq 那种"每次 amqp.Dial 重连"的浪费。
func NewLocalHTTPTransport(addr string) *LocalHTTPTransport {
	tr := &http.Transport{
		MaxIdleConns:        64,
		MaxIdleConnsPerHost: 64,
		IdleConnTimeout:     90 * time.Second,
		DisableKeepAlives:   false, // 显式开启 keep-alive，调用复用长连接
		TLSHandshakeTimeout: 10 * time.Second,
	}
	return &LocalHTTPTransport{baseURL: "http://" + addr, client: &http.Client{Transport: tr, Timeout: 10 * time.Second}}
}

// Call 经本地 HTTP 转发一次方法调用（与 CommandTransport 同契约）。
func (t *LocalHTTPTransport) Call(method string, args map[string]interface{}) (interface{}, error) {
	body, err := json.Marshal(map[string]interface{}{"args": args})
	if err != nil {
		return nil, err
	}
	resp, err := t.client.Post(t.baseURL+"/"+method, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("调用 sidecar(%s) 失败: %w", t.baseURL, err)
	}
	defer resp.Body.Close()
	var out struct {
		Result interface{} `json:"result"`
		Error  string      `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("解析 sidecar 响应失败: %w", err)
	}
	if out.Error != "" {
		return nil, fmt.Errorf("sidecar 返回错误: %s", out.Error)
	}
	return out.Result, nil
}

// SidecarExtension 构造一个「sidecar 载体」的扩展：把 methods 暴露为 ctx.<name> 的方法，
// 调用经子进程转发（CommandTransport）。cmd 是 sidecar 小程序启动命令（如 []string{"./slack-sidecar"}）。
//
// 示例（YAML 声明能力 + Lua 使用）：
//
//	reg.Register("ext.slack", SidecarExtension("ext.slack",
//	    []string{"./slack-sidecar"}, []string{"send", "history"}))
//
// 然后 Tool 声明 capabilities: [ext.slack]，Lua 里即可：
//
//	local r = ctx.ext.slack.send({ channel = "#ops", text = "alert" })
func SidecarExtension(name string, cmd []string, methods []string) *CapabilityExtension {
	return sidecarExtension(name, &CommandTransport{cmd: cmd}, methods)
}

// sidecarExtension 是 sidecar 载体的底层构造：把 methods 暴露为 ctx.<name> 的方法，
// 调用经给定 transport 转发（默认 CommandTransport 跑子进程，测试可注入 fake）。
// 与内置原语共享「授权但未实现 → ErrCapabilityUnavailable」的语义。
func sidecarExtension(name string, t SidecarTransport, methods []string) *CapabilityExtension {
	prov := &SidecarProvider{t: t}
	return &CapabilityExtension{
		Provider: func(c *cap.Context) (interface{}, error) { return prov, nil },
		BuildLua: func(L *lua.LState, c *cap.Context) (lua.LValue, error) {
			tbl := L.NewTable()
			for _, m := range methods {
				method := m
				tbl.RawSetString(method, L.NewFunction(func(L *lua.LState) int {
					p, err := c.Extension(name)
					if err != nil {
						L.RaiseError("%v", err)
						return 0
					}
					args := map[string]interface{}{}
					if m, ok := fromLuaValue(L, L.Get(1)).(map[string]interface{}); ok {
						args = m
					}
					res, err := p.(*SidecarProvider).Call(method, args)
					if err != nil {
						L.RaiseError("%v", err)
						return 0
					}
					L.Push(toLuaValue(L, res))
					return 1
				}))
			}
			return tbl, nil
		},
	}
}

// ---- 进程内 Go 原语：引擎状态 / 宿主能力类稳定扩展（不经 sidecar 子进程） ----

// InProcessExtension 构造「进程内 Go 原语」扩展：方法直接由 Go 函数 fn 处理。
// 用于「读引擎状态 / 宿主能力」类稳定原语（如 ext.registry），与 SidecarExtension 并列——
// 同样是 ext.<name> 命名空间、同样走 capabilities 白名单，只是实现载体从子进程换成 Go 闭包。
func InProcessExtension(name string, methods []string, fn func(method string, args map[string]interface{}) (interface{}, error)) *CapabilityExtension {
	return &CapabilityExtension{
		Provider: func(c *cap.Context) (interface{}, error) { return &inProcessProvider{fn: fn}, nil },
		BuildLua: func(L *lua.LState, c *cap.Context) (lua.LValue, error) {
			tbl := L.NewTable()
			for _, m := range methods {
				method := m
				tbl.RawSetString(method, L.NewFunction(func(L *lua.LState) int {
					args := map[string]interface{}{}
					if mt, ok := fromLuaValue(L, L.Get(1)).(map[string]interface{}); ok {
						args = mt
					}
					res, err := fn(method, args)
					if err != nil {
						L.RaiseError("%v", err)
						return 0
					}
					L.Push(toLuaValue(L, res))
					return 1
				}))
			}
			return tbl, nil
		},
	}
}

// inProcessProvider 是 InProcessExtension 的原生实现（go-host Tool 经 host.Extension(name) 取用）。
type inProcessProvider struct {
	fn func(string, map[string]interface{}) (interface{}, error)
}

func (p *inProcessProvider) Call(method string, args map[string]interface{}) (interface{}, error) {
	return p.fn(method, args)
}

// registryMethods 是 ext.registry 暴露的方法（注册与 schema 共用同一份清单，杜绝漂移）。
var registryMethods = []string{"tools", "flows", "capabilities"}

// registrySchemas 返回 ext.registry 的方法级参数 schema。
//
// ext.registry 由内核**始终注册**，故其 schema 也必须始终登记：否则
// `ext.registry.<任意名>` 会落在「只校验能力名」的退化路径上——那正是本次要堵的缺口。
//
// tools/flows 带 prefix + limit 两个**可选**参数（2026-09-27 加）：全量目录动辄上万 rune，
// 观察回灌给模型时被截断，模型只数得到看得见的那部分就下结论（实测「查 registry 有多少个
// Tool」答出 12，实际 34）。有了 prefix 它才能分批看，截断了也有得缩小——「请你缩小范围」
// 如果没有参数可传就是句空话。
func registrySchemas() map[string]MethodSchema {
	desc := map[string]string{
		"tools":        "列出引擎 registry 的 Tool（id/name/description/runtime/input_schema/output_schema）",
		"flows":        "列出已注册 Flow（id/name/description）",
		"capabilities": "返回内核原语（builtin + pure）与已注册 ext.* 的方法级参数 schema",
	}
	// 两个可选参数的 schema 定义一次，tools 与 flows 共用（否则改一处漏一处）。
	listParams := map[string]ParamSchema{
		"prefix": {Type: "string", Description: "只返回 id 以此开头的条目（用来分批查看）"},
		"limit":  {Type: "number", Description: "最多返回多少条（默认 50）"},
	}
	out := make(map[string]MethodSchema, len(registryMethods))
	for _, m := range registryMethods {
		params := map[string]ParamSchema{}
		if m == "tools" || m == "flows" {
			params = listParams
		}
		out[m] = MethodSchema{Description: desc[m], Parameters: params}
	}
	return out
}

// listArgs 解析 ext.registry.tools / .flows 的可选分页参数。零值语义一律「不限/默认」，
// 与「传了就是 0」区分开：不传 prefix = 不过滤，不传 limit = 默认上限。
func listArgs(args map[string]interface{}) (prefix string, limit int) {
	if v, ok := args["prefix"]; ok {
		if s, ok := v.(string); ok {
			prefix = s
		}
	}
	if v, ok := args["limit"]; ok {
		switch n := v.(type) {
		case int:
			limit = n
		case float64: // JSON 数字进 Lua 再回传会变成 float
			limit = int(n)
		case string:
			if _, err := fmt.Sscanf(n, "%d", &limit); err != nil {
				limit = 0
			}
		}
	}
	return prefix, limit
}

// defaultListLimit 是目录类观察的默认返回条数：够模型看清单，又不至于把单次观察顶到
// 截断线上（截断后模型数不请总数，宁可少给让它分批要）。
const defaultListLimit = 50

// RegistryExtension 是「进程内」稳定原语：暴露引擎 registry 的工具 / 流程 / 能力目录给 Lua
// （ctx.ext.registry.tools() / ctx.ext.registry.flows() / ctx.ext.registry.capabilities()）。
// 它是易变 agent 自我锚定的根：agent 不再依赖调用方喂 tools，而是自己读 registry，
// LLM 规划才有真实依据（PRD §9 锚定问题——否则 LLM 只能基于调用方塞进来的东西规划，必幻觉）。
//
// extCaps 是「已注册 ext.* 的方法级参数 schema」（来自 extensions.yaml 的 schemas 字段或
// 默认 ext.ai 的内置 schema），供 capabilities() 一并返回——这样 agent 既能锚定 tool 级 schema，
// 也能锚定 capability 级参数（选项2：打通 ctx.* 参数语义，堵住 tool 模式 entry 幻觉的缺口）。
func RegistryExtension(reg *registry.Registry, extCaps map[string]map[string]MethodSchema) *CapabilityExtension {
	return InProcessExtension("ext.registry", registryMethods, func(method string, args map[string]interface{}) (interface{}, error) {
		prefix, limit := listArgs(args)
		if limit <= 0 {
			limit = defaultListLimit
		}
		switch method {
		case "tools":
			tools, err := reg.ListTools()
			if err != nil {
				return nil, err
			}
			out := make([]map[string]interface{}, 0, len(tools))
			for _, t := range tools {
				if prefix != "" && !strings.HasPrefix(t.ID, prefix) {
					continue
				}
				if len(out) >= limit {
					break
				}
				out = append(out, map[string]interface{}{
					"id":            t.ID,
					"name":          t.Name,
					"description":   t.Description,
					"input_schema":  orSchema(t.InputSchema),
					"output_schema": orSchema(t.OutputSchema),
				})
			}
			// 刻意仍是数组：examples/ai-create-flow.yaml 里 `local tools =
			// ctx.ext.registry.tools() or {}` 后接 ipairs，改成 {total=..., tools=...}
			// 会让那段示例静默拿到空表。分页信息走观察侧的截断提示，不靠改形状传递。
			return out, nil
		case "flows":
			flows, err := reg.ListFlows()
			if err != nil {
				return nil, err
			}
			out := make([]map[string]interface{}, 0, len(flows))
			for _, f := range flows {
				if prefix != "" && !strings.HasPrefix(f.ID, prefix) {
					continue
				}
				if len(out) >= limit {
					break
				}
				out = append(out, map[string]interface{}{"id": f.ID, "name": f.Name, "description": f.Description})
			}
			return out, nil
		case "capabilities":
			// 内核原语（builtin + pure）+ 已注册 ext.* 的方法级参数 schema。
			// 渲染逻辑与 ext.ai.capabilities 共用（capabilityCatalog），两个入口不会各写一份。
			return capabilityCatalog(extCaps), nil
		default:
			return nil, fmt.Errorf("ext.registry 不支持的方法 %q（仅 tools/flows/capabilities）", method)
		}
	})
}

// defaultAISchemas 返回默认 ext.ai（无 extensions.yaml 时的回退）的方法级参数 schema。
// 与 examples/gear-ai-sidecar 的四个稳定原语（complete/catalog/domains/validate）一一对应，
// 保证 capabilities() 在「无配置回退」路径下也能给 agent 暴露 ext.ai 的参数语义。
func defaultAISchemas() map[string]MethodSchema {
	return map[string]MethodSchema{
		"complete": {Description: "LLM 补全，返回 {text} 或 {data}(format=json)", Parameters: map[string]ParamSchema{
			"system":      {Type: "string", Description: "系统提示（可选）"},
			"prompt":      {Type: "string", Description: "用户提示", Required: true},
			"format":      {Type: "string", Description: "text|json（可选，默认 text）"},
			"provider":    {Type: "string", Description: "模型厂商（可选；缺省回退 env GEAR_AI_PROVIDER，再回退内置默认 siliconflow）"},
			"model":       {Type: "string", Description: "模型名（可选；缺省回退 env GEAR_AI_MODEL，再回退厂商默认）"},
			"base_url":    {Type: "string", Description: "API base（可选；缺省回退 env GEAR_AI_BASE_URL，再回退厂商默认）"},
			"api_key":     {Type: "string", Description: "API key（可选；缺省回退 env GEAR_AI_API_KEY。优先写 ${secrets.x} 引用密钥源，别把明文落在 Flow 定义里）"},
			"temperature": {Type: "number", Description: "采样温度（可选）"},
		}},
		"complete_stream": {Description: "complete 的流式版：逐 token 增量输出。REST 经 POST /api/capabilities/invoke/stream 消费 SSE；Lua 经 ctx.ext.ai.complete_stream(args, function(token, reasoning) ...) 回调式消费", Parameters: map[string]ParamSchema{
			"system":      {Type: "string", Description: "系统提示（可选）"},
			"prompt":      {Type: "string", Description: "用户提示", Required: true},
			"format":      {Type: "string", Description: "text|json（可选，默认 text；json 模式增量仍是原始文本，由调用方自行聚合）"},
			"provider":    {Type: "string", Description: "模型厂商（可选）"},
			"model":       {Type: "string", Description: "模型名（可选）"},
			"base_url":    {Type: "string", Description: "API base（可选）"},
			"api_key":     {Type: "string", Description: "API key（可选）"},
			"temperature": {Type: "number", Description: "采样温度（可选）"},
		}},
		"catalog": {Description: "BM25 式能力域检索，返回候选工具列表", Parameters: map[string]ParamSchema{
			"query":  {Type: "string", Description: "查询词", Required: true},
			"domain": {Type: "string", Description: "限定能力域（可选）"},
			"tools":  {Type: "array", Description: "工具目录（来自 ext.registry.tools()）", Required: true},
			"k":      {Type: "number", Description: "Top-K（可选，默认 5）"},
		}},
		"domains": {Description: "按 namespace 归类的能力域概览", Parameters: map[string]ParamSchema{
			"tools": {Type: "array", Description: "工具目录", Required: true},
		}},
		"validate": {Description: "Flow/Tool 静态校验（复用引擎单一真相源）", Parameters: map[string]ParamSchema{
			"kind":  {Type: "string", Description: "flow|tool|hook", Required: true},
			"def":   {Type: "object", Description: "待校验定义", Required: true},
			"tools": {Type: "array", Description: "工具目录（供 call 存在性校验）", Required: true},
		}},
		"generate": {Description: "真正的 agent 编排：生成 flow/tool/hook 草稿（含生成→校验→修正闭环）", Parameters: map[string]ParamSchema{
			"kind":        {Type: "string", Description: "flow|tool|hook", Required: true},
			"intent":      {Type: "string", Description: "用户意图", Required: true},
			"max_retries": {Type: "number", Description: "最大重试次数（可选，默认配方值）"},
		}},
		"capabilities": {Description: "返回内核原语（builtin + pure 两类）与已注册 ext.* 的方法级参数 schema（锚定 ctx.* 语义）", Parameters: map[string]ParamSchema{}},
		"react": {Description: "ReAct 智能体：LLM 逐步调用内核原语（builtin + pure）与已注册 ext.* 完成任务，返回 {answer,steps,used_tools}", Parameters: map[string]ParamSchema{
			"task":        {Type: "string", Description: "任务描述", Required: true},
			"max_steps":   {Type: "number", Description: "最大步数（可选，默认 6）"},
			"allow":       {Type: "array", Description: "限制可用工具（可选）。项可以是能力名（http / ext.redis，该能力全部方法）或方法名（ext.redis.set，只开这一个）；不给则用默认集（通用能力，不是全目录）；给 \"*\" 表示不设限。工具数超上限会报错，请用方法级 allow 收窄"},
			"provider":    {Type: "string", Description: "模型厂商（可选；缺省回退 env GEAR_AI_PROVIDER，再回退内置默认 siliconflow）"},
			"model":       {Type: "string", Description: "模型名（可选；缺省回退 env GEAR_AI_MODEL，再回退厂商默认）"},
			"base_url":    {Type: "string", Description: "API base（可选；缺省回退 env GEAR_AI_BASE_URL，再回退厂商默认）"},
			"api_key":     {Type: "string", Description: "API key（可选；缺省回退 env GEAR_AI_API_KEY。优先写 ${secrets.x} 引用密钥源，别把明文落在 Flow 定义里）"},
			"temperature": {Type: "number", Description: "采样温度（可选）"},
		}},
	}
}

func orSchema(m map[string]interface{}) map[string]interface{} {
	if len(m) == 0 {
		return map[string]interface{}{"type": "object"}
	}
	return m
}

// ---- 配置化扩展加载：取代原先「启动处写死 ext.ai」 ----

// ParamSchema 描述某个方法参数的类型/语义（喂给 agent 的 capabilities() 目录，PRD §9.3 最小上下文的扩展）。
type ParamSchema struct {
	Type        string   `yaml:"type" json:"type"` // string/object/array/number/bool
	Description string   `yaml:"description" json:"description,omitempty"`
	Enum        []string `yaml:"enum" json:"enum,omitempty"`         // 枚举约束（可选）
	Required    bool     `yaml:"required" json:"required,omitempty"` // 是否必填（可选）
}

// MethodSchema 描述扩展原语某个方法的参数结构，agent 生成 tool 的 entry 时据此知道 ctx.ext.<x>.<method> 吃什么字段。
type MethodSchema struct {
	Description string                 `yaml:"description" json:"description,omitempty"`
	Parameters  map[string]ParamSchema `yaml:"parameters" json:"parameters,omitempty"` // 参数名 -> schema
}

// ExtSpec 是 extensions.yaml 中一个扩展原语的配置。
//
// 字段演进（PRD §9 + 用户决策 2026-09-15）：
//   - cmd/methods：原 sidecar 载体（任意语言小程序，stdin/stdout JSON 协议）。
//   - kind：扩展载体类型，"sidecar"（默认，任意语言子进程）或 "agent"（进程内 Go 承载，
//     经引擎依赖注入；ext.ai 改造后用）。
//   - schemas：方法级参数 schema，**必填**（用户决策：所有 ext.* 必须声明，打通 capabilities()
//     能力目录，让 agent 自我锚定参数语义，堵住「tool 模式 entry 幻觉 ctx.* 参数」的缺口）。
type ExtSpec struct {
	Cmd     []string                `yaml:"cmd"`     // sidecar 启动命令（kind=sidecar 时必填）
	Methods []string                `yaml:"methods"` // 暴露给 ctx.ext.<name> 的方法
	Kind    string                  `yaml:"kind"`    // "" | "sidecar" | "agent"
	Schemas map[string]MethodSchema `yaml:"schemas"` // 方法名 -> 参数 schema（必填）
	// 以下三字段为 service ext（常驻能力）扩展（EXT_SERVICE_DESIGN.md §3/§4/§12）：
	Service     bool               `yaml:"service"`      // 声明为常驻能力（走 process Tool + Supervisor 监管）
	Listen      string             `yaml:"listen"`       // rpc 本地监听地址（127.0.0.1:port）；省略则纯 daemon 守护
	HealthProbe *model.HealthProbe `yaml:"health_probe"` // service 健康检查探针（nil = 不探）
	// Source 声明该 ext 承载的「事件源名」（通用订阅原语 ctx.subscribe 用，首批 mqtt）。
	// 非空且 Listen 非空时，SetupExtensions 会把对应源工厂登记进 CapabilityRegistry.Sources。
	Source string `yaml:"source"`
}

// builtinCapabilitySchemas 是**内核原语**的「方法级参数 schema」硬编码单一来源。
// 含两类（分类原则见 ATOM_ROADMAP.md §0）：
//   - 受 capability 门禁的核心原语（http/call/db/secret/event/subscribe，即 cap.KnownPrimitives）；
//   - 纯函数核心原子（time/template/crypto，见 purefunc.go，始终可用、不受门禁）。
//
// 它同时是「什么算合法的内核原语方法」的判定依据：checkCapabilityMethod / validatePrimitiveArgs
// / checkCallTargets / 能力目录（capabilityCatalog）全部取这一份，方法新增或改名只需改这里
// （纯函数原子的 schema 与实现同文件，Lua 绑定还由 schema 的键自动生成，三处不会走散）。
// 供 ext.registry.capabilities() 返回给 agent——弥补 §9.3 只给 tool 级 schema、不给 capability 级
// 参数的缺口（用户决策选项2）。方法名对应 ctx.<name>.<method> 的调用形态
// （见 internal/engine/lua.go:127-357、internal/cap/cap.go）。
var builtinCapabilitySchemas = map[string]map[string]MethodSchema{
	"http": {
		"Request": {Description: "发起任意 HTTP 请求，返回 {status,body,headers}", Parameters: map[string]ParamSchema{
			"method":  {Type: "string", Description: "HTTP 方法 GET/POST/PUT/DELETE...", Required: true},
			"url":     {Type: "string", Description: "请求 URL", Required: true},
			"body":    {Type: "string", Description: "请求体（JSON 字符串，可选）"},
			"headers": {Type: "object", Description: "请求头 map（可选）"},
		}},
	},
	"call": {
		"Call": {Description: "调用另一个 Tool/Flow（插件间唯一通信通道），返回结果 map", Parameters: map[string]ParamSchema{
			"target": {Type: "string", Description: "目标 Tool 或 Flow 的 id", Required: true},
			"input":  {Type: "object", Description: "传入目标的参数 map（可选）"},
		}},
	},
	"db": {
		"query": {Description: "参数化查询，返回 []map（行集合）", Parameters: map[string]ParamSchema{
			"driver": {Type: "string", Enum: []string{"sqlite", "mysql", "postgres"}, Description: "数据库驱动（缺省 sqlite；别名 mysql/mariadb、postgres/postgresql/pg）"},
			"dsn":    {Type: "string", Description: "连接串（sqlite 文件或内存 DSN；mysql/postgres 必填，含口令优先 ${secrets.*}）"},
			"sql":    {Type: "string", Description: "SQL（用 ? 占位符，配套 params）", Required: true},
			"params": {Type: "array", Description: "预编译占位符参数（按顺序，可选）"},
		}},
		"exec": {Description: "执行写操作（INSERT/UPDATE/DELETE/DDL），成功返回 nil、失败经 err 表达", Parameters: map[string]ParamSchema{
			"driver": {Type: "string", Enum: []string{"sqlite", "mysql", "postgres"}, Description: "数据库驱动（缺省 sqlite）"},
			"dsn":    {Type: "string", Description: "连接串（mysql/postgres 必填，含口令优先 ${secrets.*}）"},
			"sql":    {Type: "string", Description: "SQL（用 ? 占位符，配套 params）", Required: true},
			"params": {Type: "array", Description: "预编译占位符参数（按顺序，可选）"},
		}},
		"batch": {Description: "事务批处理：多条语句同事务顺序执行，**全或无**（任一条失败整体回滚，committed=false，results 指明第几条、什么错）。DDL 可与 DML 混批（SQLite 同事务内同原子；其它方言见 atoms.md）", Parameters: map[string]ParamSchema{
			"driver":     {Type: "string", Enum: []string{"sqlite", "mysql", "postgres"}, Description: "数据库驱动（缺省 sqlite）"},
			"dsn":        {Type: "string", Description: "连接串（mysql/postgres 必填，含口令优先 ${secrets.*}）"},
			"statements": {Type: "array", Description: "语句数组，至少一条；每个元素是 {sql, params?} 表或裸 SQL 字符串。sql 为该条语句（用 ? 占位符），params 为该条自己的参数（可选）", Required: true},
		}},
		"ping": {Description: "探活（sqlite 文件可打开 / mysql/postgres 网络连得通即 ok）", Parameters: map[string]ParamSchema{
			"driver": {Type: "string", Enum: []string{"sqlite", "mysql", "postgres"}, Description: "数据库驱动（缺省 sqlite）"},
			"dsn":    {Type: "string", Description: "连接串（mysql/postgres 必填）"},
		}},
	},
	"secret": {
		"get": {Description: "只读取密钥（看不到存储位置），返回明文", Parameters: map[string]ParamSchema{
			"name": {Type: "string", Description: "密钥名（如 TOKEN）", Required: true},
		}},
	},
	"event": {
		"notify": {Description: "广播一个事件负载（尽力通知）", Parameters: map[string]ParamSchema{
			"name":    {Type: "string", Description: "事件名", Required: true},
			"payload": {Type: "object", Description: "负载（任意，可选）"},
		}},
		"wait": {Description: "等待某事件下一次通知，返回 payload", Parameters: map[string]ParamSchema{
			"name": {Type: "string", Description: "事件名", Required: true},
		}},
	},
	"subscribe": {
		"start": {Description: "建立订阅（source=事件源，config=源配置，callback=投递动作），返回 handle", Parameters: map[string]ParamSchema{
			"source":   {Type: "string", Description: "事件源（首批 mqtt）", Required: true},
			"config":   {Type: "object", Description: "事件源配置（mqtt: {topic, qos}）", Required: true},
			"callback": {Type: "object", Description: "投递动作 {type, target, input}", Required: true},
		}},
		"stop": {Description: "取消订阅（按 handle）", Parameters: map[string]ParamSchema{
			"handle": {Type: "string", Description: "订阅句柄", Required: true},
		}},
	},
	"config": {
		"get": {Description: "读取一项持久化配置（应用级 key/value，落点在 gearhome 的 config.db）；不存在返回错误", Parameters: map[string]ParamSchema{
			"key": {Type: "string", Description: "配置键（如 ai.api_key、app.mindstorm.theme；不带前缀即全局）", Required: true},
		}},
		"set": {Description: "写入一项持久化配置（已存在则覆盖）。传空串 = 存成空值，要清除请用 delete", Parameters: map[string]ParamSchema{
			"key":   {Type: "string", Description: "配置键", Required: true},
			"value": {Type: "string", Description: "配置值", Required: true},
		}},
		"delete": {Description: "删除一项持久化配置；不存在时返回错误", Parameters: map[string]ParamSchema{
			"key": {Type: "string", Description: "配置键", Required: true},
		}},
		"list": {Description: "列出某前缀下的全部配置（prefix 为空串 = 全部），返回 {key: value} map", Parameters: map[string]ParamSchema{
			"prefix": {Type: "string", Description: "键前缀（如 ai. / app.mindstorm.；可空）"},
		}},
	},
	// 纯函数核心原子（内核内建、始终可用、不受 capability 门禁）：schema 与实现同文件
	// （purefunc.go），方法新增/改名时 schema、Lua 绑定、静态校验、能力目录一起动。
	"time":     pureTimeSchemas,
	"template": pureTemplateSchemas,
	"crypto":   pureCryptoSchemas,
	"id":       pureIDSchemas,
	"validate": pureValidateSchemas,
}

// builtinCapabilitySummaries 返回内核原语的能力摘要（门禁：kind=builtin；纯函数：kind=pure），
// 顺序取 allBuiltinPrimitives（单一来源）。
func builtinCapabilitySummaries() []CapabilitySummary {
	out := make([]CapabilitySummary, 0, len(gatedPrimitiveOrder)+len(purePrimitiveOrder))
	for _, p := range allBuiltinPrimitives() {
		ms, ok := builtinCapabilitySchemas[p]
		if !ok {
			continue
		}
		out = append(out, CapabilitySummary{Name: p, Kind: primitiveKind(p), Methods: ms})
	}
	return out
}

// capabilityCatalog 渲染「内核原语 + 已注册 ext.*」的完整能力目录（JSON 友好形态），
// 供 ext.registry.capabilities（Lua 侧自我锚定）与 ext.ai.capabilities（REST `GET /api/capabilities`
// 与 MCP `capabilities` 工具）共用——两个入口的目录**永远同源**：此前各自维护一份
// 「http/call/db/secret/event」字面量列表，于是 subscribe 被漏在目录外、
// 纯函数原子无处登记（两份副本必然漂移）。
//
// ext 名排序输出：能力目录会被拼进提示词，必须可复现（否则提示词与测试随 map 迭代顺序抖动）。
func capabilityCatalog(extCaps map[string]map[string]MethodSchema) []map[string]interface{} {
	summaries := builtinCapabilitySummaries()
	names := make([]string, 0, len(extCaps))
	for name := range extCaps {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		summaries = append(summaries, CapabilitySummary{Name: name, Kind: "ext", Methods: extCaps[name]})
	}
	out := make([]map[string]interface{}, 0, len(summaries))
	for _, s := range summaries {
		out = append(out, map[string]interface{}{
			"name":    s.Name,
			"kind":    s.Kind,
			"methods": s.Methods,
		})
	}
	return out
}

// CapabilitySummary 是 capabilities() 返回的单条能力摘要（内置或扩展）。
type CapabilitySummary struct {
	Name    string                  `json:"name"`    // 能力名：http / ext.db / ext.ai ...
	Kind    string                  `json:"kind"`    // builtin | ext
	Methods map[string]MethodSchema `json:"methods"` // 方法名 -> 参数 schema
}

// LoadExtensionConfig 读 extensions.yaml，并把每个 ext 的 `cmd[0]` 相对路径解析成绝对路径。
//
// path 为空时按 ResolveExtensionConfigPath 的候选序列定位（可执行文件同目录优先，cwd 兜底；
// GEAR_EXT_CONFIG 可显式覆盖）。文件不存在返回空 map（调用方据此回退默认 ext.ai），
// 解析失败才返回 error。
//
// cmd 的解析基准是**配置文件所在目录**（见 extensionCmdRoots）——配置里写
// `plugin/serve-sidecar.exe` 就相对配置文件解析，于是 extensions.yaml 与 plugin/ 可以整体
// 搬家而不改一个字，也不再随 cwd 漂移。
func LoadExtensionConfig(path string) (map[string]ExtSpec, error) {
	if path == "" {
		p, err := ResolveExtensionConfigPath()
		if err != nil {
			return nil, err
		}
		path = p
	}
	if path == "" {
		return map[string]ExtSpec{}, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]ExtSpec{}, nil
		}
		return nil, err
	}
	var specs map[string]ExtSpec
	if err := yaml.Unmarshal(b, &specs); err != nil {
		return nil, fmt.Errorf("解析 %s 失败: %w", path, err)
	}
	if specs == nil {
		specs = map[string]ExtSpec{}
	}
	roots := extensionCmdRoots(path)
	for name, spec := range specs {
		spec.Cmd = resolveExtensionCmd(spec.Cmd, roots)
		specs[name] = spec
	}
	return specs, nil
}

// SetupExtensions 把「配置化扩展」+ 内建 ext.registry 注册进目录：
//  1. 始终注册 ext.registry（进程内，读引擎 registry，agent 自我锚定用）。
//  2. 若 extensions.yaml 存在且非空，逐个注册 sidecar 扩展；否则回退默认 ext.ai
//     （DefaultAISidecarCmd，行为与重构前一致）。
//
// 这样「发布后加 ext.tls」只需在 extensions.yaml 加一段，不必改 gear 源码——
// 取代原先写死在 cli/engine.go 与 server/server.go 的 Register("ext.ai", ...)。
// fail-closed：配置非法（YAML 错 / cmd 为空 / 扩展名非 ext. 开头）返回 error，调用方应中断启动。
// defaultRecipeDir 是「可编辑配方」的**可选**磁盘覆盖目录：存在同名 YAML 时优先于内嵌默认，
// 不存在则自动回退内嵌默认（internal/engine/recipes/{flow,tool,hook}.yaml）。
// 想调生成细节（提示词/约束）时，在 examples/ai/recipes/{flow,tool,hook}.yaml 放同名文件即可
// （目录按需创建），或用 GEAR_AI_RECIPES 指向自定义目录——都无需重编 gear。
const defaultRecipeDir = "examples/ai/recipes"

// applyExtSchemas 把 extCaps（ext 名 → 方法级 schema）登记进能力目录，供
// checkCapabilityMethod / validatePrimitiveArgs 做**方法级**静态校验
// （红线「能静态校验的不留到运行期」：`ext.kafka.notamethod` 应在注册期就被拒，
// 而不是拖到某次执行、甚至拖到侧车返回错误）。
//
// 单一来源不变：schema 仍只来自 extensions.yaml 的 schemas 字段（或两个内建 ext 的内置 schema），
// SetupExtensions 已对其自洽性（methods 与 schemas 一一对应）做过 fail-closed 校验；
// 目录只做转存，不生成第二份方法清单。
func applyExtSchemas(capReg *CapabilityRegistry, extCaps map[string]map[string]MethodSchema) {
	for name, ms := range extCaps {
		capReg.SetSchemas(name, ms)
	}
}

// SetupExtensions 把「配置化扩展」+ 内建 ext.registry + 进程内 ext.ai 注册进目录：
//  1. 始终注册 ext.registry（进程内，读引擎 registry，agent 自我锚定用）。
//  2. 始终注册 ext.ai（进程内 Eino agent，取代原 sidecar 子进程；用户决策 ext.ai 进程内）。
//     无论有无 extensions.yaml，ext.ai 都走进程内（kind=agent 目前仅支持 ext.ai）。
//  3. 若 extensions.yaml 存在且非空，逐个注册其余 sidecar 扩展（ext.ai 已进程内，跳过）。
//
// 返回构造好的 *AIAgent（供 Engine 持有，CLI/REST 的 generate 直接调用）。
// fail-closed：配置非法（YAML 错 / ext.* 缺 schemas / 扩展名非 ext. 开头）返回 error。
func SetupExtensions(capReg *CapabilityRegistry, reg *registry.Registry, cfgPath string) (*AIAgent, error) {
	// 可编辑配方目录：默认 examples/ai/recipes，可用 GEAR_AI_RECIPES 覆盖（指向任意目录，
	// 其下需有 flow.yaml/tool.yaml/hook.yaml，覆盖内嵌默认；目录不存在则回退内嵌默认）。
	recipeDir := defaultRecipeDir
	if v := os.Getenv("GEAR_AI_RECIPES"); v != "" {
		recipeDir = v
	}
	// 先收集 ext.* 的能力 schema（供 ext.registry.capabilities() 与 ext.ai 暴露给 agent 自我锚定）。
	extCaps := map[string]map[string]MethodSchema{}

	specs, err := LoadExtensionConfig(cfgPath)
	if err != nil {
		return nil, err
	}

	// 无配置文件 → 回退默认 ext.ai（进程内），带内置 schema 供 capabilities() 描述。
	if len(specs) == 0 {
		extCaps["ext.ai"] = defaultAISchemas()
		extCaps["ext.registry"] = registrySchemas()
		applyExtSchemas(capReg, extCaps)
		if err := capReg.Register("ext.registry", RegistryExtension(reg, extCaps)); err != nil {
			return nil, fmt.Errorf("注册 ext.registry 失败: %w", err)
		}
		agent := newAIAgent(reg, DefaultLLM(), DefaultRecipeLoader(recipeDir), extCaps)
		if err := capReg.Register("ext.ai", AIExtensionInProcess(agent)); err != nil {
			return nil, fmt.Errorf("注册 ext.ai 失败: %w", err)
		}
		return agent, nil
	}

	// 严格校验（用户决策 2026-09-15：所有 ext.* 必须声明 schemas 且 methods 与 schemas 一致）：
	// 打通 capability 级参数语义（选项2），堵住「tool 模式 entry 幻觉 ctx.* 参数」的缺口。
	for name, spec := range specs {
		if !strings.HasPrefix(name, "ext.") {
			return nil, fmt.Errorf("扩展名 %q 必须以 ext. 开头", name)
		}
		if name == "ext.ai" {
			// ext.ai 由内核**始终进程内注册**，其 methods/schemas 恒取内置 defaultAISchemas
			// （见下方 extCaps 赋值），配置里的这一段**整段被忽略**——与 extensions.example.yaml
			// 的说明、README「ext.ai 由内核始终注册」的口径一致。
			// 之所以连 schemas 必填校验也跳过：文档说了「会被忽略」，就不该再拿它报错。
			continue
		}
		if len(spec.Schemas) == 0 {
			return nil, fmt.Errorf("扩展 %q 必须声明 schemas（每个方法的参数 schema，供 agent capabilities() 锚定）；缺失请补上", name)
		}
		for _, m := range spec.Methods {
			if _, ok := spec.Schemas[m]; !ok {
				return nil, fmt.Errorf("扩展 %q 的方法 %q 缺少对应的 schemas 条目（methods 与 schemas 必须一一对应）", name, m)
			}
		}
		extCaps[name] = spec.Schemas
	}

	// ext.ai 与 ext.registry 由内核**始终注册**（与 extensions.yaml 无关），故它们的 schema
	// 也必须始终在目录里——否则这两个内建 ext 的方法会退化成「只校验能力名」。
	// ext.ai 的 schema 恒取内置 defaultAISchemas（与 AIExtensionInProcess 的方法清单同源；
	// extensions.example.yaml 已注明「配置里的 ext.ai 段会被忽略」，此处即该口径的落点）。
	extCaps["ext.ai"] = defaultAISchemas()
	if _, ok := extCaps["ext.registry"]; !ok {
		extCaps["ext.registry"] = registrySchemas()
	}
	applyExtSchemas(capReg, extCaps)

	if err := capReg.Register("ext.registry", RegistryExtension(reg, extCaps)); err != nil {
		return nil, fmt.Errorf("注册 ext.registry 失败: %w", err)
	}

	// ext.ai 始终进程内注册（用户决策：ext.ai 进程内 Eino agent）。
	agent := newAIAgent(reg, DefaultLLM(), DefaultRecipeLoader(recipeDir), extCaps)
	if err := capReg.Register("ext.ai", AIExtensionInProcess(agent)); err != nil {
		return nil, fmt.Errorf("注册 ext.ai 失败: %w", err)
	}

	// 其余 ext.* ：sidecar 子进程（kind=agent 目前仅 ext.ai 支持）。
	for name, spec := range specs {
		if name == "ext.ai" {
			continue // 已进程内注册
		}
		if spec.Kind == "agent" {
			return nil, fmt.Errorf("kind=agent 目前仅支持 ext.ai，%q 暂未实现进程内引擎", name)
		}
		if len(spec.Cmd) == 0 {
			return nil, fmt.Errorf("扩展 %q 的 cmd 不能为空（sidecar 载体需要启动命令）", name)
		}
		if spec.Service {
			// service ext：自动包成一个虚拟 process Tool（service 路径载体），并登记协调客户端。
			// 这样 flow 的 service step 引用 ext.<name> 时走 processRuntime.Start + Supervisor
			// 监管（复用现有 service 闭环），同时同主机多 gear 经 coord 共享单个 sidecar 实例。
			// prepareServiceExt 把 `listen` 落到侧车进程环境（COORD_ADDR）并收敛探针地址，
			// 令 extensions.yaml 成为协调地址的唯一真相源（详见该函数注释）。
			entry, probe, err := prepareServiceExt(spec)
			if err != nil {
				return nil, fmt.Errorf("service ext %q 配置非法: %w", name, err)
			}
			vt := &model.Tool{
				ID:           name,
				Runtime:      model.RuntimeProcess,
				Entry:        entry,
				InputSchema:  map[string]interface{}{"type": "object"},
				OutputSchema: map[string]interface{}{"type": "object"},
				Versions:     []model.ToolVersion{{Version: "1.0.0", State: model.StateStable}},
				HealthProbe:  probe,
			}
			if err := reg.UpsertTool(vt); err != nil {
				return nil, fmt.Errorf("注册 service ext %q 的虚拟 Tool 失败: %w", name, err)
			}
			ttl := 15 * time.Second
			cli, err := coord.NewClient(spec.Listen, ttl)
			if err != nil {
				return nil, fmt.Errorf("创建 service ext %q 协调客户端失败: %w", name, err)
			}
			capReg.SvcExt[name] = cli

			// 通用订阅事件源：source 声明了事件源名且 listen 存在时，把对应 sidecar 的
			// subscribe/unsubscribe RPC 包成 SourceFactory 登记进目录（mqtt/rabbitmq/kafka/webhook 统一）。
			if spec.Source != "" && spec.Listen != "" {
				sf, err := newSourceFactory(spec.Source, NewLocalHTTPTransport(spec.Listen))
				if err != nil {
					return nil, fmt.Errorf("注册 ext %q 的事件源 %q 失败: %w", name, spec.Source, err)
				}
				capReg.Sources[spec.Source] = sf
			}

			// 能力调用路径：listen 存在走本地 HTTP（复用长连接），否则回退子进程（once 兜底）。
			var t SidecarTransport
			if spec.Listen != "" {
				t = NewLocalHTTPTransport(spec.Listen)
			} else {
				t = &CommandTransport{cmd: spec.Cmd}
			}
			if err := capReg.Register(name, sidecarExtension(name, t, spec.Methods)); err != nil {
				return nil, fmt.Errorf("注册 service ext %q 失败: %w", name, err)
			}
			continue
		}
		if err := capReg.Register(name, SidecarExtension(name, spec.Cmd, spec.Methods)); err != nil {
			return nil, fmt.Errorf("注册扩展 %q 失败: %w", name, err)
		}
	}
	return agent, nil
}

// ───────── service ext：协调地址的唯一真相源 ─────────

// prepareServiceExt 为 service ext 派生「虚拟 process Tool」的两个字段，落实一条约定：
// **extensions.yaml 的 `listen` 是该 ext 协调/RPC 地址的唯一真相源**。
//
// 背景（改一处即静默失联的双份定义）：侧车自己读 COORD_ADDR 决定 bind 哪个端口，
// 而引擎用 spec.Listen 去连；两侧历史上各写一份相同字面量（yaml listen ↔ 侧车
// os.Getenv 的默认值），改任意一处就会变成「引擎连 A、侧车听 B」——进程都活着，
// 连不上也不报错，只在真正调用时超时。
//
// 对策两件：
//  1. Entry：把 listen 以 `COORD_ADDR=<listen>` 前置注入侧车进程环境；listen 的值与
//     cmd 里需要转义的 token 一律经单引号包裹（见 shellQuoteValue / quoteCmdToken）——
//     entry 是一条以空格分词的命令行，绝对路径可能含空格，不转义就会被拆成两个参数。
//     侧车那侧的 os.Getenv 默认值随即降级为「不经引擎、手工单跑侧车」时的兜底。
//  2. HealthProbe：tcp/http 探针的 addr 省略时即取 listen（侧车静态可知的监听地址
//     只有它）；显式写了却与 listen 不一致则**注册期报错**——探错地址的后果是监管
//     无限重启，属「能静态校验就不许留到运行期」的红线范围，不静默放过。
//
// listen 为空（纯 daemon 守护，引擎不主动调它）时不注入、不校验，Entry 原样返回 cmd。
func prepareServiceExt(spec ExtSpec) (string, *model.HealthProbe, error) {
	parts := make([]string, 0, len(spec.Cmd))
	for _, c := range spec.Cmd {
		parts = append(parts, quoteCmdToken(c))
	}
	entry := strings.Join(parts, " ")
	if spec.Listen == "" {
		return entry, spec.HealthProbe, nil
	}
	probe, err := serviceExtProbe(spec)
	if err != nil {
		return "", nil, err
	}
	return "COORD_ADDR=" + shellQuoteValue(spec.Listen) + " " + entry, probe, nil
}

// serviceExtProbe 收敛 service ext 的健康探针地址：省略即取 listen，显式不一致即报错。
func serviceExtProbe(spec ExtSpec) (*model.HealthProbe, error) {
	p := spec.HealthProbe
	if p == nil {
		return nil, nil
	}
	// cmd 探测（跑命令判活）与地址无关，原样保留。
	if p.Kind != model.ProbeKindHTTP && p.Kind != model.ProbeKindTCP {
		return p, nil
	}
	if p.Addr == "" {
		cp := *p // 复制一份再填 addr：不就地改写配置对象
		cp.Addr = spec.Listen
		return &cp, nil
	}
	if coordAddrKey(p.Addr) != coordAddrKey(spec.Listen) {
		return nil, fmt.Errorf("health_probe.addr=%q 与 listen=%q 不一致：探针只可能探侧车的协调端口，"+
			"两处写不同地址会让监管反复判死并无限重启；请删掉 addr（自动取 listen）或改为一致",
			p.Addr, spec.Listen)
	}
	return p, nil
}

// coordAddrKey 把 host:port 归一成可比较的键（localhost / ::1 与 127.0.0.1 视为同一目标），
// 避免「写法不同但等价」被误判为不一致。无法解析时退回小写字面量（交给 Resolve 去报错）。
func coordAddrKey(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return strings.ToLower(addr)
	}
	switch strings.ToLower(host) {
	case "", "localhost", "127.0.0.1", "::1":
		host = "127.0.0.1"
	}
	return host + ":" + port
}

// shellQuoteValue 用单引号包裹值，使其在 `sh -c` 与 gear 原生解析（cmdline.Parse）下
// 都保持字面量语义（值来自配置文件，属准可信输入，但不转义就等于把解析权交了出去）。
func shellQuoteValue(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// cmdTokenSafe 是「不需要加引号」的字符集（POSIX 的安全字符 + 常见路径字符）。
var cmdTokenSafe = func() [128]bool {
	var t [128]bool
	for _, r := range "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_@%+=:,./-" {
		t[r] = true
	}
	return t
}()

// quoteCmdToken 只在必要时给 cmd token 加单引号。
//
// 为什么必须做：cmd 里的路径被 resolveExtensionCmd 解析成绝对路径后**可能含空格**
// （`C:\Program Files\...`），而 entry 是一条「以空格分词的命令行」——不转义就会被拆成
// 两个参数。历史实现直接 strings.Join，等于默认了「路径永远不含空格」这个不成立的假设。
func quoteCmdToken(s string) string {
	if s == "" {
		return "''"
	}
	for _, r := range s {
		if r < 128 && cmdTokenSafe[r] {
			continue
		}
		return shellQuoteValue(s)
	}
	return s
}
