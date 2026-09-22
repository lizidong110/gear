package engine

// progress_writer_test.go 锁住「执行进度的输出目标可被替换」这条传输层契约。
//
// 背景是**线上的真实事故**：`plugin/mcp_wire_probe.py` 第一次跑就抓到 —— MCP stdio 下
// `gear_run` 一执行，引擎的 `✓ step …` 进度就混进了 stdout 帧流。而 README §12.3 明写
// 「stdio 下 stdout 只承载协议帧，日志/警告一律走 stderr」。本地全绿、接严格客户端全废。
//
// 修法是 `Engine.SetProgressWriter`（默认仍 os.Stdout，CLI 不受影响；`gear mcp` 改指 stderr）。
// 本用例是那条修复的**机械守卫**：实现若回归成 `fmt.Printf`（→ os.Stdout），这里必须变红。

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"gear/internal/model"
)

// TestProgressWriterRedirectsExecutionLog 断言进度只写指定目标、且**不**落到 stdout。
func TestProgressWriterRedirectsExecutionLog(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()

	// 用纯函数原子 template.render：不依赖外部进程与 PATH，任何平台都能跑。
	if err := reg.UpsertFlow(&model.Flow{
		ID:   "pwf",
		Vars: map[string]interface{}{"who": "probe"},
		Steps: []model.Step{{
			ID: "s1", Call: "template.render",
			Input: map[string]interface{}{"text": "hi ${vars.who}"},
		}},
	}); err != nil {
		t.Fatalf("注册 Flow 失败: %v", err)
	}

	// 把 os.Stdout 换成临时文件：任何「偷偷写 stdout」的实现都会在这里现形。
	saved := os.Stdout
	f, err := os.CreateTemp(t.TempDir(), "stdout-*.txt")
	if err != nil {
		t.Fatalf("建临时文件失败: %v", err)
	}
	os.Stdout = f
	restore := func() {
		os.Stdout = saved
		_ = f.Close()
	}

	var buf bytes.Buffer
	e := New(reg)
	e.SetProgressWriter(&buf)
	runErr := e.RunFlow("pwf")
	restore()
	if runErr != nil {
		t.Fatalf("RunFlow 失败: %v", runErr)
	}

	// ① 指定的目标必须收到进度（不能为了让 stdout 干净就干脆不输出——那是静默降级）。
	if got := buf.String(); !strings.Contains(got, "✓ step s1") {
		t.Fatalf("指定的进度目标没收到输出: %q", got)
	}
	if got := buf.String(); !strings.Contains(got, "执行完成") {
		t.Fatalf("指定的进度目标缺收尾统计: %q", got)
	}

	// ② stdout 必须一个字节都没被污染（MCP stdio 下这就是协议通道）。
	raw, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatalf("读回 stdout 捕获文件失败: %v", err)
	}
	if strings.Contains(string(raw), "step s1") {
		t.Fatalf("进度输出绕进了 stdout（MCP stdio 下会污染协议通道）: %q", string(raw))
	}
}

// TestProgressWriterDefaultsToStdout 锁住默认行为不被这次改动动到：
// 不显式设置时，进度仍写 os.Stdout（CLI `gear exec` 的用户体验不能变）。
func TestProgressWriterDefaultsToStdout(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	if err := reg.UpsertFlow(&model.Flow{
		ID: "pwf2",
		Steps: []model.Step{{
			ID: "s1", Call: "template.render",
			Input: map[string]interface{}{"text": "x"},
		}},
	}); err != nil {
		t.Fatalf("注册 Flow 失败: %v", err)
	}

	saved := os.Stdout
	f, err := os.CreateTemp(t.TempDir(), "stdout-*.txt")
	if err != nil {
		t.Fatalf("建临时文件失败: %v", err)
	}
	os.Stdout = f
	runErr := New(reg).RunFlow("pwf2")
	os.Stdout = saved
	_ = f.Close()
	if runErr != nil {
		t.Fatalf("RunFlow 失败: %v", runErr)
	}
	raw, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatalf("读回捕获文件失败: %v", err)
	}
	if !strings.Contains(string(raw), "step s1") {
		t.Fatalf("默认目标应是 os.Stdout（CLI 体验不变）: %q", string(raw))
	}

	// nil 显式还原默认：SetProgressWriter(nil) 必须等价于「没设置过」。
	var buf bytes.Buffer
	os.Stdout = f
	f2, _ := os.CreateTemp(t.TempDir(), "stdout2-*.txt")
	os.Stdout = f2
	e := New(reg)
	e.SetProgressWriter(&buf)
	e.SetProgressWriter(nil)
	runErr = e.RunFlow("pwf2")
	os.Stdout = saved
	_ = f.Close()
	_ = f2.Close()
	if runErr != nil {
		t.Fatalf("RunFlow(nil writer) 失败: %v", runErr)
	}
	raw2, err := os.ReadFile(f2.Name())
	if err != nil {
		t.Fatalf("读回捕获文件失败: %v", err)
	}
	if !strings.Contains(string(raw2), "step s1") {
		t.Fatalf("SetProgressWriter(nil) 应回到 os.Stdout: %q", string(raw2))
	}
	if buf.Len() != 0 {
		t.Fatalf("置 nil 后不该再写旧目标: %q", buf.String())
	}
}
