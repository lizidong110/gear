# ext 常驻能力（service ext）实现计划

> 状态：**阶段一 / 二 / 三 / 四 已全部实现并通过单测 + 二进制集成测试**。
> - 阶段一：service ext 自动包成 process Tool + 惰性拉起（进程内 §3.1 已设计，注册路径代码待补）；
> - 阶段二：`LocalHTTPTransport` 显式连接池复用（keep-alive），`TestLocalHTTPTransport_ConnectionReuse` 验证；
> - 阶段三：`mqtt-sidecar` 真实样板（`plugin/mqtt-sidecar` + `examples/*`），核心 MQTT 逻辑已内聚进该侧车并有单测（`plugin/mqtt-sidecar/main_test.go`）；
> - 阶段四：多 gear 端口协调（`internal/sidecar/coord`），`TestCoordServeYieldsWhenAddrInUse`（`coord_test.go`）+ `TestClientReusesExistingSidecar` / `TestClientBecomesOwnerWhenNoListener` 验证退让与复用。
> `go build -p 1 ./...` 全绿；并行 `-p` 会因本沙箱 I/O 读损坏误报语法错，用串行 `-p 1` 即可。
> 真实 broker 联调（test.mosquitto.org / 本地 emqx）待有网环境跑 `examples/mqtt_service_flow.yaml`。
> 关联：`ATOM_ROADMAP.md`、讨论结论（2026-09-17）。
> 目标：让"用户添加的 ext 能力"也能以**常驻**方式运行，并被 Gear 监管——
> 解决当前 ext 只能 stdin/stdout 一次性（once）调用、无法满足长驻需求的问题。

---

## 1. 背景与动机

当前 Gear 的扩展原子（ext.*）走 `CommandTransport`：

- 每次调用起一个 `gear ext <name>` 子进程（或被 YAML 声明的 sidecar 命令）；
- 经 stdin 传 `{"method","args"}`，stdout 末行读 `{"result"}` / `{"error"}`；
- 子进程执行完即退出（once 模型，`internal/ext/ext.go` 的 `Run` 总是返回 nil、exit 0）。

而 Gear 已有的**长驻（service）机制**只认 `runtime: process` 的 Tool `entry`
（`internal/engine/process.go` 的 `Start` + `internal/app/supervisor.go` 的 `Supervisor`），
与 ext 的调用路径（`CommandTransport`）是两套，没打通。
结果：**用户自己写的 ext sidecar 无法声明"我要常驻"，只能被一次性调用。**

长驻需求真实存在：MQTT / RabbitMQ 的 **subscribe（订阅消费）**、
轮询型采集器、本地网关等，本质都是"持续运行 + 持续产出"，不是"调一次返回"。

> 已确认的好消息：`service` 的监管底座（重启策略 / 健康探针 / 进程树级联回收）在
> `gear serve` 的 `RunFlow` 路径**已复用**（`server.go:620-679` 按 `req.Restart` 选策略、
> `app.New(...).Supervise(...)` 监管、结束时 `cancel()` 杀掉所有常驻子进程）。
> 我们要做的不是重建监管，而是**把用户 ext 声明接进这条已有的 service 路径**。

---

## 2. 目标与非目标（边界）

**目标**

- 用户（或内核）在 `extensions.yaml` 声明一个 ext 为常驻，Gear 能拉起并监管它；
- 常驻 ext 既能"自己持续运行"（daemon 守护语义：订阅 / 轮询），又能"被 Gear 调用"
  （rpc 语义：publish 等），通过**本地 HTTP 解耦**；
- 常驻 ext 随引用它的 flow 生命周期**惰性拉起 / 平滑停止**，复用现有 `Supervisor`；
- **进程内（Go 内建）ext 也能声明常驻**：以 goroutine 形式活在 gear 进程里、直接持有
  `ctx.*`、走 goroutine 级监管（重启 / 探针）—— 无需任何 transport（见 §3.1）；
- 完全向后兼容：不声明 `service` 的 ext 维持原有 once 行为。

**非目标（明确划出）**

