package doctor

// check_test.go —— Check 的**真实路径**测试（判据本身，不是报告渲染）。
//
// 为什么必须单独写：Map/Text 的形状好测（手拼一个 Report 就行），但 Check 里那四类判据
// ——扩展侧车齐不齐、AI 凭据有没配、端口被谁占着、台账里有没有遗留进程——恰恰是 doctor
// 存在的全部理由。此前只测渲染，等于「一个从不做体检的体检报告长什么样」测得很细。

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"gear/internal/app"
	"gear/internal/engine"
	"gear/internal/model"
	"gear/internal/registry"
)

// newCheckFixture 起一套「Check 能真跑」的环境：真 registry + 真引擎 + 真台账目录。
//
// 引擎的扩展配置指向一个空文件：这样 SetupExtensions 回退默认 ext.ai/ext.registry，
// 不受仓库里 plugin/extensions.yaml 里有几个侧车的影响——否则「缺侧车」这条断言
// 会随开发机状态变红。
func newCheckFixture(t *testing.T) (reg *registry.Registry, eng *engine.Engine, regPath, appsDir string) {
	t.Helper()
	dir := t.TempDir()
	regPath = filepath.Join(dir, "registry.db")
	appsDir = filepath.Join(dir, "apps")

	r, err := registry.Open(regPath)
	if err != nil {
		t.Fatalf("打开 registry 失败: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })

	// 空扩展配置：显式传路径绕开「读仓库里那份真配置」。
	emptyExt := filepath.Join(dir, "extensions.empty.yaml")
	if err := os.WriteFile(emptyExt, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	capReg := engine.NewCapabilityRegistry()
	agent, err := engine.SetupExtensions(capReg, r, emptyExt)
	if err != nil {
		t.Fatalf("初始化扩展失败: %v", err)
	}
	return r, engine.New(r, engine.WithCapabilities(capReg), engine.WithAIAgent(agent)), regPath, appsDir
}

// freePort 返回一个当前空闲的端口（拿到即释放；用于「这台机器上没人听」）。
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// TestCheckOnCleanEnvReportsZeroAndNoWarnings 是「一切正常」这条路径：
// 空 registry + 空台账 + AI 配了 key ⇒ 对象数为 0、空清单是 []（不是 null）、无告警。
func TestCheckOnCleanEnvReportsZeroAndNoWarnings(t *testing.T) {
	t.Setenv("GEAR_AI_API_KEY", "sk-test-not-a-real-key")
	reg, eng, regPath, appsDir := newCheckFixture(t)

	rep, err := Check(Options{Registry: reg, Engine: eng, RegistryPath: regPath, AppsDir: appsDir})
	if err != nil {
		t.Fatalf("Check 失败: %v", err)
	}
	if rep.Registry.Tools != 0 || rep.Registry.Flows != 0 || rep.Registry.Hooks != 0 {
		t.Fatalf("空库应全 0，实际 %+v", rep.Registry)
	}
	if rep.Apps == nil || len(rep.Apps) != 0 {
		t.Fatalf("Apps 应为空切片而非 nil: %#v", rep.Apps)
	}
	if rep.Orphan == nil || len(rep.Orphan) != 0 {
		t.Fatalf("Orphan 应为空切片而非 nil: %#v", rep.Orphan)
	}
	if rep.AppsDir != appsDir {
		t.Fatalf("apps_dir 应报解析后的真实目录 %s，实际 %s", appsDir, rep.AppsDir)
	}
	for _, w := range rep.Warnings {
		if strings.Contains(w, "AI 能力未就绪") {
			t.Fatalf("已配 key 却报 AI 未就绪: %v", rep.Warnings)
		}
	}
	if len(rep.Ports) != 0 {
		t.Fatalf("空台账不该有端口占用: %v", rep.Ports)
	}
}

// TestCheckResolvesAppsDirFromRegistryPath：只给 registry 路径、不给 apps-dir 时，
// 报出的目录必须是 <registry 同目录>/apps（空路径等于让人自己猜，踩过）。
func TestCheckResolvesAppsDirFromRegistryPath(t *testing.T) {
	t.Setenv("GEAR_AI_API_KEY", "sk-test")
	reg, eng, regPath, _ := newCheckFixture(t)

	rep, err := Check(Options{Registry: reg, Engine: eng, RegistryPath: regPath})
	if err != nil {
		t.Fatal(err)
	}
	want := app.AppsDir(regPath)
	if rep.AppsDir != want {
		t.Fatalf("apps_dir=%s，期望 %s", rep.AppsDir, want)
	}
}

// TestCheckReportsOrphanAndPorts 覆盖台账这条判据的两种真实形态：
//   - 进程已死（pid=0）、端口已释放 → 遗留 pid-gone
//   - 进程已死、端口仍被占 → 遗留 port-still-listening，且 Ports 表里那个端口标 in-use
func TestCheckReportsOrphanAndPorts(t *testing.T) {
	t.Setenv("GEAR_AI_API_KEY", "sk-test")
	reg, eng, regPath, appsDir := newCheckFixture(t)

	held := freePort(t) // 立刻释放 = 空闲端口
	dead := freePort(t)
	l, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(held))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close() // 保持占用

	if err := app.SaveRecord(appsDir, &app.Record{
		AppID: "app_gone", FlowID: "a.up", PID: 0,
		Ports: []int{dead}, StartedAt: time.Now(), LogPath: filepath.Join(appsDir, "a.log"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := app.SaveRecord(appsDir, &app.Record{
		AppID: "app_leak", FlowID: "b.up", PID: 0,
		Ports: []int{held}, StartedAt: time.Now().Add(time.Second),
	}); err != nil {
		t.Fatal(err)
	}

	rep, err := Check(Options{Registry: reg, Engine: eng, RegistryPath: regPath, AppsDir: appsDir})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Apps) != 2 {
		t.Fatalf("应有 2 条台账，实际 %d: %#v", len(rep.Apps), rep.Apps)
	}
	if len(rep.Orphan) != 2 {
		t.Fatalf("两条记录进程都没了，应都算遗留: %#v", rep.Orphan)
	}
	reasons := map[string]string{}
	for _, o := range rep.Orphan {
		reasons[o.AppID] = o.Reason
	}
	if reasons["app_gone"] != "pid-gone" {
		t.Fatalf("app_gone 应为 pid-gone，实际 %q", reasons["app_gone"])
	}
	if reasons["app_leak"] != "port-still-listening" {
		t.Fatalf("app_leak 应为 port-still-listening，实际 %q", reasons["app_leak"])
	}
	if got := rep.Ports[strconv.Itoa(held)]; !strings.HasPrefix(got, "in-use") {
		t.Fatalf("被占端口应标 in-use，实际 %q", got)
	}
	if got := rep.Ports[strconv.Itoa(dead)]; got != "free" {
		t.Fatalf("空闲端口应标 free，实际 %q", got)
	}
	// 遗留必须**同时**进告警（不能只躺在结构化字段里等人自己看）。
	if !hasWarningAbout(rep.Warnings, "app_leak") {
		t.Fatalf("遗留应用应有告警: %v", rep.Warnings)
	}
}

// TestCheckReportsLedgerUnreadable：台账目录读不了时必须降级成告警而不是让整个 doctor 失败——
// 其余判据（对象计数、侧车、AI）仍然有价值，一处读不了就把整份报告丢掉是错的。
//
// 触发方式用「路径含非法字符」而不是「指到一个文件」：Windows 上 os.ReadDir 一个**普通文件**
// 返回的是 ErrNotExist，会被 ListRecords 当成「暂无应用」正常放过（那是对的口径，不是缺陷）。
func TestCheckReportsLedgerUnreadable(t *testing.T) {
	t.Setenv("GEAR_AI_API_KEY", "sk-test")
	reg, eng, regPath, _ := newCheckFixture(t)

	bad := filepath.Join(t.TempDir(), "bad*dir") // Windows 非法路径字符 ⇒ ReadDir 报真错
	rep, err := Check(Options{Registry: reg, Engine: eng, RegistryPath: regPath, AppsDir: bad})
	if err != nil {
		t.Fatalf("台账读不了时 Check 应降级告警，不该返回错误: %v", err)
	}
	if !hasWarningAbout(rep.Warnings, "读取应用台账失败") {
		t.Fatalf("应如实告警台账读不了: %v", rep.Warnings)
	}
}

// TestCheckWarnsWhenAINotReady：没配 key 时必须给出**可执行的**修法，
// 而不是只说「未就绪」——这正是 doctor 相对「跑一次看报错」的价值。
func TestCheckWarnsWhenAINotReady(t *testing.T) {
	t.Setenv("GEAR_AI_API_KEY", "")
	t.Setenv("GEAR_AI_PROVIDER", "siliconflow")
	reg, eng, regPath, appsDir := newCheckFixture(t)

	rep, err := Check(Options{Registry: reg, Engine: eng, RegistryPath: regPath, AppsDir: appsDir})
	if err != nil {
		t.Fatal(err)
	}
	if ready, _ := rep.AI["ready"].(bool); ready {
		t.Fatalf("没配 key 不该报就绪: %v", rep.AI)
	}
	w := warningAbout(rep.Warnings, "AI 能力未就绪")
	if w == "" {
		t.Fatalf("应有 AI 未就绪告警: %v", rep.Warnings)
	}
	if !strings.Contains(w, "gear config set ai.api_key") {
		t.Fatalf("告警应给出可复制的修法: %s", w)
	}
}

// TestCheckAIReadyWithMockProvider：provider=mock 是离线桩，**不需要** key 就算就绪。
func TestCheckAIReadyWithMockProvider(t *testing.T) {
	t.Setenv("GEAR_AI_API_KEY", "")
	t.Setenv("GEAR_AI_PROVIDER", "mock")
	reg, eng, regPath, appsDir := newCheckFixture(t)

	rep, err := Check(Options{Registry: reg, Engine: eng, RegistryPath: regPath, AppsDir: appsDir})
	if err != nil {
		t.Fatal(err)
	}
	if ready, _ := rep.AI["ready"].(bool); !ready {
		t.Fatalf("provider=mock 应报就绪: %v", rep.AI)
	}
	if hasWarningAbout(rep.Warnings, "AI 能力未就绪") {
		t.Fatalf("mock 不该告警: %v", rep.Warnings)
	}
}

// TestCheckCountsRegistryObjects：registry 里真放对象，计数必须如实（doctor 若报 0，
// 「对象没导进去」和「doctor 坏了」就分不开了）。
func TestCheckCountsRegistryObjects(t *testing.T) {
	t.Setenv("GEAR_AI_API_KEY", "sk-test")
	reg, eng, regPath, appsDir := newCheckFixture(t)

	if err := reg.UpsertTool(testTool()); err != nil {
		t.Fatalf("写 Tool 失败: %v", err)
	}
	rep, err := Check(Options{Registry: reg, Engine: eng, RegistryPath: regPath, AppsDir: appsDir})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Registry.Tools != 1 || rep.Registry.Flows != 0 || rep.Registry.Hooks != 0 {
		t.Fatalf("计数不对: %+v", rep.Registry)
	}
}

// TestCheckWithoutEngine：Engine 为 nil（装配失败/精简调用方）时不得 panic，
// 且 Extensions.Loaded 如实为空——不能因为没引擎就编出已加载能力。
func TestCheckWithoutEngine(t *testing.T) {
	t.Setenv("GEAR_AI_API_KEY", "sk-test")
	reg, _, regPath, appsDir := newCheckFixture(t)

	rep, err := Check(Options{Registry: reg, RegistryPath: regPath, AppsDir: appsDir})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Extensions.Loaded) != 0 {
		t.Fatalf("无引擎时 Loaded 应为空: %v", rep.Extensions.Loaded)
	}
}

// ---- 扩展声明 ↔ 实现一致性（Check 内的那一段）----

// TestCheckCapsMismatchWarns 用一个真起的 /caps 服务驱动「声明了但没实现」这条告警：
// 声明 m1，/caps 只回 m2 ⇒ state=mismatch 且告警点名缺哪个方法。
func TestCheckCapsMismatchWarns(t *testing.T) {
	dir := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/caps" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"result": map[string]any{"methods": []string{"m2"}},
		})
	}))
	defer srv.Close()

	cfg := filepath.Join(dir, "extensions.yaml")
	writeExtYAML(t, cfg, "ext.demo", srv.Listener.Addr().String(), []string{"m1"})

	reg, eng, regPath, appsDir := newCheckFixture(t)
	// 让 Check 用这份配置：ExtensionConfigPath 走 GEAR_EXT_CONFIG。
	t.Setenv("GEAR_EXT_CONFIG", cfg)
	_ = eng

	rep, err := Check(Options{Registry: reg, Engine: eng, RegistryPath: regPath, AppsDir: appsDir})
	if err != nil {
		t.Fatal(err)
	}
	var found *CapsIssue
	for i := range rep.Extensions.CapsCheck {
		if rep.Extensions.CapsCheck[i].Ext == "ext.demo" {
			found = &rep.Extensions.CapsCheck[i]
		}
	}
	if found == nil {
		t.Fatalf("caps_check 应含 ext.demo: %#v", rep.Extensions.CapsCheck)
	}
	if found.State != "mismatch" {
		t.Fatalf("state=%q，期望 mismatch", found.State)
	}
	if len(found.Missing) != 1 || found.Missing[0] != "m1" {
		t.Fatalf("Missing 应点名 m1: %#v", found.Missing)
	}
	if w := warningAbout(rep.Warnings, "ext.demo"); !strings.Contains(w, "未实现") {
		t.Fatalf("应有声明↔实现不一致告警: %v", rep.Warnings)
	}
	if len(rep.Extensions.Declared) != 1 || rep.Extensions.Declared[0] != "ext.demo" {
		t.Fatalf("Declared 应含 ext.demo: %v", rep.Extensions.Declared)
	}
}

