package mcp

// tools_invoke.go：能力直调 —— gear_invoke。
//
// 存在的理由（评审 I-8）：REST 面早有 `POST /api/capabilities/invoke`，而 MCP 面只有
// 「只读目录」的 gear_capabilities。于是智能体想「试一下 db.exec 能不能用」，只能先写一条
// Flow 再 gear_run——为一次原子调用付出一整轮编排的代价。
//
// 与 REST 的同源约束（**必须**保持一致，否则同一进程两个执行面安全水位不同）：
//   - 同一套引擎入口（`engine.InvokeCapability`，与 step 直调用同一份参数校验）；
//   - 同一条禁则（`cap.BlockedDirectInvoke`：禁 `secret.*` 直调）。
//
// ⚠ 权限口径：本工具等价于「以宿主机身份执行任意非密钥能力」（可发 HTTP、可写 db、可发事件）。
// 故标注为破坏性，且 MCP 面无鉴权——唯一的防线仍是「只绑 127.0.0.1 + 同源判定」。

import (
	"context"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"gear/internal/cap"
)

// registerInvokeTools 挂上能力直调工具。
func (s *Server) registerInvokeTools(ms *mcp.Server) {
	mcp.AddTool(ms, &mcp.Tool{
		Name: "gear_invoke",
		Description: "直接调用一个原子能力的方法（如 db.query / http.Request / ext.ai.complete），" +
			"用于「试一下这个能力通不通」，不必先写一条 Flow。密钥类（secret.*）一律拒绝；" +
			"方法名与参数 schema 先看 gear_capabilities（参数按名传，与目录里的 schema 对齐）。",
		Annotations:  destructive("能力直调"),
		OutputSchema: outInvoke,
	}, s.handleInvoke)
}

// handleInvoke 直调一个原子能力的方法。
func (s *Server) handleInvoke(ctx context.Context, _ *mcp.CallToolRequest, in InvokeArgs) (*mcp.CallToolResult, any, error) {
	name := strings.TrimSpace(in.Name)
	method := strings.TrimSpace(in.Method)
	if name == "" || method == "" {
		return toolError("BAD_ARGUMENT", "name 与 method 均不能为空",
			"先用 gear_capabilities 查能力名与方法名（如 db.query）"), nil, nil
	}
	if cap.BlockedDirectInvoke(name) {
		// 与 REST 面同一条禁则（cap.BlockedDirectInvoke 是唯一真相源）。
		return toolError("INVOKE_BLOCKED",
			"能力 "+name+" 不允许直调：它是唯一「读到即不可挽回」的能力（密钥明文）",
			"密钥只在 Flow 内部用 ${secrets.NAME} / ctx.secret.get 引用；播种走 CLI（MCP 不写密钥）"), nil, nil
	}
	res, err := s.engine.InvokeCapability(name, method, in.Args)
	if err != nil {
		return toolErrorFrom("INVOKE_FAILED", err), nil, nil
	}
	return nil, map[string]any{"name": name, "method": method, "result": res}, nil
}
