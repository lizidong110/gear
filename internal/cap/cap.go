// Package cap 实现 Gear 的宿主能力层（PRD §6 宿主能力原语）。
//
// 宿主（Runtime）只向 Tool 暴露 6 个受 capability 门禁的跨边界通信原语：
// http / call / db / secret / event / subscribe。
// 另有 time / template / crypto 三个**纯函数原子**（内核内建、始终可用、纯计算无外部依赖）——
// 它们刻意**不进**本门禁（见 internal/engine/purefunc.go 与 ATOM_ROADMAP.md §0）。
// 它们处于同一抽象层级——都是「插件与外界交换数据的通道」，不混杂业务。
//
// 安全原则（PRD §6.1）：
//   - 只提供原语，不提供业务（发邮件/接钉钉都是普通 Tool，内部通过 http 实现）。
//   - **capability 精确放行**：未声明的调用一律拒绝，权限遵循最小权限原则。
//   - secret 只读：插件看不到明文存储位置，只能通过 ctx.secret.get 获取。
//
// 设计要点（契合方法论）：
//   - Primitive 枚举**单一来源**（KnownPrimitives），解析点（GrantsFromStrings）拒绝未知值，
//     文档/模型/引擎共用同一份，避免漂移（红线 15：枚举式声明必须逐项登记）。
//   - Context 的每个访问器先查 Grants，未授权即返回 ErrCapabilityDenied；已授权但未提供
//     实现的原语（如 db）返回 ErrCapabilityUnavailable——把「文档说有、代码无」变成可见错误。
package cap

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
)

// Primitive 是宿主能力原语（PRD §6）。枚举单一来源，防漂移（方法论红线 15）。
type Primitive string

const (
	PrimHTTP      Primitive = "http"
	PrimCall      Primitive = "call"
	PrimDB        Primitive = "db"
	PrimSecret    Primitive = "secret"
	PrimEvent     Primitive = "event"
	PrimSubscribe Primitive = "subscribe"
)

// KnownPrimitives 是所有合法 capability 枚举值（单一来源，供校验与文档共用）。
var KnownPrimitives = map[Primitive]bool{
	PrimHTTP:      true,
	PrimCall:      true,
	PrimDB:        true,
	PrimSecret:    true,
	PrimEvent:     true,
	PrimSubscribe: true,
}

// extensionNames 是用户/插件在启动期注册的「扩展原语」名集合（区别于上面 7 个受门禁的内核原语）。
// 仅启动期写入（RegisterExtensionName），运行期只读，无并发；加锁仅为防御性。
var extensionNames = map[Primitive]bool{}

// RegisterExtensionName 由引擎在启动期把用户/插件注册的扩展能力名并入合法 capability 集合。
// 约束：
//   - 扩展名必须以 "ext." 前缀开头，避免与内置原语或未来内置名冲突；
//   - 不能覆盖已有的内置原语名（内置集是引擎受信核心，不得被扩展名遮蔽）。
//
// 注册成功后，GrantsFromStrings 据此放行该名（红线 15「未知枚举即拒」原样保留为
// 「未注册即拒」，只是枚举从写死 7 个变为「7 个 + 启动期注册的名字」）。
func RegisterExtensionName(p Primitive) error {
	if KnownPrimitives[p] {
		return fmt.Errorf("不能覆盖内置原语 %q", p)
	}
	if !strings.HasPrefix(string(p), "ext.") {
		return fmt.Errorf("扩展原语名 %q 必须以 ext. 开头", p)
	}
	extensionNames[p] = true
	return nil
}

// IsBuiltin 报告 p 是否为引擎内置原语（区别于用户注册的扩展原语）。
func IsBuiltin(p Primitive) bool { return KnownPrimitives[p] }

// Known 报告 p 是否为合法枚举值（内核门禁原语 ∪ 启动期注册的扩展名）。
// 注意：纯函数原子（time/template/crypto）**不在**此列——它们无需声明即始终可用，
// 因此把它们写进 Tool.Capabilities 会被 GrantsFromStrings 拒绝（口径见 purefunc.go）。
func (p Primitive) Known() bool { return KnownPrimitives[p] || extensionNames[p] }