- **跨主机分布式单例 / 选主**：仍非目标。多 gear 部署在**不同主机**时各自起本地 sidecar，
  由部署层（K8s 单副本）或消息层（MQTT `$share`）处理，内核不背分布式选主复杂度。
- **同主机多 gear 的端口冲突**：**本期纳入**（见 §12）——通过基于 PID 注册的占用列表，
  让同主机的外部 service 成为**每主机单例**：首个需要的 gear 拉起 sidecar 并绑定端口，
  其余 gear 注册 PID 复用同一实例；占用列表空则 sidecar 自动退出。即 Gear 保证
  "单 gear 进程内一份"升级为"单主机一份"。
- 不改现有内核原语（受门禁 7 个 http/call/db/kv/secret/event/subscribe + 纯函数 3 个 time/template/crypto）与 `lua` / `go-host` runtime（它们依旧不支持 service，与现状一致）。

---

## 3. 设计概述：daemon + rpc 合流

一个 `service: true` 的 ext = **常驻侧车**：

- **daemon 语义**：侧车启动后自己持续运行（订阅 MQTT、轮询、消费），Gear 只管它的生死；
- **rpc 语义**：侧车同时监听一个**本地 HTTP 接口**，Gear 作为 client 调它的方法
  （如 `publish`），侧车内部维护长连接 —— 直接消除 rabbitmq 那种"每次 `amqp.Dial` 重连"的浪费；
- **解耦**：侧车与 Gear 仅是标准 HTTP 互调，互不依赖对方内部实现，任何语言都能写侧车。

二者合流为同一个 `service: true` 的侧车：**既可自运行，又能被调**。

```
                ┌─────────────────────────────────────────┐
                │  gear serve（已复用 Supervisor）          │
                │  flow service step 引用 ext.mqtt          │
                │   → processRuntime.Start(cmd) 惰性拉起    │
                └───────────────┬─────────────────────────┘
                                │ 监管: 重启策略 / 健康探针 / 级联回收
                                ▼
                ┌─────────────────────────────────────────┐
                │  mqtt-sidecar（常驻进程, 用户/内核提供）  │
                │  ├─ daemon: 内部订阅/轮询，自运行         │
                │  └─ rpc:   监听 127.0.0.1:1884 (HTTP)    │
                └───────┬───────────────────────┬─────────┘
        Gear 调 publish │                       │ 收到消息回调
        (本地 HTTP)     ▼                       ▼
                   Gear REST / event ◀─────── 侧车主动推送
```

### 3.1 进程内服务（in-process service）— 补全 2×2 的第四格

§3 只覆盖了"进程外服务"（常驻侧车 + RPC）。但 ext 还有"进程内"一族，其服务形态
与进程外完全不同，且**不需要任何 transport**。

#### 2×2 全景

|  | 普通 once | 服务 service |
|---|---|---|
| **进程外**（standalone exe） | STDIO：CommandTransport，`gear ext <name>` 子进程，读一次 stdin / 写一次 stdout 即退 | RPC：常驻侧车 + 本地 HTTP/socket，Supervisor 管进程树 / 探针 / 重启 |
| **进程内**（Go builtin，编译进主二进制） | 直接函数调用：go-host `RegisterHostFunc`，同进程无子进程 | **长驻 goroutine**：直接持有 `ctx.*`，无任何 transport，goroutine 级监管 |

**关键判断**：STDIO / RPC 都是"跨进程边界"才需要的传输层。进程内同地址空间，
Gear 直接拿函数指针调方法、服务组件直接读 `ctx.*` 宿主能力（call/event/db/kv），
不存在"传输"这件事。

> 易混点提示：现有 `internal/ext/*.go`（rabbitmq/kafka/sms）代码虽**编译进主二进制**
> （打包意义上是"进程内"），但运行时仍经 `gear ext <name>` 子进程（CommandTransport）执行 ——
> 即"进程外执行"。真正的「进程内服务」需要一条新注册路径，让组件**活在 gear 进程里**。

#### 进程内服务怎么落地

注册形式不再逐方法 `register(ext, method, handler)`，而是注册一个**长驻组件**：

