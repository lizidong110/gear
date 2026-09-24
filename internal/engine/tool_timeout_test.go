package engine

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"gear/internal/model"
)

// luaSleepTool 造一个 lua Tool：睡 0.5 秒后正常返回 {ok=true}。
// timeout 为 Tool 声明的执行超时（空 = 不声明，应回退 defaultExecTimeout）。
//
// 用 lua 而非 process 型：process 版依赖外部 `sleep` 命令在测试机 PATH 上存在，
// 本组测试不应被外部命令可用性影响（parallel_test.go 的 sleepTool 是 process 型，
// 服务的是并行耗时对比，与这里的目标不同，故另造一个而不改它）。
//
// 之所以用「睡 0.5s 后返回」而不是死循环：本组测试要区分两种方向——
//
//	① Tool 声明**更长**超时 → 不该被更短的默认值误杀（必须成功返回）；
//	② Tool 声明**更短**超时 → 必须被自己的超时中断。
//
// 死循环只能测 ②。0.5s 的活配合不同的默认值即可覆盖两种方向。
func luaSleepTool(id, timeout string) *model.Tool {
	return &model.Tool{
		ID: id, Name: id, Runtime: model.RuntimeLua,
		Entry:       `ctx.time.sleep({seconds = 0.5}); return {ok = true}`,
		InputSchema: objSchema(), OutputSchema: objSchema(),
		Versions: []model.ToolVersion{{Version: "1.0.0", State: model.StateStable}},
		Timeout:  timeout,
	}
}

// withDefaultExecTimeout 临时改写引擎默认超时，测试结束自动还原。
// defaultExecTimeout 是包级变量（注释明确「设为变量便于测试注入更短/更长时限」），
// 本组测试未并行，故直接改写是安全的。
func withDefaultExecTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	old := defaultExecTimeout
	defaultExecTimeout = d
	t.Cleanup(func() { defaultExecTimeout = old })
}

// E-tool-to-1（正向，核心）：Tool 声明的 timeout **优先于** defaultExecTimeout。
// 默认值短到 150ms，Tool 声明 5s → 跑 0.5s 的活必须成功，证明声明真的生效。
func TestToolTimeoutExtendsBeyondDefault(t *testing.T) {
	withDefaultExecTimeout(t, 150*time.Millisecond)

	reg := newTestRegistry(t)
	defer reg.Close()
	if err := reg.UpsertTool(luaSleepTool("slow_ok", "5s")); err != nil {
		t.Fatalf("注册 Tool 失败: %v", err)
	}
	out, err := New(reg).RunTool("slow_ok", nil)
	if err != nil {
		t.Fatalf("Tool 声明 5s 应覆盖默认 150ms，实际失败: %v", err)
	}
	if out["ok"] != true {
		t.Fatalf("输出异常: %v", out)
	}
}

// E-tool-to-2：Tool 声明的 timeout 也可以**比默认更短**（双向覆盖，不是只放宽）。
// 默认值 10s，Tool 声明 200ms → 跑 0.5s 的活应被自己的超时中断，且远早于默认值。
func TestToolTimeoutShortensDefault(t *testing.T) {
	withDefaultExecTimeout(t, 10*time.Second)

	reg := newTestRegistry(t)
	defer reg.Close()
	if err := reg.UpsertTool(luaSleepTool("slow_short", "200ms")); err != nil {
		t.Fatalf("注册 Tool 失败: %v", err)
	}
	start := time.Now()
	_, err := New(reg).RunTool("slow_short", nil)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("声明 200ms 超时却跑了 0.5s 的活，应被中断")
	}
	// 注意：ctx 取消无法打断阻塞中的宿主调用（sleep 是 Go 阻塞调用），
	// 必须等它返回、回到 lua 解释循环才检查取消 ⇒ 实际中断点 ≈0.5s。
	// 故只断言「远早于默认的 10s」，不断言精确耗时。
	if elapsed > 5*time.Second {
		t.Fatalf("应在声明超时附近中断，实际耗时 %v", elapsed)
	}
}

// E-tool-to-3（负向）：未声明 timeout 的 Tool 仍受默认闸门保护——防挂死语义不被本次改动削弱。
func TestToolWithoutTimeoutStillUsesDefaultGate(t *testing.T) {
	withDefaultExecTimeout(t, 150*time.Millisecond)

	reg := newTestRegistry(t)
	defer reg.Close()
	if err := reg.UpsertTool(luaSleepTool("no_timeout", "")); err != nil {
		t.Fatalf("注册 Tool 失败: %v", err)
	}
	if _, err := New(reg).RunTool("no_timeout", nil); err == nil {
		t.Fatal("未声明 timeout 应回退 defaultExecTimeout 并被中断")
	}
}

// E-tool-to-4（fail-closed）：非法 timeout 在**注册期**就被拒绝，不留到运行期。
// 断言错误必须来自 timeout 校验本身（而非 id 等其它字段），避免「碰巧被拒」的假绿。
func TestToolTimeoutRejectedAtRegister(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	for i, bad := range []string{"abc", "5", "-1s", "0s"} {
		err := reg.UpsertTool(luaSleepTool(fmt.Sprintf("bad_to_%d", i), bad))
		if err == nil {
			t.Fatalf("非法 timeout=%q 应注册期拒绝", bad)
		}
		if !strings.Contains(err.Error(), "timeout") {
			t.Fatalf("timeout=%q 的错误应指明 timeout 字段，实际: %v", bad, err)
		}
	}
}
