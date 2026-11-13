package engine

import (
	"context"
	"strings"
	"sync"
	"testing"

	"gear/internal/model"
)

// TestSuperviseReportsLogsToSink 锁住 `app run` 路径的 LogSink 挂点（REQ-API-APP-LOGS）。
//
// 为什么单独立一条：detached App 的实时日志完全依赖 Supervise 里的 rc.logSink。
// 这条路径没有 REST 执行记录 id，历史上 logSinkFor 对空 execID 直接返回 nil ——
// 一旦有人「顺手清理」掉注入，所有单元测试仍绿，但 UI 的台账会标着「已接入实时日志」
// 却永远收不到一行。用一条端到端断言把这个静默失效钉死。
func TestSuperviseReportsLogsToSink(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	// entry 末行必须是 JSON（process 契约，PRD §3.3）：前一行进 LogSink、末行 {} 作输出。
	// 旧写法 `sh -c 'echo svc-line'` 只吐纯文本，Supervise 必然解析失败（2026-11-13 修）。
	if err := reg.UpsertTool(svcTool(`echo svc-line && echo {}`)); err != nil {
		t.Fatalf("注册 Tool 失败: %v", err)
	}
	flow := &model.Flow{ID: "slog", Steps: []model.Step{
		{ID: "once", Call: "svc", Input: map[string]interface{}{}},
	}}
	if err := reg.UpsertFlow(flow); err != nil {
		t.Fatalf("注册 Flow 失败: %v", err)
	}

	var mu sync.Mutex
	var got []struct{ execID, stepID, stream, line string }
	eng := New(reg, WithLogSink(func(execID, stepID, stream, line string) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, struct{ execID, stepID, stream, line string }{execID, stepID, stream, line})
	}))
	if _, err := eng.Supervise(context.Background(), "slog"); err != nil {
		t.Fatalf("Supervise 失败: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	found := false
	for _, l := range got {
		if !strings.Contains(l.line, "svc-line") {
			continue
		}
		found = true
		if l.execID != "supervise:slog" || l.stepID != "once" {
			t.Fatalf("日志关联字段不对: %+v", l)
		}
	}
	if !found {
		t.Fatalf("Supervise 的 process 输出没有上报到 LogSink，实际收到 %d 行: %+v", len(got), got)
	}
}

// TestSuperviseWithoutSinkKeepsWorking：没装 sink（普通 CLI 前台 app run）时行为零变化。
func TestSuperviseWithoutSinkKeepsWorking(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	if err := reg.UpsertTool(svcTool(`echo ok && echo {}`)); err != nil {
		t.Fatal(err)
	}
	if err := reg.UpsertFlow(&model.Flow{ID: "plain", Steps: []model.Step{
		{ID: "s1", Call: "svc", Input: map[string]interface{}{}},
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := New(reg).Supervise(context.Background(), "plain"); err != nil {
		t.Fatalf("无 sink 的 Supervise 失败: %v", err)
	}
}
