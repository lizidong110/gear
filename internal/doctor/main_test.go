package doctor

import (
	"os"
	"testing"

	"gear/internal/gearhome"
)

// TestMain 隔离配置库与执行记录库。
//
// doctor 的 AIReadiness 会真去读 config.db（判据要求「配过就绪、没配报修法」）。
// 不隔离的话，在配过 ai.api_key 的机器上「未就绪」那批断言会随机红——
// 而 config.db 里存的是用户真实密钥，测试读它本身也不合适（见 gearhome.IsolateForTest 注释）。
func TestMain(m *testing.M) {
	cleanup := gearhome.IsolateForTest()
	code := m.Run()
	cleanup()
	os.Exit(code)
}
