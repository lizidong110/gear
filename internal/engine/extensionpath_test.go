package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gear/internal/cmdline"
	"gear/internal/model"
)

// ───────── extensions.yaml 的定位 ─────────

// TestResolveExtensionConfigPathPrefersExeDirOverCwd 是本轮修复的核心红线锁之一。
//
// 复现的故障：宿主（WorkBuddy）派生 gear 时 cwd 不可控，而配置此前只按 `./extensions.yaml`
// 找 ⇒ 读不到 ⇒ 7 个 ext 能力全丢 ⇒ 声明 `ext.*` 的 Tool 一律被判「未知的 capability」
// ⇒ MCP 面下注册不了任何带扩展能力的应用。修法是「以可执行文件为锚」：
// **exe 同目录的配置必须压过 cwd 的配置**，因为前者才是随二进制交付的那一份。
func TestResolveExtensionConfigPathPrefersExeDirOverCwd(t *testing.T) {
	exeDir := t.TempDir()
	cwdDir := t.TempDir()
	byExe := filepath.Join(exeDir, "extensions.yaml")
	byCwd := filepath.Join(cwdDir, "extensions.yaml")
	writeText(t, byExe, "ext.a:\n  cmd: [\"a\"]\n")
	writeText(t, byCwd, "ext.b:\n  cmd: [\"b\"]\n")

	got, err := resolveExtensionConfigPath("", exeDir, cwdDir)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if got != byExe {
		t.Fatalf("exe 同目录的配置应优先于 cwd，期望 %q 实际 %q", byExe, got)
	}

	// exe 目录没有时，才轮到 cwd（历史行为，降级为兜底）。
	if err := os.Remove(byExe); err != nil {
		t.Fatal(err)
	}
	got, err = resolveExtensionConfigPath("", exeDir, cwdDir)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if got != byCwd {
		t.Fatalf("exe 目录无配置时应回落到 cwd，期望 %q 实际 %q", byCwd, got)
	}
}

// TestResolveExtensionConfigPathFindsRepoPluginLayout：源码仓布局
// `<repo>/build/gear.exe` ↔ `<repo>/plugin/extensions.yaml`。
// 这条保证了「直接在仓库里跑 ./build/gear.exe」也能读到扩展，无需 cd。
func TestResolveExtensionConfigPathFindsRepoPluginLayout(t *testing.T) {
	repo := t.TempDir()
	exeDir := filepath.Join(repo, "build")
	if err := os.MkdirAll(exeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(repo, "plugin", "extensions.yaml")
	writeText(t, cfg, "ext.a:\n  cmd: [\"a\"]\n")

	got, err := resolveExtensionConfigPath("", exeDir, t.TempDir())
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if got != cfg {
		t.Fatalf("应命中 <repo>/plugin/extensions.yaml，期望 %q 实际 %q", cfg, got)
	}
}

// TestResolveExtensionConfigPathExplicitMissingIsError：GEAR_EXT_CONFIG 显式指了路径却
// 读不到 → 报错，而不是静默回退默认 ext.ai。能力静默缺失是最难排查的一类故障：
// 「配了却等于没配」在很久之后才以别的形态显形。
func TestResolveExtensionConfigPathExplicitMissingIsError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.yaml")
	_, err := resolveExtensionConfigPath(missing, t.TempDir(), t.TempDir())
	if err == nil {
		t.Fatal("显式指定的配置不存在时应报错")
	}
	if !strings.Contains(err.Error(), extensionConfigEnv) {
		t.Fatalf("错误信息应点名 %s，实际 %q", extensionConfigEnv, err)
	}
	// 找不到任何配置（也没有显式指定）仍是合法的「无扩展」状态。
	got, err := resolveExtensionConfigPath("", t.TempDir(), t.TempDir())
	if err != nil || got != "" {
		t.Fatalf("无配置应返回空路径且不报错，实际 (%q, %v)", got, err)
	}
}

// ───────── cmd 的相对路径解析 ─────────

