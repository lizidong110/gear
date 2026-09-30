package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"gear/internal/sidecar/coord"
)

const (
	defaultRedisPort = 6379
	defaultRedisDB   = 0
	maxKeyLen        = 2048
	maxTokenLen      = 2048
	maxBatch         = 1000
)

// 错误码（错误响应的 `error` 字段形如 "<code>: <人话>"）。
const (
	errBadRequest   = "bad_request"
	errMissingKey   = "missing_key"
	errBadKey       = "bad_key"
	errMissingHost  = "missing_host"
	errBadType      = "bad_type"
	errRedisUnreach = "redis_unreachable"
)

// nowFn 是唯一的时间入口，测试里替换成固定时间后才能对延迟队列的 score 做断言。
var nowFn = time.Now

// codedError 带稳定机器码的错误（调用方靠它分支，中文文案随时会改）。
type codedError struct{ code, msg string }

func (e *codedError) Error() string { return e.code + ": " + e.msg }

func fail(code, format string, a ...interface{}) error {
	return &codedError{code, fmt.Sprintf(format, a...)}
}

// ── 配置解析 ──

type redisConfig struct {
	addr     string // host:port
	password string
	db       int
	pool     *Pool
}

func parseConfig(args map[string]interface{}) (redisConfig, error) {
	host := strArg(args, "host")
	if host == "" {
		return redisConfig{}, fail(errMissingHost, "host 必填（如 127.0.0.1 / redis.example.com）")
	}
	port := defaultRedisPort
	if p, ok := intArg(args, "port"); ok && p > 0 {
		port = int(p)
	}
	db := defaultRedisDB
	if d, ok := intArg(args, "db"); ok && d >= 0 {
		db = int(d)
	}
	pw := strArg(args, "password")
	addr := fmt.Sprintf("%s:%d", host, port)
	return redisConfig{addr: addr, password: pw, db: db, pool: getPool(addr, pw)}, nil
}

// 连接池缓存：侧车长驻，按 addr+password 复用 Pool（否则每请求新建 Pool、idle 永不复用）。
var (
	poolMu    sync.Mutex
	poolCache = map[string]*Pool{}
)

func getPool(addr, password string) *Pool {
	key := addr + "\x00" + password
	poolMu.Lock()
	if p, ok := poolCache[key]; ok {
		poolMu.Unlock()
		return p
	}
	p := NewPool(addr, password)
	poolCache[key] = p
	poolMu.Unlock()
	return p
}

// normalizeKey 校验 redis key。
//
// redis 的 key 不是文件路径（与 oss 的 S3 key 不同），所以**不做** `..`/前导`/`/反斜杠那套
// 路径语义拒绝；但必须挡掉会在边界处理上出错的非法字符（\0 / \n / \r），并限长。
func normalizeKey(key string) (string, error) {
	return checkToken(key)
}

// normalizeToken 校验 field / member / payload 等非 key 字符串项（语义同 normalizeKey）。
func checkToken(s string) (string, error) {
	if s == "" {
		return "", fail(errMissingKey, "不能为空字符串")
	}
	if len(s) > maxTokenLen {
		return "", fail(errBadKey, "太长（>%d 字节）", maxTokenLen)
	}
	if strings.ContainsRune(s, 0) || strings.ContainsRune(s, '\n') || strings.ContainsRune(s, '\r') {
		return "", fail(errBadKey, "含非法控制字符（\\0/\\n/\\r）")
	}
	return s, nil
}

// ── 参数解析辅助 ──

func strSlice(m map[string]interface{}, k string) ([]string, bool) {
	v, ok := m[k]
	if !ok {
		return nil, false
	}
	arr, ok := v.([]interface{})
	if !ok {
		return nil, false
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		switch t := e.(type) {
		case string:
			out = append(out, t)
		case float64:
			out = append(out, strconv.FormatInt(int64(t), 10))
		default:
			return nil, false
		}
	}
	return out, true
}

func mapArg(m map[string]interface{}, k string) (map[string]string, bool) {
	v, ok := m[k]
	if !ok {
		return nil, false
	}
	obj, ok := v.(map[string]interface{})
	if !ok {
		return nil, false
	}
	out := make(map[string]string, len(obj))
	for kk, vv := range obj {
		switch t := vv.(type) {
		case string:
			out[kk] = t
		case float64:
			out[kk] = strconv.FormatInt(int64(t), 10)
		case bool:
			out[kk] = strconv.FormatBool(t)
		default:
			return nil, false
		}
	}
	return out, true
}

func objSlice(m map[string]interface{}, k string) ([]map[string]interface{}, bool) {
	v, ok := m[k]
	if !ok {
		return nil, false
	}
	arr, ok := v.([]interface{})
	if !ok {
		return nil, false
	}
	out := make([]map[string]interface{}, 0, len(arr))
	for _, e := range arr {
		obj, ok := e.(map[string]interface{})
		if !ok {
			return nil, false
		}
		out = append(out, obj)
	}
	return out, true
}

