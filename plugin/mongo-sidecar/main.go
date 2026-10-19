// Command mongo-sidecar 是 ext.mongo（MongoDB 数据面）的常驻侧车。
//
// 形态与其它常驻侧车一致（EXT_SERVICE_DESIGN.md §3/§12）：
//   - 绑定 COORD_ADDR（extensions.yaml 的 listen，127.0.0.1 only），
//     对外暴露业务 RPC 端点（POST /<method>，body {"args":...} → {"result"|"error"}）；
//   - 占用列表空 → 自动退出；端口被占 → 退让退出（owner gear 改以 client 复用）。
//
// 三个刻意的设计约束（都会在 responses 里如实体现，不静默降级）：
//
//	① **无状态**：每个 HTTP 调用彼此独立，MongoDB 的 session 不跨请求存活，
//	   所以**不提供多文档事务**（事务必须 begin/commit 在同一会话里，跨 HTTP 做不到）。
//	   需要原子性时用 `bulk_write`（单次调用内的有序批量写）或 `$isolated` 语义的 update。
//	② **JSON 是唯一数据交换格式**：MongoDB 的 BSON 有 ObjectId / Date / Long / Binary
//	   等 JSON 没有的类型，本侧车统一走 **MongoDB Extended JSON（relaxed）**——
//	   入参里写 `{"$oid":"..."}` / `{"$date":"..."}` / `{"$numberLong":"..."}`，
//	   出参里 `_id` 回 `{"$oid":"..."}`。这样入参出参可原样往返，而不是把 ObjectId
//	   悄悄降级成字符串（降级了就再也写不回去了）。
//	③ **连接按需建立、按 URI 复用**：侧车长驻，`*mongo.Client` 按 URI 缓存；
//	   `mongo.Connect` 不发网络包，真正的连通性由 `ping` 或首个操作暴露——
//	   所以连不上时是**明确的 mongo_unreachable**，不是一个假装成功的假连接。
//
// 凭据从调用参数传（uri / username+password），**不读环境变量、不读配置库**——
// 与 ext.redis 同一口径：侧车刻意不做配置回退，避免凭据来源变成隐式契约。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"gear/internal/sidecar/coord"
	"gear/internal/sidecar/describe"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

const (
	defaultMongoPort = 27017
	defaultLimit     = 100 // find 的默认条数：不给 limit 也不能把整个集合拖回来
	maxDocs          = 10000
	defaultTimeoutMs = 8000 // 内核 LocalHTTPTransport 的客户端超时是 10s，留 2s 余量
	maxTimeoutMs     = 9000
	defaultWaitMs    = 3000 // watch 最长等待（同样受 10s 传输超时约束）
	maxWaitMs        = 8000
	defaultBatch     = 100
	maxBatch         = 1000
	defaultAuthSrc   = "admin"

	// serverSelectionTimeout 是「选不到可用节点就放弃」的上限，独立于单次操作超时：
	// 连不上时不要让用户干等一整个操作超时。
	serverSelectionTimeout = 5 * time.Second
)

// 错误码（响应 `error` 字段形如 "<code>: <人话>"；调用方靠 code 分支，中文文案随时会改）。
const (
	errBadRequest  = "bad_request"
	errUnreachable = "mongo_unreachable"
	errMongo       = "mongo_error"
	errUnsupported = "unsupported"
)

// methods 是本侧车实现的全部业务方法（caps 与 Handlers 共用同一份，杜绝两处漂移）。
var methods = []string{
	// 连通性
	"ping",
	// 读
	"find", "find_one", "count", "estimated_count", "aggregate", "distinct",
	// 写
	"insert_one", "insert_many",
	"update_one", "update_many", "replace_one",
	"delete_one", "delete_many",
	"find_one_and_update", "find_one_and_replace", "find_one_and_delete",
	"bulk_write",
	// 索引
	"create_index", "drop_index", "list_indexes",
	// 库/集合管理
	"create_collection", "drop_collection", "rename_collection", "list_collections",
	"list_databases", "stats",
	// 逃生口 / 变更流
	"run_command", "watch",
}

// ── 错误 ──

type codedError struct{ code, msg string }

func (e *codedError) Error() string { return e.code + ": " + e.msg }

func fail(code, format string, a ...interface{}) error {
	return &codedError{code, fmt.Sprintf(format, a...)}
}

// classify 把驱动返回的错误归成稳定机器码。
//
// 区分两类很关键：**服务端拒绝**（mongo_error，命令真的发出去了、被业务规则挡回，
// 调用方该改自己的参数）与**连不上**（mongo_unreachable，是环境问题，重试或换地址）。
// 混成一类的话，调用方会把「唯一键冲突」当成「网络抖动」去重试，越试越错。
func classify(err error) error {
	if err == nil {
		return nil
	}
	var ce *codedError
	if errors.As(err, &ce) {
		return err
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fail(errUnreachable, "操作超时（可加大 timeout_ms，上限 %dms）", maxTimeoutMs)
	}
	var cmdErr mongo.CommandError
	if errors.As(err, &cmdErr) {
		return fail(errMongo, "服务端拒绝: %s (code=%d)", cmdErr.Message, cmdErr.Code)
	}
	var wExc mongo.WriteException
	if errors.As(err, &wExc) {
		msg := "写入被拒"
		if len(wExc.WriteErrors) > 0 {
			msg = wExc.WriteErrors[0].Message
		}
		return fail(errMongo, "%s", msg)
	}
	var bwExc mongo.BulkWriteException
	if errors.As(err, &bwExc) {
		msg := "批量写入被拒"
		if len(bwExc.WriteErrors) > 0 {
			msg = bwExc.WriteErrors[0].Message
		}
		return fail(errMongo, "%s", msg)
	}
	return fail(errUnreachable, "MongoDB 连接/读写失败: %v", err)
}

// ── 连接参数 ──

type connSpec struct {
	uri      string
	database string
	timeout  time.Duration
}

