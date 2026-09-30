package app

import "testing"

// 派生 argv 里少一个 flag，就是「`gear app start --scope x` 只在父进程守住了」——
// 而父进程照样返回「已启动」，不报错、不告警。这一组盯住那条约定的两个方向。

func TestDeriveRunArgsScopePropagated(t *testing.T) {
	got := deriveRunArgs("/tmp/reg.db", "mindstorm.up", "always", "mindstorm")
	want := []string{"--registry", "/tmp/reg.db", "app", "run", "mindstorm.up", "--restart", "always", "--scope", "mindstorm"}
	if len(got) != len(want) {
		t.Fatalf("argv 长度不符：\n got %v\nwant %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("argv[%d] = %q，期望 %q（完整 %v）", i, got[i], want[i], got)
		}
	}
}

// 空 scope 是默认（不启用守卫），不该在命令行上留下痕迹。
func TestDeriveRunArgsNoScope(t *testing.T) {
	got := deriveRunArgs("/tmp/reg.db", "f.up", "never", "")
	for _, a := range got {
		if a == "--scope" {
			t.Fatalf("空 scope 不该出现在 argv 里：%v", got)
		}
	}
}
