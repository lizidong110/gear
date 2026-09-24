package mcp

// mcp_review_test.go —— MCP 接口评审（MCP_接口与协议评审_v1.md）各项修复的回归锁：
//
//	I-1 执行上下文的取消传播 + 有界并发（背压）+ ④孤儿执行识别；
//	I-4 gear_app 破坏性标注；I-5 guide topic 单一真相源；I-6 outputSchema；
//	I-7 大 payload 分页/裁剪；I-8 能力直调（gear_invoke）；
//	O-1..O-6 能力协商如实性、协议版本钉死、resources 模板与单测；
//	以及一条「纯 MCP 接入者」验收闭环（只读 tools/list + resources + gear_guide 即可走通）。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"gear/internal/app"
	"gear/internal/engine"
	"gear/internal/execstore"
	"gear/internal/mcp/guide"
	"gear/internal/registry"
)

// capsWire 把服务端声明的 capabilities 序列化成 JSON 文本。
//
// 为什么不直接读 `caps.Logging` 字段：该字段在 SDK 里已标记 **Deprecated**（SEP-2577），
// 直接读会触发 staticcheck SA1019（闸门 ④ 会红）。序列化后查键名既绕开废弃 API，
// 又恰好断言的正是「客户端在线上真正收到什么」——比读结构体字段更接近事实。
func capsWire(t *testing.T, caps *mcp.ServerCapabilities) string {
	t.Helper()
	b, err := json.Marshal(caps)
	if err != nil {
		t.Fatalf("序列化 capabilities 失败: %v", err)
	}
	return string(b)
}

// ---- I-6 ----

// TestMCPToolsAdvertiseOutputSchema：每个工具都必须声明 outputSchema，且是「只列键、不设约束」的
// 对象 schema。
func TestMCPToolsAdvertiseOutputSchema(t *testing.T) {
	session := newSmokeSession(t)
	tools, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools.Tools) == 0 {
		t.Fatal("工具面为空")
	}
	for _, tt := range tools.Tools {
		if tt.OutputSchema == nil {
			t.Errorf("工具 %s 未声明 outputSchema（客户端将无法静态知道返回形状）", tt.Name)
			continue
		}
		m, ok := tt.OutputSchema.(map[string]any)
		if !ok {
			t.Errorf("工具 %s 的 outputSchema 应为对象，实际 %T", tt.Name, tt.OutputSchema)
			continue
		}
		if m["type"] != "object" {
			t.Errorf("工具 %s 的 outputSchema 顶层应为 object，实际 %v", tt.Name, m["type"])
		}
		props, ok := m["properties"].(map[string]any)
		if !ok || len(props) == 0 {
			t.Errorf("工具 %s 的 outputSchema 应列出具名 properties", tt.Name)
		}
		// 红线：SDK 会对**每次**返回做 schema 校验（server.go toolForErr → applySchema），
		// 因此绝不能设 required / 禁 additionalProperties——合法返回里缺失/多余的键会被判失败。
		if _, has := m["required"]; has {
			t.Errorf("工具 %s 的 outputSchema 不得设 required（会把合法返回拒掉）", tt.Name)
		}
		if ap, has := m["additionalProperties"]; has && ap == false {
			t.Errorf("工具 %s 的 outputSchema 不得禁 additionalProperties", tt.Name)
		}
	}
}

// ---- I-1 ----

// TestExecutionSnapshotStopsOnCancel：客户端断开 / 超时（ctx 取消）时，executionSnapshot 的忙等
// 必须**立刻**停止并如实返回当前快照——此前不看 ctx.Done()，客户端早走了还会等满 N 秒。
func TestExecutionSnapshotStopsOnCancel(t *testing.T) {
	f := newSmokeFixture(t)
	rec := f.server.execs.Create("f") // 造一条「运行中」的执行（故意不 Finish）

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 立即取消

	start := time.Now()
	snap := f.server.executionSnapshot(ctx, rec.ID, 30)
	elapsed := time.Since(start)

	if elapsed > 3*time.Second {
		t.Fatalf("ctx 已取消仍继续忙等（等了 %s，应为毫秒级）", elapsed)
	}
	if snap["status"] != "running" {
		t.Fatalf("取消后应如实返回当前快照（running），实际 %v", snap["status"])
	}
}

