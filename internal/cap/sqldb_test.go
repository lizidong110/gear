package cap

// sqldb_test.go：锁死「db 原语的库定位」这条可移植性契约。
//
// 背景（2026-09-25）：应用想在 db 原语里持久化就必须显式传 dsn，而相对 dsn 此前由 SQLite
// 按**子进程 cwd** 解析；Lua 沙箱禁了 os 库、模板也没有 ${env} 前缀，脚本读不到 GEAR_HOME，
// 于是应用只能把绝对路径硬编码进 YAML（mindstorm_data.yaml 里 18 处
// `local DSN = "D:/gadern/gear/build/mindstorm.db"`），仓库拷到别的机器就跑不起来。
// 修法是宿主把「相对库名」锚定到应用数据目录（gearhome.AppDataDir）——本文件就是这个锚的回归锁。

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMultiDBRelativeDSNIsAnchoredToRoot 正向：相对库名必须落到 root 下，且**与 cwd 无关**。
func TestMultiDBRelativeDSNIsAnchoredToRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "appdata")
	db := NewMultiDB("", root)
	t.Cleanup(func() { _ = db.Close() })

	if err := db.Exec("sqlite", "mindstorm.db", "CREATE TABLE t(id INTEGER, v TEXT)"); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	if err := db.Exec("sqlite", "mindstorm.db", "INSERT INTO t VALUES(1,'a')"); err != nil {
		t.Fatalf("插入失败: %v", err)
	}
	want := filepath.Join(root, "mindstorm.db")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("相对库名应落到应用数据目录 %q: %v", want, err)
	}

	// 换个 cwd 用另一个实例读同一库：落点不变才是可移植（否则「换目录启动就换一份数据」）。
	t.Chdir(t.TempDir())
	other := NewMultiDB("", root)
	t.Cleanup(func() { _ = other.Close() })
	rows, err := other.Query("sqlite", "mindstorm.db", "SELECT id, v FROM t")
	if err != nil {
		t.Fatalf("换 cwd 后读库失败: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("换 cwd 后应读到 1 行，实际 %d 行", len(rows))
	}
}

// TestMultiDBRelativeDSNKeepsCwdWhenRootEmpty 负向：宿主未装配 root 时**不加工** dsn。
// 静默改变「没装配时的语义」会让既有应用的数据悄悄搬家，这比不改更糟。
func TestMultiDBRelativeDSNKeepsCwdWhenRootEmpty(t *testing.T) {
	cwd := t.TempDir()
	t.Chdir(cwd)
	db := NewMultiDB("", "")
	t.Cleanup(func() { _ = db.Close() })

	if err := db.Exec("sqlite", "legacy.db", "CREATE TABLE t(id INTEGER)"); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cwd, "legacy.db")); err != nil {
		t.Fatalf("未装配 root 时应保持「相对 cwd」的历史行为: %v", err)
	}
}

// TestMultiDBAbsoluteDSNIsNotRewritten 负向：调用方已经给明确的绝对路径（或 URI），
// 宿主不得再把它拼进应用数据目录——「你传什么就是什么」。
func TestMultiDBAbsoluteDSNIsNotRewritten(t *testing.T) {
	root := filepath.Join(t.TempDir(), "appdata")
	abs := filepath.Join(t.TempDir(), "explicit.db")
	db := NewMultiDB("", root)
	t.Cleanup(func() { _ = db.Close() })

	if err := db.Exec("sqlite", abs, "CREATE TABLE t(id INTEGER)"); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	if _, err := os.Stat(abs); err != nil {
		t.Fatalf("绝对路径 dsn 必须原样使用: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "explicit.db")); err == nil {
		t.Fatalf("绝对路径 dsn 被拼进了应用数据目录 %q", root)
	}
}

// TestMultiDBURIDSNIsNotRewritten 负向：`file:xxx?mode=memory` 这类 URI 形态一旦被拼进目录，
// 内存 scratch 库就变成磁盘文件（语义变了、还可能落一堆垃圾文件）。
func TestMultiDBURIDSNIsNotRewritten(t *testing.T) {
	root := filepath.Join(t.TempDir(), "appdata")
	db := NewMultiDB("", root)
	t.Cleanup(func() { _ = db.Close() })

	dsn := "file:portable_mem?mode=memory&cache=shared"
	if err := db.Exec("sqlite", dsn, "CREATE TABLE t(id INTEGER)"); err != nil {
		t.Fatalf("URI 形态建表失败: %v", err)
	}
	if entries, err := os.ReadDir(root); err == nil && len(entries) > 0 {
		t.Fatalf("URI 形态 dsn 不应在应用数据目录里落文件: %v", entries)
	}
}

// TestMultiDBRelativeDSNCreatesRootDir：应用数据目录可能还不存在（首次运行），
// SQLite 不会替你建目录——必须在解析时建，否则报的是看不懂的「unable to open database file」。
func TestMultiDBRelativeDSNCreatesRootDir(t *testing.T) {
	root := filepath.Join(t.TempDir(), "nested", "appdata")
	db := NewMultiDB("", root)
	t.Cleanup(func() { _ = db.Close() })

	if err := db.Exec("sqlite", "fresh.db", "CREATE TABLE t(id INTEGER)"); err != nil {
		t.Fatalf("首次运行建库失败（应自动创建目录）: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "fresh.db")); err != nil {
		t.Fatalf("库未落到 %q: %v", root, err)
	}
}
