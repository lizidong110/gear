package cli

// ext.go 提供 `gear ext <name>`：把内置扩展原语（email / sms / wecom / webhook / rabbitmq /
// kafka …）以 sidecar 形态跑起来。
//
// 它是 ext 原子的**进程载体**：引擎侧（extensions.yaml 的 `cmd`）用
// `gear ext <name>` 拉起子进程，然后按 stdio 协议逐次调用：
//
//	stdin  一行 JSON：{"method":"<方法名>","args":{...}}
//	stdout 末行 JSON：{"result":<结果>} 或 {"error":"<错误信息>"}
//
// 所以本命令自身**不解析业务参数**（参数语义在方法实现里），只做「选扩展 → 接管 stdio」。
// 人工调试：
//
//	echo '{"method":"send","args":{...}}' | gear ext email
//
// 未知扩展名不在本命令里拦截：错误由 ext.Run 统一给出（它持有注册表，能列出可用扩展名），
// 这里再维护一份名单必然会与注册表漂移。

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"gear/internal/engine"
	"gear/internal/ext"
)

// extCmd 构造 `gear ext <name>` 子命令。
//
// 另挂一个 `describe` 子命令（扫描侧车自描述）：它抢占了「名为 describe 的扩展」这个名字，
// 这是**刻意的**——扩展名都以 `ext.` 开头（内核强制），而 `gear ext <name>` 传的是不带前缀的
// 短名，冲突面为零；换来的是「扫描」这个动作有个显然的入口，不必再记一条新命令。
func extCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "ext <name>",
		Short: "运行内置扩展原语 sidecar（如：email/webhook...）",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// 用 cmd.InOrStdin / cmd.OutOrStdout：默认即 os.Stdin/os.Stdout（sidecar 的真实通道），
			// 同时让测试能塞一段假输入、收一段假输出（不必碰真实标准流）。
			if err := ext.Run(args[0], cmd.InOrStdin(), cmd.OutOrStdout()); err != nil {
				return fmt.Errorf("扩展原语 %q 执行失败: %w", args[0], err)
			}
			return nil
		},
	}
	c.AddCommand(extDescribeCmd(), extScanCmd())
	return c
}

// extDescribeCmd 构造 `gear ext describe <ext 名>`：扫描一个侧车的自描述。
//
// 存在意义：自描述是「加载时才发生」的事，出了岔子（侧车没实现 --describe、输出不合法、
// 方法对不上）人看不到过程，只知道「起服失败」。本命令让这件事可以随时手工核对，
// 且**与内核加载走同一个 ProbeSidecarDescribe**——人工看到的就是内核拿到的。
func extDescribeCmd() *cobra.Command {
	var asJSON bool
	c := &cobra.Command{
		Use:   "describe <ext 名>",
		Short: "扫描侧车自描述（跑一次 <cmd> --describe），打印它声明的方法与参数",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			path, err := engine.ResolveExtensionConfigPath()
			if err != nil {
				return err
			}
			specs, err := engine.LoadExtensionConfig(path)
			if err != nil {
				return err
			}
			spec, ok := specs[name]
			if !ok {
				names := make([]string, 0, len(specs))
				for k := range specs {
					names = append(names, k)
				}
				sort.Strings(names)
				return fmt.Errorf("扩展 %q 不在 %s 里（现有: %s）", name, pathOrDefault(path), strings.Join(names, ", "))
			}
			if len(spec.Cmd) == 0 {
				return fmt.Errorf("扩展 %q 没有 cmd，无从扫描", name)
			}
			d, err := engine.ProbeSidecarDescribe(spec.Cmd)
			if err != nil {
				return fmt.Errorf("扫描扩展 %q 失败: %w", name, err)
			}
			out := cmd.OutOrStdout()
			if asJSON {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(d)
			}
			fmt.Fprintf(out, "扩展 %s（%s）\n", name, strings.Join(spec.Cmd, " "))
			if !spec.Describe {
				fmt.Fprintf(out, "注意：该扩展**未**声明 describe: true，加载时仍以 extensions.yaml 的 schemas 为准；"+
					"下面是按需扫描的结果，可用于核对两边是否一致。\n")
			}
			fmt.Fprintf(out, "方法 %d 个：\n", len(d.Methods))
			ms := append([]string(nil), d.Methods...)
			sort.Strings(ms)
			for _, m := range ms {
				s := d.Schemas[m]
				req := []string{}
				for p, ps := range s.Parameters {
					if ps.Required {
						req = append(req, p)
					}
				}
				sort.Strings(req)
				line := fmt.Sprintf("  %-22s %s", m, s.Description)
				if len(req) > 0 {
					line += "（必填: " + strings.Join(req, ", ") + "）"
				}
				fmt.Fprintln(out, line)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "输出原始自描述 JSON（而不是人读摘要）")
	return c
}

func pathOrDefault(p string) string {
	if p == "" {
		return "扩展配置（本机未找到 extensions.yaml）"
	}
	return p
}
