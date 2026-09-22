// Package cmdline 把「一行命令字符串」翻译成可直接执行的 *exec.Cmd。
//
// 存在的理由（2026-09-21 实测缺陷）：引擎此前用 `exec.Command("sh","-c",entry)` 拉起
// process 型 Tool（含 4 个 service 侧车），而 Windows 的系统 PATH 里**通常没有 sh**
// （Git for Windows 只在 `Git\cmd` 放 git.exe，sh 在 `Git\usr\bin`）。WorkBuddy 这类
// GUI 宿主派生的 gear 继承系统 PATH ⇒ 找不到 sh ⇒ 拉不起任何侧车 ⇒ **任何应用都起不来**，
// 报错 `exec: "sh": executable file not found in %PATH%`。
//
// 更根本的是：gear 自己生成的 entry 根本不需要 shell —— `COORD_ADDR=127.0.0.1:8095
// plugin/serve-sidecar.exe` 只是「环境变量前缀 + 可执行文件 + 参数」，用 argv + Env 就能
// 精确表达。把这种行交给 shell，等于把语义解释权交给一个可能不存在的程序，还额外引入
// 引号/通配/展开等一整类解析歧义（见 GEAR_工程方法 红线 17 的历史踩坑）。
//
// 因此本包的分工是：
//   - **原生路径（默认）**：自行切词（单/双引号、反斜杠转义、`NAME=value` 前缀）后直接 exec。
//   - **shell 兜底（仅在必须时）**：命令行含**未加引号**的 shell 元字符（管道、重定向、
//     `&&`、通配、变量展开…）时，才去找一个 POSIX shell 解释——这类语义无法用 argv 表达，
//     且失败时 fail-closed 报错，绝不静默降级成另一种解释器。
package cmdline

import (
	"fmt"
	"strings"
)

// Spec 是一行命令的解析结果。
type Spec struct {
	// Argv 是「可执行文件 + 参数」。NeedsShell 为真时为空（该行交给 shell 解释）。
	Argv []string
	// Env 是行首的 POSIX 风格环境变量赋值（`NAME=value cmd args`），保持出现顺序。
	// 仅当赋值 token **不以引号开头**时才算数（与 POSIX 一致：`'A=B'` 是命令名而非赋值）。
	Env []string
	// NeedsShell 为真表示这行含未加引号的 shell 元字符，只能用 shell 解释。
	NeedsShell bool
	// Meta 记录触发 NeedsShell 的元字符（去重，按首次出现顺序）。供错误信息指认病因。
	Meta []string
}

// shellMeta 是「未加引号时只有 shell 才能解释」的字符。
//
// 刻意**不含** `{` `}` `[` `]` `=` `!`：它们在 JSON / 参数值里极常见（`echo {"v":1}`、
// `--set a=[1]`），把它们当成元字符，等于为一行普通命令白白引入「必须有 shell」的前置条件
// ——而本包存在的全部理由就是拆掉这个前置条件。
//
// ⚠ 不触发 shell ≠ 引号不参与词法：`{"v":"1"}` 里的双引号仍按 POSIX 规则被**去引号**
// （与 `sh -c` 完全一致），所以「用单引号包住 JSON」仍是必须遵守的写法
// （GEAR_工程方法 红线 17），只是这条规则现在由 gear 自己执行，不再依赖外部 sh 的存在。
var shellMeta = map[rune]bool{
	'|': true, '&': true, ';': true, '<': true, '>': true,
	'(': true, ')': true, '$': true, '`': true,
	'*': true, '?': true,
	'\n': true, '\r': true,
}

// token 是切词中间结果：文本 + 「是否以引号开头」（影响赋值识别与 `~` 展开判定）。
type token struct {
	text      string
	fromQuote bool
}

