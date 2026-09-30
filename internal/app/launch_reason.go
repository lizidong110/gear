package app

import "strings"

// 起服失败的根因归因（REQ-MCP-DIAG-STARTFAIL）。
//
// 此前 `startup_failed` 只带 last_logs，而 CLI 与 MCP 各印一句硬编码的泛化文案
// （「常见原因：端口被占、侧车未构建、registry 路径不符」）——那句话三种故障都说得中，
// 于是不说等于没说。真正的原因只在日志里，而日志末尾还夹着 cobra 打出的 usage 块。
//
// 口径：**只收录已经实测见过的模式**，没见过的原样返回。猜出来的归因比没有归因更糟——
// 因为智能体会照着它去改，改错处比「去读日志」更浪费一轮。

// startupFailureReason 从日志尾部挑出根因行（去掉 `Error:` 前缀与尾随的 usage 块）。
//
// 为什么取「最后一条 `Error:` 行」而不是最后一行：cobra 把 Usage 块也打进 stderr，
// 于是日志末尾的顺序是 `Error: <病因>` → 一整块 usage。取最后一行拿到的是 usage，
// 病因被挤掉——2026-09-27 实测两次（缺侧车持有步、模板引用未产出步骤）都是这个形态。
func startupFailureReason(logs []string) string {
	for i := len(logs) - 1; i >= 0; i-- {
		l := strings.TrimSpace(logs[i])
		if !strings.HasPrefix(l, "Error:") {
			continue
		}
		msg := strings.TrimSpace(strings.TrimPrefix(l, "Error:"))
		if msg == "" {
			continue
		}
		// usage 块若被折行紧贴在后，砍掉（它可能落在同一行：Usage: 前无换行时不会，
		// 但 `Error: xxx Usage: gear app run` 这种拼接在历史日志里出现过）。
		if i := strings.Index(msg, "Usage:"); i >= 0 {
			msg = strings.TrimSpace(msg[:i])
		}
		return msg
	}
	return ""
}

// classifyStartupFailure 给一句人话，附在根因后面。识别不出就原样返回。
func classifyStartupFailure(reason string) string {
	switch {
	case reason == "":
		return ""
	case strings.Contains(reason, "Only one usage of each socket address"):
		return reason + " —— 端口被别的进程占了。换端口重跑，或先停掉占着它的旧实例。"
	case strings.Contains(reason, "未知的原子能力"):
		return reason + " —— 调用了 registry 里不存在的能力：Tool 没注册，或属于另一个库。"
	case strings.Contains(reason, "未知的 capability"):
		return reason + " —— 声明的 capability 未在 extensions.yaml 登记（注意校验期按 cwd 定位该文件）。"
	case strings.Contains(reason, "键不存在"):
		return reason + " —— 模板引用了一个还没产出的步骤输出：**同层步骤是并行执行的**，" +
			"站点步不能引用与它同层的后台步。要么把端口写死在两处，要么给引擎补一条屏障语义。"
	case strings.Contains(reason, "connection refused"):
		return reason + " —— 要用的侧车没人持有：Flow 里缺一条 `mode: service` 的 `ext.serve` 步。"
	case strings.Contains(reason, "executable file not found"):
		return reason + " —— process 型 Tool 的 entry 首 token 找不到可执行文件（相对当前 cwd 解析）。"
	case strings.Contains(reason, "存在循环依赖"):
		return reason + " —— Flow 的依赖成环；拓扑排序直接拒绝，没有「先跑一部分」这回事。"
	default:
		// 没见过的模式不猜。原样返回即可，智能体去读 last_logs。
		return reason
	}
}
