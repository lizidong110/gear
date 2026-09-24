// Package execstore 是「执行记录存储」的稳定导入路径。
//
// 历史沿革（2026-09 重构）：
//   - 原实现位于本包（internal/execstore/execstore.go）。
//   - 重构中该实现随 REST 面一并下沉进 internal/server（package server，
//     见 internal/server/execstore.go），本目录因此一度缺包，导致
//     internal/mcp 的 `gear/internal/execstore` 导入断裂。
//
// 本文件是**转发垫片**：把 internal/server 里的执行库类型原样别名出来，
// 让调用方（internal/mcp）继续以「与 REST 面同一份 SQLite」的方式复用，
// 而不必让 MCP 面反向依赖 HTTP server 包的具体类型名。
//
// 语义要点：server.NewExecStore() 走同一份 execDBPath() 解析，
// 因此 REST 与 MCP 两个执行面天然共享同一个 SQLite 文件——
// 「运行中不可注销」的 RunningToolIDs 判据跨执行面一致（见 mcp.go 包注释）。
//
// 收敛方式（供后续）：若最终决定执行库回迁为独立包，把
// internal/server/execstore.go 的内容整体移入本目录、删除本垫片、
// 再把 internal/server 里的引用改为 execstore.* 即可；本垫片可直接删除。
package execstore

import "gear/internal/server"

// 类型别名（非新类型）：调用方拿到的就是 server 包里的同一批类型，
// 方法集与语义完全一致，不存在两套实现。
type (
	// ExecStore 是执行记录存储（SQLite 持久化）。
	ExecStore = server.ExecStore
	// Execution 是一次 Flow 执行的记录。
	Execution = server.Execution
	// LogEntry 是某次执行中某个 step 的一条日志行。
	LogEntry = server.LogEntry
)

// 执行状态常量（转发自 server，避免调用方各写一份字面量）。
const (
	StatusRunning   = server.StatusRunning
	StatusSucceeded = server.StatusSucceeded
	StatusFailed    = server.StatusFailed
	StatusRejected  = server.StatusRejected
)

// NewExecStore 打开（必要时建库并迁移）执行记录存储，与 REST 面同源。
func NewExecStore() *ExecStore { return server.NewExecStore() }