// TestStartFlowExecBackpressure：在途执行达并发上限时，startFlowExec 必须**背压拒绝**
// （EXECUTION_BUSY），而不是无限起 goroutine。
func TestStartFlowExecBackpressure(t *testing.T) {
	f := newSmokeFixtureWith(t, func(reg *registry.Registry) { applySmokeBundle(t, reg) })

	// 灌满并发槽，模拟「在途执行已满」。
	for i := 0; i < maxConcurrentExecs; i++ {
		f.server.execSem <- struct{}{}
	}
	defer func() {
		for i := 0; i < maxConcurrentExecs; i++ {
			<-f.server.execSem
		}
	}()

	_, err := f.server.startFlowExec("smoke.up", nil)
	if err == nil {
		t.Fatal("并发已达上限时应背压拒绝，实际放行了一条新执行")
	}
	ds := engine.Diagnostics(err)
	if len(ds) == 0 || ds[0].Code != "EXECUTION_BUSY" {
		t.Fatalf("应返回 EXECUTION_BUSY 码，实际 %v", err)
	}
}

// ---- I-1④ 孤儿执行识别 ----

// TestOrphanExecutionDetection：只有「属主进程已不存在」才算孤儿，且**绝不靠时长猜**。
func TestOrphanExecutionDetection(t *testing.T) {
	f := newSmokeFixture(t)
	s := f.server

	// ① 属主就是本进程 → 正常在途，不是孤儿。
	if orphan, why := s.orphanStatus(execstore.Execution{Status: execstore.StatusRunning, OwnerPID: os.Getpid()}); orphan {
		t.Fatalf("本进程在途的执行不得判为孤儿: %s", why)
	}
	// ② 无属主信息（老记录，owner_pid=0）→ **不猜**。
	if orphan, _ := s.orphanStatus(execstore.Execution{Status: execstore.StatusRunning}); orphan {
		t.Fatal("缺少属主 pid 时不得臆断为孤儿")
	}
	// ③ 已结束的执行 → 不是孤儿（即便属主进程早没了）。
	dead := deadPID(t)
	if orphan, _ := s.orphanStatus(execstore.Execution{Status: execstore.StatusSucceeded, OwnerPID: dead}); orphan {
		t.Fatal("已结束的执行不得判为孤儿")
	}
	// ④ 属主进程确已不存在 → 孤儿，且给出可解释的原因。
	orphan, why := s.orphanStatus(execstore.Execution{Status: execstore.StatusRunning, OwnerPID: dead})
	if !orphan || why == "" {
		t.Fatalf("属主进程不存在时应判为孤儿并给出原因，实际 orphan=%v why=%q", orphan, why)
	}
	if !strings.Contains(why, fmt.Sprint(dead)) {
		t.Fatalf("孤儿原因应指出属主 pid（%d）: %s", dead, why)
	}

	// 集成侧：本进程创建的 running 记录在 gear_execution 列表里**不得**被标孤儿（负向方向）。
	rec := s.execs.Create("smoke.up")
	var list map[string]any
	callTool(t, f.session, "gear_execution", nil, &list)
	ex, _ := list["executions"].([]any)
	var found bool
	for _, it := range ex {
		m, _ := it.(map[string]any)
		if m["id"] != rec.ID {
			continue
		}
		found = true
		if _, has := m["orphan"]; has {
			t.Fatalf("本进程在途的执行在列表里不得标 orphan: %v", m)
		}
	}
	if !found {
		t.Fatalf("刚创建的 execution %s 应出现在列表里: %v", rec.ID, list)
	}
}

