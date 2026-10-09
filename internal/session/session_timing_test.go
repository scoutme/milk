package session

import (
	"encoding/json"
	"testing"
	"time"
)

// TestAddTiming_AccumulatesAndSkipsUnmeasured: measured requests accumulate
// TTFT/decode seconds and the request count; requests with no timing at all
// (both zero — e.g. generic subprocess agents) are not counted, so a partial
// denominator never dilutes tok/s.
func TestAddTiming_AccumulatesAndSkipsUnmeasured(t *testing.T) {
	s := &Session{}
	s.AddTiming("m", "primary", 800*time.Millisecond, 2*time.Second)
	s.AddTiming("m", "primary", 200*time.Millisecond, 3*time.Second)
	s.AddTiming("m", "primary", 0, 0)

	e := s.Tokens["m\x00primary"]
	if e == nil {
		t.Fatal("expected a token-usage entry for m/primary")
	}
	if e.DecodeSeconds != 5 {
		t.Errorf("DecodeSeconds = %v, want 5", e.DecodeSeconds)
	}
	if e.TTFTSeconds != 1 {
		t.Errorf("TTFTSeconds = %v, want 1", e.TTFTSeconds)
	}
	if e.Requests != 2 {
		t.Errorf("Requests = %d, want 2 (unmeasured request not counted)", e.Requests)
	}
	// Token fields must be untouched by AddTiming.
	if e.Prompt != 0 || e.Completion != 0 {
		t.Errorf("token counts changed by AddTiming: %+v", e)
	}
}

// TestAddTiming_NoopOnUnmeasuredAndBlank: no entry is created when nothing
// was measured or the model/role is blank (mirrors AddTokensFull's guards).
func TestAddTiming_NoopOnUnmeasuredAndBlank(t *testing.T) {
	s := &Session{}
	s.AddTiming("m", "primary", 0, 0)
	s.AddTiming("", "primary", time.Second, time.Second)
	s.AddTiming("m", "", time.Second, time.Second)
	if len(s.Tokens) != 0 {
		t.Errorf("expected no entries, got %+v", s.Tokens)
	}
}

// TestTokenUsageTimingJSONRoundTrip: the timing fields persist with the
// session file, and pre-feature JSON (no timing keys) still decodes.
func TestTokenUsageTimingJSONRoundTrip(t *testing.T) {
	s := &Session{}
	s.AddTiming("m", "escalation", 400*time.Millisecond, 4*time.Second)
	b, err := json.Marshal(s.Tokens)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back map[string]*TokenUsage
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	e := back["m\x00escalation"]
	if e == nil || e.DecodeSeconds != 4 || e.Requests != 1 {
		t.Errorf("round-trip entry = %+v, want DecodeSeconds=4 Requests=1", e)
	}

	old := []byte(`{"k":{"model":"m","agent":"primary","prompt":10,"completion":5}}`)
	var legacy map[string]*TokenUsage
	if err := json.Unmarshal(old, &legacy); err != nil {
		t.Fatalf("legacy unmarshal: %v", err)
	}
	if le := legacy["k"]; le == nil || le.DecodeSeconds != 0 || le.Requests != 0 {
		t.Errorf("legacy entry = %+v, want zero timing", le)
	}
}
