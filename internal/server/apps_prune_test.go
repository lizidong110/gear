package server

// apps_prune_test.go：锁住 POST /api/apps/prune —— 让「清僵尸台账」在 REST 面也能做。
//
// 为什么值得专测：这个端点横跨**两个不同模型**——同前缀的 GET/POST/DELETE /api/apps 管的是
// 本进程**内存**里的 AppInstance（进程一退即消失，既没有台账也没有孤儿），而本端点动的是
// `gear app start` 落下的**落盘台账**。若有人「顺手统一」成内存模型，或让 Server 退回
// gearhome 默认路径去清另一个库的台账，这里的断言必须红。
//
// 同时锁住一条表达纪律：保留项的 `state`（结案状态）与 `probe_state`（实时判据）必须**分得开**。
// 只报一个字段就会输出「state: startup_failed 却 reason: 仍在运行」这种自相矛盾的话。

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gear/internal/app"
	"gear/internal/registry"
)

// newPruneServer 造一套隔离的 registry + 台账目录，并把两者注入 Server。
func newPruneServer(t *testing.T) (s *Server, appsDir string) {
	t.Helper()
	regPath := filepath.Join(t.TempDir(), "registry.db")
	reg, err := registry.Open(regPath)
	if err != nil {
		t.Fatalf("打开 registry 失败: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	dir := filepath.Join(t.TempDir(), "apps")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	srv, err := New(reg, WithRegistryPath(regPath), WithAppsDir(dir))
	if err != nil {
		t.Fatalf("构造 Server 失败: %v", err)
	}
	return srv, dir
}

// pruneList 取出响应里的 pruned / skipped 列表。
func pruneList(t *testing.T, out map[string]interface{}, key string) []map[string]interface{} {
	t.Helper()
	raw, ok := out[key].([]interface{})
	if !ok {
		t.Fatalf("响应缺少 %s 列表（不能是 null）: %v", key, out)
	}
	items := make([]map[string]interface{}, 0, len(raw))
	for _, v := range raw {
		m, ok := v.(map[string]interface{})
		if !ok {
			t.Fatalf("%s 的元素不是对象: %v", key, v)
		}
		items = append(items, m)
	}
	return items
}

func TestPruneAppsEndpoint(t *testing.T) {
	s, dir := newPruneServer(t)

	// ① 确定已终止：无端口 ⇒ pid 单判据即可定案（pid=0 必判死）。
	if err := app.SaveRecord(dir, &app.Record{
		AppID: "app_dead", FlowID: "x.up", StartedAt: time.Now(),
		FinalState: string(app.StateStartupFailed),
	}); err != nil {
		t.Fatal(err)
	}

	// ② 结案了、但**实时判据不敢删**：pid 已死而端口仍被占（残留孙进程攥着端口）⇒ probe=unknown。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	heldPort := ln.Addr().(*net.TCPAddr).Port
	if err := app.SaveRecord(dir, &app.Record{
		AppID: "app_ambig", FlowID: "x.up", Ports: []int{heldPort}, StartedAt: time.Now(),
		FinalState: string(app.StateStartupFailed),
	}); err != nil {
		t.Fatal(err)
	}

	// ③ dry_run：只汇报，不动磁盘。
	rec, out := do(t, s.Handler(), "POST", "/api/apps/prune", map[string]interface{}{"dry_run": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("prune(dry_run) 应 200，实际 %d body=%s", rec.Code, rec.Body.String())
	}
	if out["dry_run"] != true || out["pruned_count"].(float64) != 1 || out["skipped_count"].(float64) != 1 {
		t.Fatalf("dry_run 计数不符: %v", out)
	}
	// 必须回显**注入的**台账目录：若退回去取 gearhome 默认值，这里立刻红。
	if out["apps_dir"] != dir {
		t.Fatalf("apps_dir 应为注入的目录 %q，实际 %v", dir, out["apps_dir"])
	}
	skipped := pruneList(t, out, "skipped")
	if skipped[0]["state"] != string(app.StateStartupFailed) ||
		skipped[0]["probe_state"] != string(app.StateUnknown) {
		t.Fatalf("保留项应把「结案状态」与「实时判据」分开报，实际 %v", skipped[0])
	}
	for _, id := range []string{"app_dead", "app_ambig"} {
		if _, err := app.LoadRecord(dir, id); err != nil {
			t.Fatalf("dry_run 不得动磁盘，%s 应还在: %v", id, err)
		}
	}

	// ④ 真删：终态那条被清，实时判据不敢确定的那条留下。
	rec, out = do(t, s.Handler(), "POST", "/api/apps/prune", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("prune 应 200，实际 %d body=%s", rec.Code, rec.Body.String())
	}
	if out["dry_run"] != false || out["pruned_count"].(float64) != 1 {
		t.Fatalf("实际清理计数不符: %v", out)
	}
	if _, err := app.LoadRecord(dir, "app_dead"); err == nil {
		t.Fatal("终态记录应已从磁盘删除")
	}
	if _, err := app.LoadRecord(dir, "app_ambig"); err != nil {
		t.Fatalf("实时判据 unknown 的记录必须保留: %v", err)
	}
}

func TestPruneAppsFlowFilterAndGuards(t *testing.T) {
	s, dir := newPruneServer(t)
	for _, id := range []string{"app_a", "app_b"} {
		flow := "a.up"
		if id == "app_b" {
			flow = "b.up"
		}
		if err := app.SaveRecord(dir, &app.Record{AppID: id, FlowID: flow, StartedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}

	// flow_id 过滤：只清目标 flow 的台账。
	rec, out := do(t, s.Handler(), "POST", "/api/apps/prune", map[string]interface{}{"flow_id": "a.up"})
	if rec.Code != http.StatusOK || out["pruned_count"].(float64) != 1 {
		t.Fatalf("按 flow 过滤失败: code=%d %v", rec.Code, out)
	}
	if _, err := app.LoadRecord(dir, "app_b"); err != nil {
		t.Fatalf("别的 flow 的台账不该被清: %v", err)
	}

	// 未注入 registry 路径 ⇒ **拒绝执行**，而不是退回默认值去清另一个库。
	reg2, err := registry.Open(filepath.Join(t.TempDir(), "r2.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reg2.Close() })
	bare, err := New(reg2)
	if err != nil {
		t.Fatal(err)
	}
	rec, _ = do(t, bare.Handler(), "POST", "/api/apps/prune", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("定位不到台账时应 500（拒绝猜位置），实际 %d", rec.Code)
	}

	// 非法 JSON body ⇒ 400，不静默当成「全量清理」。
	req := httptest.NewRequest("POST", "/api/apps/prune", strings.NewReader("{不是 JSON"))
	bad := httptest.NewRecorder()
	s.Handler().ServeHTTP(bad, req)
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("非法 body 应 400，实际 %d body=%s", bad.Code, bad.Body.String())
	}
}
