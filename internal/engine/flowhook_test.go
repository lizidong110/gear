package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gear/internal/model"
)

// F-hook-1：before_flow Hook（Tool 载体）在 Flow 启动前改写初始 vars，下游 step 看到改后值。
func TestBeforeFlowHookToolRewritesVars(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	// echo：原样返回 input（objSchema，无必填字段）
	putTool(t, reg, luaTool("echo", `return ctx.input`, objSchema(), objSchema()))
	// before_flow 载体：把 vars.base 加 1
	putTool(t, reg, luaTool("bf", `return { base = ctx.input.base + 1 }`, objSchema(), objSchema()))
	putHook(t, reg, &model.Hook{ID: "hbf", Target: "*", Stage: model.StageBeforeFlow, Action: "bf"})
	putFlow(t, reg, &model.Flow{
		ID:   "f",
		Vars: map[string]interface{}{"base": 10},
		Steps: []model.Step{
			{ID: "s1", Call: "echo", Input: map[string]interface{}{"base": "${vars.base}"}},
		},
	})

	res, err := New(reg).RunFlowResult("f")
	if err != nil {
		t.Fatalf("RunFlow 失败: %v", err)
	}
	o, ok := res.Steps["s1"]["output"].(map[string]interface{})
	if !ok {
		t.Fatalf("s1 无 output: %v", res.Steps["s1"])
	}
	if got := fmt.Sprint(o["base"]); got != "11" {
		t.Fatalf("before_flow 应把 base 改写为 11，实际 %q", got)
	}
}

// F-hook-2：before_flow Hook（Flow 载体，#72）同步内联执行子 Flow，结果暴露进主 Flow vars。
func TestBeforeFlowHookFlowInline(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	putTool(t, reg, luaTool("echo", `return ctx.input`, objSchema(), objSchema()))
	// 子 Flow：产出 val=42
	putFlow(t, reg, &model.Flow{
		ID: "sub",
		Steps: []model.Step{
			{ID: "ss", Call: "echo", Input: map[string]interface{}{"val": 42}},
		},
	})
	// 主 Flow：before_flow 内联执行 sub，下游经 vars.sub.ss.output.val 引用
	// 注意：Target 必须指定主 Flow id "f"（而非 "*"），否则会同时命中子 Flow 自身引发循环（递归守卫会拦截）。
	putHook(t, reg, &model.Hook{ID: "hbf", Target: "f", Stage: model.StageBeforeFlow, Action: "sub"})
	putFlow(t, reg, &model.Flow{
		ID: "f",
		Steps: []model.Step{
			{ID: "s1", Call: "echo", Input: map[string]interface{}{"fromSub": "${vars.sub.ss.output.val}"}},
		},
	})

	res, err := New(reg).RunFlowResult("f")
	if err != nil {
		t.Fatalf("RunFlow 失败: %v", err)
	}
	o, ok := res.Steps["s1"]["output"].(map[string]interface{})
	if !ok {
		t.Fatalf("s1 无 output: %v", res.Steps["s1"])
	}
	if got := fmt.Sprint(o["fromSub"]); got != "42" {
		t.Fatalf("内联子 Flow 结果应暴露给 vars.sub.ss.output.val=42，实际 %q", got)
	}
}

// F-hook-3：on_success Hook（Tool 载体）仅在 Flow 全部成功时触发，payload 为 RunResult。
func TestOnSuccessHookRunsAfterSuccess(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	putTool(t, reg, luaTool("echo", `return ctx.input`, objSchema(), objSchema()))
	// on_success 载体：把收到的 RunResult（stdin JSON）写入临时文件，证明它确实跑了且拿到结果。
	// 同时向 stdout 输出 "{}" —— process runtime 要求 stdout 末行为合法 JSON，否则报「stdout 为空」。
	// 注意：Windows 临时路径含 `\0` 等，经 `sh -c` 会被解释为 NUL，故传给 shell 的路径用正斜杠（Git Bash 可解析）。
	tmpWin := filepath.Join(t.TempDir(), "onsuccess.out")
	tmpSh := filepath.ToSlash(tmpWin)
	putTool(t, reg, procTool("notify", fmt.Sprintf("cat > %s; echo '{}'", tmpSh), objSchema(), objSchema()))
	putHook(t, reg, &model.Hook{ID: "hos", Target: "*", Stage: model.StageOnSuccess, Action: "notify"})
	putFlow(t, reg, &model.Flow{
		ID: "f",
		Steps: []model.Step{
			{ID: "s1", Call: "echo", Input: map[string]interface{}{"msg": "hi"}},
		},
	})

	if _, err := New(reg).RunFlowResult("f"); err != nil {
		t.Fatalf("RunFlow 失败: %v", err)
	}
	b, err := os.ReadFile(tmpWin)
	if err != nil {
		t.Fatalf("on_success Hook 应已写入结果文件: %v", err)
	}
	if !strings.Contains(string(b), "hi") {
		t.Fatalf("on_success 收到的 RunResult 应包含 msg=hi，实际文件内容: %s", string(b))
	}
}

