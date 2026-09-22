package mcp

// tools_run.go：执行类工具 —— gear_run / gear_execution。
//
// 两条语义分界线（§9.1 第 2 条）：
//   - **执行失败不是工具错误**：Flow 跑挂了也**成功返回**记录（`status: failed` + error + result），
//     让智能体从记录里定位到具体 step；
//   - **前置条件不满足才是工具错误**：对象不存在、参数非法、service Flow 被拒。

import (
	"context"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// registerRunTools 挂上执行与观察类工具。
func (s *Server) registerRunTools(ms *mcp.Server) {
	mcp.AddTool(ms, &mcp.Tool{
		Name: "gear_run",
		Description: "跑一次编排。kind=flow：**异步**启动并返回 execution_id（`wait_seconds` 短等结果，再用 gear_execution 取详情）；" +
			"kind=tool：同步执行单个 Tool 并直接返回输出。**长驻应用请用 gear_app(start)**——service Flow 在这里一定被拒。",
		Annotations: idempotent("执行编排"),
	}, s.handleRun)

	mcp.AddTool(ms, &mcp.Tool{
		Name: "gear_execution",
		Description: "查看执行记录：给 id 看单次执行（可带日志），不给 id 则列出最近执行。" +
			"排障时先用它定位到具体 step，改完 YAML 再重跑。",
		Annotations: readOnly("执行记录"),
	}, s.handleExecution)
}

// handleRun 执行一次编排。
//
// kind=flow 为什么默认异步：MCP 调用受客户端超时约束，而一条要 build / 抓数据 / 计算的 Flow
// 很容易顶到超时。REST 面早已是「202 + execution id」，这里对齐同一语义（§6.2④）。
// kind=tool 保持同步：Tool 是原子动作（毫秒级），异步反而给智能体增加一次往返。
func (s *Server) handleRun(_ context.Context, _ *mcp.CallToolRequest, in RunArgs) (*mcp.CallToolResult, any, error) {
	kind := strings.ToLower(strings.TrimSpace(in.Kind))
	if kind == "" {
		kind = "flow"
	}
	if strings.TrimSpace(in.ID) == "" {
		return toolError("BAD_ARGUMENT", "id 不能为空", "先用 gear_catalog 确认 id"), nil, nil
	}
	switch kind {
	case "tool":
		// 先确认 Tool 存在：engine.RunTool 对「不存在」返回的是**无 sentinel 的纯文本**错误
		// （`Tool 不存在: x`），不先分流就会跟「Tool 跑挂了」混成同一个 TOOL_RUN_FAILED 码——
		// 而两者的修法完全相反（一个是 id 写错，一个要看执行日志）。这里只是**归类**，
		// 存在性判定仍以 registry 为唯一真相源（同 gear_describe 的做法）。
		found, ferr := s.reg.GetTool(in.ID)
		if ferr != nil {
			return toolErrorFrom("NOT_FOUND", ferr), nil, nil
		}
		if found == nil {
			return toolError("NOT_FOUND", "Tool 不存在: "+in.ID,
				"先用 gear_catalog(kind=tool) 确认 id（可能是应用前缀没写全）"), nil, nil
		}
		out, err := s.engine.RunTool(in.ID, in.Input)
		if err != nil {
			return toolErrorFrom("TOOL_RUN_FAILED", err), nil, nil
		}
		if out == nil {
			out = map[string]any{}
		}
		return nil, map[string]any{"kind": "tool", "id": in.ID, "output": out}, nil
	case "flow":
		id, err := s.startFlowExec(in.ID, in.Vars)
		if err != nil {
			return toolErrorFrom("FLOW_RUN_FAILED", err), nil, nil
		}
		snap := s.executionSnapshot(id, in.WaitSeconds)
		snap["hint"] = "执行是异步的：用 gear_execution(id) 取最终结果与日志"
		return nil, snap, nil
	default:
		return toolError("BAD_ARGUMENT", "kind 必须是 tool | flow（实际: "+in.Kind+")", ""), nil, nil
	}
}

// handleExecution 返回单次执行记录或执行列表。
func (s *Server) handleExecution(_ context.Context, _ *mcp.CallToolRequest, in ExecutionArgs) (*mcp.CallToolResult, any, error) {
	if strings.TrimSpace(in.ID) == "" {
		list := s.execs.List()
		items := make([]map[string]any, 0, len(list))
		for _, e := range list {
			m := map[string]any{
				"id": e.ID, "flow_id": e.FlowID, "status": e.Status, "started_at": e.StartedAt,
			}
			if e.Error != "" {
				m["error"] = e.Error
			}
			if e.FinishedAt != nil {
				m["finished_at"] = *e.FinishedAt
			}
			items = append(items, m)
		}
		return nil, map[string]any{"executions": items, "total": len(items)}, nil
	}

	e, ok := s.execs.Get(in.ID)
	if !ok {
		return toolError("NOT_FOUND", "执行记录不存在: "+in.ID,
			"不带 id 调 gear_execution 可列出最近执行"), nil, nil
	}
	out := map[string]any{
		"id": e.ID, "flow_id": e.FlowID, "status": e.Status, "started_at": e.StartedAt,
	}
	if e.Error != "" {
		out["error"] = e.Error
	}
	if e.Result != nil {
		out["result"] = e.Result
	}
	if len(e.ToolIDs) > 0 {
		out["tool_ids"] = e.ToolIDs
	}
	if e.FinishedAt != nil {
		out["finished_at"] = *e.FinishedAt
	}
	if in.WithLogs {
		logs := s.execs.Logs(in.ID)
		if in.Tail > 0 && len(logs) > in.Tail {
			logs = logs[len(logs)-in.Tail:]
		}
		out["logs"] = logs
	}
	return nil, out, nil
}
