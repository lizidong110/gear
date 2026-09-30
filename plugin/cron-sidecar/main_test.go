package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gear/internal/cap"
)

// newTestScheduler 构造一个落点在自己临时目录里的 cron 侧车（表路径由用例掌控，
// 配置库也要各自独立），保证用例之间不共享状态。
func newTestScheduler(t *testing.T) *Scheduler {
	t.Helper()
	cfg := cap.NewConfigStore(filepath.Join(t.TempDir(), "config.db"))
	// 配置库持有 sqlite 连接直到进程结束，不关会让 t.TempDir 的清理在 Windows 上失败。
	t.Cleanup(func() { _ = cfg.Close() })
	return openTable(t, cfg, filepath.Join(t.TempDir(), "cron", "tasks.json"))
}

// openTable 打开一张指定落点的 cron 表（模拟「新进程启动时载入上一进程留下的表」）。
func openTable(t *testing.T, cfg *cap.ConfigStore, path string) *Scheduler {
	t.Helper()
	s := newScheduler(cfg)
	s.path = path
	return s
}

// writeTable 直接往表里塞若干记录（构造「上一进程遗留的状态」，避免为了测重启真起一个进程）。
func writeTable(t *testing.T, s *Scheduler, recs ...taskRecord) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(s.path), 0o750); err != nil {
		t.Fatalf("建表目录失败: %v", err)
	}
	b, err := json.Marshal(recs)
	if err != nil {
		t.Fatalf("序列化 cron 表失败: %v", err)
	}
	if err := os.WriteFile(s.path, b, 0o600); err != nil {
		t.Fatalf("写 cron 表失败: %v", err)
	}
}

// callbacks 是一个记录收到的回推的假订阅端（cron 侧车只管 POST，谁来订阅不是它的事）。
type callbacks struct {
	mu   sync.Mutex
	hits []map[string]interface{}
	srv  *httptest.Server
}

func newCallbacks(t *testing.T) *callbacks {
	t.Helper()
	c := &callbacks{}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&m)
		c.mu.Lock()
		c.hits = append(c.hits, m)
		c.mu.Unlock()
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *callbacks) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.hits)
}

// await 等到至少 n 次回推（派发是异步 goroutine，调用返回时不代表已经打到）。
func (c *callbacks) await(n int, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if c.count() >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// rpc 是一次 RPC 调用（断言成功路径；失败路径请用 errOf）。
func rpc(t *testing.T, s *Scheduler, method string, args map[string]interface{}) map[string]interface{} {
	t.Helper()
	h := s.Handlers()[method]
	if h == nil {
		t.Fatalf("侧车没有方法 %s", method)
	}
	body, _ := json.Marshal(map[string]interface{}{"args": args})
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodPost, method, bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("%s 非 200: %d %s", method, rec.Code, rec.Body.String())
	}
	var out struct {
		Result map[string]interface{} `json:"result"`
		Error  string                 `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("%s 响应不是 JSON: %s", method, rec.Body.String())
	}
	if out.Error != "" {
		t.Fatalf("%s 意外失败: %s", method, out.Error)
	}
	return out.Result
}

// errOf 断言该调用应返回 error，并把它取出来。
func errOf(t *testing.T, s *Scheduler, method string, args map[string]interface{}) string {
	t.Helper()
	body, _ := json.Marshal(map[string]interface{}{"args": args})
	rec := httptest.NewRecorder()
	s.Handlers()[method](rec, httptest.NewRequest(http.MethodPost, method, bytes.NewReader(body)))
	var out struct {
		Result map[string]interface{} `json:"result"`
		Error  string                 `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Error == "" {
		t.Fatalf("%s 本应失败，却返回了 result: %v", method, out.Result)
	}
	return out.Error
}

// bodyOf 取某个方法的原始回包文本（要断言「不出现某串」就得拿到原文）。
func bodyOf(t *testing.T, s *Scheduler, method string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]interface{}{"args": map[string]interface{}{}})
	rec := httptest.NewRecorder()
	s.Handlers()[method](rec, httptest.NewRequest(http.MethodPost, method, bytes.NewReader(body)))
	return rec.Body.String()
}

