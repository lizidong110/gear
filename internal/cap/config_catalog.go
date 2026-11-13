package cap

// config_catalog.go —— 内部关键配置键的**单一真相源**（REQ-API-CONFIG-CATALOG）。
//
// 存在理由：让 AI 用上某个模型/key 需要写 `ai.provider` / `ai.model` / `ai.api_key` …
// 这些字符串此前只活在 CLI 帮助文本与 ai_llm.go 的解析链里，用户（和 AI）只能靠背。
// 配置页要按「中文名 + 说明 + 默认值」渲染表单，就必须有一份机器可读的目录；
// 目录写在内核而不是前端，是为了让 UI 不硬编码 key —— 否则加一个键时前端不改就静默漏掉。
//
// 防漂移红线：目录里的 ai.* 键集合必须与 engine/ai_llm.go 的 cfg("ai.xxx") 解析链**完全一致**，
// 由 config_catalog_test.go 用 go/parser 直接读源码比对（描述比实现窄是静默缺陷，编译期发现不了）。
// 新增配置键时先改解析链，再补目录并跑该测试。

import "strings"

// ConfigKeySpec 是一项「内部关键配置键」的展示与编辑元数据。
// 它只描述展示，**不改变**运行期取值口径：实际生效值仍由 engine.resolveLLMConfig 的
// 「调用参数 > 环境变量 > 配置库 > 内置默认」链决定，见 Default/Env 字段的说明。
type ConfigKeySpec struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	Group       string `json:"group"`
	Description string `json:"description"`
	// Default 是**内置默认**的展示值（未写配置库、也无环境变量时生效）。
	// 随 provider 变化的项（base_url / model）只给主默认值，动态部分写进 DefaultNote。
	Default     string `json:"default"`
	DefaultNote string `json:"default_note,omitempty"`
	// Env 是优先级高于配置库的环境变量；非空时 UI 须提示「环境变量会覆盖本页设置」。
	Env string `json:"env,omitempty"`
	// Sensitive 标记凭据类键：列表响应只回掩码，绝不回明文（与 secrets 管理面同一红线）。
	Sensitive bool `json:"sensitive"`
	// Options 非空时 UI 渲染下拉框（ValueType="enum"）。
	Options   []string `json:"options,omitempty"`
	ValueType string   `json:"value_type"` // string | number | enum | secret
}

// ConfigCatalog 返回内置关键配置键目录。顺序即 UI 渲染顺序（分组内按语义排列）。
//
// ⚠ 键集合被 config_catalog_test.go 与 ai_llm.go 解析链双向锁死，勿随手增删。
func ConfigCatalog() []ConfigKeySpec {
	return []ConfigKeySpec{
		{
			Key:         "ai.provider",
			Label:       "模型服务商",
			Group:       "AI 模型",
			Description: "决定内置默认的服务地址与模型名；也决定请求体按哪家的协议组装。",
			Default:     "siliconflow",
			Env:         "GEAR_AI_PROVIDER",
			Options:     []string{"siliconflow", "ollama"},
			ValueType:   "enum",
		},
		{
			Key:         "ai.base_url",
			Label:       "API 服务地址",
			Group:       "AI 模型",
			Description: "大模型 HTTP API 的根地址，通常以 /v1 结尾。",
			Default:     "https://api.siliconflow.cn/v1",
			DefaultNote: "provider=ollama 时默认为 http://localhost:11434（可用 GEAR_AI_OLLAMA_URL 覆盖）",
			Env:         "GEAR_AI_BASE_URL",
			ValueType:   "string",
		},
		{
			Key:         "ai.api_key",
			Label:       "API 密钥",
			Group:       "AI 模型",
			Description: "调用大模型 API 的凭据。写入后只回掩码，页面不会显示明文；留空表示不修改已存的值。",
			Default:     "",
			DefaultNote: "未配置时请求不带凭据（本地 ollama 通常无需配置）",
			Env:         "GEAR_AI_API_KEY",
			Sensitive:   true,
			ValueType:   "secret",
		},
		{
			Key:         "ai.model",
			Label:       "模型名称",
			Group:       "AI 模型",
			Description: "实际调用的模型 ID，须与服务商支持的名称一致。",
			Default:     "Qwen/Qwen3-8B",
			DefaultNote: "provider=ollama 时默认为 qwen3:latest；亦可用 GEAR_AI_SILICONFLOW_MODEL / GEAR_AI_OLLAMA_MODEL 覆盖",
			Env:         "GEAR_AI_MODEL",
			ValueType:   "string",
		},
		{
			Key:         "ai.max_tokens",
			Label:       "单次最大生成 token 数",
			Group:       "AI 模型",
			Description: "限制一次生成的上限，给推理模型留出「思考 + 结构化输出」的余量。必须为正整数。",
			Default:     "4096",
			Env:         "GEAR_AI_MAX_TOKENS",
			ValueType:   "number",
		},
		{
			Key:         "fs.roots",
			Label:       "fs 附加安全根",
			Group:       "文件访问",
			Description: "fs 原语（ctx.fs.*）在默认根之外的附加可读写目录，多个用分号或换行分隔（如 D:/shared/docs;D:/data/inbox）。主根恒为 GEAR_APP_DATA（<GEAR_HOME>/data），不受此项影响；越界一律报「路径逃出安全目录」。",
			Default:     "",
			DefaultNote: "留空 = 只开放默认安全根；改后需重启 gear 进程生效（根集合在引擎装配期固定）",
			ValueType:   "string",
		},
	}
}

// ConfigKeyByKey 按 key 查目录项。未在目录中的自定义配置键返回 ok=false
// （管理面仍允许读写它们，只是没有中文名/默认值可展示）。
func ConfigKeyByKey(key string) (ConfigKeySpec, bool) {
	for _, s := range ConfigCatalog() {
		if s.Key == key {
			return s, true
		}
	}
	return ConfigKeySpec{}, false
}

// LooksSecretKey 判断键名是否像凭据（只按名字判断，不看值——避免把普通配置误当密钥脱敏）。
//
// CLI `gear config list` 与 REST `/api/config` 共用本函数：两处各写一份必然漂移，
// 而漂移的方向总是「某一边漏脱敏」，那是最难发现的一类泄密。
func LooksSecretKey(k string) bool {
	low := strings.ToLower(k)
	for _, s := range []string{"api_key", "apikey", "token", "password", "secret", "passwd"} {
		if strings.HasSuffix(low, s) || strings.Contains(low, "."+s) {
			return true
		}
	}
	return false
}

// MaskSecret 把凭据压成「前 4 + … + 后 4」：既能确认配的是不是自己那把 key，
// 又不必让明文落在终端/HTTP 响应/截图里。短于 12 字符一律全遮（短串遮不住，遮了等于没遮）。
func MaskSecret(v string) string {
	if v == "" {
		return ""
	}
	if len(v) < 12 {
		return strings.Repeat("*", len(v))
	}
	return v[:4] + "…" + v[len(v)-4:]
}
