package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"gear/internal/server"
)

// serveCmd 启动 REST 执行面。监听地址复用根命令的持久 --addr 标志
// （与 exec / ai 等客户端子命令一致，便于联调同一执行面）。
func serveCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "serve",
		Short: "启动 REST 执行面（PRD §11；执行是异步的，POST 返回 202 + execution id）",
		RunE: func(cmd *cobra.Command, args []string) error {
			reg, err := openRegistry()
			if err != nil {
				return err
			}
			defer reg.Close()
			fmt.Printf("Gear REST 服务监听 %s（registry=%s）\n", serverAddr, registryPath)
			return server.New(reg).ListenAndServe(serverAddr)
		},
	}
	return c
}
