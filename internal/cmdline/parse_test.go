package cmdline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// ───────── Parse：切词 / 引号 / 环境变量前缀 / 元字符判定 ─────────

// TestParseSplitsLeadingEnvAndQuotes 锁定「gear 自己生成的 entry 不需要 shell」这条前提。
//
// 最关键的一行是 service 侧车那条：`COORD_ADDR='127.0.0.1:8095' plugin/serve-sidecar.exe`
// —— 它是引擎实际拉起的命令形态。若这里判成 NeedsShell，就等于把「能不能起应用」再次
// 押在一个可能不存在的 sh 上（2026-09-21 实测缺陷的根因）。
func TestParseSplitsLeadingEnvAndQuotes(t *testing.T) {
	cases := []struct {
		name       string
		line       string
		wantArgv   []string
		wantEnv    []string
		wantShell  bool
		wantMetaIn string // 期望 Meta 里出现（子串匹配），仅 wantShell 时检查
	}{
		{
			name:      "service 侧车：环境变量前缀 + 引号值",
			line:      "COORD_ADDR='127.0.0.1:8095' plugin/serve-sidecar.exe",
			wantArgv:  []string{"plugin/serve-sidecar.exe"},
			wantEnv:   []string{"COORD_ADDR=127.0.0.1:8095"},
			wantShell: false,
		},
		{
			name:      "多个前缀赋值",
			line:      "A=1 B='x y' cmd -f",
			wantArgv:  []string{"cmd", "-f"},
			wantEnv:   []string{"A=1", "B=x y"},
			wantShell: false,
		},
		{
			name:      "空值赋值也算（与 POSIX 一致）",
			line:      "A= cmd",
			wantArgv:  []string{"cmd"},
			wantEnv:   []string{"A="},
			wantShell: false,
		},
		{
			name:      "带引号开头的 token 不是赋值（POSIX：'A=B' 是命令名）",
			line:      "'A=B' cmd",
			wantArgv:  []string{"A=B", "cmd"},
			wantEnv:   nil,
			wantShell: false,
		},
		{
			name:      "非行首的 x=y 只是普通参数",
			line:      "cmd --set=a=b",
			wantArgv:  []string{"cmd", "--set=a=b"},
			wantEnv:   nil,
			wantShell: false,
		},
		{
			name: "JSON 的花括号不是 shell 元字符（不把一行普通命令推给 shell）",
			// ⚠ 不触发 shell ≠ 引号不参与词法：`{"v":"1"}` 的双引号仍按 POSIX 被去引号，
			// 与 `sh -c` 行为一致——所以「用单引号包 JSON」仍是必须的写法（红线 17）。
			line:      `tool {"v":"1"}`,
			wantArgv:  []string{"tool", "{v:1}"},
			wantEnv:   nil,
			wantShell: false,
		},
		{
			name:      "单引号包住的 JSON 原样保留（红线 17 的写法）",
			line:      `tool '{"v":"1"}'`,
			wantArgv:  []string{"tool", `{"v":"1"}`},
			wantEnv:   nil,
			wantShell: false,
		},
		{
			name:      "双引号内保留空格，反斜杠转义生效",
			line:      `tool "a b" "c\"d"`,
			wantArgv:  []string{"tool", "a b", `c"d`},
			wantEnv:   nil,
			wantShell: false,
		},
		{
			name:      "反斜杠转义空格",
			line:      `tool a\ b`,
			wantArgv:  []string{"tool", "a b"},
			wantEnv:   nil,
			wantShell: false,
		},
		{
			// 2026-09-22 实测缺陷的回归锁：原先无条件把 `\x` 折成字面量 x，于是
			// `D:\gadern\gear\build\gear.exe` 被吃成 `D:gaderngearbuildgear.exe`，
			// 报错还是误导性的 `executable file not found in %PATH%`。
			name:      "Windows 绝对路径的反斜杠原样保留",
			line:      `D:\gadern\gear\build\gear.exe serve --addr 127.0.0.1:8788`,
			wantArgv:  []string{`D:\gadern\gear\build\gear.exe`, "serve", "--addr", "127.0.0.1:8788"},
			wantEnv:   nil,
			wantShell: false,
		},
		{
			name:      "含 t/n 等字母的路径段不被当转义序列",
			line:      `D:\temp\new\x.txt --out D:\temp\out.txt`,
			wantArgv:  []string{`D:\temp\new\x.txt`, "--out", `D:\temp\out.txt`},
			wantEnv:   nil,
			wantShell: false,
		},
		{
			// `\\` 刻意不折半：UNC 路径是 Windows 上的真实写法，折半会指向错误位置。
			name:      "UNC 路径的双反斜杠开头不被折半",
			line:      `\\srv\share\tool.exe arg`,
			wantArgv:  []string{`\\srv\share\tool.exe`, "arg"},
			wantEnv:   nil,
			wantShell: false,
		},
		{
			name:      "引号外仍可转义引号与空白",
			line:      `tool a\"b c\ d`,
			wantArgv:  []string{"tool", `a"b`, "c d"},
			wantEnv:   nil,
			wantShell: false,
		},
		{
			name:      "空引号是合法的空参数",
			line:      `tool ""`,
			wantArgv:  []string{"tool", ""},
			wantEnv:   nil,
			wantShell: false,
		},
		{
			name:      "引号内的元字符是字面量，不触发 shell",
			line:      `tool 'a|b*c'`,
			wantArgv:  []string{"tool", "a|b*c"},
			wantEnv:   nil,
			wantShell: false,
		},
		{
			name:       "未加引号的重定向触发 shell",
			line:       "cat < in.txt",
			wantShell:  true,
			wantMetaIn: "<",
		},
		{
			name:       "未加引号的管道触发 shell",
			line:       "a | b",
			wantShell:  true,
			wantMetaIn: "|",
		},
		{
			name:       "未加引号的变量展开触发 shell",
			line:       "tool $HOME",
			wantShell:  true,
			wantMetaIn: "$",
		},
		{
			name:       "未加引号的通配触发 shell",
			line:       "tool *.log",
			wantShell:  true,
			wantMetaIn: "*",
		},
		{
			name:       "分号触发 shell",
			line:       "sleep 0.3; exit 1",
			wantShell:  true,
			wantMetaIn: ";",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			spec, err := Parse(c.line)
			if err != nil {
				t.Fatalf("Parse(%q) 报错: %v", c.line, err)
			}
			if spec.NeedsShell != c.wantShell {
				t.Fatalf("NeedsShell: 期望 %v 实际 %v（meta=%v），line=%q",
					c.wantShell, spec.NeedsShell, spec.Meta, c.line)
			}
			if c.wantShell {
				if len(spec.Argv) != 0 || len(spec.Env) != 0 {
					t.Fatalf("判为需 shell 时 argv/env 必须清空（否则等于给调用方留了半懂的产物）: %+v", spec)
				}
				if c.wantMetaIn != "" && !containsSub(spec.Meta, c.wantMetaIn) {
					t.Fatalf("Meta 应含 %q，实际 %v", c.wantMetaIn, spec.Meta)
				}
				return
			}
			if !equalStrings(spec.Argv, c.wantArgv) {
				t.Fatalf("argv: 期望 %#v 实际 %#v", c.wantArgv, spec.Argv)
			}
			if !equalStrings(spec.Env, c.wantEnv) {
				t.Fatalf("env: 期望 %#v 实际 %#v", c.wantEnv, spec.Env)
			}
		})
	}
}

