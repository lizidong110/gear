package model

import "testing"

// TestAPIRouteValidate 锁住 FR-3 的「非法 method / path 模板」判据（AC-C2 的一半）。
func TestAPIRouteValidate(t *testing.T) {
	ok := []struct {
		name string
		api  APIRoute
	}{
		{"无参数", APIRoute{"GET", "/api/orders"}},
		{"一段参数", APIRoute{"GET", "/api/orders/{id}"}},
		{"两段参数", APIRoute{"POST", "/api/items/{id}/parts/{partNo}"}},
		{"多段字面量", APIRoute{"DELETE", "/api/orders/archive"}},
		{"全部方法", APIRoute{"PATCH", "/api/orders/{id}"}},
	}
	for _, c := range ok {
		if err := c.api.Validate(); err != nil {
			t.Errorf("%s: 合法声明被拒: %v", c.name, err)
		}
	}

	bad := []struct {
		name string
		api  APIRoute
		want string // 报价里的关键词
	}{
		{"空方法", APIRoute{}, "api.method 不能为空"},
		{"小写方法", APIRoute{"get", "/api/orders"}, "api.method"},
		{"未知方法", APIRoute{"HEAD", "/api/orders"}, "api.method"},
		{"空路径", APIRoute{"GET", ""}, "api.path 不能为空"},
		{"没前缀", APIRoute{"GET", "orders"}, `必须以 "/api/" 开头`},
		{"只有前缀", APIRoute{"GET", "/api/"}, "至少要有一段"},
		{"结尾斜杠", APIRoute{"GET", "/api/orders/"}, "不能以 / 结尾"},
		{"空段", APIRoute{"GET", "/api/orders//42"}, "为空"},
		{"空占位符", APIRoute{"GET", "/api/orders/{}"}, "{}"},
		{"占位符名非法", APIRoute{"GET", "/api/orders/{1id}"}, "占位符名"},
		{"半截花括号", APIRoute{"GET", "/api/orders/{id"}, "不成对"},
		{"半个花括号", APIRoute{"GET", "/api/orders/x{id}"}, "不成对"},
		{"内核资源根", APIRoute{"GET", "/api/tools/{id}"}, "内核已占用"},
		{"内核资源根无参", APIRoute{"DELETE", "/api/secrets"}, "内核已占用"},
	}
	for _, c := range bad {
		err := c.api.Validate()
		if err == nil {
			t.Errorf("%s: 非法声明应被拒，却通过了", c.name)
			continue
		}
		if !contains(err.Error(), c.want) {
			t.Errorf("%s: 报错没点出「%s」: %v", c.name, c.want, err)
		}
	}
}

// TestAPIRouteParams 锁住「占位符名即输入字段名」这条映射口径（AC-C2 的另一半）。
func TestAPIRouteParams(t *testing.T) {
	a := APIRoute{"GET", "/api/items/{id}/parts/{partNo}"}
	got := a.Params()
	if len(got) != 2 || got[0] != "id" || got[1] != "partNo" {
		t.Fatalf("参数顺序与名字应为 id,partNo，实际 %v", got)
	}
	plain := APIRoute{Method: "GET", Path: "/api/orders"}
	if plain.Params() != nil {
		t.Fatalf("无参数路径应返回空，实际 %v", plain.Params())
	}
	// Pattern 就是 net/http 的注册形态，测试里顺便锁住它，免得改出去对不上。
	if a.Pattern() != "GET /api/items/{id}/parts/{partNo}" {
		t.Fatalf("Pattern 口径变了: %q", a.Pattern())
	}
}

// TestToolValidateRejectsBadAPI 锁住「注册期就拦」：能静态判定的错误不留到运行期。
func TestToolValidateRejectsBadAPI(t *testing.T) {
	base := func() Tool {
		return Tool{
			ID: "demo__x", Name: "x", Runtime: RuntimeLua, Entry: "return {}",
			InputSchema:  map[string]interface{}{"type": "object"},
			OutputSchema: map[string]interface{}{"type": "object"},
			Versions:     []ToolVersion{{Version: "1.0.0", State: StateStable}},
		}
	}
	t1 := base()
	t1.API = &APIRoute{Method: "GET", Path: "/api/orders"}
	if err := t1.Validate(); err != nil {
		t.Fatalf("合法 api 声明不该被拒: %v", err)
	}
	t2 := base()
	t2.API = &APIRoute{Method: "get", Path: "/api/orders"}
	if err := t2.Validate(); err == nil {
		t.Fatalf("非法 method 应被拒")
	} else if !contains(err.Error(), "api") {
		t.Fatalf("报错应指向 api 字段: %v", err)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
