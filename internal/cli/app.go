package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"gear/internal/app"
)

func appCmd() *cobra.Command {
	c := &cobra.Command{Use: "app", Short: "app run：把 Flow 变成受管进程"}
	c.AddCommand(appRunCmd())
	return c
}

// appRunCmd 启动并监管一个或多个 Flow 进程（S2 实现：长驻 step + restart + 信号停止）。
// 设计见 PRD §14：一个 app run = 一个进程，负责整个 Flow 生命周期；
// 服务 = 长驻不退出的 Flow，并发 = 启动多个 app run（或一次传多个 flow-id 受 maxConcurrent 约束）。
func appRunCmd() *cobra.Command {
	var restart string
	var maxConc int
	c := &cobra.Command{
		Use:   "run <flow-id>...",
		Short: "启动并监管 Flow 进程（S2：长驻 step + restart 策略）",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			reg, err := openRegistry()
			if err != nil {
				return err
			}
			defer reg.Close()

			policy := app.RestartAlways
			switch app.RestartPolicy(restart) {
			case app.RestartNever:
				policy = app.RestartNever
			case app.RestartAlways, "":
				policy = app.RestartAlways
			default:
				return fmt.Errorf("非法的 --restart=%q（应为 always 或 never）", restart)
			}

			ctx, stop := app.ContextWithSignals(context.Background())
			defer stop()

			fmt.Printf("[app run] 监管 %d 个 Flow，restart=%s，maxConcurrent=%d（Ctrl+C 停止）\n",
				len(args), policy, maxConc)
			eng, err := cliEngine(reg)
			if err != nil {
				return err
			}
			sup := app.New(eng, policy, maxConc)
			if err := sup.Supervise(ctx, args); err != nil {
				return err
			}
			fmt.Println("[app run] 已停止")
			return nil
		},
	}
	c.Flags().StringVar(&restart, "restart", "always", "service 退出后的重启策略：always / never")
	c.Flags().IntVar(&maxConc, "max", 0, "同时启动的 Flow 上限（0=不限；S2 并发闸门，进程池留 S4）")
	return c
}