// parseConn 解析连接参数。两种给法二选一：
//   - `uri`：完整 mongodb:// / mongodb+srv:// URI（副本集、TLS、readPreference 等高级
//     选项只能走这条），给了 uri 就忽略 host/port/user/password/auth_source；
//   - `host`(+`port`)：按零件拼，凭据经 url.UserPassword 转义后拼进 URI。
//
// needDB=true 时 `database` 必填（集合级操作都需要它）。
func parseConn(args map[string]interface{}, needDB bool) (connSpec, error) {
	var c connSpec

	if uri := strArg(args, "uri"); uri != "" {
		if !strings.HasPrefix(uri, "mongodb://") && !strings.HasPrefix(uri, "mongodb+srv://") {
			return c, fail(errBadRequest, "uri 必须以 mongodb:// 或 mongodb+srv:// 开头（收到 %q）", truncate(uri, 32))
		}
		c.uri = uri
	} else {
		host := strArg(args, "host")
		if host == "" {
			return c, fail(errBadRequest, "uri 与 host 二选一必填（host 只含主机，不含端口）")
		}
		port := int64(defaultMongoPort)
		if p, ok := intArg(args, "port"); ok && p > 0 {
			port = p
		}
		u := &url.URL{Scheme: "mongodb", Host: net.JoinHostPort(host, strconv.FormatInt(port, 10))}
		if user := strArg(args, "username"); user != "" {
			authSrc := strArg(args, "auth_source")
			if authSrc == "" {
				authSrc = defaultAuthSrc
			}
			u.User = url.UserPassword(user, strArg(args, "password"))
			u.RawQuery = url.Values{"authSource": {authSrc}}.Encode()
		}
		c.uri = u.String()
	}

	if needDB {
		c.database = strArg(args, "database")
		if c.database == "" {
			return c, fail(errBadRequest, "database 必填")
		}
		if err := checkDBName(c.database); err != nil {
			return c, err
		}
	}

	ms := int64(defaultTimeoutMs)
	if t, ok := intArg(args, "timeout_ms"); ok && t > 0 {
		ms = t
	}
	if ms > maxTimeoutMs {
		return c, fail(errBadRequest, "timeout_ms 不能超过 %d（内核调侧车的客户端超时是 10s）", maxTimeoutMs)
	}
	c.timeout = time.Duration(ms) * time.Millisecond
	return c, nil
}

// checkDBName 挡掉 MongoDB 明确不允许的库名字符（含 NUL / 路径分隔符 / `$`）。
// 这不是"防注入"姿态——BSON 本身没有注入可言——而是避免把明显拼错的库名
// 一路送到服务端换回一句看不懂的报错。
func checkDBName(name string) error {
	if strings.ContainsRune(name, 0) {
		return fail(errBadRequest, "database 不能含 NUL")
	}
	for _, bad := range []string{"/", "\\", ".", " ", "\"", "$"} {
		if strings.Contains(name, bad) {
			return fail(errBadRequest, "database 不能含 %q（MongoDB 库名禁用字符）", bad)
		}
	}
	return nil
}

// checkCollName 同理，集合名的约束比库名松（允许 `.`），只挡空/NUL。
func checkCollName(args map[string]interface{}) (string, error) {
	name := strArg(args, "collection")
	if name == "" {
		return "", fail(errBadRequest, "collection 必填")
	}
	if strings.ContainsRune(name, 0) {
		return "", fail(errBadRequest, "collection 不能含 NUL")
	}
	return name, nil
}

// ── 客户端缓存 ──

var (
	cliMu    sync.Mutex
	cliCache = map[string]*mongo.Client{}
)

// getClient 按 URI 复用 *mongo.Client（侧车长驻；不复用会每请求新建连接池）。
func getClient(uri string) (*mongo.Client, error) {
	cliMu.Lock()
	if c, ok := cliCache[uri]; ok {
		cliMu.Unlock()
		return c, nil
	}
	cliMu.Unlock()

	// v2 的 ApplyURI 不返回错误（错在内部，由 Connect 抛出），而 Connect 的错误
	// 又一律被归成「连不上」——那样写错的 URI 会被报成网络问题，方向完全相反。
	// 所以先自己过一遍 url.Parse：解析不过就是调用方写错，当场按 bad_request 拒。
	if _, err := url.Parse(uri); err != nil {
		return nil, fail(errBadRequest, "URI 不合法: %v", err)
	}
	opts := options.Client().ApplyURI(uri)
	opts.SetServerSelectionTimeout(serverSelectionTimeout)
	opts.SetConnectTimeout(serverSelectionTimeout)
	c, err := mongo.Connect(opts)
	if err != nil {
		return nil, fail(errUnreachable, "建立客户端失败: %v", err)
	}

	cliMu.Lock()
	defer cliMu.Unlock()
	if old, ok := cliCache[uri]; ok {
		// 竞态：别人先放进去了 → 关掉自己这个，用先到的（避免泄漏一个连接池）。
		_ = c.Disconnect(context.Background())
		return old, nil
	}
	cliCache[uri] = c
	return c, nil
}

// ── JSON ⇄ BSON（Extended JSON relaxed，见文件头 ②）──

// toBSON 把入参里的文档/数组转成 BSON。
//
// 接受两种形态：原生 JSON 对象（Lua table 转过来的），或一段 JSON 字符串
// （写复杂聚合管道时嵌在 YAML 里更舒服）。字符串形态必须先过 json.Valid，
// 否则会被当成「一个字符串值」静默解成错的 BSON。
func toBSON(v interface{}, out interface{}) error {
	if v == nil {
		return fail(errBadRequest, "文档不能为空（读全表给空对象 {}，不是 null）")
	}
	var raw []byte
	switch t := v.(type) {
	case string:
		if !json.Valid([]byte(t)) {
			return fail(errBadRequest, "不是合法 JSON 字符串: %s", truncate(t, 60))
		}
		raw = []byte(t)
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return fail(errBadRequest, "无法序列化为 JSON: %v", err)
		}
		raw = b
	}
	if err := bson.UnmarshalExtJSON(raw, false, out); err != nil {
		return fail(errBadRequest, "Extended JSON 解析失败: %v", err)
	}
	return nil
}

func toDoc(args map[string]interface{}, key string, required bool) (bson.D, error) {
	v, ok := args[key]
	if !ok || v == nil {
		if required {
			return nil, fail(errBadRequest, "%s 必填", key)
		}
		return bson.D{}, nil
	}
	var d bson.D
	if err := toBSON(v, &d); err != nil {
		return nil, err
	}
	return d, nil
}

func toArray(args map[string]interface{}, key string, required bool) (bson.A, error) {
	v, ok := args[key]
	if !ok || v == nil {
		if required {
			return nil, fail(errBadRequest, "%s 必填", key)
		}
		return bson.A{}, nil
	}
	var a bson.A
	if err := toBSON(v, &a); err != nil {
		return nil, err
	}
	return a, nil
}

// toJSON 把任意 BSON 值转成可进 HTTP 响应的纯 JSON 值。
//
// 用「包一层文档再取 v」的写法，而不是直接 Marshal 原值：BSON 的 ExtJSON 编码器
// 对**顶层标量**（一个裸 string / int）会报错，包一层后标量、文档、文档数组走同一条路，
// 调用方不必在每种返回类型上分别处理。
func toJSON(v interface{}) (interface{}, error) {
	if v == nil {
		return nil, nil
	}
	raw, err := bson.MarshalExtJSON(bson.D{{Key: "v", Value: v}}, false, false)
	if err != nil {
		return nil, fail(errUnsupported, "结果无法转成 JSON: %v", err)
	}
	// ⚠ 必须用 UseNumber：**不能用 encoding/json 的默认解码**。默认把一切数字解成 float64，
	// 于是 int64 超出 2^53 的部分被静默抹掉（实测 9007199254740993 出来变成 ...992）。
	// 这是「看起来跑通了、数据其实被改了」的典型，比报错更坏。用 json.Number 保住原始数字串。
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var m map[string]interface{}
	if err := dec.Decode(&m); err != nil {
		return nil, fail(errUnsupported, "结果 JSON 反序列化失败: %v", err)
	}
	return m["v"], nil
}

