package engine

import (
	"context"
	"fmt"

	"gear/internal/model"
)

// PromoteTool 提升某 Tool 版本到目标状态（S5 灰度）。
//
// 与普通的「改状态」不同，晋升是一个**受 Hook 守卫的边界动作**：落库前先触发
// before_promote 阶段 Hook（PRD §5.3）。任一 critical Hook 失败即中止晋升
// （fail-closed，不改任何状态）；best-effort Hook 失败仅告警、晋升继续。
//
// 为什么放在引擎而非 registry：Hook 的执行（runtime 调用）是引擎职责，registry 只管持久化。
// 这与 §8.2「运行期引用由运行侧上报」同一思路——晋升的副作用（门禁/打标/通知）交给引擎编排。
func (e *Engine) PromoteTool(toolID, version string, target model.ToolVersionState) error {
	hooks, err := e.loadHooks()
	if err != nil {
		return err
	}
	payload := map[string]interface{}{
		"tool_id": toolID,
		"version": version,
		"target":  string(target),
	}
	// before_promote Hook 不改变晋升本身，只做门禁/副作用；只看 err（critical 失败即中止）。
	if _, _, err := e.applyHooks(context.Background(), hooks, model.StageBeforePromote, toolID, payload); err != nil {
		return fmt.Errorf("before_promote Hook 中止晋升 %s@%s -> %s: %w", toolID, version, target, err)
	}
	return e.reg.PromoteToolVersion(toolID, version, target)
}
