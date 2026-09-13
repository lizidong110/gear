package model

import (
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// objectSchema 是测试用的合法 input/output schema（D5 最小结构）。
func objectSchema() map[string]interface{} {
	return map[string]interface{}{"type": "object"}
}

// T-model-01：Tool/Flow/Hook/Step 的 YAML↔struct 往返一致。
func TestYAMLRoundtrip(t *testing.T) {
	cases := []struct {
		name string
		in   interface{}
		out  interface{}
	}{
		{
			name: "Tool",
			in: Tool{
				ID: "hello", Name: "Hello", Description: "回声", Runtime: RuntimeProcess,
				Entry: "cat", InputSchema: objectSchema(), OutputSchema: objectSchema(),
				Versions: []ToolVersion{{Version: "1.0.0", State: StateStable, Note: "init"}},
			},
			out: &Tool{},
		},
		{
			name: "Flow",
			in: Flow{
				ID: "demo", Name: "Demo", Description: "两步", Vars: map[string]interface{}{"topic": "world"},
				Steps: []Step{{ID: "s1", Call: "hello", Input: map[string]interface{}{"name": "${vars.topic}"}, DependsOn: []string{"s0"}}},
			},
			out: &Flow{},
		},
		{
			name: "Hook",
			in: Hook{
				ID: "h1", Target: "*", Stage: "before_step", Action: "auth",
				Criticality: CritCritical, Order: 10,
			},
			out: &Hook{},
		},
		{
			name: "Step",
			in: Step{ID: "s1", Call: "hello", UseVersion: "1.0.0", Input: map[string]interface{}{"x": 1}, DependsOn: []string{"s0"}, When: "x>0", Parallel: true},
			out: &Step{},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b, err := yaml.Marshal(c.in)
			if err != nil {
				t.Fatalf("marshal 失败: %v", err)
			}
			if err := yaml.Unmarshal(b, c.out); err != nil {
				t.Fatalf("unmarshal 失败: %v", err)
			}
			// 再序列化比较，确保信息无损。
			b2, err := yaml.Marshal(c.out)
			if err != nil {
				t.Fatalf("二次 marshal 失败: %v", err)
			}
			if string(b) != string(b2) {
				t.Fatalf("往返不一致:\n首次=%s\n二次=%s", b, b2)
			}
		})
	}
}

// T-model-02：Runtime 枚举取值合法（合法性以 KnownRuntimes 单一来源为准，C）。
func TestRuntimeEnum(t *testing.T) {
	for r := range KnownRuntimes {
		if r == "" {
			t.Fatalf("Runtime 常量不应为空: %v", r)
		}
		if !r.Known() {
			t.Fatalf("[%s] 应在 KnownRuntimes 中", r)
		}
	}
	// 明确的关键取值。
	if RuntimeProcess != "process" || RuntimeLua != "lua" || RuntimeGoHost != "go-host" || RuntimeWASM != "wasm" {
		t.Fatalf("Runtime 常量取值与 PRD §3.3 不符")
	}
}

// T-model-02b：Known / Implemented 以单一来源表为准（C）。
func TestRuntimeKnownImplemented(t *testing.T) {
	// 四个枚举值都应 Known。
	for _, r := range []Runtime{RuntimeProcess, RuntimeLua, RuntimeGoHost, RuntimeWASM} {
		if !r.Known() {
			t.Fatalf("[%s] 应为合法枚举值", r)
		}
	}
	// 已实现：process / lua / go-host（#76）；wasm 仍未实现（V3）。
	for _, r := range []Runtime{RuntimeProcess, RuntimeLua, RuntimeGoHost} {
		if !r.Implemented() {
			t.Fatalf("[%s] 应为已实现", r)
		}
	}
	for _, r := range []Runtime{RuntimeWASM} {
		if r.Implemented() {
			t.Fatalf("[%s] 当前不应标记为已实现", r)
		}
	}
	// 未知值既非 Known 也非 Implemented。
	if Runtime("foobar").Known() || Runtime("foobar").Implemented() {
		t.Fatal("foobar 不应是合法/已实现 runtime")
	}
	// Implemented 必须是 Known 的子集（不变量）。
	for r := range ImplementedRuntimes {
		if !KnownRuntimes[r] {
			t.Fatalf("[%s] 已实现却不在 KnownRuntimes 中（列表漂移）", r)
		}
	}
}

// T-model-03：ToolVersion 字段可正确赋值与读取。
func TestToolVersionFields(t *testing.T) {
	v := ToolVersion{Version: "1.2.3", State: StateTesting, Digest: "abc", Note: "灰度", CreatedAt: time.Now()}
	if v.Version != "1.2.3" || v.State != StateTesting || v.Digest != "abc" || v.Note != "灰度" {
		t.Fatalf("ToolVersion 字段读取异常: %+v", v)
	}
}