// TestResolveExtensionCmdResolvesRelativeToRoots：配置里的 `plugin/xxx-sidecar.exe` 必须
// 按**配置文件所在目录**解析成绝对路径。此前它原样进 entry，于是「起不起得来」取决于
// 进程 cwd —— 同一个二进制换个 cwd 就整条链路失效，正是「应用起不来」的第二个成因。
func TestResolveExtensionCmdResolvesRelativeToRoots(t *testing.T) {
	root := t.TempDir()
	plug := filepath.Join(root, "plugin")
	writeText(t, filepath.Join(plug, "svc.exe"), "stub")
	writeText(t, filepath.Join(root, "gear.exe"), "stub")

	// 相对路径 → 绝对路径（相对 root）。
	got := resolveExtensionCmd([]string{"plugin/svc.exe", "--a"}, []string{root})
	if !filepath.IsAbs(got[0]) || got[0] != filepath.Join(plug, "svc.exe") {
		t.Fatalf("相对路径应解析成绝对路径，实际 %q", got[0])
	}
	if len(got) != 2 || got[1] != "--a" {
		t.Fatalf("参数必须原样保留，实际 %#v", got)
	}
	// `./x.exe` 同样解析。
	if got := resolveExtensionCmd([]string{"./gear.exe"}, []string{root}); got[0] != filepath.Join(root, "gear.exe") {
		t.Fatalf("./x.exe 应解析成绝对路径，实际 %q", got[0])
	}
	// 绝对路径原样保留（尊重配置作者写死的意图）。
	abs := filepath.Join(root, "plugin", "svc.exe")
	if got := resolveExtensionCmd([]string{abs}, []string{root}); got[0] != abs {
		t.Fatalf("绝对路径不应被改写，实际 %q", got[0])
	}
	// 裸名字是 PATH 查找语义，不是文件路径，不能当成「相对当前目录」。
	if got := resolveExtensionCmd([]string{"node", "s.js"}, []string{root}); got[0] != "node" {
		t.Fatalf("裸名字不应被改写，实际 %q", got[0])
	}
	// 所有 root 都落空 → 原样返回（宁可在运行期报「找不到命令」，也不猜一个别的路径）。
	if got := resolveExtensionCmd([]string{"plugin/none.exe"}, []string{root}); got[0] != "plugin/none.exe" {
		t.Fatalf("解析不到时应原样返回，实际 %q", got[0])
	}
}

// TestLoadExtensionConfigResolvesCmdAgainstConfigDir 是端到端的那一环：
// 只要给一条路径，LoadExtensionConfig 就该把 cmd 就地解析成绝对路径。
func TestLoadExtensionConfigResolvesCmdAgainstConfigDir(t *testing.T) {
	dir := t.TempDir()
	sidecar := filepath.Join(dir, "plugin", "svc-sidecar.exe")
	writeText(t, sidecar, "stub")
	cfg := filepath.Join(dir, "extensions.yaml")
	writeText(t, cfg, "ext.svc:\n"+
		"  cmd: [\"plugin/svc-sidecar.exe\"]\n"+
		"  methods: [ping]\n"+
		"  schemas:\n    ping:\n      parameters: {}\n")

	specs, err := LoadExtensionConfig(cfg)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if got := specs["ext.svc"].Cmd[0]; got != sidecar {
		t.Fatalf("cmd 应按配置目录解析成绝对路径，期望 %q 实际 %q", sidecar, got)
	}
}

// ───────── 两个修复的接缝：entry 必须能被重新解析回原样 ─────────

// TestPrepareServiceExtEntryRoundTripsThroughCmdline 把「配置 → entry」与「entry → 进程」
// 两段接起来验：生成的 entry 交给 cmdline.Parse 后，argv / 环境变量必须与配置语义**完全一致**。
//
// 这条锁的是「含空格的绝对路径」：entry 是以空格分词的命令行，路径不转义就会被拆成两个参数，
// 而那正是把 cmd 解析成绝对路径之后新增的风险面（`C:\Program Files\...` 随手就有空格）。
func TestPrepareServiceExtEntryRoundTripsThroughCmdline(t *testing.T) {
	sidecar := `C:\Program Files\my app\svc-sidecar.exe`
	spec := ExtSpec{
		Cmd:    []string{sidecar, "--serve"},
		Listen: "127.0.0.1:8095",
		HealthProbe: &model.HealthProbe{
			Kind:      model.ProbeKindTCP,
			Interval:  "5s",
			Timeout:   "2s",
			Threshold: 3,
		},
	}
	entry, probe, err := prepareServiceExt(spec)
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	if probe == nil || probe.Addr != spec.Listen {
		t.Fatalf("探针 addr 应取 listen，实际 %+v", probe)
	}

	parsed, err := cmdline.Parse(entry)
	if err != nil {
		t.Fatalf("生成的 entry 无法被解析: %v (entry=%q)", err, entry)
	}
	if parsed.NeedsShell {
		t.Fatalf("entry 不应需要 shell（meta=%v），否则又回到「没有 sh 就起不来」: %q", parsed.Meta, entry)
	}
	if len(parsed.Argv) != 2 || parsed.Argv[0] != sidecar || parsed.Argv[1] != "--serve" {
		t.Fatalf("含空格的路径必须原样还原成单个参数，实际 %#v (entry=%q)", parsed.Argv, entry)
	}
	if len(parsed.Env) != 1 || parsed.Env[0] != "COORD_ADDR="+spec.Listen {
		t.Fatalf("环境变量前缀应还原成 COORD_ADDR，实际 %#v (entry=%q)", parsed.Env, entry)
	}
}

// writeText 写一个测试用小文件（含中间目录）。
func writeText(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
