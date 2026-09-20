#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
Gear 负向验证仪式（negcheck）。

只跑绿灯证明不了测试有效——断言写反的测试同样 PASS。本脚本把「破坏实现 → 必须变红 →
还原 → 必须变绿」固化成一条命令，并**逐字节校验文件已还原**，杜绝「负向验证跑完文件没还原」。

替代当年丢失在 gitignored build/ 里的同名脚本，迁到入库的 tools/ 目录。

用法：
  python tools/negcheck.py <文件> <旧片段> <新片段> <包> [测试名...]

  <文件>    被改动以注入 bug 的源码（会被改写，结束后逐字节还原）
  <旧片段>  源码里真实存在的一段（破坏前）；以 @ 开头表示从文件读取，便于多行片段
  <新片段>  替换后的「坏实现」（破坏后）；以 @ 开头表示从文件读取；可为空串（删除）
  <包>      go test 的包路径，如 ./internal/cli
  [测试名] 可选，传给 `go test -run`，如 TestScaffoldToolYAMLRejectsBadRuntime；
            多个用 `|` 或空格分隔；省略则跑该包全部测试

退出码：
  0  负向验证通过（破坏→红、还原→绿、且文件逐字节还原）
  1  负向验证失败（破坏后仍绿，或还原后仍红，或文件未还原）
  2  用法错误（旧片段不在文件里等）
"""
import os
import re
import subprocess
import sys

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))


def deref(arg):
    if arg.startswith("@"):
        with open(arg[1:], "r", encoding="utf-8") as f:
            return f.read()
    return arg


def run_go_test(pkg, run_filter):
    env = dict(os.environ)
    env.update({
        "CGO_ENABLED": "1",
        "GOPROXY": "https://goproxy.cn,direct",
        "GOSUMDB": "off",
        "GOFLAGS": "-mod=mod",
    })
    cmd = ["go", "test"]
    if run_filter:
        cmd += ["-run", run_filter]
    cmd += [pkg]
    r = subprocess.run(cmd, cwd=REPO, env=env,
                       stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
    return r.returncode, r.stdout.decode("utf-8", "replace")


def main():
    args = sys.argv[1:]
    if len(args) < 4:
        print(__doc__, file=sys.stderr)
        return 2

    file_arg, old_arg, new_arg, pkg = args[0], args[1], args[2], args[3]
    test_names = args[4:]
    run_filter = "|".join(test_names) if test_names else ""

    path = os.path.join(REPO, file_arg) if not os.path.isabs(file_arg) else file_arg
    if not os.path.isfile(path):
        print(f"[negcheck] 文件不存在: {path}", file=sys.stderr)
        return 2

    old = deref(old_arg)
    new = deref(new_arg)

    original = open(path, "r", encoding="utf-8").read()
    if old not in original:
        print(f"[negcheck] 旧片段不在 {file_arg} 中，无法定位注入点。", file=sys.stderr)
        print("  旧片段预览:", repr(old[:80]), file=sys.stderr)
        return 2

    ok = True
    try:
        # 1) 注入坏实现
        broken = original.replace(old, new, 1)
        open(path, "w", encoding="utf-8").write(broken)
        print(f"[negcheck] 已注入坏实现到 {file_arg}（旧片段 → 新片段）")

        # 2) 破坏后必须变红
        rc, out = run_go_test(pkg, run_filter)
        if rc == 0:
            print("[negcheck] ❌ 负向验证失败：破坏实现后测试仍绿（测试无效）", file=sys.stderr)
            print(out, file=sys.stderr)
            ok = False
        else:
            print("[negcheck] ✅ 破坏实现后测试变红（符合预期）")
    finally:
        # 3) 无论如何先逐字节还原
        open(path, "w", encoding="utf-8").write(original)

    # 4) 校验逐字节还原
    restored = open(path, "r", encoding="utf-8").read()
    if restored != original:
        print("[negcheck] ❌ 文件未逐字节还原，请检查！", file=sys.stderr)
        ok = False
        return 1
    print("[negcheck] ✅ 文件已逐字节还原")

    if not ok:
        return 1

    # 5) 还原后必须变绿
    rc, out = run_go_test(pkg, run_filter)
    if rc != 0:
        print("[negcheck] ❌ 还原后测试仍红（实现或测试有问题）", file=sys.stderr)
        print(out, file=sys.stderr)
        return 1
    print("[negcheck] ✅ 还原后测试变绿")

    print("[negcheck] 全部通过：负向验证有效，且未留下任何改动。")
    return 0


if __name__ == "__main__":
    sys.exit(main())
