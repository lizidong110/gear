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
	sp, err := startProcess(ctx, entry, input, host, p.stepID, p.sink)
	if err != nil {
		return nil, err
	}
	return &execProcess{
		cmd:     sp.cmd,
		kill:    func() error { return killProcessTree(sp.cmd) },
		tail:    sp.tail,
		drained: sp.drained,
		first:   sp.first,
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
	first   *serviceFirst

	mu      sync.Mutex
	waitErr error
	waited  bool
	killed  bool
}

// Kill 标记「本进程已被我们杀掉」。
//
// 为什么要记：被杀的进程 ProcessState 仍是 nil（要等 Wait 才填），而 Output() 的
// 提前收工判据若只看它，每个「不报声明的 service」都要白等满 serviceFirstTimeout（2s）。
func (e *execProcess) Kill() error {
	e.mu.Lock()
	e.killed = true
	e.mu.Unlock()
	return e.kill()
}

// Output 阻塞等待（上限 serviceFirstTimeout）子进程首行声明，解析成 step output。
//
// 首行到不了（普通长驻进程 / 秒退）时返回 nil —— 如实。下游引用
// `${steps.x.output.port}` 若拿到 nil，由模板解析报「未知字段」，不静默串成空字符串。
func (e *execProcess) Output() map[string]interface{} {
	if e.first == nil {
		return nil
	}
	return e.first.wait(e.exited)
}

// exited 报告进程是否已「不会再有新输出」（供 serviceFirst.wait 提前收工）。
func (e *execProcess) exited() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.waited || e.killed || e.cmd.ProcessState != nil
}

func (e *execProcess) Pid() int { return e.cmd.Process.Pid }

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
	// 逐行实时上报：logTee 一边累积（供结果解析/错误回显）一边把完整行即时喂给 sink，
	// 不再等命令结束后才 flush 一次（那是旧实现，实时日志会变成「跑完才吐」，失去排障价值）。
	// 仅 stdout/stderr 文本进 sink，绝不包含 input（secret 等敏感数据不落日志）。
	stdout := &logTee{sink: sink, stepID: stepID, stream: "stdout"}
	stderr := &logTee{sink: sink, stepID: stepID, stream: "stderr"}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	runErr := cmd.Run()
	stdout.flush()
	stderr.flush()
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

// logTee 把子进程输出同时「累积」（供结果解析/错误回显）与「逐行实时上报」（供实时日志）。
// 子进程写入是按块到达的，一行可能跨多次 Write，因此用 pend 缓冲尚未遇换行的前缀；
// 一旦出现换行即把整行交给 sink，命令结束时的 flush 把末尾无换行的残行也发出。
// 空行/纯空白行不上报（与旧 emitLogLines 口径一致），且绝不累积 input（secret 不落日志）。
type logTee struct {
	sink   func(stepID, stream, line string)
	stepID string
	stream string
	buf    bytes.Buffer
	pend   []byte
}

func (t *logTee) Write(p []byte) (int, error) {
	t.buf.Write(p)
	t.pend = append(t.pend, p...)
	for {
		i := bytes.IndexByte(t.pend, '\n')
		if i < 0 {
			break
		}
		t.emit(string(t.pend[:i]))
		t.pend = append(t.pend[:0], t.pend[i+1:]...)
	}
	return len(p), nil
}

func (t *logTee) emit(line string) {
	line = strings.TrimRight(line, "\r")
	if strings.TrimSpace(line) == "" {
		return
	}
	if t.sink != nil {
		t.sink(t.stepID, t.stream, line)
	}
}

// flush 把末尾未遇换行的残行发出（命令结束时的最后一行可能没有换行符）。
func (t *logTee) flush() {
	t.emit(string(t.pend))
	t.pend = nil
}

func (t *logTee) Bytes() []byte  { return t.buf.Bytes() }
func (t *logTee) String() string { return t.buf.String() }

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
	// 调用上下文（IDENTITY_CONTEXT_DESIGN.md §3.3）：与 capabilities 同批注入。
	// 必须在 **input_schema 校验之后** 才写（runOnceStep 的顺序），否则会破坏入参校验——
	// 子进程因此总能看到是谁发起的这次执行，鉴权 Hook 也可以用它的对外形态做对外服务。
	caller := host.Caller()
	ctxObj["caller"] = map[string]interface{}{
		"subject": caller.Subject,
		"app_id":  caller.AppID,
		"via":     caller.Via,
	}
	input["ctx"] = ctxObj
	return input
}

