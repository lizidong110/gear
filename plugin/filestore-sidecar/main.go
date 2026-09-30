// Command filestore-sidecar 是 ext.filestore 的常驻侧车（service ext）：
//
//   - 绑定声明端口（listen / COORD_ADDR，127.0.0.1 only），对外暴露 RPC
//     （/put /get /stat /list /delete /url /caps）+ coord 协调端点（/register 等）
//     （端口默认 127.0.0.1:8096，引擎拉起时按 extensions.yaml 的 listen 注入 COORD_ADDR）；
//   - **root 不硬编码**（PRD §4.3）：优先用调用参数里的 root，其次读内核配置库的
//     `filestore.root`（与 `ctx.config` 同源——侧车是独立进程，`ctx.config` 在侧车侧
//     根本不存在，故直接取同一份 config.db），两者都没有 → fail-closed。
//     可移植锚点沿用 serve 侧车的规则：相对路径依次按「安装根 / cwd」解析。
//   - **路径穿越防护**（注意：这是 ext 原子里第一次做路径安全，规则写死在此）：
//     ① key 按 "/" 分段，空段 / "." / ".." 一律拒；② 拼接后必须仍在 root 内；
//     ③ **不跟随符号链接**：路径上任何一级（含末级文件）若是指向 root 之外的符号链接即拒。
//     第 ③ 条不是洁癖——少了它，`delete` 就能顺着链接删掉 root 外的文件，
//     而写操作会往链接指向的地方落盘。
//   - **大小上限 fail-closed**：默认 32 MiB，可被配置库的 `filestore.max_bytes` 放宽；
//     超限在**落盘之前**就拒（先解码、判长度，再写临时文件再改名），磁盘上不留残留文件。
//   - **二进制走 base64**：Lua 侧没有字节类型，故 put 收 `data_base64`、get 返回 `data_base64`。
//     不做 MIME 嗅探，content_type 信任调用方声明，缺省 application/octet-stream。
//   - **url 只返回相对路径**（`/_filestore/<key>`）：内核 REST 面 +0 是红线，
//     下发绝对 URL 等于逼应用自己拼前缀，相对路径让应用决定挂在哪。
//
// 环境变量：COORD_ADDR（默认 127.0.0.1:8096；引擎拉起时按 extensions.yaml 的 listen 注入，
// 此处默认值仅供不经引擎的手工单跑兜底）。
package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gear/internal/cap"
	"gear/internal/gearhome"
	"gear/internal/sidecar/coord"
)

const (
	// indexName 是内容类型索引的文件名（直接放在 root 下、以点开头，list 会跳过它）。
	// 为什么需要它：content_type 不是文件系统元数据，而「put 写进去、get 读出来」要保持一致
	// （AC-E2-1）。用它而不是 per-key 的 .meta 伴生文件，是为了 delete 时不用扫目录。
	indexName = ".filestore-index.json"

	defaultMaxBytes = 32 << 20 // 32 MiB

	// 配置键：与 `gear config set filestore.root=...` / `ctx.config.get({key="filestore.root"})`
	// 同一套扁平点号键（config.db 就是这么存的）。
	cfgKeyRoot    = "filestore.root"
	cfgKeyMaxByte = "filestore.max_bytes"

	defaultContentType = "application/octet-stream"

	// urlPrefix 是 url() 返回的相对路径前缀（应用自己挂反代/前端路由）。
	urlPrefix = "/_filestore/"
)

// entry 是索引里的一行（只有 content_type 需要跨调用记住；size/mod_time 一律现取现报）。
type entry struct {
	ContentType string `json:"content_type"`
}

// index 是 key → 元数据的映射。
type index map[string]entry

// Service 是 filestore 侧车的业务核心：无状态（root 在每次调用时解析），
// 唯一共享的是配置库句柄与索引写的串行化。
type Service struct {
	mu  sync.Mutex
	cfg *cap.ConfigStore
}

// newService 构造业务核心。cfgPath 为空时用内核默认落点（gearhome.ConfigDBPath）。
func newService(cfgPath string) *Service {
	if cfgPath == "" {
		cfgPath = gearhome.ConfigDBPath()
	}
	return &Service{cfg: cap.NewConfigStore(cfgPath)}
}

