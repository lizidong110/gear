package cli

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"gear/internal/engine"
	"gear/internal/registry"
)

// aiCmd 管理 AI 生成（ext.ai 结构化多阶段 agent，PRD §9 去内置化）。
func aiCmd() *cobra.Command {
	c := &cobra.Command{Use: "ai", Short: "AI 生成 Flow / Tool（ext.ai 结构化多阶段 agent）"}
	c.AddCommand(genCmd())
	c.AddCommand(&cobra.Command{
		Use:   "capabilities",
		Short: "列出当前所有原子能力（内核 builtin + pure 原语 + 已注册 ext.*）及其方法参数 schema",
		Long: "打印引擎当前装配的宿主能力目录：内核门禁原语（http/call/db/secret/event/subscribe/config）+ " +
			"纯函数原子（time/template/crypto/id/validate，始终可用、无需声明）+ 所有已注册 " +
			"ext.* 扩展，每条带方法名、参数类型与必填标记。\n" +
			"这是 agent 生成 Tool 时锚定的同一份目录（ext.registry.capabilities() / ext.ai.capabilities()），" +
			"随 extensions 配置动态变化——新增扩展会自动出现在这里。",
		RunE: func(cmd *cobra.Command, args []string) error { return runAICapabilities(cmd) },
	})
	reactC := &cobra.Command{
		Use:   "react [任务描述]",
		Short: "跑 ReAct 智能体（LLM 逐步调用内置工具完成任务）",
		Long: "用自然语言任务驱动一个 ReAct 循环：LLM 每步思考并调用一个内置工具（http/call/db/secret/event" +
			"或已注册 ext.*），观察结果后继续，直到给出 Final Answer。\n" +
			"等价于在 Tool 的 entry 里调 ctx.ext.ai.react({ task = \"...\" })。需要联网 LLM（key/provider 经参数传入，dev CLI 可用 GEAR_AI_* 注入）。",
		RunE: func(cmd *cobra.Command, args []string) error { return runAIReact(cmd, args) },
	}
	reactC.Flags().Int("max-steps", 6, "最大步数")
	reactC.Flags().StringSlice("allow", nil, "限制可用工具：能力名（http,secret）或方法名（ext.redis.set）；默认一组通用能力，不是全目录")
	reactC.Flags().Bool("allow-all", false, "不设限地开放整个能力目录。慎用：工具越多模型越选不准，且很可能撞上内核的工具数上限")
	reactC.Flags().String("model", "", "模型名（可选，缺省读 GEAR_AI_MODEL）")
	reactC.Flags().String("provider", "", "模型厂商（可选，缺省读 GEAR_AI_PROVIDER）")
	reactC.Flags().String("base-url", "", "API base（可选，缺省读 GEAR_AI_BASE_URL）")
	reactC.Flags().String("api-key", "", "API key（可选，缺省读 GEAR_AI_API_KEY）")
	completeC := &cobra.Command{
		Use:   "complete [prompt]",
		Short: "调用 ext.ai.complete（LLM 补全），可加 --stream 逐增量打印",
		RunE:  func(cmd *cobra.Command, args []string) error { return runAIComplete(cmd, args) },
	}
	completeC.Flags().StringP("system", "s", "", "系统提示（可选）")
	completeC.Flags().String("format", "text", "输出格式 text|json（可选，默认 text）")
	completeC.Flags().Bool("stream", false, "流式输出：逐 token 打印增量（含推理增量）")
	completeC.Flags().String("model", "", "模型名（可选，缺省读 GEAR_AI_MODEL）")
	completeC.Flags().String("provider", "", "模型厂商（可选，缺省读 GEAR_AI_PROVIDER）")
	completeC.Flags().String("base-url", "", "API base（可选，缺省读 GEAR_AI_BASE_URL）")
	completeC.Flags().String("api-key", "", "API key（可选，缺省读 GEAR_AI_API_KEY）")
	c.AddCommand(completeC)

	c.AddCommand(reactC)
	return c
}

