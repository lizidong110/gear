// Command cron-sidecar 是 ext.cron 的常驻侧车（service ext）：
//
//   - 绑定声明端口（listen / COORD_ADDR，127.0.0.1 only），暴露 RPC（/add /at /list /cancel /caps）
//   - coord 协调端点（/register 等），到点把事件 POST 回 callback_url（经内核 /api/events/{handle}
//     投给订阅者，与 webhook / mqtt / kafka 同一条回推线）；
//   - **生命周期硬绑定（PRD §4.1，2026-09-28 用户拍板）**：侧车随应用进程起、随进程死而亡——
//     进程一停，调度立即停止且**不接管**；应用重启后不会「接回」间隔期内错过的触发
//     （真要补须显式 catch_up）。系统因此只有「有进程在跑」与「没进程」两态，
//     不存在「表在、进程没了、谁在跑」的第三态。cron 表落盘只用于「重启后重建同一张时间表」。
//   - 三条语义（Q5 拍板）：
//     幂等 —— 去重键 = name + 触发时刻（分钟粒度），同一分钟内的重复触发静默丢弃；
//     misfire 默认不补（catch_up 显式才补，**且只补最近一次**，不是补一整个窗口——
//     否则停三天的进程一启动就是几千次触发，那叫风暴不叫补偿）；
//     overlap 默认跳过（上一轮没跑完又到点 → 跳过并点名日志，overlap 显式才并发）。
//   - 表达式自己解析（5 段标准 cron + 五个 @ 宏），零新增依赖；非法表达式在 **add/at 期**就拒
//     （fail-closed 前移到写作期——等到点才炸的调度器没人敢用）。
//
// 环境变量：COORD_ADDR（默认 127.0.0.1:8097；引擎拉起时按 extensions.yaml 的 listen 注入，
// 此处默认值仅供不经引擎的手工单跑兜底）。
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"gear/internal/cap"
	"gear/internal/gearhome"
	"gear/internal/sidecar/coord"
)

const (
	// 配置键（键名与 `gear config set cron.tasks_file ...` / `ctx.config` 同源）。
	cfgKeyTasksFile = "cron.tasks_file"
	// 默认落点：应用数据目录下的 cron/tasks.json（root 由宿主决定，侧车不硬编码）。
	defaultTasksRel = "cron" + string(filepath.Separator) + "tasks.json"

	// callbackTimeout 是回推 HTTP 的超时。全仓约定散在 3s / 5s / 10s 三处（各侧车各写各的），
	// 本批拉齐到 10s：定时任务的「到点」比「响应快」重要，宁可让回调失败可见也不要拖住下一轮。
	callbackTimeout = 10 * time.Second

	atLayouts = "2006-01-02 15:04"
)

// taskRecord 是落盘到 cron 表的一条任务（也是 list 的数据源）。
//
// ⚠ payload 会**明文**落盘（tasks.json）：别把密钥放 payload——需要凭据请让 Flow 自己去取。
type taskRecord struct {
	Name        string                 `json:"name"`
	Expr        string                 `json:"expr,omitempty"` // 5 段 cron（一次性任务为空）
	At          string                 `json:"at,omitempty"`   // 一次性任务的时刻（RFC3339 或 "2006-01-02 15:04"）
	Timezone    string                 `json:"timezone,omitempty"`
	CatchUp     bool                   `json:"catch_up"`
	Overlap     bool                   `json:"overlap"`
	CallbackURL string                 `json:"callback_url"`
	Payload     map[string]interface{} `json:"payload,omitempty"`
	LastFire    string                 `json:"last_fire,omitempty"` // RFC3339，从未触发则为空
}

// task 是进程内的 running 态对象（next / running / 去重键都不落盘，它们是「进程内状态」）。
type task struct {
	rec  taskRecord
	spec cronSpec  // Expr 型才有
	at   time.Time // At 型才有（零值 = 非一次性）
	loc  *time.Location
	next time.Time // 下一次触发（零值 = 已过期 / 已删除）

	mu      sync.Mutex // 保护 running 与去重键：回推是异步的，tick 与完成回调会撞
	running bool
	lastMin time.Time // 最近一次触发的「分钟」（去重键的一半）
}

