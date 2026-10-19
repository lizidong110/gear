package describe

import (
	"encoding/json"
	"os"
	"os/exec"
	"testing"
)

// 本包只管「自描述的线格式与开关」，不含业务逻辑，所以测试全部是纯函数级的：
// 不 fork 进程、不依赖网络。真正跑进程的那一段在 internal/engine/describe_test.go。

func TestParseAcceptsWellFormedSpec(t *testing.T) {
	raw := []byte(`{
      "methods": ["find", "insert_one"],
      "schemas": {
        "find": {"description":"查询", "parameters": {
            "database": {"type":"string","description":"库名","required":true},
            "limit": {"type":"number","description":"条数"}
        }},
        "insert_one": {"description":"插入", "parameters": {"document": {"type":"object","required":true}}}
      }
    }`)
	spec, err := Parse(raw)
	if err != nil {
		t.Fatalf("合法自描述不该被拒: %v", err)
	}
	if len(spec.Methods) != 2 {
		t.Fatalf("应有 2 个方法，实际 %d", len(spec.Methods))
	}
	p := spec.Schemas["find"].Parameters["database"]
	if p.Type != "string" || !p.Required {
		t.Fatalf("database 参数解析错了: %+v", p)
	}
}

func TestParseRejectsBrokenSpec(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		// 一个不说自己有啥方法的自描述等于没描述，让它过会让「声明了 describe:true
		// 却什么也没校验」成为新的静默缺口。
		{"methods 为空", `{"methods":[],"schemas":{}}`, "methods 为空"},
		{"不是 JSON", `i am not json`, "不是合法的 JSON"},
		{"空方法名", `{"methods":["a",""],"schemas":{}}`, "空方法名"},
		{"重复方法名", `{"methods":["a","a"],"schemas":{}}`, "重复方法名"},
		{"schema 里有多余方法", `{"methods":["a"],"schemas":{"b":{}}}`, "不在 methods 里"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Parse([]byte(tc.raw)); err == nil {
				t.Fatalf("应当被拒，却通过了")
			} else if !contains(err.Error(), tc.want) {
				t.Fatalf("错误文案不对: got=%q want包含=%q", err.Error(), tc.want)
			}
		})
	}
}

// Spec 的 JSON 形状必须与内核那边的读法一致（内核 Parse 回来的东西直接进静态校验），
// 所以这里锁住「omitempty 不该吞掉 type」这类细节。
func TestSpecMarshalsWithExpectedShape(t *testing.T) {
	s := Spec{
		Methods: []string{"ping"},
		Schemas: map[string]Method{"ping": {Description: "探活", Parameters: map[string]Param{
			"host": {Type: "string", Required: true},
		}}},
	}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	for _, want := range []string{`"methods":["ping"]`, `"type":"string"`, `"required":true`} {
		if !contains(string(b), want) {
			t.Fatalf("输出缺少 %s: %s", want, b)
		}
	}
	// 往返：自己产出的必须自己能读回（防止「写的是 A 形状、读的是 B 形状」）
	if _, err := Parse(b); err != nil {
		t.Fatalf("自己产出的自描述自己读不回: %v", err)
	}
}

func TestRequestedByFlagAndEnv(t *testing.T) {
	oldArgs := os.Args
	oldEnv := os.Getenv(EnvKey)
	defer func() {
		os.Args = oldArgs
		if oldEnv == "" {
			_ = os.Unsetenv(EnvKey)
		} else {
			_ = os.Setenv(EnvKey, oldEnv)
		}
	}()

	os.Args = []string{"sidecar", "--serve"}
	_ = os.Unsetenv(EnvKey)
	if Requested() {
		t.Fatal("没给 --describe 也不该触发")
	}
	os.Args = []string{"sidecar", "--describe"}
	if !Requested() {
		t.Fatal("给了 --describe 就该触发")
	}
	os.Args = []string{"sidecar"}
	if err := os.Setenv(EnvKey, "1"); err != nil {
		t.Fatal(err)
	}
	if !Requested() {
		t.Fatal("GEAR_DESCRIBE=1 也该触发")
	}
	// 其它「明确为真」的写法也要认（大小写、yes）
	for _, v := range []string{"true", "TRUE", "yes"} {
		if err := os.Setenv(EnvKey, v); err != nil {
			t.Fatal(err)
		}
		if !Requested() {
			t.Fatalf("GEAR_DESCRIBE=%s 也该触发", v)
		}
	}
	// 环境变量必须是「明确为真」才算：写成 0 不该触发
	// （否则 side-car 被任何带 GEAR_DESCRIBE 的环境误触发，行为不可预测）。
	if err := os.Setenv(EnvKey, "0"); err != nil {
		t.Fatal(err)
	}
	if Requested() {
		t.Fatal("GEAR_DESCRIBE=0 不该触发")
	}
}

// Emit 负责「写 JSON 到 stdout + os.Exit(0)」两个责任；用子进程跑一遍，
// 验证打印内容能被 Parse 读回（内核侧就是这么读的），且进程以 0 退出。
func TestEmitWritesJSONAndExits(t *testing.T) {
	if os.Getenv("DESCRIBE_EMIT_SUB") == "1" {
		Emit(Spec{Methods: []string{"ping"}, Schemas: map[string]Method{"ping": {Description: "探活"}}})
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestEmitWritesJSONAndExits")
	cmd.Env = append(os.Environ(), "DESCRIBE_EMIT_SUB=1")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("子进程应退出 0: %v", err)
	}
	var s Spec
	if err := json.Unmarshal(out, &s); err != nil {
		t.Fatalf("子进程没打出合法 JSON: %v\n%s", err, out)
	}
	if len(s.Methods) != 1 || s.Methods[0] != "ping" {
		t.Fatalf("打出的 spec 不对: %+v", s)
	}
}

// EmitIfRequested 在 Requested() 为真时必须走到 Emit（打印并退出），不能静悄悄地返回。
func TestEmitIfRequestedEmitsWhenRequested(t *testing.T) {
	if os.Getenv("DESCRIBE_EIR_SUB") == "1" {
		EmitIfRequested(Spec{Methods: []string{"ping"}})
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestEmitIfRequestedEmitsWhenRequested")
	cmd.Env = append(os.Environ(), "DESCRIBE_EIR_SUB=1", "GEAR_DESCRIBE=1")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("子进程应退出 0: %v", err)
	}
	var s Spec
	if err := json.Unmarshal(out, &s); err != nil {
		t.Fatalf("子进程没打出合法 JSON: %v\n%s", err, out)
	}
	if s.Methods[0] != "ping" {
		t.Fatalf("打出的 spec 不对: %+v", s)
	}
}

// EmitIfRequested 在没被要求时返回 false，且绝不碰 stdout（调用方继续正常启动）。
func TestEmitIfRequestedFalseWhenNotRequested(t *testing.T) {
	oldArgs := os.Args
	oldEnv := os.Getenv(EnvKey)
	defer func() {
		os.Args = oldArgs
		_ = os.Setenv(EnvKey, oldEnv)
	}()
	os.Args = []string{"sidecar", "--serve"}
	_ = os.Unsetenv(EnvKey)
	if EmitIfRequested(Spec{Methods: []string{"ping"}}) {
		t.Fatal("没被要求时应当返回 false")
	}
}

func contains(s, sub string) bool {
	return len(sub) > 0 && (len(s) >= len(sub)) && indexOf(s, sub) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
