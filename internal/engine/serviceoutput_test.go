package engine

import (
	"context"
	"testing"
)

// ─── 2026-09-26：长驻子进程的「首行声明」作为 step output ─────────────────────
// 背景：一次性 process 走 setStep（output = stdout 末行 JSON），但 service 分支从没调过
// setStep ⇒ `${steps.<id>.output.port}` 对长驻 step 永远取不到，而那正是全链路端口分配
// 的传递通道（static_site 的 proxy.target 要靠它拿到后端真实端口）。
// 机制：长驻进程 stdout 第一行吐 `{"port":N}`，父侧解析成 step output。解析不出来就是 nil。

// S2-svout-1：首行是 JSON object → 解析成 output。
func TestServiceFirstParsesJSONFirstLine(t *testing.T) {
	f := &serviceFirst{}
	f.set(`{"port":18923}`)
	f.set(`{"port":99999}`) // 只认首行，后续行不得覆盖
	if got := f.take()["port"]; got == nil || got != float64(18923) {
		t.Fatalf("首行应解析出 port=18923，实际 %v", f.take())
	}
}

// S2-svout-2：首行不是 JSON（banner / 普通日志）→ 如实留空，**绝不凑数**。
func TestServiceFirstIgnoresNonJSONFirstLine(t *testing.T) {
	for _, line := range []string{"Gear REST 服务监听 ...", "", "   ", "not-json-at-all"} {
		f := &serviceFirst{}
		f.set(line)
		if got := f.take(); got != nil {
			t.Fatalf("首行 %q 非 JSON 时应为 nil，实际 %v", line, got)
		}
	}
}

// S2-svout-3：rt.Start 起一个「先报端口再长驻」的进程，Output() 必须拿到端口，
// 且拿到的端口真能连上（不是把入参 0 原样回给调用方）。
func TestServiceProcessReportsRealPort(t *testing.T) {
	rt := processRuntime{}
	// 首行吐端口后长驻 3s。双引号必须转义（`\"`）——shell 会做引号移除：
	// 写成 `echo {"port":18923}` 出去的是 `{port:18923}`，父侧解析失败 ⇒ 又变成「拿不到端口」。
	// 依赖 sh（repo 内既有 process 测试同样依赖，见 TestProcessTreeKillNoOrphan）。
	p, err := rt.Start(context.Background(), `sh -c 'echo {\"port\":18923}; sleep 3'`, map[string]interface{}{}, nil)
	if err != nil {
		t.Fatalf("启动失败: %v", err)
	}
	out := outputOf(p)
	if out == nil {
		t.Fatal("service 必须能报出首行声明，实际 nil")
	}
	port, ok := out["port"].(float64)
	if !ok || int(port) != 18923 {
		t.Fatalf("port 应为 18923，实际 %v", out["port"])
	}
	_ = p.Kill()
}

// S2-svout-4：不报声明的长驻进程 → Output() 为 nil（不是空 map、不是 0），
// 且**不白等满超时**（进程秒退时立即收工）。
func TestServiceProcessWithoutDeclarationYieldsNil(t *testing.T) {
	rt := processRuntime{}
	p, err := rt.Start(context.Background(), "sleep 30", map[string]interface{}{}, nil)
	if err != nil {
		t.Fatalf("启动失败: %v", err)
	}
	_ = p.Kill() // 先杀：此用例只为验证「无声明时如实为 nil」，进程活着也无妨
	if got := outputOf(p); got != nil {
		t.Fatalf("无首行声明时应为 nil，实际 %v", got)
	}
}

// S2-svout-5：service step 的 output 经 setStep 落进 runContext，下游能引用
// `${steps.<id>.output.<k>}` —— 锁住「长驻 step 也有 output 通道」这条契约。
// 与之对照：engine_test.go:130 断言引用**不存在**的 step output 必须报错，
// 所以这里一旦漏掉 setStep，本用例会立刻红在「未知步骤」上。
func TestServiceStepOutputIsVisibleToTemplates(t *testing.T) {
	rc := &runContext{steps: map[string]map[string]interface{}{}}
	rc.setStep("api", map[string]interface{}{}, map[string]interface{}{"port": float64(18923)})

	if got, err := expandString("http://127.0.0.1:${steps.api.output.port}", rc); err != nil || got != "http://127.0.0.1:18923" {
		t.Fatalf("模板应解析出真实端口，实际 %q err=%v", got, err)
	}
	if _, err := expandString("${steps.api.output.nosuchkey}", rc); err == nil {
		t.Fatal("引用不存在的 output 字段必须报错，否则下游会拿到静默错串")
	}
}
