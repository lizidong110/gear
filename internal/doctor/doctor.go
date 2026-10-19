// Package doctor 是「环境自检」的唯一实现。
//
// 起因（2026-10-03 外部实测）：自检此前**只有 MCP 侧**（gear_doctor），CLI 侧没有。
// 于是从命令行排查的人看不到它，而从 MCP 排查的人拿到的结论无法回到命令行复核——
// 两面各写一份又会漂移（同一个坑 gear 已经在 procalive 上吃过一次）。
//
// 所以这里是**一份实现、两个消费方**：
//   - MCP：gear_doctor → Report.Map()（JSON 形状与旧版逐字一致，不破坏既有调用方与测试）
//   - CLI：gear doctor → Report.Text()（人读文本；同一份判据，不另写一套检查）
//
// 它回答的始终是同一个问题：**起不来是 YAML 写错了，还是环境没准备好**（侧车没 build、
// 凭据没配、端口被占、台账有遗留进程）。没有它，使用者只会反复重写 YAML。
package doctor

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"gear/internal/app"
	"gear/internal/cap"
	"gear/internal/engine"
	"gear/internal/gearhome"
	"gear/internal/registry"
)

// Options 是自检的输入。零值不可用：Registry 与 Engine 必填（Check 会返回错误而不是崩）。
type Options struct {
	Registry *registry.Registry
	Engine   *engine.Engine
	// RegistryPath / AppsDir 决定「在操作哪个库、台账在哪」（见 gear.App 的路径解析）。
	RegistryPath string
	AppsDir      string
}

// ---- 报告结构（字段导出且带 json tag：MCP 侧要序列化给智能体解析，不能逼它正则匹配中文）----

// RegistryInfo 是注册库概览。
type RegistryInfo struct {
	Path  string `json:"path"`
	Tools int    `json:"tools"`
	Flows int    `json:"flows"`
	Hooks int    `json:"hooks"`
}

// ExtensionsInfo 是扩展声明与实际加载的对照。
type ExtensionsInfo struct {
	Config          string         `json:"config"`
	Declared        []string       `json:"declared"`
	Loaded          []string       `json:"loaded"`
	MissingSidecars []SidecarIssue `json:"missing_sidecars"`
	CapsCheck       []CapsIssue    `json:"caps_check"`
}

// AppInfo 是一个应用台账项的摘要。
//
// Ports 用 app.PortState 原类型而不是 []int：MCP 侧的 gear_doctor 历来把端口序列化成
// `[{port,listening}]`，换成 []int 会**静默改掉既有响应形状**（MCP_BASELINE 口径）。
type AppInfo struct {
	AppID   string          `json:"app_id"`
	FlowID  string          `json:"flow_id,omitempty"`
	State   string          `json:"state"`
	PID     int             `json:"pid,omitempty"`
	Ports   []app.PortState `json:"ports,omitempty"`
	LogPath string          `json:"log_path,omitempty"`
}

// OrphanInfo 是台账里有、但进程/端口对不上的遗留应用。
type OrphanInfo struct {
	AppID   string `json:"app_id"`
	Reason  string `json:"reason"`
	PID     int    `json:"pid,omitempty"`
	LogPath string `json:"log_path,omitempty"`
}

// Report 是一次自检的完整结论。Warnings 是**给人看**的汇总，结构化字段供机器读。
type Report struct {
	Registry   RegistryInfo      `json:"registry"`
	Extensions ExtensionsInfo    `json:"extensions"`
	AI         map[string]any    `json:"ai"`
	Ports      map[string]string `json:"ports"`
	Apps       []AppInfo         `json:"apps"`
	Orphan     []OrphanInfo      `json:"orphan"`
	AppsDir    string            `json:"apps_dir"`
	Warnings   []string          `json:"warnings"`
}

