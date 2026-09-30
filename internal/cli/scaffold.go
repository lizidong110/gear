package cli

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// scaffoldCmd 生成 Tool / Flow / Hook 的 YAML 骨架（带字段注释与示例），
// 让首次使用者在「不知道 YAML 怎么写」时也能拿到一份可直接 register 的模板。
//
// 设计原则（与项目红线一致）：
//   - 生成的文件必须能通过对应 Validate（tool/hook 走 model.Validate，flow 走 engine.ValidateFlowStatic），
//     否则骨架本身就是个会让人困惑的「错误样本」。
//   - 默认打印到 stdout（管道友好）；-o/--output 落盘；-f/--force 覆盖已存在文件。
//   - 字段注释里把「必填 / 取值 / 示例」写清楚，等于把 README §8 的 schema 表搬到了命令里。
func scaffoldCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "scaffold",
		Short: "生成 Tool / Flow / Hook 的 YAML 骨架（带字段注释与示例）",
		Long: `scaffold 生成一份「可直接 register」的 YAML 模板，并就地标注每个字段的用途与取值。

用法：
  gear scaffold tool  [name] [-o file] [--runtime process|lua|go-host] [--force]
  gear scaffold flow  [name] [-o file] [--force]
  gear scaffold hook  [name] [-o file] [--force]

  name 省略时默认 my-tool / my-flow / my-hook。
  -o, --output <file>  写到文件（默认打印到 stdout，便于管道：gear scaffold tool x -o x.yaml）。
  -f, --force          目标文件已存在时覆盖（默认拒绝，避免误覆盖）。

生成后直接注册即可上手：
  gear scaffold tool my-tool -o my-tool.yaml && gear tool register my-tool.yaml
  gear scaffold flow my-flow -o my-flow.yaml && gear flow register my-flow.yaml
  gear scaffold hook my-hook -o my-hook.yaml && gear hook register my-hook.yaml

想看某类资源的完整字段说明，执行 gear scaffold <tool|flow|hook> --help。`,
	}
	c.AddCommand(scaffoldToolCmd(), scaffoldFlowCmd(), scaffoldHookCmd())
	return c
}

// scaffoldToolCmd 生成 Tool 骨架。支持 --runtime 选择 process / lua / go-host。
func scaffoldToolCmd() *cobra.Command {
	var output, runtime string
	var force bool
	c := &cobra.Command{
		Use:   "tool [name]",
		Short: "生成 Tool（原子能力）YAML 骨架",
		Long: `Tool 是原子能力（最小可调用单元），含 input/output schema，彼此无感知、不感知 Hook。

骨架已填好「可直接 gear tool register」的必填项，TODO 注释处由你补充。

顶层字段（YAML tag）：
  id            string   必填  唯一 ID（蛇形小写，如 my-tool）
  name          string   选填  显示名
  description   string   选填  用途说明
  runtime       string   必填  process | lua | go-host | wasm
                              - process ：外部命令，stdin 进 JSON、stdout 末行出 JSON
                              - lua     ：嵌入式 Lua 脚本（沙箱），entry 即脚本本体
                              - go-host ：进程内 Go 函数，entry 为函数名
                                          （须在 Go 代码 engine.RegisterHostFunc 注册，否则注册期拒绝）
                              - wasm    ：已知但未实现，注册期即拒
  entry         string   必填  process=命令；lua/go-host=函数引用
  input_schema  map      必填  JSON Schema，顶层必须 type: object
  output_schema map      必填  JSON Schema，顶层必须 type: object
  versions      []        必填  至少 1 个；每项需 version(SemVer) + state
  capabilities  []string  选填  最小权限白名单（见下）
  health_probe  object    选填  service 模式探针（仅 service step 调用时生效）

versions[].state 取值：draft / testing / stable / deprecated / rejected
capabilities 合法值（最小权限，未声明即不能用）：
  - 受门禁内核原语（须声明才可用）：http / call / db / secret / event / subscribe / config
  - 纯函数原子（始终可用，无需声明）：time / template / crypto
  - 扩展原语 ext.*（须 extensions.yaml 登记，如 ext.ai / ext.email / ext.kafka）

health_probe：kind(http|tcp|cmd) + addr(主机:端口) / cmd(命令) + interval(默认5s)
  + timeout(默认2s) + threshold(连续失败几次重启，默认3)。

示例：
  gear scaffold tool my-tool -o my-tool.yaml
  gear tool register my-tool.yaml
  gear tool run my-tool --arg name=world`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := "my-tool"
			if len(args) > 0 && args[0] != "" {
				name = args[0]
			}
			content, err := scaffoldToolYAML(name, runtime)
			if err != nil {
				return err
			}
			return emitScaffold(cmd, content, output, force)
		},
	}
	c.Flags().StringVarP(&output, "output", "o", "", "写到文件而非 stdout（默认打印）")
	c.Flags().StringVar(&runtime, "runtime", "process", "Tool runtime：process | lua | go-host（wasm 未实现）")
	c.Flags().BoolVarP(&force, "force", "f", false, "目标文件已存在时覆盖")
	return c
}

