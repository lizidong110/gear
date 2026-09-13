package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"gear/internal/model"
)

// aiCmd 是 AI 闭环（S6c）的 CLI 入口：自然语言 → Flow。
// 实现为 REST 客户端：打到执行面的 /api/ai/infer（推断器在执行面侧，可就近访问 Ollama / 远端网关）。
func aiCmd() *cobra.Command {
	c := &cobra.Command{Use: "ai", Short: "AI 闭环：自然语言 → Flow（S6c）"}
	c.AddCommand(aiGenCmd())
	return c
}

// aiGenCmd 用自然语言生成 Flow；--register 时直接落到本地 registry。
// 默认仅打印生成的 YAML，便于人工复核后再决定注册（fail-open 的 AI 输出不应静默入库）。
func aiGenCmd() *cobra.Command {
	var register bool
	c := &cobra.Command{
		Use:   "gen <prompt>",
		Short: "用自然语言生成 Flow（打到 REST 执行面的 /api/ai/infer）",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			flow, err := requestInfer(args[0])
			if err != nil {
				return err
			}
			if register {
				reg, err := openRegistry()
				if err != nil {
					return err
				}
				defer reg.Close()
				if err := reg.UpsertFlow(flow); err != nil {
					return err
				}
				cmd.Printf("✓ 已注册生成的 Flow: %s（steps=%d）\n", flow.ID, len(flow.Steps))
				return nil
			}
			out, err := yaml.Marshal(flow)
			if err != nil {
				return err
			}
			cmd.Print(string(out))
			return nil
		},
	}
	c.Flags().BoolVar(&register, "register", false, "生成后直接注册到本地 registry")
	return c
}

// requestInfer 调用执行面的 /api/ai/infer，把 prompt 收敛为 Flow。
func requestInfer(prompt string) (*model.Flow, error) {
	body, _ := json.Marshal(map[string]string{"prompt": prompt})
	resp, err := http.Post(execBaseURL()+"/api/ai/infer", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("连接执行面失败（%s）: %w", serverAddr, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("AI 生成失败: %s", respStatus(resp))
	}
	var f model.Flow
	if err := json.NewDecoder(resp.Body).Decode(&f); err != nil {
		return nil, err
	}
	return &f, nil
}
