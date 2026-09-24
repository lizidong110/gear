package engine

// primitive_step_test.go —— 「能力.方法」直调（原子能力直接作 step.call 目标）的**语义**守卫。
//
// 与 capability_validation_test.go 的分工：
//   - 那边管「什么算合法的能力.方法」（方法名/参数 schema 的静态校验与防漂移守卫）；
//   - 这边管**解析与执行语义**——schema 校验保证不了的那几条：
//     1. **Tool 优先**：字面量同名时按已注册 Tool 解析（否则既有 Tool 会被原语静默遮蔽）；
//     2. 原语载体**不支持** `use_version` / `mode=service`——必须注册期**显式拒绝**，
//        不能静默忽略（M1「when 被静默忽略」/ M6「use_version 被静默忽略」的教训：
//        静默 no-op 是最坏的漂移——用户以为钉了版本/起了长驻服务，实际都没发生）；
//     3. 原语 step 的 Hook target 用「能力.方法」同形字面量匹配；
//     4. 直调宿主**只放行那一个能力**（书写即授权的最小权限，别顺手给全量）；
//     5. 输出统一包成 `{result: ...}`（无返回值显式 null），故 `${steps.x.output.result}`
//        对 Tool 载体与原语载体同形，下游不用分情况。
//
// 注：本文件原以 kv 原语作载体，kv 已于 2026-09-24 按方案 A 移除；现改用 db 原语
// （内核受门禁、其余语义与原 kv 一致：原语直调、Hook 同形匹配、输出包裹、最小权限宿主）。
// db 用引擎默认内存 scratch，无需显式 dsn。

import (
	"fmt"
	"strings"
	"testing"

	"gear/internal/model"
)

// 解析优先级：字面量同名时 Tool 胜出；无同名 Tool 时落回原语直调。
func TestPrimitiveStepToolTakesPrecedence(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	e := New(reg)

	// ① 没有同名 Tool：`db.exec` 解析为原语载体。
	rc, err := e.resolveStepCall(model.Step{ID: "s1", Call: "db.exec"})
	if err != nil {
		t.Fatalf("无同名 Tool 时应解析为原语直调: %v", err)
	}
	if !rc.isPrimitive() || rc.prim != "db" || rc.method != "exec" {
		t.Fatalf("应解析为原语 db/exec，实际 %+v", rc)
	}
	if rc.id() != "db.exec" {
		t.Fatalf("原语载体的 id 应与 step.call 字面量同形（Hook target / 运行中注销上报依赖它），实际 %q", rc.id())
	}

	// ② 注册一个 id 恰好等于 "db.exec" 的 Tool：同名字面量此后必须按 Tool 解析。
	//    若被原语遮蔽，既有 Tool 会突然改跑引擎内建实现——语义静默反转。
	putTool(t, reg, luaTool("db.exec", `ctx.output = { result = "tool" }`, objSchema(), objSchema()))
	rc2, err := e.resolveStepCall(model.Step{ID: "s2", Call: "db.exec"})
	if err != nil {
		t.Fatalf("同名 Tool 存在时应解析为 Tool: %v", err)
	}
	if rc2.isPrimitive() || rc2.tool == nil || rc2.id() != "db.exec" {
		t.Fatalf("同名时应按 Tool 解析（Tool 优先），实际 %+v", rc2)
	}
}

// 原语载体 + mode=service / use_version：注册期显式拒绝（不静默忽略）。
func TestPrimitiveStepRejectsServiceAndUseVersion(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	e := New(reg)

	// mode=service：原语在进程内跑 Lua，没有长驻形态。
	if err := e.ValidateFlowStatic(&model.Flow{ID: "svc", Steps: []model.Step{
		{ID: "s1", Call: "db.exec", Mode: string(model.ModeService), Input: map[string]interface{}{"sql": "SELECT 1"}},
	}}); err == nil || !strings.Contains(err.Error(), "service") {
		t.Fatalf("原语直调 + mode=service 应注册期被拒，实际 %v", err)
	}

	// use_version：版本灰度只对「已注册 Tool 的版本列表」有意义，原语无版本可言。
	// 静默忽略会让用户以为钉了版本——这正是 M6 修掉的那类漂移。
	if err := e.ValidateFlowStatic(&model.Flow{ID: "ver", Steps: []model.Step{
		{ID: "s1", Call: "db.exec", UseVersion: "2.0.0", Input: map[string]interface{}{"sql": "SELECT 1"}},
	}}); err == nil || !strings.Contains(err.Error(), "use_version") {
		t.Fatalf("原语直调 + use_version 应注册期被拒（不得静默忽略），实际 %v", err)
	}

	// 反向确认：Tool 载体的 use_version 仍合法（别把正路一起拒了）。
	if err := reg.UpsertTool(twoVersionTool()); err != nil {
		t.Fatal(err)
	}
	if err := e.ValidateFlowStatic(&model.Flow{ID: "okver", Steps: []model.Step{
		{ID: "s1", Call: "hello", UseVersion: "2.0.0", Input: map[string]interface{}{}},
	}}); err != nil {
		t.Fatalf("Tool 载体 + use_version 应通过静态校验: %v", err)
	}
}

