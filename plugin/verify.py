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
import base64
import hashlib
import json
import socket
import subprocess
import tempfile
import threading
import time
import urllib.request
import urllib.error
import urllib.parse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
GEAR = os.path.join(ROOT, "build", "gear.exe")
EXT = os.path.join(ROOT, "plugin", "extensions.yaml")

# 唯一真相源锁死：gear 找扩展配置有 7 个候选路径，`<exeDir>/extensions.yaml` 排第二。
# 而 `build/extensions.yaml` 是 2026-09-22 留下的**旧副本**（只有 7 段，没有 filestore/cron/oidc），
# 它一旦存在就把 plugin/ 那份整个遮住——`tool register` 会报「未知的 capability ext.oidc」，
# 看着像代码没支持，其实支持得好好的，只是被一份过期文件挡住了。
# 故这里显式钉住 GEAR_EXT_CONFIG，让**所有**子进程（tool register / flow / app）读同一份。
os.environ["GEAR_EXT_CONFIG"] = EXT
NOTIFY_SEED = os.path.join(ROOT, "plugin", "notify_demo.yaml")
MSG_SEED = os.path.join(ROOT, "plugin", "messaging_demo.yaml")
CONSUME_SEED = os.path.join(ROOT, "plugin", "consume_demo.yaml")

# service ext 侧车端口（与 extensions.yaml 的 listen 一致）
WEBHOOK_COORD = "127.0.0.1:8089"
# openpyxl 装在 managed Python 的 venv 里：用它做 Excel 交叉验证（独立实现读回，
# 不是「自己写的自己读」）。找不到就退化成只做结构校验（openpyxl 是验证增强，不是门槛）。
VENV_PY = ""
for _cand in (
    r"C:\Users\920\.workbuddy\binaries\python\envs\default\Scripts\python.exe",
    os.path.join(ROOT, "..", "..", "..", ".workbuddy", "binaries", "python",
                 "envs", "default", "Scripts", "python.exe"),
):
    if os.path.exists(_cand):
        VENV_PY = _cand
        break
WEBHOOK_INBOUND = "127.0.0.1:8088"
RABBITMQ_COORD = "127.0.0.1:5674"
KAFKA_COORD = "127.0.0.1:9094"
SERVE_COORD = "127.0.0.1:8095"
FILESTORE_COORD = "127.0.0.1:8096"
CRON_COORD = "127.0.0.1:8097"
CRON2_COORD = "127.0.0.1:8197"   # cron 硬绑定用例专用实例（独立 GEAR_HOME + 独立端口）
OIDC_COORD = "127.0.0.1:8098"
OIDC_INBOUND = "127.0.0.1:8099"  # ext.oidc 入站回调端口（/in/oidc/{label}）
XLSX_COORD = "127.0.0.1:8100"    # ext.excel 侧车 coord 端口（与 extensions.yaml 的 listen 一致）
PDF_COORD = "127.0.0.1:8101"     # ext.pdf 侧车 coord 端口
OSS_COORD = "127.0.0.1:8102"     # ext.oss 侧车 coord 端口（8095–8101 已被占，8102 是首个空位）
REDIS_COORD = "127.0.0.1:8103"    # ext.redis 侧车 coord 端口（紧跟 oss 之后）

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


# ─────────────────────────────────────────────────────────────────────────────
# 假 IdP（ext.oidc 端到端用）
#
# 为什么要自己签：AC-E1-5 要证明「ext.oidc 拿回来的 id_token 能被 crypto.jwt_verify 验签通过」，
# 而这个假 IdP 就是 IdP 那一方——它不真签，这条链路就是空的（拿一坨 base64 当令牌，验签必败）。
# verify.py 只用标准库（加了 cryptography 就等于给端到端加一个安装前置），
# 所以 RS256 用一次 pow 现场算，JWKS 也按 IdP 该有的形状吐。
#
# 密钥是一次性生成的**测试夹具**（_OIDC_TEST_KEY），不是任何真实环境的凭据。
_OIDC_TEST_KEY = {
    "N": "26815645611045909518964208869906581952478834327236714362389388138692146230877458392676347038442979218826139600569366301058854993594250058240966282511984764532826983083879040520815129680554465299613235293639117773588266229436470575291906033107536864877699786319402934971506870816725383361816271164970743934324202971250421541355269490713605559527025112497325121300782738115728901906830536823476772709383230332601830070336167855679385643239075597106315663731375542158063221992739696736537165839232453082734689780040041026575117554216457208537276040970082603276643033372028120450500326837282954445065054339269964086110369",
    "E": "65537",
    "D": "6281752281056232752336817624780517866631541022764265334827396453900410073844487540985452765730745051757454693039675238979600398082849749288866363310271237583200730240860170126734734248145512130740073467592269338221368956275438523386315934102620805316613301943461457789493862004879326915517712178772500209831199128932986948063951058142566162837503208076905849791594787220591241973841180756607127355938133021602539353678644787585057399496597017050497835481129772510059796876412240376684758766869626525144398342997278103994184132359881579980525596527537261908441647998156104148898800920809727465509812218670906190219043",
}
_OIDC_K = 256  # 2048 位模数的字节长度（RSASSA-PKCS1-v1_5 要求 EM 与模数等长）
# SHA-256 的 DigestInfo 前缀（RFC 8017 §9.2），PKCS#1 v1.5 签名的载荷里带着它
_DIGESTINFO_SHA256 = bytes.fromhex("3031300d060960864801650304020105000420")


def _b64u(raw):
    return base64.urlsafe_b64encode(raw).decode("ascii").rstrip("=")


def _rs256_sign(signing_input):
    """rs256 签名：EM = 0x00||0x01||0xff*PS||0x00||DigestInfo，再 s = m^d mod n。"""
    n, d = int(_OIDC_TEST_KEY["N"]), int(_OIDC_TEST_KEY["D"])
    digest = hashlib.sha256(signing_input.encode("ascii")).digest()
    t = _DIGESTINFO_SHA256 + digest
    em = b"\x00\x01" + b"\xff" * (_OIDC_K - 3 - len(t)) + b"\x00" + t
    s = pow(int.from_bytes(em, "big"), d, n)
    return _b64u(s.to_bytes(_OIDC_K, "big"))


class IdPState(object):
    """假 IdP 的可调开关（模块级单例：BaseHTTPRequestHandler 没法自由实例化）。"""

    def __init__(self, issuer):
        self.issuer = issuer
        self.kid = "idp-test-key"
        self.jwks_hits = 0
        self.token_form = {}
        self.doc_issuer_bad = False    # ⇒ discovery 文档里的 issuer 写成别人（造 issuer 混淆）
        self.token_endpoint_bad = False  # ⇒ token_endpoint 换成别的 host
        self.require_verifier = None   # 非空 ⇒ 校验 code_verifier，不匹配就 400
        self.jwks_fail = False         # ⇒ JWKS 端点返回 500（造「刷新失败」）
        self.aud_bad = False           # ⇒ 令牌里的 aud 写死成别人（造「受众不符」）
        self.jwks_bogus = False        # ⇒ JWKS 换成一把毫不相关的钥匙（老签名必须验不过）


IDP = None


class FakeIdP(BaseHTTPRequestHandler):
    """按 OIDC 的形状说话的授权服务器：discovery / jwks / token 三端点。"""

    def do_GET(self):
        self._serve(self.path)

    def do_POST(self):
        n = int(self.headers.get("Content-Length", 0))
        raw = self.rfile.read(n).decode("utf-8", "ignore")
        self._serve(self.path, raw)

    def _serve(self, path, raw=None):
        f = IDP
        if path.startswith("/.well-known/openid-configuration"):
            if f.doc_issuer_bad:
                # 元数据被换过：文档里的 issuer 是别人 ⇒ 侧车应判 issuer 混淆并拒（AC-E1-2）
                issuer = "https://evil.example/issuer"
            else:
                issuer = f.issuer
            token_endpoint = f.issuer + "/token"
            if f.token_endpoint_bad:
                token_endpoint = "http://127.0.0.1:1/token"  # 换主机 ⇒ 应被 issuer 混淆检查挡下
            body = {"issuer": issuer, "authorization_endpoint": f.issuer + "/authorize",
                    "token_endpoint": token_endpoint, "jwks_uri": f.issuer + "/jwks",
                    "response_types_supported": ["code"],
                    "id_token_signing_alg_values_supported": ["RS256"]}
            code = 200
        elif path.startswith("/jwks"):
            f.jwks_hits += 1  # 计数在开关之前：这台机器被问了几次，是断言的判据
            if f.jwks_fail:
                code, body = 500, {"error": "server_error", "error_description": "JWKS 端点故意挂掉"}
            elif f.jwks_bogus:
                # 同一把 kid、换一个 modulus：等于「IdP 换了钥匙却没让老签名失效」
                body = {"keys": [{"kty": "RSA", "kid": f.kid, "use": "sig",
                                  "alg": "RS256", "n": _b64u(b"\xab" * _OIDC_K),
                                  "e": _b64u((65537).to_bytes(3, "big"))}]}
                code = 200
            else:
                n_e = _b64u((int(_OIDC_TEST_KEY["E"])).to_bytes(3, "big"))
                n_raw = (int(_OIDC_TEST_KEY["N"]).to_bytes(_OIDC_K, "big"))
                body = {"keys": [{"kty": "RSA", "kid": f.kid, "use": "sig",
                                  "alg": "RS256", "n": _b64u(n_raw), "e": n_e}]}
                code = 200
        elif path.startswith("/token"):
            form = urllib.parse.parse_qs(raw or "")
            f.token_form = form
            def _claim(name):
                v = form.get(name)
                return v[0] if v else ""
            if f.require_verifier and _claim("code_verifier") != f.require_verifier:
                code, body = 400, {"error": "invalid_grant", "error_description": "code_verifier 不匹配"}
            else:
                header = _b64u(json.dumps({"alg": "RS256", "typ": "JWT", "kid": f.kid},
                                          separators=(",", ":")).encode())
                now = int(time.time())
                aud = "someone-else" if f.aud_bad else _claim("client_id")
                claims = _b64u(json.dumps(
                    {"iss": f.issuer, "sub": "alice", "aud": aud,
                     "exp": now + 3600, "iat": now, "nonce": "n-1"},
                    separators=(",", ":")).encode())
                body = {"access_token": "AT-" + _claim("code"),
                        "id_token": header + "." + claims + "." + _rs256_sign(header + "." + claims),
                        "token_type": "Bearer", "expires_in": 3600, "scope": "openid profile"}
                code = 200
        else:
            code, body = 404, {"error": "not_found"}
        payload = json.dumps(body).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def log_message(self, *a):
        pass


