package engine

import (
	"strings"
)

// AddOpenURL 把某个 Flow 收尾步带回的「打开地址」收进引擎（按 flowID 聚合、去重）。
// 空串不收；map 首次使用时惰性初始化（New 不必预建）。
func (e *Engine) AddOpenURL(flowID, url string) {
	url = strings.TrimSpace(url)
	if url == "" {
		return
	}
	e.openURLMu.Lock()
	defer e.openURLMu.Unlock()
	if e.openURLs == nil {
		e.openURLs = map[string][]string{}
	}
	for _, u := range e.openURLs[flowID] {
		if u == url {
			return
		}
	}
	e.openURLs[flowID] = append(e.openURLs[flowID], url)
}

// OpenURLs 返回某 Flow 已收集的「打开地址」快照（副本，调用方可安全持有）。
func (e *Engine) OpenURLs(flowID string) []string {
	e.openURLMu.Lock()
	defer e.openURLMu.Unlock()
	out := e.openURLs[flowID]
	cp := make([]string, len(out))
	copy(cp, out)
	return cp
}

// ClearOpenURLs 清掉某 Flow 的打开地址集合（受管 App 停止时调用，避免留下过期链接）。
func (e *Engine) ClearOpenURLs(flowID string) {
	e.openURLMu.Lock()
	defer e.openURLMu.Unlock()
	delete(e.openURLs, flowID)
}

// collectStepOpenURLs 从一次 once 步的 output 里捞「打开地址」。
// 约定：step 返回 {url=...} 或 {open_urls={...}}（或两者兼有），全部收集。
// 这里只做纯读取 + 委托 AddOpenURL，不触碰既有 output，也不因缺少字段报错。
func collectStepOpenURLs(e *Engine, flowID string, output map[string]interface{}) {
	if output == nil {
		return
	}
	if u, ok := output["url"].(string); ok {
		e.AddOpenURL(flowID, u)
	}
	if arr, ok := output["open_urls"].([]interface{}); ok {
		for _, v := range arr {
			if s, ok := v.(string); ok {
				e.AddOpenURL(flowID, s)
			}
		}
	}
}
