package engine

// purefunc.go —— 纯函数核心原子（time / template / crypto / id / validate）。
//
// 分类（ATOM_ROADMAP.md §0「三类原子能力」）：本文件是其中第二类「纯函数核心原子」的实现——
// 内核内建、始终可用、无外部依赖的纯计算工具。
//
// **准入判据（四轴，新增族/方法前先过一遍）**——注意「纯」不是数学意义上的纯，
// `time.now` 读时钟、`crypto.random` 读熵，它们仍在本文件里。真实口径是两条：
//   A. **不碰需授权的资源**：不碰 db / 文件 / 网络 / 密钥库 / env。
//      （读时钟与熵算「环境事实」，不算资源——这就是 `time.now` 与 jwt 校验 `exp` 仍属此列的原因。）
//   B. **不持跨调用状态**：不存会话、句柄、锁、长驻缓存；每次调用都从入参重算。
// 另两轴是成本而非安全：C 依赖要轻（标准库或已在 go.mod）、D 语义要冻结（RFC 标准而非厂商实现）。
// 任一轴不成立就别放这里——该走 ext 侧车（如 OIDC 握手）或受门禁原语（如 db）。
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
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/x509"
	"encoding/asn1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"hash"

	"github.com/santhosh-tekuri/jsonschema/v5"
	"math/big"
	"strconv"
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
	"id":       true,
	"validate": true,
}

// gatedPrimitiveOrder 是**受 capability 门禁**的内核原语（内容与 cap.KnownPrimitives 一一对应，
// 由 TestGatedPrimitiveOrderMatchesCapEnums 守着不漂移），顺序即能力目录/提示词里的展示顺序。
var gatedPrimitiveOrder = []string{"http", "call", "db", "secret", "event", "subscribe", "config"}

// purePrimitiveOrder 是纯函数核心原子的展示顺序（不受门禁）。
var purePrimitiveOrder = []string{"time", "template", "crypto", "id", "validate"}

// allBuiltinPrimitives 返回内核原语的**单一来源**清单（门禁 7 个 + 纯函数 5 个）。
// （门禁 7 个 = http/call/db/secret/event/subscribe/config，与 cap.KnownPrimitives 一一对应。）
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
	"jwt_decode": {Description: "解析 JWT 的 header 与 claims（**不验签**，仅供查看；判断真伪必须用 jwt_verify）", Parameters: map[string]ParamSchema{
		"token": {Type: "string", Description: "JWT 令牌（header.payload.signature 三段）", Required: true},
	}},
	"jwt_verify": {Description: "校验 JWT 签名与 claims（exp/nbf/iss/aud），**失败即报错**（fail-closed，不返回 false 让调用方漏判）。密钥由调用方传（PEM 公钥或 JWKS），不接受 alg=none", Parameters: map[string]ParamSchema{
		"token":        {Type: "string", Description: "JWT 令牌", Required: true},
		"key":          {Type: "string", Description: "密钥材料：HS* 用共享密钥；RS*/ES*/EdDSA 用 PEM 公钥（-----BEGIN PUBLIC KEY-----）"},
		"jwks":         {Type: "object", Description: "JWKS 密钥集 {\"keys\":[{kty,kid,n,e,crv,x,y}]}；token 带 kid 时按 kid 选中，无 kid 且多把钥匙即报错（不猜）"},
		"key_encoding": {Type: "string", Enum: []string{"utf8", "hex", "base64"}, Description: "对称密钥 key 的编码（可选，默认 utf8；非对称密钥忽略此项）"},
		"algs":         {Type: "array", Description: "允许的算法（可选）。缺省按密钥类型推断：PEM/JWKS 公钥只允许 RS*/ES*/EdDSA，共享密钥只允许 HS*。★ 显式给也只能**收窄**不能跨族——杜绝 RS256→HS256 的算法混淆攻击"},
		"iss":          {Type: "string", Description: "期望的签发者（不给则不校验）"},
		"aud":          {Type: "string", Description: "期望的受众（不给则不校验；claims.aud 为数组时须包含本值）"},
		"leeway":       {Type: "number", Description: "时钟偏移容忍秒数（可选，默认 0，即不宽容）"},
	}},
	"jwt_sign": {Description: "签发 JWT（HS*/RS*/ES*/EdDSA）。密钥由调用方传，不读 env", Parameters: map[string]ParamSchema{
		"claims":       {Type: "object", Description: "claims 表（如 sub/iss/aud/exp/iat）", Required: true},
		"key":          {Type: "string", Description: "密钥材料：HS* 用共享密钥；RS*/ES*/EdDSA 用 PEM 私钥", Required: true},
		"alg":          {Type: "string", Enum: jwtAlgs, Description: "签名算法", Required: true},
		"key_encoding": {Type: "string", Enum: []string{"utf8", "hex", "base64"}, Description: "对称密钥 key 的编码（可选，默认 utf8）"},
		"kid":          {Type: "string", Description: "写进 header 的密钥 ID（可选）"},
		"ttl":          {Type: "number", Description: "有效期秒数（可选）；给则自动写 exp，claims 里已有 exp 时不覆盖"},
	}},
	"password_hash": {Description: "口令哈希（返回 PHC 串 `$pbkdf2-sha256$i=...$salt$hash`，含算法/迭代/盐，可直接存库）。**零新增依赖**（标准库 hmac 迭代）；盐缺省随机生成", Parameters: map[string]ParamSchema{
		"password":   {Type: "string", Description: "明文口令", Required: true},
		"algo":       {Type: "string", Enum: passwordAlgos, Description: "哈希算法（可选，默认 pbkdf2-sha256）"},
		"iterations": {Type: "number", Description: "迭代次数（可选，默认 210000；上限 1000000，防一个 Flow 把 CPU 打满）"},
		"salt":       {Type: "string", Description: "盐（可选，缺省随机 16 字节；仅测试或迁移旧数据时显式给）"},
	}},
	"password_verify": {Description: "校验口令与哈希是否匹配，返回 bool。★ 与 jwt_verify 的口径**刻意不同**：口令不对返回 **false**（那是正常业务分支，不是异常）；只有哈希串本身非法才报错", Parameters: map[string]ParamSchema{
		"password": {Type: "string", Description: "明文口令", Required: true},
		"hash":     {Type: "string", Description: "password_hash 的输出（PHC 串）", Required: true},
	}},
}

