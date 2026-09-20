#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
Gear 覆盖率缺口报告（cov_report）。

`go tool cover -func` 只给「每函数覆盖率百分比」，看不出大函数里**哪段分支没跑**。
本脚本解析 go test 的 cover profile，按「未覆盖语句数」倒序，定位最该补测试的
文件 / 函数；带 `lines` 参数时直接打印未覆盖块的源码行，便于精准补测。

替代当年丢失在 gitignored build/ 里的同名脚本，迁到入库的 tools/ 目录。

用法：
  go test -coverprofile=cover.out ./internal/engine/
  python tools/cov_report.py cover.out            # 按未覆盖语句数倒序，默认前 20
  python tools/cov_report.py cover.out 30         # 取前 30
  python tools/cov_report.py cover.out 20 lines   # 同时打印未覆盖源码行
"""
import os
import re
import sys

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))


def parse_profile(path):
    blocks = []  # (file, sl, sc, el, ec, num_stmt, count)
    with open(path, "r", encoding="utf-8") as f:
        for line in f:
            line = line.rstrip("\n")
            if line.startswith("mode:") or line.startswith("coverfile:") or not line.strip():
                continue
            m = re.match(r'(.+?):(\d+)\.(\d+),(\d+)\.(\d+) (\d+) (\d+)$', line)
            if not m:
                continue
            fn, sl, sc, el, ec, num, cnt = m.groups()
            blocks.append((fn, int(sl), int(sc), int(el), int(ec), int(num), int(cnt)))
    return blocks


def resolve_repo_path(fn):
    """cover profile 的路径是 module 前缀（如 gear/internal/cli/ai.go），
    去掉首段（module 名）即仓库相对路径；找不到时退化为原值（会 fail-open）。"""
    p = os.path.join(REPO, fn)
    if os.path.isfile(p):
        return p
    parts = fn.split("/")
    if len(parts) > 1:
        p2 = os.path.join(REPO, *parts[1:])
        if os.path.isfile(p2):
            return p2
    return p


def parse_functions(src_path):
    """返回 [(start_line, func_name), ...] 按行号升序；用于把覆盖块归到函数。"""
    try:
        with open(src_path, "r", encoding="utf-8") as f:
            lines = f.readlines()
    except FileNotFoundError:
        return []
    funcs = []
    pat = re.compile(r'^\s*func\s+(?:\([^)]*\)\s+)?(\w+)')
    for i, ln in enumerate(lines, 1):
        m = pat.match(ln)
        if m:
            funcs.append((i, m.group(1)))
    funcs.sort()
    return funcs


def func_of(funcs, line):
    name = "<global>"
    for start, fname in funcs:
        if start <= line:
            name = fname
        else:
            break
    return name


def main():
    args = sys.argv[1:]
    if not args:
        print("用法: python tools/cov_report.py <coverprofile> [topN] [lines]", file=sys.stderr)
        return 2
    profile = args[0]
    top_n = 20
    show_lines = False
    for a in args[1:]:
        if a.lower() == "lines":
            show_lines = True
        else:
            try:
                top_n = int(a)
            except ValueError:
                pass

    blocks = parse_profile(profile)
    # 按 (file, func) 聚合未覆盖语句数
    agg = {}  # (file, func) -> uncovered_stmt
    uncovered_blocks = []  # (file, func, sl, el) 用于 lines 打印
    func_cache = {}
    for fn, sl, sc, el, ec, num, cnt in blocks:
        if cnt == 0 and num > 0:
            rpath = resolve_repo_path(fn)
            if rpath not in func_cache:
                func_cache[rpath] = parse_functions(rpath)
            funcs = func_cache[rpath]
            fname = func_of(funcs, sl)
            key = (fn, fname)
            agg[key] = agg.get(key, 0) + num
            uncovered_blocks.append((fn, fname, sl, el))

    ranked = sorted(agg.items(), key=lambda kv: kv[1], reverse=True)
    print(f"═══ 覆盖率缺口（按未覆盖语句数倒序，共 {len(ranked)} 个函数有缺口）═══")
    print(f"{'未覆盖语句':>10}  {'文件':<45} 函数")
    for (fn, fname), n in ranked[:top_n]:
        short = fn
        if len(short) > 45:
            short = "..." + short[-42:]
        print(f"{n:>10}  {short:<45} {fname}")

    if show_lines:
        print("\n═══ 未覆盖源码行 ═══")
        # 按文件分组，逐块打印
        by_file = {}
        for fn, fname, sl, el in uncovered_blocks:
            by_file.setdefault(fn, []).append((sl, el, fname))
        shown = 0
        limit = max(top_n * 5, 40)
        for fn in sorted(by_file):
            if shown >= limit:
                break
            path = resolve_repo_path(fn)
            try:
                src = open(path, "r", encoding="utf-8").read().splitlines()
            except FileNotFoundError:
                continue
            printed_header = False
            for sl, el, fname in sorted(by_file[fn]):
                if shown >= limit:
                    break
                if not printed_header:
                    print(f"\n# {fn}")
                    printed_header = True
                for ln in range(sl, el + 1):
                    if ln - 1 < len(src):
                        print(f"  L{ln} [{fname}] {src[ln-1]}")
                        shown += 1
    total_uncovered = sum(agg.values())
    print(f"\n未覆盖语句总数: {total_uncovered}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
