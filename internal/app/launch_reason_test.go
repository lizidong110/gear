package app

// launch_reason_test.go —— 起服失败根因归因（REQ-MCP-DIAG-STARTFAIL）的行为守卫。
//
// 为什么单独立文件：归因住在 app 包（`gear app start` 的返回面），而它的正确性完全取决于
// 「从日志里挑」这个取法——取错一行就得到 usage 而不是病因。取法细节（为什么取最后一条
// `Error:` 行而不是最后一行）见 launch_reason.go 的注释，这里只钉行为。

import (
	"strings"
	"testing"
)

// realStartupLog 是 2026-09-27 实跑 agent 范例时的日志尾部（缺侧车持有步那一次）。
// 它同时是「取法必须抗 usage 干扰」的证据：末尾紧跟一整块 cobra usage。
func realStartupLog() []string {
	raw := "[app run] 监管 1 个 Flow，restart=always，maxConcurrent=0（Ctrl+C 停止）\n" +
		"✓ step anchor (agent__config) -> map[]\n" +
		"• step serve (ext.serve) 作为 service 长驻启动 (pid=0)\n" +
		"• step api (agent__api) 作为 service 长驻启动 (pid=6516)\n" +
		"! Flow agent.up 监管结束: step site 解析 input 失败: 键不存在: port\n" +
		"Error: step site 解析 input 失败: 键不存在: port\n" +
		"Usage:\n" +
		"  gear app run <flow-id>... [flags]\n" +
		"\n" +
		"Flags:\n" +
		"  -h, --help             help for run\n"
	var out []string
	for _, l := range strings.Split(raw, "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

func TestStartupFailureReasonPicksRootCause(t *testing.T) {
	got := startupFailureReason(realStartupLog())
	want := "step site 解析 input 失败: 键不存在: port"
	if got != want {
		t.Errorf("应取到根因行，got %q want %q", got, want)
	}
	// 取到 usage 就意味着白做：诊断会把人引向一整块命令帮助。
	for _, l := range []string{"Usage:", "Flags:", "gear app run <flow-id>"} {
		if strings.Contains(got, l) {
			t.Errorf("根因里混进了 usage 块（%q）：取法错了", l)
		}
	}
}

// 日志里没有 `Error:` 行（进程被外部杀掉、日志为空等）时给空串——**不编造原因**，
// 让 CLI / MCP 退回「看日志」而不是印一句猜的。
func TestStartupFailureReasonEmptyWhenNoErrorLine(t *testing.T) {
	if got := startupFailureReason([]string{"", "  ", "nothing useful here"}); got != "" {
		t.Errorf("无 Error 行应返回空，got %q", got)
	}
	if got := startupFailureReason(nil); got != "" {
		t.Errorf("无日志应返回空，got %q", got)
	}
}

// 空 `Error:` 行要跳过：宁可返回空（退回「读日志」），也不要把紧随其后的 usage 当根因——
// 那会给出一句指向命令帮助的「病因」，比没有归因更糟。
func TestStartupFailureReasonSkipsEmptyErrorLine(t *testing.T) {
	got := startupFailureReason([]string{"Error: ", "Usage:", "  gear app run"})
	if got != "" {
		t.Errorf("空 Error 行应被跳过（不把 usage 当根因），got %q", got)
	}
}

// classifyStartupFailure 的口径：**保留原文**、在后面附上人话（智能体既要能照原文去查，
// 也要能直接看懂）。识别不出就原样返回。
func TestClassifyStartupFailure(t *testing.T) {
	cases := []struct {
		name, in, wantIn string // wantIn 是必须出现在结果里的一段人话
	}{
		{"端口被占", "listen tcp 127.0.0.1:8795: bind: Only one usage of each socket address (protocol/network address/port) is normally permitted.", "端口被别的进程占了"},
		{"未知能力", `step site: 未知的原子能力 "static_site"`, "调用了 registry 里不存在的能力"},
		{"未登记 capability", `Tool x: 未知的 capability "ext.ai"`, "未在 extensions.yaml 登记"},
		{"模板竞态", "step site 解析 input 失败: 键不存在: port", "同层步骤是并行执行的"},
		{"侧车无人持有", "dial tcp 127.0.0.1:8095: connect: connection refused", "缺一条 `mode: service` 的 `ext.serve` 步"},
		{"可执行文件找不到", `exec: "node": executable file not found in %PATH%`, "entry 首 token 找不到可执行文件"},
		{"成环", "Flow 存在循环依赖", "依赖成环"},
	}
	for _, c := range cases {
		got := classifyStartupFailure(c.in)
		if !strings.Contains(got, c.wantIn) {
			t.Errorf("%s: 结果里应出现 %q，实际 %q", c.name, c.wantIn, got)
		}
		if !strings.Contains(got, c.in) {
			t.Errorf("%s: 原文必须保留以便照着查，实际 %q", c.name, got)
		}
	}

	// 没见过的模式原样返回：不猜。
	if got := classifyStartupFailure("some brand new failure"); got != "some brand new failure" {
		t.Errorf("未知模式应原样返回，got %q", got)
	}
	if got := classifyStartupFailure(""); got != "" {
		t.Errorf("空输入应返回空，got %q", got)
	}
}
