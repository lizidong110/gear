package engine

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"gear/internal/cap"
	"gear/internal/cmdline"
	"gear/internal/model"
)

// defaultExecTimeout 是单次 process 执行的默认超时（F4：防止挂死工具占住执行线程）。
// 设为变量便于测试注入更短/更长时限。
//
// 可被 **Tool 级声明**覆盖：Tool 写了 timeout 就用它（见 model.Tool.ExecTimeout 与
// execTimeoutFor）。默认值本身保持不变，因此「没声明的 Tool」依旧受 30s 防挂死保护。
var defaultExecTimeout = 30 * time.Second

// execTimeoutFor 返回某次 Tool 执行的生效超时：优先用 Tool 声明的 timeout，
// 未声明（或声明非法，注册期已拦下，此处仅防御性回退）则用引擎默认。
func execTimeoutFor(tool *model.Tool) time.Duration {
	if tool == nil {
		return defaultExecTimeout
	}
	d, err := tool.ExecTimeout()
	if err != nil || d <= 0 {
		return defaultExecTimeout
	}
	return d
}

// processRuntime 实现 Runtime 接口：外部进程，stdin 进 JSON，stdout 末行出 JSON（PRD §3.3）。
type processRuntime struct {
	// execID/stepID 用于把子进程日志关联到具体执行（REQ-API-LOGS / #71）；
	// sink 为 nil 时不记日志（兼容无日志采集的调用方）。
	execID string
	stepID string
	sink   func(stepID, stream, line string)
}

func (p *processRuntime) Run(ctx context.Context, entry string, input map[string]interface{}, host *cap.Context) (map[string]interface{}, error) {
	return runProcess(ctx, entry, input, host, p.execID, p.stepID, p.sink)
}

// Start 拉起一个长驻 process（service 模式），不等待其结束，返回可监管句柄。
func (p *processRuntime) Start(ctx context.Context, entry string, input map[string]interface{}, host *cap.Context) (Process, error) {
	sp, err := startProcess(ctx, entry, input, host)
	if err != nil {
		return nil, err
	}
	return &execProcess{
		cmd:     sp.cmd,
		kill:    func() error { return killProcessTree(sp.cmd) },
		tail:    sp.tail,
		drained: sp.drained,
	}, nil
}

// execProcess 把 *exec.Cmd 适配成 engine.Process 抽象（避免上层依赖 os/exec）。
// kill 是平台化的「级联回收」函数（见 process_unix.go / process_windows.go）：
// 不仅杀包装器本体，还要一并杀掉其孙进程（如 `sleep`），否则孤儿会脱离监管
// （红线 16 的延伸）。注意包装器**可能是 shell**（entry 含元字符时），也可能就是目标
// 进程本身（原生路径），两种形态都由 killProcessTree 统一处理。
type execProcess struct {
	cmd  *exec.Cmd
	kill func() error
	// tail/drained 承载子进程输出尾部与「抽干完成」信号（见 serviceTail），
	// 供 ExitInfo 在进程退出后回答「为什么退出」。
	tail    *serviceTail
	drained chan struct{}

	mu      sync.Mutex
	waitErr error
	waited  bool
}

func (e *execProcess) Pid() int { return e.cmd.Process.Pid }

func (e *execProcess) Kill() error { return e.kill() }

// Wait 等待子进程结束，并**记住**退出结果：ExitInfo 依赖它把退出码报给监管侧。
func (e *execProcess) Wait() error {
	err := e.cmd.Wait()
	e.mu.Lock()
	e.waitErr, e.waited = err, true
	e.mu.Unlock()
	return err
}

// ExitInfo 实现 ExitReporter：返回子进程退出码与输出尾部（service 诊断用）。
//
// 已 Wait 过时先**有界**等待抽干 goroutine 收尾（serviceDrainTimeout），否则可能丢掉
// 进程临死前最后几行——而那几行通常正是失败原因。
func (e *execProcess) ExitInfo() (int, []string) {
	e.mu.Lock()
	waited, waitErr := e.waited, e.waitErr
	e.mu.Unlock()

	if waited && e.drained != nil {
		select {
		case <-e.drained:
		case <-time.After(serviceDrainTimeout):
		}
	}

	// 退出码：正常退出为 0；带退出码的失败取真实码；其余（被信号杀、Wait 重复调用、
	// I/O 错误等）报 -1 —— 绝不伪装成「正常退出」。
	code := 0
	if waitErr != nil {
		var ee *exec.ExitError
		if errors.As(waitErr, &ee) {
			code = ee.ExitCode()
		} else {
			code = -1
		}
	}
	if e.tail == nil {
		return code, nil
	}
	return code, e.tail.snapshot()
}

