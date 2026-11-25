package eval

import (
	"bytes"
	"reflect"
	"testing"
)

// TestParseMilkStream_RouteReasonAndModels pins the two attribution fields
// the milk-vs-pi analysis found missing: the router's decision reason and
// the models that actually billed the turn. The reason is sourced from
// system/init's route decision (one milk batch run makes exactly one routing
// decision); models_used is the result line's model_usage key set — the
// billing truth when an escalation hop ran a different model from the
// session primary that `model` reports.
func TestParseMilkStream_RouteReasonAndModels(t *testing.T) {
	stream := `{"type":"system","subtype":"init","session_id":"s1","agent":{"name":"mimo-local","provider":"bearer","model":"mimo-v2.6-flash"},"route":{"target":"escalation","reason":"model classifier","conclusive":true}}
{"type":"result","subtype":"success","session_id":"s1","is_error":false,"result":"hi","stop_reason":"end_turn","route_history":[{"turn":0,"target":"escalation"}],"usage":{"input_tokens":10,"output_tokens":5},"model_usage":{"mimo-v2.6-pro":{"input_tokens":10,"output_tokens":5},"mimo-v2.6-flash":{"input_tokens":1,"output_tokens":1}}}
`
	run, err := parseMilkStream(bytes.NewReader([]byte(stream)))
	if err != nil {
		t.Fatalf("parseMilkStream: %v", err)
	}

	if !reflect.DeepEqual(run.RouteReasons, []string{"model classifier"}) {
		t.Errorf("RouteReasons = %v, want [model classifier]", run.RouteReasons)
	}
	want := []string{"mimo-v2.6-flash", "mimo-v2.6-pro"}
	if !reflect.DeepEqual(run.ModelsUsed, want) {
		t.Errorf("ModelsUsed = %v, want %v (sorted model_usage keys)", run.ModelsUsed, want)
	}
	if run.Model != "mimo-v2.6-flash" {
		t.Errorf("Model = %q, want the init primary (unchanged semantics)", run.Model)
	}

	// A run whose init carries no route decision records empty reasons
	// rather than inventing one — and no route means no reason rows at all.
	stream = `{"type":"result","subtype":"success","session_id":"s1","is_error":false,"result":"hi","stop_reason":"end_turn","route_history":[{"turn":0,"target":"primary"}],"usage":{"input_tokens":1,"output_tokens":1}}
`
	run, err = parseMilkStream(bytes.NewReader([]byte(stream)))
	if err != nil {
		t.Fatalf("parseMilkStream: %v", err)
	}
	if !reflect.DeepEqual(run.RouteReasons, []string{""}) {
		t.Errorf("RouteReasons = %v, want [\"\"] (no init reason to source)", run.RouteReasons)
	}
	if len(run.ModelsUsed) != 0 {
		t.Errorf("ModelsUsed = %v, want empty without model_usage", run.ModelsUsed)
	}
}
