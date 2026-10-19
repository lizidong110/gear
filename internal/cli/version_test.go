package cli

// version_test.go：`gear version` / `gear doctor` / 未知子命令提示。
//
// 起因（2026-10-03 外部实测）：一次 `unknown command "secret"` 让人无法判断是
// 「命令不存在」还是「二进制旧 / PATH 指错」，只能重建碰运气。这三个用例把
// 「能自查」这件事钉住：版本自报、落点自报、报错自带判据。

import (
	"bytes"
	"strings"
	"testing"

	"gear/internal/buildinfo"
)

// TestVersionCmdPrintsIdentityAndPaths：version 必须自报身份 + 落点。
//
// 断言两类内容：版本身份（含 commit/构建时间）与工作落点（GEAR_HOME/registry/
// 密钥库…）。后者同样重要——「我在操作哪个库」和「我是谁」是同一类问题，
// 而此前 CLI 一个都不答。
func TestVersionCmdPrintsIdentityAndPaths(t *testing.T) {
	var out bytes.Buffer
	root := newRootCmd()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"version"})
	if err := root.Execute(); err != nil {
		t.Fatalf("gear version 应成功: %v", err)
	}
	txt := out.String()
	for _, want := range []string{"gear ", "commit", "构建时间", "二进制", "GEAR_HOME", "registry", "密钥库"} {
		if !strings.Contains(txt, want) {
			t.Fatalf("version 输出缺 %q：\n%s", want, txt)
		}
	}
	// 未注入版本号时必须**明说**，不能让人把 dev 当成发布构建（不谎报）。
	if !buildinfo.Injected() && !strings.Contains(txt, "未注入版本号") {
		t.Fatalf("dev 版本必须标成「未注入版本号」：\n%s", txt)
	}
}

// TestRootCmdHasVersionFlag：`--version` 与 `gear version` 同源。
//
// 此前 `gear --version` 报 unknown flag，是「二进制旧了还是功能没有」无法判定的直接原因。
func TestRootCmdHasVersionFlag(t *testing.T) {
	var out bytes.Buffer
	root := newRootCmd()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"--version"})
	if err := root.Execute(); err != nil {
		t.Fatalf("gear --version 应可用: %v", err)
	}
	if !strings.HasPrefix(out.String(), "gear ") {
		t.Fatalf("--version 输出应为一行版本，实际:\n%s", out.String())
	}
}

// TestUnknownCommandHintCarriesVersion：未知子命令的报错必须自带判据。
//
// 判据是「当前二进制是哪一版 + 在哪」；没有它，智能体与人都只能重建碰运气。
func TestUnknownCommandHintCarriesVersion(t *testing.T) {
	hint := unknownCommandHint(errUnknownCommandForTest{})
	if hint == "" {
		t.Fatal("unknown command 应给出提示")
	}
	for _, want := range []string{"gear version", "scripts/build.sh"} {
		if !strings.Contains(hint, want) {
			t.Fatalf("提示缺 %q：%s", want, hint)
		}
	}
	// 负向：别的错误不得被当成「未知子命令」处理（否则所有报错都拖一段版本提示）。
	if got := unknownCommandHint(errOtherForTest{}); got != "" {
		t.Fatalf("非 unknown command 错误不该给版本提示，实际: %s", got)
	}
	if got := unknownCommandHint(nil); got != "" {
		t.Fatalf("nil 错误不该给提示，实际: %s", got)
	}
}

type errUnknownCommandForTest struct{}

func (errUnknownCommandForTest) Error() string {
	return `unknown command "secret" for "gear"`
}

type errOtherForTest struct{}

func (errOtherForTest) Error() string { return "some other failure" }

// TestDoctorCmdRunsAndReports：doctor 必须能在 CLI 侧跑起来并给出结论。
//
// 判据与 MCP 的 gear_doctor 同一份（internal/doctor），故这里只锁「CLI 侧接得上 +
// 输出含关键段落」，不重复测每一项检查（那些在 internal/doctor 的用例里）。
func TestDoctorCmdRunsAndReports(t *testing.T) {
	regPath := tempDB(t)
	var out bytes.Buffer
	root := newRootCmd()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"--registry", regPath, "doctor"})
	err := root.Execute()
	txt := out.String()
	for _, want := range []string{"registry", "扩展配置", "AI", "应用台账"} {
		if !strings.Contains(txt, want) {
			t.Fatalf("doctor 输出缺 %q：\n%s", want, txt)
		}
	}
	// 有告警时返回哨兵 error（Execute 据此只置退出码、不打印重复文案）。
	if err != nil && err != errSilentFailure {
		t.Fatalf("doctor 只应返回告警哨兵，实际: %v", err)
	}
}

// TestDoctorCmdSilentFailureIsNotPrintedAsError：哨兵 error 不该被 cobra 打进输出。
//
// 否则「自检跑完了、有告警」会变成一大段没人看的错误堆；告警本身已在报告里。
func TestDoctorCmdSilentFailureIsNotPrintedAsError(t *testing.T) {
	if !strings.Contains(errSilentFailure.Error(), "告警") {
		t.Fatalf("哨兵 error 的文案应说清是告警，实际: %s", errSilentFailure)
	}
}
