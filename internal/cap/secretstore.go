// Package cap 的密钥源实现（REQ-API-SECRETS）。
//
// 两个面，权限刻意不对称：
//   - **管理面（宿主/运维）**：Set / Delete / Names —— 经 REST /api/secrets 或 CLI `gear secret` 写入，
//     或启动期由 GEAR_SECRET_<NAME> 环境变量播种。写权限**不暴露给沙箱插件**：
//     一次覆盖即可劫持后续所有使用者的凭据，维护属宿主职责。
//   - **运行期面（Tool）**：只经 Get 读一个**已知名字**的密钥，看不到存储位置、
//     也枚举不了有哪些（cap.Secret 只有 Get，即为最小权限）。
//
// 存储形态（2026-10-02 修订）：持久化到独立 secrets.db（<GEAR_HOME>/secrets.db，可用 GEAR_SECRETS_DB 覆盖），
// 重启仍在、CLI 与 serve 共享同一份。明文落 secrets.db，但**仅管理面**（CLI/REST Set、Delete）可写，
// 运行期沙箱原语只读且不回显；GET /api/secrets 只列名、绝不回明文。
// 环境变量播种（GEAR_SECRET_*）只在内存生效、不落库——部署期权威来源，每次启动覆盖库里的同名项。
// 此前的「进程内存态、明文绝不落库、重启即失」已改为持久化（见 REQ-API-SECRETS 修订记录）。
package cap

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	_ "modernc.org/sqlite" // 纯 Go SQLite 驱动，注册名 "sqlite"，无需 cgo
)

// SecretEnvPrefix 是环境变量播种的前缀：GEAR_SECRET_<NAME> → 密钥名 <NAME>。
const SecretEnvPrefix = "GEAR_SECRET_"

// SecretStore 是密钥源：管理面可写、运行期只读（实现 cap.Secret）。
// 持持久化 backend（secrets.db）时 Set/Delete 同时落库；未 Open 时为纯内存态。
type SecretStore struct {
	mu   sync.RWMutex
	m    map[string]string
	path string
	db   *sql.DB
}

// NewSecretStore 构造空密钥源（纯内存，无持久化）。需要持久化时调用方须 Open 指定落点。
func NewSecretStore() *SecretStore {
	return &SecretStore{m: map[string]string{}}
}

// Open 挂上持久化 backend（secrets.db）：建表并把库内全部密钥载入内存。
// 必须在 SeedFromEnv 之前调用——Open 灌库、SeedFromEnv 用环境变量覆盖同名项。
// 失败即返回 error（fail-closed，绝不静默退化成内存模式：密钥静默丢失比报错难查）。
func (s *SecretStore) Open(path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db != nil {
		return nil
	}
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("密钥库未指定落点")
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("创建密钥库目录失败: %w", err)
		}
	}
	// _txlock=immediate 与 configdb.go 同一条理由：裸 BEGIN（deferred）在「读锁升写锁」时
	// 不调用 busy handler，busy_timeout 形同虚设，直接 SQLITE_BUSY。secret 库会被 CLI 写入，
	// 同时 serve 启动会 LoadFromDB，缺了它就会偶发失败。
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	c, err := sql.Open("sqlite", path+sep+"_txlock=immediate")
	if err != nil {
		return fmt.Errorf("打开密钥库失败: %w", err)
	}
	// 单连接：与 ConfigStore 的 SQLite 同口径（确定性 + 并发安全）。
	c.SetMaxOpenConns(1)
	if _, err := c.Exec("PRAGMA busy_timeout=5000"); err != nil {
		_ = c.Close()
		return fmt.Errorf("初始化密钥库失败: %w", err)
	}
	if _, err := c.Exec(`CREATE TABLE IF NOT EXISTS secrets(
		name TEXT PRIMARY KEY,
		value TEXT NOT NULL,
		updated_at TEXT DEFAULT (datetime('now')))`); err != nil {
		_ = c.Close()
		return fmt.Errorf("建密钥表失败: %w", err)
	}
	s.path = path
	s.db = c
	if err := s.loadLocked(); err != nil {
		_ = c.Close()
		s.db = nil
		return err
	}
	return nil
}