// runAIReact 实现 `ai react "任务"`：构造进程内 ext.ai agent，跑 React 循环并打印结果。
func runAIReact(cmd *cobra.Command, args []string) error {
	task := strings.Join(args, " ")
	if strings.TrimSpace(task) == "" {
		return fmt.Errorf("请提供任务描述：gear ai react \"...\"")
	}
	// 1) 持久 registry（只读用途：取真实工具目录给 agent 看）。
	//
	// 少了这一步，react 看到的 registry 里只有 SetupExtensions 自己注册的几个 ext 壳子——
	// 实测 `ext.registry.tools()` 只吐 4 个 ext，而库里其实有 34 个 Tool（mindstorm__* 等
	// 应用定义全看不见）。判据①「用 gear 自己的 AI 维护应用」的前提是它**看得见**这些定义。
	persist, err := openRegistry()
	if err != nil {
		return err
	}
	defer persist.Close()

	// 2) 内存 registry 跑 agent：react 会真的执行工具，用内存库避免把中间产物落进持久库。
	reg, err := registry.Open(fmt.Sprintf("file:gear_ai_cli_%d?mode=memory&cache=shared", os.Getpid()))
	if err != nil {
		return fmt.Errorf("打开内存 registry 失败: %w", err)
	}
	defer reg.Close()
	// 顺序要紧：先建引擎（SetupExtensions 填充合法 capability 集合），再灌工具目录——
	// 反过来的话灌入会撞「未知的 capability」。
	eng, err := cliEngine(reg)
	if err != nil {
		return err
	}
	// 3) 把持久库的目录灌进内存库（与 runAIGen 同一手法）：读是共享的，写仍隔离在内存里。
	skipped, err := seedRefRegistry(reg, persist)
	if err != nil {
		return fmt.Errorf("灌入真实工具目录失败: %w", err)
	}
	warnSkipped(skipped)
	agent := eng.AIAgent()
	if agent == nil {
		return fmt.Errorf("ext.ai 未装配（SetupExtensions 失败？）")
	}
	maxSteps, _ := cmd.Flags().GetInt("max-steps")
	allow, _ := cmd.Flags().GetStringSlice("allow")
	allowAll, _ := cmd.Flags().GetBool("allow-all")
	// --allow-all 与 --allow 互斥：两个都给说明没想清楚要什么，报出来比挑一个静默执行好。
	if allowAll && len(allow) > 0 {
		return fmt.Errorf("--allow-all 与 --allow 不能同时给：前者不设限，后者是白名单，语义冲突")
	}
	opts := engine.ReactOpts{MaxSteps: maxSteps}
	if allowAll {
		opts.Allow = []string{engine.ReactAllowAll}
		opts.AllowSet = true
	} else {
		opts.Allow = allow
	}
	if s, _ := cmd.Flags().GetString("model"); s != "" {
		opts.Model = s
	}
	if s, _ := cmd.Flags().GetString("provider"); s != "" {
		opts.Provider = s
	}
	if s, _ := cmd.Flags().GetString("base-url"); s != "" {
		opts.BaseURL = s
	}
	if s, _ := cmd.Flags().GetString("api-key"); s != "" {
		opts.APIKey = s
	}
	// 开发便利：flag 未给时回退到 GEAR_AI_* 环境变量（原子能力本身只读调用参数，不读 env）。
	if opts.Provider == "" {
		opts.Provider = os.Getenv("GEAR_AI_PROVIDER")
	}
	if opts.Model == "" {
		opts.Model = os.Getenv("GEAR_AI_MODEL")
	}
	if opts.BaseURL == "" {
		opts.BaseURL = os.Getenv("GEAR_AI_BASE_URL")
	}
	if opts.APIKey == "" {
		opts.APIKey = os.Getenv("GEAR_AI_API_KEY")
	}
	res, err := agent.React(context.Background(), task, opts)
	if err != nil {
		return fmt.Errorf("react 失败: %w", err)
	}
	cmd.Println(renderReactResult(res))
	return nil
}