// Check 跑一遍自检。
//
// 失败只可能来自「读不到库」——那属于环境坏了，如实返回错误让调用方报 DOCTOR_FAILED；
// 其余一切（缺侧车、凭据没配、端口被占、台账遗留）都进 Warnings，不阻断。
func Check(opts Options) (Report, error) {
	var rep Report
	if opts.Registry == nil {
		return rep, fmt.Errorf("doctor 需要一个已打开的 registry")
	}
	reg := opts.Registry

	tools, err := reg.ListTools()
	if err != nil {
		return rep, err
	}
	flows, err := reg.ListFlows()
	if err != nil {
		return rep, err
	}
	hooks, err := reg.ListHooks()
	if err != nil {
		return rep, err
	}
	rep.Registry = RegistryInfo{Path: opts.RegistryPath, Tools: len(tools), Flows: len(flows), Hooks: len(hooks)}

	warnings := []string{}

	// 侧车就绪：判据是「加载了的 ext.* 在产物目录里有没有对应二进制」。
	loaded := []string{}
	if opts.Engine != nil {
		if agent := opts.Engine.AIAgent(); agent != nil {
			if capsList, err := agent.CapabilitiesMethod(); err == nil {
				if list, ok := capsList.([]map[string]any); ok {
					for _, c := range list {
						if n, _ := c["name"].(string); strings.HasPrefix(n, "ext.") {
							loaded = append(loaded, n)
						}
					}
				}
			}
		}
	}
	sort.Strings(loaded)
	extCfgPath := ExtensionConfigPath()
	rep.Extensions = ExtensionsInfo{
		Config:          extCfgPath,
		Declared:        ExtensionDeclared(extCfgPath),
		Loaded:          loaded,
		MissingSidecars: MissingSidecars(loaded),
		CapsCheck:       checkDeclaredVsImplemented(extCfgPath, &warnings),
	}
	for _, m := range rep.Extensions.MissingSidecars {
		warnings = append(warnings, fmt.Sprintf(
			"能力 %s 可用但未找到对应侧车二进制 %s（已找过：%s）—— 若用到该能力，先构建（scripts/build.sh）",
			m.Ext, m.Expected, strings.Join(m.Searched, "; ")))
	}

	// 应用台账：报**解析后**的目录（入参里的 appsDir 可能是空串，给人一个空路径等于让他自己猜）。
	resolvedAppsDir := opts.AppsDir
	if d, derr := app.ResolveAppsDir(opts.RegistryPath, opts.AppsDir); derr == nil {
		resolvedAppsDir = d
	}
	rep.AppsDir = resolvedAppsDir
	rep.Ports = map[string]string{}
	if list, listErr := app.List(opts.RegistryPath, opts.AppsDir); listErr == nil {
		for _, it := range list.Items {
			rep.Apps = append(rep.Apps, AppInfo{
				AppID: it.AppID, FlowID: it.FlowID, State: it.State,
				PID: it.PID, Ports: it.Ports, LogPath: it.LogPath,
			})
			for _, p := range it.Ports {
				state := "free"
				if p.Listening {
					state = fmt.Sprintf("in-use(pid=%d)", it.PID)
				}
				rep.Ports[fmt.Sprint(p.Port)] = state
			}
		}
		for _, o := range list.Orphan {
			rep.Orphan = append(rep.Orphan, OrphanInfo{
				AppID: o.AppID, Reason: o.Reason, PID: o.PID, LogPath: o.LogPath,
			})
			warnings = append(warnings, fmt.Sprintf(
				"台账里有遗留应用 %s（%s，pid=%d）——已如实汇报，未自动清理；确认后可 gear_app(action=stop, app_id=%s)",
				o.AppID, o.Reason, o.PID, o.AppID))
		}
	} else {
		warnings = append(warnings, "读取应用台账失败: "+listErr.Error())
	}

	rep.AI = AIReadiness()
	if ready, _ := rep.AI["ready"].(bool); !ready {
		warnings = append(warnings, fmt.Sprintf(
			"AI 能力未就绪（%s）：%v —— 修法：%v", rep.AI["provider"], rep.AI["blocked"], rep.AI["how_to_fix"]))
	}

	// 空切片而非 nil：JSON 里 [] 与 null 对智能体是两种含义（「没有」而不是「没查出来」）。
	if rep.Apps == nil {
		rep.Apps = []AppInfo{}
	}
	if rep.Orphan == nil {
		rep.Orphan = []OrphanInfo{}
	}
	rep.Warnings = warnings
	return rep, nil
}

