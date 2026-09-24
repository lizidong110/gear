package engine

// purefunc.go —— 纯函数核心原子（time / template / crypto）。
//
// 分类（ATOM_ROADMAP.md §0「三类原子能力」）：本文件是其中第二类「纯函数核心原子」的实现——
// 内核内建、始终可用、无外部依赖的纯计算工具。
//
//   - **不进 capability 门禁**：`pureFuncPrimitives` 刻意不登记进 `cap.KnownPrimitives`
//     （纯函数没有「宿主能力边界」可言，要求 Tool 声明 capabilities 违背直觉）；口径与官方
//     `json`（lua.go 里无条件注入）一致。
//   - **但仍是一等调用目标**：Flow step 可写 `call: crypto.hash`（注册期即静态校验），
//     Lua 里可写 `ctx.crypto.hash({...})`，REST / MCP 的能力目录同样列出（kind=pure）。
//     路径见 stepcall.go（resolveStepCall / primitiveHost）与 ai_agent.go 的 buildActionEntry。
//
// 三条口径（针对本项目踩过的坑定死）：
//  1. **单具名 table 入参**：与 `ctx.ext.*` 同款 `ctx.time.now({ format = "date" })`。
//     不用位置参数——位置参数要在 `builtinParamOrder` 里再存一份「参数顺序」副本，
//     两份清单必然漂移（`subscribe` 曾因此漏在能力目录外）。
//  2. **实现自带入参校验**：Lua 直调路径不经过 `validatePrimitiveArgs`，实现即最后一道闸；
//     缺必填 / 未知枚举 / 非法长度一律报错，绝不静默兜底（红线：不静默降级）。
//  3. **密钥由调用方传入**：crypto 不读 env、不自带口令；密钥来自 `ctx.secret.get(name)`
//     或 Flow 里的 `${secrets.x}`（REQ-TMPL-SECRETS 的既有口径），原子本身只做纯计算。

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"hash"
	"strings"
	"time"

	// 内嵌 IANA 时区库：`tz` 参数支持 "Asia/Shanghai" 这类名字，不依赖宿主是否装了
	// zoneinfo（否则同一份 Flow 在有 GOROOT 的开发机与裸二进制上行为不同——那种
	// 「环境决定语义」正是要避免的坑）。代价约 450KB 体积。
	_ "time/tzdata"
)

// pureFuncPrimitives 是「纯函数核心原子」子集的名字集合：
// 内核内建、始终可用、**不受 capability 门禁**（Lua 侧无条件注入；Flow 直调无需声明 capabilities）。
// 判定三问见 ATOM_ROADMAP.md §0：通用计算、不绑产品、无外部世界。
var pureFuncPrimitives = map[string]bool{
	"time":     true,
	"template": true,
	"crypto":   true,
}

// gatedPrimitiveOrder 是**受 capability 门禁**的内核原语（内容与 cap.KnownPrimitives 一一对应，
// 由 TestGatedPrimitiveOrderMatchesCapEnums 守着不漂移），顺序即能力目录/提示词里的展示顺序。
var gatedPrimitiveOrder = []string{"http", "call", "db", "secret", "event", "subscribe"}

// purePrimitiveOrder 是纯函数核心原子的展示顺序（不受门禁）。
var purePrimitiveOrder = []string{"time", "template", "crypto"}

// allBuiltinPrimitives 返回内核原语的**单一来源**清单（门禁 7 个 + 纯函数 3 个）。
// 能力目录（ext.registry.capabilities / ext.ai.capabilities / REST / MCP）与给 LLM 的
// capabilitiesText / flowCapabilitiesText 全部从这里取，不再各写一份字面量列表。
func allBuiltinPrimitives() []string {
	out := make([]string, 0, len(gatedPrimitiveOrder)+len(purePrimitiveOrder))
	out = append(out, gatedPrimitiveOrder...)
	out = append(out, purePrimitiveOrder...)
	return out
}

// primitiveKind 返回一个内核原语在能力目录里的分类：
// builtin（受门禁）/ pure（纯函数原子，始终可用）。
func primitiveKind(prim string) string {
	if pureFuncPrimitives[prim] {
		return "pure"
	}
	return "builtin"
}

// ---- 方法级 schema（与实现同文件：新增/改名方法时，schema、Lua 绑定、静态校验三处不会走散）----

