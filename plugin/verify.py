#!/usr/bin/env python3
"""plugin/verify.py —— 可扩展原子（P1 通知 + P2 消息/短信）端到端验证。

不依赖任何外部服务即可证明协议正确 + 真实跑通 Gear 调用链路。

本期（2026-09-18）webhook / rabbitmq / kafka 已升级为「常驻能力（service ext）」：
  - 侧车长驻，能力调用走本地 HTTP（LocalHTTPTransport，复用长连接）；
  - webhook.listen / rabbitmq.consume / kafka.consume 经通用订阅原语 ctx.subscribe
    回推事件，本脚本直接起侧车验证「监听一个 URL / 消费队列」的路径。
email / wecom / sms 仍是一次型（once）原子，保持原 stdin/stdout 验证。

P1：
  1) ext.webhook ：起侧车，经 /send 真实 POST 接收（service 路径）
  1b) ext.webhook.listen：/subscribe 登记入站路由 + POST /in/{path} -> 侧车回推（监听一个 URL）
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
"""
import os
import sys
import json
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

    tmp = tempfile.mkdtemp(prefix="ext-")
    reg = os.path.join(tmp, "ext.db")
    email_log = os.path.join(tmp, "email.out.log")

    env = dict(os.environ)
    env["GEAR_EXT_CONFIG"] = EXT

    # 起三个常驻侧车（webhook 同时监听 coord:8089 与 inbound:8088）
    wh = start_sidecar(os.path.join(ROOT, "build", "webhook-sidecar.exe"), WEBHOOK_COORD)
    rmq = start_sidecar(os.path.join(ROOT, "build", "rabbitmq-sidecar.exe"), RABBITMQ_COORD)
    kfk = start_sidecar(os.path.join(ROOT, "build", "kafka-sidecar.exe"), KAFKA_COORD)
    sv = start_sidecar(os.path.join(ROOT, "build", "serve-sidecar.exe"), SERVE_COORD)

    try:
        # ---- 0) 红线：extensions.yaml 声明的方法，必须都被侧车真正实现（/caps）----
        # 堵住「yaml 能声明一个侧车根本没实现的方法」的缺口。期望值与 plugin/extensions.yaml
        # 各 ext 的 methods 保持一致（subset 断言：声明 ⊆ 实现）。
        caps_expect = {
            "ext.webhook": (WEBHOOK_COORD, {"send", "listen", "unsubscribe"}),
            "ext.rabbitmq": (RABBITMQ_COORD, {"publish", "declare", "consume", "unsubscribe"}),
            "ext.kafka": (KAFKA_COORD, {"produce", "consume", "unsubscribe"}),
            "ext.serve": (SERVE_COORD, {"start", "stop"}),
        }
        for _name, (_coord, _want) in caps_expect.items():
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

        # ---- 1b) ext.webhook.listen：/subscribe + POST /in/{path} -> 侧车回推 ----
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
        check("P1 ext.webhook.listen 监听入站 URL 回推事件", subscribed and pushed,
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
            time.sleep(1.2)
        import socket
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
        for p in (wh, rmq, kfk, sv):
            try:
                p.terminate()
            except Exception:
                pass
        srv.shutdown()

    passed = sum(1 for _, ok, _ in RESULTS if ok)
    print("\n==== 汇总 %d/%d 通过 ====" % (passed, len(RESULTS)))
    sys.exit(0 if passed == len(RESULTS) else 1)


if __name__ == "__main__":
    main()