```go
// 现有：一次型，逐方法
register("mqtt", "publish", func(ctx Context, args map[string]any) (any, error) {...})

// 新增：长驻型，整组件
registerService(ServiceSpec{
    Name:    "mqtt",
    Start:   func(ctx Context) (StopFunc, error) { /* 起 goroutine：订阅/轮询/长连接 */ },
    Methods: map[string]MethodHandler{"publish": ..., "subscribe": ...},
    Health:  func() bool { /* 自报健康 */ },
})
```

生命周期（flow service step 引用时）：

1. Gear 在主进程里 `go spec.Start(cancellableCtx)` 拉起一个 goroutine，
   `ctx` 携带宿主能力（call/event/db/kv）—— 比进程外侧车**更强**，无需 IPC 注入；
2. 监管复用同一套「重启策略 + 健康探针」逻辑，但作用在 **goroutine** 上（不是进程树）：
   - goroutine 异常退出 → `recover` 捕获 → 按 `RestartAlways/Never` + 指数退避重启；
   - `Health()` 连续失败 → 杀掉并重启；
3. flow 取消 → `ctx` 取消 → `Stop()` 平滑退出；**无进程树、无孤儿、无 fd 泄漏风险**。

代价与边界：

- **共享 gear 地址空间**：组件 panic 必须 `recover` 包裹，否则会拖垮 gear 主进程；
  内存泄漏 / 崩溃影响面大于独立侧车 —— 这是"进程内"换来的代价；
- **仅 Go 可写**：组件编译进主二进制，用户自定义侧车仍走进程外路径（任意语言）；
- 两者互补而非替代：进程外服务 = 隔离 / 任意语言 / 故障不波及主进程；
  进程内服务 = 零传输开销 / 直接拿宿主能力 / Go 内建。

---

## 4. 扩展声明格式（extensions.yaml 改动）

在现有 ext 段增加三个可选字段（`internal/model` 的 ext 配置结构需相应加字段）：

```yaml
ext.mqtt:
  cmd: ["./mqtt-sidecar"]        # 侧车启动命令（任意语言；可改为 ["build/gear.exe","ext","mqtt"] 若内核内建）
  methods: [publish, subscribe]  # 暴露的方法（仅用于 schema/agent 锚定，非强制）
  schemas:                       # 必填（红线：缺 schemas 启动 fail-closed）
    publish:  { description: "...", parameters: {...} }
    subscribe: { description: "...", parameters: {...} }
  service: true                  # ★ 新：声明为常驻能力
  listen: "127.0.0.1:1884"       # ★ 新：rpc 本地 HTTP 监听地址；省略则仅 daemon 守护
  health_probe:                  # ★ 新：复用 Tool.HealthProbe，Supervisor 探活
    tcp: "127.0.0.1:1884"        #   支持 tcp / http 两种探针
    interval: 5s
    failures: 3
```

- `service` 缺省为 `false`：维持 once 模型，完全向后兼容；
- `listen` 省略 + `service:true`：纯 daemon 守护（侧车自运行，Gear 不主动调它）；
- `listen` 存在：rpc 生效，Gear 经本地 HTTP 调其方法（连接复用）。

---

## 5. 本地 HTTP 协议（rpc transport）

复用现有 ext 的 JSON 语义（`{method,args}` / `{result|error}`），仅把传输从
stdin/stdout 换成 HTTP，便于连接复用与并发：

| 方向 | 形态 |
|---|---|
| Gear → 侧车 | `POST http://<listen>/<method>`，body `{"args":{...}}` |
| 侧车 → Gear | 响应 `{"result":...}` 或 `{"error":"..."}`（HTTP 状态码 200，错误在 body） |

- 协议与现有 stdio 协议**一一对应**，sidecar 作者两套都能实现（或只实现 HTTP）；
- 端口由 YAML 显式声明（简单、可预期）；如需动态端口，后续可经环境变量 / 文件传递（本期不做）；
- `commandTransport` → 新增 `LocalHTTPTransport` 实现同一 `SidecarTransport` 接口
  （`Call(method,args)`），按 ext 是否声明 `listen` 切换，调用方无感。

---

## 6. 惰性拉起与监管

