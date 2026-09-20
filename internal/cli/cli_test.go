package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gear/internal/model"
	"gear/internal/registry"
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
	want := []string{"tool", "flow", "app", "hook", "serve", "exec"}
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

// TestRegisterSubcommandMismatch 锁定「Tool/Flow 注册子命令用错」的守卫：
// 应在打开 registry 之前就报出可操作的友好错误，且**不创建/污染** registry 文件
// （否则一次误用就会把 gear 的 tools/flows/hooks 表建进用户指定的库里）。
func TestRegisterSubcommandMismatch(t *testing.T) {
	toolYAML := `id: t1
name: x
runtime: lua
entry: return {}
input_schema: {type: object}
output_schema: {type: object}
versions: [{version: 1.0.0, state: stable}]
`
	flowYAML := `id: f1
name: x
steps:
  - id: s1
    call: t1
    input: {}
`
	t.Run("tool-yaml-to-flow-register", func(t *testing.T) {
		db := tempDB(t)
		err := runCLI(t, "flow", "register", writeTempYAML(t, toolYAML), "--registry", db)
		if err == nil || !strings.Contains(err.Error(), "tool register") {
			t.Fatalf("期望提示改用 tool register，实际: %v", err)
		}
		if _, statErr := os.Stat(db); statErr == nil {
			t.Fatalf("守卫应发生在打开 registry 之前，但 %s 被创建了", db)
		}
	})
	t.Run("flow-yaml-to-tool-register", func(t *testing.T) {
		db := tempDB(t)
		err := runCLI(t, "tool", "register", writeTempYAML(t, flowYAML), "--registry", db)
		if err == nil || !strings.Contains(err.Error(), "flow register") {
			t.Fatalf("期望提示改用 flow register，实际: %v", err)
		}
		if _, statErr := os.Stat(db); statErr == nil {
			t.Fatalf("守卫应发生在打开 registry 之前，但 %s 被创建了", db)
		}
	})
}

// withStdin 用给定内容替换 os.Stdin（表现为「管道/重定向」），测试结束恢复。
// 用于验证「stdin 管道」这条输入路径；写入后立即关闭写端，读端内容即可被读到。
func withStdin(t *testing.T, content string) {
	t.Helper()
	old := os.Stdin
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("建管道失败: %v", err)
	}
	if _, err := w.WriteString(content); err != nil {
		t.Fatalf("写管道失败: %v", err)
	}
	_ = w.Close()
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = old; _ = r.Close() })
}

