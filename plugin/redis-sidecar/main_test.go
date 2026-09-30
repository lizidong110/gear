package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// 假 redis：最小 RESP/TCP 实现，覆盖侧车测试用到的命令子集。
// 不执行 Lua（锁/队列原子性由侧车单连接串行保证，假端点单线程天然成立）。
// ─────────────────────────────────────────────────────────────────────────────

type zmember struct {
	score float64
	value string
}

type fakeRedis struct {
	mu       sync.Mutex
	strs     map[string]string
	expireAt map[string]int64
	hashes   map[string]map[string]string
	lists    map[string][]string
	sets     map[string]map[string]struct{}
	zsets    map[string][]zmember
}

func newFakeRedis() *fakeRedis {
	return &fakeRedis{
		strs:     map[string]string{},
		expireAt: map[string]int64{},
		hashes:   map[string]map[string]string{},
		lists:    map[string][]string{},
		sets:     map[string]map[string]struct{}{},
		zsets:    map[string][]zmember{},
	}
}

func (f *fakeRedis) serve(l net.Listener) {
	for {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		go f.handle(conn)
	}
}

func (f *fakeRedis) maybeExpire(key string) bool {
	if t, ok := f.expireAt[key]; ok {
		if time.Now().Unix() >= t {
			delete(f.strs, key)
			delete(f.expireAt, key)
			return true
		}
	}
	return false
}

func (f *fakeRedis) handle(conn net.Conn) {
	defer conn.Close()
	br := bufio.NewReader(conn)
	bw := bufio.NewWriter(conn)
	for {
		args, err := readCmd(br)
		if err != nil {
			return
		}
		writeResp(bw, f.exec(args))
	}
}

func readCmd(br *bufio.Reader) ([]string, error) {
	line, err := br.ReadString('\n')
	if err != nil {
		return nil, err
	}
	line = strings.TrimRight(line, "\r\n")
	if line == "" || line[0] != '*' {
		return nil, fmt.Errorf("expected array")
	}
	n, _ := strconv.Atoi(line[1:])
	args := make([]string, n)
	for i := 0; i < n; i++ {
		l, err := br.ReadString('\n')
		if err != nil {
			return nil, err
		}
		l = strings.TrimRight(l, "\r\n")
		if l == "" || l[0] != '$' {
			return nil, fmt.Errorf("expected bulk")
		}
		ln, _ := strconv.Atoi(l[1:])
		buf := make([]byte, ln)
		if _, err := io.ReadFull(br, buf); err != nil {
			return nil, err
		}
		br.Discard(2)
		args[i] = string(buf)
	}
	return args, nil
}

type rval struct {
	kind byte  // '+', '-', ':', '$', '*'
	s    string
	n     int64
	arr   []rval
}

func rStr(s string) rval  { return rval{kind: '+', s: s} }
func rInt(n int64) rval   { return rval{kind: ':', n: n} }
func rBulk(s string) rval { return rval{kind: '$', s: s} }
func rNil() rval         { return rval{kind: '$', n: -1} }
func rErr(s string) rval  { return rval{kind: '-', s: s} }
func rArr(v []rval) rval  { return rval{kind: '*', arr: v} }

func writeResp(bw *bufio.Writer, r rval) {
	switch r.kind {
	case '+':
		fmt.Fprintf(bw, "+%s\r\n", r.s)
	case ':':
		fmt.Fprintf(bw, ":%d\r\n", r.n)
	case '$':
		if r.n < 0 {
			fmt.Fprint(bw, "$-1\r\n")
		} else {
			fmt.Fprintf(bw, "$%d\r\n%s\r\n", len(r.s), r.s)
		}
	case '*':
		fmt.Fprintf(bw, "*%d\r\n", len(r.arr))
		for _, e := range r.arr {
			writeResp(bw, e)
		}
	case '-':
		fmt.Fprintf(bw, "-%s\r\n", r.s)
	}
	bw.Flush()
}

