package cli

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

// runConfig 执行一次 config 子命令并捕获 stdout/stderr（config 命令不依赖 registry，
// 故不带 --registry；GEAR_CONFIG_DB 由用例指到临时库以保证密闭）。
func runConfig(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := newRootCmd()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs(append([]string{"config"}, args...))
	err := root.Execute()
	return buf.String(), err
}

// TestConfigRoundTrip 覆盖 config set/get/delete/list/path 的成功与失败路径，
// 以及 list 的脱敏开关与 looksSecretKey/maskSecret 两个纯函数分支。
func TestConfigRoundTrip(t *testing.T) {
	cfgDB := filepath.Join(t.TempDir(), "config.db")
	t.Setenv("GEAR_CONFIG_DB", cfgDB)

	// set + get 往返
	if _, err := runConfig(t, "set", "ai.provider", "siliconflow"); err != nil {
		t.Fatalf("set: %v", err)
	}
	out, err := runConfig(t, "get", "ai.provider")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !strings.Contains(out, "siliconflow") {
		t.Fatalf("get 没回显值: %q", out)
	}

	// 写一把像密钥的，验证 list 默认脱敏、--show-secrets 出明文
	if _, err := runConfig(t, "set", "ai.api_key", "sk-abcdefghijklmnopqrstuvwxyz"); err != nil {
		t.Fatalf("set secret: %v", err)
	}
	listOut, err := runConfig(t, "list")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if strings.Contains(listOut, "sk-abcdefghijklmnopqrstuvwxyz") {
		t.Fatalf("list 明文泄漏了密钥: %q", listOut)
	}
	if !strings.Contains(listOut, "ai.api_key") {
		t.Fatalf("list 漏了脱敏项: %q", listOut)
	}
	plain, err := runConfig(t, "list", "--show-secrets")
	if err != nil {
		t.Fatalf("list --show-secrets: %v", err)
	}
	if !strings.Contains(plain, "sk-abcdefghijklmnopqrstuvwxyz") {
		t.Fatalf("--show-secrets 没出明文: %q", plain)
	}

	// 前缀过滤
	prefixed, err := runConfig(t, "list", "ai.")
	if err != nil {
		t.Fatalf("list ai.: %v", err)
	}
	if !strings.Contains(prefixed, "ai.provider") || !strings.Contains(prefixed, "ai.api_key") {
		t.Fatalf("list ai. 前缀过滤异常: %q", prefixed)
	}

	// delete 成功
	if _, err := runConfig(t, "delete", "ai.provider"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	out2, err := runConfig(t, "get", "ai.provider")
	if err == nil {
		t.Fatalf("删后 get 应报错，却返回: %q", out2)
	}

	// 不存在项：get / delete 都应报错（区分「没配」与「配了空串」的 ErrConfigNotFound）
	if _, err := runConfig(t, "get", "nope.missing"); err == nil {
		t.Fatal("get 不存在项应报错")
	}
	if _, err := runConfig(t, "delete", "nope.missing"); err == nil {
		t.Fatal("delete 不存在项应报错")
	}

	// path 打印落点
	pathOut, err := runConfig(t, "path")
	if err != nil {
		t.Fatalf("path: %v", err)
	}
	if !strings.Contains(pathOut, "config.db") {
		t.Fatalf("path 落点异常: %q", pathOut)
	}
}

// TestLooksSecretKey 覆盖键名判定（后缀 / 中段 . 两种匹配）与否定分支。
func TestLooksSecretKey(t *testing.T) {
	cases := map[string]bool{
		"ai.api_key":    true,
		"apikey":        true,
		"db.token":      true,
		"user_password": true, // 后缀 password
		"x.secret":      true,
		"passwd":        true,
		"ai.model":      false,
		"app.theme":     false,
		"foo":           false,
	}
	for k, want := range cases {
		if got := looksSecretKey(k); got != want {
			t.Fatalf("looksSecretKey(%q)=%v, 想要 %v", k, got, want)
		}
	}
}

// TestMaskSecret 覆盖短串全遮与长串「前4…后4」两种分支。
func TestMaskSecret(t *testing.T) {
	if got := maskSecret("short"); strings.Contains(got, "…") {
		t.Fatalf("短串不应出现省略号: %q", got)
	}
	long := maskSecret("sk-abcdefghijklmnop")
	if !strings.HasPrefix(long, "sk-a") || !strings.Contains(long, "…") {
		t.Fatalf("长串脱敏形态异常: %q", long)
	}
}
