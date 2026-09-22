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

# 输出目录 —— 一律用**相对仓库根**的路径，绝不用 POSIX 绝对路径（`/d/...`）。
#
# ⚠ 血的教训（2026-09-21 查清）：Git Bash 下把 `/d/gadern/gear/build/gear.exe` 这样的 POSIX
# 绝对路径交给 `go build -o`，路径**不会被转换成 Windows 路径** —— Windows 版 Go 把它当成
# 「当前盘符下的绝对路径」，于是产物静静落到 `D:\d\gadern\gear\build\`，而
# `D:\gadern\gear\build\` 里的旧二进制纹丝不动。`go build` 返回 0，脚本打印 ✅ 与
# BUILD RESULT: PASS。这就是「改了代码没生效、看起来像改的代码没用」的**真正根因**：
# 不是 Go 的 bug，是路径。脚本已 `cd "$ROOT"`，相对路径既准确又跨平台。
BIN_REL="build"
PLUGINBIN_REL="build/plugin"
mkdir -p "$BIN_REL" "$PLUGINBIN_REL"
# 仅用于回显（给人看的），不参与任何构建命令。
BIN="$ROOT/$BIN_REL"
PLUGINBIN="$ROOT/$PLUGINBIN_REL"

# 编译清单："源码包路径|输出名（不含后缀）|目标目录（相对仓库根）"
#   gear 进 build/，sidecar 进 build/plugin/
TARGETS=(
  "cmd/gear|gear|$BIN_REL"
  "plugin/kafka-sidecar|kafka-sidecar|$PLUGINBIN_REL"
  "plugin/mqtt-sidecar|mqtt-sidecar|$PLUGINBIN_REL"
  "plugin/rabbitmq-sidecar|rabbitmq-sidecar|$PLUGINBIN_REL"
  "plugin/serve-sidecar|serve-sidecar|$PLUGINBIN_REL"
  "plugin/sidecar-template|sidecar-template|$PLUGINBIN_REL"
  "plugin/webhook-sidecar|webhook-sidecar|$PLUGINBIN_REL"
)

FAILED=0
echo "══════════ Gear 编译 $(date '+%F %T') ══════════"
for t in "${TARGETS[@]}"; do
  IFS='|' read -r pkg name out <<< "$t"
  dst="$out/$name$EXT"
  echo
  echo "── 编译 $pkg → $ROOT/$dst"

  # 构建到 .new 再就地换上，而不是直接覆盖目标。两个原因，都是实测出来的：
  #  1. **Windows 不允许覆盖正在运行的 exe**（开发时 gear.exe 往往正作为 MCP stdio 服务跑着），
  #     但它**允许重命名**，所以「旧文件改名 → 新文件顶上」能避开占用；
  #  2. 换名之后，「构建成功 ⇒ 目标一定是新生成的」这条判据才成立，可据以拒绝静默失败。
  # 临时名必须带平台可执行后缀：`go build -o x.new` 在 Windows 上会写出 `x.new.exe`，
  # 于是 mv 找不到源文件、报出一个与病因无关的错误（2026-09-21 踩过）。
  tmp="$out/$name.new$EXT"
  built_at="$(date +%s)"

  if ! go build -trimpath -o "$tmp" "./$pkg" 2>&1; then
    echo "❌ $name（编译失败，见上方错误）"
    FAILED=1
    continue
  fi
  if [ -f "$dst" ]; then
    mv -f "$dst" "$dst.old" 2>/dev/null || true
  fi
  if ! mv -f "$tmp" "$dst" 2>/dev/null; then
    [ -f "$dst" ] || mv -f "$dst.old" "$dst" 2>/dev/null # 还原现场，不留半成品
    echo "❌ $name（无法替换产物：它正被其它进程占用？先停掉该进程再编译）"
    FAILED=1
    continue
  fi
  rm -f "$dst.old" 2>/dev/null || true # 旧文件若仍被运行中的进程持有，删不掉也无妨（下次再删）

  # 最后一道闸：用「产物 mtime ≥ 本次构建开始」把「返回 0 却没写成」钉死。
  if [ ! -f "$dst" ] || [ "$(stat -c %Y "$dst" 2>/dev/null || echo 0)" -lt "$built_at" ]; then
    echo "❌ $name（构建返回成功但产物未更新：写入被静默跳过）"
    FAILED=1
    continue
  fi
  echo "✅ $name"
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
