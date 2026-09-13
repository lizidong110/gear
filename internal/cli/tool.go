package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"gear/internal/model"
)

func toolCmd() *cobra.Command {
	c := &cobra.Command{Use: "tool", Short: "管理 Tool（原子能力）"}
	c.AddCommand(toolRegisterCmd(), toolListCmd(), toolShowCmd(), toolUnregisterCmd())
	return c
}

func toolRegisterCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "register <file.yaml>",
		Short: "从 YAML 注册/更新一个 Tool",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, err := os.ReadFile(args[0])
			if err != nil {
				return err
			}
			var t model.Tool
			if err := yaml.Unmarshal(raw, &t); err != nil {
				return fmt.Errorf("解析 YAML 失败: %w", err)
			}
			if t.ID == "" {
				return fmt.Errorf("tool.id 不能为空")
			}
			reg, err := openRegistry()
			if err != nil {
				return err
			}
			defer reg.Close()
			if err := reg.UpsertTool(&t); err != nil {
				return err
			}
			fmt.Printf("✓ 已注册 Tool: %s (%s), runtime=%s, versions=%d\n", t.ID, t.Name, t.Runtime, len(t.Versions))
			return nil
		},
	}
}

func toolListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "列出所有 Tool",
		RunE: func(cmd *cobra.Command, args []string) error {
			reg, err := openRegistry()
			if err != nil {
				return err
			}
			defer reg.Close()
			tools, err := reg.ListTools()
			if err != nil {
				return err
			}
			if len(tools) == 0 {
				fmt.Println("（无 Tool）")
				return nil
			}
			for _, t := range tools {
				fmt.Printf("- %-16s %-20s [%s] versions=%d\n", t.ID, t.Name, t.Runtime, len(t.Versions))
			}
			return nil
		},
	}
}

func toolShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <id>",
		Short: "查看一个 Tool 详情",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			reg, err := openRegistry()
			if err != nil {
				return err
			}
			defer reg.Close()
			t, err := reg.GetTool(args[0])
			if err != nil {
				return err
			}
			if t == nil {
				return fmt.Errorf("Tool 不存在: %s", args[0])
			}
			out, _ := yaml.Marshal(t)
			fmt.Print(string(out))
			return nil
		},
	}
}

// toolUnregisterCmd 注销 Tool，复用 registry.DeleteTool 的 §8.2 守卫
// （被 Flow step / Hook action 引用、运行中引用均拒，错误经 ErrInUse/ErrNotFound 向上传递）。
func toolUnregisterCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unregister <id>",
		Short: "注销一个 Tool（§8.2 守卫：被引用 / 运行中则拒）",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			reg, err := openRegistry()
			if err != nil {
				return err
			}
			defer reg.Close()
			if err := reg.DeleteTool(args[0]); err != nil {
				return err
			}
			fmt.Printf("✓ 已注销 Tool: %s\n", args[0])
			return nil
		},
	}
}
