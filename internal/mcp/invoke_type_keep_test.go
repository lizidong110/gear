package mcp

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// invokeOnce 调一次 gear_invoke，把结果压成便于断言的短串：
// "OK" / "ERR:<首行错误文案>" / "RPCERR:<...>"。
func invokeOnce(t *testing.T, s *mcp.ClientSession, args map[string]any) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: "gear_invoke", Arguments: args})
	if err != nil {
		return "RPCERR:" + err.Error()
	}
	if !res.IsError {
		return "OK"
	}
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(interface{ GetText() string }); ok {
			sb.WriteString(tc.GetText())
		}
	}
	line := strings.TrimSpace(sb.String())
	if line == "" {
		return "ERR:<空错误内容>"
	}
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	return "ERR:" + line
}

// TestInvokeKeepsJSONTypesAcrossBridge：gear_invoke 的 JSON→Lua 桥接**保持数组与数字类型**
// （REQ-INVOKE-TYPE-KEEP）。
//
// 由来：外部 agent 清单 P1-7 报「MCP 桥接层压平数组与数字」，给了三条报错：
//
//	db.query 的 params（单元素数组） → missing argument with index 1
//	db.batch 的 statements 数组        → 缺少必填参数 statements（非空数组）
//	crypto.jwt_sign 的 ttl: 3600       → 必须是数字，实际 string
//
// 2026-10-03 端到端实测**三条均不成立**（见本用例断言），故驳回该归因。此用例转为**回归锁**，
// 防止日后有人照着错误归因去「修」一个不存在的缺陷，或在重构桥接层时真的把类型压平。
//
// 关键事实（改动前请先读）：MCP SDK v1.8.0 走 `segmentio/encoding/json` 而非标准库，
// 但两者都把 JSON 数字解成 float64（**不是 json.Number**）；`buildActionEntry` 的
// `luaLiteral` 有 float64 分支，渲染成不带引号的数字字面量。故数字全程无损。
func TestInvokeKeepsJSONTypesAcrossBridge(t *testing.T) {
	s := newSmokeSession(t)
	const key = "secret-key-for-hs256-testing-probe"

	cases := []struct {
		label string
		args  map[string]any
	}{
		{"db.query 单元素 params 数组", map[string]any{
			"name": "db", "method": "query",
			"args": map[string]any{"sql": "SELECT ? AS v", "params": []any{"A-001"}},
		}},
		{"db.batch statements 数组", map[string]any{
			"name": "db", "method": "batch",
			"args": map[string]any{"statements": []any{
				map[string]any{"sql": "CREATE TABLE IF NOT EXISTS invoke_type_keep(k TEXT)"},
				map[string]any{"sql": "INSERT INTO invoke_type_keep VALUES('a')"},
			}},
		}},
		{"crypto.jwt_sign ttl 传整数", map[string]any{
			"name": "crypto", "method": "jwt_sign",
			"args": map[string]any{
				"claims": map[string]any{"sub": "u1"}, "key": key, "alg": "HS256", "ttl": 3600,
			},
		}},
	}
	for _, c := range cases {
		if got := invokeOnce(t, s, c.args); got != "OK" {
			t.Errorf("%s 应成功穿过 MCP→Lua 桥接（REQ-INVOKE-TYPE-KEEP），实际 %s", c.label, got)
		}
	}
}

// TestInvokeNumberArgsSurviveLuaRoundTrip 见 engine 包的
// TestBuildActionEntryKeepsNumberArgsAsNumberLiteral——那个测试与 buildActionEntry 同包，
// 放在这里会跨包引用私有函数而编译不过。本文件只留端到端那一层。