type Scheduler struct {
	mu    sync.Mutex
	tasks map[string]*task
	path  string
	cfg   *cap.ConfigStore
	cli   *http.Client
	stop  chan struct{}
	wg    sync.WaitGroup
}

func newScheduler(cfg *cap.ConfigStore) *Scheduler {
	// tasks 文件路径：配置库的 cron.tasks_file 优先，缺省 <AppDataDir>/cron/tasks.json。
	p := filepath.Join(gearhome.AppDataDir(), defaultTasksRel)
	if v, err := cfg.Get(cfgKeyTasksFile); err == nil && strings.TrimSpace(v) != "" {
		p = v
	}
	return &Scheduler{
		tasks: map[string]*task{},
		path:  p,
		cfg:   cfg,
		cli:   &http.Client{Timeout: callbackTimeout},
		stop:  make(chan struct{}),
	}
}

func (s *Scheduler) Handlers() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"/add":    s.handleAdd,
		"/at":     s.handleAt,
		"/list":   s.handleList,
		"/cancel": s.handleCancel,
		"/caps":   s.handleCaps,
	}
}

// Stop 停掉调度循环并等待在飞的回推收尾（进程退出 / OnEmpty 走的就是这条路径）。
// **不做任何接管**：停了就是停了，重启由新的进程按 cron 表重建，不会接回错过的触发。
func (s *Scheduler) Stop() {
	select {
	case <-s.stop:
	default:
		close(s.stop)
	}
	s.wg.Wait()
}

// stopped 报告调度是否已停。硬绑定要落在**代码**里而不只是注释里：
// loop 与 tick 都据此早退，这样「停了之后哪怕时间走到触发点也没人派发」是可断言的。
func (s *Scheduler) stopped() bool {
	select {
	case <-s.stop:
		return true
	default:
		return false
	}
}

// ---- 落盘 ----

func (s *Scheduler) load() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o750); err != nil {
		return fmt.Errorf("创建 cron 表目录失败: %w", err)
	}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // 首次运行没有表，不是错误
		}
		return err
	}
	var records []taskRecord
	if err := json.Unmarshal(raw, &records); err != nil {
		// 表坏了不静默当空：那会让「明明配过」的任务悄悄消失，重启后看起来像没配。
		return fmt.Errorf("cron 表损坏（%s）: %w", s.path, err)
	}
	now := time.Now()
	for _, rec := range records {
		t, err := buildTask(rec)
		if err != nil {
			return fmt.Errorf("cron 表里有一条任务无法载入（name=%s）: %w", rec.Name, err)
		}
		s.tasks[rec.Name] = t
		s.reschedule(t, now)
	}
	log.Printf("[cron] 载入 %d 条任务 <- %s", len(s.tasks), s.path)
	return nil
}

func (s *Scheduler) save() {
	// 锁序恒为 s.mu → t.mu（tick 也是这条序）：LastFire 会被回推 goroutine 在 t.mu 下改写，
	// 不锁就读会跟它抢同一块 rec。
	s.mu.Lock()
	records := make([]taskRecord, 0, len(s.tasks))
	for _, t := range s.tasks {
		t.mu.Lock()
		records = append(records, t.rec)
		t.mu.Unlock()
	}
	s.mu.Unlock()
	sort.Slice(records, func(i, j int) bool { return records[i].Name < records[j].Name })
	b, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		log.Printf("[cron] 序列化 cron 表失败: %v", err)
		return
	}
	// 目录也可能还没有（表从未写过）：不建目录的话首次写入就失败，且失败只留一条日志，
	// 任务「看起来配好了、重启后凭空消失」——正是最难查的那类事故。
	if err := os.MkdirAll(filepath.Dir(s.path), 0o750); err != nil {
		log.Printf("[cron] 建 cron 表目录失败: %v", err)
		return
	}
	// 临时文件 + 改名：写一半挂掉不会留下半截表（下次启动当作损坏表直接拒）。
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		log.Printf("[cron] 写 cron 表失败: %v", err)
		return
	}
	if err := os.Rename(tmp, s.path); err != nil {
		log.Printf("[cron] 替换 cron 表失败: %v", err)
	}
}

