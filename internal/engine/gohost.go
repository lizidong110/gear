package engine

import (
	"context"
	"fmt"
	"sync"

	"gear/internal/cap"
)

// HostFunc 是 go-host runtime 注册的「进程内 Go 函数」：接收执行 ctx、已解析的 input、
// 以及宿主能力视图 host（M5 6 原语，按 Tool.Capabilities 精确授权），返回 Tool 的 output。
//
// 与 process/lua 不同，go-host 是**同进程可信代码**：由部署方在启动时经 RegisterHostFunc
// 显式登记，entry 即函数名。它可直接调用 host.HTTP/KV/Call/... 使用宿主原语，
// 也享有「声明即生产可用」（不依赖子进程/解释器）。
//
// 安全边界：go-host 函数拥有进程内原生权限，capability 最小权限由 host 视图约束但无法像
// 沙箱那样物理隔离 —— 这属已知 P0-4 风险（#79，显式 deferred）。
type HostFunc func(ctx context.Context, input map[string]interface{}, host *cap.Context) (map[string]interface{}, error)

var (
	hostFuncMu sync.RWMutex
	hostFuncs  = map[string]HostFunc{}
)

// RegisterHostFunc 登记一个 go-host 函数（部署期调用，通常 main / CLI 初始化时一次性注册）。
// 同名覆盖（后注册者胜），与「单一入口表」取向一致。
//
// 注意：注册发生在执行前（启动期），运行期只读，故 RWMutex 仅防极端并发注册，不引入热路径锁。
func RegisterHostFunc(name string, f HostFunc) {
	hostFuncMu.Lock()
	defer hostFuncMu.Unlock()
	hostFuncs[name] = f
}

// HostFuncFor 按名查注册函数（go-host runtime 寻址；未注册返回 false）。
func HostFuncFor(name string) (HostFunc, bool) {
	hostFuncMu.RLock()
	defer hostFuncMu.RUnlock()
	f, ok := hostFuncs[name]
	return f, ok
}

// goHostRuntime 实现 Runtime 接口：进程内 Go 函数分发（PRD §3.2，REQ-RUNTIME-GOHOST / #76）。
type goHostRuntime struct{}

// Run 按 entry 查注册函数并执行；未注册 → 明确报错（不让用户猜「为什么没反应」）。
func (g *goHostRuntime) Run(ctx context.Context, entry string, input map[string]interface{}, host *cap.Context) (map[string]interface{}, error) {
	f, ok := HostFuncFor(entry)
	if !ok {
		return nil, fmt.Errorf("go-host 入口 %q 未注册（请用 engine.RegisterHostFunc 登记后重启）", entry)
	}
	out, err := f(ctx, input, host)
	if err != nil {
		return nil, err
	}
	if out == nil {
		out = map[string]interface{}{}
	}
	return out, nil
}

// Start 长驻模式：go-host 不支持（与 lua 一致，REQ-SVC-RUNTIME deferred）。
// 长驻服务请用 process runtime。
func (g *goHostRuntime) Start(ctx context.Context, entry string, input map[string]interface{}, host *cap.Context) (Process, error) {
	return nil, fmt.Errorf("go-host runtime 不支持 service（长驻）模式；长驻服务请用 process runtime")
}
