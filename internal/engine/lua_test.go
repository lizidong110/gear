package engine

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"gear/internal/model"
)

// luaRun 是测试用的便捷封装。
func luaRun(t *testing.T, entry string, input map[string]interface{}) (map[string]interface{}, error) {
	t.Helper()
	rt, err := runtimeFor(model.RuntimeLua)
	if err != nil {
		t.Fatalf("lua runtime 应可用: %v", err)
	}
	return rt.Run(context.Background(), entry, input)
}

// S7a-1：once 正常 —— ctx.input 进、return table 出。
func TestLuaRunBasic(t *testing.T) {
	out, err := luaRun(t, `return { msg = ctx.input.name .. "-lua" }`, map[string]interface{}{"name": "hi"})
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	if fmt.Sprint(out["msg"]) != "hi-lua" {
		t.Fatalf("输出不符: %v", out)
	}
}

// S7a-2：ctx.output 写入路径生效（脚本无 return）。
func TestLuaCtxOutput(t *testing.T) {
	out, err := luaRun(t, `ctx.output = { ok = true, n = 3 }`, nil)
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	if out["ok"] != true {
		t.Fatalf("ok 应为 true: %v", out)
	}
	// 整数应转成 int64（让 number/integer 两种 schema 都能过）。
	if fmt.Sprint(out["n"]) != "3" {
		t.Fatalf("n 应为 3: %v", out)
	}
	if _, isInt := out["n"].(int64); !isInt {
		t.Fatalf("整数值应转 int64，实际 %T", out["n"])
	}
}

// S7a-3：沙箱 —— os/io/require/dofile 不可见（无宿主逃逸）。
func TestLuaSandboxGlobals(t *testing.T) {
	out, err := luaRun(t, `return { os = type(os), io = type(io), require = type(require), dofile = type(dofile) }`, nil)
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	for _, k := range []string{"os", "io", "require", "dofile"} {
		if fmt.Sprint(out[k]) != "nil" {
			t.Fatalf("沙箱应移除全局 %s，实际 type=%v", k, out[k])
		}
	}
}

// S7a-4：沙箱 —— 调用 os.system 必须失败。
func TestLuaSandboxEscapeBlocked(t *testing.T) {
	if _, err := luaRun(t, `os.system("echo hi")`, nil); err == nil {
		t.Fatal("os.system 应不可用（沙箱逃逸应失败）")
	}
}

// S7a-5：语法错误报错。
func TestLuaSyntaxError(t *testing.T) {
	if _, err := luaRun(t, `return {`, nil); err == nil {
		t.Fatal("语法错误应报错")
	}
}

// S7a-6：运行时错误传播。
func TestLuaRuntimeError(t *testing.T) {
	if _, err := luaRun(t, `error("boom")`, nil); err == nil {
		t.Fatal("error() 应传播为错误")
	}
}

// S7a-7：无 output 报错（既没 return table 也没写 ctx.output）。
func TestLuaNoOutput(t *testing.T) {
	if _, err := luaRun(t, `local x = 1`, nil); err == nil {
		t.Fatal("无输出应报错")
	}
	if _, err := luaRun(t, `return 42`, nil); err == nil {
		t.Fatal("返回非 table 应报错")
	}
}

// S7a-8：死循环被超时中断（SetContext 生效，防挂死）。
func TestLuaTimeout(t *testing.T) {
	rt, err := runtimeFor(model.RuntimeLua)
	if err != nil {
		t.Fatalf("lua runtime 应可用: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, runErr := rt.Run(ctx, `while true do end`, nil)
		done <- runErr
	}()
	select {
	case runErr := <-done:
		if runErr == nil {
			t.Fatal("死循环应被超时中断")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("死循环未被中断（SetContext 未生效）")
	}
}

// S7a-9：lua 不支持 service，Start 返回明确错误。
func TestLuaServiceUnsupported(t *testing.T) {
	rt, err := runtimeFor(model.RuntimeLua)
	if err != nil {
		t.Fatalf("lua runtime 应可用: %v", err)
	}
	if _, err := rt.Start(context.Background(), `return {}`, nil); err == nil {
		t.Fatal("lua Start 应报错（不支持 service）")
	} else if !strings.Contains(err.Error(), "不支持 service") {
		t.Fatalf("错误信息应说明不支持 service，实际: %v", err)
	}
}

// S7a-10：数组 / 嵌套 table / 非整浮点的双向转换。
func TestLuaArrayAndNested(t *testing.T) {
	out, err := luaRun(t, `
local sum = 0
for _, v in ipairs(ctx.input.nums) do sum = sum + v end
return {
  list   = { 1, 2, 3 },
  nested = { a = { b = "c" } },
  f      = 1.5,
  sum    = sum,
}`, map[string]interface{}{"nums": []interface{}{1, 2, 3, 4}})
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	list, ok := out["list"].([]interface{})
	if !ok || len(list) != 3 {
		t.Fatalf("list 应为长度 3 的数组，实际 %T=%v", out["list"], out["list"])
	}
	if fmt.Sprint(list[0]) != "1" {
		t.Fatalf("list[0] 应为 1: %v", list[0])
	}
	nested, ok := out["nested"].(map[string]interface{})
	if !ok {
		t.Fatalf("nested 应为对象: %T", out["nested"])
	}
	inner, ok := nested["a"].(map[string]interface{})
	if !ok || fmt.Sprint(inner["b"]) != "c" {
		t.Fatalf("nested.a.b 应为 c: %v", nested)
	}
	if fmt.Sprint(out["f"]) != "1.5" {
		t.Fatalf("f 应为 1.5: %v", out["f"])
	}
	// 输入数组求和（验证 Go slice → Lua table 方向）。
	if fmt.Sprint(out["sum"]) != "10" {
		t.Fatalf("sum 应为 10: %v", out["sum"])
	}
}

// S7a-11：lua Tool 经 registry + 引擎端到端跑通（规格一致性）。
func TestLuaFlowEndToEnd(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()

	tool := &model.Tool{
		ID: "lua_echo", Name: "Lua Echo", Runtime: model.RuntimeLua,
		Entry: `return { msg = ctx.input.name .. "-lua" }`,
		InputSchema: map[string]interface{}{
			"type": "object", "properties": map[string]interface{}{"name": map[string]interface{}{"type": "string"}},
			"required": []interface{}{"name"},
		},
		OutputSchema: map[string]interface{}{
			"type": "object", "properties": map[string]interface{}{"msg": map[string]interface{}{"type": "string"}},
			"required": []interface{}{"msg"},
		},
		Versions: []model.ToolVersion{{Version: "1.0.0", State: model.StateStable}},
	}
	if err := reg.UpsertTool(tool); err != nil {
		t.Fatalf("lua Tool 应可注册: %v", err)
	}
	flow := &model.Flow{ID: "luaflow", Vars: map[string]interface{}{"who": "world"}, Steps: []model.Step{
		{ID: "s1", Call: "lua_echo", Input: map[string]interface{}{"name": "${vars.who}"}},
	}}
	if err := reg.UpsertFlow(flow); err != nil {
		t.Fatalf("注册 Flow 失败: %v", err)
	}
	if err := New(reg).RunFlow("luaflow"); err != nil {
		t.Fatalf("lua Flow 应跑通: %v", err)
	}
}
