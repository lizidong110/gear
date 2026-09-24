# atoms：能力原语与扩展原子

## 1. 四类能力（先分清，再看用法）

| 类别 | 成员 | 受 capability 门禁 | Flow 能直调 | 说明 |
|---|---|---|---|---|
| **受门禁原语** | `http` `call` `db` `secret` `event` `subscribe`（6） | **是**（Tool 必须声明） | 能（如 `http.Request`） | 单一真相源 `internal/cap/cap.go` |
| **纯函数原子** | `time` `template` `crypto`（3） | **否**（始终可用，**声明了反而被拒**） | 能（如 `crypto.hash`） | 无 IO、确定性、单具名 table 入参 |
| **Lua 便利绑定** | `json`（`ctx.json.encode/decode`） | — | **不能** | **不是能力**，不在能力目录 |
| **扩展原子** | `ext.email` `ext.serve` `ext.kafka` … | **是**（还须在 `extensions.yaml` 注册） | 能（如 `ext.serve.start`） | 分 once 与 `service: true` 两种 |

**方法名与参数一律以 `gear_capabilities` 为准**——下面只列确信的常用形态，别凭记忆猜。

## 2. 受门禁原语的常用方法

| call | 参数（必填加 *） | 返回 |
|---|---|---|
| `http.Request` | method*, url*, body, headers | `{status, body, headers}` |
| `call.Call` | target*, input | 目标的输出 map |
| `db.query` | sql*, driver, dsn, params | 行集合 `[]map` |
| `db.exec` | sql*, driver, dsn, params | nil（失败经 err 表达） |
| `db.ping` | driver, dsn | 连通性 |
| `secret.get` | name* | 明文（**只读取**，看不到存储位置） |
| `event.notify` | name*, payload | 广播（尽力通知） |
| `event.wait` | name* | 下次通知的 payload |
| `subscribe.start` / `subscribe.stop` | source*/config*/callback* / handle* | handle / nil |

用 `db` 连外部库时，口令**优先写 `${secrets.*}`**，不要内联明文。

## 3. 纯函数原子的常用方法

| call | 参数（必填加 *） |
|---|---|
| `time.now` | format（rfc3339 等枚举）, tz（如 `Asia/Shanghai`） |
| `time.parse` | value*, format, tz → Unix 秒（数字，便于比较） |
| `time.format` | unix 或 ms（二选一）*, format, tz |
| `time.sleep` | seconds*（0 < s ≤ 60，超限即报错，避免挂死编排） |
| `template.render` | text*, vars（只解析 `${vars.*}`） |
| `crypto.hash` | algo*（md5/sha1/sha256/sha512）, data*, encoding（hex/base64） |
| `crypto.hmac` | algo*, key*, data*, encoding |
| `crypto.hmac_verify` | algo*, key*, data* → bool（恒定时比较） |

`crypto.hmac` 的 key 应经 `ctx.secret.get` 或 `${secrets.x}` 取——**别写明文**。

## 4. 扩展原子 `ext.*`

- 命名是**两级**的：`ext.<name>` 是能力名，方法另算。`ext.ai.complete` = 能力 `ext.ai` + 方法 `complete`。
- 用之前确认三件事：① `gear_capabilities` 里有没有；② `gear_doctor` 里侧车在不在；③ Tool 有没有声明该 `ext.*`。
- `service: true` 的扩展（`ext.serve` / `ext.kafka` / `ext.mqtt` / `ext.rabbitmq` / `ext.webhook`）是**常驻侧车**：
  必须由 `mode: service` 的 step 持有，后续指令 step 用 anchor 排到下一层（见 `gear_guide("service-app")`）。
- 常见方法形态：`ext.serve.start{port*,dir*,host,proxy}` / `ext.serve.stop{port?}` / `ext.email.send{to*,host*,user*,pass*,…}`。
- `ext.serve` 是**多站点**的：同一侧车可同时托管多个网站（不同 port）。`stop` 带 `port` 只停该站
  （多 app 共享侧车时用它，避免误杀别人的站）；省略 `port` 停全部。同端口不同配置再 `start` 会 fail-closed 报错。

## 5. 三条最容易写错的

1. **`crypto`/`time`/`template` 不用声明，声明了会被拒**（`GrantsFromStrings` 只认门禁原语与 `ext.*`）。
2. **`json` 不是能力**：Flow 的 `step.call` 写 `json.encode` 一定失败；它只在 Lua 里以 `ctx.json.encode/decode` 出现。
3. **`ext.*` 必须声明**：`capabilities: [ext.serve]` 不能省，否则注册期就报未知 capability。

## 6. 缺能力时怎么办

**不许凑**。不允许：
- ❌ 用 `process` runtime 里 `sh -c` / `cmd /c` 调外部程序绕开能力门禁；
- ❌ 自造 `ext.xxx` 名字（未注册的能力，声明了也被拒，还污染口径）；
- ❌ 读环境变量取凭据（机械守卫会拒）。

要按 `gear_guide("primitive-gap")` 的模板给出**基础、通用、无业务名词**的原语建议，并说清哪些步骤被阻塞。
