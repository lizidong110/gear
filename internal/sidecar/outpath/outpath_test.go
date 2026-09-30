package outpath

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveHappyPath(t *testing.T) {
	root := t.TempDir()
	got, err := Resolve(root, "reports/2026/a.xlsx")
	if err != nil {
		t.Fatalf("root 内的正常路径不该被拦: %v", err)
	}
	want := filepath.Join(root, "reports", "2026", "a.xlsx")
	if got != want {
		t.Fatalf("Resolve = %s want %s", got, want)
	}
}

func TestResolveRejectsTraversal(t *testing.T) {
	root := t.TempDir()
	for _, bad := range []string{"../out.txt", "a/../../out.txt", "..", "a\\b.txt", "C:/Windows/x.txt", ""} {
		if _, err := Resolve(root, bad); err == nil {
			t.Errorf("path %q 不该被放行", bad)
		}
	}
}

func TestResolveBadRoot(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "a-file")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(file, "a.xlsx"); err == nil || !strings.Contains(err.Error(), "不是目录") {
		t.Fatalf("root 是文件时应说清原因，实际 %v", err)
	}
	if _, err := Resolve(filepath.Join(root, "缺失的目录"), "a.xlsx"); err == nil {
		t.Fatal("root 不存在时应报错")
	}
}

func TestResolveIntermediateIsFile(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "blocker"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Resolve(root, "blocker/a.xlsx")
	if err == nil {
		t.Fatal("中间一层是文件时应报错")
	}
	if !strings.Contains(err.Error(), "不是目录") {
		t.Fatalf("报错应说清是哪一层的问题，实际 %v", err)
	}
}

func TestRejectSymlinkEscape(t *testing.T) {
	// Rel 出不来（不同盘符）必须报错，而不是「算不出就当没风险」
	if err := RejectSymlinkEscape(`C:\`, `D:\x`); err == nil {
		t.Fatal("Rel 失败时应报错")
	}
	// 目标比 root 还浅（Rel 结果是 `..` ⇒ 外层 Resolve 已经判过越界，这里不该二次误伤）
	root := t.TempDir()
	if err := RejectSymlinkEscape(root, filepath.Join(root, "a", "b.txt")); err != nil {
		t.Fatalf("root 内不存在的路径不该在这里报错: %v", err)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "sub", "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("本机不允许建符号链接: %v", err)
	}
	// 沙箱里 os.Symlink 会「创建成功」但落地的并不是链接；不先把夹具验真，
	// 这条用例就会变成假绿（2026-09-29 实踩）。
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Skipf("本机建的 %s 不是真符号链接（mode=%v），跳过穿透用例", link, fi.Mode())
	}
	if _, err := Resolve(root, "sub/escape/secret.txt"); err == nil {
		t.Fatal("顺着链接爬出 root 必须被拒")
	}
}

func TestRejectFinalSymlink(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "report.txt")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("本机不允许建符号链接: %v", err)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Skipf("本机建的 %s 不是真符号链接，跳过", link)
	}
	if err := RejectFinalSymlink(link); err == nil {
		t.Fatal("末级是符号链接时必须拒")
	}
	// 普通文件、不存在的文件都不该被这一条拦
	plain := filepath.Join(root, "plain.txt")
	if err := os.WriteFile(plain, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RejectFinalSymlink(plain); err != nil {
		t.Fatalf("普通文件不该被拒: %v", err)
	}
	if err := RejectFinalSymlink(filepath.Join(root, "尚无.png")); err != nil {
		t.Fatalf("不存在的目标不该被这一条拦: %v", err)
	}
}

func TestOutside(t *testing.T) {
	// 分隔符按本机算：Windows 上 filepath.Separator 是 `\`，跨平台写死 `/` 的那几个
	// 期望在本机就会反着成立（第一次跑就撞上了这个）。
	sep := string(filepath.Separator)
	cases := map[string]bool{
		"..":             true,
		".." + sep + "a": true,
		"a" + sep + "b":  false,
		"a":              false,
		".":              false,
		"...":            false,
		"....":           false,
		"..x" + sep:      false, // 普通目录名恰好以 ".." 开头，不能误判成越界
		"a" + sep + "..": false, // Rel 自身的产物形态，在此之前已被「不许 ..」挡掉
	}
	for rel, want := range cases {
		if got := Outside(rel); got != want {
			t.Errorf("Outside(%q) = %v want %v", rel, got, want)
		}
	}
}

func TestEnsureDirFailsWhenParentIsFile(t *testing.T) {
	root := t.TempDir()
	blocker := filepath.Join(root, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := EnsureDir(filepath.Join(blocker, "a.txt")); err == nil {
		t.Fatal("父路径是文件时应报错")
	}
}

func TestEnsureDirCreatesNestedDirs(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "a", "b", "c.txt")
	if err := EnsureDir(target); err != nil {
		t.Fatalf("EnsureDir 失败: %v", err)
	}
	if st, err := os.Stat(filepath.Dir(target)); err != nil || !st.IsDir() {
		t.Fatalf("父目录应被造出来: %v", err)
	}
}
