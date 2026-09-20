package engine

// subscriber.go 实现通用订阅机制（与 ctx.event 平级、互不依赖的新运行期原语）。
//
// 设计要点（用户已批准方案）：
//   - 订阅 = Source（事件源抽象）+ Deliver（投递动作）+ Subscription（handle+owner+callback）。
//   - Subscriber 管理器持有一份 handle -> Subscription 映射，提供 Start/Stop/StopByOwner。
//   - 回调 URL 由 gear 自身回推（--addr 拼 /api/events/{handle}），sidecar 维护
//     handle -> callbackURL 映射 —— 不再从 env 读单一 GEAR_WEBHOOK（多 gear 并行会串扰）。
//   - 生命周期归属：ctx.subscribe.start 请求带 Owner（引擎在 hostFor/primitiveHost 写入），
//     运行结束 runWithVars 经 StopByOwner 按 Flow/Exec 回收（复用 Sessions 引用计数思路）。
//
// 消费回调仅 serve/监管模式可用（需引擎监听 HTTP 回推端点）；flow run / tool run
// 一次性执行无 server 监听，Start 返回明确错误而非静默 no-op。

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"gear/internal/cap"
)

// SubscribeSource 是「事件源」的最小抽象：Start 建立订阅并注册 sink，返回一个 stop 函数。
// sink 在事件到达时被调用（payload 为事件负载）；stop 用于取消订阅（幂等）。
type SubscribeSource interface {
	Start(sink func(ev SubscriptionEvent)) (stop func(), err error)
}

// SubscriptionEvent 是事件源投递给订阅管理器的一条事件。
type SubscriptionEvent struct {
	Source  string      // 事件源名（如 "mqtt"）
	Handle  string      // 订阅句柄
	Payload interface{} // 事件负载
}

// SourceFactory 按「源名 -> 实现」构造事件源。cfg 是源专属配置（mqtt: {topic, qos}），
// callbackURL 是本订阅的回推地址（=<base>/api/events/<handle>），handle 是订阅句柄。
type SourceFactory func(cfg map[string]interface{}, callbackURL, handle string) (SubscribeSource, error)

// Subscription 是一条已建立的订阅：Handle 是唯一句柄，Owner 是发起订阅的执行归属，
// Callback 是事件到达后的投递动作，stop 是取消订阅的回调（幂等）。
type Subscription struct {
	Handle   string
	Owner    cap.SubscriptionOwner
	Callback cap.SubscriptionCallback
	stop     func()
}

// Subscriber 是通用订阅管理器（engine 层，实现 cap.Subscribe）。
type Subscriber struct {
	mu sync.Mutex

	e   *Engine
	seq int64

	callbackBase string                   // gear 自身回推基址（serve 监听时由 server 注入）
	sources      map[string]SourceFactory // 源名 -> 源工厂
	subs         map[string]*Subscription // handle -> 订阅
}

// newSubscriber 构造订阅管理器，并从扩展目录拷贝已登记的源工厂。
func newSubscriber(e *Engine) *Subscriber {
	s := &Subscriber{
		e:       e,
		sources: map[string]SourceFactory{},
		subs:    map[string]*Subscription{},
	}
	e.capReg.mu.Lock()
	for name, f := range e.capReg.Sources {
		s.sources[name] = f
	}
	e.capReg.mu.Unlock()
	return s
}

// SetCallbackBase 注入 gear 自身回推基址（serve 监听时由 server 调用）。
func (s *Subscriber) SetCallbackBase(base string) {
	s.mu.Lock()
	s.callbackBase = strings.TrimRight(base, "/")
	s.mu.Unlock()
}

// RegisterSource 登记一个事件源工厂（启动期由 SetupExtensions 经 CapabilityRegistry 写入）。
func (s *Subscriber) RegisterSource(name string, f SourceFactory) {
	s.mu.Lock()
	s.sources[name] = f
	s.mu.Unlock()
}

// Start 建立一条订阅。要求：引擎已监听回推端点（callbackBase 非空）、source 已登记。
// 返回订阅句柄，供 ctx.subscribe.stop(handle) 或运行结束经 StopByOwner 回收。
func (s *Subscriber) Start(req cap.SubscriptionRequest) (string, error) {
	s.mu.Lock()
	base := s.callbackBase
	factory, ok := s.sources[req.Source]
	s.mu.Unlock()
	if base == "" {
		return "", fmt.Errorf("subscribe 需引擎监听 HTTP 回推端点（serve/监管模式）；flow run / tool run 一次性执行无 server 监听")
	}
	if !ok {
		return "", fmt.Errorf("未知的事件源 %q（可用：%s）", req.Source, s.sourceNames())
	}

	handle := s.nextHandle()
	callbackURL := base + "/api/events/" + handle
	src, err := factory(req.Config, callbackURL, handle)
	if err != nil {
		return "", err
	}
	stop, err := src.Start(func(ev SubscriptionEvent) {
		// sink 路径目前**无任何内置源使用**：mqtt/kafka/rabbitmq/webhook 四个源全部由侧车
		// POST callback_url 回推，经 server 的 /api/events/{handle} 投给 Deliver。
		// 保留 sink 是给未来「进程内事件源」的入口；该回调签名无 error，投递失败在此无处上报，
		// 故显式忽略——真投递路径的失败由 HTTP 回推的响应码反馈（见本文件顶部说明）。
		_ = s.Deliver(ev.Handle, ev.Payload)
	})
	if err != nil {
		return "", err
	}

	s.mu.Lock()
	s.subs[handle] = &Subscription{Handle: handle, Owner: req.Owner, Callback: req.Callback, stop: stop}
	s.mu.Unlock()
	return handle, nil
}