// checkDeclaredVsImplemented 探测每个 service ext 的 /caps，比对声明与实现（见 CapsIssue）。
func checkDeclaredVsImplemented(extCfgPath string, warnings *[]string) []CapsIssue {
	out := []CapsIssue{}
	for _, spec := range ExtensionServiceSpecs(extCfgPath) {
		c := ProbeServiceCaps(spec)
		out = append(out, c)
		switch c.State {
		case "mismatch":
			*warnings = append(*warnings, fmt.Sprintf(
				"能力 %s 声明了方法 %v，但侧车 %s 的 /caps 未实现其中的 %v —— 调用会报「没有方法」；修侧车或改声明",
				c.Ext, c.Declared, c.Listen, c.Missing))
		case "probe_failed":
			*warnings = append(*warnings, fmt.Sprintf(
				"能力 %s 的侧车在 %s 已起，但 /caps 探测失败 —— service ext 必须实现 /caps（见 gear_guide(\"sidecar\") §3）",
				c.Ext, c.Listen))
		}
		// sidecar_not_running 不告警：service ext 只在应用运行时才被拉起，
		// doctor 常在应用没跑时被调用——报成错误会淹掉真问题。
	}
	return out
}

// ---- Map / Text：两个消费方的两种渲染 ----

// Map 返回与旧版 gear_doctor **逐字同形**的 map（既有调用方与测试都依赖这些字段名）。
func (r Report) Map() map[string]any {
	// 端口在旧版是 map[string]string，Apps/Orphan 是 []map[string]any。
	// 这里从结构体重建同样形状，而不是直接 Marshal Report：
	// 结构体是 CLI 的形状，Map 是 MCP 的形状，两者各自演化但判据同源。
	apps := make([]map[string]any, 0, len(r.Apps))
	for _, a := range r.Apps {
		m := map[string]any{
			"app_id": a.AppID, "flow_id": a.FlowID, "state": a.State, "ports": a.Ports,
		}
		if a.PID != 0 {
			m["pid"] = a.PID
		}
		if a.LogPath != "" {
			m["log_path"] = a.LogPath
		}
		apps = append(apps, m)
	}
	orphan := make([]map[string]any, 0, len(r.Orphan))
	for _, o := range r.Orphan {
		m := map[string]any{"app_id": o.AppID, "reason": o.Reason, "pid": o.PID, "log_path": o.LogPath}
		orphan = append(orphan, m)
	}
	warnings := r.Warnings
	if warnings == nil {
		warnings = []string{}
	}
	return map[string]any{
		"registry": map[string]any{
			"path": r.Registry.Path, "tools": r.Registry.Tools,
			"flows": r.Registry.Flows, "hooks": r.Registry.Hooks,
		},
		"extensions": map[string]any{
			"config":           r.Extensions.Config,
			"declared":         nilIfEmpty(r.Extensions.Declared),
			"loaded":           nilIfEmpty(r.Extensions.Loaded),
			"missing_sidecars": r.Extensions.MissingSidecars,
			// caps_check 是「声明 ↔ 实现」的实测结果（含 ok / 未运行 / 失败 全部状态，
			// 不只列不匹配的——只给异常清单会让人以为「没列出=已校验通过」）。
			"caps_check": r.Extensions.CapsCheck,
		},
		"ai":       r.AI,
		"ports":    r.Ports,
		"apps":     apps,
		"orphan":   orphan,
		"apps_dir": r.AppsDir,
		"warnings": warnings,
	}
}

