package mcp

import (
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// TestCapabilitiesKindDescCoversEveryKind：`gear_capabilities` 的 `kind` 描述必须列出
// **代码真正支持的全部类别**（REQ-CAP-KIND-DESC）。
//
// 由来（2026-10-03 外部实测 P0-2）：schema 描述只写 `builtin | ext`，漏了 `pure`，
// 而代码三种都支持。照描述办事的调用方会**整层漏掉纯函数原子**（time/template/crypto/
// id/validate，5 能力 22 方法）——它们不用声明 capabilities 就能调，看不见就等于不存在。
// 这类「描述比实现窄」的缺陷**编译不报错、测试也不报错**，只在下游静默少拿一层能力。
//
// 防再次漏写的做法：**不写死类别清单**，而是从「目录实际产出的 kind」反查描述。
// 将来新增第四种类别而描述没跟上，本用例立刻变红；反过来若目录退化到只剩两类，
// 自检也会先炸（避免 fixture 变化让本测试空跑成永远绿）。
func TestCapabilitiesKindDescCoversEveryKind(t *testing.T) {
	session := newSmokeSession(t)

	var out map[string]any
	callTool(t, session, "gear_capabilities", map[string]any{}, &out)
	list, ok := out["capabilities"].([]any)
	if !ok {
		t.Fatalf("gear_capabilities 应返回 capabilities 数组: %v", out)
	}
	// 收集目录里**实际出现**的类别及各自能力数（报错时直接点名，便于判断影响面）。
	kinds := map[string]int{}
	for _, it := range list {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		if k, _ := m["kind"].(string); k != "" {
			kinds[k]++
		}
	}
	// 自检：目录必须三种齐全，否则本用例的「反查」会因为样本不全而失去意义。
	for _, want := range []string{"builtin", "pure", "ext"} {
		if kinds[want] == 0 {
			t.Fatalf("能力目录应产出 %q 类别（目录退化会让本用例空跑）: %v", want, kinds)
		}
	}

	field, ok := reflect.TypeOf(CapabilitiesArgs{}).FieldByName("Kind")
	if !ok {
		t.Fatal("CapabilitiesArgs 缺 Kind 字段")
	}
	desc := field.Tag.Get("jsonschema")
	if desc == "" {
		t.Fatal("CapabilitiesArgs.Kind 缺 jsonschema 描述")
	}

	// 整词匹配而非子串：`ext` 若只出现在 `extension` 这类词里，调用方仍学不到
	// 「kind 要传 ext」这件事。\b 在 Go regexp 里按 ASCII 词边界，类别名全是小写 ASCII。
	for kind, n := range kinds {
		re := regexp.MustCompile(`\b` + regexp.QuoteMeta(kind) + `\b`)
		if !re.MatchString(desc) {
			t.Errorf("kind 描述漏了类别 %q：照描述办事会整层漏掉 %d 个能力\n描述: %s",
				kind, n, desc)
		}
	}
}

// TestCapabilitiesFiltersByPureKind：补上描述之后，`kind=pure` 这条路径本身也必须真的能用
// ——只修文案不验行为，等于把「描述改对了」当成「功能没问题」。
func TestCapabilitiesFiltersByPureKind(t *testing.T) {
	session := newSmokeSession(t)

	var out map[string]any
	callTool(t, session, "gear_capabilities", map[string]any{"kind": "pure"}, &out)
	list, ok := out["capabilities"].([]any)
	if !ok {
		t.Fatalf("kind=pure 应返回 capabilities 数组: %v", out)
	}
	if len(list) == 0 {
		t.Fatalf("kind=pure 不应为空（纯函数原子始终可用）: %v", out)
	}
	for _, it := range list {
		m, _ := it.(map[string]any)
		if k, _ := m["kind"].(string); k != "pure" {
			t.Errorf("kind=pure 过滤后混入 %v（kind=%q）", m["name"], k)
		}
	}
	// 5 个纯函数原子必须都在：缺一个就是「目录里看得到但过滤漏掉」。
	names := map[string]bool{}
	for _, it := range list {
		if m, ok := it.(map[string]any); ok {
			if n, _ := m["name"].(string); n != "" {
				names[n] = true
			}
		}
	}
	for _, want := range []string{"time", "template", "crypto", "id", "validate"} {
		if !names[want] {
			t.Errorf("kind=pure 应含纯函数原子 %s，实际: %v", want, names)
		}
	}
	// 过滤键要如实回显，便于调用方确认「我传的是这个」。
	if got, _ := out["filter_kind"].(string); !strings.EqualFold(got, "pure") {
		t.Errorf("filter_kind 应回显 pure，实际 %v", out["filter_kind"])
	}
}
