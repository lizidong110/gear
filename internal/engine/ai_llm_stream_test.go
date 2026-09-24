package engine

import (
	"context"
	"testing"

	"github.com/yuin/gopher-lua"
)

// TestMockLLMCompleteStream 验证 MockLLM 的增量版：一次性拿到完整文本后回调一次。
func TestMockLLMCompleteStream(t *testing.T) {
	m := &MockLLM{Respond: func(req LLMRequest) string { return "hello world" }}
	var deltas []string
	err := m.CompleteStream(context.Background(), LLMRequest{Prompt: "x"}, func(content, reasoning string) error {
		deltas = append(deltas, content)
		return nil
	})
	if err != nil {
		t.Fatalf("CompleteStream 报错: %v", err)
	}
	if len(deltas) != 1 || deltas[0] != "hello world" {
		t.Fatalf("期望单次回调 hello world，实际 %v", deltas)
	}
}

// TestHTTPLLMCompleteStreamMock 验证 HTTPLLM 在 mock provider 分支下也能走 CompleteStream。
func TestHTTPLLMCompleteStreamMock(t *testing.T) {
	t.Setenv("GEAR_AI_PROVIDER", "mock")
	var got string
	llm := HTTPLLM{}
	err := llm.CompleteStream(context.Background(), LLMRequest{Prompt: "hi"}, func(content, reasoning string) error {
		got += content
		return nil
	})
	if err != nil {
		t.Fatalf("CompleteStream(mock) 报错: %v", err)
	}
	if got == "" {
		t.Fatal("mock 分支未回调任何内容")
	}
}

// TestExtAICompleteStreamLua 验证 ext.ai.complete_stream 的 Lua 回调式绑定：
// 每个 LLM 增量触发一次回调（此处 MockLLM 仅一次）。
func TestExtAICompleteStreamLua(t *testing.T) {
	llm := &MockLLM{Respond: func(req LLMRequest) string { return "你好世界" }}
	reg := newTestRegistry(t)
	defer reg.Close()
	ag := newAIAgent(reg, llm, stubRecipes{}, nil)
	ext := AIExtensionInProcess(ag)

	L := lua.NewState()
	defer L.Close()

	tbl, err := ext.BuildLua(L, nil)
	if err != nil {
		t.Fatalf("BuildLua 失败: %v", err)
	}
	tblT, ok := tbl.(*lua.LTable)
	if !ok {
		t.Fatal("BuildLua 未返回 table")
	}
	fn := tblT.RawGetString("complete_stream")
	cbfn, ok := fn.(*lua.LFunction)
	if !ok {
		t.Fatal("complete_stream 不是 Lua 函数")
	}

	args := L.NewTable()
	args.RawSetString("prompt", lua.LString("hi"))
	cb := L.NewFunction(func(L *lua.LState) int {
		content := L.CheckString(1)
		cur := L.GetGlobal("_collect")
		s := ""
		if cur.Type() == lua.LTString {
			s = lua.LVAsString(cur)
		}
		L.SetGlobal("_collect", lua.LString(s+content))
		return 0
	})
	if err := L.CallByParam(lua.P{Fn: cbfn, NRet: 0, Protect: true}, args, cb); err != nil {
		t.Fatalf("调用 complete_stream 失败: %v", err)
	}
	got := lua.LVAsString(L.GetGlobal("_collect"))
	if got != "你好世界" {
		t.Fatalf("Lua 回调未收到完整内容，实际 %q", got)
	}
}