// TestParseRejectsUnterminatedQuote：引号未闭合必须报错（fail-closed）。
// 静默按字面量处理会把一个明显的笔误变成「看起来跑通了、语义却不对」的命令。
//
// 注：行末孤立的反斜杠**不再**属于「未闭合」——见 TestParseKeepsTrailingBackslashLiteral。
func TestParseRejectsUnterminatedQuote(t *testing.T) {
	for _, line := range []string{`tool "abc`, `tool 'abc`} {
		if _, err := Parse(line); err == nil {
			t.Fatalf("Parse(%q) 应报错（引号未闭合）", line)
		}
	}
}

// TestParseKeepsTrailingBackslashLiteral：行末孤立的反斜杠保留字面量。
//
// 2026-09-22 行为变更：`D:\` 这类 Windows 路径前缀是合法写法，把它当「转义未跟字符」
// 的笔误拒掉会误伤路径。引号外的转义语义已收窄到空白/引号/`$`/反引号（见
// escableOutsideQuote），孤立反斜杠不再有转义含义，故也不需要报错。
func TestParseKeepsTrailingBackslashLiteral(t *testing.T) {
	spec, err := Parse(`tool abc\`)
	if err != nil {
		t.Fatalf("行末孤立反斜杠不应报错: %v", err)
	}
	if !equalStrings(spec.Argv, []string{"tool", `abc\`}) {
		t.Fatalf("行末反斜杠应原样保留，实际 %#v", spec.Argv)
	}
}

// TestParseTreatsShellBuiltinsAsShellRequired：`exit` 这类纯内建磁盘上没有可执行文件，
// 交给原生路径必然报「可执行文件不可用」；`echo` 虽有同名 exe，但 entry 作者要的是
// shell 内建的引号语义（`echo '{"v":1"}'` 是本项目成型已久的「吐一行 JSON」写法）。
func TestParseTreatsShellBuiltinsAsShellRequired(t *testing.T) {
	for _, line := range []string{"exit 0", `echo '{"v":"1"}'`, `echo {"v":"1"}`, "cd /tmp", "export A=1"} {
		spec, err := Parse(line)
		if err != nil {
			t.Fatalf("Parse(%q) 报错: %v", line, err)
		}
		if !spec.NeedsShell {
			t.Fatalf("Parse(%q) 应判为需要 shell（%v）", line, spec)
		}
	}
	// 反向护栏：普通命令不能被误判成需要 shell，否则等于把原生路径架空。
	for _, line := range []string{"cat", "node server.js", "tool --flag a"} {
		spec, err := Parse(line)
		if err != nil {
			t.Fatalf("Parse(%q) 报错: %v", line, err)
		}
		if spec.NeedsShell {
			t.Fatalf("Parse(%q) 不应要求 shell（meta=%v）", line, spec.Meta)
		}
	}
}

// ───────── resolveShell：解析顺序与 fail-closed ─────────

func TestResolveShellPrefersExplicitThenPath(t *testing.T) {
	dir := t.TempDir()
	explicit := filepath.Join(dir, "my-sh")
	writeFile(t, explicit, "#!/bin/sh\n")
	inPath := filepath.Join(dir, "sh-in-path")
	writeFile(t, inPath, "#!/bin/sh\n")

	// 1) GEAR_SHELL 显式指定优先。
	got, err := resolveShell(explicit, func(string) (string, error) { return inPath, nil }, nil)
	if err != nil {
		t.Fatalf("显式指定应成功: %v", err)
	}
	if got.Path != explicit {
		t.Fatalf("显式指定应优先，实际 %q", got.Path)
	}
	if got.Dir != dir {
		t.Fatalf("Shell.Dir 应为所在目录，实际 %q", got.Dir)
	}

	// 2) 显式指定但文件不存在 → fail-closed（不静默退回别的 shell）。
	if _, err := resolveShell(filepath.Join(dir, "nope"), nil, nil); err == nil {
		t.Fatal("GEAR_SHELL 指向不存在的文件应报错")
	}

	// 3) 无显式指定 → PATH 里的 sh。
	got, err = resolveShell("", func(name string) (string, error) {
		if name == "sh" {
			return inPath, nil
		}
		return "", errors.New("not found")
	}, nil)
	if err != nil {
		t.Fatalf("PATH 里的 sh 应被采用: %v", err)
	}
	if got.Path != inPath {
		t.Fatalf("应采用 PATH 里的 sh，实际 %q", got.Path)
	}
}

// TestResolveShellFallsBackToGitDerived：装上 Git for Windows 但 usr\bin 不在 PATH 时，
// 仍应能由 `git` 的位置反推出 sh —— 这正是本机（WorkBuddy 派生 gear）的真实处境。
func TestResolveShellFallsBackToGitDerived(t *testing.T) {
	dir := t.TempDir()
	git := filepath.Join(dir, "cmd", "git.exe")
	sh := filepath.Join(dir, "usr", "bin", "sh.exe")
	writeFile(t, git, "stub")
	writeFile(t, sh, "stub")

	lookup := func(name string) (string, error) {
		if name == "git" {
			return git, nil
		}
		return "", errors.New("not found")
	}
	got, err := resolveShell("", lookup, nil)
	if runtime.GOOS != "windows" {
		// 类 Unix 上 sh 必在 PATH；git 反推是 Windows 专属的启发式。
		if err == nil {
			t.Skip("非 Windows：本用例只覆盖 git 反推路径")
		}
		return
	}
	if err != nil {
		t.Fatalf("应由 git 位置反推出 sh: %v", err)
	}
	if got.Path != sh {
		t.Fatalf("应取 <gitRoot>/usr/bin/sh.exe，实际 %q", got.Path)
	}
}

// TestResolveShellFailsClosed：PATH / git / 候选目录全落空 → 返回 ErrNoShell，
// 让调用方（Command）能被 errors.Is 判定并给出可执行的下一步动作。
func TestResolveShellFailsClosed(t *testing.T) {
	notFound := func(string) (string, error) { return "", errors.New("not found") }
	_, err := resolveShell("", notFound, []string{filepath.Join(t.TempDir(), "nope")})
	if !errors.Is(err, ErrNoShell) {
		t.Fatalf("应返回 ErrNoShell，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "GEAR_SHELL") {
		t.Fatalf("错误信息应给出可执行的出路（GEAR_SHELL / 改写命令），实际 %q", err)
	}
}

// ───────── Command：原生路径不依赖 shell（本次修复的核心承诺） ─────────

// TestCommandNativePathNeedsNoShell 是本轮修复的**红线锁**。
//
// 做法：把 GEAR_SHELL 指到一个不存在的文件（等价于「本机根本没有 sh」），然后要求
// 一条原生可解析的命令**照样能构造并跑起来**。修复前 gear 无条件走 `sh -c`，
// 在这种环境下必然 `exec: "sh": executable file not found in %PATH%`——
// 也就是 WorkBuddy 派生 gear 时「任何应用都起不来」的真因。
func TestCommandNativePathNeedsNoShell(t *testing.T) {
	disableShell(t)

	cmd, err := Command(context.Background(), helperEntry(t, "COORD_ADDR=127.0.0.1:8099"))
	if err != nil {
		t.Fatalf("原生 entry 不应依赖 shell，却失败: %v", err)
	}
	out := runHelper(t, cmd)
	if out["coord"] != "127.0.0.1:8099" {
		t.Fatalf("行首的 NAME=value 应落到子进程环境，实际 %v", out["coord"])
	}
	if in, _ := out["stdin"].(map[string]interface{}); in["k"] != "v" {
		t.Fatalf("stdin 应原样送达子进程，实际 %v", out["stdin"])
	}
}

// TestCommandShellPathFailsClosedWithoutShell：需要 shell 的 entry 在没有 shell 时
// 必须报出 ErrNoShell（而不是悄悄换个解释器，或报一个与病因无关的 exec 错误）。
func TestCommandShellPathFailsClosedWithoutShell(t *testing.T) {
	disableShell(t)
	if _, err := Command(context.Background(), "echo '{\"v\":1}' | cat"); !errors.Is(err, ErrNoShell) {
		t.Fatalf("需要 shell 的 entry 应返回 ErrNoShell，实际 %v", err)
	}
	if _, err := Command(context.Background(), `echo '{"v":1}'`); !errors.Is(err, ErrNoShell) {
		t.Fatalf("shell 内建命令应返回 ErrNoShell，实际 %v", err)
	}
}

// TestCommandQuotedArgSurvivesNativeExec：加引号的可执行文件路径与参数必须原样送达。
//
// 这条锁的是「绝对路径可能含空格」——entry 是以空格分词的命令行，路径不转义就会被拆成
// 两个参数（`C://Program Files\...` 是最常见的触发形态）。含空格的**参数**由
// TestParseSplitsLeadingEnvAndQuotes 在切词层覆盖；这里验的是真的能跑起来。
func TestCommandQuotedArgSurvivesNativeExec(t *testing.T) {
	disableShell(t)
	cmd, err := Command(context.Background(), helperEntry(t, ""))
	if err != nil {
		t.Fatalf("构造命令失败: %v", err)
	}
	got := runHelper(t, cmd)["argv"]
	want := []interface{}{"-test.run=TestCmdlineHelperProcess"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("参数应原样送达: 期望 %v 实际 %v", want, got)
	}
}

// ───────── 测试辅助 ─────────

// disableShell 把本进程视角的 shell 打成「不存在」，并在用例结束后还原缓存。
func disableShell(t *testing.T) {
	t.Helper()
	t.Setenv("GEAR_SHELL", filepath.Join(t.TempDir(), "no-such-sh.exe"))
	resetShellCache()
	t.Cleanup(resetShellCache)
}

// helperEntry 生成一条「以本测试二进制当外部进程」的 entry（可选前缀环境变量赋值）。
//
// 用受测二进制自身当被测进程，是为了让「原生 exec」这条路径能在**不依赖任何外部程序**
// 的前提下被真跑一遍：任何依赖 `cat` / `echo` / `sleep` 的夹具都会把「本机有没有这些
// 工具」重新引回测试，正是我们要拆掉的那种隐性前提。
func helperEntry(t *testing.T, prefix string) string {
	t.Helper()
	t.Setenv("GEAR_CMDLINE_HELPER", "1")
	entry := strconv.Quote(os.Args[0]) + " -test.run=TestCmdlineHelperProcess"
	if prefix != "" {
		entry = prefix + " " + entry
	}
	return entry
}

// runHelper 把 input 打进 stdin 跑起来，并解析子进程 stdout 的末行 JSON。
func runHelper(t *testing.T, cmd *exec.Cmd) map[string]interface{} {
	t.Helper()
	cmd.Stdin = strings.NewReader(`{"k":"v"}`)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("子进程执行失败: %v (stderr=%s)", err, stderr.String())
	}
	lines := strings.Split(strings.TrimRight(stdout.String(), "\n"), "\n")
	var out map[string]interface{}
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &out); err != nil {
		t.Fatalf("解析子进程输出失败: %v (stdout=%q)", err, stdout.String())
	}
	return out
}

// TestCmdlineHelperProcess 不是被测对象，而是 TestCommand* 用例里的「外部进程」。
// 未带标记环境变量时它什么都不做（正常跑包测试时即为此情形）。
func TestCmdlineHelperProcess(t *testing.T) {
	if os.Getenv("GEAR_CMDLINE_HELPER") != "1" {
		return
	}
	var in interface{}
	_ = json.NewDecoder(os.Stdin).Decode(&in)
	out, _ := json.Marshal(map[string]interface{}{
		"coord": os.Getenv("COORD_ADDR"),
		"argv":  os.Args[1:],
		"stdin": in,
	})
	fmt.Println(string(out))
	// 直接退出：不能让 testing 框架再往 stdout 补一行 "PASS"，
	// 否则调用方读到的「末行」就不是我们要验的那一行 JSON 了。
	os.Exit(0)
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func containsSub(xs []string, sub string) bool {
	for _, x := range xs {
		if strings.Contains(x, sub) {
			return true
		}
	}
	return false
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