// Text 渲染人读文本（`gear doctor` 的输出）。
//
// 顺序按「最可能让人白跑一趟的事」排：凭据 → 侧车 → 端口 → 台账遗留。
func (r Report) Text() string {
	var b strings.Builder
	ai := "未就绪"
	if ready, _ := r.AI["ready"].(bool); ready {
		ai = fmt.Sprintf("就绪（provider=%v，key 来自%s）", r.AI["provider"], keySource(r.AI))
	}
	fmt.Fprintf(&b, "registry    %s  tools=%d flows=%d hooks=%d\n",
		r.Registry.Path, r.Registry.Tools, r.Registry.Flows, r.Registry.Hooks)
	fmt.Fprintf(&b, "扩展配置    %s（声明 %s，加载 %s）\n",
		orNone(r.Extensions.Config), plural(len(r.Extensions.Declared)), plural(len(r.Extensions.Loaded)))
	fmt.Fprintf(&b, "AI          %s\n", ai)
	fmt.Fprintf(&b, "应用台账    %s（%d 个应用，%d 个遗留）\n",
		orNone(r.AppsDir), len(r.Apps), len(r.Orphan))
	if len(r.Ports) > 0 {
		ports := make([]string, 0, len(r.Ports))
		for p, st := range r.Ports {
			ports = append(ports, p+"="+st)
		}
		sort.Strings(ports)
		fmt.Fprintf(&b, "端口        %s\n", strings.Join(ports, "  "))
	}
	for _, m := range r.Extensions.MissingSidecars {
		fmt.Fprintf(&b, "缺侧车      %s → 期望 %s（找过：%s）\n",
			m.Ext, m.Expected, strings.Join(m.Searched, "; "))
	}
	for _, o := range r.Orphan {
		fmt.Fprintf(&b, "遗留应用    %s（%s，pid=%d）\n", o.AppID, o.Reason, o.PID)
	}
	if len(r.Warnings) == 0 {
		b.WriteString("\n无告警：环境就绪。\n")
		return b.String()
	}
	b.WriteString("\n告警\n")
	for _, w := range r.Warnings {
		fmt.Fprintf(&b, "  - %s\n", w)
	}
	return b.String()
}

func keySource(ai map[string]any) string {
	if s, _ := ai["key_from_config"].(bool); s {
		return "配置库"
	}
	if s, _ := ai["key_from_env"].(bool); s {
		return "环境变量"
	}
	return "provider=mock，无需 key"
}

func plural(n int) string { return fmt.Sprintf("%d", n) }

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "（未找到）"
	}
	return s
}

// nilIfEmpty 让空清单序列化成 null 时仍是「空」的语义：旧版对空切片给的是 nil，
// 这里保持一致，避免 CLI/MCP 两面对同一状态的描述不同。
func nilIfEmpty(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	return s
}

// ---- 侧车产物 ----

// SidecarIssue 是一条「声明了某扩展、但对应侧车二进制不存在」的记录。
//
// ⚠ 字段必须**导出且带 json tag**：本结构体被直接序列化进响应，
// 而未导出字段 Marshal 出来是 `{}`——2026-09-22 实测响应里 `missing_sidecars` 是
// `[{},{},{}]`，真正的内容只藏在 warnings 的文本里，机器读不到。
type SidecarIssue struct {
	Ext      string `json:"ext"`
	Expected string `json:"expected"`
	// Searched 是**实际找过的目录**，随告警一并报出：只说「没找到」会让人去猜
	// 「是不是没编译」，而他真正需要知道的是「我在这些位置找过」。
	Searched []string `json:"searched"`
}

// MissingSidecars 按产物命名约定检查各 ext.* 的侧车是否就绪。
//
// 约定：`ext.<name>` → `<name>-sidecar(.exe)`，落点由 gearhome.ArtifactDirs() 给出
// （**以可执行文件目录为主锚点，不依赖 cwd** —— 客户端以任意 cwd 派生 `gear mcp` 时，
// 此前只看 `./build/plugin` 会把一台侧车齐全的机器误报成缺侧车）。
// 只对在产物目录里找不到的报缺——检查不到就**报缺**而不是假装它就绪。
//
// 注意：不是每个 ext 都需要侧车（`ext.ai` 是进程内实现），所以这里只报「找到了命名约定但
// 文件不在」的情况，不做「少了谁」的穷举——穷举必然误报。
func MissingSidecars(loaded []string) []SidecarIssue {
	return missingSidecarsIn(gearhome.ArtifactDirs(), loaded)
}