// TestCheckCapsNotRunningIsNotAWarning：service ext 只在应用运行时才被拉起，
// 没起是常态而非问题——报了会把真问题淹掉（口径锁）。
func TestCheckCapsNotRunningIsNotAWarning(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "extensions.yaml")
	writeExtYAML(t, cfg, "ext.idle", "127.0.0.1:"+strconv.Itoa(freePort(t)), []string{"m1"})

	reg, eng, regPath, appsDir := newCheckFixture(t)
	t.Setenv("GEAR_EXT_CONFIG", cfg)

	rep, err := Check(Options{Registry: reg, Engine: eng, RegistryPath: regPath, AppsDir: appsDir})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Extensions.CapsCheck) != 1 || rep.Extensions.CapsCheck[0].State != "sidecar_not_running" {
		t.Fatalf("应报 sidecar_not_running: %#v", rep.Extensions.CapsCheck)
	}
	if w := warningAbout(rep.Warnings, "ext.idle"); w != "" {
		t.Fatalf("侧车没起不该告警: %s", w)
	}
}

// TestCheckCapsProbeFailedWarns：连上了但 /caps 不合法是真问题（service ext 必须实现 /caps），
// 必须告警，且与「没起」区分开。
func TestCheckCapsProbeFailedWarns(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	dir := t.TempDir()
	cfg := filepath.Join(dir, "extensions.yaml")
	writeExtYAML(t, cfg, "ext.bad", srv.Listener.Addr().String(), []string{"m1"})

	reg, eng, regPath, appsDir := newCheckFixture(t)
	t.Setenv("GEAR_EXT_CONFIG", cfg)

	rep, err := Check(Options{Registry: reg, Engine: eng, RegistryPath: regPath, AppsDir: appsDir})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Extensions.CapsCheck) != 1 || rep.Extensions.CapsCheck[0].State != "probe_failed" {
		t.Fatalf("应报 probe_failed: %#v", rep.Extensions.CapsCheck)
	}
	if w := warningAbout(rep.Warnings, "ext.bad"); !strings.Contains(w, "/caps") {
		t.Fatalf("应告警 /caps 探测失败: %v", rep.Warnings)
	}
}

