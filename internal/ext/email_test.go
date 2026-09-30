package ext

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"net/smtp"
)

// 注意：本包的连接钩子（dialTLSConn / newSMTPClient / dialSMTP）是**包级可变**的，
// 下面的用例会替换它们——故整个文件不得用 t.Parallel()。

// withConnHooks 临时替换 SMTP 连接钩子，t.Cleanup 自动还原。
func withConnHooks(t *testing.T, dt func(string, string) (net.Conn, error), nc func(net.Conn, string) (*smtp.Client, error), d func(string) (*smtp.Client, error)) {
	t.Helper()
	oldDT, oldNC, oldD := dialTLSConn, newSMTPClient, dialSMTP
	t.Cleanup(func() { dialTLSConn, newSMTPClient, dialSMTP = oldDT, oldNC, oldD })
	if dt != nil {
		dialTLSConn = dt
	}
	if nc != nil {
		newSMTPClient = nc
	}
	if d != nil {
		dialSMTP = d
	}
}

// TestSendSMTPSSLNewClientErrorPropagates —— SSL/465 路径的**变量遮蔽**回归。
//
// 原写法 `conn, err := tls.Dial(...)` 在 if 块内新建了遮蔽外层的 err，导致下一行
// `c, err = smtp.NewClient(...)` 的错误写进出块即失效的内层变量：外层 err 恒为 nil，
// `if err != nil` 检查不到失败，c 为 nil 仍继续往下走，`defer c.Quit()` 直接 panic。
// 本用例锁死「必须返回可操作的 error，绝不 panic」。
func TestSendSMTPSSLNewClientErrorPropagates(t *testing.T) {
	withConnHooks(t,
		func(string, string) (net.Conn, error) { return &net.TCPConn{}, nil },
		func(net.Conn, string) (*smtp.Client, error) { return nil, errors.New("greeting 读取失败") },
		nil,
	)

	err := sendSMTP("smtp.example.com", 465, true, "u", "p", "from@x.com", []string{"to@x.com"}, []byte("msg"))
	if err == nil {
		t.Fatal("SSL 路径 NewClient 失败必须返回 error，不能当成成功继续往下跑（原实现会静默吞掉并 panic）")
	}
	if !strings.Contains(err.Error(), "SMTP 连接失败") {
		t.Fatalf("错误应来自统一的 NewClient 失败分支，实际: %v", err)
	}
	if !strings.Contains(err.Error(), "greeting 读取失败") {
		t.Fatalf("错误必须包裹底层原因以便排查，实际: %v", err)
	}
}

// TestSendSMTPNonSSL dial 失败必须传播：非 SSL 分支没有内层遮蔽，但同样要求透出原因。
func TestSendSMTPNonSSLErrorPropagates(t *testing.T) {
	withConnHooks(t, nil, nil,
		func(string) (*smtp.Client, error) { return nil, errors.New("连接被拒") },
	)
	err := sendSMTP("127.0.0.1", 25, false, "u", "p", "from@x.com", []string{"to@x.com"}, []byte("msg"))
	if err == nil {
		t.Fatal("非 SSL 路径 dial 失败必须返回 error")
	}
	if !strings.Contains(err.Error(), "连接被拒") {
		t.Fatalf("错误必须包裹底层原因，实际: %v", err)
	}
}

// TestSendSMTPSSLDialErrorPropagates：TLS 握手失败要有独立的、可识别的错误文案
// （「SSL 连接失败」vs「连接失败」必须能区分，否则运维分不清是哪个环节断了）。
func TestSendSMTPSSLDialErrorPropagates(t *testing.T) {
	withConnHooks(t,
		func(string, string) (net.Conn, error) { return nil, errors.New("证书不受信任") },
		nil, nil,
	)
	err := sendSMTP("smtp.example.com", 465, true, "u", "p", "from@x.com", []string{"to@x.com"}, []byte("msg"))
	if err == nil {
		t.Fatal("TLS dial 失败必须返回 error")
	}
	if !strings.Contains(err.Error(), "SSL 连接失败") {
		t.Fatalf("应走独立的 SSL dial 错误分支，实际: %v", err)
	}
}

// TestEmailSendRejectsMissingRequiredArgs —— ext 原子的红线：
// 凭据与连接配置一律从 args 传，缺任何一个都 fail-closed 报错，**不得**降级/猜默认值。
func TestEmailSendRejectsMissingRequiredArgs(t *testing.T) {
	cases := []struct {
		name string
		args map[string]interface{}
		want string
	}{
		{"缺 to", map[string]interface{}{"host": "h", "user": "u", "pass": "p"}, "to 必填"},
		{"缺 host", map[string]interface{}{"to": "a@b.c", "user": "u", "pass": "p"}, "host 必填"},
		{"缺 user", map[string]interface{}{"to": "a@b.c", "host": "h", "pass": "p"}, "user 必填"},
		{"缺 pass", map[string]interface{}{"to": "a@b.c", "host": "h", "user": "u"}, "pass 必填"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, err := emailSend(c.args)
			if err == nil {
				t.Fatalf("%s：应报错，实际返回 %#v", c.name, out)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("%s：错误应含 %q，实际 %v", c.name, c.want, err)
			}
		})
	}
}

