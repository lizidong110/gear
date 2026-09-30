#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""可移植性审计（Gear 质量闸门第⑨项）。

检测「应用定义里硬编码的机器相关绝对路径」——这是 2026-09-25 实测的
可移植性根因：apps/mindstorm 里 25 处 `D:/gadern/gear/...`（db 的 DSN、
process entry 的 gear 可执行文件、static_site.serve 的 dir），仓库拷到别的
机器即跑不起来。

内核已提供可移植锚点，应用**不该**再写绝对路径：
  - db 的 dsn 写相对库名（如 "mindstorm.db"）→ 锚到 GEAR_APP_DATA（默认 <Home>/data）；
  - process entry 写裸名（如 "gear serve ..."）→ 锚到 gear 可执行文件所在目录；
  - ext.serve 的 dir 写相对安装根（如 "apps/mindstorm/www"）→ 锚到 InstallRoot。

扫描范围：apps/ 与 examples/ 下所有 *.yaml（应用/示例定义，会被部署或复制）。
命中即退出码 1，把每个违规点（文件:行号 + 原文）打到 stdout，便于定位。

判定规则（避免误伤 /api、/proxy 这类配置路由）：
  1. Windows 盘符绝对路径： `[A-Za-z]:[\\/]`（如 D:/、C:\）——绝不会是配置路由；
  2. 类 Unix 机器根路径：首段为 d/c/e（Git-Bash/MSYS 的盘挂载）、Users、home、
     mnt、Volumes、Applications、tmp、var、opt、srv、root（如 /d/gadern、/Users/920）。
     可出现在行内（如 `dir: /Users/...`），但排除 http:// 等协议串。
"""

from __future__ import annotations

import os
import re
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))

# 扫描目录（相对仓库根）
SCAN_DIRS = ["apps", "examples"]

# 规则 1：Windows 盘符绝对路径（单字母盘符 + 路径分隔符）。
# 用负向回溯 (?<![A-Za-z]) 排除 http://、https:// 这类协议串里「末字母:斜杠」的假阳性。
DRIVE_RE = re.compile(r"(?<![A-Za-z])[A-Za-z]:[\\/]")

# 规则 2：类 Unix 机器根路径。匹配首段属于已知机器根的词，可出现在行内（如 `dir: /Users/...`、
# `local DSN = "/d/..."`），但排除 `://` 与 `//`（协议串 / 双斜杠）里的 /x。
UNIX_ROOT_SEGMENTS = (
    "d", "c", "e",            # Git-Bash/MSYS 把 C:/ D:/ 挂成 /c /d /e
    "Users", "home",          # 用户主目录（含 /Users/920、/home/foo）
    "mnt", "Volumes", "Applications", "tmp", "var", "opt", "srv", "root",
)
UNIX_RE = re.compile(r"/(?:%s)\b" % "|".join(UNIX_ROOT_SEGMENTS))


def is_machine_path(text: str) -> bool:
    if DRIVE_RE.search(text):
        return True
    # 类 Unix 机器根：允许行内出现，但排除协议串（http:// / file://）与双斜杠里的 /x。
    for m in UNIX_RE.finditer(text):
        start = m.start()
        if start > 0 and text[start - 1] in (":", "/"):
            continue  # 属于协议串或双斜杠，不是机器根路径
        return True
    return False


def iter_yaml_files() -> list[str]:
    files: list[str] = []
    for d in SCAN_DIRS:
        base = os.path.join(ROOT, d)
        if not os.path.isdir(base):
            continue
        for dirpath, _dirs, names in os.walk(base):
            for n in names:
                if n.endswith(".yaml") or n.endswith(".yml"):
                    files.append(os.path.join(dirpath, n))
    return sorted(files)


def main() -> int:
    hits = 0
    files = iter_yaml_files()
    if not files:
        print("（未找到待扫描的 yaml，跳过）")
        return 0
    for path in files:
        rel = os.path.relpath(path, ROOT)
        try:
            with open(path, "r", encoding="utf-8") as f:
                lines = f.readlines()
        except (OSError, UnicodeDecodeError) as e:
            print(f"  ⚠️  读取失败 {rel}: {e}")
            continue
        for i, line in enumerate(lines, 1):
            if is_machine_path(line):
                hits += 1
                print(f"  ❌ {rel}:{i}: {line.rstrip()}")
    if hits:
        print(f"\n发现 {hits} 处机器相关绝对路径——应用定义必须可移植，请改用相对锚点：")
        print("    · db dsn 用相对库名（mindstorm.db）→ GEAR_APP_DATA")
        print("    · process entry 用裸名（gear serve ...）→ 可执行文件所在目录")
        print("    · ext.serve dir 用相对安装根（apps/<app>/www）→ InstallRoot")
        return 1
    print("✅ 应用定义无可移植性风险（无机器相关绝对路径）")
    return 0


if __name__ == "__main__":
    sys.exit(main())
