package engine

import (
	"context"
	"fmt"

	"gear/internal/cap"
	"gear/internal/model"
)

// Runtime 是 Tool 执行运行时适配器接口。引擎按 Tool.Runtime 选择实现。
//
// 两种执行模式都在此接口内，避免 service 路径绕过分发（M10 收口）：
//   - Run：一次性执行（once 模式），等待结束并返回 output。
//   - Start：长驻执行（service 模式），拉起进程**不等待**，返回可监管句柄。
//
// host 是一次执行的宿主能力视图（M5）：由各 Tool 的 Capabilities 精确授权，运行时原语
// （http/call/db/kv/secret/event）经它放行。未声明的原语调用一律被原语自身拒绝（最小权限）。
type Runtime interface {
	// Run 执行一个 Tool：input 为已解析并校验的 JSON 对象，返回 Tool 的 output JSON 对象。
	Run(ctx context.Context, entry string, input map[string]interface{}, host *cap.Context) (map[string]interface{}, error)
	// Start 拉起一个长驻进程但不等待（service 模式）。返回的 Process 由上层监管（Kill/Wait/Pid）。
	Start(ctx context.Context, entry string, input map[string]interface{}, host *cap.Context) (Process, error)
}

// Process 是 service 模式子进程的抽象句柄，避免把 *exec.Cmd 泄漏进接口/上层。
type Process interface {
	Pid() int
	Kill() error
	Wait() error
}

// runtimeFor 按 Tool.Runtime 返回对应适配器。
//
// 「已实现」判定以 model.ImplementedRuntimes 为**唯一来源**（C），本函数不再维护第二份白名单：
// 未知 runtime、已知但未实现的 runtime 都在此显式报错，绝不静默降级为 process。
// runtimeFor 是测试与历史调用的兼容入口：不含扩展原语注册表（reg=nil）。
func runtimeFor(rt model.Runtime, policy string) (Runtime, error) {
	return runtimeForCaps(rt, policy, nil)
}

// runtimeForCaps 是引擎内部统一分发点：携带扩展原语目录 reg，仅 lua runtime 需要它
// （用于把用户/插件注册的 ctx.<name> 注入 Lua）。reg 为 nil 时退化为不含扩展注入。
//
// 「已实现」判定以 model.ImplementedRuntimes 为**唯一来源**（C），本函数不再维护第二份白名单：
// 未知 runtime、已知但未实现的 runtime 都在此显式报错，绝不静默降级为 process。
func runtimeForCaps(rt model.Runtime, policy string, reg *CapabilityRegistry) (Runtime, error) {
	if !rt.Known() {
		return nil, fmt.Errorf("未知的 runtime: %q", rt)
	}
	if !rt.Implemented() {
		return nil, fmt.Errorf("runtime %q 尚未实现（当前支持 process / lua / go-host；wasm 计划 V3）", rt)
	}
	switch rt {
	case model.RuntimeProcess:
		return &processRuntime{}, nil
	case model.RuntimeLua:
		return &luaRuntime{policy: policy, caps: reg}, nil
	case model.RuntimeGoHost:
		return &goHostRuntime{}, nil
	default:
		// 被登记为「已实现」却没有适配器 —— 内部不一致，显式暴露而非跑错分支。
		return nil, fmt.Errorf("runtime %q 已登记为已实现但缺少适配器（内部不一致）", rt)
	}
}
