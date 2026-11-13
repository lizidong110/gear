package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// discovery_test.go：锁住 serve 地址发现的「写→读→注入」链路与降级判据。
// 这些断言盯的是**静默失效**：文件写坏 / serve 已死 / 地址被改成外发，
// 都不能让 App 以为自己接上了实时日志（台账 live_logs 会跟着撒谎）。

func TestPublishReadServerEndpoint(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "apps")
	token, err := PublishServerEndpoint(dir, "http://127.0.0.1:8787/")
	if err != nil {
		t.Fatalf("写发现文件失败: %v", err)
	}
	if len(token) < 16 {
		t.Fatalf("token 太短: %q", token)
	}
	ep, err := ReadServerEndpoint(dir)
	if err != nil {
		t.Fatalf("读发现文件失败: %v", err)
	}
	if ep.Addr != "http://127.0.0.1:8787" || ep.Token != token || ep.PID != os.Getpid() {
		t.Fatalf("发现文件内容不对: %+v", ep)
	}
	// 发现文件权限必须收紧：里面是回推凭据。
	// Windows 不实现 POSIX 权限位（WriteFile(0600) 读回常是 0666），只在本类平台能断言；
	// 不能让「换台机器跑测试就红」掩盖真正的逻辑回归。
	if runtime.GOOS != "windows" {
		info, err := os.Stat(ServerEndpointPath(dir))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0o077 != 0 {
			t.Fatalf("发现文件不应同组/其他用户可读: %v", info.Mode().Perm())
		}
	}

	// serve 换端口重启：token 复用，已运行的旧 App 仍持合法凭据。
	token2, err := PublishServerEndpoint(dir, "http://127.0.0.1:9999")
	if err != nil {
		t.Fatal(err)
	}
	if token2 != token {
		t.Fatalf("重启不应换 token（会让所有旧 App 实时日志失效）: %q → %q", token, token2)
	}
	ep2, _ := ReadServerEndpoint(dir)
	if ep2.Addr != "http://127.0.0.1:9999" {
		t.Fatalf("地址未更新: %+v", ep2)
	}
}

func TestServerEndpointRejectsRemoteHost(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "apps")
	if _, err := PublishServerEndpoint(dir, "http://0.0.0.0:8787"); err == nil {
		t.Fatal("非回环地址应被拒（否则日志会被发到外部）")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// 手工篡改发现文件也要挡住：读侧不能只信写侧。
	bad := ServerEndpoint{Addr: "https://evil.example.com", Token: "x", PID: os.Getpid(), At: time.Now()}
	b, _ := json.Marshal(bad)
	if err := os.WriteFile(ServerEndpointPath(dir), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadServerEndpoint(dir); err == nil {
		t.Fatal("被篡改的外部地址应判为不可用")
	}
	if env := LogIngestEnv(dir, "app1"); env != nil {
		t.Fatalf("发现文件不可用时不应注入 env: %v", env)
	}
}

func TestLogIngestEnv(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "apps")
	if env := LogIngestEnv(dir, "app1"); env != nil {
		t.Fatalf("无发现文件时应返回 nil: %v", env)
	}
	if _, err := PublishServerEndpoint(dir, "http://127.0.0.1:8787"); err != nil {
		t.Fatal(err)
	}
	env := LogIngestEnv(dir, "app-1")
	if len(env) != 3 {
		t.Fatalf("应注入 3 个环境变量，实际 %v", env)
	}
	joined := strings.Join(env, "\n")
	for _, want := range []string{
		EnvAppLogID + "=app-1",
		EnvAppLogURL + "=http://127.0.0.1:8787/api/apps/app-1/logs",
		EnvAppLogToken + "=",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("env 缺少 %q: %v", want, env)
		}
	}

	// 写它的 serve 进程已经不在 → 地址过期，不注入（台账 live_logs 才不会撒谎）。
	ep, _ := ReadServerEndpoint(dir)
	ep.PID = 2_147_483_647
	b, _ := json.Marshal(ep)
	if err := os.WriteFile(ServerEndpointPath(dir), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if env := LogIngestEnv(dir, "app-1"); env != nil {
		t.Fatalf("serve 已死仍注入回推 env: %v", env)
	}
}

func TestServerEndpointNotListedAsRecord(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "apps")
	if _, err := PublishServerEndpoint(dir, "http://127.0.0.1:8787"); err != nil {
		t.Fatal(err)
	}
	recs, err := ListRecords(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 0 {
		t.Fatalf("server.addr 不该被当成应用台账: %+v", recs)
	}
}