// missingSidecarsIn 是 MissingSidecars 的纯函数内核（产物目录由参数给定，便于单测：
// ArtifactDirs 锚在可执行文件目录上，测试进程里那个目录是 go 的临时目录，
// 测不到「确实存在 *-sidecar 产物 ⇒ 才判缺」这条分支）。判据与注释全在 MissingSidecars 上。
func missingSidecarsIn(dirs []string, loaded []string) []SidecarIssue {
	out := []SidecarIssue{}
	for _, name := range loaded {
		base := strings.TrimPrefix(name, "ext.")
		if base == "" || base == "ai" || base == "registry" {
			continue // 进程内扩展，无独立侧车
		}
		candidates := []string{}
		for _, dir := range dirs {
			candidates = append(candidates, filepath.Join(dir, base+"-sidecar"))
			candidates = append(candidates, filepath.Join(dir, base+"-sidecar.exe"))
		}
		found := false
		for _, c := range candidates {
			if st, err := os.Stat(c); err == nil && !st.IsDir() {
				found = true
				break
			}
		}
		if !found {
			// 只在**该 ext 确实有独立侧车**的情况下报缺：判据是产物目录里存在
			// 「其它」*-sidecar 产物（说明本项目确实用侧车形态），否则不猜。
			if len(hasAnySidecarArtifact(dirs)) > 0 {
				out = append(out, SidecarIssue{
					Ext:      name,
					Expected: filepath.Join(dirs[0], base+"-sidecar"),
					Searched: dirs,
				})
			}
		}
	}
	return out
}

// hasAnySidecarArtifact 返回「确实存在 *-sidecar 产物」的目录列表（空 = 本机走的是非侧车形态）。
func hasAnySidecarArtifact(dirs []string) []string {
	var out []string
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() && strings.Contains(e.Name(), "-sidecar") {
				out = append(out, dir)
				break
			}
		}
	}
	return out
}

// ---- 「声明 ↔ 实现」一致性校验（/caps 探测）----
//
// 存在理由：这条判据此前**只活在 plugin/verify.py**（一个 Python 测试脚本）里，gear 自身
// 与 MCP 都拿不到——于是「extensions.yaml 声明了某方法、侧车其实没实现」只能等运行期
// 调用才炸，而写完声明后**没有任何手段自验**。doctor 是唯一能在动手前说清这事的地方。
//
// 只校验 service ext：它长驻且按约定暴露 `/caps`，可直接探测。once 型侧车要起进程才能问、
// 且没有 /caps 约定，故不在范围内——**如实标注未校验，不假装校验过**（不静默降级）。

// ServiceExtSpec 是一条 service ext 声明的最小投影（只取校验需要的两个字段）。
type ServiceExtSpec struct {
	Name    string
	Listen  string
	Methods []string
}

