package engine

import (
	"strings"
	"sync"
	"testing"

	"gear/internal/model"
)

// ---- 订阅机制端到端测试 ----
//
// 覆盖通用订阅原语（ctx.subscribe）的核心链路：
//   - Lua 侧 ctx.subscribe.start/stop 绑定（经真实沙箱执行）
//   - Source 工厂构造（handle/callbackURL 回推到位）
//   - 事件到达 → Subscriber.Deliver → deliverTo(run) → 重调度目标 Tool
//   - 生命周期：run 结束 StopByOwner 按 Flow 归属回收
//   - mqtt 源把 topic/qos/handle/callback_url 透传给 sidecar RPC
//
// 不依赖真实 MQTT broker / 网络：事件源用 fake 注入，事件经 fake 的 sink 触达 Subscriber。

// fakeSourceState 记录 fake 源收到的构造/启动信息，供断言与手动触发投递。
type fakeSourceState struct {
	mu          sync.Mutex
	cfg         map[string]interface{}
	callbackURL string
	handle      string
	sink        func(SubscriptionEvent)
	stopCalls   int
}

// newFakeSourceFactory 返回一个「记录型」源工厂 + 其状态句柄。
func newFakeSourceFactory() (*fakeSourceState, SourceFactory) {
	st := &fakeSourceState{}
	return st, func(cfg map[string]interface{}, callbackURL, handle string) (SubscribeSource, error) {
		st.mu.Lock()
		st.cfg = cfg
		st.callbackURL = callbackURL
		st.handle = handle
		st.mu.Unlock()
		return &fakeSource{st: st}, nil
	}
}

type fakeSource struct{ st *fakeSourceState }

func (f *fakeSource) Start(sink func(SubscriptionEvent)) (func(), error) {
	f.st.mu.Lock()
	f.st.sink = sink
	f.st.mu.Unlock()
	return func() {
		f.st.mu.Lock()
		f.st.stopCalls++
		f.st.mu.Unlock()
	}, nil
}

// fire 用捕获到的 sink 模拟一条事件到达（走 Source→Subscriber→Deliver 全链路）。
func (f *fakeSourceState) fire(ev SubscriptionEvent) {
	f.mu.Lock()
	sink := f.sink
	f.mu.Unlock()
	if sink != nil {
		sink(ev)
	}
}

func (f *fakeSourceState) snapshot() (map[string]interface{}, string, string, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cfg, f.callbackURL, f.handle, f.stopCalls
}

// TestSubscribeDeliverRunsTarget：端到端跑通「Lua start → 源登记 → 事件到达 → 投递目标 Tool → stop」。
func TestSubscribeDeliverRunsTarget(t *testing.T) {
	reg := mustOpenReg(t)
	defer reg.Close()
	eng := New(reg)

	eng.SetSubscribeCallbackBase("http://127.0.0.1:8787")
	st, factory := newFakeSourceFactory()
	eng.Subscriber().RegisterSource("fake", factory)

	// 投递目标：sink 经 event 总线广播观察到的输入（观察副作用的可靠通道）。
	putTool(t, reg, luaToolWithCaps("sink", []string{"event"}, `
		ctx.event.notify("sink_payload", { payload = ctx.input.payload, extra = ctx.input.extra })
		ctx.output = { ok = true }
	`))
	// 发起订阅的 Tool：声明 subscribe 能力，返回 handle。
	putTool(t, reg, luaToolWithCaps("sub_tool", []string{"subscribe"}, `
		local h = ctx.subscribe.start({ source = "fake", config = { topic = "sensors/temp" }, callback = { type = "run", target = "sink", input = { extra = "x" } } })
		ctx.output = { handle = h }
	`))

	out, err := eng.RunToolExec("sub_tool", nil, "")
	if err != nil {
		t.Fatalf("发起订阅失败: %v", err)
	}
	handle, ok := out["handle"].(string)
	if !ok || handle == "" {
		t.Fatalf("subscribe.start 应返回非空 handle，实际 %#v", out)
	}

	// 源被正确构造（handle/callbackURL 回推到位）。
	cfg, cbURL, srcHandle, _ := st.snapshot()
	if cfg["topic"] != "sensors/temp" {
		t.Fatalf("config.topic 应透传给源，实际 %#v", cfg)
	}
	if srcHandle != handle {
		t.Fatalf("源收到的 handle = %q，期望 %q", srcHandle, handle)
	}
	if cbURL != "http://127.0.0.1:8787/api/events/"+handle {
		t.Fatalf("回推 URL = %q，期望 http://127.0.0.1:8787/api/events/%s", cbURL, handle)
	}

	// 事件到达 → Deliver → deliverTo(run) → 重调度 sink。
	st.fire(SubscriptionEvent{Source: "fake", Handle: handle, Payload: "hello"})
	p, err := eng.host.Event.Wait("sink_payload")
	if err != nil {
		t.Fatalf("投递后未收到 sink_payload 事件: %v", err)
	}
	m, ok := p.(map[string]interface{})
	if !ok || m["payload"] != "hello" || m["extra"] != "x" {
		t.Fatalf("投递后 payload/extra 异常（callback.input 模板与 payload 合并），实际 %#v", p)
	}

	// stop 取消订阅；再次投递应报「不存在」。
	if err := eng.Subscriber().Stop(handle); err != nil {
		t.Fatalf("Stop 应成功: %v", err)
	}
	if _, _, _, n := st.snapshot(); n != 1 {
		t.Fatalf("Stop 应调用源 stop 一次，实际 %d", n)
	}
	if err := eng.Deliver(handle, "again"); err == nil || !strings.Contains(err.Error(), "不存在或已回收") {
		t.Fatalf("停止后投递应报「不存在或已回收」，实际 %v", err)
	}
}