// TestExtensionConfigPathEmptyWhenUnset：显式指向不存在的文件 ⇒ 报错 ⇒ 空串。
// doctor 只如实汇报「没找到」，不替装配期报错（那是更准确的地方）。
func TestExtensionConfigPathEmptyWhenUnset(t *testing.T) {
	t.Setenv("GEAR_EXT_CONFIG", filepath.Join(t.TempDir(), "nope.yaml"))
	if got := ExtensionConfigPath(); got != "" {
		t.Fatalf("配置不存在时应返回空串，实际 %q", got)
	}
}

// ---- 侧车产物判据 ----

// TestMissingSidecarsOnlyWhenSidecarShapeExists 锁住「不猜」这条口径：
// 产物目录里根本没有 sidecar 形态时，即使加载了 ext.x 也不报缺——穷举必然误报。
func TestMissingSidecarsOnlyWhenSidecarShapeExists(t *testing.T) {
	noSidecar := t.TempDir()
	if got := missingSidecarsIn([]string{noSidecar}, []string{"ext.email"}); len(got) != 0 {
		t.Fatalf("非侧车形态不该报缺: %#v", got)
	}
}

// TestMissingSidecarsReportsExpectedAndSearched：目录里确实有 sidecar 形态时，
// 报缺并**列出找过的目录**（只说「没找到」会让人去猜是不是没编译）。
func TestMissingSidecarsReportsExpectedAndSearched(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "other-sidecar.exe"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := missingSidecarsIn([]string{dir}, []string{"ext.email", "ext.ai", "ext.registry"})
	if len(got) != 1 || got[0].Ext != "ext.email" {
		t.Fatalf("只该报 ext.email（ai/registry 是进程内）: %#v", got)
	}
	if !strings.HasSuffix(got[0].Expected, "email-sidecar") {
		t.Fatalf("Expected 应指名产物名: %s", got[0].Expected)
	}
	if len(got[0].Searched) != 1 || got[0].Searched[0] != dir {
		t.Fatalf("Searched 应是实际找过的目录: %#v", got[0].Searched)
	}
}