// deadPID 返回一个**确定已退出**的进程 pid（跑一个立刻结束的子进程并等它被回收）。
//
// 为什么不硬编码一个很大的数：pid 空间与回收语义随平台而变，硬编码等于把「碰巧为假」
// 当成判据。这里显式确认它确实不存活，否则跳过（不制造假绿）。
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestZZZNoSuchTestZZZ")
	cmd.Stdout, cmd.Stderr = nil, nil
	if err := cmd.Run(); err != nil {
		t.Fatalf("起子进程失败: %v", err)
	}
	pid := cmd.ProcessState.Pid()
	if app.ProcessAlive(pid) {
		t.Skipf("子进程 %d 退出后仍判为存活——本平台判据不同，跳过孤儿正向用例", pid)
	}
	return pid
}

// ---- I-4 gear_app 破坏性标注 ----

// TestMCPAppToolMarkedDestructive：gear_app 含 stop / prune 破坏性动作，annotations 必须
// 标 DestructiveHint（工具级 annotation 表达不了「部分动作破坏性」，按宁可过度提示取舍）。
func TestMCPAppToolMarkedDestructive(t *testing.T) {
	session := newSmokeSession(t)
	tools, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	var found bool
	for _, tt := range tools.Tools {
		if tt.Name != "gear_app" {
			continue
		}
		found = true
		if tt.Annotations == nil || tt.Annotations.DestructiveHint == nil || !*tt.Annotations.DestructiveHint {
			t.Fatalf("gear_app 必须标 DestructiveHint（含 stop/prune）: %+v", tt.Annotations)
		}
	}
	if !found {
		t.Fatal("工具面里没有 gear_app")
	}
}

// TestMCPAppStopEchoesDestructive：破坏性动作在**返回值**里也回显，供只读结果的客户端识别。
func TestMCPAppStopEchoesDestructive(t *testing.T) {
	f := newSmokeFixture(t)
	callTool(t, f.session, "gear_apply", map[string]any{"bundle": svcAppBundleYAML}, nil)
	var started map[string]any
	callTool(t, f.session, "gear_app", map[string]any{
		"action": "start", "flow_id": "svcapp.up", "restart": "never",
	}, &started)
	appID, _ := started["app_id"].(string)
	if appID == "" {
		t.Fatalf("start 应返回 app_id: %v", started)
	}
	var stopped map[string]any
	callTool(t, f.session, "gear_app", map[string]any{"action": "stop", "app_id": appID}, &stopped)
	if stopped["destructive"] != true {
		t.Fatalf("stop 的返回必须回显 destructive:true: %v", stopped)
	}

	// prune：dry_run 未实际改动 → 不标破坏性；真 prune → 标。
	var dry map[string]any
	callTool(t, f.session, "gear_app", map[string]any{"action": "prune", "dry_run": true}, &dry)
	if dry["destructive"] == true {
		t.Fatalf("prune(dry_run) 未改动任何东西，不应标 destructive: %v", dry)
	}
	var pruned map[string]any
	callTool(t, f.session, "gear_app", map[string]any{"action": "prune"}, &pruned)
	if pruned["destructive"] != true {
		t.Fatalf("真 prune 应标 destructive:true: %v", pruned)
	}
}

// ---- I-5 guide topic 单一真相源 ----

