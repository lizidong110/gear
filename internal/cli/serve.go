package cli

import (
	"fmt"
	"net"
	"os"

	"github.com/spf13/cobra"

	"gear/internal/httpsafe"
	"gear/internal/model"
	"gear/internal/registry"
	"gear/internal/server"
)

// serveCmd 启动 REST 执行面。监听地址复用根命令的持久 --addr 标志
// （与 exec / ai 等客户端子命令一致，便于联调同一执行面）。
//
// 监听闸（与 gear mcp 同一份判定，internal/httpsafe）：REST 面当前无鉴权，且 30 个端点里
// 有明文 secrets（PUT/GET /api/secrets/{name}）与 registry 写入——比 MCP 面写得还多，
// 没理由 MCP 拒绑全网卡而这里放行。非回环地址（含空 host `:8787`，等价 0.0.0.0）默认拒绝，
// 确有远程需求时显式 --allow-remote。
func serveCmd() *cobra.Command {
	var seed string
	var allowRemote bool
	c := &cobra.Command{
		Use:   "serve",
		Short: "启动 REST 执行面（PRD §11；执行面无鉴权，默认只绑回环）",
		Long: "启动 REST 执行面（PRD §11）。\n\n" +
			"两种执行语义不要混：\n" +
			"  POST /api/tools/{id}/run   同步——直接返回该 Tool 的输出（200）\n" +
			"  POST /api/executions       异步——返回 202 + execution id，用 GET /api/executions/{id} 查\n\n" +
			"⚠ 执行面**无鉴权**（只绑回环 + 校验 Origin；不带 Origin 的本机请求照常放行）。" +
			"在这个面上开发的业务应用必须自建登录与权限，平台面不提供——见 gear_guide(\"security\")。",
		RunE: func(cmd *cobra.Command, args []string) error {
			// 闸必须在 ListenAndServe 之前——等监听上了再说就晚了。
			if err := httpsafe.CheckBindAddr("REST 执行面", serverAddr, "读写 registry 与明文 secrets、执行编排", allowRemote); err != nil {
				return err
			}
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
			// WithRegistryPath：POST /api/apps/prune 要按「本进程真正在用的」registry 定位
			// 落盘台账；不注入的话它会退回 gearhome 默认值，去清另一个库的台账。
			srv, err := server.New(reg, server.WithRegistryPath(registryPath), server.WithOnListen(func(realAddr string) {
				// 顺序不可换：port 行必须是 stdout 的**第一行非空行**，否则 process runtime
				// 在长驻子进程 stdout 里找首行 JSON 时会解析到 banner。
				// banner 带**库内计数**：不带的话，「指向了一个空库」这件事完全静默——
				// 页面只是空列表，日志一个字都没有（2026-09-22 实测：应用自带的 REST 面读的是
				// 另一次实验留下的空库，排查花了一整轮）。起服时打印一行 flows=0 就够了。
				if line := servePortLine(realAddr); line != "" {
					fmt.Print(line)
				}
				fmt.Print(serveBanner(realAddr, registryPath, reg))
			}))
			if err != nil {
				return err
			}
			return srv.ListenAndServe(serverAddr)
		},
	}
	c.Flags().StringVar(&seed, "seed", "", "启动时从 YAML 灌入示例 Tool/Flow（多文档，每篇一个 Tool 或 Flow），如 examples/news-top10.yaml")
	c.Flags().BoolVar(&allowRemote, "allow-remote", false, "允许监听非回环地址（当前无鉴权，明文 secrets 与 registry 写入会暴露给局域网）")
	return c
}

// serveBanner 渲染起服横幅（抽成纯函数以便断言——它承载的是一条排障判据，不是装饰）。
//
// 为什么必须带计数：默认路径修好「不再依赖 cwd」之后，serve 可以拿着一个
// **表结构完好但 0 行**的库正常起服。这种失败模式完全静默：服务在听、页面能开、只是列表为空，
// 日志里没有任何异常。一行 `flows=0` 就能把「指错库了」与「库里真没东西」分开。
// servePortLine 渲染长驻进程给父进程看的**首行机器可读声明**（`{"port":8787}`）。
//
// 为什么是 stdout 第一行：process runtime 拉起长驻子进程后，把 stdout 第一行非空行当 JSON
// 解析成 step output（见 process.go 的 firstJSONLine）。这样 `${steps.api.output.port}`
// 就能直接喂给 static_site 的 proxy.target——全链路端口分配缺的正是这一环。
// 解析失败不报错、output 为 nil：绝不说谎，但也不替用户猜。
//
// 拿不到端口号时返回**空串**（调用方据此跳过打印），绝不会写出 `{"port":0}`——
// 0 是一个会被下游当真、且查不出来的假值，比「什么都没说」坏得多。
func servePortLine(addr string) string {
	_, port, err := net.SplitHostPort(addr)
	// port=="0" 必须一并拒绝：`--addr :0` 未展开时 SplitHostPort 会给 "0"，
	// 于是写出 `{"port":0}` —— 一个会被下游当真、且事后查不出来源的假值。
	if err != nil || port == "" || port == "0" {
		return ""
	}
	return fmt.Sprintf("{\"port\":%s}\n", port)
}

func serveBanner(addr, registry string, reg *registry.Registry) string {
	flows, _ := reg.ListFlows()
	tools, _ := reg.ListTools()
	return fmt.Sprintf("Gear REST 服务监听 %s（registry=%s, flows=%d, tools=%d）\n",
		addr, registry, len(flows), len(tools))
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
					if err := reg.UpsertFlow(old, act()); err != nil {
						fmt.Fprintf(os.Stderr, "警告: 回滚 Flow %s 失败: %v\n", id, err)
					}
				} else if err := reg.DeleteFlow(id, act()); err != nil {
					fmt.Fprintf(os.Stderr, "警告: 回滚（删除）Flow %s 失败: %v\n", id, err)
				}
				continue
			}
			if old, ok := prevTools[id]; ok {
				if old != nil {
					if err := reg.UpsertTool(old, act()); err != nil {
						fmt.Fprintf(os.Stderr, "警告: 回滚 Tool %s 失败: %v\n", id, err)
					}
				} else if err := reg.DeleteTool(id, act()); err != nil {
					fmt.Fprintf(os.Stderr, "警告: 回滚（删除）Tool %s 失败: %v\n", id, err)
				}
			}
		}
	}

	// 阶段二：落库 Tool（Flow 的 call 目标必须先存在，其静态校验才有意义）。
	for _, t := range tools {
		if err := reg.UpsertTool(t, act()); err != nil {
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
		if err := reg.UpsertFlow(f, act()); err != nil {
			rollback()
			return fmt.Errorf("灌入 Flow %s 失败（已回滚，注册表保持原状）: %w", f.ID, err)
		}
		written = append(written, f.ID)
	}
	fmt.Printf("已从 %s 灌入 %d 个 Tool、%d 个 Flow\n", path, len(tools), len(flows))
	return nil
}
