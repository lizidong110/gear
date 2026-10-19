package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gear/internal/sidecar/describe"
)

// 本文件测「侧车自描述扫描」（internal/engine/describe.go）。
//
// 假侧车的做法：**让测试二进制自己扮演侧车**。设置 GEAR_TEST_FAKE_SIDECAR 后跑 os.Args[0]，
// TestMain 会在跑任何用例之前截住它，按角色打印不同的自描述（或装死 / 报错 / 吐垃圾）。
// 这样不必在测试里 `go build` 一个额外二进制（慢且依赖工具链），
// 又真走了「fork 进程 + 读 stdout」这条路径——只 mock Parse 是测不到超时与退出码的。

const fakeSidecarEnv = "GEAR_TEST_FAKE_SIDECAR"

func TestMain(m *testing.M) {
	if role := os.Getenv(fakeSidecarEnv); role != "" {
		runFakeSidecar(role)
		return
	}
	os.Exit(m.Run())
}

func runFakeSidecar(role string) {
	switch role {
	case "ok":
		fmt.Print(`{"methods":["a","b"],"schemas":{` +
			`"a":{"description":"方法一","parameters":{"x":{"type":"string","description":"参数 x","required":true}}},` +
			`"b":{"description":"方法二","parameters":{"y":{"type":"number","description":"参数 y"}}}}}`)
		os.Exit(0)
	case "changed":
		// 与 ok 内容不同，用于验证「二进制变了必须重扫」。
		fmt.Print(`{"methods":["c"],"schemas":{"c":{"description":"方法三","parameters":{}}}}`)
		os.Exit(0)
	case "badjson":
		fmt.Print("i am not json")
		os.Exit(0)
	case "empty":
		fmt.Print(`{"methods":[],"schemas":{}}`)
		os.Exit(0)
	case "fail":
		fmt.Fprint(os.Stderr, "boom: 侧车拒绝自描述")
		os.Exit(3)
	case "hang":
		time.Sleep(30 * time.Second)
		os.Exit(0)
	default:
		fmt.Fprintf(os.Stderr, "未知角色 %q", role)
		os.Exit(9)
	}
}

func fakeCmd(t *testing.T, role string) []string {
	t.Helper()
	t.Setenv(fakeSidecarEnv, role)
	exe, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatalf("取测试二进制绝对路径失败: %v", err)
	}
	resetDescribeCache()
	t.Cleanup(resetDescribeCache)
	return []string{exe}
}

// ── ApplySidecarDescribes ──

func TestApplySidecarDescribesFillsMethodsAndSchemas(t *testing.T) {
	cmd := fakeCmd(t, "ok")
	specs := map[string]ExtSpec{"ext.fake": {Cmd: cmd, Describe: true}}
	if err := ApplySidecarDescribes(specs); err != nil {
		t.Fatalf("不该报错: %v", err)
	}
	got := specs["ext.fake"]
	if len(got.Methods) != 2 || got.Methods[0] != "a" || got.Methods[1] != "b" {
		t.Fatalf("methods 应来自侧车 [a b]，实际 %v", got.Methods)
	}
	sch, ok := got.Schemas["a"]
	if !ok {
		t.Fatal("schemas 缺方法 a")
	}
	if sch.Description != "方法一" {
		t.Fatalf("描述没带过来: %q", sch.Description)
	}
	if p := sch.Parameters["x"]; p.Type != "string" || !p.Required {
		t.Fatalf("参数 x 的 schema 不对: %+v", p)
	}
}

func TestApplySidecarDescribesRejectsSchemasAlongsideDescribe(t *testing.T) {
	cmd := fakeCmd(t, "ok")
	specs := map[string]ExtSpec{"ext.fake": {Cmd: cmd, Describe: true, Schemas: map[string]MethodSchema{
		"a": {Description: "yaml 里那份"},
	}}}
	err := ApplySidecarDescribes(specs)
	if err == nil {
		t.Fatal("同时写 schemas 与 describe:true 必须被拒（两份描述必然漂移）")
	}
	if !strings.Contains(err.Error(), "不该再写 schemas") {
		t.Fatalf("错误文案不对: %v", err)
	}
}

