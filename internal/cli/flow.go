package cli

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"gear/internal/engine"
	"gear/internal/model"
	"gear/internal/registry"
)

func flowCmd() *cobra.Command {
	c := &cobra.Command{Use: "flow", Short: "管理 Flow（通用运行单元）"}
	c.AddCommand(flowRegisterCmd(), flowListCmd(), flowRunCmd(), flowValidateCmd(), flowInspectCmd(), flowDeleteCmd())
	return c
}

func flowRegisterCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "register <file.yaml>",
		Short: "从 YAML 注册/更新 Flow（路径写 - 表示从 stdin 读；支持 --- 分隔的多文档）",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, err := readFileOrStdin(args[0])
			if err != nil {
				return err
			}
			// 多文档解析走 registry.ParseYAMLDocs 单一实现（§0 单一真相源）。
			// 此前用 yaml.Unmarshal 直接解单文档 —— 多文档文件只读第一个文档、
			// 其余静默丢弃；且「第一个文档是注释块」的多文档示例会误报成 Tool。
			tools, flows, err := registry.ParseYAMLDocs(raw)
			if err != nil {
				return err
			}
			if len(flows) == 0 {
				// 常见误用：把 Tool YAML 交给 flow register。在打开 registry 之前拦下——
				// 既给出可操作提示，也避免在用户误指定的库里建出 gear 注册表（tools/flows/hooks）。
				if len(tools) > 0 {
					return fmt.Errorf("这看起来是一个 Tool（含 runtime/entry、无 steps），请改用 `gear tool register %s`", args[0])
				}
				return fmt.Errorf("没有从 %s 解析出任何 Flow 定义", args[0])
			}
			if len(tools) > 0 {
				// 文件里 Tool/Flow 混放：flow register 只接受纯 Flow 文件。
				return fmt.Errorf("%s 同时包含 Tool 与 Flow（解析出 %d 个 Tool、%d 个 Flow）："+
					"flow register 只接受纯 Flow 文件，请拆分后分别注册，或改用 `gear serve --seed %s` 一次灌入",
					args[0], len(tools), len(flows), args[0])
			}
			// 逐项给出「至少需要一个 step」的可操作错误，而不是等静态校验抛出泛化信息。
			for _, f := range flows {
				if f.ID == "" {
					return fmt.Errorf("flow.id 不能为空")
				}
				if len(f.Steps) == 0 {
					return fmt.Errorf("flow %s 至少需要一个 step", f.ID)
				}
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
			eng, err := cliEngine(reg)
			if err != nil {
				return err
			}
			// 两阶段：先全量静态校验（阶段一），全通过才落库（阶段二），避免部分成功。
			for _, f := range flows {
				if err := eng.ValidateFlowStatic(f); err != nil {
					return fmt.Errorf("Flow %s 静态校验失败（本次未写入任何 Flow）: %w", f.ID, err)
				}
			}
			registered := make([]string, 0, len(flows))
			for _, f := range flows {
				if err := reg.UpsertFlow(f, act()); err != nil {
					return fmt.Errorf("注册 Flow %s 失败（已成功写入 %v，请先处理再重试）: %w", f.ID, registered, err)
				}
				registered = append(registered, f.ID)
			}
			fmt.Printf("✓ 已注册 %d 个 Flow: %s\n", len(registered), strings.Join(registered, ", "))
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
	var varFlags []string
	var inputJSON, inputFile string
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
			eng, err := cliEngine(reg, engine.WithMaxParallel(maxParallel))
			if err != nil {
				return err
			}
			// vars 的四种来源见 input.go（含 stdin 管道）。
			vars, err := resolveInput(inputSpec{
				jsonFlag: "--input", fileFlag: "--input-file", kvFlag: "--var",
				jsonVal: inputJSON, fileVal: inputFile, kv: varFlags,
			})
			if err != nil {
				return err
			}
			if len(vars) > 0 {
				// 覆盖 Flow 声明的 vars（本次运行有效，不写回 registry）。
				_, err := eng.RunFlowWithVars(args[0], vars)
				return err
			}
			return eng.RunFlow(args[0])
		},
	}
	c.Flags().IntVar(&maxParallel, "max-parallel", engine.DefaultMaxParallel,
		"单个 Flow 内同时执行的 step 数上限（背压；1=退化为串行）")
	c.Flags().StringArrayVar(&varFlags, "var", nil,
		"覆盖 Flow 变量（可重复）：--var city=北京 --var n=3（值按 JSON 解析，否则当字符串）")
	c.Flags().StringVar(&inputJSON, "input", "", `以 JSON 对象一次性传入 vars；写 "-" 表示从 stdin 读`)
	c.Flags().StringVar(&inputFile, "input-file", "", `从文件读取 vars JSON；写 "-" 表示从 stdin 读`)
	return c
}

