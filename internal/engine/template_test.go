package engine

import (
	"fmt"
	"strings"
	"testing"

	"gear/internal/model"
)

// tplFlow 构造一个单 step 的 Flow（vars 按需给）。
func tplFlow(vars, input map[string]interface{}) *model.Flow {
	return &model.Flow{
		ID: "f", Vars: vars,
		Steps: []model.Step{{ID: "s1", Call: "t", Input: input}},
	}
}

// T-tpl-1：合法引用通过；含嵌套 map / slice 里的引用。
func TestValidateTemplatesOK(t *testing.T) {
	f := tplFlow(
		map[string]interface{}{"topic": "world", "obj": map[string]interface{}{"a": 1}},
		map[string]interface{}{
			"a": "${vars.topic}",
			"b": "${vars.obj.a}", // 深层路径：只校验首段（边界见 validateTemplates 注释）
			"c": []interface{}{"${vars.topic}", map[string]interface{}{"d": "${vars.topic}"}},
		},
	)
	f.Steps = append(f.Steps, model.Step{
		ID: "s2", Call: "t", DependsOn: []string{"s1"},
		Input: map[string]interface{}{"x": "${steps.s1.output.name}-reply"},
	})
	if err := validateTemplates(f); err != nil {
		t.Fatalf("合法模板应通过: %v", err)
	}
}

// T-tpl-2：PRD §4.3 声明了 ${secrets.x}，现已实现（REQ-TMPL-SECRETS）——
// 注册期**允许**该前缀（Flow 定义只存引用，明文密钥留宿主机密源，不落库）；
// 密钥缺失在运行期解析时报错。此处断言注册期不再拒绝 secrets 引用。
func TestValidateTemplatesAllowsSecrets(t *testing.T) {
	f := tplFlow(nil, map[string]interface{}{"token": "${secrets.api_key}"})
	if err := validateTemplates(f); err != nil {
		t.Fatalf("${secrets.x} 现已实现，注册期应放行，实际: %v", err)
	}
	// 但残缺引用（缺密钥名）仍应拒绝。
	f2 := tplFlow(nil, map[string]interface{}{"token": "${secrets}"})
	if err := validateTemplates(f2); err == nil {
		t.Fatal("${secrets} 缺少密钥名，注册期应拒绝")
	}
}

// T-tpl-3：拼错前缀（如 ${nope.x}）注册期拒绝。
func TestValidateTemplatesRejectsUnknownPrefix(t *testing.T) {
	f := tplFlow(nil, map[string]interface{}{"a": "${nope.x}"})
	if err := validateTemplates(f); err == nil {
		t.Fatal("未知前缀应被拒绝")
	}
}

// T-tpl-4：vars 键未定义 → 注册期拒绝（避免等到某次执行才报「键不存在」）。
func TestValidateTemplatesRejectsUnknownVarKey(t *testing.T) {
	f := tplFlow(map[string]interface{}{"topic": "world"}, map[string]interface{}{"a": "${vars.typo}"})
	err := validateTemplates(f)
	if err == nil {
		t.Fatal("未定义的 vars 键应被拒绝")
	}
	if !strings.Contains(err.Error(), "typo") {
		t.Fatalf("错误应指出具体键名，实际: %v", err)
	}
}

// T-tpl-5：残缺引用 ${steps}（不带 step id）→ 拒绝。
func TestValidateTemplatesRejectsBareSteps(t *testing.T) {
	f := tplFlow(nil, map[string]interface{}{"a": "${steps}"})
	if err := validateTemplates(f); err == nil {
		t.Fatal("${steps} 缺少 step id，应被拒绝")
	}
}

// T-tpl-6：模板未闭合 → 拒绝（与运行期展开语义一致，不能注册期放过）。
func TestValidateTemplatesRejectsUnclosed(t *testing.T) {
	f := tplFlow(nil, map[string]interface{}{"a": "${vars.topic"})
	if err := validateTemplates(f); err == nil {
		t.Fatal("未闭合模板应被拒绝")
	}
}

// T-tpl-7（边界声明）：`${steps.<不存在的 id>}` **不由本函数**负责——
// 该项由依赖图（effectiveDeps/topoLevels）唯一判定，避免同一规则两处实现。
// 故此处应通过，而端到端校验（ValidateFlowStatic）仍会拒绝它。
func TestValidateTemplatesDelegatesStepExistence(t *testing.T) {
	f := tplFlow(nil, map[string]interface{}{"a": "${steps.ghost.output.x}"})
	if err := validateTemplates(f); err != nil {
		t.Fatalf("step 存在性归依赖图负责，本函数不应报错: %v", err)
	}
	if _, err := topoLevels(f.Steps); err == nil {
		t.Fatal("依赖图应拒绝引用不存在的 step")
	}
}

// T-tpl-8：展开**不二次展开**——vars 的值本身含 ${...} 时按字面输出，
// 不递归展开（旧实现循环扫描整个字符串，遇到这种值会反复展开甚至死循环）。
func TestExpandStringNoRecursiveExpansion(t *testing.T) {
	ctx := &runContext{
		vars:  map[string]interface{}{"a": "${vars.b}", "b": "x"},
		steps: map[string]map[string]interface{}{},
	}
	got, err := expandString("${vars.a}", ctx)
	if err != nil {
		t.Fatalf("展开失败: %v", err)
	}
	if got != "${vars.b}" {
		t.Fatalf("应输出字面值 ${vars.b}（不二次展开），实际 %q", got)
	}
}

