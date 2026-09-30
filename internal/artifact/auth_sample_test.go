// Copyright 2026. 本文件锁的是「Hook 鉴权样板」（examples/auth-app）这条链路的工程语义：
// 登录发令牌 → withCaller(subject=令牌) → 守卫 Hook 验签 → 受保护 Tool 执行。
//
// 为什么测试**直接读 examples/auth-app/bundle.yaml** 而不是另抄一份固件：
// 样板的正确性依赖「它自己就是能跑的」。抄一份到 testdata/ 只是让测试变绿，
// 样板一旦失效（Hook 语义漂移、caller 注入失效），测试还在绿着——那比没有测试更坏。
// 代价是本测试让 examples/ 首次被测试依赖，例外已写在 examples/README.md 的「约定」里。
package artifact

import (
	"context"
	"os"
	"strings"
	"testing"

	"gear/internal/cap"
	"gear/internal/engine"
	"gear/internal/registry"
)

// authSamplePath 是样板的定义文件（相对 artifact 包）。
const authSamplePath = "../../examples/auth-app/bundle.yaml"

// loadAuthSample 把样板的 Tool + Hook 灌进临时 registry，并返回可用它跑的引擎。
//
// sqliteRoot 必须给：样板的 db dsn 写的是相对库名 "auth-app.db"（可移植写法，见
// tools/portable_audit.py），不给落点会退回引擎默认内存库，login 的建表/查用户就全落空。
func loadAuthSample(t *testing.T, keySecrets map[string]string) (*registry.Registry, *engine.Engine, *cap.MultiDB) {
	t.Helper()
	raw, err := os.ReadFile(authSamplePath)
	if err != nil {
		t.Fatalf("读样板定义失败: %v", err)
	}
	b, diags := ParseBundle(string(raw))
	if len(diags) > 0 {
		t.Fatalf("样板 bundle 解析出诊断: %+v", diags[0])
	}
	reg := mustReg(t)
	for _, tool := range b.Tools {
		if err := reg.UpsertTool(tool); err != nil {
			t.Fatalf("注册样板 Tool %s 失败: %v", tool.ID, err)
		}
	}
	for _, h := range b.Hooks {
		if err := reg.UpsertHook(h); err != nil {
			t.Fatalf("注册样板 Hook %s 失败: %v", h.ID, err)
		}
	}
	db := cap.NewMultiDB("", t.TempDir())
	// 不关连接的话 t.TempDir 收尾会撞上「文件被占用」（sqlite 连接池还开着 auth-app.db）。
	t.Cleanup(func() { _ = db.Close() })
	secrets := cap.NewSecretStore()
	for name, value := range keySecrets {
		if err := secrets.Set(name, value); err != nil {
			t.Fatalf("播种密钥 %s 失败: %v", name, err)
		}
	}
	eng := engine.New(reg,
		engine.WithHost(&cap.Providers{DB: db}),
		engine.WithSecret(secrets))
	return reg, eng, db
}

// loginSample 跑样板的登录 Tool 并返回签出的令牌。
//
// 注意样板是**两次调用**才能拿到令牌的：首次调用只建演示账号（first_run=true），
// 再次带上同一个口令才签发。这个「建号与登录分开」不是绕路，是刻意的——见 bundle.yaml 注释。
func loginSample(t *testing.T, eng *engine.Engine) string {
	t.Helper()
	for i := 0; i < 2; i++ {
		out, err := eng.RunToolExec(engine.WithCaller(context.Background(), cap.Caller{}), "auth__login", map[string]interface{}{
			"username": "demo",
			"password": "demo-pass",
		}, "")
		if err != nil {
			t.Fatalf("登录失败: %v", err)
		}
		if tok, _ := out["token"].(string); tok != "" {
			return tok
		}
		if out["first_run"] != true {
			t.Fatalf("既没建号也没发令牌，输出异常: %v", out)
		}
	}
	t.Fatal("两次调用都没拿到令牌")
	return ""
}

// withSigningKey 是「除了签名密钥什么都没配」的最小密钥集（登录必需，IdP 公钥默认没有）。
func withSigningKey() map[string]string {
	return map[string]string{"AUTH_SIGNING_KEY": "test-key"}
}

// withIdPKey 在最小集之上补上 IdP 侧的密钥材料。
//
// 这里填的是**与签名密钥同一个值**（企业 SSO 与应用共用一把 HS256 共享密钥的常见形态）；
// 换成非对称时（PEM 公钥 / JWKS），auth__guard_ext 读的 secret 名要跟着改，
// 而 jwt_verify 会按密钥类型自动把允许算法收窄到 RS*/ES*/EdDSA。
// 特意给的是同一把：若这里给了别的密钥，下面的用例就该失败——守卫认的是**密钥**，
// 不是「这个令牌是谁签的」，那一层才是真要锁的东西。
func withIdPKey() map[string]string {
	m := withSigningKey()
	m["AUTH_IDP_PUBKEY"] = "test-key"
	return m
}

// AC-A1：带 Bearer 主体（= 令牌本体）调用受保护 Tool，守卫放行。
func TestAuthSampleProtectedToolPassesWithBearer(t *testing.T) {
	_, eng, _ := loadAuthSample(t, withSigningKey())
	tok := loginSample(t, eng)

	out, err := eng.RunToolExec(engine.WithCaller(context.Background(), cap.Caller{Subject: tok}), "auth__secret", nil, "")
	if err != nil {
		t.Fatalf("带有效令牌应放行，实际失败: %v", err)
	}
	if got := out["secret"]; got == "" {
		t.Fatalf("受保护 Tool 应产出数据，实际: %v", out)
	}
}

