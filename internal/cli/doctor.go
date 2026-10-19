package cli

// doctor.go：`gear doctor` —— 环境自检的 CLI 入口。
//
// 判据本体在 internal/doctor，与 MCP 的 gear_doctor **同一份实现**（不是各写一套）：
// 「AI 能不能通」「侧车齐不齐」「谁的端口」这些问题，从命令行和从 MCP 看到的答案必须一致。
// 这份检查此前只有 MCP 侧，于是从命令行排查的人看不到它——而恰好是命令行能做的事
// （看路径、重建、改配置）都在 CLI 侧，这是一处真实的不对称。
//
// 退出码：有告警时返回 1（可脚本化），全绿返回 0。

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"gear/internal/doctor"
)

// doctorAppsDir 覆盖应用台账目录（默认 <registry 同目录>/apps）；留空 = 用默认规则。
var doctorAppsDir string

// doctorCmd 构造 `gear doctor`。
func doctorCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "环境自检：registry 计数、扩展与侧车就绪、AI 凭据、应用台账与端口占用",
		Long: "自检**环境**（不是 YAML）：registry 里的对象数、扩展声明与侧车是否就绪、" +
			"AI 凭据有没有配（只看有没有，不回显明文）、应用台账里的遗留进程与端口占用。\n\n" +
			"用途：区分「YAML 写错了」与「环境没准备好」——扩展侧车没 build、凭据没配、端口被占、" +
			"台账有遗留进程，这四类都会表现成「应用跑起来就炸」。\n" +
			"MCP 侧的 gear_doctor 用的是同一份判据，两面结论必然一致。\n\n" +
			"有告警时退出码为 1（便于脚本判断）。",
		Args: cobra.NoArgs,
		RunE: runDoctor,
	}
	cmd.Flags().StringVar(&doctorAppsDir, "apps-dir", "",
		"应用台账目录（默认 <registry 同目录>/apps）")
	return cmd
}

func runDoctor(cmd *cobra.Command, _ []string) error {
	reg, err := openRegistry()
	if err != nil {
		return err
	}
	defer reg.Close()

	eng, err := cliEngine(reg)
	if err != nil {
		return err
	}
	rep, err := doctor.Check(doctor.Options{
		Registry:     reg,
		Engine:       eng,
		RegistryPath: registryPath,
		AppsDir:      doctorAppsDir,
	})
	if err != nil {
		return fmt.Errorf("自检失败: %w", err)
	}
	out := cmd.OutOrStdout()
	fmt.Fprint(out, rep.Text())
	if len(rep.Warnings) > 0 {
		// 退出码留给 shell 判断，不靠人读 stderr 猜（脚本里没人看告警）。
		cmd.SilenceUsage = true
		return errSilentFailure
	}
	return nil
}

// errSilentFailure 表示「自检跑完了，但有告警」——**不是**执行失败。
// Execute 见到它就只置退出码、不打印错误文案（告警本身已经在报告里说完了）。
var errSilentFailure = errors.New("doctor: 发现告警（详见上方输出）")
