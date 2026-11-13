package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// F4：非零退出码应返回错误，且 stderr 被捕获进错误信息。
func TestRunProcessNonZeroExit(t *testing.T) {
	_, err := runProcess(context.Background(), "sh -c 'echo boom >&2; exit 3'", map[string]interface{}{}, nil, "", "", nil)
	if err == nil {
		t.Fatal("非零退出码应返回错误")
	}
	if !containsSubstr(err.Error(), "boom") {
		t.Fatalf("stderr 应被捕获: %v", err)
	}
}

// F4：stderr 输出不应干扰 stdout 末行 JSON 解析。
func TestRunProcessStderrIgnored(t *testing.T) {
	out, err := runProcess(context.Background(), `sh -c 'echo warn >&2; echo "{\"name\":\"x\"}"'`, map[string]interface{}{}, nil, "", "", nil)
	if err != nil {
		t.Fatalf("stderr 不应导致失败: %v", err)
	}
	if out["name"] != "x" {
		t.Fatalf("stdout 解析异常: %v", out)
	}
}

// F4：stdout 为空应报错。
func TestRunProcessEmptyStdout(t *testing.T) {
	if _, err := runProcess(context.Background(), "true", map[string]interface{}{}, nil, "", "", nil); err == nil {
		t.Fatal("空 stdout 应报错")
	}
}

// F4：stdout 含日志行时取末行 JSON。
func TestRunProcessDirtyStdout(t *testing.T) {
	out, err := runProcess(context.Background(), `sh -c 'echo "log line"; echo "{\"name\":\"x\"}"'`, map[string]interface{}{}, nil, "", "", nil)
	if err != nil {
		t.Fatalf("脏 stdout 应跳过日志行: %v", err)
	}
	if out["name"] != "x" {
		t.Fatalf("末行 JSON 解析异常: %v", out)
	}
}

// F4：超时受控——挂死命令在时限内返回错误，不占住执行线程。
func TestRunProcessTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if _, err := runProcess(ctx, "sleep 2", map[string]interface{}{}, nil, "", "", nil); err == nil {
		t.Fatal("超时命令应返回错误")
	}
}

// F4：未超时命令正常返回。
func TestRunProcessWithinTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := runProcess(ctx, "cat", map[string]interface{}{"name": "x"}, nil, "", "", nil)
	if err != nil {
		t.Fatalf("正常命令应成功: %v", err)
	}
	if out["name"] != "x" {
		t.Fatalf("回显异常: %v", out)
	}
}

func containsSubstr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// REQ-API-LOGS-STREAM：一次 process 执行的 stdout/stderr 必须**逐行实时**经 sink 上报
// （不再等命令结束才 flush 一次）。验证：多行输出按出现顺序、带正确 stream 标签送达 sink，
// 且空行不污染（与旧 emitLogLines 口径一致）。
func TestRunProcessStreamsLinesToSink(t *testing.T) {
	var mu sync.Mutex
	var got []struct {
		stream, line string
	}
	sink := func(stepID, stream, line string) {
		mu.Lock()
		got = append(got, struct {
			stream, line string
		}{stream, line})
		mu.Unlock()
	}
	_, err := runProcess(context.Background(),
		`sh -c 'echo "L1"; echo ""; echo "L2" >&2; echo "{\"ok\":true}"'`,
		map[string]interface{}{}, nil, "execX", "stepY", sink)
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	// L1 / L2 / JSON 共 3 行（空行被 emit 跳过）。
	if len(got) != 3 {
		t.Fatalf("应恰好上报 3 行，实际 %d: %+v", len(got), got)
	}
	// 逐行带正确 stream 标签、空行被跳过（核心契约）。
	// stdout 与 stderr 是两条独立 OS 管道，跨流到达顺序不保证（终端本就如此），
	// 故按流内顺序分别校验，不跨流断言绝对位置。
	var stdoutLines, stderrLines []string
	for _, g := range got {
		switch g.stream {
		case "stdout":
			stdoutLines = append(stdoutLines, g.line)
		case "stderr":
			stderrLines = append(stderrLines, g.line)
		default:
			t.Fatalf("未知 stream 标签: %+v", g)
		}
	}
	// stdout：先 L1 后 JSON（末行结果）。
	if len(stdoutLines) != 2 || stdoutLines[0] != "L1" || stdoutLines[1] != `{"ok":true}` {
		t.Fatalf("stdout 应为 [L1, {\"ok\":true}]，实际 %+v", stdoutLines)
	}
	// stderr：仅 L2。
	if len(stderrLines) != 1 || stderrLines[0] != "L2" {
		t.Fatalf("stderr 应为 [L2]，实际 %+v", stderrLines)
	}
}