// T-model-04：Hook 四要素（target/stage/action/criticality）+ order 字段完整。
func TestHookFields(t *testing.T) {
	h := Hook{ID: "h", Target: "tool-x", Stage: "after_step", Action: "log", Criticality: CritBestEffort, Order: 5}
	if h.Target != "tool-x" || h.Stage != "after_step" || h.Action != "log" {
		t.Fatalf("Hook 主字段异常: %+v", h)
	}
	if h.Criticality != CritBestEffort {
		t.Fatalf("Hook.Criticality 应为 best-effort")
	}
	if h.Order != 5 {
		t.Fatalf("Hook.Order 异常")
	}
	// 两个 criticality 枚举都必须存在。
	if CritCritical != "critical" || CritBestEffort != "best-effort" {
		t.Fatalf("HookCriticality 取值与 PRD §5 不符")
	}
}

// T-hook-validate：Hook.Validate 守卫（F3）。
// 关键点：criticality 留空按 critical 处理（fail-closed 默认）；before_promote 自 S5 起允许
// （由 engine.PromoteTool 在晋升前触发，见 promote.go）。
func TestHookValidate(t *testing.T) {
	valid := &Hook{ID: "h", Target: "*", Stage: StageBeforeStep, Action: "mask"}
	if err := valid.Validate(); err != nil {
		t.Fatalf("合法 Hook 应通过: %v", err)
	}
	if valid.EffectiveCriticality() != CritCritical {
		t.Fatal("留空 criticality 应按 critical 处理（fail-closed 默认）")
	}
	// best-effort 显式声明应被接受并生效。
	be := &Hook{ID: "h2", Target: "x", Stage: StageAfterStep, Action: "log", Criticality: CritBestEffort}
	if err := be.Validate(); err != nil {
		t.Fatalf("best-effort Hook 应通过: %v", err)
	}
	if be.EffectiveCriticality() != CritBestEffort {
		t.Fatal("显式 best-effort 应生效")
	}
	// #72：before_flow / on_success（Flow 级）应被接受。
	for _, st := range []string{StageBeforeFlow, StageOnSuccess} {
		bf := &Hook{ID: "h3", Target: "*", Stage: st, Action: "setup"}
		if err := bf.Validate(); err != nil {
			t.Fatalf("Flow 级 stage=%q 应通过: %v", st, err)
		}
		// Flow 级 stage 的 action 可指向 Flow id（#72），这里只校验形态，存在性运行期解析。
		bfFlow := &Hook{ID: "h4", Target: "myflow", Stage: st, Action: "subflow"}
		if err := bfFlow.Validate(); err != nil {
			t.Fatalf("Flow 级 stage=%q 的 action 指向 Flow id 应通过: %v", st, err)
		}
	}

	cases := []struct {
		name string
		h    *Hook
	}{
		{name: "空 id", h: &Hook{Target: "*", Stage: StageBeforeStep, Action: "a"}},
		{name: "空 target", h: &Hook{ID: "h", Stage: StageBeforeStep, Action: "a"}},
		{name: "空 action", h: &Hook{ID: "h", Target: "*", Stage: StageBeforeStep}},
		{name: "stage 非法", h: &Hook{ID: "h", Target: "*", Stage: "whenever", Action: "a"}},
		{name: "criticality 非法", h: &Hook{ID: "h", Target: "*", Stage: StageBeforeStep, Action: "a", Criticality: HookCriticality("maybe")}},
	}
	for _, c := range cases {
		if err := c.h.Validate(); err == nil {
			t.Errorf("%s 应被拒绝", c.name)
		}
	}
}

// T-ver-parse：合法 SemVer 通过，非法拒绝。
func TestParseSemVer(t *testing.T) {
	ok := []string{"1.0.0", "v1.0.0", "0.0.1", "1.2.3", "10.20.30", "1.0.0-alpha", "1.0.0-alpha.1", "1.0.0+build.1", "1.0.0-rc.1+build"}
	for _, s := range ok {
		if err := ParseSemVer(s); err != nil {
			t.Errorf("应合法却拒绝: %q -> %v", s, err)
		}
	}
	bad := []string{"v1", "1.0", "abc", "1.0.0.0", "x.y.z", "1.0.0-", "", "01.0.0"}
	for _, s := range bad {
		if err := ParseSemVer(s); err == nil {
			t.Errorf("应拒绝却通过: %q", s)
		}
	}
}

