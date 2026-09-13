// Package cap 实现 Gear 的宿主能力层（PRD §6 宿主能力原语）。
//
// 宿主（Runtime）只向 Tool 暴露 6 个跨边界通信原语：http / call / db / kv / secret / event。
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
	"sync"
)

// Primitive 是宿主能力原语（PRD §6）。枚举单一来源，防漂移（方法论红线 15）。
type Primitive string

const (
	PrimHTTP   Primitive = "http"
	PrimCall   Primitive = "call"
	PrimDB     Primitive = "db"
	PrimKV     Primitive = "kv"
	PrimSecret Primitive = "secret"
	PrimEvent  Primitive = "event"
)

// KnownPrimitives 是所有合法 capability 枚举值（单一来源，供校验与文档共用）。
var KnownPrimitives = map[Primitive]bool{
	PrimHTTP:   true,
	PrimCall:   true,
	PrimDB:     true,
	PrimKV:     true,
	PrimSecret: true,
	PrimEvent:  true,
}

// Known 报告 p 是否为合法枚举值。
func (p Primitive) Known() bool { return KnownPrimitives[p] }

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
			return nil, fmt.Errorf("未知的 capability %q（合法值：http/call/db/kv/secret/event）", s)
		}
		g[p] = true
	}
	return g, nil
}

// Has 报告是否授予了某原语。
func (g Grants) Has(p Primitive) bool { return g[p] }

// Capability 接口：各原语对 Tool 暴露的最小 API。

// KV 简单键值存储（配置 / 状态 / 缓存）。
type KV interface {
	Get(key string) (string, bool)
	Put(key, value string)
	Delete(key string)
}

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

// Call 调用另一个 Tool / Flow（唯一插件间通信通道）。
type Call interface {
	Call(target string, input map[string]interface{}) (map[string]interface{}, error)
}

// DB 结构化数据读写（原语已定义，M5 当前未提供实现，留待后续切片）。
// Query 返回 []interface{}（元素为 map[string]interface{}）：既能直接 JSON 序列化，
// 也能被 engine 的 toLuaValue 正确转成 Lua 数组（命中 []interface{} 分支），避免被当成字符串。
type DB interface {
	Query(query string, args ...interface{}) ([]interface{}, error)
	Exec(query string, args ...interface{}) error
}

// Resolver 是 Call 原语背后的委托：由引擎注入，按 target 执行一个 Tool / Flow。
// via 是调用方的执行链（祖先 Flow id 列表，含当前 Flow），供引擎做循环调用守卫
// （A→B→A 无限递归），与 Hook 内联子 Flow 的祖先链守卫复用同一套语义（见 engine #72）。
type Resolver func(target string, input map[string]interface{}, via []string) (map[string]interface{}, error)

// Providers 汇集各原语的具体实现，由宿主（Engine）一次性注入。
type Providers struct {
	KV      KV
	Secret  Secret
	HTTP    *http.Client
	Event   Event
	Resolver Resolver
	DB      DB // 可选；nil 表示未提供（已授权时返回 ErrCapabilityUnavailable）
}

// Context 是一次 Tool 执行可见的宿主能力视图。它持有该 Tool 的 Grants，
// 每个访问器先查授权，再返回对应原语——未授权即拒（最小权限）。
type Context struct {
	grants  Grants
	prov    *Providers
	sandbox bool
	// via 是本次执行的调用链（祖先 Flow id，含当前 Flow）。经 ctx.call 透传给 Resolver，
	// 供引擎检测循环调用（A→B→A）。空表示非 Flow 内执行（如顶级 flow run / call 入口）。
	via []string
}

// SetCallChain 写入本次执行的调用链（引擎在构建宿主视图时调用）。
func (c *Context) SetCallChain(via []string) { c.via = via }

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

// Sandbox 报告是否处于沙箱模式（宿主可据此进一步收紧网络/fs 等）。
func (c *Context) Sandbox() bool { return c.sandbox }

// KV 返回键值存储原语；未授权则 ErrCapabilityDenied。
func (c *Context) KV() (KV, error) {
	if !c.grants.Has(PrimKV) {
		return nil, ErrCapabilityDenied
	}
	if c.prov == nil || c.prov.KV == nil {
		return nil, ErrCapabilityUnavailable
	}
	return c.prov.KV, nil
}

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

// MemKV 是进程内的 KV 实现（配置 / 状态 / 缓存）。
type MemKV struct {
	mu sync.Mutex
	m  map[string]string
}

// NewMemKV 构造空的内存 KV。
func NewMemKV() *MemKV { return &MemKV{m: make(map[string]string)} }

func (k *MemKV) Get(key string) (string, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	v, ok := k.m[key]
	return v, ok
}

func (k *MemKV) Put(key, value string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.m[key] = value
}

func (k *MemKV) Delete(key string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	delete(k.m, key)
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
