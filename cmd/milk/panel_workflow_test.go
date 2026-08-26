package main

import (
	"strings"
	"testing"

	"github.com/scoutme/milk/internal/workflow"
)

// ── truncatePanel ─────────────────────────────────────────────────────────────

func TestTruncatePanel_ShortStringUnchanged(t *testing.T) {
	s := "hello"
	got := truncatePanel(s, 10)
	if got != s {
		t.Errorf("truncatePanel(%q, 10) = %q, want unchanged", s, got)
	}
}

func TestTruncatePanel_ExactWidthUnchanged(t *testing.T) {
	s := "hello"
	got := truncatePanel(s, 5)
	if got != s {
		t.Errorf("truncatePanel(%q, 5) = %q, want unchanged", s, got)
	}
}

func TestTruncatePanel_PlainStringTruncated(t *testing.T) {
	s := "hello world"
	got := truncatePanel(s, 6)
	stripped := stripANSI(got)
	runes := []rune(stripped)
	if len(runes) > 6 {
		t.Errorf("truncatePanel: visual length %d > maxWidth 6 (result %q)", len(runes), got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("truncatePanel: result %q should end with ellipsis", got)
	}
}

// TestTruncatePanel_ANSIStringNoGarbage verifies that truncating a string that
// contains ANSI escape codes does not bisect any escape sequence.
// After truncation, no ESC byte (\x1b) may appear unless followed by a complete
// sequence terminator — the simplest check is that the stripped result contains
// no residual escape fragments.
func TestTruncatePanel_ANSIStringNoGarbage(t *testing.T) {
	// dim() wraps with "\x1b[2m...\x1b[0m"; the string is visually "hello world"
	// but bytes-wise is longer due to ANSI codes.
	s := dim("hello world")

	maxWidth := 6
	got := truncatePanel(s, maxWidth)

	// The result must not contain a naked ESC byte that is not part of a
	// recognised CSI/SGR sequence. The simplest proxy: after stripping ANSI the
	// result should be ≤ maxWidth visual runes.
	stripped := stripANSI(got)
	runes := []rune(stripped)
	if len(runes) > maxWidth {
		t.Errorf("visual length %d > maxWidth %d; result=%q", len(runes), maxWidth, got)
	}

	// Additionally verify no stray ESC appears mid-string beyond the ANSI codes
	// that are properly paired. We check the stripped form has no ESC at all.
	if strings.ContainsRune(stripped, '\x1b') {
		t.Errorf("stripANSI of truncated result still contains ESC byte: %q", stripped)
	}
}

// TestTruncatePanel_ANSIVisualLengthExact checks that the visual (stripped) length
// of a truncated ANSI string is exactly maxWidth (ellipsis counts as 1 rune).
func TestTruncatePanel_ANSIVisualLengthExact(t *testing.T) {
	s := dim("abcdefghijklmnopqrstuvwxyz")
	maxWidth := 8
	got := truncatePanel(s, maxWidth)
	stripped := []rune(stripANSI(got))
	if len(stripped) != maxWidth {
		t.Errorf("visual length = %d, want exactly %d; result=%q", len(stripped), maxWidth, got)
	}
}

func TestTruncatePanel_ZeroWidthReturnsInput(t *testing.T) {
	s := "something"
	got := truncatePanel(s, 0)
	if got != s {
		t.Errorf("maxWidth=0 should return input unchanged, got %q", got)
	}
}

// ── workflowPanelLineCount ────────────────────────────────────────────────────

func TestWorkflowPanelLineCount_Nil(t *testing.T) {
	// title + blank + "no active workflow" = 3
	got := workflowPanelLineCount(nil)
	if got != 3 {
		t.Errorf("lineCount(nil) = %d, want 3", got)
	}
}

func TestWorkflowPanelLineCount_ActiveRole(t *testing.T) {
	st := &workflow.State{
		WorkflowName: "dev",
		Sprint:       1,
		Pass:         1,
		Role:         "generator",
	}
	// title + blank + workflow+sprint + pass+role + blank + in-progress arrow = 6
	want := 6
	got := workflowPanelLineCount(st)
	if got != want {
		t.Errorf("lineCount(active role) = %d, want %d", got, want)
	}
}

func TestWorkflowPanelLineCount_DoneRole(t *testing.T) {
	st := &workflow.State{
		WorkflowName: "dev",
		Sprint:       1,
		Pass:         1,
		Role:         "done",
	}
	// title + blank + workflow+sprint + pass+role + blank + 0 verdicts + 0 arrow = 5
	want := 5
	got := workflowPanelLineCount(st)
	if got != want {
		t.Errorf("lineCount(done) = %d, want %d", got, want)
	}
}

func TestWorkflowPanelLineCount_DoneRoleWithVerdicts(t *testing.T) {
	st := &workflow.State{
		WorkflowName: "dev",
		Role:         "done",
		VerdictHistory: []workflow.VerdictEntry{
			{Sprint: 1, Pass: 1, Verdict: "good_to_go"},
			{Sprint: 2, Pass: 1, Verdict: "needs_refinement"},
		},
	}
	// title + blank + workflow+sprint + pass+role + blank + 2 verdicts + 0 arrow = 7
	want := 7
	got := workflowPanelLineCount(st)
	if got != want {
		t.Errorf("lineCount(done, 2 verdicts) = %d, want %d", got, want)
	}
}

func TestWorkflowPanelLineCount_ActiveRoleWithVerdicts(t *testing.T) {
	st := &workflow.State{
		WorkflowName: "dev",
		Role:         "evaluator",
		VerdictHistory: []workflow.VerdictEntry{
			{Sprint: 1, Pass: 1, Verdict: "good_to_go"},
		},
	}
	// title + blank + workflow+sprint + pass+role + blank + 1 verdict + 1 arrow = 7
	want := 7
	got := workflowPanelLineCount(st)
	if got != want {
		t.Errorf("lineCount(active, 1 verdict) = %d, want %d", got, want)
	}
}

// TestWorkflowPanelLineCount_DoneVsActive verifies done returns exactly 1 less than active.
func TestWorkflowPanelLineCount_DoneVsActive(t *testing.T) {
	verdicts := []workflow.VerdictEntry{{Sprint: 1, Pass: 1, Verdict: "good_to_go"}}
	active := &workflow.State{WorkflowName: "dev", Role: "generator", VerdictHistory: verdicts}
	done := &workflow.State{WorkflowName: "dev", Role: "done", VerdictHistory: verdicts}
	if workflowPanelLineCount(active) != workflowPanelLineCount(done)+1 {
		t.Errorf("active lineCount=%d, done lineCount=%d — expected active = done+1",
			workflowPanelLineCount(active), workflowPanelLineCount(done))
	}
}

// ── sprint X/Y label ──────────────────────────────────────────────────────────

// sprintLine returns the "workflow  sprint ..." line from a rendered panel,
// i.e. the 3rd line (title, blank, then this one).
func sprintLine(t *testing.T, st *workflow.State) string {
	t.Helper()
	lines := buildWorkflowPanelLines(st, workflowPanelContentWidth-2)
	if len(lines) < 3 {
		t.Fatalf("expected at least 3 lines, got %d: %v", len(lines), lines)
	}
	return stripANSI(lines[2])
}

func TestBuildWorkflowPanelLines_SprintShowsTotalWhenKnown(t *testing.T) {
	st := &workflow.State{WorkflowName: "dev", Sprint: 2, TotalSprints: 5, Role: "generator"}
	got := sprintLine(t, st)
	if !strings.Contains(got, "sprint 2/5") {
		t.Errorf("expected sprint line to contain %q, got %q", "sprint 2/5", got)
	}
}

func TestBuildWorkflowPanelLines_SprintFallsBackWithoutTotal(t *testing.T) {
	// TotalSprints == 0 covers both a state file saved before this field
	// existed and the designer role, before the sprint count is known —
	// neither should render as "sprint N/0".
	st := &workflow.State{WorkflowName: "dev", Sprint: 1, Role: "designer"}
	got := sprintLine(t, st)
	if strings.Contains(got, "/0") {
		t.Errorf("must never render an unknown total as /0, got %q", got)
	}
	if !strings.Contains(got, "sprint 1") {
		t.Errorf("expected sprint line to contain %q, got %q", "sprint 1", got)
	}
}

func TestBuildWorkflowPanelLines_GenericShowsStagePathNotSprint(t *testing.T) {
	st := &workflow.State{
		WorkflowName: "pair", Task: "build a thing", Role: "generator",
		StagePath: "sprint_loop[1] > pass_loop[2]", Generic: true,
	}
	lines := buildWorkflowPanelLines(st, 60)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "role: generator") {
		t.Errorf("expected the current role rendered, got:\n%s", joined)
	}
	if !strings.Contains(joined, "sprint_loop[1] > pass_loop[2]") {
		t.Errorf("expected the stage path rendered, got:\n%s", joined)
	}
	if strings.Contains(joined, "sprint 0") || strings.Contains(joined, "pass 0") {
		t.Errorf("generic state must not render the dev-shaped sprint/pass line, got:\n%s", joined)
	}
}

func TestBuildWorkflowPanelLines_DevStateUnaffectedByGenericField(t *testing.T) {
	// A zero-value Generic (the common case for every existing dev.go state)
	// must still render the legacy sprint/pass/verdict-history layout.
	st := &workflow.State{WorkflowName: "dev", Sprint: 2, TotalSprints: 3, Role: "evaluator"}
	got := sprintLine(t, st)
	if !strings.Contains(got, "sprint 2/3") {
		t.Errorf("expected the dev-shaped sprint line, got %q", got)
	}
}