// Handlers 返回 RPC 端点（与 coord 协调端点合并到同一 mux）。
func (s *Service) Handlers() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"/put":    s.handlePut,
		"/get":    s.handleGet,
		"/stat":   s.handleStat,
		"/list":   s.handleList,
		"/delete": s.handleDelete,
		"/url":    s.handleURL,
		"/caps":   s.handleCaps,
	}
}

// Stop 供 OnEmpty / 进程退出时调用（本侧车无常驻资源，唯一要做的是关掉配置库连接）。
func (s *Service) Stop() { _ = s.cfg.Close() }

// ---- root / key 解析（安全口径的唯一实现处）----

// resolveRoot 按「调用参数 root > 配置库 filestore.root > 失败」三档取存储根。
func (s *Service) resolveRoot(args map[string]interface{}) (string, error) {
	if v, ok := args["root"].(string); ok && strings.TrimSpace(v) != "" {
		return resolveDir(v)
	}
	if s.cfg != nil {
		if v, err := s.cfg.Get(cfgKeyRoot); err == nil && strings.TrimSpace(v) != "" {
			return resolveDir(v)
		}
	}
	return "", fmt.Errorf("filestore root 未配置：调用传 root 参数，或 `gear config set %s <目录>`", cfgKeyRoot)
}

// resolveDir 把入参目录解析为真实存在的目录（可移植锚点，与 serve 侧车同规则）：
// 绝对路径原样校验；相对路径依次按「安装根 / cwd」解析，都不存在即 fail-closed。
func resolveDir(dir string) (string, error) {
	if dir == "" {
		return "", fmt.Errorf("目录为空")
	}
	if !filepath.IsAbs(dir) {
		for _, base := range []string{gearhome.InstallRoot(), wd()} {
			if base == "" {
				continue
			}
			if fi, err := os.Stat(filepath.Join(base, dir)); err == nil && fi.IsDir() {
				abs, err := filepath.Abs(filepath.Join(base, dir))
				if err == nil {
					return abs, nil
				}
			}
		}
		return "", fmt.Errorf("目录不存在或不是目录: %s", dir)
	}
	fi, err := os.Stat(dir)
	if err != nil || !fi.IsDir() {
		return "", fmt.Errorf("目录不存在或不是目录: %s", dir)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("目录路径非法: %s", dir)
	}
	return abs, nil
}

// normalizeKey 校验并把 key 统一成 "/" 分隔的相对路径。
// 规则（越界即在写盘前被拒）：空 / 空段 / "." 段 / ".." 段 / 反斜杠混用一律拒。
func normalizeKey(key string) (string, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return "", fmt.Errorf("key 必填")
	}
	if strings.Contains(key, "\\") {
		// Windows 上 '\\' 也是分隔符，但混用会绕过按 "/" 的分段检查，直接拒。
		return "", fmt.Errorf("key 不允许含反斜杠: %s", key)
	}
	parts := strings.Split(key, "/")
	for _, p := range parts {
		switch p {
		case "", ".", "..":
			return "", fmt.Errorf("key 含空段或 \"%s\" 段: %s", p, key)
		}
	}
	return strings.Join(parts, "/"), nil
}

// resolveTarget 把 key 拼到 root 下并确认没越出 root。
func resolveTarget(root, key string) (string, error) {
	t := filepath.Join(root, filepath.FromSlash(key))
	rel, err := filepath.Rel(root, t)
	if err != nil {
		return "", fmt.Errorf("key 解析失败: %s", key)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("key 越出 root: %s", key)
	}
	return t, nil
}

// rejectSymlinkChain 检查 root 到 target 的路径上有没有指向 root 之外的符号链接。
// 逐级查已存在的那几层（末级文件也算），是符号链接就 EvalSymlinks 判定落点。
func rejectSymlinkChain(root, target string) error {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return fmt.Errorf("路径非法: %v", err)
	}
	parts := strings.Split(rel, string(filepath.Separator))
	cur := root
	for _, p := range parts {
		if p == ".." || p == "." {
			continue
		}
		cur = filepath.Join(cur, p)
		fi, err := os.Lstat(cur)
		if err != nil {
			continue // 还不存在（put 的新文件），放行
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			continue
		}
		real, err := filepath.EvalSymlinks(cur)
		if err != nil || !withinRoot(real, root) {
			return fmt.Errorf("路径是符号链接且指向 root 之外，拒绝跟随: %s", cur)
		}
	}
	return nil
}

