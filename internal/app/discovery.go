package app

// discovery.go —— serve 地址发现 + detached App 实时日志回推凭据（REQ-API-APP-LOGS）。
//
// 问题：`gear app start` 派生的是**另一个进程**，它不知道 `gear serve` 监听在哪个地址
// （尤其 `--addr :0` 时端口由内核分配）。没有地址，子进程就没法把日志实时回推给管理端，
// UI 只能去读 <appID>.log —— 这正是用户明确拒绝的「从日志文件拉取」。
//
// 解法：serve 绑定成功后把 `{addr, token, pid}` 原子写进 <appsDir>/server.addr；
// app.Start 派生前读它，把「回推 URL + token + app id」以环境变量注入子进程。
// 子进程只在内存里逐行 POST，UI 侧订阅 SSE —— 两端都不碰日志文件。
//
// 红线：
//  1. **不静默假装可用**：读不到 / 地址过期（pid 已不在）→ 不注入 env，台账
//     `live_logs=false`，UI 据此明说「这个应用未接入实时日志」。纯本地日志仍完整。
//  2. **只允许回环地址**：发现文件是可写的本地文件，若不校验 host，被人改成
//     外部 URL 就会让每个 App 把运行日志 POST 出去（日志里可能有业务数据）。
//  3. token 复用：serve 重启（换端口）时不换 token，已运行的旧 App 仍持有合法凭据，
//     不至于「重启一下管理端，所有应用的实时日志全哑了」。

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ServerEndpointFile 是发现文件名。刻意不叫 .json —— `ListRecords` 只认 .json，
// 否则这份文件会被当成一条应用台账列出来。
const ServerEndpointFile = "server.addr"

// 注入给 `app run` 子进程的环境变量（launcher → 子进程；applog 包消费）。
const (
	EnvAppLogID    = "GEAR_APP_LOG_ID"
	EnvAppLogURL   = "GEAR_APP_LOG_URL"
	EnvAppLogToken = "GEAR_APP_LOG_TOKEN"
)

// ServerEndpoint 是 serve 暴露给 detached App 的回推入口。
type ServerEndpoint struct {
	Addr  string    `json:"addr"` // http://127.0.0.1:8787（不含尾斜杠）
	Token string    `json:"token"`
	PID   int       `json:"pid"`
	At    time.Time `json:"at"`
}

// ServerEndpointPath 返回发现文件的绝对路径（供测试与排障直接定位）。
func ServerEndpointPath(appsDir string) string {
	return filepath.Join(appsDir, ServerEndpointFile)
}

// NewLogToken 生成 128bit 随机 token（十六进制）。
func NewLogToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("生成实时日志 token 失败: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// PublishServerEndpoint 在 serve 绑定成功后写发现文件，返回生效的 token。
//
// 原子写（tmp + rename）：app.Start 可能在 serve 写文件的同一瞬间读，半截 JSON 会被当成
// 「没启用」而静默丢掉实时日志。复用旧 token 的理由见文件头红线 3。
func PublishServerEndpoint(appsDir, baseURL string) (string, error) {
	if strings.TrimSpace(appsDir) == "" {
		return "", fmt.Errorf("应用台账目录为空，无法写入 server.addr")
	}
	if err := os.MkdirAll(appsDir, 0o755); err != nil {
		return "", err
	}
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if _, err := validateEndpointAddr(baseURL); err != nil {
		return "", err
	}
	token := ""
	if old, err := ReadServerEndpoint(appsDir); err == nil && old.Token != "" {
		token = old.Token
	}
	if token == "" {
		t, err := NewLogToken()
		if err != nil {
			return "", err
		}
		token = t
	}
	ep := ServerEndpoint{Addr: baseURL, Token: token, PID: os.Getpid(), At: time.Now()}
	b, err := json.MarshalIndent(ep, "", "  ")
	if err != nil {
		return "", err
	}
	path := ServerEndpointPath(appsDir)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return token, nil
}

// ReadServerEndpoint 读取并校验发现文件。坏文件 / 非回环地址一律视为不可用。
func ReadServerEndpoint(appsDir string) (*ServerEndpoint, error) {
	b, err := os.ReadFile(ServerEndpointPath(appsDir))
	if err != nil {
		return nil, err
	}
	var ep ServerEndpoint
	if err := json.Unmarshal(b, &ep); err != nil {
		return nil, fmt.Errorf("server.addr 解析失败: %w", err)
	}
	if _, err := validateEndpointAddr(ep.Addr); err != nil {
		return nil, err
	}
	if ep.Token == "" {
		return nil, fmt.Errorf("server.addr 缺少 token")
	}
	return &ep, nil
}

// validateEndpointAddr 只允许 http(s) + 回环 host（防止发现文件被改成外发地址）。
func validateEndpointAddr(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("非法的回推地址 %q（应为 http(s)://host:port）", raw)
	}
	h := u.Hostname()
	if h != "127.0.0.1" && h != "localhost" && h != "::1" {
		return nil, fmt.Errorf("回推地址只允许回环（127.0.0.1 / localhost / ::1），实际 %q", h)
	}
	return u, nil
}

// RemoveServerEndpoint 供测试/显式清理。生产停机**不调用**：保留 token 供下次 serve 复用，
// 由 pid 存活检查负责把过期地址判成「不可用」。
func RemoveServerEndpoint(appsDir string) error {
	err := os.Remove(ServerEndpointPath(appsDir))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// LogIngestEnv 生成要注入派生进程的环境变量；没启用实时日志时返回 nil。
//
// 纯本地降级判据：文件不存在 / 地址非法 / **写它的 serve 进程已不在**（pid 死了）。
// 最后一条很关键：serve 崩了却留着一份旧地址，新起的 App 会一直往死端口 POST，
// 台账却标着「已接入实时日志」，UI 上就是一片空白还查不出原因。
func LogIngestEnv(appsDir, appID string) []string {
	ep, err := ReadServerEndpoint(appsDir)
	if err != nil {
		return nil
	}
	if ep.PID > 0 && !processAlive(ep.PID) {
		return nil
	}
	return []string{
		EnvAppLogID + "=" + appID,
		EnvAppLogURL + "=" + ep.Addr + "/api/apps/" + url.PathEscape(appID) + "/logs",
		EnvAppLogToken + "=" + ep.Token,
	}
}
