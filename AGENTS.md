# AGENTS.md — Gear 仓干活规矩

> 给所有在本仓干活的智能体（包括我自己）。**接任务先读这个**，再读 `REQUIREMENTS.md`。
> 本仓代码与文档是同步的，所以用文档导航比用搜索导航更快、更准。

## 1. 第一动作：查索引，不是搜代码

`REQUIREMENTS.md` 是**可执行规格契约**（需求 ID → 一句话行为 + 状态 + 落点文件 + 断言测试函数名）；
设计理由与历史修正存在 `docs/requirements-archive.md`。它比搜代码准，也比搜代码快。

接任务按这三步走：

1. 读 `.workbuddy/memory/DOCMAP.md`（主题 → REQ 前缀 → 常改文件）；
2. `grep "REQ-XXX" D:/gadern/gear/REQUIREMENTS.md` —— 拿行为、落点文件、断言测试；
3. 直接打开它点名的文件和测试函数。

**禁止一上来就派 Explore 子代理或全仓 grep。**
只有三步之后仍答不上（真新代码 / 矩阵未覆盖 / 要追调用链细节）才派，
且必须带上已拿到的 REQ-ID 与文件名，不要让它从零搜一遍。

实证（2026-10-01）：摸 `ext.ai.react` 实现时派了 Explore 从头搜；
而 `REQ-REACT-TOOL-BUDGET` 一行已点名 `internal/engine/ai_agent.go` 的
`reactAllow` / `resolveReactAllow` / `maxReactTools` / `reactToolsText` 及三个测试函数——整轮探索本可省掉。

## 2. 改代码即改文档

- 改行为 → 改 `REQUIREMENTS.md` 对应行（含「断言测试」列）+ 所有引用该行为的 .md。
- 错误码单一真相源：`internal/mcp/guide/errors.md`。
- 新测试配负向验证（`tools/negcheck.py`）。
- `tools/requirements_audit.py` 机械校验引用存在性：引用了不存在的测试 = 闸门红。
- 改 markdown 用 Edit 字符串锚点，别用脚本按行号重排。

## 3. 闸门（提交前必跑）

`bash scripts/gate.sh`（全套）／`--fast`（只跑改动闭包，拿不准自动升级全套）／`--serial`（-p 1）。
日志 `build/testlogs/`。三档口径与其它实踩的坑见 `.workbuddy/memory/MEMORY.md`。

## 4. 常见动作检查表

| 改了什么 | 必须补做 |
|---|---|
| CLI 子命令 | 验二进制真更新（`build/gear.exe` 常被本会话 MCP 占用，别杀；另编 `gear-<tag>.exe` 用完删） |
| 内核 | 重建后重注册 Tool |
| ext / 侧车 | 先删 `build/<name>-sidecar.exe`，再跑 `plugin/verify.py`（需 `NO_PROXY=127.0.0.1,localhost`） |
| Go 枚举 | 重跑 `plugin/web-ui/scripts/gen-enums.mjs` |
| Tool/Flow YAML | 重跑对应 `gen-client.mjs`（如 `node apps/mindstorm/gen-client.mjs`） |
| 新内核包 | 进 `scripts/gate.sh` 的 `COVER_PACKAGES` |
