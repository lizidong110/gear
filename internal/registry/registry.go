// Package registry 是 Gear 的存储层：用 SQLite 持久化 tool/flow/hook。
// S0 阶段以 JSON blob 落库（简单、契合"配置组装"哲学）；S5 版本灰度再细化表结构。
package registry

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // 纯 Go SQLite 驱动，注册名 "sqlite"，无需 cgo

	"gear/internal/model"
)

// 语义化错误：供上层（REST / CLI）映射为 404 / 409，而不是靠匹配错误字符串。
var (
	// ErrNotFound 表示目标对象不存在。
	ErrNotFound = errors.New("not found")
	// ErrInUse 表示目标对象仍被引用，不允许删除（PRD §8.2）。
	ErrInUse = errors.New("in use")
)

// RunningChecker 报告某 Tool 是否正被**运行中的执行**使用：返回非空字符串即为「在用」，
// 内容作为拒绝原因（PRD §8.2 第三条守卫）。
//
// 为什么把它做成注入点而不是让 registry 自己查：执行状态属于上层（server 的 ExecStore /
// app 监管），registry 反向依赖它会造成依赖倒置与包循环。registry 只负责「问一句」。
type RunningChecker func(toolID string) string

// Registry 持有一个 SQLite 连接。
// mu 串行化所有写操作，配合事务保证并发 upsert 不损坏（D6）。
type Registry struct {
	db *sql.DB
	mu sync.Mutex

	// runningCheck 由上层注入（可为 nil = 不做运行中检查）。仅在启动期 Set 一次，
	// 读写都经 mu 保护，避免与 DeleteTool 并发时出现数据竞争。
	runningCheck RunningChecker
}

// Open 打开（必要时创建）registry 数据库并完成建表。
//
// DSN 必须带 busy_timeout + WAL + _txlock=immediate：多个 gear 进程（CLI / serve /
// app run）可能同时打开同一个 registry.db。没有 busy_timeout 时任何并发写都会立即报
// SQLITE_BUSY（database is locked）；没有 WAL 时读写互斥更严重；没有 _txlock 则会在
// 另一个坑上翻车，见下。与 internal/server/execstore.go 的 ExecStore 同一姿势
// （2026-09-24 修复：第二个 `gear app run` 初始化扩展时撞锁直接退出）。
//
// **为什么必须是 immediate 而不是默认的 deferred**：database/sql 的 BeginTx 发出的是裸
// `BEGIN`（deferred），先跑一个 SELECT 再改数据的写法会走「读锁升写锁」。SQLite 对这种
// 升级冲突**不调用 busy handler**——也就是说 busy_timeout=5000 在这条路上完全不生效，
// 直接甩 SQLITE_BUSY。这一点平时看不出来，因为 deferred 事务的写窗口越短越不容易撞上；
// 一旦 upsert 里多做了事（2026-09-26 下沉快照：多 1 次 SELECT + 1 次 INSERT），窗口变宽，
// TestMultiProcessConcurrentWrite 立刻从绿转红。immediate 在 BEGIN 当场就拿写锁，
// busy handler 这才轮到它干活。
func Open(path string) (*Registry, error) {
	// path 可能本身已带 query（如 ai.go 传 "file:...?mode=memory&cache=shared"），
	// 追加 pragma 时按有无 "?" 选分隔符，不能无脑拼 "?"。
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	db, err := sql.Open("sqlite", path+sep+
		"_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_txlock=immediate")
	if err != nil {
		return nil, fmt.Errorf("打开 registry 失败: %w", err)
	}
	r := &Registry{db: db}
	if err := r.migrate(); err != nil {
		return nil, err
	}
	return r, nil
}

// Close 关闭连接。
func (r *Registry) Close() error { return r.db.Close() }

func (r *Registry) migrate() error {
	for _, t := range []string{"tools", "flows", "hooks", "apps"} {
		q := fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s (id TEXT PRIMARY KEY, data TEXT, updated_at INTEGER)", t)
		if _, err := r.db.Exec(q); err != nil {
			return fmt.Errorf("建表 %s 失败: %w", t, err)
		}
	}
	// definition_history 是「定义变更的留旧」层（IDENTITY_CONTEXT_DESIGN.md §5/§9）。
	//
	// 为什么在 upsert **内部**快照，而不是让每个调用点各自记得先存一份：
	// registry.upsert 是 ON CONFLICT DO UPDATE 的**整行覆盖**，改错了只能手工重建；而写入
	// 有 9+ 处旁路直连点（cli/tool.go、cli/flow.go、server 的 create handler、apply、import…），
	// 每一处都靠纪律去快照，漏一个就是一个静默的不可回滚点。下沉到唯一的写入口，
	// 「留旧」才是结构性保证而不是约定。
	_, err := r.db.Exec("CREATE TABLE IF NOT EXISTS definition_history (" +
		"seq INTEGER PRIMARY KEY AUTOINCREMENT, " +
		"kind TEXT NOT NULL, id TEXT NOT NULL, " +
		"data TEXT, actor TEXT NOT NULL DEFAULT '', at INTEGER NOT NULL, note TEXT NOT NULL DEFAULT '')")
	if err != nil {
		return fmt.Errorf("建表 definition_history 失败: %w", err)
	}
	return nil
}

