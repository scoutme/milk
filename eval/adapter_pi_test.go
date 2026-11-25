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

// Fixture-based tests for the pi adapter. The recordings under
// testdata/pi are real `pi --mode json` runs (recorded against a live pi
// 1.6.0 install on the xiaomi provider): simple_run.jsonl is a no-tool
// reply, tool_run.jsonl is a write+write/bash run with four model
// iterations.

const piTestdataDir = "testdata/pi"

func loadPiFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(piTestdataDir, name))
	if err != nil {
		t.Fatalf("reading fixture %s: %v", name, err)
	}
	return raw
}

// TestParsePiStream_SimpleRun pins the minimal happy path: one assistant
// message, its text as the response, per-message usage (input excludes
// cache, output already includes reasoning — NOT added again), the
// catalog-derived cost, model identity, stop reason and turn count.
func TestParsePiStream_SimpleRun(t *testing.T) {
	run, err := parsePiStream(strings.NewReader(string(loadPiFixture(t, "simple_run.jsonl"))))
	if err != nil {
		t.Fatalf("parsePiStream: %v", err)
	}

	if run.Response != "pong" {
		t.Errorf("response = %q, want %q", run.Response, "pong")
	}
	if run.NumTurns != 1 || run.StopReason != "stop" {
		t.Errorf("turns/stop = %d/%q, want 1/\"stop\"", run.NumTurns, run.StopReason)
	}
	if run.Model != "mimo-v2.6-flash" {
		t.Errorf("model = %q, want mimo-v2.6-flash (message_end carries model identity)", run.Model)
	}
	// input 99, output 5 (reasoning 2 already inside), cacheRead 1280.
	want := TokenUsage{InputTokens: 99, OutputTokens: 5, CacheRead: 1280}
	if !reflect.DeepEqual(run.Tokens, want) {
		t.Errorf("tokens = %+v, want %+v (reasoning ⊆ output: 5 not 7)", run.Tokens, want)
	}
	if math.Abs(run.CostUSD-0.000018844) > 1e-9 {
		t.Errorf("cost = %v, want 0.000018844 (pi reports usage.cost.total)", run.CostUSD)
	}
	if len(run.ToolCalls) != 0 {
		t.Errorf("tool calls = %+v, want none", run.ToolCalls)
	}
	if !run.Settled {
		t.Error("Settled = false, want true (agent_settled present)")
	}
}

// TestParsePiStream_ToolRun pins the tool-run mapping: response is the
// *final* assistant message (earlier narration excluded), usage summed
// over the 4 messages with reasoning folded into output by pi itself
// (output 240 = 114+41+35+50 — NOT 350), cost summed, tool pairs keyed by
// toolCallId (args from _start, result text from _end), and the last
// message's stop_reason as the run's.
func TestParsePiStream_ToolRun(t *testing.T) {
	run, err := parsePiStream(strings.NewReader(string(loadPiFixture(t, "tool_run.jsonl"))))
	if err != nil {
		t.Fatalf("parsePiStream: %v", err)
	}

	// Final message only — the intermediate messages carry no text here,
	// so Text == Response; the join must still be the final text block.
	if !strings.HasPrefix(run.Response, "Done.") {
		t.Errorf("response = %q, want the final message starting %q", run.Response, "Done.")
	}
	if run.NumTurns != 4 || run.StopReason != "stop" {
		t.Errorf("turns/stop = %d/%q, want 4/\"stop\"", run.NumTurns, run.StopReason)
	}
	wantCalls := []ToolCall{
		{
			Name:   "write",
			Args:   `{"path":"greeting.txt","content":"hello world."}`,
			Result: "Successfully wrote to greeting.txt",
		},
		{
			Name:   "bash",
			Args:   `{"command":"ls -l greeting.txt","timeout":30}`,
			Result: "-rw-r--r-- 1 scoutme scoutme 12 Nov 25 01:09 greeting.txt\n",
		},
		{
			Name:   "bash",
			Args:   `{"command":"cat greeting.txt","timeout":30}`,
			Result: "hello world.",
		},
	}
	if !reflect.DeepEqual(run.ToolCalls, wantCalls) {
		t.Errorf("tool calls =\n%+v\nwant\n%+v", run.ToolCalls, wantCalls)
	}
	// input 135+133+81+126, output 114+41+35+50 (each includes its
	// reasoning: 85+9+5+11), cacheRead 1280+1408+1536+1536.
	want := TokenUsage{InputTokens: 475, OutputTokens: 240, CacheRead: 5760}
	if !reflect.DeepEqual(run.Tokens, want) {
		t.Errorf("tokens = %+v, want %+v (per-message sums, reasoning already in output)", run.Tokens, want)
	}
	// Summed usage.cost.total over the four messages — pi's catalog prices
	// are the models.dev xiaomi prices eval's builtin table also uses, so
	// reported pi costs and computed milk costs are directly comparable.
	if math.Abs(run.CostUSD-0.000149828) > 1e-9 {
		t.Errorf("cost = %v, want 0.000149828 (summed over the 4 messages)", run.CostUSD)
	}
}

