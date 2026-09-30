package cap

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	_ "modernc.org/sqlite" // 纯 Go SQLite 驱动，注册名 "sqlite"，无需 cgo
)

// Config 是「键值配置」原语（PRD §6 宿主能力原语，与 db 平级的第 7 个受门禁原语）。
//
// 为什么要有它，而不是让应用自己用 db 建一张 settings 表：
//
//   - db 原语的 dsn 缺省回退「引擎内存 scratch 库，重启即丢且无报错」，应用想持久化就必须显式
//     传 dsn；而 Lua 沙箱禁了 os 库、模板也没有 ${env} 前缀（见 internal/engine/template.go），
//     脚本根本读不到 GEAR_HOME —— 于是应用只能把绝对路径硬编码进 YAML（mindstorm 的
//     mindstorm_data.yaml 里 18 处 `local DSN = "D:/gadern/gear/build/mindstorm.db"`）。
//   - config 由**宿主**注入落点（gearhome.ConfigDBPath），应用声明 `capabilities: [config]`
//     即可读写持久化配置，不必知道库在哪，也不必靠环境变量把配置灌进进程。
//
// 与 secret 的分工（刻意不合并）：secret 是**只读**密钥源、看不到存储位置、且不落本库；
// config 是**可读写**的普通配置。API key 这类东西放 config 是为了「界面可改、重启仍在」，
// 它的安全边界是「本机用户数据目录」，不是「进程不可见」——两者不是一回事，不要互相顶替。
type Config interface {
	// Get 读取一项配置；不存在返回 ErrConfigNotFound（不返回空串 + nil，
	// 否则「没配」与「配了空值」无法区分，回退链会静默放过）。
	Get(key string) (string, error)
	// Set 写入一项配置（已存在则覆盖）。空 key 直接报错（fail-closed）。
	Set(key, value string) error
	// Delete 删除一项配置；不存在时返回 ErrConfigNotFound。
	Delete(key string) error
	// List 列出 prefix 前缀下的全部配置（prefix 为空串表示全部）。
	List(prefix string) (map[string]string, error)
}

// ErrConfigNotFound 表示请求的配置项不存在。
var ErrConfigNotFound = errors.New("配置项不存在")

// ConfigStore 是 Config 的 SQLite 实现（进程内单例即可，连接按需懒开）。
//
// 落点由宿主决定（默认 <gearhome>/config.db），同一台机器上「这一套 gear」共用一份——
// 与 registry.db / exec.db 同源同目录，避免「换一个 cwd 就换一份配置」这类静默故障。
type ConfigStore struct {
	path string
	mu   sync.Mutex
	db   *sql.DB
}

// NewConfigStore 构造落点 path 的配置库。函数永不返回 nil（与 NewMultiDB 同口径），
// 故 Providers.Config 用接口承载时不会出现 typed-nil 触发 Context.Config() 误判。
// 真正的打开在建表/首次读写时懒执行——构造一个不存在的路径不应 panic。
func NewConfigStore(path string) *ConfigStore {
	return &ConfigStore{path: path}
}

// Path 返回落点（供 CLI 的 doctor / 错误提示展示真实位置）。
func (s *ConfigStore) Path() string { return s.path }

// open 懒开连接并确保表存在。并发安全；失败即返回错误，绝不静默退化成「内存模式」
// （配置静默丢失比报错难查得多）。
func (s *ConfigStore) open() (*sql.DB, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db != nil {
		return s.db, nil
	}
	if strings.TrimSpace(s.path) == "" {
		return nil, fmt.Errorf("config 配置库未指定落点")
	}
	if dir := filepath.Dir(s.path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("创建配置库目录失败: %w", err)
		}
	}
	// _txlock=immediate 与 registry.go 同一条理由：裸 BEGIN（deferred）在「读锁升写锁」时
	// 不调用 busy handler，busy_timeout 形同虚设，直接 SQLITE_BUSY。config 原语会在
	// 应用运行时被 Lua 侧写入，同时可能有 `gear config set ai.*` 在跑，缺了它就会偶发失败。
	// sep 与 registry.go 同一讲究：s.path 若本身已带 query（如内存库），不能无脑拼 "?"。
	sep := "?"
	if strings.Contains(s.path, "?") {
		sep = "&"
	}
	c, err := sql.Open("sqlite", s.path+sep+"_txlock=immediate")
	if err != nil {
		return nil, fmt.Errorf("打开配置库失败: %w", err)
	}
	// 单连接：与 MultiDB 的 sqlite 同口径（确定性 + 并发安全）。
	c.SetMaxOpenConns(1)
	if _, err := c.Exec("PRAGMA busy_timeout=5000"); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("初始化配置库失败: %w", err)
	}
	if _, err := c.Exec(`CREATE TABLE IF NOT EXISTS config(
		key TEXT PRIMARY KEY,
		value TEXT NOT NULL,
		updated_at TEXT DEFAULT (datetime('now')))`); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("建配置表失败: %w", err)
	}
	s.db = c
	return s.db, nil
}

