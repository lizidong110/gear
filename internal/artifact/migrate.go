package artifact

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"gear/internal/model"
)

// 迁移错误码（口径锁：这些码的**单一真相源**在 internal/mcp/guide/errors.md；
// 那里收一个码就必须有代码里的产生点，反之亦然——tools/doc_audit.py 双向核）。
const (
	// CodeMigrateInvalid 是迁移声明本身不合法（version 非严格递增、sql 空、缺 dsn）。
	CodeMigrateInvalid = "MIGRATE_INVALID"
	// CodeMigrateGap 是声明的 version 与库里已应用的版本接不上（中间缺号）。
	CodeMigrateGap = "MIGRATE_VERSION_GAP"
	// CodeMigrateFailed 是某条迁移的 SQL 执行失败（或台账写不进去）。
	CodeMigrateFailed = "MIGRATE_SQL_FAILED"
	// CodeMigrateNoDB 是声明了迁移但入口没给出 db 实现（fail-closed：不猜一个库来跑）。
	CodeMigrateNoDB = "MIGRATE_NO_DB"
)

// migrationsTable 是迁移台账表名。它建在**目标库里**，不是 registry.db——
// 台账与 schema 同生共死，分开存只会让「改到一半」多出一种状态。
const migrationsTable = "gear_migrations"

// Migrator 是执行迁移所需的**最小** db 面（*cap.MultiDB 实现它）。
//
// 用接口而不是 *cap.MultiDB：apply 这一层不该知道 SQLite 在哪；更重要的是迁移的语义
// 几乎全在**事务边界**上，测试必须用真库——mock 出来的事务边界是假的，而假的边界
// 恰好会放过「失败没回滚」这类最要命的缺陷。
type Migrator interface {
	Query(driver, dsn, query string, args ...interface{}) ([]interface{}, error)
	Exec(driver, dsn, query string, args ...interface{}) error
	// Tx 在单事务里执行 fn：fn 返回错误即回滚。
	Tx(driver, dsn string, fn func(tx *sql.Tx) error) error
}

// ApplyMigrations 按声明顺序执行 bundle 里**尚未应用**的迁移，返回本次真正跑过的版本号。
//
// 三条硬口径（每条都有代价，写在这里是为了防止日后被「顺手优化」掉）：
//
//  1. **已应用的版本不重跑**：台账在目标库里，重复导入 / 重复启动 / dry_run 都幂等。
//  2. **fail-closed**：任一条失败即返回错误，并且**定义一个都不写**（见 ApplyBundle 的顺序），
//     绝不以「上一代 schema」启动应用。每条迁移各在**自己的事务**里，失败即整条回滚。
//  3. **dryRun 是真跑 + 全回滚**，不是「跳过」：SQL 语法错因此能在 dry_run 阶段被抓出来
//     （AC-B5），而不是等真导入才炸、且已经改了半个库。代价：MySQL 的 DDL 会隐式提交，
//     那条路上 dry_run 保护不住——文档写明，代码里不假装。
//
// 返回值是「本次走完执行流程的版本号」。dryRun=true 时它表示**会跑但已回滚**（即「将生效」），
// 不是「已生效」——调用方报给用户的文案必须区分这两者，把 will-run 说成 applied 就是撒谎。
func ApplyMigrations(db Migrator, b *Bundle, dryRun bool) ([]int, error) {
	if len(b.Migrations) == 0 {
		return nil, nil
	}
	if db == nil {
		return nil, fmt.Errorf("%s: bundle 声明了 migrations 但本次导入未提供 db 实现"+
			"（拒绝以「声明了迁移却没跑」的方式落库）", CodeMigrateNoDB)
	}
	var applied []int
	for _, set := range b.Migrations {
		if err := set.Validate(); err != nil {
			return nil, fmt.Errorf("%s: %w", CodeMigrateInvalid, err)
		}
		ran, err := applySet(db, set, dryRun)
		if err != nil {
			return nil, err
		}
		applied = append(applied, ran...)
	}
	sort.Ints(applied)
	return applied, nil
}

// target 是一条迁移与它最终落哪个库。
type target struct {
	driver, dsn string
	m           model.Migration
}

