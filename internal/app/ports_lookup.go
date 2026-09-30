package app

import (
	"fmt"

	"gear/internal/model"
	"gear/internal/registry"
)

// 本文件实现 REQ-MCP-PORTS-ONE-TRUTH：声明端口的单一真相与回退链。
//
// 此前的口径是「文档说写进 App 清单的 start.ports 就行、实现只认起服入参」——
// AppStart.Ports 全仓零消费点，bundle 里写了等于没写。现在固定为一条回退链：
//
//	显式入参（CLI --port / gear_app ports） > App 清单 start.ports > 无（只按 pid 判存活）
//
// 生效来源通过 StartResult.PortsSource 如实上报（explicit / bundle / none），
// 不让「这次到底用了哪份」变成暗口径。

// 端口来源常量（StartResult.PortsSource 的取值）。
const (
	PortsSourceExplicit = "explicit"
	PortsSourceBundle   = "bundle"
	PortsSourceNone     = "none"
)

// ResolveStartPorts 按「入参 > 清单 > 无」解析本次生效的声明端口。
//
// AppLookup 为 nil 或未命中时返回 none——**这不是错误**：没有 App 清单的裸 Flow 一样能起，
// 只是探活退化成只看 pid，调用方（CLI/MCP）会拿到 none 并可自行提示。
// AppLookup 返回的错误原样传播：读 registry 失败要红，不能静默降级成 none。
func ResolveStartPorts(opts StartOptions) (ports []int, source string, err error) {
	if len(opts.Ports) > 0 {
		return opts.Ports, PortsSourceExplicit, nil
	}
	if opts.AppLookup == nil {
		return nil, PortsSourceNone, nil
	}
	bundle, matched, err := opts.AppLookup(opts.FlowID)
	if err != nil {
		return nil, "", fmt.Errorf("反查 App 清单的 start.ports 失败: %w", err)
	}
	if !matched || len(bundle) == 0 {
		return nil, PortsSourceNone, nil
	}
	return bundle, PortsSourceBundle, nil
}

// AppPortsFromRegistry 是 AppLookup 的标准实现：在 registry 里按 EntryFlow 反查 App。
//
// 返回 (ports, matched, err)。matched 只在「恰好一个 App 命中且声明了端口」时为 true：
// 0 个 = 裸 Flow，多个 = 歧义（多个 App 共用同一 entry_flow 时猜一个，比让调用方显式
// 传更危险），两种情况都不回退。
func AppPortsFromRegistry(regPath, flowID string) (ports []int, matched bool, err error) {
	reg, err := registry.Open(regPath)
	if err != nil {
		return nil, false, err
	}
	defer reg.Close()
	apps, err := reg.ListApps()
	if err != nil {
		return nil, false, err
	}
	var hits []*model.App
	for _, a := range apps {
		if a.EntryFlow == flowID {
			hits = append(hits, a)
		}
	}
	if len(hits) != 1 || len(hits[0].Start.Ports) == 0 {
		return nil, false, nil
	}
	return hits[0].Start.Ports, true, nil
}