// TestParsePiStream_NarrationExcluded: the Response tracks the LAST
// assistant message while Text keeps every text block — pre-tool narration
// must not leak into the judged response but stays available as fallback.
func TestParsePiStream_NarrationExcluded(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"session","version":3,"id":"s","timestamp":"t","cwd":"/w"}`,
		`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"Creating the file now."}],"model":"m","stopReason":"toolUse","usage":{"input":10,"output":5,"cacheRead":0,"cacheWrite":0,"cost":{"total":0.001}}}}`,
		`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"Done."}],"model":"m","stopReason":"stop","usage":{"input":12,"output":3,"cacheRead":0,"cacheWrite":0,"cost":{"total":0.001}}}}`,
		`{"type":"agent_settled","aborted":false}`,
	}, "\n") + "\n"

	run, err := parsePiStream(strings.NewReader(stream))
	if err != nil {
		t.Fatalf("parsePiStream: %v", err)
	}
	if run.Response != "Done." {
		t.Errorf("response = %q, want the final message only", run.Response)
	}
	if !strings.Contains(run.Text, "Creating the file now.") {
		t.Errorf("Text = %q, want every text block kept", run.Text)
	}
	if run.NumTurns != 2 {
		t.Errorf("turns = %d, want 2", run.NumTurns)
	}
}

// TestParsePiStream_TruncatedAndMalformed pins the framing rules: the
// stream must contain agent_settled (a truncated run returns the PARTIAL
// run with the error, so a killed turn still keeps its streamed output —
// the milk parser's lesson), while non-JSON prose lines are kept as
// warnings instead of failing the turn.
func TestParsePiStream_TruncatedAndMalformed(t *testing.T) {
	assistant := `{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"hi"}],"model":"m","stopReason":"stop","usage":{"input":1,"output":1,"cacheRead":0,"cacheWrite":0,"cost":{"total":0}}}}`
	settled := `{"type":"agent_settled","aborted":false}`

	for _, tc := range []struct {
		name    string
		stream  string
		wantErr string
	}{
		{"empty", "", "agent_settled"},
		{"no settled", assistant + "\n", "agent_settled"},
		{"garbage only", "not json at all\n", "agent_settled"},
	} {
		run, err := parsePiStream(strings.NewReader(tc.stream))
		if err == nil {
			t.Errorf("%s: expected error containing %q, got nil", tc.name, tc.wantErr)
		} else if !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("%s: error %q does not mention %q", tc.name, err, tc.wantErr)
		}
		if run == nil {
			t.Errorf("%s: partial run must survive the truncation error", tc.name)
		}
	}

	// Prose mixed into stdout: tolerated, kept as a warning, run still parses.
	run, err := parsePiStream(strings.NewReader("prose line from pi\n" + assistant + "\n" + settled + "\n"))
	if err != nil {
		t.Fatalf("stream with prose line rejected: %v", err)
	}
	if run.Response != "hi" || run.NumTurns != 1 {
		t.Errorf("response/turns = %q/%d, want hi/1", run.Response, run.NumTurns)
	}
	if len(run.Warnings) != 1 || run.Warnings[0] != "prose line from pi" {
		t.Errorf("warnings = %q, want the prose line captured", run.Warnings)
	}

	// Truncation still returns what streamed before the cut.
	run, err = parsePiStream(strings.NewReader("prose\n" + assistant + "\n"))
	if err == nil {
		t.Error("unsettled stream: expected truncation error, got nil")
	}
	if run == nil || run.Response != "hi" {
		t.Errorf("partial run lost: %+v", run)
	}
}

// TestParsePiStream_OpenSetTolerance: unknown event types and fields are
// ignored, never rejected. The streaming message_update's cumulative usage
// must NOT be summed (double-counting the turn), a tool_execution_end
// without its _start still records the call, and other roles' message_end
// contributes nothing.
func TestParsePiStream_OpenSetTolerance(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"session","version":3,"id":"s","timestamp":"t","cwd":"/w"}`,
		`{"type":"brand_new_event","future":42}`,
		`{"type":"auto_retry_start","attempt":1,"maxAttempts":3,"errorMessage":"529"}`,
		`{"type":"message_update","usage":{"input":9999,"output":9999},"assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":"x"}}`,
		`{"type":"message_end","message":{"role":"system","content":""}}`,
		`{"type":"message_end","message":{"role":"user","content":"ask"}}`,
		`{"type":"tool_execution_end","toolCallId":"orphan","toolName":"mystery","result":{"content":[{"type":"text","text":"orphan result"}]}}`,
		`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"done"},{"type":"thinking","thinking":"hmm"}],"model":"m","stopReason":"stop","usage":{"input":100,"output":4,"cacheRead":7,"cacheWrite":1,"cost":{"total":0.02},"unknown":true}}}`,
		`{"type":"agent_settled","aborted":false}`,
	}, "\n") + "\n"

	run, err := parsePiStream(strings.NewReader(stream))
	if err != nil {
		t.Fatalf("open-set stream rejected: %v", err)
	}
	if run.Response != "done" {
		t.Errorf("response = %q, want %q (thinking block excluded)", run.Response, "done")
	}
	want := TokenUsage{InputTokens: 100, OutputTokens: 4, CacheRead: 7, CacheCreate: 1}
	if !reflect.DeepEqual(run.Tokens, want) {
		t.Errorf("tokens = %+v, want %+v (message_update's 9999 never summed)", run.Tokens, want)
	}
	if math.Abs(run.CostUSD-0.02) > 1e-9 {
		t.Errorf("cost = %v, want 0.02", run.CostUSD)
	}
	wantCalls := []ToolCall{{Name: "mystery", Args: "{}", Result: "orphan result"}}
	if !reflect.DeepEqual(run.ToolCalls, wantCalls) {
		t.Errorf("tool calls = %+v, want _end-only call kept with empty args", run.ToolCalls)
	}
}

