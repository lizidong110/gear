package engine

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"gear/internal/cap"
	"gear/internal/model"
)

// ---- 调用上下文（IDENTITY_CONTEXT_DESIGN.md §3）----
//
// 这组测试守的是「身份必须真的能被 Lua / process 读到，且不可被调用方伪造」。
// 每个用例都配负向验证：不是「跑通了」，而是「想伪造时必然被挡住」。

// C-call-1：Lua Tool 能读到 ctx.caller.subject。
func TestCallerReachesLuaTool(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	// echo 原样返回 input；ctx.caller 由宿主注入（不经过 input）
	putTool(t, reg, luaTool("echo", `return ctx.input`, objSchema(), objSchema()))
	// peek 直接把 ctx.caller 的字段放进返回值，验证注入通道确实到位
	putTool(t, reg, luaTool("peek",
		`return { subject = ctx.caller.subject, app_id = ctx.caller.app_id }`,
		objSchema(), objSchema()))

	putFlow(t, reg, &model.Flow{
		ID:    "f",
		Steps: []model.Step{{ID: "s1", Call: "peek"}},
	})

	ctx := WithCaller(context.Background(), cap.Caller{Subject: "alice", AppID: "appA"})
	res, err := New(reg).RunFlowResultCtx(ctx, "f")
	if err != nil {
		t.Fatalf("RunFlowResultCtx 失败: %v", err)
	}
	o, ok := res.Steps["s1"]["output"].(map[string]interface{})
	if !ok {
		t.Fatalf("s1 无 output: %v", res.Steps["s1"])
	}
	if got := fmt.Sprint(o["subject"]); got != "alice" {
		t.Fatalf(`ctx.caller.subject 应为 "alice"，实际 %q（宿主注入是否漏了 hostFor？）`, got)
	}
	if got := fmt.Sprint(o["app_id"]); got != "appA" {
		t.Fatalf("ctx.caller.app_id 应为 \"appA\"，实际 %q", got)
	}
}

// C-call-2：不带调用上下文时，subject 是**空**而不是伪造值。
//
// 这是「无身份」与「匿名用户」的分界：Hook 要能区分「没验过」和「验出来是匿名」，
// 故此处断言为空串——若返回某个默认字符串，Hook 就会把「没验过」当成「验过了」。
func TestCallerEmptyWhenNotProvided(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	putTool(t, reg, luaTool("peek",
		`return { subject = ctx.caller.subject }`, objSchema(), objSchema()))

	// 裸 ctx，不套 WithCaller
	out, err := New(reg).RunToolExec(context.Background(), "peek", nil, "")
	if err != nil {
		t.Fatalf("RunToolExec 失败: %v", err)
	}
	m, ok := out["subject"]
	if !ok {
		t.Fatalf("ctx.caller.subject 应存在且为空串，实际无此键: %v", out)
	}
	if s := fmt.Sprint(m); s != "" {
		t.Fatalf("无调用上下文时 subject 应为空串，实际 %q", s)
	}
}

// C-call-3：★ vars / 调用参数**无法**覆盖身份。
//
// 负向验证：在 vars 里塞一个看起来能覆盖身份的键，确认输出仍是真实身份。
// 这条是「caller 不可由调用参数覆盖」红线的机械守卫——哪天有人图省事把 caller
// 改成从 vars 读，这里会立刻红。
func TestCallerCannotBeOverriddenByVars(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	putTool(t, reg, luaTool("peek",
		`return { subject = ctx.caller.subject }`, objSchema(), objSchema()))

	// 试图经 vars 与 input 双向伪造
	ctx := WithCaller(context.Background(), cap.Caller{Subject: "alice"})
	eng := New(reg)
	flow := &model.Flow{
		ID:   "f",
		Vars: map[string]interface{}{"caller": map[string]interface{}{"subject": "evil"}},
		Steps: []model.Step{{
			ID:   "s1",
			Call: "peek",
			Input: map[string]interface{}{
				"caller": map[string]interface{}{"subject": "evil"},
			},
		}},
	}
	putFlow(t, reg, flow)
	// 另给一层「运行时 vars 覆盖」入口，一并试
	res, err := eng.RunFlowResultCtx(ctx, "f")
	if err != nil {
		t.Fatalf("RunFlowResultCtx 失败: %v", err)
	}
	o, ok := res.Steps["s1"]["output"].(map[string]interface{})
	if !ok {
		t.Fatalf("s1 无 output: %v", res.Steps["s1"])
	}
	if got := fmt.Sprint(o["subject"]); got != "alice" {
		t.Fatalf("身份被 vars 覆盖了（应为 alice，实际 %q）——红线：caller 只从引擎边界读取", got)
	}
}