// floatArg 取浮点数参数（JSON number，用于 zset 的 score）。
func floatArg(m map[string]interface{}, k string) (float64, bool) {
	switch v := m[k].(type) {
	case float64:
		return v, true
	case string:
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			return f, true
		}
	}
	return 0, false
}

// ── service ──

type service struct{}

func (s *service) Handlers() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		// KV
		"/set": s.handleSet, "/get": s.handleGet, "/del": s.handleDel,
		"/expire": s.handleExpire, "/ttl": s.handleTTL, "/exists": s.handleExists, "/incr": s.handleIncr,
		// Hash
		"/hset": s.handleHSet, "/hget": s.handleHGet, "/hgetall": s.handleHGetAll, "/hdel": s.handleHDel,
		// List
		"/lpush": s.handleLPush, "/rpush": s.handleRPush, "/lpop": s.handleLPop, "/rpop": s.handleRPop,
		"/llen": s.handleLLen, "/lrange": s.handleLRange,
		// Set
		"/sadd": s.handleSAdd, "/smembers": s.handleSMembers, "/srem": s.handleSRem, "/scard": s.handleSCard,
		// ZSet
		"/zadd": s.handleZAdd, "/zrange": s.handleZRange, "/zscore": s.handleZScore, "/zrem": s.handleZRem,
		// Lock
		"/lock": s.handleLock, "/unlock": s.handleUnlock,
		// Delay queue
		"/enqueue": s.handleEnqueue, "/dequeue": s.handleDequeue,
		// introspection
		"/caps": s.handleCaps,
	}
}

func (s *service) handleCaps(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]interface{}{"result": map[string]interface{}{
		"methods": []string{
			"set", "get", "del", "expire", "ttl", "exists", "incr",
			"hset", "hget", "hgetall", "hdel",
			"lpush", "rpush", "lpop", "rpop", "llen", "lrange",
			"sadd", "smembers", "srem", "scard",
			"zadd", "zrange", "zscore", "zrem",
			"lock", "unlock", "enqueue", "dequeue",
		},
	}})
}

// begin 解 args + 解析配置（key 校验由各 handler 自行做，因为部分方法无 key 或 key 在数组里）。
func begin(w http.ResponseWriter, r *http.Request) (map[string]interface{}, redisConfig, bool) {
	args, err := decodeArgs(r)
	if err != nil {
		writeErr(w, errBadRequest, "请求体不是合法 JSON: %v", err)
		return nil, redisConfig{}, false
	}
	cfg, err := parseConfig(args)
	if err != nil {
		writeCodedErr(w, err)
		return nil, redisConfig{}, false
	}
	return args, cfg, true
}

// handleResult 统一把 With 返回的错误转成带码响应；succ 是非错误时如何写结果。
func (s *service) handleResult(w http.ResponseWriter, err error, succ func()) {
	if err != nil {
		switch e := err.(type) {
		case *codedError:
			writeCodedErr(w, e)
		case *RedisError:
			writeCodedErr(w, fail("redis_error", "%s", e.msg))
		default:
			writeCodedErr(w, fail(errRedisUnreach, "redis 连接/读写失败: %v", err))
		}
		return
	}
	succ()
}

// ── KV ──

func (s *service) handleSet(w http.ResponseWriter, r *http.Request) {
	args, cfg, ok := begin(w, r)
	if !ok {
		return
	}
	key, err := normalizeKey(strArg(args, "key"))
	if err != nil {
		writeCodedErr(w, err)
		return
	}
	val := strArg(args, "value")
	if _, ok := args["value"]; !ok {
		writeCodedErr(w, fail(errBadRequest, "value 必填（对象内容以字符串传，Lua 侧无字节类型）"))
		return
	}
	cmd := []interface{}{"SET", key, val}
	if ex, ok := intArg(args, "ex"); ok && ex > 0 {
		cmd = append(cmd, "EX", ex)
	}
	var res interface{}
	s.handleResult(w, cfg.pool.With(func(c *Conn) error {
		v, e := c.Exec(cmd...)
		if e != nil {
			return e
		}
		res = v
		return nil
	}), func() {
		writeJSON(w, map[string]interface{}{"result": map[string]interface{}{"ok": asStringOK(res)}})
	})
}

func (s *service) handleGet(w http.ResponseWriter, r *http.Request) {
	args, cfg, ok := begin(w, r)
	if !ok {
		return
	}
	key, err := normalizeKey(strArg(args, "key"))
	if err != nil {
		writeCodedErr(w, err)
		return
	}
	var res interface{}
	s.handleResult(w, cfg.pool.With(func(c *Conn) error {
		v, e := c.Exec("GET", key)
		if e != nil {
			return e
		}
		res = v
		return nil
	}), func() {
		writeJSON(w, map[string]interface{}{"result": map[string]interface{}{"value": res}})
	})
}