// scaffoldFlowCmd 生成 Flow 骨架。起步 step 用纯函数原子 crypto.hash（无需声明、无网络，
// 注册即可跑通），并以注释展示「调用已注册 Tool」与「http.Request + when 条件」两种进阶写法。
func scaffoldFlowCmd() *cobra.Command {
	var output string
	var force bool
	c := &cobra.Command{
		Use:   "flow [name]",
		Short: "生成 Flow（通用运行单元）YAML 骨架",
		Long: `Flow 是通用运行单元：声明式编排多个 Tool / 原子能力，长驻不退出的 Flow 即「服务」。

骨架已含一个可直接跑通的 step（纯函数原子 crypto.hash），并注释展示了更多写法。

Flow 字段：
  id          string   必填  唯一 ID
  name        string   选填  显示名
  description string   选填  用途说明
  vars        map      选填  Flow 级变量；flow run --var 可覆盖；模板 ${vars.x} 引用
  steps       []Step   必填  至少 1 个

Step 字段：
  id           string   必填  步骤 ID（同 Flow 内唯一）
  call         string   必填  两种写法：
                         1) 已注册 Tool 的 id（如 my-tool）
                         2) 原子能力直调「能力.方法」（如 crypto.hash / http.Request /
                            db.query / secret.get / ext.ai.complete）
  input        map      选填  入参；值里可写模板（见下）
  depends_on   []string 选填  上游 step id（input 里出现 ${steps.x.*} 也会隐式依赖）
  when         string   选填  条件执行（表达式，不是 ${} 模板），如 "steps.s1.output.n > 1"
  parallel     bool     选填  显式并行（无依赖 step 默认就并行）
  mode         string   选填  once(默认) | service(长驻，仅 Tool 载体；须 gear app run/start)

模板引用（input 值里）：
  ${vars.x}                 Flow 变量（x 须是 vars 顶层键）
  ${steps.id.output.field}  上游 step 出参（id 须存在）
  ${secrets.name}           密钥（运行期从 GEAR_SECRET_<NAME> 或 /api/secrets 取，不落明文）

注意：service 模式（mode: service）的 Flow 必须 gear app run（前台监管，人/CI 持有）或
      gear app start（派生独立后台进程，立即返回、跨会话存活），flow run 会显式拒绝。

示例：
  gear scaffold flow my-flow -o my-flow.yaml
  gear flow register my-flow.yaml
  gear flow run my-flow --var topic=world`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := "my-flow"
			if len(args) > 0 && args[0] != "" {
				name = args[0]
			}
			content := scaffoldFlowYAML(name)
			return emitScaffold(cmd, content, output, force)
		},
	}
	c.Flags().StringVarP(&output, "output", "o", "", "写到文件而非 stdout（默认打印）")
	c.Flags().BoolVarP(&force, "force", "f", false, "目标文件已存在时覆盖")
	return c
}

