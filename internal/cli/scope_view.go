package cli

import (
	"fmt"

	"gear/internal/model"
)

// scopeGroup 是 --scope 下目录列表的分组（REQ-IDENTITY §4.3 的目录面）。
type scopeGroup int

const (
	groupAll      scopeGroup = iota // 无 scope：不过滤（存量行为逐字保持）
	groupOwn                        // 本应用名下（App.Owns 命中）
	groupPlatform                   // 平台级：无主 id（第一段不归属任何 App）
	groupForeign                    // 别人的应用——列表里隐藏
)

// scopeGroupOf 判定单个 id 的分组。口径与引擎层 guardScope 同源：复用
// model.App.Owns（写入期 checkNamespace 用的就是这份规则），不存在「运行期一套、
// 目录面另一套」的漂移。平台级 = 任何 App 都不拥有——它不是别人应用的定义，
// scope 内照常可用（2026-10-21 修复前目录列出它、执行却毙掉它，是最坏的组合）。
func scopeGroupOf(apps []*model.App, id string) scopeGroup {
	if cliScope == "" {
		return groupAll
	}
	if (&model.App{ID: cliScope}).Owns(id) {
		return groupOwn
	}
	for _, a := range apps {
		if a.Owns(id) {
			return groupForeign
		}
	}
	return groupPlatform
}

// printScopedList 按分组渲染目录列表（tool/flow/hook list 共用）。
//
// 为什么分组而不是单纯过滤：--scope 的语义承诺「本进程只认本应用名下的定义」，
// 但平台级对象（ext.serve / static_site.serve / pdf.read.demo）scope 内**确实可用**
// （守卫对无主 id 豁免）——全隐藏会让人误判能力边界，全列出又回到「列出了别的
// 应用」的老问题。分组 + 标注是唯一同时满足两者的口径。
//
// 别人的对象隐藏但**报数**：不静默——「列表为什么短了」必须可回答。
func printScopedList(apps []*model.App, ids []string, render func(id string)) {
	if cliScope == "" {
		for _, id := range ids {
			render(id)
		}
		return
	}
	var own, platform []string
	hidden := 0
	for _, id := range ids {
		switch scopeGroupOf(apps, id) {
		case groupOwn:
			own = append(own, id)
		case groupPlatform:
			platform = append(platform, id)
		default:
			hidden++
		}
	}
	if len(own) > 0 {
		fmt.Printf("── %s 名下（%d）──\n", cliScope, len(own))
		for _, id := range own {
			render(id)
		}
	}
	if len(platform) > 0 {
		fmt.Printf("── 平台级（--scope 内不受守卫，%d）──\n", len(platform))
		for _, id := range platform {
			render(id)
		}
	}
	if hidden > 0 {
		fmt.Printf("（已按 --scope %s 隐藏 %d 个其他应用的对象）\n", cliScope, hidden)
	}
	if len(own)+len(platform) == 0 {
		fmt.Println("（无可见对象）")
	}
}
