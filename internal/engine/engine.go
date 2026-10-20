// Package engine 实现 Gear 内核引擎（S1：顺序执行 + process runtime + schema 校验 + vars 传递）。
package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v5"

	"gear/internal/cap"
	"gear/internal/gearhome"
	"gear/internal/gearlog"
	"gear/internal/model"
	"gear/internal/registry"
)

// DefaultMaxParallel 是单个 Flow 内**同时执行**的 step 数上限（S4 并发闸门/背压）。
// 防止高扇出 Flow 一次性 fork 出成百上千个进程把机器打爆（PRD §14.5）。
const DefaultMaxParallel = 4

// Lua 沙箱策略（REQ-LUA-SANDBOX-CONFIG）。「拦截哪些危险行为」由策略决定，用户自抉：
//   - SandboxStrict（默认）：只开 base/string/table/math，禁 dofile/loadfile/require/module
//     （纯计算，无法接触宿主；M5 的 ctx.* 原语仍按 Capabilities 最小权限放行）。
//   - SandboxPermissive：在 strict 基础上额外开放 coroutine/utf8（纯计算安全），仍禁文件加载逃逸。
//     不提供 none/unsafe：Lua 逃逸宿主是 P0 级风险，配置项不应能关掉沙箱本身。
const (
	SandboxStrict     = "strict"
	SandboxPermissive = "permissive"
)

// Engine 持有 registry，负责把 Flow 跑起来。
type Engine struct {
	reg         *registry.Registry
	maxParallel int
	// sandboxPolicy 是 Lua 沙箱档位（strict/permissive），经 WithSandbox 设置。
	sandboxPolicy string
	// host 是宿主能力原语的实现集合（M5，PRD §6）。nil 表示不提供任何宿主能力
	// （Tool 若声明了 capability，运行期访问会收到 ErrCapabilityUnavailable）。
	host *cap.Providers
	// capReg 是扩展原语目录（用户/插件注册的 ctx.<name>）。nil 时 New 会用空目录兜底
	// （仅内核原语可用，行为与历史一致）。
	capReg *CapabilityRegistry
	// ai 是进程内 ext.ai agent（generate/complete/catalog/...）；由 SetupExtensions 注入。
	// nil 表示未装配 ext.ai（声明了 ext.ai 的 Tool 运行期收到 ErrCapabilityUnavailable）。
	ai *AIAgent
	// subscriber 是通用订阅管理器（ctx.subscribe 原语实现）。New 时装配并写入 host.Subscribe；
	// serve 监听时经 SetSubscribeCallbackBase 注入回推基址。nil 表示未装配（不会发生，
	// New 恒创建；仅防御性判空）。
	subscriber *Subscriber
	// logSink 是执行日志采集器（REQ-API-LOGS / #71）；nil 表示不记日志。
	logSink LogSink
	// secret 是宿主注入的运行期密钥源（WithSecret）；nil 表示用 host 自带（默认 nil →
	// 声明 secret 的 Tool 收到 ErrCapabilityUnavailable，fail-closed）。
	secret cap.Secret
	// config 是宿主注入的键值配置库（WithConfigStore），落点由装配方决定
	// （生产应为 gearhome.ConfigDBPath()）。nil = 不提供 config 原语：声明了 config 的
	// Tool 运行期收到 ErrCapabilityUnavailable（fail-closed，不静默退化成「内存里存一下」），
	// 且 AI 缺省配置不生效（回退链退化为 env + 内置默认）。
	config cap.Config
	// progressOut 是执行进度（step 通过/跳过/service 启动）的输出目标；nil = os.Stdout。
	//
	// 为什么可替换：MCP 的 **stdio transport 下 stdout 只能承载协议帧**——一行 `✓ step …`
	// 就足以让严格客户端解析失败（`plugin/mcp_wire_probe.py` 抓到的真实事故）。
	// 默认仍是 os.Stdout（CLI 直接看得到），由传输层（`gear mcp`）按需改指 stderr。
	progressOut io.Writer
	// scope 是本次进程归属的应用命名空间（`--scope`，IDENTITY_CONTEXT_DESIGN.md §4.3）。
	//
	// **空 = 不启用守卫**，行为与未加本特性时逐字一致——这是刻意的默认值：现有定义都是
	// 「先存在、后挂 App」（`apps/mindstorm` 的 22 个 Tool 与 apps 表是两条独立路径），
	// 默认开会让单机版立刻跑不起来。开了才 fail-closed。
	//
	// 它是**进程级**而非按请求的：`gear serve` 的引擎是长驻共享实例，而 `cap.Resolver`
	// 的签名里没有 context，按请求切换 scope 在那个面上做不到（硬要做就得改解析器契约）。
	// 这与推荐拓扑一致——每租户一个 gear 进程 + 独立 GEAR_HOME，一个进程只跑一个应用。
	// 将来若真要「一进程多应用」，改的方向是把它迁到 `cap.Caller.AppID` 而非补第二个字段
	// （那个字段已经在 ① 的身份链路里，只是水位为零）。
	scope string
	// openURLs 收集「打开地址」：Flow 收尾步（如 web_ui.done）在其 output 里带回 url / open_urls，
	// 引擎在跑 once 步时顺手收集，供 REST 监管（POST /api/apps）与进程内 app run 复用——
	// 与 CLI `app start` 走独立子进程日志扫描（GEAR_OPEN_URL=）是两条互补通道，互不冲突。
	// key = flowID（同一 Flow 多次受管共享同一地址集合，属可接受的边界）。
	openURLMu sync.Mutex
	openURLs  map[string][]string
}

// New 构造引擎。可用 WithMaxParallel / WithHost / WithSandbox / WithCapabilities 等选项覆盖默认行为。
func New(reg *registry.Registry, opts ...func(*Engine)) *Engine {
	e := &Engine{reg: reg, maxParallel: DefaultMaxParallel, sandboxPolicy: SandboxStrict}
	for _, o := range opts {
		o(e)
	}
	// 默认宿主装配（M5 收口 / #69）：未显式 WithHost 时注入 KV/HTTP/Event/DB 原语，
	// 使声明了对应 capability 的 Tool 在生产环境真正可用（不再恒返回 ErrCapabilityUnavailable）。
	// 显式 WithHost 时尊重调用方配置，但仍补注入 call 解析器（除非调用方已自带）。
	if e.host == nil {
		e.host = defaultProviders()
	}
	// 密钥源注入（REQ-API-SECRETS）：WithSecret 提供的密钥源覆盖默认 nil；
	// 调用方已显式 WithHost 自带 Secret 时尊重调用方配置（不静默覆盖）。
	if e.secret != nil && e.host.Secret == nil {
		e.host.Secret = e.secret
	}
	// 配置库注入（config 原语）：同上口径，显式 WithHost 自带的 Config 优先。
	if e.config != nil && e.host.Config == nil {
		e.host.Config = e.config
	}
	// 默认扩展目录（无扩展能力时仅内核原语可用，行为与历史一致）。
	if e.capReg == nil {
		e.capReg = NewCapabilityRegistry()
	}
	if e.host.Resolver == nil {
		e.host.Resolver = e.newResolver()
	}
	// 通用订阅管理器（ctx.subscribe）：装配并注入 host.Subscribe（与 Event 平级）。
	// serve 监听时由 server.SetSubscribeCallbackBase 再注入回推基址。
	if e.host.Subscribe == nil {
		e.subscriber = newSubscriber(e)
		e.host.Subscribe = e.subscriber
	}
	e.bindAIAgent()
	return e
}

