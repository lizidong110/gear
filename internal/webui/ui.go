// Package webui 托管编译进 gear 二进制、或来自外部目录的 Web 管理界面。
//
// 设计约束（与内核「执行面无鉴权 / 不加 CORS / Web UI 仍是可选项」一致）：
//   - 本包只提供静态文件服务，不引入任何鉴权、不修改 REST 路由语义。
//   - 内嵌资源来自 `//go:embed all:public`；若构建期未把真实前端构建产物拷入
//     public/（缺 assets/），进入「未构建」模式，对任何 GET 返回 503 + 指引，
//     而不是空白页（呼应「不静默降级」红线）。
//   - GEAR_UI_DIR 指向外部目录时优先用它（开发期热更新：pnpm build 的输出目录）。
//
// ⚠ embed 目录**不能叫 dist**：仓库根 .gitignore 有全局 `**/dist/` 规则，
// 沿用 dist 会被忽略 ⇒ 干净检出该目录为空 ⇒ go:embed 编译失败。故用 public/，
// 并只忽略真实产物、保留 _placeholder.html（保证任何检出都能编译）。
package webui

import (
	"embed"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"strings"
)

//go:embed all:public
var embedded embed.FS

// envUIDir 指向外部 UI 目录（开发期热更新）。设了且存在则优先于内嵌。
const envUIDir = "GEAR_UI_DIR"

// placeholderHTML 是「前端未编译进二进制」时返回的指引页（自带样式、零依赖）。
const placeholderHTML = `<!doctype html>
<html lang="zh-CN">
<head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Gear · Web UI 未构建</title>
<style>
  :root{color-scheme:dark}
  body{margin:0;font-family:system-ui,-apple-system,"Segoe UI",Roboto,"PingFang SC","Microsoft YaHei",sans-serif;
       background:#0f1115;color:#e6e6e6;display:flex;min-height:100vh;align-items:center;justify-content:center}
  .card{max-width:560px;padding:32px 36px;background:#171a21;border:1px solid #2a2f3a;border-radius:14px;line-height:1.7}
  h1{font-size:20px;margin:0 0 12px}
  code{background:#0b0d11;padding:2px 6px;border-radius:6px;color:#7ee787;font-size:13px}
  p{margin:10px 0;color:#aab2c0}
  .hint{margin-top:18px;font-size:13px;color:#8b93a1}
</style></head>
<body><div class="card">
  <h1>⚙️ Gear Web 管理界面未构建进本二进制</h1>
  <p>当前二进制在编译时未包含前端构建产物，因此 <code>/</code> 没有界面可展示。</p>
  <p>任选其一即可出界面：</p>
  <p>① 用仓库构建脚本重新编译（自动构建前端并内嵌）：<br><code>bash scripts/build.sh</code></p>
  <p>② 开发期指向已构建的前端目录：<br><code>GEAR_UI_DIR=plugin/web-ui/dist gear serve</code></p>
  <p class="hint">REST 执行面（<code>/api/*</code>）不受此影响，始终可用。</p>
</div></body></html>`

// Handler 返回一个 http.Handler，托管 Web UI。
//
// 源优先级：GEAR_UI_DIR（若设置且存在）> 内嵌 public/。
// 任一源若缺真实构建产物（缺 assets/），进入「未构建」模式返回 503 指引。
func Handler() http.Handler {
	if dir := os.Getenv(envUIDir); dir != "" {
		if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
			return newHandler(os.DirFS(dir), "", true)
		}
	}
	return newHandler(embedded, "public", false)
}

// MissingHandler 返回「前端未构建进本二进制」状态下的处理器：任何 GET 都返回
// 503 + 构建指引，而不是空白页。
//
// 为什么单独导出：内嵌是否「已构建」取决于**仓库当前有没有前端产物**
// （`internal/webui/public/assets/` 是 build.sh 生成、被 gitignore 的目录）。
// 干净检出 ⇒ 缺失态；跑过一次 build.sh ⇒ 完整态。
// 因此「缺失态」这个分支**不能靠内嵌来测**（测它就等于测仓库状态，构建一次就红），
// 必须有确定性构造入口——本函数即是。server 层与本包测试都用它锁住 503 行为。
func MissingHandler() http.Handler {
	return &uiHandler{fsys: emptyFS{}, prefix: "", external: false, missing: true}
}