// TestParsePiStream_ErrorStopAndSupersede: a terminal stop_reason of
// error/aborted surfaces the errorMessage as the run's error; a later
// successful message supersedes an earlier failed attempt (pi auto-retries
// internally — only the LAST message decides run health); agent_settled
// aborted reports when no message carried the reason.
func TestParsePiStream_ErrorStopAndSupersede(t *testing.T) {
	assistant := func(stop, errMsg, text string, input int64) string {
		return `{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"` + text + `"}],"model":"m","stopReason":"` + stop + `","errorMessage":"` + errMsg + `","usage":{"input":` + strconv.FormatInt(input, 10) + `,"output":1,"cacheRead":0,"cacheWrite":0,"cost":{"total":0}}}}`
	}
	settled := func(aborted bool) string {
		flag := "false"
		if aborted {
			flag = "true"
		}
		return `{"type":"agent_settled","aborted":` + flag + `}`
	}

	// Terminal error keeps the message.
	run, err := parsePiStream(strings.NewReader(assistant("error", "boom", "", 7) + "\n" + settled(false) + "\n"))
	if err != nil {
		t.Fatalf("parsePiStream: %v", err)
	}
	if run.ErrorMsg != "boom" {
		t.Errorf("errorMsg = %q, want boom", run.ErrorMsg)
	}

	// A successful retry supersedes the failed attempt.
	run, err = parsePiStream(strings.NewReader(
		assistant("error", "boom", "", 7) + "\n" +
			assistant("stop", "", "recovered", 9) + "\n" + settled(false) + "\n"))
	if err != nil {
		t.Fatalf("parsePiStream: %v", err)
	}
	if run.ErrorMsg != "" {
		t.Errorf("errorMsg = %q, want empty (successful message supersedes the failed attempt)", run.ErrorMsg)
	}
	if run.Response != "recovered" || run.Tokens.InputTokens != 16 {
		t.Errorf("response/tokens = %q/%d, want recovered/16 (both attempts' tokens billed)", run.Response, run.Tokens.InputTokens)
	}

	// Settled-aborted with no message-level reason still fails the run.
	run, err = parsePiStream(strings.NewReader(assistant("stop", "", "ok", 1) + "\n" + settled(true) + "\n"))
	if err != nil {
		t.Fatalf("parsePiStream: %v", err)
	}
	if run.ErrorMsg != "run aborted" {
		t.Errorf("errorMsg = %q, want %q", run.ErrorMsg, "run aborted")
	}
}

