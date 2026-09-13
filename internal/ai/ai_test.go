package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeInferrer 是 Inferrer 的 Mock：按序吐出预设文本（用于验证重试 / 收敛逻辑）。
type fakeInferrer struct {
	outputs []string
	idx     int
	err     error
}

func (f *fakeInferrer) Infer(ctx context.Context, prompt string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	if f.idx >= len(f.outputs) {
		return "", fmt.Errorf("mock 已无更多输出")
	}
	s := f.outputs[f.idx]
	f.idx++
	return s, nil
}

// TestStripYAMLFence 覆盖「去 ```yaml 围栏」与「无围栏原样返回」两分支。
func TestStripYAMLFence(t *testing.T) {
	fenced := "好的，这是 Flow：\n```yaml\nid: x\nsteps: []\n```\n以上。"
	got := stripYAMLFence(fenced)
	if strings.Contains(got, "```") {
		t.Fatalf("围栏未被去除: %q", got)
	}
	if !strings.Contains(got, "id: x") {
		t.Fatalf("内容丢失: %q", got)
	}

	plain := "id: y\nsteps: []"
	if stripYAMLFence(plain) != plain {
		t.Fatalf("无围栏时应原样返回")
	}
}

// TestGenerateFlowSuccess 验证「合法输出一次收敛成功」。
func TestGenerateFlowSuccess(t *testing.T) {
	inf := &fakeInferrer{outputs: []string{"```yaml\nid: gen-flow\nsteps:\n  - id: s1\n    call: t1\n```"}}
	flow, err := GenerateFlow(context.Background(), inf, "make a flow")
	if err != nil {
		t.Fatalf("应成功: %v", err)
	}
	if flow.ID != "gen-flow" {
		t.Fatalf("id 解析错误: %s", flow.ID)
	}
}

// TestGenerateFlowRetry 验证「前两次非法、第三次合法」时仍能收敛成功（重试 ≤3）。
func TestGenerateFlowRetry(t *testing.T) {
	inf := &fakeInferrer{outputs: []string{
		"这不是 YAML: foo",                    // 缺 id → Validate 失败
		"id: bad\nsteps: []",                   // 仍缺合法 step → 若上面其实合法？这里第一步才是非法
		"```yaml\nid: ok-flow\nsteps:\n  - id: s1\n    call: t1\n```",
	}}
	flow, err := GenerateFlow(context.Background(), inf, "p")
	if err != nil {
		t.Fatalf("重试后应变成功: %v", err)
	}
	if flow.ID != "ok-flow" {
		t.Fatalf("id 错误: %s", flow.ID)
	}
}

// TestGenerateFlowExhausted 验证「连续非法 → 重试 3 次仍失败 → 返回错误」。
func TestGenerateFlowExhausted(t *testing.T) {
	inf := &fakeInferrer{outputs: []string{"garbage", "garbage", "garbage"}}
	_, err := GenerateFlow(context.Background(), inf, "p")
	if err == nil {
		t.Fatal("应返回错误")
	}
	if !strings.Contains(err.Error(), "3 次") {
		t.Fatalf("错误应说明重试 3 次: %v", err)
	}
}

// TestGenerateFlowInferError 验证「推断器本身报错」也走重试并失败。
func TestGenerateFlowInferError(t *testing.T) {
	inf := &fakeInferrer{err: fmt.Errorf("ollama 离线")}
	if _, err := GenerateFlow(context.Background(), inf, "p"); err == nil {
		t.Fatal("推断报错应上抛")
	}
}

// TestOllamaInfer 用 httptest 模拟 Ollama /api/generate，验证客户端解析正确（避免真实联网）。
func TestOllamaInfer(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req["model"] != "qwen3.5:4b" {
			t.Errorf("model 应为 qwen3.5:4b，实为 %v", req["model"])
		}
		if req["stream"] != false {
			t.Errorf("stream 应为 false")
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, "{\"response\":\"```yaml\\nid: x\\nsteps:\\n  - id: s1\\n    call: t1\\n```\",\"done\":true}")
	}))
	defer ts.Close()

	c := NewOllama(ts.URL, "qwen3.5:4b")
	out, err := c.Infer(context.Background(), "hi")
	if err != nil {
		t.Fatalf("Infer 失败: %v", err)
	}
	flow, err := parseAndValidateFlow(out)
	if err != nil {
		t.Fatalf("收敛失败: %v", err)
	}
	if flow.ID != "x" {
		t.Fatalf("id 应为 x，实为 %s", flow.ID)
	}
}
