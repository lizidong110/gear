package ext

import (
	"bytes"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"net"
	"net/smtp"
	"os"
	"strings"
)

func init() { register("email", "send", emailSend) }

func resolveRecipients(v interface{}) []string {
	switch t := v.(type) {
	case nil:
		return nil
	case string:
		return []string{t}
	case []interface{}:
		out := make([]string, 0, len(t))
		for _, x := range t {
			out = append(out, fmt.Sprint(x))
		}
		return out
	default:
		return []string{fmt.Sprint(v)}
	}
}

func emailSend(args map[string]interface{}) (interface{}, error) {
	to := resolveRecipients(args["to"])
	if len(to) == 0 {
		return nil, fmt.Errorf("to 必填")
	}
	subject := strOr(args, "subject", "")
	body := strOr(args, "body", "")
	cc := resolveRecipients(args["cc"])
	html := boolOr(args, "html", false)
	from := strOr(args, "from", "")

	mode := strOr(args, "mode", "send")

	// dev 后端：写文件而非真实发送（协议与代码路径一致，仅 sink 不同）。
	if mode == "log" {
		logPath := strOr(args, "log_path", "email.out.log")
		return emailLog(from, to, cc, subject, body, logPath), nil
	}

	host := strOr(args, "host", "")
	if host == "" {
		return nil, fmt.Errorf("host 必填（SMTP 主机）")
	}
	port := intOr(args, "port", 465)
	user := strOr(args, "user", "")
	if user == "" {
		return nil, fmt.Errorf("user 必填（SMTP 账号）")
	}
	pass := strOr(args, "pass", "")
	if pass == "" {
		return nil, fmt.Errorf("pass 必填（SMTP 密码）")
	}
	if from == "" {
		from = user
	}
	if from == "" {
		from = "noreply@localhost"
	}
	useSSL := boolOr(args, "ssl", true)

	msg, err := buildMail(from, to, cc, subject, body, html)
	if err != nil {
		return nil, err
	}
	if err := sendSMTP(host, port, useSSL, user, pass, from, append(to, cc...), msg); err != nil {
		return nil, err
	}
	return map[string]interface{}{"delivered": true, "to": to}, nil
}

func emailLog(from string, to, cc []string, subject, body, logPath string) map[string]interface{} {
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return map[string]interface{}{"delivered": false, "mode": "log", "error": err.Error()}
	}
	defer f.Close()
	fmt.Fprintf(f, "==== email ====\nFrom: %s\nTo: %s\n", from, strings.Join(to, ", "))
	if len(cc) > 0 {
		fmt.Fprintf(f, "Cc: %s\n", strings.Join(cc, ", "))
	}
	fmt.Fprintf(f, "Subject: %s\nBody:\n%s\n\n", subject, body)
	return map[string]interface{}{"delivered": true, "mode": "log", "to": to}
}

func writeStrings(buf *bytes.Buffer, args ...string) {
	for _, arg := range args {
		buf.WriteString(arg)
	}
}

func buildMail(from string, to, cc []string, subject, body string, html bool) ([]byte, error) {
	var buf bytes.Buffer
	defer func() {
		if r := recover(); r == bytes.ErrTooLarge {
			print("buildMail: bytes.ErrTooLarge")
		}
	}()
	writeStrings(&buf, "To: ", strings.Join(to, ", "), "\r\n")
	if len(cc) > 0 {
		writeStrings(&buf, "Cc: ", strings.Join(cc, ", "), "\r\n")
	}
	writeStrings(&buf, "Subject: "+subject+"\r\n")
	writeStrings(&buf, "MIME-Version: 1.0\r\n")
	if html {
		boundary := (func() string {
			b := make([]byte, 16)
			rand.Read(b)
			return hex.EncodeToString(b)
		})()
		writeStrings(&buf, "Content-Type: multipart/alternative; boundary=", boundary, "\r\n\r\n")
		writeStrings(&buf, "--", boundary, "\r\n")
		writeStrings(&buf, "Content-Type: text/plain; charset=UTF-8\r\n\r\n")
		writeStrings(&buf, "This message requires HTML support.\r\n\r\n")
		writeStrings(&buf, "--", boundary, "\r\n")
		writeStrings(&buf, "Content-Type: text/html; charset=UTF-8\r\n\r\n")
		writeStrings(&buf, body, "\r\n\r\n")
		writeStrings(&buf, "--", boundary, "--\r\n")
	} else {
		writeStrings(&buf, "Content-Type: text/plain; charset=UTF-8\r\n\r\n")
		writeStrings(&buf, body, "\r\n")
	}
	return buf.Bytes(), nil
}

// 建立 SMTP 连接的三个动作抽成包级变量：默认实现即下面的标准库调用，
// 生产行为与抽之前**逐字节相同**；换掉它们只为让失败路径可被测试覆盖——
// 尤其 useSSL 分支里 `smtp.NewClient` 返回 error 的情况（真实 SMTP 服务端
// 很难构造：要么 TLS 证书不受信任走 dial 失败分支，要么 handshake 正常）。
//
// 测试替换后必须在 t.Cleanup 里还原。注意：这些是**包级可变**的，
// 故 email 的包级测试不得用 t.Parallel()。
var (
	dialTLSConn = func(addr, host string) (net.Conn, error) {
		return tls.Dial("tcp", addr, &tls.Config{ServerName: host})
	}
	newSMTPClient = func(conn net.Conn, host string) (*smtp.Client, error) { return smtp.NewClient(conn, host) }
	dialSMTP      = func(addr string) (*smtp.Client, error) { return smtp.Dial(addr) }
)

func sendSMTP(host string, port int, useSSL bool, user, pass, from string, rcpts []string, msg []byte) error {
	addr := fmt.Sprintf("%s:%d", host, port)
	var c *smtp.Client
	var err error
	if useSSL {
		// dial 的 err 必须另起名字（derr），不能写成 `conn, err := tls.Dial(...)`：
		// `:=` 会在本 if 块内新建遮蔽外层的 err，于是下一行 `c, err = ...` 写进的是
		// 出块即失效的内层 err，外层 err 恒为 nil → 下面 `if err != nil` 检查不到
		// NewClient 的失败，c 为 nil 仍继续往下走，`defer c.Quit()` 直接 nil 指针 panic。
		// 这是 465/SSL 路径独有的雷（非 SSL 分支没有内层遮蔽），故改名 + 留注 + 配回归测试。
		conn, derr := dialTLSConn(addr, host)
		if derr != nil {
			return fmt.Errorf("SMTP SSL 连接失败: %w", derr)
		}
		c, err = newSMTPClient(conn, host)
	} else {
		c, err = dialSMTP(addr)
	}
	if err != nil {
		return fmt.Errorf("SMTP 连接失败: %w", err)
	}
	defer c.Quit()
	if ok, _ := c.Extension("STARTTLS"); ok && !useSSL {
		if err := c.StartTLS(&tls.Config{ServerName: host}); err != nil {
			return fmt.Errorf("STARTTLS 失败: %w", err)
		}
	}
	if user != "" {
		if err := c.Auth(smtp.PlainAuth("", user, pass, host)); err != nil {
			return fmt.Errorf("SMTP 认证失败: %w", err)
		}
	}
	if err := c.Mail(from); err != nil {
		return fmt.Errorf("MAIL FROM 失败: %w", err)
	}
	for _, r := range rcpts {
		if err := c.Rcpt(r); err != nil {
			return fmt.Errorf("RCPT %s 失败: %w", r, err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(msg); err != nil {
		return err
	}
	return w.Close()
}