// withinRoot 判断 path 是否落在 root 之内（华为口径：等于 root 也算内）。
func withinRoot(p, root string) bool {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != "..")
}

// maxBytes 取大小上限：配置库的 filestore.max_bytes 优先，缺省 32 MiB，非法值回退缺省。
func (s *Service) maxBytes(args map[string]interface{}) (int64, error) {
	if v, ok := args["max_bytes"].(float64); ok && v > 0 {
		return int64(v), nil
	}
	if s.cfg != nil {
		if raw, err := s.cfg.Get(cfgKeyMaxByte); err == nil {
			if n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64); err == nil && n > 0 {
				return n, nil
			}
		}
	}
	return defaultMaxBytes, nil
}

// ---- 索引 ----

func (s *Service) loadIndex(root string) index {
	raw, err := os.ReadFile(filepath.Join(root, indexName))
	if err != nil {
		return index{} // 首次使用没有索引，不是错误
	}
	ix := index{}
	if err := json.Unmarshal(raw, &ix); err != nil {
		// 索引坏了：**不静默当空**（那会让已上传对象的 content_type 悄悄退化成
		// octet-stream），而是按空处理但点名报错，让运维一眼看到要重建。
		log.Printf("[filestore] 索引损坏，按空处理: %v", err)
		return index{}
	}
	return ix
}

// saveIndex 原子落盘（临时文件 + 改名）：写一半挂掉不会留下半截索引。
func (s *Service) saveIndex(root string, ix index) error {
	b, err := json.MarshalIndent(ix, "", "  ")
	if err != nil {
		return err
	}
	dst := filepath.Join(root, indexName)
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// ---- 各方法 ----

// handlePut 写入一个对象：{key, data_base64, content_type?, root?} → {key,size,content_type,mod_time}
func (s *Service) handlePut(w http.ResponseWriter, r *http.Request) {
	args, err := decodeArgs(r)
	if err != nil {
		writeJSON(w, map[string]interface{}{"error": err.Error()})
		return
	}
	root, err := s.resolveRoot(args)
	if err != nil {
		writeJSON(w, map[string]interface{}{"error": err.Error()})
		return
	}
	key, err := normalizeKey(strOr(args, "key", ""))
	if err != nil {
		writeJSON(w, map[string]interface{}{"error": err.Error()})
		return
	}
	limit, err := s.maxBytes(args)
	if err != nil {
		writeJSON(w, map[string]interface{}{"error": err.Error()})
		return
	}
	// 先解码再判长度再落盘：顺序反了就会在磁盘上留下超限的半截文件。
	raw, err := base64.StdEncoding.DecodeString(strOr(args, "data_base64", ""))
	if err != nil {
		writeJSON(w, map[string]interface{}{"error": "data_base64 不是合法 base64: " + err.Error()})
		return
	}
	if int64(len(raw)) > limit {
		writeJSON(w, map[string]interface{}{"error": fmt.Sprintf("对象超过大小上限（%d 字节，上限 %d）", len(raw), limit)})
		return
	}
	target, err := resolveTarget(root, key)
	if err != nil {
		writeJSON(w, map[string]interface{}{"error": err.Error()})
		return
	}
	if err := rejectSymlinkChain(root, target); err != nil {
		writeJSON(w, map[string]interface{}{"error": err.Error()})
		return
	}
	if fi, err := os.Lstat(target); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		writeJSON(w, map[string]interface{}{"error": "目标已存在且是符号链接，拒绝写入（避免穿透到链接指向的位置）: " + key})
		return
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		writeJSON(w, map[string]interface{}{"error": "创建目录失败: " + err.Error()})
		return
	}
	ct := strOr(args, "content_type", defaultContentType)
	// 临时文件 + 改名：写入中途失败不会留下半截对象（rename 在同一分区是原子的）。
	tmp := target + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		_ = os.Remove(tmp)
		writeJSON(w, map[string]interface{}{"error": "写入失败: " + err.Error()})
		return
	}
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		writeJSON(w, map[string]interface{}{"error": "落盘失败: " + err.Error()})
		return
	}
	mod, _ := statModTime(target)

	s.mu.Lock()
	ix := s.loadIndex(root)
	ix[key] = entry{ContentType: ct}
	saveErr := s.saveIndex(root, ix)
	s.mu.Unlock()
	if saveErr != nil {
		writeJSON(w, map[string]interface{}{"error": "写索引失败: " + saveErr.Error()})
		return
	}

	writeJSON(w, map[string]interface{}{"result": map[string]interface{}{
		"key": key, "size": len(raw), "content_type": ct, "mod_time": mod,
	}})
}

