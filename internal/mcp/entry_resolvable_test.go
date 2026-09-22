package mcp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gear/internal/engine"
	"gear/internal/model"
)

// TestCheckToolEntryResolvable 覆盖 process entry 的静态可达性检查（2026-09-22 新增）。
//
// 动机：entry 写错路径原先要等运行期才炸，报错还是误导性的
// `executable file not found in %PATH%`——把病因指向 PATH，与「路径写坏」无关。
// 分层给结论是为了**不误杀**：只有绝对路径才敢下 error，相对路径/裸名字只给提示
// （运行时 cwd 与 PATH 都可能与校验时不同）。
func TestCheckToolEntryResolvable(t *testing.T) {
	dir := t.TempDir()
	present := filepath.Join(dir, "present.exe")
	if err := os.WriteFile(present, []byte("stub"), 0o755); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "absent.exe")

	proc := func(entry string) *model.Tool {
		return &model.Tool{ID: "t.probe", Runtime: model.RuntimeProcess, Entry: entry}
	}

	// 存在的绝对路径：不报。
	if d, w := checkToolEntryResolvable(proc("'" + present + "' --flag")); len(d) != 0 || len(w) != 0 {
		t.Fatalf("存在的绝对路径不应有诊断/告警，实际 diags=%v warns=%v", d, w)
	}
	// 不存在的绝对路径：必须 error 级诊断，且带修法。
	d, w := checkToolEntryResolvable(proc("'" + missing + "' --flag"))
	if len(d) != 1 || d[0].Code != engine.CodeEntryExeUnresolved {
		t.Fatalf("不存在的绝对路径应报 %s，实际 %v", engine.CodeEntryExeUnresolved, d)
	}
	if d[0].Hint == "" || d[0].Field == "" {
		t.Fatalf("诊断必须给出字段与修法: %+v", d[0])
	}
	if len(w) != 0 {
		t.Fatalf("绝对路径已能确定结论，不该再叠一条告警: %v", w)
	}
	// 相对路径找不到：只 warning（运行时 cwd 可能不同，不敢断言错）。
	d, w = checkToolEntryResolvable(proc("./nope-xyz.exe --flag"))
	if len(d) != 0 || len(w) != 1 {
		t.Fatalf("相对路径找不到应只给告警，实际 diags=%v warns=%v", d, w)
	}
	// 裸名字找不到：同样只 warning。
	d, w = checkToolEntryResolvable(proc("definitely-not-a-real-cmd-xyz --flag"))
	if len(d) != 0 || len(w) != 1 {
		t.Fatalf("裸名字找不到应只给告警，实际 diags=%v warns=%v", d, w)
	}
	// 路径类 flag 取**相对值**：只 warning，且必须指向 GEAR_REGISTRY 这条正解
	//（判据与措辞由 internal/cmdline 提供，与 CLI 的 tool register 是同一条）。
	// 这条与「可执行文件能不能找到」是两件事，故用存在的绝对路径当 exe，隔离出单独一条告警。
	d, w = checkToolEntryResolvable(proc("'" + present + "' --registry build/ui-registry.db"))
	if len(d) != 0 || len(w) != 1 {
		t.Fatalf("相对 --registry 应只给一条告警，实际 diags=%v warns=%v", d, w)
	}
	if !strings.Contains(w[0], "--registry") || !strings.Contains(w[0], "GEAR_REGISTRY") {
		t.Fatalf("告警必须点出参数名与正解（省略它、由起服方注入）：%s", w[0])
	}
	// 绝对取值：零告警——这正是被推荐的写法，报了就是噪声（噪声会让护栏被直接忽略）。
	if d, w = checkToolEntryResolvable(proc("'" + present + "' --registry " + filepath.Join(dir, "r.db"))); len(d) != 0 || len(w) != 0 {
		t.Fatalf("绝对 --registry 不应有诊断/告警，实际 diags=%v warns=%v", d, w)
	}
	// --apps-dir 同样在清单内；但它没有 GEAR_REGISTRY 这条信道，措辞必须不同。
	if _, w = checkToolEntryResolvable(proc("'" + present + "' --apps-dir apps")); len(w) != 1 || !strings.Contains(w[0], "--apps-dir") {
		t.Fatalf("相对 --apps-dir 应给一条告警，实际 warns=%v", w)
	}
	// 非路径类 flag 不报（清单是判定的一部分，不能顺手把所有取值都算进来）。
	if d, w = checkToolEntryResolvable(proc("'" + present + "' --addr 127.0.0.1:8787")); len(d) != 0 || len(w) != 0 {
		t.Fatalf("--addr 不是路径类 flag，不应有告警，实际 diags=%v warns=%v", d, w)
	}
	// 非 process runtime：完全不检查（lua 的 entry 是脚本体，不是命令行）。
	if d, w := checkToolEntryResolvable(&model.Tool{ID: "t.lua", Runtime: model.RuntimeLua, Entry: "return {}"}); len(d) != 0 || len(w) != 0 {
		t.Fatalf("lua Tool 不应参与 entry 可达性检查，实际 diags=%v warns=%v", d, w)
	}
	if d, w := checkToolEntryResolvable(nil); len(d) != 0 || len(w) != 0 {
		t.Fatalf("nil 应安全返回，实际 %v %v", d, w)
	}
}