// TestPiAdapter_SetArgsPseudoFlags pins the split between pi's own
// pseudo-flag (--pi-bin — never forwarded) and verbatim forwarded pi flags
// (--model pins the model for this eval run).
func TestPiAdapter_SetArgsPseudoFlags(t *testing.T) {
	a := &piAdapter{}
	a.SetArgs([]string{"--pi-bin", "/opt/bin/pi", "--model", "xiaomi/mimo-v2.6-flash"})
	if a.binArg != "/opt/bin/pi" {
		t.Errorf("binArg = %q, want /opt/bin/pi", a.binArg)
	}
	if want := []string{"--model", "xiaomi/mimo-v2.6-flash"}; !reflect.DeepEqual(a.extraArgs, want) {
		t.Errorf("extraArgs = %q, want %q (pseudo-flag stripped)", a.extraArgs, want)
	}
}

// TestPiAdapter_EndToEnd_RecordedFixture drives the full adapter path —
// spawn, `pi --mode json` invocation, JSONL consumption, mapping onto
// RunResult — against a stub binary that replays a recorded run and logs
// its argv. Second turn asserts the `--continue` resume shape (pi keys
// sessions per directory, like milk — no session id to capture), and a
// re-Start (next scenario) resets the gate so turn 1 of a scenario never
// attaches to anything.
func TestPiAdapter_EndToEnd_RecordedFixture(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("stub binary is a sh script")
	}
	fixtureAbs, err := filepath.Abs(filepath.Join(piTestdataDir, "tool_run.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	argsLog := filepath.Join(tmp, "args.txt")
	stub := filepath.Join(tmp, "pi-stub")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" >> " + strconv.Quote(argsLog) + "\n" +
		"printf 'new file\\n' > created.txt\n" +
		"cat " + strconv.Quote(fixtureAbs) + "\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	workdir := t.TempDir()
	a := &piAdapter{piBin: stub}
	a.SetArgs([]string{"--model", "xiaomi/mimo-v2.6-flash"})
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
	if !reflect.DeepEqual(result.Tokens, TokenUsage{InputTokens: 475, OutputTokens: 240, CacheRead: 5760}) {
		t.Errorf("tokens = %+v", result.Tokens)
	}
	if math.Abs(result.CostUSD-0.000149828) > 1e-9 {
		t.Errorf("cost = %v, want the fixture's summed cost", result.CostUSD)
	}
	if result.Model != "mimo-v2.6-flash" {
		t.Errorf("model = %q, want mimo-v2.6-flash (filled from the stream)", result.Model)
	}
	if len(result.ToolCalls) != 3 {
		t.Errorf("tool calls = %+v, want 3", result.ToolCalls)
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

	// Second turn resumes the workdir's session via --continue.
	if _, err := a.RunPrompt(ctx, "Now verify it"); err != nil {
		t.Fatalf("RunPrompt (turn 2): %v", err)
	}

	// A new Start (next scenario) resets the gate: its turn 1 must not
	// pass --continue.
	workdir2 := t.TempDir()
	if err := a.Start(ctx, workdir2); err != nil {
		t.Fatalf("Start (scenario 2): %v", err)
	}
	if _, err := a.RunPrompt(ctx, "Fresh scenario"); err != nil {
		t.Fatalf("RunPrompt (scenario 2): %v", err)
	}

	argsData, err := os.ReadFile(argsLog)
	if err != nil {
		t.Fatalf("reading stub argv log: %v", err)
	}
	gotArgs := strings.Split(strings.TrimRight(string(argsData), "\n"), "\n")
	wantArgs := []string{
		"--mode", "json", "--model", "xiaomi/mimo-v2.6-flash", "--", "Create greeting.txt",
		"--mode", "json", "--continue", "--model", "xiaomi/mimo-v2.6-flash", "--", "Now verify it",
		"--mode", "json", "--model", "xiaomi/mimo-v2.6-flash", "--", "Fresh scenario",
	}
	if !reflect.DeepEqual(gotArgs, wantArgs) {
		t.Errorf("adapter argv =\n%q\nwant\n%q", gotArgs, wantArgs)
	}
}