// TestMissingSidecarsFindsExistingArtifact：产物在 → 不报缺（否则一台侧车齐全的机器
// 会被误报成缺侧车，这个误报曾真实发生过）。
func TestMissingSidecarsFindsExistingArtifact(t *testing.T) {
	dir := t.TempDir()
	name := "email-sidecar"
	if runtimeIsWindows() {
		name += ".exe"
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := missingSidecarsIn([]string{dir}, []string{"ext.email"}); len(got) != 0 {
		t.Fatalf("产物在就不该报缺: %#v", got)
	}
}

// ---- 工具 ----

// testTool 是最小合法 Tool（versions 必填，缺了 UpsertTool 会拒）。
func testTool() *model.Tool {
	return &model.Tool{
		ID: "t1", Name: "t1", Description: "t", Runtime: model.RuntimeProcess, Entry: "cat",
		InputSchema:  map[string]interface{}{"type": "object"},
		OutputSchema: map[string]interface{}{"type": "object"},
		Versions:     []model.ToolVersion{{Version: "1.0.0", State: model.StateStable, Note: "init"}},
	}
}

func runtimeIsWindows() bool { return os.PathSeparator == '\\' }

func warningAbout(warnings []string, substr string) string {
	for _, w := range warnings {
		if strings.Contains(w, substr) {
			return w
		}
	}
	return ""
}

func hasWarningAbout(warnings []string, substr string) bool {
	return warningAbout(warnings, substr) != ""
}

func writeExtYAML(t *testing.T, path, name, listen string, methods []string) {
	t.Helper()
	var b strings.Builder
	b.WriteString(name + ":\n")
	b.WriteString("  service: true\n")
	b.WriteString("  listen: " + strconv.Quote(listen) + "\n")
	b.WriteString("  cmd: [" + strconv.Quote("demo-sidecar") + "]\n")
	b.WriteString("  methods:\n")
	for _, m := range methods {
		b.WriteString("    - " + m + "\n")
	}
	b.WriteString("  schemas:\n")
	for _, m := range methods {
		b.WriteString("    " + m + ":\n      description: " + strconv.Quote("方法 "+m) + "\n")
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}
