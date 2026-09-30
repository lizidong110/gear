package artifact

import (
	"fmt"

	"gear/internal/engine"
	"gear/internal/model"
)

// CheckToolRoutes 校验 bundle 里所有 Tool 的 `api:` 声明（FR-3）。
//
// 两件事分开报，因为修法不同：
//   - **形状**（ROUTE_INVALID）：字段级诊断，改自己这一条即可；
//   - **冲突**（ROUTE_CONFLICT）：两边各自都合法，只是不能同时存在——必须点名两个 Tool，
//     改任意一个都行。
//
// 为什么放在**写作期**而不是起服时：net/http 对同名 pattern 的第二次注册是 panic
// （实测 go1.27：pattern "GET /api/a" conflicts with pattern "GET /api/a"）。注册发生在
// `gear serve` 起服那一刻，所以「路由冲突」如果只在起服时才发现，等于一个 YAML 字段
// 就能让整个服务起不来。这里先挡掉，server.registerGuarded 再兜一层底。
//
// 同时被三处使用：gear_validate（写作期反馈）、gear_import 的 validateImported（导入校验）、
// ApplyBundle（原子拒绝）。同一份规则三个入口，避免「这里认、那里不认」的漂移。
func CheckToolRoutes(tools []*model.Tool) []engine.Diagnostic {
	var out []engine.Diagnostic
	for _, t := range tools {
		if t == nil || t.API == nil {
			continue
		}
		field := "tool(" + t.ID + ").api"
		if err := model.ValidateAPIMethod(t.API.Method); err != nil {
			out = append(out, engine.Diagnostic{
				Code:    engine.CodeRouteInvalid,
				Field:   field + ".method",
				Message: fmt.Sprintf("Tool %s 的 api: %v", t.ID, err),
				Hint:    "method 必须是 GET / POST / PUT / PATCH / DELETE 之一且大写（如 `method: GET`）",
			})
		}
		if _, err := model.ValidateAPIPath(t.API.Path); err != nil {
			out = append(out, engine.Diagnostic{
				Code:    engine.CodeRouteInvalid,
				Field:   field + ".path",
				Message: fmt.Sprintf("Tool %s 的 api: %v", t.ID, err),
				Hint:    "path 要以 /api/ 开头、段不能为空，占位符写 {id} 这种带名字的（名字即 Tool 的输入字段）",
			})
		}
	}
	out = append(out, routeConflicts(tools)...)
	return out
}

// routeConflicts 找出 bundle 内撞在同一 method+path 上的两条声明。
func routeConflicts(tools []*model.Tool) []engine.Diagnostic {
	owner := map[string]string{} // "METHOD path" → 第一个声明它的 Tool
	var out []engine.Diagnostic
	for _, t := range tools {
		if t == nil || t.API == nil {
			continue
		}
		pattern := t.API.Pattern()
		first, seen := owner[pattern]
		if !seen {
			owner[pattern] = t.ID
			continue
		}
		out = append(out, engine.Diagnostic{
			Code:  engine.CodeRouteConflict,
			Field: "tool(" + t.ID + ").api",
			Message: fmt.Sprintf("Tool %s 与 Tool %s 声明了同一个资源路由 %s"+
				"（同一 method+path 只能有一个，否则起服时 net/http 注册同名 pattern 会 panic）",
				first, t.ID, pattern),
			Hint: "给其中一个换 path（别再写重复的那条，如它的变体 /api/orders-<something>），" +
				"或去掉它的 `api:` 声明；同一 method+path 只能有一个",
		})
	}
	return out
}

// routeRejectError 把一个「整包拒绝」的理由变成错误：ApplyBundle 用它，
// 保证「路由非法 → 一个定义都不写」与 gear_validate 的诊断口径一致（同一份 CheckToolRoutes）。
func routeRejectError(tools []*model.Tool) error {
	diags := CheckToolRoutes(tools)
	if len(diags) == 0 {
		return nil
	}
	d := diags[0]
	return engine.Diagf(d.Code, d.Field, d.Hint, "%s", d.Message)
}
