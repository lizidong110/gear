package cli

import (
	"strings"
	"testing"
)

// ② 的 CLI 面：`--scope` 必须真的贯通到引擎（否则 flag 收下了、守卫没装上，
// 表现为「命令没报错也没有任何限制」，是最难发现的一类「配了等于没配」）。
//
// cliScope 是包级变量，会跨用例残留，故每个用例都带清理。

// toolYAMLForScope 把仓库里那份**已验证可注册**的 luaTool 换成指定 id。
//
// 刻意不自己拼 YAML：手拼的 schema/versions 一旦与 registry 的校验口径漂移，
// 报错会伪装成「守卫没生效」——那正是这条测试想排除的失败。
func toolYAMLForScope(id string) string {
	return strings.NewReplacer(
		"id: lua.echo", "id: "+id,
		"name: lua echo", "name: "+id,
	).Replace(luaTool)
}

func flowYAMLForScope(id, call string) string {
	return "id: " + id + "\nname: " + id + "\ndescription: scope 守卫用例\nsteps:\n" +
		"  - id: s1\n    call: " + call + "\n    input:\n      k: v\n"
}

// TestScopeFlagReachesEngine：`gear flow run --scope mindstorm` 跑别的应用的 Tool 必须被拒。
func TestScopeFlagReachesEngine(t *testing.T) {
	withTempRegistry(t)
	t.Cleanup(func() { cliScope = "" })
	if err := runCLI(t, "tool", "register", writeTempYAML(t, toolYAMLForScope("other__x"))); err != nil {
		t.Fatalf("注册 Tool 失败: %v", err)
	}
	if err := runCLI(t, "flow", "register", writeTempYAML(t, flowYAMLForScope("mindstorm.up", "other__x"))); err != nil {
		t.Fatalf("注册 Flow 失败: %v", err)
	}
	if cliScope != "" {
		t.Fatalf("前一步不该改动 cliScope，实际 %q", cliScope)
	}

	err := runCLI(t, "flow", "run", "--scope", "mindstorm", "mindstorm.up")
	if err == nil {
		t.Fatal("跨应用引用应被 --scope 守卫拒绝")
	}
	if !strings.Contains(err.Error(), "命名空间守卫") {
		t.Fatalf("报错应点明是命名空间守卫拦的，实际: %v", err)
	}
}

// TestScopeFlagAbsentKeepsOldBehaviour：不传 --scope 时行为与今日一致（默认值策略）。
func TestScopeFlagAbsentKeepsOldBehaviour(t *testing.T) {
	withTempRegistry(t)
	t.Cleanup(func() { cliScope = "" })
	if err := runCLI(t, "tool", "register", writeTempYAML(t, toolYAMLForScope("other__x"))); err != nil {
		t.Fatalf("注册 Tool 失败: %v", err)
	}
	if err := runCLI(t, "flow", "register", writeTempYAML(t, flowYAMLForScope("f", "other__x"))); err != nil {
		t.Fatalf("注册 Flow 失败: %v", err)
	}
	if err := runCLI(t, "flow", "run", "f"); err != nil {
		t.Fatalf("无 --scope 应逐字保持原行为，实际: %v", err)
	}
}

// TestScopeFlagBlocksDirectToolRun：`gear tool run` 是**不经 Flow 就能点名执行任意 Tool**
// 的捷径，也是命名空间守卫最该堵、却最容易被漏掉的一条路——它不走 Flow 解析链。
// 落点见 engine.guardScope 的注释（第 5 处）。
func TestScopeFlagBlocksDirectToolRun(t *testing.T) {
	withTempRegistry(t)
	t.Cleanup(func() { cliScope = "" })
	if err := runCLI(t, "tool", "register", writeTempYAML(t, toolYAMLForScope("other__x"))); err != nil {
		t.Fatalf("注册 Tool 失败: %v", err)
	}
	err := runCLI(t, "tool", "run", "--scope", "mindstorm", "other__x", "--input", `{"name":"x"}`)
	if err == nil {
		t.Fatal("`gear tool run --scope mindstorm` 不应能执行别应用的 Tool")
	}
	if !strings.Contains(err.Error(), "命名空间守卫") {
		t.Fatalf("报错应点明是命名空间守卫拦的，实际: %v", err)
	}
}

// TestScopeFlagAllowsOwnTool：开了 scope 也得能跑自己名下的定义（守卫不能误伤自己）。
func TestScopeFlagAllowsOwnTool(t *testing.T) {
	withTempRegistry(t)
	t.Cleanup(func() { cliScope = "" })
	if err := runCLI(t, "tool", "register", writeTempYAML(t, toolYAMLForScope("mindstorm__x"))); err != nil {
		t.Fatalf("注册 Tool 失败: %v", err)
	}
	if err := runCLI(t, "flow", "register", writeTempYAML(t, flowYAMLForScope("mindstorm.up", "mindstorm__x"))); err != nil {
		t.Fatalf("注册 Flow 失败: %v", err)
	}
	if err := runCLI(t, "flow", "run", "--scope", "mindstorm", "mindstorm.up"); err != nil {
		t.Fatalf("本应用内的调用应放行，实际: %v", err)
	}
}