// SetProgressWriter 指定执行进度的输出目标（nil 表示回到默认 os.Stdout）。
//
// 传输层专用：`gear mcp` 必须指向 os.Stderr —— MCP 的 stdout 是协议专用通道。
// 默认值在**调用时**解析（而非构造时），所以测试里临时替换 os.Stdout 的老办法照旧有效。
func (e *Engine) SetProgressWriter(w io.Writer) { e.progressOut = w }

// progressf 输出一行执行进度（目标见 SetProgressWriter）。
func (e *Engine) progressf(format string, args ...any) {
	w := e.progressOut
	if w == nil {
		w = os.Stdout
	}
	fmt.Fprintf(w, format, args...)
}

// bindAIAgent 把引擎的工具执行器注入进程内 ext.ai agent（react 方法需要它来跑单步工具调用），
// 并把 Flow 静态校验指向引擎自身实现——使 agent 的「生成 → 校验」与 CLI/REST/MCP 的注册校验
// 共用同一份规则，不产生第二套判定（PRD §9.2 单一真相源）。
// 引擎未装配 ext.ai（e.ai 为 nil）时静默跳过。
func (e *Engine) bindAIAgent() {
	if e.ai == nil {
		return
	}
	e.ai.exec = func(ctx context.Context, entry string, input map[string]interface{}) (map[string]interface{}, error) {
		host := e.newReactHost()
		rt, err := runtimeForCaps(model.RuntimeLua, e.sandboxPolicy, e.capReg)
		if err != nil {
			return nil, err
		}
		return rt.Run(ctx, entry, input, host)
	}
	e.ai.validateFlow = e.ValidateFlowStatic
	// AI 缺省配置：宿主装配了 config 原语时，让它成为 ext.ai 回退链的第三层
	// （调用参数 > 环境变量 > 配置库 > 内置默认），使 `gear config set ai.api_key ...`
	// 配一次即在所有入口生效——不必再给每个进程 export GEAR_AI_*。
	// 未装配 config 时保持原行为（MockLLM 也不受影响）。
	if e.host != nil && e.host.Config != nil {
		e.ai.SetConfigStore(e.host.Config)
	}
}

// newReactHost 为 react 构造「声明全部 capability」的宿主视图：内核原语（builtin + pure）+ 已注册 ext.* 全部放行，
// 并注入各 ext 的原生实现——使 react 能复用与真实 Tool 完全相同的 ctx.* 通道使用任何内置工具。
// react 是引擎受信的内置 agent，故可全量声明；其执行在 Lua 沙箱内，不会超出宿主能力边界。
func (e *Engine) newReactHost() *cap.Context {
	grants := cap.Grants{}
	for p := range cap.KnownPrimitives {
		grants[p] = true
	}
	e.capReg.mu.Lock()
	for name := range e.capReg.exts {
		grants[cap.Primitive(name)] = true
	}
	e.capReg.mu.Unlock()
	c := cap.New(grants, e.host)
	e.capReg.mu.Lock()
	for name, ext := range e.capReg.exts {
		if v, err := ext.Provider(c); err == nil {
			c.SetExtension(name, v)
		}
	}
	e.capReg.mu.Unlock()
	return c
}

// InvokeCapability 直接执行一个原子能力的方法（供 REST POST /api/capabilities/invoke
// 与前端「能力目录」页的调用测试使用）。复用与 react 完全相同的宿主通道：
// newReactHost 全量授权 + Lua 沙箱，内置原语按位置参数、ext.* 按单 table 调用。
//
// args 是「以参数名为键」的 map（与 capabilities() 的 schema 对齐）：内置原语经
// builtinParamOrder 重排为位置参数，ext.* 原样作为单具名 table 传入；缺省的可选参数
// 自动补 nil。调用前按方法级参数 schema 校验 args（缺必填 / 未知参数一律拒绝）——
// 与 Flow 直调原子能力共用 validatePrimitiveArgs，两个入口的严格度不会漂移。
func (e *Engine) InvokeCapability(name, method string, args map[string]interface{}) (interface{}, error) {
	if name == "" || method == "" {
		return nil, fmt.Errorf("invoke 需要 name 与 method 均非空")
	}
	if err := e.validatePrimitiveArgs(name, method, args); err != nil {
		return nil, err
	}
	entry, err := buildActionEntry(name, method, args)
	if err != nil {
		return nil, err
	}
	host := e.newReactHost()
	rt, err := runtimeForCaps(model.RuntimeLua, e.sandboxPolicy, e.capReg)
	if err != nil {
		return nil, err
	}
	out, err := rt.Run(context.Background(), entry, map[string]interface{}{}, host)
	if err != nil {
		return nil, fmt.Errorf("调用 %s.%s 失败: %w", name, method, err)
	}
	if r, ok := out["result"]; ok {
		return r, nil
	}
	return out, nil
}

// capDBSeq 为每次默认宿主的内存 SQLite 生成唯一 DSN，避免进程内多个引擎共享同一内存库
// （cache=shared 的命名内存库是按 DSN 共享的，独立 DSN 即独立库）。
var capDBSeq int64

// defaultProviders 构造引擎默认宿主能力集（#69）：HTTP 客户端、Event 总线、
// DB 内存 SQLite（每引擎唯一 DSN，进程级共享、重启即丢，M5 scratch 语义）。
// Secret 默认 nil（无密钥源时声明 secret 的 Tool 收到 ErrCapabilityUnavailable，fail-closed）。
func defaultProviders() *cap.Providers {
	seq := atomic.AddInt64(&capDBSeq, 1)
	dsn := fmt.Sprintf("file:gear_cap_%d?mode=memory&cache=shared", seq)
	// 内置多后端 DB：默认 scratch 内存 SQLite（dsn），mysql/postgres 由调用方显式传 dsn。
	// 第二个参数是「应用给相对库名时的落点根目录」——由 gearhome 决定，与 cwd 无关，
	// 应用因此不必把绝对路径硬编码进 YAML（见 gearhome.AppDataDir 的存在理由）。
	// NewMultiDB 永不返回 nil，故 Providers.DB 用接口承载时不会出现 typed-nil。
	dbCap := cap.NewMultiDB(dsn, gearhome.AppDataDir())
	return &cap.Providers{
		HTTP:  &http.Client{Timeout: 30 * time.Second},
		Event: cap.NewMemEvent(),
		DB:    dbCap,
	}
}

// WithMaxParallel 设置并发上限；n < 1 时取 1（退化为串行执行）。
func WithMaxParallel(n int) func(*Engine) {
	return func(e *Engine) {
		if n < 1 {
			n = 1
		}
		e.maxParallel = n
	}
}

// WithHost 注入宿主能力原语实现（PRD §6 的 http/call/db/secret/event）。
// 不调用时 Engine 无宿主能力，Tool 声明的 capability 运行期会返回 ErrCapabilityUnavailable。
func WithHost(p *cap.Providers) func(*Engine) {
	return func(e *Engine) {
		e.host = p
	}
}

// WithSecret 注入运行期密钥源（REQ-API-SECRETS）：声明了 secret capability 的 Tool
// 即可 ctx.secret.get(name)，Flow 的 ${secrets.x} 也由此解析。密钥源的**写入**属于
// 宿主管理面（REST /api/secrets、GEAR_SECRET_* 环境变量播种），运行期视图只读——
// 明文密钥绝不落库，Flow/Tool 定义只存引用。
func WithSecret(s cap.Secret) func(*Engine) {
	return func(e *Engine) {
		e.secret = s
	}
}

