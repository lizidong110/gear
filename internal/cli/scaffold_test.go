package cli

import (
	"path/filepath"
	"testing"

	"gear/internal/model"
	"gear/internal/registry"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// TestScaffoldToolYAMLValid 断言三种 runtime 的 Tool 骨架都能通过 model.Tool.Validate()
// （即「生成即可 gear tool register」）。覆盖 process / lua / go-host。
func TestScaffoldToolYAMLValid(t *testing.T) {
	for _, rt := range []string{"process", "lua", "go-host"} {
		rt := rt
		t.Run(rt, func(t *testing.T) {
			content, err := scaffoldToolYAML("demo-"+rt, rt)
			if err != nil {
				t.Fatalf("scaffoldToolYAML(%s) 返回错误: %v", rt, err)
			}
			var tool model.Tool
			if err := yaml.Unmarshal([]byte(content), &tool); err != nil {
				t.Fatalf("YAML 解析失败: %v\n---\n%s", err, content)
			}
			if tool.ID == "" || tool.Runtime == "" {
				t.Fatalf("生成的 Tool 缺少 id/runtime: %s", content)
			}
			if err := tool.Validate(); err != nil {
				t.Fatalf("model.Tool.Validate 失败: %v\n---\n%s", err, content)
			}
		})
	}
}

// TestScaffoldToolYAMLRejectsBadRuntime 断言 wasm 等未实现 runtime 被生成器直接拒绝。
func TestScaffoldToolYAMLRejectsBadRuntime(t *testing.T) {
	if _, err := scaffoldToolYAML("x", "wasm"); err == nil {
		t.Fatal("wasm 未实现，scaffoldToolYAML 应返回错误")
	}
	if _, err := scaffoldToolYAML("x", "bogus"); err == nil {
		t.Fatal("非法 runtime，scaffoldToolYAML 应返回错误")
	}
}

// TestScaffoldFlowYAMLValid 断言 Flow 骨架通过结构校验 + 静态校验（ValidateFlowStatic），
// 证明起步 step（crypto.hash，纯函数原子）无需任何已注册 Tool 即可注册。
func TestScaffoldFlowYAMLValid(t *testing.T) {
	content := scaffoldFlowYAML("demo-flow")
	var flow model.Flow
	if err := yaml.Unmarshal([]byte(content), &flow); err != nil {
		t.Fatalf("YAML 解析失败: %v\n---\n%s", err, content)
	}
	if err := flow.Validate(); err != nil {
		t.Fatalf("结构校验失败: %v", err)
	}

	reg := mustOpenTempRegistry(t)
	defer reg.Close()
	eng, err := cliEngine(reg)
	if err != nil {
		t.Fatalf("cliEngine 构造失败: %v", err)
	}
	if err := eng.ValidateFlowStatic(&flow); err != nil {
		t.Fatalf("ValidateFlowStatic 失败（骨架应可直接注册）: %v\n---\n%s", err, content)
	}
}

// TestScaffoldHookYAMLValid 断言 Hook 骨架通过 model.Hook.Validate()。
func TestScaffoldHookYAMLValid(t *testing.T) {
	content := scaffoldHookYAML("demo-hook")
	var h model.Hook
	if err := yaml.Unmarshal([]byte(content), &h); err != nil {
		t.Fatalf("YAML 解析失败: %v\n---\n%s", err, content)
	}
	if err := h.Validate(); err != nil {
		t.Fatalf("model.Hook.Validate 失败: %v\n---\n%s", err, content)
	}
	// 负向：把 stage 改成非法值必须 Validate 失败（证明上面的通过不是空过）。
	h.Stage = "nonsense"
	if err := h.Validate(); err == nil {
		t.Fatal("非法 stage 应通过 Validate 失败")
	}
}

// TestScaffoldEmitScaffoldForceGuard 断言已存在文件时默认拒绝、--force 才覆盖。
func TestScaffoldEmitScaffoldForceGuard(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.yaml")
	cmd := &cobra.Command{}
	if err := emitScaffold(cmd, "v1", path, false); err != nil {
		t.Fatalf("首次写入应成功: %v", err)
	}
	if err := emitScaffold(cmd, "v2", path, false); err == nil {
		t.Fatal("文件已存在且无 --force 时应拒绝")
	}
	if err := emitScaffold(cmd, "v3", path, true); err != nil {
		t.Fatalf("--force 应覆盖: %v", err)
	}
}

func mustOpenTempRegistry(t *testing.T) *registry.Registry {
	t.Helper()
	path := filepath.Join(t.TempDir(), "reg.db")
	reg, err := registry.Open(path)
	if err != nil {
		t.Fatalf("open registry 失败: %v", err)
	}
	return reg
}
