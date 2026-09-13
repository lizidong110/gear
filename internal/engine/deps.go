package engine

import (
	"fmt"
	"regexp"
	"sort"

	"gear/internal/model"
)

// whenStepRefRe 匹配 **表达式** 里的裸 `steps.<id>` 引用（when 没有 ${} 包裹）。
// 前导断言排除标识符/点，避免把 `mysteps.x` 之类误判为引用。
//
// 注意：**模板**里的引用不在这里匹配——那走 template.go 的 scanTemplateRefs，
// 与运行期展开、注册期校验共用同一解析器（避免「什么算引用」出现多份判定）。
var whenStepRefRe = regexp.MustCompile(`(?:^|[^A-Za-z0-9_.])steps\.([A-Za-z0-9_-]+)`)

// stripQuoted 把字符串字面量（"…" / '…' / `…`）的内容替换为空格，保留引号本身。
// 目的：避免 `when: 'name == "steps.nope"'` 这类字面量被误判成依赖引用。
func stripQuoted(s string) string {
	out := []rune(s)
	var quote rune
	for i, r := range out {
		switch {
		case quote == 0 && (r == '"' || r == '\'' || r == '`'):
			quote = r
		case quote != 0 && r == quote:
			quote = 0
		case quote != 0:
			out[i] = ' '
		}
	}
	return string(out)
}

// collectWhenRefs 收集 when 表达式里引用的 step id（先剔除字符串字面量，避免误判）。
func collectWhenRefs(when string, seen map[string]bool) {
	if when == "" {
		return
	}
	for _, m := range whenStepRefRe.FindAllStringSubmatch(stripQuoted(when), -1) {
		seen[m[1]] = true
	}
}

// collectRefs 递归收集**模板值**里 `${steps.<id>.…}` 引用的 step id（去重写入 seen）。
//
// 用 template.go 的解析器而非自带正则：模板引用有三个消费者（运行期展开、依赖推断、
// 注册期静态校验），一旦各写一套，就会出现「推断认得的引用、展开不认得」这类只在特定路径
// 暴露的分叉（F8 的延伸）。解析器顺带把「模板未闭合」变成注册期错误，与运行期语义一致。
func collectRefs(v interface{}, seen map[string]bool) error {
	return walkStrings(v, func(s string) error {
		refs, err := scanTemplateRefs(s)
		if err != nil {
			return err
		}
		for _, r := range refs {
			if r.Prefix == prefixSteps && len(r.Path) > 0 {
				seen[r.Path[0]] = true
			}
		}
		return nil
	})
}

// effectiveDeps 返回每个 step 的**有效依赖**：显式 depends_on ∪ 从 input 模板推断出的引用。
//
// 为什么需要推断：S4 起无依赖的 step 会**并行**执行。若 step B 的 input 引用了
// ${steps.A.output.x} 却没声明 depends_on，并行下 B 可能先跑并读到「尚未执行」的 A。
// 推断把这种隐式依赖变成显式边，消除并行化引入的时序陷阱（PRD §4.3）。
//
// 校验（F3）：显式依赖或引用指向不存在的 step、以及自引用，都在此显式报错。
func effectiveDeps(steps []model.Step) ([][]string, error) {
	idset := make(map[string]bool, len(steps))
	for _, s := range steps {
		idset[s.ID] = true
	}

	out := make([][]string, len(steps))
	for i, s := range steps {
		seen := map[string]bool{}
		var order []string

		add := func(d string, kind string) error {
			if !idset[d] {
				return fmt.Errorf("step %s %s不存在的 step: %s", s.ID, kind, d)
			}
			if d == s.ID {
				return fmt.Errorf("step %s 不能依赖/引用自身", s.ID)
			}
			if !seen[d] {
				seen[d] = true
				order = append(order, d)
			}
			return nil
		}

		for _, d := range s.DependsOn {
			if err := add(d, "依赖"); err != nil {
				return nil, err
			}
		}
		refs := map[string]bool{}
		if err := collectRefs(s.Input, refs); err != nil {
			return nil, fmt.Errorf("step %s input 模板解析失败: %w", s.ID, err)
		}
		// when 里引用的输出同样构成依赖：必须先产出才能判断条件。
		// 注意 when 是**表达式**（无 ${} 包裹），故用独立的正则收集。
		collectWhenRefs(s.When, refs)
		// 排序保证推断边稳定（map 遍历无序，避免结果抖动）。
		names := make([]string, 0, len(refs))
		for r := range refs {
			names = append(names, r)
		}
		sort.Strings(names)
		for _, r := range names {
			if err := add(r, "引用"); err != nil {
				return nil, err
			}
		}
		out[i] = order
	}
	return out, nil
}

// depMap 返回 stepID -> 有效依赖 id 列表（含推断边），供跳过传播等场景使用。
func depMap(steps []model.Step) (map[string][]string, error) {
	deps, err := effectiveDeps(steps)
	if err != nil {
		return nil, err
	}
	m := make(map[string][]string, len(steps))
	for i, s := range steps {
		m[s.ID] = deps[i]
	}
	return m, nil
}

// topoLevels 按有效依赖做 Kahn **分层**：同一层内的 step 互不依赖，可并行执行。
// 返回按层组织的 step 下标（层内按下标升序，保证输出稳定）。
func topoLevels(steps []model.Step) ([][]int, error) {
	n := len(steps)
	deps, err := effectiveDeps(steps)
	if err != nil {
		return nil, err
	}

	id2idx := make(map[string]int, n)
	for i, s := range steps {
		id2idx[s.ID] = i
	}
	indeg := make([]int, n)
	adj := make([][]int, n)
	for i, ds := range deps {
		for _, d := range ds {
			di := id2idx[d]
			adj[di] = append(adj[di], i)
			indeg[i]++
		}
	}

	cur := make([]int, 0, n)
	for i := 0; i < n; i++ {
		if indeg[i] == 0 {
			cur = append(cur, i)
		}
	}

	var levels [][]int
	done := 0
	for len(cur) > 0 {
		sort.Ints(cur) // 稳定输出（便于日志与测试断言）
		levels = append(levels, cur)
		done += len(cur)
		next := make([]int, 0, n)
		for _, u := range cur {
			for _, v := range adj[u] {
				indeg[v]--
				if indeg[v] == 0 {
					next = append(next, v)
				}
			}
		}
		cur = next
	}
	if done != n {
		return nil, fmt.Errorf("Flow 存在循环依赖")
	}
	return levels, nil
}

// topoOrder 是 topoLevels 的扁平化版本（保持依赖顺序），供串行场景与既有测试使用。
func topoOrder(steps []model.Step) ([]int, error) {
	levels, err := topoLevels(steps)
	if err != nil {
		return nil, err
	}
	order := make([]int, 0, len(steps))
	for _, l := range levels {
		order = append(order, l...)
	}
	return order, nil
}
