#!/usr/bin/env bash
# Gear 质量闸门：全量 -race -cover 测试，日志落盘 build/testlogs/，避免「管道日志用完就扔」。
#
# 用法：  bash scripts/gate.sh                 # full（默认）：全部包，提交前 / CI 用这个
#         bash scripts/gate.sh --fast         # 只跑「本次改动影响到的包」（迭代中用）
#         bash scripts/gate.sh --serial       # 强制 -p 1 串行（排查并行争用时用）
#         bash scripts/gate.sh --fast --serial
# 退出码即闸门结果（0=PASS）。运行后：
#   - 本次完整日志： build/testlogs/gate-<时间戳>.log
#   - 最近一次：     build/testlogs/latest.log（直接打开/编辑此文件复现失败，无需重跑）
#   - 最近一次测试原始输出（⑥ 据此解析覆盖率）：build/testlogs/.test-latest.out
#   - 查看最近：     bash scripts/gatelog.sh        （cat latest）
#   - 列出历史：     bash scripts/gatelog.sh list
#
# 覆盖范围（对应 GEAR_工程方法_v1.md §5 的闸门表，避免「文档说要跑、脚本没跑」的漂移）：
#   ① 编译 go build ./...          ② 静态 go vet ./...     ③ 格式化 gofmt -l 零差异
#   ④ staticcheck ./...            ⑤ go test -race -cover  ⑥ 覆盖率门槛（engine/cap/mcp/cli ≥80%）
#   ⑦ 文档↔代码审计 doc_audit.py   ⑧ extensions 必填一致性 extensions_audit.py
#   ⑨ 应用可移植性审计 portable_audit.py（apps/examples 禁机器相关绝对路径）
#   ⑩ 需求薄索引引用审计 requirements_audit.py（REQUIREMENTS.md 的文件/测试引用必须真实存在）
# 任一项失败即整体失败，且**不中断后续**（跑完全部再汇总，一次看全所有问题）。
#
# ── 2026-09-28 提速改造（实测数据见工程方法 §5.3，三处改动不改变校验强度）───────────
#   P0 ⑥ 不再逐包重跑：⑤ 的 -cover 输出本来就带每包 `coverage: X%`，⑥ 改为解析它。
#      旧实现逐包 `go test -cover -count=1`（不带 -race ⇒ 与⑤缓存不共享，且 -count=1
#      强制重跑）实测 1029s，而跑出来的 13 个数值与⑤输出**逐包完全一致**——纯重复劳动。
#   P2 ⑤ 默认去掉 -p 1：实测 1184s → 416s。但历史上有「并行偶发静默失败」的实证
#      （2026-09-14 / 09-18），故配**失败自动回退串行重跑**：并行红不算数，-p 1 再跑一遍
#      作为最终判定，避免把环境争用误判成代码问题。--serial 强制串行。
#   P3 --fast 变更范围选路：用 go list 的 Deps 反向依赖闭包算出「改动影响到的包」。
#      口径是**不确定就升级 full**（宁可多跑，不可少跑），见 resolve_fast_packages 的注释。
#   另：④ 固定 staticcheck 版本并预装到 GOPATH/bin（@latest 每次 21s 且离线即挂）；
#       python 审计的仓库遍历改为 os.walk 剪枝（requirements_audit 26s → 1s）。
set -uo pipefail

# ── 参数 ────────────────────────────────────────────────────────────────────
MODE=full    # full = 全部包；fast = 变更影响到的包
SERIAL=0     # 1 = 强制 -p 1 串行
usage() {
  # 打印头部注释直到「提速改造」段为止（按内容截断，不写死行号——行号会随注释增删漂移）
  awk 'NR==1 {next} /^# ── 2026-09-28/ {exit} {print}' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
  exit 0
}
for arg in "$@"; do
  case "$arg" in
    --fast)   MODE=fast ;;
    --serial) SERIAL=1 ;;
    -h|--help) usage ;;
    *) echo "❌ 未知参数：$arg（用 --help 看用法）"; exit 2 ;;
  esac
