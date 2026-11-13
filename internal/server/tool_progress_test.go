package server

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gear/internal/app"
	"gear/internal/model"
	"gear/internal/registry"
)

// REQ-APP-LOGS-DIRECT-TOOL：REST **直接** Tool/能力调用必须在 serve 进度上有可见行。
//
// 背景（2026-11-13 实测）：托管启动 study 后实时日志面板空白——app run 的日志通道只收
// once 步输出 + service 子进程 stdout 行，而前端直接调的 POST /api/tools/{id}/run 此前
// 全程静默。本测试锁住：直接调用打 ✓ 完行、失败打 ✗ 行（含错误），声明资源路由同口径。
func TestDirectToolRunPrintsProgressLine(t *testing.T) {
	reg, err := registry.Open(filepath.Join(t.TempDir(), "registry.db"))
	if err != nil {
		t.Fatalf("打开 registry 失败: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	echo := &model.Tool{
		ID: "echo__p", Name: "echo__p", Runtime: model.RuntimeLua, Entry: "return {}",
		InputSchema:  map[string]interface{}{"type": "object"},
		OutputSchema: map[string]interface{}{"type": "object"},
		Versions:     []model.ToolVersion{{Version: "1.0.0", State: model.StateStable}},
	}
	boom := &model.Tool{
		ID: "boom__p", Name: "boom__p", Runtime: model.RuntimeLua, Entry: `error("炸")`,
		InputSchema:  map[string]interface{}{"type": "object"},
		OutputSchema: map[string]interface{}{"type": "object"},
		Versions:     []model.ToolVersion{{Version: "1.0.0", State: model.StateStable}},
	}
	for _, tl := range []*model.Tool{echo, boom} {
		if err := reg.UpsertTool(tl); err != nil {
			t.Fatalf("注册 Tool %s 失败: %v", tl.ID, err)
		}
	}
	s, err := New(reg)
	if err != nil {
		t.Fatalf("构造 Server 失败: %v", err)
	}
	h := s.Handler()

	// 捕获 stdout（out=nil 时调用时解析 → 临时替换生效，与 ui_test.go 同一手法）。
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe 失败: %v", err)
	}
	old := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = old }()

	do(t, h, http.MethodPost, "/api/tools/echo__p/run", map[string]interface{}{})
	do(t, h, http.MethodPost, "/api/tools/boom__p/run", map[string]interface{}{})
	do(t, h, http.MethodGet, "/api/orders/42", nil) // 未声明资源路由 → 404，不应产生进度行

	_ = w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)
	text := string(out)

	if !strings.Contains(text, "✓ tool echo__p (") {
		t.Fatalf("成功调用应打 ✓ 完行，实际输出:\n%s", text)
	}
	if !strings.Contains(text, "✗ tool boom__p (") || !strings.Contains(text, "炸") {
		t.Fatalf("失败调用应打 ✗ 行且带错误原因，实际输出:\n%s", text)
	}
	// 只有这两行进度（✗ 行的错误体可以多行，所以数前缀而不是数总行数）；404 不打进度行。
	if n := strings.Count(text, "✓ tool ") + strings.Count(text, "✗ tool ") + strings.Count(text, "▶ "); n != 2 {
		t.Fatalf("应恰好 2 行进度（404 不打），实际 %d:\n%s", n, text)
	}
	// 控制台（非回推上下文）不带 GEAR_TOOL_CALL 详情 JSON——前台 serve 的 stdout 是人看的。
	if strings.Contains(text, toolCallDetailMarker) {
		t.Fatalf("非回推上下文不应携带详情标记，实际输出:\n%s", text)
	}
}

// REQ-APP-LOGS-DIRECT-TOOL（详情标记）：回推上下文（GEAR_APP_LOG_ID 已注入 = stdout 正被
// app run 抽干进 app 日志通道）下，✓/✗ 行尾携带单行 GEAR_TOOL_CALL JSON（入参 + 返回），
// 供 UI 点击展开；失败行 error 带原因。控制台行为由上面的测试锁定（不带标记）。
func TestDirectToolRunDetailMarkerCarriesInputAndOutput(t *testing.T) {
	t.Setenv(app.EnvAppLogID, "app_test") // 注入回推上下文
	reg, err := registry.Open(filepath.Join(t.TempDir(), "registry.db"))
	if err != nil {
		t.Fatalf("打开 registry 失败: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	echo := &model.Tool{
		ID: "echo__d", Name: "echo__d", Runtime: model.RuntimeLua, Entry: "return {got=ctx.input.x}",
		InputSchema:  map[string]interface{}{"type": "object"},
		OutputSchema: map[string]interface{}{"type": "object"},
		Versions:     []model.ToolVersion{{Version: "1.0.0", State: model.StateStable}},
	}
	boom := &model.Tool{
		ID: "boom__d", Name: "boom__d", Runtime: model.RuntimeLua, Entry: `error("炸")`,
		InputSchema:  map[string]interface{}{"type": "object"},
		OutputSchema: map[string]interface{}{"type": "object"},
		Versions:     []model.ToolVersion{{Version: "1.0.0", State: model.StateStable}},
	}
	for _, tl := range []*model.Tool{echo, boom} {
		if err := reg.UpsertTool(tl); err != nil {
			t.Fatalf("注册 Tool %s 失败: %v", tl.ID, err)
		}
	}
	s, err := New(reg)
	if err != nil {
		t.Fatalf("构造 Server 失败: %v", err)
	}
	h := s.Handler()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe 失败: %v", err)
	}
	old := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = old }()

	do(t, h, http.MethodPost, "/api/tools/echo__d/run", map[string]interface{}{"x": 42})
	do(t, h, http.MethodPost, "/api/tools/boom__d/run", map[string]interface{}{"q": 1})

	_ = w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)
	text := string(out)

	success := extractToolCallDetail(t, text, "✓ tool echo__d")
	if got, _ := success["input"].(map[string]interface{}); got["x"].(float64) != 42 {
		t.Fatalf("详情 JSON 应携带入参 input.x=42，实际 %v", success)
	}
	if got, _ := success["output"].(map[string]interface{}); got["got"].(float64) != 42 {
		t.Fatalf("详情 JSON 应携带返回 output.got=42，实际 %v", success)
	}
	failed := extractToolCallDetail(t, text, "✗ tool boom__d")
	if failed["ok"] != false || failed["error"] == "" {
		t.Fatalf("失败详情应带 ok=false 与 error 原因，实际 %v", failed)
	}
}

// extractToolCallDetail 从捕获的 stdout 里找到以 prefix 开头且带详情标记的行，解码 JSON。
func extractToolCallDetail(t *testing.T, text, prefix string) map[string]interface{} {
	t.Helper()
	for _, ln := range strings.Split(text, "\n") {
		if !strings.HasPrefix(ln, prefix) || !strings.Contains(ln, toolCallDetailMarker) {
			continue
		}
		raw := ln[strings.Index(ln, toolCallDetailMarker)+len(toolCallDetailMarker):]
		var m map[string]interface{}
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			t.Fatalf("详情应为合法单行 JSON: %v\n行: %s", err, ln)
		}
		return m
	}
	t.Fatalf("没找到带详情标记的 %q 行，实际输出:\n%s", prefix, text)
	return nil
}
