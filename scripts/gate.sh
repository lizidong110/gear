#!/usr/bin/env bash
# Gear 质量闸门：全量 -race -cover 测试，日志落盘 build/testlogs/，避免「管道日志用完就扔」。
#
# 用法：  bash scripts/gate.sh
# 退出码即闸门结果（0=PASS）。运行后：
#   - 本次完整日志： build/testlogs/gate-<时间戳>.log
#   - 最近一次：     build/testlogs/latest.log（直接打开/编辑此文件复现失败，无需重跑）
#   - 查看最近：     bash scripts/gatelog.sh        （cat latest）
#   - 列出历史：     bash scripts/gatelog.sh list
#
# 覆盖范围（对应 GEAR_工程方法_v1.md §5 的闸门表，避免「文档说要跑、脚本没跑」的漂移）：
#   ① 编译 go build ./...          ② 静态 go vet ./...     ③ 格式化 gofmt -l 零差异
#   ④ staticcheck ./...            ⑤ go test -race -cover  ⑥ 覆盖率门槛（engine/cap/mcp/cli ≥80%）
#   ⑦ 文档↔代码审计 doc_audit.py   ⑧ extensions 必填一致性 extensions_audit.py
# 任一项失败即整体失败，且**不中断后续**（跑完全部再汇总，一次看全所有问题）。
#
# 说明：用 -v 保证 t.Log 也进入日志；tee 同时输出终端与文件。仅保留最近 20 份，防磁盘膨胀。
set -uo pipefail

# 项目工具链（与开发环境一致）：-race 需要 cgo，故强制 CGO_ENABLED=1；
# 其余为既定 Go proxy/校验设置，避免依赖调用方 shell 的环境。
export CGO_ENABLED=1
export GOPROXY=https://goproxy.cn,direct
export GOSUMDB=off
export GOFLAGS=-mod=mod

# 补齐 POSIX 工具链（sh/ls/...）：Windows 的 Git-Bash / CI 沙箱里 PATH 经常不含它们。
#
# ⚠ 2026-09-21 起的口径变化：引擎的 process runtime **不再默认依赖 sh**（改走
# internal/cmdline 的原生 exec，见该包注释——存在理由就是「Windows 系统 PATH 里没有 sh」）。
# 但仍需要它们，原因有二：
#   ① 脚本自身要用 ls/date/awk 等 POSIX 工具；
#   ② `cmdline` / `engine` 里「含 shell 元字符时兜底」的那条路径**真的会起 sh**，
#      缺了它，那些用例会从「跑一遍」退化成「跳过」，闸门会少验一块。
# 只在目录确实存在时前置，不覆盖调用方已有的 PATH。
for d in "/usr/bin" "/bin" "/c/Program Files/Git/usr/bin" "/c/Program Files/Git/bin"; do
  case ":$PATH:" in
    *":$d:"*) ;;
    *) [ -d "$d" ] && PATH="$d:$PATH" ;;
  esac
done
export PATH

# 引擎/内核包的覆盖率门槛（%）。来源：GEAR_工程方法_v1.md §5「引擎/内核包 ≥80%，
# model/registry 关键路径 100%」；低于门槛即阻断合并。如需调整请改文档后同步这里。
#
# 这是「枚举式声明」，必须**逐项登记**（方法论红线 15）：新增内核包忘了加进来，
# 该包就等于没有门槛。2026-09-21 补入三个漏登项——
#   internal/app（应用生命周期：起服/探活/台账/派生，风险最高的新面，实测 82%）
#   internal/cmdline（无 shell 的命令解析器：所有 process 型 Tool 与侧车的必经之路）
#   internal/gearhome（自身落盘位置的唯一真相源：错了会让「换 cwd 就换一份数据」重现）
# 未纳入：execstore（转发垫片，无自身语句）、mcp/guide（go:embed 内容）、
# ext/ai/sidecar（要么薄封装要么强依赖外部服务，纳入门槛只会制造噪声）。
COVER_MIN=80
COVER_PACKAGES="./internal/engine ./internal/cap ./internal/mcp ./internal/cli ./internal/server ./internal/model ./internal/registry ./internal/app ./internal/cmdline ./internal/gearhome"

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LOGDIR="$ROOT/build/testlogs"
mkdir -p "$LOGDIR"

TS="$(date +%Y%m%d-%H%M%S)"
LOG="$LOGDIR/gate-$TS.log"
LATEST="$LOGDIR/latest.log"

cd "$ROOT" || exit 1