// newHandler 构造 uiHandler。missing 由 fsys 内容判定（缺 assets/ 即缺失）。
func newHandler(fsys fs.FS, prefix string, external bool) http.Handler {
	return &uiHandler{fsys: fsys, prefix: prefix, external: external, missing: !hasAssets(fsys, prefix)}
}

// emptyFS 是一个永远「不存在」的 fs.FS，用于 MissingHandler（不需要真实文件）。
type emptyFS struct{}

func (emptyFS) Open(name string) (fs.File, error) { return nil, fs.ErrNotExist }

// hasAssets 判断 fsys 在 prefix 下是否含非空 assets/ 目录（真实构建产物的判据）。
func hasAssets(fsys fs.FS, prefix string) bool {
	f, err := fsys.Open(path.Join(prefix, "assets"))
	if err != nil {
		return false
	}
	defer f.Close()
	if rf, ok := f.(fs.ReadDirFile); ok {
		entries, err := rf.ReadDir(-1)
		if err != nil {
			return false
		}
		return len(entries) > 0
	}
	return true
}

// uiHandler 同源托管 Web UI。prefix 为内嵌时的 "dist" 或外部目录时的 ""。
type uiHandler struct {
	fsys     fs.FS
	prefix   string
	external bool
	missing  bool
}

func (h *uiHandler) open(name string) (fs.File, error) {
	return h.fsys.Open(path.Join(h.prefix, name))
}

func (h *uiHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.NotFound(w, r)
		return
	}
	if h.missing {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		io.WriteString(w, placeholderHTML)
		return
	}
	upath := strings.TrimPrefix(r.URL.Path, "/")
	if upath == "" {
		upath = "index.html"
	}
	// 规整路径，阻断目录穿越（../）。
	clean := strings.TrimPrefix(path.Clean("/"+upath), "/")

	f, err := h.open(clean)
	if err != nil {
		// 未知路径走 SPA 兜底（前端 hash 路由其实只用 /，此处兜底防非 hash 深链）。
		if idx, e2 := h.open("index.html"); e2 == nil {
			defer idx.Close()
			serveFile(w, r, idx, "text/html; charset=utf-8")
			return
		}
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if fi.IsDir() {
		idx, e2 := h.open(path.Join(clean, "index.html"))
		if e2 != nil {
			http.NotFound(w, r)
			return
		}
		defer idx.Close()
		serveFile(w, r, idx, "text/html; charset=utf-8")
		return
	}
	ct := contentType(clean)
	w.Header().Set("Content-Type", ct)
	if r.Method == http.MethodHead {
		w.Header().Set("Content-Length", fmt.Sprint(fi.Size()))
		return
	}
	io.Copy(w, f)
}

func serveFile(w http.ResponseWriter, r *http.Request, f fs.File, ct string) {
	fi, err := f.Stat()
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", ct)
	if r.Method == http.MethodHead {
		w.Header().Set("Content-Length", fmt.Sprint(fi.Size()))
		return
	}
	io.Copy(w, f)
}

// contentType 按扩展名给出 Content-Type（不依赖系统 mime 表，跨平台一致）。
func contentType(name string) string {
	switch strings.ToLower(path.Ext(name)) {
	case ".html", ".htm":
		return "text/html; charset=utf-8"
	case ".js", ".mjs", ".cjs":
		return "text/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".json", ".map":
		return "application/json; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".ico":
		return "image/x-icon"
	case ".woff":
		return "font/woff"
	case ".woff2":
		return "font/woff2"
	case ".ttf":
		return "font/ttf"
	case ".eot":
		return "application/vnd.ms-fontobject"
	case ".wasm":
		return "application/wasm"
	default:
		return "application/octet-stream"
	}
}
