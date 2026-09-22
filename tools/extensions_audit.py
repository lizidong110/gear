#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
extensions 配置 ↔ `internal/ext/*.go` 必填参数的一致性审计（tools/extensions_audit.py）。

**它防的是哪类故障**：`internal/ext/wecom.go` 里写着 `if key == "" { return 错误 }`——
运行时确实必填；但 `extensions.yaml` 的 schemas 没给 `required: true`。
后果是智能体照着参数表组装入参时**不知道它必填**，错误从静态阶段推迟到运行期才爆炸。
本项目的纪律是「能静态校验的绝不留到运行期」，所以这条不能只靠人记得同步。

**怎么做到的双向自动提取**（不写死清单，改动代码或配置都能跟上）：
  ① 代码侧：`internal/ext/*.go` 里 `register("<ext>", "<method>", <fn>)` 给出「扩展.方法 ↔ 实现函数」
     的映射，然后在该实现函数体内找 `if <参数> == "" {` 紧跟 `return nil, fmt.Errorf("<参数> 必填…")`。
  ② 配置侧：极简 YAML 子集解析，只取 `ext.X` → 方法 → 标了 `required: true` 的参数名。
  ③ 双向比对：代码必填而配置未标（**FAIL**）／配置标了而代码不验（advisory，不阻断）。

退出码：存在「代码必填、配置未标」→ 1（闸门 FAIL）；否则 0。

用法：
  python tools/extensions_audit.py                 # 扫入库模板 + 本机配置
  python tools/extensions_audit.py --all-build     # 额外扫 build/extensions.yaml 副本
"""
import io
import os
import re
import sys

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))

CONFIGS = [
    os.path.join(REPO, "extensions.example.yaml"),
    os.path.join(REPO, "plugin", "extensions.yaml"),
]
CONFIGS_EXTRA = [os.path.join(REPO, "build", "extensions.yaml")]

EXT_SRC_DIR = os.path.join(REPO, "internal", "ext")

# register("email", "send", emailSend)
RE_REGISTER = re.compile(r'register\(\s*"([a-z]+)"\s*,\s*"([a-z_]+)"\s*,\s*(\w+)\s*\)')
# 必填判定：从错误文案里取参数名（`fmt.Errorf("<参数> 必填…")`），并要求它前面是一个 `if` 块。
# 不写成依赖 `if x == "" {` 这种具体形态——实现里既有 `if to == ""`，也有 `if len(xs) == 0`、
# 还有先取变量再判的形态；指望 caufa单一写法会在 FIRST 次漏抓时就失去守卫价值（advisory 里
# 出现过 to/brokers/kind/phone/payload 五处漏报，正是这个原因）。
RE_REQUIRED = re.compile(
    r'\bif\b[\s\S]{0,220}?return\s+nil,\s*fmt\.Errorf\(\s*"([a-z_]+) 必填'
)


def read(path):
    try:
        return io.open(path, encoding="utf-8").read()
    except FileNotFoundError:
        return None


def collect_code_required():
    """从 internal/ext/*.go 提取 { 'ext.email.send': {'to','host',...} }。"""
    out = {}
    if not os.path.isdir(EXT_SRC_DIR):
        return out
    for name in sorted(os.listdir(EXT_SRC_DIR)):
        if not name.endswith(".go") or name.endswith("_test.go"):
            continue
        src = read(os.path.join(EXT_SRC_DIR, name))
        if src is None:
            continue
        impls = RE_REGISTER.findall(src)
        if not impls:
            continue
        # 按实现函数名切出各函数体（从 `func <name>(` 到下一个顶层 `func `）
        fn_bodies = {}
        starts = [(m.start(), m.group(1)) for m in re.finditer(r"\nfunc\s+(\w+)\(", src)]
        for i, (pos, fname) in enumerate(starts):
            end = starts[i + 1][0] if i + 1 < len(starts) else len(src)
            fn_bodies.setdefault(fname, "")
            fn_bodies[fname] += src[pos:end]
        for ext, method, impl in impls:
            body = fn_bodies.get(impl, "")
            params = sorted(set(RE_REQUIRED.findall(body)))
            if params:
                out["ext.%s.%s" % (ext, method)] = set(params)
    return out


def parse_config_required(path):
    """极简 YAML 子集：取 { 'ext.email.send': {'to','host',...} }（只认 required: true）。"""
    src = read(path)
    if src is None:
        return None
    ext = meth = None
    out = {}
    for line in src.splitlines():
        s = line.strip()
        if s.startswith("#"):
            continue
        ind = len(line) - len(line.lstrip())
        m = re.match(r"^(ext\.[a-z]+):$", s)
        if m and ind == 0:
            ext = m.group(1)
            continue
        m = re.match(r"^([a-z_]+):$", s)
        if m and ind == 4:
            meth = m.group(1)
            continue
        if ind == 8 and ext and meth:
            pm = re.match(r"^([a-z_]+):\s*\{", s)
            if pm and "required: true" in s:
                out.setdefault(ext + "." + meth, set()).add(pm.group(1))
    return out


def audit(config_paths):
    code = collect_code_required()
    if not code:
        print("⚠️ 没能从 %s 提取到任何必填判定——检查 internal/ext/ 是否改了结构" % EXT_SRC_DIR)
        return 0

    print("── extensions 必填一致性审计（%d 份配置）──" % len(config_paths))
    print("代码侧基准：%d 个方法带必填参数" % len(code))
    failed = 0
    for path in config_paths:
        cfg = parse_config_required(path)
        if cfg is None:
            print("\n  ⏭ 跳过（不存在）: %s" % path)
            continue
        rel = os.path.relpath(path, REPO)
        bad, warn = [], []
        for k in sorted(code):
            got = cfg.get(k, set())
            miss = sorted(code[k] - got)
            extra = sorted(got - code[k])
            if miss:
                bad.append("    %-22s 代码必填、配置未标: %s" % (k, ", ".join(miss)))
            if extra:
                warn.append("    %-22s 配置标了必填、代码未验: %s" % (k, ", ".join(extra)))
        if bad:
            failed += len(bad)
            print("\n  ❌ %s" % rel)
            print("\n".join(bad))
        else:
            print("\n  ✅ %s（%d 个方法逐一对齐）" % (rel, len(code)))
        if warn:
            print("    advisory（不阻断）：")
            print("\n".join(warn))
    return failed


def main():
    paths = list(CONFIGS)
    if "--all-build" in sys.argv:
        paths += CONFIGS_EXTRA
    failed = audit(paths)
    print()
    if failed:
        print("❌ extensions_audit：%d 处必填未在配置里声明（智能体据此组装入参会失败在运行期）" % failed)
        return 1
    print("✅ extensions_audit：配置↔代码的必填声明一致")
    return 0


if __name__ == "__main__":
    sys.exit(main())
