package engine

import (
	"strings"
	"testing"

	"gear/internal/model"
)

// ---- 运行期命名空间守卫（--scope，IDENTITY_CONTEXT_DESIGN.md §4.3）----
//
// 同一件事的三个面：**默认不开**（不传 --scope 的行为与未加本特性时逐字一致）、
// **开了就 fail-closed**、**开了也不能把平台原语一道切死**。
// 第三条最容易被忽略——mindstorm 的入口 Flow 同时调用了 `mindstorm__config` 与
// `static_site.serve`，若原语也在守卫范围内，开了 scope 的应用连自己的入口都跑不起来。

// S-1：scope 非空时，Flow 引用**别的应用的 Tool** 必须被拒（fail-closed）。
func TestScopeGuardBlocksForeignTool(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	putTool(t, reg, luaTool("other__x", `return ctx.input`, objSchema(), objSchema()))
	putFlow(t, reg, &model.Flow{ID: "mindstorm.up", Steps: []model.Step{
		{ID: "s1", Call: "other__x", Input: map[string]interface{}{"k": "v"}},
	}})
	// other 必须是**已注册 App**：无主 id（第一段不在 apps 表）按平台级豁免放行，
	// 不补这条注册，本用例测的就不是「跨应用」而是「平台级」了。
	if err := reg.UpsertApp(&model.App{ID: "other", EntryFlow: "other.up"}); err != nil {
		t.Fatalf("注册 App 失败: %v", err)
	}

	eng := New(reg, WithScope("mindstorm"))
	if eng.Scope() != "mindstorm" {
		t.Fatalf("WithScope 未生效: %q", eng.Scope())
	}
	_, err := eng.RunFlowResult("mindstorm.up")
	if err == nil {
		t.Fatal("跨应用引用 Tool 应被命名空间守卫拒绝")
	}
	if code := Diagnostics(err)[0].Code; code != CodeScopeViolation {
		t.Fatalf("错误码应为 %s，实际 %s（%v）", CodeScopeViolation, code, err)
	}
}

// S-2：同 scope 的 Tool 正常执行（守卫不能把「自己」也挡在门外）。
func TestScopeGuardAllowsOwnTool(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	putTool(t, reg, luaTool("mindstorm__x", `return ctx.input`, objSchema(), objSchema()))
	putFlow(t, reg, &model.Flow{ID: "mindstorm.up", Steps: []model.Step{
		{ID: "s1", Call: "mindstorm__x", Input: map[string]interface{}{"k": "v"}},
	}})

	res, err := New(reg, WithScope("mindstorm")).RunFlowResult("mindstorm.up")
	if err != nil {
		t.Fatalf("本应用内的调用应放行，实际报错: %v", err)
	}
	if _, ok := res.Steps["s1"]["output"].(map[string]interface{}); !ok {
		t.Fatalf("step 无 output: %v", res.Steps["s1"])
	}
}

// S-3：不传 scope 时跨应用引用**照旧放行**——默认不开是刻意的启用策略：
// 现有定义都是先存在、后挂 App（apps/mindstorm 的 Tool 与 apps 表是两条独立路径），
// 默认开守卫会让单机版立刻跑不起来。本测试是那条策略的**正向断言**，
// 而不只是「现有测试碰巧没设 scope」。
func TestNoScopeBehavesAsBefore(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	putTool(t, reg, luaTool("other__x", `return ctx.input`, objSchema(), objSchema()))
	putFlow(t, reg, &model.Flow{ID: "f", Steps: []model.Step{
		{ID: "s1", Call: "other__x", Input: map[string]interface{}{"k": "v"}},
	}})

	if _, err := New(reg).RunFlowResult("f"); err != nil {
		t.Fatalf("无 scope 应逐字保持原行为，实际报错: %v", err)
	}
}

// S-4：开了 scope，**原语直调不受影响**——守的是「别的应用的定义」，不是「别用平台」。
func TestScopeGuardSkipsPrimitives(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	putFlow(t, reg, &model.Flow{ID: "mindstorm.up", Steps: []model.Step{
		{ID: "s1", Call: "time.now", Input: map[string]interface{}{}},
	}})

	res, err := New(reg, WithScope("mindstorm")).RunFlowResult("mindstorm.up")
	if err != nil {
		t.Fatalf("原语直调不应被守卫拦截，实际: %v", err)
	}
	if _, ok := res.Steps["s1"]["output"].(map[string]interface{}); !ok {
		t.Fatalf("step 无 output: %v", res.Steps["s1"])
	}
}