// TestMCPGuideTopicsSingleSource：topic 清单的唯一真相源是磁盘，schema 文案**不得枚举**，
// 且每个磁盘 topic 都真能取到、返回值与磁盘集合一致。
func TestMCPGuideTopicsSingleSource(t *testing.T) {
	f := newSmokeFixture(t)

	// ① schema 文案里不得出现任何 topic 名（否则必然随磁盘演进静默过期——曾经漏过 rest-api）。
	tag, ok := reflect.TypeOf(GuideArgs{}).Field(0).Tag.Lookup("jsonschema")
	if !ok {
		t.Fatal("GuideArgs.Topic 缺少 jsonschema 标签")
	}
	for _, topic := range guide.Topics() {
		if strings.Contains(tag, topic) {
			t.Fatalf("GuideArgs 的 schema 文案枚举了 topic %q——单一真相源是磁盘，不应枚举；文案: %s", topic, tag)
		}
	}

	// ② 每个磁盘 topic 都能取到，且返回的 topics 字段与磁盘集合逐项一致。
	disk := guide.Topics()
	for _, topic := range disk {
		var out map[string]any
		callTool(t, f.session, "gear_guide", map[string]any{"topic": topic}, &out)
		if out["topic"] != topic {
			t.Fatalf("gear_guide(%s) 应回显 topic，实际 %v", topic, out["topic"])
		}
		got, _ := out["topics"].([]any)
		if len(got) != len(disk) {
			t.Fatalf("返回的 topics 数量 %d != 磁盘 %d: %v", len(got), len(disk), got)
		}
		for i, g := range got {
			if g != disk[i] {
				t.Fatalf("topics[%d]=%v 与磁盘 %v 不一致", i, g, disk[i])
			}
		}
	}

	// ③ rest-api 必须在列（评审点名的那个漏项）。
	var found bool
	for _, topic := range disk {
		if topic == "rest-api" {
			found = true
		}
	}
	if !found {
		t.Fatalf("topic 清单应含 rest-api，实际 %v", disk)
	}
}

// ---- I-7 分页与裁剪 ----

// TestMCPPaginationAndFilters：catalog 支持 offset 翻页、execution 有 limit、capabilities 可过滤。
func TestMCPPaginationAndFilters(t *testing.T) {
	f := newSmokeFixture(t)
	callTool(t, f.session, "gear_apply", map[string]any{"bundle": smokeBundleYAML}, nil) // 1 tool + 1 flow

	// catalog：limit=1 → truncated + next_offset；按 next_offset 取到**不同**的第二项。
	var page1 map[string]any
	callTool(t, f.session, "gear_catalog", map[string]any{"limit": 1}, &page1)
	if page1["truncated"] != true || fmt.Sprint(page1["total"]) != "2" {
		t.Fatalf("limit=1 应标注 truncated 且 total=2: %v", page1)
	}
	next, ok := page1["next_offset"].(float64)
	if !ok || int(next) != 1 {
		t.Fatalf("next_offset 应为 1，实际 %v", page1["next_offset"])
	}
	p1, _ := page1["items"].([]any)
	first, _ := p1[0].(map[string]any)

	var page2 map[string]any
	callTool(t, f.session, "gear_catalog", map[string]any{"limit": 1, "offset": int(next)}, &page2)
	p2, _ := page2["items"].([]any)
	if len(p2) != 1 {
		t.Fatalf("offset=1 应返回 1 项: %v", page2)
	}
	second, _ := p2[0].(map[string]any)
	if second["id"] == first["id"] {
		t.Fatalf("翻页应返回不同的项: %v vs %v", first, second)
	}
	if page2["truncated"] != false {
		t.Fatalf("取完最后一页不应再标 truncated: %v", page2)
	}

	// execution：默认不超过 defaultExecutionListLimit，显式 limit 生效。
	for i := 0; i < 3; i++ {
		callTool(t, f.session, "gear_run", map[string]any{
			"kind": "flow", "id": "smoke.up", "wait_seconds": 15,
		}, nil)
	}
	var ex map[string]any
	callTool(t, f.session, "gear_execution", map[string]any{"limit": 2}, &ex)
	list, _ := ex["executions"].([]any)
	if len(list) != 2 {
		t.Fatalf("limit=2 应只返回 2 条，实际 %d: %v", len(list), ex)
	}
	if fmt.Sprint(ex["total"]) != "3" {
		t.Fatalf("total 应是全量计数 3: %v", ex)
	}
	if _, has := ex["hint"]; !has {
		t.Fatalf("被截断时应给出 hint: %v", ex)
	}

	// capabilities：name / kind 过滤；无匹配时报空 + hint（不是错误）。
	var byName map[string]any
	callTool(t, f.session, "gear_capabilities", map[string]any{"name": "db"}, &byName)
	caps, _ := byName["capabilities"].([]any)
	if len(caps) != 1 {
		t.Fatalf("name=db 应恰好返回 1 项: %v", byName)
	}
	if m, _ := caps[0].(map[string]any); m["name"] != "db" {
		t.Fatalf("过滤结果应是 db: %v", caps[0])
	}
	var byKind map[string]any
	callTool(t, f.session, "gear_capabilities", map[string]any{"kind": "ext"}, &byKind)
	extCaps, _ := byKind["capabilities"].([]any)
	if len(extCaps) == 0 {
		t.Fatalf("kind=ext 应至少返回 ext.ai: %v", byKind)
	}
	for _, c := range extCaps {
		if m, _ := c.(map[string]any); m["kind"] != "ext" {
			t.Fatalf("kind=ext 过滤结果混入了非 ext: %v", m)
		}
	}
	var none map[string]any
	callTool(t, f.session, "gear_capabilities", map[string]any{"name": "no_such_cap"}, &none)
	if nl, _ := none["capabilities"].([]any); len(nl) != 0 {
		t.Fatalf("无匹配应返回空数组: %v", none)
	}
	if _, has := none["hint"]; !has {
		t.Fatalf("无匹配时应给出可用能力提示: %v", none)
	}
}

