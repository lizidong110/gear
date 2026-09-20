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
	"fmt"

	"github.com/spf13/cobra"

	"gear/internal/ext"
)

// extCmd 构造 `gear ext <name>` 子命令。
func extCmd() *cobra.Command {
	return &cobra.Command{
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
}
