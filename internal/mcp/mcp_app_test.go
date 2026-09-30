package mcp

// mcp_app_test.go：应用生命周期与 bundle「退化/告警」路径的补充用例。
//
// 为什么单独成文件：mcp_test.go 管「主链路 + 工具面红线」（那是一条闭环，必须一眼读得完），
// 这里管两块更偏**路径覆盖**、但同样属于红线的区域：
//
//	1. gear_app 的**真起真停**——不是造一条假台账糊过去，而是真的派生一个进程、真的收掉它。
//	   整个「MCP 能创建并运行应用」的价值就在这四条 action 上，不真跑等于没测。
//	2. bundle 的**退化路径**：无 kind 的分类启发式、未知 kind、App 清单缺字段、
//	   「定义合法但环境未就绪」的告警。这些是智能体最容易撞上的形态（它不会每次都写全 kind）。

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// svcAppBundleYAML 是一个**可被 gear_app 真正启动**的应用：App 清单 + 长驻 Tool + service Flow。
const svcAppBundleYAML = `kind: App
id: svcapp
name: 常驻冒烟
description: 真起真停，验证 gear_app 的 start/status/stop/list 四条路径
entry_flow: svcapp.up
start:
  restart: never
---
kind: Tool
id: svcapp__hold
name: Hold
description: 长驻占位（lua）
runtime: lua
entry: |
  return {}
input_schema:
  type: object
output_schema:
  type: object
versions:
  - version: "1.0.0"
    state: stable
---
kind: Flow
id: svcapp.up
name: 常驻 Flow
description: 含 mode: service 的长驻 step
steps:
  - id: hold
    call: svcapp__hold
    mode: service
    input: {}
`