// pureTimeSchemas 是 ctx.time.* 的方法级参数 schema。
var pureTimeSchemas = map[string]MethodSchema{
	"now": {Description: "当前时间，返回格式化字符串（默认 RFC3339）", Parameters: map[string]ParamSchema{
		"format": {Type: "string", Enum: timeFormatNames, Description: "输出格式（可选，默认 rfc3339）"},
		"tz":     {Type: "string", Description: "时区（可选，默认 Local；可用 UTC 或 Asia/Shanghai 这类 IANA 名）"},
	}},
	"parse": {Description: "把时间字符串解析为 Unix 秒（数字），用于时间比较/差值", Parameters: map[string]ParamSchema{
		"value":  {Type: "string", Description: "待解析的时间字符串（format=unix/unix_ms 时可传数字串）", Required: true},
		"format": {Type: "string", Enum: timeFormatNames, Description: "输入格式（可选，默认 rfc3339）"},
		"tz":     {Type: "string", Description: "按该时区解释无时区信息的输入（可选，默认 Local）"},
	}},
	"format": {Description: "把 Unix 时间戳格式化为字符串（与 now 同格式枚举）", Parameters: map[string]ParamSchema{
		"unix":   {Type: "number", Description: "Unix 秒（与 ms 二选一，必填其一）"},
		"ms":     {Type: "number", Description: "Unix 毫秒（与 unix 二选一，必填其一）"},
		"format": {Type: "string", Enum: timeFormatNames, Description: "输出格式（可选，默认 rfc3339）"},
		"tz":     {Type: "string", Description: "时区（可选，默认 Local）"},
	}},
	"sleep": {Description: "阻塞等待若干秒（上限 60s，超限即报错，避免挂死编排）", Parameters: map[string]ParamSchema{
		"seconds": {Type: "number", Description: "秒数（可含小数；0 < seconds <= 60）", Required: true},
	}},
}

// pureTemplateSchemas 是 ctx.template.* 的方法级参数 schema。
var pureTemplateSchemas = map[string]MethodSchema{
	"render": {Description: "渲染 ${vars.x} 模板（与引擎 step input 同一套引用语法，只解析 vars）", Parameters: map[string]ParamSchema{
		"text": {Type: "string", Description: "模板文本（可含 ${vars.a.b} 引用）", Required: true},
		"vars": {Type: "object", Description: "变量表（渲染时按路径下钻取值；引用缺失即报错，不静默填空）"},
	}},
}

