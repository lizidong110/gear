package cap

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // 纯 Go SQLite 驱动，注册名 "sqlite"，无需 cgo
)

// SQLiteDB 是 cap.DB 原语的 SQLite 实现（PRD §6 / M5：db 原语此前只有接口、无实现，
// 调用恒返回 ErrCapabilityUnavailable；本文件补齐宿主实现）。
//
// 设计要点：
//   - 复用项目既有的 modernc.org/sqlite（registry 同款纯 Go 驱动，无 cgo 依赖），
//     与内核「单二进制分发、依赖最小」取向一致。
//   - 单连接（SetMaxOpenConns(1)）：结构化数据原语是低吞吐的宿主能力，串行化足以
//     避免 SQLite 多连接下共享缓存 / WAL 的边界问题，同时保证行为确定。
//   - DSN 由调用方决定：文件（持久）或 `file:<name>?mode=memory&cache=shared`（进程内）。
//     引擎默认宿主用「每引擎唯一 DSN 的内存库」，进程级共享、重启即丢（M5  scratch 语义，
//     持久化留待后续切片）；需要落盘时传文件路径即可。
type SQLiteDB struct {
	db *sql.DB
}

// NewSQLiteDB 打开（必要时创建）一个 SQLite 数据库并返回 cap.DB 实现。
// path 为 SQLite DSN：传文件路径持久化，或 `file:cap_<n>?mode=memory&cache=shared` 内存态。
func NewSQLiteDB(path string) (*SQLiteDB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("打开 cap db 失败: %w", err)
	}
	db.SetMaxOpenConns(1) // 单连接：确定性与并发安全（见类型注释）
	if _, err := db.Exec("PRAGMA busy_timeout=5000"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("初始化 cap db 失败: %w", err)
	}
	return &SQLiteDB{db: db}, nil
}

// Query 执行查询，将每行转成 map[string]interface{}（列名 → 值），整体返回 []interface{}。
// 类型归一：BLOB([]byte)→字符串；time.Time→RFC3339；其余（int64/float64/string/nil）原样。
func (d *SQLiteDB) Query(query string, args ...interface{}) ([]interface{}, error) {
	rows, err := d.db.Query(query, args...)
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
		for i, c := range cols {
			row[c] = normalizeSQLValue(vals[i])
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db query 遍历失败: %w", err)
	}
	return out, nil
}

// Exec 执行 DDL/DML（INSERT/UPDATE/DELETE/CREATE 等），不返回行。
func (d *SQLiteDB) Exec(query string, args ...interface{}) error {
	if _, err := d.db.Exec(query, args...); err != nil {
		return fmt.Errorf("db exec 失败: %w", err)
	}
	return nil
}

// Close 关闭底层连接（引擎默认宿主为进程级内存库，一般不主动关闭）。
func (d *SQLiteDB) Close() error { return d.db.Close() }

// normalizeSQLValue 把 SQLite 驱动返回的原生类型归一成 JSON 友好的值。
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
