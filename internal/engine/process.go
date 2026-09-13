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
)

// defaultExecTimeout 是单次 process 执行的默认超时（F4：防止挂死工具占住执行线程）。
// 设为变量便于测试注入更短/更长时限。
var defaultExecTimeout = 30 * time.Second

// processRuntime 实现 Runtime 接口：外部进程，stdin 进 JSON，stdout 末行出 JSON（PRD §3.3）。
type processRuntime struct{}

func (p *processRuntime) Run(ctx context.Context, entry string, input map[string]interface{}) (map[string]interface{}, error) {
	return runProcess(ctx, entry, input)
}

// Start 拉起一个长驻 process（service 模式），不等待其结束，返回可监管句柄。
func (p *processRuntime) Start(ctx context.Context, entry string, input map[string]interface{}) (Process, error) {
	cmd, err := startProcess(ctx, entry, input)
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
func runProcess(ctx context.Context, entry string, input map[string]interface{}) (map[string]interface{}, error) {
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

// startProcess 拉起一个 process 子进程但**不等待其结束**（service 模式用，见 PRD §14.2）。
// input 经 stdin 传入；stdout/stderr 继承调用方（由 Supervisor 接管其生命周期）。
// 返回的 *exec.Cmd 由调用方负责在监管结束时 Kill（Supervisor.stopAll）。
func startProcess(ctx context.Context, entry string, input map[string]interface{}) (*exec.Cmd, error) {
	inBytes, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("序列化 input 失败: %w", err)
	}
	cmd := exec.CommandContext(ctx, "sh", "-c", entry)
	cmd.Stdin = bytes.NewReader(inBytes)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("%v", err)
	}
	return cmd, nil
}