func (s *service) handleDel(w http.ResponseWriter, r *http.Request) {
	args, cfg, ok := begin(w, r)
	if !ok {
		return
	}
	keys, _ := strSlice(args, "keys")
	if len(keys) == 0 {
		if k := strArg(args, "key"); k != "" {
			keys = []string{k}
		}
	}
	if len(keys) == 0 {
		writeCodedErr(w, fail(errBadRequest, "keys（或 key）必填"))
		return
	}
	for i, k := range keys {
		if _, err := normalizeKey(k); err != nil {
			writeCodedErr(w, err)
			return
		}
		keys[i] = k
	}
	var res interface{}
	s.handleResult(w, cfg.pool.With(func(c *Conn) error {
		v, e := c.Exec(append([]interface{}{"DEL"}, toIface(keys)...)...)
		if e != nil {
			return e
		}
		res = v
		return nil
	}), func() {
		n, _ := asInt(res)
		writeJSON(w, map[string]interface{}{"result": map[string]interface{}{"deleted": n}})
	})
}

func (s *service) handleExpire(w http.ResponseWriter, r *http.Request) {
	args, cfg, ok := begin(w, r)
	if !ok {
		return
	}
	key, err := normalizeKey(strArg(args, "key"))
	if err != nil {
		writeCodedErr(w, err)
		return
	}
	sec, ok := intArg(args, "seconds")
	if !ok || sec <= 0 {
		writeCodedErr(w, fail(errBadRequest, "seconds 必填且 > 0"))
		return
	}
	var res interface{}
	s.handleResult(w, cfg.pool.With(func(c *Conn) error {
		v, e := c.Exec("EXPIRE", key, sec)
		if e != nil {
			return e
		}
		res = v
		return nil
	}), func() {
		n, _ := asInt(res)
		writeJSON(w, map[string]interface{}{"result": map[string]interface{}{"set": n == 1}})
	})
}

func (s *service) handleTTL(w http.ResponseWriter, r *http.Request) {
	args, cfg, ok := begin(w, r)
	if !ok {
		return
	}
	key, err := normalizeKey(strArg(args, "key"))
	if err != nil {
		writeCodedErr(w, err)
		return
	}
	var res interface{}
	s.handleResult(w, cfg.pool.With(func(c *Conn) error {
		v, e := c.Exec("TTL", key)
		if e != nil {
			return e
		}
		res = v
		return nil
	}), func() {
		n, _ := asInt(res)
		writeJSON(w, map[string]interface{}{"result": map[string]interface{}{"ttl": n}})
	})
}

func (s *service) handleExists(w http.ResponseWriter, r *http.Request) {
	args, cfg, ok := begin(w, r)
	if !ok {
		return
	}
	keys, _ := strSlice(args, "keys")
	if len(keys) == 0 {
		if k := strArg(args, "key"); k != "" {
			keys = []string{k}
		}
	}
	if len(keys) == 0 {
		writeCodedErr(w, fail(errBadRequest, "keys（或 key）必填"))
		return
	}
	for i, k := range keys {
		if _, err := normalizeKey(k); err != nil {
			writeCodedErr(w, err)
			return
		}
		keys[i] = k
	}
	var res interface{}
	s.handleResult(w, cfg.pool.With(func(c *Conn) error {
		v, e := c.Exec(append([]interface{}{"EXISTS"}, toIface(keys)...)...)
		if e != nil {
			return e
		}
		res = v
		return nil
	}), func() {
		n, _ := asInt(res)
		writeJSON(w, map[string]interface{}{"result": map[string]interface{}{"exists": n}})
	})
}

func (s *service) handleIncr(w http.ResponseWriter, r *http.Request) {
	args, cfg, ok := begin(w, r)
	if !ok {
		return
	}
	key, err := normalizeKey(strArg(args, "key"))
	if err != nil {
		writeCodedErr(w, err)
		return
	}
	by := int64(1)
	if b, ok := intArg(args, "by"); ok {
		by = b
	}
	var res interface{}
	s.handleResult(w, cfg.pool.With(func(c *Conn) error {
		v, e := c.Exec("INCRBY", key, by)
		if e != nil {
			return e
		}
		res = v
		return nil
	}), func() {
		n, _ := asInt(res)
		writeJSON(w, map[string]interface{}{"result": map[string]interface{}{"value": n}})
	})
}

// ── Hash ──

