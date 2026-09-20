package engine

import (
	"os"
	"strings"
)

// AIMethods 是 ext.ai 暴露给 Lua 的「稳定层」原语（与 Go sidecar 的方法一一对应）。
// 注意：这里**没有** generate_flow / generate_tool —— 那是「易变的 agent 编排」，不进内核、不进 Go，
// 而是写在 Lua（examples/ai-create-flow.yaml 的 ai-gen entry）里，组合下面这些原语。
//
// 能力分层（PRD §9，去内置化）：
//   - complete  = LLM 层（参数化 model/url/key/provider），agent 经它拿文本
//   - catalog   = 能力域内 BM25 工具检索（稳定算法）
//   - domains   = 按 namespace 的能力域概览（纯规则）
//   - validate  = Flow/Tool 静态校验（复用引擎 ValidateFlowStatic / Tool.Validate）
//
// 边界（PRD §9.1）：AI 只生成 DSL（Flow/Tool 定义），绝不运行任意代码、绝不接触密钥、
// 绝不由自身提升/访问 secret——这些仍由引擎与宿主原语把关。
var AIMethods = []string{"complete", "catalog", "domains", "validate"}

// AIExtension 构造「AI 能力」扩展原语（ext.ai），以 sidecar 子进程为载体。
//
// 这是 PRD §9「AI 编排」的去内置化 + 分层落地：AI 不再是引擎内建原语，而是一个可插拔的
// ext.* 扩展；agent 编排逻辑全部活在 Lua（examples/ai-create-flow.yaml），sidecar 只提供
// 上面四个稳定原语。Lua 侧经 ctx.ext.ai.complete/catalog/domains/validate 调用；调用经
// CommandTransport 起子进程转发（cmd 见 DefaultAISidecarCmd）。
//
// Tool 必须声明 capabilities: [ext.ai] 才能拿到 ctx.ext.ai，否则与内置原语同款最小权限门禁
// （ErrCapabilityDenied）。
func AIExtension(cmd []string) *CapabilityExtension {
	return sidecarExtension("ext.ai", &CommandTransport{cmd: cmd}, AIMethods)
}

// DefaultAISidecarCmd 返回 ext.ai sidecar 的默认启动命令：
//   - 优先环境变量 GEAR_AI_SIDECAR（指向编译好的二进制，生产推荐）；
//   - 否则回退到 `go run ./examples/gear-ai-sidecar`（开发期，需在本仓库根目录运行）。
//
// 子进程协议见 examples/gear-ai-sidecar/main.go：stdin 读 {method,args}，stdout 末行写
// {result} 或 {error}。
func DefaultAISidecarCmd() []string {
	if v := strings.TrimSpace(os.Getenv("GEAR_AI_SIDECAR")); v != "" {
		return strings.Fields(v)
	}
	return []string{"go", "run", "./examples/gear-ai-sidecar"}
}
