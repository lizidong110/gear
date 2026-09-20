package engine

// ai_validate.go 实现 ext.ai 的静态校验（Validate，kind = flow|tool|hook）。
//
// 红线（PRD §9.2 单一真相源）：**Flow 的校验必须走引擎自己的 ValidateFlowStatic**——生成期
// 与注册期（CLI / REST / MCP）判同一份规则，绝不长出第二套判定。AIAgent 的 validateFlow 字段
// 由 Engine.bindAIAgent 注入 e.ValidateFlowStatic；引擎未注入时（直接 newAIAgent 的单测、
// 或未来别的宿主）退化为「结构校验 + call 目标存在性」，并在错误里说明这是退化口径，
// 不假装做了完整校验（不静默降级）。

import (
	"encoding/json"
	"fmt"
	"strings"

	"gear/internal/model"
)

// Validate 按 kind 校验一份生成草稿；nil 表示通过。
//   - flow：反序列化为 model.Flow 后走引擎静态校验（注入点见上）；
//   - tool：反序列化为 model.Tool 后走 model.Tool.Validate（version/state/schema/runtime）；
//   - hook：反序列化为 model.Hook 后走 model.Hook.Validate（id/target/action/stage/criticality）。
//
// tools 只在 flow 的退化路径用到（重建「引用的工具是否存在」），正常路径由引擎 registry 判定。
func (a *AIAgent) Validate(kind string, def map[string]interface{}, tools []ToolSummary) error {
	switch kind {
	case "flow":
		return a.validateFlowDef(def, tools)
	case "tool":
		var t model.Tool
		if err := decodeDef(def, &t); err != nil {
			return err
		}
		return t.Validate()
	case "hook":
		var h model.Hook
		if err := decodeDef(def, &h); err != nil {
			return err
		}
		return h.Validate()
	default:
		return fmt.Errorf("validate 的 kind 必须是 flow|tool|hook，收到 %q", kind)
	}
}

// validateFlowDef 校验 Flow 草稿：优先走引擎注入的 ValidateFlowStatic（单一真相源）。
func (a *AIAgent) validateFlowDef(def map[string]interface{}, tools []ToolSummary) error {
	var f model.Flow
	if err := decodeDef(def, &f); err != nil {
		return err
	}
	if a.validateFlow != nil {
		return a.validateFlow(&f)
	}
	// 退化路径：引擎未注入静态校验（例如直接构造 agent 的单测）。仍做两道硬检查——
	// 结构（model.Flow.Validate）与 call 目标存在性——但口径窄于 ValidateFlowStatic
	// （不含依赖图外的模板引用校验、when 编译、版本存在性），故在错误里显式标注。
	if err := f.Validate(); err != nil {
		return err
	}
	if err := a.checkCallTargets(f, tools); err != nil {
		return fmt.Errorf("%w（退化校验：引擎未注入 ValidateFlowStatic，本次只校验了结构与 call 目标存在性）", err)
	}
	return nil
}

// checkCallTargets 检查每个 step 的 call 是否指向「目录内工具」或「已注册的能力.方法」。
// 目标集合取 tools（调用方给的目录）与真实 registry 的并集——两者都对，
// 因为生产中 loadTools 就是从 registry 读的，测试里则可能只给一份 mock 目录。
func (a *AIAgent) checkCallTargets(f model.Flow, tools []ToolSummary) error {
	known := make(map[string]bool, len(tools))
	for _, t := range tools {
		known[t.ID] = true
	}
	if a.reg != nil {
		if list, err := a.reg.ListTools(); err == nil {
			for _, t := range list {
				if t != nil {
					known[t.ID] = true
				}
			}
		}
	}
	for _, s := range f.Steps {
		if known[s.Call] {
			continue
		}
		// 切分规则复用 splitToolMethod（与 engine.resolveStepCall 同一处真相）：
		// ext 命名空间是两级，`ext.ai.complete` 必须切成能力 `ext.ai` + 方法 `complete`。
		prim, method := splitToolMethod(s.Call, "")
		if prim == "" || method == "" {
			return fmt.Errorf("step %s 的 call %q 既不是已注册 Tool，也不是「能力.方法」形式的原子能力直调", s.ID, s.Call)
		}
		if methods, ok := builtinCapabilitySchemas[prim]; ok {
			if _, ok := methods[method]; !ok {
				return fmt.Errorf("step %s: 内置原语 %s 没有方法 %q", s.ID, prim, method)
			}
			continue
		}
		if a.extCaps != nil {
			if methods, ok := a.extCaps[prim]; ok {
				// 有 schema 就顺带校验**方法名**（与 engine.checkCapabilityMethod 同口径）：
				// 退化路径不能比主路径更松，否则「生成期放过、注册期拒绝」会让 agent 反复重试同一份草稿。
				if _, ok := methods[method]; !ok {
					return fmt.Errorf("step %s: 扩展能力 %s 没有方法 %q（该 ext 声明的方法：%s）", s.ID, prim, method, strings.Join(sortedKeys(methods), ", "))
				}
				continue
			}
		}
		return fmt.Errorf("step %s 的 call %q 引用的工具/能力不存在", s.ID, s.Call)
	}
	return nil
}

// decodeDef 把 LLM 产出的 map 反序列化为强类型定义：JSON 往返一次，复用 model 的字段
// 与校验方法（同一份标签/枚举，不手写第二套字段映射）。
func decodeDef(def map[string]interface{}, out interface{}) error {
	b, err := json.Marshal(def)
	if err != nil {
		return fmt.Errorf("序列化定义失败: %w", err)
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("定义 JSON 非法: %w", err)
	}
	return nil
}
