// Package buildinfo 是二进制版本信息的唯一落点。
//
// 存在的理由（2026-10-03 外部实测）：此前 gear **没有任何版本标识**——
// `gear --version` 报 unknown flag，`gear --help` 里也没有 version/doctor。
// 于是「我这个 gear.exe 是不是旧的 / PATH 指的是不是另一个」只能靠重建二进制碰运气，
// 一条 `unknown command "secret"` 就足以让人怀疑整套环境（实际是二进制旧了）。
//
// 三个字段用 ldflags 注入（scripts/build.sh）：
//
//	-X gear/internal/buildinfo.Version=0.1.0
//	-X gear/internal/buildinfo.Commit=<git short sha 或 none>
//	-X gear/internal/buildinfo.BuildTime=<RFC3339>
//
// **未注入时一律显示 "dev"，绝不假装有版本号**（红线：不静默降级、不谎报）。
// `gear version` 会把「dev」明确标成「未注入」，让人知道拿到的不是发布构建。
package buildinfo

// Version 是内核/CLI 的版本号（语义化）。未注入时为 "dev"。
var Version = "dev"

// Commit 是构建时的 git 短 sha；未注入或非 git 检出时为 "none"。
var Commit = "none"

// BuildTime 是构建时刻（RFC3339）。未注入时为 ""。
var BuildTime = ""

// Injected 报告版本号是否由构建脚本注入（未注入 = 不是发布构建）。
func Injected() bool { return Version != "dev" && Version != "" }