// Close 关闭底层连接（测试与进程退出时用）。未打开过是安全的 no-op。
func (s *ConfigStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil
	}
	err := s.db.Close()
	s.db = nil
	return err
}

// Get 读取一项配置。
func (s *ConfigStore) Get(key string) (string, error) {
	if err := checkConfigKey(key); err != nil {
		return "", err
	}
	db, err := s.open()
	if err != nil {
		return "", err
	}
	var v string
	err = db.QueryRow("SELECT value FROM config WHERE key = ?", key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrConfigNotFound
	}
	if err != nil {
		return "", fmt.Errorf("读取配置 %q 失败: %w", key, err)
	}
	return v, nil
}

// Set 写入一项配置（UPSERT）。
func (s *ConfigStore) Set(key, value string) error {
	if err := checkConfigKey(key); err != nil {
		return err
	}
	db, err := s.open()
	if err != nil {
		return err
	}
	if _, err := db.Exec(`INSERT INTO config(key, value, updated_at)
		VALUES(?, ?, datetime('now'))
		ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`,
		key, value); err != nil {
		return fmt.Errorf("写入配置 %q 失败: %w", key, err)
	}
	return nil
}

// Delete 删除一项配置；不存在时返回 ErrConfigNotFound（让「删了个不存在的」可见）。
func (s *ConfigStore) Delete(key string) error {
	if err := checkConfigKey(key); err != nil {
		return err
	}
	db, err := s.open()
	if err != nil {
		return err
	}
	res, err := db.Exec("DELETE FROM config WHERE key = ?", key)
	if err != nil {
		return fmt.Errorf("删除配置 %q 失败: %w", key, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil
	}
	if n == 0 {
		return ErrConfigNotFound
	}
	return nil
}

// List 列出 prefix 前缀下的配置（空前缀 = 全部）。结果按 key 升序由 SQL 保证。
func (s *ConfigStore) List(prefix string) (map[string]string, error) {
	db, err := s.open()
	if err != nil {
		return nil, err
	}
	rows, err := db.Query("SELECT key, value FROM config WHERE key LIKE ? ESCAPE '\\' ORDER BY key",
		likePrefix(prefix))
	if err != nil {
		return nil, fmt.Errorf("列出配置失败: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, fmt.Errorf("读取配置行失败: %w", err)
		}
		out[k] = v
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历配置失败: %w", err)
	}
	return out, nil
}

// checkConfigKey 校验 key（fail-closed：空 key 与含空白的 key 一律拒绝，
// 否则 `config.set({key=" "})` 会写出一个永远取不回来的条目）。
func checkConfigKey(key string) error {
	if strings.TrimSpace(key) == "" {
		return fmt.Errorf("config 的 key 不能为空")
	}
	if key != strings.TrimSpace(key) {
		return fmt.Errorf("config 的 key %q 首尾不能有空白", key)
	}
	return nil
}

// likePrefix 把字面前缀转成 LIKE 模式（转义 % _ \，避免用户 key 里的下划线被当成通配符）。
func likePrefix(prefix string) string {
	var b strings.Builder
	for _, r := range prefix {
		switch r {
		case '%', '_', '\\':
			b.WriteRune('\\')
		}
		b.WriteRune(r)
	}
	b.WriteRune('%')
	return b.String()
}
