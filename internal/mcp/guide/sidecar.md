# sidecar：怎么写一个 ext.* 扩展（施工手册）

> 本主题面向**实现者**（人或带文件工具的智能体）。
> 若你只是「缺一个能力」，先读 `gear_guide("primitive-gap")` 判定该不该申请原语——
> **大多数情况的答案是建 Tool，不是加侧车**（该主题的归属决策表最后一栏）。

## 0. 先确认你真的需要侧车

侧车是**能力底座的扩张**，代价远高于写一个 Tool。动手前先按这张表自测：

| 你要做的事 | 需要侧车吗 | 怎么做 |
|---|---|---|
| 调一个 REST / OpenAPI（Bearer、API-Key、Basic 鉴权） | ❌ 不需要 | Lua Tool + `ctx.http` |
| 需要 HMAC 签名 / JWT 验签的 API | ❌ 不需要 | `ctx.http` + `ctx.crypto.hmac*` |
| XML / SOAP / GraphQL / JSON-RPC | ❌ 不需要 | `ctx.http` 自己拼 body、抽响应 |
| OAuth2 client_credentials 换 token | ❌ 不需要 | `ctx.http` 拿 token，再 `ctx.http` 调业务 |
| 出站 webhook（我发给别人） | ❌ 不需要 | `ctx.http` |
| 入站 webhook（别人发给我） | ❌ 通常不需要 | `ctx.subscribe` 经 REST 回推 |
| 连 MySQL / PG / SQLite | ❌ 不需要 | 内置 `ctx.db` 原语 |
| **非 HTTP 协议**：MQTT / AMQP / Kafka / SMTP / IMAP / 原生 TCP / gRPC | ✅ **需要** | 侧车 |
| **长连接复用 / 长驻消费**（避免每次重连） | ✅ **需要** | `service: true` |
| **需第三方 SDK 的复杂握手** | ✅ **需要** | 侧车 |
| **本地文件系统**（Lua 沙箱禁 `io`） | 视范围 | 安全根内的轻量读写**不需要**——`ctx.fs` 原语（2026-11-11 新增，路径收口在 `GEAR_APP_DATA`，越界即拒）；安全根外的任意路径、流式大文件、文件锁/监听等仍走侧车 |
| **对外监听端口**（静态站、定时回调、OIDC 回调） | ✅ **需要** | `service: true` |
| **二进制载荷**（Lua 字符串遇 NUL 截断） | ✅ **需要** | 侧车 |

判定一句话：**`ctx.http` 已经是完整的裸 HTTP 客户端**（任意 method / 任意 header /
任意 body / 拿原始 `status`+`body`+`headers`）。凡是「HTTP 上说清楚的事」，都不必写侧车。

## 1. 两种载体形态

| 形态 | 声明 | 传输 | 生命周期 | 适用 |
|---|---|---|---|---|
| **once 型** | 只写 `cmd`+`methods`+`schemas` | `CommandTransport`：**每次调用起一个子进程**，stdin/stdout JSON | 调用结束即退出 | 无状态、可容忍重连（发邮件、发短信） |
| **service 型** | 额外写 `service: true` + `listen` | `LocalHTTPTransport`：本地 HTTP 长连接 | 长驻，由 service step 持有，占用清空后自动退出 | 长连接、长驻订阅、对外监听 |

> `listen` 是协调地址的**唯一真相源**：引擎按它注入 `COORD_ADDR` 给侧车，也按它连侧车。
> `health_probe.addr` 省略即取 `listen`；显式写出却不一致则装配期 fail-closed。

## 2. once 型：协议与最小实现

协议（与 `internal/ext/ext.go` 完全一致）：

```
stdin  读完整 stdin（实现是 io.ReadAll，不是只读一行）
       {"method": "<方法>", "args": {<参数>}}
stdout 写一行 JSON
       {"result": <结果>}   或   {"error": "<错误信息>"}
```

**三个必守的实操约定**（违反会导致错误细节丢失或引擎误判）：

1. **stdout 只能有一行结果 JSON** —— 业务日志必须走 `stderr`。引擎取 stdout 末行，
   日志混进去就会被当成结果解析。
2. **退出码恒 0** —— 错误一律写进 `{"error": ...}`。非 0 退出会让引擎报
   「sidecar 退出异常」并**丢失你的错误细节**。
3. **panic / 未捕获异常也要转成 `{"error": ...}`** —— 否则子进程崩溃，同上。

最小实现（Python，任意语言同理）：

```python
import json, sys

def greet(a):
    return {"msg": "hi " + str(a.get("name", ""))}

METHODS = {"greet": greet}

def main():
    try:
        req = json.loads(sys.stdin.read())          # 读完整 stdin
        h = METHODS.get(req.get("method"))
        if h is None:
            raise ValueError("不支持的方法 %r" % req.get("method"))
        out = {"result": h(req.get("args") or {})}
    except Exception as e:                          # 一律转成 error，不崩溃
        out = {"error": str(e)}
    sys.stdout.write(json.dumps(out, ensure_ascii=False) + "\n")   # 退出码恒 0

main()
```

对应的注册段：

```yaml
ext.greet:
  cmd: ["python", "greet_sidecar.py"]     # 路径相对 extensions.yaml 所在目录解析
  methods: [greet]
  schemas:
    greet:
      description: 打招呼（示例）
      parameters:
        name: {type: string, required: true}
```

## 3. service 型：coord 协议与 /caps

侧车起一个 HTTP 服务监听 `COORD_ADDR`（由引擎按 `listen` 注入），暴露两组端点：

