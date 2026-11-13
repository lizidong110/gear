package cli

// mcp_expose_test.go：`--expose {flows,tools,all}` 替换 `--expose-flows` 的 CLI 回归
// （REQ-MCP-EXPOSE-TOOLS）。只测**会提前返回**的路径——合法值会进入阻塞的 stdio/HTTP
// transport，测试进程不该去碰它。

import (
	"strings"
	"testing"
)

func TestMCPExposeFlagReplacesExposeFlows(t *testing.T) {
	cmd := mcpCmd()
	if cmd.Flags().Lookup("expose") == nil {
		t.Fatal("gear mcp 应注册 --expose")
	}
	if cmd.Flags().Lookup("expose-flows") != nil {
		t.Fatal("--expose-flows 已由 --expose flows 取代，不该继续注册")
	}
}

func TestMCPExposeFlagRejectsUnknownValue(t *testing.T) {
	t.Cleanup(func() { mcpExpose = "" })
	err := runCLI(t, "mcp", "--expose", "everything")
	if err == nil {
		t.Fatal("未知 --expose 值必须直接失败（不许静默当成不暴露）")
	}
	if !strings.Contains(err.Error(), "--expose") {
		t.Fatalf("错误信息应指向 --expose，实际: %v", err)
	}
}

func TestMCPExposeFlowsFlagIsGone(t *testing.T) {
	err := runCLI(t, "mcp", "--expose-flows")
	if err == nil || !strings.Contains(err.Error(), "unknown flag") {
		t.Fatalf("旧 --expose-flows 应报 unknown flag（破坏性变更已同步进文档），实际: %v", err)
	}
}

func TestMCPOnlyExposedFlagRegistered(t *testing.T) {
	cmd := mcpCmd()
	if cmd.Flags().Lookup("only-exposed") == nil {
		t.Fatal("gear mcp 应注册 --only-exposed（应用专用面）")
	}
}

func TestMCPOnlyExposedRequiresExposeFlag(t *testing.T) {
	t.Cleanup(func() { mcpOnlyExposed = false })
	err := runCLI(t, "mcp", "--only-exposed")
	if err == nil || !strings.Contains(err.Error(), "--expose") {
		t.Fatalf("--only-exposed 未配 --expose 应 fail-closed（零工具空端点），实际: %v", err)
	}
}
