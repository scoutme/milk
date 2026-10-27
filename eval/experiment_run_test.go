package eval

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// stubAdapter is a scriptable AgentAdapter + Sandboxable for harness tests.
type stubAdapter struct {
	sandboxes []SandboxConfig
	starts    int
	stops     int
	run       func(call int) RunResult
}

func (s *stubAdapter) Name() string               { return "stub" }
func (s *stubAdapter) SetArgs(args []string)      {}
func (s *stubAdapter) SetSandbox(c SandboxConfig) { s.sandboxes = append(s.sandboxes, c) }
func (s *stubAdapter) Start(ctx context.Context, wd string) error {
	s.starts++
	return nil
}
func (s *stubAdapter) Stop() error { s.stops++; return nil }
func (s *stubAdapter) RunPrompt(ctx context.Context, prompt string) (RunResult, error) {
	return s.run(len(s.sandboxes) + s.starts), nil
}

// plainAdapter is a stubAdapter without the Sandboxable interface.
type plainAdapter struct {
	starts int
}

func (p *plainAdapter) Name() string          { return "plain" }
func (p *plainAdapter) SetArgs(args []string) {}
func (p *plainAdapter) Start(ctx context.Context, wd string) error {
	p.starts++
	return nil
}
func (p *plainAdapter) Stop() error { return nil }
func (p *plainAdapter) RunPrompt(ctx context.Context, prompt string) (RunResult, error) {
	return RunResult{Response: "ok"}, nil
}

func TestRunExperiment_EndToEnd(t *testing.T) {
	// The judge talks to a mock server (scores every criterion 4.0).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"[{\"criterion\":\"correctness\",\"score\":4.0,\"reasoning\":\"ok\"}]"}}]}`))
	}))
	defer srv.Close()

	// Scenario + experiment on disk.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "scen.yaml"), []byte(`
category: metrics
scenarios:
  - name: fixed-request
    prompt: do the thing
    rubric:
      - criterion: correctness
        description: did it
        weight: 1
        scoring: scale_1_5
`), 0644); err != nil {
		t.Fatal(err)
	}

	// The "current config" the sandbox seeds from.
	root := t.TempDir()
	t.Setenv("MILK_CONFIG_ROOT", root)
	if err := os.WriteFile(filepath.Join(root, "config.json"), []byte(`{"agents":[]}`), 0644); err != nil {
		t.Fatal(err)
	}

	expPath := filepath.Join(dir, "exp.yaml")
	if err := os.WriteFile(expPath, []byte(`
name: testbed
scenarios: .
select:
  names: [fixed-request]
repetitions: 2
sandbox:
  mode: fresh
  seed: config
budget:
  max_tool_calls: 1
`), 0644); err != nil {
		t.Fatal(err)
	}
	exp, err := LoadExperiment(expPath)
	if err != nil {
		t.Fatalf("LoadExperiment: %v", err)
	}

	stub := &stubAdapter{run: func(call int) RunResult {
		return RunResult{
			Response:  "done",
			Tokens:    TokenUsage{InputTokens: int64(100 * call), OutputTokens: 10},
			Duration:  time.Second,
			ToolCalls: []ToolCall{{Name: "bash"}, {Name: "read_file"}},
		}
	}}
	h := &Harness{
		Adapters: []AgentAdapter{stub},
		Judge:    &Judge{URL: srv.URL, HTTPClient: srv.Client()},
	}

	results, err := h.RunExperiment(context.Background(), exp)
	if err != nil {
		t.Fatalf("RunExperiment: %v", err)
	}

	if len(results) != 2 {
		t.Fatalf("want 2 repetition results, got %d", len(results))
	}
	for i, sr := range results {
		if sr.Repetition != i+1 {
			t.Errorf("results[%d].Repetition = %d", i, sr.Repetition)
		}
		ar, ok := sr.AgentResults["stub"]
		if !ok {
			t.Fatalf("no stub result in %+v", sr)
		}
		if ar.BudgetResult == nil {
			t.Fatal("budget verdict missing (experiment budget must apply)")
		}
		if ar.BudgetResult.Passed {
			t.Errorf("2 tool calls must exceed max_tool_calls 1: %+v", ar.BudgetResult)
		}
		if len(ar.BudgetResult.Violations) == 0 {
			t.Error("expected a violation message")
		}
		if ar.WeightedScore != 4.0 {
			t.Errorf("WeightedScore = %v, want 4.0", ar.WeightedScore)
		}
	}

	// Sandbox wiring: configured once, fresh root per Start (2 runs).
	if len(stub.sandboxes) != 1 {
		t.Fatalf("SetSandbox calls = %d, want 1 (per experiment, not per run)", len(stub.sandboxes))
	}
	sc := stub.sandboxes[0]
	if !sc.Enabled {
		t.Error("sandbox not enabled")
	}
	if sc.SeedConfigPath != filepath.Join(root, "config.json") {
		t.Errorf("seed = %q, want the current config", sc.SeedConfigPath)
	}
	if stub.starts != 2 || stub.stops != 2 {
		t.Errorf("adapter lifecycle = %d starts / %d stops, want 2/2 (fresh per repetition)", stub.starts, stub.stops)
	}
}

func TestRunExperiment_SandboxUnsupportedAdapterNoted(t *testing.T) {
	// An experiment asking for fresh sandboxes must not fail against an
	// adapter without sandbox support — it just runs unsandboxed.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"[]"}}]}`))
	}))
	defer srv.Close()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "scen.yaml"), []byte("prompt: hi\nrubric: []\n"), 0644); err != nil {
		t.Fatal(err)
	}
	expPath := filepath.Join(dir, "exp.yaml")
	if err := os.WriteFile(expPath, []byte("scenarios: .\nsandbox: {mode: fresh}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	exp, err := LoadExperiment(expPath)
	if err != nil {
		t.Fatalf("LoadExperiment: %v", err)
	}

	h := &Harness{
		Adapters: []AgentAdapter{&plainAdapter{}},
		Judge:    &Judge{URL: srv.URL, HTTPClient: srv.Client()},
	}
	if _, err := h.RunExperiment(context.Background(), exp); err != nil {
		t.Fatalf("RunExperiment: %v", err)
	}
}
