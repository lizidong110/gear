#!/usr/bin/env python3
"""plugin/mcp_wire_probe.py —— MCP 线上帧取证（stdio / streamable HTTP 双模式）。

## 为什么必须存在这个工具

go-sdk v1.8.0 依 SEP-2106 草案**放行** `tools/call` 的 `structuredContent` 为数组/标量，
而它自带的客户端同样宽松 —— 于是「本地冒烟全绿」**不能**证明严格客户端能用。
WorkBuddy 用 zod 校验，顶层不是 record 就整条丢弃：

    {"expected":"record","path":["structuredContent"],
     "message":"Invalid input: expected record, received array"}

表现是「一个工具都用不了」，而所有 Go 用例都是绿的。这类事故只在**真实线上帧**上看得见。
所以本探针做三件事：

  ① **抓真实帧**：用**原始 JSON-RPC**（不经过任何 SDK 客户端，不走它的宽松校验）与
     `gear mcp` 对话，把每一帧原样落盘成 JSONL —— 可贴进 issue、可与历史版本逐帧比对。
  ② **用严格判据逐帧判**：`structuredContent` 顶层必须是 JSON 对象；缺了它、或是数组/标量，
     一律判 FAIL（判据与被拒客户端的报错语义一致，见 `strict_client_violations`）。
  ③ **判据自检**：喂一个**合成的裸数组帧**，必须判违规。否则「全绿」可能只是判据自己空转 ——
     这与项目对 staticcheck 做注入探测是同一纪律（工具的「绿」必须先证明它会红）。

## 用法

    python plugin/mcp_wire_probe.py                  # stdio（默认，与 WorkBuddy/Claude Desktop 同路）
    python plugin/mcp_wire_probe.py --http           # streamable HTTP（与 WorkBuddy MCP 连接器同路）
    python plugin/mcp_wire_probe.py --bin build/gear.exe
    python plugin/mcp_wire_probe.py --dump build/frames.jsonl
    python plugin/mcp_wire_probe.py --keep           # 保留临时 registry/执行库/台账目录（排障用）

退出码：`0` = 全部判据通过；`1` = 至少一条 FAIL；`2` = 环境/启动失败（二进制缺失等）。

自带临时 registry + 执行库（`GEAR_EXEC_DB`）+ 台账目录，**探完即杀、不留常驻进程** ——
与 `plugin/verify.py` 同一纪律。探针不改仓库里的任何 gated 产物。
"""
import argparse
import http.client
import json
import os
import queue
import re
import shutil
import signal
import socket
import subprocess
import sys
import tempfile
import threading
import time

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
PROTOCOL_VERSION = "2025-06-18"

# 探针自带的样例 bundle：**只用纯函数原子**（template/crypto/time 不进门禁、无外部依赖），
# 因此在任何机器上都能真的落库并真的跑一把 —— 探针自己不能是假红源。
GOOD_BUNDLE = """kind: App
id: wireprobe
name: MCP 线上探针应用
description: 探针用最小应用：一条 template 纯函数 Flow，无外部依赖
entry_flow: wireprobe.flow
requires:
  capabilities:
    - template
---
kind: Flow
id: wireprobe.flow
name: 探针 Flow
description: 单步 template.render，验证「apply → run → execution」在线上帧上闭环
vars:
  who: world
steps:
  - id: greet
    call: template.render
    input:
      text: "hello ${vars.who}"
      vars:
        who: "${vars.who}"
"""

# 故意写坏的 bundle：验证 `gear_validate` 是「编译器」（一次报全）且**不落库**。
# 三处独立问题：未注册能力 / 依赖不存在的 step / when 用了模板。
BAD_BUNDLE = """kind: Flow
id: wireprobe.bad
name: 坏 Flow
steps:
  - id: s1
    call: __no_such_capability.run
  - id: s2
    depends_on:
      - __no_such_step
    call: template.render
    input:
      text: "x"
  - id: s3
    call: template.render
    when: "${vars.flag}"
    input:
      text: "y"
"""

RESULTS = []
NOTES = []


def check(name, ok, detail=""):
    """硬判据：进退出码。"""
    RESULTS.append((name, bool(ok), detail))
    print(("[PASS] " if ok else "[FAIL] ") + name + (("  -> " + detail) if detail else ""))


def note(name, detail=""):
    """软信息：只汇报，不影响退出码（例如「SDK 是否顺带补了文本兜底」这类实现细节）。"""
    NOTES.append((name, detail))
    print("[INFO] " + name + (("  -> " + detail) if detail else ""))