// WithCapabilities 注入扩展原语目录（用户/插件注册的 ctx.<name>）。
// 不调用时使用空目录（仅内核原语可用，行为与历史一致）。
// 例：reg := engine.NewCapabilityRegistry(); reg.Register("ext.slack", ...); engine.New(r, engine.WithCapabilities(reg))
//
// WithConfigStore 装配键值配置原语（config）。
//
// 装配后两件事同时生效（同一份库，避免「界面配了、CLI 读不到」）：
//  1. 声明 `capabilities: [config]` 的 Tool 可用 ctx.config.get/set/delete/list——应用配置
//     落盘持久化，不必把 DSN 硬编码进 YAML，也不必靠环境变量把配置灌进进程。
//  2. ext.ai 的 provider/model/base_url/api_key/max_tokens 缺省时回退到配置库的 ai.* 键
//     （回退链：调用参数 > 环境变量 > 配置库 > 内置默认），使 `gear config set ai.api_key …`
//     配一次即在所有入口生效。
//
// 默认**不装配**（fail-closed，与 Secret/Subscribe 同口径）：声明了 config 的 Tool 会收到
// ErrCapabilityUnavailable，而不是静默退化成「内存里存一下、重启就丢」。
func WithConfigStore(c cap.Config) func(*Engine) {
	return func(e *Engine) { e.config = c }
}

// NewDefaultConfigStore 返回落点为 gearhome.ConfigDBPath() 的配置库（宿主默认装配件）。
//
// 各入口（CLI / REST / app）统一取用本函数，避免「两个入口各拼一次路径」导致落点漂移。
// 真正的打开是懒执行的：构造它不会碰磁盘。
func NewDefaultConfigStore() cap.Config {
	return cap.NewConfigStore(gearhome.ConfigDBPath())
}

// WithAIAgent 注入进程内 ext.ai agent（generate/complete/catalog/domains/validate/capabilities）。
// 由 SetupExtensions 在注册 ext.ai 时一并调用；不调用则引擎无 AI 能力。
func WithAIAgent(a *AIAgent) func(*Engine) {
	return func(e *Engine) { e.ai = a }
}

// WithScope 设置运行期命名空间守卫的作用域（应用 id，即 registry `apps` 表里的 App.ID）。
//
// 传空串 = 不启用（默认）。非空时，本次进程在**运行期解析点**拒绝任何不属于该应用的
// 已注册对象（Tool / Flow / Hook 的 action），见 `guardScope` 的注释。
//
// 注意它**不是**按请求的开关：`gear serve` 的引擎是长驻共享实例，作用域跟着进程走。
func WithScope(scope string) func(*Engine) {
	return func(e *Engine) { e.scope = scope }
}

// Host 返回注入的宿主能力集合（只读）。
//
// 为什么需要它：迁移这类「内核自己要动库」的场景必须拿到**引擎那个** MultiDB，
// 而不是另造一个——两个实例就是两个连接池，同一份 sqlite 被两条连接开着，
// 「busy_timeout 兜得住」只是侥幸。
func (e *Engine) Host() *cap.Providers {
	if e == nil {
		return nil
	}
	return e.host
}

// Scope 返回当前守卫作用域（空串 = 未启用）。
func (e *Engine) Scope() string { return e.scope }

// guardScope 在运行期解析点做命名空间守卫（IDENTITY_CONTEXT_DESIGN.md §4.3）。
//
// ## 守什么
//
// 已注册对象（Tool / Flow）的 **id**。判定复用 `model.App.Owns`——与写入期
// `checkNamespace` 同一份规则，不存在「写入期认、运行期不认」的漂移。
//
// ## 不守什么（同样是设计的一部分）
//
// **平台级能力不在守卫范围内**，两类：
// ① cap 原语直调（`time.now` / `db.query` 等——它们根本不是 registry 对象，走
// `resolveStepCallView` 的 ② 分支，从不进入本函数）；
// ② **平台自带的注册 Tool**（`ext.serve` / `static_site.serve` / `web_ui.api`——
// 无应用前缀、第一段不归属任何 App）。把平台能力按应用前缀一刀切，会让任何一个
// 开了 `--scope` 的应用连自己的 Flow 都跑不起来（mindstorm 的入口 Flow 同时调用了
// `mindstorm__config` 与 `static_site.serve`）。守的语义是「别碰**别人应用的
// 定义**」，不是「别用平台」。
//
// ② 的判定 = 命名空间第一段（`namespaceHead`）不是任何已注册 App。⚠ 2026-10-21 前
// 只兑现了 ①：注释承诺 ②、代码却把 ext.serve 这类注册 Tool 一并毙掉（实测
// `tool run <无前缀id> --scope wms` 被拒），承诺与实现直接矛盾。现已补齐，S-11 钉住。
//
// ## 空 scope
//
// `e.scope == ""` 直接放行。这不是「没实现」，是 §4.3 定的启用策略：默认不开，
// 否则先存在后挂 App 的老定义全部跑不起来。
//
// ## 落点（五处，缺一处就等于没守卫）
//
// | # | 位置 | 拦什么 |
// |---|---|---|
// | 1 | `runFlow` 入口 | 整个 Flow 的 id（跑别人应用的 Flow） |
// | 2 | `resolveStepCallView` | step 解析出的调用目标（Tool 与 Flow 两条分支） |
// | 3 | `newResolver` 的两处 | 由 step.Call 动态解析出的目标 |
// | 4 | `applyHooks` / `applyFlowScopedHooks` | Hook 的 action |
// | 5 | `RunToolExec` | 直接点名执行一个 Tool（`gear tool run` 那条路） |
//
// 前四处都在「解析」链路上；**第 5 处不在** —— `RunToolExec` 自己拼 `resolvedCall`，
// 从 2 那里直接跳过，而它正是「不经 Flow 就能执行任意 Tool」的捷径。
// 这第五处是 2026-09-26 实跑验证时补的：当时只验了 Flow 面，`gear tool run`
// 一测就穿了。教训是「解析点」不能按**语义**列（step / hook），得按**代码路径**列
// ——凡是自己造 out 的调用点，都得单独数一遍。
func (e *Engine) guardScope(id string) error {
	if e.scope == "" {
		return nil
	}
	if (&model.App{ID: e.scope}).Owns(id) {
		return nil
	}
	// 平台级豁免（2026-10-21 补）：id 的命名空间第一段不归属任何已注册 App ⇒ 它不是
	// 「别人应用的定义」，拦它没有保护意义，只会误杀 ext.serve / static_site.serve /
	// web_ui.api 这类**平台自带的注册 Tool**。注释一直承诺此豁免，实现直到本次才补上
	// （resolveStepCallView 的 ① 分支对任何已注册 Tool 无条件调本函数，承诺与代码
	// 曾直接矛盾；S-11 钉住）。
	//
	// 口径与 model.App.Owns 同源（第一段 = 第一个 `__` 或 `.` 前，取更靠前者）：
	// 第一段命中某个 App 且不是自己 ⇒ 别人的定义，拦；未命中/查不到 ⇒ 平台级，放行。
	if head := namespaceHead(id); head != e.scope {
		if app, err := e.reg.GetApp(head); err == nil && app != nil {
			return diagWrap(CodeScopeViolation,
				fmt.Errorf("命名空间守卫：本进程 scope=%q 不允许引用 %q", e.scope, id),
				"", "scope",
				fmt.Sprintf("把 %q 改成 %s__ 开头，或去掉 --scope 关闭守卫；要确认可用 id 用 gear_catalog(kind=tool)", id, e.scope))
		}
	}
	return nil
}

