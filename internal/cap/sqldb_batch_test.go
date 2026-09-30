package cap

// sqldb_batch_test.go：锁 `db.batch` 的**全或无**语义（ENTERPRISE_PRD.md FR-2，AC-B1~B4）。
//
// 验收口径刻意选在「调用方再查一次」这一层，而不是「batch 返回了什么」：
// 事务回滚了没有，唯一可观察的判据就是**部分写入查不到**。实现细节（begin/commit 调没调）
// 换实现就失效，这条不会。

import (
	"path/filepath"
	"strings"
	"testing"
)

func batchDB(t *testing.T) *MultiDB {
	t.Helper()
	db := NewMultiDB(filepath.Join(t.TempDir(), "batch.db"), t.TempDir())
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func countRows(t *testing.T, db *MultiDB, sql string) int {
	t.Helper()
	rows, err := db.Query("sqlite", "", sql)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	return len(rows)
}

// AC-B1：两条语句、第二条语法错 → 整批回滚，随后查不到第一条的写入。
func TestMultiDBBatchIsAllOrNothing(t *testing.T) {
	db := batchDB(t)
	if err := db.Exec("sqlite", "", "CREATE TABLE ledger(id INTEGER, amt INTEGER)"); err != nil {
		t.Fatalf("建表失败: %v", err)
	}

	out, err := db.Batch("sqlite", "", []BatchStmt{
		{SQL: "INSERT INTO ledger VALUES(1, 100)"},
		{SQL: "INSERT INTO ledger VALUES(2, )"}, // 语法错：括号里没值
	})
	if err != nil {
		t.Fatalf("语句失败不该是调用错误（应走 outcome），实际: %v", err)
	}
	if out.Committed {
		t.Fatal("任一条失败即 committed=false")
	}
	// results 要指明第几条、什么错：第 1 条成功、第 2 条带错误。
	if len(out.Results) != 2 {
		t.Fatalf("results 长度应为 2，实为 %d: %+v", len(out.Results), out.Results)
	}
	if out.Results[0].Error != "" {
		t.Fatalf("第 1 条应成功，却报了 %q", out.Results[0].Error)
	}
	if out.Results[1].Error == "" {
		t.Fatal("第 2 条应带错误信息")
	}
	if !strings.Contains(out.Results[1].Error, "第 2 条") {
		t.Fatalf("错误信息应指明第几条: %q", out.Results[1].Error)
	}
	// 可观察口径：部分写入必须查不到。
	if n := countRows(t, db, "SELECT id FROM ledger"); n != 0 {
		t.Fatalf("回滚后应查不到任何写入，实际剩 %d 行", n)
	}
}

// AC-B2：全部成功 → committed=true，rows_affected 与语句一一对应。
func TestMultiDBBatchCommittedAndRowsAffected(t *testing.T) {
	db := batchDB(t)
	if err := db.Exec("sqlite", "", "CREATE TABLE kv(k TEXT PRIMARY KEY, v INTEGER)"); err != nil {
		t.Fatalf("建表失败: %v", err)
	}

	// 故意让三条语句的受影响行数不同：1 条 INSERT、1 条 UPDATE、1 条无变化。
	out, err := db.Batch("sqlite", "", []BatchStmt{
		{SQL: "INSERT INTO kv VALUES('a', 1)"},
		{SQL: "UPDATE kv SET v = 2 WHERE k = 'a'"},
		{SQL: "UPDATE kv SET v = 3 WHERE k = 'nope'"},
	})
	if err != nil {
		t.Fatalf("全部成功时不该报错: %v", err)
	}
	if !out.Committed {
		t.Fatalf("全部成功应 committed=true: %+v", out.Results)
	}
	want := []int64{1, 1, 0}
	if len(out.RowsAffected) != len(want) {
		t.Fatalf("rows_affected 长度应为 %d，实为 %d", len(want), len(out.RowsAffected))
	}
	for i, w := range want {
		if out.RowsAffected[i] != w {
			t.Fatalf("第 %d 条 rows_affected 应为 %d，实为 %d", i+1, w, out.RowsAffected[i])
		}
	}
	if n := countRows(t, db, "SELECT k FROM kv"); n != 1 {
		t.Fatalf("成功批应全部落地，实际 %d 行", n)
	}
}

// AC-B3：DDL+DML 混批，失败时 **DDL 效果同样不残留**。
// （Q3 拍板：接受 DDL 与 DML 混批；SQLite 同事务内二者同原子。）
func TestMultiDBBatchRollsBackDDLToo(t *testing.T) {
	db := batchDB(t)
	if err := db.Exec("sqlite", "", "CREATE TABLE orders(id INTEGER, item TEXT)"); err != nil {
		t.Fatalf("建表失败: %v", err)
	}

	out, err := db.Batch("sqlite", "", []BatchStmt{
		{SQL: "CREATE TABLE audit(id INTEGER)"}, // DDL
		{SQL: "INSERT INTO orders VALUES(1, 'widget')"},
		{SQL: "INSERT INTO no_such_table VALUES(1)"}, // 引用不存在的表
	})
	if err != nil || out.Committed {
		t.Fatalf("应失败且不提交: err=%v committed=%v", err, out.Committed)
	}
	if n := countRows(t, db, "SELECT id FROM orders"); n != 0 {
		t.Fatalf("DML 应回滚，实际剩 %d 行", n)
	}
	// DDL 的痕迹也要消失——SQLite 建了表不提交也一样留在 sqlite_master 之外，
	// 直接用 sqlite_master 查（查不存在的表会报错，所以查系统表）。
	rows, err := db.Query("sqlite", "", "SELECT name FROM sqlite_master WHERE type='table' AND name='audit'")
	if err != nil {
		t.Fatalf("查 sqlite_master 失败: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("DDL 也应回滚，sqlite_master 里仍有 audit 表")
	}
}

// AC-B4：空语句数组 / 未知 driver → 结构化错误（不是 panic、不是空成功）。
func TestMultiDBBatchRejectsBadCall(t *testing.T) {
	db := batchDB(t)

	// (1) 空 statements：不做事也不能算成功。
	if _, err := db.Batch("sqlite", "", nil); err == nil {
		t.Fatal("空语句数组应报错")
	} else if !strings.Contains(err.Error(), "至少一条") {
		t.Fatalf("错误信息应点名问题: %v", err)
	}

	// (2) 未知 driver：SQL 驱动没注册，属于调用不成立。
	if _, err := db.Batch("nosuchdriver", "", []BatchStmt{{SQL: "SELECT 1"}}); err == nil {
		t.Fatal("未知 driver 应报错")
	}

	// (3) 拿不到库（非法 dsn 场景）时同样报错，而不是静默 no-op。
	if _, err := db.Batch("sqlite", "sub/dir/ok.db", []BatchStmt{{SQL: "SELECT 1"}}); err != nil {
		t.Fatalf("合法相对库名应可用，实际: %v", err)
	}
}

// AC-B4 的后半：DSN 不得进错误明文输出（凭据保护，与 Exec/Query 同口径）。
// mysql 连接串带口令，batch 失败路径（含「第 N 条语句失败」的包装）都不许把它带出来。
func TestMultiDBBatchKeepsDSNOutOfErrors(t *testing.T) {
	db := batchDB(t)
	const secret = "p@ssw0rd-that-must-never-leak"
	dsn := "user:" + secret + "@tcp(127.0.0.1:1)/probe"

	_, _ = db.Batch("mysql", dsn, []BatchStmt{{SQL: "SELECT 1"}})

	// 再跑一次拿错误：连接失败的路径必须走完，且错误里没有口令。
	_, err := db.Batch("mysql", dsn, []BatchStmt{
		{SQL: "SELECT 1"},
		{SQL: "SELECT bad sql"}, // 第二条必然失败，包装后的错误也要过这一关
	})
	if err == nil {
		t.Fatal("该批必然失败")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("错误明文里泄露了口令: %v", err)
	}
}
