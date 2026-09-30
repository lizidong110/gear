package cap

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	_ "github.com/go-sql-driver/mysql" // mysql/mariadb 驱动
	_ "github.com/jackc/pgx/v5/stdlib" // postgres 驱动（注册名 pgx）
	_ "modernc.org/sqlite"             // 纯 Go SQLite 驱动，注册名 "sqlite"，无需 cgo
)

// MultiDB 是 cap.DB 原语的多后端实现（PRD §6 / 核心原子「尽量原始」）。
//
// 一次调用通过 driver/dsn 路由到 sqlite / mysql / postgres 任一后端；连接按
// (driver, dsn) 懒开并缓存复用，避免每次调用重建连接的开销与握手。这与「结构化数据
// 读写不绑定具体产品」的核心原子取向一致——同样的 db.query 既能落本地 SQLite 文件，
// 也能直连生产 MySQL / Postgres，差异只是参数，不是新 Tool。
//
// driver 别名（大小写不敏感）：
//
//	sqlite / sqlite3           -> modernc.org/sqlite（纯 Go，无 cgo）
//	mysql / mariadb            -> go-sql-driver/mysql
//	postgres / postgresql / pg -> jackc/pgx/v5/stdlib
//
// dsn 缺省（空串）且 driver 为 sqlite 时，回退到引擎注入的 defaultSQLiteDSN
// （每引擎唯一的内存 scratch 库，进程级共享、重启即丢，保留 M5 scratch 语义，
// 便于测试/Tool 内临时状态）。mysql/postgres 必须显式传 dsn——含口令时优先用
// Flow 里的 ${secrets.*} 或 env，不要硬编码进 YAML。
//
// sqliteRoot 是「应用给相对库名时的落点根目录」（宿主装配 gearhome.AppDataDir）。
//
// 为什么需要它（2026-09-25 实测缺陷）：相对 dsn 此前由 SQLite 按**子进程 cwd** 解析，
// 而 Lua 沙箱禁了 os 库、模板也没有 ${env} 前缀，脚本读不到 GEAR_HOME——应用想持久化
// 就只能把绝对路径硬编码进 YAML（mindstorm_data.yaml 里 18 处
// `local DSN = "D:/gadern/gear/build/mindstorm.db"`），整个仓库拷到别的机器就跑不起来。
// 有了 root，「相对库名」=「应用数据目录下的同名库」，与 cwd、与机器都无关。
// root 为空表示宿主未装配：此时**不加工** dsn（保留历史行为，不静默改变语义）。
type MultiDB struct {
	defaultSQLiteDSN string
	sqliteRoot       string
	mu               sync.Mutex
	pool             map[string]*sql.DB
}

// NewMultiDB 构造多后端 DB 实现。defaultSQLiteDSN 是「db 调用省略 dsn 时的 sqlite 默认库」，
// 引擎默认宿主传每引擎唯一的内存 DSN（见 engine.defaultProviders）。sqliteRoot 是相对库名
// 的落点根目录（见类型注释；传空串表示不加工相对 dsn）。
// 函数永不返回 nil，故 Providers.DB 用接口承载时不会出现 typed-nil 触发 Context.DB() 误判。
func NewMultiDB(defaultSQLiteDSN, sqliteRoot string) *MultiDB {
	return &MultiDB{defaultSQLiteDSN: defaultSQLiteDSN, sqliteRoot: sqliteRoot, pool: make(map[string]*sql.DB)}
}