// ---- 任务构造与调度 ----

// buildTask 把一条记录变成进程内任务（所有校验都在这里，add/at/载入共用一条路径）。
func buildTask(rec taskRecord) (*task, error) {
	if strings.TrimSpace(rec.Name) == "" {
		return nil, fmt.Errorf("name 必填")
	}
	loc := time.Local
	if rec.Timezone != "" {
		l, err := time.LoadLocation(rec.Timezone)
		if err != nil {
			return nil, fmt.Errorf("timezone 非法 %q: %w", rec.Timezone, err)
		}
		loc = l
	}
	t := &task{rec: rec, loc: loc}
	if strings.TrimSpace(rec.At) != "" {
		at, err := parseAt(rec.At, loc)
		if err != nil {
			return nil, err
		}
		t.at = at
		return t, nil
	}
	spec, err := parseCron(rec.Expr)
	if err != nil {
		return nil, err
	}
	t.spec = spec
	return t, nil
}

func parseAt(s string, loc *time.Location) (time.Time, error) {
	s = strings.TrimSpace(s)
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if t, err := time.ParseInLocation(atLayouts, s, loc); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("at 无法解析（应形如 2026-09-28T18:00:00+08:00 或 2026-09-28 18:00）: %q", s)
}

// reschedule 在 now 附近安放下一次触发，并处理「间隔期内错过的」这一态。
func (s *Scheduler) reschedule(t *task, now time.Time) {
	// 一次性任务：对齐到分钟，past 一次性任务要看 catch_up。
	if !t.at.IsZero() {
		m := t.at.In(t.loc).Truncate(time.Minute)
		// ⚠ 只认「严格在未来」为待触发。「at 正好落在当前这一分钟」属于**已经错过**——
		// 进程在触发时刻若是活的，此刻早该打过了；重启后的新进程把它当成「还在等」并补打，
		// 就是硬绑定最典型的一个漏洞（verify.py P1c 真 kill 之后重启，就撞在这一条上）。
		if m.After(now) {
			t.next = m
			return
		}
		// 已经过去：catch_up 显式才补一次，否则留在表里但标记为已过期（可见，不静默删）。
		if t.rec.CatchUp {
			t.next = m
			return
		}
		t.next = time.Time{}
		return
	}
	// 一律按任务自己的时区推进（地处在别的时区时，用宿主本地时区算出来的「下一次」是错的）。
	n, ok := nextAfter(now.In(t.loc), t.spec)
	if !ok {
		t.next = time.Time{}
		return
	}
	t.next = n
	if t.rec.CatchUp {
		// 只补**最近一次**（不是补一整个窗口）：停三天的进程一启动就补几千次，那叫风暴不叫补偿。
		if p, ok := prevBefore(now.In(t.loc), t.spec); ok && p.Before(now) {
			s.dispatch(t, p, true)
		}
		// t.next 保持上面的未来值：补跑只是「额外打一发」，不打乱正常的暂停-唤醒节奏。
	}
}

// dispatch 触发一次（异步回推）。catchUp=true 表示这次是补跑，跳过 running 判定。
func (s *Scheduler) dispatch(t *task, fireTime time.Time, catchUp bool) {
	t.mu.Lock()
	if !catchUp && t.running {
		// overlap 默认跳过：慢任务叠跑是企业定时任务最常见的事故源。
		t.mu.Unlock()
		log.Printf("[cron] %s 跳过本次触发（上一轮未结束，overlap 未显式开启）at %s",
			t.rec.Name, fireTime.In(t.loc).Format(time.RFC3339))
		return
	}
	// 去重键：name + 触发时刻（分钟粒度）——PRD §4.1。命中即静默丢弃（重复是常态，不是故障）；
	// 且被丢掉的重复**不算一轮**：running 只在真正回推时置起（否则一次网络抖动会把任务永久卡死）。
	if !t.lastMin.IsZero() && t.lastMin.Equal(fireTime.In(t.loc).Truncate(time.Minute)) {
		t.mu.Unlock()
		return
	}
	t.lastMin = fireTime.In(t.loc).Truncate(time.Minute)
	t.running = true
	t.mu.Unlock()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() { t.mu.Lock(); t.running = false; t.mu.Unlock() }()
		s.post(t, fireTime)
		t.mu.Lock()
		t.rec.LastFire = fireTime.In(t.loc).Format(time.RFC3339) // 落盘，list 才看得到 last_fire
		t.mu.Unlock()
		s.save()
	}()
}