// Parse 把一行命令切成 argv + 环境变量前缀，并判定是否必须交给 shell。
//
// 语法支持（POSIX 的一个子集，覆盖 gear 自身生成的全部 entry）：
//   - 空白分隔 token；单引号内字面量；双引号内除 `\"` `\\` `\$` “ \` “ 外字面量；
//   - 引号外 `\` **仅在**后跟空白 / 引号 / `$` / 反引号时转义，其余一律原样保留
//     （故 Windows 路径 `D:\a\b\x.exe`、UNC 路径 `\\srv\share` 都不再被破坏）；
//   - 行首（连续）`NAME=value` 识别为环境变量赋值。
//
// 元字符一律按 POSIX 语义处理：引号内的 `*` `|` `;` 是字面量（不触发 shell），
// 双引号内的 `$` 与反引号仍会被 shell 展开（触发 shell），引号外的元字符触发 shell。
//
// 引号未闭合返回错误（fail-closed）——不要静默按字面量处理，那会把一条明显的笔误
// 变成一个「看起来跑通了但语义不对」的命令。
func Parse(line string) (Spec, error) {
	var (
		spec    Spec
		toks    []token
		cur     strings.Builder
		hasToks bool // 当前 token 是否已有内容（区分空 token "" 与「尚未开始」）
		startQ  bool // 当前 token 是否**以引号开头**
		seen    = map[rune]bool{}
	)

	markMeta := func(r rune) {
		spec.NeedsShell = true
		if !seen[r] {
			seen[r] = true
			spec.Meta = append(spec.Meta, string(r))
		}
	}
	flush := func() {
		if !hasToks {
			return
		}
		toks = append(toks, token{text: cur.String(), fromQuote: startQ})
		cur.Reset()
		hasToks, startQ = false, false
	}

	rs := []rune(line)
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		switch {
		case c == ' ' || c == '\t':
			flush()

		case c == '\'':
			// 单引号：内部一切字面量（POSIX：单引号内连反斜杠都不转义）。
			if !hasToks {
				startQ = true
			}
			hasToks = true
			i++
			for i < len(rs) && rs[i] != '\'' {
				cur.WriteRune(rs[i])
				i++
			}
			if i >= len(rs) {
				return Spec{}, fmt.Errorf("命令行单引号未闭合: %q", line)
			}

		case c == '"':
			if !hasToks {
				startQ = true
			}
			hasToks = true
			i++
			for i < len(rs) && rs[i] != '"' {
				// 双引号内仅这四个字符可被反斜杠转义（POSIX）。
				if rs[i] == '\\' && i+1 < len(rs) {
					switch rs[i+1] {
					case '"', '\\', '$', '`':
						cur.WriteRune(rs[i+1])
						i += 2
						continue
					}
				}
				// 双引号内的 $ 与反引号**仍会被展开**，故必须走 shell；
				// 而 * | ; < > 等在双引号内是字面量，不能误判。
				if rs[i] == '$' || rs[i] == '`' {
					markMeta(rs[i])
				}
				cur.WriteRune(rs[i])
				i++
			}
			if i >= len(rs) {
				return Spec{}, fmt.Errorf("命令行双引号未闭合: %q", line)
			}

		case c == '\\':
			// 引号外的反斜杠：只在「不转义就表达不出」时当转义符，其余**原样保留**。
			//
			// 存在的理由（2026-09-22 实测缺陷）：原先无条件把 `\x` 折成字面量 x，于是
			// Windows 绝对路径 `D:\gadern\gear\build\gear.exe` 被吃成
			// `D:gaderngearbuildgear.exe`，报错还是误导性的
			// `executable file not found in %PATH%`（指向 PATH，与真因无关）。
			// 路径分隔符是本项目 process entry 的高频内容，而 POSIX 里真正需要引号外
			// 转义的只有空白与引号（外加触发元字符判定的 `$`、反引号）；按这个最小集处理，
			// 两边都不牺牲。**刻意不收 `\` 自身**——`\\` 折半会破坏 UNC 路径。
			if i+1 < len(rs) && escableOutsideQuote(rs[i+1]) {
				cur.WriteRune(rs[i+1])
				hasToks = true
				i++
				continue
			}
			// 行末孤立的反斜杠也保留字面量而非报错：`D:\` 这种写法在 Windows 上
			// 是合法路径前缀，把它当笔误拒掉得不偿失。
			cur.WriteRune(c)
			hasToks = true

		default:
			if shellMeta[c] {
				markMeta(c)
			}
			// `~` 仅在不加引号的 token 开头才做家目录展开；其余位置是字面量。
			if c == '~' && !hasToks && !startQ {
				markMeta(c)
			}
			cur.WriteRune(c)
			hasToks = true
		}
	}
	flush()

	// 行首连续的 `NAME=value` 归入 Env（`NAME=` 空值也算）。
	i := 0
	for ; i < len(toks); i++ {
		if toks[i].fromQuote || !isEnvAssign(toks[i].text) {
			break
		}
		spec.Env = append(spec.Env, toks[i].text)
	}
	for ; i < len(toks); i++ {
		spec.Argv = append(spec.Argv, toks[i].text)
	}

	// argv[0] 是 shell 内建命令时，原生路径必然失败（`exit` / `cd` / `export` 不是可执行
	// 文件，磁盘上根本不存在），必须交给 shell。这类 entry 在既有的 service/监管测试里
	// 就有（`exit 0` 用来模拟「起来就退出」），漏判会让本来合法的写法变成
	// 「可执行文件不可用」——把一次行为变更伪装成一个莫名其妙的报错。
	if !spec.NeedsShell && len(spec.Argv) > 0 && shellBuiltins[spec.Argv[0]] {
		spec.NeedsShell = true
		spec.Meta = append(spec.Meta, spec.Argv[0]+"(shell 内建)")
	}

	// 含元字符的行必然整行交给 shell：此时 argv / env 是我们「半懂不懂」的产物，
	// 留着只会诱导调用方拿去做判断——清空，让唯一真相源只剩「交原行给 shell」。
	if spec.NeedsShell {
		spec.Argv, spec.Env = nil, nil
	}
	return spec, nil
}

