package engine

import (
	"fmt"

	"gear/internal/model"
)

// ValidateFlowStatic 对 Flow 做**静态校验**（不执行任何 step）。
//
// 校验层次（PRD §9.2「引擎静态校验」）：
//  1. 结构不变量（`model.Flow.Validate`：id/step/call/mode/未实现字段）
//  2. 依赖图：有效依赖（显式 + 推断）无环、无未知/自引用
//  3. step 的调用目标必须可解析：已注册 Tool，或「能力.方法」直调的原子能力
//     （解析规则与运行期共用 resolveStepCall，只有一处真相）；直调还额外校验
//     input 的参数名与必填项（把拼错参数名这类错误挡在注册那一刻）
//  4. `when` 表达式必须可编译
//  5. 模板引用：前缀必须是已实现的 vars/steps、vars 键存在、`${steps…}` 必须带 step id
//     （模板语法与其边界见 template.go）
//
// 供 CLI `flow register/validate`、REST `POST /api/flows`、以及 AI 生成闭环的
// 「生成 → 校验 → 修正」循环复用——共用同一份校验，避免规则漂移（§0 单一真相源）。
func (e *Engine) ValidateFlowStatic(f *model.Flow) error {
	if err := f.Validate(); err != nil {
		return err
	}
	if _, err := topoLevels(f.Steps); err != nil {
		return err
	}
	if err := validateTemplates(f); err != nil {
		return err
	}
	for _, s := range f.Steps {
		if _, err := compileWhen(s.When); err != nil {
			return fmt.Errorf("step %s: %w", s.ID, err)
		}
		call, err := e.resolveStepCall(s)
		if err != nil {
			return err
		}
		if !call.isPrimitive() {
			continue
		}
		// 直调原子能力：参数校验。input 里的值可能是模板串（如 ${vars.k}），
		// 但**键名**此刻已确定，足以查出「拼错参数名 / 漏必填」——这正是最常见的两类误写。
		if err := e.validatePrimitiveArgs(call.prim, call.method, s.Input); err != nil {
			return fmt.Errorf("step %s: %w", s.ID, err)
		}
	}
	return nil
}