// Stop 按句柄取消订阅。
func (s *Subscriber) Stop(handle string) error {
	s.mu.Lock()
	sub, ok := s.subs[handle]
	if ok {
		delete(s.subs, handle)
	}
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("订阅句柄 %q 不存在", handle)
	}
	if sub.stop != nil {
		sub.stop()
	}
	return nil
}

// StopByOwner 按执行归属回收订阅（运行结束 defer 调用）。
// ownerMatch：FlowID 必须相等；ExecID 任一方为空则只按 FlowID 匹配，否则须精确相等。
func (s *Subscriber) StopByOwner(owner cap.SubscriptionOwner) {
	s.mu.Lock()
	var toStop []*Subscription
	for h, sub := range s.subs {
		if ownerMatch(owner, sub.Owner) {
			toStop = append(toStop, sub)
			delete(s.subs, h)
		}
	}
	s.mu.Unlock()
	for _, sub := range toStop {
		if sub.stop != nil {
			sub.stop()
		}
	}
}

// ownerMatch 报告 a 是否覆盖 b 的归属（匹配口径见 StopByOwner）。
func ownerMatch(a, b cap.SubscriptionOwner) bool {
	if a.FlowID != b.FlowID {
		return false
	}
	if a.ExecID != "" && b.ExecID != "" {
		return a.ExecID == b.ExecID
	}
	return true
}

// Deliver 把一条事件投递给指定句柄的订阅（server 回推端点调用）。
func (s *Subscriber) Deliver(handle string, payload interface{}) error {
	s.mu.Lock()
	sub, ok := s.subs[handle]
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("订阅句柄 %q 不存在或已回收", handle)
	}
	return s.deliverTo(sub.Callback, payload)
}

// deliverTo 按投递动作分发：run（重调度 Tool/Flow）或 notify（触发 event 通知）。
func (s *Subscriber) deliverTo(cb cap.SubscriptionCallback, payload interface{}) error {
	switch cb.Type {
	case "", "run":
		return s.runTarget(cb, payload)
	case "notify":
		if s.e.host == nil || s.e.host.Event == nil {
			return fmt.Errorf("notify 投递需宿主提供 event 原语")
		}
		s.e.host.Event.Notify(cb.Target, payload)
		return nil
	default:
		return fmt.Errorf("未知的投递类型 %q（合法：run|notify）", cb.Type)
	}
}

// runTarget 以事件负载作为输入重调度一个 Tool 或 Flow（先 Flow 后 Tool）。
// 输入 = cb.Input 模板 + input["payload"] = 事件负载。
func (s *Subscriber) runTarget(cb cap.SubscriptionCallback, payload interface{}) error {
	input := map[string]interface{}{}
	for k, v := range cb.Input {
		input[k] = v
	}
	input["payload"] = payload

	if flow, err := s.e.reg.GetFlow(cb.Target); err == nil && flow != nil {
		_, err := s.e.RunFlowExecWithVars(cb.Target, "", nil, input)
		return err
	}
	if tool, err := s.e.reg.GetTool(cb.Target); err == nil && tool != nil {
		_, err := s.e.RunToolExec(cb.Target, input, "")
		return err
	}
	return fmt.Errorf("投递目标 %q 既不是已注册 Tool 也不是 Flow", cb.Target)
}

func (s *Subscriber) nextHandle() string {
	n := atomic.AddInt64(&s.seq, 1)
	return fmt.Sprintf("sub_%d", n)
}

func (s *Subscriber) sourceNames() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	names := make([]string, 0, len(s.sources))
	for n := range s.sources {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, "/")
}

// ---- mqtt 事件源（首批实现） ----

// mqttSource 把 mqtt-sidecar 的 subscribe/unsubscribe RPC 包成 SubscribeSource。
// 收到消息时 sidecar 直接 POST 到 callbackURL（/<handle>），故 Start 的 sink 仅占位不用。
type mqttSource struct {
	t           SidecarTransport
	cfg         map[string]interface{}
	callbackURL string
	handle      string
}

// newMQTTSource 返回 mqtt 源工厂（SetupExtensions 在 ext.mqtt 声明 source 时登记）。
func newMQTTSource(t SidecarTransport) SourceFactory {
	return func(cfg map[string]interface{}, callbackURL, handle string) (SubscribeSource, error) {
		return &mqttSource{t: t, cfg: cfg, callbackURL: callbackURL, handle: handle}, nil
	}
}

func (m *mqttSource) Start(_ func(SubscriptionEvent)) (func(), error) {
	if m.t == nil {
		return nil, fmt.Errorf("mqtt 事件源未装配 sidecar transport（ext.mqtt 未声明 listen）")
	}
	topic, _ := m.cfg["topic"].(string)
	if topic == "" {
		return nil, fmt.Errorf("mqtt 订阅缺少必填 config.topic")
	}
	args := map[string]interface{}{
		"topic":        topic,
		"qos":          m.qos(),
		"handle":       m.handle,
		"callback_url": m.callbackURL,
	}
	if _, err := m.t.Call("subscribe", args); err != nil {
		return nil, err
	}
	return func() {
		_, _ = m.t.Call("unsubscribe", map[string]interface{}{"handle": m.handle})
	}, nil
}

// qos 从 Lua/JSON 透传的 config.qos 读 QoS（float64/int64/int，缺省 0）。
func (m *mqttSource) qos() int {
	switch v := m.cfg["qos"].(type) {
	case float64:
		return int(v)
	case int64:
		return int(v)
	case int:
		return v
	default:
		return 0
	}
}
