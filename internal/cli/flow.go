package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"gear/internal/engine"
	"gear/internal/model"
)

func flowCmd() *cobra.Command {
	c := &cobra.Command{Use: "flow", Short: "管理 Flow（通用运行单元）"}
	c.AddCommand(flowRegisterCmd(), flowListCmd(), flowRunCmd(), flowValidateCmd(), flowInspectCmd())
	return c
}

func flowRegisterCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "register <file.yaml>",
		Short: "从 YAML 注册/更新一个 Flow",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, err := os.ReadFile(args[0])
			if err != nil {
				return err
			}
			var f model.Flow
			if err := yaml.Unmarshal(raw, &f); err != nil {
				return fmt.Errorf("解析 YAML 失败: %w", err)
			}
			if f.ID == "" {
				return fmt.Errorf("flow.id 不能为空")
			}
			reg, err := openRegistry()
			if err != nil {
				return err
			}
			defer reg.Close()
			// 注册期静态校验：与 REST `POST /api/flows` 共用同一份规则
			// （结构 → 依赖图 → 模板引用 → when 可编译 → Tool 存在）。
			// 此前 CLI 只做了 when 编译，导致「REST 拒绝、CLI 放行」的不一致；
			// 校验规则只能有一处（§0 单一真相源）。
			if err := engine.New(reg).ValidateFlowStatic(&f); err != nil {
				return err
			}
			if err := reg.UpsertFlow(&f); err != nil {
				return err
			}
			fmt.Printf("✓ 已注册 Flow: %s (%s), steps=%d\n", f.ID, f.Name, len(f.Steps))
			return nil
		},
	}
}

func flowListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "列出所有 Flow",
		RunE: func(cmd *cobra.Command, args []string) error {
			reg, err := openRegistry()
			if err != nil {
				return err
			}
			defer reg.Close()
			flows, err := reg.ListFlows()
			if err != nil {
				return err
			}
			if len(flows) == 0 {
				fmt.Println("（无 Flow）")
				return nil
			}
			for _, f := range flows {
				fmt.Printf("- %-16s %-20s steps=%d\n", f.ID, f.Name, len(f.Steps))
			}
			return nil
		},
	}
}

func flowRunCmd() *cobra.Command {
	var maxParallel int
	c := &cobra.Command{
		Use:   "run <id>",
		Short: "执行一个 Flow（S4：无依赖 step 并行 + Hook + schema 校验）",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			reg, err := openRegistry()
			if err != nil {
				return err
			}
			defer reg.Close()
			return engine.New(reg, engine.WithMaxParallel(maxParallel)).RunFlow(args[0])
		},
	}
	c.Flags().IntVar(&maxParallel, "max-parallel", engine.DefaultMaxParallel,
		"单个 Flow 内同时执行的 step 数上限（背压；1=退化为串行）")
	return c
}

// flowValidateCmd 仅做静态校验、不落库（与 REST POST /api/flows 共用 ValidateFlowStatic）。
func flowValidateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "validate <file.yaml>",
		Short: "静态校验一个 Flow（仅校验，不注册）",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, err := os.ReadFile(args[0])
			if err != nil {
				return err
			}
			var f model.Flow
			if err := yaml.Unmarshal(raw, &f); err != nil {
				return fmt.Errorf("解析 YAML 失败: %w", err)
			}
			reg, err := openRegistry()
			if err != nil {
				return err
			}
			defer reg.Close()
			if err := engine.New(reg).ValidateFlowStatic(&f); err != nil {
				return fmt.Errorf("校验失败: %w", err)
			}
			fmt.Printf("✓ Flow %s 校验通过\n", f.ID)
			return nil
		},
	}
}

// flowInspectCmd 查看 Flow 详情（类比 tool show）。
func flowInspectCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "inspect <id>",
		Short: "查看一个 Flow 详情",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			reg, err := openRegistry()
			if err != nil {
				return err
			}
			defer reg.Close()
			f, err := reg.GetFlow(args[0])
			if err != nil {
				return err
			}
			if f == nil {
				return fmt.Errorf("Flow 不存在: %s", args[0])
			}
			out, _ := yaml.Marshal(f)
			fmt.Print(string(out))
			return nil
		},
	}
}