// S-5：直接跑别的应用的 Flow 也要被拒（step 级引用只是其中一条逃逸路径；
// 顺带证明「只守 step.call、不管入口 Flow」的实现是不合格的）。
func TestScopeGuardBlocksForeignFlowRun(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	putTool(t, reg, luaTool("other__x", `return ctx.input`, objSchema(), objSchema()))
	putFlow(t, reg, &model.Flow{ID: "other.up", Steps: []model.Step{
		{ID: "s1", Call: "other__x", Input: map[string]interface{}{"k": "v"}},
	}})
	if err := reg.UpsertApp(&model.App{ID: "mindstorm", EntryFlow: "mindstorm.up"}); err != nil {
		t.Fatalf("注册 App 失败: %v", err)
	}
	if err := reg.UpsertApp(&model.App{ID: "other", EntryFlow: "other.up"}); err != nil {
		t.Fatalf("注册 App 失败: %v", err)
	}

	_, err := New(reg, WithScope("mindstorm")).RunFlowResult("other.up")
	if err == nil {
		t.Fatal("跑别的应用的 Flow 应被拒")
	}
	if code := Diagnostics(err)[0].Code; code != CodeScopeViolation {
		t.Fatalf("错误码应为 %s，实际 %s", CodeScopeViolation, code)
	}
}

// S-6：应用的 entry_flow 不带前缀是合法的（与 artifact.Composition 同口径）——
// 少了这条豁免，`gear app start --scope mindstorm` 会连自己的入口都跑不起来。
func TestScopeGuardExemptsAppEntryFlow(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	putTool(t, reg, luaTool("mindstorm__x", `return ctx.input`, objSchema(), objSchema()))
	putFlow(t, reg, &model.Flow{ID: "up", Steps: []model.Step{
		{ID: "s1", Call: "mindstorm__x", Input: map[string]interface{}{"k": "v"}},
	}})
	if err := reg.UpsertApp(&model.App{ID: "mindstorm", EntryFlow: "up"}); err != nil {
		t.Fatalf("注册 App 失败: %v", err)
	}

	res, err := New(reg, WithScope("mindstorm")).RunFlowResult("up")
	if err != nil {
		t.Fatalf("entry_flow 应被豁免，实际报错: %v", err)
	}
	if _, ok := res.Steps["s1"]["output"].(map[string]interface{}); !ok {
		t.Fatalf("step 无 output: %v", res.Steps["s1"])
	}
}

// S-7：Hook 的 action 是「由别人定义的行为」，必须与 step.call 同口径拦截。
func TestScopeGuardBlocksForeignHookAction(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	putTool(t, reg, luaTool("mindstorm__echo", `return ctx.input`, objSchema(), objSchema()))
	putTool(t, reg, luaTool("other__x", `return ctx.input`, objSchema(), objSchema()))
	putHook(t, reg, &model.Hook{
		ID: "mindstorm__h1", Target: "mindstorm__echo",
		Stage: model.StageBeforeStep, Action: "other__x",
	})
	if err := reg.UpsertApp(&model.App{ID: "other", EntryFlow: "other.up"}); err != nil {
		t.Fatalf("注册 App 失败: %v", err)
	}
	putFlow(t, reg, &model.Flow{ID: "mindstorm.up", Steps: []model.Step{
		{ID: "s1", Call: "mindstorm__echo", Input: map[string]interface{}{"k": "v"}},
	}})

	_, err := New(reg, WithScope("mindstorm")).RunFlowResult("mindstorm.up")
	if err == nil {
		t.Fatal("Hook action 指向别的应用的 Tool 应被拒")
	}
	if code := Diagnostics(err)[0].Code; code != CodeScopeViolation {
		t.Fatalf("错误码应为 %s，实际 %s（%v）", CodeScopeViolation, code, err)
	}
}

// S-8：ctx.call 是除 step.call 之外的第二条逃逸路径——它把 target 交给了原语直调，
// 能摸到的对象比 step.call 更多，不拦就白守卫了。
//
// ⚠ 这里**只断言错误的文本，不断言 `SCOPE_VIOLATION`**：越过 Lua 边界时错误被
// `L.RaiseError("call 失败: %v", …)` 拍成字符串，结构化诊断码就此丢失（ctx.call / 其它原语
// 的失败都是这个口径，见 lua.go）。拒绝**发生了**、原因**看得到**，但智能体没法按码去查手册自愈。
// 修它需要在 Lua 层保留错误对象再回传——那是独立的一条工作，不在本特性内假装解决。
func TestScopeGuardBlocksForeignCallTarget(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	// 主 Flow：一步用 call 原语去够别的应用的 Tool。
	putTool(t, reg, luaTool("other__x", `return ctx.input`, objSchema(), objSchema()))
	if err := reg.UpsertApp(&model.App{ID: "other", EntryFlow: "other.up"}); err != nil {
		t.Fatalf("注册 App 失败: %v", err)
	}
	putFlow(t, reg, &model.Flow{ID: "mindstorm.up", Steps: []model.Step{
		{ID: "s1", Call: "call.Call", Input: map[string]interface{}{
			"target": "other__x",
			"input":  map[string]interface{}{},
		}},
	}})

	_, err := New(reg, WithScope("mindstorm")).RunFlowResult("mindstorm.up")
	if err == nil {
		t.Fatal("ctx.call 指向别的应用的 Tool 应被拒")
	}
	// 拒绝本身与原因文本必须到达调用方（Lua 边界只把错误拍成字符串）。
	if !strings.Contains(err.Error(), "命名空间守卫") || !strings.Contains(err.Error(), "other__x") {
		t.Fatalf("拒绝原因应带守卫说明与被拒 id，实际: %v", err)
	}
}

