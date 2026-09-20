package engine

import (
	"fmt"
	"sort"
	"strings"

	"gear/internal/model"
)

// ---- 模板引用 `${...}` 的**单一声明式解析** ----
//
// 这套语法在本引擎有三类消费者：
//  1. 运行期展开      —— expandString
//  2. 依赖推断        —— deps.go collectRefs（`${steps.X.…}` ⇒ step X 是隐式依赖，并行下保证时序）
//  3. 注册期静态校验  —— validateTemplates
//
// 三者必须对「什么算一次引用 / 前缀是什么 / 路径如何切分」完全一致，否则会出现
// 「校验放过、展开报错」或「推断漏边、并行读到未产出的输出」这类只在特定路径才暴露的漂移。
// 因此解析只实现一次（本文件），其余消费者共用；新增语法（如未来的 `${secrets.x}`）时
// **改这里就同时作用于三个消费者**。

const (
	prefixVars    = "vars"    // Flow 内部变量（已实现，§4.3）
	prefixSteps   = "steps"   // 上游 step 的输出（已实现，§4.3）
	prefixSecrets = "secrets" // 密钥（已实现，REQ-TMPL-SECRETS；执行期经宿主 secret 源解析，Flow 定义只存引用）
)

// templateRef 是一次 `${...}` 引用的解析结果（含位置，供原地替换）。
type templateRef struct {
	Start  int      // "${" 的起始下标
	End    int      // "}" 的下标（含）
	Raw    string   // token 原文（去掉 ${ } 与首尾空白）
	Prefix string   // 第一段：vars / steps / secrets / 未知
	Path   []string // 前缀之后的路径段
}

// scanTemplateRefs 扫描字符串里的全部 `${...}`。
//
// 未闭合（有 `${` 无 `}`）直接报错：运行期展开历来如此，静态校验必须与之一致，
// 否则又会出现「注册期放过、运行期报错」的错位（这正是 F3 要消除的模式）。
func scanTemplateRefs(s string) ([]templateRef, error) {
	var out []templateRef
	for i := 0; i < len(s); {
		rel := strings.Index(s[i:], "${")
		if rel < 0 {
			break
		}
		start := i + rel
		relEnd := strings.Index(s[start:], "}")
		if relEnd < 0 {
			return nil, fmt.Errorf("模板未闭合: %s", s)
		}
		end := start + relEnd
		raw := strings.TrimSpace(s[start+2 : end])
		parts := strings.Split(raw, ".")
		out = append(out, templateRef{
			Start:  start,
			End:    end,
			Raw:    raw,
			Prefix: parts[0],
			Path:   parts[1:],
		})
		i = end + 1
	}
	return out, nil
}

// prefixErr 解释「该前缀为何不可用」——注册期校验与运行期展开**共用同一解释**，
// 避免同一个缺陷出现两套文案（两处文案就是两处真相，改一处漏一处）。
func prefixErr(r templateRef) error {
	return fmt.Errorf("未知引用前缀: %s", r.Prefix)
}

// expandString 展开字符串里的全部 `${...}`。
//
// 从**后往前**替换，避免前面的替换改变后面引用的下标。
// 注意：替换结果**不再二次展开**（旧实现循环扫描整个字符串，若某个 vars 值本身含 `${`
// 会被反复展开，重则死循环）。
func expandString(s string, ctx *runContext) (string, error) {
	refs, err := scanTemplateRefs(s)
	if err != nil {
		return "", err
	}
	for i := len(refs) - 1; i >= 0; i-- {
		r := refs[i]
		val, err := lookup(r, ctx)
		if err != nil {
			return "", err
		}
		s = s[:r.Start] + fmt.Sprintf("%v", val) + s[r.End+1:]
	}
	return s, nil
}

// lookup 按 (前缀, 路径) 取值。
func lookup(r templateRef, ctx *runContext) (interface{}, error) {
	switch r.Prefix {
	case prefixVars:
		if len(r.Path) == 0 {
			return nil, fmt.Errorf("vars 引用缺少键名: %s", r.Raw)
		}
		return walk(ctx.vars, r.Path)
	case prefixSteps:
		if len(r.Path) == 0 {
			return nil, fmt.Errorf("steps 引用缺少 step id: %s", r.Raw)
		}
		st, ok := ctx.steps[r.Path[0]]
		if !ok {
			return nil, fmt.Errorf("step 尚未执行: %s", r.Path[0])
		}
		return walk(st, r.Path[1:])
	case prefixSecrets:
		if len(r.Path) == 0 {
			return nil, fmt.Errorf("secrets 引用缺少密钥名: %s", r.Raw)
		}
		if ctx.secretGet == nil {
			return nil, fmt.Errorf("secret 原语未提供：宿主未注入 secret 实现（见 REQ-TMPL-SECRETS），无法解析 %s", r.Raw)
		}
		v, err := ctx.secretGet(r.Path[0])
		if err != nil {
			return nil, fmt.Errorf("解析 secret %q 失败: %w", r.Path[0], err)
		}
		return v, nil
	default:
		return nil, prefixErr(r)
	}
}

