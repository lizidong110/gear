package gearhome

// gearhome_test.go：本包存在的唯一理由就是「默认落盘位置不得依赖 cwd」，
// 所以测试的重点不是「函数返回了某个字符串」，而是**换一个 cwd，默认值一个字节都不变**。

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestDefaultsDoNotDependOnCwd 是本包的核心回归锁。
//
// 场景：`gear mcp` 由客户端以**任意 cwd** 派生（WorkBuddy 就是如此）。只要默认路径里
// 掺了 cwd，就会出现三种很难看出来的故障：
//   - 执行历史落到 `<cwd>/build/gear.db`，顺手在用户目录里造出 `build/`；
//   - 换个 cwd 再跑，同一台机器上的执行历史「不见了」；
//   - `extensions.yaml` 读不到，7 个 ext 能力凭空消失（2026-09-21 实测，两轮排查才定位）。
//
// 判据刻意用「同一次运行内跨 cwd 比较」而不是写死期望路径：写死会随环境（HOME 在哪）而脆，
// 而「换个 cwd 结果不变」正是我们真正要钉住的性质。
func TestDefaultsDoNotDependOnCwd(t *testing.T) {
	base := t.TempDir()
	t.Chdir(base)
	regA, execA := RegistryPath(), ExecDBPath()

	// 换到一个结构完全不同的目录（更深的层级 + 另一个盘符语义）。
	other := filepath.Join(t.TempDir(), "deep", "nested")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(other)
	regB, execB := RegistryPath(), ExecDBPath()

	if regA != regB {
		t.Fatalf("registry 默认路径随 cwd 变化：%q → %q", regA, regB)
	}
	if execA != execB {
		t.Fatalf("执行记录库默认路径随 cwd 变化：%q → %q", execA, execB)
	}
	// 两者必须同目录：它们是「这台机器上这一套 gear」的两个持久面，
	// 分开就意味着「换一个 cwd 就换一份历史」这类问题会从另一扇门回来。
	if filepath.Dir(regA) != filepath.Dir(execA) {
		t.Fatalf("registry 与执行库应同目录：%q vs %q", regA, execA)
	}
}

// TestExecDBPathEnvOverride：GEAR_EXEC_DB 是显式逃生门，必须优先于默认值。
func TestExecDBPathEnvOverride(t *testing.T) {
	want := filepath.Join(t.TempDir(), "custom-exec.db")
	t.Setenv(EnvExecDB, want)
	if got := ExecDBPath(); got != want {
		t.Fatalf("GEAR_EXEC_DB 应优先，实际 %q 期望 %q", got, want)
	}
}

// TestHomeHonorsEnvAndIsStable：GEAR_HOME 是隔离多实例的入口（测试隔离也靠它）。
func TestHomeHonorsEnvAndIsStable(t *testing.T) {
	want := t.TempDir()
	t.Setenv(EnvHome, want)
	if got := Home(); got != want {
		t.Fatalf("GEAR_HOME 应优先，实际 %q 期望 %q", got, want)
	}
	if got := RegistryPath(); got != filepath.Join(want, "registry.db") {
		t.Fatalf("registry 应落在 GEAR_HOME 下，实际 %q", got)
	}
	if got := ExecDBPath(); got != filepath.Join(want, "exec.db") {
		t.Fatalf("执行库应落在 GEAR_HOME 下，实际 %q", got)
	}
}

