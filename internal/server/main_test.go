package server

// main_test.go：测试进程级的环境隔离。
//
// server 包会构造 ExecStore（server.New）。执行记录库的默认落点已从「包内 build/gear.db」
// 改为用户数据目录（见 gearhome 包注释），因此必须在这里重定向，否则 `go test ./...`
// 会往用户真实的执行库写记录（2026-09-21 实测被污染）。
//
// 注意：单个用例里的 `t.Setenv("GEAR_EXEC_DB", …)` 只覆盖那一个用例，**不能**代替本文件
// ——没写 t.Setenv 的用例仍会落到真实默认路径。

import (
	"os"
	"testing"

	"gear/internal/gearhome"
)

func TestMain(m *testing.M) {
	cleanup := gearhome.IsolateForTest()
	code := m.Run()
	cleanup()
	os.Exit(code)
}