// TestValidateFlagsUnreachableProcessEntry：把「路径写坏」提前到写作期 —— 这是本项修复的
// 全部价值所在（在没跑起来之前就告诉智能体改哪个字段）。
func TestValidateFlagsUnreachableProcessEntry(t *testing.T) {
	f := newSmokeFixture(t)
	missing := filepath.Join(t.TempDir(), "absent.exe")
	yaml := fmt.Sprintf(`kind: Tool
id: ghost.tool
name: Ghost
description: entry 指向不存在的可执行文件
runtime: process
entry: '%s --flag'
input_schema: {type: object}
output_schema: {type: object}
versions:
  - version: "1.0.0"
    state: stable
`, missing)

	var out struct {
		Valid       bool                `json:"valid"`
		Diagnostics []engine.Diagnostic `json:"diagnostics"`
	}
	callTool(t, f.session, "gear_validate", map[string]any{"yaml": yaml}, &out)
	if out.Valid {
		t.Fatalf("路径不存在仍判 valid=true，诊断=%v", out.Diagnostics)
	}
	found := false
	for _, d := range out.Diagnostics {
		if d.Code == engine.CodeEntryExeUnresolved {
			found = true
		}
	}
	if !found {
		t.Fatalf("应报 %s，实际诊断 %+v", engine.CodeEntryExeUnresolved, out.Diagnostics)
	}
}

// TestValidateAcceptsResolvableProcessEntry：可达的绝对路径不得被误报
// （否则这条检查会变成噪声，用户很快学会无视它）。
func TestValidateAcceptsResolvableProcessEntry(t *testing.T) {
	f := newSmokeFixture(t)
	self, err := os.Executable()
	if err != nil {
		t.Skipf("取不到本进程可执行文件路径: %v", err)
	}
	yaml := fmt.Sprintf(`kind: Tool
id: real.tool
name: Real
description: entry 指向真实存在的可执行文件
runtime: process
entry: '%s --version'
input_schema: {type: object}
output_schema: {type: object}
versions:
  - version: "1.0.0"
    state: stable
`, self)

	var out struct {
		Valid       bool                `json:"valid"`
		Diagnostics []engine.Diagnostic `json:"diagnostics"`
	}
	callTool(t, f.session, "gear_validate", map[string]any{"yaml": yaml}, &out)
	for _, d := range out.Diagnostics {
		if d.Code == engine.CodeEntryExeUnresolved {
			t.Fatalf("真实存在的可执行文件被误报为不可达: %+v", d)
		}
	}
}

// TestDoctorReportsAIReadiness 覆盖 doctor 新增的 AI 就绪块。
//
// 动机：没配 Key 时，此前只能等业务跑起来撞到厂商的
// `LLM HTTP 401 {"code":30014,"message":"Token is invalid."}`——那句话既没说缺哪个
// 环境变量，也没说去哪配。doctor 的职责就是「把环境问题与 YAML 问题一次分开」，
// AI 凭据是这块拼图里此前唯一没被覆盖的。
func TestDoctorReportsAIReadiness(t *testing.T) {
	f := newSmokeFixture(t)
	var doc struct {
		AI struct {
			Provider   string `json:"provider"`
			KeyFromEnv bool   `json:"key_from_env"`
			Ready      bool   `json:"ready"`
			Blocked    string `json:"blocked"`
			HowToFix   string `json:"how_to_fix"`
			KeyEnv     string `json:"key_env"`
		} `json:"ai"`
		Warnings []string `json:"warnings"`
	}

	t.Setenv("GEAR_AI_API_KEY", "")
	callTool(t, f.session, "gear_doctor", nil, &doc)
	if doc.AI.Provider == "" || doc.AI.KeyEnv != "GEAR_AI_API_KEY" {
		t.Fatalf("ai 块缺基本信息: %+v", doc.AI)
	}
	if doc.AI.Ready || doc.AI.KeyFromEnv {
		t.Fatalf("未设 Key 时应报未就绪: %+v", doc.AI)
	}
	if doc.AI.Blocked == "" || doc.AI.HowToFix == "" {
		t.Fatalf("未就绪必须给出阻塞原因与修法: %+v", doc.AI)
	}
	if !hasWarningWith(doc.Warnings, "AI 能力未就绪") {
		t.Fatalf("未就绪应同时出现在 warnings 里（别让它只藏在结构体深处）: %v", doc.Warnings)
	}
	// 绝不能在响应里出现明文——key_env 只能是**变量名**。
	if strings.Contains(doc.AI.Blocked, "sk-") {
		t.Fatal("响应泄漏了疑似密钥明文")
	}

	t.Setenv("GEAR_AI_API_KEY", "sk-test-not-a-real-key")
	var doc2 struct {
		AI struct {
			KeyFromEnv bool `json:"key_from_env"`
			Ready      bool `json:"ready"`
		} `json:"ai"`
	}
	callTool(t, f.session, "gear_doctor", nil, &doc2)
	if !doc2.AI.KeyFromEnv || !doc2.AI.Ready {
		t.Fatalf("设了 Key 后应报就绪: %+v", doc2.AI)
	}
}

// TestDoctorMissingSidecarsAreStructured 锁住「结构化字段不能是空对象」。
//
// 2026-09-22 实测：missing_sidecars 返回 `[{},{},{}]`——sidecarIssue 的字段全未导出，
// Marshal 出来就是空对象，真正的内容只藏在 warnings 的中文文本里。
// 智能体要的是可解析的清单，不该逼它去正则匹配告警。
func TestDoctorMissingSidecarsAreStructured(t *testing.T) {
	b, err := json.Marshal(sidecarIssue{
		Ext:      "ext.email",
		Expected: `/build/email-sidecar`,
		Searched: []string{"/build", "/build/plugin"},
	})
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	if string(b) == "{}" {
		t.Fatal("sidecarIssue 序列化成了空对象——字段必须导出且带 json tag")
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"ext", "expected", "searched"} {
		if got[k] == nil {
			t.Fatalf("缺字段 %s: %s", k, b)
		}
	}
}

func hasWarningWith(warnings []string, sub string) bool {
	for _, w := range warnings {
		if strings.Contains(w, sub) {
			return true
		}
	}
	return false
}
