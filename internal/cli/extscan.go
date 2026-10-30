package cli

// extscan.go 实现 `gear ext scan`（2026-10-28）：把盘上已经构建出来的侧车二进制，
// 与 extensions.yaml 已登记的条目对一遍，并把「未登记」的那些生成成 YAML 草稿。
//
// 为什么需要它：侧车自描述（REQ-EXT-SELF-DESCRIBE）只解决了「方法表从哪来」，
// 没有解决「盘上有哪些侧车、各自该叫什么、cmd 该写成什么」。那一步只能靠人记得去翻
// plugin/ 目录，漏了就静默——二进制编出来了、能力却在目录里看不见（本仓的
// mqtt-sidecar / xlsx-sidecar 在 2026-10-28 就处在「编好了但没登记」的状态）。
//
// 三条纪律（都是刻意的取舍）：
//  ① **只生成草稿，绝不改写 extensions.yaml**：写盘要人确认，`--out` 落的是独立文件，
//     任何命令都不会自动加载它。
//  ② **没验证过就不生成活条目**：`describe: true` 只在 `<cmd> --describe` 真的返回合法
//     自描述时才写；探测失败的侧车只出**全注释** stub。否则草稿一贴进去，启动期
//     fail-closed 直接把人挡在门外——比不生成更糟。
//  ③ **探测是有代价的**：探针会真起一次进程（上限 10s）。所以默认只探「未登记的」与
//     「已登记且开了 describe 的」；`--probe-all` 才连手抄条目的侧车一起探（可能短暂起服务）。

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"gear/internal/engine"
)

// probeSidecarDescribe 是 engine.ProbeSidecarDescribe 的注入点。
//
// 与 `gear ext describe` / 内核加载走**同一个**函数，人工核对 == 内核拿到的；
// 这里是包级变量只为测试能替身（真探测会起进程）。
var probeSidecarDescribe = engine.ProbeSidecarDescribe

// sidecarCandidate 是盘上的一个侧车二进制。
type sidecarCandidate struct {
	Name string   // 由文件名推导的能力名（ext.mongo）；允许人工改，草稿里改字即可
	File string   // 文件名（mongo-sidecar.exe），用于人读
	Path string   // 绝对路径，探针真正执行的东西
	Cmd  []string // 草稿里写的 cmd（相对布局，如 plugin/mongo-sidecar.exe）
}

// extScanOptions 是 `gear ext scan` 的全部入参（命令行只负责填它）。
type extScanOptions struct {
	Dirs     []string // 显式扫描目录；空则按「配置目录 / exe 目录 / cwd」+ 其 plugin/ 子目录
	Out      string   // 草稿落盘路径；空则只打到 stdout
	Force    bool     // --out 指向已存在文件时是否覆盖
	NoProbe  bool     // 不执行任何二进制，纯静态对账（草稿全为注释 stub）
	ProbeAll bool     // 连「已登记但手抄描述」的侧车也探（可能短暂起服务）
	Check    bool     // 有未登记侧车 / 已登记条目自描述失败即返回错误（CI / 闸门用）
}

// extScanRow 是「一个侧车 + 对账结论」。
type extScanRow struct {
	cand      sidecarCandidate
	declared  bool
	describes bool // yaml 里写了 describe: true
	probed    bool
	methods   int
	probeErr  error
}

