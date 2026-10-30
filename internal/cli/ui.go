package cli

import (
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/spf13/cobra"
)

// uiCmd 打开 Gear Web 管理界面（REQ-UI-LAUNCH-CMD）。
//
// 行为：若本机已在 serverAddr 上起了一个 serve（无论是不是本命令起的），直接打开浏览器；
// 否则起一个带 UI 的前台 serve（复用 serveCmd，强制 --no-ui=false），由 serve 自行打印
// GEAR_OPEN_URL 并（默认）自动开浏览器。前台 serve 随 Ctrl+C 退出。
func uiCmd() *cobra.Command {
	var noOpen bool
	c := &cobra.Command{
		Use:   "ui",
		Short: "打开 Gear Web 管理界面（自动起本机 serve 并打开浏览器）",
		Long: "打开 Gear Web 管理界面。\n\n" +
			"若本机已在 --addr 上运行 serve，直接打开浏览器；否则先起一个带 UI 的 serve 再打开。\n" +
			"界面与 REST 执行面同源（默认 http://127.0.0.1:8787/），无需任何额外配置。",
		RunE: func(cmd *cobra.Command, args []string) error {
			addr := serverAddr
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				// addr 形如 ":8787"（无 host 部分）时 SplitHostPort 会失败
				host, port = "", addr
			}
			if port == "" || port == "0" {
				port = "8787"
			}
			// 监听地址可能是全网卡（0.0.0.0 / ::），浏览器只能连回环。
			listenHost := "127.0.0.1"
			if host != "" && host != "0.0.0.0" && host != "::" && host != "127.0.0.1" {
				listenHost = host
			}
			target := net.JoinHostPort(listenHost, port)

			if isPortOpen(target) {
				url := "http://" + target + "/"
				fmt.Printf("检测到本机 serve 已在 %s 运行，直接打开浏览器\n", target)
				if !noOpen {
					_ = openBrowser(url)
				}
				return nil
			}

			// 未运行：起一个带 UI 的前台 serve（复用 serve 逻辑），由它打印地址并开浏览器。
			s := serveCmd()
			_ = s.Flags().Set("no-ui", "false")
			_ = s.Flags().Set("no-open", strconv.FormatBool(noOpen))
			fmt.Printf("未在 %s 发现 serve，正在启动一个带界面的 serve…\n", target)
			return s.RunE(s, args)
		},
	}
	c.Flags().BoolVar(&noOpen, "no-open", false, "不自动打开浏览器")
	return c
}

// isPortOpen 探测 TCP 端口是否已在监听（短超时，避免界面卡顿）。
func isPortOpen(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 400*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}
