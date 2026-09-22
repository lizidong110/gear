package cmdline

// exec_test.go：Command / LookExecutable / exeCandidates / childEnv / setEnv / augmentPath
// 的**分支与错误路径**。
//
// 为什么值得单独补：parse_test.go 覆盖的是「一行字符串怎么切成 argv」，而这一层是
// 「切完之后怎么变成进程」。后者绝大多数分支只在环境异常时才走到（找不到 sh、PATH 里没有
// 这个工具、PATH 是 POSIX 形态…），正因如此最容易被漏测——可它们恰恰是「应用起不来」时
// 唯一能给出病因的地方：错误信息里有没有原始行、有没有那个名字，直接决定排障是几分钟还是几轮。

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestCommandRejectsMalformedAndEmptyInputs 锁住「构造不出命令」时的三种如实报错。
//
// 三者都是 fail-closed 的落点：宁可报错，也不要把一个明显的笔误变成「跑起来了但语义不对」。
func TestCommandRejectsMalformedAndEmptyInputs(t *testing.T) {
	// 未闭合引号：Parse 的错误必须一路上抛（不能被吞成「空命令」或字面量）。
	if _, err := Command(context.Background(), `tool "未闭合`); err == nil {
		t.Fatal("未闭合引号必须报错")
	}

	// 只有空白：argv 为空，必须明确报错——不能 exec 一个空文件名。
	if _, err := Command(context.Background(), "   "); err == nil {
		t.Fatal("空命令行必须报错")
	}

	// argv[0] 不存在：错误里必须同时带上**原始行**与**被解析出的名字**。
	// 只带其中一个都不够用：前者帮你回想写的是什么，后者帮你确认切词有没有切错。
	_, err := Command(context.Background(), "no-such-binary-xyz --flag")
	if !errors.Is(err, exec.ErrNotFound) {
		t.Fatalf("未知可执行文件应返回 exec.ErrNotFound（可判别），实际 %v", err)
	}
	for _, want := range []string{"no-such-binary-xyz", "no-such-binary-xyz --flag"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("错误应含 %q 以便定位，实际 %v", want, err)
		}
	}
}

// TestCommandShellFallbackRunsThroughShell：需要 shell 的入口在**有** shell 时必须真的跑通。
//
// 与 TestCommandShellPathFailsClosedWithoutShell 构成一对（有/无 shell 两个方向都要验）：
// 只验 fail-closed 会漏掉「找是找到了、但参数拼错导致跑不起来」这类问题。
func TestCommandShellFallbackRunsThroughShell(t *testing.T) {
	sh, err := FindShell()
	if err != nil {
		t.Skipf("本机没有可用的 POSIX shell，跳过 shell 兜底路径：%v", err)
	}
	// `echo '{"shell":"ok"}'` 走的是 shell 内建（见 shellBuiltins），必然是 shell 路径；
	// 同时它也是红线 17 记录的那种写法——外层单引号保住 JSON 的双引号。
	cmd, err := Command(context.Background(), `echo '{"shell":"ok"}'`)
	if err != nil {
		t.Fatalf("有 shell 时不应失败: %v", err)
	}
	if !strings.HasSuffix(cmd.Path, filepath.Base(sh.Path)) {
		t.Fatalf("应以解析到的 shell 执行，实际 %q（期望 %q）", cmd.Path, sh.Path)
	}
	var stdout strings.Builder
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if !strings.Contains(stdout.String(), `{"shell":"ok"}`) {
		t.Fatalf("外层单引号内的 JSON 应原样输出，实际 %q", stdout.String())
	}
}

