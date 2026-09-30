package model

import (
	"fmt"
	"strings"
)

// App 是 `kind: App` 应用清单的领域模型：`internal/mcp/tools_write.go` 里 bundle 的
// 聚合根，描述「一个应用由什么组成」。
//
// ## 为什么 2026-09-26 要把它提升为一等模型
//
// 在此之前它只活在 MCP 层（旧名 `appManifest`）且**刻意不入 registry**，注释原文是
// 「保持 PRD 的非目标（只有 Flow + app run，没有 App 实体）」。
//
// 这个决定在「gear 自己用」的场景下是对的；但当目标是**发布后让用户在 gear 上自助构建**
// 时，它带来一个结构性后果：
//
//   - 定义（Tool/Flow/Hook）在 registry.db
//   - App 清单只在客户端的 YAML 里，落库即丢弃
//   - 静态资源游离在文件系统，gear 只做 `os.Stat`
//   - 台账 `apps/<id>.json` 只有 pid/端口等运行态
//
// ⇒ **gear 里没有「用户构建的产品」这个对象**，于是也就没有导出、没有组成清单、
// 没有可交付物。这是「产物可移植」缺口的根因（不是可移植性本身——那条 2026-09-25
// 已修，修的是**定义里的路径**，这里是**产物这个对象本身**）。
//
// 把它与 Tool/Flow/Hook 同列进 registry，是 `gear_export` / `gear_import`、
// 「这个应用由什么组成」端点、以及最终 L3「交付给别人用」的共同前提。
//
// ## 兼容性
//
// 字段与 YAML/JSON 形态与旧的 `appManifest` **逐字一致**（yaml + json 双 tag 保持原样），
// MCP 层用 `type appManifest = model.App` 别名复用，既有解析与校验逻辑零改动。
type App struct {
	ID          string      `yaml:"id" json:"id"`
	Name        string      `yaml:"name" json:"name"`
	Description string      `yaml:"description" json:"description"`
	EntryFlow   string      `yaml:"entry_flow" json:"entry_flow"`
	Layout      AppLayout   `yaml:"layout" json:"layout"`
	Start       AppStart    `yaml:"start" json:"start"`
	Requires    AppRequires `yaml:"requires" json:"requires"`
}

// AppLayout 描述应用的静态资源清单。
//
// ⚠ `Files` 里的 path **不由 gear 落地**（`gear_apply` 只做存在性检查，静态资源由调用方
// 自己的文件工具写）。这是当前设计的边界：gear 知道「应该有哪些文件」，但不持有内容。
type AppLayout struct {
	Workdir string    `yaml:"workdir" json:"workdir"`
	Files   []AppFile `yaml:"files" json:"files"`
}

// AppFile 是布局里声明的一份静态资源。Required 为 false 时缺失只报 warning 不阻断。
type AppFile struct {
	Path     string `yaml:"path" json:"path"`
	Required bool   `yaml:"required" json:"required"`
}

// AppStart 描述应用启动方式（重启策略与声明端口）。
type AppStart struct {
	Restart string `yaml:"restart" json:"restart"`
	Ports   []int  `yaml:"ports" json:"ports"`
}

// AppRequires 声明应用依赖的密钥、侧车与能力，供 `gear_doctor` 与启动前校验使用。
type AppRequires struct {
	Secrets      []string `yaml:"secrets" json:"secrets"`
	Sidecars     []string `yaml:"sidecars" json:"sidecars"`
	Capabilities []string `yaml:"capabilities" json:"capabilities"`
}

// Validate 做注册期最小校验（D2/D5：能静态判定的不留到运行期）。
//
// 只校验**不依赖 bundle 上下文**的部分：入口 Flow 是否存在于本次 bundle 或已注册，
// 属于 bundle 级判定，留在 `internal/mcp` 的 `checkAppManifest`（它需要 registry 访问权）。
// 这里的校验是最后一道闸——即使上层绕过 bundle 直接调 `UpsertApp`，脏数据也进不了库。
func (a *App) Validate() error {
	if a == nil {
		return fmt.Errorf("App 为空")
	}
	if strings.TrimSpace(a.ID) == "" {
		return fmt.Errorf("App 的 id 不能为空")
	}
	if strings.ContainsAny(a.ID, " \t\r\n") {
		return fmt.Errorf("App id %q 非法（不得含空白）", a.ID)
	}
	if strings.TrimSpace(a.EntryFlow) == "" {
		return fmt.Errorf("App %s 的 entry_flow 不能为空", a.ID)
	}
	return nil
}

// Owns 判定 id 是否属于本应用的命名空间（IDENTITY_CONTEXT_DESIGN.md §4.2）。
//
// ## 规则
//
//	id 以 `<app.ID>__` 或 `<app.ID>.` 开头 —— 前者是 Tool/Hook 的约定，后者是 Flow 的。
//
// ## 为什么它在 model 包
//
// 这是**全仓唯一的前缀归属判定**。此前它有两份实现（`artifact.Composition` 的局部闭包
// 与 MCP 的 `checkNamespace`），加上运行期就是第三份；「改了写入期忘了运行期」在命名空间
// 上尤其致命——那正是「看起来对、实际错」的典型。放在 model 是唯一不产生循环依赖的位置：
// model → cap，而 cap 不 import 任何 gear 内部包，engine/artifact/mcp 都能拿到它。
//
// ## 它**不**管的事
//
// entry_flow 不带前缀是合法情况（见 artifact.Composition 的豁免）。那是「这个 id 是应用的
// 入口」这一身份带来的豁免，与「前缀规则是什么」是两件事，所以**不塞进本方法**——
// 豁免是调用方的决定：写入期与运行期各自知道自己的豁免理由，但前缀规则只有这一份。
func (a *App) Owns(id string) bool {
	if a == nil || a.ID == "" {
		return false
	}
	return strings.HasPrefix(id, a.ID+"__") || strings.HasPrefix(id, a.ID+".")
}