// TestMCPAppLifecycleEndToEnd 走完 start → status → stop → list 四条路径。
//
// 测试里 gear_app(start) 派生的是**测试二进制自身**（os.Executable），而它认不得
// `--registry … app run …` 这组参数，于是会在解析参数时立刻退出——这恰好覆盖
// 「派生后短等窗口内进程即退出」这条真实分支：必须如实报 `startup_failed` 并附日志，
// **绝不谎报 running**（谎报会让智能体对着一具尸体反复 status）。
func TestMCPAppLifecycleEndToEnd(t *testing.T) {
	f := newSmokeFixture(t)
	callTool(t, f.session, "gear_apply", map[string]any{"bundle": svcAppBundleYAML}, nil)

	// 空台账：list 成功且为空（不是错误）。
	var empty map[string]any
	callTool(t, f.session, "gear_app", map[string]any{"action": "list"}, &empty)
	if fmt.Sprint(empty["total"]) != "0" {
		t.Fatalf("初始台账应为空: %v", empty)
	}

	// ① start：真派生后台进程。
	freePort := pickFreePort(t)
	var started map[string]any
	callTool(t, f.session, "gear_app", map[string]any{
		"action": "start", "flow_id": "svcapp.up", "restart": "never",
		"ports": []any{freePort}, "wait_seconds": 5,
	}, &started)

	appID, _ := started["app_id"].(string)
	if appID == "" {
		t.Fatalf("start 应返回 app_id: %v", started)
	}
	if pid, _ := started["pid"].(float64); pid <= 0 {
		t.Fatalf("start 应返回真实 pid: %v", started)
	}
	if started["state"] != "startup_failed" {
		t.Fatalf("派生进程立刻退出时应如实报 startup_failed（不得谎报 running）: %v", started)
	}
	logPath, _ := started["log_path"].(string)
	if logPath == "" {
		t.Fatalf("start 应返回 log_path（排障入口）: %v", started)
	}
	if _, err := os.Stat(logPath); err != nil {
		t.Fatalf("start 应真的把子进程输出落盘到 %s: %v", logPath, err)
	}
	if _, ok := started["error"]; !ok {
		t.Fatalf("startup_failed 时应给出 error 说明: %v", started)
	}
	// escaped_job 必须**恒在**（未探测时为 null）——它是「这次启动到底能不能跨会话存活」
	// 的唯一凭据。曾经漏报，导致接口无条件承诺「跨会话存活」而智能体无从自检（评审 B-1）。
	// 缺这个键就是回归，故在此硬断言。
	if _, ok := started["escaped_job"]; !ok {
		t.Fatalf("start 必须返回 escaped_job 键（true/false/null 三选一，不得缺省）: %v", started)
	}

	// ② status：如实报 startup_failed（结案原因被保留），而不是笼统的 stopped / running。
	//    这条信息是排障关键——「它压根没起来过」和「它跑了一阵被停了」要分得开。
	var st map[string]any
	callTool(t, f.session, "gear_app", map[string]any{"action": "status", "app_id": appID}, &st)
	if st["state"] != "startup_failed" {
		t.Fatalf("启动失败的记录应报 startup_failed: %v", st)
	}
	if _, ok := st["last_logs"]; !ok {
		t.Fatalf("status 应默认带日志尾部（排障时「在跑吗」与「报什么错」往往同时需要）: %v", st)
	}

	// ③ stop：收干净 → stopped + ports_released（此时 state 变成 stopped——结案原因随之更新）。
	var stopped map[string]any
	callTool(t, f.session, "gear_app", map[string]any{"action": "stop", "app_id": appID}, &stopped)
	if stopped["state"] != "stopped" {
		t.Fatalf("stop 应报 stopped: %v", stopped)
	}
	if stopped["ports_released"] != true {
		t.Fatalf("stop 应确认端口已释放: %v", stopped)
	}

	// ④ list：历史记录仍在（台账是发现机制，不因停止而消失），且不再算遗留项。
	var list map[string]any
	callTool(t, f.session, "gear_app", map[string]any{"action": "list"}, &list)
	items, _ := list["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("list 应含 1 条历史记录: %v", list)
	}
	first, _ := items[0].(map[string]any)
	if first["state"] != "stopped" {
		t.Fatalf("已 stop 的记录应显示 stopped: %v", first)
	}
	if orph, _ := list["orphan"].([]any); len(orph) != 0 {
		t.Fatalf("已显式 stop 的记录不得再算遗留项: %v", orph)
	}

	// ⑤ 已 stop 的实例再 start 应重新拉起（幂等检查只认「活跃」实例）。
	var restarted map[string]any
	callTool(t, f.session, "gear_app", map[string]any{
		"action": "start", "flow_id": "svcapp.up", "restart": "never",
	}, &restarted)
	if restarted["app_id"] == appID {
		t.Fatalf("已停止后重启应产生新的 app_id: %v", restarted)
	}
}

// TestMCPAppStartRefusesWithoutRegistryPath 未配置 registry 路径时**拒绝启动**而不是尽力而为。
//
// 派生进程会继承 `--registry <path>`；没有它，子进程会去读默认的 ~/.gear/registry.db，
// 于是「启动了另一个库里的同名 Flow」——启动错的对象比启动失败难查得多。
func TestMCPAppStartRefusesWithoutRegistryPath(t *testing.T) {
	f := newSmokeFixture(t, WithRegistryPath(""))
	expectToolError(t, f.session, "gear_app",
		map[string]any{"action": "start", "flow_id": "whatever.up"}, "APP_START_FAILED")
}

// TestMCPAppCustomAppsDir 台账目录可被显式指定，且 doctor 报的是**解析后**的目录。
func TestMCPAppCustomAppsDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "myapps")
	f := newSmokeFixture(t, WithAppsDir(dir))

	var list map[string]any
	callTool(t, f.session, "gear_app", map[string]any{"action": "list"}, &list)
	if fmt.Sprint(list["total"]) != "0" {
		t.Fatalf("自定义台账目录应可用且为空: %v", list)
	}

	var doc map[string]any
	callTool(t, f.session, "gear_doctor", nil, &doc)
	if doc["apps_dir"] != dir {
		t.Fatalf("doctor 应报出显式指定的台账目录 %s，实际 %v", dir, doc["apps_dir"])
	}
}

