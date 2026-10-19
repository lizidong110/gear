package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// 说明：本机没有 mongod，所以这里**不假装做过真库端到端**——
// 能纯函数验证的（参数解析、Extended JSON 往返、错误分类）全部纯函数验证；
// 需要真实服务端的路径只验「连不上时是否 fail-closed 且错误码正确」，
// 这恰好是最容易被写错、也最该被锁死的一层（连不上却返回 result 才是致命缺陷）。

// ── 参数解析 ──

func TestParseConnBuildsURI(t *testing.T) {
	cases := []struct {
		name string
		args map[string]interface{}
		want string
	}{
		{"只给 host 走默认端口", map[string]interface{}{"host": "127.0.0.1"}, "mongodb://127.0.0.1:27017"},
		{"指定端口", map[string]interface{}{"host": "db.example.com", "port": float64(27018)}, "mongodb://db.example.com:27018"},
		{"uri 优先于 host", map[string]interface{}{
			"uri": "mongodb://a:1,b:2/?replicaSet=rs0", "host": "ignored",
		}, "mongodb://a:1,b:2/?replicaSet=rs0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseConn(tc.args, false)
			if err != nil {
				t.Fatalf("不该报错: %v", err)
			}
			if got.uri != tc.want {
				t.Fatalf("uri 拼错:\n got=%s\nwant=%s", got.uri, tc.want)
			}
			if got.timeout.Milliseconds() != defaultTimeoutMs {
				t.Fatalf("默认超时应为 %dms，实际 %v", defaultTimeoutMs, got.timeout)
			}
		})
	}

	// 带凭据的两例不断言 URI 字面量：Go 对 userinfo 的转义规则（空格是 %20 还是 +）
	// 属于标准库的实现细节，断言字面量只会把「Go 换了转义策略」误报成我们写错。
	// 这里断言**性质**：凭据原样带上了、没被吃掉也没被泄露到主机部分、authSource 生效。
	for _, tc := range []struct {
		name string
		args map[string]interface{}
		user string
		pass string
		src  string
	}{
		{"默认 authSource=admin", map[string]interface{}{
			"host": "127.0.0.1", "username": "app", "password": "p@ss w/ord",
		}, "app", "p@ss w/ord", "admin"},
		{"自定义 authSource", map[string]interface{}{
			"host": "127.0.0.1", "username": "app", "password": "pw", "auth_source": "mydb",
		}, "app", "pw", "mydb"},
	} {
		t.Run("带凭据"+tc.name, func(t *testing.T) {
			got, err := parseConn(tc.args, false)
			if err != nil {
				t.Fatalf("不该报错: %v", err)
			}
			u, err := url.Parse(got.uri)
			if err != nil {
				t.Fatalf("产出的 URI 连 url.Parse 都过不了: %s / %v", got.uri, err)
			}
			if u.Scheme != "mongodb" {
				t.Fatalf("scheme 应为 mongodb，实际 %q", u.Scheme)
			}
			if u.Host != "127.0.0.1:27017" {
				t.Fatalf("主机端口部分不对（凭据不能被拼进主机）: %q", u.Host)
			}
			if u.User.Username() != tc.user {
				t.Fatalf("用户名没带上: got=%q want=%q", u.User.Username(), tc.user)
			}
			if pw, _ := u.User.Password(); pw != tc.pass {
				t.Fatalf("密码没原样带上（含特殊字符必须转义后能还原）: got=%q want=%q", pw, tc.pass)
			}
			if q := u.Query().Get("authSource"); q != tc.src {
				t.Fatalf("authSource 不对: got=%q want=%q", q, tc.src)
			}
		})
	}
}

func TestParseConnRejectsBadInput(t *testing.T) {
	cases := []struct {
		name  string
		args  map[string]interface{}
		need  bool
		wants string
	}{
		{"既无 uri 也无 host", map[string]interface{}{}, false, "uri 与 host 二选一必填"},
		{"uri 协议不对", map[string]interface{}{"uri": "redis://127.0.0.1:6379"}, false, "mongodb://"},
		{"缺 database", map[string]interface{}{"host": "127.0.0.1"}, true, "database 必填"},
		{"database 含点号", map[string]interface{}{"host": "h", "database": "a.b"}, true, "禁用字符"},
		{"database 含斜杠", map[string]interface{}{"host": "h", "database": "a/b"}, true, "禁用字符"},
		{"timeout 超限", map[string]interface{}{"host": "h", "timeout_ms": float64(20000)}, false, "timeout_ms 不能超过"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseConn(tc.args, tc.need)
			if err == nil {
				t.Fatalf("应当被拒，却通过了")
			}
			if !strings.Contains(err.Error(), tc.wants) {
				t.Fatalf("错误文案不对:\n got=%s\nwant包含=%s", err.Error(), tc.wants)
			}
			if !strings.HasPrefix(err.Error(), errBadRequest+":") {
				t.Fatalf("错误码应为 %s，实际 %s", errBadRequest, err.Error())
			}
		})
	}
}