// kindTable / tableKind 是「kind ↔ 表名」的唯一映射。
var (
	kindTable = map[string]string{"tool": "tools", "flow": "flows", "hook": "hooks", "app": "apps"}
	tableKind = map[string]string{"tools": "tool", "flows": "flow", "hooks": "hook", "apps": "app"}
)

// ---- 通用 CRUD 原语 ----

// upsertOptions 是写入的附加参数。
type upsertOptions struct {
	// actor 是本次变更的发起者（写入 definition_history.actor）。
	actor string
}

// UpsertOption 调整一次写入的附加行为。刻意做成**变长可变参数**：registry 的 Upsert*
// 有 9+ 处调用点，改成必填参数会把每一个都动一遍，反而没人愿意加身份信息。
type UpsertOption func(*upsertOptions)

// withOpts 把调用点传来的可变参数收成一个 upsertOptions 值。
func withOpts(opts []UpsertOption) upsertOptions {
	var o upsertOptions
	for _, f := range opts {
		if f != nil {
			f(&o)
		}
	}
	return o
}

// WithActor 标记「这次变更是谁做的」，落到 history 的 actor 列（追责用；不传即空串）。
func WithActor(actor string) UpsertOption {
	return func(o *upsertOptions) { o.actor = actor }
}

// upsert 是唯一的写入口，也是唯一的快照点。
func (r *Registry) upsert(table, id string, v interface{}, opts upsertOptions) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	// 写锁 + 事务：D6 并发安全。同 id 并发 upsert 串行进同一事务，互不覆盖中间态。
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.withTx(func(tx *sql.Tx) error {
		// 先记「变更前」的状态，再改写。同一事务 ⇒ 快照与写入要么都成，要么都不成。
		if err := snapshotBefore(tx, table, id, opts.actor, ""); err != nil {
			return err
		}
		q := fmt.Sprintf("INSERT INTO %s(id,data,updated_at) VALUES(?,?,strftime('%%s','now')) "+
			"ON CONFLICT(id) DO UPDATE SET data=excluded.data, updated_at=excluded.updated_at", table)
		_, err := tx.Exec(q, id, string(b))
		return err
	})
}

// withTx 开启事务并在返回前按结果提交/回滚（defer Rollback 在 Commit 后是 no-op）。
//
// 抽出来的理由：upsert 与 RestoreSnapshot 都需要「同一个事务里既快照又改写」，
// 各写一遍 begin/defer 的样板只会让两处的提交点不一致。
func (r *Registry) withTx(fn func(tx *sql.Tx) error) error {
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// noteDelete 是删除留在 definition_history 里的 note。
// 刻意与 upsert 的空 note 区分开：`gear history list` 要能一眼看出这一行是「谁把它删了」。
const noteDelete = "delete"

// deleteWithSnapshot 在删除前记一条历史，再删。
//
// 为什么删除也要留痕（2026-09-26，③ 审计）：④ 只把快照下沉进了 upsert，而**删除根本不走
// upsert** —— 结果是 definition_history 里查不到任何一次删除：「谁删的」答不了，而且删掉的
// 时刻在历史链上连一个位置都没有，事后无从还原时间线。
//
// data **保留**被删内容（区别于「当时不存在」的 NULL），由此「撤销一次删除」不需要新代码：
// RestoreSnapshot(这条 seq) 会把对象原样写回 —— 回滚语义天然覆盖了它。
//
// 同一事务 ⇒ 留痕与删除要么都成，要么都不成；进程被强杀也不会留下「删了却没记账」的行。
//
// 调用方需已通过各自的引用守卫，且本方法会自行取 r.mu。
func (r *Registry) deleteWithSnapshot(table, id string, opts upsertOptions) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.withTx(func(tx *sql.Tx) error {
		if err := snapshotBefore(tx, table, id, opts.actor, noteDelete); err != nil {
			return err
		}
		res, err := tx.Exec("DELETE FROM "+table+" WHERE id=?", id)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			// 目标本就不存在：报错**并让 withTx 回滚** ⇒ 不留痕。
			// 下沉进事务后这层判据得自己拿回来——DELETE 对不存在的行「成功」但影响 0 行，
			// 不查 RowsAffected 的话重复删除会静默 no-op（回归：TestHookLifecycle 逮到）。
			kind := tableKind[table]
			return fmt.Errorf("%s 不存在: %s: %w", strings.ToUpper(kind[:1])+kind[1:], id, ErrNotFound)
		}
		return nil
	})
}

