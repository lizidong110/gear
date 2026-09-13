// Package cli 实现 gear 命令行管理面（V1 永久基线，见 PRD §14）。
package cli

import (
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"gear/internal/registry"
)

var registryPath string

// serverAddr 是 REST 执行面地址（exec / ai 等客户端子命令与 serve 共用，作为持久标志）。
// 默认 127.0.0.1:8787；可用环境变量 GEAR_ADDR 覆盖（便于联调远端执行面）。
var serverAddr string

// newRootCmd 构造根命令（供 CLI 入口与测试复用）。
func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "gear",
		Short: "Gear — 插件化自动化编排框架内核",
		Long:  "Gear 内核：引擎 + ctx.* 宿主能力 + app run 启动器。\nTool 是原子能力，Flow 是通用运行单元，服务=长驻不退出的 Flow。",
	}
	root.PersistentFlags().StringVar(&registryPath, "registry", defaultRegistry(),
		"registry 数据库路径（默认 ~/.gear/registry.db）")
	root.PersistentFlags().StringVar(&serverAddr, "addr", defaultAddr(),
		"REST 执行面地址（默认 127.0.0.1:8787，可用 GEAR_ADDR 覆盖）")

	root.AddCommand(toolCmd(), flowCmd(), appCmd(), hookCmd(), serveCmd(), execCmd(), aiCmd())
	return root
}

// defaultAddr 返回 --addr 的默认值：优先环境变量 GEAR_ADDR，否则内置默认。
func defaultAddr() string {
	if v := os.Getenv("GEAR_ADDR"); v != "" {
		return v
	}
	return "127.0.0.1:8787"
}

// Execute 是 CLI 入口。
func Execute() {
	if err := newRootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}

func defaultRegistry() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "gear.db"
	}
	return filepath.Join(home, ".gear", "registry.db")
}

// openRegistry 确保目录存在并打开 registry。
func openRegistry() (*registry.Registry, error) {
	dir := filepath.Dir(registryPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return registry.Open(registryPath)
}
