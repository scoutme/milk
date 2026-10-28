package eval

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Fixture-based tests for the mimo-code adapter. The recordings under
// testdata/mimo are real `mimo run --format json` runs (recorded against a
// live mimocode install): simple_run.jsonl is a no-tool reply,
// tool_run.jsonl is a write+bash run with re-emitted tool_use events.

const mimoTestdataDir = "testdata/mimo"

func loadMimoFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(mimoTestdataDir, name))
	if err != nil {
		t.Fatalf("reading fixture %s: %v", name, err)
	}
	return raw
}

// TestParseMimoStream_SimpleRun pins the minimal happy path: one step, the
// single message's text as the response, summed usage, reported cost,
// session capture for later-turn resume, stop reason, and turn count.
func TestParseMimoStream_SimpleRun(t *testing.T) {
	run, err := parseMimoStream(strings.NewReader(string(loadMimoFixture(t, "simple_run.jsonl"))))
	if err != nil {
		t.Fatalf("parseMimoStream: %v", err)
	}

	if run.Response != "pong" {
		t.Errorf("response = %q, want %q", run.Response, "pong")
	}
	if run.SessionID != "ses_ffe5e801b2b80ffeqRTfyRVSIj" {
		t.Errorf("sessionID = %q, want the run's session id", run.SessionID)
	}
	if run.NumTurns != 1 || run.StopReason != "stop" {
		t.Errorf("turns/stop = %d/%q, want 1/\"stop\"", run.NumTurns, run.StopReason)
	}
	want := TokenUsage{InputTokens: 123, OutputTokens: 15, CacheRead: 25472}
	if !reflect.DeepEqual(run.Tokens, want) {
		t.Errorf("tokens = %+v, want %+v", run.Tokens, want)
	}
	if math.Abs(run.CostUSD-0.0001582542) > 1e-9 {
		t.Errorf("cost = %v, want 0.0001582542 (mimo reports cost, unlike the milk adapters)", run.CostUSD)
	}
	if len(run.ToolCalls) != 0 {
		t.Errorf("tool calls = %+v, want none", run.ToolCalls)
	}
}

// TestParseMimoStream_ToolRun pins the tool-run mapping: tool_use events
// are deduped by callID (mimo re-emits each completed call several times —
// 9 events for 3 calls in this recording), response is the *final*
// assistant message (earlier narration must not leak into the judged
// response), usage summed over the 4 steps with reasoning folded into
// output, and the last step_finish reason as stop_reason.
func TestParseMimoStream_ToolRun(t *testing.T) {
	run, err := parseMimoStream(strings.NewReader(string(loadMimoFixture(t, "tool_run.jsonl"))))
	if err != nil {
		t.Fatalf("parseMimoStream: %v", err)
	}

	// Final message only — the pre-tool narration ("Creating the file now.")
	// belongs to an earlier message and must not pollute the response.
	if !strings.HasPrefix(run.Response, "Done.") {
		t.Errorf("response = %q, want the final message starting %q", run.Response, "Done.")
	}
	if strings.Contains(run.Response, "Creating the file now.") {
		t.Errorf("response leaked pre-tool narration: %q", run.Response)
	}
	if !strings.Contains(run.Text, "Creating the file now.") {
		t.Errorf("Text should keep every text block, got %q", run.Text)
	}
	if run.SessionID != "ses_ffe5e801b06acffeSMK1KNzdFZ" {
		t.Errorf("sessionID = %q, want the run's session id", run.SessionID)
	}
	if run.NumTurns != 4 || run.StopReason != "stop" {
		t.Errorf("turns/stop = %d/%q, want 4/\"stop\"", run.NumTurns, run.StopReason)
	}
	// 9 tool_use events → 3 unique callIDs: write, bash (ls), bash (cat).
	wantCalls := []ToolCall{
		{
			Name:   "write",
			Args:   `{"file_path":"/home/scoutme/greeting.txt","content":"hello world.\n"}`,
			Result: "Wrote file successfully.",
		},
		{
			Name:   "bash",
			Args:   `{"command":"ls -l /home/scoutme/greeting.txt","description":"Confirm greeting.txt exists"}`,
			Result: "-rw-r--r-- 1 scoutme scoutme 13 Oct 28 01:24 /home/scoutme/greeting.txt\n",
		},
		{
			Name:   "bash",
			Args:   `{"command":"cat /home/scoutme/greeting.txt","description":"Show greeting.txt contents"}`,
			Result: "hello world.\n",
		},
	}
	if !reflect.DeepEqual(run.ToolCalls, wantCalls) {
		t.Errorf("tool calls =\n%+v\nwant\n%+v", run.ToolCalls, wantCalls)
	}
	want := TokenUsage{InputTokens: 795, OutputTokens: 517, CacheRead: 102400}
	if !reflect.DeepEqual(run.Tokens, want) {
		t.Errorf("tokens = %+v, want %+v (per-step sums, reasoning in output)", run.Tokens, want)
	}
	if math.Abs(run.CostUSD-0.0011642550) > 1e-9 {
		t.Errorf("cost = %v, want 0.001164255 (summed over the 4 steps)", run.CostUSD)
	}
}

