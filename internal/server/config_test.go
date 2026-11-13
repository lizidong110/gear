package server

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"gear/internal/cap"
)

// configServer 构造一个配置库指向临时目录的 Server（避免污染真实 <GEAR_HOME>/config.db）。
func configServer(t *testing.T) *Server {
	t.Helper()
	s, _ := newTestServer(t, WithConfigStore(cap.NewConfigStore(filepath.Join(t.TempDir(), "config.db"))))
	return s
}

func decodeItems(t *testing.T, rec *httptestRecorder) []configItemResp {
	t.Helper()
	var out struct {
		Items []configItemResp `json:"items"`
	}
	if err := json.Unmarshal([]byte(rec.body), &out); err != nil {
		t.Fatalf("解析响应失败: %v; body=%s", err, rec.body)
	}
	return out.Items
}

// httptestRecorder 只是把 do() 返回的 recorder 的 body 拿出来，避免每处重复 Body.String()。
type httptestRecorder struct{ body string }

func recOf(t *testing.T, s *Server, method, path string, body interface{}) *httptestRecorder {
	t.Helper()
	rec, _ := do(t, s.mux, method, path, body)
	return &httptestRecorder{body: rec.Body.String()}
}

// TestConfigCatalogEndpoint：目录端点是静态的、不读库，且 key 与内核目录一致。
func TestConfigCatalogEndpoint(t *testing.T) {
	s := configServer(t)
	rec := recOf(t, s, "GET", "/api/config/catalog", nil)
	var out struct {
		Items    []cap.ConfigKeySpec `json:"items"`
		Groups   []string            `json:"groups"`
		Priority string              `json:"priority"`
	}
	if err := json.Unmarshal([]byte(rec.body), &out); err != nil {
		t.Fatalf("解析目录失败: %v", err)
	}
	if len(out.Items) != len(cap.ConfigCatalog()) {
		t.Fatalf("目录条数 %d ≠ 内核 %d", len(out.Items), len(cap.ConfigCatalog()))
	}
	if len(out.Groups) == 0 || !strings.Contains(out.Priority, "环境变量 > 配置库") {
		t.Fatalf("缺分组或优先级说明: %+v", out)
	}
	for i, it := range out.Items {
		if it.Key != cap.ConfigCatalog()[i].Key || it.Label == "" {
			t.Fatalf("第 %d 项与内核目录不一致: %+v", i, it)
		}
	}
}

// TestConfigRoundTrip：写入 → 读出 → 删除（恢复默认），且非敏感值回原文。
func TestConfigRoundTrip(t *testing.T) {
	s := configServer(t)
	r1, m1 := do(t, s.mux, "PUT", "/api/config/ai.model", map[string]string{"value": "Qwen/Qwen3-32B"})
	if r1.Code != http.StatusOK || m1["ok"] != true || m1["known"] != true {
		t.Fatalf("写入失败: %d %v", r1.Code, m1)
	}
	items := decodeItems(t, recOf(t, s, "GET", "/api/config", nil))
	var found *configItemResp
	for i := range items {
		if items[i].Spec.Key == "ai.model" {
			found = &items[i]
		}
	}
	if found == nil || !found.Set || found.Value != "Qwen/Qwen3-32B" || found.Masked {
		t.Fatalf("写入后读不到原值: %+v", found)
	}
	r2, _ := do(t, s.mux, "DELETE", "/api/config/ai.model", nil)
	if r2.Code != http.StatusOK {
		t.Fatalf("删除失败: %d", r2.Code)
	}
	items = decodeItems(t, recOf(t, s, "GET", "/api/config", nil))
	for _, it := range items {
		if it.Spec.Key == "ai.model" && it.Set {
			t.Fatal("删除后仍标记已配置")
		}
	}
	r3, _ := do(t, s.mux, "DELETE", "/api/config/ai.model", nil)
	if r3.Code != http.StatusNotFound {
		t.Fatalf("删除不存在的键应 404，实际 %d", r3.Code)
	}
}