// loadLocked 把库内全部密钥载入内存（调用方须持 s.mu 写锁）。
func (s *SecretStore) loadLocked() error {
	rows, err := s.db.Query("SELECT name, value FROM secrets")
	if err != nil {
		return fmt.Errorf("读取密钥库失败: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name, value string
		if err := rows.Scan(&name, &value); err != nil {
			return fmt.Errorf("读取密钥行失败: %w", err)
		}
		s.m[name] = value
	}
	return rows.Err()
}

// Close 关闭底层连接（测试与进程退出时用）。未打开过是安全的 no-op。
func (s *SecretStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil
	}
	err := s.db.Close()
	s.db = nil
	return err
}

// Get 实现 cap.Secret：按名字读明文；不存在返回 ErrSecretNotFound。
// 注意：返回的是**明文**——调用方（引擎/原语）负责不把它写进日志。
func (s *SecretStore) Get(name string) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.m[name]
	if !ok {
		return "", ErrSecretNotFound
	}
	return v, nil
}

// Set 写入/覆盖一个密钥（管理面）。名字非法即拒（fail-closed，不写入半个脏数据）。
// 已 Open 时同时落库（持久化）。
func (s *SecretStore) Set(name, value string) error {
	if err := checkSecretName(name); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[name] = value
	if s.db != nil {
		if _, err := s.db.Exec(`INSERT INTO secrets(name, value, updated_at)
			VALUES(?, ?, datetime('now'))
			ON CONFLICT(name) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`,
			name, value); err != nil {
			return fmt.Errorf("写入密钥 %q 失败: %w", name, err)
		}
	}
	return nil
}

// Delete 删除一个密钥（管理面）。不存在返回 ErrSecretNotFound（不静默成功）。
func (s *SecretStore) Delete(name string) error {
	if err := checkSecretName(name); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.m[name]; !ok {
		return ErrSecretNotFound
	}
	delete(s.m, name)
	if s.db != nil {
		if _, err := s.db.Exec("DELETE FROM secrets WHERE name = ?", name); err != nil {
			return fmt.Errorf("删除密钥 %q 失败: %w", name, err)
		}
	}
	return nil
}

// Names 返回全部密钥名（升序）。**只回名字，绝不回明文**——管理面列表也遵守该红线。
func (s *SecretStore) Names() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.m))
	for k := range s.m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Len 返回密钥条数。
func (s *SecretStore) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.m)
}

// SeedFromEnv 从 GEAR_SECRET_<NAME> 环境变量播种（启动期调用）。
// 已有同名键会被环境变量覆盖（环境是部署期的权威来源）。**只写内存、不落库**——
// 部署期权威来源，每次启动覆盖库里同名项，但环境变量本身不持久化（避免把部署密钥固化进库）。
// 名字非法的环境变量返回 error（不静默忽略——否则「配了却读不到」极难排查）。
func (s *SecretStore) SeedFromEnv() error {
	var bad []string
	for _, kv := range os.Environ() {
		idx := strings.IndexByte(kv, '=')
		if idx < 0 {
			continue
		}
		key, val := kv[:idx], kv[idx+1:]
		if !strings.HasPrefix(key, SecretEnvPrefix) {
			continue
		}
		name := strings.TrimPrefix(key, SecretEnvPrefix)
		if err := checkSecretName(name); err != nil {
			bad = append(bad, key)
			continue
		}
		s.mu.Lock()
		s.m[name] = val
		s.mu.Unlock()
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		return fmt.Errorf("环境变量名非法（%s 之后须为非空、不含空白/花括号/美元符的名字）: %s",
			SecretEnvPrefix, strings.Join(bad, ", "))
	}
	return nil
}

// checkSecretName 校验密钥名。
//
// 名字会被写进 `${secrets.<NAME>}` 模板，故必须排除模板语法字符（`$ { }`）与空白——
// 否则「名字里有花括号」会让模板解析歧义，属注册期就该拒的输入（红线 15 同款取向）。
func checkSecretName(name string) error {
	if name == "" {
		return fmt.Errorf("密钥名不能为空")
	}
	if strings.ContainsAny(name, " \t\r\n${}") {
		return fmt.Errorf("密钥名 %q 非法（不得含空白或 ${} 等模板语法字符）", name)
	}
	return nil
}

// ValidSecretName 报告 name 是否是可用的密钥名（供管理面做 400 判定，语义与 checkSecretName 一致）。
func ValidSecretName(name string) bool { return checkSecretName(name) == nil }
