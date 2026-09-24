// Package httpsafe 提供 Gear 两个 HTTP 面**共用**的请求来源判定。
//
// 为什么单独成包：REST 面（internal/server）与 MCP 面（internal/mcp，streamable HTTP
// transport）都按同一条规则挡跨站请求。这条规则此前只在 REST 面实现（`server.go` 的
// `originAllowed`），MCP 面裸挂 handler 完全没有校验——同一个进程暴露的两个面**安全水位
// 不一致**（2026-09-24 评审 B-2）。两处各写一份必然漂移，故抽到这里共用。
package httpsafe

import (
	"net/http"
	"net/url"
	"strings"
)

// OriginAllowed 判定请求是否来自「本机来源」。
//
// 这么做的理由：Gear 的 REST/MCP 面**当前没有鉴权**，唯一的防线是「只绑 127.0.0.1」。
// 但「绑本机」挡不住浏览器——任意网页都能向 `http://127.0.0.1:<port>/…` 发起请求，
// 浏览器会自动带上 Origin 头。于是「绑回环」这条防线在浏览器面前形同虚设，
// 必须补一条来源判定，否则一个恶意页面就能调用 /api/capabilities/invoke、/api/secrets
// 这类威力极大的端点。
//
// 策略：带 Origin 头且来源不是本机 → 拒绝；**不带 Origin 一律放行**（curl / CLI /
// 服务端调用都不带，它们是本机使用者的正常入口）。这样零配置就能挡住跨站，又不给
// 本机命令行使用增加任何门槛。
func OriginAllowed(r *http.Request) bool {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	switch strings.ToLower(u.Hostname()) {
	case "127.0.0.1", "localhost", "::1":
		return true
	}
	return false
}