// ── Extended JSON 往返（BSON ↔ JSON 的边界是最容易静默降级的地方）──

func TestBSONRoundTripKeepsTypes(t *testing.T) {
	in := map[string]interface{}{
		"_id":    map[string]interface{}{"$oid": "507f1f77bcf86cd799439011"},
		"when":   map[string]interface{}{"$date": "2026-10-09T06:50:08Z"},
		"big":    map[string]interface{}{"$numberLong": "9007199254740993"},
		"tags":   []interface{}{"a", "b"},
		"n":      float64(1),
		"nested": map[string]interface{}{"ok": true},
	}
	var d bson.D
	if err := toBSON(in, &d); err != nil {
		t.Fatalf("toBSON 失败: %v", err)
	}
	// ObjectId 必须真的解成 bson.ObjectID，而不是留成字符串——留字符串写回库里就变类型了。
	// （v2 去掉了 bson.D.Map()，所以自己按 key 找。）
	idVal := lookup(d, "_id")
	if _, ok := idVal.(bson.ObjectID); !ok {
		t.Fatalf("_id 应解成 bson.ObjectID，实际 %T", idVal)
	}
	out, err := toJSON(d)
	if err != nil {
		t.Fatalf("toJSON 失败: %v", err)
	}
	raw, _ := json.Marshal(out)
	for _, want := range []string{`"$oid":"507f1f77bcf86cd799439011"`, `"$date"`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("出参丢失类型信息 %s：%s", want, raw)
		}
	}
	// 大整数（>2^53）必须原样出，不能被 float64 抹掉末位：
	// 9007199254740993 经 float64 会变成 ...992，这是「看起来跑通、数据被改」的典型。
	if !strings.Contains(string(raw), "9007199254740993") {
		t.Fatalf("大整数精度被抹掉了：%s", raw)
	}
	// 再解一遍应回到等价结构（往返稳定，才能「读出来再写回去」）。
	var again bson.D
	if err := toBSON(out, &again); err != nil {
		t.Fatalf("二次 toBSON 失败: %v", err)
	}
	if _, ok := lookup(again, "_id").(bson.ObjectID); !ok {
		t.Fatalf("往返后 _id 掉了类型：%T", lookup(again, "_id"))
	}
}

func TestToBSONAcceptsJSONStringAndRejectsGarbage(t *testing.T) {
	var d bson.D
	if err := toBSON(`{"a":1}`, &d); err != nil {
		t.Fatalf("合法 JSON 字符串应被接受: %v", err)
	}
	if len(d) != 1 {
		t.Fatalf("应解出 1 个字段，实际 %d", len(d))
	}
	for _, bad := range []interface{}{nil, `{"a":`, map[string]interface{}{"a": func() {}}} {
		if err := toBSON(bad, &d); err == nil {
			t.Fatalf("%v 应当被拒，却通过了", bad)
		}
	}
	// 数组形态（pipeline / documents）
	var a bson.A
	if err := toBSON([]interface{}{map[string]interface{}{"$match": map[string]interface{}{"x": float64(1)}}}, &a); err != nil {
		t.Fatalf("数组解析失败: %v", err)
	}
	if len(a) != 1 {
		t.Fatalf("应解出 1 个元素，实际 %d", len(a))
	}
}

// toJSON 必须能处理「顶层标量」——直接 Marshal 一个裸字符串 BSON 编码器会报错，
// 所以实现里包了一层文档，这里专门锁住这条路径。
func TestToJSONHandlesTopLevelScalar(t *testing.T) {
	got, err := toJSON("hello")
	if err != nil {
		t.Fatalf("标量转换失败: %v", err)
	}
	if got != "hello" {
		t.Fatalf("标量值不对: %v", got)
	}
	if nilJSON, err := toJSON(nil); err != nil || nilJSON != nil {
		t.Fatalf("nil 应转成 nil: %v / %v", nilJSON, err)
	}
}

// ── 错误分类 ──

