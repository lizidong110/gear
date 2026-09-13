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
# 说明：用 -v 保证 t.Log 也进入日志；tee 同时输出终端与文件。仅保留最近 20 份，防磁盘膨胀。
set -uo pipefail

# 项目工具链（与开发环境一致）：-race 需要 cgo，故强制 CGO_ENABLED=1；
# 其余为既定 Go proxy/校验设置，避免依赖调用方 shell 的环境。
export CGO_ENABLED=1
export GOPROXY=https://goproxy.cn,direct
export GOSUMDB=off
export GOFLAGS=-mod=mod

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LOGDIR="$ROOT/build/testlogs"
mkdir -p "$LOGDIR"

TS="$(date +%Y%m%d-%H%M%S)"
LOG="$LOGDIR/gate-$TS.log"
LATEST="$LOGDIR/latest.log"

cd "$ROOT" || exit 1

set +e
go test -race -cover -p 1 -v ./... 2>&1 | tee "$LOG"
RC=${PIPESTATUS[0]}
set -e

# latest.log 便于直接打开最近一次
cat "$LOG" > "$LATEST"

# 仅保留最近 20 份，避免磁盘膨胀
ls -1t "$LOGDIR"/gate-*.log 2>/dev/null | tail -n +21 | xargs rm -f 2>/dev/null

echo
if [ "$RC" -eq 0 ]; then
  echo "✅ GATE PASS  (coverage 见上)  本次日志: $LOG"
else
  echo "❌ GATE FAIL  (exit=$RC)  本次日志: $LOG  最近日志: $LATEST"
  echo "   直接打开 $LATEST 复现，无需重跑整个闸门。"
fi
exit "$RC"
