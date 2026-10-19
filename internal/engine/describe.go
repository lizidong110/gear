package engine

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"gear/internal/sidecar/describe"
)

// 本文件实现「侧车自描述扫描」（2026-10-09）：
// extensions.yaml 里写 `describe: true` 的 ext，其 `methods` 与 `schemas` 不再手写，
// 而是加载时跑一次 `<cmd> --describe` 从侧车自己那里取回来。
//
// 为什么值得做（不是为了少写字）：此前每个侧车的参数语义要在**两处**维护——
// 真正读参数的 sidecar 源码，与给静态校验/agent 看的 extensions.yaml schemas 段。
// 两处漂移不报错，只在 agent 照着过期描述组装入参时才炸，而 extensions_audit.py
// 只审得到 internal/ext/*.go（进程内扩展），侧车天然在它的视野之外。
// 自描述把方向反过来：**描述跟着实现走**，改 handler 时描述就在同一个包里。

// describeMaxBytes 限制 stdout 体量：自描述不该是几 MB 的文档，
// 越界即判为「这个侧车把 --describe 当成别的什么了」并报错。
const describeMaxBytes = 4 << 20

// describeTimeout 是单次自描述扫描的上限。
//
// 侧车被要求「带 --describe 立刻打印并退出」，正常情况下是毫秒级；
// 给到 10s 是容忍冷启动（如脚本语言写的侧车首跑要解解释器）。
//
// 是 var 不是 const：测试要把它调到毫秒级才能验证「侧车不退出」这条 fail-closed 路径，
// 否则一个超时用例就要真等 10 秒。
var describeTimeout = 10 * time.Second

// describeCache 按「可执行文件 + 它的 mtime + size」缓存扫描结果。
//
// 缓存键**必须带上 mtime/size**：这正是 2026-10-08 那次事故的形态——MCP 面长期跑着
// 一个 10-03 编译的旧二进制，而源码早已改过。若只按 cmd 字符串缓存，重编后的侧车
// 会继续用旧的自描述，于是「加了方法却看不见」再次静默发生。
var (
	describeMu    sync.Mutex
	describeCache = map[string]describe.Spec{}
)