// TestSubscribeStartGatesOnCallbackBase：未 serve 监听（无回推基址）时 start 必须报明确错误，
// 而非静默 no-op（消费回调仅 serve/监管模式可用）。
func TestSubscribeStartGatesOnCallbackBase(t *testing.T) {
	reg := mustOpenReg(t)
	defer reg.Close()
	eng := New(reg) // 未 SetCallbackBase

	eng.Subscriber().RegisterSource("fake", func(map[string]interface{}, string, string) (SubscribeSource, error) {
		return &fakeSource{}, nil
	})
	putTool(t, reg, luaToolWithCaps("sub_tool", []string{"subscribe"}, `
		local h = ctx.subscribe.start({ source = "fake", config = {}, callback = { target = "x" } })
		ctx.output = { handle = h }
	`))

	if _, err := eng.RunToolExec("sub_tool", nil, ""); err == nil || !strings.Contains(err.Error(), "需引擎监听") {
		t.Fatalf("无回推基址时 subscribe.start 应报「需引擎监听」，实际 %v", err)
	}
}

// TestSubscribeUnknownSource：源未登记时 start 报「未知的事件源」并列出可用源。
func TestSubscribeUnknownSource(t *testing.T) {
	reg := mustOpenReg(t)
	defer reg.Close()
	eng := New(reg)
	eng.SetSubscribeCallbackBase("http://127.0.0.1:8787")

	putTool(t, reg, luaToolWithCaps("sub_tool", []string{"subscribe"}, `
		local h = ctx.subscribe.start({ source = "nope", config = {}, callback = { target = "x" } })
		ctx.output = { handle = h }
	`))

	if _, err := eng.RunToolExec("sub_tool", nil, ""); err == nil || !strings.Contains(err.Error(), "未知的事件源") {
		t.Fatalf("未登记源应报「未知的事件源」，实际 %v", err)
	}
}