// runProcess 执行 process runtime 的 Tool：把 input 序列化为 JSON 经 stdin 传入，
// 读取 stdout 的最后一行非空内容作为 JSON 输出（见 PRD §3.3 的 process 契约）。
//
// 命令解析交给 cmdline（**默认不经 shell**）：entry 里的 `NAME=value` 前缀与引号由 gear
// 自己解析成 argv + 环境变量。此前写死 `sh -c <entry>`，而 Windows 系统 PATH 里通常没有
// sh（Git 只把 git.exe 放进 `Git\cmd`）⇒ GUI 宿主派生的 gear 拉不起任何 process 型 Tool
// ⇒ 服务侧车全起不来（2026-09-21 实测）。仅当 entry 含未加引号的 shell 元字符
// （管道 / 重定向 / && / 通配 / 变量展开）时才回落到 shell，且找不到 shell 即 fail-closed。
// ctx 用于超时控制：超时时命令被杀死并返回错误（F4）。
func runProcess(ctx context.Context, entry string, input map[string]interface{}, host *cap.Context, execID, stepID string, sink func(stepID, stream, line string)) (map[string]interface{}, error) {
	input = withHostCapabilities(input, host)
	inBytes, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("序列化 input 失败: %w", err)
	}

	cmd, err := cmdline.Command(ctx, entry)
	if err != nil {
		return nil, procFail(err.Error(), entry, nil, nil)
	}
	cmd.Stdin = bytes.NewReader(inBytes)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()
	// 无论成功失败，都把子进程每行输出上报给日志采集器（REQ-API-LOGS / #71）。
	// 注意：仅 stdout/stderr 文本，绝不包含 input（secret 等敏感数据不落日志）。
	if sink != nil {
		emitLogLines(sink, stepID, "stdout", stdout.String())
		emitLogLines(sink, stepID, "stderr", stderr.String())
	}
	if runErr != nil {
		return nil, procFail(fmt.Sprintf("%v (stderr=%s)", runErr, stderr.String()), entry, stdout.Bytes(), stderr.Bytes())
	}

	last := lastNonEmptyLine(stdout.String())
	if last == "" {
		return nil, procFail("stdout 为空", entry, stdout.Bytes(), stderr.Bytes())
	}

	var out map[string]interface{}
	if err := json.Unmarshal([]byte(last), &out); err != nil {
		return nil, procFail(fmt.Sprintf("解析 stdout 失败(%q): %v", last, err), entry, stdout.Bytes(), stderr.Bytes())
	}
	return out, nil
}

// emitLogLines 把一段输出按行上报给 sink（空行跳过）；stream 标记 stdout/stderr。
func emitLogLines(sink func(stepID, stream, line string), stepID, stream, buf string) {
	for _, ln := range strings.Split(buf, "\n") {
		ln = strings.TrimRight(ln, "\r")
		if strings.TrimSpace(ln) == "" {
			continue
		}
		sink(stepID, stream, ln)
	}
}

func lastNonEmptyLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			return lines[i]
		}
	}
	return ""
}

// withHostCapabilities 把本次执行的宿主能力视图注入到 step input 的 ctx 子对象，
// 供外部进程在运行期做最小权限自检（M5：进程侧实时强制留待后续切片，但声明必须先透传）。
// 仅当 host 非空时注入；已有 ctx 键则在其上合并，不破坏既有字段。
func withHostCapabilities(input map[string]interface{}, host *cap.Context) map[string]interface{} {
	if host == nil {
		return input
	}
	ctxObj, _ := input["ctx"].(map[string]interface{})
	if ctxObj == nil {
		ctxObj = map[string]interface{}{}
	}
	ctxObj["capabilities"] = host.Capabilities()
	ctxObj["sandbox"] = host.Sandbox()
	input["ctx"] = ctxObj
	return input
}

const (
	// serviceTailLines 是每个 service 子进程保留的输出行数上限（诊断用，内存恒定）。
	serviceTailLines = 50
	// serviceDrainTimeout 是 ExitInfo 等待「最后几行落进缓冲」的上限。
	// 必须有界：若孙进程仍持有写端（级联回收完成前的窗口），读到 EOF 可能永远等不到。
	serviceDrainTimeout = 300 * time.Millisecond
)

