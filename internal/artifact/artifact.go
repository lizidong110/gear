// Package artifact 实现「应用产物」：把一个 App 的**定义**（App 清单 + 名下 Tool/Flow/Hook）
// 与它声明的**静态资源**组装成一个自包含、可 diff、可搬运的目录（或 zip），并能导回另一台机器。
//
// ## 为什么要有这个包（2026-09-26）
//
// gear 要作为**发布出去的平台**交付，构建者是用户自己的智能体（经 MCP 面），终极目标 L3 是
// 「用户能构建出类 PalantirLite 的产品并交付给别人用」。在此之前 gear 里的状态是四处散落的：
//
//	定义（Tool/Flow/Hook）→ registry.db
//	App 清单              → 只在客户端 YAML 里（2026-09-26 起已入 registry）
//	静态资源              → 文件系统，gear 只做 os.Stat，从不持有
//	运行台账              → apps/<id>.json（机器绑定，搬走无意义）
//
// 于是「用户构建的产品」不是一个对象，A 机器 → B 机器只有一条路：整拷 GEAR_HOME。
// 那不是交付，是克隆机器。本包给这个对象一个可搬运的形态。
//
// ⚠ 与 2026-09-25 已修的「可移植性」是**两件不同的事**，别混：
// 那次修的是「定义里的路径不能硬编码机器绝对路径」（拷过去跑不起来）；
// 这次解决的是「拷什么」——把产物定义成一个自包含的目录。
//
// ## 形态（APP_ARTIFACT_DESIGN.md §3，格式 v1）
//
//	myapp/
//	├── gear-artifact.yaml   # 元信息：格式版本、导出者、导出时间、组成清单
//	├── app.yaml             # kind: App 清单原样
//	├── bundle.yaml          # 名下全部 Tool/Flow/Hook（多文档 YAML，与 gear_apply 入参同形）
//	└── www/…                # 静态资源（按相对 layout.workdir 的路径原样拷贝）
//
// 目录而非单文件是刻意选择：可 diff、可 git 版本化、可直接改完再导入。
// zip（.gearapp）只是「发给别人」时的同内容打包，两种形态导入端都吃。
//
// ## 静态资源的取舍（设计文档 §3.4）
//
// 导出**拷贝**（产物必须自包含，否则「交付」是假的）；导入**默认不落地**——
// 保持既有边界「gear 从不持有静态资源，那是调用方文件工具的职责」。
// 传 AppsDir 才落地，且会**重写** App 清单的 layout 指向落地位置（否则导进来的应用
// 指向的路径在这台机器上根本不存在）。
//
// ## 2026-10-10 的两处放宽（都是「让 export 真的能用」）
//
//   - **没有 App 清单也能导**：用户的多数应用只有 Flow，把 appID 当入口 Flow 解析并临时
//     合成清单（不写库）；静态资源按入口 Flow 的 static_site.serve 的 dir 推断。
//   - **基准目录自动上溯**：不传 StaticRoot 时从 cwd 逐级向上找能解析声明路径的目录。
//     清单里的路径通常相对仓库根写，而用户常在子目录（build/）里跑 gear。
package artifact

import (
	"archive/zip"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"gear/internal/engine"
	"gear/internal/model"
	"gear/internal/registry"
)

// FormatVersion 是产物格式版本。导入端遇到不认识的版本**直接拒**（不猜、不静默降级）——
// 「能静态判定的绝不留到运行期」在产物格式上同样成立：格式不认识就导入，
// 结果是落一半库，比拒绝难收拾得多。
const FormatVersion = 1

// GearVersion 是写入产物元信息的 gear 内核版本（仅记录 + 兼容提示，导入端不做硬校验）。
//
// ⚠ 与 `internal/mcp.Version` 是同一个值的两份声明：MCP 面用它构造 Implementation，
// 而本包**不能** import internal/mcp（mcp → artifact，反方向会成环）。故改版本必须两处同改，
// 由 `internal/mcp` 的 TestArtifactGearVersionMatchesMCP 机械钉死。
const GearVersion = "0.3.0"

// 产物内的固定文件名。
const (
	// MetaFile 是元信息文件名。
	MetaFile = "gear-artifact.yaml"
	// AppFile 是 App 清单文件名。
	AppFile = "app.yaml"
	// BundleFile 是 Tool/Flow/Hook 定义文件名。
	BundleFile = "bundle.yaml"
	// ArtifactKind 是元信息文档的 kind 值。
	ArtifactKind = "GearArtifact"
	// ZipExt 是包形态的扩展名。
	ZipExt = ".gearapp"
)

// Meta 是 `gear-artifact.yaml` 的内容。
//
// ⚠ Kind **刻意不进 yaml tag**：`kind: GearArtifact` 由 marshalDoc 写在文档头，
// 若结构体里再有一个同名键，序列化出两份 kind，yaml 解析直接报
// 「mapping key "kind" already defined」——**导出的产物自己导不回来**。
// 故落盘的 kind 归文档头，结构体里的 Kind 只在内存/JSON 输出里带（Load 时回填）。
//
// Components 是**冗余但有价值**的：它让一个没装 gear 的人也能看懂这个产品由什么组成，
// 也让导入端能在落库前先做一次自洽性检查（声明了但没带的 Tool 要报出来）。
type Meta struct {
	Kind        string     `yaml:"-" json:"kind"`
	Version     int        `yaml:"version" json:"version"`
	AppID       string     `yaml:"app_id" json:"app_id"`
	GearVersion string     `yaml:"gear_version,omitempty" json:"gear_version,omitempty"`
	ExportedAt  string     `yaml:"exported_at" json:"exported_at"`
	Components  Components `yaml:"components" json:"components"`
}

// Components 是产物的组成清单（与 gear_describe(kind=app).composition 同源、同一份判定）。
type Components struct {
	EntryFlow   string   `yaml:"entry_flow" json:"entry_flow"`
	Tools       []string `yaml:"tools" json:"tools"`
	Flows       []string `yaml:"flows" json:"flows"`
	Hooks       []string `yaml:"hooks" json:"hooks"`
	StaticFiles []string `yaml:"static_files" json:"static_files"`
}

// Bundle 是一个解析后的交付物（定义集合）。
//
// ⚠ 这是 `internal/mcp` 里 bundle 类型的**生产侧定义**（mcp 用类型别名复用），
// 而非复制品：导出、导入、apply 必须走同一份结构，否则「导出的东西 apply 不认」
// 这类漂移迟早发生。
type Bundle struct {
	Apps  []*model.App
	Tools []*model.Tool
	Flows []*model.Flow
	Hooks []*model.Hook
	// Migrations 是「库怎么演进」的声明（ENTERPRISE_PRD FR-2b / Q4）。它是**第三类
	// 产物内容**：不进 registry，也不算定义——内核按它改完目标库就完事，不留一个「迁移对象」。
	// 留空（绝大多数 bundle）时，`migrations` 这件事对内核零影响。
	Migrations []*model.MigrationSet
}