// post 把一次触发 POST 到 callback_url（body：task / fire_time / payload）。
func (s *Scheduler) post(t *task, fireTime time.Time) {
	body := map[string]interface{}{
		"task":      t.rec.Name,
		"fire_time": fireTime.In(t.loc).Format(time.RFC3339),
		"payload":   t.rec.Payload,
	}
	b, err := json.Marshal(body)
	if err != nil {
		log.Printf("[cron] %s 序列化事件失败: %v", t.rec.Name, err)
		return
	}
	req, err := http.NewRequest(http.MethodPost, t.rec.CallbackURL, strings.NewReader(string(b)))
	if err != nil {
		log.Printf("[cron] %s 构造回推请求失败: %v", t.rec.Name, err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.cli.Do(req)
	if err != nil {
		// 回推失败**只记日志不重试**：重试会把一次网络抖动放大成重复业务（幂等键是分钟粒度，
		// 重试落在同一分钟内也会被去重丢掉，白跑）。业务该自己保证幂等。
		log.Printf("[cron] %s 回推失败: %v", t.rec.Name, err)
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= http.StatusBadRequest {
		log.Printf("[cron] %s 回推返回 %d", t.rec.Name, resp.StatusCode)
	}
}

// tick 处理所有到点的任务（在锁内跑，保证「同一时刻只有一个 tick 在派发」）。
func (s *Scheduler) tick(now time.Time) {
	if s.stopped() {
		return // 已停：不再派发任何一轮（硬绑定，PRD §4.1）
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	due := []*task{}
	for _, t := range s.tasks {
		if !t.next.IsZero() && !t.next.After(now) {
			due = append(due, t)
		}
	}
	sort.Slice(due, func(i, j int) bool { return due[i].next.Before(due[j].next) })
	for _, t := range due {
		fireTime := t.next
		if !t.at.IsZero() {
			delete(s.tasks, t.rec.Name) // 一次性任务：触发后即消失
		} else {
			n, ok := nextAfter(fireTime, t.spec)
			if ok {
				t.next = n
			} else {
				t.next = time.Time{}
			}
		}
		s.dispatch(t, fireTime, false)
	}
}

// loop 按「睡到下一个触发时刻」而不是固定周期轮询：分钟级 cron 的触发延迟因此是秒级内的，
// 而不是「平均 30 秒」。
func (s *Scheduler) loop() {
	for !s.stopped() {
		now := time.Now()
		s.tick(now)
		n := s.nextWake(now)
		d := time.Until(n)
		if d < 200*time.Millisecond {
			d = 200 * time.Millisecond
		}
		select {
		case <-s.stop:
			return
		case <-time.After(d):
		}
	}
}

// nextWake 返回下一个该醒来的时刻（无任务则 1 分钟后）。
func (s *Scheduler) nextWake(now time.Time) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	earliest := now.Add(time.Minute)
	for _, t := range s.tasks {
		if !t.next.IsZero() && t.next.Before(earliest) {
			earliest = t.next
		}
	}
	return earliest
}

// ---- RPC ----

// handleAdd 注册一条周期性任务。
func (s *Scheduler) handleAdd(w http.ResponseWriter, r *http.Request) {
	args, err := decodeArgs(r)
	if err != nil {
		writeJSON(w, map[string]interface{}{"error": err.Error()})
		return
	}
	rec, err := argsToRecord(args, false)
	if err != nil {
		writeJSON(w, map[string]interface{}{"error": err.Error()})
		return
	}
	out, err := s.add(rec)
	if err != nil {
		writeJSON(w, map[string]interface{}{"error": err.Error()})
		return
	}
	writeJSON(w, map[string]interface{}{"result": out})
}

// handleAt 注册一条一次性任务（到点触发一次即消失）。
func (s *Scheduler) handleAt(w http.ResponseWriter, r *http.Request) {
	args, err := decodeArgs(r)
	if err != nil {
		writeJSON(w, map[string]interface{}{"error": err.Error()})
		return
	}
	rec, err := argsToRecord(args, true)
	if err != nil {
		writeJSON(w, map[string]interface{}{"error": err.Error()})
		return
	}
	out, err := s.add(rec)
	if err != nil {
		writeJSON(w, map[string]interface{}{"error": err.Error()})
		return
	}
	writeJSON(w, map[string]interface{}{"result": out})
}

// handleList 列出任务。刻意**不回显 payload / callback_url**：list 的输出会进 Flow 输出、
// 执行日志与 MCP 回包，把回调地址与业务载荷摊开不符合「凭据不进可观察面」。
func (s *Scheduler) handleList(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	items := make([]map[string]interface{}, 0, len(s.tasks))
	names := make([]string, 0, len(s.tasks))
	for name := range s.tasks {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		t := s.tasks[name]
		// state 只有两态，与 §4.1 的「硬绑定」同一口径：有下次触发 = scheduled，没有 = expired。
		// 刻意不输出 "idle" 之类中间态——系统里不存在「表在但没人跑」的第三态。
		state := "scheduled"
		if t.next.IsZero() {
			state = "expired"
		}
		items = append(items, map[string]interface{}{
			"name":      name,
			"expr":      t.rec.Expr,
			"at":        t.rec.At,
			"timezone":  t.rec.Timezone,
			"next_fire": fmtTime(t.next, t.loc),
			"last_fire": fmtTime(lastFireTime(t), t.loc),
			"state":     state,
			"catch_up":  t.rec.CatchUp,
			"overlap":   t.rec.Overlap,
		})
	}
	s.mu.Unlock()
	writeJSON(w, map[string]interface{}{"result": map[string]interface{}{"tasks": items}})
}

// handleCancel 按名字取消（幂等：不存在的名字报错，fail-visible）。
func (s *Scheduler) handleCancel(w http.ResponseWriter, r *http.Request) {
	args, _ := decodeArgs(r)
	name := strOr(args, "name", "")
	s.mu.Lock()
	_, ok := s.tasks[name]
	delete(s.tasks, name)
	s.mu.Unlock()
	s.save()
	if !ok {
		writeJSON(w, map[string]interface{}{"error": "任务不存在: " + name})
		return
	}
	writeJSON(w, map[string]interface{}{"result": map[string]interface{}{"ok": true, "name": name}})
}

func (s *Scheduler) handleCaps(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]interface{}{"result": map[string]interface{}{
		"ext":     "cron",
		"methods": []string{"add", "at", "list", "cancel"},
	}})
}

// add 校验并登记一条任务（add/at/载入共用）。
func (s *Scheduler) add(rec taskRecord) (map[string]interface{}, error) {
	t, err := buildTask(rec)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(rec.CallbackURL) == "" {
		// 不给默认回调：猜一个等于把触发投给一个没人订阅的地址，比直接拒更难查。
		return nil, fmt.Errorf("callback_url 必填（侧车不会替你猜往哪推）")
	}
	if u, err := url.Parse(rec.CallbackURL); err != nil || (u.Host == "" && u.Path == "") {
		return nil, fmt.Errorf("callback_url 不是合法 URL: %q", rec.CallbackURL)
	}
	s.mu.Lock()
	s.tasks[rec.Name] = t
	s.mu.Unlock()
	now := time.Now()
	s.reschedule(t, now)
	if !t.at.IsZero() && t.next.IsZero() {
		// 一次性任务且已过去、又没开 catch_up：留一条 expired，别偷偷删掉（用户要看得见）。
		log.Printf("[cron] 任务 %s 的 at 时刻已过且 catch_up 未开启，标记为已过期", rec.Name)
	}
	s.save()
	out := map[string]interface{}{
		"name": rec.Name, "expr": rec.Expr, "at": rec.At, "timezone": rec.Timezone,
		"next_fire": fmtTime(t.next, t.loc), "catch_up": rec.CatchUp, "overlap": rec.Overlap,
	}
	return out, nil
}

// ---- 小工具 ----

// argsToRecord 把 RPC 入参转成落盘记录（once=true 时要求 at 而不许给 expr）。
func argsToRecord(args map[string]interface{}, once bool) (taskRecord, error) {
	var rec taskRecord
	rec.Name = strOr(args, "name", "")
	rec.Timezone = strOr(args, "timezone", "Local")
	rec.CallbackURL = strOr(args, "callback_url", "")
	rec.CatchUp = boolOr(args, "catch_up", false)
	rec.Overlap = boolOr(args, "overlap", false)
	if m, ok := args["payload"].(map[string]interface{}); ok {
		rec.Payload = m
	}
	if once {
		rec.At = strOr(args, "at", "")
		if rec.At == "" {
			return rec, fmt.Errorf("at 必填（一次性任务）")
		}
		if strings.TrimSpace(strOr(args, "expr", "")) != "" {
			return rec, fmt.Errorf("一次性任务不许再给 expr（二选一）")
		}
	} else {
		rec.Expr = strOr(args, "expr", "")
		if rec.Expr == "" {
			return rec, fmt.Errorf("expr 必填（5 段 cron，如 */5 * * * *；一次性用 at）")
		}
		if strings.TrimSpace(strOr(args, "at", "")) != "" {
			return rec, fmt.Errorf("周期性任务不许再给 at（二选一）")
		}
	}
	return rec, nil
}

// lastFireTime 读 rec.LastFire（会在回推 goroutine 里被改写，故取锁）。
func lastFireTime(t *task) time.Time {
	t.mu.Lock()
	s := t.rec.LastFire
	t.mu.Unlock()
	if v, err := time.Parse(time.RFC3339, s); err == nil {
		return v
	}
	return time.Time{}
}

func fmtTime(t time.Time, loc *time.Location) string {
	if t.IsZero() {
		return ""
	}
	return t.In(loc).Format(time.RFC3339)
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

func strOr(m map[string]interface{}, k, def string) string {
	if v, ok := m[k].(string); ok && v != "" {
		return v
	}
	return def
}

func boolOr(m map[string]interface{}, k string, def bool) bool {
	if v, ok := m[k].(bool); ok {
		return v
	}
	return def
}

func main() {
	addr := os.Getenv("COORD_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8097"
	}
	cfg := cap.NewConfigStore(gearhome.ConfigDBPath())
	svc := newScheduler(cfg)
	if err := svc.load(); err != nil {
		log.Fatalf("cron-sidecar 载入 cron 表失败: %v", err)
	}

	c := coord.New(coord.Options{
		CheckInterval: 5 * time.Second,
		TTL:           15 * time.Second,
		OnEmpty: func() {
			log.Printf("占用列表为空，cron-sidecar 自动退出（调度随之停止，不接管）")
			svc.Stop()
			os.Exit(0)
		},
	})

	// 调度循环：与 coord 的存活判定无关地跑——进程活着就到点触发，**进程一停就彻底停**
	// （这是硬绑定的落点，别改成「停了把表留着下次接回来」）。
	log.Printf("cron-sidecar 监听 %s（cron 表 %s）", addr, svc.path)
	svc.wg.Add(1)
	go func() {
		defer svc.wg.Done()
		svc.loop()
	}()

	if err := c.Serve(addr, svc.Handlers()); err != nil {
		if coord.IsAddrInUse(err) {
			log.Printf("端口 %s 已被占用，cron-sidecar 退让退出", addr)
			os.Exit(coord.ExitAddrInUse)
		}
		log.Fatalf("cron-sidecar 异常退出: %v", err)
	}
}
