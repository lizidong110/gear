// Package applog 把 detached `app run` 子进程里引擎产生的日志行**实时回推**给 gear serve。
//
// 为什么需要它：`gear app start` 派生的是独立进程，stdout/stderr 由 OS 直接重定向到
// <appsDir>/<appID>.log。管理端在另一个进程，若不回推，UI 想看日志只能去读那个文件——
// 正是用户明确拒绝的「从日志文件拉取」。
//
// 挂点选择：阶段 1 已让 process / service 运行时的 stdout/stderr 逐行经 engine.LogSink
// 上报，所以这里只需要给 `app run` 的 cliEngine 装一个 sink，把每行 POST 出去。
// 不读文件、不 tail、不回放历史，数据全程在内存里走。
//
// 红线（与 LogBroker 同一套）：
//  1. **绝不阻塞引擎**：Sink 只做一次非阻塞入队（有界 256），满即丢；
//     网络 POST 全在独立 goroutine 里批量做，失败静默降级（本地日志文件仍完整）。
//  2. **绝不外发**：URL 只接受回环 host，env 被人改坏时直接报错而不是悄悄发到外部。
//  3. Close 有界等待，不能让一条卡住的网络请求把 `app run` 的退出挂死。
package applog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"

	"gear/internal/app"
)

const (
	queueSize   = 256
	batchSize   = 32
	batchEvery  = 200 * time.Millisecond
	postTimeout = 2 * time.Second
	closeWait   = 3 * time.Second
)

// Line 是回推的一行日志（形状与 server.LiveLog 对齐，便于两端不必各写一套映射）。
type Line struct {
	StepID string `json:"step_id,omitempty"`
	Stream string `json:"stream"`
	Line   string `json:"line"`
	TS     int64  `json:"ts"`
}

type ingestBody struct {
	Token string `json:"token"`
	Lines []Line `json:"lines"`
}

// Reporter 是异步批量回推器。
type Reporter struct {
	url    string
	token  string
	appID  string
	client *http.Client

	ch     chan Line
	done   chan struct{}
	mu     sync.RWMutex
	closed bool
	once   sync.Once
}

// FromEnv 按环境变量决定是否启用。三者全空 = 未启用（返回 nil，行为零变化）；
// 只填了一部分 = 配置写坏了，直接报错（绝不静默降级成「以为开了其实没开」）。
func FromEnv() (*Reporter, error) {
	id := os.Getenv(app.EnvAppLogID)
	rawURL := os.Getenv(app.EnvAppLogURL)
	token := os.Getenv(app.EnvAppLogToken)
	if id == "" && rawURL == "" && token == "" {
		return nil, nil
	}
	if id == "" || rawURL == "" || token == "" {
		return nil, fmt.Errorf("实时日志回推配置不完整：%s / %s / %s 必须同时设置",
			app.EnvAppLogID, app.EnvAppLogURL, app.EnvAppLogToken)
	}
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Path == "" {
		return nil, fmt.Errorf("非法的回推 URL %q", rawURL)
	}
	if h := u.Hostname(); h != "127.0.0.1" && h != "localhost" && h != "::1" {
		return nil, fmt.Errorf("回推 URL 只允许回环地址，实际 %q", h)
	}
	r := &Reporter{
		url:    rawURL,
		token:  token,
		appID:  id,
		client: &http.Client{Timeout: postTimeout},
		ch:     make(chan Line, queueSize),
		done:   make(chan struct{}),
	}
	go r.run()
	return r, nil
}

// AppID 返回本进程对应的台账 app id（供调用方打印/断言）。
func (r *Reporter) AppID() string { return r.appID }

// Sink 满足 engine.LogSink 的签名。execID 是执行记录 id，在 App 场景下无消费者，刻意忽略。
func (r *Reporter) Sink(_execID, stepID, stream, line string) {
	if line == "" {
		return
	}
	// 与 Close 共用一把锁：Close 置 closed + 关 channel 时持写锁，这里持读锁，
	// 从根本上杜绝「向已关闭 channel 发送」panic（Supervisor 退出时仍有 goroutine 在打日志）。
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return
	}
	select {
	case r.ch <- Line{StepID: stepID, Stream: stream, Line: line, TS: time.Now().UnixNano()}:
	default:
		// 队列满：丢弃。排障日志丢几行不影响定位，反压引擎则会影响业务。
	}
}

func (r *Reporter) run() {
	defer close(r.done)
	tick := time.NewTicker(batchEvery)
	defer tick.Stop()
	batch := make([]Line, 0, batchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		r.post(batch)
		batch = batch[:0]
	}
	for {
		select {
		case l, ok := <-r.ch:
			if !ok {
				flush()
				return
			}
			batch = append(batch, l)
			if len(batch) >= batchSize {
				flush()
			}
		case <-tick.C:
			flush()
		}
	}
}

func (r *Reporter) post(lines []Line) {
	b, err := json.Marshal(ingestBody{Token: r.token, Lines: lines})
	if err != nil {
		return
	}
	req, err := http.NewRequest(http.MethodPost, r.url, bytes.NewReader(b))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		// 管理端没起 / 换了端口：静默降级。本地 <appID>.log 仍是完整的，
		// UI 侧会靠台账的 live_logs 与 SSE 空窗给出提示，不在这里刷错误日志。
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}

// Close 停止接收、flush 在途批次，并有界等待回推 goroutine 退出。
func (r *Reporter) Close() {
	r.once.Do(func() {
		r.mu.Lock()
		r.closed = true
		close(r.ch)
		r.mu.Unlock()
	})
	select {
	case <-r.done:
	case <-time.After(closeWait):
		// 网络卡住不让进程退出：剩下的日志已经在本地文件里，不值得为它挂着。
	}
}
