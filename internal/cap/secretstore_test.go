package cap

import (
	"strings"
	"testing"
)

// 本文件覆盖 cap.SecretStore（REQ-API-SECRETS 的载体）。三个红点是刻意断言的：
//   1. 名字非法即拒（fail-closed），不写入半个脏数据；
//   2. SeedFromEnv 遇到非法环境变量名必须**返回 error**，绝不静默忽略
//      （否则「配了却读不到」在运维侧极难排查）；
//   3. Names 只回名字、绝不回明文。

func TestSecretStoreGetSetDelete(t *testing.T) {
	s := NewSecretStore()
	if _, err := s.Get("nope"); err != ErrSecretNotFound {
		t.Fatalf("读不存在的密钥应返回 ErrSecretNotFound，实际 %v", err)
	}
	if err := s.Set("db_password", "s3cr3t"); err != nil {
		t.Fatalf("Set 合法名不该报错：%v", err)
	}
	got, err := s.Get("db_password")
	if err != nil || got != "s3cr3t" {
		t.Fatalf("Get 应回读明文，实际 (%q, %v)", got, err)
	}
	// 覆盖写
	if err := s.Set("db_password", "rotated"); err != nil {
		t.Fatalf("覆盖写不该报错：%v", err)
	}
	if got, _ := s.Get("db_password"); got != "rotated" {
		t.Fatalf("Set 应可覆盖，实际 %q", got)
	}
	if err := s.Delete("db_password"); err != nil {
		t.Fatalf("Delete 已存在的键不该报错：%v", err)
	}
	if err := s.Delete("db_password"); err != ErrSecretNotFound {
		t.Fatalf("Delete 不存在的键应返回 ErrSecretNotFound（不静默成功），实际 %v", err)
	}
	if s.Len() != 0 {
		t.Fatalf("删完应为空，实际 Len=%d", s.Len())
	}
}

func TestSecretStoreRejectsInvalidName(t *testing.T) {
	bad := []string{"", "has space", "tab\there", "dollar$x", "brace{y", "brace}z"}
	s := NewSecretStore()
	for _, name := range bad {
		if err := s.Set(name, "v"); err == nil {
			t.Fatalf("Set(%q) 应被拒（名字含空白或 ${} 会让模板解析歧义），实际 nil", name)
		}
		if err := s.Delete(name); err == nil {
			t.Fatalf("Delete(%q) 应被拒，实际 nil", name)
		}
	}
	if s.Len() != 0 {
		t.Fatalf("非法名一律不写入，实际 Len=%d", s.Len())
	}
}

func TestSecretStoreNamesSortedNamesOnly(t *testing.T) {
	s := NewSecretStore()
	for _, n := range []string{"zeta", "alpha", "mid"} {
		if err := s.Set(n, "secret-"+n); err != nil {
			t.Fatalf("Set(%q) 失败：%v", n, err)
		}
	}
	names := s.Names()
	want := []string{"alpha", "mid", "zeta"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("Names 应升序，实际 %v", names)
	}
	for _, n := range names {
		if strings.Contains(n, "secret-") {
			t.Fatalf("Names 只回名字、绝不回明文，实际泄漏：%q", n)
		}
	}
	if s.Len() != 3 {
		t.Fatalf("Len 应为 3，实际 %d", s.Len())
	}
}

func TestValidSecretNameMirrorsCheck(t *testing.T) {
	ok := []string{"a", "DB_PASSWORD", "api.key-1", "名"}
	for _, n := range ok {
		if !ValidSecretName(n) {
			t.Fatalf("ValidSecretName(%q) 应为 true", n)
		}
	}
	for _, n := range []string{"", "a b", "a$b", "a{b", "a}b", "a\nb"} {
		if ValidSecretName(n) {
			t.Fatalf("ValidSecretName(%q) 应为 false", n)
		}
	}
}

func TestSecretStoreSeedFromEnv(t *testing.T) {
	t.Setenv("GEAR_SECRET_ALPHA", "a-value")
	t.Setenv("GEAR_SECRET_BETA", "b-value")
	s := NewSecretStore()
	// 预置同名键：环境变量是部署期权威来源，应覆盖已有值
	if err := s.Set("ALPHA", "old"); err != nil {
		t.Fatalf("Set 失败：%v", err)
	}
	if err := s.SeedFromEnv(); err != nil {
		t.Fatalf("SeedFromEnv 不该报错：%v", err)
	}
	if got, _ := s.Get("ALPHA"); got != "a-value" {
		t.Fatalf("同名键应被环境变量覆盖，实际 %q", got)
	}
	if got, _ := s.Get("BETA"); got != "b-value" {
		t.Fatalf("应播种 BETA，实际 (%q)", got)
	}
}

func TestSecretStoreSeedFromEnvFailsClosedOnBadName(t *testing.T) {
	// 名字非法 → 返回 error，绝不静默忽略；且该键不得入库。
	t.Setenv("GEAR_SECRET_BAD NAME", "v")
	s := NewSecretStore()
	err := s.SeedFromEnv()
	if err == nil {
		t.Fatal("SeedFromEnv 遇到非法环境变量名必须返回 error（fail-closed）")
	}
	if !strings.Contains(err.Error(), "GEAR_SECRET_BAD NAME") {
		t.Fatalf("error 应点名是哪个环境变量非法，实际 %q", err.Error())
	}
	if _, getErr := s.Get("BAD NAME"); getErr != ErrSecretNotFound {
		t.Fatalf("非法的键不得入库，实际 Get 错误 %v", getErr)
	}
}

func TestSecretStoreConcurrentSetGetIsSafe(t *testing.T) {
	// 读写锁是防御性的；-race 下本用例同时验证无数据竞争。
	s := NewSecretStore()
	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func(i int) {
			defer func() { done <- struct{}{} }()
			name := "k" + string(rune('a'+i))
			_ = s.Set(name, "v")
			_, _ = s.Get(name)
			_ = s.Names()
			_ = s.Len()
		}(i)
	}
	for i := 0; i < 8; i++ {
		<-done
	}
	if s.Len() != 8 {
		t.Fatalf("8 个并发写应全部生效，实际 Len=%d", s.Len())
	}
}
