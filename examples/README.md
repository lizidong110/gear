# examples/ — 可运行的演示（Demo）集合

本目录存放**面向用户的演示 / 种子文件**，可用 `gear serve --seed examples/<file>.yaml` 灌入引擎，
或作为编写 Tool / Flow 的参考模板。

> **约定：测试不再依赖本目录。** 测试固件统一放在仓库根的 `testdata/`（与 `examples/` 解耦），
> 所以改这里的演示文件**不会**影响测试；但若想让测试也覆盖新写法，请同步更新 `testdata/` 对应文件。

| 文件 | 说明 |
| --- | --- |
| `hello_flow.yaml` | 入门 Flow：变量用 `${vars.x}` 简写（PRD §4.1 修正后示例），端到端可跑通。 |
| `hello_tool.yaml` | 入门 Tool：仅用内核原语，无 ext 依赖，适合作为最小模板。 |
| `service_flow.yaml` | 编排 Flow 示例：`call: sleeper` 调用下面的 sleep 工具。 |
| `sleeper_tool.yaml` | 示例 Tool：`sleep` 工具，供 `service_flow.yaml` 调用。 |
| `ai-create-flow.yaml` | `gear ai gen` 的 `ai-gen` 配方来源（含 `ai-gen` Tool 的 Lua entry，是生成逻辑的单一真相源）。 |
| `news-top10.yaml` | `gear serve --seed examples/news-top10.yaml` 灌入的 `news.top10` Flow + `news.fetch` Tool 示例，提供 `/api/news` 演示。 |
