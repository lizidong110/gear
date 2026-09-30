package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ── 超时与连接池 ──
//
// 内核调侧车用的是 LocalHTTPTransport，其 http.Client 硬超时 10s
// （internal/engine/extensions.go:212）—— 侧车必须在 10s 内把 HTTP 响应吐回去，
// 否则内核就判超时。所以单条命令的 deadline、以及 With 整个 fn 的总时长都钳在 10s。
const (
	redisDialTimeout = 3 * time.Second
	redisOpTimeout   = 10 * time.Second
	maxIdleConns     = 8
)

// RedisError 是 redis 返回的业务错误（如 WRONGTYPE / NOSCRIPT），携带原始文案。
// 它不是网络错误，连接仍可复用。
type RedisError struct{ msg string }

func (e *RedisError) Error() string { return "redis_error: " + e.msg }

// Conn 封装一条到 redis 的 TCP 连接。单连接串行：Exec 期间通过 mu 独占，
// 保证「借一条连接、连续发多条命令」时中间不被别的调用插队 —— 这正是
// 分布式锁 unlock（GET+DEL）、延迟队列 dequeue（ZRANGEBYSCORE+ZREM）能不用 Lua
// 也保持原子性的根本原因（redis 单线程 + 侧车单连接串行 = 等价于原子）。
type Conn struct {
	conn net.Conn
	br   *bufio.Reader
	bw   *bufio.Writer
	mu   sync.Mutex
}

func newConn(addr, password string) (*Conn, error) {
	nc, err := net.DialTimeout("tcp", addr, redisDialTimeout)
	if err != nil {
		return nil, err
	}
	c := &Conn{conn: nc, br: bufio.NewReader(nc), bw: bufio.NewWriter(nc)}
	if password != "" {
		if _, err := c.Exec("AUTH", password); err != nil {
			_ = c.Close()
			return nil, err
		}
	}
	return c, nil
}

// Close 关闭底层连接（连接池淘汰或检测到网络错误时调用）。
func (c *Conn) Close() error { return c.conn.Close() }

// Exec 发一条命令并解码响应。args 只接受 string / []byte / int / int64。
// 返回解码后的 RESP 值：string（简单串/批量串）、int64（整数）、
// []interface{}（数组）、nil（nil bulk / nil array）、*RedisError（业务错误）。
func (c *Conn) Exec(args ...interface{}) (interface{}, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.writeCommand(args); err != nil {
		return nil, err
	}
	if err := c.bw.Flush(); err != nil {
		return nil, err
	}
	return c.readReply()
}

func (c *Conn) writeCommand(args []interface{}) error {
	if _, err := c.bw.WriteString(fmt.Sprintf("*%d\r\n", len(args))); err != nil {
		return err
	}
	for _, a := range args {
		var b []byte
		switch v := a.(type) {
		case string:
			b = []byte(v)
		case []byte:
			b = v
		case int:
			b = []byte(strconv.Itoa(v))
		case int64:
			b = []byte(strconv.FormatInt(v, 10))
		default:
			return fmt.Errorf("redis: 不支持的参数类型 %T", a)
		}
		if _, err := c.bw.WriteString(fmt.Sprintf("$%d\r\n", len(b))); err != nil {
			return err
		}
		if _, err := c.bw.Write(b); err != nil {
			return err
		}
		if _, err := c.bw.WriteString("\r\n"); err != nil {
			return err
		}
	}
	return nil
}

// readReply 解码一个 RESP 响应值。
func (c *Conn) readReply() (interface{}, error) {
	line, err := c.readLine()
	if err != nil {
		return nil, err
	}
	if line == "" {
		return nil, errors.New("redis: 空响应（连接可能已断）")
	}
	switch line[0] {
	case '+':
		return line[1:], nil
	case '-':
		return nil, &RedisError{msg: line[1:]}
	case ':':
		n, err := strconv.ParseInt(line[1:], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("redis: 无法解析整数响应 %q: %w", line[1:], err)
		}
		return n, nil
	case '$':
		n, err := strconv.ParseInt(line[1:], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("redis: 无法解析 bulk 长度 %q: %w", line[1:], err)
		}
		if n < 0 {
			return nil, nil // nil bulk（如 GET 一个不存在的 key）
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(c.br, buf); err != nil {
			return nil, err
		}
		if _, err := c.br.Discard(2); err != nil { // 吃掉尾随 \r\n
			return nil, err
		}
		return string(buf), nil
	case '*':
		n, err := strconv.ParseInt(line[1:], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("redis: 无法解析数组长度 %q: %w", line[1:], err)
		}
		if n < 0 {
			return nil, nil // nil array
		}
		arr := make([]interface{}, n)
		for i := range arr {
			v, err := c.readReply()
			if err != nil {
				return nil, err
			}
			arr[i] = v
		}
		return arr, nil
	default:
		return nil, fmt.Errorf("redis: 无法识别的响应首字节 %q", line[0])
	}
}

// readLine 读一行（到 \n），返回已剥除行尾 \r\n 的内容。只用于读类型行/长度行，
// bulk 内容永远走 io.ReadFull（二进制安全）。
func (c *Conn) readLine() (string, error) {
	line, err := c.br.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// Pool 是到单台 redis 的连接池。With 借一个连接、执行 fn（可发多条命令）、归还，
// 从而在 fn 内部保证连接独占、命令连续 —— 多命令原子操作无需 Lua。
type Pool struct {
	addr     string
	password string
	mu       sync.Mutex
	idle     []*Conn
	maxIdle  int
}

func NewPool(addr, password string) *Pool {
	return &Pool{addr: addr, password: password, maxIdle: maxIdleConns}
}

// With 借一个连接执行 fn，结束后归还（网络错误则回收该连接）。
func (p *Pool) With(fn func(*Conn) error) error {
	c, err := p.get()
	if err != nil {
		return err
	}
	defer func() { _ = c.conn.SetDeadline(time.Time{}) }() // 归池连接不设 deadline
	c.conn.SetDeadline(time.Now().Add(redisOpTimeout))
	fnErr := fn(c)
	if fnErr != nil && isNetErr(fnErr) {
		_ = c.Close()
		return fnErr
	}
	p.put(c)
	return fnErr
}

func (p *Pool) get() (*Conn, error) {
	p.mu.Lock()
	if n := len(p.idle); n > 0 {
		c := p.idle[n-1]
		p.idle = p.idle[:n-1]
		p.mu.Unlock()
		return c, nil
	}
	p.mu.Unlock()
	return newConn(p.addr, p.password)
}

func (p *Pool) put(c *Conn) {
	p.mu.Lock()
	if len(p.idle) < p.maxIdle {
		p.idle = append(p.idle, c)
		p.mu.Unlock()
		return
	}
	p.mu.Unlock()
	_ = c.Close()
}

// isNetErr 判断是否是网络层错误（连接断/超时），这类错误要回收连接而非归还。
func isNetErr(err error) bool {
	if err == nil {
		return false
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return true
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	return false
}

// ── 响应辅助：handlers 把解码后的 RESP 值转成 JSON 时用 ──

func asString(v interface{}) (string, bool) { s, ok := v.(string); return s, ok }

func asInt(v interface{}) (int64, bool) { n, ok := v.(int64); return n, ok }

func asArray(v interface{}) ([]interface{}, bool) { a, ok := v.([]interface{}); return a, ok }