func toJSONSlice(docs []bson.D) ([]interface{}, error) {
	out := make([]interface{}, 0, len(docs))
	for _, d := range docs {
		j, err := toJSON(d)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, nil
}

// ── 参数辅助 ──

func strArg(m map[string]interface{}, k string) string {
	if v, ok := m[k].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

func intArg(m map[string]interface{}, k string) (int64, bool) {
	switch v := m[k].(type) {
	case float64:
		return int64(v), true
	case string:
		if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
			return n, true
		}
	}
	return 0, false
}

func boolArg(m map[string]interface{}, k string) (bool, bool) {
	switch v := m[k].(type) {
	case bool:
		return v, true
	case string:
		if b, err := strconv.ParseBool(strings.TrimSpace(v)); err == nil {
			return b, true
		}
	case float64:
		return v != 0, true
	}
	return false, false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ── 调用上下文 ──

// op 是一次方法调用的上下文。db 在 needDB=false 时保持 nil（ping/list_databases
// 不属于某个库），误用会在 nil 上 panic，所以每个 handler 自己清楚要不要库。
type op struct {
	ctx    context.Context
	client *mongo.Client
	db     *mongo.Database
}

func (o *op) coll(args map[string]interface{}) (*mongo.Collection, error) {
	name, err := checkCollName(args)
	if err != nil {
		return nil, err
	}
	return o.db.Collection(name), nil
}

func (o *op) admin() *mongo.Database { return o.client.Database("admin") }

// ── service ──

type service struct{}

func (s *service) Handlers() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"/ping": s.handlePing,
		// 读
		"/find": s.handleFind, "/find_one": s.handleFindOne,
		"/count": s.handleCount, "/estimated_count": s.handleEstimatedCount,
		"/aggregate": s.handleAggregate, "/distinct": s.handleDistinct,
		// 写
		"/insert_one": s.handleInsertOne, "/insert_many": s.handleInsertMany,
		"/update_one": s.handleUpdateOne, "/update_many": s.handleUpdateMany,
		"/replace_one": s.handleReplaceOne,
		"/delete_one":  s.handleDeleteOne, "/delete_many": s.handleDeleteMany,
		"/find_one_and_update":  s.handleFindOneAndUpdate,
		"/find_one_and_replace": s.handleFindOneAndReplace,
		"/find_one_and_delete":  s.handleFindOneAndDelete,
		"/bulk_write":           s.handleBulkWrite,
		// 索引
		"/create_index": s.handleCreateIndex, "/drop_index": s.handleDropIndex,
		"/list_indexes": s.handleListIndexes,
		// 库/集合管理
		"/create_collection": s.handleCreateCollection,
		"/drop_collection":   s.handleDropCollection,
		"/rename_collection": s.handleRenameCollection,
		"/list_collections":  s.handleListCollections,
		"/list_databases":    s.handleListDatabases,
		"/stats":             s.handleStats,
		// 逃生口 / 变更流
		"/run_command": s.handleRunCommand, "/watch": s.handleWatch,
		// 自省
		"/caps": s.handleCaps,
	}
}

func (s *service) handleCaps(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]interface{}{"result": map[string]interface{}{
		"methods": methods,
		"notes": []string{
			"入参/出参统一走 MongoDB Extended JSON（relaxed）：ObjectId 写 {\"$oid\":\"...\"}、日期写 {\"$date\":\"...\"}、长整写 {\"$numberLong\":\"...\"}",
			"连接参数每次调用都要带（uri 或 host+port），侧车不做配置库回退",
			"不提供多文档事务：侧车无状态，session 不跨 HTTP 请求存活；单次调用的原子性用 bulk_write",
		},
	}})
}

