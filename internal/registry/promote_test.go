package registry

import (
	"testing"

	"gear/internal/model"
)

// S5：PromoteToolVersion 合法迁移落库、非法迁移/缺失版本/缺失 Tool 均报错。
func TestPromoteToolVersion(t *testing.T) {
	reg := mustOpen(t)
	defer reg.Close()

	tool := validTool("t")
	tool.Versions = []model.ToolVersion{{Version: "1.0.0", State: model.StateDraft}}
	if err := reg.UpsertTool(tool); err != nil {
		t.Fatalf("注册失败: %v", err)
	}

	// 合法：draft -> testing
	if err := reg.PromoteToolVersion("t", "1.0.0", model.StateTesting); err != nil {
		t.Fatalf("draft->testing 应成功: %v", err)
	}
	got, _ := reg.GetTool("t")
	if got.Versions[0].State != model.StateTesting {
		t.Fatalf("晋升后状态未持久化: %s", got.Versions[0].State)
	}

	// 非法：testing -> deprecated（跳过 stable）应拒绝且不改状态
	if err := reg.PromoteToolVersion("t", "1.0.0", model.StateDeprecated); err == nil {
		t.Fatal("testing->deprecated 应非法")
	}
	got, _ = reg.GetTool("t")
	if got.Versions[0].State != model.StateTesting {
		t.Fatalf("非法晋升不应改变状态: %s", got.Versions[0].State)
	}

	// 缺失版本
	if err := reg.PromoteToolVersion("t", "9.9.9", model.StateStable); err == nil {
		t.Fatal("缺失版本应报错")
	}
	// 缺失 Tool
	if err := reg.PromoteToolVersion("nope", "1.0.0", model.StateStable); err == nil {
		t.Fatal("缺失 Tool 应报错")
	}
}