// ExtensionServiceSpecs 读扩展声明文件里 service ext 的 listen 与 methods。
//
// 解析失败**返回空**：与 ExtensionDeclared 同款口径——配置文件的语法问题会在装配期以
// 更准确的方式暴露，doctor 的价值是「尽量说清现状」而不是再报一遍语法错。
func ExtensionServiceSpecs(path string) []ServiceExtSpec {
	if path == "" {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var cfg map[string]struct {
		Methods []string `yaml:"methods"`
		Service bool     `yaml:"service"`
		Listen  string   `yaml:"listen"`
	}
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return nil
	}
	out := []ServiceExtSpec{}
	for k, v := range cfg {
		if !strings.HasPrefix(k, "ext.") || !v.Service || v.Listen == "" {
			continue // once 型 / 未声明 listen 的，不在探测范围
		}
		out = append(out, ServiceExtSpec{Name: k, Listen: v.Listen, Methods: v.Methods})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// CapsIssue 是一条「声明的方法与侧车真实实现不一致」的记录。
//
// ⚠ 字段必须**导出且带 json tag**：理由同 SidecarIssue（2026-09-22 实测：未导出字段
// Marshal 出来是 `{}`，调用方只能去正则匹配中文告警才能拿到内容）。
type CapsIssue struct {
	Ext         string   `json:"ext"`
	Listen      string   `json:"listen"`
	State       string   `json:"state"` // ok / mismatch / sidecar_not_running / probe_failed
	Declared    []string `json:"declared"`
	Implemented []string `json:"implemented"`
	// Missing 是「声明了但侧车没实现」的方法；为空即一致。
	Missing []string `json:"missing"`
}

// capsProbeTimeout 是单次 /caps 探测的超时。doctor 是诊断工具，不该被一个没响应的
// 侧车挂住（侧车未运行时 connect 会立刻 refused，超时是给「连上了但不应答」兜底）。
const capsProbeTimeout = 2 * time.Second

// ProbeServiceCaps 探测一个 service ext 的 /caps，比对「声明的 methods ⊆ 实现的 methods」。
//
// 四种状态必须分清（不许合并成一句「校验失败」——那会让调用方无从下手）：
//   - sidecar_not_running：连不上。**不是错误**——service ext 只在应用运行时才被拉起，
//     doctor 常在应用没跑的时候被调用；报成错误会淹掉真问题。
//   - probe_failed：连上了但拿不到合法 /caps。**真问题**：service ext 必须实现 /caps。
//   - mismatch：拿到了，但声明的方法有缺。**真问题**：调用会报「没有方法」。
//   - ok：一致。
func ProbeServiceCaps(spec ServiceExtSpec) CapsIssue {
	issue := CapsIssue{Ext: spec.Name, Listen: spec.Listen, State: "sidecar_not_running"}
	client := &http.Client{Timeout: capsProbeTimeout}
	resp, err := client.Get("http://" + spec.Listen + "/caps")
	if err != nil {
		return issue // 连不上 = 侧车没起，不是配置错
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		issue.State = "probe_failed"
		return issue
	}
	var body struct {
		Result struct {
			Methods []string `json:"methods"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		issue.State = "probe_failed"
		return issue
	}
	issue.Implemented = body.Result.Methods
	issue.Declared = spec.Methods
	got := map[string]bool{}
	for _, m := range issue.Implemented {
		got[m] = true
	}
	for _, m := range spec.Methods {
		if !got[m] {
			issue.Missing = append(issue.Missing, m)
		}
	}
	if len(issue.Missing) > 0 {
		issue.State = "mismatch"
		return issue
	}
	issue.State = "ok"
	return issue
}

// ---- AI 就绪 ----

// AIReadiness 汇报 ext.ai 的就绪情况（**只报存在与否，绝不回显密钥明文**）。
//
// 存在的理由（2026-09-22 实测缺陷）：没配 Key 时，只能等业务跑起来撞到厂商的
// `LLM HTTP 401 {"code":30014,"message":"Token is invalid."}` —— 那句话既没说缺哪个
// 环境变量，也没说去哪配。doctor 的职责正是「把环境问题与 YAML 问题一次分开」，
// 而 AI 凭据是这块拼图里此前唯一没被覆盖的。
//
// 优先级链与 engine 的 resolveLLMConfig 保持一致：调用参数 > 环境变量 > 配置库 > 内置默认；
// 这里看不到「调用参数」这一层（那是每次调用才有的），但环境变量与配置库都能看到，
// 故 ready=false 才真正意味着「这两层都没有 key」——不代表用户不能靠 api_key 参数跑通，
// 文案里写清楚这一点。
func AIReadiness() map[string]any {
	pick := func(env, cfgKey, def string) string {
		if v := strings.TrimSpace(os.Getenv(env)); v != "" {
			return v
		}
		if v := configValue(cfgKey); v != "" {
			return v
		}
		return def
	}
	provider := pick("GEAR_AI_PROVIDER", "ai.provider", "siliconflow")
	keyFromConfig := strings.TrimSpace(configValue("ai.api_key")) != ""

	out := map[string]any{
		"provider": provider,
		// base_url / model 留空即用厂商默认，故空串是**有意**的合法值（不是缺失）。
		"base_url":     pick("GEAR_AI_BASE_URL", "ai.base_url", ""),
		"model":        pick("GEAR_AI_MODEL", "ai.model", ""),
		"key_env":      "GEAR_AI_API_KEY",
		"key_from_env": os.Getenv("GEAR_AI_API_KEY") != "",
		// 配置库层：用 `gear config set ai.api_key ...` 配过即为 true（与环境变量同效）。
		"key_from_config": keyFromConfig,
		"note_priority":   "调用参数 > 环境变量 > 配置库（gear config set ai.*）> 内置默认（见 engine.resolveLLMConfig）",
	}

	if strings.EqualFold(provider, "mock") {
		out["ready"] = true
		out["note"] = "provider=mock：离线桩实现，不需要 API Key"
		return out
	}
	if !out["key_from_env"].(bool) && !keyFromConfig {
		out["ready"] = false
		out["blocked"] = "环境变量 GEAR_AI_API_KEY 与配置库 ai.api_key 都没有：任何 ext.ai.* 调用都会以厂商 401 失败（报错文本是厂商原文，不含病因）"
		out["how_to_fix"] = strings.Join([]string{
			"① 用 gear config set ai.api_key <key> 写进配置库（推荐：配一次，所有进程生效，不必再 export）",
			"② 或设环境变量 GEAR_AI_API_KEY（经 MCP/stdio 使用时写进客户端的 env 块）",
			"③ 或让 Flow 用 ${secrets.NAME} 引用密钥源（密钥由人在 CLI 或 REST 面播种，MCP 不写密钥）",
			"④ 或在每次调用时显式传 api_key 参数（适合 BYOK 的浏览器端场景）",
		}, "；")
		return out
	}
	out["ready"] = true
	return out
}

// configValue 读配置库里的一项（AI 缺省配置层）。
//
// 与 engine.resolveLLMConfig 的第三层**同源同一个库**（gearhome.ConfigDBPath），否则 doctor
// 会指着一份「用户明明配过」的配置报未配置——那正是 doctor 最不该犯的错（假红比不报更坏）。
// 库不存在 / 打不开 / 该项未配置一律返回空串：doctor 只如实汇报，不替用户创建任何东西。
func configValue(key string) string {
	store := cap.NewConfigStore(gearhome.ConfigDBPath())
	defer func() { _ = store.Close() }()
	v, err := store.Get(key)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(v)
}

// ExtensionConfigPath 返回扩展配置文件路径（与装配期同一解析器：GEAR_EXT_CONFIG 优先，
// 否则按「可执行文件同目录 → plugin/ → cwd」候选序列定位）。
//
// 与 engine.ResolveExtensionConfigPath 共用一份实现是有意为之：doctor 说的位置必须就是
// 装配期真正读的那个文件，两套候选序列必然漂移（「doctor 说有配置、实际没加载」这类假绿
// 极难排查）。显式路径不存在时返回空串——doctor 只负责如实汇报，报错留给装配期。
func ExtensionConfigPath() string {
	p, err := engine.ResolveExtensionConfigPath()
	if err != nil {
		return ""
	}
	return p
}

// ExtensionDeclared 读扩展声明文件里的 ext 名清单（供报告展示「声明了哪些」）。
//
// 解析失败**不报错**：doctor 的价值是「尽量说清现状」，而配置文件的语法问题会在装配期
// 以更准确的方式暴露；这里返回空并让调用方如实标注即可。
func ExtensionDeclared(path string) []string {
	if path == "" {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var cfg map[string]struct {
		Cmd []string `yaml:"cmd"`
	}
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return nil
	}
	out := make([]string, 0, len(cfg))
	for k := range cfg {
		if strings.HasPrefix(k, "ext.") {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}