// scaffoldHookCmd 生成 Hook 骨架。
func scaffoldHookCmd() *cobra.Command {
	var output string
	var force bool
	c := &cobra.Command{
		Use:   "hook [name]",
		Short: "生成 Hook（横切控制）YAML 骨架",
		Long: `Hook 在指定生命周期节点插入执行逻辑（认证 / 脱敏 / 限流 / 日志 / 汇总），
由宿主策略显式装配，而非插件私下约定耦合。

Hook 字段：
  id          string   必填  唯一 ID
  target      string   必填  匹配对象：step 级按 tool id、flow 级按 flow id；"*" = 全部
  stage       string   必填  触发节点（见下）
  action      string   必填  已注册 Tool id 或 Flow id（Hook 逻辑本身是一个 Tool/Flow）
  criticality string   选填  critical(默认, 失败即终止编排) | best-effort(失败仅记日志)
  order       int      选填  装配顺序（升序执行；同序按注册先后）

stage 合法值（已实现）：
  before_flow     Flow 开始前（可改写初始 vars）            —— Flow 级
  before_step     step 执行前（可改写 input）               —— step 级
  after_step      step 执行后（可改写 output）              —— step 级
  on_error        step 失败时（仅补偿/告警，不恢复主流程）  —— step 级
  on_success      Flow 全部成功时（汇总通知/收尾）          —— Flow 级
  before_promote  版本晋升前（S5 由 PromoteTool 触发）

匹配维度由 stage 决定：step 级 stage 按 step 的 tool id 匹配；Flow 级 stage 按 flow id 匹配；
两者都支持 "*" 表示全部。action 指向的 Tool/Flow 须在运行期已注册（注册期不校验其存在）。

示例：
  gear scaffold hook my-hook -o my-hook.yaml
  gear hook register my-hook.yaml
  gear hook list`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := "my-hook"
			if len(args) > 0 && args[0] != "" {
				name = args[0]
			}
			content := scaffoldHookYAML(name)
			return emitScaffold(cmd, content, output, force)
		},
	}
	c.Flags().StringVarP(&output, "output", "o", "", "写到文件而非 stdout（默认打印）")
	c.Flags().BoolVarP(&force, "force", "f", false, "目标文件已存在时覆盖")
	return c
}

