package model

// app_mcp_test.go：App 清单 mcp.tools 策展区的注册期最后一道闸（REQ-APP-MCP-CURATE）。
// App.Validate 是「即使上层绕过 bundle 直接 UpsertApp」也会跑的那层，
// 所以 ref/alias 形状、重名、描述长度这些不依赖上下文的判定必须在这里挡住。

import (
	"strings"
	"testing"
)

func validMCPApp() *App {
	return &App{
		ID: "study", EntryFlow: "study.up",
		MCP: AppMCP{Tools: []AppMCPTool{
			{Ref: "study__plan_propose", Alias: "study_plan_propose", Args: map[string]any{"session": "parent"}},
			{Ref: "study__plan_confirm"},
		}},
	}
}

func TestAppMCPValidateAcceptsValidRules(t *testing.T) {
	if err := validMCPApp().Validate(); err != nil {
		t.Fatalf("合法策展区不该被拒: %v", err)
	}
	// 没写 mcp 区的存量 App 必须逐字不受影响。
	if err := (&App{ID: "old", EntryFlow: "old.up"}).Validate(); err != nil {
		t.Fatalf("无 mcp 区的存量 App 不应受影响: %v", err)
	}
}

func TestAppMCPValidateRejectsBadRules(t *testing.T) {
	cases := []struct {
		name  string
		tools []AppMCPTool
		want  string
	}{
		{"空 ref", []AppMCPTool{{Alias: "x"}}, "ref 不能为空"},
		{"alias 含空格", []AppMCPTool{{Ref: "study__a", Alias: "bad name"}}, "非法字符"},
		{"alias 含中文", []AppMCPTool{{Ref: "study__a", Alias: "计划"}}, "非法字符"},
		{"alias 超长", []AppMCPTool{{Ref: "study__a", Alias: strings.Repeat("a", 129)}}, "128"},
		{"重名", []AppMCPTool{
			{Ref: "study__a", Alias: "same"},
			{Ref: "study__b", Alias: "same"},
		}, "重名"},
		{"ref 与 alias 撞名", []AppMCPTool{
			{Ref: "study__a", Alias: "study__b"},
			{Ref: "study__b"},
		}, "重名"},
		{"args 空键", []AppMCPTool{{Ref: "study__a", Args: map[string]any{"": 1}}}, "空键"},
		{"描述超长", []AppMCPTool{{Ref: "study__a", Description: strings.Repeat("禁", 2001)}}, "2000"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := validMCPApp()
			a.MCP.Tools = tc.tools
			err := a.Validate()
			if err == nil {
				t.Fatal("非法策展规则应被拒")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误信息应含 %q，实际: %v", tc.want, err)
			}
		})
	}
}

func TestAppMCPExposedName(t *testing.T) {
	if got := (AppMCPTool{Ref: "study__a"}).ExposedName(); got != "study__a" {
		t.Fatalf("无 alias 时用 ref，实际 %q", got)
	}
	if got := (AppMCPTool{Ref: "study__a", Alias: "preview"}).ExposedName(); got != "preview" {
		t.Fatalf("alias 优先，实际 %q", got)
	}
}