| 端点 | 归属 | 说明 |
|---|---|---|
| `POST /<method>` | 业务 | body `{"args": {...}}` → 响应 `{"result": ...}` |
| `GET /caps` | **必实现** | 见下 |
| `/register` `/heartbeat` `/deregister` `/occupants` | coord | 占用协调，SDK 已封装 |

**`/caps` 是红线端点**，返回本侧车真正实现的方法名：

```json
{"result": {"ext": "webhook", "methods": ["send", "subscribe", "unsubscribe"]}}
```

它的用途是校验「`extensions.yaml` 声明的 methods ⊆ 侧车真实实现的 methods」——
这是**唯一**能把「声明 ↔ 实现」对上的判据（见 §6）。

其他必守约定：占用列表为空时自动退出（`OnEmpty`）；端口被占时退让
（`coord.IsAddrInUse` → `os.Exit(coord.ExitAddrInUse)`），不要死等。

现役参考实现：`plugin/webhook-sidecar/main.go`（含 `/caps`）、
`plugin/sidecar-template/main.go`（骨架）。

> ⚠ `plugin/sidecar-template/main.go` **没有** `/caps`，照抄它不满足 §6 的校验——补上再用。

## 4. extensions.yaml 字段参考

| 字段 | 必填 | 说明 |
|---|---|---|
| `cmd` | ✅ | 启动命令；`cmd[0]` 相对**配置文件所在目录**解析为绝对路径 |
| `methods` | ✅ | 暴露给 `ctx.ext.<name>` 的方法名 |
| `schemas` | ✅ | 方法 → 参数 schema；**必须与 `methods` 一一对应**，否则启动 fail-closed |
| `kind` | — | `""`（默认）/ `sidecar`；`agent` 仅内核 `ext.ai` 可用，填了会被拒 |
| `service` | — | `true` 即 service 型 |
| `listen` | service 型必填 | 协调 + RPC 地址，如 `127.0.0.1:8089` |
| `source` | — | 承载的事件源名，供 `ctx.subscribe` 使用 |
| `health_probe` | — | `{kind: tcp\|http\|cmd, interval, threshold}`；`addr` 省略即取 `listen` |

`schemas` 的参数字段：`{type, description, enum, required}`。

## 5. 五条红线

1. **凭据一律从调用参数（`args`）传，禁止读 `os.Getenv`**（有机械守卫
   `TestExtAtomsMustNotReadEnv`）。唯一例外是 `COORD_ADDR`——那是侧车自身运行位置，由引擎注入。
2. **方法名必须 `ext.` 前缀**，`methods` 与 `schemas` 必须一一对应，缺 `schemas` 启动即拒。
3. **service 型必须实现 `/caps`**。
4. **配置文件存在时「以配置为准」**：未列出的 ext 不会被注册（含 `ext.ai`，漏了 AI 能力会静默消失）。
5. **不许造 `ext.*` 名字来承载业务逻辑**——那是 Tool，见 `gear_guide("primitive-gap")` 六条判据。

## 6. 自验（当前缺口，避免期待错误）

| 你想验的事 | 有没有现成手段 |
|---|---|
| YAML 内部自洽（`methods` ↔ `schemas`） | ✅ 启动期 fail-closed |
| **声明的 methods ↔ 侧车真实实现** | ⚠️ **只在 `plugin/verify.py` 里**（`/caps` 比对），**gear 自身与 MCP 都没有** |
| 侧车二进制在不在 | ✅ `gear_doctor` 返回 `missing_sidecars` |

也就是说：`gear ext check` / `probe` **不存在**，写完侧车后**没有内建手段自验「声明 ↔ 实现」**。
目前只能手工跑 `plugin/verify.py`，或起侧车后自己 `curl http://<listen>/caps` 比对。
`gear_validate` **不校验**这一项——它只验 YAML 自洽与 process entry 首 token 是否可达。

## 7. 载体语言怎么选

`once` 型**语言自由**（只要能出可执行文件、能读写 stdin/stdout JSON）。`service` 型则要看
**coord 协议有没有该语言的 SDK**——目前**只有 Go 版** `internal/sidecar/coord`，
换语言就得自己重写 `/register` `/heartbeat` `/deregister` `/occupants` + `OnEmpty` 自动退出
+ `EADDRINUSE` 退让这一整套。

三个判据，按优先级：

1. **目标系统有没有成熟驱动/SDK**（第一优先）。要连 Kafka / RabbitMQ / MQTT / SMTP 这类，
   该语言没有成熟驱动就**直接换语言**——协议实现不是靠写代码能补的。
2. **service 型优先 Go**：现成 SDK、现成模板、`build.sh` 已纳入构建。
3. **若代码主要由 AI 生成，优先选训练语料充足的语言**。生成正确率远比编译速度重要——
   AI 不在乎编译是 1 秒还是 10 秒，但很在乎「能不能一次写对」和「错了能不能看懂报错」。

## 边界（避免期待错误）

- **MCP 当前不提供写原语的接口**：智能体经 MCP 只能写 Tool / Flow / Hook，不能直接注册 `ext.*`。
  但这**不是禁止扩张**——扩张能力边界是设计目的（人同意即可），底线只有一条：
  **不得自行塞入可执行代码**（侧车二进制 / Lua 脚本体）。缺能力按 `gear_guide("primitive-gap")`
  出报告，**由人决定是否加**。
- **MCP 不提供文件读写**：侧车代码与 `extensions.yaml` 用你自己的文件工具落盘，gear 无从校验。
- `kind: agent` **不是**给用户开的口子：只有内核 `ext.ai` 能用，填别的名字装配期即拒。
- 每次 `once` 型调用都会**新起一个进程**（无状态、会重连）。高频或长连接场景用 `service: true`。