# --------------------------------------------------------------------------- #
# 判定：严格客户端同款判据
# --------------------------------------------------------------------------- #

_MISSING = object()


def strict_client_violations(result):
    """模拟严格客户端（WorkBuddy / zod）对一条 `tools/call` result 的校验。

    返回违规列表（空列表 = 可以收下）。判据来源：
      - MCP 2025-06-18 规定 `structuredContent` 顶层**必须是 JSON 对象**；
      - 严格客户端对**成功**响应做对象校验，对 `isError:true` 响应不做（规范无约定）。
    判据自身必须先被证明会红（见 probe() 开头的自检），否则「全绿」没有意义。
    """
    if not isinstance(result, dict):
        return ["result 不是 JSON 对象：%s" % type(result).__name__]
    if result.get("isError") is True:
        return []  # 错误响应只看文本（[CODE] 在 content 里），不做对象校验
    sc = result.get("structuredContent", _MISSING)
    if sc is _MISSING:
        return ["缺 structuredContent：成功响应的结构化通道缺失（严格客户端拿不到结构化结果）"]
    if sc is None:
        return ["structuredContent 是 null：严格客户端 expected record, received null"]
    if not isinstance(sc, dict):
        return ["structuredContent 顶层不是 record：expected record, received %s"
                % type(sc).__name__]
    return []


def content_text(result):
    """取出第一个 text content（错误码 `[CODE] hint` 就在这里）。"""
    for item in result.get("content") or []:
        if isinstance(item, dict) and item.get("type") == "text":
            return item.get("text", "")
    return ""


# --------------------------------------------------------------------------- #
# 环境
# --------------------------------------------------------------------------- #

def free_port():
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def port_listening(port, host="127.0.0.1"):
    with socket.socket() as s:
        s.settimeout(0.5)
        return s.connect_ex((host, port)) == 0


def probe_env(reg_path):
    """派生 gear 的环境：临时执行库 + 清代理（沙箱里 HTTP_PROXY 会让 127.0.0.1 请求走代理）。

    台账目录不加环境变量：它由 `--apps-dir` 显式指定（唯一真相源），
    免得出现「探针以为指了 A、gear 实际写的是 B」这类只有排障时才发现的错位。
    """
    env = dict(os.environ)
    for k in ("HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy",
              "ALL_PROXY", "all_proxy", "GEAR_AI_API_KEY"):
        env.pop(k, None)
    env["NO_PROXY"] = "127.0.0.1,localhost"
    env["no_proxy"] = env["NO_PROXY"]
    env["GEAR_EXEC_DB"] = os.path.join(os.path.dirname(reg_path), "exec.db")
    return env


def kill_tree(pid):
    """结束 gear 进程树（Windows 上 DETACHED/子进程要靠 /T）。"""
    if pid is None:
        return
    try:
        if os.name == "nt":
            subprocess.run(["taskkill", "/PID", str(pid), "/T", "/F"],
                           capture_output=True, timeout=15)
        else:
            os.kill(pid, signal.SIGKILL)
    except Exception:
        pass


def stop_process(proc, grace=3):
    """收尾一个 gear 进程：先请它自己退，超时再杀树。返回 True = 自行退出（未被强杀）。

    实测语义（`gear mcp --http`）：会话 `DELETE /mcp` 只注销**该会话**（204），
    监听进程**继续服务**其它会话 —— 它是多会话长驻 server，由 SIGTERM/Ctrl-C 结束
    （与 `internal/cli/mcp.go` 的注释口径一致）。所以「探完即杀」是探针的职责，
    本函数就是履行它；强杀与否只作 INFO 记录，不算失败。
    """
    try:
        proc.wait(timeout=grace)
        return True
    except Exception:
        pass
    if os.name != "nt":
        try:
            os.kill(proc.pid, signal.SIGTERM)
            proc.wait(timeout=5)
            return True
        except Exception:
            pass
    kill_tree(proc.pid)
    return False


# --------------------------------------------------------------------------- #
# 帧记录
# --------------------------------------------------------------------------- #

class Frames:
    """原始帧落盘（JSONL）：每行一帧，便于 diff 两个版本的工具面 / 贴 issue。"""

    def __init__(self, mode, bin_path, bin_size, bin_mtime):
        self.rows = []
        self.meta = {
            "mode": mode,
            "protocol_version": PROTOCOL_VERSION,
            "bin": bin_path,
            "bin_size": bin_size,
            "bin_mtime": bin_mtime,
            "started": time.strftime("%Y-%m-%dT%H:%M:%S"),
        }

    def add(self, direction, frame):
        self.rows.append({"_dir": direction, "_seq": len(self.rows), "frame": frame})

    def dump(self, path):
        with open(path, "w", encoding="utf-8", newline="\n") as f:
            f.write(json.dumps({"_meta": self.meta}, ensure_ascii=False) + "\n")
            for row in self.rows:
                f.write(json.dumps(row, ensure_ascii=False) + "\n")
        return path