func (s *service) handleHSet(w http.ResponseWriter, r *http.Request) {
	args, cfg, ok := begin(w, r)
	if !ok {
		return
	}
	key, err := normalizeKey(strArg(args, "key"))
	if err != nil {
		writeCodedErr(w, err)
		return
	}
	// 两种形态：field/value 单对，或 fields 映射
	var pairs []interface{}
	fields, hasMap := mapArg(args, "fields")
	if hasMap {
		if len(fields) == 0 {
			writeCodedErr(w, fail(errBadRequest, "fields 不能为空对象"))
			return
		}
		for f, v := range fields {
			if _, err := checkToken(f); err != nil {
				writeCodedErr(w, err)
				return
			}
			pairs = append(pairs, f, v)
		}
	} else {
		f := strArg(args, "field")
		v := strArg(args, "value")
		if f == "" || !hasKey(args, "value") {
			writeCodedErr(w, fail(errBadRequest, "field 与 value 必填，或给 fields 对象"))
			return
		}
		if _, err := checkToken(f); err != nil {
			writeCodedErr(w, err)
			return
		}
		pairs = append(pairs, f, v)
	}
	var res interface{}
	s.handleResult(w, cfg.pool.With(func(c *Conn) error {
		v, e := c.Exec(append([]interface{}{"HSET", key}, pairs...)...)
		if e != nil {
			return e
		}
		res = v
		return nil
	}), func() {
		n, _ := asInt(res)
		writeJSON(w, map[string]interface{}{"result": map[string]interface{}{"added": n}})
	})
}

func (s *service) handleHGet(w http.ResponseWriter, r *http.Request) {
	args, cfg, ok := begin(w, r)
	if !ok {
		return
	}
	key, err := normalizeKey(strArg(args, "key"))
	if err != nil {
		writeCodedErr(w, err)
		return
	}
	f, err := checkToken(strArg(args, "field"))
	if err != nil {
		writeCodedErr(w, err)
		return
	}
	var res interface{}
	s.handleResult(w, cfg.pool.With(func(c *Conn) error {
		v, e := c.Exec("HGET", key, f)
		if e != nil {
			return e
		}
		res = v
		return nil
	}), func() {
		writeJSON(w, map[string]interface{}{"result": map[string]interface{}{"value": res}})
	})
}

func (s *service) handleHGetAll(w http.ResponseWriter, r *http.Request) {
	args, cfg, ok := begin(w, r)
	if !ok {
		return
	}
	key, err := normalizeKey(strArg(args, "key"))
	if err != nil {
		writeCodedErr(w, err)
		return
	}
	var res interface{}
	s.handleResult(w, cfg.pool.With(func(c *Conn) error {
		v, e := c.Exec("HGETALL", key)
		if e != nil {
			return e
		}
		res = v
		return nil
	}), func() {
		arr, _ := asArray(res)
		fields := make([]map[string]interface{}, 0, len(arr)/2)
		for i := 0; i+1 < len(arr); i += 2 {
			f, _ := asString(arr[i])
			v, _ := asString(arr[i+1])
			fields = append(fields, map[string]interface{}{"field": f, "value": v})
		}
		writeJSON(w, map[string]interface{}{"result": map[string]interface{}{"fields": fields}})
	})
}

func (s *service) handleHDel(w http.ResponseWriter, r *http.Request) {
	args, cfg, ok := begin(w, r)
	if !ok {
		return
	}
	key, err := normalizeKey(strArg(args, "key"))
	if err != nil {
		writeCodedErr(w, err)
		return
	}
	fields, _ := strSlice(args, "fields")
	if len(fields) == 0 {
		if f := strArg(args, "field"); f != "" {
			fields = []string{f}
		}
	}
	if len(fields) == 0 {
		writeCodedErr(w, fail(errBadRequest, "fields（或 field）必填"))
		return
	}
	for i, f := range fields {
		if _, err := checkToken(f); err != nil {
			writeCodedErr(w, err)
			return
		}
		fields[i] = f
	}
	var res interface{}
	s.handleResult(w, cfg.pool.With(func(c *Conn) error {
		v, e := c.Exec(append([]interface{}{"HDEL", key}, toIface(fields)...)...)
		if e != nil {
			return e
		}
		res = v
		return nil
	}), func() {
		n, _ := asInt(res)
		writeJSON(w, map[string]interface{}{"result": map[string]interface{}{"deleted": n}})
	})
}

// ── List ──

func (s *service) handleLPush(w http.ResponseWriter, r *http.Request) { s.handlePush(w, r, "LPUSH") }
func (s *service) handleRPush(w http.ResponseWriter, r *http.Request) { s.handlePush(w, r, "RPUSH") }