// 语义化错误。
var (
	// ErrCapabilityDenied 表示 Tool 未声明该 capability（最小权限，拒绝）。
	ErrCapabilityDenied = errors.New("capability 未授权")
	// ErrCapabilityUnavailable 表示已授权但该原语当前未提供实现（如 db，留待后续）。
	ErrCapabilityUnavailable = errors.New("capability 已授权但尚未实现")
	// ErrSecretNotFound 表示请求的名称在 secret 源中不存在。
	ErrSecretNotFound = errors.New("secret 不存在")
)

// Grants 是某 Tool 被允许的 capability 集合（最小权限）。
type Grants map[Primitive]bool

// GrantsFromStrings 由 Tool 声明的 capability 字符串列表构建 Grants，遇到未知值立即报错
// （红线 15：解析点必须拒绝未知枚举，而非静默忽略）。
func GrantsFromStrings(ss []string) (Grants, error) {
	g := Grants{}
	for _, s := range ss {
		p := Primitive(s)
		if !p.Known() {
			return nil, fmt.Errorf("未知的 capability %q（合法值：内核受门禁原语 http/call/db/secret/event/subscribe，或已注册的 ext.* 扩展名；time/template/crypto 是纯函数原子，始终可用、无需声明）", s)
		}
		g[p] = true
	}
	return g, nil
}

// Has 报告是否授予了某原语。
func (g Grants) Has(p Primitive) bool { return g[p] }

// Capability 接口：各原语对 Tool 暴露的最小 API。

// Secret 只读密钥读取。
type Secret interface {
	Get(name string) (string, error)
}

// Response 是 HTTP 原语的原始响应。
type Response struct {
	StatusCode int
	Body       []byte
	Headers    map[string]string
}

// HTTP 任意 HTTP 请求（传输层）。
type HTTP interface {
	Request(method, url string, body []byte, headers map[string]string) (*Response, error)
}

// Event 异步事件通知 / 等待。
type Event interface {
	Notify(name string, payload interface{})
	Wait(name string) (interface{}, error)
}

// SubscriptionOwner 标识发起订阅的执行单元（Flow / Exec / Step），供引擎在运行结束时
// 按归属回收订阅（与订阅自身的生命周期管理思路同源）。
type SubscriptionOwner struct {
	FlowID string // 所属 Flow id
	ExecID string // 所属 Exec id（serve 模式下每次执行唯一）
	StepID string // 发起订阅的 Step id（可选）
}

// SubscriptionCallback 描述事件到达后的投递动作（可插拔，与 Source 解耦）：
//   - Type="run"：重调度 Target（Tool 或 Flow id），Input 为模板（与事件负载合并后作为输入）。
//   - Type="notify"：触发 event 通知，Target 为事件名。
type SubscriptionCallback struct {
	Type   string                 // run | notify（空等价于 run）
	Target string                 // run: tool/flow id；notify: event name
	Input  map[string]interface{} // run: 输入模板
}

// SubscriptionRequest 是「来自 Tool 的一次订阅声明」（经 ctx.subscribe.start 传入）。
// Source 指定事件源（首批 mqtt），Config 是事件源专属配置（mqtt: {topic, qos}），
// Callback 是事件到达后的投递动作，Owner 由引擎写回（发起订阅的执行单元，供回收）。
type SubscriptionRequest struct {
	Source   string                 `json:"source"`
	Config   map[string]interface{} `json:"config"`
	Callback SubscriptionCallback   `json:"callback"`
	Owner    SubscriptionOwner      `json:"owner,omitempty"`
}

// Subscribe 通用订阅原语（与 Event 平级）：Start 建立订阅返回句柄，Stop 按句柄取消。
// 消费回调仅在 serve/监管模式可用（需 engine 监听 HTTP 回推端点）；flow run / tool run
// 一次性执行无 server 监听，Start 返回明确错误而非静默 no-op。
type Subscribe interface {
	Start(req SubscriptionRequest) (string, error)
	Stop(handle string) error
}