# --------------------------------------------------------------------------- #
# 两种 transport（都用**原始 JSON-RPC**，不经 SDK 客户端）
# --------------------------------------------------------------------------- #

class TransportError(Exception):
    pass


class StdioSession:
    """stdio transport：newline-delimited JSON-RPC。stdout 只应有协议帧，日志走 stderr。"""

    mode = "stdio"

    def __init__(self, argv, env, frames, timeout=15):
        self.frames = frames
        self.timeout = timeout
        self.noise = []          # stdout 上的非 JSON 行 = 协议污染证据
        self.stderr = []
        self.server_frames = []  # 服务端主动发的通知/请求
        self._q = queue.Queue()
        self._id = 0
        self.proc = subprocess.Popen(
            argv, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
            env=env, cwd=ROOT)
        threading.Thread(target=self._pump_stdout, daemon=True).start()
        threading.Thread(target=self._pump_stderr, daemon=True).start()

    def _pump_stdout(self):
        for raw in self.proc.stdout:
            line = raw.decode("utf-8", "replace").strip()
            if not line:
                continue
            try:
                self._q.put(json.loads(line))
            except Exception:
                self.noise.append(line[:200])

    def _pump_stderr(self):
        for raw in self.proc.stderr:
            self.stderr.append(raw.decode("utf-8", "replace").rstrip())

    def _write(self, frame):
        self.frames.add("c2s", frame)
        try:
            self.proc.stdin.write((json.dumps(frame, ensure_ascii=False) + "\n").encode("utf-8"))
            self.proc.stdin.flush()
        except Exception as e:
            raise TransportError("写 stdin 失败（服务进程已退出？%s）: %s"
                                 % (self.stderr_tail(), e))

    def stderr_tail(self, n=3):
        return " | ".join(self.stderr[-n:]) if self.stderr else "无 stderr"

    def notify(self, method, params=None):
        frame = {"jsonrpc": "2.0", "method": method}
        if params is not None:
            frame["params"] = params
        self._write(frame)

    def request(self, method, params=None, timeout=None):
        self._id += 1
        rid = self._id
        frame = {"jsonrpc": "2.0", "id": rid, "method": method}
        if params is not None:
            frame["params"] = params
        self._write(frame)
        return self._await(rid, method, timeout or self.timeout)

    def _await(self, rid, method, timeout):
        deadline = time.time() + timeout
        while True:
            remain = deadline - time.time()
            if remain <= 0:
                raise TransportError("%s 超时（%.0fs 内没有 id=%s 的响应；stderr: %s）"
                                     % (method, timeout, rid, self.stderr_tail()))
            try:
                obj = self._q.get(timeout=remain)
            except queue.Empty:
                raise TransportError("%s 超时（%.0fs；stderr: %s）"
                                     % (method, timeout, self.stderr_tail()))
            self.frames.add("s2c", obj)
            if obj.get("id") == rid:
                return obj
            self.server_frames.append(obj)

    def close(self):
        try:
            self.proc.stdin.close()   # EOF → SDK 的 Run 返回，进程自然退出
        except Exception:
            pass
        return stop_process(self.proc, grace=5)


