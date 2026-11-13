package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"gear/internal/engine"
	"gear/internal/model"
	"gear/internal/registry"
)

// 声明式 REST 路由（ENTERPRISE_PRD FR-3）：Tool 在 YAML 里写 `api: {method, path}`，
// 引擎就让它**额外**成一个资源（如 `GET /api/orders/{id}`）。
//
// 三条语义（对应 AC-C1~C4）：
//  1. 资源路由与 `POST /api/tools/{id}/run` **同时存在**——声明是加挂不是替换；
//  2. 路径参数按名字映射进 Tool 输入（`{id}` → `input["id"]`），值是字符串（不做类型转换）；
//  3. 未声明 `api` 的 Tool 一个字节都不走这里，行为一字不变（AC-C4 / 全量既有测试零变化）。
//
// 「它对外是个资源」的执行者是 apiIndex（见下方），挂载点是每个方法一条
// `METHOD /api/{path...}`——**只有一条**，而不是「每个声明一条」。后者看着更直接，
// 但 net/http 不允许同名 pattern 注册第二次（实测 go1.27 直接 panic），于是「改了声明、
// 删了 Tool、运行期新建 Tool」这些日常动作每一次都可能把服务打挂。一条通配挂载点 +
// 请求期自己匹配，就绕开了「注册即固化」这个坑，代价只是十几行匹配代码。

// toolAPIRoute 是一条已注册的声明路由（解析索引里的条目）。
type toolAPIRoute struct {
	segs   []string // 路径段（已拆好，段内区分字面量与 {name} 占位符）
	names  []string // 与 segs 一一对应；字面量段为空串
	toolID string
}

// apiIndex 缓存「method → 声明路由列表」，按 registry 的 Tool 表指纹失效。
type apiIndex struct {
	mu       sync.Mutex
	sig      string
	byMethod map[string][]toolAPIRoute
}

// apiRouteFor 返回某个方法下所有声明路由（必要时重建索引）。
func (ix *apiIndex) apiRouteFor(reg *registry.Registry, method string) []toolAPIRoute {
	// 指纹查询是「count + max(updated_at)」，比整表读出反序列化便宜一个数量级，
	// 于是可以放心在每个请求上做——索引因此永远是新鲜的（不必重启去拿新声明）。
	sig, err := reg.ToolsSignature()
	if err != nil || sig == ix.sig {
		return ix.byMethod[method]
	}
	tools, err := reg.ListTools()
	if err != nil {
		return ix.byMethod[method]
	}
	next := map[string][]toolAPIRoute{method: {}}
	for _, t := range tools {
		if t == nil || t.API == nil {
			continue
		}
		// 段由 model 拆分并判定（与 gear_validate 同一份规则），这里不另写一套拆分口径。
		segs, err := model.ValidateAPIPath(t.API.Path)
		if err != nil {
			continue // 未通过 model 校验的声明不会进 registry，这里只是兜底
		}
		names := make([]string, len(segs))
		for i, s := range segs {
			if strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}") {
				names[i] = s[1 : len(s)-1]
			}
		}
		next[t.API.Method] = append(next[t.API.Method], toolAPIRoute{segs: segs, names: names, toolID: t.ID})
	}
	ix.mu.Lock()
	ix.sig, ix.byMethod = sig, next
	ix.mu.Unlock()
	return next[method]
}