// shellBuiltins 是**必须由 shell 解释**的命令名，分两类：
//
//  1. 磁盘上根本不存在同名可执行文件的纯内建：`exit` / `cd` / `export` / `set` …
//     交给原生路径必然报「可执行文件不可用」——而 `exit 0` 这类写法在既有测试
//     （模拟「起来就退出」的 service）里就在用，漏判会把一次行为变更伪装成一个
//     莫名其妙的报错。
//  2. 既存在于磁盘、又是内建，且**内建语义才是 entry 作者预期**的：`echo` / `printf`。
//     这条不是洁癖：`echo '{"v":"1"}'` 是本项目文档（红线 17）与测试夹具里成型已久的
//     「让进程吐一行 JSON」写法，靠的正是 shell 内建的引号处理；一旦落到磁盘上的
//     `echo.exe`（Git 的 MSYS 版本），Windows 命令行转义与 MSYS 的 argv 规则叠加后
//     引号会被吃掉，输出变成空行——比不修还糟。
//
// 刻意**不收** `test` / `[` / `true` / `false` / `pwd` / `kill`：它们的内建与外部程序
// 语义等价，交给原生路径更精确（也少一次「必须有 shell」的机会）。
var shellBuiltins = map[string]bool{
	":": true, ".": true, "source": true, "cd": true, "exec": true, "eval": true,
	"exit": true, "set": true, "unset": true, "export": true, "readonly": true,
	"shift": true, "return": true, "break": true, "continue": true,
	"trap": true, "umask": true, "wait": true, "alias": true, "unalias": true,
	"local": true, "declare": true, "typeset": true, "let": true, "getopts": true,
	"echo": true, "printf": true,
}

// escableOutsideQuote 判定「引号外的反斜杠 + 该字符」是否构成转义。
//
// 只收四类**不转义就表达不出**的字符：
//   - 空白（` `、tab）：不转义就会被当成 token 分隔符；
//   - 单/双引号：不转义就会开启/关闭引号状态；
//   - `$`、反引号：不转义就会被判为 shell 元字符（整行走 shell）。
//
// **刻意不收 `\` 自身**：POSIX 语义是 `\\` → `\`，但那会把 UNC 路径
// （`\\srv\share`）与前缀 `\\` 的 Windows 路径折半；本包的头号使用场景正是
// Windows 上的可执行文件路径，故宁可与 POSIX 在此处分歧。
func escableOutsideQuote(r rune) bool {
	switch r {
	case ' ', '\t', '\'', '"', '$', '`':
		return true
	}
	return false
}

// isEnvAssign 判定一个 token 是否形如 `NAME=value`（NAME 为合法标识符）。
// 只做**行首**判定，故 `--set=a=b` 这类参数不会被误认。
func isEnvAssign(tok string) bool {
	eq := strings.IndexByte(tok, '=')
	if eq <= 0 {
		return false
	}
	for i, r := range tok[:eq] {
		if r == '_' || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') ||
			(i > 0 && r >= '0' && r <= '9') {
			continue
		}
		return false
	}
	return true
}