// ParseBundle 把多文档 YAML 解析成 Bundle。
//
// 分类规则（**优先看 kind，无 kind 时退化为字段启发式**）：
//
//	kind: App  → 应用清单
//	kind: Tool/Flow/Hook → 对应类型
//	kind: Migrations → 迁移声明（FR-2b；不落 registry，见 Bundle.Migrations）
//	无 kind    → 有 steps 视为 Flow，有 stage 视为 Hook，否则视为 Tool
//	              （后两条兼容仓库里 examples/*.yaml 的历史写法）
//	未知 kind  → 明确报错（**不猜成 Tool**：猜错会让一份合法文档报出一堆莫名其妙的
//	              「runtime 非法」错，把真正的问题埋掉）
//
// 解析失败不中断：把问题作为诊断收集起来继续解析其余文档——一次报多条才是「编译器」该有的样子。
//
// 2026-09-26：从 internal/mcp 上提到本包，让「MCP 的 gear_apply」与「本包的 gear_import」
// 共用同一份解析（此前若各写一份，导出的产物 apply 不认只是时间问题）。
func ParseBundle(text string) (*Bundle, []engine.Diagnostic) {
	b := &Bundle{}
	var diags []engine.Diagnostic
	for i, doc := range registry.SplitYAMLDocs([]byte(text)) {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		docNo := i + 1
		var probe map[string]any
		if err := yaml.Unmarshal([]byte(doc), &probe); err != nil {
			diags = append(diags, engine.Diagnostic{
				Code: "BAD_YAML", Field: fmt.Sprintf("doc[%d]", docNo),
				Message: fmt.Sprintf("第 %d 份文档不是合法 YAML: %v", docNo, err),
				Hint:    "常见原因：缩进不一致、冒号后缺空格、字符串里的特殊字符没加引号",
			})
			continue
		}
		if len(probe) == 0 {
			continue // 分隔符产生的空文档
		}
		switch classifyDoc(probe) {
		case "app":
			var a model.App
			if err := yaml.Unmarshal([]byte(doc), &a); err != nil {
				diags = append(diags, badDocDiag(docNo, "App", err))
				continue
			}
			b.Apps = append(b.Apps, &a)
		case "tool":
			var t model.Tool
			if err := yaml.Unmarshal([]byte(doc), &t); err != nil {
				diags = append(diags, badDocDiag(docNo, "Tool", err))
				continue
			}
			b.Tools = append(b.Tools, &t)
		case "flow":
			var f model.Flow
			if err := yaml.Unmarshal([]byte(doc), &f); err != nil {
				diags = append(diags, badDocDiag(docNo, "Flow", err))
				continue
			}
			b.Flows = append(b.Flows, &f)
		case "hook":
			var h model.Hook
			if err := yaml.Unmarshal([]byte(doc), &h); err != nil {
				diags = append(diags, badDocDiag(docNo, "Hook", err))
				continue
			}
			b.Hooks = append(b.Hooks, &h)
		case "migrations":
			var ms model.MigrationSet
			if err := yaml.Unmarshal([]byte(doc), &ms); err != nil {
				diags = append(diags, badDocDiag(docNo, "Migrations", err))
				continue
			}
			b.Migrations = append(b.Migrations, &ms)
		default:
			raw, _ := probe["kind"].(string)
			diags = append(diags, engine.Diagnostic{
				Code: "UNKNOWN_KIND", Field: fmt.Sprintf("doc[%d].kind", docNo),
				Message: fmt.Sprintf("第 %d 份文档的 kind=%q 未知（只认 App / Tool / Flow / Hook / Migrations）", docNo, raw),
				Hint:    "bundle 的第一份通常是 `kind: App` 清单，其余是 `kind: Tool` / `kind: Flow` / `kind: Hook`，库演进声明是 `kind: Migrations`",
			})
		}
	}
	return b, diags
}

func badDocDiag(docNo int, kind string, err error) engine.Diagnostic {
	return engine.Diagnostic{
		Code: "BAD_YAML", Field: fmt.Sprintf("doc[%d]", docNo),
		Message: fmt.Sprintf("第 %d 份文档按 %s 解析失败: %v", docNo, kind, err),
		Hint:    "对照 gear_guide(\"authoring\") 的全字段语义检查字段名与类型",
	}
}

// classifyDoc 判定一份文档的类型（见 ParseBundle 的规则表）。
func classifyDoc(probe map[string]any) string {
	if k, ok := probe["kind"].(string); ok {
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "app":
			return "app"
		case "tool":
			return "tool"
		case "flow":
			return "flow"
		case "hook":
			return "hook"
		case "migrations", "migration":
			return "migrations"
		default:
			return "unknown"
		}
	}
	if _, isFlow := probe["steps"]; isFlow {
		return "flow"
	}
	if _, isHook := probe["stage"]; isHook {
		return "hook"
	}
	return "tool"
}

// ---- 序列化 ----

// marshalDoc 把对象渲染成带 kind 头的单份 YAML 文档。
//
// 为什么手写 `kind: X` 而不是给 model 加 Kind 字段：kind 是**交付格式的组织约定**，
// 不是领域模型的属性（registry 存的是 JSON blob，不靠它分类）。塞进模型会让
// 「落库的对象」与「磁盘上的文档」两份形态互相污染。
func marshalDoc(kind string, v any) ([]byte, error) {
	body, err := yaml.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("序列化 %s 失败: %w", kind, err)
	}
	out := make([]byte, 0, len(body)+16)
	out = append(out, "kind: "+kind+"\n"...)
	out = append(out, body...)
	return out, nil
}

// MarshalApp 渲染 App 清单文档。
func MarshalApp(a *model.App) ([]byte, error) { return marshalDoc("App", a) }

// MarshalBundle 渲染 Tool/Flow/Hook 的多文档 YAML（依赖顺序：Tool → Flow → Hook）。
//
// 与 persistBundle 的落库顺序一致——不是巧合：导出的产物被导入时要按同样的顺序落，
// 顺序本身就是「中途失败时已写了什么」的可解释性来源。
func MarshalBundle(b *Bundle) ([]byte, error) {
	var docs [][]byte
	add := func(kind string, v any) error {
		d, err := marshalDoc(kind, v)
		if err != nil {
			return err
		}
		docs = append(docs, d)
		return nil
	}
	for _, t := range b.Tools {
		if err := add("Tool", t); err != nil {
			return nil, err
		}
	}
	for _, f := range b.Flows {
		if err := add("Flow", f); err != nil {
			return nil, err
		}
	}
	for _, h := range b.Hooks {
		if err := add("Hook", h); err != nil {
			return nil, err
		}
	}
	return joinDocs(docs), nil
}

