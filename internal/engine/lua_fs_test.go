package engine

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"gear/internal/cap"
)

// luaFSHost 构造声明 fs、以 t.TempDir() 为安全根的宿主能力视图（仿 lua_cap_test.go 的 luaHost）。
func luaFSHost(t *testing.T) *cap.Context {
	t.Helper()
	f, err := cap.NewRootedFS([]string{t.TempDir()})
	if err != nil {
		t.Fatalf("NewRootedFS 失败: %v", err)
	}
	return luaHost(t, []string{"fs"}, &cap.Providers{FS: f})
}

// TestLuaFSRoundtrip 正向：声明 fs 后，write/read/list/mkdir/remove 全链路可用。
func TestLuaFSRoundtrip(t *testing.T) {
	host := luaFSHost(t)
	out, err := luaRunHost(t, `
		ctx.fs.mkdir({ path = "logs" })
		ctx.fs.write({ path = "logs/a.txt", data = "hello" })
		ctx.fs.write({ path = "logs/a.txt", data = " world", append = true })
		local content = ctx.fs.read({ path = "logs/a.txt" })
		local entries = ctx.fs.list({ path = "logs" })
		ctx.fs.remove({ path = "logs/a.txt" })
		local after = ctx.fs.list({ path = "logs" })
		return { content = content, first = entries[1].name, size = entries[1].size, left = #after }
	`, nil, host)
	if err != nil {
		t.Fatalf("lua fs 往返失败: %v", err)
	}
	if out["content"] != "hello world" {
		t.Fatalf("content 不符: %v", out["content"])
	}
	if out["first"] != "a.txt" {
		t.Fatalf("list 首项不符: %v", out["first"])
	}
	switch out["size"].(type) {
	case int64, float64:
	default:
		t.Fatalf("size 应是数字: %T", out["size"])
	}
	if fmt.Sprint(out["left"]) != "0" {
		t.Fatalf("删除后应剩 0 项: %v", out["left"])
	}
}

// TestLuaFSEscapeRejected 负向：Lua 侧经 ctx.fs 越界（.. / 根外绝对路径）必须报错，
// 错误信息含「逃出安全目录」（REQ-FS-CONFINEMENT 的负向验证在 Lua 执行面上复验一次）。
func TestLuaFSEscapeRejected(t *testing.T) {
	host := luaFSHost(t)

	_, err := luaRunHost(t, `return ctx.fs.read({ path = "../x" })`, nil, host)
	if err == nil || !strings.Contains(err.Error(), "逃出安全目录") {
		t.Fatalf("ctx.fs.read ../ 应报路径逃逸，实际: %v", err)
	}

	_, err = luaRunHost(t, `return ctx.fs.write({ path = "C:/Windows/temp/evil.txt", data = "no" })`, nil, host)
	if err == nil || !strings.Contains(err.Error(), "逃出安全目录") {
		t.Fatalf("ctx.fs.write 根外绝对路径应报路径逃逸，实际: %v", err)
	}

	_, err = luaRunHost(t, `return ctx.fs.remove({ path = "" })`, nil, host)
	if err == nil || !strings.Contains(err.Error(), "逃出安全目录") {
		t.Fatalf("ctx.fs.remove 空路径应报路径逃逸，实际: %v", err)
	}
}

// TestLuaFSUndeclared 负向：未声明 fs 时 ctx.fs 根本不存在（最小权限 + 防御纵深，
// 与 ctx.db 未声明同口径——脚本连探测边界的机会都没有）。
func TestLuaFSUndeclared(t *testing.T) {
	host := luaHost(t, nil, &cap.Providers{})
	_, err := luaRunHost(t, `return ctx.fs.read({ path = "x" })`, nil, host)
	if err == nil {
		t.Fatal("未声明 fs 时访问 ctx.fs 应报错")
	}

	// 声明了但宿主没装配实现 → ErrCapabilityUnavailable（fail-closed，不静默 no-op）
	host = luaHost(t, []string{"fs"}, &cap.Providers{})
	_, err = luaRunHost(t, `return ctx.fs.read({ path = "x" })`, nil, host)
	if err == nil || !strings.Contains(err.Error(), "尚未实现") {
		t.Fatalf("声明 fs 但未提供应报 ErrCapabilityUnavailable，实际: %v", err)
	}
}

