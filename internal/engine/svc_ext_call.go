package engine

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"gear/internal/gearhome"
	"gear/internal/sidecar/coord"
)

// serviceHTTPTransport 是 service ext 在**能力调用路径**上的载体：与 LocalHTTPTransport
// 同契约（POST /<method>，body {"args":...}，响应 {"result"|"error"}），但首次 Call 前
// 先经 coord.RegisterOrStart 确保侧车在线——无人监听时本 gear 当 owner 拉起侧车进程，
// 并持有占用 + watchdog 心跳保活；此后复用本地 HTTP 长连接。
//
// 补的缺口（2026-10-20 真机实测）：此前只有 Flow 的 service step
// （startServiceStep → SvcExt[...].RegisterOrStart）走 coord 握手；能力调用路径
// （planner 选 ext.<name>.<method> / Lua ctx.ext.<name>.<method>）直接打 HTTP，
// 侧车没起就是 connection refused——`ai rewoo` 总结 PDF 时 planner 已能正确选
// ext.pdf.read，执行期却连不上 127.0.0.1:8101，用户还得手工先起 sidecar。
// 接上之后 CLI/REST/MCP 三条执行面共享同一协调客户端，「用户跑 CLI 完成任务」
// 不再需要懂得侧车生命周期（同主机多 gear 仍经 coord 共享单个侧车实例，不抢端口）。
type serviceHTTPTransport struct {
	name  string
	entry string // 侧车启动命令行（COORD_ADDR=<listen> 前缀已由 prepareServiceExt 注入）
	cli   *coord.Client
	base  *LocalHTTPTransport
	start func() (coord.Process, error) // 本 gear 当 owner 时的拉起动作（processRuntime.Start）

	once sync.Once
	sess *coord.Session
	err  error
}

// newServiceHTTPTransport 构造能力调用路径的 service ext 载体。
// start 为 nil 时侧车永远拉不起来——调用方（SetupExtensions）必须传真实动作，
// 这里显式拒绝，避免「以为接了自动拉起其实没有」的静默假象。
func newServiceHTTPTransport(name, entry string, cli *coord.Client, base *LocalHTTPTransport, start func() (coord.Process, error)) *serviceHTTPTransport {
	if start == nil {
		panic("serviceHTTPTransport 需要非 nil 的 start（owner 拉起动作）")
	}
	return &serviceHTTPTransport{name: name, entry: entry, cli: cli, base: base, start: start}
}

// ensure 首次调用时经 coord 握手确保侧车在线（幂等：sync.Once）。
//
// 失败结果被缓存不在每次 Call 重试：RegisterOrStart 内部已有退避重试与竞态收敛，
// 走到这里还失败的原因多是持久性的（二进制未构建 / 端口被非 coord 实例占着），
// 反复重拉只会把一次可操作的报错拖成 N 次超时。
func (t *serviceHTTPTransport) ensure() error {
	t.once.Do(func() {
		sess, err := t.cli.RegisterOrStart(t.start)
		if err != nil {
			t.err = fmt.Errorf("ext %q 侧车未就绪（自动拉起失败）：%w", t.name, err)
			return
		}
		t.sess = sess
	})
	return t.err
}

// Call 经本地 HTTP 转发一次方法调用（与 LocalHTTPTransport 同契约），调用前确保侧车在线。
func (t *serviceHTTPTransport) Call(method string, args map[string]interface{}) (interface{}, error) {
	if err := t.ensure(); err != nil {
		return nil, err
	}
	return t.base.Call(method, args)
}

// Close 注销占用并停止本会话的心跳。长驻执行面（gear serve / MCP）应在关停时调用；
// 短命令（CLI 单次执行）不调也行——进程退出即停心跳，侧车按 coord 规则自行退场。
func (t *serviceHTTPTransport) Close() error {
	if t.sess == nil {
		return nil
	}
	return t.sess.Close()
}

// resolveSidecarArgv0 把 service ext 启动命令的 argv[0] 解析成「任意 CWD 下都找得到」的形式。
//
// 背景：extensions.yaml 的 cmd 写相对路径（plugin/pdf-sidecar.exe），历史上靠
// 「用户 cd build/ 再跑 gear」成立——app 路径的工作约定。但 ai rewoo 这类裸 CLI 的
// CWD 是任意目录，相对路径直接解析失败，「自动拉起」变成「起不来」。
// 规则（按序取第一个存在的，都不中则原样返回让运行期报错指向根因）：
//  1. 相对当前 CWD 已存在 → 原样（既有行为一字不变）；
//  2. gear 可执行文件所在目录下存在 → 换成该绝对路径（build/ 下的侧车产物恰好是
//     plugin/<name>.exe，相对 exe 目录即命中；这与 LookExecutable 的「裸名 exe 目录
//     优先」同精神，只是这里补含分隔符的相对路径）；
//  3. 都没有 → 原样返回（二进制没构建，让调用方看到可操作的失败）。
func resolveSidecarArgv0(argv0 string) string {
	if argv0 == "" || !strings.ContainsAny(argv0, `/\`) {
		return argv0 // 裸名交给 cmdline.LookExecutable（exe 目录优先于 PATH）
	}
	if fileExists(argv0) {
		return argv0
	}
	if d := gearhome.ExeDir(); d != "" {
		if cand := filepath.Join(d, argv0); fileExists(cand) {
			return cand
		}
	}
	return argv0
}
