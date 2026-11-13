package cap

// config_catalog_test.go —— 锁死「目录 ⊇ 解析链」的防漂移闸门。
//
// 背景（GEAR_工程方法 红线）：schema/文档比实现少写枚举值，编译测试全绿，
// 只让下游静默少拿一层能力。配置键目录同理——若 ai_llm.go 新增了一个 cfg("ai.xxx")
// 而目录没跟上，配置页就永远看不到那个键，且没有任何报错。
// 这里用 go/parser 直接读 ai_llm.go 源码，把解析链里真实读的键抠出来与目录比对。

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// configKeysInEngineFiles 从引擎侧源码抠出**真实读取**的配置键，供目录双向比对：
//   - engine/ai_llm.go 的 cfg("ai.xxx") 字面量（AI 缺省配置解析链）；
//   - engine/engine.go 的 <recv>.Get("fs.roots") 单字符串字面量（内核其它自带键，如 fs 根）。
//
// 新增「内核自身读配置库」的键时，读取点落在哪个文件就用哪种形态写；
// 只要目录没跟上，本测试编译期就红（红线：页面绝不能让人配一个不生效的键）。
func configKeysInEngineFiles(t *testing.T) []string {
	t.Helper()
	_, this, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("拿不到当前测试文件路径")
	}
	dir := filepath.Join(filepath.Dir(this), "..", "engine")

	read := func(path string, match func(call *ast.CallExpr) (string, bool)) []string {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("解析 %s 失败: %v", path, err)
		}
		var keys []string
		seen := map[string]bool{}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			k, ok := match(call)
			if !ok || seen[k] {
				return true
			}
			seen[k] = true
			keys = append(keys, k)
			return true
		})
		return keys
	}
	unquote := func(e ast.Expr) (string, bool) {
		lit, ok := e.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(lit.Value)
		if err != nil {
			return "", false
		}
		return s, true
	}

	// ai_llm.go：cfg("...") 全部单参数字符串字面量。
	aiKeys := read(filepath.Join(dir, "ai_llm.go"), func(call *ast.CallExpr) (string, bool) {
		id, ok := call.Fun.(*ast.Ident)
		if !ok || id.Name != "cfg" || len(call.Args) != 1 {
			return "", false
		}
		return unquote(call.Args[0])
	})
	if len(aiKeys) == 0 {
		t.Fatal("ai_llm.go 里一个 cfg(...) 都没找到——是测试写错了还是解析链被改名了？")
	}

	// engine.go：<recv>.Get("...") 单字符串字面量（如 fsRootsFromConfig 的 c.Get("fs.roots")；
	// 变量参数的调用天然不命中，不会被误抓）。
	engKeys := read(filepath.Join(dir, "engine.go"), func(call *ast.CallExpr) (string, bool) {
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Get" || len(call.Args) != 1 {
			return "", false
		}
		return unquote(call.Args[0])
	})

	return append(aiKeys, engKeys...)
}

// TestConfigCatalogMatchesResolveChain：目录与解析链**双向**一致。
// 单向「解析链 ⊆ 目录」只能防漏；「目录 ⊆ 解析链」还要防目录里留着已废弃的键，
// 否则用户改一个根本不生效的键，页面却信誓旦旦说配置成功了。
func TestConfigCatalogMatchesResolveChain(t *testing.T) {
	catalog := map[string]ConfigKeySpec{}
	for _, s := range ConfigCatalog() {
		if catalog[s.Key].Key != "" {
			t.Fatalf("目录里 key 重复: %s", s.Key)
		}
		if s.Label == "" || s.Group == "" || s.ValueType == "" {
			t.Fatalf("目录项 %s 缺中文名/分组/类型（UI 会渲染成空标签）", s.Key)
		}
		catalog[s.Key] = s
	}
	for _, k := range configKeysInEngineFiles(t) {
		if _, ok := catalog[k]; !ok {
			t.Errorf("解析链读了 %q 但目录没有 —— 配置页会静默漏掉这个键", k)
			continue
		}
		delete(catalog, k)
	}
	for k := range catalog {
		t.Errorf("目录里有 %q 但解析链不读 —— 页面会让人配一个不生效的键", k)
	}
}

// TestConfigCatalogKeyMetadata：每个键的元数据自洽，且 api_key 必须标敏感。
func TestConfigCatalogKeyMetadata(t *testing.T) {
	for _, s := range ConfigCatalog() {
		switch s.ValueType {
		case "string", "number", "enum", "secret":
		default:
			t.Errorf("%s 的 value_type=%q 不在 UI 已知四类中", s.Key, s.ValueType)
		}
		if s.ValueType == "enum" && len(s.Options) == 0 {
			t.Errorf("%s 声明 enum 却没有 options（UI 渲染成空下拉框）", s.Key)
		}
		if s.ValueType == "enum" {
			found := false
			for _, o := range s.Options {
				if o == s.Default {
					found = true
				}
			}
			if !found {
				t.Errorf("%s 的默认值 %q 不在 options 里", s.Key, s.Default)
			}
		}
		if s.ValueType == "secret" && !s.Sensitive {
			t.Errorf("%s 是 secret 类型却没标 sensitive（明文会进 HTTP 响应）", s.Key)
		}
		if s.Default != strings.TrimSpace(s.Default) {
			t.Errorf("%s 的默认值首尾有空白", s.Key)
		}
	}
	spec, ok := ConfigKeyByKey("ai.api_key")
	if !ok || !spec.Sensitive || spec.ValueType != "secret" || spec.Env != "GEAR_AI_API_KEY" {
		t.Fatalf("ai.api_key 目录项不符合安全口径: %+v", spec)
	}
	if _, ok := ConfigKeyByKey("not.in.catalog"); ok {
		t.Fatal("未登记的键不应被当成目录项")
	}
}

// TestMaskSecretShared：REST 与 CLI 共用的脱敏口径。
func TestMaskSecretShared(t *testing.T) {
	if !LooksSecretKey("ai.api_key") || !LooksSecretKey("db.token") || LooksSecretKey("ai.model") {
		t.Fatal("敏感键判定口径漂移")
	}
	if got := MaskSecret("short"); strings.Contains(got, "…") || len(got) != len("short") {
		t.Fatalf("短串应全遮: %q", got)
	}
	if got := MaskSecret("sk-abcdefghijklmnop"); got != "sk-a…mnop" {
		t.Fatalf("长串掩码形态异常: %q", got)
	}
	if got := MaskSecret(""); got != "" {
		t.Fatalf("空值不该被遮成星号: %q", got)
	}
}
