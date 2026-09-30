package cli

import (
	"fmt"
	"path"
	"strings"

	"gear/internal/registry"
)

// 本文件是 `gear tool unregister` 与 `gear flow delete` 两个批量删除入口的公共展开逻辑。
// 原先两套各写一份「展开 pattern → 逐个删」，任何一侧改了「空集算不算错」另一侧都会悄悄
// 分叉；清理一个应用时会同时用到这两个命令，语义必须一致。

// hasWildcard 判断 pattern 是否含通配符（* ? [ ... ]）。不含则按精确 id 处理，
// 保留既有「不存在」的错误文案，也让常见路径零额外开销。
func hasWildcard(pattern string) bool {
	return strings.ContainsAny(pattern, "*?[")
}

// globMatch 把一个 pattern 展开成**确实存在于 existing 里**的 id 列表。
//
// 两条硬约束都在这里：
//  1. 只按 existing 匹配，绝不按 pattern 猜 id —— 否则打错字会「删掉一个不存在的
//     Flow / Tool」却报成功。
//  2. 空集是错误而不是 no-op：通配符打了空（多半是拼写问题）必须让命令红，
//     不许静默打印成功。
//
// pattern 本身非法（如 "[abc"）时把 path.Match 的 ErrBadPattern 转述出来，
// 不让它漏到运行期变成「莫名其妙匹配不到」。
func globMatch(pattern string, existing []string) ([]string, error) {
	var hit []string
	for _, id := range existing {
		ok, err := path.Match(pattern, id)
		if err != nil {
			return nil, fmt.Errorf("非法通配符 %q: %w", pattern, err)
		}
		if ok {
			hit = append(hit, id)
		}
	}
	if len(hit) == 0 {
		return nil, fmt.Errorf("通配符 %q 没有匹配到任何已注册项（现有 %d 个，用 `gear tool list` / `gear flow list` 查看）",
			pattern, len(existing))
	}
	return hit, nil
}

// expandDeleteTargets 展开删除命令的多个参数（精确 id 或含通配符的 pattern），保序去重。
// pattern 不含通配符时原样透传，由下游 Delete* 抛「不存在」—— 精确 id 的报错文案
// 是既有契约，不该在这里被换掉。match 只负责按 pattern 筛出命中集。
func expandDeleteTargets(patterns []string, match func(string) ([]string, error)) ([]string, error) {
	var ids []string
	seen := make(map[string]bool, len(patterns))
	for _, p := range patterns {
		list := []string{p}
		if hasWildcard(p) {
			var err error
			if list, err = match(p); err != nil {
				return nil, err
			}
		}
		for _, id := range list {
			if seen[id] {
				continue // 多个 pattern 可能重叠（slice* 与 slice_api 都命中 slice_api）
			}
			seen[id] = true
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// deleteAll 逐个调用 deleteOne，遇错即停（fail-closed）。
//
// 批量删场景下「删掉能删的、剩下的算了」会把「还有一个 Hook 在引用它」这种事实藏起来，
// 比直接失败危险得多，所以命中集里任何一个被 §8.2 守卫拒绝就整批失败。
// 已删部分不回滚 —— 快照留痕可用 gear_apply 的 restore_from 找回，这里打印出来就够了。
func deleteAll(noun string, ids []string, deleteOne func(string) error) error {
	var removed []string
	for _, id := range ids {
		if err := deleteOne(id); err != nil {
			if len(removed) > 0 {
				return fmt.Errorf("已注销 %d 个（%s）后，%s 注销失败: %w（已删的可由 gear_apply 的 restore_from 找回）",
					len(removed), strings.Join(removed, ", "), id, err)
			}
			return err
		}
		removed = append(removed, id)
	}
	switch len(removed) {
	case 0:
		// 理论不可达：每个 pattern 至少贡献一个目标。留着是为了让「删了 0 个却打印成功」
		// 这种静默降级没有落脚点（globMatch 的空集检查是它的第一道，这里是第二道）。
		return fmt.Errorf("没有可注销的 %s", noun)
	case 1:
		fmt.Printf("✓ 已注销 %s: %s\n", noun, removed[0])
	default:
		fmt.Printf("✓ 已注销 %d 个 %s:\n", len(removed), noun)
		for _, id := range removed {
			fmt.Printf("  - %s\n", id)
		}
	}
	return nil
}

// toolIDs 取 registry 现存 Tool id —— 通配符展开的唯一输入来源。
func toolIDs(reg *registry.Registry) ([]string, error) {
	all, err := reg.ListTools()
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(all))
	for _, t := range all {
		ids = append(ids, t.ID)
	}
	return ids, nil
}

// flowIDs 取 registry 现存 Flow id，语义同 toolIDs。
func flowIDs(reg *registry.Registry) ([]string, error) {
	all, err := reg.ListFlows()
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(all))
	for _, f := range all {
		ids = append(ids, f.ID)
	}
	return ids, nil
}
