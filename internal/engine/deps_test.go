package engine

import (
	"testing"

	"gear/internal/model"
)

func stepsOf(ids ...string) []model.Step {
	out := make([]model.Step, 0, len(ids))
	for _, id := range ids {
		out = append(out, model.Step{ID: id, Call: "x"})
	}
	return out
}

// D-1：无依赖的 step 全部落在同一层（即可并行）。
func TestTopoLevelsIndependent(t *testing.T) {
	levels, err := topoLevels(stepsOf("a", "b", "c"))
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if len(levels) != 1 || len(levels[0]) != 3 {
		t.Fatalf("3 个无依赖 step 应在同一层，实际 %v", levels)
	}
}

// D-2：链式依赖逐层展开。
func TestTopoLevelsChain(t *testing.T) {
	s := stepsOf("a", "b", "c")
	s[1].DependsOn = []string{"a"}
	s[2].DependsOn = []string{"b"}
	levels, err := topoLevels(s)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if len(levels) != 3 {
		t.Fatalf("链式依赖应分 3 层，实际 %d 层: %v", len(levels), levels)
	}
}

// D-3：菱形依赖——b/c 可并行，d 在最后。
func TestTopoLevelsDiamond(t *testing.T) {
	s := stepsOf("a", "b", "c", "d")
	s[1].DependsOn = []string{"a"}
	s[2].DependsOn = []string{"a"}
	s[3].DependsOn = []string{"b", "c"}
	levels, err := topoLevels(s)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if len(levels) != 3 {
		t.Fatalf("菱形依赖应分 3 层，实际 %v", levels)
	}
	if len(levels[1]) != 2 {
		t.Fatalf("b/c 应在同一层（可并行），实际 %v", levels[1])
	}
	if len(levels[2]) != 1 || levels[2][0] != 3 {
		t.Fatalf("d 应在最后一层，实际 %v", levels[2])
	}
}

// D-4：循环依赖被检出。
func TestTopoLevelsCycle(t *testing.T) {
	s := stepsOf("a", "b")
	s[0].DependsOn = []string{"b"}
	s[1].DependsOn = []string{"a"}
	if _, err := topoLevels(s); err == nil {
		t.Fatal("循环依赖应报错")
	}
}

// D-5：**依赖推断** —— input 引用 ${steps.X.output.y} 即隐式依赖 X（无须写 depends_on）。
func TestInferDepsFromInput(t *testing.T) {
	s := stepsOf("b", "a") // 故意把被引用者放在后面
	s[0].Input = map[string]interface{}{"v": "${steps.a.output.name}"}
	levels, err := topoLevels(s)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if len(levels) != 2 {
		t.Fatalf("引用应产生隐式依赖并分层，实际 %v", levels)
	}
	if levels[0][0] != 1 {
		t.Fatalf("被引用的 a（下标 1）应先执行，实际首层 %v", levels[0])
	}
}

// D-6：引用不存在的 step → 显式报错（不再等到运行时才炸）。
func TestInferDepsUnknownRef(t *testing.T) {
	s := stepsOf("a")
	s[0].Input = map[string]interface{}{"v": "${steps.ghost.output.x}"}
	if _, err := topoLevels(s); err == nil {
		t.Fatal("引用不存在的 step 应报错")
	}
}

// D-7：自引用报错。
func TestInferDepsSelfRef(t *testing.T) {
	s := stepsOf("a")
	s[0].Input = map[string]interface{}{"v": "${steps.a.output.x}"}
	if _, err := topoLevels(s); err == nil {
		t.Fatal("自引用应报错")
	}
}

// D-8：嵌套结构（map/slice）里的引用也能被推断出来。
func TestInferDepsNested(t *testing.T) {
	s := stepsOf("a", "b")
	s[1].Input = map[string]interface{}{
		"deep": map[string]interface{}{"list": []interface{}{"${steps.a.output.name}"}},
	}
	levels, err := topoLevels(s)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if len(levels) != 2 || levels[0][0] != 0 {
		t.Fatalf("嵌套引用应被推断，实际 %v", levels)
	}
}

// D-9：显式依赖指向不存在的 step 仍报错。
func TestEffectiveDepsMissingExplicit(t *testing.T) {
	s := stepsOf("a")
	s[0].DependsOn = []string{"ghost"}
	if _, err := topoLevels(s); err == nil {
		t.Fatal("显式依赖不存在的 step 应报错")
	}
}

// D-10（回归锁）：**when 里的引用同样构成依赖**。
// 曾经的 bug：只扫 input，导致 when 引用上游输出的 step 与上游同层并发，
// 求值时读到还没产出的 output（`cannot fetch n from <nil>`）。
func TestInferDepsFromWhen(t *testing.T) {
	s := stepsOf("judge", "src")         // judge 写在前面
	s[0].When = "steps.src.output.n > 1" // 仅 when 里引用 src，input 为空
	levels, err := topoLevels(s)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if len(levels) != 2 {
		t.Fatalf("when 引用应产生依赖并分层，实际 %v", levels)
	}
	if levels[0][0] != 1 {
		t.Fatalf("被 when 引用的 src（下标 1）应先执行，实际首层 %v", levels[0])
	}
}

// D-11：when 引用不存在的 step → 显式报错。
func TestInferDepsWhenUnknownRef(t *testing.T) {
	s := stepsOf("a")
	s[0].When = "steps.ghost.output.n > 1"
	if _, err := topoLevels(s); err == nil {
		t.Fatal("when 引用不存在的 step 应报错")
	}
}