func (f *fakeRedis) exec(args []string) rval {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(args) == 0 {
		return rErr("ERR empty command")
	}
	cmd := strings.ToUpper(args[0])
	switch cmd {
	case "SET":
		if len(args) < 3 {
			return rErr("ERR wrong number of arguments for 'set'")
		}
		key, val := args[1], args[2]
		nx := false
		i := 3
		var exSec int64
		for i < len(args) {
			switch strings.ToUpper(args[i]) {
			case "EX":
				if i+1 < len(args) {
					exSec, _ = strconv.ParseInt(args[i+1], 10, 64)
				}
				i += 2
			case "PX":
				if i+1 < len(args) {
					ms, _ := strconv.ParseInt(args[i+1], 10, 64)
					exSec = ms / 1000
				}
				i += 2
			case "NX":
				nx = true
				i++
			case "XX":
				i++
			default:
				i++
			}
		}
		if nx && f.strs[key] != "" {
			return rNil()
		}
		f.strs[key] = val
		if exSec > 0 {
			f.expireAt[key] = time.Now().Unix() + exSec
		} else {
			delete(f.expireAt, key)
		}
		return rStr("OK")
	case "GET":
		f.maybeExpire(args[1])
		if v, ok := f.strs[args[1]]; ok {
			return rBulk(v)
		}
		return rNil()
	case "DEL":
		cnt := int64(0)
		for _, k := range args[1:] {
			f.maybeExpire(k)
			if _, ok := f.strs[k]; ok {
				delete(f.strs, k)
				delete(f.expireAt, k)
				cnt++
			}
		}
		return rInt(cnt)
	case "EXPIRE":
		f.maybeExpire(args[1])
		if _, ok := f.strs[args[1]]; !ok {
			return rInt(0)
		}
		sec, _ := strconv.ParseInt(args[2], 10, 64)
		f.expireAt[args[1]] = time.Now().Unix() + sec
		return rInt(1)
	case "TTL":
		f.maybeExpire(args[1])
		if _, ok := f.strs[args[1]]; !ok {
			return rInt(-2)
		}
		t, ok := f.expireAt[args[1]]
		if !ok {
			return rInt(-1)
		}
		return rInt(t - time.Now().Unix())
	case "EXISTS":
		cnt := int64(0)
		for _, k := range args[1:] {
			f.maybeExpire(k)
			if _, ok := f.strs[k]; ok {
				cnt++
			}
		}
		return rInt(cnt)
	case "INCRBY":
		f.maybeExpire(args[1])
		cur := int64(0)
		if v, ok := f.strs[args[1]]; ok {
			var err error
			cur, err = strconv.ParseInt(v, 10, 64)
			if err != nil {
				return rErr("ERR value is not an integer or out of range")
			}
		}
		by, _ := strconv.ParseInt(args[2], 10, 64)
		cur += by
		f.strs[args[1]] = strconv.FormatInt(cur, 10)
		return rInt(cur)
	case "HSET":
		key := args[1]
		if len(args[2:])%2 != 0 {
			return rErr("ERR wrong number of arguments for 'hset'")
		}
		h := f.hashes[key]
		if h == nil {
			h = map[string]string{}
			f.hashes[key] = h
		}
		added := int64(0)
		for i := 2; i < len(args); i += 2 {
			if _, existed := h[args[i]]; !existed {
				added++
			}
			h[args[i]] = args[i+1]
		}
		return rInt(added)
	case "HGET":
		h := f.hashes[args[1]]
		if v, ok := h[args[2]]; ok {
			return rBulk(v)
		}
		return rNil()
	case "HGETALL":
		h := f.hashes[args[1]]
		out := []rval{}
		for k, v := range h {
			out = append(out, rBulk(k), rBulk(v))
		}
		return rArr(out)
	case "HDEL":
		h := f.hashes[args[1]]
		cnt := int64(0)
		for _, field := range args[2:] {
			if _, ok := h[field]; ok {
				delete(h, field)
				cnt++
			}
		}
		return rInt(cnt)
	case "LPUSH":
		l := f.lists[args[1]]
		for i := len(args) - 1; i >= 2; i-- {
			l = append([]string{args[i]}, l...)
		}
		f.lists[args[1]] = l
		return rInt(int64(len(l)))
	case "RPUSH":
		l := f.lists[args[1]]
		l = append(l, args[2:]...)
		f.lists[args[1]] = l
		return rInt(int64(len(l)))
	case "LPOP", "RPOP":
		l := f.lists[args[1]]
		if len(args) > 2 {
			cnt, _ := strconv.ParseInt(args[2], 10, 64)
			out := []rval{}
			for cnt > 0 && len(l) > 0 {
				var v string
				if cmd == "LPOP" {
					v, l = l[0], l[1:]
				} else {
					v, l = l[len(l)-1], l[:len(l)-1]
				}
				out = append(out, rBulk(v))
				cnt--
			}
			f.lists[args[1]] = l
			return rArr(out)
		}
		if len(l) == 0 {
			f.lists[args[1]] = l
			return rNil()
		}
		var v string
		if cmd == "LPOP" {
			v, l = l[0], l[1:]
		} else {
			v, l = l[len(l)-1], l[:len(l)-1]
		}
		f.lists[args[1]] = l
		return rBulk(v)
	case "LLEN":
		return rInt(int64(len(f.lists[args[1]])))
	case "LRANGE":
		l := f.lists[args[1]]
		start, _ := strconv.Atoi(args[2])
		stop, _ := strconv.Atoi(args[3])
		start, stop = normRange(start, stop, len(l))
		out := []rval{}
		for i := start; i <= stop && i < len(l); i++ {
			out = append(out, rBulk(l[i]))
		}
		return rArr(out)
	case "SADD":
		s := f.sets[args[1]]
		if s == nil {
			s = map[string]struct{}{}
			f.sets[args[1]] = s
		}
		added := int64(0)
		for _, m := range args[2:] {
			if _, ok := s[m]; !ok {
				s[m] = struct{}{}
				added++
			}
		}
		return rInt(added)
	case "SMEMBERS":
		s := f.sets[args[1]]
		out := []rval{}
		for m := range s {
			out = append(out, rBulk(m))
		}
		return rArr(out)
	case "SREM":
		s := f.sets[args[1]]
		cnt := int64(0)
		for _, m := range args[2:] {
			if _, ok := s[m]; ok {
				delete(s, m)
				cnt++
			}
		}
		return rInt(cnt)
	case "SCARD":
		return rInt(int64(len(f.sets[args[1]])))
	case "ZADD":
		z := f.zsets[args[1]]
		added := int64(0)
		for i := 2; i+1 < len(args); i += 2 {
			score, _ := strconv.ParseFloat(args[i], 64)
			val := args[i+1]
			found := false
			for j, m := range z {
				if m.value == val {
					z[j].score = score
					found = true
					break
				}
			}
			if !found {
				z = append(z, zmember{score, val})
				added++
			}
		}
		f.zsets[args[1]] = z
		return rInt(added)
	case "ZSCORE":
		for _, m := range f.zsets[args[1]] {
			if m.value == args[2] {
				return rBulk(strconv.FormatFloat(m.score, 'f', -1, 64))
			}
		}
		return rNil()
	case "ZREM":
		z := f.zsets[args[1]]
		cnt := int64(0)
		for _, val := range args[2:] {
			for j, m := range z {
				if m.value == val {
					z = append(z[:j], z[j+1:]...)
					cnt++
					break
				}
			}
		}
		f.zsets[args[1]] = z
		return rInt(cnt)
	case "ZRANGEBYSCORE":
		// ZRANGEBYSCORE key min max [LIMIT off cnt]
		maxS := parseBound(args[3])
		out := []rval{}
		for _, m := range f.zsets[args[1]] {
			if m.score <= maxS {
				out = append(out, rBulk(m.value))
			}
		}
		sort.Slice(out, func(i, j int) bool { return bulkLess(out[i], out[j], f.zsets[args[1]]) })
		return rArr(out)
	case "ZRANGE":
		z := f.zsets[args[1]]
		sort.Slice(z, func(i, j int) bool {
			if z[i].score != z[j].score {
				return z[i].score < z[j].score
			}
			return z[i].value < z[j].value
		})
		start, _ := strconv.Atoi(args[2])
		stop, _ := strconv.Atoi(args[3])
		start, stop = normRange(start, stop, len(z))
		withScores := false
		for _, a := range args[4:] {
			if strings.EqualFold(a, "WITHSCORES") {
				withScores = true
			}
		}
		out := []rval{}
		for i := start; i <= stop && i < len(z); i++ {
			if withScores {
				out = append(out, rBulk(z[i].value), rBulk(strconv.FormatFloat(z[i].score, 'f', -1, 64)))
			} else {
				out = append(out, rBulk(z[i].value))
			}
		}
		return rArr(out)
	default:
		return rErr("ERR unknown command '" + cmd + "'")
	}
}

