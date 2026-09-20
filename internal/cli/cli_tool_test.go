package cli

import (
	"strings"
	"testing"
)

// 本文件补 `gear tool` 的三条此前无人执行的命令（run / versions / promote）与 `gear ext`。
// 它们的共同点：注册期校验与运行期执行都要走**同一套引擎装配**（cliEngine），
// 不测就没人知道「命令参数 → 引擎调用」这一层有没有接错。

// luaTool 是不依赖外部命令的 Tool（process runtime 要能找到 sh/echo，lua 则完全进程内）。
const luaTool = `id: lua.echo
name: lua echo
description: 回显输入
runtime: lua
entry: |
  return { echo = ctx.input.name }
input_schema:
  type: object
output_schema:
  type: object
versions:
  - version: 1.0.0
    state: stable
`

func TestToolRunExecutesAndPrintsOutput(t *testing.T) {
	withTempRegistry(t)
	if err := runCLI(t, "tool", "register", writeTempYAML(t, luaTool)); err != nil {
		t.Fatalf("注册 Tool 失败: %v", err)
	}
	// --arg 单字段输入（值按 JSON 解析）
	if err := runCLI(t, "tool", "run", "lua.echo", "--arg", `name="hi"`); err != nil {
		t.Fatalf("tool run 失败: %v", err)
	}
	// --input 整个 JSON 对象
	if err := runCLI(t, "tool", "run", "lua.echo", "--input", `{"name":"x"}`); err != nil {
		t.Fatalf("tool run --input 失败: %v", err)
	}
	// 不存在的 Tool 必须报错（不静默成功）
	if err := runCLI(t, "tool", "run", "nope"); err == nil {
		t.Fatal("运行不存在的 Tool 必须报错")
	}
}

func TestToolVersionsListsAndReportsMissing(t *testing.T) {
	withTempRegistry(t)
	if err := runCLI(t, "tool", "register", writeTempYAML(t, validTool)); err != nil {
		t.Fatalf("注册 Tool 失败: %v", err)
	}
	if err := runCLI(t, "tool", "versions", "t1"); err != nil {
		t.Fatalf("tool versions 失败: %v", err)
	}
	err := runCLI(t, "tool", "versions", "nope")
	if err == nil || !strings.Contains(err.Error(), "Tool 不存在") {
		t.Fatalf("不存在的 Tool 必须报错（不静默打印空列表），实际 %v", err)
	}
}

func TestToolPromoteRejectsIllegalStateThenPromotes(t *testing.T) {
	withTempRegistry(t)
	if err := runCLI(t, "tool", "register", writeTempYAML(t, validTool)); err != nil {
		t.Fatalf("注册 Tool 失败: %v", err)
	}
	// 非法状态在命令层就拒（未知枚举即拒，不交给下层静默处理）
	err := runCLI(t, "tool", "promote", "t1", "1.0.0", "bogus")
	if err == nil || !strings.Contains(err.Error(), "非法目标状态") {
		t.Fatalf("非法目标状态必须被拒，实际 %v", err)
	}
	// 合法迁移：stable → deprecated
	if err := runCLI(t, "tool", "promote", "t1", "1.0.0", "deprecated"); err != nil {
		t.Fatalf("合法晋升应成功: %v", err)
	}
	// 非法迁移（deprecated → draft）由 model.CanPromote 在 registry 层拒
	if err := runCLI(t, "tool", "promote", "t1", "1.0.0", "draft"); err == nil {
		t.Fatal("非法状态迁移必须被拒")
	}
}

func TestExtCmdReportsUnknownExtensionOnWire(t *testing.T) {
	// 未知扩展名由 ext.Run 统一报错（本命令刻意不维护第二份名单，避免与注册表漂移）。
	// 报错走 **sidecar 协议帧**（stdout 的 {"error":...}）而不是进程退出码——
	// 这样引擎能拿到具体原因，而不是笼统的「sidecar 退出异常」。
	var buf strings.Builder
	root := newRootCmd()
	root.SetArgs([]string{"ext", "no_such_ext"})
	// 喂一行合法请求帧：未知名是在**解析之后**才判定的（先有合法请求，才谈得上未知扩展）
	root.SetIn(strings.NewReader(`{"method":"whatever","args":{}}`))
	root.SetOut(&buf)
	root.SetErr(&buf)
	if err := root.Execute(); err != nil {
		t.Fatalf("未知名应以协议帧报错、不靠退出码，实际退出错误: %v", err)
	}
	if !strings.Contains(buf.String(), "未知扩展") || !strings.Contains(buf.String(), "no_such_ext") {
		t.Fatalf("应向调用方写回「未知扩展」错误帧，实际输出: %s", buf.String())
	}
}
