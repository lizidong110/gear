package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

// S-port-1：servePortLine 吐出的是**可解析的数字**，且端口号与绑定地址同源。
// 下游解析首行 JSON 拿到的若是字符串（`"8787"`），拼出来的 URL 会被当成路径、查不出原因。
func TestServePortLineEmitsParseableNumber(t *testing.T) {
	line := servePortLine("127.0.0.1:8787")
	if !strings.HasPrefix(line, "{\"port\":") {
		t.Fatalf("首行应是 {\"port\":N} 形态，实际 %q", line)
	}
	var obj map[string]interface{}
	if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &obj); err != nil {
		t.Fatalf("首行必须是合法 JSON（父侧按 JSON 解析）：%v，实际 %q", err, line)
	}
	port, ok := obj["port"].(float64)
	if !ok || port != 8787 {
		t.Fatalf("port 应为数字 8787，实际 %v（类型 %T）", obj["port"], obj["port"])
	}
}

// S-port-2：拿不到端口号时必须返回**空串**，绝不吐 `{"port":0}`。
// 0 是一个会被下游当真、且事后查不出来源的假值，比「什么都没说」坏得多。
func TestServePortLineRefusesToInventPort(t *testing.T) {
	for _, addr := range []string{"", ":0", "http://127.0.0.1:8787", "127.0.0.1"} {
		if got := servePortLine(addr); got != "" {
			t.Fatalf("地址 %q 拿不到端口时应返回空串，实际 %q", addr, got)
		}
	}
}
