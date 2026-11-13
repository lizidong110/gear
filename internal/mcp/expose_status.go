package mcp

// expose_status.go：让智能体在 MCP 会话内**自查**「为什么我的策展工具没出现」。
//
// 背景（REQ-MCP-EXPOSE-DISCOVER）：`--expose` 是 MCP 服务端的启动参数，智能体在会话里
// 既开不了、也看不见。此前它写完 `mcp.tools` 却等不到 tools/list 变化时，手上没有任何
// 判据，只能在 YAML 里反复瞎改——与「环境没准备好却重写配置」是同一类最贵的误导。
// 两面补齐：
//   - gear_doctor 附加 `mcp` 块（本会话实际装配状态 + 可执行的下一步）；
//   - gear_validate 对「声明了 Tool 却没写策展区」给 advisory warning（不影响 valid）。
//
// ⚠ 为什么不把 mcp 块塞进 internal/doctor.Check：doctor 的判据本体是 CLI `gear doctor`
// 与 MCP `gear_doctor` **共用的唯一一份实现**，而 CLI 侧根本没有 `--expose` 这个概念。
// 往 Options 里塞会让两面结论长出不同字段，反而破坏「两面必然一致」。这里附加的是
// 「MCP transport 自身的装配元数据」，不属于环境体检结论，故只在 handleDoctor 里补一次。

import (
	"fmt"
	"strings"
)

// mcpExposureStatus 返回当前 MCP 会话的反向暴露装配状态。
//
// 字段名刻意做成机器可读（tools_enabled / flows_enabled / scope / curated_tools），
// hint 则给可直接照做的下一步——AI 需要的不是一句「未开启」，而是「谁、改哪里、重连」。
func (s *Server) mcpExposureStatus() map[string]any {
	mode := string(s.expose)
	if mode == "" {
		mode = "none"
	}
	// 空集合必须是 [] 而不是 null：消费方 .length 在 null 上会拿到 undefined，
	// 把「一个都没装配」误判成「字段没给」（Lua/JS 侧真实事故口径）。
	names := make([]string, 0, len(s.exposedToolNames))
	names = append(names, s.exposedToolNames...)

	hint := ""
	switch {
	case !s.expose.wantsTools():
		hint = fmt.Sprintf("当前会话未开启 Tool 反向暴露（expose=%s）：App 清单的 mcp.tools "+
			"不会出现在 tools/list，这是白名单制的默认行为，不是清单写错。"+
			"--expose 是 MCP 服务端启动参数，AI 会话内无法开启；需由人在 MCP 客户端配置 "+
			"`gear mcp --expose tools --scope <appId>` 并重连。期间仍可用 gear_run(kind=tool) 调用注册 Tool。"+
			"清单写法见 gear_guide(\"authoring\")。", mode)
	case len(names) == 0:
		// 理论不可达：wantsTools 时 registerExposedTools 要么装出 ≥1 个工具，要么让 New 失败。
		// 留一条显式文案，避免将来有人改了装配次序后这里静默给空 hint。
		hint = fmt.Sprintf("expose=%s / scope=%q 已开启，但本次会话没有装配出任何策展工具；"+
			"检查服务端启动日志里的装配错误（无 scope / App 未注册 / 无 mcp.tools 会直接拒绝启动）。", mode, s.scope)
	default:
		hint = fmt.Sprintf("已按 App %q 的 mcp.tools 白名单装配 %d 个工具：%s。"+
			"若客户端仍看不到，检查它是否缓存了旧的 tools/list（重连刷新），"+
			"或 --scope 是否指向了别的应用。", s.scope, len(names), strings.Join(names, ", "))
	}

	return map[string]any{
		"expose":        mode,
		"tools_enabled": s.expose.wantsTools(),
		"flows_enabled": s.expose.wantsFlows(),
		"scope":         s.scope,
		"curated_tools": names,
		"hint":          hint,
	}
}

// mcpCurationWarning 是 gear_validate 的 advisory warning（**不影响 valid**）。
//
// 触发条件刻意收窄：bundle 里有 Tool、且 App 清单没写 mcp.tools。没有 Tool 的纯 Flow 应用
// 不该被刷屏；写了策展区也不该再提醒。文案必须同时说清三件事，否则 Advisory 会变成误导：
// ① 默认不暴露是刻意的；② 想暴露要补清单 + 由人改启动参数并重连；③ 不补也合法。
func mcpCurationWarning(a *appManifest, toolCount int) string {
	return fmt.Sprintf("[MCP_CURATION_ABSENT] App %s 本次声明了 %d 个 Tool，但 kind: App 清单没有 "+
		"mcp.tools 策展区：默认白名单制下它们不会出现在 tools/list（刻意设计，不是漏注册）。"+
		"若希望 MCP 客户端直接调用，在清单补 mcp.tools（ref / alias / args / description，见 "+
		"gear_guide(\"authoring\")），再由人在 MCP 客户端把服务端启动成 "+
		"`gear mcp --expose tools --scope %s` 并重连——这是服务端启动参数，AI 会话内改不了。"+
		"不补也完全合法：仍可用 gear_run(kind=tool) 调用这些 Tool。", a.ID, toolCount, a.ID)
}