// TestMCPBundleDegeneratePaths 解析器的退化路径：无 kind 的启发式分类、未知 kind、坏 YAML、多份 App。
//
// 这些形态智能体**每天都会写出来**（它不会每次都记得写 kind），所以它们不是边角料：
// 每条都必须给出「能看懂、能照着改」的诊断，而不是一句笼统的失败。
func TestMCPBundleDegeneratePaths(t *testing.T) {
	f := newSmokeFixture(t)

	// 无 kind：有 steps → Flow；有 stage → Hook；否则 → Tool（兼容 examples/*.yaml 的历史写法）。
	const noKind = `id: heur__t
name: 无 kind Tool
description: 靠字段启发式
runtime: lua
entry: |
  return {}
input_schema:
  type: object
output_schema:
  type: object
versions:
  - version: "1.0.0"
    state: stable
---
id: heur.up
name: 无 kind Flow
steps:
  - id: s1
    call: heur__t
---
id: heur__h
target: "*"
stage: before_step
action: heur__t
`
	var out map[string]any
	callTool(t, f.session, "gear_validate", map[string]any{"yaml": noKind}, &out)
	if out["valid"] != true {
		t.Fatalf("无 kind 的历史写法应能按字段分类通过: %v", out)
	}
	summary, _ := out["summary"].(map[string]any)
	if fmt.Sprint(summary["tools"]) != "1" || fmt.Sprint(summary["flows"]) != "1" || fmt.Sprint(summary["hooks"]) != "1" {
		t.Fatalf("三份文档应各归各类: %v", summary)
	}

	// 未知 kind：**明确报错**，不许猜成 Tool（猜错会刷出一堆莫名其妙的「runtime 非法」）。
	var bad map[string]any
	callTool(t, f.session, "gear_validate", map[string]any{"yaml": "kind: Widget\nid: w1\n"}, &bad)
	if bad["valid"] != false {
		t.Fatalf("未知 kind 应判 invalid: %v", bad)
	}
	if !diagsContain(bad, "UNKNOWN_KIND") {
		t.Fatalf("未知 kind 应报 UNKNOWN_KIND: %v", bad["diagnostics"])
	}

	// 坏 YAML：报 BAD_YAML 且指出是第几份文档，而不是整段失败。
	var broken map[string]any
	callTool(t, f.session, "gear_validate", map[string]any{"yaml": "id: ok\n---\n\tbad: :\n"}, &broken)
	if broken["valid"] != false || !diagsContain(broken, "BAD_YAML") {
		t.Fatalf("坏 YAML 应报 BAD_YAML: %v", broken)
	}

	// 多于一份 App 清单：一个 bundle 只描述一个应用。
	const twoApps = `kind: App
id: a1
entry_flow: a1.up
---
kind: App
id: a2
entry_flow: a2.up
`
	var multi map[string]any
	callTool(t, f.session, "gear_validate", map[string]any{"yaml": twoApps}, &multi)
	if multi["valid"] != false || !diagsContain(multi, "BAD_APP_MANIFEST") {
		t.Fatalf("一个 bundle 里多于一份 App 清单应被拒: %v", multi)
	}

	// 空 bundle：明确拒绝（不是静默 no-op）。
	expectToolError(t, f.session, "gear_apply", map[string]any{"bundle": "   "}, "BAD_ARGUMENT")
}