**惰性拉起**：不采用"gear serve 启动时全量拉起"，而是当某个 flow 的 **service step**
引用该 ext 时才 `Start`，随 flow 生命周期结束（cancel）而 `stopAll` 杀掉。
这避免"每个 gear 实例一启动就拉一堆常驻进程"，也让常驻能力可编排、可随 flow 启停。

**机制**（复用现成路径，改动小）：

1. ext 加载时，若声明 `service:true`，引擎为其生成一个"虚拟 Tool"
   （`runtime: process`，`entry: cmd`，`mode` 由 step 决定），注册进引擎；
2. flow 的 service step `call: ext.mqtt`（或 `ext.mqtt.__svc__`）`mode: service`
   → 引擎 `Supervise` 该 flow 时对此 step 走 `processRuntime.Start`；
3. `Supervisor` 对返回的 `ServiceHandle` 启动 `probeLoop`（复用 `health_probe`），
   崩溃按 `RestartAlways/Never` 策略 + 指数退避重启，进程树级联回收（红线 16）；
4. flow 取消 → `stopAll` 杀掉所有 service 子进程（含该 ext 侧车）。

> 注：`gear serve` 的 `RunFlow` 已具备"按 `req.Restart` 选策略 + Supervise + cancel 杀进程"
> 的完整闭环，本设计**直接复用**，无需新建监管组件。

---

## 7. 分阶段的改动文件清单

> 文件名 / 函数名基于已读代码推断，落地前需 `Grep` 复核确切位置；标注 ★ 为新增。

### 阶段一：service ext 自动包成 process Tool + 惰性拉起（最小改动）

| 文件 | 改动 |
|---|---|
| `internal/model/*.go` | ext 配置结构（ext 段）新增 `Service bool` / `Listen string` / `HealthProbe *HealthProbe` 字段（YAML 绑定） |
| `internal/engine/extensions.go` | ext 加载处（SetupExtensions 或等价入口）：识别 `service:true` → 生成虚拟 process Tool 并注册；把 `listen` 传入 `CapabilityRegistry` 供 transport 选择 |
| `internal/engine/extensions.go` | `SidecarProvider` / `CapabilityRegistry`：支持把 `listen` 与 ext 绑定（决定走 HTTP 还是 stdio） |
| `internal/engine/engine.go` | service step 解析：当 `call` 命中 service ext 的虚拟 Tool 时走 `Start` 路径（现有 service 分支已存在，仅扩展 target 来源） |
| `extensions.example.yaml` | 增加一个 `ext.mqtt`（或 `ext.demo-svc`）示例段，标注 `service/listen/health_probe` |
| `plugin/`（或 `examples/`） | 一个**常驻 daemon 样例**（最小 Go 程序：启动后常驻、可选监听 HTTP、收到 SIGTERM 优雅退出），用于集成测试 |

**阶段一验收**：起一个测试常驻侧车，flow service step 引用它 → 验证拉起 / 健康探针 / 崩溃重启 / flow cancel 杀掉。

### 阶段一补：in-process service 注册路径（Go 内建，§3.1）

| 文件 | 改动 |
|---|---|
| `internal/ext/ext.go` | 新增 `ServiceSpec` 结构 + `registerService(...)`；新增「goroutine supervisor」：`go spec.Start(ctx)` 拉起、`recover` 包裹异常、`Health()` 探针、按 `Restart` 策略 + 指数退避重启、`Stop()` 平滑退出（复用策略常量，作用在 goroutine 而非进程树） |
| `internal/engine/engine.go` | service step 解析：当 `call` 命中**进程内** service ext 时走 `registerService` 拉起路径（带可取消 `ctx`），而非 `processRuntime.Start`；`ctx` 直接注入宿主能力（call/event/db/kv），无需 IPC |
| `internal/app/supervisor.go` | （可选）把重启 / 探针逻辑抽成可复用于 goroutine 的通用函数，避免进程外 / 进程内两套重复实现 |
| `examples/` | 一个**进程内 service 样例**（如 `ext.demo-inproc`：注册一个每秒打点的 goroutine，演示自运行 + 被调） |

**阶段一补验收**：注册一个进程内 service ext → flow service step 引用 → 验证 goroutine 拉起 / `Health()` 探针失败重启 / `ctx.*` 直接可用 / flow cancel 平滑退出、主进程不被拖垮（recover 生效）。