// normRange 处理负索引（redis 语义：负数从尾部倒数，-1 = 最后一个）。
func normRange(start, stop, n int) (int, int) {
	if start < 0 {
		start += n
	}
	if stop < 0 {
		stop += n
	}
	if start < 0 {
		start = 0
	}
	if stop >= n {
		stop = n - 1
	}
	if start > stop || start >= n {
		return 0, -1
	}
	return start, stop
}

func parseBound(s string) float64 {
	if s == "+inf" {
		return 1e18
	}
	f, _ := strconv.ParseFloat(s, 64)
	return f
}

// bulkLess 仅用于 ZRANGEBYSCORE 的排序占位（按 member 字典序），保证稳定即可。
func bulkLess(a, b rval, z []zmember) bool { return a.s < b.s }

// ─────────────────────────────────────────────────────────────────────────────
// 测试脚手架
// ─────────────────────────────────────────────────────────────────────────────

func startFakeRedis(t *testing.T) string {
	f := newFakeRedis()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go f.serve(l)
	return l.Addr().String()
}

func splitAddr(addr string) (string, string) {
	i := strings.LastIndex(addr, ":")
	return addr[:i], addr[i+1:]
}

// call 构造 POST 请求打到侧车的 HTTP handler，返回解析后的 JSON body。
func call(t *testing.T, addr, method string, extra map[string]interface{}) map[string]interface{} {
	host, port := splitAddr(addr)
	args := map[string]interface{}{"host": host, "port": port}
	for k, v := range extra {
		args[k] = v
	}
	body, _ := json.Marshal(map[string]interface{}{"args": args})
	req := httptest.NewRequest("POST", "/"+method, bytes.NewReader(body))
	w := httptest.NewRecorder()
	svc := &service{}
	handler, ok := svc.Handlers()["/"+method]
	if !ok {
		t.Fatalf("未知方法 %s", method)
	}
	handler(w, req)
	var out map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是 JSON: %s", w.Body.String())
	}
	return out
}