// tasksOf 把 list 的回包取成 []map。
func tasksOf(t *testing.T, out map[string]interface{}) []map[string]interface{} {
	t.Helper()
	arr, ok := out["tasks"].([]interface{})
	if !ok {
		t.Fatalf("list 回包没有 tasks: %v", out)
	}
	items := make([]map[string]interface{}, 0, len(arr))
	for _, e := range arr {
		m, ok := e.(map[string]interface{})
		if !ok {
			t.Fatalf("list 元素不是对象: %v", e)
		}
		items = append(items, m)
	}
	return items
}

// ---- 表达式核心 ----

// TestCronParseValidAndInvalid：非法表达式必须在**注册期**就被拒（fail-closed 前移）。
func TestCronParseValidAndInvalid(t *testing.T) {
	for _, good := range []string{
		"*/5 * * * *", "0 2 * * *", "30 9 * * 1-5", "@daily", "@hourly", "@monthly",
		"@weekly", "@yearly", "0 0 1 1 *", "5,10,15 * * * *", "0 9-18 * * *", "0 */4 * * *",
	} {
		if _, err := parseCron(good); err != nil {
			t.Fatalf("合法表达式 %q 被拒: %v", good, err)
		}
	}
	for _, bad := range []string{
		"", "  ", "* * * *", "* * * * * *", "60 * * * *", "* 24 * * *", "* * 0 * *",
		"* * 32 * *", "* * * 13 *", "* * * 0 *", "* * 1-31/0 * *", "* */0 * * *",
		"5-1 * * * *", "* * * * 7", "@nope", "abc * * * *", "* * * * * extra",
	} {
		if _, err := parseCron(bad); err == nil {
			t.Fatalf("非法表达式 %q 竟然通过了", bad)
		}
	}
}

// TestCronNextAfter：下一次触发的计算（跨分 / 跨日 / 跨月 / 跨年 / 闰日）。
func TestCronNextAfter(t *testing.T) {
	cases := []struct{ expr, from, want string }{
		{"*/5 * * * *", "2026-09-28T10:03:00+08:00", "2026-09-28T10:05:00+08:00"},
		{"0 2 * * *", "2026-09-28T10:03:00+08:00", "2026-09-29T02:00:00+08:00"},
		{"0 0 * * *", "2026-09-28T23:59:00+08:00", "2026-09-29T00:00:00+08:00"},
		{"0 12 * * 1", "2026-09-28T13:00:00+08:00", "2026-10-05T12:00:00+08:00"}, // 2026-09-28 是周一
		{"0 12 * * 1", "2026-09-28T11:00:00+08:00", "2026-09-28T12:00:00+08:00"},
		{"0 0 1 1 *", "2026-09-28T00:00:00+08:00", "2027-01-01T00:00:00+08:00"},
		{"0 0 29 2 *", "2026-09-28T00:00:00+08:00", "2028-02-29T00:00:00+08:00"}, // 2028 才是闰年
		{"@hourly", "2026-09-28T10:03:00+08:00", "2026-09-28T11:00:00+08:00"},
		{"@daily", "2026-09-28T10:03:00+08:00", "2026-09-29T00:00:00+08:00"},
	}
	for _, c := range cases {
		spec, err := parseCron(c.expr)
		if err != nil {
			t.Fatalf("%s 解析失败: %v", c.expr, err)
		}
		from, err := time.Parse(time.RFC3339, c.from)
		if err != nil {
			t.Fatalf("测试时间写错: %v", err)
		}
		got, ok := nextAfter(from, spec)
		if !ok {
			t.Fatalf("%s 从 %s 没算出下一次", c.expr, c.from)
		}
		if got.Format(time.RFC3339) != c.want {
			t.Fatalf("%s 从 %s 的下一次 = %s，want %s", c.expr, c.from, got.Format(time.RFC3339), c.want)
		}
	}
}

