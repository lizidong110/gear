// Package cap 的密钥源实现（REQ-API-SECRETS）。
//
// 两个面，权限刻意不对称：
//   - **管理面（宿主/运维）**：Set / Delete / Names —— 经 REST /api/secrets 写入，
//     或启动期由 GEAR_SECRET_<NAME> 环境变量播种。写权限**不暴露给沙箱插件**：
//     一次覆盖即可劫持后续所有使用者的凭据，维护属宿主职责。
//   - **运行期面（Tool）**：只经 Get 读一个**已知名字**的密钥，看不到存储位置、
//     也枚举不了有哪些（cap.Secret 只有 Get，即为最小权限）。
//
// 存储形态：**进程内存态**（map + 读写锁），重启即失，明文绝不落库。
// 这是显式声明而非默认假设（工程方法 §0 第14条：状态的持久性必须显式声明）。
package cap

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
)

// SecretEnvPrefix 是环境变量播种的前缀：GEAR_SECRET_<NAME> → 密钥名 <NAME>。
const SecretEnvPrefix = "GEAR_SECRET_"

// SecretStore 是进程内密钥源：管理面可写、运行期只读（实现 cap.Secret）。
type SecretStore struct {
	mu sync.RWMutex
	m  map[string]string
}

// NewSecretStore 构造空密钥源。
func NewSecretStore() *SecretStore {
	return &SecretStore{m: map[string]string{}}
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
func (s *SecretStore) Set(name, value string) error {
	if err := checkSecretName(name); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[name] = value
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
// 已有同名键会被环境变量覆盖（环境是部署期的权威来源）。
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
