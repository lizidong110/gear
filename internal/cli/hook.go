package cli

import (
	"fmt"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"gear/internal/model"
)

func hookCmd() *cobra.Command {
	c := &cobra.Command{Use: "hook", Short: "管理 Hook（横切控制：改写 / 脱敏 / 限流 / 日志）"}
	c.AddCommand(hookRegisterCmd(), hookListCmd(), hookShowCmd(), hookRemoveCmd())
	return c
}

func hookRegisterCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "register <file.yaml>",
		Short: "从 YAML 注册/更新一个 Hook（路径写 - 表示从 stdin 读）",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, err := readFileOrStdin(args[0])
			if err != nil {
				return err
			}
			var h model.Hook
			if err := yaml.Unmarshal(raw, &h); err != nil {
				return fmt.Errorf("解析 YAML 失败: %w", err)
			}
			reg, err := openRegistry()
			if err != nil {
				return err
			}
			defer reg.Close()
			if err := reg.UpsertHook(&h, act()); err != nil {
				return err
			}
			fmt.Printf("✓ 已注册 Hook: %s, stage=%s, target=%s, action=%s, criticality=%s, order=%d\n",
				h.ID, h.Stage, h.Target, h.Action, h.EffectiveCriticality(), h.Order)
			return nil
		},
	}
}

func hookListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "列出所有 Hook（按 Order 装配顺序）",
		RunE: func(cmd *cobra.Command, args []string) error {
			reg, err := openRegistry()
			if err != nil {
				return err
			}
			defer reg.Close()
			hooks, err := reg.ListHooks()
			if err != nil {
				return err
			}
			if len(hooks) == 0 {
				fmt.Println("（无 Hook）")
				return nil
			}
			byID := make(map[string]*model.Hook, len(hooks))
			ids := make([]string, 0, len(hooks))
			for _, h := range hooks {
				byID[h.ID] = h
				ids = append(ids, h.ID)
			}
			apps, err := reg.ListApps()
			if err != nil {
				return err
			}
			printScopedList(apps, ids, func(id string) {
				h := byID[id]
				fmt.Printf("- %-16s %-12s target=%-14s action=%-14s %s order=%d\n",
					h.ID, h.Stage, h.Target, h.Action, h.EffectiveCriticality(), h.Order)
			})
			return nil
		},
	}
}

func hookShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <id>",
		Short: "查看一个 Hook 详情",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			reg, err := openRegistry()
			if err != nil {
				return err
			}
			defer reg.Close()
			h, err := reg.GetHook(args[0])
			if err != nil {
				return err
			}
			if h == nil {
				return fmt.Errorf("Hook 不存在: %s", args[0])
			}
			out, _ := yaml.Marshal(h)
			fmt.Print(string(out))
			return nil
		},
	}
}

func hookRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "remove <id>",
		Short: "删除一个 Hook",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			reg, err := openRegistry()
			if err != nil {
				return err
			}
			defer reg.Close()
			if err := reg.DeleteHook(args[0], act()); err != nil {
				return err
			}
			fmt.Printf("✓ 已删除 Hook: %s\n", args[0])
			return nil
		},
	}
}
