package mcp

// tools_app.go：应用生命周期 —— gear_app(start/stop/status/list)。
//
// 这个工具是「通过 MCP 创建应用」的最后一环，也是最容易做错的一环：
//   - **start 派生独立后台进程后立即返回**，本进程**从不持有**它——持有者是那个 `gear app run`
//     自己（PRD §14.1）。判据一句话：**长驻子进程不得出现在 MCP 进程树内**（§9.1 第 5 条）。
//   - 因此它**通常**跨会话存活：WorkBuddy 关掉、MCP Server 退出，应用照跑。这既是它的价值，
//     也意味着**必须提供 stop**——「能活过会话却关不掉」才是真缺陷（§6.2⑤）。
//
// ⚠ 但「跨会话存活」**不是无条件的**（2026-09-24 修正，评审 B-1）：
//   `CREATE_BREAKAWAY_FROM_JOB` 失败时会退化为普通派生，进程仍留在调用方的 Job Object 内，
//   **调用方一退出就被连坐回收**。此时 `StartResult.EscapedJob = false` 并带 `Warning`。
//   所以 start 的返回值**必须**如实透传 `escaped_job` / `warning`，文案也**不能**无条件承诺
//   「跨会话存活」——否则智能体会把「现在能访问」说成「一直能访问」，正是 Instructions
//   七条铁律点名的「智能体最贵的错误」。
//   - 进程归属与台账的完整论证见 MCP_AGENT_DESIGN.md §0.2。

import (
	"context"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"gear/internal/app"
)

// registerAppTools 挂上应用生命周期工具。
func (s *Server) registerAppTools(ms *mcp.Server) {
	mcp.AddTool(ms, &mcp.Tool{
		Name: "gear_app",
		Description: "长驻应用生命周期。start：派生**独立后台进程**后立即返回（是否跨会话存活见返回的 escaped_job）；status：pid + 端口双判据；" +
			"stop：终止并复检端口（**破坏性**，收不干净会报 ORPHAN_APP）；list：全部台账与遗留项；" +
			"prune：清理**确定已终止**的台账项（running/unknown 一律保留，不会误删在跑的应用）。service Flow 只能用这里启动。",
		Annotations:  idempotent("应用生命周期"),
		OutputSchema: outApp,
	}, s.handleApp)
}