// TestEmailSendLogMode：mode=log 写文件而不真发邮件（协议路径一致，仅 sink 不同）。
func TestEmailSendLogMode(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "mail.log")
	out, err := emailSend(map[string]interface{}{
		"to": "a@b.c", "host": "h", "user": "u", "pass": "p",
		"subject": "标题", "body": "正文", "mode": "log", "log_path": logPath,
	})
	if err != nil {
		t.Fatalf("log 模式不应报错: %v", err)
	}
	m, ok := out.(map[string]interface{})
	if !ok || m["delivered"] != true {
		t.Fatalf("log 模式应返回 delivered=true，实际 %#v", out)
	}
	b, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("应写出日志文件: %v", err)
	}
	for _, want := range []string{"To: a@b.c", "Subject: 标题", "正文"} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("日志内容应含 %q，实际:\n%s", want, b)
		}
	}
}

// TestResolveRecipients：to/cc 支持标量、列表、其它类型三种输入。
func TestResolveRecipients(t *testing.T) {
	if got := resolveRecipients(nil); len(got) != 0 {
		t.Fatalf("nil 应得空列表，实际 %v", got)
	}
	if got := resolveRecipients("a@b.c"); len(got) != 1 || got[0] != "a@b.c" {
		t.Fatalf("字符串应得单元素列表，实际 %v", got)
	}
	got := resolveRecipients([]interface{}{"a@b.c", "d@e.f"})
	if len(got) != 2 || got[0] != "a@b.c" || got[1] != "d@e.f" {
		t.Fatalf("列表应原序展开，实际 %v", got)
	}
	if got := resolveRecipients(42); len(got) != 1 || got[0] != "42" {
		t.Fatalf("其它类型应 Sprint 成单元素，实际 %v", got)
	}
}

// TestExtAtomsMustNotReadEnv —— 红线守卫：
// 扩展原子的凭据与连接配置必须全部来自调用参数，**禁止**读进程环境变量
// （否则同一原子在不同 Tool 里行为会随宿主环境漂移，且无从追溯）。
// 这条守卫同时防止那个已被删除的 intOrEnv（os.Getenv 读端口）死灰复燃。
// sidecarGlobs 是 service ext 侧车的源码目录（相对本包 internal/ext）。
//
// 2026-09-30 补上：此前这条守卫只扫 `internal/ext`，于是 `plugin/*-sidecar` 里的
// os.Getenv **完全没人管** —— mqtt-sidecar 就在读 MQTT_BROKER 却一直没被拦。
// once 型原子和 service 型侧车只是载体不同，**都是 ext 的实现**，规矩必须一样。
var sidecarGlobs = []string{"../../plugin/*-sidecar"}

// bootstrapEnvWhitelist 是「进程自身怎么启动」这类环境变量的白名单。
//
// 判据：它描述这个进程**该在哪儿跑 / 启动时往哪连**，而不是「去连谁的凭据」——
// 凭据与连接配置一律从调用参数进，没有例外。新增任何一项都必须在这里写清理由，
// 否则就是给规矩开后门。
var bootstrapEnvWhitelist = map[string]map[string]string{
	"mqtt-sidecar": {
		"MQTT_BROKER": "侧车启动时就得连上一个 broker，这是「往哪连」的兜底默认值（公共测试 broker），不是凭据；真正的 broker 由 publish/subscribe 的入参给",
	},
	"pdf-sidecar": {
		"WINDIR": "定位系统字体目录（%WINDIR%\\Fonts）—— 找的是本机装字体的固定位置，不是凭据也不是「去连谁」；字体路径本身可由 font 入参显式指定",
	},
}

func TestExtAtomsMustNotReadEnv(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("读取本包源文件失败: %v", err)
	}
	for _, f := range files {
		assertNoEnvRead(t, f, nil)
	}

	checked := 0
	for _, dir := range sidecarGlobs {
		dirs, err := filepath.Glob(dir)
		if err != nil {
			t.Fatalf("扫描侧车目录失败: %v", err)
		}
		if len(dirs) == 0 {
			t.Fatalf("%s 没匹配到任何目录：路径变了还是侧车被挪走了？守卫不能静默失效", dir)
		}
		for _, d := range dirs {
			src, _ := filepath.Glob(filepath.Join(d, "*.go"))
			for _, f := range src {
				assertNoEnvRead(t, f, bootstrapEnvWhitelist[filepath.Base(d)])
				checked++
			}
		}
	}
	if checked == 0 {
		t.Fatal("一个侧车源文件都没扫到，守卫形同虚设")
	}
}

func assertNoEnvRead(t *testing.T, f string, allow map[string]string) {
	t.Helper()
	if strings.HasSuffix(f, "_test.go") {
		return
	}
	b, err := os.ReadFile(f)
	if err != nil {
		t.Fatalf("读 %s 失败: %v", f, err)
	}
	for i, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		if !strings.Contains(line, "os.Getenv") {
			continue
		}
		name := envNameIn(line)
		// COORD_ADDR 是引擎注入的「我在哪个端口上服务」，全侧车通用（不是连接配置）。
		if name == "COORD_ADDR" || name == "INBOUND_ADDR" || name == "WEBHOOK_ADDR" {
			continue
		}
		if _, ok := allow[name]; ok {
			continue
		}
		t.Fatalf("%s:%d 违反红线：ext 不得读取环境变量（凭据与连接配置一律从调用参数进）\n  %s",
			f, i+1, strings.TrimSpace(line))
	}
}

// envNameIn 取一行里 os.Getenv("X") 的 X；取不到就返回空（空=不放行，fail-closed）。
func envNameIn(line string) string {
	const mark = "os.Getenv("
	i := strings.Index(line, mark)
	if i < 0 {
		return ""
	}
	rest := line[i+len(mark):]
	j := strings.Index(rest, `"`)
	if j < 0 {
		return ""
	}
	rest = rest[j+1:]
	k := strings.Index(rest, `"`)
	if k < 0 {
		return ""
	}
	return rest[:k]
}
