package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gear/internal/model"
	"gear/internal/registry"
	"gear/internal/server"
	"gopkg.in/yaml.v3"
)

// execTool / execFlow 是 exec 测试专用的「能真正执行成功」的 Tool/Flow：
// 工具 entry 用单引号包裹合法 JSON（避免 sh 吞掉双引号，见 version_test 的同类修复），
// 使 process runtime 的 stdout 末行能被解析为 JSON。
const execTool = `id: t1
name: json echo tool
runtime: process
entry: echo '{"ok":true}'
input_schema:
  type: object
output_schema:
  type: object
versions:
  - version: 1.0.0
    state: stable
`

const execFlow = `id: f1
steps:
  - id: s1
    call: t1
    when: "true"
`

// newTestServer 起一个带 t1 / f1 的 httptest 执行面，返回 server 与 host:port。
func newTestServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	// 每个测试用独立的执行记录库（GEAR_EXEC_DB），避免 #70 持久化后跨测试/跨 gate 运行
	// 残留历史执行导致 "空列表" 等断言误判（生产仍走默认 build/gear.db）。
	t.Setenv("GEAR_EXEC_DB", filepath.Join(t.TempDir(), "gear.db"))
	reg, err := registry.Open(tempDB(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reg.Close() })

	var tool model.Tool
	if err := yaml.Unmarshal([]byte(execTool), &tool); err != nil {
		t.Fatal(err)
	}
	if err := reg.UpsertTool(&tool); err != nil {
		t.Fatal(err)
	}
	var flow model.Flow
	if err := yaml.Unmarshal([]byte(execFlow), &flow); err != nil {
		t.Fatal(err)
	}
	if err := reg.UpsertFlow(&flow); err != nil {
		t.Fatal(err)
	}

	srv, err := server.New(reg)
	if err != nil {
		t.Fatalf("构造 Server 失败: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	// 先停 HTTP，再 Shutdown（后者会关闭 ExecStore 释放 SQLite 句柄，使 t.TempDir 清理成功）。
	t.Cleanup(ts.Close)
	t.Cleanup(srv.Shutdown)
	return ts, strings.TrimPrefix(ts.URL, "http://")
}

// getJSON 拉取 REST JSON 到 v。
func getJSON(t *testing.T, url string, v interface{}) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s → %d", url, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatal(err)
	}
}

// pollExecutionID 轮询执行面，找到 flowID 对应的执行 id（最多 ~3s）。
func pollExecutionID(t *testing.T, addr, flowID string) string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var list []struct {
			ID     string `json:"id"`
			FlowID string `json:"flow_id"`
		}
		getJSON(t, "http://"+addr+"/api/executions", &list)
		for _, e := range list {
			if e.FlowID == flowID {
				return e.ID
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return ""
}

// TestExecStartStatusList 覆盖「触发 → 轮询到成功 → status 查到 → list 列出」闭环。
func TestExecStartStatusList(t *testing.T) {
	_, addr := newTestServer(t)

	if err := runCLI(t, "exec", "start", "f1", "--addr", addr); err != nil {
		t.Fatalf("exec start: %v", err)
	}
	id := pollExecutionID(t, addr, "f1")
	if id == "" {
		t.Fatal("未找到执行记录")
	}

	// 轮询到终态
	status := ""
	for i := 0; i < 100; i++ {
		var rec struct {
			Status string `json:"status"`
		}
		getJSON(t, "http://"+addr+"/api/executions/"+id, &rec)
		status = rec.Status
		if status == "succeeded" || status == "failed" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if status != "succeeded" {
		t.Fatalf("执行应成功，实为 %s", status)
	}

	// exec status <id> 输出应含该 id
	var buf bytes.Buffer
	root := newRootCmd()
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"exec", "status", id, "--addr", addr})
	if err := root.Execute(); err != nil {
		t.Fatalf("exec status: %v", err)
	}
	if !strings.Contains(buf.String(), id) {
		t.Fatalf("exec status 输出应含 id: %s", buf.String())
	}

	// exec list 输出应含 f1
	buf.Reset()
	root = newRootCmd()
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"exec", "list", "--addr", addr})
	if err := root.Execute(); err != nil {
		t.Fatalf("exec list: %v", err)
	}
	if !strings.Contains(buf.String(), "f1") {
		t.Fatalf("exec list 输出应含 f1: %s", buf.String())
	}
}

// TestExecStartUnknownFlow 覆盖「触发不存在的 Flow → 报错（非 202）」。
func TestExecStartUnknownFlow(t *testing.T) {
	_, addr := newTestServer(t)
	if err := runCLI(t, "exec", "start", "ghost", "--addr", addr); err == nil {
		t.Fatal("触发不存在的 Flow 应报错")
	}
}

// TestExecListEmpty 覆盖「无执行记录时打印提示」分支。
func TestExecListEmpty(t *testing.T) {
	_, addr := newTestServer(t)
	var buf bytes.Buffer
	root := newRootCmd()
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"exec", "list", "--addr", addr})
	if err := root.Execute(); err != nil {
		t.Fatalf("exec list: %v", err)
	}
	if !strings.Contains(buf.String(), "（无执行记录）") {
		t.Fatalf("空列表应提示：%s", buf.String())
	}
}