// joinDocs 用 `---` 连接多份文档。空集合返回空切片（不是只有分隔符的文档）。
func joinDocs(docs [][]byte) []byte {
	var out []byte
	for i, d := range docs {
		if i > 0 {
			out = append(out, "---\n"...)
		}
		out = append(out, d...)
	}
	return out
}

// ---- 组成判定 ----

// Composition 反查「这个应用由什么组成」。
//
// 归属判定复用 apply 期的 namespace 规则：id 以 `<appID>__` 或 `<appID>.` 开头即算该应用名下；
// entry_flow 另外单算（它的 id 可能不带前缀，而它就是这个应用的入口）。
//
// 静态资源只报**清单里声明的路径**（且转成相对 layout.workdir 的形态，见 staticRelPath）：
// gear 从不落地这些文件（由调用方自己的文件工具写），所以这里给的是「应该有哪些」，
// 不是「磁盘上有哪些」——后者要用 gear doctor。
func Composition(reg *registry.Registry, a *model.App) *Components {
	c := &Components{
		EntryFlow:   a.EntryFlow,
		Tools:       []string{},
		Flows:       []string{},
		Hooks:       []string{},
		StaticFiles: []string{},
	}
	// 前缀规则只有 model.App.Owns 一份（见其注释）；entry_flow 的豁免是这里的决定。
	if ts, err := reg.ListTools(); err == nil {
		for _, t := range ts {
			if a.Owns(t.ID) {
				c.Tools = append(c.Tools, t.ID)
			}
		}
	}
	if fs, err := reg.ListFlows(); err == nil {
		for _, f := range fs {
			if f.ID == a.EntryFlow || a.Owns(f.ID) {
				c.Flows = append(c.Flows, f.ID)
			}
		}
	}
	if hs, err := reg.ListHooks(); err == nil {
		for _, h := range hs {
			if a.Owns(h.ID) {
				c.Hooks = append(c.Hooks, h.ID)
			}
		}
	}
	for _, f := range a.Layout.Files {
		c.StaticFiles = append(c.StaticFiles, staticRelPath(a, f.Path))
	}
	sort.Strings(c.Tools)
	sort.Strings(c.Flows)
	sort.Strings(c.Hooks)
	sort.Strings(c.StaticFiles)
	return c
}

// staticRelPath 把 App 清单里声明的静态资源路径转成**产物内相对路径**。
//
// 规则：声明路径以 `layout.workdir` 为前缀时剥掉该前缀（`apps/mindstorm/www/index.html`
// + workdir `apps/mindstorm` ⇒ `www/index.html`）；否则原样保留（清洗后）。
//
// 为什么要剥：workdir 是「这个应用自己的根」，产物内应当以它为零点——否则
// 导进来的产物会把导出方的目录结构（`apps/mindstorm/…`）一起搬过去，
// 而那只是导出方仓库的摆放习惯，不是应用的一部分。
func staticRelPath(a *model.App, declared string) string {
	p := filepath.ToSlash(filepath.Clean(strings.TrimSpace(declared)))
	wd := filepath.ToSlash(filepath.Clean(strings.TrimSpace(a.Layout.Workdir)))
	if wd != "" && wd != "." {
		if strings.HasPrefix(p, wd+"/") {
			return strings.TrimPrefix(p, wd+"/")
		}
	}
	return strings.TrimPrefix(p, "./")
}

// ---- 应用解析：App 清单优先，缺失则按 Flow 合成 ----

// resolveExportApp 取出「要导出的 App 清单」。
//
// 优先 registry 里的 kind: App 清单；没有则退一步把 appID 当作**入口 Flow** 的 id
// 解析（见 findEntryFlow），并按该 Flow 临时**合成**一份清单（**不写库**）。
//
// ## 为什么允许合成（2026-10-10）
//
// 用户的绝大多数应用是「只注册了 Flow」就跑起来的（`gear app run/start`），从来没有
// kind: App 清单——于是 `gear export wms` 直接 NOT_FOUND，而用户要的只是「把这个应用的
// 定义带走」。合成让 export 对任何已注册的入口立即可用；静态资源按入口 Flow 的
// static_site.serve 步推断（见 inferLayoutFromFlow）。
//
// 合成**不写库**：export 是只读操作，产物里的 app.yaml 就是这份合成清单，
// 导回（`gear import`）时才会真正落进 registry。
//
// 返回的第二个值是合成说明（非空表示走了合成路径），由调用方并进 Warnings——
// 产物能用，但清单是推断出来的，用户有权知道。
func resolveExportApp(reg *registry.Registry, appID string) (*model.App, string, error) {
	a, err := reg.GetApp(appID)
	if err != nil {
		return nil, "", fmt.Errorf("读取应用 %s 失败: %w", appID, err)
	}
	if a != nil {
		return a, "", nil
	}
	flow, err := findEntryFlow(reg, appID)
	if err != nil {
		return nil, "", err
	}
	if flow == nil {
		return nil, "", engine.Diagf("NOT_FOUND", "app_id",
			"用 gear_catalog(kind=app) 看有哪些 App 清单；只有 Flow 没有清单的应用，"+
				"直接用入口 Flow 的 id 也能导（如 wms 或 wms.up）",
			"应用 %s 不存在：registry 里既没有它的 App 清单，也没有可对应的 Flow", appID)
	}
	lay := inferLayoutFromFlow(flow)
	app := &model.App{
		ID:          appID,
		Name:        flow.Name,
		Description: flow.Description,
		EntryFlow:   flow.ID,
		Layout:      lay,
	}
	note := fmt.Sprintf("registry 里没有 %s 的 App 清单，已按入口 Flow %s 临时合成一份用于导出（不写库）", appID, flow.ID)
	if len(lay.Files) > 0 {
		note += fmt.Sprintf("；静态资源按该 Flow 的 static_site.serve dir=%s 推断", lay.Files[0].Path)
	} else {
		note += "；该 Flow 未服务静态站，产物只含定义"
	}
	note += "。要固化这份清单，用 gear_apply 提交 kind: App 定义。"
	return app, note, nil
}