// run 是所有业务方法的统一外壳：解参数 → 取客户端 → 限时 → 执行 → 写响应。
// needDB=true 时要求 database 必填并注入 db。
func (s *service) run(w http.ResponseWriter, r *http.Request, needDB bool,
	exec func(o *op, args map[string]interface{}) (interface{}, error)) {

	args, err := decodeArgs(r)
	if err != nil {
		writeErr(w, errBadRequest, "请求体不是合法 JSON: %v", err)
		return
	}
	c, err := parseConn(args, needDB)
	if err != nil {
		writeCodedErr(w, err)
		return
	}
	client, err := getClient(c.uri)
	if err != nil {
		writeCodedErr(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()

	o := &op{ctx: ctx, client: client}
	if needDB {
		o.db = client.Database(c.database)
	}
	res, err := exec(o, args)
	if err != nil {
		writeCodedErr(w, classify(err))
		return
	}
	writeJSON(w, map[string]interface{}{"result": res})
}

// ── 连通性 ──

func (s *service) handlePing(w http.ResponseWriter, r *http.Request) {
	s.run(w, r, false, func(o *op, _ map[string]interface{}) (interface{}, error) {
		// mongo.Connect 不发网络包，ping 才是真正的连通性证明。
		if err := o.client.Ping(o.ctx, nil); err != nil {
			return nil, err
		}
		return map[string]interface{}{"ok": true}, nil
	})
}

// ── 读 ──

func (s *service) handleFind(w http.ResponseWriter, r *http.Request) {
	s.run(w, r, true, func(o *op, args map[string]interface{}) (interface{}, error) {
		coll, err := o.coll(args)
		if err != nil {
			return nil, err
		}
		filter, err := toDoc(args, "filter", false)
		if err != nil {
			return nil, err
		}
		fo := options.Find()
		if p, err := toDoc(args, "projection", false); err != nil {
			return nil, err
		} else if len(p) > 0 {
			fo.SetProjection(p)
		}
		if so, err := toDoc(args, "sort", false); err != nil {
			return nil, err
		} else if len(so) > 0 {
			fo.SetSort(so)
		}
		limit := int64(defaultLimit)
		if l, ok := intArg(args, "limit"); ok {
			limit = l
		}
		// 不给 limit 也不能把整个集合拖回来：默认 100，上限 10000，越界直接拒。
		if limit <= 0 || limit > maxDocs {
			return nil, fail(errBadRequest, "limit 必须在 1..%d（默认 %d）", maxDocs, defaultLimit)
		}
		fo.SetLimit(limit)
		if sk, ok := intArg(args, "skip"); ok && sk >= 0 {
			fo.SetSkip(sk)
		}
		cur, err := coll.Find(o.ctx, filter, fo)
		if err != nil {
			return nil, err
		}
		defer cur.Close(o.ctx)
		var docs []bson.D
		if err := cur.All(o.ctx, &docs); err != nil {
			return nil, err
		}
		out, err := toJSONSlice(docs)
		if err != nil {
			return nil, err
		}
		return map[string]interface{}{"documents": out, "count": len(out)}, nil
	})
}

func (s *service) handleFindOne(w http.ResponseWriter, r *http.Request) {
	s.run(w, r, true, func(o *op, args map[string]interface{}) (interface{}, error) {
		coll, err := o.coll(args)
		if err != nil {
			return nil, err
		}
		filter, err := toDoc(args, "filter", false)
		if err != nil {
			return nil, err
		}
		fo := options.FindOne()
		if p, err := toDoc(args, "projection", false); err != nil {
			return nil, err
		} else if len(p) > 0 {
			fo.SetProjection(p)
		}
		if so, err := toDoc(args, "sort", false); err != nil {
			return nil, err
		} else if len(so) > 0 {
			fo.SetSort(so)
		}
		if sk, ok := intArg(args, "skip"); ok && sk >= 0 {
			fo.SetSkip(sk)
		}
		var doc bson.D
		err = coll.FindOne(o.ctx, filter, fo).Decode(&doc)
		if err != nil {
			// 没命中不是错误：给 null，让调用方自己判空（与 redis get 的口径一致）。
			if errors.Is(err, mongo.ErrNoDocuments) {
				return map[string]interface{}{"document": nil}, nil
			}
			return nil, err
		}
		out, err := toJSON(doc)
		if err != nil {
			return nil, err
		}
		return map[string]interface{}{"document": out}, nil
	})
}

func (s *service) handleCount(w http.ResponseWriter, r *http.Request) {
	s.run(w, r, true, func(o *op, args map[string]interface{}) (interface{}, error) {
		coll, err := o.coll(args)
		if err != nil {
			return nil, err
		}
		filter, err := toDoc(args, "filter", false)
		if err != nil {
			return nil, err
		}
		co := options.Count()
		if l, ok := intArg(args, "limit"); ok && l > 0 {
			co.SetLimit(l)
		}
		if sk, ok := intArg(args, "skip"); ok && sk >= 0 {
			co.SetSkip(sk)
		}
		n, err := coll.CountDocuments(o.ctx, filter, co)
		if err != nil {
			return nil, err
		}
		return map[string]interface{}{"count": n}, nil
	})
}

func (s *service) handleEstimatedCount(w http.ResponseWriter, r *http.Request) {
	s.run(w, r, true, func(o *op, args map[string]interface{}) (interface{}, error) {
		coll, err := o.coll(args)
		if err != nil {
			return nil, err
		}
		n, err := coll.EstimatedDocumentCount(o.ctx)
		if err != nil {
			return nil, err
		}
		return map[string]interface{}{"count": n, "estimated": true}, nil
	})
}

func (s *service) handleAggregate(w http.ResponseWriter, r *http.Request) {
	s.run(w, r, true, func(o *op, args map[string]interface{}) (interface{}, error) {
		coll, err := o.coll(args)
		if err != nil {
			return nil, err
		}
		pipeline, err := toArray(args, "pipeline", true)
		if err != nil {
			return nil, err
		}
		if len(pipeline) == 0 {
			return nil, fail(errBadRequest, "pipeline 不能为空数组")
		}
		ao := options.Aggregate()
		if b, ok := boolArg(args, "allow_disk_use"); ok {
			ao.SetAllowDiskUse(b)
		}
		if bs, ok := intArg(args, "batch_size"); ok && bs > 0 {
			ao.SetBatchSize(int32(bs))
		}
		cur, err := coll.Aggregate(o.ctx, pipeline, ao)
		if err != nil {
			return nil, err
		}
		defer cur.Close(o.ctx)
		docs, err := drainCursor(o.ctx, cur, maxDocs)
		if err != nil {
			return nil, err
		}
		out, err := toJSONSlice(docs)
		if err != nil {
			return nil, err
		}
		return map[string]interface{}{"documents": out, "count": len(out)}, nil
	})
}

func (s *service) handleDistinct(w http.ResponseWriter, r *http.Request) {
	s.run(w, r, true, func(o *op, args map[string]interface{}) (interface{}, error) {
		coll, err := o.coll(args)
		if err != nil {
			return nil, err
		}
		field := strArg(args, "field")
		if field == "" {
			return nil, fail(errBadRequest, "field 必填")
		}
		filter, err := toDoc(args, "filter", false)
		if err != nil {
			return nil, err
		}
		var raw bson.A
		if err := coll.Distinct(o.ctx, field, filter).Decode(&raw); err != nil {
			return nil, err
		}
		out, err := toJSON(raw)
		if err != nil {
			return nil, err
		}
		if out == nil {
			out = []interface{}{}
		}
		return map[string]interface{}{"values": out}, nil
	})
}

// ── 写 ──

func (s *service) handleInsertOne(w http.ResponseWriter, r *http.Request) {
	s.run(w, r, true, func(o *op, args map[string]interface{}) (interface{}, error) {
		coll, err := o.coll(args)
		if err != nil {
			return nil, err
		}
		doc, err := toDoc(args, "document", true)
		if err != nil {
			return nil, err
		}
		if len(doc) == 0 {
			return nil, fail(errBadRequest, "document 不能是空对象")
		}
		res, err := coll.InsertOne(o.ctx, doc)
		if err != nil {
			return nil, err
		}
		id, err := toJSON(res.InsertedID)
		if err != nil {
			return nil, err
		}
		return map[string]interface{}{"inserted_id": id, "acknowledged": res.Acknowledged}, nil
	})
}

func (s *service) handleInsertMany(w http.ResponseWriter, r *http.Request) {
	s.run(w, r, true, func(o *op, args map[string]interface{}) (interface{}, error) {
		coll, err := o.coll(args)
		if err != nil {
			return nil, err
		}
		arr, ok := args["documents"].([]interface{})
		if !ok || len(arr) == 0 {
			return nil, fail(errBadRequest, "documents 必填（非空数组）")
		}
		if len(arr) > maxDocs {
			return nil, fail(errBadRequest, "documents 最多 %d 条（分批调用）", maxDocs)
		}
		docs := make([]interface{}, 0, len(arr))
		for i, e := range arr {
			var d bson.D
			if err := toBSON(e, &d); err != nil {
				return nil, fail(errBadRequest, "documents[%d]: %v", i, err)
			}
			if len(d) == 0 {
				return nil, fail(errBadRequest, "documents[%d] 是空对象", i)
			}
			docs = append(docs, d)
		}
		io := options.InsertMany()
		if o, ok := boolArg(args, "ordered"); ok {
			io.SetOrdered(o)
		}
		res, err := coll.InsertMany(o.ctx, docs, io)
		if err != nil {
			return nil, err
		}
		ids, err := toJSON(res.InsertedIDs)
		if err != nil {
			return nil, err
		}
		return map[string]interface{}{
			"inserted_ids":   ids,
			"inserted_count": len(res.InsertedIDs),
			"acknowledged":   res.Acknowledged,
		}, nil
	})
}

func (s *service) handleUpdateOne(w http.ResponseWriter, r *http.Request) {
	s.run(w, r, true, func(o *op, args map[string]interface{}) (interface{}, error) {
		coll, err := o.coll(args)
		if err != nil {
			return nil, err
		}
		filter, err := toDoc(args, "filter", false)
		if err != nil {
			return nil, err
		}
		update, err := toDoc(args, "update", true)
		if err != nil {
			return nil, err
		}
		uo := options.UpdateOne()
		if b, ok := boolArg(args, "upsert"); ok {
			uo.SetUpsert(b)
		}
		af, err := arrayFilters(args)
		if err != nil {
			return nil, err
		}
		if af != nil {
			uo.SetArrayFilters(af)
		}
		res, err := coll.UpdateOne(o.ctx, filter, update, uo)
		if err != nil {
			return nil, err
		}
		return updateResultJSON(res)
	})
}

func (s *service) handleUpdateMany(w http.ResponseWriter, r *http.Request) {
	s.run(w, r, true, func(o *op, args map[string]interface{}) (interface{}, error) {
		coll, err := o.coll(args)
		if err != nil {
			return nil, err
		}
		filter, err := toDoc(args, "filter", false)
		if err != nil {
			return nil, err
		}
		update, err := toDoc(args, "update", true)
		if err != nil {
			return nil, err
		}
		uo := options.UpdateMany()
		if b, ok := boolArg(args, "upsert"); ok {
			uo.SetUpsert(b)
		}
		af, err := arrayFilters(args)
		if err != nil {
			return nil, err
		}
		if af != nil {
			uo.SetArrayFilters(af)
		}
		res, err := coll.UpdateMany(o.ctx, filter, update, uo)
		if err != nil {
			return nil, err
		}
		return updateResultJSON(res)
	})
}

func (s *service) handleReplaceOne(w http.ResponseWriter, r *http.Request) {
	s.run(w, r, true, func(o *op, args map[string]interface{}) (interface{}, error) {
		coll, err := o.coll(args)
		if err != nil {
			return nil, err
		}
		filter, err := toDoc(args, "filter", false)
		if err != nil {
			return nil, err
		}
		rep, err := toDoc(args, "replacement", true)
		if err != nil {
			return nil, err
		}
		if len(rep) == 0 {
			return nil, fail(errBadRequest, "replacement 不能是空对象")
		}
		ro := options.Replace()
		if b, ok := boolArg(args, "upsert"); ok {
			ro.SetUpsert(b)
		}
		res, err := coll.ReplaceOne(o.ctx, filter, rep, ro)
		if err != nil {
			return nil, err
		}
		return updateResultJSON(res)
	})
}

func (s *service) handleDeleteOne(w http.ResponseWriter, r *http.Request) {
	s.run(w, r, true, func(o *op, args map[string]interface{}) (interface{}, error) {
		coll, err := o.coll(args)
		if err != nil {
			return nil, err
		}
		filter, err := toDoc(args, "filter", false)
		if err != nil {
			return nil, err
		}
		res, err := coll.DeleteOne(o.ctx, filter)
		if err != nil {
			return nil, err
		}
		return map[string]interface{}{"deleted_count": res.DeletedCount, "acknowledged": res.Acknowledged}, nil
	})
}

func (s *service) handleDeleteMany(w http.ResponseWriter, r *http.Request) {
	s.run(w, r, true, func(o *op, args map[string]interface{}) (interface{}, error) {
		coll, err := o.coll(args)
		if err != nil {
			return nil, err
		}
		filter, err := toDoc(args, "filter", false)
		if err != nil {
			return nil, err
		}
		res, err := coll.DeleteMany(o.ctx, filter)
		if err != nil {
			return nil, err
		}
		return map[string]interface{}{"deleted_count": res.DeletedCount, "acknowledged": res.Acknowledged}, nil
	})
}

// findOneAndX 三兄弟：默认返回**改前**的文档（MongoDB 原生语义），
// 要改后的传 return_document="after"。
func (s *service) handleFindOneAndUpdate(w http.ResponseWriter, r *http.Request) {
	s.run(w, r, true, func(o *op, args map[string]interface{}) (interface{}, error) {
		coll, err := o.coll(args)
		if err != nil {
			return nil, err
		}
		filter, err := toDoc(args, "filter", false)
		if err != nil {
			return nil, err
		}
		update, err := toDoc(args, "update", true)
		if err != nil {
			return nil, err
		}
		fo := options.FindOneAndUpdate()
		if fo, err = applyOneOpts(args, fo); err != nil {
			return nil, err
		}
		var doc bson.D
		err = coll.FindOneAndUpdate(o.ctx, filter, update, fo).Decode(&doc)
		if err != nil {
			if errors.Is(err, mongo.ErrNoDocuments) {
				return map[string]interface{}{"document": nil}, nil
			}
			return nil, err
		}
		return oneDocResult(doc)
	})
}

func (s *service) handleFindOneAndReplace(w http.ResponseWriter, r *http.Request) {
	s.run(w, r, true, func(o *op, args map[string]interface{}) (interface{}, error) {
		coll, err := o.coll(args)
		if err != nil {
			return nil, err
		}
		filter, err := toDoc(args, "filter", false)
		if err != nil {
			return nil, err
		}
		rep, err := toDoc(args, "replacement", true)
		if err != nil {
			return nil, err
		}
		fo := options.FindOneAndReplace()
		if fo, err = applyOneOpts(args, fo); err != nil {
			return nil, err
		}
		var doc bson.D
		err = coll.FindOneAndReplace(o.ctx, filter, rep, fo).Decode(&doc)
		if err != nil {
			if errors.Is(err, mongo.ErrNoDocuments) {
				return map[string]interface{}{"document": nil}, nil
			}
			return nil, err
		}
		return oneDocResult(doc)
	})
}

func (s *service) handleFindOneAndDelete(w http.ResponseWriter, r *http.Request) {
	s.run(w, r, true, func(o *op, args map[string]interface{}) (interface{}, error) {
		coll, err := o.coll(args)
		if err != nil {
			return nil, err
		}
		filter, err := toDoc(args, "filter", false)
		if err != nil {
			return nil, err
		}
		fo := options.FindOneAndDelete()
		if p, err := toDoc(args, "projection", false); err != nil {
			return nil, err
		} else if len(p) > 0 {
			fo.SetProjection(p)
		}
		if so, err := toDoc(args, "sort", false); err != nil {
			return nil, err
		} else if len(so) > 0 {
			fo.SetSort(so)
		}
		var doc bson.D
		err = coll.FindOneAndDelete(o.ctx, filter, fo).Decode(&doc)
		if err != nil {
			if errors.Is(err, mongo.ErrNoDocuments) {
				return map[string]interface{}{"document": nil}, nil
			}
			return nil, err
		}
		return oneDocResult(doc)
	})
}

// ── 批量写 ──

// bulk_write 是本侧车提供的**单次调用内**原子批量写；operations 形如：
//
//	[{"type":"insert_one","document":{...}},
//	 {"type":"update_one","filter":{...},"update":{...},"upsert":true},
//	 {"type":"delete_many","filter":{...}}, ...]
//
// 跨调用的原子性做不到（无状态侧车没有跨请求 session），这是刻意的能力边界。
func (s *service) handleBulkWrite(w http.ResponseWriter, r *http.Request) {
	s.run(w, r, true, func(o *op, args map[string]interface{}) (interface{}, error) {
		coll, err := o.coll(args)
		if err != nil {
			return nil, err
		}
		arr, ok := args["operations"].([]interface{})
		if !ok || len(arr) == 0 {
			return nil, fail(errBadRequest, "operations 必填（非空数组）")
		}
		if len(arr) > maxBatch {
			return nil, fail(errBadRequest, "operations 最多 %d 条", maxBatch)
		}
		models := make([]mongo.WriteModel, 0, len(arr))
		for i, e := range arr {
			m, err := buildWriteModel(e)
			if err != nil {
				return nil, fail(errBadRequest, "operations[%d]: %v", i, err)
			}
			models = append(models, m)
		}
		bo := options.BulkWrite()
		if b, ok := boolArg(args, "ordered"); ok {
			bo.SetOrdered(b)
		}
		res, err := coll.BulkWrite(o.ctx, models, bo)
		if err != nil {
			return nil, err
		}
		upserted, err := toJSON(res.UpsertedIDs)
		if err != nil {
			return nil, err
		}
		return map[string]interface{}{
			"inserted_count": res.InsertedCount,
			"matched_count":  res.MatchedCount,
			"modified_count": res.ModifiedCount,
			"deleted_count":  res.DeletedCount,
			"upserted_count": res.UpsertedCount,
			"upserted_ids":   upserted,
			"acknowledged":   res.Acknowledged,
		}, nil
	})
}

func buildWriteModel(e interface{}) (mongo.WriteModel, error) {
	obj, ok := e.(map[string]interface{})
	if !ok {
		return nil, fail(errBadRequest, "每项必须是对象且带 type")
	}
	typ := strArg(obj, "type")
	if typ == "" {
		return nil, fail(errBadRequest, "type 必填（insert_one/update_one/update_many/replace_one/delete_one/delete_many）")
	}
	switch typ {
	case "insert_one":
		d, err := toDoc(obj, "document", true)
		if err != nil {
			return nil, err
		}
		return mongo.NewInsertOneModel().SetDocument(d), nil
	case "update_one":
		f, err := toDoc(obj, "filter", false)
		if err != nil {
			return nil, err
		}
		u, err := toDoc(obj, "update", true)
		if err != nil {
			return nil, err
		}
		m := mongo.NewUpdateOneModel().SetFilter(f).SetUpdate(u)
		if b, ok := boolArg(obj, "upsert"); ok {
			m.SetUpsert(b)
		}
		return m, nil
	case "update_many":
		f, err := toDoc(obj, "filter", false)
		if err != nil {
			return nil, err
		}
		u, err := toDoc(obj, "update", true)
		if err != nil {
			return nil, err
		}
		m := mongo.NewUpdateManyModel().SetFilter(f).SetUpdate(u)
		if b, ok := boolArg(obj, "upsert"); ok {
			m.SetUpsert(b)
		}
		return m, nil
	case "replace_one":
		f, err := toDoc(obj, "filter", false)
		if err != nil {
			return nil, err
		}
		r, err := toDoc(obj, "replacement", true)
		if err != nil {
			return nil, err
		}
		m := mongo.NewReplaceOneModel().SetFilter(f).SetReplacement(r)
		if b, ok := boolArg(obj, "upsert"); ok {
			m.SetUpsert(b)
		}
		return m, nil
	case "delete_one":
		f, err := toDoc(obj, "filter", false)
		if err != nil {
			return nil, err
		}
		return mongo.NewDeleteOneModel().SetFilter(f), nil
	case "delete_many":
		f, err := toDoc(obj, "filter", false)
		if err != nil {
			return nil, err
		}
		return mongo.NewDeleteManyModel().SetFilter(f), nil
	default:
		return nil, fail(errBadRequest, "未知 type %q", typ)
	}
}

// ── 索引 ──

func (s *service) handleCreateIndex(w http.ResponseWriter, r *http.Request) {
	s.run(w, r, true, func(o *op, args map[string]interface{}) (interface{}, error) {
		coll, err := o.coll(args)
		if err != nil {
			return nil, err
		}
		keys, err := toDoc(args, "keys", true)
		if err != nil {
			return nil, err
		}
		if len(keys) == 0 {
			return nil, fail(errBadRequest, "keys 不能是空对象（如 {name: 1} / {created_at: -1}）")
		}
		io := options.Index()
		if n := strArg(args, "name"); n != "" {
			io.SetName(n)
		}
		if b, ok := boolArg(args, "unique"); ok {
			io.SetUnique(b)
		}
		if b, ok := boolArg(args, "sparse"); ok {
			io.SetSparse(b)
		}
		if e, ok := intArg(args, "expire_after_seconds"); ok && e >= 0 {
			io.SetExpireAfterSeconds(int32(e))
		}
		name, err := coll.Indexes().CreateOne(o.ctx, mongo.IndexModel{Keys: keys, Options: io})
		if err != nil {
			return nil, err
		}
		return map[string]interface{}{"name": name}, nil
	})
}

func (s *service) handleDropIndex(w http.ResponseWriter, r *http.Request) {
	s.run(w, r, true, func(o *op, args map[string]interface{}) (interface{}, error) {
		coll, err := o.coll(args)
		if err != nil {
			return nil, err
		}
		name := strArg(args, "name")
		if name == "" {
			// 不给名字就删不掉：MongoDB 的 dropIndexes 要索引名，而名字是服务端按 key
			// 生成的（如 name_1）。这里不瞎猜，直接指调用方去 list_indexes 取。
			return nil, fail(errBadRequest, "name 必填（不知道名字先调 list_indexes）")
		}
		if err := coll.Indexes().DropOne(o.ctx, name); err != nil {
			return nil, err
		}
		return map[string]interface{}{"ok": true, "name": name}, nil
	})
}

func (s *service) handleListIndexes(w http.ResponseWriter, r *http.Request) {
	s.run(w, r, true, func(o *op, args map[string]interface{}) (interface{}, error) {
		coll, err := o.coll(args)
		if err != nil {
			return nil, err
		}
		cur, err := coll.Indexes().List(o.ctx)
		if err != nil {
			return nil, err
		}
		defer cur.Close(o.ctx)
		docs, err := drainCursor(o.ctx, cur, maxBatch)
		if err != nil {
			return nil, err
		}
		out, err := toJSONSlice(docs)
		if err != nil {
			return nil, err
		}
		return map[string]interface{}{"indexes": out, "count": len(out)}, nil
	})
}

// ── 库 / 集合管理 ──

func (s *service) handleCreateCollection(w http.ResponseWriter, r *http.Request) {
	s.run(w, r, true, func(o *op, args map[string]interface{}) (interface{}, error) {
		name, err := checkCollName(args)
		if err != nil {
			return nil, err
		}
		co := options.CreateCollection()
		if b, ok := boolArg(args, "capped"); ok {
			co.SetCapped(b)
		}
		if n, ok := intArg(args, "size_bytes"); ok && n > 0 {
			co.SetSizeInBytes(n)
		}
		if n, ok := intArg(args, "max_docs"); ok && n > 0 {
			co.SetMaxDocuments(n)
		}
		if err := o.db.CreateCollection(o.ctx, name, co); err != nil {
			return nil, err
		}
		return map[string]interface{}{"ok": true, "collection": name}, nil
	})
}

func (s *service) handleDropCollection(w http.ResponseWriter, r *http.Request) {
	s.run(w, r, true, func(o *op, args map[string]interface{}) (interface{}, error) {
		name, err := checkCollName(args)
		if err != nil {
			return nil, err
		}
		if err := o.db.Collection(name).Drop(o.ctx); err != nil {
			return nil, err
		}
		return map[string]interface{}{"ok": true, "collection": name}, nil
	})
}

// rename_collection 走 admin 库的 renameCollection 命令：
// 同库改名时新旧全名都要带库名前缀（`<db>.<coll>`），这点极易写错，故在此显式拼。
func (s *service) handleRenameCollection(w http.ResponseWriter, r *http.Request) {
	s.run(w, r, true, func(o *op, args map[string]interface{}) (interface{}, error) {
		old, err := checkCollName(args)
		if err != nil {
			return nil, err
		}
		nu := strArg(args, "new_name")
		if nu == "" {
			return nil, fail(errBadRequest, "new_name 必填")
		}
		dbName := o.db.Name()
		cmd := bson.D{
			{Key: "renameCollection", Value: dbName + "." + old},
			{Key: "to", Value: dbName + "." + nu},
		}
		if b, ok := boolArg(args, "drop_target"); ok {
			cmd = append(cmd, bson.E{Key: "dropTarget", Value: b})
		}
		var out bson.D
		if err := o.admin().RunCommand(o.ctx, cmd).Decode(&out); err != nil {
			return nil, err
		}
		return map[string]interface{}{"ok": true, "from": old, "to": nu}, nil
	})
}

func (s *service) handleListCollections(w http.ResponseWriter, r *http.Request) {
	s.run(w, r, true, func(o *op, args map[string]interface{}) (interface{}, error) {
		filter, err := toDoc(args, "filter", false)
		if err != nil {
			return nil, err
		}
		names, err := o.db.ListCollectionNames(o.ctx, filter)
		if err != nil {
			return nil, err
		}
		if names == nil {
			names = []string{}
		}
		return map[string]interface{}{"collections": names, "count": len(names)}, nil
	})
}

func (s *service) handleListDatabases(w http.ResponseWriter, r *http.Request) {
	s.run(w, r, false, func(o *op, args map[string]interface{}) (interface{}, error) {
		cmd := bson.D{{Key: "listDatabases", Value: 1}}
		if b, ok := boolArg(args, "name_only"); ok {
			cmd = append(cmd, bson.E{Key: "nameOnly", Value: b})
		}
		var out bson.D
		if err := o.admin().RunCommand(o.ctx, cmd).Decode(&out); err != nil {
			return nil, err
		}
		j, err := toJSON(out)
		if err != nil {
			return nil, err
		}
		m, _ := j.(map[string]interface{})
		dbs := []interface{}{}
		if m != nil {
			if arr, ok := m["databases"].([]interface{}); ok {
				dbs = arr
			}
		}
		return map[string]interface{}{"databases": dbs, "count": len(dbs)}, nil
	})
}

func (s *service) handleStats(w http.ResponseWriter, r *http.Request) {
	s.run(w, r, true, func(o *op, args map[string]interface{}) (interface{}, error) {
		name, err := checkCollName(args)
		if err != nil {
			return nil, err
		}
		var out bson.D
		if err := o.db.RunCommand(o.ctx, bson.D{{Key: "collStats", Value: name}}).Decode(&out); err != nil {
			return nil, err
		}
		j, err := toJSON(out)
		if err != nil {
			return nil, err
		}
		return map[string]interface{}{"stats": j}, nil
	})
}

// run_command 是逃生口：任何本侧车没包一层的数据库命令都能走它。
// 有它就不需要为 collStats / createUser / explain 之类逐个加方法——
// 但也正因为它能跑任意命令，Tool 声明 `ext.mongo` 就等于把该库的命令权限给了它。
func (s *service) handleRunCommand(w http.ResponseWriter, r *http.Request) {
	s.run(w, r, true, func(o *op, args map[string]interface{}) (interface{}, error) {
		cmd, err := toDoc(args, "command", true)
		if err != nil {
			return nil, err
		}
		if len(cmd) == 0 {
			return nil, fail(errBadRequest, "command 不能是空对象")
		}
		var out bson.D
		if err := o.db.RunCommand(o.ctx, cmd).Decode(&out); err != nil {
			return nil, err
		}
		j, err := toJSON(out)
		if err != nil {
			return nil, err
		}
		return map[string]interface{}{"result": j}, nil
	})
}

// watch 是有界的变更流：等最多 max_wait_ms，把这段时间里到达的事件一次性返回。
//
// 为什么不做常驻订阅：侧车每次调用彼此独立，没有跨请求的 cursor 句柄，
// 也没有「把事件回推给调用方」的通道（那要走 ctx.subscribe，是另一条机制）。
// 所以这里提供的是**轮询式**的变更读取；要真正的流式订阅应走 Flow + 定时触发。
// 另外：变更流要求**副本集**，单机 mongod 会直接报 mongo_error（不静默降级）。
func (s *service) handleWatch(w http.ResponseWriter, r *http.Request) {
	s.run(w, r, true, func(o *op, args map[string]interface{}) (interface{}, error) {
		coll, err := o.coll(args)
		if err != nil {
			return nil, err
		}
		pipeline, err := toArray(args, "pipeline", false)
		if err != nil {
			return nil, err
		}
		wait := time.Duration(defaultWaitMs) * time.Millisecond
		if w, ok := intArg(args, "max_wait_ms"); ok && w > 0 {
			if w > maxWaitMs {
				return nil, fail(errBadRequest, "max_wait_ms 不能超过 %d（内核调侧车的客户端超时是 10s）", maxWaitMs)
			}
			wait = time.Duration(w) * time.Millisecond
		}
		size := int64(defaultBatch)
		if b, ok := intArg(args, "batch"); ok && b > 0 {
			size = b
			if size > maxBatch {
				size = maxBatch
			}
		}
		cs, err := coll.Watch(o.ctx, pipeline, options.ChangeStream().SetBatchSize(int32(size)))
		if err != nil {
			return nil, err
		}
		defer cs.Close(o.ctx)

		// Next 会阻塞等事件，所以另开一个「等待窗口」的子 ctx 来收口，
		// 而不是让它一路卡到操作超时——否则每次 watch 都要耗满 timeout_ms。
		sub, cancel := context.WithTimeout(o.ctx, wait)
		defer cancel()
		events := []bson.D{}
		for int64(len(events)) < size {
			if !cs.Next(sub) {
				break
			}
			var d bson.D
			if err := cs.Decode(&d); err != nil {
				return nil, err
			}
			events = append(events, d)
		}
		// 窗口到期导致的 ctx 错误是预期内的（"这段时间没事件"），不当错误处理。
		if err := cs.Err(); err != nil &&
			!errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
			return nil, err
		}
		out, err := toJSONSlice(events)
		if err != nil {
			return nil, err
		}
		return map[string]interface{}{"events": out, "count": len(out), "waited_ms": wait.Milliseconds()}, nil
	})
}

// ── 通用辅助 ──

// drainCursor 把游标读完并**硬上限** max 条。
// 聚合结果没有 limit 参数可依赖，不加闸的话一次 $unwind 就能把侧车内存打满。
func drainCursor(ctx context.Context, cur *mongo.Cursor, max int64) ([]bson.D, error) {
	docs := []bson.D{}
	for int64(len(docs)) < max && cur.Next(ctx) {
		var d bson.D
		if err := cur.Decode(&d); err != nil {
			return nil, err
		}
		docs = append(docs, d)
	}
	if err := cur.Err(); err != nil {
		return nil, err
	}
	return docs, nil
}

func updateResultJSON(res *mongo.UpdateResult) (interface{}, error) {
	id, err := toJSON(res.UpsertedID)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"matched_count":  res.MatchedCount,
		"modified_count": res.ModifiedCount,
		"upserted_count": res.UpsertedCount,
		"upserted_id":    id,
		"acknowledged":   res.Acknowledged,
	}, nil
}

