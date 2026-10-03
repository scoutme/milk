package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Fixture-based tests for the batch stream-json adapter. The recordings under
// internal/transport/streamjson/testdata are the ADR-0050 locked-contract
// goldens (recorded `--output-format stream-json` runs) with expected-parse
// companions — replayed here both through parseMilkStream directly (fixture
// tests) and through the full spawn-a-process adapter path (end-to-end tests
// against a stub binary that replays a recording).

const (
	streamJSONEventsDir   = "../internal/transport/streamjson/testdata/events"
	streamJSONExpectedDir = "../internal/transport/streamjson/testdata/expected"
)

// expectedTurn is the testdata/expected/*.json companion shape: the eval
// report fields one recorded stream-json run must map onto.
type expectedTurn struct {
	Response   string     `json:"response"`
	Tokens     TokenUsage `json:"tokens"`
	ToolCalls  []ToolCall `json:"tool_calls"`
	IsError    bool       `json:"is_error"`
	StopReason string     `json:"stop_reason"`
	NumTurns   int64      `json:"num_turns"`
}

func loadExpectedTurn(t *testing.T, base string) expectedTurn {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(streamJSONExpectedDir, base+".json"))
	if err != nil {
		t.Fatalf("reading expected companion for %s: %v", base, err)
	}
	var want expectedTurn
	if err := json.Unmarshal(data, &want); err != nil {
		t.Fatalf("parsing expected companion for %s: %v", base, err)
	}
	return want
}

// assertRunMatches checks the parsed run's eval-shaped mapping against an
// expected companion.
func assertRunMatches(t *testing.T, base string, run *milkStreamRun, want expectedTurn) {
	t.Helper()
	if run.Response != want.Response {
		t.Errorf("%s: response = %q, want %q", base, run.Response, want.Response)
	}
	if !reflect.DeepEqual(run.Tokens, want.Tokens) {
		t.Errorf("%s: tokens = %+v, want %+v", base, run.Tokens, want.Tokens)
	}
	if len(run.ToolCalls) != len(want.ToolCalls) {
		t.Fatalf("%s: got %d tool calls, want %d (%+v)", base, len(run.ToolCalls), len(want.ToolCalls), run.ToolCalls)
	}
	for i, got := range run.ToolCalls {
		w := want.ToolCalls[i]
		if got.Name != w.Name || got.Args != w.Args || got.Result != w.Result {
			t.Errorf("%s: tool call %d = %+v, want %+v", base, i, got, w)
		}
	}
	if run.IsError != want.IsError {
		t.Errorf("%s: is_error = %v, want %v", base, run.IsError, want.IsError)
	}
	if run.StopReason != want.StopReason {
		t.Errorf("%s: stop_reason = %q, want %q", base, run.StopReason, want.StopReason)
	}
	if run.NumTurns != want.NumTurns {
		t.Errorf("%s: num_turns = %d, want %d", base, run.NumTurns, want.NumTurns)
	}
}

// TestParseMilkStream_RecordedFixtures feeds every recorded stream-json golden
// through the parser and asserts the parsed turn/usage against its expected
// companion — the fixture-based half of the adapter contract.
func TestParseMilkStream_RecordedFixtures(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(streamJSONEventsDir, "*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatalf("no recorded fixtures in %s", streamJSONEventsDir)
	}
	for _, f := range files {
		base := strings.TrimSuffix(filepath.Base(f), ".jsonl")
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("reading fixture %s: %v", f, err)
		}
		run, err := parseMilkStream(bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("parseMilkStream(%s): %v", base, err)
		}
		assertRunMatches(t, base, run, loadExpectedTurn(t, base))
	}
}

