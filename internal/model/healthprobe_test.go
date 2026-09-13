package model

import (
	"testing"
	"time"
)

// T-probe-01：HealthProbe.Resolve 解析 + 校验各类探针配置。
func TestHealthProbeResolve(t *testing.T) {
	// nil → (nil, nil)
	if rp, err := (*HealthProbe)(nil).Resolve(); err != nil || rp != nil {
		t.Fatalf("nil 探针应解析为 (nil,nil)，实为 %v %v", rp, err)
	}

	// http 走默认值
	rp, err := (&HealthProbe{Kind: ProbeKindHTTP, Addr: "127.0.0.1:8080"}).Resolve()
	if err != nil {
		t.Fatalf("http 默认解析失败: %v", err)
	}
	if rp.Interval != defaultProbeInterval || rp.Timeout != defaultProbeTimeout ||
		rp.Threshold != defaultProbeThreshold || rp.Path != defaultProbePath {
		t.Fatalf("http 默认值不符: %+v", rp)
	}

	// tcp 显式字段
	rp, err = (&HealthProbe{Kind: ProbeKindTCP, Addr: "127.0.0.1:9090", Interval: "2s", Timeout: "500ms", Threshold: 5}).Resolve()
	if err != nil {
		t.Fatalf("tcp 解析失败: %v", err)
	}
	if rp.Interval != 2*time.Second || rp.Timeout != 500*time.Millisecond || rp.Threshold != 5 {
		t.Fatalf("tcp 字段不符: %+v", rp)
	}

	// cmd 合法
	if _, err := (&HealthProbe{Kind: ProbeKindCmd, Cmd: "true"}).Resolve(); err != nil {
		t.Fatalf("cmd 解析失败: %v", err)
	}

	// 非法 kind
	if _, err := (&HealthProbe{Kind: "ping"}).Resolve(); err == nil {
		t.Fatal("非法 kind 应报错")
	}
	// cmd 缺 cmd
	if _, err := (&HealthProbe{Kind: ProbeKindCmd}).Resolve(); err == nil {
		t.Fatal("cmd 缺 cmd 应报错")
	}
	// tcp 缺端口
	if _, err := (&HealthProbe{Kind: ProbeKindTCP, Addr: "localhost"}).Resolve(); err == nil {
		t.Fatal("tcp 缺端口应报错")
	}
	// 非法 interval
	if _, err := (&HealthProbe{Kind: ProbeKindTCP, Addr: "127.0.0.1:1", Interval: "notadur"}).Resolve(); err == nil {
		t.Fatal("非法 interval 应报错")
	}
}

// T-probe-02：Tool.Validate 在注册期校验探针；无探针正常通过，非法探针拒绝。
func TestToolValidateHealthProbe(t *testing.T) {
	base := func() *Tool {
		return &Tool{ID: "t", Runtime: RuntimeProcess, Entry: "echo",
			InputSchema: objectSchema(), OutputSchema: objectSchema(),
			Versions: []ToolVersion{{Version: "1.0.0", State: StateStable}}}
	}
	if err := base().Validate(); err != nil {
		t.Fatalf("无探针应通过: %v", err)
	}
	t1 := base()
	t1.HealthProbe = &HealthProbe{Kind: ProbeKindTCP, Addr: "127.0.0.1:1"}
	if err := t1.Validate(); err != nil {
		t.Fatalf("合法探针应通过: %v", err)
	}
	t2 := base()
	t2.HealthProbe = &HealthProbe{Kind: "ping"}
	if err := t2.Validate(); err == nil {
		t.Fatal("非法探针应在注册期拒绝")
	}
}