func resultOK(t *testing.T, out map[string]interface{}) map[string]interface{} {
	if err, ok := out["error"]; ok {
		t.Fatalf("期望成功，却报错: %v", err)
	}
	r, ok := out["result"].(map[string]interface{})
	if !ok {
		t.Fatalf("响应缺少 result: %v", out)
	}
	return r
}

func errCode(t *testing.T, out map[string]interface{}) string {
	e, ok := out["error"].(string)
	if !ok {
		t.Fatalf("期望错误，却成功: %v", out)
	}
	return e
}

// ─────────────────────────────────────────────────────────────────────────────
// KV
// ─────────────────────────────────────────────────────────────────────────────

func TestKVSetGet(t *testing.T) {
	addr := startFakeRedis(t)
	r := resultOK(t, call(t, addr, "set", map[string]interface{}{"key": "foo", "value": "bar"}))
	if r["ok"] != true {
		t.Fatalf("set.ok 应为 true: %v", r)
	}
	r = resultOK(t, call(t, addr, "get", map[string]interface{}{"key": "foo"}))
	if r["value"] != "bar" {
		t.Fatalf("get 应返回 bar: %v", r)
	}
}

func TestKVExpireTTL(t *testing.T) {
	addr := startFakeRedis(t)
	resultOK(t, call(t, addr, "set", map[string]interface{}{"key": "foo", "value": "bar"}))
	r := resultOK(t, call(t, addr, "expire", map[string]interface{}{"key": "foo", "seconds": 100}))
	if r["set"] != true {
		t.Fatalf("expire.set 应为 true")
	}
	r = resultOK(t, call(t, addr, "ttl", map[string]interface{}{"key": "foo"}))
	ttl, ok := r["ttl"].(float64)
	if !ok || ttl < 95 || ttl > 105 {
		t.Fatalf("ttl 应在 95~105 之间: %v", r)
	}
}

