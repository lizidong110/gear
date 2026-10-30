package cli

import (
	"testing"
)

// TestOpenBrowserSkippedInCI 锁住 REQ-UI-LAUNCH-AUTOOPEN：CI 环境下自动开浏览器必须跳过。
//
// 为什么单独测：自动开浏览器在 CI 上没有桌面，`cmd /c start` 会弹窗或报「找不到文件」，
// 表现为 serve 进程起不来或刷错误日志。而自动化场景根本不需要浏览器——
// 静默跳过是唯一正确行为（「开不了浏览器」绝不能让 serve 失败）。
func TestOpenBrowserSkippedInCI(t *testing.T) {
	t.Setenv("CI", "true")
	if err := openBrowser("http://127.0.0.1:1/"); err != nil {
		t.Fatalf("CI 环境下 openBrowser 应静默返回 nil，实际报错: %v", err)
	}
}

// TestOpenBrowserDoesNotFailServer 锁住 REQ-UI-LAUNCH-AUTOOPEN 的另一半：
// 浏览器开不起来（命令不存在 / 无桌面）也只返回 error，不得 panic。
// serve 侧对 openBrowser 的错误是「忽略」，所以这里只要求「返回一个可处理的 error」。
func TestOpenBrowserDoesNotFailServer(t *testing.T) {
	// 显式清空 CI，确保走真实分支（本机若无 xdg-open/cmd start 会返回 error）。
	t.Setenv("CI", "")
	// 不 Fatal：不同平台/沙箱下能否启动浏览器不确定，这里只断言「不 panic 且不挂死」。
	_ = openBrowser("http://127.0.0.1:1/")
}

// TestAutoOpenDisabledHonorsCIEnv 锁住 CI 判定口径：
// 环境变量**存在且非空**才算 CI；`CI=`（空串）算「没设」。
// 这条防的是「有人 export CI= 之后自动开浏览器静默失效」这类难查的坑。
func TestAutoOpenDisabledHonorsCIEnv(t *testing.T) {
	t.Setenv("CI", "true")
	if !autoOpenDisabled() {
		t.Fatal("CI=true 时应跳过自动开浏览器")
	}
	t.Setenv("CI", "")
	if autoOpenDisabled() {
		t.Fatal("CI=（空串）应视为「没设 CI」，不该跳过自动开浏览器")
	}
	t.Setenv("CI", " ")
	if !autoOpenDisabled() {
		t.Fatal("CI=空格（非空）应视为 CI")
	}
}