// findEntryFlow 把一个「应用 id」解析成入口 Flow：精确 id → `<id>.up` → 唯一 `<id>.` 前缀。
//
// 多个前缀命中即报歧义：宁可让用户把 id 说清楚，也不猜一个——猜错会导出一个
// 名字对不上的产物，而错在哪要等导回时才发现。
func findEntryFlow(reg *registry.Registry, appID string) (*model.Flow, error) {
	if f, err := reg.GetFlow(appID); err != nil {
		return nil, err
	} else if f != nil {
		return f, nil
	}
	if f, err := reg.GetFlow(appID + ".up"); err != nil {
		return nil, err
	} else if f != nil {
		return f, nil
	}
	flows, err := reg.ListFlows()
	if err != nil {
		return nil, err
	}
	var hits []string
	for _, f := range flows {
		if strings.HasPrefix(f.ID, appID+".") {
			hits = append(hits, f.ID)
		}
	}
	switch len(hits) {
	case 0:
		return nil, nil
	case 1:
		return reg.GetFlow(hits[0])
	default:
		sort.Strings(hits)
		return nil, engine.Diagf("BAD_ARGUMENT", "app_id",
			"用入口 Flow 的**完整 id** 再导一次即可消歧",
			"%s 对应多个 Flow（%s），无法确定哪个是入口", appID, strings.Join(hits, ", "))
	}
}

// inferLayoutFromFlow 从入口 Flow 的 static_site.serve 步推断静态资源布局。
//
// 约定与手写清单一致：把 dir 的父目录当 workdir、dir 本身声明为一个「目录」项，
// 产物内便是 dir 的末段（`apps/wms/www` → workdir `apps/wms` → 产物内 `www/`）。
// 绝对 dir 也照推断：源解析时绝对路径绕过基准目录，产物仍是自包含的。
//
// dir 为空或含模板（`${...}`）时**不推断**——猜一个会静默带上错目录，
// 不如留空并让说明里讲清「产物只含定义」。
func inferLayoutFromFlow(f *model.Flow) model.AppLayout {
	for _, s := range f.Steps {
		if s.Call != "static_site.serve" {
			continue
		}
		raw, _ := s.Input["dir"].(string)
		raw = strings.TrimSpace(raw)
		if raw == "" || strings.Contains(raw, "${") {
			return model.AppLayout{}
		}
		clean := filepath.Clean(raw)
		dir := filepath.ToSlash(clean)
		wd := filepath.ToSlash(filepath.Dir(clean))
		if wd == "." {
			wd = ""
		}
		return model.AppLayout{
			Workdir: wd,
			Files:   []model.AppFile{{Path: dir, Required: false, Dir: true}},
		}
	}
	return model.AppLayout{}
}

// ---- 导出 ----

// ExportOptions 是导出的入参。
type ExportOptions struct {
	// AppID 是要导出的应用 id（registry 里的 App 清单 id）。
	AppID string
	// Out 是产物输出路径：dir 形态为要创建的目录，zip 形态为 zip 文件路径。
	Out string
	// Format 是产物形态：`dir`（默认，可 diff 可 git 版本化）| `zip`（打成单个包便于发送）。
	Format string
	// NoStatic 为 true 时**不**拷静态资源，只导出定义。默认为 false（拷）——
	// 「产物必须自包含」是默认值：忘了设就得到一个不能交付的产物，代价太大。
	// 用 opt-out 而不是 opt-in（IncludeStatic），正是为了让「漏设」落在安全的一侧。
	NoStatic bool
	// StaticRoot 是解析 layout.files 相对路径的根目录；留空表示当前工作目录。
	// gear 从不持有静态资源，所以「它在哪」只能由调用方告诉。
	StaticRoot string
	// GearVersion 写入元信息的 gear 版本；留空用本包的 GearVersion。
	GearVersion string
}

// ExportResult 是导出的产出。
type ExportResult struct {
	AppID      string     `json:"app_id"`
	Path       string     `json:"path"`
	Format     string     `json:"format"`
	Components Components `json:"components"`
	// Files 是已拷入产物的静态资源（产物内相对路径）。
	Files []string `json:"files"`
	// Warnings 是「不影响导出成功，但产物不完整」的提示（例如声明的文件在磁盘上找不到）。
	Warnings []string `json:"warnings"`
	// StaticRoot 是本次解析静态资源**实际使用**的基准目录（清单里有静态声明时才有值）。
	// 让调用方看得见「gear 到底拿哪个目录去拼声明的路径」——这正是最容易出错、又最难猜的一环。
	StaticRoot string `json:"static_root,omitempty"`
	// StaticRootAuto 为 true 表示 StaticRoot 是自动向上查找得到的（调用方没显式指定）。
	StaticRootAuto bool `json:"static_root_auto,omitempty"`
}

