package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gear/internal/engine"
	"gear/internal/sidecar/describe"
)

// stubProbe 用给定的探测结果替换真探针（真探针会起进程，测试里不能跑）。
func stubProbe(t *testing.T, fn func(cmd []string) (describe.Spec, error)) {
	t.Helper()
	old := probeSidecarDescribe
	probeSidecarDescribe = fn
	t.Cleanup(func() { probeSidecarDescribe = old })
}

// TestSidecarNameFromFile —— 文件名 → 能力名的推导规则（推导错就是把能力登记成别人的名字）。
func TestSidecarNameFromFile(t *testing.T) {
	cases := []struct {
		file string
		want string
		ok   bool
	}{
		{"mongo-sidecar.exe", "ext.mongo", true},
		{"mongo-sidecar", "ext.mongo", true},
		{"Mongo-Sidecar.EXE", "ext.mongo", true},
		{"xlsx-sidecar.exe", "ext.xlsx", true},
		{"sidecar-template.exe", "", false}, // 模板不是能力
		{"sidecar-template", "", false},
		{"gear.exe", "", false},
		{"-sidecar.exe", "", false}, // 空 stem 不猜
	}
	for _, c := range cases {
		got, ok := sidecarNameFromFile(c.file)
		if ok != c.ok || got != c.want {
			t.Errorf("sidecarNameFromFile(%q) = (%q, %v)，期望 (%q, %v)", c.file, got, ok, c.want, c.ok)
		}
	}
}