// pureCryptoSchemas 是 ctx.crypto.* 的方法级参数 schema。
var pureCryptoSchemas = map[string]MethodSchema{
	"hash": {Description: "摘要（md5/sha1/sha256/sha512），返回 hex 或 base64 文本", Parameters: map[string]ParamSchema{
		"algo":     {Type: "string", Enum: []string{"md5", "sha1", "sha256", "sha512"}, Description: "摘要算法", Required: true},
		"data":     {Type: "string", Description: "待摘要的数据", Required: true},
		"encoding": {Type: "string", Enum: []string{"hex", "base64"}, Description: "输出编码（可选，默认 hex）"},
	}},
	"hmac": {Description: "HMAC 签名（sha1/sha256/sha512），返回 hex 或 base64 文本", Parameters: map[string]ParamSchema{
		"algo":     {Type: "string", Enum: []string{"sha1", "sha256", "sha512"}, Description: "HMAC 算法", Required: true},
		"key":      {Type: "string", Description: "密钥材料（经 ctx.secret.get 或 ${secrets.x} 取，别写明文）", Required: true},
		"data":     {Type: "string", Description: "被签名的数据（是待签名原文，不是它的摘要）", Required: true},
		"encoding": {Type: "string", Enum: []string{"hex", "base64"}, Description: "输出编码（可选，默认 hex）"},
	}},
	"hmac_verify": {Description: "校验 HMAC 签名（恒定时比较），返回 bool", Parameters: map[string]ParamSchema{
		"algo":      {Type: "string", Enum: []string{"sha1", "sha256", "sha512"}, Description: "HMAC 算法（须与签名方一致）", Required: true},
		"key":       {Type: "string", Description: "密钥材料", Required: true},
		"data":      {Type: "string", Description: "被签名的数据", Required: true},
		"signature": {Type: "string", Description: "待校验的签名文本", Required: true},
		"encoding":  {Type: "string", Enum: []string{"hex", "base64"}, Description: "签名文本的编码（可选，默认 hex）"},
	}},
	"encrypt": {Description: "AES-GCM 加密（密钥长度决定 AES-128/192/256），返回 base64(nonce||密文)", Parameters: map[string]ParamSchema{
		"key":          {Type: "string", Description: "密钥材料（16/24/32 字节；经 secret 取，别写明文）", Required: true},
		"plaintext":    {Type: "string", Description: "明文", Required: true},
		"key_encoding": {Type: "string", Enum: []string{"utf8", "hex", "base64"}, Description: "key 的编码（可选，默认 utf8）"},
		"aad":          {Type: "string", Description: "附加认证数据（可选）"},
	}},
	"decrypt": {Description: "AES-GCM 解密（与 encrypt 对偶），返回明文字符串", Parameters: map[string]ParamSchema{
		"key":          {Type: "string", Description: "密钥材料（16/24/32 字节）", Required: true},
		"ciphertext":   {Type: "string", Description: "base64(nonce||密文)（encrypt 的输出）", Required: true},
		"key_encoding": {Type: "string", Enum: []string{"utf8", "hex", "base64"}, Description: "key 的编码（可选，默认 utf8）"},
		"aad":          {Type: "string", Description: "附加认证数据（可选，须与加密时一致）"},
	}},
	"random": {Description: "密码学安全随机字节（hex 文本），用于 nonce/盐", Parameters: map[string]ParamSchema{
		"bytes": {Type: "number", Description: "字节数（可选，默认 32；1..1024）"},
	}},
	"base64_encode": {Description: "base64 编码（与摘要/HMAC 输出配套，Lua 侧无原生 base64）", Parameters: map[string]ParamSchema{
		"data":    {Type: "string", Description: "待编码数据", Required: true},
		"variant": {Type: "string", Enum: []string{"std", "url"}, Description: "标准或 URL-safe 字母表（可选，默认 std；均带 padding）"},
	}},
	"base64_decode": {Description: "base64 解码，返回原文（解码失败即报错，不返回空串）", Parameters: map[string]ParamSchema{
		"data":    {Type: "string", Description: "base64 文本", Required: true},
		"variant": {Type: "string", Enum: []string{"std", "url"}, Description: "字母表（可选，默认 std）"},
	}},
}

// timeFormatNames 是时间格式枚举（now/parse/format 共用同一份，避免三处各写一遍）。
var timeFormatNames = []string{"rfc3339", "unix", "unix_ms", "datetime", "date", "time"}

// timeLayouts 把格式枚举映射到 Go 布局串（"unix"/"unix_ms" 是数值形态，不在此表）。
var timeLayouts = map[string]string{
	"rfc3339":  time.RFC3339,
	"datetime": "2006-01-02 15:04:05",
	"date":     "2006-01-02",
	"time":     "15:04:05",
}

// callPureFunc 是纯函数原子的**唯一调度点**：
// Lua 绑定（lua.go 由 schema 的键自动生成方法）与 Flow 直调 entry（ai_agent.go 单 table 形态）
// 都落到这里，因此「目录声明的方法」与「真的能跑的方法」由构造保证一致。
func callPureFunc(prim, method string, args map[string]interface{}) (interface{}, error) {
	if args == nil {
		args = map[string]interface{}{}
	}
	switch prim {
	case "time":
		return pureTimeCall(method, args)
	case "template":
		return pureTemplateCall(method, args)
	case "crypto":
		return pureCryptoCall(method, args)
	}
	return nil, fmt.Errorf("未知的纯函数原子 %q（合法值：%s）", prim, strings.Join(purePrimitiveOrder, ", "))
}

// ---- 入参辅助：全部显式报错（纯函数原子的 Lua 直调不过 schema 校验，实现即最后一道闸）----

// pureStr 取必填字符串参数；缺失 / 类型不符 / 空串一律报错。
func pureStr(args map[string]interface{}, name string) (string, error) {
	v, ok := args[name]
	if !ok || v == nil {
		return "", fmt.Errorf("缺少必填参数 %q", name)
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("参数 %q 必须是字符串，实际 %T", name, v)
	}
	if s == "" {
		return "", fmt.Errorf("参数 %q 不能为空串", name)
	}
	return s, nil
}

// pureOptStr 取可选字符串参数（缺省返回 def）。
func pureOptStr(args map[string]interface{}, name, def string) (string, error) {
	v, ok := args[name]
	if !ok || v == nil {
		return def, nil
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("参数 %q 必须是字符串，实际 %T", name, v)
	}
	return s, nil
}

