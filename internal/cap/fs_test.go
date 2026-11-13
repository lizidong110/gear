package cap

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newTestFS 构造以 t.TempDir() 为主根的 RootedFS（含一个额外根，供跨根放行验证）。
func newTestFS(t *testing.T) (*RootedFS, string, string) {
	t.Helper()
	root := t.TempDir()
	extra := t.TempDir()
	f, err := NewRootedFS([]string{root, extra})
	if err != nil {
		t.Fatalf("NewRootedFS 失败: %v", err)
	}
	return f, root, extra
}

// TestFSRoundtrip 正向：write → read → list → mkdir → remove 全链路。
func TestFSRoundtrip(t *testing.T) {
	f, _, _ := newTestFS(t)

	if err := f.Write("a/b.txt", []byte("hello"), false); err != nil {
		t.Fatalf("write 失败: %v", err)
	}
	got, err := f.Read("a/b.txt")
	if err != nil {
		t.Fatalf("read 失败: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("read 内容不符: %q", got)
	}

	// append 追加而非覆盖
	if err := f.Write("a/b.txt", []byte(" world"), true); err != nil {
		t.Fatalf("append write 失败: %v", err)
	}
	got, _ = f.Read("a/b.txt")
	if string(got) != "hello world" {
		t.Fatalf("append 结果不符: %q", got)
	}
	// 非 append 截断重写
	if err := f.Write("a/b.txt", []byte("x"), false); err != nil {
		t.Fatalf("trunc write 失败: %v", err)
	}
	got, _ = f.Read("a/b.txt")
	if string(got) != "x" {
		t.Fatalf("trunc 结果不符: %q", got)
	}

	if err := f.Mkdir("dir1/dir2"); err != nil {
		t.Fatalf("mkdir 失败: %v", err)
	}
	entries, err := f.List("")
	if err != nil {
		t.Fatalf("list 失败: %v", err)
	}
	names := map[string]bool{}
	for _, e := range entries {
		names[e.Name] = true
		if e.Name == "a" && !e.IsDir {
			t.Fatalf("a 应是目录项")
		}
	}
	if !names["a"] || !names["dir1"] {
		t.Fatalf("list 缺项: %v", entries)
	}

	if err := f.Remove("a/b.txt"); err != nil {
		t.Fatalf("remove 失败: %v", err)
	}
	if _, err := f.Read("a/b.txt"); err == nil {
		t.Fatal("删除后 read 应报错")
	}
}

// TestFSRejectsTraversal 负向：相对路径带 .. 逃出安全根必须拒（REQ-FS-CONFINEMENT）。
func TestFSRejectsTraversal(t *testing.T) {
	f, _, _ := newTestFS(t)
	if _, err := f.Read("../x"); !errors.Is(err, ErrPathEscape) {
		t.Fatalf("../x 应报 ErrPathEscape，实际: %v", err)
	}
	if err := f.Write("../x", []byte("no"), false); !errors.Is(err, ErrPathEscape) {
		t.Fatalf("write ../x 应报 ErrPathEscape，实际: %v", err)
	}
	// 深层 .. 同理
	if err := f.Write("a/../../x", []byte("no"), false); !errors.Is(err, ErrPathEscape) {
		t.Fatalf("a/../../x 应报 ErrPathEscape，实际: %v", err)
	}
}

// TestFSRejectsAbsoluteOutsideRoot 负向：根外绝对路径必须拒；根内绝对路径放行。
func TestFSRejectsAbsoluteOutsideRoot(t *testing.T) {
	f, root, extra := newTestFS(t)

	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := f.Write(outside, []byte("no"), false); !errors.Is(err, ErrPathEscape) {
		t.Fatalf("根外绝对路径应报 ErrPathEscape，实际: %v", err)
	}

	// 根内绝对路径放行
	if err := f.Write(filepath.Join(root, "abs.txt"), []byte("ok"), false); err != nil {
		t.Fatalf("根内绝对路径应放行: %v", err)
	}
	// 第二个根同样放行（绝对路径命中任一安全根即可）
	if err := f.Write(filepath.Join(extra, "extra.txt"), []byte("ok"), false); err != nil {
		t.Fatalf("第二根内绝对路径应放行: %v", err)
	}
}

// TestFSRejectsSymlinkEscape 负向：安全根内的符号链接指向根外，必须拒
// （EvalSymlinks 消解后再做 containment，链接逃逸无路可走）。
func TestFSRejectsSymlinkEscape(t *testing.T) {
	f, root, _ := newTestFS(t)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Skipf("无法创建符号链接（权限不足）: %v", err)
	}
	if _, err := f.Read(filepath.Join("link", "secret.txt")); !errors.Is(err, ErrPathEscape) {
		t.Fatalf("符号链接逃逸应报 ErrPathEscape，实际: %v", err)
	}
	if err := f.Write(filepath.Join("link", "evil.txt"), []byte("no"), false); !errors.Is(err, ErrPathEscape) {
		t.Fatalf("经符号链接写根外应报 ErrPathEscape，实际: %v", err)
	}
}

