#!/usr/bin/env python3
"""plugin/verify.py —— 可扩展原子（P1 通知 + P2 消息/短信）与应用生命周期端到端验证。

不依赖任何外部服务即可证明协议正确 + 真实跑通 Gear 调用链路。

本期（2026-09-18）webhook / rabbitmq / kafka 已升级为「常驻能力（service ext）」：
  - 侧车长驻，能力调用走本地 HTTP（LocalHTTPTransport，复用长连接）；
  - webhook.subscribe / rabbitmq.consume / kafka.consume 经通用订阅原语 ctx.subscribe
    回推事件，本脚本直接起侧车验证「监听一个 URL / 消费队列」的路径。
email / wecom / sms 仍是一次型（once）原子，保持原 stdin/stdout 验证。

P1：
  1) ext.webhook ：起侧车，经 /send 真实 POST 接收（service 路径）
  1b) ext.webhook.subscribe：/subscribe 登记入站路由 + POST /in/{path} -> 侧车回推（监听一个 URL）
  2) ext.email ：log 模式真实落盘（代码路径与真实 SMTP 一致，仅 sink 不同）
  3) ext.wecom ：用假 key 真实请求 -> 必须返回 {"error":...}（fail-closed 证明）
  4) Gear 链路 ：gear tool run notify.all，ctx.ext.webhook.send 经侧车真实跑通
P2：
  5) ext.rabbitmq ：起侧车，连 localhost 无 broker -> /publish 真实失败 -> {"error":...}（fail-closed）
  6) ext.kafka    ：起侧车，连 localhost 无 broker -> /produce 真实失败 -> {"error":...}（fail-closed）
  7) ext.sms      ：真实 POST 到本地 sink -> 200 接收
  8) Gear 链路    ：gear tool run messaging.fanout，kafka 失败被 pcall 隔离、sms 成功
红线：
  9) methods/schemas 不一致的 ext 配置，gear 启动必须 fail-closed
 10) 统一注册表 plugin/extensions.yaml 加载后，gear ai capabilities 应含全部 6 个 ext
P4（应用生命周期，2026-09-21 补）：
 11) gear app start：派生**独立后台进程**后立即返回 → 父命令退出后端口仍服务（自持）
     → 同 flow 二次 start 幂等 → status/list 如实报 running → stop 终止进程并**真释放端口**。
     这是 MCP 面 gear_app(start) 的同一条链路（两者共用 internal/app），
     即「通过 MCP 创建并运行一个应用」落到**真进程**上的端到端证据。
     关键前提：app 必须**自己**持有 ext.serve 的 coord 端口。若沿用本脚本常驻侧车已听的
     8095，app 的 service 步会复用那个监听、随即完成 → app 进程秒退，而端口照样在服务
     ——「自持」就成了假象（2026-09-21 实测踩到：served=True 却来自别人的侧车）。
     故此处给 app 一份**独立 extensions 副本**（listen 换空闲端口 + cmd 换绝对路径）。
"""
import os
import re
import sys
import json
import socket
import subprocess
import tempfile
import threading
import time
import urllib.request
import urllib.error
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
GEAR = os.path.join(ROOT, "build", "gear.exe")
EXT = os.path.join(ROOT, "plugin", "extensions.yaml")
NOTIFY_SEED = os.path.join(ROOT, "plugin", "notify_demo.yaml")
MSG_SEED = os.path.join(ROOT, "plugin", "messaging_demo.yaml")
CONSUME_SEED = os.path.join(ROOT, "plugin", "consume_demo.yaml")

# service ext 侧车端口（与 extensions.yaml 的 listen 一致）
WEBHOOK_COORD = "127.0.0.1:8089"
WEBHOOK_INBOUND = "127.0.0.1:8088"
RABBITMQ_COORD = "127.0.0.1:5674"
KAFKA_COORD = "127.0.0.1:9094"
SERVE_COORD = "127.0.0.1:8095"

RESULTS = []


def check(name, ok, detail=""):
    RESULTS.append((name, ok, detail))
    print(("[PASS] " if ok else "[FAIL] ") + name + (("  -> " + detail) if detail else ""))


class Sink(BaseHTTPRequestHandler):
    received = []

    def do_POST(self):
        n = int(self.headers.get("Content-Length", 0))
        body = self.rfile.read(n).decode("utf-8", "ignore")
        Sink.received.append(body)
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(b'{"ok":true}')

    def log_message(self, *a):
        pass


def start_sink():
    srv = ThreadingHTTPServer(("127.0.0.1", 0), Sink)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    return srv, srv.server_address[1]


class ApiMock(BaseHTTPRequestHandler):
    """最小后端桩：把收到的请求路径回写成 JSON，供 ext.serve 反代断言（8c）。"""

    def do_GET(self):
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(json.dumps({"path": self.path, "from": "backend"}).encode("utf-8"))

    def log_message(self, *a):
        pass


def start_api():
    srv = ThreadingHTTPServer(("127.0.0.1", 0), ApiMock)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    return srv, srv.server_address[1]


def run_gear(args, env=None, stdin=None, cwd=ROOT):
    # stdin 若为 dict/list 则 JSON 序列化；若为字符串（YAML 种子）则原样以字节传入，
    # 不能 json.dumps 字符串——那会把整段 YAML 变成 JSON 字符串导致 gear 解析失败。
    inp = None
    if stdin is not None:
        if isinstance(stdin, (dict, list)):
            inp = json.dumps(stdin).encode("utf-8")
        else:
            inp = stdin.encode("utf-8")
    p = subprocess.run(
        [GEAR] + args,
        input=inp,
        env=env or os.environ,
        cwd=cwd,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        timeout=120,
    )
    return p.returncode, p.stdout.decode("utf-8", "ignore")


