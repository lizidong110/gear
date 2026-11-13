package applog

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"gear/internal/app"
)

func clearLogEnv(t *testing.T) {
	t.Helper()
	t.Setenv(app.EnvAppLogID, "")
	t.Setenv(app.EnvAppLogURL, "")
	t.Setenv(app.EnvAppLogToken, "")
}

func TestFromEnvDisabledAndIncomplete(t *testing.T) {
	clearLogEnv(t)
	r, err := FromEnv()
	if err != nil || r != nil {
		t.Fatalf("三者全空应视为未启用，实际 r=%v err=%v", r, err)
	}
	// 只填一半 = 配置写坏，必须报错（不能静默降级成「以为开了其实没开」）。
	t.Setenv(app.EnvAppLogID, "app1")
	if _, err := FromEnv(); err == nil {
		t.Fatal("环境变量不完整应报错")
	}
}

func TestFromEnvRejectsRemoteURL(t *testing.T) {
	clearLogEnv(t)
	t.Setenv(app.EnvAppLogID, "app1")
	t.Setenv(app.EnvAppLogToken, "tok")
	t.Setenv(app.EnvAppLogURL, "https://evil.example.com/api/apps/app1/logs")
	if _, err := FromEnv(); err == nil {
		t.Fatal("非回环回推地址必须拒绝（日志里可能有业务数据）")
	}
	t.Setenv(app.EnvAppLogURL, "not-a-url")
	if _, err := FromEnv(); err == nil {
		t.Fatal("非法 URL 必须拒绝")
	}
}

// TestReporterPostsBatches：Sink 的行经批量 POST 到达 ingest 端点；Close 负责 flush。
func TestReporterPostsBatches(t *testing.T) {
	var mu sync.Mutex
	var batches []struct {
		Token string `json:"token"`
		Lines []Line `json:"lines"`
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("应为 POST，实际 %s", r.Method)
		}
		var b struct {
			Token string `json:"token"`
			Lines []Line `json:"lines"`
		}
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
			t.Errorf("解析回推体失败: %v", err)
		}
		mu.Lock()
		batches = append(batches, b)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	clearLogEnv(t)
	t.Setenv(app.EnvAppLogID, "app1")
	t.Setenv(app.EnvAppLogToken, "secret-token")
	t.Setenv(app.EnvAppLogURL, ts.URL+"/api/apps/app1/logs")
	r, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv 失败: %v", err)
	}
	if r.AppID() != "app1" {
		t.Fatalf("AppID 不对: %q", r.AppID())
	}
	r.Sink("exec-1", "s1", "stdout", "line-A")
	r.Sink("exec-1", "s1", "stderr", "line-B")
	r.Close() // 必须在断言前 flush 在途批次

	mu.Lock()
	defer mu.Unlock()
	seen := map[string]bool{}
	for _, b := range batches {
		if b.Token != "secret-token" {
			t.Fatalf("token 不对: %q", b.Token)
		}
		for _, l := range b.Lines {
			seen[l.Line] = true
			if l.Stream == "" || l.TS == 0 || l.StepID != "s1" {
				t.Fatalf("日志行字段丢失: %+v", l)
			}
		}
	}
	if !seen["line-A"] || !seen["line-B"] {
		t.Fatalf("两行都应送达，实际 %v / batches=%+v", seen, batches)
	}
}

// TestReporterSinkNeverBlocksAndCloseSafe：队列满即丢，绝不反压引擎；Close 幂等，
// 且 Close 之后仍被 Supervisor 的收尾 goroutine 调 Sink 也不能 panic。
func TestReporterSinkNeverBlocksAndCloseSafe(t *testing.T) {
	r := &Reporter{
		client: &http.Client{},
		ch:     make(chan Line, 1),
		done:   make(chan struct{}),
	}
	// 故意不启动 run()：channel 填满后若 Sink 改成阻塞发送，这里立刻挂住。
	start := time.Now()
	for i := 0; i < 1000; i++ {
		r.Sink("", "s1", "stdout", "x")
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Fatalf("Sink 阻塞了引擎路径: %v", d)
	}
	close(r.done) // 让 Close 不必等满 3s
	r.Close()
	r.Close()
	r.Sink("", "s1", "stdout", "after-close") // 不应 panic
}

// TestReporterSilentlyDegradesOnDeadEndpoint：管理端没起时 Close 不能把进程挂死，
// 本地日志仍完整（这里只验「不阻塞、不 panic」）。
func TestReporterSilentlyDegradesOnDeadEndpoint(t *testing.T) {
	clearLogEnv(t)
	t.Setenv(app.EnvAppLogID, "app1")
	t.Setenv(app.EnvAppLogToken, "tok")
	t.Setenv(app.EnvAppLogURL, "http://127.0.0.1:1/api/apps/app1/logs")
	r, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	r.Sink("", "s1", "stdout", "line")
	start := time.Now()
	r.Close()
	if d := time.Since(start); d > closeWait+time.Second {
		t.Fatalf("管理端不可达时 Close 挂太久: %v", d)
	}
}