// TestResolveInput 覆盖统一输入的四种来源（--arg / --input / --input-file / **stdin 管道**）
// 与互斥、非法 JSON、非法 k=v、空管道各分支。
func TestResolveInput(t *testing.T) {
	spec := func(json, file string, kv ...string) inputSpec {
		return inputSpec{jsonFlag: "--input", fileFlag: "--input-file", kvFlag: "--arg",
			jsonVal: json, fileVal: file, kv: kv}
	}

	// ① --arg：值按 JSON 解析（字符串/数字/布尔各一）
	m, err := resolveInput(spec("", "", "city=上海", "n=3", "flag=true"))
	if err != nil {
		t.Fatalf("--arg 解析失败: %v", err)
	}
	if m["city"] != "上海" {
		t.Fatalf("city 应为字符串，实际 %#v", m["city"])
	}
	if m["n"] != float64(3) {
		t.Fatalf("n 应按 JSON 解析为数字，实际 %#v", m["n"])
	}
	if m["flag"] != true {
		t.Fatalf("flag 应按 JSON 解析为布尔，实际 %#v", m["flag"])
	}

	// ② --input：整体 JSON 对象
	if m, err = resolveInput(spec(`{"a":1}`, "")); err != nil || m["a"] != float64(1) {
		t.Fatalf("--input 解析异常: %v %#v", err, m)
	}

	// ③ --input-file
	p := filepath.Join(t.TempDir(), "in.json")
	if err := os.WriteFile(p, []byte(`{"b":"x"}`), 0o644); err != nil {
		t.Fatalf("写输入文件失败: %v", err)
	}
	if m, err = resolveInput(spec("", p)); err != nil || m["b"] != "x" {
		t.Fatalf("--input-file 解析异常: %v %#v", err, m)
	}

	// ④ `--input -`：显式从 stdin 读
	withStdin(t, `{"from":"stdin"}`)
	if m, err = resolveInput(spec("-", "")); err != nil || m["from"] != "stdin" {
		t.Fatalf("--input - 解析异常: %v %#v", err, m)
	}

	// ⑤ 无标志 + stdin 是管道 → **自动**读（`cat x.json | gear ...`）
	withStdin(t, `{"auto":true}`)
	if m, err = resolveInput(spec("", "")); err != nil || m["auto"] != true {
		t.Fatalf("stdin 自动读异常: %v %#v", err, m)
	}

	// ⑥ 无标志 + 空管道 → 空 map（不报错）
	withStdin(t, "")
	if m, err = resolveInput(spec("", "")); err != nil || len(m) != 0 {
		t.Fatalf("空管道应返回空 map: %v %#v", err, m)
	}

	// ⑦ 混用显式标志 → 报错（避免「以谁为准」的歧义）
	if _, err = resolveInput(spec(`{}`, "", "a=1")); err == nil {
		t.Fatal("混用 --input 与 --arg 应报错")
	}
	// ⑧ `--input -` 与 --arg 混用 → 报错
	if _, err = resolveInput(spec("-", "", "a=1")); err == nil {
		t.Fatal("--input - 与 --arg 混用应报错")
	}
	// ⑨ 非法 JSON → 报错
	if _, err = resolveInput(spec("not-json", "")); err == nil {
		t.Fatal("非法 JSON 应报错")
	}
	// ⑩ 非法 k=v → 报错（不静默）
	if _, err = resolveInput(spec("", "", "novalue")); err == nil {
		t.Fatal("缺 = 的 --arg 应报错")
	}
}

// TestReadFileOrStdin 覆盖「路径写 - 从 stdin 读」这条注册路径。
func TestReadFileOrStdin(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.yaml")
	if err := os.WriteFile(p, []byte("id: from-file\n"), 0o644); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}
	b, err := readFileOrStdin(p)
	if err != nil || string(b) != "id: from-file\n" {
		t.Fatalf("读文件异常: %v %q", err, b)
	}

	withStdin(t, "id: from-stdin\n")
	b, err = readFileOrStdin("-")
	if err != nil || string(b) != "id: from-stdin\n" {
		t.Fatalf("读 stdin 异常: %v %q", err, b)
	}
}

