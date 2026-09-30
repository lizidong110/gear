package engine

import (
	"fmt"
	"regexp"
	"strings"
)

// CheckPortablePaths 对应用定义 YAML 做**可移植性静态检查**：找出其中硬编码的
// 机器相关绝对路径（与 CI 闸门 tools/portable_audit.py 第⑨项**同口径**）。
// 命中即返回致命诊断，供 gear_validate / gear_apply 在写作期拦截——
// 应用主要由智能体通过 MCP 接口编写（gear_validate 是「编译器」、gear_apply 是
// 唯一写入口），只有把检查前移到这里，才能给作者即时反馈；CI 闸门是兜底。
//
// 为什么要拦：2026-09-25 实测 mindstorm 在 apps/ 下硬编码了 25 处 `D:/gadern/gear/...`
// （db 的 dsn、process entry 的 gear 可执行文件、ext.serve 的 dir），仓库拷到别的机器即废。
// 内核已提供相对锚点，应用**不该**再写绝对路径：dsn 用相对库名、entry 用裸名、dir 用相对安装根。
func CheckPortablePaths(text string) []Diagnostic {
	var diags []Diagnostic
	for i, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		where, ok := machinePathIn(line)
		if !ok {
			continue
		}
		diags = append(diags, Diagnostic{
			Code:    CodePortablePathHardcoded,
			Field:   yamlKeyOf(line),
			Message: fmt.Sprintf("第 %d 行出现机器相关绝对路径 %q：应用定义必须可移植，否则换台机器就跑不起来", i+1, where),
			Hint: "改用内核提供的相对锚点：db 的 dsn 写相对库名（如 mindstorm.db，锚到 GEAR_APP_DATA）；" +
				"process entry 写裸名（如 \"gear serve ...\"，锚到可执行文件所在目录）；" +
				"ext.serve 的 dir 写相对安装根（如 apps/<app>/www，锚到 InstallRoot）。" +
				"详见 gear_guide(\"portability\") 与 REQ-APP-PORTABLE。",
		})
	}
	return diags
}

// machinePathIn 判断一行是否包含「机器相关绝对路径」，并返回命中的片段。
// 与 tools/portable_audit.py 同口径：① Windows 盘符绝对路径（排除 http:// 等协议串里的
// 「末字母:斜杠」假阳性，也不许前面是 /，以免 http://d/ 被误判）；② 类 Unix 机器根路径
// （首段是 d/c/e/Users/home/mnt/...），可出现在行内（如 `dir: /Users/...`、`local DSN = "/d/..."`），
// 但排除 `://` 与 `//`（协议串 / 双斜杠）里的 /x。
func machinePathIn(line string) (string, bool) {
	for _, m := range drivePathRE.FindAllStringIndex(line, -1) {
		start := m[0]
		if start == 0 {
			return line[start:m[1]], true
		}
		prev := line[start-1]
		if !isASCIILetter(prev) && prev != '/' {
			return line[start:m[1]], true
		}
	}
	for _, m := range unixRootRE.FindAllStringIndex(line, -1) {
		start := m[0]
		if start == 0 {
			return line[start:m[1]], true
		}
		prev := line[start-1]
		if prev != ':' && prev != '/' {
			return line[start:m[1]], true
		}
	}
	return "", false
}

var (
	drivePathRE = regexp.MustCompile(`[A-Za-z]:[\\/]`)
	// 首段属于已知机器根的词（Git-Bash/MSYS 把 C:/ D:/ 挂成 /c /d /e）。
	unixRootRE = regexp.MustCompile(`/(?:d|c|e|Users|home|mnt|Volumes|Applications|tmp|var|opt|srv|root)\b`)
)

func isASCIILetter(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// yamlKeyOf 从一行 YAML/Lua 里尽量抽取字段名（用于诊断的 field，便于智能体定位）。
// 取首个分隔符（: 或 =）之前的片段，再剥掉 `local ` 前缀与路径/类型修饰。
func yamlKeyOf(line string) string {
	sep := strings.IndexAny(line, ":=")
	if sep < 0 {
		return ""
	}
	key := strings.TrimSpace(line[:sep])
	key = strings.TrimPrefix(key, "local ")
	if i := strings.LastIndexAny(key, " .\t"); i >= 0 {
		key = key[i+1:]
	}
	return strings.TrimSpace(key)
}