// TestConfigSecretNeverLeaks：ai.api_key 明文绝不出现在任何响应里（红线）。
func TestConfigSecretNeverLeaks(t *testing.T) {
	s := configServer(t)
	const plain = "sk-abcdefghijklmnopqrstuvwxyz"
	rec := recOf(t, s, "PUT", "/api/config/ai.api_key", map[string]string{"value": plain})
	if strings.Contains(rec.body, plain) {
		t.Fatalf("PUT 响应回显了明文: %s", rec.body)
	}
	body := recOf(t, s, "GET", "/api/config", nil).body
	if strings.Contains(body, plain) {
		t.Fatalf("GET 响应回显了明文: %s", body)
	}
	items := decodeItems(t, &httptestRecorder{body: body})
	var key *configItemResp
	for i := range items {
		if items[i].Spec.Key == "ai.api_key" {
			key = &items[i]
		}
	}
	if key == nil || !key.Set || !key.Masked || key.Value != cap.MaskSecret(plain) {
		t.Fatalf("敏感项未按掩码返回: %+v", key)
	}
}

// TestConfigCustomKeyVisible：CLI/Lua 写的目录外键也必须可见，否则「配了却看不见」。
func TestConfigCustomKeyVisible(t *testing.T) {
	s := configServer(t)
	rec, _ := do(t, s.mux, "PUT", "/api/config/myapp.theme", map[string]string{"value": "dark"})
	if rec.Code != http.StatusOK {
		t.Fatalf("自定义键写入失败: %d", rec.Code)
	}
	items := decodeItems(t, recOf(t, s, "GET", "/api/config", nil))
	for _, it := range items {
		if it.Spec.Key == "myapp.theme" {
			if it.Known || it.Value != "dark" || it.Spec.Group != "自定义配置" {
				t.Fatalf("自定义键渲染异常: %+v", it)
			}
			return
		}
	}
	t.Fatal("目录外的自定义键没出现在响应里")
}

// TestConfigCustomSecretMasked：目录外的敏感键同样按名字脱敏（不依赖目录标记）。
func TestConfigCustomSecretMasked(t *testing.T) {
	s := configServer(t)
	const plain = "tok-abcdefghijklmnop"
	_, _ = do(t, s.mux, "PUT", "/api/config/myapp.token", map[string]string{"value": plain})
	body := recOf(t, s, "GET", "/api/config", nil).body
	if strings.Contains(body, plain) {
		t.Fatalf("目录外敏感键泄漏明文: %s", body)
	}
}

// TestConfigEnvOverrideReported：环境变量压过配置库时必须报出来，
// 否则用户在页面改了却看不到效果（不静默降级）。
func TestConfigEnvOverrideReported(t *testing.T) {
	t.Setenv("GEAR_AI_MODEL", "env-model-wins")
	s := configServer(t)
	_, _ = do(t, s.mux, "PUT", "/api/config/ai.model", map[string]string{"value": "config-value"})
	items := decodeItems(t, recOf(t, s, "GET", "/api/config", nil))
	for _, it := range items {
		if it.Spec.Key == "ai.model" {
			if !it.EnvOverride || it.EnvName != "GEAR_AI_MODEL" {
				t.Fatalf("环境变量覆盖未被报出: %+v", it)
			}
			return
		}
	}
	t.Fatal("响应里没有 ai.model")
}

// TestConfigBadRequests：缺 value / 空 key 都要在写入口被拒，不留到运行期。
func TestConfigBadRequests(t *testing.T) {
	s := configServer(t)
	if rec, _ := do(t, s.mux, "PUT", "/api/config/ai.model", map[string]string{}); rec.Code != http.StatusBadRequest {
		t.Fatalf("缺 value 应 400，实际 %d", rec.Code)
	}
	if rec, _ := do(t, s.mux, "PUT", "/api/config/%20", map[string]string{"value": "x"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("空白 key 应 400，实际 %d", rec.Code)
	}
	// 空串是合法值（显式置空以回退下一级），不是 400。
	if rec, _ := do(t, s.mux, "PUT", "/api/config/ai.model", map[string]string{"value": ""}); rec.Code != http.StatusOK {
		t.Fatalf("空串应可写入，实际 %d", rec.Code)
	}
}