func TestApplySidecarDescribesRejectsMethodMismatch(t *testing.T) {
	cmd := fakeCmd(t, "ok")
	specs := map[string]ExtSpec{"ext.fake": {Cmd: cmd, Describe: true, Methods: []string{"a", "zzz"}}}
	err := ApplySidecarDescribes(specs)
	if err == nil {
		t.Fatal("methods 与侧车不一致必须被拒")
	}
	// 必须点名哪个方法对不上，否则人还得自己去比对两份清单。
	for _, want := range []string{"zzz", "侧车有而配置没有", "b"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("错误文案没点名差异（缺 %q）: %v", want, err)
		}
	}
}

func TestApplySidecarDescribesKeepsIdenticalYamlMethods(t *testing.T) {
	cmd := fakeCmd(t, "ok")
	// yaml 里写了 methods 但与侧车一致：应当保留（不因顺序不同就报错——
	// diff 按集合比，不按序列比，否则「把顺序调一下」就成了破坏性改动）。
	specs := map[string]ExtSpec{"ext.fake": {Cmd: cmd, Describe: true, Methods: []string{"b", "a"}}}
	if err := ApplySidecarDescribes(specs); err != nil {
		t.Fatalf("顺序不同但集合相同不该报错: %v", err)
	}
}

func TestApplySidecarDescribesLeavesOthersAlone(t *testing.T) {
	cmd := fakeCmd(t, "ok")
	specs := map[string]ExtSpec{
		"ext.fake": {Cmd: cmd, Describe: true},
		// 没声明 describe 的：既不该被扫描改动，也不该因为它没有 cmd 就报错——
		// 扫描是**逐扩展**的可选行为，不是全局前提。
		"ext.plain": {Methods: []string{"m"}, Schemas: map[string]MethodSchema{
			"m": {Description: "yaml 描述"},
		}},
	}
	if err := ApplySidecarDescribes(specs); err != nil {
		t.Fatalf("不该报错: %v", err)
	}
	plain := specs["ext.plain"]
	if len(plain.Methods) != 1 || plain.Methods[0] != "m" || plain.Schemas["m"].Description != "yaml 描述" {
		t.Fatalf("未声明 describe 的 ext 不该被扫描改动: %+v", plain)
	}
	if len(specs["ext.fake"].Methods) == 0 {
		t.Fatal("声明了 describe 的 ext 的方法应被填")
	}
}

// ── ProbeSidecarDescribe：连不上 / 吐垃圾 / 不退出，都必须 fail-closed ──

func TestProbeSidecarDescribeFailClosed(t *testing.T) {
	cases := []struct {
		role string
		want string
	}{
		{"badjson", "输出不合格式"},
		{"empty", "methods 为空"},
		{"fail", "失败"},
	}
	for _, tc := range cases {
		t.Run(tc.role, func(t *testing.T) {
			cmd := fakeCmd(t, tc.role)
			if _, err := ProbeSidecarDescribe(cmd); err == nil {
				t.Fatalf("侧车 %q 的情形必须报错，却返回了成功", tc.role)
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误文案不对: got=%q want包含=%q", err.Error(), tc.want)
			}
		})
	}
}

func TestProbeSidecarDescribeRejectsMissingBinary(t *testing.T) {
	resetDescribeCache()
	if _, err := ProbeSidecarDescribe([]string{filepath.Join(t.TempDir(), "不存在.exe")}); err == nil {
		t.Fatal("二进制不存在必须报错")
	}
	if _, err := ProbeSidecarDescribe(nil); err == nil {
		t.Fatal("cmd 为空必须报错")
	}
}