// AC-A2：无身份 / 令牌无效 → 守卫 Hook 拒绝，**执行失败**（fail-closed，不是空结果放行）。
func TestAuthSampleGuardIsFailClosed(t *testing.T) {
	_, eng, _ := loadAuthSample(t, withSigningKey())
	tok := loginSample(t, eng)

	// (1) 空身份：CLI / MCP 这类没有 Bearer 的调用面走的就是这条。
	noID, err := eng.RunToolExec(engine.WithCaller(context.Background(), cap.Caller{}), "auth__secret", nil, "")
	if err == nil {
		t.Fatalf("无身份时守卫应拒绝执行（fail-closed 失效），却返回了: %v", noID)
	}
	// (2) 验签过的令牌但改了签名字节 —— 模拟「令牌被截获后改写」。
	bad := tok[:len(tok)-4] + "aaaa"
	if _, err := eng.RunToolExec(engine.WithCaller(context.Background(), cap.Caller{Subject: bad}), "auth__secret", nil, ""); err == nil {
		t.Fatal("签名被改动的令牌应被拒绝")
	}
	// AC-A5：拒绝信息里不得出现令牌本体 —— 否则等于把凭据写进了可被任何人读的日志。
	for _, s := range []string{tok, tok[:len(tok)-4]} {
		if strings.Contains(err.Error(), s) {
			t.Fatalf("守卫的拒绝信息里泄露了令牌: %v", err)
		}
	}
}

// 登录侧：口令错误 / 用户不存在都拒，且两种情形的错误**不可区分**（防用户枚举）。
func TestAuthSampleLoginRejectsBadPassword(t *testing.T) {
	_, eng, _ := loadAuthSample(t, withSigningKey())

	wrongPwd, err := eng.RunToolExec(engine.WithCaller(context.Background(), cap.Caller{}), "auth__login", map[string]interface{}{
		"username": "demo",
		"password": "not-the-password",
	}, "")
	if err == nil {
		t.Fatalf("口令错误应失败，却返回了: %v", wrongPwd)
	}
	noUser, err2 := eng.RunToolExec(engine.WithCaller(context.Background(), cap.Caller{}), "auth__login", map[string]interface{}{
		"username": "nobody",
		"password": "whatever",
	}, "")
	if err2 == nil {
		t.Fatalf("用户不存在应失败，却返回了: %v", noUser)
	}
	// 两条错误消息里的**业务文案**必须一致：区分它们等于给用户枚举接口。
	// （整串比对不行——Lua 堆栈带行号，29 行与 32 行的差异是无意义的噪音。）
	msg, msg2 := err.Error(), err2.Error()
	for _, s := range []string{"nobody", "不存在", "no such user", "not found"} {
		if strings.Contains(msg, s) || strings.Contains(msg2, s) {
			t.Fatalf("登录错误泄露了「用户不存在」这类区分信息（用户枚举）: %q / %q", msg, msg2)
		}
	}
	if strings.Count(msg, "用户名或口令错误") == 0 || strings.Count(msg2, "用户名或口令错误") == 0 {
		t.Fatalf("两种情形都该给出同一句统一口径的拒绝: %q / %q", msg, msg2)
	}
	// 且错误里不许出现口令本体或哈希串。
	for _, s := range []string{"not-the-password", "whatever", "$pbkdf2"} {
		if strings.Contains(msg, s) {
			t.Fatalf("登录错误里泄露了 %q: %v", s, msg)
		}
	}
}

// AC-A3：外部 IdP 路线 —— 用**另一把**密钥签出的令牌同样能作为 Bearer 主体被守卫验过。
// 与本地路线共用同一套 jwt_verify，差别只在密钥材料来源，故这里锁的是「换个签发方仍成立」。
func TestAuthSampleExternalIdPTokenIsAccepted(t *testing.T) {
	_, eng, _ := loadAuthSample(t, withIdPKey())
	tok := loginSample(t, eng)

	out, err := eng.RunToolExec(engine.WithCaller(context.Background(), cap.Caller{Subject: tok}), "auth__ext_secret", nil, "")
	if err != nil {
		t.Fatalf("外部 IdP 路线的受保护 Tool 应放行，实际失败: %v", err)
	}
	if out["secret"] == "" {
		t.Fatalf("外部路线受保护 Tool 应产出数据，实际: %v", out)
	}
}

// 缺 IdP 公钥时守卫必须拒绝，**不能退化成「没配密钥就跳过校验」**——
// 那是「配置一忘，全站敞开」的经典失效模式。
func TestAuthSampleExternalIdPGuardFailsClosedWithoutKey(t *testing.T) {
	_, eng, _ := loadAuthSample(t, withSigningKey())
	tok := loginSample(t, eng)

	out, err := eng.RunToolExec(engine.WithCaller(context.Background(), cap.Caller{Subject: tok}), "auth__ext_secret", nil, "")
	if err == nil {
		t.Fatalf("缺 IdP 公钥时守卫应拒绝，却返回了: %v", out)
	}
}
