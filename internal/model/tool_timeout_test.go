package model

import (
	"strings"
	"testing"
	"time"
)

// T-model-to-1：Tool.ExecTimeout 的解析语义——空表示「未声明」，
// 合法 duration 原样返回，非法/非正一律报错（注册期即拦，fail-closed）。
func TestToolExecTimeout(t *testing.T) {
	// 未声明 → (0, nil)：0 是「用引擎默认」的信号，不是「零超时」。
	if d, err := (Tool{ID: "t"}).ExecTimeout(); err != nil || d != 0 {
		t.Fatalf("未声明应返回 (0, nil)，实际 (%v, %v)", d, err)
	}
	// 空白字符串同样视为未声明（YAML 里 `timeout: ""` 不该被当成非法配置）。
	if d, err := (Tool{ID: "t", Timeout: "   "}).ExecTimeout(); err != nil || d != 0 {
		t.Fatalf("纯空白应视为未声明，实际 (%v, %v)", d, err)
	}
	// 合法声明原样解析。
	if d, err := (Tool{ID: "t", Timeout: "5m"}).ExecTimeout(); err != nil || d != 5*time.Minute {
		t.Fatalf("5m 应解析为 5 分钟，实际 (%v, %v)", d, err)
	}
	// 非法 / 非正一律拒绝，且错误必须指明 timeout 字段（便于用户定位）。
	for _, bad := range []string{"abc", "5", "-1s", "0s", "0"} {
		_, err := (Tool{ID: "t", Timeout: bad}).ExecTimeout()
		if err == nil {
			t.Fatalf("timeout=%q 应报错", bad)
		}
		if !strings.Contains(err.Error(), "timeout") {
			t.Fatalf("timeout=%q 的错误应指明字段，实际: %v", bad, err)
		}
	}
}

// T-model-to-2：Tool.Validate 把非法 timeout 变成**注册期**错误——
// 工具链不会等到真跑起来才发现配置写错（与 health_probe 同口径）。
func TestToolValidateRejectsBadTimeout(t *testing.T) {
	base := func() Tool {
		return Tool{
			ID: "t", Name: "t", Runtime: RuntimeProcess, Entry: "cat",
			InputSchema: objectSchema(), OutputSchema: objectSchema(),
			Versions: []ToolVersion{{Version: "1.0.0", State: StateStable}},
		}
	}
	// 先确认基线本身合法（否则后面的「非法」断言毫无意义）。
	if err := base().Validate(); err != nil {
		t.Fatalf("基线 Tool 应合法: %v", err)
	}
	tool := base()
	tool.Timeout = "nope"
	err := tool.Validate()
	if err == nil {
		t.Fatal("非法 timeout 应被 Validate 拒绝")
	}
	if !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("错误应指明 timeout 字段，实际: %v", err)
	}
	// 合法声明必须放行（否则本字段形同虚设）。
	ok := base()
	ok.Timeout = "300s"
	if err := ok.Validate(); err != nil {
		t.Fatalf("合法 timeout 不应被拒: %v", err)
	}
}