// ApplySidecarDescribes 就地补全 specs 里声明了 `describe: true` 的扩展的方法与 schema。
//
// fail-closed（一条都不放过，因为 describe:true 是作者**显式承诺**「我的侧车会自描述」）：
//   - 同时写了 schemas → 报错（两份描述必然漂移，不允许并存）；
//   - cmd 为空 / 二进制不存在 / 超时 / 输出不合法 → 报错并点名原因；
//   - yaml 里写了 methods 但与侧车返回的不一致 → 报错并点名差异；
//   - 侧车返回的 schemas 没覆盖它的 methods → 报错。
//
// 未按名字排序前先收集：map 迭代顺序随机会让报错文案抖动。
func ApplySidecarDescribes(specs map[string]ExtSpec) error {
	names := make([]string, 0, len(specs))
	for name, spec := range specs {
		if spec.Describe {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	for _, name := range names {
		spec := specs[name]
		if len(spec.Schemas) > 0 {
			return fmt.Errorf("扩展 %q 声明了 describe: true，就不该再写 schemas 段"+
				"（两份描述必然漂移；要么删掉 schemas 交给侧车自描述，要么去掉 describe: true）", name)
		}
		if len(spec.Cmd) == 0 {
			return fmt.Errorf("扩展 %q 声明了 describe: true 却没有 cmd（无从扫描）", name)
		}
		d, err := ProbeSidecarDescribe(spec.Cmd)
		if err != nil {
			return fmt.Errorf("扩展 %q 自描述扫描失败: %w", name, err)
		}
		if len(spec.Methods) == 0 {
			spec.Methods = d.Methods
		} else if diff := diffStringSets(spec.Methods, d.Methods); diff != "" {
			return fmt.Errorf("扩展 %q 的 methods 与侧车自描述不一致（%s）："+
				"要么删掉 extensions.yaml 里的 methods 全交给侧车，要么改到一致", name, diff)
		}
		spec.Schemas = describeToSchemas(d)
		specs[name] = spec
	}
	return nil
}

// ProbeSidecarDescribe 跑一次 `<cmd...> --describe` 并解析它的自描述。
//
// 导出的原因：CLI 的 `gear ext describe <name>` 走同一条扫描路径——
// 「人工核对看到的结果」与「内核加载时用的结果」必须是同一次探测逻辑的产物，
// 否则又是一个两边漂移的口子。
func ProbeSidecarDescribe(cmd []string) (describe.Spec, error) {
	if len(cmd) == 0 || cmd[0] == "" {
		return describe.Spec{}, fmt.Errorf("cmd 为空")
	}
	exe := cmd[0]
	if key, ok := describeCacheKey(exe); ok {
		describeMu.Lock()
		cached, hit := describeCache[key]
		describeMu.Unlock()
		if hit {
			return cached, nil
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), describeTimeout)
	defer cancel()
	c := exec.CommandContext(ctx, exe, append(append([]string(nil), cmd[1:]...), describe.Flag)...)
	var out, errb bytes.Buffer
	c.Stdout = &out
	c.Stderr = &errb
	runErr := c.Run()

	if ctx.Err() == context.DeadlineExceeded {
		return describe.Spec{}, fmt.Errorf("%s 带 %s 跑超过 %v 未退出（自描述应当打印后立刻退出）",
			exe, describe.Flag, describeTimeout)
	}
	if runErr != nil {
		return describe.Spec{}, fmt.Errorf("跑 %s %s 失败: %v%s",
			exe, describe.Flag, runErr, stderrHint(errb.String()))
	}
	if out.Len() > describeMaxBytes {
		return describe.Spec{}, fmt.Errorf("%s 的自描述输出 %d 字节，超过 %d 上限（它把 %s 当成别的功能了？）",
			exe, out.Len(), describeMaxBytes, describe.Flag)
	}
	spec, err := describe.Parse(out.Bytes())
	if err != nil {
		return describe.Spec{}, fmt.Errorf("%s 的 %s 输出不合格式: %w", exe, describe.Flag, err)
	}

	if key, ok := describeCacheKey(exe); ok {
		describeMu.Lock()
		describeCache[key] = spec
		describeMu.Unlock()
	}
	return spec, nil
}

// describeToSchemas 把自描述的方法 schema 转成引擎的 MethodSchema。
//
// 派生字段（InvokeForm / LuaCall）**不在这里填**：它们是给能力目录看的渲染产物，
// 由 withInvokeForm 在目录构建时统一补，提前填会让「声明值」被「派生值」污染。
func describeToSchemas(d describe.Spec) map[string]MethodSchema {
	out := make(map[string]MethodSchema, len(d.Schemas))
	for m, s := range d.Schemas {
		ms := MethodSchema{Description: s.Description}
		if len(s.Parameters) > 0 {
			ms.Parameters = make(map[string]ParamSchema, len(s.Parameters))
			for p, ps := range s.Parameters {
				ms.Parameters[p] = ParamSchema{
					Type:        ps.Type,
					Description: ps.Description,
					Required:    ps.Required,
					Enum:        ps.Enum,
				}
			}
		}
		out[m] = ms
	}
	return out
}

// diffStringSets 描述两份字符串集合的差异（只在报错文案里用）。
// 返回空串表示「内容相同（顺序无关）」。
func diffStringSets(want, got []string) string {
	a, b := map[string]bool{}, map[string]bool{}
	for _, s := range want {
		a[s] = true
	}
	for _, s := range got {
		b[s] = true
	}
	var onlyWant, onlyGot []string
	for s := range a {
		if !b[s] {
			onlyWant = append(onlyWant, s)
		}
	}
	for s := range b {
		if !a[s] {
			onlyGot = append(onlyGot, s)
		}
	}
	if len(onlyWant) == 0 && len(onlyGot) == 0 {
		return ""
	}
	sort.Strings(onlyWant)
	sort.Strings(onlyGot)
	parts := []string{}
	if len(onlyWant) > 0 {
		parts = append(parts, "配置有而侧车没有: "+strings.Join(onlyWant, ","))
	}
	if len(onlyGot) > 0 {
		parts = append(parts, "侧车有而配置没有: "+strings.Join(onlyGot, ","))
	}
	return strings.Join(parts, "；")
}

// resetDescribeCache 清空扫描缓存（测试用：各用例要用不同的假侧车角色，
// 不清会让上一个用例的结果被下一个用例命中，测试之间互相污染）。
func resetDescribeCache() {
	describeMu.Lock()
	defer describeMu.Unlock()
	describeCache = map[string]describe.Spec{}
}

// describeCacheKey 用「路径 + mtime + 大小」当缓存键。
// stat 失败时返回 ok=false（此时不缓存也不读缓存——拿不到指纹就不该复用）。
func describeCacheKey(exe string) (string, bool) {
	st, err := os.Stat(exe)
	if err != nil {
		return "", false
	}
	return fmt.Sprintf("%s|%d|%d", exe, st.ModTime().UnixNano(), st.Size()), true
}

func stderrHint(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return "；stderr: " + s
}
