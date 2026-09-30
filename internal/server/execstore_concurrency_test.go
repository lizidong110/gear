package server

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

// ExecStore 并发写回归锁（2026-09-26）。
//
// 与 registry 的 TestMultiProcessConcurrentWrite 是**同一根因**：DSN 缺 `_txlock=immediate`
// 时，database/sql 发的裸 BEGIN 是 deferred，SQLite 对「读锁升写锁」冲突**不调用 busy handler**，
// busy_timeout=5000 形同虚设，直接甩 SQLITE_BUSY。
//
// 这里单独补一条的理由是：ExecStore 的写入方法（Create/Finish/Reject/Log/SetTools）
// **把错误全吞掉**（`_, _ = s.db.Exec(...)`）。也就是说 registry 那边是把错误抛出来被测试看见，
// 而 execstore 这边 BUSY 会**静默丢行**——写进去了 20 次、库里只有 18 条，谁都不会报错。
// 所以断言只能是「行数是否齐全」，不能是「有没有报错」。
func TestExecStoreConcurrentWriteKeepsEveryRow(t *testing.T) {
	t.Setenv("GEAR_EXEC_DB", filepath.Join(t.TempDir(), "exec.db"))
	s1 := NewExecStore()
	defer s1.Close()
	s2 := NewExecStore() // 独立连接 = 第二个 gear 进程
	defer s2.Close()

	const n = 20
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			s1.Create(fmt.Sprintf("flow-%d", i), "actor-1")
		}(i)
		go func(i int) {
			defer wg.Done()
			s2.Create(fmt.Sprintf("flow-%d", i), "actor-2")
		}(i)
	}
	wg.Wait()

	got := len(s1.List())
	if got != 2*n {
		t.Fatalf("并发写入后应有 %d 条执行记录，实得 %d——"+
			"差额来自静默丢弃的 SQLITE_BUSY：ExecStore 吞错误，只能靠行数发现", 2*n, got)
	}
}