// Call 调用另一个 Tool / Flow（唯一插件间通信通道）。
type Call interface {
	Call(target string, input map[string]interface{}) (map[string]interface{}, error)
}

// DB 结构化数据读写（核心原子，多后端：sqlite / mysql / postgres）。
// Query/Exec 的签名带 driver/dsn：driver 决定后端（sqlite/mysql/postgres，见 sqldb.go
// normDriver 别名表），dsn 决定具体库（sqlite 文件或内存 DSN、mysql/postgres 连接串）。
// dsn 缺省且 driver=sqlite 时回退引擎注入的 defaultSQLiteDSN（每引擎唯一内存 scratch 库）。
// 返回 []interface{}（元素为 map[string]interface{}）：既能直接 JSON 序列化，也能被
// engine 的 toLuaValue 正确转成 Lua 数组（命中 []interface{} 分支），避免被当成字符串。
type DB interface {
	Query(driver, dsn, query string, args ...interface{}) ([]interface{}, error)
	Exec(driver, dsn, query string, args ...interface{}) error
	Ping(driver, dsn string) error
}

// Resolver 是 Call 原语背后的委托：由引擎注入，按 target 执行一个 Tool / Flow。
// via 是调用方的执行链（祖先 Flow id 列表，含当前 Flow），供引擎做循环调用守卫
// （A→B→A 无限递归），与 Hook 内联子 Flow 的祖先链守卫复用同一套语义（见 engine #72）。
type Resolver func(target string, input map[string]interface{}, via []string) (map[string]interface{}, error)

// Providers 汇集各原语的具体实现，由宿主（Engine）一次性注入。
type Providers struct {
	Secret   Secret
	HTTP     *http.Client
	Event    Event
	Resolver Resolver
	DB       DB // 内置多后端 DB 实现（cap.NewMultiDB）；nil 表示未提供（已授权时返回 ErrCapabilityUnavailable）
	// Subscribe 是通用订阅原语实现（引擎装配；与 Event 平级）。nil 表示未提供，
	// 声明了 subscribe 的 Tool 运行期收到 ErrCapabilityUnavailable（fail-closed）。
	Subscribe Subscribe
}

// Context 是一次 Tool 执行可见的宿主能力视图。它持有该 Tool 的 Grants，
// 每个访问器先查授权，再返回对应原语——未授权即拒（最小权限）。
type Context struct {
	grants  Grants
	prov    *Providers
	sandbox bool
	// exts 是已注入的「扩展原语」名 -> Provider 返回的原生实现对象（由引擎在构造时填入）。
	// Tool 须声明对应 ext.* 才能取到（最小权限）；未声明或已声明但未注入均经 Extension() 报错。
	exts map[string]interface{}
	// via 是本次执行的调用链（祖先 Flow id，含当前 Flow）。经 ctx.call 透传给 Resolver，
	// 供引擎检测循环调用（A→B→A）。空表示非 Flow 内执行（如顶级 flow run / call 入口）。
	via []string
	// owner 是发起本次执行的订阅归属（Flow/Exec/Step），引擎在构建宿主视图时写入。
	// Subscribe() 据此把 owner 绑定进 Subscribe.Start 的请求，供引擎在运行结束时回收订阅。
	owner SubscriptionOwner
}

// SetCallChain 写入本次执行的调用链（引擎在构建宿主视图时调用）。
func (c *Context) SetCallChain(via []string) { c.via = via }

// SetSubscriptionOwner 写入本次执行的订阅归属（引擎在构建宿主视图时调用）。
func (c *Context) SetSubscriptionOwner(o SubscriptionOwner) { c.owner = o }

// New 构造一次执行的宿主能力视图。grants 来自 Tool.Capabilities；prov 来自宿主。
func New(grants Grants, prov *Providers) *Context {
	if grants == nil {
		grants = Grants{}
	}
	return &Context{grants: grants, prov: prov}
}