// T-tpl-9：一次字符串里的多个引用都要展开，且后面的引用不被前面的替换影响下标。
func TestExpandStringMultipleRefs(t *testing.T) {
	ctx := &runContext{
		vars:  map[string]interface{}{"a": "AAA", "bb": "BBBBBB"},
		steps: map[string]map[string]interface{}{},
	}
	got, err := expandString("x-${vars.a}-${vars.bb}-y", ctx)
	if err != nil {
		t.Fatalf("展开失败: %v", err)
	}
	if got != "x-AAA-BBBBBB-y" {
		t.Fatalf("多引用展开错误: %q", got)
	}
}

// T-tpl-9b：执行期 ${secrets.x} 从宿主 secret 源解析（REQ-TMPL-SECRETS）。
// 明文密钥只在运行期出现，Flow 定义只存引用。
func TestExpandStringResolvesSecrets(t *testing.T) {
	ctx := &runContext{
		vars:  map[string]interface{}{},
		steps: map[string]map[string]interface{}{},
		secretGet: func(name string) (string, error) {
			if name == "api_key" {
				return "s3cr3t", nil
			}
			return "", fmt.Errorf("secret %q 不存在", name)
		},
	}
	got, err := expandString("Bearer ${secrets.api_key}", ctx)
	if err != nil {
		t.Fatalf("展开失败: %v", err)
	}
	if got != "Bearer s3cr3t" {
		t.Fatalf("secret 未解析，实际 %q", got)
	}

	// 缺失密钥 → 运行期明确报错（不静默空值）。
	if _, err := expandString("${secrets.missing}", ctx); err == nil {
		t.Fatal("缺失 secret 应报错")
	}
}

// T-tpl-9c：宿主未注入 secret 实现时，${secrets.x} 解析报错（不静默失败）。
func TestExpandStringSecretsUnavailable(t *testing.T) {
	ctx := &runContext{vars: map[string]interface{}{}, steps: map[string]map[string]interface{}{}}
	if _, err := expandString("${secrets.api_key}", ctx); err == nil {
		t.Fatal("宿主无 secret 实现时，secrets 解析应报错")
	}
}

// T-tpl-10：未闭合模板在**依赖推断**阶段同样报错（三个消费者共用同一解析器）。
func TestDepsRejectUnclosedTemplate(t *testing.T) {
	steps := []model.Step{{ID: "s1", Call: "t", Input: map[string]interface{}{"a": "${steps.s1"}}}
	if _, err := topoLevels(steps); err == nil {
		t.Fatal("未闭合模板应在依赖推断阶段报错")
	}
}

// T-tpl-11：端到端——照 PRD §4.3 写 ${secrets.x} 的 Flow 现在**注册期放行**
// （REQ-TMPL-SECRETS 已实现：引用合法，密钥在执行期从宿主 secret 源解析）。
func TestSpecSecretsTemplateAllowedAtRegister(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	putTool(t, reg, luaTool("t", `return ctx.input`, objSchema(), objSchema()))
	putFlow(t, reg, &model.Flow{ID: "ok", Vars: map[string]interface{}{"k": "v"}, Steps: []model.Step{
		{ID: "s1", Call: "t", Input: map[string]interface{}{"x": "${vars.k}"}},
	}})

	// 含 ${secrets.x} 的 Flow 现在应通过静态校验（注册期只校验前缀合法性，不校验密钥存在性）。
	good := &model.Flow{ID: "good", Steps: []model.Step{
		{ID: "s1", Call: "t", Input: map[string]interface{}{"token": "${secrets.api_key}"}},
	}}
	if err := New(reg).ValidateFlowStatic(good); err != nil {
		t.Fatalf("含 ${secrets.x} 的 Flow 现应注册期放行，实际: %v", err)
	}
}

// T-tpl-12（回归锁）：**没有 when 的普通 step 必须能通过静态校验**。
//
// 修复前的行为：ValidateFlowStatic 直接调 compileWhen("")，被 expr 判为
// `unexpected token EOF` —— 于是**所有不含 when 的 Flow 都无法通过静态校验**
// （REST `POST /api/flows` 一律 400、CLI 接入静态校验后同样拒收）。
// 之所以长期没暴露：当时 REST 静态校验的用例恰好都带了 `when: "true"`。
// 根因是同一个 compileWhen 出现了两种空值语义（CompileWhen 判空、compileWhen 不判）。
func TestStaticValidateAcceptsFlowWithoutWhen(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	putTool(t, reg, luaTool("t", `return ctx.input`, objSchema(), objSchema()))

	f := &model.Flow{ID: "nowhen", Vars: map[string]interface{}{"k": "v"}, Steps: []model.Step{
		{ID: "s1", Call: "t", Input: map[string]interface{}{"x": "${vars.k}"}},
		{ID: "s2", Call: "t", DependsOn: []string{"s1"}, Input: map[string]interface{}{"y": "plain"}},
	}}
	if err := New(reg).ValidateFlowStatic(f); err != nil {
		t.Fatalf("不含 when 的 Flow 应通过静态校验: %v", err)
	}
	// 空 when 就是「无条件执行」：执行时必须真的跑起来（静态与运行期语义一致）。
	putFlow(t, reg, f)
	res, err := New(reg).RunFlowResult("nowhen")
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if len(res.Skipped) != 0 || len(res.Steps) != 2 {
		t.Fatalf("空 when 应为无条件执行（2 步全跑），实际 steps=%d skipped=%v", len(res.Steps), res.Skipped)
	}
}
