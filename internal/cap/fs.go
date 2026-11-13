package cap

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// FSEntry 是 fs.list 返回的单条目录项。
type FSEntry struct {
	Name  string `json:"name"`
	Size  int64  `json:"size"`
	IsDir bool   `json:"is_dir"`
}

// FS 受安全目录收口的宿主侧文件读写原语（REQ-FS-PRIM）。
//
// 为什么不开放 Lua 原生 io 库：REQ-LUA-SANDBOX 的契约是 io/os 永不加载（文件加载类全局
// 是 Lua 逃逸宿主的主通道，配置项不能关掉沙箱本身）。文件能力因此做成**宿主侧原语**：
// 路径解析、符号链接消解、根目录包含校验全部在 Go 侧完成，脚本只能拿到收口后的结果——
// 与 db 的 dsn 回退同理由，「文件在哪」这件宿主才知道的事留在宿主侧。
//
// 收口规则（REQ-FS-CONFINEMENT，每次调用都走 resolve，无缓存旁路）：
//   - 相对路径锚定主根 roots[0]；绝对路径必须落在任一安全根内；
//   - 路径先 Clean 再逐段消解符号链接（realPath，含 NTFS junction），
//     符号链接逃逸同样被拒；
//   - Windows 盘符大小写不敏感（strings.EqualFold），其余逐字符比较；
//   - Remove 拒绝根目录本身（不许删根）。
type FS interface {
	Read(path string) ([]byte, error)
	// Write 写文件；appendMode=true 时追加（O_APPEND），否则截断重写（O_TRUNC）。
	Write(path string, data []byte, appendMode bool) error
	List(path string) ([]FSEntry, error)
	// Mkdir 递归建目录（MkdirAll 语义；已存在不报错）。
	Mkdir(path string) error
	// Remove 删除文件或空目录（os.Remove 语义，非递归）。
	Remove(path string) error
}

// ErrPathEscape 是 fs 原语的路径逃逸错误（统一文案，便于测试与文档对齐）。
var ErrPathEscape = errors.New("路径逃出安全目录")

// RootedFS 是 FS 的默认实现：把所有路径收口在预解析的安全根集合内。
type RootedFS struct {
	// roots[0] 是主根（相对路径落这），其余是额外放行的只读/读写根。
	roots []string
}

// NewRootedFS 构造以 roots 为安全根的文件原语。roots 至少 1 个；每个根在构造期
// 转绝对路径、Clean、MkdirAll 并 EvalSymlinks 预解析（macOS 的 /tmp → /private/tmp
// 这类链接在构造期消掉，运行期 containment 比较才不会假阴性）。
func NewRootedFS(roots []string) (*RootedFS, error) {
	if len(roots) == 0 {
		return nil, fmt.Errorf("安全根至少 1 个")
	}
	cleaned := make([]string, 0, len(roots))
	for _, r := range roots {
		abs, err := filepath.Abs(r)
		if err != nil {
			return nil, fmt.Errorf("安全根 %q 解析失败: %w", r, err)
		}
		abs = filepath.Clean(abs)
		if err := os.MkdirAll(abs, 0o755); err != nil {
			return nil, fmt.Errorf("安全根 %q 建目录失败: %w", abs, err)
		}
		real, err := realPath(abs)
		if err != nil {
			return nil, fmt.Errorf("安全根 %q 解析符号链接失败: %w", abs, err)
		}
		cleaned = append(cleaned, filepath.Clean(real))
	}
	return &RootedFS{roots: cleaned}, nil
}

// maxSymlinkDepth 是单条路径符号链接消解的深度上限（防符号链接环死递归）。
const maxSymlinkDepth = 32

// realPath 把任意路径消解成物理真实路径。
//
// 为什么不用 filepath.EvalSymlinks：它在部分 Windows 环境（本仓实机 `C:\Users\<u>\AppData`
// 之下，含 AppData\Local\Temp）对**真实存在**的目录报 ERROR_PATH_NOT_FOUND，逐段手工
// 消解则全部正常——故改为逐段 Lstat + Readlink：目录直接深入，符号链接（含 NTFS junction，
// Go 的 Lstat 同样报 ModeSymlink）就地展开重解，不存在的末段原样保留（运行期 Write 会创建）。
func realPath(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("路径 %q 解析失败: %w", p, err)
	}
	return resolveSymlinks(filepath.Clean(abs), 0)
}

