package main

import (
	"testing"

	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/router"
	"github.com/scoutme/milk/internal/session"
	"github.com/scoutme/milk/internal/transport/streamjson"
)

func TestParseOutputFormat(t *testing.T) {
	tests := []struct {
		in      string
		want    outputFormat
		wantErr bool
	}{
		{"text", formatText, false},
		{"json", formatJSON, false},
		{"stream-json", formatStreamJSON, false},
		{"", "", true},
		{"xml", "", true},
		{"JSON", "", true}, // case-sensitive — matches cobra's own flag convention
	}
	for _, tt := range tests {
		got, err := parseOutputFormat(tt.in)
		if tt.wantErr {
			if err == nil {
				t.Errorf("parseOutputFormat(%q): want error, got nil", tt.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseOutputFormat(%q): unexpected error: %v", tt.in, err)
		}
		if got != tt.want {
			t.Errorf("parseOutputFormat(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestWireTarget(t *testing.T) {
	if got := wireTarget(router.TargetLocal); got != "primary" {
		t.Errorf("wireTarget(TargetLocal) = %q, want %q", got, "primary")
	}
	if got := wireTarget(router.TargetEscalation); got != "escalation" {
		t.Errorf("wireTarget(TargetEscalation) = %q, want %q", got, "escalation")
	}
}

func TestTokenUsageDelta(t *testing.T) {
	before := map[string]session.TokenUsage{
		"modelA\x00primary": {Model: "modelA", Agent: "primary", Prompt: 100, Completion: 50, CacheRead: 10, CacheCreation: 5},
	}
	after := map[string]session.TokenUsage{
		"modelA\x00primary":          {Model: "modelA", Agent: "primary", Prompt: 150, Completion: 80, CacheRead: 20, CacheCreation: 5},
		"modelB\x00escalation":       {Model: "modelB", Agent: "escalation", Prompt: 40, Completion: 10},
		"modelC\x00primary:subagent": {Model: "modelC", Agent: "primary:subagent", Prompt: 0, Completion: 0}, // untouched this turn
	}

	total, perModel := tokenUsageDelta(before, after)

	wantTotal := &streamjson.Usage{InputTokens: 50 + 40, OutputTokens: 30 + 10, CacheRead: 10, CacheCreation: 0}
	if *total != *wantTotal {
		t.Errorf("total = %+v, want %+v", total, wantTotal)
	}

	if len(perModel) != 2 {
		t.Fatalf("perModel has %d entries, want 2 (modelC should be dropped as a zero-delta entry): %+v", len(perModel), perModel)
	}
	if m := perModel["modelA"]; m == nil || m.InputTokens != 50 || m.OutputTokens != 30 || m.CacheRead != 10 {
		t.Errorf("perModel[modelA] = %+v, want {InputTokens:50 OutputTokens:30 CacheRead:10 CacheCreation:0}", m)
	}
	if m := perModel["modelB"]; m == nil || m.InputTokens != 40 || m.OutputTokens != 10 {
		t.Errorf("perModel[modelB] = %+v, want {InputTokens:40 OutputTokens:10}", m)
	}
	if _, ok := perModel["modelC"]; ok {
		t.Errorf("perModel[modelC] should be absent (zero delta), got %+v", perModel["modelC"])
	}
}

func TestBuildInitEvent(t *testing.T) {
	cfg := config.Config{
		Agents: []config.AgentConfig{
			{Name: "qwen-local", Provider: "local", Model: "qwen2.5-coder-32b"},
		},
		EscalationAgent: "claude",
	}
	decision := router.Decision{Target: router.TargetLocal, Reason: "default", Conclusive: true}

	ev := buildInitEvent("sess_123", "/repo", cfg, decision, router.TargetLocal, []string{"warn1"})

	if ev.Type != streamjson.TypeSystem || ev.Subtype != streamjson.SubtypeInit {
		t.Errorf("type/subtype = %q/%q, want system/init", ev.Type, ev.Subtype)
	}
	if ev.SessionID != "sess_123" || ev.CWD != "/repo" {
		t.Errorf("SessionID/CWD = %q/%q", ev.SessionID, ev.CWD)
	}
	agent, err := ev.AsAgent()
	if err != nil {
		t.Fatalf("AsAgent: %v", err)
	}
	if agent == nil || agent.Name != "qwen-local" || agent.Role != "primary" {
		t.Errorf("agent = %+v, want Name=qwen-local Role=primary", agent)
	}
	if ev.EscalationAgent == nil || ev.EscalationAgent.Provider != "claude-cli" {
		t.Errorf("EscalationAgent = %+v, want the default claude-cli entry", ev.EscalationAgent)
	}
	if ev.Route == nil || ev.Route.Target != "primary" || ev.Route.Reason != "default" {
		t.Errorf("Route = %+v, want {Target:primary Reason:default}", ev.Route)
	}
	if len(ev.Warnings) != 1 || ev.Warnings[0] != "warn1" {
		t.Errorf("Warnings = %v, want [warn1]", ev.Warnings)
	}
	if len(ev.Capabilities) != 1 || ev.Capabilities[0] != streamjson.CapabilityStream {
		t.Errorf("Capabilities = %v, want [stream_v1] only", ev.Capabilities)
	}

	// Round-trip through the locked contract codec to catch shape drift.
	line, err := streamjson.EncodeLine(ev)
	if err != nil {
		t.Fatalf("EncodeLine: %v", err)
	}
	if _, err := streamjson.Decode(line[:len(line)-1]); err != nil {
		t.Fatalf("Decode(EncodeLine(ev)): %v", err)
	}
}

func TestBuildAssistantEvent(t *testing.T) {
	ev := buildAssistantEvent("sess_123", "hello there")
	if ev.Type != streamjson.TypeAssistant || ev.SessionID != "sess_123" {
		t.Errorf("Type/SessionID = %q/%q", ev.Type, ev.SessionID)
	}
	msg, err := ev.AsMessage()
	if err != nil {
		t.Fatalf("AsMessage: %v", err)
	}
	if msg == nil || len(msg.Content) != 1 || msg.Content[0].Text != "hello there" {
		t.Errorf("message = %+v, want one text block %q", msg, "hello there")
	}

	line, err := streamjson.EncodeLine(ev)
	if err != nil {
		t.Fatalf("EncodeLine: %v", err)
	}
	if _, err := streamjson.Decode(line[:len(line)-1]); err != nil {
		t.Fatalf("Decode(EncodeLine(ev)): %v", err)
	}
}

func TestBuildResultEvent_Success(t *testing.T) {
	before := map[string]session.TokenUsage{}
	after := map[string]session.TokenUsage{
		"m\x00primary": {Model: "m", Agent: "primary", Prompt: 10, Completion: 5},
	}

	ev := buildResultEvent("sess_123", router.TargetLocal, nil, "the answer", 1234, before, after)

	if ev.Type != streamjson.TypeResult || ev.Subtype != streamjson.ResultSuccess {
		t.Errorf("Type/Subtype = %q/%q, want result/success", ev.Type, ev.Subtype)
	}
	if ev.IsError == nil || *ev.IsError {
		t.Errorf("IsError = %v, want false", ev.IsError)
	}
	if ev.NumTurns != 1 || ev.DurationMS != 1234 || ev.Result != "the answer" {
		t.Errorf("NumTurns/DurationMS/Result = %d/%d/%q", ev.NumTurns, ev.DurationMS, ev.Result)
	}
	if ev.StopReason != "end_turn" {
		t.Errorf("StopReason = %q, want end_turn", ev.StopReason)
	}
	if len(ev.RouteHistory) != 1 || ev.RouteHistory[0].Target != "primary" {
		t.Errorf("RouteHistory = %+v, want one hop target=primary", ev.RouteHistory)
	}
	if ev.Usage == nil || ev.Usage.InputTokens != 10 || ev.Usage.OutputTokens != 5 {
		t.Errorf("Usage = %+v, want InputTokens=10 OutputTokens=5", ev.Usage)
	}

	line, err := streamjson.EncodeLine(ev)
	if err != nil {
		t.Fatalf("EncodeLine: %v", err)
	}
	if _, err := streamjson.Decode(line[:len(line)-1]); err != nil {
		t.Fatalf("Decode(EncodeLine(ev)): %v", err)
	}
}

func TestBuildResultEvent_Error(t *testing.T) {
	ev := buildResultEvent("sess_123", router.TargetEscalation, errTest, "", 500, nil, nil)

	if ev.Subtype != streamjson.ResultErrorDuringExecution {
		t.Errorf("Subtype = %q, want error_during_execution", ev.Subtype)
	}
	if ev.IsError == nil || !*ev.IsError {
		t.Errorf("IsError = %v, want true", ev.IsError)
	}
	if ev.StopReason != "" {
		t.Errorf("StopReason = %q, want empty on error", ev.StopReason)
	}
	if ev.Result != "" {
		t.Errorf("Result = %q, want empty", ev.Result)
	}
}

var errTest = &testError{"boom"}

type testError struct{ msg string }

func (e *testError) Error() string { return e.msg }