def run_ext(name, req, env=None):
    """调用内置 sidecar 载体 `gear ext <name>`：stdin 写 {method,args}，读末行 JSON。"""
    p = subprocess.run(
        [GEAR, "ext", name],
        input=json.dumps(req).encode("utf-8"),
        env=env or os.environ,
        cwd=ROOT,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        timeout=30,
    )
    return p.stdout.decode("utf-8", "ignore")


def parse_ext(out):
    """gear ext 输出末行 JSON（取最后一个非空行，容忍其它噪声）。"""
    for line in reversed(out.strip().splitlines()):
        line = line.strip()
        if line:
            try:
                return json.loads(line)
            except Exception:
                pass
    return {"raw": out}


def http_post_json(url, obj, timeout=10):
    """POST JSON 到侧车，返回解析后的 dict（或 {"error": <str>}）。"""
    data = json.dumps(obj).encode("utf-8")
    req = urllib.request.Request(url, data=data, headers={"Content-Type": "application/json"}, method="POST")
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            return json.loads(resp.read().decode("utf-8", "ignore") or "{}")
    except urllib.error.HTTPError as e:
        body = e.read().decode("utf-8", "ignore")
        try:
            return json.loads(body or "{}")
        except Exception:
            return {"error": body}
    except Exception as e:  # 连接被拒 / 超时等
        return {"error": str(e)}


def http_get_json(url, timeout=10):
    """GET JSON（用于 /caps 这类内省端点），返回解析后的 dict（或 {"error": <str>}）。"""
    try:
        with urllib.request.urlopen(url, timeout=timeout) as resp:
            return json.loads(resp.read().decode("utf-8", "ignore") or "{}")
    except Exception as e:
        return {"error": str(e)}


def start_sidecar(binary, coord_addr):
    """起侧车子进程，并向 coord 注册一个 dummy 占用 + 后台心跳（防止 OnEmpty 自动退出），返回 proc。

    关键点：
      - 占用 pid 必须用**真实**的 os.getpid()：coord 的巡检会调 processAlive(pid)，对假 PID（如 999999）
        在 Windows 上 OpenProcess 直接失败 → 判为不存活 → 占用项被剔除 → 触发 OnEmpty 侧车自停。
        用本进程 PID 则整轮 verify 期间 processAlive 恒为 true。
      - coord 占用项 TTL 被钳到 2×serverTTL（默认 30s），不心跳会在 30s 后被巡检剔除，故起守护线程
        周期 /heartbeat 刷新 lastSeen，保证整轮 verify（尤其最后才跑的 P3 ext.serve）期间侧车不被回收。
    """
    proc = subprocess.Popen(
        [binary],
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
        cwd=ROOT,
    )
    time.sleep(1.0)  # 等侧车绑定端口
    pid = os.getpid()
    http_post_json("http://%s/register" % coord_addr,
                   {"pid": pid, "token": "verify-keepalive", "ttl": 3600})

    def _beat():
        while True:
            try:
                http_post_json("http://%s/heartbeat" % coord_addr,
                               {"pid": pid, "token": "verify-keepalive"}, timeout=2)
            except Exception:
                pass
            time.sleep(1)

    threading.Thread(target=_beat, daemon=True).start()
    return proc


def build_sidecar(rel, outname):
    """若侧车二进制缺失则构建（幂等）。"""
    out = os.path.join(ROOT, "build", outname)
    if os.path.exists(out):
        return
    env = dict(os.environ)
    env["GOPROXY"] = "goproxy.cn,direct"
    env["GOFLAGS"] = "-p=1"
    subprocess.run(["go", "build", "-o", out, rel], cwd=ROOT, env=env, check=True)


def parse_ext_methods(path):
    """从 extensions.yaml 解析 {ext 名: [方法名...]}（零依赖正则，不引入 pyyaml）。

    为什么必须**现读**而不是在测试里硬编码一份期望值：硬编码副本自身会漂移，于是「声明 ⊆ 实现」
    这条红线退化成「测试的错期望 ⊆ 实现」，两边一起错还能互证通过。2026-09-19 就真的发生过——
    把 yaml 对齐到测试里写错的 `listen`，而没有对齐侧车 `/caps` 的 `subscribe`，
    结果 `ctx.ext.webhook.listen` POST 到一个侧车根本不存在的路由（404）却全程无人发现。
    """
    out = {}
    cur = None
    try:
        with open(path, "r", encoding="utf-8") as f:
            for line in f:
                m = re.match(r"^ext\.([A-Za-z0-9_]+)\s*:\s*$", line)
                if m:
                    cur = "ext." + m.group(1)
                    continue
                if cur is None:
                    continue
                m = re.match(r"^\s+methods\s*:\s*\[(.*)\]", line)
                if m:
                    out[cur] = [x.strip() for x in m.group(1).split(",") if x.strip()]
    except OSError:
        return {}
    return out