func (s *service) handlePush(w http.ResponseWriter, r *http.Request, cmd string) {
	args, cfg, ok := begin(w, r)
	if !ok {
		return
	}
	key, err := normalizeKey(strArg(args, "key"))
	if err != nil {
		writeCodedErr(w, err)
		return
	}
	vals, ok := strSlice(args, "values")
	if !ok || len(vals) == 0 {
		if v := strArg(args, "value"); v != "" {
			vals = []string{v}
		}
	}
	if len(vals) == 0 {
		writeCodedErr(w, fail(errBadRequest, "values（或 value）必填"))
		return
	}
	var res interface{}
	s.handleResult(w, cfg.pool.With(func(c *Conn) error {
		v, e := c.Exec(append([]interface{}{cmd, key}, toIface(vals)...)...)
		if e != nil {
			return e
		}
		res = v
		return nil
	}), func() {
		n, _ := asInt(res)
		writeJSON(w, map[string]interface{}{"result": map[string]interface{}{"length": n}})
	})
}

func (s *service) handleLPop(w http.ResponseWriter, r *http.Request) { s.handlePop(w, r, "LPOP") }
func (s *service) handleRPop(w http.ResponseWriter, r *http.Request) { s.handlePop(w, r, "RPOP") }

func (s *service) handlePop(w http.ResponseWriter, r *http.Request, cmd string) {
	args, cfg, ok := begin(w, r)
	if !ok {
		return
	}
	key, err := normalizeKey(strArg(args, "key"))
	if err != nil {
		writeCodedErr(w, err)
		return
	}
	var res interface{}
	s.handleResult(w, cfg.pool.With(func(c *Conn) error {
		if cnt, ok := intArg(args, "count"); ok && cnt > 1 {
			v, e := c.Exec(cmd, key, cnt)
			if e != nil {
				return e
			}
			res = v
			return nil
		}
		v, e := c.Exec(cmd, key)
		if e != nil {
			return e
		}
		res = v
		return nil
	}), func() {
		if _, ok := asArray(res); ok {
			arr, _ := asArray(res)
			vals := make([]string, 0, len(arr))
			for _, e := range arr {
				if s, ok := asString(e); ok {
					vals = append(vals, s)
				}
			}
			writeJSON(w, map[string]interface{}{"result": map[string]interface{}{"values": vals}})
		} else {
			writeJSON(w, map[string]interface{}{"result": map[string]interface{}{"value": res}})
		}
	})
}

func (s *service) handleLLen(w http.ResponseWriter, r *http.Request) {
	args, cfg, ok := begin(w, r)
	if !ok {
		return
	}
	key, err := normalizeKey(strArg(args, "key"))
	if err != nil {
		writeCodedErr(w, err)
		return
	}
	var res interface{}
	s.handleResult(w, cfg.pool.With(func(c *Conn) error {
		v, e := c.Exec("LLEN", key)
		if e != nil {
			return e
		}
		res = v
		return nil
	}), func() {
		n, _ := asInt(res)
		writeJSON(w, map[string]interface{}{"result": map[string]interface{}{"length": n}})
	})
}

func (s *service) handleLRange(w http.ResponseWriter, r *http.Request) {
	args, cfg, ok := begin(w, r)
	if !ok {
		return
	}
	key, err := normalizeKey(strArg(args, "key"))
	if err != nil {
		writeCodedErr(w, err)
		return
	}
	start, ok := intArg(args, "start")
	if !ok {
		start = 0
	}
	stop, ok := intArg(args, "stop")
	if !ok {
		stop = -1
	}
	var res interface{}
	s.handleResult(w, cfg.pool.With(func(c *Conn) error {
		v, e := c.Exec("LRANGE", key, start, stop)
		if e != nil {
			return e
		}
		res = v
		return nil
	}), func() {
		arr, _ := asArray(res)
		vals := make([]string, 0, len(arr))
		for _, e := range arr {
			if s, ok := asString(e); ok {
				vals = append(vals, s)
			}
		}
		writeJSON(w, map[string]interface{}{"result": map[string]interface{}{"values": vals}})
	})
}

// ── Set ──

func (s *service) handleSAdd(w http.ResponseWriter, r *http.Request) {
	args, cfg, ok := begin(w, r)
	if !ok {
		return
	}
	key, err := normalizeKey(strArg(args, "key"))
	if err != nil {
		writeCodedErr(w, err)
		return
	}
	members, ok := strSlice(args, "members")
	if !ok || len(members) == 0 {
		if m := strArg(args, "member"); m != "" {
			members = []string{m}
		}
	}
	if len(members) == 0 {
		writeCodedErr(w, fail(errBadRequest, "members（或 member）必填"))
		return
	}
	for i, m := range members {
		if _, err := checkToken(m); err != nil {
			writeCodedErr(w, err)
			return
		}
		members[i] = m
	}
	var res interface{}
	s.handleResult(w, cfg.pool.With(func(c *Conn) error {
		v, e := c.Exec(append([]interface{}{"SADD", key}, toIface(members)...)...)
		if e != nil {
			return e
		}
		res = v
		return nil
	}), func() {
		n, _ := asInt(res)
		writeJSON(w, map[string]interface{}{"result": map[string]interface{}{"added": n}})
	})
}

