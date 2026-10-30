package cli

import (
	"os"
	"os/exec"
	"runtime"
)

// openBrowser 在桌面环境打开默认浏览器访问 url。
//
// 设计取舍：
//   - CI / 无桌面环境（CI 环境变量）直接跳过，不报错（自动化场景没浏览器可开）。
//   - 用 Start 而非 Run：不阻塞当前进程；浏览器进程生命周期与它无关。
//   - 失败静默忽略：开浏览器是「锦上添花」，绝不能因为开不了而让 serve 失败。
func openBrowser(url string) error {
	if autoOpenDisabled() {
		return nil
	}
	var cmd string
	var args []string
	switch runtime.GOOS {
	case "windows":
		cmd = "cmd"
		args = []string{"/c", "start", "", url}
	case "darwin":
		cmd = "open"
		args = []string{url}
	default:
		cmd = "xdg-open"
		args = []string{url}
	}
	return exec.Command(cmd, args...).Start()
}

// autoOpenDisabled 判断是否跳过自动开浏览器。
//
// 口径：CI 环境变量**存在且非空**才算 CI。存在但为空串（`CI=`）算「没设」——
// 有人 export CI= 之后自动开浏览器静默失效，属于难查的坑，单独留测试钉住。
func autoOpenDisabled() bool {
	return os.Getenv("CI") != ""
}