// T-state-valid：5 个 state 枚举合法，非法拒绝。
func TestToolVersionStateValid(t *testing.T) {
	for _, s := range []ToolVersionState{StateDraft, StateTesting, StateStable, StateDeprecated, StateRejected} {
		if !ValidStates[s] {
			t.Errorf("合法 state 未被收录: %q", s)
		}
	}
	if ValidStates[ToolVersionState("bogus")] {
		t.Errorf("非法 state 不应被收录")
	}
}

// T-model-05：Tool.Validate 拦截非法 version / state / 缺 schema。
func TestToolValidate(t *testing.T) {
	t.Run("合法", func(t *testing.T) {
		tl := &Tool{ID: "ok", Runtime: RuntimeProcess, InputSchema: objectSchema(), OutputSchema: objectSchema(),
			Versions: []ToolVersion{{Version: "1.0.0", State: StateStable}}}
		if err := tl.Validate(); err != nil {
			t.Fatalf("合法 Tool 应过: %v", err)
		}
	})
	t.Run("非法 version", func(t *testing.T) {
		tl := &Tool{ID: "bad", Runtime: RuntimeProcess, InputSchema: objectSchema(), OutputSchema: objectSchema(),
			Versions: []ToolVersion{{Version: "v1", State: StateStable}}}
		if err := tl.Validate(); err == nil {
			t.Fatal("v1 非 SemVer，应拒绝")
		}
	})
	t.Run("非法 state", func(t *testing.T) {
		tl := &Tool{ID: "bad", Runtime: RuntimeProcess, InputSchema: objectSchema(), OutputSchema: objectSchema(),
			Versions: []ToolVersion{{Version: "1.0.0", State: ToolVersionState("bogus")}}}
		if err := tl.Validate(); err == nil {
			t.Fatal("bogus state 应拒绝")
		}
	})
	t.Run("缺 input schema", func(t *testing.T) {
		tl := &Tool{ID: "bad", Runtime: RuntimeProcess, OutputSchema: objectSchema(),
			Versions: []ToolVersion{{Version: "1.0.0", State: StateStable}}}
		if err := tl.Validate(); err == nil {
			t.Fatal("缺 input_schema 应拒绝")
		}
	})
	t.Run("schema type 非 object", func(t *testing.T) {
		bad := map[string]interface{}{"type": "string"}
		tl := &Tool{ID: "bad", Runtime: RuntimeProcess, InputSchema: bad, OutputSchema: objectSchema(),
			Versions: []ToolVersion{{Version: "1.0.0", State: StateStable}}}
		if err := tl.Validate(); err == nil {
			t.Fatal("input_schema.type!=object 应拒绝")
		}
	})
	t.Run("缺 output schema", func(t *testing.T) {
		tl := &Tool{ID: "bad", Runtime: RuntimeProcess, InputSchema: objectSchema(),
			Versions: []ToolVersion{{Version: "1.0.0", State: StateStable}}}
		if err := tl.Validate(); err == nil {
			t.Fatal("缺 output_schema 应拒绝")
		}
	})
	t.Run("output schema type 非 object", func(t *testing.T) {
		bad := map[string]interface{}{"type": "string"}
		tl := &Tool{ID: "bad", Runtime: RuntimeProcess, InputSchema: objectSchema(), OutputSchema: bad,
			Versions: []ToolVersion{{Version: "1.0.0", State: StateStable}}}
		if err := tl.Validate(); err == nil {
			t.Fatal("output_schema.type!=object 应拒绝")
		}
	})
	t.Run("无 version", func(t *testing.T) {
		tl := &Tool{ID: "bad", Runtime: RuntimeProcess, InputSchema: objectSchema(), OutputSchema: objectSchema()}
		if err := tl.Validate(); err == nil {
			t.Fatal("无 version 应拒绝")
		}
	})
	t.Run("空 id", func(t *testing.T) {
		tl := &Tool{Runtime: RuntimeProcess, InputSchema: objectSchema(), OutputSchema: objectSchema(),
			Versions: []ToolVersion{{Version: "1.0.0", State: StateStable}}}
		if err := tl.Validate(); err == nil {
			t.Fatal("空 id 应拒绝")
		}
	})
	// F3（G1 修复）：runtime 在注册期校验，不再推迟到执行期。
	t.Run("未知 runtime", func(t *testing.T) {
		tl := &Tool{ID: "bad", Runtime: Runtime("foobar"), InputSchema: objectSchema(), OutputSchema: objectSchema(),
			Versions: []ToolVersion{{Version: "1.0.0", State: StateStable}}}
		if err := tl.Validate(); err == nil {
			t.Fatal("未知 runtime 应拒绝")
		}
	})
	t.Run("已知但未实现的 runtime", func(t *testing.T) {
		// #76 起 go-host 已实现；仅 wasm 仍 deferred（V3）。
		for _, rt := range []Runtime{RuntimeWASM} {
			tl := &Tool{ID: "bad", Runtime: rt, InputSchema: objectSchema(), OutputSchema: objectSchema(),
				Versions: []ToolVersion{{Version: "1.0.0", State: StateStable}}}
			if err := tl.Validate(); err == nil {
				t.Fatalf("未实现 runtime %q 应拒绝", rt)
			}
		}
	})
	t.Run("已实现 runtime 通过", func(t *testing.T) {
		for _, rt := range []Runtime{RuntimeProcess, RuntimeLua, RuntimeGoHost} {
			tl := &Tool{ID: "ok_" + string(rt), Runtime: rt, InputSchema: objectSchema(), OutputSchema: objectSchema(),
				Versions: []ToolVersion{{Version: "1.0.0", State: StateStable}}}
			if err := tl.Validate(); err != nil {
				t.Fatalf("已实现 runtime %q 应通过: %v", rt, err)
			}
		}
	})
}

