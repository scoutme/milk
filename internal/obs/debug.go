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
	format := cfg.LogFormat
	if format == "" {
		format = "text"
	}
	switch format {
	case "text", "json", LogFormatPretty:
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
	maxBytes := cfg.DebugLogMaxBytes
	if maxBytes <= 0 {
		maxBytes = 104857600
	}
	maxFiles := cfg.DebugLogMaxFiles
	if maxFiles <= 0 {
		maxFiles = 5
	}
	rw, err := NewRotatingWriter(path, maxBytes, maxFiles)
	if err != nil {
		return nil, err
	}

	opts := &slog.HandlerOptions{Level: parseLogLevel(cfg.LogLevel)}
	// The gate wrapper makes /otel off stop milk.log too — the log lives in
	// the otel directory and is archived by /otel trim, so it belongs to the
	// same emission gate as the signal files (#458).
	milkLogger = slog.New(newLogHandler(format, &gatedWriter{w: rw}, opts))
	return func() { rw.Close() }, nil //nolint:errcheck
}

// newLogHandler picks the handler for a validated format name and stamps
// every record it builds with the writing process's pid. milk.log is shared
// by all milk sessions on a config root; with no session tag available on the
// plain-slog paths (most call sites have no context), pid is the unambiguous
// "who wrote this line" (#455 — per-line session tagging for the remaining
// WARN sites is tracked in
// docs/internal/plans/escalation-and-context-enhancements-plan.md).
func newLogHandler(format string, w io.Writer, opts *slog.HandlerOptions) slog.Handler {
	var h slog.Handler
	switch format {
	case "json":
		h = slog.NewJSONHandler(w, opts)
	case LogFormatPretty:
		h = newPrettyHandler(w, opts.Level.Level())
	default:
		h = slog.NewTextHandler(w, opts)
	}
	return &pidAttrHandler{Handler: h}
}

// pidAttrHandler injects pid= into every record. Handle receives the record
// by value, so adding attributes mutates only the copy being emitted.
type pidAttrHandler struct{ slog.Handler }

func (h *pidAttrHandler) Handle(ctx context.Context, r slog.Record) error {
	r.AddAttrs(slog.Int("pid", os.Getpid()))
	return h.Handler.Handle(ctx, r)
}

func (h *pidAttrHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &pidAttrHandler{Handler: h.Handler.WithAttrs(attrs)}
}

func (h *pidAttrHandler) WithGroup(name string) slog.Handler {
	return &pidAttrHandler{Handler: h.Handler.WithGroup(name)}
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

// LogPayload emits the full serialised request payload at DEBUG level,
// with optional extra key/value attributes (e.g. the originating job ID).
// No-op when milkLogger is nil (logging disabled) or payload is empty.
func LogPayload(endpoint string, payload []byte, attrs ...any) {
	if milkLogger == nil || len(payload) == 0 {
		return
	}
	milkLogger.Debug("request payload", append([]any{"endpoint", endpoint, "payload", string(payload)}, attrs...)...)
}

// Info emits an info-level message to the milk log (no-op when disabled).
func Info(msg string, args ...any) {
	if milkLogger != nil {
		milkLogger.Info(msg, args...)
	}
}

// Warn emits a warn-level message to the milk log (no-op when disabled).
func Warn(msg string, args ...any) {
	if milkLogger != nil {
		milkLogger.Warn(msg, args...)
	}
}

// Logger returns the package-level milk-log logger, for packages that can't
// import obs directly (e.g. internal/loop, which internal/config already
// imports, so obs -> loop would cycle) but still want their diagnostics in
// milk's own log file instead of falling back to slog.Default()'s stderr.
// Returns a discard logger when logging is disabled/not yet initialized.
func Logger() *slog.Logger {
	if milkLogger == nil {
		return slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return milkLogger
}

// SetLogger replaces the package-level milk-log logger (nil restores the
// not-initialized no-op guard). Mirrors internal/loop.SetLogger; exists so
// tests can capture emitted lines (e.g. the session_id attribution on WARN
// lines, issue #455) instead of asserting against a real ~/.milk log file.
func SetLogger(l *slog.Logger) {
	milkLogger = l
}

// DebugCtx emits a debug-level message with context to the milk log.
func DebugCtx(ctx context.Context, msg string, args ...any) {
	if milkLogger != nil {
		milkLogger.DebugContext(ctx, msg, args...)
	}
}
