package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"gear/internal/model"
	"gear/internal/registry"
	"gopkg.in/yaml.v3"
)

// gateToolYAML / gateFlowYAML 是并发闸门测试专用的「能真正执行成功」的 Tool/Flow
// （与 cli/exec_test.go 的 execTool/execFlow 同构：process runtime echo 合法 JSON）。
const gateToolYAML = `id: t_gate
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

const gateFlowYAML = `id: f_gate
steps:
  - id: s1
    call: t_gate
    when: "true"
`

// newGateServer 起一个带 t_gate / f_gate 的 httptest 执行面，支持注入并发闸门选项。
func newGateServer(t *testing.T, opts ...ServerOption) (*Server, *httptest.Server) {
	t.Helper()
	t.Setenv("GEAR_EXEC_DB", filepath.Join(t.TempDir(), "gear.db"))
	reg, err := registry.Open(filepath.Join(t.TempDir(), "registry.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reg.Close() })

	var tool model.Tool
	if err := yaml.Unmarshal([]byte(gateToolYAML), &tool); err != nil {
		t.Fatal(err)
	}
	if err := reg.UpsertTool(&tool); err != nil {
		t.Fatal(err)
	}
	var flow model.Flow
	if err := yaml.Unmarshal([]byte(gateFlowYAML), &flow); err != nil {
		t.Fatal(err)
	}
	if err := reg.UpsertFlow(&flow); err != nil {
		t.Fatal(err)
	}

	srv, err := New(reg, opts...)
	if err != nil {
		t.Fatalf("构造 Server 失败: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	t.Cleanup(srv.Shutdown)
	return srv, ts
}

// TestHandleCreateExecutionRejectedWhenGateFull 覆盖 REQ-CONCURRENCY-CROSS 的背压拒绝分支：
// 入口并发达上限且队列已满时，新提交返回 429、执行记录置 rejected（不真正运行）。
func TestHandleCreateExecutionRejectedWhenGateFull(t *testing.T) {
	srv, ts := newGateServer(t, WithMaxConcurrent(1), WithExecQueueSize(0))

	// 占住唯一 worker，模拟一个长任务正在跑（闸门满），窗口期约 500ms。
	occupied := make(chan struct{})
	srv.gate.Submit(func() { close(occupied); time.Sleep(500 * time.Millisecond) })
	select {
	case <-occupied:
	case <-time.After(time.Second):
		t.Fatal("gate worker 未进入占用任务")
	}

	const N = 8
	var wg sync.WaitGroup
	codes := make([]int, N)
	ids := make([]string, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body, _ := json.Marshal(map[string]string{"flow_id": "f_gate"})
			resp, err := http.Post(ts.URL+"/api/executions", "application/json", bytes.NewReader(body))
			if err != nil {
				t.Errorf("POST %d: %v", i, err)
				return
			}
			defer resp.Body.Close()
			codes[i] = resp.StatusCode
			var r struct {
				ID     string `json:"id"`
				Status string `json:"status"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&r)
			ids[i] = r.ID
		}(i)
	}
	wg.Wait()

	accepted, rejected := 0, 0
	for _, c := range codes {
		switch c {
		case http.StatusAccepted:
			accepted++
		case http.StatusTooManyRequests:
			rejected++
		default:
			t.Errorf("意外的状态码 %d", c)
		}
	}
	if accepted != 0 {
		t.Fatalf("闸门满时不应有请求被接受, 实得 %d", accepted)
	}
	if rejected != N {
		t.Fatalf("期望 %d 个 429, 实得 %d", N, rejected)
	}
	// 被拒的执行记录应标记为 rejected（供查询可见，区别于 running/succeeded/failed）。
	for _, id := range ids {
		if id == "" {
			continue
		}
		rec, ok := srv.execs.Get(id)
		if !ok {
			t.Fatalf("被拒执行 %s 无记录", id)
		}
		if rec.Status != StatusRejected {
			t.Fatalf("被拒执行 %s 状态应为 rejected, 实为 %s", id, rec.Status)
		}
	}
}

// TestHandleCreateExecutionAcceptedUnderCapacity 覆盖正常路径：闸门有空位时返回 202，
// 且执行最终收敛为 succeeded（验证派发链路未被闸门破坏）。
func TestHandleCreateExecutionAcceptedUnderCapacity(t *testing.T) {
	_, ts := newGateServer(t) // 默认闸门（64 worker / 256 队列），单发必然有空位

	body, _ := json.Marshal(map[string]string{"flow_id": "f_gate"})
	resp, err := http.Post(ts.URL+"/api/executions", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("期望 202, 实得 %d", resp.StatusCode)
	}
	var r struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		t.Fatal(err)
	}
	if r.ID == "" {
		t.Fatal("应返回 execution id")
	}

	// 轮询到终态
	deadline := time.Now().Add(3 * time.Second)
	status := ""
	for time.Now().Before(deadline) {
		resp2, err := http.Get(ts.URL + "/api/executions/" + r.ID)
		if err != nil {
			t.Fatal(err)
		}
		var rec struct {
			Status string `json:"status"`
		}
		_ = json.NewDecoder(resp2.Body).Decode(&rec)
		resp2.Body.Close()
		status = rec.Status
		if status == "succeeded" || status == "failed" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if status != "succeeded" {
		t.Fatalf("执行应成功, 实为 %s", status)
	}
}
