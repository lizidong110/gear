package cmdline

// pathflags.go：扫出「entry 里把**相对路径**传给路径类 flag」这一误用。
//
// 为什么需要它（2026-09-22 实测，同一个根因发作两次）：`--registry` 的值按**运行时 cwd**
// 解析，而 cwd 是调用方的属性、不是 gear 的属性（见 internal/gearhome 包注释的红线）。
// 于是一个写坏了的相对值不会报任何错——它只是安静地指向另一个库：
//
//	① `gear app start --registry build/reg.db`：台账落在 `<cwd>/build/`，换个 cwd 再查就「没有应用」；
//	② 应用自带 REST 面的 `web_ui.api` Tool entry 写死 `./gear.exe serve --registry ./ui-registry.db`：
//	   子进程按自己的 cwd 解析，读到的是另一次实验留下的空库 ⇒ 页面列表永远为空，日志一个字都没有。
//
// 判定必须**只有一份**，所以清单与措辞都放在这里，由 CLI（tool register）与 MCP
// （gear_validate 的 checkToolEntryResolvable）共同引用——两处各写一份必然漂移，
// 将来新增一个路径类 flag 只会补上其中一边。

import (
	"fmt"
	"path/filepath"
	"strings"

	"gear/internal/gearhome"
)

// PathValueFlags 是「取值为文件/目录路径」的 flag 名（不含前导横线）。
var PathValueFlags = []string{"registry", "apps-dir"}

// RelPathFlag 是 entry 里一个「取值为相对路径」的路径类 flag。
type RelPathFlag struct {
	Name  string // 不含前导横线，如 registry
	Value string // 原始取值，如 build/ui-registry.db
}

// ScanRelPathFlags 在已切词的 argv 里找「路径类 flag 取了相对路径」的情形。
//
// 两种写法都认（pflag 的 `--name value` 与 `--name=value`；单横线一视同仁）。
// 绝对路径不报——它与 cwd 无关，正是被推荐的写法；值缺失或为空也不报，那是别的校验的活。
func ScanRelPathFlags(argv []string, names []string) []RelPathFlag {
	if len(argv) == 0 || len(names) == 0 {
		return nil
	}
	want := make(map[string]bool, len(names))
	for _, n := range names {
		want[strings.TrimLeft(n, "-")] = true
	}
	var out []RelPathFlag
	for i := 0; i < len(argv); i++ {
		tok := argv[i]
		if len(tok) < 2 || tok[0] != '-' {
			continue
		}
		body := strings.TrimLeft(tok, "-")
		if body == "" {
			break // 单独的 "--"：其后都是位置参数，不再是 flag
		}
		name, value, hasEq := strings.Cut(body, "=")
		if !want[name] {
			continue
		}
		if !hasEq {
			if i+1 >= len(argv) {
				continue // flag 在末尾且没有值：判不了，交给其它校验
			}
			value = argv[i+1]
			i++
		}
		if value == "" || filepath.IsAbs(value) {
			continue
		}
		out = append(out, RelPathFlag{Name: name, Value: value})
	}
	return out
}

// RelPathFlagsInLine 解析一行命令后扫描；解析失败或该行走 shell 兜底时返回 nil
// （写坏的 entry / shell 行另有诊断，这里不重复报）。
func RelPathFlagsInLine(line string) []RelPathFlag {
	spec, err := Parse(line)
	if err != nil || spec.NeedsShell {
		return nil
	}
	return ScanRelPathFlags(spec.Argv, PathValueFlags)
}

// RelPathFlagHint 给出统一措辞的告警（owner 是「谁在报」，如 `tool web_ui.api`）。
//
// `--registry` 的措辞与其它 flag 不同，因为它有一个**正解**：省略它，由起服进程把生效值
// 经 GEAR_REGISTRY 注入子进程。其它路径类 flag 没有这条信道，只能建议写绝对路径。
func RelPathFlagHint(owner string, f RelPathFlag) string {
	if f.Name == "registry" {
		return fmt.Sprintf(
			"%s 的 entry 把相对路径传给了 --registry（%q）：它按**运行时 cwd** 解析，"+
				"换个 cwd 就会静默指向另一个库。建议改成绝对路径，或**直接省略该参数**——"+
				"起服进程会把生效的 registry 经 %s 注入子进程。",
			owner, f.Value, gearhome.EnvRegistry)
	}
	return fmt.Sprintf(
		"%s 的 entry 把相对路径传给了 --%s（%q）：它按**运行时 cwd** 解析，"+
			"换个 cwd 就会静默指向另一个位置。建议改成绝对路径。",
		owner, f.Name, f.Value)
}
