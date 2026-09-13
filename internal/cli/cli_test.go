package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// validTool 是一个可成功注册的 process runtime Tool（覆盖 Tool.Validate 全部不变量）。
const validTool = `id: t1
name: demo tool
description: 示例
runtime: process
entry: echo
input_schema:
  type: object
output_schema:
  type: object
versions:
  - version: 1.0.0
    state: stable
`

// validFlow 引用已注册 Tool t1，单 step、when 合法表达式（"true"）。
const validFlow = `id: f1
name: demo flow
description: 示例
steps:
  - id: s1
    call: t1
    input:
      x: 1
    when: "true"
`

// validHook 指向已注册 Tool t1（action 是否真实存在不影响 Hook 级校验）。
const validHook = `id: h1
target: "*"
stage: before_step
action: t1
criticality: critical
order: 1
`

// runCLI 以给定参数执行一次根命令（每次都是独立的命令树，避免子命令间状态串扰）。
func runCLI(t *testing.T, args ...string) error {
	t.Helper()
	root := newRootCmd()
	root.SetArgs(args)
	return root.Execute()
}

// writeTempYAML 把 YAML 内容落到临时文件，返回路径。
func writeTempYAML(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "spec.yaml")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("写临时 YAML 失败: %v", err)
	}
	return p
}

// tempDB 返回一个随测试清理的 registry 路径。
func tempDB(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "registry.db")
}

// TestNewRootCmd 校验根命令装配了全部子命令（管理面完整性）。
func TestNewRootCmd(t *testing.T) {
	root := newRootCmd()
	want := []string{"tool", "flow", "app", "hook", "serve", "exec", "ai"}
	got := make(map[string]bool)
	for _, c := range root.Commands() {
		got[c.Name()] = true
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("根命令缺少子命令 %q", w)
		}
	}
}

// TestToolLifecycle 覆盖 tool register/list/show 成功路径。
func TestToolLifecycle(t *testing.T) {
	db := tempDB(t)
	p := writeTempYAML(t, validTool)
	if err := runCLI(t, "tool", "register", p, "--registry", db); err != nil {
		t.Fatalf("tool register: %v", err)
	}
	if err := runCLI(t, "tool", "list", "--registry", db); err != nil {
		t.Fatalf("tool list: %v", err)
	}
	if err := runCLI(t, "tool", "show", "t1", "--registry", db); err != nil {
		t.Fatalf("tool show: %v", err)
	}
}

// TestToolRegisterErrors 覆盖 tool register 的各错误分支（缺文件/空 id/未实现 runtime）。
func TestToolRegisterErrors(t *testing.T) {
	cases := []struct {
		name string
		yaml string
	}{
		{"missing-file", ""}, // 通过路径不存在触发
		{"empty-id", `id: ""
name: x
runtime: process
entry: echo
input_schema: {type: object}
output_schema: {type: object}
versions: [{version: 1.0.0, state: stable}]
`},
		{"unimplemented-runtime", `id: t2
name: x
runtime: wasm
entry: echo
input_schema: {type: object}
output_schema: {type: object}
versions: [{version: 1.0.0, state: stable}]
`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db := tempDB(t)
			var args []string
			if c.name == "missing-file" {
				args = []string{"tool", "register", "nope.yaml", "--registry", db}
			} else {
				args = []string{"tool", "register", writeTempYAML(t, c.yaml), "--registry", db}
			}
			if err := runCLI(t, args...); err == nil {
				t.Fatalf("期望报错，但未报错（case=%s）", c.name)
			}
		})
	}
}

// TestToolShowMissing 覆盖 show 不存在对象时的错误分支。
func TestToolShowMissing(t *testing.T) {
	db := tempDB(t)
	if err := runCLI(t, "tool", "show", "nope", "--registry", db); err == nil {
		t.Fatal("期望报错：Tool 不存在")
	}
}

// TestToolListEmpty 覆盖 list 空库分支（打印「（无 Tool）」）。
func TestToolListEmpty(t *testing.T) {
	db := tempDB(t)
	if err := runCLI(t, "tool", "list", "--registry", db); err != nil {
		t.Fatalf("tool list 空库应成功: %v", err)
	}
}