// resolveSQLiteDSN 把 sqlite 的「相对库名」锚定到 sqliteRoot（仅在此处做路径加工）。
//
// 原样放过的形态（绝不擅自改写调用方已经明确给出的东西）：
//   - 绝对路径（含 Windows 盘符路径）；
//   - URI 形态：`file:...`、`xxx://...`、带 `?` 查询串（mode=memory 等参数属于这一族）；
//   - 含 `:` 的其它形态（`user:pass@tcp(...)/db` 之类，非 sqlite 也会走到这里）。
func (m *MultiDB) resolveSQLiteDSN(dsn string) (string, error) {
	if m.sqliteRoot == "" || dsn == "" || filepath.IsAbs(dsn) {
		return dsn, nil
	}
	if strings.Contains(dsn, "://") || strings.HasPrefix(dsn, "file:") || strings.ContainsRune(dsn, '?') {
		return dsn, nil
	}
	if strings.ContainsRune(dsn, ':') {
		// Windows 的驱动器相对路径（`C:foo`）与类 Unix 的 `host:port/db` 都不该被拼进目录。
		return dsn, nil
	}
	if runtime.GOOS == "windows" && strings.ContainsAny(dsn, `/\`) && len(dsn) > 1 && dsn[1] == ':' {
		return dsn, nil // `C:/x/y.db`——IsAbs 在个别形态下不可靠，这里再挡一次
	}
	target, err := filepath.Abs(filepath.Join(m.sqliteRoot, filepath.FromSlash(dsn)))
	if err != nil {
		return dsn, fmt.Errorf("应用库 %q 落点解析失败: %w", dsn, err)
	}
	// SQLite 不会替你建目录：目录不存在时它报的是「unable to open database file」，
	// 看不出真正原因。这里显式建（与 ConfigStore.open 同口径）。
	if dir := filepath.Dir(target); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return dsn, fmt.Errorf("创建应用数据目录 %q 失败: %w", dir, err)
		}
	}
	return target, nil
}

// normDriver 把 driver 别名归一为 database/sql 的驱动注册名。
func normDriver(driver string) string {
	switch strings.ToLower(driver) {
	case "", "sqlite", "sqlite3":
		return "sqlite"
	case "mysql", "mariadb":
		return "mysql"
	case "postgres", "postgresql", "pg":
		return "pgx"
	default:
		return strings.ToLower(driver)
	}
}

// db 按 (driver, dsn) 取得（必要时懒开并缓存）一个 *sql.DB 连接池。
// 并发安全：锁仅覆盖查/建缓存这一段；sqlite 单连接（SetMaxOpenConns(1)）序列化写、
// 保证与原 SQLiteDB 一致的行为确定性；mysql/postgres 放开连接数。
func (m *MultiDB) db(driver, dsn string) (*sql.DB, error) {
	drv := normDriver(driver)
	if drv == "sqlite" {
		if dsn == "" {
			dsn = m.defaultSQLiteDSN
		} else {
			resolved, err := m.resolveSQLiteDSN(dsn)
			if err != nil {
				return nil, err
			}
			dsn = resolved
		}
	}
	if dsn == "" {
		return nil, fmt.Errorf("db(%s) 需要 dsn：mysql/postgres 必须显式连接串", drv)
	}
	key := drv + "\x00" + dsn
	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok := m.pool[key]; ok {
		return c, nil
	}
	c, err := sql.Open(drv, dsn)
	if err != nil {
		return nil, fmt.Errorf("打开 %s 连接失败: %w", drv, err)
	}
	if drv == "sqlite" {
		c.SetMaxOpenConns(1) // 单连接：确定性与并发安全（SQLite 共享缓存边界）
		if _, err := c.Exec("PRAGMA busy_timeout=5000"); err != nil {
			_ = c.Close()
			return nil, fmt.Errorf("初始化 %s 连接失败: %w", drv, err)
		}
	} else {
		c.SetMaxOpenConns(10)
		c.SetConnMaxLifetime(5 * time.Minute)
	}
	m.pool[key] = c
	return c, nil
}

// Query 参数化查询，返回 []interface{}（每行一个 map[string]interface{}）。
func (m *MultiDB) Query(driver, dsn, query string, args ...interface{}) ([]interface{}, error) {
	c, err := m.db(driver, dsn)
	if err != nil {
		return nil, err
	}
	return queryRows(c, query, args...)
}

// Exec 执行 DDL/DML（INSERT/UPDATE/DELETE/CREATE 等），不返回行。
func (m *MultiDB) Exec(driver, dsn, query string, args ...interface{}) error {
	c, err := m.db(driver, dsn)
	if err != nil {
		return err
	}
	if _, err := c.Exec(query, args...); err != nil {
		return fmt.Errorf("db exec 失败: %w", err)
	}
	return nil
}

// db.batch 的错误码（与 internal/mcp/guide/errors.md 同一张表；doc_audit ③ 双向核对
// 认这些常量名，故码的**字面量只在常量里出现一次**，错误包装里一律引用常量）。
const (
	CodeTxBeginFailed    = "TX_BEGIN_FAILED"
	CodeTxCommitFailed   = "TX_COMMIT_FAILED"
	CodeTxBatchFailed    = "TX_BATCH_FAILED"
	CodeTxRollbackFailed = "TX_ROLLBACK_FAILED"
)

// Batch 在一个事务里顺序执行多条语句，**全或无**：任一条失败就整体回滚，
// 连已经执行过的 DDL 也不残留（SQLite 在同一事务内 DDL 与 DML 同原子）。
//
// 与 Registry 的 withTx 同姿势（defer tx.Rollback() 在 Commit 后是 no-op），差别只在
// 事务取自本包的连接池入口 m.db()，因此 DSN 归一化 / 建目录 / 连接池缓存全部复用；
// 不复用 Registry.withTx——那是另一套库（registry.db），且带着快照语义。
//
// 语句失败返回 (outcome{Committed:false}, nil)——失败是业务结果，不是调用错误，
// 详见 cap.DB 接口上的取舍说明。错误值只表达「调用本身不成立」，且从不把 dsn
// 拼进错误消息（凭据保护，与 Exec/Query 同口径）。
func (m *MultiDB) Batch(driver, dsn string, stmts []BatchStmt) (BatchOutcome, error) {
	if len(stmts) == 0 {
		return BatchOutcome{}, fmt.Errorf("db.batch 需要至少一条语句（statements 不能为空）")
	}
	c, err := m.db(driver, dsn)
	if err != nil {
		return BatchOutcome{}, err
	}
	tx, err := c.Begin()
	if err != nil {
		return BatchOutcome{}, fmt.Errorf("%s: db.batch 开启事务失败: %w", CodeTxBeginFailed, err)
	}
	defer tx.Rollback()

	results := make([]BatchResult, 0, len(stmts))
	fail := func(i int, msg string) (BatchOutcome, error) {
		for len(results) <= i {
			results = append(results, BatchResult{})
		}
		results[i] = BatchResult{Error: msg}
		return batchOutcome(results, false), nil
	}
	for i, s := range stmts {
		if strings.TrimSpace(s.SQL) == "" {
			return fail(i, "语句为空（sql 不能是空白串）")
		}
		res, err := tx.Exec(s.SQL, s.Params...)
		if err != nil {
			// 回滚失败是异常路径：事务可能**部分生效**，此时把 outcome 当「没写进去」返回
			// 就是静默降级，必须让调用方看见。
			if rerr := rollbackTx(tx); rerr != nil {
				return BatchOutcome{}, rerr
			}
			return fail(i, fmt.Sprintf("%s: 第 %d 条语句失败: %v", CodeTxBatchFailed, i+1, err))
		}
		n, _ := res.RowsAffected()
		results = append(results, BatchResult{RowsAffected: n})
	}
	if err := tx.Commit(); err != nil {
		if rerr := rollbackTx(tx); rerr != nil {
			return BatchOutcome{}, rerr
		}
		return BatchOutcome{}, fmt.Errorf("%s: db.batch 提交失败: %w", CodeTxCommitFailed, err)
	}
	return batchOutcome(results, true), nil
}

// Tx 在**一条事务**里执行 fn，fn 返回错误即回滚并返回该错误（fn 内的写入按未发生处理）；
// fn 返回 nil 才提交。
//
// 为什么需要它（2026-09-28，ENTERPRISE_PRD FR-2b 迁移）：`Batch` 只能跑「已拆好的语句数组」，
// 而迁移的 sql 是一整段可能含多条语句的文本，且必须**整段原子**——半截 schema 落库比
// 不落库更糟（后续迁移会照着它写，错得更远）。只有本方法给得了这个语义。
//
// 与 Registry.withTx 是**两套连接**（这里是应用库，那是 registry.db），不像也没有要像：
// 复用点各不相同，硬凑一个通用层只会把「哪个库、带不带删除快照」这层差异藏起来。
//
// ⚠ MySQL 的 DDL 会隐式提交事务（`CREATE TABLE` 之后 tx 就不再是原子的），本方法在
// MySQL 上保证不了「整段迁移原子」。 documented 在 PRD 与 guide/atoms.md，不写在代码注释里——
// 代码注释挡不住有人拿它当保证用。
func (m *MultiDB) Tx(driver, dsn string, fn func(tx *sql.Tx) error) error {
	c, err := m.db(driver, dsn)
	if err != nil {
		return err
	}
	tx, err := c.Begin()
	if err != nil {
		return fmt.Errorf("%s: 开启事务失败: %w", CodeTxBeginFailed, err)
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		if rerr := rollbackTx(tx); rerr != nil {
			return rerr
		}
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("%s: 提交事务失败（事务未生效）: %w", CodeTxCommitFailed, err)
	}
	return nil
}

// rollbackTx 回滚事务，把「回滚本身失败」说出口。
// Commit 成功/失败后 Rollback 返回 sql.ErrTxDone（no-op），不算失败。
func rollbackTx(tx *sql.Tx) error {
	if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		return fmt.Errorf("%s: db.batch 回滚失败（事务可能部分生效）: %w", CodeTxRollbackFailed, err)
	}
	return nil
}

// batchOutcome 把逐条结果收成整批结果（rows_affected 数组由 results 派生，避免两处维护）。
func batchOutcome(results []BatchResult, committed bool) BatchOutcome {
	ra := make([]int64, 0, len(results))
	for _, r := range results {
		ra = append(ra, r.RowsAffected)
	}
	return BatchOutcome{Results: results, RowsAffected: ra, Committed: committed}
}

// Ping 探活（mysql/postgres 跨网络连得通、sqlite 文件可打开即 ok）。
func (m *MultiDB) Ping(driver, dsn string) error {
	c, err := m.db(driver, dsn)
	if err != nil {
		return err
	}
	if err := c.Ping(); err != nil {
		return fmt.Errorf("db ping 失败: %w", err)
	}
	return nil
}

// Close 关闭全部缓存连接（引擎默认宿主一般不主动关；测试收尾用）。
func (m *MultiDB) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	var firstErr error
	for k, c := range m.pool {
		if err := c.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		delete(m.pool, k)
	}
	return firstErr
}

// queryRows 把查询结果规整成 []interface{}（行集合），类型按 normalizeSQLValue 归一。
func queryRows(c *sql.DB, query string, args ...interface{}) ([]interface{}, error) {
	rows, err := c.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("db query 失败: %w", err)
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	out := make([]interface{}, 0)
	for rows.Next() {
		vals := make([]interface{}, len(cols))
		ptrs := make([]interface{}, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, fmt.Errorf("db scan 失败: %w", err)
		}
		row := make(map[string]interface{}, len(cols))
		for i, cname := range cols {
			row[cname] = normalizeSQLValue(vals[i])
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db query 遍历失败: %w", err)
	}
	return out, nil
}

// normalizeSQLValue 把驱动返回的原生类型归一成 JSON 友好的值。
func normalizeSQLValue(v interface{}) interface{} {
	switch x := v.(type) {
	case []byte:
		return string(x)
	case time.Time:
		return x.Format(time.RFC3339)
	default:
		return x
	}
}
