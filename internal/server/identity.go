package server

import (
	"net/http"
	"strings"

	"gear/internal/cap"
)

// 调用主体提取（IDENTITY_CONTEXT_DESIGN.md §3.5：REST 侧身份是**真的**）。
//
// 与 MCP 面截然相反：MCP 的 tool handler 拿不到 `*http.Request`（go-sdk v1.8.0 的
// CallToolParams 里没有请求元信息位），而 REST 的 handler 手里就是 `*http.Request`，
// 能直接读到凭据。所以 gear 的信任模型是：
//
//   - **REST 面 = 真身份**（本文件实现）：执行时注入 `cap.Caller`，Hook 与 ctx.caller 可见。
//   - **MCP 面 = 无身份**：不做任何假装，`gear_run` 拿到的 caller 恒为「无身份」。
//
// 因此「L3 产品的鉴权必须发生在它自己的前门（带 token 的 REST）」——gear 的 MCP 面是
// 管理平面，不是信任边界。这条边界请在产品的文档里照实写，别让智能体以为 gear 验过身份。

const (
	// subjectHeader 是显式主体声明头。
	subjectHeader = "X-Gear-Subject"
)

// callerOf 从请求里提取调用主体。
//
// 取值顺序：`Authorization: Bearer <主体>` → `X-Gear-Subject: <主体>` → 无身份（空串）。
//
// gear **不解析 bearer token 的语义**（没有用户库、没有 JWT 密钥），把它当**不透明主体标识**：
// 「谁持有这个 token」就是「谁」。这是有意的最小实现——身份链路的价值在于「让执行知道谁」，
// 不在于「让 gear 当身份提供者（IdP）」；真 IdP 是应用前门的事。
//
// ⚠ 两个如实说明，别读成「gear 已经做了鉴权」：
//  1. 头不存在时 Subject 为**空串**（= 无身份），不是 "anonymous"——「没有主体」与
//     「有个叫 anonymous 的主体」是两回事，Hook 应当据此拒绝而不是放行。
//  2. X-Gear-Subject 是**调用方自己填的**，可被同机任意进程伪造（这一层挡不住浏览器，
//     见 httpsafe.OriginAllowed）。它只对「事后追责」有意义，不构成访问控制。
func callerOf(r *http.Request) cap.Caller {
	return cap.Caller{Subject: subjectOf(r)}
}

func subjectOf(r *http.Request) string {
	if s := bearerSubject(r.Header.Get("Authorization")); s != "" {
		return s
	}
	if s := strings.TrimSpace(r.Header.Get(subjectHeader)); s != "" {
		return s
	}
	return ""
}

// bearerSubject 解析 `Authorization: Bearer <主体>`，非 Bearer 形态返回空串。
//
// 主体取「Bearer 之后的全部内容」而不是按空格切成第一段：bearer token 本身不含空格，
// 按空格切只会把「填错的多余内容」静默吞掉——静默修正比原样带回更难查，
// 宁可让一个带空格的 token 被当作一个奇怪的主体（Hook 会拒绝，人立刻看得见）。
func bearerSubject(header string) string {
	h := strings.TrimSpace(header)
	if len(h) < 7 || !strings.EqualFold(h[:6], "Bearer") || h[6] != ' ' {
		return ""
	}
	return strings.TrimSpace(h[7:])
}