// TestMCPAppManifestDiags App 清单的硬约束：id / entry_flow 缺失、entry_flow 不存在。
func TestMCPAppManifestDiags(t *testing.T) {
	f := newSmokeFixture(t)

	// 缺 id：命名空间无从谈起。
	var noID map[string]any
	callTool(t, f.session, "gear_validate", map[string]any{"yaml": "kind: App\nentry_flow: x.up\n"}, &noID)
	if noID["valid"] != false || !diagsContain(noID, "BAD_APP_MANIFEST") {
		t.Fatalf("App 清单缺 id 应被拒: %v", noID)
	}

	// 缺 entry_flow。
	var noEntry map[string]any
	callTool(t, f.session, "gear_validate", map[string]any{"yaml": "kind: App\nid: e1\n"}, &noEntry)
	if noEntry["valid"] != false || !diagsContain(noEntry, "BAD_APP_MANIFEST") {
		t.Fatalf("App 清单缺 entry_flow 应被拒: %v", noEntry)
	}

	// entry_flow 既不在 bundle 里、也没注册。
	const ghost = `kind: App
id: g1
entry_flow: g1.up
---
kind: Tool
id: g1__t
name: G
description: d
runtime: lua
entry: |
  return {}
input_schema:
  type: object
output_schema:
  type: object
versions:
  - version: "1.0.0"
    state: stable
`
	var miss map[string]any
	callTool(t, f.session, "gear_validate", map[string]any{"yaml": ghost}, &miss)
	if miss["valid"] != false || !diagsContain(miss, "UNKNOWN_CALL_TARGET") {
		t.Fatalf("entry_flow 不存在应报 UNKNOWN_CALL_TARGET: %v", miss)
	}
}

// TestMCPAppManifestWarnings「定义合法但环境未就绪」只给告警，**不影响 valid**。
//
// 把它们混进 valid 会让智能体去改本来正确的 YAML——那是把「环境问题」误诊成「定义问题」，
// 属于最昂贵的一类误导（改完还是起不来，且越改越乱）。
func TestMCPAppManifestWarnings(t *testing.T) {
	f := newSmokeFixture(t)

	const bundle = `kind: App
id: warn
name: 告警冒烟
entry_flow: warn.up
layout:
  files:
    - path: no/such/dir/asset.txt
      required: true
requires:
  secrets:
    - SLACK_TOKEN
  sidecars:
    - ext.mail
---
kind: Tool
id: warn__t
name: W
description: d
runtime: lua
entry: |
  return {}
input_schema:
  type: object
output_schema:
  type: object
versions:
  - version: "1.0.0"
    state: stable
---
kind: Flow
id: warn.up
steps:
  - id: s1
    call: warn__t
`
	var out map[string]any
	callTool(t, f.session, "gear_validate", map[string]any{"yaml": bundle}, &out)
	if out["valid"] != true {
		t.Fatalf("环境未就绪不得影响 valid: %v", out)
	}
	warns, _ := out["warnings"].([]any)
	joined := fmt.Sprint(warns)
	for _, want := range []string{"MISSING_FILE", "MISSING_SECRET", "SIDECAR_NOT_BUILT"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("应报出 %s 告警，实际: %v", want, warns)
		}
	}
}

