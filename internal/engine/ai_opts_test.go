package engine

import (
	"context"
	"testing"
)

// stubRecipes 是最小的配方加载器：不依赖内嵌 YAML / 外部目录，只为跑通 generate 链路。
type stubRecipes struct{}

func (stubRecipes) Load(kind string) (*Recipe, error) {
	return &Recipe{
		Kind:            kind,
		MaxRetries:      1,
		SystemDecompose: "decompose",
		SystemAssemble:  "assemble",
		OutputHints:     "hints",
	}, nil
}

// recordingLLM 记录每一次收到的 LLMRequest，然后返回一段固定文本。
// 注意：返回 "{}" 会让 generate 在校验阶段失败——这不影响本用例，我们只断言
// 「LLM 收到了什么」，即 opts 的模型覆盖是否被真正应用到调用链上。
type recordingLLM struct{ got []LLMRequest }

func (l *recordingLLM) Complete(_ context.Context, req LLMRequest) (string, error) {
	l.got = append(l.got, req)
	return "{}", nil
}

func (l *recordingLLM) CompleteStream(_ context.Context, req LLMRequest, onDelta func(content, reasoning string) error) error {
	l.got = append(l.got, req)
	if onDelta != nil {
		return onDelta("{}", "")
	}
	return nil
}

// TestGenerateAppliesLLMOverridesFromOpts —— GenerateOpts 的模型覆盖必须真的生效。
//
// 曾踩的坑：CLI `gear ai gen` 把 GEAR_AI_MODEL / GEAR_AI_PROVIDER 填进 opts，
// 但 Generate **从不消费**它们（withLLMOverrides 写好了却没接线），
// 结果是「配了环境变量却静默失效」——既不报错也不生效，属红线禁止的静默降级。
//
// 另一个隐含契约：必须用 `a = a.withLLMOverrides(opts)` 的形式在**组合 Eino 链之前**
// 替换 receiver——`compose.InvokableLambda(a.decompose)` 这类方法值在取值那一刻就绑定了
// receiver，晚一步替换 llm 对 decompose/catalog/assemble 无效。本用例同时锁住这一点。
func TestGenerateAppliesLLMOverridesFromOpts(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()

	llm := &recordingLLM{}
	ag := newAIAgent(reg, llm, stubRecipes{}, nil)

	// Generate 大概率会在后续校验/解析阶段失败；我们只关心 LLM 是否被调用、带了什么参数。
	_, _ = ag.Generate(context.Background(), "flow", "做个 demo", GenerateOpts{
		Provider: "ollama",
		Model:    "cli-model",
		BaseURL:  "http://127.0.0.1:11434",
		APIKey:   "k-from-cli",
	})

	if len(llm.got) == 0 {
		t.Fatal("LLM 一次都没被调用——无法验证模型覆盖是否生效（测试夹具失效）")
	}
	for i, r := range llm.got {
		if r.Provider != "ollama" {
			t.Fatalf("第 %d 次 LLM 调用 provider=%q，期望 %q（opts 未生效）", i+1, r.Provider, "ollama")
		}
		if r.Model != "cli-model" {
			t.Fatalf("第 %d 次 LLM 调用 model=%q，期望 %q（opts 未生效）", i+1, r.Model, "cli-model")
		}
		if r.BaseURL != "http://127.0.0.1:11434" {
			t.Fatalf("第 %d 次 LLM 调用 base_url=%q，期望 opts 注入的值", i+1, r.BaseURL)
		}
		if r.APIKey != "k-from-cli" {
			t.Fatalf("第 %d 次 LLM 调用 api_key=%q，期望 opts 注入的值", i+1, r.APIKey)
		}
	}
}

// TestGenerateWithoutOverridesStillCallsLLMSameWay：opts 没给模型参数时行为不变
// （仍然调用 LLM，只是不注入任何覆盖字段），锁住「接线不改默认值」。
func TestGenerateWithoutOverridesLeavesRequestUntouched(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()

	llm := &recordingLLM{}
	ag := newAIAgent(reg, llm, stubRecipes{}, nil)
	_, _ = ag.Generate(context.Background(), "flow", "做个 demo", GenerateOpts{})

	if len(llm.got) == 0 {
		t.Fatal("LLM 未被调用")
	}
	if got := llm.got[0].Model; got != "" {
		t.Fatalf("未给 opts 时不应注入 model，实际 %q", got)
	}
}

// TestWithLLMOverridesReturnsSameAgentWhenEmpty：opts 全空时返回**同一个** agent，
// 避免每次 generate 都无谓地浅拷贝一份。
func TestWithLLMOverridesReturnsSameAgentWhenEmpty(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	ag := newAIAgent(reg, &recordingLLM{}, stubRecipes{}, nil)

	if got := ag.withLLMOverrides(GenerateOpts{}); got != ag {
		t.Fatal("opts 全空时应返回原 agent（同一指针）")
	}
	if got := ag.withLLMOverrides(GenerateOpts{MaxRetries: 3}); got != ag {
		t.Fatal("只给了非模型字段时也应返回原 agent")
	}
	if got := ag.withLLMOverrides(GenerateOpts{Model: "m"}); got == ag {
		t.Fatal("给了模型字段时应返回新 agent")
	}
}

// TestOverrideLLMOnlyFillsEmptyFields —— overrideLLM 的优先级语义：
// 调用点显式传的参数**不被** opts 覆盖顶掉（注释里承诺的优先级）。
func TestOverrideLLMOnlyFillsEmptyFields(t *testing.T) {
	base := &recordingLLM{}
	o := overrideLLM{base: base, opts: GenerateOpts{
		Provider: "opt-prov", Model: "opt-model", BaseURL: "opt-url", APIKey: "opt-key",
	}}

	// 完全空的 request → 四个字段都被覆盖填上
	if _, err := o.Complete(context.Background(), LLMRequest{}); err != nil {
		t.Fatalf("Complete 失败: %v", err)
	}
	got := base.got[0]
	if got.Provider != "opt-prov" || got.Model != "opt-model" || got.BaseURL != "opt-url" || got.APIKey != "opt-key" {
		t.Fatalf("空字段应被 opts 覆盖填满，实际 %+v", got)
	}

	// 调用点显式给了值 → 不被覆盖
	if _, err := o.Complete(context.Background(), LLMRequest{Model: "explicit", Provider: "exp"}); err != nil {
		t.Fatalf("Complete 失败: %v", err)
	}
	got = base.got[1]
	if got.Model != "explicit" {
		t.Fatalf("调用点显式给的 model 不应被覆盖，实际 %q", got.Model)
	}
	if got.Provider != "exp" {
		t.Fatalf("调用点显式给的 provider 不应被覆盖，实际 %q", got.Provider)
	}
	if got.BaseURL != "opt-url" || got.APIKey != "opt-key" {
		t.Fatalf("调用点没给的字段仍应被填上，实际 %+v", got)
	}
}