// pureIDSchemas 是 ctx.id.* 的方法级参数 schema（标识符生成，零依赖）。
var pureIDSchemas = map[string]MethodSchema{
	"uuid": {Description: "生成 UUID **v4**（密码学安全随机，RFC 4122）", Parameters: map[string]ParamSchema{
		"format": {Type: "string", Enum: []string{"plain", "braced", "urn", "hex"}, Description: "输出形态（可选，默认 plain；hex = 无连字符）"},
	}},
	"nanoid": {Description: "生成 URL 安全的短 ID（默认 21 字符，NanoID 同款字母表）", Parameters: map[string]ParamSchema{
		"size":     {Type: "number", Description: "长度（可选，默认 21；1..256）"},
		"alphabet": {Type: "string", Description: "字母表（可选，默认 A-Za-z0-9_-；不得重复字符、长度 ≥2）"},
	}},
	"code": {Description: "生成随机码（短信验证码 / 邀请码 / 兑换码），**无模偏倚**", Parameters: map[string]ParamSchema{
		"length":  {Type: "number", Description: "长度（可选，默认 6；1..64）"},
		"charset": {Type: "string", Enum: idCharsetNames, Description: "字符集（可选，默认 digits）"},
	}},
}

// pureValidateSchemas 是 ctx.validate.* 的方法级参数 schema。
//
// 注意 `data` 没有 Type：它可以是任意 JSON 形态（对象/数组/标量），而 ParamSchema 的
// Type 枚举里没有「任意」，留空即不约束 —— 刻意不写成 object，否则标量数据会被静态校验挡掉。
var pureValidateSchemas = map[string]MethodSchema{
	"json_schema": {Description: "按 JSON Schema 校验数据，返回 `{valid, errors}`。★ 口径：数据不合法**不是错误**（返回 valid=false + 明细，用于把错误展示给用户）；只有 **schema 本身非法**才报错", Parameters: map[string]ParamSchema{
		"schema": {Type: "object", Description: "JSON Schema（如 {type:\"object\", required:[\"name\"]}）", Required: true},
		"data":   {Description: "待校验的数据（任意类型：对象 / 数组 / 标量）", Required: true},
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
	case "id":
		return pureIDCall(method, args)
	case "validate":
		return pureValidateCall(method, args)
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
	case "jwt_decode":
		return cryptoJWTDecode(args)
	case "jwt_verify":
		return cryptoJWTVerify(args)
	case "jwt_sign":
		return cryptoJWTSign(args)
	case "password_hash":
		return cryptoPasswordHash(args)
	case "password_verify":
		return cryptoPasswordVerify(args)
	}
	return nil, fmt.Errorf("crypto 没有方法 %q（合法方法：hash, hmac, hmac_verify, encrypt, decrypt, random, base64_encode, base64_decode, jwt_decode, jwt_verify, jwt_sign, password_hash, password_verify）", method)
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

// ---- ctx.crypto.* JWT（RFC 7515 / 7518）----
//
// 为什么进内核而不是 ext 侧车：算法集是 RFC 冻结的、标准库就有（crypto/rsa|ecdsa|ed25519）、
// 无需长驻、密钥由调用方当参数传入 ⇒ 四轴判据（A✗无副作用 / B✗无跨调用状态 / C轻 / D冻结）全过。
// 判据与归类见 ENTERPRISE_PRIMITIVE_GAP.md §3.7。
//
// 三条安全口径（都是 JWT 的经典坑，在此钉死，不靠调用方自觉）：
//  1. **alg 不由令牌决定**：header.alg 只用来「在允许集里挑一个」，允许集由**密钥类型**推断
//     （共享密钥 → HS*；PEM/JWKS 公钥 → RS*/ES*/EdDSA）。显式给 `algs` 也只能**收窄**不能跨族，
//     否则 RS256→HS256「拿公钥当 HMAC 密钥」的算法混淆攻击就能过。
//  2. **alg=none 一律拒**：没有参数可以打开它。
//  3. **失败即报错**（fail-closed）：不返回 `{valid=false}` —— 那种形态最容易被调用方漏判，
//     一个被忽略的返回值就是一个敞开的门。

// jwtAlgs 是支持的 JWS 算法（RFC 7518 §3）。
var jwtAlgs = []string{"HS256", "HS384", "HS512", "RS256", "RS384", "RS512", "ES256", "ES384", "ES512", "EdDSA"}

// jwtHMACHash / jwtRSAHash / jwtECHash 把算法名映射到摘要。
// 签名与验签共用同一份映射 —— 否则「签名用 256、验签用 384」这类漂移只能到对端才炸。
var jwtHMACHash = map[string]func() hash.Hash{
	"HS256": sha256.New,
	"HS384": sha512.New384,
	"HS512": sha512.New,
}
var jwtRSAHash = map[string]crypto.Hash{
	"RS256": crypto.SHA256,
	"RS384": crypto.SHA384,
	"RS512": crypto.SHA512,
}
var jwtECHash = map[string]crypto.Hash{
	"ES256": crypto.SHA256,
	"ES384": crypto.SHA384,
	"ES512": crypto.SHA512,
}

// jwtKeyClass 是密钥类别，它决定**默认允许哪些算法**（安全口径 1 的落点）。
type jwtKeyClass int

const (
	jwtClassSymmetric  jwtKeyClass = iota // 共享密钥（HS*）
	jwtClassAsymmetric                    // 公钥（RS*/ES*/EdDSA）
)

// jwtClassAlgs 返回该类别默认允许的算法集。
func (c jwtKeyClass) jwtClassAlgs() []string {
	if c == jwtClassSymmetric {
		return []string{"HS256", "HS384", "HS512"}
	}
	return []string{"RS256", "RS384", "RS512", "ES256", "ES384", "ES512", "EdDSA"}
}

// jwtB64 / jwtB64Decode 是 JWT 专用的 base64url（**无 padding**，与 base64_encode 的 std 不同）。
func jwtB64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// jwtParse 拆开并解析令牌；返回三段原文、header、claims 与签名字节。
func jwtParse(token string) (parts []string, header, claims map[string]interface{}, sig []byte, err error) {
	parts = strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, nil, nil, nil, fmt.Errorf("JWT 必须是 header.payload.signature 三段，实际 %d 段", len(parts))
	}
	hb, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("header 不是合法 base64url: %w", err)
	}
	cb, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("payload 不是合法 base64url: %w", err)
	}
	sig, err = base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("signature 不是合法 base64url: %w", err)
	}
	header = map[string]interface{}{}
	if err := json.Unmarshal(hb, &header); err != nil {
		return nil, nil, nil, nil, fmt.Errorf("header 不是合法 JSON: %w", err)
	}
	claims = map[string]interface{}{}
	if err := json.Unmarshal(cb, &claims); err != nil {
		return nil, nil, nil, nil, fmt.Errorf("claims 不是合法 JSON: %w", err)
	}
	return parts, header, claims, sig, nil
}

