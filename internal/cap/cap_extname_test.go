package cap

import (
	"strings"
	"testing"
)

// 本文件覆盖「扩展原语名」注册面（cap.RegisterExtensionName / IsBuiltin / Known）。
// 红线 15「未知枚举即拒」在此保持原样：只是合法枚举从写死 6 个变为
// 「6 个内核原语 ∪ 启动期注册的 ext.* 名」。两条必须拒的情况刻意各测一次：
//  1. 注册名撞内置原语（内置集是受信核心，不得被扩展名遮蔽）；
//  2. 注册名不带 ext. 前缀（与内置名空间冲突）。
func TestRegisterExtensionNameAcceptsExtPrefix(t *testing.T) {
	const name = Primitive("ext.cap_test_probe")
	if err := RegisterExtensionName(name); err != nil {
		t.Fatalf("带 ext. 前缀的合法扩展名应注册成功，实际 %v", err)
	}
	if !name.Known() {
		t.Fatal("注册后 Known() 应为 true")
	}
	if IsBuiltin(name) {
		t.Fatal("扩展名不得被当作内置原语（IsBuiltin 应 false）")
	}
	// 注册后 GrantsFromStrings 必须放行该名（否则注册形同虚设）
	g, err := GrantsFromStrings([]string{string(name)})
	if err != nil || !g.Has(name) {
		t.Fatalf("注册后的扩展名应可被声明，实际 (%v, %v)", g, err)
	}
}

func TestRegisterExtensionNameRejectsBuiltinAndUnprefixed(t *testing.T) {
	if err := RegisterExtensionName(PrimHTTP); err == nil {
		t.Fatal("不得覆盖内置原语 http")
	}
	if err := RegisterExtensionName(PrimDB); !strings.Contains(err.Error(), "内置") {
		t.Fatalf("拒绝内置名时应说明原因，实际 %v", err)
	}
	if err := RegisterExtensionName(Primitive("nosep")); err == nil {
		t.Fatal("不带 ext. 前缀的扩展名必须被拒")
	}
	if err := RegisterExtensionName(Primitive("extless")); !strings.Contains(err.Error(), "ext.") {
		t.Fatalf("拒绝无前缀名时应点名 ext. 前缀要求，实际 %v", err)
	}
}

func TestIsBuiltinAndKnownForKernelPrimitives(t *testing.T) {
	for p := range KnownPrimitives {
		if !IsBuiltin(p) || !p.Known() {
			t.Fatalf("内核原语 %q 应同时满足 IsBuiltin 与 Known", p)
		}
	}
	// 未注册的名字：Known=false，且 GrantsFromStrings 必须拒（未知枚举即拒）
	if Primitive("ext.never_registered").Known() {
		t.Fatal("未注册的 ext. 名不应 Known")
	}
	if _, err := GrantsFromStrings([]string{"bogus"}); err == nil {
		t.Fatal("未知 capability 必须被拒")
	}
	// 纯函数原子不在门禁枚举里：写进 Capabilities 会被拒（口径见 purefunc.go）
	for _, p := range []string{"time", "template", "crypto"} {
		if _, err := GrantsFromStrings([]string{p}); err == nil {
			t.Fatalf("纯函数原子 %q 不该出现在 capability 门禁里", p)
		}
	}
}