// pureEnum 取枚举参数：未知值即报错并列出合法值（红线：不静默放过未知枚举）。
func pureEnum(args map[string]interface{}, name, def string, allowed []string) (string, error) {
	v, err := pureOptStr(args, name, def)
	if err != nil {
		return "", err
	}
	for _, a := range allowed {
		if v == a {
			return v, nil
		}
	}
	return "", fmt.Errorf("参数 %q 的值 %q 非法（合法值：%s）", name, v, strings.Join(allowed, ", "))
}

// pureNum 取可选数字参数（整数或浮点；JSON 解出的数字是 float64）。
func pureNum(args map[string]interface{}, name string) (float64, bool, error) {
	v, ok := args[name]
	if !ok || v == nil {
		return 0, false, nil
	}
	switch x := v.(type) {
	case float64:
		return x, true, nil
	case int:
		return float64(x), true, nil
	case int64:
		return float64(x), true, nil
	}
	return 0, false, fmt.Errorf("参数 %q 必须是数字，实际 %T", name, v)
}

// pureLoc 解析时区名（缺省 Local）；未知时区名报错，不静默回退 UTC。
func pureLoc(args map[string]interface{}) (*time.Location, error) {
	name, err := pureOptStr(args, "tz", "Local")
	if err != nil {
		return nil, err
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("未知时区 %q（可用 UTC / Local / Asia/Shanghai 这类 IANA 名）: %w", name, err)
	}
	return loc, nil
}

// ---- ctx.time.* ----

func pureTimeCall(method string, args map[string]interface{}) (interface{}, error) {
	switch method {
	case "now":
		return timeNow(args)
	case "parse":
		return timeParse(args)
	case "format":
		return timeFormat(args)
	case "sleep":
		return timeSleep(args)
	}
	return nil, fmt.Errorf("time 没有方法 %q（合法方法：now, parse, format, sleep）", method)
}

func timeNow(args map[string]interface{}) (interface{}, error) {
	loc, err := pureLoc(args)
	if err != nil {
		return nil, err
	}
	return layoutTime(time.Now().In(loc), args)
}

func timeParse(args map[string]interface{}) (interface{}, error) {
	value, err := pureStr(args, "value")
	if err != nil {
		return nil, err
	}
	layout, err := pureEnum(args, "format", "rfc3339", timeFormatNames)
	if err != nil {
		return nil, err
	}
	switch layout {
	case "unix", "unix_ms":
		var f float64
		if _, err := fmt.Sscanf(strings.TrimSpace(value), "%g", &f); err != nil {
			return nil, fmt.Errorf("按 %s 解析 %q 失败: 不是数字", layout, value)
		}
		if layout == "unix_ms" {
			f /= 1000
		}
		return int64(f), nil
	}
	loc, err := pureLoc(args)
	if err != nil {
		return nil, err
	}
	t, err := time.ParseInLocation(timeLayouts[layout], strings.TrimSpace(value), loc)
	if err != nil {
		return nil, fmt.Errorf("按 %s 解析 %q 失败: %w", layout, value, err)
	}
	return t.Unix(), nil
}

func timeFormat(args map[string]interface{}) (interface{}, error) {
	sec, okSec, err := pureNum(args, "unix")
	if err != nil {
		return nil, err
	}
	ms, okMs, err := pureNum(args, "ms")
	if err != nil {
		return nil, err
	}
	switch {
	case okSec && okMs:
		return nil, fmt.Errorf("unix 与 ms 只能给一个")
	case okSec:
	case okMs:
		sec = ms / 1000
	default:
		return nil, fmt.Errorf("缺少必填参数 unix 或 ms（二选一）")
	}
	loc, err := pureLoc(args)
	if err != nil {
		return nil, err
	}
	return layoutTime(time.Unix(int64(sec), 0).In(loc), args)
}

// layoutTime 按 format/tz 语义把一个时刻渲染成结果值：
// unix/unix_ms 返回**数字**（供算术），其余返回字符串。
func layoutTime(t time.Time, args map[string]interface{}) (interface{}, error) {
	format, err := pureEnum(args, "format", "rfc3339", timeFormatNames)
	if err != nil {
		return nil, err
	}
	switch format {
	case "unix":
		return t.Unix(), nil
	case "unix_ms":
		return t.UnixMilli(), nil
	}
	return t.Format(timeLayouts[format]), nil
}

