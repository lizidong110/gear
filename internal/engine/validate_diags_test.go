package engine

// validate_diags_test.go —— 静态校验「一次报全」的**行为**守卫（REQ-MCP-DIAGS）。
//
// 为什么单独立文件：MCP 面的 `gear_validate` 是智能体的「编译器」，其全部价值建立在
// 「改一处坏 YAML，一次拿到全部错」上。这条性质住在 **engine** 层（validate_static.go），
// 而此前只有 mcp 包的端到端用例间接覆盖它——一旦有人把逐 step 收集改回整体 fail-fast，
// mcp 那条用例的失败信息指向的是 MCP 层，定位成本高。这里在行为归属方直测。
//
// 分两层断言：
//  1. **step 局部问题不互相掩盖**：多个 step 各坏一处 → 一次拿到每 step 一条；
//  2. **整体不变量仍 fail-fast**：缺 id / 无 step 只报一条（刻意的取舍，不是遗漏）。

import (
	"errors"
	"testing"

	"gear/internal/model"
)

// brokenFlowWithFiveStepErrors 把 5 个**互相独立**的 step 局部问题塞进一条 Flow。
//
// 每一处都只在它自己那一步成立，故「收集到几条」直接反映「有没有被前一步掩盖」：
//   - s1 调不存在的 Tool        → UNKNOWN_CALL_TARGET
//   - s2 调不存在的「能力.方法」 → UNKNOWN_CAPABILITY（与 s1 是**两种**码：一个是 id 查不到，
//     一个是能力名不存在——分开才能让智能体知道该去 catalog 找 Tool 还是去 capabilities 找能力）
//   - s3 when 误用 ${}          → BAD_WHEN（走结构判定，因为 model 层直接拒）
//   - s4 input 模板前缀非法     → BAD_TEMPLATE
//   - s5 直调缺必填参数         → BAD_PRIMITIVE_ARGS
func brokenFlowWithFiveStepErrors() *model.Flow {
	return &model.Flow{
		ID: "diag.up",
		Vars: map[string]interface{}{
			"flag": "1", // 让 s3 的 ${vars.flag} 只剩「when 不得用模板」这一个问题
		},
		Steps: []model.Step{
			{ID: "s1", Call: "no_such_tool"},
			{ID: "s2", Call: "bogus.method"},
			{ID: "s3", Call: "db.exec", When: "${vars.flag}", Input: map[string]interface{}{"sql": "SELECT 1"}},
			{ID: "s4", Call: "db.exec", Input: map[string]interface{}{"sql": "${bogus.x}"}},
			{ID: "s5", Call: "db.exec", Input: map[string]interface{}{}}, // 缺必填 sql
		},
	}
}

// 逐 step 收集：5 处坏点必须一次全报，且各带自己的 step 定位与 error code。
func TestValidateFlowStaticDiagsReportsAllStepErrors(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	e := New(reg)

	diags := e.ValidateFlowStaticDiags(brokenFlowWithFiveStepErrors())
	if len(diags) < 5 {
		t.Fatalf("「编译器」应一次报全 5 处错误，实际只拿到 %d 条: %+v", len(diags), diags)
	}

	// 按 step 归口：每个坏 step 都得有诊断，且 code 与预期一致。
	want := map[string]string{
		"s1": CodeUnknownCallTarget,
		"s2": CodeUnknownCapability,
		"s3": CodeBadWhen,
		"s4": CodeBadTemplate,
		"s5": CodeBadPrimitiveArgs,
	}
	got := map[string]string{}
	for _, d := range diags {
		if d.Step == "" {
			t.Errorf("step 局部问题必须带 step 定位: %+v", d)
			continue
		}
		if _, dup := got[d.Step]; dup {
			t.Errorf("step %s 报了多条（同一处问题不应重复）: %+v", d.Step, d)
		}
		got[d.Step] = d.Code
		if d.Message == "" {
			t.Errorf("诊断必须带人类可读 message: %+v", d)
		}
	}
	for step, code := range want {
		if got[step] != code {
			t.Errorf("step %s 的诊断码应为 %s，实际 %q（该 step 的问题可能被前一步掩盖）", step, code, got[step])
		}
	}
}