// F-hook-4：Flow 级 Hook 的 Target 按 flow id 匹配 —— 命中才生效，不命中不生效。
func TestFlowHookTargetFilter(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	putTool(t, reg, luaTool("echo", `return ctx.input`, objSchema(), objSchema()))
	putTool(t, reg, luaTool("bf", `return { base = 999 }`, objSchema(), objSchema()))

	// 命中：Target 为 "*" 或精确 flow id "f" 都应改写 base。
	for _, target := range []string{"*", "f"} {
		reg2 := newTestRegistry(t)
		putTool(t, reg2, luaTool("echo", `return ctx.input`, objSchema(), objSchema()))
		putTool(t, reg2, luaTool("bf", `return { base = 999 }`, objSchema(), objSchema()))
		putHook(t, reg2, &model.Hook{ID: "hbf", Target: target, Stage: model.StageBeforeFlow, Action: "bf"})
		putFlow(t, reg2, &model.Flow{
			ID:   "f",
			Vars: map[string]interface{}{"base": 10},
			Steps: []model.Step{
				{ID: "s1", Call: "echo", Input: map[string]interface{}{"base": "${vars.base}"}},
			},
		})
		res, err := New(reg2).RunFlowResult("f")
		if err != nil {
			t.Fatalf("target=%q RunFlow 失败: %v", target, err)
		}
		o := res.Steps["s1"]["output"].(map[string]interface{})
		if fmt.Sprint(o["base"]) != "999" {
			t.Fatalf("target=%q 应改写 base=999，实际 %q", target, fmt.Sprint(o["base"]))
		}
		reg2.Close()
	}

	// 不命中：Target 指向其它 flow id，不生效（base 保持 10）。
	putHook(t, reg, &model.Hook{ID: "hbf", Target: "other-flow", Stage: model.StageBeforeFlow, Action: "bf"})
	putFlow(t, reg, &model.Flow{
		ID:   "f",
		Vars: map[string]interface{}{"base": 10},
		Steps: []model.Step{
			{ID: "s1", Call: "echo", Input: map[string]interface{}{"base": "${vars.base}"}},
		},
	})
	res, err := New(reg).RunFlowResult("f")
	if err != nil {
		t.Fatalf("Target 不命中 RunFlow 失败: %v", err)
	}
	o := res.Steps["s1"]["output"].(map[string]interface{})
	if fmt.Sprint(o["base"]) != "10" {
		t.Fatalf("Target 不命中的 Hook 不应生效（base 应=10），实际 %q", fmt.Sprint(o["base"]))
	}
}

// F-hook-5：before_flow 内联子 Flow 不会无限递归（结构性无递归），可正常返回。
func TestBeforeFlowHookFlowNoInfiniteRecursion(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	putTool(t, reg, luaTool("echo", `return ctx.input`, objSchema(), objSchema()))
	// 子 Flow 自身没有命中它的 before_flow Hook（Target="f" 仅匹配主 Flow），因此只跑一次，不会递归。
	putFlow(t, reg, &model.Flow{
		ID: "sub",
		Steps: []model.Step{
			{ID: "ss", Call: "echo", Input: map[string]interface{}{"val": 7}},
		},
	})
	putHook(t, reg, &model.Hook{ID: "hbf", Target: "f", Stage: model.StageBeforeFlow, Action: "sub"})
	putFlow(t, reg, &model.Flow{
		ID: "f",
		Steps: []model.Step{
			{ID: "s1", Call: "echo", Input: map[string]interface{}{"fromSub": "${vars.sub.ss.output.val}"}},
		},
	})

	res, err := New(reg).RunFlowResult("f")
	if err != nil {
		t.Fatalf("RunFlow 失败（疑似递归挂死）: %v", err)
	}
	o := res.Steps["s1"]["output"].(map[string]interface{})
	if fmt.Sprint(o["fromSub"]) != "7" {
		t.Fatalf("内联子 Flow 应正常返回 val=7，实际 %q", fmt.Sprint(o["fromSub"]))
	}
}

// F-hook-6：递归守卫 —— before_flow 的 Flow 载体若会造成循环调用（如 Target="*" 钩中自身），
// 必须被拦截并返回错误（fail-closed），绝不能无限递归挂死。
func TestBeforeFlowHookFlowCycleGuarded(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	putTool(t, reg, luaTool("echo", `return ctx.input`, objSchema(), objSchema()))
	// 自引用的 before_flow Hook：Target="*" 会命中 f 自身，action 又指向 f → 循环。
	putHook(t, reg, &model.Hook{ID: "hbf", Target: "*", Stage: model.StageBeforeFlow, Action: "f"})
	putFlow(t, reg, &model.Flow{
		ID: "f",
		Steps: []model.Step{
			{ID: "s1", Call: "echo", Input: map[string]interface{}{"x": 1}},
		},
	})

	_, err := New(reg).RunFlowResult("f")
	if err == nil {
		t.Fatal("自引用的 before_flow 应被递归守卫拦截并报错，而非无限递归")
	}
	if !strings.Contains(err.Error(), "循环") {
		t.Fatalf("错误应指明循环调用，实际: %v", err)
	}
}
