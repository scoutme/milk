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