// snapshotBefore 在改写某对象**之前**记一条历史：data 是它当前生效的内容，
// NULL 表示「当时这个对象还不存在」（这类历史 restore 时会执行删除）。
//
// 调用方必须已持有 r.mu 且正处于本事务内。
func snapshotBefore(tx *sql.Tx, table, id, actor, note string) error {
	var old sql.NullString
	// 无行时 Scan 返回 ErrNoRows，此处要的正是「old 保持 NULL」（= 当时不存在），忽略它。
	_ = tx.QueryRow("SELECT data FROM "+table+" WHERE id=?", id).Scan(&old)
	_, err := tx.Exec("INSERT INTO definition_history(kind,id,data,actor,at,note) VALUES(?,?,?,?,?,?)",
		tableKind[table], id, old, actor, time.Now().UnixMilli(), note)
	return err
}

func (r *Registry) get(table, id string, out interface{}) error {
	var data string
	q := fmt.Sprintf("SELECT data FROM %s WHERE id=?", table)
	err := r.db.QueryRow(q, id).Scan(&data)
	if err == sql.ErrNoRows {
		return sql.ErrNoRows
	}
	if err != nil {
		return err
	}
	return json.Unmarshal([]byte(data), out)
}

func (r *Registry) list(table string, decode func(string) error) error {
	q := fmt.Sprintf("SELECT data FROM %s", table)
	rows, err := r.db.Query(q)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return err
		}
		if err := decode(data); err != nil {
			return err
		}
	}
	return rows.Err()
}

// ---- Tools ----

// UpsertTool 校验后落库（D2/D5 最小真实校验：version 必须合法 SemVer、state 必须枚举、
// input/output schema 存在且 type=object）。非法 Tool 拒绝入库。
func (r *Registry) UpsertTool(t *model.Tool, opts ...UpsertOption) error {
	if err := t.Validate(); err != nil {
		return err
	}
	// 声明式 REST 路由（FR-3）的**跨 bundle** 守卫：bundle 内冲突在 artifact 层挡，
	// 但「应用 A 与后来导入的应用 B 撞同一 method+path」那条路看不见——两次 apply 是
	// 两个事务、两份 bundle。同名 pattern 第二次注册会让 net/http panic（实测 go1.27），
	// 所以宁可在这里拒，也不让它在起服时炸。
	if t.API != nil {
		if err := r.routeOwnerConflict(t); err != nil {
			return err
		}
	}
	return r.upsert("tools", t.ID, t, withOpts(opts))
}