// serviceTail 是 service 子进程输出的内存环形缓冲（只留尾部 serviceTailLines 行）。
//
// 为什么需要它 —— 红线 16 的第二次修正：service 子进程**不能**继承调用方的
// os.Stdout/os.Stderr（孙进程成为孤儿后会长期占住该管道，令 `go test` 报
// "Test I/O incomplete"，故早期实现把服务输出直接丢进 os.DevNull）；
// 但「丢进 DevNull」把**进程为什么退出**也一并丢了：2026-09-21 实测 `:8787` 被占
// 导致 `gear serve` 秒退，监管侧只有「↻ 重启 第 N 次」，原因完全不可见。
//
// 解法：父子之间**自开一根管道**（不碰调用方 fd），父侧后台 goroutine 持续抽干并存入
// 本缓冲；进程退出时由 ExitInfo 取尾部交给监管打印。孤儿孙进程最多让这根自有管道晚一点
// EOF（抽干 goroutine 多活一会儿），不会污染调用方管道，也不会阻塞任何事。
type serviceTail struct {
	mu    sync.Mutex
	lines []string
}

// push 追加一行；超出上限则丢弃最旧的行（内存恒定，崩溃循环下也不会无限增长）。
func (t *serviceTail) push(line string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lines = append(t.lines, line)
	if len(t.lines) > serviceTailLines {
		t.lines = append([]string(nil), t.lines[len(t.lines)-serviceTailLines:]...)
	}
}

// snapshot 返回当前保留行的副本（调用方不得依赖其与后续 push 的一致性）。
func (t *serviceTail) snapshot() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]string, len(t.lines))
	copy(out, t.lines)
	return out
}

// drainServiceTail 把自有管道按行抽干进缓冲，直到 EOF（写端全关时触发）后关闭读端。
func drainServiceTail(rc io.ReadCloser, t *serviceTail) {
	defer rc.Close()
	sc := bufio.NewScanner(rc)
	// 单行上限提到 1MB：默认 64KB 会让超长行提前终止抽取，连带丢掉其后（可能含失败原因）的行。
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		t.push(sc.Text())
	}
}

// startedProcess 是 startProcess 的产物：命令本体 + 输出缓冲 + 抽干完成信号。
type startedProcess struct {
	cmd     *exec.Cmd
	tail    *serviceTail
	drained chan struct{}
}

// startProcess 拉起一个 process 子进程但**不等待其结束**（service 模式用，见 PRD §14.2）。
// input 经 stdin 传入；stdout/stderr 汇入**自有管道**（见 serviceTail：既不继承调用方
// 管道，也不丢进 DevNull），由父侧 goroutine 抽干进内存缓冲，供监管诊断退出原因。
// 返回的 startedProcess 由调用方负责在监管结束时 Kill（Supervisor.stopAll）。
func startProcess(ctx context.Context, entry string, input map[string]interface{}, host *cap.Context) (*startedProcess, error) {
	input = withHostCapabilities(input, host)
	inBytes, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("序列化 input 失败: %w", err)
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("创建 service 输出管道失败: %v", err)
	}
	cmd, err := cmdline.Command(ctx, entry)
	if err != nil {
		_ = pr.Close()
		_ = pw.Close()
		return nil, err
	}
	cmd.Stdin = bytes.NewReader(inBytes)
	cmd.Stdout = pw
	cmd.Stderr = pw
	configureProcessTree(cmd)                                 // 平台钩子：Unix 置 Setpgid；Windows 无操作
	cmd.Cancel = func() error { return killProcessTree(cmd) } // ctx 取消也走级联回收（非只杀 sh）
	if err := cmd.Start(); err != nil {
		_ = pr.Close()
		_ = pw.Close()
		return nil, fmt.Errorf("%v", err)
	}
	// 父侧立刻关闭写端：子进程已在 Start 时拿到自己的 fd 副本，父侧若继续持有写端，
	// 读端在子进程退出后将永远等不到 EOF（抽干 goroutine 一直挂着）。
	_ = pw.Close()

	tail := &serviceTail{}
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		drainServiceTail(pr, tail)
	}()
	return &startedProcess{cmd: cmd, tail: tail, drained: drained}, nil
}
