package engine

import (
	"testing"

	"gear/internal/registry"
)

// mustOpenReg 在临时目录打开一个注册表实例供测试使用，调用方负责 Close。
//
// 【恢复说明】本函数原定义所在的测试文件在 2026-09-19 的批量误删事件中丢失，且四路取证
// （git 全对象库 / 全盘搜索 / 回收站 / 编辑器本地历史）均无可恢复副本；调用方
// subscriber_test.go 仍在引用它，缺失会导致 internal/engine 测试包无法编译。
//
// 语义与同包的 newTestRegistry（engine_test.go）完全一致——临时目录 + registry.Open，
// 差异失败即 t.Fatalf。故按同语义补回以保证测试可编译可运行。
// 若日后找回原定义，直接删除本文件即可。
func mustOpenReg(t *testing.T) *registry.Registry {
	t.Helper()
	return newTestRegistry(t)
}
