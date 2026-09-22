package mcp

import (
	"strings"
	"testing"
)

// TestAppPruneAction 覆盖 gear_app(action=prune)。
//
// 动机（2026-09-22 实测）：一轮调试下来台账里堆了 12 条 startup_failed / unknown / exited，
// 而此前只有按 app_id 单个 stop 的入口——清理要调 12 次，且无从按状态过滤。
func TestAppPruneAction(t *testing.T) {
	f := newSmokeFixture(t)

	var out struct {
		DryRun       bool             `json:"dry_run"`
		Pruned       []map[string]any `json:"pruned"`
		Skipped      []map[string]any `json:"skipped"`
		PrunedCount  int              `json:"pruned_count"`
		SkippedCount int              `json:"skipped_count"`
		AppsDir      string           `json:"apps_dir"`
	}
	callTool(t, f.session, "gear_app", map[string]any{"action": "prune", "dry_run": true}, &out)

	if !out.DryRun {
		t.Fatalf("dry_run 应回显 true: %+v", out)
	}
	// 台账目录必须回**解析后**的值：报空串等于让调用方自己猜去哪找（AppDir 的默认规则
	// 由 app.ResolveAppsDir 决定）。
	if out.AppsDir == "" {
		t.Fatal("应返回解析后的台账目录")
	}
	if out.PrunedCount != len(out.Pruned) || out.SkippedCount != len(out.Skipped) {
		t.Fatalf("计数与明细必须一致: %+v", out)
	}
}

// TestAppPruneActionUnknownListsPrune：action 白名单必须把 prune 写进去，
// 否则调用方（尤其智能体）只会看到「start | stop | status | list」而永远发现不了这个入口。
func TestAppPruneActionUnknownListsPrune(t *testing.T) {
	f := newSmokeFixture(t)
	text := expectToolError(t, f.session, "gear_app", map[string]any{"action": "nope"}, "prune")
	if !strings.Contains(text, "prune") {
		t.Fatalf("非法 action 的提示里应列出 prune: %s", text)
	}
}