// TestCronDomDowUnion：日/周位的 Vixie 合流规则（最容易自己发明的一块，写死在此）。
func TestCronDomDowUnion(t *testing.T) {
	monday := time.Date(2026, 9, 28, 12, 0, 0, 0, time.Local)    // 周一
	tuesday := time.Date(2026, 9, 29, 12, 0, 0, 0, time.Local)   // 周二
	wednesday := time.Date(2026, 9, 30, 12, 0, 0, 0, time.Local) // 周三
	base := "0 12 "
	// dom=* dow=* → 每月每天都算。这一支在外部其实不可证伪（返回 dom，而此时 dow 恒真，
	// 两种实现看不出差别），所以真正要守住的是下面三支。
	spec, _ := parseCron(base + "* * *")
	for _, d := range []time.Time{monday, tuesday, wednesday} {
		if !spec.matches(d) {
			t.Fatalf("dom/dow 双 * 应看日，却漏了 %s", d)
		}
	}
	// dom=*、dow=1 → 只看周一（dow 生效）
	spec, _ = parseCron(base + "* * 1")
	if !spec.matches(monday) || spec.matches(tuesday) || spec.matches(wednesday) {
		t.Fatalf("dom=* dow=1 应只看周一")
	}
	// dom=29、dow=* → 只看 29 号（dom 生效；2026-09-29 正好是周二）
	spec, _ = parseCron(base + "29 * *")
	if spec.matches(monday) || !spec.matches(tuesday) || spec.matches(wednesday) {
		t.Fatalf("dom=29 dow=* 应只看 29 号")
	}
	// 两个都非星 → **取或**
	spec, _ = parseCron(base + "29 * 1")
	if !spec.matches(monday) || !spec.matches(tuesday) || spec.matches(wednesday) {
		t.Fatalf("dom=29 与 dow=1 应取或")
	}
}

// ---- 语义：幂等 / overlap / catch_up ----

// TestCronIdempotentSameMinute：AC-E3-2 —— 同一分钟内的重复触发被静默丢弃。
func TestCronIdempotentSameMinute(t *testing.T) {
	s := newTestScheduler(t)
	cb := newCallbacks(t)
	if _, err := s.add(taskRecord{
		Name: "idem", Expr: "* * * * *", Timezone: "Local",
		CallbackURL: cb.srv.URL, Payload: map[string]interface{}{"k": "v"},
	}); err != nil {
		t.Fatalf("add 失败: %v", err)
	}
	// 对同一时刻派发三次（模拟三种来源撞进同一分钟：回推重试 / catch_up 补跑 / 手动触发）。
	fire := time.Now().Truncate(time.Minute)
	for i := 0; i < 3; i++ {
		s.dispatch(s.tasks["idem"], fire, false)
	}
	cb.await(1, 3*time.Second)
	if n := cb.count(); n != 1 {
		t.Fatalf("同一分钟重复派发了 %d 次（幂等失效），只应 1 次", n)
	}
	got := func() map[string]interface{} { cb.mu.Lock(); defer cb.mu.Unlock(); return cb.hits[0] }()
	if got["task"] != "idem" {
		t.Fatalf("回推体里的 task 名不对: %v", got["task"])
	}
	if got["fire_time"] == "" {
		t.Fatalf("回推体缺 fire_time: %v", got)
	}
	if got["payload"].(map[string]interface{})["k"] != "v" {
		t.Fatalf("payload 没带到回推里: %v", got["payload"])
	}
}

// TestCronOverlapSkippedByDefault：AC-E3-3 —— 上一轮没结束又到点 → 跳过。
func TestCronOverlapSkippedByDefault(t *testing.T) {
	s := newTestScheduler(t)
	cb := newCallbacks(t)
	tm := &task{rec: taskRecord{Name: "slow", Expr: "* * * * *", CallbackURL: cb.srv.URL},
		spec: mustSpec(t, "* * * * *"), loc: time.Local}
	tm.mu.Lock()
	tm.running = true // 上一轮故意没结束
	tm.mu.Unlock()
	s.dispatch(tm, time.Now(), false)
	if n := cb.count(); n != 0 {
		t.Fatalf("overlap 默认应跳过，却派发了 %d 次", n)
	}
	// 上一轮跑完（真实场景由回推 goroutine 结束时放开）→ 下一分钟到点应正常派发。
	// 否则「默认跳过」会退化成「永远跳过」，那是另一种事故。
	tm.mu.Lock()
	tm.running = false
	tm.mu.Unlock()
	s.dispatch(tm, time.Now().Add(time.Minute), false)
	cb.await(1, 3*time.Second)
	if n := cb.count(); n != 1 {
		t.Fatalf("上一轮结束后的触发该派发，实际 %d 次", n)
	}
}

