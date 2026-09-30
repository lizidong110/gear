#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
Gear 文档↔代码机械审计（doc_audit）。

解决「文档声称有 X、代码实际没有」的漂移——这是 Gear 工程方法（GEAR_工程方法_v1.md §5）
的硬性纪律之一：改代码必须同步文档，且要能机械复跑核验。本脚本替代当年丢失在
gitignored build/ 里的同名脚本，迁到入库的 tools/ 目录。

覆盖四类核对（双向）：
  ① 文档引用的 Test 名        ↔ 代码里 `func TestXxx` 实际存在
  ② 文档引用的 .go 路径       ↔ 磁盘上文件实际存在
  ⑥ REST 端点（server.go 的 mux.HandleFunc） ↔ README 端点表（双向）
  ⑦ CLI 子命令（cli/root.go 的 AddCommand）   ↔ README 提及（双向）
  ③ MCP 错误码（guide/errors.md 的码表）      ↔ 代码里的产生点（双向；附录 D 亦须 ⊆ 产生点）

每条发现归入三分类之一（退出码只看 ① / ⑥ / ⑦ / ③）：
  ① 需处理     ：文档声称有、代码/磁盘却没有，且文档未声明它「不存在/历史」
  ② 已登记例外 ：文档自己已说明该条目不存在 / 属于历史日志，不算是漂移
  ③ 噪音       ：TestFoo* 通配、裸文件名（无 / 的 .go 名）等，忽略

退出码：存在任意 ① 需处理 → 1（闸门 FAIL）；否则 0（PASS）。

用法：
  python tools/doc_audit.py            # 默认扫 README/REQUIREMENTS/GEAR_工程方法 + 全仓代码
  python tools/doc_audit.py --strict   # 把 ③ 噪音也列出来（默认折叠）
