package engine

import (
	"fmt"

	"gear/internal/model"
)

// resolveToolVersion 按 step.UseVersion 解析生效的 Tool 视图（S5 灰度）。
//
// 语义：
//   - use_version 为空 → 原样返回（使用 Tool 级 Entry / IO schema）。
//   - 否则取对应版本的 Entry / InputSchema / OutputSchema 覆盖 Tool 级；
//     版本侧缺省（空）的字段回退 Tool 级，保证「钉版本」最小侵入。
//   - 版本不存在 → 错误（钉了不存在的版本属于配置错误，必须 fail-closed）。
//
// 返回的是 Tool 的**浅拷贝**，不改动注册表里的原对象（运行期只读快照，见 §8.2 守卫）。
func resolveToolVersion(tool *model.Tool, step model.Step) (*model.Tool, error) {
	if step.UseVersion == "" {
		return tool, nil
	}
	var v *model.ToolVersion
	for i := range tool.Versions {
		if tool.Versions[i].Version == step.UseVersion {
			v = &tool.Versions[i]
			break
		}
	}
	if v == nil {
		return nil, fmt.Errorf("step %s 钉了 use_version=%s，但 Tool %s 不存在该版本", step.ID, step.UseVersion, tool.ID)
	}
	eff := *tool // 浅拷贝
	if v.Entry != "" {
		eff.Entry = v.Entry
	}
	if v.InputSchema != nil {
		eff.InputSchema = v.InputSchema
	}
	if v.OutputSchema != nil {
		eff.OutputSchema = v.OutputSchema
	}
	return &eff, nil
}
