# examples/ — 可运行的演示（Demo）集合

本目录存放**面向用户的演示 / 种子文件**，可用 `gear serve --seed examples/<file>.yaml` 灌入引擎，
或作为编写 Tool / Flow 的参考模板。

> **约定：测试不再依赖本目录。** 测试固件统一放在仓库根的 `testdata/`（与 `examples/` 解耦），
> 所以改这里的演示文件**不会**影响测试；但若想让测试也覆盖新写法，请同步更新 `testdata/` 对应文件。
>
> **唯一例外：`examples/auth-app/`。** `internal/artifact/auth_sample_test.go` 直接读它的
> `bundle.yaml` 跑鉴权链路（放行 / 拒绝 / 不泄露凭据）。这条破例是刻意的：样板的价值在于
> 「它自己就是能跑的」，另抄一份固件只会让测试变绿而样板失效。改这份 bundle 的 Tool / Hook
> 请一并跑 `go test ./internal/artifact/ -run TestAuthSample`。

| 文件 | 说明 |
| --- | --- |
| `hello_flow.yaml` | 入门 Flow：变量用 `${vars.x}` 简写（PRD §4.1 修正后示例），端到端可跑通。 |
| `hello_tool.yaml` | 入门 Tool：仅用内核原语，无 ext 依赖，适合作为最小模板。 |
| `service_flow.yaml` | 编排 Flow 示例：`call: sleeper` 调用下面的 sleep 工具。 |
| `sleeper_tool.yaml` | 示例 Tool：`sleep` 工具，供 `service_flow.yaml` 调用。 |
| `ai-create-flow.yaml` | `gear ai gen` 的 `ai-gen` 配方来源（含 `ai-gen` Tool 的 Lua entry，是生成逻辑的单一真相源）。 |
| `news-top10.yaml` | `gear serve --seed examples/news-top10.yaml` 灌入的 `news.top10` Flow + `news.fetch` Tool 示例，提供 `/api/news` 演示。 |
| `auth-app/` | **Hook 鉴权样板**（`ENTERPRISE_PRD.md` FR-1）：登录发令牌 → Bearer 主体 → 守卫 Hook 验签 → 受保护 Tool。零内核改动，含 `README.md` 与回归测试。 |

## 不会写 YAML？用 `gear scaffold`

如果不知道某种资源怎么写，直接用命令生成带注释、且能直接 `register` 的骨架，比从示例改更快：

```bash
gear scaffold tool my-tool -o my-tool.yaml     # --runtime lua|go-host 切换 runtime
gear scaffold flow my-flow -o my-flow.yaml
gear scaffold hook my-hook -o my-hook.yaml
gear scaffold app my-app -o my-app.yaml       # ⚠ App 清单不走 register：gear apply 落库
gear scaffold tool --help                       # 内嵌完整字段表 / 取值 / 示例
```

生成物填好 `TODO` 注释后即可 `gear tool register` / `flow register` / `hook register`。
App 清单例外——`tool/flow register` 永远不写 App 行，它走 `gear apply`（CLI）/ `gear_apply`（MCP）。
注意 `gear import` 只吃 `gear export` 的产物（目录/.gearapp），**不接受单份 YAML**，别把
App 清单喂给它。入口 Flow 已注册时直接 `gear apply` 即可。App 与 Flow 的分工、三条注册路径见
[`docs/app-registration.md`](../docs/app-registration.md)。
