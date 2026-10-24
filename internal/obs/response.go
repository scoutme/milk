package obs

import "time"

// ResponseLog is what one finished agent turn reports to the milk log.
type ResponseLog struct {
	Agent, Model, Role string
	Input, Output      int64
	CacheRead, CacheWr int64
	TTFT, Decode       time.Duration
	CostUSD            float64
}

// LogResponse writes one structured "agent response" line when an agent
// answers: who answered, what it cost and how fast it was. It is Info level, so
// it shows without enabling debug; with log_format "pretty" it is colored.
// Rates are left out when the provider reported no timing, never faked as 0.
func LogResponse(r ResponseLog) {
	attrs := []any{
		"agent", r.Agent, "model", r.Model, "role", r.Role,
		"input", r.Input, "output", r.Output,
	}
	if r.CacheRead > 0 || r.CacheWr > 0 {
		attrs = append(attrs, "cache_read", r.CacheRead, "cache_write", r.CacheWr)
	}
	if r.TTFT > 0 {
		attrs = append(attrs, "ttft_ms", r.TTFT.Milliseconds())
	}
	if r.Decode > 0 && r.Output > 0 {
		attrs = append(attrs, "tok_s", float64(int(float64(r.Output)/r.Decode.Seconds()*10))/10)
	}
	if r.CostUSD > 0 {
		attrs = append(attrs, "cost_usd", r.CostUSD)
	}
	Info("agent response", attrs...)
}