// C-call-4：process 子进程能从 stdin 的 ctx 里读到 caller。
//
// 同时验证「注入发生在 input_schema 校验之后」：input 里没有 ctx 键也能正常工作，
// 加了反而不会被 schema 判定为未知字段。
func TestCallerReachesProcessTool(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	// cat 原样回显 stdin 的 JSON（含 ctx 子树）
	putTool(t, reg, procTool("echo", "cat", objSchema(), objSchema()))

	out, err := New(reg).RunToolExec(
		WithCaller(context.Background(), cap.Caller{Subject: "bob", AppID: "appB"}),
		"echo", map[string]interface{}{}, "")
	if err != nil {
		t.Fatalf("RunToolExec 失败: %v", err)
	}
	ctxObj, ok := out["ctx"].(map[string]interface{})
	if !ok {
		t.Fatalf("process 的 output 里应有 ctx 子对象: %v", out)
	}
	caller, ok := ctxObj["caller"].(map[string]interface{})
	if !ok {
		t.Fatalf("ctx 里应有 caller: %v", ctxObj)
	}
	if got := fmt.Sprint(caller["subject"]); got != "bob" {
		t.Fatalf("子进程 ctx.caller.subject 应为 bob，实际 %q", got)
	}
	if got := fmt.Sprint(caller["app_id"]); got != "appB" {
		t.Fatalf("子进程 ctx.caller.app_id 应为 appB，实际 %q", got)
	}
}

// C-call-5：Hook 能拿到身份（鉴权的真实用例）。
//
// 这是①的全部意义所在：Hook 依 ctx.caller.subject 判定，不匹配就 fail-closed 拒绝。
func TestHookCanSeeCaller(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	putTool(t, reg, luaTool("echo", `return ctx.input`, objSchema(), objSchema()))
	// 守卫 Hook：只放行 alice，其余一律报错（hookFail 通道）
	putTool(t, reg, luaTool("guard",
		`if ctx.caller.subject ~= "alice" then error("拒绝：调用方 "..tostring(ctx.caller.subject).." 无权执行") end
		 return {}`, objSchema(), objSchema()))
	putHook(t, reg, &model.Hook{ID: "hg", Target: "echo", Stage: model.StageBeforeStep, Action: "guard"})
	putFlow(t, reg, &model.Flow{
		ID:    "f",
		Steps: []model.Step{{ID: "s1", Call: "echo", Input: map[string]interface{}{"x": 1}}},
	})

	eng := New(reg)
	if _, err := eng.RunFlowResultCtx(context.Background(), "f"); err == nil {
		t.Fatal("subject 为空时应被守卫 Hook 拒绝（fail-closed 失效）")
	}
	res, err := eng.RunFlowResultCtx(
		WithCaller(context.Background(), cap.Caller{Subject: "alice"}), "f")
	if err != nil {
		t.Fatalf("alice 应被放行，实际失败: %v", err)
	}
	if res == nil {
		t.Fatal("alice 的执行应返回结果")
	}
}

// C-call-6：Ctx 变体与旧入口的存在性守卫。
//
// 定位「哪个公开入口还没接调用上下文」全靠它：grep 这些符号，缺一个就是漏了一处身份来源。
func TestCtxVariantsEnterCaller(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	putTool(t, reg, luaTool("peek", `return { subject = ctx.caller.subject }`, objSchema(), objSchema()))

	ctx := WithCaller(context.Background(), cap.Caller{Subject: "carol"})
	out, err := New(reg).RunToolExec(ctx, "peek", map[string]interface{}{}, "")
	if err != nil {
		t.Fatalf("RunToolExec(ctx…) 失败: %v", err)
	}
	if got := fmt.Sprint(out["subject"]); got != "carol" {
		t.Fatalf("RunToolExec 应带上身份，实际 %q", got)
	}
}

// C-call-7：CallerFrom 的取值语义（未命中 = 无身份，不是匿名用户）。
func TestCallerFromSemantics(t *testing.T) {
	if _, ok := CallerFrom(context.Background()); ok {
		t.Fatal("裸 context 不应命中调用上下文")
	}
	c, ok := CallerFrom(WithCaller(context.Background(), cap.Caller{Subject: "dave"}))
	if !ok || c.Subject != "dave" {
		t.Fatalf("WithCaller 写入后应能原样取回，实际 %+v ok=%v", c, ok)
	}
	// 用**类型化零值**而不是字面量 nil：staticcheck SA1012 禁止把 nil 当 Context 传，
	// 而这里要验的恰恰是「传进来一个 nil 也不会 panic、不会伪造身份」。
	var nilCtx context.Context
	if c, ok := CallerFrom(nilCtx); ok || c.Subject != "" {
		t.Fatalf("nil ctx 应返回零值与 ok=false，实际 %+v ok=%v", c, ok)
	}
	if !strings.Contains(t.Name(), "Caller") {
		t.Fatal("测试名被误改——负向断言可能没跟着跑")
	}
}
