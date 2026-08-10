// Package logger 提供统一的 slog 文本日志。
package logger

import (
	"log/slog"
	"os"
)

// New 按级别创建 slog 文本日志(输出到 stderr)。
func New(level string) *slog.Logger {
	var lv slog.Level
	switch level {
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lv}))
}
