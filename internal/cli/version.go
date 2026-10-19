package cli

// version.go：`gear version` —— 自查「我手上这个二进制是新的还是旧的」。
//
// 起因（2026-10-03 外部实测）：MCP 侧有 gear_doctor，CLI 侧却没有 version / doctor。
// 一次 `unknown command "secret"` 让人无法判断是「功能没有」还是「二进制旧 / PATH 指错」，
// 只能重建碰运气——而 gear 恰恰是那个**最容易被当成过时**的东西（它天天在改）。
//
// 所以这里给三样东西，缺一不可：
//  1. 版本身份（版本号 / commit / 构建时间，且明说是不是发布构建）；
//  2. **落点**（GEAR_HOME 与三本库的实际路径）——「我到底在操作哪个库」和「我是谁」同等重要；
//  3. 侧车产物清单（带构建时间）——侧车没有版本号，如实给时间而不是编一个版本出来。

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"gear/internal/buildinfo"
	"gear/internal/gearhome"
)

// versionCmd 构造 `gear version`。
func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "版本自查：二进制版本、构建时间、GEAR_HOME 与各库落点、侧车产物",
		Long: "打印当前 gear 二进制的版本身份与工作落点。\n\n" +
			"用途：出现「命令不存在 / 参数不认识」这类怀疑二进制过时的症状时，先跑它——\n" +
			"若显示 dev（未注入版本号），说明这不是发布构建，重建（bash scripts/build.sh）后再判断。\n" +
			"环境自检（侧车缺失、端口占用、台账遗留）用 `gear doctor`。",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "gear %s", buildinfo.Version)
			if !buildinfo.Injected() {
				// 不谎报：dev 就是「构建脚本没注入版本号」，不是「版本 0.0.0」。
				fmt.Fprintf(out, "（dev = 构建时未注入版本号，不是发布构建）")
			}
			fmt.Fprintln(out)
			fmt.Fprintf(out, "  commit      %s\n", buildinfo.Commit)
			fmt.Fprintf(out, "  构建时间    %s\n", orNone(buildinfo.BuildTime))
			fmt.Fprintf(out, "  二进制      %s\n", selfPath())

			fmt.Fprintf(out, "\n工作落点（cwd 无关，均由 gearhome 解析）\n")
			fmt.Fprintf(out, "  GEAR_HOME   %s\n", gearhome.Home())
			fmt.Fprintf(out, "  registry    %s\n", gearhome.RegistryPath())
			fmt.Fprintf(out, "  执行记录    %s\n", gearhome.ExecDBPath())
			fmt.Fprintf(out, "  配置库      %s\n", gearhome.ConfigDBPath())
			fmt.Fprintf(out, "  密钥库      %s\n", gearhome.SecretsDBPath())

			if s := sidecarArtifacts(); s != "" {
				fmt.Fprintf(out, "\n侧车产物（%s）\n%s", "侧车没有版本号，如实给构建时间", s)
			}
			fmt.Fprintf(out, "\n环境自检用：gear doctor\n")
			return nil
		},
	}
}

// orNone 空串显示 none（不留空行让人猜是不是漏了）。
func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "none（未注入）"
	}
	return s
}

// selfPath 返回当前可执行文件路径；取不到时如实说取不到。
func selfPath() string {
	p, err := os.Executable()
	if err != nil {
		return "取不到（" + err.Error() + "）"
	}
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	return p
}

// sidecarArtifacts 列出产物目录里的 *-sidecar 及其修改时间/大小。
//
// 只列**存在**的产物，不猜缺哪个（缺侧车的判据在 `gear doctor`——那里还会比对
// extensions.yaml 的声明）。此处的作用是回答「我这套侧车是什么时候构建的」。
func sidecarArtifacts() string {
	var lines []string
	for _, dir := range gearhome.ArtifactDirs() {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.Contains(e.Name(), "-sidecar") {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			lines = append(lines, fmt.Sprintf("  %-24s %s  %d 字节\n",
				e.Name(), info.ModTime().Format("2006-01-02 15:04:05"), info.Size()))
		}
	}
	if len(lines) == 0 {
		return ""
	}
	sort.Strings(lines)
	return strings.Join(lines, "")
}

// unknownCommandHint 在 cobra 报「未知子命令」时补一句可判定的信息。
//
// 单一用途：把「二进制旧了 / PATH 指错」与「命令真的不存在」这两种情况分开——
// 没有这句时，智能体与人都只能靠重建二进制碰运气（2026-10-03 实测的卡死点）。
// 拼装与 `gear version` 同源，避免两处说法漂移。
func unknownCommandHint(err error) string {
	if err == nil || !strings.Contains(err.Error(), "unknown command") {
		return ""
	}
	cur := buildinfo.Version
	if !buildinfo.Injected() {
		cur = "dev（未注入版本号，无法判断是否最新）"
	}
	return fmt.Sprintf(
		"提示：当前二进制是 gear %s（%s）。若该子命令在更新的版本里才有，请重建（bash scripts/build.sh）；"+
			"先用 `gear version` 确认版本与落点，用 `gear help` 看当前二进制实际支持哪些子命令。",
		cur, selfPath())
}