// parseKVFlags 把「可重复的 k=v 标志」（如 flow run 的 --var、tool run 的 --arg）解析成 map。
// 值先按 JSON 解析（得到数字/布尔/null/数组/对象），失败则原样作字符串——
// 这样 `--var n=3` 是数字、`--var city=北京` 是字符串。k 为空或缺 `=` 即报错（不静默）。
func parseKVFlags(flagName string, pairs []string) (map[string]interface{}, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	out := make(map[string]interface{}, len(pairs))
	for _, p := range pairs {
		k, v, ok := strings.Cut(p, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" {
			return nil, fmt.Errorf("%s 需为 k=v 形式，收到 %q", flagName, p)
		}
		var parsed interface{}
		if err := json.Unmarshal([]byte(v), &parsed); err == nil {
			out[k] = parsed
		} else {
			out[k] = v
		}
	}
	return out, nil
}

// flowValidateCmd 仅做静态校验、不落库（与 REST POST /api/flows 共用 ValidateFlowStatic）。
func flowValidateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "validate <file.yaml>",
		Short: "静态校验一个 Flow（仅校验，不注册；路径写 - 表示从 stdin 读）",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, err := readFileOrStdin(args[0])
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
			eng, err := cliEngine(reg)
			if err != nil {
				return err
			}
			if err := eng.ValidateFlowStatic(&f); err != nil {
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

// flowDeleteCmd 注销一个 Flow，复用 registry.DeleteFlow 的 §8.2 守卫
// （被其它 Flow 的 step 引用 / 被 Hook 的 action 引用 / 运行中引用均拒，
// 错误经 ErrInUse / ErrNotFound 向上传递）。
//
// 为什么补这一个子命令：删除能力此前只存在于 REST（DELETE /api/flows/{id}）、MCP
// 与 Web UI，CLI 面只有 `gear tool unregister`——于是「删掉一个 Flow」在命令行上
// 是做不到的，只能去 UI 点按钮。补齐后 CLI 与另外三个面同一套守卫，不存在「CLI 能绕
// 过的删除」。删除本身会留痕（registry 在同一事务里先快照再删，note=delete），可回滚。
func flowDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "delete <id|pattern> [...]",
		Short: "注销 Flow（id 支持 * ? [...] 通配符；被引用 / 运行中则拒）",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			reg, err := openRegistry()
			if err != nil {
				return err
			}
			defer reg.Close()
			// 与 `gear tool unregister` 同一套展开规则（globid.go）：只匹配现存 id、
			// 保序去重、空集报错、命中集里任一被 §8.2 拒则整批失败。
			ids, err := expandDeleteTargets(args, func(p string) ([]string, error) {
				all, e := flowIDs(reg)
				if e != nil {
					return nil, e
				}
				return globMatch(p, all)
			})
			if err != nil {
				return err
			}
			return deleteAll("Flow", ids, func(id string) error {
				return reg.DeleteFlow(id, act())
			})
		},
	}
}
