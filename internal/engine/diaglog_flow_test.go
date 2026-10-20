package engine

// diaglog_flow_test.go 钉住引擎埋点（REQ-CLI-DIAG-LOG）在**真跑 Flow** 时落档案。
//
// 为什么值得单独立一个文件：CLI 面真机跑 flow run 会撞上本机 sandbox 对子进程/网络的
// 限制（实测挂起，与诊断日志无关），「档案行长什么样」不能靠手跑验证——那就用进程内
// RunFlow 走同一条 runWithVars 路径，把内容钉在测试里。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gear/internal/gearlog"
	"gear/internal/model"
)

// TestRunFlowWritesDiagLog —— 跑一个纯模板 Flow，档案必须含执行完成行（flow/exec/steps/耗时）。
func TestRunFlowWritesDiagLog(t *testing.T) {
	gearlog.ResetForTest()
	t.Cleanup(gearlog.ResetForTest)

	reg := newTestRegistry(t)
	t.Cleanup(func() { _ = reg.Close() })
	if err := reg.UpsertFlow(&model.Flow{
		ID:   "diagdemo",
		Vars: map[string]interface{}{"who": "probe"},
		Steps: []model.Step{{
			ID: "s1", Call: "template.render",
			Input: map[string]interface{}{"text": "hi ${vars.who}"},
		}},
	}); err != nil {
		t.Fatalf("注册 Flow 失败: %v", err)
	}

	path := filepath.Join(t.TempDir(), "engine.log")
	closer, err := gearlog.Setup(path, "info")
	if err != nil {
		t.Fatalf("Setup 失败: %v", err)
	}
	t.Cleanup(func() { _ = closer.Close() })

	if err := New(reg).RunFlow("diagdemo"); err != nil {
		t.Fatalf("RunFlow 失败: %v", err)
	}
	_ = closer.Close() // 先落盘再读（Windows 上句柄不关读得到，但关掉更稳）

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("档案应已创建: %v", err)
	}
	got := string(raw)
	for _, want := range []string{"level=INFO", "Flow 执行完成", "flow=diagdemo", "steps=1", "elapsed_ms="} {
		if !strings.Contains(got, want) {
			t.Fatalf("档案缺 %q，实际:\n%s", want, got)
		}
	}
}

// TestRunFlowDiagLogRecordsFailure —— Flow 执行失败也要落 ERROR 档案行
// （「失败了但档案里只有开始」是事后最难查的一类）。
func TestRunFlowDiagLogRecordsFailure(t *testing.T) {
	gearlog.ResetForTest()
	t.Cleanup(gearlog.ResetForTest)

	reg := newTestRegistry(t)
	t.Cleanup(func() { _ = reg.Close() })
	if err := reg.UpsertFlow(&model.Flow{
		ID: "diagfail",
		Steps: []model.Step{{
			ID: "s1", Call: "template.render",
			Input: map[string]interface{}{"text": "${vars.nonexistent.deep}"}, // 解析失败
		}},
	}); err != nil {
		t.Fatalf("注册 Flow 失败: %v", err)
	}

	path := filepath.Join(t.TempDir(), "engine-fail.log")
	closer, err := gearlog.Setup(path, "info")
	if err != nil {
		t.Fatalf("Setup 失败: %v", err)
	}
	t.Cleanup(func() { _ = closer.Close() })

	_ = New(reg).RunFlow("diagfail") // 预期失败，错误在档案里
	_ = closer.Close()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("档案应已创建: %v", err)
	}
	got := string(raw)
	if !strings.Contains(got, "level=ERROR") || !strings.Contains(got, "Flow 执行失败") {
		t.Fatalf("失败必须落 ERROR 档案行，实际:\n%s", got)
	}
	if !strings.Contains(got, "flow=diagfail") {
		t.Fatalf("失败行必须带 flow id，实际:\n%s", got)
	}
}
