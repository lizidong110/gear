"""临时脚本：手工起 xlsx 侧车，按 wire 协议打一条 write，再用 openpyxl（第三方，
不是我们自己）把产物读回来——AC-E4-1 要的就是「不自己验自己」。

跑法：先 $env:COORD_ADDR="127.0.0.1:8123" 起 plugin/xlsx-sidecar.exe，再跑本脚本。
用完即删：正式用例会落进 plugin/verify.py 的 P1e 段。
"""
import json
import sys
import tempfile
import urllib.request

COORD = "http://127.0.0.1:8123"

# 代理必须显式关掉：本机若有 http_proxy，127.0.0.1 会被送去代理，开局就是个假故障。
opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))


_LINES = []


def print(*a, **k):  # noqa: A001 - 劫持 print 是为了把结果自己落成 UTF-8 日志
    _LINES.append(" ".join(str(x) for x in a))


def call(method, args, timeout=30):
    body = json.dumps({"args": args}).encode("utf-8")
    req = urllib.request.Request(COORD + "/" + method, data=body,
                                 headers={"Content-Type": "application/json"})
    with opener.open(req, timeout=timeout) as r:
        return json.loads(r.read())


def main():
    root = tempfile.mkdtemp(prefix="xlsx-cross-")
    rows = [["产品", "数量", "单价", "是否进口", None, 0]]
    for i in range(1, 2001):
        rows.append(["产品-%d" % i, float(i), round(i * 1.5, 2), i % 2 == 0, None, ""])
    # 各种“脏”字符：都要原样回来
    rows.append(["<a> & \"b\" 'c' 100%", "  前后有空格  ", "中文/Tab\t混合", "Emoji 🎉", None, 1e21])
    res = call("write", {
        "path": "reports/2026-09.xlsx",
        "root": root,
        "sheets": [
            {"name": "明细", "rows": rows},
            {"name": "汇总 sheet", "rows": [["合计", 999]]},
        ],
    })
    if "result" not in res:
        print("FAIL 调用失败:", res)
        return 1
    path = res["result"]["path"]
    print("产物:", path, res["result"]["size"], "字节")

    import openpyxl
    wb = openpyxl.load_workbook(path)
    fails = []

    def ck(name, ok, detail=""):
        print(("  PASS " if ok else "  FAIL ") + name + ("" if ok else " <- " + str(detail)))
        if not ok:
            fails.append(name)

    ck("工作表名和顺序", wb.sheetnames == ["明细", "汇总 sheet"], wb.sheetnames)
    ws = wb["明细"]
    ck("行数", ws.max_row == 2002, ws.max_row)
    ck("列数", ws.max_column == 6, ws.max_column)
    ck("表头文本", ws["A1"].value == "产品" and ws.cell(row=1, column=2).value == "数量",
       [ws["A1"].value, ws.cell(row=1, column=2).value])
    ck("第 2000 行数字", ws.cell(row=2001, column=2).value == 2000.0, ws.cell(row=2001, column=2).value)
    ck("小数保持两位", ws.cell(row=2001, column=3).value == 3000.0, ws.cell(row=2001, column=3).value)
    # 注意 openpyxl 的行号从 1 起算：第 2001 行是 i=2000 那条（true/false 交替，故为 True）
    ck("布尔是布尔不是 1/0", ws.cell(row=2001, column=4).value is True,
       repr(ws.cell(row=2001, column=4).value))
    ck("空单元格是 None 而不是空串", ws.cell(row=2001, column=5).value is None,
       repr(ws.cell(row=2001, column=5).value))
    ck("空串单元格也视为空", ws.cell(row=2001, column=6).value is None,
       repr(ws.cell(row=2001, column=6).value))
    last = ws.max_row
    ck("XML 特殊字符原样回来", ws.cell(row=last, column=1).value == "<a> & \"b\" 'c' 100%",
       repr(ws.cell(row=last, column=1).value))
    ck("首尾空格保留", ws.cell(row=last, column=2).value == "  前后有空格  ",
       repr(ws.cell(row=last, column=2).value))
    ck("Tab 与中文混合", ws.cell(row=last, column=3).value == "中文/Tab\t混合",
       repr(ws.cell(row=last, column=3).value))
    ck("非 BMP 字符（emoji）", ws.cell(row=last, column=4).value == "Emoji 🎉",
       repr(ws.cell(row=last, column=4).value))
    # 没带小数点的数字回来是 int（写出来的字面量确实没有小数点），带了才是 float
    _big = ws.cell(row=last, column=6).value
    ck("极大数既不丢失也不是科学计数法", (_big == 10**21) and isinstance(_big, (int, float))
       and "e" not in repr(_big).lower(), repr(_big))
    ws2 = wb["汇总 sheet"]
    ck("第二个工作表", ws2["A1"].value == "合计" and ws2["B1"].value == 999.0,
       [ws2["A1"].value, ws2["B1"].value])

    print("\nopenpyxl 交叉验证：", "PASS" if not fails else "FAIL %d 项" % len(fails))
    return 1 if fails else 0


if __name__ == "__main__":
    rc = main()
    with open(r"D:\gadern\gear\build\xlsx_cross.log", "w", encoding="utf-8") as f:
        f.write("\n".join(_LINES))
        f.write("\nrc=%d\n" % rc)
    sys.exit(rc)
