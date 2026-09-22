package server

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"gear/internal/engine"
	"gear/internal/gearhome"

	_ "modernc.org/sqlite"
)

// 执行状态（对外可见）。
const (
	StatusRunning   = "running"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
	// StatusRejected 表示执行因入口繁忙被拒绝（REQ-CONCURRENCY-CROSS / #73）：
	// 超出并发上限且队列已满，未真正运行。
	StatusRejected = "rejected"
)

// Execution 是一次 Flow 触发的记录（对外 JSON 表示）。
type Execution struct {
	ID     string `json:"id"`
	FlowID string `json:"flow_id"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
	// ToolIDs 是本次执行**可能用到**的 Tool id（step 的 call ∪ 生效 Hook 的 action），
	// 由引擎在加载快照后上报（见 engine.RunFlowResultObserved）。
	// 两个用途：一是 §8.2「运行中不可注销」守卫的判据，二是排障时能直接看到
	// 「这次执行动了哪些 Tool」，不必反查 Flow + Hook。
	ToolIDs    []string          `json:"tool_ids,omitempty"`
	Result     *engine.RunResult `json:"result,omitempty"`
	StartedAt  time.Time         `json:"started_at"`
	FinishedAt *time.Time        `json:"finished_at,omitempty"`
}

// ExecStore 是执行记录的**持久化**存储（REQ-EXEC-PERSIST，#70）。
//
// 早期为内存态（重启即失，S6a 有意选择）；本次改为 SQLite（modernc.org/sqlite，纯 Go 无 cgo），
// 服务重启后可恢复全部执行历史与查询。并发安全由 SQLite 事务 + 单写点保证；
// RunningToolIDs 这种需遍历的运行期查询直接走 SQL（status='running'）。
//
// 路径：环境变量 GEAR_EXEC_DB，缺省 build/gear.db（已在 .gitignore 忽略）。
type ExecStore struct {
	db *sql.DB
	mu sync.Mutex // 保护「读-改-写」类操作（如 SetTools 需先取再存）
}

// execDBPath 解析数据库文件路径。
//
// 委托给 gearhome：**绝不用 cwd 相对路径**。曾经的默认值 `build/gear.db` 让执行历史跟着
// 调用方的工作目录走——`gear mcp` 被客户端以任意 cwd 派生时，历史会落到 `<cwd>/build/gear.db`
// （顺手在用户目录里凭空造出 `build/`），换个 cwd 再跑就「查不到刚才那次执行」。
// 现默认 `<GEAR_HOME|~>/.gear/exec.db`，与 registry 同目录；GEAR_EXEC_DB 仍可显式覆盖。
func execDBPath() string {
	return gearhome.ExecDBPath()
}

// NewExecStore 打开（必要时创建）SQLite 库并完成建表。
func NewExecStore() *ExecStore {
	path := execDBPath()
	if dir := filepath.Dir(path); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		// 极端情况下（如只读文件系统）退化为纯内存，保证服务能起；
		// 此时重启即失，但运行期语义不变。
		db, _ = sql.Open("sqlite", "file::memory:?cache=shared")
	}
	s := &ExecStore{db: db}
	s.migrate()
	return s
}

// Close 释放底层 SQLite 句柄（WAL 侧车文件随之解锁）。
// Server.Shutdown 在优雅停机时调用，便于测试用临时库路径后能干净回收 t.TempDir()。
func (s *ExecStore) Close() error {
	if s.db == nil {
		return nil
	}
	err := s.db.Close()
	s.db = nil
	return err
}

func (s *ExecStore) migrate() {
	_, _ = s.db.Exec(`CREATE TABLE IF NOT EXISTS executions (
		id          TEXT PRIMARY KEY,
		flow_id     TEXT NOT NULL,
		status      TEXT NOT NULL,
		error       TEXT,
		tool_ids    TEXT,
		result      TEXT,
		started_at  TEXT NOT NULL,
		finished_at TEXT
	)`)
	_, _ = s.db.Exec(`CREATE TABLE IF NOT EXISTS execution_logs (
		id       INTEGER PRIMARY KEY AUTOINCREMENT,
		exec_id  TEXT NOT NULL,
		step_id  TEXT NOT NULL,
		stream   TEXT NOT NULL,
		line     TEXT NOT NULL,
		ts       TEXT NOT NULL
	)`)
	_, _ = s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_exec_logs ON execution_logs(exec_id, id)`)
}

// LogEntry 是一条执行日志（stdout/stderr 行，关联到 exec + step）。
type LogEntry struct {
	ExecID string    `json:"exec_id"`
	StepID string    `json:"step_id"`
	Stream string    `json:"stream"` // stdout | stderr
	Line   string    `json:"line"`
	TS     time.Time `json:"ts"`
}

// Log 追加一条 step 运行日志（REQ-API-LOGS / #71）。
func (s *ExecStore) Log(execID, stepID, stream, line string) {
	_, _ = s.db.Exec(
		`INSERT INTO execution_logs (exec_id, step_id, stream, line, ts) VALUES (?, ?, ?, ?, ?)`,
		execID, stepID, stream, line, time.Now().Format(time.RFC3339Nano),
	)
}

