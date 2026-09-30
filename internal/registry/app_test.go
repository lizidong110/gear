package registry

import (
	"errors"
	"strings"
	"testing"

	"gear/internal/model"
)

// 2026-09-26：App 清单入 registry（此前只活在 MCP 层的 appManifest 且落库即丢弃）。
// 后果是「用户构建的产品」在 gear 里没有对应对象 ⇒ 没有导出、没有组成清单、没有可交付物。
// 这组测试锁死 apps 表的 CRUD 与注册期校验，是 gear_export / gear_import 的地基。

func validApp(id string) *model.App {
	return &model.App{
		ID:        id,
		Name:      id + " 应用",
		EntryFlow: id + ".up",
		Layout: model.AppLayout{
			Workdir: "apps/" + id,
			Files: []model.AppFile{
				{Path: "apps/" + id + "/www/index.html", Required: true},
				{Path: "apps/" + id + "/www/app.js", Required: false},
			},
		},
		Start:    model.AppStart{Restart: "on-failure", Ports: []int{8090}},
		Requires: model.AppRequires{Secrets: []string{"API_KEY"}, Capabilities: []string{"db"}},
	}
}

// T-reg-app-01：UpsertApp/GetApp/ListApps 落库往返 + 嵌套结构完整。
//
// 嵌套（Layout.Files / Start.Ports / Requires.*）是最容易在 JSON blob 里丢的部分：
// 旧形态是匿名嵌套 struct，新版换成具名类型后结构变了但 tag 没变，这里守住等价性。
func TestAppRoundtrip(t *testing.T) {
	reg := mustOpen(t)
	defer reg.Close()

	in := validApp("demo")
	if err := reg.UpsertApp(in); err != nil {
		t.Fatalf("UpsertApp 失败: %v", err)
	}

	got, err := reg.GetApp("demo")
	if err != nil {
		t.Fatalf("GetApp 失败: %v", err)
	}
	if got == nil {
		t.Fatal("GetApp 返回 nil")
	}
	if got.ID != "demo" || got.EntryFlow != "demo.up" {
		t.Fatalf("顶层字段丢失: %+v", got)
	}
	if len(got.Layout.Files) != 2 || got.Layout.Files[0].Path != "apps/demo/www/index.html" || !got.Layout.Files[0].Required {
		t.Fatalf("Layout.Files blob 不完整: %+v", got.Layout.Files)
	}
	if len(got.Start.Ports) != 1 || got.Start.Ports[0] != 8090 {
		t.Fatalf("Start.Ports blob 不完整: %+v", got.Start.Ports)
	}
	if len(got.Requires.Secrets) != 1 || got.Requires.Secrets[0] != "API_KEY" {
		t.Fatalf("Requires blob 不完整: %+v", got.Requires)
	}

	all, err := reg.ListApps()
	if err != nil {
		t.Fatalf("ListApps 失败: %v", err)
	}
	if len(all) != 1 || all[0].ID != "demo" {
		t.Fatalf("ListApps 结果不符: %+v", all)
	}

	// 幂等覆盖：同 id 再写一次不应产生第二条。
	if err := reg.UpsertApp(validApp("demo")); err != nil {
		t.Fatalf("UpsertApp 覆盖失败: %v", err)
	}
	all2, err := reg.ListApps()
	if err != nil {
		t.Fatalf("ListApps 失败: %v", err)
	}
	if len(all2) != 1 {
		t.Fatalf("同 id 覆盖应仍是 1 条，实际 %d 条", len(all2))
	}
}

// T-reg-app-02：GetApp 对不存在的 id 返回 (nil, nil)，与 GetTool 同口径
// ——「没有」不是错误，上层据此区分「不存在」与「读库失败」。
func TestAppGetMissingReturnsNil(t *testing.T) {
	reg := mustOpen(t)
	defer reg.Close()

	got, err := reg.GetApp("nope")
	if err != nil {
		t.Fatalf("GetApp 不该报错: %v", err)
	}
	if got != nil {
		t.Fatalf("不存在的 App 应返回 nil，实际 %+v", got)
	}
}