// handleGet 读取一个对象：{key, root?} → {data_base64, content_type, size, mod_time}
func (s *Service) handleGet(w http.ResponseWriter, r *http.Request) {
	args, _ := decodeArgs(r)
	root, err := s.resolveRoot(args)
	if err != nil {
		writeJSON(w, map[string]interface{}{"error": err.Error()})
		return
	}
	key, err := normalizeKey(strOr(args, "key", ""))
	if err != nil {
		writeJSON(w, map[string]interface{}{"error": err.Error()})
		return
	}
	target, err := resolveTarget(root, key)
	if err != nil {
		writeJSON(w, map[string]interface{}{"error": err.Error()})
		return
	}
	if err := rejectSymlinkChain(root, target); err != nil {
		writeJSON(w, map[string]interface{}{"error": err.Error()})
		return
	}
	raw, err := os.ReadFile(target)
	if err != nil {
		writeJSON(w, map[string]interface{}{"error": "读取失败: " + notFound(err, key)})
		return
	}
	mod, _ := statModTime(target)
	s.mu.Lock()
	ct := s.loadIndex(root)[key].ContentType
	s.mu.Unlock()
	if ct == "" {
		ct = defaultContentType
	}
	writeJSON(w, map[string]interface{}{"result": map[string]interface{}{
		"key": key, "data_base64": base64.StdEncoding.EncodeToString(raw),
		"content_type": ct, "size": len(raw), "mod_time": mod,
	}})
}

// handleStat 只看不取：{key, root?} → {key,size,content_type,mod_time}
func (s *Service) handleStat(w http.ResponseWriter, r *http.Request) {
	args, _ := decodeArgs(r)
	root, err := s.resolveRoot(args)
	if err != nil {
		writeJSON(w, map[string]interface{}{"error": err.Error()})
		return
	}
	key, err := normalizeKey(strOr(args, "key", ""))
	if err != nil {
		writeJSON(w, map[string]interface{}{"error": err.Error()})
		return
	}
	target, err := resolveTarget(root, key)
	if err != nil {
		writeJSON(w, map[string]interface{}{"error": err.Error()})
		return
	}
	if err := rejectSymlinkChain(root, target); err != nil {
		writeJSON(w, map[string]interface{}{"error": err.Error()})
		return
	}
	fi, err := os.Stat(target)
	if err != nil {
		writeJSON(w, map[string]interface{}{"error": "对象不存在: " + key})
		return
	}
	mod := fi.ModTime().UTC().Format(time.RFC3339)
	s.mu.Lock()
	ct := s.loadIndex(root)[key].ContentType
	s.mu.Unlock()
	if ct == "" {
		ct = defaultContentType
	}
	writeJSON(w, map[string]interface{}{"result": map[string]interface{}{
		"key": key, "size": fi.Size(), "content_type": ct, "mod_time": mod,
	}})
}

// handleList 列 key（默认全量）：{prefix?, root?} → {keys:[...]}
func (s *Service) handleList(w http.ResponseWriter, r *http.Request) {
	args, _ := decodeArgs(r)
	root, err := s.resolveRoot(args)
	if err != nil {
		writeJSON(w, map[string]interface{}{"error": err.Error()})
		return
	}
	prefix := strOr(args, "prefix", "")
	keys := []string{}
	// WalkDir 按顺序遍历，跳过索引文件本身（它是元数据，不是用户数据）。
	err = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // 目录被删了之类的，跳过而不是整体失败
		}
		if d.Name() == indexName || d.Name() == indexName+".tmp" {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		key, err := normalizeKey(filepath.ToSlash(strings.TrimPrefix(strings.TrimPrefix(p, root), string(filepath.Separator))))
		if err != nil {
			return nil
		}
		if prefix != "" && !strings.HasPrefix(key, prefix) {
			return nil
		}
		keys = append(keys, key)
		return nil
	})
	if err != nil {
		writeJSON(w, map[string]interface{}{"error": "列出失败: " + err.Error()})
		return
	}
	sort.Strings(keys)
	writeJSON(w, map[string]interface{}{"result": map[string]interface{}{"keys": keys}})
}