// renderReactResult 把 React 返回 {answer,steps,used_tools} 渲染成可读文本。
func renderReactResult(res interface{}) string {
	m, ok := res.(map[string]interface{})
	if !ok {
		b, _ := yaml.Marshal(res)
		return string(b)
	}
	var sb strings.Builder
	sb.WriteString("=== ReAct 结果 ===\n")
	if ans, _ := m["answer"].(string); ans != "" {
		sb.WriteString("Final Answer:\n" + ans + "\n")
	}
	if tools, ok := m["used_tools"].([]string); ok && len(tools) > 0 {
		sb.WriteString("\n使用的工具：\n- " + strings.Join(tools, "\n- ") + "\n")
	}
	if steps, ok := m["steps"].([]map[string]interface{}); ok && len(steps) > 0 {
		sb.WriteString(fmt.Sprintf("\n步骤（%d）：\n", len(steps)))
		for i, s := range steps {
			act, _ := s["action"].(string)
			obs, _ := s["observation"].(string)
			if act == "" {
				act = "(Final Answer)"
			}
			sb.WriteString(fmt.Sprintf("\n[%d] 行动: %s\n    观察: %s\n", i+1, act, truncateForCLI(obs, 500)))
		}
	}
	return sb.String()
}

// truncateForCLI 按 rune 数截断（复用引擎逻辑的可读版本，避免跨包依赖细节）。
func truncateForCLI(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "...(truncated)"
}

// runAICapabilities 打印当前实时能力目录（人/Agent 同源：CapabilitiesMethod）。
func runAICapabilities(cmd *cobra.Command) error {
	reg, err := registry.Open(fmt.Sprintf("file:gear_ai_cli_%d?mode=memory&cache=shared", os.Getpid()))
	if err != nil {
		return fmt.Errorf("打开内存 registry 失败: %w", err)
	}
	defer reg.Close()
	eng, err := cliEngine(reg)
	if err != nil {
		return err
	}
	agent := eng.AIAgent()
	if agent == nil {
		return fmt.Errorf("ext.ai 未装配（SetupExtensions 失败？）")
	}
	sums, err := agent.CapabilitiesMethod()
	if err != nil {
		return fmt.Errorf("获取能力目录失败: %w", err)
	}
	cmd.Println(renderCapabilities(sums))
	return nil
}

// renderCapabilities 把 CapabilitiesMethod 返回的目录渲染成可读树（单一真相源，与提示词同源）。
func renderCapabilities(sums interface{}) string {
	rows, ok := sums.([]map[string]interface{})
	if !ok {
		b, _ := yaml.Marshal(sums)
		return string(b)
	}
	var sb strings.Builder
	sb.WriteString("当前原子能力目录（内核 builtin + pure 原语 + 已注册 ext.*，随 extensions 配置动态变化）：\n")
	nBuiltin, nPure, nExt := 0, 0, 0
	for _, r := range rows {
		name, _ := r["name"].(string)
		kind, _ := r["kind"].(string)
		switch kind {
		case "builtin":
			nBuiltin++
		case "pure":
			nPure++
		default:
			nExt++
		}
		sb.WriteString(fmt.Sprintf("\n[%s] %s\n", kind, name))
		switch ms := r["methods"].(type) {
		case map[string]engine.MethodSchema:
			for m, sch := range ms {
				sb.WriteString(fmt.Sprintf("  - %s: %s\n", m, sch.Description))
				for k, ps := range sch.Parameters {
					req := ""
					if ps.Required {
						req = " (必填)"
					}
					sb.WriteString(fmt.Sprintf("      · %s: %s — %s%s\n", k, ps.Type, ps.Description, req))
				}
			}
		case map[string]interface{}:
			for m := range ms {
				sb.WriteString(fmt.Sprintf("  - %s\n", m))
			}
		}
	}
	sb.WriteString(fmt.Sprintf("\n共 %d 个门禁原语、%d 个纯函数原子（始终可用、无需声明）、%d 个扩展原语。\n", nBuiltin, nPure, nExt))
	return sb.String()
}

// genCmd 是 `ai gen` 的父命令，承载公共选项（--intent / --register）。
// 参考配方默认内嵌（internal/engine/recipes/*.yaml）；examples/ai/recipes/ 或 GEAR_AI_RECIPES 可覆盖（可选）。
func genCmd() *cobra.Command {
	c := &cobra.Command{Use: "gen", Short: "生成 Flow 或 Tool"}
	c.PersistentFlags().StringP("intent", "i", "", "自然语言意图（也可用位置参数）")
	c.PersistentFlags().Bool("register", false, "通过后自动把草稿注册进 registry（否则仅打印草稿供人工确认）")
	c.AddCommand(
		&cobra.Command{
			Use:   "flow",
			Short: "用自然语言意图生成 Flow（组合已有 Tool）",
			RunE:  func(cmd *cobra.Command, args []string) error { return runAIGen(cmd, "flow", args) },
		},
		&cobra.Command{
			Use:   "tool",
			Short: "用自然语言意图生成单个 Tool（原子能力）草稿",
			RunE:  func(cmd *cobra.Command, args []string) error { return runAIGen(cmd, "tool", args) },
		},
	)
	return c
}