// namespaceHead 取 id 的命名空间第一段：第一个 `__` 或第一个 `.` 之前的部分（取更靠前
// 者）。与 model.App.Owns 的前缀判定同口径——Owns 用 HasPrefix(id, appID+"__") /
// HasPrefix(id, appID+".")，所以 App.ID 含单个下划线（web_ui）的形态必须按 `__`/`.`
// 切，不能按 `_`/`.` 切（`web_ui.api` 的第一段是 `web_ui` 不是 `web`）。
func namespaceHead(id string) string {
	if i := strings.Index(id, "__"); i >= 0 {
		if j := strings.Index(id, "."); j < 0 || i < j {
			return id[:i]
		}
	}
	if j := strings.Index(id, "."); j >= 0 {
		return id[:j]
	}
	return id
}

// guardFlowScope 是 guardScope 的 Flow 版本：**额外放行应用自己的 entry_flow**。
//
// 入口 Flow 按约定**可以**不带前缀（与 artifact.Composition 的豁免同口径：它的 id 可能就是
// 裸的 `up`）。少了这条豁免，`gear app start --scope mindstorm mindstorm.up` 会连自己的入口
// 都跑不起来——守卫把「自己」也挡在门外的实现，比没有守卫更糟。
func (e *Engine) guardFlowScope(flow *model.Flow) error {
	if e.scope == "" {
		return nil
	}
	if (&model.App{ID: e.scope}).Owns(flow.ID) {
		return nil
	}
	if app, err := e.reg.GetApp(e.scope); err == nil && app != nil && flow.ID == app.EntryFlow {
		return nil
	}
	return e.guardScope(flow.ID)
}

// AIAgent 返回进程内 ext.ai agent（未装配时 nil）。
func (e *Engine) AIAgent() *AIAgent { return e.ai }

// SetSubscribeCallbackBase 注入订阅回推基址（serve 监听时由 server 调用）。
// 未装配订阅管理器时静默跳过（不会发生，New 恒创建）。
func (e *Engine) SetSubscribeCallbackBase(base string) {
	if e.subscriber != nil {
		e.subscriber.SetCallbackBase(base)
	}
}

// Subscriber 返回通用订阅管理器（未装配时 nil）。
func (e *Engine) Subscriber() *Subscriber { return e.subscriber }

// Deliver 把一条事件投递给指定订阅句柄（server 的回推端点调用）。
func (e *Engine) Deliver(handle string, payload interface{}) error {
	if e.subscriber == nil {
		return fmt.Errorf("subscribe 原语未装配")
	}
	return e.subscriber.Deliver(handle, payload)
}
func WithCapabilities(reg *CapabilityRegistry) func(*Engine) {
	return func(e *Engine) {
		e.capReg = reg
	}
}

// LogSink 接收一次执行中每个 step 的运行日志行（REQ-API-LOGS / #71）。
// 参数为 (execID, stepID, stream, line)；stream ∈ {stdout, stderr}。
type LogSink func(execID, stepID, stream, line string)

// WithLogSink 注入执行日志采集器；execID 非空时引擎上报 step 日志，供
// GET /api/executions/{id}/logs 查询。不调用则不记日志。
func WithLogSink(fn LogSink) func(*Engine) {
	return func(e *Engine) {
		e.logSink = fn
	}
}

// WithSandbox 设置 Lua 沙箱策略（strict / permissive）。未知策略被忽略，保持默认 strict
// （绝不静默接受一个会关掉沙箱的值）。
func WithSandbox(policy string) func(*Engine) {
	return func(e *Engine) {
		switch policy {
		case SandboxStrict, SandboxPermissive:
			e.sandboxPolicy = policy
		}
	}
}

// callChainOf 从 runContext 拼出本次执行的调用链（祖先 Flow 链 + 本 Flow），空元素已剔除。
// 调用链有两个用途（见 withCallChain），且**只有此处一处拼法**：
// Tool 载体（hostFor）与直调原子能力（primitiveHost）共用，避免两条路径的守卫口径漂移。
func callChainOf(rc *runContext) []string {
	if rc == nil {
		return nil
	}
	var via []string
	for _, id := range rc.ancestors {
		if id != "" {
			via = append(via, id)
		}
	}
	if rc.flowID != "" {
		via = append(via, rc.flowID)
	}
	return via
}

// withCallChain 把调用链写入宿主视图：
//   - ctx.call 的循环守卫依据（复用 #72 祖先链语义）。
func withCallChain(c *cap.Context, via []string) *cap.Context {
	if len(via) > 0 {
		c.SetCallChain(via)
	}
	return c
}

// hostFor 为一次执行构建宿主能力视图：按 Tool.Capabilities 精确授权，从引擎持有
// 的宿主实现集中取原语。rc 非空时把当前执行链写入 ctx（供 ctx.call 递归守卫）；
// 注册期已校验 Capabilities 合法（model.Tool.Validate），故此处忽略错误。
func (e *Engine) hostFor(tool *model.Tool, rc *runContext) *cap.Context {
	c := e.hostForChain(tool, callChainOf(rc))
	if rc != nil {
		c.SetSubscriptionOwner(cap.SubscriptionOwner{FlowID: rc.flowID, ExecID: rc.execID})
		c.SetCaller(rc.caller) // 身份唯一的注入点（见 cap.Caller：别在别处再写一遍）
	}
	return c
}

// hostForChain 用给定的调用链构造宿主视图（调用链用于 ctx.call 循环守卫，复用 #72 祖先链语义）。
func (e *Engine) hostForChain(tool *model.Tool, via []string) *cap.Context {
	grants, _ := cap.GrantsFromStrings(tool.Capabilities)
	c := withCallChain(cap.New(grants, e.host), via)
	// 注入 Tool 声明、且已注册的扩展原语：经 Provider 取原生实现对象填进 c.exts。
	// 未在此处注入的扩展名（如声明了但 Provider 报错）会经 cap.Context.Extension 返回
	// ErrCapabilityUnavailable，与内置原语「授权但未实现」同语义。
	for _, name := range tool.Capabilities {
		if ext, ok := e.capReg.Lookup(name); ok {
			if v, err := ext.Provider(c); err == nil {
				c.SetExtension(name, v)
			}
		}
	}
	return c
}

// newResolver 构造引擎的 call 解析器（插件间唯一通信通道，PRD §6）：
//   - target 是已注册 Tool：直接执行其 runtime（按自身 Capabilities 授权）。
//   - target 是已注册 Flow：同步内联执行（复用 e.run，调用链追加 target 做循环守卫）。
//   - 否则报错（既非 Tool 也非 Flow）。
func (e *Engine) newResolver() cap.Resolver {
	return func(target string, input map[string]interface{}, via []string) (map[string]interface{}, error) {
		// 1) Tool 优先（call 的主通道是调用一个已注册 Tool）。
		if tool, err := e.reg.GetTool(target); err == nil && tool != nil {
			// 运行期命名空间守卫（`--scope`）：ctx.call 是除 step.call 之外的第二条逃逸路径
			// ——它把 target 交给了原语直调，能摸到的对象比 step.call 多，不拦就白守卫了。
			if err := e.guardScope(tool.ID); err != nil {
				return nil, err
			}
			rt, err := runtimeForCaps(tool.Runtime, e.sandboxPolicy, e.capReg)
			if err != nil {
				return nil, err
			}
			host := e.hostForChain(tool, via)
			out, err := rt.Run(context.Background(), tool.Entry, input, host)
			if err != nil {
				return nil, fmt.Errorf("call Tool %q 失败: %w", target, err)
			}
			return out, nil
		}
		// 2) Flow：内联执行，循环守卫（target 已在调用链上则拦截，避免 A→B→A 无限递归）。
		if flow, err := e.reg.GetFlow(target); err == nil && flow != nil {
			if err := e.guardScope(flow.ID); err != nil {
				return nil, err
			}
			for _, id := range via {
				if id == target {
					return nil, fmt.Errorf("call %q 会形成循环调用（调用链 %v），已拦截", target, via)
				}
			}
			res, err := e.run(flow, nil, "", append(append([]string{}, via...), target))
			if err != nil {
				return nil, fmt.Errorf("call Flow %q 失败: %w", target, err)
			}
			if res == nil {
				return nil, fmt.Errorf("call Flow %q 返回空结果", target)
			}
			// 返回子 Flow 的 step 输出集合（stepID → {input, output}），供调用方经 r.<stepID>.output 取用。
			out := make(map[string]interface{}, len(res.Steps))
			for k, v := range res.Steps {
				out[k] = v
			}
			return out, nil
		}
		return nil, fmt.Errorf("call 目标 %q 既不是已注册 Tool 也不是 Flow", target)
	}
}

