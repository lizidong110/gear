package cmdline

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestScanRelPathFlags：路径类 flag 取相对值是「换个 cwd 就静默指向别处」的源头
// （2026-09-22 实测：应用自带 REST 面因此读了另一个 registry，/api/flows 返回 []）。
// 这条判据必须精确——多报会变成噪声（用户直接忽略护栏），少报则等于没有。
func TestScanRelPathFlags(t *testing.T) {
	// 绝对路径必须**用当前平台的方式**构造：Windows 上 `/srv/x.db` 不是绝对路径
	// （它是「当前盘符根下的路径」，filepath.IsAbs 为 false），写死 POSIX 形态会把用例
	// 变成平台相关的假红。这本身也是 PosixPathHint 那条护栏存在的原因。
	abs := filepath.Join(t.TempDir(), "reg.db")
	cases := []struct {
		name string
		argv []string
		want []RelPathFlag
	}{
		{"相对值-空格式", []string{"serve", "--registry", "build/ui-registry.db"}, []RelPathFlag{{"registry", "build/ui-registry.db"}}},
		{"相对值-等号式", []string{"serve", "--registry=build/ui-registry.db"}, []RelPathFlag{{"registry", "build/ui-registry.db"}}},
		{"绝对值不报", []string{"serve", "--registry", abs}, nil},
		{"非路径 flag 不报", []string{"serve", "--addr", "127.0.0.1:8787"}, nil},
		{"两个都报", []string{"--registry", "a.db", "--apps-dir", "apps"}, []RelPathFlag{{"registry", "a.db"}, {"apps-dir", "apps"}}},
		{"末尾无值判不了", []string{"serve", "--registry"}, nil},
		{"空值不报", []string{"serve", "--registry", ""}, nil},
		{"-- 之后都是位置参数", []string{"serve", "--", "--registry", "a.db"}, nil},
		{"裸 token 不是 flag", []string{"serve", "registry", "a.db"}, nil},
		{"无可执行文件", []string{}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ScanRelPathFlags(c.argv, PathValueFlags)
			if len(got) != len(c.want) {
				t.Fatalf("期望 %v，实际 %v", c.want, got)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("第 %d 项：期望 %v，实际 %v", i, c.want[i], got[i])
				}
			}
		})
	}
}

// TestScanRelPathFlagsNamesAreFiltered：names 之外的路径类 flag 不报——
// 清单是「谁负责判定」的一部分，不能顺手把所有带取值的 flag 都算进来。
func TestScanRelPathFlagsNamesAreFiltered(t *testing.T) {
	if got := ScanRelPathFlags([]string{"--workdir", "w"}, PathValueFlags); len(got) != 0 {
		t.Fatalf("未列入 PathValueFlags 的 flag 不应被报，实际 %v", got)
	}
	if got := ScanRelPathFlags([]string{"--registry", "a.db"}, nil); len(got) != 0 {
		t.Fatalf("names 为空应直接返回 nil，实际 %v", got)
	}
}

// TestRelPathFlagsInLine：整行 entry 的入口。走 shell 兜底的行**不判**
// （此时 argv 由 shell 决定，静态看不见），写坏的行也不判（另有诊断）。
func TestRelPathFlagsInLine(t *testing.T) {
	got := RelPathFlagsInLine("build/gear.exe serve --registry build/ui-registry.db --addr 127.0.0.1:8787")
	if len(got) != 1 || got[0].Name != "registry" {
		t.Fatalf("应从真实 entry 里扫出 --registry，实际 %v", got)
	}
	// 绝对取值：零提示（这正是被推荐的写法）。
	if got := RelPathFlagsInLine("serve --registry " + filepath.Join(t.TempDir(), "reg.db")); len(got) != 0 {
		t.Fatalf("绝对路径不应被报，实际 %v", got)
	}
	// 含 shell 元字符 ⇒ 走 sh -c 兜底，argv 由 shell 决定，静态判定放弃
	// （若这里漏判，会把下一行那种「本来就该交给 shell」的 entry 也报一遍）。
	if got := RelPathFlagsInLine("gear serve --registry a.db > out.log"); len(got) != 0 {
		t.Fatalf("shell 兜底的行不应静态判定，实际 %v", got)
	}
}

// TestRelPathFlagHint：措辞本身是交付物（智能体靠它自愈）。
// --registry 有**正解**（省略，由起服方注入），必须写明；其它 flag 只能建议绝对路径。
func TestRelPathFlagHint(t *testing.T) {
	reg := RelPathFlagHint("tool web_ui.api", RelPathFlag{Name: "registry", Value: "build/ui-registry.db"})
	for _, want := range []string{"tool web_ui.api", "--registry", "build/ui-registry.db", "绝对路径", "省略", "GEAR_REGISTRY"} {
		if !strings.Contains(reg, want) {
			t.Errorf("--registry 的提示缺少 %q：%s", want, reg)
		}
	}
	dir := RelPathFlagHint("tool t.x", RelPathFlag{Name: "apps-dir", Value: "apps"})
	if !strings.Contains(dir, "--apps-dir") || !strings.Contains(dir, "绝对路径") {
		t.Errorf("--apps-dir 的提示不完整：%s", dir)
	}
	if strings.Contains(dir, "GEAR_REGISTRY") {
		t.Errorf("--apps-dir 没有 GEAR_REGISTRY 这条信道，不该提它：%s", dir)
	}
}
