// Package describe 定义「侧车自描述」的线格式与开关约定。
//
// 背景（2026-10-09）：ext.* 的方法级参数 schema 此前**只能**写在 extensions.yaml 的
// `schemas` 段里，于是每个侧车的参数语义都得在两处维护：
//
//	plugin/xxx-sidecar/main.go   ← 真正读参数的地方
//	plugin/extensions.yaml       ← 给静态校验与 agent 看的描述
//
// 这两处天然会漂移，而漂移不报错——只在 agent 照着过期描述组装入参时才炸。
// （extensions_audit.py 只能审 internal/ext/*.go 那一类进程内扩展，审不到侧车。）
//
// 自描述把这个方向反过来：**描述跟着实现走**。侧车自己声明「我有哪些方法、每个方法
// 吃什么参数」，extensions.yaml 里写一行 `describe: true` 就不再写 schemas，
// 加载时内核跑一次 `<cmd> --describe` 拿回来（EXT 自描述扫描）。
//
// 形态刻意保持**任意语言可参与**：协议就是「带 `--describe` 跑一次，stdout 打一段 JSON」，
// 不要求侧车是 Go 写的、也不 import 本包（本包只是给 Go 侧车省事 + 给内核定格式）。
package describe

import (
	"encoding/json"
	"fmt"
	"os"
)

// Flag / EnvKey 是触发自描述的两种方式：命令行 `--describe`，或环境变量 GEAR_DESCRIBE=1。
// 两种都要，因为：
//   - 命令行参数会被某些载体吞掉（如经 shell 拼出来的 entry）；
//   - 环境变量在「不想改 argv」的场合更稳（如 Windows 服务包装）。
const (
	Flag   = "--describe"
	EnvKey = "GEAR_DESCRIBE"
)

// Param 是单个参数的 schema（与 engine.ParamSchema 的 JSON 形状对齐，
// 少数字段本包暂不需要，故不重复定义：不用的字段不该出现在协议里）。
type Param struct {
	Type        string   `json:"type"`                  // string / number / boolean / object / array
	Description string   `json:"description,omitempty"` // 人话说明（给 agent 看）
	Required    bool     `json:"required,omitempty"`    // 是否必填
	Enum        []string `json:"enum,omitempty"`        // 枚举约束
}

// Method 是一个方法的描述。
type Method struct {
	Description string           `json:"description,omitempty"`
	Parameters  map[string]Param `json:"parameters,omitempty"`
}

// Spec 是侧车的完整自描述。
type Spec struct {
	Methods []string          `json:"methods"`
	Schemas map[string]Method `json:"schemas"`
}

// Requested 判断本次运行是否被要求输出自描述。
func Requested() bool {
	for _, a := range os.Args[1:] {
		if a == Flag {
			return true
		}
	}
	switch os.Getenv(EnvKey) {
	case "1", "true", "TRUE", "yes":
		return true
	}
	return false
}

// Emit 把自描述以 JSON 写到 stdout 并**立即退出**（退出码 0）。
//
// 为什么是「打印后退出」而不是「返回字符串」：自描述是一次性的进程级动作，
// 调用方（内核）拿完就该结束这次进程；留着继续跑会让它变成半个侧车。
func Emit(s Spec) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(s); err != nil {
		fmt.Fprintf(os.Stderr, "自描述序列化失败: %v\n", err)
		os.Exit(1)
	}
	os.Exit(0)
}

// EmitIfRequested 在 Requested() 为真时打印自描述并退出；否则返回 false 让调用方正常启动。
//
// 用法（侧车 main 的第一行）：
//
//	func main() {
//	    describe.EmitIfRequested(mongoDescribe())
//	    ... 正常 serve ...
//	}
func EmitIfRequested(s Spec) bool {
	if !Requested() {
		return false
	}
	Emit(s)
	return true
}

// Parse 解析侧车 stdout 里的自描述 JSON（内核侧用）。
//
// fail-closed：非空校验在内核做（这里只管「是不是合法 JSON、形状对不对」），
// 但**空 methods 直接报错**——一个不说自己有啥方法的自描述等于没描述，
// 让它静默通过会让「声明了 describe:true 却什么也没校验」成为新的静默缺口。
func Parse(data []byte) (Spec, error) {
	var s Spec
	if err := json.Unmarshal(data, &s); err != nil {
		return Spec{}, fmt.Errorf("不是合法的 JSON: %w", err)
	}
	if len(s.Methods) == 0 {
		return Spec{}, fmt.Errorf("methods 为空（自描述必须至少声明一个方法）")
	}
	seen := map[string]bool{}
	for _, m := range s.Methods {
		if m == "" {
			return Spec{}, fmt.Errorf("methods 含空方法名")
		}
		if seen[m] {
			return Spec{}, fmt.Errorf("methods 含重复方法名 %q", m)
		}
		seen[m] = true
	}
	for m := range s.Schemas {
		if !seen[m] {
			return Spec{}, fmt.Errorf("schemas 里的 %q 不在 methods 里（两处必须一一对应）", m)
		}
	}
	return s, nil
}