// applySet 执行一份迁移文档，返回本次跑过的版本号（dryRun 恒为空）。
func applySet(db Migrator, set *model.MigrationSet, dryRun bool) ([]int, error) {
	// 按 version 升序执行。Validate 只保证「严格递增」，这里仍要显式排一次：
	// YAML 里手写的顺序与 version 顺序是两回事，而 SQL 常引用上一条建的表。
	ordered := make([]model.Migration, len(set.Migrations))
	copy(ordered, set.Migrations)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Version < ordered[j].Version })

	var queue []target
	for _, m := range ordered {
		driver, dsn := m.Target(*set)
		queue = append(queue, target{driver: driver, dsn: dsn, m: m})
	}

	// 台账表先就位，已应用版本先读出——**这一步是提交**，不是 dry run 语义。
	//
	// 为什么不能把「读已应用版本」放进后面那些会被回滚的事务里：dry run 下那样读到的是
	// 回滚后的空台账，于是「库里已有 v1、v2 该跑」会被误判成断号。dry run 的判断必须站在
	// 真实状态上。
	//
	// 代价：目标库原本没有台账时，一次 dry run 会留下**一张空台账表**（不含任何应用数据）。
	// 这是 dry run 唯一会碰的库结构；库里已有 schema 时，本步连空表也不建。
	applied := map[string]map[int]bool{}
	ran := make([]int, 0, len(queue))
	for _, t := range queue {
		k := t.dsn
		if applied[k] == nil {
			vs, err := loadLedger(db, t.driver, t.dsn)
			if err != nil {
				return nil, err
			}
			applied[k] = vs
		}
		if applied[k][t.m.Version] {
			continue // 幂等：已应用的版本不重跑
		}
		// version 必须接在已应用的最大版本之后。断号是最常见的人为错误（从 2 开始重新编号），
		// 而它的症状是「这条 CREATE TABLE 报表已存在」——把原因说清楚比让人猜强。
		maxApplied := 0
		for v := range applied[k] {
			if v > maxApplied {
				maxApplied = v
			}
		}
		if t.m.Version != maxApplied+1 {
			return nil, fmt.Errorf("%s: %s 的迁移断号：本条 %s，库里已应用到 version=%d，"+
				"缺 version=%d（version 必须从 1 起依次 +1）",
				CodeMigrateGap, t.dsn, t.m.Label(), maxApplied, maxApplied+1)
		}
		if err := runOne(db, t, dryRun); err != nil {
			return nil, err
		}
		if !dryRun {
			applied[k][t.m.Version] = true
			ran = append(ran, t.m.Version)
		}
	}
	return ran, nil
}

// loadLedger 建台账表并读出已应用的版本号（这一整步**提交**；理由见 applySet 的注释）。
func loadLedger(db Migrator, driver, dsn string) (map[int]bool, error) {
	out := make(map[int]bool)
	err := db.Tx(driver, dsn, func(tx *sql.Tx) error {
		if _, err := tx.Exec("CREATE TABLE IF NOT EXISTS " + migrationsTable + "(" +
			"version INTEGER PRIMARY KEY, name TEXT, applied_at TEXT)"); err != nil {
			return err
		}
		rows, err := tx.Query("SELECT version FROM " + migrationsTable)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var v int64
			if err := rows.Scan(&v); err != nil {
				return err
			}
			out[int(v)] = true
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("初始化迁移台账 %s 失败: %w", migrationsTable, err)
	}
	return out, nil
}

// runOne 在单事务里执行一条迁移并记台账。dryRun 时事务退出即回滚，不留痕。
func runOne(db Migrator, t target, dryRun bool) error {
	label := t.m.Label()
	return db.Tx(t.driver, t.dsn, func(tx *sql.Tx) error {
		if _, err := tx.Exec(t.m.SQL); err != nil {
			// 回滚已由 Tx 负责（fn 返回错误即回滚）。这里只把「哪条、为什么」说清楚。
			return fmt.Errorf("%s: %s 执行失败: %w", CodeMigrateFailed, label, err)
		}
		if _, err := tx.Exec("INSERT INTO "+migrationsTable+
			"(version,name,applied_at) VALUES(?,?,?)",
			t.m.Version, strings.TrimSpace(t.m.Name), time.Now().UTC().Format(time.RFC3339)); err != nil {
			return fmt.Errorf("%s: %s 生效但台账写不进去（事务已回滚，schema 未生效）: %w",
				CodeMigrateFailed, label, err)
		}
		return nil
	})
}