class HttpSession:
    """streamable HTTP transport：POST /mcp，可能回 SSE 或单帧 JSON。"""

    mode = "http"

    def __init__(self, argv, env, frames, port, timeout=15):
        self.frames = frames
        self.timeout = timeout
        self.port = port
        self.path = "/mcp"
        self.session_id = None
        self.stderr = []
        self.server_frames = []
        # stdout **必须接管而不能丢进 DEVNULL**：HTTP 下它虽然不是协议通道，但按 CLI 口径
        # 「诊断一律 stderr」它也不该有任何输出。早先这里用的 DEVNULL 让协议卫生判据在
        # HTTP 模式下**完全空转**（把接线摘掉探针照样绿）——是负向验证抓出来的。
        self.noise = []
        self._id = 0
        self.proc = subprocess.Popen(argv, stdout=subprocess.PIPE,
                                     stderr=subprocess.PIPE, env=env, cwd=ROOT)
        threading.Thread(target=self._pump_stdout, daemon=True).start()
        threading.Thread(target=self._pump_stderr, daemon=True).start()
        deadline = time.time() + 25
        while time.time() < deadline:
            if port_listening(port):
                return
            if self.proc.poll() is not None:
                raise TransportError("gear mcp --http 提前退出（rc=%s）: %s"
                                     % (self.proc.returncode, self.stderr_tail()))
            time.sleep(0.2)
        raise TransportError("等待 %d 端口监听超时（stderr: %s）" % (port, self.stderr_tail()))

    def _pump_stdout(self):
        for raw in self.proc.stdout:
            line = raw.decode("utf-8", "replace").rstrip()
            if line.strip():
                self.noise.append(line[:200])

    def _pump_stderr(self):
        for raw in self.proc.stderr:
            self.stderr.append(raw.decode("utf-8", "replace").rstrip())

    def stderr_tail(self, n=3):
        return " | ".join(self.stderr[-n:]) if self.stderr else "无 stderr"

    def _post(self, frame, timeout=None):
        body = json.dumps(frame, ensure_ascii=False).encode("utf-8")
        headers = {
            "Content-Type": "application/json",
            "Accept": "application/json, text/event-stream",
            "MCP-Protocol-Version": PROTOCOL_VERSION,
        }
        if self.session_id:
            headers["Mcp-Session-Id"] = self.session_id
        conn = http.client.HTTPConnection("127.0.0.1", self.port, timeout=timeout or self.timeout)
        try:
            conn.request("POST", self.path, body=body, headers=headers)
            resp = conn.getresponse()
            status = resp.status
            ct = (resp.getheader("Content-Type") or "").lower()
            sid = resp.getheader("Mcp-Session-Id") or resp.getheader("mcp-session-id")
            data = resp.read()
        finally:
            conn.close()
        if sid:
            self.session_id = sid
        # 端点路径自适应：go-sdk 的 handler 通常挂 /mcp，但允许挂在 / ——探针不该因为
        # 「路径猜错」而报一个假红，所以 404 时换一处再试一次。
        if status == 404 and self.path == "/mcp":
            self.path = "/"
            return self._post(frame, timeout)
        return status, ct, data.decode("utf-8", "replace")

    def _frames_in(self, status, ct, text):
        if not text.strip():
            return []
        if "text/event-stream" in ct:
            out = []
            for line in text.splitlines():
                line = line.rstrip("\r")
                if line.startswith("data:"):
                    payload = line[5:].strip()
                    if payload and payload != "[DONE]":
                        try:
                            out.append(json.loads(payload))
                        except Exception:
                            self.noise.append(payload[:200])
            return out
        try:
            return [json.loads(text)]
        except Exception:
            self.noise.append(text[:200])
            return []

    def notify(self, method, params=None):
        frame = {"jsonrpc": "2.0", "method": method}
        if params is not None:
            frame["params"] = params
        self.frames.add("c2s", frame)
        status, ct, text = self._post(frame)
        for obj in self._frames_in(status, ct, text):
            self.frames.add("s2c", obj)

    def request(self, method, params=None, timeout=None):
        self._id += 1
        rid = self._id
        frame = {"jsonrpc": "2.0", "id": rid, "method": method}
        if params is not None:
            frame["params"] = params
        self.frames.add("c2s", frame)
        status, ct, text = self._post(frame, timeout)
        got = self._frames_in(status, ct, text)
        for obj in got:
            self.frames.add("s2c", obj)
        for obj in got:
            if obj.get("id") == rid:
                return obj
        raise TransportError("%s 没有拿到 id=%s 的响应（http=%d ct=%s body=%s）"
                             % (method, rid, status, ct, text[:200]))

    def close(self):
        # 规范给的会话注销方式是 DELETE + Mcp-Session-Id。它只注销**本会话**（实测 204），
        # 不会终止监听进程 —— 那由 stop_process 负责。
        if self.session_id:
            try:
                conn = http.client.HTTPConnection("127.0.0.1", self.port, timeout=5)
                conn.request("DELETE", self.path, headers={"Mcp-Session-Id": self.session_id})
                conn.getresponse().read()
                conn.close()
            except Exception:
                pass
        return stop_process(self.proc)


# --------------------------------------------------------------------------- #
# 探针主体（两种 transport 共用）
# --------------------------------------------------------------------------- #