def start_fake_idp():
    global IDP
    srv = ThreadingHTTPServer(("127.0.0.1", 0), FakeIdP)
    port = srv.server_address[1]
    st = IdPState("http://127.0.0.1:%d" % port)
    IDP = st  # 模块级单例：请求处理器是框架造的，只能靠全局变量把状态递给它
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    return srv, st


def parse_tool_out(out):
    """把 `gear tool run` 打印的 `key: value` 文本解析成 dict。

    样例 Tool 走的是 Lua 入口、返回表，所以命令行打印的是逐行 `key: value`，
    不是 JSON——拿 `out[out.rfind("{"):]` 去截 JSON 会得到空 dict，
    断言于是永远「subject 是 None」，看着像链路没通，其实通得很（第一版就栽在这）。
    """
    kv = {}
    for _line in out.splitlines():
        if ": " not in _line:
            continue
        _k, _val = _line.split(": ", 1)
        kv[_k.strip()] = _val.strip().strip("'")
    return kv


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


def start_sidecar(binary, coord_addr, env=None):
    """起侧车子进程，并向 coord 注册一个 dummy 占用 + 后台心跳（防止 OnEmpty 自动退出），返回 proc。

    env 可覆盖继承的环境（用于给某个实例设 GEAR_HOME 做隔离，例如 cron 的硬绑定用例）。

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
        env=env or os.environ,
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


def parse_ext_listen(path):
    """从 extensions.yaml 解析 {ext 名: listen}（零依赖正则，与 parse_ext_methods 同一套路）。

    端口**没有**集中登记处：extensions.yaml 的 `listen` 是唯一真源，本文件顶部的
    *_COORD 常量是手工同步的第二份（而且只有 methods 是现读的）。新增 ext 时若抄了一个
    已被占用的端口，侧车会「退让退出」（ExitAddrInUse 退出码 2），于是 verify 打到的是
    残留的旧进程 —— 症状是假绿。这里机械校验端口互不冲突，把这类错误挡在起侧车之前。
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
                # 先剥行尾注释（listen 行常带 `# coord + RPC 端口`），否则匹配不到
                body = re.sub(r"\s+#.*$", "", line)
                m = re.match(r'^\s+listen\s*:\s*"?([^"\s]+)"?\s*$', body)
                if m:
                    out[cur] = m.group(1)
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
    # E4：Excel / PDF 零依赖写出侧车（手写 OOXML + 手写 TTF 子集，go.mod 不动）
    build_sidecar("./plugin/xlsx-sidecar", "plugin/xlsx-sidecar.exe")
    build_sidecar("./plugin/pdf-sidecar", "plugin/pdf-sidecar.exe")
    # E5 第三批：ext.oss（S3 兼容对象存储，手写 SigV4，go.mod 不动）
    build_sidecar("./plugin/oss-sidecar", "plugin/oss-sidecar.exe")
    # 同批：ext.redis（手写 RESP2，零第三方依赖，go.mod 不动）
    build_sidecar("./plugin/redis-sidecar", "plugin/redis-sidecar.exe")

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
                         ("kafka", KAFKA_COORD), ("serve", SERVE_COORD),
                         ("filestore", FILESTORE_COORD), ("cron", CRON_COORD),
                         ("oidc", OIDC_COORD), ("xlsx", XLSX_COORD),
                         ("pdf", PDF_COORD), ("oss", OSS_COORD),
                         ("redis", REDIS_COORD)):
        _host, _, _port = _addr.partition(":")
        _s = socket.socket()
        _s.settimeout(0.5)
        try:
            _s.connect((_host, int(_port)))
            print("[FATAL] %s 侧车已在 %s 上运行（残留进程）：本轮会打到旧二进制，结论不可信。" % (_name, _addr))
            print("        请先结束残留侧车再重跑，例如：")
            print("        taskkill /F /IM serve-sidecar.exe /IM webhook-sidecar.exe"
                  " /IM rabbitmq-sidecar.exe /IM kafka-sidecar.exe /IM filestore-sidecar.exe"
                  " /IM cron-sidecar.exe /IM oidc-sidecar.exe")
            sys.exit(2)
        except SystemExit:
            raise
        except Exception:
            pass
        finally:
            _s.close()

    # 起三个常驻侧车（webhook 同时监听 coord:8089 与 inbound:8088）
    wh = start_sidecar(os.path.join(ROOT, "build", "plugin", "webhook-sidecar.exe"), WEBHOOK_COORD)
    rmq = start_sidecar(os.path.join(ROOT, "build", "plugin", "rabbitmq-sidecar.exe"), RABBITMQ_COORD)
    kfk = start_sidecar(os.path.join(ROOT, "build", "plugin", "kafka-sidecar.exe"), KAFKA_COORD)
    sv = start_sidecar(os.path.join(ROOT, "build", "plugin", "serve-sidecar.exe"), SERVE_COORD)
    fs = start_sidecar(os.path.join(ROOT, "build", "plugin", "filestore-sidecar.exe"), FILESTORE_COORD)
    cr = start_sidecar(os.path.join(ROOT, "build", "plugin", "cron-sidecar.exe"), CRON_COORD)
    # oidc 必须跟着这批一起起：红线那一段（下方 0）就要打它的 /caps，晚一步起就来不及。
    _oidc_env = dict(os.environ)
    _oidc_env["INBOUND_ADDR"] = OIDC_INBOUND
    od = start_sidecar(os.path.join(ROOT, "build", "plugin", "oidc-sidecar.exe"),
                       OIDC_COORD, env=_oidc_env)

    # E4：Excel / PDF 侧车（各自的 COORD_ADDR 必须显式注入，否则会落到默认的 8100/8101
    # 之外的端口，而下面打的是我们约定的 XLSX_COORD / PDF_COORD -> 连不上）。
    _xls_env = dict(os.environ)
    _xls_env["COORD_ADDR"] = XLSX_COORD
    xls = start_sidecar(os.path.join(ROOT, "build", "plugin", "xlsx-sidecar.exe"),
                        XLSX_COORD, env=_xls_env)
    _pdf_env = dict(os.environ)
    _pdf_env["COORD_ADDR"] = PDF_COORD
    pdf = start_sidecar(os.path.join(ROOT, "build", "plugin", "pdf-sidecar.exe"),
                        PDF_COORD, env=_pdf_env)
    _oss_env = dict(os.environ)
    _oss_env["COORD_ADDR"] = OSS_COORD
    oss = start_sidecar(os.path.join(ROOT, "build", "plugin", "oss-sidecar.exe"),
                        OSS_COORD, env=_oss_env)
    _redis_env = dict(os.environ)
    _redis_env["COORD_ADDR"] = REDIS_COORD
    rds = start_sidecar(os.path.join(ROOT, "build", "plugin", "redis-sidecar.exe"),
                        REDIS_COORD, env=_redis_env)

    # 所有起过的侧车都要登记：8b 可能**再拉起一个** serve 实例（占用心跳偶发超时兜底），
    # 若只 terminate 最后那个变量，前一个就变成残留进程——它会一直占着 coord 端口，
    # 让下一轮 verify 起的新侧车「退让退出」，于是跑的还是**旧二进制**（结果假绿/假红）。
    procs = [wh, rmq, kfk, sv, fs, cr, od, xls, pdf, oss, rds]

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
            "ext.filestore": FILESTORE_COORD,
            "ext.cron": CRON_COORD,
            "ext.oidc": OIDC_COORD,
            # excel / pdf 之前漏在这里（2026-09-30 发现），oss 一并接上
            "ext.excel": XLSX_COORD,
            "ext.pdf": PDF_COORD,
            "ext.oss": OSS_COORD,
            "ext.redis": REDIS_COORD,
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

        # ---- 0b) 端口：listen 必须从权威声明现读，互不冲突，且两份配置一致 ----
        _la = parse_ext_listen(EXT)
        _lb = parse_ext_listen(os.path.join(ROOT, "extensions.example.yaml"))
        _by_port = {}
        for _n, _v in sorted(_la.items()):
            _by_port.setdefault(_v, []).append(_n)
        _dups = dict((k, v) for k, v in _by_port.items() if len(v) > 1)
        check("红线：ext 的 listen 端口互不冲突", bool(_la) and not _dups,
              "重复=%s（共 %d 个 ext）" % (_dups, len(_la)))
        _diff = dict((k, (_la[k], _lb.get(k))) for k in _la if k in _lb and _lb[k] != _la[k])
        check("红线：模板与真身的 listen 一致", bool(_lb) and not _diff, "不一致=%s" % _diff)

        # ---- 1c) ext.filestore：put/get 真实往返 + 越界 fail-closed（PRD §4.3 / AC-E2-1·2·3）----
        import base64 as _b64
        _fs_root = tempfile.mkdtemp(prefix="gear-filestore-")
        _payload = b"\x00\x01\x02 \xe4\xb8\xad\xe6\x96\x87 \xff\xfe"
        _fs_key = "contracts/2026/a.pdf"
        _put = http_post_json("http://%s/put" % FILESTORE_COORD,
                              {"args": {"root": _fs_root, "key": _fs_key,
                                        "data_base64": _b64.b64encode(_payload).decode(),
                                        "content_type": "application/pdf"}})
        _put_ok = "result" in _put and _put["result"].get("size") == len(_payload)
        _get = http_post_json("http://%s/get" % FILESTORE_COORD,
                              {"args": {"root": _fs_root, "key": _fs_key}})
        _gr = _get.get("result", {})
        _back = _b64.b64decode(_gr.get("data_base64", ""))
        _round_ok = _put_ok and _back == _payload and _gr.get("content_type") == "application/pdf"
        check("P1b ext.filestore put/get 真实往返（二进制 + 多级 key + content_type）", _round_ok,
              "size=%s back_bytes=%d ct=%s" % (_put.get("result", {}).get("size"), len(_back),
                                               _gr.get("content_type")))

        # 路径穿越：越界 key 必须被拒，且磁盘上不留任何额外文件（fail-closed，不是「写了个空」）
        _trav = http_post_json("http://%s/get" % FILESTORE_COORD,
                               {"args": {"root": _fs_root, "key": "../../escape.txt"}})
        _files = []
        for _dp, _dn, _fn in os.walk(_fs_root):
            for _f in _fn:
                _files.append(os.path.relpath(os.path.join(_dp, _f), _fs_root).replace(os.sep, "/"))
        _expect_files = sorted([".filestore-index.json", _fs_key])
        check("P1b ext.filestore 路径穿越 fail-closed 且无残留", "error" in _trav
              and sorted(_files) == _expect_files,
              "error=%s files=%s" % (_trav.get("error"), sorted(_files)))

        # 大小上限：超限拒写，磁盘不留半截文件
        _over = http_post_json("http://%s/put" % FILESTORE_COORD,
                               {"args": {"root": _fs_root, "key": "big.bin",
                                         "data_base64": _b64.b64encode(b"0123456789").decode(),
                                         "max_bytes": 4}})
        _over_left = []
        for _dp, _dn, _fn in os.walk(_fs_root):
            for _f in _fn:
                if _f == "big.bin":
                    _over_left.append(os.path.join(_dp, _f))
        check("P1b ext.filestore 超限 fail-closed（拒写且不落残file）",
              "error" in _over and "大小上限" in _over.get("error", "") and not _over_left,
              "error=%s left=%s" % (_over.get("error"), _over_left))

        # ---- 1c-2) ext.cron：注册期 fail-closed + list 不泄露载荷 ----
        _bad_expr = http_post_json("http://%s/add" % CRON_COORD,
                                   {"args": {"name": "x", "expr": "每分钟"}})
        check("P1c ext.cron 非法表达式在注册期被拒（fail-closed 前移）", "error" in _bad_expr,
              "error=%s" % _bad_expr.get("error"))
        _no_cb = http_post_json("http://%s/add" % CRON_COORD,
                                {"args": {"name": "y", "expr": "* * * * *"}})
        check("P1c ext.cron 缺 callback_url 被拒（不猜往哪推）", "error" in _no_cb,
              "error=%s" % _no_cb.get("error"))
        _leak = http_post_json("http://%s/add" % CRON_COORD,
                               {"args": {"name": "quiet", "expr": "0 3 * * *",
                                         "callback_url": sink_url,
                                         "payload": {"password": "p@ss"}}})
        _lst = http_post_json("http://%s/list" % CRON_COORD, {"args": {}})
        _lst_txt = json.dumps(_lst, ensure_ascii=False)
        check("P1c ext.cron list 不摊开 payload / callback_url",
              "result" in _leak and "p@ss" not in _lst_txt and "password" not in _lst_txt,
              "list=%s" % (_lst_txt[:200],))
        _cancel_unknown = http_post_json("http://%s/cancel" % CRON_COORD,
                                         {"args": {"name": "从不存在的任务"}})
        check("P1c ext.cron 取消不存在的任务 fail-visible", "error" in _cancel_unknown,
              "result=%s" % _cancel_unknown.get("result"))

        # ---- 1c-3) ext.cron 硬绑定：AC-E3-5 —— 「kill 后到点无任何回推」+「重启不接管」----
        # 这条只能靠真进程证明：把「到点」安排在一个**被 kill 掉**的侧车身上，等它真的过去，
        # 再起一个新进程读同一张表。任何用内存态伪造的证明都不算。
        import datetime as _dt
        _cron_bin = os.path.join(ROOT, "build", "plugin", "cron-sidecar.exe")
        _cron_home = tempfile.mkdtemp(prefix="gear-cronhome-")
        _cron_env = dict(os.environ)
        _cron_env["GEAR_HOME"] = _cron_home     # 与前面那个 cron 实例彻底隔离（另一份表）
        _cron_env["COORD_ADDR"] = CRON2_COORD
        # 一次性任务的 at 只能到分钟（Truncate(minute)），所以「到点」必然落在下一个分钟边界。
        _fire_at = _dt.datetime.now().replace(second=0, microsecond=0) + _dt.timedelta(minutes=1)
        _before = len(Sink.received)
        cr2 = start_sidecar(_cron_bin, CRON2_COORD, env=_cron_env)
        procs.append(cr2)
        _armed = http_post_json("http://%s/at" % CRON2_COORD,
                                {"args": {"name": "hard-bind-probe",
                                          "at": _fire_at.strftime("%Y-%m-%d %H:%M"),
                                          "callback_url": sink_url,
                                          "payload": {"probe": "hb"}}})
        check("P1c ext.cron at 注册成功（硬绑定用例的前置）", "result" in _armed,
              "result=%s" % _armed.get("result"))
        # 在触发时刻之前杀掉进程：让「到点」发生在一个已经死的调度器身上。
        try:
            cr2.terminate()
        except Exception:
            pass
        _wait = (_fire_at + _dt.timedelta(seconds=10) - _dt.datetime.now()).total_seconds()
        if _wait > 0:
            time.sleep(_wait)
        check("P1c ext.cron 硬绑定：侧车进程退出后，到点无任何回推",
              len(Sink.received) == _before,
              "新增回推 %d 条（触发时刻 %s）" % (len(Sink.received) - _before, _fire_at))
        # 重启：新进程读同一张表、进程活着（有占用心跳），但 catch_up 未开 → 不接管。
        cr3 = start_sidecar(_cron_bin, CRON2_COORD, env=_cron_env)
        procs.append(cr3)
        time.sleep(2.0)
        check("P1c ext.cron 重启不接管错过的触发（表在、进程在、就是不打）",
              len(Sink.received) == _before,
              "新增回推 %d 条" % (len(Sink.received) - _before))

        # ---- 1d) ext.oidc：AC-E1-1~E1-5 ----
        _idp_srv, idp = start_fake_idp()
        # 侧车已在上面那批里起好（红线段要用它的 /caps），这里只登记回收
        procs.append(od)
        _rd = lambda m, a: http_post_json("http://%s/%s" % (OIDC_COORD, m), {"args": a})

        # AC-E1-1 前半：discovery 正常返回元数据，且 issuer 与入参一致
        _disc = _rd("discovery", {"issuer": idp.issuer})
        _dres = _disc.get("result", {})
        check("P1d ext.oidc discovery 返回 issuer 元数据",
              "result" in _disc and _dres.get("issuer") == idp.issuer
              and _dres.get("token_endpoint") == idp.issuer + "/token"
              and _dres.get("jwks_uri") == idp.issuer + "/jwks",
              "issuer=%s token=%s" % (_dres.get("issuer"), _dres.get("token_endpoint")))

        # AC-E1-1 后半：issuer 混淆 —— 文档里的 issuer 被换成别人 ⇒ 必须拒
        idp.doc_issuer_bad = True
        _conf = _rd("discovery", {"issuer": idp.issuer})
        check("P1d ext.oidc 拒绝 issuer 混淆（文档 issuer ≠ 入参）", "error" in _conf,
              "error=%s" % _conf.get("error"))
        # token_endpoint 换主机 ⇒ 同样拒（这一支挡的是「把凭据送给别人」）
        idp.doc_issuer_bad = False
        idp.token_endpoint_bad = True
        _conf2 = _rd("discovery", {"issuer": idp.issuer})
        check("P1d ext.oidc 拒绝 token_endpoint 换主机（凭据不能送给别人）", "error" in _conf2,
              "error=%s" % _conf2.get("error"))
        idp.token_endpoint_bad = False

        # AC-E1-2：PKCE。先给对的东西，再给错的，看后者是否「结构化错误 + 不吐令牌」
        _args_ok = {"issuer": idp.issuer, "code": "the-code", "client_id": "app",
                    "client_secret": "s3cret", "redirect_uri": idp.issuer + "/cb"}
        _x = _rd("code_exchange", _args_ok)
        _xres = _x.get("result", {})
        check("P1d ext.oidc code_exchange 拿到令牌",
              _xres.get("access_token") == "AT-the-code" and bool(_xres.get("id_token")),
              "token_type=%s" % _xres.get("token_type"))
        # 表单必须带上 redirect_uri / client_secret，且没给 code_verifier 就不带
        _xf = idp.token_form
        check("P1d ext.oidc 令牌请求表单正确（grant_type/redirect_uri/client_secret）",
              _xf.get("grant_type") == ["authorization_code"]
              and _xf.get("redirect_uri") == [idp.issuer + "/cb"]
              and _xf.get("client_secret") == ["s3cret"]
              and not _xf.get("code_verifier"),
              "form keys=%s" % sorted(_xf.keys()))
        idp.require_verifier = "the-right-verifier"
        _pkce_bad = _rd("code_exchange", dict(_args_ok, code_verifier="wrong-verifier"))
        _pkce_txt = json.dumps(_pkce_bad, ensure_ascii=False)
        check("P1d ext.oidc 授权服务器拒收 → 结构化错误、不吐令牌",
              "error" in _pkce_bad and "code_verifier 不匹配" in _pkce_txt
              and "access_token" not in _pkce_txt and "id_token" not in _pkce_txt,
              "error=%s" % _pkce_bad.get("error"))
        idp.require_verifier = None
        # 必填不猜：缺 redirect_uri 直接拒
        _miss = _rd("code_exchange", {"issuer": idp.issuer, "code": "c", "client_id": "app"})
        check("P1d ext.oidc redirect_uri 缺失即拒（不猜回调往哪）", "error" in _miss,
              "error=%s" % _miss.get("error"))

        # AC-E1-3：JWKS 长驻缓存 —— 第二次调用**一次 HTTP 都不发**
        _j1 = _rd("jwks", {"issuer": idp.issuer})
        _hits1 = idp.jwks_hits
        time.sleep(0.3)
        _j2 = _rd("jwks", {"issuer": idp.issuer})
        check("P1d ext.oidc JWKS 长驻缓存（二次调用不发 HTTP）",
              "result" in _j1 and "result" in _j2 and idp.jwks_hits == _hits1
              and _j2.get("result", {}).get("stale") is False,
              "jwks 拉取次数 %d（首 %d 次，二次后仍 %d）" % (_hits1, _hits1, idp.jwks_hits))

        # 未知 kid ⇒ **强制刷新一次**：IdP 换钥匙，而缓存里还是老那把
        _hits_a = idp.jwks_hits
        idp.kid = "idp-rotated-key"
        _j3 = _rd("jwks", {"issuer": idp.issuer, "kid": "idp-rotated-key"})
        _kid3 = [k.get("kid") for k in _j3.get("result", {}).get("keys", [])]
        check("P1d ext.oidc 未知 kid ⇒ 强制刷新一次（拿回新钥匙，stale=false）",
              idp.jwks_hits == _hits_a + 1 and _kid3 == ["idp-rotated-key"]
              and _j3.get("result", {}).get("stale") is False,
              "hits %d→%d keys=%s stale=%s" % (_hits_a, idp.jwks_hits, _kid3,
                                               _j3.get("result", {}).get("stale")))

        # 刷新**失败** ⇒ 退回旧缓存 + stale=true（fail-soft 但不静默）
        # 判据是 stale 位，不是「又发了一次 HTTP」：刷新成功本来也会发一次，
        # 用次数区分不了「刷新成功了」和「刷新失败后又试了一次」。
        # kid 要挑一个**缓存里也没有**的（idp-rotated-key 现在已在缓存里，用它只会命中）。
        idp.jwks_fail = True
        _j4 = _rd("jwks", {"issuer": idp.issuer, "kid": "ghost-kid"})
        check("P1d ext.oidc JWKS 刷新失败时退回旧缓存并置 stale（fail-soft 但不静默）",
              "result" in _j4 and _j4.get("result", {}).get("stale") is True
              and [k.get("kid") for k in _j4["result"].get("keys", [])] == ["idp-rotated-key"],
              "stale=%s keys=%s" % (_j4.get("result", {}).get("stale"),
                                    [k.get("kid") for k in _j4.get("result", {}).get("keys", [])]))
        idp.jwks_fail = False

        # 换回原 kid ⇒ 刷新成功、缓存随之更新 ⇒ 紧接着的同一请求走命中，不再发 HTTP
        idp.kid = "idp-test-key"
        _hits_b = idp.jwks_hits
        _j5 = _rd("jwks", {"issuer": idp.issuer, "kid": "idp-test-key"})
        _hits_c = idp.jwks_hits
        _j6 = _rd("jwks", {"issuer": idp.issuer, "kid": "idp-test-key"})
        check("P1d ext.oidc 刷新成功后缓存随即更新（同 kid 再取一次命中、不发 HTTP）",
              "result" in _j5 and idp.jwks_hits == _hits_b + 1
              and _j6.get("result", {}).get("stale") is False
              and [k.get("kid") for k in _j6["result"].get("keys", [])] == ["idp-test-key"]
              and idp.jwks_hits == _hits_c,
              "hits %d→%d→%d stale=%s" % (_hits_b, _hits_c, idp.jwks_hits,
                                          _j6.get("result", {}).get("stale")))

        # 入站回调：浏览器从 IdP 弹回 /in/oidc/{label}?code=&state=
        try:
            _cb = json.loads(urllib.request.urlopen(
                "http://%s/in/oidc/demo?code=FROM_IDP&state=STATE1" % OIDC_INBOUND, timeout=5).read())
        except Exception as _e:
            _cb = {"error": str(_e)}
        # 响应是包了一层的（{"result": {...}}），断言得往里取一层
        _cbr = _cb.get("result", _cb)
        check("P1d ext.oidc 入站回调原样带回 code/state（不建会话）",
              _cbr.get("code") == "FROM_IDP" and _cbr.get("state") == "STATE1"
              and "session" not in _cbr,
              "cb=%s" % _cb)

        # AC-E1-6 + 零内核改动：能力目录只多 ext.oidc，MCP 工具 +0
        # AC-E1-5：样例 Tool 把 discovery→code_exchange→jwks→crypto.jwt_verify 一气跑通
        _oidc_seed = os.path.join(ROOT, "plugin", "oidc_demo.yaml")
        with open(_oidc_seed, encoding="utf-8") as f:
            _seed_txt = f.read()
        _reg = tempfile.mkdtemp(prefix="gear-oidcreg-")
        _regdb = os.path.join(_reg, "reg.db")
        rc, out = run_gear(["tool", "register", "--registry", _regdb, "-"], stdin=_seed_txt)
        check("P1d gear tool register（oidc.login 样例）", rc == 0, "rc=%d %s" % (rc, out[:120]))
        rc, out = run_gear(["tool", "run", "oidc.login", "--registry", _regdb, "--input",
                            json.dumps({"issuer": idp.issuer, "code": "chain-code",
                                        "client_id": "app", "client_secret": "s3cret",
                                        "redirect_uri": idp.issuer + "/cb"})])
        _chain = parse_tool_out(out)  # 样例 Tool 打印 `key: value` 文本，不是 JSON
        check("P1d ext.oidc→crypto.jwt_verify 链路打通（AC-E1-5：id_token 真能验签）",
              rc == 0 and _chain.get("subject") == "alice" and _chain.get("alg") == "RS256",
              "rc=%d subject=%s alg=%s" % (rc, _chain.get("subject"), _chain.get("alg")))
        # ---- 反向验证：不造假故障的话，上面那条「通过」可能只是「压根没验」 ----
        # 这段复用的仍是上面同一套缓存状态，只是换 Tool 链路再走一遍（含 kid 交回）。
        # 故障必须造在**验签那一侧**（IdP 手里），不能只改调用参数：Tool 的 aud 取的就是
        # ctx.input.client_id，而假 IdP 又按 client_id 写 aud——改 client_id 会两边一起变，
        # 于是「换个 client_id 也通过」，这条检查等于没写（第一版就踩了这个空转）。
        _login = lambda cid: run_gear(
            ["tool", "run", "oidc.login", "--registry", _regdb, "--input",
             json.dumps({"issuer": idp.issuer, "code": "chain-code", "client_id": cid,
                         "redirect_uri": idp.issuer + "/cb"})])

        # 判据是「输出里没有 subject 行」，不是 rc：样例 Tool 用 pcall 把失败收进 error 字段，
        # 所以失败路径的 rc 同样是 0——用 rc 判会得出「全都通过了」这种最坏的结论。
        idp.aud_bad = True
        _rc2, _out2 = _login("app")
        _txt2 = _out2.lower()
        check("P1d ext.oidc→crypto.jwt_verify 真在验 audience（aud 不符即 fail-closed）",
              "subject" not in _out2 and ("aud" in _txt2 or "受众" in _out2),
              "rc=%d out=%s" % (_rc2, _out2[-200:]))
        idp.aud_bad = False

        # AC-E1-3 走 Tool 链路：IdP 换钥匙 ⇒ 令牌里的 kid 在缓存里没有 ⇒ 强制刷新一次
        idp.kid = "idp-rotated-key"
        _hits_a = idp.jwks_hits
        _rcA, _outA = _login("app")
        check("P1d 未知 kid ⇒ 强制刷新一次（走 Tool 链路也能看到）",
              _rcA == 0 and "alice" in _outA and idp.jwks_hits == _hits_a + 1,
              "rc=%d hits %d→%d" % (_rcA, _hits_a, idp.jwks_hits))
        # 刷新已落地 ⇒ 再跑一次走缓存，一次 HTTP 都不发
        _hits_b = idp.jwks_hits
        _rcB, _outB = _login("app")
        check("P1d 刷新后缓存随即更新（第二次同 kid 命中、不发 HTTP）",
              _rcB == 0 and "alice" in _outB and idp.jwks_hits == _hits_b,
              "rc=%d hits %d→%d" % (_rcB, _hits_b, idp.jwks_hits))

        # 验签真跑了吗：再换一把钥匙，且**这一把是假的**（modulus 完全不同）。
        # 若 jwt_verify 只解 claims 不验签，这里会照样「通过」。
        idp.kid = "idp-bogus-key"
        idp.jwks_bogus = True
        _rcC, _outC = _login("app")
        _txtC = _outC.lower()
        check("P1d ext.oidc→crypto.jwt_verify 真在验签名（换成假钥匙，老签名必败）",
              "subject" not in _outC and ("签名" in _outC or "verification error" in _txtC),
              "rc=%d out=%s" % (_rcC, _outC[-200:]))
        # 换回真钥匙 ⇒ 未知 kid 又触发一次刷新 ⇒ 链路恢复
        idp.kid = "idp-test-key"
        idp.jwks_bogus = False
        _rcD, _outD = _login("app")
        check("P1d 换回真钥匙后链路恢复（刷新把缓存拉回正轨）",
              _rcD == 0 and "alice" in _outD and "RS256" in _outD,
              "rc=%d out=%s" % (_rcD, _outD[-160:]))

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

        # ---- P1e) ext.excel / ext.pdf：零依赖写出 + openpyxl 交叉验证（PRD §4.5 / AC-E4-1·2·3·4）----
        # E4 交付面：xlsx 手写 OOXML、pdf 手写 TTF 子集 + PDF 写出，全程不引第三方 Go 依赖
        # （AC-E4-4：go.mod / go.sum 一字不变）。关键证据：
        #  ① Excel 真能用 openpyxl（独立实现）读回，单元格值一致 —— 不是「自己写的自己读」；
        #  ② PDF 把 CJK 字形子集嵌进文件（FontFile2），否则中文会画成方框；
        #  ③ 两路都做 fail-closed：路径越界拒写、字体缺字报错（不静默画方框）。
        _e4 = tempfile.mkdtemp(prefix="e4-")

        # --- Excel ---
        _xls_root = tempfile.mkdtemp(prefix="excel-")
        _xls_path = "demo.xlsx"
        _xls_sheets = {"args": {"root": _xls_root, "path": _xls_path, "sheets": [
            {"name": "明细", "rows": [
                ["单号", "金额", "状态"],
                ["A1001", 12345.6, True],
                ["A1002", -78, False],
                ["合计", 12267.6, None],
            ]},
            {"name": "Sheet2", "rows": [["中文", "CJK"]]},
        ]}}
        _x = http_post_json("http://%s/write" % XLSX_COORD, _xls_sheets)
        _xout = _x.get("result", {})
        _xls_file = os.path.join(_xls_root, _xls_path)
        _xls_ok = ("result" in _x and os.path.exists(_xls_file)
                   and _xout.get("sheets") == 2 and _xout.get("rows") == 5)
        check("P1e ext.excel 写出 xlsx（多 sheet + 数字/布尔/空 混合）", _xls_ok,
              "sheets=%s rows=%s size=%s" % (_xout.get("sheets"), _xout.get("rows"), _xout.get("size")))

        # 交叉验证：用 openpyxl（独立实现）读回，确认值真的写对
        _xls_cross = False
        _xls_detail = "openpyxl 不可用（仅做结构校验）"
        if VENV_PY and os.path.exists(_xls_file):
            _code = (
                "import openpyxl,sys,json\n"
                "wb=openpyxl.load_workbook(sys.argv[1])\n"
                "assert wb.sheetnames==['明细','Sheet2'], wb.sheetnames\n"
                "ws=wb['明细']\n"
                "v=[ [c.value for c in r] for r in ws.iter_rows()]\n"
                "def eq(a,b):\n"
                "    if isinstance(a,(int,float)) and isinstance(b,(int,float)): return abs(a-b)<1e-6\n"
                "    return a==b\n"
                "assert v[0]==['单号','金额','状态'], v[0]\n"
                "assert eq(v[1][1],12345.6) and v[1][2] is True, v[1]\n"
                "assert eq(v[2][1],-78) and v[2][2] is False, v[2]\n"
                "assert eq(v[3][1],12267.6), v[3]\n"
                "print('OK '+json.dumps(wb.sheetnames,ensure_ascii=False))\n"
            )
            _r = subprocess.run([VENV_PY, "-c", _code, _xls_file],
                               capture_output=True, text=True, timeout=60)
            _xls_cross = (_r.returncode == 0 and "OK" in _r.stdout)
            _xls_detail = (_r.stdout.strip() + " | " + _r.stderr.strip())[:200]
        check("P1e ext.excel openpyxl 交叉验证（独立实现读回，值一致）", _xls_cross, _xls_detail)

        # fail-closed：越界路径必须被拒（不得写到 root 之外）
        _xls_esc = http_post_json("http://%s/write" % XLSX_COORD,
                                  {"args": {"root": _xls_root, "path": "../escape.xlsx",
                                            "sheets": [{"name": "s", "rows": [["x"]]}]}})
        check("P1e ext.excel 路径越界 fail-closed",
              "error" in _xls_esc and "path_escape" in _xls_esc.get("error", ""),
              "error=%s" % _xls_esc.get("error"))

        # --- PDF ---
        # 找一颗系统 TTF（simhei 覆盖 GB2312 + 常用汉字）。pdf 侧车只认单文件 TTF，
        # 故只列 .ttf 候选；.ttc / OTF 会被它拒，不应塞进来当成可用字体。
        _pdf_font = ""
        for _fc in (r"C:\Windows\Fonts\simhei.ttf", r"C:\Windows\Fonts\simkai.ttf",
                    r"C:\Windows\Fonts\simfang.ttf", r"C:\Windows\Fonts\simsun.ttf"):
            if os.path.exists(_fc):
                _pdf_font = _fc
                break
        _pdf_root = tempfile.mkdtemp(prefix="pdf-")
        _pdf_path = "report.pdf"
        _pdf_pages = {"args": {"root": _pdf_root, "path": _pdf_path, "font": _pdf_font, "pages": [
            {"text": "对账明细 A 复 B"},
            {"text": "第二页：金额合计 12267.60"},
        ]}}
        _p = http_post_json("http://%s/write" % PDF_COORD, _pdf_pages)
        _pout = _p.get("result", {})
        _pdf_file = os.path.join(_pdf_root, _pdf_path)
        _pdf_ok = ("result" in _p and os.path.exists(_pdf_file) and _pout.get("pages") == 2)
        if _pdf_font:
            _pdf_ok = _pdf_ok and _pout.get("glyphs", 0) >= 1
        check("P1e ext.pdf 写出 PDF（A4 + CJK 子集嵌入）", _pdf_ok,
              "pages=%s glyphs=%s size=%s font=%s" % (_pout.get("pages"), _pout.get("glyphs"),
                                                      _pout.get("size"), _pout.get("font")))

        # 结构校验：%PDF 头 + %%EOF 尾 + 字体已嵌入（FontFile2）+ Type0/Identity-H
        _pdf_struct = False
        _pdf_sdetail = ""
        if os.path.exists(_pdf_file):
            with open(_pdf_file, "rb") as f:
                _raw = f.read()
            _pdf_struct = (_raw[:8] == b"%PDF-1.4" and b"%%EOF" in _raw
                          and b"FontFile2" in _raw and b"/Type0" in _raw
                          and b"Identity-H" in _raw)
            _pdf_sdetail = "bytes=%d font_embedded=%s" % (len(_raw), b"FontFile2" in _raw)
        check("P1e ext.pdf 结构合法（头/尾/Type0/Identity-H/字体嵌入）", _pdf_struct, _pdf_sdetail)

        # fail-closed：字体里没有的字必须报错（不许悄咪咪画成方框）。
        # 用 U+1D11E（音乐谱号 G）这种**任何常见字体都不会带**的字 —— 它既不是 GB2312（simhei），
        # 也不是 Latin/Greek/Cyrillic（DejaVuSans），两条路都必然命中「查不到字形」。
        _pdf_bad = http_post_json("http://%s/write" % PDF_COORD,
                                  {"args": {"root": _pdf_root, "path": "bad.pdf",
                                            "font": _pdf_font, "pages": [{"text": "𝄞"}]}})
        check("P1e ext.pdf 缺字 fail-closed（不许画成方框）",
              "error" in _pdf_bad and "font_no_glyph" in _pdf_bad.get("error", ""),
              "error=%s" % _pdf_bad.get("error"))

        # fail-closed：越界路径
        _pdf_esc = http_post_json("http://%s/write" % PDF_COORD,
                                  {"args": {"root": _pdf_root, "path": "../escape.pdf",
                                            "font": _pdf_font, "pages": [{"text": "x"}]}})
        check("P1e ext.pdf 路径越界 fail-closed",
              "error" in _pdf_esc and "path_escape" in _pdf_esc.get("error", ""),
              "error=%s" % _pdf_esc.get("error"))

        # --- 经内核调用（E4 最关键的一条证据）：证明 ctx.ext.excel / ctx.ext.pdf 真接进了能力目录 ---
        #
        #   上面几项全是**直连侧车 HTTP**，绕开了内核与能力目录。侧车写对 ≠ 能力交付：
        #   2026-09-30 就栽在这里——`plugin/extensions.yaml` 里压根没有 ext.excel / ext.pdf
        #   两段声明，应用里 `ctx.ext.excel.write` 根本不存在，而直连侧车的测试照样 71/71 全绿。
        #   故补这条：注册一个声明 `capabilities: [ext.excel, ext.pdf]` 的 Tool 真跑一次，
        #   走的是内核门禁 → extensions.yaml 能力目录 → 侧车拉起，全链路。
        _rd_seed = os.path.join(ROOT, "plugin", "report_demo.yaml")
        with open(_rd_seed, encoding="utf-8") as f:
            _rd_txt = f.read()
        rc, out = run_gear(["tool", "register", "--registry", reg, "-"], env=env, stdin=_rd_txt)
        check("P1e gear tool register（report.export，声明 ext.excel+ext.pdf）",
              rc == 0, "rc=%d %s" % (rc, out[:160]))

        _re4_root = tempfile.mkdtemp(prefix="e4-kernel-")
        rc, out = run_gear(["tool", "run", "report.export", "--registry", reg, "--input",
                            json.dumps({"root": _re4_root, "title": "对账报表"})], env=env)
        _re4 = parse_tool_out(out)
        # 判据是「输出里有 xlsx / pdf 两行且文件真在」，不是 rc：
        # 样例 Tool 用 pcall 把失败收进 error 字段，失败路径 rc 同样是 0——只看 rc 会全绿。
        _re4_ok = (rc == 0 and bool(_re4.get("xlsx")) and bool(_re4.get("pdf"))
                   and os.path.exists(str(_re4.get("xlsx", "")))
                   and os.path.exists(str(_re4.get("pdf", ""))))
        check("P1e 经内核调用 ctx.ext.excel/pdf.write（能力目录真接上了）", _re4_ok,
              "rc=%d xlsx=%s pdf=%s err=%s" % (rc, _re4.get("xlsx"), _re4.get("pdf"),
                                               _re4.get("error")))

        # 经内核产出的 xlsx 同样交给 openpyxl 读回（不止「文件存在」）
        _re4_cross = False
        _re4_detail = "openpyxl 不可用"
        if VENV_PY and _re4.get("xlsx") and os.path.exists(str(_re4["xlsx"])):
            _code2 = (
                "import openpyxl,sys,json\n"
                "wb=openpyxl.load_workbook(sys.argv[1])\n"
                "assert wb.sheetnames==['明细'], wb.sheetnames\n"
                "ws=wb['明细']\n"
                "v=[[c.value for c in r] for r in ws.iter_rows()]\n"
                "assert v[0]==['项目','数量','是否加急'], v[0]\n"
                "assert v[1][0]=='对账报表' and v[1][1]==12 and v[1][2] is True, v[1]\n"
                "assert v[2][0]=='合计' and v[2][1]==12 and v[2][2] is False, v[2]\n"
                "print('OK '+json.dumps(wb.sheetnames,ensure_ascii=False))\n"
            )
            _r2 = subprocess.run([VENV_PY, "-c", _code2, str(_re4["xlsx"])],
                                 capture_output=True, text=True, timeout=60)
            _re4_cross = (_r2.returncode == 0 and "OK" in _r2.stdout)
            _re4_detail = (_r2.stdout.strip() + " | " + _r2.stderr.strip())[:200]
        check("P1e 经内核产出的 xlsx 也能被 openpyxl 读回", _re4_cross, _re4_detail)

        # 反向验证：越界路径经内核这层也要被守住（不是只在直连侧车时拦得住）
        rc, out = run_gear(["tool", "run", "report.export", "--registry", reg, "--input",
                            json.dumps({"root": _re4_root, "xlsx_path": "../escape.xlsx"})], env=env)
        _esc_out = parse_tool_out(out)
        check("P1e 经内核调用的路径越界仍 fail-closed",
              "path_escape" in str(_esc_out.get("error", "")),
              "error=%s" % _esc_out.get("error"))

        # ---- 4b) ext.oss：S3 兼容对象存储（P1f）----
        #
        # 验证分两层，各司其职，不许互相顶替：
        #   ① **签名正确性** —— 由 Go 单测把关：假 S3 端点从收到的原始请求反推 canonical
        #      request 重算签名比对，且 SigV4 的黄金向量来自 botocore 与 aws4 两个独立实现
        #      （对同一请求给出同一签名）。
        #   ② **接线正确性** —— 这里：直连侧车跑真实往返，再经内核 `tool run` 跑一遍，
        #      证明 `ctx.ext.oss.*` 在应用里真的存在（E4 就是在这一层漏掉的）。
        # 下面的假端点**不验签**（验签是 ① 的事），只负责把对象存起来再吐回去。
        import base64 as _b64oss
        import threading as _thoss
        from http.server import BaseHTTPRequestHandler as _OssBase, ThreadingHTTPServer as _OssSrv
        from urllib.parse import urlparse as _up, parse_qs as _pq

        _oss_store = {}
        _OSS_BUCKET = "fakebucket"

        class _FakeS3(_OssBase):
            protocol_version = "HTTP/1.1"

            def log_message(self, *a):
                pass

            def _key(self):
                p = self.path.split("?", 1)[0]
                pre = "/" + _OSS_BUCKET + "/"
                return p[len(pre):] if p.startswith(pre) else None

            def _send(self, code, body=b"", ctype="application/xml"):
                self.send_response(code)
                self.send_header("Content-Type", ctype)
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                if body:
                    self.wfile.write(body)

            def do_PUT(self):
                n = int(self.headers.get("Content-Length") or 0)
                body = self.rfile.read(n) if n else b""
                _oss_store[self._key()] = (body,
                                           self.headers.get("Content-Type") or "application/octet-stream")
                self.send_response(200)
                self.send_header("ETag", '"v"')
                self.send_header("Content-Length", "0")
                self.end_headers()

            def do_GET(self):
                if _pq(_up(self.path).query).get("list-type") == ["2"]:
                    items = "".join(
                        "<Contents><Key>%s</Key><Size>%d</Size>"
                        "<LastModified>Mon, 30 Sep 2026 00:00:00 GMT</LastModified>"
                        "<ETag>&quot;e&quot;</ETag></Contents>" % (k, len(v[0]))
                        for k, v in sorted(_oss_store.items()))
                    self._send(200, ("<?xml version=\"1.0\" encoding=\"UTF-8\"?>"
                                     "<ListBucketResult><IsTruncated>false</IsTruncated>%s"
                                     "</ListBucketResult>" % items).encode())
                    return
                k = self._key()
                if k not in _oss_store:
                    self._send(404, b"<?xml version=\"1.0\" encoding=\"UTF-8\"?>"
                                    b"<Error><Code>NoSuchKey</Code><Message>no such key</Message></Error>")
                    return
                data, ct = _oss_store[k]
                self.send_response(200)
                self.send_header("Content-Type", ct)
                self.send_header("ETag", '"v"')
                self.send_header("Last-Modified", "Mon, 30 Sep 2026 00:00:00 GMT")
                self.send_header("Content-Length", str(len(data)))
                self.end_headers()
                self.wfile.write(data)

            def do_HEAD(self):
                k = self._key()
                if k not in _oss_store:
                    self._send(404, b"")
                    return
                data, ct = _oss_store[k]
                self.send_response(200)
                self.send_header("Content-Type", ct)
                self.send_header("ETag", '"v"')
                self.send_header("Last-Modified", "Mon, 30 Sep 2026 00:00:00 GMT")
                self.send_header("Content-Length", str(len(data)))
                self.end_headers()

            def do_DELETE(self):
                _oss_store.pop(self._key(), None)
                self.send_response(204)
                self.send_header("Content-Length", "0")
                self.end_headers()

        _oss_srv = _OssSrv(("127.0.0.1", 0), _FakeS3)
        _oss_port = _oss_srv.server_address[1]
        _thoss.Thread(target=_oss_srv.serve_forever, daemon=True).start()

        _oss_conn = {
            "endpoint": "http://127.0.0.1:%d" % _oss_port,
            "region": "us-east-1",
            "bucket": _OSS_BUCKET,
            "access_key_id": "AKIAIOSFODNN7EXAMPLE",
            "access_key_secret": "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY",
        }

        def _oss(method, extra=None):
            a = dict(_oss_conn)
            a.update(extra or {})
            return http_post_json("http://%s/%s" % (OSS_COORD, method), {"args": a})

        _oss_key = "报表/2026 09.txt"
        _oss_body = "你好 oss"
        _oss_b64 = _b64oss.b64encode(_oss_body.encode("utf-8")).decode()

        _p = _oss("put", {"key": _oss_key, "data_base64": _oss_b64, "content_type": "text/plain"})
        check("P1f ext.oss put 成功",
              "result" in _p and _p["result"].get("size") == len(_oss_body.encode("utf-8")),
              "resp=%s" % str(_p)[:120])

        _g = _oss("get", {"key": _oss_key})
        check("P1f ext.oss get 读回内容一致",
              "result" in _g and _g["result"].get("data_base64") == _oss_b64,
              "resp=%s" % str(_g)[:120])
        check("P1f ext.oss get 保住 content_type",
              _g.get("result", {}).get("content_type") == "text/plain",
              "content_type=%s" % _g.get("result", {}).get("content_type"))

        _s = _oss("stat", {"key": _oss_key})
        check("P1f ext.oss stat 取到元数据",
              _s.get("result", {}).get("size") == len(_oss_body.encode("utf-8"))
              and "T" in str(_s.get("result", {}).get("mod_time", "")),
              "resp=%s" % str(_s)[:120])

        _l = _oss("list", {"prefix": "报表/"})
        check("P1f ext.oss list 列到该对象",
              _l.get("result", {}).get("count") == 1,
              "resp=%s" % str(_l)[:160])

        _ps = _oss("presign", {"key": _oss_key, "expires": 600})
        _ps_url = str(_ps.get("result", {}).get("url", ""))
        check("P1f ext.oss presign 签出带签名的 URL",
              "X-Amz-Signature=" in _ps_url and "X-Amz-Expires=600" in _ps_url,
              "url=%s" % _ps_url[:120])

        _d = _oss("delete", {"key": _oss_key})
        check("P1f ext.oss delete 成功", _d.get("result", {}).get("deleted") is True,
              "resp=%s" % str(_d)[:120])
        _g2 = _oss("get", {"key": _oss_key})
        check("P1f ext.oss 删后 get 报 not_found",
              "not_found" in str(_g2.get("error", "")), "error=%s" % _g2.get("error"))

        # 反向验证：坏 key 必须在发出任何请求之前就被拒（fail-closed，不留半截）
        _bk = _oss("get", {"key": "../secret.txt"})
        check("P1f ext.oss 坏 key fail-closed", "bad_key" in str(_bk.get("error", "")),
              "error=%s" % _bk.get("error"))
        # 反向验证：端点连不上 -> upstream_failed（不是静默成功，也不是 panic）
        _ue = _oss("get", {"key": "x.txt", "endpoint": "http://127.0.0.1:1"})
        check("P1f ext.oss 端点连不上 fail-closed", "upstream_failed" in str(_ue.get("error", "")),
              "error=%s" % _ue.get("error"))

        # 经内核：register + run oss.demo —— 这是「ctx.ext.oss.* 真的接进能力目录」的证据
        _oss_seed = os.path.join(ROOT, "plugin", "oss_demo.yaml")
        _oss_seed_txt = open(_oss_seed, encoding="utf-8").read()
        rc, out = run_gear(["tool", "register", "--registry", reg, "-"], env=env, stdin=_oss_seed_txt)
        check("P1f 经内核 register oss.demo", rc == 0, "rc=%s out=%s" % (rc, out[:120]))

        rc, out = run_gear(["tool", "run", "oss.demo", "--registry", reg, "--input",
                            json.dumps(_oss_conn)], env=env)
        _oss_out = parse_tool_out(out)
        check("P1f 经内核调用 ctx.ext.oss 六方法",
              _oss_out.get("put_size") == "9" and _oss_out.get("get_size") == "9"
              and _oss_out.get("stat_size") == "9" and _oss_out.get("list_count") == "1"
              and str(_oss_out.get("presign_ok", "")).strip('"') == "yes"
              and str(_oss_out.get("deleted", "")).strip('"') == "true",
              "out=%s" % str(_oss_out)[:200])

        # 反向验证：坏 key 经内核这层也要被守住（不是只在直连侧车时拦得住）
        rc, out = run_gear(["tool", "run", "oss.demo", "--registry", reg, "--input",
                            json.dumps(dict(_oss_conn, key="../escape.txt"))], env=env)
        _oss_esc = parse_tool_out(out)
        check("P1f 经内核调用的坏 key 仍 fail-closed",
              "bad_key" in str(_oss_esc.get("error", "")), "error=%s" % _oss_esc.get("error"))

        _oss_srv.shutdown()
        _oss_srv.server_close()

        # ---- 4c) ext.redis：手写 RESP2 侧车（P1g）----
        #
        # 验证分两层，与 oss 一致：
        #   ① 协议正确性 —— 由 Go 单测把关（main_test.go 里的 fakeRedis 覆盖全数据结构/锁/队列/fail-closed）。
        #   ② 接线正确性 —— 这里：直连侧车跑真实往返，再经内核 `tool run` 跑一遍，
        #      证明 `ctx.ext.redis.*` 在应用里真的存在（E4 就是在这一层漏掉的）。
        # 下面的假端点**不实现完整 redis**，只实现侧车实际用到的命令子集（足够跑通演示），
        # 且不做 WRONGTYPE 等严格语义（演示用 distinct key，不会踩到）。
        import time as _t, socket as _sock, threading as _th

        class _FakeRedis:
            def __init__(self):
                self._lk = _th.Lock()
                self.kv = {}      # key -> [value, expiry|None]
                self.h = {}       # key -> {field: value}
                self.l = {}       # key -> [items]
                self.s = {}       # key -> set
                self.z = {}       # key -> [(score, member)]
                self._now = _t.time

            def _expired(self, key):
                v = self.kv.get(key)
                if v and v[1] is not None and v[1] <= self._now():
                    del self.kv[key]
                    return True
                return False

            def dispatch(self, args):
                if not args:
                    return b""
                cmd = args[0].upper()
                a = args[1:]
                with self._lk:
                    M = {
                        "SET": self._set, "GET": self._get, "DEL": self._del,
                        "EXISTS": self._exists, "EXPIRE": self._expire, "TTL": self._ttl,
                        "INCRBY": self._incrby, "HSET": self._hset, "HGET": self._hget,
                        "HGETALL": self._hgetall, "HDEL": self._hdel,
                        "LPUSH": self._lpush, "RPUSH": self._rpush, "LPOP": self._lpop,
                        "RPOP": self._rpop, "LLEN": self._llen, "LRANGE": self._lrange,
                        "SADD": self._sadd, "SMEMBERS": self._smembers, "SREM": self._srem,
                        "SCARD": self._scard, "ZADD": self._zadd, "ZSCORE": self._zscore,
                        "ZREM": self._zrem, "ZRANGE": self._zrange,
                        "ZRANGEBYSCORE": self._zrangebyscore,
                    }
                    fn = M.get(cmd)
                    if not fn:
                        return b"-ERR unknown command\r\n"
                    try:
                        return fn(a)
                    except Exception as e:
                        return ("-ERR %s\r\n" % e).encode()

            # ---- encoders ----
            @staticmethod
            def _ok():
                return b"+OK\r\n"
            @staticmethod
            def _int(n):
                return (":%d\r\n" % n).encode()
            @staticmethod
            def _bulk(s):
                if s is None:
                    return b"$-1\r\n"
                b = s.encode() if isinstance(s, str) else str(s).encode()
                return b"$%d\r\n%s\r\n" % (len(b), b)
            @staticmethod
            def _arr(items):
                if items is None:
                    return b"*-1\r\n"
                out = b"*%d\r\n" % len(items)
                for it in items:
                    out += it
                return out

            def _exists_key(self, key):
                return (key in self.kv) or (key in self.h) or (key in self.l) \
                    or (key in self.s) or (key in self.z)

            # ---- KV ----
            def _set(self, a):
                key = a[0]; val = a[1]
                i = 2; ex = None
                while i + 1 < len(a):
                    if a[i].upper() == "EX":
                        ex = int(a[i + 1]); i += 2
                    else:
                        i += 1
                exp = (self._now() + ex) if ex is not None else None
                self.kv[key] = [val, exp]
                return self._ok()
            def _get(self, a):
                self._expired(a[0])
                v = self.kv.get(a[0])
                return self._bulk(v[0] if v else None)
            def _del(self, a):
                c = 0
                for k in a:
                    if self._exists_key(k):
                        self.kv.pop(k, None); self.h.pop(k, None)
                        self.l.pop(k, None); self.s.pop(k, None); self.z.pop(k, None)
                        c += 1
                return self._int(c)
            def _exists(self, a):
                return self._int(sum(1 for k in a if self._exists_key(k)))
            def _expire(self, a):
                self._expired(a[0])
                if a[0] in self.kv:
                    self.kv[a[0]][1] = self._now() + int(a[1])
                    return self._int(1)
                return self._int(0)
            def _ttl(self, a):
                self._expired(a[0])
                v = self.kv.get(a[0])
                if not v:
                    return self._int(-2)
                if v[1] is None:
                    return self._int(-1)
                return self._int(int(v[1] - self._now()))
            def _incrby(self, a):
                key = a[0]; by = int(a[1])
                self._expired(key)
                cur = self.kv.get(key)
                base = 0 if not cur else int(cur[0])
                newv = base + by
                self.kv[key] = [str(newv), None]
                return self._int(newv)

            # ---- Hash ----
            def _hset(self, a):
                m = self.h.setdefault(a[0], {})
                added = 0
                for i in range(1, len(a) - 1, 2):
                    f, v = a[i], a[i + 1]
                    if f not in m:
                        added += 1
                    m[f] = v
                return self._int(added)
            def _hget(self, a):
                return self._bulk(self.h.get(a[0], {}).get(a[1]))
            def _hgetall(self, a):
                items = []
                for f, v in self.h.get(a[0], {}).items():
                    items.append(self._bulk(f)); items.append(self._bulk(v))
                return self._arr(items)
            def _hdel(self, a):
                m = self.h.get(a[0], {})
                c = 0
                for f in a[1:]:
                    if f in m:
                        del m[f]; c += 1
                return self._int(c)

            # ---- List ----
            def _push(self, a, left):
                lst = self.l.setdefault(a[0], [])
                if left:
                    lst[0:0] = a[1:]
                else:
                    lst.extend(a[1:])
                return self._int(len(lst))
            def _lpush(self, a):
                return self._push(a, True)
            def _rpush(self, a):
                return self._push(a, False)
            def _lpop(self, a):
                lst = self.l.get(a[0])
                cnt = int(a[1]) if len(a) > 1 else 1
                if not lst:
                    return self._bulk(None) if cnt <= 1 else self._arr([])
                if cnt > 1:
                    popped = lst[:cnt]; del lst[:cnt]
                    return self._arr([self._bulk(x) for x in popped])
                return self._bulk(lst.pop(0))
            def _rpop(self, a):
                lst = self.l.get(a[0])
                cnt = int(a[1]) if len(a) > 1 else 1
                if not lst:
                    return self._bulk(None) if cnt <= 1 else self._arr([])
                if cnt > 1:
                    popped = lst[-cnt:]; del lst[-cnt:]
                    return self._arr([self._bulk(x) for x in popped])
                return self._bulk(lst.pop())
            def _llen(self, a):
                return self._int(len(self.l.get(a[0], [])))
            def _lrange(self, a):
                lst = self.l.get(a[0], [])
                n = len(lst)
                start = int(a[1]); stop = int(a[2])
                if start < 0: start += n
                if stop < 0: stop += n
                stop = min(stop, n - 1)
                if start < 0: start = 0
                seg = lst[start:stop + 1] if start <= stop else []
                return self._arr([self._bulk(x) for x in seg])

            # ---- Set ----
            def _sadd(self, a):
                st = self.s.setdefault(a[0], set())
                c = 0
                for m in a[1:]:
                    if m not in st:
                        st.add(m); c += 1
                return self._int(c)
            def _smembers(self, a):
                return self._arr([self._bulk(m) for m in self.s.get(a[0], set())])
            def _srem(self, a):
                st = self.s.get(a[0], set())
                c = 0
                for m in a[1:]:
                    if m in st:
                        st.discard(m); c += 1
                return self._int(c)
            def _scard(self, a):
                return self._int(len(self.s.get(a[0], set())))

            # ---- ZSet ----
            def _zadd(self, a):
                z = self.z.setdefault(a[0], [])
                added = 0
                for i in range(1, len(a) - 1, 2):
                    sc = float(a[i]); mem = a[i + 1]
                    hit = False
                    for j, (s2, m2) in enumerate(z):
                        if m2 == mem:
                            z[j] = (sc, mem); hit = True; break
                    if not hit:
                        z.append((sc, mem)); added += 1
                return self._int(added)
            def _zscore(self, a):
                for sc, mem in self.z.get(a[0], []):
                    if mem == a[1]:
                        return self._bulk("%s" % sc)
                return self._bulk(None)
            def _zrem(self, a):
                z = self.z.get(a[0], [])
                c = 0
                for mem in a[1:]:
                    for j, (sc, m2) in enumerate(z):
                        if m2 == mem:
                            del z[j]; c += 1; break
                return self._int(c)
            def _zrange(self, a):
                z = sorted(self.z.get(a[0], []))
                n = len(z)
                start = int(a[1]); stop = int(a[2])
                if start < 0: start += n
                if stop < 0: stop += n
                stop = min(stop, n - 1)
                if start < 0: start = 0
                seg = z[start:stop + 1] if start <= stop else []
                withscores = len(a) > 3 and a[3].upper() == "WITHSCORES"
                items = []
                for sc, mem in seg:
                    items.append(self._bulk(mem))
                    if withscores:
                        items.append(self._bulk("%s" % sc))
                return self._arr(items)
            def _zrangebyscore(self, a):
                z = sorted(self.z.get(a[0], []))
                lo = -1e300 if a[1] in ("-inf",) else float(a[1])
                hi = 1e300 if a[2] in ("+inf",) else float(a[2])
                seg = [(sc, mem) for sc, mem in z if lo <= sc <= hi]
                off = 0; cnt = len(seg)
                up = [x.upper() for x in a[3:]]
                if "LIMIT" in up:
                    li = up.index("LIMIT")
                    off = int(a[3 + li + 1]); cnt = int(a[3 + li + 2])
                seg = seg[off:off + cnt]
                return self._arr([self._bulk(mem) for sc, mem in seg])

        _fr = _FakeRedis()

        def _read_exact(f, n):
            buf = b""
            while len(buf) < n:
                chunk = f.read(n - len(buf))
                if not chunk:
                    raise EOFError
                buf += chunk
            return buf

        def _read_cmd(f):
            line = f.readline()
            if not line or line[:1] != b"*":
                return None
            try:
                n = int(line[1:].strip())
            except Exception:
                return None
            out = []
            for _ in range(n):
                h = f.readline()
                if h[:1] != b"$":
                    return None
                blen = int(h[1:].strip())
                if blen == -1:
                    out.append(None); continue
                out.append(_read_exact(f, blen).decode("utf-8"))
                f.read(2)
            return out

        def _redis_serve(conn):
            f = conn.makefile("rwb")
            try:
                while True:
                    args = _read_cmd(f)
                    if args is None:
                        break
                    resp = _fr.dispatch(args)
                    if resp:
                        f.write(resp); f.flush()
            except Exception:
                pass
            finally:
                try: conn.close()
                except Exception: pass

        def _accept_loop():
            while True:
                try:
                    c, _ = _rsrv.accept()
                except Exception:
                    break
                _th.Thread(target=_redis_serve, args=(c,), daemon=True).start()

        _rsrv = _sock.socket(_sock.AF_INET, _sock.SOCK_STREAM)
        _rsrv.setsockopt(_sock.SOL_SOCKET, _sock.SO_REUSEADDR, 1)
        _rsrv.bind(("127.0.0.1", 0))
        _rsrv.listen(8)
        _rport = _rsrv.getsockname()[1]
        _th.Thread(target=_accept_loop, daemon=True).start()

        def _redis(method, extra=None):
            a = {"host": "127.0.0.1", "port": _rport}
            a.update(extra or {})
            return http_post_json("http://%s/%s" % (REDIS_COORD, method), {"args": a})

        # ① 直连侧车：全数据结构 + 锁 + 延迟队列真实往返
        _rset = _redis("set", {"key": "kv:1", "value": "hi"})
        _rget = _redis("get", {"key": "kv:1"})
        check("P1g ext.redis set/get 往返", "result" in _rset and "result" in _rget
              and _rget["result"].get("value") == "hi", "get=%s" % _rget.get("result"))

        _rinc = _redis("incr", {"key": "cnt:1", "by": 3})
        check("P1g ext.redis incr", "result" in _rinc and _rinc["result"].get("value") == 3,
              "v=%s" % _rinc.get("result"))

        _rhset = _redis("hset", {"key": "h:1", "fields": {"name": "枢"}})
        _rhget = _redis("hget", {"key": "h:1", "field": "name"})
        check("P1g ext.redis hash hset/hget", "result" in _rhset
              and _rhget["result"].get("value") == "枢", "hget=%s" % _rhget.get("result"))

        _rlr = _redis("rpush", {"key": "l:1", "values": ["a", "b", "c"]})
        _rlra = _redis("lrange", {"key": "l:1", "start": 0, "stop": -1})
        check("P1g ext.redis list rpush/lrange", "result" in _rlra
              and _rlra["result"].get("values") == ["a", "b", "c"], "vals=%s" % _rlra.get("result"))

        _rsc = _redis("sadd", {"key": "s:1", "members": ["x", "y", "z"]})
        _rsca = _redis("scard", {"key": "s:1"})
        check("P1g ext.redis set sadd/scard", "result" in _rsca
              and _rsca["result"].get("cardinality") == 3, "card=%s" % _rsca.get("result"))

        _rzsc = _redis("zadd", {"key": "z:1", "members": [
            {"score": 1, "value": "low"}, {"score": 2, "value": "mid"}]})
        _rzsca = _redis("zscore", {"key": "z:1", "member": "mid"})
        check("P1g ext.redis zset zadd/zscore", "result" in _rzsca
              and "2" in str(_rzsca["result"].get("score")), "score=%s" % _rzsca.get("result"))

        _rlk = _redis("lock", {"key": "lock:1", "token": "t1", "ttl_seconds": 30})
        _rulk = _redis("unlock", {"key": "lock:1", "token": "t1"})
        check("P1g ext.redis lock/unlock（单连接串行原子）",
              "result" in _rlk and _rlk["result"].get("acquired") is True
              and "result" in _rulk and _rulk["result"].get("released") is True,
              "acq=%s rel=%s" % (_rlk.get("result"), _rulk.get("result")))

        _renq = _redis("enqueue", {"key": "dq:1", "items": [
            {"payload": "j1", "delay_seconds": 0}, {"payload": "j2", "delay_seconds": 0}]})
        _rdeq = _redis("dequeue", {"key": "dq:1", "batch": 10})
        _items = _rdeq.get("result", {}).get("items", [])
        check("P1g ext.redis 延迟队列入队+原子出队", "result" in _renq
              and len(_items) == 2 and _items[0].get("payload") == "j1",
              "items=%s" % _items)

        # 反向验证：坏 key（含 \n）在侧车校验层就被拦（不连假端点也能 fail-closed）
        _rbk = _redis("set", {"key": "bad\nkey"})
        check("P1g ext.redis 坏 key fail-closed（含 \\n）", "error" in _rbk, "err=%s" % _rbk.get("error"))

        # ② 经内核：register + run redis.demo —— 证明 ctx.ext.redis.* 真的接进能力目录
        _redis_seed = os.path.join(ROOT, "plugin", "redis_demo.yaml")
        _redis_seed_txt = open(_redis_seed, encoding="utf-8").read()
        rc, out = run_gear(["tool", "register", "--registry", reg, "-"], env=env, stdin=_redis_seed_txt)
        check("P1g 经内核 register redis.demo", rc == 0, "rc=%s out=%s" % (rc, out[:120]))

        rc, out = run_gear(["tool", "run", "redis.demo", "--registry", reg, "--input",
                            json.dumps({"host": "127.0.0.1", "port": _rport})], env=env)
        _redis_out = parse_tool_out(out)
        check("P1g 经内核调用 ctx.ext.redis 全结构+锁+队列",
              _redis_out.get("kv_got") == "hello"
              and _redis_out.get("incr_val") == "5"
              and _redis_out.get("hash_val") == "枢"
              and _redis_out.get("list_vals") == "a,b,c"
              and _redis_out.get("set_card") == "3"
              and _redis_out.get("lock_acquired", "").strip('"') == "true"
              and _redis_out.get("lock_released", "").strip('"') == "true"
              and _redis_out.get("deq_count") == "2",
              "out=%s" % str(_redis_out)[:200])

        # 反向验证：坏 key 经内核这层也要被守住（不是只在直连侧车时拦得住）
        rc, out = run_gear(["tool", "run", "redis.demo", "--registry", reg, "--input",
                            json.dumps({"host": "127.0.0.1", "port": _rport,
                                        "kv_key": "bad\nkey"})], env=env)
        _redis_bad = parse_tool_out(out)
        check("P1g 经内核调用的坏 key 仍 fail-closed",
              "bad_key" in str(_redis_bad.get("error", "")), "err=%s" % _redis_bad.get("error"))

        try:
            _rsrv.close()
        except Exception:
            pass

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
            sv = start_sidecar(os.path.join(ROOT, "build", "plugin", "serve-sidecar.exe"), SERVE_COORD)
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

        # ---- 10) 统一注册表应含 extensions.yaml 声明的全部 ext ----
        rc, out = run_gear(["ai", "capabilities"], env=env)
        import re
        caps = set(re.findall(r"\[ext\]\s+(ext\.\w+)", out))
        # 期望集合**从 extensions.yaml 现场解析**，不手写死集合。
        # 2026-09-30 教训：此前手写 all9（标题写「9 个」但集合里其实列了 10 个），
        # 而且漏了 ext.excel / ext.pdf——新增 ext 时没人同步这个集合，断言形同虚设，
        # E4「侧车已交付、却没注册进能力目录」就这么被 71/71 全绿放过去了。
        _ext_yml = os.path.join(ROOT, "plugin", "extensions.yaml")
        with open(_ext_yml, encoding="utf-8") as _f:
            _declared = set(m[:-1] for m in re.findall(r"^ext\.[a-z0-9]+:", _f.read(), re.M))
        check("统一注册表含 extensions.yaml 声明的全部 ext（%d 个）" % len(_declared),
              _declared.issubset(caps),
              "缺: %s | 发现: %s" % (sorted(_declared - caps), sorted(caps)))
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