done

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
# 2026-09-26 补入 internal/artifact（产物导出/导入：L3「用户构建的东西能带走」的必经路径，
# 实测 82%）——新增内核包忘了加进来，该包就等于没有门槛。
# 2026-09-26 补入 internal/httpsafe（监听闸 + 同源判定：bind.go 从 mcp 提升为共享后，
# 判定逻辑漏网即闸门漂移重演）。已存在但此前一直漏登记，一并补上。
COVER_MIN=80
COVER_PACKAGES="./internal/engine ./internal/cap ./internal/mcp ./internal/cli ./internal/server ./internal/model ./internal/registry ./internal/app ./internal/cmdline ./internal/gearhome ./internal/artifact ./internal/httpsafe ./internal/procalive ./internal/doctor ./internal/luacap ./internal/sidecar/describe"

# staticcheck 固定版本：原来写 @latest，每次都要联网解析版本再编译（实测 21s，且离线即挂），
# 还会因为上游发新版而「同一份代码今天绿明天红」。现在预装到 GOPATH/bin 复用（实测 19s/8s），
# 版本不符或缺失时自动装一次。升级版本：改这里 + 删掉 $GOPATH/bin/staticcheck。
SC_VERSION=2026.2.1

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LOGDIR="$ROOT/build/testlogs"
mkdir -p "$LOGDIR"

TS="$(date +%Y%m%d-%H%M%S)"
LOG="$LOGDIR/gate-$TS.log"
LATEST="$LOGDIR/latest.log"
# ⑤ 的原始输出落盘一份：⑥ 的覆盖率从它解析，不用再跑一遍（见文件头 P0）。
TESTOUT="$LOGDIR/.test-latest.out"

cd "$ROOT" || exit 1
GATE_START="$(date +%s)"

ts()  { date '+%H:%M:%S'; }            # 段标题用：能一眼看出卡在哪一段
dur() { echo "$(( $(date +%s) - $1 ))s"; }   # dur <起始秒>：段耗时

