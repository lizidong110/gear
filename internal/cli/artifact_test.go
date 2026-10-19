package cli

// CLI 侧产物命令的用例：证明「人的接口」与 MCP 走的是同一份实现，且失败路径会**报错**而不是静默。

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gear/internal/artifact"
	"gear/internal/model"
	"gear/internal/registry"
)

// runArtifact 执行一次 export/import 子命令（registry 指到临时库，避免污染 ~/.gear）。
func runArtifact(t *testing.T, regPath string, args ...string) (string, error) {
	t.Helper()
	root := newRootCmd()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs(append([]string{"--registry", regPath}, args...))
	err := root.Execute()
	return buf.String(), err
}

// seedApp 往 registry 里放一个应用（App + Tool + Flow）并把静态资源写到 root 下。
func seedApp(t *testing.T, regPath, root, id string) {
	t.Helper()
	reg, err := registry.Open(regPath)
	if err != nil {
		t.Fatalf("打开 registry 失败: %v", err)
	}
	defer reg.Close()
	a := &model.App{
		ID:        id,
		Name:      id,
		EntryFlow: id + ".up",
		Layout: model.AppLayout{
			Workdir: "apps/" + id,
			Files:   []model.AppFile{{Path: "apps/" + id + "/www/index.html", Required: true}},
		},
	}
	p := filepath.Join(root, filepath.FromSlash(a.Layout.Files[0].Path))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("<html></html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	b := &artifact.Bundle{
		Apps: []*model.App{a},
		Tools: []*model.Tool{{
			ID: id + "__t", Runtime: model.RuntimeLua, Entry: "return {}",
			InputSchema:  map[string]any{"type": "object"},
			OutputSchema: map[string]any{"type": "object"},
			Versions:     []model.ToolVersion{{Version: "1.0.0", State: model.StateStable}},
		}},
		Flows: []*model.Flow{{ID: id + ".up", Steps: []model.Step{{ID: "s1", Call: id + "__t"}}}},
	}
	if _, _, _, err := artifact.ApplyBundle(reg, b, nil, true); err != nil {
		t.Fatalf("落库失败: %v", err)
	}
}

// seedAppWithCapabilities 同 seedApp，但让落库的 Tool 声明给定的 capabilities。
//
// 为什么要单独一个：REQ-IMPORT-EXTNAMES 要验的正是「声明了 ext.* 的 Tool 能被导入」，
// 没有这个变体就只能验一个不带 capability 的普通应用——而那条路径本来就不会触发前置。
func seedAppWithCapabilities(t *testing.T, regPath, root, id string, caps []string) {
	t.Helper()
	reg, err := registry.Open(regPath)
	if err != nil {
		t.Fatalf("打开 registry 失败: %v", err)
	}
	defer reg.Close()
	a := &model.App{
		ID: id, Name: id, EntryFlow: id + ".up",
		Layout: model.AppLayout{Workdir: "apps/" + id},
	}
	b := &artifact.Bundle{
		Apps: []*model.App{a},
		Tools: []*model.Tool{{
			ID: id + "__t", Runtime: model.RuntimeLua, Entry: "return {}",
			Capabilities: caps,
			InputSchema:  map[string]any{"type": "object"},
			OutputSchema: map[string]any{"type": "object"},
			Versions:     []model.ToolVersion{{Version: "1.0.0", State: model.StateStable}},
		}},
		Flows: []*model.Flow{{ID: id + ".up", Steps: []model.Step{{ID: "s1", Call: id + "__t"}}}},
	}
	if _, _, _, err := artifact.ApplyBundle(reg, b, nil, true); err != nil {
		t.Fatalf("落库失败: %v", err)
	}
}

// TestExportImportCommands CLI 侧 export → import 往返（含 zip 形态与 dry-run）。
func TestExportImportCommands(t *testing.T) {
	dir := t.TempDir()
	regPath := filepath.Join(dir, "src.db")
	root := t.TempDir()
	seedApp(t, regPath, root, "demo")

	// 目录形态导出。
	out := filepath.Join(dir, "artifact")
	str, err := runArtifact(t, regPath, "export", "demo", "--out", out, "--root", root)
	if err != nil {
		t.Fatalf("export 失败: %v (%s)", err, str)
	}
	if !strings.Contains(str, "已导出 demo") {
		t.Fatalf("export 输出不像成功: %q", str)
	}
	if _, err := os.Stat(filepath.Join(out, "bundle.yaml")); err != nil {
		t.Fatalf("产物缺 bundle.yaml: %v", err)
	}

	// zip 形态导出（扩展名自动补）。
	zipOut := filepath.Join(dir, "demo")
	if _, err := runArtifact(t, regPath, "export", "demo", "--out", zipOut, "--format", "zip", "--root", root); err != nil {
		t.Fatalf("export zip 失败: %v", err)
	}
	if _, err := os.Stat(zipOut + artifact.ZipExt); err != nil {
		t.Fatalf("zip 产物不在 %s: %v", zipOut+artifact.ZipExt, err)
	}

	// 导进另一个库（先 dry-run 再真导）。
	dstPath := filepath.Join(dir, "dst.db")
	str, err = runArtifact(t, dstPath, "import", out, "--dry-run")
	if err != nil {
		t.Fatalf("import --dry-run 失败: %v", err)
	}
	if !strings.Contains(str, "dry-run") {
		t.Fatalf("dry-run 应明确说没写入: %q", str)
	}
	str, err = runArtifact(t, dstPath, "import", out, "--apps-dir", filepath.Join(dir, "apps"))
	if err != nil {
		t.Fatalf("import 失败: %v", err)
	}
	if !strings.Contains(str, "created=") {
		t.Fatalf("import 输出缺 created: %q", str)
	}
}

// TestExportImportCommandErrors 失败路径必须**报错退出**，不能静默成功。
func TestExportImportCommandErrors(t *testing.T) {
	dir := t.TempDir()
	regPath := filepath.Join(dir, "src.db")

	// 缺 --out。
	if _, err := runArtifact(t, regPath, "export", "demo"); err == nil {
		t.Fatal("缺 --out 应报错")
	}
	// 应用不存在。
	if _, err := runArtifact(t, regPath, "export", "nope", "--out", filepath.Join(dir, "a")); err == nil {
		t.Fatal("应用不存在应报错")
	}
	// 导入路径不存在。
	if _, err := runArtifact(t, regPath, "import", filepath.Join(dir, "nope")); err == nil {
		t.Fatal("产物路径不存在应报错")
	}
	// 导入的不是产物。
	if _, err := runArtifact(t, regPath, "import", dir); err == nil {
		t.Fatal("不是产物的目录应报错")
	}
}