// Logs 返回某次执行的全部日志（按产生顺序）。
func (s *ExecStore) Logs(execID string) []LogEntry {
	rows, err := s.db.Query(`SELECT exec_id, step_id, stream, line, ts FROM execution_logs WHERE exec_id=? ORDER BY id ASC`, execID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := make([]LogEntry, 0)
	for rows.Next() {
		var e LogEntry
		var ts string
		if err := rows.Scan(&e.ExecID, &e.StepID, &e.Stream, &e.Line, &ts); err != nil {
			continue
		}
		if t, perr := time.Parse(time.RFC3339Nano, ts); perr == nil {
			e.TS = t
		}
		out = append(out, e)
	}
	return out
}

// LogSink 把 ExecStore 适配为 engine.LogSink（供 RunFlowExec 注入）。
// 导出：server.New 在构造引擎时引用（engine.WithLogSink(s.execs.LogSink())）。
func (s *ExecStore) LogSink() engine.LogSink {
	return func(execID, stepID, stream, line string) {
		s.Log(execID, stepID, stream, line)
	}
}

// Create 登记一次新的执行（状态 running）。
func (s *ExecStore) Create(flowID string) Execution {
	e := Execution{ID: newID("exec_"), FlowID: flowID, Status: StatusRunning, StartedAt: time.Now()}
	_, _ = s.db.Exec(
		`INSERT INTO executions (id, flow_id, status, tool_ids, started_at) VALUES (?, ?, ?, '[]', ?)`,
		e.ID, e.FlowID, e.Status, e.StartedAt.Format(time.RFC3339Nano),
	)
	return e
}

// Finish 记录执行结果。
func (s *ExecStore) Finish(id string, res *engine.RunResult, err error) {
	now := time.Now()
	status := StatusSucceeded
	var errMsg string
	if err != nil {
		status = StatusFailed
		errMsg = err.Error()
	}
	var resultJSON *string
	if res != nil {
		if b, merr := json.Marshal(res); merr == nil {
			js := string(b)
			resultJSON = &js
		}
	}
	_, _ = s.db.Exec(
		`UPDATE executions SET status=?, error=?, result=?, finished_at=? WHERE id=?`,
		status, errMsg, resultJSON, now.Format(time.RFC3339Nano), id,
	)
}

// Reject 标记一次执行因入口繁忙被拒绝（REQ-CONCURRENCY-CROSS / #73）：
// 并发上限且队列已满，任务未真正运行即被拒，status 置为 rejected 供查询可见。
func (s *ExecStore) Reject(id, reason string) {
	now := time.Now()
	_, _ = s.db.Exec(
		`UPDATE executions SET status=?, error=?, finished_at=? WHERE id=?`,
		StatusRejected, reason, now.Format(time.RFC3339Nano), id,
	)
}

// SetTools 记录本次执行「可能用到」的 Tool id（引擎加载快照后回调，见 engine.ToolObserver）。
func (s *ExecStore) SetTools(id string, toolIDs []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, _ := json.Marshal(toolIDs)
	_, _ = s.db.Exec(`UPDATE executions SET tool_ids=? WHERE id=?`, string(b), id)
}

// RunningToolIDs 返回「运行中的执行」所用到的 Tool id → 触发它的执行 id。
//
// 供 §8.2「运行中不可注销」守卫使用：Registry 只看得见**静态**引用，
// 运行期引用（Hook 被删 / Flow 被改写后仍在跑的那部分）只能由此补上。
func (s *ExecStore) RunningToolIDs() map[string]string {
	rows, err := s.db.Query(`SELECT id, tool_ids FROM executions WHERE status=?`, StatusRunning)
	if err != nil {
		return map[string]string{}
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var eid, raw string
		if err := rows.Scan(&eid, &raw); err != nil {
			continue
		}
		var ids []string
		_ = json.Unmarshal([]byte(raw), &ids)
		for _, t := range ids {
			out[t] = eid
		}
	}
	return out
}

// Get 返回执行记录的副本（并发安全）。
func (s *ExecStore) Get(id string) (Execution, bool) {
	row := s.db.QueryRow(`SELECT id, flow_id, status, error, tool_ids, result, started_at, finished_at FROM executions WHERE id=?`, id)
	e, err := scanExecution(row)
	if err != nil {
		return Execution{}, false
	}
	return e, true
}

// List 返回全部执行记录（按创建顺序，即 started_at 升序；同秒用 id 稳定排序）。
func (s *ExecStore) List() []Execution {
	rows, err := s.db.Query(`SELECT id, flow_id, status, error, tool_ids, result, started_at, finished_at FROM executions ORDER BY started_at ASC, id ASC`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := make([]Execution, 0)
	for rows.Next() {
		if e, err := scanExecution(rows); err == nil {
			out = append(out, e)
		}
	}
	return out
}

// scanExecution 从一行（*sql.Row 或 *sql.Rows）扫描为 Execution。
func scanExecution(scanner interface {
	Scan(dest ...interface{}) error
}) (Execution, error) {
	var (
		id, flowID, status, errMsg, toolIDs, result, startedAt, finishedAt sql.NullString
	)
	if err := scanner.Scan(&id, &flowID, &status, &errMsg, &toolIDs, &result, &startedAt, &finishedAt); err != nil {
		return Execution{}, err
	}
	e := Execution{ID: id.String, FlowID: flowID.String, Status: status.String, Error: errMsg.String}
	if b := toolIDs.String; b != "" {
		_ = json.Unmarshal([]byte(b), &e.ToolIDs)
	}
	if b := result.String; b != "" {
		var r engine.RunResult
		if json.Unmarshal([]byte(b), &r) == nil {
			e.Result = &r
		}
	}
	if t, err := time.Parse(time.RFC3339Nano, startedAt.String); err == nil {
		e.StartedAt = t
	}
	if finishedAt.String != "" {
		if t, err := time.Parse(time.RFC3339Nano, finishedAt.String); err == nil {
			e.FinishedAt = &t
		}
	}
	return e, nil
}

// newID 生成短随机 id（crypto/rand，避免引入 uuid 依赖）。
func newID(prefix string) string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return prefix + hex.EncodeToString(b)
}
