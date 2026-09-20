// Package ext 是 Gear 内置扩展原语的 sidecar 实现（Go 单二进制载体）。
//
// 协议与原来的 Python sidecar 完全一致（引擎零改动即可切换）：
//
//	stdin 读一行 {"method": "<方法>", "args": {<参数>}}
//	stdout 写末行 {"result": <结果>} 或 {"error": "<错误信息>"}
//
// 引擎的 CommandTransport 每次起子进程执行 `gear ext <name>`，本包按 name+method 路由到
// 具体 handler。这样「Python 装镜像包」的痛点消失——所有扩展随 gear 主二进制编译成一
// 个静态文件，运行期零依赖；同时保留「用户仍可写任意语言 sidecar」的可插拔约定
// （用户自己的 ext 在 extensions.yaml 里写自己的 cmd 即可，不冲突）。
package ext

import (
	"encoding/json"
	"fmt"
	"io"
)

// Handler 是单个 ext 方法的实现：吃 args map，返回结果或 error。
type Handler func(args map[string]interface{}) (interface{}, error)

// registry 是 ext 名（短名，不带 ext. 前缀）→ method → Handler。
var registry = map[string]map[string]Handler{}

// register 把 handler 登记进 registry（各 ext 文件在 init 中调用）。
func register(extName, method string, h Handler) {
	if registry[extName] == nil {
		registry[extName] = map[string]Handler{}
	}
	registry[extName][method] = h
}

// Run 是 `gear ext <name>` 子命令入口：从 in 读 {method,args}，路由 handler，写末行 JSON 到 out。
// 总是返回 nil（错误已写进 stdout JSON），保证子进程退出码 0、与 Python 版行为一致。
func Run(name string, in io.Reader, out io.Writer) error {
	// 任何 handler panic 都转成 {"error":"panic: ..."} 返回，而不是让 sidecar 子进程崩溃
	// （exit 非 0 会让引擎报 "sidecar 退出异常" 且丢失错误细节）。fail-closed 优先于崩溃。
	defer func() {
		if r := recover(); r != nil {
			writeErr(out, fmt.Sprintf("panic: %v", r))
		}
	}()
	raw, err := io.ReadAll(in)
	if err != nil {
		writeErr(out, "读取 stdin 失败: "+err.Error())
		return nil
	}
	var req struct {
		Method string                 `json:"method"`
		Args   map[string]interface{} `json:"args"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		writeErr(out, "请求解析失败: "+err.Error())
		return nil
	}
	methods, ok := registry[name]
	if !ok {
		writeErr(out, fmt.Sprintf("未知扩展 %q", name))
		return nil
	}
	h, ok := methods[req.Method]
	if !ok {
		writeErr(out, fmt.Sprintf("扩展 %q 不支持的方法 %q", name, req.Method))
		return nil
	}
	res, err := h(req.Args)
	if err != nil {
		writeErr(out, err.Error())
		return nil
	}
	writeOK(out, res)
	return nil
}

func writeOK(out io.Writer, res interface{}) {
	b, _ := json.Marshal(map[string]interface{}{"result": res})
	fmt.Fprintln(out, string(b))
}

func writeErr(out io.Writer, msg string) {
	b, _ := json.Marshal(map[string]string{"error": msg})
	fmt.Fprintln(out, string(b))
}

// ---- 通用参数助手（统一处理 Lua/JSON 传来的弱类型值）----
func intOrDef(m map[string]interface{}, k string, def func() int) int {
	if v, ok := m[k]; ok && v != nil {
		if s, ok := v.(int); ok {
			return s
		}
	}
	return def()
}

func intOr(m map[string]interface{}, k string, def int) int {
	return intOrDef(m, k, func() int { return def })
}

func strOrDef(m map[string]interface{}, k string, def func() string) string {
	if v, ok := m[k]; ok && v != nil {
		if s, ok := v.(string); ok {
			return s
		}
		return fmt.Sprint(v)
	}
	return def()
}

func strOr(m map[string]interface{}, k, def string) string {
	return strOrDef(m, k, func() string {
		return def
	})
}

func boolOrDef(m map[string]interface{}, k string, def func() bool) bool {
	if v, ok := m[k]; ok && v != nil {
		switch t := v.(type) {
		case bool:
			return t
		case float64:
			return t != 0
		}
	}
	return def()
}

func boolOr(m map[string]interface{}, k string, def bool) bool {
	return boolOrDef(m, k, func() bool {
		return def
	})
}

func mapOr(m map[string]interface{}, k string) map[string]interface{} {
	if v, ok := m[k]; ok {
		if mm, ok := v.(map[string]interface{}); ok {
			return mm
		}
	}
	return map[string]interface{}{}
}

func arrOr(m map[string]interface{}, k string) []interface{} {
	if v, ok := m[k]; ok {
		if a, ok := v.([]interface{}); ok {
			return a
		}
	}
	return nil
}

// argToBytes 把 body/value 之类参数统一成字节：字符串直接用，其它 JSON 序列化。
func argToBytes(v interface{}) []byte {
	if v == nil {
		return []byte("")
	}
	if s, ok := v.(string); ok {
		return []byte(s)
	}
	b, _ := json.Marshal(v)
	return b
}

// isJSON 粗略判断字节是否为 JSON 对象/数组（用于推断 content-type）。
func isJSON(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	if b[0] != '{' && b[0] != '[' {
		return false
	}
	var x interface{}
	return json.Unmarshal(b, &x) == nil
}
