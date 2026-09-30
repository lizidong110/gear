package gearhome

// isolate.go：给各包的 TestMain 用的**测试隔离**助手。
//
// 为什么它在非 _test 文件里：Go 不允许跨包引用别人的 _test.go，而「把执行记录库指向临时目录」
// 这件事必须由**每个会构造 ExecStore 的包**各自在 TestMain 里做（server / mcp / cli 三个）。
// 与其在三处复制同一段话，不如放一个显式的、名字就说明用途的函数。
//
// 为什么必须隔离（2026-09-21 实测）：执行记录库的默认落点从「包内 `build/gear.db`」
// 改为「用户数据目录 `<GEAR_HOME|~>/.gear/exec.db`」之后，`go test ./...` 会直接往
// **用户真实的执行库**里写记录、并留下 `-wal`/`-shm` 侧车——测试污染了生产数据，
// 而那些记录又会被 `gear_execution` 列出来。默认值修正本身是对的，隔离是它的配套义务。

import (
	"os"
	"path/filepath"
)

// IsolateForTest 把执行记录库指向一个新建的临时目录，返回清理函数。
//
// 隔离 `GEAR_EXEC_DB` 与 `GEAR_CONFIG_DB`（两个落在用户数据目录里的文件库）——刻意不碰
// `GEAR_HOME`：registry 的默认值仍应保持「用户数据目录」，因为不少用例（CLI 帮助文本、
// 默认路径断言）要看的正是那个值。隔离的目标是「别把测试写进真实数据、也别让测试偷读真实
// 配置」（config.db 里可能存着用户真实的 AI api_key，未隔离会让 doctor 一类用例读到它、
// 断言「未就绪」直接红），不是「假装没有用户目录」。
// 隔离的目标是「别把测试写进真实数据」，不是「假装没有用户目录」。
//
// 同时**清掉 `GEAR_REGISTRY`**（用完还原）：那个变量是「上一次调用选定的库」的继承信道，
// 由 CLI 的 openRegistry 写入（见 RegistryPath 注释）。若不清，同一个测试进程里先跑的
// 用例会把它的临时库泄漏给后面的用例——后者看到 `RegistryPath()` 不再等于
// `<Home>/registry.db`，默认值断言与环境耦合，红起来还找不到原因。与「不碰 GEAR_HOME」
// 同一个理由：默认值必须保持可预测。
//
// 典型用法（各包 TestMain）：
//
//	func TestMain(m *testing.M) {
//		cleanup := gearhome.IsolateForTest()
//		code := m.Run()
//		cleanup()
//		os.Exit(code)
//	}
//
// 建不出临时目录时**不阻断测试**（退化为不隔离并返回空操作）：宁可有污染风险，
// 也不要让整套测试因为一个环境问题全红——那种红会掩盖真正的回归。
func IsolateForTest() func() {
	prevRegistry, hadRegistry := os.LookupEnv(EnvRegistry)
	_ = os.Unsetenv(EnvRegistry)
	restoreRegistry := func() {
		if hadRegistry {
			_ = os.Setenv(EnvRegistry, prevRegistry)
			return
		}
		_ = os.Unsetenv(EnvRegistry)
	}

	prevConfigDB, hadConfigDB := os.LookupEnv(EnvConfigDB)
	_ = os.Unsetenv(EnvConfigDB)
	restoreConfigDB := func() {
		if hadConfigDB {
			_ = os.Setenv(EnvConfigDB, prevConfigDB)
			return
		}
		_ = os.Unsetenv(EnvConfigDB)
	}

	dir, err := os.MkdirTemp("", "gear-test-exec-")
	if err != nil {
		restoreConfigDB()
		return restoreRegistry
	}
	if err := os.Setenv(EnvExecDB, filepath.Join(dir, "exec.db")); err != nil {
		_ = os.RemoveAll(dir)
		restoreConfigDB()
		return restoreRegistry
	}
	// 把配置库也指向同一个临时目录：否则测试会去读用户真实的 config.db（可能含真实 AI
	// api_key），导致「未配置即未就绪」这类断言在带配置的机器上随机红（2026-09-25 实测）。
	if err := os.Setenv(EnvConfigDB, filepath.Join(dir, "config.db")); err != nil {
		_ = os.RemoveAll(dir)
		restoreConfigDB()
		return restoreRegistry
	}
	return func() {
		_ = os.RemoveAll(dir)
		restoreRegistry()
		restoreConfigDB()
	}
}
