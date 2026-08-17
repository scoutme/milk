package eval

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// withTempActivityState redirects claudeActivityStatePath to a file under a
// fresh temp dir for the duration of the test, so tests never touch the real
// ~/.milk state file, and restores the original afterward.
func withTempActivityState(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "claude-cache-activity")
	orig := claudeActivityStatePath
	claudeActivityStatePath = func() (string, error) { return path, nil }
	t.Cleanup(func() { claudeActivityStatePath = orig })
	return path
}

func writeActivityTimestamp(t *testing.T, path string, when time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(when.Format(time.RFC3339Nano)), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestClaudeAdapter_SetArgs_CacheCooldown(t *testing.T) {
	a := &claudeAdapter{}
	a.SetArgs([]string{"--foo", "bar", "--cache-cooldown", "5m", "--baz"})

	if a.cacheCooldownErr != nil {
		t.Fatalf("unexpected error: %v", a.cacheCooldownErr)
	}
	if a.cacheCooldown != 5*time.Minute {
		t.Errorf("got cacheCooldown=%v, want 5m", a.cacheCooldown)
	}
	want := []string{"--foo", "bar", "--baz"}
	if len(a.extraArgs) != len(want) {
		t.Fatalf("got extraArgs=%v, want %v", a.extraArgs, want)
	}
	for i, w := range want {
		if a.extraArgs[i] != w {
			t.Errorf("extraArgs[%d] = %q, want %q", i, a.extraArgs[i], w)
		}
	}
}

func TestClaudeAdapter_SetArgs_NoCooldown(t *testing.T) {
	a := &claudeAdapter{}
	a.SetArgs([]string{"--agent", "mimo-local"})

	if a.cacheCooldownErr != nil {
		t.Fatalf("unexpected error: %v", a.cacheCooldownErr)
	}
	if a.cacheCooldown != 0 {
		t.Errorf("got cacheCooldown=%v, want 0 (unset)", a.cacheCooldown)
	}
	if len(a.extraArgs) != 2 || a.extraArgs[0] != "--agent" || a.extraArgs[1] != "mimo-local" {
		t.Errorf("extraArgs mutated unexpectedly: %v", a.extraArgs)
	}
}

func TestClaudeAdapter_SetArgs_CacheCooldown_InvalidDuration(t *testing.T) {
	a := &claudeAdapter{}
	a.SetArgs([]string{"--cache-cooldown", "not-a-duration"})

	if a.cacheCooldownErr == nil {
		t.Fatal("expected an error for invalid duration, got nil")
	}
}

func TestClaudeAdapter_SetArgs_CacheCooldown_MissingValue(t *testing.T) {
	a := &claudeAdapter{}
	a.SetArgs([]string{"--cache-cooldown"})

	if a.cacheCooldownErr == nil {
		t.Fatal("expected an error for missing duration value, got nil")
	}
}

func TestClaudeAdapter_WaitForCacheCooldown_Disabled(t *testing.T) {
	withTempActivityState(t)
	a := &claudeAdapter{} // cacheCooldown left at zero value: disabled
	start := time.Now()
	if err := a.waitForCacheCooldown(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Errorf("expected no wait when cooldown is disabled, took %v", elapsed)
	}
}

func TestClaudeAdapter_WaitForCacheCooldown_NeverRecorded(t *testing.T) {
	withTempActivityState(t) // no file written: simulates "never run before"
	a := &claudeAdapter{cacheCooldown: 5 * time.Minute}
	start := time.Now()
	if err := a.waitForCacheCooldown(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Errorf("expected no wait when nothing was ever recorded, took %v", elapsed)
	}
}

func TestClaudeAdapter_WaitForCacheCooldown_WaitsOutRemainder(t *testing.T) {
	path := withTempActivityState(t)
	writeActivityTimestamp(t, path, time.Now())

	a := &claudeAdapter{cacheCooldown: 100 * time.Millisecond}
	start := time.Now()
	if err := a.waitForCacheCooldown(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 80*time.Millisecond {
		t.Errorf("expected to wait close to 100ms, only waited %v", elapsed)
	}
}

func TestClaudeAdapter_WaitForCacheCooldown_AlreadyElapsed(t *testing.T) {
	path := withTempActivityState(t)
	writeActivityTimestamp(t, path, time.Now().Add(-time.Hour))

	a := &claudeAdapter{cacheCooldown: 5 * time.Minute}
	start := time.Now()
	if err := a.waitForCacheCooldown(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Errorf("expected no wait when cooldown already elapsed, took %v", elapsed)
	}
}

func TestClaudeAdapter_WaitForCacheCooldown_RespectsContextCancellation(t *testing.T) {
	path := withTempActivityState(t)
	writeActivityTimestamp(t, path, time.Now())

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	a := &claudeAdapter{cacheCooldown: time.Hour}
	err := a.waitForCacheCooldown(ctx)
	if err == nil {
		t.Fatal("expected context deadline error, got nil")
	}
}

// ---------------------------------------------------------------------------
// sumTurnTokens tests for subagent/workflow token parsing
// ---------------------------------------------------------------------------

func mkAssistantLine(input, output, cacheCreate, cacheRead int64) claudeJSONLLine {
	return claudeJSONLLine{
		Type: "assistant",
		Message: claudeMessage{
			Usage: claudeUsage{
				InputTokens:              input,
				OutputTokens:             output,
				CacheCreationInputTokens: cacheCreate,
				CacheReadInputTokens:     cacheRead,
			},
		},
	}
}

func mkResultLine(subagent, workflow *claudeUsage) claudeJSONLLine {
	return claudeJSONLLine{
		Type:          "result",
		SubagentUsage: subagent,
		WorkflowUsage: workflow,
	}
}

func TestSumTurnTokens_MainOnly(t *testing.T) {
	lines := []claudeJSONLLine{
		mkAssistantLine(1000, 200, 50, 300),
		mkAssistantLine(500, 100, 0, 200),
	}
	tok := sumTurnTokens(lines)
	if tok.InputTokens != 1500 {
		t.Errorf("InputTokens: got %d, want 1500", tok.InputTokens)
	}
	if tok.OutputTokens != 300 {
		t.Errorf("OutputTokens: got %d, want 300", tok.OutputTokens)
	}
	if tok.CacheCreate != 50 {
		t.Errorf("CacheCreate: got %d, want 50", tok.CacheCreate)
	}
	if tok.CacheRead != 500 {
		t.Errorf("CacheRead: got %d, want 500", tok.CacheRead)
	}
	if tok.SubagentInputTokens != 0 || tok.WorkflowInputTokens != 0 {
		t.Errorf("subagent/workflow tokens should be zero, got sub=%d wf=%d", tok.SubagentInputTokens, tok.WorkflowInputTokens)
	}
}

func TestSumTurnTokens_SubagentOnly(t *testing.T) {
	lines := []claudeJSONLLine{
		mkAssistantLine(1000, 200, 0, 0),
		mkResultLine(&claudeUsage{
			InputTokens:              300,
			OutputTokens:             50,
			CacheCreationInputTokens: 10,
			CacheReadInputTokens:     100,
		}, nil),
	}
	tok := sumTurnTokens(lines)
	if tok.InputTokens != 1000 {
		t.Errorf("InputTokens: got %d, want 1000", tok.InputTokens)
	}
	if tok.SubagentInputTokens != 300 {
		t.Errorf("SubagentInputTokens: got %d, want 300", tok.SubagentInputTokens)
	}
	if tok.SubagentOutputTokens != 50 {
		t.Errorf("SubagentOutputTokens: got %d, want 50", tok.SubagentOutputTokens)
	}
	if tok.SubagentCacheCreate != 10 {
		t.Errorf("SubagentCacheCreate: got %d, want 10", tok.SubagentCacheCreate)
	}
	if tok.SubagentCacheRead != 100 {
		t.Errorf("SubagentCacheRead: got %d, want 100", tok.SubagentCacheRead)
	}
	if tok.WorkflowInputTokens != 0 {
		t.Errorf("WorkflowInputTokens should be 0, got %d", tok.WorkflowInputTokens)
	}
}

func TestSumTurnTokens_WorkflowOnly(t *testing.T) {
	lines := []claudeJSONLLine{
		mkAssistantLine(500, 100, 0, 0),
		mkResultLine(nil, &claudeUsage{
			InputTokens:              200,
			OutputTokens:             80,
			CacheCreationInputTokens: 5,
			CacheReadInputTokens:     50,
		}),
	}
	tok := sumTurnTokens(lines)
	if tok.SubagentInputTokens != 0 {
		t.Errorf("SubagentInputTokens should be 0, got %d", tok.SubagentInputTokens)
	}
	if tok.WorkflowInputTokens != 200 {
		t.Errorf("WorkflowInputTokens: got %d, want 200", tok.WorkflowInputTokens)
	}
	if tok.WorkflowOutputTokens != 80 {
		t.Errorf("WorkflowOutputTokens: got %d, want 80", tok.WorkflowOutputTokens)
	}
	if tok.WorkflowCacheCreate != 5 {
		t.Errorf("WorkflowCacheCreate: got %d, want 5", tok.WorkflowCacheCreate)
	}
	if tok.WorkflowCacheRead != 50 {
		t.Errorf("WorkflowCacheRead: got %d, want 50", tok.WorkflowCacheRead)
	}
}

func TestSumTurnTokens_SubagentAndWorkflow(t *testing.T) {
	lines := []claudeJSONLLine{
		mkAssistantLine(1000, 200, 0, 0),
		mkResultLine(
			&claudeUsage{InputTokens: 300, OutputTokens: 50, CacheCreationInputTokens: 10, CacheReadInputTokens: 100},
			&claudeUsage{InputTokens: 200, OutputTokens: 80, CacheCreationInputTokens: 5, CacheReadInputTokens: 50},
		),
		mkAssistantLine(500, 100, 0, 0),
		// Second result line with additional tokens (accumulates).
		mkResultLine(
			&claudeUsage{InputTokens: 100, OutputTokens: 20, CacheCreationInputTokens: 0, CacheReadInputTokens: 50},
			nil,
		),
	}
	tok := sumTurnTokens(lines)
	if tok.InputTokens != 1500 {
		t.Errorf("InputTokens: got %d, want 1500", tok.InputTokens)
	}
	if tok.SubagentInputTokens != 400 {
		t.Errorf("SubagentInputTokens: got %d, want 400 (300+100)", tok.SubagentInputTokens)
	}
	if tok.SubagentOutputTokens != 70 {
		t.Errorf("SubagentOutputTokens: got %d, want 70 (50+20)", tok.SubagentOutputTokens)
	}
	if tok.SubagentCacheRead != 150 {
		t.Errorf("SubagentCacheRead: got %d, want 150 (100+50)", tok.SubagentCacheRead)
	}
	if tok.WorkflowInputTokens != 200 {
		t.Errorf("WorkflowInputTokens: got %d, want 200", tok.WorkflowInputTokens)
	}
	if tok.WorkflowOutputTokens != 80 {
		t.Errorf("WorkflowOutputTokens: got %d, want 80", tok.WorkflowOutputTokens)
	}
}

func TestSumTurnTokens_NonResultLineWithUsageFieldsIgnored(t *testing.T) {
	// A non-result line that happens to have SubagentUsage/WorkflowUsage set
	// should NOT have its subagent/workflow tokens counted, thanks to the
	// l.Type == "result" guard.
	line := claudeJSONLLine{
		Type: "assistant",
		Message: claudeMessage{
			Usage: claudeUsage{InputTokens: 100, OutputTokens: 20},
		},
		SubagentUsage: &claudeUsage{InputTokens: 999, OutputTokens: 999},
		WorkflowUsage: &claudeUsage{InputTokens: 888, OutputTokens: 888},
	}
	tok := sumTurnTokens([]claudeJSONLLine{line})
	if tok.SubagentInputTokens != 0 {
		t.Errorf("non-result line SubagentInputTokens should be 0, got %d", tok.SubagentInputTokens)
	}
	if tok.WorkflowInputTokens != 0 {
		t.Errorf("non-result line WorkflowInputTokens should be 0, got %d", tok.WorkflowInputTokens)
	}
	if tok.InputTokens != 100 {
		t.Errorf("InputTokens: got %d, want 100", tok.InputTokens)
	}
}

func TestSumTurnTokens_NoUsageData(t *testing.T) {
	lines := []claudeJSONLLine{
		{Type: "assistant", Message: claudeMessage{Usage: claudeUsage{InputTokens: 500, OutputTokens: 100}}},
		{Type: "result"}, // no subagent/workflow usage
	}
	tok := sumTurnTokens(lines)
	if tok.InputTokens != 500 || tok.OutputTokens != 100 {
		t.Errorf("main tokens wrong: in=%d out=%d", tok.InputTokens, tok.OutputTokens)
	}
	if tok.SubagentInputTokens != 0 || tok.WorkflowInputTokens != 0 {
		t.Errorf("subagent/workflow should be 0: sub=%d wf=%d", tok.SubagentInputTokens, tok.WorkflowInputTokens)
	}
}

// TestClaudeAdapter_CooldownPersistsAcrossProcesses simulates two separate
// `milk eval run` invocations sharing the same state file: the second
// "process" (a fresh path lookup, no in-memory state carried over) must still
// see the first one's recorded activity and wait accordingly.
func TestClaudeAdapter_CooldownPersistsAcrossProcesses(t *testing.T) {
	withTempActivityState(t)

	recordClaudeActivity() // "process 1" finishes a turn

	a := &claudeAdapter{cacheCooldown: 150 * time.Millisecond}
	start := time.Now()
	if err := a.waitForCacheCooldown(context.Background()); err != nil { // "process 2"
		t.Fatalf("unexpected error: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
		t.Errorf("expected the second call to wait out the persisted cooldown, only waited %v", elapsed)
	}
}
