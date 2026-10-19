package engine

import "testing"

// 引擎「打开地址」收集逻辑（openurl.go）的隔离测试：不依赖完整 Engine 装配，
// 直接用零值 Engine 验证 AddOpenURL / OpenURLs / ClearOpenURLs / collectStepOpenURLs。
func TestOpenURLCollection(t *testing.T) {
	e := &Engine{} // 零值：openURLs 为 nil，由方法惰性初始化

	// done 步带回 {url=...} 应收进集合
	collectStepOpenURLs(e, "web_ui.up", map[string]interface{}{"url": "http://127.0.0.1:8080/"})
	got := e.OpenURLs("web_ui.up")
	if len(got) != 1 || got[0] != "http://127.0.0.1:8080/" {
		t.Fatalf("期望 [http://127.0.0.1:8080/]，实际 %v", got)
	}

	// open_urls 数组也收；与 url 去重合并
	collectStepOpenURLs(e, "web_ui.up", map[string]interface{}{
		"url":       "http://127.0.0.1:8080/",
		"open_urls": []interface{}{"http://127.0.0.1:8080/", "http://localhost:8080/admin"},
	})
	got = e.OpenURLs("web_ui.up")
	if len(got) != 2 {
		t.Fatalf("期望去重后 2 条，实际 %v", got)
	}
	has := func(s string) bool {
		for _, u := range got {
			if u == s {
				return true
			}
		}
		return false
	}
	if !has("http://localhost:8080/admin") {
		t.Fatalf("缺少 open_urls 里的地址，实际 %v", got)
	}

	// 空串 / 纯空白不收
	e.AddOpenURL("web_ui.up", "   ")
	if len(e.OpenURLs("web_ui.up")) != 2 {
		t.Fatalf("空白地址不应被收，实际 %v", e.OpenURLs("web_ui.up"))
	}

	// 缺字段不报错、不收集
	collectStepOpenURLs(e, "web_ui.up", map[string]interface{}{"foo": "bar"})

	// 不同 flow 各自独立
	collectStepOpenURLs(e, "other.up", map[string]interface{}{"url": "http://x:9/"})
	if got := e.OpenURLs("other.up"); len(got) != 1 || got[0] != "http://x:9/" {
		t.Fatalf("other.up 期望单独一条，实际 %v", got)
	}

	// 清空
	e.ClearOpenURLs("web_ui.up")
	if len(e.OpenURLs("web_ui.up")) != 0 {
		t.Fatalf("Clear 后应空，实际 %v", e.OpenURLs("web_ui.up"))
	}
	if len(e.OpenURLs("other.up")) != 1 {
		t.Fatalf("Clear 不应影响其他 flow，实际 %v", e.OpenURLs("other.up"))
	}
}
