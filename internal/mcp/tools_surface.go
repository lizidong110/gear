package mcp

// tools_surface.go：工具面的**生产侧登记表**（评审 O-8）。
//
// 它此前只活在 `mcp_test.go` 里（纯测试用）。好处是「新增工具不登记即 CI 红」，但它本身
// 也是「工具面有哪些成员 + 谁返回列表形态」的唯一机读声明——放在测试里，生产代码无法复用
// （任何「自描述工具面」的想法都只能再抄一份，而那正是漂移起点）。
//
// 提升为生产常量后：测试改用它做覆盖度反查（语义不变，仍是「不登记即 FAIL」）；
// 未来要报「工具预算用了几个」之类，直接读这里。
//
// ⚠ 表里**每一项都必须与实际注册的工具一一对应**：`TestMCPToolSurfaceMatchesRegistry`
// 双向核对（不多不少），漏一个或有僵尸项都 FAIL。

// toolSurface 是工具面登记表：工具名 → 是否返回**列表形态**
// （structuredContent 下有一个具名数组键，须进线格式红线的 calls 表）。
var toolSurface = map[string]bool{
	"gear_guide":        false,
	"gear_catalog":      true, // items
	"gear_describe":     false,
	"gear_capabilities": true, // capabilities
	"gear_validate":     false,
	"gear_apply":        false,
	"gear_delete":       false,
	"gear_promote":      false,
	"gear_invoke":       false, // 返回单个 result，非列表
	"gear_run":          false,
	"gear_execution":    true, // executions / logs
	"gear_app":          true, // items / orphan
	"gear_doctor":       false,
}

// toolBudget 是工具数的硬预算（机械守卫：超过即 FAIL）。
//
// 为什么有预算：每多一个工具，模型在选路时选错的概率就上升一分（§1「负资产」）。
// 13 = 12（意图级全集）+ 1（评审 I-8 补的 `gear_invoke` 能力直调：它消除了「为一次原子调用
// 付一整轮编排」的成本，收益明确大于一个工具位的代价）。放宽预算必须连同
// MCP_AGENT_DESIGN.md §6.1 的论证一起改。
const toolBudget = 13
