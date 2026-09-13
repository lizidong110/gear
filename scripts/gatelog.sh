#!/usr/bin/env bash
# 查看最近一次闸门日志（或列出历史）。
#   bash scripts/gatelog.sh        -> cat build/testlogs/latest.log
#   bash scripts/gatelog.sh list   -> 列出最近 20 份 gate-*.log
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LOGDIR="$ROOT/build/testlogs"

case "${1:-cat}" in
  list)
    ls -1t "$LOGDIR"/gate-*.log 2>/dev/null | head -20 || echo "尚无历史日志"
    ;;
  *)
    if [ -f "$LOGDIR/latest.log" ]; then
      cat "$LOGDIR/latest.log"
    else
      echo "尚无闸门日志（先运行: bash scripts/gate.sh）"
    fi
    ;;
esac
