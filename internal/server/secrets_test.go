package server

// 密钥管理面（REQ-API-SECRETS）的契约测试。
//
// 这个面此前**零测试覆盖**（测试文件在整树重建中丢失，见 REQUIREMENTS.md §已知缺口），
// 而它是全仓唯一能写密钥的入口——写权限一旦被越权或明文被回显，等于劫持后续所有使用者的凭据。
// 因此三条红线在这里逐条钉死：
//   1. **任何响应都不回明文**（含列表与写入响应，错误信息也只能带名字）；
//   2. 名字语法与 `${secrets.<NAME>}` 模板同源（cap.ValidSecretName），非法即 400；
//   3. 删除不存在 → 404（不静默成功）。
//
// 另有一条端到端：REST 写入的密钥，必须真的能被运行期 `${secrets.x}` 解析到——
// 否则「写进去了但用不了」这种断链只在生产才暴露。

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"gear/internal/model"
	"gear/internal/registry"
)

// secretNames 把 GET /api/secrets 的响应解成名字列表。
func secretNames(t *testing.T, body map[string]interface{}) []string {
	t.Helper()
	raw, ok := body["names"].([]interface{})
	if !ok {
		t.Fatalf("响应应含 names 数组，实际 %v", body)
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		out = append(out, fmt.Sprint(v))
	}
	return out
}

// TestSecretsManagementAPI：PUT → GET（只回名字）→ 覆盖 → DELETE → 404 全生命周期。
func TestSecretsManagementAPI(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.Handler()

	// 初始为空。
	rec, out := do(t, h, "GET", "/api/secrets", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("列表期望 200，实际 %d %s", rec.Code, rec.Body.String())
	}
	if names := secretNames(t, out); len(names) != 0 {
		t.Fatalf("初始应无密钥，实际 %v", names)
	}

	// 写入。
	rec, out = do(t, h, "PUT", "/api/secrets/API_KEY", map[string]interface{}{"value": "s3cr3t-value"})
	if rec.Code != http.StatusOK || out["ok"] != true || out["name"] != "API_KEY" {
		t.Fatalf("写入期望 200 + ok，实际 %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "s3cr3t-value") {
		t.Fatal("红线：写入响应不得回显明文")
	}

	// 列出：只有名字，没有值。
	rec, out = do(t, h, "GET", "/api/secrets", nil)
	if got := secretNames(t, out); len(got) != 1 || got[0] != "API_KEY" {
		t.Fatalf("列表应恰含 API_KEY，实际 %v", got)
	}
	if strings.Contains(rec.Body.String(), "s3cr3t-value") {
		t.Fatal("红线：列表响应不得回显明文")
	}

	// 覆盖：条数不变、值被换掉（用运行期取值验证，响应里看不到）。
	if rec, _ = do(t, h, "PUT", "/api/secrets/API_KEY", map[string]interface{}{"value": "rotated"}); rec.Code != http.StatusOK {
		t.Fatalf("覆盖写期望 200，实际 %d", rec.Code)
	}
	if _, out = do(t, h, "GET", "/api/secrets", nil); len(secretNames(t, out)) != 1 {
		t.Fatalf("覆盖不应新增条目: %v", out)
	}
	if v, err := s.secrets.Get("API_KEY"); err != nil || v != "rotated" {
		t.Fatalf("覆盖后应读到新值，实际 %q err=%v", v, err)
	}

	// 删除 → 200；再删 → 404（不静默成功）。
	if rec, _ = do(t, h, "DELETE", "/api/secrets/API_KEY", nil); rec.Code != http.StatusOK {
		t.Fatalf("删除期望 200，实际 %d %s", rec.Code, rec.Body.String())
	}
	if rec, _ = do(t, h, "DELETE", "/api/secrets/API_KEY", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("删除不存在的密钥期望 404，实际 %d", rec.Code)
	}
}

// TestSecretsManagementRejectsBadInput：所有「客户端可修」的错误都必须是 400，
// 且不得因非法输入写进半个脏数据。
func TestSecretsManagementRejectsBadInput(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.Handler()

	cases := []struct {
		name string
		req  func() *http.Request
	}{
		{"缺 value 字段", func() *http.Request {
			return httptest.NewRequest("PUT", "/api/secrets/NO_VALUE", strings.NewReader(`{}`))
		}},
		{"空 value", func() *http.Request {
			return httptest.NewRequest("PUT", "/api/secrets/EMPTY", strings.NewReader(`{"value":""}`))
		}},
		{"body 不是合法 JSON", func() *http.Request {
			return httptest.NewRequest("PUT", "/api/secrets/BAD_JSON", strings.NewReader(`{not json`))
		}},
		{"名字含模板语法字符", func() *http.Request {
			// `${}` 会被写进 `${secrets.<NAME>}` 造成解析歧义，故名字层就要拒。
			return httptest.NewRequest("PUT", "/api/secrets/a%7Bb", strings.NewReader(`{"value":"x"}`))
		}},
	}
	for _, c := range cases {
		req := c.req()
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s：期望 400，实际 %d %s", c.name, rec.Code, rec.Body.String())
		}
	}
	if n := s.secrets.Len(); n != 0 {
		t.Fatalf("非法请求不得写入任何密钥，实际写入 %d 条", n)
	}
}

