package cap

import (
	"errors"
	"path/filepath"
	"testing"
)

// newTestConfig 造一个落在临时目录的配置库，并保证测试结束关闭（与 MultiDB 测试同口径：
// 不 Close 会让 sqlite 句柄泄漏，在 -race 下表现为偶发的「文件被占用」）。
func newTestConfig(t *testing.T) *ConfigStore {
	t.Helper()
	s := NewConfigStore(filepath.Join(t.TempDir(), "config.db"))
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestConfigStoreRoundTrip(t *testing.T) {
	s := newTestConfig(t)
	if err := s.Set("ai.model", "deepseek-ai/DeepSeek-V4-Flash"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := s.Get("ai.model")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "deepseek-ai/DeepSeek-V4-Flash" {
		t.Fatalf("Get = %q, 想要写入的那个值", got)
	}
	// 覆盖：同一个 key 再写一次应替换而非新增一行。
	if err := s.Set("ai.model", "other"); err != nil {
		t.Fatalf("Set(覆盖): %v", err)
	}
	if got, _ = s.Get("ai.model"); got != "other" {
		t.Fatalf("覆盖后 Get = %q, 想要 other", got)
	}
	m, err := s.List("")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(m) != 1 {
		t.Fatalf("List 长度 = %d, 想要 1（覆盖不应产生第二行）：%v", len(m), m)
	}
	if err := s.Delete("ai.model"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get("ai.model"); !errors.Is(err, ErrConfigNotFound) {
		t.Fatalf("删除后 Get err = %v, 想要 ErrConfigNotFound（「没配」与「配了空值」必须可分）", err)
	}
}

// TestConfigStoreEmptyValueIsNotMissing 守住「空值也是值」：set 成空串后 get 必须返回空串 + nil，
// 不能返回 ErrConfigNotFound——回退链靠这个区分「用户故意留空」与「没配过」。
func TestConfigStoreEmptyValueIsNotMissing(t *testing.T) {
	s := newTestConfig(t)
	if err := s.Set("ai.base_url", ""); err != nil {
		t.Fatalf("Set(空值): %v", err)
	}
	v, err := s.Get("ai.base_url")
	if err != nil {
		t.Fatalf("Get(空值) err = %v, 想要 nil", err)
	}
	if v != "" {
		t.Fatalf("Get(空值) = %q, 想要空串", v)
	}
}

func TestConfigStoreListPrefix(t *testing.T) {
	s := newTestConfig(t)
	for k, v := range map[string]string{
		"ai.provider":      "siliconflow",
		"ai.model":         "m",
		"app.mindstorm.x":  "1",
		"app.brainstorm.y": "2",
	} {
		if err := s.Set(k, v); err != nil {
			t.Fatalf("Set(%s): %v", k, err)
		}
	}
	m, err := s.List("ai.")
	if err != nil {
		t.Fatalf("List(ai.): %v", err)
	}
	if len(m) != 2 || m["ai.provider"] != "siliconflow" || m["ai.model"] != "m" {
		t.Fatalf("List(ai.) = %v, 想要恰好两项 ai.*", m)
	}
	m, err = s.List("app.")
	if err != nil {
		t.Fatalf("List(app.): %v", err)
	}
	if len(m) != 2 {
		t.Fatalf("List(app.) = %v, 想要恰好两项 app.*", m)
	}
}

// TestConfigStoreListPrefixEscapesWildcards 是负向验证：key 里的下划线是 LIKE 的通配符，
// 不转义的话 `List("a_b.")` 会连 `acb.` 一起命中——「多出来的配置项」比报错更难查。
func TestConfigStoreListPrefixEscapesWildcards(t *testing.T) {
	s := newTestConfig(t)
	if err := s.Set("a_b.k", "1"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := s.Set("acb.k", "2"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	m, err := s.List("a_b.")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(m) != 1 || m["a_b.k"] != "1" {
		t.Fatalf("List(\"a_b.\") = %v, 想要只命中字面前缀 a_b.（下划线不得当通配符）", m)
	}
}

func TestConfigStoreRejectsBadKey(t *testing.T) {
	s := newTestConfig(t)
	for _, bad := range []string{"", "   ", " ai.model", "ai.model "} {
		if err := s.Set(bad, "v"); err == nil {
			t.Fatalf("Set(%q) 应报错（空 key / 首尾空白会写出永远取不回来的条目）", bad)
		}
		if _, err := s.Get(bad); err == nil {
			t.Fatalf("Get(%q) 应报错", bad)
		}
		if err := s.Delete(bad); err == nil {
			t.Fatalf("Delete(%q) 应报错", bad)
		}
	}
}

func TestConfigStoreDeleteMissing(t *testing.T) {
	s := newTestConfig(t)
	if err := s.Delete("nope"); !errors.Is(err, ErrConfigNotFound) {
		t.Fatalf("Delete(不存在) err = %v, 想要 ErrConfigNotFound", err)
	}
}

// TestConfigStorePersistsAcrossReopen 守住「配置真的落盘」——这一整件事的意义就是重启后还在
// （db 原语的默认库是内存，重启即丢，正是 config 要解决的那个问题）。
func TestConfigStorePersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.db")
	s1 := NewConfigStore(path)
	if err := s1.Set("ai.api_key", "sk-test"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	s2 := NewConfigStore(path)
	t.Cleanup(func() { _ = s2.Close() })
	v, err := s2.Get("ai.api_key")
	if err != nil {
		t.Fatalf("重开后 Get: %v", err)
	}
	if v != "sk-test" {
		t.Fatalf("重开后 Get = %q, 想要 sk-test（配置必须跨进程保留）", v)
	}
}

func TestConfigStoreEmptyPathFailsClosed(t *testing.T) {
	s := NewConfigStore("")
	if err := s.Set("k", "v"); err == nil {
		t.Fatal("落点为空时应报错，不能静默退化成内存模式（配置静默丢失比报错难查得多）")
	}
}
