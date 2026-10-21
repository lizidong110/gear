package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"gear/internal/artifact"
	"gear/internal/cap"
	"gear/internal/gearhome"
)

// applyCmd 声明式落库一个 bundle YAML（多文档：kind: App / Tool / Flow / Hook）。
//
// 与 gear import 的区别：import 吃 gear export 的产物（目录 / .gearapp 包），apply 吃
// 手写 / 脚手架生成的多文档 YAML —— 也就是「从零建应用」的落库命令。两者共用
// artifact.ApplyBundle 唯一一份实现（顺序语义、冲突语义、错误文案完全一致）。
//
// 为什么需要它：scaffold app 生成的 App 清单**不能**走 tool/flow register（那两个命令只写
// Tool/Flow 行），此前唯一落库路径是 MCP gear_apply —— 纯 CLI 用户「写好 App 清单却没地方
// 落库」，正是「导入了 flow 却不是 app」困惑的根因之一。artifact.go 的 import 注释也写着
// 「冲突语义与 gear apply 一致」，本命令把它接出来。
func applyCmd() *cobra.Command {
	var overwrite, dryRun bool
	c := &cobra.Command{
		Use:   "apply <file.yaml>",
		Short: "声明式落库 bundle YAML（App+Tool+Flow+Hook；原子、幂等、可选 dry-run）",
		Long: `apply 把一份多文档 YAML（kind: App / Tool / Flow / Hook，--- 分隔）落进 registry，语义与
MCP gear_apply 一致：

  - 按依赖顺序落库：App → Tool → Flow → Hook（先落被依赖者）
  - 先跑迁移（若有），失败即整包拒绝，一个定义都不写
  - 未传 --overwrite 且「已存在且内容不同」→ 整批拒绝（原子）；内容相同视为幂等更新
  - 校验不过就一个字节都不写

与 gear import 的区别：import 吃 gear export 的产物（目录或 .gearapp 包）；apply 吃
手写 / 脚手架生成的多文档 YAML。一个 bundle 只能放一份 kind: App 清单。

典型用法（从零建应用）：
  gear scaffold app my-app -o app.yaml          # 生成 App 清单骨架
  gear scaffold flow my-app.up -o flow.yaml     # 生成入口 Flow（id 须与 App.entry_flow 对上）
  cat app.yaml flow.yaml > my-app.bundle.yaml   # 拼成 bundle
  gear apply my-app.bundle.yaml                 # 落库
  gear apply --dry-run my-app.bundle.yaml       # 只看会动什么，一个字节不写`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, err := readFileOrStdin(args[0])
			if err != nil {
				return err
			}
			b, diags := artifact.ParseBundle(string(raw))
			if len(diags) > 0 {
				for _, d := range diags {
					fmt.Fprintf(os.Stderr, "✗ [%s] %s\n", d.Code, d.Message)
					if d.Hint != "" {
						fmt.Fprintf(os.Stderr, "  提示: %s\n", d.Hint)
					}
				}
				return fmt.Errorf("bundle 校验未通过（未写入任何内容）；先改 YAML 再试")
			}
			if len(b.Apps)+len(b.Tools)+len(b.Flows)+len(b.Hooks) == 0 {
				return fmt.Errorf("bundle 为空（没解析出任何 App/Tool/Flow/Hook 定义）")
			}
			reg, err := openRegistry()
			if err != nil {
				return err
			}
			defer reg.Close()
			// 先并入 extensions.yaml 里的扩展名，apply 期 capability 校验才放行声明 ext.* 的定义
			// （与 tool/flow register、import 同一条装配纪律；漏掉则声明了 ext.ai 的 Tool 在第一步就报
			// 「未知的 capability」，而同一个定义用 gear_apply 却能过 —— 两条装配路径口径不一）。
			if err := initExtensionNames(reg); err != nil {
				return err
			}
			// 迁移执行用的库：与 import 同口径，相对库名锚到 gearhome.AppDataDir。
			// 仅当 bundle 有 migrations 时被读取；无迁移时传它无害（ApplyBundle 内部按是否声明判定）。
			db := cap.NewMultiDB("", gearhome.AppDataDir())
			w := cmd.OutOrStdout()
			if dryRun {
				created := artifact.DiffExisting(reg, b, false)
				updated := artifact.DiffExisting(reg, b, true)
				fmt.Fprintf(w, "dry-run：未写入任何内容\n")
				fmt.Fprintf(w, "  created=%v\n", created)
				fmt.Fprintf(w, "  updated=%v\n", updated)
				return nil
			}
			created, updated, migrations, err := artifact.ApplyBundle(reg, b, db, overwrite)
			if err != nil {
				return fmt.Errorf("apply 失败（整批回滚，未写入或部分写入已撤销）: %w", err)
			}
			fmt.Fprintf(w, "✓ 已 apply：created=%v updated=%v\n", created, updated)
			if len(migrations) > 0 {
				fmt.Fprintf(w, "  迁移已执行 version=%v\n", migrations)
			}
			fmt.Fprintf(w, "  下一步：gear app start <entry_flow> 起应用（写入 ≠ 运行）\n")
			return nil
		},
	}
	c.Flags().BoolVar(&overwrite, "overwrite", false, "允许覆盖内容不同但 id 已存在的对象（默认 false，整批拒绝）")
	c.Flags().BoolVar(&dryRun, "dry-run", false, "只汇报将创建/更新什么，一个字节不写")
	return c
}
