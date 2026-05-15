package memory

// This file contains all OTel instrumentation for the memory package.
// Store methods call these helpers so the main logic stays readable.

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/log/global"

	"github.com/scoutme/milk/internal/obs"
)

const (
	scope      = "github.com/scoutme/milk/memory"
	meterScope = scope
)

// logPercept emits a DEBUG log for a Percept write event.
func logPercept(ctx context.Context, p Percept, sessionID string) {
	logger := global.Logger(scope)
	var r log.Record
	r.SetSeverity(log.SeverityDebug)
	r.SetBody(log.StringValue("percept_recorded"))
	r.SetTimestamp(time.Now())
	r.AddAttributes(
		log.String("percept_id", p.ID[:8]),
		log.String("producer", string(p.Producer)),
		log.Float64("w", p.W),
		log.Bool("core", p.Core),
		log.String("session_id", sessionID),
	)
	logger.Emit(ctx, r)
}

// logRecall emits a DEBUG log for a get_memory query.
func logRecall(ctx context.Context, query string, resultsN int, sessionID string) {
	logger := global.Logger(scope)
	var r log.Record
	r.SetSeverity(log.SeverityDebug)
	r.SetBody(log.StringValue("percept_recalled"))
	r.SetTimestamp(time.Now())
	r.AddAttributes(
		log.String("query", query),
		log.Int("results_n", resultsN),
		log.String("session_id", sessionID),
	)
	logger.Emit(ctx, r)
}

// logConsolidation emits an INFO log for a consolidation run.
func logConsolidation(ctx context.Context, sessionID string, before, decayed, pruned, promoted, globalTotal int) {
	logger := global.Logger(scope)
	var r log.Record
	r.SetSeverity(log.SeverityInfo)
	r.SetBody(log.StringValue("consolidation_run"))
	r.SetTimestamp(time.Now())
	r.AddAttributes(
		log.String("session_id", sessionID),
		log.Int("percepts_before", before),
		log.Int("decayed", decayed),
		log.Int("pruned", pruned),
		log.Int("promoted", promoted),
		log.Int("global_total", globalTotal),
	)
	logger.Emit(ctx, r)
}

// metricsRecord increments the percepts_recorded counter.
func metricsRecord(ctx context.Context, producer Producer, scope_ string) {
	obs.Inc(ctx, meterScope, "milk.memory.percepts_recorded",
		attribute.String("producer", string(producer)),
		attribute.String("scope", scope_),
	)
}

// metricsRecall increments the percepts_recalled counter.
func metricsRecall(ctx context.Context) {
	obs.Inc(ctx, meterScope, "milk.memory.percepts_recalled")
}

// metricsConsolidation records consolidation counters and the global size gauge.
func metricsConsolidation(ctx context.Context, decayed, pruned, promoted, globalTotal int) {
	obs.Add(ctx, meterScope, "milk.memory.consolidation.decayed", int64(decayed))
	obs.Add(ctx, meterScope, "milk.memory.consolidation.pruned", int64(pruned))
	obs.Add(ctx, meterScope, "milk.memory.consolidation.promoted", int64(promoted))
	obs.SetGauge(ctx, meterScope, "milk.memory.global_size", int64(globalTotal))
}

// traceRecord returns a span for a record_memory operation.
func traceRecord(ctx context.Context, producer Producer) (context.Context, func(error)) {
	ctx, span := obs.StartSpan(ctx, "milk.memory.record",
		attribute.String("producer", string(producer)),
	)
	return ctx, func(err error) { obs.EndSpan(span, err) }
}

// traceRecall returns a span for a get_memory operation.
func traceRecall(ctx context.Context, query string) (context.Context, func(int, error)) {
	ctx, span := obs.StartSpan(ctx, "milk.memory.recall",
		attribute.String("query", query),
	)
	return ctx, func(resultsN int, err error) {
		span.SetAttributes(attribute.Int("results_n", resultsN))
		obs.EndSpan(span, err)
	}
}

// traceConsolidation returns a span for the NREM consolidation run.
func traceConsolidation(ctx context.Context) (context.Context, func(decayed, pruned, promoted int, err error)) {
	ctx, span := obs.StartSpan(ctx, "milk.memory.consolidation")
	return ctx, func(decayed, pruned, promoted int, err error) {
		span.SetAttributes(
			attribute.Int("decayed", decayed),
			attribute.Int("pruned", pruned),
			attribute.Int("promoted", promoted),
		)
		obs.EndSpan(span, err)
	}
}

// elapsedMs returns elapsed milliseconds since start.
func elapsedMs(start time.Time) float64 {
	return float64(time.Since(start).Milliseconds())
}