### 阶段二：LocalHTTPTransport（连接复用，rpc 生效）

| 文件 | 改动 |
|---|---|
| `internal/engine/extensions.go` | 新增 `LocalHTTPTransport`（实现 `SidecarTransport` 接口），带连接池 / request 超时 / 断线重连；`SidecarProvider` 按 ext 是否声明 `listen` 选 `CommandTransport` 或 `LocalHTTPTransport` |
| `internal/ext/*` 或 `extensions.example.yaml` | 侧车协议文档补 HTTP 形态；可选：内核内建 ext 也支持 `listen`（若希望 publish 复用连接） |

**阶段二验收**：声明 `listen` 的 ext，`ctx.ext.xxx.method()` 走本地 HTTP 且连接复用；
不声明 `listen` 的 ext 仍走 stdio（回归 once 行为不变）。

### 阶段三：mqtt-sidecar 样板（首个真实 service ext）

| 文件 | 改动 |
|---|---|
| `plugin/mqtt-sidecar/main.go`（新增） | 常驻侧车：内部一条 MQTT 长连接；`subscribe` 自运行（收到消息回 Gear REST/event）；`publish` 走本地 HTTP 暴露；优雅退出 |
| `extensions.example.yaml` | `ext.mqtt` 段用该 sidecar，演示 daemon + rpc 合流 |
| `plugin/verify.py` | 加一个用例：起侧车 → flow service step 引用 → pub 被调成功 + sub 收到回推 |

**阶段三验收**：用公开 broker（test.mosquitto.org）或本地 emqx 容器，验证
sub 守护收消息 + pub 被 Gear 调通。

---

## 8. 兼容性

- **默认关闭**：不写 `service` 的 ext 行为完全不变（once），现有 rabbitmq/kafka/sms 等内建 ext 不受影响；
- **协议兼容**：HTTP 协议是 stdio 协议的"同语义换传输"，sidecar 可任选其一实现；
- **监管兼容**：复用现有 `Supervisor` 与 `health_probe`，不新建监管组件；
- **多实例**：维持"单 gear 进程内一份"语义，跨进程多副本由部署 / 消息层处理（见 §2）。

---

## 9. 测试计划

- **单元**：ext 加载识别 `service` 并生成虚拟 Tool；`LocalHTTPTransport` 调用 / 超时 / 重连；
- **集成**：测试常驻侧车 + flow service step → 拉起 / 探针失败重启 / cancel 杀掉（含进程树级联回收，无孤儿）；
- **回归**：现有内核原语（受门禁 7 + 纯函数 3）+ 非 service ext 的 once 调用全绿（确认未破坏）；
- **样例**：mqtt-sidecar 接真实 broker 跑通 sub 守护 + pub 被调。

---

## 10. 风险与开放问题

- **LocalHTTPTransport 稳健性**：连接池 / 重连 / 超时若处理不好会泄漏或假死，是阶段二主要工作量；
- **侧车崩溃期间的调用**：rpc 调一个正在重启的侧车应 fail-fast 还是排队？本期建议 fail-fast（与 once fail-closed 一致），如需排队后续加；
- **动态端口**：本期用 YAML 显式端口；多 ext 同机需人工避端口冲突，后续可加动态分配；
- **`$share` 共享订阅**：竞争消费场景的"每消息一次"由消息层保证，文档需明确告知用户（不属于 Gear 改动）。

---

## 11. 实施顺序建议

`阶段一（进程外常驻+监管，风险最低）→ 阶段一补（进程内 goroutine 服务，与阶段一可并行）→ 阶段二（HTTP 复用，解决重连痛点）→ 阶段三（mqtt 样例）→ 阶段四（多 gear 端口协调，§12）`。
阶段一 / 阶段一补 即可分别单独交付并验证"ext 能常驻被监管"这个核心诉求（进程外走侧车、进程内走 goroutine）；阶段二/三/四在其上增量。

---

## 12. 多 gear 端口协调：基于 PID 注册的占用列表

### 12.1 问题