// TestDraftCmdPath —— 草稿里的 cmd 要用 yaml 惯用的相对写法（相对路径解析不到就能直接崩）。
func TestDraftCmdPath(t *testing.T) {
	cases := []struct{ in, want string }{
		{filepath.Join("repo", "build", "plugin", "mongo-sidecar.exe"), "plugin/mongo-sidecar.exe"},
		{filepath.Join("repo", "build", "gear.exe"), "gear.exe"},
		{filepath.Join("repo", "Plugin", "x-sidecar.exe"), "plugin/x-sidecar.exe"}, // 目录名大小写不敏感
	}
	for _, c := range cases {
		if got := draftCmdPath(c.in); got != c.want {
			t.Errorf("draftCmdPath(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}

// TestDiscoverSidecarsIgnoresNonSidecars —— 只认 `*-sidecar[.exe]`，跳过目录与无关文件。
func TestDiscoverSidecarsIgnoresNonSidecars(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "plugin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"mongo-sidecar.exe", "redis-sidecar.exe", "notes.txt", "sidecar-template.exe"} {
		if err := os.WriteFile(filepath.Join(dir, f), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "kafka-sidecar"), 0o755); err != nil {
		t.Fatal(err)
	}

	cands, scanned := discoverSidecars([]string{dir, filepath.Join(dir, "missing")})
	if len(scanned) != 1 || scanned[0] != dir {
		t.Fatalf("只应把「存在且可读」的目录算进扫描结果，得到 %v", scanned)
	}
	if len(cands) != 2 {
		t.Fatalf("应找到 2 个侧车，得到 %d：%+v", len(cands), cands)
	}
	// 排序稳定：报告与草稿的顺序不该随 ReadDir 顺序漂移。
	if cands[0].Name != "ext.mongo" || cands[1].Name != "ext.redis" {
		t.Fatalf("候选应按能力名排序，得到 %s / %s", cands[0].Name, cands[1].Name)
	}
	if cands[0].Cmd[0] != "plugin/mongo-sidecar.exe" {
		t.Fatalf("cmd 应是相对写法，得到 %v", cands[0].Cmd)
	}
}

// TestRenderExtensionDraftVerifiedIsActive —— 探测通过才生成**活**条目，且不带手抄 methods/schemas。
func TestRenderExtensionDraftVerifiedIsActive(t *testing.T) {
	got := renderExtensionDraft(sidecarCandidate{
		Name: "ext.mongo",
		File: "mongo-sidecar.exe",
		Cmd:  []string{"plugin/mongo-sidecar.exe"},
	}, 29, true, "", newPortBook(""))

	for _, want := range []string{
		`cmd: ["plugin/mongo-sidecar.exe"]`,
		"describe: true",
		"自描述 29 方法",
		// 常驻声明不能漏：只留 cmd + describe 会退化成「每次调用重启侧车」，且不报错。
		"service: true",
		`listen: "127.0.0.1:8100"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("草稿缺少 %q：\n%s", want, got)
		}
	}
	if !strings.HasPrefix(got, "ext.mongo:\n") {
		t.Errorf("草稿应以「顶格的 ext.mongo:」开头（前面有注释就成了无效条目）：\n%s", got)
	}
	if strings.Contains(got, "methods:") || strings.Contains(got, "schemas:") {
		t.Errorf("开了 describe 就不该再手抄 methods/schemas（两份描述必漂）：\n%s", got)
	}
}

// TestPortBookAvoidsOccupiedPorts —— 建议端口必须避开 yaml 里已有的 listen，且多条草稿互不重复。
func TestPortBookAvoidsOccupiedPorts(t *testing.T) {
	book := newPortBook(`
ext.a:
  listen: "127.0.0.1:8100"
ext.b:
  listen: "127.0.0.1:8101"
`)
	if got := book.pick(); got != 8102 {
		t.Fatalf("应跳过已占用的 8100/8101，得到 %d", got)
	}
	if got := book.pick(); got != 8103 {
		t.Fatalf("第二条草稿不该复用同一端口，得到 %d", got)
	}
	if got := newPortBook("").pick(); got != scanPortBase {
		t.Fatalf("没有现有条目时应从 %d 起，得到 %d", scanPortBase, got)
	}
}

// TestRenderExtensionDraftUnverifiedIsFullyCommented —— 没验证过的条目必须是**全注释**。
//
// 这是本命令最要紧的一条：半激活条目（有 cmd、没有 methods/schemas）贴进 yaml 后，
// 启动期会按「describe: true 但探测失败」或「缺 schemas」fail-closed，把人挡在门外。
func TestRenderExtensionDraftUnverifiedIsFullyCommented(t *testing.T) {
	cases := []struct {
		name string
		perr string
	}{
		{"探测失败", "侧车未输出自描述 JSON"},
		{"未探测", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := renderExtensionDraft(sidecarCandidate{
				Name: "ext.mqtt",
				File: "mqtt-sidecar.exe",
				Cmd:  []string{"plugin/mqtt-sidecar.exe"},
			}, 0, false, c.perr, newPortBook(""))
			for _, line := range strings.Split(strings.TrimRight(got, "\n"), "\n") {
				if line != "" && !strings.HasPrefix(line, "#") {
					t.Fatalf("未验证的草稿出现非注释行 %q（贴进 yaml 会变成活条目）：\n%s", line, got)
				}
			}
			if !strings.Contains(got, "# ext.mqtt:") {
				t.Errorf("注释 stub 应给出条目骨架：\n%s", got)
			}
		})
	}
}

// TestRunExtScanReportsAndDraftsByDeclarationState —— 一次扫到「已登记（自描述）」与「未登记」两种。
func TestRunExtScanReportsAndDraftsByDeclarationState(t *testing.T) {
	root, _ := scanFixture(t, []string{"mongo-sidecar.exe", "mqtt-sidecar.exe"}, `
ext.mongo:
  cmd: ["plugin/mongo-sidecar.exe"]
  describe: true
`)
	stubProbe(t, func(cmd []string) (describe.Spec, error) {
		if strings.Contains(filepath.Base(cmd[0]), "mongo") {
			return describe.Spec{Methods: []string{"ping", "find"}}, nil
		}
		return describe.Spec{}, os.ErrDeadlineExceeded
	})

	var out bytes.Buffer
	if err := runExtScan(&out, extScanOptions{Dirs: []string{root}}); err != nil {
		t.Fatalf("扫描应成功: %v", err)
	}
	text := out.String()
	for _, want := range []string{
		"命中侧车 2 个（已登记 1 / 未登记 1）",
		"已登记 · 自描述 2 方法",
		"未登记 · 侧车不响应 --describe → 只出注释 stub",
		"# ext.mqtt:",
		"待确认的草稿 1 条",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("报告缺少 %q：\n%s", want, text)
		}
	}
	if strings.Contains(text, "\next.mqtt:\n") {
		t.Errorf("未验证的侧车不该生成活条目：\n%s", text)
	}
}

// TestRunExtScanVerifiedDraftLoadsAsExtensionConfig —— 草稿必须是**能被内核吃下**的 yaml。
//
// 只断言「文本里有 describe: true」不够：缩进/引号写错时人眼看不出，粘进 yaml 才炸。
// 这里把草稿真的喂回 LoadExtensionConfig 验一遍形状。
func TestRunExtScanVerifiedDraftLoadsAsExtensionConfig(t *testing.T) {
	root, _ := scanFixture(t, []string{"mqtt-sidecar.exe"}, "ext.other:\n  cmd: [\"gear.exe\", \"ext\", \"email\"]\n  methods: [send]\n  schemas:\n    send: {description: x}\n")
	stubProbe(t, func([]string) (describe.Spec, error) {
		return describe.Spec{Methods: []string{"publish", "subscribe"}}, nil
	})

	outPath := filepath.Join(t.TempDir(), "draft.yaml")
	var out bytes.Buffer
	if err := runExtScan(&out, extScanOptions{Dirs: []string{root}, Out: outPath}); err != nil {
		t.Fatalf("扫描应成功: %v", err)
	}
	if !strings.Contains(out.String(), "未登记 · 自描述 2 方法 → 已出草稿") {
		t.Fatalf("应生成草稿：\n%s", out.String())
	}

	specs, err := engine.LoadExtensionConfig(outPath)
	if err != nil {
		t.Fatalf("草稿应能被解析成扩展配置: %v", err)
	}
	spec, ok := specs["ext.mqtt"]
	if !ok {
		t.Fatalf("草稿里应有 ext.mqtt，得到 %v", specs)
	}
	if !spec.Describe {
		t.Error("草稿的 ext.mqtt 应声明 describe: true")
	}
	if !spec.Service || spec.Listen == "" {
		t.Errorf("草稿应声明常驻（service + listen），得到 service=%v listen=%q", spec.Service, spec.Listen)
	}
	if len(spec.Cmd) == 0 || filepath.Base(spec.Cmd[0]) != "mqtt-sidecar.exe" {
		t.Errorf("草稿的 cmd 应指向侧车二进制，得到 %v", spec.Cmd)
	}
}

// TestRunExtScanCheckFailsOnUndeclared —— --check 给 CI / 闸门用：有未登记就退出码 1。
func TestRunExtScanCheckFailsOnUndeclared(t *testing.T) {
	root, _ := scanFixture(t, []string{"mqtt-sidecar.exe"}, "ext.other:\n  cmd: [\"gear.exe\"]\n  methods: [m]\n  schemas:\n    m: {description: x}\n")
	stubProbe(t, func([]string) (describe.Spec, error) { return describe.Spec{Methods: []string{"m"}}, nil })

	var out bytes.Buffer
	err := runExtScan(&out, extScanOptions{Dirs: []string{root}, Check: true})
	if err == nil || !strings.Contains(err.Error(), "ext.mqtt") {
		t.Fatalf("--check 应在有未登记侧车时报错并点名，得到 %v", err)
	}
}

// TestRunExtScanOutRefusesSilentOverwrite —— 草稿落盘不许悄悄覆盖已存在的文件。
func TestRunExtScanOutRefusesSilentOverwrite(t *testing.T) {
	root, _ := scanFixture(t, []string{"mqtt-sidecar.exe"}, "ext.other:\n  cmd: [\"gear.exe\"]\n  methods: [m]\n  schemas:\n    m: {description: x}\n")
	stubProbe(t, func([]string) (describe.Spec, error) { return describe.Spec{Methods: []string{"m"}}, nil })

	outPath := filepath.Join(t.TempDir(), "draft.yaml")
	if err := os.WriteFile(outPath, []byte("原有内容\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := runExtScan(&out, extScanOptions{Dirs: []string{root}, Out: outPath})
	if err == nil || !strings.Contains(err.Error(), "已存在") {
		t.Fatalf("已存在的 --out 目标应拒绝覆盖，得到 %v", err)
	}
	raw, _ := os.ReadFile(outPath)
	if string(raw) != "原有内容\n" {
		t.Fatalf("拒绝覆盖时不该动原文件，实际: %q", string(raw))
	}
}

// TestRunExtScanNoProbeTouchesNoBinary —— --no-probe 下不许调用探针（纯静态对账）。
func TestRunExtScanNoProbeTouchesNoBinary(t *testing.T) {
	root, _ := scanFixture(t, []string{"mqtt-sidecar.exe"}, "ext.other:\n  cmd: [\"gear.exe\"]\n  methods: [m]\n  schemas:\n    m: {description: x}\n")
	probed := 0
	stubProbe(t, func([]string) (describe.Spec, error) {
		probed++
		return describe.Spec{Methods: []string{"m"}}, nil
	})

	var out bytes.Buffer
	if err := runExtScan(&out, extScanOptions{Dirs: []string{root}, NoProbe: true}); err != nil {
		t.Fatalf("扫描应成功: %v", err)
	}
	if probed != 0 {
		t.Fatalf("--no-probe 下不应执行任何探针，实际执行了 %d 次", probed)
	}
	if !strings.Contains(out.String(), "未探测（--no-probe）") {
		t.Errorf("报告应说明未探测：\n%s", out.String())
	}
	if !strings.Contains(out.String(), "# ext.mqtt:") {
		t.Errorf("--no-probe 仍应给出注释 stub：\n%s", out.String())
	}
}

// TestExtScanSubcommandIsRegistered —— `gear ext scan` 挂得上（子命令挂在 ext 下，别漏 AddCommand）。
func TestExtScanSubcommandIsRegistered(t *testing.T) {
	if err := runCLI(t, "ext", "scan", "--no-probe", "--dir", t.TempDir()); err != nil {
		t.Fatalf("gear ext scan 应可执行: %v", err)
	}
}

// scanFixture 造一个「侧车目录 + 扩展配置」，并把 GEAR_EXT_CONFIG 指过去。
func scanFixture(t *testing.T, files []string, cfgYAML string) (root string, cfgPath string) {
	t.Helper()
	base := t.TempDir()
	root = filepath.Join(base, "plugin")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(root, f), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfgPath = filepath.Join(base, "extensions.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfgYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GEAR_EXT_CONFIG", cfgPath)
	return root, cfgPath
}