// TestParseMilkStream_UsageMapping pins the §6.4 usage mapping explicitly:
// result.usage (snake_case cache_read/cache_creation) is authoritative and
// maps onto TokenUsage.CacheRead/CacheCreate; model_usage is the per-model
// breakdown, summed when usage is absent (recorded in
// interrupted_result.jsonl).
func TestParseMilkStream_UsageMapping(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(streamJSONEventsDir, "tool_use_turn.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	run, err := parseMilkStream(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	want := TokenUsage{InputTokens: 2234, OutputTokens: 767, CacheRead: 8000, CacheCreate: 200}
	if !reflect.DeepEqual(run.Tokens, want) {
		t.Errorf("usage = %+v, want %+v", run.Tokens, want)
	}

	// interrupted_result.jsonl carries only model_usage (500 + 20 from the
	// single model) — the fallback sum must land on the same eval shape.
	raw, err = os.ReadFile(filepath.Join(streamJSONEventsDir, "interrupted_result.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	run, err = parseMilkStream(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	want = TokenUsage{InputTokens: 500, OutputTokens: 20}
	if !reflect.DeepEqual(run.Tokens, want) {
		t.Errorf("model_usage fallback = %+v, want %+v", run.Tokens, want)
	}
}

// TestParseMilkStream_InitStateMapping pins the system/init + system/state
// mapping: model identity + config warnings from init, session state and the
// idle stop_reason from state — which also fills in for a result line with no
// stop_reason of its own.
func TestParseMilkStream_InitStateMapping(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"system","subtype":"init","session_id":"s","seq":0,"agent":{"name":"qwen-local","provider":"local","model":"qwen2.5-coder-32b","role":"primary"},"session_state":"idle","warnings":["config: unknown key \"memory_top_k\" — ignored"],"capabilities":["stream_v1"]}`,
		`{"type":"system","subtype":"state","session_id":"s","seq":1,"session_state":"running"}`,
		`{"type":"system","subtype":"warning","session_id":"s","seq":2,"message":"loop streak 4/5"}`,
		`{"type":"assistant","session_id":"s","seq":3,"agent":"primary","message":{"id":"m1","role":"assistant","content":[{"type":"text","text":"partial work"}]}}`,
		`{"type":"system","subtype":"state","session_id":"s","seq":4,"session_state":"idle","stop_reason":"interrupted"}`,
		`{"type":"result","subtype":"interrupted","session_id":"s","seq":5,"is_error":false,"num_turns":1,"duration_ms":42,"result":"partial work"}`,
	}, "\n") + "\n"

	run, err := parseMilkStream(strings.NewReader(stream))
	if err != nil {
		t.Fatalf("parseMilkStream: %v", err)
	}
	if run.Model != "qwen2.5-coder-32b" {
		t.Errorf("model = %q, want qwen2.5-coder-32b (system/init.agent.model)", run.Model)
	}
	if len(run.Warnings) != 2 {
		t.Errorf("warnings = %+v, want the init warning + the system/warning message", run.Warnings)
	}
	if run.State != "idle" {
		t.Errorf("state = %q, want idle (last system/state)", run.State)
	}
	if run.StateStopReason != "interrupted" {
		t.Errorf("state stop_reason = %q, want interrupted", run.StateStopReason)
	}
	// The result line has no stop_reason: system/state supplies it.
	if run.StopReason != "interrupted" {
		t.Errorf("stop_reason = %q, want the system/state fallback interrupted", run.StopReason)
	}
}

// TestParseMilkStream_TruncatedAndMalformed pins the one framing rule a
// consumer must enforce: the terminal result line is required (a stream
// without one is a truncated run). Malformed lines are decode errors.
func TestParseMilkStream_TruncatedAndMalformed(t *testing.T) {
	const initLine = `{"type":"system","subtype":"init","session_id":"s","seq":0,"session_state":"idle","capabilities":["stream_v1"]}`
	const resultLine = `{"type":"result","subtype":"success","session_id":"s","seq":1,"is_error":false,"num_turns":1,"duration_ms":10,"result":"hi","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":2}}`

	for _, tc := range []struct {
		name    string
		stream  string
		wantErr string
	}{
		{"empty", "", "no terminal result event"},
		{"no result", initLine + "\n", "no terminal result event"},
		{"garbage line", initLine + "\n" + "not json\n" + resultLine + "\n", "decode"},
	} {
		_, err := parseMilkStream(strings.NewReader(tc.stream))
		if err == nil {
			t.Errorf("%s: expected error containing %q, got nil", tc.name, tc.wantErr)
			continue
		}
		if !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("%s: error %q does not mention %q", tc.name, err, tc.wantErr)
		}
	}

	// A complete run parses.
	if _, err := parseMilkStream(strings.NewReader(initLine + "\n" + resultLine + "\n")); err != nil {
		t.Errorf("valid stream rejected: %v", err)
	}
}

// TestParseMilkStream_OpenSetTolerance: unknown line types, unknown system
// subtypes and unknown fields must be ignored (§8.3 — open-set enums
// everywhere), never rejected.
func TestParseMilkStream_OpenSetTolerance(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"system","subtype":"init","session_id":"s","seq":0,"agent":{"name":"qwen-local","provider":"local","model":"m1"},"session_state":"idle","capabilities":["stream_v1","future_thing_v1"]}`,
		`{"type":"system","subtype":"route","session_id":"s","seq":1,"target":"primary","brand_new_field":true}`,
		`{"type":"totally_new_event_type","session_id":"s","seq":2,"whatever":[1,2,3]}`,
		`{"type":"stream_event","session_id":"s","seq":3,"event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"done"}}}`,
		`{"type":"assistant","session_id":"s","seq":4,"agent":"primary","message":{"id":"m1","role":"assistant","content":[{"type":"text","text":"done"}]},"extra_field":"ignored"}`,
		`{"type":"system","subtype":"state","session_id":"s","seq":5,"session_state":"idle","stop_reason":"end_turn"}`,
		`{"type":"result","subtype":"brand_new_subtype","session_id":"s","seq":6,"is_error":false,"num_turns":1,"duration_ms":5,"result":"done","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`,
	}, "\n") + "\n"

	run, err := parseMilkStream(strings.NewReader(stream))
	if err != nil {
		t.Fatalf("open-set stream rejected: %v", err)
	}
	if run.Response != "done" {
		t.Errorf("response = %q, want %q", run.Response, "done")
	}
	if run.State != "idle" || run.StopReason != "end_turn" {
		t.Errorf("state/stop_reason = %q/%q, want idle/end_turn", run.State, run.StopReason)
	}
	if run.Subtype != "brand_new_subtype" {
		t.Errorf("result subtype = %q, want the unrecognized subtype preserved verbatim", run.Subtype)
	}
}

