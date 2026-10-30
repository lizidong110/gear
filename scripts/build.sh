#!/usr/bin/env bash
# Gear 一键编译脚本：把「需要编译的」二进制一次性产出到 build/ 与 build/plugin/，
# 改完代码跑一次即可在 build/ 下直接做测试，无需重复敲 go build。
#
# 覆盖范围：
#   - build/gear(.exe)                  ← cmd/gear（主 CLI / 内核）
#   - build/plugin/*-sidecar(.exe)      ← plugin/ 下全部 package main（共 15 个，见 TARGETS）
#                                          （此处不再写死数字口径：历史上一直停在「共 6 个」，
#                                           每次加侧车都忘了改，反而是个会骗人的注释）
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
  "plugin/cron-sidecar|cron-sidecar|$PLUGINBIN_REL"
  "plugin/filestore-sidecar|filestore-sidecar|$PLUGINBIN_REL"
  "plugin/kafka-sidecar|kafka-sidecar|$PLUGINBIN_REL"
  # 2026-09-30 补：这三个一直漏在清单外（此前只靠 verify.py 自己 go build 才编出来）
  "plugin/xlsx-sidecar|xlsx-sidecar|$PLUGINBIN_REL"
  "plugin/pdf-sidecar|pdf-sidecar|$PLUGINBIN_REL"
  "plugin/oss-sidecar|oss-sidecar|$PLUGINBIN_REL"
  "plugin/redis-sidecar|redis-sidecar|$PLUGINBIN_REL"
  "plugin/mqtt-sidecar|mqtt-sidecar|$PLUGINBIN_REL"
  "plugin/mongo-sidecar|mongo-sidecar|$PLUGINBIN_REL"
  "plugin/oidc-sidecar|oidc-sidecar|$PLUGINBIN_REL"
  "plugin/rabbitmq-sidecar|rabbitmq-sidecar|$PLUGINBIN_REL"
  "plugin/serve-sidecar|serve-sidecar|$PLUGINBIN_REL"
  "plugin/sidecar-template|sidecar-template|$PLUGINBIN_REL"
  "plugin/webhook-sidecar|webhook-sidecar|$PLUGINBIN_REL"
)

FAILED=0
# 版本注入（2026-10-03）：`gear version` / `--version` 的数据源。
#
# 取值口径：优先 git tag（vX.Y.Z），没有精确 tag 时用 `describe --always` 的短 sha，
# 再没有（不在 git 检出里 / 无 git）才是 dev。**注入失败不阻塞构建**——
# buildinfo.Version 保持 dev，`gear version` 会明说「dev = 构建时未注入版本号」，
# 而不是让人以为拿到的是发布构建（不谎报）。
GEAR_VER="$(git describe --tags --exact-match 2>/dev/null || git describe --tags --always --dirty 2>/dev/null || echo dev)"
GEAR_SHA="$(git rev-parse --short HEAD 2>/dev/null || echo none)"
GEAR_TS="$(date -u '+%Y-%m-%dT%H:%M:%SZ')"
GEAR_LDFLAGS="-X gear/internal/buildinfo.Version=$GEAR_VER"
GEAR_LDFLAGS="$GEAR_LDFLAGS -X gear/internal/buildinfo.Commit=$GEAR_SHA"
GEAR_LDFLAGS="$GEAR_LDFLAGS -X gear/internal/buildinfo.BuildTime=$GEAR_TS"

# 前端 Web UI 构建（REQ-UI-NF-EMBED-PIPE）：把 plugin/web-ui 的构建产物拷入 internal/webui/public，
# 供 Go 侧 //go:embed 编译进二进制（同源托管，零 CORS）。
#   仅当 pnpm 可用且依赖已装（node_modules 存在）时才构建；否则跳过并告警——
#   此时 internal/webui/public 只有占位页 _placeholder.html，运行期访问 / 返回 503 指引，而不是空白。
#   拷入前先清掉上次的真实产物（index.html / assets / 各类静态文件），避免残留旧构建；
#   _placeholder.html 永不被清，保证 embed 始终能编译。
UI_DIST="internal/webui/public"
if command -v pnpm >/dev/null 2>&1 && [ -d "plugin/web-ui/node_modules" ]; then
  echo "── 构建 Web UI (plugin/web-ui) → $UI_DIST"
  if (cd plugin/web-ui && pnpm run build); then
    rm -f "$UI_DIST/index.html" "$UI_DIST"/*.js "$UI_DIST"/*.css "$UI_DIST"/*.json 2>/dev/null || true
    rm -rf "$UI_DIST/assets" 2>/dev/null || true
    if cp -r plugin/web-ui/dist/. "$UI_DIST/"; then
      echo "✅ Web UI 产物已拷入 $UI_DIST"
    else
      echo "⚠ Web UI 构建成功但产物拷入失败；$UI_DIST 将仅有占位页（运行期 / 返回 503 指引）"
    fi
  else
    echo "⚠ Web UI 构建失败，将使用占位页（运行期访问 / 返回 503 指引）"
  fi
else
  echo "⚠ 跳过 Web UI 构建（pnpm 不可用或未安装依赖 plugin/web-ui/node_modules）；运行期若访问 / 将返回 503 指引"
fi

echo "══════════ Gear 编译 $(date '+%F %T') ══════════"
echo "版本注入：Version=$GEAR_VER Commit=$GEAR_SHA BuildTime=$GEAR_TS"
for t in "${TARGETS[@]}"; do
  IFS='|' read -r pkg name out <<< "$t"
  dst="$out/$name$EXT"
  echo
  echo "── 编译 $pkg → $ROOT/$dst"

  # 只有主二进制带版本注入：侧车是插件，不是 gear 本体，注入它们没有消费者
  # （真要标侧车版本，`gear version` 里给的是产物构建时间）。
  # ⚠ `-ldflags=` 必须作为**一个整体参数**交给 go build，不能把变量裸展开在命令行里：
  # GEAR_LDFLAGS 含 3 个 `-X`（空格分隔），写成 `go build $ldflags -o ...` 时会被 shell
  # 拆成多个词，于是 `-o` 落到「包路径」的位置 → `malformed import path "-o": leading dash`。
  # 实测（2026-10-08）：本 bug 随上面这段版本注入一起引入，**此后 build/gear.exe 一次都没被
  # 成功重建过**——脚本仍会继续把 14 个 sidecar 全编完、最后才在汇总打一个 ❌，极易被忽略。
  # 代价是 MCP 面一直跑着 10-03 的旧二进制（见 .workbuddy/memory/2026-10-08.md 根因分析）。
  ldflags_value=""
  [ "$name" = "gear" ] && ldflags_value="$GEAR_LDFLAGS"

  # 构建到 .new 再就地换上，而不是直接覆盖目标。两个原因，都是实测出来的：
  #  1. **Windows 不允许覆盖正在运行的 exe**（开发时 gear.exe 往往正作为 MCP stdio 服务跑着），
  #     但它**允许重命名**，所以「旧文件改名 → 新文件顶上」能避开占用；
  #  2. 换名之后，「构建成功 ⇒ 目标一定是新生成的」这条判据才成立，可据以拒绝静默失败。
  # 临时名必须带平台可执行后缀：`go build -o x.new` 在 Windows 上会写出 `x.new.exe`，
  # 于是 mv 找不到源文件、报出一个与病因无关的错误（2026-09-21 踩过）。
  tmp="$out/$name.new$EXT"
  built_at="$(date +%s)"

  if ! go build -trimpath "-ldflags=$ldflags_value" -o "$tmp" "./$pkg" 2>&1; then
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