func oneDocResult(doc bson.D) (interface{}, error) {
	out, err := toJSON(doc)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{"document": out}, nil
}

func arrayFilters(args map[string]interface{}) ([]interface{}, error) {
	v, ok := args["array_filters"]
	if !ok || v == nil {
		return nil, nil
	}
	arr, ok := v.([]interface{})
	if !ok || len(arr) == 0 {
		return nil, fail(errBadRequest, "array_filters 必须是非空数组")
	}
	out := make([]interface{}, 0, len(arr))
	for i, e := range arr {
		var d bson.D
		if err := toBSON(e, &d); err != nil {
			return nil, fail(errBadRequest, "array_filters[%d]: %v", i, err)
		}
		out = append(out, d)
	}
	return out, nil
}

// applyOneOpts 给 find_one_and_update / find_one_and_replace 套公共选项。
// 用泛型约束到「有这四个 setter 的 builder」，避免两处复制粘贴后漂移。
// applyOneOpts 给 find_one_and_update / find_one_and_replace 套公共选项。
// 用泛型约束到「有这四个 setter 的 builder」，避免两处复制粘贴后漂移。
//
// 注意：返回 (T, error) 而非只改入参——fo 是值类型，若只改入参、调用方拿到的还是旧副本，
// upsert/projection/sort/return_document 会被静默吞掉（这正是 SA4006 抓出的真 bug）。
func applyOneOpts[T interface {
	SetUpsert(bool) T
	SetProjection(interface{}) T
	SetSort(interface{}) T
	SetReturnDocument(options.ReturnDocument) T
}](args map[string]interface{}, fo T) (T, error) {
	if b, ok := boolArg(args, "upsert"); ok {
		fo = fo.SetUpsert(b)
	}
	if p, err := toDoc(args, "projection", false); err != nil {
		return fo, err
	} else if len(p) > 0 {
		fo = fo.SetProjection(p)
	}
	if s, err := toDoc(args, "sort", false); err != nil {
		return fo, err
	} else if len(s) > 0 {
		fo = fo.SetSort(s)
	}
	// MongoDB 默认是 before（返回改之前的文档）；要改之后的显式说 after。
	if rd := strArg(args, "return_document"); rd == "after" {
		fo = fo.SetReturnDocument(options.After)
	} else if rd != "" && rd != "before" {
		return fo, fail(errBadRequest, "return_document 只能是 before 或 after（收到 %q）", rd)
	}
	return fo, nil
}