// maxSleepSeconds 是 sleep 的上限：编排里的等待应当显式且有限，
// 无上限会让一个坏 Flow 永久占住 worker（与 defaultExecTimeout 一起构成双重闸门）。
const maxSleepSeconds = 60

func timeSleep(args map[string]interface{}) (interface{}, error) {
	sec, ok, err := pureNum(args, "seconds")
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("缺少必填参数 \"seconds\"")
	}
	if sec <= 0 || sec > maxSleepSeconds {
		return nil, fmt.Errorf("seconds 必须在 (0, %d] 之间，实际 %v", maxSleepSeconds, sec)
	}
	time.Sleep(time.Duration(sec * float64(time.Second)))
	return true, nil
}

// ---- ctx.template.* ----

func pureTemplateCall(method string, args map[string]interface{}) (interface{}, error) {
	switch method {
	case "render":
		return templateRender(args)
	}
	return nil, fmt.Errorf("template 没有方法 %q（合法方法：render）", method)
}

// templateRender 用引擎既有的 `${...}` 语法渲染一段文本，取值只来自参数里的 vars 表。
//
// 为什么只支持 vars 前缀：`${steps.*}` 是引擎 step input 展开期（有执行状态）才有的语义，
// `${secrets.*}` 需要宿主密钥源——让一个「纯函数原子」去读密钥会让它越过 secret 门禁。
// 两者在此**显式报错**并给出替代写法（`ctx.secret.get` + vars），而不是留空或忽略，
// 否则用户会拿到一段「少了几段内容」的结果还不知道为什么（红线：不静默降级）。
func templateRender(args map[string]interface{}) (interface{}, error) {
	text, err := pureStr(args, "text")
	if err != nil {
		return nil, err
	}
	vars := map[string]interface{}{}
	if v, ok := args["vars"]; ok && v != nil {
		m, ok := v.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("参数 \"vars\" 必须是 object，实际 %T", v)
		}
		vars = m
	}
	refs, err := scanTemplateRefs(text)
	if err != nil {
		return nil, err
	}
	// 从后往前替换：前面的替换会改变后面引用的下标（与 expandString 同款处理）。
	for i := len(refs) - 1; i >= 0; i-- {
		r := refs[i]
		if r.Prefix != prefixVars {
			return nil, fmt.Errorf("template.render 只支持 ${%s.x} 引用（遇到 ${%s}）："+
				"${steps.*} 由引擎在 step input 展开期解析，${secrets.*} 请先用 ctx.secret.get 取到再放进 vars", prefixVars, r.Raw)
		}
		if len(r.Path) == 0 {
			return nil, fmt.Errorf("vars 引用缺少键名: %s", r.Raw)
		}
		val, err := walk(vars, r.Path)
		if err != nil {
			return nil, fmt.Errorf("渲染 ${%s} 失败: %w", r.Raw, err)
		}
		text = text[:r.Start] + fmt.Sprintf("%v", val) + text[r.End+1:]
	}
	return text, nil
}

// ---- ctx.crypto.* ----

func pureCryptoCall(method string, args map[string]interface{}) (interface{}, error) {
	switch method {
	case "hash":
		return cryptoHash(args)
	case "hmac":
		return cryptoHMAC(args)
	case "hmac_verify":
		return cryptoHMACVerify(args)
	case "encrypt":
		return cryptoEncrypt(args)
	case "decrypt":
		return cryptoDecrypt(args)
	case "random":
		return cryptoRandom(args)
	case "base64_encode":
		return cryptoBase64(args, true)
	case "base64_decode":
		return cryptoBase64(args, false)
	}
	return nil, fmt.Errorf("crypto 没有方法 %q（合法方法：hash, hmac, hmac_verify, encrypt, decrypt, random, base64_encode, base64_decode）", method)
}

// sumAlgos / macAlgos 是摘要与 HMAC 的算法枚举（与 schema 的 Enum 一一对应）。
var sumAlgos = []string{"md5", "sha1", "sha256", "sha512"}
var macAlgos = []string{"sha1", "sha256", "sha512"}

// newHasher 按算法名构造 hash.Hash；未知算法报错（不静默回退某个默认算法）。
func newHasher(algo string, allowed []string) (hash.Hash, error) {
	switch algo {
	case "md5":
		return md5.New(), nil
	case "sha1":
		return sha1.New(), nil
	case "sha256":
		return sha256.New(), nil
	case "sha512":
		return sha512.New(), nil
	}
	return nil, fmt.Errorf("不支持的算法 %q（合法值：%s）", algo, strings.Join(allowed, ", "))
}

