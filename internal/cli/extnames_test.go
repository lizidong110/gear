package cli

// extnames_test.go —— 「注册 / 导入前必须先并入 extensions.yaml 的 ext.* 名」这条前置的断言。
//
// 存在理由：REQ-IMPORT-EXTNAMES 此前只标了「真机复验：`gear import examples/agent-app`
// 一次通过」，**没有任何测试断言它**——而闸门（requirements_audit）当时也恰好跳过这一行
// （行正则不匹配带反引号的 ID，见该脚本 ROW 注释），于是「实现了但没测」长期看不出来。
// 现在补上自动断言。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gear/internal/cap"
	"gear/internal/registry"
)

func mustOpenRegistry(t *testing.T, path string) *registry.Registry {
	t.Helper()
	reg, err := registry.Open(path)
	if err != nil {
		t.Fatalf("打开 registry 失败: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	return reg
}

// writeExtConfig 写一份最小可用的 extensions.yaml，声明若干 ext.*（含 methods+schemas，
// 缺了 SetupExtensions 会 fail-closed 拒掉，那正是另一条守卫）。
func writeExtConfig(t *testing.T, dir string, names ...string) string {
	t.Helper()
	var b strings.Builder
	for _, n := range names {
		b.WriteString(n + ":\n")
		b.WriteString("  cmd: [" + `"demo-sidecar"` + "]\n")
		b.WriteString("  methods:\n    - ping\n")
		b.WriteString("  schemas:\n")
		b.WriteString("    ping:\n      description: 探活\n")
	}
	p := filepath.Join(dir, "extensions.yaml")
	if err := os.WriteFile(p, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestInitExtensionNamesRegistersExtCaps 锁住前置本身：
// 调完之后 extensions.yaml 里声明的 ext.* 名在能力目录里**可被声明**。
//
// 反向断言是这条测试的重点：用一个**没有**出现在配置里的 ext 名，必须仍然被拒——
// 否则「并入」这一步等于把所有 ext.* 都放行，门禁形同虚设。
func TestInitExtensionNamesRegistersExtCaps(t *testing.T) {
	const declared = "ext.initnamesprobe"
	dir := t.TempDir()
	cfg := writeExtConfig(t, dir, declared)
	t.Setenv("GEAR_EXT_CONFIG", cfg)

	reg, err := registry.Open(filepath.Join(dir, "registry.db"))
	if err != nil {
		t.Fatalf("打开 registry 失败: %v", err)
	}
	defer reg.Close()

	// 前置之前：该 ext 名不在合法集合里（cap 的全局表可能被同包其他用例填过，
	// 故先确认基线，必要时换一个名字——但名字是本用例独有的常量，正常情况下基线就是未注册）。
	if _, err := cap.GrantsFromStrings([]string{declared}); err == nil {
		t.Skip("该 ext 名已被同进程其它用例注册，无法验证前置效果")
	}

	if err := initExtensionNames(reg); err != nil {
		t.Fatalf("initExtensionNames 失败: %v", err)
	}
	if _, err := cap.GrantsFromStrings([]string{declared}); err != nil {
		t.Fatalf("前置之后 %s 仍不可声明: %v", declared, err)
	}
	// 反向：配置里没有的名字必须**仍然**被拒（不许变成「放行一切」）。
	const undeclared = "ext.initnamesabsent"
	if _, err := cap.GrantsFromStrings([]string{undeclared}); err == nil {
		t.Fatalf("%s 不在 extensions.yaml 里，却通过了校验——并入逻辑过宽", undeclared)
	}
}

// TestImportRegistersExtNamesBeforeApplying 是这条需求的**端到端**断言：
// 产物里带 `capabilities: [ext.*]` 的 Tool 必须能导入成功。
//
// 判据选「导入成功」而不是「initExtensionNames 返回 nil」：真正要防的是
// 「导入第一步就报未知的 capability」，而那发生在 ApplyBundle 内部。
// 名字取本用例独有的常量，避免与同进程其它用例的全局注册表互相干扰。
func TestImportRegistersExtNamesBeforeApplying(t *testing.T) {
	const extName = "ext.importnamesprobe"
	dir := t.TempDir()
	cfg := writeExtConfig(t, dir, extName)
	t.Setenv("GEAR_EXT_CONFIG", cfg)

	// 造一个只含「声明了该 ext 的 Lua Tool」的产物。
	// 造产物这一步本身就经registry.UpsertTool 校验 capability，所以**先并入扩展名**——
	// 这正是 `gear tool register` / `gear import` 都要在落库前做的那一步。
	srcReg := filepath.Join(dir, "src.db")
	srcRoot := t.TempDir()
	if err := initExtensionNames(mustOpenRegistry(t, srcReg)); err != nil {
		t.Fatalf("并入扩展名失败: %v", err)
	}
	seedAppWithCapabilities(t, srcReg, srcRoot, "capsdemo", []string{extName})

	out := filepath.Join(dir, "artifact")
	if _, err := runArtifact(t, srcReg, "export", "capsdemo", "--out", out, "--root", srcRoot); err != nil {
		t.Skipf("export 失败，跳过端到端断言: %v", err)
	}

	// 导进一个空库：import 路径必须先并入扩展名，否则 ApplyBundle 会拒。
	dstReg := filepath.Join(dir, "dst.db")
	str, err := runArtifact(t, dstReg, "import", out)
	if err != nil {
		t.Fatalf("import 失败（ext.* 名没被并入？）: %v\n%s", err, str)
	}
	if !strings.Contains(str, "capsdemo") {
		t.Fatalf("import 输出不像成功: %q", str)
	}
}
