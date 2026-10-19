package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"gear/internal/cap"
	"gear/internal/gearhome"
)

// secretCmd 管理 gear 的持久化密钥（secret 原语）。
//
// 存在的理由（2026-10-02）：此前密钥只有两条入口——REST /api/secrets（需 serve 常驻）
// 或 GEAR_SECRET_<NAME> 环境变量（仅内存、重启即失）。想「写一把密钥让后续 Flow/Tool 稳定用」
// 却没有本机 CLI 等价物，只能 curl 或 export。本子命令与 config 子命令同构：落点 secrets.db，
// 写一次所有进程生效，且重启仍在。
//
// 与 REST 面同一个 secrets.db（gearhome.SecretsDBPath），故 CLI 写进去、serve 读得到，反之亦然。
// 只有 get 回明文；list 只回名字（不回显、不回值）——与「运行期面不回显明文」的红线一致。
func secretCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "secret",
		Short: "管理密钥（secret 原语，落盘 secrets.db）",
		Long: "管理 gear 的持久化密钥（secret 原语），落点 <GEAR_HOME>/secrets.db（可用 GEAR_SECRETS_DB 覆盖）。\n\n" +
			"set 写入/覆盖、delete 删除、list 列出名字（不回明文）、get 读明文。\n" +
			"写入后即持久化，重启 gear serve 仍在；Flow/Tool 经 ${secrets.<NAME>} 或 ctx.secret.get 读取。\n" +
			"运行期面（沙箱）只读且不枚举；本子命令是宿主管理面。",
	}
	c.AddCommand(&cobra.Command{
		Use:   "set <name> <value>",
		Short: "写入/覆盖一个密钥（已存在则覆盖）",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withSecret(func(s *cap.SecretStore) error {
				if err := s.Set(args[0], args[1]); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "已写入 %s\n", args[0])
				return nil
			})
		},
	})
	c.AddCommand(&cobra.Command{
		Use:   "get <name>",
		Short: "读取一个密钥的明文（仅本管理面可回明文）",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withSecret(func(s *cap.SecretStore) error {
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
		Use:   "delete <name>",
		Short: "删除一个密钥（不存在即报错）",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withSecret(func(s *cap.SecretStore) error {
				if err := s.Delete(args[0]); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "已删除 %s\n", args[0])
				return nil
			})
		},
	})
	c.AddCommand(&cobra.Command{
		Use:   "list [前缀]",
		Short: "列出密钥名（不回明文；可加前缀过滤）",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			prefix := ""
			if len(args) > 0 {
				prefix = args[0]
			}
			return withSecret(func(s *cap.SecretStore) error {
				names := s.Names()
				if prefix != "" {
					kept := names[:0]
					for _, n := range names {
						if strings.HasPrefix(n, prefix) {
							kept = append(kept, n)
						}
					}
					names = kept
				}
				if len(names) == 0 {
					fmt.Fprintln(cmd.OutOrStdout(), "（无密钥）")
					return nil
				}
				for _, n := range names {
					fmt.Fprintln(cmd.OutOrStdout(), n)
				}
				return nil
			})
		},
	})
	c.AddCommand(&cobra.Command{
		Use:   "path",
		Short: "打印密钥库落点",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintln(cmd.OutOrStdout(), gearhome.SecretsDBPath())
			return nil
		},
	})
	return c
}

// withSecret 打开密钥库执行 fn 并保证关闭。落点与内核/REST 面同源（gearhome.SecretsDBPath），
// 否则「CLI 写进去、服务读不到」这类故障极难排查（与 config 的 withConfig 同口径）。
func withSecret(fn func(*cap.SecretStore) error) error {
	s := cap.NewSecretStore()
	if err := s.Open(gearhome.SecretsDBPath()); err != nil {
		return err
	}
	defer func() { _ = s.Close() }()
	return fn(s)
}