// handleDelete 删除对象（含索引条目）：{key, root?} → {ok:true}
func (s *Service) handleDelete(w http.ResponseWriter, r *http.Request) {
	args, _ := decodeArgs(r)
	root, err := s.resolveRoot(args)
	if err != nil {
		writeJSON(w, map[string]interface{}{"error": err.Error()})
		return
	}
	key, err := normalizeKey(strOr(args, "key", ""))
	if err != nil {
		writeJSON(w, map[string]interface{}{"error": err.Error()})
		return
	}
	target, err := resolveTarget(root, key)
	if err != nil {
		writeJSON(w, map[string]interface{}{"error": err.Error()})
		return
	}
	if err := rejectSymlinkChain(root, target); err != nil {
		writeJSON(w, map[string]interface{}{"error": err.Error()})
		return
	}
	if fi, err := os.Lstat(target); err != nil || fi.Mode()&os.ModeSymlink != 0 {
		// 不存在要说清楚（fail-visible：静默成功会让调用方以为删掉了），
		// 末级是符号链接则拒——跟着链接删会删到 root 外的文件。
		if err != nil {
			writeJSON(w, map[string]interface{}{"error": "对象不存在: " + key})
			return
		}
		writeJSON(w, map[string]interface{}{"error": "目标已是符号链接，拒绝删除: " + key})
		return
	}
	if err := os.Remove(target); err != nil {
		writeJSON(w, map[string]interface{}{"error": "删除失败: " + err.Error()})
		return
	}
	s.mu.Lock()
	ix := s.loadIndex(root)
	delete(ix, key)
	saveErr := s.saveIndex(root, ix)
	s.mu.Unlock()
	if saveErr != nil {
		writeJSON(w, map[string]interface{}{"error": "写索引失败: " + saveErr.Error()})
		return
	}
	writeJSON(w, map[string]interface{}{"result": map[string]interface{}{"ok": true, "key": key}})
}

// handleURL 只返回相对路径：{key, root?} → {path}
func (s *Service) handleURL(w http.ResponseWriter, r *http.Request) {
	args, _ := decodeArgs(r)
	key, err := normalizeKey(strOr(args, "key", ""))
	if err != nil {
		writeJSON(w, map[string]interface{}{"error": err.Error()})
		return
	}
	writeJSON(w, map[string]interface{}{"result": map[string]interface{}{
		"path": urlPrefix + key,
	}})
}

// handleCaps 返回本侧车已实现的方法名（供红线校验：extensions.yaml 的 methods ⊆ caps）。
func (s *Service) handleCaps(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]interface{}{"result": map[string]interface{}{
		"ext":     "filestore",
		"methods": []string{"put", "get", "stat", "list", "delete", "url"},
	}})
}

// ---- 小工具 ----

func statModTime(target string) (string, error) {
	fi, err := os.Stat(target)
	if err != nil {
		return "", err
	}
	return fi.ModTime().UTC().Format(time.RFC3339), nil
}

// notFound 把「文件不存在」翻成人话（调用方看到的是 key，不是内核的 /tmp/xxx）。
func notFound(err error, key string) string {
	if os.IsNotExist(err) {
		return "对象不存在: " + key
	}
	return err.Error()
}

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

func strOr(m map[string]interface{}, k, def string) string {
	if v, ok := m[k].(string); ok && v != "" {
		return v
	}
	return def
}

// wd 返回进程当前工作目录；取不到时退化为 "."（fail-closed 由调用方保证）。
func wd() string {
	d, err := os.Getwd()
	if err != nil {
		return "."
	}
	return d
}

func main() {
	addr := os.Getenv("COORD_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8096"
	}

	svc := newService("")
	c := coord.New(coord.Options{
		CheckInterval: 5 * time.Second,
		TTL:           15 * time.Second,
		OnEmpty: func() {
			log.Printf("占用列表为空，filestore-sidecar 自动退出")
			svc.Stop()
			os.Exit(0)
		},
	})

	log.Printf("filestore-sidecar 监听 %s（RPC/coord 端口；存储根缺省取配置库的 %s）", addr, cfgKeyRoot)
	if err := c.Serve(addr, svc.Handlers()); err != nil {
		if coord.IsAddrInUse(err) {
			log.Printf("端口 %s 已被占用，filestore-sidecar 退让退出", addr)
			os.Exit(coord.ExitAddrInUse)
		}
		log.Fatalf("filestore-sidecar 异常退出: %v", err)
	}
}