// TestMilkAdapter_EndToEnd_RecordedFixture drives the full adapter path —
// spawn, `--output-format stream-json` invocation, JSONL consumption, mapping
// onto RunResult — against a stub binary that replays a recorded golden and
// logs its argv. This is the end-to-end adapter test; against a real installed
// milk binary the same path consumes live stream-json output.
func TestMilkAdapter_EndToEnd_RecordedFixture(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("stub binary is a sh script")
	}
	goldenAbs, err := filepath.Abs(filepath.Join(streamJSONEventsDir, "tool_use_turn.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	argsLog := filepath.Join(tmp, "args.txt")
	stub := filepath.Join(tmp, "milk-stub")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > " + strconv.Quote(argsLog) + "\n" +
		"printf 'new file\\n' > created.txt\n" +
		"cat " + strconv.Quote(goldenAbs) + "\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	workdir := t.TempDir()
	a := &milkAdapter{milkBin: stub}
	a.SetArgs([]string{"--agent", "mimo-local"})
	ctx := context.Background()
	if err := a.Start(ctx, workdir); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer a.Stop()

	result, err := a.RunPrompt(ctx, "What does go.mod declare?")
	if err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}

	want := loadExpectedTurn(t, "tool_use_turn")
	if result.Response != want.Response {
		t.Errorf("response = %q, want %q", result.Response, want.Response)
	}
	if !reflect.DeepEqual(result.Tokens, want.Tokens) {
		t.Errorf("tokens = %+v, want %+v", result.Tokens, want.Tokens)
	}
	if len(result.ToolCalls) != len(want.ToolCalls) {
		t.Fatalf("got %d tool calls, want %d", len(result.ToolCalls), len(want.ToolCalls))
	}
	if result.ToolCalls[0].Name != "read_file" || result.ToolCalls[0].Result != "38 lines" {
		t.Errorf("tool call = %+v, want read_file with tool_use_result.summary \"38 lines\"", result.ToolCalls[0])
	}
	if result.Duration <= 0 || result.Duration > 10*time.Second {
		t.Errorf("duration = %s, want a positive wall-clock measurement", result.Duration)
	}
	if len(result.FileChanges) != 1 || result.FileChanges[0].Path != "created.txt" || !result.FileChanges[0].IsNew {
		t.Errorf("file changes = %+v, want one new file created.txt", result.FileChanges)
	}

	// The batch invocation shape: `milk --output-format stream-json [flags]
	// "prompt"` — the stream-json flag is always present, adapter args are
	// forwarded verbatim, the prompt is the trailing positional.
	argsData, err := os.ReadFile(argsLog)
	if err != nil {
		t.Fatalf("reading stub argv log: %v", err)
	}
	gotArgs := strings.Split(strings.TrimRight(string(argsData), "\n"), "\n")
	wantArgs := []string{"--output-format", "stream-json", "--agent", "mimo-local", "What does go.mod declare?"}
	if !reflect.DeepEqual(gotArgs, wantArgs) {
		t.Errorf("adapter argv = %q, want %q", gotArgs, wantArgs)
	}
}

// TestMilkAdapter_EndToEnd_ErrorResult: a recorded failure run (result subtype
// error_during_execution, is_error) surfaces on RunResult.Error while still
// mapping the partial turn.
func TestMilkAdapter_EndToEnd_ErrorResult(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("stub binary is a sh script")
	}
	goldenAbs, err := filepath.Abs(filepath.Join(streamJSONEventsDir, "error_result.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	stub := filepath.Join(t.TempDir(), "milk-stub")
	script := "#!/bin/sh\ncat " + strconv.Quote(goldenAbs) + "\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	a := &milkAdapter{milkBin: stub}
	if err := a.Start(context.Background(), t.TempDir()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer a.Stop()

	result, err := a.RunPrompt(context.Background(), "do something")
	if err != nil {
		t.Fatalf("RunPrompt should map an is_error result onto RunResult.Error, not fail the call: %v", err)
	}
	if result.Error == nil {
		t.Fatal("expected RunResult.Error for an is_error terminal result")
	}
	if !strings.Contains(result.Error.Error(), "error_during_execution") {
		t.Errorf("error %q should name the result subtype", result.Error)
	}
	if result.Response != "provider request failed: connection reset" {
		t.Errorf("partial turn response = %q", result.Response)
	}
}