func (s *service) handleSMembers(w http.ResponseWriter, r *http.Request) {
	args, cfg, ok := begin(w, r)
	if !ok {
		return
	}
	key, err := normalizeKey(strArg(args, "key"))
	if err != nil {
		writeCodedErr(w, err)
		return
	}
	var res interface{}
	s.handleResult(w, cfg.pool.With(func(c *Conn) error {
		v, e := c.Exec("SMEMBERS", key)
		if e != nil {
			return e
		}
		res = v
		return nil
	}), func() {
		arr, _ := asArray(res)
		members := make([]string, 0, len(arr))
		for _, e := range arr {
			if s, ok := asString(e); ok {
				members = append(members, s)
			}
		}
		writeJSON(w, map[string]interface{}{"result": map[string]interface{}{"members": members}})
	})
}

func (s *service) handleSRem(w http.ResponseWriter, r *http.Request) {
	args, cfg, ok := begin(w, r)
	if !ok {
		return
	}
	key, err := normalizeKey(strArg(args, "key"))
	if err != nil {
		writeCodedErr(w, err)
		return
	}
	members, ok := strSlice(args, "members")
	if !ok || len(members) == 0 {
		if m := strArg(args, "member"); m != "" {
			members = []string{m}
		}
	}
	if len(members) == 0 {
		writeCodedErr(w, fail(errBadRequest, "members（或 member）必填"))
		return
	}
	for i, m := range members {
		if _, err := checkToken(m); err != nil {
			writeCodedErr(w, err)
			return
		}
		members[i] = m
	}
	var res interface{}
	s.handleResult(w, cfg.pool.With(func(c *Conn) error {
		v, e := c.Exec(append([]interface{}{"SREM", key}, toIface(members)...)...)
		if e != nil {
			return e
		}
		res = v
		return nil
	}), func() {
		n, _ := asInt(res)
		writeJSON(w, map[string]interface{}{"result": map[string]interface{}{"deleted": n}})
	})
}

func (s *service) handleSCard(w http.ResponseWriter, r *http.Request) {
	args, cfg, ok := begin(w, r)
	if !ok {
		return
	}
	key, err := normalizeKey(strArg(args, "key"))
	if err != nil {
		writeCodedErr(w, err)
		return
	}
	var res interface{}
	s.handleResult(w, cfg.pool.With(func(c *Conn) error {
		v, e := c.Exec("SCARD", key)
		if e != nil {
			return e
		}
		res = v
		return nil
	}), func() {
		n, _ := asInt(res)
		writeJSON(w, map[string]interface{}{"result": map[string]interface{}{"cardinality": n}})
	})
}

// ── ZSet ──

func (s *service) handleZAdd(w http.ResponseWriter, r *http.Request) {
	args, cfg, ok := begin(w, r)
	if !ok {
		return
	}
	key, err := normalizeKey(strArg(args, "key"))
	if err != nil {
		writeCodedErr(w, err)
		return
	}
	members, ok := objSlice(args, "members")
	if !ok || len(members) == 0 {
		writeCodedErr(w, fail(errBadRequest, "members 必填（元素 {score, value}）"))
		return
	}
	pairs := []interface{}{}
	for _, m := range members {
		score, ok := floatArg(m, "score")
		if !ok {
			writeCodedErr(w, fail(errBadType, "每个 member 必须有数值 score"))
			return
		}
		v, ok := m["value"].(string)
		if !ok || v == "" {
			writeCodedErr(w, fail(errBadType, "每个 member 必须有字符串 value"))
			return
		}
		if _, err := checkToken(v); err != nil {
			writeCodedErr(w, err)
			return
		}
		pairs = append(pairs, formatScore(score), v)
	}
	var res interface{}
	s.handleResult(w, cfg.pool.With(func(c *Conn) error {
		v, e := c.Exec(append([]interface{}{"ZADD", key}, pairs...)...)
		if e != nil {
			return e
		}
		res = v
		return nil
	}), func() {
		n, _ := asInt(res)
		writeJSON(w, map[string]interface{}{"result": map[string]interface{}{"added": n}})
	})
}

