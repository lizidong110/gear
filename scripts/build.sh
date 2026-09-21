#!/usr/bin/env bash
# Gear 一键编译脚本：把「需要编译的」二进制一次性产出到 build/ 与 build/plugin/，
# 改完代码跑一次即可在 build/ 下直接做测试，无需重复敲 go build。
#
# 覆盖范围：
#   - build/gear(.exe)                  ← cmd/gear（主 CLI / 内核）
#   - build/plugin/*-sidecar(.exe)      ← plugin/ 下全部 package main（共 6 个）
#
# 用法：  bash scripts/build.sh
# 退出码即结果（0=PASS）。go build 本身增量编译，重复跑只重建改动过的包。
#
# 说明：环境（PATH 兜底 / GOPROXY / GOSUMDB / GOFLAGS）与 scripts/gate.sh 对齐，
# 避免在 Windows Git-Bash / CI 沙箱里因缺 POSIX 工具或 proxy 卡住。
set -uo pipefail

# 1) 补齐 POSIX 工具链（sh/ls/...）：Windows 的 Git-Bash / CI 沙箱里 PATH 经常不含它们。
#    只在目录确实存在时前置，不覆盖调用方已有的 PATH。
for d in "/usr/bin" "/bin" "/c/Program Files/Git/usr/bin" "/c/Program Files/Git/bin"; do
  case ":$PATH:" in
    *":$d:"*) ;;
    *) [ -d "$d" ] && PATH="$d:$PATH" ;;
  esac
done
export PATH

# 2) Go 环境（与开发/测试环境一致：避免 proxy/sumdb 卡住，mod=mod 允许按需取依赖）
export GOPROXY=https://goproxy.cn,direct
export GOSUMDB=off
export GOFLAGS=-mod=mod

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT" || exit 1

# 目标平台可执行文件后缀（windows=.exe, linux/darwin=空），保证脚本跨平台可用
EXT="$(go env GOEXE)"

# 输出目录
BIN="$ROOT/build"
PLUGINBIN="$ROOT/build/plugin"
mkdir -p "$BIN" "$PLUGINBIN"

# 编译清单："源码包路径|输出名（不含后缀）|目标目录"
#   gear 进 build/，sidecar 进 build/plugin/
TARGETS=(
  "cmd/gear|gear|$BIN"
  "plugin/kafka-sidecar|kafka-sidecar|$PLUGINBIN"
  "plugin/mqtt-sidecar|mqtt-sidecar|$PLUGINBIN"
  "plugin/rabbitmq-sidecar|rabbitmq-sidecar|$PLUGINBIN"
  "plugin/serve-sidecar|serve-sidecar|$PLUGINBIN"
  "plugin/sidecar-template|sidecar-template|$PLUGINBIN"
  "plugin/webhook-sidecar|webhook-sidecar|$PLUGINBIN"
)

FAILED=0
echo "══════════ Gear 编译 $(date '+%F %T') ══════════"
for t in "${TARGETS[@]}"; do
  IFS='|' read -r pkg name out <<< "$t"
  dst="$out/$name$EXT"
  echo
  echo "── 编译 $pkg → $dst"
  if go build -trimpath -o "$dst" "./$pkg" 2>&1; then
    echo "✅ $name"
  else
    echo "❌ $name（编译失败，见上方错误）"
    FAILED=1
  fi
done

echo
echo "══════════ 编译汇总 ══════════"
if [ "$FAILED" -eq 0 ]; then
  echo "BUILD RESULT: PASS"
  echo "二进制位置："
  echo "  $BIN/gear$EXT"
  echo "  $PLUGINBIN/*$EXT"
else
  echo "BUILD RESULT: FAIL（见上方 ❌ 项）"
fi
exit "$FAILED"