// TestMCPCatalogAndDescribeOtherKinds catalog / describe 的 hook 与 app 分支。
func TestMCPCatalogAndDescribeOtherKinds(t *testing.T) {
	f := newSmokeFixture(t)

	const withHook = `kind: Tool
id: cat__t
name: T
description: d
runtime: lua
entry: |
  return {}
input_schema:
  type: object
output_schema:
  type: object
versions:
  - version: "1.0.0"
    state: stable
---
kind: Hook
id: cat__h
target: "*"
stage: before_step
action: cat__t
criticality: best-effort
order: 3
`
	callTool(t, f.session, "gear_apply", map[string]any{"bundle": withHook}, nil)

	// catalog：hook 分支（tool/flow/execution/app 由别的用例覆盖，这里是 hook）。
	var env map[string]any
	callTool(t, f.session, "gear_catalog", map[string]any{"kind": "hook"}, &env)
	items, _ := env["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("catalog(kind=hook) 应含 1 项: %v", env)
	}
	hk, _ := items[0].(map[string]any)
	if hk["stage"] != "before_step" || hk["criticality"] != "best-effort" {
		t.Fatalf("catalog 应报出 hook 的关键字段: %v", hk)
	}

	// catalog：limit 截断要如实标注（不能悄悄少给）。
	var limited map[string]any
	callTool(t, f.session, "gear_catalog", map[string]any{"limit": 1}, &limited)
	if limited["truncated"] != true || fmt.Sprint(limited["total"]) != "2" {
		t.Fatalf("limit 生效时应标注 truncated 并给出 total: %v", limited)
	}

	// describe：hook。
	var hdesc map[string]any
	callTool(t, f.session, "gear_describe", map[string]any{"kind": "hook", "id": "cat__h"}, &hdesc)
	hobj, _ := hdesc["object"].(map[string]any)
	if hobj["action"] != "cat__t" {
		t.Fatalf("describe(hook) 应返回完整定义: %v", hdesc)
	}

	// describe：app —— 台账里的每一条都要能按 id 打开。
	recDir := filepath.Join(filepath.Dir(f.regPath), "apps")
	if err := os.MkdirAll(recDir, 0o755); err != nil {
		t.Fatal(err)
	}
	rec := `{"app_id":"app_desc","flow_id":"cat.up","pid":0,"started_at":"2026-01-01T00:00:00Z","ports":[1]}`
	if err := os.WriteFile(filepath.Join(recDir, "app_desc.json"), []byte(rec), 0o644); err != nil {
		t.Fatal(err)
	}
	var adesc map[string]any
	callTool(t, f.session, "gear_describe", map[string]any{"kind": "app", "id": "app_desc"}, &adesc)
	aobj, _ := adesc["object"].(map[string]any)
	if aobj["app_id"] != "app_desc" {
		t.Fatalf("describe(app) 应返回状态快照: %v", adesc)
	}
	// 不存在的 app_id → 明确的 NOT_FOUND。
	expectToolError(t, f.session, "gear_describe", map[string]any{"kind": "app", "id": "app_nope"}, "APP_NOT_FOUND")

	// catalog：app 分支（台账里有一条）。
	callTool(t, f.session, "gear_catalog", map[string]any{"kind": "app"}, &env)
	if fmt.Sprint(env["total"]) != "1" {
		t.Fatalf("catalog(kind=app) 应报出台账条目: %v", env)
	}
}

// TestMCPApplyConflictOnFlowAndHook overwrite 语义对 Flow / Hook 同样生效（不只 Tool）。
//
// 三条路径各自独立，只测 Tool 会漏掉另外两条——而那两条恰恰是「引用关系更复杂、
// 覆盖代价更高」的两类（Flow 被依赖、Hook 挂在别人的执行路径上）。
func TestMCPApplyConflictOnFlowAndHook(t *testing.T) {
	f := newSmokeFixture(t)

	const base = `kind: Tool
id: cf__t
name: T
description: d
runtime: lua
entry: |
  return {}
input_schema:
  type: object
output_schema:
  type: object
versions:
  - version: "1.0.0"
    state: stable
---
kind: Flow
id: cf.up
name: CF
description: 一版
steps:
  - id: s1
    call: cf__t
`
	callTool(t, f.session, "gear_apply", map[string]any{"bundle": base}, nil)

	// Flow 内容变了 → 默认拒绝覆盖。
	changedFlow := replaceFirst(base, "description: 一版", "description: 二版")
	expectToolError(t, f.session, "gear_apply", map[string]any{"bundle": changedFlow}, "APPLY_CONFLICT")

	// Hook 冲突：先落一个 Hook，再用不同 order 覆盖。
	const hookDoc = `kind: Hook
id: cf__h
target: "*"
stage: before_step
action: cf__t
order: 1
`
	callTool(t, f.session, "gear_apply", map[string]any{"bundle": hookDoc}, nil)
	conflictHook := replaceFirst(hookDoc, "order: 1", "order: 9")
	expectToolError(t, f.session, "gear_apply", map[string]any{"bundle": conflictHook}, "APPLY_CONFLICT")

	// 显式 overwrite 才允许：两条都应成功，且 updated 如实列出。
	var out map[string]any
	callTool(t, f.session, "gear_apply", map[string]any{
		"bundle": changedFlow + "---\n" + conflictHook, "overwrite": true,
	}, &out)
	if out["valid"] != true {
		t.Fatalf("overwrite=true 应通过: %v", out)
	}
	if !strings.Contains(fmt.Sprint(out["updated"]), "flow:cf.up") || !strings.Contains(fmt.Sprint(out["updated"]), "hook:cf__h") {
		t.Fatalf("updated 应如实列出被覆盖的 flow 与 hook: %v", out["updated"])
	}

	// 引用守卫：Tool 被 Flow 与 Hook 引用 → 删不掉，且错误里说明被谁引用。
	text := expectToolError(t, f.session, "gear_delete", map[string]any{"kind": "tool", "id": "cf__t"}, "IN_USE")
	if !strings.Contains(text, "cf.up") {
		t.Fatalf("IN_USE 的错误应指出被谁引用: %s", text)
	}
}