// extScanCmd 构造 `gear ext scan`。
//
// 挂在 `ext` 下面而不是顶层命令：它做的是「扩展注册表 ↔ 侧车二进制」的对账，
// 与 `gear ext describe`（单个侧车的自描述）是同一族动作。
func extScanCmd() *cobra.Command {
	var opt extScanOptions
	c := &cobra.Command{
		Use:   "scan",
		Short: "对账盘上的侧车二进制与 extensions.yaml，为未登记者生成 YAML 草稿",
		Long: `扫描侧车目录，把「盘上有二进制、extensions.yaml 里没有条目」的侧车列出来，
并跑一次 <cmd> --describe 生成可直接粘进 extensions.yaml 的草稿。

  - 只生成草稿，**不改写** extensions.yaml（写盘要人确认；--out 落独立文件）
  - describe: true 只在探针真的返回合法自描述时才写，否则只出全注释 stub
  - 默认只探「未登记的」和「已登记且开了 describe 的」侧车；--probe-all 才全探

典型用法：
  gear ext scan                        # 人读报告 + 草稿打到 stdout
  gear ext scan --out plugin/new.yaml  # 草稿落盘，人工确认后再并入 extensions.yaml
  gear ext scan --check                # 有未登记侧车 / 自描述扫描失败即退出码 1（CI）
  gear ext scan --no-probe             # 不执行任何二进制，只看「谁没登记」`,
		Args: cobra.NoArgs,
		// --check 的失败是「发现了问题」而不是「命令用错了」，不该甩一屏 flag 说明。
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runExtScan(cmd.OutOrStdout(), opt)
		},
	}
	c.Flags().StringSliceVar(&opt.Dirs, "dir", nil, "显式扫描目录（可重复）；默认按配置目录 / exe 目录 / cwd 及其 plugin/ 子目录")
	c.Flags().StringVar(&opt.Out, "out", "", "把草稿写到该文件（已存在需 --force）；不传则只打到标准输出")
	c.Flags().BoolVar(&opt.Force, "force", false, "允许 --out 覆盖已存在的文件")
	c.Flags().BoolVar(&opt.NoProbe, "no-probe", false, "不执行任何侧车（纯静态对账；草稿只出注释 stub）")
	c.Flags().BoolVar(&opt.ProbeAll, "probe-all", false, "连已登记但手抄 methods/schemas 的侧车也探（会短暂起进程）")
	c.Flags().BoolVar(&opt.Check, "check", false, "存在未登记侧车、或已登记条目的自描述扫描失败时返回错误（退出码 1）")
	return c
}

// runExtScan 是命令的全部逻辑（与 cobra 解耦：测试直接喂 options 与 buffer）。
func runExtScan(w io.Writer, opt extScanOptions) error {
	cfgPath, err := engine.ResolveExtensionConfigPath()
	if err != nil {
		return err
	}
	specs, err := engine.LoadExtensionConfig(cfgPath)
	if err != nil {
		return err
	}

	roots := scanRoots(opt.Dirs, cfgPath)
	cands, scanned := discoverSidecars(roots)

	fmt.Fprintf(w, "扩展配置：%s\n", pathOrDefault(cfgPath))
	if len(scanned) == 0 {
		fmt.Fprintf(w, "扫描目录 %d 个，均不存在或无访问权限：%s\n", len(roots), strings.Join(roots, "  "))
		fmt.Fprintln(w, "提示：用 --dir 指定侧车所在目录（通常是 build/plugin 或 <安装目录>/plugin）。")
		return nil
	}
	nDeclared := 0
	for _, c := range cands {
		if _, ok := specs[c.Name]; ok {
			nDeclared++
		}
	}
	fmt.Fprintf(w, "扫描目录 %d 个，命中侧车 %d 个（已登记 %d / 未登记 %d）\n",
		len(scanned), len(cands), nDeclared, len(cands)-nDeclared)

	rows := make([]extScanRow, 0, len(cands))
	for _, c := range cands {
		row := extScanRow{cand: c}
		if spec, ok := specs[c.Name]; ok {
			row.declared = true
			row.describes = spec.Describe
		}
		if !opt.NoProbe && (!row.declared || row.describes || opt.ProbeAll) {
			// 先打印再探：探针最长 10s，卡住时必须让人看见卡在哪个侧车上。
			fmt.Fprintf(w, "  探测 %-16s %s --describe（上限 %s）…\n",
				c.Name, c.File, engine.DescribeTimeout())
			row.probed = true
			d, perr := probeSidecarDescribe([]string{c.Path})
			if perr != nil {
				row.probeErr = perr
			} else {
				row.methods = len(d.Methods)
			}
		}
		rows = append(rows, row)
	}

	fmt.Fprintln(w)
	for _, row := range rows {
		fmt.Fprintf(w, "  %-16s %s\n", row.cand.Name, scanStateText(row))
	}

	drafts, fails := buildExtensionDrafts(rows, newPortBook(readText(cfgPath)))
	if len(drafts) > 0 {
		fmt.Fprintf(w, "\n待确认的草稿 %d 条（确认后并入 extensions.yaml；本命令不会自动写该文件）：\n\n", len(drafts))
		for _, d := range drafts {
			fmt.Fprint(w, d)
			fmt.Fprintln(w)
		}
	} else {
		fmt.Fprintln(w, "\n没有未登记的侧车——盘上的侧车都已在 extensions.yaml 里。")
	}

	if fails > 0 {
		fmt.Fprintf(w, "\n⚠ %d 条已登记条目的自描述扫描失败：这是启动期会 fail-closed 的同一条判据，先修它。\n", fails)
	}

	if opt.Out != "" {
		if len(drafts) == 0 {
			fmt.Fprintf(w, "草稿为空，未写 %s。\n", opt.Out)
		} else if err := writeDraftFile(opt.Out, drafts, opt.Force); err != nil {
			return err
		} else {
			fmt.Fprintf(w, "草稿已写入 %s（%d 条）。\n", opt.Out, len(drafts))
		}
	}

	if opt.Check {
		// 两类问题都算：未登记（能力在盘上却看不见）与「已登记但扫不出自描述」
		// （启动期会 fail-closed 的同一条判据）。后者只在真探过的时候才判。
		var problems []string
		for _, row := range rows {
			switch {
			case !row.declared:
				problems = append(problems, row.cand.Name+" 未登记")
			case row.describes && row.probed && row.probeErr != nil:
				problems = append(problems, row.cand.Name+" 自描述扫描失败")
			}
		}
		if len(problems) > 0 {
			return fmt.Errorf("侧车对账有 %d 处问题：%s", len(problems), strings.Join(problems, "；"))
		}
	}
	return nil
}