// TestArtifactDirsAnchorsOnExecutable 锁住「产物查找以可执行文件目录为主锚点」。
//
// 缺陷形态（2026-09-21 实测）：doctor 只看 `./build/plugin` 与 `./build`，于是客户端以
// 任意 cwd 派生 `gear mcp` 时，一台侧车齐全的机器会被报成「能力可用但未找到侧车二进制」，
// 把智能体引去重编译——而真正的问题是它在错误的目录里找。
//
// 判据：候选序列必须**同时**含「exe 同目录」「exe 同目录/plugin」，且 exe 相关候选排在
// cwd 相关候选之前（cwd 只是最低可信的兜底）。
func TestArtifactDirsAnchorsOnExecutable(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	exeDir := filepath.Dir(exe)

	dirs := ArtifactDirs()
	idxOf := func(want string) int {
		for i, d := range dirs {
			if d == want {
				return i
			}
		}
		return -1
	}
	iExe := idxOf(exeDir)
	iPlugin := idxOf(filepath.Join(exeDir, "plugin"))
	if iExe < 0 || iPlugin < 0 {
		t.Fatalf("候选序列必须含 exe 同目录与其 plugin 子目录，实际 %v（exeDir=%s）", dirs, exeDir)
	}
	iCwd := idxOf(filepath.Join("build", "plugin"))
	if iCwd >= 0 && iCwd < iExe {
		t.Fatalf("cwd 相关候选不得排在 exe 相关候选之前（否则 cwd 又会压过真实产物目录）：%v", dirs)
	}
	// 去重：同一个目录出现两次会让告警里出现重复的「已找过」。
	seen := map[string]bool{}
	for _, d := range dirs {
		if seen[d] {
			t.Fatalf("候选目录重复：%q（%v）", d, dirs)
		}
		seen[d] = true
	}
}

// TestPosixPathHintDetectsMsysPaths：POSIX 路径误用的护栏。
//
// 这个坑在同一轮里连着咬了两次（`go build -o /d/...` 的产物落到 `D:\d\...` 而脚本报 PASS；
// `--registry /d/x.db` 的库落到 `D:\d\x.db`）。它之所以难查，是因为
// `filepath.IsAbs("/d/x")` 在 Windows 上返回 **true** —— 任何基于 IsAbs 的校验都拦不住，
// 只能靠「首段是单个字母」这条形态判据把它认出来。
func TestPosixPathHintDetectsMsysPaths(t *testing.T) {
	cases := []struct {
		in       string
		wantHit  bool
		wantPath string
	}{
		{"/d/gadern/gear/build/gear.exe", true, `D:\gadern\gear\build\gear.exe`},
		{"/c/Users/920/x.db", true, `C:\Users\920\x.db`},
		{`D:\gadern\gear\build\gear.exe`, false, ""},
		{`\Windows\System32`, false, ""}, // 合法的驱动器相对路径：首段是词，不误报
		{"/usr/bin/sh", false, ""},       // 首段不是单字母
		{"/d", false, ""},                // 没有后续段
		{"", false, ""},
		{"relative/path", false, ""},
	}
	for _, c := range cases {
		got := PosixPathHint(c.in)
		if c.wantHit {
			if got == "" {
				t.Errorf("%q 应被判为 POSIX 路径误用", c.in)
				continue
			}
			if !strings.Contains(got, c.wantPath) {
				t.Errorf("%q 的提示应给出真实落点 %q，实际 %q", c.in, c.wantPath, got)
			}
			continue
		}
		// 合法路径必须**零提示**：一条会误报的护栏等于没有护栏（噪声会让人直接忽略它）。
		if got != "" {
			// 非 Windows 平台没有这个概念，理应一律返回空。
			if runtime.GOOS == "windows" {
				t.Errorf("%q 是合法路径，不应提示，实际 %q", c.in, got)
			}
		}
	}
}

