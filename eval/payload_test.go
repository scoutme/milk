package eval

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"
)

// TestParseMilkStream_Payload pins result.payload capture (issue #462's
// meter): the first-request composition lands on the internal run and the
// exported RunResult; a result without payload stays nil (older builds,
// non-HTTP providers).
func TestParseMilkStream_Payload(t *testing.T) {
	stream := `{"type":"system","subtype":"init","session_id":"s1","agent":{"name":"mimo-local","provider":"bearer","model":"mimo-v2.6-flash"},"route":{"target":"primary","reason":"short prompt (local)","conclusive":true}}
{"type":"result","subtype":"success","session_id":"s1","is_error":false,"result":"hi","stop_reason":"end_turn","route_history":[{"turn":0,"target":"primary"}],"payload":{"system_bytes":6400,"tools_bytes":15100,"messages_bytes":6700,"diet":"compact"},"usage":{"input_tokens":10,"output_tokens":5}}
`
	run, err := parseMilkStream(bytes.NewReader([]byte(stream)))
	if err != nil {
		t.Fatalf("parseMilkStream: %v", err)
	}
	if run.Payload == nil {
		t.Fatalf("Payload not captured from result line")
	}
	if run.Payload.SystemBytes != 6400 || run.Payload.ToolsBytes != 15100 || run.Payload.MessagesBytes != 6700 || run.Payload.Diet != "compact" {
		t.Errorf("Payload = %+v, want the result payload verbatim", run.Payload)
	}

	res := milkRunResult(run, runStart(), nil, nil)
	if res.Payload == nil || res.Payload.ToolsBytes != 15100 {
		t.Errorf("RunResult.Payload = %+v, want the parsed payload", res.Payload)
	}

	// Result without payload → nil, not a fabricated zero value.
	stream = `{"type":"result","subtype":"success","session_id":"s1","is_error":false,"result":"hi","stop_reason":"end_turn","route_history":[{"turn":0,"target":"primary"}],"usage":{"input_tokens":1,"output_tokens":1}}
`
	run, err = parseMilkStream(bytes.NewReader([]byte(stream)))
	if err != nil {
		t.Fatalf("parseMilkStream: %v", err)
	}
	if run.Payload != nil {
		t.Errorf("Payload = %+v, want nil when the result reports none", run.Payload)
	}
}

// TestBuildPayloadLines pins the text report line (mirrors
// buildRoutingLines' placement behind the metrics table).
func TestBuildPayloadLines(t *testing.T) {
	sr := ScenarioResult{
		ScenarioName: "add-function",
		AgentResults: map[string]AgentResult{
			"milk": {RunResults: []RunResult{{
				Payload: &PayloadInfo{SystemBytes: 8700, ToolsBytes: 20700, MessagesBytes: 10000, Diet: "full"},
			}}},
			"silent": {RunResults: []RunResult{{}}},
		},
	}
	got := buildPayloadLines(sr, []string{"milk", "silent"})
	want := "Payload [milk]: ~7.7k tok first request — system 8.7k B, tools 20.7k B, msgs 10.0k B (diet: full)\n\n"
	if got != want {
		t.Errorf("buildPayloadLines =\n%q\nwant\n%q", got, want)
	}

	// No reporter anywhere → no section at all.
	sr2 := ScenarioResult{AgentResults: map[string]AgentResult{"milk": {RunResults: []RunResult{{}}}}}
	if got := buildPayloadLines(sr2, []string{"milk"}); got != "" {
		t.Errorf("empty payload section = %q, want empty", got)
	}
}

// TestPayloadJSONField checks the field survives GenerateJSON's shape:
// results.json carries the per-turn payload for cross-run comparisons.
func TestPayloadJSONField(t *testing.T) {
	r := JSONRunResult{Payload: &PayloadInfo{SystemBytes: 1, ToolsBytes: 2, MessagesBytes: 3, Diet: "lean"}}
	out, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Contains(out, []byte(`"payload":`)) || !bytes.Contains(out, []byte(`"diet":"lean"`)) {
		t.Errorf("JSONRunResult payload not serialized: %s", out)
	}
	// omitempty: nil payload stays out of the file (old runs re-render clean).
	out, err = json.Marshal(JSONRunResult{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if bytes.Contains(out, []byte(`"payload"`)) {
		t.Errorf("nil payload must be omitted: %s", out)
	}
}

// helpers -----------------------------------------------------------------

func runStart() time.Time { return time.Time{} }
