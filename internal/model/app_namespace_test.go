package model

import "testing"

// App.Owns 是**全仓唯一**的命名空间前缀规则（写入期 checkNamespace 与运行期守卫都调它）。
// 这一组盯的是规则本身，而不是某个调用方——规则漂移比「某处忘了调」更难发现，
// 因为两种漂移的表现都是「看起来对、实际错」。

func TestAppOwnsPrefixRule(t *testing.T) {
	a := &App{ID: "mindstorm"}
	cases := []struct {
		id   string
		want bool
	}{
		{"mindstorm__init", true},    // Tool / Hook 约定：双下划线
		{"mindstorm.up", true},       // Flow 约定：点
		{"mindstorm.", true},         // 恰好等于前缀 + 分隔符（边界：算自己的）
		{"mindstorm", false},         // 恰好等于 app id（不是它名下的对象）
		{"other__x", false},          // 别的应用
		{"mindstormextra__x", false}, // 前缀相同但分隔符不同（不能靠 HasPrefix 猜）
		{"", false},
	}
	for _, c := range cases {
		if got := a.Owns(c.id); got != c.want {
			t.Errorf("App{ID:mindstorm}.Owns(%q) = %v，期望 %v", c.id, got, c.want)
		}
	}
}

// 空 App（nil 接收者或空 ID）一律不拥有任何东西：守卫据此 fail-closed，
// 而不是「没配就放行」——nil 判空若漏了，前缀规则就会拿空串去匹配一切。
func TestAppOwnsEmptyOwner(t *testing.T) {
	var nilApp *App
	if nilApp.Owns("mindstorm__x") {
		t.Error("nil App 不应拥有任何 id")
	}
	empty := &App{}
	if empty.Owns("mindstorm__x") {
		t.Error("空 ID 的 App 不应拥有任何 id")
	}
}
