package registry

// 多文档 YAML 解析器（REQ-REG-MULTIDOC / REQ-REG-ATOMIC）的契约测试。
//
// `ParseYAMLDocs` 是**唯一**的多文档解析实现（单一真相源）：`LoadExamples`（文件）、
// CLI `tool/flow register`、MCP `create_tool/create_flow` 全部走它。它的失效形态很隐蔽——
// 「只读了第一个文档、其余静默丢弃」不会报错，只会让用户注册了 5 个只生效 1 个。
// 故这里把切分规则、Tool/Flow 判别、坏输入 fail-closed 逐条钉死。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestParseYAMLDocsMultiDocLeadsWithComment：首个 `---` 之前的注释块要被跳过，两篇文档各归其类。
func TestParseYAMLDocsMultiDocLeadsWithComment(t *testing.T) {
	src := `# 文件头注释（位于首个 --- 之前，应被跳过）
# 第二行注释
---
id: t1
name: T1
runtime: lua
entry: return {}
---
id: f1
steps:
  - id: s1
    call: t1
`
	tools, flows, err := ParseYAMLDocs([]byte(src))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(tools) != 1 || len(flows) != 1 {
		t.Fatalf("应解析出 1 Tool + 1 Flow，实际 %d + %d", len(tools), len(flows))
	}
	if tools[0].ID != "t1" || tools[0].Name != "T1" || tools[0].Runtime != "lua" {
		t.Fatalf("Tool 字段未正确解析: %+v", tools[0])
	}
	if flows[0].ID != "f1" {
		t.Fatalf("Flow ID 未正确解析: %+v", flows[0])
	}
	if len(flows[0].Steps) != 1 || flows[0].Steps[0].ID != "s1" || flows[0].Steps[0].Call != "t1" {
		t.Fatalf("Flow steps 未正确解析: %+v", flows[0].Steps)
	}
}

// TestParseYAMLDocsSplitsToolAndFlow：分类**只**看有无 `steps` 字段，
// 与 id/name/wheels 等命名无关（这是唯一判别口径，不得各自发挥）。
func TestParseYAMLDocsSplitsToolAndFlow(t *testing.T) {
	src := `---
id: flow-looking-tool
name: 名字像 Flow 的 Tool
runtime: lua
entry: return {}
---
id: tool-looking-flow
steps: []
`
	tools, flows, err := ParseYAMLDocs([]byte(src))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(tools) != 1 || tools[0].ID != "flow-looking-tool" {
		t.Fatalf("无 steps 的文档应归 Tool，实际 tools=%+v", tools)
	}
	if len(flows) != 1 || flows[0].ID != "tool-looking-flow" {
		t.Fatalf("含 steps 的文档应归 Flow（即使 steps 为空），实际 flows=%+v", flows)
	}
}

// TestParseYAMLDocsReportsBadDoc：任一篇坏掉即整体报错、**不返回半个批次**
// （避免「前 3 篇注册成功、第 4 篇静默丢失」这种半成功状态）。
func TestParseYAMLDocsReportsBadDoc(t *testing.T) {
	good := "---\nid: ok\nname: OK\nruntime: lua\nentry: return {}\n"
	bad := "---\nid: [未闭合的流式序列\n"
	tools, flows, err := ParseYAMLDocs([]byte(good + bad))
	if err == nil {
		t.Fatal("含非法 YAML 时必须报错")
	}
	if tools != nil || flows != nil {
		t.Fatalf("报错时不得返回部分结果（实际 tools=%d flows=%d）", len(tools), len(flows))
	}
	if !strings.Contains(err.Error(), "解析") {
		t.Fatalf("错误信息应说明是解析失败: %v", err)
	}
}

// TestParseYAMLDocsNoLeadingSeparator：首篇文档**不带**前导 `---` 也必须被解析出来。
// 这是 `testdata/hello_tool.yaml` 那种单文档写法与「多文档 + 前导注释」写法共存的保证——
// 若实现只在 `---` 之后才开始收集文档，单文档文件会被静默解析成空集。
func TestParseYAMLDocsNoLeadingSeparator(t *testing.T) {
	src := `id: solo
name: 单文档无前导分隔符
runtime: lua
entry: return {}
`
	tools, flows, err := ParseYAMLDocs([]byte(src))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(tools) != 1 || tools[0].ID != "solo" {
		t.Fatalf("无前导 --- 的单篇文档也应解析出 1 Tool，实际 %d 个: %+v", len(tools), tools)
	}
	if len(flows) != 0 {
		t.Fatalf("不应解析出 Flow，实际 %d 个", len(flows))
	}
}