// emitScaffold 把生成的骨架写到文件（带 force 守卫）或 stdout。
func emitScaffold(cmd *cobra.Command, content, output string, force bool) error {
	if output == "" {
		fmt.Print(content)
		return nil
	}
	if _, err := os.Stat(output); err == nil && !force {
		return fmt.Errorf("%s 已存在；用 --force 覆盖", output)
	}
	f, err := os.Create(output)
	if err != nil {
		return fmt.Errorf("创建 %s 失败: %w", output, err)
	}
	defer f.Close()
	if _, err := io.WriteString(f, content); err != nil {
		return fmt.Errorf("写入 %s 失败: %w", output, err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "✓ 已生成骨架：%s\n", output)
	return nil
}

// scaffoldToolYAML 返回一份可直接 gear tool register 的 Tool 骨架。
// runtime 选择 process / lua / go-host（wasm 未实现，给出提示但不强行生成）。
func scaffoldToolYAML(name, runtime string) (string, error) {
	rt := strings.TrimSpace(runtime)
	if rt == "" {
		rt = "process"
	}
	switch rt {
	case "process", "lua", "go-host":
		// ok
	case "wasm":
		return "", fmt.Errorf("runtime=wasm 尚未实现（注册期会被拒绝）；请改用 process / lua / go-host")
	default:
		return "", fmt.Errorf("非法 runtime %q（应为 process / lua / go-host）", rt)
	}

	var entryBlock string
	switch rt {
	case "process":
		entryBlock = "entry: cat                 # process=命令（stdin 进 JSON，stdout 末行出 JSON）；cat 为透传示例，替换成你的命令"
	case "lua":
		entryBlock = "entry: |\n" +
			"  -- 入参：ctx.input（map）；出参：写 ctx.output（map）\n" +
			"  local name = ctx.input.name or \"world\"\n" +
			"  ctx.output.name = \"hello, \" .. name"
	case "go-host":
		entryBlock = "entry: myFunc             # 须先在 Go 代码 engine.RegisterHostFunc(\"myFunc\", ...) 注册，否则注册期拒绝"
	}

	const tmpl = `# Gear Tool 骨架 —— 由 gear scaffold tool %s 生成
# 注册：gear tool register <本文件>          校验：model.Tool.Validate()
# 完整字段说明见 gear scaffold tool --help 与 README §8。
id: %s
name: %s
description: "TODO: 用一句话描述这个原子能力做什么"
runtime: %s
%s
capabilities: []            # 最小权限白名单：http/call/db/secret/event/subscribe/config；ext.* 需 extensions.yaml 登记；time/template/crypto/id/validate 无需声明
input_schema:
  type: object
  properties:
    # TODO: 定义入参（JSON Schema）
    name:
      type: string
      description: 示例入参
  required: [name]
output_schema:
  type: object
  properties:
    # TODO: 定义出参（JSON Schema）；此处与入参一致（cat 透传）
    name:
      type: string
      description: 透传出参（cat 把输入原样回显；替换 entry 后改这里）
  required: [name]
versions:
  - version: 1.0.0
    state: stable             # draft / testing / stable / deprecated / rejected
    note: 初始版本
`
	return fmt.Sprintf(tmpl, name, name, name, rt, entryBlock), nil
}

// scaffoldFlowYAML 返回一份可直接 gear flow register 的 Flow 骨架。
// 起步 step 用纯函数原子 crypto.hash（无需声明 capabilities、无网络），注册即可跑通。
func scaffoldFlowYAML(name string) string {
	const tmpl = `# Gear Flow 骨架 —— 由 gear scaffold flow %s 生成
# 注册：gear flow register <本文件>    校验：engine.ValidateFlowStatic()
# 完整字段说明见 gear scaffold flow --help 与 README §8。
#
# step.call 两种写法：
#   1) 已注册 Tool 的 id（如 my-tool）
#   2) 原子能力直调「能力.方法」（如 crypto.hash / http.Request / db.query / ext.ai.complete）
# 模板：${vars.x}（Flow 变量）/ ${steps.id.output.field}（上游出参）/ ${secrets.name}（密钥）
# 条件执行：when: "steps.hash.output.result != ''"（表达式，不是 ${} 模板）
id: %s
name: %s
description: "TODO: 描述这个编排做什么"
vars:
  topic: world             # TODO: Flow 级变量；flow run --var topic=foo 可覆盖；模板 ${vars.topic} 引用
steps:
  - id: hash
    call: crypto.hash       # 纯函数原子，无需声明 capabilities，可直接跑通（演示用）
    input:
      algo: sha256
      data: "${vars.topic}"
  # 取消注释即可体验更多写法：
  # - id: calltool
  #   call: my-tool          # 改成你已注册的 Tool id
  #   depends_on: [hash]
  #   input:
  #     name: "${steps.hash.output.result}"
  # - id: fetch
  #   call: http.Request
  #   when: "steps.hash.output.result != ''"   # 条件执行（表达式）
  #   input:
  #     method: GET
  #     url: https://example.com
`
	return fmt.Sprintf(tmpl, name, name, name)
}

// scaffoldHookYAML 返回一份可直接 gear hook register 的 Hook 骨架。
func scaffoldHookYAML(name string) string {
	const tmpl = `# Gear Hook 骨架 —— 由 gear scaffold hook %s 生成
# 注册：gear hook register <本文件>    校验：model.Hook.Validate()
# 完整字段说明见 gear scaffold hook --help 与 README §8。
id: %s
target: "*"                # 匹配对象：step 级按 tool id、flow 级按 flow id；"*" = 全部
stage: before_step         # before_flow / before_step / after_step / on_error / on_success / before_promote
action: my-tool            # 已注册 Tool id 或 Flow id（Hook 逻辑本身是一个 Tool/Flow）
criticality: critical      # critical(默认, 失败即终止编排) / best-effort(失败仅记日志)
order: 10                  # 装配顺序（升序执行；同序按注册先后）
`
	return fmt.Sprintf(tmpl, name, name)
}