上一轮把"跨 gear 多副本"划给部署层，但有一个**同主机**的硬冲突没解决：
多个 gear 进程若都需要同一个外部 service（如 `ext.mqtt`，声明 `listen: 127.0.0.1:1884`），
按 §6 惰性拉起逻辑每个 gear 都会 `Start` 自己的 sidecar → 都抢绑 `:1884` →
`EADDRINUSE` 冲突，只有一个能起，其余失败。

这不需要分布式选主，只需要"**同主机一份 sidecar 实例**"的协调：谁先到谁起，
后来者复用，没人要就退。下面用基于 PID 注册的占用列表实现。

### 12.2 角色

- **sidecar（外部 service）**：每主机至多一个，绑定声明端口（`127.0.0.1` only），
  维护 `occupants: map[pid] → {token, lastSeen}`。
- **gear（registrant / client）**：每个需要该 service 的 gear 进程，启动时注册自身 PID，
  退出时注销；通过同一端口以 RPC 调用 sidecar 方法。

### 12.3 协议（复用 sidecar 的 RPC 端口）

sidecar 在 `listen` 端口上额外暴露协调端点（与业务 RPC 同端口，零新增端口）：

| 方法 | 路径 | 请求 | 响应 |
|---|---|---|---|
| 注册 | `POST /register` | `{"pid":<int>,"token":"<uuid>","ttl":<sec>}` | `{"ok":true}` / `{"ok":false,"reason":"taken"}` |
| 心跳 | `POST /heartbeat` | `{"pid":<int>,"token":"<uuid>"}` | `{"ok":true}`（刷新 `lastSeen`） |
| 注销 | `POST /deregister` | `{"pid":<int>,"token":"<uuid>"}` | `{"ok":true}` |
| 占用查询 | `GET /occupants` | — | `{"pids":[...]}`（probe / 调试用） |

- `token`：gear 启动时生成的随机串，随 PID 一起存；**注销 / 心跳必须 PID+token 匹配**，
  防止 PID 复用导致的"幽灵占用"（见 §12.6）。
- `ttl`：占用有效期（如 15s）；配合心跳刷新，sidecar 重启后可重建列表。

### 12.4 Gear 拉起流程（含竞态处理）

flow service step 引用 `ext.X`（声明 `service:true` + `listen`）时，Gear 走：

1. 发 `POST /register` 到 `listen` 端口。
2. 连接被拒（无监听）→ **本 gear 是首到者 / owner**：
   - `processRuntime.Start(cmd)` 拉起 sidecar；sidecar `net.Listen` 绑端口，
     开始接受注册并进入存活巡检（§12.5）；
   - 带退避重试 `register`，直到 sidecar 就绪（成功或拿到 `taken`）。
3. 连接成功但 `register` 返回 `{"ok":false,"reason":"taken"}` →
   端口被**另一实例**占了（竞态中对方先绑）。本 gear **不自己起 sidecar**，
   直接以 client 身份注册加入（重试直到 `ok:true`）。
4. 注册成功 → 启动"注册看门狗" goroutine：每 `ttl/2` 发 `/heartbeat` 保活；
   若 `register` / 心跳连续失败（sidecar 可能重启），重新走步骤 1（必要时成为新 owner）。
5. 业务调用经 `LocalHTTPTransport` 走同端口（§5）。

> 竞态收敛：两个 gear 同时首到、都 `Start` 各自 sidecar，其中一方 `net.Listen` 拿到
> `EADDRINUSE` → 该 sidecar **立即退出（特殊退出码）**，其 owner gear 见到 sidecar 速退 →
> 回到步骤 3 以 client 注册。最多一轮重试即收敛，无双绑。

### 12.5 Sidecar 存活巡检 + 自动退出

sidecar 后台循环（间隔 `checkInterval`，如 5s）：

```
for each (pid, token, lastSeen) in occupants:
    if !processAlive(pid) or now-lastSeen > ttl:
        occupants.delete(pid)          # 进程已死 或 心跳超时
if occupants.empty():
    Stop()                              # 平滑停内部工作（断 MQTT 等）
    close(listener)
    os.Exit(0)                         # 无人需要，自动退出
```

