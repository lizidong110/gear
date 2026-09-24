package mcp

// mcp_review_test.go —— MCP 接口评审（MCP_接口与协议评审_v1.md）两项 P1 修复的回归锁：
//
//	I-1 执行上下文的取消传播 + 有界并发（背压）——此前 12 个 handler 全丢弃 ctx、
//	    忙等不看 ctx.Done()、每条执行裸起 goroutine（无上限、无收敛）；
//	I-6 12 个工具对外声明 outputSchema——此前全是 `Out = any`，客户端无法静态知道返回形状。

import (
	"context"
	"testing"
	"time"

	"gear/internal/engine"
	"gear/internal/registry"
)

// ---- I-6 ----

// TestMCPToolsAdvertiseOutputSchema：每个工具都必须声明 outputSchema，且是「只列键、不设约束」的
// 对象 schema。
func TestMCPToolsAdvertiseOutputSchema(t *testing.T) {
	session := newSmokeSession(t)
	tools, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools.Tools) == 0 {
		t.Fatal("工具面为空")
	}
	for _, tt := range tools.Tools {
		if tt.OutputSchema == nil {
			t.Errorf("工具 %s 未声明 outputSchema（客户端将无法静态知道返回形状）", tt.Name)
			continue
		}
		m, ok := tt.OutputSchema.(map[string]any)
		if !ok {
			t.Errorf("工具 %s 的 outputSchema 应为对象，实际 %T", tt.Name, tt.OutputSchema)
			continue
		}
		if m["type"] != "object" {
			t.Errorf("工具 %s 的 outputSchema 顶层应为 object，实际 %v", tt.Name, m["type"])
		}
		props, ok := m["properties"].(map[string]any)
		if !ok || len(props) == 0 {
			t.Errorf("工具 %s 的 outputSchema 应列出具名 properties", tt.Name)
		}
		// 红线：SDK 会对**每次**返回做 schema 校验（server.go toolForErr → applySchema），
		// 因此绝不能设 required / 禁 additionalProperties——合法返回里缺失/多余的键会被判失败。
		if _, has := m["required"]; has {
			t.Errorf("工具 %s 的 outputSchema 不得设 required（会把合法返回拒掉）", tt.Name)
		}
		if ap, has := m["additionalProperties"]; has && ap == false {
			t.Errorf("工具 %s 的 outputSchema 不得禁 additionalProperties", tt.Name)
		}
	}
}

// ---- I-1 ----

// TestExecutionSnapshotStopsOnCancel：客户端断开 / 超时（ctx 取消）时，executionSnapshot 的忙等
// 必须**立刻**停止并如实返回当前快照——此前不看 ctx.Done()，客户端早走了还会等满 N 秒。
func TestExecutionSnapshotStopsOnCancel(t *testing.T) {
	f := newSmokeFixture(t)
	rec := f.server.execs.Create("f") // 造一条「运行中」的执行（故意不 Finish）

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 立即取消

	start := time.Now()
	snap := f.server.executionSnapshot(ctx, rec.ID, 30)
	elapsed := time.Since(start)

	if elapsed > 3*time.Second {
		t.Fatalf("ctx 已取消仍继续忙等（等了 %s，应为毫秒级）", elapsed)
	}
	if snap["status"] != "running" {
		t.Fatalf("取消后应如实返回当前快照（running），实际 %v", snap["status"])
	}
}

// TestStartFlowExecBackpressure：在途执行达并发上限时，startFlowExec 必须**背压拒绝**
// （EXECUTION_BUSY），而不是无限起 goroutine。
func TestStartFlowExecBackpressure(t *testing.T) {
	f := newSmokeFixtureWith(t, func(reg *registry.Registry) { applySmokeBundle(t, reg) })

	// 灌满并发槽，模拟「在途执行已满」。
	for i := 0; i < maxConcurrentExecs; i++ {
		f.server.execSem <- struct{}{}
	}
	defer func() {
		for i := 0; i < maxConcurrentExecs; i++ {
			<-f.server.execSem
		}
	}()

	_, err := f.server.startFlowExec("smoke.up", nil)
	if err == nil {
		t.Fatal("并发已达上限时应背压拒绝，实际放行了一条新执行")
	}
	ds := engine.Diagnostics(err)
	if len(ds) == 0 || ds[0].Code != "EXECUTION_BUSY" {
		t.Fatalf("应返回 EXECUTION_BUSY 码，实际 %v", err)
	}
}
