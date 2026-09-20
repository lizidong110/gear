package server

// 密钥管理面（REQ-API-SECRETS）。
//
// 端点与语义（与 README §11 的接口表逐条对应）：
//
//	GET    /api/secrets        列出密钥名（**只回名字，绝不回明文**）
//	PUT    /api/secrets/{name} 写入/覆盖密钥（body {"value":"..."}；空值或非法名 → 400）
//	DELETE /api/secrets/{name} 删除密钥（不存在 → 404）
//
// 安全红线（写在这里，避免后续改动无意破坏）：
//  1. **任何响应都不回明文**——包括错误信息里也不能带 value（错误只带密钥名）。
//  2. **写权限只给宿主/运维**，不暴露给沙箱插件：一次覆盖即可劫持后续所有使用者的凭据。
//  3. 名字会被写进 `${secrets.<NAME>}` 模板，故语法校验在 cap 层统一（cap.ValidSecretName）。
//
// 存储为进程内存态、重启即失；启动期由 GEAR_SECRET_<NAME> 播种（见 cap.SecretStore.SeedFromEnv）。

import (
	"errors"
	"fmt"
	"net/http"

	"gear/internal/cap"
)

// handleListSecrets 列出密钥名（不回明文、不回值）。
func (s *Server) handleListSecrets(w http.ResponseWriter, r *http.Request) {
	if s.secrets == nil {
		writeErr(w, http.StatusServiceUnavailable, fmt.Errorf("密钥源未初始化"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"names": s.secrets.Names()})
}

// handleSetSecret 写入/覆盖一个密钥。body 为 {"value":"..."}。
//
// 400 的两种情形都属「客户端可修」的错误，故分开表述：body 不是合法 JSON / 缺 value（或空）。
// 密钥名非法同样 400（cap 层判，与 ${secrets.<NAME>} 模板语法同源）。
func (s *Server) handleSetSecret(w http.ResponseWriter, r *http.Request) {
	if s.secrets == nil {
		writeErr(w, http.StatusServiceUnavailable, fmt.Errorf("密钥源未初始化"))
		return
	}
	name := r.PathValue("name")
	if !cap.ValidSecretName(name) {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("密钥名 %q 非法（不得为空，且不得含空白或 ${} 等模板语法字符）", name))
		return
	}
	var body struct {
		Value *string `json:"value"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if body.Value == nil || *body.Value == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("缺少 value（空值密钥无意义，如需占位请写非空字符串）"))
		return
	}
	if err := s.secrets.Set(name, *body.Value); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	// 刻意不回显 value（红线：任何响应不含明文）。
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "name": name})
}

// handleDeleteSecret 删除一个密钥；不存在 → 404。
func (s *Server) handleDeleteSecret(w http.ResponseWriter, r *http.Request) {
	if s.secrets == nil {
		writeErr(w, http.StatusServiceUnavailable, fmt.Errorf("密钥源未初始化"))
		return
	}
	name := r.PathValue("name")
	if !cap.ValidSecretName(name) {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("密钥名 %q 非法", name))
		return
	}
	if err := s.secrets.Delete(name); err != nil {
		if errors.Is(err, cap.ErrSecretNotFound) {
			writeErr(w, http.StatusNotFound, err)
			return
		}
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "name": name})
}