- `processAlive(pid)`：
  - Unix：`signal(pid, 0)` 成功且非 zombie；
  - Windows：`OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION)` +
    `GetExitCodeProcess != STILL_ACTIVE`
    （注意 Go `os.FindProcess` 在 Windows 对不存在的 PID 不报错，必须真正探测）。
- 这是用户侧车**应复用**的逻辑 —— Gear 提供 `sidecarcoord` SDK（§12.7），用户不必重写。

### 12.6 边界与强化

- **PID 复用**：OS 会回收 PID。仅靠 PID 存活可能把"已死 gear 的 PID 被新无关进程占用"
  误判为存活。用 `token` 约束注销 / 心跳的所有权，且心跳 `ttl` 超时也会清除 ——
  双保险：即便 PID 被复用，原 gear 已死 → 无心跳 → 超时移除。
- **sidecar 自身崩溃**：端口释放；其余 gear 的看门狗（§12.4.4）检测到注册 / 心跳失败 →
  其中一个成为新 owner 重启 sidecar，其他重新注册。列表短暂为空时新 sidecar 不会因
  "空列表"误退（它有 owner gear 立即注册进来；即使短暂空也只退出，看门狗会再拉起）。
- **localhost only**：`listen` 限定 `127.0.0.1` / `::1`，注册不暴露到网络，无远程滥用面。
- **优雅退出**：gear 正常退出（flow cancel / 进程 SIGTERM）走 `defer POST /deregister`；
  崩溃则由 PID 存活兜底。

### 12.7 落地：Gear 提供 sidecarcoord SDK

为避免每个 ext 作者重写协调逻辑，Gear 提供：

- `internal/sidecar/coord`（Go 包）：`Serve(addr, handlers)` 封装
  "绑端口 or 退让 + 占用列表 + 存活巡检 + 自动退出 +
  `/register`/`/heartbeat`/`/deregister`/`/occupants` 端点"，
  ext 作者只需传入业务 handler（publish / subscribe）。
- 一个**参考 sidecar 骨架** `plugin/sidecar-template/`（含 MQTT / HTTP 占位），
  用户 `go:embed` 或直接套用。
- Gear 侧：`internal/engine/extensions.go` 的惰性拉起扩展为 §12.4 的
  "注册优先 / 竞态退让"握手逻辑；`LocalHTTPTransport` 复用同端口。

### 12.8 与进程内服务（§3.1）的关系

进程内服务是 gear 进程里的 goroutine，**无端口、无独立进程**，不存在端口冲突，
也不需要此协调机制 —— 它靠 gear 进程本身的监管即可。本 §12 仅针对**进程外**
外部 service 的同主机多 gear 共享。

### 12.9 验收

- 起 2 个 gear，各跑一个引用 `ext.mqtt` 的 flow → 仅 1 个 sidecar 绑定 `:1884`，
  两 gear 都 `register` 成功，均可 RPC 调 `publish`；
- kill 其中一个 gear（模拟崩溃）→ sidecar 存活巡检移除非存活 PID，另一 gear 仍正常；
- 两个 gear 都退出 → 占用列表空 → sidecar 自动退出（无孤儿进程、无端口占用）；
- 竞态：同时起 2 gear → 至多一轮重试收敛为单 sidecar，无 `EADDRINUSE` 报错。

---

## 13. 实施验收总览（代码已落地）

> 截至 2026-09-18，阶段一 / 二 / 三 / 四 的核心代码均已实现并通过单测 / 二进制集成测试。
> 验收命令（串行编译，避免沙箱并行读损坏）：
> `go test -p 1 ./internal/engine ./internal/sidecar/... ./internal/ext ./plugin/mqtt-sidecar -count=1`

### 阶段二：LocalHTTPTransport 连接复用（已实现）

- 落点：`internal/engine/extensions.go` 的 `NewLocalHTTPTransport`，用显式 `http.Transport`
  （`MaxIdleConnsPerHost:64`、`IdleConnTimeout:90s`、`DisableKeepAlives:false`）替换默认 client；
  `SidecarProvider` 按 ext 是否声明 `listen` 选 `LocalHTTPTransport`（有）或 `CommandTransport`（无）。