// 原语 step 的 Hook target 用「能力.方法」字面量匹配，且改写真的作用于该原语调用。
func TestPrimitiveStepHookTargetMatchesPrimitiveCall(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	// Hook 动作工具：把 get 的 sql 改写成能取到 v1 的那条（before_step 的返回值即新 input）。
	// 注：mk 是普通 Tool，不声明任何 capability——仅用于产出 input。
	putTool(t, reg, luaTool("mk", `ctx.output = { sql = "SELECT v FROM p_kv WHERE k = 'k'" }`, objSchema(), objSchema()))
	putHook(t, reg, &model.Hook{ID: "hk", Target: "db.query", Stage: model.StageBeforeStep, Action: "mk"})
	putFlow(t, reg, &model.Flow{ID: "f", Steps: []model.Step{
		{ID: "setup", Call: "db.exec", Input: map[string]interface{}{"sql": "CREATE TABLE IF NOT EXISTS p_kv(k TEXT PRIMARY KEY, v TEXT)"}},
		{ID: "put", Call: "db.exec", Input: map[string]interface{}{"sql": "INSERT INTO p_kv(k,v) VALUES('k','v1')"}, DependsOn: []string{"setup"}},
		// sql 故意写错：只有 Hook 命中并改写，后面才读得到 v1。
		// 显式依赖 put：无依赖的 step 会与 put 同层并行，读到未写入的表（S4 语义）。
		{ID: "get", Call: "db.query", Input: map[string]interface{}{"sql": "SELECT v FROM p_kv WHERE k = 'WRONG'"}, DependsOn: []string{"put"}},
	}})

	res, err := New(reg).RunFlowResult("f")
	if err != nil {
		t.Fatalf("RunFlow 失败: %v", err)
	}
	if got := stepField(t, res, "get", "result"); !strings.Contains(fmt.Sprint(got), "v1") {
		t.Fatalf("target=db.query 的 before_step Hook 应改写原语 step 的 input（否则读不到 v1），实际 %v", got)
	}
}

// 直调宿主只放行该能力（最小权限：书写即授权，不多给）。
func TestPrimitiveHostGrantsOnlyThatCapability(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	e := New(reg)

	host, err := e.primitiveHost("db", nil)
	if err != nil {
		t.Fatalf("db 应可作直调目标: %v", err)
	}
	if got := host.Capabilities(); len(got) != 1 || got[0] != "db" {
		t.Fatalf("直调宿主应只放行 db 一个能力，实际 %v", got)
	}
	if _, err := host.HTTP(); err == nil {
		t.Fatal("只放行 db 时访问 http 原语应被拒（最小权限）")
	}

	// 纯函数原子不受 capability 门禁，但必须仍是合法直调目标（primitiveHost 显式放行）。
	for _, p := range purePrimitiveOrder {
		if _, err := e.primitiveHost(p, nil); err != nil {
			t.Fatalf("纯函数原子 %q 应可作直调目标: %v", p, err)
		}
	}
	// 未知能力名（既非内核原语、也非纯函数、也非已注册 ext）必须拒。
	if _, err := e.primitiveHost("nope", nil); err == nil {
		t.Fatal("未知能力名应被拒")
	}
}