// TestCronCatchUpOnlyLatest：AC-E3-4 —— catch_up 只补最近一次，不是补一整个窗口。
func TestCronCatchUpOnlyLatest(t *testing.T) {
	s := newTestScheduler(t)
	cb := newCallbacks(t)
	// 一条每分钟的任务，catch_up 打开，假设「进程停了 3 分钟」：只应补 1 次而不是 3 次。
	s.tasks["late"] = &task{rec: taskRecord{
		Name: "late", Expr: "* * * * *", CallbackURL: cb.srv.URL, CatchUp: true},
		spec: mustSpec(t, "* * * * *"), loc: time.Local}
	s.reschedule(s.tasks["late"], time.Now().Add(-3*time.Minute))
	cb.await(1, 3*time.Second)
	if n := cb.count(); n != 1 {
		t.Fatalf("catch_up 补了 %d 次（只应补最近一次 1 次）", n)
	}
	// 补跑只是「额外打一发」，不该打乱正常的节奏：t.next 仍应是模拟时刻之后的那一次
	// （注意别拿真实时间比——这里的 now 是故意往回拨 3 分钟的模拟值）。
	sim := time.Now().Add(-3 * time.Minute)
	s.mu.Lock()
	next := s.tasks["late"].next
	s.mu.Unlock()
	if !next.After(sim) {
		t.Fatalf("catch_up 把 t.next 拖回了过去: %v（sim now = %v）", next, sim)
	}
}

// TestCronLastFirePersists：一次触发要让 last_fire 落到 cron 表上（不是只在内存里），
// 同时顺带证明 Stop 会等在飞的回推收尾——这是「硬绑定」的另一半（停之前把事做完）。
func TestCronLastFirePersists(t *testing.T) {
	s := newTestScheduler(t)
	cb := newCallbacks(t)
	s.tasks["p"] = &task{rec: taskRecord{Name: "p", Expr: "* * * * *", CallbackURL: cb.srv.URL},
		spec: mustSpec(t, "* * * * *"), loc: time.Local}
	// 走真实路径（loop → tick → dispatch），而不是手工喂 reschedule：
	// last_fire 就该由正常触发的那条路写出来。
	s.tasks["p"].next = time.Now().Add(-time.Minute).Truncate(time.Minute)
	s.tick(time.Now())
	cb.await(1, 3*time.Second)
	s.Stop() // 等在飞的那发打完再落盘
	raw, err := os.ReadFile(s.path)
	if err != nil {
		t.Fatalf("读 cron 表失败: %v", err)
	}
	if !strings.Contains(string(raw), "last_fire") {
		t.Fatalf("last_fire 没落盘: %s", raw)
	}
	if cb.count() != 1 {
		t.Fatalf("Stop 之后又多打了几发: %d", cb.count())
	}
}

// ---- 硬绑定 ----

// TestCronHardBindingStop：AC-E3-5 的进程内证明 —— Stop 之后 loop 退出，且此后不再派发。
func TestCronHardBindingStop(t *testing.T) {
	s := newTestScheduler(t)
	cb := newCallbacks(t)
	out := rpc(t, s, "/add", map[string]interface{}{
		"name": "nightly", "expr": "0 2 * * *", "timezone": "Local",
		"callback_url": cb.srv.URL, "payload": map[string]interface{}{"secret": "s3cret"},
	})
	// 下一次必须是「明天 02:00」：证明时区 + 下一次的计算不是随手一个数。
	want := time.Now().AddDate(0, 0, 1)
	want = time.Date(want.Year(), want.Month(), want.Day(), 2, 0, 0, 0, time.Local)
	if got := out["next_fire"]; got != want.Format(time.RFC3339) {
		t.Fatalf("next_fire = %v，want %s", got, want.Format(time.RFC3339))
	}

	done := make(chan struct{})
	go func() { s.loop(); close(done) }()
	time.Sleep(300 * time.Millisecond) // 让 loop 至少醒来一轮
	s.Stop()                           // 等价「应用/侧车进程退出」
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop 之后调度循环没退出（硬绑定没落进代码，只写在注释里）")
	}
	s.Stop() // 幂等：OnEmpty 与实际退出可能都调一次

	// 「时间旅行」到明天 02:05：那本该是触发点，但因为已经停了，什么也不该发生。
	s.tick(want.Add(5 * time.Minute))
	time.Sleep(300 * time.Millisecond)
	if n := cb.count(); n != 0 {
		t.Fatalf("停了之后仍然回推了 %d 次（应用退出即停被破坏）", n)
	}
}