// Export 把一个应用导出成可移植产物。
//
// 步骤：读 App 清单 → 反查组成 → 取回名下 Tool/Flow/Hook 定义 → 先写临时目录 →
// 再整体落到 Out（dir 直接改名、zip 打包）。**先 staging 后落地**是刻意的：
// 中途失败时不留下一个「看起来像产物」的半截目录。
func Export(reg *registry.Registry, o ExportOptions) (*ExportResult, error) {
	appID := strings.TrimSpace(o.AppID)
	if appID == "" {
		return nil, engine.Diagf("BAD_ARGUMENT", "app_id", "先用 gear_catalog(kind=app) 看有哪些应用", "app_id 不能为空")
	}
	out := strings.TrimSpace(o.Out)
	if out == "" {
		return nil, engine.Diagf("BAD_ARGUMENT", "out_dir", "给一个输出路径：dir 形态是目录，zip 形态是 .gearapp 文件", "out_dir 不能为空")
	}
	format := strings.ToLower(strings.TrimSpace(o.Format))
	if format == "" {
		format = "dir"
	}
	if format != "dir" && format != "zip" {
		return nil, engine.Diagf("BAD_ARGUMENT", "format", "只支持 dir 与 zip", "format 只能是 dir 或 zip（实际: %s）", o.Format)
	}

	a, synthNote, err := resolveExportApp(reg, appID)
	if err != nil {
		return nil, err
	}
	if err := a.Validate(); err != nil {
		return nil, engine.Diagf("ARTIFACT_INVALID", "app("+appID+")", "先修正清单再导出", "App 清单非法: %v", err)
	}

	comp := Composition(reg, a)
	// 取回定义：按 id 逐个读（registry 只按 id 索引），顺序按组成清单里的字典序，
	// 保证同样的库导出两次得到**逐字节相同**的产物（可 diff 的前提）。
	b := &Bundle{Apps: []*model.App{a}}
	for _, id := range comp.Tools {
		t, err := reg.GetTool(id)
		if err != nil {
			return nil, fmt.Errorf("读取 Tool %s 失败: %w", id, err)
		}
		if t == nil {
			return nil, engine.Diagf("NOT_FOUND", "tool("+id+")", "组成清单来自 registry 反查，读到空说明刚被删掉；重试一次", "Tool %s 在组成清单里但已不在 registry", id)
		}
		b.Tools = append(b.Tools, t)
	}
	for _, id := range comp.Flows {
		f, err := reg.GetFlow(id)
		if err != nil {
			return nil, fmt.Errorf("读取 Flow %s 失败: %w", id, err)
		}
		if f == nil {
			return nil, engine.Diagf("NOT_FOUND", "flow("+id+")", "组成清单来自 registry 反查，读到空说明刚被删掉；重试一次", "Flow %s 在组成清单里但已不在 registry", id)
		}
		b.Flows = append(b.Flows, f)
	}
	for _, id := range comp.Hooks {
		h, err := reg.GetHook(id)
		if err != nil {
			return nil, fmt.Errorf("读取 Hook %s 失败: %w", id, err)
		}
		if h == nil {
			return nil, engine.Diagf("NOT_FOUND", "hook("+id+")", "组成清单来自 registry 反查，读到空说明刚被删掉；重试一次", "Hook %s 在组成清单里但已不在 registry", id)
		}
		b.Hooks = append(b.Hooks, h)
	}

	res := &ExportResult{
		AppID: appID, Format: format, Components: Components{},
		Files: []string{}, Warnings: []string{},
	}
	// 走了「按 Flow 合成清单」那条路就说出来：产物能用，但清单是推断的，用户有权知道。
	if synthNote != "" {
		res.Warnings = append(res.Warnings, synthNote)
	}

	// ---- 静态资源清单（展开目录）----
	// layout.files 支持 `dir: true` 单项声明「整个目录递归拷贝」，避免把带 hash 的
	// 构建产物（dist/assets/*.js）逐个列名——那种写法一旦重新构建就全失效。
	// 展开后的实际文件写入 comp.StaticFiles，保证导出的 meta 与落地一致
	// （import 端按 meta 逐文件落地，列目录会让它拿不到文件）。
	//
	// 基准目录（StaticRoot）：显式传的照用；没传则**从 cwd 逐级向上找**能解析声明路径的
	// 祖先目录（见 findStaticRoot）——用户常在仓库子目录（build/）里跑 gear，而死守 cwd
	// 会把 build/dist 拼成 build/build/dist。绝对路径的声明与基准目录无关，原样用。
	root := strings.TrimSpace(o.StaticRoot)
	autoRoot := root == ""
	if autoRoot {
		root = findStaticRoot(a.Layout.Files)
	}
	if len(a.Layout.Files) > 0 {
		res.StaticRoot, res.StaticRootAuto = root, autoRoot
	}
	entries := make([]staticEntry, 0, len(a.Layout.Files))
	for _, f := range a.Layout.Files {
		rel := staticRelPath(a, f.Path)
		src := staticSrc(root, f.Path)
		if f.Dir {
			sub, err := expandDir(src, rel)
			if err != nil {
				// 目录项缺失：仍把声明路径记进组成清单（meta 要反映意图），
				// 但产物里不会有内容，按 required 给不同级别的警告。
				comp.StaticFiles = append(comp.StaticFiles, rel)
				level := "warning"
				if f.Required {
					level = "**required**"
				}
				res.Warnings = append(res.Warnings, fmt.Sprintf(
					"静态资源目录 %s（%s）在静态根 %s 下找不到，产物里不会有它："+
						"gear 不持有静态资源，只按 layout.files 声明的路径去拷；"+
						"用 --root 指向它所在的目录再导出", f.Path, level, root))
				continue
			}
			entries = append(entries, sub...)
			continue
		}
		entries = append(entries, staticEntry{rel: rel, src: src, required: f.Required})
	}
	// 组成清单的静态文件列表用**展开后的实际文件**，保证导出的 meta 与落地一致。
	comp.StaticFiles = comp.StaticFiles[:0]
	for _, e := range entries {
		comp.StaticFiles = append(comp.StaticFiles, e.rel)
	}
	sort.Strings(comp.StaticFiles)

	// ---- staging ----
	stage, err := os.MkdirTemp("", "gear-export-")
	if err != nil {
		return nil, fmt.Errorf("创建临时目录失败: %w", err)
	}
	defer os.RemoveAll(stage)

	gv := strings.TrimSpace(o.GearVersion)
	if gv == "" {
		gv = GearVersion
	}
	if err := writeArtifactFiles(stage, a, b, comp, gv); err != nil {
		return nil, err
	}
	if !o.NoStatic {
		for _, e := range entries {
			if _, err := os.Stat(e.src); err != nil {
				level := "warning"
				if e.required {
					level = "**required**"
				}
				res.Warnings = append(res.Warnings, fmt.Sprintf(
					"静态资源 %s（%s）在磁盘上找不到，产物里不会有它："+
						"gear 不持有静态资源，只按 layout.files 声明的路径去拷；"+
						"用 --root 指向它所在的目录再导出", e.src, level))
				continue
			}
			if err := copyFile(e.src, filepath.Join(stage, filepath.FromSlash(e.rel))); err != nil {
				return nil, fmt.Errorf("拷贝静态资源 %s 失败: %w", e.src, err)
			}
			res.Files = append(res.Files, e.rel)
		}
		sort.Strings(res.Files)
	} else if len(entries) > 0 {
		res.Warnings = append(res.Warnings,
			"仅导出定义（no_static）：，静态资源未拷入产物，这个产物不能当交付物用")
	}

	// ---- 落地 ----
	dst := out
	if format == "zip" {
		if filepath.Ext(dst) == "" {
			dst += ZipExt
		}
		if err := zipDir(stage, dst); err != nil {
			return nil, fmt.Errorf("打包产物失败: %w", err)
		}
	} else {
		if err := ensureDirTarget(dst); err != nil {
			return nil, err
		}
		// 目标为空目录时 Rename 会失败（非空），先删空目录再改名。
		_ = os.Remove(dst)
		if err := os.Rename(stage, dst); err != nil {
			return nil, fmt.Errorf("落地产物目录失败: %w", err)
		}
	}
	abs, _ := filepath.Abs(dst)
	res.Path = abs
	// 在末尾再回填组成清单：Components 含展开后的静态文件列表，且不能在 res 创建时
	// 用 `*comp` 浅拷贝（slice 头会共享底层数组，前面 staticRelPath 的展开会污染它）。
	res.Components = *comp
	return res, nil
}