func (s *service) handleZRange(w http.ResponseWriter, r *http.Request) {
	args, cfg, ok := begin(w, r)
	if !ok {
		return
	}
	key, err := normalizeKey(strArg(args, "key"))
	if err != nil {
		writeCodedErr(w, err)
		return
	}
	start, ok := intArg(args, "start")
	if !ok {
		start = 0
	}
	stop, ok := intArg(args, "stop")
	if !ok {
		stop = -1
	}
	withScores, _ := boolArg(args, "with_scores")
	var res interface{}
	s.handleResult(w, cfg.pool.With(func(c *Conn) error {
		cmd := []interface{}{"ZRANGE", key, start, stop}
		if withScores {
			cmd = append(cmd, "WITHSCORES")
		}
		v, e := c.Exec(cmd...)
		if e != nil {
			return e
		}
		res = v
		return nil
	}), func() {
		arr, _ := asArray(res)
		if withScores {
			items := make([]map[string]interface{}, 0, len(arr)/2)
			for i := 0; i+1 < len(arr); i += 2 {
				v, _ := asString(arr[i])
				sc, _ := asString(arr[i+1])
				items = append(items, map[string]interface{}{"value": v, "score": sc})
			}
			writeJSON(w, map[string]interface{}{"result": map[string]interface{}{"items": items}})
		} else {
			vals := make([]string, 0, len(arr))
			for _, e := range arr {
				if s, ok := asString(e); ok {
					vals = append(vals, s)
				}
			}
			writeJSON(w, map[string]interface{}{"result": map[string]interface{}{"values": vals}})
		}
	})
}

func (s *service) handleZScore(w http.ResponseWriter, r *http.Request) {
	args, cfg, ok := begin(w, r)
	if !ok {
		return
	}
	key, err := normalizeKey(strArg(args, "key"))
	if err != nil {
		writeCodedErr(w, err)
		return
	}
	member, err := checkToken(strArg(args, "member"))
	if err != nil {
		writeCodedErr(w, err)
		return
	}
	var res interface{}
	s.handleResult(w, cfg.pool.With(func(c *Conn) error {
		v, e := c.Exec("ZSCORE", key, member)
		if e != nil {
			return e
		}
		res = v
		return nil
	}), func() {
		writeJSON(w, map[string]interface{}{"result": map[string]interface{}{"score": res}})
	})
}

func (s *service) handleZRem(w http.ResponseWriter, r *http.Request) {
	args, cfg, ok := begin(w, r)
	if !ok {
		return
	}
	key, err := normalizeKey(strArg(args, "key"))
	if err != nil {
		writeCodedErr(w, err)
		return
	}
	members, ok := strSlice(args, "members")
	if !ok || len(members) == 0 {
		if m := strArg(args, "member"); m != "" {
			members = []string{m}
		}
	}
	if len(members) == 0 {
		writeCodedErr(w, fail(errBadRequest, "members（或 member）必填"))
		return
	}
	for i, m := range members {
		if _, err := checkToken(m); err != nil {
			writeCodedErr(w, err)
			return
		}
		members[i] = m
	}
	var res interface{}
	s.handleResult(w, cfg.pool.With(func(c *Conn) error {
		v, e := c.Exec(append([]interface{}{"ZREM", key}, toIface(members)...)...)
		if e != nil {
			return e
		}
		res = v
		return nil
	}), func() {
		n, _ := asInt(res)
		writeJSON(w, map[string]interface{}{"result": map[string]interface{}{"deleted": n}})
	})
}

// ── 分布式锁（SET NX PX + 安全释放） ──
//
// 不用 Lua：redis 单线程 + 侧车单连接串行（With 借一个连接、GET/DEL 连续发）即等价于原子，
// 假端点也天然成立。token 由调用方生成（比对才能释放，防止误删别人的锁）。

func (s *service) handleLock(w http.ResponseWriter, r *http.Request) {
	args, cfg, ok := begin(w, r)
	if !ok {
		return
	}
	key, err := normalizeKey(strArg(args, "key"))
	if err != nil {
		writeCodedErr(w, err)
		return
	}
	token, err := checkToken(strArg(args, "token"))
	if err != nil {
		writeCodedErr(w, err)
		return
	}
	ttlMs, ok := intArg(args, "ttl_seconds")
	if !ok || ttlMs <= 0 {
		writeCodedErr(w, fail(errBadRequest, "ttl_seconds 必填且 > 0"))
		return
	}
	var res interface{}
	s.handleResult(w, cfg.pool.With(func(c *Conn) error {
		v, e := c.Exec("SET", key, token, "NX", "PX", ttlMs*1000)
		if e != nil {
			return e
		}
		res = v
		return nil
	}), func() {
		_, ok := asString(res)
		writeJSON(w, map[string]interface{}{"result": map[string]interface{}{"acquired": ok}})
	})
}

func (s *service) handleUnlock(w http.ResponseWriter, r *http.Request) {
	args, cfg, ok := begin(w, r)
	if !ok {
		return
	}
	key, err := normalizeKey(strArg(args, "key"))
	if err != nil {
		writeCodedErr(w, err)
		return
	}
	token, err := checkToken(strArg(args, "token"))
	if err != nil {
		writeCodedErr(w, err)
		return
	}
	var released bool
	s.handleResult(w, cfg.pool.With(func(c *Conn) error {
		got, e := c.Exec("GET", key)
		if e != nil {
			return e
		}
		if s, ok := asString(got); ok && s == token {
			if _, e := c.Exec("DEL", key); e != nil {
				return e
			}
			released = true
		}
		return nil
	}), func() {
		writeJSON(w, map[string]interface{}{"result": map[string]interface{}{"released": released}})
	})
}

