// Package cli 实现 gear 命令行管理面（V1 永久基线，见 PRD §14）。
package cli

import (
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"gear/internal/registry"
)

var registryPath string

// Execute 是 CLI 入口。
func Execute() {
	root := &cobra.Command{
		Use:   "gear",
		Short: "Gear — 插件化自动化编排框架内核",
		Long:  "Gear 内核：引擎 + ctx.* 宿主能力 + app run 启动器。\nTool 是原子能力，Flow 是通用运行单元，服务=长驻不退出的 Flow。",
	}
	root.PersistentFlags().StringVar(&registryPath, "registry", defaultRegistry(),
		"registry 数据库路径（默认 ~/.gear/registry.db）")

	root.AddCommand(toolCmd(), flowCmd(), appCmd(), hookCmd(), serveCmd())

	if err := root.Execute(); err != nil {
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