// ---- I-8 能力直调 ----

// TestMCPInvokeCapability：gear_invoke 能直调原子能力；secret.* 被禁；未知方法报结构化错。
func TestMCPInvokeCapability(t *testing.T) {
	session := newSmokeSession(t)

	// 正常路径：纯函数原子（确定性、离线）。base64("hi") == "aGk="。
	var ok map[string]any
	callTool(t, session, "gear_invoke", map[string]any{
		"name": "crypto", "method": "base64_encode", "args": map[string]any{"data": "hi"},
	}, &ok)
	if ok["result"] != "aGk=" {
		t.Fatalf("gear_invoke(crypto.base64_encode) 应返回 aGk=，实际 %v", ok)
	}
	if ok["name"] != "crypto" || ok["method"] != "base64_encode" {
		t.Fatalf("应回显 name/method: %v", ok)
	}

	// 缺参 → 引擎的 schema 校验拒绝（结构化码）。
	text := expectToolError(t, session, "gear_invoke", map[string]any{
		"name": "crypto", "method": "base64_encode", "args": map[string]any{},
	}, "")
	if !strings.Contains(text, "base64_encode") {
		t.Fatalf("缺必填应指出方法名: %s", text)
	}

	// 未知方法 → 结构化错误（不是裸文本）。
	expectToolError(t, session, "gear_invoke", map[string]any{
		"name": "crypto", "method": "no_such_method",
	}, "")

	// secret.* 被禁（与 REST 面同一条禁则）。
	blocked := expectToolError(t, session, "gear_invoke", map[string]any{
		"name": "secret", "method": "get", "args": map[string]any{"name": "X"},
	}, "")
	if !strings.Contains(blocked, "INVOKE_BLOCKED") {
		t.Fatalf("调 secret.* 应报 INVOKE_BLOCKED: %s", blocked)
	}

	// 空 name/method → BAD_ARGUMENT。
	expectToolError(t, session, "gear_invoke", map[string]any{"name": "", "method": ""}, "BAD_ARGUMENT")
}

// ---- O-1/O-2/O-3 能力协商如实性 ----