// 直调入参的**两类**误写（拼错名 / 漏必填）都必须被挡住——这是最常见的两类手写错误。
func TestValidatePrimitiveArgsCatchesUnknownAndMissing(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	e := New(reg)

	cases := []struct {
		name  string
		input map[string]interface{}
	}{
		{"未知参数名", map[string]interface{}{"sql": "SELECT 1", "kay": "k"}},
		{"缺必填参数", map[string]interface{}{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &model.Flow{ID: "f", Steps: []model.Step{{ID: "s1", Call: "db.exec", Input: c.input}}}
			diags := e.ValidateFlowStaticDiags(f)
			if len(diags) != 1 || diags[0].Code != CodeBadPrimitiveArgs {
				t.Fatalf("%s 应报恰好一条 %s，实际 %+v", c.name, CodeBadPrimitiveArgs, diags)
			}
			if diags[0].Hint == "" {
				t.Errorf("诊断要告诉智能体怎么修（hint 为空）: %+v", diags[0])
			}
		})
	}
}

// 整体不变量仍 fail-fast：缺 id / 一个 step 都没有 → 恰好一条，不产派生噪声。
func TestValidateFlowStaticDiagsFailsFastOnWholeFlowInvariants(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	e := New(reg)

	cases := []struct {
		name string
		flow *model.Flow
	}{
		{"缺 id", &model.Flow{Steps: []model.Step{{ID: "s1", Call: "no_such_tool"}}}},
		{"无 step", &model.Flow{ID: "f"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			diags := e.ValidateFlowStaticDiags(c.flow)
			if len(diags) != 1 {
				t.Fatalf("%s 应只报整体不变量一条（刻意 fail-fast），实际 %d 条: %+v", c.name, len(diags), diags)
			}
			if diags[0].Code != CodeBadFlowStructure {
				t.Errorf("%s 的码应为 %s，实际 %s", c.name, CodeBadFlowStructure, diags[0].Code)
			}
			if diags[0].Hint == "" {
				t.Errorf("整体不变量诊断必须带修法: %+v", diags[0])
			}
		})
	}
}

// 合法 Flow 不得报任何诊断——「一次报全」不能以误报为代价。
func TestValidateFlowStaticDiagsCleanFlowIsSilent(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	if err := reg.UpsertTool(&model.Tool{
		ID: "hello", Runtime: model.RuntimeProcess, Entry: "cat",
		InputSchema:  map[string]interface{}{"type": "object"},
		OutputSchema: map[string]interface{}{"type": "object"},
		Versions:     []model.ToolVersion{{Version: "1.0.0", State: model.StateStable, Entry: "cat"}},
	}); err != nil {
		t.Fatal(err)
	}
	f := &model.Flow{ID: "f", Steps: []model.Step{
		{ID: "s1", Call: "hello"},
		{ID: "s2", Call: "db.exec", Input: map[string]interface{}{"sql": "SELECT 1"}},
	}}
	if diags := New(reg).ValidateFlowStaticDiags(f); len(diags) != 0 {
		t.Fatalf("合法 Flow 不应有诊断，实际: %+v", diags)
	}
}

// 回归锁：`ValidateFlowStatic` 仍是「首条」包装——签名与错误文本逐字不变（既有调用方零改动），
// 且 `errors.As` 能取出结构化诊断。
func TestValidateFlowStaticReturnsFirstDiagOnly(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	e := New(reg)

	f := brokenFlowWithFiveStepErrors()
	diags := e.ValidateFlowStaticDiags(f)
	if len(diags) < 2 {
		t.Fatalf("前置条件：本 Flow 应有多条诊断，实际 %d", len(diags))
	}

	err := e.ValidateFlowStatic(f)
	if err == nil {
		t.Fatal("坏 Flow 必须返回 error")
	}
	var de *DiagError
	if !errors.As(err, &de) {
		t.Fatalf("error 链上应能取出 *DiagError，实际 %T", err)
	}
	if de.Diag.Code != diags[0].Code || de.Diag.Message != diags[0].Message {
		t.Errorf("ValidateFlowStatic 应返回首条诊断，want %+v got %+v", diags[0], de.Diag)
	}
	if err.Error() != de.Diag.Message {
		t.Errorf("Error() 必须等于诊断 message（对外可见文本不变），got %q want %q", err.Error(), de.Diag.Message)
	}
}
