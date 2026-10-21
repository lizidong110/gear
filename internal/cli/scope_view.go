package cli

import (
	"fmt"
	"os"

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

// warnScopeUnregistered 在 --scope 指向一个 apps 表里没有行的 App 时，向 stderr
// 显式告警（REQ-CLI-SCOPE-WARN）。运行期（cliEngine 构造点）与目录面
// （printScopedList）共用这一份，不存在「一边喊一边不喊」的漂移。
//
// 为什么必须喊：scope 名打错（或 App 清单还没 apply）时，守卫不报错也不放空——
// 除平台级无主对象外的一切（包括你本想保护的本应用定义，如 scope 打成 wms2 时
// 的 wms__x）都会被判 foreign 误拦。用户看到的是「我的 tool 突然被
// SCOPE_VIOLATION」，很难联想到根因是 scope 名拼错——此前这一切静默发生。
//
// 为什么只喊不拒：「先用 scope 跑起来、App 清单后补」是合法用法（2026-10-21 给
// mindstorm 们补注册前本机就是这样跑的），直接拒绝会切断这条路。告警的职责是
// 把静默变显式，不是改变行为——与 REQ-CLI-DIAG-LOG 红线 2 同口径。
//
// apps 为 nil（调用方只有单个查询结果）时等价于「未注册」。
func warnScopeUnregistered(apps []*model.App) {
	if cliScope == "" {
		return
	}
	for _, a := range apps {
		if a != nil && a.ID == cliScope {
			return
		}
	}
	fmt.Fprintf(os.Stderr,
		"⚠ --scope %s 未注册（registry 里没有这个 App）：守卫将把非平台级对象判为「别人的应用」\n"+
			"  误拦——包括你本想保护的定义；仅无应用前缀的平台级对象可用。\n"+
			"  拼写错误？gear tool list（不带 --scope）可看已注册命名空间；确要启用请先 apply App 清单（docs/app-registration.md）。\n",
		cliScope)
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
	warnScopeUnregistered(apps)
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