// TestMCPCapabilitiesAndProtocolAreHonest：不声明 logging、listChanged=false、协议版本被钉死。
func TestMCPCapabilitiesAndProtocolAreHonest(t *testing.T) {
	session := newSmokeSession(t)
	init := session.InitializeResult()
	if init == nil {
		t.Fatal("客户端未拿到 initialize 结果")
	}
	if init.ProtocolVersion != supportedProtocolVersion {
		t.Fatalf("协议版本应被钉死为 %s，实际 %q", supportedProtocolVersion, init.ProtocolVersion)
	}
	caps := init.Capabilities
	if caps == nil {
		t.Fatal("服务端未声明 capabilities")
	}
	if wire := capsWire(t, caps); strings.Contains(wire, `"logging"`) {
		t.Fatalf("不得声明 logging——本包从不发日志通知，声明了就是骗客户端（评审 O-1）；实际: %s", wire)
	}
	if caps.Tools == nil || caps.Tools.ListChanged {
		t.Fatalf("tools 的 listChanged 应为 false（运行期从不变化）: %+v", caps.Tools)
	}
	if caps.Resources == nil || caps.Resources.ListChanged {
		t.Fatalf("resources 的 listChanged 应为 false（运行期从不变化）: %+v", caps.Resources)
	}
}

// ---- O-4/O-5/O-6 resources ----

// TestMCPResourcesExposeGuideTopics：resources 逐条列出全部 guide topic（不含 summary，
// 与工具侧口径一致），URI 模板可读，且静态资源真能读出 markdown。
func TestMCPResourcesExposeGuideTopics(t *testing.T) {
	f := newSmokeFixture(t)
	ctx := context.Background()

	res, err := f.session.ListResources(ctx, nil)
	if err != nil {
		t.Fatalf("ListResources: %v", err)
	}
	got := map[string]string{}
	for _, r := range res.Resources {
		got[r.URI] = r.MIMEType
	}
	for _, topic := range guide.Topics() {
		uri := "gear://guide/" + topic
		mt, ok := got[uri]
		if !ok {
			t.Fatalf("resources 应含 %s，实际: %v", uri, got)
		}
		if mt != "text/markdown" {
			t.Fatalf("%s 的 MIMEType 应为 text/markdown，实际 %s", uri, mt)
		}
	}
	// summary 不进 resources（已被 Instructions 常驻注入，再暴露一份只是重复）。
	if _, has := got["gear://guide/"+guide.SummaryTopic]; has {
		t.Fatalf("summary 不应作为资源暴露（与 gear_guide 的 Topics 口径统一，评审 O-6）: %v", got)
	}

	// 静态资源能读出内容。
	rr, err := f.session.ReadResource(ctx, &mcp.ReadResourceParams{URI: "gear://guide/errors"})
	if err != nil {
		t.Fatalf("ReadResource: %v", err)
	}
	if len(rr.Contents) == 0 || len(rr.Contents[0].Text) < 200 {
		t.Fatalf("读 errors 资源应有实质内容: %+v", rr.Contents)
	}

	// URI 模板存在，且经模板读取任意 topic 也可用（评审 O-4）。
	tpl, err := f.session.ListResourceTemplates(ctx, nil)
	if err != nil {
		t.Fatalf("ListResourceTemplates: %v", err)
	}
	var hasTemplate bool
	for _, t2 := range tpl.ResourceTemplates {
		if t2.URITemplate == guideURITemplate {
			hasTemplate = true
		}
	}
	if !hasTemplate {
		t.Fatalf("应声明 URI 模板 %s，实际: %+v", guideURITemplate, tpl.ResourceTemplates)
	}
	tr, err := f.session.ReadResource(ctx, &mcp.ReadResourceParams{URI: "gear://guide/rest-api"})
	if err != nil {
		t.Fatalf("经模板/URI 读 rest-api 失败: %v", err)
	}
	if len(tr.Contents) == 0 || len(tr.Contents[0].Text) < 100 {
		t.Fatalf("rest-api 资源应有内容: %+v", tr.Contents)
	}
}

// ---- 验收：一个「纯 MCP 接入者」的闭环 ----

