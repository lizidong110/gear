package webui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repoRoot 从包目录（internal/webui）上溯两级到仓库根。
func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Clean(filepath.Join(wd, "..", ".."))
}

// TestBuildPipelineEmbedsWebUI 锁住 REQ-UI-NF-EMBED-PIPE：
// 「pnpm build → 拷入 internal/webui/public → go build」这条管线必须写在 scripts/build.sh 里，
// 且拷贝目标必须是 go:embed 实际吃的那个目录。
//
// 为什么用文本断言而不是真跑一遍构建：这条需求的失效模式是**「改目录名只改一半」**——
// 比如有人把 embed 目录从 public 改成 dist，build.sh 忘了同步，于是构建成功、
// 二进制里却只有占位页（运行期 503）。真跑构建要 pnpm + node_modules，闸门里跑不动；
// 而「两处目录名必须一致」这件事，用文本比对 + 下面那条 embed 存在性断言就够。
func TestBuildPipelineEmbedsWebUI(t *testing.T) {
	root := repoRoot(t)
	sh, err := os.ReadFile(filepath.Join(root, "scripts", "build.sh"))
	if err != nil {
		t.Fatalf("读 scripts/build.sh 失败: %v", err)
	}
	s := string(sh)

	for _, want := range []string{
		"plugin/web-ui",         // 前端源码目录
		"internal/webui/public", // 拷贝目标 = embed 目录
		"pnpm run build",        // 前端构建命令
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("scripts/build.sh 缺少 Web UI 构建管线关键片段 %q（REQ-UI-NF-EMBED-PIPE）", want)
		}
	}
}

// TestWebUIEmbedDirIsNotGitIgnored 锁住 embed 目录不被 gitignore 吞掉。
//
// 背景：仓库根 .gitignore 有全局 `**/dist/` 规则。若 embed 目录沿用 dist/ 命名，
// 干净检出里该目录为空 → `//go:embed all:public`（或 dist）编译失败。
// 现口径：embed 目录叫 `public`（不被 `**/dist/` 命中），且只忽略真实产物、
// 保留 `_placeholder.html`（保证任何检出都能编译）。本测试把这条钉死。
func TestWebUIEmbedDirIsNotGitIgnored(t *testing.T) {
	root := repoRoot(t)
	gi, err := os.ReadFile(filepath.Join(root, ".gitignore"))
	if err != nil {
		t.Fatalf("读 .gitignore 失败: %v", err)
	}
	s := string(gi)

	// 1) embed 目录下的真实产物必须被忽略（否则 dist 产物会污染仓库）
	if !strings.Contains(s, "internal/webui/public/*") {
		t.Fatal(".gitignore 应忽略 internal/webui/public/*（前端构建产物不入库）")
	}
	// 2) 占位页必须例外保留（否则干净检出 embed 目录为空 → 编译失败）
	if !strings.Contains(s, "!internal/webui/public/_placeholder.html") {
		t.Fatal(".gitignore 应保留 !internal/webui/public/_placeholder.html（保证 embed 恒可编译）")
	}
	// 3) embed 目录不得被全局 dist 规则命中
	if strings.Contains(s, "internal/webui/dist") {
		t.Fatal("embed 目录不应再叫 dist（会被全局 **/dist/ 规则吞掉）")
	}
}
