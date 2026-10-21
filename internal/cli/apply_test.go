package cli

import (
	"strings"
	"testing"
)

// TestApplyCmdRegistered 锁住「apply 已接入根命令树」——App 的落库命令若漏登记，
// scaffold app 生成的定义就无处落地（正是「导入 flow 却不是 app」困惑的根因之一）。
func TestApplyCmdRegistered(t *testing.T) {
	root := newRootCmd()
	for _, c := range root.Commands() {
		if c.Name() == "apply" {
			return
		}
	}
	t.Fatalf("根命令树缺少 apply 子命令")
}

// TestApplyCmdRegistersAppFromBundle 端到端：先注册入口 Flow，再 apply 一份只含 App 清单
// 的 YAML，App 应落进 apps 表（验证 scaffold app 的真实落库路径，而非文档里写错的 import）。
func TestApplyCmdRegistersAppFromBundle(t *testing.T) {
	db := tempDB(t)
	t.Cleanup(func() { cliScope = "" })

	// 1) 注册入口 Flow（entry_flow 必须存在，apply 才合法）。
	flowPath := writeTempYAML(t, scopeHashFlow("myapply.up"))
	if err := runCLI(t, "flow", "register", flowPath, "--registry", db); err != nil {
		t.Fatalf("注册 Flow 失败: %v", err)
	}

	// 2) 生成 App 清单（kind: App，entry_flow 指向已注册的 Flow）并 apply。
	appYAML := "kind: App\nid: myapply\nname: 我的应用\nentry_flow: myapply.up\n"
	appPath := writeTempYAML(t, appYAML)
	if err := runCLI(t, "apply", appPath, "--registry", db); err != nil {
		t.Fatalf("apply 失败: %v", err)
	}

	// 3) 验证已落库：App 出现在 apps 表。
	reg, err := openRegistry()
	if err != nil {
		t.Fatalf("打开 registry 失败: %v", err)
	}
	defer reg.Close()
	if a, err := reg.GetApp("myapply"); err != nil || a == nil {
		t.Fatalf("apply 后 apps 表应存在 myapply，实际 err=%v app=%v", err, a)
	}
}

// TestApplyCmdDryRunWritesNothing 锁住 dry-run 的原子语义：先注册 Flow，dry-run apply
// 一个 App 清单后，apps 表不应出现该 App（一个字节都没写）。
func TestApplyCmdDryRunWritesNothing(t *testing.T) {
	db := tempDB(t)
	t.Cleanup(func() { cliScope = "" })

	flowPath := writeTempYAML(t, scopeHashFlow("dryapp.up"))
	if err := runCLI(t, "flow", "register", flowPath, "--registry", db); err != nil {
		t.Fatalf("注册 Flow 失败: %v", err)
	}
	appPath := writeTempYAML(t, "kind: App\nid: dryapp\nname: 干跑\nentry_flow: dryapp.up\n")
	if err := runCLI(t, "apply", "--dry-run", appPath, "--registry", db); err != nil {
		t.Fatalf("dry-run apply 不应失败: %v", err)
	}
	reg, err := openRegistry()
	if err != nil {
		t.Fatalf("打开 registry 失败: %v", err)
	}
	defer reg.Close()
	if a, _ := reg.GetApp("dryapp"); a != nil {
		t.Fatalf("dry-run 不应落库，但 apps 表出现了 dryapp")
	}
}

// TestApplyCmdRejectsEmptyBundle 锁住空 bundle 守卫：没有任何 App/Tool/Flow/Hook 定义时
// 必须显式报错（不是 exit 0 静默 no-op）。
func TestApplyCmdRejectsEmptyBundle(t *testing.T) {
	db := tempDB(t)
	t.Cleanup(func() { cliScope = "" })
	emptyPath := writeTempYAML(t, "# 只有注释\n")
	err := runCLI(t, "apply", emptyPath, "--registry", db)
	if err == nil || !strings.Contains(err.Error(), "空") {
		t.Fatalf("空 bundle 应报错，实际 err=%v", err)
	}
}