// TestLuaFSDefaultRootAnchored 正向：相对路径锚定主根——写入落在安全根内而非 cwd。
func TestLuaFSDefaultRootAnchored(t *testing.T) {
	root := t.TempDir()
	f, err := cap.NewRootedFS([]string{root})
	if err != nil {
		t.Fatalf("NewRootedFS 失败: %v", err)
	}
	host := luaHost(t, []string{"fs"}, &cap.Providers{FS: f})
	if _, err := luaRunHost(t, `return { wrote = ctx.fs.write({ path = "nested/f.txt", data = "x" }) }`, nil, host); err != nil {
		t.Fatalf("write 失败: %v", err)
	}
	if _, err := f.Read(filepath.Join(root, "nested", "f.txt")); err != nil {
		t.Fatalf("文件应落在安全根内: %v", err)
	}
}

// TestFSRootsFromConfig 配置口：config 键 `fs.roots` 提供的**附加安全根**在引擎装配期并入
// （与 ai.* 缺省配置同通道），根内放行、根外仍拒；主根恒为默认 AppDataDir 不受影响。
func TestFSRootsFromConfig(t *testing.T) {
	store := cap.NewConfigStore(filepath.Join(t.TempDir(), "config.db"))
	defer store.Close() // 不关会把 sqlite 文件句柄留到测试结束，TempDir 清理直接炸
	extra := t.TempDir()
	if err := store.Set("fs.roots", extra+";   \n"); err != nil {
		t.Fatalf("set fs.roots 失败: %v", err)
	}
	reg := newTestRegistry(t)
	defer reg.Close()
	e := New(reg, WithConfigStore(store))
	if e.host.FS == nil {
		t.Fatal("fs 应默认装配")
	}

	// 附加根内：绝对路径放行（写 + 读回）。
	if err := e.host.FS.Write(filepath.Join(extra, "a.txt"), []byte("ok"), false); err != nil {
		t.Fatalf("附加根内写入应放行: %v", err)
	}
	if got, err := e.host.FS.Read(filepath.Join(extra, "a.txt")); err != nil || string(got) != "ok" {
		t.Fatalf("附加根内读取不符: %q, %v", got, err)
	}

	// 附加根的兄弟目录：仍拒（前缀带分隔符边界）。
	sibling := filepath.Join(filepath.Dir(extra), filepath.Base(extra)+"x")
	if err := e.host.FS.Write(sibling, []byte("no"), false); !errors.Is(err, cap.ErrPathEscape) {
		t.Fatalf("附加根外应报 ErrPathEscape，实际: %v", err)
	}
	// 相对路径锚主根的行为由 TestLuaFSDefaultRootAnchored 覆盖，这里不重复（避免依赖真实 GEAR_HOME）。
}

// TestFSRootsUnsetDefaultsOnly 负向：未配置 fs.roots 时只有默认主根——任意其它目录仍拒
// （fail-safe：配置口只可能放行更多，不可能在缺省时意外放大）。
func TestFSRootsUnsetDefaultsOnly(t *testing.T) {
	store := cap.NewConfigStore(filepath.Join(t.TempDir(), "config.db"))
	defer store.Close()
	outside := t.TempDir()
	reg := newTestRegistry(t)
	defer reg.Close()
	e := New(reg, WithConfigStore(store))
	if e.host.FS == nil {
		t.Fatal("fs 应默认装配")
	}
	if err := e.host.FS.Write(filepath.Join(outside, "x.txt"), []byte("no"), false); !errors.Is(err, cap.ErrPathEscape) {
		t.Fatalf("未配置附加根时根外应报 ErrPathEscape，实际: %v", err)
	}
}
