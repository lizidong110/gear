package cli

import (
	"io"
	"os"
	"strings"
	"testing"

	"gear/internal/model"
)

// captureStderr 捕获一段代码的 stderr（告警直写 os.Stderr，没有 writer 注入点；
// 与 captureStdout 同型的管道对拷）。
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("建管道失败: %v", err)
	}
	os.Stderr = w
	done := make(chan string, 1)
	go func() {
		raw, _ := io.ReadAll(r)
		done <- string(raw)
	}()
	fn()
	_ = w.Close()
	os.Stderr = old
	out := <-done
	_ = r.Close()
	return out
}

// TestWarnScopeUnregistered —— REQ-CLI-SCOPE-WARN 的判定单测：空 scope 与已注册
// scope 必须静默（否则默认路径全是噪音），未注册才喊、且点名字（只说「有问题」
// 不说「哪个名字」的告警等于没告）。
func TestWarnScopeUnregistered(t *testing.T) {
	t.Cleanup(func() { cliScope = "" })

	cliScope = ""
	if out := captureStderr(t, func() { warnScopeUnregistered(nil) }); out != "" {
		t.Fatalf("空 scope 不应告警，实际: %q", out)
	}

	cliScope = "wms"
	apps := []*model.App{{ID: "wms"}, {ID: "other"}}
	if out := captureStderr(t, func() { warnScopeUnregistered(apps) }); out != "" {
		t.Fatalf("已注册 scope 不应告警，实际: %q", out)
	}

	cliScope = "wms2"
	out := captureStderr(t, func() { warnScopeUnregistered(apps) })
	if !strings.Contains(out, "未注册") || !strings.Contains(out, "wms2") {
		t.Fatalf("未注册 scope 应显式告警且点名字，实际: %q", out)
	}
	// nil apps（cliEngine 的 GetApp 返回 nil 路径）等价于未注册。
	if out := captureStderr(t, func() { warnScopeUnregistered(nil) }); !strings.Contains(out, "wms2") {
		t.Fatalf("nil apps 应判未注册，实际: %q", out)
	}
}

// scopeHashFlow 造一个起步 step 用纯函数原子 crypto.hash 的 Flow YAML——
// 无外部依赖，注册即可跑通，让用例聚焦「告警与守卫」而非 step 能不能跑。
func scopeHashFlow(id string) string {
	return "id: " + id + "\nname: " + id + "\ndescription: scope 告警用例\nsteps:\n" +
		"  - id: s1\n    call: crypto.hash\n    input:\n      algo: sha256\n      data: x\n"
}

// TestScopeRunWarnsWhenUnregistered —— 端到端：--scope 打错名（未注册）时，
// ① stderr 显式告警点明根因；② 命令**不**被告警阻断（只喊不拒——「先用起来、
// App 清单后补」是合法用法）。平台级原子 crypto.hash 此时仍照常放行。
func TestScopeRunWarnsWhenUnregistered(t *testing.T) {
	withTempRegistry(t)
	t.Cleanup(func() { cliScope = "" })
	if err := runCLI(t, "flow", "register", writeTempYAML(t, scopeHashFlow("mindstorm.up"))); err != nil {
		t.Fatalf("注册 Flow 失败: %v", err)
	}
	// fixture 里没有 mindstorm App（未注册）；--scope 显式传（StringVar 会写回默认值）。
	errOut := captureStderr(t, func() {
		if err := runCLI(t, "flow", "run", "--scope", "mindstorm", "mindstorm.up"); err != nil {
			t.Fatalf("scope 未注册只应告警、不应阻断命令，实际: %v", err)
		}
	})
	if !strings.Contains(errOut, "未注册") || !strings.Contains(errOut, "mindstorm") {
		t.Fatalf("stderr 应告警 --scope mindstorm 未注册，实际:\n%s", errOut)
	}
}