// TestParseYAMLDocsEmptyInput：空输入/纯注释不算错误，返回空集（首尾空文档由实现跳过）。
func TestParseYAMLDocsEmptyInput(t *testing.T) {
	for _, src := range []string{"", "\n\n", "# 只有注释\n", "---\n---\n"} {
		tools, flows, err := ParseYAMLDocs([]byte(src))
		if err != nil {
			t.Fatalf("输入 %q 不应报错: %v", src, err)
		}
		if len(tools) != 0 || len(flows) != 0 {
			t.Fatalf("输入 %q 应解析出空集，实际 %d + %d", src, len(tools), len(flows))
		}
	}
}

// TestSplitYAMLDocsExported：切分规则只认「单独成行的 ---」——
// `----`、`--- x`、以及**作为 YAML 内容缩进**的 `---` 变体不得误切，
// 否则一篇文档会被切成两半，报出莫名其妙的「Flow 缺 id」。
func TestSplitYAMLDocsExported(t *testing.T) {
	src := "a: 1\n---\nb: 2\n----\nc: 3\n--- d\ne: 4\n"
	docs := SplitYAMLDocs([]byte(src))
	if len(docs) != 2 {
		t.Fatalf("只应在单独成行的 --- 处切分（期望 2 段），实际 %d 段: %q", len(docs), docs)
	}
	if !strings.Contains(docs[1], "b: 2") || !strings.Contains(docs[1], "c: 3") ||
		!strings.Contains(docs[1], "--- d") || !strings.Contains(docs[1], "e: 4") {
		t.Fatalf("第二段应含 b/c/--- d/e 全部内容，实际 %q", docs[1])
	}
}

// TestParseYAMLDocsRealExample：仓库里的真实示例必须能被解析器全链吃下。
// 锁的是「testdata/*.yaml 的写法 ↔ 解析器的口径」——两者一旦错位，
// 用户照着示例写却注册不上，属文档与代码的漂移。
func TestParseYAMLDocsRealExample(t *testing.T) {
	// ① 多文档示例（注释头 + Tool + Flow 两篇）：结构解析正确。
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "ai-create-flow.yaml"))
	if err != nil {
		t.Fatalf("读取示例失败: %v", err)
	}
	tools, flows, err := ParseYAMLDocs(b)
	if err != nil {
		t.Fatalf("解析示例失败: %v", err)
	}
	if len(tools) != 1 || tools[0].ID != "ai-gen" {
		t.Fatalf("示例应含 1 个 ai-gen Tool，实际 %+v", tools)
	}
	if len(flows) != 1 {
		t.Fatalf("示例应含 1 个 Flow，实际 %d 个", len(flows))
	}

	// ② 入门示例（hello）：解析产物必须真的能注册进 registry。
	// 「能解析」不等于「能注册」——两者口径错位就会让用户照抄示例却注册失败。
	// 这里刻意用无 ext 依赖的 hello（ai-gen 声明的 ext.ai 需扩展目录，属引擎层职责，
	// registry 只依赖内核原语门禁，见 cap.GrantsFromStrings）。
	toolDoc := mustRead(t, "hello_tool.yaml")
	flowDoc := mustRead(t, "hello_flow.yaml")
	ht, _, err := ParseYAMLDocs(toolDoc)
	if err != nil || len(ht) != 1 {
		t.Fatalf("解析 hello_tool.yaml 失败: %v (%d 个)", err, len(ht))
	}
	_, hf, err := ParseYAMLDocs(flowDoc)
	if err != nil || len(hf) != 1 {
		t.Fatalf("解析 hello_flow.yaml 失败: %v (%d 个)", err, len(hf))
	}
	reg := mustOpen(t)
	defer reg.Close()
	if err := reg.UpsertTool(ht[0]); err != nil {
		t.Fatalf("hello Tool 应可注册: %v", err)
	}
	if err := reg.UpsertFlow(hf[0]); err != nil {
		t.Fatalf("hello Flow 应可注册（且引用的 hello Tool 已就位）: %v", err)
	}
}

// mustRead 读 testdata 下的示例文件（路径相对本包目录）。
func mustRead(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", name))
	if err != nil {
		t.Fatalf("读取 testdata/%s 失败: %v", name, err)
	}
	return b
}