// scanStateText 把一行对账结论翻成人话（按「是否登记 / 是否开了自描述 / 探没探」分档）。
func scanStateText(row extScanRow) string {
	switch {
	case row.declared && row.describes && row.probed && row.probeErr == nil:
		return fmt.Sprintf("已登记 · 自描述 %d 方法", row.methods)
	case row.declared && row.describes && row.probed:
		return "已登记 · ✗ 自描述扫描失败：" + row.probeErr.Error()
	case row.declared && row.describes:
		return "已登记 · 自描述（本轮未探测）"
	case row.declared && row.probed && row.probeErr == nil:
		// 手抄条目却探得出自描述：说明侧车其实支持 --describe，可以切过去少维护一份。
		return fmt.Sprintf("已登记 · 手抄描述，但侧车支持自描述（%d 方法，可切 describe: true）", row.methods)
	case row.declared && row.probed:
		return "已登记 · 手抄描述（侧车不支持 --describe）"
	case row.declared:
		return "已登记 · 手抄描述"
	case row.probed && row.probeErr == nil:
		return fmt.Sprintf("未登记 · 自描述 %d 方法 → 已出草稿", row.methods)
	case row.probed:
		return "未登记 · 侧车不响应 --describe → 只出注释 stub"
	default:
		return "未登记 · 未探测（--no-probe）"
	}
}

// buildExtensionDrafts 只为**未登记**的侧车出草稿；返回草稿数与「已登记但扫描失败」的条数。
//
// ports 承载「草稿之间、草稿与现有条目之间不撞端口」的分配状态（只用于建议值）。
func buildExtensionDrafts(rows []extScanRow, ports *portBook) (drafts []string, failCount int) {
	for _, row := range rows {
		if row.declared {
			if row.describes && row.probed && row.probeErr != nil {
				failCount++
			}
			continue
		}
		verified := row.probed && row.probeErr == nil
		drafts = append(drafts, renderExtensionDraft(row.cand, row.methods, verified, probeErrText(row), ports))
	}
	return drafts, failCount
}

func probeErrText(row extScanRow) string {
	if row.probeErr == nil {
		return ""
	}
	return row.probeErr.Error()
}

