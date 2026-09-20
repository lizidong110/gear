package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"gear/internal/model"
	"gear/internal/registry"
	"gear/internal/server"
)

// serveCmd 启动 REST 执行面。监听地址复用根命令的持久 --addr 标志
// （与 exec / ai 等客户端子命令一致，便于联调同一执行面）。
func serveCmd() *cobra.Command {
	var seed string
	c := &cobra.Command{
		Use:   "serve",
		Short: "启动 REST 执行面（PRD §11；执行是异步的，POST 返回 202 + execution id）",
		RunE: func(cmd *cobra.Command, args []string) error {
			reg, err := openRegistry()
			if err != nil {
				return err
			}
			defer reg.Close()
			if seed != "" {
				if err := seedExamples(reg, seed); err != nil {
					return err
				}
			}
			fmt.Printf("Gear REST 服务监听 %s（registry=%s）\n", serverAddr, registryPath)
			srv, err := server.New(reg)
			if err != nil {
				return err
			}
			return srv.ListenAndServe(serverAddr)
		},
	}
	c.Flags().StringVar(&seed, "seed", "", "启动时从 YAML 灌入示例 Tool/Flow（多文档，每篇一个 Tool 或 Flow），如 examples/news-top10.yaml")
	return c
}

// seedExamples 把示例 YAML 里的 Tool/Flow 灌入注册表（供 GET /api/news 等端点开箱即用）。
//
// 原子性：示例文件里「第 1 个 Tool 成功、第 3 个失败」会留下半套目录，服务带着它起来
// 比直接失败更难排查。但这里**不能**照搬 MCP create_tool/create_flow 的「先全量校验
// 再全量落库」——Flow 的静态校验要检查 call 目标是否存在，而示例里的 Flow 往往正好
// 调用同一文件里定义的 Tool（如 news.top10 → news.fetch），Tool 没入库就校验必然失败。
//
// 因此顺序是：校验 Tool → 落库 Tool → 校验 Flow → 落库 Flow；任一步失败则按已写入
// 的清单**回滚**，使注册表回到调用前的状态（新增回滚，而非留下部分结果）。
func seedExamples(reg *registry.Registry, path string) error {
	tools, flows, err := registry.LoadExamples(path)
	if err != nil {
		return fmt.Errorf("读取示例 %s 失败: %w", path, err)
	}
	// 阶段一：Tool 全量校验（不依赖其它对象，可以前置）。
	for _, t := range tools {
		if err := t.Validate(); err != nil {
			return fmt.Errorf("示例 Tool %s 校验失败（本次未灌入任何 Tool/Flow）: %w", t.ID, err)
		}
	}
	// 记录将被覆盖的 Tool / Flow 原值，供回滚恢复（之前不存在则为 nil → 回滚时删除）。
	prevTools := map[string]*model.Tool{}
	for _, t := range tools {
		old, err := reg.GetTool(t.ID)
		if err != nil {
			return fmt.Errorf("读取 Tool %s 原值失败: %w", t.ID, err)
		}
		prevTools[t.ID] = old
	}
	prevFlows := map[string]*model.Flow{}
	for _, f := range flows {
		old, err := reg.GetFlow(f.ID)
		if err != nil {
			return fmt.Errorf("读取 Flow %s 原值失败: %w", f.ID, err)
		}
		prevFlows[f.ID] = old
	}

	// 回滚：按已写入清单恢复原值（原先不存在则删除）。回滚自身的错误不掩盖原始错误，
	// 但会追加到 stderr 提示，避免「回滚也失败了」被静默吞掉。
	written := []string{}
	rollback := func() {
		for _, id := range written {
			if old, ok := prevFlows[id]; ok {
				if old != nil {
					if err := reg.UpsertFlow(old); err != nil {
						fmt.Fprintf(os.Stderr, "警告: 回滚 Flow %s 失败: %v\n", id, err)
					}
				} else if err := reg.DeleteFlow(id); err != nil {
					fmt.Fprintf(os.Stderr, "警告: 回滚（删除）Flow %s 失败: %v\n", id, err)
				}
				continue
			}
			if old, ok := prevTools[id]; ok {
				if old != nil {
					if err := reg.UpsertTool(old); err != nil {
						fmt.Fprintf(os.Stderr, "警告: 回滚 Tool %s 失败: %v\n", id, err)
					}
				} else if err := reg.DeleteTool(id); err != nil {
					fmt.Fprintf(os.Stderr, "警告: 回滚（删除）Tool %s 失败: %v\n", id, err)
				}
			}
		}
	}

	// 阶段二：落库 Tool（Flow 的 call 目标必须先存在，其静态校验才有意义）。
	for _, t := range tools {
		if err := reg.UpsertTool(t); err != nil {
			rollback()
			return fmt.Errorf("灌入 Tool %s 失败（已回滚，注册表保持原状）: %w", t.ID, err)
		}
		written = append(written, t.ID)
	}
	// 阶段三：Flow 全量静态校验。引擎需在 Tool 入库后装配（扩展目录挂在它上面，
	// 与 REST/CLI 注册同一条规则——校验规则只能有一处，§0 单一真相源）。
	if len(flows) > 0 {
		eng, err := cliEngine(reg)
		if err != nil {
			rollback()
			return err
		}
		for _, f := range flows {
			if err := eng.ValidateFlowStatic(f); err != nil {
				rollback()
				return fmt.Errorf("示例 Flow %s 静态校验失败（已回滚本次全部灌入）: %w", f.ID, err)
			}
		}
	}
	// 阶段四：落库 Flow。
	for _, f := range flows {
		if err := reg.UpsertFlow(f); err != nil {
			rollback()
			return fmt.Errorf("灌入 Flow %s 失败（已回滚，注册表保持原状）: %w", f.ID, err)
		}
		written = append(written, f.ID)
	}
	fmt.Printf("已从 %s 灌入 %d 个 Tool、%d 个 Flow\n", path, len(tools), len(flows))
	return nil
}
