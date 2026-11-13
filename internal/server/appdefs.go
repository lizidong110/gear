package server

// appdefs.go —— App 清单管理面 + 导入导出（REQ-API-APP-DEFS）。
//
// 端点（与 README §11 接口表逐条对应）：
//
//	GET    /api/app-defs              列出 registry 里的 App 清单（含组成统计）
//	GET    /api/app-defs/{id}         单查一份清单（含完整组成）
//	GET    /api/app-defs/{id}/export  导出为多文档 YAML 文本（Content-Disposition: attachment）
//	POST   /api/app-defs/import       从 bundle YAML 文本导入（ParseBundle → ApplyBundle）
//	DELETE /api/app-defs/{id}         删除 App 清单（被引用 → 409）
//
// ⚠ 与 /api/apps **不是同一个模型**：/api/apps 管的是本进程内存里的受管长驻进程
// （AppInstance，等价 `gear app run`）；本文件管的是 registry 里的 **App 清单定义**
// （kind: App），即「这个应用由什么组成」的持久化实体。两者同名前缀，别混。
//
// 设计红线：
//  1. **REST 面不碰服务器文件系统**：artifact.Export/Import 吃本地路径，直接透传等于
//     把任意文件读写暴露给浏览器。这里导出走 BuildBundle → MarshalBundle 生成内存文本，
//     导入走 ParseBundle(text) → ApplyBundle，两端都不读磁盘。
//  2. **写入口唯一**：导入必须经 artifact.ApplyBundle（与 gear_apply / gear_import 共用
//     同一份顺序、冲突与校验语义），不许在 REST 层自己 UpsertApp 绕过。
//  3. **静态资源与迁移不在本面**：静态文件无法经文本往返，迁移需要目标库句柄（Migrator）。
//     声明了 migrations 的 bundle 在 REST 导入时**显式拒绝**并指明 CLI 入口，绝不静默跳过
//     —— 否则用户以为导入完整，实际库结构没演进。
//  4. **不静默降级**：dry_run 只汇报不写；冲突（id 已存在且内容不同）默认拒绝，
//     需显式 overwrite=true。

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"gear/internal/artifact"
	"gear/internal/model"
	"gear/internal/registry"
)

// AppDefDTO 是一份 App 清单及其组成摘要。
type AppDefDTO struct {
	App        *model.App           `json:"app"`
	Components *artifact.Components `json:"components"`
	Counts     map[string]int       `json:"counts"`
	SynthNote  string               `json:"synth_note,omitempty"`
}

func appDefSummary(a *model.App, comp *artifact.Components, note string) AppDefDTO {
	return AppDefDTO{
		App: a, Components: comp, SynthNote: note,
		Counts: map[string]int{
			"tools":  len(comp.Tools),
			"flows":  len(comp.Flows),
			"hooks":  len(comp.Hooks),
			"static": len(comp.StaticFiles),
		},
	}
}

// handleListAppDefs 列出全部 App 清单（含组成统计，供列表页展示「由什么组成」）。
func (s *Server) handleListAppDefs(w http.ResponseWriter, r *http.Request) {
	apps, err := s.reg.ListApps()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	out := make([]AppDefDTO, 0, len(apps))
	for _, a := range apps {
		out = append(out, appDefSummary(a, artifact.Composition(s.reg, a), ""))
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"items": out})
}