// T-tool-capability：Tool.Capabilities 注册期校验（M5，最小权限，红线 15）。
// 未知原语在注册期即拒绝，而非执行期才报错。
func TestToolValidateCapability(t *testing.T) {
	base := func(caps []string) *Tool {
		return &Tool{ID: "ok", Runtime: RuntimeProcess, InputSchema: objectSchema(),
			OutputSchema: objectSchema(), Versions: []ToolVersion{{Version: "1.0.0", State: StateStable}},
			Capabilities: caps}
	}
	t.Run("合法原语", func(t *testing.T) {
		if err := base([]string{"kv", "secret", "http"}).Validate(); err != nil {
			t.Fatalf("合法 capability 应过: %v", err)
		}
	})
	t.Run("空列表允许", func(t *testing.T) {
		if err := base(nil).Validate(); err != nil {
			t.Fatalf("空 capability 应过: %v", err)
		}
	})
	t.Run("未知原语拒绝", func(t *testing.T) {
		if err := base([]string{"kv", "bogus"}).Validate(); err == nil {
			t.Fatal("未知 capability 应拒绝")
		}
	})
}

// T-flow-validate：Flow.Validate 守卫（F3 严格模型守卫）。
// 不支持的字段（when/use_version）显式拒绝，而非静默忽略（RC-D）。
// S4 起：无依赖 step 默认并行，parallel 仅为显式标注；parallel+depends_on 属误导，拒绝。
func TestFlowValidate(t *testing.T) {
	valid := &Flow{ID: "ok", Steps: []Step{{ID: "s1", Call: "hello", Input: map[string]interface{}{"x": 1}}}}
	if err := valid.Validate(); err != nil {
		t.Fatalf("合法 Flow 应过: %v", err)
	}
	// S4：parallel 作为显式标注应被接受（无依赖时）。
	marked := &Flow{ID: "okp", Steps: []Step{{ID: "s1", Call: "hello", Parallel: true}}}
	if err := marked.Validate(); err != nil {
		t.Fatalf("parallel 显式标注（无依赖）应被接受: %v", err)
	}
	// S2：once（默认）与 service 两种模式都应被接受。
	for _, m := range []string{"", "once", "service"} {
		f := &Flow{ID: "m", Steps: []Step{{ID: "s1", Call: "hello", Mode: m}}}
		if err := f.Validate(); err != nil {
			t.Fatalf("mode=%q 应被接受: %v", m, err)
		}
	}
	cases := []struct {
		name string
		f    *Flow
	}{
		{name: "空 id", f: &Flow{Steps: []Step{{ID: "s1", Call: "hello"}}}},
		{name: "无 step", f: &Flow{ID: "x"}},
		{name: "step 缺 call", f: &Flow{ID: "x", Steps: []Step{{ID: "s1"}}}},
		{name: "step 缺 id", f: &Flow{ID: "x", Steps: []Step{{Call: "hello"}}}},
		{name: "依赖不存在", f: &Flow{ID: "x", Steps: []Step{{ID: "s1", Call: "hello", DependsOn: []string{"ghost"}}}}},
		{name: "mode 非法", f: &Flow{ID: "x", Steps: []Step{{ID: "s1", Call: "hello", Mode: "daemon"}}}},
		{name: "when 误用 ${} 模板语法", f: &Flow{ID: "x", Steps: []Step{{ID: "s1", Call: "hello", When: "${steps.s1.output.n} > 1"}}}},
		{name: "parallel 与 depends_on 同存（误导标注）", f: &Flow{ID: "x", Steps: []Step{{ID: "s0", Call: "hello"}, {ID: "s1", Call: "hello", Parallel: true, DependsOn: []string{"s0"}}}}},
	}
	for _, c := range cases {
		if err := c.f.Validate(); err == nil {
			t.Errorf("%s 应被拒绝", c.name)
		}
	}
}