// TestRegisterFromStdin 端到端：`gear tool register -` 从管道读 YAML 并注册成功。
func TestRegisterFromStdin(t *testing.T) {
	db := tempDB(t)
	withStdin(t, validTool)
	if err := runCLI(t, "tool", "register", "-", "--registry", db); err != nil {
		t.Fatalf("从 stdin 注册失败: %v", err)
	}
	if err := runCLI(t, "tool", "show", "t1", "--registry", db); err != nil {
		t.Fatalf("注册后应能查到: %v", err)
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

// ---- 多文档注册回归（BUG-3）----
//
// 背景：`tool register` / `flow register` 此前用 yaml.Unmarshal 直接解单文档，
// 多文档文件**只注册第一篇、其余静默丢弃**（exit 0 且打印成功）。examples/news-top10.yaml
// 这种「前导注释块 + `---` + Tool + `---` + Flow」的文件交给 tool register 甚至会
// 「注册 1 个 Tool 却把 Flow 悄悄丢掉」。属静默降级（GEAR 红线），故加护栏。

// countRegistered 读注册表统计某类对象的数量（通过 list 命令的输出行数近似判断）。
// 这里直接用 registry 打开库读，避免依赖输出格式。
func assertToolExists(t *testing.T, db, id string, want bool) {
	t.Helper()
	reg, err := registry.Open(db)
	if err != nil {
		t.Fatalf("打开 registry: %v", err)
	}
	defer reg.Close()
	tool, err := reg.GetTool(id)
	if err != nil {
		t.Fatalf("GetTool(%s): %v", id, err)
	}
	if want && tool == nil {
		t.Errorf("期望 Tool %s 已注册，但不存在", id)
	}
	if !want && tool != nil {
		t.Errorf("期望 Tool %s 未注册，但存在", id)
	}
}

func assertFlowExists(t *testing.T, db, id string, want bool) {
	t.Helper()
	reg, err := registry.Open(db)
	if err != nil {
		t.Fatalf("打开 registry: %v", err)
	}
	defer reg.Close()
	f, err := reg.GetFlow(id)
	if err != nil {
		t.Fatalf("GetFlow(%s): %v", id, err)
	}
	if want && f == nil {
		t.Errorf("期望 Flow %s 已注册，但不存在", id)
	}
	if !want && f != nil {
		t.Errorf("期望 Flow %s 未注册，但存在", id)
	}
}

// TestToolRegisterMultiDoc 锁定 tool register 解析**全部**文档。
// 关键形态：首个文档是前导注释块（复刻 examples/news-top10.yaml）。
func TestToolRegisterMultiDoc(t *testing.T) {
	multi := `# 前导注释块：不是任何对象的文档
---
id: t1
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
---
id: t2
name: demo tool 2
description: 第二个 Tool
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
	db := tempDB(t)
	if err := runCLI(t, "tool", "register", writeTempYAML(t, multi), "--registry", db); err != nil {
		t.Fatalf("tool register 多文档: %v", err)
	}
	assertToolExists(t, db, "t1", true)
	assertToolExists(t, db, "t2", true) // 修复前：被静默丢弃
}

// TestToolRegisterMultiDocRejectsFlowDoc 锁定：多文档里混入 Flow 要报错并给出可操作指引，
// 而不是「注册掉 Tool 部分、把 Flow 部分丢掉」。
func TestToolRegisterMultiDocRejectsFlowDoc(t *testing.T) {
	multi := `id: t1
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
---
id: f1
name: demo flow
description: 混进来的 Flow
steps:
  - id: s1
    call: t1
`
	db := tempDB(t)
	err := runCLI(t, "tool", "register", writeTempYAML(t, multi), "--registry", db)
	if err == nil {
		t.Fatal("期望报错：Tool 与 Flow 混放的文件不能走 tool register")
	}
	// 错误信息要说明「同时包含两类」并给出出路（拆分注册 或 --seed 一次灌入）。
	if !strings.Contains(err.Error(), "同时包含 Tool 与 Flow") {
		t.Errorf("错误信息应指明文件混放了两类定义，实际: %v", err)
	}
	if !strings.Contains(err.Error(), "serve --seed") {
		t.Errorf("错误信息应给出 --seed 出路，实际: %v", err)
	}
	// 守卫在打开 registry 之前生效：不应有任何部分写入。
	assertToolExists(t, db, "t1", false)
}

// TestToolRegisterMultiDocAtomic 锁定两阶段写入：多文档里有一个非法时整体不写。
func TestToolRegisterMultiDocAtomic(t *testing.T) {
	multi := `id: good_one
name: good
description: 合法
runtime: process
entry: echo
input_schema:
  type: object
output_schema:
  type: object
versions:
  - version: 1.0.0
    state: stable
---
id: bad_one
name: bad
description: runtime=wasm 未实现，注册期应被拒
runtime: wasm
entry: echo
input_schema:
  type: object
output_schema:
  type: object
versions:
  - version: 1.0.0
    state: stable
`
	db := tempDB(t)
	err := runCLI(t, "tool", "register", writeTempYAML(t, multi), "--registry", db)
	if err == nil {
		t.Fatal("期望报错：第二个 Tool 的 runtime 未实现")
	}
	if !strings.Contains(err.Error(), "未写入任何") {
		t.Errorf("错误信息应说明本次未写入任何 Tool，实际: %v", err)
	}
	// 关键：第一个合法 Tool 不能被部分写入。
	assertToolExists(t, db, "good_one", false)
	assertToolExists(t, db, "bad_one", false)
}

// TestFlowRegisterMultiDoc 锁定 flow register 解析全部文档（同样含前导注释块）。
func TestFlowRegisterMultiDoc(t *testing.T) {
	db := tempDB(t)
	if err := runCLI(t, "tool", "register", writeTempYAML(t, validTool), "--registry", db); err != nil {
		t.Fatalf("register tool: %v", err)
	}
	multi := `# 前导注释块
---
id: f1
name: flow one
description: 第一个 Flow
steps:
  - id: s1
    call: t1
---
id: f2
name: flow two
description: 第二个 Flow（修复前被静默丢弃）
steps:
  - id: s1
    call: t1
`
	if err := runCLI(t, "flow", "register", writeTempYAML(t, multi), "--registry", db); err != nil {
		t.Fatalf("flow register 多文档: %v", err)
	}
	assertFlowExists(t, db, "f1", true)
	assertFlowExists(t, db, "f2", true)
}

// TestFlowRegisterMultiDocAtomic 锁定 Flow 侧的两阶段写入。
func TestFlowRegisterMultiDocAtomic(t *testing.T) {
	db := tempDB(t)
	if err := runCLI(t, "tool", "register", writeTempYAML(t, validTool), "--registry", db); err != nil {
		t.Fatalf("register tool: %v", err)
	}
	multi := `id: f_good
name: good flow
description: 合法
steps:
  - id: s1
    call: t1
---
id: f_bad
name: bad flow
description: call 目标不存在，静态校验应拒
steps:
  - id: s1
    call: no_such_tool_at_all
`
	err := runCLI(t, "flow", "register", writeTempYAML(t, multi), "--registry", db)
	if err == nil {
		t.Fatal("期望报错：第二个 Flow 引用了不存在的 Tool")
	}
	if !strings.Contains(err.Error(), "未写入任何") {
		t.Errorf("错误信息应说明本次未写入任何 Flow，实际: %v", err)
	}
	assertFlowExists(t, db, "f_good", false)
	assertFlowExists(t, db, "f_bad", false)
}

// TestRegisterFlowOnlyFileViaFlowRegister 锁定单文档 Flow 仍可正常注册
// （多文档改造不能把单文档这条常见路径弄坏）。
func TestRegisterFlowOnlyFileViaFlowRegister(t *testing.T) {
	db := tempDB(t)
	if err := runCLI(t, "tool", "register", writeTempYAML(t, validTool), "--registry", db); err != nil {
		t.Fatalf("register tool: %v", err)
	}
	single := `id: f_single
name: single flow
description: 无前导 --- 的单文档
steps:
  - id: s1
    call: t1
`
	if err := runCLI(t, "flow", "register", writeTempYAML(t, single), "--registry", db); err != nil {
		t.Fatalf("flow register 单文档: %v", err)
	}
	assertFlowExists(t, db, "f_single", true)
}

// TestToolRegisterEmptyDocErrors 锁定「解析不出任何 Tool」要报错（静默成功是红线）。
func TestToolRegisterEmptyDocErrors(t *testing.T) {
	db := tempDB(t)
	err := runCLI(t, "tool", "register", writeTempYAML(t, "# 只有注释\n"), "--registry", db)
	if err == nil || !strings.Contains(err.Error(), "解析出任何 Tool") {
		t.Fatalf("期望报错「没有解析出任何 Tool」，实际: %v", err)
	}
}

// TestFlowRegisterEmptyDocErrors 同上，Flow 侧。
func TestFlowRegisterEmptyDocErrors(t *testing.T) {
	db := tempDB(t)
	err := runCLI(t, "flow", "register", writeTempYAML(t, "# 只有注释\n"), "--registry", db)
	if err == nil || !strings.Contains(err.Error(), "解析出任何 Flow") {
		t.Fatalf("期望报错「没有解析出任何 Flow」，实际: %v", err)
	}
}

// ---- seedExamples 原子性 / 回滚回归 ----
//
// 背景：seedExamples 原先是「边校验边写」的循环，示例文件里第 N 个对象失败会留下
// 半套灌入的注册表，serve 带着它起来更难排查。修复后：Tool 先全量校验、落库，
// Flow 再校验（因为 Flow 的 call 目标可能就是同文件里的 Tool）、落库；
// 任一步失败按已写清单回滚到调用前状态。

// seedYAMLFor 写一个 seed 用 YAML 到临时文件。
func seedYAMLFor(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "seed.yaml")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("写 seed YAML 失败: %v", err)
	}
	return p
}

// openReg 打开 registry 供断言使用。
func openReg(t *testing.T, db string) *registry.Registry {
	t.Helper()
	reg, err := registry.Open(db)
	if err != nil {
		t.Fatalf("打开 registry: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	return reg
}

// TestToolRegisterPureFlowHint 锁定最常见的误用：纯 Flow 文件交给 tool register，
// 应提示改用 `gear flow register`（与混放文件的提示区分开）。
func TestToolRegisterPureFlowHint(t *testing.T) {
	pureFlow := `id: f_only
name: only flow
description: 纯 Flow 文件
steps:
  - id: s1
    call: whatever
`
	db := tempDB(t)
	err := runCLI(t, "tool", "register", writeTempYAML(t, pureFlow), "--registry", db)
	if err == nil || !strings.Contains(err.Error(), "gear flow register") {
		t.Fatalf("期望提示改用 gear flow register，实际: %v", err)
	}
	if _, statErr := os.Stat(db); statErr == nil {
		t.Errorf("守卫应发生在打开 registry 之前，但 %s 被创建了", db)
	}
}

// TestFlowRegisterPureToolHint 反向：纯 Tool 文件交给 flow register。
func TestFlowRegisterPureToolHint(t *testing.T) {
	db := tempDB(t)
	err := runCLI(t, "flow", "register", writeTempYAML(t, validTool), "--registry", db)
	if err == nil || !strings.Contains(err.Error(), "gear tool register") {
		t.Fatalf("期望提示改用 gear tool register，实际: %v", err)
	}
	if _, statErr := os.Stat(db); statErr == nil {
		t.Errorf("守卫应发生在打开 registry 之前，但 %s 被创建了", db)
	}
}

// TestFlowRegisterMixedFileRejected 锁定 Flow 侧对混放文件的拒绝与出路提示。
func TestFlowRegisterMixedFileRejected(t *testing.T) {
	mixed := `id: t_mix
name: a tool
description: Tool 部分
runtime: process
entry: echo
input_schema:
  type: object
output_schema:
  type: object
versions:
  - version: 1.0.0
    state: stable
---
id: f_mix
name: a flow
description: Flow 部分
steps:
  - id: s1
    call: t_mix
`
	db := tempDB(t)
	err := runCLI(t, "flow", "register", writeTempYAML(t, mixed), "--registry", db)
	if err == nil {
		t.Fatal("期望报错：混放文件不能走 flow register")
	}
	if !strings.Contains(err.Error(), "同时包含 Tool 与 Flow") ||
		!strings.Contains(err.Error(), "serve --seed") {
		t.Errorf("错误信息应指明混放并给出 --seed 出路，实际: %v", err)
	}
	assertFlowExists(t, db, "f_mix", false)
	assertToolExists(t, db, "t_mix", false)
}

// TestSeedExamplesToolFlowSameFile 锁定「同文件里 Flow 调用该文件的 Tool」这条
// 最常见的示例形态（examples/news-top10.yaml 即如此）。回归背景：若 Flow 在校验时
// Tool 尚未入库，会被误判「call 目标不存在」而整份 seed 失败。
func TestSeedExamplesToolFlowSameFile(t *testing.T) {
	db := tempDB(t)
	// 先建库
	reg := openReg(t, db)
	defer reg.Close()

	seed := `id: seeded_tool
name: Seeded
description: seed 的 Tool
runtime: lua
entry: |
  return {}
input_schema: {type: object}
output_schema: {type: object}
versions: [{version: "1.0.0", state: stable}]
---
id: seeded_flow
name: Seeded Flow
description: 调用同文件的 Tool
steps:
  - id: s1
    call: seeded_tool
`
	p := seedYAMLFor(t, seed)
	if err := seedExamples(reg, p); err != nil {
		t.Fatalf("seed 同文件 Tool+Flow 应成功，实际: %v", err)
	}
	assertToolExists(t, db, "seeded_tool", true)
	assertFlowExists(t, db, "seeded_flow", true)
}

// TestSeedExamplesRollbackOnBadFlow 锁定回滚：Flow 校验失败时，同批已写入的 Tool
// 必须被撤销，且**不能**误删调用前就存在的对象。
func TestSeedExamplesRollbackOnBadFlow(t *testing.T) {
	db := tempDB(t)
	reg := openReg(t, db)
	defer reg.Close()

	// 预置一个既有 Tool（回滚不应碰到它）。
	preset := `id: pre_existing
name: Pre
description: 调用前就存在
runtime: lua
entry: |
  return {}
input_schema: {type: object}
output_schema: {type: object}
versions: [{version: "1.0.0", state: stable}]
`
	if err := reg.UpsertTool(mustParseTool(t, preset)); err != nil {
		t.Fatalf("预置 Tool: %v", err)
	}

	// seed：一个新 Tool（会被写入）+ 一个引用不存在目标的 Flow（必然失败）。
	seed := `id: new_seeded_tool
name: New
description: 会被写入然后回滚
runtime: lua
entry: |
  return {}
input_schema: {type: object}
output_schema: {type: object}
versions: [{version: "1.0.0", state: stable}]
---
id: bad_seeded_flow
name: Bad
description: 引用不存在的 Tool
steps:
  - id: s1
    call: does_not_exist_at_all
`
	p := seedYAMLFor(t, seed)
	if err := seedExamples(reg, p); err == nil {
		t.Fatal("期望 seed 失败：Flow 引用了不存在的 Tool")
	}
	// 回滚生效：新 Tool 不应残留。
	assertToolExists(t, db, "new_seeded_tool", false)
	assertFlowExists(t, db, "bad_seeded_flow", false)
	// 既有对象必须存活（回滚只撤销本次写入）。
	assertToolExists(t, db, "pre_existing", true)
}

// TestSeedExamplesRollbackRestoresOverwritten 锁定回滚恢复「被同 id 覆盖」的原值，
// 而不是简单地把它删掉。
func TestSeedExamplesRollbackRestoresOverwritten(t *testing.T) {
	db := tempDB(t)
	reg := openReg(t, db)
	defer reg.Close()

	orig := mustParseTool(t, `id: shared
name: ORIGINAL
description: 原始
runtime: lua
entry: |
  return { v = 1 }
input_schema: {type: object}
output_schema: {type: object}
versions: [{version: "1.0.0", state: stable}]
`)
	if err := reg.UpsertTool(orig); err != nil {
		t.Fatalf("预置 Tool: %v", err)
	}

	seed := `id: shared
name: OVERWRITTEN
description: 覆盖版
runtime: lua
entry: |
  return { v = 2 }
input_schema: {type: object}
output_schema: {type: object}
versions: [{version: "2.0.0", state: stable}]
---
id: bad_flow_here
name: Bad
description: 引用不存在
steps:
  - id: s1
    call: nope_nope_nope
`
	p := seedYAMLFor(t, seed)
	if err := seedExamples(reg, p); err == nil {
		t.Fatal("期望 seed 失败")
	}
	got, err := reg.GetTool("shared")
	if err != nil {
		t.Fatalf("GetTool: %v", err)
	}
	if got == nil {
		t.Fatal("回滚应恢复原值，而不是删除 shared")
	}
	if got.Name != "ORIGINAL" {
		t.Errorf("回滚后 name = %q，期望 ORIGINAL（应还原为调用前的版本）", got.Name)
	}
	if len(got.Versions) != 1 || got.Versions[0].Version != "1.0.0" {
		t.Errorf("回滚后版本 = %+v，期望还原为 1.0.0", got.Versions)
	}
}

// mustParseTool 用 ParseYAMLDocs 解出单个 Tool（测试辅助）。
func mustParseTool(t *testing.T, yamlText string) *model.Tool {
	t.Helper()
	tools, flows, err := registry.ParseYAMLDocs([]byte(yamlText))
	if err != nil {
		t.Fatalf("解析 Tool YAML: %v", err)
	}
	if len(tools) != 1 || len(flows) != 0 {
		t.Fatalf("期望 1 Tool 0 Flow，实得 %d/%d", len(tools), len(flows))
	}
	return tools[0]
}

// TestCLIEngineSeedsSecretFromEnv 锁定 cliEngine（gear mcp / gear exec 等 CLI 路径使用的引擎）
// 在构造期从 GEAR_SECRET_<NAME> 环境变量播种密钥源并注入——这是修复「MCP 路径下
// ${secrets.*} 报 secret 原语未提供」的关键（修复前 cliEngine 没调 engine.WithSecret）。
//
// 与 server/secrets_test.go 的 TestSecretsSeedFromEnv 同构，区别仅在于引擎来源换成 cliEngine：
// 证明同一份 SeedFromEnv + WithSecret 机制在 CLI/MCP 路径同样生效，不再只在 REST 服务生效。
func TestCLIEngineSeedsSecretFromEnv(t *testing.T) {
	t.Setenv("GEAR_SECRET_CLI_FOO", "cli-bar")

	reg := openReg(t, tempDB(t))
	if err := reg.UpsertTool(&model.Tool{
		ID:           "cli-env-reader",
		Name:         "E",
		Runtime:      model.RuntimeLua,
		Capabilities: []string{"secret"},
		Entry:        `return { v = ctx.secret.get("CLI_FOO") }`,
		InputSchema:  map[string]interface{}{"type": "object"},
		OutputSchema: map[string]interface{}{"type": "object"},
		Versions:     []model.ToolVersion{{Version: "1.0.0", State: model.StateStable}},
	}); err != nil {
		t.Fatalf("注册 Tool 失败: %v", err)
	}
	if err := reg.UpsertFlow(&model.Flow{ID: "read-cli", Steps: []model.Step{
		{ID: "s1", Call: "cli-env-reader", Input: map[string]interface{}{}},
	}}); err != nil {
		t.Fatalf("注册 Flow 失败: %v", err)
	}

	eng, err := cliEngine(reg)
	if err != nil {
		t.Fatalf("cliEngine 构造失败: %v", err)
	}
	res, err := eng.RunFlowResult("read-cli")
	if err != nil {
		t.Fatalf("运行 Flow 失败: %v", err)
	}
	s1, ok := res.Steps["s1"]
	if !ok {
		t.Fatalf("缺少 step s1 结果: %+v", res)
	}
	out, _ := s1["output"].(map[string]interface{})
	if out == nil || out["v"] != "cli-bar" {
		t.Fatalf("应经 cliEngine 注入的密钥源读到 cli-bar，实际 %+v", s1)
	}
}