// secretResolver 返回执行期 ${secrets.x} 的解析器（REQ-TMPL-SECRETS）。
// 宿主未注入 secret 实现时返回 nil —— 模板解析会据此显式报错，而非用空值静默替换。
// 设计要点：Flow 定义只存 `${secrets.api_key}` 引用，明文密钥始终留在宿主 secret 源
// （如环境变量注入的 map），绝不落库，规避「secret 经注册期定义泄露」。
func (e *Engine) secretResolver() func(name string) (string, error) {
	if e.host == nil || e.host.Secret == nil {
		return nil
	}
	return func(name string) (string, error) {
		return e.host.Secret.Get(name)
	}
}

// logSinkFor 返回绑定了 execID 的 step 级日志回调；execID 为空或无 sink 时返回 nil
// （不记日志，兼容无日志采集的调用方）。
func (e *Engine) logSinkFor(execID string) func(stepID, stream, line string) {
	if e.logSink == nil || execID == "" {
		return nil
	}
	return func(stepID, stream, line string) {
		e.logSink(execID, stepID, stream, line)
	}
}

// RunResult 是一次 Flow 执行的结构化结果。
// Steps：step id → {input, output}（**仅已执行**的 step）。
// Skipped：step id → 跳过原因（when 为假或上游被跳过）——「没跑」与「跑失败」必须分开表达（F6）。
//
// JSON tag 是**对外契约的一部分**（REST /api/executions 直接序列化它），故显式声明
// snake_case，避免默认导出 Go 字段名（FlowID/Steps）泄漏到 API 形状里。
type RunResult struct {
	FlowID  string                            `json:"flow_id"`
	Steps   map[string]map[string]interface{} `json:"steps,omitempty"`
	Skipped map[string]string                 `json:"skipped,omitempty"`
}

// RunFlow 一次性执行（flow run）：所有 step 必须为 once 模式。
// 遇到 service 模式 step 报错，提示改用 app run（F3：service 长驻不能一次性执行）。
func (e *Engine) RunFlow(flowID string) error {
	_, err := e.RunFlowResult(flowID)
	return err
}

// RunFlowResult 是 RunFlow 的带结果版本，返回每个 step 的最终 input/output。
func (e *Engine) RunFlowResult(flowID string) (*RunResult, error) {
	return e.RunFlowResultObserved(flowID, nil)
}

// ToolObserver 接收一次运行「可能用到」的 Tool id 集合（已去重、升序）。
type ToolObserver func(toolIDs []string)

// RunFlowResultObserved 与 RunFlowResult 相同，但在**加载完 Flow 与 Hook 快照之后**
// 回调一次 observe，告知本次运行可能用到的 Tool。
//
// 为什么需要「运行期引用」可观测（PRD §8.2 第三条守卫）：
// Registry 的注销守卫只能看到**静态**引用（Flow 的 step.call / Hook 的 action）。
// 一旦 Hook 被删除、或 Flow 被重新注册去掉了该 step，静态引用就消失了，
// 但**正在跑的执行**仍持有这些 Tool（它在启动时已把 Hook 快照读进内存）——
// 此时若允许注销，正在跑的执行会在下次触发 Hook 时拿到 nil Tool 而中途失败。
// 唯一可靠的判据来自运行侧，所以由引擎在快照点上报，而不是让 Registry 去猜。
func (e *Engine) RunFlowResultObserved(flowID string, observe ToolObserver) (*RunResult, error) {
	return e.RunFlowExec(flowID, "", observe)
}

// RunFlowResultCtx 是 RunFlowResult 的带调用上下文版本。
//
// 存在的唯一理由：大部分场景（含全部测试）不需要身份，用 RunFlowResult 更省事；
// 而**需要身份**的场景（Hook 鉴权、审计）只有这一处入口，不会漏。
// 实现上它就是 RunFlowExecWithVars 的薄壳，没有第二套执行路径。
func (e *Engine) RunFlowResultCtx(ctx context.Context, flowID string) (*RunResult, error) {
	return e.RunFlowExecWithVars(ctx, flowID, "", nil, nil)
}

// RunFlowExec 是 RunFlowResultObserved 的带执行 id 版本：execID 非空时，引擎会把每个
// step 的运行日志（stdout/stderr 行）经 logSink 上报（REQ-API-LOGS / #71），供
// `GET /api/executions/{id}/logs` 查询。execID 为空则行为与 RunFlowResultObserved 完全一致
// （不记日志，兼容历史调用点与测试）。
func (e *Engine) RunFlowExec(flowID, execID string, observe ToolObserver) (*RunResult, error) {
	return e.RunFlowExecCtx(context.Background(), flowID, execID, observe)
}

// RunFlowExecCtx 是 RunFlowExec 的带调用上下文版本（与 RunFlowResultCtx 同一套路）。
//
// 它的**唯一**现存调用方是 REST 的 `POST /api/executions`：执行是在闸门 worker 里异步跑的，
// 不能在闭包里沿用请求的 ctx（handler 一返回请求 ctx 就取消，跑到一半的执行会被判超时）。
// 故请求侧的做法是「只把主体字符串抓出来」，在这里重新构造一个活着的 ctx。
func (e *Engine) RunFlowExecCtx(ctx context.Context, flowID, execID string, observe ToolObserver) (*RunResult, error) {
	return e.RunFlowExecWithVars(ctx, flowID, execID, observe, nil)
}

// RunFlowWithVars 与 RunFlowResult 相同，但把 vars **覆盖合并**进 Flow 声明的初始 vars
// （覆盖优先；只作用于本次运行，**不写回 registry**）。
//
// 这是「运行时给 Flow 传参」的入口：Flow 定义里的 vars 视为默认值，调用方按需覆盖；
// 合并结果作为 rc.vars，before_flow Hook 仍可在此基础上继续改写。
// vars 为空/nil 时行为等价于 RunFlowResult。
func (e *Engine) RunFlowWithVars(flowID string, vars map[string]interface{}) (*RunResult, error) {
	return e.runFlow(context.Background(), flowID, "", nil, vars)
}