// TestParseMimoStream_TruncatedAndMalformed pins the framing rules: the
// stream must contain at least one step_finish (a truncated run is an
// error), while non-JSON prose lines are kept as warnings instead of
// failing the turn.
func TestParseMimoStream_TruncatedAndMalformed(t *testing.T) {
	const textLine = `{"type":"text","sessionID":"s","part":{"type":"text","text":"hi","messageID":"m1"}}`
	const stepLine = `{"type":"step_finish","sessionID":"s","part":{"type":"step-finish","reason":"stop","tokens":{"input":1,"output":2,"reasoning":0,"cache":{"write":0,"read":0}},"cost":0.25}}`

	for _, tc := range []struct {
		name    string
		stream  string
		wantErr string
	}{
		{"empty", "", "no step_finish"},
		{"text only", textLine + "\n", "no step_finish"},
		{"garbage only", "not json at all\n", "no step_finish"},
	} {
		if _, err := parseMimoStream(strings.NewReader(tc.stream)); err == nil {
			t.Errorf("%s: expected error containing %q, got nil", tc.name, tc.wantErr)
		} else if !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("%s: error %q does not mention %q", tc.name, err, tc.wantErr)
		}
	}

	// Prose mixed into stdout: tolerated, kept as a warning, run still parses.
	run, err := parseMimoStream(strings.NewReader("prose line from mimo\n" + textLine + "\n" + stepLine + "\n"))
	if err != nil {
		t.Fatalf("stream with prose line rejected: %v", err)
	}
	if run.Response != "hi" || run.NumTurns != 1 {
		t.Errorf("response/turns = %q/%d, want hi/1", run.Response, run.NumTurns)
	}
	if len(run.Warnings) != 1 || run.Warnings[0] != "prose line from mimo" {
		t.Errorf("warnings = %q, want the prose line captured", run.Warnings)
	}
}

