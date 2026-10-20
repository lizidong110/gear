package cli

import (
	"io"
	"os"
	"strings"
	"testing"

	"gear/internal/model"
	"gear/internal/registry"
)

// captureStdout 捕获一段代码的 stdout（列表命令直写 os.Stdout，没有 writer 注入点）。
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("建管道失败: %v", err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		raw, _ := io.ReadAll(r)
		done <- string(raw)
	}()
	fn()
	_ = w.Close()
	os.Stdout = old
	out := <-done
	_ = r.Close()
	return out
}

// scopeToolYAML 造一个 lua echo Tool 的注册 YAML（id 决定 scope 分组）。
func scopeToolYAML(t *testing.T, id string) string {
	t.Helper()
	return writeTempYAML(t, `id: `+id+`
name: `+id+`
description: scope 列表测试
runtime: lua
entry: |
  return { ok = true }
input_schema:
  type: object
output_schema:
  type: object
versions:
  - version: 1.0.0
    state: stable
    digest: ""
    note: scope 列表测试
`)
}

// seedScopeRegistry 注册 wms__ok（本应用）/ other__x（别人的）/ plat.probe（平台级），
// 并注册 wms 与 other 两个 App——「跨应用」判定要求对方确实是已注册 App
// （无主 id 一律按平台级，这是 2026-10-21 修复后的口径）。
func seedScopeRegistry(t *testing.T, db string) {
	t.Helper()
	for _, id := range []string{"wms__ok", "other__x", "plat.probe"} {
		if err := runCLI(t, "tool", "register", scopeToolYAML(t, id), "--registry", db); err != nil {
			t.Fatalf("注册 %s 失败: %v", id, err)
		}
	}
	reg, err := registry.Open(db)
	if err != nil {
		t.Fatalf("打开 registry 失败: %v", err)
	}
	defer reg.Close()
	for _, a := range []*model.App{
		{ID: "wms", EntryFlow: "wms.up"},
		{ID: "other", EntryFlow: "other.up"},
	} {
		if err := reg.UpsertApp(a); err != nil {
			t.Fatalf("注册 App %s 失败: %v", a.ID, err)
		}
	}
}

// seedScopeApp 往当前测试 registry 注册一个 App。UpsertApp 硬校验 entry_flow 非空。
// 「跨应用」判定的前提是对方**确实是已注册 App**——无主 id 一律按平台级豁免放行，
// 这是 2026-10-21 修复后的口径；不补注册，用例测的就不是「跨应用」而是「平台级」。
func seedScopeApp(t *testing.T, id, entry string) {
	t.Helper()
	reg, err := registry.Open(testRegistryOverride)
	if err != nil {
		t.Fatalf("打开 registry 失败: %v", err)
	}
	defer reg.Close()
	if err := reg.UpsertApp(&model.App{ID: id, EntryFlow: entry}); err != nil {
		t.Fatalf("注册 App %s 失败: %v", id, err)
	}
}

// TestScopedListGroupsAndHides —— 2026-10-21 用户实测报缺：`tool list --scope wms`
// 把其他应用的 Tool 全列出来（目录面从不看 scope）。修复口径：本应用名下为主体组、
// 平台级另组标注「scope 内不受守卫」、别人的隐藏但报数（不静默）。
func TestScopedListGroupsAndHides(t *testing.T) {
	db := tempDB(t)
	seedScopeRegistry(t, db)

	out := captureStdout(t, func() {
		if err := runCLI(t, "tool", "list", "--scope", "wms", "--registry", db); err != nil {
			t.Fatalf("list 失败: %v", err)
		}
	})
	for _, want := range []string{"── wms 名下（1）──", "wms__ok", "── 平台级（--scope 内不受守卫，1）──", "plat.probe", "已按 --scope wms 隐藏 1 个"} {
		if !strings.Contains(out, want) {
			t.Fatalf("输出应含 %q，实际:\n%s", want, out)
		}
	}
	if strings.Contains(out, "other__x") {
		t.Fatalf("别人的 Tool 应被隐藏，实际:\n%s", out)
	}
}

// TestScopedListNoScopeUnchanged —— 无 --scope 时输出必须逐字保持存量行为：
// 全量平列、无组标题、无隐藏行（默认不开是启用策略，回归保护）。
func TestScopedListNoScopeUnchanged(t *testing.T) {
	db := tempDB(t)
	seedScopeRegistry(t, db)

	out := captureStdout(t, func() {
		if err := runCLI(t, "tool", "list", "--registry", db); err != nil {
			t.Fatalf("list 失败: %v", err)
		}
	})
	for _, want := range []string{"wms__ok", "other__x", "plat.probe"} {
		if !strings.Contains(out, want) {
			t.Fatalf("无 scope 应全量列出 %q，实际:\n%s", want, out)
		}
	}
	for _, unwanted := range []string{"──", "名下", "平台级", "隐藏"} {
		if strings.Contains(out, unwanted) {
			t.Fatalf("无 scope 不应出现分组/隐藏字样 %q，实际:\n%s", unwanted, out)
		}
	}
}

// TestScopeGroupOf 单测分组判定的四个分支 + 平台级/跨应用两面。
func TestScopeGroupOf(t *testing.T) {
	apps := []*model.App{{ID: "wms"}, {ID: "other"}}
	cases := []struct {
		scope, id string
		want      scopeGroup
	}{
		{"wms", "wms__ok", groupOwn},
		{"wms", "wms.up", groupOwn}, // Owns 的第二种前缀形态
		{"wms", "other__x", groupForeign},
		{"wms", "other.up", groupForeign},
		{"wms", "ext.serve", groupPlatform},      // 第一段 ext 不是 App
		{"wms", "web_ui.api", groupPlatform},     // 单下划线形态：第一段是 web_ui 不是 web
		{"wms", "platform_probe", groupPlatform}, // 无分隔符：整个 id 是第一段
		{"", "other__x", groupAll},               // 无 scope：不过滤
	}
	for _, c := range cases {
		cliScope = c.scope
		t.Cleanup(func() { cliScope = "" })
		if got := scopeGroupOf(apps, c.id); got != c.want {
			t.Fatalf("scopeGroupOf(%q, %q) = %d，期望 %d", c.scope, c.id, got, c.want)
		}
	}
}