// routeOwnerConflict 找出与本条声明撞在同一个 method+path 上的**另一个** Tool。
func (r *Registry) routeOwnerConflict(t *model.Tool) error {
	tools, err := r.ListTools()
	if err != nil {
		return err
	}
	for _, o := range tools {
		if o.ID == t.ID || o.API == nil {
			continue
		}
		if o.API.Method == t.API.Method && o.API.Path == t.API.Path {
			return fmt.Errorf("Tool %s 声明的 api 路由 %s 与 Tool %s 重复：同一 method+path 只能有一个，"+
				"两个应用不能同时对外提供同一个资源路径（本次未写入，请给其中一个换个 path）",
				t.ID, t.API.Pattern(), o.ID)
		}
	}
	return nil
}
func (r *Registry) GetTool(id string) (*model.Tool, error) {
	t := &model.Tool{}
	if err := r.get("tools", id, t); err == sql.ErrNoRows {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	return t, nil
}
func (r *Registry) ListTools() ([]*model.Tool, error) {
	var out []*model.Tool
	err := r.list("tools", func(d string) error {
		t := &model.Tool{}
		if e := json.Unmarshal([]byte(d), t); e != nil {
			return e
		}
		out = append(out, t)
		return nil
	})
	return out, err
}

// ToolsSignature 是 Tool 表的廉价指纹（行数 + 最大 updated_at）。
//
// 用途：上层（server 的声明式 REST 路由索引）靠它判断「索引要不要重建」，
// 从而不必让**每个请求**都把整张 tools 表读出来反序列化一遍——一次 SQLite 的
// COUNT+MAX 比一次 SELECT id,data 便宜一个数量级。
func (r *Registry) ToolsSignature() (string, error) {
	var sig string
	if err := r.db.QueryRow(`SELECT COUNT(*) || ':' || COALESCE(MAX(updated_at), 0) FROM tools`).Scan(&sig); err != nil {
		return "", err
	}
	return sig, nil
}

// PromoteToolVersion 将某 Tool 的某个版本提升到目标状态（S5 灰度）。
// 校验状态迁移合法（model.CanPromote）；版本不存在 / 迁移非法均返回错误，不改变任何状态。
// 晋升成功（state 落入 testing/stable/deprecated）时记录 CreatedAt 为本次晋升时刻。
func (r *Registry) PromoteToolVersion(toolID, version string, target model.ToolVersionState, opts ...UpsertOption) error {
	t, err := r.GetTool(toolID)
	if err != nil {
		return err
	}
	if t == nil {
		return fmt.Errorf("Tool 不存在: %s: %w", toolID, ErrNotFound)
	}
	idx := -1
	for i, v := range t.Versions {
		if v.Version == version {
			idx = i
			break
		}
	}
	if idx < 0 {
		return fmt.Errorf("Tool %s 不存在版本 %s: %w", toolID, version, ErrNotFound)
	}
	cur := t.Versions[idx].State
	if !model.CanPromote(cur, target) {
		return fmt.Errorf("Tool %s 版本 %s 状态迁移非法: %s -> %s", toolID, version, cur, target)
	}
	t.Versions[idx].State = target
	if target == model.StateStable || target == model.StateTesting || target == model.StateDeprecated {
		t.Versions[idx].CreatedAt = time.Now()
	}
	// 重新校验整 Tool（含 version 合法状态），再落库。
	// opts 透传：晋升也是一种「谁改的」，不能因为它是状态迁移就不留 actor。
	return r.UpsertTool(t, opts...)
}

// ---- Flows ----

// UpsertFlow 校验后落库（F3：when/parallel/use_version 等未实现字段在注册期即显式拒绝）。
func (r *Registry) UpsertFlow(f *model.Flow, opts ...UpsertOption) error {
	if err := f.Validate(); err != nil {
		return err
	}
	return r.upsert("flows", f.ID, f, withOpts(opts))
}
func (r *Registry) GetFlow(id string) (*model.Flow, error) {
	f := &model.Flow{}
	if err := r.get("flows", id, f); err == sql.ErrNoRows {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	return f, nil
}
func (r *Registry) ListFlows() ([]*model.Flow, error) {
	var out []*model.Flow
	err := r.list("flows", func(d string) error {
		f := &model.Flow{}
		if e := json.Unmarshal([]byte(d), f); e != nil {
			return e
		}
		out = append(out, f)
		return nil
	})
	return out, err
}

// SetRunningChecker 注入「运行中」检查器（PRD §8.2 第三条守卫）。
// 应在开始并发服务前调用一次；传 nil 表示不启用该守卫。
func (r *Registry) SetRunningChecker(c RunningChecker) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runningCheck = c
}

// runningReason 询问注入的检查器。
// 注意：**不持有锁**调用 c —— 检查器可能回调上层的 store，持锁会造成无谓的锁序风险。
func (r *Registry) runningReason(toolID string) string {
	r.mu.Lock()
	c := r.runningCheck
	r.mu.Unlock()
	if c == nil {
		return ""
	}
	return c(toolID)
}

// DeleteTool 注销 Tool，并按 PRD §8.2 的守卫拒绝危险删除：
//  1. 被任何 Flow 的 step 引用 → 拒绝
//  2. 被任何 Hook 的 action 引用 → 拒绝
//  3. 正被**运行中的执行**使用 → 拒绝（由注入的 RunningChecker 判定）
//
// 第 3 条不是第 1/2 条的重复：静态引用会在「Hook 被删除」「Flow 被重新注册去掉了该 step」
// 之后消失，而**正在跑的执行**仍持有启动时的 Hook 快照与 Flow —— 此时放行注销会让它
// 在下次触发 Hook 时拿到 nil Tool 而中途失败。运行期引用只能由运行侧上报。
//
// 返回的错误会指明「被谁引用」，让调用方能直接定位。
func (r *Registry) DeleteTool(id string, opts ...UpsertOption) error {
	t, err := r.GetTool(id)
	if err != nil {
		return err
	}
	if t == nil {
		return fmt.Errorf("Tool 不存在: %s: %w", id, ErrNotFound)
	}

	flows, err := r.ListFlows()
	if err != nil {
		return err
	}
	for _, f := range flows {
		for _, s := range f.Steps {
			if s.Call == id {
				return fmt.Errorf("Tool %s 被 Flow %s 的 step %s 引用，无法注销: %w", id, f.ID, s.ID, ErrInUse)
			}
		}
	}

	hooks, err := r.ListHooks()
	if err != nil {
		return err
	}
	for _, h := range hooks {
		if h.Action == id {
			return fmt.Errorf("Tool %s 被 Hook %s 的 action 引用，无法注销: %w", id, h.ID, ErrInUse)
		}
	}

	// 第 3 条守卫：运行中的执行仍可能用到它 —— 即使 Registry 里已看不到任何引用。
	if reason := r.runningReason(id); reason != "" {
		return fmt.Errorf("Tool %s %s，无法注销: %w", id, reason, ErrInUse)
	}

	return r.deleteWithSnapshot("tools", id, withOpts(opts))
}

// DeleteFlow 注销 Flow，并按 PRD §8.2 的守卫拒绝危险删除：
//  1. 被任何其它 Flow 的 step 引用（作为 call）→ 拒绝
//  2. 被任何 Hook 的 action 引用（Flow 载体，#72 起 before_flow/on_success 可指向 Flow）→ 拒绝
//  3. 正被**运行中的执行**作为 Hook 的 Flow 载体使用 → 拒绝（由注入的 RunningChecker 判定）
//  4. 是某个 App 的 entry_flow → 拒绝（否则 App 定义悬空，见下方注释）
//
// 第 3 条不是第 1/2 条的重复：静态引用会在 Hook 被删 / Flow 被改写后消失，而**正在跑的执行**
// 仍持有启动时的 Hook 快照 —— 此时放行注销会让它在 before_flow 触发时拿到 nil Flow 而中途失败。
//
// 第 4 条与前三条形质不同：前三条是「别人还在用我」，第 4 条是「删了我之后有人会悬空」。
// App.EntryFlow 只是一个字符串，apps 表与 flows 表之间没有外键，注销 Flow 不会留下任何
// 痕迹——App 定义会指向一个不存在的 Flow，直到 `app start` 那一刻才炸，而那时早已看不出
// 是谁的哪次操作造成的（2026-09-27 实证：删掉 slice.up 后 slice App 悬空，起服才暴露）。
// 拒绝比留悬空强：改 entry_flow 或注销该 App 都是一次显式操作。
func (r *Registry) DeleteFlow(id string, opts ...UpsertOption) error {
	f, err := r.GetFlow(id)
	if err != nil {
		return err
	}
	if f == nil {
		return fmt.Errorf("Flow 不存在: %s: %w", id, ErrNotFound)
	}

	flows, err := r.ListFlows()
	if err != nil {
		return err
	}
	for _, other := range flows {
		if other.ID == id {
			continue
		}
		for _, s := range other.Steps {
			if s.Call == id {
				return fmt.Errorf("Flow %s 被 Flow %s 的 step %s 引用，无法注销: %w", id, other.ID, s.ID, ErrInUse)
			}
		}
	}

	hooks, err := r.ListHooks()
	if err != nil {
		return err
	}
	for _, h := range hooks {
		// 仅 Flow 级 stage 的 action 可能指向 Flow（step 级 action 指向 Tool，不会误伤同名 Flow）。
		if model.IsFlowLevelStage(h.Stage) && h.Action == id {
			return fmt.Errorf("Flow %s 被 Hook %s（stage=%s）的 action 引用，无法注销: %w", id, h.ID, h.Stage, ErrInUse)
		}
	}

	// 第 3 条守卫：运行中的执行仍可能用到它。
	if reason := r.runningReason(id); reason != "" {
		return fmt.Errorf("Flow %s %s，无法注销: %w", id, reason, ErrInUse)
	}

	// 第 4 条守卫：它是某个 App 的入口 Flow 吗（悬空 entry_flow，见函数注释）。
	apps, err := r.ListApps()
	if err != nil {
		return err
	}
	var owners []string
	for _, a := range apps {
		if a.EntryFlow == id {
			owners = append(owners, a.ID)
		}
	}
	if len(owners) > 0 {
		return fmt.Errorf("Flow %s 是 App %s 的 entry_flow，无法注销"+
			"（先注销该 App：`gear app delete %s` / MCP `gear_delete(kind=app, id=%s)`，"+
			"或把该 App 的 entry_flow 指向别处）: %w",
			id, strings.Join(owners, ", "), owners[0], owners[0], ErrInUse)
	}

	return r.deleteWithSnapshot("flows", id, withOpts(opts))
}

// ---- Hooks ----

// UpsertHook 校验后落库（F3：stage 非法 / before_promote / action 为空等在注册期即拒绝）。
func (r *Registry) UpsertHook(h *model.Hook, opts ...UpsertOption) error {
	if err := h.Validate(); err != nil {
		return err
	}
	return r.upsert("hooks", h.ID, h, withOpts(opts))
}
func (r *Registry) GetHook(id string) (*model.Hook, error) {
	h := &model.Hook{}
	if err := r.get("hooks", id, h); err == sql.ErrNoRows {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	return h, nil
}
func (r *Registry) ListHooks() ([]*model.Hook, error) {
	var out []*model.Hook
	err := r.list("hooks", func(d string) error {
		h := &model.Hook{}
		if e := json.Unmarshal([]byte(d), h); e != nil {
			return e
		}
		out = append(out, h)
		return nil
	})
	return out, err
}

// DeleteHook 删除一个 Hook；不存在时返回错误（避免静默 no-op）。
// 删除同样留痕（见 deleteWithSnapshot）。
func (r *Registry) DeleteHook(id string, opts ...UpsertOption) error {
	return r.deleteWithSnapshot("hooks", id, withOpts(opts))
}

// ---- Apps ----
//
// 2026-09-26 新增。App 是 bundle 的 `kind: App` 清单，此前**刻意不入 registry**，
// 后果是「用户构建的产品」在 gear 里没有对应对象（详见 internal/model/app.go 的包注释）。
// 表结构与 tools/flows/hooks 完全同构（id + data blob + updated_at），复用同一套 CRUD 原语。

// UpsertApp 校验后落库（App.ID 与 entry_flow 必填，见 model.App.Validate）。
func (r *Registry) UpsertApp(a *model.App, opts ...UpsertOption) error {
	if err := a.Validate(); err != nil {
		return err
	}
	return r.upsert("apps", a.ID, a, withOpts(opts))
}
func (r *Registry) GetApp(id string) (*model.App, error) {
	a := &model.App{}
	if err := r.get("apps", id, a); err == sql.ErrNoRows {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	return a, nil
}
func (r *Registry) ListApps() ([]*model.App, error) {
	var out []*model.App
	err := r.list("apps", func(d string) error {
		a := &model.App{}
		if e := json.Unmarshal([]byte(d), a); e != nil {
			return e
		}
		out = append(out, a)
		return nil
	})
	return out, err
}

// ---- 定义变更历史（④留旧/回滚，IDENTITY_CONTEXT_DESIGN.md §5）----

// HistoryRow 是 definition_history 的一行：某个对象在某一时刻**生效**的状态。
//
// 语义是「那时它是这样的」，不是「那时它被改成了」——restore 站在「想回到哪个状态」这一侧，
// 传进来的 seq 直接就是那一行。At 是 unix 毫秒。
type HistoryRow struct {
	Seq   int    `json:"seq"`
	Kind  string `json:"kind"`
	ID    string `json:"id"`
	Actor string `json:"actor"`
	At    int64  `json:"at"`
	Note  string `json:"note"`
}

// ListHistory 列出某个对象的变更历史（**新的在前**），供回滚时挑 seq。
//
// kind/id 留空表示不筛。limit<=0 按 20 处理（CLI 默认看最近 20 条）。
func (r *Registry) ListHistory(kind, id string, limit int) ([]HistoryRow, error) {
	if limit <= 0 {
		limit = 20
	}
	kind, id = strings.TrimSpace(kind), strings.TrimSpace(id)
	q := "SELECT seq,kind,id,actor,at,note FROM definition_history"
	var args []any
	if kind != "" || id != "" {
		q += " WHERE 1=1"
		if kind != "" {
			q += " AND kind=?"
			args = append(args, kind)
		}
		if id != "" {
			q += " AND id=?"
			args = append(args, id)
		}
	}
	q += " ORDER BY seq DESC LIMIT ?"
	args = append(args, limit)
	rows, err := r.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []HistoryRow{}
	for rows.Next() {
		var h HistoryRow
		if err := rows.Scan(&h.Seq, &h.Kind, &h.ID, &h.Actor, &h.At, &h.Note); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// RestoreSnapshot 把某个历史点的状态写回 registry——**定义变更的回滚**。
//
// seq 指向「你想回到哪个状态」的那一行；该行 data 为 NULL（当时对象不存在）时执行删除，
// 于是「撤销一次新建」也是同一条通道。
//
// 三个必须说清的语义：
//  1. **回滚本身会被再次快照**：写回前先把当前状态记一条（note=`restore from <seq>`），
//     所以链路永远是线性的、能一路往前回滚，不会出现「回滚后原来的坏状态消失」。
//  2. **写回内容会重新校验**：registry 只存合法定义这条不变式不能因为回滚破掉；
//     若快照里的定义在当前 model 规则下已不合法（规则收紧过），整次回滚**原子失败**，
//     一个字节都不写，并给出「这条快照太旧」的提示。宁可拒绝，不给用户一个跑不起来的回滚。
//  3. restore 不是「删历史/改 id」，它就是一个普通写入：被回滚的对象 id 不变。
func (r *Registry) RestoreSnapshot(seq int) (HistoryRow, error) {
	if seq <= 0 {
		return HistoryRow{}, fmt.Errorf("快照序号必须为正数: %w", ErrNotFound)
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	var out HistoryRow
	err := r.withTx(func(tx *sql.Tx) error {
		var kind, id, note string
		var data, old sql.NullString
		if err := tx.QueryRow("SELECT kind,id,data,note FROM definition_history WHERE seq=?",
			seq).Scan(&kind, &id, &data, &note); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("快照 %d 不存在（用 `gear history list` 看有哪些 seq）: %w", seq, ErrNotFound)
			}
			return err
		}
		table, ok := kindTable[kind]
		if !ok {
			return fmt.Errorf("快照 %d 的 kind %q 无法识别", seq, kind)
		}
		// 写回前先留一条「当前状态」：回滚本身也是一次变更，也要留痕，链路才是线性的。
		_ = tx.QueryRow("SELECT data FROM "+table+" WHERE id=?", id).Scan(&old)
		if _, err := tx.Exec("INSERT INTO definition_history(kind,id,data,actor,at,note) VALUES(?,?,?,?,?,?)",
			kind, id, old, "", time.Now().UnixMilli(), fmt.Sprintf("restore from %d", seq)); err != nil {
			return err
		}
		if data.Valid {
			// 重新校验：规则可能在这条快照之后收紧过，那时宁可拒绝回滚，
			// 也不把一个跑不起来的定义放回 registry（事务保证这里失败=什么都没发生）。
			if err := validateBlob(kind, data.String); err != nil {
				return fmt.Errorf("快照 %d 的内容在当前规则下已不合法，无法回滚（%w）；"+
					"要么改回去，要么手工重建", seq, err)
			}
			q := fmt.Sprintf("INSERT INTO %s(id,data,updated_at) VALUES(?,?,strftime('%%s','now')) "+
				"ON CONFLICT(id) DO UPDATE SET data=excluded.data, updated_at=excluded.updated_at", table)
			if _, err := tx.Exec(q, id, data.String); err != nil {
				return err
			}
		} else {
			// 当时这个对象还不存在 ⇒ 回滚就是「撤回这次新建」。
			if _, err := tx.Exec("DELETE FROM "+table+" WHERE id=?", id); err != nil {
				return err
			}
		}
		out = HistoryRow{Seq: seq, Kind: kind, ID: id, Note: note}
		return nil
	})
	if err != nil {
		return HistoryRow{}, err
	}
	return out, nil
}

// validateBlob 按 kind 解出一个定义并校验（回滚专用：只信任当前规则）。
func validateBlob(kind, data string) error {
	switch kind {
	case "tool":
		var t model.Tool
		if err := json.Unmarshal([]byte(data), &t); err != nil {
			return err
		}
		return t.Validate()
	case "flow":
		var f model.Flow
		if err := json.Unmarshal([]byte(data), &f); err != nil {
			return err
		}
		return f.Validate()
	case "hook":
		var h model.Hook
		if err := json.Unmarshal([]byte(data), &h); err != nil {
			return err
		}
		return h.Validate()
	case "app":
		var a model.App
		if err := json.Unmarshal([]byte(data), &a); err != nil {
			return err
		}
		return a.Validate()
	default:
		return fmt.Errorf("未知对象类型 %q", kind)
	}
}

// 刻意**不做**引用守卫：App 只是聚合描述，它引用的 Tool/Flow 是独立注册的实体，
// 删清单不等于删应用；反过来先删 Flow 再删清单也无害。若这里加了守卫，
// 「想删掉一个废弃应用」就得先拆掉它的所有 Tool，反而堵死清理路径。
func (r *Registry) DeleteApp(id string, opts ...UpsertOption) error {
	return r.deleteWithSnapshot("apps", id, withOpts(opts))
}

// DeleteAppCascade 级联注销 App：删 App 清单 + 其拥有的全部 Tool/Flow/Hook（连根拔起）。
//
// 与 DeleteApp（只删清单、刻意不级联）相对，这是 CLI `gear app delete --cascade` 的落地。
// 归属沿用 model.App.Owns 这一份唯一规则（id 以 `<app>__` 或 `<app>.` 开头）。
//
// 守卫取舍（2026-11-11）：
//   - 保留「运行中」守卫（runningReason）：正跑的不许删，与单次删除一致。
//   - 保留「被**其它** App 引用」守卫：删 Tool 走 DeleteTool，它查全部 Flow/Hook 引用，
//     跨 App 的引用仍会拒删——避免误删别人正在用的工具。
//   - 绕过 entry_flow 守卫（DeleteFlow 第 4 条）：级联删 App 时 entry_flow 悬空本就是预期，
//     没必要拦。
//   - 绕过「同 App 内引用」守卫：同 App 的 Flow/Tool 随 App 一起删，内部引用不构成悬空。
//
// 顺序：Flow → Hook → Tool → App → 历史。
//
//	Flow 先删（清掉它们对 Tool/Hook 的引用），再 Hook（无守卫），最后 Tool（DeleteTool 此时
//	只剩跨 App 引用会被拦），最后删 App 清单本身，最后把这一切在 definition_history 里的
//	历史一并清掉（见下方段落）。
//
// 历史清理（2026-11-12，修复「连根拔起」名不副实）：级联删把对象从各表物理删除后，
// 若留下 definition_history 行，`gear history restore` 仍能按 seq 把已删对象原样写回——
// 等于没删干净；且 `gear history list` 里满屏已不存在的对象，排障时误导。所以末一步把
// App + 全部子资源的 history 一并 purge（含 deleteWithSnapshot 本次写的 note=delete 留痕）。
//
// 任一子资源删除被守卫拒绝即整批失败（fail-closed，与 deleteAll 语义一致）；已删部分不回滚，
// 但本函数会在返回错误前把已删部分的 history 也清掉，避免「registry 没了、history 还能还原」。
// 返回被删 id 列表（含 App 自身，末位）。
func (r *Registry) DeleteAppCascade(id string, opts ...UpsertOption) ([]string, error) {
	app, err := r.GetApp(id)
	if err != nil {
		return nil, err
	}
	if app == nil {
		return nil, fmt.Errorf("App 不存在: %s: %w", id, ErrNotFound)
	}

	tools, err := r.ListTools()
	if err != nil {
		return nil, err
	}
	flows, err := r.ListFlows()
	if err != nil {
		return nil, err
	}
	hooks, err := r.ListHooks()
	if err != nil {
		return nil, err
	}

	var removed []string

	// abort 在任一步失败时调用：deleteWithSnapshot / DeleteTool 各自提交，已删部分的
	// registry 行已落盘；这里把它们的历史也清掉，避免「registry 没了但 history 还能还原」的幽灵态。
	abort := func(e error) ([]string, error) {
		_ = r.purgeHistory(removed)
		return removed, e
	}

	// 1) Flow：绕过 entry_flow 守卫（App 即将被删），保留运行中守卫。
	for _, f := range flows {
		if !app.Owns(f.ID) {
			continue
		}
		if reason := r.runningReason(f.ID); reason != "" {
			return abort(fmt.Errorf("Flow %s %s，无法级联注销: %w", f.ID, reason, ErrInUse))
		}
		if err := r.deleteWithSnapshot("flows", f.ID, withOpts(opts)); err != nil {
			return abort(err)
		}
		removed = append(removed, f.ID)
	}

	// 2) Hook：无引用守卫（与 DeleteHook 一致），保留运行中守卫。
	for _, h := range hooks {
		if !app.Owns(h.ID) {
			continue
		}
		if reason := r.runningReason(h.ID); reason != "" {
			return abort(fmt.Errorf("Hook %s %s，无法级联注销: %w", h.ID, reason, ErrInUse))
		}
		if err := r.deleteWithSnapshot("hooks", h.ID, withOpts(opts)); err != nil {
			return abort(err)
		}
		removed = append(removed, h.ID)
	}

	// 3) Tool：走 DeleteTool，跨 App 引用与运行中守卫照常生效；同 App 内引用已随 Flow/Hook 删掉。
	for _, t := range tools {
		if !app.Owns(t.ID) {
			continue
		}
		if err := r.DeleteTool(t.ID, opts...); err != nil {
			return abort(err)
		}
		removed = append(removed, t.ID)
	}

	// 4) App 清单本身。
	if err := r.DeleteApp(id, opts...); err != nil {
		return abort(err)
	}
	removed = append(removed, id)

	// 5) 历史：App + 全部子资源的 definition_history 一并清掉（连根拔起）。
	if err := r.purgeHistory(removed); err != nil {
		return removed, err
	}
	return removed, nil
}

// purgeHistory 删除 definition_history 中 id 命中给定列表的全部历史行。
//
// 仅由 DeleteAppCascade 在「对象已从各表物理删除」之后调用——否则历史会与存活对象脱节，
// 且 note=delete 这类留痕行不该孤立存在。入参为空直接返回，避免拼出 `IN ()` 这种非法 SQL。
func (r *Registry) purgeHistory(ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_, err := r.db.Exec("DELETE FROM definition_history WHERE id IN ("+ph+")", args...)
	return err
}
