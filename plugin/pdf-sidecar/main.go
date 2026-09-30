// Command pdf-sidecar 是 ext.pdf 的常驻侧车（service ext）：把正文写成 A4 的 PDF。
//
// 四条口径（PRD §4.5）：
//  1. **零第三方依赖**：PDF 语法与 TrueType 子集全部手写，go.mod / go.sum 一字不变
//     （AC-E4-4 可机械验证）。
//  2. **CJK 靠运行时读系统 TTF + 按字子集嵌入**（2026-09-29 拍板，胜出理由是
//     零捆绑 + 覆盖跟系统字体走）：不把 10MB 字体塞进仓库，也不把 PDF 撑成 10MB。
//     代价是 `font.go` 里那段手写子集化——PRD §4.5 风险登记的就是它。
//  3. **文本超出一页即报错**，不偷偷补页：pages 是调用方给的，它对自己会拿到几页有预期。
//  4. **确定性输出**：不写时间戳。同样的输入必须得到同样的字节，归档时可比对哈希。
//
// 环境变量：COORD_ADDR（默认 127.0.0.1:8101），由引擎按 extensions.yaml 注入。
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gear/internal/sidecar/coord"
	"gear/internal/sidecar/outpath"
)

// 错误码（错误响应的 `error` 字段形如 "<code>: <人话>"）。
const (
	errBadRequest  = "bad_request"
	errMissingPath = "missing_path"
	errNoPages     = "no_pages"
	errBadPage     = "bad_page"
	errBadSize     = "bad_font_size"
	errOverflow    = "page_overflow"
	errPathEscape  = "path_escape"
	errIOFailed    = "io_failed"
)

type service struct{}

func (s *service) Handlers() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"/write": s.handleWrite,
		"/caps":  s.handleCaps,
	}
}

func (s *service) handleCaps(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]interface{}{"result": map[string]interface{}{
		"methods": []string{"write"},
	}})
}

// ---- write ----

func (s *service) handleWrite(w http.ResponseWriter, r *http.Request) {
	args, err := decodeArgs(r)
	if err != nil {
		writeErr(w, errBadRequest, "请求体不是合法 JSON: %v", err)
		return
	}
	path := strArg(args, "path")
	if path == "" {
		writeErr(w, errMissingPath, "path 必填（相对 root 的文件名，如 reports/2026.pdf）")
		return
	}
	pages, err := parsePages(args["pages"])
	if err != nil {
		writeErr(w, errNoPages, "%s", err.Error())
		return
	}
	fontSize := defaultFontSize
	if v, ok := args["font_size"].(float64); ok {
		fontSize = v
	}
	if fontSize < minFontSize || fontSize > maxFontSize {
		writeErr(w, errBadSize, "font_size=%.1f 超出可用区间（%.0f~%.0f）", fontSize, minFontSize, maxFontSize)
		return
	}

	root := strArg(args, "root")
	if root == "" {
		root, err = os.Getwd()
		if err != nil {
			writeErr(w, errIOFailed, "取不到进程工作目录: %v", err)
			return
		}
	}
	target, err := outpath.Resolve(root, path)
	if err != nil {
		writeErr(w, errPathEscape, "%s", err.Error())
		return
	}
	if err := outpath.RejectFinalSymlink(target); err != nil {
		writeErr(w, errPathEscape, "%s", err.Error())
		return
	}

	fontData, fontName, err := loadFont(strArg(args, "font"))
	if err != nil {
		writeErr(w, errFontNoFont, "%s", err.Error())
		return
	}
	src, err := parseTTF(fontData)
	if err != nil {
		writeErr(w, errFontNoFont, "%s", err.Error())
		return
	}
	runes := collectRunes(pages)
	plan, fontBytes, err := src.subset(runes)
	if err != nil {
		writeErr(w, errCode(err), "%s", errMsg(err))
		return
	}
	out, lines, err := writePDF(pages, plan, fontBytes, src, fontSize, familyName(fontName))
	if err != nil {
		if oe, ok := err.(*pageOverflowErr); ok {
			writeErr(w, errOverflow, "%s", oe.Error())
			return
		}
		writeErr(w, errIOFailed, "生成 PDF 失败: %v", err)
		return
	}
	size, err := writeBytesAtomic(target, out)
	if err != nil {
		writeErr(w, errIOFailed, "写 %s 失败: %v", target, err)
		return
	}
	writeJSON(w, map[string]interface{}{"result": map[string]interface{}{
		"path":      target,
		"size":      size,
		"pages":     len(pages),
		"lines":     lines,
		"font":      fontName,
		"font_size": fontSize,
		"glyphs":    len(plan.order),
	}})
}

// ---- 入参 ----

// parsePages 把 pages 解析成正文列表。
//
// 同时接受 `[{text="..."}]` 与 `["..."]` 两种写法：前者是 PRD §4.5 的契约形态，
// 后者是 Lua 侧最自然的写法（每页一坨文本）。多认一种不增加昂贵的复杂度，
// 却省掉一批「为什么它说我的参数不对」的排查。
func parsePages(v interface{}) ([]string, error) {
	list, ok := v.([]interface{})
	if !ok || len(list) == 0 {
		return nil, fmt.Errorf("pages 必填且是非空数组：[{text=\"...\"}, ...] 或 [\"第一页\", \"第二页\"]")
	}
	out := make([]string, 0, len(list))
	for i, raw := range list {
		switch t := raw.(type) {
		case string:
			out = append(out, t)
		case map[string]interface{}:
			text, _ := t["text"].(string)
			if text == "" {
				return nil, fmt.Errorf("pages[%d] 缺 text（空的第 %d 页请写空串 \\\"\\\"，而不是不填）", i, i+1)
			}
			out = append(out, text)
		default:
			return nil, fmt.Errorf("pages[%d] 既不是字符串也不是 {text=...} 对象", i)
		}
	}
	return out, nil
}

