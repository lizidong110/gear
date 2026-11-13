# 应用专用 MCP 面

这个端点只暴露应用清单 `mcp.tools` **白名单**里的工具（若同时开了 `--expose flows`，还会有
`flow__{id}` Flow 快照）。它由 `gear mcp --expose ... --only-exposed` 启动，工具面在启动时固定。

**这里没有 `gear_*` 管理工具**：`gear_guide` / `gear_catalog` / `gear_validate` / `gear_apply` /
`gear_run` / `gear_execution` / `gear_doctor` 等在本端点都**不存在**。不要尝试调用它们，
也不要在回答里编造它们的结果——看到工具列表以外的名字，就是它不在这个面上。

按每个工具自身的描述直接调用即可；参数、描述与产品护栏由应用作者在 App 清单里策展。
要管理 registry、校验或落库 bundle、查执行记录，请另连一个标准 `gear mcp` 实例
（默认带 15 个 `gear_*` 意图级工具与完整手册）。本端点是给业务调用方 / 终端用户用的，
刻意不携带内核写面。