// TestScopeRunSilentWhenRegistered —— 反向：scope 确已注册时零告警。
// 告警的价值全在「不乱喊」——默认路径刷噪音的告警会被用户训练性无视。
func TestScopeRunSilentWhenRegistered(t *testing.T) {
	withTempRegistry(t)
	t.Cleanup(func() { cliScope = "" })
	if err := runCLI(t, "flow", "register", writeTempYAML(t, scopeHashFlow("wms.up"))); err != nil {
		t.Fatalf("注册 Flow 失败: %v", err)
	}
	seedScopeApp(t, "wms", "wms.up")
	errOut := captureStderr(t, func() {
		if err := runCLI(t, "flow", "run", "--scope", "wms", "wms.up"); err != nil {
			t.Fatalf("本应用 Flow 应放行，实际: %v", err)
		}
	})
	if strings.Contains(errOut, "未注册") {
		t.Fatalf("scope 已注册时不应告警，实际:\n%s", errOut)
	}
}

// TestScopeTypoMisBlocksOwnDefs —— 打错 scope 名的完整后果闭环：本应用定义被
// 守卫误拦（SCOPE_VIOLATION）+ 告警把根因摆在用户眼前。这是告警存在的全部
// 理由——修复前这一切静默发生，用户只看到「我的 tool 突然不能用了」。
func TestScopeTypoMisBlocksOwnDefs(t *testing.T) {
	withTempRegistry(t)
	t.Cleanup(func() { cliScope = "" })
	if err := runCLI(t, "tool", "register", writeTempYAML(t, toolYAMLForScope("wms__x"))); err != nil {
		t.Fatalf("注册 Tool 失败: %v", err)
	}
	seedScopeApp(t, "wms", "wms.up")
	if err := runCLI(t, "flow", "register", writeTempYAML(t, flowYAMLForScope("wms.up", "wms__x"))); err != nil {
		t.Fatalf("注册 Flow 失败: %v", err)
	}
	// 打错名：wms 已注册、wms2 没有。--scope 显式传（StringVar 会写回默认值）。
	errOut := captureStderr(t, func() {
		err := runCLI(t, "flow", "run", "--scope", "wms2", "wms.up")
		if err == nil {
			t.Fatal("scope 打错名时，本应用定义应被守卫误拦（这正是告警描述的后果）")
		}
		if !strings.Contains(err.Error(), "命名空间守卫") {
			t.Fatalf("报错应点明是命名空间守卫拦的，实际: %v", err)
		}
	})
	if !strings.Contains(errOut, "未注册") || !strings.Contains(errOut, "wms2") {
		t.Fatalf("stderr 应告警 scope 未注册，实际:\n%s", errOut)
	}
}

// TestScopedListWarnsWhenUnregistered —— 目录面（printScopedList）与运行期共用
// 同一份判定：tool list --scope 打错名同样要喊（列短了 + 为什么列短了一起说）。
func TestScopedListWarnsWhenUnregistered(t *testing.T) {
	db := tempDB(t)
	seedScopeRegistry(t, db)
	t.Cleanup(func() { cliScope = "" })
	// ⚠ 必须显式传 --scope：newRootCmd 每次执行都会用 StringVar 把 flag 默认值
	// 写回 cliScope（见 cli_test.go testRegistryOverride 注释），预设变量会被冲掉。
	out := captureStdout(t, func() {
		errOut := captureStderr(t, func() {
			if err := runCLI(t, "tool", "list", "--scope", "wms2", "--registry", db); err != nil {
				t.Fatalf("list 失败: %v", err)
			}
		})
		if !strings.Contains(errOut, "wms2") || !strings.Contains(errOut, "未注册") {
			t.Fatalf("list 面应同样告警，实际:\n%s", errOut)
		}
	})
	// 分组本身照常：错名名下为空，别人的仍隐藏但报数（告警不改变目录面行为）。
	if !strings.Contains(out, "已按 --scope wms2 隐藏") {
		t.Fatalf("告警不应改变分组输出，实际:\n%s", out)
	}
}
