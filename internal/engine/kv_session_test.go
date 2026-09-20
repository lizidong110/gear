package engine

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"gear/internal/cap"
	"gear/internal/model"
)

// E-kvl-1：session 作用域 KV 在同一 flow 执行的多个 step 间共享，且 run 结束后被回收。
//
// 验证点（M-KV-LIFECYCLE 引擎层 DoD）：
//  1. s1 写入 ScopeSession 键，s2（依赖 s1，保证后跑）能读到同一份会话态 → 跨 step 共享；
//  2. RunFlowResult 返回后，SessionRegistry 对该 flow.ID 的命名空间被回收（引用计数归零即删），
//     再次 Session(flowID) 取到的是空命名空间（不含 s1 写入的键）。
func TestKVSessionSharedAcrossStepsAndRecycled(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()

	// 注入可观测宿主：持有 Sessions 引用，run 结束后检查回收（不依赖默认宿主的内部状态）。
	host := &cap.Providers{
		KV:       cap.NewMemKV(),
		HTTP:     &http.Client{Timeout: 30 * time.Second},
		Event:    cap.NewMemEvent(),
		Sessions: cap.NewMemSessionRegistry(),
	}
	eng := New(reg, WithHost(host))

	writer := luaToolWithCaps("kvs_writer", []string{"kv"}, `
		ctx.kv.put("cached", "v", "session")
		return { wrote = true }
	`)
	reader := luaToolWithCaps("kvs_reader", []string{"kv"}, `
		return { got = ctx.kv.get("cached", "session") }
	`)
	if err := reg.UpsertTool(writer); err != nil {
		t.Fatalf("注册 writer 失败: %v", err)
	}
	if err := reg.UpsertTool(reader); err != nil {
		t.Fatalf("注册 reader 失败: %v", err)
	}
	// s2 依赖 s1 → 保证后跑，才能验证「跨 step 共享」而非「同 step 内」。
	flow := &model.Flow{ID: "kvlcycle", Steps: []model.Step{
		{ID: "s1", Call: "kvs_writer"},
		{ID: "s2", Call: "kvs_reader", DependsOn: []string{"s1"}},
	}}
	if err := reg.UpsertFlow(flow); err != nil {
		t.Fatalf("注册 Flow 失败: %v", err)
	}

	res, err := eng.RunFlowResult("kvlcycle")
	if err != nil {
		t.Fatalf("RunFlowResult 失败: %v", err)
	}

	// 1) 跨 step 共享：s2 读到了 s1 在 session 作用域写入的值。
	got := fmt.Sprint(res.Steps["s2"]["output"].(map[string]interface{})["got"])
	if got != "v" {
		t.Fatalf("session KV 跨 step 未共享：期望 v，实为 %q", got)
	}

	// 2) 回收：run 结束后该 flow 的 session 命名空间应被释放（引用计数归零即删）。
	//    Session(id) 对不存在的命名空间会新建空 map，故不含 s1 的残留键即证明已回收。
	sess := host.Sessions.Session("kvlcycle")
	if v, ok := sess.Get("cached", cap.ScopeSession); ok {
		t.Fatalf("flow run 结束后 session 命名空间应被回收，但仍有残留键: %v", v)
	}
}

// E-kvl-2：session 作用域与 global 作用域在引擎层各自独立——
// session 键不会污染 global 命名空间，反之亦然。
func TestKVSessionAndGlobalAreIndependent(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()

	host := &cap.Providers{
		KV:       cap.NewMemKV(),
		HTTP:     &http.Client{Timeout: 30 * time.Second},
		Event:    cap.NewMemEvent(),
		Sessions: cap.NewMemSessionRegistry(),
	}
	eng := New(reg, WithHost(host))

	// 只写 session 作用域，不碰 global。
	writer := luaToolWithCaps("kvs_only", []string{"kv"}, `
		ctx.kv.put("k", "session-value", "session")
		return {}
	`)
	if err := reg.UpsertTool(writer); err != nil {
		t.Fatalf("注册 Tool 失败: %v", err)
	}
	flow := &model.Flow{ID: "kvindep", Steps: []model.Step{{ID: "s1", Call: "kvs_only"}}}
	if err := reg.UpsertFlow(flow); err != nil {
		t.Fatalf("注册 Flow 失败: %v", err)
	}
	if _, err := eng.RunFlowResult("kvindep"); err != nil {
		t.Fatalf("RunFlowResult 失败: %v", err)
	}

	// global KV 不应被 session 写入污染。
	if v, ok := host.KV.Get("k", cap.ScopeGlobal); ok {
		t.Fatalf("session 写入不应污染 global 命名空间，但读到: %v", v)
	}

	// session 命名空间已回收（run 结束）。
	if v, ok := host.Sessions.Session("kvindep").Get("k", cap.ScopeSession); ok {
		t.Fatalf("run 结束后 session 应回收，但仍有残留: %v", v)
	}
}