// TestIsolateForTestRedirectsExecDB 验证「测试隔离」本身是有效的。
//
// 这条不是自娱自乐：internal/server / internal/cli / internal/mcp 三个包的 TestMain 都靠它
// 把执行库挪出用户数据目录。一旦它悄悄失效，`go test ./...` 会往**用户真实的执行库**里写
// 记录（2026-09-21 实测发生过一次），而那些记录又会被 gear_execution 列出来。
func TestIsolateForTestRedirectsExecDB(t *testing.T) {
	before := ExecDBPath() // 读一次，确认隔离前后确实不同（顺序无关，避免依赖环境）
	cleanup := IsolateForTest()
	defer cleanup()

	got := ExecDBPath()
	if got == before {
		t.Fatalf("隔离后执行库路径不应与隔离前相同：%q", got)
	}
	if _, err := os.Stat(filepath.Dir(got)); err != nil {
		t.Fatalf("隔离目录应已创建: %v", err)
	}
	dir := filepath.Dir(got)
	cleanup()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("清理后临时目录应被删除，实际 err=%v", err)
	}
	// 再次调用必须幂等可用（三个包各自调用一次，互不影响）。
	cleanup2 := IsolateForTest()
	defer cleanup2()
	if ExecDBPath() == got {
		t.Fatalf("每次隔离都应新建目录，实际仍是 %q", got)
	}
}

// TestRegistryPathHonorsEnvRegistry：GEAR_REGISTRY 是「继承」信道，不是普通配置项。
//
// 为什么必须优先于 <Home>/registry.db：起服进程选定库之后，它派生的一切（监管进程、
// 以及**应用自己拉起的长驻 Tool**）都必须沿用同一个。少了这条信道，应用只能在 entry 里
// 写死 `--registry`，而写死的相对值按子进程自己的 cwd 解析 ⇒ 静默读到另一个库
// （2026-09-22 实测症状：`/api/flows` 返回 `[]` 而 CLI `flow list` 有 10 条）。
func TestRegistryPathHonorsEnvRegistry(t *testing.T) {
	pinned := filepath.Join(t.TempDir(), "pinned.db")
	t.Setenv(EnvRegistry, pinned)
	if got := RegistryPath(); got != pinned {
		t.Fatalf("%s 应优先于 <Home>/registry.db，期望 %q 实际 %q", EnvRegistry, pinned, got)
	}
	// 空值等同未设置：退回 <Home>/registry.db。否则「显式设成空串」会让路径退化成 "." 之类。
	t.Setenv(EnvRegistry, "")
	if got, want := RegistryPath(), filepath.Join(Home(), "registry.db"); got != want {
		t.Fatalf("空 %s 应退回默认，期望 %q 实际 %q", EnvRegistry, want, got)
	}
}

// TestIsolateForTestClearsRegistryEnv：测试隔离必须连**继承信道**一起清掉。
//
// 不清的后果不是「多写一个文件」：同进程里先跑的用例会把它的临时库泄漏给后面的用例，
// 于是「默认路径」类断言开始依赖执行顺序——那种红极难定位（与「不碰 GEAR_HOME」同一个理由）。
func TestIsolateForTestClearsRegistryEnv(t *testing.T) {
	leaked := filepath.Join(t.TempDir(), "leaked.db")
	t.Setenv(EnvRegistry, leaked)

	cleanup := IsolateForTest()
	if got := os.Getenv(EnvRegistry); got != "" {
		cleanup()
		t.Fatalf("隔离应清掉 %s，实际仍是 %q", EnvRegistry, got)
	}
	cleanup()
	// 清理是**还原**而非无条件删除：隔离不该改变调用方原本的环境。
	if got := os.Getenv(EnvRegistry); got != leaked {
		t.Fatalf("清理应还原隔离前的值 %q，实际 %q", leaked, got)
	}
}

// TestRunCoverage: 兜底断言本包的三个入口在默认环境下都能给出非空路径。
// 极端环境（没有 HOME、没有可执行文件）下也不允许返回空串——空串会让调用方
// `filepath.Dir("")` 得到 "."，于是路径又悄悄变回「相对 cwd」。
func TestRunCoverage(t *testing.T) {
	for name, got := range map[string]string{
		"Home":         Home(),
		"RegistryPath": RegistryPath(),
		"ExecDBPath":   ExecDBPath(),
	} {
		if strings.TrimSpace(got) == "" {
			t.Fatalf("%s 不得返回空串（否则会退化成相对 cwd）", name)
		}
	}
}