func decodeArgs(r *http.Request) (map[string]interface{}, error) {
	var req struct {
		Args map[string]interface{} `json:"args"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return nil, err
	}
	return req.Args, nil
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code, format string, a ...interface{}) {
	writeJSON(w, map[string]interface{}{"error": code + ": " + fmt.Sprintf(format, a...)})
}

func writeCodedErr(w http.ResponseWriter, err error) {
	if ce, ok := err.(*codedError); ok {
		writeJSON(w, map[string]interface{}{"error": ce.Error()})
		return
	}
	writeErr(w, errBadRequest, "%v", err)
}

func main() {
	// 自描述（2026-10-09）：`--describe` / GEAR_DESCRIBE=1 时打印方法清单与参数 schema 后退出，
	// 不进下面的常规启动路径。描述与 handler 同包（见 describe.go），extensions.yaml
	// 里写 `describe: true` 即可省掉那一份手工同步的 schemas。
	describe.EmitIfRequested(mongoDescribe())

	addr := os.Getenv("COORD_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8104"
	}
	svc := &service{}
	c := coord.New(coord.Options{
		CheckInterval: 5 * time.Second,
		TTL:           15 * time.Second,
		OnEmpty: func() {
			log.Printf("占用列表为空，mongo-sidecar 自动退出")
			os.Exit(0)
		},
	})
	log.Printf("mongo-sidecar 协调监听 %s", addr)
	if err := c.Serve(addr, svc.Handlers()); err != nil {
		if coord.IsAddrInUse(err) {
			log.Printf("端口 %s 已被占用，mongo-sidecar 退让退出", addr)
			os.Exit(coord.ExitAddrInUse)
		}
		log.Fatalf("mongo-sidecar 异常退出: %v", err)
	}
}