// TestFlowLifecycle 覆盖 flow register/list 成功路径（需先注册被引用的 Tool）。
func TestFlowLifecycle(t *testing.T) {
	db := tempDB(t)
	if err := runCLI(t, "tool", "register", writeTempYAML(t, validTool), "--registry", db); err != nil {
		t.Fatalf("前置 tool register: %v", err)
	}
	if err := runCLI(t, "flow", "register", writeTempYAML(t, validFlow), "--registry", db); err != nil {
		t.Fatalf("flow register: %v", err)
	}
	if err := runCLI(t, "flow", "list", "--registry", db); err != nil {
		t.Fatalf("flow list: %v", err)
	}
}

// TestFlowRegisterMissingTool 覆盖「引用的 Tool 不存在」校验分支。
func TestFlowRegisterMissingTool(t *testing.T) {
	db := tempDB(t)
	flow := `id: f1
steps:
  - id: s1
    call: missing
`
	if err := runCLI(t, "flow", "register", writeTempYAML(t, flow), "--registry", db); err == nil {
		t.Fatal("期望报错：引用的 Tool 不存在")
	}
}

// TestFlowRegisterBadDep 覆盖「depends_on 指向不存在 step」校验分支。
func TestFlowRegisterBadDep(t *testing.T) {
	db := tempDB(t)
	if err := runCLI(t, "tool", "register", writeTempYAML(t, validTool), "--registry", db); err != nil {
		t.Fatalf("前置 tool register: %v", err)
	}
	flow := `id: f1
steps:
  - id: s1
    call: t1
    depends_on: [ghost]
`
	if err := runCLI(t, "flow", "register", writeTempYAML(t, flow), "--registry", db); err == nil {
		t.Fatal("期望报错：依赖不存在的 step")
	}
}

// TestFlowRunUnknown 覆盖 flow run 未知 Flow 的错误分支（不实际拉起进程）。
func TestFlowRunUnknown(t *testing.T) {
	db := tempDB(t)
	if err := runCLI(t, "flow", "run", "nope", "--registry", db); err == nil {
		t.Fatal("期望报错：Flow 不存在")
	}
}

// TestHookLifecycle 覆盖 hook register/list/show/remove 成功路径与重复 remove 报错。
func TestHookLifecycle(t *testing.T) {
	db := tempDB(t)
	if err := runCLI(t, "hook", "register", writeTempYAML(t, validHook), "--registry", db); err != nil {
		t.Fatalf("hook register: %v", err)
	}
	if err := runCLI(t, "hook", "list", "--registry", db); err != nil {
		t.Fatalf("hook list: %v", err)
	}
	if err := runCLI(t, "hook", "show", "h1", "--registry", db); err != nil {
		t.Fatalf("hook show: %v", err)
	}
	if err := runCLI(t, "hook", "remove", "h1", "--registry", db); err != nil {
		t.Fatalf("hook remove: %v", err)
	}
	if err := runCLI(t, "hook", "remove", "h1", "--registry", db); err == nil {
		t.Fatal("期望报错：重复 remove 不存在的 Hook")
	}
}

// TestHookRegisterInvalid 覆盖 Hook 级校验错误分支（非法 stage / 空 action）。
func TestHookRegisterInvalid(t *testing.T) {
	cases := []struct {
		name string
		yaml string
	}{
		{"bad-stage", `id: h2
target: "*"
stage: not_a_real_stage
action: t1
`},
		{"empty-action", `id: h3
target: "*"
stage: before_step
action: ""
`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db := tempDB(t)
			if err := runCLI(t, "hook", "register", writeTempYAML(t, c.yaml), "--registry", db); err == nil {
				t.Fatalf("期望报错（case=%s）", c.name)
			}
		})
	}
}