// ---- jwt_decode ----

func cryptoJWTDecode(args map[string]interface{}) (interface{}, error) {
	token, err := pureStr(args, "token")
	if err != nil {
		return nil, err
	}
	_, header, claims, _, err := jwtParse(token)
	if err != nil {
		return nil, fmt.Errorf("jwt_decode: %w", err)
	}
	return map[string]interface{}{"header": header, "claims": claims}, nil
}

// ---- jwt_verify ----

func cryptoJWTVerify(args map[string]interface{}) (interface{}, error) {
	token, err := pureStr(args, "token")
	if err != nil {
		return nil, err
	}
	parts, header, claims, sig, err := jwtParse(token)
	if err != nil {
		return nil, fmt.Errorf("jwt_verify: %w", err)
	}

	alg, _ := header["alg"].(string)
	if alg == "" {
		return nil, fmt.Errorf("jwt_verify: header 缺少 alg")
	}
	if strings.EqualFold(alg, "none") {
		return nil, fmt.Errorf("jwt_verify: 拒绝 alg=none（未签名的令牌一律不接受）")
	}

	// 密钥来源决定类别，类别决定默认算法集（安全口径 1）。
	key, class, err := jwtResolveVerifyKey(args, header)
	if err != nil {
		return nil, err
	}
	allowed, err := jwtNarrowAlgs(class.jwtClassAlgs(), args)
	if err != nil {
		return nil, err
	}
	if !jwtContains(jwtAlgs, alg) {
		return nil, fmt.Errorf("jwt_verify: 不支持的 alg %q（合法值：%s）", alg, strings.Join(jwtAlgs, ", "))
	}
	if !jwtContains(allowed, alg) {
		return nil, fmt.Errorf("jwt_verify: alg %q 不在允许的算法 %v 内"+
			"（★ 算法集由密钥类型决定、不由令牌决定；共享密钥只能 HS*，公钥只能 RS*/ES*/EdDSA）", alg, allowed)
	}

	signing := []byte(parts[0] + "." + parts[1])
	if err := jwtVerifySignature(alg, key, signing, sig); err != nil {
		return nil, fmt.Errorf("jwt_verify: 签名校验失败: %w", err)
	}
	if err := jwtCheckClaims(claims, args); err != nil {
		return nil, err
	}
	out := map[string]interface{}{"header": header, "claims": claims}
	if sub, ok := claims["sub"].(string); ok {
		out["subject"] = sub
	}
	return out, nil
}

// jwtNarrowAlgs 把「密钥类别的默认算法集」与显式给的 algs 求交集。
// 只能收窄、不能跨族 —— 交集为空即报错（不静默回退到默认集，那等于跨族攻击又开了）。
func jwtNarrowAlgs(def []string, args map[string]interface{}) ([]string, error) {
	given, err := jwtStringList(args, "algs")
	if err != nil {
		return nil, err
	}
	if len(given) == 0 {
		return def, nil
	}
	kept := make([]string, 0, len(given))
	for _, a := range given {
		if jwtContains(def, a) {
			kept = append(kept, a)
		}
	}
	if len(kept) == 0 {
		return nil, fmt.Errorf("jwt_verify: algs %v 与密钥类型允许的 %v 无交集"+
			"（对称密钥只能 HS*，公钥只能 RS*/ES*/EdDSA）", given, def)
	}
	return kept, nil
}

// jwtResolveVerifyKey 解析验签密钥：优先 jwks，其次 PEM 公钥，都没有则当共享密钥。
func jwtResolveVerifyKey(args map[string]interface{}, header map[string]interface{}) (interface{}, jwtKeyClass, error) {
	if raw, ok := args["jwks"]; ok && raw != nil {
		pub, err := jwtPublicFromJWKS(raw, header)
		if err != nil {
			return nil, 0, err
		}
		return pub, jwtClassAsymmetric, nil
	}
	key, err := pureStr(args, "key")
	if err != nil {
		return nil, 0, fmt.Errorf("jwt_verify: 缺少密钥（key 与 jwks 必给其一，密钥由调用方传，不读 env）")
	}
	if strings.Contains(key, "-----BEGIN") {
		pub, err := jwtParsePublicPEM([]byte(key))
		if err != nil {
			return nil, 0, fmt.Errorf("jwt_verify: %w", err)
		}
		return pub, jwtClassAsymmetric, nil
	}
	enc, err := pureEnum(args, "key_encoding", "utf8", []string{"utf8", "hex", "base64"})
	if err != nil {
		return nil, 0, err
	}
	secret, err := jwtDecodeKeyMaterial(key, enc)
	if err != nil {
		return nil, 0, err
	}
	return secret, jwtClassSymmetric, nil
}

// jwtDecodeKeyMaterial 按 key_encoding 把对称密钥字符串还原成字节。
func jwtDecodeKeyMaterial(key, enc string) ([]byte, error) {
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

// jwtParsePublicPEM 解析公钥 PEM（PKIX 优先，其次 PKCS1）。
func jwtParsePublicPEM(raw []byte) (interface{}, error) {
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, fmt.Errorf("不是合法的 PEM 块（缺少 -----BEGIN ... -----）")
	}
	if key, err := x509.ParsePKIXPublicKey(block.Bytes); err == nil {
		switch k := key.(type) {
		case *rsa.PublicKey, *ecdsa.PublicKey, ed25519.PublicKey:
			return k, nil
		}
		return nil, fmt.Errorf("不支持的公钥类型 %T", key)
	}
	if key, err := x509.ParsePKCS1PublicKey(block.Bytes); err == nil {
		return key, nil
	}
	return nil, fmt.Errorf("既不是 PKIX 也不是 PKCS1 公钥")
}