func TestClassifyDistinguishesServerRejectFromUnreachable(t *testing.T) {
	// 服务端拒绝（如唯一键冲突）：改参数有用，重试无用
	cmdErr := mongo.CommandError{Code: 11000, Message: "E11000 duplicate key error"}
	if got := classify(cmdErr); !strings.HasPrefix(got.Error(), errMongo+":") {
		t.Fatalf("服务端错误应归为 %s，实际 %s", errMongo, got)
	}
	// 网络/超时：环境问题
	if got := classify(errors.New("server selection error")); !strings.HasPrefix(got.Error(), errUnreachable+":") {
		t.Fatalf("连接失败应归为 %s，实际 %s", errUnreachable, got)
	}
	if got := classify(context.DeadlineExceeded); !strings.HasPrefix(got.Error(), errUnreachable+":") {
		t.Fatalf("超时应归为 %s，实际 %s", errUnreachable, got)
	}
	// 已是带码错误的原样透出，不要二次包装
	orig := fail(errBadRequest, "原始错误")
	if got := classify(orig); got.Error() != orig.Error() {
		t.Fatalf("带码错误不应被改写: %s", got)
	}
}

// ── 方法面一致性 ──

// TestDescribeMatchesImplementation 锁住自描述与实现的一致性：
// `mongoDescribe()` 声明的方法必须就是 `methods`（也就是 Handlers 里真有的端点），
// 且每个方法都要有 schema。这条不成立时，`describe: true` 的扩展会在加载期被内核拒，
// 但**只在有人真的切过去时**才暴露——所以在这里提前锁死。
func TestDescribeMatchesImplementation(t *testing.T) {
	d := mongoDescribe()
	if len(d.Methods) != len(methods) {
		t.Fatalf("自描述声明 %d 个方法，实现有 %d 个", len(d.Methods), len(methods))
	}
	for i, m := range methods {
		if d.Methods[i] != m {
			t.Fatalf("第 %d 个方法不一致: describe=%q code=%q", i, d.Methods[i], m)
		}
		if _, ok := d.Schemas[m]; !ok {
			t.Fatalf("方法 %q 缺 schema", m)
		}
	}
	for m := range d.Schemas {
		if !contains(methods, m) {
			t.Fatalf("自描述给方法 %q 写了 schema，但实现里没有该方法", m)
		}
	}
	// 集合级操作必须要求 database（needDB），否则侧车会在 nil db 上 panic。
	for _, m := range []string{"find", "insert_one", "aggregate", "stats", "watch"} {
		if _, ok := d.Schemas[m].Parameters["database"]; !ok {
			t.Fatalf("方法 %q 的 schema 应含必填 database", m)
		}
	}
	for _, m := range []string{"ping", "list_databases"} {
		if _, ok := d.Schemas[m].Parameters["database"]; ok {
			t.Fatalf("方法 %q 不属于某个库，不该要求 database", m)
		}
	}
}

func TestHandlersCoverExactlyDeclaredMethods(t *testing.T) {
	h := (&service{}).Handlers()
	if len(h) != len(methods)+1 { // +caps
		t.Fatalf("handler 数 %d 与声明方法数 %d(+caps) 不符", len(h), len(methods))
	}
	for _, m := range methods {
		if _, ok := h["/"+m]; !ok {
			t.Fatalf("声明了方法 %q 但没有对应 handler", m)
		}
	}
	if _, ok := h["/caps"]; !ok {
		t.Fatal("缺 /caps 自省端点")
	}
	for path := range h {
		if path != "/caps" && !contains(methods, strings.TrimPrefix(path, "/")) {
			t.Fatalf("handler %s 不在声明的方法清单里（改了 handler 就要改 methods）", path)
		}
	}
}