func TestKVIncr(t *testing.T) {
	addr := startFakeRedis(t)
	resultOK(t, call(t, addr, "set", map[string]interface{}{"key": "n", "value": "10"}))
	r := resultOK(t, call(t, addr, "incr", map[string]interface{}{"key": "n", "by": 5}))
	if r["value"] != float64(15) {
		t.Fatalf("incr by 5 应为 15: %v", r)
	}
	r = resultOK(t, call(t, addr, "incr", map[string]interface{}{"key": "n"}))
	if r["value"] != float64(16) {
		t.Fatalf("incr 默认 +1 应为 16: %v", r)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Hash
// ─────────────────────────────────────────────────────────────────────────────

func TestHash(t *testing.T) {
	addr := startFakeRedis(t)
	r := resultOK(t, call(t, addr, "hset", map[string]interface{}{"key": "h", "field": "f1", "value": "v1"}))
	if r["added"] != float64(1) {
		t.Fatalf("hset 应新增 1: %v", r)
	}
	r = resultOK(t, call(t, addr, "hset", map[string]interface{}{"key": "h", "fields": map[string]interface{}{"f2": "v2"}}))
	if r["added"] != float64(1) {
		t.Fatalf("hset fields 应新增 1: %v", r)
	}
	r = resultOK(t, call(t, addr, "hget", map[string]interface{}{"key": "h", "field": "f1"}))
	if r["value"] != "v1" {
		t.Fatalf("hget f1 应为 v1: %v", r)
	}
	r = resultOK(t, call(t, addr, "hgetall", map[string]interface{}{"key": "h"}))
	fields, ok := r["fields"].([]interface{})
	if !ok || len(fields) != 2 {
		t.Fatalf("hgetall 应有 2 个字段: %v", r)
	}
	r = resultOK(t, call(t, addr, "hdel", map[string]interface{}{"key": "h", "field": "f1"}))
	if r["deleted"] != float64(1) {
		t.Fatalf("hdel 应删 1: %v", r)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// List
// ─────────────────────────────────────────────────────────────────────────────

func TestList(t *testing.T) {
	addr := startFakeRedis(t)
	r := resultOK(t, call(t, addr, "rpush", map[string]interface{}{"key": "l", "values": []interface{}{"a", "b", "c"}}))
	if r["length"] != float64(3) {
		t.Fatalf("rpush 长度应为 3: %v", r)
	}
	r = resultOK(t, call(t, addr, "lrange", map[string]interface{}{"key": "l", "start": 0, "stop": -1}))
	vals := toStrings(r["values"])
	if strings.Join(vals, ",") != "a,b,c" {
		t.Fatalf("lrange 应返回 a,b,c: %v", vals)
	}
	r = resultOK(t, call(t, addr, "lpop", map[string]interface{}{"key": "l"}))
	if r["value"] != "a" {
		t.Fatalf("lpop 应为 a: %v", r)
	}
	r = resultOK(t, call(t, addr, "llen", map[string]interface{}{"key": "l"}))
	if r["length"] != float64(2) {
		t.Fatalf("llen 应为 2: %v", r)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Set
// ─────────────────────────────────────────────────────────────────────────────

func TestSet(t *testing.T) {
	addr := startFakeRedis(t)
	r := resultOK(t, call(t, addr, "sadd", map[string]interface{}{"key": "s", "members": []interface{}{"x", "y"}}))
	if r["added"] != float64(2) {
		t.Fatalf("sadd 应新增 2: %v", r)
	}
	r = resultOK(t, call(t, addr, "smembers", map[string]interface{}{"key": "s"}))
	members := toStrings(r["members"])
	if len(members) != 2 {
		t.Fatalf("smembers 应有 2: %v", members)
	}
	r = resultOK(t, call(t, addr, "srem", map[string]interface{}{"key": "s", "member": "x"}))
	if r["deleted"] != float64(1) {
		t.Fatalf("srem 应删 1: %v", r)
	}
	r = resultOK(t, call(t, addr, "scard", map[string]interface{}{"key": "s"}))
	if r["cardinality"] != float64(1) {
		t.Fatalf("scard 应为 1: %v", r)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// ZSet
// ─────────────────────────────────────────────────────────────────────────────

func TestZSet(t *testing.T) {
	addr := startFakeRedis(t)
	r := resultOK(t, call(t, addr, "zadd", map[string]interface{}{"key": "z", "members": []interface{}{
		map[string]interface{}{"score": 1, "value": "a"},
		map[string]interface{}{"score": 2, "value": "b"},
	}}))
	if r["added"] != float64(2) {
		t.Fatalf("zadd 应新增 2: %v", r)
	}
	r = resultOK(t, call(t, addr, "zrange", map[string]interface{}{"key": "z", "start": 0, "stop": -1, "with_scores": true}))
	items, ok := r["items"].([]interface{})
	if !ok || len(items) != 2 {
		t.Fatalf("zrange with_scores 应有 2 项: %v", r)
	}
	r = resultOK(t, call(t, addr, "zscore", map[string]interface{}{"key": "z", "member": "b"}))
	if r["score"] != "2" {
		t.Fatalf("zscore b 应为 2: %v", r)
	}
	r = resultOK(t, call(t, addr, "zrem", map[string]interface{}{"key": "z", "member": "a"}))
	if r["deleted"] != float64(1) {
		t.Fatalf("zrem 应删 1: %v", r)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 分布式锁（SET NX PX + 同连接串行释放）
// ─────────────────────────────────────────────────────────────────────────────

func TestLockUnlock(t *testing.T) {
	addr := startFakeRedis(t)
	r := resultOK(t, call(t, addr, "lock", map[string]interface{}{"key": "lk", "token": "t1", "ttl_seconds": 1000}))
	if r["acquired"] != true {
		t.Fatalf("首次 lock 应拿到: %v", r)
	}
	// 同 key 不同 token 的 NX 应拿不到
	r = resultOK(t, call(t, addr, "lock", map[string]interface{}{"key": "lk", "token": "t2", "ttl_seconds": 1000}))
	if r["acquired"] != false {
		t.Fatalf("第二次 lock（NX）应拿不到: %v", r)
	}
	// 错误 token 释放应失败
	r = resultOK(t, call(t, addr, "unlock", map[string]interface{}{"key": "lk", "token": "t2"}))
	if r["released"] != false {
		t.Fatalf("错误 token 的 unlock 应 released=false: %v", r)
	}
	// 正确 token 释放应成功
	r = resultOK(t, call(t, addr, "unlock", map[string]interface{}{"key": "lk", "token": "t1"}))
	if r["released"] != true {
		t.Fatalf("正确 token 的 unlock 应 released=true: %v", r)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 延迟队列（基于 zset，score = now + delay）
// ─────────────────────────────────────────────────────────────────────────────

func TestDelayQueue(t *testing.T) {
	addr := startFakeRedis(t)
	base := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	old := nowFn
	nowFn = func() time.Time { return base }
	defer func() { nowFn = old }()

	r := resultOK(t, call(t, addr, "enqueue", map[string]interface{}{"key": "q", "items": []interface{}{
		map[string]interface{}{"payload": "now", "delay_seconds": 0},
		map[string]interface{}{"payload": "later", "delay_seconds": 1000},
	}}))
	if r["enqueued"] != float64(2) {
		t.Fatalf("enqueue 应入队 2: %v", r)
	}
	// now 时刻只该取到 delay=0 的
	r = resultOK(t, call(t, addr, "dequeue", map[string]interface{}{"key": "q", "batch": 10}))
	items := toItems(r["items"])
	if len(items) != 1 || items[0] != "now" {
		t.Fatalf("dequeue@base 应只取到 now: %v", items)
	}
	// 再 dequeue 应空
	r = resultOK(t, call(t, addr, "dequeue", map[string]interface{}{"key": "q", "batch": 10}))
	if len(toItems(r["items"])) != 0 {
		t.Fatalf("dequeue@base 第二次应空: %v", r)
	}
	// 时间推进到 base+2000s，应取到 later
	nowFn = func() time.Time { return base.Add(2000 * time.Second) }
	r = resultOK(t, call(t, addr, "dequeue", map[string]interface{}{"key": "q", "batch": 10}))
	items = toItems(r["items"])
	if len(items) != 1 || items[0] != "later" {
		t.Fatalf("dequeue@base+2000 应取到 later: %v", items)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// fail-closed
// ─────────────────────────────────────────────────────────────────────────────

func TestBadKeyFailClosed(t *testing.T) {
	addr := startFakeRedis(t)
	// key 含 \0 → 应在发出任何连接前被拒
	out := call(t, addr, "get", map[string]interface{}{"key": "a\x00b"})
	if !strings.Contains(errCode(t, out), "bad_key") {
		t.Fatalf("坏 key 应报 bad_key: %v", out)
	}
}

func TestUnreachableFailClosed(t *testing.T) {
	// 连一个没人听的端口 → redis_unreachable（不是静默成功、也不是 panic）
	out := call(t, "127.0.0.1:1", "get", map[string]interface{}{"key": "foo"})
	if !strings.Contains(errCode(t, out), "redis_unreachable") {
		t.Fatalf("连不上应报 redis_unreachable: %v", out)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 辅助
// ─────────────────────────────────────────────────────────────────────────────

func toStrings(v interface{}) []string {
	arr, ok := v.([]interface{})
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func toItems(v interface{}) []string {
	arr, ok := v.([]interface{})
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		m, ok := e.(map[string]interface{})
		if !ok {
			continue
		}
		if p, ok := m["payload"].(string); ok {
			out = append(out, p)
		}
	}
	return out
}
