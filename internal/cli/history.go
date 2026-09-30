package cli

import (
	"os/user"
	"time"

	"github.com/spf13/cobra"

	"gear/internal/registry"
)

// ④「留旧/回滚」的**读**入口。
//
// 为什么只给读、回滚走 gear_apply 的 restore_from：
// gear_apply 已经是唯一的写入口（原子、幂等、冲突语义明确），再在 CLI 开一个
// `gear history restore` 就是第二个写入口——两个写入口各写一份回滚，日后必然漂移。
// 所以这里只做「让人/智能体找到 seq」这件事，写回只有一个实现。
//
// 本命令在 root.go 里**逐项**登记（`historyCmd()`），与 export/import 同理：
// tools/doc_audit.py 的 CLI 双向核对按 `AddCommand(...Cmd()...)` 文本提取，
// 藏进聚合函数会让命令从审计里消失（同类陷阱：枚举式声明必须逐项登记）。

func historyCmd() *cobra.Command {
	var kind, id string
	var limit int
	c := &cobra.Command{
		Use:   "history",
		Short: "查看定义变更历史（回滚用）",
		Long: "查看对象定义被改动的先后顺序（definition_history），供回滚挑选 seq。\n\n" +
			"回滚本身走 `gear_apply(restore_from=<seq>)`（MCP 面），这里只读不写。\n\n" +
			"一行的含义是「那时它是这样的」，不是「那时它被改成了」：想回到哪一步，就把那一步的\n" +
			"seq 填进 restore_from。seq=1 那行通常是「当时还不存在」，回滚它等价于撤回这次新建。",
		RunE: func(cmd *cobra.Command, args []string) error {
			reg, err := openRegistry()
			if err != nil {
				return err
			}
			defer reg.Close()
			rows, err := reg.ListHistory(kind, id, limit)
			if err != nil {
				return err
			}
			if len(rows) == 0 {
				cmd.Println("（暂无变更历史）")
				return nil
			}
			for _, h := range rows {
				ts := time.UnixMilli(h.At).Format("01-02 15:04:05")
				note := h.Note
				if note == "" {
					note = "—"
				}
				cmd.Printf("#%-4d %s %-28s %s  %s\n", h.Seq, ts, h.Kind+" "+h.ID, actorOf(h.Actor), note)
			}
			cmd.Printf("\n回滚：gear_apply(restore_from=<seq>)\n")
			return nil
		},
	}
	c.Flags().StringVar(&kind, "kind", "", "只看某类对象：tool | flow | hook | app")
	c.Flags().StringVar(&id, "id", "", "只看某个 id")
	c.Flags().IntVar(&limit, "limit", 20, "最多显示几条（新的在前）")
	return c
}

// cliActor 是 CLI 写入时记入 definition_history.actor 的主体：跑这条命令的**操作系统用户**。
//
// 为什么不取更“权威”的东西：gear 没有自己的用户库（§3.5 明说 gear 不当 IdP），
// 而 `whoami` 是这条命令真实归属——足够回答「谁把这版流水线的 step 改了」，
// 又不至于假装有登录体系。取不到时返回空串=无主体，不编造名字。
func cliActor() string {
	if u, err := user.Current(); err == nil && u != nil && u.Username != "" {
		return u.Username
	}
	return ""
}

// act 是「本次 CLI 变更」的 actor 选项，所有面向人的 CLI 写入点都该带上它。
//
// 刻意做成短名字 + 一个函数而不是在每个调用点写 `registry.WithActor(cliActor())`：
// CLI 的写入点有十来处（tool/flow/hook/app 各自的 set 与 delete、serve 的重新对齐、
// promote），每处长这一串只会有人偷懒不写——漏一处就是一条无主变更。
func act() registry.UpsertOption { return registry.WithActor(cliActor()) }

// actorOf 把历史里的 actor 渲染成人读得到的形态：空串是「当时没有主体信息」
// （CLI / apply 批量写入等无调用方场景），显示成 "-" 而不是假装有个匿名用户。
func actorOf(actor string) string {
	if actor == "" || actor == "-" {
		return "-"
	}
	return actor
}

// 编译期守卫：本命令依赖的 registry 历史 API 还在（改名会让这里红，而不是运行期找不到）。
var _ = registry.WithActor
