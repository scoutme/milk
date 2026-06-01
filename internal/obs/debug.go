package obs

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/scoutme/milk/internal/config"
)

var milkLogger *slog.Logger

// initMilkLogger opens (or creates) the milk.log file and installs a package-level
// slog logger filtered by cfg.LogLevel. Returns a shutdown function.
func initMilkLogger(cfg config.OtelConfig, otelDir string) (shutdown func(), err error) {
	switch cfg.LogFormat {
	case "text", "json":
	default:
		milkLogger = slog.New(slog.NewTextHandler(io.Discard, nil))
		return func() {}, nil
	}

	if err := os.MkdirAll(otelDir, 0o700); err != nil {
		return nil, err
	}
	path, err := config.MilkLogPath()
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}

	opts := &slog.HandlerOptions{Level: parseLogLevel(cfg.LogLevel)}
	var h slog.Handler
	if cfg.LogFormat == "json" {
		h = slog.NewJSONHandler(f, opts)
	} else {
		h = slog.NewTextHandler(f, opts)
	}
	milkLogger = slog.New(h)
	return func() { f.Close() }, nil //nolint:errcheck
}

func parseLogLevel(s string) slog.Level {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "DEBUG":
		return slog.LevelDebug
	case "WARN", "WARNING":
		return slog.LevelWarn
	case "ERROR":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// Debug emits a debug-level message to the milk log (no-op when disabled).
func Debug(msg string, args ...any) {
	if milkLogger != nil {
		milkLogger.Debug(msg, args...)
	}
}

// Info emits an info-level message to the milk log (no-op when disabled).
func Info(msg string, args ...any) {
	if milkLogger != nil {
		milkLogger.Info(msg, args...)
	}
}

// DebugCtx emits a debug-level message with context to the milk log.
func DebugCtx(ctx context.Context, msg string, args ...any) {
	if milkLogger != nil {
		milkLogger.DebugContext(ctx, msg, args...)
	}
}