// REQ-API-LOGS-STREAM：service 模式的长驻进程输出也要实时经 sink 上报
// （stdout/stderr 在 startProcess 里合并到同一根管道，统一标 stdout）。
func TestServiceProcessStreamsLinesToSink(t *testing.T) {
	var mu sync.Mutex
	var got []string
	sink := func(stepID, stream, line string) {
		mu.Lock()
		got = append(got, line)
		mu.Unlock()
	}
	rt := processRuntime{sink: sink, stepID: "stepZ"}
	p, err := rt.Start(context.Background(), `sh -c 'echo "svc-line"; sleep 0.3'`, map[string]interface{}{}, nil)
	if err != nil {
		t.Fatalf("启动失败: %v", err)
	}
	defer p.Kill()
	// 等待首行送达（实时，不依赖进程退出）。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) < 1 || got[0] != "svc-line" {
		t.Fatalf("service 输出应实时上报，实际 %+v", got)
	}
}

// Fork3：service 子进程级联回收——杀掉 sh 时其孙进程（sleep）也必须被回收，
// 否则会成为脱离监管的孤儿（红线 16 的延伸）。用「孙进程在 Kill 后是否触碰 marker」
// 来区分：若被级联回收，marker 永不会出现。
//
// 命令结构：`sh -c "(sleep 2; echo > MARKER) & wait"` —— sh 后台起一个子 shell，
// 子 shell 先 sleep 2s 再 echo 出 marker。若 Kill 只杀 sh、不杀子 shell，则 sleep
// 变孤儿并在 ~2s 时 echo 出 marker；若级联回收生效，子 shell 一并被杀，marker 不出现。
func TestProcessTreeKillNoOrphan(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "orphan_marker")
	rt := processRuntime{}
	p, err := rt.Start(context.Background(), fmt.Sprintf("(sleep 2; echo > %s) & wait", marker), map[string]interface{}{}, nil)
	if err != nil {
		t.Fatalf("启动失败: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if err := p.Kill(); err != nil {
		t.Fatalf("Kill 失败: %v", err)
	}
	// 最多等 3s：若孙进程（sleep 2）未被级联回收，会在 ~2s 时 echo 出 marker。
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(marker); err == nil {
			t.Fatalf("孙进程未被级联回收：marker 出现 %s", marker)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// ─── 2026-09-21：service 子进程输出留底与退出原因上报 ───────────────────────────
// 背景：service 输出原先丢进 os.DevNull（红线 16 的取舍），导致「拉起即秒退」的故障
// （端口被占、配置写错）在监管侧只剩「↻ 重启 第 N 次」，原因完全不可见。
// 现改为自有管道 + 内存缓冲，由 ExitInfo 交出「退出码 + 输出尾部」。

// 非零退出：退出码取真实值，stderr 内容留在尾部（失败原因不能丢）。
func TestServiceProcessExitInfoReportsCodeAndStderr(t *testing.T) {
	rt := processRuntime{}
	p, err := rt.Start(context.Background(), "echo svc-boom 1>&2; exit 3", map[string]interface{}{}, nil)
	if err != nil {
		t.Fatalf("启动失败: %v", err)
	}
	_ = p.Wait()

	rep, ok := p.(ExitReporter)
	if !ok {
		t.Fatal("service Process 必须实现 ExitReporter（否则监管无从打印退出原因）")
	}
	code, tail := rep.ExitInfo()
	if code != 3 {
		t.Fatalf("退出码应为 3，实际 %d", code)
	}
	if !tailHas(tail, "svc-boom") {
		t.Fatalf("输出尾部应含 stderr 内容，实际 %v", tail)
	}
}

// 正常退出：退出码 0，stdout 同样留底（如 `gear serve` 的启动横幅）。
func TestServiceProcessExitInfoCleanExitKeepsStdout(t *testing.T) {
	rt := processRuntime{}
	p, err := rt.Start(context.Background(), "echo svc-banner", map[string]interface{}{}, nil)
	if err != nil {
		t.Fatalf("启动失败: %v", err)
	}
	_ = p.Wait()

	code, tail := p.(ExitReporter).ExitInfo()
	if code != 0 {
		t.Fatalf("正常退出码应为 0，实际 %d", code)
	}
	if !tailHas(tail, "svc-banner") {
		t.Fatalf("stdout 应留底，实际 %v", tail)
	}
}

// 被 Kill 的 service 退出码不得为 0：否则「被杀」会被监管报成「正常退出」。
func TestServiceProcessExitInfoNonZeroWhenKilled(t *testing.T) {
	rt := processRuntime{}
	p, err := rt.Start(context.Background(), "sleep 5", map[string]interface{}{}, nil)
	if err != nil {
		t.Fatalf("启动失败: %v", err)
	}
	if err := p.Kill(); err != nil {
		t.Fatalf("Kill 失败: %v", err)
	}
	_ = p.Wait()
	if code, _ := p.(ExitReporter).ExitInfo(); code == 0 {
		t.Fatal("被杀的 service 退出码不应为 0")
	}
}

// 红线 16 回归守卫：service 子进程的输出**不得**继承调用方 os.Stdout。
// 做法：把 os.Stdout 换成临时文件，再跑一个会 echo 的 service。若实现回归成
// cmd.Stdout = os.Stdout，marker 就会落进该文件（并进而让孤儿孙进程占住调用方管道）。
func TestServiceProcessDoesNotInheritCallerStdout(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "caller-stdout")
	if err != nil {
		t.Fatalf("建临时文件失败: %v", err)
	}
	defer f.Close()

	saved := os.Stdout
	os.Stdout = f

	rt := processRuntime{}
	p, err := rt.Start(context.Background(), "echo leak-marker", map[string]interface{}{}, nil)
	if err != nil {
		os.Stdout = saved
		t.Fatalf("启动失败: %v", err)
	}
	_ = p.Wait()
	_, tail := p.(ExitReporter).ExitInfo()
	os.Stdout = saved

	if !tailHas(tail, "leak-marker") {
		t.Fatalf("service 输出应进自有缓冲，实际 %v", tail)
	}
	if data, _ := os.ReadFile(f.Name()); len(data) != 0 {
		t.Fatalf("service 输出泄漏进调用方 stdout: %q", string(data))
	}
}

// 尾部缓冲有界：崩溃循环下内存恒定，不随输出总量增长。
func TestServiceTailKeepsOnlyLastLines(t *testing.T) {
	tail := &serviceTail{}
	total := serviceTailLines * 3
	for i := 0; i < total; i++ {
		tail.push(fmt.Sprintf("line-%d", i))
	}
	got := tail.snapshot()
	if len(got) != serviceTailLines {
		t.Fatalf("应只保留 %d 行，实际 %d", serviceTailLines, len(got))
	}
	if want := fmt.Sprintf("line-%d", total-1); got[len(got)-1] != want {
		t.Fatalf("末行应为 %s，实际 %s", want, got[len(got)-1])
	}
	if want := fmt.Sprintf("line-%d", total-serviceTailLines); got[0] != want {
		t.Fatalf("首行应为 %s，实际 %s", want, got[0])
	}
}

// tailHas 判断输出尾部是否含某子串。
func tailHas(tail []string, sub string) bool {
	for _, ln := range tail {
		if containsSubstr(ln, sub) {
			return true
		}
	}
	return false
}
