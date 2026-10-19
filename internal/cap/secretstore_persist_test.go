package cap

import (
	"path/filepath"
	"testing"
)

// TestSecretStorePersist 覆盖 secret 持久化到 secrets.db 的往返与跨 Open 可见性：
// 写 → 关闭 → 重新 Open 应看到；Delete 也持久化。
func TestSecretStorePersist(t *testing.T) {
	db := filepath.Join(t.TempDir(), "secrets.db")

	s1 := NewSecretStore()
	if err := s1.Open(db); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s1.Set("WMS_JWT_KEY", "lizidong110"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := s1.Set("DB_PWD", "s3cret"); err != nil {
		t.Fatalf("Set2: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// 重新 Open：应看到持久化的两把
	s2 := NewSecretStore()
	if err := s2.Open(db); err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	defer s2.Close()
	if v, err := s2.Get("WMS_JWT_KEY"); err != nil || v != "lizidong110" {
		t.Fatalf("持久化后 Get 应为 lizidong110，实为 %q/%v", v, err)
	}
	if names := s2.Names(); len(names) != 2 {
		t.Fatalf("持久化后应有 2 个名字，实为 %v", names)
	}

	// Delete 也持久化
	if err := s2.Delete("DB_PWD"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := s2.Close(); err != nil {
		t.Fatalf("Close2: %v", err)
	}
	s3 := NewSecretStore()
	if err := s3.Open(db); err != nil {
		t.Fatalf("re-Open2: %v", err)
	}
	defer s3.Close()
	if _, err := s3.Get("DB_PWD"); err != ErrSecretNotFound {
		t.Fatalf("Delete 应持久化，DB_PWD 应不存在，实为 %v", err)
	}
}

// TestSecretStoreOpenBadPath 覆盖非法落点 fail-closed（绝不静默退化成内存模式）。
func TestSecretStoreOpenBadPath(t *testing.T) {
	s := NewSecretStore()
	if err := s.Open(""); err == nil {
		t.Fatalf("空落点应报错，不应静默内存模式")
	}
}
