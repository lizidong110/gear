// 反向代理：让 ext.serve 像 nginx 一样「静态目录 + location 反代」一起托管。
//
// 动机（真实缺口）：web-ui 是**同源**应用（默认请求自身 origin 的 `/api`），而内核 REST 跑在
// 另一个端口且不带 CORS 头。纯静态托管下 `/api/**` 全 404，页面显示「内核离线」。
// 于是给 /start 增加可选参数 proxy，把 /api 转发到内核，静态与接口同源。
//
// 形态（args.proxy，缺省=纯静态，完全向后兼容）：
//
//	proxy = {
//	  { path = "/api", target = "http://127.0.0.1:8787" },
//	  { path = "/legacy", target = "http://127.0.0.1:9000", strip_prefix = true },
//	}
//
// 语义对齐 nginx 的 `location <path> { proxy_pass <target>; }`：
//   - 命中 path **前缀**（边界化：/api 命中 /api 与 /api/x，不命中 /apix）；
//   - 多条命中时**最长前缀优先**（最具体的 location 先匹配）；
//   - strip_prefix=false（默认）→ 转发原路径；true → 去掉该前缀再转发
//     （等价 `proxy_pass http://host/;` 的「以 URI 覆盖前缀」写法）；
//   - 未命中任何规则 → 回落静态文件（FileServer）。
//
// 两条纪律：
//   - 规则非法（path 不以 / 开头、target 非 http(s) URL 等）→ **报错**，绝不静默丢弃。
//     静默丢弃正是「UI /api 全 404」这类问题的成因。
//   - 后端不可达 → 返回可见的 **502**，不静默 200、不挂起。否则又变成「页面看着在跑、数据全空」。
package main

import (
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strings"
)

// proxyRule 是一条反向代理规则（类 nginx 的 `location <path> { proxy_pass <target>; }`）。
type proxyRule struct {
	Path        string   // 规范化后的匹配前缀（无尾随 '/'，根路径除外）
	Target      *url.URL // 后端基址（含 scheme/host，可能带路径前缀）
	StripPrefix bool     // 转发时是否去掉 Path 前缀
	TargetRaw   string   // 原始 target 文本（回显 / 报错用）
}

// match 报告路径是否命中本规则前缀（整段前缀语义，带 '/' 边界）。
func (r proxyRule) match(p string) bool {
	if r.Path == "/" {
		return true
	}
	return p == r.Path || strings.HasPrefix(p, r.Path+"/")
}

// normPrefix 规范化匹配前缀：去掉尾部 '/'（根路径保持 "/"）。
func normPrefix(p string) string {
	t := strings.TrimRight(p, "/")
	if t == "" {
		return "/"
	}
	return t
}

// parseProxyRules 解析 args.proxy。缺失返回 (nil, nil)，即纯静态站。
// 任何一条规则不合法都返回 error（fail-closed）。
func parseProxyRules(v interface{}) ([]proxyRule, error) {
	if v == nil {
		return nil, nil
	}
	arr, ok := v.([]interface{})
	if !ok {
		return nil, fmt.Errorf("proxy 必须是数组（[{path,target,strip_prefix}]）")
	}
	rules := make([]proxyRule, 0, len(arr))
	for i, item := range arr {
		m, ok := item.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("proxy[%d] 必须是对象（{path,target,strip_prefix}）", i)
		}
		path := strOr(m, "path", "")
		if path == "" {
			return nil, fmt.Errorf("proxy[%d].path 必填（匹配前缀，如 /api）", i)
		}
		if !strings.HasPrefix(path, "/") {
			return nil, fmt.Errorf("proxy[%d].path 必须以 / 开头，实际 %q", i, path)
		}
		target := strOr(m, "target", "")
		if target == "" {
			return nil, fmt.Errorf("proxy[%d].target 必填（后端基址，如 http://127.0.0.1:8787）", i)
		}
		u, err := url.Parse(target)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf("proxy[%d].target 非法（需 http(s)://host[:port]），实际 %q", i, target)
		}
		strip, _ := m["strip_prefix"].(bool)
		rules = append(rules, proxyRule{
			Path:        normPrefix(path),
			Target:      u,
			StripPrefix: strip,
			TargetRaw:   target,
		})
	}
	// 最长前缀优先：更具体的规则先匹配（类 nginx 最具体 location 优先）。
	sort.SliceStable(rules, func(a, b int) bool { return len(rules[a].Path) > len(rules[b].Path) })
	return rules, nil
}

// newProxyHandler 为一条规则构造反向代理 handler（标准库 httputil，无第三方依赖）。
//
// 用 Rewrite 而非 Director：Go 1.26 起 Director 已弃用（SA1019）。Rewrite 在**出站副本**上改写，
// 语义与 nginx 对齐——SetURL 负责「scheme/host + 基址路径拼接」并把 Host 换成后端
// （等价 proxy_set_header Host $proxy_host）；Rewrite 模式下 X-Forwarded-* 不会自动加，
// 需要转发链信息时显式调 SetXForwarded。
func newProxyHandler(rule proxyRule) http.Handler {
	base := rule.Target
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			if rule.StripPrefix {
				p := strings.TrimPrefix(pr.In.URL.Path, rule.Path)
				if p == "" {
					p = "/"
				}
				if !strings.HasPrefix(p, "/") {
					p = "/" + p
				}
				pr.Out.URL.Path = p
				pr.Out.URL.RawPath = "" // 路径已重写，旧 RawPath 不再成立
			}
			pr.SetURL(base)
			pr.SetXForwarded()
		},
		// 后端不可达必须可见（502），不得静默 200/挂起。
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("[serve] 代理失败 %s -> %s: %v", r.URL.Path, base.String(), err)
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = fmt.Fprintf(w, "502 代理后端不可达: %s", base.String())
		},
	}
}

// newSiteHandler 组装最终 handler：按规则（最长前缀优先）先做反代，未命中回落静态文件。
// 无规则时直接返回纯静态 FileServer —— 与加 proxy 之前的行为逐字节一致（向后兼容）。
func newSiteHandler(dir string, rules []proxyRule) http.Handler {
	fs := http.FileServer(http.Dir(dir))
	if len(rules) == 0 {
		return fs
	}
	handlers := make([]http.Handler, len(rules))
	for i, r := range rules {
		handlers[i] = newProxyHandler(r)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		for i, r := range rules {
			if r.match(req.URL.Path) {
				handlers[i].ServeHTTP(w, req)
				return
			}
		}
		fs.ServeHTTP(w, req)
	})
}

// proxyRulesJSON 把规则序列化成 /start 返回值里的可读回显（可观测：起服时确认规则真的生效）。
func proxyRulesJSON(rules []proxyRule) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(rules))
	for _, r := range rules {
		out = append(out, map[string]interface{}{
			"path":         r.Path,
			"target":       r.TargetRaw,
			"strip_prefix": r.StripPrefix,
		})
	}
	return out
}