// runAIGen 实现 `ai gen flow/tool/hook`：构造进程内 ext.ai agent，注入真实工具目录后调用
// Generate 得到草稿并打印；--register 时把草稿落库（取代原「跑 ai-create-flow YAML + sidecar」）。
func runAIGen(cmd *cobra.Command, mode string, args []string) error {
	intent, _ := cmd.Flags().GetString("intent")
	if intent == "" {
		intent = strings.Join(args, " ")
	}
	if strings.TrimSpace(intent) == "" {
		return fmt.Errorf("请提供意图：--intent \"...\" 或位置参数")
	}
	register, _ := cmd.Flags().GetBool("register")

	// 1) 打开持久 registry（取目录工具 + 落库草稿）
	reg, err := openRegistry()
	if err != nil {
		return err
	}
	defer reg.Close()

	// 2) 内存 registry 跑 agent（注入 intent/mode），不污染持久 registry。
	refReg, err := registry.Open(fmt.Sprintf("file:gear_ai_cli_%d?mode=memory&cache=shared", os.Getpid()))
	if err != nil {
		return fmt.Errorf("打开内存 registry 失败: %w", err)
	}
	defer refReg.Close()

	// 3) 引擎 + 进程内 ext.ai（SetupExtensions 注册 ext.ai 与 ext.registry）
	eng, err := cliEngine(refReg)
	if err != nil {
		return err
	}
	skipped, err := seedRefRegistry(refReg, reg)
	if err != nil {
		return fmt.Errorf("灌入真实工具目录失败: %w", err)
	}
	warnSkipped(skipped)

	agent := eng.AIAgent()
	if agent == nil {
		return fmt.Errorf("ext.ai 未装配（SetupExtensions 失败？）")
	}

	opts := engine.GenerateOpts{}
	if register {
		opts.RegisterInto = reg
	}
	// 开发便利：生成阶段经 GEAR_AI_* 环境变量注入模型参数（原子能力本身不读 env）。
	if v := os.Getenv("GEAR_AI_PROVIDER"); v != "" {
		opts.Provider = v
	}
	if v := os.Getenv("GEAR_AI_MODEL"); v != "" {
		opts.Model = v
	}
	if v := os.Getenv("GEAR_AI_BASE_URL"); v != "" {
		opts.BaseURL = v
	}
	if v := os.Getenv("GEAR_AI_API_KEY"); v != "" {
		opts.APIKey = v
	}
	res, err := agent.Generate(context.Background(), mode, intent, opts)
	if err != nil {
		return fmt.Errorf("AI 生成失败: %w", err)
	}

	b, err := yaml.Marshal(res.Def)
	if err != nil {
		return fmt.Errorf("序列化草稿失败: %w", err)
	}
	cmd.Printf("=== %s 草稿 ===\n%s\n", strings.ToUpper(mode), string(b))
	if register {
		cmd.Printf("✓ 已注册 %s\n", mode)
	} else {
		cmd.Println("\n（未注册：加 --register 可把草稿落库，或人工修订后再注册）")
	}
	return nil
}

