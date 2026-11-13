package server

import (
	"sync"
	"time"
)

// LiveLog 是实时推送给订阅者的单条日志（不回放持久化日志；只推订阅后新产生的行）。
type LiveLog struct {
	StepID string `json:"step_id"`
	Stream string `json:"stream"` // stdout | stderr（service 模式 stdout/stderr 合并，统一标 stdout）
	Line   string `json:"line"`
	TS     int64  `json:"ts"`
}

// LogBroker 是执行日志的**内存**发布/订阅中心：引擎的 LogSink 在每行产生时 Publish，
// SSE 端点 Subscribe 后实时取走。刻意不做持久化回放——「实时」语义要求只推订阅后新行，
// 历史行由 GET /api/executions/{id}/logs（SQLite）提供，二者职责分离（见 REQ-API-LOGS-STREAM）。
//
// 红线：Publish 必须非阻塞——订阅者慢/断开时丢弃，绝不反压生产端（引擎执行路径）。
// 因此订阅 channel 有界，满了即丢（diagnostic 日志，丢几行不影响排障）。
type LogBroker struct {
	mu   sync.Mutex
	subs map[string]map[chan LiveLog]struct{}
}

// NewLogBroker 构造空 broker。
func NewLogBroker() *LogBroker {
	return &LogBroker{subs: make(map[string]map[chan LiveLog]struct{})}
}

// Publish 把一行日志推给订阅了 execID 的全部客户端。无订阅者时立即返回（零成本）。
//
// 红线：绝不在持有锁时阻塞——本函数对每路订阅只做「非阻塞」发送（channel 满即丢），
// 因此持锁窗口极短，不会反压引擎执行路径。同时，发送与 unsub 的「删除+关闭 channel」
// 共用同一把锁，且只有仍在 map 中的 channel 才会被发送，从根本上杜绝
// 「向已关闭 channel 发送」的 panic（客户端断连恰与引擎正在发日志并发时最容易踩）。
func (b *LogBroker) Publish(execID, stepID, stream, line string) {
	ll := LiveLog{StepID: stepID, Stream: stream, Line: line, TS: time.Now().UnixNano()}
	b.mu.Lock()
	defer b.mu.Unlock()
	subs := b.subs[execID]
	if len(subs) == 0 {
		return
	}
	for ch := range subs {
		select {
		case ch <- ll:
		default:
			// 慢订阅者：丢弃，不阻塞引擎执行路径。
		}
	}
}

// Subscribe 注册一个 execID 的订阅，返回接收 channel 与退订函数。
// channel 有界（256），满则 Publish 侧丢弃；退订后 channel 被关闭（幂等）。
func (b *LogBroker) Subscribe(execID string) (<-chan LiveLog, func()) {
	ch := make(chan LiveLog, 256)
	var once sync.Once
	b.mu.Lock()
	if b.subs[execID] == nil {
		b.subs[execID] = make(map[chan LiveLog]struct{})
	}
	b.subs[execID][ch] = struct{}{}
	b.mu.Unlock()
	unsub := func() {
		b.mu.Lock()
		if m := b.subs[execID]; m != nil {
			if _, ok := m[ch]; ok {
				delete(m, ch)
				if len(m) == 0 {
					delete(b.subs, execID)
				}
			}
		}
		b.mu.Unlock()
		// close 不可重复调用，用 Once 保证幂等（退订函数可能被多次调用）。
		once.Do(func() { close(ch) })
	}
	return ch, unsub
}