// S-9：`RunToolExec` 自己拼 `resolvedCall`，**不经 `resolveStepCallView`**——
// 于是「step 解析链上那一处守卫」对它无效，而它正长在 `gear tool run` 上：
// 不经过任何 Flow 就能点名执行一个 Tool。这条捷径不堵，前面 8 条全白守。
//
// 它是实跑验证时补的（2026-09-26）：当时只验了 Flow 面，`gear tool run --scope a appb__ping`
// 一下就跑通了。落点要按**代码路径**数，不能按语义（step / hook）数。
func TestScopeGuardBlocksDirectToolRun(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	putTool(t, reg, luaTool("other__x", `return ctx.input`, objSchema(), objSchema()))
	if err := reg.UpsertApp(&model.App{ID: "other", EntryFlow: "other.up"}); err != nil {
		t.Fatalf("注册 App 失败: %v", err)
	}

	_, err := New(reg, WithScope("mindstorm")).RunTool("other__x", map[string]interface{}{"k": "v"})
	if err == nil {
		t.Fatal("`gear tool run` 不应能执行别的应用的 Tool")
	}
	if code := Diagnostics(err)[0].Code; code != CodeScopeViolation {
		t.Fatalf("错误码应为 %s，实际 %s（%v）", CodeScopeViolation, code, err)
	}
}

// S-10：同一条路径上，自己名下的 Tool 必须照常可执行（S-9 不能把 `tool run` 整体废掉）。
func TestScopeGuardAllowsOwnDirectToolRun(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	putTool(t, reg, luaTool("mindstorm__x", `return ctx.input`, objSchema(), objSchema()))

	out, err := New(reg, WithScope("mindstorm")).RunTool("mindstorm__x", map[string]interface{}{"k": "v"})
	if err != nil {
		t.Fatalf("本应用名下的 Tool 应放行，实际报错: %v", err)
	}
	if _, ok := out["k"]; !ok {
		t.Fatalf("输出应回显输入，实际: %v", out)
	}
}

// S-11：平台级注册 Tool（无应用前缀、第一段不归属任何 App）在 scope 下必须放行。
// 2026-10-21 真机事故：`tool list --scope wms` 列出 ext.serve / static_site.serve /
// web_ui.api，用户顺理成章调用——却被守卫毙掉（注释承诺豁免，实现直到本次才补）。
// 平台级对象不是「别人应用的定义」，拦它没有保护意义只有破坏：任何调平台 Tool 的
// 应用（web 类标配）一开 --scope 就崩。step.call 与 tool run 两条路径都钉。
func TestScopeGuardAllowsPlatformLevelTool(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	putTool(t, reg, luaTool("ext.serve", `return { ok = "platform" }`, objSchema(), objSchema()))
	putTool(t, reg, luaTool("web_ui.api", `return { ok = "platform" }`, objSchema(), objSchema()))

	eng := New(reg, WithScope("mindstorm"))
	// 路径 1：step.call（resolveStepCallView 的 ① 分支——事故正是这里）
	putFlow(t, reg, &model.Flow{ID: "mindstorm.up", Steps: []model.Step{
		{ID: "s1", Call: "ext.serve", Input: map[string]interface{}{}},
	}})
	res, err := eng.RunFlowResult("mindstorm.up")
	if err != nil {
		t.Fatalf("平台级注册 Tool 经 step.call 应放行，实际报错: %v", err)
	}
	if _, ok := res.Steps["s1"]["output"].(map[string]interface{}); !ok {
		t.Fatalf("step 无 output: %v", res.Steps["ss1"])
	}
	// 路径 2：tool run（RunToolExec——S-9 补的那条捷径，豁免口径必须一致）
	out, err := eng.RunTool("web_ui.api", map[string]interface{}{})
	if err != nil {
		t.Fatalf("平台级注册 Tool 经 tool run 应放行，实际报错: %v", err)
	}
	if out["ok"] != "platform" {
		t.Fatalf("输出不符: %v", out)
	}
}

// S-12：边界——第一段是别的已注册 App 时仍拦（豁免不放松「跨应用」）。
// 与 S-11 互为两面：无主 id 放行 ≠ 别人的定义也放行。
func TestScopeGuardStillBlocksForeignAfterPlatformExemption(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	putTool(t, reg, luaTool("other__x", `return ctx.input`, objSchema(), objSchema()))
	if err := reg.UpsertApp(&model.App{ID: "other", EntryFlow: "other.up"}); err != nil {
		t.Fatalf("注册 App 失败: %v", err)
	}

	if _, err := New(reg, WithScope("mindstorm")).RunTool("other__x", map[string]interface{}{}); err == nil {
		t.Fatal("别的应用的 Tool 仍应被拒（平台级豁免不放松跨应用）")
	}
}