// renderExtensionDraft 渲染一条候选条目。
//
// verified=false 时**整段都是注释**——这是「没验证过就不生成活条目」的落点：
// 半激活的条目（有 cmd、没有 methods）会让启动期直接 fail-closed。
//
// verified=true 时按仓库现有惯例补齐 `service: true` + `listen`：本仓 12 个已登记的
// 常驻扩展全是 service ext，只留 cmd + describe 反而会退化成「按次调用的命令传输」——
// 侧车是常驻进程，那样一来每次调用都要重启它（含连接建立），且没有任何报错提示。
// 端口是**建议值**（避开现有条目占用的端口），标了 TODO 由人确认。
func renderExtensionDraft(c sidecarCandidate, methods int, verified bool, perr string, ports *portBook) string {
	var b strings.Builder
	if verified {
		fmt.Fprintf(&b, "%s:\n", c.Name)
		fmt.Fprintf(&b, "  cmd: %s\n", yamlCmdList(c.Cmd))
		b.WriteString("  service: true                 # 现有 *-sidecar 一律常驻（一次性能力改成 false）\n")
		if port := ports.pick(); port > 0 {
			fmt.Fprintf(&b, "  listen: \"127.0.0.1:%d\"      # TODO 确认端口（已避开现有条目占用的端口）\n", port)
		} else {
			b.WriteString("  # listen: \"127.0.0.1:<端口>\"   # TODO 无可建议端口，人工补\n")
		}
		fmt.Fprintf(&b, "  describe: true                # 自描述 %d 方法，methods/schemas 不手抄\n", methods)
		return b.String()
	}
	reason := "本轮未探测（--no-probe）"
	if perr != "" {
		reason = "未响应 --describe：" + perr
	}
	fmt.Fprintf(&b, "# %s 未登记，且没有可用的自描述（%s）。\n", c.Name, reason)
	b.WriteString("# 支持自描述的侧车会响应 `<cmd> --describe`；不支持就只能手抄 methods + schemas。\n")
	b.WriteString("# 补全后再去掉下面三行的注释：\n")
	fmt.Fprintf(&b, "# %s:\n", c.Name)
	fmt.Fprintf(&b, "#   cmd: %s\n", yamlCmdList(c.Cmd))
	b.WriteString("#   methods: [ ... ]   # 或改成 describe: true（侧车支持时）\n")
	return b.String()
}

