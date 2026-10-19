package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestExtDescribeScansRealSidecar 验证 `gear ext describe` 走的是与内核加载同一个
// ProbeSidecarDescribe：拿到的就是内核拿到的方法/参数清单。
//
// 被测侧车用真正实现了 --describe 的 mongo-sidecar.exe（gate 里先 build 才有；
// 单独跑本测试而二进制不存在时跳过，不强制依赖构建顺序）。
func TestExtDescribeScansRealSidecar(t *testing.T) {
	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	sidecar := filepath.Join(repoRoot, "build", "plugin", "mongo-sidecar.exe")
	if _, err := os.Stat(sidecar); err != nil {
		t.Skipf("侧车二进制不存在（gate 里先 build 才有）：%v", err)
	}

	cfg := filepath.Join(t.TempDir(), "ext_probe.yaml")
	// filepath.ToSlash 去掉反斜杠：YAML 双引号串里 \U 之类会被当成非法转义。
	yaml := fmt.Sprintf("ext.cli_probe:\n  cmd: [%q]\n  describe: true\n", filepath.ToSlash(sidecar))
	if err := os.WriteFile(cfg, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GEAR_EXT_CONFIG", cfg)

	var buf strings.Builder
	root := newRootCmd()
	root.SetArgs([]string{"ext", "describe", "ext.cli_probe"})
	root.SetOut(&buf)
	root.SetErr(&buf)
	if err := root.Execute(); err != nil {
		t.Fatalf("describe 不应报错: %v\n%s", err, buf.String())
	}
	out := buf.String()
	if !strings.Contains(out, "方法") || !strings.Contains(out, "find") {
		t.Fatalf("摘要应列出方法（含 find），实际: %s", out)
	}

	// --json 应输出可解析的原始 spec
	var buf2 strings.Builder
	root2 := newRootCmd()
	root2.SetArgs([]string{"ext", "describe", "ext.cli_probe", "--json"})
	root2.SetOut(&buf2)
	root2.SetErr(&buf2)
	if err := root2.Execute(); err != nil {
		t.Fatalf("--json 不应报错: %v", err)
	}
	var s map[string]interface{}
	if err := json.Unmarshal([]byte(buf2.String()), &s); err != nil {
		t.Fatalf("--json 应打出合法 JSON: %v\n%s", err, buf2.String())
	}
	if _, ok := s["methods"]; !ok {
		t.Fatalf("--json 输出缺 methods: %s", buf2.String())
	}
}

// TestExtDescribeReportsUnknownExtension 验证未知扩展名应被点名报错（走 error 而非静默通过）。
func TestExtDescribeReportsUnknownExtension(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "ext_empty.yaml")
	if err := os.WriteFile(cfg, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GEAR_EXT_CONFIG", cfg)

	var buf strings.Builder
	root := newRootCmd()
	root.SetArgs([]string{"ext", "describe", "no_such_ext"})
	root.SetOut(&buf)
	root.SetErr(&buf)
	if err := root.Execute(); err == nil {
		t.Fatal("未知扩展应当报错")
	}
	if !strings.Contains(buf.String(), "no_such_ext") {
		t.Fatalf("错误应点名未知扩展: %s", buf.String())
	}
}