// TestMCPAgentAcceptanceFlow 按评审 §4 的建议，模拟一个**只读 tools/list + resources + gear_guide**
// 的接入者走完「读环境 → 查能力 → 校验 → 落库 → 执行 → 看日志」的闭环：
// 每一步都只用接口自描述的信息（不读 Go 源码），因此评审列出的每条接口缺口都会在此真实撞到。
func TestMCPAgentAcceptanceFlow(t *testing.T) {
	f := newSmokeFixture(t)

	// ① 自描述：工具面每个成员都必须有 outputSchema（客户端据此静态知道返回形状）。
	tools, err := f.session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	for _, tt := range tools.Tools {
		if tt.OutputSchema == nil {
			t.Fatalf("工具 %s 缺少 outputSchema，接入者无法静态知道返回形状", tt.Name)
		}
	}

	// ② 环境自检：接入者第一件事是分辨「环境没备好」与「YAML 写错」。
	var doc map[string]any
	callTool(t, f.session, "gear_doctor", nil, &doc)
	if _, ok := doc["registry"]; !ok {
		t.Fatalf("doctor 应报告 registry 状态: %v", doc)
	}

	// ③ 能力目录（带过滤）：写 step.call 前必须先看。
	var caps map[string]any
	callTool(t, f.session, "gear_capabilities", map[string]any{"kind": "builtin"}, &caps)
	if n, _ := caps["total"].(float64); n <= 0 {
		t.Fatalf("能力目录应含内核原语: %v", caps)
	}

	// ④ 手册：安全主题必须可达（破坏性清单与密钥规则在那里）。
	var sec map[string]any
	callTool(t, f.session, "gear_guide", map[string]any{"topic": "security"}, &sec)
	if c, _ := sec["content"].(string); len(c) < 200 {
		t.Fatalf("security 手册应有内容: %v", sec)
	}

	// ⑤ 试一个原子能力（不必写 Flow）。
	var inv map[string]any
	callTool(t, f.session, "gear_invoke", map[string]any{
		"name": "crypto", "method": "base64_encode", "args": map[string]any{"data": "gear"},
	}, &inv)
	if s, _ := inv["result"].(string); s == "" {
		t.Fatalf("gear_invoke 应直接返回结果: %v", inv)
	}

	// ⑥ 校验 → 落库（唯一写入口）。
	var val map[string]any
	callTool(t, f.session, "gear_validate", map[string]any{"yaml": smokeBundleYAML}, &val)
	if val["valid"] != true {
		t.Fatalf("冒烟 bundle 应通过校验: %v", val)
	}
	var applied map[string]any
	callTool(t, f.session, "gear_apply", map[string]any{"bundle": smokeBundleYAML}, &applied)
	if applied["valid"] != true {
		t.Fatalf("apply 应成功: %v", applied)
	}

	// ⑦ 跑一次一次性编排 → 从执行记录拿到成功状态与日志。
	var run map[string]any
	callTool(t, f.session, "gear_run", map[string]any{
		"kind": "flow", "id": "smoke.up", "wait_seconds": 15,
	}, &run)
	execID, _ := run["execution_id"].(string)
	if execID == "" {
		t.Fatalf("gear_run(flow) 应返回 execution_id: %v", run)
	}
	var ex map[string]any
	callTool(t, f.session, "gear_execution", map[string]any{"id": execID, "with_logs": true}, &ex)
	if ex["status"] != "succeeded" {
		t.Fatalf("执行应成功，实际: %v", ex)
	}
	if _, ok := ex["logs"]; !ok {
		t.Fatalf("with_logs 应返回日志: %v", ex)
	}

	// ⑧ 元信息：协议版本被钉死、不声明 logging（接入者可据此预期行为）。
	init := f.session.InitializeResult()
	if init == nil || init.ProtocolVersion != supportedProtocolVersion {
		t.Fatalf("initialize 应钉死协议版本 %s: %+v", supportedProtocolVersion, init)
	}
	if wire := capsWire(t, init.Capabilities); strings.Contains(wire, `"logging"`) {
		t.Fatalf("不得声明 logging: %s", wire)
	}
}