set +e
{
echo "══════════ Gate $(date '+%F %T') ══════════"

FAILED=0

step() {   # step <标题> <命令...>
  local name="$1"; shift
  echo
  echo "───── $name ─────"
  local out rc
  out="$("$@" 2>&1)"; rc=$?
  if [ -n "$out" ]; then echo "$out"; fi
  if [ "$rc" -eq 0 ]; then
    echo "✅ $name"
  else
    echo "❌ $name (exit=$rc)"
    FAILED=1
  fi
  return 0
}

step "① 编译 go build ./..." go build ./...
step "② 静态 go vet ./..."   go vet ./...

echo
echo "───── ③ 格式化 gofmt -l ─────"
UNFMT="$(gofmt -l internal cmd plugin examples 2>&1)"
if [ -n "$UNFMT" ]; then
  echo "以下文件未格式化："
  echo "$UNFMT"
  echo "❌ gofmt (差异 ${#UNFMT} 字节；修： gofmt -w <文件>)"
  FAILED=1
else
  echo "✅ gofmt 零差异"
fi

step "④ staticcheck ./..." go run honnef.co/go/tools/cmd/staticcheck@latest ./...

echo
echo "───── ⑤ 测试 go test -race -cover -p 1 -v ./... ─────"
go test -race -cover -p 1 -v ./...
RC=${PIPESTATUS[0]}
if [ "$RC" -eq 0 ]; then echo "✅ go test"; else echo "❌ go test (exit=$RC)"; FAILED=1; fi

echo
echo "───── ⑥ 覆盖率门槛（≥ ${COVER_MIN}%）─────"
for pkg in $COVER_PACKAGES; do
  line="$(go test -cover "$pkg" -count=1 2>&1 | tail -1)"
  pct="$(echo "$line" | grep -o 'coverage: [0-9.]*%' | grep -o '[0-9.]*')"
  if [ -z "$pct" ]; then
    echo "  ⚠️  $pkg 取不到覆盖率：$line"
    continue
  fi
  ok="$(awk -v a="$pct" -v b="$COVER_MIN" 'BEGIN{print (a+0>=b+0)?"yes":"no"}')"
  if [ "$ok" = "yes" ]; then
    echo "  ✅ $pkg ${pct}%"
  else
    echo "  ❌ $pkg ${pct}% < ${COVER_MIN}%"
    FAILED=1
  fi
done

echo
echo "───── ⑦ 文档↔代码审计 tools/doc_audit.py ─────"
# 机械 doc↔code 一致性核对（恢复当年丢失在 gitignored build/ 的脚本，已迁 tools/）：
# ⑥ REST 端点双向核对、⑦ CLI 子命令双向核对计入闸门；① Test 名 / ② .go 路径为 advisory。
if command -v python >/dev/null 2>&1; then
  python tools/doc_audit.py
  rc=$?
  if [ "$rc" -eq 0 ]; then echo "✅ doc_audit（退出码 0）"; else echo "❌ doc_audit（exit=$rc）"; FAILED=1; fi
else
  echo "⚠️ 未找到 python，跳过 doc_audit（需 python3 才能跑机械审计；不会因此阻断闸门）"
fi

echo
echo "───── ⑧ extensions 必填一致性 tools/extensions_audit.py ─────"
# extensions.yaml 的 schemas 与 internal/ext/*.go 的必填判定必须一致：
# 代码写着 `if key == "" { 报错 }` 而配置没标 `required: true` 时，智能体照参数表组装入参
# 会一路走到运行期才失败——违反「能静态校验的绝不留到运行期」。2026-09-21 实测两处漂移，
# 其中 sms.gateway 的描述还写着「可选参数…缺省则报错」这种自我矛盾的话。
if command -v python >/dev/null 2>&1; then
  python tools/extensions_audit.py
  rc=$?
  if [ "$rc" -eq 0 ]; then echo "✅ extensions_audit（退出码 0）"; else echo "❌ extensions_audit（exit=$rc）"; FAILED=1; fi
else
  echo "⚠️ 未找到 python，跳过 extensions_audit"
fi

echo
echo "══════════ Gate 汇总 ══════════"
if [ "$FAILED" -eq 0 ]; then
  echo "GATE RESULT: PASS"
else
  echo "GATE RESULT: FAIL（见上方各 ❌ 项）"
fi
} 2>&1 | tee "$LOG"
GATE_RC=$?

set -e

# latest.log 便于直接打开最近一次
cat "$LOG" > "$LATEST"

# 汇总行决定退出码：CI 用得上（杜绝「某项红了但脚本仍 exit 0」）
if grep -q 'GATE RESULT: PASS' "$LATEST"; then RC=0; else RC=1; fi

# 仅保留最近 20 份，避免磁盘膨胀
ls -1t "$LOGDIR"/gate-*.log 2>/dev/null | tail -n +21 | xargs rm -f 2>/dev/null

echo
if [ "$RC" -eq 0 ]; then
  echo "✅ GATE PASS  本次日志: $LOG"
else
  echo "❌ GATE FAIL  本次日志: $LOG  最近日志: $LATEST"
  echo "   直接打开 $LATEST 复现，无需重跑整个闸门。"
fi
exit "$RC"
