// Package registry 是 Gear 的存储层：用 SQLite 持久化 tool/flow/hook。
// S0 阶段以 JSON blob 落库（简单、契合"配置组装"哲学）；S5 版本灰度再细化表结构。
package registry

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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
func Open(path string) (*Registry, error) {
	db, err := sql.Open("sqlite", path)
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
	for _, t := range []string{"tools", "flows", "hooks"} {
		q := fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s (id TEXT PRIMARY KEY, data TEXT, updated_at INTEGER)", t)
		if _, err := r.db.Exec(q); err != nil {
			return fmt.Errorf("建表 %s 失败: %w", t, err)
		}
	}
	return nil
}

// ---- 通用 CRUD 原语 ----

func (r *Registry) upsert(table, id string, v interface{}) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	// 写锁 + 事务：D6 并发安全。同 id 并发 upsert 串行进同一事务，互不覆盖中间态。
	r.mu.Lock()
	defer r.mu.Unlock()
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := fmt.Sprintf("INSERT INTO %s(id,data,updated_at) VALUES(?,?,strftime('%%s','now')) "+
		"ON CONFLICT(id) DO UPDATE SET data=excluded.data, updated_at=excluded.updated_at", table)
	if _, err := tx.Exec(q, id, string(b)); err != nil {
		return err
	}
	return tx.Commit()
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
func (r *Registry) UpsertTool(t *model.Tool) error {
	if err := t.Validate(); err != nil {
		return err
	}
	return r.upsert("tools", t.ID, t)
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

// PromoteToolVersion 将某 Tool 的某个版本提升到目标状态（S5 灰度）。
// 校验状态迁移合法（model.CanPromote）；版本不存在 / 迁移非法均返回错误，不改变任何状态。
// 晋升成功（state 落入 testing/stable/deprecated）时记录 CreatedAt 为本次晋升时刻。
func (r *Registry) PromoteToolVersion(toolID, version string, target model.ToolVersionState) error {
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
	return r.UpsertTool(t)
}

// ---- Flows ----

// UpsertFlow 校验后落库（F3：when/parallel/use_version 等未实现字段在注册期即显式拒绝）。
func (r *Registry) UpsertFlow(f *model.Flow) error {
	if err := f.Validate(); err != nil {
		return err
	}
	return r.upsert("flows", f.ID, f)
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
func (r *Registry) DeleteTool(id string) error {
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

	r.mu.Lock()
	defer r.mu.Unlock()
	res, err := r.db.Exec("DELETE FROM tools WHERE id=?", id)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("Tool 不存在: %s: %w", id, ErrNotFound)
	}
	return nil
}

// DeleteFlow 注销 Flow，并按 PRD §8.2 的守卫拒绝危险删除：
//  1. 被任何其它 Flow 的 step 引用（作为 call）→ 拒绝
//  2. 被任何 Hook 的 action 引用（Flow 载体，#72 起 before_flow/on_success 可指向 Flow）→ 拒绝
//  3. 正被**运行中的执行**作为 Hook 的 Flow 载体使用 → 拒绝（由注入的 RunningChecker 判定）
//
// 第 3 条不是第 1/2 条的重复：静态引用会在 Hook 被删 / Flow 被改写后消失，而**正在跑的执行**
// 仍持有启动时的 Hook 快照 —— 此时放行注销会让它在 before_flow 触发时拿到 nil Flow 而中途失败。
func (r *Registry) DeleteFlow(id string) error {
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

	r.mu.Lock()
	defer r.mu.Unlock()
	res, err := r.db.Exec("DELETE FROM flows WHERE id=?", id)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("Flow 不存在: %s: %w", id, ErrNotFound)
	}
	return nil
}

// ---- Hooks ----

// UpsertHook 校验后落库（F3：stage 非法 / before_promote / action 为空等在注册期即拒绝）。
func (r *Registry) UpsertHook(h *model.Hook) error {
	if err := h.Validate(); err != nil {
		return err
	}
	return r.upsert("hooks", h.ID, h)
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
func (r *Registry) DeleteHook(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	res, err := r.db.Exec("DELETE FROM hooks WHERE id=?", id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("Hook 不存在: %s: %w", id, ErrNotFound)
	}
	return nil
}
