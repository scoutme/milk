package obs

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	otelmetric "go.opentelemetry.io/otel/metric"
)

// withUnit returns an OTel metric option for unit annotation.
func withUnit(u string) otelmetric.InstrumentOption {
	return otelmetric.WithUnit(u)
}

// withAttrs wraps attribute.KeyValue slice into a MeasurementOption.
func withAttrs(attrs ...attribute.KeyValue) otelmetric.MeasurementOption {
	return otelmetric.WithAttributes(attrs...)
}

// withInt64Callback returns an Int64ObservableOption that reports value on each collection.
func withInt64Callback(ctx context.Context, value int64, attrs ...attribute.KeyValue) otelmetric.Int64ObservableOption {
	return otelmetric.WithInt64Callback(func(_ context.Context, o otelmetric.Int64Observer) error {
		o.Observe(value, otelmetric.WithAttributes(attrs...))
		return nil
	})
}

// contextKey is unexported to avoid collisions.
type contextKey struct{ name string }

// sessionIDKey is stored on span contexts for correlation.
var sessionIDKey = &contextKey{"session_id"}

// ContextWithSessionID returns a context carrying the session ID.
func ContextWithSessionID(ctx context.Context, sessionID string) context.Context {
	return context.WithValue(ctx, sessionIDKey, sessionID)
}

// SessionIDFromContext extracts the session ID, returning "" if absent.
func SessionIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(sessionIDKey).(string)
	return v
}
