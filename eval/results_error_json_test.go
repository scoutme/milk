package eval

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A failed turn's error must survive the results.json round-trip. The bare
// `error` interface marshals to `{}` (no exported fields), and `{}` cannot
// decode into an `error`-typed field — before the MarshalJSON/UnmarshalJSON
// pair on RunResult, any run containing a failed turn made `milk eval
// report` fail with:
//
//	json: cannot unmarshal object into Go struct field RunResult.agent_results.run_results.error of type error
func TestRunResultErrorJSONRoundTrip(t *testing.T) {
	res := RunResult{Response: "partial", Error: errors.New("milk batch run: exit status 1")}

	data, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(data), `"error":"milk batch run: exit status 1"`) {
		t.Errorf("error not serialised as its message string: %s", data)
	}

	var back RunResult
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Error == nil || back.Error.Error() != "milk batch run: exit status 1" {
		t.Errorf("round-tripped Error = %v, want %q", back.Error, "milk batch run: exit status 1")
	}

	// A nil error stays absent and decodes back as nil.
	plain, err := json.Marshal(RunResult{Response: "ok"})
	if err != nil {
		t.Fatalf("marshal plain: %v", err)
	}
	if strings.Contains(string(plain), `"error"`) {
		t.Errorf("nil error must be omitted: %s", plain)
	}
	var plainBack RunResult
	if err := json.Unmarshal(plain, &plainBack); err != nil {
		t.Fatalf("unmarshal plain: %v", err)
	}
	if plainBack.Error != nil {
		t.Errorf("nil round-trip Error = %v, want nil", plainBack.Error)
	}

	// Legacy object form (files written before the fix): must still load,
	// flagged as failed with a placeholder message.
	var legacy RunResult
	if err := json.Unmarshal([]byte(`{"response":"x","error":{}}`), &legacy); err != nil {
		t.Fatalf("legacy object form must load: %v", err)
	}
	if legacy.Error == nil {
		t.Error("legacy object form must still flag the turn as failed")
	}

	// Nested shape, as written into results.json and loaded by loadResults.
	sr := ScenarioResult{
		ScenarioName: "failed-turn",
		AgentResults: map[string]AgentResult{
			"milk-tui": {RunResults: []RunResult{res}},
		},
	}
	nested, err := json.Marshal([]ScenarioResult{sr})
	if err != nil {
		t.Fatalf("marshal scenario: %v", err)
	}
	var nestedBack []ScenarioResult
	if err := json.Unmarshal(nested, &nestedBack); err != nil {
		t.Fatalf("unmarshal scenario: %v", err)
	}
	got := nestedBack[0].AgentResults["milk-tui"].RunResults[0]
	if got.Error == nil || got.Error.Error() != "milk batch run: exit status 1" {
		t.Errorf("nested round-tripped Error = %v", got.Error)
	}
}

// TestLoadResultsFailedTurn reproduces the original failure at the exact
// entry point: a results.json holding a failed turn must load (string error
// form now; legacy `{}` tolerated).
func TestLoadResultsFailedTurn(t *testing.T) {
	const payload = `[{
		"scenario_name": "proceed-on-hard-design-task",
		"category": "routing",
		"difficulty": "hard",
		"agent_results": {
			"milk-tui": {
				"run_results": [
					{"response": "partial", "duration": 950000000000,
					 "error": "milk batch run: exit status 1"}
				],
				"scores": [],
				"weighted_score": 1.8
			},
			"mimo-code": {"run_results": [], "scores": [], "weighted_score": 4.4}
		}
	}]`
	for _, tc := range []struct {
		name    string
		payload string
		wantErr string
	}{
		{"string form", payload, "milk batch run: exit status 1"},
		{"legacy object form", strings.Replace(payload, `"milk batch run: exit status 1"`, `{}`, 1), "error detail not serialised"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "results.json"), []byte(tc.payload), 0o644); err != nil {
				t.Fatal(err)
			}
			results, err := loadResults(dir)
			if err != nil {
				t.Fatalf("loadResults: %v", err)
			}
			got := results[0].AgentResults["milk-tui"].RunResults[0]
			if got.Error == nil {
				t.Fatal("failed turn must load with a non-nil Error")
			}
			if !strings.Contains(got.Error.Error(), tc.wantErr) {
				t.Errorf("loaded Error = %q, want it to contain %q", got.Error, tc.wantErr)
			}
			if results[0].AgentResults["mimo-code"].WeightedScore != 4.4 {
				t.Errorf("unrelated fields must survive: %v", results[0].AgentResults["mimo-code"].WeightedScore)
			}
		})
	}
}