def run_calls(sess, calls, seen):
    """逐个跑 tools/call，返回 {tool: {"result":…, "violations":[…], "keys":[…], "text":str}}。

    `seen` 是全局成功帧台账——每次调用都记进去，最后统一过严格判据，
    这样「探针以后新增一次调用却忘了判」不会成为缺口。
    """
    out = {}
    for name, args in calls:
        resp = sess.request("tools/call", {"name": name, "arguments": args})
        if "error" in resp:
            out[name] = {"rpc_error": resp["error"], "violations": ["JSON-RPC error"],
                         "keys": [], "text": ""}
            continue
        result = resp.get("result") or {}
        seen.append((name, result))
        sc = result.get("structuredContent")
        out[name] = {
            "result": result,
            "violations": strict_client_violations(result),
            "keys": sorted(sc.keys()) if isinstance(sc, dict) else [],
            "text": content_text(result),
        }
    return out


def probe(sess, frames):
    # 成功帧与「刻意错误」帧分开记账，最后统一过严格判据（见 §7）。
    seen = []
    seen_err = []

    def call(name, args, timeout=None, expect_error=False):
        """统一的 tools/call 入口：登记到 seen（成功帧台账），返回 (resp, result)。

        `expect_error=True` 用于**刻意构造的错误路径**：它们同样是 isError，但不该算进
        「成功帧都是对象」的台账，也不该触发「意外 isError」判据——否则判据会假阳性。
        """
        resp = sess.request("tools/call", {"name": name, "arguments": args}, timeout)
        result = resp.get("result") or {}
        (seen_err if expect_error else seen).append((name, result))
        return resp, result

    # ---- 0) 判据自检：工具自己的「绿」必须先证明会红 ----
    bad = strict_client_violations({"structuredContent": [{"a": 1}]})
    check("判据自检：合成的裸数组帧必须被判违规（防判据空转）", bool(bad),
          bad[0][:80] if bad else "判据没抓到裸数组！")
    check("判据自检：合法对象帧必须通过（防判据过严误伤）",
          not strict_client_violations({"structuredContent": {"items": []}}))
    check("判据自检：缺 structuredContent 必须被判违规",
          bool(strict_client_violations({"content": [{"type": "text", "text": "x"}]})))
    check("判据自检：isError 响应不做对象校验（规范无约定）",
          not strict_client_violations({"isError": True, "content": [{"type": "text", "text": "[X] y"}]}))

    # ---- 1) 握手 ----
    resp = sess.request("initialize", {
        "protocolVersion": PROTOCOL_VERSION,
        "capabilities": {},
        "clientInfo": {"name": "gear-wire-probe", "version": "1.0.0"},
    })
    if "error" in resp:
        check("initialize 握手成功", False, json.dumps(resp["error"])[:160])
        return
    result = resp.get("result") or {}
    instr = result.get("instructions") or ""
    check("initialize：返回非空 instructions（R1：客户端能否看到心智模型）",
          len(instr) > 0, "len=%d rune" % len(instr))
    si = result.get("serverInfo") or {}
    check("initialize：serverInfo 报 gear 且带版本（客户端可分辨新旧工具面）",
          si.get("name") == "gear" and bool(si.get("version")),
          "name=%s version=%s" % (si.get("name"), si.get("version")))
    caps = result.get("capabilities") or {}
    check("initialize：声明 tools 能力", "tools" in caps, "capabilities=%s" % sorted(caps.keys()))
    note("initialize：声明的其余能力（resources 是否存在决定客户端能否按需取手册）",
         "resources=%s prompts=%s" % ("resources" in caps, "prompts" in caps))

    sess.notify("notifications/initialized")

    # ---- 2) tools/list：工具面 + 描述体量预算 ----
    resp = sess.request("tools/list", {})
    tools = ((resp.get("result") or {}).get("tools")) or []
    names = [t.get("name", "") for t in tools]
    gear_tools = [n for n in names if n.startswith("gear_")]
    check("tools/list：12 个意图级 gear_ 工具齐备", len(gear_tools) >= 12,
          "共 %d 个：%s" % (len(gear_tools), ",".join(sorted(gear_tools))))
    check("tools/list：名称全部合法（[A-Za-z0-9_.-]，≤128）",
          bool(names) and all(re.fullmatch(r"[A-Za-z0-9_.-]{1,128}", n) for n in names),
          "非法：%s" % [n for n in names if not re.fullmatch(r"[A-Za-z0-9_.-]{1,128}", n)])

    descs = {t.get("name", ""): (t.get("description") or "") for t in tools if t.get("name", "").startswith("gear_")}
    over = sorted([(n, len(d)) for n, d in descs.items() if len(d) > 240])
    total = sum(len(d) for d in descs.values())
    check("描述体量：单条 ≤240 rune（常驻上下文不随工具数膨胀）", not over, "超限：%s" % over)
    check("描述体量：合计 ≤1800 rune", total <= 1800, "合计 %d rune / %d 个工具" % (total, len(descs)))
    check("描述体量：每条描述是完整句（以句号收尾）",
          all(d.strip().endswith(("。", ".")) for d in descs.values()),
          "未收尾：%s" % [n for n, d in descs.items() if not d.strip().endswith(("。", "."))])

    missing_ann = [t.get("name") for t in tools
                   if not ((t.get("annotations") or {}).get("title"))]
    check("tools/list：每个工具都带 annotations.title（客户端展示可读名）", not missing_ann,
          "缺 title：%s" % missing_ann)
    bad_schema = [t.get("name") for t in tools
                  if (t.get("inputSchema") or {}).get("type") != "object"]
    check("tools/list：每个工具 inputSchema 顶层是 object", not bad_schema,
          "非 object：%s" % bad_schema)

    # ---- 3) resources：手册能否按需取 ----
    resp = sess.request("resources/list", {})
    if "error" in resp:
        check("resources/list 可用（客户端可走资源通道取手册）", False,
              json.dumps(resp["error"])[:160])
        guide_uris = []
    else:
        res_list = ((resp.get("result") or {}).get("resources")) or []
        guide_uris = [r.get("uri", "") for r in res_list if r.get("uri", "").startswith("gear://guide/")]
        check("resources/list：gear://guide/* 与 guide 主题一一对应（≥9）",
              len(guide_uris) >= 9, "共 %d 个：%s" % (len(guide_uris), ",".join(sorted(guide_uris))))
    if guide_uris:
        resp = sess.request("resources/read", {"uri": guide_uris[0]})
        ctx = ((resp.get("result") or {}).get("contents")) or []
        text = ctx[0].get("text", "") if ctx else ""
        check("resources/read：返回非空 markdown",
              bool(text) and (ctx[0].get("mimeType") or "").startswith("text/"),
              "%s → %d 字符 mime=%s" % (guide_uris[0], len(text), ctx[0].get("mimeType") if ctx else None))

    # ---- 4) 成功路径：严格判据逐帧判 ----
    calls = [
        ("gear_guide", {"topic": "overview"}),
        ("gear_catalog", {"kind": "all"}),
        ("gear_capabilities", {}),
        ("gear_execution", {}),
        ("gear_doctor", {}),
        ("gear_validate", {"yaml": BAD_BUNDLE}),
    ]
    got = run_calls(sess, calls, seen)
    no_result = [n for n, r in got.items() if not r.get("result")]
    check("成功路径：本组 tools/call 都返回 result（无一被协议层丢掉）", not no_result,
          "缺 result：%s" % no_result)
    if got:
        note("成功帧的具名键（顶层对象形态的实证）",
             "; ".join("%s→%s" % (n, r["keys"]) for n, r in sorted(got.items())))
        note("文本兜底（SDK 是否顺带补 content 文本；设计未承诺，仅记录）",
             "; ".join("%s→%s" % (n, "有" if r["text"] else "无") for n, r in sorted(got.items())))

    # gear_validate 是「编译器」：坏 bundle 必须一次报多条且 valid=false
    v = got.get("gear_validate", {}).get("result", {})
    vsc = v.get("structuredContent") or {}
    diags = vsc.get("diagnostics") or []
    check("gear_validate：坏 bundle 报 multiple 诊断且 valid=false（一次报全、不落库）",
          vsc.get("valid") is False and len(diags) >= 3,
          "valid=%s 诊断 %d 条：%s" % (vsc.get("valid"), len(diags),
                                      [d.get("code") for d in diags if isinstance(d, dict)]))
    check("gear_validate：每条诊断自带 code/message（智能体可自愈）",
          bool(diags) and all(isinstance(d, dict) and d.get("code") and d.get("message") for d in diags))

    # ---- 5) 写路径闭环：apply → run → execution（全走线上帧） ----
    _, result = call("gear_apply", {"bundle": GOOD_BUNDLE, "dry_run": True})
    dry = result.get("structuredContent") or {}
    check("gear_apply(dry_run)：校验通过且一个字节都不写",
          dry.get("valid") is True and not (dry.get("applied") or {}).get("flows"),
          "valid=%s applied=%s" % (dry.get("valid"), dry.get("applied")))

    _, result = call("gear_apply", {"bundle": GOOD_BUNDLE})
    applied = result.get("structuredContent") or {}
    check("gear_apply：真落库（flows 含 wireprobe.flow）",
          applied.get("valid") is True and "wireprobe.flow" in (applied.get("applied") or {}).get("flows", []),
          "applied=%s" % (applied.get("applied"),))

    _, result = call("gear_apply", {"bundle": GOOD_BUNDLE})
    again = result.get("structuredContent") or {}
    check("gear_apply：同 bundle 二次 apply 幂等（不重复创建、不报冲突）",
          again.get("valid") is True, "valid=%s updated=%s" % (again.get("valid"), again.get("updated")))

    _, result = call("gear_run", {"kind": "flow", "id": "wireprobe.flow", "wait_seconds": 15},
                     timeout=40)
    run = result.get("structuredContent") or {}
    check("gear_run(kind=flow)：返回 execution_id 且严格判据通过",
          bool(run.get("execution_id") or run.get("id")),
          "keys=%s status=%s" % (sorted(run.keys()), run.get("status")))

    eid = run.get("execution_id") or run.get("id")
    if eid:
        _, result = call("gear_execution", {"id": eid, "with_logs": True})
        ex = result.get("structuredContent") or {}
        check("gear_execution：按 id 取到该次执行（线上帧闭环）", bool(ex),
              "keys=%s" % sorted(ex.keys()))

    _, result = call("gear_catalog", {"kind": "flow"})
    cat = result.get("structuredContent") or {}
    ids = [i.get("id") for i in (cat.get("items") or []) if isinstance(i, dict)]
    check("gear_catalog：落库后的 Flow 可见（写入→读回闭环）", "wireprobe.flow" in ids,
          "flows=%s" % ids)

    # ---- 6) 错误路径：前置失败必须是工具错误 + [CODE] ----
    resp, result = call("gear_describe", {"kind": "tool", "id": "__no_such_tool__"},
                        expect_error=True)
    text = content_text(result)
    check("错误路径：不存在的对象 → isError=true 且文本以 [CODE] 开头",
          result.get("isError") is True and bool(re.match(r"^\[[A-Z_]+\]", text)),
          "isError=%s text=%r" % (result.get("isError"), text.splitlines()[0] if text else ""))
    check("错误路径：错误响应不带 structuredContent（规范对错误无约定，别让客户端做对象校验）",
          result.get("structuredContent") in (None, {}),
          "structuredContent=%r" % (result.get("structuredContent"),))

    resp, result = call("gear_describe", {}, expect_error=True)
    text = content_text(result)
    rejected = result.get("isError") is True or bool(resp.get("error"))
    check("错误路径：必填入参缺失被挡在 handler 之前（不静默成功）",
          rejected, "isError=%s rpc_error=%s" % (result.get("isError"), bool(resp.get("error"))))
    check("错误路径：SDK 的 schema 拒绝**没有** [CODE] 前缀（如实记录——这条错不能靠码自愈）",
          rejected and not re.match(r"^\[[A-Z_]+\]", text),
          repr(text.splitlines()[0]) if text else "无文本")

    # ---- 7) 累计严格判据：探针发出的**每一次** tools/call 都过一遍 ----
    violated = [(n, strict_client_violations(r)) for n, r in seen if strict_client_violations(r)]
    errs = [n for n, r in seen if r.get("isError") is True]
    check("严格判据（累计 %d 次 tools/call）：成功帧的 structuredContent 顶层都是对象（严格客户端可收下）"
          % len(seen), not violated, "违规 %d 条：%s" % (len(violated), violated[:3]))
    check("累计：成功帧里没有意外的 isError（%d 次刻意构造的错误帧已单独记账）"
          % len(seen_err), not errs, "意外 isError：%s" % errs)

    # ---- 8) 协议卫生：stdio 下 stdout 只能有协议帧 ----
    what = ("协议帧流无噪声（日志未混进帧流）" if sess.mode == "stdio"
            else "stdout 应为空（HTTP 下它不是协议通道，但按口径也不该有任何输出）")
    check("协议卫生（%s）：%s" % (sess.mode, what), not sess.noise,
          "噪声 %d 行：%s" % (len(sess.noise), sess.noise[:2]))
    note("进度日志的落点（证明它们没消失、只是换了通道）",
         "stderr 末行：%s" % (sess.stderr[-1][:90] if sess.stderr else "（空）"))