// TestCapsMatchesExtensionsYAML 锁住「extensions.yaml 的 ext.mongo 段」与「侧车自描述」的一致性：
// 切到 describe:true 后，yaml 不再手抄 methods/schemas（那正是会漂移的两份），
// 只声明「本扩展自描述」；方法清单的真值在 mongoDescribe()（= 代码里的 methods 变量，即 Handlers 端点）。
// 这条断言保证：谁要在 yaml 里把 methods 又手抄回来、或把 schemas 加回来，都会被拦下——
// describe:true 与手抄 schemas 同时出现会在内核加载期 fail-closed（见 ApplySidecarDescribes），
// 这里提前在单测里锁死，避免「切过去又悄悄手抄一份」把漂移口子重新打开。
func TestCapsMatchesExtensionsYAML(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "plugin", "extensions.yaml"))
	if err != nil {
		t.Fatalf("读 extensions.yaml 失败: %v", err)
	}
	inSection, hasDescribe, hasSchemas, hasMethods := false, false, false, false
	for _, ln := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimRight(ln, "\r")
		if trimmed == "ext.mongo:" {
			inSection = true
			continue
		}
		if !inSection {
			continue
		}
		if strings.HasPrefix(trimmed, "ext.") && strings.HasSuffix(trimmed, ":") && trimmed != "ext.mongo:" {
			break // 进入下一段，ext.mongo 段结束
		}
		s := strings.TrimSpace(trimmed)
		if s == "describe: true" || s == "describe:true" {
			hasDescribe = true
		}
		if strings.HasPrefix(s, "schemas:") {
			hasSchemas = true
		}
		if strings.HasPrefix(s, "methods:") {
			hasMethods = true
		}
	}
	if !hasDescribe {
		t.Fatal("ext.mongo 段必须声明 describe: true（自描述开关）")
	}
	if hasSchemas {
		t.Fatal("ext.mongo 已切自描述，extensions.yaml 不应再手抄 schemas（会与侧车自描述漂移，加载期被拒）")
	}
	if hasMethods {
		t.Fatal("ext.mongo 已切自描述，extensions.yaml 不应再手抄 methods（方法真值在 mongoDescribe()）")
	}
	// 自描述的方法清单 == 代码里的 methods（真正的锁：侧车实现了什么，yaml 就让内核取到什么）
	d := mongoDescribe()
	if len(d.Methods) != len(methods) {
		t.Fatalf("自描述 %d 个方法，实现有 %d 个", len(d.Methods), len(methods))
	}
	for i, m := range methods {
		if d.Methods[i] != m {
			t.Fatalf("第 %d 个方法不一致: describe=%q code=%q", i, d.Methods[i], m)
		}
	}
}

// ── handler 级：连不上必须 fail-closed ──

// 127.0.0.1:1 上没有服务（连接会被立刻拒绝），用来验证「连不上」这条路
// 是**明确报错**而不是返回空结果。
const deadHost = "127.0.0.1"

const deadPort = 1