func cryptoHash(args map[string]interface{}) (interface{}, error) {
	algo, err := pureEnum(args, "algo", "", sumAlgos)
	if err != nil {
		return nil, err
	}
	data, err := pureStr(args, "data")
	if err != nil {
		return nil, err
	}
	enc, err := pureEnum(args, "encoding", "hex", []string{"hex", "base64"})
	if err != nil {
		return nil, err
	}
	h, err := newHasher(algo, sumAlgos)
	if err != nil {
		return nil, err
	}
	_, _ = h.Write([]byte(data))
	return encodeSum(h.Sum(nil), enc), nil
}

func cryptoHMAC(args map[string]interface{}) (interface{}, error) {
	algo, err := pureEnum(args, "algo", "", macAlgos)
	if err != nil {
		return nil, err
	}
	key, err := pureStr(args, "key")
	if err != nil {
		return nil, err
	}
	data, err := pureStr(args, "data")
	if err != nil {
		return nil, err
	}
	enc, err := pureEnum(args, "encoding", "hex", []string{"hex", "base64"})
	if err != nil {
		return nil, err
	}
	sum, err := hmacSum(algo, key, data)
	if err != nil {
		return nil, err
	}
	return encodeSum(sum, enc), nil
}

// macFactory 返回 HMAC 算法对应的 hash 构造函数。
// 刻意不做「未知算法回退 sha256」的兜底：hmac 的算法名选错必须报错，
// 静默换算法会让签名在本地「看起来成功」、到对端验签才炸（红线：不静默降级）。
func macFactory(algo string) (func() hash.Hash, error) {
	switch algo {
	case "sha1":
		return sha1.New, nil
	case "sha256":
		return sha256.New, nil
	case "sha512":
		return sha512.New, nil
	}
	return nil, fmt.Errorf("不支持的 HMAC 算法 %q（合法值：%s）", algo, strings.Join(macAlgos, ", "))
}

// hmacSum 计算 HMAC 原始字节（hmac/hmac_verify 共用，保证签名与校验同一份算法映射）。
func hmacSum(algo, key, data string) ([]byte, error) {
	factory, err := macFactory(algo)
	if err != nil {
		return nil, err
	}
	mac := hmac.New(factory, []byte(key))
	_, _ = mac.Write([]byte(data))
	return mac.Sum(nil), nil
}

func cryptoHMACVerify(args map[string]interface{}) (interface{}, error) {
	algo, err := pureEnum(args, "algo", "", macAlgos)
	if err != nil {
		return nil, err
	}
	key, err := pureStr(args, "key")
	if err != nil {
		return nil, err
	}
	data, err := pureStr(args, "data")
	if err != nil {
		return nil, err
	}
	sig, err := pureStr(args, "signature")
	if err != nil {
		return nil, err
	}
	enc, err := pureEnum(args, "encoding", "hex", []string{"hex", "base64"})
	if err != nil {
		return nil, err
	}
	want, err := decodeSum(sig, enc)
	if err != nil {
		return nil, err
	}
	got, err := hmacSum(algo, key, data)
	if err != nil {
		return nil, err
	}
	return hmac.Equal(got, want), nil
}

// encodeSum / decodeSum 是「摘要文本 ↔ 字节」的唯一编解码对（hash/hmac 输出与 verify 输入共用）。
func encodeSum(sum []byte, encoding string) string {
	if encoding == "base64" {
		return base64.StdEncoding.EncodeToString(sum)
	}
	return hex.EncodeToString(sum)
}

func decodeSum(s string, encoding string) ([]byte, error) {
	if encoding == "base64" {
		b, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			return nil, fmt.Errorf("签名不是合法的 base64: %w", err)
		}
		return b, nil
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("签名不是合法的 hex: %w", err)
	}
	return b, nil
}

// keyBytes 解析密钥材料：key_encoding 决定怎么把字符串变成字节。
// 长度合法性由 aesGCM 判定（16/24/32 字节）。
func keyBytes(args map[string]interface{}) ([]byte, error) {
	key, err := pureStr(args, "key")
	if err != nil {
		return nil, err
	}
	enc, err := pureEnum(args, "key_encoding", "utf8", []string{"utf8", "hex", "base64"})
	if err != nil {
		return nil, err
	}
	switch enc {
	case "hex":
		b, err := hex.DecodeString(key)
		if err != nil {
			return nil, fmt.Errorf("key 不是合法 hex: %w", err)
		}
		return b, nil
	case "base64":
		b, err := base64.StdEncoding.DecodeString(key)
		if err != nil {
			return nil, fmt.Errorf("key 不是合法 base64: %w", err)
		}
		return b, nil
	}
	return []byte(key), nil
}