// handleToolAPIAny 是通配挂载点的入口：把 URL 解析回「哪个 Tool + 哪些路径参数」。
//
// 匹配规则刻意与内核其它路由一致——**字面量段精确匹配，只有一整段被 `{}` 占住才当参数**，
// 所以 `/api/orders/new` 命中字面量路由而不会误判成 `id=new`（Go net/http 自己也是这个口径）。
func (s *Server) handleToolAPIAny(w http.ResponseWriter, r *http.Request) {
	raw := strings.Trim(r.PathValue("path"), "/")
	if raw == "" {
		writeErr(w, http.StatusNotFound, fmt.Errorf(
			"没有 Tool 声明这条资源路由：%s /api（用 gear_catalog 或 GET /api/tools 查哪些 Tool 声明了 api）", r.Method))
		return
	}
	segs := strings.Split(raw, "/")
	for _, rt := range s.apiIdx.apiRouteFor(s.reg, r.Method) {
		if len(rt.segs) != len(segs) {
			continue
		}
		hit := true
		for i := range segs {
			if rt.names[i] == "" && rt.segs[i] != segs[i] {
				hit = false
				break
			}
		}
		if !hit {
			continue
		}
		// ⚠ 路径参数只能在这里取：挂载点是 `/api/{path...}`，它只认识一个叫 path 的通配段，
		// r.PathValue("id") 拿不到声明路由里的 {id}。所以按段自己配一遍——这也是这条设计
		// 必须写清的地方：通配挂载点省掉了「注册即固化」，代价就是取值得自己来。
		params := make(map[string]string, len(rt.names))
		for i, n := range rt.names {
			if n != "" {
				params[n] = segs[i]
			}
		}
		s.handleToolAPI(w, r, rt.toolID, params)
		return
	}
	writeErr(w, http.StatusNotFound, fmt.Errorf(
		"没有 Tool 声明这条资源路由：%s /%s（已声明的资源见 GET /api/tools 里带 api 字段的那些）", r.Method, raw))
}

// handleToolAPI 执行一条资源路由：把 Tool 输出直接当作响应体。
//
// 与 handleRunTool 的差别只有「入参从哪来」与「响应体是什么」：
//   - 入参 = JSON 请求体（POST/PUT/PATCH）并上路径参数，后者优先级更高
//     （路径里写着的那个 id 就是资源身份，不该被 body 里的同名字段顶掉）；
//   - 响应体 = **Tool 输出本身**，不包 `{id, output}`（FR-3：「响应体即 Tool 输出」）。
//
// 身份链路与 handleRunTool 同源：REST 是**真身份面**，必须在引擎边界注入 caller，
// 此后 Hook 与 ctx.caller 才看得见「谁在调」。用 engine.RunTool 会退化成
// context.Background()，鉴权形同虚设。
func (s *Server) handleToolAPI(w http.ResponseWriter, r *http.Request, toolID string, params map[string]string) {
	// 存在性先分流：engine.RunToolExec 对「不存在」给的是无 sentinel 的纯文本错误，
	// 不先查会把「id 写错」与「Tool 跑挂了」混成同一个 400，而两者修法相反。
	found, err := s.reg.GetTool(toolID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if found == nil {
		writeErr(w, http.StatusNotFound, fmt.Errorf(
			"Tool 不存在: %s（用 GET /api/tools 确认 id，注意应用前缀）", toolID))
		return
	}
	input := map[string]interface{}{}
	// 只有带主体的 method 读 body：GET/DELETE 没有 body，读出来了也只是丢弃。
	switch r.Method {
	case http.MethodPost, http.MethodPut, http.MethodPatch:
		var body map[string]interface{}
		if derr := json.NewDecoder(r.Body).Decode(&body); derr != nil && !errors.Is(derr, io.EOF) {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("入参不是合法 JSON 对象: %w", derr))
			return
		}
		for k, v := range body {
			input[k] = v
		}
	}
	// 路径参数最后写：它的优先级高于 body 里的同名键。
	for k, v := range params {
		input[k] = v
	}
	ctx := engine.WithCaller(r.Context(), callerOf(r))
	start := time.Now()
	out, err := s.engine.RunToolExec(ctx, toolID, input, "")
	s.progressToolDone("tool", toolID, start, err, input, out) // 声明资源路由同属直接调用，也要可见（见 progressToolDone）
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if out == nil {
		out = map[string]interface{}{}
	}
	writeJSON(w, http.StatusOK, out)
}
