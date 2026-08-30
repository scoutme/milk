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

func TestBuildWorkflowPanelLines_GenericShowsStageTreeNotSprint(t *testing.T) {
	st := &workflow.State{
		WorkflowName: "pair", Task: "build a thing", Role: "generator",
		StageTree: &workflow.StageNode{
			Label:    "sprint_loop[1]",
			Children: []*workflow.StageNode{{Label: "pass_loop[2]"}},
		},
		Generic: true,
	}
	lines := buildWorkflowPanelLines(st, 60)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "role: generator") {
		t.Errorf("expected the current role rendered, got:\n%s", joined)
	}
	if !strings.Contains(joined, "sprint 1") {
		t.Errorf("expected the stage tree rendered, got:\n%s", joined)
	}
	if !strings.Contains(joined, "pass 2") {
		t.Errorf("expected nested child rendered, got:\n%s", joined)
	}
	if strings.Contains(joined, "sprint 0") || strings.Contains(joined, "pass 0") {
		t.Errorf("generic state must not render the dev-shaped sprint/pass line, got:\n%s", joined)
	}
}

func TestBuildWorkflowPanelLines_GenericShowsFullStageTreeWithActiveMarker(t *testing.T) {
	st := &workflow.State{
		WorkflowName: "pair",
		Task:         "build a thing",
		Role:         "reviewer",
		StageTree: &workflow.StageNode{Label: "workflow", Children: []*workflow.StageNode{
			{Label: "design  designer"},
			{Label: "implement  generator"},
			{Label: "review  reviewer"},
		}},
		ActiveStageTree: &workflow.StageNode{Label: "review"},
		Generic:         true,
	}
	joined := stripANSI(strings.Join(buildWorkflowPanelLines(st, 60), "\n"))
	for _, stage := range []string{"design", "impl", "eval"} {
		if !strings.Contains(joined, stage) {
			t.Fatalf("expected generic panel to show stage %q, got:\n%s", stage, joined)
		}
	}
	if !strings.Contains(joined, "▸   eval") {
		t.Fatalf("expected active review marker inside full tree, got:\n%s", joined)
	}
}

func TestBuildWorkflowPanelLines_GenericMarksStaticRoleNodeActive(t *testing.T) {
	st := &workflow.State{
		WorkflowName: "dev",
		Role:         "designer",
		StageTree: &workflow.StageNode{Label: "workflow", Children: []*workflow.StageNode{
			{Label: "designer  designer"},
			{Label: "sprint_loop"},
		}},
		ActiveStageTree: &workflow.StageNode{Label: "designer"},
		Generic:         true,
	}
	joined := stripANSI(strings.Join(buildWorkflowPanelLines(st, 80), "\n"))
	if !strings.Contains(joined, "▸   design") {
		t.Fatalf("expected active marker on static designer node, got:\n%s", joined)
	}
}

func TestBuildWorkflowPanelLines_GenericMarksStaticRoleNodeActiveFromRoleFallback(t *testing.T) {
	st := &workflow.State{
		WorkflowName: "dev",
		Role:         "designer",
		StageTree: &workflow.StageNode{Label: "workflow", Children: []*workflow.StageNode{
			{Label: "designer  designer"},
			{Label: "sprint_loop"},
		}},
		Generic: true,
	}
	joined := stripANSI(strings.Join(buildWorkflowPanelLines(st, 80), "\n"))
	if !strings.Contains(joined, "▸   design") {
		t.Fatalf("expected role fallback to mark static designer node active, got:\n%s", joined)
	}
}

func TestBuildWorkflowPanelLines_GenericMarksStaticRoleSuffixActive(t *testing.T) {
	st := &workflow.State{
		WorkflowName: "custom",
		Role:         "implementer",
		StageTree: &workflow.StageNode{Label: "workflow", Children: []*workflow.StageNode{
			{Label: "final_implementation  implementer"},
		}},
		ActiveStageTree: &workflow.StageNode{Label: "implementer"},
		Generic:         true,
	}
	joined := stripANSI(strings.Join(buildWorkflowPanelLines(st, 80), "\n"))
	if !strings.Contains(joined, "▸   final impl") {
		t.Fatalf("expected active marker on static role-suffix node, got:\n%s", joined)
	}
}

func TestBuildWorkflowPanelLines_GenericShowsConcurrentFanoutBranches(t *testing.T) {
	st := &workflow.State{
		WorkflowName: "swarm",
		Role:         "worker",
		StageTree: &workflow.StageNode{Label: "workflow", Children: []*workflow.StageNode{
			{Label: "designer  designer"},
			{Label: "worker_fanout", Children: []*workflow.StageNode{
				{Label: "worker_pass_loop", Children: []*workflow.StageNode{
					{Label: "worker  worker"},
					{Label: "worker_eval  evaluator"},
				}},
			}},
			{Label: "final_evaluation  evaluator"},
		}},
		ActiveStageTree: &workflow.StageNode{Label: "worker_fanout (2 items)", Children: []*workflow.StageNode{
			{Label: "worker_fanout item[1]", Children: []*workflow.StageNode{{Label: "worker_pass_loop[1]", Children: []*workflow.StageNode{{Label: "worker"}}}}},
			{Label: "worker_fanout item[2]", Children: []*workflow.StageNode{{Label: "worker_pass_loop[1]", Children: []*workflow.StageNode{{Label: "worker"}}}}},
		}},
		Generic: true,
	}
	joined := stripANSI(strings.Join(buildWorkflowPanelLines(st, 80), "\n"))
	for _, label := range []string{"item 1", "item 2", "pass 1", "work", "final eval"} {
		if !strings.Contains(joined, label) {
			t.Fatalf("expected panel to show %q, got:\n%s", label, joined)
		}
	}
	if strings.Contains(joined, "  pass loop") {
		t.Fatalf("static fanout body template should be hidden when item branches are present, got:\n%s", joined)
	}
}

