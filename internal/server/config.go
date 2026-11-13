package server

// config.go —— 配置键管理面（REQ-API-CONFIG-CATALOG）。
//
// 端点（与 README §11 接口表逐条对应）：
//
//	GET    /api/config/catalog   内置关键配置键目录（中文名/说明/默认值/类型），纯静态
//	GET    /api/config           逐项当前值 + 是否已配置 + 环境变量是否覆盖
//	PUT    /api/config/{key}     写入/覆盖一项（body {"value":"..."}）
//	DELETE /api/config/{key}     删除一项，回到内置默认（不存在 → 404）
//
// 设计红线：
//  1. **目录是内核单一真相源**（cap.ConfigCatalog），UI 不硬编码 key —— 新增键时前端不改
//     就静默漏掉，是「描述比实现窄」的典型静默缺陷。
//  2. **响应绝不回敏感值明文**：ai.api_key 这类只回掩码（与 /api/secrets 同一红线）。
//     PUT 成功也只回 key，不回 value。
//  3. **不静默降级**：环境变量优先级高于配置库，若某个键被 GEAR_AI_* 覆盖，GET 必须报
//     env_override=true —— 否则用户在页面改了却看不到效果，是排障黑洞。
//  4. 允许写目录外的自定义键（与 CLI `gear config set` 同口径，应用自己声明 capabilities:[config]
//     时可能用任意 key），但响应带 known=false，UI 据此提示「请确认拼写」。
//
// 存储与 CLI / 引擎同源（gearhome.ConfigDBPath()），写一次全局生效；实际取值链见
// engine.resolveLLMConfig：调用参数 > 环境变量 > 配置库 > 内置默认。

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"

	"gear/internal/cap"
)

// configItemResp 是一项配置在「当前进程」下的状态。
type configItemResp struct {
	Spec cap.ConfigKeySpec `json:"spec"`
	// Value：非敏感项=库中原值；敏感项=掩码（绝不回明文）；未配置=空串。
	Value string `json:"value"`
	// Set 区分「没配」与「配了空值」：两者在回退链里语义不同，UI 也要分别显示。
	Set    bool `json:"set"`
	Masked bool `json:"masked"`
	// EnvOverride：对应的环境变量非空 ⇒ 该项当前被环境变量压过，页面改它不生效。
	EnvOverride bool   `json:"env_override"`
	EnvName     string `json:"env_name,omitempty"`
	// Known：是否在内置目录里。false 的项没有中文名/默认值，Label 退化为 key 本身。
	Known bool `json:"known"`
}

// handleConfigCatalog 返回内置配置键目录。不读库，纯静态（UI 启动时拉一次即可）。
func (s *Server) handleConfigCatalog(w http.ResponseWriter, r *http.Request) {
	items := cap.ConfigCatalog()
	groups := make([]string, 0, 2)
	seen := map[string]bool{}
	for _, it := range items {
		if !seen[it.Group] {
			seen[it.Group] = true
			groups = append(groups, it.Group)
		}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"items":    items,
		"groups":   groups,
		"priority": "调用参数 > 环境变量 > 配置库 > 内置默认",
	})
}

// handleGetConfig 返回「目录内全部键 + 库里额外的自定义键」的当前状态。
//
// 自定义键必须一并列出：此前用 CLI 或 Lua ctx.config.set 写过的键若不在目录里，
// 页面只渲染目录就会让用户以为「我配的东西不见了」。
func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	if s.config == nil {
		writeErr(w, http.StatusServiceUnavailable, fmt.Errorf("配置库未初始化"))
		return
	}
	stored, err := s.config.List("")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}

	catalog := cap.ConfigCatalog()
	items := make([]configItemResp, 0, len(catalog)+len(stored))
	known := make(map[string]bool, len(catalog))
	for _, spec := range catalog {
		known[spec.Key] = true
		items = append(items, configItem(spec, stored, true))
	}

	// 目录外的键按 key 升序列在后面（稳定顺序，避免 UI 每次刷新乱跳）。
	extra := make([]string, 0, len(stored))
	for k := range stored {
		if !known[k] {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	for _, k := range extra {
		spec := cap.ConfigKeySpec{
			Key: k, Label: k, Group: "自定义配置",
			Description: "不在内置目录里（由 CLI / 应用自行写入）",
			ValueType:   "string",
			Sensitive:   cap.LooksSecretKey(k),
		}
		items = append(items, configItem(spec, stored, false))
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"items": items})
}

// configItem 把一条目录项 + 库里实际值渲染成响应（含脱敏与环境变量覆盖判定）。
func configItem(spec cap.ConfigKeySpec, stored map[string]string, known bool) configItemResp {
	raw, set := stored[spec.Key]
	sensitive := spec.Sensitive || cap.LooksSecretKey(spec.Key)
	out := configItemResp{
		Spec: spec, Set: set, Masked: sensitive && set, Known: known,
		EnvName: spec.Env,
	}
	if set {
		if sensitive {
			out.Value = cap.MaskSecret(raw)
		} else {
			out.Value = raw
		}
	}
	if spec.Env != "" && strings.TrimSpace(os.Getenv(spec.Env)) != "" {
		out.EnvOverride = true
	}
	return out
}

// knownCatalogKey 判断 key 是否在内置目录（用于 configItem 的 Known 回填）。
func knownCatalogKey(key string) bool {
	_, ok := cap.ConfigKeyByKey(key)
	return ok
}

// handleSetConfig 写入/覆盖一项配置。body 为 {"value":"..."}。
//
// 空 value 是合法的（ConfigStore 明确区分「没配」与「配了空值」），用来把某个键
// 显式置空、让它回退到下一级；要彻底删除请用 DELETE。
func (s *Server) handleSetConfig(w http.ResponseWriter, r *http.Request) {
	if s.config == nil {
		writeErr(w, http.StatusServiceUnavailable, fmt.Errorf("配置库未初始化"))
		return
	}
	key := r.PathValue("key")
	var body struct {
		Value *string `json:"value"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if body.Value == nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("缺少 value（如需清空请显式传空串，或改用 DELETE）"))
		return
	}
	if err := s.config.Set(key, *body.Value); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	// 刻意不回显 value：敏感项写进去的是明文，任何回显都会让它进日志/浏览器缓存。
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok": true, "key": key, "known": knownCatalogKey(key),
	})
}

// handleDeleteConfig 删除一项配置（回到内置默认）；不存在 → 404。
func (s *Server) handleDeleteConfig(w http.ResponseWriter, r *http.Request) {
	if s.config == nil {
		writeErr(w, http.StatusServiceUnavailable, fmt.Errorf("配置库未初始化"))
		return
	}
	key := r.PathValue("key")
	if err := s.config.Delete(key); err != nil {
		if errors.Is(err, cap.ErrConfigNotFound) {
			writeErr(w, http.StatusNotFound, err)
			return
		}
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "key": key})
}
