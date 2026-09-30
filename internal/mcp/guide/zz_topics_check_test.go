package guide_test

import (
	"strings"
	"testing"

	"gear/internal/mcp/guide"
)

func TestSidecarTopicRegistered(t *testing.T) {
	joined := strings.Join(guide.Topics(), ",")
	if !strings.Contains(joined, "sidecar") {
		t.Fatalf("sidecar 未进 Topics(): %v", guide.Topics())
	}
	body, err := guide.Get("sidecar")
	if err != nil || len(body) < 500 {
		t.Fatalf("取 sidecar 正文失败: err=%v len=%d", err, len(body))
	}
	t.Logf("topics(%d)=%s", len(guide.Topics()), joined)
}