// T-reg-app-05：DeleteFlow 拒绝注销「某个 App 的 entry_flow」（§8.2 第 4 条守卫）。
//
// 这条守卫的形质与前三条不同：前三条是「别人还在用我」，这条是「删了我之后有人会悬空」。
// App.EntryFlow 只是个字符串，apps 与 flows 两表之间没有外键，注销 Flow 不留任何痕迹——
// App 定义会指向不存在的 Flow，直到 `app start` 才炸，而那时已看不出是哪次操作造成的。
func TestDeleteFlowRejectedWhenItIsAppEntryFlow(t *testing.T) {
	reg := mustOpen(t)
	defer reg.Close()

	entry := &model.Flow{ID: "demo.up", Name: "入口", Steps: []model.Step{{ID: "s1", Call: "noop"}}}
	if err := reg.UpsertFlow(entry); err != nil {
		t.Fatalf("UpsertFlow 失败: %v", err)
	}
	if err := reg.UpsertApp(validApp("demo")); err != nil { // EntryFlow = demo.up
		t.Fatalf("UpsertApp 失败: %v", err)
	}

	// 1) 是某个 App 的入口 → 拒，且报错必须点名是哪个 App（否则人不知道该动谁）
	err := reg.DeleteFlow("demo.up")
	if !errors.Is(err, ErrInUse) {
		t.Fatalf("App 的 entry_flow 必须拒绝注销，实际: %v", err)
	}
	if !strings.Contains(err.Error(), "demo") {
		t.Fatalf("报错必须点名引用它的 App: %v", err)
	}
	if f, _ := reg.GetFlow("demo.up"); f == nil {
		t.Fatal("被拒的删除不能真的把 Flow 删掉")
	}

	// 2) 守卫不能过宽：与该 App 无关的 Flow 照常可删
	if err := reg.UpsertFlow(&model.Flow{ID: "other", Steps: []model.Step{{ID: "s1", Call: "noop"}}}); err != nil {
		t.Fatalf("UpsertFlow other 失败: %v", err)
	}
	if err := reg.DeleteFlow("other"); err != nil {
		t.Fatalf("与 App 无关的 Flow 应可注销: %v", err)
	}

	// 3) 摘掉引用者（注销该 App）后可删——清理路径必须走得通，不能把自己锁死
	if err := reg.DeleteApp("demo"); err != nil {
		t.Fatalf("DeleteApp 失败: %v", err)
	}
	if err := reg.DeleteFlow("demo.up"); err != nil {
		t.Fatalf("App 注销后 entry_flow 应可注销: %v", err)
	}
	if f, _ := reg.GetFlow("demo.up"); f != nil {
		t.Fatal("删除后不应再读到")
	}
}
func TestAppRejectsInvalid(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*model.App)
		want string
	}{
		{"缺 id", func(a *model.App) { a.ID = "" }, "id"},
		{"id 含空白", func(a *model.App) { a.ID = "bad id" }, "空白"},
		{"缺 entry_flow", func(a *model.App) { a.EntryFlow = "" }, "entry_flow"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reg := mustOpen(t)
			defer reg.Close()
			a := validApp("demo")
			c.mut(a)
			err := reg.UpsertApp(a)
			if err == nil {
				t.Fatalf("应拒绝 %s，实际接受了", c.name)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("错误信息应点明 %q，实际: %v", c.want, err)
			}
			// 拒绝了就一定不能落库（不是「报了错但还是写进去」）。
			got, gerr := reg.GetApp(a.ID)
			if gerr == nil && got != nil {
				t.Fatalf("拒绝后仍落库: %+v", got)
			}
		})
	}
	// 对照组：合法清单必须能通过（否则上面三条可能是「全都拒」的假绿）。
	reg := mustOpen(t)
	defer reg.Close()
	if err := reg.UpsertApp(validApp("ok")); err != nil {
		t.Fatalf("合法 App 应通过，实际被拒: %v", err)
	}
}

// T-reg-app-04：DeleteApp 的语义——能删、删了能确认、删不存在的报 ErrNotFound。
//
// 刻意不做引用守卫（见 DeleteApp 注释）：App 只是聚合描述，加守卫会堵死清理路径。
// 这里锁死「不存在时报 ErrNotFound」这一条，避免上层把「打错 id」当成成功。
func TestAppDelete(t *testing.T) {
	reg := mustOpen(t)
	defer reg.Close()

	if err := reg.UpsertApp(validApp("demo")); err != nil {
		t.Fatalf("UpsertApp 失败: %v", err)
	}
	if err := reg.DeleteApp("demo"); err != nil {
		t.Fatalf("DeleteApp 失败: %v", err)
	}
	got, err := reg.GetApp("demo")
	if err != nil {
		t.Fatalf("GetApp 失败: %v", err)
	}
	if got != nil {
		t.Fatalf("删除后应查不到: %+v", got)
	}
	if err := reg.DeleteApp("demo"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("删不存在的 App 应报 ErrNotFound，实际: %v", err)
	}
}

// T-reg-app-05：apps 表与 tools/flows/hooks 互不干扰。
//
// 四张表同构且共用 upsert/get/list 原语，一次笔误（表名写错）会静默写进另一张表。
// 这条是「表名串台」的护栏。
func TestAppTableIsolate(t *testing.T) {
	reg := mustOpen(t)
	defer reg.Close()

	if err := reg.UpsertApp(validApp("demo")); err != nil {
		t.Fatalf("UpsertApp 失败: %v", err)
	}
	if err := reg.UpsertTool(validTool("demo")); err != nil {
		t.Fatalf("UpsertTool 失败: %v", err)
	}

	apps, err := reg.ListApps()
	if err != nil || len(apps) != 1 || apps[0].ID != "demo" {
		t.Fatalf("ListApps 应只见 App: %v / %+v", err, apps)
	}
	tools, err := reg.ListTools()
	if err != nil || len(tools) != 1 || tools[0].ID != "demo" {
		t.Fatalf("ListTools 应只见 Tool: %v / %+v", err, tools)
	}
	// 关键：id 相同但类型不同，取回来不能串。
	a, _ := reg.GetApp("demo")
	if a == nil || a.EntryFlow != "demo.up" {
		t.Fatalf("GetApp 取到了非 App 数据: %+v", a)
	}
	tl, _ := reg.GetTool("demo")
	if tl == nil || tl.Runtime != model.RuntimeProcess {
		t.Fatalf("GetTool 取到了非 Tool 数据: %+v", tl)
	}
}
