package doctor

// doctor_test.go —— 报告形状与两个消费方（MCP / CLI）的一致性。
//
// 这里是 doctor 的**对外契约**所在：判据可以改，形状不能悄悄变——
// gear_doctor 的响应字段被智能体解析，gear doctor 的文本被人读。

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestSidecarIssueMarshalsStructured 锁住「结构化字段不能是空对象」。
//
// 2026-09-22 实测：missing_sidecars 返回 `[{},{},{}]`——字段全未导出，Marshal 出来
// 就是空对象，真正的内容只藏在 warnings 的中文文本里。智能体要的是可解析的清单，
// 不该逼它去正则匹配告警。
func TestSidecarIssueMarshalsStructured(t *testing.T) {
	b, err := json.Marshal(SidecarIssue{
		Ext:      "ext.email",
		Expected: `/build/email-sidecar`,
		Searched: []string{"/build", "/build/plugin"},
	})
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	if string(b) == "{}" {
		t.Fatal("SidecarIssue 序列化成了空对象——字段必须导出且带 json tag")
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

// TestMapKeepsMCPResponseShape 锁住 gear_doctor 的响应形状。
//
// Map 是 MCP 面的唯一出口，字段名与嵌套层级被既有调用方与测试依赖；
// CLI 的 Text() 是另一个出口，两者同源于 Report，但形状各自独立演进（故各锁各的）。
func TestMapKeepsMCPResponseShape(t *testing.T) {
	rep := Report{
		Registry: RegistryInfo{Path: "/tmp/reg.db", Tools: 3, Flows: 2, Hooks: 1},
		Extensions: ExtensionsInfo{
			Config:          "/tmp/extensions.yaml",
			Declared:        []string{"ext.ai"},
			Loaded:          []string{"ext.ai"},
			MissingSidecars: []SidecarIssue{{Ext: "ext.email", Expected: "/b/email-sidecar", Searched: []string{"/b"}}},
			CapsCheck:       []CapsIssue{{Ext: "ext.svc", State: "ok", Implemented: []string{"a"}}},
		},
		AI:       map[string]any{"ready": true},
		Ports:    map[string]string{"8080": "in-use(pid=42)"},
		Apps:     []AppInfo{{AppID: "wms", State: "running", PID: 42}},
		Orphan:   []OrphanInfo{{AppID: "old", Reason: "pid 不在了", PID: 7}},
		AppsDir:  "/tmp/apps",
		Warnings: []string{"某告警"},
	}
	m := rep.Map()

	for _, k := range []string{"registry", "extensions", "ai", "ports", "apps", "orphan", "apps_dir", "warnings"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("Map 缺顶层字段 %s（MCP 响应形状变了）", k)
		}
	}
	reg, _ := m["registry"].(map[string]any)
	if reg["path"] != "/tmp/reg.db" {
		t.Fatalf("registry.path 不对: %v", reg["path"])
	}
	ext, _ := m["extensions"].(map[string]any)
	if _, ok := ext["caps_check"]; !ok {
		t.Fatal("extensions 缺 caps_check（只列异常会让人以为没列出=已校验通过）")
	}
	missing, _ := ext["missing_sidecars"].([]SidecarIssue)
	if len(missing) != 1 || missing[0].Ext != "ext.email" {
		t.Fatalf("missing_sidecars 形状不对: %#v", ext["missing_sidecars"])
	}
	// apps 必须是 []map（既有解析方按 map 取键），不是 []AppInfo。
	apps, ok := m["apps"].([]map[string]any)
	if !ok || len(apps) != 1 || apps[0]["app_id"] != "wms" {
		t.Fatalf("apps 形状不对: %#v", m["apps"])
	}
	orphan, ok := m["orphan"].([]map[string]any)
	if !ok || len(orphan) != 1 || orphan[0]["app_id"] != "old" {
		t.Fatalf("orphan 形状不对: %#v", m["orphan"])
	}
	if w, _ := m["warnings"].([]string); len(w) != 1 {
		t.Fatalf("warnings 不对: %#v", m["warnings"])
	}
}

// TestMapEmptyCollectionsAreNotNull：空清单序列化成 [] 而不是 null——
// 对解析方，「没有」与「没查出来」必须能分开。
func TestMapEmptyCollectionsAreNotNull(t *testing.T) {
	b, err := json.Marshal(Report{Ports: map[string]string{}}.Map())
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"apps", "orphan", "warnings"} {
		if back[k] == nil {
			t.Fatalf("%s 序列化成了 null，应为 []：%s", k, b)
		}
	}
	if _, ok := back["ports"].(map[string]any); !ok {
		t.Fatalf("ports 应为对象（空对象），实际 %v", back["ports"])
	}
}

// TestTextShowsWarningsAndOrigins 锁 CLI 文本的关键信息：告警逐条列出、路径如实显示。
func TestTextShowsWarningsAndOrigins(t *testing.T) {
	rep := Report{
		Registry:   RegistryInfo{Path: "/tmp/reg.db", Tools: 3},
		Extensions: ExtensionsInfo{Config: "", Declared: nil, Loaded: []string{"ext.ai"}},
		AI:         map[string]any{"ready": false, "provider": "siliconflow", "blocked": "没配 key", "how_to_fix": "gear config set ai.api_key"},
		AppsDir:    "/tmp/apps",
		Orphan:     []OrphanInfo{{AppID: "old", Reason: "pid 不在了", PID: 7}},
		Warnings:   []string{"AI 凭据没配", "台账里有遗留应用 old"},
	}
	txt := rep.Text()
	for _, want := range []string{"/tmp/reg.db", "AI", "遗留应用 old", "AI 凭据没配", "台账里有遗留应用 old"} {
		if !strings.Contains(txt, want) {
			t.Fatalf("Text 缺 %q：\n%s", want, txt)
		}
	}
	// 扩展配置找不到时必须明说「未找到」，不能留空让人以为没这项。
	if !strings.Contains(txt, "未找到") {
		t.Fatalf("空路径应显示「未找到」：\n%s", txt)
	}
}

// TestTextNoWarningsSaysClean：全绿时也要有一句结论——
// 「没告警」与「没跑」在文本上必须分得开。
func TestTextNoWarningsSaysClean(t *testing.T) {
	rep := Report{
		Registry: RegistryInfo{Path: "/tmp/reg.db"},
		AI:       map[string]any{"ready": true, "provider": "mock", "key_from_config": false, "key_from_env": false},
		Warnings: []string{},
	}
	txt := rep.Text()
	if !strings.Contains(txt, "无告警") {
		t.Fatalf("无告警时应明说：\n%s", txt)
	}
	if !strings.Contains(txt, "mock") {
		t.Fatalf("provider=mock 应显示为无需 key：\n%s", txt)
	}
}

// TestCheckRequiresRegistry：没有 registry 时如实报错，不 panic（CLI 侧会打这句）。
func TestCheckRequiresRegistry(t *testing.T) {
	if _, err := Check(Options{}); err == nil {
		t.Fatal("没有 registry 时应返回错误")
	}
}