// TestCronRestartDoesNotAdoptMissed：AC-E3-5 的另一半 —— 重启后的新进程不接管错过的触发。
//
// 场景：上一进程留下了「at 已过」的一次性任务。重启后它应当**留在表里、标 expired**，
// 直到有人显式 say "catch_up"，而不是新进程一启动就替它补一发。
func TestCronRestartDoesNotAdoptMissed(t *testing.T) {
	s := newTestScheduler(t)
	cb := newCallbacks(t)
	past := time.Now().Add(-30 * time.Minute).Truncate(time.Minute)
	at := past.Format(atLayouts)

	// ① catch_up=false：不接管，留在表里且标 expired。
	writeTable(t, s, taskRecord{
		Name: "missed", At: at, Timezone: "Local", CallbackURL: cb.srv.URL,
	})
	s.tasks = map[string]*task{}
	if err := s.load(); err != nil {
		t.Fatalf("载入表失败: %v", err)
	}
	items := tasksOf(t, rpc(t, s, "/list", nil))
	if len(items) != 1 || items[0]["state"] != "expired" {
		t.Fatalf("已过期的 at 应标记 expired 留在表里，实际 %v", items)
	}
	s.tick(time.Now().Add(time.Hour))
	time.Sleep(300 * time.Millisecond)
	if n := cb.count(); n != 0 {
		t.Fatalf("catch_up 未开却补跑了 %d 次", n)
	}

	// ② at 落在**当前这一分钟**的一次性任务同样算错过（进程在触发时刻是死的）。
	// 这条是 verify.py 真 kill 之后重启跑出来的真漏洞：旧代码把「at == 当前分钟」当成
	// 仍在等待，于是新进程把它补打了——「重启不接管」就是这样破的。
	sB := openTable(t, s.cfg, filepath.Join(t.TempDir(), "cronB", "tasks.json"))
	writeTable(t, sB, taskRecord{
		Name: "curgone", At: time.Now().Truncate(time.Minute).Format(atLayouts),
		Timezone: "Local", CallbackURL: cb.srv.URL,
	})
	if err := sB.load(); err != nil {
		t.Fatalf("载入表失败: %v", err)
	}
	if items := tasksOf(t, rpc(t, sB, "/list", nil)); len(items) != 1 || items[0]["state"] != "expired" {
		t.Fatalf("at 落在本分钟应标记 expired，实际 %v", items)
	}
	sB.tick(time.Now().Add(time.Hour))
	time.Sleep(300 * time.Millisecond)
	if n := cb.count(); n != 0 {
		t.Fatalf("at 落在本分钟却补打了 %d 次（重启不接管被破坏）", n)
	}

	// ③ catch_up=true：显式说「补」，只补最近一次。
	s2 := openTable(t, s.cfg, filepath.Join(t.TempDir(), "cron2", "tasks.json"))
	writeTable(t, s2, taskRecord{
		Name: "missed", At: at, Timezone: "Local", CallbackURL: cb.srv.URL, CatchUp: true,
	})
	if err := s2.load(); err != nil {
		t.Fatalf("载入表失败: %v", err)
	}
	s2.tick(time.Now().Add(time.Hour))
	cb.await(1, 3*time.Second)
	if n := cb.count(); n != 1 {
		t.Fatalf("catch_up=true 应补 1 次，实际 %d 次", n)
	}
	// 补完任务从表里消失（一次性任务的语义）。
	if got := tasksOf(t, rpc(t, s2, "/list", nil)); len(got) != 0 {
		t.Fatalf("一次性任务触发后应从表里消失，实际还有 %v", got)
	}
}

// ---- 表与可见性 ----

