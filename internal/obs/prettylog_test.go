package obs

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/scoutme/milk/internal/config"
)

func TestPrettyHandler_ColoredStructuredLine(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(newLogHandler(LogFormatPretty, &buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	logger.Info("agent response", "agent", "deepseek", "output", 514)
	logger.Debug("hidden below the level")

	out := buf.String()
	if !strings.Contains(out, "\x1b[") {
		t.Errorf("pretty output must be colored even into a buffer: %q", out)
	}
	for _, want := range []string{"agent response", "agent", "deepseek", "output", "514"} {
		if !strings.Contains(out, want) {
			t.Errorf("pretty line misses %q: %q", want, out)
		}
	}
	if strings.Contains(out, "hidden below") {
		t.Error("the configured level must filter debug lines")
	}
}

func TestInitMilkLogger_AcceptsPrettyFormat(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	dir := t.TempDir()
	shutdown, err := initMilkLogger(config.OtelConfig{LogFormat: LogFormatPretty, LogLevel: "INFO"}, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer shutdown()
	if milkLogger == nil {
		t.Fatal("pretty is a valid format and must install a logger")
	}
}

func TestLogResponse_OmitsUnknownTimingAndCost(t *testing.T) {
	var buf bytes.Buffer
	old := milkLogger
	milkLogger = slog.New(slog.NewTextHandler(&buf, nil))
	t.Cleanup(func() { milkLogger = old })

	LogResponse(ResponseLog{Agent: "a", Model: "m", Role: "primary", Input: 10, Output: 20})
	line := buf.String()
	for _, absent := range []string{"ttft_ms", "tok_s", "cost_usd", "cache_read"} {
		if strings.Contains(line, absent) {
			t.Errorf("%s must be left out when unknown: %s", absent, line)
		}
	}

	buf.Reset()
	LogResponse(ResponseLog{Agent: "a", Model: "m", Role: "primary", Input: 10, Output: 100,
		TTFT: 1200 * time.Millisecond, Decode: 2 * time.Second, CostUSD: 0.01})
	line = buf.String()
	for _, want := range []string{"ttft_ms=1200", "tok_s=50", "cost_usd=0.01"} {
		if !strings.Contains(line, want) {
			t.Errorf("missing %s: %s", want, line)
		}
	}
}