// RunFlowExecWithVars 是 RunFlowExec 与 RunFlowWithVars 的合体入口：
// 带 exec id（step 日志经 logSink 落盘 + 运行期注销守卫上报）**且**支持 vars
// 运行时覆盖。MCP 执行面用这个：同步等结果、要日志、要传参，三者都要。
// vars 为 nil 时行为与 RunFlowExec 完全一致。
//
// ctx 携带调用上下文（IDENTITY_CONTEXT_DESIGN.md）。它是**唯一**带 ctx 的 Flow 执行入口，
// 其余公开入口一律传 context.Background()（= 无身份，与改造前同语义）后委托到它——
// 这样身份来源只有一个，不存在「两条路径两套身份」的漂移。
func (e *Engine) RunFlowExecWithVars(ctx context.Context, flowID, execID string, observe ToolObserver, vars map[string]interface{}) (*RunResult, error) {
	return e.runFlow(ctx, flowID, execID, observe, vars)
}

// runFlow 是 RunFlowExec / RunFlowWithVars 的公共前置：取 Flow → 拒绝 service step → 执行。
func (e *Engine) runFlow(ctx context.Context, flowID, execID string, observe ToolObserver, varsOverride map[string]interface{}) (*RunResult, error) {
	flow, err := e.reg.GetFlow(flowID)
	if err != nil {
		return nil, err
	}
	if flow == nil {
		return nil, fmt.Errorf("Flow 不存在: %s", flowID)
	}
	// 运行期命名空间守卫（`--scope`）：本进程只允许跑自己名下的 Flow。
	// 放在取到 flow 之后、拒绝 service 之前：两条拒绝互不掩盖，先报「越权」才报「别用 flow run 起长驻」。
	if err := e.guardFlowScope(flow); err != nil {
		return nil, err
	}
	for _, s := range flow.Steps {
		if s.IsService() {
			return nil, fmt.Errorf("step %s 是 service 模式（长驻），不能用 flow run 一次性执行；请用 `app run %s` 监管", s.ID, flowID)
		}
	}
	return e.runWithVars(ctx, flow, observe, execID, nil, varsOverride)
}

// mergeVars 把运行期覆盖合并进 Flow 声明的 vars（覆盖优先），返回新 map。
// 不修改任何入参——flow.Vars 来自 registry 快照，绝不能被就地改写。
func mergeVars(base, override map[string]interface{}) map[string]interface{} {
	if len(override) == 0 {
		return base
	}
	out := make(map[string]interface{}, len(base)+len(override))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range override {
		out[k] = v
	}
	return out
}

// RunTool 一次性执行单个 Tool（供 `gear tool run` 与「单跑一个原子能力」的调用方）。
//
// 语义与「在 Flow 里执行一个 step」**完全一致**：input_schema 校验 → capability 门禁 →
// runtime 执行（受「Tool 声明的 timeout，缺省 defaultExecTimeout」限制）→ step 级 Hook（before_step / after_step，按 tool id 匹配）
// → output_schema 校验。返回该 Tool 的 output。
//
// 会触发按 tool id 匹配的 **step 级** Hook；**不**触发 Flow 级 Hook（before_flow / on_success
// 需依附某个 Flow，此处没有 Flow 可依附）。session 作用域命名空间用 tool id（等价于「只含一步的 Flow」），
// 运行结束即回收。
func (e *Engine) RunTool(toolID string, input map[string]interface{}) (map[string]interface{}, error) {
	return e.RunToolExec(context.Background(), toolID, input, "")
}

