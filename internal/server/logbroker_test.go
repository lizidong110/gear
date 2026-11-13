package server

import (
	"testing"
	"time"
)

// 订阅后能实时收到发布行，且 step/stream/line 内容正确（REQ-API-LOGS-STREAM）。
func TestLogBrokerPublishSubscribe(t *testing.T) {
	b := NewLogBroker()
	ch, unsub := b.Subscribe("exec-1")
	defer unsub()

	b.Publish("exec-1", "s1", "stdout", "hello")
	select {
	case ll, ok := <-ch:
		if !ok {
			t.Fatal("channel 被意外关闭")
		}
		if ll.StepID != "s1" || ll.Stream != "stdout" || ll.Line != "hello" {
			t.Fatalf("收到行内容不符: %+v", ll)
		}
	case <-time.After(time.Second):
		t.Fatal("1s 内未收到发布行")
	}
}

// 无订阅者时 Publish 立即返回，不阻塞——红线：绝不反压生产端（引擎执行路径）。
func TestLogBrokerNoSubscriberDoesNotBlock(t *testing.T) {
	b := NewLogBroker()
	done := make(chan struct{})
	go func() {
		b.Publish("nobody", "s", "stdout", "x")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("无订阅者时 Publish 阻塞了")
	}
}

// 慢订阅者：channel 满（256）后 Publish 丢弃而非阻塞（非阻塞红线）。
// 这是「实时、不反压引擎」契约的核心：丢几行诊断日志远好过卡住一次执行。
func TestLogBrokerSlowSubscriberDropped(t *testing.T) {
	b := NewLogBroker()
	_, unsub := b.Subscribe("exec-2")
	defer unsub()

	// 填满有界缓冲（不消费）。
	for i := 0; i < 256; i++ {
		b.Publish("exec-2", "s", "stdout", "fill")
	}
	// 再 Publish 不得阻塞：在 goroutine 里发，超时即失败。
	done := make(chan struct{})
	go func() {
		b.Publish("exec-2", "s", "stdout", "overflow")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("慢订阅者时 Publish 阻塞了（应丢弃）")
	}
}

// 退订函数幂等且关闭 channel（再次读取应收到关闭信号，不 panic）。
func TestLogBrokerUnsubscribeClosesChannel(t *testing.T) {
	b := NewLogBroker()
	ch, unsub := b.Subscribe("exec-3")
	unsub()
	unsub() // 幂等，不应 panic
	if _, ok := <-ch; ok {
		t.Fatal("退订后 channel 应已关闭")
	}
}