// seedRefRegistry 把持久 registry 的真实工具/流程灌入内存 refReg，使 agent 经
// ctx.ext.registry 读到真实目录（ext.registry 关闭于引擎执行所用的 registry，即 refReg）。
// 只灌真实内容，不灌参考 Flow 自身的 ai-gen/ai-create-flow（那些由 runAIGen 另行 upsert）。
// 灌入的是指针副本，不会写回持久 registry。
// seedRefRegistry 把持久 registry 的目录灌进内存 registry，返回**被跳过的定义 id**。
//
// 为什么跳过而不是报错退出：库里可能有过期定义（例如声明了已从内核删除的原语 kv——
// 实测 `mindstorm__ui_set` 就是这种残留）。seed 的目的是「让 agent 看见目录」，因为一条
// 坏记录就让整个 AI 能力不可用，代价与问题严重性不匹配。跳过并把 id 交回调用方点名，
// 用户才知道该去修哪条定义——静默跳过等于把坏定义藏起来。
func seedRefRegistry(refReg, reg *registry.Registry) ([]string, error) {
	tools, err := reg.ListTools()
	if err != nil {
		return nil, err
	}
	var skipped []string
	for _, t := range tools {
		if err := refReg.UpsertTool(t); err != nil {
			skipped = append(skipped, t.ID)
			continue
		}
	}
	flows, err := reg.ListFlows()
	if err != nil {
		return skipped, err
	}
	for _, f := range flows {
		if err := refReg.UpsertFlow(f); err != nil {
			skipped = append(skipped, f.ID)
			continue
		}
	}
	return skipped, nil
}

// warnSkipped 把被跳过的定义打到 stderr：不打断流程，但必须让用户看得见。
func warnSkipped(skipped []string) {
	if len(skipped) == 0 {
		return
	}
	fmt.Fprintf(os.Stderr, "警告：以下定义无法灌入 agent 目录（多半声明了不存在的原语，需重新注册）：%s\n", strings.Join(skipped, ", "))
}

// runAIComplete 实现 `ai complete "prompt"`：构造进程内 ext.ai agent，调 Complete（或 CompleteStream）。
// 这是 CLI 侧对流式原子能力的最小消费示例——REST SSE、Lua 回调式走同一 AIAgent.CompleteStream 通道。
func runAIComplete(cmd *cobra.Command, args []string) error {
	prompt := strings.Join(args, " ")
	if strings.TrimSpace(prompt) == "" {
		return fmt.Errorf("请提供 prompt：gear ai complete \"...\"")
	}
	reg, err := registry.Open(fmt.Sprintf("file:gear_ai_cli_%d?mode=memory&cache=shared", os.Getpid()))
	if err != nil {
		return fmt.Errorf("打开内存 registry 失败: %w", err)
	}
	defer reg.Close()
	eng, err := cliEngine(reg)
	if err != nil {
		return err
	}
	agent := eng.AIAgent()
	if agent == nil {
		return fmt.Errorf("ext.ai 未装配（SetupExtensions 失败？）")
	}

	system, _ := cmd.Flags().GetString("system")
	format, _ := cmd.Flags().GetString("format")
	model, _ := cmd.Flags().GetString("model")
	provider, _ := cmd.Flags().GetString("provider")
	baseURL, _ := cmd.Flags().GetString("base-url")
	apiKey, _ := cmd.Flags().GetString("api-key")
	stream, _ := cmd.Flags().GetBool("stream")

	// 开发便利：flag 未给时回退到 GEAR_AI_* 环境变量（原子能力本身只读调用参数，不读 env）。
	if provider == "" {
		provider = os.Getenv("GEAR_AI_PROVIDER")
	}
	if model == "" {
		model = os.Getenv("GEAR_AI_MODEL")
	}
	if baseURL == "" {
		baseURL = os.Getenv("GEAR_AI_BASE_URL")
	}
	if apiKey == "" {
		apiKey = os.Getenv("GEAR_AI_API_KEY")
	}

	reqArgs := map[string]interface{}{"prompt": prompt}
	if system != "" {
		reqArgs["system"] = system
	}
	if format != "" {
		reqArgs["format"] = format
	}
	if model != "" {
		reqArgs["model"] = model
	}
	if provider != "" {
		reqArgs["provider"] = provider
	}
	if baseURL != "" {
		reqArgs["base_url"] = baseURL
	}
	if apiKey != "" {
		reqArgs["api_key"] = apiKey
	}

	if !stream {
		res, err := agent.Complete(reqArgs)
		if err != nil {
			return fmt.Errorf("complete 失败: %w", err)
		}
		b, _ := yaml.Marshal(res)
		cmd.Println(string(b))
		return nil
	}
	// 流式：每个增量直接打印（reasoning 与 content 分开，便于区分推理过程）。
	return agent.CompleteStream(reqArgs, func(content, reasoning string) error {
		if reasoning != "" {
			cmd.Print(reasoning)
		}
		if content != "" {
			cmd.Print(content)
		}
		return nil
	})
}