def main():
    # 环境坑：本机 HTTP_PROXY=http://127.0.0.1:xxxxx 会拦截所有 127.0.0.1 调用（Go http.Client
    # 出站 / 侧车回推 / urllib 探针全中招，表现为 502 / WinError 10054）。直连侧车与 gear 都走
    # 127.0.0.1，必须把 localhost 排除出代理，并清掉 HTTP(S)_PROXY 以免 Go 客户端走代理。
    os.environ["NO_PROXY"] = "127.0.0.1,localhost"
    os.environ["no_proxy"] = "127.0.0.1,localhost"
    for _k in ("HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"):
        os.environ.pop(_k, None)

    if not os.path.exists(GEAR):
        print("gear 二进制缺失，先构建...")
        subprocess.run(["go", "build", "-o", "build/gear.exe", "./cmd/gear"], cwd=ROOT, check=True)

    # 构建三个常驻侧车
    build_sidecar("./plugin/webhook-sidecar", "webhook-sidecar.exe")
    build_sidecar("./plugin/rabbitmq-sidecar", "rabbitmq-sidecar.exe")
    build_sidecar("./plugin/kafka-sidecar", "kafka-sidecar.exe")
    build_sidecar("./plugin/serve-sidecar", "serve-sidecar.exe")

    srv, port = start_sink()
    sink_url = "http://127.0.0.1:%d/sink" % port

    # 反代用的后端桩（供 8c 断言 ext.serve 的 proxy 规则真的把 /api 转了出去）
    api_srv, api_port = start_api()
    api_url = "http://127.0.0.1:%d" % api_port

    tmp = tempfile.mkdtemp(prefix="ext-")
    reg = os.path.join(tmp, "ext.db")
    # 应用生命周期场景（P4）用**独立**的 registry 与台账目录：不与本轮的 ext 注册混在一起，
    # 也让「stop 后端口释放」有一个干净的前后对照。
    app_reg = os.path.join(tmp, "app.db")
    app_apps = os.path.join(tmp, "apps")
    started_apps = []  # 供 finally 兜底回收（stop 失败时不留残留后台进程）
    email_log = os.path.join(tmp, "email.out.log")

    env = dict(os.environ)
    env["GEAR_EXT_CONFIG"] = EXT

    # 前置：coord 端口必须空闲。若有残留侧车占着端口，我们起的实例会「退让退出」（ExitAddrInUse），
    # 于是整轮 verify 实际打的是**那个旧进程——很可能是旧二进制**：结果看似通过却毫无意义
    # （2026-09-20 真踩过：用旧 serve-sidecar 跑，proxy 全 404 却以为是代码问题）。
    # 宁可在这里明确失败，也不要静默跑出一份不可信的报告。
    for _name, _addr in (("webhook", WEBHOOK_COORD), ("rabbitmq", RABBITMQ_COORD),
                         ("kafka", KAFKA_COORD), ("serve", SERVE_COORD)):
        _host, _, _port = _addr.partition(":")
        _s = socket.socket()
        _s.settimeout(0.5)
        try:
            _s.connect((_host, int(_port)))
            print("[FATAL] %s 侧车已在 %s 上运行（残留进程）：本轮会打到旧二进制，结论不可信。" % (_name, _addr))
            print("        请先结束残留侧车再重跑，例如：")
            print("        taskkill /F /IM serve-sidecar.exe /IM webhook-sidecar.exe"
                  " /IM rabbitmq-sidecar.exe /IM kafka-sidecar.exe")
            sys.exit(2)
        except SystemExit:
            raise
        except Exception:
            pass
        finally:
            _s.close()

    # 起三个常驻侧车（webhook 同时监听 coord:8089 与 inbound:8088）
    wh = start_sidecar(os.path.join(ROOT, "build", "webhook-sidecar.exe"), WEBHOOK_COORD)
    rmq = start_sidecar(os.path.join(ROOT, "build", "rabbitmq-sidecar.exe"), RABBITMQ_COORD)
    kfk = start_sidecar(os.path.join(ROOT, "build", "kafka-sidecar.exe"), KAFKA_COORD)
    sv = start_sidecar(os.path.join(ROOT, "build", "serve-sidecar.exe"), SERVE_COORD)

    # 所有起过的侧车都要登记：8b 可能**再拉起一个** serve 实例（占用心跳偶发超时兜底），
    # 若只 terminate 最后那个变量，前一个就变成残留进程——它会一直占着 coord 端口，
    # 让下一轮 verify 起的新侧车「退让退出」，于是跑的还是**旧二进制**（结果假绿/假红）。
    procs = [wh, rmq, kfk, sv]

    try:
        # ---- 0) 红线：extensions.yaml 声明的方法，必须都被侧车真正实现（/caps）----
        # 堵住「yaml 能声明一个侧车根本没实现的方法」的缺口。期望值**从权威 schema 现读**
        # （不再硬编码——硬编码副本会与 yaml 各自漂移，见 parse_ext_methods 注释）。
        _decl = parse_ext_methods(EXT)
        caps_expect = {
            "ext.webhook": WEBHOOK_COORD,
            "ext.rabbitmq": RABBITMQ_COORD,
            "ext.kafka": KAFKA_COORD,
            "ext.serve": SERVE_COORD,
        }
        for _name, _coord in caps_expect.items():
            _want = set(_decl.get(_name, []))
            # fail-closed：解析不到（文件缺失/格式变了）必须报错，**不能拿空集去比**——
            # 空集对任何集合都是子集，会静默通过，红线形同虚设。
            if not _want:
                check("红线：%s 声明的方法都被侧车实现(/caps)" % _name, False,
                      "无法从 %s 解析出 %s 的 methods（文件缺失或格式变化）" % (EXT, _name))
                continue
            _cap = http_get_json("http://%s/caps" % _coord)
            _got = set(_cap.get("result", {}).get("methods", []) or [])
            check("红线：%s 声明的方法都被侧车实现(/caps)" % _name, _want <= _got,
                  "want=%s got=%s" % (sorted(_want), sorted(_got)))

        # ---- 1) ext.webhook 经侧车 /send 真实 POST 接收（service 路径）----
        # 注意：直接打侧车用的是 Gear 的 LocalHTTPTransport 线格式，body 必须包成 {"args": {...}}。
        j = http_post_json("http://%s/send" % WEBHOOK_COORD,
                           {"args": {"url": sink_url, "payload": {"event": "onboard", "who": "E2001"}}})
        ok = "result" in j and Sink.received
        check("P1 ext.webhook 经侧车 send 真实 POST 接收", ok, "sink 收到 %d 条" % len(Sink.received))

        # ---- 1b) ext.webhook.subscribe：/subscribe + POST /in/{path} -> 侧车回推 ----
        before = len(Sink.received)
        js = http_post_json("http://%s/subscribe" % WEBHOOK_COORD,
                            {"args": {"handle": "h1", "callback_url": sink_url, "path": "demo"}})
        subscribed = "result" in js
        # 外部系统向入站端口 POST
        http_post_json("http://%s/in/demo" % WEBHOOK_INBOUND,
                       {"event": "ping", "ts": 123})
        # 轮询侧车回推（异步）
        pushed = False
        for _ in range(30):
            if len(Sink.received) > before:
                pushed = True
                break
            time.sleep(0.1)
        check("P1 ext.webhook.subscribe 监听入站 URL 回推事件", subscribed and pushed,
              "subscribed=%s 回推=%s" % (subscribed, pushed))

        # ---- 1c) ext.kafka.consume / ext.rabbitmq.consume 直接调用（不再只经 ctx.subscribe 特例）----
        # pull 是直接可调的一等方法；无 broker 时必须 fail-closed（不是静默成功）。
        j = http_post_json("http://%s/consume" % KAFKA_COORD,
                           {"args": {"topic": "hr", "brokers": "127.0.0.1:9092",
                                     "mode": "pull", "max_messages": 1, "timeout": "1s"}})
        check("P2 ext.kafka.consume pull 无 broker fail-closed", "error" in j, "error: " + str(j.get("error", ""))[:70])

        j = http_post_json("http://%s/consume" % KAFKA_COORD, {"args": {"brokers": "127.0.0.1:9092"}})
        check("P2 ext.kafka.consume 缺 topic fail-closed", j.get("error") == "topic 必填", "实际 %s" % j)

        j = http_post_json("http://%s/consume" % KAFKA_COORD, {"args": {"mode": "peek"}})
        check("P2 ext.kafka.consume 未知 mode 被拒", "未知的 consume mode" in str(j.get("error", "")), "实际 %s" % j)

        j = http_post_json("http://%s/consume" % KAFKA_COORD,
                           {"args": {"mode": "push", "topic": "hr", "brokers": "127.0.0.1:9092"}})
        check("P2 ext.kafka.consume push 缺 callback_url fail-closed",
              j.get("error") == "缺少 callback_url", "实际 %s" % j)

        j = http_post_json("http://%s/consume" % RABBITMQ_COORD, {"args": {}})
        check("P2 ext.rabbitmq.consume 缺 queue fail-closed", "缺少 queue" in str(j.get("error", "")), "实际 %s" % j)

        j = http_post_json("http://%s/consume" % RABBITMQ_COORD,
                           {"args": {"queue": "q", "host": "127.0.0.1", "port": 1}})
        check("P2 ext.rabbitmq.consume 无 broker fail-closed", "连接失败" in str(j.get("error", "")), "实际 %s" % j)

        # ---- 1d) Gear 链路：直接调 consume（证明 schema 接受、方法存在，非「不支持/未知参数」）----
        with open(CONSUME_SEED, encoding="utf-8") as f:
            seed = f.read()
        rc, out = run_gear(["tool", "register", "--registry", reg, "-"], env=env, stdin=seed)
        check("P2 gear tool register (consume.probe)", rc == 0, "rc=%d" % rc)
        rc, out = run_gear(
            ["tool", "run", "consume.probe", "--registry", reg, "--input",
             json.dumps({"topic": "hr", "kafka_brokers": "127.0.0.1:9092", "queue": "hr-q"})],
            env=env,
        )
        called = ("kafka" in out and "rabbitmq" in out)
        schema_ok = ("不支持的方法" not in out) and ("未知参数" not in out)
        check("P2 gear tool run consume.probe（consume 直接可调）", rc == 0 and called and schema_ok,
              "rc=%d called=%s schema_ok=%s" % (rc, called, schema_ok))

        # ---- 2) ext.email log 模式落盘 ----
        j = parse_ext(run_ext("email", {"method": "send",
                                        "args": {"to": "mgr@acme.com", "subject": "入职提醒", "body": "欢迎加入",
                                                 "mode": "log", "log_path": email_log}}, env=env))
        log_ok = "result" in j and os.path.exists(email_log) and "入职提醒" in open(email_log, encoding="utf-8").read()
        check("P1 ext.email log 模式落盘", log_ok, "email.out.log 含主题")

        # ---- 3) ext.wecom 失败路径 fail-closed ----
        j = parse_ext(run_ext("wecom", {"method": "send", "args": {"key": "bogus-key-000", "content": "hi"}}))
        check("P1 ext.wecom 失败路径 fail-closed", "error" in j, "返回 error: " + str(j.get("error", ""))[:80])

        # ---- 4) Gear 链路：register + run 真实调用（webhook 走侧车）----
        with open(NOTIFY_SEED, encoding="utf-8") as f:
            seed = f.read()
        rc, out = run_gear(["tool", "register", "--registry", reg, "-"], env=env, stdin=seed)
        check("P1 gear tool register", rc == 0, "rc=%d" % rc)

        rc, out = run_gear(
            ["tool", "run", "notify.all", "--registry", reg, "--input",
             json.dumps({"to": "mgr@acme.com", "subject": "Gear 通知联调", "body": "三个原子打通",
                         "webhook_url": sink_url, "wecom_key": "bogus-key-000",
                         "email_mode": "log", "email_log_path": email_log})],
            env=env,
        )
        email_ok = ("email" in out and "delivered" in out)
        webhook_ok = ("webhook" in out and len(Sink.received) >= 2)
        check("P1 gear tool run 调用三原子（webhook 经侧车）", rc == 0 and email_ok and webhook_ok,
              "rc=%d email=%s webhook=%s" % (rc, email_ok, webhook_ok))

        # ---- 5) ext.rabbitmq 连 localhost 无 broker -> 侧车 /publish 失败 -> fail-closed ----
        j = http_post_json("http://%s/publish" % RABBITMQ_COORD,
                           {"args": {"routing_key": "onboard", "body": {"id": "E2001"},
                            "url": "amqp://guest:guest@127.0.0.1:5672/"}})
        check("P2 ext.rabbitmq 连接失败 fail-closed", "error" in j, "error: " + str(j.get("error", ""))[:80])

        # ---- 6) ext.kafka 连 localhost 无 broker -> 侧车 /produce 失败 -> fail-closed ----
        j = http_post_json("http://%s/produce" % KAFKA_COORD,
                           {"args": {"topic": "hr", "value": {"id": "E2001"}, "brokers": "127.0.0.1:9092"}})
        check("P2 ext.kafka 连接失败 fail-closed", "error" in j, "error: " + str(j.get("error", ""))[:80])

        # ---- 7) ext.sms 真实 POST 到 sink -> 200 ----
        j = parse_ext(run_ext("sms", {"method": "send",
                                      "args": {"gateway": sink_url, "phone": "13800000000", "content": "事件通知"}}))
        ok = "result" in j and Sink.received
        check("P2 ext.sms 真实 POST 到网关", ok, "sink 收到 %d 条" % len(Sink.received))

        # ---- 8) Gear 链路：messaging.fanout（kafka 失败被 pcall 隔离、sms 成功）----
        with open(MSG_SEED, encoding="utf-8") as f:
            seed = f.read()
        rc, out = run_gear(["tool", "register", "--registry", reg, "-"], env=env, stdin=seed)
        check("P2 gear tool register (messaging)", rc == 0, "rc=%d" % rc)
        rc, out = run_gear(
            ["tool", "run", "messaging.fanout", "--registry", reg, "--input",
             json.dumps({"topic": "hr", "phone": "13800000000", "sms_gateway": sink_url})],
            env=env,
        )
        sms_ok = ("sms" in out and len(Sink.received) >= 1)
        check("P2 gear tool run messaging.fanout（kafka 经侧车隔离）", rc == 0 and sms_ok,
              "rc=%d sms=%s" % (rc, sms_ok))

        # ---- 8b) ext.serve：起静态网站服务 → 浏览器可访问 → 停止释放端口（P3 文件/网站服务）----
        # 健壮性：若 P1/P2 长耗时期间 serve 侧车因占用心跳偶发超时而 OnEmpty 自停，
        # 这里先探测 /caps，已退出则重新拉起专用实例，避免 P3 偶发失败。
        if "result" not in http_get_json("http://%s/caps" % SERVE_COORD):
            sv = start_sidecar(os.path.join(ROOT, "build", "serve-sidecar.exe"), SERVE_COORD)
            procs.append(sv)
            time.sleep(1.2)
        s0 = socket.socket()
        s0.bind(("127.0.0.1", 0))
        serve_port = s0.getsockname()[1]
        s0.close()
        site = tempfile.mkdtemp(prefix="site-")
        with open(os.path.join(site, "index.html"), "w", encoding="utf-8") as f:
            f.write("<h1>hello gear serve</h1>")
        j = http_post_json("http://%s/start" % SERVE_COORD,
                           {"args": {"port": serve_port, "dir": site}})
        started = "result" in j and "url" in j.get("result", {})
        fetched = False
        serr = ""
        if started:
            try:
                with urllib.request.urlopen("http://127.0.0.1:%d/index.html" % serve_port, timeout=5) as r:
                    fetched = (r.read().decode("utf-8", "ignore") == "<h1>hello gear serve</h1>")
            except Exception as e:
                serr = str(e)
        jstop = http_post_json("http://%s/stop" % SERVE_COORD, {"args": {}})
        stopped = "result" in jstop
        check("P3 ext.serve 起静态网站并可访问后停止", started and fetched and stopped,
              "start=%s fetch=%s stop=%s %s" % (started, fetched, stopped, serr))

        # ---- 8c) ext.serve 反向代理（proxy 配置）：/api 转发到后端、静态照常、非法规则 fail-closed ----
        # 这是「UI 同源 /api 全 404」的修复面：起服时给 proxy 规则，命中前缀的请求进后端，
        # 其余仍走静态文件；规则写错（如 path 不以 / 开头）必须报错，不得静默丢弃。
        s1 = socket.socket()
        s1.bind(("127.0.0.1", 0))
        proxy_port = s1.getsockname()[1]
        s1.close()
        j2 = http_post_json("http://%s/start" % SERVE_COORD,
                            {"args": {"port": proxy_port, "dir": site,
                                      "proxy": [{"path": "/api", "target": api_url}]}})
        started2 = "result" in j2
        echoed = j2.get("result", {}).get("proxy") if started2 else None
        proxied = False
        static_ok = False
        perr = ""
        if started2:
            try:
                with urllib.request.urlopen("http://127.0.0.1:%d/api/healthz" % proxy_port, timeout=5) as r:
                    body = json.loads(r.read().decode("utf-8", "ignore"))
                    proxied = (body.get("path") == "/api/healthz" and body.get("from") == "backend")
            except Exception as e:
                perr = str(e)
            try:
                with urllib.request.urlopen("http://127.0.0.1:%d/index.html" % proxy_port, timeout=5) as r:
                    static_ok = (r.read().decode("utf-8", "ignore") == "<h1>hello gear serve</h1>")
            except Exception as e:
                perr += " static:" + str(e)
        # 规则非法（path 缺前导 /）必须 fail-closed
        jbad = http_post_json("http://%s/start" % SERVE_COORD,
                              {"args": {"port": proxy_port, "dir": site,
                                        "proxy": [{"path": "api", "target": api_url}]}})
        bad_rejected = "error" in jbad
        http_post_json("http://%s/stop" % SERVE_COORD, {"args": {}})
        check("P3 ext.serve 反向代理：/api 转发后端 + 静态照常 + 非法规则 fail-closed",
              started2 and bool(echoed) and proxied and static_ok and bad_rejected,
              "start=%s echo=%s proxy=%s static=%s bad_reject=%s %s"
              % (started2, bool(echoed), proxied, static_ok, bad_rejected, perr))

        # ---- 8d) ext.serve 多站点：同一侧车同时托管两个网站，按端口停其一（2026-09-24 修复面）----
        # 旧代码是单站点：第二个 /start 被「幂等」静默吞掉（返回第一个站的 url），
        # 多 app 共享侧车时第二个应用永远起不来。多站点语义：两站共存可访问、
        # /stop 带 port 只停该站、同端口不同配置 fail-closed 报错。
        def free_tcp_port():
            s_ = socket.socket()
            s_.bind(("127.0.0.1", 0))
            p_ = s_.getsockname()[1]
            s_.close()
            return p_
        ms_a, ms_b = free_tcp_port(), free_tcp_port()
        siteA, siteB = tempfile.mkdtemp(prefix="ms-a-"), tempfile.mkdtemp(prefix="ms-b-")
        with open(os.path.join(siteA, "index.html"), "w", encoding="utf-8") as f:
            f.write("multi-A")
        with open(os.path.join(siteB, "index.html"), "w", encoding="utf-8") as f:
            f.write("multi-B")
        ja = http_post_json("http://%s/start" % SERVE_COORD, {"args": {"port": ms_a, "dir": siteA}})
        jb = http_post_json("http://%s/start" % SERVE_COORD, {"args": {"port": ms_b, "dir": siteB}})
        both_started = "result" in ja and "result" in jb
        fetched_a = fetched_b = b_gone_after_stop = a_alive_after_stop = conflict_ok = False
        if both_started:
            try:
                with urllib.request.urlopen("http://127.0.0.1:%d/index.html" % ms_a, timeout=5) as r:
                    fetched_a = r.read().decode("utf-8", "ignore") == "multi-A"
                with urllib.request.urlopen("http://127.0.0.1:%d/index.html" % ms_b, timeout=5) as r:
                    fetched_b = r.read().decode("utf-8", "ignore") == "multi-B"
            except Exception as e:
                serr = str(e)
            # 同端口换 dir → 必须报错（fail-closed）
            jc = http_post_json("http://%s/start" % SERVE_COORD, {"args": {"port": ms_a, "dir": siteB}})
            conflict_ok = "error" in jc and "已被另一站点占用" in jc.get("error", "")
            # 按端口只停 B
            jst = http_post_json("http://%s/stop" % SERVE_COORD, {"args": {"port": ms_b}})
            if "result" in jst:
                try:
                    with urllib.request.urlopen("http://127.0.0.1:%d/index.html" % ms_a, timeout=5) as r:
                        a_alive_after_stop = r.read().decode("utf-8", "ignore") == "multi-A"
                except Exception as e:
                    a_alive_after_stop = False; serr = str(e)
                try:
                    urllib.request.urlopen("http://127.0.0.1:%d/index.html" % ms_b, timeout=5)
                    b_gone_after_stop = False
                except Exception:
                    b_gone_after_stop = True
        http_post_json("http://%s/stop" % SERVE_COORD, {"args": {}})
        check("P3 ext.serve 多站点：两站共存 + 按端口停其一 + 同端口换配置 fail-closed",
              both_started and fetched_a and fetched_b and conflict_ok
              and b_gone_after_stop and a_alive_after_stop,
              "started=%s fetchA=%s fetchB=%s conflict=%s stopB(gone=%s, aAlive=%s) %s"
              % (both_started, fetched_a, fetched_b, conflict_ok,
                 b_gone_after_stop, a_alive_after_stop, serr))

        # ---- 11) 应用生命周期（REQ-APP-LAUNCH）：MCP 面的 gear_app(start) 与 CLI `app start`
        #          共用 internal/app **同一份实现**，故这里用真实二进制验同一条链路：
        #          派生独立后台进程 → 立即返回 → 端口真能服务 → 幂等 → stop 释放端口。
        #          这是「通过 MCP 创建并运行应用」的最后一步落在一个**真进程**上的证据。
        #
        # 为什么用独立 registry/台账：不与本轮的 ext 注册混在一起，也让「stop 后端口释放」
        # 有干净的前后对照（同库同名 Flow 的干扰会让失败归因变得不可能）。
        app_site = os.path.join(tmp, "appsite")
        os.makedirs(app_site, exist_ok=True)
        with open(os.path.join(app_site, "index.html"), "w", encoding="utf-8") as f:
            f.write("<h1>hello gear app</h1>")
        _sa = socket.socket()
        _sa.bind(("127.0.0.1", 0))
        app_port = _sa.getsockname()[1]
        _sa.close()

        # app 专用 extensions 副本 —— 两处必须改，否则「自持」根本验不出来：
        #   ① listen 换成空闲端口：否则 app 的 ext.serve 会复用本轮常驻侧车已在听的 8095，
        #      service 步立即完成、app 进程秒退（端口照样被服务，但那是**别人的**侧车在服务，
        #      于是 status 看到的 pid 已死、端口仍在听 → unknown 而非 running）。
        #   ② cmd 换成绝对路径：原值 `plugin/serve-sidecar.exe` 是相对 cwd 的，而派生进程的
        #      cwd 未必是 build/，找不到侧车就等于 service 步直接失败。
        _sc = socket.socket()
        _sc.bind(("127.0.0.1", 0))
        app_coord_port = _sc.getsockname()[1]
        _sc.close()
        _ext_raw = open(EXT, encoding="utf-8").read()
        # 锚点唯一性 fail-closed：extensions.yaml 形状变了就必须**显式失败**，而不是静默生成
        # 一份「没改到点子上」的副本，让整个场景悄悄失去意义（那正是本轮踩过的坑）。
        if _ext_raw.count('listen: "127.0.0.1:8095"') != 1:
            raise RuntimeError("extensions.yaml 的 ext.serve listen 锚点不唯一，"
                               "无法生成 app 专用副本（形状变了？）")
        if _ext_raw.count('cmd: ["plugin/serve-sidecar.exe"]') != 1:
            raise RuntimeError("extensions.yaml 的 serve cmd 锚点不唯一，"
                               "无法生成 app 专用副本（形状变了？）")
        _sidecar_abs = os.path.join(ROOT, "build", "plugin", "serve-sidecar.exe")
        _ext_patched = _ext_raw.replace('listen: "127.0.0.1:8095"',
                                        'listen: "127.0.0.1:%d"' % app_coord_port)
        _ext_patched = _ext_patched.replace('cmd: ["plugin/serve-sidecar.exe"]',
                                            'cmd: ["%s"]' % _sidecar_abs.replace(os.sep, "/"))
        ext_app = os.path.join(tmp, "extensions_app.yaml")
        with open(ext_app, "w", encoding="utf-8") as f:
            f.write(_ext_patched)
        app_env = dict(env)
        app_env["GEAR_EXT_CONFIG"] = ext_app

        # 自包含 bundle：anchor（once 排序锚点）+ 常驻 App Flow。
        # 为什么必须有 anchor：service step **不能被依赖**（引擎显式拒绝），没有它 site 会与
        # serve 落在**同一层并发**，/start 打在侧车完成 coord 注册之前 → connection refused。
        # anchor 把 site 推到下一层，等 serve 注册成功再跑。
        # 站点目录写**绝对路径**：派生出去的后台进程 cwd 与本进程无关，相对路径必然找不到。
        anchor_yaml = """id: app_demo.anchor
name: 排序锚点（不做事，只把后续 once 步推到下一层）
runtime: lua
input_schema:
  type: object
output_schema:
  type: object
versions:
  - version: 1.0.0
    state: stable
entry: |
  return {ok = true}
"""
        flow_yaml = """id: app_demo.up
name: 验证用常驻应用（ext.serve 托管静态站）
steps:
  - id: serve
    call: ext.serve
    mode: service
    input: {}
  - id: anchor
    call: app_demo.anchor
    input: {}
  - id: site
    call: static_site.serve
    depends_on: [anchor]
    input:
      port: %d
      dir: "%s"
      host: 127.0.0.1
""" % (app_port, app_site.replace("\\", "/"))
        ap_anchor = os.path.join(tmp, "app_anchor.yaml")
        ap_flow = os.path.join(tmp, "app_flow.yaml")
        with open(ap_anchor, "w", encoding="utf-8") as f:
            f.write(anchor_yaml)
        with open(ap_flow, "w", encoding="utf-8") as f:
            f.write(flow_yaml)

        rc_a, _ = run_gear(["tool", "register", "--registry", app_reg,
                            os.path.join(ROOT, "plugin", "static_site_demo.yaml")], env=env)
        rc_b, _ = run_gear(["tool", "register", "--registry", app_reg, ap_anchor], env=env)
        rc_c, _ = run_gear(["flow", "register", "--registry", app_reg, ap_flow], env=env)
        app_reg_ok = (rc_a == 0 and rc_b == 0 and rc_c == 0)

        rc, out = run_gear(["app", "start", "app_demo.up", "--registry", app_reg,
                            "--apps-dir", app_apps, "--port", str(app_port),
                            "--wait", "12", "--restart", "never"], env=app_env)
        _t1 = (out.strip().splitlines() or [""])[0].split()
        app_state = _t1[0] if _t1 else ""
        app_id = _t1[1] if len(_t1) > 1 else ""
        if app_id:
            started_apps.append(app_id)
        # 关键断言：这次 fetch 发生在 `app start` 子进程**已经退出之后**——
        # 端口仍在服务即证明应用是自持的独立进程（没有被 start 命令带走）。
        app_served = False
        if app_id:
            try:
                with urllib.request.urlopen("http://127.0.0.1:%d/index.html" % app_port, timeout=5) as r:
                    app_served = (r.read().decode("utf-8", "ignore") == "<h1>hello gear app</h1>")
            except Exception:
                pass
        check("P4 app start 派生后台进程→立即返回→父命令退出后端口仍服务（应用真自持）",
              app_reg_ok and rc == 0 and app_state == "running" and app_served,
              "register=%s rc=%d state=%s app_id=%s served=%s" % (app_reg_ok, rc, app_state, app_id, app_served))

        rc2, out2 = run_gear(["app", "start", "app_demo.up", "--registry", app_reg,
                              "--apps-dir", app_apps, "--port", str(app_port),
                              "--wait", "3", "--restart", "never"], env=env)
        _t2 = (out2.strip().splitlines() or [""])[0].split()
        check("P4 同 flow 二次 start 幂等（already_running，同一 app_id，不重复派生）",
              bool(_t2) and _t2[0] == "already_running" and len(_t2) > 1 and _t2[1] == app_id,
              "state=%s app_id=%s（期望 already_running / %s）"
              % (_t2[0] if _t2 else "?", _t2[1] if len(_t2) > 1 else "?", app_id))

        rc_st, out_st = run_gear(["app", "status", app_id, "--registry", app_reg,
                                  "--apps-dir", app_apps], env=env)
        rc_ls, out_ls = run_gear(["app", "list", "--registry", app_reg,
                                  "--apps-dir", app_apps], env=env)
        st_ok = (rc_st == 0 and "running" in out_st and "listening=true" in out_st)
        ls_ok = (rc_ls == 0 and app_id in out_ls)
        check("P4 app status/list 如实反映（pid + 端口双判据）", st_ok and ls_ok,
              "status_ok=%s list_ok=%s status=%r" % (st_ok, ls_ok, out_st.strip().splitlines()[:1]))

        # stop 的语义：app **自己**持有 ext.serve 侧车与站点端口（见本段的 extensions 副本），
        # 所以 stop 必须把进程树（app run + 它拉起的 serve-sidecar）一起收干净 ——
        # stopped + 端口**真释放**，而不是留在那被别人的进程占着。
        # 「收不干净绝不谎报成功」的另一半（端口被外部持有 → ORPHAN_APP + 非 0 退出）由单测
        # TestStopReportsOrphanWhenPortHeld 覆盖，两者合起来才是完整语义。
        rc_sp, out_sp = run_gear(["app", "stop", app_id, "--registry", app_reg,
                                  "--apps-dir", app_apps], env=env)
        released = False
        for _ in range(15):
            try:
                with urllib.request.urlopen("http://127.0.0.1:%d/index.html" % app_port, timeout=2) as r:
                    r.read()
            except Exception:
                released = True
                break
            time.sleep(0.3)
        check("P4 app stop 终止应用并真释放站点端口（stopped + listening=false）",
              rc_sp == 0 and released and "stopped" in out_sp and "listening=false" in out_sp,
              "rc=%d released=%s out=%r" % (rc_sp, released, out_sp.strip().splitlines()[:1]))

        # ---- 9) 红线：methods/schemas 不一致的 ext 必须启动 fail-closed ----
        bad = os.path.join(tmp, "bad.yaml")
        with open(bad, "w", encoding="utf-8") as f:
            f.write('ext.broken:\n  cmd: ["echo"]\n  methods: [send]\n  # 故意不写 schemas\n')
        env_bad = dict(os.environ)
        env_bad["GEAR_EXT_CONFIG"] = bad
        rc, out = run_gear(["ai", "capabilities"], env=env_bad)
        check("红线：缺 schemas 启动 fail-closed", rc != 0, "rc=%d" % rc)

        # ---- 10) 统一注册表应含全部 6 个 ext ----
        rc, out = run_gear(["ai", "capabilities"], env=env)
        import re
        caps = set(re.findall(r"\[ext\]\s+(ext\.\w+)", out))
        six = {"ext.email", "ext.webhook", "ext.wecom", "ext.rabbitmq", "ext.kafka", "ext.sms", "ext.serve"}
        check("统一注册表含 6 个 ext", six.issubset(caps), "发现: %s" % sorted(caps))
    finally:
        # 后台应用先收：它派生的进程**不在 procs 里**（是 app run 自持的独立进程树），
        # 漏收会留下占着端口的残留进程，下一轮 verify 的端口前置检查就会直接 FATAL。
        for _aid in started_apps:
            try:
                run_gear(["app", "stop", _aid, "--registry", app_reg, "--apps-dir", app_apps],
                         env=env, cwd=ROOT)
            except Exception:
                pass
        for p in procs:
            try:
                p.terminate()
            except Exception:
                pass
        srv.shutdown()
        api_srv.shutdown()

    passed = sum(1 for _, ok, _ in RESULTS if ok)
    print("\n==== 汇总 %d/%d 通过 ====" % (passed, len(RESULTS)))
    sys.exit(0 if passed == len(RESULTS) else 1)


if __name__ == "__main__":
    main()