// RunToolExec 是 RunTool 的带执行 id 版本：execID 非空时把运行日志经 logSink 上报，
// 供 `GET /api/executions/{id}/logs` 查询（与 RunFlowExec 同语义）。
func (e *Engine) RunToolExec(ctx context.Context, toolID string, input map[string]interface{}, execID string) (map[string]interface{}, error) {
	tool, err := e.reg.GetTool(toolID)
	if err != nil {
		return nil, err
	}
	if tool == nil {
		return nil, fmt.Errorf("Tool 不存在: %s", toolID)
	}
	// 第 5 个运行期解析点（其余四处见 guardScope 注释里的落点表）。这条路由本方法自己拼
	// resolvedCall，**不经 `resolveStepCallView`**，所以那条路上的守卫对它无效——
	// 而它就长在 `gear tool run` 上：不经过任何 Flow 也能点名执行一个 Tool，
	// 正是最直接、最该堵死的越权入口。守卫必须在这里插一次，不能指望上游替我们拦。
	if err := e.guardScope(toolID); err != nil {
		return nil, err
	}
	if input == nil {
		input = map[string]interface{}{}
	}

	hooks, err := e.loadHooks()
	if err != nil {
		return nil, err
	}
	rc := &runContext{
		vars:      map[string]interface{}{},
		steps:     map[string]map[string]interface{}{},
		secretGet: e.secretResolver(),
		execID:    execID,
		logSink:   e.logSinkFor(execID),
		flowID:    toolID, // 供 hostFor 拼调用链
	}
	// 与 runWithVars 同口径：调用上下文在 rc 构造处读一次（见 cap.Caller）。
	if cr, ok := CallerFrom(ctx); ok {
		rc.caller = cr
	}
	// step.ID 用 tool id：出错时「step <id> 执行失败」能直接指出是哪个 Tool。
	step := model.Step{ID: toolID, Call: toolID, Input: input}
	_, out, err := e.runOnceStep(context.Background(), rc, hooks, toolID, step, resolvedCall{tool: tool})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// plannedToolIDs 计算一次运行可能用到的调用目标：step 的 call ∪ 生效 Hook 的 action。
//
// step.Call 可能是已注册 Tool，也可能是「能力.方法」直调的原子能力（见 primitive.go）——
// 这里**原样上报 step.Call**，不做区分：上报结果用于 §8.2「运行中不可注销」守卫，
// 而该守卫判的正是「字符串是否与待注销的 id 相等」，两种载体天然一致（同名 Tool 优先解析，
// 因此「Tool 名恰好等于某能力方法名」时上报的也正是那个 Tool，语义正确）。
//
// Hook 的选择规则**复用 hooksFor**（applyHooks 用的同一个函数），不在此重写匹配逻辑——
// 否则「哪些 Hook 会触发」就出现了第二处判定，日后加 stage 或改 target 语义时必漏一处
// （G2/M10 的教训：同一判定出现两处即漂移）。
func plannedToolIDs(flow *model.Flow, hooks []model.Hook) []string {
	seen := map[string]bool{}
	// 全部已实现 stage 都参与「计划用到的 Tool」计算：Flow 级 stage 按 flow.ID 匹配，
	// step 级 stage 按 step.Call 匹配（匹配维度由 hooksFor 内部按 stage 选，避免第二处判定）。
	allStages := []string{
		model.StageBeforeFlow, model.StageBeforeStep, model.StageAfterStep,
		model.StageOnError, model.StageOnSuccess,
	}
	for _, s := range flow.Steps {
		seen[s.Call] = true
		for _, stage := range allStages {
			for _, h := range hooksFor(hooks, stage, flow.ID, s.Call) {
				seen[h.Action] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Strings(out) // 稳定输出，便于测试与日志断言
	return out
}

func (e *Engine) run(flow *model.Flow, observe ToolObserver, execID string, ancestors []string) (*RunResult, error) {
	return e.runWithVars(context.Background(), flow, observe, execID, ancestors, nil)
}

// runWithVars 执行一个**已加载**的 Flow（run 的执行内核）。varsOverride 非空时与 flow.Vars
// 合并后作为初始 vars（覆盖优先）——用于「运行时给 Flow 传参」，不改动 registry 里的 Flow 快照；
// 合并结果随后仍会被 before_flow Hook 改写。
// runWithVars 执行一个**已加载**的 Flow（run 的执行内核）。varsOverride 非空时与 flow.Vars
// 合并后作为初始 vars（覆盖优先）——用于「运行时给 Flow 传参」，不改动 registry 里的 Flow 快照；
// 合并结果随后仍会被 before_flow Hook 改写。
//
// ctx 携带调用上下文（IDENTITY_CONTEXT_DESIGN.md），在**此处**（rc 构造点）被读一次写进
// runContext.caller，此后整条运行期都从 rc 取——不必再把 ctx 传到最底层的 Hook / step 里。
// 当前上游公开入口一律传 context.Background()，语义等同改造前的「无身份」。
func (e *Engine) runWithVars(ctx context.Context, flow *model.Flow, observe ToolObserver, execID string, ancestors []string, varsOverride map[string]interface{}) (res *RunResult, err error) {
	flowStart := time.Now()
	// 诊断日志（REQ-CLI-DIAG-LOG）：Flow 执行的档案行。与 progressf 正交——
	// 进度面向盯着看的人（stdout），这里面向事后归档（--log-file）；未启用时
	// gearlog 是 discard，零副作用。defer + 命名返回值让各 return 点全覆盖。
	defer func() {
		elapsed := time.Since(flowStart)
		if err != nil {
			gearlog.L().Error("Flow 执行失败",
				slog.String("flow", flow.ID), slog.String("exec_id", execID),
				slog.String("err", err.Error()), slog.Duration("elapsed_ms", elapsed))
			return
		}
		gearlog.L().Info("Flow 执行完成",
			slog.String("flow", flow.ID), slog.String("exec_id", execID),
			slog.Int("steps", len(flow.Steps)), slog.Duration("elapsed_ms", elapsed))
	}()
	// 订阅所属按 Flow/Exec 回收（运行结束 defer）：
	// 本次运行中经 ctx.subscribe.start 建立的订阅在此一并 StopByOwner。
	if e.subscriber != nil && flow.ID != "" {
		defer e.subscriber.StopByOwner(cap.SubscriptionOwner{FlowID: flow.ID, ExecID: execID})
	}
	// 直接用调用方传入的 ctx（**不**再起一个 Background）：它既承担运行期取消的传递，
	// 也承载调用上下文。层级取消由 execLevel 自行 WithCancel 派生，与本 ctx 无关。
	levels, err := topoLevels(flow.Steps)
	if err != nil {
		return nil, err
	}

	hooks, err := e.loadHooks()
	if err != nil {
		return nil, err
	}

	// 在快照点上报「本次运行可能用到的 Tool」——这是上层实现 §8.2「运行中不可注销」
	// 守卫的唯一可靠依据（静态引用会在 Hook 被删 / Flow 被改写后消失）。
	if observe != nil {
		observe(plannedToolIDs(flow, hooks))
	}

	rc := &runContext{
		vars:      mergeVars(flow.Vars, varsOverride),
		steps:     map[string]map[string]interface{}{},
		secretGet: e.secretResolver(),
		execID:    execID,
		logSink:   e.logSinkFor(execID),
		ancestors: ancestors,
		flowID:    flow.ID,
	}
	// 调用上下文在**此处**读一次，此后整条运行期都从 rc 取。
	// 未命中 = 无身份（Subject 为空），不是「匿名用户」——Hook 要能区分「没验过」。
	if cr, ok := CallerFrom(ctx); ok {
		rc.caller = cr
	}

	// before_flow Hook（#72）：Flow 开始执行前，按 flow.ID 匹配。可改写初始 vars
	// （Tool 载体整体替换 vars；Flow 载体内联执行并把结果暴露进 vars）。失败即终止编排。
	if err := e.applyFlowScopedHooks(ctx, rc, hooks, model.StageBeforeFlow, flow.ID); err != nil {
		return nil, err
	}

	// when 跳过需要沿**有效依赖**（含推断边）传播：上游被跳过 ⇒ 下游也跳过。
	deps, err := depMap(flow.Steps)
	if err != nil {
		return nil, err
	}
	skipped := map[string]string{} // stepID -> 跳过原因
	var skMu sync.Mutex
	markSkipped := func(id, reason string) {
		skMu.Lock()
		skipped[id] = reason
		skMu.Unlock()
	}
	skipReasonFor := func(id string) (string, bool) {
		skMu.Lock()
		defer skMu.Unlock()
		for _, d := range deps[id] {
			if r, ok := skipped[d]; ok {
				return fmt.Sprintf("上游 step %s 被跳过（%s）", d, r), true
			}
		}
		return "", false
	}

	// S4：分层执行——同一层内无依赖的 step 并行调度（PRD §4.3），受 maxParallel 限流。
	for _, level := range levels {
		err := e.execLevel(flow.Steps, level, func(ctx context.Context, step model.Step, call resolvedCall) error {
			if reason, ok := skipReasonFor(step.ID); ok {
				markSkipped(step.ID, reason)
				e.progressf("- step %s 跳过（%s）\n", step.ID, reason)
				return nil
			}

			input, output, err := e.runOnceStep(ctx, rc, hooks, flow.ID, step, call)
			if errors.Is(err, ErrStepSkipped) {
				markSkipped(step.ID, "when=false")
				e.progressf("- step %s 跳过（when=false）\n", step.ID)
				return nil
			}
			if err != nil {
				return err
			}
			rc.setStep(step.ID, input, output)
			e.progressf("✓ step %s (%s) -> %v\n", step.ID, step.Call, output)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}

	e.progressf("✓ Flow %s 执行完成，共 %d 步", flow.ID, len(flow.Steps))
	if n := len(skipped); n > 0 {
		e.progressf("，跳过 %d 步", n)
	}
	e.progressf("\n")

	result := &RunResult{FlowID: flow.ID, Steps: rc.steps, Skipped: skipped}
	rc.setResult(result)

	// on_success Hook（#72）：仅当 Flow「全部成功」时触发（step 失败已在上面返回错误，不会到达此处）。
	// 其 payload 是最终 RunResult（仅副作用，输出丢弃）；critical 失败会让「已成功的主 Flow」返回错误。
	if err := e.applyFlowScopedHooks(ctx, rc, hooks, model.StageOnSuccess, flow.ID); err != nil {
		return nil, err
	}
	return result, nil
}

// execLevel 并发执行一层 step（受 maxParallel 限制）。
//
// 每个 step 的 `call` 在此统一解析成可执行单元（resolveStepCall）：已注册 Tool，或
// 「能力.方法」直调的原子能力——解析失败即 fail-closed，不会拿一个不存在的目标去执行。
//
// 失败语义（fail-closed）：首个错误即取消本层上下文（阻止未启动的 step 起跑，
// 并让在跑的子进程随 ctx 被杀），等待本层全部 goroutine 收尾后返回该错误；
// 后续层不再执行。
func (e *Engine) execLevel(steps []model.Step, level []int, fn func(ctx context.Context, step model.Step, call resolvedCall) error) error {
	lvlCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sem := make(chan struct{}, e.maxParallel)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error

	record := func(err error) {
		mu.Lock()
		if firstErr == nil {
			firstErr = err
		}
		mu.Unlock()
		cancel()
	}

loop:
	for _, idx := range level {
		select {
		case <-lvlCtx.Done():
			break loop
		case sem <- struct{}{}:
		}
		wg.Add(1)
		step := steps[idx]
		go func(step model.Step) {
			defer wg.Done()
			defer func() { <-sem }()

			// 唯一解析点：step.Call 先按已注册 Tool 解析，再按「能力.方法」直调原语兜底
			// （规则与注册期 ValidateFlowStatic 共用 resolveStepCall，不在此重写判定）。
			call, err := e.resolveStepCall(step)
			if err != nil {
				record(err)
				return
			}
			if err := fn(lvlCtx, step, call); err != nil {
				record(err)
			}
		}(step)
	}
	wg.Wait()
	return firstErr
}

// 拓扑排序 / 依赖推断见 deps.go（topoLevels / topoOrder / effectiveDeps）。

// runContext 保存跨 step 的数据流上下文。
//
// S4 并发安全：同一层的 step 会**并行**执行并写入 steps，故所有访问都经 mu 保护。
// vars 是 Flow 级只读（并行 step 不写 vars），无并发写问题。
type runContext struct {
	mu    sync.Mutex
	vars  map[string]interface{}
	steps map[string]map[string]interface{}
	// secretGet 是执行期 secret 解析器（REQ-TMPL-SECRETS）：从宿主 secret 源取密钥明文。
	// nil 表示宿主未注入 secret 实现，${secrets.x} 解析时显式报错（不静默失败）。
	secretGet func(name string) (string, error)
	// execID 是当前执行的 id（供日志采集关联）；空表示不记日志。
	execID string
	// logSink 是 step 级日志回调（REQ-API-LOGS / #71）；nil 表示不记日志。
	logSink func(stepID, stream, line string)
	// result 是本次 Flow 的最终结果（供 on_success Hook 取用，仅 once 模式）。
	result *RunResult
	// ancestors 是拉起本 Flow 的「祖先 Flow id」链（不含自身），用于 before_flow 内联子 Flow
	// 的递归守卫（#72）：防止 A→B→A 循环或被 Target="*" 的 before_flow 钩中自身而无限递归。
	ancestors []string
	// flowID 是本 Flow 的 id（供递归守卫快速拼出完整调用链）。
	flowID string
	// caller 是本次执行的调用上下文（IDENTITY_CONTEXT_DESIGN.md §3.2）。**唯一写入口**：
	// 只从引擎边界的 ctx 值读取，永不取自 vars / 调用参数——调用方无法伪造身份。
	// 经 hostFor / primitiveHost 注入宿主视图，故 Hook 与 Tool 都能看到。
	caller cap.Caller
}

// setStep 记录一个 step 的最终 input/output（并发安全）。
func (c *runContext) setStep(id string, input, output map[string]interface{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.steps[id] = map[string]interface{}{"input": input, "output": output}
}

// varsSnapshot 返回 vars 的浅拷贝（before_flow Hook 改写前的快照，避免 Hook 读到改写中途状态）。
func (c *runContext) varsSnapshot() map[string]interface{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]interface{}, len(c.vars))
	for k, v := range c.vars {
		out[k] = v
	}
	return out
}

// replaceVars 整体替换 vars（before_flow 的 Tool 载体返回非空 object 时调用）。
func (c *runContext) replaceVars(m map[string]interface{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.vars = m
}

// exposeFlowResult 把内联子 Flow 的 steps 输出暴露进主 Flow 的 vars（key=子 Flow id），
// 供下游 step 经 `vars.<flowID>.<stepID>.output` 引用。类型上把
// map[string]map[string]interface{} 转成 map[string]interface{}（值本就是 map[string]interface{}），
// 以保证模板解析的嵌套 map 递归能正确下钻（否则 map[string]map[...] 不匹配 map[string]interface{} 分支）。
func (c *runContext) exposeFlowResult(flowID string, res *RunResult) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.vars == nil {
		c.vars = map[string]interface{}{}
	}
	exposed := make(map[string]interface{}, len(res.Steps))
	for k, v := range res.Steps {
		exposed[k] = v
	}
	c.vars[flowID] = exposed
}

// setResult 记录最终结果（供 on_success Hook 取用）。
func (c *runContext) setResult(r *RunResult) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.result = r
}

// lastResultMap 把最终结果序列化成 map（on_success Hook 的 payload；仅副作用，输出丢弃）。
func (c *runContext) lastResultMap() map[string]interface{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.result == nil {
		return map[string]interface{}{}
	}
	m := map[string]interface{}{"flow_id": c.result.FlowID}
	if c.result.Steps != nil {
		m["steps"] = c.result.Steps
	}
	if c.result.Skipped != nil {
		m["skipped"] = c.result.Skipped
	}
	return m
}

// snapshot 返回 vars 与 steps 的**浅拷贝**，供 when 求值等只读场景使用。
// 直接把 map 交给外部读取会在并行写入时触发 data race，故这里在锁内拷贝外层 map
// （内层 map 由 setStep 整体替换、从不原地修改，故浅拷贝足够）。
func (c *runContext) snapshot() (map[string]interface{}, map[string]map[string]interface{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	steps := make(map[string]map[string]interface{}, len(c.steps))
	for k, v := range c.steps {
		steps[k] = v
	}
	return c.vars, steps
}

// resolveInput 对 step.input 做模板替换（vars / steps 引用）。
// 持有读锁期间完成解析，避免并发读到「写了一半」的 steps。
func resolveInput(in map[string]interface{}, ctx *runContext) (map[string]interface{}, error) {
	ctx.mu.Lock()
	defer ctx.mu.Unlock()

	out := make(map[string]interface{}, len(in))
	for k, v := range in {
		rv, err := resolveValue(v, ctx)
		if err != nil {
			return nil, err
		}
		out[k] = rv
	}
	return out, nil
}

func resolveValue(v interface{}, ctx *runContext) (interface{}, error) {
	switch x := v.(type) {
	case string:
		// 整串为单一引用（如 `${vars.tools}`、`${vars.max_retries}`）时返回原始值，
		// 让 map/slice/number 等复杂值原样穿过输入层到达 ctx.input，而不是被字符串化；
		// 其余（含字面量的嵌入引用，如 `prefix-${vars.x}`）仍走字符串展开。
		if refs, err := scanTemplateRefs(x); err == nil && len(refs) == 1 &&
			refs[0].Start == 0 && refs[0].End == len(x)-1 {
			if val, lerr := lookup(refs[0], ctx); lerr == nil {
				return val, nil
			}
		}
		return expandString(x, ctx)
	case map[string]interface{}:
		m := make(map[string]interface{}, len(x))
		for k, vv := range x {
			rv, err := resolveValue(vv, ctx)
			if err != nil {
				return nil, err
			}
			m[k] = rv
		}
		return m, nil
	case []interface{}:
		s := make([]interface{}, len(x))
		for i, vv := range x {
			rv, err := resolveValue(vv, ctx)
			if err != nil {
				return nil, err
			}
			s[i] = rv
		}
		return s, nil
	default:
		return v, nil
	}
}

// 模板语法（expandString / lookup / walk / scanTemplateRefs / validateTemplates）见 template.go——
// 解析是一次实现、三方共用（运行期展开 / 依赖推断 / 注册期静态校验），不再放在本文件里
// 以免又长出第二份「什么算引用」的判定。

// validate 用 JSON Schema 校验 instance；schema 为空则跳过。
func validate(schema map[string]interface{}, instance interface{}) error {
	if len(schema) == 0 {
		return nil
	}
	schemaBytes, err := json.Marshal(schema)
	if err != nil {
		return fmt.Errorf("序列化 schema 失败: %w", err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("schema.json", bytes.NewReader(schemaBytes)); err != nil {
		return fmt.Errorf("编译 schema 失败: %w", err)
	}
	sch, err := c.Compile("schema.json")
	if err != nil {
		return fmt.Errorf("编译 schema 失败: %w", err)
	}
	if err := sch.Validate(instance); err != nil {
		return fmt.Errorf("%w", err)
	}
	return nil
}
