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
	got := workflowPanelLineCount(nil)
	if got != 1 {
		t.Errorf("lineCount(nil) = %d, want 1", got)
	}
}

func TestWorkflowPanelLineCount_ActiveRole(t *testing.T) {
	st := &workflow.State{
		WorkflowName: "dev",
		Sprint:       1,
		Pass:         1,
		Role:         "generator",
	}
	// 3 (header lines) + 0 (no verdicts) + 1 (in-progress arrow)
	want := 4
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
	// 3 + 0 verdicts + 0 (role is "done" so no in-progress line)
	want := 3
	got := workflowPanelLineCount(st)
	if got != want {
		t.Errorf("lineCount(done) = %d, want %d (off-by-one bug if %d)", got, want, want+1)
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
	// 3 + 2 verdicts + 0 (done, no arrow)
	want := 5
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
	// 3 + 1 verdict + 1 arrow
	want := 5
	got := workflowPanelLineCount(st)
	if got != want {
		t.Errorf("lineCount(active, 1 verdict) = %d, want %d", got, want)
	}
}

// TestWorkflowPanelLineCount_DoneVsActive verifies done returns exactly 1 less than active.
func TestWorkflowPanelLineCount_DoneVsActive(t *testing.T) {
	verdicts := []workflow.VerdictEntry{{Sprint: 1, Pass: 1, Verdict: "good_to_go"}}
	active := &workflow.State{Role: "generator", VerdictHistory: verdicts}
	done := &workflow.State{Role: "done", VerdictHistory: verdicts}
	if workflowPanelLineCount(active) != workflowPanelLineCount(done)+1 {
		t.Errorf("active lineCount=%d, done lineCount=%d — expected active = done+1",
			workflowPanelLineCount(active), workflowPanelLineCount(done))
	}
}