// writeArtifactFiles 把三份文件写进 staging 目录。
func writeArtifactFiles(dir string, a *model.App, b *Bundle, comp *Components, gearVersion string) error {
	appDoc, err := MarshalApp(a)
	if err != nil {
		return fmt.Errorf("渲染 App 清单失败: %w", err)
	}
	bundleDoc, err := MarshalBundle(b)
	if err != nil {
		return fmt.Errorf("渲染定义失败: %w", err)
	}
	meta := &Meta{
		Kind:        ArtifactKind,
		Version:     FormatVersion,
		AppID:       a.ID,
		GearVersion: gearVersion,
		ExportedAt:  time.Now().Format(time.RFC3339),
		Components:  *comp,
	}
	metaDoc, err := marshalDoc(ArtifactKind, meta)
	if err != nil {
		return fmt.Errorf("渲染产物元信息失败: %w", err)
	}
	for name, doc := range map[string][]byte{
		MetaFile: metaDoc, AppFile: appDoc, BundleFile: bundleDoc,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), doc, 0o644); err != nil {
			return fmt.Errorf("写 %s 失败: %w", name, err)
		}
	}
	return nil
}

// ensureDirTarget 保证 dir 形态的目标「不存在或为空目录」。
//
// 为什么不直接覆盖：**导出目录里可能有用户自己放的东西**。为一个可重新生成的产物
// 去删一个未知目录，代价与收益完全不对称。zip 形态覆盖单个文件则允许（文件是明确定价的）。
func ensureDirTarget(dst string) error {
	entries, err := os.ReadDir(dst)
	if err == nil {
		if len(entries) > 0 {
			return engine.Diagf("BAD_ARGUMENT", "out_dir",
				"换一个空目录，或先把目标目录清空；gear 不会为一个可重新生成的产物删掉未知目录",
				"目标目录 %s 已存在且非空（%d 项），拒绝覆盖", dst, len(entries))
		}
		return nil
	}
	if !os.IsNotExist(err) {
		return fmt.Errorf("检查目标目录 %s 失败: %w", dst, err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("创建目标目录失败: %w", err)
	}
	return nil
}

// findStaticRoot 在**调用方没显式指定基准目录**时，为声明的静态路径挑一个根。
//
// ## 为什么不是简单的 cwd（2026-10-10 实测）
//
// 用户常在仓库的子目录里跑 gear（典型：`build>`），而 layout.files 的路径是相对**仓库根**
// 写的（`build/dist`、`apps/wms/www`）。若死守 cwd，`build> gear export web_ui` 会把
// `build/dist` 拼成 `build/build/dist`，报「找不到」——而这几乎总是用户想要的导出。
// 于是从 cwd 起**逐级向上**找，取第一个能让任一「相对」声明路径存在的祖先目录；
// 一个都找不到就退回 cwd，让警告如实报出实际用的根（参数问题仍需用户用 --root 明说）。
//
// 只看相对路径：绝对路径的解析与基准目录无关（见 staticSrc）。都不存在时退回 cwd。
func findStaticRoot(files []model.AppFile) string {
	cwd, err := os.Getwd()
	if err != nil {
		return "."
	}
	rels := make([]string, 0, len(files))
	for _, f := range files {
		p := strings.TrimSpace(f.Path)
		if p == "" || filepath.IsAbs(p) {
			continue
		}
		rels = append(rels, filepath.FromSlash(p))
	}
	if len(rels) == 0 {
		return cwd
	}
	for dir := cwd; ; {
		for _, rel := range rels {
			if _, err := os.Stat(filepath.Join(dir, rel)); err == nil {
				return dir
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return cwd
}

// staticSrc 解析一份静态资源在磁盘上的位置：绝对路径原样用（与基准目录无关），
// 相对路径挂到 root 下。
func staticSrc(root, declared string) string {
	p := filepath.FromSlash(strings.TrimSpace(declared))
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(root, p)
}

// staticEntry 是导出时「一个待拷的静态资源项」：rel=产物内相对路径（已剥 workdir），
// src=磁盘源路径，required=缺失是否阻断。目录项展开后每个文件都是一个 staticEntry。
type staticEntry struct {
	rel      string
	src      string
	required bool
}

// expandDir 把一个目录展开成「逐文件」的 staticEntry 列表。
//
// rel 是目录在产物内的相对路径（已剥 workdir 前缀），其下的每个文件保持相对结构
// （dist → dist/index.html、dist/assets/x.js）。源路径不存在或不是目录则返回错误，
// 由调用方转成警告（目录项缺失不应让导出失败）。
func expandDir(src, rel string) ([]staticEntry, error) {
	info, err := os.Stat(src)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s 不是目录", src)
	}
	var out []staticEntry
	err = filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		sub := strings.TrimPrefix(filepath.ToSlash(p[len(src):]), "/")
		out = append(out, staticEntry{
			rel:      filepath.ToSlash(filepath.Join(rel, sub)),
			src:      p,
			required: true,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// copyFile 拷贝单个文件（自动建目标目录）。
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// zipDir 把目录打包成 zip（路径用斜杠，便于跨平台解开）。
func zipDir(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	err = filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		w, err := zw.Create(filepath.ToSlash(rel))
		if err != nil {
			return err
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		_, err = io.Copy(w, in)
		return err
	})
	if err != nil {
		zw.Close()
		return err
	}
	return zw.Close()
}

// unzip 解开 zip 到目标目录（含 zip-slip 守卫）。
func unzip(src, dst string) error {
	r, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer r.Close()
	root := filepath.Clean(dst)
	for _, f := range r.File {
		rel := filepath.FromSlash(f.Name)
		target := filepath.Join(root, rel)
		// 守卫：条目逃出目标目录即拒（恶意产物或手写 zip 都可能踩到）。
		if target != root && !strings.HasPrefix(target, root+string(os.PathSeparator)) {
			return fmt.Errorf("产物包含越界路径 %q，拒绝解开", f.Name)
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		in, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.Create(target)
		if err != nil {
			in.Close()
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			in.Close()
			out.Close()
			return err
		}
		in.Close()
		out.Close()
	}
	return nil
}

// ---- 导入 ----

// ImportOptions 是导入的入参。
type ImportOptions struct {
	// Source 是产物路径（目录，或 .gearapp / zip 文件）。
	Source string
	// Overwrite 允许覆盖「id 已存在且内容不同」的对象（默认 false，与 gear_apply 同款）。
	Overwrite bool
	// DryRun 只汇报将创建/更新什么，一个字节不写。
	DryRun bool
	// AppsDir 是静态资源的落地根目录；**留空表示不落地**（只登记清单 + 校验路径存在性）。
	AppsDir string
	// DB 是执行 bundle 里 `migrations:` 用的库实现（*cap.MultiDB）。
	// 产物没声明迁移时它**不被读取**（可传 nil）；声明了却传 nil → fail-closed 拒绝。
	DB Migrator
}

// ImportResult 是导入的产出。
type ImportResult struct {
	AppID   string   `json:"app_id"`
	DryRun  bool     `json:"dry_run"`
	Created []string `json:"created"`
	Updated []string `json:"updated"`
	// MigrationsApplied 是本次走完执行流程的迁移版本号（按升序）。
	// dry run 下它表示「会跑、但整包已回滚」——文案必须照这个口径写（CLI 里就分了两行）。
	MigrationsApplied []int `json:"migrations_applied"`
	// StaticLanded 是已落地的静态资源（相对产物内的路径）。
	StaticLanded []string `json:"static_landed"`
	// StaticMissing 是组成清单声明了、产物里却没有的静态资源。
	StaticMissing []string `json:"static_missing"`
	Warnings      []string `json:"warnings"`
}

// Load 读出一个产物（目录或 zip）的内容。
//
// 返回值的 Dir 是内容所在目录；zip 会先解到临时目录，调用方须在完成后调 Cleanup。
type Loaded struct {
	Meta    *Meta
	Bundle  *Bundle
	Dir     string
	tempDir string
}

// Cleanup 清理 Load 为 zip 解开的临时目录（目录形态是 no-op）。
func (l *Loaded) Cleanup() {
	if l == nil || l.tempDir == "" {
		return
	}
	_ = os.RemoveAll(l.tempDir)
}

// Load 解析产物：校验格式版本 → 读 App 清单 → 读定义。
func Load(source string) (*Loaded, error) {
	src := strings.TrimSpace(source)
	if src == "" {
		return nil, engine.Diagf("BAD_ARGUMENT", "source", "给产物目录或 .gearapp 文件路径", "source 不能为空")
	}
	info, err := os.Stat(src)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, engine.Diagf("NOT_FOUND", "source", "确认路径拼写（相对路径按当前工作目录解析）", "产物路径不存在: %s", src)
		}
		return nil, fmt.Errorf("访问产物 %s 失败: %w", src, err)
	}
	dir := src
	var temp string
	if !info.IsDir() {
		temp, err = os.MkdirTemp("", "gear-import-")
		if err != nil {
			return nil, fmt.Errorf("创建临时目录失败: %w", err)
		}
		if err := unzip(src, temp); err != nil {
			_ = os.RemoveAll(temp)
			return nil, engine.Diagf("ARTIFACT_INVALID", "source", "确认它是 gear export --format=zip 产出的包", "解开产物失败（它是不是 zip？）: %v", err)
		}
		dir = temp
	}
	l := &Loaded{Dir: dir, tempDir: temp}

	metaPath := filepath.Join(dir, MetaFile)
	raw, err := os.ReadFile(metaPath)
	if err != nil {
		l.Cleanup()
		return nil, engine.Diagf("ARTIFACT_INVALID", MetaFile,
			"目录里应有 gear-artifact.yaml / app.yaml / bundle.yaml 三份文件；确认传的是产物根目录",
			"读 %s 失败: %v", MetaFile, err)
	}
	var probe map[string]any
	if err := yaml.Unmarshal(raw, &probe); err == nil {
		if k, _ := probe["kind"].(string); !strings.EqualFold(strings.TrimSpace(k), ArtifactKind) {
			l.Cleanup()
			return nil, engine.Diagf("ARTIFACT_INVALID", MetaFile+".kind",
				"目录里应是 gear export 产出的产物（含 gear-artifact.yaml / app.yaml / bundle.yaml）",
				"%s 的 kind 是 %q（应为 %s）", MetaFile, k, ArtifactKind)
		}
	}
	var meta Meta
	if err := yaml.Unmarshal(raw, &meta); err != nil {
		l.Cleanup()
		return nil, engine.Diagf("ARTIFACT_INVALID", MetaFile, "产物元信息损坏，重新导出一份", "解析 %s 失败: %v", MetaFile, err)
	}
	meta.Kind = ArtifactKind
	if meta.Version != FormatVersion {
		l.Cleanup()
		return nil, engine.Diagf("ARTIFACT_VERSION_UNSUPPORTED", MetaFile+".version",
			fmt.Sprintf("本 gear 只认格式 v%d；用同版本 gear 重新导出，或升级 gear 再导入", FormatVersion),
			"产物格式版本 %d 不被支持（本 gear 支持 v%d）", meta.Version, FormatVersion)
	}
	l.Meta = &meta

	text, err := readBundleText(dir)
	if err != nil {
		l.Cleanup()
		return nil, err
	}
	b, diags := ParseBundle(text)
	if len(diags) > 0 {
		l.Cleanup()
		return nil, engine.Diagf("ARTIFACT_INVALID", BundleFile,
			"产物里的定义解析不过，说明导出与导入的 gear 版本差异较大；重新导出一份",
			"解析产物定义失败: %s", diags[0].Message)
	}
	l.Bundle = b
	return l, nil
}

// readBundleText 读出「App 清单 + 定义」的合并文本（ParseBundle 吃多文档 YAML）。
func readBundleText(dir string) (string, error) {
	var parts []string
	for _, name := range []string{AppFile, BundleFile} {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return "", engine.Diagf("ARTIFACT_INVALID", name,
				"目录里应有 gear-artifact.yaml / app.yaml / bundle.yaml 三份文件", "读 %s 失败: %v", name, err)
		}
		parts = append(parts, string(raw))
	}
	return strings.Join(parts, "\n---\n"), nil
}

// Import 把一个产物导进 registry。
//
// 顺序与 gear_apply 完全一致（App → Tool → Flow → Hook），冲突语义也一致
// （未传 overwrite 且「已存在但内容不同」→ 整批拒绝，原子）。
func Import(reg *registry.Registry, o ImportOptions) (*ImportResult, error) {
	l, err := Load(o.Source)
	if err != nil {
		return nil, err
	}
	defer l.Cleanup()

	b := l.Bundle
	if len(b.Apps) != 1 {
		return nil, engine.Diagf("ARTIFACT_INVALID", AppFile,
			"一个产物描述一个应用；重新导出一份", "产物里有 %d 份 App 清单（应为 1 份）", len(b.Apps))
	}
	a := b.Apps[0]
	res := &ImportResult{
		AppID: a.ID, DryRun: o.DryRun,
		Created: []string{}, Updated: []string{}, MigrationsApplied: []int{},
		StaticLanded: []string{}, StaticMissing: []string{}, Warnings: []string{},
	}

	if diags := validateImported(reg, b); len(diags) > 0 {
		return nil, engine.Diagf("ARTIFACT_INVALID", diags[0].Field, diags[0].Hint, "%s", diags[0].Message)
	}

	// 静态资源：默认不落地（gear 不持有静态资源），传了 AppsDir 才拷。
	// 落地时**重写** manifest 的 layout 指向实际位置——否则导进来的应用指向的路径
	// 在这台机器上根本不存在，gear_app(start) 会一头撞在「静态资源缺失」上。
	appsDir := strings.TrimSpace(o.AppsDir)
	for _, rel := range l.Meta.Components.StaticFiles {
		src := filepath.Join(l.Dir, filepath.FromSlash(rel))
		info, err := os.Stat(src)
		if err != nil {
			res.StaticMissing = append(res.StaticMissing, rel)
			continue
		}
		if !info.IsDir() {
			// 单文件：直接落地。
			if appsDir == "" {
				continue
			}
			dst := filepath.Join(appsDir, a.ID, filepath.FromSlash(rel))
			if !o.DryRun {
				if err := copyFile(src, dst); err != nil {
					return nil, fmt.Errorf("落地静态资源 %s 失败: %w", rel, err)
				}
			}
			res.StaticLanded = append(res.StaticLanded, rel)
			continue
		}
		// 目录项：递归落地每个文件，rel 展开成「目录/子路径」，与导出端一致。
		if appsDir == "" {
			continue
		}
		err = filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			sub := strings.TrimPrefix(filepath.ToSlash(p[len(src):]), "/")
			frel := filepath.ToSlash(filepath.Join(rel, sub))
			dst := filepath.Join(appsDir, a.ID, filepath.FromSlash(frel))
			if !o.DryRun {
				if err := copyFile(p, dst); err != nil {
					return fmt.Errorf("落地静态资源 %s 失败: %w", frel, err)
				}
			}
			res.StaticLanded = append(res.StaticLanded, frel)
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("落地静态资源目录 %s 失败: %w", rel, err)
		}
	}
	if appsDir != "" && len(res.StaticLanded) > 0 {
		wd := filepath.ToSlash(filepath.Join(appsDir, a.ID))
		a.Layout.Workdir = wd
		files := make([]model.AppFile, 0, len(res.StaticLanded))
		for _, rel := range res.StaticLanded {
			files = append(files, model.AppFile{Path: wd + "/" + rel, Required: true})
		}
		a.Layout.Files = files
	} else if appsDir == "" && len(l.Meta.Components.StaticFiles) > 0 {
		res.Warnings = append(res.Warnings,
			"未落地静态资源（未传 apps_dir）：gear 不持有静态资源，清单里的路径需你自己用文件工具摆好；"+
				"要一键还原请传 apps_dir")
	}
	if len(res.StaticMissing) > 0 {
		res.Warnings = append(res.Warnings, fmt.Sprintf(
			"组成清单声明了但产物里没有的静态资源：%s（导出时它们就不在磁盘上）",
			strings.Join(res.StaticMissing, ", ")))
	}

	// 迁移与定义同一个原子动作：dry run 走**真跑 + 全回滚**（SQL 错因此能在 dry run 里被抓出来），
	// 但 dry run 不会把版本号写进台账——「跑了却没记」的事绝不能谎报成「已应用」。
	applied, err := ApplyMigrations(o.DB, b, o.DryRun)
	if err != nil {
		return nil, err
	}
	res.MigrationsApplied = applied

	if o.DryRun {
		res.Created, res.Updated = DiffExisting(reg, b, false), DiffExisting(reg, b, true)
		return res, nil
	}
	created, updated, _, err := ApplyBundle(reg, b, o.DB, o.Overwrite)
	if err != nil {
		return nil, err
	}
	res.Created, res.Updated = created, updated
	return res, nil
}

// validateImported 对导入的定义做最小校验（不依赖 bundle 之外的上下文）。
//
// 为什么不用 mcp 那套 validateBundle：它需要 engine（校验 Flow 的静态引用），而本包
// 刻意不依赖 engine 的执行面——导入只是「把定义放进库」，跑不跑得起来由
// gear_validate / gear_app 在该机器上重新判定（那里的环境才是权威）。
// 这里只挡「明显不是合法定义」的东西，避免把坏数据写进库。
func validateImported(reg *registry.Registry, b *Bundle) []engine.Diagnostic {
	var out []engine.Diagnostic
	for _, t := range b.Tools {
		if err := t.Validate(); err != nil {
			out = append(out, engine.Diagnostic{
				Code: engine.CodeBadToolStructure, Field: "tool(" + t.ID + ")",
				Message: fmt.Sprintf("Tool %s: %v", t.ID, err),
				Hint:    "产物里的定义非法，重新导出一份",
			})
		}
	}
	// 声明式 REST 路由（FR-3）：与 gear_validate 同一份规则，导入时也要挡掉
	// 「同一 method+path 被两个 Tool 声明」这类会让人起服时 panic 的写法。
	out = append(out, CheckToolRoutes(b.Tools)...)
	for _, h := range b.Hooks {
		if err := h.Validate(); err != nil {
			out = append(out, engine.Diagnostic{
				Code: engine.CodeBadHookStructure, Field: "hook(" + h.ID + ")",
				Message: fmt.Sprintf("Hook %s: %v", h.ID, err),
				Hint:    "产物里的定义非法，重新导出一份",
			})
		}
	}
	for _, f := range b.Flows {
		if err := f.Validate(); err != nil {
			out = append(out, engine.Diagnostic{
				Code: engine.CodeBadFlowStructure, Field: "flow(" + f.ID + ")",
				Message: fmt.Sprintf("Flow %s: %v", f.ID, err),
				Hint:    "产物里的定义非法，重新导出一份",
			})
		}
	}
	a := b.Apps[0]
	if a.EntryFlow != "" {
		found := false
		for _, f := range b.Flows {
			if f.ID == a.EntryFlow {
				found = true
				break
			}
		}
		if !found {
			if reg != nil {
				if f, err := reg.GetFlow(a.EntryFlow); err == nil && f != nil {
					found = true
				}
			}
		}
		if !found {
			out = append(out, engine.Diagnostic{
				Code: engine.CodeUnknownCallTarget, Field: "kind: App.entry_flow",
				Message: fmt.Sprintf("entry_flow %s 既不在产物里，也不是本 registry 已注册的 Flow", a.EntryFlow),
				Hint:    "先导入它依赖的 Flow，或改 entry_flow",
			})
		}
	}
	return out
}

// 本包的诊断一律走 engine.Diagf（code/field/hint 三件套），**不**自造一个短名包装：
// tools/doc_audit.py 按 `Diagf("` / `toolError("` 这些**调用形态**反查「代码实际产生的错误码」，
// 换成自造的短名会让新码从审计里消失——「手册收录了但代码不产生」正是它要抓的漂移。