// handleGetAppDef 单查一份 App 清单；不存在 → 404。
func (s *Server) handleGetAppDef(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a, err := s.reg.GetApp(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if a == nil {
		writeErr(w, http.StatusNotFound, fmt.Errorf("App 清单不存在: %s: %w", id, registry.ErrNotFound))
		return
	}
	writeJSON(w, http.StatusOK, appDefSummary(a, artifact.Composition(s.reg, a), ""))
}

// handleExportAppDef 把一份 App 连同名下 Tool/Flow/Hook 导出成多文档 YAML 文本。
//
// 文本形态 = `gear_apply` 的 bundle 入参（App 文档 + 定义文档），可直接粘回导入框；
// 静态资源无法经文本往返，故在 warnings 里如实说明（不假装导出了完整 .gearapp）。
func (s *Server) handleExportAppDef(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a, note, comp, b, err := artifact.BuildBundle(s.reg, id)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	appDoc, err := artifact.MarshalApp(a)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	bundleDoc, err := artifact.MarshalBundle(b)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	var sb strings.Builder
	sb.WriteString("# Gear App 定义导出\n")
	if note != "" {
		sb.WriteString("# 提示: " + note + "\n")
	}
	if len(comp.StaticFiles) > 0 {
		sb.WriteString("# 注意: 本应用声明了 " + fmt.Sprint(len(comp.StaticFiles)) +
			" 个静态资源，文本导出不含文件内容；完整产物请用 CLI `gear export " + id + "`\n")
	}
	sb.WriteString("---\n")
	sb.Write(appDoc)
	sb.WriteString("---\n")
	sb.Write(bundleDoc)

	w.Header().Set("Content-Type", "application/x-yaml; charset=utf-8")
	w.Header().Set("Content-Disposition", "attachment; filename=\""+strings.ReplaceAll(id, "\"", "")+".bundle.yaml\"")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(sb.String()))
}

// importAppDefsReq 是导入请求体。
type importAppDefsReq struct {
	Bundle    *string `json:"bundle"`
	Overwrite bool    `json:"overwrite,omitempty"`
	DryRun    bool    `json:"dry_run,omitempty"`
}

// handleImportAppDef 从 bundle YAML 文本导入（写入口 = artifact.ApplyBundle）。
//
// dry_run 只汇报 created/updated，一个字节不写；冲突默认拒绝，需 overwrite=true。
func (s *Server) handleImportAppDef(w http.ResponseWriter, r *http.Request) {
	var req importAppDefsReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.Bundle == nil || strings.TrimSpace(*req.Bundle) == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("缺少 bundle（粘贴 App 导出的多文档 YAML）"))
		return
	}
	b, diags := artifact.ParseBundle(*req.Bundle)
	if len(diags) > 0 {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"code": "BAD_BUNDLE", "diagnostics": diags,
			"message": "bundle 解析失败（见 diagnostics，含文档序号与修法）",
		})
		return
	}
	if len(b.Migrations) > 0 {
		// 迁移要连目标库执行 SQL，REST 面没有也不该持有该句柄。静默跳过会让「导入成功」
		// 变成「库结构没演进」的假象，故直接拒绝并指明唯一可行入口。
		writeErr(w, http.StatusBadRequest, fmt.Errorf(
			"bundle 声明了 %d 组 migrations：REST 导入不执行数据库迁移（无目标库句柄）。请用 CLI `gear import` 或 MCP `gear_import`",
			len(b.Migrations)))
		return
	}
	if req.DryRun {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"dry_run":   true,
			"created":   artifact.DiffExisting(s.reg, b, false),
			"updated":   artifact.DiffExisting(s.reg, b, true),
			"conflicts": conflictsFor(s.reg, b, req.Overwrite),
		})
		return
	}
	// db=nil：上面已拒绝声明 migrations 的 bundle，ApplyMigrations 在无迁移时不会读 db。
	created, updated, migrations, err := artifact.ApplyBundle(s.reg, b, nil, req.Overwrite)
	if err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"dry_run": false, "created": created, "updated": updated,
		"migrations_applied": migrations,
	})
}

// conflictsFor 在 dry_run 下预告「不覆盖就会被拒」的已存在对象。
// DiffExisting 只判 id 是否已存在，不比内容；ApplyBundle 还会对内容相同的对象放行，
// 所以这里是**保守上界**（宁可多报、不可漏报），文案按「可能冲突」理解。
func conflictsFor(reg *registry.Registry, b *artifact.Bundle, overwrite bool) []string {
	if overwrite {
		return nil
	}
	return artifact.DiffExisting(reg, b, true)
}

// handleDeleteAppDef 删除一份 App 清单；被引用 → 409（registry.ErrInUse）。
func (s *Server) handleDeleteAppDef(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.reg.DeleteApp(id); err != nil {
		switch {
		case errors.Is(err, registry.ErrNotFound):
			writeErr(w, http.StatusNotFound, err)
		case errors.Is(err, registry.ErrInUse):
			writeErr(w, http.StatusConflict, err)
		default:
			writeErr(w, http.StatusInternalServerError, err)
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "id": id})
}