// TestMCPRunToolSyncAndExecutionList gear_run(kind=tool) 与 gear_execution 的列表形态。
func TestMCPRunToolSyncAndExecutionList(t *testing.T) {
	f := newSmokeFixture(t)
	callTool(t, f.session, "gear_apply", map[string]any{"bundle": smokeBundleYAML}, nil)

	// 不带 id → 列出最近执行（初始为空数组，不是 null）。
	var none map[string]any
	callTool(t, f.session, "gear_execution", nil, &none)
	if ex, ok := none["executions"].([]any); !ok || len(ex) != 0 {
		t.Fatalf("无执行时应返回空数组: %v", none)
	}

	// kind=tool：同步返回输出（异步反而多一次往返——Tool 是原子动作）。
	var toolRun map[string]any
	callTool(t, f.session, "gear_run", map[string]any{
		"kind": "tool", "id": "smoke__hello", "input": map[string]any{"name": "sync"},
	}, &toolRun)
	out, _ := toolRun["output"].(map[string]any)
	if fmt.Sprint(out["greeting"]) != "hi sync" {
		t.Fatalf("run(tool) 应同步返回输出: %v", toolRun)
	}

	// kind=flow 后，列表应含该执行且带 finished_at。
	var run map[string]any
	callTool(t, f.session, "gear_run", map[string]any{
		"kind": "flow", "id": "smoke.up", "wait_seconds": 15,
	}, &run)
	execID, _ := run["execution_id"].(string)
	if execID == "" {
		t.Fatalf("gear_run(flow) 应返回 execution_id: %v", run)
	}
	var list map[string]any
	callTool(t, f.session, "gear_execution", nil, &list)
	ex, _ := list["executions"].([]any)
	if len(ex) != 1 {
		t.Fatalf("执行列表应含 1 条: %v", list)
	}
	one, _ := ex[0].(map[string]any)
	if one["id"] != execID || one["finished_at"] == nil {
		t.Fatalf("列表条目应含 id 与 finished_at: %v", one)
	}

	// tail 只取末 N 行。
	var tailed map[string]any
	callTool(t, f.session, "gear_execution", map[string]any{"id": execID, "with_logs": true, "tail": 1}, &tailed)
	logs, _ := tailed["logs"].([]any)
	if len(logs) > 1 {
		t.Fatalf("tail=1 应只返回末 1 行: %v", logs)
	}
}

// ---- 小工具 ----

