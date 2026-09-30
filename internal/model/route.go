package model

import (
	"fmt"
	"regexp"
	"strings"
)

// APIRoute 是 Tool 可选字段 `api:` 的内容（ENTERPRISE_PRD FR-3）。
//
// 它回答的是「这个 Tool 对外是个什么资源」：`api: {method: GET, path: /api/orders/{id}}`
// 让引擎额外挂一条 RESTful 路由，路径参数映射进 Tool 输入，响应体即 Tool 输出。
// 声明了 `api` 的 Tool **仍然**挂在 `POST /api/tools/{id}/run` 上——资源路由是加挂，
// 不是替换（FR-3 的向后兼容红线，见 AC-C4）。
//
// 刻意不做的事：
//   - **不做鉴权**。gear 没有用户库（Q1 已拍板：授权是业务的事，走 Hook）。资源路由是
//     「长得像 REST」，不是「真的 REST 服务器」——前门该由应用自己的 Hook 守。
//   - **不做类型转换**。路径参数恒为字符串（与 JSON body 里的 "42" 同一口径），
//     Tool 自己决定要不要 `tonumber(input.id)`。
type APIRoute struct {
	// Method 必须是 APIMethods 里的一个（大写，精确匹配；小写会被拒而不是被归一化——
	// 静默改写用户输入是最难查的一类「帮忙」）。
	Method string `yaml:"method" json:"method"`
	// Path 必须以 `/api/` 开头；`{name}` 是路径参数，名字即输入字段名。
	Path string `yaml:"path" json:"path"`
}

// APIMethods 是允许声明的 HTTP 方法（单一来源：校验、诊断文案、前端生成器都读它）。
var APIMethods = map[string]bool{
	"GET": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true,
}

// APIPathPrefix 是所有声明路由的公共前缀（内核 REST 面也挂在这一棵树上）。
const APIPathPrefix = "/api/"

// ReservedRoots 是内核已占用的资源根（相对 `/api/` 的那一段，如 tools/flows/hooks…）。
// 声明路由踩进这些子树，轻则与内核路由撞 pattern（net/http 注册同名 pattern 会
// **panic**，而那一次 panic 发生在起服时，等于整个服务起不来），重则把内核 API 遮掉。
//
// ⚠ 这份表与 internal/server/server.go 的路由表是同一个事实的两处表达（model 不能
// import server，会成环）。兜底在 server 的通配挂载点（请求期按段匹配，撞名那条直接
// 匹配不上 → 404 + 点名），所以即便这里漏了一条，最坏结果是那条路由不生效并留下 404，
// 而不是 panic。
var ReservedRoots = []string{
	"apps", "capabilities", "events", "executions", "flows",
	"healthz", "hooks", "news", "secrets", "tools",
}

var apiPlaceholderName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Validate 校验 `api:` 的**形状**（不涉及 registry、不涉及其它 Tool）。
//
// 与 FR-2b 的迁移同理：能静态判定的错误不留到运行期。路由非法如果只在起服时炸，
// 那就是「一个 YAML 字段能让人打不开服务」——这种破坏力必须在 gear_validate 就被挡住。
func (a APIRoute) Validate() error {
	if err := ValidateAPIMethod(a.Method); err != nil {
		return err
	}
	segs, err := ValidateAPIPath(a.Path)
	if err != nil {
		return err
	}
	for _, r := range ReservedRoots {
		if segs[0] == r {
			return fmt.Errorf("path=%q 占着内核已占用的资源根 /%s：换一个应用自己的子树，"+
				"（如 /api/<你的资源>），内核 API 不能被声明路由遮掉", a.Path, r)
		}
	}
	return nil
}

// ValidateAPIMethod 校验方法名。单独导出是为了让校验期给出**字段级**诊断
// （`tool(x).api.method` 而不是笼统的一条），智能体才知道该改哪一行。
func ValidateAPIMethod(m string) error {
	if m == "" {
		return fmt.Errorf("api.method 不能为空（应为 GET / POST / PUT / PATCH / DELETE 之一）")
	}
	if !APIMethods[m] {
		return fmt.Errorf("api.method=%q 非法（应为 %s 之一，且必须大写）", m, strings.Join(apiMethodList(), "/"))
	}
	return nil
}

// ValidateAPIPath 校验路径模板并返回它的段。段里只允许字面量或单个 `{name}` 占位符。
func ValidateAPIPath(p string) ([]string, error) {
	if p == "" {
		return nil, fmt.Errorf("api.path 不能为空（资源路由必须指向一个路径，如 /api/orders）")
	}
	if !strings.HasPrefix(p, APIPathPrefix) {
		return nil, fmt.Errorf("api.path=%q 必须以 %q 开头（资源路由挂在内核 REST 面这一棵树上）", p, APIPathPrefix)
	}
	rel := strings.TrimPrefix(p, APIPathPrefix)
	if strings.TrimSpace(rel) == "" {
		return nil, fmt.Errorf("api.path 至少要有一段（%q 之上没有资源名）", p)
	}
	if strings.HasSuffix(rel, "/") {
		return nil, fmt.Errorf("api.path=%q 不能以 / 结尾", p)
	}
	segs := strings.Split(rel, "/")
	for i, s := range segs {
		if strings.TrimSpace(s) == "" {
			return nil, fmt.Errorf("api.path=%q 第 %d 段为空（不要写 //）", p, i+1)
		}
		if s == "{}" {
			return nil, fmt.Errorf("api.path=%q 第 %d 段是空的 {}：占位符要写名字（如 {id}），"+
				"名字同时是 Tool 的输入字段名", p, i+1)
		}
		if strings.HasPrefix(s, "{") || strings.HasSuffix(s, "}") {
			if !strings.HasPrefix(s, "{") || !strings.HasSuffix(s, "}") {
				return nil, fmt.Errorf("api.path=%q 第 %d 段 %q 的 { } 不成对", p, i+1, s)
			}
			name := s[1 : len(s)-1]
			if !apiPlaceholderName.MatchString(name) {
				return nil, fmt.Errorf("api.path=%q 第 %d 段 %q 的占位符名 %q 非法"+
					"（应为字母/下划线开头的字母数字，如 {id} / {orderNo}）", p, i+1, s, name)
			}
			continue
		}
		if strings.Contains(s, "{") || strings.Contains(s, "}") {
			return nil, fmt.Errorf("api.path=%q 第 %d 段 %q 里出现了半个 { 或 }", p, i+1, s)
		}
	}
	return segs, nil
}

// Params 返回路径里所有占位符的**名字**、按它们在路径中出现的顺序。
// 顺序即把 URL 映射进 Tool 输入的依据（handler 用 r.PathValue(name) 取值）。
func (a APIRoute) Params() []string {
	segs, err := ValidateAPIPath(a.Path)
	if err != nil {
		return nil // 未校验通过的声明不会走到注册，给 nil 让调用方不必判空
	}
	var out []string
	for _, s := range segs {
		if len(s) > 2 && strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}") {
			out = append(out, s[1:len(s)-1])
		}
	}
	return out
}

// Pattern 是 net/http 路由表的注册形态（"GET /api/orders/{id}"）。
func (a APIRoute) Pattern() string { return a.Method + " " + a.Path }

func apiMethodList() []string {
	out := make([]string, 0, len(APIMethods))
	for m := range APIMethods {
		out = append(out, m)
	}
	return out
}
