package artifact

import (
	"path/filepath"
	"strings"
	"testing"

	"gear/internal/engine"
	"gear/internal/model"
	"gear/internal/registry"
)

func routeTool(id, method, path string) *model.Tool {
	return &model.Tool{
		ID:           id,
		Name:         id,
		Description:  id,
		Runtime:      model.RuntimeLua,
		Entry:        "return {}",
		InputSchema:  map[string]interface{}{"type": "object"},
		OutputSchema: map[string]interface{}{"type": "object"},
		Versions:     []model.ToolVersion{{Version: "1.0.0", State: model.StateStable}},
		API:          &model.APIRoute{Method: method, Path: path},
	}
}

func openRouteReg(t *testing.T) *registry.Registry {
	t.Helper()
	reg, err := registry.Open(filepath.Join(t.TempDir(), "registry.db"))
	if err != nil {
		t.Fatalf("打开 registry 失败: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	return reg
}

// TestCheckToolRoutesFieldLevelDiags 锁住 AC-C2 的「字段级诊断」：
// method 与 path 分开报，智能体才知道该改 YAML 的哪一行。
func TestCheckToolRoutesFieldLevelDiags(t *testing.T) {
	diags := CheckToolRoutes([]*model.Tool{
		routeTool("a__bad_method", "get", "/api/orders"),
		routeTool("b__bad_path", "GET", "orders/{id}"),
	})
	if len(diags) != 2 {
		t.Fatalf("应各报一条，实际 %d 条: %#v", len(diags), diags)
	}
	if diags[0].Code != engine.CodeRouteInvalid || diags[0].Field != "tool(a__bad_method).api.method" {
		t.Fatalf("method 应有自己的字段路径，实际 %+v", diags[0])
	}
	if diags[1].Code != engine.CodeRouteInvalid || diags[1].Field != "tool(b__bad_path).api.path" {
		t.Fatalf("path 应有自己的字段路径，实际 %+v", diags[1])
	}
	// 合法声明不产生诊断（也就不会把 valid 判成 false）。
	if d := CheckToolRoutes([]*model.Tool{routeTool("a__ok", "GET", "/api/orders/{id}")}); len(d) != 0 {
		t.Fatalf("合法声明不该有诊断: %#v", d)
	}
}

// TestCheckToolRoutesConflictDiag 锁住 AC-C3 的诊断文案：必须点名**两个** Tool——
// 两边各自都合法，改任意一个都行，所以只报一个等于没给修法。
func TestCheckToolRoutesConflictDiag(t *testing.T) {
	diags := CheckToolRoutes([]*model.Tool{
		routeTool("a__orders_get", "GET", "/api/orders"),
		routeTool("b__orders_list", "GET", "/api/orders"),
	})
	if len(diags) != 1 {
		t.Fatalf("两条声明撞一次应只报一条，实际 %d 条: %#v", len(diags), diags)
	}
	d := diags[0]
	if d.Code != engine.CodeRouteConflict {
		t.Fatalf("码应为 ROUTE_CONFLICT，实际 %q", d.Code)
	}
	for _, want := range []string{"a__orders_get", "b__orders_list", "GET /api/orders"} {
		if !strings.Contains(d.Message, want) {
			t.Fatalf("诊断应点名 %s，实际: %s", want, d.Message)
		}
	}
}

// TestApplyBundleRejectsRouteConflict 锁住 AC-C3 的「拒绝整包、原子语义不变」：
// 冲突的 bundle 一个定义都不落库（这与迁移、定义非法走同一条原子路径）。
func TestApplyBundleRejectsRouteConflict(t *testing.T) {
	reg := openRouteReg(t)
	b := &Bundle{Tools: []*model.Tool{
		routeTool("a__orders_get", "GET", "/api/orders"),
		routeTool("b__orders_list", "GET", "/api/orders"),
	}}
	_, _, _, err := ApplyBundle(reg, b, nil, false)
	if err == nil {
		t.Fatalf("冲突 bundle 应被拒")
	}
	if !strings.Contains(err.Error(), "GET /api/orders") {
		t.Fatalf("错误应点名冲突的路由: %v", err)
	}
	tools, err := reg.ListTools()
	if err != nil {
		t.Fatalf("列出 Tool 失败: %v", err)
	}
	if len(tools) != 0 {
		t.Fatalf("冲突 bundle 不该写入任何定义，实际落了 %d 条", len(tools))
	}
}

// TestRouteOwnerConflictAcrossBundles 锁住**跨 bundle** 那一条路：
// 两次 apply 是两个事务、两份 bundle，bundle 内的冲突检查看不见它。
// 同名 pattern 第二次注册会让 net/http panic（起服时），所以必须在入库前拒。
func TestRouteOwnerConflictAcrossBundles(t *testing.T) {
	reg := openRouteReg(t)
	first := routeTool("a__orders_get", "GET", "/api/orders")
	if err := reg.UpsertTool(first); err != nil {
		t.Fatalf("首次注册应成功: %v", err)
	}
	second := routeTool("b__orders_list", "GET", "/api/orders")
	err := reg.UpsertTool(second)
	if err == nil {
		t.Fatalf("撞名的第二次注册应被拒（否则起服时 net/http 会 panic）")
	}
	if !strings.Contains(err.Error(), "b__orders_list") || !strings.Contains(err.Error(), "a__orders_get") {
		t.Fatalf("错误应点名两边: %v", err)
	}
	// 换个 path 就该正常（不是「声明过 api 就不能写」）。
	ok := routeTool("b__orders_list", "GET", "/api/orders-view")
	if err := reg.UpsertTool(ok); err != nil {
		t.Fatalf("换个 path 后应可注册: %v", err)
	}
}