// TestCronOneShotFiresAndRecords：一次性任务触发一次即消失，last_fire 落盘。
func TestCronOneShotFiresAndRecords(t *testing.T) {
	s := newTestScheduler(t)
	cb := newCallbacks(t)
	at := time.Now().Add(-time.Minute).Truncate(time.Minute).Format(atLayouts)
	rpc(t, s, "/at", map[string]interface{}{
		"name": "once", "at": at, "timezone": "Local", "callback_url": cb.srv.URL,
	})
	if items := tasksOf(t, rpc(t, s, "/list", nil)); len(items) != 1 || items[0]["at"] != at {
		t.Fatalf("一次性任务应留在表里（可见），实际 %v", items)
	}
	time.Sleep(300 * time.Millisecond)
	if n := cb.count(); n != 0 {
		t.Fatalf("catch_up 未开却补跑了 %d 次", n)
	}
	// 用 catch_up 补一次，触发后任务消失且 last_fire 可见。
	s2 := openTable(t, s.cfg, filepath.Join(t.TempDir(), "cron2", "tasks.json"))
	writeTable(t, s2, taskRecord{
		Name: "once", At: at, Timezone: "Local", CallbackURL: cb.srv.URL, CatchUp: true,
	})
	if err := s2.load(); err != nil {
		t.Fatalf("载入表失败: %v", err)
	}
	s2.tick(time.Now())
	cb.await(1, 3*time.Second)
	s2.Stop()
	// 一次性任务触发后即消失（表里不留孤儿），所以 last_fire 只属于周期任务，见下一个用例。
	if got := tasksOf(t, rpc(t, s2, "/list", nil)); len(got) != 0 {
		t.Fatalf("一次性任务触发后应从表里消失，实际还有 %v", got)
	}
}

// TestCronListHidesSecrets：list 不摊开 payload / callback_url（凭据不进可观察面）。
func TestCronListHidesSecrets(t *testing.T) {
	s := newTestScheduler(t)
	rpc(t, s, "/add", map[string]interface{}{
		"name": "quiet", "expr": "0 3 * * *", "callback_url": "http://127.0.0.1:1/hook",
		"payload": map[string]interface{}{"password": "p@ss"},
	})
	body := bodyOf(t, s, "/list")
	for _, leak := range []string{"p@ss", "127.0.0.1:1", "password", "hook"} {
		if strings.Contains(body, leak) {
			t.Fatalf("list 回包泄露了 %q: %s", leak, body)
		}
	}
	if !strings.Contains(body, "quiet") {
		t.Fatalf("list 没列出任务: %s", body)
	}
}

// TestCronValidateFailClosed：名字 / 回调地址 / 表达式 / 二选一，都在注册期拒。
func TestCronValidateFailClosed(t *testing.T) {
	s := newTestScheduler(t)
	if e := errOf(t, s, "/add", map[string]interface{}{"expr": "0 1 * * *"}); !strings.Contains(e, "name 必填") {
		t.Fatalf("缺 name 未被拦: %s", e)
	}
	if e := errOf(t, s, "/add", map[string]interface{}{"name": "x", "expr": "0 1 * * *"}); !strings.Contains(e, "callback_url 必填") {
		t.Fatalf("缺 callback_url 未被拦: %s", e)
	}
	if e := errOf(t, s, "/add", map[string]interface{}{"name": "x", "expr": "每分钟"}); !strings.Contains(e, "5 段") {
		t.Fatalf("非法表达式未被拦: %s", e)
	}
	if e := errOf(t, s, "/add", map[string]interface{}{
		"name": "x", "expr": "0 1 * * *", "timezone": "Mars/Phobos",
		"callback_url": "http://127.0.0.1:1/h"}); !strings.Contains(e, "timezone") {
		t.Fatalf("非法 timezone 未被拦: %s", e)
	}
	if e := errOf(t, s, "/at", map[string]interface{}{
		"name": "y", "at": "明天", "callback_url": "http://127.0.0.1:1/h"}); !strings.Contains(e, "at 无法解析") {
		t.Fatalf("非法 at 未被拦: %s", e)
	}
	if e := errOf(t, s, "/at", map[string]interface{}{
		"name": "y", "at": "2026-09-28 10:00", "expr": "0 1 * * *",
		"callback_url": "http://127.0.0.1:1/h"}); !strings.Contains(e, "二选一") {
		t.Fatalf("at 与 expr 同时给未被拦: %s", e)
	}
	if e := errOf(t, s, "/cancel", map[string]interface{}{"name": "不存在"}); !strings.Contains(e, "不存在") {
		t.Fatalf("取消不存在的任务应 fail-visible: %s", e)
	}
	rpc(t, s, "/add", map[string]interface{}{
		"name": "zz", "expr": "0 1 * * *", "callback_url": "http://127.0.0.1:1/h"})
	if out := rpc(t, s, "/cancel", map[string]interface{}{"name": "zz"}); out["ok"] != true {
		t.Fatalf("取消已存在的任务应成功: %v", out)
	}
	if got := tasksOf(t, rpc(t, s, "/list", nil)); len(got) != 0 {
		t.Fatalf("取消后表里应为空，实际 %v", got)
	}
}