// TestParseMimoStream_OpenSetTolerance: unknown event types, unknown
// fields, unknown part shapes and tool records without a callID must be
// ignored, never rejected (same open-set rule as the milk stream parser).
func TestParseMimoStream_OpenSetTolerance(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"step_start","sessionID":"s","future_field":1,"part":{"id":"p1"}}`,
		`{"type":"brand_new_event","sessionID":"s","part":{"whatever":[1,2,3]}}`,
		`{"type":"tool_use","sessionID":"s","part":{"type":"tool","tool":"mystery","state":{"status":"pending"}}}`,
		`{"type":"text","sessionID":"s","part":{"type":"text","text":"done","messageID":"m1","future":"x"}}`,
		`{"type":"step_finish","sessionID":"s","part":{"type":"step-finish","reason":"end_turn","tokens":{"total":9,"input":1,"output":1,"reasoning":1,"cache":{"write":1,"read":1},"unknown":true},"cost":0.01,"extra":42}}`,
	}, "\n") + "\n"

	run, err := parseMimoStream(strings.NewReader(stream))
	if err != nil {
		t.Fatalf("open-set stream rejected: %v", err)
	}
	if run.Response != "done" {
		t.Errorf("response = %q, want %q", run.Response, "done")
	}
	if run.StopReason != "end_turn" {
		t.Errorf("stop_reason = %q, want end_turn", run.StopReason)
	}
	// tool_use without a callID: not a tool-call record, skipped.
	if len(run.ToolCalls) != 0 {
		t.Errorf("tool calls = %+v, want none (callID-less part skipped)", run.ToolCalls)
	}
	// reasoning 1 + output 1, cache write 1 / read 1, cost 0.01.
	want := TokenUsage{InputTokens: 1, OutputTokens: 2, CacheCreate: 1, CacheRead: 1}
	if !reflect.DeepEqual(run.Tokens, want) {
		t.Errorf("tokens = %+v, want %+v", run.Tokens, want)
	}
	if math.Abs(run.CostUSD-0.01) > 1e-9 {
		t.Errorf("cost = %v, want 0.01", run.CostUSD)
	}
}

// TestMimoAdapter_SetArgsPseudoFlags pins the split between mimo-code's own
// pseudo-flags (--mimo-bin, --no-yolo — never forwarded) and verbatim
// forwarded `mimo run` flags.
func TestMimoAdapter_SetArgsPseudoFlags(t *testing.T) {
	a := &mimoAdapter{}
	a.SetArgs([]string{"--mimo-bin", "/opt/bin/mimo", "--no-yolo", "-m", "anthropic/claude-sonnet-4-5"})
	if a.binArg != "/opt/bin/mimo" {
		t.Errorf("binArg = %q, want /opt/bin/mimo", a.binArg)
	}
	if !a.noYolo {
		t.Error("noYolo = false, want true (--no-yolo sets it)")
	}
	if want := []string{"-m", "anthropic/claude-sonnet-4-5"}; !reflect.DeepEqual(a.extraArgs, want) {
		t.Errorf("extraArgs = %q, want %q (pseudo-flags stripped)", a.extraArgs, want)
	}
}

// TestMimoAdapter_EndToEnd_RecordedFixture drives the full adapter path —
// spawn, `mimo run --format json` invocation, JSONL consumption, mapping
// onto RunResult, session resume — against a stub binary that replays a
// recorded run and logs its argv. Second turn asserts the `-s <sessionID>`
// resume shape and multi-turn conversation continuity.
func TestMimoAdapter_EndToEnd_RecordedFixture(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("stub binary is a sh script")
	}
	fixtureAbs, err := filepath.Abs(filepath.Join(mimoTestdataDir, "tool_run.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	argsLog := filepath.Join(tmp, "args.txt")
	stub := filepath.Join(tmp, "mimo-stub")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" >> " + strconv.Quote(argsLog) + "\n" +
		"printf 'new file\\n' > created.txt\n" +
		"cat " + strconv.Quote(fixtureAbs) + "\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	workdir := t.TempDir()
	a := &mimoAdapter{mimoBin: stub}
	a.SetArgs([]string{"-m", "anthropic/claude-sonnet-4-5"})
	ctx := context.Background()
	if err := a.Start(ctx, workdir); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer a.Stop()

	result, err := a.RunPrompt(ctx, "Create greeting.txt")
	if err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	if !strings.HasPrefix(result.Response, "Done.") {
		t.Errorf("response = %q, want the final message", result.Response)
	}
	if !reflect.DeepEqual(result.Tokens, TokenUsage{InputTokens: 795, OutputTokens: 517, CacheRead: 102400}) {
		t.Errorf("tokens = %+v", result.Tokens)
	}
	if math.Abs(result.CostUSD-0.0011642550) > 1e-9 {
		t.Errorf("cost = %v, want the fixture's summed cost", result.CostUSD)
	}
	if len(result.ToolCalls) != 3 {
		t.Errorf("tool calls = %+v, want 3 (deduped from 9 events)", result.ToolCalls)
	}
	if result.NumTurns != 4 || result.StopReason != "stop" {
		t.Errorf("turns/stop = %d/%q, want 4/stop", result.NumTurns, result.StopReason)
	}
	if len(result.FileChanges) != 1 || result.FileChanges[0].Path != "created.txt" || !result.FileChanges[0].IsNew {
		t.Errorf("file changes = %+v, want one new file created.txt", result.FileChanges)
	}
	if result.Duration <= 0 || result.Duration > 10*time.Second {
		t.Errorf("duration = %s, want a positive wall-clock measurement", result.Duration)
	}
	if a.sessionID != "ses_ffe5e801b06acffeSMK1KNzdFZ" {
		t.Fatalf("sessionID not captured from the run's events: %q", a.sessionID)
	}

	// Second turn resumes the captured session.
	if _, err := a.RunPrompt(ctx, "Now verify it"); err != nil {
		t.Fatalf("RunPrompt (turn 2): %v", err)
	}

	argsData, err := os.ReadFile(argsLog)
	if err != nil {
		t.Fatalf("reading stub argv log: %v", err)
	}
	gotArgs := strings.Split(strings.TrimRight(string(argsData), "\n"), "\n")
	wantArgs := []string{
		"run", "--format", "json", "--dir", workdir, "--yolo",
		"-m", "anthropic/claude-sonnet-4-5", "--", "Create greeting.txt",
		"run", "--format", "json", "--dir", workdir, "--yolo",
		"-s", "ses_ffe5e801b06acffeSMK1KNzdFZ",
		"-m", "anthropic/claude-sonnet-4-5", "--", "Now verify it",
	}
	if !reflect.DeepEqual(gotArgs, wantArgs) {
		t.Errorf("adapter argv =\n%q\nwant\n%q", gotArgs, wantArgs)
	}
}

// TestMimoAdapter_NoYoloOmitsFlag: --no-yolo must drop the pseudo-flag from
// the spawned argv (and mimo still starts).
func TestMimoAdapter_NoYoloOmitsFlag(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("stub binary is a sh script")
	}
	fixtureAbs, err := filepath.Abs(filepath.Join(mimoTestdataDir, "simple_run.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	argsLog := filepath.Join(tmp, "args.txt")
	stub := filepath.Join(tmp, "mimo-stub")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" >> " + strconv.Quote(argsLog) + "\n" +
		"cat " + strconv.Quote(fixtureAbs) + "\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	a := &mimoAdapter{mimoBin: stub}
	a.SetArgs([]string{"--no-yolo"})
	ctx := context.Background()
	if err := a.Start(ctx, t.TempDir()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer a.Stop()
	if _, err := a.RunPrompt(ctx, "ping"); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}

	argsData, err := os.ReadFile(argsLog)
	if err != nil {
		t.Fatal(err)
	}
	got := string(argsData)
	if strings.Contains(got, "--yolo") {
		t.Errorf("--no-yolo still forwarded --yolo: %q", got)
	}
}