# ───────────────────────────────────────────────────────────────────────────
# --fast：按变更范围选路（P3）
#
# 口径：**不确定就升级 full**——宁可多跑，不可少跑。只有「变更集能干净地映射到 Go 包」
# 时才走 fast，否则一律 full。回退 full 的触发条件（每条都会打印原因）：
#   · 不在 git 仓库 / git 不可用 / 工作区无变更
#   · porcelain 里出现 rename（"a -> b" 没法机械拆成两个路径）
#   · 变更路径不在 internal/ plugin/ cmd/ 下 —— 改的是 yaml / testdata / 文档 / 脚本 /
#     extensions.yaml / 闸门自身，影响面无法用依赖闭包刻画
#   · 变更文件所在目录不是 Go 包（例如 internal/testdata/）
# 命中 fast 时跑的是「变更包 + 所有（传递）依赖了变更包的包」：用 go list 输出的 Deps
# （Go 给的是传递闭包）反查，机械正确，不靠人肉维护的「改 A 要测 B」名单。
#
# 唯一的收窄：**只改了 *_test.go 就不取闭包**，只跑这些包自己。
# 测试文件只进本包的测试二进制、不会被别的包 import，所以改测试影响不到别的包——
# 这是 Go 的确定性事实，不是启发式。没有这条的话，改 cli 的一个测试也会把
# engine(289s) + cli(259s) + … 整个闭包拉起来，--fast 跑出来和 full 一样久
# （实测：改 internal/procalive 实现 → 闭包 16 包 592s，full 573s，快不出来）。
# ───────────────────────────────────────────────────────────────────────────
# 约定（踩过坑，别改回去）：**stdout 只允许出现包列表**——调用方把 stdout 直接当
# `go test` 的参数列表用。任何说明文字一律走 stderr（stderr 仍会进日志，因为整个块
# 是 `2>&1 | tee`），或写进 FAST_REASON 供汇总行引用。
# 2026-09-28 事故：提示语进了 stdout，于是 `go test 变更全是 _test.go：… ./internal/cli`
# 拿到三个中文「包名」，报 malformed import path，闸门假红。
FAST_REASON=""
resolve_fast_packages() {   # 成功：stdout 打印包列表（./x 形式，每行一个）并返回 0；否则返回 1
  git -C "$ROOT" rev-parse --git-dir >/dev/null 2>&1 || { FAST_REASON="非 git 仓库"; echo "  $FAST_REASON" >&2; return 1; }

  local -a files=()
  local line
  while IFS= read -r line; do
    [ -n "$line" ] || continue
    files+=("${line:3}")   # 去掉 porcelain 的 3 字符状态前缀（" M a.go" → "a.go"）
  done < <(git -C "$ROOT" status --porcelain -uall 2>/dev/null)
  [ "${#files[@]}" -gt 0 ] || { FAST_REASON="工作区无变更"; echo "  $FAST_REASON" >&2; return 1; }

  local f d pkg
  for f in "${files[@]}"; do
    case "$f" in
      *" -> "*) FAST_REASON="变更含重命名，无法机械解析：$f"; echo "  $FAST_REASON" >&2; return 1 ;;
      internal/*|plugin/*|cmd/*) ;;
      *) FAST_REASON="变更含非 Go 源码路径（影响面无法用依赖闭包刻画）：$f"; echo "  $FAST_REASON" >&2; return 1 ;;
    esac
  done

  # 只改了 *_test.go：**不取依赖闭包**，只跑这些包自己。
  # 这是机械正确而非启发式——测试文件只进本包的测试二进制，不会被别的包 import，
  # 所以改测试不可能影响别的包的测试结果。没有这条，改 cli 的一个测试也会把
  # engine(289s)+cli(259s)+… 整个闭包拉起来，--fast 等于没快。
  local all_tests=1
  for f in "${files[@]}"; do case "$f" in *_test.go) ;; *) all_tests=0 ;; esac; done

  local -a seeds=()
  for f in "${files[@]}"; do
    d="$(dirname "$f")"
    pkg="$(cd "$ROOT/$d" 2>/dev/null && go list . 2>/dev/null)" || pkg=""
    [ -n "$pkg" ] || { FAST_REASON="$d 不是 Go 包"; echo "  $FAST_REASON" >&2; return 1; }
    seeds+=("$pkg")
  done
  if [ "$all_tests" = 1 ]; then
    echo "  变更全是 _test.go：只跑这些包自己（测试文件不被别的包 import，无需取依赖闭包）" >&2
    printf '%s\n' "${seeds[@]}" | sort -u | sed "s#^$(go list -m)/#./#"
    return 0
  fi

  # 反向依赖闭包：列出所有 import 了任一变更包的包（含传递），外加变更包自己。
  # go list -deps 方向相反（打印某包的依赖），故这里自己反查。
  go list -f '{{.ImportPath}}|{{join .Deps " "}}' ./... 2>/dev/null | \
    awk -v seeds="${seeds[*]}" '
      BEGIN { n = split(seeds, s, " "); for (i = 1; i <= n; i++) seed[s[i]] = 1 }
      {
        split($0, a, "|"); pkg = a[1]
        hit = 0
        nd = split(a[2], d, " ")
        for (i = 1; i <= nd; i++) if (d[i] in seed) { hit = 1; break }
        if (hit || (pkg in seed)) print pkg
      }' | sort -u | sed "s#^$(go list -m)/#./#"
}

MODULE="$(go list -m 2>/dev/null || echo gear)"

# ⑥ 判「这个包本次有没有被测到」：full 一律算测到（没测到就是失败），
# fast 只在选中列表里才算——没测到的包要明确打印「未覆盖」，不许静默跳过。
is_selected() {
  [ "$MODE" = full ] && return 0
  local p
  for p in $SELECTED_PKGS; do [ "$p" = "$1" ] && return 0; done
  return 1
}

# ⑤ 的实际执行：run_tests <serial 0|1> <verbose 0|1>
#   并行轮不带 -v：多包 t.Log 交错会让日志不可读；失败回退串行时才加 -v（那时才需要细节）。
run_tests() {
  local serial="$1" verbose="$2"
  local -a args=(-race -cover)
  [ "$serial" = 1 ] && args+=(-p 1)
  [ "$verbose" = 1 ] && args+=(-v)
  go test "${args[@]}" $SELECTED_PKGS 2>&1 | tee "$TESTOUT"
  return "${PIPESTATUS[0]}"
}

set +e
{
echo "══════════ Gate $(date '+%F %T') ══════════"

FAILED=0
SELECTED_PKGS="./..."
FAST_NOTE=""

# 选路放在日志块内：--fast 到底选了哪些包、为什么回退 full，必须留在日志里可追溯，
# 否则「这次闸门其实只测了 3 个包」这件事就成了没人知道的隐情。
if [ "$MODE" = fast ]; then
  echo
  echo "───── 选路 --fast：计算变更影响范围 ─────"
  # 不能用 FAST_OUT="$(resolve_fast_packages)"：命令替换是子 shell，函数里写的
  # FAST_REASON 传不出来。故重定向到文件（重定向不进子 shell），再读回来。
  FAST_SEL="$LOGDIR/.fast-selection"
  : > "$FAST_SEL"
  if resolve_fast_packages > "$FAST_SEL"; then
    SELECTED_PKGS="$(tr '\n' ' ' < "$FAST_SEL")"
    echo "  影响到的包：$(tr ' ' '\n' < "$FAST_SEL" | grep -c . | tr -d ' ') 个"
    sed 's/^/    /' "$FAST_SEL"
  else
    FAST_NOTE="升级为 full：$FAST_REASON"
    echo "  ⚠️  --fast $FAST_NOTE"
    MODE=full
  fi
fi

echo "模式：${MODE}$([ -n "$FAST_NOTE" ] && echo "（$FAST_NOTE）")  测试并发：$([ "$SERIAL" = 1 ] && echo "串行 -p 1" || echo "并行，失败回退串行")"

step() {   # step <标题> <命令...>
  local name="$1"; shift
  local t0
  t0="$(date +%s)"
  echo
  echo "───── $name [$(ts)] ─────"
  local out rc
  out="$("$@" 2>&1)"; rc=$?
  if [ -n "$out" ]; then echo "$out"; fi
  if [ "$rc" -eq 0 ]; then
    echo "✅ $name（$(dur "$t0")）"
  else
    echo "❌ $name (exit=$rc，$(dur "$t0"))"
    FAILED=1
  fi
  return 0
}

# ① ② 跟随选路范围：full = ./...，fast = 本次影响的包。
# 依据：build/vet 的结果是**包自身**的函数，不在闭包内的包没被本次改动碰过，结果不会变
# ——和⑤ 能靠 Go 测试缓存跳过是一个道理。
# 实测收益：① 编译 49s → 5s。② vet **对中心包没收益**（105s → 107s）——vet 分析的是
# **整棵依赖树**，而 cli/engine 的依赖树就约等于全仓；改叶子包（如某个 plugin sidecar）
# 时才会明显变快。另注：go vet 在本机没有有效缓存（刚跑完立刻再跑仍 103s），
# 所以这 100 秒是每次都要付的固定成本，别再指望靠选路省掉它。
SCOPE_LABEL="./..."
[ "$MODE" = fast ] && SCOPE_LABEL="本次影响的 $(tr ' ' '\n' <<<"$SELECTED_PKGS" | grep -c . | tr -d ' ') 个包"
step "① 编译 go build（${SCOPE_LABEL}）" go build $SELECTED_PKGS
step "② 静态 go vet（${SCOPE_LABEL}）"   go vet $SELECTED_PKGS

echo
echo "───── ③ 格式化 gofmt -l [$(ts)] ─────"
UNFMT="$(gofmt -l internal cmd plugin examples 2>&1)"
if [ -n "$UNFMT" ]; then
  echo "以下文件未格式化："
  echo "$UNFMT"
  echo "❌ gofmt (差异 ${#UNFMT} 字节；修： gofmt -w <文件>)"
  FAILED=1
else
  echo "✅ gofmt 零差异"
fi

echo
echo "───── ④ staticcheck ./...（固定版本 ${SC_VERSION}）─────"
# 优先用预装到 GOPATH/bin 的二进制（省掉每次 `go run @version` 的编译与版本解析）；
# 缺失或版本不符时自动装一次，装不上再回退 go run（保证「没有二进制≠跳过静态检查」）。
SC_BIN="$(go env GOPATH)/bin/staticcheck$(go env GOEXE)"
if ! "$SC_BIN" -version 2>/dev/null | grep -q "$SC_VERSION"; then
  echo "  未找到 ${SC_VERSION} 的 staticcheck，安装到 $(go env GOPATH)/bin ..."
  ( unset GOFLAGS; go install "honnef.co/go/tools/cmd/staticcheck@${SC_VERSION}" ) 2>&1 | tail -3
fi
if "$SC_BIN" -version 2>/dev/null | grep -q "$SC_VERSION"; then
  step_out="$("$SC_BIN" ./... 2>&1)"; sc_rc=$?
else
  echo "  ⚠️ 预装失败，回退 go run（慢，且需要联网）"
  step_out="$(go run "honnef.co/go/tools/cmd/staticcheck@${SC_VERSION}" ./... 2>&1)"; sc_rc=$?
fi
if [ -n "$step_out" ]; then echo "$step_out"; fi
if [ "$sc_rc" -eq 0 ]; then echo "✅ staticcheck"; else echo "❌ staticcheck (exit=$sc_rc)"; FAILED=1; fi

echo
echo "───── ⑤ 测试 go test -race -cover ${SELECTED_PKGS} [$(ts)] ─────"
T5="$(date +%s)"
if [ "$SERIAL" = 1 ]; then
  run_tests 1 1
  RC=$?
  if [ "$RC" -eq 0 ]; then echo "✅ go test（串行）"; else echo "❌ go test（串行，exit=$RC）"; FAILED=1; fi
else
  run_tests 0 0
  RC=$?
  if [ "$RC" -eq 0 ]; then
    echo "✅ go test（并行）"
  else
    # 历史实证（2026-09-14 / 09-18）：并行下出现过「静默失败 / 沙箱并发 I/O 损坏误报语法错」。
    # 所以并行红**不直接判死**，用 -p 1 再跑一遍作为最终判定。
    echo "⚠️  并行轮失败（exit=$RC）——可能是包间争用，用 -p 1 串行重跑确认（带 -v）"
    run_tests 1 1
    RC=$?
    if [ "$RC" -eq 0 ]; then
      echo "✅ go test（串行重跑通过：上轮判为环境争用，非代码问题）"
    else
      echo "❌ go test（串行重跑仍失败，exit=$RC）"
      FAILED=1
    fi
  fi
fi
echo "   ⏱ ⑤ 段耗时 $(dur "$T5")"

echo
echo "───── ⑥ 覆盖率门槛（≥ ${COVER_MIN}%）─────"
T6="$(date +%s)"
# 2026-09-28 起：**不再逐包重跑** go test。⑤ 带了 -cover，输出里每包都有
# `coverage: X% of statements`，直接解析即可——旧实现为此多跑一遍实测 1029s，
# 而解析出来的数值与它逐包完全一致（13/13 核对通过，见工程方法 §5.3）。
# 取不到数值 = 该包没跑成功 ⇒ fail-closed 判失败，绝不静默跳过。
# 一次 awk 把「包名 → 覆盖率」抽进关联数组，之后纯 bash 查表。
# 原来每包起 grep+grep+awk 三个子进程：Windows 上每进程约 0.3s，13 包 ≈ 16s 全是进程开销。
declare -A COVER
while read -r _p _c; do
  [ -n "$_p" ] && COVER["$_p"]="$_c"
done < <(awk '/^(ok|FAIL)[[:space:]]/ {
    for (i = 1; i <= NF; i++)
      if ($i == "coverage:") { p = $(i + 1); sub("%", "", p); print $2, p; break }
  }' "$TESTOUT")

COVER_SKIPPED=0
for pkg in $COVER_PACKAGES; do
  rel="${pkg#./}"                                  # ./internal/engine → internal/engine
  pct="${COVER[${MODULE}/${rel}]:-}"
  if [ -z "$pct" ]; then
    if is_selected "$pkg"; then
      echo "  ❌ $pkg 取不到覆盖率（该包未通过测试或输出异常）"
      FAILED=1
    else
      echo "  ⏭  $pkg 本次 --fast 未覆盖（变更闭包不含它）"
      COVER_SKIPPED=$((COVER_SKIPPED + 1))
    fi
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
echo "   ⏱ ⑥ 段耗时 $(dur "$T6")"

echo
echo "───── ⑦ 文档↔代码审计 tools/doc_audit.py [$(ts)] ─────"
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
echo "───── ⑧ extensions 必填一致性 tools/extensions_audit.py [$(ts)] ─────"
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
echo "───── ⑨ 应用可移植性审计 tools/portable_audit.py [$(ts)] ─────"
# apps/ 与 examples/ 下的应用定义不得硬编码机器相关绝对路径（D:/gadern/gear/... 这类），
# 否则仓库拷到别的机器即跑不起来——这是 2026-09-25 实测的可移植性根因（mindstorm 25 处）。
# 内核已提供可移植锚点（db dsn 相对库名→GEAR_APP_DATA、process entry 裸名→exe 目录、
# ext.serve dir 相对安装根→InstallRoot），应用改用相对写法即跨机器可用。
if command -v python >/dev/null 2>&1; then
  python tools/portable_audit.py
  rc=$?
  if [ "$rc" -eq 0 ]; then echo "✅ portable_audit（退出码 0）"; else echo "❌ portable_audit（exit=$rc）"; FAILED=1; fi
else
  echo "⚠️ 未找到 python，跳过 portable_audit"
fi

echo
echo "───── ⑩ 需求薄索引引用审计 tools/requirements_audit.py [$(ts)] ─────"
# REQUIREMENTS.md 是薄索引（一句话行为 + 状态 + 落点）：落点列引用的测试文件 / 测试函数
# 必须真实存在，否则索引就退化成另一份会漂移的文档。2026-09-26 首跑即抓出 8 处真缺口。
# 2026-09-28：仓库遍历由 rglob 改 os.walk 剪枝（node_modules 2.3 万文件），26s → 1s。
if command -v python >/dev/null 2>&1; then
  python tools/requirements_audit.py
  rc=$?
  if [ "$rc" -eq 0 ]; then echo "✅ requirements_audit（退出码 0）"; else echo "❌ requirements_audit（exit=$rc）"; FAILED=1; fi
else
  echo "⚠️ 未找到 python，跳过 requirements_audit"
fi

echo
echo "══════════ Gate 汇总 ══════════"
echo "耗时：$(( $(date +%s) - GATE_START ))s   模式：${MODE}$([ -n "$FAST_NOTE" ] && echo "（$FAST_NOTE）")"
if [ "$COVER_SKIPPED" -gt 0 ]; then
  echo "⏭ 本次 --fast 有 ${COVER_SKIPPED} 个内核包未测（见⑥）——提交前必须跑 bash scripts/gate.sh（full）"
fi
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
