package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"gear/internal/artifact"
	"gear/internal/cap"
	"gear/internal/gearhome"
)

// 本文件的两个命令在 root.go 里**逐项**登记（`exportCmd(), importCmd()`）而不是走一个
// 聚合函数：tools/doc_audit.py 的 CLI 双向核对按 `AddCommand(...Cmd()...)` 文本提取，
// 聚合会让两个命令从审计里消失（同类陷阱：枚举式声明必须逐项登记）。

// exportCmd 把一个应用导出成可移植产物。
func exportCmd() *cobra.Command {
	var format, staticRoot string
	var noStatic bool
	c := &cobra.Command{
		Use:   "export <app-id>",
		Short: "把应用导出成可移植产物（目录或 .gearapp 包）",
		Long: "把一个应用导出成自包含的产物：App 清单 + 名下 Tool/Flow/Hook + 声明的静态资源。\n\n" +
			"产物形态：\n" +
			"  dir（默认）目录，含 gear-artifact.yaml / app.yaml / bundle.yaml / 静态资源，可 diff 可 git 版本化\n" +
			"  zip        打成单个 .gearapp 包，便于发送\n\n" +
			"dir 形态的 --out 必须是**不存在的路径或空目录**：gear 不会为一个可重新生成的产物\n" +
			"删掉一个内容未知的目录。zip 形态覆盖同名文件。\n\n" +
			"静态资源按 layout.files 声明的路径、相对 --root（默认当前工作目录）去找。\n" +
			"gear 从不持有这些文件（由调用方的文件工具写），所以「它在哪」只能由你告诉。",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			out, _ := cmd.Flags().GetString("out")
			if strings.TrimSpace(out) == "" {
				return fmt.Errorf("缺少 --out：dir 形态给目标目录，zip 形态给 .gearapp 文件路径")
			}
			reg, err := openRegistry()
			if err != nil {
				return err
			}
			defer reg.Close()
			res, err := artifact.Export(reg, artifact.ExportOptions{
				AppID:      args[0],
				Out:        out,
				Format:     format,
				NoStatic:   noStatic,
				StaticRoot: staticRoot,
			})
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			fmt.Fprintf(w, "已导出 %s → %s（%s）\n", res.AppID, res.Path, res.Format)
			fmt.Fprintf(w, "  组成: tools=%d flows=%d hooks=%d static=%d entry_flow=%s\n",
				len(res.Components.Tools), len(res.Components.Flows),
				len(res.Components.Hooks), len(res.Components.StaticFiles), res.Components.EntryFlow)
			fmt.Fprintf(w, "  静态资源: 已拷 %d 份\n", len(res.Files))
			for _, warn := range res.Warnings {
				fmt.Fprintf(w, "  ⚠ %s\n", warn)
			}
			fmt.Fprintf(w, "  导回：gear import %s\n", res.Path)
			return nil
		},
	}
	c.Flags().String("out", "", "产物输出路径（dir 形态为目录；zip 形态为文件路径，扩展名留空自动补 .gearapp）")
	c.Flags().StringVar(&format, "format", "dir", "产物形态：dir | zip")
	c.Flags().StringVar(&staticRoot, "root", "", "解析 layout.files 相对路径的根目录（默认当前工作目录）")
	c.Flags().BoolVar(&noStatic, "no-static", false, "只导出定义，不拷静态资源（产物不自包含，不能当交付物用）")
	return c
}

// importCmd 把一个产物导进 registry。
func importCmd() *cobra.Command {
	var appsDir string
	var overwrite, dryRun bool
	c := &cobra.Command{
		Use:   "import <path>",
		Short: "把产物导进 registry（目录或 .gearapp 包）",
		Long: "把 gear export 产出的产物导进当前 registry：校验格式版本 → 落库（App→Tool→Flow→Hook）。\n\n" +
			"**静态资源默认不落地**（gear 不持有它们，那是调用方文件工具的职责）。\n" +
			"传 --apps-dir 才落地到 `<dir>/<app-id>/`，并**重写**清单里的 layout 指向实际位置——\n" +
			"否则导进来的应用指向的路径在这台机器上根本不存在。\n\n" +
			"冲突语义与 gear apply 一致：未传 --overwrite 且「已存在但内容不同」→ 整批拒绝（原子）。\n" +
			"先跑 --dry-run 看会动什么。",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			reg, err := openRegistry()
			if err != nil {
				return err
			}
			defer reg.Close()
			// 与 `gear tool register` 同一条前置：注册期校验 capability 时，合法集合里必须
			// 已有 extensions.yaml 登记的 ext.* 名。漏掉这一句，产物里任何一个声明
			// `capabilities: [ext.ai]` 的 Tool（= 任何 agent 应用）都会在导入第一步就报
			// 「未知的 capability」，而同一个定义用 gear_apply 却能过——两条装配路径口径不一。
			if err := initExtensionNames(reg); err != nil {
				return err
			}
			res, err := artifact.Import(reg, artifact.ImportOptions{
				Source:    args[0],
				Overwrite: overwrite,
				DryRun:    dryRun,
				AppsDir:   appsDir,
				// 迁移执行用的库：与 `gear tool register` 那条同一条装配纪律——
				// 缺了它，产物里声明了 migrations 就会在导入期被 fail-closed 拒掉。
				// 相对库名锚到 gearhome.AppDataDir，与 `ctx.db.*` 的口径一致。
				DB: cap.NewMultiDB("", gearhome.AppDataDir()),
			})
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			if dryRun {
				fmt.Fprintf(w, "dry-run：未写入任何内容\n")
			}
			fmt.Fprintf(w, "应用 %s：created=%v updated=%v\n", res.AppID, res.Created, res.Updated)
			if len(res.MigrationsApplied) > 0 {
				if dryRun {
					fmt.Fprintf(w, "  迁移（dry-run，已回滚，未生效）：would run version=%v\n", res.MigrationsApplied)
				} else {
					fmt.Fprintf(w, "  迁移已执行 version=%v\n", res.MigrationsApplied)
				}
			}
			if len(res.StaticLanded) > 0 {
				fmt.Fprintf(w, "  静态资源已落地 %d 份到 %s\n", len(res.StaticLanded), appsDir)
			}
			for _, warn := range res.Warnings {
				fmt.Fprintf(w, "  ⚠ %s\n", warn)
			}
			if !dryRun {
				fmt.Fprintf(w, "  确认：gear app status --app-id %s（起应用用 gear app start）\n", res.AppID)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&overwrite, "overwrite", false, "允许覆盖已存在且内容不同的对象（默认 false：整批拒绝）")
	c.Flags().BoolVar(&dryRun, "dry-run", false, "只汇报将创建/更新什么，一个字节不写")
	c.Flags().StringVar(&appsDir, "apps-dir", "", "静态资源落地根目录；留空表示不落地")
	return c
}
