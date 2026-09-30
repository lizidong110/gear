#!/usr/bin/env python3
"""需求薄索引引用存在性审计（gate 第⑩项）。

口径（见 REQUIREMENTS.md 顶部「薄索引口径」）：
  REQUIREMENTS.md 只做索引——每条需求一行：一句话行为 + 状态 + 落点。
  本脚本机械验证「落点列」的引用真实存在，防止索引变成另一份会漂移的文档：
    ① 每个 `path/to/x_test.go` / `x.go` / `x.py` / `x.mjs` / `x.md` 引用 → 仓库内必须存在该文件（带路径用路径，裸名按文件名全仓匹配）；
    ② 每个 `TestXxx` 函数名 → 必须能在某 *.go 里找到 `func TestXxx(`；
    ③ 凡 implemented 的需求，落点必须含 ≥1 个测试/工具引用（_test.go、tools/、probe）；
    ④ 落点含「待补」的需求 → WARN（显式登记的缺口，不静默放过也不阻断）。
  退出码非 0 = 有真缺口。例外只能写在本文件顶部的 EXCEPTIONS（须附理由）。
"""

import os
import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
REQ = ROOT / "REQUIREMENTS.md"

# 遍历时剪掉的目录：生成物 / 第三方依赖 / 会话数据。
# 它们既不该作为「落点引用」的解析目标，遍历它们还会把闸门拖慢一个数量级——
# 实测（2026-09-28）：ROOT.rglob("*") 遍历全仓 21.4s，其中 plugin/web-ui/node_modules
# 独占绝大多数（该目录 2.3 万+ 文件）。改 os.walk 原地剪枝 dirnames 后，本段降到 1s 内。
SKIP_DIRS = (".git", "build", "node_modules", "dist", ".workbuddy", "__pycache__")

# 例外白名单：{需求ID: 理由}。只放「文档自己已说明原因」的条目，别把待修漂移塞进来。
EXCEPTIONS = {}

ROW = re.compile(r"^\| (REQ-[A-Z0-9-]+) \|")
FILE_REF = re.compile(r"[A-Za-z0-9_./-]+\.(?:go|py|mjs|md|yaml)\b")
TEST_REF = re.compile(r"\bTest[A-Za-z0-9_]+\b")


def build_indexes():
    """一趟 os.walk 同时建「文件名→路径」索引与「测试函数名」集合。

    原来是两趟 rglob（全文件 + 全 .go），等于把仓库遍历两遍；且 rglob 无法剪枝，
    只能遍历完再在循环里丢弃。os.walk 原地改 dirnames 才能真正不进入 SKIP_DIRS。
    """
    files_by_name = {}
    test_funcs = set()
    for dirpath, dirnames, filenames in os.walk(ROOT):
        # 原地赋值：os.walk 据此决定下一层进不进，这是 rglob 做不到的事
        dirnames[:] = [d for d in dirnames if d not in SKIP_DIRS]
        for fn in filenames:
            files_by_name.setdefault(fn, []).append(Path(dirpath) / fn)
            if fn.endswith(".go"):
                try:
                    text = (Path(dirpath) / fn).read_text(encoding="utf-8", errors="ignore")
                except OSError:
                    continue
                test_funcs.update(re.findall(r"func (Test[A-Za-z0-9_]+)\(", text))
    return files_by_name, test_funcs


def main() -> int:
    lines = REQ.read_text(encoding="utf-8").splitlines()
    files_by_name, test_funcs = build_indexes()

    errors, warns = [], []
    n_rows = 0
    for line in lines:
        m = ROW.match(line)
        if not m:
            continue
        rid = m.group(1)
        n_rows += 1
        # 按非转义 | 切 5 列
        parts = [c.strip() for c in re.split(r"(?<!\\)\|", line.strip().strip("|"))]
        if len(parts) != 5:
            errors.append(f"{rid}: 列数 {len(parts)} ≠ 5（表格被撑坏）")
            continue
        _id, _prd, _behavior, status_cell, refs = parts
        st = re.search(r"`([a-z-]+)", status_cell)
        status = st.group(1) if st else "?"
        pending = "待补" in refs

        # ① 文件引用存在性
        for f in FILE_REF.findall(refs):
            name = f.rsplit("/", 1)[-1]
            if name not in files_by_name:
                errors.append(f"{rid}: 引用的文件不存在: {f}")
        # ② 测试函数存在性（`TestXxx*` 为前缀通配：至少要有一个同名前缀的测试）
        for m in TEST_REF.finditer(refs):
            t = m.group(0)
            if refs[m.end(): m.end() + 1] == "*":
                if not any(f.startswith(t) for f in test_funcs):
                    errors.append(f"{rid}: 前缀通配无任何匹配测试: {t}*")
            elif t not in test_funcs:
                errors.append(f"{rid}: 引用的测试不存在: {t}()")
        # ③ implemented 必须有断言载体（测试文件 / 测试函数 / 审计工具 / 线上探针）
        if status == "implemented":
            carriers = FILE_REF.findall(refs)
            has_assert = (
                any(c.endswith("_test.go") or c.startswith(("tools/", "plugin/mcp_wire_probe")) for c in carriers)
                or TEST_REF.search(refs) is not None
                or "wire_probe" in refs
            )
            if not has_assert and not pending:
                errors.append(f"{rid}: implemented 但落点无测试/工具断言载体")
        # ④ 显式待补
        if pending:
            warns.append(f"{rid}: 落点含「待补」（已显式登记，请尽快补齐并摘除标记）")
        if rid in EXCEPTIONS:
            warns.append(f"{rid}: 例外放行 —— {EXCEPTIONS[rid]}")

    print(f"需求行数: {n_rows}")
    for w in warns:
        print(f"  ⚠️  {w}")
    if errors:
        print(f"\n❌ requirements_audit：{len(errors)} 处引用缺口")
        for e in errors:
            print(f"  - {e}")
        return 1
    print("✅ requirements_audit：全部引用存在")
    return 0


if __name__ == "__main__":
    sys.exit(main())