// pickFreePort 取一个当前空闲的端口（用于台账端口探活）。
func pickFreePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("取空闲端口失败: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// diagsContain 报告诊断列表里是否出现某错误码。
func diagsContain(out map[string]any, code string) bool {
	return strings.Contains(fmt.Sprint(out["diagnostics"]), code)
}

// replaceFirst 替换首个出现的 old（用来构造「内容不同」的第二版定义）。
func replaceFirst(s, old, new string) string {
	return strings.Replace(s, old, new, 1)
}

// TestMCPDescribeAppReturnsManifestAndComposition 「这个应用由什么组成」必须一次答全。
//
// 2026-09-25 的缺口原文：「没有任何端点能回答一个 App 由哪些 Tool/Flow/静态资源/数据表组成」。
// 这条是它的回归锁——只给运行态（pid/端口）等于没答，智能体仍然要猜。
// 组成判定与 `gear_export` 共用 `artifact.Composition`（此处锁的是「同一份判定」这件事）。
func TestMCPDescribeAppReturnsManifestAndComposition(t *testing.T) {
	f := newSmokeFixture(t)
	callTool(t, f.session, "gear_apply", map[string]any{"bundle": smokeBundleYAML}, nil)

	var out map[string]any
	callTool(t, f.session, "gear_describe", map[string]any{"kind": "app", "id": "smoke"}, &out)
	if out["kind"] != "app" {
		t.Fatalf("kind 不符: %v", out)
	}
	manifest, _ := out["manifest"].(map[string]any)
	if manifest == nil {
		t.Fatalf("manifest 不应为 null（已 apply 过 App 清单）: %v", out)
	}
	if manifest["entry_flow"] != "smoke.up" {
		t.Fatalf("manifest 缺 entry_flow: %v", manifest)
	}
	comp, _ := out["composition"].(map[string]any)
	if comp == nil {
		t.Fatalf("composition 不应为 null: %v", out)
	}
	if comp["entry_flow"] != "smoke.up" {
		t.Fatalf("composition 缺 entry_flow: %v", comp)
	}
	tools, _ := comp["tools"].([]any)
	if len(tools) != 1 || tools[0] != "smoke__hello" {
		t.Fatalf("composition.tools 应为名下 Tool（按 namespace 前缀反查）: %v", comp["tools"])
	}
	flows, _ := comp["flows"].([]any)
	if len(flows) != 1 || flows[0] != "smoke.up" {
		t.Fatalf("composition.flows 应含 entry_flow: %v", comp["flows"])
	}
	// 静态资源是「清单里声明的路径」，不是「磁盘上有哪些」——后者归 gear_doctor。
	if _, ok := comp["static_files"]; !ok {
		t.Fatalf("composition 缺 static_files: %v", comp)
	}
}

// TestMCPDescribeAppReportsNotFoundOnlyWhenBothMissing 三份信息互不包含，缺的那份如实为 null。
//
// 「只起过没清单」与「有清单没起过」都是合法状态：**任一份存在就不报 NOT_FOUND**。
// 反过来，两者皆无才是真的没有——这条同时是「不要因为查不到台账就把应用判成不存在」的守卫。
func TestMCPDescribeAppReportsNotFoundOnlyWhenBothMissing(t *testing.T) {
	f := newSmokeFixture(t)

	// ① 只有台账（手工塞一条），没有清单 → object 可用、manifest 为 null、不报 NOT_FOUND。
	appsDir := filepath.Join(filepath.Dir(f.regPath), "apps")
	if err := os.MkdirAll(appsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(appsDir, "app_fake.json"),
		[]byte(`{"app_id":"app_fake","flow_id":"smoke.up","pid":0,"started_at":"2026-01-01T00:00:00Z"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	var onlyLedger map[string]any
	callTool(t, f.session, "gear_describe", map[string]any{"kind": "app", "id": "app_fake"}, &onlyLedger)
	if onlyLedger["object"] == nil {
		t.Fatalf("仅台账存在时 object 应可用: %v", onlyLedger)
	}
	if onlyLedger["manifest"] != nil {
		t.Fatalf("没有清单时 manifest 应为 null（如实，不编）: %v", onlyLedger)
	}

	// ② 只有清单（apply 一份 App），没起过 → manifest/composition 可用、object 为 null。
	callTool(t, f.session, "gear_apply", map[string]any{"bundle": smokeBundleYAML}, nil)
	var onlyManifest map[string]any
	callTool(t, f.session, "gear_describe", map[string]any{"kind": "app", "id": "smoke"}, &onlyManifest)
	if onlyManifest["manifest"] == nil || onlyManifest["composition"] == nil {
		t.Fatalf("有清单时 manifest/composition 应可用: %v", onlyManifest)
	}

	// ③ 两者皆无 → APP_NOT_FOUND（不是空结果，也不是 200 + 全 null）。
	text := expectToolError(t, f.session, "gear_describe", map[string]any{"kind": "app", "id": "nope"}, "APP_NOT_FOUND")
	if !strings.Contains(text, "APP_NOT_FOUND") {
		t.Fatalf("两者皆无时应报 APP_NOT_FOUND: %s", text)
	}
}