// TestAppRunIllegalRestart 覆盖 app run 非法 --restart 的错误分支（不拉起 supervisor）。
func TestAppRunIllegalRestart(t *testing.T) {
	db := tempDB(t)
	if err := runCLI(t, "app", "run", "f1", "--restart", "bogus", "--registry", db); err == nil {
		t.Fatal("期望报错：非法 --restart")
	}
}

// TestServeCmdConstruction 校验 --addr 作为根命令持久标志存在（serve / exec / ai 共用，
// 执行路径会阻塞，不在此跑）。
func TestServeCmdConstruction(t *testing.T) {
	root := newRootCmd()
	fl := root.PersistentFlags().Lookup("addr")
	if fl == nil {
		t.Fatal("根命令缺少 --addr 持久标志")
	}
	if fl.DefValue != "127.0.0.1:8787" {
		t.Errorf("--addr 默认值应为 127.0.0.1:8787，实为 %q", fl.DefValue)
	}
}

// TestToolUnregister 覆盖 tool unregister 成功路径与重复 unregister 报错（§8.2 守卫）。
func TestToolUnregister(t *testing.T) {
	db := tempDB(t)
	if err := runCLI(t, "tool", "register", writeTempYAML(t, validTool), "--registry", db); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := runCLI(t, "tool", "unregister", "t1", "--registry", db); err != nil {
		t.Fatalf("unregister: %v", err)
	}
	if err := runCLI(t, "tool", "unregister", "t1", "--registry", db); err == nil {
		t.Fatal("期望报错：重复 unregister 不存在的 Tool")
	}
}

// TestToolUnregisterInUse 覆盖 §8.2 注销守卫：被 Flow 引用的 Tool 不可注销。
func TestToolUnregisterInUse(t *testing.T) {
	db := tempDB(t)
	if err := runCLI(t, "tool", "register", writeTempYAML(t, validTool), "--registry", db); err != nil {
		t.Fatalf("register tool: %v", err)
	}
	if err := runCLI(t, "flow", "register", writeTempYAML(t, validFlow), "--registry", db); err != nil {
		t.Fatalf("register flow: %v", err)
	}
	if err := runCLI(t, "tool", "unregister", "t1", "--registry", db); err == nil {
		t.Fatal("期望报错：Tool 被 Flow 引用，§8.2 守卫应拒")
	}
}

// TestFlowValidateOK 覆盖 flow validate 成功路径（需被引用 Tool 存在）。
func TestFlowValidateOK(t *testing.T) {
	db := tempDB(t)
	if err := runCLI(t, "tool", "register", writeTempYAML(t, validTool), "--registry", db); err != nil {
		t.Fatalf("register tool: %v", err)
	}
	if err := runCLI(t, "flow", "validate", writeTempYAML(t, validFlow), "--registry", db); err != nil {
		t.Fatalf("flow validate 应通过: %v", err)
	}
}

// TestFlowValidateBad 覆盖 flow validate 引用缺失 Tool 的报错分支。
func TestFlowValidateBad(t *testing.T) {
	db := tempDB(t)
	flow := `id: f1
steps:
  - id: s1
    call: missing
`
	if err := runCLI(t, "flow", "validate", writeTempYAML(t, flow), "--registry", db); err == nil {
		t.Fatal("期望报错：引用的 Tool 不存在")
	}
}

// TestFlowInspect 覆盖 flow inspect 成功路径与不存在报错。
func TestFlowInspect(t *testing.T) {
	db := tempDB(t)
	if err := runCLI(t, "tool", "register", writeTempYAML(t, validTool), "--registry", db); err != nil {
		t.Fatalf("register tool: %v", err)
	}
	if err := runCLI(t, "flow", "register", writeTempYAML(t, validFlow), "--registry", db); err != nil {
		t.Fatalf("register flow: %v", err)
	}
	if err := runCLI(t, "flow", "inspect", "f1", "--registry", db); err != nil {
		t.Fatalf("flow inspect: %v", err)
	}
	if err := runCLI(t, "flow", "inspect", "nope", "--registry", db); err == nil {
		t.Fatal("期望报错：Flow 不存在")
	}
}