// collectRunes 收集全部正文里用到的字符（去重、升序）。
//
// '\n' 是唯一的例外者：它是排版指令而不是要画出来的字，字体内没有它的字形，
// 把它也算进去只会换来一句「字体里没有 U+000A」。
func collectRunes(pages []string) []rune {
	set := map[rune]bool{}
	for _, p := range pages {
		for _, r := range p {
			if r == '\n' {
				continue
			}
			set[r] = true
		}
	}
	out := make([]rune, 0, len(set))
	for r := range set {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// ---- 字体供给 ----

// loadFont 挑一颗系统字体出来。
//
// 顺序：调用方显式给的路径 > 候选列表。候选里排第一的是 `simhei.ttf`（黑体，
// 单文件 TTF，GB2312 + 常用汉字全覆盖）——它是本机最容易到手的那颗。
//
// 候选逐个试而不是「取第一个存在的」：有些机器上的第一个候选是 .ttc / OTF，
// 那种形状本侧车处理不了，跳过它换来的是「这次居然能用」而不是一条无解的报错。
func loadFont(explicit string) ([]byte, string, error) {
	if explicit != "" {
		b, err := readFontFile(explicit)
		if err != nil {
			return nil, "", err
		}
		return b, filepath.Base(explicit), nil
	}
	var tried []string
	for _, c := range fontCandidates() {
		b, err := readFontFile(c)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			tried = append(tried, fmt.Sprintf("%s（%v）", c, err))
			continue
		}
		if _, err := parseTTF(b); err != nil {
			tried = append(tried, fmt.Sprintf("%s（%v）", c, err))
			continue
		}
		return b, filepath.Base(c), nil
	}
	return nil, "", fmt.Errorf("找不到可用的 TTF 字体%s；"+
		"请用 font 参数显式给一个路径（本侧车只支持 TrueType 轮廓的单文件 .ttf，不支持 .ttc / OTF）",
		triedSummary(tried))
}

func triedSummary(tried []string) string {
	if len(tried) == 0 {
		return ""
	}
	return "，已试过：" + strings.Join(tried, "；")
}

// readFontFile 读字体文件，带大小上限。
func readFontFile(path string) ([]byte, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if fi.Size() > maxFontBytes {
		return nil, &fontErr{errFontTooBig, fmt.Sprintf("%s 有 %d 字节，超过 %d 上限：换颗小一点的字体", path, fi.Size(), maxFontBytes)}
	}
	if fi.IsDir() {
		return nil, &fontErr{errFontLoad, fmt.Sprintf("%s 是目录而不是字体文件", path)}
	}
	return os.ReadFile(path)
}

// fontCandidates 是本机可能存在的字体路径（按「最可能可用」排序）。
func fontCandidates() []string {
	win := "C:\\Windows\\Fonts"
	if w := os.Getenv("WINDIR"); w != "" {
		win = filepath.Join(w, "Fonts")
	}
	return []string{
		filepath.Join(win, "simhei.ttf"),
		filepath.Join(win, "MSMHei.ttf"),
		filepath.Join(win, "simkai.ttf"),
		filepath.Join(win, "simfang.ttf"),
		filepath.Join(win, "SimsunExtG.ttf"),
		"/usr/share/fonts/truetype/wqy/wqy-zenhei.ttc",
		"/usr/share/fonts/truetype/arphic/uming.ttc",
		"/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf",
		"/Library/Fonts/Arial Unicode.ttf",
	}
}

// ---- 通用辅助 ----

func decodeArgs(r *http.Request) (map[string]interface{}, error) {
	var req struct {
		Args map[string]interface{} `json:"args"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return nil, err
	}
	return req.Args, nil
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code, format string, a ...interface{}) {
	writeJSON(w, map[string]interface{}{"error": code + ": " + fmt.Sprintf(format, a...)})
}

func strArg(m map[string]interface{}, k string) string {
	if v, ok := m[k].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

// errCode / errMsg 把 *fontErr 拆成响应要用的两截。
func errCode(err error) string {
	if e, ok := err.(*fontErr); ok {
		return e.code
	}
	return errFontCorrupt
}

func errMsg(err error) string {
	if e, ok := err.(*fontErr); ok {
		return e.msg
	}
	return err.Error()
}

func main() {
	addr := os.Getenv("COORD_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8101"
	}
	svc := &service{}
	c := coord.New(coord.Options{
		CheckInterval: 5 * time.Second,
		TTL:           15 * time.Second,
		OnEmpty: func() {
			log.Printf("占用列表为空，pdf-sidecar 自动退出")
			os.Exit(0)
		},
	})
	log.Printf("pdf-sidecar 协调监听 %s", addr)
	if err := c.Serve(addr, svc.Handlers()); err != nil {
		if coord.IsAddrInUse(err) {
			log.Printf("端口 %s 已被占用，pdf-sidecar 退让退出", addr)
			os.Exit(coord.ExitAddrInUse)
		}
		log.Fatalf("pdf-sidecar 异常退出: %v", err)
	}
}
