package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gear/internal/gearhome"
)

// 本文件覆盖 2026-09-22 新增的「registry 沿进程树继承」机制。
//
// 为什么它值得专门的用例：机制的失效方式是**完全静默**的——子进程会愉快地用自己的默认值
// 起服，页面只是空列表，日志一个字都没有（实测症状：`/api/flows` 返回 `[]` 而 CLI 有 10 条）。
// 所以这里的断言都盯着「子进程拿到的那个值到底是什么」，而不是「函数被调用了」。

// TestDefaultRegistryHonorsEnv：`--registry` 的**默认值**必须能被 GEAR_REGISTRY 覆盖。
// 只覆盖默认值、不动显式 flag——机制负责「没说的时候永远一致」，不替人说过的决定翻案。
func TestDefaultRegistryHonorsEnv(t *testing.T) {
	pinned := filepath.Join(t.TempDir(), "pinned.db")
	t.Setenv(gearhome.EnvRegistry, pinned)
	if got := defaultRegistry(); got != pinned {
		t.Fatalf("%s 应覆盖 --registry 默认值，期望 %q 实际 %q", gearhome.EnvRegistry, pinned, got)
	}
}

// TestOpenRegistryPinsEnvForChildren：openRegistry 是「本进程决定用哪个库」的**唯一分叉点**，
// 它必须把生效路径**绝对化**后留给子进程。
//
// 用相对路径调一次真实命令，断言环境变量里落的是绝对路径、且指向同一个库。
// 「绝对化」这一步不能省：相对值原样传下去，子进程只会按**自己的** cwd 再解析一遍，
// 那正是要消除的不确定性。
func TestOpenRegistryPinsEnvForChildren(t *testing.T) {
	// db 先取：withTempRegistry 内部也调 t.TempDir()，**每次调用返回不同目录**，
	// 顺序颠倒会让 chdir 到 A 而断言库里在 B（本用例第一版就踩了这个，红得莫名其妙）。
	db := withTempRegistry(t) // 绝对路径；runCLI 会把它前置，显式用例的值仍然后置胜出
	// 必须 chdir 到库所在目录：跨盘符时 filepath.Rel 给不出相对路径，
	// 而「用户传相对值」正是本用例要模拟的场景。
	dir := filepath.Dir(db)
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })

	t.Setenv(gearhome.EnvRegistry, "")
	rel := filepath.Base(db)

	if err := runCLI(t, "flow", "list", "--registry", rel); err != nil {
		t.Fatalf("flow list --registry %s 失败: %v", rel, err)
	}
	got := os.Getenv(gearhome.EnvRegistry)
	if !filepath.IsAbs(got) {
		t.Fatalf("%s 必须是绝对路径（相对值会被子进程按自己的 cwd 再解析一次），实际 %q",
			gearhome.EnvRegistry, got)
	}
	if filepath.Clean(got) != filepath.Clean(db) {
		t.Fatalf("钉住的应是本次生效的库，期望 %q 实际 %q", db, got)
	}
}

// TestPinRegistryEnvEdgeCases：URI 形态不下传、路径与库无关的失败分支不能静默。
func TestPinRegistryEnvEdgeCases(t *testing.T) {
	t.Setenv(gearhome.EnvRegistry, "sentinel")
	// URI 形态：不做路径处理，也不下传（子进程的语义未必相同）。
	pinRegistryEnv("file::memory:?cache=shared")
	if got := os.Getenv(gearhome.EnvRegistry); got != "sentinel" {
		t.Fatalf("URI 形态不应改写 %s，实际 %q", gearhome.EnvRegistry, got)
	}
	// 绝对路径原样（绝对化是幂等的，不该产生第二个真相）。
	abs := filepath.Join(t.TempDir(), "r.db")
	pinRegistryEnv(abs)
	if got := os.Getenv(gearhome.EnvRegistry); filepath.Clean(got) != filepath.Clean(abs) {
		t.Fatalf("绝对路径应原样落盘，期望 %q 实际 %q", abs, got)
	}
}

// TestRelativeRegistryPathWarning：相对路径的提示是**可执行的**——它必须给出真实落点
// 与传递信道名，否则用户只知道「有风险」而不知道现在到底用了哪个库。
func TestRelativeRegistryPathWarning(t *testing.T) {
	if got := relativeRegistryPathWarning(filepath.Join(t.TempDir(), "r.db")); got != "" {
		t.Fatalf("绝对路径不该提示，实际 %q", got)
	}
	if got := relativeRegistryPathWarning(""); got != "" {
		t.Fatalf("空路径不该提示，实际 %q", got)
	}
	got := relativeRegistryPathWarning(filepath.Join("build", "ui-registry.db"))
	for _, want := range []string{"--registry", "ui-registry.db", gearhome.EnvRegistry, "绝对路径"} {
		if !strings.Contains(got, want) {
			t.Errorf("提示缺少 %q：\n%s", want, got)
		}
	}
}

// bannerFlow 供 TestServeBannerReportsCounts 用（只依赖 luaTool）。
const bannerFlow = `id: once
name: once
description: 一次性 Flow
steps:
  - id: s1
    call: lua.echo
    input:
      name: hi
`

// TestServeBannerReportsCounts：起服横幅必须带库内计数。
//
// 这是「指向了空库」唯一的**非静默**信号：服务在听、页面能开、只是列表为空。
// 一行 `flows=0` 就能把「指错库了」与「库里真没东西」分开——2026-09-22 的排查里，
// 缺这一行的代价是整整一轮定位。
func TestServeBannerReportsCounts(t *testing.T) {
	withTempRegistry(t)
	if err := runCLI(t, "tool", "register", writeTempYAML(t, luaTool)); err != nil {
		t.Fatalf("注册 Tool 失败: %v", err)
	}
	if err := runCLI(t, "flow", "register", writeTempYAML(t, bannerFlow)); err != nil {
		t.Fatalf("注册 Flow 失败: %v", err)
	}
	reg, err := openRegistry()
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()

	out := serveBanner("127.0.0.1:8787", registryPath, reg)
	for _, want := range []string{"127.0.0.1:8787", "flows=1", "tools=1"} {
		if !strings.Contains(out, want) {
			t.Errorf("banner 缺少 %q：%s", want, out)
		}
	}
	if !strings.Contains(out, registryPath) {
		t.Errorf("banner 必须回显实际用的 registry（否则「指错库」看不出）：%s", out)
	}
}
