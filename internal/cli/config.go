package cli

import (
	"fmt"
	"sort"

	"github.com/spf13/cobra"

	"gear/internal/cap"
	"gear/internal/gearhome"
)

// configCmd 管理 gear 的持久化配置（config 原语）。
//
// 存在的理由（2026-09-25）：此前「让 AI 用上某个模型/key」只有两条路——每次调用带参数，
// 或给每个进程 export GEAR_AI_*。前者要求所有入口都记得传，后者在「别人派生我」的场景
// （MCP 客户端、app 监管进程）根本传不进去。配置库把它变成「写一次、所有进程生效」。
//
// 应用侧同理：声明 capabilities: [config] 即可 ctx.config.get/set，不必把 DSN 硬编码进 YAML
// （mindstorm 的 mindstorm_data.yaml 里曾有 18 处 `local DSN = "D:/…/mindstorm.db"`）。
func configCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "config",
		Short: "读写持久化配置（config 原语）",
		Long: "读写 gear 的持久化键值配置，落点 <GEAR_HOME>/config.db（可用 GEAR_CONFIG_DB 覆盖）。\n\n" +
			"两个用途：\n" +
			"  1. AI 缺省配置：写 ai.provider / ai.model / ai.base_url / ai.api_key / ai.max_tokens，\n" +
			"     此后 ext.ai 的所有调用自动带上，不必再 export GEAR_AI_*。优先级链是\n" +
			"     「调用参数 > 环境变量 > 配置库 > 内置默认」——环境变量仍可临时覆盖。\n" +
			"  2. 应用配置：声明 capabilities: [config] 的 Tool 可在 Lua 里 ctx.config.get/set，\n" +
			"     落点由宿主决定，应用不必知道库在哪。\n\n" +
			"含 api_key / token / password / secret 的键在 list 时默认脱敏；要看明文用 get 或 --show-secrets。",
	}
	c.AddCommand(&cobra.Command{
		Use:   "get <key>",
		Short: "读取一项配置（不存在即报错）",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withConfig(func(s *cap.ConfigStore) error {
				v, err := s.Get(args[0])
				if err != nil {
					return err
				}
				fmt.Fprintln(cmd.OutOrStdout(), v)
				return nil
			})
		},
	})
	c.AddCommand(&cobra.Command{
		Use:   "set <key> <value>",
		Short: "写入一项配置（已存在则覆盖）",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withConfig(func(s *cap.ConfigStore) error {
				if err := s.Set(args[0], args[1]); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "已写入 %s\n", args[0])
				return nil
			})
		},
	})
	c.AddCommand(&cobra.Command{
		Use:   "delete <key>",
		Short: "删除一项配置（不存在即报错）",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withConfig(func(s *cap.ConfigStore) error {
				if err := s.Delete(args[0]); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "已删除 %s\n", args[0])
				return nil
			})
		},
	})
	listC := &cobra.Command{
		Use:   "list [前缀]",
		Short: "列出配置（前缀可空 = 全部）",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			prefix := ""
			if len(args) > 0 {
				prefix = args[0]
			}
			show, _ := cmd.Flags().GetBool("show-secrets")
			return withConfig(func(s *cap.ConfigStore) error {
				m, err := s.List(prefix)
				if err != nil {
					return err
				}
				if len(m) == 0 {
					fmt.Fprintln(cmd.OutOrStdout(), "（无配置项）")
					return nil
				}
				keys := make([]string, 0, len(m))
				for k := range m {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				w := cmd.OutOrStdout()
				for _, k := range keys {
					v := m[k]
					if !show && looksSecretKey(k) {
						v = maskSecret(v)
					}
					fmt.Fprintf(w, "%s = %s\n", k, v)
				}
				return nil
			})
		},
	}
	listC.Flags().Bool("show-secrets", false, "显示 api_key/token 等敏感项的明文")
	c.AddCommand(listC)
	c.AddCommand(&cobra.Command{
		Use:   "path",
		Short: "打印配置库落点",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintln(cmd.OutOrStdout(), gearhome.ConfigDBPath())
			return nil
		},
	})
	return c
}

// withConfig 打开配置库执行 fn 并保证关闭。落点与内核/REST 面同源（gearhome.ConfigDBPath），
// 否则「CLI 写进去、服务读不到」这类故障极难排查。
func withConfig(fn func(*cap.ConfigStore) error) error {
	s := cap.NewConfigStore(gearhome.ConfigDBPath())
	defer func() { _ = s.Close() }()
	return fn(s)
}

// looksSecretKey / maskSecret 委托 cap 包的唯一实现（REQ-API-CONFIG-CATALOG）。
// REST 管理面也要脱敏，两处各写一份必然漂移，且漂移方向总是「某一边漏脱敏」。
// 这里只保留 CLI 特有的「怎么看清明文」提示尾巴；判定与掩码形态归 cap 管。
func looksSecretKey(k string) bool { return cap.LooksSecretKey(k) }

func maskSecret(v string) string {
	m := cap.MaskSecret(v)
	if m == "" || len(v) < 12 {
		return m
	}
	return m + "（用 config get 看明文）"
}
