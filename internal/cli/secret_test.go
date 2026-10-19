package cli

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

// runSecret 执行一次 secret 子命令并捕获 stdout/stderr（secret 命令不依赖 registry，
// 故不带 --registry；GEAR_SECRETS_DB 由用例指到临时库以保证密闭）。
func runSecret(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := newRootCmd()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs(append([]string{"secret"}, args...))
	err := root.Execute()
	return buf.String(), err
}

// TestSecretCLIRoundTrip 覆盖 gear secret set/get/list/delete/path 的成功与失败路径，
// 以及「set 后 get 回明文、list 不回明文」的红线与跨命令持久化。
func TestSecretCLIRoundTrip(t *testing.T) {
	secDB := filepath.Join(t.TempDir(), "secrets.db")
	t.Setenv("GEAR_SECRETS_DB", secDB)

	// path 打印落点
	if out, err := runSecret(t, "path"); err != nil {
		t.Fatalf("path: %v", err)
	} else if !strings.Contains(out, "secrets.db") {
		t.Fatalf("path 应含 secrets.db，实为 %q", out)
	}

	// set + get 往返（get 回明文）
	if _, err := runSecret(t, "set", "WMS_JWT_KEY", "lizidong110"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if out, err := runSecret(t, "get", "WMS_JWT_KEY"); err != nil || strings.TrimSpace(out) != "lizidong110" {
		t.Fatalf("get 应回明文 lizidong110，实为 %q/%v", out, err)
	}

	// list 只回名字、不回明文
	if _, err := runSecret(t, "set", "DB_PWD", "topsecret-value"); err != nil {
		t.Fatalf("set2: %v", err)
	}
	if out, err := runSecret(t, "list"); err != nil {
		t.Fatalf("list: %v", err)
	} else {
		if !strings.Contains(out, "WMS_JWT_KEY") || !strings.Contains(out, "DB_PWD") {
			t.Fatalf("list 应含两个名字，实为 %q", out)
		}
		if strings.Contains(out, "lizidong110") || strings.Contains(out, "topsecret-value") {
			t.Fatalf("list 不应回明文，实为 %q", out)
		}
	}

	// 持久化：新命令实例也能看到（模拟重启后 CLI 读库）
	if out, err := runSecret(t, "get", "WMS_JWT_KEY"); err != nil || strings.TrimSpace(out) != "lizidong110" {
		t.Fatalf("跨命令持久化失败，实为 %q/%v", out, err)
	}

	// 失败路径：不存在即报错
	if _, err := runSecret(t, "delete", "NOPE"); err == nil {
		t.Fatalf("delete 不存在的应报错")
	}
	if _, err := runSecret(t, "get", "NOPE"); err == nil {
		t.Fatalf("get 不存在的应报错")
	}

	// delete 成功 + list 不再含
	if _, err := runSecret(t, "delete", "DB_PWD"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if out, err := runSecret(t, "list"); err != nil {
		t.Fatalf("list after delete: %v", err)
	} else if strings.Contains(out, "DB_PWD") {
		t.Fatalf("delete 后 list 不应含 DB_PWD，实为 %q", out)
	}

	// 非法名 → 报错（fail-closed）
	if _, err := runSecret(t, "set", "BAD NAME", "x"); err == nil {
		t.Fatalf("非法密钥名应报错")
	}
}
