package engine

import (
	"errors"
	"fmt"
	"strings"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/vm"

	"gear/internal/model"
)

// ErrStepSkipped 表示 step 因 `when` 为假（或上游被跳过）而**未执行**。
//
// 它是控制流信号，**不是失败**：调用方必须用 errors.Is 与真实错误区分开
// （跳过 → 记录 Skipped 并继续；失败 → fail-closed 终止）。见方法论 F6。
var ErrStepSkipped = errors.New("step skipped")

// whenEnv 是 `when` 表达式的求值环境。
//
// 关键设计：`when` 用**表达式引擎**（带类型），而不是把 `${...}` 插值进字符串再 eval。
// 原因：插值会丢失类型（"200" vs 200），产生 `x != 200` 永远为真的幽灵 bug。
// 根变量名用 expr tag 固定为小写 `vars` / `steps`，与模板前缀保持一致；
// 写错根名（如 `step.`）会在**编译期**就报错。
type whenEnv struct {
	Vars  map[string]any            `expr:"vars"`
	Steps map[string]map[string]any `expr:"steps"`
}

// CompileWhen 编译并静态校验一个 when 表达式（不执行）。
// 供**注册期**调用，把「用户把表达式写错」前移到注册时暴露（F3）。
// 空表达式视为「无条件执行」，返回 nil。
func CompileWhen(when string) error {
	_, err := compileWhen(when)
	return err
}

// compileWhen 编译 when 表达式；**空表达式返回 (nil, nil)** 表示「无条件执行」。
//
// 空值语义只在这里判定一次（evalWhen 对 nil program 返回 true）。
// 修复记录：此前空值判定被放在 CompileWhen 里、compileWhen 不判空，于是静态校验路径
// （ValidateFlowStatic 直接调 compileWhen）对「没有 when 的普通 step」报
// `unexpected token EOF` —— **所有不含 when 的 Flow 都会被注册期拒绝**。
// 同一个函数出现两种空值语义是必踩的坑：判定只能有一处。
func compileWhen(when string) (*vm.Program, error) {
	if strings.TrimSpace(when) == "" {
		return nil, nil
	}
	if strings.Contains(when, "${") {
		return nil, fmt.Errorf("when 是表达式，不使用 ${} 模板语法：请直接写变量名，如 `steps.s1.output.n > 1`")
	}
	prog, err := expr.Compile(when, expr.Env(whenEnv{}), expr.AllowUndefinedVariables())
	if err != nil {
		return nil, fmt.Errorf("when 表达式非法: %w", err)
	}
	return prog, nil
}

// evalWhen 求值 when 表达式；空 when 恒为 true（无条件执行）。
// env 里的 steps 快照由调用方在锁内拷贝，避免并行写入时读取。
func evalWhen(prog *vm.Program, env whenEnv) (bool, error) {
	if prog == nil {
		return true, nil
	}
	out, err := expr.Run(prog, env)
	if err != nil {
		return false, fmt.Errorf("when 求值失败: %w", err)
	}
	b, ok := out.(bool)
	if !ok {
		return false, fmt.Errorf("when 表达式必须返回布尔值，实际得到 %T（%v）", out, out)
	}
	return b, nil
}

// whenHolds 在 step 执行前判断 when：为真才继续执行，为假返回 ErrStepSkipped。
// 空 when 由 compileWhen 统一判定为 nil program，evalWhen 对 nil 返回 true（无条件执行）。
func (e *Engine) whenHolds(rc *runContext, step model.Step) error {
	prog, err := compileWhen(step.When)
	if err != nil {
		return fmt.Errorf("step %s: %w", step.ID, err)
	}
	vars, steps := rc.snapshot()
	ok, err := evalWhen(prog, whenEnv{Vars: vars, Steps: steps})
	if err != nil {
		return fmt.Errorf("step %s: %w", step.ID, err)
	}
	if !ok {
		return ErrStepSkipped
	}
	return nil
}