// handleApp 分发四个 action。
func (s *Server) handleApp(ctx context.Context, _ *mcp.CallToolRequest, in AppArgs) (*mcp.CallToolResult, any, error) {
	regPath, appsDir := s.appEnv()
	switch strings.ToLower(strings.TrimSpace(in.Action)) {
	case "start":
		if strings.TrimSpace(in.FlowID) == "" {
			return toolError("BAD_ARGUMENT", "start 需要 flow_id",
				"flow_id 是应用入口 Flow（须含 mode: service 的长驻 step），先用 gear_catalog(kind=flow) 确认"), nil, nil
		}
		res, err := s.appStart(in)
		if err != nil {
			return toolErrorFrom("APP_START_FAILED", err), nil, nil
		}
		out := map[string]any{
			"app_id": res.AppID, "flow_id": res.FlowID, "pid": res.PID,
			"state": res.State, "log_path": res.LogPath, "ports": res.Ports,
		}
		// ★ escaped_job / warning 必须透传：它们是「这次启动到底能不能跨会话存活」的**唯一凭据**。
		//   漏报会让智能体把「现在能访问」说成「一直能访问」（评审 B-1），故此处不再是可选项。
		//
		//   键**恒输出**（未探测时为 null）：客户端总该看到这个字段，而不是要靠「键不存在」去猜。
		//   三种取值：true=已脱离（真跨会话）；false=未脱离（随本进程退出）；null=未探测（同样不许承诺）。
		out["escaped_job"] = res.EscapedJob
		if res.Warning != "" {
			out["warning"] = res.Warning
		}
		if res.LastLogs != nil {
			out["last_logs"] = res.LastLogs
		}
		if res.State == string(app.StateStartupFailed) {
			// 短等窗口内进程就退出了：这不是「启动成功」，如实说明并指向日志。
			out["error"] = "进程在短等窗口内退出（见 last_logs）"
			out["hint"] = "常见原因：端口被占、侧车未构建、registry 路径不符；先用 gear_doctor 看环境"
			return nil, out, nil
		}
		// hint 按 escaped_job 分三种：已脱离才敢承诺，未脱离要说破，未探测也不能承诺。
		const stopHint = "停止用 gear_app(action=stop, app_id=…)"
		if res.EscapedJob != nil && !*res.EscapedJob {
			out["hint"] = "⚠ escaped_job=false：派生未脱离调用方进程沙箱，应用会随本进程退出而终止——" +
				"不要向用户承诺它长期可用；" + stopHint
		} else if res.EscapedJob == nil {
			out["hint"] = "未能确认应用是否脱离本进程沙箱（escaped_job 未上报）：不要向用户承诺长期可用；" + stopHint
		} else {
			out["hint"] = "应用已脱离本进程沙箱（escaped_job=true），跨会话存活；" + stopHint
		}
		return nil, out, nil

	case "stop":
		if strings.TrimSpace(in.AppID) == "" {
			return toolError("BAD_ARGUMENT", "stop 需要 app_id",
				"先 gear_app(action=list) 拿 app_id"), nil, nil
		}
		res, err := app.Stop(app.StopOptions{RegistryPath: regPath, AppsDir: appsDir, AppID: in.AppID})
		if err != nil {
			return toolErrorFrom("APP_NOT_FOUND", err), nil, nil
		}
		if res.Code != "" {
			// 没收干净：**不谎报成功**（§6.2⑤ / R3b）。带上 pid 与端口现状，让人能接手。
			return toolError(res.Code,
				"进程未收干净：pid="+strconv.Itoa(res.PID)+"，端口见 ports", res.Hint), nil, nil
		}
		return nil, map[string]any{
			"app_id": res.AppID, "flow_id": res.FlowID, "pid": res.PID,
			"state": res.State, "ports_released": res.PortsReleased, "ports": res.Ports,
		}, nil

	case "status":
		if strings.TrimSpace(in.AppID) == "" {
			return toolError("BAD_ARGUMENT", "status 需要 app_id",
				"先 gear_app(action=list) 拿 app_id；要看全部用 action=list"), nil, nil
		}
		st, err := app.Status(app.StatusOptions{
			RegistryPath: regPath, AppsDir: appsDir, AppID: in.AppID,
			TailLogs: 20, // 状态查询默认带日志尾部：排障时「在跑吗」与「报什么错」往往同时需要
		})
		if err != nil {
			return toolErrorFrom("APP_NOT_FOUND", err), nil, nil
		}
		return nil, map[string]any{
			"app_id": st.AppID, "flow_id": st.FlowID, "pid": st.PID, "state": st.State,
			"started_at": st.StartedAt, "uptime_s": st.UptimeS,
			"ports": st.Ports, "log_path": st.LogPath, "last_logs": st.LastLogs,
		}, nil

	case "list":
		res, err := s.appList()
		if err != nil {
			return toolErrorFrom("APP_LIST_FAILED", err), nil, nil
		}
		items := make([]map[string]any, 0, len(res.Items))
		for _, it := range res.Items {
			items = append(items, map[string]any{
				"app_id": it.AppID, "flow_id": it.FlowID, "pid": it.PID, "state": it.State,
				"started_at": it.StartedAt, "ports": it.Ports, "log_path": it.LogPath,
			})
		}
		orphan := make([]map[string]any, 0, len(res.Orphan))
		for _, o := range res.Orphan {
			orphan = append(orphan, map[string]any{
				"app_id": o.AppID, "reason": o.Reason, "pid": o.PID, "log_path": o.LogPath,
			})
		}
		return nil, map[string]any{"items": items, "orphan": orphan, "total": len(items)}, nil

	case "prune":
		// 台账清理：只删**确定已终止**的记录，不杀进程、不碰端口（那是 stop 的职责）。
		pdir := appsDir
		if d, derr := app.ResolveAppsDir(regPath, appsDir); derr == nil {
			pdir = d
		}
		res, err := app.Prune(app.PruneOptions{
			AppsDir: pdir, FlowID: strings.TrimSpace(in.FlowID), DryRun: in.DryRun,
		})
		if err != nil {
			return toolErrorFrom("APP_PRUNE_FAILED", err), nil, nil
		}
		out := map[string]any{
			"dry_run": res.DryRun, "pruned": res.Pruned, "skipped": res.Skipped,
			"pruned_count": len(res.Pruned), "skipped_count": len(res.Skipped),
			"apps_dir": pdir,
		}
		if len(res.Skipped) > 0 {
			out["hint"] = "skipped 里的项仍在跑或状态不确定（unknown 不敢删，删了可能变成无人可查的孤儿）；" +
				"要停它们请先 gear_app(action=stop, app_id=…)"
		}
		return nil, out, nil

	default:
		return toolError("BAD_ARGUMENT",
			"action 必须是 start | stop | status | list | prune（实际: "+in.Action+")",
			"start 起应用、status 查状态、stop 停应用、list 看全部、prune 清理已终止的台账"), nil, nil
	}
}
