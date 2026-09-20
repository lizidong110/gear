package engine

// source.go 把 sidecar 的订阅 RPC 包成通用「事件源工厂」（SourceFactory）。
//
// 背景：service ext 只要在 extensions.yaml 里声明了 `source: <name>`，就承诺能承载同名事件源——
// ctx.subscribe.start({ source = "kafka" | "mqtt" | ... }) 的 source 字段直接对应这条声明。
// 四个侧车（mqtt / kafka / rabbitmq / webhook）的订阅线格式**完全一致**：
//
//	subscribe   { <源专属配置...>, handle, callback_url }   → 建立订阅；事件到达时侧车 POST callback_url
//	unsubscribe { handle }                                  → 取消订阅（幂等）
//
// 故这里只维护一张「源名 → 工厂」表并透传 cfg/handle/callback_url；**未知源名在启动期**
// （SetupExtensions 登记事件源时）就报错——fail-closed：宁可在起服务时暴露 source 名拼错，
// 也不要拖到运行期 ctx.subscribe.start 才失败（那时已经在跑业务 Flow 了）。

import "fmt"

// knownSources 是已实现的通用事件源名（与 plugin/extensions.yaml 的 source 声明一致）。
// 新增一个承载订阅的侧车时：这里是第一处、extensions.yaml 的 source 是第二处，两处都要加。
var knownSources = map[string]bool{
	"mqtt":     true,
	"kafka":    true,
	"rabbitmq": true,
	"webhook":  true,
}

// newSourceFactory 按源名构造事件源工厂。t 是该 ext 的侧车调用通道（本地 HTTP 长连接）。
// 未知源名 → 报错（调用方 SetupExtensions 会把它变成一条带 ext 名的启动期错误）。
func newSourceFactory(source string, t SidecarTransport) (SourceFactory, error) {
	if source == "mqtt" {
		// mqtt 有专属校验（必填 config.topic），沿用既有实现，行为与历史测试一致。
		return newMQTTSource(t), nil
	}
	if knownSources[source] {
		return newSidecarSource(source, t), nil
	}
	return nil, fmt.Errorf("未知的事件源 %q（已实现：kafka/mqtt/rabbitmq/webhook）", source)
}

// sidecarSource 是通用事件源：把一次订阅请求透传给侧车的 subscribe / unsubscribe RPC。
//
// 源专属配置（kafka 的 topic/group、rabbitmq 的 queue、webhook 的 path…）原样放进 cfg 交给
// 侧车自己校验——引擎不维护第二份「哪个源要哪些字段」的清单，那种清单必然与侧车实现漂移
// （G2/M10 同款教训：真相源只能有一处，这里的一处是侧车）。
type sidecarSource struct {
	name        string
	t           SidecarTransport
	cfg         map[string]interface{}
	callbackURL string
	handle      string
}

// newSidecarSource 返回某源名的通用工厂（工厂只捕获配置，真正的 RPC 在 Start 时才发）。
func newSidecarSource(name string, t SidecarTransport) SourceFactory {
	return func(cfg map[string]interface{}, callbackURL, handle string) (SubscribeSource, error) {
		return &sidecarSource{name: name, t: t, cfg: cfg, callbackURL: callbackURL, handle: handle}, nil
	}
}

// Start 发一次 subscribe RPC（携带 cfg + handle + callback_url），返回的 stop 发一次
// unsubscribe RPC。sink 不使用：事件由侧车直接 POST 回 callback_url，经 server 的
// /api/events/{handle} 端点投给 Subscriber.Deliver（见 subscriber.go 顶部说明）。
func (s *sidecarSource) Start(_ func(SubscriptionEvent)) (func(), error) {
	if s.t == nil {
		return nil, fmt.Errorf("%s 事件源未装配 sidecar transport（该 ext 未声明 listen）", s.name)
	}
	args := make(map[string]interface{}, len(s.cfg)+2)
	for k, v := range s.cfg {
		args[k] = v
	}
	// handle / callback_url 由引擎生成，放在 cfg 之后写入——同名键以引擎为准，
	// 否则用户配置里一个手滑的 handle 会覆盖真实句柄，订阅就再也回收不掉。
	args["handle"] = s.handle
	args["callback_url"] = s.callbackURL
	if _, err := s.t.Call("subscribe", args); err != nil {
		return nil, err
	}
	return func() {
		// 取消失败无法补救（订阅已退出生命周期），与 newMQTTSource 同口径忽略错误。
		_, _ = s.t.Call("unsubscribe", map[string]interface{}{"handle": s.handle})
	}, nil
}