// ── 延迟队列（基于 zset，score = 入队时刻 + delay） ──

func (s *service) handleEnqueue(w http.ResponseWriter, r *http.Request) {
	args, cfg, ok := begin(w, r)
	if !ok {
		return
	}
	key, err := normalizeKey(strArg(args, "key"))
	if err != nil {
		writeCodedErr(w, err)
		return
	}
	items, ok := objSlice(args, "items")
	if !ok || len(items) == 0 {
		writeCodedErr(w, fail(errBadRequest, "items 必填（元素 {payload, delay_seconds}）"))
		return
	}
	nowSec := float64(nowFn().Unix())
	pairs := []interface{}{}
	for _, it := range items {
		payload, ok := it["payload"].(string)
		if !ok || payload == "" {
			writeCodedErr(w, fail(errBadType, "每个 item 必须有字符串 payload"))
			return
		}
		if _, err := checkToken(payload); err != nil {
			writeCodedErr(w, err)
			return
		}
		delay, ok := floatArg(it, "delay_seconds")
		if !ok || delay < 0 {
			writeCodedErr(w, fail(errBadType, "每个 item 必须有 >=0 的 delay_seconds"))
			return
		}
		pairs = append(pairs, formatScore(nowSec+delay), payload)
	}
	var res interface{}
	s.handleResult(w, cfg.pool.With(func(c *Conn) error {
		v, e := c.Exec(append([]interface{}{"ZADD", key}, pairs...)...)
		if e != nil {
			return e
		}
		res = v
		return nil
	}), func() {
		n, _ := asInt(res)
		writeJSON(w, map[string]interface{}{"result": map[string]interface{}{"enqueued": n}})
	})
}

func (s *service) handleDequeue(w http.ResponseWriter, r *http.Request) {
	args, cfg, ok := begin(w, r)
	if !ok {
		return
	}
	key, err := normalizeKey(strArg(args, "key"))
	if err != nil {
		writeCodedErr(w, err)
		return
	}
	batch := int64(1)
	if b, ok := intArg(args, "batch"); ok && b > 0 {
		batch = b
		if batch > maxBatch {
			batch = maxBatch
		}
	}
	nowSec := formatScore(float64(nowFn().Unix()))
	var items []map[string]interface{}
	s.handleResult(w, cfg.pool.With(func(c *Conn) error {
		// 同连接串行：先取 score<=now 的一批，再原子移除（不用 Lua，原子性由单连接保证）
		v, e := c.Exec("ZRANGEBYSCORE", key, "-inf", nowSec, "LIMIT", 0, batch)
		if e != nil {
			return e
		}
		arr, _ := asArray(v)
		if len(arr) == 0 {
			return nil
		}
		rmArgs := []interface{}{"ZREM", key}
		for _, m := range arr {
			if s, ok := asString(m); ok {
				items = append(items, map[string]interface{}{"payload": s})
				rmArgs = append(rmArgs, s)
			}
		}
		if _, e := c.Exec(rmArgs...); e != nil {
			return e
		}
		return nil
	}), func() {
		writeJSON(w, map[string]interface{}{"result": map[string]interface{}{"items": items}})
	})
}

// ── 通用辅助 ──

// strArg 取字符串参数并 Trim（" " 这种填了等于没填）；类型不对也算没传。
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

func hasKey(m map[string]interface{}, k string) bool { _, ok := m[k]; return ok }

// asStringOK 把 +OK 简单字符串（或任意字符串）归一成 true，用于 set 的 ok 字段。
func asStringOK(v interface{}) bool { _, ok := asString(v); return ok }

// toIface 把 []string 转 []interface{}（拼进命令数组用）。
func toIface(ss []string) []interface{} {
	out := make([]interface{}, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// formatScore 把浮点 score 编成 redis 接受的字符串（足够精度，不带多余尾零格式问题）。
func formatScore(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

// decodeArgs 从请求体解 {args: {...}}。
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
	addr := os.Getenv("COORD_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8103"
	}
	svc := &service{}
	c := coord.New(coord.Options{
		CheckInterval: 5 * time.Second,
		TTL:           15 * time.Second,
		OnEmpty: func() {
			log.Printf("占用列表为空，redis-sidecar 自动退出")
			os.Exit(0)
		},
	})
	log.Printf("redis-sidecar 协调监听 %s", addr)
	if err := c.Serve(addr, svc.Handlers()); err != nil {
		if coord.IsAddrInUse(err) {
			log.Printf("端口 %s 已被占用，redis-sidecar 退让退出", addr)
			os.Exit(coord.ExitAddrInUse)
		}
		log.Fatalf("redis-sidecar 异常退出: %v", err)
	}
}