const (
	// serviceTailLines 是每个 service 子进程保留的输出行数上限（诊断用，内存恒定）。
	serviceTailLines = 50
	// serviceDrainTimeout 是 ExitInfo 等待「最后几行落进缓冲」的上限。
	// 必须有界：若孙进程仍持有写端（级联回收完成前的窗口），读到 EOF 可能永远等不到。
	serviceDrainTimeout = 300 * time.Millisecond
	// serviceFirstTimeout 是等待长驻子进程「首行声明」的上限（见 serviceFirst）。
	//
	// 取 2s 而非 serviceDrainTimeout 的 300ms：`gear serve` 这类进程要先 net.Listen、
	// 加载 registry，首行常滞后百毫秒以上；而首行 output 一旦漏掉，下游
	// `${steps.x.output.port}` 拿到 nil，失败形态是「反代 target 变成
	// http://127.0.0.1:」这种静默错串——比多等一会儿贵得多。
	serviceFirstTimeout = 2 * time.Second
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

// serviceFirst 捕获长驻子进程输出的**第一行**，并尝试把它解析成 JSON object。
//
// 长驻进程用它上报一次性声明（`{"port":8787}`），process runtime 再把这行当 step output，
// 于是 `${steps.api.output.port}` 能直接喂给 static_site 的 proxy.target —— 全链路端口
// 分配缺的正是这一环（其余两环：内核挑端口、能查到真实端口）。
//
// 契约：**第一行非空行**必须是那个 JSON；解析不出来就留空。绝不用「看起来像端口」的东西
// 凑数——0 之类的假值会让下游写出查不出原因的错串。
type serviceFirst struct {
	mu      sync.Mutex
	arrived bool
	object  map[string]interface{}
}

// set 记录一行并尝试解析；**只认第一行**（后续调用被忽略——有人重复喂也绝不覆盖）。
func (f *serviceFirst) set(line string) {
	var obj map[string]interface{}
	if trimmed := strings.TrimSpace(line); trimmed != "" {
		if err := json.Unmarshal([]byte(trimmed), &obj); err != nil {
			return // 不是 JSON（banner / 普通日志）：如实留空，不猜
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.arrived {
		return
	}
	f.arrived = true
	f.object = obj
}

// take 返回已解析的 object（未到或解析失败为 nil）。调用方**必须**先 wait 过。
func (f *serviceFirst) take() map[string]interface{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.object
}

// arrivedNow 在锁内报告首行是否已到（供 wait 提前结束）。
func (f *serviceFirst) arrivedNow() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.arrived
}

// wait 有界等待首行（上限 serviceFirstTimeout），返回解析结果（可能 nil）。
//
// 为什么要等：rt.Start 返回时子进程往往**还没打印第一行**（要先 listen），而调用方
// 紧接着就会读 output。进程已退出时可立即收工——不必为「秒退且没输出」白等满超时。
func (f *serviceFirst) wait(procDone func() bool) map[string]interface{} {
	deadline := time.NewTimer(serviceFirstTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		if f.arrivedNow() {
			return f.take()
		}
		if procDone != nil && procDone() {
			return f.take() // 进程已走，首行不会再来了
		}
		select {
		case <-deadline.C:
			return f.take()
		case <-ticker.C:
		}
	}
}

// drainServiceTail 把自有管道按行抽干进缓冲，直到 EOF（写端全关时触发）后关闭读端。
// 第一行非空行额外送进 first（长驻进程的一次性声明），供 step output 使用。
// drainServiceTail 把自有管道按行抽干进缓冲，直到 EOF（写端全关时触发）后关闭读端。
// 第一行非空行额外送进 first（长驻进程的一次性声明），供 step output 使用。
// sink 非空时，每行（非空）同时实时上报——service 的 stdout/stderr 在 startProcess 里合并到
// 同一根管道，故统一标 stdout；这行经组合 sink 同时落盘 + 推 broker，让长驻进程的实时输出可见。
func drainServiceTail(rc io.ReadCloser, t *serviceTail, first *serviceFirst, sink func(stepID, stream, line string), stepID string) {
	defer rc.Close()
	sc := bufio.NewScanner(rc)
	// 单行上限提到 1MB：默认 64KB 会让超长行提前终止抽取，连带丢掉其后（可能含失败原因）的行。
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	firstLine := true
	for sc.Scan() {
		line := sc.Text()
		t.push(line) // 内存环形缓冲（退出原因诊断），始终保留。
		if firstLine {
			firstLine = false
			first.set(line)
		}
		if sink != nil && strings.TrimSpace(line) != "" {
			sink(stepID, "stdout", line)
		}
	}
}

// startedProcess 是 startProcess 的产物：命令本体 + 输出缓冲 + 抽干完成信号 + 首行声明。
type startedProcess struct {
	cmd     *exec.Cmd
	tail    *serviceTail
	drained chan struct{}
	first   *serviceFirst
}

// startProcess 拉起一个 process 子进程但**不等待其结束**（service 模式用，见 PRD §14.2）。
// input 经 stdin 传入；stdout/stderr 汇入**自有管道**（见 serviceTail：既不继承调用方
// 管道，也不丢进 DevNull），由父侧 goroutine 抽干进内存缓冲，供监管诊断退出原因。
// 返回的 startedProcess 由调用方负责在监管结束时 Kill（Supervisor.stopAll）。
func startProcess(ctx context.Context, entry string, input map[string]interface{}, host *cap.Context, stepID string, sink func(stepID, stream, line string)) (*startedProcess, error) {
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
	first := &serviceFirst{}
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		drainServiceTail(pr, tail, first, sink, stepID)
	}()
	return &startedProcess{cmd: cmd, tail: tail, drained: drained, first: first}, nil
}