// TestFSRejectsRemoveRoot 负向：不允许删除安全根本身。
func TestFSRejectsRemoveRoot(t *testing.T) {
	f, root, _ := newTestFS(t)
	if err := f.Remove(""); !errors.Is(err, ErrPathEscape) {
		t.Fatalf("remove 空路径应报 ErrPathEscape，实际: %v", err)
	}
	if err := f.Remove(root); !errors.Is(err, ErrPathEscape) {
		t.Fatalf("remove 安全根应报 ErrPathEscape，实际: %v", err)
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("安全根本身不应被删除: %v", err)
	}
}

// TestFSRejectsPrefixSibling 负向：根 /tmp/a 不放行 /tmp/ab（前缀必须带分隔符边界）。
func TestFSRejectsPrefixSibling(t *testing.T) {
	sibling := t.TempDir()
	siblingBase := filepath.Base(sibling)
	parent := filepath.Dir(sibling)
	root := filepath.Join(parent, siblingBase+"x") // 相邻目录名 = 原名 + "x"，不创建也能测收口
	f, err := NewRootedFS([]string{root})
	if err != nil {
		t.Fatalf("NewRootedFS 失败: %v", err)
	}
	if _, err := f.Read(sibling); !errors.Is(err, ErrPathEscape) {
		t.Fatalf("兄弟前缀目录 %q 应报 ErrPathEscape，实际: %v", sibling, err)
	}
}

// TestFSContextGating 守卫：fs 的 capability 门禁语义与 db 同口径——
// 未声明 = ErrCapabilityDenied；声明但未提供实现 = ErrCapabilityUnavailable。
func TestFSContextGating(t *testing.T) {
	// 未声明
	ctx := New(Grants{}, &Providers{FS: &RootedFS{}})
	if _, err := ctx.FS(); !errors.Is(err, ErrCapabilityDenied) {
		t.Fatalf("未声明 fs 应 ErrCapabilityDenied，实际: %v", err)
	}
	// 声明但未提供
	ctx = New(Grants{PrimFS: true}, &Providers{})
	if _, err := ctx.FS(); !errors.Is(err, ErrCapabilityUnavailable) {
		t.Fatalf("声明 fs 但未提供应 ErrCapabilityUnavailable，实际: %v", err)
	}
	// 声明且提供 → 放行
	f, root, _ := newTestFS(t)
	ctx = New(Grants{PrimFS: true}, &Providers{FS: f})
	got, err := ctx.FS()
	if err != nil {
		t.Fatalf("声明且提供应放行: %v", err)
	}
	if err := got.Write("ok.txt", []byte("ok"), false); err != nil {
		t.Fatalf("经门禁写文件失败: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "ok.txt")); err != nil {
		t.Fatalf("文件应落在主根: %v", err)
	}
	// 错误文案含 fs（GrantsFromStrings 合法值清单）
	_, err = GrantsFromStrings([]string{"nope"})
	if err == nil || !strings.Contains(err.Error(), "fs") {
		t.Fatalf("GrantsFromStrings 错误文案应列出 fs: %v", err)
	}
}