// TestLookExecutableResolutionOrder 锁住 argv[0] 的三段查找顺序与各自的落点。
func TestLookExecutableResolutionOrder(t *testing.T) {
	// ① 空名字：明确报错，绝不能退化成「当前目录」。
	if _, err := LookExecutable("", nil); err == nil {
		t.Fatal("空可执行文件名必须报错")
	}

	// ② 含路径分隔符：原样返回，不做存在性判断（交给 os/exec 的平台语义，
	//    Windows 会按 PATHEXT 补扩展名）。做成「存在性判断」会在 PATHEXT 场景误杀合法写法。
	if got, err := LookExecutable("plugin/serve-sidecar.exe", nil); err != nil || got != "plugin/serve-sidecar.exe" {
		t.Fatalf("含分隔符应原样返回: got=%q err=%v", got, err)
	}

	// ③ 裸名字：先走进程 PATH（`go` 此刻必然在，因为正在用它跑测试）。
	if _, err := LookExecutable("go", nil); err != nil {
		t.Fatalf("PATH 里的裸名字应解析成功: %v", err)
	}

	// ④ PATH 里没有、但 shell 目录里有 → 走 Shell.Dir 兜底。
	//    这条是为兼容 `cat` / `echo` / `sleep` 这类随 Git 装进 usr\bin、而 usr\bin 不在
	//    PATH 的写法（Windows 上的常态）。
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "faketool.exe"), "x")
	got, err := LookExecutable("faketool-zzz", &Shell{Path: filepath.Join(dir, "sh.exe"), Dir: dir})
	if err == nil {
		t.Fatalf("目录里没有该名字时必须报错，实际返回 %q", got)
	}
	writeFile(t, filepath.Join(dir, "faketool-zzz.exe"), "x")
	got, err = LookExecutable("faketool-zzz", &Shell{Path: filepath.Join(dir, "sh.exe"), Dir: dir})
	if err != nil {
		t.Fatalf("Shell.Dir 兜底应命中: %v", err)
	}
	if !strings.EqualFold(filepath.Base(got), "faketool-zzz.exe") {
		t.Fatalf("应返回 shell 目录里的候选，实际 %q", got)
	}
}

// TestExeCandidatesHonorsPathext：Windows 下裸名字要按 PATHEXT 展开候选。
func TestExeCandidatesHonorsPathext(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("PATHEXT 是 Windows 概念")
	}

	// 空 PATHEXT ⇒ 用内置默认清单（否则精简环境里一个候选都补不出来）。
	t.Setenv("PATHEXT", "")
	got := exeCandidates("x")
	for _, want := range []string{"x", "x.exe", "x.com", "x.cmd"} {
		if !containsSub(got, want) {
			t.Fatalf("默认清单应含 %q，实际 %v", want, got)
		}
	}

	// 已带同名扩展名的 base 不得再叠一层（`x.exe` 不该产生 `x.exe.exe`）。
	for _, c := range exeCandidates("x.exe") {
		if strings.HasSuffix(strings.ToLower(c), ".exe.exe") {
			t.Fatalf("不应产生重复扩展名候选: %v", exeCandidates("x.exe"))
		}
	}

	// 自定义 PATHEXT：空项被忽略，且大小写归一（Windows 环境变量与扩展名都大小写不敏感）。
	t.Setenv("PATHEXT", ".EXE;;.CMD")
	got = exeCandidates("y")
	for _, c := range got {
		if c == "" {
			t.Fatalf("空扩展名项必须被忽略（否则会产出一个空候选），实际 %v", got)
		}
	}
	for _, want := range []string{"y.exe", "y.cmd"} {
		if !containsSub(got, want) {
			t.Fatalf("自定义 PATHEXT 应含 %q，实际 %v", want, got)
		}
	}
}

// TestChildEnvCarriesAssignmentsAndShellDir：子进程环境的两件事都要收在这里——
// 行首 `NAME=value` 赋值，以及「shell 目录不在 PATH 时补进去」。
func TestChildEnvCarriesAssignmentsAndShellDir(t *testing.T) {
	// 无 shell：环境原样继承 + 追加赋值，不碰 PATH。
	env := childEnv([]string{"COORD_ADDR=127.0.0.1:8099"}, nil)
	if !containsSub(env, "COORD_ADDR=127.0.0.1:8099") {
		t.Fatalf("赋值应追加进子进程环境: %v", env)
	}

	// 有 shell 且其目录不在 PATH 里 ⇒ 前置该目录。
	// 用分号形态的 PATH：Windows 上 POSIX 形态（无 `;`）是**有意不补**的（那种形态只可能
	// 来自 MSYS 宿主，此时 sh 本就是靠 PATH 找到的，硬塞 `C:\...` 会把整条 PATH 拆错）。
	t.Setenv("PATH", `C:\Windows\System32;C:\Windows`)
	shDir := filepath.Join(t.TempDir(), "usr", "bin")
	env = childEnv(nil, &Shell{Path: filepath.Join(shDir, "sh.exe"), Dir: shDir})
	var pathVal string
	for _, kv := range env {
		if strings.HasPrefix(strings.ToUpper(kv), "PATH=") {
			pathVal = kv[len("PATH="):]
		}
	}
	if !strings.HasPrefix(pathVal, shDir+string(os.PathListSeparator)) {
		t.Fatalf("shell 目录应被前置进 PATH，实际 PATH=%q", pathVal)
	}
}

