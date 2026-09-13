package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/spf13/cobra"
)

// execCmd 触发并查询 Flow 执行（REST 执行面，PRD §11）。
// 三个子命令对应「触发 / 查询单个 / 列出全部」三件事，均为无状态 HTTP 客户端，
// 不打开本地 registry（执行面是服务端权威）。
func execCmd() *cobra.Command {
	c := &cobra.Command{Use: "exec", Short: "触发并查询 Flow 执行（REST 执行面，PRD §11）"}
	c.AddCommand(execStartCmd(), execStatusCmd(), execListCmd())
	return c
}

// execBaseURL 拼出执行面基址（--addr 是 host:port，补 http:// 前缀）。
func execBaseURL() string {
	return "http://" + strings.TrimRight(serverAddr, "/")
}

// execStartCmd 触发一次 Flow 执行（异步）：POST 立即返回 202 + execution id。
func execStartCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "start <flow_id>",
		Short: "触发一次 Flow 执行（异步：立即返回 execution id）",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			body, _ := json.Marshal(map[string]string{"flow_id": args[0]})
			resp, err := http.Post(execBaseURL()+"/api/executions", "application/json", bytes.NewReader(body))
			if err != nil {
				return fmt.Errorf("连接执行面失败（%s）: %w", serverAddr, err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusAccepted {
				return fmt.Errorf("触发执行失败: %s", respStatus(resp))
			}
			var out struct {
				ID     string `json:"id"`
				Status string `json:"status"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
				return err
			}
			cmd.Printf("✓ 已触发执行: id=%s status=%s\n", out.ID, out.Status)
			return nil
		},
	}
}

// execStatusCmd 查询一次执行的状态 / 结果（GET）。
func execStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status <id>",
		Short: "查询一次执行的状态 / 结果",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, err := http.Get(execBaseURL() + "/api/executions/" + args[0])
			if err != nil {
				return fmt.Errorf("连接执行面失败（%s）: %w", serverAddr, err)
			}
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusNotFound {
				return fmt.Errorf("执行记录不存在: %s", args[0])
			}
			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("查询执行失败: %s", respStatus(resp))
			}
			b, err := io.ReadAll(resp.Body)
			if err != nil {
				return err
			}
			cmd.Println(string(b))
			return nil
		},
	}
}

// execListCmd 列出全部执行记录（GET /api/executions）。
func execListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "列出全部执行记录",
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, err := http.Get(execBaseURL() + "/api/executions")
			if err != nil {
				return fmt.Errorf("连接执行面失败（%s）: %w", serverAddr, err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("列出执行失败: %s", respStatus(resp))
			}
			b, err := io.ReadAll(resp.Body)
			if err != nil {
				return err
			}
			var execs []map[string]interface{}
			if err := json.Unmarshal(b, &execs); err != nil {
				return err
			}
			if len(execs) == 0 {
				cmd.Println("（无执行记录）")
				return nil
			}
			for _, e := range execs {
				cmd.Printf("- %-28s flow=%-12v status=%-10v\n", e["id"], e["flow_id"], e["status"])
			}
			return nil
		},
	}
}

// respStatus 把非 2xx 响应压缩成「<状态码> <body 前若干字节>」便于排错。
func respStatus(resp *http.Response) string {
	b, _ := io.ReadAll(resp.Body)
	msg := strings.TrimSpace(string(b))
	if len(msg) > 200 {
		msg = msg[:200] + "..."
	}
	return fmt.Sprintf("HTTP %d: %s", resp.StatusCode, msg)
}