func TestBuildWorkflowPanelLines_GenericKeepsCompletedFanoutItemsAtDone(t *testing.T) {
	st := &workflow.State{
		WorkflowName: "swarm",
		Role:         "done",
		StageTree: &workflow.StageNode{Label: "workflow", Children: []*workflow.StageNode{
			{Label: "designer  designer"},
			{Label: "worker_fanout", Children: []*workflow.StageNode{
				{Label: "worker_pass_loop", Children: []*workflow.StageNode{
					{Label: "worker  worker"},
					{Label: "worker_eval  evaluator"},
				}},
			}},
			{Label: "final_evaluation  evaluator"},
		}},
		CompletedStageTree: &workflow.StageNode{Label: "worker_fanout (2 items)", Children: []*workflow.StageNode{
			{Label: "worker_fanout item[1]"},
			{Label: "worker_fanout item[2]"},
		}},
		Generic: true,
	}
	joined := stripANSI(strings.Join(buildWorkflowPanelLines(st, 80), "\n"))
	for _, label := range []string{"✓     item 1", "✓     item 2"} {
		if !strings.Contains(joined, label) {
			t.Fatalf("expected completed fanout item %q, got:\n%s", label, joined)
		}
	}
	if strings.Contains(joined, "▸") {
		t.Fatalf("done workflow should not show active markers, got:\n%s", joined)
	}
}

func TestBuildWorkflowPanelLines_GenericDevShowsDynamicSprintAndPass(t *testing.T) {
	st := &workflow.State{
		WorkflowName: "dev",
		Role:         "generator",
		StageTree: &workflow.StageNode{Label: "workflow", Children: []*workflow.StageNode{
			{Label: "designer  designer"},
			{Label: "sprint_loop", Children: []*workflow.StageNode{
				{Label: "pass_loop", Children: []*workflow.StageNode{
					{Label: "generator  generator"},
					{Label: "evaluator  evaluator"},
				}},
			}},
		}},
		ActiveStageTree: &workflow.StageNode{Label: "sprint_loop[2]", Children: []*workflow.StageNode{
			{Label: "pass_loop[3]", Children: []*workflow.StageNode{{Label: "generator"}}},
		}},
		Generic: true,
	}
	joined := stripANSI(strings.Join(buildWorkflowPanelLines(st, 80), "\n"))
	for _, label := range []string{"▸   sprint 2", "▸     pass 3", "▸       generator"} {
		if !strings.Contains(joined, label) {
			t.Fatalf("expected dynamic dev label %q, got:\n%s", label, joined)
		}
	}
	if strings.Contains(joined, "sprint loop") || strings.Contains(joined, "pass loop") {
		t.Fatalf("expected dynamic loop labels instead of templates, got:\n%s", joined)
	}
}

func TestBuildWorkflowPanelLines_GenericDevShowsCompletedGeneratorAndActiveEvaluator(t *testing.T) {
	st := &workflow.State{
		WorkflowName: "dev",
		Role:         "evaluator",
		StageTree: &workflow.StageNode{Label: "workflow", Children: []*workflow.StageNode{
			{Label: "designer  designer"},
			{Label: "sprint_loop", Children: []*workflow.StageNode{
				{Label: "pass_loop", Children: []*workflow.StageNode{
					{Label: "generator  generator"},
					{Label: "evaluator  evaluator"},
				}},
			}},
		}},
		ActiveStageTree: &workflow.StageNode{Label: "sprint_loop[1]", Children: []*workflow.StageNode{
			{Label: "pass_loop[1]", Children: []*workflow.StageNode{{Label: "evaluator"}}},
		}},
		CompletedStageTree: &workflow.StageNode{Label: "sprint_loop[1]", Children: []*workflow.StageNode{
			{Label: "pass_loop[1]", Children: []*workflow.StageNode{{Label: "generator"}}},
		}},
		Generic: true,
	}
	joined := stripANSI(strings.Join(buildWorkflowPanelLines(st, 80), "\n"))
	for _, label := range []string{"▸   sprint 1", "▸     pass 1", "✓       generator", "▸       evaluator"} {
		if !strings.Contains(joined, label) {
			t.Fatalf("expected panel to show %q, got:\n%s", label, joined)
		}
	}
}

func TestBuildWorkflowPanelLines_GenericDevKeepsCompletedSprintsAtDone(t *testing.T) {
	st := &workflow.State{
		WorkflowName: "dev",
		Role:         "done",
		StageTree: &workflow.StageNode{Label: "workflow", Children: []*workflow.StageNode{
			{Label: "designer  designer"},
			{Label: "sprint_loop", Children: []*workflow.StageNode{
				{Label: "pass_loop", Children: []*workflow.StageNode{
					{Label: "generator  generator"},
					{Label: "evaluator  evaluator"},
				}},
			}},
		}},
		CompletedStageTree: &workflow.StageNode{Label: "workflow", Children: []*workflow.StageNode{
			{Label: "sprint_loop[1]", Children: []*workflow.StageNode{{Label: "pass_loop[1]"}}},
			{Label: "sprint_loop[2]", Children: []*workflow.StageNode{{Label: "pass_loop[1]"}}},
		}},
		Generic: true,
	}
	joined := stripANSI(strings.Join(buildWorkflowPanelLines(st, 80), "\n"))
	for _, label := range []string{"✓   sprint 1", "✓   sprint 2"} {
		if !strings.Contains(joined, label) {
			t.Fatalf("expected completed dev sprint %q, got:\n%s", label, joined)
		}
	}
	if strings.Contains(joined, "▸") {
		t.Fatalf("done workflow should not show active markers, got:\n%s", joined)
	}
}