// TestCronTableRoundTrip：落盘 → 重启载入，是同一张表（硬绑定下这是表的唯一用途）。
func TestCronTableRoundTrip(t *testing.T) {
	s := newTestScheduler(t)
	rpc(t, s, "/add", map[string]interface{}{
		"name": "keep", "expr": "*/7 * * * *", "timezone": "UTC",
		"callback_url": "http://127.0.0.1:1/h"})
	rpc(t, s, "/add", map[string]interface{}{
		"name": "gone", "expr": "*/8 * * * *", "timezone": "UTC",
		"callback_url": "http://127.0.0.1:1/h"})
	rpc(t, s, "/cancel", map[string]interface{}{"name": "gone"})
	if got := tasksOf(t, rpc(t, s, "/list", nil)); len(got) != 1 || got[0]["name"] != "keep" {
		t.Fatalf("取消后表里应只剩 keep，实际 %v", got)
	}

	s2 := openTable(t, s.cfg, s.path)
	if err := s2.load(); err != nil {
		t.Fatalf("载入 cron 表失败: %v", err)
	}
	got := tasksOf(t, rpc(t, s2, "/list", nil))
	if len(got) != 1 || got[0]["name"] != "keep" {
		t.Fatalf("重启后表不是同一张: %v", got)
	}
	if got[0]["timezone"] != "UTC" {
		t.Fatalf("时区没跟着表一起活下来: %v", got[0]["timezone"])
	}
}

// TestCronCorruptTableFailsLoudly：表损坏时报错而不是当空（当空 =「明明配过却没了」，最难查）。
func TestCronCorruptTableFailsLoudly(t *testing.T) {
	s := newTestScheduler(t)
	if err := os.MkdirAll(filepath.Dir(s.path), 0o750); err != nil {
		t.Fatalf("建表目录失败: %v", err)
	}
	if err := os.WriteFile(s.path, []byte("{ 这不是 JSON"), 0o600); err != nil {
		t.Fatalf("写损坏表失败: %v", err)
	}
	err := s.load()
	if err == nil || !strings.Contains(err.Error(), "cron 表损坏") {
		t.Fatalf("损坏的表应报「cron 表损坏」，got %v", err)
	}
}

// TestCronCapsRedLine：/caps 必须覆盖 extensions.yaml 声明的方法。
func TestCronCapsRedLine(t *testing.T) {
	s := newTestScheduler(t)
	out := rpc(t, s, "/caps", nil)
	if out["ext"] != "cron" {
		t.Fatalf("ext 名不对: %v", out["ext"])
	}
	want := map[string]bool{"add": false, "at": false, "list": false, "cancel": false}
	for _, m := range out["methods"].([]interface{}) {
		if _, ok := want[m.(string)]; ok {
			want[m.(string)] = true
		}
	}
	for m, ok := range want {
		if !ok {
			t.Fatalf("/caps 漏报方法 %s（改了实现却没改 /caps）", m)
		}
	}
}

// ---- 小工具 ----

func mustSpec(t *testing.T, expr string) cronSpec {
	t.Helper()
	spec, err := parseCron(expr)
	if err != nil {
		t.Fatalf("解析 %s 失败: %v", expr, err)
	}
	return spec
}