// yamlCmdList 与 extensions.yaml 现有写法一致：JSON 风格的字符串数组。
func yamlCmdList(cmd []string) string {
	quoted := make([]string, 0, len(cmd))
	for _, s := range cmd {
		quoted = append(quoted, fmt.Sprintf("%q", s))
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

func writeDraftFile(path string, drafts []string, force bool) error {
	if !force {
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("%s 已存在（用 --force 覆盖，或换个 --out 路径）", path)
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	var b strings.Builder
	b.WriteString("# 由 `gear ext scan` 生成的候选条目：逐条确认后再并入 extensions.yaml。\n")
	b.WriteString("# 本文件不会被任何命令自动加载（gear 只认 extensions.yaml 本身）。\n\n")
	for _, d := range drafts {
		b.WriteString(d)
		b.WriteString("\n")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

// reListenAddr 从现有 extensions.yaml 里抠出已被占用的 `127.0.0.1:<端口>`。
// 只用来「避开」建议端口，不解析整份 yaml——这里要的是端口集合，不是配置语义。
var reListenAddr = regexp.MustCompile(`127\.0\.0\.1:(\d{2,5})`)

// scanPortBase 是建议端口的起点：本仓现有 service ext 落在 8093-8104，
// 从 8100 往上找第一个空位通常就是答案，且不会撞上低位系统端口。
const scanPortBase = 8100

// portBook 记录「已知被占用」的端口，并按需发一个新端口（草稿之间的分配状态）。
//
// 端口是**建议值**：真正可用性由人确认（本机可能还有别的进程占着）。
// 但它至少能保证「新条目不会和自己 yaml 里已有的条目撞端口」——那是复制粘贴最容易犯的错。
type portBook struct{ used map[int]bool }

func newPortBook(cfgText string) *portBook {
	b := &portBook{used: map[int]bool{}}
	for _, m := range reListenAddr.FindAllStringSubmatch(cfgText, -1) {
		if p, err := strconv.Atoi(m[1]); err == nil {
			b.used[p] = true
		}
	}
	return b
}

// pick 返回第一个未占用的端口并标记占用；端口用尽返回 0（不猜）。
func (b *portBook) pick() int {
	for p := scanPortBase; p <= 65535; p++ {
		if !b.used[p] {
			b.used[p] = true
			return p
		}
	}
	return 0
}

// readText 读配置文件原文（读不到就当空：端口建议退化为「从 8100 起」，不影响对账结论）。
func readText(path string) string {
	if path == "" {
		return ""
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(raw)
}

// scanRoots 给出默认扫描目录：配置目录 / exe 目录 / cwd，以及各自的 plugin/ 子目录。
// 覆盖三种布局：源码仓（<repo>/plugin/extensions.yaml + <repo>/build/plugin/*.exe）、
// 发布目录（<dir>/extensions.yaml + <dir>/plugin/*.exe）、只带 plugin/ 子目录的部署。
func scanRoots(explicit []string, cfgPath string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(p string) {
		if p == "" {
			return
		}
		abs, err := filepath.Abs(p)
		if err != nil {
			abs = p
		}
		if seen[abs] {
			return
		}
		seen[abs] = true
		out = append(out, abs)
	}
	if len(explicit) > 0 {
		for _, d := range explicit {
			add(d)
		}
		return out
	}
	var bases []string
	if cfgPath != "" {
		bases = append(bases, filepath.Dir(cfgPath))
	}
	if exe, err := os.Executable(); err == nil {
		bases = append(bases, filepath.Dir(exe))
	}
	if cwd, err := os.Getwd(); err == nil {
		bases = append(bases, cwd)
	}
	for _, b := range bases {
		add(b)
		add(filepath.Join(b, "plugin"))
	}
	return out
}

// discoverSidecars 扫目录找 `*-sidecar` / `*-sidecar.exe`，返回候选与「真正存在的目录」。
//
// 只认 `*-sidecar` 后缀：`sidecar-template.exe` 这类模板不是可登记的能力，
// 而名字里带 sidecar 却不以此结尾的文件（如 sidecar-template）一律不猜。
func discoverSidecars(roots []string) (cands []sidecarCandidate, scanned []string) {
	seen := map[string]bool{}
	for _, root := range roots {
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		scanned = append(scanned, root)
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			name, ok := sidecarNameFromFile(e.Name())
			if !ok || seen[name] {
				continue
			}
			seen[name] = true
			abs := filepath.Join(root, e.Name())
			cands = append(cands, sidecarCandidate{
				Name: name,
				File: e.Name(),
				Path: abs,
				Cmd:  []string{draftCmdPath(abs)},
			})
		}
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].Name < cands[j].Name })
	return cands, scanned
}

// sidecarNameFromFile 由文件名推导能力名：`mongo-sidecar.exe` → `ext.mongo`。
func sidecarNameFromFile(file string) (string, bool) {
	base := strings.ToLower(file)
	base = strings.TrimSuffix(base, ".exe")
	stem, ok := strings.CutSuffix(base, "-sidecar")
	if !ok || stem == "" {
		return "", false
	}
	return "ext." + stem, true
}

// draftCmdPath 把绝对路径压成 extensions.yaml 里惯用的相对写法（`plugin/x-sidecar.exe`）。
//
// 只做「父目录叫 plugin/ 就去掉前缀」这一件事：cmd 的解析顺序是
// 配置目录 → exe 目录 → cwd（见 engine.resolveExtensionCmd），
// 写 `plugin/x.exe` 在源码仓与发布目录两种布局下都能命中。
func draftCmdPath(abs string) string {
	base := filepath.Base(abs)
	if strings.EqualFold(filepath.Base(filepath.Dir(abs)), "plugin") {
		return "plugin/" + base
	}
	return base
}