def main():
    ap = argparse.ArgumentParser(description="Gear MCP 线上帧取证（stdio / streamable HTTP）")
    ap.add_argument("--http", action="store_true", help="用 streamable HTTP transport（默认 stdio）")
    ap.add_argument("--addr", default="", help="--http 时的监听地址（默认 127.0.0.1:<随机空闲端口>）")
    ap.add_argument("--bin", default="", help="gear 可执行文件（默认 build/gear.exe 或 build/gear）")
    ap.add_argument("--dump", default="", help="帧落盘路径（默认 build/mcp_wire_frames.<mode>.jsonl）")
    ap.add_argument("--timeout", type=int, default=15, help="单次请求超时秒数（默认 15）")
    ap.add_argument("--keep", action="store_true", help="保留临时 registry/执行库/台账目录")
    args = ap.parse_args()

    if args.bin:
        bin_path = args.bin
    else:
        cand = [os.path.join(ROOT, "build", "gear.exe"), os.path.join(ROOT, "build", "gear"),
                os.path.join(ROOT, "gear.exe"), os.path.join(ROOT, "gear")]
        bin_path = next((p for p in cand if os.path.exists(p)), cand[0])

    if not os.path.exists(bin_path):
        print("[FAIL] 找不到 gear 可执行文件：%s\n       先 `bash scripts/build.sh`（或 --bin 指定）" % bin_path)
        return 2

    st = os.stat(bin_path)
    # 二进制新鲜度必须显式可见：`build/gear.exe` 陈旧过一次，端到端全红而 `build.sh` 报 PASS。
    note("被探的二进制", "%s  size=%d  mtime=%s" % (
        bin_path, st.st_size, time.strftime("%Y-%m-%d %H:%M:%S", time.localtime(st.st_mtime))))

    tmp = tempfile.mkdtemp(prefix="mcp-probe-")
    reg_path = os.path.join(tmp, "registry.db")
    apps_dir = os.path.join(tmp, "apps")
    os.makedirs(apps_dir, exist_ok=True)
    env = probe_env(reg_path)
    frames = Frames("http" if args.http else "stdio", bin_path, st.st_size,
                    time.strftime("%Y-%m-%dT%H:%M:%S", time.localtime(st.st_mtime)))

    dump_path = args.dump or os.path.join(ROOT, "build",
                                          "mcp_wire_frames.%s.jsonl" % frames.meta["mode"])

    port = 0
    sess = None
    base = [bin_path, "mcp", "--registry", reg_path, "--apps-dir", apps_dir]
    try:
        if args.http:
            if args.addr:
                host, _, port_s = args.addr.rpartition(":")
                addr = args.addr
                port = int(port_s)
            else:
                port = free_port()
                addr = "127.0.0.1:%d" % port
            sess = HttpSession(base + ["--http", addr], env, frames, port, args.timeout)
            print("== gear mcp 线上帧取证：streamable HTTP (http://%s%s) ==\n" % (addr, sess.path))
        else:
            sess = StdioSession(base, env, frames, args.timeout)
            print("== gear mcp 线上帧取证：stdio ==\n")

        probe(sess, frames)
        graceful = sess.close()
        if args.http:
            released = False
            for _ in range(24):
                if not port_listening(port):
                    released = True
                    break
                time.sleep(0.25)
            gone = sess.proc.poll() is not None
            check("收尾：探完即杀 —— 进程已终止且端口已释放（不留常驻监听）",
                  gone and released, "gone=%s released=%s port=%d" % (gone, released, port))
            note("收尾方式",
                 "自行退出" if graceful else
                 "需强杀：`gear mcp --http` 是多会话长驻 server，会话 DELETE 只注销该会话，"
                 "不终止监听（由 SIGTERM/Ctrl-C 结束）—— 与 internal/cli/mcp.go 注释口径一致")
        else:
            check("收尾：stdio 会话随 stdin EOF 自然退出（无强杀）", graceful, "graceful=%s" % graceful)
    except TransportError as e:
        check("transport 可用", False, str(e))
    finally:
        if sess is not None:
            try:
                sess.close()
            except Exception:
                pass
        path = frames.dump(dump_path)
        if args.keep:
            print("\n临时目录（--keep）：%s" % tmp)
        else:
            shutil.rmtree(tmp, ignore_errors=True)

    passed = sum(1 for _, ok, _ in RESULTS if ok)
    failed = [(n, d) for n, ok, d in RESULTS if not ok]
    print("\n" + "=" * 72)
    print("帧落盘：%s（%d 帧）" % (path, len(frames.rows)))
    print("判据：%d/%d 通过" % (passed, len(RESULTS)))
    for n, d in failed:
        print("  FAIL %s  -> %s" % (n, d))
    print("RESULT: %s" % ("PASS" if not failed else "FAIL"))
    print("=" * 72)
    return 0 if not failed else 1


if __name__ == "__main__":
    sys.exit(main())
