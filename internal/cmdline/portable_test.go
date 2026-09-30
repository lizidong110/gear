package cmdline

// portable_test.go：锁住「应用定义可移植」这条契约在命令解析侧的落点。
//
// 背景（2026-09-25）：应用的 process entry 此前只能写成
// `D:/gadern/gear/build/gear.exe serve --addr 127.0.0.1:8790`——因为裸名既不在 PATH 里、
// 也没有任何「锚到 gear 自己」的解析规则，写相对路径又会被子进程 cwd 左右。
// 结果就是整个仓库拷到别的机器即废。修法是裸名先查 gear 可执行文件所在目录，
// 于是 entry 可以写成 `gear serve --addr 127.0.0.1:8790`，与机器无关。

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"gear/internal/gearhome"
)

// TestLookExecutableResolvesBareNameFromExeDir 正向：裸名命中 gear 可执行文件所在目录。
func TestLookExecutableResolvesBareNameFromExeDir(t *testing.T) {
	exeDir := gearhome.ExeDir()
	if exeDir == "" {
		t.Skip("拿不到可执行文件目录（os.Executable 失败）")
	}
	name := "gear_portable_probe"
	file := filepath.Join(exeDir, name)
	if runtime.GOOS == "windows" {
		file += ".exe" // exeCandidates 按 PATHEXT 展开，Windows 上命中带后缀的候选
	}
	if err := os.WriteFile(file, []byte("stub"), 0o755); err != nil {
		t.Skipf("无法在 exe 目录 %q 写探针文件: %v", exeDir, err)
	}
	t.Cleanup(func() { _ = os.Remove(file) })

	got, err := LookExecutable(name, nil)
	if err != nil {
		t.Fatalf("裸名 %q 应解析到 exe 同目录: %v", name, err)
	}
	if filepath.Dir(got) != exeDir {
		t.Fatalf("解析结果不在 exe 同目录: got=%q exeDir=%q", got, exeDir)
	}
	// 静态校验（gear_validate）走的是 ResolveExecutable，必须与运行期同口径，
	// 否则会出现「注册时说找不到、跑起来却正常」这类漂移。
	if got2, err := ResolveExecutable(name, nil); err != nil || got2 != got {
		t.Fatalf("ResolveExecutable 应与 LookExecutable 同口径: got=%q err=%v", got2, err)
	}
}

// TestLookExecutableUnknownBareNameFails 负向：找不到的裸名必须报 ErrNotFound，
// 绝不退化成「返回原样字符串、交给 os/exec 去碰运气」（那会让错误推迟到运行期且语焉不详）。
func TestLookExecutableUnknownBareNameFails(t *testing.T) {
	if _, err := LookExecutable("gear_definitely_not_here_20260925", nil); !errors.Is(err, exec.ErrNotFound) {
		t.Fatalf("未知裸名应返回 exec.ErrNotFound，实际 %v", err)
	}
}
