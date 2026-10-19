package artifact

import (
	"archive/zip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gear/internal/engine"
	"gear/internal/model"
	"gear/internal/registry"
)

// errCode 取出错误的诊断码。
//
// ⚠ 不能直接 `strings.Contains(err.Error(), "XXX")`：Gear 的诊断码挂在**错误链上的 Diagnostic**
// 里，不在 err 的文本中（文本是给人看的 message）。智能体侧的 toolErrorFrom 就是靠这条链取码，
// 测试必须用同一条路径——否则「改了文案但码还在」这类回归测不出来。
func errCode(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		t.Fatal("期望报错，但 err 为 nil")
	}
	diags := engine.Diagnostics(err)
	if len(diags) == 0 {
		t.Fatalf("错误没有挂诊断（码丢失）: %v", err)
	}
	return diags[0].Code
}

// 本组测试锁死「产物」这条 L3 必经路径：用户构建的应用必须能**备份、换机器、交付**。
// 每条都对着一个真实后果写——不是「功能能用」，而是「导出的东西一定导得回来」。

func mustReg(t *testing.T) *registry.Registry {
	t.Helper()
	reg, err := registry.Open(filepath.Join(t.TempDir(), "registry.db"))
	if err != nil {
		t.Fatalf("Open registry 失败: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	return reg
}

func fxApp(id string, files ...string) *model.App {
	a := &model.App{
		ID:        id,
		Name:      id + " 应用",
		EntryFlow: id + ".up",
		Layout:    model.AppLayout{Workdir: "apps/" + id},
		Start:     model.AppStart{Restart: "always", Ports: []int{8090}},
	}
	if len(files) == 0 {
		files = []string{"apps/" + id + "/www/index.html"}
	}
	for _, f := range files {
		a.Layout.Files = append(a.Layout.Files, model.AppFile{Path: f, Required: true})
	}
	return a
}

func fxTool(id string) *model.Tool {
	return &model.Tool{
		ID:           id,
		Name:         id,
		Runtime:      model.RuntimeLua,
		Entry:        "return {}",
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
		Versions:     []model.ToolVersion{{Version: "1.0.0", State: model.StateStable}},
	}
}

func fxFlow(id, call string) *model.Flow {
	return &model.Flow{ID: id, Steps: []model.Step{{ID: "s1", Call: call}}}
}

func fxHook(id, action string) *model.Hook {
	return &model.Hook{ID: id, Stage: model.StageBeforeFlow, Target: "*", Action: action}
}

// seed 往 registry 里放一个完整应用（App + Tool + Flow），并把静态资源写到 root 下。
func seed(t *testing.T, reg *registry.Registry, root, id string) *model.App {
	t.Helper()
	a := fxApp(id)
	// 静态资源：按清单声明的路径真实落一份，导出才有东西可拷。
	p := filepath.Join(root, filepath.FromSlash(a.Layout.Files[0].Path))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("<html>"+id+"</html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	b := &Bundle{
		Apps:  []*model.App{a},
		Tools: []*model.Tool{fxTool(id + "__hello")},
		Flows: []*model.Flow{fxFlow(id+".up", id+"__hello")},
	}
	if _, _, _, err := ApplyBundle(reg, b, nil, true); err != nil {
		t.Fatalf("ApplyBundle 失败: %v", err)
	}
	return a
}

// ---- 导出 ----

// T-artifact-01：目录形态导出必须**自包含**——三份文件 + 静态资源都在产物里。
func TestExportDirIsSelfContained(t *testing.T) {
	reg := mustReg(t)
	root := t.TempDir()
	seed(t, reg, root, "demo")

	out := filepath.Join(t.TempDir(), "artifact")
	res, err := Export(reg, ExportOptions{AppID: "demo", Out: out, StaticRoot: root})
	if err != nil {
		t.Fatalf("Export 失败: %v", err)
	}
	if res.Format != "dir" || res.AppID != "demo" {
		t.Fatalf("导出结果不符: %+v", res)
	}
	for _, name := range []string{MetaFile, AppFile, BundleFile, "www/index.html"} {
		if _, err := os.Stat(filepath.Join(out, filepath.FromSlash(name))); err != nil {
			t.Fatalf("产物缺少 %s: %v", name, err)
		}
	}
	// 静态资源路径必须相对 layout.workdir（剥掉 apps/demo/ 前缀）——
	// 导出方的仓库摆放习惯不是应用的一部分。
	if len(res.Files) != 1 || res.Files[0] != "www/index.html" {
		t.Fatalf("静态资源路径未归一: %v", res.Files)
	}
	if len(res.Components.Tools) != 1 || res.Components.Tools[0] != "demo__hello" {
		t.Fatalf("组成清单缺 tool: %+v", res.Components)
	}
	if res.Components.EntryFlow != "demo.up" {
		t.Fatalf("entry_flow 丢失: %+v", res.Components)
	}
}

// T-artifact-02：zip 形态与目录形态内容等价，且能被导回（「发给别人」的路径）。
func TestExportZipRoundTrip(t *testing.T) {
	reg := mustReg(t)
	root := t.TempDir()
	seed(t, reg, root, "demo")

	out := filepath.Join(t.TempDir(), "demo") // 无扩展名 → 自动补 .gearapp
	res, err := Export(reg, ExportOptions{AppID: "demo", Out: out, Format: "zip", StaticRoot: root})
	if err != nil {
		t.Fatalf("Export(zip) 失败: %v", err)
	}
	if !strings.HasSuffix(res.Path, ZipExt) {
		t.Fatalf("zip 产物应自动补 %s: %s", ZipExt, res.Path)
	}

	// 导进一个**全新空库**：这条通过才叫「换机器可行」。
	dst := mustReg(t)
	imp, err := Import(dst, ImportOptions{Source: res.Path})
	if err != nil {
		t.Fatalf("Import(zip) 失败: %v", err)
	}
	if len(imp.Created) != 3 {
		t.Fatalf("应创建 app+tool+flow 三个对象: %v", imp.Created)
	}
	got, err := dst.GetTool("demo__hello")
	if err != nil || got == nil {
		t.Fatalf("导入后 Tool 不在库里: %v", err)
	}
	if got.Entry != "return {}" {
		t.Fatalf("导入后 Tool 内容不符: %+v", got)
	}
}

// T-artifact-03：目录形态导回（**含静态资源落地**），且 layout 被重写到落地位置。
func TestImportLandsStaticAndRewritesLayout(t *testing.T) {
	reg := mustReg(t)
	root := t.TempDir()
	seed(t, reg, root, "demo")

	out := filepath.Join(t.TempDir(), "artifact")
	if _, err := Export(reg, ExportOptions{AppID: "demo", Out: out, StaticRoot: root}); err != nil {
		t.Fatal(err)
	}

	dst := mustReg(t)
	appsDir := t.TempDir()
	imp, err := Import(dst, ImportOptions{Source: out, AppsDir: appsDir})
	if err != nil {
		t.Fatalf("Import 失败: %v", err)
	}
	if len(imp.StaticLanded) != 1 {
		t.Fatalf("静态资源未落地: %+v", imp)
	}
	landed := filepath.Join(appsDir, "demo", "www", "index.html")
	if _, err := os.Stat(landed); err != nil {
		t.Fatalf("静态资源没落在 %s: %v", landed, err)
	}
	a, err := dst.GetApp("demo")
	if err != nil || a == nil {
		t.Fatalf("导入后 App 不在库里: %v", err)
	}
	want := filepath.ToSlash(filepath.Join(appsDir, "demo")) + "/www/index.html"
	if len(a.Layout.Files) != 1 || a.Layout.Files[0].Path != want {
		t.Fatalf("layout 未重写到落地位置（应用会指向不存在的路径）: %+v", a.Layout.Files)
	}
}

// T-artifact-04：不传 AppsDir 时**不落地**静态资源，但要如实警告（gear 不持有静态资源）。
func TestImportWithoutAppsDirDoesNotLandStatic(t *testing.T) {
	reg := mustReg(t)
	root := t.TempDir()
	seed(t, reg, root, "demo")
	out := filepath.Join(t.TempDir(), "artifact")
	if _, err := Export(reg, ExportOptions{AppID: "demo", Out: out, StaticRoot: root}); err != nil {
		t.Fatal(err)
	}

	dst := mustReg(t)
	imp, err := Import(dst, ImportOptions{Source: out})
	if err != nil {
		t.Fatal(err)
	}
	if len(imp.StaticLanded) != 0 {
		t.Fatalf("未传 apps_dir 就不该落地: %v", imp.StaticLanded)
	}
	if len(imp.Warnings) == 0 {
		t.Fatal("未落地静态资源却没给警告——用户会以为已经还原好了")
	}
}

// T-artifact-05：dry_run 一个字节不写。
func TestImportDryRunWritesNothing(t *testing.T) {
	reg := mustReg(t)
	root := t.TempDir()
	seed(t, reg, root, "demo")
	out := filepath.Join(t.TempDir(), "artifact")
	if _, err := Export(reg, ExportOptions{AppID: "demo", Out: out, StaticRoot: root}); err != nil {
		t.Fatal(err)
	}

	dst := mustReg(t)
	imp, err := Import(dst, ImportOptions{Source: out, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(imp.Created) == 0 {
		t.Fatal("dry_run 应列出将要创建的对象")
	}
	if got, _ := dst.GetApp("demo"); got != nil {
		t.Fatal("dry_run 竟然写了库")
	}
}

// T-artifact-06：未传 overwrite 且内容不同 → 整批拒绝（与 gear_apply 同款原子语义）。
func TestImportConflictRequiresOverwrite(t *testing.T) {
	src := mustReg(t)
	root := t.TempDir()
	seed(t, src, root, "demo")
	out := filepath.Join(t.TempDir(), "artifact")
	if _, err := Export(src, ExportOptions{AppID: "demo", Out: out, StaticRoot: root}); err != nil {
		t.Fatal(err)
	}

	dst := mustReg(t)
	// 先放一个内容不同的同 id Tool 进去。
	other := fxTool("demo__hello")
	other.Entry = "return {x=1}"
	if _, _, _, err := ApplyBundle(dst, &Bundle{Tools: []*model.Tool{other}}, nil, true); err != nil {
		t.Fatal(err)
	}
	if _, err := Import(dst, ImportOptions{Source: out}); err == nil {
		t.Fatal("内容冲突且未传 overwrite 时应拒绝导入")
	} else if code := errCode(t, err); code != "APPLY_CONFLICT" {
		t.Fatalf("冲突应带 APPLY_CONFLICT 码，实际 %s: %v", code, err)
	}
	// 写了 overwrite 就该过。
	if _, err := Import(dst, ImportOptions{Source: out, Overwrite: true}); err != nil {
		t.Fatalf("overwrite 后应导入成功: %v", err)
	}
}

// T-artifact-07：格式版本不认识**直接拒**（不猜、不静默降级）。
func TestImportRejectsUnsupportedVersion(t *testing.T) {
	reg := mustReg(t)
	root := t.TempDir()
	seed(t, reg, root, "demo")
	out := filepath.Join(t.TempDir(), "artifact")
	if _, err := Export(reg, ExportOptions{AppID: "demo", Out: out, StaticRoot: root}); err != nil {
		t.Fatal(err)
	}
	// 手改成未来版本：导入端必须拒，而不是「尽力导一半」。
	metaPath := filepath.Join(out, MetaFile)
	raw, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metaPath, []byte(strings.Replace(string(raw), "version: 1", "version: 99", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Import(mustReg(t), ImportOptions{Source: out}); err == nil {
		t.Fatal("不认识的格式版本应被拒绝")
	} else if code := errCode(t, err); code != "ARTIFACT_VERSION_UNSUPPORTED" {
		t.Fatalf("应带 ARTIFACT_VERSION_UNSUPPORTED 码，实际 %s: %v", code, err)
	}
}

// T-artifact-08：导出侧的入参校验（空 app_id / 空 out / 非法 format / app 不存在）。
func TestExportRejectsBadInput(t *testing.T) {
	reg := mustReg(t)
	cases := []struct {
		name string
		o    ExportOptions
		want string
	}{
		{"空 app_id", ExportOptions{Out: "x"}, "BAD_ARGUMENT"},
		{"空 out", ExportOptions{AppID: "demo"}, "BAD_ARGUMENT"},
		{"非法 format", ExportOptions{AppID: "demo", Out: "x", Format: "tar"}, "BAD_ARGUMENT"},
		{"app 不存在", ExportOptions{AppID: "nope", Out: "x"}, "NOT_FOUND"},
	}
	for _, c := range cases {
		if _, err := Export(reg, c.o); err == nil {
			t.Fatalf("%s：应报错", c.name)
		} else if code := errCode(t, err); code != c.want {
			t.Fatalf("%s：错误码应为 %s，实际 %s（%v）", c.name, c.want, code, err)
		}
	}
}

// T-artifact-09：dir 形态**拒绝覆盖非空目录**——为一个可重新生成的产物删未知目录，
// 代价与收益完全不对称。
func TestExportRefusesNonEmptyDir(t *testing.T) {
	reg := mustReg(t)
	root := t.TempDir()
	seed(t, reg, root, "demo")
	out := t.TempDir() // 非空（TempDir 本身存在但为空）→ 先塞一个文件
	if err := os.WriteFile(filepath.Join(out, "keep.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Export(reg, ExportOptions{AppID: "demo", Out: out, StaticRoot: root}); err == nil {
		t.Fatal("非空目录应被拒绝")
	}
	if _, err := os.Stat(filepath.Join(out, "keep.txt")); err != nil {
		t.Fatal("拒绝导出时不应动到目标目录里的东西")
	}
}

// T-artifact-10：静态资源在磁盘上找不到 → 导出仍成功，但**如实警告**（产物不完整）。
func TestExportWarnsOnMissingStatic(t *testing.T) {
	reg := mustReg(t)
	root := t.TempDir()
	seed(t, reg, root, "demo")
	// 把静态资源删掉，模拟「清单声明了但文件没了」。
	if err := os.RemoveAll(filepath.Join(root, "apps")); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "artifact")
	res, err := Export(reg, ExportOptions{AppID: "demo", Out: out, StaticRoot: root})
	if err != nil {
		t.Fatalf("静态资源缺失不应让导出失败: %v", err)
	}
	if len(res.Warnings) == 0 {
		t.Fatal("静态资源缺失却没警告——用户会以为产物是完整的")
	}
}

// ---- 落库（typed-nil 陷阱回归锁）----

// T-artifact-11：空库上 ApplyBundle 必须全部判为 created。
//
// 这是 typed-nil 陷阱的回归锁：`return reg.GetApp(id)`（返回 `*model.App`）赋给 `any`
// 时，未命中也是**非 nil 接口**，于是所有对象被误判「已存在」——created 永远为空、
// 冲突判定在空库上都会触发。不写这条，ApplyBundle 会静默做错事且不报错。
func TestApplyBundleOnEmptyRegistryCreatesAll(t *testing.T) {
	reg := mustReg(t)
	b := &Bundle{
		Apps:  []*model.App{fxApp("demo")},
		Tools: []*model.Tool{fxTool("demo__hello")},
		Flows: []*model.Flow{fxFlow("demo.up", "demo__hello")},
		Hooks: []*model.Hook{fxHook("demo__guard", "demo__hello")},
	}
	created, updated, _, err := ApplyBundle(reg, b, nil, false)
	if err != nil {
		t.Fatalf("ApplyBundle 失败: %v", err)
	}
	if len(updated) != 0 {
		t.Fatalf("空库上不该有 updated: %v", updated)
	}
	want := []string{"app:demo", "tool:demo__hello", "flow:demo.up", "hook:demo__guard"}
	if strings.Join(created, ",") != strings.Join(want, ",") {
		t.Fatalf("created 应为依赖顺序 %v，实际 %v", want, created)
	}
	// 再导一次（内容相同）→ 走 updated，不报冲突（幂等）。
	c2, u2, _, err := ApplyBundle(reg, b, nil, false)
	if err != nil {
		t.Fatalf("重复 ApplyBundle 应幂等: %v", err)
	}
	if len(c2) != 0 || len(u2) != len(want) {
		t.Fatalf("重复导入应全部判 updated: created=%v updated=%v", c2, u2)
	}
}

// T-artifact-12：DiffExisting 与 ApplyBundle 的分流必须一致（dry_run 不说谎）。
func TestDiffExistingMatchesApply(t *testing.T) {
	reg := mustReg(t)
	b := &Bundle{Apps: []*model.App{fxApp("demo")}, Tools: []*model.Tool{fxTool("demo__t")}}
	if got := DiffExisting(reg, b, false); len(got) != 2 {
		t.Fatalf("空库上应全部判新增: %v", got)
	}
	if got := DiffExisting(reg, b, true); len(got) != 0 {
		t.Fatalf("空库上不该有已存在: %v", got)
	}
	if _, _, _, err := ApplyBundle(reg, b, nil, true); err != nil {
		t.Fatal(err)
	}
	if got := DiffExisting(reg, b, true); len(got) != 2 {
		t.Fatalf("落库后应全部判已存在: %v", got)
	}
}

// ---- 解析与 zip 守卫 ----

// T-artifact-13：ParseBundle 的 kind 判定（未知 kind 明确报错，不猜成 Tool）。
func TestParseBundleClassifiesDocs(t *testing.T) {
	b, diags := ParseBundle("kind: App\nid: demo\nentry_flow: demo.up\n---\nkind: Tool\nid: demo__t\n")
	if len(diags) != 0 {
		t.Fatalf("不该有诊断: %+v", diags)
	}
	if len(b.Apps) != 1 || len(b.Tools) != 1 {
		t.Fatalf("分类不符: apps=%d tools=%d", len(b.Apps), len(b.Tools))
	}
	if _, diags := ParseBundle("kind: Widget\nid: x\n"); len(diags) == 0 {
		t.Fatal("未知 kind 应报错")
	}
}

// T-artifact-14：zip-slip 守卫——越界条目必须拒，不能解开到目标目录之外。
func TestUnzipRejectsPathTraversal(t *testing.T) {
	dir := t.TempDir()
	zpath := filepath.Join(dir, "evil.gearapp")
	f, err := os.Create(zpath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	w, err := zw.Create("../escaped.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()

	dst := filepath.Join(dir, "out")
	if err := unzip(zpath, dst); err == nil {
		t.Fatal("越界条目应被拒绝")
	}
	if _, err := os.Stat(filepath.Join(dir, "escaped.txt")); !os.IsNotExist(err) {
		t.Fatal("文件被写到了目标目录之外")
	}
}

// T-artifact-15：导入路径不存在 / 不是产物目录 → 明确报错（不静默返回空结果）。
func TestImportRejectsBadSource(t *testing.T) {
	if _, err := Import(mustReg(t), ImportOptions{Source: ""}); err == nil {
		t.Fatal("空 source 应报错")
	}
	if _, err := Import(mustReg(t), ImportOptions{Source: filepath.Join(t.TempDir(), "nope")}); err == nil {
		t.Fatal("不存在的路径应报错")
	}
	empty := t.TempDir()
	if _, err := Import(mustReg(t), ImportOptions{Source: empty}); err == nil {
		t.Fatal("不是产物的目录应报错")
	}
}

// T-artifact-16：staticRelPath 的规则（剥 workdir 前缀；无前缀时原样）。
func TestStaticRelPath(t *testing.T) {
	if got := staticRelPath(fxApp("demo"), "apps/demo/www/index.html"); got != "www/index.html" {
		t.Fatalf("应剥掉 workdir 前缀: %q", got)
	}
	if got := staticRelPath(fxApp("demo"), "www/index.html"); got != "www/index.html" {
		t.Fatalf("无前缀时原样: %q", got)
	}
}

// T-artifact-17：ParseBundle 的退化分类与坏文档（一次报全，不中断）。
func TestParseBundleEdgeCases(t *testing.T) {
	// 无 kind：有 steps 视为 Flow，有 stage 视为 Hook（兼容历史写法）。
	b, diags := ParseBundle("id: x\nsteps:\n  - id: s1\n    call: y\n---\nid: h\nstage: before_flow\ntarget: \"*\"\naction: y\n")
	if len(diags) != 0 {
		t.Fatalf("不该有诊断: %+v", diags)
	}
	if len(b.Flows) != 1 || len(b.Hooks) != 1 {
		t.Fatalf("退化分类不符: flows=%d hooks=%d", len(b.Flows), len(b.Hooks))
	}
	// 不是合法 YAML → BAD_YAML，且**继续**解析后续文档（一次报全）。
	if _, diags := ParseBundle("a: [1,\n"); len(diags) == 0 {
		t.Fatal("坏 YAML 应报诊断")
	}
	// kind 合法但字段类型不对 → 按对应类型解析失败的诊断（不猜成别的类型）。
	if _, diags := ParseBundle("kind: Tool\nid: [1,2]\n"); len(diags) == 0 {
		t.Fatal("字段类型不对应报诊断")
	}
}

// T-artifact-18：产物里的定义非法 → 拒导入（不把坏数据写进库）。
func TestImportRejectsInvalidDefinitions(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	meta := "kind: GearArtifact\nversion: 1\napp_id: demo\ncomponents:\n  entry_flow: demo.up\n  tools: []\n  flows: []\n  hooks: []\n  static_files: []\n"
	write(MetaFile, meta)
	write(AppFile, "kind: App\nid: demo\nentry_flow: demo.up\n")
	// 缺 versions → model.Tool.Validate 会拒。
	write(BundleFile, "kind: Tool\nid: demo__t\nruntime: lua\nentry: \"return {}\"\n")
	if _, err := Import(mustReg(t), ImportOptions{Source: dir}); err == nil {
		t.Fatal("定义非法的产物应被拒绝")
	}

	// entry_flow 既不在产物里也没注册 → 拒（否则应用起来才炸）。
	write(BundleFile, "kind: Tool\nid: demo__t\nruntime: lua\nentry: \"return {}\"\ninput_schema: {type: object}\noutput_schema: {type: object}\nversions:\n  - version: 1.0.0\n    state: stable\n")
	if _, err := Import(mustReg(t), ImportOptions{Source: dir}); err == nil {
		t.Fatal("entry_flow 无法解析时应拒绝导入")
	}
}

// T-artifact-19：source 是文件但不是 zip → 明确报错（不静默当空产物）。
func TestImportRejectsNonZipFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.gearapp")
	if err := os.WriteFile(p, []byte("not a zip"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Import(mustReg(t), ImportOptions{Source: p}); err == nil {
		t.Fatal("不是 zip 的文件应报错")
	} else if code := errCode(t, err); code != "ARTIFACT_INVALID" {
		t.Fatalf("应带 ARTIFACT_INVALID 码，实际 %s", code)
	}
}

// T-artifact-20：NoStatic 只导定义，且**如实警告**产物不自包含。
func TestExportNoStaticOnlyDefinitions(t *testing.T) {
	reg := mustReg(t)
	root := t.TempDir()
	seed(t, reg, root, "demo")
	out := filepath.Join(t.TempDir(), "artifact")
	res, err := Export(reg, ExportOptions{AppID: "demo", Out: out, NoStatic: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Files) != 0 {
		t.Fatalf("no_static 时不该拷静态资源: %v", res.Files)
	}
	if len(res.Warnings) == 0 {
		t.Fatal("只导定义却没警告产物不自包含——用户会拿它当交付物")
	}
}

// T-artifact-21：layout.files 支持 `dir: true` 整目录递归导出（含子目录），产物自包含。
func TestExportDirEntryRecurses(t *testing.T) {
	reg := mustReg(t)
	root := t.TempDir()
	a := &model.App{
		ID: "demo", Name: "demo", EntryFlow: "demo.up",
		Layout: model.AppLayout{
			Workdir: "apps/demo",
			Files:   []model.AppFile{{Path: "apps/demo/www", Dir: true, Required: true}},
		},
		Start: model.AppStart{Restart: "always", Ports: []int{8090}},
	}
	www := filepath.Join(root, "apps/demo/www")
	if err := os.MkdirAll(filepath.Join(www, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(www, "index.html"), []byte("<html>demo</html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(www, "assets", "app.js"), []byte("console.log(1)"), 0o644); err != nil {
		t.Fatal(err)
	}
	b := &Bundle{
		Apps:  []*model.App{a},
		Tools: []*model.Tool{fxTool("demo__hello")},
		Flows: []*model.Flow{fxFlow("demo.up", "demo__hello")},
	}
	if _, _, _, err := ApplyBundle(reg, b, nil, true); err != nil {
		t.Fatalf("ApplyBundle 失败: %v", err)
	}

	out := filepath.Join(t.TempDir(), "artifact")
	res, err := Export(reg, ExportOptions{AppID: "demo", Out: out, StaticRoot: root})
	if err != nil {
		t.Fatalf("Export 失败: %v", err)
	}
	if len(res.Files) != 2 {
		t.Fatalf("dir 导出应递归包含 2 个文件，实际 %v", res.Files)
	}
	if _, err := os.Stat(filepath.Join(out, "www", "index.html")); err != nil {
		t.Fatalf("产物缺 www/index.html: %v", err)
	}
	if _, err := os.Stat(filepath.Join(out, "www", "assets", "app.js")); err != nil {
		t.Fatalf("产物缺 www/assets/app.js: %v", err)
	}
	// meta 里的组成清单必须是**展开后的逐文件**（import 端据此落地）。
	if len(res.Components.StaticFiles) != 2 {
		t.Fatalf("组成清单应展开为 2 个文件，实际 %v", res.Components.StaticFiles)
	}
}

// T-artifact-22：dir 声明的静态资源导入后整树落地，且 layout 重写为展开的逐文件路径。
func TestImportLandsDirAndRewritesLayout(t *testing.T) {
	reg := mustReg(t)
	root := t.TempDir()
	a := &model.App{
		ID: "demo", Name: "demo", EntryFlow: "demo.up",
		Layout: model.AppLayout{
			Workdir: "apps/demo",
			Files:   []model.AppFile{{Path: "apps/demo/www", Dir: true, Required: true}},
		},
		Start: model.AppStart{Restart: "always", Ports: []int{8090}},
	}
	www := filepath.Join(root, "apps/demo/www")
	if err := os.MkdirAll(filepath.Join(www, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(www, "index.html"), []byte("<html>demo</html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(www, "assets", "app.js"), []byte("console.log(1)"), 0o644); err != nil {
		t.Fatal(err)
	}
	b := &Bundle{
		Apps:  []*model.App{a},
		Tools: []*model.Tool{fxTool("demo__hello")},
		Flows: []*model.Flow{fxFlow("demo.up", "demo__hello")},
	}
	if _, _, _, err := ApplyBundle(reg, b, nil, true); err != nil {
		t.Fatalf("ApplyBundle 失败: %v", err)
	}

	out := filepath.Join(t.TempDir(), "artifact")
	if _, err := Export(reg, ExportOptions{AppID: "demo", Out: out, StaticRoot: root}); err != nil {
		t.Fatal(err)
	}

	dst := mustReg(t)
	appsDir := t.TempDir()
	imp, err := Import(dst, ImportOptions{Source: out, AppsDir: appsDir})
	if err != nil {
		t.Fatalf("Import 失败: %v", err)
	}
	if len(imp.StaticLanded) != 2 {
		t.Fatalf("dir 静态资源应落地 2 个文件，实际 %v", imp.StaticLanded)
	}
	if _, err := os.Stat(filepath.Join(appsDir, "demo", "www", "index.html")); err != nil {
		t.Fatalf("未落在 www/index.html: %v", err)
	}
	if _, err := os.Stat(filepath.Join(appsDir, "demo", "www", "assets", "app.js")); err != nil {
		t.Fatalf("未落在 www/assets/app.js: %v", err)
	}
	got, err := dst.GetApp("demo")
	if err != nil || got == nil {
		t.Fatalf("导入后 App 不在库里: %v", err)
	}
	want := filepath.ToSlash(filepath.Join(appsDir, "demo")) + "/www/index.html"
	found := false
	for _, f := range got.Layout.Files {
		if f.Path == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("layout 未重写为展开文件：%v", got.Layout.Files)
	}
	if got.Layout.Workdir != filepath.ToSlash(filepath.Join(appsDir, "demo")) {
		t.Fatalf("workdir 未重写：%q", got.Layout.Workdir)
	}
}

// T-artifact-23：未指定 --root 时，从 cwd **逐级向上**找能解析声明路径的目录。
//
// 真实场景：用户在仓库子目录 build/ 里跑 `gear export web_ui`，而清单里写的是相对
// 仓库根的 `build/dist`。死守 cwd 会拼成 build/build/dist 报「找不到」，而这正是用户
// 想要的导出。
func TestExportFindsStaticRootByWalkingUp(t *testing.T) {
	reg := mustReg(t)
	root := t.TempDir()
	a := &model.App{
		ID: "ui", Name: "ui", EntryFlow: "ui.up",
		Layout: model.AppLayout{
			Workdir: "build",
			Files:   []model.AppFile{{Path: "build/dist", Dir: true, Required: true}},
		},
	}
	p := filepath.Join(root, "build", "dist", "index.html")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("<html>ui</html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	b := &Bundle{
		Apps:  []*model.App{a},
		Tools: []*model.Tool{fxTool("ui__hello")},
		Flows: []*model.Flow{fxFlow("ui.up", "ui__hello")},
	}
	if _, _, _, err := ApplyBundle(reg, b, nil, true); err != nil {
		t.Fatalf("ApplyBundle 失败: %v", err)
	}

	// 从子目录里导出，且**不传** StaticRoot。
	t.Chdir(filepath.Join(root, "build"))
	out := filepath.Join(t.TempDir(), "artifact")
	res, err := Export(reg, ExportOptions{AppID: "ui", Out: out})
	if err != nil {
		t.Fatalf("Export 失败: %v", err)
	}
	if !res.StaticRootAuto {
		t.Fatal("未指定 --root 时应标记为自动解析")
	}
	if res.StaticRoot != root {
		t.Fatalf("静态根应自动上溯到仓库根 %s，实际 %s", root, res.StaticRoot)
	}
	if len(res.Files) != 1 || res.Files[0] != "dist/index.html" {
		t.Fatalf("静态资源应被找到并归一为 dist/index.html，实际 %v", res.Files)
	}
	if _, err := os.Stat(filepath.Join(out, "dist", "index.html")); err != nil {
		t.Fatalf("产物缺 dist/index.html: %v", err)
	}
	for _, w := range res.Warnings {
		if strings.Contains(w, "找不到") {
			t.Fatalf("自动上溯找到了资源，不该有「找不到」警告: %s", w)
		}
	}
}

// T-artifact-24：registry 里**只有 Flow 没有 App 清单**时，export 按入口 Flow 临时合成清单
// （不写库），并从 static_site.serve 推断静态资源——正是 wms/mindstorm 那种"只有 Flow"的应用。
func TestExportSynthesizesManifestFromFlow(t *testing.T) {
	reg := mustReg(t)
	root := t.TempDir()
	// 故意**不**放 Apps —— 只注册 Tool 与 Flow。
	flow := &model.Flow{
		ID: "wms.up", Name: "WMS 起服",
		Steps: []model.Step{
			{ID: "site", Call: "static_site.serve", Input: map[string]any{"dir": "apps/wms/www", "port": 8091}},
			{ID: "api", Call: "wms__hello", DependsOn: []string{"site"}},
		},
	}
	b := &Bundle{
		Tools: []*model.Tool{fxTool("wms__hello")},
		Flows: []*model.Flow{flow},
	}
	if _, _, _, err := ApplyBundle(reg, b, nil, true); err != nil {
		t.Fatalf("ApplyBundle 失败: %v", err)
	}
	if got, _ := reg.GetApp("wms"); got != nil {
		t.Fatal("前置条件：这个库里不该有 wms 的 App 清单")
	}
	www := filepath.Join(root, "apps/wms/www")
	if err := os.MkdirAll(filepath.Join(www, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(www, "index.html"), []byte("<html>wms</html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(www, "assets", "app.js"), []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(t.TempDir(), "artifact")
	res, err := Export(reg, ExportOptions{AppID: "wms", Out: out, StaticRoot: root})
	if err != nil {
		t.Fatalf("Export 失败（应能按 Flow 合成清单）: %v", err)
	}
	if res.Components.EntryFlow != "wms.up" {
		t.Fatalf("合成清单的 entry_flow 应为 wms.up，实际 %s", res.Components.EntryFlow)
	}
	// 静态资源由 flow 的 static_site.serve dir 推断：workdir=apps/wms ⇒ 产物内 www/。
	if len(res.Files) != 2 || res.Files[0] != "www/assets/app.js" || res.Files[1] != "www/index.html" {
		t.Fatalf("推断的静态资源不符，实际 %v", res.Files)
	}
	warned := false
	for _, w := range res.Warnings {
		if strings.Contains(w, "合成") {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("合成清单必须如实告知用户（这是推断的，不是他写的），实际 warnings=%v", res.Warnings)
	}
	// 合成清单**不写库**：导出是只读操作。
	if got, _ := reg.GetApp("wms"); got != nil {
		t.Fatal("export 不该把合成清单写进库（导出应只读）")
	}

	// 产物里的 app.yaml 就是这份合成清单 → 导回时才会真正落库。
	dst := mustReg(t)
	if _, err := Import(dst, ImportOptions{Source: out}); err != nil {
		t.Fatalf("Import 失败: %v", err)
	}
	got, err := dst.GetApp("wms")
	if err != nil || got == nil {
		t.Fatalf("导回后应有 wms 的 App 清单: %v", err)
	}
	if got.EntryFlow != "wms.up" {
		t.Fatalf("导回的 entry_flow 应为 wms.up，实际 %s", got.EntryFlow)
	}
}

// T-artifact-24b：短 id 与完整 Flow id 等价（`<id>.up` 是强约定，优先命中）；
// 没有 `<id>.up`、而多个 `<id>.*` 命中时**报错**而非猜一个。
func TestExportResolvesFlowIDAndRejectsAmbiguity(t *testing.T) {
	reg := mustReg(t)
	root := t.TempDir()
	b := &Bundle{
		Tools: []*model.Tool{fxTool("wms__hello")},
		Flows: []*model.Flow{
			fxFlow("wms.up", "wms__hello"),
			fxFlow("wms.extra", "wms__hello"),
			fxFlow("wm.alpha", "wms__hello"),
			fxFlow("wm.beta", "wms__hello"),
		},
	}
	if _, _, _, err := ApplyBundle(reg, b, nil, true); err != nil {
		t.Fatalf("ApplyBundle 失败: %v", err)
	}
	// 短 id `wms`：`<id>.up` 强约定优先 → 命中 wms.up（即便还有 wms.extra）。
	out := filepath.Join(t.TempDir(), "artifact")
	res, err := Export(reg, ExportOptions{AppID: "wms", Out: out, StaticRoot: root})
	if err != nil {
		t.Fatalf("短 id 应命中 wms.up: %v", err)
	}
	if res.Components.EntryFlow != "wms.up" {
		t.Fatalf("短 id 应命中 .up 约定，实际 %s", res.Components.EntryFlow)
	}
	// 完整 id 精确命中。
	if _, err := Export(reg, ExportOptions{AppID: "wms.extra", Out: filepath.Join(t.TempDir(), "e"), StaticRoot: root}); err != nil {
		t.Fatalf("完整 Flow id 应成功: %v", err)
	}
	// `wm`：没有 wm.up，wm.alpha / wm.beta 两个前缀命中 → 必须报 BAD_ARGUMENT（不猜）。
	if _, err := Export(reg, ExportOptions{AppID: "wm", Out: filepath.Join(t.TempDir(), "x"), StaticRoot: root}); err == nil {
		t.Fatal("前缀歧义时应报错")
	} else if code := errCode(t, err); code != "BAD_ARGUMENT" {
		t.Fatalf("歧义应报 BAD_ARGUMENT，实际 %s（%v）", code, err)
	}
}

// T-artifact-25：声明为**绝对路径**的静态资源与基准目录无关，原样解析（含 dir 展开）。
func TestExportAbsoluteStaticIgnoresRoot(t *testing.T) {
	reg := mustReg(t)
	root := t.TempDir()
	site := filepath.Join(root, "www") // 绝对路径
	if err := os.MkdirAll(site, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(site, "index.html"), []byte("<html>abs</html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := &model.App{
		ID: "abs", Name: "abs", EntryFlow: "abs.up",
		Layout: model.AppLayout{
			Workdir: filepath.ToSlash(root),
			Files:   []model.AppFile{{Path: filepath.ToSlash(site), Dir: true, Required: true}},
		},
	}
	b := &Bundle{
		Apps:  []*model.App{a},
		Tools: []*model.Tool{fxTool("abs__hello")},
		Flows: []*model.Flow{fxFlow("abs.up", "abs__hello")},
	}
	if _, _, _, err := ApplyBundle(reg, b, nil, true); err != nil {
		t.Fatalf("ApplyBundle 失败: %v", err)
	}

	out := filepath.Join(t.TempDir(), "artifact")
	// StaticRoot 指一个**无关**目录：绝对路径不该受它影响。
	res, err := Export(reg, ExportOptions{AppID: "abs", Out: out, StaticRoot: t.TempDir()})
	if err != nil {
		t.Fatalf("Export 失败: %v", err)
	}
	if len(res.Files) != 1 || res.Files[0] != "www/index.html" {
		t.Fatalf("绝对 dir 应被解析并归一为 www/index.html，实际 %v", res.Files)
	}
	if _, err := os.Stat(filepath.Join(out, "www", "index.html")); err != nil {
		t.Fatalf("产物缺 www/index.html: %v", err)
	}
}
