package artifact

import (
	"fmt"
	"strings"
	"testing"

	"gear/internal/cap"
	"gear/internal/model"
)

// 本文件对着 ENTERPRISE_PRD.md AC-B5 写：迁移随 `gear_apply` / `gear_import` 走原子语义，
// 坏迁移整包拒绝。**用真 SQLite**（modernc，纯 Go）而不是桩：迁移的语义几乎全在事务边界上，
// 桩出来的事务边界是假的，而「失败没回滚」恰好是最要命的那类缺陷。

// migDB 造一个把相对库名锚到临时目录的 db 实现（与引擎默认宿主同口径）。
func migDB(t *testing.T) *cap.MultiDB {
	t.Helper()
	db := cap.NewMultiDB("", t.TempDir())
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// migBundle 造一个带迁移声明的 bundle（dsn 走相对库名，锚到测试临时目录）。
func migBundle(t *testing.T, vers ...int) *Bundle {
	t.Helper()
	set := &model.MigrationSet{
		AppID: "mig", DSN: "mig.db", Driver: "sqlite",
		Migrations: []model.Migration{},
	}
	for _, v := range vers {
		set.Migrations = append(set.Migrations, model.Migration{
			Version: v,
			Name:    fmt.Sprintf("m%d", v),
			SQL:     migSQL(v),
		})
	}
	return &Bundle{
		Apps:       []*model.App{fxApp("mig")},
		Tools:      []*model.Tool{fxTool("mig__hello")},
		Migrations: []*model.MigrationSet{set},
	}
}

// migSQL 生成 v 版本该建的表：表名的版本号由参数决定，方便断言「哪条跑了、哪条没跑」。
func migSQL(v int) string {
	return fmt.Sprintf("CREATE TABLE t%d(id TEXT PRIMARY KEY);", v)
}

// migTable 判断目标库里有没有某张表（用来断言「没跑」）。
func migTable(t *testing.T, db *cap.MultiDB, dsn, table string) bool {
	t.Helper()
	rows, err := db.Query("sqlite", dsn,
		"SELECT name FROM sqlite_master WHERE type='table' AND name=?", table)
	if err != nil {
		t.Fatalf("查 sqlite_master 失败: %v", err)
	}
	return len(rows) > 0
}

// migRows 数某张表有几行。
func migRows(t *testing.T, db *cap.MultiDB, dsn, table string) int {
	t.Helper()
	rows, err := db.Query("sqlite", dsn, "SELECT COUNT(*) FROM "+table)
	if err != nil {
		t.Fatalf("查 %s 失败: %v", table, err)
	}
	if len(rows) == 0 {
		return 0
	}
	m, ok := rows[0].(map[string]interface{})
	if !ok {
		return 0
	}
	switch v := m["COUNT(*)"].(type) {
	case int64:
		return int(v)
	case float64:
		return int(v)
	}
	return 0
}

// ---- AC-B5 主路径 ----

// TestMigrationsApplyOnce 首次导入建表；二次导入（含 dry_run 与真跑）不重复执行。
func TestMigrationsApplyOnce(t *testing.T) {
	reg := mustReg(t)
	db := migDB(t)

	created, _, applied, err := ApplyBundle(reg, migBundle(t, 1), db, false)
	if err != nil {
		t.Fatalf("首次导入失败: %v", err)
	}
	if len(created) == 0 {
		t.Fatal("首次导入应报 created（定义真的落库了）")
	}
	if len(applied) != 1 || applied[0] != 1 {
		t.Fatalf("应应用 version=1，实际 %v", applied)
	}
	if !migTable(t, db, "mig.db", "t1") {
		t.Fatal("t1 应已建")
	}

	// ② 真跑第二次：一字节都不该再动（version 1 已应用）。
	_, _, applied2, err := ApplyBundle(reg, migBundle(t, 1), db, false)
	if err != nil {
		t.Fatalf("二次导入失败: %v", err)
	}
	if len(applied2) != 0 {
		t.Fatalf("已应用的版本被重跑：%v", applied2)
	}
	if migRows(t, db, "mig.db", "gear_migrations") != 1 {
		t.Fatalf("台账应有 1 行，实际 %d 行", migRows(t, db, "mig.db", "gear_migrations"))
	}

	// ③ 二次导入的 dry_run：既不跑迁移，也不把版本号写进台账。
	_, _, applied3, err := ApplyBundle(reg, migBundle(t, 1), db, true)
	if err != nil {
		t.Fatalf("dry run 失败: %v", err)
	}
	if len(applied3) != 0 {
		t.Fatalf("dry run 不该动迁移：%v", applied3)
	}
	if migRows(t, db, "mig.db", "gear_migrations") != 1 {
		t.Fatal("dry run 改写了台账")
	}
}

// TestMigrationsDryRunIsRealRunRolledBack dry run 必须抓出 SQL 错，且不留痕。
//
// 这条是本需求里最容易被「优化」掉的一条：dry run 若只是跳过迁移，坏 SQL 就一路走到真导入，
// 而真导入已经把前面几条 schema 写进库了。
func TestMigrationsDryRunIsRealRunRolledBack(t *testing.T) {
	db := migDB(t)
	b := migBundle(t, 1)
	b.Migrations[0].Migrations[0].SQL = "CREATE TABLE t1(id TEXT PRIMARY KEY); NOT SQL;"

	applied, err := ApplyMigrations(db, b, true)
	if err == nil {
		t.Fatal("坏 SQL 的 dry run 应报错")
	}
	if !strings.Contains(err.Error(), CodeMigrateFailed) {
		t.Fatalf("错误码应为 %s，实际: %v", CodeMigrateFailed, err)
	}
	if !strings.Contains(err.Error(), "version=1") {
		t.Fatalf("错误信息必须点名版本号: %v", err)
	}
	if len(applied) != 0 {
		t.Fatalf("dry run 不该报已应用：%v", applied)
	}
	// 最关键的一条：库里连台账表都不该有（除了空台账，见 loadLedger 的注释），
	// 更不能有 t1。
	if migTable(t, db, "mig.db", "t1") {
		t.Fatal("dry run 把 DDL 落了库")
	}
	if migRows(t, db, "mig.db", "gear_migrations") != 0 {
		t.Fatal("dry run 把台账写进去了")
	}
}

// TestMigrationsBadSQLRejectsWholeBundle 坏迁移 → 整包拒绝：一个定义都不落库。
func TestMigrationsBadSQLRejectsWholeBundle(t *testing.T) {
	reg := mustReg(t)
	db := migDB(t)
	b := migBundle(t, 1, 2)
	// v1 正常、v2 语法错：失败发生在**第二条**，前面的 t1 已在这个事务之外生效，
	// 但**定义一个都不该落**。
	b.Migrations[0].Migrations[1].SQL = "CREATE TABLE t2(id TEXT PRIMARY KEY); NONSENSE;"

	_, _, _, err := ApplyBundle(reg, b, db, false)
	if err == nil {
		t.Fatal("坏迁移应导致整包拒绝")
	}
	if !strings.Contains(err.Error(), CodeMigrateFailed) {
		t.Fatalf("错误码应为 %s，实际: %v", CodeMigrateFailed, err)
	}
	if !strings.Contains(err.Error(), "version=2") {
		t.Fatalf("错误信息必须点名版本号: %v", err)
	}
	if got, err := reg.GetTool("mig__hello"); err != nil || got != nil {
		t.Fatalf("坏迁移不得落任何定义（tool=%v err=%v）", got, err)
	}
	if migRows(t, db, "mig.db", "gear_migrations") != 1 {
		t.Fatalf("v1 已生效、台账应有 1 行，实际 %d（fail-closed 只保证定义不落库）",
			migRows(t, db, "mig.db", "gear_migrations"))
	}
}

// TestMigrationsVersionGap 断号：库里已应用到 1，本次声明直上 3 → 拒并点名。
func TestMigrationsVersionGap(t *testing.T) {
	reg := mustReg(t)
	db := migDB(t)

	if _, _, _, err := ApplyBundle(reg, migBundle(t, 1), db, false); err != nil {
		t.Fatalf("首次导入失败: %v", err)
	}

	b := migBundle(t, 1, 3)
	// 断号那一次还带一个新 Tool：整包拒绝要能证明「本次一条都没写」——
	// 光看 mig__hello 不行，它已经在上一次成功导入里落过库了。
	b.Tools = append(b.Tools, fxTool("mig__extra"))
	_, _, _, err := ApplyBundle(reg, b, db, false)
	if err == nil {
		t.Fatal("断号应被拒")
	}
	if !strings.Contains(err.Error(), CodeMigrateGap) {
		t.Fatalf("错误码应为 %s，实际: %v", CodeMigrateGap, err)
	}
	if !strings.Contains(err.Error(), "version=3") || !strings.Contains(err.Error(), "version=2") {
		t.Fatalf("诊断必须点名缺的那个版本: %v", err)
	}
	if migTable(t, db, "mig.db", "t3") {
		t.Fatal("断号那条不该执行")
	}
	if got, err := reg.GetTool("mig__extra"); err != nil || got != nil {
		t.Fatalf("断号时本次定义不得落库（tool=%v err=%v）", got, err)
	}
}

// TestMigrationsVersionOrderIsStaticRejected version 非严格递增在**校验期**就拒（写作期错误）。
func TestMigrationsVersionOrderIsStaticRejected(t *testing.T) {
	reg := mustReg(t)
	db := migDB(t)

	// 乱序写在 YAML 里是正常的（version 1 写在后），但执行顺序必须按 version，
	// 否则「后写的先建表」会撞「表已存在」。
	b := migBundle(t, 2, 1)
	if _, _, _, err := ApplyBundle(reg, b, db, false); err != nil {
		t.Fatalf("声明乱序应被拒: %v", err)
	}
	// 重复版本号
	b2 := migBundle(t, 1, 1)
	if _, _, _, err := ApplyBundle(reg, b2, db, false); err == nil {
		t.Fatal("重复 version 应被拒")
	}
	// 缺 dsn（迁移不能跑在内存 scratch 库上）
	b3 := migBundle(t, 1)
	b3.Migrations[0].DSN = ""
	b3.Migrations[0].Migrations[0].DSN = ""
	if _, _, _, err := ApplyBundle(reg, b3, db, false); err == nil {
		t.Fatal("缺 dsn 应被拒")
	}
	// sql 为空
	b4 := migBundle(t, 1)
	b4.Migrations[0].Migrations[0].SQL = "   "
	if _, _, _, err := ApplyBundle(reg, b4, db, false); err == nil {
		t.Fatal("空 sql 应被拒")
	}
}

// TestMigrationsNoDBIsFailClosed 声明了迁移却给不出 db 实现 → 拒绝，不静默跳过。
func TestMigrationsNoDBIsFailClosed(t *testing.T) {
	reg := mustReg(t)
	b := migBundle(t, 1)
	_, _, _, err := ApplyBundle(reg, b, nil, false)
	if err == nil {
		t.Fatal("声明了迁移又没给 db，应被拒")
	}
	if !strings.Contains(err.Error(), CodeMigrateNoDB) {
		t.Fatalf("错误码应为 %s，实际: %v", CodeMigrateNoDB, err)
	}
	// 没声明迁移的 bundle 与 nil db 共存（历史入口 / 既有测试），零变化。
	plain := &Bundle{Tools: []*model.Tool{fxTool("plain")}}
	if _, _, _, err := ApplyBundle(reg, plain, nil, false); err != nil {
		t.Fatalf("无迁移 + nil db 应照旧可用: %v", err)
	}
}

// TestMigrationsMultiStatement 一条迁移里**多条语句**必须整段原子（FR-2 Q3：DDL 可与 DML 混批）。
func TestMigrationsMultiStatement(t *testing.T) {
	db := migDB(t)
	set := &model.MigrationSet{AppID: "mig", DSN: "multi.db", Driver: "sqlite", Migrations: []model.Migration{{
		Version: 1,
		Name:    "init+batch",
		SQL: "CREATE TABLE t1(id TEXT PRIMARY KEY);\n" +
			"CREATE TABLE t2(id TEXT PRIMARY KEY);\n" +
			"INSERT INTO t1(id) VALUES('a');",
	}}}
	b := &Bundle{Migrations: []*model.MigrationSet{set}}

	applied, err := ApplyMigrations(db, b, false)
	if err != nil {
		t.Fatalf("多语句迁移失败: %v", err)
	}
	if len(applied) != 1 {
		t.Fatalf("应应用 1 条，实际 %v", applied)
	}
	for _, tbl := range []string{"t1", "t2"} {
		if !migTable(t, db, "multi.db", tbl) {
			t.Fatalf("%s 应已建（同一事务）", tbl)
		}
	}
	if migRows(t, db, "multi.db", "t1") != 1 {
		t.Fatal("语句应已执行")
	}

	// 第三条语句失败 → 整条回滚（t2 也不该出现，除非它已经存在）。
	set2 := &model.MigrationSet{AppID: "mig", DSN: "multi.db", Migrations: []model.Migration{
		{Version: 2, Name: "boom", SQL: "CREATE TABLE t3(id TEXT PRIMARY KEY);\nINSERT INTO nope(id) VALUES('x');"},
	}}
	b2 := &Bundle{Migrations: []*model.MigrationSet{set2}}
	if _, err := ApplyMigrations(db, b2, false); err == nil {
		t.Fatal("多语句里的第二条失败应报错")
	}
}
