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
//  3. 引用的 Tool 必须已注册
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
		t, err := e.reg.GetTool(s.Call)
		if err != nil {
			return err
		}
		if t == nil {
			return fmt.Errorf("step %s 引用的 Tool 不存在: %s", s.ID, s.Call)
		}
		// S5：钉版本必须存在，否则运行期解析会失败——提前到注册期暴露。
		if s.UseVersion != "" {
			ok := false
			for _, v := range t.Versions {
				if v.Version == s.UseVersion {
					ok = true
					break
				}
			}
			if !ok {
				return fmt.Errorf("step %s 钉了 use_version=%s，但 Tool %s 不存在该版本", s.ID, s.UseVersion, s.Call)
			}
		}
	}
	return nil
}