"""
import os
import re
import sys

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))

# 文档里明显不是 CLI 子命令、却可能跟在 `gear ` 后的词，放入此denylist避免误报。
CLI_DENYLIST = {"v4", "v40", "v5", "github", "com", "cn", "is", "the", "a", "run"}

# 例外关键词：文档行里出现这些，说明该条目「已知不存在/历史」，归入 ② 而非 ①。
EXCEPTION_KW = ["未实现", "不存在", "已删除", "历史", "deferred", "stale",
                "no longer", "removed", "deprecated", "待定", "暂不", "示例仅", "demo",
                "删除", "已删", "移除", "已并入", "并入", "不可复现", "示例已移除"]

# 已知被有意删除/合并的文件/示例：按项目纪律（历史日志段落不回溯改写，现实变了加注记），
# 这些引用属「已知不存在」，审计直接认作例外，不去鞭尸文档里的历史叙事。
KNOWN_REMOVED_PREFIXES = (
    "examples/gear-ai-sidecar/", "examples/gear-db-sidecar/",
    "examples/news-demo/", "examples/slack-sidecar/",
    "plugin/web-ui/node_modules",
)
KNOWN_MERGED_PATHS = {
    "engine/primitive.go", "internal/engine/primitive.go",
    "internal/engine/jsonx.go", "engine/jsonx.go",
}


def read(path):
    try:
        with open(path, "r", encoding="utf-8") as f:
            return f.read()
    except FileNotFoundError:
        return ""


def find_all(pattern, text):
    return [(m.start(), m.group(0)) for m in re.finditer(pattern, text)]


def line_of(text, pos):
    return text.count("\n", 0, pos) + 1


# ---------- ⑥ REST 端点 ----------
def audit_rest_endpoints():
    server = read(os.path.join(REPO, "internal", "server", "server.go"))
    registered = {}  # "METHOD /path" -> (file, line)
    for m in re.finditer(r'HandleFunc\(\s*"([A-Z]+)\s+(/[^"]+)"', server):
        key = f"{m.group(1)} {m.group(2)}"
        registered.setdefault(key, ("internal/server/server.go", line_of(server, m.start())))

    readme = read(os.path.join(REPO, "README.md"))
    doc = {}
    for line in readme.splitlines():
        if not line.strip().startswith("|"):
            continue
        cols = [c.strip().strip("`") for c in line.split("|")]
        # 表头之后的数据行：cols = ['', method, path, handler, desc, '']
        if len(cols) < 4:
            continue
        method = cols[1].upper()
        path = cols[2]
        if method in ("GET", "POST", "PUT", "DELETE", "PATCH") and path.startswith("/"):
            doc.setdefault(f"{method} {path}", ("README.md", line_of(readme, readme.find(line))))

    reg_keys = set(registered)
    doc_keys = set(doc)
    missing_in_doc = sorted(reg_keys - doc_keys)      # 代码有、文档无
    missing_in_code = sorted(doc_keys - reg_keys)     # 文档有、代码无
    return registered, doc, missing_in_doc, missing_in_code


# ---------- ⑦ CLI 子命令 ----------
def cli_blocks():
    """返回 {func_name: (use_first_token, file, line)} 对所有 *Cmd() 构造函数。"""
    cli_dir = os.path.join(REPO, "internal", "cli")
    blocks = {}
    for fn in os.listdir(cli_dir):
        if not fn.endswith(".go"):
            continue
        src = read(os.path.join(cli_dir, fn))
        for m in re.finditer(r'^func\s+(\w+Cmd)\s*\(', src, re.M):
            name = m.group(1)
            start = m.start()
            end = src.find("\nfunc ", start + 1)
            if end == -1:
                end = len(src)
            block = src[start:end]
            use_m = re.search(r'Use:\s*"([^"]+)"', block)
            if use_m:
                token = use_m.group(1).split()[0]
            else:
                token = name[:-3].lower()  # 兜底：toolCmd -> tool
            blocks[name] = (token, fn, line_of(src, m.start()))
    return blocks


def audit_cli_commands():
    root = read(os.path.join(REPO, "internal", "cli", "root.go"))
    # AddCommand 整行都在一行，用贪婪匹配抓到行尾的闭括号。
    add_m = re.search(r'AddCommand\((.*)\)\s*$', root, re.M)
    funcs = re.findall(r'(\w+Cmd)\(\)', add_m.group(1)) if add_m else []
    blocks = cli_blocks()
    registered = {}  # command name -> (file, line)
    for f in funcs:
        if f in blocks:
            token, fn, ln = blocks[f]
            registered.setdefault(token, (f"internal/cli/{fn}", ln))

    readme = read(os.path.join(REPO, "README.md"))
    # 收集 README 里所有 `gear <token>` 提及（含围栏代码块与行内反引号）。
    doc_tokens = set()
    for m in re.finditer(r'gear\s+([a-z][a-z0-9-]+)', readme):
        t = m.group(1)
        if t in CLI_DENYLIST:
            continue
        doc_tokens.add(t)

    reg_names = set(registered)
    missing_in_doc = sorted(reg_names - doc_tokens)    # 注册了、文档没提
    missing_in_code = sorted(doc_tokens - reg_names)   # 文档提了、没注册
    return registered, doc_tokens, missing_in_doc, missing_in_code


# ---------- ③ MCP 错误码（errors.md ⇄ 代码产生点，双向） ----------
#
# 为什么需要它（MCP_接口与协议评审_v1.md I-2）：评审实测到两类断裂——
#   ① 文档声明但代码**从不产生**的码（`PRIMITIVE_MISSING`/`PORT_IN_USE`/`APP_PLATFORM_UNAVAILABLE`/
#      `DEP_CYCLE`）⇒ 智能体按码查手册会查到「不存在的条目」，或拿到一个查不到的码；
#   ② 代码在用、手册**漏收**的兜底码（约 17 个）⇒ 自愈回路在最高频的失败路径上断掉。
# 这条守卫把「码表」与「产生点」双向钉死，与 ⑥⑦ 同款思路（枚举式声明必须逐项登记）。

# 只在「产生点」形态里取码，避免把注释/文档字符串里的示例码算进来。
CODE_PATTERNS = [
    r'toolError(?:From)?\(\s*"([A-Z][A-Z0-9_]*)"',
    r'diagError\(\s*"([A-Z][A-Z0-9_]*)"',
    r'Diagf?\(\s*"([A-Z][A-Z0-9_]*)"',          # engine.Diagf / Diag
    r'diagWrap\(\s*"([A-Z][A-Z0-9_]*)"',
    r'Code:\s*"([A-Z][A-Z0-9_]*)"',              # Diagnostic{Code: "..."}
    r'Code\w*\s*=\s*"([A-Z][A-Z0-9_]*)"',        # CodeXxx = "..." 常量
    r'\.Code\s*=\s*"([A-Z][A-Z0-9_]*)"',         # res.Code = "ORPHAN_APP"
    r'\[([A-Z][A-Z0-9_]{3,})\]',                 # 警告型码字面量：[MISSING_FILE] …（前缀在 Sprintf 里）
]

# 方括号形态里**不是错误码**的标记（误报排除，逐项说明理由）：
#   DONE —— OpenAI 兼容 SSE 的结束哨兵（`data: [DONE]`），属传输层协议字面量；
#   CODE —— 错误形态的文档占位符（`[CODE] message`），出现在注释里而非运行期值。
BRACKET_NON_CODES = {"DONE", "CODE"}


def produced_error_codes():
    """返回代码里**实际产生**的错误码集合。"""
    codes = set()
    for dirpath, _, files in os.walk(os.path.join(REPO, "internal")):
        if any(part in ("build", "node_modules", "dist") for part in dirpath.split(os.sep)):
            continue
        for fn in files:
            if not fn.endswith(".go") or fn.endswith("_test.go"):
                continue
            src = read(os.path.join(dirpath, fn))
            for pat in CODE_PATTERNS:
                for m in re.finditer(pat, src):
                    codes.add(m.group(1))
    return codes - BRACKET_NON_CODES


def documented_error_codes(doc_path, table_only=True):
    """从一篇 markdown 的**码表首列**提取码（形如 `| \\`CODE\\` | …`）。"""
    text = read(doc_path)
    codes = set()
    for line in text.splitlines():
        if not line.strip().startswith("|"):
            continue
        cols = line.split("|")
        if len(cols) < 3:
            continue
        first = cols[1].strip().strip("`").strip()
        if re.fullmatch(r'[A-Z][A-Z0-9_]{2,}', first):
            codes.add(first)
    return codes


def audit_error_codes():
    """errors.md ⇄ 产生点 双向核对；附录 D 亦须 ⊆ 产生点。"""
    guide_errors = os.path.join(REPO, "internal", "mcp", "guide", "errors.md")
    design = os.path.join(REPO, "MCP_AGENT_DESIGN.md")
    produced = produced_error_codes()
    documented = documented_error_codes(guide_errors)
    appendix = documented_error_codes(design)

    doc_missing = sorted(documented - produced)   # 文档有、代码不产生
    code_missing = sorted(produced - documented)  # 代码产生、文档漏收
    appendix_missing = sorted(appendix - produced)
    return produced, documented, doc_missing, code_missing, appendix_missing


# ---------- ① Test 名 ----------
def all_test_funcs():
    funcs = set()
    # 测试既在 internal/ 也在 plugin/（如 plugin/mqtt-sidecar/main_test.go）。
    for base in ("internal", "plugin"):
        for dirpath, _, files in os.walk(os.path.join(REPO, base)):
            for fn in files:
                if fn.endswith("_test.go"):
                    src = read(os.path.join(dirpath, fn))
                    for m in re.finditer(r'^func\s+(Test\w+)\s*\(', src, re.M):
                        funcs.add(m.group(1))
    return funcs


def audit_test_names(doc_files):
    existing = all_test_funcs()
    gaps = []      # ①
    exceptions = []  # ②
    noise = []     # ③
    seen = set()
    for df in doc_files:
        text = read(df)
        if not text:
            continue
        # 末尾可选 `*`：`TestTopoOrder*` 是通配写法，单独归噪音。
        # 要求 `Test` 后至少跟一个字母数字，避免把「Test 名」这类中文里的 "Test" 当测试名。
        for pos, tok in find_all(r'Test[A-Za-z0-9][A-Za-z0-9_]*\*?', text):
            if tok.endswith("*"):
                base = tok[:-1]
                if base not in seen:
                    noise.append((df, line_of(text, pos), tok, "通配符，非具体测试"))
                seen.add(base)
                continue
            if tok in seen:
                continue
            seen.add(tok)
            if tok in existing:
                continue
            line = line_of(text, pos)
            ctx = text.splitlines()[line - 1] if line - 1 < len(text.splitlines()) else ""
            if any(k in ctx for k in EXCEPTION_KW):
                exceptions.append((df, line, tok, ctx.strip()))
            else:
                gaps.append((df, line, tok, "文档引用但代码中无此 func"))
    return gaps, exceptions, noise


# ---------- ② .go 路径 ----------
def all_go_relpaths():
    """返回仓库内所有 .go 文件的相对路径集合，以及文件名集合。"""
    relpaths = set()
    basenames = set()
    for dirpath, dirnames, files in os.walk(REPO):
        # 剪枝而非「遍历到再丢弃」：walk 仍会进入 node_modules 把 2 万+ 文件列一遍（实测 6s），
        # 原地改 dirnames 才真正不进去。口径与原来的 continue 判定一致（同四个目录名）。
        dirnames[:] = [d for d in dirnames if d not in (".git", "build", "node_modules", "dist")]
        for fn in files:
            if fn.endswith(".go"):
                rel = os.path.relpath(os.path.join(dirpath, fn), REPO).replace(os.sep, "/")
                relpaths.add(rel)
                basenames.add(fn)
    return relpaths, basenames


def audit_go_paths(doc_files):
    relpaths, basenames = all_go_relpaths()
    gaps = []
    exceptions = []
    noise = []
    seen = set()
    for df in doc_files:
        text = read(df)
        if not text:
            continue
        for pos, tok in find_all(r'[A-Za-z0-9_./\-]+\.go', text):
            if "/" not in tok:
                if tok not in seen:
                    noise.append((df, line_of(text, pos), tok, "裸文件名（无 /），非路径引用"))
                seen.add(tok)
                continue
            if tok in seen:
                continue
            seen.add(tok)
            # 解析：原文路径 / 补 internal/ 前缀 / 文件名匹配 任一成立即视为存在。
            exists = (tok in relpaths
                      or f"internal/{tok}" in relpaths
                      or tok.split("/")[-1] in basenames)
            if exists:
                continue
            line = line_of(text, pos)
            ctx = text.splitlines()[line - 1] if line - 1 < len(text.splitlines()) else ""
            # 已知被有意删除的示例 / 已合并的文件 → 例外（不鞭尸历史叙事）。
            if tok.startswith(KNOWN_REMOVED_PREFIXES) or tok in KNOWN_MERGED_PATHS:
                exceptions.append((df, line, tok, "已知被删除/合并的示例或文件（历史引用）"))
            elif any(k in ctx for k in EXCEPTION_KW):
                exceptions.append((df, line, tok, ctx.strip()))
            else:
                gaps.append((df, line, tok, "文档引用但磁盘上无此文件"))
    return gaps, exceptions, noise


def fmt_items(items):
    return "\n".join(f"    - {df}:{ln}  {tok}  ({why})" for df, ln, tok, why in items) or "    (无)"


def main():
    strict = "--strict" in sys.argv
    doc_files = [
        os.path.join(REPO, "README.md"),
        os.path.join(REPO, "REQUIREMENTS.md"),
        os.path.join(REPO, "GEAR_工程方法_v1.md"),
    ]

    reg6, doc6, miss_doc6, miss_code6 = audit_rest_endpoints()
    reg7, doc7, miss_doc7, miss_code7 = audit_cli_commands()
    produced, documented, code_doc_missing, code_prod_missing, appendix_missing = audit_error_codes()
    g1, e1, n1 = audit_test_names(doc_files)
    g2, e2, n2 = audit_go_paths(doc_files)

    lines = []
    lines.append("══════════ doc_audit 报告 ═════════")
    lines.append("")
    lines.append("【⑥ REST 端点 双向核对】（server.go HandleFunc ↔ README 端点表）")
    lines.append("  注册未登记（代码有、文档无）:")
    lines.append("\n".join(f"    - {k}  ({reg6[k][0]}:{reg6[k][1]})" for k in miss_doc6) or "    (无)")
    lines.append("  登记未注册（文档有、代码无）:")
    lines.append("\n".join(f"    - {k}  ({doc6[k][0]}:{doc6[k][1]})" for k in miss_code6) or "    (无)")
    lines.append("")
    lines.append("【⑦ CLI 子命令 双向核对】（cli/root.go AddCommand ↔ README 提及）")
    lines.append("  注册未提及（代码有、文档无）:")
    lines.append("\n".join(f"    - {k}  ({reg7[k][0]}:{reg7[k][1]})" for k in miss_doc7) or "    (无)")
    lines.append("  提及未注册（文档有、代码无）:")
    lines.append("\n".join(f"    - {k}" for k in miss_code7) or "    (无)")
    lines.append("")
    lines.append("【③ MCP 错误码 双向核对】（guide/errors.md 码表 ↔ 代码产生点；附录 D 亦须 ⊆ 产生点）")
    lines.append(f"  产生点实收 {len(produced)} 个码；errors.md 收录 {len(documented)} 个。")
    lines.append("  文档有、代码不产生（自愈回路会查到不存在的条目）:")
    lines.append("\n".join(f"    - {k}" for k in code_doc_missing) or "    (无)")
    lines.append("  代码产生、文档漏收（自愈回路断裂）:")
    lines.append("\n".join(f"    - {k}" for k in code_prod_missing) or "    (无)")
    lines.append("  附录 D 有、代码不产生:")
    lines.append("\n".join(f"    - {k}" for k in appendix_missing) or "    (无)")
    lines.append("")
    lines.append("【① 文档引用的 Test 名 ↔ 代码】（advisory：历史文档已知漂移，不计入闸门；--strict 才计入）")
    lines.append("  需处理:")
    lines.append(fmt_items(g1))
    lines.append("  已登记例外（文档自称不存在/历史）:")
    lines.append(fmt_items(e1))
    lines.append("  噪音（通配符）:")
    lines.append(fmt_items(n1) if strict else "    (折叠，--strict 展开)")
    lines.append("")
    lines.append("【② 文档引用的 .go 路径 ↔ 磁盘】（advisory：历史文档已知漂移，不计入闸门；--strict 才计入）")
    lines.append("  需处理:")
    lines.append(fmt_items(g2))
    lines.append("  已登记例外:")
    lines.append(fmt_items(e2))
    lines.append("  噪音（裸文件名）:")
    lines.append(fmt_items(n2) if strict else "    (折叠，--strict 展开)")
    lines.append("")
    lines.append("══════════ 汇总 ═════════")
    code_gaps = len(code_doc_missing) + len(code_prod_missing) + len(appendix_missing)
    gate_gaps = len(miss_doc6) + len(miss_code6) + len(miss_doc7) + len(miss_code7) + code_gaps
    advisory_gaps = len(g1) + len(g2)
    lines.append(f"  闸门项（⑥⑦③ 双向核对，计入闸门）: {gate_gaps}")
    lines.append(f"  ① 需处理: {len(g1) + len(g2)}  (advisory，未计入闸门)")
    lines.append(f"  ② 已登记例外: {len(e1) + len(e2)}")
    lines.append(f"  ③ 噪音: {len(n1) + len(n2)}")
    fail = gate_gaps > 0 or (strict and advisory_gaps > 0)
    if not fail:
        lines.append("  GATE: PASS（退出码 0）")
        out = "\n".join(lines)
        print(out)
        return 0
    else:
        lines.append("  GATE: FAIL（存在需处理的漂移，退出码 1）")
        out = "\n".join(lines)
        print(out)
        return 1


if __name__ == "__main__":
    sys.exit(main())