// Capabilities 返回该 Tool 已声明的原语名列表（供注入到 process 的 input.ctx）。
func (c *Context) Capabilities() []string {
	out := make([]string, 0, len(c.grants))
	for p := range c.grants {
		out = append(out, string(p))
	}
	return out
}

// Extension 取回一个已声明的「扩展原语」的原生实现对象（供 go-host Tool 经 host.Extension(name)
// 使用）。未声明该扩展名 → ErrCapabilityDenied；已声明但引擎未注入实现 → ErrCapabilityUnavailable
// （与内置原语「授权但未实现」语义一致，把「文档说有、代码无」变成可见错误）。
func (c *Context) Extension(name string) (interface{}, error) {
	if !c.grants.Has(Primitive(name)) {
		return nil, ErrCapabilityDenied
	}
	v, ok := c.exts[name]
	if !ok {
		return nil, ErrCapabilityUnavailable
	}
	return v, nil
}

// SetExtension 由引擎在构造宿主视图时注入一个扩展原语的原生实现对象。
func (c *Context) SetExtension(name string, v interface{}) {
	if c.exts == nil {
		c.exts = map[string]interface{}{}
	}
	c.exts[name] = v
}

// Sandbox 报告是否处于沙箱模式（宿主可据此进一步收紧网络/fs 等）。
func (c *Context) Sandbox() bool { return c.sandbox }

// Secret 返回只读密钥原语；未授权则 ErrCapabilityDenied。
func (c *Context) Secret() (Secret, error) {
	if !c.grants.Has(PrimSecret) {
		return nil, ErrCapabilityDenied
	}
	if c.prov == nil || c.prov.Secret == nil {
		return nil, ErrCapabilityUnavailable
	}
	return c.prov.Secret, nil
}

// HTTP 返回 HTTP 原语；未授权则 ErrCapabilityDenied；宿主未注入 Providers 则 ErrCapabilityUnavailable
// （绝不静默回退到默认 client 去访问公网——没有宿主即没有网络能力，最小权限）。
func (c *Context) HTTP() (HTTP, error) {
	if !c.grants.Has(PrimHTTP) {
		return nil, ErrCapabilityDenied
	}
	if c.prov == nil {
		return nil, ErrCapabilityUnavailable
	}
	cli := c.prov.HTTP
	if cli == nil {
		cli = http.DefaultClient
	}
	return httpCap{client: cli}, nil
}

// Event 返回事件原语；未授权则 ErrCapabilityDenied；未提供则 ErrCapabilityUnavailable。
func (c *Context) Event() (Event, error) {
	if !c.grants.Has(PrimEvent) {
		return nil, ErrCapabilityDenied
	}
	if c.prov == nil || c.prov.Event == nil {
		return nil, ErrCapabilityUnavailable
	}
	return c.prov.Event, nil
}

// Subscribe 返回订阅原语；未授权则 ErrCapabilityDenied；未提供则 ErrCapabilityUnavailable。
// 返回的绑定会把本次执行的订阅归属（owner）写入 Start 请求，供引擎在运行结束时回收订阅。
func (c *Context) Subscribe() (Subscribe, error) {
	if !c.grants.Has(PrimSubscribe) {
		return nil, ErrCapabilityDenied
	}
	if c.prov == nil || c.prov.Subscribe == nil {
		return nil, ErrCapabilityUnavailable
	}
	return subscribeBinding{sub: c.prov.Subscribe, owner: c.owner}, nil
}

// subscribeBinding 把执行归属绑定进 Subscribe.Start（Stop 按句柄，无需归属）。
type subscribeBinding struct {
	sub   Subscribe
	owner SubscriptionOwner
}

func (b subscribeBinding) Start(req SubscriptionRequest) (string, error) {
	req.Owner = b.owner
	return b.sub.Start(req)
}

func (b subscribeBinding) Stop(handle string) error { return b.sub.Stop(handle) }