func TestUnreachableServerFailsClosed(t *testing.T) {
	svc := &service{}
	for _, m := range []string{"/ping", "/find", "/insert_one", "/aggregate", "/list_databases"} {
		t.Run(m, func(t *testing.T) {
			args := map[string]interface{}{
				"host": deadHost, "port": float64(deadPort), "timeout_ms": float64(1500),
				"database": "testdb", "collection": "c",
				"document": map[string]interface{}{"a": float64(1)},
				"pipeline": []interface{}{map[string]interface{}{"$match": map[string]interface{}{}}},
			}
			w := call(svc, m, args)
			if w.Code != http.StatusOK {
				t.Fatalf("HTTP 状态应为 200（错误走 body 表达），实际 %d", w.Code)
			}
			var out struct {
				Result interface{} `json:"result"`
				Error  string      `json:"error"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
				t.Fatalf("响应不是合法 JSON: %v / %s", err, w.Body.String())
			}
			if out.Error == "" {
				t.Fatalf("连不上却没报错（最危险的静默降级），body=%s", w.Body.String())
			}
			if !strings.HasPrefix(out.Error, errUnreachable+":") {
				t.Fatalf("错误码应为 %s，实际 %s", errUnreachable, out.Error)
			}
			if out.Result != nil {
				t.Fatalf("报错时不应同时返回 result: %v", out.Result)
			}
		})
	}
}

func TestBadRequestBeforeTouchingServer(t *testing.T) {
	svc := &service{}
	cases := []struct {
		path string
		args map[string]interface{}
		want string
	}{
		{"/find", map[string]interface{}{"host": deadHost, "port": float64(deadPort), "collection": "c"}, "database 必填"},
		{"/find", map[string]interface{}{"host": deadHost, "port": float64(deadPort), "database": "d"}, "collection 必填"},
		{"/find", map[string]interface{}{"host": deadHost, "database": "d", "collection": "c", "limit": float64(999999)}, "limit 必须在"},
		{"/aggregate", map[string]interface{}{"host": deadHost, "database": "d", "collection": "c"}, "pipeline 必填"},
		{"/insert_one", map[string]interface{}{"host": deadHost, "database": "d", "collection": "c"}, "document 必填"},
		{"/distinct", map[string]interface{}{"host": deadHost, "database": "d", "collection": "c"}, "field 必填"},
		{"/drop_index", map[string]interface{}{"host": deadHost, "database": "d", "collection": "c"}, "name 必填"},
		{"/create_index", map[string]interface{}{"host": deadHost, "database": "d", "collection": "c"}, "keys 必填"},
		{"/bulk_write", map[string]interface{}{"host": deadHost, "database": "d", "collection": "c",
			"operations": []interface{}{map[string]interface{}{"type": "teleport"}}}, "未知 type"},
		{"/rename_collection", map[string]interface{}{"host": deadHost, "database": "d", "collection": "c"}, "new_name 必填"},
		{"/find", map[string]interface{}{"uri": "http://x", "database": "d", "collection": "c"}, "mongodb://"},
	}
	for _, tc := range cases {
		t.Run(tc.path+" "+tc.want, func(t *testing.T) {
			w := call(svc, tc.path, tc.args)
			var out struct {
				Error string `json:"error"`
			}
			_ = json.Unmarshal(w.Body.Bytes(), &out)
			if !strings.Contains(out.Error, tc.want) {
				t.Fatalf("期望错误包含 %q，实际 %q", tc.want, out.Error)
			}
			if !strings.HasPrefix(out.Error, errBadRequest+":") {
				t.Fatalf("错误码应为 %s，实际 %q", errBadRequest, out.Error)
			}
		})
	}
}

func TestMalformedBodyIsBadRequest(t *testing.T) {
	svc := &service{}
	r := httptest.NewRequest(http.MethodPost, "/find", strings.NewReader("{not json"))
	w := httptest.NewRecorder()
	svc.Handlers()["/find"](w, r)
	var out struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if !strings.HasPrefix(out.Error, errBadRequest+":") {
		t.Fatalf("请求体非法应报 %s，实际 %q", errBadRequest, out.Error)
	}
}

func TestCapsNeedsNoConnection(t *testing.T) {
	w := call(&service{}, "/caps", map[string]interface{}{})
	var out struct {
		Result struct {
			Methods []string `json:"methods"`
			Notes   []string `json:"notes"`
		} `json:"result"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("caps 响应解析失败: %v", err)
	}
	if len(out.Result.Methods) != len(methods) {
		t.Fatalf("caps 应列出 %d 个方法，实际 %d", len(methods), len(out.Result.Methods))
	}
	if len(out.Result.Notes) == 0 {
		t.Fatal("caps 应带上使用说明（事务边界 / Extended JSON 口径是调用方最容易踩的）")
	}
}

// ── 辅助 ──

func call(svc *service, path string, args map[string]interface{}) *httptest.ResponseRecorder {
	h, ok := svc.Handlers()[path]
	if !ok {
		panic("未知端点 " + path)
	}
	body, _ := json.Marshal(map[string]interface{}{"args": args})
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(body)))
	w := httptest.NewRecorder()
	h(w, r)
	return w
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// lookup 按 key 取 bson.D 的值（v2 的 bson.D 没有 Map() 了）。
func lookup(d bson.D, key string) interface{} {
	for _, e := range d {
		if e.Key == key {
			return e.Value
		}
	}
	return nil
}

// ── applyOneOpts：回归锁 ──
// 曾因「只改入参、不返回」被 staticcheck SA4006 抓出真 bug：
// find_one_and_* 的 upsert/projection/sort/return_document 被静默吞掉，调用方毫无察觉。
// 修复后返回 (T, error)，调用方拿到的就是配置好的 builder（mongo 驱动 v2 用 builder，
// 其配置存在未导出字段里，纯函数层只能验「返回非 nil + 错误分支正确」；真正生效由
// 调用点把返回值交给 coll.FindOneAndUpdate 保证，编译器会拦住「又改回只返回 error」的回退）。
func TestApplyOneOptsAppliesAllFields(t *testing.T) {
	args := map[string]interface{}{
		"upsert":          true,
		"projection":      map[string]interface{}{"name": 1},
		"sort":            map[string]interface{}{"score": -1},
		"return_document": "after",
	}
	fo, err := applyOneOpts(args, options.FindOneAndUpdate())
	if err != nil {
		t.Fatalf("不该报错: %v", err)
	}
	if fo == nil {
		t.Fatal("应返回非 nil 的 builder")
	}

	// return_document 非法值应被拒
	if _, err := applyOneOpts(map[string]interface{}{"return_document": "sideways"}, options.FindOneAndUpdate()); err == nil {
		t.Fatal("非法 return_document 应当被拒")
	}
	// 缺省：不报错，返回非 nil builder
	if fo2, err := applyOneOpts(map[string]interface{}{}, options.FindOneAndUpdate()); err != nil || fo2 == nil {
		t.Fatalf("空参数不该报错且应返回非 nil builder: err=%v fo2=%v", err, fo2)
	}
}