// TestSetEnvReplacesOrAppends：同名键（Windows 下不分大小写）就地替换，不存在则追加。
func TestSetEnvReplacesOrAppends(t *testing.T) {
	env := []string{"A=1", "Path=old", "B=2"}
	got := setEnv(env, "PATH", "new")
	if got[1] != "PATH=new" {
		t.Fatalf("应就地替换（保留原位置），实际 %v", got)
	}
	if len(got) != 3 {
		t.Fatalf("替换不应改变长度，实际 %v", got)
	}
	got = setEnv([]string{"A=1"}, "GEAR_NEW_KEY", "v")
	if len(got) != 2 || got[1] != "GEAR_NEW_KEY=v" {
		t.Fatalf("不存在的键应追加，实际 %v", got)
	}
}

// TestAugmentPathDeclinesUnsafePrepend：几个「不该补」的边界必须真的不补。
//
// 这不是洁癖——补错一次会让整条 PATH 失效（子进程连系统目录都找不到），
// 而症状是「什么都没改，工具却找不到了」，极难与代码改动联系起来。
func TestAugmentPathDeclinesUnsafePrepend(t *testing.T) {
	sep := string(os.PathListSeparator)
	cases := []struct {
		name, cur, dir, want string
	}{
		{"目录为空：不动", `C:\a;C:\b`, "", `C:\a;C:\b`},
		{"PATH 为空：不动", "", `C:\tools`, ""},
		{"目录已在 PATH 里（子串命中）：不动", `C:\tools;C:\a`, `C:\tools`, `C:\tools;C:\a`},
		{"已在首位（前缀命中）：不重复前置", `C:\tools` + sep + `C:\a`, `C:\tools`, `C:\tools` + sep + `C:\a`},
		{"POSIX 形态 PATH（Windows 上无 `;`）：不动", `/usr/bin:/bin`, `C:\tools`,
			func() string {
				if runtime.GOOS == "windows" {
					return `/usr/bin:/bin`
				}
				return `C:\tools` + sep + `/usr/bin:/bin`
			}()},
		{"常规：前置", `C:\a;C:\b`, `C:\tools`, `C:\tools` + sep + `C:\a;C:\b`},
	}
	for _, c := range cases {
		if got := augmentPath(c.cur, c.dir); got != c.want {
			t.Errorf("%s: augmentPath(%q, %q) = %q，期望 %q", c.name, c.cur, c.dir, got, c.want)
		}
	}
}

// TestParseBranchesNotCoveredByTable 补 parse_test.go 表格未覆盖的四个分支。
//
// 表格用例覆盖的是「常见写法长什么样」，这里补的是**边角输入**——它们各自对应一条
// 会让整行语义改变的分支，所以值得逐条钉住。
func TestParseBranchesNotCoveredByTable(t *testing.T) {
	cases := []struct {
		name      string
		line      string
		wantArgv  []string
		wantMeta  []string
		wantShell bool
	}{
		{
			name:     "行首空白：切词器在「尚未开始 token」时空转，不产生空参数",
			line:     "  tool arg",
			wantArgv: []string{"tool", "arg"},
		},
		{
			name:      "双引号内的 $：会被展开，仍须走 shell（引号不豁免展开）",
			line:      `tool "a$b"`,
			wantMeta:  []string{"$"},
			wantShell: true,
		},
		{
			name:      "不加引号的 token 开头的 ~：家目录展开，须走 shell",
			line:      `tool ~/bin/x`,
			wantMeta:  []string{"~"},
			wantShell: true,
		},
		{
			name:     "不是合法标识符的 x=y：只当普通参数，不误认成环境变量赋值",
			line:     "--set=a=b tool",
			wantArgv: []string{"--set=a=b", "tool"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			spec, err := Parse(c.line)
			if err != nil {
				t.Fatalf("不应报错: %v", err)
			}
			if spec.NeedsShell != c.wantShell {
				t.Fatalf("NeedsShell = %v，期望 %v（meta=%v）", spec.NeedsShell, c.wantShell, spec.Meta)
			}
			if !equalStrings(spec.Meta, c.wantMeta) {
				t.Fatalf("Meta = %v，期望 %v", spec.Meta, c.wantMeta)
			}
			if !equalStrings(spec.Argv, c.wantArgv) {
				t.Fatalf("Argv = %v，期望 %v", spec.Argv, c.wantArgv)
			}
		})
	}
}
