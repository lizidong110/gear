package engine

import (
	"context"

	"gear/internal/cap"
)

// 调用上下文（IDENTITY_CONTEXT_DESIGN.md §3.4）经由 context 值传给引擎边界。
//
// 为什么 key 定义在本包而不是 cap 包：MCP 与 server 两个执行面都要往 ctx 里写身份，
// 而它们本来就 import engine（反之不成立）——放这里没有循环依赖，放 cap 反而要 engine
// 反向 import cap（cap 不 import engine，是可以的，但会多一条不必要的依赖边）。
//
// ## 现状
//
// 目前**只有引擎内部**会读它：`runWithVars` 与 `RunToolExec` 两处把值写进 runContext.caller。
// 公开的引擎入口（`RunFlowResult` / `RunTool` 等）这轮仍传 context.Background()，
// 语义与改造前完全一致（= 无身份）。下一步把各执行面（REST 解析 bearer、MCP 见设计文档
// §3.5 的「声明式身份」）的 ctx 接进来时，改的只是这若干处调用点——本文件不用动。
type callCtxKey struct{}

// WithCaller 返回携带调用上下文的 ctx。
//
// 它由各执行面的入口调用（REST/MCP/CLI），是身份进入 gear 的**唯一入口**。
func WithCaller(ctx context.Context, c cap.Caller) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, callCtxKey{}, c)
}

// CallerFrom 取回 ctx 里的调用上下文。
//
// 未命中时 ok=false：此时 caller 是零值（Subject 为空 = **无身份**），
// 而不是伪造一个「匿名用户」——Hook 要能区分「没验过」和「验出来是匿名」。
func CallerFrom(ctx context.Context) (cap.Caller, bool) {
	if ctx == nil {
		return cap.Caller{}, false
	}
	c, ok := ctx.Value(callCtxKey{}).(cap.Caller)
	return c, ok
}