// jwtPublicFromJWKS 从 JWKS 挑一把钥匙：header.kid 命中即取；无 kid 时只有一把才取，
// 多把且无 kid 直接报错 —— 不猜（猜错了就是「看起来验过了」）。
func jwtPublicFromJWKS(raw interface{}, header map[string]interface{}) (interface{}, error) {
	// Lua/JSON 进来是 map，先过一遍 JSON 再落到结构体，省得手写一堆类型断言。
	b, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("jwt_verify: jwks 无法序列化: %w", err)
	}
	var doc struct {
		Keys []struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			N   string `json:"n"`
			E   string `json:"e"`
			Crv string `json:"crv"`
			X   string `json:"x"`
			Y   string `json:"y"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("jwt_verify: jwks 不是合法 JSON: %w", err)
	}
	if len(doc.Keys) == 0 {
		return nil, fmt.Errorf("jwt_verify: jwks.keys 为空")
	}
	kid, _ := header["kid"].(string)
	idx := -1
	switch {
	case kid != "":
		for i, k := range doc.Keys {
			if k.Kid == kid {
				idx = i
				break
			}
		}
		if idx < 0 {
			return nil, fmt.Errorf("jwt_verify: jwks 里没有 kid=%q 的钥匙（共 %d 把）", kid, len(doc.Keys))
		}
	case len(doc.Keys) == 1:
		idx = 0
	default:
		return nil, fmt.Errorf("jwt_verify: jwks 有 %d 把钥匙但令牌没带 kid，无法判断用哪把（不猜）", len(doc.Keys))
	}
	k := doc.Keys[idx]
	switch k.Kty {
	case "RSA":
		n, err := base64.RawURLEncoding.DecodeString(k.N)
		if err != nil {
			return nil, fmt.Errorf("jwt_verify: JWK n 不是合法 base64url: %w", err)
		}
		e, err := base64.RawURLEncoding.DecodeString(k.E)
		if err != nil {
			return nil, fmt.Errorf("jwt_verify: JWK e 不是合法 base64url: %w", err)
		}
		var exp big.Int
		exp.SetBytes(e)
		if !exp.IsInt64() {
			return nil, fmt.Errorf("jwt_verify: JWK e 超出范围")
		}
		return &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(exp.Int64())}, nil
	case "EC":
		curve, err := jwtECDHCurve(k.Crv)
		if err != nil {
			return nil, err
		}
		x, err := base64.RawURLEncoding.DecodeString(k.X)
		if err != nil {
			return nil, fmt.Errorf("jwt_verify: JWK x 不是合法 base64url: %w", err)
		}
		y, err := base64.RawURLEncoding.DecodeString(k.Y)
		if err != nil {
			return nil, fmt.Errorf("jwt_verify: JWK y 不是合法 base64url: %w", err)
		}
		// 拼成未压缩点：0x04 || X || Y（JWK 的 x/y 正是这两段坐标）。
		// 长度不对 / 点不在曲线上，ParseUncompressedPublicKey 一律报错 → fail-closed。
		der := make([]byte, 0, 1+len(x)+len(y))
		der = append(der, 4)
		der = append(der, x...)
		der = append(der, y...)
		pub, err := ecdsa.ParseUncompressedPublicKey(curve, der)
		if err != nil {
			return nil, fmt.Errorf("jwt_verify: JWK 公钥点无效（坐标长度不符或不在曲线上）: %w", err)
		}
		return pub, nil
	case "OKP":
		if k.Crv != "Ed25519" {
			return nil, fmt.Errorf("jwt_verify: 不支持的 OKP 曲线 %q（只支持 Ed25519）", k.Crv)
		}
		x, err := base64.RawURLEncoding.DecodeString(k.X)
		if err != nil {
			return nil, fmt.Errorf("jwt_verify: JWK x 不是合法 base64url: %w", err)
		}
		if len(x) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("jwt_verify: Ed25519 公钥应为 %d 字节，实际 %d", ed25519.PublicKeySize, len(x))
		}
		return ed25519.PublicKey(x), nil
	}
	return nil, fmt.Errorf("jwt_verify: 不支持的 JWK kty %q（合法值：RSA, EC, OKP）", k.Kty)
}

// jwtECDHCurve 把 JWK 的 crv 映射到椭圆曲线。
// 注意 Go 1.27 起 `ecdsa.PublicKey{X,Y *big.Int}` 的直接构造/修改已被 SA1019 标记废弃
// （Raw 坐标改了会产出非法密钥，big.Int 也不适合承载密码学值）。JWK 的 x/y 是未压缩点的
// 坐标分段（X||Y），走 `ParseUncompressedPublicKey(0x04||X||Y)` 既是非废弃路径，
// 又顺带校验「点真的在曲线上」、坐标长度正确——旧写法这两项都不查。
func jwtECDHCurve(crv string) (elliptic.Curve, error) {
	switch crv {
	case "P-256":
		return elliptic.P256(), nil
	case "P-384":
		return elliptic.P384(), nil
	case "P-521":
		return elliptic.P521(), nil
	}
	return nil, fmt.Errorf("jwt_verify: 不支持的 EC 曲线 %q（合法值：P-256, P-384, P-521）", crv)
}

// jwtVerifySignature 按算法验签；签名不匹配一律返回 error（fail-closed，安全口径 3）。
func jwtVerifySignature(alg string, key interface{}, signing, sig []byte) error {
	if f, ok := jwtHMACHash[alg]; ok {
		secret, ok := key.([]byte)
		if !ok {
			return fmt.Errorf("%s 需要共享密钥，实际 %T", alg, key)
		}
		mac := hmac.New(f, secret)
		mac.Write(signing)
		if !hmac.Equal(mac.Sum(nil), sig) {
			return fmt.Errorf("签名不匹配")
		}
		return nil
	}
	if h, ok := jwtRSAHash[alg]; ok {
		pub, ok := key.(*rsa.PublicKey)
		if !ok {
			return fmt.Errorf("%s 需要 RSA 公钥，实际 %T", alg, key)
		}
		hf := h.New()
		hf.Write(signing)
		return rsa.VerifyPKCS1v15(pub, h, hf.Sum(nil), sig)
	}
	if h, ok := jwtECHash[alg]; ok {
		pub, ok := key.(*ecdsa.PublicKey)
		if !ok {
			return fmt.Errorf("%s 需要 EC 公钥，实际 %T", alg, key)
		}
		hf := h.New()
		hf.Write(signing)
		// JWT 的 ES* 签名是**裸 r||s**（各填充到曲线字节长度），标准库只吃 ASN.1 DER ⇒ 先补 DER 头。
		der, err := jwtRawSigToDER(sig)
		if err != nil {
			return err
		}
		if !ecdsa.VerifyASN1(pub, hf.Sum(nil), der) {
			return fmt.Errorf("签名不匹配")
		}
		return nil
	}
	if alg == "EdDSA" {
		pub, ok := key.(ed25519.PublicKey)
		if !ok {
			return fmt.Errorf("EdDSA 需要 Ed25519 公钥，实际 %T", key)
		}
		if !ed25519.Verify(pub, signing, sig) {
			return fmt.Errorf("签名不匹配")
		}
		return nil
	}
	return fmt.Errorf("不支持的算法 %q", alg)
}

// jwtRawSigToDER 把裸 r||s 签名包成 ASN.1 DER（供 ecdsa.VerifyASN1）。
func jwtRawSigToDER(sig []byte) ([]byte, error) {
	if len(sig) == 0 || len(sig)%2 != 0 {
		return nil, fmt.Errorf("ES* 签名长度非法（应为非空偶数，实际 %d 字节）", len(sig))
	}
	half := len(sig) / 2
	type ecdsaSig struct{ R, S *big.Int }
	return asn1.Marshal(ecdsaSig{R: new(big.Int).SetBytes(sig[:half]), S: new(big.Int).SetBytes(sig[half:])})
}

// jwtDERToRawSig 把 ASN.1 DER 签名摊平成裸 r||s（ES* 签发用）。
func jwtDERToRawSig(der []byte, curve elliptic.Curve) ([]byte, error) {
	type ecdsaSig struct{ R, S *big.Int }
	var s ecdsaSig
	if _, err := asn1.Unmarshal(der, &s); err != nil {
		return nil, fmt.Errorf("ES* 签名不是合法 DER: %w", err)
	}
	size := (curve.Params().BitSize + 7) / 8
	out := make([]byte, 2*size)
	s.R.FillBytes(out[:size])
	s.S.FillBytes(out[size:])
	return out, nil
}

// jwtCheckClaims 校验时间类与受众类 claims（exp/nbf/iss/aud）。
// 只校验**调用方给了期望值**的那几项 —— 没给期望却去校验等于替应用做决定。
func jwtCheckClaims(claims map[string]interface{}, args map[string]interface{}) error {
	leeway := 0.0
	if n, ok, err := pureNum(args, "leeway"); err != nil {
		return err
	} else if ok {
		if n < 0 {
			return fmt.Errorf("jwt_verify: leeway 不能为负数，实际 %v", n)
		}
		leeway = n
	}
	now := float64(time.Now().Unix())
	if v, ok := claims["exp"]; ok {
		exp, err := jwtNumClaim(v, "exp")
		if err != nil {
			return err
		}
		if now > exp+leeway {
			return fmt.Errorf("jwt_verify: 令牌已过期（exp=%d，当前 %d，leeway=%v 秒）", int64(exp), int64(now), leeway)
		}
	}
	if v, ok := claims["nbf"]; ok {
		nbf, err := jwtNumClaim(v, "nbf")
		if err != nil {
			return err
		}
		if now < nbf-leeway {
			return fmt.Errorf("jwt_verify: 令牌尚未生效（nbf=%d，当前 %d）", int64(nbf), int64(now))
		}
	}
	if want, err := pureOptStr(args, "iss", ""); err != nil {
		return err
	} else if want != "" {
		if got, _ := claims["iss"].(string); got != want {
			return fmt.Errorf("jwt_verify: 签发者不符（期望 %q，实际 %q）", want, got)
		}
	}
	if want, err := pureOptStr(args, "aud", ""); err != nil {
		return err
	} else if want != "" && !jwtAudMatches(claims["aud"], want) {
		return fmt.Errorf("jwt_verify: 受众不符（期望包含 %q，实际 %v）", want, claims["aud"])
	}
	return nil
}

// jwtNumClaim 取数值型 claim（JSON 解出来是 float64）。
func jwtNumClaim(v interface{}, name string) (float64, error) {
	switch x := v.(type) {
	case float64:
		return x, nil
	case int:
		return float64(x), nil
	case int64:
		return float64(x), nil
	}
	return 0, fmt.Errorf("jwt_verify: claim %q 必须是数字（Unix 秒），实际 %T", name, v)
}

// jwtAudMatches 判断 aud（字符串或字符串数组）是否包含期望值。
func jwtAudMatches(v interface{}, want string) bool {
	switch a := v.(type) {
	case string:
		return a == want
	case []interface{}:
		for _, x := range a {
			if s, ok := x.(string); ok && s == want {
				return true
			}
		}
	}
	return false
}

// ---- jwt_sign ----

func cryptoJWTSign(args map[string]interface{}) (interface{}, error) {
	raw, ok := args["claims"]
	if !ok || raw == nil {
		return nil, fmt.Errorf("jwt_sign: 缺少必填参数 \"claims\"")
	}
	claims, ok := raw.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("jwt_sign: 参数 \"claims\" 必须是 object，实际 %T", raw)
	}
	alg, err := pureEnum(args, "alg", "", jwtAlgs)
	if err != nil {
		return nil, err
	}
	key, err := pureStr(args, "key")
	if err != nil {
		return nil, err
	}
	if ttl, ok, err := pureNum(args, "ttl"); err != nil {
		return nil, err
	} else if ok {
		if ttl <= 0 {
			return nil, fmt.Errorf("jwt_sign: ttl 必须为正数，实际 %v", ttl)
		}
		if _, exists := claims["exp"]; !exists {
			claims["exp"] = float64(time.Now().Unix()) + ttl
		}
	}
	header := map[string]interface{}{"alg": alg, "typ": "JWT"}
	if kid, err := pureOptStr(args, "kid", ""); err != nil {
		return nil, err
	} else if kid != "" {
		header["kid"] = kid
	}
	hb, err := json.Marshal(header)
	if err != nil {
		return nil, fmt.Errorf("jwt_sign: header 序列化失败: %w", err)
	}
	cb, err := json.Marshal(claims)
	if err != nil {
		return nil, fmt.Errorf("jwt_sign: claims 序列化失败: %w", err)
	}
	seg := jwtB64(hb) + "." + jwtB64(cb)
	sig, err := jwtSignWith(alg, key, args, []byte(seg))
	if err != nil {
		return nil, fmt.Errorf("jwt_sign: %w", err)
	}
	return seg + "." + jwtB64(sig), nil
}

// jwtSignWith 按算法签名；私钥/共享密钥由调用方传入（不读 env、不落盘）。
func jwtSignWith(alg, key string, args map[string]interface{}, signing []byte) ([]byte, error) {
	if f, ok := jwtHMACHash[alg]; ok {
		enc, err := pureEnum(args, "key_encoding", "utf8", []string{"utf8", "hex", "base64"})
		if err != nil {
			return nil, err
		}
		secret, err := jwtDecodeKeyMaterial(key, enc)
		if err != nil {
			return nil, err
		}
		mac := hmac.New(f, secret)
		mac.Write(signing)
		return mac.Sum(nil), nil
	}
	priv, err := jwtParsePrivatePEM([]byte(key))
	if err != nil {
		return nil, err
	}
	if h, ok := jwtRSAHash[alg]; ok {
		p, ok := priv.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("%s 需要 RSA 私钥，实际 %T", alg, priv)
		}
		hf := h.New()
		hf.Write(signing)
		return rsa.SignPKCS1v15(rand.Reader, p, h, hf.Sum(nil))
	}
	if h, ok := jwtECHash[alg]; ok {
		p, ok := priv.(*ecdsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("%s 需要 EC 私钥，实际 %T", alg, priv)
		}
		hf := h.New()
		hf.Write(signing)
		der, err := ecdsa.SignASN1(rand.Reader, p, hf.Sum(nil))
		if err != nil {
			return nil, err
		}
		return jwtDERToRawSig(der, p.Curve)
	}
	if alg == "EdDSA" {
		p, ok := priv.(ed25519.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("EdDSA 需要 Ed25519 私钥，实际 %T", priv)
		}
		return ed25519.Sign(p, signing), nil
	}
	return nil, fmt.Errorf("不支持的算法 %q", alg)
}

// jwtParsePrivatePEM 解析私钥 PEM（PKCS1 / SEC1 / PKCS8 依次尝试）。
func jwtParsePrivatePEM(raw []byte) (interface{}, error) {
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, fmt.Errorf("不是合法的 PEM 私钥块（缺少 -----BEGIN ... -----）")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	if k, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		switch kk := k.(type) {
		case *rsa.PrivateKey, *ecdsa.PrivateKey, ed25519.PrivateKey:
			return kk, nil
		}
		return nil, fmt.Errorf("不支持的 PKCS8 私钥类型 %T", k)
	}
	return nil, fmt.Errorf("既不是 PKCS1 / SEC1 / PKCS8 私钥")
}

// jwtStringList 取字符串数组参数（Lua 的数组进来是 []interface{}）。
func jwtStringList(args map[string]interface{}, name string) ([]string, error) {
	raw, ok := args[name]
	if !ok || raw == nil {
		return nil, nil
	}
	arr, ok := raw.([]interface{})
	if !ok {
		return nil, fmt.Errorf("参数 %q 必须是字符串数组，实际 %T", name, raw)
	}
	out := make([]string, 0, len(arr))
	for _, x := range arr {
		s, ok := x.(string)
		if !ok {
			return nil, fmt.Errorf("参数 %q 的元素必须是字符串，实际 %T", name, x)
		}
		out = append(out, s)
	}
	return out, nil
}

// jwtContains 判断字符串是否在列表中（刻意不复用同名工具函数，避免与其它包的语义打架）。
func jwtContains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// ---- ctx.crypto.* 口令哈希 ----
//
// 为什么只做 pbkdf2：**零新增依赖** —— PBKDF2 就是 HMAC 迭代，标准库够了。
// 接口用 `algo` 参数化，将来要加 bcrypt / argon2 不必改调用形态；但那会引入
// golang.org/x/crypto，是「新增依赖」决策，不在这批做（见 ENTERPRISE_PRIMITIVE_GAP.md §5）。
//
// 输出用 **PHC 串**（`$algo$i=N$salt$hash`）而不是裸 hex：把算法、迭代次数、盐一起存进去，
// 将来调迭代次数或换算法时，老口令仍能验（验的是串里记的参数，不是当前默认值）。

var passwordAlgos = []string{"pbkdf2-sha256", "pbkdf2-sha512"}

const (
	defaultPasswordIterations = 210000 // Spring Security 同量级；可调，见 iterations 参数
	maxPasswordIterations     = 1000000
	passwordSaltBytes         = 16
)

// passwordPRF 把算法名映射到 PBKDF2 用的伪随机函数。
var passwordPRF = map[string]func() hash.Hash{
	"pbkdf2-sha256": sha256.New,
	"pbkdf2-sha512": sha512.New,
}

func cryptoPasswordHash(args map[string]interface{}) (interface{}, error) {
	password, err := pureStr(args, "password")
	if err != nil {
		return nil, err
	}
	algo, err := pureEnum(args, "algo", "pbkdf2-sha256", passwordAlgos)
	if err != nil {
		return nil, err
	}
	iterations := defaultPasswordIterations
	if n, ok, err := pureNum(args, "iterations"); err != nil {
		return nil, err
	} else if ok {
		if n != float64(int(n)) || n < 1 || n > maxPasswordIterations {
			return nil, fmt.Errorf("password_hash: iterations 必须是 1..%d 的整数，实际 %v", maxPasswordIterations, n)
		}
		iterations = int(n)
	}
	saltStr, err := pureOptStr(args, "salt", "")
	if err != nil {
		return nil, err
	}
	salt := []byte(saltStr)
	if saltStr == "" {
		salt = make([]byte, passwordSaltBytes)
		if _, err := rand.Read(salt); err != nil {
			return nil, fmt.Errorf("password_hash: 生成随机盐失败: %w", err)
		}
	}
	dk := pbkdf2Key(passwordPRF[algo], []byte(password), salt, iterations)
	return fmt.Sprintf("$%s$i=%d$%s$%s", algo, iterations,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(dk)), nil
}

// pbkdf2Key 是 PBKDF2（RFC 8018）的最小实现：dk = U_1 xor U_2 xor ... xor U_c。
//
// 只取**一个块**（T_1）：本函数输出的 dk 长度 = 摘要长度（sha256→32 / sha512→64 字节），
// 而 PBKDF2 每块产出正好一个摘要长度，故 l=1 恒成立。若将来要支持更长输出，需补块循环。
func pbkdf2Key(h func() hash.Hash, password, salt []byte, iter int) []byte {
	// U_1 = PRF(P, S || INT_32_BE(1))
	seed := make([]byte, 0, len(salt)+4)
	seed = append(seed, salt...)
	seed = append(seed, 0, 0, 0, 1)
	t := pbkdf2PRF(h, password, seed)
	block := make([]byte, len(t))
	copy(block, t)
	for i := 1; i < iter; i++ {
		t = pbkdf2PRF(h, password, t)
		for j := range block {
			block[j] ^= t[j]
		}
	}
	return block
}

// pbkdf2PRF 是一次 HMAC 调用（PBKDF2 的 PRF）。
func pbkdf2PRF(h func() hash.Hash, password, data []byte) []byte {
	mac := hmac.New(h, password)
	mac.Write(data)
	return mac.Sum(nil)
}

func cryptoPasswordVerify(args map[string]interface{}) (interface{}, error) {
	password, err := pureStr(args, "password")
	if err != nil {
		return nil, err
	}
	encoded, err := pureStr(args, "hash")
	if err != nil {
		return nil, err
	}
	algo, iter, salt, want, err := parsePHC(encoded)
	if err != nil {
		return nil, err
	}
	prf, ok := passwordPRF[algo]
	if !ok {
		return nil, fmt.Errorf("password_verify: 不支持的算法 %q（合法值：%s）", algo, strings.Join(passwordAlgos, ", "))
	}
	// 恒定时比较（hmac.Equal），不提前退出 —— 早退会泄漏「前几个字节对了」。
	return hmac.Equal(pbkdf2Key(prf, []byte(password), salt, iter), want), nil
}

// parsePHC 解析 password_hash 的输出（PHC 串 `$algo$i=N$salt$hash`）。
//
// 格式非法一律**报错**而不是返回 false：哈希串烂了若表现为「校验失败」，
// 「所有人都登录不上」就会被误诊成「大家密码都记错了」—— 那是静默降级，不能忍。
func parsePHC(s string) (algo string, iterations int, salt, dk []byte, err error) {
	parts := strings.Split(s, "$")
	if len(parts) != 5 || parts[0] != "" {
		return "", 0, nil, nil, fmt.Errorf("password_verify: 哈希串不是合法的 PHC 格式"+
			"（应为 $algo$i=N$salt$hash），实际 %d 段", len(parts))
	}
	algo = parts[1]
	if !strings.HasPrefix(parts[2], "i=") {
		return "", 0, nil, nil, fmt.Errorf("password_verify: 第二段应为 i=<迭代次数>，实际 %q", parts[2])
	}
	n, err := strconv.Atoi(strings.TrimPrefix(parts[2], "i="))
	if err != nil || n < 1 {
		return "", 0, nil, nil, fmt.Errorf("password_verify: 迭代次数非法（%q）", parts[2])
	}
	if salt, err = base64.RawStdEncoding.DecodeString(parts[3]); err != nil {
		return "", 0, nil, nil, fmt.Errorf("password_verify: 盐不是合法 base64: %w", err)
	}
	if dk, err = base64.RawStdEncoding.DecodeString(parts[4]); err != nil {
		return "", 0, nil, nil, fmt.Errorf("password_verify: 哈希值不是合法 base64: %w", err)
	}
	if len(dk) == 0 {
		return "", 0, nil, nil, fmt.Errorf("password_verify: 哈希值为空")
	}
	return algo, n, salt, dk, nil
}

// ---- ctx.id.* ----
//
// 标识符生成。零依赖：UUID v4 只是「16 随机字节 + 置位 + hex」，不必为此把
// github.com/google/uuid 从 indirect 转正；随机取字符一律走 crypto/rand.Int（**无模偏倚**），
// 不用 `randByte % len(alphabet)` —— 后者对验证码这类安全场景是真实可利用的偏斜。

// idCharsets 是 code 的预置字符集。
var idCharsets = map[string]string{
	"digits": "0123456789",
	"upper":  "ABCDEFGHIJKLMNOPQRSTUVWXYZ",
	"lower":  "abcdefghijklmnopqrstuvwxyz",
	"alnum":  "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789",
	"hex":    "0123456789abcdef",
}

// idCharsetNames 是字符集枚举（顺序稳定，避免 map 迭代顺序进能力目录/提示词）。
var idCharsetNames = []string{"digits", "upper", "lower", "alnum", "hex"}

// idNanoIDAlphabet 是 nanoid 的默认字母表（与 NanoID 官方一致：URL 安全、无转义）。
const idNanoIDAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_-"

func pureIDCall(method string, args map[string]interface{}) (interface{}, error) {
	switch method {
	case "uuid":
		return idUUID(args)
	case "nanoid":
		return idNanoID(args)
	case "code":
		return idCode(args)
	}
	return nil, fmt.Errorf("id 没有方法 %q（合法方法：uuid, nanoid, code）", method)
}

func idUUID(args map[string]interface{}) (interface{}, error) {
	format, err := pureEnum(args, "format", "plain", []string{"plain", "braced", "urn", "hex"})
	if err != nil {
		return nil, err
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("id.uuid: 读取随机数失败: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version = 0100 (v4)
	b[8] = (b[8] & 0x3f) | 0x80 // variant = 10 (RFC 4122)
	h := hex.EncodeToString(b)
	switch format {
	case "hex":
		return h, nil
	case "braced":
		return "{" + idUUIDString(h) + "}", nil
	case "urn":
		return "urn:uuid:" + idUUIDString(h), nil
	}
	return idUUIDString(h), nil
}

// idUUIDString 把 32 位 hex 拼成 8-4-4-4-12 的标准形态。
func idUUIDString(h string) string {
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

func idNanoID(args map[string]interface{}) (interface{}, error) {
	size := 21
	if n, ok, err := pureNum(args, "size"); err != nil {
		return nil, err
	} else if ok {
		if n != float64(int(n)) || n < 1 || n > 256 {
			return nil, fmt.Errorf("id.nanoid: size 必须是 1..256 的整数，实际 %v", n)
		}
		size = int(n)
	}
	alphabet, err := pureOptStr(args, "alphabet", idNanoIDAlphabet)
	if err != nil {
		return nil, err
	}
	if err := idCheckAlphabet(alphabet); err != nil {
		return nil, fmt.Errorf("id.nanoid: %w", err)
	}
	return idRandomString(size, alphabet)
}

func idCode(args map[string]interface{}) (interface{}, error) {
	length := 6
	if n, ok, err := pureNum(args, "length"); err != nil {
		return nil, err
	} else if ok {
		if n != float64(int(n)) || n < 1 || n > 64 {
			return nil, fmt.Errorf("id.code: length 必须是 1..64 的整数，实际 %v", n)
		}
		length = int(n)
	}
	name, err := pureEnum(args, "charset", "digits", idCharsetNames)
	if err != nil {
		return nil, err
	}
	return idRandomString(length, idCharsets[name])
}

// idCheckAlphabet 校验自定义字母表（去重与最小长度）。
// 不去重会让「出现概率不均」静默发生 —— 那是随机码最隐蔽的一类 bug。
func idCheckAlphabet(alphabet string) error {
	if len(alphabet) < 2 {
		return fmt.Errorf("字母表长度至少为 2，实际 %d", len(alphabet))
	}
	seen := map[rune]bool{}
	for _, r := range alphabet {
		if seen[r] {
			return fmt.Errorf("字母表含重复字符 %q（会让字符出现概率不均）", r)
		}
		seen[r] = true
	}
	return nil
}

// idRandomString 从字母表取 n 个字符。用 rand.Int（拒绝采样，**无模偏倚**），
// 刻意不用 `随机字节 % len(alphabet)` —— 255 不被字母表长度整除时会产生偏斜。
func idRandomString(n int, alphabet string) (string, error) {
	max := big.NewInt(int64(len(alphabet)))
	out := make([]byte, 0, n)
	for i := 0; i < n; i++ {
		v, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", fmt.Errorf("读取随机数失败: %w", err)
		}
		out = append(out, alphabet[v.Int64()])
	}
	return string(out), nil
}

// ---- ctx.validate.* ----
//
// 复用引擎既有的 `validate`（engine.go，AI 输出校验也走它）—— 不另写一份编译逻辑，
// 免得两处 JSON Schema 行为漂移。
//
// ★ 三种「失败」的口径刻意不同，别混淆：
//   - 数据不合法 → `{valid=false, errors=[...]}`：**不是 error**，调用方要把明细展示给用户；
//   - schema 本身非法（编译不过） → **报错**：那是应用的 bug，静默成 valid=false 会让
//     「所有数据都校验不过」看起来像「用户填错了」；
//   - 缺参数 → 报错（与其它纯函数原子同款 fail-closed）。

// maxValidateErrors 是返回给调用方的错误明细上限。
// 截断必须**如实标注**（truncated + total），不能悄悄砍掉 —— 与目录类观察截断同一口径。
const maxValidateErrors = 50

func pureValidateCall(method string, args map[string]interface{}) (interface{}, error) {
	switch method {
	case "json_schema":
		return validateJSONSchema(args)
	}
	return nil, fmt.Errorf("validate 没有方法 %q（合法方法：json_schema）", method)
}

func validateJSONSchema(args map[string]interface{}) (interface{}, error) {
	rawSchema, ok := args["schema"]
	if !ok || rawSchema == nil {
		return nil, fmt.Errorf("validate.json_schema: 缺少必填参数 \"schema\"")
	}
	schema, ok := rawSchema.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("validate.json_schema: 参数 \"schema\" 必须是 JSON Schema 对象，实际 %T", rawSchema)
	}
	data, ok := args["data"]
	if !ok {
		return nil, fmt.Errorf("validate.json_schema: 缺少必填参数 \"data\"（可为任意类型；传 nil 表示显式校验 null）")
	}

	err := validate(schema, data)
	if err == nil {
		return map[string]interface{}{"valid": true, "errors": []string{}}, nil
	}
	// 先判「schema 本身非法」：它比「数据不合法」严重，必须报错而不是当成校验结果。
	var se *jsonschema.SchemaError
	if errors.As(err, &se) {
		return nil, fmt.Errorf("validate.json_schema: schema 本身非法: %w", err)
	}
	var ve *jsonschema.ValidationError
	if !errors.As(err, &ve) {
		return nil, fmt.Errorf("validate.json_schema: %w", err)
	}

	msgs := jsonschemaLeafMessages(ve)
	total := len(msgs)
	out := map[string]interface{}{"valid": false, "errors": msgs, "total": total}
	if total > maxValidateErrors {
		out["errors"] = msgs[:maxValidateErrors]
		out["truncated"] = true
	}
	return out, nil
}

// jsonschemaLeafMessages 递归展开 ValidationError，只取**叶子**原因（带实例位置）。
// 中间节点的 Message 是「doesn't validate with ...」这类套话，对排障没用。
func jsonschemaLeafMessages(ve *jsonschema.ValidationError) []string {
	if len(ve.Causes) == 0 {
		loc := ve.InstanceLocation
		if loc == "" {
			loc = "/"
		}
		return []string{loc + ": " + ve.Message}
	}
	out := make([]string, 0, len(ve.Causes))
	for _, c := range ve.Causes {
		out = append(out, jsonschemaLeafMessages(c)...)
	}
	return out
}