func TestProbeSidecarDescribeTimeoutIsEnforced(t *testing.T) {
	cmd := fakeCmd(t, "hang")
	old := describeTimeout
	describeTimeout = 300 * time.Millisecond
	defer func() { describeTimeout = old }()
	start := time.Now()
	_, err := ProbeSidecarDescribe(cmd)
	if err == nil {
		t.Fatal("侧车不退出必须按超时判失败（否则一次扫描能把起服卡死）")
	}
	// 断言落在「未退出」上：文案是「跑超过 N 未退出」（「超时」二字未必出现在同一处），
	// 断在能区分的那一段——是超时失败，而不是别的失败。
	if !strings.Contains(err.Error(), "未退出") {
		t.Fatalf("应报超时: %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("超时没生效，等了 %v", d)
	}
}

// ── 缓存 ──

// 缓存键带 mtime/size 是刻意的：只按路径缓存的话，重编后的侧车会继续用旧自描述，
// 「加了方法却看不见」就又变成一次静默事故（2026-10-08 那次就是这个形态）。
func TestProbeCacheInvalidatesWhenBinaryChanges(t *testing.T) {
	exe := fakeCmd(t, "ok")[0]

	first, err := ProbeSidecarDescribe([]string{exe})
	if err != nil {
		t.Fatalf("首次扫描失败: %v", err)
	}
	// 换角色但二进制没动 → 应当命中缓存（同一次进程内不重复 fork）
	_ = os.Setenv(fakeSidecarEnv, "changed")
	if _, err := ProbeSidecarDescribe([]string{exe}); err != nil {
		t.Fatalf("二次扫描失败: %v", err)
	}
	// 把 mtime 往前挪 → 视为换了二进制 → 必须重扫
	future := time.Now().Add(2 * time.Hour)
	if err := os.Chtimes(exe, future, future); err != nil {
		t.Fatalf("改 mtime 失败: %v", err)
	}
	again, err := ProbeSidecarDescribe([]string{exe})
	if err != nil {
		t.Fatalf("改 mtime 后扫描失败: %v", err)
	}
	if len(again.Methods) != 1 || again.Methods[0] != "c" {
		t.Fatalf("二进制变了却还在用旧自描述: %v（首次 %v）", again.Methods, first.Methods)
	}
}

func TestDescribeCacheKeyUsesPathMtimeAndSize(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "sidecar")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString("x"); err != nil {
		t.Fatal(err)
	}
	k1, ok := describeCacheKey(f.Name())
	if !ok {
		t.Fatal("存在的文件应能算出缓存键")
	}
	k2, _ := describeCacheKey(f.Name())
	if k1 != k2 {
		t.Fatal("同一文件两次取键应一致")
	}
	if _, err := describeCacheKey(filepath.Join(t.TempDir(), "不存在")); err {
		t.Fatal("不存在的文件不应参与缓存（拿不到指纹就不该复用）")
	}
}

// ── 纯函数 ──

func TestDescribeToSchemasKeepsRequiredAndEnum(t *testing.T) {
	out := describeToSchemas(mustParse(t, `{"methods":["m"],"schemas":{"m":{"description":"d","parameters":{
		"kind":{"type":"string","description":"类型","required":true,"enum":["a","b"]}}}}}`))
	sch := out["m"]
	if sch.Description != "d" {
		t.Fatalf("描述丢了: %q", sch.Description)
	}
	p := sch.Parameters["kind"]
	if !p.Required || len(p.Enum) != 2 {
		t.Fatalf("required/enum 没带过来: %+v", p)
	}
	// 派生字段（InvokeForm/LuaCall）不在转换期填：那是目录渲染时才补的东西，
	// 提前填会让「声明值」被「派生值」污染。
	if sch.InvokeForm != "" || sch.LuaCall != "" {
		t.Fatalf("转换期不该填派生字段: %+v", sch)
	}
}

func TestDiffStringSets(t *testing.T) {
	if diffStringSets([]string{"a", "b"}, []string{"b", "a"}) != "" {
		t.Fatal("集合相同（顺序不同）应判无差异")
	}
	d := diffStringSets([]string{"a", "b"}, []string{"b", "c"})
	for _, want := range []string{"a", "c"} {
		if !strings.Contains(d, want) {
			t.Fatalf("差异描述应点名 %q: %s", want, d)
		}
	}
}

func mustParse(t *testing.T, raw string) describe.Spec {
	t.Helper()
	s, err := describe.Parse([]byte(raw))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	return s
}
