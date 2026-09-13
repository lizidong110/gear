package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"gear/internal/server"
)

func serveCmd() *cobra.Command {
	var addr string
	c := &cobra.Command{
		Use:   "serve",
		Short: "启动 REST 执行面（PRD §11；执行是异步的，POST 返回 202 + execution id）",
		RunE: func(cmd *cobra.Command, args []string) error {
			reg, err := openRegistry()
			if err != nil {
				return err
			}
			defer reg.Close()
			fmt.Printf("Gear REST 服务监听 %s（registry=%s）\n", addr, registryPath)
			return server.New(reg).ListenAndServe(addr)
		},
	}
	c.Flags().StringVar(&addr, "addr", "127.0.0.1:8787", "监听地址")
	return c
}
