package server

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"

	"gear/internal/engine"
)

// 执行状态（对外可见）。
const (
	StatusRunning   = "running"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
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

// ExecStore 是**内存态**执行记录。
//
// ⚠️ 持久性显式声明（方法论 §11·状态持久性）：服务进程重启 ⇒ 记录全部丢失。
// 这是 S6a 的**有意选择**（MVP 先跑通触发/查询闭环），持久化执行历史属后续切片；
// 不要把它当作「已持久化」。
type ExecStore struct {
	mu  sync.Mutex
	m   map[string]*Execution
	seq []string
}

// NewExecStore 构造空 store。
func NewExecStore() *ExecStore {
	return &ExecStore{m: map[string]*Execution{}}
}

// Create 登记一次新的执行（状态 running）。
func (s *ExecStore) Create(flowID string) Execution {
	e := &Execution{ID: newID("exec_"), FlowID: flowID, Status: StatusRunning, StartedAt: time.Now()}
	s.mu.Lock()
	s.m[e.ID] = e
	s.seq = append(s.seq, e.ID)
	s.mu.Unlock()
	return *e
}

// Finish 记录执行结果。
func (s *ExecStore) Finish(id string, res *engine.RunResult, err error) {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[id]
	if !ok {
		return
	}
	e.FinishedAt = &now
	if err != nil {
		e.Status = StatusFailed
		e.Error = err.Error()
		return
	}
	e.Status = StatusSucceeded
	e.Result = res
}

// SetTools 记录本次执行「可能用到」的 Tool id（引擎加载快照后回调，见 engine.ToolObserver）。
func (s *ExecStore) SetTools(id string, toolIDs []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.m[id]; ok {
		e.ToolIDs = append([]string(nil), toolIDs...) // 复制：调用方切片可能被复用
	}
}

// RunningToolIDs 返回「运行中的执行」所用到的 Tool id → 触发它的执行 id。
//
// 供 §8.2「运行中不可注销」守卫使用：Registry 只看得见**静态**引用，
// 运行期引用（Hook 被删 / Flow 被改写后仍在跑的那部分）只能由此补上。
func (s *ExecStore) RunningToolIDs() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]string{}
	for _, id := range s.seq {
		e, ok := s.m[id]
		if !ok || e.Status != StatusRunning {
			continue
		}
		for _, t := range e.ToolIDs {
			out[t] = e.ID
		}
	}
	return out
}

// Get 返回执行记录的**副本**（并发安全：Finish 会原地改字段，
// 直接返回指针会让调用方在锁外读到写一半的结构；ToolIDs 是切片，
// 共享底层数组同样会在并发 SetTools 时竞争，故一并复制）。
func (s *ExecStore) Get(id string) (Execution, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[id]
	if !ok {
		return Execution{}, false
	}
	out := *e
	out.ToolIDs = append([]string(nil), e.ToolIDs...)
	return out, true
}

// List 返回全部执行记录（副本，按创建顺序）。
func (s *ExecStore) List() []Execution {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Execution, 0, len(s.seq))
	for _, id := range s.seq {
		e, ok := s.m[id]
		if !ok {
			continue
		}
		cp := *e
		cp.ToolIDs = append([]string(nil), e.ToolIDs...)
		out = append(out, cp)
	}
	return out
}

// newID 生成短随机 id（crypto/rand，避免引入 uuid 依赖）。
func newID(prefix string) string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return prefix + hex.EncodeToString(b)
}