// TestSecretsEndToEnd：REST 写入的密钥必须能被运行期 ${secrets.x} 与 ctx.secret.get 取到。
// 这条覆盖「管理面 → 引擎」的注入链（WithSecret → host.Secret → secretResolver），
// 断链时表现为「写进去了但解析报错」，只有端到端才能发现。
func TestSecretsEndToEnd(t *testing.T) {
	s, reg := newTestServer(t)
	h := s.Handler()

	if rec, _ := do(t, h, "PUT", "/api/secrets/SVC_TOKEN", map[string]interface{}{"value": "tok-123"}); rec.Code != http.StatusOK {
		t.Fatalf("写入密钥失败: %d %s", rec.Code, rec.Body.String())
	}

	// Tool 走 ctx.secret.get（运行期只读面，需声明 secret capability）。
	tool := &model.Tool{
		ID: "sec-read", Name: "sec-read", Runtime: model.RuntimeLua,
		Entry:        `return { v = ctx.secret.get("SVC_TOKEN") }`,
		Capabilities: []string{"secret"},
		InputSchema:  map[string]interface{}{"type": "object"},
		OutputSchema: map[string]interface{}{"type": "object"},
		Versions:     []model.ToolVersion{{Version: "1.0.0", State: model.StateStable}},
	}
	if err := reg.UpsertTool(tool); err != nil {
		t.Fatalf("注册 Tool 失败: %v", err)
	}
	// 同一个 Flow 里：step 1 直取密钥，step 2 经 ${secrets.svc_token} 模板引用另一条密钥。
	flow := &model.Flow{ID: "sec-flow", Steps: []model.Step{
		{ID: "get", Call: "sec-read", Input: map[string]interface{}{}},
		{ID: "tpl", Call: "sec-read", Input: map[string]interface{}{"echo": "${secrets.SVC_TOKEN}"}, DependsOn: []string{"get"}},
	}}
	if err := reg.UpsertFlow(flow); err != nil {
		t.Fatalf("注册含 ${secrets.*} 的 Flow 失败（REQ-TMPL-SECRETS 应在注册期接受该前缀）: %v", err)
	}

	rec, out := do(t, h, "POST", "/api/executions", map[string]interface{}{"flow_id": "sec-flow"})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("触发执行期望 202，实际 %d %s", rec.Code, rec.Body.String())
	}
	final := waitStatus(t, h, out["id"].(string), StatusSucceeded)
	result, _ := final["result"].(map[string]interface{})
	steps, _ := result["steps"].(map[string]interface{})
	getOut, _ := steps["get"].(map[string]interface{})
	inner, _ := getOut["output"].(map[string]interface{})
	if fmt.Sprint(inner["v"]) != "tok-123" {
		t.Fatalf("ctx.secret.get 应读到 REST 写入的明文，实际 %v", steps["get"])
	}
	tplOut, _ := steps["tpl"].(map[string]interface{})
	tplIn, _ := tplOut["input"].(map[string]interface{})
	if fmt.Sprint(tplIn["echo"]) != "tok-123" {
		t.Fatalf("${secrets.SVC_TOKEN} 应在运行期展开为明文，实际 %v", tplOut["input"])
	}
}

// TestSecretsSeedFromEnv：启动期 GEAR_SECRET_<NAME> 播种进密钥源（部署凭据来源）。
// 另锁「未注入 secret 实现时 ${secrets.x} 显式报错」——不静默替换为空串。
func TestSecretsSeedFromEnv(t *testing.T) {
	t.Setenv("GEAR_SECRET_SEEDED_KEY", "from-env")
	s, _ := newTestServer(t) // New() 内部完成 SeedFromEnv

	// 播种进来的密钥可直接经运行期面读到。
	if v, err := s.secrets.Get("SEEDED_KEY"); err != nil || v != "from-env" {
		t.Fatalf("环境变量未播种进密钥源: v=%q err=%v", v, err)
	}
	_, out := do(t, s.Handler(), "GET", "/api/secrets", nil)
	found := false
	for _, n := range secretNames(t, out) {
		if n == "SEEDED_KEY" {
			found = true
		}
	}
	if !found {
		t.Fatalf("管理面应列出播种的密钥，实际 %v", out)
	}
}

// TestSecretsNotInjectedFailsClosed：密钥源未初始化时三个端点都必须回 503（显式表明能力缺失），
// 而不是静默回空列表——「没配密钥源」与「密钥源里没有这个 key」必须可区分。
// 走 REST 不会出现该情形（New 必装配密钥源），故直接对 handler 注入 nil secrets 验证分支。
func TestSecretsNotInjectedFailsClosed(t *testing.T) {
	s := &Server{} // secrets == nil：模拟未注入
	cases := []struct {
		name   string
		method string
		path   string
		handle func(http.ResponseWriter, *http.Request)
	}{
		{"列表", "GET", "/api/secrets", s.handleListSecrets},
		{"写入", "PUT", "/api/secrets/K", s.handleSetSecret},
		{"删除", "DELETE", "/api/secrets/K", s.handleDeleteSecret},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		c.handle(rec, httptest.NewRequest(c.method, c.path, strings.NewReader("{}")))
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s：未初始化密钥源期望 503，实际 %d %s", c.name, rec.Code, rec.Body.String())
		}
	}
}

// TestServerRejectsIllegalSecretEnvName：GEAR_SECRET_ 下的非法名字必须让 New 直接失败。
// cap.SecretStore.SeedFromEnv 返回 error 正是为了让「配了却读不到」在**启动期**暴露；
// 调用方若 `_ =` 丢弃，运维看到的就是「环境变量明明设了，Tool 运行期却说密钥不存在」。
func TestServerRejectsIllegalSecretEnvName(t *testing.T) {
	t.Setenv("GEAR_SECRET_", "x") // 空前缀之后为空名：checkSecretName 显式拒绝
	reg, err := registry.Open(filepath.Join(t.TempDir(), "registry.db"))
	if err != nil {
		t.Fatalf("打开 registry 失败: %v", err)
	}
	defer reg.Close()
	if _, err := New(reg); err == nil {
		t.Fatal("非法的 GEAR_SECRET_* 变量名应导致启动失败（fail-closed），不得静默忽略")
	}
}
