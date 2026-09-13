package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"gear/internal/cap"
)

// defaultExecTimeout 是单次 process 执行的默认超时（F4：防止挂死工具占住执行线程）。
// 设为变量便于测试注入更短/更长时限。
var defaultExecTimeout = 30 * time.Second

// processRuntime 实现 Runtime 接口：外部进程，stdin 进 JSON，stdout 末行出 JSON（PRD §3.3）。
type processRuntime struct{}

func (p *processRuntime) Run(ctx context.Context, entry string, input map[string]interface{}, host *cap.Context) (map[string]interface{}, error) {
	return runProcess(ctx, entry, input, host)
}

// Start 拉起一个长驻 process（service 模式），不等待其结束，返回可监管句柄。
func (p *processRuntime) Start(ctx context.Context, entry string, input map[string]interface{}, host *cap.Context) (Process, error) {
	cmd, err := startProcess(ctx, entry, input, host)
	if err != nil {
		return nil, err
	}
	return &execProcess{cmd: cmd}, nil
}

// execProcess 把 *exec.Cmd 适配成 engine.Process 抽象（避免上层依赖 os/exec）。
type execProcess struct{ cmd *exec.Cmd }

func (e *execProcess) Pid() int { return e.cmd.Process.Pid }

func (e *execProcess) Kill() error { return e.cmd.Process.Kill() }

func (e *execProcess) Wait() error { return e.cmd.Wait() }

// runProcess 执行 process runtime 的 Tool：把 input 序列化为 JSON 经 stdin 传入，
// 读取 stdout 的最后一行非空内容作为 JSON 输出（见 PRD §3.3 的 process 契约）。
//
// 当前通过 `sh -c <entry>` 启动命令（开发环境为 Git Bash，sh 可用）。
// 跨平台/生产化时再抽成可配置的 shell 解析器。
// ctx 用于超时控制：超时时命令被杀死并返回错误（F4）。
func runProcess(ctx context.Context, entry string, input map[string]interface{}, host *cap.Context) (map[string]interface{}, error) {
	input = withHostCapabilities(input, host)
	inBytes, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("序列化 input 失败: %w", err)
	}

	cmd := exec.CommandContext(ctx, "sh", "-c", entry)
	cmd.Stdin = bytes.NewReader(inBytes)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%v (stderr=%s)", err, stderr.String())
	}

	last := lastNonEmptyLine(stdout.String())
	if last == "" {
		return nil, fmt.Errorf("stdout 为空")
	}

	var out map[string]interface{}
	if err := json.Unmarshal([]byte(last), &out); err != nil {
		return nil, fmt.Errorf("解析 stdout 失败(%q): %w", last, err)
	}
	return out, nil
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

// serviceOut 是 service 模式子进程的 stdout/stderr 汇（默认丢到 os.DevNull）。
//
// 关键约束（红线 16：service 子进程不得继承调用方 stdout/stderr）：
// service 通过 `sh -c <entry>` 拉起，其孙进程（如 `sleep`）在父 sh 被 Kill() 后会成为
// 孤儿并继续持有继承来的文件描述符。若直接继承 os.Stdout（go test 的管道），孤儿进程会
// 长期占用该管道 → "Test I/O incomplete"。将输出脱离调用方管道可彻底消除该泄漏。
// 结构化 service 日志（按 service 落盘 / 上报 Supervisor）留待后续日志切片，本处仅保证
// 生命周期与 fd 正确性。
var serviceOut *os.File

func init() {
	f, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		// os.DevNull 几乎不可能打开失败；极端情况下退回 os.Stderr（仍可能继承管道，
		// 但仅在 DevNull 异常时触发，属不可达分支）。
		serviceOut = os.Stderr
		return
	}
	serviceOut = f
}

// startProcess 拉起一个 process 子进程但**不等待其结束**（service 模式用，见 PRD §14.2）。
// input 经 stdin 传入；stdout/stderr 统一导向 serviceOut（见上方红线 16 说明，不继承调用方管道）。
// 返回的 *exec.Cmd 由调用方负责在监管结束时 Kill（Supervisor.stopAll）。
func startProcess(ctx context.Context, entry string, input map[string]interface{}, host *cap.Context) (*exec.Cmd, error) {
	input = withHostCapabilities(input, host)
	inBytes, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("序列化 input 失败: %w", err)
	}
	cmd := exec.CommandContext(ctx, "sh", "-c", entry)
	cmd.Stdin = bytes.NewReader(inBytes)
	cmd.Stdout = serviceOut
	cmd.Stderr = serviceOut
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("%v", err)
	}
	return cmd, nil
}