// aesGCM 按密钥长度构造 AES-GCM（16/24/32 字节 → AES-128/192/256）；其余长度直接报错。
func aesGCM(key []byte) (cipher.AEAD, error) {
	switch len(key) {
	case 16, 24, 32:
	default:
		return nil, fmt.Errorf("AES 密钥长度必须是 16/24/32 字节（对应 AES-128/192/256），实际 %d 字节", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func cryptoEncrypt(args map[string]interface{}) (interface{}, error) {
	key, err := keyBytes(args)
	if err != nil {
		return nil, err
	}
	plain, err := pureStr(args, "plaintext")
	if err != nil {
		return nil, err
	}
	aad, err := pureOptStr(args, "aad", "")
	if err != nil {
		return nil, err
	}
	aead, err := aesGCM(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("生成 nonce 失败: %w", err)
	}
	// 输出把 nonce 前置（解密侧从同一布局切分），避免调用方自己管 nonce 顺序。
	sealed := aead.Seal(nonce, nonce, []byte(plain), optAAD(aad))
	return base64.StdEncoding.EncodeToString(sealed), nil
}

func cryptoDecrypt(args map[string]interface{}) (interface{}, error) {
	key, err := keyBytes(args)
	if err != nil {
		return nil, err
	}
	ct, err := pureStr(args, "ciphertext")
	if err != nil {
		return nil, err
	}
	aad, err := pureOptStr(args, "aad", "")
	if err != nil {
		return nil, err
	}
	raw, err := base64.StdEncoding.DecodeString(ct)
	if err != nil {
		return nil, fmt.Errorf("ciphertext 不是合法 base64: %w", err)
	}
	aead, err := aesGCM(key)
	if err != nil {
		return nil, err
	}
	if len(raw) < aead.NonceSize() {
		return nil, fmt.Errorf("ciphertext 太短（%d 字节 < nonce %d 字节）", len(raw), aead.NonceSize())
	}
	nonce, body := raw[:aead.NonceSize()], raw[aead.NonceSize():]
	plain, err := aead.Open(nil, nonce, body, optAAD(aad))
	if err != nil {
		return nil, fmt.Errorf("解密失败（密钥/AAD 不匹配或密文被篡改）: %w", err)
	}
	return string(plain), nil
}

// optAAD 把空串映射成 nil（GCM 的 AAD 参数语义：nil = 无附加数据）。
func optAAD(s string) []byte {
	if s == "" {
		return nil
	}
	return []byte(s)
}

// maxRandomBytes 是随机字节上限（防止一个 Flow 一次要 100MB 熵把内存打满）。
const maxRandomBytes = 1024

func cryptoRandom(args map[string]interface{}) (interface{}, error) {
	n, ok, err := pureNum(args, "bytes")
	if err != nil {
		return nil, err
	}
	count := 32
	if ok {
		if n != float64(int64(n)) || n < 1 || n > maxRandomBytes {
			return nil, fmt.Errorf("bytes 必须是 1..%d 的整数，实际 %v", maxRandomBytes, n)
		}
		count = int(n)
	}
	buf := make([]byte, count)
	if _, err := rand.Read(buf); err != nil {
		return nil, fmt.Errorf("读取随机数失败: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

func cryptoBase64(args map[string]interface{}, encode bool) (interface{}, error) {
	name := "base64_decode"
	if encode {
		name = "base64_encode"
	}
	data, err := pureStr(args, "data")
	if err != nil {
		return nil, err
	}
	variant, err := pureEnum(args, "variant", "std", []string{"std", "url"})
	if err != nil {
		return nil, err
	}
	enc := base64.StdEncoding
	if variant == "url" {
		enc = base64.URLEncoding
	}
	if encode {
		return enc.EncodeToString([]byte(data)), nil
	}
	b, err := enc.DecodeString(data)
	if err != nil {
		return nil, fmt.Errorf("%s: 不是合法的 base64(%s): %w", name, variant, err)
	}
	return string(b), nil
}
