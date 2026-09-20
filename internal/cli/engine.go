package cli

import (
	"fmt"

	"gear/internal/cap"
	"gear/internal/engine"
	"gear/internal/registry"
)

// cliEngine 构造 CLI 侧引擎，统一注入扩展原语目录（配置化加载，含内建 ext.registry）。
//
// 去内置化（PRD §9）：AI 不再内建，而是经 ext.ai 扩展原语提供。CLI 作为「应用层」选择
// 注册它——引擎内核只认内核原语（受门禁 7 个 + 纯函数 3 个），扩展由 app 装配。sidecar 二进制缺失时调用
// ctx.ext.ai.* 返回 ErrCapabilityUnavailable（fail-closed），不影响其它能力。
// 扩展来源：extensions.yaml（GEAR_EXT_CONFIG 可覆盖路径），无配置则回退默认 ext.ai。
//
// 密钥源：构造期从 GEAR_SECRET_<NAME> 环境变量播种一份 SecretStore 并注入（engine.WithSecret），
// 与 REST 服务的 secrets 行为一致——由此 CLI/MCP 路径也能用 ctx.secret.get / ${secrets.x}
// （此前缺这步，MCP 路径下 ${secrets.*} 会报「secret 原语未提供」）。
func cliEngine(reg *registry.Registry, opts ...func(*engine.Engine)) (*engine.Engine, error) {
	capReg := engine.NewCapabilityRegistry()
	agent, err := engine.SetupExtensions(capReg, reg, "")
	if err != nil {
		return nil, fmt.Errorf("初始化扩展失败: %w", err)
	}
	secrets := cap.NewSecretStore()
	// 从 GEAR_SECRET_<NAME> 播种，让 MCP/CLI 路径也可用 ${secrets.x}（与 REST 服务的 s.secrets 一致）。
	// 播种失败即 fail-closed：名字非法（空名/含空白/${}）会让该密钥静默缺失——「配了却读不到」
	// 在 CLI/MCP 路径同样极难排查。REST 面（server.New）同口径，两个入口对同一环境变量必须同行为。
	if err := secrets.SeedFromEnv(); err != nil {
		return nil, fmt.Errorf("播种环境变量密钥失败: %w", err)
	}
	opts = append(opts, engine.WithCapabilities(capReg), engine.WithAIAgent(agent), engine.WithSecret(secrets))
	return engine.New(reg, opts...), nil
}

// initExtensionNames 读 extensions.yaml（GEAR_EXT_CONFIG 可覆盖），把其中扩展名并入全局合法
// capability 集合（engine.SetupExtensions → cap.RegisterExtensionName）。
//
// registry 的 UpsertTool 在注册期就用该集合校验 Tool 声明的 capability，所以「先注册、后运行」
// 的 CLI 路径（tool register 等）必须在校验前先调用本函数，否则声明 ext.* 的 Tool 会被判为
// 「未知的 capability」。这正是「发布后用户配好 ext.tls 再注册工具」会撞上的坑。
// 只借用其副作用（全局名集合），返回的 capReg 作用域内即弃。
func initExtensionNames(reg *registry.Registry) error {
	if _, err := engine.SetupExtensions(engine.NewCapabilityRegistry(), reg, ""); err != nil {
		return fmt.Errorf("初始化扩展失败: %w", err)
	}
	return nil
}
