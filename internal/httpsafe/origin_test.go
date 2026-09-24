package httpsafe

import (
	"net/http/httptest"
	"testing"
)

// TestOriginAllowed 表驱动覆盖判定本身（含畸形 Origin）。
//
// 从 internal/server/run_tool_test.go 随函数一并搬来（2026-09-24）：判定逻辑归 httpsafe 后，
// 它的单测就该在 httpsafe 里——否则「函数搬走了、测试还留在原包」会形成第二处真相。
func TestOriginAllowed(t *testing.T) {
	cases := []struct {
		origin string
		want   bool
	}{
		{"", true},
		{"http://127.0.0.1:8787", true},
		{"http://127.0.0.1", true},
		{"http://localhost:5173", true},
		{"http://LOCALHOST:5173", true},
		{"http://[::1]:8080", true},
		{"https://evil.example", false},
		{"null", false},                      // 沙箱 iframe / 本地文件的 Origin
		{"http://127.0.0.1.evil.com", false}, // 前缀伪装
		{"://bad", false},                    // 解析失败
	}
	for _, c := range cases {
		req := httptest.NewRequest("POST", "/api/capabilities/invoke", nil)
		if c.origin != "" {
			req.Header.Set("Origin", c.origin)
		}
		if got := OriginAllowed(req); got != c.want {
			t.Fatalf("OriginAllowed(%q) = %v，期望 %v", c.origin, got, c.want)
		}
	}
}
