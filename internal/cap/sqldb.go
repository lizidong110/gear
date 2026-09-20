package cap

import (
	"database/sql"
	"fmt"
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
type MultiDB struct {
	defaultSQLiteDSN string
	mu               sync.Mutex
	pool             map[string]*sql.DB
}

// NewMultiDB 构造多后端 DB 实现。defaultSQLiteDSN 是「db 调用省略 dsn 时的 sqlite 默认库」，
// 引擎默认宿主传每引擎唯一的内存 DSN（见 engine.defaultProviders）。函数永不返回 nil，
// 故 Providers.DB 用接口承载时不会出现 typed-nil 触发 Context.DB() 误判。
func NewMultiDB(defaultSQLiteDSN string) *MultiDB {
	return &MultiDB{defaultSQLiteDSN: defaultSQLiteDSN, pool: make(map[string]*sql.DB)}
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
	if drv == "sqlite" && dsn == "" {
		dsn = m.defaultSQLiteDSN
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