// 端到端：原语输出的 `{result: ...}` 包装 + 模板串联（下游对 Tool/原语同形取值）。
func TestPrimitiveStepEndToEndResultWrapping(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	putFlow(t, reg, &model.Flow{ID: "p", Steps: []model.Step{
		{ID: "setup", Call: "db.exec", Input: map[string]interface{}{"sql": "CREATE TABLE IF NOT EXISTS p_kv(k TEXT PRIMARY KEY, v TEXT)"}},
		{ID: "w", Call: "db.exec", Input: map[string]interface{}{"sql": "INSERT INTO p_kv(k,v) VALUES('k','v1')"}, DependsOn: []string{"setup"}},
		// 显式依赖：无依赖的 step 与上游同层**并行**（S4 语义），会读到尚未写入的表。
		// 这里刻意写全 depends_on，让用例只测「原语直调本身」，不掺并行时序的运气。
		{ID: "r", Call: "db.query", Input: map[string]interface{}{"sql": "SELECT v FROM p_kv WHERE k = 'k'"}, DependsOn: []string{"w"}},
		// 模板只支持「点号路径下钻」、不支持下标：db.query 返回的是**行列表**，故串联改用一个
		// 返回**标量**的纯函数原子（crypto.hash）——证明 `${steps.x.output.result}` 对原语载体同形可取。
		{ID: "h", Call: "crypto.hash", Input: map[string]interface{}{"algo": "sha256", "data": "v1"}},
		// sql 引用 h 的标量 result；w2 用显式 depends_on 保证表已建好（setup 与它是并行层）。
		{ID: "w2", Call: "db.exec", Input: map[string]interface{}{"sql": "INSERT INTO p_kv(k,v) VALUES('k2','${steps.h.output.result}')"}, DependsOn: []string{"setup"}},
		{ID: "r2", Call: "db.query", Input: map[string]interface{}{"sql": "SELECT v FROM p_kv WHERE k = 'k2'"}, DependsOn: []string{"w2"}},
	}})

	e := New(reg)
	flow, err := reg.GetFlow("p")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.ValidateFlowStatic(flow); err != nil {
		t.Fatalf("静态校验应通过: %v", err)
	}
	res, err := e.RunFlowResult("p")
	if err != nil {
		t.Fatalf("RunFlow 失败: %v", err)
	}
	if got := stepField(t, res, "r", "result"); !strings.Contains(fmt.Sprint(got), "v1") {
		t.Fatalf("db.query 应读出 v1，实际 %q", got)
	}
	hashVal := fmt.Sprint(stepField(t, res, "h", "result"))
	if hashVal == "" {
		t.Fatalf("crypto.hash 应返回非空标量 result")
	}
	if got := stepField(t, res, "r2", "result"); !strings.Contains(fmt.Sprint(got), hashVal) {
		t.Fatalf("模板串联失败：r2 应含 %q，实际 %q", hashVal, got)
	}
	// 无返回值的方法（db.exec）输出仍是对象且 result 显式为 nil——不是缺字段。
	out, ok := res.Steps["w"]["output"].(map[string]interface{})
	if !ok {
		t.Fatalf("原语 step 输出应是对象（统一包装），实际 %T", res.Steps["w"]["output"])
	}
	if v, ok := out["result"]; !ok || v != nil {
		t.Fatalf("无返回值的原语应显式给 result=null，实际 %#v", out)
	}
}

// 解析失败必须是可操作的错误（不是笼统的「执行失败」）。
func TestPrimitiveStepUnknownTargetErrors(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	e := New(reg)

	cases := []struct {
		name string
		call string
		in   map[string]interface{}
		want string
	}{
		{"未知能力名", "nope.put", nil, "未知的原子能力"},
		{"已知能力未知方法", "db.nope", nil, "没有方法"},
		{"既非 Tool 也非能力.方法", "nosuch", nil, "既不是已注册 Tool"},
		{"缺必填参数", "db.exec", map[string]interface{}{}, "缺少必填参数"},
		{"多余参数", "db.query", map[string]interface{}{"sql": "SELECT 1", "oops": 1}, "不接受参数"},
	}
	for i, tc := range cases {
		err := e.ValidateFlowStatic(&model.Flow{
			ID:    fmt.Sprintf("bad%d", i),
			Steps: []model.Step{{ID: "s1", Call: tc.call, Input: tc.in}},
		})
		if err == nil {
			t.Fatalf("[%s] 期望注册期拒绝，实际通过", tc.name)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("[%s] 错误信息应含 %q，实际: %v", tc.name, tc.want, err)
		}
	}
}