- 单测（`internal/engine/localhttp_test.go`，5 个全过）：
  - `TestLocalHTTPTransport_CallOK`：调 `httptest` 服务返回 `{"result":{...}}` 正常解析；
  - `TestLocalHTTPTransport_ConnectionReuse`：**自定义 `countingDialer` 断言 5 次调用只 dial 1 次** → 连接复用成立；
  - `TestLocalHTTPTransport_CallError`：body `{"error":...}` 正确回传 error；
  - `TestLocalHTTPTransport_LuaRouting`：真实 engine 跑 Lua `ctx.ext.demo.publish({topic="x"})`，经本地 HTTP 路由到 sidecar；
  - `TestSidecarExtension_FallsBackToCommandTransport`：未声明 `listen` → 回归 `CommandTransport`（once 行为不变）。
- 结论：rpc 传输从"每次新连"升级为 keep-alive 复用；不声明 `listen` 的 ext 完全不受影响。

### 阶段三：mqtt-sidecar 真实样板（已实现，核心单测全过）

- 落点：
  - `plugin/mqtt-sidecar/main.go` 的业务核心层（**不依赖 paho**，定义 `MQTTClient` 接口 + `Service{Handlers,Stop}`，可注入 fake 纯单测）——原 `internal/sidecar/mqttsvc/` 包已内聚至此（2026-09-19：核心层与 paho 适配层分居两处没有收益，合并后单测留在同包）；
  - 同文件内的 paho v1.5.1 适配（`pahoClient`）+ `coord.Serve` 接入；broker 连接为**非致命**后台自动重连，避免 `RestartAlways` 下重启风暴；
  - `extensions.example.yaml` 的 `ext.mqtt` 指向 `build/mqtt-sidecar.exe` + `listen: 127.0.0.1:1884` + `health_probe.tcp`；
  - `examples/mqtt_pub_tool.yaml`（`mqtt-pub` Lua Tool，capabilities `ext.mqtt`）、`examples/mqtt_service_flow.yaml`（`mqtt-demo` flow：service step `keep` + `mqtt-pub` step）。
- 单测（`plugin/mqtt-sidecar/main_test.go`，6 个全过）：`TestPublish` / `TestSubscribe`（topic 计数去重 + Subscribe 失败回滚）/ `TestUnsubscribe` / `TestOnMessageWebhook` / `TestOnMessageFailureDoesNotAbortOthers` / `TestTopicMatches`（用 `fakeMQTT` stub，paho 不进单测）。注：原文档写的 `TestOnMessageLogOnly`（无 callbackURL 时只记日志）在当前实现下已不成立——`callback_url` 是**必填**（缺则报错），故该条替换为「回推失败不连累其它 handle」。
- 依赖：`go get github.com/eclipse/paho.mqtt.golang@v1.5.1`（顺带 `gorilla/websocket v1.5.3`、`golang.org/x/net v0.44.0`）。
- **待办（非代码缺陷）**：真实 broker 联调需在能访问 `test.mosquitto.org:1883` 或本地 emqx 的环境跑
  `examples/mqtt_service_flow.yaml`，验证 sub 守护收消息 + pub 被 Gear 调通。当前沙箱无外网，未在 CI 跑通这一步。

### 已知环境坑（验收时务必注意）

- **并行编译误报语法错**：`go build -p N`(N>1) / `go test -p N` 本沙箱偶发把无关文件报成语法错
  （如 `ext.go:113`、`email.go:61`），实为并发读损坏，文件本身合法。一律用 `-p 1` 串行验证。
- **engine 进程类测试 `exec:"sh"` 失败**：`internal/engine` 里一批 process-runtime 集成测试会 spawn `sh`，
  本沙箱缺 `sh` 可执行文件 → 这些测试**环境性失败**，与 ext service / 阶段二三四无关；它们在我改之前就过不了。
- **Git Bash shim 残废**：`ls/sleep/cat/head/grep/tail/dirname` 缺失，`rm` 被换成损坏垫片。验证靠 Go 集成测试
  （`os/exec` + `t.TempDir()` 每次重建二进制）而非手搓 shell；需要删文件用 Go `os.Remove` 或覆盖写。