// Call 返回调用原语；未授权则 ErrCapabilityDenied；未注入 Resolver 则 ErrCapabilityUnavailable。
// 调用链 via 一并透传给 Resolver，供引擎循环调用守卫。
func (c *Context) Call() (Call, error) {
	if !c.grants.Has(PrimCall) {
		return nil, ErrCapabilityDenied
	}
	if c.prov == nil || c.prov.Resolver == nil {
		return nil, ErrCapabilityUnavailable
	}
	return resolverCall{fn: c.prov.Resolver, via: c.via}, nil
}

// DB 返回结构化数据原语；未授权则 ErrCapabilityDenied；未提供实现则 ErrCapabilityUnavailable。
func (c *Context) DB() (DB, error) {
	if !c.grants.Has(PrimDB) {
		return nil, ErrCapabilityDenied
	}
	if c.prov == nil || c.prov.DB == nil {
		return nil, ErrCapabilityUnavailable
	}
	return c.prov.DB, nil
}

// ---- 具体实现 ----

// httpCap 是 HTTP 原语的标准库实现。
type httpCap struct{ client *http.Client }

func (h httpCap) Request(method, url string, body []byte, headers map[string]string) (*Response, error) {
	var rdr io.Reader
	if len(body) > 0 {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	hmap := make(map[string]string, len(resp.Header))
	for k := range resp.Header {
		hmap[k] = resp.Header.Get(k)
	}
	return &Response{StatusCode: resp.StatusCode, Body: respBody, Headers: hmap}, nil
}

// resolverCall 把 Resolver 适配成 Call 接口，并携带调用链 via。
type resolverCall struct {
	fn  Resolver
	via []string
}

func (r resolverCall) Call(target string, input map[string]interface{}) (map[string]interface{}, error) {
	return r.fn(target, input, r.via)
}

// MapSecret 是只读密钥源（来自宿主配置，如环境变量注入的 map）。
type MapSecret map[string]string

// Get 按名读取密钥；不存在返回 ErrSecretNotFound（secret 只读，看不到存储位置）。
func (s MapSecret) Get(name string) (string, error) {
	v, ok := s[name]
	if !ok {
		return "", ErrSecretNotFound
	}
	return v, nil
}

// MemEvent 是进程内的事件总线（一对多 notify / Wait）。
type MemEvent struct {
	mu    sync.Mutex
	chans map[string]chan interface{}
}

// NewMemEvent 构造空的内存事件总线。
func NewMemEvent() *MemEvent { return &MemEvent{chans: make(map[string]chan interface{})} }

// Notify 向事件名广播一个负载（新建或复用该事件的 channel）。
func (e *MemEvent) Notify(name string, payload interface{}) {
	e.mu.Lock()
	ch, ok := e.chans[name]
	if !ok {
		ch = make(chan interface{}, 16)
		e.chans[name] = ch
	}
	e.mu.Unlock()
	// 非阻塞发送：满则丢弃（事件为尽力通知，不阻塞生产方）。
	select {
	case ch <- payload:
	default:
	}
}

// Wait 等待某事件名下一次通知（同步，直到有负载或 ctx 取消）。
func (e *MemEvent) Wait(name string) (interface{}, error) {
	e.mu.Lock()
	ch, ok := e.chans[name]
	if !ok {
		ch = make(chan interface{}, 16)
		e.chans[name] = ch
	}
	e.mu.Unlock()
	select {
	case payload := <-ch:
		return payload, nil
	case <-context.Background().Done():
		return nil, context.Background().Err()
	}
}

// WaitCtx 带取消的 Wait（Wait 的无 ctx 版本便于 lua 等简单调用方；需超时控制时用本函数）。
func (e *MemEvent) WaitCtx(name string, ctx context.Context) (interface{}, error) {
	e.mu.Lock()
	ch, ok := e.chans[name]
	if !ok {
		ch = make(chan interface{}, 16)
		e.chans[name] = ch
	}
	e.mu.Unlock()
	select {
	case payload := <-ch:
		return payload, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