// walk 沿路径下钻取值。
func walk(m map[string]interface{}, keys []string) (interface{}, error) {
	var cur interface{} = m
	for _, k := range keys {
		mm, ok := cur.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("路径 %q 不能继续下钻", k)
		}
		cur, ok = mm[k]
		if !ok {
			return nil, fmt.Errorf("键不存在: %s", k)
		}
	}
	return cur, nil
}

// walkStrings 递归访问值里所有字符串（map / slice 下钻），对每个字符串调用 fn。
// map 键先排序，保证多个错误时**报错顺序稳定**（map 遍历无序会让报错抖动）。
func walkStrings(v interface{}, fn func(string) error) error {
	switch x := v.(type) {
	case string:
		return fn(x)
	case map[string]interface{}:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if err := walkStrings(x[k], fn); err != nil {
				return err
			}
		}
	case []interface{}:
		for _, vv := range x {
			if err := walkStrings(vv, fn); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateTemplates 注册期静态校验 Flow 里全部模板引用（F3：能静态校验的不留到运行期）。
//
// 校验范围（只覆盖能被静态判定的部分）：
//   - 前缀必须是内核**已实现**的三类：vars / steps / secrets（其余前缀在此显式拒绝——
//     新增前缀必须同时改本文件的 lookup 与这里的 switch，单一真相源）
//   - `${vars.<k>}`：k 必须是 Flow.vars 的顶层键（拼错在注册期暴露，而不是等到某次执行）
//   - `${steps.<id>}`：必须带 step id（拒绝 `${steps}` 这种残缺引用）
//   - `${secrets.<name>}`：必须带密钥名；密钥本身**是否存在**不在此校验（密钥源是运行期
//     宿主能力，注册期无从知晓），缺失留待运行期解析时报错
//
// 刻意**不**在此重复校验的内容（同一规则只留一处实现，避免漂移）：
//   - `${steps.X}` 的 X 是否存在、是否自引用 —— 由依赖图（effectiveDeps）唯一负责；
//     它在 topoLevels 中被调用，注册期同样会执行。
//
// 边界（显式声明）：只判定**首段**存在性，不判定深层路径与类型。
// `${vars.a.b}` 中 a 存在而 b 不存在，仍留待执行期报错——深层校验需要值的静态类型，
// 当前模型不携带该信息，硬做会与运行期语义分叉（§0 单一真相源）。
func validateTemplates(f *model.Flow) error {
	for _, s := range f.Steps {
		err := walkStrings(s.Input, func(str string) error {
			refs, err := scanTemplateRefs(str)
			if err != nil {
				return fmt.Errorf("step %s: %w", s.ID, err)
			}
			for _, r := range refs {
				switch r.Prefix {
				case prefixVars:
					if len(r.Path) == 0 {
						return fmt.Errorf("step %s: vars 引用缺少键名: %s", s.ID, r.Raw)
					}
					if _, ok := f.Vars[r.Path[0]]; !ok {
						return fmt.Errorf("step %s: 引用 ${%s} 未定义的 vars 键: %s", s.ID, r.Raw, r.Path[0])
					}
				case prefixSteps:
					if len(r.Path) == 0 {
						return fmt.Errorf("step %s: steps 引用缺少 step id: %s", s.ID, r.Raw)
					}
				case prefixSecrets:
					if len(r.Path) == 0 {
						return fmt.Errorf("step %s: secrets 引用缺少密钥名: %s", s.ID, r.Raw)
					}
					// 注：secret 密钥本身**不**在注册期校验（密钥源是运行期宿主能力，
					// 注册期无从知晓）；缺失密钥在运行期解析时报错（同 vars 的深层路径留待运行期）。
				default:
					return fmt.Errorf("step %s: %w", s.ID, prefixErr(r))
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}
