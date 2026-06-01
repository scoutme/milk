// Package obs is the single OTel boundary for milk. All provider bootstrapping,
// exporter wiring, and signal accessors live here. Other packages call standard
// OTel APIs via the accessors in this package — they have no knowledge of how
// signals are exported.
package obs

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/scoutme/milk/internal/config"
)

const instrumentationScope = "github.com/scoutme/milk"

// CheckFileSizes checks all otel signal files against warn_mb and max_mb
// thresholds. Returns a warning message if any file exceeds warn_mb, and
// whether any file exceeded max_mb (in which case OTel should be disabled).
func CheckFileSizes(cfg config.OtelConfig, otelDir string) (warning string, exceeded bool) {
	if !cfg.Enabled {
		return "", false
	}
	stats := FileStats(otelDir)
	var totalBytes int64
	for _, s := range stats {
		totalBytes += s.Bytes
		mb := int(s.Bytes / 1024 / 1024)
		if cfg.MaxMB > 0 && mb >= cfg.MaxMB {
			return fmt.Sprintf("otel file %s is %d MB (max_mb=%d) — OTel disabled for this session; run /otel trim to reset",
				s.Name, mb, cfg.MaxMB), true
		}
		if cfg.WarnMB > 0 && mb >= cfg.WarnMB {
			warning = fmt.Sprintf("~/.milk/otel/%s is %d MB — run /otel trim to archive", s.Name, mb)
		}
	}
	_ = totalBytes
	return warning, false
}

// StartSpan is a convenience wrapper for starting a named span.
func StartSpan(ctx context.Context, spanName string, attrs ...attribute.KeyValue) (context.Context, oteltrace.Span) {
	return Tracer(instrumentationScope).Start(ctx, spanName,
		oteltrace.WithAttributes(attrs...),
	)
}

// EndSpan ends a span, recording an error if non-nil.
func EndSpan(span oteltrace.Span, err error) {
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	span.End()
}

// RecordDuration records a duration histogram value with the given meter, instrument name, and labels.
func RecordDuration(ctx context.Context, meterName, instrument string, elapsed time.Duration, attrs ...attribute.KeyValue) {
	m := Meter(meterName)
	h, err := m.Float64Histogram(instrument, withUnit("ms"))
	if err != nil {
		slog.Default().Warn("obs: histogram init failed", "instrument", instrument, "err", err)
		return
	}
	h.Record(ctx, float64(elapsed.Milliseconds()), withAttrs(attrs...))
}

// Inc increments a named counter by 1.
func Inc(ctx context.Context, meterName, instrument string, attrs ...attribute.KeyValue) {
	Add(ctx, meterName, instrument, 1, attrs...)
}

// Add increments a named counter by n. No-op when n <= 0.
func Add(ctx context.Context, meterName, instrument string, n int64, attrs ...attribute.KeyValue) {
	if n <= 0 {
		return
	}
	m := Meter(meterName)
	c, err := m.Int64Counter(instrument)
	if err != nil {
		slog.Default().Warn("obs: counter init failed", "instrument", instrument, "err", err)
		return
	}
	c.Add(ctx, n, withAttrs(attrs...))
}

// RecordTokens emits prompt, completion, and total token counters with model
// and agent-role labels, and updates the in-memory session accumulator.
// model and agentRole must be non-empty.
// agentRole should be "primary", "escalation", or "router".
func RecordTokens(ctx context.Context, model, agentRole string, prompt, completion int64) {
	if model == "" || agentRole == "" || (prompt == 0 && completion == 0) {
		return
	}
	attrs := []attribute.KeyValue{
		attribute.String("model", model),
		attribute.String("agent", agentRole),
	}
	Add(ctx, instrumentationScope, "milk.tokens.prompt", prompt, attrs...)
	Add(ctx, instrumentationScope, "milk.tokens.completion", completion, attrs...)
	Add(ctx, instrumentationScope, "milk.tokens.total", prompt+completion, attrs...)
	accumulateSessionTokens(model, agentRole, prompt, completion)
	Debug("tokens", "model", model, "agent", agentRole, "prompt", prompt, "completion", completion)
}

// SetGauge sets an observable gauge via a callback. Registers a new observable
// gauge each call — intended for low-frequency gauges (e.g. session end).
func SetGauge(ctx context.Context, meterName, instrument string, value int64, attrs ...attribute.KeyValue) {
	m := Meter(meterName)
	_, err := m.Int64ObservableGauge(instrument,
		withInt64Callback(ctx, value, attrs...),
	)
	if err != nil {
		slog.Default().Warn("obs: gauge init failed", "instrument", instrument, "err", err)
	}
}