// resolveSymlinks 从左到右逐段消解符号链接。depth 防环（超过 maxSymlinkDepth 即报错）。
func resolveSymlinks(p string, depth int) (string, error) {
	if depth > maxSymlinkDepth {
		return "", fmt.Errorf("路径 %q 符号链接嵌套过深（疑似环）", p)
	}
	if fi, err := os.Lstat(p); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(p)
		if err != nil {
			return "", fmt.Errorf("读取符号链接 %q 失败: %w", p, err)
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(p), target)
		}
		return resolveSymlinks(filepath.Clean(target), depth+1)
	}
	dir, base := filepath.Split(filepath.Clean(p))
	if dir == "" || base == "" {
		// 到达卷根或无末段：整条已消解完。
		return filepath.Clean(p), nil
	}
	realDir, err := resolveSymlinks(filepath.Clean(dir), depth)
	if err != nil {
		return "", err
	}
	return filepath.Join(realDir, base), nil
}

// resolve 把用户路径收口到安全根内，返回物理真实路径。每次调用都走，无缓存旁路。
// 空路径视为主根本身（fs.list({path=""}) = 列根）；Remove 对根本身另有显式拒绝。
func (f *RootedFS) resolve(p string) (string, error) {
	if strings.TrimSpace(p) == "" {
		return f.roots[0], nil
	}
	var joined string
	if filepath.IsAbs(p) {
		joined = filepath.Clean(p)
	} else {
		joined = filepath.Join(f.roots[0], p)
	}
	real, err := realPath(joined)
	if err != nil {
		return "", err
	}
	for _, root := range f.roots {
		if contained(real, root) {
			return real, nil
		}
	}
	return "", fmt.Errorf("%w: %q（落点 %q 不在任一安全根内）", ErrPathEscape, p, real)
}

// contained 报告 path 是否等于 root 或位于 root 内。
// Windows 盘符/目录大小写不敏感（EqualFold），其余平台逐字符比较；
// 前缀比较必须带分隔符边界，防止 /tmp/a 放行 /tmp/ab。
func contained(path, root string) bool {
	if root == path {
		return true
	}
	sep := string(os.PathSeparator)
	if !strings.HasSuffix(root, sep) {
		root += sep
	}
	if len(path) < len(root) {
		return false
	}
	prefix := path[:len(root)]
	if os.PathSeparator == '\\' {
		return strings.EqualFold(prefix, root)
	}
	return prefix == root
}

func (f *RootedFS) Read(path string) ([]byte, error) {
	real, err := f.resolve(path)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(real)
}

func (f *RootedFS) Write(path string, data []byte, appendMode bool) error {
	real, err := f.resolve(path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(real), 0o755); err != nil {
		return fmt.Errorf("建父目录失败: %w", err)
	}
	flags := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	if appendMode {
		flags = os.O_CREATE | os.O_WRONLY | os.O_APPEND
	}
	file, err := os.OpenFile(real, flags, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = file.Write(data)
	return err
}

func (f *RootedFS) List(path string) ([]FSEntry, error) {
	real, err := f.resolve(path)
	if err != nil {
		return nil, err
	}
	dirents, err := os.ReadDir(real)
	if err != nil {
		return nil, err
	}
	out := make([]FSEntry, 0, len(dirents))
	for _, d := range dirents {
		entry := FSEntry{Name: d.Name(), IsDir: d.IsDir()}
		if info, err := d.Info(); err == nil && !d.IsDir() {
			entry.Size = info.Size()
		}
		out = append(out, entry)
	}
	return out, nil
}

func (f *RootedFS) Mkdir(path string) error {
	real, err := f.resolve(path)
	if err != nil {
		return err
	}
	return os.MkdirAll(real, 0o755)
}

func (f *RootedFS) Remove(path string) error {
	real, err := f.resolve(path)
	if err != nil {
		return err
	}
	for _, root := range f.roots {
		if real == root || (os.PathSeparator == '\\' && strings.EqualFold(real, root)) {
			return fmt.Errorf("%w: 不允许删除安全根本身 %q", ErrPathEscape, root)
		}
	}
	return os.Remove(real)
}
