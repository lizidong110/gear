package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"gear/internal/cmdline"
	"gear/internal/model"
	"gear/internal/registry"
)

func toolCmd() *cobra.Command {
	c := &cobra.Command{Use: "tool", Short: "管理 Tool（原子能力）"}
	c.AddCommand(toolRegisterCmd(), toolRunCmd(), toolListCmd(), toolShowCmd(), toolUnregisterCmd(), toolPromoteCmd(), toolVersionsCmd())
	return c
}

func toolRegisterCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "register <file.yaml>",
		Short: "从 YAML 注册/更新 Tool（路径写 - 表示从 stdin 读；支持 --- 分隔的多文档）",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, err := readFileOrStdin(args[0])
			if err != nil {
				return err
			}
			// 多文档解析走 registry.ParseYAMLDocs 单一实现（§0 单一真相源）。
			// 此前用 yaml.Unmarshal 直接解单文档 —— 多文档文件**只读第一个文档，
			// 其余静默丢弃**（exit 0 且打印成功），属静默降级；examples/news-top10.yaml
			// 这类带前导注释块的多文档示例甚至会「注册 0 个 Tool 却报成功」。
			tools, flows, err := registry.ParseYAMLDocs(raw)
			if err != nil {
				return err
			}
			if len(flows) > 0 {
				// 常见误用：把 Flow YAML 交给 tool register，或文件里 Tool/Flow 混放。
				// 在打开 registry 之前拦下（不创建/污染库文件），并区分两种情形给不同提示：
				// 纯 Flow → 引导改用 flow register；Tool+Flow 混合 → 说明需拆开或改用 --seed。
				if len(tools) == 0 {
					return fmt.Errorf("这看起来是一个 Flow（含 steps），请改用 `gear flow register %s`", args[0])
				}
				return fmt.Errorf("%s 同时包含 Tool 与 Flow（解析出 %d 个 Tool、%d 个 Flow）："+
					"tool register 只接受纯 Tool 文件，请拆分后分别注册，或改用 `gear serve --seed %s` 一次灌入",
					args[0], len(tools), len(flows), args[0])
			}
			if len(tools) == 0 {
				return fmt.Errorf("没有从 %s 解析出任何 Tool 定义", args[0])
			}
			reg, err := openRegistry()
			if err != nil {
				return err
			}
			defer reg.Close()
			// 先并入 extensions.yaml 里的扩展名，注册期 capability 校验才放行声明 ext.* 的 Tool。
			if err := initExtensionNames(reg); err != nil {
				return err
			}
			// 两阶段：先全量校验（阶段一），全通过才落库（阶段二）。
			// 否则多文档里「第 1 个成功、第 2 个失败」会留下部分写入（与 MCP create_tool 同款）。
			for _, t := range tools {
				if err := t.Validate(); err != nil {
					return fmt.Errorf("Tool %s 校验失败（本次未写入任何 Tool）: %w", t.ID, err)
				}
			}
			// entry 里给路径类 flag 传了相对路径 → stderr 提示。判据与 MCP 的 gear_validate
			// 共用 internal/cmdline 那一份，两个入口结论必然一致。
			// ⚠ 只提示不阻断：相对值在「就地手跑」时合法，但换个 cwd 就会静默指向别处——
			// 2026-09-22 正是这么踩到「应用自带 REST 面读了另一个库、列表永远为空」的。
			for _, t := range tools {
				for _, f := range cmdline.RelPathFlagsInLine(t.Entry) {
					fmt.Fprintf(os.Stderr, "⚠ %s\n", cmdline.RelPathFlagHint("tool "+t.ID, f))
				}
			}
			registered := make([]string, 0, len(tools))
			for _, t := range tools {
				if err := reg.UpsertTool(t); err != nil {
					return fmt.Errorf("注册 Tool %s 失败（已成功写入 %v，请先处理再重试）: %w", t.ID, registered, err)
				}
				registered = append(registered, t.ID)
			}
			fmt.Printf("✓ 已注册 %d 个 Tool: %s\n", len(registered), strings.Join(registered, ", "))
			return nil
		},
	}
}

// toolRunCmd 一次性执行单个 Tool（原子能力）。语义与「在 Flow 里跑一个 step」完全一致：
// input_schema 校验 → capability 门禁 → runtime 执行 → step 级 Hook → output_schema 校验；
// 会触发按 tool id 匹配的 step 级 Hook，不触发 Flow 级 Hook（无 Flow）。
// 输入四种来源（见 input.go）：--input '<json>' / --input-file <path> / --arg k=v（可重复）
// / **stdin 管道**（`cat in.json | gear tool run t`，或 `--input -` 显式指定）。
func toolRunCmd() *cobra.Command {
	var inputJSON, inputFile string
	var argFlags []string
	c := &cobra.Command{
		Use:   "run <id>",
		Short: "执行一个 Tool（原子能力，一次性）",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			input, err := resolveInput(inputSpec{
				jsonFlag: "--input", fileFlag: "--input-file", kvFlag: "--arg",
				jsonVal: inputJSON, fileVal: inputFile, kv: argFlags,
			})
			if err != nil {
				return err
			}
			reg, err := openRegistry()
			if err != nil {
				return err
			}
			defer reg.Close()
			eng, err := cliEngine(reg)
			if err != nil {
				return err
			}
			out, err := eng.RunTool(args[0], input)
			if err != nil {
				return err
			}
			b, err := yaml.Marshal(out)
			if err != nil {
				return fmt.Errorf("序列化输出失败: %w", err)
			}
			cmd.Printf("✓ Tool %s 执行成功\n%s", args[0], string(b))
			return nil
		},
	}
	c.Flags().StringVar(&inputJSON, "input", "", `输入 JSON 对象，如 --input '{"city":"上海"}'；写 "-" 表示从 stdin 读`)
	c.Flags().StringVar(&inputFile, "input-file", "", `从文件读取输入 JSON；写 "-" 表示从 stdin 读`)
	c.Flags().StringArrayVar(&argFlags, "arg", nil, "单字段输入（可重复）：--arg city=上海（值按 JSON 解析）")
	return c
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

// toolPromoteCmd 晋升某 Tool 版本到目标状态（S5 灰度）。
// 走 engine.PromoteTool：先触发 before_promote Hook 门禁，再落库；状态迁移非法由
// model.CanPromote 在 registry 层校验。
func toolPromoteCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "promote <id> <version> <target>",
		Short: "晋升某 Tool 版本到目标状态（S5 灰度）",
		Args:  cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			target := model.ToolVersionState(args[2])
			if !model.ValidStates[target] {
				return fmt.Errorf("非法目标状态: %q（应为 draft/testing/stable/deprecated/rejected）", args[2])
			}
			reg, err := openRegistry()
			if err != nil {
				return err
			}
			defer reg.Close()
			eng, err := cliEngine(reg)
			if err != nil {
				return err
			}
			if err := eng.PromoteTool(args[0], args[1], target); err != nil {
				return err
			}
			fmt.Printf("✓ Tool %s 版本 %s 已晋升为 %s\n", args[0], args[1], target)
			return nil
		},
	}
}

// toolVersionsCmd 列出某 Tool 的全部版本与状态。
func toolVersionsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "versions <id>",
		Short: "列出某 Tool 的全部版本与状态",
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
			if len(t.Versions) == 0 {
				fmt.Println("（无版本）")
				return nil
			}
			for _, v := range t.Versions {
				fmt.Printf("- %-10s %-12s %s\n", v.Version, v.State, v.Note)
			}
			return nil
		},
	}
}
