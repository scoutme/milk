package obs

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/log/global"
)

// Event emits a structured lifecycle event at INFO level to both destinations
// that matter for triage: the milk log (milk.log) and the OTel logs signal
// file (logs.jsonl). Use it for point-in-time facts about a resource's
// lifecycle — e.g. "background.spawned" / "background.completed". Failures
// use EventWarn instead.
//
// args are key/value pairs, mirroring Debug/Info/Warn. Safe to call before
// obs.Init: the milk-log half is a no-op and the OTel half resolves to the
// global no-op logger provider.
func Event(name string, args ...any) {
	emitEvent(log.SeverityInfo, name, args...)
}

// EventWarn emits a structured lifecycle event at WARN level to milk.log and
// logs.jsonl — see Event. Use for failure-shaped lifecycle facts (e.g.
// "background.failed") that should stand out when scanning either signal.
func EventWarn(name string, args ...any) {
	emitEvent(log.SeverityWarn, name, args...)
}

func emitEvent(sev log.Severity, name string, args ...any) {
	if milkLogger != nil {
		if sev == log.SeverityWarn {
			milkLogger.Warn(name, args...)
		} else {
			milkLogger.Info(name, args...)
		}
	}
	rec := log.Record{}
	rec.SetTimestamp(time.Now())
	rec.SetSeverity(sev)
	rec.SetSeverityText(sev.String())
	rec.SetBody(log.StringValue(name))
	rec.AddAttributes(eventAttrs(args)...)
	global.GetLoggerProvider().Logger("github.com/scoutme/milk/obs").Emit(context.Background(), rec)
}

// eventAttrs converts key/value pairs (as passed to Debug/Info/Warn/Event)
// into OTel log attributes.
func eventAttrs(args []any) []log.KeyValue {
	var out []log.KeyValue
	for i := 0; i+1 < len(args); i += 2 {
		k, ok := args[i].(string)
		if !ok {
			k = fmt.Sprintf("%v", args[i])
		}
		out = append(out, log.KeyValue{Key: k, Value: eventValue(args[i+1])})
	}
	return out
}

// eventValue converts one attribute value into an OTel log value, falling
// back to fmt for exotic types. A typed-nil error becomes the empty string
// value rather than "<nil>".
func eventValue(v any) log.Value {
	switch t := v.(type) {
	case string:
		return log.StringValue(t)
	case int:
		return log.Int64Value(int64(t))
	case int64:
		return log.Int64Value(t)
	case bool:
		return log.BoolValue(t)
	case float64:
		return log.Float64Value(t)
	case time.Duration:
		return log.StringValue(t.String())
	case error:
		if t == nil {
			return log.StringValue("")
		}
		return log.StringValue(t.Error())
	default:
		return log.StringValue(fmt.Sprintf("%v", v))
	}
}
