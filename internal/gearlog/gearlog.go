// Package gearlog 是 gear CLI 的**进程级诊断日志**设施。
//
// 存在的理由：gear 此前没有任何诊断日志——命令失败了只能看到 cobra 那一行错误文案，
// 「它读的是哪个 registry、跑了多久、内部警告过什么」全部无处可查。排障靠重跑加 printf，
// 而重跑不一定是同一条路径（cwd、registry、版本都可能变），于是「在我机器上不复现」。
//
// 与**执行进度**（Engine.progressf，`✓ step …`）是正交的两层，不要合并：
//
//   - 进度是面向**人盯着看**的输出，目标是 stdout（传输层可改，如 MCP 的 stderr）；
//   - 诊断日志是面向**事后归档**的档案，目标是文件，带时间戳与结构化字段。
//
// 合并的坏处：把 `--log-file` 接去抢 stdout，会让「结果可管道消费」（gear exec status | jq）
// 与「日志留档」互相踩。所以本包只管档案，stdout 一个字节都不碰。
//
// 三条设计红线：
//
//  1. **不启用 = 零行为变化**：path 为空时 L() 给 discard logger，存量脚本逐字不受影响
//     （诊断日志是选配，不是默认开启的噪音源）。
//  2. **不静默降级**：给了 path 却开不了文件（权限/目录冲突），主命令照常跑，但必须
//     往 stderr 明说「日志没启用」——否则用户会以为有档案，排障时对着一个不存在的
//     文件猜。这正是本项目反复吃亏的「静默降级比报错更坏」。
//  3. **敏感值不回显**：api-key/secret/token 类字段的值一律打码（见 SensitiveKeys）。
//     CLI 会把用户命令行原样记进档案，不打码等于把密钥写进一个"随便就会贴出去"的文件。
package gearlog

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// SensitiveKeys 是日志字段名的敏感名单（精确匹配，大小写不敏感）。
// 命中时值替换为 RedactedMark——**名单之外的键照原样记录**（过度打码会让档案失去排障价值）。
var SensitiveKeys = map[string]bool{
	"api_key": true, "apikey": true, "api-key": true,
	"secret": true, "token": true, "password": true, "passwd": true,
	"key": true, "authorization": true, "auth": true,
}

// RedactedMark 是打码后的替身。用可识别的标记而不是空串——「这里有值但我不能看」
// 与「这里没值」是两件事，混为一谈会让排障多绕一圈。
const RedactedMark = "<redacted>"

var (
	mu     sync.Mutex
	logger *slog.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	closer io.Closer
)

// Setup 初始化进程级诊断日志，返回需 close 的对象（文件句柄；未启用时为 nil）。
//
// path 为空 = 不启用（返回 nil, nil，L() 保持 discard）——**默认不写文件**。
// level 取值 debug/info/warn/error，空串按 info；非法值返回错误（调用方应直接失败：
// 阈值是用户明确表达的意图，静默兜底成另一档等于篡改意图）。
func Setup(path, level string) (io.Closer, error) {
	lvl, err := parseLevel(level)
	if err != nil {
		return nil, err
	}
	if path == "" {
		// 显式关停也要释放上一次的句柄：CLI 测试与长驻进程会复用进程。
		mu.Lock()
		defer mu.Unlock()
		if closer != nil {
			_ = closer.Close()
			closer = nil
		}
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
		return nil, nil
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("创建日志目录 %s 失败: %w", dir, err)
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("打开日志文件 %s 失败: %w", path, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if closer != nil {
		_ = closer.Close()
	}
	closer = f
	// TextHandler：key=value 单行、带时间戳。不引第三方依赖（本项目零新增依赖的延续）。
	logger = slog.New(slog.NewTextHandler(f, &slog.HandlerOptions{Level: lvl}))
	return f, nil
}

// L 返回进程级 logger。未 Setup（或 Setup 传空 path）时是 discard——
// 调用方无需判 nil，L().Info(...) 永远安全。
func L() *slog.Logger {
	mu.Lock()
	defer mu.Unlock()
	return logger
}

// Attr 构造一个**自动脱敏**的字段：键命中 SensitiveKeys 时值替换为 RedactedMark。
// 记录用户命令行/配置片段时一律走它，不要手 slog.String("api_key", v)。
func Attr(key, value string) slog.Attr {
	return slog.String(key, RedactValue(key, value))
}

// RedactValue 按键名脱敏值（键不命中则原样返回）。供 CLI 渲染 `--flag=value` 片段。
func RedactValue(key, value string) string {
	if SensitiveKeys[strings.ToLower(key)] {
		return RedactedMark
	}
	return value
}

// ParseLevel 导出级解析供调用方**先行校验**：CLI 要在开文件之前区分
// 「level 写错了（用户输入错误，直接失败）」与「文件开不了（降级继续 + stderr 明说）」
// ——两类失败的处置相反，混在一个 error 里就只能选一种，必错一边。
func ParseLevel(level string) (slog.Level, error) { return parseLevel(level) }

// parseLevel 把用户输入翻译成 slog 级别。空串 = info（默认档，不报错——
// 「没表达」与「表达错了」是两件事，前者走默认、后者必须死）。
func parseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, fmt.Errorf("非法的 --log-level %q（应为 debug|info|warn|error）", s)
	}
}

// ResetForTest 把进程级状态复位（测试隔离用；生产路径不要调）。
func ResetForTest() {
	mu.Lock()
	defer mu.Unlock()
	if closer != nil {
		_ = closer.Close()
		closer = nil
	}
	logger = slog.New(slog.NewTextHandler(io.Discard, nil))
}
