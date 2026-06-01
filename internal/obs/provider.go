package obs

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"go.opentelemetry.io/otel/exporters/stdout/stdoutlog"
	"go.opentelemetry.io/otel/exporters/stdout/stdoutmetric"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/log/global"
	otelmetric "go.opentelemetry.io/otel/metric"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	oteltrace "go.opentelemetry.io/otel/trace"

	otel "go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/sdk/resource"

	"github.com/scoutme/milk/internal/config"
)

// providers holds the active SDK providers so Shutdown can stop them all.
type providers struct {
	tp *sdktrace.TracerProvider
	mp *sdkmetric.MeterProvider
	lp *sdklog.LoggerProvider
}

var active providers

// Init bootstraps OTel with file-based exporters. It must be called once at
// startup before any instrumented code runs. Returns a shutdown function that
// flushes and closes all providers.
func Init(cfg config.OtelConfig, otelDir string) (shutdown func(context.Context) error, err error) {
	if !cfg.Enabled {
		return func(context.Context) error { return nil }, nil
	}

	if err := os.MkdirAll(otelDir, 0o700); err != nil {
		return nil, err
	}

	res, err := resource.New(context.Background(),
		resource.WithAttributes(semconv.ServiceName("milk")),
	)
	if err != nil {
		return nil, err
	}

	var shutdowns []func(context.Context) error

	// --- Traces ---
	if cfg.Traces {
		tf, err := openSignalFile(otelDir, "traces.jsonl")
		if err != nil {
			return nil, err
		}
		te, err := stdouttrace.New(stdouttrace.WithWriter(tf), stdouttrace.WithPrettyPrint())
		if err != nil {
			return nil, err
		}
		tp := sdktrace.NewTracerProvider(
			sdktrace.WithBatcher(te),
			sdktrace.WithResource(res),
		)
		otel.SetTracerProvider(tp)
		active.tp = tp
		shutdowns = append(shutdowns, func(ctx context.Context) error {
			err := tp.Shutdown(ctx)
			tf.Close() //nolint:errcheck
			return err
		})
	}

	// --- Metrics ---
	if cfg.Metrics {
		mf, err := openSignalFile(otelDir, "metrics.jsonl")
		if err != nil {
			return nil, err
		}
		me, err := stdoutmetric.New(stdoutmetric.WithWriter(mf))
		if err != nil {
			return nil, err
		}
		mp := sdkmetric.NewMeterProvider(
			sdkmetric.WithReader(sdkmetric.NewPeriodicReader(me,
				sdkmetric.WithInterval(metricFlushInterval(cfg)),
			)),
			sdkmetric.WithResource(res),
		)
		otel.SetMeterProvider(mp)
		active.mp = mp

		// Register self-observability gauges that report otel file sizes at
		// every collection. Captures the otelDir in the closure.
		if err := registerFileSizeGauges(mp, otelDir); err != nil {
			return nil, err
		}

		shutdowns = append(shutdowns, func(ctx context.Context) error {
			err := mp.Shutdown(ctx)
			mf.Close() //nolint:errcheck
			return err
		})
	}

	// --- Logs ---
	{
		lf, err := openSignalFile(otelDir, "logs.jsonl")
		if err != nil {
			return nil, err
		}
		le, err := stdoutlog.New(stdoutlog.WithWriter(lf))
		if err != nil {
			return nil, err
		}
		lp := sdklog.NewLoggerProvider(
			sdklog.WithProcessor(sdklog.NewBatchProcessor(le)),
			sdklog.WithResource(res),
		)
		global.SetLoggerProvider(lp)
		active.lp = lp
		shutdowns = append(shutdowns, func(ctx context.Context) error {
			err := lp.Shutdown(ctx)
			lf.Close() //nolint:errcheck
			return err
		})
	}

	// --- Milk log ---
	stopDebug, err := initMilkLogger(cfg, otelDir)
	if err != nil {
		return nil, err
	}

	return func(ctx context.Context) error {
		stopDebug()
		var last error
		for _, fn := range shutdowns {
			if err := fn(ctx); err != nil {
				last = err
			}
		}
		return last
	}, nil
}

// registerFileSizeGauges registers three observable gauges that report the
// current byte size of each otel signal file at every collection cycle.
func registerFileSizeGauges(mp *sdkmetric.MeterProvider, otelDir string) error {
	m := mp.Meter("github.com/scoutme/milk/obs")
	files := []struct {
		instrument string
		name       string
	}{
		{"milk.otel.logs_bytes", "logs.jsonl"},
		{"milk.otel.traces_bytes", "traces.jsonl"},
		{"milk.otel.metrics_bytes", "metrics.jsonl"},
	}
	for _, f := range files {
		path := filepath.Join(otelDir, f.name)
		instrument := f.instrument
		_, err := m.Int64ObservableGauge(instrument,
			otelmetric.WithInt64Callback(func(_ context.Context, o otelmetric.Int64Observer) error {
				info, err := os.Stat(path)
				if err != nil {
					o.Observe(0)
					return nil
				}
				o.Observe(info.Size())
				return nil
			}),
		)
		if err != nil {
			return err
		}
	}
	return nil
}

// metricFlushInterval converts MetricsFlushMinutes to a duration.
// Falls back to 5 minutes when 0 (session-end export still happens via Shutdown).
func metricFlushInterval(cfg config.OtelConfig) time.Duration {
	if cfg.MetricsFlushMinutes > 0 {
		return time.Duration(cfg.MetricsFlushMinutes) * time.Minute
	}
	// Large interval so the periodic reader is effectively session-end only;
	// Shutdown forces a final export regardless.
	return 24 * time.Hour
}

// openSignalFile opens (or creates) a signal file for append.
func openSignalFile(dir, name string) (*os.File, error) {
	return os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
}

// Tracer returns a named tracer from the global provider.
func Tracer(name string) oteltrace.Tracer {
	return otel.Tracer(name)
}

// Meter returns a named meter from the global provider.
func Meter(name string) otelmetric.Meter {
	return otel.Meter(name)
}