// TestSubscribeOwnerCleanupAfterFlow：flow 运行结束，runWithVars 经 StopByOwner 按 Flow 归属回收
// 本次运行建立的订阅（复用 Sessions 引用计数思路），随后投递报「不存在」。
func TestSubscribeOwnerCleanupAfterFlow(t *testing.T) {
	reg := mustOpenReg(t)
	defer reg.Close()
	eng := New(reg)
	eng.SetSubscribeCallbackBase("http://127.0.0.1:8787")

	_, factory := newFakeSourceFactory()
	eng.Subscriber().RegisterSource("fake", factory)

	putTool(t, reg, luaToolWithCaps("sub_tool", []string{"subscribe"}, `
		local h = ctx.subscribe.start({ source = "fake", config = { topic = "t" }, callback = { target = "sink" } })
		ctx.output = { handle = h }
	`))
	putFlow(t, reg, &model.Flow{ID: "flow_sub", Steps: []model.Step{{ID: "s1", Call: "sub_tool"}}})

	res, err := eng.RunFlowResult("flow_sub")
	if err != nil {
		t.Fatalf("flow 运行失败: %v", err)
	}
	s1Out, ok := res.Steps["s1"]["output"].(map[string]interface{})
	if !ok {
		t.Fatalf("s1 应有 output，实际 %#v", res.Steps["s1"])
	}
	handle, _ := s1Out["handle"].(string)
	if handle == "" {
		t.Fatalf("flow 应产生非空 handle，实际 %#v", s1Out)
	}

	if err := eng.Deliver(handle, "x"); err == nil || !strings.Contains(err.Error(), "不存在或已回收") {
		t.Fatalf("flow 结束后订阅应被 StopByOwner 回收，投递应报错，实际 %v", err)
	}
}

// ---- mqtt 源：RPC 接线（topic/qos/handle/callback_url 透传） ----

type recordingCall struct {
	method string
	args   map[string]interface{}
}

type recordingTransport struct {
	mu    sync.Mutex
	calls []recordingCall
}

func (f *recordingTransport) Call(method string, args map[string]interface{}) (interface{}, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := map[string]interface{}{}
	for k, v := range args {
		cp[k] = v
	}
	f.calls = append(f.calls, recordingCall{method: method, args: cp})
	return nil, nil
}

func (f *recordingTransport) snapshot() []recordingCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]recordingCall, len(f.calls))
	copy(out, f.calls)
	return out
}

// TestMQTTSourcePassesCallbackThroughRPC：mqtt 源的 Start/stop 把订阅请求转发为 sidecar
// 的 subscribe/unsubscribe RPC，携带 topic/qos/handle/callback_url。
func TestMQTTSourcePassesCallbackThroughRPC(t *testing.T) {
	tr := &recordingTransport{}
	src, err := newMQTTSource(tr)(map[string]interface{}{"topic": "a/b", "qos": 1},
		"http://127.0.0.1:8787/api/events/sub_9", "sub_9")
	if err != nil {
		t.Fatalf("mqtt 源工厂构造失败: %v", err)
	}
	stop, err := src.Start(nil)
	if err != nil {
		t.Fatalf("mqtt 源 Start 失败: %v", err)
	}

	calls := tr.snapshot()
	if len(calls) != 1 || calls[0].method != "subscribe" {
		t.Fatalf("Start 应发起一次 subscribe RPC，实际 %#v", calls)
	}
	args := calls[0].args
	if args["topic"] != "a/b" || args["handle"] != "sub_9" ||
		args["callback_url"] != "http://127.0.0.1:8787/api/events/sub_9" {
		t.Fatalf("subscribe RPC 应透传 topic/handle/callback_url，实际 %#v", args)
	}
	if v, ok := args["qos"].(int); !ok || v != 1 {
		t.Fatalf("qos 应为 int(1)，实际 %#v(%T)", args["qos"], args["qos"])
	}

	stop()
	calls = tr.snapshot()
	if len(calls) != 2 || calls[1].method != "unsubscribe" || calls[1].args["handle"] != "sub_9" {
		t.Fatalf("stop 应发起一次 unsubscribe RPC 且带 handle，实际 %#v", calls)
	}
}

// TestMQTTSourceRequiresTopic：缺 config.topic 时 mqtt 源 Start 报明确错误。
func TestMQTTSourceRequiresTopic(t *testing.T) {
	tr := &recordingTransport{}
	src, _ := newMQTTSource(tr)(map[string]interface{}{}, "http://x/api/events/s", "s")
	if _, err := src.Start(nil); err == nil || !strings.Contains(err.Error(), "config.topic") {
		t.Fatalf("缺 topic 应报含 config.topic 的错误，实际 %v", err)
	}
}
